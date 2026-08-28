package inputlatency

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Admission is the source evidence for one V5 input frame. BrokerReceivedTicks
// is captured immediately after the complete authenticated frame payload has
// been read. TransportAdmittedTicks is captured at the successful terminal
// presentation commit, after USB/IP has written the complete response.
type Admission struct {
	Sequence               uint32
	ReceivedAt             time.Time
	BrokerReceivedTicks    int64
	TransportAdmittedTicks int64
	PresentationToken      uint64
	PresentationGeneration uint64
}

type pendingAdmission struct {
	Admission
	consumed bool
}

// Session is a deliberately single-capture recorder. A global hook is needed
// because the API client, broker, and USB/IP server run in one e2e test process
// but do not share a request context. Restricting the recorder to one active
// capture makes cross-device or nested evidence fail closed.
type Session struct {
	mu sync.Mutex

	closed       bool
	source       uintptr
	sourceBound  bool
	failure      error
	bySequence   map[uint32]*pendingAdmission
	byReceivedAt map[time.Time]uint32
	wake         chan struct{}
}

var active atomic.Pointer[Session]

// Start begins one globally visible instrumentation session. It changes no
// device or transport state and fails if another capture is still active.
func Start() (*Session, error) {
	session := &Session{
		bySequence:   make(map[uint32]*pendingAdmission),
		byReceivedAt: make(map[time.Time]uint32),
		wake:         make(chan struct{}, 1),
	}
	if !active.CompareAndSwap(nil, session) {
		return nil, errors.New("an input latency capture is already active")
	}
	return session, nil
}

// Close removes this session from the process hook. It is idempotent.
func (session *Session) Close() {
	if session == nil {
		return
	}
	active.CompareAndSwap(session, nil)
	session.mu.Lock()
	session.closed = true
	session.signalLocked()
	session.mu.Unlock()
}

func (session *Session) signalLocked() {
	select {
	case session.wake <- struct{}{}:
	default:
	}
}

func (session *Session) failLocked(err error) {
	if session.failure == nil {
		session.failure = err
	}
	session.signalLocked()
}

func (session *Session) bindSourceLocked(source uintptr) bool {
	if source == 0 {
		session.failLocked(errors.New("input latency source identity is zero"))
		return false
	}
	if !session.sourceBound {
		session.source = source
		session.sourceBound = true
		return true
	}
	if session.source != source {
		session.failLocked(fmt.Errorf(
			"input latency capture observed multiple DualSense sources: %#x then %#x",
			session.source, source))
		return false
	}
	return true
}

// RecordBrokerReceived is called only by the viiper_latency build-tag hook.
// It binds the session to the first source and rejects sequence/timestamp
// ambiguity rather than guessing which published frame a later claim owns.
func RecordBrokerReceived(source uintptr, sequence uint32, receivedAt time.Time,
	ticks int64) {
	session := active.Load()
	if session == nil {
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || !session.bindSourceLocked(source) {
		return
	}
	if receivedAt.IsZero() || ticks <= 0 {
		session.failLocked(fmt.Errorf(
			"V5 frame %d has an absent broker timestamp", sequence))
		return
	}
	if _, exists := session.bySequence[sequence]; exists {
		session.failLocked(fmt.Errorf("duplicate V5 input sequence %d", sequence))
		return
	}
	if previous, exists := session.byReceivedAt[receivedAt]; exists {
		session.failLocked(fmt.Errorf(
			"V5 input sequences %d and %d have an ambiguous receive timestamp",
			previous, sequence))
		return
	}
	record := &pendingAdmission{Admission: Admission{
		Sequence: sequence, ReceivedAt: receivedAt, BrokerReceivedTicks: ticks,
	}}
	session.bySequence[sequence] = record
	session.byReceivedAt[receivedAt] = sequence
	session.signalLocked()
}

// RecordTransportAdmitted correlates an accepted presentation claim with the
// exact broker receive boundary copied through Claim. Unmatched commits are
// normal USB polling of an already presented state and are ignored.
func RecordTransportAdmitted(source uintptr, receivedAt time.Time,
	token, generation uint64, ticks int64) {
	session := active.Load()
	if session == nil {
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || !session.sourceBound {
		return
	}
	if session.source != source {
		session.failLocked(fmt.Errorf(
			"input latency admission came from source %#x, want %#x",
			source, session.source))
		return
	}
	sequence, exists := session.byReceivedAt[receivedAt]
	if !exists {
		return
	}
	record := session.bySequence[sequence]
	if record == nil {
		session.failLocked(fmt.Errorf("V5 frame %d lost its pending admission", sequence))
		return
	}
	if token == 0 || generation == 0 || ticks <= record.BrokerReceivedTicks {
		session.failLocked(fmt.Errorf(
			"V5 frame %d has invalid terminal admission evidence", sequence))
		return
	}
	if record.TransportAdmittedTicks != 0 {
		session.failLocked(fmt.Errorf(
			"V5 frame %d was terminally admitted more than once", sequence))
		return
	}
	record.TransportAdmittedTicks = ticks
	record.PresentationToken = token
	record.PresentationGeneration = generation
	session.signalLocked()
}

// Await returns one complete, unconsumed admission or the first recorder
// failure. A sequence can never be claimed twice by the probe.
func (session *Session) Await(ctx context.Context, sequence uint32) (Admission, error) {
	if session == nil {
		return Admission{}, errors.New("nil input latency session")
	}
	for {
		session.mu.Lock()
		if session.failure != nil {
			err := session.failure
			session.mu.Unlock()
			return Admission{}, err
		}
		record := session.bySequence[sequence]
		if record != nil && record.TransportAdmittedTicks != 0 {
			if record.consumed {
				session.mu.Unlock()
				return Admission{}, fmt.Errorf(
					"V5 frame %d admission was already consumed", sequence)
			}
			record.consumed = true
			result := record.Admission
			session.mu.Unlock()
			return result, nil
		}
		closed := session.closed
		session.mu.Unlock()
		if closed {
			return Admission{}, errors.New("input latency session closed before admission")
		}
		select {
		case <-ctx.Done():
			return Admission{}, fmt.Errorf(
				"wait for V5 frame %d admission: %w", sequence, ctx.Err())
		case <-session.wake:
		}
	}
}

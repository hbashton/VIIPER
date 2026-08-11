package inputstatequeue

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

var (
	ErrRevisionExhausted   = errors.New("input state revision is exhausted")
	ErrGenerationChanged   = errors.New("input transition generation changed")
	ErrBackpressureTimeout = errors.New("input transition backpressure timed out")
)

const defaultBackpressureTimeout = 5 * time.Second

type entry[T any] struct {
	state    T
	revision uint64
}

// Queue retains every discrete controller transition in a fixed ring while
// coalescing analog/motion-only updates into one latest-state snapshot. Signal
// is edge-triggered; revision bookkeeping re-arms it until all retained work
// has been observed.
type Queue[T any] struct {
	mu sync.Mutex

	transitions []entry[T]
	signal      chan struct{}
	space       chan struct{}
	head        int
	count       int

	latest              T
	latestRevision      uint64
	deliveredRevision   uint64
	edgeSignature       uint64
	generation          uint64
	backpressureTimeout time.Duration
}

func New[T any](initial T, edgeSignature uint64, capacity int) *Queue[T] {
	if capacity <= 0 {
		panic("input transition queue capacity must be positive")
	}
	return &Queue[T]{
		transitions:         make([]entry[T], capacity),
		signal:              make(chan struct{}, 1),
		space:               make(chan struct{}, 1),
		latest:              initial,
		latestRevision:      1,
		deliveredRevision:   1,
		edgeSignature:       edgeSignature,
		backpressureTimeout: defaultBackpressureTimeout,
	}
}

// Publish accepts one source-ordered state. A changed edge signature is
// retained exactly; an unchanged signature updates only the latest snapshot.
// Capacity pressure is propagated to the producer before any state is
// accepted, so no half-committed controller state can be published.
func (q *Queue[T]) Publish(state T, edgeSignature uint64) error {
	return q.PublishUntil(nil, state, edgeSignature)
}

// PublishUntil applies bounded backpressure for a discrete transition. Closing
// done cancels a publication which has not yet been accepted; nil waits until
// lifecycle invalidation or the consumer frees a slot.
func (q *Queue[T]) PublishUntil(
	done <-chan struct{}, state T, edgeSignature uint64,
) error {
	var generation uint64
	generationKnown := false
	var backpressureTimer *time.Timer
	defer func() {
		if backpressureTimer != nil {
			backpressureTimer.Stop()
		}
	}()
	for {
		if done != nil {
			select {
			case <-done:
				return context.Canceled
			default:
			}
		}
		q.mu.Lock()
		if !generationKnown {
			generation = q.generation
			generationKnown = true
		} else if generation != q.generation {
			q.mu.Unlock()
			return ErrGenerationChanged
		}
		transition := edgeSignature != q.edgeSignature
		if !transition || q.count < len(q.transitions) {
			if q.latestRevision == math.MaxUint64 {
				q.mu.Unlock()
				return ErrRevisionExhausted
			}
			q.latestRevision++
			q.latest = state
			q.edgeSignature = edgeSignature
			if transition {
				index := (q.head + q.count) % len(q.transitions)
				q.transitions[index] = entry[T]{state: state, revision: q.latestRevision}
				q.count++
			}
			q.notify()
			q.mu.Unlock()
			return nil
		}
		q.mu.Unlock()

		if backpressureTimer == nil {
			backpressureTimer = time.NewTimer(q.backpressureTimeout)
		}
		select {
		case <-done:
			return context.Canceled
		case <-q.space:
		case <-backpressureTimer.C:
			return ErrBackpressureTimeout
		}
	}
}

// Wait returns the oldest retained transition, otherwise the latest snapshot.
// A nil deadline preserves the legacy context-deadline behavior used by the
// USB/IP poller; native callers pass a reusable endpoint timer channel.
func (q *Queue[T]) Wait(
	ctx context.Context, deadline <-chan time.Time,
) (state T, transition bool, err error) {
	select {
	case <-ctx.Done():
		if deadline != nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return state, false, ctx.Err()
		}
		return q.take(true)
	case <-deadline:
		if err := ctx.Err(); err != nil {
			return state, false, err
		}
		return q.take(true)
	case <-q.signal:
		if err := ctx.Err(); err != nil {
			if deadline == nil && errors.Is(err, context.DeadlineExceeded) {
				return q.take(false)
			}
			return state, false, err
		}
		return q.take(false)
	}
}

// Invalidate establishes a lifecycle generation boundary. Retained pre-reset
// transitions are discarded, while the current snapshot remains available to
// the first post-boundary host poll.
func (q *Queue[T]) Invalidate() {
	q.mu.Lock()
	q.head = 0
	q.count = 0
	q.generation++
	q.deliveredRevision = q.latestRevision
	select {
	case <-q.signal:
	default:
	}
	q.notifySpace()
	q.mu.Unlock()
}

func (q *Queue[T]) take(drainSignal bool) (state T, transition bool, err error) {
	q.mu.Lock()
	if drainSignal {
		select {
		case <-q.signal:
		default:
		}
	}
	if q.count > 0 {
		item := q.transitions[q.head]
		var zero entry[T]
		q.transitions[q.head] = zero
		q.head = (q.head + 1) % len(q.transitions)
		q.count--
		q.deliveredRevision = item.revision
		state = item.state
		transition = true
	} else {
		state = q.latest
		q.deliveredRevision = q.latestRevision
	}
	pending := q.count > 0 || q.latestRevision > q.deliveredRevision
	if transition {
		q.notifySpace()
	}
	if pending {
		q.notify()
	}
	q.mu.Unlock()
	return state, transition, nil
}

func (q *Queue[T]) notify() {
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *Queue[T]) notifySpace() {
	select {
	case q.space <- struct{}{}:
	default:
	}
}

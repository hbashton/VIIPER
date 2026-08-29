package inputpresentation

import (
	"errors"
	"sync"
	"time"
)

const (
	// FixedReportMaximumSize keeps the presentation hot path allocation-free
	// while covering every interrupt report currently owned by VIIPER.
	FixedReportMaximumSize = 512
	// FixedReportTransitionCapacity is deliberately bounded. Continuous state is
	// coalesced separately and can never consume transition journal capacity.
	FixedReportTransitionCapacity = 64
)

// FixedReportEncoder writes one complete report and returns its exact size. It
// runs while the scheduler lock is held and therefore must not block, allocate,
// or call back into the scheduler.
type FixedReportEncoder[T any] func(state *T, destination []byte) int

// TransitionClassifier identifies semantic boundaries which may not be
// coalesced. It runs while the scheduler lock is held and must be pure.
type TransitionClassifier[T any] func(previous, next T) bool

type fixedReportEntry[T any] struct {
	state      T
	receivedAt time.Time
	ordinal    uint64
	ordered    bool
}

// FixedReportScheduler implements the common latest-state plus ordered-journal
// policy used by fixed-size controller interrupt reports. Reports are encoded
// into immutable claim storage; an ordered defer retries those exact bytes
// ahead of later work. Continuous state remains replaceable.
//
// The zero value is not usable. Construct a scheduler with
// NewFixedReportScheduler.
type FixedReportScheduler[T any] struct {
	mu sync.Mutex

	reportSize int
	encode     FixedReportEncoder[T]
	transition TransitionClassifier[T]

	journal [FixedReportTransitionCapacity]fixedReportEntry[T]
	head    int
	count   int

	latest    fixedReportEntry[T]
	hasLatest bool
	retry     fixedReportEntry[T]
	hasRetry  bool
	retryData [FixedReportMaximumSize]byte

	claimed         fixedReportEntry[T]
	claimedData     [FixedReportMaximumSize]byte
	claimToken      uint64
	claimSource     fixedReportClaimSource
	claimGeneration uint64
	hasClaim        bool

	previous    T
	hasPrevious bool
	last        fixedReportEntry[T]
	lastData    [FixedReportMaximumSize]byte

	nextOrdinal uint64
	nextToken   uint64
	generation  uint64

	received  uint64
	selected  uint64
	replaced  uint64
	overflows uint64
	highWater int
}

type fixedReportClaimSource uint8

const (
	fixedReportClaimRetry fixedReportClaimSource = iota + 1
	fixedReportClaimJournal
	fixedReportClaimLatest
	fixedReportClaimIdle
)

// FixedReportSchedulerSnapshot is a lock-bounded diagnostics value. It contains
// counters only; formatting and logging belong off the input path.
type FixedReportSchedulerSnapshot struct {
	Generation          uint64
	Received            uint64
	Selected            uint64
	TransitionDepth     int
	TransitionHighWater int
	ContinuousPending   bool
	ContinuousReplaced  uint64
	Overflows           uint64
}

func NewFixedReportScheduler[T any](reportSize int, neutral T,
	encode FixedReportEncoder[T], transition TransitionClassifier[T],
	now time.Time) (*FixedReportScheduler[T], error) {
	if reportSize < 1 || reportSize > FixedReportMaximumSize {
		return nil, errors.New("inputpresentation: fixed report size outside supported range")
	}
	if encode == nil {
		return nil, errors.New("inputpresentation: fixed report encoder is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	s := &FixedReportScheduler[T]{
		reportSize:  reportSize,
		encode:      encode,
		transition:  transition,
		previous:    neutral,
		hasPrevious: true,
		generation:  1,
		last: fixedReportEntry[T]{
			state: neutral, receivedAt: now, ordinal: 1,
		},
		nextOrdinal: 1,
	}
	if n := s.encode(&s.last.state, s.lastData[:reportSize]); n != reportSize {
		return nil, errors.New("inputpresentation: encoder returned a non-canonical report size")
	}
	return s, nil
}

// Publish accepts one complete semantic state. Continuous updates replace an
// older continuous snapshot. Before an ordered boundary is journaled, any
// pending continuous state is promoted ahead of it; this preserves trigger
// peaks and stick positions observed between press and release reports.
func (s *FixedReportScheduler[T]) Publish(state T, receivedAt time.Time) bool {
	if s == nil {
		return false
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ordered := s.transition != nil && s.transition(s.previous, state)
	required := 1
	if ordered && s.hasLatest {
		required++
	}
	if ordered && len(s.journal)-s.count < required {
		s.overflows++
		return false
	}

	s.nextOrdinal++
	if s.nextOrdinal == 0 {
		s.nextOrdinal = 1
	}
	entry := fixedReportEntry[T]{
		state: state, receivedAt: receivedAt, ordinal: s.nextOrdinal,
		ordered: ordered,
	}
	if ordered {
		if s.hasLatest {
			promoted := s.latest
			promoted.ordered = true
			s.pushJournal(promoted)
			s.latest = fixedReportEntry[T]{}
			s.hasLatest = false
		}
		s.pushJournal(entry)
	} else {
		if s.hasLatest {
			s.replaced++
		}
		s.latest = entry
		s.hasLatest = true
	}
	s.previous = state
	s.received++
	return true
}

func (s *FixedReportScheduler[T]) pushJournal(entry fixedReportEntry[T]) {
	index := (s.head + s.count) % len(s.journal)
	s.journal[index] = entry
	s.count++
	if s.count > s.highWater {
		s.highWater = s.count
	}
}

func (s *FixedReportScheduler[T]) popJournal() fixedReportEntry[T] {
	entry := s.journal[s.head]
	s.journal[s.head] = fixedReportEntry[T]{}
	s.head = (s.head + 1) % len(s.journal)
	s.count--
	return entry
}

// ClaimInputPresentation selects and copies one immutable report without
// waiting for downstream I/O. Only one claim may be active at a time.
func (s *FixedReportScheduler[T]) ClaimInputPresentation(destination []byte,
	selectedAt time.Time) Claim {
	if s == nil || len(destination) < s.reportSize {
		return Claim{}
	}
	if selectedAt.IsZero() {
		selectedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasClaim {
		return Claim{}
	}

	s.claimSource = fixedReportClaimIdle
	s.claimed = s.last
	s.claimed.ordered = false
	copy(s.claimedData[:s.reportSize], s.lastData[:s.reportSize])
	switch {
	case s.hasRetry:
		s.claimSource = fixedReportClaimRetry
		s.claimed = s.retry
		copy(s.claimedData[:s.reportSize], s.retryData[:s.reportSize])
		s.retry = fixedReportEntry[T]{}
		clear(s.retryData[:s.reportSize])
		s.hasRetry = false
	case s.count > 0:
		s.claimSource = fixedReportClaimJournal
		s.claimed = s.popJournal()
		clear(s.claimedData[:s.reportSize])
		if n := s.encode(&s.claimed.state,
			s.claimedData[:s.reportSize]); n != s.reportSize {
			s.restoreFailedSelection()
			return Claim{}
		}
	case s.hasLatest:
		s.claimSource = fixedReportClaimLatest
		s.claimed = s.latest
		s.latest = fixedReportEntry[T]{}
		s.hasLatest = false
		clear(s.claimedData[:s.reportSize])
		if n := s.encode(&s.claimed.state,
			s.claimedData[:s.reportSize]); n != s.reportSize {
			s.restoreFailedSelection()
			return Claim{}
		}
	}

	s.nextToken++
	if s.nextToken == 0 {
		s.nextToken = 1
	}
	s.claimToken = s.nextToken
	s.claimGeneration = s.generation
	s.hasClaim = true
	s.selected++
	copy(destination[:s.reportSize], s.claimedData[:s.reportSize])
	return Claim{
		Token: s.claimToken, Generation: s.claimGeneration,
		Size: s.reportSize, ReceivedAt: s.claimed.receivedAt,
		SelectedAt: selectedAt, Ordered: s.claimed.ordered,
	}
}

func (s *FixedReportScheduler[T]) restoreFailedSelection() {
	switch s.claimSource {
	case fixedReportClaimJournal:
		s.head = (s.head - 1 + len(s.journal)) % len(s.journal)
		s.journal[s.head] = s.claimed
		s.count++
	case fixedReportClaimLatest:
		s.latest = s.claimed
		s.hasLatest = true
	case fixedReportClaimRetry:
		s.retry = s.claimed
		copy(s.retryData[:s.reportSize], s.claimedData[:s.reportSize])
		s.hasRetry = true
	}
	s.claimed = fixedReportEntry[T]{}
	clear(s.claimedData[:s.reportSize])
	s.claimSource = 0
}

// ResolveInputPresentation applies the one terminal outcome for claim. A
// deferred ordered claim is recovered byte-for-byte ahead of all newer work.
func (s *FixedReportScheduler[T]) ResolveInputPresentation(claim Claim,
	outcome Outcome, _ time.Time) bool {
	if s == nil || !outcome.Valid() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasClaim || claim.Token == 0 || claim.Token != s.claimToken ||
		claim.Generation == 0 || claim.Generation != s.claimGeneration ||
		claim.Generation != s.generation {
		return false
	}

	switch outcome {
	case OutcomeCommit:
		s.last = s.claimed
		copy(s.lastData[:s.reportSize], s.claimedData[:s.reportSize])
	case OutcomeDefer:
		if s.claimed.ordered {
			s.retry = s.claimed
			copy(s.retryData[:s.reportSize], s.claimedData[:s.reportSize])
			s.hasRetry = true
		} else if s.claimSource != fixedReportClaimIdle && !s.hasRetry &&
			s.count == 0 && !s.hasLatest {
			s.latest = s.claimed
			s.hasLatest = true
		}
	case OutcomeRetire:
		s.advanceGeneration()
	}
	s.clearClaim()
	return true
}

func (s *FixedReportScheduler[T]) clearClaim() {
	s.claimed = fixedReportEntry[T]{}
	clear(s.claimedData[:s.reportSize])
	s.claimToken = 0
	s.claimSource = 0
	s.claimGeneration = 0
	s.hasClaim = false
}

// RetireInputPresentationGeneration drops in-flight/retry ownership from the
// retiring backend generation. Pending semantic states remain eligible for the
// successor; serialized bytes from the old generation never do.
func (s *FixedReportScheduler[T]) RetireInputPresentationGeneration(
	generation uint64, retiredAt time.Time) bool {
	if s == nil || generation == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return false
	}
	if s.hasClaim {
		s.clearClaim()
	}
	s.retry = fixedReportEntry[T]{}
	clear(s.retryData[:s.reportSize])
	s.hasRetry = false
	s.advanceGeneration()
	return true
}

func (s *FixedReportScheduler[T]) advanceGeneration() {
	s.generation++
	if s.generation == 0 {
		s.generation = 1
	}
}

func (s *FixedReportScheduler[T]) Generation() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	generation := s.generation
	s.mu.Unlock()
	return generation
}

func (s *FixedReportScheduler[T]) Snapshot() FixedReportSchedulerSnapshot {
	if s == nil {
		return FixedReportSchedulerSnapshot{}
	}
	s.mu.Lock()
	snapshot := FixedReportSchedulerSnapshot{
		Generation: s.generation, Received: s.received, Selected: s.selected,
		TransitionDepth:     s.count + boolToInt(s.hasRetry),
		TransitionHighWater: s.highWater, ContinuousPending: s.hasLatest,
		ContinuousReplaced: s.replaced, Overflows: s.overflows,
	}
	s.mu.Unlock()
	return snapshot
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

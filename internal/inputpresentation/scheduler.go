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
	producer   uint64
	ordered    bool
}

// FixedReportProducerLease identifies the producer epoch which owns
// publication. A fault or presentation-generation retirement revokes every
// previously issued lease. The zero value is invalid.
type FixedReportProducerLease struct {
	epoch uint64
}

// Valid reports whether the lease identifies a producer epoch.
func (lease FixedReportProducerLease) Valid() bool {
	return lease.epoch != 0
}

// FixedReportPublishDisposition is the detailed result of a lease-bound
// publication or resynchronization attempt.
type FixedReportPublishDisposition uint8

const (
	FixedReportPublishAcceptedContinuous FixedReportPublishDisposition = iota + 1
	FixedReportPublishAcceptedOrdered
	FixedReportPublishAcceptedResynchronization
	FixedReportPublishRejectedStaleProducer
	FixedReportPublishRejectedNeutralPending
	FixedReportPublishRejectedResynchronizationRequired
	FixedReportPublishRejectedResynchronizationNotRequired
	FixedReportPublishRejectedInvalidTimestamp
	FixedReportPublishRejectedOverflow
	FixedReportPublishFaultedOverflow
)

// Accepted reports whether the supplied complete state became scheduler-owned.
func (disposition FixedReportPublishDisposition) Accepted() bool {
	return disposition >= FixedReportPublishAcceptedContinuous &&
		disposition <= FixedReportPublishAcceptedResynchronization
}

// FixedReportFaultReason identifies a fail-closed history purge. Faults are
// diagnostic facts, not instructions to selectively discard an old edge.
type FixedReportFaultReason uint8

const (
	FixedReportFaultNone FixedReportFaultReason = iota
	FixedReportFaultOverflow
	FixedReportFaultOrderedAge
)

// FixedReportScheduler implements the common latest-state plus ordered-journal
// policy used by fixed-size controller interrupt reports. Reports are encoded
// into immutable claim storage; an ordered defer retries those exact bytes
// ahead of later work. Continuous state remains replaceable.
//
// The zero value is not usable. Construct a scheduler with
// NewFixedReportScheduler.
type FixedReportScheduler[T any] struct {
	mu sync.Mutex

	reportSize        int
	encode            FixedReportEncoder[T]
	transition        TransitionClassifier[T]
	maximumOrderedAge time.Duration
	faultOnOverflow   bool
	neutral           T
	neutralData       [FixedReportMaximumSize]byte

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
	// claimRequiresOrderedRecovery marks a continuous claim as a dependency of
	// a later accepted transition. If its downstream completion defers, its
	// immutable bytes must be retried ahead of that transition.
	claimRequiresOrderedRecovery bool
	claimAdmissionValidated      bool
	claimSelectedAt              time.Time
	claimAdmittedAt              time.Time
	hasClaim                     bool

	mandatoryNeutral        fixedReportEntry[T]
	mandatoryNeutralData    [FixedReportMaximumSize]byte
	mandatoryNeutralEncoded bool
	mandatoryNeutralPending bool
	resynchronizationNeeded bool
	// minimumResynchronizationAt fences a producer snapshot captured before
	// the history fault which made resynchronization necessary. It is cleared
	// only by an accepted fresh resynchronization or generation retirement.
	minimumResynchronizationAt time.Time

	previous    T
	hasPrevious bool
	last        fixedReportEntry[T]
	lastData    [FixedReportMaximumSize]byte

	nextOrdinal uint64
	nextToken   uint64
	generation  uint64
	producer    uint64
	// Producer timestamps are ordered within the producer sequence only.
	// Presentation boundaries are validated against the exact claim they
	// advance. Comparing either one to the other globally is invalid: a
	// producer callback can win this lock after a transport captured an older
	// selection, admission, completion, or lifecycle boundary.
	lastProducerAt time.Time

	received          uint64
	rejected          uint64
	selected          uint64
	replaced          uint64
	overflows         uint64
	staleFaults       uint64
	resyncs           uint64
	invalidTimestamps uint64
	lastFault         FixedReportFaultReason
	highWater         int
}

type fixedReportClaimSource uint8

const (
	fixedReportClaimRetry fixedReportClaimSource = iota + 1
	fixedReportClaimJournal
	fixedReportClaimLatest
	fixedReportClaimIdle
	fixedReportClaimMandatoryNeutral
)

// FixedReportSchedulerSnapshot is a lock-bounded diagnostics value. It contains
// counters only; formatting and logging belong off the input path.
type FixedReportSchedulerSnapshot struct {
	Generation          uint64
	ProducerEpoch       uint64
	Received            uint64
	Rejected            uint64
	Selected            uint64
	TransitionDepth     int
	TransitionHighWater int
	ContinuousPending   bool
	ContinuousReplaced  uint64
	Overflows           uint64
	StaleFaults         uint64
	Resynchronizations  uint64
	InvalidTimestamps   uint64
	MaximumOrderedAge   time.Duration
	MandatoryNeutral    bool
	Resynchronization   bool
	LastFault           FixedReportFaultReason
}

// NewFixedReportScheduler constructs the compatibility scheduler. Its ordered
// age policy is disabled, so it must not be treated as production-safe against
// wall-clock stale replay and retains its historical reject-only overflow
// behavior. New production callers must use either the fail-closed overflow or
// explicit ordered-age constructor together with lease-bound publication,
// CanAdmitInputPresentation, and explicit resynchronization.
func NewFixedReportScheduler[T any](reportSize int, neutral T,
	encode FixedReportEncoder[T], transition TransitionClassifier[T],
	now time.Time) (*FixedReportScheduler[T], error) {
	return newFixedReportScheduler(reportSize, neutral, encode, transition, 0,
		false, now)
}

// NewFixedReportSchedulerWithOverflowFault constructs a lease-bound scheduler
// with no ordered-age deadline, but with fail-closed overflow handling. This is
// the production compatibility policy for integrations which have not declared
// an age limit: capacity exhaustion still purges ambiguous history, presents a
// mandatory neutral, and requires one fresh producer resynchronization.
func NewFixedReportSchedulerWithOverflowFault[T any](reportSize int, neutral T,
	encode FixedReportEncoder[T], transition TransitionClassifier[T],
	now time.Time) (*FixedReportScheduler[T], error) {
	return newFixedReportScheduler(reportSize, neutral, encode, transition, 0,
		true, now)
}

// NewFixedReportSchedulerWithMaximumOrderedAge constructs a scheduler whose
// ordered entries and exact retries fault as one history unit once their age
// reaches maximumOrderedAge. The caller must pass receive, selection,
// admission, and completion boundaries from the same monotonic clock domain.
// No default deadline is invented by this package.
func NewFixedReportSchedulerWithMaximumOrderedAge[T any](reportSize int,
	neutral T, encode FixedReportEncoder[T], transition TransitionClassifier[T],
	maximumOrderedAge time.Duration, now time.Time,
) (*FixedReportScheduler[T], error) {
	if maximumOrderedAge <= 0 {
		return nil, errors.New("inputpresentation: maximum ordered age must be positive")
	}
	return newFixedReportScheduler(reportSize, neutral, encode, transition,
		maximumOrderedAge, true, now)
}

func newFixedReportScheduler[T any](reportSize int, neutral T,
	encode FixedReportEncoder[T], transition TransitionClassifier[T],
	maximumOrderedAge time.Duration, faultOnOverflow bool, now time.Time,
) (*FixedReportScheduler[T], error) {
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
		reportSize:        reportSize,
		encode:            encode,
		transition:        transition,
		maximumOrderedAge: maximumOrderedAge,
		faultOnOverflow:   faultOnOverflow,
		neutral:           neutral,
		previous:          neutral,
		hasPrevious:       true,
		generation:        1,
		producer:          1,
		last: fixedReportEntry[T]{
			state: neutral, receivedAt: now, ordinal: 1, producer: 1,
		},
		nextOrdinal: 1,
	}
	if n := s.encode(&s.last.state, s.lastData[:reportSize]); n != reportSize {
		return nil, errors.New("inputpresentation: encoder returned a non-canonical report size")
	}
	copy(s.neutralData[:reportSize], s.lastData[:reportSize])
	return s, nil
}

// Publish is the compatibility publication surface. It uses the producer epoch
// current at lock acquisition, but still fails closed after a history fault.
// It cannot distinguish a stale callback from the current producer; new callers
// must use PublishWithLease.
func (s *FixedReportScheduler[T]) Publish(state T, receivedAt time.Time) bool {
	if s == nil {
		return false
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publishLocked(FixedReportProducerLease{epoch: s.producer},
		state, receivedAt).Accepted()
}

// PublishWithLease accepts one complete semantic state from the identified
// producer epoch. Continuous updates replace an older continuous snapshot.
// Before an ordered boundary is journaled, any pending continuous state is
// promoted ahead of it; promotion and boundary admission are atomic.
func (s *FixedReportScheduler[T]) PublishWithLease(
	lease FixedReportProducerLease, state T, receivedAt time.Time,
) FixedReportPublishDisposition {
	if s == nil {
		return FixedReportPublishRejectedStaleProducer
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	s.mu.Lock()
	disposition := s.publishLocked(lease, state, receivedAt)
	s.mu.Unlock()
	return disposition
}

func (s *FixedReportScheduler[T]) publishLocked(lease FixedReportProducerLease,
	state T, receivedAt time.Time,
) FixedReportPublishDisposition {
	if !lease.Valid() || lease.epoch != s.producer {
		s.rejected++
		return FixedReportPublishRejectedStaleProducer
	}
	if s.mandatoryNeutralPending {
		s.rejected++
		return FixedReportPublishRejectedNeutralPending
	}
	if s.resynchronizationNeeded {
		s.rejected++
		return FixedReportPublishRejectedResynchronizationRequired
	}
	if !s.observeProducerTimestamp(receivedAt) {
		s.rejected++
		return FixedReportPublishRejectedInvalidTimestamp
	}

	ordered := s.hasPrevious && s.transition != nil &&
		s.transition(s.previous, state)
	required := 1
	if ordered && s.hasLatest {
		required++
	}
	if ordered && len(s.journal)-s.count < required {
		s.rejected++
		if !s.faultOnOverflow {
			// Compatibility callers have no lease/resynchronization integration.
			// Preserve their historical reject-only behavior; diagnostics make
			// clear that this is not the strict production contract.
			s.overflows++
			return FixedReportPublishRejectedOverflow
		}
		s.enterHistoryFault(FixedReportFaultOverflow, receivedAt)
		return FixedReportPublishFaultedOverflow
	}

	entry := s.newEntry(state, receivedAt, ordered)
	if ordered {
		if !s.hasLatest && s.hasClaim &&
			s.claimSource == fixedReportClaimLatest && !s.claimed.ordered {
			s.claimRequiresOrderedRecovery = true
		}
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
	s.hasPrevious = true
	s.received++
	if ordered {
		return FixedReportPublishAcceptedOrdered
	}
	return FixedReportPublishAcceptedContinuous
}

// ProducerLease returns the publication lease for the current producer epoch.
// A fault or presentation retirement invalidates the returned value.
func (s *FixedReportScheduler[T]) ProducerLease() FixedReportProducerLease {
	if s == nil {
		return FixedReportProducerLease{}
	}
	s.mu.Lock()
	lease := FixedReportProducerLease{epoch: s.producer}
	s.mu.Unlock()
	return lease
}

// RetireProducerLease ends one producer connection without rotating the USB
// presentation generation. Pending history and an unadmitted claim from that
// producer are purged, its lease is invalidated, and one canonical neutral is
// placed ahead of the successor. A fresh successor snapshot must explicitly
// resynchronize after that neutral commits.
func (s *FixedReportScheduler[T]) RetireProducerLease(
	lease FixedReportProducerLease, retiredAt time.Time,
) (FixedReportProducerLease, bool) {
	if s == nil || !lease.Valid() {
		return FixedReportProducerLease{}, false
	}
	if retiredAt.IsZero() {
		retiredAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if lease.epoch != s.producer {
		return FixedReportProducerLease{}, false
	}
	if s.lastProducerAt.After(retiredAt) ||
		(s.hasClaim && s.claimSelectedAt.After(retiredAt)) ||
		(s.hasClaim && s.claimAdmittedAt.After(retiredAt)) {
		s.invalidTimestamps++
	}
	s.advanceProducer()
	s.clearNonActiveHistory()
	if s.hasClaim {
		s.clearClaim()
	}
	s.previous = s.neutral
	s.hasPrevious = false
	s.mandatoryNeutral = s.newEntry(s.neutral, retiredAt, true)
	clear(s.mandatoryNeutralData[:s.reportSize])
	s.mandatoryNeutralEncoded = false
	s.mandatoryNeutralPending = true
	s.resynchronizationNeeded = false
	s.minimumResynchronizationAt = retiredAt
	return FixedReportProducerLease{epoch: s.producer}, true
}

// Resynchronize supplies the one complete, fresh producer snapshot required
// after the mandatory fault neutral commits. It is a new current baseline, not
// a reconstructed edge. It cannot bypass a neutral still awaiting commitment.
func (s *FixedReportScheduler[T]) Resynchronize(
	lease FixedReportProducerLease, state T, receivedAt time.Time,
) FixedReportPublishDisposition {
	if s == nil {
		return FixedReportPublishRejectedStaleProducer
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !lease.Valid() || lease.epoch != s.producer {
		s.rejected++
		return FixedReportPublishRejectedStaleProducer
	}
	if s.mandatoryNeutralPending {
		s.rejected++
		return FixedReportPublishRejectedNeutralPending
	}
	if !s.resynchronizationNeeded {
		s.rejected++
		return FixedReportPublishRejectedResynchronizationNotRequired
	}
	if receivedAt.Before(s.minimumResynchronizationAt) {
		s.invalidTimestamps++
		s.rejected++
		return FixedReportPublishRejectedInvalidTimestamp
	}
	if !s.observeProducerTimestamp(receivedAt) {
		s.rejected++
		return FixedReportPublishRejectedInvalidTimestamp
	}

	entry := s.newEntry(state, receivedAt, false)
	s.latest = entry
	s.hasLatest = true
	s.previous = state
	s.hasPrevious = true
	s.resynchronizationNeeded = false
	s.minimumResynchronizationAt = time.Time{}
	s.received++
	s.resyncs++
	return FixedReportPublishAcceptedResynchronization
}

func (s *FixedReportScheduler[T]) newEntry(state T, receivedAt time.Time,
	ordered bool,
) fixedReportEntry[T] {
	s.nextOrdinal++
	if s.nextOrdinal == 0 {
		s.nextOrdinal = 1
	}
	return fixedReportEntry[T]{
		state: state, receivedAt: receivedAt, ordinal: s.nextOrdinal,
		producer: s.producer, ordered: ordered,
	}
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

// HasPendingInputPresentation reports queued semantic work, excluding a claim
// already owned by the backend and excluding an unchanged idle image. Callers
// requiring an atomic decision with ClaimInputPresentation must serialize their
// producer and selector around both operations; this query consumes no work.
func (s *FixedReportScheduler[T]) HasPendingInputPresentation() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mandatoryNeutralPending || s.hasRetry || s.count > 0 || s.hasLatest
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
	if s.pendingPresentationFaultRequired(selectedAt) {
		s.enterHistoryFault(FixedReportFaultOrderedAge, selectedAt)
	}

	s.claimSource = fixedReportClaimIdle
	s.claimRequiresOrderedRecovery = false
	s.claimed = s.last
	s.claimed.ordered = false
	copy(s.claimedData[:s.reportSize], s.lastData[:s.reportSize])
	switch {
	case s.mandatoryNeutralPending:
		s.claimSource = fixedReportClaimMandatoryNeutral
		s.claimed = s.mandatoryNeutral
		if !s.mandatoryNeutralEncoded {
			clear(s.mandatoryNeutralData[:s.reportSize])
			if n := s.encode(&s.claimed.state,
				s.mandatoryNeutralData[:s.reportSize]); n != s.reportSize {
				s.claimed = fixedReportEntry[T]{}
				s.claimSource = 0
				clear(s.mandatoryNeutralData[:s.reportSize])
				return Claim{}
			}
			s.mandatoryNeutralEncoded = true
		}
		copy(s.claimedData[:s.reportSize],
			s.mandatoryNeutralData[:s.reportSize])
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
	s.claimSelectedAt = selectedAt
	s.claimAdmittedAt = time.Time{}
	s.hasClaim = true
	s.selected++
	copy(destination[:s.reportSize], s.claimedData[:s.reportSize])
	return Claim{
		Token: s.claimToken, Generation: s.claimGeneration,
		Size: s.reportSize, ReceivedAt: s.claimed.receivedAt,
		SelectedAt: selectedAt, Ordered: s.claimed.ordered,
	}
}

// CanAdmitInputPresentation revalidates exact claim ownership, generation, and
// ordered age at the downstream admission boundary. A caller must invoke it in
// the final write-admission predicate, after any pause and immediately before
// exposing claim bytes. Returning false means those bytes must not be written.
//
// If an ordered claim has aged beyond the explicitly configured limit, the
// entire history faults to a mandatory neutral and this claim is terminally
// revoked. With the compatibility constructor, age validation is disabled but
// exact token and generation validation still applies.
func (s *FixedReportScheduler[T]) CanAdmitInputPresentation(claim Claim,
	admissionAt time.Time) bool {
	if s == nil {
		return false
	}
	if admissionAt.IsZero() {
		admissionAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.claimMatches(claim) || s.claimAdmissionValidated {
		return false
	}
	if admissionAt.Before(s.claimSelectedAt) {
		s.invalidTimestamps++
		return false
	}
	if s.claimOrderedAgeExceeded(admissionAt) {
		s.enterHistoryFault(FixedReportFaultOrderedAge, admissionAt)
		return false
	}
	s.claimAdmissionValidated = true
	s.claimAdmittedAt = admissionAt
	return true
}

// StateForInputPresentationClaim returns the immutable complete semantic state
// owned by an exact live claim. It does not select, admit, or resolve work. A
// device can use this to version a control-report cache alongside the encoded
// interrupt bytes without reading a newer producer-side latest state.
func (s *FixedReportScheduler[T]) StateForInputPresentationClaim(
	claim Claim,
) (T, bool) {
	var zero T
	if s == nil {
		return zero, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.claimMatches(claim) {
		return zero, false
	}
	return s.claimed.state, true
}

// pendingPresentationFaultRequired validates the entry which this call would
// actually select. A future-dated producer entry makes freshness unprovable,
// so strict mode purges the whole history and presents its canonical neutral.
// Continuous entries are not age-limited, but they are still subject to this
// future-timestamp check.
func (s *FixedReportScheduler[T]) pendingPresentationFaultRequired(
	at time.Time,
) bool {
	if s.maximumOrderedAge <= 0 || s.mandatoryNeutralPending {
		return false
	}
	var entry fixedReportEntry[T]
	orderedDependency := false
	switch {
	case s.hasRetry:
		entry = s.retry
		orderedDependency = true
	case s.count > 0:
		entry = s.journal[s.head]
		orderedDependency = true
	case s.hasLatest:
		entry = s.latest
	default:
		return false
	}
	if entry.receivedAt.IsZero() || at.Before(entry.receivedAt) {
		s.invalidTimestamps++
		return true
	}
	return orderedDependency &&
		at.Sub(entry.receivedAt) >= s.maximumOrderedAge
}

func (s *FixedReportScheduler[T]) claimOrderedAgeExceeded(at time.Time) bool {
	if s.maximumOrderedAge <= 0 ||
		s.claimSource == fixedReportClaimMandatoryNeutral ||
		(!s.claimed.ordered && !s.claimRequiresOrderedRecovery) {
		return false
	}
	return s.ageExceeded(s.claimed.receivedAt, at)
}

func (s *FixedReportScheduler[T]) ageExceeded(receivedAt, boundary time.Time) bool {
	if receivedAt.IsZero() || boundary.IsZero() || boundary.Before(receivedAt) {
		// A strict ordered-age decision cannot prove freshness when timestamps
		// are missing or move backward. Faulting the whole history is safer than
		// making a future-dated transition immortal.
		return true
	}
	return boundary.Sub(receivedAt) >= s.maximumOrderedAge
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
	s.claimRequiresOrderedRecovery = false
	s.claimAdmissionValidated = false
}

// ResolveInputPresentation applies the one terminal outcome for claim. A
// deferred ordered claim is recovered byte-for-byte ahead of all newer work.
func (s *FixedReportScheduler[T]) ResolveInputPresentation(claim Claim,
	outcome Outcome, completedAt time.Time) bool {
	if s == nil || !outcome.Valid() {
		return false
	}
	if completedAt.IsZero() {
		completedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.claimMatches(claim) {
		return false
	}
	if outcome == OutcomeCommit && s.maximumOrderedAge > 0 &&
		!s.claimAdmissionValidated {
		// A strict scheduler never accepts a completion for bytes which were
		// not revalidated at the actual downstream admission boundary.
		return false
	}
	// A matching terminal callback must never leave the source wedged merely
	// because an unrelated producer won the lock after this boundary was
	// captured. Validate against this claim's own boundary sequence and clamp
	// a malformed regression conservatively while still consuming the claim.
	minimumCompletedAt := s.claimSelectedAt
	if s.claimAdmissionValidated && s.claimAdmittedAt.After(minimumCompletedAt) {
		minimumCompletedAt = s.claimAdmittedAt
	}
	if completedAt.Before(minimumCompletedAt) {
		s.invalidTimestamps++
		completedAt = minimumCompletedAt
	}
	if outcome != OutcomeRetire && s.claimOrderedAgeExceeded(completedAt) {
		s.enterHistoryFault(FixedReportFaultOrderedAge, completedAt)
		return true
	}

	switch outcome {
	case OutcomeCommit:
		s.last = s.claimed
		s.last.ordered = false
		copy(s.lastData[:s.reportSize], s.claimedData[:s.reportSize])
		if s.claimSource == fixedReportClaimMandatoryNeutral {
			s.mandatoryNeutralPending = false
			s.mandatoryNeutralEncoded = false
			clear(s.mandatoryNeutralData[:s.reportSize])
			s.resynchronizationNeeded = true
		}
	case OutcomeDefer:
		if s.claimSource == fixedReportClaimMandatoryNeutral {
			// The canonical neutral remains pending unchanged and ahead of all
			// producer work until a later admitted completion commits it.
		} else if s.claimed.ordered || s.claimRequiresOrderedRecovery {
			s.retry = s.claimed
			// Recovery is now an ordering dependency even when the original
			// latest-state claim was continuous. Preserve that property across
			// repeated downstream deferrals.
			s.retry.ordered = true
			copy(s.retryData[:s.reportSize], s.claimedData[:s.reportSize])
			s.hasRetry = true
		} else if s.claimSource != fixedReportClaimIdle && !s.hasRetry &&
			s.count == 0 && !s.hasLatest {
			// With no newer semantic state to supersede it, retain the exact
			// serialized continuous report. Re-encoding would advance stateful
			// device counters/timestamps even though the transport merely retried
			// the same report. A later publication may still supersede a continuous
			// claim before this branch is reached.
			s.retry = s.claimed
			copy(s.retryData[:s.reportSize], s.claimedData[:s.reportSize])
			s.hasRetry = true
		}
	case OutcomeRetire:
		s.retireCurrentGeneration(completedAt)
		return true
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
	s.claimRequiresOrderedRecovery = false
	s.claimAdmissionValidated = false
	s.claimSelectedAt = time.Time{}
	s.claimAdmittedAt = time.Time{}
	s.hasClaim = false
}

func (s *FixedReportScheduler[T]) claimMatches(claim Claim) bool {
	return s.hasClaim && claim.Token != 0 && claim.Token == s.claimToken &&
		claim.Generation != 0 && claim.Generation == s.claimGeneration &&
		claim.Generation == s.generation && claim.Size == s.reportSize &&
		s.claimed.producer == s.producer
}

func (s *FixedReportScheduler[T]) enterHistoryFault(
	reason FixedReportFaultReason, faultAt time.Time,
) {
	if faultAt.IsZero() {
		faultAt = time.Now()
	}
	s.lastFault = reason
	switch reason {
	case FixedReportFaultOverflow:
		s.overflows++
	case FixedReportFaultOrderedAge:
		s.staleFaults++
	}

	s.advanceProducer()
	s.clearNonActiveHistory()
	if s.hasClaim {
		s.clearClaim()
	}
	s.previous = s.neutral
	s.hasPrevious = false
	s.mandatoryNeutral = s.newEntry(s.neutral, faultAt, true)
	clear(s.mandatoryNeutralData[:s.reportSize])
	s.mandatoryNeutralEncoded = false
	s.mandatoryNeutralPending = true
	s.resynchronizationNeeded = false
	s.minimumResynchronizationAt = faultAt
}

func (s *FixedReportScheduler[T]) clearNonActiveHistory() {
	clear(s.journal[:])
	s.head = 0
	s.count = 0
	s.latest = fixedReportEntry[T]{}
	s.hasLatest = false
	s.retry = fixedReportEntry[T]{}
	clear(s.retryData[:s.reportSize])
	s.hasRetry = false
}

// RetireInputPresentationGeneration collapses the retiring generation to one
// current semantic snapshot. Historical transitions, retry bytes, and an
// in-flight claim are discarded so a successor cannot replay a phantom tap.
// The newest complete producer state remains eligible as non-ordered current
// state for the successor.
func (s *FixedReportScheduler[T]) RetireInputPresentationGeneration(
	generation uint64, retiredAt time.Time) bool {
	_, retired := s.RetireInputPresentationGenerationWithCurrentState(
		generation, retiredAt)
	return retired
}

// RetireInputPresentationGenerationWithCurrentState performs the same exact
// generation fence and also returns the complete semantic state selected for
// the successor. Devices with a separate, versioned control-report cache can
// encode that state without guessing from producer-side mirrors or consuming
// the successor journal.
func (s *FixedReportScheduler[T]) RetireInputPresentationGenerationWithCurrentState(
	generation uint64, retiredAt time.Time,
) (T, bool) {
	var zero T
	if s == nil || generation == 0 {
		return zero, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return zero, false
	}
	if retiredAt.IsZero() {
		retiredAt = time.Now()
	}
	// Lifecycle capture can legitimately precede a producer callback which
	// wins this lock. Retirement is terminal for the matching generation.
	// Diagnose that ordering, but retain the real lifecycle boundary: carrying
	// a later old-epoch producer timestamp into the successor would poison its
	// otherwise-fresh publications.
	if s.lastProducerAt.After(retiredAt) ||
		(s.hasClaim && s.claimSelectedAt.After(retiredAt)) ||
		(s.hasClaim && s.claimAdmittedAt.After(retiredAt)) {
		s.invalidTimestamps++
	}
	return s.retireCurrentGeneration(retiredAt), true
}

func (s *FixedReportScheduler[T]) retireCurrentGeneration(retiredAt time.Time) T {
	currentState := s.previous
	if !s.hasPrevious {
		currentState = s.neutral
	}
	return s.retireCurrentGenerationWithBaseline(currentState, retiredAt)
}

// RetireInputPresentationGenerationWithBaseline fences exactly one generation
// and supplies the complete state captured by an authoritative lifecycle owner.
// This is for protocols whose START response establishes a new current image;
// it must not be used to bypass an ordinary journal or recover an overflow by
// inventing missing transitions. The caller serializes baseline capture with
// producer publication. No report is encoded until subsequent selection.
func (s *FixedReportScheduler[T]) RetireInputPresentationGenerationWithBaseline(
	generation uint64, baseline T, retiredAt time.Time,
) bool {
	if s == nil || generation == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return false
	}
	s.retireCurrentGenerationWithBaseline(baseline, retiredAt)
	return true
}

func (s *FixedReportScheduler[T]) retireCurrentGenerationWithBaseline(currentState T, retiredAt time.Time) T {
	if retiredAt.IsZero() {
		retiredAt = time.Now()
	}
	s.clearNonActiveHistory()
	if s.hasClaim {
		s.clearClaim()
	}
	s.mandatoryNeutral = fixedReportEntry[T]{}
	clear(s.mandatoryNeutralData[:s.reportSize])
	s.mandatoryNeutralEncoded = false
	s.mandatoryNeutralPending = false
	s.resynchronizationNeeded = false
	s.minimumResynchronizationAt = time.Time{}
	s.advanceGeneration()
	s.advanceProducer()
	current := s.newEntry(currentState, retiredAt, false)
	s.latest = current
	s.hasLatest = true
	s.last = current
	s.previous = currentState
	s.hasPrevious = true
	// The successor's complete current state is pending in latest and must be
	// encoded only when a transport actually claims it. Encoding here would
	// advance stateful report counters/timestamps at a lifecycle boundary where
	// no bytes were presented. Until latest commits, no idle report from the
	// retired generation is eligible.
	clear(s.lastData[:s.reportSize])
	return currentState
}

func (s *FixedReportScheduler[T]) advanceGeneration() {
	s.generation = advanceNonzero(s.generation)
}

func (s *FixedReportScheduler[T]) advanceProducer() {
	s.producer = advanceNonzero(s.producer)
	// Producer timestamp ordering is epoch-local. A future or malformed sample
	// which caused an epoch-ending fault must not poison fresh resynchronization
	// or the successor presentation generation.
	s.lastProducerAt = time.Time{}
}

func advanceNonzero(value uint64) uint64 {
	value++
	if value == 0 {
		return 1
	}
	return value
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
		Generation: s.generation, ProducerEpoch: s.producer,
		Received: s.received, Rejected: s.rejected, Selected: s.selected,
		TransitionDepth: s.count + boolToInt(s.hasRetry) +
			boolToInt(s.mandatoryNeutralPending),
		TransitionHighWater: s.highWater, ContinuousPending: s.hasLatest,
		ContinuousReplaced: s.replaced, Overflows: s.overflows,
		StaleFaults: s.staleFaults, Resynchronizations: s.resyncs,
		InvalidTimestamps: s.invalidTimestamps,
		MaximumOrderedAge: s.maximumOrderedAge,
		MandatoryNeutral:  s.mandatoryNeutralPending,
		Resynchronization: s.resynchronizationNeeded,
		LastFault:         s.lastFault,
	}
	s.mu.Unlock()
	return snapshot
}

func (s *FixedReportScheduler[T]) observeProducerTimestamp(at time.Time) bool {
	if s.maximumOrderedAge <= 0 {
		return true
	}
	if at.IsZero() || (!s.lastProducerAt.IsZero() &&
		at.Before(s.lastProducerAt)) {
		s.invalidTimestamps++
		return false
	}
	s.lastProducerAt = at
	return true
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

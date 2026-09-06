package inputpresentation

import (
	"bytes"
	"testing"
	"time"
)

type schedulerTestState struct {
	buttons uint8
	axis    uint8
}

func newTestScheduler(t *testing.T) *FixedReportScheduler[schedulerTestState] {
	t.Helper()
	scheduler, err := NewFixedReportScheduler(2, schedulerTestState{},
		func(state *schedulerTestState, destination []byte) int {
			destination[0] = state.buttons
			destination[1] = state.axis
			return 2
		},
		func(previous, next schedulerTestState) bool {
			return previous.buttons != next.buttons
		}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}

func newStrictTestScheduler(t *testing.T, maximumOrderedAge time.Duration,
) *FixedReportScheduler[schedulerTestState] {
	t.Helper()
	scheduler, err := NewFixedReportSchedulerWithMaximumOrderedAge(
		2, schedulerTestState{},
		func(state *schedulerTestState, destination []byte) int {
			destination[0] = state.buttons
			destination[1] = state.axis
			return 2
		},
		func(previous, next schedulerTestState) bool {
			return previous.buttons != next.buttons
		}, maximumOrderedAge, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}

func newOverflowFaultTestScheduler(
	t *testing.T,
) *FixedReportScheduler[schedulerTestState] {
	t.Helper()
	scheduler, err := NewFixedReportSchedulerWithOverflowFault(
		2, schedulerTestState{},
		func(state *schedulerTestState, destination []byte) int {
			destination[0] = state.buttons
			destination[1] = state.axis
			return 2
		},
		func(previous, next schedulerTestState) bool {
			return previous.buttons != next.buttons
		}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}

func admitAndResolve(t *testing.T,
	scheduler *FixedReportScheduler[schedulerTestState], claim Claim,
	outcome Outcome, admittedAt, completedAt time.Time,
) {
	t.Helper()
	if !scheduler.CanAdmitInputPresentation(claim, admittedAt) {
		t.Fatal("expected claim admission to succeed")
	}
	if !scheduler.ResolveInputPresentation(claim, outcome, completedAt) {
		t.Fatal("expected claim resolution to succeed")
	}
}

func claimAndResolve(t *testing.T, scheduler *FixedReportScheduler[schedulerTestState],
	outcome Outcome, at time.Time) ([]byte, Claim) {
	t.Helper()
	report := make([]byte, 2)
	claim := scheduler.ClaimInputPresentation(report, at)
	if !claim.Valid() {
		t.Fatal("expected a valid claim")
	}
	if !scheduler.ResolveInputPresentation(claim, outcome, at.Add(time.Microsecond)) {
		t.Fatal("expected claim resolution to succeed")
	}
	return report, claim
}

func TestFixedReportSchedulerPreservesContinuousPeakBeforeRelease(t *testing.T) {
	s := newTestScheduler(t)
	base := time.Unix(2, 0)
	if !s.Publish(schedulerTestState{buttons: 1, axis: 10}, base) ||
		!s.Publish(schedulerTestState{buttons: 1, axis: 255}, base.Add(time.Microsecond)) ||
		!s.Publish(schedulerTestState{}, base.Add(2*time.Microsecond)) {
		t.Fatal("publish failed")
	}

	want := [][]byte{{1, 10}, {1, 255}, {0, 0}}
	for index, expected := range want {
		got, claim := claimAndResolve(t, s, OutcomeCommit,
			base.Add(time.Duration(index+1)*time.Millisecond))
		if !bytes.Equal(got, expected) {
			t.Fatalf("report %d = %v, want %v", index, got, expected)
		}
		if !claim.Ordered {
			t.Fatalf("report %d must be ordered", index)
		}
	}
}

func TestFixedReportSchedulerProducerRetirementNeutralizesWithoutRotatingPresentation(
	t *testing.T,
) {
	s := newStrictTestScheduler(t, time.Second)
	base := time.Unix(2, 0)
	presentationGeneration := s.Generation()
	oldLease := s.ProducerLease()
	if disposition := s.PublishWithLease(oldLease,
		(schedulerTestState{buttons: 1, axis: 9}), base); !disposition.Accepted() {
		t.Fatalf("initial publish disposition = %d", disposition)
	}
	var report [2]byte
	oldClaim := s.ClaimInputPresentation(report[:], base)
	if !oldClaim.Valid() {
		t.Fatal("expected an old-producer claim")
	}

	newLease, retired := s.RetireProducerLease(oldLease,
		base.Add(time.Millisecond))
	if !retired || !newLease.Valid() || newLease == oldLease {
		t.Fatalf("producer retirement = (%+v, %v)", newLease, retired)
	}
	if s.Generation() != presentationGeneration {
		t.Fatalf("producer retirement rotated presentation %d -> %d",
			presentationGeneration, s.Generation())
	}
	if s.ResolveInputPresentation(oldClaim, OutcomeCommit,
		base.Add(2*time.Millisecond)) {
		t.Fatal("old producer claim survived retirement")
	}
	if disposition := s.PublishWithLease(oldLease,
		(schedulerTestState{buttons: 2}), base.Add(2*time.Millisecond)); disposition != FixedReportPublishRejectedStaleProducer {
		t.Fatalf("old lease publish disposition = %d", disposition)
	}
	if disposition := s.PublishWithLease(newLease,
		(schedulerTestState{buttons: 2, axis: 7}), base.Add(2*time.Millisecond)); disposition != FixedReportPublishRejectedNeutralPending {
		t.Fatalf("successor before neutral disposition = %d", disposition)
	}

	neutralClaim := s.ClaimInputPresentation(report[:],
		base.Add(2*time.Millisecond))
	if !neutralClaim.Valid() || report != [2]byte{} {
		t.Fatalf("mandatory neutral = %#v, %+v", report, neutralClaim)
	}
	admitAndResolve(t, s, neutralClaim, OutcomeCommit,
		base.Add(2*time.Millisecond), base.Add(2*time.Millisecond))
	if disposition := s.Resynchronize(newLease,
		(schedulerTestState{buttons: 2, axis: 7}), base.Add(3*time.Millisecond)); disposition != FixedReportPublishAcceptedResynchronization {
		t.Fatalf("successor resynchronization disposition = %d", disposition)
	}
	freshClaim := s.ClaimInputPresentation(report[:],
		base.Add(3*time.Millisecond))
	if !freshClaim.Valid() || report != [2]byte{2, 7} {
		t.Fatalf("fresh successor state = %#v, %+v", report, freshClaim)
	}
}

func TestFixedReportSchedulerMandatoryNeutralEncodesAtFirstClaimAndRetriesBytes(
	t *testing.T,
) {
	base := time.Unix(2, 0)
	mode := byte(1)
	sequence := byte(0)
	s, err := NewFixedReportSchedulerWithOverflowFault(
		2, schedulerTestState{},
		func(_ *schedulerTestState, destination []byte) int {
			sequence++
			destination[0] = mode
			destination[1] = sequence
			return 2
		}, nil, base)
	if err != nil {
		t.Fatal(err)
	}
	if sequence != 1 {
		t.Fatalf("constructor encode count = %d, want 1", sequence)
	}

	oldLease := s.ProducerLease()
	newLease, retired := s.RetireProducerLease(oldLease,
		base.Add(time.Millisecond))
	if !retired {
		t.Fatal("producer retirement failed")
	}
	mode = 5
	var first [2]byte
	firstClaim := s.ClaimInputPresentation(first[:],
		base.Add(2*time.Millisecond))
	if !firstClaim.Valid() || first != [2]byte{5, 2} || sequence != 2 {
		t.Fatalf("first neutral = %v claim=%+v encodes=%d",
			first, firstClaim, sequence)
	}
	if !s.CanAdmitInputPresentation(firstClaim,
		base.Add(2*time.Millisecond)) ||
		!s.ResolveInputPresentation(firstClaim, OutcomeDefer,
			base.Add(2*time.Millisecond)) {
		t.Fatal("neutral defer failed")
	}

	// Once selected, the neutral is one immutable report. A report-mode change
	// cannot alter its retry bytes or advance the encoder sequence again.
	mode = 9
	var retry [2]byte
	retryClaim := s.ClaimInputPresentation(retry[:],
		base.Add(3*time.Millisecond))
	if !retryClaim.Valid() || retry != first || sequence != 2 {
		t.Fatalf("neutral retry = %v claim=%+v encodes=%d; want %v",
			retry, retryClaim, sequence, first)
	}
	if !s.CanAdmitInputPresentation(retryClaim,
		base.Add(3*time.Millisecond)) ||
		!s.ResolveInputPresentation(retryClaim, OutcomeCommit,
			base.Add(3*time.Millisecond)) {
		t.Fatal("neutral retry commit failed")
	}
	if disposition := s.Resynchronize(newLease,
		schedulerTestState{buttons: 1}, base.Add(4*time.Millisecond)); disposition != FixedReportPublishAcceptedResynchronization {
		t.Fatalf("resynchronization disposition = %d", disposition)
	}
	var fresh [2]byte
	freshClaim := s.ClaimInputPresentation(fresh[:],
		base.Add(4*time.Millisecond))
	if !freshClaim.Valid() || fresh != [2]byte{9, 3} || sequence != 3 {
		t.Fatalf("fresh report = %v claim=%+v encodes=%d",
			fresh, freshClaim, sequence)
	}
}

func TestFixedReportSchedulerCoalescesContinuousState(t *testing.T) {
	s := newTestScheduler(t)
	base := time.Unix(3, 0)
	for axis := uint8(1); axis <= 4; axis++ {
		if !s.Publish(schedulerTestState{axis: axis}, base.Add(time.Duration(axis))) {
			t.Fatal("publish failed")
		}
	}
	report, claim := claimAndResolve(t, s, OutcomeCommit, base.Add(time.Millisecond))
	if !bytes.Equal(report, []byte{0, 4}) || claim.Ordered {
		t.Fatalf("claim = %v ordered=%v", report, claim.Ordered)
	}
	if got := s.Snapshot().ContinuousReplaced; got != 3 {
		t.Fatalf("replaced = %d, want 3", got)
	}
}

func TestFixedReportSchedulerRetriesOrderedBytesImmutably(t *testing.T) {
	s := newTestScheduler(t)
	base := time.Unix(4, 0)
	if !s.Publish(schedulerTestState{buttons: 1, axis: 10}, base) {
		t.Fatal("publish failed")
	}
	first, firstClaim := claimAndResolve(t, s, OutcomeDefer, base.Add(time.Millisecond))
	if !s.Publish(schedulerTestState{buttons: 1, axis: 99}, base.Add(2*time.Millisecond)) {
		t.Fatal("publish failed")
	}
	retry, retryClaim := claimAndResolve(t, s, OutcomeCommit, base.Add(3*time.Millisecond))
	if !bytes.Equal(first, retry) {
		t.Fatalf("retry changed: first=%v retry=%v", first, retry)
	}
	if retryClaim.Token == firstClaim.Token {
		t.Fatal("retry must have a fresh ownership token")
	}
}

func TestFixedReportSchedulerRecoversInflightContinuousPredecessorBeforeEdge(
	t *testing.T,
) {
	s := newTestScheduler(t)
	base := time.Unix(4, 0)
	if !s.Publish(schedulerTestState{buttons: 1}, base) {
		t.Fatal("held baseline publish failed")
	}
	claimAndResolve(t, s, OutcomeCommit, base.Add(time.Millisecond))
	if !s.Publish(schedulerTestState{buttons: 1, axis: 255},
		base.Add(2*time.Millisecond)) {
		t.Fatal("continuous peak publish failed")
	}

	var peakReport [2]byte
	peak := s.ClaimInputPresentation(
		peakReport[:], base.Add(3*time.Millisecond))
	if !peak.Valid() || peak.Ordered ||
		!bytes.Equal(peakReport[:], []byte{1, 255}) {
		t.Fatalf("peak claim=%+v report=%v", peak, peakReport)
	}
	if !s.Publish(schedulerTestState{}, base.Add(4*time.Millisecond)) {
		t.Fatal("release publish failed")
	}
	if !s.ResolveInputPresentation(
		peak, OutcomeDefer, base.Add(5*time.Millisecond)) {
		t.Fatal("peak deferral failed")
	}

	firstRetry, firstRetryClaim := claimAndResolve(
		t, s, OutcomeDefer, base.Add(6*time.Millisecond))
	if !bytes.Equal(firstRetry, peakReport[:]) || !firstRetryClaim.Ordered {
		t.Fatalf("first retry=%v claim=%+v", firstRetry, firstRetryClaim)
	}
	secondRetry, secondRetryClaim := claimAndResolve(
		t, s, OutcomeCommit, base.Add(7*time.Millisecond))
	if !bytes.Equal(secondRetry, peakReport[:]) || !secondRetryClaim.Ordered {
		t.Fatalf("second retry=%v claim=%+v", secondRetry, secondRetryClaim)
	}
	release, releaseClaim := claimAndResolve(
		t, s, OutcomeCommit, base.Add(8*time.Millisecond))
	if !bytes.Equal(release, []byte{0, 0}) || !releaseClaim.Ordered {
		t.Fatalf("release=%v claim=%+v", release, releaseClaim)
	}
}

func TestFixedReportSchedulerDoesNotDuplicateInflightPredecessorAfterCommit(
	t *testing.T,
) {
	s := newTestScheduler(t)
	base := time.Unix(4, 0)
	if !s.Publish(schedulerTestState{buttons: 1}, base) {
		t.Fatal("held baseline publish failed")
	}
	claimAndResolve(t, s, OutcomeCommit, base.Add(time.Millisecond))
	if !s.Publish(schedulerTestState{buttons: 1, axis: 255},
		base.Add(2*time.Millisecond)) {
		t.Fatal("continuous peak publish failed")
	}
	var peakReport [2]byte
	peak := s.ClaimInputPresentation(
		peakReport[:], base.Add(3*time.Millisecond))
	if !peak.Valid() || peak.Ordered {
		t.Fatalf("peak claim=%+v", peak)
	}
	if !s.Publish(schedulerTestState{}, base.Add(4*time.Millisecond)) {
		t.Fatal("release publish failed")
	}
	if !s.ResolveInputPresentation(
		peak, OutcomeCommit, base.Add(5*time.Millisecond)) {
		t.Fatal("peak commit failed")
	}

	release, releaseClaim := claimAndResolve(
		t, s, OutcomeCommit, base.Add(6*time.Millisecond))
	if !bytes.Equal(release, []byte{0, 0}) || !releaseClaim.Ordered {
		t.Fatalf("release=%v claim=%+v", release, releaseClaim)
	}
	idle, idleClaim := claimAndResolve(
		t, s, OutcomeCommit, base.Add(7*time.Millisecond))
	if !bytes.Equal(idle, release) || idleClaim.Ordered {
		t.Fatalf("predecessor was duplicated: idle=%v claim=%+v",
			idle, idleClaim)
	}
}

func TestFixedReportSchedulerRetireRejectsStaleCompletion(t *testing.T) {
	s := newTestScheduler(t)
	base := time.Unix(5, 0)
	if !s.Publish(schedulerTestState{buttons: 1}, base) {
		t.Fatal("publish failed")
	}
	var report [2]byte
	claim := s.ClaimInputPresentation(report[:], base.Add(time.Millisecond))
	if !claim.Valid() {
		t.Fatal("expected claim")
	}
	if !s.RetireInputPresentationGeneration(claim.Generation, base.Add(2*time.Millisecond)) {
		t.Fatal("retire failed")
	}
	if s.ResolveInputPresentation(claim, OutcomeCommit, base.Add(3*time.Millisecond)) {
		t.Fatal("stale completion was accepted")
	}
	if got := s.Generation(); got == claim.Generation {
		t.Fatal("generation did not advance")
	}
}

func TestFixedReportSchedulerRetireCollapsesHistoryToCurrentState(t *testing.T) {
	s := newTestScheduler(t)
	base := time.Unix(5, 0)
	if !s.Publish(schedulerTestState{buttons: 1, axis: 10}, base) ||
		!s.Publish(schedulerTestState{buttons: 1, axis: 200},
			base.Add(time.Microsecond)) ||
		!s.Publish(schedulerTestState{axis: 30},
			base.Add(2*time.Microsecond)) {
		t.Fatal("publish failed")
	}
	retiredGeneration := s.Generation()
	if !s.RetireInputPresentationGeneration(
		retiredGeneration, base.Add(time.Millisecond)) {
		t.Fatal("retire failed")
	}

	report, claim := claimAndResolve(t, s, OutcomeCommit,
		base.Add(2*time.Millisecond))
	if !bytes.Equal(report, []byte{0, 30}) {
		t.Fatalf("successor report = %v, want current release state", report)
	}
	if claim.Ordered {
		t.Fatal("collapsed successor snapshot must not replay as a transition")
	}
	if snapshot := s.Snapshot(); snapshot.TransitionDepth != 0 {
		t.Fatalf("retired transition history survived: %+v", snapshot)
	}
}

func TestFixedReportSchedulerOverflowPurgesHistoryAndRequiresNeutralResync(
	t *testing.T,
) {
	s := newStrictTestScheduler(t, 2*time.Second)
	base := time.Unix(6, 0)
	faultedLease := s.ProducerLease()
	for index := 0; index < FixedReportTransitionCapacity; index++ {
		disposition := s.PublishWithLease(faultedLease,
			schedulerTestState{buttons: uint8(index%2 + 1)},
			base.Add(time.Duration(index)))
		if disposition != FixedReportPublishAcceptedOrdered {
			t.Fatalf("publish %d failed early", index)
		}
	}
	if disposition := s.PublishWithLease(faultedLease,
		schedulerTestState{}, base.Add(time.Second)); disposition != FixedReportPublishFaultedOverflow {
		t.Fatalf("overflow disposition = %v", disposition)
	}
	snapshot := s.Snapshot()
	if snapshot.TransitionDepth != 1 || snapshot.Overflows != 1 ||
		!snapshot.MandatoryNeutral || snapshot.ContinuousPending ||
		snapshot.LastFault != FixedReportFaultOverflow {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.ProducerEpoch == faultedLease.epoch {
		t.Fatal("overflow did not revoke producer lease")
	}
	if disposition := s.PublishWithLease(faultedLease,
		schedulerTestState{buttons: 2}, base.Add(time.Second)); disposition != FixedReportPublishRejectedStaleProducer {
		t.Fatalf("stale producer disposition = %v", disposition)
	}
	if s.Publish(schedulerTestState{buttons: 2}, base.Add(time.Second)) {
		t.Fatal("legacy publication bypassed the mandatory neutral")
	}

	var neutral [2]byte
	first := s.ClaimInputPresentation(neutral[:], base.Add(time.Second))
	if !first.Valid() || !first.Ordered || neutral != [2]byte{} {
		t.Fatalf("neutral claim=%+v bytes=%v", first, neutral)
	}
	if !s.CanAdmitInputPresentation(first, base.Add(time.Second)) ||
		!s.ResolveInputPresentation(first, OutcomeDefer,
			base.Add(time.Second)) {
		t.Fatal("neutral defer failed")
	}
	var retry [2]byte
	second := s.ClaimInputPresentation(retry[:], base.Add(time.Second))
	if !second.Valid() || second.Token == first.Token ||
		second.ReceivedAt != first.ReceivedAt || retry != neutral {
		t.Fatalf("neutral retry changed: first=%+v second=%+v bytes=%v",
			first, second, retry)
	}
	admitAndResolve(t, s, second, OutcomeCommit,
		base.Add(time.Second), base.Add(time.Second))
	if snapshot = s.Snapshot(); snapshot.MandatoryNeutral ||
		!snapshot.Resynchronization {
		t.Fatalf("post-neutral snapshot = %+v", snapshot)
	}

	recoveryLease := s.ProducerLease()
	if disposition := s.PublishWithLease(recoveryLease,
		schedulerTestState{buttons: 2}, base.Add(time.Second)); disposition != FixedReportPublishRejectedResynchronizationRequired {
		t.Fatalf("publication bypassed resync: %v", disposition)
	}
	if disposition := s.Resynchronize(recoveryLease,
		(schedulerTestState{buttons: 2, axis: 99}), base.Add(time.Second)); disposition != FixedReportPublishAcceptedResynchronization {
		t.Fatalf("resync disposition = %v", disposition)
	}
	var fresh [2]byte
	claim := s.ClaimInputPresentation(fresh[:], base.Add(time.Second))
	admitAndResolve(t, s, claim, OutcomeCommit,
		base.Add(time.Second), base.Add(time.Second))
	if fresh != [2]byte{2, 99} || claim.Ordered {
		t.Fatalf("fresh report=%v claim=%+v", fresh, claim)
	}
}

func TestFixedReportSchedulerCompatibilityOverflowRemainsRejectOnly(
	t *testing.T,
) {
	s := newTestScheduler(t)
	base := time.Unix(8, 0)
	lease := s.ProducerLease()
	for index := 0; index < FixedReportTransitionCapacity; index++ {
		if disposition := s.PublishWithLease(lease,
			(schedulerTestState{buttons: uint8(index%2 + 1)}),
			base.Add(time.Duration(index))); disposition != FixedReportPublishAcceptedOrdered {
			t.Fatalf("fill %d disposition = %v", index, disposition)
		}
	}
	if disposition := s.PublishWithLease(lease, schedulerTestState{},
		base.Add(time.Second)); disposition != FixedReportPublishRejectedOverflow {
		t.Fatalf("overflow disposition = %v", disposition)
	}
	snapshot := s.Snapshot()
	if snapshot.TransitionDepth != FixedReportTransitionCapacity ||
		snapshot.Overflows != 1 || snapshot.MandatoryNeutral ||
		snapshot.Resynchronization || snapshot.ProducerEpoch != lease.epoch {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	var oldest [2]byte
	claim := s.ClaimInputPresentation(oldest[:], base.Add(time.Second))
	if oldest != [2]byte{1, 0} || !claim.Ordered ||
		!s.ResolveInputPresentation(claim, OutcomeCommit, base.Add(time.Second)) {
		t.Fatalf("oldest claim=%+v report=%v", claim, oldest)
	}
	if !s.Publish(schedulerTestState{}, base.Add(time.Second)) {
		t.Fatal("compatibility producer remained frozen after capacity returned")
	}
}

func TestFixedReportSchedulerNoAgeProductionOverflowFailsClosed(
	t *testing.T,
) {
	s := newOverflowFaultTestScheduler(t)
	base := time.Unix(9, 0)
	lease := s.ProducerLease()
	for index := 0; index < FixedReportTransitionCapacity; index++ {
		disposition := s.PublishWithLease(lease,
			schedulerTestState{buttons: uint8(index%2 + 1)},
			base.Add(time.Duration(index)))
		if disposition != FixedReportPublishAcceptedOrdered {
			t.Fatalf("fill %d disposition = %v", index, disposition)
		}
	}
	if disposition := s.PublishWithLease(lease, schedulerTestState{},
		base.Add(time.Second)); disposition != FixedReportPublishFaultedOverflow {
		t.Fatalf("overflow disposition = %v", disposition)
	}
	snapshot := s.Snapshot()
	if snapshot.MaximumOrderedAge != 0 || !snapshot.MandatoryNeutral ||
		snapshot.TransitionDepth != 1 || snapshot.ProducerEpoch == lease.epoch ||
		snapshot.LastFault != FixedReportFaultOverflow {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestFixedReportSchedulerStrictConstructorRequiresPositiveAge(t *testing.T) {
	encode := func(state *schedulerTestState, destination []byte) int {
		destination[0] = state.buttons
		destination[1] = state.axis
		return 2
	}
	transition := func(previous, next schedulerTestState) bool {
		return previous.buttons != next.buttons
	}
	for _, maximumAge := range []time.Duration{0, -time.Nanosecond} {
		if _, err := NewFixedReportSchedulerWithMaximumOrderedAge(
			2, schedulerTestState{}, encode, transition, maximumAge,
			time.Unix(1, 0)); err == nil {
			t.Fatalf("maximum age %v unexpectedly accepted", maximumAge)
		}
	}
	s := newStrictTestScheduler(t, 7*time.Millisecond)
	if snapshot := s.Snapshot(); snapshot.MaximumOrderedAge != 7*time.Millisecond {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestFixedReportSchedulerOverflowRevokesUnadmittedActiveClaim(t *testing.T) {
	s := newStrictTestScheduler(t, time.Second)
	base := time.Unix(9, 0)
	lease := s.ProducerLease()
	if !s.PublishWithLease(lease,
		(schedulerTestState{buttons: 1}), base).Accepted() {
		t.Fatal("initial publish failed")
	}
	var oldBytes [2]byte
	active := s.ClaimInputPresentation(oldBytes[:], base.Add(time.Millisecond))
	if !active.Valid() || oldBytes != [2]byte{1, 0} {
		t.Fatalf("active=%+v bytes=%v", active, oldBytes)
	}
	for index := 0; index < FixedReportTransitionCapacity; index++ {
		buttons := uint8(2)
		if index%2 != 0 {
			buttons = 1
		}
		if disposition := s.PublishWithLease(lease,
			(schedulerTestState{buttons: buttons}),
			base.Add(time.Duration(index+2)*time.Millisecond)); disposition != FixedReportPublishAcceptedOrdered {
			t.Fatalf("fill %d disposition = %v", index, disposition)
		}
	}
	if disposition := s.PublishWithLease(lease,
		(schedulerTestState{buttons: 2}), base.Add(100*time.Millisecond)); disposition != FixedReportPublishFaultedOverflow {
		t.Fatalf("overflow disposition = %v", disposition)
	}
	if s.CanAdmitInputPresentation(active, base.Add(101*time.Millisecond)) ||
		s.ResolveInputPresentation(active, OutcomeCommit,
			base.Add(101*time.Millisecond)) {
		t.Fatal("pre-fault claim survived overflow")
	}
	var neutralBytes [2]byte
	neutral := s.ClaimInputPresentation(
		neutralBytes[:], base.Add(101*time.Millisecond))
	if !neutral.Valid() || !neutral.Ordered || neutralBytes != [2]byte{} {
		t.Fatalf("neutral=%+v bytes=%v", neutral, neutralBytes)
	}
}

func TestFixedReportSchedulerStaleJournalFaultsWholeHistoryToNeutral(
	t *testing.T,
) {
	s := newStrictTestScheduler(t, 5*time.Millisecond)
	base := time.Unix(10, 0)
	faultedLease := s.ProducerLease()
	if s.PublishWithLease(faultedLease,
		(schedulerTestState{buttons: 1}), base) !=
		FixedReportPublishAcceptedOrdered ||
		s.PublishWithLease(faultedLease, schedulerTestState{},
			base.Add(time.Millisecond)) != FixedReportPublishAcceptedOrdered {
		t.Fatal("ordered history publication failed")
	}

	var report [2]byte
	neutral := s.ClaimInputPresentation(report[:], base.Add(6*time.Millisecond))
	if !neutral.Valid() || !neutral.Ordered || report != [2]byte{} {
		t.Fatalf("stale history was replayed: claim=%+v report=%v", neutral, report)
	}
	snapshot := s.Snapshot()
	if snapshot.StaleFaults != 1 || snapshot.TransitionDepth != 1 ||
		!snapshot.MandatoryNeutral || snapshot.LastFault != FixedReportFaultOrderedAge {
		t.Fatalf("fault snapshot = %+v", snapshot)
	}
	if disposition := s.PublishWithLease(faultedLease,
		schedulerTestState{buttons: 2}, base.Add(7*time.Millisecond)); disposition != FixedReportPublishRejectedStaleProducer {
		t.Fatalf("old lease disposition = %v", disposition)
	}
	admitAndResolve(t, s, neutral, OutcomeCommit,
		base.Add(7*time.Millisecond), base.Add(7*time.Millisecond))

	recoveryLease := s.ProducerLease()
	if disposition := s.Resynchronize(recoveryLease,
		(schedulerTestState{buttons: 2, axis: 8}),
		base.Add(8*time.Millisecond)); disposition != FixedReportPublishAcceptedResynchronization {
		t.Fatalf("resync disposition = %v", disposition)
	}
	fresh := s.ClaimInputPresentation(report[:], base.Add(9*time.Millisecond))
	if report != [2]byte{2, 8} || fresh.Ordered {
		t.Fatalf("fresh claim=%+v report=%v", fresh, report)
	}
	admitAndResolve(t, s, fresh, OutcomeCommit,
		base.Add(9*time.Millisecond), base.Add(9*time.Millisecond))
	if next := s.ClaimInputPresentation(report[:], base.Add(10*time.Millisecond)); next.Ordered || report != [2]byte{2, 8} {
		t.Fatalf("purged edge survived resync: claim=%+v report=%v", next, report)
	}
}

func TestFixedReportSchedulerOrderedAgeFaultsAtExactBoundary(t *testing.T) {
	t.Run("queued head", func(t *testing.T) {
		s := newStrictTestScheduler(t, 5*time.Millisecond)
		base := time.Unix(10, 0)
		if !s.PublishWithLease(s.ProducerLease(),
			schedulerTestState{buttons: 1}, base).Accepted() {
			t.Fatal("publish failed")
		}
		var report [2]byte
		claim := s.ClaimInputPresentation(
			report[:], base.Add(5*time.Millisecond))
		if !claim.Valid() || !claim.Ordered || report != [2]byte{} {
			t.Fatalf("exact-boundary history escaped: claim=%+v report=%v",
				claim, report)
		}
		if snapshot := s.Snapshot(); snapshot.StaleFaults != 1 ||
			!snapshot.MandatoryNeutral {
			t.Fatalf("snapshot = %+v", snapshot)
		}
	})

	t.Run("retry", func(t *testing.T) {
		s := newStrictTestScheduler(t, 5*time.Millisecond)
		base := time.Unix(10, 0)
		if !s.PublishWithLease(s.ProducerLease(),
			schedulerTestState{buttons: 1}, base).Accepted() {
			t.Fatal("publish failed")
		}
		var report [2]byte
		first := s.ClaimInputPresentation(report[:], base.Add(time.Millisecond))
		admitAndResolve(t, s, first, OutcomeDefer,
			base.Add(2*time.Millisecond), base.Add(2*time.Millisecond))
		claim := s.ClaimInputPresentation(
			report[:], base.Add(5*time.Millisecond))
		if !claim.Valid() || !claim.Ordered || report != [2]byte{} {
			t.Fatalf("exact-boundary retry escaped: claim=%+v report=%v",
				claim, report)
		}
	})

	t.Run("final admission", func(t *testing.T) {
		s := newStrictTestScheduler(t, 5*time.Millisecond)
		base := time.Unix(10, 0)
		if !s.PublishWithLease(s.ProducerLease(),
			schedulerTestState{buttons: 1}, base).Accepted() {
			t.Fatal("publish failed")
		}
		var report [2]byte
		claim := s.ClaimInputPresentation(report[:], base.Add(time.Millisecond))
		if s.CanAdmitInputPresentation(claim,
			base.Add(5*time.Millisecond)) {
			t.Fatal("exact-boundary claim admitted")
		}
		if snapshot := s.Snapshot(); snapshot.StaleFaults != 1 ||
			!snapshot.MandatoryNeutral {
			t.Fatalf("snapshot = %+v", snapshot)
		}
	})
}

func TestFixedReportSchedulerResynchronizationRejectsPreFaultSnapshot(
	t *testing.T,
) {
	s := newStrictTestScheduler(t, 5*time.Millisecond)
	base := time.Unix(11, 0)
	oldLease := s.ProducerLease()
	if !s.PublishWithLease(oldLease,
		schedulerTestState{buttons: 1}, base).Accepted() {
		t.Fatal("publish failed")
	}
	var report [2]byte
	neutral := s.ClaimInputPresentation(
		report[:], base.Add(5*time.Millisecond))
	admitAndResolve(t, s, neutral, OutcomeCommit,
		base.Add(5*time.Millisecond), base.Add(5*time.Millisecond))

	lease := s.ProducerLease()
	if disposition := s.Resynchronize(lease,
		(schedulerTestState{buttons: 2, axis: 7}),
		base.Add(4*time.Millisecond)); disposition != FixedReportPublishRejectedInvalidTimestamp {
		t.Fatalf("pre-fault resync disposition = %v", disposition)
	}
	if snapshot := s.Snapshot(); !snapshot.Resynchronization ||
		snapshot.ContinuousPending {
		t.Fatalf("pre-fault snapshot changed state: %+v", snapshot)
	}
	if disposition := s.Resynchronize(lease,
		(schedulerTestState{buttons: 2, axis: 8}),
		base.Add(5*time.Millisecond)); disposition != FixedReportPublishAcceptedResynchronization {
		t.Fatalf("exact-boundary resync disposition = %v", disposition)
	}
	claim := s.ClaimInputPresentation(
		report[:], base.Add(6*time.Millisecond))
	if claim.Ordered || report != [2]byte{2, 8} {
		t.Fatalf("fresh resync claim=%+v report=%v", claim, report)
	}
}

func TestFixedReportSchedulerFutureDatedOrderedEntryFaultsWholeHistory(
	t *testing.T,
) {
	s := newStrictTestScheduler(t, time.Second)
	base := time.Unix(10, 0)
	lease := s.ProducerLease()
	if !s.PublishWithLease(lease, schedulerTestState{buttons: 1},
		base.Add(time.Second)).Accepted() {
		t.Fatal("publish failed")
	}
	var report [2]byte
	claim := s.ClaimInputPresentation(report[:], base)
	if !claim.Valid() || !claim.Ordered || report != [2]byte{} {
		t.Fatalf("future-dated edge did not become neutral: claim=%+v report=%v",
			claim, report)
	}
	if snapshot := s.Snapshot(); snapshot.InvalidTimestamps != 1 ||
		snapshot.StaleFaults != 1 || !snapshot.MandatoryNeutral ||
		snapshot.TransitionDepth != 1 {
		t.Fatalf("future-entry fault snapshot = %+v", snapshot)
	}
	admitAndResolve(t, s, claim, OutcomeCommit, base, base)
	if snapshot := s.Snapshot(); !snapshot.Resynchronization ||
		snapshot.TransitionDepth != 0 {
		t.Fatalf("future edge survived neutral: %+v", snapshot)
	}
	if disposition := s.Resynchronize(s.ProducerLease(),
		schedulerTestState{axis: 2}, base.Add(time.Millisecond)); disposition != FixedReportPublishAcceptedResynchronization {
		t.Fatalf("future old epoch poisoned immediate resync: %v", disposition)
	}
}

func TestFixedReportSchedulerStrictProducerTimestampsAreNondecreasing(
	t *testing.T,
) {
	t.Run("reverse publication does not mutate history", func(t *testing.T) {
		s := newStrictTestScheduler(t, 2*time.Second)
		base := time.Unix(11, 0)
		lease := s.ProducerLease()
		if disposition := s.PublishWithLease(lease,
			schedulerTestState{buttons: 1}, base.Add(time.Second)); disposition != FixedReportPublishAcceptedOrdered {
			t.Fatalf("press disposition = %v", disposition)
		}
		if disposition := s.PublishWithLease(lease, schedulerTestState{},
			base); disposition != FixedReportPublishRejectedInvalidTimestamp {
			t.Fatalf("reverse release disposition = %v", disposition)
		}
		// Equal timestamps are valid: a coarse monotonic clock can stamp two
		// distinct reports at the same instant.
		if disposition := s.PublishWithLease(lease,
			schedulerTestState{buttons: 1, axis: 9}, base.Add(time.Second)); disposition != FixedReportPublishAcceptedContinuous {
			t.Fatalf("equal timestamp disposition = %v", disposition)
		}
		var report [2]byte
		press := s.ClaimInputPresentation(
			report[:], base.Add(time.Second))
		if report != [2]byte{1, 0} || !press.Ordered {
			t.Fatalf("history changed: claim=%+v report=%v", press, report)
		}
		admitAndResolve(t, s, press, OutcomeCommit,
			base.Add(time.Second), base.Add(time.Second))
		latest := s.ClaimInputPresentation(
			report[:], base.Add(time.Second))
		if report != [2]byte{1, 9} || latest.Ordered {
			t.Fatalf("equal-time latest missing: claim=%+v report=%v",
				latest, report)
		}
	})

	t.Run("reverse publication during active claim is rejected", func(t *testing.T) {
		s := newStrictTestScheduler(t, 2*time.Second)
		base := time.Unix(12, 0)
		lease := s.ProducerLease()
		if !s.PublishWithLease(lease,
			schedulerTestState{axis: 1}, base.Add(time.Second)).Accepted() {
			t.Fatal("publish failed")
		}
		var report [2]byte
		claim := s.ClaimInputPresentation(
			report[:], base.Add(2*time.Second))
		if disposition := s.PublishWithLease(lease,
			schedulerTestState{axis: 2}, base); disposition != FixedReportPublishRejectedInvalidTimestamp {
			t.Fatalf("reverse active disposition = %v", disposition)
		}
		admitAndResolve(t, s, claim, OutcomeCommit,
			base.Add(2*time.Second), base.Add(2*time.Second))
		if report != [2]byte{0, 1} {
			t.Fatalf("active claim changed: %v", report)
		}
	})

	t.Run("post-resync producer fence remains active", func(t *testing.T) {
		s := newStrictTestScheduler(t, 5*time.Millisecond)
		base := time.Unix(13, 0)
		if !s.PublishWithLease(s.ProducerLease(),
			schedulerTestState{buttons: 1}, base).Accepted() {
			t.Fatal("publish failed")
		}
		var report [2]byte
		neutral := s.ClaimInputPresentation(
			report[:], base.Add(5*time.Millisecond))
		admitAndResolve(t, s, neutral, OutcomeCommit,
			base.Add(5*time.Millisecond), base.Add(5*time.Millisecond))
		lease := s.ProducerLease()
		if disposition := s.Resynchronize(lease,
			schedulerTestState{axis: 3}, base.Add(5*time.Millisecond)); disposition != FixedReportPublishAcceptedResynchronization {
			t.Fatalf("resync disposition = %v", disposition)
		}
		if disposition := s.PublishWithLease(lease,
			schedulerTestState{axis: 4}, base.Add(4*time.Millisecond)); disposition != FixedReportPublishRejectedInvalidTimestamp {
			t.Fatalf("pre-resync publication disposition = %v", disposition)
		}
	})
}

func TestFixedReportSchedulerStrictBoundariesAreClaimLocal(
	t *testing.T,
) {
	t.Run("future continuous state", func(t *testing.T) {
		s := newStrictTestScheduler(t, time.Second)
		base := time.Unix(14, 0)
		if !s.PublishWithLease(s.ProducerLease(),
			schedulerTestState{axis: 7}, base.Add(time.Second)).Accepted() {
			t.Fatal("publish failed")
		}
		var report [2]byte
		claim := s.ClaimInputPresentation(report[:], base)
		if !claim.Valid() || !claim.Ordered || report != [2]byte{} {
			t.Fatalf("future continuous did not become neutral: claim=%+v report=%v",
				claim, report)
		}
		if snapshot := s.Snapshot(); snapshot.InvalidTimestamps != 1 ||
			!snapshot.MandatoryNeutral || snapshot.ContinuousPending ||
			snapshot.StaleFaults != 1 {
			t.Fatalf("snapshot = %+v", snapshot)
		}
		admitAndResolve(t, s, claim, OutcomeCommit, base, base)
		if disposition := s.Resynchronize(s.ProducerLease(),
			schedulerTestState{axis: 8}, base.Add(time.Millisecond)); disposition != FixedReportPublishAcceptedResynchronization {
			t.Fatalf("future continuous epoch poisoned resync: %v", disposition)
		}
	})

	t.Run("selection admission and completion", func(t *testing.T) {
		s := newStrictTestScheduler(t, time.Second)
		base := time.Unix(15, 0)
		if !s.PublishWithLease(s.ProducerLease(),
			schedulerTestState{buttons: 1}, base).Accepted() {
			t.Fatal("publish failed")
		}
		var report [2]byte
		claim := s.ClaimInputPresentation(report[:], base)
		if !claim.Valid() {
			t.Fatal("claim failed")
		}
		if s.CanAdmitInputPresentation(claim, base.Add(-time.Nanosecond)) {
			t.Fatal("backward admission succeeded")
		}
		if !s.CanAdmitInputPresentation(claim, base) {
			t.Fatal("equal admission failed")
		}
		if !s.ResolveInputPresentation(claim, OutcomeCommit,
			base.Add(-time.Nanosecond)) {
			t.Fatal("matching completion wedged on a regressed timestamp")
		}
		if s.ResolveInputPresentation(claim, OutcomeCommit, base) {
			t.Fatal("duplicate completion succeeded")
		}
		if snapshot := s.Snapshot(); snapshot.InvalidTimestamps != 2 ||
			snapshot.MandatoryNeutral {
			t.Fatalf("snapshot = %+v", snapshot)
		}
	})

	t.Run("newer producer cannot wedge terminal completion", func(t *testing.T) {
		s := newStrictTestScheduler(t, time.Second)
		base := time.Unix(16, 0)
		lease := s.ProducerLease()
		if !s.PublishWithLease(lease,
			schedulerTestState{buttons: 1}, base).Accepted() {
			t.Fatal("press failed")
		}
		var report [2]byte
		claim := s.ClaimInputPresentation(report[:], base.Add(time.Millisecond))
		if !s.CanAdmitInputPresentation(claim, base.Add(2*time.Millisecond)) {
			t.Fatal("admission failed")
		}
		if !s.PublishWithLease(lease,
			(schedulerTestState{buttons: 1, axis: 9}),
			base.Add(4*time.Millisecond)).Accepted() {
			t.Fatal("concurrent newer publication failed")
		}
		if !s.ResolveInputPresentation(claim, OutcomeCommit,
			base.Add(3*time.Millisecond)) {
			t.Fatal("newer producer wedged matching completion")
		}
		next := s.ClaimInputPresentation(report[:], base.Add(5*time.Millisecond))
		if !next.Valid() || next.Ordered || report != [2]byte{1, 9} {
			t.Fatalf("successor state missing: claim=%+v report=%v", next, report)
		}
	})

	t.Run("newer producer cannot wedge generation retirement", func(t *testing.T) {
		s := newStrictTestScheduler(t, time.Second)
		base := time.Unix(17, 0)
		lease := s.ProducerLease()
		if !s.PublishWithLease(lease,
			schedulerTestState{axis: 7}, base.Add(4*time.Millisecond)).Accepted() {
			t.Fatal("publication failed")
		}
		generation := s.Generation()
		if !s.RetireInputPresentationGeneration(
			generation, base.Add(3*time.Millisecond)) {
			t.Fatal("newer producer wedged matching retirement")
		}
		if s.Generation() == generation {
			t.Fatal("generation did not advance")
		}
		if snapshot := s.Snapshot(); snapshot.InvalidTimestamps != 1 {
			t.Fatalf("retirement diagnostic missing: %+v", snapshot)
		}
		if disposition := s.PublishWithLease(s.ProducerLease(),
			schedulerTestState{axis: 8}, base.Add(3500*time.Microsecond)); disposition != FixedReportPublishAcceptedContinuous {
			t.Fatalf("old epoch poisoned successor producer: %v", disposition)
		}
		var report [2]byte
		claim := s.ClaimInputPresentation(report[:], base.Add(5*time.Millisecond))
		if !claim.Valid() || report != [2]byte{0, 8} {
			t.Fatalf("successor claim wedged: claim=%+v report=%v", claim, report)
		}
	})
}

func TestFixedReportSchedulerStaleRetryCannotReplay(t *testing.T) {
	s := newStrictTestScheduler(t, 5*time.Millisecond)
	base := time.Unix(11, 0)
	lease := s.ProducerLease()
	if !s.PublishWithLease(lease,
		(schedulerTestState{buttons: 1, axis: 7}), base).Accepted() {
		t.Fatal("publish failed")
	}
	var firstReport [2]byte
	first := s.ClaimInputPresentation(firstReport[:], base.Add(time.Millisecond))
	admitAndResolve(t, s, first, OutcomeDefer,
		base.Add(2*time.Millisecond), base.Add(2*time.Millisecond))

	var nextReport [2]byte
	next := s.ClaimInputPresentation(nextReport[:], base.Add(6*time.Millisecond))
	if nextReport != [2]byte{} || !next.Ordered ||
		next.ReceivedAt != base.Add(6*time.Millisecond) {
		t.Fatalf("stale retry escaped: first=%v next=%v claim=%+v",
			firstReport, nextReport, next)
	}
	if snapshot := s.Snapshot(); snapshot.StaleFaults != 1 ||
		!snapshot.MandatoryNeutral {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestFixedReportSchedulerCanAdmitRevalidatesInternalAgeAndIdentity(
	t *testing.T,
) {
	s := newStrictTestScheduler(t, 5*time.Millisecond)
	base := time.Unix(12, 0)
	lease := s.ProducerLease()
	if !s.PublishWithLease(lease,
		(schedulerTestState{buttons: 1}), base).Accepted() {
		t.Fatal("publish failed")
	}
	var report [2]byte
	claim := s.ClaimInputPresentation(report[:], base.Add(time.Millisecond))
	wrongToken := claim
	wrongToken.Token++
	if s.CanAdmitInputPresentation(wrongToken, base.Add(2*time.Millisecond)) {
		t.Fatal("wrong token admitted")
	}
	wrongGeneration := claim
	wrongGeneration.Generation++
	if s.CanAdmitInputPresentation(wrongGeneration,
		base.Add(2*time.Millisecond)) {
		t.Fatal("wrong generation admitted")
	}
	wrongSize := claim
	wrongSize.Size++
	if s.CanAdmitInputPresentation(wrongSize, base.Add(2*time.Millisecond)) {
		t.Fatal("wrong size admitted")
	}

	forgedTimestamp := claim
	forgedTimestamp.ReceivedAt = base.Add(time.Hour)
	if s.CanAdmitInputPresentation(forgedTimestamp,
		base.Add(6*time.Millisecond)) {
		t.Fatal("forged diagnostic timestamp bypassed internal age")
	}
	if s.ResolveInputPresentation(claim, OutcomeCommit,
		base.Add(6*time.Millisecond)) {
		t.Fatal("age-faulted claim remained resolvable")
	}
	if snapshot := s.Snapshot(); snapshot.StaleFaults != 1 ||
		!snapshot.MandatoryNeutral {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestFixedReportSchedulerStrictCommitRequiresAdmissionValidation(
	t *testing.T,
) {
	s := newStrictTestScheduler(t, time.Second)
	base := time.Unix(13, 0)
	lease := s.ProducerLease()
	if !s.PublishWithLease(lease,
		(schedulerTestState{buttons: 1}), base).Accepted() {
		t.Fatal("publish failed")
	}
	var report [2]byte
	claim := s.ClaimInputPresentation(report[:], base.Add(time.Millisecond))
	if s.ResolveInputPresentation(claim, OutcomeCommit,
		base.Add(2*time.Millisecond)) {
		t.Fatal("unvalidated strict commit succeeded")
	}
	if !s.CanAdmitInputPresentation(claim, base.Add(2*time.Millisecond)) {
		t.Fatal("valid admission failed")
	}
	if s.CanAdmitInputPresentation(claim, base.Add(2*time.Millisecond)) {
		t.Fatal("duplicate admission validation succeeded")
	}
	if !s.ResolveInputPresentation(claim, OutcomeCommit,
		base.Add(2*time.Millisecond)) {
		t.Fatal("validated strict commit failed")
	}
}

func TestFixedReportSchedulerCompletionAgeFaultsAfterAdmission(t *testing.T) {
	s := newStrictTestScheduler(t, 5*time.Millisecond)
	base := time.Unix(14, 0)
	lease := s.ProducerLease()
	if !s.PublishWithLease(lease,
		(schedulerTestState{buttons: 1}), base).Accepted() {
		t.Fatal("publish failed")
	}
	var report [2]byte
	claim := s.ClaimInputPresentation(report[:], base.Add(time.Millisecond))
	if !s.CanAdmitInputPresentation(claim, base.Add(4*time.Millisecond)) {
		t.Fatal("pre-deadline admission failed")
	}
	if !s.ResolveInputPresentation(claim, OutcomeCommit,
		base.Add(6*time.Millisecond)) {
		t.Fatal("matching completion was not consumed")
	}
	if snapshot := s.Snapshot(); snapshot.StaleFaults != 1 ||
		!snapshot.MandatoryNeutral || snapshot.ProducerEpoch == lease.epoch {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestFixedReportSchedulerRetirementRevokesPausedWriterAndProducer(
	t *testing.T,
) {
	s := newStrictTestScheduler(t, time.Second)
	base := time.Unix(15, 0)
	oldLease := s.ProducerLease()
	if !s.PublishWithLease(oldLease,
		(schedulerTestState{buttons: 1, axis: 4}), base).Accepted() {
		t.Fatal("publish failed")
	}

	claimed := make(chan Claim, 1)
	resume := make(chan struct{})
	result := make(chan [2]bool, 1)
	go func() {
		var report [2]byte
		claim := s.ClaimInputPresentation(report[:], base.Add(time.Millisecond))
		claimed <- claim
		<-resume
		admitted := s.CanAdmitInputPresentation(
			claim, base.Add(3*time.Millisecond))
		resolved := s.ResolveInputPresentation(
			claim, OutcomeCommit, base.Add(3*time.Millisecond))
		result <- [2]bool{admitted, resolved}
	}()
	claim := <-claimed
	if !claim.Valid() {
		t.Fatal("paused writer did not obtain a claim")
	}
	if !s.RetireInputPresentationGeneration(
		claim.Generation, base.Add(2*time.Millisecond)) {
		t.Fatal("retirement failed")
	}
	close(resume)
	if got := <-result; got != [2]bool{} {
		t.Fatalf("paused writer survived retirement: %v", got)
	}
	if disposition := s.PublishWithLease(oldLease,
		schedulerTestState{buttons: 2}, base.Add(3*time.Millisecond)); disposition != FixedReportPublishRejectedStaleProducer {
		t.Fatalf("old producer survived retirement: %v", disposition)
	}

	var successorReport [2]byte
	successor := s.ClaimInputPresentation(
		successorReport[:], base.Add(3*time.Millisecond))
	if successor.Generation == claim.Generation ||
		successorReport != [2]byte{1, 4} || successor.Ordered {
		t.Fatalf("successor claim=%+v report=%v", successor, successorReport)
	}
	admitAndResolve(t, s, successor, OutcomeCommit,
		base.Add(3*time.Millisecond), base.Add(3*time.Millisecond))
}

func TestFixedReportSchedulerHotPathDoesNotAllocate(t *testing.T) {
	s := newTestScheduler(t)
	var report [2]byte
	now := time.Unix(7, 0)
	allocations := testing.AllocsPerRun(1000, func() {
		s.Publish(schedulerTestState{axis: 1}, now)
		claim := s.ClaimInputPresentation(report[:], now)
		if !claim.Valid() || !s.ResolveInputPresentation(claim, OutcomeCommit, now) {
			panic("claim cycle failed")
		}
	})
	if allocations != 0 {
		t.Fatalf("hot path allocated %.2f times per cycle", allocations)
	}
}

func TestFixedReportSchedulerStrictHotPathDoesNotAllocate(t *testing.T) {
	s := newStrictTestScheduler(t, time.Second)
	lease := s.ProducerLease()
	var report [2]byte
	now := time.Unix(16, 0)
	allocations := testing.AllocsPerRun(1000, func() {
		disposition := s.PublishWithLease(
			lease, schedulerTestState{axis: 1}, now)
		claim := s.ClaimInputPresentation(report[:], now)
		if disposition != FixedReportPublishAcceptedContinuous ||
			!claim.Valid() ||
			!s.CanAdmitInputPresentation(claim, now) ||
			!s.ResolveInputPresentation(claim, OutcomeCommit, now) {
			panic("strict claim cycle failed")
		}
	})
	if allocations != 0 {
		t.Fatalf("strict hot path allocated %.2f times per cycle", allocations)
	}
}

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

func TestFixedReportSchedulerRejectsOverflowWithoutCorruptingJournal(t *testing.T) {
	s := newTestScheduler(t)
	base := time.Unix(6, 0)
	for index := 0; index < FixedReportTransitionCapacity; index++ {
		if !s.Publish(schedulerTestState{buttons: uint8(index%2 + 1)},
			base.Add(time.Duration(index))) {
			t.Fatalf("publish %d failed early", index)
		}
	}
	if s.Publish(schedulerTestState{}, base.Add(time.Second)) {
		t.Fatal("overflow publish unexpectedly succeeded")
	}
	snapshot := s.Snapshot()
	if snapshot.TransitionDepth != FixedReportTransitionCapacity || snapshot.Overflows != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
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

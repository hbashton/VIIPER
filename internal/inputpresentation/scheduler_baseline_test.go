package inputpresentation

import (
	"testing"
	"time"
)

func TestFixedReportLifecycleBaselineFencesClaimAndProducerWithoutEncoding(t *testing.T) {
	encoded := 0
	s, err := NewFixedReportSchedulerWithOverflowFault(2, schedulerTestState{},
		func(state *schedulerTestState, dst []byte) int {
			encoded++
			dst[0], dst[1] = state.buttons, state.axis
			return 2
		}, func(previous, next schedulerTestState) bool { return previous.buttons != next.buttons }, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	lease := s.ProducerLease()
	if !s.PublishWithLease(lease, schedulerTestState{buttons: 1}, time.Unix(2, 0)).Accepted() {
		t.Fatal("publication rejected")
	}
	var wire [2]byte
	old := s.ClaimInputPresentation(wire[:], time.Unix(3, 0))
	if !s.CanAdmitInputPresentation(old, time.Unix(3, 0)) {
		t.Fatal("admission rejected")
	}
	before := encoded
	baseline := schedulerTestState{buttons: 2, axis: 42}
	if !s.RetireInputPresentationGenerationWithBaseline(old.Generation, baseline, time.Unix(4, 0)) {
		t.Fatal("baseline fence failed")
	}
	if encoded != before {
		t.Fatal("lifecycle boundary encoded an unpresented report")
	}
	if s.ResolveInputPresentation(old, OutcomeCommit, time.Unix(5, 0)) ||
		s.PublishWithLease(lease, schedulerTestState{buttons: 8}, time.Unix(5, 0)).Accepted() {
		t.Fatal("retired claim/producer affected successor")
	}
	if s.RetireInputPresentationGenerationWithBaseline(old.Generation, schedulerTestState{}, time.Unix(5, 0)) {
		t.Fatal("stale lifecycle callback reset successor")
	}
	// The first publication after the cut must remain behind its baseline.
	next := schedulerTestState{buttons: 4, axis: 43}
	if !s.PublishWithLease(s.ProducerLease(), next, time.Unix(5, 0)).Accepted() {
		t.Fatal("successor publication rejected")
	}
	for _, want := range []schedulerTestState{baseline, next} {
		claim := s.ClaimInputPresentation(wire[:], time.Unix(6, 0))
		state, ok := s.StateForInputPresentationClaim(claim)
		if !ok || state != want || wire != [2]byte{want.buttons, want.axis} {
			t.Fatalf("successor = %+v / %v, want %+v", state, wire, want)
		}
		if !s.CanAdmitInputPresentation(claim, time.Unix(6, 0)) ||
			!s.ResolveInputPresentation(claim, OutcomeCommit, time.Unix(6, 0)) {
			t.Fatal("successor completion rejected")
		}
	}
}

func TestFixedReportPendingQueryDoesNotMaterializeIdleOrConsumeWork(t *testing.T) {
	s := newTestScheduler(t)
	if s.HasPendingInputPresentation() {
		t.Fatal("unchanged idle image reported queued work")
	}
	if !s.PublishWithLease(s.ProducerLease(), schedulerTestState{buttons: 1}, time.Unix(2, 0)).Accepted() {
		t.Fatal("publication failed")
	}
	for i := 0; i < 10; i++ {
		if !s.HasPendingInputPresentation() {
			t.Fatal("pending query consumed work")
		}
	}
	var wire [2]byte
	claim := s.ClaimInputPresentation(wire[:], time.Unix(3, 0))
	if s.HasPendingInputPresentation() {
		t.Fatal("owned claim incorrectly counted as selectable work")
	}
	if !s.ResolveInputPresentation(claim, OutcomeDefer, time.Unix(3, 0)) || !s.HasPendingInputPresentation() {
		t.Fatal("ordered retry was not pending")
	}
	claim = s.ClaimInputPresentation(wire[:], time.Unix(4, 0))
	if !s.ResolveInputPresentation(claim, OutcomeCommit, time.Unix(4, 0)) || s.HasPendingInputPresentation() {
		t.Fatal("completed input did not become idle")
	}
}

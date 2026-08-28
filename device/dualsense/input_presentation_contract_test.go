package dualsense

import (
	"bytes"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
	presentationfake "github.com/Alia5/VIIPER/internal/inputpresentation/fake"
)

func newInputPresentationTestDevice(t testing.TB) *DualSense {
	t.Helper()
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return dev
}

func TestInputPresentationCommitIsExactlyOnce(t *testing.T) {
	dev := newInputPresentationTestDevice(t)
	receivedAt := dev.input.timestampBase.Add(10 * time.Millisecond)
	state := neutralInputState()
	state.Buttons = ButtonCross
	if !dev.input.updateAt(&state, 0, receivedAt) {
		t.Fatal("ordered press was not accepted")
	}

	transport := presentationfake.New(
		InputReportSize, receivedAt.Add(2*time.Millisecond))
	claim, ok := transport.Claim(dev)
	if !ok || !claim.Valid() {
		t.Fatalf("claim=%+v ok=%v", claim, ok)
	}
	if !claim.Ordered || !claim.ReceivedAt.Equal(receivedAt) ||
		!claim.SelectedAt.Equal(transport.Now()) {
		t.Fatalf("claim did not preserve transition metadata: %+v", claim)
	}

	var before [InputReportSize]byte
	_, versionBefore := dev.SnapshotInputReportInto(before[:])
	transport.Advance(time.Millisecond)
	record, accepted := transport.Resolve(dev, inputpresentation.OutcomeCommit)
	if !accepted || !record.Accepted {
		t.Fatalf("commit was rejected: %+v", record)
	}
	var presented [InputReportSize]byte
	_, versionAfter := dev.SnapshotInputReportInto(presented[:])
	if versionAfter == versionBefore || !bytes.Equal(presented[:], record.Report) {
		t.Fatalf("commit did not publish exact claimed bytes: versions=%d/%d",
			versionBefore, versionAfter)
	}
	if dev.ResolveInputPresentation(claim, inputpresentation.OutcomeCommit,
		transport.Now().Add(time.Millisecond)) {
		t.Fatal("duplicate commit was accepted")
	}
	if dev.ResolveInputPresentation(claim, inputpresentation.OutcomeDefer,
		transport.Now().Add(2*time.Millisecond)) {
		t.Fatal("already committed claim was deferred a second time")
	}
	_, finalVersion := dev.SnapshotInputReportInto(presented[:])
	if finalVersion != versionAfter {
		t.Fatalf("duplicate resolution advanced version %d -> %d",
			versionAfter, finalVersion)
	}
}

func TestInputPresentationRejectsInvalidResolutionWithoutMutatingActiveClaim(
	t *testing.T,
) {
	dev := newInputPresentationTestDevice(t)
	receivedAt := dev.input.timestampBase.Add(15 * time.Millisecond)
	state := neutralInputState()
	state.Buttons = ButtonCross
	dev.input.updateAt(&state, 0, receivedAt)
	selectedAt := receivedAt.Add(time.Millisecond)
	var report [InputReportSize]byte
	claim := dev.ClaimInputPresentation(report[:], selectedAt)
	if !claim.Valid() {
		t.Fatalf("active claim=%+v", claim)
	}
	before := dev.InputSchedulerState()

	tests := []struct {
		name    string
		claim   inputpresentation.Claim
		outcome inputpresentation.Outcome
	}{
		{
			name: "wrong token",
			claim: func() inputpresentation.Claim {
				wrong := claim
				wrong.Token++
				return wrong
			}(),
			outcome: inputpresentation.OutcomeCommit,
		},
		{
			name: "wrong generation",
			claim: func() inputpresentation.Claim {
				wrong := claim
				wrong.Generation++
				return wrong
			}(),
			outcome: inputpresentation.OutcomeCommit,
		},
		{
			name: "zero token",
			claim: func() inputpresentation.Claim {
				wrong := claim
				wrong.Token = 0
				return wrong
			}(),
			outcome: inputpresentation.OutcomeCommit,
		},
		{
			name: "invalid outcome", claim: claim,
			outcome: inputpresentation.Outcome(0xff),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if dev.ResolveInputPresentation(
				test.claim, test.outcome, selectedAt.Add(time.Millisecond)) {
				t.Fatal("invalid resolution was accepted")
			}
			if after := dev.InputSchedulerState(); after != before {
				t.Fatalf("invalid resolution mutated scheduler\nbefore=%+v\nafter=%+v",
					before, after)
			}
			dev.input.mu.Lock()
			active := dev.input.hasClaim &&
				dev.input.claimToken == claim.Token &&
				dev.input.claimedPresentationGeneration == claim.Generation
			dev.input.mu.Unlock()
			if !active {
				t.Fatal("invalid resolution released or replaced active claim")
			}
		})
	}

	if !dev.ResolveInputPresentation(
		claim, inputpresentation.OutcomeCommit, selectedAt.Add(2*time.Millisecond)) {
		t.Fatal("valid commit was rejected after invalid attempts")
	}
	if dev.ResolveInputPresentation(
		claim, inputpresentation.OutcomeCommit, selectedAt.Add(3*time.Millisecond)) {
		t.Fatal("valid claim committed more than once")
	}
	var presented [InputReportSize]byte
	dev.SnapshotInputReportInto(presented[:])
	if !bytes.Equal(report[:], presented[:]) {
		t.Fatal("valid terminal commit did not publish the original claim")
	}
}

func TestInputPresentationDeferralRetriesExactBytesAheadOfLaterTransition(
	t *testing.T,
) {
	dev := newInputPresentationTestDevice(t)
	base := dev.input.timestampBase.Add(20 * time.Millisecond)
	neutral := neutralInputState()
	dev.input.updateAt(&neutral, 0, base)
	press := neutral
	press.Buttons = ButtonCross
	dev.input.updateAt(&press, 0, base.Add(time.Millisecond))

	transport := presentationfake.New(
		InputReportSize, base.Add(2*time.Millisecond))
	first, ok := transport.Claim(dev)
	if !ok || !first.Ordered {
		t.Fatalf("ordered press claim=%+v ok=%v", first, ok)
	}
	// A contradictory release arrives while the press is immutable in the
	// transport. Deferral must put the identical press back ahead of it.
	dev.input.updateAt(&neutral, 0, base.Add(3*time.Millisecond))
	transport.Advance(2 * time.Millisecond)
	if _, accepted := transport.Resolve(
		dev, inputpresentation.OutcomeDefer); !accepted {
		t.Fatal("deferral was rejected")
	}

	retry, ok := transport.Claim(dev)
	if !ok || retry.Token == first.Token || retry.Generation != first.Generation {
		t.Fatalf("retry claim=%+v first=%+v", retry, first)
	}
	transport.Advance(time.Millisecond)
	if _, accepted := transport.Resolve(
		dev, inputpresentation.OutcomeCommit); !accepted {
		t.Fatal("retry commit was rejected")
	}
	_, ok = transport.Claim(dev)
	if !ok {
		t.Fatal("release was not claimable after retry")
	}
	transport.Advance(time.Millisecond)
	if _, accepted := transport.Resolve(
		dev, inputpresentation.OutcomeCommit); !accepted {
		t.Fatal("release commit was rejected")
	}

	records := transport.Records()
	if len(records) != 3 {
		t.Fatalf("records=%d want 3", len(records))
	}
	if !bytes.Equal(records[0].Report, records[1].Report) {
		t.Fatalf("deferred report changed on retry\nfirst=% x\nretry=% x",
			records[0].Report, records[1].Report)
	}
	if records[1].Report[8]&byte(ButtonCross) == 0 ||
		records[2].Report[8]&byte(ButtonCross) != 0 {
		t.Fatalf("retry/release order was not preserved: %02x -> %02x",
			records[1].Report[8], records[2].Report[8])
	}
}

func TestInputPresentationRetirementDoesNotLeakClaimIntoNextGeneration(
	t *testing.T,
) {
	dev := newInputPresentationTestDevice(t)
	base := dev.input.timestampBase.Add(30 * time.Millisecond)
	press := neutralInputState()
	press.Buttons = ButtonCross
	dev.input.updateAt(&press, 0, base)
	transport := presentationfake.New(InputReportSize, base.Add(time.Millisecond))
	retiredClaim, ok := transport.Claim(dev)
	if !ok {
		t.Fatal("press was not claimed")
	}
	transport.Advance(time.Millisecond)
	if !transport.RetireGeneration(dev, retiredClaim.Generation) {
		t.Fatal("active generation retirement was rejected")
	}
	if dev.ResolveInputPresentation(retiredClaim,
		inputpresentation.OutcomeCommit, transport.Now()) {
		t.Fatal("retired generation accepted a late commit")
	}
	if transport.RetireGeneration(dev, retiredClaim.Generation) {
		t.Fatal("retired generation was retired twice")
	}

	next, ok := transport.Claim(dev)
	if !ok || next.Generation == retiredClaim.Generation {
		t.Fatalf("successor claim=%+v retired=%+v", next, retiredClaim)
	}
	transport.Advance(time.Millisecond)
	record, accepted := transport.Resolve(dev, inputpresentation.OutcomeCommit)
	if !accepted {
		t.Fatal("successor neutral commit was rejected")
	}
	if record.Report[8]&byte(ButtonCross) != 0 {
		t.Fatalf("retired press leaked into generation %d: % x",
			next.Generation, record.Report[:11])
	}
	if !transport.RetireGeneration(dev, next.Generation) {
		t.Fatal("idle successor generation retirement was rejected")
	}
	third, ok := transport.Claim(dev)
	if !ok || third.Generation == next.Generation {
		t.Fatalf("idle retirement did not advance generation: %+v", third)
	}
}

func TestInputPresentationRetirementDiscardsDeferredClaim(t *testing.T) {
	dev := newInputPresentationTestDevice(t)
	base := dev.input.timestampBase.Add(35 * time.Millisecond)
	press := neutralInputState()
	press.Buttons = ButtonCross
	dev.input.updateAt(&press, 0, base)
	transport := presentationfake.New(InputReportSize, base.Add(time.Millisecond))
	deferred, ok := transport.Claim(dev)
	if !ok {
		t.Fatal("press was not claimed")
	}
	if _, accepted := transport.Resolve(
		dev, inputpresentation.OutcomeDefer); !accepted {
		t.Fatal("press deferral was rejected")
	}
	if !transport.RetireGeneration(dev, deferred.Generation) {
		t.Fatal("generation with deferred work was not retired")
	}
	next, ok := transport.Claim(dev)
	if !ok || next.Generation == deferred.Generation {
		t.Fatalf("successor claim=%+v deferred=%+v", next, deferred)
	}
	transport.Advance(time.Millisecond)
	record, accepted := transport.Resolve(dev, inputpresentation.OutcomeCommit)
	if !accepted || record.Report[8]&byte(ButtonCross) != 0 {
		t.Fatalf("deferred retired press leaked into successor: %+v % x",
			record, record.Report)
	}
}

func TestInputPresentationPreservesTriggerPeakBeforeRelease(t *testing.T) {
	dev := newInputPresentationTestDevice(t)
	base := dev.input.timestampBase.Add(40 * time.Millisecond)
	state := neutralInputState()
	dev.input.updateAt(&state, 0, base)
	state.R2 = 1
	dev.input.updateAt(&state, 0, base.Add(time.Millisecond))
	state.R2 = 255
	dev.input.updateAt(&state, 0, base.Add(2*time.Millisecond))
	state.R2 = 0
	state.Buttons &^= ButtonR2
	dev.input.updateAt(&state, 0, base.Add(3*time.Millisecond))

	transport := presentationfake.New(
		InputReportSize, base.Add(4*time.Millisecond))
	for range 2 {
		claim, ok := transport.Claim(dev)
		if !ok || !claim.Ordered {
			t.Fatalf("ordered trigger claim=%+v ok=%v", claim, ok)
		}
		transport.Advance(time.Millisecond)
		if _, accepted := transport.Resolve(
			dev, inputpresentation.OutcomeCommit); !accepted {
			t.Fatal("trigger claim commit was rejected")
		}
	}
	records := transport.Records()
	if records[0].Report[6] != 255 || records[1].Report[6] != 0 {
		t.Fatalf("trigger sequence=%d -> %d, want peak -> release",
			records[0].Report[6], records[1].Report[6])
	}
}

func TestInputPresentationSaturationDoesNotBlockProducerAndRetainsFinalState(
	t *testing.T,
) {
	dev := newInputPresentationTestDevice(t)
	base := dev.input.timestampBase.Add(50 * time.Millisecond)
	neutral := neutralInputState()
	dev.input.updateAt(&neutral, 0, base)
	transport := presentationfake.New(InputReportSize, base.Add(time.Millisecond))
	if _, ok := transport.Claim(dev); !ok {
		t.Fatal("failed to hold initial transport claim")
	}

	const publications = dualSenseInputTransitionCapacity*4 + 1
	publicationDone := make(chan struct{})
	go func() {
		state := neutral
		for index := range publications {
			if index%2 == 0 {
				state.Buttons = ButtonCross
			} else {
				state.Buttons = 0
			}
			dev.input.updateAt(
				&state, 0, base.Add(time.Duration(index+2)*time.Microsecond))
		}
		close(publicationDone)
	}()
	select {
	case <-publicationDone:
	case <-time.After(time.Second):
		t.Fatal("bounded scheduler blocked the producer while transport was stalled")
	}

	snapshot := dev.InputSchedulerState()
	if snapshot.TransitionDepth != dualSenseInputTransitionCapacity ||
		!snapshot.ContinuousPending || snapshot.Overflows == 0 {
		t.Fatalf("saturation was not bounded as expected: %+v", snapshot)
	}
	transport.Advance(time.Millisecond)
	if _, accepted := transport.Resolve(
		dev, inputpresentation.OutcomeCommit); !accepted {
		t.Fatal("held claim commit was rejected")
	}

	// Drain the fixed transition ring and its one latest recovery state.
	for range dualSenseInputTransitionCapacity + 1 {
		if _, ok := transport.Claim(dev); !ok {
			t.Fatal("saturated scheduler stopped producing during bounded drain")
		}
		transport.Advance(time.Microsecond)
		if _, accepted := transport.Resolve(
			dev, inputpresentation.OutcomeCommit); !accepted {
			t.Fatal("bounded drain commit was rejected")
		}
	}
	records := transport.Records()
	last := records[len(records)-1].Report
	if last[8]&byte(ButtonCross) == 0 {
		t.Fatalf("final recovery state was not retained: % x", last[:11])
	}
}

func TestInputPresentationReportsDeterministicMaximumEdgeAge(t *testing.T) {
	dev := newInputPresentationTestDevice(t)
	receivedAt := dev.input.timestampBase.Add(time.Second)
	state := neutralInputState()
	state.Buttons = ButtonSquare
	dev.input.updateAt(&state, 0, receivedAt)
	transport := presentationfake.New(
		InputReportSize, receivedAt.Add(7*time.Millisecond))
	claim, ok := transport.Claim(dev)
	if !ok || !claim.Ordered {
		t.Fatalf("edge claim=%+v ok=%v", claim, ok)
	}
	transport.Advance(3 * time.Millisecond)
	record, accepted := transport.Resolve(dev, inputpresentation.OutcomeCommit)
	if !accepted {
		t.Fatal("edge commit was rejected")
	}
	if record.Age != 10*time.Millisecond ||
		transport.MaximumAge() != 10*time.Millisecond {
		t.Fatalf("edge age=%s maximum=%s want 10ms",
			record.Age, transport.MaximumAge())
	}
	snapshot := dev.InputSchedulerState()
	if snapshot.MaximumQueueAge != 10*time.Millisecond ||
		snapshot.MaximumSelectionAge != 7*time.Millisecond {
		t.Fatalf("scheduler ages do not match fake clock: %+v", snapshot)
	}
}

func BenchmarkInputPresentationSaturatedProducer(b *testing.B) {
	dev := newInputPresentationTestDevice(b)
	base := dev.input.timestampBase.Add(time.Second)
	state := neutralInputState()
	for index := range dualSenseInputTransitionCapacity {
		if index%2 == 0 {
			state.Buttons = ButtonCross
		} else {
			state.Buttons = 0
		}
		dev.input.updateAt(&state, 0, base.Add(time.Duration(index)))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if index&1 == 0 {
			state.Buttons = ButtonCross
		} else {
			state.Buttons = 0
		}
		dev.input.updateAt(&state, 0, base.Add(time.Duration(index)))
	}
}

func BenchmarkInputPresentationClaimCommit(b *testing.B) {
	dev := newInputPresentationTestDevice(b)
	var report [InputReportSize]byte
	base := dev.input.timestampBase.Add(time.Second)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		at := base.Add(time.Duration(index))
		claim := dev.ClaimInputPresentation(report[:], at)
		if !dev.ResolveInputPresentation(
			claim, inputpresentation.OutcomeCommit, at) {
			b.Fatal("claim commit rejected")
		}
	}
}

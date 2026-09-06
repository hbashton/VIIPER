package ns2pro

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
	"github.com/Alia5/VIIPER/usbip"
)

func newEnabledPresentationDevice(t *testing.T) *NS2Pro {
	t.Helper()
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	dev.HandleTransfer(context.Background(), 2, usbip.DirOut,
		enableReportsCommand())
	return dev
}

func claimPresentation(t *testing.T, dev *NS2Pro,
	at time.Time,
) ([InputReportSize]byte, inputpresentation.Claim) {
	t.Helper()
	var report [InputReportSize]byte
	claim := dev.ClaimInputPresentation(report[:], at)
	if !claim.Valid() {
		t.Fatal("expected a valid ns2pro input claim")
	}
	return report, claim
}

func admitAndResolvePresentation(t *testing.T, dev *NS2Pro,
	claim inputpresentation.Claim, outcome inputpresentation.Outcome,
	at time.Time,
) {
	t.Helper()
	if !dev.CanAdmitInputPresentation(claim, at) {
		t.Fatal("expected ns2pro input claim admission")
	}
	if !dev.ResolveInputPresentation(claim, outcome, at) {
		t.Fatal("expected ns2pro input claim resolution")
	}
}

func proButtonPressed(report [InputReportSize]byte, mask byte) bool {
	return report[3]&mask != 0
}

func TestNS2ProPresentationJournalsButtonsAndCoalescesMotion(t *testing.T) {
	dev := newEnabledPresentationDevice(t)
	base := time.Now()
	lease := dev.input.ProducerLease()
	press := *NewInputState()
	press.Buttons = ButtonA
	lease, disposition := dev.publishInputStateWithLease(lease, press, base)
	if disposition != inputpresentation.FixedReportPublishAcceptedOrdered {
		t.Fatalf("press disposition = %d", disposition)
	}
	motion1 := press
	motion1.LX = 0x0900
	lease, disposition = dev.publishInputStateWithLease(lease, motion1,
		base.Add(time.Microsecond))
	if disposition != inputpresentation.FixedReportPublishAcceptedContinuous {
		t.Fatalf("first motion disposition = %d", disposition)
	}
	motion2 := press
	motion2.LX = 0x0a00
	lease, disposition = dev.publishInputStateWithLease(lease, motion2,
		base.Add(2*time.Microsecond))
	if disposition != inputpresentation.FixedReportPublishAcceptedContinuous {
		t.Fatalf("second motion disposition = %d", disposition)
	}
	release := *NewInputState()
	_, disposition = dev.publishInputStateWithLease(lease, release,
		base.Add(3*time.Microsecond))
	if disposition != inputpresentation.FixedReportPublishAcceptedOrdered {
		t.Fatalf("release disposition = %d", disposition)
	}

	first, firstClaim := claimPresentation(t, dev, base.Add(time.Millisecond))
	admitAndResolvePresentation(t, dev, firstClaim,
		inputpresentation.OutcomeCommit, base.Add(time.Millisecond))
	second, secondClaim := claimPresentation(t, dev,
		base.Add(2*time.Millisecond))
	admitAndResolvePresentation(t, dev, secondClaim,
		inputpresentation.OutcomeCommit, base.Add(2*time.Millisecond))
	third, thirdClaim := claimPresentation(t, dev,
		base.Add(3*time.Millisecond))
	admitAndResolvePresentation(t, dev, thirdClaim,
		inputpresentation.OutcomeCommit, base.Add(3*time.Millisecond))

	if !firstClaim.Ordered || !secondClaim.Ordered || !thirdClaim.Ordered {
		t.Fatalf("button-dependent claims were not all ordered: %v %v %v",
			firstClaim.Ordered, secondClaim.Ordered, thirdClaim.Ordered)
	}
	if !proButtonPressed(first, 0x02) || !proButtonPressed(second, 0x02) ||
		proButtonPressed(third, 0x02) {
		t.Fatalf("button sequence = %#x %#x %#x",
			first[3], second[3], third[3])
	}
	var packed [3]byte
	packStick12(packed[:], motion2.LX, motion2.LY)
	if !bytes.Equal(second[6:9], packed[:]) {
		t.Fatalf("coalesced motion = %x, want %x", second[6:9], packed)
	}
	snapshot := dev.InputSchedulerSnapshot()
	if snapshot.ContinuousReplaced != 1 || snapshot.TransitionDepth != 0 {
		t.Fatalf("scheduler snapshot = %+v", snapshot)
	}
}

func TestNS2ProPresentationDeferredReportRetriesExactBytes(t *testing.T) {
	dev := newEnabledPresentationDevice(t)
	state := *NewInputState()
	state.Buttons = ButtonB
	if !dev.UpdateInputState(state) {
		t.Fatal("input update failed")
	}
	base := time.Now()
	first, firstClaim := claimPresentation(t, dev, base)
	admitAndResolvePresentation(t, dev, firstClaim,
		inputpresentation.OutcomeDefer, base)
	retry, retryClaim := claimPresentation(t, dev, base.Add(time.Millisecond))
	if !bytes.Equal(first[:], retry[:]) || firstClaim.Token == retryClaim.Token ||
		firstClaim.ReceivedAt != retryClaim.ReceivedAt {
		t.Fatalf("retry changed: first=%+v retry=%+v\n%x\n%x",
			firstClaim, retryClaim, first, retry)
	}
	admitAndResolvePresentation(t, dev, retryClaim,
		inputpresentation.OutcomeCommit, base.Add(time.Millisecond))
}

func TestNS2ProRawProducerRetirementNeutralThenFreshWithoutGenerationRotation(
	t *testing.T,
) {
	dev := newEnabledPresentationDevice(t)
	oldLease, acquired := dev.acquireInputProducer()
	if !acquired {
		t.Fatal("failed to acquire initial producer")
	}
	state := *NewInputState()
	state.Buttons = ButtonA
	oldLease, disposition := dev.publishInputStateWithLease(oldLease, state,
		time.Now())
	if !disposition.Accepted() {
		t.Fatalf("initial disposition = %d", disposition)
	}
	oldReport, oldClaim := claimPresentation(t, dev, time.Now())
	if !proButtonPressed(oldReport, 0x02) {
		t.Fatal("old producer claim does not contain A")
	}
	generation := dev.InputPresentationGeneration()
	dev.releaseInputProducer()
	if dev.InputPresentationGeneration() != generation {
		t.Fatal("raw producer retirement rotated USB presentation generation")
	}
	control, handled := dev.HandleControl(hidClassRequestIn, hidGetReport,
		uint16(reportTypeInput)<<8|ReportIDPro, 0, InputReportSize, nil)
	if !handled || len(control) != InputReportSize || control[3] != 0 {
		t.Fatalf("post-retirement GET_REPORT exposed old producer: %x", control)
	}
	if dev.ResolveInputPresentation(oldClaim, inputpresentation.OutcomeCommit,
		time.Now()) {
		t.Fatal("retired producer claim committed")
	}

	newLease, acquired := dev.acquireInputProducer()
	if !acquired || !newLease.Valid() || newLease == oldLease {
		t.Fatalf("successor producer lease = %+v acquired=%v", newLease, acquired)
	}
	t.Cleanup(dev.releaseInputProducer)
	fresh := *NewInputState()
	fresh.Buttons = ButtonB
	_, disposition = dev.publishInputStateWithLease(newLease, fresh, time.Now())
	if disposition != inputpresentation.FixedReportPublishRejectedNeutralPending {
		t.Fatalf("pre-neutral successor disposition = %d", disposition)
	}
	neutral, neutralClaim := claimPresentation(t, dev, time.Now())
	if neutral[0] != ReportIDPro || neutral[3] != 0 {
		t.Fatalf("mandatory neutral = %x", neutral[:16])
	}
	admitAndResolvePresentation(t, dev, neutralClaim,
		inputpresentation.OutcomeCommit, time.Now())
	freshReport, freshClaim := claimPresentation(t, dev, time.Now())
	if !proButtonPressed(freshReport, 0x01) || freshClaim.Ordered {
		t.Fatalf("fresh successor report=%x claim=%+v",
			freshReport[:16], freshClaim)
	}
	admitAndResolvePresentation(t, dev, freshClaim,
		inputpresentation.OutcomeCommit, time.Now())
}

func TestNS2ProCompatibilityToRawHandoffFencesQueuedState(t *testing.T) {
	dev := newEnabledPresentationDevice(t)
	compatibilityLease := dev.input.ProducerLease()
	compatibilityEpoch := dev.InputSchedulerSnapshot().ProducerEpoch
	compatibilityState := *NewInputState()
	compatibilityState.Buttons = ButtonA
	if !dev.UpdateInputState(compatibilityState) {
		t.Fatal("compatibility state was rejected")
	}
	compatibilityReport, compatibilityClaim := claimPresentation(
		t, dev, time.Now())
	if !proButtonPressed(compatibilityReport, 0x02) {
		t.Fatal("pre-handoff claim did not contain compatibility A")
	}
	generation := dev.InputPresentationGeneration()
	rawLease, acquired := dev.acquireInputProducer()
	if !acquired || !rawLease.Valid() || rawLease == compatibilityLease {
		t.Fatalf("raw acquisition = %+v, %v", rawLease, acquired)
	}

	afterAcquire := dev.InputSchedulerSnapshot()
	if afterAcquire.Generation != generation ||
		afterAcquire.ProducerEpoch == compatibilityEpoch ||
		!afterAcquire.MandatoryNeutral || afterAcquire.TransitionDepth != 1 {
		t.Fatalf("compatibility handoff snapshot = %+v", afterAcquire)
	}
	if second, ok := dev.acquireInputProducer(); ok || second.Valid() {
		t.Fatalf("second raw acquisition = %+v, %v", second, ok)
	}
	afterRejectedAcquire := dev.InputSchedulerSnapshot()
	if afterRejectedAcquire.Generation != afterAcquire.Generation ||
		afterRejectedAcquire.ProducerEpoch != afterAcquire.ProducerEpoch ||
		afterRejectedAcquire.TransitionDepth != afterAcquire.TransitionDepth {
		t.Fatalf("failed acquisition mutated ownership: before=%+v after=%+v",
			afterAcquire, afterRejectedAcquire)
	}
	control, handled := dev.HandleControl(hidClassRequestIn, hidGetReport,
		uint16(reportTypeInput)<<8|ReportIDPro, 0, InputReportSize, nil)
	if !handled || len(control) != InputReportSize || control[3] != 0 {
		t.Fatalf("handoff GET_REPORT exposed compatibility state: %x", control)
	}
	if dev.CanAdmitInputPresentation(compatibilityClaim, time.Now()) ||
		dev.ResolveInputPresentation(compatibilityClaim,
			inputpresentation.OutcomeCommit, time.Now()) {
		t.Fatal("pre-handoff compatibility claim survived raw acquisition")
	}

	staleAt := time.Now()
	_, disposition := dev.publishInputStateWithLease(
		compatibilityLease, compatibilityState, staleAt)
	if disposition != inputpresentation.FixedReportPublishRejectedStaleProducer {
		t.Fatalf("old compatibility lease disposition = %d", disposition)
	}
	if dev.hasPendingResync {
		t.Fatal("retired compatibility state became a raw resynchronization candidate")
	}
	rawState := *NewInputState()
	rawState.Buttons = ButtonB
	rawAt := staleAt.Add(time.Millisecond)
	rawLease, disposition = dev.publishInputStateWithLease(
		rawLease, rawState, rawAt)
	if disposition != inputpresentation.FixedReportPublishRejectedNeutralPending {
		t.Fatalf("pre-neutral raw disposition = %d", disposition)
	}

	neutral, neutralClaim := claimPresentation(t, dev,
		rawAt.Add(time.Millisecond))
	if neutral[3] != 0 {
		t.Fatalf("first post-handoff report was not neutral: %x", neutral[:16])
	}
	admitAndResolvePresentation(t, dev, neutralClaim,
		inputpresentation.OutcomeCommit, rawAt.Add(time.Millisecond))
	fresh, freshClaim := claimPresentation(t, dev,
		rawAt.Add(2*time.Millisecond))
	if proButtonPressed(fresh, 0x02) || !proButtonPressed(fresh, 0x01) ||
		freshClaim.Ordered {
		t.Fatalf("post-handoff raw report=%x claim=%+v", fresh[:16], freshClaim)
	}
	admitAndResolvePresentation(t, dev, freshClaim,
		inputpresentation.OutcomeCommit, rawAt.Add(2*time.Millisecond))

	dev.releaseInputProducer()
	afterRelease := dev.InputSchedulerSnapshot()
	if afterRelease.Generation != generation || !afterRelease.MandatoryNeutral ||
		afterRelease.TransitionDepth != 1 {
		t.Fatalf("raw release duplicated/rotated neutral boundary: %+v",
			afterRelease)
	}
}

func TestNS2ProCompatibilityAndRawAcquisitionLinearize(t *testing.T) {
	for iteration := 0; iteration < 32; iteration++ {
		dev := newEnabledPresentationDevice(t)
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		var compatibilityAccepted bool
		var rawLease inputpresentation.FixedReportProducerLease
		var rawAcquired bool
		go func() {
			defer wait.Done()
			<-start
			state := *NewInputState()
			state.Buttons = ButtonA
			compatibilityAccepted = dev.UpdateInputState(state)
		}()
		go func() {
			defer wait.Done()
			<-start
			rawLease, rawAcquired = dev.acquireInputProducer()
		}()
		close(start)
		wait.Wait()
		if !rawAcquired || !rawLease.Valid() {
			t.Fatalf("iteration %d raw acquisition failed", iteration)
		}
		snapshot := dev.InputSchedulerSnapshot()
		if compatibilityAccepted != snapshot.MandatoryNeutral {
			t.Fatalf("iteration %d non-linearized result: compat=%v snapshot=%+v",
				iteration, compatibilityAccepted, snapshot)
		}
		dev.releaseInputProducer()
		afterRelease := dev.InputSchedulerSnapshot()
		if afterRelease.Generation != snapshot.Generation ||
			!afterRelease.MandatoryNeutral || afterRelease.TransitionDepth != 1 {
			t.Fatalf("iteration %d release duplicated neutral: %+v",
				iteration, afterRelease)
		}
	}
}

func TestNS2ProPresentationOverflowFailsClosedToNeutralAndFreshState(
	t *testing.T,
) {
	dev := newEnabledPresentationDevice(t)
	base := time.Now()
	lease := dev.input.ProducerLease()
	for index := 0; index < inputpresentation.FixedReportTransitionCapacity; index++ {
		state := *NewInputState()
		if index%2 == 0 {
			state.Buttons = ButtonA
		} else {
			state.Buttons = ButtonB
		}
		var disposition inputpresentation.FixedReportPublishDisposition
		lease, disposition = dev.publishInputStateWithLease(lease, state,
			base.Add(time.Duration(index)*time.Microsecond))
		if disposition != inputpresentation.FixedReportPublishAcceptedOrdered {
			t.Fatalf("fill %d disposition = %d", index, disposition)
		}
	}
	overflow := *NewInputState()
	overflow.Buttons = ButtonA
	oldLease := lease
	lease, disposition := dev.publishInputStateWithLease(lease, overflow,
		base.Add(time.Second))
	if disposition != inputpresentation.FixedReportPublishFaultedOverflow ||
		lease == oldLease {
		t.Fatalf("overflow disposition=%d lease=%+v old=%+v",
			disposition, lease, oldLease)
	}
	snapshot := dev.InputSchedulerSnapshot()
	if !snapshot.MandatoryNeutral || snapshot.TransitionDepth != 1 ||
		snapshot.Overflows != 1 {
		t.Fatalf("overflow snapshot = %+v", snapshot)
	}
	control, handled := dev.HandleControl(hidClassRequestIn, hidGetReport,
		uint16(reportTypeInput)<<8|ReportIDPro, 0, InputReportSize, nil)
	if !handled || len(control) != InputReportSize || control[3] != 0 {
		t.Fatalf("overflow GET_REPORT exposed purged state: %x", control)
	}
	neutral, neutralClaim := claimPresentation(t, dev, base.Add(time.Second))
	if neutral[3] != 0 {
		t.Fatalf("overflow neutral buttons = %#x", neutral[3])
	}
	admitAndResolvePresentation(t, dev, neutralClaim,
		inputpresentation.OutcomeCommit, base.Add(time.Second))
	fresh, freshClaim := claimPresentation(t, dev,
		base.Add(time.Second+time.Millisecond))
	if !proButtonPressed(fresh, 0x02) || freshClaim.Ordered {
		t.Fatalf("overflow resync report=%x claim=%+v", fresh[:16], freshClaim)
	}
}

func TestNS2ProUSBGenerationRetirementRejectsOldClaimAndReencodesCurrent(
	t *testing.T,
) {
	dev := newEnabledPresentationDevice(t)
	state := *NewInputState()
	state.Buttons = ButtonA
	if !dev.UpdateInputState(state) {
		t.Fatal("input update failed")
	}
	oldReport, oldClaim := claimPresentation(t, dev, time.Now())
	if oldReport[1] != 1 {
		t.Fatalf("old counter = %d, want 1", oldReport[1])
	}
	if !dev.RetireInputPresentationGeneration(oldClaim.Generation, time.Now()) {
		t.Fatal("generation retirement failed")
	}
	if dev.CanAdmitInputPresentation(oldClaim, time.Now()) ||
		dev.ResolveInputPresentation(oldClaim, inputpresentation.OutcomeCommit,
			time.Now()) {
		t.Fatal("retired generation claim remained live")
	}
	successor, successorClaim := claimPresentation(t, dev, time.Now())
	if successorClaim.Generation == oldClaim.Generation || successorClaim.Ordered ||
		!proButtonPressed(successor, 0x02) || successor[1] != 2 {
		t.Fatalf("successor report=%x claim=%+v", successor[:16], successorClaim)
	}
	admitAndResolvePresentation(t, dev, successorClaim,
		inputpresentation.OutcomeCommit, time.Now())
}

func TestNS2ProUSBGenerationRetirementPreviewsCollapsedCurrentOnControl(
	t *testing.T,
) {
	dev := newEnabledPresentationDevice(t)
	press := *NewInputState()
	press.Buttons = ButtonA
	if !dev.UpdateInputState(press) {
		t.Fatal("press update failed")
	}
	_, pressClaim := claimPresentation(t, dev, time.Now())
	admitAndResolvePresentation(t, dev, pressClaim,
		inputpresentation.OutcomeCommit, time.Now())
	var before [InputReportSize]byte
	_, beforeVersion := dev.SnapshotInputReportInto(before[:])

	release := *NewInputState()
	if !dev.UpdateInputState(release) {
		t.Fatal("release update failed")
	}
	generation := dev.InputPresentationGeneration()
	if !dev.RetireInputPresentationGeneration(generation, time.Now()) {
		t.Fatal("generation retirement failed")
	}
	if dev.InputReportSnapshotCurrent(beforeVersion) {
		t.Fatal("pre-retirement control snapshot remained current")
	}
	control, handled := dev.HandleControl(hidClassRequestIn, hidGetReport,
		uint16(reportTypeInput)<<8|ReportIDPro, 0, InputReportSize, nil)
	if !handled || len(control) != InputReportSize || control[3]&0x02 != 0 ||
		control[1] != before[1] {
		t.Fatalf("retirement control preview = %x; before=%x",
			control[:16], before[:16])
	}
	successor, successorClaim := claimPresentation(t, dev, time.Now())
	if proButtonPressed(successor, 0x02) || successorClaim.Ordered ||
		successor[1] != before[1]+1 {
		t.Fatalf("retirement successor = %x claim=%+v",
			successor[:16], successorClaim)
	}
}

func TestNS2ProFaultGenerationRetirementControlPreviewRemainsNeutral(
	t *testing.T,
) {
	dev := newEnabledPresentationDevice(t)
	press := *NewInputState()
	press.Buttons = ButtonA
	if !dev.UpdateInputState(press) {
		t.Fatal("press update failed")
	}
	_, pressClaim := claimPresentation(t, dev, time.Now())
	admitAndResolvePresentation(t, dev, pressClaim,
		inputpresentation.OutcomeCommit, time.Now())

	base := time.Now()
	lease := dev.input.ProducerLease()
	for index := 0; index < inputpresentation.FixedReportTransitionCapacity; index++ {
		state := *NewInputState()
		if index%2 == 0 {
			state.Buttons = ButtonB
		} else {
			state.Buttons = ButtonA
		}
		var disposition inputpresentation.FixedReportPublishDisposition
		lease, disposition = dev.publishInputStateWithLease(lease, state,
			base.Add(time.Duration(index)*time.Microsecond))
		if disposition != inputpresentation.FixedReportPublishAcceptedOrdered {
			t.Fatalf("fill %d disposition = %d", index, disposition)
		}
	}
	faultState := *NewInputState()
	faultState.Buttons = ButtonB
	_, disposition := dev.publishInputStateWithLease(lease, faultState,
		base.Add(time.Second))
	if disposition != inputpresentation.FixedReportPublishFaultedOverflow {
		t.Fatalf("overflow disposition = %d", disposition)
	}
	if !dev.RetireInputPresentationGeneration(
		dev.InputPresentationGeneration(), base.Add(2*time.Second)) {
		t.Fatal("fault generation retirement failed")
	}
	control, handled := dev.HandleControl(hidClassRequestIn, hidGetReport,
		uint16(reportTypeInput)<<8|ReportIDPro, 0, InputReportSize, nil)
	if !handled || len(control) != InputReportSize || control[3] != 0 {
		t.Fatalf("fault retirement preview resurrected staged state: %x",
			control[:16])
	}
}

func TestNS2ProWrongGenerationRetirementPreservesStagedResynchronization(
	t *testing.T,
) {
	dev := newEnabledPresentationDevice(t)
	_, acquired := dev.acquireInputProducer()
	if !acquired {
		t.Fatal("failed to acquire initial producer")
	}
	dev.releaseInputProducer()
	lease, acquired := dev.acquireInputProducer()
	if !acquired {
		t.Fatal("failed to acquire successor producer")
	}
	t.Cleanup(dev.releaseInputProducer)
	state := *NewInputState()
	state.Buttons = ButtonB
	_, disposition := dev.publishInputStateWithLease(lease, state, time.Now())
	if disposition != inputpresentation.FixedReportPublishRejectedNeutralPending ||
		!dev.hasPendingResync {
		t.Fatalf("staged disposition=%d pending=%v",
			disposition, dev.hasPendingResync)
	}
	wrongGeneration := dev.InputPresentationGeneration() + 1
	if dev.RetireInputPresentationGeneration(wrongGeneration, time.Now()) {
		t.Fatal("wrong generation retirement succeeded")
	}
	if !dev.hasPendingResync {
		t.Fatal("wrong generation retirement erased current resynchronization")
	}
	_, neutralClaim := claimPresentation(t, dev, time.Now())
	admitAndResolvePresentation(t, dev, neutralClaim,
		inputpresentation.OutcomeCommit, time.Now())
	fresh, freshClaim := claimPresentation(t, dev, time.Now())
	if !proButtonPressed(fresh, 0x01) || freshClaim.Ordered {
		t.Fatalf("fresh staged report=%x claim=%+v", fresh[:16], freshClaim)
	}
}

func TestNS2ProReportModeChangeRevokesActiveOldModeClaim(t *testing.T) {
	dev := newEnabledPresentationDevice(t)
	state := *NewInputState()
	state.Buttons = ButtonA
	if !dev.UpdateInputState(state) {
		t.Fatal("input update failed")
	}
	old, oldClaim := claimPresentation(t, dev, time.Now())
	if old[0] != ReportIDPro {
		t.Fatalf("old report ID = %#x", old[0])
	}
	dev.HandleTransfer(context.Background(), 2, usbip.DirOut,
		selectReportCommand(ReportIDCommon))
	if dev.CanAdmitInputPresentation(oldClaim, time.Now()) ||
		dev.ResolveInputPresentation(oldClaim, inputpresentation.OutcomeDefer,
			time.Now()) {
		t.Fatal("old report-mode claim survived selection command")
	}
	control, handled := dev.HandleControl(hidClassRequestIn, hidGetReport,
		uint16(reportTypeInput)<<8|ReportIDCommon, 0, InputReportSize, nil)
	if !handled || len(control) != InputReportSize || control[0] != ReportIDCommon ||
		control[5]&0x08 != 0 {
		t.Fatalf("mode-fence GET_REPORT = %x", control)
	}
	neutral, neutralClaim := claimPresentation(t, dev, time.Now())
	if neutral[0] != ReportIDCommon || neutral[5]&0x08 != 0 {
		t.Fatalf("new-mode neutral = %x", neutral[:17])
	}
	admitAndResolvePresentation(t, dev, neutralClaim,
		inputpresentation.OutcomeCommit, time.Now())
	fresh, freshClaim := claimPresentation(t, dev, time.Now())
	if fresh[0] != ReportIDCommon || fresh[5]&0x08 == 0 {
		t.Fatalf("new-mode fresh state = %x", fresh[:17])
	}
	admitAndResolvePresentation(t, dev, freshClaim,
		inputpresentation.OutcomeCommit, time.Now())
}

func TestNS2ProFeatureChangeFencesDeferredOldBytes(t *testing.T) {
	dev := newEnabledPresentationDevice(t)
	state := *NewInputState()
	state.Buttons = ButtonA
	if !dev.UpdateInputState(state) {
		t.Fatal("input update failed")
	}
	old, oldClaim := claimPresentation(t, dev, time.Now())
	if old[12] != 0x30 {
		t.Fatalf("old feature byte = %#x", old[12])
	}
	admitAndResolvePresentation(t, dev, oldClaim,
		inputpresentation.OutcomeDefer, time.Now())
	dev.HandleTransfer(context.Background(), 2, usbip.DirOut,
		featureCommand(subFeatureEnable, FeatureRumble))
	neutral, neutralClaim := claimPresentation(t, dev, time.Now())
	if neutral[12] != 0x38 || bytes.Equal(neutral[:], old[:]) {
		t.Fatalf("feature-fenced neutral=%x old=%x",
			neutral[:16], old[:16])
	}
	admitAndResolvePresentation(t, dev, neutralClaim,
		inputpresentation.OutcomeCommit, time.Now())
	fresh, freshClaim := claimPresentation(t, dev, time.Now())
	if fresh[12] != 0x38 || !proButtonPressed(fresh, 0x02) {
		t.Fatalf("feature-fenced fresh state = %x", fresh[:16])
	}
	admitAndResolvePresentation(t, dev, freshClaim,
		inputpresentation.OutcomeCommit, time.Now())
}

func TestNS2ProReportDisableRevokesActiveClaimUntilReenabled(t *testing.T) {
	dev := newEnabledPresentationDevice(t)
	state := *NewInputState()
	state.Buttons = ButtonA
	if !dev.UpdateInputState(state) {
		t.Fatal("input update failed")
	}
	_, oldClaim := claimPresentation(t, dev, time.Now())
	disable := []byte{cmdUSB, 0x91, 0, subUSBEnableReports, 0, 4, 0, 0, 0}
	dev.HandleTransfer(context.Background(), 2, usbip.DirOut, disable)
	if dev.CanAdmitInputPresentation(oldClaim, time.Now()) {
		t.Fatal("disabled report claim remained admissible")
	}
	var report [InputReportSize]byte
	if claim := dev.ClaimInputPresentation(report[:], time.Now()); claim.Valid() {
		t.Fatalf("disabled source returned claim %+v", claim)
	}
	dev.HandleTransfer(context.Background(), 2, usbip.DirOut,
		enableReportsCommand())
	neutral, claim := claimPresentation(t, dev, time.Now())
	if neutral[3] != 0 {
		t.Fatalf("reenabled mandatory neutral buttons = %#x", neutral[3])
	}
	admitAndResolvePresentation(t, dev, claim,
		inputpresentation.OutcomeCommit, time.Now())
}

func TestNS2ProControlGetReportDoesNotConsumeJournalOrInterruptCounter(
	t *testing.T,
) {
	dev := newEnabledPresentationDevice(t)
	state := *NewInputState()
	state.Buttons = ButtonA
	if !dev.UpdateInputState(state) {
		t.Fatal("input update failed")
	}
	before := dev.InputSchedulerSnapshot()
	wValue := uint16(reportTypeInput)<<8 | ReportIDPro
	control, handled := dev.HandleControl(hidClassRequestIn, hidGetReport,
		wValue, 0, InputReportSize, nil)
	if !handled || len(control) != InputReportSize || control[1] != 0 {
		t.Fatalf("control report handled=%v length=%d counter=%d",
			handled, len(control), control[1])
	}
	if control[3] != 0 {
		t.Fatalf("GET_REPORT overtook pending press: %x", control[:16])
	}
	after := dev.InputSchedulerSnapshot()
	if after.Selected != before.Selected ||
		after.TransitionDepth != before.TransitionDepth {
		t.Fatalf("GET_REPORT consumed scheduler work: before=%+v after=%+v",
			before, after)
	}
	interrupt, claim := claimPresentation(t, dev, time.Now())
	if interrupt[1] != 1 || !proButtonPressed(interrupt, 0x02) {
		t.Fatalf("first interrupt report = %x", interrupt[:16])
	}
	admitAndResolvePresentation(t, dev, claim,
		inputpresentation.OutcomeCommit, time.Now())
	control, handled = dev.HandleControl(hidClassRequestIn, hidGetReport,
		wValue, 0, InputReportSize, nil)
	if !handled || control[1] != 1 {
		t.Fatalf("post-interrupt control counter = %d", control[1])
	}
	release := *NewInputState()
	if !dev.UpdateInputState(release) {
		t.Fatal("release update failed")
	}
	control, handled = dev.HandleControl(hidClassRequestIn, hidGetReport,
		wValue, 0, InputReportSize, nil)
	if !handled || control[3]&0x02 == 0 {
		t.Fatalf("GET_REPORT overtook pending release: %x", control[:16])
	}
	next, nextClaim := claimPresentation(t, dev, time.Now())
	if next[1] != 2 {
		t.Fatalf("second interrupt counter = %d", next[1])
	}
	if proButtonPressed(next, 0x02) {
		t.Fatalf("second interrupt did not present release: %x", next[:16])
	}
	admitAndResolvePresentation(t, dev, nextClaim,
		inputpresentation.OutcomeCommit, time.Now())
}

func TestNS2ProInactiveLayoutGetReportUsesLastCommittedSemanticState(
	t *testing.T,
) {
	dev := newEnabledPresentationDevice(t)
	state := *NewInputState()
	state.Buttons = ButtonA
	if !dev.UpdateInputState(state) {
		t.Fatal("input update failed")
	}
	_, claim := claimPresentation(t, dev, time.Now())
	admitAndResolvePresentation(t, dev, claim,
		inputpresentation.OutcomeCommit, time.Now())
	before := dev.InputSchedulerSnapshot()
	common, handled := dev.HandleControl(hidClassRequestIn, hidGetReport,
		uint16(reportTypeInput)<<8|ReportIDCommon, 0, InputReportSize, nil)
	if !handled || len(common) != InputReportSize ||
		common[0] != ReportIDCommon || common[5]&0x08 == 0 {
		t.Fatalf("inactive-layout committed controls = %x", common[:17])
	}
	if got := binary.LittleEndian.Uint32(common[1:5]); got != 0 {
		t.Fatalf("inactive Common GET_REPORT advanced counter to %d", got)
	}
	active, handled := dev.HandleControl(hidClassRequestIn, hidGetReport,
		uint16(reportTypeInput)<<8, 0, InputReportSize, nil)
	if !handled || len(active) != InputReportSize ||
		active[0] != ReportIDPro || active[3]&0x02 == 0 {
		t.Fatalf("report-ID-zero committed controls = %x", active[:16])
	}
	after := dev.InputSchedulerSnapshot()
	if after.Selected != before.Selected ||
		after.TransitionDepth != before.TransitionDepth {
		t.Fatalf("inactive GET_REPORT consumed interrupt work: before=%+v after=%+v",
			before, after)
	}
}

func TestNS2ProNonFencedConfigurationReencodesCommittedControls(t *testing.T) {
	dev := newEnabledPresentationDevice(t)
	state := *NewInputState()
	state.Buttons = ButtonA
	if !dev.UpdateInputState(state) {
		t.Fatal("input update failed")
	}
	_, claim := claimPresentation(t, dev, time.Now())
	admitAndResolvePresentation(t, dev, claim,
		inputpresentation.OutcomeCommit, time.Now())

	dev.HandleTransfer(context.Background(), 2, usbip.DirOut,
		featureCommand(subFeatureEnable, FeatureRumble))
	pro, handled := dev.HandleControl(hidClassRequestIn, hidGetReport,
		uint16(reportTypeInput)<<8|ReportIDPro, 0, InputReportSize, nil)
	if !handled || pro[3]&0x02 == 0 || pro[12] != 0x38 {
		t.Fatalf("feature re-encoding lost committed press: %x", pro[:16])
	}

	dev.SetMetaState(MetaState{
		SerialNumber:  DefaultSerial,
		BatteryLevel:  5,
		ExternalPower: true,
		BatteryVolts:  DefaultBatteryVolts,
	})
	pro, handled = dev.HandleControl(hidClassRequestIn, hidGetReport,
		uint16(reportTypeInput)<<8|ReportIDPro, 0, InputReportSize, nil)
	if !handled || pro[3]&0x02 == 0 || pro[2] != 0x15 {
		t.Fatalf("metadata re-encoding lost committed press: %x", pro[:16])
	}

	dev.HandleTransfer(context.Background(), 2, usbip.DirOut,
		selectReportCommand(ReportIDCommon))
	common, handled := dev.HandleControl(hidClassRequestIn, hidGetReport,
		uint16(reportTypeInput)<<8|ReportIDCommon, 0, InputReportSize, nil)
	if !handled || common[0] != ReportIDCommon || common[5]&0x08 == 0 {
		t.Fatalf("mode re-encoding lost committed press: %x", common[:17])
	}
}

func TestNS2ProModeChangeDuringResynchronizationCannotReplayOldLayout(
	t *testing.T,
) {
	dev := newEnabledPresentationDevice(t)
	_, acquired := dev.acquireInputProducer()
	if !acquired {
		t.Fatal("initial producer acquisition failed")
	}
	dev.releaseInputProducer()
	oldNeutral, oldNeutralClaim := claimPresentation(t, dev, time.Now())
	if oldNeutral[0] != ReportIDPro {
		t.Fatalf("initial mandatory neutral ID = %#x", oldNeutral[0])
	}
	admitAndResolvePresentation(t, dev, oldNeutralClaim,
		inputpresentation.OutcomeCommit, time.Now())
	if !dev.InputSchedulerSnapshot().Resynchronization {
		t.Fatal("expected post-neutral resynchronization state")
	}

	dev.HandleTransfer(context.Background(), 2, usbip.DirOut,
		selectReportCommand(ReportIDCommon))
	if !dev.InputSchedulerSnapshot().MandatoryNeutral {
		t.Fatal("mode change did not fence resynchronization with a new neutral")
	}
	lease, acquired := dev.acquireInputProducer()
	if !acquired {
		t.Fatal("successor producer acquisition failed")
	}
	t.Cleanup(dev.releaseInputProducer)
	freshState := *NewInputState()
	freshState.Buttons = ButtonA
	_, disposition := dev.publishInputStateWithLease(lease, freshState,
		time.Now())
	if disposition != inputpresentation.FixedReportPublishRejectedNeutralPending {
		t.Fatalf("fresh staging disposition = %d", disposition)
	}
	newNeutral, newNeutralClaim := claimPresentation(t, dev, time.Now())
	if newNeutral[0] != ReportIDCommon || newNeutral[5]&0x08 != 0 {
		t.Fatalf("new-layout neutral = %x", newNeutral[:17])
	}
	admitAndResolvePresentation(t, dev, newNeutralClaim,
		inputpresentation.OutcomeCommit, time.Now())
	fresh, freshClaim := claimPresentation(t, dev, time.Now())
	if fresh[0] != ReportIDCommon || fresh[5]&0x08 == 0 {
		t.Fatalf("new-layout fresh report = %x", fresh[:17])
	}
	admitAndResolvePresentation(t, dev, freshClaim,
		inputpresentation.OutcomeCommit, time.Now())
}

func TestNS2ProSourceOwnsOnlyMainHIDInputEndpoint(t *testing.T) {
	dev := newEnabledPresentationDevice(t)
	if !dev.OwnsInputPresentationEndpoint(EndpointHIDIn&0x0f) ||
		dev.OwnsInputPresentationEndpoint(EndpointBulkIn&0x0f) {
		t.Fatal("ns2pro presentation source endpoint ownership is incorrect")
	}
	if !dev.SupportsInputReportSnapshot(0) ||
		!dev.SupportsInputReportSnapshot(ReportIDCommon) ||
		!dev.SupportsInputReportSnapshot(ReportIDPro) ||
		dev.SupportsInputReportSnapshot(0x01) {
		t.Fatal("ns2pro GET_REPORT snapshot ID ownership is incorrect")
	}
}

func TestNS2ProOnlyOneRawInputProducerOwnsLease(t *testing.T) {
	dev := newEnabledPresentationDevice(t)
	first, acquired := dev.acquireInputProducer()
	if !acquired || !first.Valid() {
		t.Fatal("first raw producer acquisition failed")
	}
	if second, ok := dev.acquireInputProducer(); ok || second.Valid() {
		t.Fatalf("second producer acquisition = %+v, %v", second, ok)
	}
	if dev.UpdateInputState(*NewInputState()) {
		t.Fatal("compatibility producer interleaved with raw producer")
	}
	dev.releaseInputProducer()
	if successor, ok := dev.acquireInputProducer(); !ok || !successor.Valid() ||
		successor == first {
		t.Fatalf("successor acquisition = %+v, %v", successor, ok)
	}
	dev.releaseInputProducer()
}

func TestNS2ProInputBuildHotPathDoesNotAllocate(t *testing.T) {
	dev := newEnabledPresentationDevice(t)
	var report [InputReportSize]byte
	allocations := testing.AllocsPerRun(1000, func() {
		if n := dev.BuildInputReportInto(report[:]); n != InputReportSize {
			t.Fatalf("report size = %d", n)
		}
	})
	if allocations != 0 {
		t.Fatalf("allocations per input report = %f", allocations)
	}
}

func TestNS2ProMotionTimestampAndCounterAdvanceAtUniqueClaimsOnly(t *testing.T) {
	dev := newEnabledPresentationDevice(t)
	dev.HandleTransfer(context.Background(), 2, usbip.DirOut,
		featureCommand(subFeatureEnable, FeatureIMU))
	dev.HandleTransfer(context.Background(), 2, usbip.DirOut,
		selectReportCommand(ReportIDCommon))
	first, firstClaim := claimPresentation(t, dev, time.Now())
	firstCounter := binary.LittleEndian.Uint32(first[1:5])
	firstMotion := binary.LittleEndian.Uint32(first[0x2b:0x2f])
	admitAndResolvePresentation(t, dev, firstClaim,
		inputpresentation.OutcomeDefer, time.Now())
	time.Sleep(time.Millisecond)
	retry, retryClaim := claimPresentation(t, dev, time.Now())
	if !bytes.Equal(first[:], retry[:]) {
		t.Fatal("deferred Common report changed counter or motion timestamp")
	}
	admitAndResolvePresentation(t, dev, retryClaim,
		inputpresentation.OutcomeCommit, time.Now())
	time.Sleep(time.Millisecond)
	next, nextClaim := claimPresentation(t, dev, time.Now())
	if got := binary.LittleEndian.Uint32(next[1:5]); got != firstCounter+1 {
		t.Fatalf("next Common counter = %d, want %d", got, firstCounter+1)
	}
	if got := binary.LittleEndian.Uint32(next[0x2b:0x2f]); got <= firstMotion {
		t.Fatalf("next motion timestamp = %d, want > %d", got, firstMotion)
	}
	admitAndResolvePresentation(t, dev, nextClaim,
		inputpresentation.OutcomeCommit, time.Now())
}

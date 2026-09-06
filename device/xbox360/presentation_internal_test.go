package xbox360

import (
	"sync"
	"testing"
	"time"

	devicebase "github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/internal/inputpresentation"
	"github.com/stretchr/testify/require"
)

func TestStrictCreatePolicyIsExplicitAndReported(t *testing.T) {
	dev, err := New(&devicebase.CreateOptions{DeviceSpecific: `{"maximumOrderedAgeMilliseconds":17}`})
	require.NoError(t, err)
	require.Equal(t, 17*time.Millisecond,
		dev.InputSchedulerSnapshot().MaximumOrderedAge)
	require.Equal(t, int64(17),
		dev.GetDeviceSpecificArgs()["maximumOrderedAgeMilliseconds"])

	for _, payload := range []string{
		`{"maximumOrderedAgeMilliseconds":0}`,
		`{"maximumOrderedAgeMilliseconds":60001}`,
		`{"maximumOrderedAgeMilliseconds":"1"}`,
	} {
		_, err := New(&devicebase.CreateOptions{DeviceSpecific: payload})
		require.Error(t, err, payload)
	}
}

func TestStrictOverflowNeutralThenFreshProducerResynchronization(t *testing.T) {
	dev, err := New(&devicebase.CreateOptions{DeviceSpecific: `{"maximumOrderedAgeMilliseconds":1000}`})
	require.NoError(t, err)
	lease, acquired := dev.acquireInputProducer()
	require.True(t, acquired)
	defer dev.releaseInputProducer()
	_, acquired = dev.acquireInputProducer()
	require.False(t, acquired, "a second raw stream cannot become a producer")

	base := time.Now().Add(-time.Second)
	var disposition inputpresentation.FixedReportPublishDisposition
	for index := 0; index < inputpresentation.FixedReportTransitionCapacity; index++ {
		state := InputState{}
		if index%2 == 0 {
			state.Buttons = ButtonA
		}
		lease, disposition = dev.publishInputStateWithLease(lease, state,
			base.Add(time.Duration(index)*time.Microsecond))
		require.True(t, disposition.Accepted(), "edge %d: %d", index,
			disposition)
	}

	fresh := InputState{Buttons: ButtonA, LX: 12345}
	lease, disposition = dev.publishInputStateWithLease(lease, fresh,
		base.Add(100*time.Microsecond))
	require.Equal(t, inputpresentation.FixedReportPublishFaultedOverflow,
		disposition)
	require.True(t, lease.Valid())
	require.True(t, dev.InputSchedulerSnapshot().MandatoryNeutral)

	var report [20]byte
	neutralClaim := dev.ClaimInputPresentation(report[:],
		base.Add(101*time.Microsecond))
	require.True(t, neutralClaim.Valid())
	require.Equal(t, NewInputState().BuildReport(), report[:])
	require.True(t, dev.CanAdmitInputPresentation(neutralClaim,
		base.Add(102*time.Microsecond)))
	require.True(t, dev.ResolveInputPresentation(neutralClaim,
		inputpresentation.OutcomeCommit,
		base.Add(103*time.Microsecond)))

	require.False(t, dev.InputSchedulerSnapshot().Resynchronization,
		"fresh post-fault state should be explicitly resynchronized")
	freshClaim := dev.ClaimInputPresentation(report[:],
		base.Add(104*time.Microsecond))
	require.True(t, freshClaim.Valid())
	require.Equal(t, fresh.BuildReport(), report[:])
	require.True(t, dev.CanAdmitInputPresentation(freshClaim,
		base.Add(105*time.Microsecond)))
	require.True(t, dev.ResolveInputPresentation(freshClaim,
		inputpresentation.OutcomeCommit,
		base.Add(106*time.Microsecond)))
}

func TestRawStreamProducerHandoffCannotLeakOldState(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)
	firstLease, acquired := dev.acquireInputProducer()
	require.True(t, acquired)
	base := time.Now().Add(-time.Second)
	first := InputState{Buttons: ButtonA, LX: 111}
	_, disposition := dev.publishInputStateWithLease(firstLease, first, base)
	require.True(t, disposition.Accepted())
	dev.releaseInputProducer()
	require.True(t, dev.InputSchedulerSnapshot().MandatoryNeutral)

	secondLease, acquired := dev.acquireInputProducer()
	require.True(t, acquired)
	defer dev.releaseInputProducer()
	second := InputState{Buttons: ButtonB, LX: -222}
	// The successor snapshot is captured after producer retirement. A snapshot
	// timestamped before that lifecycle boundary must remain ineligible for
	// resynchronization, even in compatibility mode.
	secondReceivedAt := time.Now()
	secondLease, disposition = dev.publishInputStateWithLease(secondLease,
		second, secondReceivedAt)
	require.Equal(t, inputpresentation.FixedReportPublishRejectedNeutralPending,
		disposition)
	require.True(t, secondLease.Valid())

	var report [20]byte
	neutral := dev.ClaimInputPresentation(report[:],
		secondReceivedAt.Add(time.Millisecond))
	require.True(t, neutral.Valid())
	require.Equal(t, NewInputState().BuildReport(), report[:])
	require.True(t, dev.CanAdmitInputPresentation(neutral,
		secondReceivedAt.Add(time.Millisecond)))
	require.True(t, dev.ResolveInputPresentation(neutral,
		inputpresentation.OutcomeCommit,
		secondReceivedAt.Add(time.Millisecond)))

	fresh := dev.ClaimInputPresentation(report[:],
		secondReceivedAt.Add(2*time.Millisecond))
	require.True(t, fresh.Valid())
	require.Equal(t, second.BuildReport(), report[:])
	require.NotEqual(t, first.BuildReport(), report[:])
}

func TestCompatibilityToRawHandoffFencesQueuedState(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)
	compatibilityLease := dev.input.ProducerLease()
	compatibilityEpoch := dev.InputSchedulerSnapshot().ProducerEpoch
	compatibilityState := InputState{Buttons: ButtonA, LX: 111}
	require.True(t, dev.UpdateInputState(compatibilityState))
	base := time.Now()
	var report [20]byte
	compatibilityClaim := dev.ClaimInputPresentation(report[:], base)
	require.True(t, compatibilityClaim.Valid())
	require.Equal(t, compatibilityState.BuildReport(), report[:])
	generation := dev.InputPresentationGeneration()

	rawLease, acquired := dev.acquireInputProducer()
	require.True(t, acquired)
	require.True(t, rawLease.Valid())
	require.NotEqual(t, compatibilityLease, rawLease)
	afterAcquire := dev.InputSchedulerSnapshot()
	require.Equal(t, generation, afterAcquire.Generation)
	require.NotEqual(t, compatibilityEpoch, afterAcquire.ProducerEpoch)
	require.True(t, afterAcquire.MandatoryNeutral)
	require.Equal(t, 1, afterAcquire.TransitionDepth)
	second, acquired := dev.acquireInputProducer()
	require.False(t, acquired)
	require.False(t, second.Valid())
	afterRejectedAcquire := dev.InputSchedulerSnapshot()
	require.Equal(t, afterAcquire.Generation,
		afterRejectedAcquire.Generation)
	require.Equal(t, afterAcquire.ProducerEpoch,
		afterRejectedAcquire.ProducerEpoch)
	require.Equal(t, afterAcquire.TransitionDepth,
		afterRejectedAcquire.TransitionDepth)
	require.False(t, dev.CanAdmitInputPresentation(
		compatibilityClaim, base.Add(time.Millisecond)))
	require.False(t, dev.ResolveInputPresentation(compatibilityClaim,
		inputpresentation.OutcomeCommit, base.Add(time.Millisecond)))

	staleAt := time.Now()
	_, disposition := dev.publishInputStateWithLease(
		compatibilityLease, compatibilityState, staleAt)
	require.Equal(t,
		inputpresentation.FixedReportPublishRejectedStaleProducer,
		disposition)
	require.False(t, dev.hasPendingResync,
		"retired compatibility state became a raw resynchronization candidate")
	rawState := InputState{Buttons: ButtonB, LX: -222}
	rawAt := staleAt.Add(time.Millisecond)
	rawLease, disposition = dev.publishInputStateWithLease(
		rawLease, rawState, rawAt)
	require.Equal(t,
		inputpresentation.FixedReportPublishRejectedNeutralPending,
		disposition)

	neutral := dev.ClaimInputPresentation(report[:],
		rawAt.Add(time.Millisecond))
	require.True(t, neutral.Valid())
	require.Equal(t, NewInputState().BuildReport(), report[:])
	require.True(t, dev.CanAdmitInputPresentation(neutral,
		rawAt.Add(time.Millisecond)))
	require.True(t, dev.ResolveInputPresentation(neutral,
		inputpresentation.OutcomeCommit, rawAt.Add(time.Millisecond)))
	fresh := dev.ClaimInputPresentation(report[:],
		rawAt.Add(2*time.Millisecond))
	require.True(t, fresh.Valid())
	require.False(t, fresh.Ordered)
	require.Equal(t, rawState.BuildReport(), report[:])
	require.NotEqual(t, compatibilityState.BuildReport(), report[:])
	require.True(t, dev.CanAdmitInputPresentation(fresh,
		rawAt.Add(2*time.Millisecond)))
	require.True(t, dev.ResolveInputPresentation(fresh,
		inputpresentation.OutcomeCommit, rawAt.Add(2*time.Millisecond)))

	dev.releaseInputProducer()
	afterRelease := dev.InputSchedulerSnapshot()
	require.Equal(t, generation, afterRelease.Generation)
	require.True(t, afterRelease.MandatoryNeutral)
	require.Equal(t, 1, afterRelease.TransitionDepth)
}

func TestCompatibilityAndRawAcquisitionLinearize(t *testing.T) {
	for iteration := 0; iteration < 32; iteration++ {
		dev, err := New(nil)
		require.NoError(t, err)
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		var compatibilityAccepted bool
		var rawLease inputpresentation.FixedReportProducerLease
		var rawAcquired bool
		go func() {
			defer wait.Done()
			<-start
			compatibilityAccepted = dev.UpdateInputState(
				InputState{Buttons: ButtonA})
		}()
		go func() {
			defer wait.Done()
			<-start
			rawLease, rawAcquired = dev.acquireInputProducer()
		}()
		close(start)
		wait.Wait()
		require.True(t, rawAcquired, "iteration %d", iteration)
		require.True(t, rawLease.Valid(), "iteration %d", iteration)
		snapshot := dev.InputSchedulerSnapshot()
		require.Equal(t, compatibilityAccepted, snapshot.MandatoryNeutral,
			"iteration %d: %+v", iteration, snapshot)
		dev.releaseInputProducer()
		afterRelease := dev.InputSchedulerSnapshot()
		require.Equal(t, snapshot.Generation, afterRelease.Generation,
			"iteration %d", iteration)
		require.True(t, afterRelease.MandatoryNeutral, "iteration %d", iteration)
		require.Equal(t, 1, afterRelease.TransitionDepth,
			"iteration %d: %+v", iteration, afterRelease)
	}
}

func TestPreResetSampleIsNotRepublishedUnderSuccessorLease(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)
	lease, acquired := dev.acquireInputProducer()
	require.True(t, acquired)
	defer dev.releaseInputProducer()

	base := time.Now().Add(-time.Second)
	beforeReset := InputState{Buttons: ButtonA, LX: 111}
	lease, disposition := dev.publishInputStateWithLease(
		lease, beforeReset, base)
	require.True(t, disposition.Accepted())
	retiringGeneration := dev.InputPresentationGeneration()
	require.True(t, dev.RetireInputPresentationGeneration(
		retiringGeneration, base.Add(time.Millisecond)))

	// This complete sample was received before the lifecycle boundary but its
	// callback resumed afterward with the retired lease. It must be discarded,
	// not retried under the current epoch.
	capturedBeforeReset := InputState{Buttons: ButtonB, LX: -222}
	successorLease, disposition := dev.publishInputStateWithLease(
		lease, capturedBeforeReset, base.Add(500*time.Microsecond))
	require.Equal(t,
		inputpresentation.FixedReportPublishRejectedStaleProducer,
		disposition)
	require.True(t, successorLease.Valid())

	var report [20]byte
	claim := dev.ClaimInputPresentation(report[:], base.Add(2*time.Millisecond))
	require.True(t, claim.Valid())
	require.Equal(t, beforeReset.BuildReport(), report[:],
		"a pre-reset callback contaminated the successor generation")
}

func TestUSBRetirementPurgesStagedResynchronizationAcrossLaterFault(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)
	lease, acquired := dev.acquireInputProducer()
	require.True(t, acquired)
	defer dev.releaseInputProducer()

	base := time.Now()
	for index := 0; index < inputpresentation.FixedReportTransitionCapacity; index++ {
		state := InputState{}
		if index%2 == 0 {
			state.Buttons = ButtonA
		}
		lease, _ = dev.publishInputStateWithLease(lease, state,
			base.Add(time.Duration(index)*time.Microsecond))
	}
	oldRecovery := InputState{Buttons: ButtonB, LX: -111}
	oldRecoveryAt := base.Add(time.Hour)
	lease, disposition := dev.publishInputStateWithLease(lease, oldRecovery,
		oldRecoveryAt)
	require.Equal(t, inputpresentation.FixedReportPublishFaultedOverflow,
		disposition)
	require.True(t, dev.hasPendingResync)

	retiringGeneration := dev.InputPresentationGeneration()
	require.True(t, dev.RetireInputPresentationGeneration(
		retiringGeneration, base.Add(100*time.Microsecond)))
	require.False(t, dev.hasPendingResync,
		"USB retirement retained an old-generation recovery snapshot")

	// The first callback carrying the retired lease may adopt the successor,
	// but its pre-boundary state is deliberately discarded.
	lease, disposition = dev.publishInputStateWithLease(lease, InputState{},
		base.Add(101*time.Microsecond))
	require.Equal(t,
		inputpresentation.FixedReportPublishRejectedStaleProducer,
		disposition)
	var report [20]byte
	successorCurrent := dev.ClaimInputPresentation(report[:],
		base.Add(150*time.Microsecond))
	require.True(t, successorCurrent.Valid())
	require.Equal(t, NewInputState().BuildReport(), report[:])
	require.True(t, dev.CanAdmitInputPresentation(successorCurrent,
		base.Add(151*time.Microsecond)))
	require.True(t, dev.ResolveInputPresentation(successorCurrent,
		inputpresentation.OutcomeCommit,
		base.Add(152*time.Microsecond)))

	for index := 0; index < inputpresentation.FixedReportTransitionCapacity; index++ {
		state := InputState{}
		if index%2 == 0 {
			state.Buttons = ButtonA
		}
		lease, disposition = dev.publishInputStateWithLease(lease, state,
			base.Add(time.Duration(200+index)*time.Microsecond))
		require.True(t, disposition.Accepted(), "edge %d: %d", index,
			disposition)
	}
	newRecovery := InputState{Buttons: ButtonA, LX: 222}
	newRecoveryAt := base.Add(300 * time.Microsecond)
	lease, disposition = dev.publishInputStateWithLease(lease, newRecovery,
		newRecoveryAt)
	require.Equal(t, inputpresentation.FixedReportPublishFaultedOverflow,
		disposition)

	neutral := dev.ClaimInputPresentation(report[:],
		base.Add(301*time.Microsecond))
	require.True(t, neutral.Valid())
	require.Equal(t, NewInputState().BuildReport(), report[:])
	require.True(t, dev.CanAdmitInputPresentation(neutral,
		base.Add(302*time.Microsecond)))
	require.True(t, dev.ResolveInputPresentation(neutral,
		inputpresentation.OutcomeCommit,
		base.Add(303*time.Microsecond)))

	fresh := dev.ClaimInputPresentation(report[:],
		base.Add(304*time.Microsecond))
	require.True(t, fresh.Valid())
	require.Equal(t, newRecovery.BuildReport(), report[:],
		"an old-generation staged snapshot crossed the USB generation fence")
	require.NotEqual(t, oldRecovery.BuildReport(), report[:])
}

func TestNewestStagedSnapshotWinsNeutralCommitResynchronization(t *testing.T) {
	dev, err := New(&devicebase.CreateOptions{DeviceSpecific: `{"maximumOrderedAgeMilliseconds":1000}`})
	require.NoError(t, err)
	lease, acquired := dev.acquireInputProducer()
	require.True(t, acquired)
	defer dev.releaseInputProducer()

	base := time.Now().Add(-time.Second)
	for index := 0; index < inputpresentation.FixedReportTransitionCapacity; index++ {
		state := InputState{}
		if index%2 == 0 {
			state.Buttons = ButtonA
		}
		lease, _ = dev.publishInputStateWithLease(lease, state,
			base.Add(time.Duration(index)*time.Microsecond))
	}
	initial := InputState{Buttons: ButtonA, LX: 1}
	lease, disposition := dev.publishInputStateWithLease(lease, initial,
		base.Add(100*time.Microsecond))
	require.Equal(t, inputpresentation.FixedReportPublishFaultedOverflow,
		disposition)

	newest := InputState{Buttons: ButtonB, LX: 200}
	older := InputState{Buttons: ButtonA, LX: 150}
	dev.stageInputResynchronization(newest, base.Add(200*time.Microsecond))
	dev.stageInputResynchronization(older, base.Add(150*time.Microsecond))

	var report [20]byte
	neutral := dev.ClaimInputPresentation(report[:],
		base.Add(201*time.Microsecond))
	require.True(t, neutral.Valid())
	require.True(t, dev.CanAdmitInputPresentation(neutral,
		base.Add(202*time.Microsecond)))
	require.True(t, dev.ResolveInputPresentation(neutral,
		inputpresentation.OutcomeCommit,
		base.Add(203*time.Microsecond)))

	fresh := dev.ClaimInputPresentation(report[:],
		base.Add(204*time.Microsecond))
	require.True(t, fresh.Valid())
	require.Equal(t, newest.BuildReport(), report[:],
		"a delayed older callback cleared or replaced the newest staged state")
}

func TestCompatibilityProducerCannotInterleaveWithRawStream(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)
	_, acquired := dev.acquireInputProducer()
	require.True(t, acquired)
	require.False(t, dev.UpdateInputState(InputState{Buttons: ButtonA}),
		"the compatibility producer interleaved with an active raw stream")
	dev.releaseInputProducer()
}

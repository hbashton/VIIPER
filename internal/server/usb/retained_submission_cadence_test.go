package usb

import (
	"context"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
	"github.com/stretchr/testify/require"
)

func newCadencedTestScheduler(t *testing.T, owner *scriptedRetainedOwner) *retainedSubmissionScheduler {
	t.Helper()
	scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
	require.NoError(t, scheduler.configureEndpointCadence(retainedImportTransportDescriptor(owner.limits), 0))
	return scheduler
}

func TestRetainedEndpointCadenceParksFloodWithoutBlockingOtherLanes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner := newScriptedRetainedOwner(8)
		for i := 0; i < 8; i++ {
			owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultData, data: []byte{byte(i)}})
		}
		owner.appendPlan(retainedusb.LaneInterruptOut, retainedOwnerPlan{result: retainedusb.ResultSuccess, actualLength: 1})
		owner.appendPlan(retainedusb.LaneControl, retainedOwnerPlan{result: retainedusb.ResultSuccess})
		scheduler := newCadencedTestScheduler(t, owner)
		base := time.Now()
		for seq := uint32(1); seq <= 8; seq++ {
			require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(seq)))
		}
		progressed, _, err := scheduler.serviceRound(base)
		require.NoError(t, err)
		require.True(t, progressed, "first service must be immediate")
		for i := 0; i < 1000; i++ {
			owner.advanceReadiness()
			progressed, next, err := scheduler.serviceRound(time.Now())
			require.NoError(t, err)
			require.False(t, progressed)
			require.Equal(t, base.Add(4*time.Millisecond), next)
		}
		require.EqualValues(t, 1, owner.prepareCalls.Load(), "readiness must not bypass descriptor cadence")
		require.NoError(t, scheduler.enqueue(retainedInterruptOutEnvelope(9, []byte{1})))
		require.NoError(t, scheduler.enqueue(retainedControlOutEnvelope(10, nil)))
		for i := 0; i < 2; i++ {
			progressed, _, err := scheduler.serviceRound(time.Now())
			require.NoError(t, err)
			require.True(t, progressed, "waiting IN must not delay another endpoint or EP0")
		}
		time.Sleep(4*time.Millisecond - time.Nanosecond)
		progressed, _, err = scheduler.serviceRound(time.Now())
		require.NoError(t, err)
		require.False(t, progressed)
		time.Sleep(time.Nanosecond)
		progressed, _, err = scheduler.serviceRound(time.Now())
		require.NoError(t, err)
		require.True(t, progressed)
		require.EqualValues(t, 4, owner.prepareCalls.Load())
	})
}

func TestRetainedEndpointCadenceBoundsOneSecondOfImmediateHostRefills(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner := newScriptedRetainedOwner(1)
		for i := 0; i < 251; i++ {
			owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultData, data: []byte{1}})
		}
		scheduler := newCadencedTestScheduler(t, owner)
		seq := uint32(1)
		require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(seq)))
		for i := 0; i < 10_000; i++ {
			owner.advanceReadiness()
			progressed, _, err := scheduler.serviceRound(time.Now())
			require.NoError(t, err)
			if progressed {
				seq++
				require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(seq)))
			}
			time.Sleep(100 * time.Microsecond)
		}
		require.EqualValues(t, 250, owner.prepareCalls.Load())
		require.Len(t, owner.completions, 250)
	})
}

func TestRetainedEndpointCadenceReanchorsAfterSerializerDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner := newScriptedRetainedOwner(2)
		for i := 0; i < 2; i++ {
			owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultData, data: []byte{1}})
		}
		scheduler := newCadencedTestScheduler(t, owner)
		require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(1)))
		require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(2)))
		base := time.Now()
		// Reproduce an attempt timestamp captured before a delayed serializer
		// admission. Mutex waits are not durably blocked in testing/synctest;
		// retain the stale timestamp explicitly instead of freezing its clock.
		time.Sleep(20 * time.Millisecond)
		progressed, _, err := scheduler.serviceRound(base)
		require.NoError(t, err)
		require.True(t, progressed)
		progressed, next, err := scheduler.serviceRound(time.Now())
		require.NoError(t, err)
		require.False(t, progressed, "expired service slots must not burst")
		require.Equal(t, base.Add(24*time.Millisecond), next)
		time.Sleep(4 * time.Millisecond)
		progressed, _, err = scheduler.serviceRound(time.Now())
		require.NoError(t, err)
		require.True(t, progressed)
	})
}

func TestRetainedEndpointCadencePendingWakeDoesNotAddAnotherInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner := newScriptedRetainedOwner(1)
		owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultPending, currentEpoch: true})
		owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultData, data: []byte{1}})
		scheduler := newCadencedTestScheduler(t, owner)
		require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(1)))
		progressed, next, err := scheduler.serviceRound(time.Now())
		require.NoError(t, err)
		require.False(t, progressed)
		require.True(t, next.IsZero())
		time.Sleep(100 * time.Microsecond)
		owner.advanceReadiness()
		progressed, _, err = scheduler.serviceRound(time.Now())
		require.NoError(t, err)
		require.True(t, progressed, "new state at an unused opportunity is immediately eligible")
	})
}

func TestRetainedEndpointCadenceUsesExactDescriptorAndScopedReset(t *testing.T) {
	owner := newScriptedRetainedOwner(1)
	scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
	descriptor := retainedImportTransportDescriptor(owner.limits)
	descriptor.Device.Speed = 3
	descriptor.Interfaces[0].Endpoints[0].BInterval = 5
	descriptor.Interfaces[0].Endpoints[1].BInterval = 4
	require.NoError(t, scheduler.configureEndpointCadence(descriptor, 0))
	in, _ := retainedusb.LaneInterruptIn.Index()
	out, _ := retainedusb.LaneInterruptOut.Index()
	require.Equal(t, time.Millisecond, scheduler.endpointIntervals[in])
	require.Equal(t, 2*time.Millisecond, scheduler.endpointIntervals[out])
	descriptor.Interfaces[0].Endpoints[1].BInterval = 1
	require.Equal(t, time.Millisecond, scheduler.endpointIntervals[in], "descriptor is snapshotted")
	require.Error(t, scheduler.configureEndpointCadence(descriptor, 0), "cold configuration is one-shot")
	base := time.Now()
	scheduler.advanceEndpointCadenceLocked(in, base)
	scheduler.advanceEndpointCadenceLocked(out, base)
	scheduler.applyControlLifecycleRoutingLocked(controlLifecycleSetup{kind: controlLifecycleClearEndpointHalt, endpointAddress: owner.limits.InterruptInRoute.EndpointAddress})
	require.True(t, scheduler.endpointNextService[in].IsZero())
	require.Equal(t, base.Add(2*time.Millisecond), scheduler.endpointNextService[out])
	scheduler.applyControlLifecycleRoutingLocked(controlLifecycleSetup{kind: controlLifecycleSetConfiguration})
	require.Equal(t, [3]time.Time{}, scheduler.endpointNextService)
	allocs := testing.AllocsPerRun(1000, func() { scheduler.advanceEndpointCadenceLocked(in, base) })
	require.Zero(t, allocs)
}

func TestRetainedEndpointCadenceStartedWorkerUsesTimerAndReadiness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner := newScriptedRetainedOwner(8)
		for i := 0; i < 8; i++ {
			owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultData, data: []byte{1}})
		}
		scheduler := newCadencedTestScheduler(t, owner)
		for seq := uint32(1); seq <= 8; seq++ {
			require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(seq)))
		}
		// Exercise the production arbitration loop and reusable timer, not only
		// manual service rounds. The test owner requires no import authority.
		scheduler.started = true
		go scheduler.run()
		synctest.Wait()
		require.EqualValues(t, 1, owner.prepareCalls.Load())
		for tick := 1; tick <= 28; tick++ {
			time.Sleep(time.Millisecond)
			// The second completion must come from the timer alone. Later
			// completions also exercise a readiness event before every slot.
			if tick > 4 {
				owner.advanceReadiness()
			}
			synctest.Wait()
			require.EqualValues(t, 1+tick/4, owner.prepareCalls.Load())
		}
		require.NoError(t, scheduler.close())
		synctest.Wait()
	})
}

func TestRetainedEndpointCadenceUnlinkAndCloseWhileParked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner := newScriptedRetainedOwner(4)
		for i := 0; i < 4; i++ {
			owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultData, data: []byte{1}})
		}
		scheduler := newCadencedTestScheduler(t, owner)
		for seq := uint32(1); seq <= 4; seq++ {
			require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(seq)))
		}
		scheduler.started = true
		go scheduler.run()
		synctest.Wait()
		require.EqualValues(t, 1, owner.prepareCalls.Load())
		unlinked, err := scheduler.unlink(2, time.Now())
		require.NoError(t, err)
		require.True(t, unlinked)
		time.Sleep(4 * time.Millisecond)
		synctest.Wait()
		require.EqualValues(t, 2, owner.prepareCalls.Load())
		require.EqualValues(t, 3, owner.requests[1].request.Sequence)
		require.NoError(t, scheduler.close())
		time.Sleep(8 * time.Millisecond)
		synctest.Wait()
		require.EqualValues(t, 2, owner.prepareCalls.Load(), "closed worker must not service the remaining request")
	})
}

func TestRetainedEndpointCadenceWarmedTerminalPathAllocatesZero(t *testing.T) {
	owner := &allocationRetainedOwner{identity: 99, limits: retainedTestLimits(1), ready: make(chan struct{}, 1)}
	scheduler, err := newRetainedSubmissionScheduler(context.Background(), 23, owner, newResponseWriter(io.Discard, nil), false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = scheduler.close() })
	require.NoError(t, scheduler.configureEndpointCadence(retainedImportTransportDescriptor(owner.limits), 0))
	in, _ := retainedusb.LaneInterruptIn.Index()
	var sequence uint32
	allocations := testing.AllocsPerRun(1000, func() {
		// Represent an already-due opportunity without sleeping in an allocation
		// measurement. Selection, descriptor cadence advancement, encode/copy,
		// response write/flush, completion and removal all remain real.
		scheduler.endpointNextService[in] = time.Time{}
		sequence++
		if err := scheduler.enqueue(retainedInterruptInEnvelope(sequence)); err != nil {
			panic(err)
		}
		progressed, _, err := scheduler.serviceRound(time.Now())
		if err != nil || !progressed {
			panic("cadenced retained allocation run did not complete")
		}
	})
	require.Zero(t, allocations)
}

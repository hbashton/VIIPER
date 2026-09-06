package usb

import (
	"encoding/binary"
	"io"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/require"
)

func forEachRetainedInputServicePolicy(t *testing.T, test func(*testing.T, int, time.Duration)) {
	t.Helper()
	for _, milliseconds := range []int{0, 4, 2, 1} {
		t.Run(strconv.Itoa(milliseconds)+"ms", func(t *testing.T) {
			interval := time.Duration(milliseconds) * time.Millisecond
			if milliseconds == 0 {
				interval = 4 * time.Millisecond
			}
			test(t, milliseconds, interval)
		})
	}
}

func newInputServicePolicyTestScheduler(t *testing.T, owner *scriptedRetainedOwner, milliseconds int) *retainedSubmissionScheduler {
	t.Helper()
	scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
	require.NoError(t, scheduler.configureEndpointCadence(retainedImportTransportDescriptor(owner.limits), milliseconds))
	return scheduler
}

func TestRetainedInputServicePolicyCLIAndEnvironment(t *testing.T) {
	for _, source := range []string{"cli", "environment"} {
		t.Run(source, func(t *testing.T) {
			for _, milliseconds := range []int{0, 1, 2, 4, -1, 3, 5, 1000} {
				t.Run(strconv.Itoa(milliseconds), func(t *testing.T) {
					var cli struct {
						USB ServerConfig `embed:"" prefix:"usb."`
					}
					value := strconv.Itoa(milliseconds)
					var args []string
					if source == "environment" {
						t.Setenv("VIIPER_USB_RETAINED_INPUT_SERVICE_MS", value)
					} else {
						t.Setenv("VIIPER_USB_RETAINED_INPUT_SERVICE_MS", "0")
						args = []string{"--usb.retained-input-service-ms=" + value}
					}
					parser, err := kong.New(&cli)
					require.NoError(t, err)
					_, err = parser.Parse(args)
					_, policyErr := retainedInputServiceInterval(milliseconds)
					if policyErr != nil {
						require.Error(t, err, "invalid policy must fail CLI/environment validation")
						return
					}
					require.NoError(t, err)
					require.Equal(t, milliseconds, cli.USB.RetainedInputServiceMS)
				})
			}
		})
	}
}

func TestRetainedInputServicePolicyColdSnapshotAndScopedReset(t *testing.T) {
	forEachRetainedInputServicePolicy(t, func(t *testing.T, milliseconds int, interval time.Duration) {
		owner := newScriptedRetainedOwner(1)
		scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
		descriptor := retainedImportTransportDescriptor(owner.limits)
		expectedDescriptor := retainedImportTransportDescriptor(owner.limits)
		require.NoError(t, scheduler.configureEndpointCadence(descriptor, milliseconds))
		require.Equal(t, expectedDescriptor, descriptor, "including speed and both bInterval fields")
		in, _ := retainedusb.LaneInterruptIn.Index()
		out, _ := retainedusb.LaneInterruptOut.Index()
		control, _ := retainedusb.LaneControl.Index()
		require.Equal(t, interval, scheduler.endpointIntervals[in])
		require.Equal(t, 4*time.Millisecond, scheduler.endpointIntervals[out])
		require.Zero(t, scheduler.endpointIntervals[control])
		descriptor.Interfaces[0].Endpoints[1].BInterval = 20
		require.Equal(t, interval, scheduler.endpointIntervals[in], "no hot descriptor lookup")
		require.ErrorIs(t, scheduler.configureEndpointCadence(descriptor, 1), errRetainedSubmissionInvalidRequest)

		base := time.Now()
		scheduler.advanceEndpointCadenceLocked(in, base)
		scheduler.advanceEndpointCadenceLocked(out, base)
		scheduler.applyControlLifecycleRoutingLocked(controlLifecycleSetup{
			kind: controlLifecycleClearEndpointHalt, endpointAddress: owner.limits.InterruptOutRoute.EndpointAddress,
		})
		require.Equal(t, base.Add(interval), scheduler.endpointNextService[in], "OUT clear-halt cannot reset IN")
		require.True(t, scheduler.endpointNextService[out].IsZero())
		scheduler.advanceEndpointCadenceLocked(out, base)
		scheduler.applyControlLifecycleRoutingLocked(controlLifecycleSetup{
			kind: controlLifecycleClearEndpointHalt, endpointAddress: owner.limits.InterruptInRoute.EndpointAddress,
		})
		require.True(t, scheduler.endpointNextService[in].IsZero())
		require.Equal(t, base.Add(4*time.Millisecond), scheduler.endpointNextService[out])
		scheduler.advanceEndpointCadenceLocked(in, base)
		scheduler.applyControlLifecycleRoutingLocked(controlLifecycleSetup{kind: controlLifecycleSetConfiguration})
		require.Equal(t, [3]time.Time{}, scheduler.endpointNextService)
		require.Equal(t, interval, scheduler.endpointIntervals[in], "reset preserves the immutable policy")
		require.Zero(t, testing.AllocsPerRun(1000, func() { scheduler.advanceEndpointCadenceLocked(in, base) }))
	})
}

func TestRetainedInputServicePolicyBoundsOneSecondOfImmediateHostRefills(t *testing.T) {
	forEachRetainedInputServicePolicy(t, func(t *testing.T, milliseconds int, interval time.Duration) {
		synctest.Test(t, func(t *testing.T) {
			owner := newScriptedRetainedOwner(1)
			expected := int(time.Second / interval)
			for i := 0; i <= expected; i++ {
				owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultData, data: []byte{byte(i)}})
			}
			scheduler := newInputServicePolicyTestScheduler(t, owner, milliseconds)
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
			require.EqualValues(t, expected, owner.prepareCalls.Load())
			require.Len(t, owner.completions, expected)
		})
	})
}

func TestRetainedInputServicePolicyUsesExistingTimerAndCancellationGates(t *testing.T) {
	forEachRetainedInputServicePolicy(t, func(t *testing.T, milliseconds int, interval time.Duration) {
		synctest.Test(t, func(t *testing.T) {
			owner := newScriptedRetainedOwner(8)
			for i := 0; i < 8; i++ {
				owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultData, data: []byte{1}})
			}
			scheduler := newInputServicePolicyTestScheduler(t, owner, milliseconds)
			for seq := uint32(1); seq <= 8; seq++ {
				require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(seq)))
			}
			scheduler.started = true
			go scheduler.run()
			synctest.Wait()
			require.EqualValues(t, 1, owner.prepareCalls.Load())
			time.Sleep(interval - time.Nanosecond)
			owner.advanceReadiness()
			synctest.Wait()
			require.EqualValues(t, 1, owner.prepareCalls.Load(), "readiness cannot bypass service time")
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			require.EqualValues(t, 2, owner.prepareCalls.Load(), "existing timer alone releases the next due request")
			unlinked, err := scheduler.unlink(3, time.Now())
			require.NoError(t, err)
			require.True(t, unlinked)
			time.Sleep(interval)
			synctest.Wait()
			require.EqualValues(t, 3, owner.prepareCalls.Load())
			require.EqualValues(t, 4, owner.requests[2].request.Sequence)
			require.NoError(t, scheduler.close())
			time.Sleep(2 * interval)
			owner.advanceReadiness()
			synctest.Wait()
			require.EqualValues(t, 3, owner.prepareCalls.Load(), "no service after close, including readiness/timer wakes")
		})
	})
}

func TestRetainedInputServicePolicyDoesNotConsumePendingOpportunity(t *testing.T) {
	forEachRetainedInputServicePolicy(t, func(t *testing.T, milliseconds int, _ time.Duration) {
		synctest.Test(t, func(t *testing.T) {
			owner := newScriptedRetainedOwner(1)
			owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultPending, currentEpoch: true})
			owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultData, data: []byte{1}})
			scheduler := newInputServicePolicyTestScheduler(t, owner, milliseconds)
			require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(1)))
			progressed, next, err := scheduler.serviceRound(time.Now())
			require.NoError(t, err)
			require.False(t, progressed)
			require.True(t, next.IsZero())
			time.Sleep(100 * time.Microsecond)
			owner.advanceReadiness()
			progressed, _, err = scheduler.serviceRound(time.Now())
			require.NoError(t, err)
			require.True(t, progressed, "the unused service opportunity is immediately available on readiness")
		})
	})
}

func TestRetainedInputServicePolicyRejectsInvalidColdConfiguration(t *testing.T) {
	for _, milliseconds := range []int{-1, 3, 5, 1000, int(^uint(0) >> 1)} {
		t.Run(strconv.Itoa(milliseconds), func(t *testing.T) {
			owner := newScriptedRetainedOwner(1)
			scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
			descriptor := retainedImportTransportDescriptor(owner.limits)
			require.ErrorIs(t, scheduler.configureEndpointCadence(descriptor, milliseconds), errRetainedInputServicePolicy)
			require.False(t, scheduler.endpointCadenceConfigured)
			require.Equal(t, [3]time.Duration{}, scheduler.endpointIntervals)
			require.Zero(t, owner.prepareCalls.Load())
		})
	}
}

func TestRetainedInputServicePolicyRejectsImportBeforeOwnerBind(t *testing.T) {
	for _, milliseconds := range []int{-1, 3, 5, 1000} {
		t.Run(strconv.Itoa(milliseconds), func(t *testing.T) {
			owner, hot := newRetainedImportTransportOwner()
			_, client, result := startRetainedImportTransportServerWithConfig(t, 968, owner, ServerConfig{
				ConnectionTimeout: time.Second, RetainedImportAuthorityID: 0x9681, RetainedInputServiceMS: milliseconds,
			})
			require.NoError(t, client.SetDeadline(time.Now().Add(2*time.Second)))
			writeRetainedImportRequest(t, client, "968-1")
			var reply [8]byte
			require.NoError(t, usbip.ReadExactly(client, reply[:]))
			require.Equal(t, uint16(usbip.OpRepImport), binary.BigEndian.Uint16(reply[2:4]))
			require.NotZero(t, binary.BigEndian.Uint32(reply[4:8]))
			require.ErrorIs(t, <-result, errRetainedInputServicePolicy)
			require.Zero(t, owner.bindCalls.Load())
			require.Zero(t, hot.prepareCalls.Load())
		})
	}
}

func TestRetainedInputServicePolicyServerCopiesConstructionConfig(t *testing.T) {
	forEachRetainedInputServicePolicy(t, func(t *testing.T, milliseconds int, _ time.Duration) {
		config := ServerConfig{RetainedInputServiceMS: milliseconds}
		server := New(config, nil, nil)
		config.RetainedInputServiceMS = 3
		require.Equal(t, milliseconds, server.config.RetainedInputServiceMS,
			"caller changes cannot alter the constructed %dms policy", milliseconds)
	})
}

func TestRetainedInputServicePolicyProductionImportUsesConfiguredCadence(t *testing.T) {
	forEachRetainedInputServicePolicy(t, func(t *testing.T, milliseconds int, interval time.Duration) {
		synctest.Test(t, func(t *testing.T) {
			owner, hot := newRetainedImportTransportOwner()
			hot.appendPlan(retainedusb.LaneControl, retainedOwnerPlan{result: retainedusb.ResultSuccess})
			hot.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultData, data: []byte{1}})
			hot.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultData, data: []byte{2}})
			server, client, result := startRetainedImportTransportServerWithConfig(t, 969, owner, ServerConfig{
				ConnectionTimeout: time.Second, RetainedImportAuthorityID: 0x9691, RetainedInputServiceMS: milliseconds,
			})
			require.NoError(t, client.SetDeadline(time.Now().Add(time.Second)))
			writeRetainedImportRequest(t, client, "969-1")
			readSuccessfulRetainedImport(t, client)
			setConfiguration := [8]byte{usbReqTypeStandardToDevice, usbReqSetConfiguration, 1}
			writeRetainedSubmit(t, client, 100, usbip.DirOut, 0, 0, setConfiguration, nil)
			_, status, _, _ := readRetainedSubmitResponseForDirection(t, client, usbip.DirOut)
			require.Zero(t, status)
			writeRetainedSubmit(t, client, 101, usbip.DirIn, 1, 64, [8]byte{}, nil)
			sequence, status, actual, data := readRetainedSubmitResponse(t, client)
			require.EqualValues(t, 101, sequence)
			require.Zero(t, status)
			require.EqualValues(t, 1, actual)
			require.Equal(t, []byte{1}, data)
			synctest.Wait()
			writeRetainedSubmit(t, client, 102, usbip.DirIn, 1, 64, [8]byte{}, nil)
			var response [retSubmitHeaderSize + 1]byte
			readResult := make(chan error, 1)
			go func() { readResult <- usbip.ReadExactly(client, response[:]) }()
			synctest.Wait()
			require.EqualValues(t, 2, hot.prepareCalls.Load(), "EP0 plus the first IN only")
			time.Sleep(interval - time.Nanosecond)
			synctest.Wait()
			require.EqualValues(t, 2, hot.prepareCalls.Load(), "refill cannot bypass the imported policy")
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			require.NoError(t, <-readResult)
			require.EqualValues(t, 3, hot.prepareCalls.Load())
			require.EqualValues(t, 102, binary.BigEndian.Uint32(response[4:8]))
			require.Zero(t, binary.BigEndian.Uint32(response[20:24]))
			require.EqualValues(t, 1, binary.BigEndian.Uint32(response[24:28]))
			require.EqualValues(t, 2, response[retSubmitHeaderSize])
			require.NoError(t, server.Close())
			require.NoError(t, <-result)
			require.EqualValues(t, 1, owner.bindCalls.Load())
			require.EqualValues(t, 1, owner.drainCalls.Load())
			require.EqualValues(t, 1, owner.disconnectCalls.Load())
		})
	})
}

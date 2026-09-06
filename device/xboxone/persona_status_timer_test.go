package xboxone

import (
	"bytes"
	"errors"
	"testing"
)

func requirePersonaPeriodicDeadline(t *testing.T, engine *ControllerPersonaEngine, want uint64) {
	t.Helper()
	before := engine.statusTimer
	due, scheduled := engine.NextPeriodicStatusDeadlineMilliseconds()
	if due != want || scheduled != (want != 0) {
		t.Fatalf("status deadline = (%d, %t), want %d", due, scheduled, want)
	}
	if engine.statusTimer != before {
		t.Fatal("reading the deadline mutated the timer")
	}
}

func requirePersonaPeriodicClaim(t *testing.T, engine *ControllerPersonaEngine, nowMS uint64) ControllerPersonaClaim {
	t.Helper()
	claim, present, err := engine.ClaimPoll(nowMS)
	if err != nil || !present || claim.Action() != ControllerPersonaSendCurrentStatus ||
		claim.Size() != ExtendedStatusNoEventsMessageSize {
		t.Fatalf("periodic status = (%+v, %t, %v)", claim, present, err)
	}
	return claim
}

func TestControllerPersonaPeriodicStatusCadenceAndSequencePools(t *testing.T) {
	engine, _ := makePersonaActive(t, []byte{1})
	// The helper delivered START's status at 3 ms, then initial input and
	// Permit at 5/7 ms. The timing anchor is status delivery, not Permit.
	for tick := uint64(1); tick <= 10; tick++ {
		due := 3 + tick*1000
		requirePersonaPeriodicDeadline(t, engine, due)
		if claim, present, err := engine.ClaimPoll(due - 1); err != nil || present || claim.Valid() {
			t.Fatalf("early status at %d: (%+v, %t, %v)", due-1, claim, present, err)
		}
		before := engine.Snapshot()
		claim := requirePersonaPeriodicClaim(t, engine, due)
		if claim.Sequence() != uint8(tick+2) || engine.Snapshot().GlobalSequence != before.GlobalSequence {
			t.Fatalf("selection advanced/wrong Global sequence: claim=%+v snapshot=%+v", claim, engine.Snapshot())
		}
		requirePersonaPeriodicDeadline(t, engine, due)
		wire := deliverPersonaClaim(t, engine, claim, due)
		sequence, status, err := DecodeExtendedStatusNoEventsMessage(wire)
		if err != nil || sequence != claim.Sequence() || status != NewWiredNoBatteryStatus(false) {
			t.Fatalf("status wire = % x, decoded (%d, %+v, %v)", wire, sequence, status, err)
		}
		if engine.Snapshot().InputSequence != before.InputSequence {
			t.Fatal("periodic status consumed an ordinary-input sequence")
		}
	}
	requirePersonaPeriodicDeadline(t, engine, 30003)
	claim := requirePersonaPeriodicClaim(t, engine, 30003)
	deliverPersonaClaim(t, engine, claim, 30003)
	requirePersonaPeriodicDeadline(t, engine, 50003)
}

func TestControllerPersonaPeriodicStatusDeliveryResetsWithoutCatchUp(t *testing.T) {
	engine, _ := makePersonaActive(t, []byte{1})
	claim := requirePersonaPeriodicClaim(t, engine, 8000)
	requirePersonaPeriodicDeadline(t, engine, 1003)
	deliverPersonaClaim(t, engine, claim, 8500)
	requirePersonaPeriodicDeadline(t, engine, 9500)
	if _, present, err := engine.ClaimPoll(8500); err != nil || present {
		t.Fatalf("replayed a missed status: present=%t err=%v", present, err)
	}
	claim = requirePersonaPeriodicClaim(t, engine, 20000)
	deliverPersonaClaim(t, engine, claim, 21000)
	requirePersonaPeriodicDeadline(t, engine, 41000)
	if _, present, err := engine.ClaimPoll(21000); err != nil || present {
		t.Fatalf("replayed a steady-state missed status: present=%t err=%v", present, err)
	}
}

func TestControllerPersonaPeriodicStatusRetryIsExactAndDoesNotAdvanceTimer(t *testing.T) {
	engine, _ := makePersonaActive(t, []byte{1})
	claim := requirePersonaPeriodicClaim(t, engine, 1003)
	var original [ExtendedStatusNoEventsMessageSize]byte
	if err := engine.AdmitAndCopy(claim, original[:], 1004); err != nil {
		t.Fatal(err)
	}
	if err := engine.Resolve(claim, ControllerPersonaDeliveryFailed, 1010); err != nil {
		t.Fatal(err)
	}
	requirePersonaPeriodicDeadline(t, engine, 1003)
	changed := ExtendedStatusNoEventsBodyV1{PowerLevel: StatusFullPower,
		ChargeState: StatusCharging, BatteryType: StatusBatteryRechargeable, BatteryLevel: StatusBatteryMedium}
	if err := engine.SetCurrentStatus(changed); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.ClaimPoll(1020); !errors.Is(err, ErrControllerPersonaRetryRequired) {
		t.Fatalf("new status overtook mandatory retry: %v", err)
	}
	retry, err := engine.ClaimRetry(1020)
	if err != nil || retry.Sequence() != claim.Sequence() || retry.Action() != claim.Action() {
		t.Fatalf("status retry = (%+v, %v)", retry, err)
	}
	retryWire := deliverPersonaClaim(t, engine, retry, 1025)
	if !bytes.Equal(original[:], retryWire) {
		t.Fatalf("status retry resampled current source: % x -> % x", original, retryWire)
	}
	requirePersonaPeriodicDeadline(t, engine, 2025)
	claim = requirePersonaPeriodicClaim(t, engine, 2025)
	_, status, err := DecodeExtendedStatusNoEventsMessage(deliverPersonaClaim(t, engine, claim, 2025))
	if err != nil || status != changed {
		t.Fatalf("next status omitted latest source: %+v, %v", status, err)
	}
}

func TestControllerPersonaPeriodicStatusStartsOnlyAfterDeliveredSTARTStatus(t *testing.T) {
	engine := newTestControllerPersonaEngine(t, []byte{1}, 0)
	requirePersonaPeriodicDeadline(t, engine, 0)
	configurePersonaUSB(t, engine, 0)
	hello, _, err := engine.ClaimPoll(0)
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, hello, 1)
	requirePersonaPeriodicDeadline(t, engine, 0)
	start := claimPersonaHostAction(t, engine, 2,
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
	if err := engine.AdmitAndCopy(start, nil, 2); !errors.Is(err, ErrInvalidControllerPersonaDestination) {
		t.Fatalf("short destination = %v", err)
	}
	if err := engine.Resolve(start, ControllerPersonaDeferred, 3); err != nil {
		t.Fatal(err)
	}
	if engine.statusTimer.started {
		t.Fatal("failed/selected START armed status timer")
	}
	retry, err := engine.ClaimRetry(4)
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, retry, 7)
	if !engine.statusTimer.started || engine.statusTimer.startedAtMS != 7 || engine.statusTimer.nextAtMS != 1007 {
		t.Fatalf("START completion did not anchor timer: %+v", engine.statusTimer)
	}
	requirePersonaPeriodicDeadline(t, engine, 0) // initial and Permit still own lifecycle
	for _, now := range []uint64{8, 9} {
		next, err := engine.ClaimNextLifecycleAction(now)
		if err != nil {
			t.Fatal(err)
		}
		deliverPersonaClaim(t, engine, next, now)
	}
	requirePersonaPeriodicDeadline(t, engine, 1007)
}

func TestControllerPersonaPeriodicStatusStopRestartAndPowerTransitions(t *testing.T) {
	for _, state := range []SetDeviceStateValue{SetDeviceStateStop, SetDeviceStateOff, SetDeviceStateReset} {
		t.Run(string(rune('0'+state)), func(t *testing.T) {
			engine, _ := makePersonaActive(t, []byte{1})
			gate := claimPersonaHostAction(t, engine, 100,
				[]byte{0x05, 0x20, 0x03, 0x01, byte(state)})
			deliverPersonaClaim(t, engine, gate, 100)
			if engine.statusTimer != (controllerPersonaStatusTimer{}) {
				t.Fatal("delivered gate retained periodic timer")
			}
			requirePersonaPeriodicDeadline(t, engine, 0)
			clear, err := engine.ClaimNextLifecycleAction(101)
			if err != nil {
				t.Fatal(err)
			}
			deliverPersonaClaim(t, engine, clear, 101)
			if state != SetDeviceStateStop {
				status, err := engine.ClaimNextLifecycleAction(102)
				if err != nil || status.Action() != ControllerPersonaSendPoweringOffStatus {
					t.Fatalf("power status = (%+v, %v)", status, err)
				}
				deliverPersonaClaim(t, engine, status, 102)
				requirePersonaPeriodicDeadline(t, engine, 0)
				terminal, present, err := engine.ClaimPoll(102 + PowerTransitionDelayMilliseconds)
				if err != nil || !present {
					t.Fatalf("terminal action = (%+v, %t, %v)", terminal, present, err)
				}
				deliverPersonaClaim(t, engine, terminal, 102+PowerTransitionDelayMilliseconds)
				requirePersonaPeriodicDeadline(t, engine, 0)
				return
			}
			if _, present, err := engine.ClaimPoll(100000); err != nil || present {
				t.Fatalf("stopped controller emitted periodic status: %t, %v", present, err)
			}
			start := claimPersonaHostAction(t, engine, 100001,
				[]byte{0x05, 0x20, 0x04, 0x01, byte(SetDeviceStateStart)})
			deliverPersonaClaim(t, engine, start, 100002)
			for _, now := range []uint64{100003, 100004} {
				next, err := engine.ClaimNextLifecycleAction(now)
				if err != nil {
					t.Fatal(err)
				}
				deliverPersonaClaim(t, engine, next, now)
			}
			requirePersonaPeriodicDeadline(t, engine, 101002)
		})
	}
}

func TestControllerPersonaPeriodicStatusTransportBoundaryRetiresRetryAndTimer(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		name := "USB reset"
		if disconnect {
			name = "disconnect and reconnect"
		}
		t.Run(name, func(t *testing.T) {
			engine, _ := makePersonaActive(t, []byte{1})
			claim := requirePersonaPeriodicClaim(t, engine, 1003)
			if err := engine.Resolve(claim, ControllerPersonaDeferred, 1004); err != nil {
				t.Fatal(err)
			}
			var boundary ControllerPersonaClaim
			var err error
			if disconnect {
				boundary, err = engine.BeginDisconnect(1005)
			} else {
				boundary, err = engine.BeginUSBReset(1005)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.Resolve(claim, ControllerPersonaDelivered, 1005); !errors.Is(err, ErrInvalidControllerPersonaClaim) {
				t.Fatalf("stale status completion crossed boundary: %v", err)
			}
			deliverPersonaClaim(t, engine, boundary, 1005)
			if disconnect {
				if err := engine.Reconnect(1006); err != nil {
					t.Fatal(err)
				}
			}
			requirePersonaPeriodicDeadline(t, engine, 0)
			if engine.statusTimer.started || engine.Snapshot().RetryPending || engine.Snapshot().GlobalSequence != 0 {
				t.Fatalf("boundary retained timer/sequence/retry: %+v, %+v", engine.statusTimer, engine.Snapshot())
			}
		})
	}
}

func TestControllerPersonaPeriodicStatusHaltAndConfigurationGates(t *testing.T) {
	for _, unconfigure := range []bool{false, true} {
		name := "IN halt"
		if unconfigure {
			name = "configuration loss"
		}
		t.Run(name, func(t *testing.T) {
			engine, _ := makePersonaActive(t, []byte{1})
			setup := testUSBSetup(usbRequestTypeEndpointOut, usbRequestSetFeature, usbFeatureEndpointHalt, 0x81, 0)
			if unconfigure {
				setup = testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration, 0, 0, 0)
			}
			control, err := engine.ClaimUSBControl(setup)
			if err != nil {
				t.Fatal(err)
			}
			deliverPersonaClaim(t, engine, control, 100)
			requirePersonaPeriodicDeadline(t, engine, 0)
			if unconfigure {
				clear, present, err := engine.ClaimPoll(101)
				if err != nil || !present || clear.Action() != ControllerPersonaClearOutputs {
					t.Fatalf("configuration clear = (%+v, %t, %v)", clear, present, err)
				}
				deliverPersonaClaim(t, engine, clear, 101)
			}
			if _, present, err := engine.ClaimPoll(2000); err != nil || present {
				t.Fatalf("gated status = %t, %v", present, err)
			}
			setup = testUSBSetup(usbRequestTypeEndpointOut, usbRequestClearFeature, usbFeatureEndpointHalt, 0x81, 0)
			if unconfigure {
				setup = testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration, 1, 0, 0)
			}
			control, err = engine.ClaimUSBControl(setup)
			if err != nil {
				t.Fatal(err)
			}
			deliverPersonaClaim(t, engine, control, 2001)
			requirePersonaPeriodicDeadline(t, engine, 1003)
			claim := requirePersonaPeriodicClaim(t, engine, 2002)
			deliverPersonaClaim(t, engine, claim, 2002)
			requirePersonaPeriodicDeadline(t, engine, 3002)
		})
	}
}

func TestControllerPersonaPeriodicStatusTimestampSaturationDoesNotSpin(t *testing.T) {
	var timer controllerPersonaStatusTimer
	maximum := ^uint64(0)
	timer.delivered(maximum-500, true)
	if !timer.scheduled || timer.nextAtMS != maximum {
		t.Fatalf("deadline wrapped at clock horizon: %+v", timer)
	}
	timer.delivered(maximum, false)
	if timer.scheduled || timer.nextAtMS != maximum {
		t.Fatalf("terminal clock created an always-due timer: %+v", timer)
	}
}

func TestControllerPersonaPeriodicStatusWarmedPathAllocatesZero(t *testing.T) {
	engine, _ := makePersonaActive(t, []byte{1})
	var scratch [ExtendedStatusNoEventsMessageSize]byte
	allocations := testing.AllocsPerRun(1000, func() {
		due, scheduled := engine.NextPeriodicStatusDeadlineMilliseconds()
		if !scheduled {
			panic("status deadline unavailable")
		}
		claim, present, err := engine.ClaimPoll(due)
		if err != nil || !present || claim.Action() != ControllerPersonaSendCurrentStatus {
			panic("status claim failed")
		}
		if err := engine.AdmitAndCopy(claim, scratch[:], due); err != nil {
			panic(err)
		}
		if err := engine.Resolve(claim, ControllerPersonaDelivered, due); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("warmed periodic status allocations = %v", allocations)
	}
}

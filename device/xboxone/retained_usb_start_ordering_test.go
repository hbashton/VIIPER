package xboxone

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
)

// The host may submit its next OUT after current status, before it requests
// START's mandatory initial IN. That ordering is a lifecycle wait, not malformed
// USB traffic, and must not quarantine the entire retained import.
func TestRetainedUSBStartRetainsOUTUntilLatestInitialInput(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	motor := RumbleBodyV1{Enabled: MotorLeftVibration, LeftVibration: 11}
	var motorWire [DirectMotorMessageSize]byte
	if err := EncodeDirectMotorMessageInto(motorWire[:], 0x31, motor); err != nil {
		t.Fatal(err)
	}
	out, err := adapter.Stage(retainedOUTRequest(41, 6, 6, motorWire[:]))
	if err != nil {
		t.Fatal(err)
	}
	for attempt := uint64(4); attempt <= 5; attempt++ {
		prepared, err := adapter.Prepare(out, nil, adapterTestTime(attempt))
		if err != nil || prepared.Result != retainedusb.ResultPending || prepared.ActualLength != 0 ||
			!prepared.RetryAt.IsZero() || prepared.ReadinessEpoch == 0 {
			t.Fatalf("OUT awaiting initial input = (%+v, %v)", prepared, err)
		}
	}
	active, claim, _ := retainedJournalLifecycleState(adapter)
	if active || claim.Valid() {
		t.Fatal("OUT selected an early initial input baseline")
	}
	latest := GamepadInputReportV1{State: InputStateV1{B: true, LeftStickX: 1234}}
	if err := adapter.PublishSemanticInput(41, 2, latest); err != nil {
		t.Fatal(err)
	}
	beforeInitial, ready := adapter.Readiness()
	select {
	case <-ready:
	default:
	}
	if got := readRetainedJournalTestInput(t, adapter, 7); got != latest {
		t.Fatalf("initial input = %+v, want latest %+v", got, latest)
	}
	if after, _ := adapter.Readiness(); after <= beforeInitial {
		t.Fatal("initial IN completion did not advance OUT readiness")
	}
	select {
	case <-ready:
	default:
		t.Fatal("initial IN completion did not wake the retained OUT")
	}
	// The same OUT must then wait through the local permission action and be
	// admitted exactly once, without retransmission or a replacement ticket.
	deadline := time.Now().Add(time.Second)
	for {
		prepared, err := adapter.Prepare(out, nil, adapterTestTime(8))
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Result == retainedusb.ResultSuccess {
			if prepared.ActualLength != uint32(len(motorWire)) {
				t.Fatalf("OUT length = %d", prepared.ActualLength)
			}
			break
		}
		if prepared.Result != retainedusb.ResultPending || time.Now().After(deadline) {
			t.Fatalf("OUT did not resume after initial input: %+v", prepared)
		}
		time.Sleep(time.Millisecond)
	}
	waitForLocalExecution(t, executor, ControllerPersonaPermitNormalUpstream)
	if err := adapter.Complete(out, true, adapterTestTime(8)); err != nil {
		t.Fatal(err)
	}
	triggerAndRetireLocalAction(t, adapter, 8, 9)
	if delivered := waitForLocalExecution(t, executor, ControllerPersonaApplyDirectMotor); delivered.DirectMotor != motor {
		t.Fatalf("retained OUT changed feedback: %+v", delivered)
	}
	if err := adapter.Complete(out, true, adapterTestTime(9)); err == nil {
		t.Fatal("duplicate OUT completion accepted")
	}
}

func TestRetainedUSBStartWaitingOUTCanBeUnlinkedWithoutFeedback(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	var wire [DirectMotorMessageSize]byte
	if err := EncodeDirectMotorMessageInto(wire[:], 0x31,
		RumbleBodyV1{Enabled: MotorRightImpulse, RightImpulse: 22}); err != nil {
		t.Fatal(err)
	}
	out, err := adapter.Stage(retainedOUTRequest(41, 6, 6, wire[:]))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := adapter.Prepare(out, nil, adapterTestTime(4)); err != nil || result.Result != retainedusb.ResultPending {
		t.Fatalf("initial wait = (%+v, %v)", result, err)
	}
	if err := adapter.Retire(out, retainedusb.RetireUnlink, adapterTestTime(4)); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Prepare(out, nil, adapterTestTime(4)); !errors.Is(err, errDormantRetainedUSBInvalidTicket) {
		t.Fatalf("retired OUT remained usable: %v", err)
	}
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 7, 7, 64), 4)
	triggerAndRetireLocalAction(t, adapter, 8, 5)
	waitForLocalExecution(t, executor, ControllerPersonaPermitNormalUpstream)
	executor.mu.Lock()
	defer executor.mu.Unlock()
	for _, execution := range executor.executions {
		if execution.Action == ControllerPersonaApplyDirectMotor {
			t.Fatal("unlinked OUT actuated after initial IN")
		}
	}
}

func TestRetainedUSBStartWaitingOUTSurvivesFailedInitialInputDelivery(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	initial := GamepadInputReportV1{State: InputStateV1{A: true}}
	if err := adapter.PublishSemanticInput(41, 2, initial); err != nil {
		t.Fatal(err)
	}
	motor := RumbleBodyV1{Enabled: MotorLeftImpulse, LeftImpulse: 19}
	var command [DirectMotorMessageSize]byte
	if err := EncodeDirectMotorMessageInto(command[:], 0x31, motor); err != nil {
		t.Fatal(err)
	}
	out, err := adapter.Stage(retainedOUTRequest(41, 6, 6, command[:]))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := adapter.Prepare(out, nil, adapterTestTime(4)); err != nil || got.Result != retainedusb.ResultPending {
		t.Fatalf("before initial IN = (%+v, %v)", got, err)
	}
	input, err := adapter.Stage(retainedINRequest(41, 7, 7, 64))
	if err != nil {
		t.Fatal(err)
	}
	var wire [64]byte
	prepared, err := adapter.Prepare(input, wire[:], adapterTestTime(5))
	if err != nil || prepared.Result != retainedusb.ResultData {
		t.Fatalf("initial IN = (%+v, %v)", prepared, err)
	}
	if err := adapter.Complete(input, false, adapterTestTime(5)); err != nil {
		t.Fatal(err)
	}
	newer := GamepadInputReportV1{State: InputStateV1{B: true}}
	if err := adapter.PublishSemanticInput(41, 3, newer); err != nil {
		t.Fatal(err)
	}
	// A failed delivery may use the existing bounded retry deadline; unlike
	// the original START wait, that is a retry of an actual failed boundary.
	if got, err := adapter.Prepare(out, nil, adapterTestTime(6)); err != nil || got.Result != retainedusb.ResultPending {
		t.Fatalf("after failed initial IN = (%+v, %v)", got, err)
	}
	_, retry := retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 8, 8, 64), 7)
	if !bytes.Equal(retry, wire[:prepared.ActualLength]) {
		t.Fatalf("initial retry changed bytes: % x -> % x", wire[:prepared.ActualLength], retry)
	}
	_, decoded, err := DecodeGamepadInputMessage(retry)
	if err != nil || decoded != initial {
		t.Fatalf("retry replaced baseline with newer input: %+v, %v", decoded, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		got, err := adapter.Prepare(out, nil, adapterTestTime(8))
		if err != nil {
			t.Fatal(err)
		}
		if got.Result == retainedusb.ResultSuccess {
			break
		}
		if got.Result != retainedusb.ResultPending || time.Now().After(deadline) {
			t.Fatalf("OUT after initial retry = %+v", got)
		}
		time.Sleep(time.Millisecond)
	}
	executions, _, _ := executor.snapshot()
	for _, execution := range executions {
		if execution.Action == ControllerPersonaApplyDirectMotor {
			t.Fatal("OUT actuated before its own successful completion")
		}
	}
	if err := adapter.Complete(out, true, adapterTestTime(8)); err != nil {
		t.Fatal(err)
	}
	if got := waitForLocalExecution(t, executor, ControllerPersonaApplyDirectMotor); got.DirectMotor != motor {
		t.Fatalf("feedback changed across initial retry: %+v", got)
	}
	if err := adapter.Complete(out, true, adapterTestTime(9)); err == nil {
		t.Fatal("duplicate OUT completion accepted")
	}
	if got := readRetainedJournalTestInput(t, adapter, 10); got != newer {
		t.Fatalf("newer input lost after initial retry: %+v", got)
	}
	executions, _, _ = executor.snapshot()
	motorCount := 0
	for _, execution := range executions {
		if execution.Action == ControllerPersonaApplyDirectMotor {
			motorCount++
		}
	}
	if motorCount != 1 {
		t.Fatalf("motor executions = %d, want exactly one", motorCount)
	}
}

func TestRetainedUSBStartWaitingOUTRetiresAtTerminalBoundariesWithoutInitialIN(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "connection close"
		if reset {
			name = "device reset"
		}
		t.Run(name, func(t *testing.T) {
			adapter, executor := startRetainedJournalTestPersona(t)
			lease := adapterTestLease(adapter, 41)
			var command [DirectMotorMessageSize]byte
			if err := EncodeDirectMotorMessageInto(command[:], 0x31,
				RumbleBodyV1{Enabled: MotorRightImpulse, RightImpulse: 23}); err != nil {
				t.Fatal(err)
			}
			out, err := adapter.Stage(retainedOUTRequest(41, 6, 6, command[:]))
			if err != nil {
				t.Fatal(err)
			}
			if got, err := adapter.Prepare(out, nil, adapterTestTime(4)); err != nil || got.Result != retainedusb.ResultPending {
				t.Fatalf("waiting OUT = (%+v, %v)", got, err)
			}
			reason := retainedusb.RetireConnectionClose
			if reset {
				reason = retainedusb.RetireDeviceReset
			}
			if err := adapter.Retire(out, reason, adapterTestTime(5)); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			if reset {
				resetLease := adapterTestReset(lease, 71, 1)
				result, err := adapter.ResetAndRestart(resetLease, deadline)
				if err != nil || result.State != retainedusb.ImportResetSafe {
					t.Fatalf("reset without initial IN = (%+v, %v)", result, err)
				}
				if duplicate, err := adapter.ResetAndRestart(resetLease, time.Time{}); err != nil || duplicate != result {
					t.Fatalf("duplicate reset = (%+v, %v)", duplicate, err)
				}
				executor.mu.Lock()
				neutrals := append([]ControllerPersonaLocalExecution(nil), executor.resetNeutrals...)
				drains := executor.resetDrainCount
				executor.mu.Unlock()
				if drains != 1 || len(neutrals) != 1 || neutrals[0].Action != ControllerPersonaClearOutputs ||
					neutrals[0].Generation != 2 || neutrals[0].ClearEpoch == 0 {
					t.Fatalf("reset boundary duplicated or lost: drains=%d neutrals=%+v", drains, neutrals)
				}
				if snapshot, ok := adapter.coordinator.snapshot(); !ok || snapshot.Generation != 2 ||
					snapshot.ClaimOutstanding || snapshot.RetryPending {
					t.Fatalf("successor inherited pending work: %+v", snapshot)
				}
			} else {
				if result, err := adapter.CancelAndDrain(lease, retainedusb.ImportClosePeerDisconnect, deadline); err != nil || result.State != retainedusb.ImportDrainDrained {
					t.Fatalf("drain without initial IN = (%+v, %v)", result, err)
				}
				if result, err := adapter.DisconnectNeutral(lease, retainedusb.ImportClosePeerDisconnect, deadline); err != nil || result.State != retainedusb.ImportDisconnectSafe {
					t.Fatalf("neutral without initial IN = (%+v, %v)", result, err)
				}
				if _, err := adapter.DisconnectNeutral(lease, retainedusb.ImportClosePeerDisconnect, deadline); err == nil {
					t.Fatal("duplicate disconnect accepted")
				}
				_, neutrals, drains := executor.snapshot()
				if drains != 1 || len(neutrals) != 1 || neutrals[0].Action != ControllerPersonaClearOutputs || neutrals[0].ClearEpoch == 0 {
					t.Fatalf("close boundary duplicated or lost: drains=%d neutrals=%+v", drains, neutrals)
				}
			}
			if _, err := adapter.Prepare(out, nil, adapterTestTime(6)); err == nil {
				t.Fatal("old OUT preparation survived terminal boundary")
			}
			if err := adapter.Complete(out, true, adapterTestTime(6)); err == nil {
				t.Fatal("old OUT completion survived terminal boundary")
			}
			executions, _, _ := executor.snapshot()
			for _, execution := range executions {
				if execution.Action == ControllerPersonaApplyDirectMotor {
					t.Fatal("terminally retired OUT actuated without initial IN")
				}
			}
		})
	}
}

func TestRetainedUSBStartWaitTranslationRemainsNarrow(t *testing.T) {
	adapter := &DormantRetainedUSBAdapter{}
	for _, test := range []struct {
		name   string
		lane   retainedusb.Lane
		reason controllerPersonaTransportWaitReason
		action ControllerPersonaAction
	}{
		{"IN without input", retainedusb.LaneInterruptIn, controllerPersonaTransportInputRequired, ControllerPersonaSendInitialInput},
		{"control without input", retainedusb.LaneControl, controllerPersonaTransportInputRequired, ControllerPersonaSendInitialInput},
		{"wrong action", retainedusb.LaneInterruptOut, controllerPersonaTransportInputRequired, ControllerPersonaSendCurrentStatus},
		{"response outstanding", retainedusb.LaneInterruptOut, controllerPersonaTransportResponseOutstanding, 0},
		{"failed host boundary", retainedusb.LaneInterruptOut, controllerPersonaTransportBoundaryRequired, 0},
		{"undersized IN", retainedusb.LaneInterruptIn, controllerPersonaTransportBufferTooSmall, ControllerPersonaSendInitialInput},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, err := adapter.translateAdmission(retainedusb.Ticket{Lane: test.lane},
				controllerPersonaTransportAdmission{disposition: controllerPersonaTransportWait,
					waitReason: test.reason, action: test.action}, 64, adapterTestTime(1))
			if !errors.Is(err, errDormantRetainedUSBInvalidPreparation) {
				t.Fatalf("unexpected wait was accepted: %v", err)
			}
		})
	}
}

func TestRetainedUSBStartSharePersonaRetainsDocumentedSevenByteCommand(t *testing.T) {
	engine := newOfficialInputPersonaEngine(t,
		OfficialGamepadMetadataConsoleFunctionMap, GamepadInputReportV1{})
	executor := newScriptedControllerPersonaLocalExecutor()
	adapter, err := NewDormantRetainedUSBAdapter(engine, 11, 22, executor, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := adapter.BindImport(adapterTestLease(adapter, 41), time.Now().Add(time.Second)); err != nil || result.State != retainedusb.ImportBindBound {
		t.Fatalf("bind = (%+v, %v)", result, err)
	}
	t.Cleanup(func() { stopRetainedUSBAdapterTestWorker(adapter) })
	configureRetainedTestPersona(t, adapter)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	retainedPrepareAndComplete(t, adapter, retainedOUTRequest(41, 4, 4,
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)}), 2)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 5, 5, 64), 3)

	// This is a source-valid MS-GIPUSB LED fixture, not a claim that the
	// uncaptured seven-byte OUT in the live failure was an LED command.
	led := GuideLEDCommandV1{Pattern: GuideLEDPatternOn, Intensity: 20}
	var command [GuideLEDCommandMessageSize]byte
	if err := EncodeGuideLEDCommandMessageInto(command[:], 0x31, led); err != nil {
		t.Fatal(err)
	}
	if len(command) != 7 {
		t.Fatal("fixture no longer covers the seven-byte host request")
	}
	out, err := adapter.Stage(retainedOUTRequest(41, 6, 6, command[:]))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := adapter.Prepare(out, nil, adapterTestTime(4)); err != nil ||
		got.Result != retainedusb.ResultPending || !got.RetryAt.IsZero() {
		t.Fatalf("Share persona initial wait = (%+v, %v)", got, err)
	}
	latest := GamepadInputReportV1{State: InputStateV1{Share: true, A: true, LeftStickX: 2345}}
	if err := adapter.PublishSemanticInput(41, 2, latest); err != nil {
		t.Fatal(err)
	}
	_, wire := retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 7, 7, 64), 5)
	if len(wire) != 36 {
		t.Fatalf("Share initial input length = %d, want 36", len(wire))
	}
	if _, got, err := DecodeConsoleFunctionMapGamepadInputMessage(wire); err != nil || got != latest {
		t.Fatalf("Share initial input = (%+v, %v)", got, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		got, err := adapter.Prepare(out, nil, adapterTestTime(6))
		if err != nil {
			t.Fatal(err)
		}
		if got.Result == retainedusb.ResultSuccess {
			if got.ActualLength != 7 {
				t.Fatalf("OUT completion length = %d", got.ActualLength)
			}
			break
		}
		if got.Result != retainedusb.ResultPending || time.Now().After(deadline) {
			t.Fatalf("LED after initial IN = %+v", got)
		}
		time.Sleep(time.Millisecond)
	}
	if err := adapter.Complete(out, true, adapterTestTime(6)); err != nil {
		t.Fatal(err)
	}
	if got := waitForLocalExecution(t, executor, ControllerPersonaApplyGuideLED); got.GuideLED != led {
		t.Fatalf("retained LED changed: %+v", got)
	}
}

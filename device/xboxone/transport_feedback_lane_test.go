package xboxone

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
)

func beginTestCoordinatorFeedback(t *testing.T, c *controllerPersonaTransportCoordinator, wire []byte) controllerPersonaLocalLease {
	t.Helper()
	const now uint64 = 10
	ticket, err := c.stageInterruptOut(wire)
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.admit(ticket, nil, now)
	if err != nil || a.disposition != controllerPersonaTransportInterruptOutResponse {
		t.Fatalf("feedback OUT: %+v, %v", a, err)
	}
	if err := c.completeResponse(ticket, true, now); err != nil {
		t.Fatal(err)
	}
	lease, present, err := c.admitLocal(now)
	if err != nil || !present {
		t.Fatalf("feedback admission: %+v, %t, %v", lease, present, err)
	}
	return lease
}

func TestControllerPersonaTransportFeedbackLaneAllowsInputGuideAndStatus(t *testing.T) {
	var led [GuideLEDCommandMessageSize]byte
	if err := EncodeGuideLEDCommandMessageInto(led[:], 1, GuideLEDCommandV1{Pattern: GuideLEDPatternOn, Intensity: 20}); err != nil {
		t.Fatal(err)
	}
	for name, wire := range map[string][]byte{"motor": testCoordinatorDirectMotorWire(t, 1, testDirectMotorBody()), "led": led[:]} {
		t.Run(name, func(t *testing.T) {
			c := newTestControllerPersonaTransportCoordinator(t, []byte{1})
			makeTestCoordinatorActive(t, c)
			before, _ := c.snapshot()
			lease := beginTestCoordinatorFeedback(t, c, wire)
			for i := 0; i < 256; i++ {
				want := GamepadInputReportV1{State: InputStateV1{A: i%2 == 0, LeftStickX: int16(i + 1)}}
				_, gotWire := admitAndCompleteTestSemanticIN(t, c, uint64(i+11), want)
				_, got, err := DecodeGamepadInputMessage(gotWire)
				if err != nil || got != want {
					t.Fatalf("input %d: %+v, %v", i, got, err)
				}
			}
			if after, _ := c.snapshot(); after.Feedback != before.Feedback {
				t.Fatal("IN progress falsely delivered feedback")
			}
			ticket, err := c.stageInterruptIn(64)
			if err != nil {
				t.Fatal(err)
			}
			var scratch [64]byte
			guide := GuideButtonStatusV1{Down: true}
			a, err := c.admitInputWithGuide(ticket, scratch[:], 300, GamepadInputReportV1{}, &guide)
			if err != nil || a.action != ControllerPersonaSendGuideButtonStatus || !a.consumesGuideEdge {
				t.Fatalf("Guide blocked: %+v, %v", a, err)
			}
			if err := c.completeResponse(ticket, true, 300); err != nil {
				t.Fatal(err)
			}
			a, status := admitAndCompleteTestIN(t, c, 1003)
			if a.action != ControllerPersonaSendCurrentStatus {
				t.Fatalf("periodic status blocked: %+v", a)
			}
			if _, _, err := DecodeExtendedStatusNoEventsMessage(status); err != nil {
				t.Fatal(err)
			}
			if err := c.completeLocal(lease, ControllerPersonaDelivered, 1003); err != nil {
				t.Fatal(err)
			}
			if after, _ := c.snapshot(); after.Feedback == before.Feedback {
				t.Fatal("successful feedback did not commit")
			}
		})
	}
}

func TestControllerPersonaTransportFeedbackLaneCompletionPreservesAdmittedIN(t *testing.T) {
	for _, outcome := range []ControllerPersonaOutcome{ControllerPersonaDelivered, ControllerPersonaDeferred, ControllerPersonaDeliveryFailed, ControllerPersonaExecutionCancelled} {
		t.Run(string(rune('0'+outcome)), func(t *testing.T) {
			c := newTestControllerPersonaTransportCoordinator(t, []byte{1})
			makeTestCoordinatorActive(t, c)
			motor := testDirectMotorBody()
			lease := beginTestCoordinatorFeedback(t, c, testCoordinatorDirectMotorWire(t, 1, motor))
			ticket, err := c.stageInterruptIn(64)
			if err != nil {
				t.Fatal(err)
			}
			var scratch [64]byte
			want := GamepadInputReportV1{State: InputStateV1{A: true, LeftTrigger: 777}}
			a, err := c.admitInput(ticket, scratch[:], 11, want)
			if err != nil || a.disposition != controllerPersonaTransportInterruptInResponse {
				t.Fatalf("input: %+v, %v", a, err)
			}
			response := c.activeResponse
			if err := c.completeLocal(lease, outcome, 12); err != nil {
				t.Fatal(err)
			}
			if c.activeResponse != response {
				t.Fatal("feedback completion replaced admitted IN ownership")
			}
			if outcome != ControllerPersonaDelivered {
				retry, present, err := c.admitLocal(13)
				if err != nil || !present || retry.directMotor != motor || retry.token == lease.token {
					t.Fatalf("independent exact feedback retry: %+v, %t, %v", retry, present, err)
				}
				if err := c.completeLocal(retry, ControllerPersonaDelivered, 14); err != nil {
					t.Fatal(err)
				}
				if c.activeResponse != response {
					t.Fatal("feedback retry replaced admitted IN ownership")
				}
			}
			if err := c.completeResponse(ticket, true, 15); err != nil {
				t.Fatal(err)
			}
			_, got, err := DecodeGamepadInputMessage(scratch[:a.size])
			if err != nil || got != want {
				t.Fatalf("admitted input changed: %+v, %v", got, err)
			}
			if err := c.completeLocal(lease, outcome, 16); !errors.Is(err, errInvalidControllerPersonaTransportCompletion) {
				t.Fatalf("duplicate local completion accepted: %v", err)
			}
		})
	}
}

func TestControllerPersonaTransportFeedbackLaneBothRetriesRemainExact(t *testing.T) {
	for _, feedbackFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "input-first", true: "feedback-first"}[feedbackFirst], func(t *testing.T) {
			c := newTestControllerPersonaTransportCoordinator(t, []byte{1})
			makeTestCoordinatorActive(t, c)
			motor := testDirectMotorBody()
			lease := beginTestCoordinatorFeedback(t, c, testCoordinatorDirectMotorWire(t, 1, motor))
			ticket, err := c.stageInterruptIn(64)
			if err != nil {
				t.Fatal(err)
			}
			var first [64]byte
			a, err := c.admitInput(ticket, first[:], 11, GamepadInputReportV1{State: InputStateV1{Y: true}})
			if err != nil || a.disposition != controllerPersonaTransportInterruptInResponse {
				t.Fatalf("input: %+v, %v", a, err)
			}
			if err := c.completeResponse(ticket, false, 12); err != nil {
				t.Fatal(err)
			}
			if err := c.completeLocal(lease, ControllerPersonaDeliveryFailed, 13); err != nil {
				t.Fatal(err)
			}
			completeFeedback := func() {
				retry, present, err := c.admitLocal(14)
				if err != nil || !present || retry.directMotor != motor {
					t.Fatalf("feedback retry: %+v, %t, %v", retry, present, err)
				}
				if err := c.completeLocal(retry, ControllerPersonaDelivered, 14); err != nil {
					t.Fatal(err)
				}
			}
			if feedbackFirst {
				completeFeedback()
			}
			ticket, err = c.stageInterruptIn(64)
			if err != nil {
				t.Fatal(err)
			}
			var second [64]byte
			b, err := c.admitInput(ticket, second[:], 14, GamepadInputReportV1{State: InputStateV1{B: true}})
			if err != nil || b.disposition != controllerPersonaTransportInterruptInResponse || !bytes.Equal(first[:a.size], second[:b.size]) {
				t.Fatalf("IN retry changed: %+v, %v, % x / % x", b, err, first[:a.size], second[:b.size])
			}
			if err := c.completeResponse(ticket, true, 14); err != nil {
				t.Fatal(err)
			}
			if !feedbackFirst {
				completeFeedback()
			}
		})
	}
}

func TestControllerPersonaTransportFeedbackLaneKeepsControlAndSTOPFenced(t *testing.T) {
	c := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	makeTestCoordinatorActive(t, c)
	lease := beginTestCoordinatorFeedback(t, c, testCoordinatorDirectMotorWire(t, 1, testDirectMotorBody()))
	control, err := c.stageControl(testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration, 0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	stop, err := c.stageInterruptOut([]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStop)})
	if err != nil {
		t.Fatal(err)
	}
	for _, ticket := range []controllerPersonaTransportTicket{control, stop} {
		a, err := c.admit(ticket, nil, 11)
		if err != nil || a.disposition != controllerPersonaTransportLocalRequired {
			t.Fatalf("lifecycle overtook feedback: %+v, %v", a, err)
		}
	}
	if err := c.beginUSBReset(11); !errors.Is(err, errControllerPersonaTransportLocalOutstanding) {
		t.Fatalf("reset crossed active feedback: %v", err)
	}
	if err := c.beginDisconnect(11); !errors.Is(err, errControllerPersonaTransportLocalOutstanding) {
		t.Fatalf("disconnect crossed active feedback: %v", err)
	}
	if err := c.completeLocal(lease, ControllerPersonaDeferred, 12); err != nil {
		t.Fatal(err)
	}
	for _, ticket := range []controllerPersonaTransportTicket{control, stop} {
		a, err := c.admit(ticket, nil, 13)
		if err != nil || a.disposition != controllerPersonaTransportLocalRequired {
			t.Fatalf("lifecycle overtook feedback retry: %+v, %v", a, err)
		}
	}
	retry, present, err := c.admitLocal(14)
	if err != nil || !present {
		t.Fatalf("feedback retry: %+v, %t, %v", retry, present, err)
	}
	if err := c.completeLocal(retry, ControllerPersonaDelivered, 14); err != nil {
		t.Fatal(err)
	}
	a, err := c.admit(stop, nil, 15)
	if err != nil || a.disposition != controllerPersonaTransportInterruptOutResponse {
		t.Fatalf("STOP unavailable after feedback: %+v, %v", a, err)
	}
	if err := c.completeResponse(stop, true, 15); err != nil {
		t.Fatal(err)
	}
	gate, present, err := c.admitLocal(15)
	if err != nil || !present || gate.action != ControllerPersonaGateNormalUpstream {
		t.Fatalf("STOP gate: %+v, %t, %v", gate, present, err)
	}
	in, err := c.stageInterruptIn(64)
	if err != nil {
		t.Fatal(err)
	}
	var scratch [64]byte
	a, err = c.admitInput(in, scratch[:], 16, GamepadInputReportV1{State: InputStateV1{A: true}})
	if err != nil || a.disposition != controllerPersonaTransportLocalRequired {
		t.Fatalf("STOP gate was bypassed: %+v, %v", a, err)
	}
	if err := c.retire(in); err != nil {
		t.Fatal(err)
	}
	if err := c.retire(control); err != nil {
		t.Fatal(err)
	}
	if err := c.completeLocal(gate, ControllerPersonaDelivered, 16); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedInputJournalContinuesWhileFeedbackExecutorIsBlocked(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
	permitRetainedJournalTestInput(t, adapter, executor)
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	executor.mu.Lock()
	executor.executeEntered = entered
	executor.executeRelease = release
	executor.mu.Unlock()
	retainedPrepareAndComplete(t, adapter, retainedOUTRequest(41, 8, 8, testCoordinatorDirectMotorWire(t, 1, testDirectMotorBody())), 6)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("feedback executor did not enter")
	}
	for i := 0; i < 256; i++ {
		want := GamepadInputReportV1{State: InputStateV1{A: i%2 == 0, LeftStickX: int16(i + 1)}}
		if err := adapter.PublishSemanticInput(41, uint64(i+2), want); err != nil {
			t.Fatal(err)
		}
		ticket, err := adapter.Stage(retainedINRequest(41, uint64(i+10), uint32(i+10), 64))
		if err != nil {
			t.Fatal(err)
		}
		var scratch [64]byte
		p, err := adapter.Prepare(ticket, scratch[:], adapterTestTime(20))
		if err != nil || p.Result != retainedusb.ResultData {
			t.Fatalf("input %d waited for executor: %+v, %v", i, p, err)
		}
		_, got, err := DecodeGamepadInputMessage(scratch[:p.ActualLength])
		if err != nil || got != want {
			t.Fatalf("input %d changed: %+v, %v", i, got, err)
		}
		if err := adapter.Complete(ticket, true, adapterTestTime(20)); err != nil {
			t.Fatal(err)
		}
	}
	if snapshot, _ := adapter.coordinator.snapshot(); snapshot.Feedback.DirectMotor == testDirectMotorBody() {
		t.Fatal("blocked executor falsely committed feedback")
	}
}

func TestRetainedFeedbackDetachmentSignalsReadinessWithoutPublication(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		t.Run(map[bool]string{false: "wake", true: "exhausted"}[exhausted], func(t *testing.T) {
			adapter := activeRetainedJournalLifecyclePersona(t)
			c := adapter.coordinator
			// Stage through the exact coordinator without waking the asynchronous
			// executor. This isolates admitLocalAt's detachment notification from
			// publication, OUT completion, and executor completion notifications.
			ticket, err := c.stageInterruptOut(testCoordinatorDirectMotorWire(t, 1, testDirectMotorBody()))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.admit(ticket, nil, 10); err != nil {
				t.Fatal(err)
			}
			if err := c.completeResponse(ticket, true, 10); err != nil {
				t.Fatal(err)
			}
			adapter.mu.Lock()
			if exhausted {
				adapter.readinessEpoch = ^uint64(0) - 1
			}
			before := adapter.readinessEpoch
			ready := adapter.readiness
			adapter.mu.Unlock()
			for len(ready) != 0 {
				<-ready
			}
			lease, present, _, err := adapter.admitLocalAt(adapterTestTime(10))
			if exhausted {
				if present || !errors.Is(err, errDormantRetainedUSBReadinessExhausted) {
					t.Fatalf("exhausted admission: %+v, %t, %v", lease, present, err)
				}
				state, _ := c.snapshot()
				if state.OrdinaryFeedbackAdmitted || !state.OrdinaryFeedbackRetryPending || c.activeLocal.lease.valid() {
					t.Fatalf("exhaustion stranded admitted feedback: %+v", state)
				}
				return
			}
			if err != nil || !present {
				t.Fatalf("local admission: %+v, %t, %v", lease, present, err)
			}
			select {
			case <-ready:
			default:
				t.Fatal("detachment advanced an epoch but did not wake the parked scheduler")
			}
			epoch, _ := adapter.Readiness()
			if epoch <= before {
				t.Fatal("detachment did not advance readiness")
			}
			if _, err := adapter.completeLocalAt(lease, ControllerPersonaDelivered, 10); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestControllerPersonaTransportFeedbackLaneWarmedPathAllocatesZero(t *testing.T) {
	c := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	makeTestCoordinatorActive(t, c)
	wire := testCoordinatorDirectMotorWire(t, 1, testDirectMotorBody())
	var scratch [64]byte
	allocations := testing.AllocsPerRun(1000, func() {
		lease := beginTestCoordinatorFeedback(t, c, wire)
		ticket, err := c.stageInterruptIn(64)
		if err != nil {
			t.Fatal(err)
		}
		a, err := c.admitInput(ticket, scratch[:], 10, GamepadInputReportV1{State: InputStateV1{A: true}})
		if err != nil || a.disposition != controllerPersonaTransportInterruptInResponse {
			t.Fatalf("input: %+v, %v", a, err)
		}
		if err := c.completeLocal(lease, ControllerPersonaDelivered, 10); err != nil {
			t.Fatal(err)
		}
		if err := c.completeResponse(ticket, true, 10); err != nil {
			t.Fatal(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("ordinary feedback + concurrent IN = %v allocations, want zero", allocations)
	}
}

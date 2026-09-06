package xboxone

import (
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
	"github.com/Alia5/VIIPER/internal/retainedusb"
	"github.com/Alia5/VIIPER/usb"
)

type authenticatedRetirementTestConn struct{ net.Conn }

func (authenticatedRetirementTestConn) VIIPERAuthenticated() bool { return true }

// Uses the production persona, executor, X1BR reader/writer and bridge. The
// net.Pipe peer stands in for DS4Windows; no physical acceptance is claimed.
func startProductionRetirementTest(t *testing.T) (*AuthorizedDormantRetainedUSBDevice, net.Conn, *productionBrokerFrameWriter) {
	t.Helper()
	device, _ := testProductionRetainedUSBDevice(t)
	adapter := device.adapter
	lease := retainedusb.ImportLease{
		AuthorityID: 11, DeviceID: device.deviceID, OwnerID: adapter.Identity(),
		ImportToken: 33, SessionGeneration: 41,
	}
	if bound, err := adapter.BindImport(lease, time.Now().Add(time.Second)); err != nil || bound.State != retainedusb.ImportBindBound {
		t.Fatalf("bind: %+v, %v", bound, err)
	}
	t.Cleanup(func() { stopRetainedUSBAdapterTestWorker(adapter) })
	server, client := net.Pipe()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var usbDevice usb.Device = device
	go func() { done <- ProductionStreamHandler(authenticatedRetirementTestConn{server}, &usbDevice, nil) }()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("broker did not join")
		}
	})
	writer := newProductionBrokerFrameWriter(client)
	if err := writer.write(productionBrokerConsumerReady, 0, nil); err != nil {
		t.Fatal(err)
	}
	var scratch [productionBrokerMaximumPayload]byte
	if frame, err := readProductionBrokerFrame(client, scratch[:]); err != nil || frame.typeID != productionBrokerConsumerReadyAck {
		t.Fatalf("ready: %+v, %v", frame, err)
	}
	configureRetainedTestPersona(t, adapter)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	retainedPrepareAndComplete(t, adapter, retainedOUTRequest(41, 4, 4,
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)}), 2)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 5, 5, 64), 3)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
	triggerAndRetireLocalAction(t, adapter, 7, 5)
	deadline := time.Now().Add(time.Second)
	for {
		snapshot, _ := adapter.coordinator.snapshot()
		if snapshot.NormalUpstream && !snapshot.ClaimOutstanding {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("input permission did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	return device, client, writer
}

func overflowProductionRetirementTest(t *testing.T, device *AuthorizedDormantRetainedUSBDevice, client net.Conn, writer *productionBrokerFrameWriter) uint64 {
	t.Helper()
	var wire [SemanticInputWireSize]byte
	var scratch [productionBrokerMaximumPayload]byte
	for revision := uint64(2); revision < 200; revision++ {
		if err := EncodeSemanticInputWireV1Into(wire[:], InputStateV1{A: revision%2 == 0}); err != nil {
			t.Fatal(err)
		}
		if err := writer.write(productionBrokerSemanticInput, revision, wire[:]); err != nil {
			t.Fatal(err)
		}
		frame, err := readProductionBrokerFrame(client, scratch[:])
		if err != nil || frame.typeID != productionBrokerSemanticInputAck || frame.correlation != revision {
			t.Fatalf("input ACK %d: %+v, %v", revision, frame, err)
		}
		if frame.payload[0] == productionBrokerRejected {
			request, err := device.adapter.RetainedImportRetirement()
			if err != nil || !request.Valid() || request.Lease != device.adapter.boundLease {
				t.Fatalf("retirement request: %+v, %v", request, err)
			}
			return revision
		}
	}
	t.Fatal("journal did not overflow")
	return 0
}

func TestProductionInputRetirementRequiresOriginalConsumerStopAck(t *testing.T) {
	for _, disposition := range []string{"accepted", "rejected", "wrong-correlation", "lost-socket"} {
		t.Run(disposition, func(t *testing.T) {
			device, client, writer := startProductionRetirementTest(t)
			rejectedRevision := overflowProductionRetirementTest(t, device, client, writer)
			if device.ProductionBrokerConsumerReady() || device.TryBeginProductionBrokerActivation() {
				t.Fatal("retiring owner allowed activation")
			}
			if _, ok := device.acquireBrokerStream(); ok {
				t.Fatal("retiring owner allowed replacement stream")
			}
			// A queued successor frame cannot mutate input or end the ACK lane.
			var wire [SemanticInputWireSize]byte
			_ = EncodeSemanticInputWireV1Into(wire[:], InputStateV1{B: true})
			if err := writer.write(productionBrokerSemanticInput, rejectedRevision+1, wire[:]); err != nil {
				t.Fatal(err)
			}
			var scratch [productionBrokerMaximumPayload]byte
			frame, err := readProductionBrokerFrame(client, scratch[:])
			if err != nil || frame.typeID != productionBrokerSemanticInputAck || frame.payload[0] != productionBrokerRejected {
				t.Fatalf("successor rejection: %+v, %v", frame, err)
			}
			device.brokerMu.Lock()
			unchanged := device.brokerInputRevision == rejectedRevision-1
			device.brokerMu.Unlock()
			if !unchanged {
				t.Fatal("retired input advanced revision")
			}
			adapter := device.adapter
			lease := adapter.boundLease
			deadline := time.Now().Add(time.Second)
			if drain, err := adapter.CancelAndDrain(lease, retainedusb.ImportCloseOwnerRequested, deadline); err != nil || drain.State != retainedusb.ImportDrainDrained {
				t.Fatalf("drain: %+v, %v", drain, err)
			}
			type result struct {
				value retainedusb.ImportDisconnectResult
				err   error
			}
			done := make(chan result, 1)
			go func() {
				value, err := adapter.DisconnectNeutral(lease, retainedusb.ImportCloseOwnerRequested, deadline)
				done <- result{value, err}
			}()
			frame, err = readProductionBrokerFrame(client, scratch[:])
			if err != nil || frame.typeID != productionBrokerCanonicalFeedback {
				t.Fatalf("terminal feedback: %+v, %v", frame, err)
			}
			var feedback controllerfeedback.Frame
			if err := feedback.UnmarshalFrom(frame.payload); err != nil || !feedback.IsStop() {
				t.Fatalf("missing Stop: %+v, %v", feedback, err)
			}
			select {
			case got := <-done:
				t.Fatalf("disconnect completed before ACK: %+v", got)
			default:
			}
			if disposition == "lost-socket" {
				_ = client.Close()
			} else {
				status := byte(productionBrokerAccepted)
				if disposition == "rejected" {
					status = productionBrokerRejected
				}
				if disposition == "wrong-correlation" {
					frame.correlation++
				}
				if err := writer.write(productionBrokerCanonicalAck, frame.correlation, []byte{status}); err != nil {
					t.Fatal(err)
				}
			}
			got := <-done
			if disposition == "accepted" {
				if got.err != nil || got.value.State != retainedusb.ImportDisconnectSafe {
					t.Fatalf("acknowledged Stop: %+v", got)
				}
			} else if got.err == nil || got.value.State != retainedusb.ImportDisconnectQuarantined {
				t.Fatalf("unproven Stop became safe: %+v", got)
			}
			if err := adapter.ReconnectDormantSession(newScriptedControllerPersonaLocalExecutor(), time.Second, time.Now()); err == nil {
				t.Fatal("overflow incarnation reconnected")
			}
		})
	}
}

func TestRetainedInputRetirementKeepsAlreadyAdmittedCompletionAccountable(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "delivered"}[delivered], func(t *testing.T) {
			adapter, executor := startRetainedJournalTestPersona(t)
			retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
			permitRetainedJournalTestInput(t, adapter, executor)
			if err := adapter.PublishSemanticInput(41, 2, GamepadInputReportV1{State: InputStateV1{A: true}}); err != nil {
				t.Fatal(err)
			}
			ticket, err := adapter.Stage(retainedINRequest(41, 10, 10, 64))
			if err != nil {
				t.Fatal(err)
			}
			var scratch [64]byte
			if p, err := adapter.Prepare(ticket, scratch[:], adapterTestTime(10)); err != nil || p.Result != retainedusb.ResultData {
				t.Fatalf("prepare: %+v, %v", p, err)
			}
			var fault error
			for revision := uint64(3); revision < 200; revision++ {
				fault = adapter.PublishSemanticInput(41, revision, GamepadInputReportV1{State: InputStateV1{A: revision%2 == 0}})
				if fault != nil {
					break
				}
			}
			if !errors.Is(fault, errRetainedInputHistoryFault) {
				t.Fatalf("fault: %v", fault)
			}
			if err := adapter.Complete(ticket, delivered, adapterTestTime(11)); err != nil {
				t.Fatalf("admitted completion after fault: %v", err)
			}
			if err := adapter.Complete(ticket, delivered, adapterTestTime(11)); err == nil {
				t.Fatal("duplicate completion accepted")
			}
			if request, err := adapter.RetainedImportRetirement(); err != nil || !request.Valid() {
				t.Fatalf("retirement after completion: %+v, %v", request, err)
			}
			if err := adapter.coordinator.inputJournal.complete(delivered); err == nil {
				t.Fatal("unowned journal completion accepted")
			}
		})
	}
}

func TestProductionInputRetirementDrainDeadlineDoesNotExtendWithTraffic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		device, client, writer := startProductionRetirementTest(t)
		revision := overflowProductionRetirementTest(t, device, client, writer)
		if err := client.SetDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
		var wire [SemanticInputWireSize]byte
		_ = EncodeSemanticInputWireV1Into(wire[:], InputStateV1{})
		var scratch [productionBrokerMaximumPayload]byte
		for i := 0; i < 2; i++ {
			time.Sleep(10 * time.Second)
			revision++
			if err := writer.write(productionBrokerSemanticInput, revision, wire[:]); err != nil {
				t.Fatal(err)
			}
			frame, err := readProductionBrokerFrame(client, scratch[:])
			if err != nil || frame.typeID != productionBrokerSemanticInputAck || frame.payload[0] != productionBrokerRejected {
				t.Fatalf("draining rejection: %+v, %v", frame, err)
			}
		}
		time.Sleep(11 * time.Second)
		synctest.Wait()
		if _, err := readProductionBrokerFrame(client, scratch[:]); err == nil {
			t.Fatal("traffic extended the absolute teardown deadline")
		}
		if _, ok := device.acquireBrokerStream(); ok {
			t.Fatal("deadline expiry reopened the failed incarnation")
		}
		if device.adapter.state == dormantRetainedUSBDisconnected {
			t.Fatal("deadline expiry was mistaken for an acknowledged Stop")
		}
	})
}

func TestRetainedInputRetirementFatalOwnershipStillWins(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
	permitRetainedJournalTestInput(t, adapter, executor)
	for revision := uint64(2); revision < 200; revision++ {
		if err := adapter.PublishSemanticInput(41, revision, GamepadInputReportV1{State: InputStateV1{A: revision%2 == 0}}); err != nil {
			if !errors.Is(err, errRetainedInputHistoryFault) {
				t.Fatal(err)
			}
			break
		}
	}
	if request, err := adapter.RetainedImportRetirement(); err != nil || !request.Valid() {
		t.Fatalf("missing initial retirement: %+v, %v", request, err)
	}
	cause := errors.New("ambiguous feedback must remain fatal")
	adapter.markLocalFatal(cause)
	if request, err := adapter.RetainedImportRetirement(); !errors.Is(err, cause) || request.Valid() {
		t.Fatalf("fatal downgraded to retirement: %+v, %v", request, err)
	}
	adapter.mu.Lock()
	adapter.quarantineLocked(cause)
	adapter.mu.Unlock()
	if request, err := adapter.RetainedImportRetirement(); !errors.Is(err, errDormantRetainedUSBQuarantined) || request.Valid() {
		t.Fatalf("quarantine downgraded to retirement: %+v, %v", request, err)
	}
}

func TestRetainedInputRetirementFencesAllLanesAndReset(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
	permitRetainedJournalTestInput(t, adapter, executor)
	for revision := uint64(2); revision < 200; revision++ {
		epoch, ready := adapter.Readiness()
		select {
		case <-ready:
		default:
		}
		err := adapter.PublishSemanticInput(41, revision, GamepadInputReportV1{State: InputStateV1{A: revision%2 == 0}})
		if err != nil {
			if !errors.Is(err, errRetainedInputHistoryFault) {
				t.Fatal(err)
			}
			if after, _ := adapter.Readiness(); after <= epoch {
				t.Fatal("fault did not advance readiness")
			}
			select {
			case <-ready:
			default:
				t.Fatal("fault did not signal readiness")
			}
			break
		}
	}
	request, err := adapter.RetainedImportRetirement()
	if err != nil || !request.Valid() {
		t.Fatalf("retirement: %+v, %v", request, err)
	}
	for _, envelope := range []retainedusb.Request{
		retainedINRequest(41, 10, 10, 64),
		retainedControlRequest(41, 11, 11, testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration, 0, 0, 0)),
		retainedOUTRequest(41, 12, 12, []byte{0x05, 0x20, 0x03, 0x01, byte(SetDeviceStateStop)}),
	} {
		ticket, err := adapter.Stage(envelope)
		if err != nil {
			t.Fatal(err)
		}
		var scratch [64]byte
		for i := range scratch {
			scratch[i] = 0xa5
		}
		p, err := adapter.Prepare(ticket, scratch[:], adapterTestTime(12))
		if err != nil || p.Result != retainedusb.ResultPending || p.ActualLength != 0 {
			t.Fatalf("lane %v admitted after retirement: %+v, %v", envelope.Lane, p, err)
		}
		for _, b := range scratch {
			if b != 0xa5 {
				t.Fatal("retirement copied report bytes")
			}
		}
		if err := adapter.Retire(ticket, retainedusb.RetireOwnerRequested, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := adapter.FenceAndDrainReset(adapterTestReset(request.Lease, 1, 1), time.Now().Add(time.Second)); !errors.Is(err, errRetainedInputHistoryFault) {
		t.Fatalf("reset bypassed retirement: %v", err)
	}
	if after, err := adapter.RetainedImportRetirement(); err != nil || after != request {
		t.Fatalf("retirement changed: %+v, %v", after, err)
	}
}

package xboxone

import (
	"bytes"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
)

func startRetainedJournalTestPersona(t *testing.T) (*DormantRetainedUSBAdapter, *scriptedControllerPersonaLocalExecutor) {
	t.Helper()
	adapter, executor, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	configureRetainedTestPersona(t, adapter)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	retainedPrepareAndComplete(t, adapter, retainedOUTRequest(41, 4, 4,
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)}), 2)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 5, 5, 64), 3)
	return adapter, executor
}

func permitRetainedJournalTestInput(t *testing.T, adapter *DormantRetainedUSBAdapter, executor *scriptedControllerPersonaLocalExecutor) {
	t.Helper()
	triggerAndRetireLocalAction(t, adapter, 7, 5)
	waitForLocalExecution(t, executor, ControllerPersonaPermitNormalUpstream)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snapshot, _ := adapter.coordinator.snapshot()
		if snapshot.NormalUpstream && !snapshot.ClaimOutstanding {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("normal input permission did not complete")
}

func readRetainedJournalTestInput(t *testing.T, adapter *DormantRetainedUSBAdapter, ordinal uint64) GamepadInputReportV1 {
	t.Helper()
	_, wire := retainedPrepareAndComplete(t, adapter,
		retainedINRequest(41, ordinal, uint32(ordinal), 64), ordinal)
	_, report, err := DecodeGamepadInputMessage(wire)
	if err != nil {
		t.Fatalf("input decode: %v, wire=% x", err, wire)
	}
	return report
}

func TestRetainedInputJournalPreservesButtonAndTriggerBeforePoll(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
	permitRetainedJournalTestInput(t, adapter, executor)
	states := []GamepadInputReportV1{
		{State: InputStateV1{A: true}},
		{},
		{State: InputStateV1{LeftTrigger: 1}},
		{State: InputStateV1{LeftTrigger: 1023}},
		{},
	}
	for i, state := range states {
		if err := adapter.PublishSemanticInput(41, uint64(i+2), state); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range states {
		if got := readRetainedJournalTestInput(t, adapter, uint64(i+10)); got != want {
			t.Fatalf("transition %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestRetainedInputJournalStartSelectionRetainsLaterTransitions(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	// Pre-START history collapses to the current image at selection, not at
	// completion. Publications during the write belong after that image.
	for i, state := range []GamepadInputReportV1{{State: InputStateV1{A: true}}, {}} {
		if err := adapter.PublishSemanticInput(41, uint64(i+2), state); err != nil {
			t.Fatal(err)
		}
	}
	ticket, err := adapter.Stage(retainedINRequest(41, 6, 6, 64))
	if err != nil {
		t.Fatal(err)
	}
	var wire [64]byte
	prepared, err := adapter.Prepare(ticket, wire[:], adapterTestTime(4))
	if err != nil || prepared.Result != retainedusb.ResultData {
		t.Fatalf("START input = %+v, %v", prepared, err)
	}
	_, initial, err := DecodeGamepadInputMessage(wire[:prepared.ActualLength])
	if err != nil || initial != (GamepadInputReportV1{}) {
		t.Fatalf("START replayed pre-selection history: %+v, %v", initial, err)
	}
	press := GamepadInputReportV1{State: InputStateV1{B: true}}
	if err := adapter.PublishSemanticInput(41, 4, press); err != nil {
		t.Fatal(err)
	}
	if err := adapter.PublishSemanticInput(41, 5, GamepadInputReportV1{}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Complete(ticket, true, adapterTestTime(4)); err != nil {
		t.Fatal(err)
	}
	permitRetainedJournalTestInput(t, adapter, executor)
	if got := readRetainedJournalTestInput(t, adapter, 10); got != press {
		t.Fatalf("lost transition during START write: %+v", got)
	}
	if got := readRetainedJournalTestInput(t, adapter, 11); got != (GamepadInputReportV1{}) {
		t.Fatalf("lost release during START write: %+v", got)
	}
}

func TestRetainedInputJournalRetryKeepsExactBytesAndFollowingRelease(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
	permitRetainedJournalTestInput(t, adapter, executor)
	press := GamepadInputReportV1{State: InputStateV1{X: true, RightTrigger: 901}}
	if err := adapter.PublishSemanticInput(41, 2, press); err != nil {
		t.Fatal(err)
	}
	ticket, err := adapter.Stage(retainedINRequest(41, 10, 10, 64))
	if err != nil {
		t.Fatal(err)
	}
	var wire [64]byte
	prepared, err := adapter.Prepare(ticket, wire[:], adapterTestTime(10))
	if err != nil || prepared.Result != retainedusb.ResultData {
		t.Fatalf("prepare = %+v, %v", prepared, err)
	}
	if err := adapter.Complete(ticket, false, adapterTestTime(10)); err != nil {
		t.Fatal(err)
	}
	for i, state := range []GamepadInputReportV1{{}, {State: InputStateV1{Y: true}}, {}} {
		if err := adapter.PublishSemanticInput(41, uint64(i+3), state); err != nil {
			t.Fatal(err)
		}
	}
	_, retry := retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 11, 11, 64), 11)
	if !bytes.Equal(retry, wire[:prepared.ActualLength]) {
		t.Fatalf("retry changed: % x -> % x", wire[:prepared.ActualLength], retry)
	}
	for i, want := range []GamepadInputReportV1{{}, {State: InputStateV1{Y: true}}, {}} {
		if got := readRetainedJournalTestInput(t, adapter, uint64(i+12)); got != want {
			t.Fatalf("post-retry transition %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestRetainedInputJournalEveryOrdinaryButtonAndShare(t *testing.T) {
	engine, _, _ := makeOfficialInputPersonaActive(t, OfficialGamepadMetadataConsoleFunctionMap, GamepadInputReportV1{})
	// Test the same retained coordinator/USB completion path with the official
	// Share extension. Engine initialization was completed before transfer of
	// ownership; no second writer ever mutates it after adapter construction.
	executor := newScriptedControllerPersonaLocalExecutor()
	adapter, err := NewDormantRetainedUSBAdapter(engine, 11, 22, executor, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// BindImport expects a newly addressed/configurable persona. Exercise the
	// coordinator here, whose production adapter has already installed the
	// identical semantic owner, without inventing a second USB configuration.
	journal := adapter.coordinator.inputJournal
	if _, _, err := journal.selectReport(true); err != nil {
		t.Fatal(err)
	}
	if err := journal.admit(); err != nil {
		t.Fatal(err)
	}
	if err := journal.complete(true); err != nil {
		t.Fatal(err)
	}
	var want []GamepadInputReportV1
	for bit := uint(0); bit < 16; bit++ {
		state := decodeSemanticInputButtons(1 << bit)
		if state.Guide {
			continue
		}
		want = append(want, GamepadInputReportV1{State: state}, GamepadInputReportV1{})
	}
	for _, state := range want {
		if err := journal.publish(state); err != nil {
			t.Fatal(err)
		}
	}
	var scratch [64]byte
	for i, state := range want {
		coordinator := adapter.coordinator
		ticket, err := coordinator.stageInterruptIn(64)
		if err != nil {
			t.Fatal(err)
		}
		admission, err := coordinator.admitInput(ticket, scratch[:], uint64(i+10), GamepadInputReportV1{})
		if err != nil || admission.disposition != controllerPersonaTransportInterruptInResponse {
			t.Fatalf("input %d admission = %+v, %v", i, admission, err)
		}
		_, got, err := DecodeConsoleFunctionMapGamepadInputMessage(scratch[:admission.size])
		if err != nil || got != state {
			t.Fatalf("input %d = %+v, %v; want %+v", i, got, err, state)
		}
		if err := coordinator.completeResponse(ticket, true, uint64(i+10)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetainedInputJournalCoalescesContinuousMotion(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
	permitRetainedJournalTestInput(t, adapter, executor)
	var latest GamepadInputReportV1
	for i := uint64(2); i < 1002; i++ {
		latest = GamepadInputReportV1{State: InputStateV1{LeftStickX: int16(i), RightStickY: -int16(i)}}
		if err := adapter.PublishSemanticInput(41, i, latest); err != nil {
			t.Fatal(err)
		}
	}
	if got := readRetainedJournalTestInput(t, adapter, 10); got != latest {
		t.Fatalf("continuous motion was queued or stale: %+v, want %+v", got, latest)
	}
	if snapshot := adapter.coordinator.inputJournal.source.Snapshot(); snapshot.TransitionDepth != 0 || snapshot.ContinuousPending {
		t.Fatalf("continuous backlog: %+v", snapshot)
	}
}

func TestRetainedInputJournalOverflowFencesRetryAndBrokerReconnect(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
	permitRetainedJournalTestInput(t, adapter, executor)
	// Use the real broker lease path, while keeping this test's physical sink
	// scripted. The publication failure must make this incarnation terminal.
	device, _ := testProductionRetainedUSBDevice(t)
	device.adapter = adapter
	lease, ok := device.acquireBrokerStream()
	if !ok {
		t.Fatal("broker acquire failed")
	}
	defer lease.release()
	if err := lease.consumerReady(); err != nil {
		t.Fatal(err)
	}
	var wire [SemanticInputWireSize]byte
	if err := EncodeSemanticInputWireV1Into(wire[:], InputStateV1{A: true}); err != nil {
		t.Fatal(err)
	}
	if err := lease.publishInput(2, wire[:]); err != nil {
		t.Fatal(err)
	}
	ticket, err := adapter.Stage(retainedINRequest(41, 10, 10, 64))
	if err != nil {
		t.Fatal(err)
	}
	var scratch [64]byte
	if p, err := adapter.Prepare(ticket, scratch[:], adapterTestTime(10)); err != nil || p.Result != retainedusb.ResultData {
		t.Fatalf("prepare = %+v, %v", p, err)
	}
	if err := adapter.Complete(ticket, false, adapterTestTime(10)); err != nil {
		t.Fatal(err)
	}
	var fault error
	for revision := uint64(3); revision < 200; revision++ {
		if err := EncodeSemanticInputWireV1Into(wire[:], InputStateV1{A: revision%2 == 0}); err != nil {
			t.Fatal(err)
		}
		fault = lease.publishInput(revision, wire[:])
		if fault != nil {
			break
		}
	}
	if !errors.Is(fault, errRetainedInputHistoryFault) {
		t.Fatalf("missing terminal history fault: %v", fault)
	}
	if _, ok := device.acquireBrokerStream(); ok {
		t.Fatal("faulted incarnation accepted a replacement broker")
	}
	ticket, err = adapter.Stage(retainedINRequest(41, 11, 11, 64))
	if err != nil {
		t.Fatal(err)
	}
	for i := range scratch {
		scratch[i] = 0xa5
	}
	if p, err := adapter.Prepare(ticket, scratch[:], adapterTestTime(11)); err != nil || p.Result != retainedusb.ResultPending {
		t.Fatalf("revoked retry must await exact retirement: %+v, %v", p, err)
	}
	if retirement, err := adapter.RetainedImportRetirement(); err != nil || !retirement.Valid() ||
		retirement.Lease != adapter.boundLease || retirement.Reason != retainedusb.ImportRetirementInputHistoryOverflow {
		t.Fatalf("missing exact retirement request: %+v, %v", retirement, err)
	}
	for _, b := range scratch {
		if b != 0xa5 {
			t.Fatal("revoked retry exposed bytes")
		}
	}
}

func TestRetainedInputJournalWarmedPublishThroughUSBCompletionAllocatesZero(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
	permitRetainedJournalTestInput(t, adapter, executor)
	var scratch [64]byte
	revision, ordinal := uint64(1), uint64(10)
	allocations := testing.AllocsPerRun(1000, func() {
		revision++
		ordinal++
		if err := adapter.PublishSemanticInput(41, revision, GamepadInputReportV1{State: InputStateV1{A: revision%2 == 0}}); err != nil {
			panic(err)
		}
		ticket, err := adapter.Stage(retainedINRequest(41, ordinal, uint32(ordinal), 64))
		if err != nil {
			panic(err)
		}
		p, err := adapter.Prepare(ticket, scratch[:], adapterTestTime(ordinal))
		if err != nil || p.Result != retainedusb.ResultData {
			panic("input not admitted")
		}
		if err := adapter.Complete(ticket, true, adapterTestTime(ordinal)); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("input hot path allocated %.2f times per cycle", allocations)
	}
}

func TestRetainedInputJournalConcurrentPublicationAndUSBDelivery(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
	permitRetainedJournalTestInput(t, adapter, executor)
	consumed := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan error, 1)
	defer close(stop)
	go func() {
		revision := uint64(1)
		for batch := int16(1); batch <= 100; batch++ {
			for _, down := range []bool{true, false} {
				revision++
				if err := adapter.PublishSemanticInput(41, revision,
					GamepadInputReportV1{State: InputStateV1{A: down, LeftStickX: batch}}); err != nil {
					done <- err
					return
				}
			}
			select {
			case <-consumed:
			case <-stop:
				done <- nil
				return
			}
		}
		done <- nil
	}()
	deadline := time.Now().Add(3 * time.Second)
	batch, pressed, ordinal := int16(1), false, uint64(10)
	var ticket retainedusb.Ticket
	var scratch [64]byte
	for batch <= 100 && time.Now().Before(deadline) {
		if !ticket.Valid() {
			var err error
			ticket, err = adapter.Stage(retainedINRequest(41, ordinal, uint32(ordinal), 64))
			if err != nil {
				t.Fatal(err)
			}
			ordinal++
		}
		prepared, err := adapter.Prepare(ticket, scratch[:], adapterTestTime(10))
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Result == retainedusb.ResultPending {
			// If input arrived after the empty decision, its epoch must still
			// be newer. Snapshot publication and epoch under its actual mutex.
			adapter.mu.Lock()
			queued := adapter.coordinator.inputJournal.source.HasPendingInputPresentation()
			epoch := adapter.readinessEpoch
			adapter.mu.Unlock()
			if queued && epoch <= prepared.ReadinessEpoch {
				t.Fatal("concurrent publication lost its wake while IN parked")
			}
			runtime.Gosched()
			continue
		}
		_, got, err := DecodeGamepadInputMessage(scratch[:prepared.ActualLength])
		if err != nil {
			t.Fatal(err)
		}
		if err := adapter.Complete(ticket, true, adapterTestTime(10)); err != nil {
			t.Fatal(err)
		}
		ticket = retainedusb.Ticket{}
		if got.State.LeftStickX < batch { // unchanged idle predecessor
			runtime.Gosched()
			continue
		}
		if got.State.LeftStickX != batch {
			t.Fatalf("reordered transition: got batch %d, want %d", got.State.LeftStickX, batch)
		}
		if got.State.A {
			pressed = true
		} else {
			if !pressed {
				t.Fatalf("release overtook press in batch %d", batch)
			}
			pressed = false
			batch++
			consumed <- struct{}{}
		}
	}
	if batch != 101 {
		t.Fatalf("concurrent delivery stopped at batch %d", batch)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

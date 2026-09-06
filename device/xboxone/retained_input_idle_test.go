package xboxone

import (
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
)

func TestRetainedInputJournalIdleParksUntilChange(t *testing.T) {
	adapter := activeRetainedJournalLifecyclePersona(t)
	ticket, err := adapter.Stage(retainedINRequest(41, 10, 10, 64))
	if err != nil {
		t.Fatal(err)
	}
	var scratch [64]byte
	before := adapter.coordinator.inputJournal.source.Snapshot()
	now := adapterTestTime(10)
	prepared, err := adapter.Prepare(ticket, scratch[:], now)
	if err != nil || prepared.Result != retainedusb.ResultPending || prepared.RetryAt.Sub(now) < 900*time.Millisecond {
		t.Fatalf("idle input should park on readiness, not repeat reports or poll: %+v, %v", prepared, err)
	}
	if after := adapter.coordinator.inputJournal.source.Snapshot(); after.Selected != before.Selected {
		t.Fatal("idle poll selected an unchanged semantic claim")
	}
	press := GamepadInputReportV1{State: InputStateV1{A: true}}
	if err := adapter.PublishSemanticInput(41, 2, press); err != nil {
		t.Fatal(err)
	}
	epoch, _ := adapter.Readiness()
	if epoch <= prepared.ReadinessEpoch {
		t.Fatal("new input did not wake the parked request")
	}
	prepared, err = adapter.Prepare(ticket, scratch[:], adapterTestTime(11))
	if err != nil || prepared.Result != retainedusb.ResultData {
		t.Fatalf("parked input was not immediately available after publication: %+v, %v", prepared, err)
	}
	_, got, err := DecodeGamepadInputMessage(scratch[:prepared.ActualLength])
	if err != nil || got != press {
		t.Fatalf("input = %+v, %v", got, err)
	}
	if err := adapter.Complete(ticket, true, adapterTestTime(11)); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedInputJournalIdenticalPublicationDoesNotCreateUSBReport(t *testing.T) {
	adapter := activeRetainedJournalLifecyclePersona(t)
	press := GamepadInputReportV1{State: InputStateV1{A: true, LeftStickX: 99}}
	if err := adapter.PublishSemanticInput(41, 2, press); err != nil {
		t.Fatal(err)
	}
	if got := readRetainedJournalTestInput(t, adapter, 10); got != press {
		t.Fatalf("press = %+v", got)
	}
	if err := adapter.PublishSemanticInput(41, 3, press); err != nil {
		t.Fatal(err)
	}
	ticket, err := adapter.Stage(retainedINRequest(41, 11, 11, 64))
	if err != nil {
		t.Fatal(err)
	}
	var scratch [64]byte
	now := adapterTestTime(11)
	p, err := adapter.Prepare(ticket, scratch[:], now)
	if err != nil || p.Result != retainedusb.ResultPending || p.RetryAt.Sub(now) < 900*time.Millisecond {
		t.Fatalf("identical state did not stay parked: %+v, %v", p, err)
	}
	if err := adapter.Retire(ticket, retainedusb.RetireUnlink, adapterTestTime(11)); err != nil {
		t.Fatal(err)
	}
	// An explicit KeepAlive publication is an intentional report request,
	// even when its controller state and flag equal the preceding report.
	press.KeepAlive = true
	for revision := uint64(4); revision <= 5; revision++ {
		if err := adapter.PublishSemanticInput(41, revision, press); err != nil {
			t.Fatal(err)
		}
		if got := readRetainedJournalTestInput(t, adapter, revision+10); got != press {
			t.Fatalf("explicit KeepAlive = %+v", got)
		}
	}
}

func TestRetainedInputJournalIdleWakesForProtocolStatusDeadline(t *testing.T) {
	adapter := activeRetainedJournalLifecyclePersona(t)
	ticket, err := adapter.Stage(retainedINRequest(41, 10, 10, 64))
	if err != nil {
		t.Fatal(err)
	}
	var scratch [64]byte
	before := adapter.coordinator.inputJournal.source.Snapshot()
	p, err := adapter.Prepare(ticket, scratch[:], adapter.clockOrigin.Add(20*time.Millisecond))
	if err != nil || p.Result != retainedusb.ResultPending || p.RetryAt.IsZero() {
		t.Fatalf("idle did not retain periodic protocol deadline: %+v, %v", p, err)
	}
	deadline := p.RetryAt
	p, err = adapter.Prepare(ticket, scratch[:], deadline.Add(-time.Millisecond))
	if err != nil || p.Result != retainedusb.ResultPending {
		t.Fatalf("periodic status was early: %+v, %v", p, err)
	}
	p, err = adapter.Prepare(ticket, scratch[:], deadline)
	if err != nil || p.Result != retainedusb.ResultData {
		t.Fatalf("timer-only status was unavailable: %+v, %v", p, err)
	}
	if _, _, err := DecodeExtendedStatusNoEventsMessage(scratch[:p.ActualLength]); err != nil {
		t.Fatalf("idle deadline produced non-status bytes: % x, %v", scratch[:p.ActualLength], err)
	}
	if err := adapter.Complete(ticket, true, deadline); err != nil {
		t.Fatal(err)
	}
	if after := adapter.coordinator.inputJournal.source.Snapshot(); after.Selected != before.Selected {
		t.Fatal("status consumed an ordinary input claim")
	}
	// Subsequent input must still be able to wake before the next status tick.
	press := GamepadInputReportV1{State: InputStateV1{X: true}}
	if err := adapter.PublishSemanticInput(41, 2, press); err != nil {
		t.Fatal(err)
	}
	ticket, err = adapter.Stage(retainedINRequest(41, 11, 11, 64))
	if err != nil {
		t.Fatal(err)
	}
	p, err = adapter.Prepare(ticket, scratch[:], deadline.Add(time.Millisecond))
	if err != nil || p.Result != retainedusb.ResultData {
		t.Fatalf("status delayed a new input: %+v, %v", p, err)
	}
	_, got, err := DecodeGamepadInputMessage(scratch[:p.ActualLength])
	if err != nil || got != press {
		t.Fatalf("input following status: %+v, %v", got, err)
	}
	if err := adapter.Complete(ticket, true, deadline.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
}

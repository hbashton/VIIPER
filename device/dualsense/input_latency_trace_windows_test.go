//go:build windows && viiper_latency

package dualsense

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
	"github.com/Alia5/VIIPER/internal/testsupport/inputlatency"
)

func TestTaggedLatencyHookCorrelatesOnlyAcceptedTerminalCommit(t *testing.T) {
	session, err := inputlatency.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	dev := newInputPresentationTestDevice(t)

	receivedAt := dev.input.timestampBase.Add(time.Second)
	brokerTicks := inputLatencyCounter()
	traceInputBrokerReceived(dev, 17, receivedAt, brokerTicks)
	state := neutralInputState()
	state.Buttons = ButtonCross
	if !dev.input.updateAt(&state, 0, receivedAt) {
		t.Fatal("press was not published")
	}
	var report [InputReportSize]byte
	claim := dev.ClaimInputPresentation(report[:], receivedAt.Add(time.Millisecond))
	if !claim.Valid() {
		t.Fatalf("invalid claim: %+v", claim)
	}
	if !dev.ResolveInputPresentation(
		claim, inputpresentation.OutcomeCommit, receivedAt.Add(2*time.Millisecond)) {
		t.Fatal("commit was not accepted")
	}
	record, err := session.Await(context.Background(), 17)
	if err != nil {
		t.Fatal(err)
	}
	if record.BrokerReceivedTicks != brokerTicks ||
		record.TransportAdmittedTicks <= brokerTicks ||
		record.PresentationToken != claim.Token ||
		record.PresentationGeneration != claim.Generation {
		t.Fatalf("unexpected admission: %+v claim=%+v", record, claim)
	}

	deferredAt := receivedAt.Add(3 * time.Millisecond)
	traceInputBrokerReceived(dev, 18, deferredAt, inputLatencyCounter())
	state.Buttons = 0
	if !dev.input.updateAt(&state, 0, deferredAt) {
		t.Fatal("release was not published")
	}
	claim = dev.ClaimInputPresentation(report[:], deferredAt.Add(time.Millisecond))
	if !claim.Valid() || !dev.ResolveInputPresentation(
		claim, inputpresentation.OutcomeDefer, deferredAt.Add(2*time.Millisecond)) {
		t.Fatal("defer was not accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err = session.Await(ctx, 18); err == nil ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deferred claim produced terminal admission: %v", err)
	}
}

func TestTaggedLatencyHookCapturesActualV5ReaderBoundary(t *testing.T) {
	dev, client, readerDone := newV5ReaderTest(t)
	defer func() {
		_ = client.Close()
		select {
		case err := <-readerDone:
			if err != nil {
				t.Errorf("V5 reader shutdown: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("V5 reader did not stop")
		}
	}()
	session, err := inputlatency.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	state := NewInputState()
	state.Buttons = ButtonCross
	payload, err := state.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Write(makeV5StreamFrame(
		StreamFrameInputState, 0, payload)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for dev.InputSchedulerState().Received == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if dev.InputSchedulerState().Received != 1 {
		t.Fatalf("V5 frame was not published: %+v", dev.InputSchedulerState())
	}

	var report [InputReportSize]byte
	claim := dev.ClaimInputPresentation(report[:], time.Now())
	if !claim.Valid() || !dev.ResolveInputPresentation(
		claim, inputpresentation.OutcomeCommit, time.Now()) {
		t.Fatalf("V5 claim did not commit: %+v", claim)
	}
	record, err := session.Await(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !record.ReceivedAt.Equal(claim.ReceivedAt) ||
		record.BrokerReceivedTicks <= 0 ||
		record.TransportAdmittedTicks <= record.BrokerReceivedTicks {
		t.Fatalf("actual reader correlation failed: record=%+v claim=%+v",
			record, claim)
	}
}

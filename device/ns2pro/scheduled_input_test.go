package ns2pro

import (
	"context"
	"testing"
	"time"
)

func TestScheduledInterruptInputPreservesSwitchStateAndDeadlineReplay(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	dev.protoMu.Lock()
	dev.usbReportsEnabled = true
	dev.protoMu.Unlock()
	state := *NewInputState()
	state.Buttons, state.LX = ButtonA|ButtonHome, 0x321
	dev.UpdateInputState(state)
	buffer := make([]byte, InputReportSize)
	never := make(chan time.Time)
	if written, readErr := dev.ReadScheduledInterruptInput(context.Background(), never, EndpointHIDIn&0x0f, buffer); readErr != nil || written != InputReportSize {
		t.Fatalf("event read=(%d, %v)", written, readErr)
	}
	if buffer[0] != ReportIDPro {
		t.Fatalf("event report ID=%02x", buffer[0])
	}
	if buffer[1] != 1 {
		t.Fatalf("event counter=%d want=1", buffer[1])
	}
	deadline := make(chan time.Time, 1)
	deadline <- time.Now()
	if written, readErr := dev.ReadScheduledInterruptInput(context.Background(), deadline, EndpointHIDIn&0x0f, buffer); readErr != nil || written != InputReportSize {
		t.Fatalf("deadline read=(%d, %v)", written, readErr)
	}
	if buffer[0] != ReportIDPro {
		t.Fatalf("deadline report ID=%02x", buffer[0])
	}
	if buffer[1] != 2 {
		t.Fatalf("deadline counter=%d want=2", buffer[1])
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state.LX = 0x654
	dev.UpdateInputState(state)
	readyDeadline := make(chan time.Time, 1)
	readyDeadline <- time.Now()
	if _, err = dev.ReadScheduledInterruptInput(ctx, readyDeadline, EndpointHIDIn&0x0f, buffer); err != context.Canceled {
		t.Fatalf("lifecycle cancellation=%v want %v", err, context.Canceled)
	}
	if written, readErr := dev.ReadScheduledInterruptInput(context.Background(), never, EndpointHIDIn&0x0f, buffer); readErr != nil || written != InputReportSize {
		t.Fatalf("post-cancel event read=(%d, %v)", written, readErr)
	}
	if buffer[1] != 3 {
		t.Fatalf("post-cancel counter=%d want=3", buffer[1])
	}
}

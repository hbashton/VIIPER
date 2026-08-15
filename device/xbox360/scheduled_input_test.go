package xbox360

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestScheduledInterruptInputPreservesXboxStateAndDeadlineReplay(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := *NewInputState()
	state.Buttons, state.LT, state.RX = 0x1234, 199, -4567
	dev.UpdateInputState(state)
	buffer := make([]byte, 32)
	never := make(chan time.Time)
	for index, want := range nativeDataInitializationReports {
		written, transition, readErr := dev.ReadClassifiedScheduledInterruptInput(
			context.Background(), never, 1, buffer)
		if readErr != nil || !transition || written != len(want) {
			t.Fatalf("initialization report %d=(%d, %v, %v)", index, written, transition, readErr)
		}
		if got := buffer[:written]; string(got) != string(want) {
			t.Fatalf("initialization report %d=%x want=%x", index, got, want)
		}
	}
	if written, readErr := dev.ReadScheduledInterruptInput(context.Background(), never, 1, buffer); readErr != nil || written != 20 {
		t.Fatalf("event read=(%d, %v)", written, readErr)
	}
	if buffer[4] != state.LT {
		t.Fatalf("event state=%x", buffer)
	}
	deadline := make(chan time.Time, 1)
	deadline <- time.Now()
	if written, readErr := dev.ReadScheduledInterruptInput(context.Background(), deadline, 1, buffer); readErr != nil || written != 20 {
		t.Fatalf("deadline read=(%d, %v)", written, readErr)
	}
	if buffer[4] != state.LT {
		t.Fatalf("deadline state=%x", buffer)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state.LT = 211
	dev.UpdateInputState(state)
	readyDeadline := make(chan time.Time, 1)
	readyDeadline <- time.Now()
	if _, err = dev.ReadScheduledInterruptInput(ctx, readyDeadline, 1, buffer); err != context.Canceled {
		t.Fatalf("lifecycle cancellation=%v want %v", err, context.Canceled)
	}
	if written, readErr := dev.ReadScheduledInterruptInput(context.Background(), never, 1, buffer); readErr != nil || written != 20 {
		t.Fatalf("post-cancel event read=(%d, %v)", written, readErr)
	}
	if buffer[4] != state.LT {
		t.Fatalf("post-cancel state=%x", buffer)
	}
}

func TestNativeInterruptEndpointSelectionAndControlInitialization(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	for endpoint := uint32(1); endpoint <= 4; endpoint++ {
		want := endpoint == 1 || endpoint == 3
		if got := dev.SupportsInterruptInputEndpoint(endpoint); got != want {
			t.Fatalf("endpoint %d support=%v want=%v", endpoint, got, want)
		}
	}
	buffer := make([]byte, 32)
	never := make(chan time.Time)
	written, transition, err := dev.ReadClassifiedScheduledInterruptInput(
		context.Background(), never, 3, buffer)
	if err != nil || !transition || written != len(nativeControlInitializationReport) {
		t.Fatalf("control initialization=(%d, %v, %v)", written, transition, err)
	}
	if got := buffer[:written]; string(got) != string(nativeControlInitializationReport[:]) {
		t.Fatalf("control initialization=%x want=%x", got, nativeControlInitializationReport)
	}
	deadline := make(chan time.Time, 1)
	deadline <- time.Now()
	if _, _, err = dev.ReadClassifiedScheduledInterruptInput(
		context.Background(), deadline, 3, buffer); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("idle control endpoint error=%v want deadline exceeded", err)
	}
}

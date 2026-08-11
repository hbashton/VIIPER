package xbox360

import (
	"context"
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
	buffer := make([]byte, 20)
	never := make(chan time.Time)
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

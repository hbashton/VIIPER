package mouse

import (
	"context"
	"testing"
	"time"
)

func TestScheduledInterruptInputPreservesMouseEventAndZeroingContract(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := *NewInputState()
	state.Buttons, state.DX, state.DY = 3, 120, -45
	dev.UpdateInputState(state)
	buffer := make([]byte, 9)
	never := make(chan time.Time)
	if written, readErr := dev.ReadScheduledInterruptInput(context.Background(), never, 1, buffer); readErr != nil || written != 9 {
		t.Fatalf("event read=(%d, %v)", written, readErr)
	}
	if buffer[0] != state.Buttons || buffer[1] != byte(state.DX) {
		t.Fatalf("event state=%x", buffer)
	}
	// Relative movement is emitted once, then the device's queued zero-delta
	// state is preserved exactly as on the original context-deadline path.
	if _, readErr := dev.ReadScheduledInterruptInput(context.Background(), never, 1, buffer); readErr != nil {
		t.Fatal(readErr)
	}
	if buffer[0] != state.Buttons || buffer[1] != 0 || buffer[3] != 0 {
		t.Fatalf("zeroed relative state=%x", buffer)
	}
	deadline := make(chan time.Time, 1)
	deadline <- time.Now()
	if _, err = dev.ReadScheduledInterruptInput(context.Background(), deadline, 1, buffer); err != context.DeadlineExceeded {
		t.Fatalf("idle deadline=%v want %v", err, context.DeadlineExceeded)
	}

	state.DX, state.DY = -321, 123
	dev.UpdateInputState(state)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	readyDeadline := make(chan time.Time, 1)
	readyDeadline <- time.Now()
	if _, err = dev.ReadScheduledInterruptInput(ctx, readyDeadline, 1, buffer); err != context.Canceled {
		t.Fatalf("lifecycle cancellation=%v want %v", err, context.Canceled)
	}
	if written, readErr := dev.ReadScheduledInterruptInput(context.Background(), never, 1, buffer); readErr != nil || written != 9 {
		t.Fatalf("post-cancel event read=(%d, %v)", written, readErr)
	}
	if buffer[1] != byte(state.DX) || buffer[2] != byte(state.DX>>8) ||
		buffer[3] != byte(state.DY) || buffer[4] != byte(state.DY>>8) {
		t.Fatalf("post-cancel movement=%x", buffer)
	}
}

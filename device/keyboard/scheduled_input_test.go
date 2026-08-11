package keyboard

import (
	"context"
	"testing"
	"time"
)

func TestScheduledInterruptInputPreservesKeyboardEventContract(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := *NewInputState()
	state.Modifiers = 0x5a
	state.KeyBitmap[3] = 0x80
	dev.UpdateInputState(state)
	buffer := make([]byte, 34)
	never := make(chan time.Time)
	if written, readErr := dev.ReadScheduledInterruptInput(context.Background(), never, 1, buffer); readErr != nil || written != 34 {
		t.Fatalf("event read=(%d, %v)", written, readErr)
	}
	if buffer[0] != state.Modifiers || buffer[5] != state.KeyBitmap[3] {
		t.Fatalf("event state=%x", buffer)
	}
	deadline := make(chan time.Time, 1)
	deadline <- time.Now()
	if _, err = dev.ReadScheduledInterruptInput(context.Background(), deadline, 1, buffer); err != context.DeadlineExceeded {
		t.Fatalf("idle deadline=%v want %v", err, context.DeadlineExceeded)
	}

	state.Modifiers = 0xa5
	dev.UpdateInputState(state)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	readyDeadline := make(chan time.Time, 1)
	readyDeadline <- time.Now()
	if _, err = dev.ReadScheduledInterruptInput(ctx, readyDeadline, 1, buffer); err != context.Canceled {
		t.Fatalf("lifecycle cancellation=%v want %v", err, context.Canceled)
	}
	if written, readErr := dev.ReadScheduledInterruptInput(context.Background(), never, 1, buffer); readErr != nil || written != 34 {
		t.Fatalf("post-cancel event read=(%d, %v)", written, readErr)
	}
	if buffer[0] != state.Modifiers {
		t.Fatalf("post-cancel state=%x", buffer)
	}
}

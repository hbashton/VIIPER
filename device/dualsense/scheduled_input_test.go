package dualsense

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

func TestScheduledInterruptInputPreservesDualSenseStateAndCadence(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := NewInputState()
	state.LX, state.R2, state.Buttons = 23, 177, ButtonCross
	dev.UpdateInputState(state)
	buffer := make([]byte, InputReportSize)
	never := make(chan time.Time)
	written, err := dev.ReadScheduledInterruptInput(context.Background(), never, EndpointIn&0x0f, buffer)
	if err != nil || written != InputReportSize {
		t.Fatalf("event read=(%d, %v)", written, err)
	}
	if buffer[1] != uint8(int16(state.LX)+128) || buffer[6] != state.R2 || buffer[7] != 1 {
		t.Fatalf("event state/counter=%x", buffer[:11])
	}
	firstTimestamp := binary.LittleEndian.Uint32(buffer[28:32])

	deadline := make(chan time.Time, 1)
	deadline <- time.Now()
	written, err = dev.ReadScheduledInterruptInput(context.Background(), deadline, EndpointIn&0x0f, buffer)
	if err != nil || written != InputReportSize {
		t.Fatalf("deadline read=(%d, %v)", written, err)
	}
	if buffer[1] != uint8(int16(state.LX)+128) || buffer[6] != state.R2 || buffer[7] != 2 {
		t.Fatalf("deadline state/counter=%x", buffer[:11])
	}
	secondTimestamp := binary.LittleEndian.Uint32(buffer[28:32])
	if secondTimestamp < firstTimestamp || binary.LittleEndian.Uint32(buffer[49:53]) != secondTimestamp {
		t.Fatalf("deadline timestamps first=%d second=%d mirror=%d", firstTimestamp,
			secondTimestamp, binary.LittleEndian.Uint32(buffer[49:53]))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state.LX = 44
	dev.UpdateInputState(state)
	readyDeadline := make(chan time.Time, 1)
	readyDeadline <- time.Now()
	if _, err = dev.ReadScheduledInterruptInput(ctx, readyDeadline, EndpointIn&0x0f, buffer); err != context.Canceled {
		t.Fatalf("lifecycle cancellation=%v want %v", err, context.Canceled)
	}
	if written, err = dev.ReadScheduledInterruptInput(context.Background(), never, EndpointIn&0x0f, buffer); err != nil || written != InputReportSize {
		t.Fatalf("post-cancel event read=(%d, %v)", written, err)
	}
	if buffer[1] != uint8(int16(state.LX)+128) || buffer[7] != 3 {
		t.Fatalf("post-cancel state/counter=%x", buffer[:11])
	}
	if thirdTimestamp := binary.LittleEndian.Uint32(buffer[28:32]); thirdTimestamp < secondTimestamp {
		t.Fatalf("post-cancel timestamp=%d before previous=%d", thirdTimestamp, secondTimestamp)
	}
}

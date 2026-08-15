package dualshock4

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

func TestScheduledInterruptInputPreservesDualShock4StateAndCadence(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := NewInputState()
	state.LX, state.L2, state.Buttons = -31, 166, ButtonCross
	dev.UpdateInputState(state)
	buffer := make([]byte, InputReportSize)
	never := make(chan time.Time)
	written, err := dev.ReadScheduledInterruptInput(context.Background(), never, EndpointIn&0x0f, buffer)
	if err != nil || written != InputReportSize {
		t.Fatalf("event read=(%d, %v)", written, err)
	}
	if buffer[1] != uint8(int16(state.LX)+128) || buffer[8] != state.L2 || buffer[7]>>CounterShift != 1 {
		t.Fatalf("event state/counter=%x", buffer[:12])
	}
	firstTimestamp := binary.LittleEndian.Uint16(buffer[10:12])

	deadline := make(chan time.Time, 1)
	deadline <- time.Now()
	written, err = dev.ReadScheduledInterruptInput(context.Background(), deadline, EndpointIn&0x0f, buffer)
	if err != nil || written != InputReportSize {
		t.Fatalf("deadline read=(%d, %v)", written, err)
	}
	if buffer[1] != uint8(int16(state.LX)+128) || buffer[8] != state.L2 || buffer[7]>>CounterShift != 2 {
		t.Fatalf("deadline state/counter=%x", buffer[:12])
	}
	secondTimestamp := binary.LittleEndian.Uint16(buffer[10:12])
	if secondTimestamp < firstTimestamp {
		t.Fatalf("deadline timestamp=%d before event=%d", secondTimestamp, firstTimestamp)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state.LX = 45
	dev.UpdateInputState(state)
	readyDeadline := make(chan time.Time, 1)
	readyDeadline <- time.Now()
	if _, err = dev.ReadScheduledInterruptInput(ctx, readyDeadline, EndpointIn&0x0f, buffer); err != context.Canceled {
		t.Fatalf("lifecycle cancellation=%v want %v", err, context.Canceled)
	}
	written, err = dev.ReadScheduledInterruptInput(context.Background(), never, EndpointIn&0x0f, buffer)
	if err != nil || written != InputReportSize {
		t.Fatalf("post-cancel event read=(%d, %v)", written, err)
	}
	if buffer[1] != uint8(int16(state.LX)+128) || buffer[7]>>CounterShift != 3 {
		t.Fatalf("post-cancel state/counter=%x", buffer[:12])
	}
	if thirdTimestamp := binary.LittleEndian.Uint16(buffer[10:12]); thirdTimestamp < secondTimestamp {
		t.Fatalf("post-cancel timestamp=%d before previous=%d", thirdTimestamp, secondTimestamp)
	}
}

func TestClassifiedNativeInputPreservesQueuedDualShock4Transitions(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	press := NewInputState()
	press.LX, press.Buttons = -51, ButtonCross
	release := NewInputState()
	release.LX = 67
	dev.UpdateInputState(press)
	dev.UpdateInputState(release)

	buffer := make([]byte, InputReportSize)
	never := make(chan time.Time)
	written, transition, err := dev.ReadClassifiedScheduledInterruptInput(
		context.Background(), never, EndpointIn&0x0f, buffer)
	if err != nil || written != InputReportSize || !transition ||
		buffer[1] != uint8(int16(press.LX)+128) {
		t.Fatalf("press read=(%d, %t, %v) state=%x", written, transition, err, buffer[:12])
	}
	written, transition, err = dev.ReadClassifiedScheduledInterruptInput(
		context.Background(), never, EndpointIn&0x0f, buffer)
	if err != nil || written != InputReportSize || !transition ||
		buffer[1] != uint8(int16(release.LX)+128) {
		t.Fatalf("release read=(%d, %t, %v) state=%x", written, transition, err, buffer[:12])
	}
	analog := *release
	analog.LX = 19
	if err = dev.UpdateInputState(&analog); err != nil {
		t.Fatal(err)
	}
	written, transition, err = dev.ReadClassifiedScheduledInterruptInput(
		context.Background(), never, EndpointIn&0x0f, buffer)
	if err != nil || written != InputReportSize || transition ||
		buffer[1] != uint8(int16(analog.LX)+128) {
		t.Fatalf("analog read=(%d, %t, %v) state=%x", written, transition, err, buffer[:12])
	}
}

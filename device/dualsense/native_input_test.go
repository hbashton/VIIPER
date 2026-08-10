//go:build !race

package dualsense

import (
	"io"
	"testing"
)

func TestNativeInputEncodingUsesCallerBufferWithoutAllocating(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := NewInputState()
	meta := &MetaState{BatteryStatus: BatteryFullyCharged}
	buffer := make([]byte, InputReportSize)
	if allocations := testing.AllocsPerRun(1000, func() {
		written, encodeErr := dev.buildUSBInputReportInto(state, meta, buffer)
		if encodeErr != nil || written != InputReportSize {
			panic("DualSense native input encoding failed")
		}
	}); allocations != 0 {
		t.Fatalf("native input allocations=%v want 0", allocations)
	}
	if _, err = dev.buildUSBInputReportInto(state, meta, buffer[:InputReportSize-1]); err != io.ErrShortBuffer {
		t.Fatalf("short-buffer error=%v want %v", err, io.ErrShortBuffer)
	}
}

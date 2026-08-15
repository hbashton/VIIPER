//go:build !race

package xbox360

import (
	"io"
	"testing"
)

func TestNativeInputEncodingUsesCallerBufferWithoutAllocating(t *testing.T) {
	state := NewInputState()
	buffer := make([]byte, 20)
	if allocations := testing.AllocsPerRun(1000, func() {
		written, err := state.BuildReportInto(buffer)
		if err != nil || written != 20 {
			panic("Xbox 360 native input encoding failed")
		}
	}); allocations != 0 {
		t.Fatalf("native input allocations=%v want 0", allocations)
	}
	if _, err := state.BuildReportInto(buffer[:19]); err != io.ErrShortBuffer {
		t.Fatalf("short-buffer error=%v want %v", err, io.ErrShortBuffer)
	}
}

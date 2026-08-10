//go:build !race

package mouse

import (
	"io"
	"testing"
)

func TestNativeInputEncodingUsesCallerBufferWithoutAllocating(t *testing.T) {
	state := NewInputState()
	buffer := make([]byte, 9)
	if allocations := testing.AllocsPerRun(1000, func() {
		written, err := state.BuildReportInto(buffer)
		if err != nil || written != 9 {
			panic("mouse native input encoding failed")
		}
	}); allocations != 0 {
		t.Fatalf("native input allocations=%v want 0", allocations)
	}
	if _, err := state.BuildReportInto(buffer[:8]); err != io.ErrShortBuffer {
		t.Fatalf("short-buffer error=%v want %v", err, io.ErrShortBuffer)
	}
}

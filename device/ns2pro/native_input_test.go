//go:build !race

package ns2pro

import (
	"io"
	"testing"
)

func TestNativeInputEncodingUsesCallerBufferWithoutAllocating(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, InputReportSize)
	if allocations := testing.AllocsPerRun(1000, func() {
		written, encodeErr := dev.inputReportForIDInto(ReportIDPro, buffer)
		if encodeErr != nil || written != InputReportSize {
			panic("Switch 2 Pro native input encoding failed")
		}
	}); allocations != 0 {
		t.Fatalf("native input allocations=%v want 0", allocations)
	}
	if _, err = dev.inputReportForIDInto(ReportIDPro, buffer[:InputReportSize-1]); err != io.ErrShortBuffer {
		t.Fatalf("short-buffer error=%v want %v", err, io.ErrShortBuffer)
	}
}

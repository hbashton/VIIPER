//go:build windows

package udecx

import (
	"testing"
	"unsafe"
)

func TestIORequestOverlappedRemainsFirstField(t *testing.T) {
	if got := unsafe.Offsetof(ioRequest{}.overlapped); got != 0 {
		t.Fatalf("unsafe.Offsetof(ioRequest{}.overlapped)=%d want=0", got)
	}
}

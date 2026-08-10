//go:build windows

package usb

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var queryInterruptTimePrecise = windows.NewLazySystemDLL("api-ms-win-core-realtime-l1-1-1.dll").
	NewProc("QueryInterruptTimePrecise")

func nativeClockSnapshot() nativeClockSample {
	var interruptTime100ns uint64
	queryInterruptTimePrecise.Call(uintptr(unsafe.Pointer(&interruptTime100ns)))
	return nativeClockSample{
		now:   time.Now(),
		frame: uint32(interruptTime100ns / 10_000),
	}
}

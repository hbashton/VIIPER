//go:build windows

// Package inputlatency owns the opt-in, source-bound timing evidence shared by
// the in-process Windows latency probe and VIIPER's instrumented input path.
package inputlatency

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32QPC               = windows.NewLazySystemDLL("kernel32.dll")
	queryPerformanceCounter   = kernel32QPC.NewProc("QueryPerformanceCounter")
	queryPerformanceFrequency = kernel32QPC.NewProc("QueryPerformanceFrequency")
)

func query(proc *windows.LazyProc) (int64, error) {
	var value int64
	ok, _, callErr := proc.Call(uintptr(unsafe.Pointer(&value)))
	if ok == 0 {
		return 0, fmt.Errorf("%s: %w", proc.Name, callErr)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s returned %d", proc.Name, value)
	}
	return value, nil
}

// Counter returns raw QueryPerformanceCounter ticks. Every canonical stage in
// one probe capture is stamped by this function in the same process.
func Counter() (int64, error) { return query(queryPerformanceCounter) }

// Frequency returns the QueryPerformanceCounter frequency used to convert the
// raw evidence. The report retains raw ticks and this exact frequency.
func Frequency() (int64, error) { return query(queryPerformanceFrequency) }

// ClockIdentity names the exact in-process QPC identity used by the probe.
// The PID intentionally prevents evidence from an external VIIPER process
// being combined with client-side ticks from the harness process.
func ClockIdentity() (string, error) {
	frequency, err := Frequency()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("windows-qpc/process=%d/frequency=%d", os.Getpid(), frequency), nil
}

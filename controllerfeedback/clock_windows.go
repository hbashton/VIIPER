//go:build windows

package controllerfeedback

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32ControllerFeedback = windows.NewLazySystemDLL("kernel32.dll")
	queryPerformanceCounter    = kernel32ControllerFeedback.NewProc(
		"QueryPerformanceCounter")
	queryPerformanceFrequency = kernel32ControllerFeedback.NewProc(
		"QueryPerformanceFrequency")
	controllerFeedbackQPCFrequency = readQPCFrequency()
)

// HostMonotonicMicroseconds samples the normative CFBK v1 clock domain. The
// returned value is directly comparable across processes on this Windows host.
func HostMonotonicMicroseconds() (uint64, bool) {
	if controllerFeedbackQPCFrequency <= 0 {
		return 0, false
	}
	var counter int64
	result, _, _ := queryPerformanceCounter.Call(
		uintptr(unsafe.Pointer(&counter)))
	if result == 0 || counter < 0 {
		return 0, false
	}
	return qpcToMicroseconds(uint64(counter),
		uint64(controllerFeedbackQPCFrequency))
}

func readQPCFrequency() int64 {
	var frequency int64
	result, _, _ := queryPerformanceFrequency.Call(
		uintptr(unsafe.Pointer(&frequency)))
	if result == 0 || frequency <= 0 {
		return 0
	}
	return frequency
}

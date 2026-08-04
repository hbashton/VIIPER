//go:build windows

package usb

import (
	"errors"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	avrtDLL                          = windows.NewLazySystemDLL("avrt.dll")
	avSetMmThreadCharacteristicsProc = avrtDLL.NewProc("AvSetMmThreadCharacteristicsW")
	avSetMmThreadPriorityProc        = avrtDLL.NewProc("AvSetMmThreadPriority")
	avRevertMmThreadCharacteristics  = avrtDLL.NewProc("AvRevertMmThreadCharacteristics")
)

const avrtPriorityCritical = 2

// enterRealtimeMediaThread keeps the USB/IP isochronous service clock on one
// Windows thread and registers that thread with MMCSS. Blocking socket reads
// still yield normally; CPU pressure can no longer strand an audio completion
// behind unrelated best-effort work after its USB service deadline.
func enterRealtimeMediaThread() (func(), error) {
	runtime.LockOSThread()
	release := func() {
		runtime.UnlockOSThread()
	}

	taskName, err := windows.UTF16PtrFromString("Pro Audio")
	if err != nil {
		return release, err
	}

	var taskIndex uint32
	handle, _, callErr := avSetMmThreadCharacteristicsProc.Call(
		uintptr(unsafe.Pointer(taskName)),
		uintptr(unsafe.Pointer(&taskIndex)))
	if handle == 0 {
		if errors.Is(callErr, syscall.Errno(0)) {
			callErr = errors.New("AvSetMmThreadCharacteristicsW returned no handle")
		}
		return release, callErr
	}

	cleanup := func() {
		avRevertMmThreadCharacteristics.Call(handle)
		runtime.UnlockOSThread()
	}
	priorityApplied, _, priorityErr := avSetMmThreadPriorityProc.Call(
		handle, uintptr(avrtPriorityCritical))
	if priorityApplied == 0 {
		if errors.Is(priorityErr, syscall.Errno(0)) {
			priorityErr = errors.New("AvSetMmThreadPriority returned false")
		}
		return cleanup, priorityErr
	}

	return cleanup, nil
}

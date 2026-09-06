//go:build windows

package api

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This seam owns one OVERLAPPED operation, not a hub port or all I/O on a
// handle. Injected functions let completion/cancellation races be tested
// without opening the installed driver.
type nativeAttachOperations struct {
	control     nativeAttachIOControl
	result      func(windows.Handle, *windows.Overlapped, *uint32, bool) error
	cancel      func(windows.Handle, *windows.Overlapped) error
	createEvent func() (windows.Handle, error)
	closeEvent  func(windows.Handle) error
}

func windowsNativeAttachOperations() nativeAttachOperations {
	return nativeAttachOperations{
		control: windows.DeviceIoControl, result: windows.GetOverlappedResult,
		cancel:      windows.CancelIoEx,
		createEvent: func() (windows.Handle, error) { return windows.CreateEvent(nil, 1, 0, nil) },
		closeEvent:  windows.CloseHandle,
	}
}

func submitAttachIOCTLContext(ctx context.Context, handle windows.Handle,
	request *attachIOCTL, calls nativeAttachOperations,
) (int32, uint32, error) {
	if request == nil || request.Size != uint32(unsafe.Sizeof(attachIOCTL{})) {
		return 0, 0, fmt.Errorf("argumentValidation: invalid native attach request")
	}
	returned, err := submitNativeIOCTLContext(ctx, handle, request,
		(*byte)(unsafe.Pointer(request)), request.Size, 8, nativeAttachControlCode(request), calls)
	if err != nil {
		return 0, returned, err
	}
	return request.PortOutput, returned, nil
}

// The pinned owner keeps the entire request alive until actual completion,
// including cancellation callbacks. Shared by attach and exact-location stop.
func submitNativeIOCTLContext(ctx context.Context, handle windows.Handle, owner any,
	data *byte, size, responseSize, controlCode uint32, calls nativeAttachOperations,
) (uint32, error) {
	if ctx == nil || data == nil || size == 0 || responseSize == 0 ||
		calls.control == nil || calls.result == nil || calls.cancel == nil ||
		calls.createEvent == nil || calls.closeEvent == nil {
		return 0, fmt.Errorf("argumentValidation: invalid asynchronous native request")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	event, err := calls.createEvent()
	if err != nil {
		return 0, fmt.Errorf("create native completion event: %w", err)
	}
	if event == 0 || event == windows.InvalidHandle {
		return 0, fmt.Errorf("create native completion event: invalid handle")
	}
	defer calls.closeEvent(event) //nolint:errcheck
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	overlapped := &windows.Overlapped{HEvent: event}
	var pinned runtime.Pinner
	pinned.Pin(owner)
	pinned.Pin(overlapped)
	defer pinned.Unpin()
	var returned uint32
	err = calls.control(handle, controlCode, data, size,
		data, responseSize, &returned, overlapped)
	if errors.Is(err, windows.ERROR_IO_PENDING) {
		cancelFinished := make(chan struct{})
		stopCancel := context.AfterFunc(ctx, func() {
			defer close(cancelFinished)
			// ERROR_NOT_FOUND can mean completion won the race. Cancellation
			// is only a request; the actual completion below remains authoritative.
			_ = calls.cancel(handle, overlapped)
		})
		defer func() {
			if !stopCancel() {
				<-cancelFinished // callback cannot outlive its handle/buffer
			}
		}()
		for {
			err = calls.result(handle, overlapped, &returned, true)
			if !errors.Is(err, windows.ERROR_IO_INCOMPLETE) && !errors.Is(err, windows.ERROR_IO_PENDING) {
				break
			}
			// Never release pending native memory or infer completion from a
			// timeout. This is a cold-path defensive retry, not input pacing.
			time.Sleep(time.Millisecond)
		}
	}
	if err != nil {
		if errors.Is(err, windows.ERROR_OPERATION_ABORTED) && ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		return returned, err
	}
	if returned != responseSize {
		return returned, fmt.Errorf("usbip-win2 returned %d bytes; expected %d", returned, responseSize)
	}
	// A native success can race cancellation. Preserve that result; the
	// activation transaction must recheck its context and close its captured
	// registration, never pretend cancellation detached the returned port.
	return returned, nil
}

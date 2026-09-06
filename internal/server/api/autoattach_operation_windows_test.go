//go:build windows

package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func nativeOperationFixture(t *testing.T) (*attachIOCTL, nativeAttachOperations, *atomic.Int32) {
	t.Helper()
	alias, err := usbip.NewProductionXboxOneBusID()
	require.NoError(t, err)
	meta := usbip.ExportMeta{BusID: 17, DevID: 4}
	copy(meta.USBBusID[:], alias)
	request, err := newAttachIOCTL(&meta, 3241)
	require.NoError(t, err)
	closed := &atomic.Int32{}
	return &request, nativeAttachOperations{
		createEvent: func() (windows.Handle, error) { return 101, nil },
		closeEvent:  func(windows.Handle) error { closed.Add(1); return nil },
		control: func(_ windows.Handle, _ uint32, _ *byte, _ uint32, _ *byte, _ uint32, returned *uint32, _ *windows.Overlapped) error {
			request.PortOutput, *returned = 7, 8
			return nil
		},
		result: func(_ windows.Handle, _ *windows.Overlapped, returned *uint32, _ bool) error {
			request.PortOutput, *returned = 7, 8
			return nil
		},
		cancel: func(windows.Handle, *windows.Overlapped) error { return nil },
	}, closed
}

func TestContextNativeAttachPreservesABIAndOperationChoice(t *testing.T) {
	for _, production := range []bool{true, false} {
		t.Run(map[bool]string{true: "Xbox once", false: "legacy ordinary"}[production], func(t *testing.T) {
			req, calls, closed := nativeOperationFixture(t)
			if !production {
				req.BusID = [32]byte{}
				copy(req.BusID[:], "17-4")
			}
			expected := *req
			calls.control = func(handle windows.Handle, code uint32, in *byte, inSize uint32, out *byte, outSize uint32, returned *uint32, ov *windows.Overlapped) error {
				require.Equal(t, windows.Handle(202), handle)
				want := uint32(ioctlPluginHardware)
				if production {
					want = uint32(ioctlPluginHardwareOnce)
				}
				require.Equal(t, want, code)
				require.Equal(t, expected, *req)
				require.Equal(t, (*byte)(unsafe.Pointer(req)), in)
				require.Equal(t, in, out)
				require.Equal(t, uint32(1100), inSize)
				require.Equal(t, uint32(8), outSize)
				require.NotNil(t, ov)
				require.Equal(t, windows.Handle(101), ov.HEvent)
				req.PortOutput, *returned = 9, 8
				return nil
			}
			calls.result = func(windows.Handle, *windows.Overlapped, *uint32, bool) error {
				t.Fatal("synchronous completion queried again")
				return nil
			}
			port, returned, err := submitAttachIOCTLContext(context.Background(), 202, req, calls)
			require.NoError(t, err)
			require.Equal(t, int32(9), port)
			require.Equal(t, uint32(8), returned)
			require.Equal(t, int32(1), closed.Load())
		})
	}
}

func TestContextNativeAttachCancellationBeforeSubmissionDoesNotStartIO(t *testing.T) {
	for _, cancelDuringEvent := range []bool{false, true} {
		req, calls, closed := nativeOperationFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		if cancelDuringEvent {
			calls.createEvent = func() (windows.Handle, error) { cancel(); return 101, nil }
		} else {
			cancel()
		}
		calls.control = func(windows.Handle, uint32, *byte, uint32, *byte, uint32, *uint32, *windows.Overlapped) error {
			t.Fatal("canceled request submitted")
			return nil
		}
		_, _, err := submitAttachIOCTLContext(ctx, 202, req, calls)
		require.ErrorIs(t, err, context.Canceled)
		if cancelDuringEvent {
			require.Equal(t, int32(1), closed.Load())
		} else {
			require.Zero(t, closed.Load())
		}
	}
}

func TestContextNativeAttachCancellationWaitsForNativeAndCancelCompletion(t *testing.T) {
	req, calls, closed := nativeOperationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, canceled := make(chan struct{}), make(chan struct{})
	releaseNative, releaseCancel := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseNative:
		default:
			close(releaseNative)
		}
		select {
		case <-releaseCancel:
		default:
			close(releaseCancel)
		}
	})
	var operation *windows.Overlapped
	var exactCancel, exactResult atomic.Bool
	calls.control = func(_ windows.Handle, _ uint32, _ *byte, _ uint32, _ *byte, _ uint32, _ *uint32, ov *windows.Overlapped) error {
		operation = ov
		return windows.ERROR_IO_PENDING
	}
	calls.result = func(handle windows.Handle, ov *windows.Overlapped, _ *uint32, wait bool) error {
		exactResult.Store(handle == 202 && ov == operation && wait)
		close(entered)
		<-releaseNative
		return windows.ERROR_OPERATION_ABORTED
	}
	calls.cancel = func(handle windows.Handle, ov *windows.Overlapped) error {
		exactCancel.Store(handle == 202 && ov == operation && ov != nil)
		close(canceled)
		<-releaseCancel
		return nil
	}
	finished := make(chan error, 1)
	go func() { _, _, err := submitAttachIOCTLContext(ctx, 202, req, calls); finished <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("native wait not entered")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("exact cancellation not requested")
	}
	select {
	case <-finished:
		t.Fatal("cancellation was mistaken for completion")
	default:
	}
	require.Zero(t, closed.Load())
	close(releaseNative)
	select {
	case <-finished:
		t.Fatal("operation storage released before cancel callback returned")
	case <-time.After(20 * time.Millisecond):
	}
	require.Zero(t, closed.Load())
	close(releaseCancel)
	select {
	case err := <-finished:
		require.ErrorIs(t, err, windows.ERROR_OPERATION_ABORTED)
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("retired operation did not return")
	}
	require.True(t, exactCancel.Load())
	require.True(t, exactResult.Load())
	require.Equal(t, int32(1), closed.Load())
}

func TestContextNativeAttachCompletionCanWinCancellationWithoutLosingPort(t *testing.T) {
	req, calls, closed := nativeOperationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelObserved := make(chan struct{})
	calls.control = func(windows.Handle, uint32, *byte, uint32, *byte, uint32, *uint32, *windows.Overlapped) error {
		cancel()
		return windows.ERROR_IO_PENDING
	}
	calls.cancel = func(windows.Handle, *windows.Overlapped) error { close(cancelObserved); return windows.ERROR_NOT_FOUND }
	calls.result = func(_ windows.Handle, _ *windows.Overlapped, returned *uint32, _ bool) error {
		<-cancelObserved
		req.PortOutput, *returned = 9, 8
		return nil
	}
	port, _, err := submitAttachIOCTLContext(ctx, 202, req, calls)
	require.NoError(t, err, "Native success remains evidence even when its caller canceled; the transaction must clean up.")
	require.Equal(t, int32(9), port)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Equal(t, int32(1), closed.Load())
}

func TestContextNativeAttachIncompleteObservationCannotRetireStorage(t *testing.T) {
	req, calls, closed := nativeOperationFixture(t)
	calls.control = func(windows.Handle, uint32, *byte, uint32, *byte, uint32, *uint32, *windows.Overlapped) error {
		return windows.ERROR_IO_PENDING
	}
	observations := 0
	calls.result = func(_ windows.Handle, _ *windows.Overlapped, returned *uint32, wait bool) error {
		observations++
		require.True(t, wait)
		require.Zero(t, closed.Load())
		if observations == 1 {
			return windows.ERROR_IO_INCOMPLETE
		}
		req.PortOutput, *returned = 7, 8
		return nil
	}
	port, _, err := submitAttachIOCTLContext(context.Background(), 202, req, calls)
	require.NoError(t, err)
	require.Equal(t, 2, observations)
	require.Equal(t, int32(7), port)
	require.Equal(t, int32(1), closed.Load())
}

func TestContextNativeAttachRejectsMalformedCompletionAndEventFailure(t *testing.T) {
	for _, returnedSize := range []uint32{0, 4, 7, 9, 1100} {
		req, calls, closed := nativeOperationFixture(t)
		calls.control = func(windows.Handle, uint32, *byte, uint32, *byte, uint32, *uint32, *windows.Overlapped) error {
			return windows.ERROR_IO_PENDING
		}
		calls.result = func(_ windows.Handle, _ *windows.Overlapped, returned *uint32, _ bool) error {
			req.PortOutput, *returned = 7, returnedSize
			return nil
		}
		port, got, err := submitAttachIOCTLContext(context.Background(), 202, req, calls)
		require.ErrorContains(t, err, "expected 8")
		require.Zero(t, port)
		require.Equal(t, returnedSize, got)
		require.Equal(t, int32(1), closed.Load())
	}
	req, calls, closed := nativeOperationFixture(t)
	calls.createEvent = func() (windows.Handle, error) { return 0, windows.ERROR_ACCESS_DENIED }
	_, _, err := submitAttachIOCTLContext(context.Background(), 202, req, calls)
	require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED)
	require.Zero(t, closed.Load())
}

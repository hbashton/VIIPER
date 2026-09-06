//go:build windows

package api

import (
	"bytes"
	"context"
	"testing"
	"unsafe"

	"github.com/Alia5/VIIPER/usbip"
	"golang.org/x/sys/windows"

	"github.com/stretchr/testify/require"
)

// Keep the earlier ABI/retry regression cases on the actual production
// operation helper, not on an obsolete synchronous implementation.
func submitAttachIOCTLTest(t *testing.T, request *attachIOCTL,
	control nativeAttachIOControl) (int32, uint32, error) {
	t.Helper()
	_, calls, closed := nativeOperationFixture(t)
	calls.control = control
	port, returned, err := submitAttachIOCTLContext(context.Background(), 202, request, calls)
	if request != nil && request.Size == 1100 && control != nil {
		require.Equal(t, int32(1), closed.Load())
	} else {
		require.Zero(t, closed.Load())
	}
	return port, returned, err
}

func TestUsbipAttachABISizes(t *testing.T) {
	require.Equal(t, uintptr(1100), unsafe.Sizeof(attachIOCTL{}),
		"usbip-win2 0.9.7.7 plugin_hardware ABI changed")
}

func TestNativeAttachPayloadPreservesExactAliasWithoutOpeningDriver(t *testing.T) {
	alias, err := usbip.NewProductionXboxOneBusID()
	require.NoError(t, err)
	meta := usbip.ExportMeta{BusID: 17, DevID: 4}
	copy(meta.USBBusID[:], alias)
	request, err := newAttachIOCTL(&meta, 3241)
	require.NoError(t, err)
	require.Equal(t, uint32(1100), request.Size)
	require.Equal(t, meta.USBBusID, request.BusID)
	require.Equal(t, "3241", string(bytes.TrimRight(request.Service[:], "\x00")))
	require.Equal(t, "localhost", string(bytes.TrimRight(request.Host[:], "\x00")))
	require.Zero(t, request.PortOutput)
	meta.USBBusID = [32]byte{}
	_, err = newAttachIOCTL(&meta, 3241)
	require.Error(t, err)
}

func TestNativeAutoAttachResultCarriesExactPort(t *testing.T) {
	got := AutoAttachResult{
		USBIPPort: 7,
	}

	require.Equal(t, int32(7), got.USBIPPort)
	require.Empty(t, got.USBIPOwnerSerial)
}

func TestNativeXboxOneAttachFailureUsesOneAttemptWithoutFallback(t *testing.T) {
	alias, err := usbip.NewProductionXboxOneBusID()
	require.NoError(t, err)
	meta := usbip.ExportMeta{BusID: 17, DevID: 4}
	copy(meta.USBBusID[:], alias)
	request, err := newAttachIOCTL(&meta, 3241)
	require.NoError(t, err)
	calls := 0
	var controlCode uint32
	port, returned, err := submitAttachIOCTLTest(t, &request,
		func(_ windows.Handle, code uint32, in *byte, inSize uint32,
			out *byte, outSize uint32, _ *uint32, overlapped *windows.Overlapped) error {
			calls++
			controlCode = code
			require.Equal(t, (*byte)(unsafe.Pointer(&request)), in)
			require.Equal(t, in, out)
			require.Equal(t, uint32(1100), inSize)
			require.Equal(t, uint32(8), outSize)
			require.NotNil(t, overlapped)
			return windows.ERROR_TIMEOUT
		})
	require.ErrorIs(t, err, windows.ERROR_TIMEOUT)
	require.Zero(t, port)
	require.Zero(t, returned)
	require.Equal(t, 1, calls)
	// v.0.9.7.7 include/usbip/vhci.h: PLUGIN_HARDWARE_ONCE,
	// CTL_CODE(FILE_DEVICE_UNKNOWN, 0x806, METHOD_BUFFERED, READ|WRITE).
	require.Equal(t, uint32(0x0022e018), controlCode,
		"failed transactional Xbox activation must not start background kernel attaches")
}

func TestNativeLegacyAttachKeepsRetryOperationAndReturnsExactPort(t *testing.T) {
	meta := usbip.ExportMeta{BusID: 17, DevID: 4}
	copy(meta.USBBusID[:], "17-4")
	request, err := newAttachIOCTL(&meta, 3241)
	require.NoError(t, err)
	calls := 0
	port, returned, err := submitAttachIOCTLTest(t, &request,
		func(_ windows.Handle, code uint32, _ *byte, _ uint32,
			_ *byte, _ uint32, bytesReturned *uint32, _ *windows.Overlapped) error {
			calls++
			require.Equal(t, uint32(0x0022e000), code)
			request.PortOutput = 7
			*bytesReturned = 8
			return nil
		})
	require.NoError(t, err)
	require.Equal(t, int32(7), port)
	require.Equal(t, uint32(8), returned)
	require.Equal(t, 1, calls)
}

func TestNativeXboxOneAttachSuccessPreservesPayloadAndPort(t *testing.T) {
	alias, err := usbip.NewProductionXboxOneBusID()
	require.NoError(t, err)
	meta := usbip.ExportMeta{BusID: 17, DevID: 4}
	copy(meta.USBBusID[:], alias)
	request, err := newAttachIOCTL(&meta, 3241)
	require.NoError(t, err)
	expected := request
	calls := 0
	port, returned, err := submitAttachIOCTLTest(t, &request,
		func(_ windows.Handle, code uint32, _ *byte, _ uint32,
			_ *byte, _ uint32, bytesReturned *uint32, _ *windows.Overlapped) error {
			calls++
			require.Equal(t, uint32(0x0022e018), code)
			require.Equal(t, expected, request)
			request.PortOutput = 9
			*bytesReturned = 8
			return nil
		})
	require.NoError(t, err)
	require.Equal(t, int32(9), port)
	require.Equal(t, uint32(8), returned)
	require.Equal(t, 1, calls)
}

func TestNativeAttachInvalidRequestDoesNotReachDriver(t *testing.T) {
	called := false
	fake := func(_ windows.Handle, _ uint32, _ *byte, _ uint32,
		_ *byte, _ uint32, _ *uint32, _ *windows.Overlapped) error {
		called = true
		return nil
	}
	for _, request := range []*attachIOCTL{nil, {}, {Size: 8}, {Size: 1104}} {
		_, _, err := submitAttachIOCTLTest(t, request, fake)
		require.Error(t, err)
	}
	_, _, err := submitAttachIOCTLTest(t, &attachIOCTL{Size: 1100}, nil)
	require.Error(t, err)
	require.False(t, called)
}

func TestNativeAttachRejectsTruncatedAndOversizedSuccessWithoutRetry(t *testing.T) {
	for _, size := range []uint32{0, 4, 7, 9, 1100} {
		meta := usbip.ExportMeta{BusID: 17, DevID: 4}
		copy(meta.USBBusID[:], "17-4")
		request, err := newAttachIOCTL(&meta, 3241)
		require.NoError(t, err)
		calls := 0
		port, returned, err := submitAttachIOCTLTest(t, &request,
			func(_ windows.Handle, _ uint32, _ *byte, _ uint32,
				_ *byte, _ uint32, bytesReturned *uint32, _ *windows.Overlapped) error {
				calls++
				request.PortOutput = 7
				*bytesReturned = size
				return nil
			})
		require.ErrorContains(t, err, "expected 8")
		require.Zero(t, port)
		require.Equal(t, size, returned)
		require.Equal(t, 1, calls)
	}
}

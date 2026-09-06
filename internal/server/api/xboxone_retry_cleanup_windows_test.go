//go:build windows

package api

import (
	"bytes"
	"context"
	"testing"
	"unsafe"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestStopRetry0977ABIHasExactLocationAndNeverDetachesOrStopsAll(t *testing.T) {
	alias, err := usbip.NewProductionXboxOneBusID()
	require.NoError(t, err)
	meta := usbip.ExportMeta{BusID: 17, DevID: 4}
	copy(meta.USBBusID[:], alias)
	request, err := newStopAttachIOCTL(&meta, 3241)
	require.NoError(t, err)
	require.Equal(t, uintptr(1104), unsafe.Sizeof(request))
	require.Equal(t, uintptr(1100), unsafe.Offsetof(request.Count))
	require.Equal(t, uint32(0x0022e014), uint32(ioctlStopAttachAttempts))
	require.Equal(t, alias, string(bytes.TrimRight(request.BusID[:], "\x00")))
	require.Equal(t, "localhost", string(bytes.TrimRight(request.Host[:], "\x00")))
	require.Equal(t, "3241", string(bytes.TrimRight(request.Service[:], "\x00")))
	require.Zero(t, request.PortOutput)
	_, calls, closed := nativeOperationFixture(t)
	calls.control = func(_ windows.Handle, code uint32, in *byte, inSize uint32, out *byte, outSize uint32, returned *uint32, _ *windows.Overlapped) error {
		require.Equal(t, uint32(ioctlStopAttachAttempts), code)
		require.Equal(t, in, out)
		require.Equal(t, uint32(1104), inSize)
		require.Equal(t, inSize, outSize)
		request.Count = 3
		*returned = outSize
		return nil
	}
	_, err = submitNativeIOCTLContext(context.Background(), 202, &request, (*byte)(unsafe.Pointer(&request)),
		request.Size, request.Size, ioctlStopAttachAttempts, calls)
	require.NoError(t, err)
	require.Equal(t, int32(3), request.Count)
	require.Equal(t, int32(1), closed.Load())
	_, err = newStopAttachIOCTL(nil, 3241)
	require.Error(t, err)
	_, err = newStopAttachIOCTL(&meta, 0)
	require.Error(t, err)
	copy(meta.USBBusID[:], make([]byte, 32))
	copy(meta.USBBusID[:], "17-4")
	_, err = newStopAttachIOCTL(&meta, 3241)
	require.Error(t, err, "Legacy/numeric locations never qualify")
}

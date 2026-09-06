//go:build windows

package api

import (
	"context"
	"fmt"
	"unsafe"

	"github.com/Alia5/VIIPER/usbip"
	"golang.org/x/sys/windows"
)

// usbip-win2 0.9.7.7 vhci::ioctl::stop_attach_attempts, not PLUGOUT.
// All three location strings must be present: zero strings mean stop ALL.
type stopAttachIOCTL struct {
	attachIOCTL
	Count int32
}

const ioctlStopAttachAttempts = (fileDeviceUnknown << 16) | ((fileReadData | fileWriteData) << 14) | (0x805 << 2)

func newStopAttachIOCTL(meta *usbip.ExportMeta, port uint16) (stopAttachIOCTL, error) {
	alias, err := validatedAutoAttachBusID(meta, port)
	if err != nil || !usbip.ValidProductionXboxOneBusID(alias) {
		return stopAttachIOCTL{}, fmt.Errorf("invalid production retry location")
	}
	location, err := newAttachIOCTL(meta, port)
	if err != nil {
		return stopAttachIOCTL{}, err
	}
	request := stopAttachIOCTL{attachIOCTL: location}
	request.Size = uint32(unsafe.Sizeof(request))
	return request, nil
}

func stopLocalhostXboxOneRetries(ctx context.Context, meta usbip.ExportMeta, port uint16) (int32, error) {
	request, err := newStopAttachIOCTL(&meta, port)
	if err != nil {
		return 0, err
	}
	path, err := getDeviceInterfacePath(&deviceGUID)
	if err != nil {
		return 0, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	_, err = submitNativeIOCTLContext(ctx, handle, &request, (*byte)(unsafe.Pointer(&request)),
		request.Size, request.Size, ioctlStopAttachAttempts, windowsNativeAttachOperations())
	if err != nil {
		return 0, err
	}
	if request.Count < 0 {
		return 0, fmt.Errorf("invalid stopped retry count")
	}
	return request.Count, nil
}

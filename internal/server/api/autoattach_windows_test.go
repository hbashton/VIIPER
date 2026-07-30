//go:build windows

package api

import (
	"strings"
	"testing"
	"unsafe"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

func TestUsbipAttachABISizes(t *testing.T) {
	require.Equal(t, uintptr(1116), unsafe.Sizeof(attachIOCTL{}),
		"usbip-win2 0.9.7.8 plugin_hardware ABI changed")
}

func TestDS4WindowsOwnerSerialFitsUsbip0978Contract(t *testing.T) {
	first := buildDS4WindowsOwnerSerial(&usbip.ExportMeta{
		BusID: 17,
		DevID: 3,
	}, 3241)
	same := buildDS4WindowsOwnerSerial(&usbip.ExportMeta{
		BusID: 17,
		DevID: 3,
	}, 3241)
	different := buildDS4WindowsOwnerSerial(&usbip.ExportMeta{
		BusID: 17,
		DevID: 4,
	}, 3241)

	require.Len(t, first, 15)
	require.True(t, strings.HasPrefix(first, "DS4W"))
	require.Equal(t, first, same)
	require.NotEqual(t, first, different)
	for _, character := range first {
		require.True(t,
			character >= '0' && character <= '9' ||
				character >= 'A' && character <= 'Z')
	}
}

func TestNativeAutoAttachResultCarriesExactPortAndOwnerSerial(t *testing.T) {
	meta := &usbip.ExportMeta{BusID: 17, DevID: 3}
	ownerSerial := buildDS4WindowsOwnerSerial(meta, 3241)

	got := AutoAttachResult{
		USBIPPort:        7,
		USBIPOwnerSerial: ownerSerial,
	}

	require.Equal(t, int32(7), got.USBIPPort)
	require.Equal(t, ownerSerial, got.USBIPOwnerSerial)
	require.Len(t, got.USBIPOwnerSerial, 15)
}

func TestUsbip0978SerialFieldIsAppended(t *testing.T) {
	var current attachIOCTL
	require.Equal(t, uintptr(1097), unsafe.Offsetof(current.Serial),
		"the 0.9.7.8 serial field must follow the complete 0.9.7.7 ABI")
}

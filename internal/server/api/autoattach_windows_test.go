//go:build windows

package api

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func TestUsbipAttachABISizes(t *testing.T) {
	require.Equal(t, uintptr(1100), unsafe.Sizeof(attachIOCTL{}),
		"usbip-win2 0.9.7.7 plugin_hardware ABI changed")
}

func TestNativeAutoAttachResultCarriesExactPort(t *testing.T) {
	got := AutoAttachResult{
		USBIPPort: 7,
	}

	require.Equal(t, int32(7), got.USBIPPort)
	require.Empty(t, got.USBIPOwnerSerial)
}

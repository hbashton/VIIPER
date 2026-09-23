package usb

import (
	"testing"

	usbdesc "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

// Exported only in test builds so the external-package integration test can
// supply a real DualSense without an internal/server/api import cycle.
func AssertEdgeProfileStallTransport(t *testing.T, device usbdesc.Device) {
	t.Helper()
	stream := newTransactionalControlStream(t, device)
	var hidInterface uint16
	for _, iface := range device.GetDescriptor().Interfaces {
		if iface.HID != nil {
			hidInterface = uint16(iface.Descriptor.BInterfaceNumber)
			break
		}
	}
	seq := uint32(1)
	for _, id := range []byte{0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x68,
		0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7A, 0x7B} {
		read := submitTransactionalControl(t, stream.client, seq, usbip.DirIn,
			transactionalControlSetup(0xA1, 1, 0x300|uint16(id), hidInterface, 64))
		require.Equal(t, int32(errPipe), read.status, "GET feature %02X", id)
		require.Zero(t, read.actual)
		seq++
		data := make([]byte, 64)
		data[0] = id
		write := submitTransactionalControl(t, stream.client, seq, usbip.DirOut,
			transactionalControlSetup(0x21, 9, 0x300|uint16(id), hidInterface, 64), data)
		require.Equal(t, int32(errPipe), write.status, "SET feature %02X", id)
		require.Zero(t, write.actual)
		seq++
	}
	// A STALL ends that request, not the device connection. Ordinary descriptor
	// and implemented firmware reads must still take their unchanged paths.
	descriptor := submitTransactionalControl(t, stream.client, seq, usbip.DirIn,
		transactionalControlSetup(0x80, 6, 0x100, 0, 18))
	require.Zero(t, descriptor.status)
	require.Equal(t, uint32(18), descriptor.actual)
	firmware := submitTransactionalControl(t, stream.client, seq+1, usbip.DirIn,
		transactionalControlSetup(0xA1, 1, 0x320, hidInterface, 64))
	require.Zero(t, firmware.status)
	require.NotEmpty(t, firmware.data)
	require.Equal(t, byte(0x20), firmware.data[0])
}

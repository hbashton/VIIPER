package usb

import (
	"encoding/binary"
	"net"
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

func AssertEdgeConfigurationStallTransport(t *testing.T, device usbdesc.Device) {
	t.Helper()
	stream := newTransactionalControlStream(t, device)
	var hidInterface uint16
	var outputEndpoint uint32
	for _, iface := range device.GetDescriptor().Interfaces {
		if iface.HID != nil {
			hidInterface = uint16(iface.Descriptor.BInterfaceNumber)
			for _, endpoint := range iface.Endpoints {
				if endpoint.BEndpointAddress&0x80 == 0 {
					outputEndpoint = uint32(endpoint.BEndpointAddress & 0x0F)
				}
			}
			break
		}
	}
	require.NotZero(t, outputEndpoint)
	configured := submitTransactionalControl(t, stream.client, 1, usbip.DirOut,
		transactionalControlSetup(0, 9, 1, 0, 0))
	require.Zero(t, configured.status)
	seq := uint32(2)
	for _, endpoint := range []uint32{0, outputEndpoint} {
		for _, size := range []int{48, 64} {
			for _, flagOffset := range []int{39, 41} {
				for _, withID := range []bool{true, false} {
					data := make([]byte, size)
					data[0], data[1], data[3], data[4] = 2, 0x03, 100, 180
					data[flagOffset] = 0x80
					if flagOffset == 39 {
						data[41] = 3 // trigger-deadzone preview, not adaptive feedback
					}
					if size > 50 {
						data[50] = 123
					}
					if !withID {
						data = data[1:]
					}
					setup := transactionalControlSetup(0x21, 9, 0x202, hidInterface, uint16(len(data)))
					response := submitEdgeOutputCommand(t, stream.client, seq, endpoint, setup, data)
					if endpoint == 0 {
						require.Equal(t, int32(errPipe), response.status, "configuration must STALL on EP0")
					} else {
						require.Equal(t, int32(errNoSpace), response.status, "interrupt configuration must fail admission")
					}
					require.Zero(t, response.actual)
					seq++
				}
			}
		}
	}
	// The same stream must continue accepting ordinary game feedback after the
	// rejected configuration packets. No endpoint reset or reconnect is needed.
	for _, endpoint := range []uint32{0, outputEndpoint} {
		data := []byte{2, 3, 0, 17, 29}
		response := submitEdgeOutputCommand(t, stream.client, seq, endpoint,
			transactionalControlSetup(0x21, 9, 0x202, hidInterface, uint16(len(data))), data)
		require.Zero(t, response.status)
		require.Equal(t, uint32(len(data)), response.actual)
		seq++
	}
	firmware := submitTransactionalControl(t, stream.client, seq, usbip.DirIn,
		transactionalControlSetup(0xA1, 1, 0x320, hidInterface, 64))
	require.Zero(t, firmware.status)
	require.Equal(t, byte(0x20), firmware.data[0])
}

func submitEdgeOutputCommand(t *testing.T, connection net.Conn, seq, endpoint uint32,
	setup [8]byte, data []byte) transactionalControlResponse {
	t.Helper()
	command := usbip.CmdSubmit{
		Basic:             usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq, Dir: usbip.DirOut, Ep: endpoint},
		TransferBufferLen: uint32(len(data)), NumberOfPackets: -1, Setup: setup,
	}
	require.NoError(t, command.Write(connection))
	_, err := connection.Write(data)
	require.NoError(t, err)
	var header [retSubmitHeaderSize]byte
	require.NoError(t, usbip.ReadExactly(connection, header[:]))
	require.Equal(t, seq, binary.BigEndian.Uint32(header[4:8]))
	// An OUT response acknowledges bytes but has no returned payload.
	return transactionalControlResponse{status: int32(binary.BigEndian.Uint32(header[20:24])),
		actual: binary.BigEndian.Uint32(header[24:28])}
}

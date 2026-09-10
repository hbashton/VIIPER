package usb

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/require"
)

type outputCommandAdmissionTestDevice struct {
	*transactionalInterruptOutTestDevice
	accept   atomic.Bool
	attempts atomic.Int32
}

func (device *outputCommandAdmissionTestDevice) TryHandleOutputCommand(
	endpoint uint8, setup [8]byte, payload []byte,
) (bool, bool) {
	if len(payload) == 0 || payload[0] != 0x02 ||
		(endpoint != 0 && endpoint != 1) {
		return false, false
	}
	device.attempts.Add(1)
	return true, device.accept.Load()
}

func TestOutputCommandAdmissionReportsFullWithoutDisconnectingStream(t *testing.T) {
	for _, endpoint := range []uint32{0, 1} {
		t.Run(map[uint32]string{0: "control-set-report", 1: "interrupt-out"}[endpoint], func(t *testing.T) {
			device := &outputCommandAdmissionTestDevice{
				transactionalInterruptOutTestDevice: newTransactionalInterruptOutTestDevice(),
			}
			bus := virtualbus.New(249)
			_, err := bus.Add(device)
			require.NoError(t, err)
			server := New(ServerConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			require.NoError(t, server.AddBus(bus))
			serverConn, clientConn := net.Pipe()
			require.NoError(t, clientConn.SetDeadline(time.Now().Add(2*time.Second)))
			done := make(chan error, 1)
			go func() { done <- server.handleUrbStream(serverConn, device) }()
			t.Cleanup(func() {
				_ = clientConn.Close()
				_ = serverConn.Close()
				_ = bus.Close()
				<-done
			})

			payload := []byte{0x02, 0x03, 0, 90, 120}
			submit := func(seq uint32) (int32, uint32) {
				command := usbip.CmdSubmit{
					Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode, Seqnum: seq,
						Dir: usbip.DirOut, Ep: endpoint},
					TransferBufferLen: uint32(len(payload)), NumberOfPackets: -1,
					Setup: [8]byte{0x21, 0x09, 0x02, 0x02, 0, 0, byte(len(payload)), 0},
				}
				require.NoError(t, command.Write(clientConn))
				_, err := clientConn.Write(payload)
				require.NoError(t, err)
				var response [retSubmitHeaderSize]byte
				require.NoError(t, usbip.ReadExactly(clientConn, response[:]))
				require.Equal(t, seq, binary.BigEndian.Uint32(response[4:8]))
				return int32(binary.BigEndian.Uint32(response[20:24])),
					binary.BigEndian.Uint32(response[24:28])
			}
			status, actual := submit(1)
			require.Equal(t, int32(errNoSpace), status, "full command queue was falsely acknowledged")
			require.Zero(t, actual)
			device.accept.Store(true)
			status, actual = submit(2)
			require.Zero(t, status)
			require.Equal(t, uint32(len(payload)), actual)
			require.Equal(t, int32(2), device.attempts.Load())
			snapshot := device.snapshot()
			require.Zero(t, snapshot.claimCalls, "admission-owned command fell through to another owner")
			require.Zero(t, snapshot.legacyCalls)
		})
	}
}

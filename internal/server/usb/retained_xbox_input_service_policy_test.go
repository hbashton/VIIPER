package usb

import (
	"io"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/require"
)

// This is a virtual-time transport integration test, not a physical-controller
// or Windows input-consumer latency measurement. Every counted report is decoded
// and matched to a uniquely changing state through the real retained journal.
func TestRetainedXboxInputServicePolicyDeliversDistinctJournalStates(t *testing.T) {
	forEachRetainedInputServicePolicy(t, func(t *testing.T, milliseconds int, interval time.Duration) {
		synctest.Test(t, func(t *testing.T) {
			const authorityID uint64 = 0x9701
			const deviceID uint64 = 0x9702
			device := newRetainedXboxDeviceIntegrationDevice(t, authorityID, deviceID)
			expectedDescriptor, err := sealRetainedImportDescriptor(device.GetDescriptor())
			require.NoError(t, err)
			require.EqualValues(t, 2, expectedDescriptor.Device.Speed)
			// The existing authorized fixture advertises an 8 ms IN interval.
			// Default policy must preserve that value, not silently select 4 ms.
			if milliseconds == 0 {
				interval = 8 * time.Millisecond
			}
			bus := virtualbus.New(970)
			t.Cleanup(func() { _ = bus.Close() })
			_, err = bus.Add(device)
			require.NoError(t, err)
			server := New(ServerConfig{
				ConnectionTimeout: time.Second, RetainedImportAuthorityID: authorityID,
				RetainedInputServiceMS: milliseconds,
			}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			require.NoError(t, server.AddBus(bus))
			serverConn, client := netPipeWithDeadline(t, 970)
			result := make(chan error, 1)
			go func() { result <- server.handleConn(serverConn) }()
			writeRetainedImportRequest(t, client, "970-1")
			readSuccessfulRetainedImport(t, client)

			getConfigurationDescriptor := [8]byte{0x80, 0x06, 0, 0x02, 0, 0, 255, 0}
			writeRetainedSubmit(t, client, 300, usbip.DirIn, 0, 255, getConfigurationDescriptor, nil)
			sequence, status, _, payload := readRetainedSubmitResponse(t, client)
			require.EqualValues(t, 300, sequence)
			require.Zero(t, status)
			require.Equal(t, server.buildConfigDescriptor(&expectedDescriptor), payload,
				"the host receives the unchanged advertised endpoint intervals")
			setConfiguration := [8]byte{0x00, 0x09, 0x01}
			writeRetainedSubmit(t, client, 301, usbip.DirOut, 0, 0, setConfiguration, nil)
			_, status, _, _ = readRetainedSubmitResponseForDirection(t, client, usbip.DirOut)
			require.Zero(t, status)
			writeRetainedSubmit(t, client, 302, usbip.DirIn, 1, 64, [8]byte{}, nil)
			_, status, _, payload = readRetainedSubmitResponse(t, client)
			require.Zero(t, status)
			_, err = xboxone.DecodeHelloMessage(payload)
			require.NoError(t, err)

			probe := []byte{0x05, 0x20, 0x02, 0x0f,
				0x06, 0, 0, 0, 0, 0, 0, 0x55, 0x53, 0, 0, 0, 0, 0, 0}
			writeRetainedSubmit(t, client, 303, usbip.DirOut, 1, uint32(len(probe)), [8]byte{}, probe)
			_, status, actual, _ := readRetainedSubmitResponseForDirection(t, client, usbip.DirOut)
			require.Zero(t, status)
			require.EqualValues(t, len(probe), actual)
			start := []byte{0x05, 0x20, 0x03, 0x01, byte(xboxone.SetDeviceStateStart)}
			writeRetainedSubmit(t, client, 304, usbip.DirOut, 1, uint32(len(start)), [8]byte{}, start)
			_, status, actual, _ = readRetainedSubmitResponseForDirection(t, client, usbip.DirOut)
			require.Zero(t, status)
			require.EqualValues(t, len(start), actual)
			writeRetainedSubmit(t, client, 305, usbip.DirIn, 1, 64, [8]byte{}, nil)
			_, status, _, payload = readRetainedSubmitResponse(t, client)
			require.Zero(t, status)
			_, _, err = xboxone.DecodeExtendedStatusNoEventsMessage(payload)
			require.NoError(t, err)
			synctest.Wait()

			const sampleWindow = 256 * time.Millisecond
			sampleCount := int(sampleWindow / interval)
			var firstPresentation time.Time
			var previousState xboxone.InputStateV1
			for i := 0; i < sampleCount; i++ {
				want := xboxone.InputStateV1{
					A: i%2 == 0, LeftTrigger: uint16(i + 1),
					LeftStickX: int16(i + 1), RightStickY: -int16(i + 1),
				}
				var wire [xboxone.SemanticInputWireSize]byte
				require.NoError(t, xboxone.EncodeSemanticInputWireV1Into(wire[:], want))
				require.NoError(t, device.PublishSemanticInputWire(uint64(i+2), wire[:]))
				writeRetainedSubmit(t, client, uint32(400+i), usbip.DirIn, 1, 64, [8]byte{}, nil)
				sequence, status, actual, payload = readRetainedSubmitResponse(t, client)
				require.EqualValues(t, 400+i, sequence)
				require.Zero(t, status)
				require.Positive(t, actual)
				_, input, decodeErr := xboxone.DecodeGamepadInputMessage(payload)
				require.NoError(t, decodeErr, "only distinct, valid GIP input counts as a sample")
				require.Equal(t, want, input.State, "journal ordering and analog values must survive presentation")
				require.NotEqual(t, previousState, input.State, "duplicate readings cannot count toward throughput")
				previousState = input.State
				if i == 0 {
					firstPresentation = time.Now()
				}
				require.Equal(t, time.Duration(i)*interval, time.Since(firstPresentation),
					"actual returned GIP input must obey the configured service cadence")
				synctest.Wait()
			}
			actualDescriptor, err := sealRetainedImportDescriptor(device.GetDescriptor())
			require.NoError(t, err)
			require.Equal(t, expectedDescriptor, actualDescriptor,
				"including full-speed identity and the advertised 8 ms IN / 4 ms OUT intervals")
			require.NoError(t, server.Close())
			require.NoError(t, <-result)
			require.Empty(t, bus.Devices(), "one-shot import retires cleanly after real journal delivery")
		})
	})
}

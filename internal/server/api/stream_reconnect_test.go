package api_test

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	"github.com/Alia5/VIIPER/internal/log"
	_ "github.com/Alia5/VIIPER/internal/registry" // Register devices.
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/api/handler"
	srvusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/viiperclient"
	"github.com/Alia5/VIIPER/virtualbus"
)

// TestAPIServer_DeviceStreamCloseFirstReconnectKeepsDS4MicrophoneQueue proves
// the complete lifecycle contract used by DS4Windows recovery, including its
// natural close-old then open-new ordering. The replacement connection retains
// buffered capture, while a final disconnect still resets it after the grace.
func TestAPIServer_DeviceStreamCloseFirstReconnectKeepsDS4MicrophoneQueue(t *testing.T) {
	const (
		busID          = uint32(71004)
		cleanupTimeout = 750 * time.Millisecond
	)

	usbServer := srvusb.New(srvusb.ServerConfig{Addr: "127.0.0.1:0"},
		slog.Default(), log.NewRaw(nil))
	apiServer := api.New(usbServer, "127.0.0.1:0", api.ServerConfig{
		Addr:                        "127.0.0.1:0",
		DeviceHandlerConnectTimeout: cleanupTimeout,
	}, slog.Default())
	apiServer.Router().Register("bus/{id}/add",
		handler.BusDeviceAdd(usbServer, apiServer))
	apiServer.Router().RegisterStream("bus/{busId}/{deviceid}",
		api.DeviceStreamHandler(usbServer))
	require.NoError(t, apiServer.Start())
	defer apiServer.Close()
	defer usbServer.Close() //nolint:errcheck

	bus, err := virtualbus.NewWithBusID(busID)
	require.NoError(t, err)
	defer bus.Close() //nolint:errcheck
	require.NoError(t, usbServer.AddBus(bus))

	client := viiperclient.New(apiServer.Addr())
	created, err := client.DeviceAdd(busID, "dualshock4micv2", nil)
	require.NoError(t, err)
	require.NotNil(t, created)

	devices := bus.Devices()
	require.Len(t, devices, 1)
	ds4, ok := devices[0].(*dualshock4.DualShock4)
	require.True(t, ok)
	ds4.SetInterfaceAltSetting(dualshock4.InterfaceMicrophone, 1)

	first, err := client.OpenStream(context.Background(), busID, created.DevID)
	require.NoError(t, err)
	defer first.Close() //nolint:errcheck
	writeDS4MicrophoneFrames(t, first, 6, 0x11)
	require.Eventually(t, func() bool {
		state := ds4.GetDeviceSpecificArgs()
		return state["microphoneQueuePrimed"] == true
	}, time.Second, 5*time.Millisecond)

	require.NoError(t, first.Close())
	// Give the server enough time to observe EOF and finish the old generation;
	// this deliberately exercises close-first rather than displacement by claim.
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, dualshock4.USBMicrophoneClientFrameSize*6,
		ds4.GetDeviceSpecificArgs()["queuedMicrophoneBytes"])

	second, err := client.OpenStream(context.Background(), busID, created.DevID)
	require.NoError(t, err)
	defer second.Close() //nolint:errcheck
	writeDS4MicrophoneFrames(t, second, 1, 0x44)

	// Wait beyond the old generation's 250 ms finalization deadline. Its stale
	// timer must be unable to erase either the retained or replacement frame.
	time.Sleep(300 * time.Millisecond)
	state := ds4.GetDeviceSpecificArgs()
	require.Equal(t, dualshock4.USBMicrophoneClientFrameSize*7,
		state["queuedMicrophoneBytes"])
	require.Equal(t, true, state["microphoneQueuePrimed"])
	require.Len(t, bus.Devices(), 1,
		"displaced stream generation removed the live virtual device")

	// With no next replacement, the final current stream performs the one
	// definitive reset after the reconnect grace, before device removal.
	require.NoError(t, second.Close())
	require.Eventually(t, func() bool {
		return ds4.GetDeviceSpecificArgs()["queuedMicrophoneBytes"] == 0
	}, 500*time.Millisecond, 5*time.Millisecond)
	require.Len(t, bus.Devices(), 1)
}

func writeDS4MicrophoneFrames(t *testing.T, stream *viiperclient.DeviceStream,
	count uint32, value byte) {
	t.Helper()
	for sequence := uint32(0); sequence < count; sequence++ {
		payload := make([]byte, dualshock4.USBMicrophoneClientFrameSize)
		for index := range payload {
			payload[index] = value
		}
		frame := makeDS4StreamFrame(sequence, payload)
		written, err := stream.Write(frame)
		require.NoError(t, err)
		require.Equal(t, len(frame), written)
	}
}

func makeDS4StreamFrame(sequence uint32, payload []byte) []byte {
	const headerSize = 16
	header := make([]byte, headerSize)
	copy(header[0:4], []byte{'V', 'P', 'C', 'M'})
	header[4] = dualshock4.StreamFrameVersionV2
	header[5] = dualshock4.StreamFrameMicrophonePCM
	binary.LittleEndian.PutUint16(header[6:8], uint16(len(payload)))
	binary.LittleEndian.PutUint32(header[8:12], sequence)
	hash := crc32.NewIEEE()
	_, _ = hash.Write(header[4:12])
	_, _ = hash.Write(payload)
	binary.LittleEndian.PutUint32(header[12:16], hash.Sum32())
	return append(header, payload...)
}

func TestAPIServer_DeviceStreamCloseFirstReconnectPreservesDualSenseMicrophoneQueue(t *testing.T) {
	const (
		busID          = uint32(71005)
		cleanupTimeout = 750 * time.Millisecond
	)

	usbServer := srvusb.New(srvusb.ServerConfig{Addr: "127.0.0.1:0"},
		slog.Default(), log.NewRaw(nil))
	apiServer := api.New(usbServer, "127.0.0.1:0", api.ServerConfig{
		Addr:                        "127.0.0.1:0",
		DeviceHandlerConnectTimeout: cleanupTimeout,
	}, slog.Default())
	apiServer.Router().Register("bus/{id}/add",
		handler.BusDeviceAdd(usbServer, apiServer))
	apiServer.Router().RegisterStream("bus/{busId}/{deviceid}",
		api.DeviceStreamHandler(usbServer))
	require.NoError(t, apiServer.Start())
	defer apiServer.Close()
	defer usbServer.Close() //nolint:errcheck

	bus, err := virtualbus.NewWithBusID(busID)
	require.NoError(t, err)
	defer bus.Close() //nolint:errcheck
	require.NoError(t, usbServer.AddBus(bus))

	client := viiperclient.New(apiServer.Addr())
	created, err := client.DeviceAdd(busID, "dualsensecombinedmicv2", nil)
	require.NoError(t, err)
	require.NotNil(t, created)

	devices := bus.Devices()
	require.Len(t, devices, 1)
	ds, ok := devices[0].(*dualsense.DualSense)
	require.True(t, ok)
	ds.SetInterfaceAltSetting(dualsense.InterfaceMicrophone, 1)

	first, err := client.OpenStream(context.Background(), busID, created.DevID)
	require.NoError(t, err)
	defer first.Close() //nolint:errcheck
	writeDualSenseMicrophoneFrames(t, first, 6, 0x22)
	require.Eventually(t, func() bool {
		return ds.GetDeviceSpecificArgs()["microphoneQueuePrimed"] == true
	}, time.Second, 5*time.Millisecond)

	require.NoError(t, first.Close())
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, dualsense.USBMicrophoneClientFrameSize*6,
		ds.GetDeviceSpecificArgs()["queuedMicrophoneBytes"])

	second, err := client.OpenStream(context.Background(), busID, created.DevID)
	require.NoError(t, err)
	defer second.Close() //nolint:errcheck
	writeDualSenseMicrophoneFrames(t, second, 1, 0x55)

	time.Sleep(300 * time.Millisecond)
	state := ds.GetDeviceSpecificArgs()
	require.Equal(t, dualsense.USBMicrophoneClientFrameSize*7,
		state["queuedMicrophoneBytes"])
	require.Equal(t, true, state["microphoneQueuePrimed"])
	require.Len(t, bus.Devices(), 1,
		"displaced stream generation removed the live virtual device")

	require.NoError(t, second.Close())
	require.Eventually(t, func() bool {
		return ds.GetDeviceSpecificArgs()["queuedMicrophoneBytes"] == 0
	}, 500*time.Millisecond, 5*time.Millisecond)
	require.Len(t, bus.Devices(), 1)
}

func writeDualSenseMicrophoneFrames(t *testing.T,
	stream *viiperclient.DeviceStream, count uint32, value byte) {
	t.Helper()
	for sequence := uint32(0); sequence < count; sequence++ {
		payload := make([]byte, dualsense.USBMicrophoneClientFrameSize)
		for index := range payload {
			payload[index] = value
		}
		frame := makeDualSenseStreamFrame(sequence, payload)
		written, err := stream.Write(frame)
		require.NoError(t, err)
		require.Equal(t, len(frame), written)
	}
}

func makeDualSenseStreamFrame(sequence uint32, payload []byte) []byte {
	const headerSize = 16
	header := make([]byte, headerSize)
	copy(header[0:4], []byte{'V', 'P', 'C', 'M'})
	header[4] = dualsense.StreamFrameVersionV2
	header[5] = dualsense.StreamFrameMicrophonePCM
	binary.LittleEndian.PutUint16(header[6:8], uint16(len(payload)))
	binary.LittleEndian.PutUint32(header[8:12], sequence)
	hash := crc32.NewIEEE()
	_, _ = hash.Write(header[4:12])
	_, _ = hash.Write(payload)
	binary.LittleEndian.PutUint32(header[12:16], hash.Sum32())
	return append(header, payload...)
}

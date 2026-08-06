package xboxseries_test

import (
	"context"
	"io"
	"testing"
	"time"

	viiperTesting "github.com/Alia5/VIIPER/_testing"
	"github.com/Alia5/VIIPER/device/xboxseries"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/api/handler"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/viiperclient"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/require"
)

func TestAPIStreamPreservesNativeFourMotorFeedback(t *testing.T) {
	server := viiperTesting.NewTestServer(t)
	defer server.UsbServer.Close() //nolint:errcheck
	defer server.ApiServer.Close() //nolint:errcheck

	router := server.ApiServer.Router()
	router.Register("bus/{id}/add",
		handler.BusDeviceAdd(server.UsbServer, server.ApiServer))
	router.RegisterStream("bus/{busId}/{deviceid}",
		api.DeviceStreamHandler(server.UsbServer))
	require.NoError(t, server.ApiServer.Start())

	bus, err := virtualbus.NewWithBusID(1)
	require.NoError(t, err)
	defer bus.Close() //nolint:errcheck
	require.NoError(t, server.UsbServer.AddBus(bus))

	client := viiperclient.New(server.ApiServer.Addr())
	stream, _, err := client.AddDeviceAndConnect(context.Background(),
		bus.BusID(), "xboxseries", nil)
	require.NoError(t, err)
	defer stream.Close() //nolint:errcheck

	usbClient := viiperTesting.NewUsbIpClient(t, server.UsbServer.Addr())
	devices, err := usbClient.ListDevices()
	require.NoError(t, err)
	require.Len(t, devices, 1)
	attached, err := usbClient.AttachDevice(devices[0].BusID)
	require.NoError(t, err)
	defer attached.Conn.Close() //nolint:errcheck

	// GIP direct-motor command: LT, RT, left-body, right-body, followed by
	// duration/delay/repeat. Keep the duration long enough to prove the first
	// API feedback frame is the authored state rather than the timed stop.
	hostPacket := []byte{0x09, 0x00, 0x21, 0x09,
		0x00, 0x0F, 100, 50, 25, 75, 255, 0, 0}
	require.NoError(t, usbClient.Submit(attached.Conn, usbip.DirOut, 1,
		hostPacket, nil))

	var feedback [7]byte
	require.NoError(t, stream.SetReadDeadline(time.Now().Add(
		viiperTesting.IntegrationTimeout)))
	_, err = io.ReadFull(stream, feedback[:])
	require.NoError(t, err)
	require.Equal(t, xboxseries.MotorState{
		LeftMotor: 64, RightMotor: 191,
		LeftImpulse: 255, RightImpulse: 128,
		Duration: 255,
	}, decodeMotor(t, feedback[:]))
}

func decodeMotor(t *testing.T, data []byte) xboxseries.MotorState {
	t.Helper()
	var state xboxseries.MotorState
	require.NoError(t, state.UnmarshalBinary(data))
	return state
}

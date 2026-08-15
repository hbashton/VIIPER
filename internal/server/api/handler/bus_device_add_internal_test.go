package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/Alia5/VIIPER/device/xbox360"
	th "github.com/Alia5/VIIPER/internal/_testing"
	"github.com/Alia5/VIIPER/internal/server/api"
	usbs "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
	usbdevice "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/viiperclient"
	"github.com/Alia5/VIIPER/viipertypes"
	"github.com/Alia5/VIIPER/virtualbus"
)

type apiNativeCorrelationDriver struct {
	destroyed []udecx.DeviceIdentity
}

func (*apiNativeCorrelationDriver) CreateDevice(_ context.Context, device udecx.CreateDevice) (udecx.DeviceRegistration, error) {
	return udecx.DeviceRegistration{
		DeviceIdentity:       udecx.DeviceIdentity{DeviceID: device.DeviceID, Generation: device.Generation},
		Speed:                device.Speed,
		ControllerSessionID:  17,
		USB20PortNumber:      5,
		ControllerInstanceID: `ROOT\VIIPERUDE\0042`,
	}, nil
}

func (d *apiNativeCorrelationDriver) DestroyDevice(_ context.Context, identity udecx.DeviceIdentity) error {
	d.destroyed = append(d.destroyed, identity)
	return nil
}
func (*apiNativeCorrelationDriver) Dequeue(ctx context.Context, _ []byte) (udecx.Operation, error) {
	<-ctx.Done()
	return udecx.Operation{}, ctx.Err()
}
func (*apiNativeCorrelationDriver) Complete(context.Context, udecx.Completion) error { return nil }
func (*apiNativeCorrelationDriver) QueryStats(context.Context) (udecx.Stats, error) {
	return udecx.Stats{}, nil
}

type apiNativeCorrelationProcessor struct{}

func (*apiNativeCorrelationProcessor) Process(context.Context, usbdevice.Device, udecx.Operation) (udecx.Completion, error) {
	return udecx.Completion{}, nil
}
func (*apiNativeCorrelationProcessor) Lifecycle(context.Context, usbdevice.Device, udecx.Operation) error {
	return nil
}
func (*apiNativeCorrelationProcessor) Reset(usbdevice.Device, udecx.DeviceIdentity) {}

func TestBusDeviceAddReturnsNativeAutoAttachMetadata(t *testing.T) {
	const ownerSerial = "DS4W123456789AB"
	type attachCall struct {
		busID  uint32
		devID  uint32
		native bool
	}
	attachCalls := make(chan attachCall, 1)
	originalAutoAttach := attachLocalhostClientWithResult
	t.Cleanup(func() { attachLocalhostClientWithResult = originalAutoAttach })
	attachLocalhostClientWithResult = func(_ context.Context, meta *usbip.ExportMeta, _ uint16, native bool, _ *slog.Logger) (api.AutoAttachResult, error) {
		attachCalls <- attachCall{busID: meta.BusID, devID: meta.DevID, native: native}
		return api.AutoAttachResult{
			USBIPPort:        7,
			USBIPOwnerSerial: ownerSerial,
		}, nil
	}

	addr, _, done := th.StartAPIServer(t, func(r *api.Router, s *usbs.Server, apiSrv *api.Server) {
		apiSrv.Config().ConnectionTimeout = time.Second
		apiSrv.Config().AutoAttachLocalClient = true
		apiSrv.Config().AutoAttachWindowsNative = true

		bus, err := virtualbus.NewWithBusID(80251)
		require.NoError(t, err)
		require.NoError(t, s.AddBus(bus))

		r.Register("bus/{id}/add", BusDeviceAdd(s, apiSrv))
	})
	defer done()

	client := viiperclient.NewTransport(addr)
	response, err := client.Do("bus/{id}/add", `{"type":"xbox360"}`, map[string]string{"id": "80251"})
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"busId": 80251,
		"devId": "1",
		"deviceSpecific": {"subType": 1},
		"vid": "0x045e",
		"pid": "0x028e",
		"transport": "usbip",
		"type": "xbox360",
		"usbipPort": 7,
		"usbipOwnerSerial": "DS4W123456789AB"
	}`, response)

	call := <-attachCalls
	require.Equal(t, uint32(80251), call.busID)
	require.Equal(t, uint32(1), call.devID)
	require.True(t, call.native)
}

func TestBusDeviceAddAndListReturnExactNativeCorrelation(t *testing.T) {
	const busID = uint32(81234)
	addr, _, done := th.StartAPIServer(t, func(r *api.Router, s *usbs.Server, apiSrv *api.Server) {
		apiSrv.Config().ConnectionTimeout = time.Second
		apiSrv.Config().DeviceHandlerConnectTimeout = 30 * time.Second
		bus, err := virtualbus.NewWithBusID(busID)
		require.NoError(t, err)
		require.NoError(t, s.AddBus(bus))
		host, err := udecx.NewHost(&apiNativeCorrelationDriver{}, &apiNativeCorrelationProcessor{}, 0)
		require.NoError(t, err)
		require.NoError(t, s.EnableNativeTransport(host))
		r.Register("bus/{id}/add", BusDeviceAdd(s, apiSrv))
		r.Register("bus/{id}/list", BusDevicesList(s))
	})
	defer done()

	client := viiperclient.NewTransport(addr)
	response, err := client.Do("bus/{id}/add", `{"type":"xbox360"}`,
		map[string]string{"id": strconv.FormatUint(uint64(busID), 10)})
	require.NoError(t, err)
	var created viipertypes.Device
	require.NoError(t, json.Unmarshal([]byte(response), &created))
	wantDeviceID := strconv.FormatUint(uint64(busID)<<32|1, 10)
	require.Equal(t, "native-ude", created.Transport)
	require.NotNil(t, created.NativeUDE)
	require.Equal(t, wantDeviceID, created.NativeUDE.DeviceID)
	require.Equal(t, uint32(1), created.NativeUDE.DeviceGeneration)
	require.Equal(t, "17", created.NativeUDE.ControllerSessionID)
	require.Equal(t, `ROOT\VIIPERUDE\0042`, created.NativeUDE.ControllerInstanceID)
	require.Equal(t, uint32(5), created.NativeUDE.USB20PortNumber)
	require.Zero(t, created.NativeUDE.USB30PortNumber)
	require.Zero(t, created.USBIPPort)
	require.Empty(t, created.USBIPOwnerSerial)

	response, err = client.Do("bus/{id}/list", nil,
		map[string]string{"id": strconv.FormatUint(uint64(busID), 10)})
	require.NoError(t, err)
	var listed viipertypes.DevicesListResponse
	require.NoError(t, json.Unmarshal([]byte(response), &listed))
	require.Len(t, listed.Devices, 1)
	require.Equal(t, created.NativeUDE, listed.Devices[0].NativeUDE)
	require.Equal(t, "native-ude", listed.Devices[0].Transport)
}

func TestNativeExactRemoveRejectsStaleReceiptAndPreservesSuccessor(t *testing.T) {
	const busID = uint32(81235)
	driver := &apiNativeCorrelationDriver{}
	addr, _, done := th.StartAPIServer(t, func(r *api.Router, s *usbs.Server, apiSrv *api.Server) {
		apiSrv.Config().ConnectionTimeout = time.Second
		apiSrv.Config().DeviceHandlerConnectTimeout = 30 * time.Second
		bus, err := virtualbus.NewWithBusID(busID)
		require.NoError(t, err)
		require.NoError(t, s.AddBus(bus))
		host, err := udecx.NewHost(driver, &apiNativeCorrelationProcessor{}, 0)
		require.NoError(t, err)
		require.NoError(t, s.EnableNativeTransport(host))
		r.Register("bus/{id}/add", BusDeviceAdd(s, apiSrv))
		r.Register("bus/{id}/list", BusDevicesList(s))
		r.Register("bus/{id}/remove", BusDeviceRemove(s))
		r.Register("bus/{id}/remove-native", BusDeviceRemoveNative(s))
		r.Register("bus/remove", BusRemove(s))
	})
	defer done()

	client := viiperclient.NewTransport(addr)
	params := map[string]string{"id": strconv.FormatUint(uint64(busID), 10)}
	response, err := client.Do("bus/{id}/add", `{"type":"xbox360"}`, params)
	require.NoError(t, err)
	var created viipertypes.Device
	require.NoError(t, json.Unmarshal([]byte(response), &created))
	require.NotNil(t, created.NativeUDE)

	response, err = client.Do("bus/{id}/remove", created.DevID, params)
	require.NoError(t, err)
	var unsafeRemove viipertypes.APIError
	require.NoError(t, json.Unmarshal([]byte(response), &unsafeRemove))
	require.Equal(t, 409, unsafeRemove.Status)
	require.Empty(t, driver.destroyed)

	staleNative := *created.NativeUDE
	staleNative.DeviceGeneration++
	staleRequest := viipertypes.NativeUDEDeviceRemoveRequest{
		DevID: created.DevID, Transport: "native-ude", NativeUDE: &staleNative,
	}
	response, err = client.Do("bus/{id}/remove-native", staleRequest, params)
	require.NoError(t, err)
	var conflict viipertypes.APIError
	require.NoError(t, json.Unmarshal([]byte(response), &conflict))
	require.Equal(t, 409, conflict.Status)
	require.Empty(t, driver.destroyed)

	response, err = client.Do("bus/remove", strconv.FormatUint(uint64(busID), 10), nil)
	require.NoError(t, err)
	var busConflict viipertypes.APIError
	require.NoError(t, json.Unmarshal([]byte(response), &busConflict))
	require.Equal(t, 409, busConflict.Status)
	require.Empty(t, driver.destroyed)

	response, err = client.Do("bus/{id}/list", nil, params)
	require.NoError(t, err)
	var listed viipertypes.DevicesListResponse
	require.NoError(t, json.Unmarshal([]byte(response), &listed))
	require.Len(t, listed.Devices, 1)
	require.Equal(t, created.NativeUDE, listed.Devices[0].NativeUDE)

	exactRequest := viipertypes.NativeUDEDeviceRemoveRequest{
		DevID: created.DevID, Transport: "native-ude", NativeUDE: created.NativeUDE,
	}
	response, err = client.Do("bus/{id}/remove-native", exactRequest, params)
	require.NoError(t, err)
	require.JSONEq(t, `{"busId":81235,"devId":"1"}`, response)
	require.Len(t, driver.destroyed, 1)
}

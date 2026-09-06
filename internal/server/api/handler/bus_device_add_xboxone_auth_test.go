package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/internal/log"
	"github.com/Alia5/VIIPER/internal/registry"
	"github.com/Alia5/VIIPER/internal/server/api"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/viipertypes"
	"github.com/Alia5/VIIPER/virtualbus"
)

type authenticatedXboxOneActivationTestConn struct{ net.Conn }

func (*authenticatedXboxOneActivationTestConn) VIIPERAuthenticated() bool {
	return true
}

func TestXboxOneFactoryRejectsUnauthenticatedRequestBeforeParsing(t *testing.T) {
	handler := BusDeviceAddAuthorizedXboxOne(nil, nil)
	err := handler(&api.Request{Ctx: context.Background()},
		&api.Response{}, slog.Default())
	apiErr, ok := err.(viipertypes.APIError)
	if !ok || apiErr.Status != 401 {
		t.Fatalf("factory error = %#v, want API status 401", err)
	}
}

func TestXboxOneActivationRejectsUnauthenticatedRequestBeforeLookup(t *testing.T) {
	handler := BusDeviceActivateAuthorizedXboxOne(nil, nil)
	err := handler(&api.Request{Ctx: context.Background()},
		&api.Response{}, slog.Default())
	apiErr, ok := err.(viipertypes.APIError)
	if !ok || apiErr.Status != 401 {
		t.Fatalf("activation error = %#v, want API status 401", err)
	}
}

func TestXboxOneActivationAcceptsPinned0977NativePortWithoutOwnerSerial(
	t *testing.T,
) {
	for _, test := range []struct {
		name   string
		result api.AutoAttachResult
		want   bool
	}{
		{name: "native 0.9.7.7", result: api.AutoAttachResult{USBIPPort: 7}, want: true},
		{name: "future owner token", result: api.AutoAttachResult{
			USBIPPort: 8, USBIPOwnerSerial: "DS4W123456789AB"}, want: true},
		{name: "missing port", result: api.AutoAttachResult{}, want: false},
		{name: "negative port", result: api.AutoAttachResult{USBIPPort: -1}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validXboxOneActivationAttachResult(test.result); got != test.want {
				t.Fatalf("validXboxOneActivationAttachResult(%+v) = %t, want %t",
					test.result, got, test.want)
			}
		})
	}
}

func TestXboxOneReadyConsumerActivatesPinned0977PortEndToEnd(t *testing.T) {
	const (
		busID       = uint32(63001)
		authorityID = uint64(0x4453345758423031)
		deviceID    = uint64(0x0000fffb01020304)
	)
	server := serverusb.New(serverusb.ServerConfig{
		Addr: "127.0.0.1:0", RetainedImportAuthorityID: authorityID,
		BusCleanupTimeout: time.Hour,
	}, slog.Default(), log.NewRaw(nil))
	bus, err := virtualbus.NewWithBusID(busID)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if server.GetBus(busID) == bus {
			_ = server.RemoveBus(busID)
		}
		_ = server.Close()
	})

	options := xboxone.ProductionRetainedUSBDeviceOptions{
		Identity: xboxone.ControllerIdentity{
			VendorID: 0xf00d, ProductID: 0xbeef,
			DeviceReleaseBCD: 0x0102, DeviceID: deviceID,
			Firmware: xboxone.FirmwareVersion{
				Major: 1, Minor: 2, Build: 3, Revision: 4,
			},
			HardwareMajor: 5, HardwareMinor: 6,
		},
		USB: xboxone.ControllerUSBConfig{
			MaxPower2mA: 0x32, OUTIntervalMS: 4, INIntervalMS: 4,
		},
		Strings: xboxone.ControllerUSBIdentityStrings{
			Manufacturer: "VIIPER test",
			Product:      "Production Xbox activation test",
			Serial:       "0000fffb01020304a1b2c3d4e5f60708",
		},
		IdentityAuthorization: xboxone.ControllerIdentityAuthorizationGranted,
		FeedbackBinding: xboxone.ControllerPersonaFeedbackBindingV1{
			Source:            controllerfeedback.SourceXboxOneVirtualDevice,
			PersonaGeneration: 1, DeviceGeneration: 2,
			TransportGeneration: 3, OwnershipEpoch: 4,
			TimeToLiveMicroseconds: 250_000,
		},
		ProtocolTimeMS: 10, AuthorityID: authorityID,
		ImportDeviceID: deviceID, LocalTimeout: 100 * time.Millisecond,
	}
	registration, err := registry.RegisterProductionXboxOneRetainedUSB(
		server, registry.ProductionXboxOneRetainedUSBRequest{
			BusID: busID, Options: options,
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registration.Close() })

	metas := bus.GetAllDeviceMetas()
	if len(metas) != 1 {
		t.Fatalf("registered devices = %d, want 1", len(metas))
	}
	device := metas[0].Dev
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	handlerDone := make(chan error, 1)
	go func() {
		handlerDone <- xboxone.ProductionStreamHandler(
			&authenticatedXboxOneActivationTestConn{Conn: serverConn},
			&device, slog.Default())
	}()
	ready := []byte{
		'X', '1', 'B', 'R', 1, 0x01, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
	}
	if _, err := clientConn.Write(ready); err != nil {
		t.Fatal(err)
	}
	readyAck := make([]byte, len(ready))
	if _, err := io.ReadFull(clientConn, readyAck); err != nil {
		t.Fatal(err)
	}
	wantReadyAck := append([]byte(nil), ready...)
	wantReadyAck[5] = 0x81
	if !bytes.Equal(readyAck, wantReadyAck) {
		t.Fatalf("ready ACK = % x, want % x", readyAck, wantReadyAck)
	}

	originalAutoAttach := attachLocalhostClientWithResult
	t.Cleanup(func() { attachLocalhostClientWithResult = originalAutoAttach })
	attachLocalhostClientWithResult = func(
		_ context.Context, meta *usbip.ExportMeta, _ uint16,
		native bool, _ *slog.Logger,
	) (api.AutoAttachResult, error) {
		if meta.BusID != busID || meta.DevID != metas[0].Meta.DevID || !native {
			t.Fatalf("attach target = %d-%d native=%t",
				meta.BusID, meta.DevID, native)
		}
		return api.AutoAttachResult{USBIPPort: 7}, nil
	}
	apiConfig := api.ServerConfig{AutoAttachLocalClient: true}
	apiConfig.AutoAttachWindowsNative = true
	apiServer := api.New(server, "127.0.0.1:0", apiConfig,
		slog.Default())
	response := &api.Response{}
	removalToken, present := registration.ProductionRemovalToken()
	if !present {
		t.Fatal("missing creation capability")
	}
	err = BusDeviceActivateAuthorizedXboxOne(server, apiServer)(
		&api.Request{
			Ctx: context.Background(), Authenticated: true,
			Payload: `{"version":1,"removalToken":"` + removalToken + `"}`,
			Params: map[string]string{
				"busId": "63001", "devId": "1",
			},
		}, response, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	var activated xboxOneActivationResponse
	if err := json.Unmarshal([]byte(response.JSON), &activated); err != nil {
		t.Fatal(err)
	}
	if activated.Version != 1 || !usbip.ValidProductionXboxOneBusID(activated.USBIPBusID) ||
		activated.USBIPPort != 7 || activated.USBIPOwnerSerial != "" {
		t.Fatalf("activation response = %+v", activated)
	}

	_ = clientConn.Close()
	select {
	case err := <-handlerDone:
		if err != nil {
			t.Fatalf("production stream close = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("production stream did not retire")
	}
}

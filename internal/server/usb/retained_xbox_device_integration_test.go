package usb

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

type retainedXboxDeviceIntegrationExecutor struct {
	marker        byte
	cancelEntered chan struct{}
	cancelRelease <-chan struct{}
}

func (*retainedXboxDeviceIntegrationExecutor) Execute(
	xboxone.ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func (*retainedXboxDeviceIntegrationExecutor) ResetAndDrain(time.Time) error {
	return nil
}

func (*retainedXboxDeviceIntegrationExecutor) ResetNeutral(
	xboxone.ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func (executor *retainedXboxDeviceIntegrationExecutor) CancelAndDrain(
	time.Time,
) error {
	if executor.cancelEntered != nil {
		close(executor.cancelEntered)
	}
	if executor.cancelRelease != nil {
		<-executor.cancelRelease
	}
	return nil
}

func (*retainedXboxDeviceIntegrationExecutor) DisconnectNeutral(
	xboxone.ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func newRetainedXboxDeviceIntegrationDevice(
	t *testing.T,
	authorityID uint64,
	deviceID uint64,
) *xboxone.AuthorizedDormantRetainedUSBDevice {
	return newRetainedXboxDeviceIntegrationDeviceWithExecutor(
		t, authorityID, deviceID,
		&retainedXboxDeviceIntegrationExecutor{marker: 1})
}

func newRetainedXboxDeviceIntegrationDeviceWithExecutor(
	t *testing.T,
	authorityID uint64,
	deviceID uint64,
	executor xboxone.ControllerPersonaLocalExecutor,
) *xboxone.AuthorizedDormantRetainedUSBDevice {
	t.Helper()
	profile, err := xboxone.NewUnregisteredControllerProfile(
		xboxone.ControllerIdentity{
			VendorID: 0xf00d, ProductID: 0xbeef, DeviceReleaseBCD: 0x0102,
			DeviceID: 0x0000fffb01020304,
			Firmware: xboxone.FirmwareVersion{
				Major: 1, Minor: 2, Build: 3, Revision: 4,
			},
			HardwareMajor: 5, HardwareMinor: 6,
		},
		xboxone.ControllerUSBConfig{
			MaxPower2mA: 0x32, OUTIntervalMS: 4, INIntervalMS: 8,
		})
	if err != nil {
		t.Fatalf("NewUnregisteredControllerProfile: %v", err)
	}
	metadata, err := profile.BindExternallyCompiledMetadata([]byte{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("BindExternallyCompiledMetadata: %v", err)
	}
	authorization, err := xboxone.NewAuthorizedControllerPersonaConfig(
		xboxone.ControllerPersonaConfig{
			Profile: profile, Metadata: metadata,
			CurrentInput:      xboxone.GamepadInputReportV1{},
			CurrentStatus:     xboxone.NewWiredNoBatteryStatus(false),
			PoweringOffStatus: xboxone.NewWiredNoBatteryStatus(true),
		},
		xboxone.ControllerUSBIdentityStrings{
			Manufacturer: "Authorized Test Vendor",
			Product:      "Authorized Retained Pad",
			Serial:       "0000fffb01020304A1B2C3D4E5F60708",
		},
		xboxone.ControllerIdentityAuthorizationGranted)
	if err != nil {
		t.Fatalf("NewAuthorizedControllerPersonaConfig: %v", err)
	}
	device, err := xboxone.NewAuthorizedDormantRetainedUSBDevice(
		authorization, 100, authorityID, deviceID,
		executor,
		100*time.Millisecond)
	if err != nil {
		t.Fatalf("NewAuthorizedDormantRetainedUSBDevice: %v", err)
	}
	return device
}

func TestAuthorizedXboxDeviceEnumeratesEP0ThroughProductionRetainedImport(
	t *testing.T,
) {
	testAuthorizedXboxProductionRetainedImport(t, false)
}

func TestAuthorizedXboxProductionRetainedImportAcceptsOUTBeforeInitialIN(t *testing.T) {
	testAuthorizedXboxProductionRetainedImport(t, true)
}

func testAuthorizedXboxProductionRetainedImport(t *testing.T, earlyHostCommand bool) {
	t.Helper()
	const authorityID uint64 = 0x9501
	const deviceID uint64 = 0x9502
	device := newRetainedXboxDeviceIntegrationDevice(
		t, authorityID, deviceID)
	bus := virtualbus.New(950)
	defer bus.Close() //nolint:errcheck
	if _, err := bus.Add(device); err != nil {
		t.Fatalf("add Xbox device: %v", err)
	}
	server := New(ServerConfig{
		ConnectionTimeout: time.Second, RetainedImportAuthorityID: authorityID,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := server.AddBus(bus); err != nil {
		t.Fatalf("add bus: %v", err)
	}
	serverConn, clientConn := netPipeWithDeadline(t, 950)
	result := make(chan error, 1)
	go func() { result <- server.handleConn(serverConn) }()

	writeRetainedImportRequest(t, clientConn, "950-1")
	readSuccessfulRetainedImport(t, clientConn)
	setup := [8]byte{0x80, 0x06, 0x00, 0x01, 0x00, 0x00,
		xboxone.USBDeviceDescriptorSize, 0x00}
	writeRetainedSubmit(t, clientConn, 301, usbip.DirIn, 0,
		xboxone.USBDeviceDescriptorSize, setup, nil)
	sequence, status, actual, payload := readRetainedSubmitResponse(t, clientConn)
	if sequence != 301 || status != 0 ||
		actual != xboxone.USBDeviceDescriptorSize ||
		!bytes.Equal(payload, device.GetDescriptor().Bytes()) {
		t.Fatalf("EP0 descriptor response = seq=%d status=%d actual=%d payload=% x",
			sequence, status, actual, payload)
	}

	// A host may advertise a larger EP0 IN window than the descriptor it asks
	// for. The production command reader must retain the full uint16 request
	// while the owner and scheduler cap the actual response independently.
	setup[6] = 0xff
	setup[7] = 0xff
	writeRetainedSubmit(t, clientConn, 302, usbip.DirIn, 0,
		uint32(^uint16(0)), setup, nil)
	sequence, status, actual, payload = readRetainedSubmitResponse(t, clientConn)
	if sequence != 302 || status != 0 ||
		actual != xboxone.USBDeviceDescriptorSize ||
		!bytes.Equal(payload, device.GetDescriptor().Bytes()) {
		t.Fatalf("maximum-window EP0 response = seq=%d status=%d actual=%d payload=% x",
			sequence, status, actual, payload)
	}

	setup = [8]byte{0x00, 0x05, 0x01, 0x00}
	writeRetainedSubmit(t, clientConn, 303, usbip.DirOut, 0, 0, setup, nil)
	sequence, status, actual, payload = readRetainedSubmitResponse(t, clientConn)
	if sequence != 303 || status != 0 || actual != 0 || len(payload) != 0 {
		t.Fatalf("SET_ADDRESS response = seq=%d status=%d actual=%d payload=% x",
			sequence, status, actual, payload)
	}
	setup = [8]byte{0x00, 0x09, 0x01, 0x00}
	writeRetainedSubmit(t, clientConn, 304, usbip.DirOut, 0, 0, setup, nil)
	sequence, status, actual, payload = readRetainedSubmitResponse(t, clientConn)
	if sequence != 304 || status != 0 || actual != 0 || len(payload) != 0 {
		t.Fatalf("SET_CONFIGURATION response = seq=%d status=%d actual=%d payload=% x",
			sequence, status, actual, payload)
	}

	writeRetainedSubmit(t, clientConn, 305, usbip.DirIn, 1, 64, [8]byte{}, nil)
	sequence, status, actual, payload = readRetainedSubmitResponse(t, clientConn)
	if sequence != 305 || status != 0 || actual == 0 {
		t.Fatalf("Hello response = seq=%d status=%d actual=%d payload=% x",
			sequence, status, actual, payload)
	}
	if _, err := xboxone.DecodeHelloMessage(payload); err != nil {
		t.Fatalf("decode Hello: %v (wire=% x)", err, payload)
	}

	probeWire := []byte{
		0x05, 0x20, 0x02, 0x0f,
		0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x55,
		0x53, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	writeRetainedSubmit(t, clientConn, 306, usbip.DirOut, 1,
		uint32(len(probeWire)), [8]byte{}, probeWire)
	sequence, status, actual, payload = readRetainedSubmitResponseForDirection(
		t, clientConn, usbip.DirOut)
	if sequence != 306 || status != 0 || actual != uint32(len(probeWire)) ||
		len(payload) != 0 {
		t.Fatalf("extended initialization response = seq=%d status=%d actual=%d payload=% x",
			sequence, status, actual, payload)
	}

	startWire := []byte{0x05, 0x20, 0x03, 0x01,
		byte(xboxone.SetDeviceStateStart)}
	writeRetainedSubmit(t, clientConn, 307, usbip.DirOut, 1,
		uint32(len(startWire)), [8]byte{}, startWire)
	sequence, status, actual, payload = readRetainedSubmitResponseForDirection(
		t, clientConn, usbip.DirOut)
	if sequence != 307 || status != 0 || actual != uint32(len(startWire)) ||
		len(payload) != 0 {
		t.Fatalf("START response = seq=%d status=%d actual=%d payload=% x",
			sequence, status, actual, payload)
	}

	writeRetainedSubmit(t, clientConn, 308, usbip.DirIn, 1, 64, [8]byte{}, nil)
	sequence, status, actual, payload = readRetainedSubmitResponse(t, clientConn)
	if sequence != 308 || status != 0 || actual == 0 {
		t.Fatalf("status response = seq=%d status=%d actual=%d payload=% x",
			sequence, status, actual, payload)
	}
	if _, _, err := xboxone.DecodeExtendedStatusNoEventsMessage(payload); err != nil {
		t.Fatalf("decode status: %v (wire=% x)", err, payload)
	}

	securityCompleteWire := []byte{0x06, 0x20, 0x01, 0x02, 0x01, 0x00}
	if earlyHostCommand {
		// This exact documented no-op command was already covered after
		// initial IN below. Move its host URB earlier to exercise the real
		// command reader, retained lanes, readiness wake and serializer.
		writeRetainedSubmit(t, clientConn, 310, usbip.DirOut, 1,
			uint32(len(securityCompleteWire)), [8]byte{}, securityCompleteWire)
	}
	wantInput := xboxone.InputStateV1{
		A: true, Y: true, LeftTrigger: 777, RightTrigger: 888,
		LeftStickX: -1234, RightStickY: 2345,
	}
	var semanticWire [xboxone.SemanticInputWireSize]byte
	if err := xboxone.EncodeSemanticInputWireV1Into(
		semanticWire[:], wantInput); err != nil {
		t.Fatalf("encode semantic input: %v", err)
	}
	if err := device.PublishSemanticInputWire(2, semanticWire[:]); err != nil {
		t.Fatalf("publish semantic input: %v", err)
	}
	writeRetainedSubmit(t, clientConn, 309, usbip.DirIn, 1, 64, [8]byte{}, nil)
	sequence, status, actual, payload = readRetainedSubmitResponse(t, clientConn)
	if sequence != 309 || status != 0 || actual == 0 {
		t.Fatalf("initial input response = seq=%d status=%d actual=%d payload=% x",
			sequence, status, actual, payload)
	}
	_, input, err := xboxone.DecodeGamepadInputMessage(payload)
	if err != nil || input.State != wantInput {
		t.Fatalf("decode initial input = (%+v, %v), want %+v (wire=% x)",
			input, err, wantInput, payload)
	}

	if !earlyHostCommand {
		writeRetainedSubmit(t, clientConn, 310, usbip.DirOut, 1,
			uint32(len(securityCompleteWire)), [8]byte{}, securityCompleteWire)
	}
	sequence, status, actual, payload = readRetainedSubmitResponseForDirection(
		t, clientConn, usbip.DirOut)
	if sequence != 310 || status != 0 ||
		actual != uint32(len(securityCompleteWire)) || len(payload) != 0 {
		t.Fatalf("security complete response = seq=%d status=%d actual=%d payload=% x",
			sequence, status, actual, payload)
	}
	// Ordinary input is change-driven: security completion does not invent a
	// duplicate controller reading. Publish an actual successor state to prove
	// this otherwise-no-op host command leaves normal input usable.
	wantInput.A = false
	if err := xboxone.EncodeSemanticInputWireV1Into(semanticWire[:], wantInput); err != nil {
		t.Fatal(err)
	}
	if err := device.PublishSemanticInputWire(3, semanticWire[:]); err != nil {
		t.Fatal(err)
	}
	writeRetainedSubmit(t, clientConn, 311, usbip.DirIn, 1, 64, [8]byte{}, nil)
	sequence, status, actual, payload = readRetainedSubmitResponse(t, clientConn)
	if sequence != 311 || status != 0 || actual == 0 {
		t.Fatalf("post-security input response = seq=%d status=%d actual=%d payload=% x",
			sequence, status, actual, payload)
	}
	if _, got, err := xboxone.DecodeGamepadInputMessage(payload); err != nil || got.State != wantInput {
		t.Fatalf("decode post-security input: %v (wire=% x)", err, payload)
	}

	if err := clientConn.Close(); err != nil {
		t.Fatalf("close retained client: %v", err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("peer disconnect unexpectedly returned nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Xbox retained import did not drain")
	}
	if devices := bus.Devices(); len(devices) != 0 {
		t.Fatalf("safely disconnected one-shot Xbox device remained advertised: %d",
			len(devices))
	}
}

func TestAuthorizedXboxActiveImportExplicitRemovalIsSafeAndIdempotent(
	t *testing.T,
) {
	tests := []struct {
		name   string
		busID  uint32
		remove func(*Server, uint32) error
	}{
		{
			name: "device", busID: 951,
			remove: func(server *Server, busID uint32) error {
				return server.RemoveDeviceByID(busID, "1")
			},
		},
		{
			name: "bus", busID: 952,
			remove: func(server *Server, busID uint32) error {
				return server.RemoveBus(busID)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authorityID := uint64(test.busID)<<32 | 1
			deviceID := uint64(test.busID)<<32 | 2
			device := newRetainedXboxDeviceIntegrationDevice(
				t, authorityID, deviceID)
			bus := virtualbus.New(test.busID)
			t.Cleanup(func() { _ = bus.Close() })
			_, err := bus.Add(device)
			if err != nil {
				t.Fatalf("add Xbox device: %v", err)
			}
			server := New(ServerConfig{
				ConnectionTimeout:         time.Second,
				BusCleanupTimeout:         time.Hour,
				RetainedImportAuthorityID: authorityID,
			}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			if err := server.AddBus(bus); err != nil {
				t.Fatalf("add bus: %v", err)
			}
			serverConn, clientConn := netPipeWithDeadline(t, test.busID)
			result := make(chan error, 1)
			go func() { result <- server.handleConn(serverConn) }()
			writeRetainedImportRequest(
				t, clientConn, testBusID(test.busID, 1))
			readSuccessfulRetainedImport(t, clientConn)

			if err := test.remove(server, test.busID); err != nil {
				t.Fatalf("explicit %s removal: %v", test.name, err)
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("safe explicit removal became teardown failure: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("explicit removal did not close retained import")
			}
			if devices := bus.Devices(); len(devices) != 0 {
				t.Fatalf("explicit removal left %d device(s)", len(devices))
			}
			server.retainedFailureMu.Lock()
			if len(server.retainedDeviceAdmissions) != 0 ||
				len(server.retainedOwnerAdmissions) != 1 {
				server.retainedFailureMu.Unlock()
				t.Fatal("explicit removal lost device cleanup or owner proof")
			}
			server.retainedFailureMu.Unlock()
			server.retainedImports.mu.Lock()
			activeImports := len(server.retainedImports.active)
			server.retainedImports.mu.Unlock()
			if activeImports != 0 {
				t.Fatalf("safe explicit removal retained %d authority session(s)",
					activeImports)
			}
			if err := server.Close(); err != nil {
				t.Fatalf("close server: %v", err)
			}
			server.retainedFailureMu.Lock()
			if server.retainedOwnerAdmissions != nil {
				server.retainedFailureMu.Unlock()
				t.Fatal("server close retained owner proof")
			}
			server.retainedFailureMu.Unlock()
		})
	}
}

func TestAuthorizedXboxOldOneShotRetirementPreservesSamePointerSuccessor(
	t *testing.T,
) {
	tests := []struct {
		name  string
		busID uint32
		move  func(*Server, *virtualbus.VirtualBus,
			*xboxone.AuthorizedDormantRetainedUSBDevice) *virtualbus.VirtualBus
	}{
		{
			name: "device re-add", busID: 957,
			move: func(server *Server, bus *virtualbus.VirtualBus,
				device *xboxone.AuthorizedDormantRetainedUSBDevice,
			) *virtualbus.VirtualBus {
				if err := server.RemoveDeviceByID(957, "1"); err != nil {
					t.Fatalf("remove device: %v", err)
				}
				return bus
			},
		},
		{
			name: "replacement bus", busID: 958,
			move: func(server *Server, bus *virtualbus.VirtualBus,
				device *xboxone.AuthorizedDormantRetainedUSBDevice,
			) *virtualbus.VirtualBus {
				if err := server.RemoveBus(958); err != nil {
					t.Fatalf("remove bus: %v", err)
				}
				replacement := virtualbus.New(958)
				if err := server.AddBus(replacement); err != nil {
					t.Fatalf("add replacement bus: %v", err)
				}
				return replacement
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authorityID := uint64(test.busID)<<32 | 1
			cancelEntered := make(chan struct{})
			cancelRelease := make(chan struct{})
			executor := &retainedXboxDeviceIntegrationExecutor{
				marker: 1, cancelEntered: cancelEntered,
				cancelRelease: cancelRelease,
			}
			device := newRetainedXboxDeviceIntegrationDeviceWithExecutor(
				t, authorityID, uint64(test.busID)<<32|2, executor)
			bus := virtualbus.New(test.busID)
			t.Cleanup(func() { _ = bus.Close() })
			_, err := bus.Add(device)
			if err != nil {
				t.Fatalf("add device: %v", err)
			}
			server := New(ServerConfig{
				ConnectionTimeout: time.Second, BusCleanupTimeout: time.Hour,
				RetainedImportAuthorityID: authorityID,
			}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			if err := server.AddBus(bus); err != nil {
				t.Fatalf("add bus: %v", err)
			}
			serverConn, clientConn := netPipeWithDeadline(t, test.busID)
			result := make(chan error, 1)
			go func() { result <- server.handleConn(serverConn) }()
			writeRetainedImportRequest(
				t, clientConn, testBusID(test.busID, 1))
			readSuccessfulRetainedImport(t, clientConn)

			successorBus := test.move(server, bus, device)
			if successorBus != bus {
				t.Cleanup(func() { _ = successorBus.Close() })
			}
			select {
			case <-cancelEntered:
			case <-time.After(time.Second):
				t.Fatal("old import did not enter local drain")
			}
			_, err = successorBus.Add(device)
			if err != nil {
				t.Fatalf("re-add same device pointer: %v", err)
			}
			current, _, found := successorBus.GetDeviceImportSnapshot(1)
			if !found {
				t.Fatal("successor registration missing")
			}
			close(cancelRelease)
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("old import teardown: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("old import teardown did not finish")
			}
			if !successorBus.AuthenticatesRegistration(current) {
				t.Fatal("old one-shot retirement removed successor registration")
			}
		})
	}
}

func TestAuthorizedXboxRealTCPShutdownAndRemovalDrainOneShot(t *testing.T) {
	tests := []struct {
		name   string
		busID  uint32
		remove func(*Server, uint32) error
	}{
		{"server close", 959, func(server *Server, _ uint32) error {
			return server.Close()
		}},
		{"device removal", 960, func(server *Server, busID uint32) error {
			return server.RemoveDeviceByID(busID, "1")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authorityID := uint64(test.busID)<<32 | 1
			device := newRetainedXboxDeviceIntegrationDevice(
				t, authorityID, uint64(test.busID)<<32|2)
			bus := virtualbus.New(test.busID)
			t.Cleanup(func() { _ = bus.Close() })
			_, err := bus.Add(device)
			if err != nil {
				t.Fatalf("add device: %v", err)
			}
			server := New(ServerConfig{
				Addr: "127.0.0.1:0", ConnectionTimeout: time.Second,
				BusCleanupTimeout:         time.Hour,
				RetainedImportAuthorityID: authorityID,
			}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			if err := server.AddBus(bus); err != nil {
				t.Fatalf("add bus: %v", err)
			}
			serveResult := make(chan error, 1)
			go func() { serveResult <- server.ListenAndServe() }()
			select {
			case <-server.Ready():
			case <-time.After(time.Second):
				t.Fatal("TCP server did not become ready")
			}
			rawClient, err := net.DialTimeout(
				"tcp", server.Addr(), time.Second)
			if err != nil {
				t.Fatalf("dial TCP server: %v", err)
			}
			client := &retainedTestClientConnection{
				Conn: rawClient, wireDeviceID: test.busID<<16 | 1,
			}
			t.Cleanup(func() { _ = client.Close() })
			if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatalf("set TCP deadline: %v", err)
			}
			writeRetainedImportRequest(
				t, client, testBusID(test.busID, 1))
			readSuccessfulRetainedImport(t, client)
			if err := test.remove(server, test.busID); err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			var closed [1]byte
			_, readErr := client.Read(closed[:])
			if readErr == nil {
				t.Fatal("real TCP retained connection remained open")
			}
			if devices := bus.Devices(); len(devices) != 0 {
				t.Fatalf("real TCP teardown left %d one-shot device(s)",
					len(devices))
			}
			if err := server.Close(); err != nil {
				t.Fatalf("close TCP server: %v", err)
			}
			select {
			case err := <-serveResult:
				if err != nil {
					t.Fatalf("ListenAndServe: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("TCP listener did not join")
			}
		})
	}
}

func testBusID(busID, deviceID uint32) string {
	return fmt.Sprintf("%d-%d", busID, deviceID)
}

func netPipeWithDeadline(
	t *testing.T,
	busID uint32,
) (serverConn, clientConn net.Conn) {
	t.Helper()
	serverConn, rawClientConn := net.Pipe()
	clientConn = &retainedTestClientConnection{
		Conn: rawClientConn, wireDeviceID: busID<<16 | 1,
	}
	if err := clientConn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set client deadline: %v", err)
	}
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})
	return serverConn, clientConn
}

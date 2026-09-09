package usb

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
	"github.com/Alia5/VIIPER/device/xboxone"
	rootusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/require"
)

// Real production personas and retained USB/IP streams, with independent
// synthetic broker/USB peers. No Windows driver or physical controller is used.
// Every primary GIP identity is distinct, and differs from its import lease ID.
type productionMultiXboxPad struct {
	server       *Server
	registration virtualbus.DeviceMeta
	alias        string
	importID     uint64
	gipID        uint64
	broker       net.Conn
	usb          net.Conn
	usbDone      chan error
	sequence     uint32
	revision     uint64
	imported     bool
}

func newProductionMultiXboxPad(t *testing.T, server *Server, authority, importID uint64, busID uint32) *productionMultiXboxPad {
	t.Helper()
	gipID := uint64(0x0000fffb00000000) | uint64(busID)
	preparation, err := xboxone.PrepareProductionRetainedUSBDevice(xboxone.ProductionRetainedUSBDeviceOptions{
		Identity: xboxone.ControllerIdentity{
			VendorID: 0xf00d, ProductID: 0xbeef, DeviceReleaseBCD: 0x0102,
			DeviceID: gipID,
			Firmware: xboxone.FirmwareVersion{Major: 1},
		},
		USB: xboxone.ControllerUSBConfig{MaxPower2mA: 50, OUTIntervalMS: 4, INIntervalMS: 4},
		Strings: xboxone.ControllerUSBIdentityStrings{
			Manufacturer: "Synthetic test", Product: "Independent Xbox pad",
			Serial: fmt.Sprintf("%016x%016x", gipID, busID),
		},
		IdentityAuthorization: xboxone.ControllerIdentityAuthorizationGranted,
		FeedbackBinding: xboxone.ControllerPersonaFeedbackBindingV1{
			Source: controllerfeedback.SourceXboxOneVirtualDevice, PersonaGeneration: 1,
			DeviceGeneration: uint64(busID), TransportGeneration: 1,
			OwnershipEpoch: uint64(busID), TimeToLiveMicroseconds: 250_000,
		},
		ProtocolTimeMS: 10, AuthorityID: authority, ImportDeviceID: importID, LocalTimeout: time.Second,
	})
	require.NoError(t, err)
	authorization, now, authorityID, selectedImportID, executor, timeout, valid := preparation.AuthorizedConstructionInputs()
	require.True(t, valid)
	require.Equal(t, importID, selectedImportID)
	device, err := xboxone.NewAuthorizedDormantRetainedUSBDevice(authorization, now, authorityID, selectedImportID, executor, timeout)
	require.NoError(t, err)
	require.NoError(t, preparation.AttachConstructed(device))
	bus := virtualbus.New(busID)
	require.NoError(t, server.AddBus(bus))
	registration, err := server.AddProductionXboxOneRetainedDeviceRegistration(busID, authority, device)
	require.NoError(t, err)
	alias, err := usbip.ExportBusID(registration.Meta)
	require.NoError(t, err)
	brokerServer, brokerClient := net.Pipe()
	require.NoError(t, brokerClient.SetDeadline(time.Now().Add(10*time.Second)))
	brokerDone := make(chan error, 1)
	var usbDevice rootusb.Device = device
	go func() {
		brokerDone <- xboxone.ProductionStreamHandler(retainedXboxRetirementAuthenticatedConn{brokerServer}, &usbDevice, nil)
	}()
	t.Cleanup(func() {
		_ = brokerClient.Close()
		_ = brokerServer.Close()
		select {
		case <-brokerDone:
		case <-time.After(2 * time.Second):
			t.Error("synthetic Xbox broker did not join")
		}
	})
	writeRetirementBrokerFrame(t, brokerClient, 0x01, 0, nil)
	kind, _, _ := readRetirementBrokerFrame(t, brokerClient)
	require.Equal(t, byte(0x81), kind)
	return &productionMultiXboxPad{server: server, registration: registration,
		alias: alias, importID: importID, gipID: gipID, broker: brokerClient, revision: 1}
}

func (pad *productionMultiXboxPad) submit(t *testing.T, direction, endpoint, length uint32, setup [8]byte, payload []byte) {
	t.Helper()
	pad.sequence++
	writeRetainedSubmit(t, pad.usb, pad.sequence, direction, endpoint, length, setup, payload)
}

func (pad *productionMultiXboxPad) importAndStart(t *testing.T) {
	t.Helper()
	usbServer, usbClient := netPipeWithDeadline(t, pad.registration.Meta.BusID)
	require.NoError(t, usbClient.SetDeadline(time.Now().Add(10*time.Second)))
	pad.usb, pad.usbDone = usbClient, make(chan error, 1)
	go func() { pad.usbDone <- pad.server.handleConn(usbServer) }()
	writeRetainedImportRequest(t, usbClient, pad.alias)
	readSuccessfulRetainedImport(t, usbClient)
	pad.imported = true
	pad.submit(t, usbip.DirOut, 0, 0, [8]byte{0, 9, 1}, nil)
	_, status, _, _ := readRetainedSubmitResponseForDirection(t, usbClient, usbip.DirOut)
	require.Zero(t, status)
	pad.submit(t, usbip.DirIn, 1, 64, [8]byte{}, nil)
	_, status, _, hello := readRetainedSubmitResponse(t, usbClient)
	require.Zero(t, status)
	decodedHello, err := xboxone.DecodeHelloMessage(hello)
	require.NoError(t, err)
	require.Equal(t, pad.gipID, decodedHello.DeviceID)
	start := []byte{0x05, 0x20, 0x02, 0x01, byte(xboxone.SetDeviceStateStart)}
	pad.submit(t, usbip.DirOut, 1, uint32(len(start)), [8]byte{}, start)
	_, status, _, _ = readRetainedSubmitResponseForDirection(t, usbClient, usbip.DirOut)
	require.Zero(t, status)
	for range 2 {
		pad.submit(t, usbip.DirIn, 1, 64, [8]byte{}, nil)
		_, status, _, payload := readRetainedSubmitResponse(t, usbClient)
		require.Zero(t, status)
		require.NotEmpty(t, payload)
	}
}

func (pad *productionMultiXboxPad) verifyInput(t *testing.T, state xboxone.InputStateV1) {
	t.Helper()
	pad.submit(t, usbip.DirIn, 1, 64, [8]byte{}, nil)
	var wire [xboxone.SemanticInputWireSize]byte
	require.NoError(t, xboxone.EncodeSemanticInputWireV1Into(wire[:], state))
	pad.revision++
	writeRetirementBrokerFrame(t, pad.broker, 0x02, pad.revision, wire[:])
	kind, correlation, payload := readRetirementBrokerFrame(t, pad.broker)
	require.Equal(t, byte(0x82), kind)
	require.Equal(t, pad.revision, correlation)
	require.Equal(t, []byte{1}, payload)
	_, status, _, payload := readRetainedSubmitResponse(t, pad.usb)
	require.Zero(t, status)
	// The production factory advertises the Share-capable Console Function Map,
	// not the smaller unregistered test persona's base-input report.
	_, input, err := xboxone.DecodeConsoleFunctionMapGamepadInputMessage(payload)
	require.NoError(t, err)
	require.Equal(t, state, input.State)
}

func (pad *productionMultiXboxPad) verifyFeedback(t *testing.T) {
	t.Helper()
	var wire [xboxone.DirectMotorMessageSize]byte
	require.NoError(t, xboxone.EncodeDirectMotorMessageInto(wire[:], 0x31,
		xboxone.RumbleBodyV1{Enabled: xboxone.MotorAll, LeftVibration: 20,
			RightVibration: 40, LeftImpulse: 60, RightImpulse: 80, Duration: 10}))
	pad.submit(t, usbip.DirOut, 1, uint32(len(wire)), [8]byte{}, wire[:])
	// USB/IP OUT completion publishes the retained local-work ticket. A
	// synchronous net.Pipe host must drain that completion before awaiting CFBK.
	_, status, actual, _ := readRetainedSubmitResponseForDirection(t, pad.usb, usbip.DirOut)
	require.Zero(t, status)
	require.Equal(t, uint32(len(wire)), actual)
	kind, correlation, payload := readRetirementBrokerFrame(t, pad.broker)
	require.Equal(t, byte(0x83), kind)
	var feedback controllerfeedback.Frame
	require.NoError(t, feedback.UnmarshalFrom(payload))
	require.False(t, feedback.IsStop())
	require.Equal(t, uint64(pad.registration.Meta.BusID), feedback.DeviceGeneration)
	require.Equal(t, uint64(pad.registration.Meta.BusID), feedback.OwnershipEpoch)
	require.NotZero(t, feedback.BodyLow)
	require.NotZero(t, feedback.BodyHigh)
	require.NotZero(t, feedback.LeftTrigger)
	require.NotZero(t, feedback.RightTrigger)
	writeRetirementBrokerFrame(t, pad.broker, 0x03, correlation, []byte{1})
}

func (pad *productionMultiXboxPad) remove(t *testing.T) {
	t.Helper()
	type result struct {
		removed bool
		err     error
	}
	closed := make(chan result, 1)
	go func() {
		removed, err := pad.server.CloseAndRemoveRetainedDeviceRegistrationIfPresent(pad.registration)
		closed <- result{removed, err}
	}()
	if pad.imported {
		kind, correlation, payload := readRetirementBrokerFrame(t, pad.broker)
		require.Equal(t, byte(0x83), kind)
		var stop controllerfeedback.Frame
		require.NoError(t, stop.UnmarshalFrom(payload))
		require.True(t, stop.IsStop())
		writeRetirementBrokerFrame(t, pad.broker, 0x03, correlation, []byte{1})
	}
	select {
	case result := <-closed:
		require.NoError(t, result.err)
		require.True(t, result.removed)
	case <-time.After(4 * time.Second):
		t.Fatal("exact Xbox registration removal did not complete")
	}
	if pad.imported {
		select {
		case err := <-pad.usbDone:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("removed Xbox USB stream did not join")
		}
	}
}

func TestProductionXboxIndependentImportIDsKeepBothPadsLiveAndIsolateExactRemoval(t *testing.T) {
	requireWindowsCanonicalFeedbackClock(t)
	const authority = uint64(0x9781)
	server := New(ServerConfig{ConnectionTimeout: time.Second, BusCleanupTimeout: time.Hour,
		RetainedImportAuthorityID: authority}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	t.Cleanup(func() { _ = server.Close() })
	first := newProductionMultiXboxPad(t, server, authority, 0x978101, 978)
	second := newProductionMultiXboxPad(t, server, authority, 0x978102, 979)
	require.NotEqual(t, first.alias, second.alias)
	require.NotEqual(t, first.gipID, second.gipID)
	first.importAndStart(t)
	second.importAndStart(t)
	server.retainedImports.mu.Lock()
	activeImports := len(server.retainedImports.active)
	server.retainedImports.mu.Unlock()
	require.Equal(t, 2, activeImports)
	first.verifyInput(t, xboxone.InputStateV1{A: true, Share: true, LeftStickX: 1234})
	second.verifyInput(t, xboxone.InputStateV1{B: true, RightStickY: -2345})
	first.verifyFeedback(t)
	second.verifyFeedback(t)
	second.remove(t)
	server.retainedImports.mu.Lock()
	activeImports = len(server.retainedImports.active)
	firstRetained := server.retainedImports.active[first.importID] != nil
	server.retainedImports.mu.Unlock()
	require.Equal(t, 1, activeImports)
	require.True(t, firstRetained)
	first.verifyInput(t, xboxone.InputStateV1{X: true, LeftTrigger: 345})
	first.verifyFeedback(t)
	first.remove(t)
}

func TestProductionXboxDuplicateImportIDRejectionAndRemovalCannotRetireFirstPad(t *testing.T) {
	requireWindowsCanonicalFeedbackClock(t)
	const authority, duplicateImportID = uint64(0x9791), uint64(0x979101)
	server := New(ServerConfig{ConnectionTimeout: time.Second, BusCleanupTimeout: time.Hour,
		RetainedImportAuthorityID: authority}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	t.Cleanup(func() { _ = server.Close() })
	first := newProductionMultiXboxPad(t, server, authority, duplicateImportID, 980)
	second := newProductionMultiXboxPad(t, server, authority, duplicateImportID, 981)
	first.importAndStart(t)
	first.verifyInput(t, xboxone.InputStateV1{A: true})
	usbServer, usbClient := netPipeWithDeadline(t, second.registration.Meta.BusID)
	done := make(chan error, 1)
	go func() { done <- server.handleConn(usbServer) }()
	writeRetainedImportRequest(t, usbClient, second.alias)
	var reply [8]byte
	_, err := io.ReadFull(usbClient, reply[:])
	require.NoError(t, err)
	require.Equal(t, uint16(usbip.OpRepImport), binary.BigEndian.Uint16(reply[2:4]))
	require.NotZero(t, binary.BigEndian.Uint32(reply[4:8]))
	select {
	case err := <-done:
		require.ErrorIs(t, err, errRetainedImportBusy)
	case <-time.After(2 * time.Second):
		t.Fatal("duplicate Xbox import did not reject")
	}
	second.remove(t)
	require.True(t, first.registration.Bus.AuthenticatesRegistration(first.registration))
	require.False(t, second.registration.Bus.AuthenticatesRegistration(second.registration))
	first.verifyInput(t, xboxone.InputStateV1{Y: true, RightTrigger: 456})
	first.verifyFeedback(t)
	first.remove(t)
}

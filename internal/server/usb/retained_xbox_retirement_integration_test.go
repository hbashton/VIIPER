package usb

import (
	"bytes"
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

type retainedXboxRetirementAuthenticatedConn struct{ net.Conn }

func (retainedXboxRetirementAuthenticatedConn) VIIPERAuthenticated() bool { return true }

// X1BR v1 frames exercise the actual public production handler. Test-only
// framing is intentionally independent of Xbox package-private codec helpers.
func writeRetirementBrokerFrame(t *testing.T, conn net.Conn, kind byte, correlation uint64, payload []byte) {
	t.Helper()
	frame := make([]byte, 16+len(payload))
	copy(frame, "X1BR")
	frame[4], frame[5] = 1, kind
	binary.LittleEndian.PutUint16(frame[6:8], uint16(len(payload)))
	binary.LittleEndian.PutUint64(frame[8:16], correlation)
	copy(frame[16:], payload)
	_, err := io.Copy(conn, bytes.NewReader(frame))
	require.NoError(t, err)
}

func readRetirementBrokerFrame(t *testing.T, conn net.Conn) (byte, uint64, []byte) {
	t.Helper()
	var header [16]byte
	_, err := io.ReadFull(conn, header[:])
	require.NoError(t, err)
	require.Equal(t, "X1BR", string(header[:4]))
	require.Equal(t, byte(1), header[4])
	payload := make([]byte, int(binary.LittleEndian.Uint16(header[6:8])))
	_, err = io.ReadFull(conn, payload)
	require.NoError(t, err)
	return header[5], binary.LittleEndian.Uint64(header[8:16]), payload
}

func newProductionRetirementIntegrationDevice(t *testing.T, authority, deviceID uint64) *xboxone.AuthorizedDormantRetainedUSBDevice {
	t.Helper()
	return newProductionRetirementIntegrationIdentityDevice(t, authority, deviceID, deviceID,
		fmt.Sprintf("%016XA1B2C3D4E5F60708", deviceID))
}

func newProductionRetirementIntegrationIdentityDevice(t *testing.T, authority, deviceID, importID uint64, serial string) *xboxone.AuthorizedDormantRetainedUSBDevice {
	t.Helper()
	preparation, err := xboxone.PrepareProductionRetainedUSBDevice(xboxone.ProductionRetainedUSBDeviceOptions{
		Identity: xboxone.ControllerIdentity{
			VendorID: 0xf00d, ProductID: 0xbeef, DeviceReleaseBCD: 0x0102, DeviceID: deviceID,
			Firmware: xboxone.FirmwareVersion{Major: 1, Minor: 2, Build: 3, Revision: 4}, HardwareMajor: 5, HardwareMinor: 6,
		},
		USB:                   xboxone.ControllerUSBConfig{MaxPower2mA: 0x32, OUTIntervalMS: 4, INIntervalMS: 4},
		Strings:               xboxone.ControllerUSBIdentityStrings{Manufacturer: "Test Vendor", Product: "Retirement Test Pad", Serial: serial},
		IdentityAuthorization: xboxone.ControllerIdentityAuthorizationGranted,
		FeedbackBinding:       xboxone.ControllerPersonaFeedbackBindingV1{Source: controllerfeedback.SourceXboxOneVirtualDevice, PersonaGeneration: 1, DeviceGeneration: 2, TransportGeneration: 3, OwnershipEpoch: 4, TimeToLiveMicroseconds: 250_000},
		ProtocolTimeMS:        10, AuthorityID: authority, ImportDeviceID: importID, LocalTimeout: time.Second,
	})
	require.NoError(t, err)
	authorization, now, authorityID, importID, executor, timeout, ok := preparation.AuthorizedConstructionInputs()
	require.True(t, ok)
	device, err := xboxone.NewAuthorizedDormantRetainedUSBDevice(authorization, now, authorityID, importID, executor, timeout)
	require.NoError(t, err)
	require.NoError(t, preparation.AttachConstructed(device))
	return device
}

func TestProductionXboxInputRetirementRemovesOneShotOnlyAfterBrokerStopAck(t *testing.T) {
	const authorityID, deviceID = uint64(0x9571), uint64(0x0000fffb01020304)
	device := newProductionRetirementIntegrationDevice(t, authorityID, deviceID)
	brokerServer, brokerClient := net.Pipe()
	require.NoError(t, brokerClient.SetDeadline(time.Now().Add(5*time.Second)))
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
		case <-time.After(time.Second):
			t.Error("broker did not join")
		}
	})
	writeRetirementBrokerFrame(t, brokerClient, 0x01, 0, nil)
	kind, correlation, payload := readRetirementBrokerFrame(t, brokerClient)
	require.Equal(t, byte(0x81), kind)
	require.Zero(t, correlation)
	require.Empty(t, payload)

	bus := virtualbus.New(957)
	t.Cleanup(func() { _ = bus.Close() })
	_, err := bus.Add(device)
	require.NoError(t, err)
	server := New(ServerConfig{ConnectionTimeout: time.Second, BusCleanupTimeout: time.Hour, RetainedImportAuthorityID: authorityID}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	usbServer, usbClient := netPipeWithDeadline(t, 957)
	usbDone := make(chan error, 1)
	go func() { usbDone <- server.handleConn(usbServer) }()
	writeRetainedImportRequest(t, usbClient, "957-1")
	readSuccessfulRetainedImport(t, usbClient)
	writeRetainedSubmit(t, usbClient, 1, usbip.DirOut, 0, 0, [8]byte{0, 9, 1}, nil)
	_, status, _, _ := readRetainedSubmitResponse(t, usbClient)
	require.Zero(t, status)
	writeRetainedSubmit(t, usbClient, 2, usbip.DirIn, 1, 64, [8]byte{}, nil)
	_, status, _, payload = readRetainedSubmitResponse(t, usbClient)
	require.Zero(t, status)
	_, err = xboxone.DecodeHelloMessage(payload)
	require.NoError(t, err)
	start := []byte{0x05, 0x20, 0x02, 0x01, byte(xboxone.SetDeviceStateStart)}
	writeRetainedSubmit(t, usbClient, 3, usbip.DirOut, 1, uint32(len(start)), [8]byte{}, start)
	_, status, _, _ = readRetainedSubmitResponseForDirection(t, usbClient, usbip.DirOut)
	require.Zero(t, status)
	for _, sequence := range []uint32{4, 5} {
		writeRetainedSubmit(t, usbClient, sequence, usbip.DirIn, 1, 64, [8]byte{}, nil)
		_, status, _, payload = readRetainedSubmitResponse(t, usbClient)
		require.Zero(t, status)
		require.NotEmpty(t, payload)
	}
	// The next IN drives START's local Permit, then consumes one changed
	// reading. No host submission remains queued during the later overflow.
	writeRetainedSubmit(t, usbClient, 6, usbip.DirIn, 1, 64, [8]byte{}, nil)
	var semantic [xboxone.SemanticInputWireSize]byte
	require.NoError(t, xboxone.EncodeSemanticInputWireV1Into(semantic[:], xboxone.InputStateV1{A: true}))
	writeRetirementBrokerFrame(t, brokerClient, 0x02, 2, semantic[:])
	kind, correlation, payload = readRetirementBrokerFrame(t, brokerClient)
	require.Equal(t, byte(0x82), kind)
	require.Equal(t, uint64(2), correlation)
	require.Equal(t, []byte{1}, payload)
	_, status, _, payload = readRetainedSubmitResponse(t, usbClient)
	require.Zero(t, status)
	require.NotEmpty(t, payload)

	var stopCorrelation uint64
	var stopFrame controllerfeedback.Frame
	rejected := false
	for revision := uint64(3); revision < 200 && !rejected; revision++ {
		require.NoError(t, xboxone.EncodeSemanticInputWireV1Into(semantic[:], xboxone.InputStateV1{A: revision%2 == 0}))
		writeRetirementBrokerFrame(t, brokerClient, 0x02, revision, semantic[:])
		for {
			kind, correlation, payload = readRetirementBrokerFrame(t, brokerClient)
			if kind == 0x83 {
				require.Zero(t, stopCorrelation)
				stopCorrelation = correlation
				require.NoError(t, stopFrame.UnmarshalFrom(payload))
				require.True(t, stopFrame.IsStop())
				continue // Stop and rejected input ACK may legally interleave.
			}
			require.Equal(t, byte(0x82), kind)
			require.Equal(t, revision, correlation)
			require.Len(t, payload, 1)
			rejected = payload[0] == 0
			break
		}
	}
	require.True(t, rejected)
	if stopCorrelation == 0 {
		kind, stopCorrelation, payload = readRetirementBrokerFrame(t, brokerClient)
		require.Equal(t, byte(0x83), kind)
		require.NoError(t, stopFrame.UnmarshalFrom(payload))
		require.True(t, stopFrame.IsStop())
	}
	require.Len(t, bus.Devices(), 1, "registration must remain until exact Stop is acknowledged")
	server.retainedImports.mu.Lock()
	session := server.retainedImports.active[deviceID]
	server.retainedImports.mu.Unlock()
	require.NotNil(t, session)
	require.False(t, session.released.Load())
	select {
	case err := <-usbDone:
		t.Fatalf("USB import ended before Stop ACK: %v", err)
	default:
	}
	writeRetirementBrokerFrame(t, brokerClient, 0x03, stopCorrelation, []byte{1})
	select {
	case err := <-usbDone:
		require.ErrorContains(t, err, "input presentation history overflow")
	case <-time.After(2 * time.Second):
		t.Fatal("USB import did not retire after exact Stop ACK")
	}
	require.Empty(t, bus.Devices())
	require.True(t, session.released.Load())
	require.False(t, session.quarantined.Load())
}

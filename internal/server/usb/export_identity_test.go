package usb

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
	"github.com/Alia5/VIIPER/device/xboxone"
	rootusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/require"
)

func TestProductionRemovalBudgetIsDerivedRoundedAndBounded(t *testing.T) {
	for _, test := range []struct {
		timeout      time.Duration
		milliseconds uint32
		valid        bool
	}{
		{0, 15000, true}, {30 * time.Second, 90000, true}, {time.Nanosecond, 1, true},
		{time.Millisecond + time.Nanosecond, 4, true}, {100 * time.Second, 300000, true},
		{100*time.Second + time.Nanosecond, 0, false}, {-time.Second, 0, false}, {time.Duration(1<<63 - 1), 0, false},
	} {
		t.Run(test.timeout.String(), func(t *testing.T) {
			server := New(ServerConfig{ConnectionTimeout: test.timeout}, slog.Default(), nil)
			actual, err := server.ProductionRemovalTimeoutMilliseconds()
			if test.valid {
				require.NoError(t, err)
				require.Equal(t, test.milliseconds, actual)
			} else {
				require.Error(t, err)
				require.Zero(t, actual)
			}
		})
	}
}

func TestProductionAliasCollisionIncludesUnpublishedRegistrations(t *testing.T) {
	const authority = uint64(0x9621)
	server := New(ServerConfig{RetainedImportAuthorityID: authority, BusCleanupTimeout: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	first, second := virtualbus.New(962), virtualbus.New(963)
	t.Cleanup(func() { _ = server.RemoveBus(first.BusID()); _ = server.RemoveBus(second.BusID()); _ = server.Close() })
	require.NoError(t, server.AddBus(first))
	require.NoError(t, server.AddBus(second))
	alias, err := usbip.NewProductionXboxOneBusID()
	require.NoError(t, err)
	_, err = first.AddProvisionalRegistrationWithXboxOneBusIDThrough(newProductionRetirementIntegrationDevice(t, authority, 0x0000fffb01020304), 0xffff, alias)
	require.NoError(t, err)
	_, err = server.addRetainedDeviceRegistration(second.BusID(), authority, newProductionRetirementIntegrationDevice(t, authority, 0x0000fffb01020304), alias)
	require.ErrorContains(t, err, "duplicate USB/IP export alias")
	require.Empty(t, second.Devices())
	_, err = server.lookupUSBIPImportRegistration(alias)
	require.Error(t, err, "unpublished collision guard must not expose first device")
}

func TestInitialEmptyBusCleanupUsesExactConfiguredIncarnation(t *testing.T) {
	server := New(ServerConfig{BusCleanupTimeout: 5 * time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	bus := virtualbus.New(964)
	require.NoError(t, server.AddBus(bus))
	t.Cleanup(func() {
		if server.GetBus(bus.BusID()) != nil {
			_ = server.RemoveBus(bus.BusID())
		}
		_ = server.Close()
	})
	require.Eventually(t, func() bool { return server.GetBus(bus.BusID()) == nil }, time.Second, time.Millisecond)
	require.True(t, bus.IsClosed())
	successor, err := virtualbus.NewWithBusID(bus.BusID())
	require.NoError(t, err)
	_, err = successor.Add(&schedulerTestDevice{desc: testCompositeDescriptor()})
	require.NoError(t, err)
	require.NoError(t, server.AddBus(successor))
	staleEmpty := bus.GetBusEmptyContext()
	require.Nil(t, staleEmpty)
	require.Same(t, successor, server.GetBus(bus.BusID()))
}

func TestProductionAliasCannotSelectAddressBusOrServerSuccessor(t *testing.T) {
	const authority = uint64(0x9601)
	const busID = uint32(960)
	newServer := func() *Server {
		return New(ServerConfig{RetainedImportAuthorityID: authority, BusCleanupTimeout: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	}
	server := newServer()
	t.Cleanup(func() {
		if server.GetBus(busID) != nil {
			_ = server.RemoveBus(busID)
		}
		_ = server.Close()
	})
	var oldAliases []string
	var previous virtualbus.DeviceMeta
	for round := range 4 {
		if round == 2 || round == 3 {
			require.NoError(t, server.RemoveBus(busID))
		}
		if round == 3 {
			require.NoError(t, server.Close())
			server = newServer()
		}
		if server.GetBus(busID) == nil {
			bus, err := virtualbus.NewWithBusID(busID)
			require.NoError(t, err)
			require.NoError(t, server.AddBus(bus))
		}
		device := newProductionRetirementIntegrationDevice(t, authority, 0x0000fffb01020304)
		registration, err := server.AddProductionXboxOneRetainedDeviceRegistration(busID, authority, device)
		require.NoError(t, err)
		require.Equal(t, uint32(1), registration.Meta.DevID)
		alias, err := usbip.ExportBusID(registration.Meta)
		require.NoError(t, err)
		require.True(t, usbip.ValidProductionXboxOneBusID(alias))
		for _, stale := range oldAliases {
			require.NotEqual(t, stale, alias)
			_, err := server.lookupUSBIPImportRegistration(stale)
			require.Error(t, err)
			result := performTestImport(t, server, stale)
			require.NotZero(t, result.status)
			require.Error(t, result.err)
		}
		_, err = server.lookupUSBIPImportRegistration(fmt.Sprintf("%d-1", busID))
		require.Error(t, err)
		numeric := performTestImport(t, server, fmt.Sprintf("%d-1", busID))
		require.NotZero(t, numeric.status)
		require.Error(t, numeric.err)
		actual, err := server.lookupUSBIPImportRegistration(alias)
		require.NoError(t, err)
		require.Equal(t, registration.RegistrationToken, actual.RegistrationToken)
		require.Same(t, device, actual.Dev)
		for _, malformed := range []string{strings.ToUpper(alias), alias + "a", alias[:len(alias)-1] + "b"} {
			_, err := server.lookupUSBIPImportRegistration(malformed)
			require.Error(t, err)
		}
		if round > 0 {
			removed, err := server.RemoveDeviceRegistrationIfPresent(previous)
			require.NoError(t, err)
			require.False(t, removed)
		}
		oldAliases = append(oldAliases, alias)
		previous = registration
		if round == 0 {
			removed, err := server.RemoveDeviceRegistrationIfPresent(registration)
			require.NoError(t, err)
			require.True(t, removed)
		}
	}
}

func TestProductionAliasDevlistImportAndNumericURBIdentityAgree(t *testing.T) {
	const authority, deviceID = uint64(0x9611), uint64(0x0000fffb01020304)
	device := newProductionRetirementIntegrationDevice(t, authority, deviceID)
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
	kind, _, _ := readRetirementBrokerFrame(t, brokerClient)
	require.Equal(t, byte(0x81), kind)
	bus := virtualbus.New(961)
	server := New(ServerConfig{ConnectionTimeout: time.Second, RetainedImportAuthorityID: authority, BusCleanupTimeout: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, server.AddBus(bus))
	t.Cleanup(func() { _ = server.RemoveBus(bus.BusID()); _ = server.Close() })
	registration, err := server.AddProductionXboxOneRetainedDeviceRegistration(bus.BusID(), authority, device)
	require.NoError(t, err)
	alias, err := usbip.ExportBusID(registration.Meta)
	require.NoError(t, err)
	listServer, listClient := net.Pipe()
	require.NoError(t, listClient.SetDeadline(time.Now().Add(time.Second)))
	listDone := make(chan error, 1)
	go func() { listDone <- server.handleDevList(listServer) }()
	var devlist [12 + 312 + 4]byte
	_, err = io.ReadFull(listClient, devlist[:])
	require.NoError(t, err)
	require.NoError(t, <-listDone)
	_ = listClient.Close()
	_ = listServer.Close()
	require.Equal(t, uint32(1), binary.BigEndian.Uint32(devlist[8:12]))
	require.Equal(t, registration.Meta.USBBusID[:], devlist[12+256:12+288])
	require.Equal(t, bus.BusID(), binary.BigEndian.Uint32(devlist[12+288:12+292]))
	require.Equal(t, uint32(1), binary.BigEndian.Uint32(devlist[12+292:12+296]))
	usbServer, usbClient := netPipeWithDeadline(t, bus.BusID())
	usbDone := make(chan error, 1)
	go func() { usbDone <- server.handleConn(usbServer) }()
	writeRetainedImportRequest(t, usbClient, alias)
	var imported [8 + 312]byte
	_, err = io.ReadFull(usbClient, imported[:])
	require.NoError(t, err)
	require.Zero(t, binary.BigEndian.Uint32(imported[4:8]))
	require.Equal(t, registration.Meta.USBBusID[:], imported[8+256:8+288])
	require.Equal(t, devlist[12+288:12+296], imported[8+288:8+296])
	writeRetainedSubmit(t, usbClient, 1, usbip.DirOut, 0, 0, [8]byte{0, 9, 1}, nil)
	_, status, _, _ := readRetainedSubmitResponse(t, usbClient)
	require.Zero(t, status)
	writeRetainedSubmit(t, usbClient, 2, usbip.DirIn, 1, 64, [8]byte{}, nil)
	_, status, _, hello := readRetainedSubmitResponse(t, usbClient)
	require.Zero(t, status)
	_, err = xboxone.DecodeHelloMessage(hello)
	require.NoError(t, err)
	type result struct {
		removed bool
		err     error
	}
	closed := make(chan result, 1)
	go func() {
		removed, err := server.CloseAndRemoveRetainedDeviceRegistrationIfPresent(registration)
		closed <- result{removed, err}
	}()
	kind, correlation, payload := readRetirementBrokerFrame(t, brokerClient)
	require.Equal(t, byte(0x83), kind)
	var stop controllerfeedback.Frame
	require.NoError(t, stop.UnmarshalFrom(payload))
	require.True(t, stop.IsStop())
	writeRetirementBrokerFrame(t, brokerClient, 0x03, correlation, []byte{1})
	select {
	case result := <-closed:
		require.NoError(t, result.err)
		require.True(t, result.removed)
	case <-time.After(2 * time.Second):
		t.Fatal("alias import did not close safely")
	}
	select {
	case <-usbDone:
	case <-time.After(time.Second):
		t.Fatal("alias USB stream did not join")
	}
	_, err = server.lookupUSBIPImportRegistration(alias)
	require.Error(t, err)
}

package registry

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Alia5/VIIPER/controllerfeedback"
	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/internal/log"
	"github.com/Alia5/VIIPER/internal/server/api"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	rootusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/virtualbus"
)

const (
	registryTestAuthorityID = uint64(0xa701)
	registryTestDeviceID    = uint64(0x0000fffb01020304)
	registryTestTimeout     = 50 * time.Millisecond
)

type registryTestXboxExecutor struct{ marker byte }

type registryCapacityDevice struct{ marker byte }

func (*registryCapacityDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	return nil
}

func (*registryCapacityDevice) GetDescriptor() *rootusb.Descriptor { return nil }

func (*registryCapacityDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func (*registryTestXboxExecutor) Execute(
	xboxone.ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func (*registryTestXboxExecutor) ResetAndDrain(time.Time) error { return nil }

func (*registryTestXboxExecutor) ResetNeutral(
	xboxone.ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func (*registryTestXboxExecutor) CancelAndDrain(time.Time) error { return nil }

func (*registryTestXboxExecutor) DisconnectNeutral(
	xboxone.ControllerPersonaLocalExecution,
	time.Time,
) error {
	return nil
}

func registryTestXboxAuthorization(
	t *testing.T,
) xboxone.AuthorizedControllerPersonaConfig {
	t.Helper()
	profile, err := xboxone.NewUnregisteredControllerProfile(
		xboxone.ControllerIdentity{
			VendorID: 0xf00d, ProductID: 0xbeef,
			DeviceReleaseBCD: 0x0102, DeviceID: registryTestDeviceID,
			Firmware: xboxone.FirmwareVersion{
				Major: 1, Minor: 2, Build: 3, Revision: 4,
			},
			HardwareMajor: 5, HardwareMinor: 6,
		},
		xboxone.ControllerUSBConfig{
			MaxPower2mA: 0x32, OUTIntervalMS: 4, INIntervalMS: 8,
		},
	)
	require.NoError(t, err)
	metadata, err := profile.BindExternallyCompiledMetadata([]byte{1, 2, 3, 4})
	require.NoError(t, err)
	authorization, err := xboxone.NewAuthorizedControllerPersonaConfig(
		xboxone.ControllerPersonaConfig{
			Profile: profile, Metadata: metadata,
			CurrentInput:      xboxone.GamepadInputReportV1{},
			CurrentStatus:     xboxone.NewWiredNoBatteryStatus(false),
			PoweringOffStatus: xboxone.NewWiredNoBatteryStatus(true),
		},
		xboxone.ControllerUSBIdentityStrings{
			Manufacturer: "VIIPER test",
			Product:      "Authorized retained Xbox test",
			Serial:       "0000fffb01020304a1b2c3d4e5f60708",
		},
		xboxone.ControllerIdentityAuthorizationGranted,
	)
	require.NoError(t, err)
	return authorization
}

func registryTestXboxRequest(
	t *testing.T,
	busID uint32,
	authorityID uint64,
) AuthorizedXboxOneRetainedUSBRequest {
	t.Helper()
	return AuthorizedXboxOneRetainedUSBRequest{
		BusID: busID, AuthorityID: authorityID,
		DeviceID: registryTestDeviceID, ProtocolTimeMilliseconds: 91,
		Authorization: registryTestXboxAuthorization(t),
		LocalExecutor: &registryTestXboxExecutor{marker: 1},
		LocalTimeout:  registryTestTimeout,
	}
}

func registryTestProductionXboxRequest(
	busID uint32,
	authorityID uint64,
) ProductionXboxOneRetainedUSBRequest {
	return ProductionXboxOneRetainedUSBRequest{
		BusID: busID,
		Options: xboxone.ProductionRetainedUSBDeviceOptions{
			Identity: xboxone.ControllerIdentity{
				VendorID: 0xf00d, ProductID: 0xbeef,
				DeviceReleaseBCD: 0x0102, DeviceID: registryTestDeviceID,
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
				Product:      "Production retained Xbox test",
				Serial:       "0000fffb01020304a1b2c3d4e5f60708",
			},
			IdentityAuthorization: xboxone.ControllerIdentityAuthorizationGranted,
			FeedbackBinding: xboxone.ControllerPersonaFeedbackBindingV1{
				Source:            controllerfeedback.SourceXboxOneVirtualDevice,
				PersonaGeneration: 1, DeviceGeneration: 2,
				TransportGeneration: 3, OwnershipEpoch: 4,
				TimeToLiveMicroseconds: 250_000,
			},
			ProtocolTimeMS: 91, AuthorityID: authorityID,
			ImportDeviceID: registryTestDeviceID,
			LocalTimeout:   registryTestTimeout,
		},
	}
}

func registryTestUSBServer(
	t *testing.T,
	busID uint32,
	authorityID uint64,
) (*serverusb.Server, *virtualbus.VirtualBus) {
	return registryTestUSBServerWithLimit(t, busID, authorityID, 0)
}

func registryTestUSBServerWithLimit(
	t *testing.T,
	busID uint32,
	authorityID uint64,
	deviceAddressLimit uint32,
) (*serverusb.Server, *virtualbus.VirtualBus) {
	t.Helper()
	server := serverusb.New(serverusb.ServerConfig{
		Addr: "127.0.0.1:0", RetainedImportAuthorityID: authorityID,
		RetainedImportDeviceAddressLimit: deviceAddressLimit,
		BusCleanupTimeout:                time.Hour,
	}, slog.Default(), log.NewRaw(nil))
	bus, err := virtualbus.NewWithBusID(busID)
	require.NoError(t, err)
	require.NoError(t, server.AddBus(bus))
	t.Cleanup(func() {
		if server.GetBus(busID) == bus {
			require.NoError(t, server.RemoveBus(busID))
		}
		require.NoError(t, server.Close())
	})
	return server, bus
}

func TestAuthorizedXboxOneFactoryCapacityPreflightDoesNotConsumeAuthorization(
	t *testing.T,
) {
	const busID = uint32(62006)
	server, bus := registryTestUSBServerWithLimit(
		t, busID, registryTestAuthorityID, 1)
	_, err := bus.Add(&registryCapacityDevice{marker: 1})
	require.NoError(t, err)
	request := registryTestXboxRequest(t, busID, registryTestAuthorityID)

	registration, err := RegisterAuthorizedXboxOneRetainedUSB(server, request)
	require.Nil(t, registration)
	require.ErrorIs(t, err, ErrAuthorizedXboxOneRegistration)
	require.ErrorContains(t, err, "capacity is unavailable")

	device, err := xboxone.NewAuthorizedDormantRetainedUSBDevice(
		request.Authorization,
		request.ProtocolTimeMilliseconds,
		request.AuthorityID,
		request.DeviceID,
		request.LocalExecutor,
		request.LocalTimeout,
	)
	require.NoError(t, err)
	require.NotNil(t, device)
}

func TestAuthorizedXboxOneFactoryPreflightDoesNotConsumeAuthorization(
	t *testing.T,
) {
	const busID = uint32(62001)
	server, _ := registryTestUSBServer(t, busID, 0)
	request := registryTestXboxRequest(t, busID, registryTestAuthorityID)

	registration, err := RegisterAuthorizedXboxOneRetainedUSB(server, request)
	require.Nil(t, registration)
	require.ErrorIs(t, err, ErrAuthorizedXboxOneRegistration)

	// The default-off server rejection happened before consuming the exact
	// authorization. The canonical constructor can still consume it once.
	device, err := xboxone.NewAuthorizedDormantRetainedUSBDevice(
		request.Authorization,
		request.ProtocolTimeMilliseconds,
		request.AuthorityID,
		request.DeviceID,
		request.LocalExecutor,
		request.LocalTimeout,
	)
	require.NoError(t, err)
	require.NotNil(t, device)
}

func TestAuthorizedXboxOneFactoryRegistersOneExactDefaultHiddenDevice(
	t *testing.T,
) {
	const busID = uint32(62002)
	server, bus := registryTestUSBServer(t, busID, registryTestAuthorityID)
	request := registryTestXboxRequest(t, busID, registryTestAuthorityID)

	registration, err := RegisterAuthorizedXboxOneRetainedUSB(server, request)
	require.NoError(t, err)
	require.NotNil(t, registration)
	require.Equal(t, busID, registration.BusID())
	require.Equal(t, uint32(1), registration.DeviceAddress())
	require.Len(t, bus.GetAllDeviceMetas(), 1)
	for _, name := range []string{"xboxone", "xbox-one", "xboxseries", "xbox-series"} {
		require.Nil(t, api.GetRegistration(name),
			"explicit factory must not expose generic API product %q", name)
	}

	// Until an exact USB/IP import binds the retained generation, the sole
	// broker ingress rejects input rather than buffering it in another stack.
	err = registration.PublishSemanticInputWire(
		1, make([]byte, xboxone.SemanticInputWireSize))
	require.ErrorIs(t, err, ErrAuthorizedXboxOneRegistration)

	require.NoError(t, registration.Close())
	require.NoError(t, registration.Close())
	require.Empty(t, bus.GetAllDeviceMetas())
	require.ErrorIs(t,
		registration.PublishSemanticInputWire(
			2, make([]byte, xboxone.SemanticInputWireSize)),
		ErrAuthorizedXboxOneRegistrationInactive)
}

func TestProductionXboxOneFactoryRemainsGenericHiddenAndStreamRoutable(
	t *testing.T,
) {
	const busID = uint32(62008)
	server, bus := registryTestUSBServer(t, busID, registryTestAuthorityID)
	registration, err := RegisterProductionXboxOneRetainedUSB(
		server, registryTestProductionXboxRequest(busID, registryTestAuthorityID))
	require.NoError(t, err)
	require.NotNil(t, registration)
	require.Len(t, bus.GetAllDeviceMetas(), 1)
	require.Nil(t, api.GetRegistration("xboxone"),
		"production stream routing must not expose generic construction")
	api.RegisterStreamHandler("xboxone", xboxone.ProductionStreamHandler)
	require.NotNil(t, api.GetStreamHandler("xboxone"))
	meta, active := registration.DeviceMeta()
	require.True(t, active)
	require.Equal(t, "xboxone", meta.Dev.(interface {
		VIIPERDeviceType() string
	}).VIIPERDeviceType())
}

func TestAuthorizedXboxOneFactoryCloseCannotRemoveSamePointerSuccessor(
	t *testing.T,
) {
	const busID = uint32(62003)
	server, bus := registryTestUSBServer(t, busID, registryTestAuthorityID)
	request := registryTestXboxRequest(t, busID, registryTestAuthorityID)
	registration, err := RegisterAuthorizedXboxOneRetainedUSB(server, request)
	require.NoError(t, err)

	metas := bus.GetAllDeviceMetas()
	require.Len(t, metas, 1)
	stale := metas[0]
	removed, err := server.RemoveDeviceRegistrationIfPresent(stale)
	require.NoError(t, err)
	require.True(t, removed)

	successorContext, err := bus.Add(stale.Dev)
	require.NoError(t, err)
	successor, ok := bus.GetDeviceRegistration(stale.Dev, successorContext)
	require.True(t, ok)
	require.NotEqual(t, stale.RegistrationToken, successor.RegistrationToken)

	require.NoError(t, registration.Close())
	require.True(t, bus.AuthenticatesRegistration(successor))
	require.Len(t, bus.GetAllDeviceMetas(), 1)
	require.ErrorIs(t,
		registration.PublishSemanticInputWire(
			2, make([]byte, xboxone.SemanticInputWireSize)),
		ErrAuthorizedXboxOneRegistrationInactive)
}

func TestAuthorizedXboxOnePublishLeaseFencesRemoveAndSamePointerReAdd(
	t *testing.T,
) {
	const busID = uint32(62005)
	server, bus := registryTestUSBServer(t, busID, registryTestAuthorityID)
	request := registryTestXboxRequest(t, busID, registryTestAuthorityID)
	registration, err := RegisterAuthorizedXboxOneRetainedUSB(server, request)
	require.NoError(t, err)
	metas := bus.GetAllDeviceMetas()
	require.Len(t, metas, 1)
	exact := metas[0]

	entered := make(chan struct{})
	release := make(chan struct{})
	registration.state.beforeSemanticPublish = func() {
		close(entered)
		<-release
	}
	publishResult := make(chan error, 1)
	go func() {
		publishResult <- registration.PublishSemanticInputWire(
			1, make([]byte, xboxone.SemanticInputWireSize))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("semantic publish did not acquire its registration lease")
	}

	removeResult := make(chan struct {
		removed bool
		err     error
	}, 1)
	go func() {
		removed, err := server.RemoveDeviceRegistrationIfPresent(exact)
		removeResult <- struct {
			removed bool
			err     error
		}{removed: removed, err: err}
	}()
	select {
	case result := <-removeResult:
		t.Fatalf("removal crossed active publish lease: %+v", result)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	require.ErrorIs(t, <-publishResult, ErrAuthorizedXboxOneRegistration)
	removed := <-removeResult
	require.NoError(t, removed.err)
	require.True(t, removed.removed)
	successor, err := bus.AddRegistration(exact.Dev)
	require.NoError(t, err)
	require.NotEqual(t, exact.RegistrationToken, successor.RegistrationToken)
	require.True(t, bus.AuthenticatesRegistration(successor))
	require.NoError(t, registration.Close())
	require.True(t, bus.AuthenticatesRegistration(successor))
}

func TestAuthorizedXboxOneCloseJoinsConcurrentMarkedBusDrain(t *testing.T) {
	const busID = uint32(62007)
	server, bus := registryTestUSBServer(t, busID, registryTestAuthorityID)
	request := registryTestXboxRequest(t, busID, registryTestAuthorityID)
	registration, err := RegisterAuthorizedXboxOneRetainedUSB(server, request)
	require.NoError(t, err)
	copiedRegistration := *registration
	metas := bus.GetAllDeviceMetas()
	require.Len(t, metas, 1)
	exact := metas[0]
	lease, active := bus.AcquireRegistrationOperation(exact)
	require.True(t, active)

	busRemoval := make(chan error, 1)
	go func() { busRemoval <- server.RemoveBus(busID) }()
	require.Eventually(t, func() bool {
		return bus.IsClosed() && !bus.AuthenticatesRegistration(exact)
	}, time.Second, time.Millisecond)
	handleClose := make(chan error, 1)
	go func() { handleClose <- registration.Close() }()
	repeatedHandleClose := make(chan error, 1)
	go func() { repeatedHandleClose <- copiedRegistration.Close() }()
	select {
	case closeErr := <-handleClose:
		t.Fatalf("handle Close returned before marked bus drain: %v", closeErr)
	case closeErr := <-repeatedHandleClose:
		t.Fatalf("repeated handle Close did not join first Close: %v", closeErr)
	case <-time.After(20 * time.Millisecond):
	}
	require.NoError(t, exact.Context.Err())

	lease.Release()
	require.NoError(t, <-busRemoval)
	require.NoError(t, <-handleClose)
	require.NoError(t, <-repeatedHandleClose)
	require.ErrorIs(t, exact.Context.Err(), context.Canceled)
}

func TestAuthorizedXboxOneFactoryRejectsAuthorityMismatchBeforeConsumption(
	t *testing.T,
) {
	const busID = uint32(62004)
	server, _ := registryTestUSBServer(t, busID, registryTestAuthorityID)
	request := registryTestXboxRequest(t, busID, registryTestAuthorityID+1)

	registration, err := RegisterAuthorizedXboxOneRetainedUSB(server, request)
	require.Nil(t, registration)
	require.ErrorIs(t, err, ErrAuthorizedXboxOneRegistration)

	_, err = xboxone.NewAuthorizedDormantRetainedUSBDevice(
		request.Authorization,
		request.ProtocolTimeMilliseconds,
		request.AuthorityID,
		request.DeviceID,
		request.LocalExecutor,
		request.LocalTimeout,
	)
	require.NoError(t, err)
	_, err = xboxone.NewAuthorizedDormantRetainedUSBDevice(
		request.Authorization,
		request.ProtocolTimeMilliseconds,
		request.AuthorityID,
		request.DeviceID,
		request.LocalExecutor,
		request.LocalTimeout,
	)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrAuthorizedXboxOneRegistration))
}

func TestAuthorizedXboxOneFactoryRejectsUnrepresentableBusBeforeConsumption(
	t *testing.T,
) {
	const busID = uint32(70000)
	server, _ := registryTestUSBServer(t, busID, registryTestAuthorityID)
	request := registryTestXboxRequest(t, busID, registryTestAuthorityID)

	registration, err := RegisterAuthorizedXboxOneRetainedUSB(server, request)
	require.Nil(t, registration)
	require.ErrorIs(t, err, ErrAuthorizedXboxOneRegistration)

	device, err := xboxone.NewAuthorizedDormantRetainedUSBDevice(
		request.Authorization,
		request.ProtocolTimeMilliseconds,
		request.AuthorityID,
		request.DeviceID,
		request.LocalExecutor,
		request.LocalTimeout,
	)
	require.NoError(t, err)
	require.NotNil(t, device)
}

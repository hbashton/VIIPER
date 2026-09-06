package api

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/internal/registry"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/require"
)

func TestXboxStartupOwnershipClaimDoesNotHoldRegistrationReadFence(t *testing.T) {
	const busID, authorityID, deviceID = 63301, 9001, 9002
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := serverusb.New(serverusb.ServerConfig{Addr: "127.0.0.1:0", RetainedImportAuthorityID: authorityID,
		BusCleanupTimeout: time.Hour}, logger, nil)
	bus, err := virtualbus.NewWithBusID(busID)
	require.NoError(t, err)
	require.NoError(t, server.AddBus(bus))
	t.Cleanup(func() {
		if server.GetBus(busID) == bus {
			_ = server.RemoveBus(busID)
		}
		_ = server.Close()
	})
	registration, err := registry.RegisterProductionXboxOneRetainedUSB(server,
		registry.ProductionXboxOneRetainedUSBRequest{BusID: busID, Options: xboxone.ProductionRetainedUSBDeviceOptions{
			Identity: xboxone.ControllerIdentity{VendorID: 0xf00d, ProductID: 0xbeef, DeviceReleaseBCD: 0x0102,
				DeviceID: 0x0000fffb01020304, Firmware: xboxone.FirmwareVersion{Major: 1, Minor: 2, Build: 3, Revision: 4},
				HardwareMajor: 5, HardwareMinor: 6},
			USB: xboxone.ControllerUSBConfig{MaxPower2mA: 0x32, INIntervalMS: 4, OUTIntervalMS: 4},
			Strings: xboxone.ControllerUSBIdentityStrings{Manufacturer: "VIIPER test", Product: "Admission lock test",
				Serial: "0000fffb01020304a1b2c3d4e5f60708"},
			IdentityAuthorization: xboxone.ControllerIdentityAuthorizationGranted,
			FeedbackBinding: xboxone.ControllerPersonaFeedbackBindingV1{Source: controllerfeedback.SourceXboxOneVirtualDevice,
				PersonaGeneration: 1, DeviceGeneration: 2, TransportGeneration: 3, OwnershipEpoch: 4, TimeToLiveMicroseconds: 250_000},
			ProtocolTimeMS: 10, AuthorityID: authorityID, ImportDeviceID: deviceID, LocalTimeout: time.Second,
		}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = registration.Close() })
	token, ok := registration.ProductionRemovalToken()
	require.True(t, ok)
	admission, err := SelectAuthorizedXboxOneRegistration(server, "63301", "1",
		`{"version":1,"removalToken":"`+token+`"}`)
	require.NoError(t, err)
	removed := make(chan error, 1)
	removedBeforeClaimReturned := false
	lease, err := admission.claimStream(func() *deviceStreamLease {
		// Model the existing stream coordinator holding its mutex while its
		// cleanup callback removes the registration. A registration read lease
		// held around this ownership callback would force the timeout below.
		go func() {
			_, err := server.RemoveDeviceRegistrationIfPresent(admission.Registration())
			removed <- err
		}()
		select {
		case err := <-removed:
			require.NoError(t, err)
			removedBeforeClaimReturned = true
		case <-time.After(time.Second):
		}
		return &deviceStreamLease{}
	})
	require.NoError(t, err)
	require.NotNil(t, lease)
	if !removedBeforeClaimReturned {
		require.NoError(t, <-removed) // release a regressed read fence before cleanup
	}
	require.True(t, removedBeforeClaimReturned, "registration fence inverted stream cleanup lock order")
	require.Error(t, admission.Run(func() error { t.Fatal("removed startup admitted mutation"); return nil }))
}

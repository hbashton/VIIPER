package api

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/internal/registry"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/require"
)

func retryCleanupRegistration(t *testing.T) (*serverusb.Server, virtualbus.DeviceMeta) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := serverusb.New(serverusb.ServerConfig{Addr: "127.0.0.1:0", RetainedImportAuthorityID: 9001,
		BusCleanupTimeout: time.Hour}, logger, nil)
	bus, err := virtualbus.NewWithBusID(63302)
	require.NoError(t, err)
	require.NoError(t, s.AddBus(bus))
	r, err := registry.RegisterProductionXboxOneRetainedUSB(s,
		registry.ProductionXboxOneRetainedUSBRequest{BusID: 63302, Options: xboxone.ProductionRetainedUSBDeviceOptions{
			Identity: xboxone.ControllerIdentity{VendorID: 0xf00d, ProductID: 0xbeef, DeviceReleaseBCD: 0x0102,
				DeviceID: 0x0000fffb01020304, Firmware: xboxone.FirmwareVersion{Major: 1, Minor: 2, Build: 3, Revision: 4}, HardwareMajor: 5, HardwareMinor: 6},
			USB:                   xboxone.ControllerUSBConfig{MaxPower2mA: 0x32, INIntervalMS: 4, OUTIntervalMS: 4},
			Strings:               xboxone.ControllerUSBIdentityStrings{Manufacturer: "VIIPER test", Product: "Retry cleanup test", Serial: "0000fffb01020304a1b2c3d4e5f60708"},
			IdentityAuthorization: xboxone.ControllerIdentityAuthorizationGranted,
			FeedbackBinding: xboxone.ControllerPersonaFeedbackBindingV1{Source: controllerfeedback.SourceXboxOneVirtualDevice,
				PersonaGeneration: 1, DeviceGeneration: 2, TransportGeneration: 3, OwnershipEpoch: 4, TimeToLiveMicroseconds: 250000},
			ProtocolTimeMS: 10, AuthorityID: 9001, ImportDeviceID: 9002, LocalTimeout: time.Second,
		}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close(); _ = s.RemoveBus(63302); _ = s.Close() })
	token, ok := r.ProductionRemovalToken()
	require.True(t, ok)
	admission, err := SelectAuthorizedXboxOneRegistration(s, "63302", "1", `{"version":1,"removalToken":"`+token+`"}`)
	require.NoError(t, err)
	return s, admission.Registration()
}

func TestRetryCleanupRequiresRetirementAndNativeCompletionAndCatchesLateRetry(t *testing.T) {
	s, registration := retryCleanupRegistration(t)
	alias, err := usbip.ExportBusID(registration.Meta)
	require.NoError(t, err)
	c := newXboxOneRetryCleanup(slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.delays = []time.Duration{0}
	calls := make(chan usbip.ExportMeta, 10)
	c.stop = func(ctx context.Context, meta usbip.ExportMeta, port uint16) (int32, error) {
		require.Equal(t, uint16(3241), port)
		require.NoError(t, ctx.Err())
		calls <- meta
		return 0, nil // An empty queue now is NOT proof it cannot be enqueued later.
	}
	finish, err := c.arm(registration, 3241)
	require.NoError(t, err)
	_, err = c.arm(registration, 3241)
	require.Error(t, err)
	c.observe(alias)
	c.observe("x1-not-owned")
	require.Empty(t, calls, "No request may retire an active registration")
	_, err = s.RemoveDeviceRegistrationIfPresent(registration)
	require.NoError(t, err)
	c.observe(alias)
	require.Empty(t, calls, "Cancellation must not be mistaken for native completion")
	finish()
	finish()
	select {
	case got := <-calls:
		require.Equal(t, registration.Meta, got)
	case <-time.After(time.Second):
		t.Fatal("missing retirement sweep")
	}
	require.Eventually(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return !c.entries[alias].running }, time.Second, time.Millisecond)
	// Drain a coalesced concurrent cancellation notification before measuring
	// a brand-new late import callback.
	for len(calls) > 0 {
		<-calls
	}
	c.observe(alias)
	select {
	case got := <-calls:
		require.Equal(t, registration.Meta, got)
	case <-time.After(time.Second):
		t.Fatal("late retry escaped tombstone")
	}
	require.Eventually(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return !c.entries[alias].running }, time.Second, time.Millisecond)
	_, err = c.arm(registration, 3241)
	require.Error(t, err, "A stale retired registration cannot arm a new cleanup owner")
}

func TestRetryCleanupRejectsMissingPortOrRegistration(t *testing.T) {
	c := newXboxOneRetryCleanup(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := c.arm(virtualbus.DeviceMeta{}, 3241)
	require.Error(t, err)
	s, registration := retryCleanupRegistration(t)
	_ = s
	_, err = c.arm(registration, 0)
	require.Error(t, err)
	require.Empty(t, c.entries)
}

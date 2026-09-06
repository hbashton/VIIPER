package usb

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Alia5/VIIPER/internal/log"
	"github.com/Alia5/VIIPER/internal/retainedusb"
	rootusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/virtualbus"
)

type blockingRegistrationDescriptorDevice struct {
	entered chan struct{}
	release chan struct{}
	desc    rootusb.Descriptor
}

func (*blockingRegistrationDescriptorDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	return nil
}

func (device *blockingRegistrationDescriptorDevice) GetDescriptor() *rootusb.Descriptor {
	close(device.entered)
	<-device.release
	return &device.desc
}

func (*blockingRegistrationDescriptorDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func (*blockingRegistrationDescriptorDevice) RetainedUSBImportDeviceID() uint64 {
	return 1
}

func (*blockingRegistrationDescriptorDevice) RetainedUSBImportOwner() retainedusb.ImportOwner {
	return nil
}

type valueRetainedRegistrationDevice struct{ marker byte }

func (valueRetainedRegistrationDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	return nil
}
func (valueRetainedRegistrationDevice) GetDescriptor() *rootusb.Descriptor { return nil }
func (valueRetainedRegistrationDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}
func (valueRetainedRegistrationDevice) RetainedUSBImportDeviceID() uint64 { return 1 }
func (valueRetainedRegistrationDevice) RetainedUSBImportOwner() retainedusb.ImportOwner {
	return nil
}

type zeroSizedRetainedRegistrationDevice struct{}

func (*zeroSizedRetainedRegistrationDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	return nil
}
func (*zeroSizedRetainedRegistrationDevice) GetDescriptor() *rootusb.Descriptor { return nil }
func (*zeroSizedRetainedRegistrationDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}
func (*zeroSizedRetainedRegistrationDevice) RetainedUSBImportDeviceID() uint64 { return 1 }
func (*zeroSizedRetainedRegistrationDevice) RetainedUSBImportOwner() retainedusb.ImportOwner {
	return nil
}

type nonComparableRetainedRegistrationDevice []byte

type repeatableRetainedRegistrationDevice struct {
	desc rootusb.Descriptor
}

func (*repeatableRetainedRegistrationDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	return nil
}
func (device *repeatableRetainedRegistrationDevice) GetDescriptor() *rootusb.Descriptor {
	return &device.desc
}
func (*repeatableRetainedRegistrationDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}
func (*repeatableRetainedRegistrationDevice) RetainedUSBImportDeviceID() uint64 {
	return 1
}
func (*repeatableRetainedRegistrationDevice) RetainedUSBImportOwner() retainedusb.ImportOwner {
	return nil
}

func (nonComparableRetainedRegistrationDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	return nil
}
func (nonComparableRetainedRegistrationDevice) GetDescriptor() *rootusb.Descriptor {
	return nil
}
func (nonComparableRetainedRegistrationDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}
func (nonComparableRetainedRegistrationDevice) RetainedUSBImportDeviceID() uint64 {
	return 1
}
func (nonComparableRetainedRegistrationDevice) RetainedUSBImportOwner() retainedusb.ImportOwner {
	return nil
}

func retainedRegistrationTestDescriptor() rootusb.Descriptor {
	return rootusb.Descriptor{
		Device: rootusb.DeviceDescriptor{
			BcdUSB: 0x0200, BDeviceClass: 0xff, BDeviceSubClass: 0x47,
			BDeviceProtocol: 0xd0, BMaxPacketSize0: 64,
			IDVendor: 0xf00d, IDProduct: 0xbeef,
			BcdDevice: 0x0102, BNumConfigurations: 1, Speed: 2,
		},
		Configuration: rootusb.ConfigurationDescriptor{
			BConfigurationValue: 1, BMAttributes: 0xa0, BMaxPower: 0x32,
		},
		Interfaces: []rootusb.InterfaceConfig{{
			Descriptor: rootusb.InterfaceDescriptor{
				BInterfaceNumber: 0, BAlternateSetting: 0, BNumEndpoints: 2,
				BInterfaceClass: 0xff, BInterfaceSubClass: 0x47,
				BInterfaceProtocol: 0xd0,
			},
			Endpoints: []rootusb.EndpointDescriptor{
				{BEndpointAddress: 0x01, BMAttributes: 0x03,
					WMaxPacketSize: 64, BInterval: 4},
				{BEndpointAddress: 0x81, BMAttributes: 0x03,
					WMaxPacketSize: 64, BInterval: 8},
			},
		}},
	}
}

func retainedRegistrationTestServer(
	t *testing.T,
	busID uint32,
	authorityID uint64,
) (*Server, *virtualbus.VirtualBus) {
	t.Helper()
	server := New(ServerConfig{
		Addr: "127.0.0.1:0", RetainedImportAuthorityID: authorityID,
		ConnectionTimeout: time.Second, BusCleanupTimeout: time.Hour,
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

func TestRetainedRegistrationIsHiddenUntilDescriptorAdmissionCommits(
	t *testing.T,
) {
	const (
		busID       = uint32(63001)
		authorityID = uint64(0x63001)
	)
	server, bus := retainedRegistrationTestServer(t, busID, authorityID)
	device := &blockingRegistrationDescriptorDevice{
		entered: make(chan struct{}), release: make(chan struct{}),
		desc: retainedRegistrationTestDescriptor(),
	}
	type result struct {
		registration virtualbus.DeviceMeta
		err          error
	}
	resultCh := make(chan result, 1)
	go func() {
		registration, err := server.AddRetainedDeviceRegistration(
			busID, authorityID, device)
		resultCh <- result{registration: registration, err: err}
	}()

	select {
	case <-device.entered:
	case <-time.After(time.Second):
		t.Fatal("descriptor admission did not begin")
	}
	require.Empty(t, bus.GetAllDeviceMetas())
	require.Empty(t, bus.Devices())
	_, _, discovered := bus.GetDeviceImportSnapshot(1)
	require.False(t, discovered)
	_, discovered = bus.GetDeviceByID(1)
	require.False(t, discovered)

	close(device.release)
	registered := <-resultCh
	require.NoError(t, registered.err)
	require.True(t, bus.AuthenticatesRegistration(registered.registration))
	require.Len(t, bus.GetAllDeviceMetas(), 1)
	_, _, discovered = bus.GetDeviceImportSnapshot(1)
	require.True(t, discovered)
}

func TestRetainedRegistrationRejectsInvalidDeviceIdentityBeforeBusMutation(
	t *testing.T,
) {
	const (
		busID       = uint32(63002)
		authorityID = uint64(0x63002)
	)
	server, bus := retainedRegistrationTestServer(t, busID, authorityID)
	var typedNil *blockingRegistrationDescriptorDevice
	tests := []struct {
		name   string
		device rootusb.Device
	}{
		{name: "typed nil", device: typedNil},
		{name: "value", device: valueRetainedRegistrationDevice{marker: 1}},
		{name: "zero sized pointer", device: &zeroSizedRetainedRegistrationDevice{}},
		{name: "non comparable", device: nonComparableRetainedRegistrationDevice{1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.NotPanics(t, func() {
				_, err := server.AddRetainedDeviceRegistration(
					busID, authorityID, test.device)
				require.ErrorIs(t, err, errRetainedImportInvalid)
			})
			require.Empty(t, bus.GetAllDeviceMetas())
			require.Empty(t, bus.Devices())
			require.NotNil(t, bus.GetBusEmptyContext(),
				"invalid device mutated hidden registration state")
		})
	}
}

func TestDuplicateRemoveBusJoinsCompleteServerRemoval(t *testing.T) {
	const (
		busID       = uint32(63003)
		authorityID = uint64(0x63003)
	)
	server, bus := retainedRegistrationTestServer(t, busID, authorityID)
	_, err := bus.Add(&valueRetainedRegistrationDevice{marker: 1})
	require.NoError(t, err)
	entered := make(chan struct{})
	release := make(chan struct{})
	server.beforeBusRemovalComplete = func() {
		close(entered)
		<-release
	}
	first := make(chan error, 1)
	go func() { first <- server.RemoveBus(busID) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first RemoveBus did not reach completion fence")
	}
	second := make(chan error, 1)
	go func() { second <- server.RemoveBus(busID) }()
	select {
	case secondErr := <-second:
		t.Fatalf("duplicate RemoveBus escaped server cleanup: %v", secondErr)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-first)
	require.NoError(t, <-second)
}

func TestDirectVirtualBusTeardownRetiresRetainedAdmissionForReAdd(
	t *testing.T,
) {
	t.Run("exact removal", func(t *testing.T) {
		const (
			busID       = uint32(63004)
			authorityID = uint64(0x63004)
		)
		server, bus := retainedRegistrationTestServer(t, busID, authorityID)
		device := &repeatableRetainedRegistrationDevice{
			desc: retainedRegistrationTestDescriptor(),
		}
		first, err := server.AddRetainedDeviceRegistration(
			busID, authorityID, device)
		require.NoError(t, err)
		removed, err := bus.RemoveRegistrationIfPresent(first)
		require.NoError(t, err)
		require.True(t, removed)
		successor, err := server.AddRetainedDeviceRegistration(
			busID, authorityID, device)
		require.NoError(t, err)
		require.NotEqual(t, first.RegistrationToken,
			successor.RegistrationToken)
		server.retainedFailureMu.Lock()
		oldAdmissionPresent := false
		for key := range server.retainedDeviceAdmissions {
			oldAdmissionPresent = oldAdmissionPresent ||
				key.bus == bus && key.registrationToken ==
					first.RegistrationToken
		}
		server.retainedFailureMu.Unlock()
		require.False(t, oldAdmissionPresent)
	})

	t.Run("whole bus close", func(t *testing.T) {
		const (
			firstBusID  = uint32(63005)
			secondBusID = uint32(63006)
			authorityID = uint64(0x63005)
		)
		server, firstBus := retainedRegistrationTestServer(
			t, firstBusID, authorityID)
		device := &repeatableRetainedRegistrationDevice{
			desc: retainedRegistrationTestDescriptor(),
		}
		first, err := server.AddRetainedDeviceRegistration(
			firstBusID, authorityID, device)
		require.NoError(t, err)
		require.NoError(t, firstBus.Close())
		require.NoError(t, server.RemoveBus(firstBusID))
		server.retainedFailureMu.Lock()
		oldAdmissionPresent := false
		for key := range server.retainedDeviceAdmissions {
			oldAdmissionPresent = oldAdmissionPresent ||
				key.bus == firstBus && key.registrationToken ==
					first.RegistrationToken
		}
		server.retainedFailureMu.Unlock()
		require.False(t, oldAdmissionPresent)
		secondBus, err := virtualbus.NewWithBusID(secondBusID)
		require.NoError(t, err)
		require.NoError(t, server.AddBus(secondBus))
		t.Cleanup(func() {
			if server.GetBus(secondBusID) == secondBus {
				require.NoError(t, server.RemoveBus(secondBusID))
			}
		})
		successor, err := server.AddRetainedDeviceRegistration(
			secondBusID, authorityID, device)
		require.NoError(t, err)
		require.NotZero(t, successor.RegistrationToken)
		require.NotSame(t, first.Bus, successor.Bus)
	})
}

func TestRetainedRollbackForgetsAdmissionAfterEarlyWatcherAndServerRemoval(
	t *testing.T,
) {
	const (
		busID       = uint32(63007)
		authorityID = uint64(0x63007)
	)
	server, bus := retainedRegistrationTestServer(t, busID, authorityID)
	device := &blockingRegistrationDescriptorDevice{
		entered: make(chan struct{}), release: make(chan struct{}),
		desc: retainedRegistrationTestDescriptor(),
	}
	beforeAdmission := make(chan struct{})
	releaseAdmission := make(chan struct{})
	watcherForgot := make(chan struct{})
	beforeRollback := make(chan struct{})
	releaseRollback := make(chan struct{})
	server.beforeRegisteredRetainedAdmission = func() {
		close(beforeAdmission)
		<-releaseAdmission
	}
	server.afterRetainedRegistrationWatchForget = func() {
		close(watcherForgot)
	}
	server.beforeRetainedRegistrationRollback = func() {
		close(beforeRollback)
		<-releaseRollback
	}
	type result struct {
		registration virtualbus.DeviceMeta
		err          error
	}
	resultCh := make(chan result, 1)
	go func() {
		registration, err := server.AddRetainedDeviceRegistration(
			busID, authorityID, device)
		resultCh <- result{registration: registration, err: err}
	}()
	select {
	case <-beforeAdmission:
	case <-time.After(time.Second):
		t.Fatal("registered callback did not reach pre-admission boundary")
	}
	require.NoError(t, bus.Close())
	select {
	case <-watcherForgot:
	case <-time.After(time.Second):
		t.Fatal("context watcher did not complete its early no-op retirement")
	}
	close(releaseAdmission)
	select {
	case <-beforeRollback:
	case <-time.After(time.Second):
		t.Fatal("registration did not reach rollback boundary")
	}
	require.NoError(t, server.RemoveBus(busID))
	close(releaseRollback)
	registered := <-resultCh
	require.Error(t, registered.err)
	require.Zero(t, registered.registration.RegistrationToken)

	deviceReference, valid := exactRetainedImportOwnerReference(device)
	require.True(t, valid)
	server.retainedFailureMu.Lock()
	staleAdmissionPresent := false
	for key := range server.retainedDeviceAdmissions {
		staleAdmissionPresent = staleAdmissionPresent ||
			key.device == deviceReference
	}
	server.retainedFailureMu.Unlock()
	require.False(t, staleAdmissionPresent)
	select {
	case <-device.entered:
		t.Fatal("descriptor callback ran after exact bus retirement")
	default:
	}
}

func TestRetainedReAddWaitsForMarkedPredecessorOperationDrain(t *testing.T) {
	const (
		firstBusID  = uint32(63008)
		secondBusID = uint32(63009)
		authorityID = uint64(0x63008)
	)
	server, firstBus := retainedRegistrationTestServer(
		t, firstBusID, authorityID)
	secondBus, err := virtualbus.NewWithBusID(secondBusID)
	require.NoError(t, err)
	require.NoError(t, server.AddBus(secondBus))
	t.Cleanup(func() {
		if server.GetBus(secondBusID) == secondBus {
			require.NoError(t, server.RemoveBus(secondBusID))
		}
	})
	device := &repeatableRetainedRegistrationDevice{
		desc: retainedRegistrationTestDescriptor(),
	}
	first, err := server.AddRetainedDeviceRegistration(
		firstBusID, authorityID, device)
	require.NoError(t, err)
	lease, active := firstBus.AcquireRegistrationOperation(first)
	require.True(t, active)
	closeResult := make(chan error, 1)
	go func() { closeResult <- firstBus.Close() }()
	require.Eventually(t, func() bool {
		return firstBus.IsClosed() &&
			!firstBus.AuthenticatesRegistration(first)
	}, time.Second, time.Millisecond)
	require.NoError(t, first.Context.Err())

	_, err = server.AddRetainedDeviceRegistration(
		secondBusID, authorityID, device)
	require.ErrorIs(t, err, errRetainedImportBusy)
	require.NoError(t, first.Context.Err())

	lease.Release()
	require.NoError(t, <-closeResult)
	require.ErrorIs(t, first.Context.Err(), context.Canceled)
	successor, err := server.AddRetainedDeviceRegistration(
		secondBusID, authorityID, device)
	require.NoError(t, err)
	require.NotSame(t, first.Bus, successor.Bus)
}

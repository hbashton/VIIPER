package usb

import (
	"fmt"
	"sync"
	"testing"
	"time"

	rootusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/virtualbus"
	"github.com/stretchr/testify/require"
)

type gipIdentityFixture struct {
	*repeatableRetainedRegistrationDevice
	id        uint64
	panicRead bool
}

func (device *gipIdentityFixture) RetainedUSBPrimaryGIPDeviceID() uint64 {
	if device.panicRead {
		panic("test identity callback")
	}
	return device.id
}

type blockingGIPIdentityFixture struct {
	*blockingRegistrationDescriptorDevice
	id uint64
}

func (device *blockingGIPIdentityFixture) RetainedUSBPrimaryGIPDeviceID() uint64 { return device.id }

func TestPrimaryGIPIdentityRejectsDifferentSerialAndImportIDBeforeExposure(t *testing.T) {
	const authority, gipID = uint64(0x9871), uint64(0x0000fffb01020304)
	server, firstBus := retainedRegistrationTestServer(t, 987, authority)
	secondBus := virtualbus.New(988)
	require.NoError(t, server.AddBus(secondBus))
	t.Cleanup(func() { _ = server.RemoveBus(secondBus.BusID()) })
	first := newProductionRetirementIntegrationIdentityDevice(t, authority, gipID, 100,
		fmt.Sprintf("%016x%016x", gipID, 100))
	second := newProductionRetirementIntegrationIdentityDevice(t, authority, gipID, 101,
		fmt.Sprintf("%016x%016x", gipID, 101))
	require.Equal(t, gipID, first.RetainedUSBPrimaryGIPDeviceID())
	registration, err := server.AddProductionXboxOneRetainedDeviceRegistration(firstBus.BusID(), authority, first)
	require.NoError(t, err)
	_, err = server.AddProductionXboxOneRetainedDeviceRegistration(secondBus.BusID(), authority, second)
	require.ErrorContains(t, err, "primary GIP identity was already registered")
	require.Empty(t, secondBus.Devices())
	require.True(t, firstBus.AuthenticatesRegistration(registration))
	removed, err := server.RemoveDeviceRegistrationIfPresent(registration)
	require.NoError(t, err)
	require.True(t, removed)
	require.NoError(t, server.RemoveBus(firstBus.BusID()))
	// Native PDO removal is unproven even after the USB/IP bus is gone.
	_, err = server.AddProductionXboxOneRetainedDeviceRegistration(secondBus.BusID(), authority, second)
	require.ErrorContains(t, err, "primary GIP identity was already registered")
	require.Empty(t, secondBus.Devices())
	fresh := newProductionRetirementIntegrationDevice(t, authority, gipID+1)
	_, err = server.AddProductionXboxOneRetainedDeviceRegistration(secondBus.BusID(), authority, fresh)
	require.NoError(t, err)
}

func TestPrimaryGIPIdentityReservationIncludesUnpublishedDescriptors(t *testing.T) {
	const authority, gipID = uint64(0x9891), uint64(0x0000fffb01020304)
	server, bus := retainedRegistrationTestServer(t, 989, authority)
	device := &blockingGIPIdentityFixture{&blockingRegistrationDescriptorDevice{
		entered: make(chan struct{}), release: make(chan struct{}), desc: retainedRegistrationTestDescriptor()}, gipID}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(device.release) }) })
	done := make(chan error, 1)
	go func() {
		_, err := server.AddProductionXboxOneRetainedDeviceRegistration(bus.BusID(), authority, device)
		done <- err
	}()
	select {
	case <-device.entered:
	case <-time.After(time.Second):
		t.Fatal("descriptor admission not entered")
	}
	// This read and duplicate rejection must not wait on the blocked callback.
	require.Empty(t, bus.Devices())
	duplicate := &gipIdentityFixture{repeatableRetainedRegistrationDevice: &repeatableRetainedRegistrationDevice{desc: retainedRegistrationTestDescriptor()}, id: gipID}
	_, err := server.AddRetainedDeviceRegistration(bus.BusID(), authority, duplicate)
	require.ErrorContains(t, err, "primary GIP identity was already registered")
	release.Do(func() { close(device.release) })
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("descriptor admission did not complete")
	}
}

func TestPrimaryGIPIdentityConcurrentAdmissionHasOneWinner(t *testing.T) {
	const authority, gipID = uint64(0x9901), uint64(0x0000fffb01020304)
	server, bus := retainedRegistrationTestServer(t, 990, authority)
	const count = 32
	results := make(chan error, count)
	for range count {
		go func() {
			device := &gipIdentityFixture{repeatableRetainedRegistrationDevice: &repeatableRetainedRegistrationDevice{desc: retainedRegistrationTestDescriptor()}, id: gipID}
			_, err := server.AddProductionXboxOneRetainedDeviceRegistration(bus.BusID(), authority, device)
			results <- err
		}()
	}
	winners := 0
	for range count {
		select {
		case err := <-results:
			if err == nil {
				winners++
			} else {
				require.ErrorContains(t, err, "primary GIP identity was already registered")
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent registration did not complete")
		}
	}
	require.Equal(t, 1, winners)
	require.Len(t, bus.Devices(), 1)
}

func TestPrimaryGIPIdentityInvalidOrMissingCapabilityFailsClosed(t *testing.T) {
	const authority = uint64(0x9911)
	server, bus := retainedRegistrationTestServer(t, 991, authority)
	for _, device := range []rootusb.Device{
		&repeatableRetainedRegistrationDevice{desc: retainedRegistrationTestDescriptor()},
		&gipIdentityFixture{repeatableRetainedRegistrationDevice: &repeatableRetainedRegistrationDevice{}, id: 0},
		&gipIdentityFixture{repeatableRetainedRegistrationDevice: &repeatableRetainedRegistrationDevice{}, id: 1},
		&gipIdentityFixture{repeatableRetainedRegistrationDevice: &repeatableRetainedRegistrationDevice{}, panicRead: true},
	} {
		_, err := server.AddProductionXboxOneRetainedDeviceRegistration(bus.BusID(), authority, device)
		require.ErrorIs(t, err, errRetainedImportInvalid)
		require.Empty(t, bus.Devices())
	}
	require.Empty(t, server.usedPrimaryGIPDeviceIDs)
}

func TestPrimaryGIPIdentityCapacityCannotEvictEarlierReservations(t *testing.T) {
	const authority = uint64(0x9921)
	server, bus := retainedRegistrationTestServer(t, 992, authority)
	server.usedPrimaryGIPDeviceIDs = make(map[uint64]struct{}, maxPrimaryGIPIdentityReservations)
	for i := range maxPrimaryGIPIdentityReservations {
		server.usedPrimaryGIPDeviceIDs[0x0000fffb00000000|uint64(i)] = struct{}{}
	}
	device := &gipIdentityFixture{repeatableRetainedRegistrationDevice: &repeatableRetainedRegistrationDevice{}, id: 0x0000fffbfffffff0}
	_, err := server.AddProductionXboxOneRetainedDeviceRegistration(bus.BusID(), authority, device)
	require.ErrorContains(t, err, "reservation capacity exhausted")
	require.Empty(t, bus.Devices())
	require.Len(t, server.usedPrimaryGIPDeviceIDs, maxPrimaryGIPIdentityReservations)
}

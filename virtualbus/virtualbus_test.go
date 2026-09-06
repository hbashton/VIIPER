package virtualbus

import (
	"context"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/usb"
	"github.com/stretchr/testify/require"
)

type closeFenceTestDevice struct{ id byte }

type nonComparableBusTestDevice []byte

type runtimeNonComparableBusTestDevice struct{ payload any }

func (*closeFenceTestDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	return nil
}

func (*closeFenceTestDevice) GetDescriptor() *usb.Descriptor { return nil }

func (*closeFenceTestDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func (nonComparableBusTestDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	return nil
}

func (nonComparableBusTestDevice) GetDescriptor() *usb.Descriptor { return nil }

func (nonComparableBusTestDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func (runtimeNonComparableBusTestDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	return nil
}

func (runtimeNonComparableBusTestDevice) GetDescriptor() *usb.Descriptor {
	return nil
}

func (runtimeNonComparableBusTestDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func TestCloseAndTakeDevicesFencesAddAndReturnsExactDrain(t *testing.T) {
	bus := New(0xff001)
	first := &closeFenceTestDevice{id: 1}
	second := &closeFenceTestDevice{id: 2}
	firstContext, err := bus.Add(first)
	require.NoError(t, err)
	secondContext, err := bus.Add(second)
	require.NoError(t, err)

	removed, err := bus.CloseAndTakeDevices()
	require.NoError(t, err)
	require.Equal(t, []usb.Device{first, second}, removed)
	require.Empty(t, bus.Devices())
	select {
	case <-firstContext.Done():
	default:
		t.Fatal("first device context survived bus close")
	}
	select {
	case <-secondContext.Done():
	default:
		t.Fatal("second device context survived bus close")
	}
	_, err = bus.Add(&closeFenceTestDevice{id: 3})
	require.ErrorContains(t, err, "is closed")
	removed, err = bus.CloseAndTakeDevices()
	require.NoError(t, err)
	require.Empty(t, removed)
}

func TestRemoveIfPresentCannotRemoveAddressReuseSuccessor(t *testing.T) {
	bus := New(0xff002)
	t.Cleanup(func() { _ = bus.Close() })
	old := &closeFenceTestDevice{id: 1}
	_, err := bus.Add(old)
	require.NoError(t, err)
	require.NoError(t, bus.Remove(old))
	replacement := &closeFenceTestDevice{id: 2}
	_, err = bus.Add(replacement)
	require.NoError(t, err)

	removed, err := bus.RemoveIfPresent(old)
	require.NoError(t, err)
	require.False(t, removed)
	require.Equal(t, []usb.Device{replacement}, bus.Devices())
}

func TestRegistrationTokenCannotRemoveSamePointerReAdd(t *testing.T) {
	bus := New(0xff003)
	t.Cleanup(func() { _ = bus.Close() })
	device := &closeFenceTestDevice{id: 1}
	_, err := bus.Add(device)
	require.NoError(t, err)
	first, _, found := bus.GetDeviceImportSnapshot(1)
	require.True(t, found)
	removed, err := bus.RemoveRegistrationIfPresent(first)
	require.NoError(t, err)
	require.True(t, removed)
	_, err = bus.Add(device)
	require.NoError(t, err)
	second, _, found := bus.GetDeviceImportSnapshot(1)
	require.True(t, found)
	require.NotEqual(t, first.RegistrationToken, second.RegistrationToken)

	removed, err = bus.RemoveRegistrationIfPresent(first)
	require.NoError(t, err)
	require.False(t, removed)
	require.True(t, bus.AuthenticatesRegistration(second))
}

func TestAddRegistrationReturnsExactAuthenticatedIncarnation(t *testing.T) {
	bus, err := NewWithBusID(60006)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	device := &closeFenceTestDevice{id: 1}

	first, err := bus.AddRegistration(device)
	require.NoError(t, err)
	require.NotNil(t, first.Context)
	require.NotZero(t, first.RegistrationToken)
	require.True(t, bus.AuthenticatesRegistration(first))

	removed, err := bus.RemoveRegistrationIfPresent(first)
	require.NoError(t, err)
	require.True(t, removed)
	successor, err := bus.AddRegistration(device)
	require.NoError(t, err)
	require.NotEqual(t, first.RegistrationToken, successor.RegistrationToken)
	require.False(t, bus.AuthenticatesRegistration(first))
	require.True(t, bus.AuthenticatesRegistration(successor))

	removed, err = bus.RemoveRegistrationIfPresent(first)
	require.NoError(t, err)
	require.False(t, removed)
	require.True(t, bus.AuthenticatesRegistration(successor))
}

func TestRegistrationCapabilityCannotBeForgedAcrossBuses(t *testing.T) {
	firstBus, err := NewWithBusID(60010)
	require.NoError(t, err)
	t.Cleanup(func() { _ = firstBus.Close() })
	secondBus, err := NewWithBusID(60011)
	require.NoError(t, err)
	t.Cleanup(func() { _ = secondBus.Close() })
	registration, err := firstBus.AddRegistration(
		&closeFenceTestDevice{id: 1})
	require.NoError(t, err)

	forged := registration
	forged.Bus = secondBus
	require.False(t, secondBus.AuthenticatesRegistration(forged))
	lease, active := secondBus.AcquireRegistrationOperation(forged)
	require.False(t, active)
	lease.Release()
	require.True(t, firstBus.AuthenticatesRegistration(registration))
}

func TestRegistrationOperationLeaseFencesRemovalWithoutAllocation(t *testing.T) {
	bus, err := NewWithBusID(60007)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	device := &closeFenceTestDevice{id: 1}
	registration, err := bus.AddRegistration(device)
	require.NoError(t, err)

	allocations := testing.AllocsPerRun(1000, func() {
		lease, active := bus.AcquireRegistrationOperation(registration)
		if !active {
			panic("exact registration operation was not admitted")
		}
		lease.Release()
	})
	require.Zero(t, allocations)

	lease, active := bus.AcquireRegistrationOperation(registration)
	require.True(t, active)
	removeResult := make(chan struct {
		removed bool
		err     error
	}, 1)
	go func() {
		removed, err := bus.RemoveRegistrationIfPresent(registration)
		removeResult <- struct {
			removed bool
			err     error
		}{removed: removed, err: err}
	}()
	select {
	case result := <-removeResult:
		t.Fatalf("removal crossed registration operation lease: %+v", result)
	case <-time.After(20 * time.Millisecond):
	}
	reentered := make(chan struct{})
	go func() {
		_ = bus.BusID()
		_ = bus.GetAllDeviceMetas()
		close(reentered)
	}()
	select {
	case <-reentered:
	case <-time.After(time.Second):
		t.Fatal("operation-time bus reentry deadlocked behind removal")
	}
	lease.Release()
	result := <-removeResult
	require.NoError(t, result.err)
	require.True(t, result.removed)
	_, active = bus.AcquireRegistrationOperation(registration)
	require.False(t, active)
}

func TestExactRemovalAndConcurrentCloseJoinMarkedDrain(t *testing.T) {
	bus, err := NewWithBusID(60012)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	registration, err := bus.AddRegistration(&closeFenceTestDevice{id: 1})
	require.NoError(t, err)
	lease, active := bus.AcquireRegistrationOperation(registration)
	require.True(t, active)

	firstClose := make(chan []DeviceMeta, 1)
	go func() {
		removed, closeErr := bus.CloseAndTakeRegistrations()
		if closeErr != nil {
			panic(closeErr)
		}
		firstClose <- removed
	}()
	require.Eventually(t, func() bool {
		return bus.IsClosed() && !bus.AuthenticatesRegistration(registration)
	}, time.Second, time.Millisecond)

	joinedRemoval := make(chan bool, 1)
	go func() {
		removed, removeErr := bus.RemoveRegistrationIfPresent(registration)
		if removeErr != nil {
			panic(removeErr)
		}
		joinedRemoval <- removed
	}()
	secondClose := make(chan []DeviceMeta, 1)
	go func() {
		removed, closeErr := bus.CloseAndTakeRegistrations()
		if closeErr != nil {
			panic(closeErr)
		}
		secondClose <- removed
	}()
	select {
	case <-joinedRemoval:
		t.Fatal("exact no-op removal returned before marked drain completed")
	case <-secondClose:
		t.Fatal("concurrent Close returned before marked drain completed")
	case <-time.After(20 * time.Millisecond):
	}
	require.NoError(t, registration.Context.Err())

	lease.Release()
	require.Len(t, <-firstClose, 1)
	require.False(t, <-joinedRemoval)
	require.Empty(t, <-secondClose)
	require.ErrorIs(t, registration.Context.Err(), context.Canceled)
}

func TestBoundedRegistrationCapacityAndTokenPreflight(t *testing.T) {
	bus, err := NewWithBusID(60008)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	require.True(t, bus.CanAddRepresentableRegistration(1))
	first, err := bus.AddProvisionalRegistrationThrough(
		&closeFenceTestDevice{id: 1}, 1)
	require.NoError(t, err)
	require.Equal(t, uint32(1), first.Meta.DevID)
	require.False(t, bus.CanAddRepresentableRegistration(1))
	_, err = bus.AddProvisionalRegistrationThrough(
		&closeFenceTestDevice{id: 2}, 1)
	require.ErrorContains(t, err, "no available device address")

	removed, err := bus.RemoveRegistrationIfPresent(first)
	require.NoError(t, err)
	require.True(t, removed)
	require.True(t, bus.CanAddRepresentableRegistration(1))
	bus.mtx.Lock()
	bus.nextRegistrationToken = ^uint64(0)
	bus.mtx.Unlock()
	require.False(t, bus.CanAddRepresentableRegistration(1))
	_, err = bus.AddProvisionalRegistrationThrough(
		&closeFenceTestDevice{id: 3}, 1)
	require.ErrorContains(t, err, "registration token exhausted")
}

func TestRejectedDeviceIdentityCannotMutateFirstRegistration(t *testing.T) {
	bus, err := NewWithBusID(60009)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	var typedNil *closeFenceTestDevice
	for _, device := range []usb.Device{
		typedNil,
		nonComparableBusTestDevice{1},
		runtimeNonComparableBusTestDevice{payload: []byte{1}},
	} {
		require.NotPanics(t, func() {
			_, addErr := bus.AddRegistration(device)
			require.Error(t, addErr)
		})
		require.Empty(t, bus.GetAllDeviceMetas())
		require.NotNil(t, bus.GetBusEmptyContext())
	}
	registration, err := bus.AddRegistration(&closeFenceTestDevice{id: 1})
	require.NoError(t, err)
	forged := registration
	forged.Dev = runtimeNonComparableBusTestDevice{payload: []byte{1}}
	require.NotPanics(t, func() {
		require.False(t, bus.AuthenticatesRegistration(forged))
		lease, active := bus.AcquireRegistrationOperation(forged)
		require.False(t, active)
		lease.Release()
		published, publishErr := bus.PublishRegistration(forged)
		require.False(t, published)
		require.Error(t, publishErr)
		removed, removeErr := bus.RemoveRegistrationIfPresent(forged)
		require.False(t, removed)
		require.Error(t, removeErr)
	})
	require.True(t, bus.AuthenticatesRegistration(registration))
}

func TestStaleEmptyContextCannotCloseLaterEmptyIncarnation(t *testing.T) {
	bus := New(0xff004)
	t.Cleanup(func() { _ = bus.Close() })
	firstEmpty := bus.GetBusEmptyContext()
	require.NotNil(t, firstEmpty)
	device := &closeFenceTestDevice{id: 1}
	deviceContext, err := bus.Add(device)
	require.NoError(t, err)
	registration, found := bus.GetDeviceRegistration(device, deviceContext)
	require.True(t, found)
	require.NoError(t, bus.Remove(device))
	secondEmpty := bus.GetBusEmptyContext()
	require.NotNil(t, secondEmpty)
	require.NotEqual(t, firstEmpty, secondEmpty)

	closed, err := bus.CloseIfEmptyContext(firstEmpty)
	require.NoError(t, err)
	require.False(t, closed)
	require.False(t, bus.IsClosed())
	_, err = bus.Add(device)
	require.NoError(t, err)
	require.False(t, bus.AuthenticatesRegistration(registration))
}

func TestRejectedAddDoesNotCancelCurrentEmptyIncarnation(t *testing.T) {
	bus := New(0xff005)
	t.Cleanup(func() { _ = bus.Close() })
	empty := bus.GetBusEmptyContext()
	_, err := bus.Add(nil)
	require.Error(t, err)
	select {
	case <-empty.Done():
		t.Fatal("rejected Add cancelled the current empty incarnation")
	default:
	}
	closed, err := bus.CloseIfEmptyContext(empty)
	require.NoError(t, err)
	require.True(t, closed)
}

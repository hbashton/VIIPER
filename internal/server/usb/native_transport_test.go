package usb

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/transport/udecx"
	usbdevice "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/virtualbus"
)

type nativeTransportTestDriver struct {
	createErr error
	created   []udecx.CreateDevice
	destroyed []udecx.DeviceIdentity
}

type blockingNativeTransportTestDriver struct {
	nativeTransportTestDriver
	createStarted chan struct{}
	allowCreate   chan struct{}
}

type blockingDestroyNativeTransportTestDriver struct {
	nativeTransportTestDriver
	destroyStarted chan struct{}
	allowDestroy   chan struct{}
}

func (d *blockingDestroyNativeTransportTestDriver) DestroyDevice(
	ctx context.Context, identity udecx.DeviceIdentity,
) error {
	close(d.destroyStarted)
	select {
	case <-d.allowDestroy:
	case <-ctx.Done():
		return ctx.Err()
	}
	return d.nativeTransportTestDriver.DestroyDevice(ctx, identity)
}

func (d *blockingNativeTransportTestDriver) CreateDevice(
	ctx context.Context, device udecx.CreateDevice,
) (udecx.DeviceRegistration, error) {
	close(d.createStarted)
	select {
	case <-d.allowCreate:
	case <-ctx.Done():
		return udecx.DeviceRegistration{}, ctx.Err()
	}
	return d.nativeTransportTestDriver.CreateDevice(ctx, device)
}

func (d *nativeTransportTestDriver) CreateDevice(_ context.Context, device udecx.CreateDevice) (udecx.DeviceRegistration, error) {
	d.created = append(d.created, device)
	if d.createErr != nil {
		return udecx.DeviceRegistration{}, d.createErr
	}
	registration := udecx.DeviceRegistration{
		DeviceIdentity: udecx.DeviceIdentity{DeviceID: device.DeviceID, Generation: device.Generation},
		Speed:          device.Speed, ControllerSessionID: 17,
		ControllerInstanceID: `ROOT\VIIPERUDE\0000`,
	}
	if device.Speed == udecx.DeviceSpeedSuper {
		registration.USB30PortNumber = udecx.MaxDevices + 1
	} else {
		registration.USB20PortNumber = 1
	}
	return registration, nil
}
func (d *nativeTransportTestDriver) DestroyDevice(_ context.Context, identity udecx.DeviceIdentity) error {
	d.destroyed = append(d.destroyed, identity)
	return nil
}
func (*nativeTransportTestDriver) Dequeue(ctx context.Context, _ []byte) (udecx.Operation, error) {
	<-ctx.Done()
	return udecx.Operation{}, ctx.Err()
}
func (*nativeTransportTestDriver) Complete(context.Context, udecx.Completion) error { return nil }
func (*nativeTransportTestDriver) QueryStats(context.Context) (udecx.Stats, error) {
	return udecx.Stats{}, nil
}

type nativeTransportTestProcessor struct{}

func (*nativeTransportTestProcessor) Process(context.Context, usbdevice.Device, udecx.Operation) (udecx.Completion, error) {
	return udecx.Completion{}, nil
}
func (*nativeTransportTestProcessor) Lifecycle(context.Context, usbdevice.Device, udecx.Operation) error {
	return nil
}
func (*nativeTransportTestProcessor) Reset(usbdevice.Device, udecx.DeviceIdentity) {}

func newNativeTransportTestDevice() usbdevice.Device {
	return &altSettingTestDevice{desc: &usbdevice.Descriptor{
		Device: usbdevice.DeviceDescriptor{
			BcdUSB: 0x0200, BMaxPacketSize0: 64, IDVendor: 1, IDProduct: 2,
			BNumConfigurations: 1, Speed: uint32(udecx.DeviceSpeedHigh),
		},
		Interfaces: []usbdevice.InterfaceConfig{{Descriptor: usbdevice.InterfaceDescriptor{
			BInterfaceNumber: 0, BNumEndpoints: 1, BInterfaceClass: 3,
		}, Endpoints: []usbdevice.EndpointDescriptor{{
			BEndpointAddress: 0x81, BMAttributes: 3, WMaxPacketSize: 64, BInterval: 4,
		}}}},
	}}
}

func TestNativeTransportPublishesAndUnpublishesWithVirtualBus(t *testing.T) {
	driver := &nativeTransportTestDriver{}
	host, err := udecx.NewHost(driver, &nativeTransportTestProcessor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	server := New(ServerConfig{ConnectionTimeout: time.Second}, slog.Default(), nil)
	if err := server.EnableNativeTransport(host); err != nil {
		t.Fatal(err)
	}
	bus, err := virtualbus.NewWithBusID(98101)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}

	if _, err := server.AddDeviceToBus(context.Background(), bus.BusID(), newNativeTransportTestDevice()); err != nil {
		t.Fatal(err)
	}
	if len(driver.created) != 1 || len(bus.Devices()) != 1 {
		t.Fatalf("created=%d bus devices=%d want 1/1", len(driver.created), len(bus.Devices()))
	}
	if err := server.RemoveDeviceByID(bus.BusID(), "1"); err != nil {
		t.Fatal(err)
	}
	if len(driver.destroyed) != 1 || len(bus.Devices()) != 0 {
		t.Fatalf("destroyed=%d bus devices=%d want 1/0", len(driver.destroyed), len(bus.Devices()))
	}
}

func TestNativeTransportExactRemoveCannotDeleteIDReusingSuccessor(t *testing.T) {
	driver := &nativeTransportTestDriver{}
	host, err := udecx.NewHost(driver, &nativeTransportTestProcessor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	server := New(ServerConfig{
		ConnectionTimeout: time.Second, BusCleanupTimeout: time.Hour,
	}, slog.Default(), nil)
	if err := server.EnableNativeTransport(host); err != nil {
		t.Fatal(err)
	}
	bus, err := virtualbus.NewWithBusID(98104)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}

	firstDevice := newNativeTransportTestDevice()
	firstContext, first, err := server.AddDeviceToBusWithRegistration(
		context.Background(), bus.BusID(), firstDevice)
	if err != nil || first == nil {
		t.Fatalf("add first native device: registration=%v error=%v", first, err)
	}
	if captured, ok := server.NativeDeviceRegistrationForDevice(
		bus.BusID(), 1, firstDevice, firstContext); !ok ||
		captured.DeviceIdentity != first.DeviceIdentity {
		t.Fatalf("exact first lifetime capture: ok=%t captured=%+v want=%+v",
			ok, captured.DeviceIdentity, first.DeviceIdentity)
	}
	unrelatedContext, cancelUnrelated := context.WithCancel(context.Background())
	defer cancelUnrelated()
	if _, ok := server.NativeDeviceRegistrationForDevice(
		bus.BusID(), 1, firstDevice, unrelatedContext); ok {
		t.Fatal("native lifetime capture accepted an unrelated device context")
	}
	if err := server.RemoveNativeDeviceExact(bus.BusID(), "1", *first); err != nil {
		t.Fatalf("remove first native device: %v", err)
	}

	successorDevice := newNativeTransportTestDevice()
	successorContext, successor, err := server.AddDeviceToBusWithRegistration(
		context.Background(), bus.BusID(), successorDevice)
	if err != nil || successor == nil {
		t.Fatalf("add successor native device: registration=%v error=%v", successor, err)
	}
	if successor.DeviceID != first.DeviceID || successor.Generation <= first.Generation {
		t.Fatalf("successor identity=%+v did not reuse ID after first=%+v", successor.DeviceIdentity, first.DeviceIdentity)
	}

	err = server.RemoveNativeDeviceExact(bus.BusID(), "1", *first)
	if !errors.Is(err, ErrNativeDeviceCorrelationMismatch) {
		t.Fatalf("stale exact remove error=%v want correlation mismatch", err)
	}
	if len(driver.destroyed) != 1 || len(bus.Devices()) != 1 {
		t.Fatalf("stale removal mutated successor: destroyed=%d devices=%d want 1/1",
			len(driver.destroyed), len(bus.Devices()))
	}
	if _, ok := server.NativeDeviceRegistrationForDevice(
		bus.BusID(), 1, firstDevice, firstContext); ok {
		t.Fatal("retired first device/context captured the successor registration")
	}
	if captured, ok := server.NativeDeviceRegistrationForDevice(
		bus.BusID(), 1, successorDevice, successorContext); !ok ||
		captured.DeviceIdentity != successor.DeviceIdentity {
		t.Fatalf("exact successor lifetime capture: ok=%t captured=%+v want=%+v",
			ok, captured.DeviceIdentity, successor.DeviceIdentity)
	}
	snapshots, err := server.SnapshotBusDevices(bus.BusID())
	if err != nil || len(snapshots) != 1 || snapshots[0].NativeRegistration == nil ||
		snapshots[0].NativeRegistration.DeviceIdentity != successor.DeviceIdentity {
		t.Fatalf("successor registration changed after stale remove: snapshots=%v error=%v want=%+v",
			snapshots, err, successor.DeviceIdentity)
	}

	if err := server.RemoveNativeDeviceExact(bus.BusID(), "1", *successor); err != nil {
		t.Fatalf("remove exact successor: %v", err)
	}
	if len(driver.destroyed) != 2 || len(bus.Devices()) != 0 {
		t.Fatalf("exact successor removal: destroyed=%d devices=%d want 2/0",
			len(driver.destroyed), len(bus.Devices()))
	}
}

func TestNativeTransportExactRemoveRejectsMalformedOrStaleReceiptWithoutMutation(t *testing.T) {
	mutations := []struct {
		name string
		edit func(*udecx.DeviceRegistration)
	}{
		{"device id", func(r *udecx.DeviceRegistration) { r.DeviceID++ }},
		{"device generation", func(r *udecx.DeviceRegistration) { r.Generation++ }},
		{"controller session", func(r *udecx.DeviceRegistration) { r.ControllerSessionID++ }},
		{"controller root", func(r *udecx.DeviceRegistration) { r.ControllerInstanceID = `ROOT\VIIPERUDE\0001` }},
		{"usb port", func(r *udecx.DeviceRegistration) { r.USB20PortNumber++ }},
		{"two ports", func(r *udecx.DeviceRegistration) { r.USB30PortNumber = udecx.MaxDevices + 1 }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			driver := &nativeTransportTestDriver{}
			host, err := udecx.NewHost(driver, &nativeTransportTestProcessor{}, 1)
			if err != nil {
				t.Fatal(err)
			}
			server := New(ServerConfig{
				ConnectionTimeout: time.Second, BusCleanupTimeout: time.Hour,
			}, slog.Default(), nil)
			if err := server.EnableNativeTransport(host); err != nil {
				t.Fatal(err)
			}
			bus, err := virtualbus.NewWithBusID(98105)
			if err != nil {
				t.Fatal(err)
			}
			defer bus.Close()
			if err := server.AddBus(bus); err != nil {
				t.Fatal(err)
			}
			_, registration, err := server.AddDeviceToBusWithRegistration(
				context.Background(), bus.BusID(), newNativeTransportTestDevice())
			if err != nil || registration == nil {
				t.Fatalf("add native device: registration=%v error=%v", registration, err)
			}
			stale := *registration
			mutation.edit(&stale)
			err = server.RemoveNativeDeviceExact(bus.BusID(), "1", stale)
			if err == nil {
				t.Fatal("malformed/stale exact remove unexpectedly succeeded")
			}
			if len(driver.destroyed) != 0 || len(bus.Devices()) != 1 {
				t.Fatalf("rejected exact remove mutated device: destroyed=%d devices=%d",
					len(driver.destroyed), len(bus.Devices()))
			}
		})
	}
}

func TestNativeDeviceListSnapshotCannotObserveHalfRemovedRegistration(t *testing.T) {
	driver := &blockingDestroyNativeTransportTestDriver{
		destroyStarted: make(chan struct{}), allowDestroy: make(chan struct{}),
	}
	host, err := udecx.NewHost(driver, &nativeTransportTestProcessor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	server := New(ServerConfig{
		ConnectionTimeout: time.Second, BusCleanupTimeout: time.Hour,
	}, slog.Default(), nil)
	if err := server.EnableNativeTransport(host); err != nil {
		t.Fatal(err)
	}
	bus, err := virtualbus.NewWithBusID(98108)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}
	_, registration, err := server.AddDeviceToBusWithRegistration(
		context.Background(), bus.BusID(), newNativeTransportTestDevice())
	if err != nil || registration == nil {
		t.Fatalf("add native device: registration=%v error=%v", registration, err)
	}

	removeDone := make(chan error, 1)
	go func() {
		removeDone <- server.RemoveNativeDeviceExact(bus.BusID(), "1", *registration)
	}()
	<-driver.destroyStarted

	snapshotDone := make(chan struct {
		snapshots []BusDeviceSnapshot
		err       error
	}, 1)
	go func() {
		snapshots, snapshotErr := server.SnapshotBusDevices(bus.BusID())
		snapshotDone <- struct {
			snapshots []BusDeviceSnapshot
			err       error
		}{snapshots, snapshotErr}
	}()
	select {
	case result := <-snapshotDone:
		t.Fatalf("snapshot crossed in-progress removal: snapshots=%v error=%v",
			result.snapshots, result.err)
	case <-time.After(25 * time.Millisecond):
	}

	close(driver.allowDestroy)
	if err := <-removeDone; err != nil {
		t.Fatal(err)
	}
	result := <-snapshotDone
	if result.err != nil || len(result.snapshots) != 0 {
		t.Fatalf("post-removal snapshot=%v error=%v want empty", result.snapshots, result.err)
	}

	_, successor, err := server.AddDeviceToBusWithRegistration(
		context.Background(), bus.BusID(), newNativeTransportTestDevice())
	if err != nil || successor == nil {
		t.Fatalf("add successor: registration=%v error=%v", successor, err)
	}
	snapshots, err := server.SnapshotBusDevices(bus.BusID())
	if err != nil || len(snapshots) != 1 || snapshots[0].NativeRegistration == nil ||
		snapshots[0].NativeRegistration.DeviceIdentity != successor.DeviceIdentity {
		t.Fatalf("successor snapshot=%v error=%v want exact %+v",
			snapshots, err, successor.DeviceIdentity)
	}
}

func TestEmptyBusCleanupCannotRemoveReusedBusInstance(t *testing.T) {
	server := New(ServerConfig{BusCleanupTimeout: 75 * time.Millisecond}, slog.Default(), nil)
	oldBus, err := virtualbus.NewWithBusID(98106)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.AddBus(oldBus); err != nil {
		t.Fatal(err)
	}
	if _, err := oldBus.Add(newNativeTransportTestDevice()); err != nil {
		t.Fatal(err)
	}
	if err := server.RemoveDeviceByID(oldBus.BusID(), "1"); err != nil {
		t.Fatal(err)
	}
	if err := server.RemoveBus(oldBus.BusID()); err != nil {
		t.Fatal(err)
	}

	successor, err := virtualbus.NewWithBusID(oldBus.BusID())
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	if err := server.AddBus(successor); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if current := server.GetBus(successor.BusID()); current != successor {
		t.Fatalf("stale empty-bus timer removed successor bus: current=%p successor=%p", current, successor)
	}
}

func TestEmptyBusRemovalRechecksNonemptyStateUnderLifecycleLock(t *testing.T) {
	server := New(ServerConfig{}, slog.Default(), nil)
	bus, err := virtualbus.NewWithBusID(98107)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Add(newNativeTransportTestDevice()); err != nil {
		t.Fatal(err)
	}
	if err := server.removeCurrentBusIfEmpty(bus.BusID(), bus); !errors.Is(err, ErrBusNotEmpty) {
		t.Fatalf("nonempty exact bus cleanup error=%v want ErrBusNotEmpty", err)
	}
	if current := server.GetBus(bus.BusID()); current != bus || len(bus.Devices()) != 1 {
		t.Fatalf("nonempty exact bus cleanup mutated bus: current=%p bus=%p devices=%d",
			current, bus, len(bus.Devices()))
	}
}

func TestNativeTransportRollsBackVirtualBusWhenPlugInFails(t *testing.T) {
	driver := &nativeTransportTestDriver{createErr: errors.New("driver rejected child")}
	host, _ := udecx.NewHost(driver, &nativeTransportTestProcessor{}, 1)
	server := New(ServerConfig{}, slog.Default(), nil)
	if err := server.EnableNativeTransport(host); err != nil {
		t.Fatal(err)
	}
	bus, err := virtualbus.NewWithBusID(98102)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	if err := server.AddBus(bus); err != nil {
		t.Fatal(err)
	}

	if _, err := server.AddDeviceToBus(context.Background(), bus.BusID(), newNativeTransportTestDevice()); err == nil {
		t.Fatal("native plug-in unexpectedly succeeded")
	}
	if len(bus.Devices()) != 0 {
		t.Fatal("failed native plug-in leaked a virtual bus device")
	}
}

func TestNativeTransportRemovalCannotRaceUncommittedPlugIn(t *testing.T) {
	driver := &blockingNativeTransportTestDriver{
		createStarted: make(chan struct{}),
		allowCreate:   make(chan struct{}),
	}
	host, err := udecx.NewHost(driver, &nativeTransportTestProcessor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	server := New(ServerConfig{ConnectionTimeout: time.Second}, slog.Default(), nil)
	if err = server.EnableNativeTransport(host); err != nil {
		t.Fatal(err)
	}
	bus, err := virtualbus.NewWithBusID(98103)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	if err = server.AddBus(bus); err != nil {
		t.Fatal(err)
	}

	addDone := make(chan error, 1)
	go func() {
		_, addErr := server.AddDeviceToBus(
			context.Background(), bus.BusID(), newNativeTransportTestDevice())
		addDone <- addErr
	}()
	<-driver.createStarted

	removeStarted := make(chan struct{})
	removeDone := make(chan error, 1)
	go func() {
		close(removeStarted)
		removeDone <- server.RemoveDeviceByID(bus.BusID(), "1")
	}()
	<-removeStarted
	select {
	case err = <-removeDone:
		t.Fatalf("remove crossed uncommitted native plug-in: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(driver.allowCreate)
	if err = <-addDone; err != nil {
		t.Fatal(err)
	}
	if err = <-removeDone; err != nil {
		t.Fatal(err)
	}
	if len(driver.created) != 1 || len(driver.destroyed) != 1 || len(bus.Devices()) != 0 {
		t.Fatalf("created=%d destroyed=%d bus devices=%d want 1/1/0",
			len(driver.created), len(driver.destroyed), len(bus.Devices()))
	}
	server.nativeMu.Lock()
	remaining := len(server.nativeIDs)
	server.nativeMu.Unlock()
	if remaining != 0 {
		t.Fatalf("native identity table retained %d entries", remaining)
	}
}

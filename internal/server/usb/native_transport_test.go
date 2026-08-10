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

func (d *nativeTransportTestDriver) CreateDevice(_ context.Context, device udecx.CreateDevice) error {
	d.created = append(d.created, device)
	return d.createErr
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

package xboxone

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
)

func TestAuthorizedRetainedUSBDeviceBindsOneExactAdapterAndDescriptor(t *testing.T) {
	authorization := testOnlyAuthorizedPersonaConfig(t)
	profile := authorization.owner.config.Profile
	executor := newScriptedControllerPersonaLocalExecutor()
	device, err := NewAuthorizedDormantRetainedUSBDevice(
		authorization, 91, testAuthorizedRetainedAuthorityID,
		testAuthorizedRetainedDeviceID, executor, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("NewAuthorizedDormantRetainedUSBDevice: %v", err)
	}
	if device == nil || device.adapter == nil ||
		device.RetainedUSBImportOwner() != device.adapter ||
		device.RetainedUSBImportDeviceID() != testAuthorizedRetainedDeviceID {
		t.Fatal("device did not retain one exact adapter/resource identity")
	}

	var wantDevice [USBDeviceDescriptorSize]byte
	if err := profile.EncodeUSBDeviceDescriptorInto(wantDevice[:]); err != nil {
		t.Fatalf("EncodeUSBDeviceDescriptorInto: %v", err)
	}
	if got := device.GetDescriptor().Bytes(); !bytes.Equal(got, wantDevice[:]) {
		t.Fatalf("exported device descriptor = % x, want % x", got, wantDevice)
	}
	interfaceDescriptor := device.descriptor.Interfaces[0]
	if interfaceDescriptor.Descriptor.BInterfaceClass != 0xff ||
		interfaceDescriptor.Descriptor.BInterfaceSubClass != 0x47 ||
		interfaceDescriptor.Descriptor.BInterfaceProtocol != 0xd0 ||
		len(interfaceDescriptor.Endpoints) != 2 ||
		interfaceDescriptor.Endpoints[0].BEndpointAddress != 0x01 ||
		interfaceDescriptor.Endpoints[1].BEndpointAddress != 0x81 ||
		interfaceDescriptor.Endpoints[0].BInterval != uint8(profile.usb.OUTIntervalMS) ||
		interfaceDescriptor.Endpoints[1].BInterval != uint8(profile.usb.INIntervalMS) {
		t.Fatal("exported route descriptor diverged from the authorized profile")
	}
	if authorization.owner.binding.engine != device.adapter.coordinator.engine {
		t.Fatal("device descriptor and retained owner do not share authorization")
	}
}

func TestAuthorizedRetainedUSBDeviceDescriptorExposureCannotMutateIdentity(
	t *testing.T,
) {
	device, err := NewAuthorizedDormantRetainedUSBDevice(
		testOnlyAuthorizedPersonaConfig(t), 91,
		testAuthorizedRetainedAuthorityID, testAuthorizedRetainedDeviceID,
		newScriptedControllerPersonaLocalExecutor(), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("NewAuthorizedDormantRetainedUSBDevice: %v", err)
	}

	first := device.GetDescriptor()
	first.Device.IDVendor = 1
	first.Interfaces[0].Descriptor.BInterfaceClass = 3
	first.Interfaces[0].Endpoints[0].BEndpointAddress = 0x8f
	first.Interfaces = append(first.Interfaces, first.Interfaces[0])

	second := device.GetDescriptor()
	if second == first || second.Device.IDVendor != device.descriptor.Device.IDVendor ||
		len(second.Interfaces) != 1 ||
		second.Interfaces[0].Descriptor.BInterfaceClass != 0xff ||
		second.Interfaces[0].Endpoints[0].BEndpointAddress != 0x01 {
		t.Fatalf("mutable descriptor escaped: first=%+v second=%+v",
			first, second)
	}
	second.Interfaces[0].Endpoints[1].BEndpointAddress = 0x82
	third := device.GetDescriptor()
	if third.Interfaces[0].Endpoints[1].BEndpointAddress != 0x81 {
		t.Fatal("nested endpoint storage aliases a prior descriptor snapshot")
	}
}

func TestAuthorizedRetainedUSBDeviceRejectsLegacyTransferByQuarantine(t *testing.T) {
	device, err := NewAuthorizedDormantRetainedUSBDevice(
		testOnlyAuthorizedPersonaConfig(t), 92,
		testAuthorizedRetainedAuthorityID, testAuthorizedRetainedDeviceID,
		newScriptedControllerPersonaLocalExecutor(), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("NewAuthorizedDormantRetainedUSBDevice: %v", err)
	}
	if response := device.HandleTransfer(
		context.Background(), 1, 1, nil); response != nil {
		t.Fatalf("legacy dispatch returned data: % x", response)
	}
	device.adapter.mu.Lock()
	state := device.adapter.state
	quarantine := device.adapter.quarantine
	device.adapter.mu.Unlock()
	if state != dormantRetainedUSBQuarantine ||
		!errors.Is(quarantine, errAuthorizedRetainedUSBLegacyDispatch) {
		t.Fatalf("legacy dispatch did not quarantine: state=%d err=%v",
			state, quarantine)
	}
}

func TestAuthorizedRetainedUSBDevicePublishesOnlyToExactBoundSession(t *testing.T) {
	device, err := NewAuthorizedDormantRetainedUSBDevice(
		testOnlyAuthorizedPersonaConfig(t), 93,
		testAuthorizedRetainedAuthorityID, testAuthorizedRetainedDeviceID,
		newScriptedControllerPersonaLocalExecutor(), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("NewAuthorizedDormantRetainedUSBDevice: %v", err)
	}
	state := InputStateV1{LeftStickX: 0x1234}
	wire := make([]byte, SemanticInputWireSize)
	if err := EncodeSemanticInputWireV1Into(wire, state); err != nil {
		t.Fatalf("EncodeSemanticInputWireV1Into: %v", err)
	}
	if err := device.PublishSemanticInputWire(2, wire); !errors.Is(
		err, errDormantRetainedUSBInvalidRequest) {
		t.Fatalf("unbound publish = %v", err)
	}
	lease := retainedusb.ImportLease{
		AuthorityID: testAuthorizedRetainedAuthorityID,
		DeviceID:    testAuthorizedRetainedDeviceID,
		OwnerID:     device.adapter.Identity(), ImportToken: 1,
		SessionGeneration: 1,
	}
	if result, err := device.adapter.BindImport(
		lease, time.Now().Add(time.Second)); err != nil ||
		result.State != retainedusb.ImportBindBound {
		t.Fatalf("BindImport = (%+v, %v)", result, err)
	}
	if err := device.PublishSemanticInputWire(2, wire); err != nil {
		t.Fatalf("bound publish: %v", err)
	}
	device.adapter.mu.Lock()
	got := device.adapter.input.State
	device.adapter.mu.Unlock()
	if got != state {
		t.Fatalf("published state = %+v, want %+v", got, state)
	}
}

package xboxone

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
	"github.com/Alia5/VIIPER/usb"
)

var errAuthorizedRetainedUSBLegacyDispatch = errors.New(
	"xboxone: retained-only USB device reached legacy transfer dispatch")

// AuthorizedDormantRetainedUSBDevice is the retained-only usb.Device wrapper
// for one authorized Xbox One/Series owner. It lets the production USB/IP
// server select descriptors and the explicit retained import capability
// without exposing the private persona engine or creating a second mapping
// stack. It remains absent from the generic API device registry; the explicit
// internal broker factory is its only production construction site.
type AuthorizedDormantRetainedUSBDevice struct {
	adapter            *DormantRetainedUSBAdapter
	descriptor         usb.Descriptor
	deviceID           uint64
	primaryGIPDeviceID uint64

	brokerMu            sync.Mutex
	brokerStreamToken   uint64
	brokerStreamActive  bool
	brokerFailed        bool // permanent after input retirement or ambiguous/failed feedback
	brokerInputRetired  bool // preserves only the original feedback consumer during teardown
	brokerConsumerReady bool
	brokerActivating    bool
	brokerActivated     bool
	brokerInputRevision uint64
	feedbackBridge      *controllerPersonaFeedbackStreamBridge
	removal             productionRemovalCapability
}

// NewAuthorizedDormantRetainedUSBDevice consumes the same one-shot external
// identity authorization as NewAuthorizedDormantRetainedUSBAdapter and wraps
// that exact adapter pointer in the server's retained-device capability. The
// exported descriptor is derived from the successfully authorized engine's
// exact profile, so USB/IP discovery and the owner's EP0 responses cannot be
// composed from different caller identities.
func NewAuthorizedDormantRetainedUSBDevice(
	authorization AuthorizedControllerPersonaConfig,
	nowMS uint64,
	expectedAuthorityID uint64,
	expectedDeviceID uint64,
	local ControllerPersonaLocalExecutor,
	localTimeout time.Duration,
) (*AuthorizedDormantRetainedUSBDevice, error) {
	adapter, err := NewAuthorizedDormantRetainedUSBAdapter(
		authorization, nowMS, expectedAuthorityID, expectedDeviceID,
		local, localTimeout)
	if err != nil {
		return nil, err
	}
	if adapter == nil || adapter.coordinator == nil ||
		adapter.coordinator.engine == nil {
		return nil, ErrInvalidAuthorizedRetainedUSBComposition
	}
	descriptor, err := makeAuthorizedRetainedUSBDescriptor(
		adapter.coordinator.engine.profile)
	if err != nil {
		adapter.mu.Lock()
		adapter.quarantineLocked(err)
		adapter.mu.Unlock()
		return nil, errors.Join(ErrInvalidAuthorizedRetainedUSBComposition, err)
	}
	return &AuthorizedDormantRetainedUSBDevice{
		adapter: adapter, descriptor: descriptor, deviceID: expectedDeviceID,
		primaryGIPDeviceID: adapter.coordinator.engine.profile.identity.DeviceID,
	}, nil
}

func (device *AuthorizedDormantRetainedUSBDevice) RetainedUSBPrimaryGIPDeviceID() uint64 {
	if device == nil {
		return 0
	}
	return device.primaryGIPDeviceID
}

func makeAuthorizedRetainedUSBDescriptor(
	profile UnregisteredControllerProfile,
) (usb.Descriptor, error) {
	if err := profile.validate(); err != nil {
		return usb.Descriptor{}, err
	}
	identity := profile.identity
	config := profile.usb
	return usb.Descriptor{
		Device: usb.DeviceDescriptor{
			BcdUSB: 0x0200, BDeviceClass: 0xff, BDeviceSubClass: 0x47,
			BDeviceProtocol: 0xd0, BMaxPacketSize0: 0x40,
			IDVendor: identity.VendorID, IDProduct: identity.ProductID,
			BcdDevice:     identity.DeviceReleaseBCD,
			IManufacturer: 1, IProduct: 2, ISerialNumber: 3,
			BNumConfigurations: 1, Speed: 2,
		},
		Configuration: usb.ConfigurationDescriptor{
			BConfigurationValue: 1, BMAttributes: 0xa0,
			BMaxPower: uint8(config.MaxPower2mA),
		},
		Interfaces: []usb.InterfaceConfig{{
			Descriptor: usb.InterfaceDescriptor{
				BInterfaceNumber: 0, BAlternateSetting: 0, BNumEndpoints: 2,
				BInterfaceClass: 0xff, BInterfaceSubClass: 0x47,
				BInterfaceProtocol: 0xd0,
			},
			Endpoints: []usb.EndpointDescriptor{
				{BEndpointAddress: 0x01, BMAttributes: 0x03,
					WMaxPacketSize: 0x0040, BInterval: uint8(config.OUTIntervalMS)},
				{BEndpointAddress: 0x81, BMAttributes: 0x03,
					WMaxPacketSize: 0x0040, BInterval: uint8(config.INIntervalMS)},
			},
		}},
	}, nil
}

// HandleTransfer must never be reached: the server rejects this device when
// retained imports are disabled and routes all three admitted lanes through
// retainedusb.Owner when enabled. Quarantine makes an accidental legacy call
// terminal instead of silently presenting a second behavior.
func (device *AuthorizedDormantRetainedUSBDevice) HandleTransfer(
	context.Context,
	uint32,
	uint32,
	[]byte,
) []byte {
	if device != nil && device.adapter != nil {
		device.adapter.mu.Lock()
		device.adapter.quarantineLocked(errAuthorizedRetainedUSBLegacyDispatch)
		device.adapter.mu.Unlock()
	}
	return nil
}

func (device *AuthorizedDormantRetainedUSBDevice) GetDescriptor() *usb.Descriptor {
	if device == nil {
		return nil
	}
	// usb.Descriptor contains slice, map, and pointer fields. Returning the
	// stored value would let an otherwise read-only discovery caller mutate the
	// descriptor after authorization and make DEVLIST/import identity diverge
	// from the retained owner's exact EP0/control-plane identity. Descriptor
	// exposure is cold-path, so return an independently owned deep snapshot.
	descriptor := cloneAuthorizedRetainedUSBDescriptor(device.descriptor)
	return &descriptor
}

func cloneAuthorizedRetainedUSBDescriptor(source usb.Descriptor) usb.Descriptor {
	clone := source
	if source.MicrosoftOS10 != nil {
		value := *source.MicrosoftOS10
		clone.MicrosoftOS10 = &value
	}
	clone.Associations = append(
		[]usb.InterfaceAssociationDescriptor(nil), source.Associations...)
	clone.Interfaces = make([]usb.InterfaceConfig, len(source.Interfaces))
	for interfaceIndex := range source.Interfaces {
		interfaceSource := source.Interfaces[interfaceIndex]
		interfaceClone := interfaceSource
		interfaceClone.Endpoints = make(
			[]usb.EndpointDescriptor, len(interfaceSource.Endpoints))
		for endpointIndex := range interfaceSource.Endpoints {
			endpointSource := interfaceSource.Endpoints[endpointIndex]
			endpointClone := endpointSource
			endpointClone.Trailing = append(
				usb.Data(nil), endpointSource.Trailing...)
			endpointClone.ClassDescriptors = cloneAuthorizedRetainedUSBClassDescriptors(
				endpointSource.ClassDescriptors)
			interfaceClone.Endpoints[endpointIndex] = endpointClone
		}
		interfaceClone.ClassDescriptors = cloneAuthorizedRetainedUSBClassDescriptors(
			interfaceSource.ClassDescriptors)
		if interfaceSource.HID != nil {
			hidClone := *interfaceSource.HID
			hidClone.Descriptor.Descriptors = append(
				[]usb.HIDSubDescriptor(nil),
				interfaceSource.HID.Descriptor.Descriptors...)
			hidClone.ReportDescriptorBytes = append(
				usb.Data(nil), interfaceSource.HID.ReportDescriptorBytes...)
			interfaceClone.HID = &hidClone
		}
		clone.Interfaces[interfaceIndex] = interfaceClone
	}
	if source.Strings != nil {
		clone.Strings = make(map[uint8]string, len(source.Strings))
		for index, value := range source.Strings {
			clone.Strings[index] = value
		}
	}
	return clone
}

func cloneAuthorizedRetainedUSBClassDescriptors(
	source []usb.ClassSpecificDescriptor,
) []usb.ClassSpecificDescriptor {
	clone := make([]usb.ClassSpecificDescriptor, len(source))
	for index := range source {
		clone[index] = source[index]
		clone[index].Payload = append(usb.Data(nil), source[index].Payload...)
	}
	return clone
}

func (device *AuthorizedDormantRetainedUSBDevice) GetDeviceSpecificArgs() map[string]any {
	if device == nil {
		return nil
	}
	return map[string]any{
		"retainedUSB": true,
		"deviceID":    device.deviceID,
	}
}

// VIIPERDeviceType selects the production Xbox stream handler without adding
// this retained-only device to the generic construction registry.
func (device *AuthorizedDormantRetainedUSBDevice) VIIPERDeviceType() string {
	return "xboxone"
}

func (device *AuthorizedDormantRetainedUSBDevice) RetainedUSBImportDeviceID() uint64 {
	if device == nil {
		return 0
	}
	return device.deviceID
}

func (device *AuthorizedDormantRetainedUSBDevice) RetainedUSBImportOwner() retainedusb.ImportOwner {
	if device == nil || device.adapter == nil {
		return nil
	}
	return device.adapter
}

// RetainedUSBRemoveAfterSafeDisconnect declares the wrapper's actual one-shot
// executor lifetime. A future reconnect-capable composition must provide a
// fresh authenticated executor factory and use a different device policy.
func (device *AuthorizedDormantRetainedUSBDevice) RetainedUSBRemoveAfterSafeDisconnect() bool {
	return device != nil
}

// PublishSemanticInputWire publishes into the exact currently bound retained
// session without exposing that outer capability to the broker. A concurrent
// close/reset can invalidate the sampled generation and then the adapter
// rejects the publication without changing input state.
func (device *AuthorizedDormantRetainedUSBDevice) PublishSemanticInputWire(
	revision uint64,
	wire []byte,
) error {
	if device == nil || device.adapter == nil {
		return errDormantRetainedUSBUninitialized
	}
	device.adapter.mu.Lock()
	if device.adapter.state != dormantRetainedUSBBound ||
		!device.adapter.boundLease.Valid() {
		device.adapter.mu.Unlock()
		return errDormantRetainedUSBInvalidRequest
	}
	generation := device.adapter.boundLease.SessionGeneration
	device.adapter.mu.Unlock()
	return device.adapter.PublishSemanticInputWire(generation, revision, wire)
}

var _ usb.Device = (*AuthorizedDormantRetainedUSBDevice)(nil)
var _ retainedusb.ImportDevice = (*AuthorizedDormantRetainedUSBDevice)(nil)
var _ retainedusb.OneShotImportDevice = (*AuthorizedDormantRetainedUSBDevice)(nil)

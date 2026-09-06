package xboxone

import (
	"encoding/binary"
	"fmt"
)

const (
	// USBDeviceDescriptorSize is the MS-GIPUSB table 7 device descriptor.
	USBDeviceDescriptorSize = 18
	// USBControllerConfigurationDescriptorSize is one configuration, the GIP
	// data interface, and its OUT and IN endpoint descriptors.
	USBControllerConfigurationDescriptorSize = 9 + 9 + 7 + 7
	// USBLanguageIDDescriptorSize is the mandatory English (United States)
	// language string descriptor.
	USBLanguageIDDescriptorSize = 4
	// MicrosoftOSStringDescriptorSize is the fixed MSFT100 descriptor.
	MicrosoftOSStringDescriptorSize = 18
	// MicrosoftExtendedCompatibleIDDescriptorSize is the one-interface
	// XGIP10 extended compatible ID descriptor.
	MicrosoftExtendedCompatibleIDDescriptorSize = 40
	// MicrosoftExtendedPropertiesDescriptorSize is the header-only OS 1.0
	// properties response used when the persona publishes no registry values.
	MicrosoftExtendedPropertiesDescriptorSize = 10

	// MicrosoftOSStringIndex is the standard Microsoft OS string index.
	MicrosoftOSStringIndex byte = 0xee
	// MicrosoftOSVendorCode is the default vendor code specified by
	// MS-GIPUSB 1.0 table 5.
	MicrosoftOSVendorCode byte = 0x90
	// USBEnglishUnitedStatesLanguageID is the only LANGID required by
	// MS-GIPUSB 1.0 section 2.2.4.
	USBEnglishUnitedStatesLanguageID uint16 = 0x0409

	minimumGIPInterruptIntervalMS uint16 = 4
	maximumUSBPower2mA            uint16 = 250
	primaryDeviceIDPrefix         uint64 = 0x0000fffb00000000
	primaryDeviceIDPrefixMask     uint64 = 0xffffffff00000000
)

// FirmwareVersion is the four-part version carried in the primary Hello.
type FirmwareVersion struct {
	Major    uint16
	Minor    uint16
	Build    uint16
	Revision uint16
}

func (version FirmwareVersion) validate() error {
	if version.Major == 0 && version.Minor == 0 &&
		version.Build == 0 && version.Revision == 0 {
		return ErrInvalidFirmwareVersion
	}
	return nil
}

// ControllerIdentity contains only caller-owned numeric identity. The
// package provides no default VID, PID, Device ID, firmware identity, or
// manufacturer identity.
type ControllerIdentity struct {
	VendorID         uint16
	ProductID        uint16
	DeviceReleaseBCD uint16
	DeviceID         uint64
	Firmware         FirmwareVersion
	HardwareMajor    uint8
	HardwareMinor    uint8
}

// Validate enforces the identity fields that MS-GIPUSB defines locally. It
// cannot establish that the caller owns the supplied VID/PID allocation.
func (identity ControllerIdentity) Validate() error {
	if identity.VendorID == 0 || identity.ProductID == 0 {
		return fmt.Errorf("%w: vid=0x%04x pid=0x%04x",
			ErrInvalidUSBIdentity, identity.VendorID, identity.ProductID)
	}
	if identity.DeviceID&primaryDeviceIDPrefixMask != primaryDeviceIDPrefix {
		return fmt.Errorf("%w: 0x%016x", ErrInvalidDeviceID, identity.DeviceID)
	}
	if !validBCD16(identity.DeviceReleaseBCD) {
		return fmt.Errorf("%w: 0x%04x", ErrInvalidDeviceReleaseBCD, identity.DeviceReleaseBCD)
	}
	if err := identity.Firmware.validate(); err != nil {
		return err
	}
	return nil
}

func validBCD16(value uint16) bool {
	for shift := uint(0); shift < 16; shift += 4 {
		if value>>shift&0x0f > 9 {
			return false
		}
	}
	return true
}

// ControllerUSBConfig supplies the two descriptor values that cannot be
// inferred safely: actual peak bus power and endpoint polling intervals.
// Both intervals are milliseconds and must be at least four.
type ControllerUSBConfig struct {
	MaxPower2mA   uint16
	OUTIntervalMS uint16
	INIntervalMS  uint16
}

// controllerProfileIssuance is a private, non-zero-sized construction
// credential. Value copies of one profile intentionally share an issuance;
// independently reconstructed equal numeric profiles do not. The byte keeps
// distinct allocations from being coalesced as zero-sized Go objects.
type controllerProfileIssuance struct {
	marker byte
}

// Validate enforces MS-GIPUSB tables 8, 10, and 11.
func (config ControllerUSBConfig) Validate() error {
	if config.MaxPower2mA == 0 || config.MaxPower2mA > maximumUSBPower2mA {
		return fmt.Errorf("%w: %d units", ErrInvalidUSBPower, config.MaxPower2mA)
	}
	if config.OUTIntervalMS < minimumGIPInterruptIntervalMS || config.OUTIntervalMS > 0xff {
		return fmt.Errorf("%w: OUT=%dms", ErrInvalidUSBInterval, config.OUTIntervalMS)
	}
	if config.INIntervalMS < minimumGIPInterruptIntervalMS || config.INIntervalMS > 0xff {
		return fmt.Errorf("%w: IN=%dms", ErrInvalidUSBInterval, config.INIntervalMS)
	}
	return nil
}

// UnregisteredControllerProfile binds descriptor and Hello generation to one
// explicit caller identity. It has no registration, transport, or backend
// behavior. Its zero value is deliberately unusable.
type UnregisteredControllerProfile struct {
	identity ControllerIdentity
	usb      ControllerUSBConfig
	issuance *controllerProfileIssuance
	valid    bool
}

// NewUnregisteredControllerProfile validates an explicit identity and USB
// configuration. Successful construction is not evidence that the caller
// owns the VID/PID or that Windows will bind the resulting descriptors.
func NewUnregisteredControllerProfile(
	identity ControllerIdentity,
	usb ControllerUSBConfig,
) (UnregisteredControllerProfile, error) {
	if err := identity.Validate(); err != nil {
		return UnregisteredControllerProfile{}, err
	}
	if err := usb.Validate(); err != nil {
		return UnregisteredControllerProfile{}, err
	}
	return UnregisteredControllerProfile{
		identity: identity,
		usb:      usb,
		issuance: &controllerProfileIssuance{marker: 1},
		valid:    true,
	}, nil
}

func (profile UnregisteredControllerProfile) validate() error {
	if !profile.valid || profile.issuance == nil || profile.issuance.marker != 1 {
		return ErrUninitializedControllerProfile
	}
	if err := profile.identity.Validate(); err != nil {
		return err
	}
	return profile.usb.Validate()
}

// Identity returns the validated caller identity bound to the profile.
func (profile UnregisteredControllerProfile) Identity() (ControllerIdentity, error) {
	if err := profile.validate(); err != nil {
		return ControllerIdentity{}, err
	}
	return profile.identity, nil
}

// EncodeUSBDeviceDescriptorInto writes the exact table 7 descriptor. String
// indexes one through three remain caller-supplied descriptor seams.
func (profile UnregisteredControllerProfile) EncodeUSBDeviceDescriptorInto(dst []byte) error {
	if len(dst) != USBDeviceDescriptorSize {
		return exactLengthError("USB device descriptor", len(dst), USBDeviceDescriptorSize)
	}
	if err := profile.validate(); err != nil {
		return err
	}

	var wire [USBDeviceDescriptorSize]byte
	wire[0] = USBDeviceDescriptorSize
	wire[1] = 0x01
	binary.LittleEndian.PutUint16(wire[2:4], 0x0200)
	wire[4] = 0xff
	wire[5] = 0x47
	wire[6] = 0xd0
	wire[7] = 0x40
	binary.LittleEndian.PutUint16(wire[8:10], profile.identity.VendorID)
	binary.LittleEndian.PutUint16(wire[10:12], profile.identity.ProductID)
	binary.LittleEndian.PutUint16(wire[12:14], profile.identity.DeviceReleaseBCD)
	wire[14] = 0x01
	wire[15] = 0x02
	wire[16] = 0x03
	wire[17] = 0x01
	copy(dst, wire[:])
	return nil
}

// EncodeUSBControllerConfigurationDescriptorInto writes the exact
// controller-only configuration, FF/47/D0 data interface, and 64-byte
// interrupt OUT/IN endpoints from tables 8 through 11.
func (profile UnregisteredControllerProfile) EncodeUSBControllerConfigurationDescriptorInto(
	dst []byte,
) error {
	if len(dst) != USBControllerConfigurationDescriptorSize {
		return exactLengthError(
			"USB controller configuration descriptor",
			len(dst),
			USBControllerConfigurationDescriptorSize,
		)
	}
	if err := profile.validate(); err != nil {
		return err
	}

	var wire [USBControllerConfigurationDescriptorSize]byte
	// Configuration descriptor.
	wire[0] = 0x09
	wire[1] = 0x02
	binary.LittleEndian.PutUint16(wire[2:4], USBControllerConfigurationDescriptorSize)
	wire[4] = 0x01
	wire[5] = 0x01
	wire[6] = 0x00
	wire[7] = 0xa0
	wire[8] = byte(profile.usb.MaxPower2mA)
	// GIP data interface.
	copy(wire[9:18], []byte{0x09, 0x04, 0x00, 0x00, 0x02, 0xff, 0x47, 0xd0, 0x00})
	// Interrupt OUT endpoint 1.
	copy(wire[18:25], []byte{0x07, 0x05, 0x01, 0x03, 0x40, 0x00, byte(profile.usb.OUTIntervalMS)})
	// Interrupt IN endpoint 1.
	copy(wire[25:32], []byte{0x07, 0x05, 0x81, 0x03, 0x40, 0x00, byte(profile.usb.INIntervalMS)})
	copy(dst, wire[:])
	return nil
}

// EncodeUSBLanguageIDDescriptorInto writes the mandatory 0x0409 LANGID
// string descriptor.
func (profile UnregisteredControllerProfile) EncodeUSBLanguageIDDescriptorInto(dst []byte) error {
	if len(dst) != USBLanguageIDDescriptorSize {
		return exactLengthError("USB LANGID descriptor", len(dst), USBLanguageIDDescriptorSize)
	}
	if err := profile.validate(); err != nil {
		return err
	}
	dst[0], dst[1], dst[2], dst[3] = 0x04, 0x03, 0x09, 0x04
	return nil
}

// EncodeMicrosoftOSStringDescriptorInto writes table 5 exactly:
// MSFT100 in UTF-16LE, vendor code 0x90, and a zero pad byte.
func (profile UnregisteredControllerProfile) EncodeMicrosoftOSStringDescriptorInto(dst []byte) error {
	if len(dst) != MicrosoftOSStringDescriptorSize {
		return exactLengthError(
			"Microsoft OS string descriptor", len(dst), MicrosoftOSStringDescriptorSize)
	}
	if err := profile.validate(); err != nil {
		return err
	}
	copy(dst, []byte{
		0x12, 0x03,
		0x4d, 0x00, 0x53, 0x00, 0x46, 0x00, 0x54, 0x00,
		0x31, 0x00, 0x30, 0x00, 0x30, 0x00,
		MicrosoftOSVendorCode, 0x00,
	})
	return nil
}

// EncodeMicrosoftExtendedCompatibleIDDescriptorInto writes table 6 exactly
// for one non-audio interface and the XGIP10 compatible ID.
func (profile UnregisteredControllerProfile) EncodeMicrosoftExtendedCompatibleIDDescriptorInto(
	dst []byte,
) error {
	if len(dst) != MicrosoftExtendedCompatibleIDDescriptorSize {
		return exactLengthError(
			"Microsoft extended compatible ID descriptor",
			len(dst),
			MicrosoftExtendedCompatibleIDDescriptorSize,
		)
	}
	if err := profile.validate(); err != nil {
		return err
	}

	var wire [MicrosoftExtendedCompatibleIDDescriptorSize]byte
	binary.LittleEndian.PutUint32(wire[0:4], MicrosoftExtendedCompatibleIDDescriptorSize)
	binary.LittleEndian.PutUint16(wire[4:6], 0x0100)
	binary.LittleEndian.PutUint16(wire[6:8], 0x0004)
	wire[8] = 0x01
	wire[16] = 0x00
	wire[17] = 0x01
	copy(wire[18:26], []byte{'X', 'G', 'I', 'P', '1', '0', 0x00, 0x00})
	copy(dst, wire[:])
	return nil
}

// EncodeMicrosoftExtendedPropertiesDescriptorInto writes a valid empty
// Microsoft OS 1.0 extended-properties descriptor. XGIP binding does not need
// a device-interface GUID, but Windows still probes index 0x0005.
func (profile UnregisteredControllerProfile) EncodeMicrosoftExtendedPropertiesDescriptorInto(
	dst []byte,
) error {
	if len(dst) != MicrosoftExtendedPropertiesDescriptorSize {
		return exactLengthError(
			"Microsoft extended properties descriptor",
			len(dst),
			MicrosoftExtendedPropertiesDescriptorSize,
		)
	}
	if err := profile.validate(); err != nil {
		return err
	}

	var wire [MicrosoftExtendedPropertiesDescriptorSize]byte
	binary.LittleEndian.PutUint32(wire[0:4], MicrosoftExtendedPropertiesDescriptorSize)
	binary.LittleEndian.PutUint16(wire[4:6], 0x0100)
	binary.LittleEndian.PutUint16(wire[6:8], 0x0005)
	// wCount at bytes 8..9 remains zero.
	copy(dst, wire[:])
	return nil
}

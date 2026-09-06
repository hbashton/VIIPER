package xboxone

import (
	"errors"
	"testing"
)

// These deliberately conspicuous values are synthetic test data only. They
// are not a VID/PID allocation and must never be used for an actual device.
const (
	testOnlySyntheticVID uint16 = 0xf00d
	testOnlySyntheticPID uint16 = 0xbeef
)

func testOnlySyntheticControllerIdentity() ControllerIdentity {
	return ControllerIdentity{
		VendorID:         testOnlySyntheticVID,
		ProductID:        testOnlySyntheticPID,
		DeviceReleaseBCD: 0x0102,
		DeviceID:         0x0000fffb01020304,
		Firmware: FirmwareVersion{
			Major: 1, Minor: 2, Build: 3, Revision: 4,
		},
		HardwareMajor: 5,
		HardwareMinor: 6,
	}
}

func testOnlySyntheticControllerProfile(t *testing.T) UnregisteredControllerProfile {
	t.Helper()
	profile, err := NewUnregisteredControllerProfile(
		testOnlySyntheticControllerIdentity(),
		ControllerUSBConfig{MaxPower2mA: 0x32, OUTIntervalMS: 4, INIntervalMS: 8},
	)
	if err != nil {
		t.Fatalf("NewUnregisteredControllerProfile: %v", err)
	}
	return profile
}

func TestUnregisteredUSBDescriptorsAreByteExact(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)

	t.Run("device", func(t *testing.T) {
		want := [USBDeviceDescriptorSize]byte{
			0x12, 0x01, 0x00, 0x02, 0xff, 0x47, 0xd0, 0x40,
			0x0d, 0xf0, 0xef, 0xbe, 0x02, 0x01, 0x01, 0x02,
			0x03, 0x01,
		}
		var got [USBDeviceDescriptorSize]byte
		if err := profile.EncodeUSBDeviceDescriptorInto(got[:]); err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got != want {
			t.Fatalf("device descriptor = % x, want % x", got, want)
		}
	})

	t.Run("configuration interface and endpoints", func(t *testing.T) {
		want := [USBControllerConfigurationDescriptorSize]byte{
			0x09, 0x02, 0x20, 0x00, 0x01, 0x01, 0x00, 0xa0, 0x32,
			0x09, 0x04, 0x00, 0x00, 0x02, 0xff, 0x47, 0xd0, 0x00,
			0x07, 0x05, 0x01, 0x03, 0x40, 0x00, 0x04,
			0x07, 0x05, 0x81, 0x03, 0x40, 0x00, 0x08,
		}
		var got [USBControllerConfigurationDescriptorSize]byte
		if err := profile.EncodeUSBControllerConfigurationDescriptorInto(got[:]); err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got != want {
			t.Fatalf("configuration descriptor = % x, want % x", got, want)
		}
	})

	t.Run("language ID", func(t *testing.T) {
		want := [USBLanguageIDDescriptorSize]byte{0x04, 0x03, 0x09, 0x04}
		var got [USBLanguageIDDescriptorSize]byte
		if err := profile.EncodeUSBLanguageIDDescriptorInto(got[:]); err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got != want {
			t.Fatalf("LANGID descriptor = % x, want % x", got, want)
		}
	})

	t.Run("MSFT100", func(t *testing.T) {
		want := [MicrosoftOSStringDescriptorSize]byte{
			0x12, 0x03,
			0x4d, 0x00, 0x53, 0x00, 0x46, 0x00, 0x54, 0x00,
			0x31, 0x00, 0x30, 0x00, 0x30, 0x00,
			0x90, 0x00,
		}
		var got [MicrosoftOSStringDescriptorSize]byte
		if err := profile.EncodeMicrosoftOSStringDescriptorInto(got[:]); err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got != want {
			t.Fatalf("Microsoft OS string = % x, want % x", got, want)
		}
	})

	t.Run("XGIP10", func(t *testing.T) {
		want := [MicrosoftExtendedCompatibleIDDescriptorSize]byte{
			0x28, 0x00, 0x00, 0x00, 0x00, 0x01, 0x04, 0x00,
			0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x01, 'X', 'G', 'I', 'P', '1', '0', 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		}
		var got [MicrosoftExtendedCompatibleIDDescriptorSize]byte
		if err := profile.EncodeMicrosoftExtendedCompatibleIDDescriptorInto(got[:]); err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got != want {
			t.Fatalf("extended compatible ID = % x, want % x", got, want)
		}
	})

	t.Run("empty extended properties", func(t *testing.T) {
		want := [MicrosoftExtendedPropertiesDescriptorSize]byte{
			0x0a, 0x00, 0x00, 0x00, 0x00, 0x01, 0x05, 0x00,
			0x00, 0x00,
		}
		var got [MicrosoftExtendedPropertiesDescriptorSize]byte
		if err := profile.EncodeMicrosoftExtendedPropertiesDescriptorInto(got[:]); err != nil {
			t.Fatalf("encode: %v", err)
		}
		if got != want {
			t.Fatalf("extended properties = % x, want % x", got, want)
		}
	})
}

func TestUnregisteredControllerProfileRejectsInvalidIdentityAndUSBValues(t *testing.T) {
	validIdentity := testOnlySyntheticControllerIdentity()
	validUSB := ControllerUSBConfig{MaxPower2mA: 1, OUTIntervalMS: 4, INIntervalMS: 4}

	tests := []struct {
		name     string
		identity ControllerIdentity
		usb      ControllerUSBConfig
		want     error
	}{
		{name: "zero VID", identity: func() ControllerIdentity { value := validIdentity; value.VendorID = 0; return value }(), usb: validUSB, want: ErrInvalidUSBIdentity},
		{name: "zero PID", identity: func() ControllerIdentity { value := validIdentity; value.ProductID = 0; return value }(), usb: validUSB, want: ErrInvalidUSBIdentity},
		{name: "wrong Device ID prefix", identity: func() ControllerIdentity { value := validIdentity; value.DeviceID = 1; return value }(), usb: validUSB, want: ErrInvalidDeviceID},
		{name: "invalid BCD", identity: func() ControllerIdentity { value := validIdentity; value.DeviceReleaseBCD = 0x01fa; return value }(), usb: validUSB, want: ErrInvalidDeviceReleaseBCD},
		{name: "zero firmware", identity: func() ControllerIdentity { value := validIdentity; value.Firmware = FirmwareVersion{}; return value }(), usb: validUSB, want: ErrInvalidFirmwareVersion},
		{name: "zero power", identity: validIdentity, usb: ControllerUSBConfig{OUTIntervalMS: 4, INIntervalMS: 4}, want: ErrInvalidUSBPower},
		{name: "power above 500mA", identity: validIdentity, usb: ControllerUSBConfig{MaxPower2mA: 251, OUTIntervalMS: 4, INIntervalMS: 4}, want: ErrInvalidUSBPower},
		{name: "OUT interval below four", identity: validIdentity, usb: ControllerUSBConfig{MaxPower2mA: 1, OUTIntervalMS: 3, INIntervalMS: 4}, want: ErrInvalidUSBInterval},
		{name: "IN interval below four", identity: validIdentity, usb: ControllerUSBConfig{MaxPower2mA: 1, OUTIntervalMS: 4, INIntervalMS: 3}, want: ErrInvalidUSBInterval},
		{name: "IN interval wider than byte", identity: validIdentity, usb: ControllerUSBConfig{MaxPower2mA: 1, OUTIntervalMS: 4, INIntervalMS: 256}, want: ErrInvalidUSBInterval},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewUnregisteredControllerProfile(test.identity, test.usb); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestZeroProfileCannotEmitDescriptorAndLeavesDestinationUnchanged(t *testing.T) {
	want := [USBDeviceDescriptorSize]byte{}
	for index := range want {
		want[index] = 0xa5
	}
	got := want
	var profile UnregisteredControllerProfile
	if err := profile.EncodeUSBDeviceDescriptorInto(got[:]); !errors.Is(err, ErrUninitializedControllerProfile) {
		t.Fatalf("error = %v, want ErrUninitializedControllerProfile", err)
	}
	if got != want {
		t.Fatalf("destination = % x, want unchanged % x", got, want)
	}
}

func TestUSBDescriptorEncodersRequireExactLength(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	tests := []struct {
		name string
		size int
		fn   func([]byte) error
	}{
		{name: "device", size: USBDeviceDescriptorSize, fn: profile.EncodeUSBDeviceDescriptorInto},
		{name: "configuration", size: USBControllerConfigurationDescriptorSize, fn: profile.EncodeUSBControllerConfigurationDescriptorInto},
		{name: "LANGID", size: USBLanguageIDDescriptorSize, fn: profile.EncodeUSBLanguageIDDescriptorInto},
		{name: "MSFT100", size: MicrosoftOSStringDescriptorSize, fn: profile.EncodeMicrosoftOSStringDescriptorInto},
		{name: "XGIP10", size: MicrosoftExtendedCompatibleIDDescriptorSize, fn: profile.EncodeMicrosoftExtendedCompatibleIDDescriptorInto},
		{name: "extended properties", size: MicrosoftExtendedPropertiesDescriptorSize, fn: profile.EncodeMicrosoftExtendedPropertiesDescriptorInto},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, size := range []int{test.size - 1, test.size + 1} {
				if err := test.fn(make([]byte, size)); !errors.Is(err, ErrInvalidLength) {
					t.Errorf("length %d: error = %v, want ErrInvalidLength", size, err)
				}
			}
		})
	}
}

func TestUSBDescriptorEncodersAllocateZero(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	var device [USBDeviceDescriptorSize]byte
	var configuration [USBControllerConfigurationDescriptorSize]byte
	var osString [MicrosoftOSStringDescriptorSize]byte
	var compatible [MicrosoftExtendedCompatibleIDDescriptorSize]byte
	var properties [MicrosoftExtendedPropertiesDescriptorSize]byte
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := profile.EncodeUSBDeviceDescriptorInto(device[:]); err != nil {
			panic(err)
		}
		if err := profile.EncodeUSBControllerConfigurationDescriptorInto(configuration[:]); err != nil {
			panic(err)
		}
		if err := profile.EncodeMicrosoftOSStringDescriptorInto(osString[:]); err != nil {
			panic(err)
		}
		if err := profile.EncodeMicrosoftExtendedCompatibleIDDescriptorInto(compatible[:]); err != nil {
			panic(err)
		}
		if err := profile.EncodeMicrosoftExtendedPropertiesDescriptorInto(properties[:]); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("descriptor allocations = %v, want 0", allocs)
	}
}

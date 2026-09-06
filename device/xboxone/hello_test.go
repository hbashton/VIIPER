package xboxone

import (
	"errors"
	"reflect"
	"testing"
)

func TestHelloMessageGoldenRoundTripAndDescriptorIdentityMatch(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	want := [HelloMessageSize]byte{
		0x02, 0x20, 0x7f, 0x1c,
		0x04, 0x03, 0x02, 0x01, 0xfb, 0xff, 0x00, 0x00,
		0x0d, 0xf0, 0xef, 0xbe,
		0x01, 0x00, 0x02, 0x00, 0x03, 0x00, 0x04, 0x00,
		0x05, 0x06,
		0x01, 0x00, 0x01, 0x00, 0x01, 0x00,
	}
	var got [HelloMessageSize]byte
	if err := profile.EncodeHelloMessageInto(got[:], 0x7f); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got != want {
		t.Fatalf("Hello = % x, want % x", got, want)
	}
	hello, err := DecodeHelloMessage(got[:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantHello := HelloMessageV1{
		Sequence:      0x7f,
		DeviceID:      0x0000fffb01020304,
		VendorID:      testOnlySyntheticVID,
		ProductID:     testOnlySyntheticPID,
		Firmware:      FirmwareVersion{Major: 1, Minor: 2, Build: 3, Revision: 4},
		HardwareMajor: 5,
		HardwareMinor: 6,
	}
	if !reflect.DeepEqual(hello, wantHello) {
		t.Fatalf("decoded = %+v, want %+v", hello, wantHello)
	}

	var device [USBDeviceDescriptorSize]byte
	if err := profile.EncodeUSBDeviceDescriptorInto(device[:]); err != nil {
		t.Fatalf("device descriptor: %v", err)
	}
	if device[8] != got[12] || device[9] != got[13] ||
		device[10] != got[14] || device[11] != got[15] {
		t.Fatalf("descriptor VID/PID % x does not match Hello % x", device[8:12], got[12:16])
	}
}

func TestHelloMessageRequiresExactLength(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	for _, size := range []int{HelloMessageSize - 1, HelloMessageSize + 1} {
		if err := profile.EncodeHelloMessageInto(make([]byte, size), 1); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("encode length %d: error = %v, want ErrInvalidLength", size, err)
		}
		if _, err := DecodeHelloMessage(make([]byte, size)); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("decode length %d: error = %v, want ErrInvalidLength", size, err)
		}
	}
}

func TestHelloMessageRejectsMalformedFields(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	var valid [HelloMessageSize]byte
	if err := profile.EncodeHelloMessageInto(valid[:], 1); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		offset int
		value  byte
	}{
		{name: "wrong message", offset: 0, value: 0x03},
		{name: "non-system", offset: 1, value: 0x00},
		{name: "ack requested", offset: 1, value: 0x30},
		{name: "zero sequence", offset: 2, value: 0x00},
		{name: "wrong payload length", offset: 3, value: 0x1b},
		{name: "wrong Device ID prefix", offset: 10, value: 0x01},
		{name: "wrong RF version", offset: 26, value: 0x02},
		{name: "wrong security minor", offset: 29, value: 0x01},
		{name: "wrong GIP major", offset: 30, value: 0x00},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wire := valid
			wire[test.offset] = test.value
			if _, err := DecodeHelloMessage(wire[:]); !errors.Is(err, ErrInvalidHelloMessage) {
				t.Fatalf("error = %v, want ErrInvalidHelloMessage", err)
			}
		})
	}

	t.Run("zero VID", func(t *testing.T) {
		wire := valid
		wire[12], wire[13] = 0, 0
		if _, err := DecodeHelloMessage(wire[:]); !errors.Is(err, ErrInvalidHelloMessage) {
			t.Fatalf("error = %v, want ErrInvalidHelloMessage", err)
		}
	})

	t.Run("all-zero firmware", func(t *testing.T) {
		wire := valid
		for index := 16; index < 24; index++ {
			wire[index] = 0
		}
		if _, err := DecodeHelloMessage(wire[:]); !errors.Is(err, ErrInvalidHelloMessage) {
			t.Fatalf("error = %v, want ErrInvalidHelloMessage", err)
		}
	})
}

func TestHelloEncodeFailureDoesNotMutateDestination(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	want := [HelloMessageSize]byte{}
	for index := range want {
		want[index] = 0xa5
	}
	got := want
	if err := profile.EncodeHelloMessageInto(got[:], 0); !errors.Is(err, ErrReservedSequence) {
		t.Fatalf("error = %v, want ErrReservedSequence", err)
	}
	if got != want {
		t.Fatalf("destination = % x, want unchanged % x", got, want)
	}
}

func TestHelloCodecAllocatesZero(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	var wire [HelloMessageSize]byte
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := profile.EncodeHelloMessageInto(wire[:], 1); err != nil {
			panic(err)
		}
		if _, err := DecodeHelloMessage(wire[:]); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("Hello codec allocations = %v, want 0", allocs)
	}
}

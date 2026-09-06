package xboxone

import (
	"encoding/binary"
	"errors"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func FuzzExternalUSBStringDescriptorEncoding(f *testing.F) {
	f.Add("Acme")
	f.Add("Pad \U0001f3ae")
	f.Add("")
	f.Add(string([]byte{0xff}))
	f.Fuzz(func(t *testing.T, value string) {
		descriptor, err := encodeExternalUSBStringDescriptor("fuzz", value)
		if len(value) > usbMaximumStringSourceUTF8Bytes {
			if err == nil {
				t.Fatalf("overlong %d-byte UTF-8 source was admitted", len(value))
			}
			return
		}
		if err != nil {
			return
		}
		if value == "" || !utf8.ValidString(value) || !descriptor.validate() {
			t.Fatalf("invalid source admitted: %q descriptor=% x", value,
				descriptor.wire[:descriptor.size])
		}
		units := make([]uint16, 0, (int(descriptor.size)-2)/2)
		for offset := 2; offset < int(descriptor.size); offset += 2 {
			units = append(units,
				binary.LittleEndian.Uint16(descriptor.wire[offset:offset+2]))
		}
		if got := string(utf16.Decode(units)); got != value {
			t.Fatalf("UTF-16 round trip = %q, want %q", got, value)
		}
	})
}

func FuzzExternalUSBSerialIdentityGate(f *testing.F) {
	f.Add(testOnlySyntheticSerial)
	f.Add("0000FFFB01020304ffffffffffffffff")
	f.Add("11111111111111112222222222222222")
	f.Add("short")
	f.Fuzz(func(t *testing.T, serial string) {
		strings := testOnlyIdentityStrings()
		strings.Serial = serial
		_, err := buildExternalUSBIdentityDescriptorSet(
			testOnlySyntheticControllerProfileForFuzz(), strings)
		if err != nil {
			return
		}
		if len(serial) != 32 ||
			!serialContainsDeviceID(serial, testOnlySyntheticControllerIdentity().DeviceID) {
			t.Fatalf("invalid serial admitted: %q", serial)
		}
		for index := range serial {
			character := serial[index]
			if !((character >= '0' && character <= '9') ||
				(character >= 'a' && character <= 'f') ||
				(character >= 'A' && character <= 'F')) {
				t.Fatalf("non-hex serial admitted: %q", serial)
			}
		}
	})
}

func FuzzAuthorizedUSBIdentityRequestGate(f *testing.F) {
	f.Add(byte(1), USBEnglishUnitedStatesLanguageID, uint16(255))
	f.Add(byte(3), USBEnglishUnitedStatesLanguageID, uint16(1))
	f.Add(byte(4), USBEnglishUnitedStatesLanguageID, uint16(255))
	f.Add(byte(2), uint16(0x0411), uint16(255))
	f.Fuzz(func(t *testing.T, index byte, language, wLength uint16) {
		config := ControllerPersonaConfig{
			Profile:           testOnlySyntheticControllerProfileForFuzz(),
			CurrentInput:      GamepadInputReportV1{},
			CurrentStatus:     NewWiredNoBatteryStatus(false),
			PoweringOffStatus: NewWiredNoBatteryStatus(true),
		}
		metadata, err := config.Profile.BindExternallyCompiledMetadata([]byte{1})
		if err != nil {
			t.Fatal(err)
		}
		config.Metadata = metadata
		authorization, err := NewAuthorizedControllerPersonaConfig(
			config, testOnlyIdentityStrings(), ControllerIdentityAuthorizationGranted)
		if err != nil {
			t.Fatal(err)
		}
		engine, err := NewAuthorizedControllerPersonaEngine(authorization, 0)
		if err != nil {
			t.Fatal(err)
		}
		var setup [usbSetupPacketSize]byte
		setup[0] = usbRequestTypeDeviceIn
		setup[1] = usbRequestGetDescriptor
		binary.LittleEndian.PutUint16(setup[2:4],
			uint16(usbDescriptorTypeString)<<8|uint16(index))
		binary.LittleEndian.PutUint16(setup[4:6], language)
		binary.LittleEndian.PutUint16(setup[6:8], wLength)
		claim, err := engine.ClaimUSBControl(setup[:])
		valid := index >= 1 && index <= 3 &&
			language == USBEnglishUnitedStatesLanguageID
		if !valid {
			if !errors.Is(err, ErrUSBControlRequestStalled) || claim.Valid() ||
				engine.Snapshot().ClaimOutstanding {
				t.Fatalf("rejected shape = claim %+v err %v snapshot %+v",
					claim, err, engine.Snapshot())
			}
			return
		}
		if err != nil || !claim.Valid() {
			t.Fatalf("valid shape rejected: claim %+v err %v", claim, err)
		}
		var response [usbMaximumStringDescriptorSize]byte
		if claim.Size() > int(wLength) || claim.Size() > len(response) {
			t.Fatalf("response size %d exceeds request %d", claim.Size(), wLength)
		}
		if err := engine.AdmitAndCopy(claim, response[:claim.Size()], 0); err != nil {
			t.Fatal(err)
		}
		if err := engine.Resolve(claim, ControllerPersonaDelivered, 0); err != nil {
			t.Fatal(err)
		}
	})
}

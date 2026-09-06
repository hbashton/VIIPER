package xboxone

import (
	"bytes"
	"errors"
	"testing"
)

func TestOfficialBaseGamepadMetadataMatchesMicrosoftPCOptOutVector(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	firmware := profile.identity.Firmware
	firmware.Major = 1
	firmware.Minor = 0
	shape, err := OfficialGamepadMetadataBase.shape()
	if err != nil {
		t.Fatal(err)
	}
	got := compileOfficialGamepadMetadataV1(firmware, shape)
	want := []byte{
		0x10, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xc6, 0x00,
		0x87, 0x00, 0x16, 0x00, 0x1b, 0x00, 0x1c, 0x00, 0x23, 0x00, 0x29, 0x00, 0x46, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x06, 0x01, 0x02, 0x03,
		0x04, 0x06, 0x07, 0x05, 0x01, 0x04, 0x05, 0x06, 0x0a, 0x01, 0x1a, 0x00, 0x57, 0x69, 0x6e, 0x64,
		0x6f, 0x77, 0x73, 0x2e, 0x58, 0x62, 0x6f, 0x78, 0x2e, 0x49, 0x6e, 0x70, 0x75, 0x74, 0x2e, 0x47,
		0x61, 0x6d, 0x65, 0x70, 0x61, 0x64, 0x04, 0x56, 0xff, 0x76, 0x97, 0xfd, 0x9b, 0x81, 0x45, 0xad,
		0x45, 0xb6, 0x45, 0xbb, 0xa5, 0x26, 0xd6, 0x2c, 0x40, 0x2e, 0x08, 0xdf, 0x07, 0xe1, 0x45, 0xa5,
		0xab, 0xa3, 0x12, 0x7a, 0xf1, 0x97, 0xb5, 0xe7, 0x1f, 0xf3, 0xb8, 0x86, 0x73, 0xe9, 0x40, 0xa9,
		0xf8, 0x2f, 0x21, 0x26, 0x3a, 0xcf, 0xb7,
		0x77, 0xce, 0x34, 0x7a, 0xe2, 0x7d, 0xc6, 0x45, 0x8c, 0xa4, 0x00, 0x42, 0xc0, 0x8b, 0xd9, 0x4a,
		0x02, 0x17, 0x00, 0x20, 0x0e, 0x00, 0x01, 0x00, 0x10,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x17,
		0x00, 0x09, 0x09, 0x00, 0x01, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("compiled metadata differs from official vector\n got: % x\nwant: % x", got, want)
	}
	if err := validateOfficialGamepadMetadataV1(
		got, firmware, OfficialGamepadMetadataBase); err != nil {
		t.Fatal(err)
	}
}

func TestOfficialGamepadMetadataVariantsAndFirmwareBinding(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	for _, variant := range []OfficialGamepadMetadataVariant{
		OfficialGamepadMetadataBase,
		OfficialGamepadMetadataConsoleFunctionMap,
	} {
		metadata, err := profile.BindOfficialGamepadMetadataV1(variant)
		if err != nil {
			t.Fatalf("variant %d: %v", variant, err)
		}
		if !metadata.valid || metadata.identity != profile.identity ||
			metadata.profileIssuance != profile.issuance {
			t.Fatalf("variant %d produced an unbound result", variant)
		}
		if err := validateOfficialGamepadMetadataV1(
			metadata.data, profile.identity.Firmware, variant); err != nil {
			t.Fatalf("variant %d did not validate: %v", variant, err)
		}
		wrong := profile.identity.Firmware
		wrong.Minor++
		if err := validateOfficialGamepadMetadataV1(
			metadata.data, wrong, variant); !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("variant %d firmware mismatch error = %v", variant, err)
		}
	}
}

func TestOfficialGamepadMetadataSemanticValidatorRejectsEveryMutation(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	for _, variant := range []OfficialGamepadMetadataVariant{
		OfficialGamepadMetadataBase,
		OfficialGamepadMetadataConsoleFunctionMap,
	} {
		shape, _ := variant.shape()
		golden := compileOfficialGamepadMetadataV1(profile.identity.Firmware, shape)
		for index := range golden {
			mutated := append([]byte(nil), golden...)
			mutated[index] ^= 0x80
			if err := validateOfficialGamepadMetadataV1(
				mutated, profile.identity.Firmware, variant); !errors.Is(err, ErrInvalidMetadata) {
				t.Fatalf("variant %d byte %d mutation error = %v", variant, index, err)
			}
		}
	}
}

func TestOfficialGamepadMetadataRejectsUndefinedVariant(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	for _, variant := range []OfficialGamepadMetadataVariant{0, 3, 255} {
		if _, err := profile.BindOfficialGamepadMetadataV1(variant); !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("variant %d error = %v", variant, err)
		}
	}
}

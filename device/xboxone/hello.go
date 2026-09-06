package xboxone

import (
	"encoding/binary"
	"fmt"
)

const (
	// HelloPayloadSize is the primary Hello payload from MS-GIPUSB 1.0
	// section 3.1.5.5.1, table 27.
	HelloPayloadSize = 28
	// HelloMessageSize includes the exact four-byte single-packet header.
	HelloMessageSize = SinglePacketHeaderSize + HelloPayloadSize
	// MessageNumberHello is system Command 2.
	MessageNumberHello uint8 = 2
)

// HelloMessageV1 is the decoded, fixed protocol-1.0 primary-device Hello.
// USB device release is not part of the Hello payload.
type HelloMessageV1 struct {
	Sequence      uint8
	DeviceID      uint64
	VendorID      uint16
	ProductID     uint16
	Firmware      FirmwareVersion
	HardwareMajor uint8
	HardwareMinor uint8
}

func (hello HelloMessageV1) validate() error {
	if hello.Sequence == 0 {
		return ErrReservedSequence
	}
	if hello.DeviceID&primaryDeviceIDPrefixMask != primaryDeviceIDPrefix {
		return fmt.Errorf("%w: 0x%016x", ErrInvalidDeviceID, hello.DeviceID)
	}
	if hello.VendorID == 0 || hello.ProductID == 0 {
		return fmt.Errorf("%w: vid=0x%04x pid=0x%04x",
			ErrInvalidUSBIdentity, hello.VendorID, hello.ProductID)
	}
	return hello.Firmware.validate()
}

// EncodeHelloMessageInto writes table 27 exactly from the same validated
// identity used by the profile's USB device descriptor. This guarantees the
// required VID/PID match without supplying an identity default.
func (profile UnregisteredControllerProfile) EncodeHelloMessageInto(
	dst []byte,
	sequence uint8,
) error {
	if len(dst) != HelloMessageSize {
		return exactLengthError("GIP Hello message", len(dst), HelloMessageSize)
	}
	if err := profile.validate(); err != nil {
		return err
	}
	hello := HelloMessageV1{
		Sequence:      sequence,
		DeviceID:      profile.identity.DeviceID,
		VendorID:      profile.identity.VendorID,
		ProductID:     profile.identity.ProductID,
		Firmware:      profile.identity.Firmware,
		HardwareMajor: profile.identity.HardwareMajor,
		HardwareMinor: profile.identity.HardwareMinor,
	}
	if err := hello.validate(); err != nil {
		return err
	}

	var wire [HelloMessageSize]byte
	header := SinglePacketHeader{
		DataClass:     DataClassCommand,
		MessageNumber: MessageNumberHello,
		System:        true,
		Sequence:      sequence,
		PayloadLength: HelloPayloadSize,
	}
	if err := EncodeSinglePacketHeaderInto(wire[:SinglePacketHeaderSize], header); err != nil {
		return err
	}
	binary.LittleEndian.PutUint64(wire[4:12], hello.DeviceID)
	binary.LittleEndian.PutUint16(wire[12:14], hello.VendorID)
	binary.LittleEndian.PutUint16(wire[14:16], hello.ProductID)
	binary.LittleEndian.PutUint16(wire[16:18], hello.Firmware.Major)
	binary.LittleEndian.PutUint16(wire[18:20], hello.Firmware.Minor)
	binary.LittleEndian.PutUint16(wire[20:22], hello.Firmware.Build)
	binary.LittleEndian.PutUint16(wire[22:24], hello.Firmware.Revision)
	wire[24] = hello.HardwareMajor
	wire[25] = hello.HardwareMinor
	// RF, Security, and GIP protocol versions are all exactly 1.0.
	copy(wire[26:32], []byte{0x01, 0x00, 0x01, 0x00, 0x01, 0x00})
	copy(dst, wire[:])
	return nil
}

// DecodeHelloMessage decodes exactly one primary-device protocol-1.0 Hello.
func DecodeHelloMessage(src []byte) (HelloMessageV1, error) {
	if len(src) != HelloMessageSize {
		return HelloMessageV1{}, exactLengthError("GIP Hello message", len(src), HelloMessageSize)
	}
	header, err := DecodeSinglePacketHeader(src[:SinglePacketHeaderSize])
	if err != nil {
		return HelloMessageV1{}, fmt.Errorf("%w: %v", ErrInvalidHelloMessage, err)
	}
	if header.DataClass != DataClassCommand ||
		header.MessageNumber != MessageNumberHello ||
		!header.System || header.AcknowledgementRequested ||
		header.ExpansionIndex != 0 || header.PayloadLength != HelloPayloadSize {
		return HelloMessageV1{}, ErrInvalidHelloMessage
	}
	if src[26] != 0x01 || src[27] != 0x00 ||
		src[28] != 0x01 || src[29] != 0x00 ||
		src[30] != 0x01 || src[31] != 0x00 {
		return HelloMessageV1{}, ErrInvalidHelloMessage
	}

	hello := HelloMessageV1{
		Sequence:  header.Sequence,
		DeviceID:  binary.LittleEndian.Uint64(src[4:12]),
		VendorID:  binary.LittleEndian.Uint16(src[12:14]),
		ProductID: binary.LittleEndian.Uint16(src[14:16]),
		Firmware: FirmwareVersion{
			Major:    binary.LittleEndian.Uint16(src[16:18]),
			Minor:    binary.LittleEndian.Uint16(src[18:20]),
			Build:    binary.LittleEndian.Uint16(src[20:22]),
			Revision: binary.LittleEndian.Uint16(src[22:24]),
		},
		HardwareMajor: src[24],
		HardwareMinor: src[25],
	}
	if err := hello.validate(); err != nil {
		return HelloMessageV1{}, fmt.Errorf("%w: %v", ErrInvalidHelloMessage, err)
	}
	return hello, nil
}

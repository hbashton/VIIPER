package xboxone

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

const (
	officialMetadataHeaderSize       = 16
	officialDeviceMetadataHeaderSize = 22
	officialMessageMetadataSize      = 23

	officialGamepadInputMessageType = 0x20
	officialGamepadMotorMessageType = 0x09
	officialBaseInputPayloadSize    = 14
	officialShareInputPayloadSize   = 32
	officialDirectMotorPayloadSize  = 9
	officialMetadataDataTypeCustom  = 1
	officialMetadataFlagDownstream  = 1 << 3
	officialMetadataFlagUpstream    = 1 << 4
)

var (
	officialGamepadPreferredType = []byte("Windows.Xbox.Input.Gamepad")
	officialGamepadInterfaces    = [...][16]byte{
		// Windows.Xbox.Input.IController
		{0x56, 0xff, 0x76, 0x97, 0xfd, 0x9b, 0x81, 0x45, 0xad, 0x45, 0xb6, 0x45, 0xbb, 0xa5, 0x26, 0xd6},
		// Windows.Xbox.Input.IGamepad
		{0x2c, 0x40, 0x2e, 0x08, 0xdf, 0x07, 0xe1, 0x45, 0xa5, 0xab, 0xa3, 0x12, 0x7a, 0xf1, 0x97, 0xb5},
		// Windows.Xbox.Input.INavigationController
		{0xe7, 0x1f, 0xf3, 0xb8, 0x86, 0x73, 0xe9, 0x40, 0xa9, 0xf8, 0x2f, 0x21, 0x26, 0x3a, 0xcf, 0xb7},
		// IDevAuthPCOptOut. The official GIP hardware documentation says a
		// controller targeting Windows PC over USB should advertise this
		// interface so the host succeeds security without an exchange.
		{0x77, 0xce, 0x34, 0x7a, 0xe2, 0x7d, 0xc6, 0x45, 0x8c, 0xa4, 0x00, 0x42, 0xc0, 0x8b, 0xd9, 0x4a},
	}
	officialConsoleFunctionMapInterface = [16]byte{
		0xfe, 0xd2, 0xdd, 0xec, 0x87, 0xd3, 0x94, 0x42,
		0xbd, 0x96, 0x1a, 0x71, 0x2e, 0x3d, 0xc7, 0x7d,
	}
)

// OfficialGamepadMetadataVariant is the closed set of ordinary gamepad
// metadata shapes implemented by the official GIP metadata/compiler sources.
// The Share-capable shape adds IConsoleFunctionMap and expands the one low-
// latency input payload from fourteen to thirty-two bytes.
type OfficialGamepadMetadataVariant uint8

const (
	OfficialGamepadMetadataBase OfficialGamepadMetadataVariant = iota + 1
	OfficialGamepadMetadataConsoleFunctionMap
)

type officialGamepadMetadataShape struct {
	interfaceCount byte
	inputLength    uint16
	deviceSize     uint16
	totalSize      uint16
	share          bool
}

func (variant OfficialGamepadMetadataVariant) shape() (officialGamepadMetadataShape, error) {
	switch variant {
	case OfficialGamepadMetadataBase:
		return officialGamepadMetadataShape{
			interfaceCount: 4,
			inputLength:    officialBaseInputPayloadSize,
			deviceSize:     135,
			totalSize:      198,
		}, nil
	case OfficialGamepadMetadataConsoleFunctionMap:
		return officialGamepadMetadataShape{
			interfaceCount: 5,
			inputLength:    officialShareInputPayloadSize,
			deviceSize:     151,
			totalSize:      214,
			share:          true,
		}, nil
	default:
		return officialGamepadMetadataShape{}, fmt.Errorf(
			"%w: unsupported official gamepad variant %d", ErrInvalidMetadata, variant)
	}
}

// BindOfficialGamepadMetadataV1 compiles, independently validates, and binds
// the exact official ordinary-gamepad metadata shape to this profile. The
// advertised firmware major/minor is taken from the same profile that emits
// Hello, preventing a metadata/Hello version mismatch that Windows rejects.
func (profile UnregisteredControllerProfile) BindOfficialGamepadMetadataV1(
	variant OfficialGamepadMetadataVariant,
) (BoundCompiledMetadata, error) {
	if err := profile.validate(); err != nil {
		return BoundCompiledMetadata{}, err
	}
	shape, err := variant.shape()
	if err != nil {
		return BoundCompiledMetadata{}, err
	}
	blob := compileOfficialGamepadMetadataV1(profile.identity.Firmware, shape)
	if err := validateOfficialGamepadMetadataV1(
		blob, profile.identity.Firmware, variant); err != nil {
		return BoundCompiledMetadata{}, err
	}
	metadata, err := profile.BindExternallyCompiledMetadata(blob)
	if err != nil {
		return BoundCompiledMetadata{}, err
	}
	metadata.officialGamepadVariant = variant
	return metadata, nil
}

func compileOfficialGamepadMetadataV1(
	firmware FirmwareVersion,
	shape officialGamepadMetadataShape,
) []byte {
	blob := make([]byte, int(shape.totalSize))

	// Metadata header: size, version 1.0, four reserved words, total size.
	binary.LittleEndian.PutUint16(blob[0:2], officialMetadataHeaderSize)
	binary.LittleEndian.PutUint16(blob[2:4], 1)
	binary.LittleEndian.PutUint16(blob[14:16], shape.totalSize)

	device := blob[officialMetadataHeaderSize : officialMetadataHeaderSize+int(shape.deviceSize)]
	binary.LittleEndian.PutUint16(device[0:2], shape.deviceSize)
	binary.LittleEndian.PutUint16(device[2:4], 22)
	binary.LittleEndian.PutUint16(device[4:6], 27)
	binary.LittleEndian.PutUint16(device[6:8], 28)
	binary.LittleEndian.PutUint16(device[8:10], 35)
	binary.LittleEndian.PutUint16(device[10:12], 41)
	binary.LittleEndian.PutUint16(device[12:14], 70)
	// Supported HID descriptors are absent in metadata version 1.0. All
	// remaining device-header words are reserved zero.

	device[22] = 1
	binary.LittleEndian.PutUint16(device[23:25], firmware.Major)
	binary.LittleEndian.PutUint16(device[25:27], firmware.Minor)
	device[27] = 0
	copy(device[28:35], []byte{6, 1, 2, 3, 4, 6, 7})
	copy(device[35:41], []byte{5, 1, 4, 5, 6, 10})
	device[41] = 1
	binary.LittleEndian.PutUint16(device[42:44], uint16(len(officialGamepadPreferredType)))
	copy(device[44:70], officialGamepadPreferredType)
	device[70] = shape.interfaceCount
	offset := 71
	for _, identifier := range officialGamepadInterfaces {
		copy(device[offset:offset+len(identifier)], identifier[:])
		offset += len(identifier)
	}
	if shape.share {
		copy(device[offset:offset+len(officialConsoleFunctionMapInterface)],
			officialConsoleFunctionMapInterface[:])
	}

	messages := blob[officialMetadataHeaderSize+int(shape.deviceSize):]
	messages[0] = 2
	encodeOfficialMessageMetadata(messages[1:1+officialMessageMetadataSize],
		officialGamepadInputMessageType, shape.inputLength,
		officialMetadataFlagUpstream)
	encodeOfficialMessageMetadata(messages[1+officialMessageMetadataSize:],
		officialGamepadMotorMessageType, officialDirectMotorPayloadSize,
		officialMetadataFlagDownstream)
	return blob
}

func encodeOfficialMessageMetadata(
	dst []byte,
	messageType byte,
	payloadLength uint16,
	flags uint32,
) {
	binary.LittleEndian.PutUint16(dst[0:2], officialMessageMetadataSize)
	dst[2] = messageType
	binary.LittleEndian.PutUint16(dst[3:5], payloadLength)
	binary.LittleEndian.PutUint16(dst[5:7], officialMetadataDataTypeCustom)
	binary.LittleEndian.PutUint32(dst[7:11], flags)
	// Period, persistence timeout, and four reserved words remain zero.
}

func validateOfficialGamepadMetadataV1(
	blob []byte,
	firmware FirmwareVersion,
	variant OfficialGamepadMetadataVariant,
) error {
	shape, err := variant.shape()
	if err != nil {
		return err
	}
	invalid := func(field string) error {
		return fmt.Errorf("%w: official gamepad %s", ErrInvalidMetadata, field)
	}
	if len(blob) != int(shape.totalSize) {
		return invalid("total length")
	}
	if binary.LittleEndian.Uint16(blob[0:2]) != officialMetadataHeaderSize ||
		binary.LittleEndian.Uint16(blob[2:4]) != 1 ||
		binary.LittleEndian.Uint16(blob[4:6]) != 0 ||
		!allZero(blob[6:14]) ||
		binary.LittleEndian.Uint16(blob[14:16]) != shape.totalSize {
		return invalid("header")
	}

	device := blob[officialMetadataHeaderSize : officialMetadataHeaderSize+int(shape.deviceSize)]
	wantDeviceHeader := [...]uint16{
		shape.deviceSize, 22, 27, 28, 35, 41, 70, 0, 0, 0, 0,
	}
	for index, expected := range wantDeviceHeader {
		if binary.LittleEndian.Uint16(device[index*2:index*2+2]) != expected {
			return invalid(fmt.Sprintf("device header word %d", index))
		}
	}
	if device[22] != 1 ||
		binary.LittleEndian.Uint16(device[23:25]) != firmware.Major ||
		binary.LittleEndian.Uint16(device[25:27]) != firmware.Minor {
		return invalid("firmware list")
	}
	if device[27] != 0 ||
		!bytes.Equal(device[28:35], []byte{6, 1, 2, 3, 4, 6, 7}) ||
		!bytes.Equal(device[35:41], []byte{5, 1, 4, 5, 6, 10}) {
		return invalid("audio or system-command lists")
	}
	if device[41] != 1 ||
		binary.LittleEndian.Uint16(device[42:44]) != uint16(len(officialGamepadPreferredType)) ||
		!bytes.Equal(device[44:70], officialGamepadPreferredType) {
		return invalid("preferred type")
	}
	if device[70] != shape.interfaceCount {
		return invalid("interface count")
	}
	offset := 71
	for index, expected := range officialGamepadInterfaces {
		if !bytes.Equal(device[offset:offset+len(expected)], expected[:]) {
			return invalid(fmt.Sprintf("interface %d", index))
		}
		offset += len(expected)
	}
	if shape.share {
		if !bytes.Equal(device[offset:offset+len(officialConsoleFunctionMapInterface)],
			officialConsoleFunctionMapInterface[:]) {
			return invalid("console-function-map interface")
		}
		offset += len(officialConsoleFunctionMapInterface)
	}
	if offset != len(device) {
		return invalid("device boundary")
	}

	messages := blob[officialMetadataHeaderSize+len(device):]
	if len(messages) != 1+2*officialMessageMetadataSize || messages[0] != 2 {
		return invalid("message list")
	}
	if !validateOfficialMessageMetadata(
		messages[1:1+officialMessageMetadataSize], officialGamepadInputMessageType,
		shape.inputLength, officialMetadataFlagUpstream) {
		return invalid("input message")
	}
	if !validateOfficialMessageMetadata(
		messages[1+officialMessageMetadataSize:], officialGamepadMotorMessageType,
		officialDirectMotorPayloadSize, officialMetadataFlagDownstream) {
		return invalid("motor message")
	}
	return nil
}

func validateOfficialMessageMetadata(
	src []byte,
	messageType byte,
	payloadLength uint16,
	flags uint32,
) bool {
	return len(src) == officialMessageMetadataSize &&
		binary.LittleEndian.Uint16(src[0:2]) == officialMessageMetadataSize &&
		src[2] == messageType &&
		binary.LittleEndian.Uint16(src[3:5]) == payloadLength &&
		binary.LittleEndian.Uint16(src[5:7]) == officialMetadataDataTypeCustom &&
		binary.LittleEndian.Uint32(src[7:11]) == flags && allZero(src[11:])
}

func allZero(src []byte) bool {
	for _, value := range src {
		if value != 0 {
			return false
		}
	}
	return true
}

package xboxone

import (
	"encoding/binary"
	"fmt"
)

const (
	// ProtocolControlACKBodySize is the exact payload size in the official
	// downloadable Original GIP Specification, table 4-14.
	ProtocolControlACKBodySize = 9
	// ProtocolControlACKMessageSize includes the four-byte GIP header.
	ProtocolControlACKMessageSize = SinglePacketHeaderSize + ProtocolControlACKBodySize

	// MessageNumberProtocolControl is system Command message 1.
	MessageNumberProtocolControl uint8 = 1
	// ProtocolControlCodeACK is the only currently valid ControlCode.
	ProtocolControlCodeACK byte = 0
	// ProtocolControlACKVersion is the codec contract implemented here.
	ProtocolControlACKVersion uint16 = 1
)

// ProtocolControlACKBodyV1 is the exact nine-byte reliable-transfer ACK body.
// FragmentOffset is the total sequential data received. RemainingBuffer is the
// receiver's available message-buffer space; it is preserved but is not a
// sender-side progress grant.
type ProtocolControlACKBodyV1 struct {
	ReferencedDataClass      DataClass
	ReferencedMessageNumber  uint8
	ReferencedSystem         bool
	ReferencedExpansionIndex uint8
	FragmentOffset           uint32
	RemainingBuffer          uint16
}

// Validate enforces the RefMessageType domain and the three-bit Expansion
// Index. RefMessageFlags are constructed from typed System/Index fields, so
// Fragment, InitFrag, ACME, and Reserved cannot be emitted.
func (body ProtocolControlACKBodyV1) Validate() error {
	if body.ReferencedDataClass > DataClassAudio {
		return fmt.Errorf("%w: %d", ErrReservedDataClass, body.ReferencedDataClass)
	}
	if body.ReferencedMessageNumber > 0x1f {
		return fmt.Errorf("%w: %d", ErrInvalidMessageNumber, body.ReferencedMessageNumber)
	}
	if body.ReferencedExpansionIndex > flagExpansion {
		return fmt.Errorf("%w: %d", ErrInvalidExpansionIndex, body.ReferencedExpansionIndex)
	}
	return nil
}

func (body ProtocolControlACKBodyV1) referencedMessageType() byte {
	return byte(body.ReferencedDataClass)<<5 | body.ReferencedMessageNumber
}

func (body ProtocolControlACKBodyV1) referencedFlags() byte {
	flags := body.ReferencedExpansionIndex
	if body.ReferencedSystem {
		flags |= flagSystem
	}
	return flags
}

// EncodeProtocolControlACKBodyInto writes exactly nine bytes without
// allocating.
func EncodeProtocolControlACKBodyInto(dst []byte, body ProtocolControlACKBodyV1) error {
	if len(dst) != ProtocolControlACKBodySize {
		return exactLengthError("protocol-control ACK body", len(dst), ProtocolControlACKBodySize)
	}
	if err := body.Validate(); err != nil {
		return err
	}
	dst[0] = ProtocolControlCodeACK
	dst[1] = body.referencedMessageType()
	dst[2] = body.referencedFlags()
	binary.LittleEndian.PutUint32(dst[3:7], body.FragmentOffset)
	binary.LittleEndian.PutUint16(dst[7:9], body.RemainingBuffer)
	return nil
}

// DecodeProtocolControlACKBody decodes the official nine-byte ACK body. The
// formerly assigned non-zero control codes and every reserved value fail
// closed. RefMessageFlags may carry only System and Expansion Index.
func DecodeProtocolControlACKBody(src []byte) (ProtocolControlACKBodyV1, error) {
	if len(src) != ProtocolControlACKBodySize {
		return ProtocolControlACKBodyV1{}, exactLengthError(
			"protocol-control ACK body", len(src), ProtocolControlACKBodySize)
	}
	if src[0] != ProtocolControlCodeACK {
		return ProtocolControlACKBodyV1{}, fmt.Errorf(
			"%w: 0x%02x", ErrUnsupportedProtocolControlCode, src[0])
	}
	if src[2]&^(flagSystem|flagExpansion) != 0 {
		return ProtocolControlACKBodyV1{}, fmt.Errorf(
			"%w: 0x%02x", ErrInvalidProtocolControlReferenceFlags, src[2])
	}
	body := ProtocolControlACKBodyV1{
		ReferencedDataClass:      DataClass(src[1] >> 5),
		ReferencedMessageNumber:  src[1] & 0x1f,
		ReferencedSystem:         src[2]&flagSystem != 0,
		ReferencedExpansionIndex: src[2] & flagExpansion,
		FragmentOffset:           binary.LittleEndian.Uint32(src[3:7]),
		RemainingBuffer:          binary.LittleEndian.Uint16(src[7:9]),
	}
	if err := body.Validate(); err != nil {
		return ProtocolControlACKBodyV1{}, err
	}
	return body, nil
}

// ProtocolControlACKHeader returns the exact primary system Command-1 header.
// The sequence must match the acknowledged message's sequence.
func ProtocolControlACKHeader(sequence uint8) SinglePacketHeader {
	return SinglePacketHeader{
		DataClass:     DataClassCommand,
		MessageNumber: MessageNumberProtocolControl,
		System:        true,
		Sequence:      sequence,
		PayloadLength: ProtocolControlACKBodySize,
	}
}

// EncodeProtocolControlACKMessageInto writes the exact 13-byte message
// atomically with respect to dst.
func EncodeProtocolControlACKMessageInto(
	dst []byte,
	sequence uint8,
	body ProtocolControlACKBodyV1,
) error {
	if len(dst) != ProtocolControlACKMessageSize {
		return exactLengthError(
			"protocol-control ACK message", len(dst), ProtocolControlACKMessageSize)
	}
	if err := body.Validate(); err != nil {
		return err
	}
	var wire [ProtocolControlACKMessageSize]byte
	if err := EncodeSinglePacketHeaderInto(
		wire[:SinglePacketHeaderSize], ProtocolControlACKHeader(sequence)); err != nil {
		return err
	}
	if err := EncodeProtocolControlACKBodyInto(wire[SinglePacketHeaderSize:], body); err != nil {
		return err
	}
	copy(dst, wire[:])
	return nil
}

// DecodeProtocolControlACKMessage decodes one exact, uncoalesced,
// primary-device Protocol Control ACK.
func DecodeProtocolControlACKMessage(
	wire []byte,
) (sequence uint8, body ProtocolControlACKBodyV1, err error) {
	if len(wire) != ProtocolControlACKMessageSize {
		return 0, ProtocolControlACKBodyV1{}, exactLengthError(
			"protocol-control ACK message", len(wire), ProtocolControlACKMessageSize)
	}
	header, err := DecodeSinglePacketHeader(wire[:SinglePacketHeaderSize])
	if err != nil {
		return 0, ProtocolControlACKBodyV1{}, err
	}
	if header.DataClass != DataClassCommand ||
		header.MessageNumber != MessageNumberProtocolControl || !header.System ||
		header.AcknowledgementRequested || header.ExpansionIndex != 0 ||
		header.PayloadLength != ProtocolControlACKBodySize {
		return 0, ProtocolControlACKBodyV1{}, ErrInvalidProtocolControlMessage
	}
	body, err = DecodeProtocolControlACKBody(wire[SinglePacketHeaderSize:])
	if err != nil {
		return 0, ProtocolControlACKBodyV1{}, err
	}
	return header.Sequence, body, nil
}

// DecodeMetadataReliableAcknowledgement bridges an exact Protocol Control ACK
// into the identity-fenced metadata progress seam. Transport generation and
// transfer epoch are local transaction facts and never come from wire bytes.
func DecodeMetadataReliableAcknowledgement(
	wire []byte,
	transferGeneration uint64,
	transferEpoch uint64,
) (ReliableAcknowledgement, error) {
	if transferGeneration == 0 || transferEpoch == 0 {
		return ReliableAcknowledgement{}, ErrInvalidAcknowledgement
	}
	sequence, body, err := DecodeProtocolControlACKMessage(wire)
	if err != nil {
		return ReliableAcknowledgement{}, err
	}
	if body.ReferencedDataClass != DataClassCommand ||
		body.ReferencedMessageNumber != messageNumberMetadataRequest ||
		!body.ReferencedSystem || body.ReferencedExpansionIndex != 0 ||
		body.FragmentOffset > MetadataMaximumBoundLength {
		return ReliableAcknowledgement{}, ErrInvalidAcknowledgement
	}
	return ReliableAcknowledgement{
		TransferGeneration:           transferGeneration,
		TransferEpoch:                transferEpoch,
		MessageNumber:                messageNumberMetadataRequest,
		Sequence:                     sequence,
		ContiguousPayloadBytes:       uint16(body.FragmentOffset),
		ReceiverRemainingBufferBytes: body.RemainingBuffer,
	}, nil
}

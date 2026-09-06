package xboxone

import "fmt"

const (
	// SinglePacketHeaderSize is the non-extended, non-fragmented GIP header
	// size defined by MS-GIPUSB 1.0 section 2.2.10, table 12.
	SinglePacketHeaderSize = 4

	// MessageNumberDirectMotor is Command message 9.
	MessageNumberDirectMotor uint8 = 9
	// MessageNumberGamepadInput is Low Latency message 0.
	MessageNumberGamepadInput uint8 = 0
)

// DataClass is the three-bit GIP MessageType class.
type DataClass uint8

const (
	DataClassCommand DataClass = iota
	DataClassLowLatency
	DataClassStandardLatency
	DataClassAudio
)

const (
	flagFragment     byte = 1 << 7
	flagInitFragment byte = 1 << 6
	flagSystem       byte = 1 << 5
	flagAcknowledge  byte = 1 << 4
	flagReserved     byte = 1 << 3
	flagExpansion    byte = 0x07
	lengthExtended   byte = 1 << 7
)

// SinglePacketHeader is the strict four-byte GIP header subset used by the
// unfragmented gamepad input and Direct Motor messages implemented here.
// Fragment/TLO and extended-length fields require a different future type so
// callers cannot accidentally accept one as this fixed header.
type SinglePacketHeader struct {
	DataClass     DataClass
	MessageNumber uint8

	System                   bool
	AcknowledgementRequested bool
	ExpansionIndex           uint8

	Sequence      uint8
	PayloadLength uint16
}

// Validate enforces the normative fixed-header grammar and data-class MTU.
func (header SinglePacketHeader) Validate() error {
	if header.DataClass > DataClassAudio {
		return fmt.Errorf("%w: %d", ErrReservedDataClass, header.DataClass)
	}
	if header.MessageNumber > 0x1f {
		return fmt.Errorf("%w: %d", ErrInvalidMessageNumber, header.MessageNumber)
	}
	if header.ExpansionIndex > flagExpansion {
		return fmt.Errorf("%w: %d", ErrInvalidExpansionIndex, header.ExpansionIndex)
	}
	if header.Sequence == 0 {
		return ErrReservedSequence
	}

	// A four-byte header has one seven-bit payload-length field. Command,
	// Low Latency, and Standard Latency messages also have a 64-byte MTU
	// inclusive of the header. Audio's USB MTU is larger, but values above
	// 127 require an extended header and therefore do not fit this type.
	maxPayload := uint16(127)
	if header.DataClass != DataClassAudio {
		maxPayload = 64 - SinglePacketHeaderSize
	}
	if header.PayloadLength > maxPayload {
		return fmt.Errorf("%w: class=%d got=%d max=%d",
			ErrPayloadTooLarge, header.DataClass, header.PayloadLength, maxPayload)
	}
	return nil
}

func (header SinglePacketHeader) messageType() byte {
	return byte(header.DataClass)<<5 | header.MessageNumber
}

// EncodeSinglePacketHeaderInto writes exactly four bytes without allocating.
func EncodeSinglePacketHeaderInto(dst []byte, header SinglePacketHeader) error {
	if len(dst) != SinglePacketHeaderSize {
		return exactLengthError("single-packet header", len(dst), SinglePacketHeaderSize)
	}
	if err := header.Validate(); err != nil {
		return err
	}

	flags := header.ExpansionIndex
	if header.System {
		flags |= flagSystem
	}
	if header.AcknowledgementRequested {
		flags |= flagAcknowledge
	}
	dst[0] = header.messageType()
	dst[1] = flags
	dst[2] = header.Sequence
	dst[3] = byte(header.PayloadLength)
	return nil
}

// DecodeSinglePacketHeader decodes exactly four bytes and rejects reserved,
// fragmented, and extended-length encodings rather than guessing their shape.
func DecodeSinglePacketHeader(src []byte) (SinglePacketHeader, error) {
	if len(src) != SinglePacketHeaderSize {
		return SinglePacketHeader{}, exactLengthError(
			"single-packet header", len(src), SinglePacketHeaderSize)
	}

	dataClass := DataClass(src[0] >> 5)
	if dataClass > DataClassAudio {
		return SinglePacketHeader{}, fmt.Errorf("%w: %d", ErrReservedDataClass, dataClass)
	}
	flags := src[1]
	if flags&flagFragment != 0 {
		return SinglePacketHeader{}, ErrFragmentedHeader
	}
	if flags&flagInitFragment != 0 {
		return SinglePacketHeader{}, ErrInvalidInitFragment
	}
	if flags&flagReserved != 0 {
		return SinglePacketHeader{}, ErrReservedHeaderFlag
	}
	if src[2] == 0 {
		return SinglePacketHeader{}, ErrReservedSequence
	}
	if src[3]&lengthExtended != 0 {
		return SinglePacketHeader{}, ErrExtendedPayloadLength
	}

	header := SinglePacketHeader{
		DataClass:                dataClass,
		MessageNumber:            src[0] & 0x1f,
		System:                   flags&flagSystem != 0,
		AcknowledgementRequested: flags&flagAcknowledge != 0,
		ExpansionIndex:           flags & flagExpansion,
		Sequence:                 src[2],
		PayloadLength:            uint16(src[3]),
	}
	if err := header.Validate(); err != nil {
		return SinglePacketHeader{}, err
	}
	return header, nil
}

// DirectMotorHeader returns the exact message identity and length from
// MS-GIPUSB 1.0 section 3.1.5.6.1, table 56. The caller must allocate sequence
// from the Direct Motor message's unique pool.
func DirectMotorHeader(sequence uint8) SinglePacketHeader {
	return SinglePacketHeader{
		DataClass:     DataClassCommand,
		MessageNumber: MessageNumberDirectMotor,
		Sequence:      sequence,
		PayloadLength: RumbleBodySize,
	}
}

// GamepadInputHeader returns the exact message identity and length from
// MS-GIPUSB 1.0 section 3.1.5.6.1.1, table 57. The caller must allocate
// sequence from the Gamepad Input message's unique pool.
func GamepadInputHeader(sequence uint8) SinglePacketHeader {
	return SinglePacketHeader{
		DataClass:     DataClassLowLatency,
		MessageNumber: MessageNumberGamepadInput,
		Sequence:      sequence,
		PayloadLength: BaseInputBodySize,
	}
}

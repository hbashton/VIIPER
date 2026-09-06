package xboxone

import "fmt"

const (
	// GuideLEDCommandBodySize is the exact payload size from MS-GIPUSB 1.0
	// section 3.1.5.5.7, table 41.
	GuideLEDCommandBodySize = 3
	// GuideLEDCommandMessageSize includes the four-byte GIP header.
	GuideLEDCommandMessageSize = SinglePacketHeaderSize + GuideLEDCommandBodySize

	// MessageNumberLEDCommand is system Command message 10.
	MessageNumberLEDCommand uint8 = 10
	// GuideLEDCommandValue distinguishes the Guide LED form from the deprecated
	// IR LED form carried under the same message number.
	GuideLEDCommandValue byte = 0
	// GuideLEDMaximumIntensity is the normative 47-percent upper bound.
	GuideLEDMaximumIntensity byte = 47
	// GuideLEDCommandVersion is the codec contract implemented here.
	GuideLEDCommandVersion uint16 = 1
)

// GuideLEDPattern is the closed pattern domain in MS-GIPUSB 1.0 table 42.
type GuideLEDPattern uint8

const (
	GuideLEDPatternOff           GuideLEDPattern = 0x00
	GuideLEDPatternOn            GuideLEDPattern = 0x01
	GuideLEDPatternFastBlink     GuideLEDPattern = 0x02
	GuideLEDPatternSlowBlink     GuideLEDPattern = 0x03
	GuideLEDPatternChargingBlink GuideLEDPattern = 0x04
	GuideLEDPatternRampToLevel   GuideLEDPattern = 0x0d
)

func (pattern GuideLEDPattern) valid() bool {
	switch pattern {
	case GuideLEDPatternOff,
		GuideLEDPatternOn,
		GuideLEDPatternFastBlink,
		GuideLEDPatternSlowBlink,
		GuideLEDPatternChargingBlink,
		GuideLEDPatternRampToLevel:
		return true
	default:
		return false
	}
}

// GuideLEDCommandV1 preserves the protocol pattern and raw percentage. It
// does not choose a physical LED, ownership priority, animation clock, or
// brightness transfer function.
type GuideLEDCommandV1 struct {
	Pattern   GuideLEDPattern
	Intensity byte
}

// Validate rejects every reserved pattern and intensity above 47 percent.
func (command GuideLEDCommandV1) Validate() error {
	if !command.Pattern.valid() {
		return fmt.Errorf("%w: 0x%02x", ErrReservedGuideLEDPattern, command.Pattern)
	}
	if command.Intensity > GuideLEDMaximumIntensity {
		return fmt.Errorf("%w: %d", ErrGuideLEDIntensityOutOfRange, command.Intensity)
	}
	return nil
}

// EncodeGuideLEDCommandBodyInto writes the exact three-byte Guide LED body.
func EncodeGuideLEDCommandBodyInto(dst []byte, command GuideLEDCommandV1) error {
	if len(dst) != GuideLEDCommandBodySize {
		return exactLengthError(
			"guide LED command body", len(dst), GuideLEDCommandBodySize)
	}
	if err := command.Validate(); err != nil {
		return err
	}
	dst[0] = GuideLEDCommandValue
	dst[1] = byte(command.Pattern)
	dst[2] = command.Intensity
	return nil
}

// DecodeGuideLEDCommandBody decodes only the Guide LED body. The deprecated
// IR LED command has a different six-byte grammar and is rejected here.
func DecodeGuideLEDCommandBody(src []byte) (GuideLEDCommandV1, error) {
	if len(src) != GuideLEDCommandBodySize {
		return GuideLEDCommandV1{}, exactLengthError(
			"guide LED command body", len(src), GuideLEDCommandBodySize)
	}
	if src[0] != GuideLEDCommandValue {
		return GuideLEDCommandV1{}, fmt.Errorf(
			"%w: 0x%02x", ErrInvalidGuideLEDCommand, src[0])
	}
	command := GuideLEDCommandV1{
		Pattern: GuideLEDPattern(src[1]), Intensity: src[2],
	}
	if err := command.Validate(); err != nil {
		return GuideLEDCommandV1{}, err
	}
	return command, nil
}

// GuideLEDCommandHeader returns the exact primary-device system Command-10
// header. The host allocates its sequence from the global Command pool.
func GuideLEDCommandHeader(sequence uint8) SinglePacketHeader {
	return SinglePacketHeader{
		DataClass:     DataClassCommand,
		MessageNumber: MessageNumberLEDCommand,
		System:        true,
		Sequence:      sequence,
		PayloadLength: GuideLEDCommandBodySize,
	}
}

// EncodeGuideLEDCommandMessageInto writes one exact seven-byte message
// atomically with respect to dst.
func EncodeGuideLEDCommandMessageInto(
	dst []byte,
	sequence uint8,
	command GuideLEDCommandV1,
) error {
	if len(dst) != GuideLEDCommandMessageSize {
		return exactLengthError(
			"guide LED command message", len(dst), GuideLEDCommandMessageSize)
	}
	if err := command.Validate(); err != nil {
		return err
	}
	var wire [GuideLEDCommandMessageSize]byte
	if err := EncodeSinglePacketHeaderInto(
		wire[:SinglePacketHeaderSize], GuideLEDCommandHeader(sequence)); err != nil {
		return err
	}
	if err := EncodeGuideLEDCommandBodyInto(
		wire[SinglePacketHeaderSize:], command); err != nil {
		return err
	}
	copy(dst, wire[:])
	return nil
}

// DecodeGuideLEDCommandMessage decodes one exact, uncoalesced primary-device
// Guide LED Command.
func DecodeGuideLEDCommandMessage(
	wire []byte,
) (sequence uint8, command GuideLEDCommandV1, err error) {
	if len(wire) != GuideLEDCommandMessageSize {
		return 0, GuideLEDCommandV1{}, exactLengthError(
			"guide LED command message", len(wire), GuideLEDCommandMessageSize)
	}
	header, err := DecodeSinglePacketHeader(wire[:SinglePacketHeaderSize])
	if err != nil {
		return 0, GuideLEDCommandV1{}, err
	}
	if header.DataClass != DataClassCommand ||
		header.MessageNumber != MessageNumberLEDCommand || !header.System ||
		header.AcknowledgementRequested || header.ExpansionIndex != 0 ||
		header.PayloadLength != GuideLEDCommandBodySize {
		return 0, GuideLEDCommandV1{}, ErrInvalidGuideLEDMessage
	}
	command, err = DecodeGuideLEDCommandBody(wire[SinglePacketHeaderSize:])
	if err != nil {
		return 0, GuideLEDCommandV1{}, err
	}
	return header.Sequence, command, nil
}

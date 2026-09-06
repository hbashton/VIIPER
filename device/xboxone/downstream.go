package xboxone

import "fmt"

const (
	// ControllerDownstreamPacketMaximumSize is the controller data-interface
	// maximum packet size from the fixed MS-GIPUSB descriptor. A backend which
	// receives a larger aggregate must split it at proven USB packet boundaries
	// before using this decoder; this type never guesses those boundaries.
	ControllerDownstreamPacketMaximumSize = 64

	controllerDownstreamPacketMaximumMessages = ControllerDownstreamPacketMaximumSize / SinglePacketHeaderSize
)

// ControllerDownstreamMessageKind is the closed exact-message domain accepted
// by the offline controller persona. It is intentionally not a generic GIP
// transfer parser: fragmented and extended messages still require a separately
// proven reassembler. DecodeControllerDownstreamPacket can preserve several
// complete supported messages coalesced in one data packet without applying
// them to the persona.
type ControllerDownstreamMessageKind uint8

const (
	ControllerDownstreamLifecycle ControllerDownstreamMessageKind = iota + 1
	ControllerDownstreamProtocolControlACK
	ControllerDownstreamDirectMotor
	ControllerDownstreamGuideLED
)

// ControllerDownstreamMessage is a value-only classification of one exact,
// uncoalesced downstream GIP message. Only the field selected by Kind is
// meaningful. Sequence always preserves the non-zero wire sequence.
type ControllerDownstreamMessage struct {
	Kind        ControllerDownstreamMessageKind
	Sequence    uint8
	Lifecycle   ControllerHostCommand
	ACK         ProtocolControlACKBodyV1
	DirectMotor RumbleBodyV1
	GuideLED    GuideLEDCommandV1
}

// ControllerDownstreamPacket is an immutable, fixed-capacity classification
// of every supported complete message coalesced into one controller data
// packet. The zero value contains no messages and is never a successful decode.
//
// This is not a fragment reassembler, receive replay window, or execution
// transaction. DormantControllerPersonaInterruptOutAdapter preserves Message
// order through the canonical batch owner and publishes participant work only
// after the host acknowledgement reaches its USB/IP terminal outcome.
type ControllerDownstreamPacket struct {
	messages [controllerDownstreamPacketMaximumMessages]ControllerDownstreamMessage
	length   uint8
}

// Len returns the number of complete messages in the packet.
func (packet *ControllerDownstreamPacket) Len() int {
	if packet == nil {
		return 0
	}
	return int(packet.length)
}

// Message returns one classified message in wire order.
func (packet *ControllerDownstreamPacket) Message(
	index int,
) (ControllerDownstreamMessage, bool) {
	if packet == nil || index < 0 || index >= int(packet.length) {
		return ControllerDownstreamMessage{}, false
	}
	return packet.messages[index], true
}

// DecodeControllerDownstreamMessage classifies only the byte-defined message
// families implemented by this package. A syntactically valid but unsupported
// family returns ErrUnsupportedControllerPersonaHostMessage.
func DecodeControllerDownstreamMessage(wire []byte) (ControllerDownstreamMessage, error) {
	if len(wire) < SinglePacketHeaderSize {
		return ControllerDownstreamMessage{}, exactLengthError(
			"controller downstream message header", len(wire), SinglePacketHeaderSize)
	}
	header, err := DecodeSinglePacketHeader(wire[:SinglePacketHeaderSize])
	if err != nil {
		return ControllerDownstreamMessage{}, err
	}
	if len(wire) != SinglePacketHeaderSize+int(header.PayloadLength) {
		return ControllerDownstreamMessage{}, exactLengthError(
			"controller downstream message", len(wire),
			SinglePacketHeaderSize+int(header.PayloadLength))
	}

	switch {
	case header.DataClass == DataClassCommand && header.System &&
		(header.MessageNumber == messageNumberMetadataRequest ||
			header.MessageNumber == messageNumberSetDeviceState):
		command, err := DecodeControllerHostCommand(wire)
		if err != nil {
			return ControllerDownstreamMessage{}, err
		}
		return ControllerDownstreamMessage{
			Kind: ControllerDownstreamLifecycle, Sequence: command.Sequence,
			Lifecycle: command,
		}, nil

	case header.DataClass == DataClassCommand && header.System &&
		header.MessageNumber == messageNumberSecurityData:
		command, err := DecodeControllerHostCommand(wire)
		if err != nil {
			// Only the exact documented completion marker belongs to the
			// supported persona. Keep every other Security message in the
			// same unsupported-family error class as before.
			return ControllerDownstreamMessage{},
				ErrUnsupportedControllerPersonaHostMessage
		}
		return ControllerDownstreamMessage{
			Kind: ControllerDownstreamLifecycle, Sequence: command.Sequence,
			Lifecycle: command,
		}, nil

	case header.DataClass == DataClassCommand && header.System &&
		header.MessageNumber == MessageNumberProtocolControl:
		sequence, body, err := DecodeProtocolControlACKMessage(wire)
		if err != nil {
			return ControllerDownstreamMessage{}, err
		}
		return ControllerDownstreamMessage{
			Kind:     ControllerDownstreamProtocolControlACK,
			Sequence: sequence, ACK: body,
		}, nil

	case header.DataClass == DataClassCommand && !header.System &&
		header.MessageNumber == MessageNumberDirectMotor:
		sequence, body, err := DecodeDirectMotorMessage(wire)
		if err != nil {
			return ControllerDownstreamMessage{}, err
		}
		return ControllerDownstreamMessage{
			Kind:     ControllerDownstreamDirectMotor,
			Sequence: sequence, DirectMotor: body,
		}, nil

	case header.DataClass == DataClassCommand && header.System &&
		header.MessageNumber == MessageNumberLEDCommand:
		sequence, command, err := DecodeGuideLEDCommandMessage(wire)
		if err != nil {
			return ControllerDownstreamMessage{}, err
		}
		return ControllerDownstreamMessage{
			Kind:     ControllerDownstreamGuideLED,
			Sequence: sequence, GuideLED: command,
		}, nil
	}
	return ControllerDownstreamMessage{}, ErrUnsupportedControllerPersonaHostMessage
}

// DecodeControllerDownstreamPacket classifies all supported, non-fragmented
// messages in one exact controller data packet. MS-GIPUSB permits several
// small messages to be coalesced in one packet, so a packet boundary is not a
// message boundary. Each declared payload length selects the next boundary.
//
// Decoding is failure-atomic: an empty or oversized packet, a truncated later
// message, an unsupported family, or any malformed message returns the zero
// packet. The result contains typed values rather than slices into wire, so
// caller mutation after success cannot alter the classification.
func DecodeControllerDownstreamPacket(
	wire []byte,
) (ControllerDownstreamPacket, error) {
	if len(wire) == 0 || len(wire) > ControllerDownstreamPacketMaximumSize {
		return ControllerDownstreamPacket{}, fmt.Errorf(
			"%w: controller downstream packet got=%d range=1..%d",
			ErrInvalidLength, len(wire), ControllerDownstreamPacketMaximumSize)
	}

	var packet ControllerDownstreamPacket
	for offset := 0; offset < len(wire); {
		remaining := wire[offset:]
		if len(remaining) < SinglePacketHeaderSize {
			return ControllerDownstreamPacket{}, exactLengthError(
				"controller downstream packet message header",
				len(remaining), SinglePacketHeaderSize)
		}
		header, err := DecodeSinglePacketHeader(
			remaining[:SinglePacketHeaderSize])
		if err != nil {
			return ControllerDownstreamPacket{}, err
		}
		messageSize := SinglePacketHeaderSize + int(header.PayloadLength)
		if len(remaining) < messageSize {
			return ControllerDownstreamPacket{}, exactLengthError(
				"controller downstream packet message", len(remaining), messageSize)
		}
		message, err := DecodeControllerDownstreamMessage(remaining[:messageSize])
		if err != nil {
			return ControllerDownstreamPacket{}, err
		}
		// The fixed 64-byte packet and four-byte minimum header make this bound
		// unreachable after the checks above. Keep it explicit so a future
		// packet-size or header change fails closed instead of indexing past the
		// value-owned array.
		if int(packet.length) >= len(packet.messages) {
			return ControllerDownstreamPacket{}, fmt.Errorf(
				"%w: controller downstream packet message count",
				ErrInvalidLength)
		}
		packet.messages[packet.length] = message
		packet.length++
		offset += messageSize
	}
	return packet, nil
}

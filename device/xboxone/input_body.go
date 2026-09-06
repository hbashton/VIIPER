package xboxone

import (
	"encoding/binary"
	"fmt"
)

const (
	// BaseInputBodySize is the exact standard GIP Gamepad Input Report payload
	// size from MS-GIPUSB 1.0 section 3.1.5.6.1.1, table 57.
	BaseInputBodySize = 14
	// GamepadInputMessageSize includes the exact four-byte single-packet
	// envelope and the standard 14-byte payload.
	GamepadInputMessageSize = SinglePacketHeaderSize + BaseInputBodySize
	// ConsoleFunctionMapSize is the fixed function-map extension used by the
	// official Share-button gamepad profile.
	ConsoleFunctionMapSize = 18
	// ConsoleFunctionMapGamepadInputPayloadSize includes the base gamepad body
	// and its fixed function-map extension.
	ConsoleFunctionMapGamepadInputPayloadSize = BaseInputBodySize + ConsoleFunctionMapSize
	// ConsoleFunctionMapGamepadInputMessageSize includes the four-byte GIP
	// envelope and the Share-capable payload.
	ConsoleFunctionMapGamepadInputMessageSize = SinglePacketHeaderSize +
		ConsoleFunctionMapGamepadInputPayloadSize
)

// GamepadInputReportVersion is the wire-envelope contract implemented by
// GamepadInputReportV1.
const GamepadInputReportVersion uint16 = 1

// GamepadInputReportV1 keeps the protocol's Keep Alive bit out of the
// transport-neutral controller state while preserving it byte-for-byte.
type GamepadInputReportV1 struct {
	State     InputStateV1
	KeepAlive bool
}

// Validate rejects semantic state that the standard 14-byte report cannot
// represent losslessly.
func (report GamepadInputReportV1) Validate() error {
	return report.State.ValidateBaseInputBody()
}

const (
	baseButtonKeepAlive uint16 = 1 << 1
	baseButtonMenu      uint16 = 1 << (1 + iota)
	baseButtonView
	baseButtonA
	baseButtonB
	baseButtonX
	baseButtonY
	baseButtonDPadUp
	baseButtonDPadDown
	baseButtonDPadLeft
	baseButtonDPadRight
	baseButtonLeftBumper
	baseButtonRightBumper
	baseButtonLeftStick
	baseButtonRightStick
)

const baseButtonReservedMask uint16 = 0x0001

// EncodeBaseInputBodyInto writes the exact 14-byte base GIP input body without
// allocating. dst must be exactly BaseInputBodySize bytes. Guide and Share are
// rejected because Guide uses a separate status message and Share uses the
// Console Function Map extension.
func EncodeBaseInputBodyInto(dst []byte, report GamepadInputReportV1) error {
	if len(dst) != BaseInputBodySize {
		return exactLengthError("base input", len(dst), BaseInputBodySize)
	}
	if err := report.Validate(); err != nil {
		return err
	}
	state := report.State

	binary.LittleEndian.PutUint16(dst[0:2], encodeBaseButtons(report))
	binary.LittleEndian.PutUint16(dst[2:4], state.LeftTrigger)
	binary.LittleEndian.PutUint16(dst[4:6], state.RightTrigger)
	binary.LittleEndian.PutUint16(dst[6:8], uint16(state.LeftStickX))
	binary.LittleEndian.PutUint16(dst[8:10], uint16(state.LeftStickY))
	binary.LittleEndian.PutUint16(dst[10:12], uint16(state.RightStickX))
	binary.LittleEndian.PutUint16(dst[12:14], uint16(state.RightStickY))
	return nil
}

// DecodeBaseInputBody decodes an exact 14-byte base GIP input body without
// allocating on the success path. Reserved button bits fail closed; all four
// independently encoded D-pad bits are preserved.
func DecodeBaseInputBody(src []byte) (GamepadInputReportV1, error) {
	if len(src) != BaseInputBodySize {
		return GamepadInputReportV1{}, exactLengthError("base input", len(src), BaseInputBodySize)
	}

	buttons := binary.LittleEndian.Uint16(src[0:2])
	if buttons&baseButtonReservedMask != 0 {
		return GamepadInputReportV1{}, fmt.Errorf("%w: 0x%04x", ErrReservedButtonBits,
			buttons&baseButtonReservedMask)
	}

	report := GamepadInputReportV1{
		State:     decodeBaseButtons(buttons),
		KeepAlive: buttons&baseButtonKeepAlive != 0,
	}
	state := &report.State
	state.LeftTrigger = binary.LittleEndian.Uint16(src[2:4])
	state.RightTrigger = binary.LittleEndian.Uint16(src[4:6])
	state.LeftStickX = int16(binary.LittleEndian.Uint16(src[6:8]))
	state.LeftStickY = int16(binary.LittleEndian.Uint16(src[8:10]))
	state.RightStickX = int16(binary.LittleEndian.Uint16(src[10:12]))
	state.RightStickY = int16(binary.LittleEndian.Uint16(src[12:14]))
	if err := report.Validate(); err != nil {
		return GamepadInputReportV1{}, err
	}
	return report, nil
}

// EncodeGamepadInputMessageInto writes one exact, uncoalesced standard
// Gamepad Input Report. Validation is atomic with respect to dst: an error
// leaves every destination byte unchanged.
//
// sequence belongs to the Gamepad Input message's unique sequence pool. This
// codec validates the non-zero wire value but deliberately does not allocate
// or commit that caller-owned transaction.
func EncodeGamepadInputMessageInto(
	dst []byte,
	sequence uint8,
	report GamepadInputReportV1,
) error {
	if len(dst) != GamepadInputMessageSize {
		return exactLengthError(
			"gamepad input message", len(dst), GamepadInputMessageSize)
	}
	if err := report.Validate(); err != nil {
		return err
	}
	var wire [GamepadInputMessageSize]byte
	if err := EncodeSinglePacketHeaderInto(
		wire[:SinglePacketHeaderSize], GamepadInputHeader(sequence)); err != nil {
		return err
	}
	if err := EncodeBaseInputBodyInto(
		wire[SinglePacketHeaderSize:], report); err != nil {
		return err
	}
	copy(dst, wire[:])
	return nil
}

// DecodeGamepadInputMessage decodes only the exact standard 18-byte Gamepad
// Input Report form. A transfer containing coalesced messages must be split by
// a separately proven transport parser before it reaches this boundary.
func DecodeGamepadInputMessage(
	wire []byte,
) (sequence uint8, report GamepadInputReportV1, err error) {
	if len(wire) != GamepadInputMessageSize {
		return 0, GamepadInputReportV1{}, exactLengthError(
			"gamepad input message", len(wire), GamepadInputMessageSize)
	}
	header, err := DecodeSinglePacketHeader(wire[:SinglePacketHeaderSize])
	if err != nil {
		return 0, GamepadInputReportV1{}, err
	}
	if header.DataClass != DataClassLowLatency ||
		header.MessageNumber != MessageNumberGamepadInput || header.System ||
		header.AcknowledgementRequested || header.ExpansionIndex != 0 ||
		header.PayloadLength != BaseInputBodySize {
		return 0, GamepadInputReportV1{}, ErrInvalidGamepadInputMessage
	}
	report, err = DecodeBaseInputBody(wire[SinglePacketHeaderSize:])
	if err != nil {
		return 0, GamepadInputReportV1{}, err
	}
	return header.Sequence, report, nil
}

// EncodeConsoleFunctionMapGamepadInputMessageInto writes the exact official
// Share-capable 36-byte input message. Function ID 1 occupies only the first
// extension byte while Share is held; all other function slots remain zero.
// Guide is still rejected because it travels in system Command 7.
func EncodeConsoleFunctionMapGamepadInputMessageInto(
	dst []byte,
	sequence uint8,
	report GamepadInputReportV1,
) error {
	if len(dst) != ConsoleFunctionMapGamepadInputMessageSize {
		return exactLengthError("Console Function Map gamepad input message",
			len(dst), ConsoleFunctionMapGamepadInputMessageSize)
	}
	if err := report.State.Validate(); err != nil {
		return err
	}
	if report.State.Guide {
		return ErrGuideRequiresStatusMessage
	}
	base := report
	base.State.Share = false
	var wire [ConsoleFunctionMapGamepadInputMessageSize]byte
	header := SinglePacketHeader{
		DataClass: DataClassLowLatency, MessageNumber: MessageNumberGamepadInput,
		Sequence:      sequence,
		PayloadLength: ConsoleFunctionMapGamepadInputPayloadSize,
	}
	if err := EncodeSinglePacketHeaderInto(wire[:SinglePacketHeaderSize], header); err != nil {
		return err
	}
	if err := EncodeBaseInputBodyInto(
		wire[SinglePacketHeaderSize:SinglePacketHeaderSize+BaseInputBodySize],
		base); err != nil {
		return err
	}
	if report.State.Share {
		wire[SinglePacketHeaderSize+BaseInputBodySize] = 1
	}
	copy(dst, wire[:])
	return nil
}

// DecodeConsoleFunctionMapGamepadInputMessage decodes only the canonical
// Share-capable form. Unknown function IDs and non-zero unused slots fail
// closed instead of becoming phantom application controls.
func DecodeConsoleFunctionMapGamepadInputMessage(
	wire []byte,
) (sequence uint8, report GamepadInputReportV1, err error) {
	if len(wire) != ConsoleFunctionMapGamepadInputMessageSize {
		return 0, GamepadInputReportV1{}, exactLengthError(
			"Console Function Map gamepad input message", len(wire),
			ConsoleFunctionMapGamepadInputMessageSize)
	}
	header, err := DecodeSinglePacketHeader(wire[:SinglePacketHeaderSize])
	if err != nil {
		return 0, GamepadInputReportV1{}, err
	}
	if header.DataClass != DataClassLowLatency ||
		header.MessageNumber != MessageNumberGamepadInput || header.System ||
		header.AcknowledgementRequested || header.ExpansionIndex != 0 ||
		header.PayloadLength != ConsoleFunctionMapGamepadInputPayloadSize {
		return 0, GamepadInputReportV1{}, ErrInvalidGamepadInputMessage
	}
	report, err = DecodeBaseInputBody(
		wire[SinglePacketHeaderSize : SinglePacketHeaderSize+BaseInputBodySize])
	if err != nil {
		return 0, GamepadInputReportV1{}, err
	}
	extension := wire[SinglePacketHeaderSize+BaseInputBodySize:]
	if extension[0] > 1 || !allZero(extension[1:]) {
		return 0, GamepadInputReportV1{}, ErrInvalidConsoleFunctionMap
	}
	report.State.Share = extension[0] == 1
	return header.Sequence, report, nil
}

func encodeBaseButtons(report GamepadInputReportV1) uint16 {
	state := report.State
	var buttons uint16
	set := func(value bool, mask uint16) {
		if value {
			buttons |= mask
		}
	}
	set(report.KeepAlive, baseButtonKeepAlive)
	set(state.Menu, baseButtonMenu)
	set(state.View, baseButtonView)
	set(state.A, baseButtonA)
	set(state.B, baseButtonB)
	set(state.X, baseButtonX)
	set(state.Y, baseButtonY)
	set(state.DPadUp, baseButtonDPadUp)
	set(state.DPadDown, baseButtonDPadDown)
	set(state.DPadLeft, baseButtonDPadLeft)
	set(state.DPadRight, baseButtonDPadRight)
	set(state.LeftBumper, baseButtonLeftBumper)
	set(state.RightBumper, baseButtonRightBumper)
	set(state.LeftStickButton, baseButtonLeftStick)
	set(state.RightStickButton, baseButtonRightStick)
	return buttons
}

func decodeBaseButtons(buttons uint16) InputStateV1 {
	return InputStateV1{
		Menu:             buttons&baseButtonMenu != 0,
		View:             buttons&baseButtonView != 0,
		A:                buttons&baseButtonA != 0,
		B:                buttons&baseButtonB != 0,
		X:                buttons&baseButtonX != 0,
		Y:                buttons&baseButtonY != 0,
		DPadUp:           buttons&baseButtonDPadUp != 0,
		DPadDown:         buttons&baseButtonDPadDown != 0,
		DPadLeft:         buttons&baseButtonDPadLeft != 0,
		DPadRight:        buttons&baseButtonDPadRight != 0,
		LeftBumper:       buttons&baseButtonLeftBumper != 0,
		RightBumper:      buttons&baseButtonRightBumper != 0,
		LeftStickButton:  buttons&baseButtonLeftStick != 0,
		RightStickButton: buttons&baseButtonRightStick != 0,
	}
}

func exactLengthError(body string, got, want int) error {
	return fmt.Errorf("%w: %s got %d want %d", ErrInvalidLength, body, got, want)
}

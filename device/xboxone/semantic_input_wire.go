package xboxone

import (
	"encoding/binary"
	"fmt"
)

const (
	// SemanticInputWireVersion is the private DS4Windows-to-VIIPER contract.
	// It is deliberately distinct from the public GIP wire protocol.
	SemanticInputWireVersion uint16 = 1
	SemanticInputWireSize           = 24
)

const (
	semanticButtonMenu uint32 = 1 << iota
	semanticButtonView
	semanticButtonA
	semanticButtonB
	semanticButtonX
	semanticButtonY
	semanticButtonDPadUp
	semanticButtonDPadDown
	semanticButtonDPadLeft
	semanticButtonDPadRight
	semanticButtonLeftBumper
	semanticButtonRightBumper
	semanticButtonLeftStick
	semanticButtonRightStick
	semanticButtonGuide
	semanticButtonShare
)

const semanticInputValidButtons uint32 = (1 << 16) - 1

// EncodeSemanticInputWireV1Into writes the exact private, versioned semantic
// frame used between DS4Windows and VIIPER. Validation is atomic with respect
// to dst. GIP sequence IDs, Keep Alive, Guide status, Share extensions, and
// endpoint lifecycle do not belong to this frame and remain VIIPER-owned.
func EncodeSemanticInputWireV1Into(dst []byte, state InputStateV1) error {
	if len(dst) != SemanticInputWireSize {
		return exactLengthError("broker semantic input", len(dst), SemanticInputWireSize)
	}
	if err := state.Validate(); err != nil {
		return err
	}

	var wire [SemanticInputWireSize]byte
	binary.LittleEndian.PutUint16(wire[0:2], SemanticInputWireVersion)
	binary.LittleEndian.PutUint16(wire[2:4], SemanticInputWireSize)
	binary.LittleEndian.PutUint32(wire[4:8], encodeSemanticInputButtons(state))
	binary.LittleEndian.PutUint16(wire[8:10], state.LeftTrigger)
	binary.LittleEndian.PutUint16(wire[10:12], state.RightTrigger)
	binary.LittleEndian.PutUint16(wire[12:14], uint16(state.LeftStickX))
	binary.LittleEndian.PutUint16(wire[14:16], uint16(state.LeftStickY))
	binary.LittleEndian.PutUint16(wire[16:18], uint16(state.RightStickX))
	binary.LittleEndian.PutUint16(wire[18:20], uint16(state.RightStickY))
	copy(dst, wire[:])
	return nil
}

// DecodeSemanticInputWireV1 decodes only the exact private semantic frame.
// Reserved fields fail closed so a future contract cannot be misinterpreted
// as v1. Guide and Share remain explicit semantic controls even though their
// eventual GIP codecs are gated independently.
func DecodeSemanticInputWireV1(src []byte) (InputStateV1, error) {
	if len(src) != SemanticInputWireSize {
		return InputStateV1{}, exactLengthError(
			"broker semantic input", len(src), SemanticInputWireSize)
	}
	version := binary.LittleEndian.Uint16(src[0:2])
	embeddedSize := binary.LittleEndian.Uint16(src[2:4])
	if version != SemanticInputWireVersion || embeddedSize != SemanticInputWireSize {
		return InputStateV1{}, fmt.Errorf(
			"%w: version=%d size=%d", ErrInvalidSemanticInputContract,
			version, embeddedSize)
	}
	buttons := binary.LittleEndian.Uint32(src[4:8])
	if buttons&^semanticInputValidButtons != 0 {
		return InputStateV1{}, fmt.Errorf(
			"%w: reserved buttons=0x%08x", ErrInvalidSemanticInputContract,
			buttons&^semanticInputValidButtons)
	}
	if binary.LittleEndian.Uint32(src[20:24]) != 0 {
		return InputStateV1{}, fmt.Errorf(
			"%w: reserved tail=0x%08x", ErrInvalidSemanticInputContract,
			binary.LittleEndian.Uint32(src[20:24]))
	}

	state := decodeSemanticInputButtons(buttons)
	state.LeftTrigger = binary.LittleEndian.Uint16(src[8:10])
	state.RightTrigger = binary.LittleEndian.Uint16(src[10:12])
	state.LeftStickX = int16(binary.LittleEndian.Uint16(src[12:14]))
	state.LeftStickY = int16(binary.LittleEndian.Uint16(src[14:16]))
	state.RightStickX = int16(binary.LittleEndian.Uint16(src[16:18]))
	state.RightStickY = int16(binary.LittleEndian.Uint16(src[18:20]))
	if err := state.Validate(); err != nil {
		return InputStateV1{}, err
	}
	return state, nil
}

func encodeSemanticInputButtons(state InputStateV1) uint32 {
	var buttons uint32
	set := func(value bool, mask uint32) {
		if value {
			buttons |= mask
		}
	}
	set(state.Menu, semanticButtonMenu)
	set(state.View, semanticButtonView)
	set(state.A, semanticButtonA)
	set(state.B, semanticButtonB)
	set(state.X, semanticButtonX)
	set(state.Y, semanticButtonY)
	set(state.DPadUp, semanticButtonDPadUp)
	set(state.DPadDown, semanticButtonDPadDown)
	set(state.DPadLeft, semanticButtonDPadLeft)
	set(state.DPadRight, semanticButtonDPadRight)
	set(state.LeftBumper, semanticButtonLeftBumper)
	set(state.RightBumper, semanticButtonRightBumper)
	set(state.LeftStickButton, semanticButtonLeftStick)
	set(state.RightStickButton, semanticButtonRightStick)
	set(state.Guide, semanticButtonGuide)
	set(state.Share, semanticButtonShare)
	return buttons
}

func decodeSemanticInputButtons(buttons uint32) InputStateV1 {
	return InputStateV1{
		Menu:             buttons&semanticButtonMenu != 0,
		View:             buttons&semanticButtonView != 0,
		A:                buttons&semanticButtonA != 0,
		B:                buttons&semanticButtonB != 0,
		X:                buttons&semanticButtonX != 0,
		Y:                buttons&semanticButtonY != 0,
		DPadUp:           buttons&semanticButtonDPadUp != 0,
		DPadDown:         buttons&semanticButtonDPadDown != 0,
		DPadLeft:         buttons&semanticButtonDPadLeft != 0,
		DPadRight:        buttons&semanticButtonDPadRight != 0,
		LeftBumper:       buttons&semanticButtonLeftBumper != 0,
		RightBumper:      buttons&semanticButtonRightBumper != 0,
		LeftStickButton:  buttons&semanticButtonLeftStick != 0,
		RightStickButton: buttons&semanticButtonRightStick != 0,
		Guide:            buttons&semanticButtonGuide != 0,
		Share:            buttons&semanticButtonShare != 0,
	}
}

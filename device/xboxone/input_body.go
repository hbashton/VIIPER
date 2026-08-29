package xboxone

import (
	"encoding/binary"
	"fmt"
)

const (
	// BaseInputBodySize is the exact size of the pinned GIP gamepad input body.
	BaseInputBodySize = 14

	// GuideVirtualKeyBodySize is the exact size of a Guide virtual-key body.
	GuideVirtualKeyBodySize = 2
	// CommandVirtualKey is the GIP command carrying the separate Guide event.
	CommandVirtualKey byte = 0x07
	// GuideVirtualKeyCode is the key value observed for the Guide event.
	GuideVirtualKeyCode byte = 0x5b
)

const (
	baseButtonMenu uint16 = 1 << (2 + iota)
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

const baseButtonReservedMask uint16 = 0x0003

// EncodeBaseInputBodyInto writes the exact 14-byte base GIP input body without
// allocating. dst must be exactly BaseInputBodySize bytes. Guide and Share are
// rejected because they use separate or model-dependent messages.
func EncodeBaseInputBodyInto(dst []byte, state InputStateV1) error {
	if len(dst) != BaseInputBodySize {
		return exactLengthError("base input", len(dst), BaseInputBodySize)
	}
	if err := state.ValidateBaseInputBody(); err != nil {
		return err
	}

	binary.LittleEndian.PutUint16(dst[0:2], encodeBaseButtons(state))
	binary.LittleEndian.PutUint16(dst[2:4], state.LeftTrigger)
	binary.LittleEndian.PutUint16(dst[4:6], state.RightTrigger)
	binary.LittleEndian.PutUint16(dst[6:8], uint16(state.LeftStickX))
	binary.LittleEndian.PutUint16(dst[8:10], uint16(state.LeftStickY))
	binary.LittleEndian.PutUint16(dst[10:12], uint16(state.RightStickX))
	binary.LittleEndian.PutUint16(dst[12:14], uint16(state.RightStickY))
	return nil
}

// DecodeBaseInputBody decodes an exact 14-byte base GIP input body without
// allocating on the success path. Reserved button bits and impossible D-pad
// combinations fail closed.
func DecodeBaseInputBody(src []byte) (InputStateV1, error) {
	if len(src) != BaseInputBodySize {
		return InputStateV1{}, exactLengthError("base input", len(src), BaseInputBodySize)
	}

	buttons := binary.LittleEndian.Uint16(src[0:2])
	if buttons&baseButtonReservedMask != 0 {
		return InputStateV1{}, fmt.Errorf("%w: 0x%04x", ErrReservedButtonBits,
			buttons&baseButtonReservedMask)
	}

	state := decodeBaseButtons(buttons)
	state.LeftTrigger = binary.LittleEndian.Uint16(src[2:4])
	state.RightTrigger = binary.LittleEndian.Uint16(src[4:6])
	state.LeftStickX = int16(binary.LittleEndian.Uint16(src[6:8]))
	state.LeftStickY = int16(binary.LittleEndian.Uint16(src[8:10]))
	state.RightStickX = int16(binary.LittleEndian.Uint16(src[10:12]))
	state.RightStickY = int16(binary.LittleEndian.Uint16(src[12:14]))
	if err := state.Validate(); err != nil {
		return InputStateV1{}, err
	}
	return state, nil
}

// GuideEventV1 represents the Guide state carried outside the base input body.
type GuideEventV1 struct {
	Down bool
}

// EncodeGuideVirtualKeyBodyInto writes the exact two-byte Guide virtual-key
// body without allocating.
func EncodeGuideVirtualKeyBodyInto(dst []byte, event GuideEventV1) error {
	if len(dst) != GuideVirtualKeyBodySize {
		return exactLengthError("guide virtual-key", len(dst), GuideVirtualKeyBodySize)
	}
	dst[0] = 0
	if event.Down {
		dst[0] = 1
	}
	dst[1] = GuideVirtualKeyCode
	return nil
}

// DecodeGuideVirtualKeyBody decodes a strict Guide virtual-key body. Values
// other than zero or one are rejected instead of being coerced to true.
func DecodeGuideVirtualKeyBody(src []byte) (GuideEventV1, error) {
	if len(src) != GuideVirtualKeyBodySize {
		return GuideEventV1{}, exactLengthError("guide virtual-key", len(src),
			GuideVirtualKeyBodySize)
	}
	if src[0] > 1 || src[1] != GuideVirtualKeyCode {
		return GuideEventV1{}, fmt.Errorf("%w: down=0x%02x key=0x%02x",
			ErrInvalidGuideBody, src[0], src[1])
	}
	return GuideEventV1{Down: src[0] == 1}, nil
}

func encodeBaseButtons(state InputStateV1) uint16 {
	var buttons uint16
	set := func(value bool, mask uint16) {
		if value {
			buttons |= mask
		}
	}
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

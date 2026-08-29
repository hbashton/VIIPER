package xboxone

import "fmt"

const (
	// RumbleBodySize is the exact size of the pinned four-actuator GIP body.
	RumbleBodySize = 9

	// Pinned stop timing values are the values used by xone's explicit startup
	// stop. They are intentionally not generalized into effect timing policy.
	PinnedStopDuration byte = 0xff
	PinnedStopRepeat   byte = 0xeb
)

// MotorMask selects which magnitudes in RumbleBodyV1 are active.
type MotorMask uint8

const (
	// MotorHighFrequency is the right body motor (wire mask bit 0).
	MotorHighFrequency MotorMask = 1 << iota
	// MotorLowFrequency is the left body motor (wire mask bit 1).
	MotorLowFrequency
	MotorRightTrigger
	MotorLeftTrigger

	MotorAll = MotorHighFrequency | MotorLowFrequency |
		MotorRightTrigger | MotorLeftTrigger
)

// RumbleBodyV1 is the strict semantic representation of the pinned nine-byte
// four-actuator body. Magnitudes remain uint8 because the valid device range is
// not assumed before Windows captures. Disabled channels must be zero.
type RumbleBodyV1 struct {
	// Reserved maps to the first wire byte. It must remain zero until its
	// semantics are established by owned captures.
	Reserved byte
	Enabled  MotorMask

	LeftTrigger   byte
	RightTrigger  byte
	LowFrequency  byte
	HighFrequency byte

	Duration byte
	Delay    byte
	Repeat   byte
}

// Validate enforces the known motor mask and VIIPER's no-stale-magnitude safety
// invariant. The latter is a local fail-closed rule, not a claim about what all
// physical Xbox devices accept.
func (body RumbleBodyV1) Validate() error {
	if body.Reserved != 0 {
		return fmt.Errorf("%w: 0x%02x", ErrReservedRumbleField, body.Reserved)
	}
	if body.Enabled&^MotorAll != 0 {
		return fmt.Errorf("%w: 0x%02x", ErrInvalidMotorMask, body.Enabled)
	}
	if body.LeftTrigger != 0 && body.Enabled&MotorLeftTrigger == 0 {
		return disabledMotorMagnitudeError("left trigger")
	}
	if body.RightTrigger != 0 && body.Enabled&MotorRightTrigger == 0 {
		return disabledMotorMagnitudeError("right trigger")
	}
	if body.LowFrequency != 0 && body.Enabled&MotorLowFrequency == 0 {
		return disabledMotorMagnitudeError("low frequency")
	}
	if body.HighFrequency != 0 && body.Enabled&MotorHighFrequency == 0 {
		return disabledMotorMagnitudeError("high frequency")
	}
	return nil
}

// IsExplicitStop reports whether all four motors are selected with zero
// magnitude. A zero enable mask is not treated as a stop because it need not
// command an already-running actuator to change state.
func (body RumbleBodyV1) IsExplicitStop() bool {
	return body.Enabled == MotorAll && body.LeftTrigger == 0 &&
		body.RightTrigger == 0 && body.LowFrequency == 0 &&
		body.HighFrequency == 0
}

// NewPinnedStopRumbleBody returns the explicit all-motor stop body observed in
// the pinned xone reference. It does not imply a general duration/repeat policy.
func NewPinnedStopRumbleBody() RumbleBodyV1 {
	return RumbleBodyV1{
		Enabled:  MotorAll,
		Duration: PinnedStopDuration,
		Repeat:   PinnedStopRepeat,
	}
}

// EncodeRumbleBodyInto writes the exact nine-byte four-actuator body without
// allocating. dst must be exactly RumbleBodySize bytes.
func EncodeRumbleBodyInto(dst []byte, body RumbleBodyV1) error {
	if len(dst) != RumbleBodySize {
		return exactLengthError("rumble", len(dst), RumbleBodySize)
	}
	if err := body.Validate(); err != nil {
		return err
	}

	dst[0] = body.Reserved
	dst[1] = byte(body.Enabled)
	dst[2] = body.LeftTrigger
	dst[3] = body.RightTrigger
	dst[4] = body.LowFrequency
	dst[5] = body.HighFrequency
	dst[6] = body.Duration
	dst[7] = body.Delay
	dst[8] = body.Repeat
	return nil
}

// DecodeRumbleBody decodes an exact nine-byte body and applies the same strict
// validation as the encoder.
func DecodeRumbleBody(src []byte) (RumbleBodyV1, error) {
	if len(src) != RumbleBodySize {
		return RumbleBodyV1{}, exactLengthError("rumble", len(src), RumbleBodySize)
	}
	body := RumbleBodyV1{
		Reserved:      src[0],
		Enabled:       MotorMask(src[1]),
		LeftTrigger:   src[2],
		RightTrigger:  src[3],
		LowFrequency:  src[4],
		HighFrequency: src[5],
		Duration:      src[6],
		Delay:         src[7],
		Repeat:        src[8],
	}
	if err := body.Validate(); err != nil {
		return RumbleBodyV1{}, err
	}
	return body, nil
}

func disabledMotorMagnitudeError(name string) error {
	return fmt.Errorf("%w: %s", ErrDisabledMotorMagnitude, name)
}

package xboxone

import "fmt"

const (
	// RumbleBodyVersion is the semantic contract implemented by RumbleBodyV1.
	RumbleBodyVersion uint16 = 1

	// RumbleBodySize is the exact Direct Motor Command payload size from
	// MS-GIPUSB 1.0 section 3.1.5.6.1, table 56.
	RumbleBodySize = 9
	// DirectMotorMessageSize includes the exact four-byte single-packet
	// envelope and the nine-byte payload.
	DirectMotorMessageSize = SinglePacketHeaderSize + RumbleBodySize

	// DirectMotorCommandValue is the fixed first payload byte.
	DirectMotorCommandValue byte = 0
	// DirectMotorMaximumLevel is the normative 100-percent upper bound for
	// each of the four motor level fields.
	DirectMotorMaximumLevel byte = 100
)

// MotorMask selects which magnitudes in RumbleBodyV1 are active.
type MotorMask uint8

const (
	// MotorRightVibration is wire mask bit 0.
	MotorRightVibration MotorMask = 1 << iota
	// MotorLeftVibration is wire mask bit 1.
	MotorLeftVibration
	// MotorRightImpulse is the right-trigger actuator at wire mask bit 2.
	MotorRightImpulse
	// MotorLeftImpulse is the left-trigger actuator at wire mask bit 3.
	MotorLeftImpulse

	MotorAll = MotorRightVibration | MotorLeftVibration |
		MotorRightImpulse | MotorLeftImpulse
)

// RumbleBodyV1 is the strict semantic representation of the nine-byte Direct
// Motor Command payload. Level values are percentages in the inclusive range
// 0..100. Duration and Delay are 10 ms units; Repeat is a repeat count.
type RumbleBodyV1 struct {
	Enabled MotorMask

	LeftImpulse    byte
	RightImpulse   byte
	LeftVibration  byte
	RightVibration byte

	Duration byte
	Delay    byte
	Repeat   byte
}

// Validate enforces the reserved motor-bitmap bits and the normative
// percentage range. A level on a motor whose bitmap bit is clear is not
// rejected: MS-GIPUSB does not make such a combination malformed. Duration
// zero cancels all motors and explicitly makes every level ignored.
func (body RumbleBodyV1) Validate() error {
	if body.Enabled&^MotorAll != 0 {
		return fmt.Errorf("%w: 0x%02x", ErrInvalidMotorMask, body.Enabled)
	}
	levels := [...]struct {
		name  string
		value byte
	}{
		{name: "left impulse", value: body.LeftImpulse},
		{name: "right impulse", value: body.RightImpulse},
		{name: "left vibration", value: body.LeftVibration},
		{name: "right vibration", value: body.RightVibration},
	}
	for _, level := range levels {
		if level.value > DirectMotorMaximumLevel {
			return fmt.Errorf("%w: %s=%d", ErrMotorLevelOutOfRange,
				level.name, level.value)
		}
	}
	return nil
}

// IsCancellation reports the normative MS-GIPUSB cancellation condition: the
// complete body validates and Duration is zero. Levels, Delay, and Repeat do
// not change that wire-level cancellation classification.
func (body RumbleBodyV1) IsCancellation() bool {
	return body.Validate() == nil && body.Duration == 0
}

// IsCanonicalImmediateStop reports VIIPER's narrower locally generated stop
// form. It must never be used in place of IsCancellation when interpreting a
// host command.
func (body RumbleBodyV1) IsCanonicalImmediateStop() bool {
	return body.IsCancellation() && body.Delay == 0 && body.Repeat == 0
}

// NewStopRumbleBody returns the canonical zero-valued cancellation body.
func NewStopRumbleBody() RumbleBodyV1 {
	return RumbleBodyV1{}
}

// EncodeRumbleBodyInto writes the exact nine-byte four-actuator body without
// allocating. dst must be exactly RumbleBodySize bytes.
func EncodeRumbleBodyInto(dst []byte, body RumbleBodyV1) error {
	if len(dst) != RumbleBodySize {
		return exactLengthError("direct motor", len(dst), RumbleBodySize)
	}
	if err := body.Validate(); err != nil {
		return err
	}

	dst[0] = DirectMotorCommandValue
	dst[1] = byte(body.Enabled)
	dst[2] = body.LeftImpulse
	dst[3] = body.RightImpulse
	dst[4] = body.LeftVibration
	dst[5] = body.RightVibration
	dst[6] = body.Duration
	dst[7] = body.Delay
	dst[8] = body.Repeat
	return nil
}

// DecodeRumbleBody decodes an exact nine-byte body and applies the same strict
// validation as the encoder.
func DecodeRumbleBody(src []byte) (RumbleBodyV1, error) {
	if len(src) != RumbleBodySize {
		return RumbleBodyV1{}, exactLengthError("direct motor", len(src), RumbleBodySize)
	}
	if src[0] != DirectMotorCommandValue {
		return RumbleBodyV1{}, fmt.Errorf("%w: 0x%02x",
			ErrInvalidDirectMotorCommand, src[0])
	}
	body := RumbleBodyV1{
		Enabled:        MotorMask(src[1]),
		LeftImpulse:    src[2],
		RightImpulse:   src[3],
		LeftVibration:  src[4],
		RightVibration: src[5],
		Duration:       src[6],
		Delay:          src[7],
		Repeat:         src[8],
	}
	if err := body.Validate(); err != nil {
		return RumbleBodyV1{}, err
	}
	return body, nil
}

// EncodeDirectMotorMessageInto writes one exact, uncoalesced Direct Motor
// Command. Validation is atomic with respect to dst: an error leaves every
// destination byte unchanged.
//
// sequence belongs to the Direct Motor message's unique sequence pool. This
// codec validates the non-zero wire value but deliberately does not allocate
// or commit that caller-owned transaction.
func EncodeDirectMotorMessageInto(
	dst []byte,
	sequence uint8,
	body RumbleBodyV1,
) error {
	if len(dst) != DirectMotorMessageSize {
		return exactLengthError(
			"direct motor message", len(dst), DirectMotorMessageSize)
	}
	if err := body.Validate(); err != nil {
		return err
	}
	var wire [DirectMotorMessageSize]byte
	if err := EncodeSinglePacketHeaderInto(
		wire[:SinglePacketHeaderSize], DirectMotorHeader(sequence)); err != nil {
		return err
	}
	if err := EncodeRumbleBodyInto(
		wire[SinglePacketHeaderSize:], body); err != nil {
		return err
	}
	copy(dst, wire[:])
	return nil
}

// DecodeDirectMotorMessage decodes only the exact 13-byte Direct Motor
// Command form. A transfer containing coalesced messages must be split by a
// separately proven transport parser before it reaches this boundary.
func DecodeDirectMotorMessage(
	wire []byte,
) (sequence uint8, body RumbleBodyV1, err error) {
	if len(wire) != DirectMotorMessageSize {
		return 0, RumbleBodyV1{}, exactLengthError(
			"direct motor message", len(wire), DirectMotorMessageSize)
	}
	header, err := DecodeSinglePacketHeader(wire[:SinglePacketHeaderSize])
	if err != nil {
		return 0, RumbleBodyV1{}, err
	}
	if header.DataClass != DataClassCommand ||
		header.MessageNumber != MessageNumberDirectMotor || header.System ||
		header.AcknowledgementRequested || header.ExpansionIndex != 0 ||
		header.PayloadLength != RumbleBodySize {
		return 0, RumbleBodyV1{}, ErrInvalidDirectMotorMessage
	}
	body, err = DecodeRumbleBody(wire[SinglePacketHeaderSize:])
	if err != nil {
		return 0, RumbleBodyV1{}, err
	}
	return header.Sequence, body, nil
}

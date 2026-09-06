package xboxone

import "fmt"

const (
	// ExtendedStatusNoEventsBodySize is the exact payload size from MS-GIPUSB
	// 1.0 section 3.1.5.5.2.2, table 29 when Events Present is clear.
	ExtendedStatusNoEventsBodySize = 4
	// ExtendedStatusNoEventsMessageSize includes the four-byte GIP header.
	ExtendedStatusNoEventsMessageSize = SinglePacketHeaderSize + ExtendedStatusNoEventsBodySize

	// MessageNumberStatusDevice is system Command message 3.
	MessageNumberStatusDevice uint8 = 3
	// ExtendedStatusNoEventsVersion is the codec contract implemented here.
	ExtendedStatusNoEventsVersion uint16 = 1
)

// StatusPowerLevel is Status byte bits 7:6.
type StatusPowerLevel uint8

const (
	StatusPoweringOff StatusPowerLevel = 0
	StatusFullPower   StatusPowerLevel = 2
)

// StatusChargeState is Status byte bits 5:4.
type StatusChargeState uint8

const (
	StatusNotCharging StatusChargeState = iota
	StatusCharging
	StatusChargeError
)

// StatusBatteryType is Status byte bits 3:2.
type StatusBatteryType uint8

const (
	StatusBatteryAbsent StatusBatteryType = iota
	StatusBatteryStandard
	StatusBatteryRechargeable
)

// StatusBatteryLevel is Status byte bits 1:0.
type StatusBatteryLevel uint8

const (
	StatusBatteryCriticallyLow StatusBatteryLevel = iota
	StatusBatteryLow
	StatusBatteryMedium
	StatusBatteryFull
)

// ExtendedStatusNoEventsBodyV1 is the four-byte Extended Status payload with
// Events Present clear. It deliberately cannot represent event records: those
// change the payload grammar to a 35-through-55-byte form.
//
// DeviceActive is preserved as a wire fact. MS-GIPUSB says devices without IR
// LEDs should clear it; that capability policy belongs to the profile that
// creates the body.
type ExtendedStatusNoEventsBodyV1 struct {
	PowerLevel   StatusPowerLevel
	ChargeState  StatusChargeState
	BatteryType  StatusBatteryType
	BatteryLevel StatusBatteryLevel
	DeviceActive bool
}

// Validate rejects the deprecated/reserved power value and all reserved
// charge, battery-type, and width values. A zero composite Status byte remains
// representable because MS-GIPUSB explicitly permits it for a powering-off
// USB-only device without batteries.
func (body ExtendedStatusNoEventsBodyV1) Validate() error {
	if body.PowerLevel != StatusPoweringOff && body.PowerLevel != StatusFullPower {
		return fmt.Errorf("%w: power=%d", ErrReservedStatusValue, body.PowerLevel)
	}
	if body.ChargeState > StatusChargeError {
		return fmt.Errorf("%w: charge=%d", ErrReservedStatusValue, body.ChargeState)
	}
	if body.BatteryType > StatusBatteryRechargeable {
		return fmt.Errorf("%w: battery-type=%d", ErrReservedStatusValue, body.BatteryType)
	}
	if body.BatteryLevel > StatusBatteryFull {
		return fmt.Errorf("%w: battery-level=%d", ErrReservedStatusValue, body.BatteryLevel)
	}
	return nil
}

// NewWiredNoBatteryStatus returns the exact table-31 status policy for a
// USB-bus-powered controller with no battery. The active bit is clear because
// this helper asserts no IR-LED capability.
func NewWiredNoBatteryStatus(poweringOff bool) ExtendedStatusNoEventsBodyV1 {
	power := StatusFullPower
	if poweringOff {
		power = StatusPoweringOff
	}
	return ExtendedStatusNoEventsBodyV1{
		PowerLevel:   power,
		ChargeState:  StatusNotCharging,
		BatteryType:  StatusBatteryAbsent,
		BatteryLevel: StatusBatteryCriticallyLow,
	}
}

func (body ExtendedStatusNoEventsBodyV1) statusByte() byte {
	return byte(body.PowerLevel)<<6 |
		byte(body.ChargeState)<<4 |
		byte(body.BatteryType)<<2 |
		byte(body.BatteryLevel)
}

// EncodeExtendedStatusNoEventsBodyInto writes exactly four bytes without
// allocating. Reserved bytes two and three and Events Present are always zero.
func EncodeExtendedStatusNoEventsBodyInto(
	dst []byte,
	body ExtendedStatusNoEventsBodyV1,
) error {
	if len(dst) != ExtendedStatusNoEventsBodySize {
		return exactLengthError(
			"extended status no-events body", len(dst), ExtendedStatusNoEventsBodySize)
	}
	if err := body.Validate(); err != nil {
		return err
	}
	extended := byte(0)
	if body.DeviceActive {
		extended = 1
	}
	dst[0] = body.statusByte()
	dst[1] = extended
	dst[2] = 0
	dst[3] = 0
	return nil
}

// DecodeExtendedStatusNoEventsBody decodes only the four-byte no-events form.
// Reserved bits/bytes fail closed, and Events Present is rejected rather than
// interpreting an event-bearing payload under the short grammar.
func DecodeExtendedStatusNoEventsBody(
	src []byte,
) (ExtendedStatusNoEventsBodyV1, error) {
	if len(src) != ExtendedStatusNoEventsBodySize {
		return ExtendedStatusNoEventsBodyV1{}, exactLengthError(
			"extended status no-events body", len(src), ExtendedStatusNoEventsBodySize)
	}
	if src[1]&0xfc != 0 || src[2] != 0 || src[3] != 0 {
		return ExtendedStatusNoEventsBodyV1{}, fmt.Errorf(
			"%w: extended=0x%02x reserved=%02x%02x",
			ErrReservedExtendedStatusFlag, src[1], src[2], src[3])
	}
	if src[1]&0x02 != 0 {
		return ExtendedStatusNoEventsBodyV1{}, ErrUnsupportedStatusEvents
	}
	status := src[0]
	body := ExtendedStatusNoEventsBodyV1{
		PowerLevel:   StatusPowerLevel(status >> 6),
		ChargeState:  StatusChargeState(status >> 4 & 0x03),
		BatteryType:  StatusBatteryType(status >> 2 & 0x03),
		BatteryLevel: StatusBatteryLevel(status & 0x03),
		DeviceActive: src[1]&0x01 != 0,
	}
	if err := body.Validate(); err != nil {
		return ExtendedStatusNoEventsBodyV1{}, err
	}
	return body, nil
}

// ExtendedStatusNoEventsHeader returns the exact primary-device system
// Command-3 header. Status uses the global sequence pool.
func ExtendedStatusNoEventsHeader(sequence uint8) SinglePacketHeader {
	return SinglePacketHeader{
		DataClass:     DataClassCommand,
		MessageNumber: MessageNumberStatusDevice,
		System:        true,
		Sequence:      sequence,
		PayloadLength: ExtendedStatusNoEventsBodySize,
	}
}

// EncodeExtendedStatusNoEventsMessageInto writes the exact eight-byte message
// atomically with respect to dst: validation failure leaves dst unchanged.
func EncodeExtendedStatusNoEventsMessageInto(
	dst []byte,
	sequence uint8,
	body ExtendedStatusNoEventsBodyV1,
) error {
	if len(dst) != ExtendedStatusNoEventsMessageSize {
		return exactLengthError(
			"extended status no-events message", len(dst), ExtendedStatusNoEventsMessageSize)
	}
	if err := body.Validate(); err != nil {
		return err
	}
	var wire [ExtendedStatusNoEventsMessageSize]byte
	if err := EncodeSinglePacketHeaderInto(
		wire[:SinglePacketHeaderSize], ExtendedStatusNoEventsHeader(sequence)); err != nil {
		return err
	}
	if err := EncodeExtendedStatusNoEventsBodyInto(
		wire[SinglePacketHeaderSize:], body); err != nil {
		return err
	}
	copy(dst, wire[:])
	return nil
}

// DecodeExtendedStatusNoEventsMessage decodes one exact, uncoalesced,
// primary-device Extended Status message with Events Present clear.
func DecodeExtendedStatusNoEventsMessage(
	wire []byte,
) (sequence uint8, body ExtendedStatusNoEventsBodyV1, err error) {
	if len(wire) != ExtendedStatusNoEventsMessageSize {
		return 0, ExtendedStatusNoEventsBodyV1{}, exactLengthError(
			"extended status no-events message", len(wire), ExtendedStatusNoEventsMessageSize)
	}
	header, err := DecodeSinglePacketHeader(wire[:SinglePacketHeaderSize])
	if err != nil {
		return 0, ExtendedStatusNoEventsBodyV1{}, err
	}
	if header.DataClass != DataClassCommand ||
		header.MessageNumber != MessageNumberStatusDevice || !header.System ||
		header.AcknowledgementRequested || header.ExpansionIndex != 0 ||
		header.PayloadLength != ExtendedStatusNoEventsBodySize {
		return 0, ExtendedStatusNoEventsBodyV1{}, ErrInvalidStatusMessage
	}
	body, err = DecodeExtendedStatusNoEventsBody(wire[SinglePacketHeaderSize:])
	if err != nil {
		return 0, ExtendedStatusNoEventsBodyV1{}, err
	}
	return header.Sequence, body, nil
}

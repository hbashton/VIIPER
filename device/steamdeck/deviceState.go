package steamdeck

import (
	"encoding/binary"
	"fmt"
	"io"
)

// InputState is the client-facing input state for a Steam Deck (Jupiter/LCD)
// controller.
//
// This struct mirrors SDL's `SteamDeckStatePacket_t` fields (minus unPacketNum)
//
// Wire format (client -> device stream): fixed 52 bytes, little-endian.
// viiper:wire steamdeck c2s buttons:u64 lpX:i16 lpY:i16 rpX:i16 rpY:i16 ax:i16 ay:i16 az:i16 gx:i16 gy:i16 gz:i16 qw:i16 qx:i16 qy:i16 qz:i16 tl:u16 tr:u16 lsx:i16 lsy:i16 rsx:i16 rsy:i16 pl:u16 pr:u16
type InputState struct {
	Buttons uint64

	LeftPadX  int16
	LeftPadY  int16
	RightPadX int16
	RightPadY int16

	AccelX int16
	AccelY int16
	AccelZ int16

	GyroX int16
	GyroY int16
	GyroZ int16

	GyroQuatW int16
	GyroQuatX int16
	GyroQuatY int16
	GyroQuatZ int16

	TriggerRawL uint16
	TriggerRawR uint16

	LeftStickX int16
	LeftStickY int16

	RightStickX int16
	RightStickY int16

	PressurePadLeft  uint16
	PressurePadRight uint16
}

// MarshalBinary encodes InputState to the fixed 52-byte wire format.
func (s InputState) MarshalBinary() ([]byte, error) {
	b := make([]byte, InputStateSize)
	o := 0

	binary.LittleEndian.PutUint64(b[o:o+8], s.Buttons)
	o += 8

	putI16 := func(v int16) {
		binary.LittleEndian.PutUint16(b[o:o+2], uint16(v))
		o += 2
	}
	putU16 := func(v uint16) {
		binary.LittleEndian.PutUint16(b[o:o+2], v)
		o += 2
	}

	putI16(s.LeftPadX)
	putI16(s.LeftPadY)
	putI16(s.RightPadX)
	putI16(s.RightPadY)

	putI16(s.AccelX)
	putI16(s.AccelY)
	putI16(s.AccelZ)

	putI16(s.GyroX)
	putI16(s.GyroY)
	putI16(s.GyroZ)

	putI16(s.GyroQuatW)
	putI16(s.GyroQuatX)
	putI16(s.GyroQuatY)
	putI16(s.GyroQuatZ)

	putU16(s.TriggerRawL)
	putU16(s.TriggerRawR)

	putI16(s.LeftStickX)
	putI16(s.LeftStickY)
	putI16(s.RightStickX)
	putI16(s.RightStickY)

	putU16(s.PressurePadLeft)
	putU16(s.PressurePadRight)

	return b, nil
}

// UnmarshalBinary decodes InputState from the fixed 52-byte wire format.
func (s *InputState) UnmarshalBinary(data []byte) error {
	if len(data) < InputStateSize {
		return io.ErrUnexpectedEOF
	}
	o := 0

	s.Buttons = binary.LittleEndian.Uint64(data[o : o+8])
	o += 8

	getI16 := func() int16 {
		v := int16(binary.LittleEndian.Uint16(data[o : o+2]))
		o += 2
		return v
	}
	getU16 := func() uint16 {
		v := binary.LittleEndian.Uint16(data[o : o+2])
		o += 2
		return v
	}

	s.LeftPadX = getI16()
	s.LeftPadY = getI16()
	s.RightPadX = getI16()
	s.RightPadY = getI16()

	s.AccelX = getI16()
	s.AccelY = getI16()
	s.AccelZ = getI16()

	s.GyroX = getI16()
	s.GyroY = getI16()
	s.GyroZ = getI16()

	s.GyroQuatW = getI16()
	s.GyroQuatX = getI16()
	s.GyroQuatY = getI16()
	s.GyroQuatZ = getI16()

	s.TriggerRawL = getU16()
	s.TriggerRawR = getU16()

	s.LeftStickX = getI16()
	s.LeftStickY = getI16()
	s.RightStickX = getI16()
	s.RightStickY = getI16()

	s.PressurePadLeft = getU16()
	s.PressurePadRight = getU16()

	return nil
}

// HapticState is the client-facing representation of Steam Deck haptic feedback.
// Values are 16-bit "motor" speeds as used by SDL's Steam Deck HID driver.
type HapticState struct {
	LeftMotor  uint16
	RightMotor uint16
}

// MarshalBinary encodes HapticState as 4 bytes (2x uint16 little-endian).
func (r HapticState) MarshalBinary() ([]byte, error) {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b[0:2], r.LeftMotor)
	binary.LittleEndian.PutUint16(b[2:4], r.RightMotor)
	return b, nil
}

// UnmarshalBinary decodes HapticState from 4 bytes (2x uint16 little-endian).
func (r *HapticState) UnmarshalBinary(data []byte) error {
	if len(data) < 4 {
		return fmt.Errorf("Invalid haptic packet len, got %d, want 4", len(data))
	}
	r.LeftMotor = binary.LittleEndian.Uint16(data[0:2])
	r.RightMotor = binary.LittleEndian.Uint16(data[2:4])
	return nil
}

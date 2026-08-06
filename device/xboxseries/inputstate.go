package xboxseries

import (
	"encoding/binary"
	"io"
)

// The client-facing state deliberately mirrors the existing Xbox 360 state
// packet. Bit 16 is reserved by VIIPER for the Series Share button; Guide uses
// the standard XInput-compatible bit 10 and is emitted as a separate GIP key
// message on the USB side.
//
// viiper:wire xboxseries c2s buttons:u32 lt:u8 rt:u8 lx:i16 ly:i16 rx:i16 ry:i16 reserved:u8*6
type InputState struct {
	Buttons  uint32
	LT, RT   uint8
	LX, LY   int16
	RX, RY   int16
	Reserved [6]byte
}

const (
	ButtonDPadUp    uint32 = 0x0001
	ButtonDPadDown  uint32 = 0x0002
	ButtonDPadLeft  uint32 = 0x0004
	ButtonDPadRight uint32 = 0x0008
	ButtonMenu      uint32 = 0x0010
	ButtonView      uint32 = 0x0020
	ButtonL3        uint32 = 0x0040
	ButtonR3        uint32 = 0x0080
	ButtonLB        uint32 = 0x0100
	ButtonRB        uint32 = 0x0200
	ButtonGuide     uint32 = 0x0400
	ButtonA         uint32 = 0x1000
	ButtonB         uint32 = 0x2000
	ButtonX         uint32 = 0x4000
	ButtonY         uint32 = 0x8000
	ButtonShare     uint32 = 0x00010000
)

func NewInputState() *InputState { return &InputState{} }

func (s *InputState) MarshalBinary() ([]byte, error) {
	b := make([]byte, 20)
	binary.LittleEndian.PutUint32(b[0:4], s.Buttons)
	b[4], b[5] = s.LT, s.RT
	binary.LittleEndian.PutUint16(b[6:8], uint16(s.LX))
	binary.LittleEndian.PutUint16(b[8:10], uint16(s.LY))
	binary.LittleEndian.PutUint16(b[10:12], uint16(s.RX))
	binary.LittleEndian.PutUint16(b[12:14], uint16(s.RY))
	copy(b[14:20], s.Reserved[:])
	return b, nil
}

func (s *InputState) UnmarshalBinary(data []byte) error {
	if len(data) < 20 {
		return io.ErrUnexpectedEOF
	}
	s.Buttons = binary.LittleEndian.Uint32(data[0:4])
	s.LT, s.RT = data[4], data[5]
	s.LX = int16(binary.LittleEndian.Uint16(data[6:8]))
	s.LY = int16(binary.LittleEndian.Uint16(data[8:10]))
	s.RX = int16(binary.LittleEndian.Uint16(data[10:12]))
	s.RY = int16(binary.LittleEndian.Uint16(data[12:14]))
	copy(s.Reserved[:], data[14:20])
	return nil
}

// MotorState is the normalized VIIPER feedback contract. Motor amplitudes are
// 0..255. Timing remains in native GIP 10 ms units so clients can preserve the
// game's duration, delay, and repeat intent.
//
// viiper:wire xboxseries s2c left:u8 right:u8 leftImpulse:u8 rightImpulse:u8 duration:u8 delay:u8 repeat:u8
type MotorState struct {
	LeftMotor, RightMotor     uint8
	LeftImpulse, RightImpulse uint8
	Duration, Delay, Repeat   uint8
}

func (m *MotorState) MarshalBinary() ([]byte, error) {
	return []byte{m.LeftMotor, m.RightMotor, m.LeftImpulse, m.RightImpulse, m.Duration, m.Delay, m.Repeat}, nil
}

func (m *MotorState) UnmarshalBinary(data []byte) error {
	if len(data) < 7 {
		return io.ErrUnexpectedEOF
	}
	m.LeftMotor, m.RightMotor = data[0], data[1]
	m.LeftImpulse, m.RightImpulse = data[2], data[3]
	m.Duration, m.Delay, m.Repeat = data[4], data[5], data[6]
	return nil
}

func (s InputState) buildGamepadPayload() []byte {
	p := make([]byte, 32)
	if s.Buttons&ButtonY != 0 {
		p[0] |= 0x80
	}
	if s.Buttons&ButtonX != 0 {
		p[0] |= 0x40
	}
	if s.Buttons&ButtonB != 0 {
		p[0] |= 0x20
	}
	if s.Buttons&ButtonA != 0 {
		p[0] |= 0x10
	}
	if s.Buttons&ButtonView != 0 {
		p[0] |= 0x08
	}
	if s.Buttons&ButtonMenu != 0 {
		p[0] |= 0x04
	}
	if s.Buttons&ButtonR3 != 0 {
		p[1] |= 0x80
	}
	if s.Buttons&ButtonL3 != 0 {
		p[1] |= 0x40
	}
	if s.Buttons&ButtonRB != 0 {
		p[1] |= 0x20
	}
	if s.Buttons&ButtonLB != 0 {
		p[1] |= 0x10
	}
	if s.Buttons&ButtonDPadRight != 0 {
		p[1] |= 0x08
	}
	if s.Buttons&ButtonDPadLeft != 0 {
		p[1] |= 0x04
	}
	if s.Buttons&ButtonDPadDown != 0 {
		p[1] |= 0x02
	}
	if s.Buttons&ButtonDPadUp != 0 {
		p[1] |= 0x01
	}
	binary.LittleEndian.PutUint16(p[2:4], scaleTrigger(s.LT))
	binary.LittleEndian.PutUint16(p[4:6], scaleTrigger(s.RT))
	binary.LittleEndian.PutUint16(p[6:8], uint16(s.LX))
	binary.LittleEndian.PutUint16(p[8:10], uint16(s.LY))
	binary.LittleEndian.PutUint16(p[10:12], uint16(s.RX))
	binary.LittleEndian.PutUint16(p[12:14], uint16(s.RY))
	if s.Buttons&ButtonShare != 0 {
		p[14] = 0x01 // Console Function Map: Share
	}
	return p
}

func scaleTrigger(v uint8) uint16 {
	return uint16((uint32(v)*1023 + 127) / 255)
}

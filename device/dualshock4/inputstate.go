package dualshock4

import (
	"encoding/binary"
	"io"
)

// viiper:wire dualshock4 c2s stickLX:i8 stickLY:i8 stickRX:i8 stickRY:i8 buttons:u16 dpad:u8 triggerL2:u8 triggerR2:u8 touch1X:u16 touch1Y:u16 touch1Active:bool touch2X:u16 touch2Y:u16 touch2Active:bool gyroX:i16 gyroY:i16 gyroZ:i16 accelX:i16 accelY:i16 accelZ:i16
type InputState struct {
	LX, LY  int8
	RX, RY  int8
	Buttons uint16
	DPad    uint8
	L2, R2  uint8

	Touch1X, Touch1Y uint16
	Touch1Active     bool
	Touch2X, Touch2Y uint16
	Touch2Active     bool

	GyroX, GyroY, GyroZ    int16
	AccelX, AccelY, AccelZ int16
}

func (s *InputState) MarshalBinary() ([]byte, error) {
	b := make([]byte, 31)
	b[0] = uint8(s.LX)
	b[1] = uint8(s.LY)
	b[2] = uint8(s.RX)
	b[3] = uint8(s.RY)
	binary.LittleEndian.PutUint16(b[4:6], s.Buttons)
	b[6] = s.DPad
	b[7] = s.L2
	b[8] = s.R2
	binary.LittleEndian.PutUint16(b[9:11], s.Touch1X)
	binary.LittleEndian.PutUint16(b[11:13], s.Touch1Y)
	if s.Touch1Active {
		b[13] = 1
	} else {
		b[13] = 0
	}
	binary.LittleEndian.PutUint16(b[14:16], s.Touch2X)
	binary.LittleEndian.PutUint16(b[16:18], s.Touch2Y)
	if s.Touch2Active {
		b[18] = 1
	} else {
		b[18] = 0
	}
	binary.LittleEndian.PutUint16(b[19:21], uint16(s.GyroX))
	binary.LittleEndian.PutUint16(b[21:23], uint16(s.GyroY))
	binary.LittleEndian.PutUint16(b[23:25], uint16(s.GyroZ))
	binary.LittleEndian.PutUint16(b[25:27], uint16(s.AccelX))
	binary.LittleEndian.PutUint16(b[27:29], uint16(s.AccelY))
	binary.LittleEndian.PutUint16(b[29:31], uint16(s.AccelZ))
	return b, nil
}

func (s *InputState) UnmarshalBinary(data []byte) error {
	if len(data) < 31 {
		return io.ErrUnexpectedEOF
	}
	s.LX = int8(data[0])
	s.LY = int8(data[1])
	s.RX = int8(data[2])
	s.RY = int8(data[3])
	s.Buttons = binary.LittleEndian.Uint16(data[4:6])
	s.DPad = data[6]
	s.L2 = data[7]
	s.R2 = data[8]
	s.Touch1X = binary.LittleEndian.Uint16(data[9:11])
	s.Touch1Y = binary.LittleEndian.Uint16(data[11:13])
	s.Touch1Active = data[13] != 0
	s.Touch2X = binary.LittleEndian.Uint16(data[14:16])
	s.Touch2Y = binary.LittleEndian.Uint16(data[16:18])
	s.Touch2Active = data[18] != 0
	s.GyroX = int16(binary.LittleEndian.Uint16(data[19:21]))
	s.GyroY = int16(binary.LittleEndian.Uint16(data[21:23]))
	s.GyroZ = int16(binary.LittleEndian.Uint16(data[23:25]))
	s.AccelX = int16(binary.LittleEndian.Uint16(data[25:27]))
	s.AccelY = int16(binary.LittleEndian.Uint16(data[27:29]))
	s.AccelZ = int16(binary.LittleEndian.Uint16(data[29:31]))
	return nil
}

// viiper:wire dualshock4 s2c rumbleSmall:u8 rumbleLarge:u8 ledRed:u8 ledGreen:u8 ledBlue:u8 flashOn:u8 flashOff:u8
type OutputState struct {
	RumbleSmall uint8 // (0-255)
	RumbleLarge uint8 // (0-255)
	LedRed      uint8 // (0-255)
	LedGreen    uint8 // (0-255)
	LedBlue     uint8 // (0-255)
	FlashOn     uint8 // (units of 2.5ms)
	FlashOff    uint8 // (units of 2.5ms)
}

func (f *OutputState) MarshalBinary() ([]byte, error) {
	return []byte{
		f.RumbleSmall,
		f.RumbleLarge,
		f.LedRed,
		f.LedGreen,
		f.LedBlue,
		f.FlashOn,
		f.FlashOff,
	}, nil
}

func (f *OutputState) UnmarshalBinary(data []byte) error {
	if len(data) < 7 {
		return io.ErrUnexpectedEOF
	}
	f.RumbleSmall = data[0]
	f.RumbleLarge = data[1]
	f.LedRed = data[2]
	f.LedGreen = data[3]
	f.LedBlue = data[4]
	f.FlashOn = data[5]
	f.FlashOff = data[6]
	return nil
}

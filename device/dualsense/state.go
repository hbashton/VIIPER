package dualsense

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"time"
)

// nolint
// viiper:wire dualsense c2s stickLX:i8 stickLY:i8 stickRX:i8 stickRY:i8 buttons:u32 dpad:u8 triggerL2:u8 triggerR2:u8 touch1X:u16 touch1Y:u16 touch1Active:bool touch2X:u16 touch2Y:u16 touch2Active:bool gyroX:i16 gyroY:i16 gyroZ:i16 accelX:i16 accelY:i16 accelZ:i16
type InputState struct {
	LX, LY  int8
	RX, RY  int8
	Buttons uint32
	DPad    uint8
	L2, R2  uint8

	Touch1X, Touch1Y       uint16
	Touch1Active           bool
	Touch1Tracking         uint8
	Touch2X, Touch2Y       uint16
	Touch2Active           bool
	Touch2Tracking         uint8

	GyroX, GyroY, GyroZ    int16
	AccelX, AccelY, AccelZ int16
}

// NewInputState returns a DualSense input state in its neutral/resting state.
func NewInputState() *InputState {
	x, y, z := DefaultAccelRaw()
	return &InputState{
		AccelX: x,
		AccelY: y,
		AccelZ: z,
	}
}

func (s *InputState) MarshalBinary() ([]byte, error) {
	b := make([]byte, InputStateSize)
	b[0] = uint8(s.LX)
	b[1] = uint8(s.LY)
	b[2] = uint8(s.RX)
	b[3] = uint8(s.RY)
	binary.LittleEndian.PutUint32(b[4:8], s.Buttons)
	b[8] = s.DPad
	b[9] = s.L2
	b[10] = s.R2
	binary.LittleEndian.PutUint16(b[11:13], s.Touch1X)
	binary.LittleEndian.PutUint16(b[13:15], s.Touch1Y)
	b[15] = encodeTouchStatus(s.Touch1Active, s.Touch1Tracking)
	if s.Touch1Active && b[15] == 0 {
		b[15] = 1
	}
	binary.LittleEndian.PutUint16(b[16:18], s.Touch2X)
	binary.LittleEndian.PutUint16(b[18:20], s.Touch2Y)
	b[20] = encodeTouchStatus(s.Touch2Active, s.Touch2Tracking)
	if s.Touch2Active && b[20] == 0 {
		b[20] = 1
	}
	binary.LittleEndian.PutUint16(b[21:23], uint16(s.GyroX))
	binary.LittleEndian.PutUint16(b[23:25], uint16(s.GyroY))
	binary.LittleEndian.PutUint16(b[25:27], uint16(s.GyroZ))
	binary.LittleEndian.PutUint16(b[27:29], uint16(s.AccelX))
	binary.LittleEndian.PutUint16(b[29:31], uint16(s.AccelY))
	binary.LittleEndian.PutUint16(b[31:33], uint16(s.AccelZ))
	return b, nil
}

func (s *InputState) UnmarshalBinary(data []byte) error {
	if len(data) < InputStateSize {
		return io.ErrUnexpectedEOF
	}
	s.LX = int8(data[0])
	s.LY = int8(data[1])
	s.RX = int8(data[2])
	s.RY = int8(data[3])
	s.Buttons = binary.LittleEndian.Uint32(data[4:8])
	s.DPad = data[8]
	s.L2 = data[9]
	s.R2 = data[10]
	s.Touch1X = binary.LittleEndian.Uint16(data[11:13])
	s.Touch1Y = binary.LittleEndian.Uint16(data[13:15])
	s.Touch1Active, s.Touch1Tracking = decodeTouchStatus(data[15])
	s.Touch2X = binary.LittleEndian.Uint16(data[16:18])
	s.Touch2Y = binary.LittleEndian.Uint16(data[18:20])
	s.Touch2Active, s.Touch2Tracking = decodeTouchStatus(data[20])
	s.GyroX = int16(binary.LittleEndian.Uint16(data[21:23]))
	s.GyroY = int16(binary.LittleEndian.Uint16(data[23:25]))
	s.GyroZ = int16(binary.LittleEndian.Uint16(data[25:27]))
	s.AccelX = int16(binary.LittleEndian.Uint16(data[27:29]))
	s.AccelY = int16(binary.LittleEndian.Uint16(data[29:31]))
	s.AccelZ = int16(binary.LittleEndian.Uint16(data[31:33]))
	return nil
}

// nolint
// viiper:wire dualsense s2c rumbleSmall:u8 rumbleLarge:u8 ledRed:u8 ledGreen:u8 ledBlue:u8 playerLeds:u8 triggerR2Mode:u8 triggerR2StartResistance:u8 triggerR2EffectForce:u8 triggerR2RangeForce:u8 triggerR2NearReleaseStrength:u8 triggerR2NearMiddleStrength:u8 triggerR2PressedStrength:u8 triggerR2Frequency:u8 triggerL2Mode:u8 triggerL2StartResistance:u8 triggerL2EffectForce:u8 triggerL2RangeForce:u8 triggerL2NearReleaseStrength:u8 triggerL2NearMiddleStrength:u8 triggerL2PressedStrength:u8 triggerL2Frequency:u8
type OutputState struct {
	RumbleSmall uint8
	RumbleLarge uint8
	LedRed      uint8
	LedGreen    uint8
	LedBlue     uint8
	PlayerLeds  uint8

	TriggerR2Mode                uint8
	TriggerR2StartResistance     uint8
	TriggerR2EffectForce         uint8
	TriggerR2RangeForce          uint8
	TriggerR2NearReleaseStrength uint8
	TriggerR2NearMiddleStrength  uint8
	TriggerR2PressedStrength     uint8
	TriggerR2Frequency           uint8
	TriggerL2Mode                uint8
	TriggerL2StartResistance     uint8
	TriggerL2EffectForce         uint8
	TriggerL2RangeForce          uint8
	TriggerL2NearReleaseStrength uint8
	TriggerL2NearMiddleStrength  uint8
	TriggerL2PressedStrength     uint8
	TriggerL2Frequency           uint8
}

func (f *OutputState) MarshalBinary() ([]byte, error) {
	return []byte{
		f.RumbleSmall,
		f.RumbleLarge,
		f.LedRed,
		f.LedGreen,
		f.LedBlue,
		f.PlayerLeds,
	}, nil
}

func (f *OutputState) MarshalExtendedBinary() ([]byte, error) {
	b := make([]byte, OutputStateExtSize)
	b[0] = f.RumbleSmall
	b[1] = f.RumbleLarge
	b[2] = f.LedRed
	b[3] = f.LedGreen
	b[4] = f.LedBlue
	b[5] = f.PlayerLeds

	b[6] = f.TriggerR2Mode
	b[7] = f.TriggerR2StartResistance
	b[8] = f.TriggerR2EffectForce
	b[9] = f.TriggerR2RangeForce
	b[10] = f.TriggerR2NearReleaseStrength
	b[11] = f.TriggerR2NearMiddleStrength
	b[12] = f.TriggerR2PressedStrength
	b[15] = f.TriggerR2Frequency

	b[17] = f.TriggerL2Mode
	b[18] = f.TriggerL2StartResistance
	b[19] = f.TriggerL2EffectForce
	b[20] = f.TriggerL2RangeForce
	b[21] = f.TriggerL2NearReleaseStrength
	b[22] = f.TriggerL2NearMiddleStrength
	b[23] = f.TriggerL2PressedStrength
	b[26] = f.TriggerL2Frequency
	return b, nil
}

func (f *OutputState) UnmarshalBinary(data []byte) error {
	if len(data) < OutputStateSize {
		return io.ErrUnexpectedEOF
	}
	f.RumbleSmall = data[0]
	f.RumbleLarge = data[1]
	f.LedRed = data[2]
	f.LedGreen = data[3]
	f.LedBlue = data[4]
	f.PlayerLeds = data[5]
	if len(data) < OutputStateExtSize {
		return nil
	}

	f.TriggerR2Mode = data[6]
	f.TriggerR2StartResistance = data[7]
	f.TriggerR2EffectForce = data[8]
	f.TriggerR2RangeForce = data[9]
	f.TriggerR2NearReleaseStrength = data[10]
	f.TriggerR2NearMiddleStrength = data[11]
	f.TriggerR2PressedStrength = data[12]
	f.TriggerR2Frequency = data[15]
	f.TriggerL2Mode = data[17]
	f.TriggerL2StartResistance = data[18]
	f.TriggerL2EffectForce = data[19]
	f.TriggerL2RangeForce = data[20]
	f.TriggerL2NearReleaseStrength = data[21]
	f.TriggerL2NearMiddleStrength = data[22]
	f.TriggerL2PressedStrength = data[23]
	f.TriggerL2Frequency = data[26]
	return nil
}

func encodeTouchStatus(active bool, tracking uint8) uint8 {
	if tracking != 0 {
		if active {
			return tracking &^ 0x80
		}
		return tracking | 0x80
	}
	if active {
		return 0
	}
	return TouchInactiveMask
}

func decodeTouchStatus(status uint8) (bool, uint8) {
	return status&0x80 == 0, status
}

type MetaState struct {
	SerialNumber string    `json:"serial_number"`
	MACAddress   string    `json:"mac_address"` // "XX:XX:XX:XX:XX:XX"
	Board        string    `json:"board"`
	BuildTime    time.Time `json:"build_time"`

	BatteryStatus      uint8   `json:"battery_status"`
	TemperatureCelsius float64 `json:"temperature_celsius"`
	BatteryVoltage     float64 `json:"battery_voltage"`

	ShellColor string `json:"shell_color"` // hardware variant / controller color code, e.g. "00", "Z1"
}

func (m *MetaState) ToMap() map[string]any {
	bytes, err := json.Marshal(m)
	if err != nil {
		slog.Error("marshal meta state for map", "error", err)
		return map[string]any{}
	}
	var res map[string]any
	err = json.Unmarshal(bytes, &res)
	if err != nil {
		slog.Error("unmarshal meta state for map", "error", err)
		return map[string]any{}
	}
	return res
}

func (m *MetaState) UpdateFromMap(data map[string]any) {
	bytes, err := json.Marshal(data)
	if err != nil {
		slog.Error("marshal meta state for update", "error", err)
		return
	}
	var newMeta MetaState
	err = json.Unmarshal(bytes, &newMeta)
	if err != nil {
		slog.Error("unmarshal meta state for update", "error", err)
		return
	}
	if newMeta.SerialNumber != "" {
		m.SerialNumber = newMeta.SerialNumber
	}
	if newMeta.MACAddress != "" {
		m.MACAddress = newMeta.MACAddress
	}
	if newMeta.Board != "" {
		m.Board = newMeta.Board
	}
	if !newMeta.BuildTime.IsZero() {
		m.BuildTime = newMeta.BuildTime
	}
	if newMeta.BatteryStatus != 0 {
		m.BatteryStatus = newMeta.BatteryStatus
	}
	if newMeta.TemperatureCelsius != 0 {
		m.TemperatureCelsius = newMeta.TemperatureCelsius
	}
	if newMeta.BatteryVoltage != 0 {
		m.BatteryVoltage = newMeta.BatteryVoltage
	}
	m.ShellColor = newMeta.ShellColor
}

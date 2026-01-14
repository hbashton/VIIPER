package dualshock4

const (
	DefaultVID = 0x054C
	DefaultPID = 0x05C4
)

const (
	EndpointIn  = 0x84
	EndpointOut = 0x03
)

const (
	ReportIDInput   = 0x01
	ReportIDOutput  = 0x05
	ReportIDFeature = 0x02
)

const (
	InputReportSize  = 64
	OutputReportSize = 32
)

const (
	ButtonSquare   uint16 = 0x0010
	ButtonCross    uint16 = 0x0020
	ButtonCircle   uint16 = 0x0040
	ButtonTriangle uint16 = 0x0080

	DPadMask uint8 = 0x0F
)

const (
	ButtonL1      uint16 = 0x0100
	ButtonR1      uint16 = 0x0200
	ButtonL2      uint16 = 0x0400
	ButtonR2      uint16 = 0x0800
	ButtonShare   uint16 = 0x1000
	ButtonOptions uint16 = 0x2000
	ButtonL3      uint16 = 0x4000
	ButtonR3      uint16 = 0x8000

	ButtonPS            uint16 = 0x0001
	ButtonTouchpadClick uint16 = 0x0002
)

const (
	ButtonPSUSB            uint8 = 0x01
	ButtonTouchpadClickUSB uint8 = 0x02

	CounterMask  = 0xFC
	CounterShift = 2
)

const (
	DPadUSBUp        = 0x00
	DPadUSBUpRight   = 0x01
	DPadUSBRight     = 0x02
	DPadUSBDownRight = 0x03
	DPadUSBDown      = 0x04
	DPadUSBDownLeft  = 0x05
	DPadUSBLeft      = 0x06
	DPadUSBUpLeft    = 0x07
	DPadUSBNeutral   = 0x08
)

const (
	DPadUp    = 0x01
	DPadDown  = 0x02
	DPadLeft  = 0x04
	DPadRight = 0x08
)

// The DS4 USB input report carries gyro/accel as signed int16 values.
// VIIPER's wire protocol keeps them as int16, but interprets them as fixed-point
// physical units to avoid float serialization across clients.
//
// Gyro fields (GyroX/Y/Z): °/s scaled by GyroCountsPerDps.
// Accel fields (AccelX/Y/Z): m/s² scaled by AccelCountsPerMS2.
const (
	// GyroCountsPerDps is the fixed-point scale factor for °/s.
	// resolution is 0.0625 °/s and range is about +-2048 °/s.
	GyroCountsPerDps = 16.0

	// AccelCountsPerMS2 is the fixed-point scale factor for m/s².
	// resolution is ~0.00195 m/s² and range is about +-64 m/s² (~+-6.5 g).
	AccelCountsPerMS2 = 512.0

	StandardGravityMS2 = 9.81
)

// Default accelerometer raw values for a controller lying flat on a table.
const (
	DefaultAccelXRaw int16 = 0
	DefaultAccelYRaw int16 = 0
	// -StandardGravityMS2 * AccelCountsPerMS2 = (-9.81 * 512) = -5023
	DefaultAccelZRaw int16 = -5023
)

const (
	TouchpadMinX uint16 = 0
	TouchpadMaxX uint16 = 1920
	TouchpadMinY uint16 = 0
	TouchpadMaxY uint16 = 942

	TouchInactiveMask uint8 = 0x80
)

const (
	BatteryLevelMask    = 0x0F
	BatteryChargingFlag = 0x10
	BatteryFullyCharged = 0x0B
	BatteryDefault      = 0x1B
)

const (
	OutOffsetReportID    = 0
	OutOffsetFlags       = 1
	OutOffsetRumbleSmall = 4
	OutOffsetRumbleLarge = 5
	OutOffsetLedRed      = 6
	OutOffsetLedGreen    = 7
	OutOffsetLedBlue     = 8
	OutOffsetFlashOn     = 9  // Flash on time (units of 2.5ms)
	OutOffsetFlashOff    = 10 // Flash off time (units of 2.5ms)
)

const (
	DefaultLedRed   = 0x00
	DefaultLedGreen = 0x00
	DefaultLedBlue  = 0x40
)

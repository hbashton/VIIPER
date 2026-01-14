package dualshock4

import "math"

// GyroDpsToRaw converts a gyro angular velocity value in degrees/second (°/s)
// into the fixed-point raw int16 wire/report representation.
func GyroDpsToRaw(dps float64) int16 {
	return clampI16(math.Round(dps * GyroCountsPerDps))
}

// GyroRawToDps converts a fixed-point raw gyro value into degrees/second (°/s).
func GyroRawToDps(raw int16) float64 {
	return float64(raw) / GyroCountsPerDps
}

// AccelMS2ToRaw converts an acceleration value in meters/second^2 (m/s²)
// into the fixed-point raw int16 wire/report representation.
func AccelMS2ToRaw(ms2 float64) int16 {
	return clampI16(math.Round(ms2 * AccelCountsPerMS2))
}

// AccelRawToMS2 converts a fixed-point raw accelerometer value into m/s².
func AccelRawToMS2(raw int16) float64 {
	return float64(raw) / AccelCountsPerMS2
}

// DefaultAccelRaw returns the default ("neutral") accelerometer vector for a
// controller lying flat on a table.
func DefaultAccelRaw() (x, y, z int16) {
	return DefaultAccelXRaw, DefaultAccelYRaw, DefaultAccelZRaw
}

func clampI16(v float64) int16 {
	if v > math.MaxInt16 {
		return math.MaxInt16
	}
	if v < math.MinInt16 {
		return math.MinInt16
	}
	return int16(v)
}

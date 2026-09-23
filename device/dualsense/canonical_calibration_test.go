package dualsense

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVirtualSonyCalibrationMatchesDS4WindowsCanonicalMotion(t *testing.T) {
	// Cross-repository contract vector from the real DS4Windows canonical mapper:
	// ViiperCanonicalMotionCalibrationTests serializes pitch +100, yaw +200,
	// roll -50 degrees/s and +1g Z. Sony yaw/roll wire signs are reversed.
	// The fixed bytes make a units mismatch visible at either side of V5.
	motion, err := hex.DecodeString("400680F32003000000000020")
	require.NoError(t, err)
	var packet [InputStateSize]byte
	copy(packet[21:], motion)
	var state InputState
	require.NoError(t, state.UnmarshalBinary(packet[:]))
	read16 := func(data []byte, offset int) int32 {
		return int32(int16(binary.LittleEndian.Uint16(data[offset : offset+2])))
	}
	for _, edge := range []bool{false, true} {
		d, err := new(nil, edge)
		require.NoError(t, err)
		var report [InputReportSize]byte
		require.True(t, encodeUSBInputReportInto(&state, 0, 1, 1, 1, edge, report[:]))
		require.Equal(t, motion, report[16:28])
		calibration := d.featureReportCalibration()
		// Same physical calibration equation/offsets as SDL's PS5 driver
		// c71abd0 lines636-664; expressed directly in degrees/s and g here.
		gyroSpeed := read16(calibration, 19) + read16(calibration, 21)
		for axis, want := range []float64{100, -200, 50} {
			bias := read16(calibration, 1+axis*2)
			rangeUnits := read16(calibration, 7+axis*4) - read16(calibration, 9+axis*4)
			measured := float64(read16(report[:], 16+axis*2)-bias) * float64(gyroSpeed) / float64(rangeUnits)
			require.Equal(t, want, measured, "edge=%t gyro axis=%d", edge, axis)
		}
		for axis, want := range []float64{0, 0, 1} {
			plus := read16(calibration, 23+axis*4)
			rangeUnits := plus - read16(calibration, 25+axis*4)
			bias := plus - rangeUnits/2
			measured := float64(read16(report[:], 22+axis*2)-bias) * 2 / float64(rangeUnits)
			require.Equal(t, want, measured, "edge=%t accel axis=%d", edge, axis)
		}
	}
}

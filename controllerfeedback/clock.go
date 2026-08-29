package controllerfeedback

import (
	"math"
	"math/bits"
)

const (
	// ClockDomainWindowsQPCV1 identifies CFBK v1's only interoperable clock
	// domain. It is the system-wide Windows QueryPerformanceCounter value,
	// converted to integer microseconds using QueryPerformanceFrequency.
	// Process-relative elapsed-time origins are not compatible with this domain.
	ClockDomainWindowsQPCV1 = "windows-qpc-host-v1"

	microsecondsPerSecond uint64 = 1_000_000
)

// qpcToMicroseconds performs the normative QPC conversion without overflow.
// It truncates sub-microsecond precision consistently with DS4Windows.
func qpcToMicroseconds(counter, frequency uint64) (uint64, bool) {
	if frequency == 0 {
		return 0, false
	}
	whole := counter / frequency
	if whole > math.MaxUint64/microsecondsPerSecond {
		return 0, false
	}
	remainder := counter % frequency
	high, low := bits.Mul64(remainder, microsecondsPerSecond)
	fraction, _ := bits.Div64(high, low, frequency)
	return whole*microsecondsPerSecond + fraction, true
}

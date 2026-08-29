package controllerfeedback

import (
	"math"
	"testing"
)

func TestQPCConversionVectorsMatchDS4Windows(t *testing.T) {
	tests := []struct {
		counter   uint64
		frequency uint64
		want      uint64
		valid     bool
	}{
		{0, 10_000_000, 0, true},
		{10_000_000, 10_000_000, 1_000_000, true},
		{12_345_678, 10_000_000, 1_234_567, true},
		{math.MaxUint64, math.MaxUint64, 1_000_000, true},
		{1, 0, 0, false},
		{math.MaxUint64, 1, 0, false},
	}
	for _, test := range tests {
		got, valid := qpcToMicroseconds(test.counter, test.frequency)
		if got != test.want || valid != test.valid {
			t.Fatalf("counter=%d frequency=%d got=(%d,%t) want=(%d,%t)",
				test.counter, test.frequency, got, valid, test.want,
				test.valid)
		}
	}
}

func TestHostClockDomainIsExplicit(t *testing.T) {
	if ClockDomainWindowsQPCV1 != "windows-qpc-host-v1" {
		t.Fatalf("clock domain = %q", ClockDomainWindowsQPCV1)
	}
}

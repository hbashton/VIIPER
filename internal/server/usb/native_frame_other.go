//go:build !windows

package usb

import "time"

var nativeProcessClockStart = time.Now()

func nativeClockSnapshot() nativeClockSample {
	now := time.Now()
	return nativeClockSample{
		now:   now,
		frame: uint32(now.Sub(nativeProcessClockStart) / time.Millisecond),
	}
}

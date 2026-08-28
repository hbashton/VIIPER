//go:build !windows

package inputlatency

import "errors"

var errWindowsQPCRequired = errors.New("input latency evidence requires Windows QueryPerformanceCounter")

func Counter() (int64, error)        { return 0, errWindowsQPCRequired }
func Frequency() (int64, error)      { return 0, errWindowsQPCRequired }
func ClockIdentity() (string, error) { return "", errWindowsQPCRequired }

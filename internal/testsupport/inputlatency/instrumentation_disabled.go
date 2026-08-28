//go:build !windows || !viiper_latency

package inputlatency

// InstrumentationEnabled reports whether the DualSense broker/commit hooks are
// compiled into this process. Ordinary production builds deliberately return
// false and cannot construct a live USB/IP latency probe.
func InstrumentationEnabled() bool { return false }

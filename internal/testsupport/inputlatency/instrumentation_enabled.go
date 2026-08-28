//go:build windows && viiper_latency

package inputlatency

// InstrumentationEnabled reports whether the DualSense broker/commit hooks are
// compiled into this process.
func InstrumentationEnabled() bool { return true }

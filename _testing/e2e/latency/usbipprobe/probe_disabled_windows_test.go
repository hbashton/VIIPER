//go:build windows && !viiper_latency

package usbipprobe

import (
	"strings"
	"testing"
)

func TestProbeFailsFastWithoutTaggedInstrumentation(t *testing.T) {
	if probe, err := New(Config{}); err == nil {
		_ = probe
		t.Fatal("ordinary build constructed an unusable live latency probe")
	} else if !strings.Contains(err.Error(), "-tags viiper_latency") {
		t.Fatalf("unexpected fail-fast error: %v", err)
	}
}

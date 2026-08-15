//go:build !windows

package e2e_bench_test

import (
	"os"
	"runtime"
	"testing"
)

func TestLiveControllerToGameLatencyGate(t *testing.T) {
	if os.Getenv("VIIPER_E2E_LIVE_LATENCY") == "1" {
		t.Fatalf("live controller-to-game latency requires Windows with CGO/SDL3; got %s CGO-disabled build",
			runtime.GOOS)
	}
	t.Skip("live controller-to-game latency is an explicit Windows+CGO production gate")
}

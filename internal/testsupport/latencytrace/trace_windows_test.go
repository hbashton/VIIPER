//go:build windows

package latencytrace

import (
	"sync/atomic"
	"testing"

	"github.com/Microsoft/go-winio/pkg/etw"
)

func TestCaptureStateDoesNotDisableActiveProvider(t *testing.T) {
	var enabled atomic.Bool
	applyProviderState(&enabled, etw.ProviderStateEnable)
	applyProviderState(&enabled, etw.ProviderStateCaptureState)
	if !enabled.Load() {
		t.Fatal("ETW capture-state request disabled the active marker provider")
	}
	applyProviderState(&enabled, etw.ProviderStateDisable)
	if enabled.Load() {
		t.Fatal("ETW disable request left the marker provider enabled")
	}
}

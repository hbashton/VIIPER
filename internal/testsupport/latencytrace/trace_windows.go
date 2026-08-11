//go:build windows

// Package latencytrace supplies the Windows clocks and source-controlled
// TraceLogging markers used to correlate each latency JSON sample with ETL.
package latencytrace

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/Alia5/VIIPER/_testing/e2e/latency"
	"github.com/Microsoft/go-winio/pkg/etw"
	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"
)

var (
	kernel32QPC               = windows.NewLazySystemDLL("kernel32.dll")
	queryPerformanceCounter   = kernel32QPC.NewProc("QueryPerformanceCounter")
	queryPerformanceFrequency = kernel32QPC.NewProc("QueryPerformanceFrequency")
)

func query(proc *windows.LazyProc) (int64, error) {
	var value int64
	ok, _, callErr := proc.Call(uintptr(unsafe.Pointer(&value)))
	if ok == 0 {
		return 0, fmt.Errorf("%s: %w", proc.Name, callErr)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s returned %d", proc.Name, value)
	}
	return value, nil
}

func Counter() (int64, error)   { return query(queryPerformanceCounter) }
func Frequency() (int64, error) { return query(queryPerformanceFrequency) }

type Provider struct {
	provider *etw.Provider
	enabled  atomic.Bool
}

func applyProviderState(enabled *atomic.Bool, state etw.ProviderState) {
	switch state {
	case etw.ProviderStateEnable:
		enabled.Store(true)
	case etw.ProviderStateDisable:
		enabled.Store(false)
	case etw.ProviderStateCaptureState:
		// A capture-state request asks an already enabled provider to emit
		// rundown state; it does not disable the session.
	}
}

func NewProvider() (*Provider, error) {
	id, err := guid.FromString(strings.Trim(latency.TraceProviderGUID, "{}"))
	if err != nil {
		return nil, fmt.Errorf("parse latency provider GUID: %w", err)
	}
	result := &Provider{}
	provider, err := etw.NewProviderWithID(latency.TraceProviderName, id,
		func(_ guid.GUID, state etw.ProviderState, _ etw.Level, _, _ uint64, _ uintptr) {
			applyProviderState(&result.enabled, state)
		})
	if err != nil {
		return nil, fmt.Errorf("register latency TraceLogging provider: %w", err)
	}
	result.provider = provider
	return result, nil
}

func (p *Provider) Close() error {
	if p == nil || p.provider == nil {
		return nil
	}
	return p.provider.Close()
}

func (p *Provider) Enabled() bool { return p != nil && p.enabled.Load() }

func (p *Provider) WriteSample(controller, transport string, block int, sample latency.Sample) error {
	if !p.Enabled() {
		return errors.New("latency TraceLogging provider is not enabled by the active WPR profile")
	}
	return p.provider.WriteEvent("TransitionObserved",
		[]etw.EventOpt{etw.WithLevel(etw.LevelInfo)},
		etw.WithFields(
			etw.StringField("MarkerID", sample.MarkerID),
			etw.StringField("Controller", controller),
			etw.StringField("Transport", transport),
			etw.IntField("TransportBlock", block),
			etw.IntField("Sequence", sample.Sequence),
			etw.StringField("Transition", string(sample.Transition)),
			etw.Int64Field("StartQPCTicks", sample.StartQPCTicks),
			etw.Int64Field("EndQPCTicks", sample.EndQPCTicks),
			etw.Int64Field("MarkerQPCTicks", sample.MarkerQPCTicks),
			etw.Int64Field("LatencyNS", sample.LatencyNS),
			etw.Uint64Field("SDLEventTimestampNS", sample.EventTimestampNS),
			etw.Uint64Field("SDLFenceTimestampNS", sample.SDLFenceTimestampNS),
		))
}

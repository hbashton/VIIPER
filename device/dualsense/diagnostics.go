package dualsense

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const trafficEventLimit = 65536

type TrafficEvent struct {
	TimeUTC       string `json:"timeUtc"`
	Direction     string `json:"direction"`
	Source        string `json:"source"`
	ReportType    string `json:"reportType,omitempty"`
	ReportID      string `json:"reportId,omitempty"`
	Request       string `json:"request,omitempty"`
	Value         string `json:"value,omitempty"`
	Index         string `json:"index,omitempty"`
	Length        int    `json:"length"`
	Hex           string `json:"hex,omitempty"`
	Summary       string `json:"summary,omitempty"`
	DecodedOutput string `json:"decodedOutput,omitempty"`
}

var trafficDiagnosticsEnabled atomic.Bool

var trafficDiagnostics = struct {
	sync.Mutex
	events []TrafficEvent
}{}

func init() {
	trafficDiagnosticsEnabled.Store(rawOutputLogEnabled)
}

func SetTrafficDiagnosticsEnabled(enabled bool, clear bool) {
	trafficDiagnosticsEnabled.Store(enabled)
	if clear {
		ClearTrafficDiagnostics()
	}
	slog.Info("DualSense traffic diagnostics toggled", "enabled", enabled, "clear", clear)
}

func TrafficDiagnosticsEnabled() bool {
	return trafficDiagnosticsEnabled.Load()
}

func ClearTrafficDiagnostics() {
	trafficDiagnostics.Lock()
	trafficDiagnostics.events = nil
	trafficDiagnostics.Unlock()
}

func TrafficDiagnosticsSnapshot() []TrafficEvent {
	trafficDiagnostics.Lock()
	defer trafficDiagnostics.Unlock()

	events := make([]TrafficEvent, len(trafficDiagnostics.events))
	copy(events, trafficDiagnostics.events)
	return events
}

func recordTrafficEvent(event TrafficEvent) {
	if !TrafficDiagnosticsEnabled() {
		return
	}

	if event.TimeUTC == "" {
		event.TimeUTC = time.Now().UTC().Format(time.RFC3339Nano)
	}

	trafficDiagnostics.Lock()
	if len(trafficDiagnostics.events) >= trafficEventLimit {
		copy(trafficDiagnostics.events, trafficDiagnostics.events[1:])
		trafficDiagnostics.events[len(trafficDiagnostics.events)-1] = event
	} else {
		trafficDiagnostics.events = append(trafficDiagnostics.events, event)
	}
	trafficDiagnostics.Unlock()

	attrs := []any{
		"direction", event.Direction,
		"source", event.Source,
		"len", event.Length,
	}
	if event.ReportType != "" {
		attrs = append(attrs, "reportType", event.ReportType)
	}
	if event.ReportID != "" {
		attrs = append(attrs, "reportID", event.ReportID)
	}
	if event.Request != "" {
		attrs = append(attrs, "request", event.Request)
	}
	if event.Summary != "" {
		attrs = append(attrs, "summary", event.Summary)
	}
	if event.DecodedOutput != "" {
		attrs = append(attrs, "decodedOutput", event.DecodedOutput)
	}
	if event.Hex != "" {
		attrs = append(attrs, "hex", event.Hex)
	}
	slog.Info("DualSense host traffic", attrs...)
}

func recordTrafficBytes(direction, source string, data []byte, fields ...any) {
	event := TrafficEvent{
		Direction: direction,
		Source:    source,
		Length:    len(data),
		Hex:       hex.EncodeToString(data),
	}
	for i := 0; i+1 < len(fields); i += 2 {
		key, _ := fields[i].(string)
		value := fmt.Sprint(fields[i+1])
		switch key {
		case "reportType":
			event.ReportType = value
		case "reportID":
			event.ReportID = value
		case "request":
			event.Request = value
		case "value":
			event.Value = value
		case "index":
			event.Index = value
		case "summary":
			event.Summary = value
		case "decodedOutput":
			event.DecodedOutput = value
		}
	}
	recordTrafficEvent(event)
}

func describeOutputState(out OutputState) string {
	return fmt.Sprintf(
		"rumbleSmall=%d rumbleLarge=%d led=%d,%d,%d playerLeds=0x%02X r2=%02X/%02X/%02X/%02X/%02X/%02X/%02X/%02X l2=%02X/%02X/%02X/%02X/%02X/%02X/%02X/%02X",
		out.RumbleSmall,
		out.RumbleLarge,
		out.LedRed,
		out.LedGreen,
		out.LedBlue,
		out.PlayerLeds,
		out.TriggerR2Mode,
		out.TriggerR2StartResistance,
		out.TriggerR2EffectForce,
		out.TriggerR2RangeForce,
		out.TriggerR2NearReleaseStrength,
		out.TriggerR2NearMiddleStrength,
		out.TriggerR2PressedStrength,
		out.TriggerR2Frequency,
		out.TriggerL2Mode,
		out.TriggerL2StartResistance,
		out.TriggerL2EffectForce,
		out.TriggerL2RangeForce,
		out.TriggerL2NearReleaseStrength,
		out.TriggerL2NearMiddleStrength,
		out.TriggerL2PressedStrength,
		out.TriggerL2Frequency)
}

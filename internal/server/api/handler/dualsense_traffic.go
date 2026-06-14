package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/internal/server/api"
	apierror "github.com/Alia5/VIIPER/internal/server/api/error"
	"github.com/Alia5/VIIPER/viipertypes"
)

func DualSenseTrafficSet() api.HandlerFunc {
	return func(req *api.Request, res *api.Response, logger *slog.Logger) error {
		var payload viipertypes.DualSenseTrafficSetRequest
		if req.Payload != "" {
			if err := json.Unmarshal([]byte(req.Payload), &payload); err != nil {
				return apierror.ErrBadRequest(fmt.Sprintf("invalid JSON payload: %v", err))
			}
		}

		dualsense.SetTrafficDiagnosticsEnabled(payload.Enabled, payload.Clear)
		logger.Info("DualSense traffic diagnostics set", "enabled", payload.Enabled, "clear", payload.Clear)
		events := dualsense.TrafficDiagnosticsSnapshot()
		out, err := json.Marshal(viipertypes.DualSenseTrafficResponse{
			Enabled: dualsense.TrafficDiagnosticsEnabled(),
			Count:   len(events),
		})
		if err != nil {
			return apierror.ErrInternal(fmt.Sprintf("failed to marshal response: %v", err))
		}
		res.JSON = string(out)
		return nil
	}
}

func DualSenseTrafficGet() api.HandlerFunc {
	return func(_ *api.Request, res *api.Response, _ *slog.Logger) error {
		events := dualsense.TrafficDiagnosticsSnapshot()
		out, err := json.Marshal(viipertypes.DualSenseTrafficResponse{
			Enabled: dualsense.TrafficDiagnosticsEnabled(),
			Count:   len(events),
			Events:  makeDualSenseTrafficEvents(events),
		})
		if err != nil {
			return apierror.ErrInternal(fmt.Sprintf("failed to marshal response: %v", err))
		}
		res.JSON = string(out)
		return nil
	}
}

func DualSenseTrafficClear() api.HandlerFunc {
	return func(_ *api.Request, res *api.Response, logger *slog.Logger) error {
		dualsense.ClearTrafficDiagnostics()
		logger.Info("DualSense traffic diagnostics cleared")
		events := dualsense.TrafficDiagnosticsSnapshot()
		out, err := json.Marshal(viipertypes.DualSenseTrafficResponse{
			Enabled: dualsense.TrafficDiagnosticsEnabled(),
			Count:   len(events),
		})
		if err != nil {
			return apierror.ErrInternal(fmt.Sprintf("failed to marshal response: %v", err))
		}
		res.JSON = string(out)
		return nil
	}
}

func makeDualSenseTrafficEvents(events []dualsense.TrafficEvent) []viipertypes.DualSenseTrafficEvent {
	out := make([]viipertypes.DualSenseTrafficEvent, len(events))
	for idx, event := range events {
		out[idx] = viipertypes.DualSenseTrafficEvent{
			TimeUTC:       event.TimeUTC,
			Direction:     event.Direction,
			Source:        event.Source,
			ReportType:    event.ReportType,
			ReportID:      event.ReportID,
			Request:       event.Request,
			Value:         event.Value,
			Index:         event.Index,
			Length:        event.Length,
			Hex:           event.Hex,
			Summary:       event.Summary,
			DecodedOutput: event.DecodedOutput,
		}
	}

	return out
}

package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/internal/server/api"
	apierror "github.com/Alia5/VIIPER/internal/server/api/error"
)

type dualSenseTrafficSetRequest struct {
	Enabled bool `json:"enabled"`
	Clear   bool `json:"clear"`
}

type dualSenseTrafficResponse struct {
	Enabled bool                     `json:"enabled"`
	Count   int                      `json:"count"`
	Events  []dualsense.TrafficEvent `json:"events,omitempty"`
}

func DualSenseTrafficSet() api.HandlerFunc {
	return func(req *api.Request, res *api.Response, logger *slog.Logger) error {
		var payload dualSenseTrafficSetRequest
		if req.Payload != "" {
			if err := json.Unmarshal([]byte(req.Payload), &payload); err != nil {
				return apierror.ErrBadRequest(fmt.Sprintf("invalid JSON payload: %v", err))
			}
		}

		dualsense.SetTrafficDiagnosticsEnabled(payload.Enabled, payload.Clear)
		logger.Info("DualSense traffic diagnostics set", "enabled", payload.Enabled, "clear", payload.Clear)
		return writeDualSenseTrafficResponse(res, false)
	}
}

func DualSenseTrafficGet() api.HandlerFunc {
	return func(_ *api.Request, res *api.Response, _ *slog.Logger) error {
		return writeDualSenseTrafficResponse(res, true)
	}
}

func DualSenseTrafficClear() api.HandlerFunc {
	return func(_ *api.Request, res *api.Response, logger *slog.Logger) error {
		dualsense.ClearTrafficDiagnostics()
		logger.Info("DualSense traffic diagnostics cleared")
		return writeDualSenseTrafficResponse(res, false)
	}
}

func writeDualSenseTrafficResponse(res *api.Response, includeEvents bool) error {
	events := dualsense.TrafficDiagnosticsSnapshot()
	response := dualSenseTrafficResponse{
		Enabled: dualsense.TrafficDiagnosticsEnabled(),
		Count:   len(events),
	}
	if includeEvents {
		response.Events = events
	}

	out, err := json.Marshal(response)
	if err != nil {
		return apierror.ErrInternal(fmt.Sprintf("failed to marshal response: %v", err))
	}
	res.JSON = string(out)
	return nil
}

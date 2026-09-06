package handler

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/Alia5/VIIPER/internal/server/api"
	apierror "github.com/Alia5/VIIPER/internal/server/api/error"
	"github.com/Alia5/VIIPER/internal/server/usb"
)

const maximumNS2ProRuntimeStatusPayloadBytes = 512

type ns2ProRuntimeStatusV1Device interface {
	UpdateNS2ProRuntimeStatusV1(payload string) error
}

// BusDeviceNS2ProRuntimeStatusV1 updates only the mutable NS2 Pro power
// snapshot. The explicit versioned route prevents clients from treating this
// as an extensible replacement for the fixed input-state wire contract.
func BusDeviceNS2ProRuntimeStatusV1(s *usb.Server) api.HandlerFunc {
	return func(req *api.Request, res *api.Response, _ *slog.Logger) error {
		busIDText, ok := req.Params["busId"]
		if !ok {
			return apierror.ErrBadRequest("missing busId parameter")
		}
		deviceIDText, ok := req.Params["devId"]
		if !ok {
			return apierror.ErrBadRequest("missing devId parameter")
		}
		if req.Payload == "" {
			return apierror.ErrBadRequest("missing ns2pro status payload")
		}
		if len(req.Payload) > maximumNS2ProRuntimeStatusPayloadBytes {
			return apierror.ErrBadRequest("ns2pro status payload is too large")
		}
		busID, err := strconv.ParseUint(busIDText, 10, 32)
		if err != nil {
			return apierror.ErrBadRequest(fmt.Sprintf("invalid busId: %v", err))
		}
		bus := s.GetBus(uint32(busID))
		if bus == nil {
			return apierror.ErrNotFound(fmt.Sprintf("bus %d not found", busID))
		}
		deviceID, err := strconv.ParseUint(deviceIDText, 10, 32)
		if err != nil {
			return apierror.ErrBadRequest(fmt.Sprintf("invalid devId: %v", err))
		}
		dev, found := bus.GetDeviceByID(uint32(deviceID))
		if !found {
			return apierror.ErrNotFound(fmt.Sprintf(
				"device %s not found on bus %d", deviceIDText, busID))
		}
		statusDevice, supported := dev.(ns2ProRuntimeStatusV1Device)
		if !supported {
			return apierror.ErrBadRequest(fmt.Sprintf(
				"device %s does not expose ns2pro runtime status v1",
				deviceIDText))
		}
		if err := statusDevice.UpdateNS2ProRuntimeStatusV1(req.Payload); err != nil {
			return apierror.ErrBadRequest(fmt.Sprintf(
				"invalid ns2pro runtime status v1: %v", err))
		}
		res.JSON = `{"version":1,"updated":true}`
		return nil
	}
}

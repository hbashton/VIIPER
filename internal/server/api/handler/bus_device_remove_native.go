package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/Alia5/VIIPER/internal/server/api"
	apierror "github.com/Alia5/VIIPER/internal/server/api/error"
	"github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
	"github.com/Alia5/VIIPER/viipertypes"
)

// BusDeviceRemoveNative performs a correlation-conditioned native removal.
// The full add/list receipt is compared atomically by usb.Server immediately
// before Unregister, so a delayed lifetime cannot remove an ID-reusing child.
func BusDeviceRemoveNative(s *usb.Server) api.HandlerFunc {
	return func(req *api.Request, res *api.Response, _ *slog.Logger) error {
		idStr, ok := req.Params["id"]
		if !ok {
			return apierror.ErrBadRequest("missing id parameter")
		}
		busID64, err := strconv.ParseUint(idStr, 10, 32)
		if err != nil {
			return apierror.ErrBadRequest(fmt.Sprintf("invalid busId: %v", err))
		}
		if req.Payload == "" {
			return apierror.ErrBadRequest("missing payload")
		}

		var removeRequest viipertypes.NativeUDEDeviceRemoveRequest
		if err := json.Unmarshal([]byte(req.Payload), &removeRequest); err != nil {
			return apierror.ErrBadRequest(fmt.Sprintf("invalid JSON payload: %v", err))
		}
		if removeRequest.Transport != "native-ude" {
			return apierror.ErrBadRequest("transport must be exactly native-ude")
		}
		if removeRequest.NativeUDE == nil {
			return apierror.ErrBadRequest("missing nativeUde correlation receipt")
		}

		deviceID, err := parseCanonicalUint(removeRequest.DevID, 32)
		if err != nil || deviceID == 0 {
			return apierror.ErrBadRequest("devId must be a canonical nonzero uint32 decimal string")
		}
		nativeDeviceID, err := parseCanonicalUint(removeRequest.NativeUDE.DeviceID, 64)
		if err != nil || nativeDeviceID == 0 {
			return apierror.ErrBadRequest("nativeUde.deviceId must be a canonical nonzero uint64 decimal string")
		}
		controllerSessionID, err := parseCanonicalUint(removeRequest.NativeUDE.ControllerSessionID, 64)
		if err != nil || controllerSessionID == 0 {
			return apierror.ErrBadRequest("nativeUde.controllerSessionId must be a canonical nonzero uint64 decimal string")
		}

		expected := udecx.DeviceRegistration{
			DeviceIdentity: udecx.DeviceIdentity{
				DeviceID: nativeDeviceID, Generation: removeRequest.NativeUDE.DeviceGeneration,
			},
			ControllerSessionID:  controllerSessionID,
			ControllerInstanceID: removeRequest.NativeUDE.ControllerInstanceID,
			USB20PortNumber:      removeRequest.NativeUDE.USB20PortNumber,
			USB30PortNumber:      removeRequest.NativeUDE.USB30PortNumber,
		}
		if err := s.RemoveNativeDeviceExact(uint32(busID64), removeRequest.DevID, expected); err != nil {
			switch {
			case errors.Is(err, usb.ErrInvalidNativeDeviceCorrelation):
				return apierror.ErrBadRequest(err.Error())
			case errors.Is(err, usb.ErrNativeDeviceCorrelationMismatch):
				return apierror.ErrConflict("native device correlation is stale; no device was removed")
			case errors.Is(err, usb.ErrBusNotFound):
				return apierror.ErrNotFound(fmt.Sprintf("bus %d not found", busID64))
			default:
				return apierror.ErrInternal(fmt.Sprintf("failed to remove native device: %v", err))
			}
		}

		response, err := json.Marshal(viipertypes.DeviceRemoveResponse{
			BusID: uint32(busID64), DevID: removeRequest.DevID,
		})
		if err != nil {
			return apierror.ErrInternal(fmt.Sprintf("failed to marshal response: %v", err))
		}
		res.JSON = string(response)
		return nil
	}
}

func parseCanonicalUint(value string, bitSize int) (uint64, error) {
	parsed, err := strconv.ParseUint(value, 10, bitSize)
	if err != nil || strconv.FormatUint(parsed, 10) != value {
		return 0, fmt.Errorf("non-canonical unsigned decimal value")
	}
	return parsed, nil
}

package handler

import (
	"log/slog"
	"strconv"

	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/internal/server/api"
	apierror "github.com/Alia5/VIIPER/internal/server/api/error"
	usbs "github.com/Alia5/VIIPER/internal/server/usb"
)

// BusDeviceRemoveAuthorizedXboxOne consumes only a creation-time capability
// for the exact production Add incarnation. The path is a lookup hint, never
// removal authority. No fallback to numeric device or bus removal is allowed.
func BusDeviceRemoveAuthorizedXboxOne(s *usbs.Server) api.HandlerFunc {
	return func(req *api.Request, res *api.Response, _ *slog.Logger) error {
		if !req.Authenticated {
			return apierror.ErrUnauthorized("authenticated Xbox One removal session required")
		}
		busID, busErr := strconv.ParseUint(req.Params["busId"], 10, 16)
		deviceID, deviceErr := strconv.ParseUint(req.Params["devId"], 10, 16)
		if busErr != nil || deviceErr != nil || busID == 0 || deviceID == 0 {
			return apierror.ErrBadRequest("invalid Xbox One removal address")
		}
		token, err := decodeXboxOneRemovalRequest(req.Payload)
		if err != nil {
			return err
		}
		res.JSON = `{"version":1,"removed":false}`
		bus := s.GetBus(uint32(busID))
		if bus == nil {
			return nil
		}
		// Capture the exact registration once. After this point, all decisions
		// and mutation use this snapshot, even if topology changes underneath it.
		for _, registration := range bus.GetAllDeviceMetas() {
			if registration.Meta.DevID != uint32(deviceID) {
				continue
			}
			device, ok := registration.Dev.(*xboxone.AuthorizedDormantRetainedUSBDevice)
			if !ok || registration.Context == nil ||
				!device.AuthorizesProductionRemoval(registration.Context.Done(), token) {
				return nil
			}
			removed, err := s.CloseAndRemoveRetainedDeviceRegistrationIfPresent(registration)
			if err != nil {
				// Do not echo payloads or capability material into API errors/logs.
				return apierror.ErrConflict("exact Xbox One registration removal failed")
			}
			if removed {
				res.JSON = `{"version":1,"removed":true}`
			}
			return nil
		}
		return nil
	}
}

// Decode a closed, bounded version-one schema. encoding/json's normal struct
// decoder accepts duplicate and case-folded keys; neither is allowed here.
// Errors deliberately never contain the token or unknown user-supplied keys.
func decodeXboxOneRemovalRequest(payload string) (string, error) {
	token, err := xboxone.DecodeProductionRegistrationRequest(payload)
	if err != nil {
		return "", apierror.ErrBadRequest("invalid Xbox One removal payload")
	}
	return token, nil
}

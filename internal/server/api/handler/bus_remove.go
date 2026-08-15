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
	"github.com/Alia5/VIIPER/viipertypes"
)

// BusRemove returns a handler that removes a bus.
func BusRemove(s *usb.Server) api.HandlerFunc {
	return func(req *api.Request, res *api.Response, logger *slog.Logger) error {
		if req.Payload == "" {
			return apierror.ErrBadRequest("missing busId")
		}
		busID, err := strconv.ParseUint(req.Payload, 10, 32)
		if err != nil {
			return apierror.ErrBadRequest(fmt.Sprintf("invalid busId: %v", err))
		}
		remove := s.RemoveBus
		if s.NativeTransportEnabled() {
			remove = s.RemoveBusIfEmpty
		}
		if err := remove(uint32(busID)); err != nil {
			if errors.Is(err, usb.ErrBusNotEmpty) {
				return apierror.ErrConflict(
					"native transport refuses ID-only removal of a non-empty bus",
				)
			}
			return apierror.ErrNotFound(fmt.Sprintf("bus %d not found", busID))
		}
		out, err := json.Marshal(viipertypes.BusRemoveResponse{BusID: uint32(busID)})
		if err != nil {
			return apierror.ErrInternal(fmt.Sprintf("failed to marshal response: %v", err))
		}
		res.JSON = string(out)
		return nil
	}
}

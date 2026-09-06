package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/Alia5/VIIPER/internal/server/api"
	apierror "github.com/Alia5/VIIPER/internal/server/api/error"
	usbs "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/usbip"
)

type xboxOneActivationResponse struct {
	Version          uint16 `json:"version"`
	USBIPBusID       string `json:"usbipBusId"`
	USBIPPort        int32  `json:"usbipPort"`
	USBIPOwnerSerial string `json:"usbipOwnerSerial"`
}

// BusDeviceActivateAuthorizedXboxOne performs the second half of the closed
// production transaction. The persona exists but is not visible to Windows
// until its sole authenticated stream has installed the exact DS4Windows
// canonical-feedback consumer.
func BusDeviceActivateAuthorizedXboxOne(
	s *usbs.Server,
	apiSrv *api.Server,
) api.HandlerFunc {
	return func(req *api.Request, res *api.Response, logger *slog.Logger) (handlerErr error) {
		if !req.Authenticated {
			return apierror.ErrUnauthorized(
				"authenticated Xbox One activation session required")
		}
		if req.Ctx == nil || req.Ctx.Err() != nil {
			return apierror.ErrConflict("Xbox One activation context is unavailable or canceled")
		}
		admission, err := api.SelectAuthorizedXboxOneRegistration(s,
			req.Params["busId"], req.Params["devId"], req.Payload)
		if err != nil {
			return err
		}
		target := admission.Device()
		meta := admission.Registration().Meta
		alias, err := usbip.ExportBusID(meta)
		if err != nil || !usbip.ValidProductionXboxOneBusID(alias) {
			return apierror.ErrConflict("Xbox One export identity is unavailable")
		}
		if !apiSrv.Config().AutoAttachLocalClient {
			return apierror.ErrConflict(
				"VIIPER local auto-attach is required for Xbox One activation")
		}
		budget := apiSrv.Config().ConnectionTimeout
		if budget <= 0 {
			budget = 30 * time.Second
		}
		activationCtx, cancelActivation := context.WithTimeout(req.Ctx, budget)
		defer cancelActivation()
		stopRegistrationCancellation := context.AfterFunc(admission.Registration().Context, cancelActivation)
		defer stopRegistrationCancellation()
		if err := admission.Run(func() error {
			if activationCtx.Err() != nil {
				return apierror.ErrConflict("Xbox One activation was canceled before native submission")
			}
			if !target.TryBeginProductionBrokerActivation() {
				return apierror.ErrConflict("Xbox One feedback consumer is not ready or activation already ran")
			}
			return nil
		}); err != nil {
			return err
		}
		activated := false
		defer func() {
			defer target.CompleteProductionBrokerActivation(activated)
			if !activated && activationCtx.Err() != nil {
				// Native completion can win cancellation and return a port. Close
				// only this registration through the existing retained lifecycle,
				// including cancellation observed between native return and commit.
				res.JSON = ""
				handlerErr = apierror.ErrConflict("Xbox One activation canceled")
				if _, closeErr := s.CloseAndRemoveRetainedDeviceRegistrationIfPresent(admission.Registration()); closeErr != nil {
					handlerErr = apierror.ErrConflict("Xbox One activation canceled; exact registration cleanup is incomplete")
				}
			}
		}()
		result, err := attachLocalhostClientWithResult(
			activationCtx, &meta, s.GetListenPort(),
			apiSrv.Config().AutoAttachWindowsNative, logger)
		if activationCtx.Err() != nil {
			return apierror.ErrConflict("Xbox One activation canceled")
		}
		if err != nil {
			return apierror.ErrConflict(fmt.Sprintf(
				"failed to activate Xbox One persona: %v", err))
		}
		if !validXboxOneActivationAttachResult(result) {
			return apierror.ErrConflict(
				"Xbox One activation returned no positive usbip-win2 port")
		}
		// Native attach performs I/O and may reenter the import admission path.
		// Do not hold a registration read lease across it. Its immutable alias
		// cannot select a successor; authenticate again before reporting success.
		if err := admission.Run(func() error {
			if activationCtx.Err() != nil {
				return apierror.ErrConflict("Xbox One activation expired before commit")
			}
			if !target.CompleteProductionBrokerActivation(true) {
				return apierror.ErrConflict("Xbox One activation lost its feedback consumer")
			}
			return nil
		}); err != nil {
			return err
		}
		payload, err := json.Marshal(xboxOneActivationResponse{
			Version: 1, USBIPBusID: alias,
			USBIPPort:        result.USBIPPort,
			USBIPOwnerSerial: result.USBIPOwnerSerial,
		})
		if err != nil {
			return apierror.ErrInternal(fmt.Sprintf(
				"failed to marshal Xbox One activation: %v", err))
		}
		activated = true
		res.JSON = string(payload)
		return nil
	}
}

// usbip-win2 0.9.7.7's native attach IOCTL returns exactly the base size and
// assigned hub port; it has no attach-time serial override or owner-token
// field. The authenticated capability selects the exact registration; its
// separate public export alias survives automatic reconnect without selecting
// a same-numeric-address successor. A positive port is an attach result, never
// authority to detach that number later. Requiring a nonempty serial here
// would reject every real 0.9.7.7 native attach after it already succeeded.
func validXboxOneActivationAttachResult(result api.AutoAttachResult) bool {
	return result.USBIPPort > 0
}

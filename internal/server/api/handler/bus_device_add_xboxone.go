package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/internal/registry"
	"github.com/Alia5/VIIPER/internal/server/api"
	apierror "github.com/Alia5/VIIPER/internal/server/api/error"
	usbs "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/viipertypes"
)

var xboxOneHostMonotonicMicroseconds = controllerfeedback.HostMonotonicMicroseconds

// The removal capability belongs only to this authenticated create response.
// Do not add it to viipertypes.Device, whose values also appear in listings.
type xboxOneAuthorizedCreateResponse struct {
	viipertypes.Device
	RemovalToken               string `json:"removalToken"`
	USBIPBusID                 string `json:"usbipBusId"`
	RemovalTimeoutMilliseconds uint32 `json:"removalTimeoutMilliseconds"`
}

// BusDeviceAddAuthorizedXboxOne is the authenticated, closed production
// factory. Generic bus/{id}/add remains incapable of issuing an Xbox external
// identity authorization or reconstructing the canonical feedback executor.
func BusDeviceAddAuthorizedXboxOne(
	s *usbs.Server,
	apiSrv *api.Server,
) api.HandlerFunc {
	return func(req *api.Request, res *api.Response, _ *slog.Logger) error {
		if !req.Authenticated {
			return apierror.ErrUnauthorized(
				"authenticated Xbox One factory session required")
		}
		busID, err := parseXboxOneBusID(req)
		if err != nil {
			return err
		}
		if req.Payload == "" {
			return apierror.ErrBadRequest("missing payload")
		}
		var create viipertypes.XboxOneAuthorizedCreateRequestV1
		decoder := json.NewDecoder(strings.NewReader(req.Payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&create); err != nil {
			return apierror.ErrBadRequest(fmt.Sprintf(
				"invalid Xbox One factory payload: %v", err))
		}
		if err := requireXboxOneJSONEOF(decoder); err != nil {
			return apierror.ErrBadRequest(err.Error())
		}
		if create.Version != 1 {
			return apierror.ErrBadRequest(fmt.Sprintf(
				"unsupported Xbox One factory version: %d", create.Version))
		}
		if !create.IdentityAuthorizationGranted {
			return apierror.ErrBadRequest(
				"Xbox One external identity authorization was not granted")
		}
		authorityID, enabled := s.RetainedImportAuthorityID()
		if !enabled {
			return apierror.ErrConflict(
				"retained USB/IP imports are not enabled for this VIIPER server")
		}
		protocolTime, ok := xboxOneHostMonotonicMicroseconds()
		if !ok {
			return apierror.ErrInternal(
				"Windows QPC clock is unavailable for Xbox feedback fencing")
		}

		registration, err := registry.RegisterProductionXboxOneRetainedUSB(
			s,
			registry.ProductionXboxOneRetainedUSBRequest{
				BusID: busID,
				Options: xboxone.ProductionRetainedUSBDeviceOptions{
					Identity: xboxone.ControllerIdentity{
						VendorID:         create.Identity.VendorID,
						ProductID:        create.Identity.ProductID,
						DeviceReleaseBCD: create.Identity.DeviceReleaseBCD,
						DeviceID:         create.Identity.DeviceID,
						Firmware: xboxone.FirmwareVersion{
							Major:    create.Identity.FirmwareMajor,
							Minor:    create.Identity.FirmwareMinor,
							Build:    create.Identity.FirmwareBuild,
							Revision: create.Identity.FirmwareRevision,
						},
						HardwareMajor: create.Identity.HardwareMajor,
						HardwareMinor: create.Identity.HardwareMinor,
					},
					USB: xboxone.ControllerUSBConfig{
						MaxPower2mA:   create.USB.MaxPower2mA,
						OUTIntervalMS: create.USB.OUTIntervalMS,
						INIntervalMS:  create.USB.INIntervalMS,
					},
					Strings: xboxone.ControllerUSBIdentityStrings{
						Manufacturer: create.Strings.Manufacturer,
						Product:      create.Strings.Product,
						Serial:       create.Strings.Serial,
					},
					IdentityAuthorization: xboxone.ControllerIdentityAuthorizationGranted,
					FeedbackBinding: xboxone.ControllerPersonaFeedbackBindingV1{
						Source:                 controllerfeedback.Source(create.Feedback.Source),
						PersonaGeneration:      create.Feedback.PersonaGeneration,
						DeviceGeneration:       create.Feedback.DeviceGeneration,
						TransportGeneration:    create.Feedback.TransportGeneration,
						OwnershipEpoch:         create.Feedback.OwnershipEpoch,
						TimeToLiveMicroseconds: create.Feedback.TimeToLiveMicroseconds,
					},
					ProtocolTimeMS: protocolTime / 1_000,
					AuthorityID:    authorityID,
					ImportDeviceID: create.ImportDeviceID,
					LocalTimeout: time.Duration(
						create.LocalTimeoutMilliseconds) * time.Millisecond,
				},
			},
		)
		if err != nil {
			return apierror.ErrBadRequest(fmt.Sprintf(
				"failed to create authorized Xbox One persona: %v", err))
		}
		meta, active := registration.DeviceMeta()
		if !active {
			_ = registration.Close()
			return apierror.ErrConflict(
				"Xbox One registration changed during creation")
		}
		descriptor, err := s.SnapshotDeviceDescriptor(meta)
		if err != nil {
			_ = registration.Close()
			return apierror.ErrInternal(fmt.Sprintf(
				"failed to snapshot Xbox One descriptor: %v", err))
		}
		removalToken, hasRemovalToken := registration.ProductionRemovalToken()
		if !hasRemovalToken {
			_ = registration.Close()
			return apierror.ErrInternal("Xbox One removal capability is unavailable")
		}
		exportBusID, exportErr := usbip.ExportBusID(meta.Meta)
		removalTimeout, timeoutErr := s.ProductionRemovalTimeoutMilliseconds()
		if exportErr != nil || !usbip.ValidProductionXboxOneBusID(exportBusID) || timeoutErr != nil {
			_ = registration.Close()
			return apierror.ErrInternal("Xbox One registration lifetime receipt is unavailable")
		}
		apiSrv.ScheduleDeviceCleanup(meta)

		payload, err := json.Marshal(xboxOneAuthorizedCreateResponse{Device: viipertypes.Device{
			BusID:          busID,
			DevID:          fmt.Sprintf("%d", meta.Meta.DevID),
			Vid:            fmt.Sprintf("0x%04x", descriptor.Device.IDVendor),
			Pid:            fmt.Sprintf("0x%04x", descriptor.Device.IDProduct),
			Type:           "xboxone",
			DeviceSpecific: meta.Dev.GetDeviceSpecificArgs(),
			// The separate authenticated activation route attaches only
			// after the device stream has acknowledged ConsumerReady.
			USBIPPort:        0,
			USBIPOwnerSerial: "",
		}, RemovalToken: removalToken, USBIPBusID: exportBusID, RemovalTimeoutMilliseconds: removalTimeout})
		if err != nil {
			_ = registration.Close()
			return apierror.ErrInternal(fmt.Sprintf(
				"failed to marshal Xbox One response: %v", err))
		}
		res.JSON = string(payload)
		return nil
	}
}

func requireXboxOneJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("Xbox One factory payload has trailing JSON")
		}
		return fmt.Errorf("invalid trailing Xbox One factory JSON: %v", err)
	}
	return nil
}

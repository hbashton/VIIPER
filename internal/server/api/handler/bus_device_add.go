package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/internal/server/api"
	apierror "github.com/Alia5/VIIPER/internal/server/api/error"
	usbs "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
	"github.com/Alia5/VIIPER/viipertypes"
)

var attachLocalhostClientWithResult = api.AttachLocalhostClientWithResult

// BusDeviceAdd returns a handler to add devices to a bus.
func BusDeviceAdd(s *usbs.Server, apiSrv *api.Server) api.HandlerFunc {
	return func(req *api.Request, res *api.Response, logger *slog.Logger) error {
		idStr, ok := req.Params["id"]
		if !ok {
			return apierror.ErrBadRequest("missing id parameter")
		}
		busID, err := strconv.ParseUint(idStr, 10, 32)
		if err != nil {
			return apierror.ErrBadRequest(fmt.Sprintf("invalid busId: %v", err))
		}
		b := s.GetBus(uint32(busID))
		if b == nil {
			return apierror.ErrNotFound(fmt.Sprintf("bus %d not found", busID))
		}
		if req.Payload == "" {
			return apierror.ErrBadRequest("missing payload")
		}
		var deviceCreateReq viipertypes.DeviceCreateRequest
		err = json.Unmarshal([]byte(req.Payload), &deviceCreateReq)
		if err != nil {
			return apierror.ErrBadRequest(fmt.Sprintf("invalid JSON payload: %v", err))
		}
		if deviceCreateReq.Type == nil {
			return apierror.ErrBadRequest("missing device type")
		}

		name := strings.ToLower(*deviceCreateReq.Type)

		reg := api.GetRegistration(name)
		if reg == nil {
			return apierror.ErrBadRequest(fmt.Sprintf("unknown device type: %s", name))
		}

		opts := device.CreateOptions{
			IDVendor:  deviceCreateReq.IDVendor,
			IDProduct: deviceCreateReq.IDProduct,
		}
		if deviceCreateReq.DeviceSpecific != nil {
			b, err := json.Marshal(deviceCreateReq.DeviceSpecific)
			if err != nil {
				return apierror.ErrBadRequest(fmt.Sprintf("invalid deviceSpecific JSON: %v", err))
			}
			opts.DeviceSpecific = string(b)
		}

		dev, err := reg.CreateDevice(&opts)
		if err != nil {
			return apierror.ErrBadRequest(fmt.Sprintf("failed to create device: %v", err))
		}
		devCtx, nativeRegistration, err := s.AddDeviceToBusWithRegistration(req.Ctx, uint32(busID), dev)
		if err != nil {
			return apierror.ErrInternal(fmt.Sprintf("failed to add device to bus: %v", err))
		}

		exportMeta := device.GetDeviceMeta(devCtx)
		if exportMeta == nil {
			return apierror.ErrInternal("failed to get device metadata from context")
		}

		apiSrv.ScheduleDeviceCleanup(uint32(busID),
			fmt.Sprintf("%d", exportMeta.DevID), devCtx, nativeRegistration)

		autoAttachResult := api.AutoAttachResult{}
		if apiSrv.Config().AutoAttachLocalClient && !s.NativeTransportEnabled() {
			autoAttachResult, err = attachLocalhostClientWithResult(
				req.Ctx,
				exportMeta,
				s.GetListenPort(),
				apiSrv.Config().AutoAttachWindowsNative,
				logger,
			)
			if err != nil {
				logger.Error("failed to auto-attach localhost client", "error", err)
				return apierror.ErrConflict(fmt.Sprintf(
					"Failed to auto-attach device: %v", err,
				))
			}
		}

		transport := "usbip"
		var nativeInfo *viipertypes.NativeUDEDeviceInfo
		if nativeRegistration != nil {
			transport = "native-ude"
			nativeInfo = nativeUDEDeviceInfo(*nativeRegistration)
		}
		payload, err := json.Marshal(viipertypes.Device{
			BusID:            uint32(busID),
			DevID:            fmt.Sprintf("%d", exportMeta.DevID),
			Vid:              fmt.Sprintf("0x%04x", dev.GetDescriptor().Device.IDVendor),
			Pid:              fmt.Sprintf("0x%04x", dev.GetDescriptor().Device.IDProduct),
			Type:             name,
			DeviceSpecific:   dev.GetDeviceSpecificArgs(),
			Transport:        transport,
			NativeUDE:        nativeInfo,
			USBIPPort:        autoAttachResult.USBIPPort,
			USBIPOwnerSerial: autoAttachResult.USBIPOwnerSerial,
		})
		if err != nil {
			return apierror.ErrInternal(fmt.Sprintf("failed to marshal response: %v", err))
		}

		res.JSON = string(payload)
		return nil
	}
}

func nativeUDEDeviceInfo(registration udecx.DeviceRegistration) *viipertypes.NativeUDEDeviceInfo {
	return &viipertypes.NativeUDEDeviceInfo{
		DeviceID:             strconv.FormatUint(registration.DeviceID, 10),
		DeviceGeneration:     registration.Generation,
		ControllerSessionID:  strconv.FormatUint(registration.ControllerSessionID, 10),
		ControllerInstanceID: registration.ControllerInstanceID,
		USB20PortNumber:      registration.USB20PortNumber,
		USB30PortNumber:      registration.USB30PortNumber,
	}
}

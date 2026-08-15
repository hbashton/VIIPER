package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/Alia5/VIIPER/internal/server/api"
	apierror "github.com/Alia5/VIIPER/internal/server/api/error"
	"github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/viipertypes"
)

// BusDevicesList returns a handler that lists devices on a bus.
func BusDevicesList(s *usb.Server) api.HandlerFunc {
	return func(req *api.Request, res *api.Response, logger *slog.Logger) error {
		idStr, ok := req.Params["id"]
		if !ok {
			return apierror.ErrBadRequest("missing id parameter")
		}
		busID, err := strconv.ParseUint(idStr, 10, 32)
		if err != nil {
			return apierror.ErrBadRequest(fmt.Sprintf("invalid busId: %v", err))
		}
		snapshots, err := s.SnapshotBusDevices(uint32(busID))
		if err != nil {
			switch {
			case errors.Is(err, usb.ErrBusNotFound):
				return apierror.ErrNotFound(fmt.Sprintf("bus %d not found", busID))
			case errors.Is(err, usb.ErrNativeDeviceCorrelationMismatch):
				return apierror.ErrConflict("native bus topology changed during list")
			default:
				return apierror.ErrInternal(fmt.Sprintf("snapshot bus %d: %v", busID, err))
			}
		}
		out := make([]viipertypes.Device, 0, len(snapshots))
		for _, snapshot := range snapshots {
			m := snapshot.DeviceMeta
			dtype := inferDeviceType(m.Dev)
			transport := "usbip"
			var nativeInfo *viipertypes.NativeUDEDeviceInfo
			if snapshot.NativeRegistration != nil {
				transport = "native-ude"
				nativeInfo = nativeUDEDeviceInfo(*snapshot.NativeRegistration)
			}
			out = append(out, viipertypes.Device{
				BusID:          m.Meta.BusID,
				DevID:          fmt.Sprintf("%d", m.Meta.DevID),
				Vid:            fmt.Sprintf("0x%04x", m.Dev.GetDescriptor().Device.IDVendor),
				Pid:            fmt.Sprintf("0x%04x", m.Dev.GetDescriptor().Device.IDProduct),
				Type:           dtype,
				DeviceSpecific: m.Dev.GetDeviceSpecificArgs(),
				Transport:      transport,
				NativeUDE:      nativeInfo,
			})
		}
		payload, err := json.Marshal(viipertypes.DevicesListResponse{Devices: out})
		if err != nil {
			return apierror.ErrInternal(fmt.Sprintf("failed to marshal response: %v", err))
		}
		res.JSON = string(payload)
		return nil
	}
}

// inferDeviceType attempts to derive a friendly device type name from the concrete type.
// For devices under /devices/<name>, we return the last path element (e.g., "xbox360").
// Fallback to the lowercased concrete type name if the package path is unavailable.
func inferDeviceType(dev any) string {
	if dev == nil {
		return ""
	}
	if typed, ok := dev.(interface{ VIIPERDeviceType() string }); ok {
		if deviceType := strings.ToLower(typed.VIIPERDeviceType()); deviceType != "" {
			return deviceType
		}
	}
	t := reflect.TypeOf(dev)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	pkg := t.PkgPath() // e.g., "github.com/Alia5/VIIPER/device/xbox360"
	if pkg != "" {
		base := filepath.Base(pkg)
		if base != "." && base != string(filepath.Separator) {
			return strings.ToLower(base)
		}
	}
	return strings.ToLower(t.Name())
}

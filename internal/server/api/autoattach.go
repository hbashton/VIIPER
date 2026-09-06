package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Alia5/VIIPER/usbip"
)

// AutoAttachResult identifies the USB/IP import created by auto-attach.
// Command-based attach implementations cannot reliably discover this metadata
// without a global port scan, so they return the zero value.
type AutoAttachResult struct {
	USBIPPort        int32
	USBIPOwnerSerial string
}

func AttachLocalhostClient(ctx context.Context, deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, useNativeIOCTL bool, logger *slog.Logger) error {
	_, err := AttachLocalhostClientWithResult(ctx, deviceExportMeta, usbipServerPort, useNativeIOCTL, logger)
	return err
}

// AttachLocalhostClientWithResult attaches a device and returns the exact
// USB/IP import metadata when the platform attach mechanism provides it.
func AttachLocalhostClientWithResult(ctx context.Context, deviceExportMeta *usbip.ExportMeta, usbipServerPort uint16, useNativeIOCTL bool, logger *slog.Logger) (AutoAttachResult, error) {
	if ctx != nil {
		if cleanup, ok := ctx.Value(xboxOneRetryContextKey{}).(xboxOneRetryContext); ok {
			if cleanup.server == nil || deviceExportMeta == nil ||
				*deviceExportMeta != cleanup.registration.Meta || usbipServerPort != cleanup.server.usbs.GetListenPort() {
				return AutoAttachResult{}, fmt.Errorf("native attach does not match its cleanup registration")
			}
			finish, err := cleanup.server.ArmXboxOneRetryCleanup(cleanup.registration)
			if err != nil {
				return AutoAttachResult{}, err
			}
			defer finish()
		}
	}
	return attachLocalhostClientImpl(ctx, deviceExportMeta, usbipServerPort, useNativeIOCTL, logger)
}

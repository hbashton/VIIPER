package api

import (
	"context"
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
	return attachLocalhostClientImpl(ctx, deviceExportMeta, usbipServerPort, useNativeIOCTL, logger)
}

//go:build !windows

package api

import (
	"context"
	"github.com/Alia5/VIIPER/usbip"
)

// The usbip-win2 retry queue is Windows-specific.
func stopLocalhostXboxOneRetries(context.Context, usbip.ExportMeta, uint16) (int32, error) {
	return 0, nil
}

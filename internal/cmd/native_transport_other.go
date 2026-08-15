//go:build !windows

package cmd

import (
	"context"
	"errors"

	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
)

func startNativeUDETransport(context.Context, *serverusb.Server) (nativeUDETransport, error) {
	return nil, errors.New("native UDE transport is available only on Windows")
}

//go:build !windows

package cmd

import (
	"context"
	"errors"
	"log/slog"
)

func recoverNativePackage(
	context.Context,
	*slog.Logger,
	nativePackageRecoverRequest,
) error {
	return errors.New("native package journal recovery is available only on Windows")
}

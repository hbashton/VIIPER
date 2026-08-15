//go:build !windows

package cmd

import (
	"context"
	"errors"
	"log/slog"
)

func installNativePackage(context.Context, *slog.Logger, nativePackageRequest) error {
	return errors.New("native UDE package installation is supported only on Windows")
}

func commitNativePackageBroker(
	*slog.Logger, string, string, string, string, string, bool,
) (nativePackageBrokerCommitResult, error) {
	return nativePackageBrokerPreflightFailure(
		errors.New("native UDE package installation is supported only on Windows"),
	)
}

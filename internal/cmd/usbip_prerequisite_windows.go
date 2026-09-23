//go:build windows

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	requiredUSBIPVersion  = "0.9.7.7"
	usbipProbeTimeout     = 10 * time.Second
	usbipPipeDrainTimeout = 250 * time.Millisecond
)

type usbipCommandRunner func(context.Context, string, ...string) ([]byte, error)

func requireUSBIPRuntime() error {
	return requireUSBIPRuntimeContext(context.Background())
}

func requireUSBIPRuntimeContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	usbipPath, err := canonicalUSBIPExecutable()
	if err != nil {
		return startupFailure(StartupUSBIPUnavailable, err)
	}

	return probeUSBIPRuntimeContext(ctx, usbipPath, runUSBIPCommand)
}

func canonicalUSBIPExecutable() (string, error) {
	programFiles := strings.TrimSpace(os.Getenv("ProgramW6432"))
	if programFiles == "" {
		programFiles = strings.TrimSpace(os.Getenv("ProgramFiles"))
	}
	if programFiles == "" {
		return "", fmt.Errorf("USB/IP prerequisite failed: Windows Program Files directory is unavailable")
	}

	usbipPath := filepath.Join(programFiles, "USBip", "usbip.exe")
	info, err := os.Stat(usbipPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf(
				"USB/IP prerequisite failed: usbip-win2 %s is not installed at %s; run the DS4Windows VIIPER setup",
				requiredUSBIPVersion,
				usbipPath,
			)
		}
		return "", fmt.Errorf("USB/IP prerequisite failed: cannot inspect %s: %w", usbipPath, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("USB/IP prerequisite failed: %s is not an executable file", usbipPath)
	}

	return usbipPath, nil
}

func runUSBIPCommand(ctx context.Context, executable string, args ...string) ([]byte, error) {
	return usbipCommand(ctx, executable, args...).CombinedOutput()
}

func usbipCommand(ctx context.Context, executable string, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, executable, args...)
	// CommandContext bounds the process, but CombinedOutput also joins its
	// redirected pipe readers. A descendant retaining a pipe handle must not
	// keep prerequisite startup alive after that process exits or times out.
	command.WaitDelay = usbipPipeDrainTimeout
	return command
}

func probeUSBIPRuntime(usbipPath string, run usbipCommandRunner) error {
	return probeUSBIPRuntimeContext(context.Background(), usbipPath, run)
}

func probeUSBIPRuntimeContext(ctx context.Context, usbipPath string, run usbipCommandRunner) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	versionCtx, cancelVersion := context.WithTimeout(ctx, usbipProbeTimeout)
	versionOutput, versionErr := run(versionCtx, usbipPath, "--version")
	versionTimedOut := usbipCommandTimedOut(versionCtx, versionErr)
	cancelVersion()
	if err := ctx.Err(); err != nil {
		return err // Owner cancellation is not an incompatible driver.
	}

	if versionTimedOut {
		return startupFailure(StartupUSBIPTimeout,
			fmt.Errorf("USB/IP prerequisite failed: %s --version timed out", usbipPath))
	}
	if versionErr != nil {
		return startupFailure(StartupUSBIPUnavailable, fmt.Errorf(
			"USB/IP prerequisite failed: cannot query %s version: %w%s",
			usbipPath,
			versionErr,
			formatUSBIPOutput(versionOutput),
		))
	}

	installedVersion := strings.TrimSpace(string(versionOutput))
	if installedVersion != requiredUSBIPVersion {
		if installedVersion == "" {
			installedVersion = "unknown"
		}
		return startupFailure(StartupUSBIPVersion, fmt.Errorf(
			"USB/IP prerequisite failed: VIIPER requires usbip-win2 %s at %s (found %s); run the DS4Windows VIIPER setup",
			requiredUSBIPVersion,
			usbipPath,
			installedVersion,
		))
	}

	portCtx, cancelPort := context.WithTimeout(ctx, usbipProbeTimeout)
	portOutput, portErr := run(portCtx, usbipPath, "port")
	portTimedOut := usbipCommandTimedOut(portCtx, portErr)
	cancelPort()
	if err := ctx.Err(); err != nil {
		return err
	}

	if portTimedOut {
		return startupFailure(StartupUSBIPTimeout,
			fmt.Errorf("USB/IP prerequisite failed: %s port timed out", usbipPath))
	}
	if portErr != nil {
		return startupFailure(StartupUSBIPDriver, fmt.Errorf(
			"USB/IP prerequisite failed: usbip-win2 %s driver/CLI probe failed: %w%s",
			requiredUSBIPVersion,
			portErr,
			formatUSBIPOutput(portOutput),
		))
	}
	if reason := usbipProbeFailure(portOutput); reason != "" {
		return startupFailure(StartupUSBIPDriver, fmt.Errorf(
			"USB/IP prerequisite failed: usbip-win2 %s driver/CLI probe reported %s; repair USBIP and reboot before starting VIIPER",
			requiredUSBIPVersion,
			reason,
		))
	}

	return nil
}

func usbipCommandTimedOut(ctx context.Context, err error) bool {
	return ctx.Err() == context.DeadlineExceeded ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, exec.ErrWaitDelay)
}

func usbipProbeFailure(output []byte) string {
	text := strings.ToLower(strings.TrimSpace(string(output)))
	for _, marker := range []string{
		"abi mismatch",
		"unexpected size",
		"specified conversion is not valid",
		"invalid structure size",
	} {
		if strings.Contains(text, marker) {
			return marker
		}
	}
	return ""
}

func formatUSBIPOutput(output []byte) string {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return ""
	}
	return ": " + text
}

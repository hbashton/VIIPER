//go:build windows

package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	requiredUSBIPVersion = "0.9.7.7"
	usbipProbeTimeout    = 10 * time.Second
)

type usbipCommandRunner func(context.Context, string, ...string) ([]byte, error)

func requireUSBIPRuntime() error {
	usbipPath, err := canonicalUSBIPExecutable()
	if err != nil {
		return err
	}

	return probeUSBIPRuntime(usbipPath, runUSBIPCommand)
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
	return exec.CommandContext(ctx, executable, args...).CombinedOutput()
}

func probeUSBIPRuntime(usbipPath string, run usbipCommandRunner) error {
	versionCtx, cancelVersion := context.WithTimeout(context.Background(), usbipProbeTimeout)
	versionOutput, versionErr := run(versionCtx, usbipPath, "--version")
	versionTimedOut := versionCtx.Err() == context.DeadlineExceeded
	cancelVersion()

	if versionTimedOut {
		return fmt.Errorf("USB/IP prerequisite failed: %s --version timed out", usbipPath)
	}
	if versionErr != nil {
		return fmt.Errorf(
			"USB/IP prerequisite failed: cannot query %s version: %w%s",
			usbipPath,
			versionErr,
			formatUSBIPOutput(versionOutput),
		)
	}

	installedVersion := strings.TrimSpace(string(versionOutput))
	if installedVersion != requiredUSBIPVersion {
		if installedVersion == "" {
			installedVersion = "unknown"
		}
		return fmt.Errorf(
			"USB/IP prerequisite failed: VIIPER requires usbip-win2 %s at %s (found %s); run the DS4Windows VIIPER setup",
			requiredUSBIPVersion,
			usbipPath,
			installedVersion,
		)
	}

	portCtx, cancelPort := context.WithTimeout(context.Background(), usbipProbeTimeout)
	portOutput, portErr := run(portCtx, usbipPath, "port")
	portTimedOut := portCtx.Err() == context.DeadlineExceeded
	cancelPort()

	if portTimedOut {
		return fmt.Errorf("USB/IP prerequisite failed: %s port timed out", usbipPath)
	}
	if portErr != nil {
		return fmt.Errorf(
			"USB/IP prerequisite failed: usbip-win2 %s driver/CLI probe failed: %w%s",
			requiredUSBIPVersion,
			portErr,
			formatUSBIPOutput(portOutput),
		)
	}
	if reason := usbipProbeFailure(portOutput); reason != "" {
		return fmt.Errorf(
			"USB/IP prerequisite failed: usbip-win2 %s driver/CLI probe reported %s; repair USBIP and reboot before starting VIIPER",
			requiredUSBIPVersion,
			reason,
		)
	}

	return nil
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

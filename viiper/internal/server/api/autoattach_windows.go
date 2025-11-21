//go:build windows

package api

import (
	"log/slog"
	"os/exec"
)

// CheckAutoAttachPrerequisites checks if auto-attach prerequisites are met on Windows.
// Returns true if all requirements are satisfied, false otherwise with helpful log messages.
func CheckAutoAttachPrerequisites(logger *slog.Logger) bool {
	// Check if usbip.exe is available (from usbip-win2)
	if _, err := exec.LookPath("usbip.exe"); err != nil {
		logger.Warn("USB/IP tool 'usbip.exe' not found in PATH")
		logger.Warn("Auto-attach requires usbip-win2")
		logger.Info("Download and install usbip-win2:")
		logger.Info("  https://github.com/vadimgrn/usbip-win2")
		return false
	}

	logger.Debug("usbip.exe tool found in PATH")
	return true
}

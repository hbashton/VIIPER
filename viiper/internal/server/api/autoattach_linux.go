//go:build linux

package api

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
)

// CheckAutoAttachPrerequisites checks if auto-attach prerequisites are met on Linux.
// Returns true if all requirements are satisfied, false otherwise with helpful log messages.
func CheckAutoAttachPrerequisites(logger *slog.Logger) bool {
	allOk := true

	// Check if usbip tool is available
	if _, err := exec.LookPath("usbip"); err != nil {
		logger.Warn("USB/IP tool 'usbip' not found in PATH")
		logger.Warn("Auto-attach requires the usbip command-line tool")
		logger.Info("Install usbip:")
		logger.Info("  Ubuntu/Debian: sudo apt install linux-tools-generic")
		logger.Info("  Arch Linux:    sudo pacman -S usbip")
		allOk = false
	} else {
		logger.Debug("usbip tool found in PATH")
	}

	// Check if vhci-hcd kernel module is loaded
	data, err := os.ReadFile("/proc/modules")
	if err != nil {
		logger.Debug("Could not read /proc/modules", "error", err)
		// Don't fail here, might be in a container or restricted environment
	} else if !bytes.Contains(data, []byte("vhci_hcd")) {
		logger.Warn("USB/IP kernel module 'vhci-hcd' is not loaded")
		logger.Warn("Auto-attach will not work until the module is loaded")
		logger.Info("To load the module now, run in another terminal:")
		logger.Info("  sudo modprobe vhci-hcd")
		logger.Info("")
		logger.Info("To automatically load at boot:")
		logger.Info("  echo 'vhci-hcd' | sudo tee /etc/modules-load.d/viiper.conf")
		allOk = false
	} else {
		logger.Debug("vhci-hcd kernel module is loaded")
	}

	return allOk
}

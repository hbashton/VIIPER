package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Install sets up VIIPER to run automatically. On Windows native-ude uses an
// SCM-owned LocalSystem broker; the legacy usbip developer path retains its
// historical per-user startup registration.
type Install struct {
	Transport     string `help:"Virtual USB transport to register: usbip or native-ude." default:"usbip"`
	TargetUserSID string `help:"Interactive Windows user SID that owns DS4Windows startup state." hidden:""`
}

// Uninstall removes VIIPER's platform-owned service/startup state. Production
// Windows packages also remove their exact native devnode and Driver Store package.
type Uninstall struct {
	Yes                  bool   `help:"Confirm removal without prompting." short:"y"`
	TargetUserSID        string `help:"Interactive Windows user SID that owns VIIPER startup state." hidden:""`
	DriverHelper         string `help:"Path to the packaged ViiperUdeCtl.exe used for exact native package removal." hidden:""`
	ExpectedHelperSHA256 string `help:"Installer-embedded SHA-256 of ViiperUdeCtl.exe used for exact native package removal." hidden:""`
}

func (c *Install) Run(logger *slog.Logger) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	if strings.Contains(exe, "go-build") {
		return errors.New("cannot install from 'go run'")
	}

	transport := strings.ToLower(strings.TrimSpace(c.Transport))
	if transport != "usbip" && transport != "native-ude" {
		return fmt.Errorf("unsupported VIIPER transport %q (expected usbip or native-ude)", c.Transport)
	}

	return install(logger, transport, strings.TrimSpace(c.TargetUserSID))
}

func (c *Uninstall) Run(logger *slog.Logger) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	if strings.Contains(exe, "go-build") {
		return errors.New("cannot uninstall from 'go run'")
	}

	if !c.Yes {
		fmt.Print("Remove VIIPER's installed service/startup ownership and any exact native device/driver package managed by this installation? [y/N]: ")
		answer, readErr := bufio.NewReader(os.Stdin).ReadString('\n')
		if readErr != nil && len(answer) == 0 {
			return fmt.Errorf("could not read uninstall confirmation: %w", readErr)
		}
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println("Uninstall canceled. No changes were made.")
			return nil
		}
	}

	return uninstall(
		logger,
		strings.TrimSpace(c.TargetUserSID),
		strings.TrimSpace(c.DriverHelper),
		strings.ToLower(strings.TrimSpace(c.ExpectedHelperSHA256)),
	)
}

func currentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}

	return filepath.Abs(exe)
}

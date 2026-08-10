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

// Install sets up VIIPER to run automatically.
type Install struct {
	Transport string `help:"Virtual USB transport to register: usbip or native-ude." default:"usbip"`
}

// Uninstall removes VIIPER startup configuration.
type Uninstall struct {
	Yes bool `help:"Confirm removal without prompting." short:"y"`
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

	return install(logger, transport)
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
		fmt.Print("Remove VIIPER startup registration and stop its server? [y/N]: ")
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

	return uninstall(logger)
}

func currentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}

	return filepath.Abs(exe)
}

//go:build windows

package tray

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/Alia5/VIIPER/internal/codegen/common"
	"golang.org/x/sys/windows/registry"
)

//go:embed icon.ico
var trayIcon []byte

const (
	runKeyPath  = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueKey = "VIIPER"
)

func Run(ctx context.Context, shutdown func()) func() {
	ready := make(chan struct{})
	stopped := make(chan struct{})
	var readyOnce sync.Once
	var stoppedOnce sync.Once

	go func() {
		// The Win32 window and its message pump must remain on the same OS
		// thread. A movable goroutine can intermittently lose menu input.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		systray.Run(func() {

			systray.SetIcon(trayIcon)
			systray.SetTooltip("VIIPER")

			version := readVersion()
			infoStr := fmt.Sprintf("VIIPER - %s", version)
			versionItem := systray.AddMenuItem(infoStr, infoStr)
			versionItem.Disable()

			systray.AddSeparator()

			autoStartItem := systray.AddMenuItemCheckbox("Run at startup", "", autoStartEnabled())

			systray.AddSeparator()

			exitItem := systray.AddMenuItem("Quit", "Exit VIIPER")
			readyOnce.Do(func() { close(ready) })

			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-autoStartItem.ClickedCh:
						if toggleAutoStart() {
							autoStartItem.Check()
						} else {
							autoStartItem.Uncheck()
						}
					case <-exitItem.ClickedCh:
						shutdown()
						return
					}
				}
			}()

		}, func() {
			stoppedOnce.Do(func() { close(stopped) })
		})
	}()

	go func() {
		<-ctx.Done()
		select {
		case <-ready:
			systray.Quit()
		case <-stopped:
		}
	}()

	return func() {
		select {
		case <-ready:
			systray.Quit()
		case <-stopped:
			return
		case <-time.After(2 * time.Second):
			return
		}

		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
		}
	}
}

func readVersion() string {
	// Release builds inject this value explicitly. debug.ReadBuildInfo reports
	// "(devel)" for a binary built directly from a checkout, even when the
	// release linker metadata is present, which previously made the tray show
	// "VIIPER - dev" for packaged 0.0.6 builds.
	if version, err := common.GetVersion(); err == nil &&
		version != "0.0.1-dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		v := info.Main.Version
		if v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

func autoStartEnabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close() //nolint:errcheck
	_, _, err = key.GetStringValue(runValueKey)
	return err == nil
}

func toggleAutoStart() bool {
	if autoStartEnabled() {
		key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
		if err != nil {
			slog.Error("Failed to open registry key", "error", err)
			return true
		}
		defer key.Close() //nolint:errcheck
		_ = key.DeleteValue(runValueKey)
		slog.Info("Auto-start disabled")
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		slog.Error("Failed to get executable path", "error", err)
		return false
	}
	selfPath, err := filepath.EvalSymlinks(exe)
	if err != nil {
		slog.Error("Failed to evaluate symlinks", "error", err)
		return false
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.ALL_ACCESS)
	if err != nil {
		slog.Error("Failed to create registry key", "error", err)
		return false
	}
	defer key.Close() //nolint:errcheck
	value := fmt.Sprintf("\"%s\" server", selfPath)
	if err := key.SetStringValue(runValueKey, value); err != nil {
		slog.Error("Failed to set registry value", "error", err)
		return false
	}
	slog.Info("Auto-start enabled")
	return true
}

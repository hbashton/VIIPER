//go:build windows

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Alia5/VIIPER/internal/configpaths"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	runKeyPath       = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueKey      = "VIIPER"
	runScheduledTask = "RunVIIPER"
)

func install(logger *slog.Logger, transport, targetUserSID string) error {
	if transport == "native-ude" {
		release, err := acquireNamedNativePackageMutex(
			nativePackageMutexName, nativePackageTransactionTimeout,
		)
		if err != nil {
			return err
		}
		defer release()
		return installNativeBroker(logger, targetUserSID)
	}
	if targetUserSID != "" {
		return errors.New("--target-user-sid is valid only with --transport native-ude")
	}
	if os.Getenv("VIIPER_DEVELOPER_STANDALONE") != "1" {
		return errors.New("standalone VIIPER startup registration is developer-only on Windows; use the signed DS4Windows installer or its built-in VIIPER repair so one verified owner manages VIIPER and USB-IP")
	}
	release, err := acquireNativeInstallMutex(nativeServiceInstallTimeout)
	if err != nil {
		return err
	}
	defer release()
	if transport == "usbip" {
		if err := requireUSBIPRuntime(); err != nil {
			return err
		}
	}
	scheduledExe, err := currentScheduledTaskExe()
	if err != nil {
		return fmt.Errorf("failed to inspect legacy %s scheduled task: %w", runScheduledTask, err)
	}
	if err := removeScheduledTask(); err != nil {
		return fmt.Errorf("failed to remove legacy %s scheduled task: %w", runScheduledTask, err)
	}
	if scheduledExe != "" {
		if err := killProcessesByExe(scheduledExe, logger); err != nil {
			return fmt.Errorf("failed to stop legacy scheduled VIIPER instance: %w", err)
		}
	}

	exePath, err := currentExecutable()
	if err != nil {
		return err
	}

	previousExe, err := currentAutorunExe()
	if err != nil {
		return err
	}

	cfgDir, err := configpaths.DefaultConfigDir()
	if err != nil {
		return fmt.Errorf("failed to resolve config dir: %w", err)
	}
	logFile := filepath.Join(cfgDir, "viiper.log")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return fmt.Errorf("failed to create log directory %s: %w", cfgDir, err)
	}

	if previousExe != "" {
		if err := killProcessesByExe(previousExe, logger); err != nil {
			return fmt.Errorf("failed to stop previous autorun instance: %w", err)
		}
	}
	value := windowsAutorunCommand(exePath, transport, logFile)
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.ALL_ACCESS)
	if err != nil {
		return err
	}
	defer key.Close() //nolint:errcheck

	if err := key.SetStringValue(runValueKey, value); err != nil {
		return err
	}

	if err := exec.Command(exePath, serverArguments(transport, logFile)...).Start(); err != nil {
		return fmt.Errorf("failed to start server: %w", err)
	}

	logger.Info("VIIPER install completed for Windows autorun", "exe", exePath,
		"transport", transport, "logFile", logFile)
	return nil
}

func serverArguments(transport, logFile string) []string {
	return []string{"server", "--transport", transport, "--log.file", logFile}
}

func windowsAutorunCommand(exePath, transport, logFile string) string {
	return fmt.Sprintf("\"%s\" server --transport %s --log.file \"%s\"",
		exePath, transport, logFile)
}

func requireNativeUDEBroker() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := udecx.Open(ctx)
	if err != nil {
		return fmt.Errorf("native UDE driver preflight failed without changing autorun: %w", err)
	}
	if err := client.Close(); err != nil {
		return fmt.Errorf("native UDE driver preflight close failed without changing autorun: %w", err)
	}
	return nil
}

func uninstall(
	logger *slog.Logger,
	targetUserSID, driverHelper, expectedHelperSHA256 string,
) error {
	request := nativePackageUninstallRequest{
		driverHelper: driverHelper, expectedHelperSHA256: expectedHelperSHA256,
		targetUserSID: targetUserSID,
	}
	if err := request.validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativePackageTransactionTimeout)
	defer cancel()
	return uninstallNativePackage(ctx, logger, request)
}

func currentScheduledTaskExe() (string, error) {
	// Enumerate the exact root task and fail closed on provider errors. A
	// targeted Get-ScheduledTask call with SilentlyContinue cannot distinguish
	// "not found" from an unavailable or access-denied Task Scheduler provider.
	script := `$ErrorActionPreference='Stop';$m=@(Get-ScheduledTask -ErrorAction Stop|Where-Object{$_.TaskPath -ceq '\' -and $_.TaskName -ieq 'RunVIIPER'});if($m.Count -eq 0){exit 0};if($m.Count -ne 1){throw 'expected zero or one root RunVIIPER task'};$a=@($m[0].Actions);if($a.Count -ne 1){throw 'scheduled task must contain exactly one action'};$a[0].Execute`
	powershell, err := trustedSystemExecutable("WindowsPowerShell", "v1.0", "powershell.exe")
	if err != nil {
		return "", fmt.Errorf("resolve trusted PowerShell: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeServiceInstallTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("scheduled task query timed out: %w", ctx.Err())
		}
		return "", fmt.Errorf("scheduled task query failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	path := strings.Trim(strings.TrimSpace(string(output)), `"`)
	if path == "" {
		return "", nil
	}
	path = filepath.Clean(path)
	if !strings.EqualFold(filepath.Base(path), "viiper.exe") {
		return "", fmt.Errorf("%s action is not a VIIPER executable: %s", runScheduledTask, path)
	}
	return path, nil
}

func removeScheduledTask() error {
	// Get-ScheduledTask makes absence distinguishable from an access-denied
	// deletion. Never report uninstall success while a highest-privilege task
	// can silently start VIIPER again at the next logon.
	script := `$ErrorActionPreference='Stop';$m=@(Get-ScheduledTask -ErrorAction Stop|Where-Object{$_.TaskPath -ceq '\' -and $_.TaskName -ieq 'RunVIIPER'});if($m.Count -eq 0){exit 0};if($m.Count -ne 1){throw 'expected exactly one root RunVIIPER task'};Unregister-ScheduledTask -TaskName $m[0].TaskName -TaskPath '\' -Confirm:$false -ErrorAction Stop;$after=@(Get-ScheduledTask -ErrorAction Stop|Where-Object{$_.TaskPath -ceq '\' -and $_.TaskName -ieq 'RunVIIPER'});if($after.Count -ne 0){throw 'scheduled task still exists'}`
	powershell, err := trustedSystemExecutable("WindowsPowerShell", "v1.0", "powershell.exe")
	if err != nil {
		return fmt.Errorf("resolve trusted PowerShell: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeServiceInstallTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("scheduled task removal timed out: %w", ctx.Err())
		}
		return fmt.Errorf("scheduled task removal failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func currentAutorunExe() (string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer key.Close() //nolint:errcheck

	val, _, err := key.GetStringValue(runValueKey)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", nil
		}
		return "", err
	}

	trimmed := strings.TrimSpace(val)
	if trimmed == "" {
		return "", nil
	}

	if strings.HasPrefix(trimmed, "\"") {
		trimmed = strings.TrimPrefix(trimmed, "\"")
		if end := strings.Index(trimmed, "\""); end >= 0 {
			trimmed = trimmed[:end]
		}
	}

	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", nil
	}

	path := fields[0]
	if path == "" {
		return "", nil
	}
	return filepath.Clean(path), nil
}

func killProcessesByExe(target string, logger *slog.Logger) error {
	target = filepath.Clean(target)
	if target == "" {
		return nil
	}

	script := fmt.Sprintf(
		"$ErrorActionPreference='SilentlyContinue';$t='%s';Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -eq $t } | Select-Object -ExpandProperty ProcessId",
		strings.ReplaceAll(target, "'", "''"),
	)
	powershell, err := trustedSystemExecutable("WindowsPowerShell", "v1.0", "powershell.exe")
	if err != nil {
		return fmt.Errorf("resolve trusted PowerShell: %w", err)
	}
	cmd := exec.Command(powershell, "-NoProfile", "-Command", script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("process query failed: %w: %s", err, strings.TrimSpace(string(output)))
	}

	scanner := bufio.NewScanner(bytes.NewReader(output))
	var pids []int
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err == nil {
			pids = append(pids, pid)
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	if len(pids) == 0 {
		return nil
	}

	self := os.Getpid()
	for _, pid := range pids {
		if pid == self {
			continue
		}
		taskkill, pathErr := trustedSystemExecutable("taskkill.exe")
		if pathErr != nil {
			return fmt.Errorf("resolve trusted taskkill: %w", pathErr)
		}
		cmd := exec.Command(taskkill, "/PID", strconv.Itoa(pid), "/T", "/F")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("taskkill pid %d failed: %w: %s", pid, err, strings.TrimSpace(string(output)))
		}
		logger.Info("terminated autorun instance", "pid", pid)
	}

	return nil
}

func trustedSystemExecutable(relativeParts ...string) (string, error) {
	if len(relativeParts) == 0 {
		return "", errors.New("trusted system executable path is empty")
	}
	for _, part := range relativeParts {
		if part == "" || part == "." || part == ".." || filepath.Base(part) != part {
			return "", fmt.Errorf("invalid trusted system path component %q", part)
		}
	}
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	current := filepath.Clean(systemDirectory)
	root, err := openNativePathWithoutReparse(current, windows.FILE_READ_ATTRIBUTES, true)
	if err != nil {
		return "", fmt.Errorf("open Windows system directory: %w", err)
	}
	windows.CloseHandle(root) //nolint:errcheck
	for index, part := range relativeParts {
		current = filepath.Join(current, part)
		isDirectory := index < len(relativeParts)-1
		handle, err := openNativePathWithoutReparse(current, windows.FILE_READ_ATTRIBUTES, isDirectory)
		if err != nil {
			return "", fmt.Errorf("open trusted system path %s: %w", current, err)
		}
		windows.CloseHandle(handle) //nolint:errcheck
	}
	return current, nil
}

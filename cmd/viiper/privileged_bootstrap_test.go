package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Alia5/VIIPER/internal/config"
	"github.com/alecthomas/kong"
)

func TestClassifyBootstrapSealsPrivilegedLifecycleCommands(t *testing.T) {
	t.Parallel()

	for _, command := range []string{
		"install",
		"uninstall",
		"native-package-install",
		"native-package-broker-commit",
		"native-package-recover",
	} {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			mode, err := classifyBootstrap([]string{"--help", command})
			if err != nil {
				t.Fatalf("classifyBootstrap() error = %v", err)
			}
			if mode != bootstrapPrivilegedLifecycle {
				t.Fatalf("classifyBootstrap() mode = %v, want privileged", mode)
			}
		})
	}
}

func TestClassifyBootstrapRejectsPrivilegedGlobalInjection(t *testing.T) {
	t.Parallel()

	tests := [][]string{
		{"--config", `C:\attacker\config.json`, "native-package-recover"},
		{"--config=C:\\attacker\\config.json", "native-package-install"},
		{"--update-notify", "prerelease", "install"},
		{"native-package-broker-commit", "--update-notify=stable"},
		{"--log.level", "trace", "uninstall"},
		{"-l", "trace", "native-package-install"},
		{"-ltrace", "native-package-recover"},
		{"native-package-recover", "--log.file", `C:\protected.log`},
		{"native-package-recover", "--log.raw-file=C:\\protected.raw"},
	}
	for _, args := range tests {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			t.Parallel()
			mode, err := classifyBootstrap(args)
			if mode != bootstrapPrivilegedLifecycle {
				t.Fatalf("classifyBootstrap(%q) mode = %v, want privileged", args, mode)
			}
			if err == nil {
				t.Fatalf("classifyBootstrap(%q) unexpectedly allowed configurable global option", args)
			}
		})
	}
}

func TestClassifyBootstrapRejectsPrivilegedCommandAliasesAndCase(t *testing.T) {
	t.Parallel()

	for _, command := range []string{
		"Install", "UNINSTALL", "Native-Package-Install",
		"NATIVE-PACKAGE-BROKER-COMMIT", "native-package-RECOVER",
	} {
		mode, err := classifyBootstrap([]string{command})
		if mode != bootstrapPrivilegedLifecycle || err == nil {
			t.Errorf("classifyBootstrap(%q) = (%v, %v), want privileged rejection", command, mode, err)
		}
	}
}

func TestClassifyBootstrapPreservesOrdinaryDispatch(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"server"},
		{"service", "run"},
		{"proxy", "--upstream", "native-package-recover"},
		// The protected-looking token is the value of --config, not a command.
		{"--config", "native-package-recover", "server"},
	} {
		mode, err := classifyBootstrap(args)
		if err != nil || mode != bootstrapStandard {
			t.Errorf("classifyBootstrap(%q) = (%v, %v), want standard", args, mode, err)
		}
	}
}

func TestPrivilegedLifecycleParserPreservesCapabilityArguments(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"helper":        `C:\stage\ViiperUdeCtl.exe`,
		"certificate":   `C:\stage\ViiperUdeTest.cer`,
		"authorization": `C:\stage\failed-install-recovery-progress.json`,
		"capability":    `C:\stage\failed-install-recovery-capability.json`,
	}
	var cli privilegedLifecycleCLI
	parser, err := kong.New(&cli, privilegedLifecycleKongOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	revision := strings.Repeat("b", 40)
	ctx, err := parser.Parse([]string{
		"native-package-recover",
		"--driver-helper", values["helper"],
		"--expected-helper-sha-256", hash,
		"--certificate-path", values["certificate"],
		"--expected-certificate-sha-256", hash,
		"--recovery-authorization", values["authorization"],
		"--expected-recovery-authorization-sha-256", hash,
		"--recovery-root-authorization-sha-256", hash,
		"--source-revision", revision,
		"--recovery-capability", values["capability"],
		"--expected-recovery-capability-sha-256", hash,
		"--current-package-lock-sha-256", hash,
		"--current-bundle-manifest-sha-256", hash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Command() != "native-package-recover" {
		t.Fatalf("command = %q", ctx.Command())
	}
	if cli.NativePackageRecover.DriverHelper != values["helper"] ||
		cli.NativePackageRecover.CertificatePath != values["certificate"] ||
		cli.NativePackageRecover.RecoveryAuthorization != values["authorization"] ||
		cli.NativePackageRecover.RecoveryCapability != values["capability"] ||
		cli.NativePackageRecover.ExpectedRecoveryCapabilitySHA256 != hash {
		t.Fatalf("command-specific recovery arguments changed during sealed parse: %+v", cli.NativePackageRecover)
	}
}

func TestPrivilegedLifecycleLoggerIgnoresEnvironmentPoisoning(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "poisoned.log")
	rawPath := filepath.Join(directory, "poisoned.raw")
	for _, path := range []string{logPath, rawPath} {
		if err := os.WriteFile(path, []byte("sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("VIIPER_LOG_LEVEL", "trace")
	t.Setenv("VIIPER_LOG_FILE", logPath)
	t.Setenv("VIIPER_LOG_RAW_FILE", rawPath)
	t.Setenv("VIIPER_UPDATE_NOTIFY", "prerelease")

	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	logger, closers, err := setupPrivilegedLifecycleLogger()
	if err != nil {
		t.Fatal(err)
	}
	defer closeLogFiles(closers)
	if len(closers) != 0 {
		t.Fatalf("privileged logger opened %d files", len(closers))
	}
	logger.Info("sealed bootstrap logger probe")
	for _, path := range []string{logPath, rawPath} {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(content) != "sentinel" {
			t.Fatalf("poisoned path %q was created, appended, or truncated: %q", path, content)
		}
	}
}

func TestPrivilegedLifecycleDisablesUpdater(t *testing.T) {
	t.Parallel()

	for _, notify := range []config.UpdateNotify{
		config.UpdateNotifyStable,
		config.UpdateNotifyPrerelease,
	} {
		if shouldStartUpdateNotifier(bootstrapPrivilegedLifecycle, "install", notify) {
			t.Fatalf("privileged lifecycle enabled updater for %q", notify)
		}
	}
	if shouldStartUpdateNotifier(bootstrapStandard, "service run", config.UpdateNotifyStable) {
		t.Fatal("service command enabled updater")
	}
	if !shouldStartUpdateNotifier(bootstrapStandard, "server", config.UpdateNotifyStable) {
		t.Fatal("ordinary server lost its update notifier")
	}
}

func TestPrivilegedBootstrapPrecedesConfigLogAndUpdaterSource(t *testing.T) {
	t.Parallel()

	_, sourcePath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(sourcePath), "viiper.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	mainStart := strings.Index(text, "func main()")
	mainEnd := strings.Index(text, "type bootstrapMode")
	if mainStart < 0 || mainEnd <= mainStart {
		t.Fatal("locate main bootstrap source")
	}
	mainBody := text[mainStart:mainEnd]
	classify := strings.Index(mainBody, "classifyBootstrap(os.Args[1:])")
	plainHelp := strings.Index(mainBody, "handlePlainHelpFlag()")
	standard := strings.Index(mainBody, "runStandard()")
	if classify < 0 || plainHelp < 0 || standard < 0 || classify > plainHelp || classify > standard {
		t.Fatalf("raw privileged classifier is not the first bootstrap decision:\n%s", mainBody)
	}

	sealedStart := strings.Index(text, "func runPrivilegedLifecycle()")
	sealedEnd := strings.Index(text, "func runStandard()")
	if sealedStart < 0 || sealedEnd <= sealedStart {
		t.Fatal("locate privileged lifecycle source")
	}
	sealedBody := text[sealedStart:sealedEnd]
	for _, forbidden := range []string{
		"findUserConfig", "ConfigCandidatePaths", "kong.Configuration",
		"setupRawLogger", "cli.Log", "updater.", "CheckUpdate",
	} {
		if strings.Contains(sealedBody, forbidden) {
			t.Errorf("privileged lifecycle source contains forbidden bootstrap dependency %q", forbidden)
		}
	}
}

func TestPrivilegedLifecycleSubprocessIgnoresConfigAndLogEnvironment(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess build disabled in short mode")
	}
	directory := t.TempDir()
	binary := filepath.Join(directory, "viiper-bootstrap-test")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, ".")
	buildOutput, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build subprocess fixture: %v\n%s", err, buildOutput)
	}

	configDir := filepath.Join(directory, "VIIPER")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	invalidConfig := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(invalidConfig, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(directory, "must-not-exist.log")
	rawPath := filepath.Join(directory, "must-not-exist.raw")
	updatePath := filepath.Join(configDir, "update-dismissed")

	hash := strings.Repeat("a", 64)
	revision := strings.Repeat("b", 40)
	command := exec.Command(binary,
		"native-package-recover",
		"--driver-helper", "relative-helper.exe",
		"--expected-helper-sha-256", hash,
		"--certificate-path", "relative-certificate.cer",
		"--expected-certificate-sha-256", hash,
		"--recovery-authorization", "relative-authorization.json",
		"--expected-recovery-authorization-sha-256", hash,
		"--recovery-root-authorization-sha-256", hash,
		"--source-revision", revision,
		"--recovery-capability", "relative-capability.json",
		"--expected-recovery-capability-sha-256", hash,
		"--current-package-lock-sha-256", hash,
		"--current-bundle-manifest-sha-256", hash,
	)
	command.Env = append(os.Environ(),
		fmt.Sprintf("APPDATA=%s", directory),
		fmt.Sprintf("VIIPER_CONFIG=%s", invalidConfig),
		"VIIPER_LOG_LEVEL=trace",
		fmt.Sprintf("VIIPER_LOG_FILE=%s", logPath),
		fmt.Sprintf("VIIPER_LOG_RAW_FILE=%s", rawPath),
		"VIIPER_UPDATE_NOTIFY=prerelease",
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("invalid recovery fixture unexpectedly succeeded: %s", output)
	}
	if !bytes.Contains(output, []byte("driver helper must be an absolute path")) {
		t.Fatalf("sealed subprocess did not reach command-specific validation:\n%s", output)
	}
	if bytes.Contains(output, []byte("config")) && bytes.Contains(output, []byte("not-json")) {
		t.Fatalf("sealed subprocess consulted poisoned config:\n%s", output)
	}
	for _, path := range []string{logPath, rawPath, updatePath} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("sealed subprocess created or updated %q (stat error %v)", path, statErr)
		}
	}
}

package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	viipercmd "github.com/Alia5/VIIPER/internal/cmd"
	"github.com/Alia5/VIIPER/internal/config"
	"github.com/Alia5/VIIPER/internal/configpaths"
	"github.com/Alia5/VIIPER/internal/log"
	"github.com/Alia5/VIIPER/internal/updater"

	_ "github.com/Alia5/VIIPER/internal/registry" // Register all device handlers

	"github.com/alecthomas/kong"
	kongtoml "github.com/alecthomas/kong-toml"
	kongyaml "github.com/alecthomas/kong-yaml"
	"golang.org/x/term"
)

func main() {
	bootstrap, err := classifyBootstrap(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "refusing privileged lifecycle bootstrap:", err)
		os.Exit(2)
	}

	handlePlainHelpFlag()
	if bootstrap == bootstrapPrivilegedLifecycle {
		runPrivilegedLifecycle()
		return
	}
	runStandard()
}

type bootstrapMode uint8

const (
	bootstrapStandard bootstrapMode = iota
	bootstrapPrivilegedLifecycle
)

// privilegedLifecycleCLI intentionally contains no configurable global fields.
// These commands can be launched elevated from a user-controlled environment;
// parsing them through config.CLI would consult env-backed config, log, and
// updater settings before their command-specific authorization checks run.
type privilegedLifecycleCLI struct {
	Install                   viipercmd.Install                   `cmd:"" help:"Add the current VIIPER executable to system startup and runs it (creates a Systemd service on Linux)"`
	Uninstall                 viipercmd.Uninstall                 `cmd:"" help:"Remove any VIIPER system startup configuration / Systemd service"`
	NativePackageInstall      viipercmd.NativePackageInstall      `cmd:"" name:"native-package-install" help:"Install a verified native UDE package and broker transactionally" hidden:""`
	NativePackageRecover      viipercmd.NativePackageRecover      `cmd:"" name:"native-package-recover" help:"Reconcile only retained native package journals" hidden:""`
	NativePackageBrokerCommit viipercmd.NativePackageBrokerCommit `cmd:"" name:"native-package-broker-commit" help:"Commit the broker inside an active native package transaction" hidden:""`
}

func runPrivilegedLifecycle() {
	var cli privilegedLifecycleCLI
	ctx := kong.Parse(&cli, privilegedLifecycleKongOptions()...)

	// A privileged lifecycle command never opens a configured file logger or
	// raw packet logger. The fixed console logger is independent of argv, env,
	// user config, and default config locations.
	logger, closeFiles, err := setupPrivilegedLifecycleLogger()
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to setup privileged lifecycle console logger:", err)
		os.Exit(2)
	}
	defer closeLogFiles(closeFiles)

	rawLogger := log.NewRaw(nil)
	ctx.Bind(logger)
	ctx.BindTo(rawLogger, (*log.RawLogger)(nil))

	err = ctx.Run()
	ctx.FatalIfErrorf(err)
}

func privilegedLifecycleKongOptions() []kong.Option {
	return []kong.Option{
		kong.Name("VIIPER"),
		kong.Description(Description()),
		kong.UsageOnError(),
		kong.Help(kong.DefaultHelpPrinter),
	}
}

func setupPrivilegedLifecycleLogger() (*slog.Logger, []io.Closer, error) {
	return log.SetupLogger("info", "")
}

func runStandard() {

	userCfg := findUserConfig(os.Args[1:])
	jsonPaths, yamlPaths, tomlPaths := configpaths.ConfigCandidatePaths(userCfg)

	var cli config.CLI
	ctx := kong.Parse(&cli,
		kong.Name("VIIPER"),
		kong.Description(Description()),
		kong.UsageOnError(),
		kong.Help(helpWithASCIIArt),
		// Load configuration from JSON/YAML/TOML in priority order; flags/env override config values.
		kong.Configuration(kong.JSON, jsonPaths...),
		kong.Configuration(kongyaml.Loader, yamlPaths...),
		kong.Configuration(kongtoml.Loader, tomlPaths...),
	)

	logger, closeFiles, err := log.SetupLogger(cli.Log.Level, cli.Log.File) // nolint
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to setup logger:", err)
		os.Exit(2)
	}
	rawLogger := setupRawLogger(&cli, logger, &closeFiles)
	defer closeLogFiles(closeFiles)

	ctx.Bind(logger)
	ctx.BindTo(rawLogger, (*log.RawLogger)(nil))

	// A broker hosted by Service Control Manager has no interactive desktop.
	// Update UI belongs to DS4Windows/the package installer, never session 0.
	if shouldStartUpdateNotifier(bootstrapStandard, ctx.Command(), cli.UpdateNotify) {
		go func() {
			time.Sleep(10 * time.Second)
			updater.CheckUpdate(Version, cli.UpdateNotify)
			for range time.NewTicker(1 * time.Hour).C {
				updater.CheckUpdate(Version, cli.UpdateNotify)
			}
		}()
	}

	err = ctx.Run()
	ctx.FatalIfErrorf(err)
}

func shouldStartUpdateNotifier(
	bootstrap bootstrapMode,
	command string,
	notify config.UpdateNotify,
) bool {
	return bootstrap == bootstrapStandard &&
		!strings.HasPrefix(command, "service") &&
		notify != config.UpdateNotifyNone
}

func closeLogFiles(closeFiles []io.Closer) {
	for _, closer := range closeFiles {
		_ = closer.Close()
	}
}

func classifyBootstrap(args []string) (bootstrapMode, error) {
	command := rawCommand(args)
	canonical, protected := privilegedLifecycleCommand(command)
	if !protected {
		return bootstrapStandard, nil
	}
	if command != canonical {
		return bootstrapPrivilegedLifecycle, fmt.Errorf(
			"command %q must use canonical spelling %q", command, canonical,
		)
	}
	for _, arg := range args {
		if option, forbidden := privilegedBootstrapForbiddenOption(arg); forbidden {
			return bootstrapPrivilegedLifecycle, fmt.Errorf(
				"%s is not allowed for command %q", option, canonical,
			)
		}
	}
	return bootstrapPrivilegedLifecycle, nil
}

func rawCommand(args []string) string {
	for position := 0; position < len(args); position++ {
		arg := args[position]
		if arg == "--" {
			if position+1 < len(args) {
				return args[position+1]
			}
			return ""
		}
		if option, consumesNext := configurableGlobalOption(arg); option != "" {
			if consumesNext && position+1 < len(args) {
				position++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}

func configurableGlobalOption(arg string) (name string, consumesNext bool) {
	lower := strings.ToLower(arg)
	for _, option := range []string{
		"--config", "--update-notify", "--log.level", "--log.file", "--log.raw-file",
	} {
		if lower == option {
			return option, true
		}
		if strings.HasPrefix(lower, option+"=") {
			return option, false
		}
	}
	if lower == "-l" {
		return "-l", true
	}
	if strings.HasPrefix(lower, "-l=") || (strings.HasPrefix(lower, "-l") && len(lower) > 2) {
		return "-l", false
	}
	return "", false
}

func privilegedBootstrapForbiddenOption(arg string) (string, bool) {
	option, _ := configurableGlobalOption(arg)
	if option == "" {
		return "", false
	}
	return option, true
}

func privilegedLifecycleCommand(command string) (canonical string, protected bool) {
	for _, candidate := range []string{
		"install",
		"uninstall",
		"native-package-install",
		"native-package-broker-commit",
		"native-package-recover",
	} {
		if strings.EqualFold(command, candidate) {
			return candidate, true
		}
	}
	return "", false
}

func handlePlainHelpFlag() {
	for i, arg := range os.Args[1:] {
		if arg == "-p" {
			os.Setenv("VIIPER_HELP_STYLE", "plain") // nolint
			os.Args[i+1] = "-h"
			return
		}
	}
}

func findUserConfig(args []string) string {
	for i, a := range args {
		if strings.HasPrefix(a, "--config=") {
			return a[len("--config="):]
		}
		if a == "--config" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return os.Getenv("VIIPER_CONFIG")
}

func setupRawLogger(cli *config.CLI, logger *slog.Logger, closeFiles *[]io.Closer) log.RawLogger {
	if cli.Log.RawFile != "" { // nolint
		f, err := log.OpenBoundedFile(cli.Log.RawFile, 0o644) // nolint
		if err != nil {
			logger.Error("failed to open raw log file", "file", cli.Log.RawFile, "error", err) // nolint
			return log.NewRaw(nil)
		}
		*closeFiles = append(*closeFiles, f)
		return log.NewRaw(f)
	}
	if cli.Log.Level == "trace" { // nolint
		return log.NewRaw(os.Stdout)
	}
	return log.NewRaw(nil)
}

func helpWithASCIIArt(options kong.HelpOptions, ctx *kong.Context) error {
	// VIIPER_HELP_STYLE env var: "plain", "big", "small", or auto-detect
	helpStyle := strings.ToLower(os.Getenv("VIIPER_HELP_STYLE"))
	if helpStyle == "" {
		helpStyle = detectHelpStyle()
	}
	if helpStyle == "plain" {
		return kong.DefaultHelpPrinter(options, ctx)
	}

	helpText := captureHelpOutput(options, ctx)

	art := asciiBrailleColoredSmall
	if helpStyle == "big" {
		art = asciiBrailleColoredBig
	}

	output := mergeArtWithHelp(normalizeLineEndings(art), normalizeLineEndings(helpText))
	_, err := fmt.Fprint(ctx.Stdout, output)
	return err
}

func captureHelpOutput(options kong.HelpOptions, ctx *kong.Context) string {
	var buf bytes.Buffer
	origStdout := ctx.Stdout
	ctx.Stdout = &buf
	_ = kong.DefaultHelpPrinter(options, ctx)
	ctx.Stdout = origStdout
	return buf.String()
}

func normalizeLineEndings(s string) string {
	s = strings.TrimRight(s, "\r\n")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func mergeArtWithHelp(art, help string) string {
	artLines := strings.Split(art, "\n")
	helpLines := strings.Split(help, "\n")

	artWidth := maxVisibleWidth(artLines) + 2

	maxLines := max(len(artLines), len(helpLines))
	artOffset := (len(helpLines) - len(artLines)) / 2
	if artOffset < 0 {
		artOffset = 0
	}

	var out strings.Builder
	for i := range maxLines {
		artLine := ""
		if idx := i - artOffset; idx >= 0 && idx < len(artLines) {
			artLine = artLines[idx]
		}

		helpLine := ""
		if i < len(helpLines) {
			helpLine = helpLines[i]
		}

		padding := artWidth - visibleWidth(artLine)
		out.WriteString(artLine)
		out.WriteString(strings.Repeat(" ", padding))
		out.WriteString(helpLine)
		out.WriteString("\n")
	}
	return out.String()
}

func maxVisibleWidth(lines []string) int {
	maxWidth := 0
	for _, line := range lines {
		if w := visibleWidth(line); w > maxWidth {
			maxWidth = w
		}
	}
	return maxWidth
}

func visibleWidth(s string) int {
	inEscape := false
	width := 0
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		width++
	}
	return width
}

func detectHelpStyle() string {
	fd := int(os.Stdout.Fd())
	if !term.IsTerminal(fd) {
		fd = int(os.Stderr.Fd())
		if !term.IsTerminal(fd) {
			return "plain"
		}
	}

	if os.Getenv("TERM") == "dumb" {
		return "plain"
	}

	width, _, err := term.GetSize(fd)
	if err != nil || width <= 0 {
		return "small"
	}

	const (
		bigThreshold   = 140
		smallThreshold = 110
	)
	switch {
	case width >= bigThreshold:
		return "big"
	case width >= smallThreshold:
		return "small"
	default:
		return "plain"
	}
}

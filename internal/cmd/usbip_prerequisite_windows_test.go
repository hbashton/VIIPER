//go:build windows

package cmd

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrerequisiteFailuresExposePhaseWithoutParsingDriverText(t *testing.T) {
	for _, test := range []struct {
		name, phase, output string
		err                 error
		want                int
	}{
		{"version-launch", "--version", "", errors.New("launch denied"), 70},
		{"wrong-version", "--version", "0.9.7.8", nil, 71},
		{"version-timeout", "--version", "", context.DeadlineExceeded, 72},
		{"version-pipe-timeout", "--version", "", exec.ErrWaitDelay, 72},
		{"port-timeout", "port", "", context.DeadlineExceeded, 72},
		{"port-pipe-timeout", "port", "", exec.ErrWaitDelay, 72},
		{"driver-failed", "port", "private driver output", errors.New("exit 1"), 73},
		{"driver-abi", "port", "ABI mismatch", nil, 73},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := probeUSBIPRuntime(`C:\synthetic\usbip.exe`,
				func(_ context.Context, _ string, args ...string) ([]byte, error) {
					if args[0] == test.phase {
						return []byte(test.output), test.err
					}
					require.Equal(t, "--version", args[0])
					return []byte(requiredUSBIPVersion), nil
				})
			code, known := StartupExitCode(err)
			require.True(t, known)
			require.Equal(t, test.want, code)
		})
	}
}

func TestUSBIPCommandBoundsProcessAndRedirectedPipeDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), usbipProbeTimeout)
	defer cancel()
	command := usbipCommand(ctx, `C:\synthetic\usbip.exe`, "port")
	// Inspect only: never execute a driver CLI or touch installed components.
	require.Equal(t, []string{`C:\synthetic\usbip.exe`, "port"}, command.Args)
	require.NotNil(t, command.Cancel, "CommandContext must retain child cancellation")
	require.Equal(t, 250*time.Millisecond, command.WaitDelay)
	require.Less(t, 2*(usbipProbeTimeout+command.WaitDelay), 25*time.Second,
		"The client's readiness budget must exceed both complete prerequisite probes")
}

func TestUSBIPPrerequisiteProbesEachHaveAFiniteDeadline(t *testing.T) {
	calls := 0
	err := probeUSBIPRuntime(`C:\synthetic\usbip.exe`,
		func(ctx context.Context, _ string, args ...string) ([]byte, error) {
			calls++
			deadline, bounded := ctx.Deadline()
			require.True(t, bounded)
			require.Positive(t, time.Until(deadline))
			require.LessOrEqual(t, time.Until(deadline), usbipProbeTimeout)
			if args[0] == "--version" {
				return []byte(requiredUSBIPVersion), nil
			}
			return nil, nil
		})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
}

func TestUSBIPPrerequisiteCancellationStopsBeforeAnotherCommand(t *testing.T) {
	for _, phase := range []string{"before", "--version", "port"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "before" {
				cancel()
			}
			calls := 0
			err := probeUSBIPRuntimeContext(ctx, `C:\synthetic\usbip.exe`,
				func(child context.Context, _ string, args ...string) ([]byte, error) {
					calls++
					if args[0] == phase {
						cancel()
						select {
						case <-child.Done():
						case <-time.After(time.Second):
							t.Fatal("prerequisite command did not inherit owner cancellation")
						}
					}
					return []byte(requiredUSBIPVersion), nil
				})
			require.ErrorIs(t, err, context.Canceled)
			_, classified := StartupExitCode(err)
			require.False(t, classified, "an intentional stop is not an incompatible driver")
			want := map[string]int{"before": 0, "--version": 1, "port": 2}[phase]
			require.Equal(t, want, calls)
		})
	}
}

func TestProbeUSBIPRuntimeAcceptsPinnedCompatibleRuntime(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, executable string, args ...string) ([]byte, error) {
		call := append([]string{executable}, args...)
		calls = append(calls, call)
		switch args[0] {
		case "--version":
			return []byte("0.9.7.7\r\n"), nil
		case "port":
			return []byte("Imported USB devices\r\n====================\r\n"), nil
		default:
			t.Fatalf("unexpected arguments: %v", args)
			return nil, nil
		}
	}

	err := probeUSBIPRuntime(`C:\Program Files\USBip\usbip.exe`, run)

	require.NoError(t, err)
	assert.Equal(t, [][]string{
		{`C:\Program Files\USBip\usbip.exe`, "--version"},
		{`C:\Program Files\USBip\usbip.exe`, "port"},
	}, calls)
}

func TestProbeUSBIPRuntimeRejectsEveryOtherVersionBeforeDriverProbe(t *testing.T) {
	portCalled := false
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "port" {
			portCalled = true
		}
		return []byte("0.9.7.8\r\n"), nil
	}

	err := probeUSBIPRuntime(`C:\Program Files\USBip\usbip.exe`, run)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires usbip-win2 0.9.7.7")
	assert.Contains(t, err.Error(), "found 0.9.7.8")
	assert.False(t, portCalled)
}

func TestProbeUSBIPRuntimeRejectsSuccessfulABIErrorOutput(t *testing.T) {
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "--version" {
			return []byte("0.9.7.7"), nil
		}
		return []byte("error: ABI mismatch, unexpected size of the input structure"), nil
	}

	err := probeUSBIPRuntime(`C:\Program Files\USBip\usbip.exe`, run)

	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "abi mismatch")
}

func TestProbeUSBIPRuntimeIncludesPortFailureOutput(t *testing.T) {
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "--version" {
			return []byte("0.9.7.7"), nil
		}
		return []byte("driver query failed"), errors.New("exit status 1")
	}

	err := probeUSBIPRuntime(`C:\Program Files\USBip\usbip.exe`, run)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "driver query failed")
}

func TestUSBIPProbeFailureRecognizesKnownABIConversionError(t *testing.T) {
	assert.Equal(t,
		"specified conversion is not valid",
		usbipProbeFailure([]byte("ERROR: The specified conversion is not valid.")),
	)
}

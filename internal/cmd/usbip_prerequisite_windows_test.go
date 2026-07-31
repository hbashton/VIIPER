//go:build windows

package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

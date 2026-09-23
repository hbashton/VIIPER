package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Alia5/VIIPER/internal/cmd"
	"github.com/stretchr/testify/require"
)

func TestMainMapsTypedStartupFailureToItsProcessExitCode(t *testing.T) {
	// Relative explicit keys fail before any installed prerequisite, socket,
	// tray, driver, or controller is accessed.
	key := "relative-key-is-invalid"
	server := cmd.Server{KeyFile: &key}
	err := server.StartServer(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.Error(t, err)
	var diagnostic bytes.Buffer
	exits := []int{}
	require.True(t, exitForStartupFailure(err, &diagnostic, func(code int) { exits = append(exits, code) }))
	require.Equal(t, []int{74}, exits)
	require.Contains(t, diagnostic.String(), "VIIPER startup failed:")
}

func TestMainLeavesOrdinaryCLIErrorAndSuccessExitBehaviorUnchanged(t *testing.T) {
	for _, err := range []error{nil, errors.New("ordinary argument error")} {
		var diagnostic bytes.Buffer
		require.False(t, exitForStartupFailure(err, &diagnostic,
			func(int) { t.Fatal("Not a classified startup failure") }))
		require.Empty(t, diagnostic.String())
	}
}

//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestConfigOnlyRejectsUnreadableFileWithoutFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
	widePath, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	handle, err := windows.CreateFile(widePath, windows.GENERIC_READ, 0,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
	options, only, err := configurationOptions([]string{"--config-only", "--config", path})
	require.Error(t, err)
	require.True(t, only)
	require.Nil(t, options)
}

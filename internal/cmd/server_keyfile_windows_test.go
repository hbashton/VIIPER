//go:build windows

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestServerKeyFileCommandLineDistinguishesOmittedFromExplicitEmpty(t *testing.T) {
	original := configureCurrentServerProcessPriority
	t.Cleanup(func() { configureCurrentServerProcessPriority = original })
	configureCurrentServerProcessPriority = func() serverProcessPriorityConfiguration {
		return serverProcessPriorityConfiguration{high: serverProcessPriorityAttempt{
			requested: 1, effective: 1,
		}}
	}
	path := filepath.Join(t.TempDir(), keyFileName)
	for _, tc := range []struct {
		name string
		args []string
		want *string
		fail bool
	}{
		{name: "omitted", args: []string{"server"}},
		{name: "explicit", args: []string{"server", "--key-file", path}, want: &path},
		{name: "empty", args: []string{"server", "--key-file="}, fail: true},
		{name: "relative", args: []string{"server", "--key-file=key.txt"}, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cli struct {
				Server Server `cmd:""`
			}
			parser, err := kong.New(&cli)
			require.NoError(t, err)
			_, err = parser.Parse(tc.args)
			if tc.fail {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, cli.Server.KeyFile)
		})
	}
}

func TestServerExplicitUnreadableKeyIsNotRegenerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), keyFileName)
	require.NoError(t, os.WriteFile(path, []byte("unchanged"), 0o600))
	widePath, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	// Deny only data-read/write sharing; metadata validation can still run.
	handle, err := windows.CreateFile(widePath, windows.GENERIC_READ, 0,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
	password, generated, err := loadServerAPIKey(path, true)
	require.Error(t, err)
	require.Empty(t, password)
	require.False(t, generated)
	require.NoError(t, windows.CloseHandle(handle))
	handle = windows.InvalidHandle
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "unchanged", string(data))
}

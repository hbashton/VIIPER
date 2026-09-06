package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/require"
)

func TestConfigOnlyRequiresOneExplicitConfigArgument(t *testing.T) {
	t.Setenv("VIIPER_CONFIG", filepath.Join(t.TempDir(), "environment.json"))
	for _, args := range [][]string{
		{"--config-only"},
		{"--config-only", "--config=a", "--config=b"},
		{"--config-only", "--config-only", "--config=a"},
		{"--config-only=invalid"},
	} {
		_, _, err := exclusiveConfigArgument(args)
		require.Error(t, err, "args=%v", args)
	}
	path := filepath.Join(t.TempDir(), "server.json")
	for _, args := range [][]string{
		{"--config-only", "--config", path},
		{"--config=" + path, "--config-only=true"},
	} {
		selected, only, err := exclusiveConfigArgument(args)
		require.NoError(t, err)
		require.True(t, only)
		require.Equal(t, path, selected)
	}
	_, only, err := exclusiveConfigArgument([]string{"--config-only=false"})
	require.NoError(t, err)
	require.False(t, only)
}

func TestConfigOnlyLoadsExactFileWithoutDefaultCandidates(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("AppData", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	// Every usual format contains malformed fallback content. None may be read.
	for _, directory := range []string{root, filepath.Join(root, "VIIPER"), filepath.Join(root, "github.com", "Alia5", "viiper")} {
		require.NoError(t, os.MkdirAll(directory, 0o700))
		for _, name := range []string{"config.json", "server.yaml", "proxy.toml"} {
			require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte("[invalid content"), 0o600))
		}
	}
	for _, tc := range []struct {
		ext  string
		data string
	}{
		{ext: ".json", data: `{"value":"selected"}`},
		{ext: ".yaml", data: "value: selected\n"},
		{ext: ".yml", data: "value: selected\n"},
		{ext: ".toml", data: "value = \"selected\"\n"},
	} {
		t.Run(tc.ext, func(t *testing.T) {
			path := filepath.Join(root, "explicit"+tc.ext)
			require.NoError(t, os.WriteFile(path, []byte(tc.data), 0o600))
			args := []string{"--config-only", "--config", path}
			options, only, err := configurationOptions(args)
			require.NoError(t, err)
			require.True(t, only)
			require.Len(t, options, 1)
			var cli struct {
				ConfigOnly bool
				ConfigPath string `name:"config"`
				Value      string
			}
			parser, err := kong.New(&cli, options...)
			require.NoError(t, err)
			_, err = parser.Parse(args)
			require.NoError(t, err)
			require.True(t, cli.ConfigOnly)
			require.Equal(t, "selected", cli.Value)
			// Explicit flags still take precedence over the selected config.
			_, err = parser.Parse(append(args, "--value=cli"))
			require.NoError(t, err)
			require.Equal(t, "cli", cli.Value)
		})
	}
}

func TestConfigOnlyRejectsMissingRelativeNonregularOrMalformedConfig(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"", "relative.json", root, filepath.Join(root, "missing.json")} {
		_, _, err := configurationOptions([]string{"--config-only", "--config", path})
		require.Error(t, err, "path=%q", path)
	}
	for _, tc := range []struct {
		name string
		ext  string
		data string
	}{
		{name: "invalid-json", ext: ".json", data: "{"},
		{name: "empty-json", ext: ".json"},
		{name: "null-json", ext: ".json", data: "null"},
		{name: "array-json", ext: ".json", data: "[]"},
		{name: "trailing-json", ext: ".json", data: "{} garbage"},
		{name: "double-json", ext: ".json", data: "{} {}"},
		{name: "invalid-yaml", ext: ".yaml", data: "key: ["},
		{name: "empty-yaml", ext: ".yml"},
		{name: "double-yaml", ext: ".yaml", data: "value: one\n---\nvalue: two"},
		{name: "invalid-toml", ext: ".toml", data: "value = ["},
		{name: "unknown-extension", ext: ".txt", data: "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name+tc.ext)
			require.NoError(t, os.WriteFile(path, []byte(tc.data), 0o600))
			_, _, err := configurationOptions([]string{"--config-only", "--config", path})
			require.Error(t, err)
		})
	}
}

func TestConfigurationOmissionPreservesDefaultDiscovery(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("AppData", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("VIIPER_CONFIG", "")
	require.NoError(t, os.WriteFile(filepath.Join(root, "server.json"), []byte(`{"value":"default-discovery"}`), 0o600))
	options, only, err := configurationOptions(nil)
	require.NoError(t, err)
	require.False(t, only)
	require.Len(t, options, 3)
	var cli struct{ Value string }
	parser, err := kong.New(&cli, options...)
	require.NoError(t, err)
	_, err = parser.Parse(nil)
	require.NoError(t, err)
	require.Equal(t, "default-discovery", cli.Value)
}

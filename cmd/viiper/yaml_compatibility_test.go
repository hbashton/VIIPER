package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/require"
)

func TestYAMLConfigurationPreservesNestedAndFlatCompatibility(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		for _, nested := range []bool{false, true} {
			name := "normal-flat"
			if exclusive {
				name = "exclusive-flat"
			}
			if nested {
				name += "-nested"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				t.Chdir(root)
				t.Setenv("AppData", root)
				t.Setenv("XDG_CONFIG_HOME", root)
				t.Setenv("VIIPER_CONFIG", "")
				data := "server-address: selected\nserver-enabled: false\nlegacy-unused: retained\n"
				if nested {
					data = "server:\n  address: selected\n  enabled: false\nlegacy-unused: retained\n"
				}
				path := filepath.Join(root, "explicit.yaml")
				require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
				args := []string{"--config", path}
				if exclusive {
					args = append(args, "--config-only")
					// An explicitly selected file must not fall back to this.
					require.NoError(t, os.WriteFile(filepath.Join(root, "server.yaml"), []byte("[broken"), 0o600))
				}
				options, only, err := configurationOptions(args)
				require.NoError(t, err)
				require.Equal(t, exclusive, only)
				var cli struct {
					ConfigOnly bool
					ConfigPath string `name:"config"`
					Server     struct {
						Address string
						Enabled bool `default:"true"`
					} `cmd:""`
				}
				parser, err := kong.New(&cli, options...)
				require.NoError(t, err)
				_, err = parser.Parse(append(args, "server"))
				require.NoError(t, err)
				require.Equal(t, "selected", cli.Server.Address)
				require.False(t, cli.Server.Enabled)
				_, err = parser.Parse(append(args, "server", "--address=command-line", "--enabled"))
				require.NoError(t, err)
				require.Equal(t, "command-line", cli.Server.Address)
				require.True(t, cli.Server.Enabled)
			})
		}
	}
}

func TestYAMLCompatibilityStillRejectsMalformedConfiguration(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		name := "normal"
		if exclusive {
			name = "exclusive"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			t.Setenv("AppData", root)
			t.Setenv("XDG_CONFIG_HOME", root)
			t.Setenv("VIIPER_CONFIG", "")
			path := filepath.Join(root, "broken.yaml")
			require.NoError(t, os.WriteFile(path, []byte("value: ["), 0o600))
			args := []string{"--config", path}
			if exclusive {
				args = append(args, "--config-only")
			}
			options, _, err := configurationOptions(args)
			if exclusive {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			var cli struct {
				ConfigPath string `name:"config"`
				Value      string
			}
			_, err = kong.New(&cli, options...)
			require.Error(t, err)
		})
	}
}

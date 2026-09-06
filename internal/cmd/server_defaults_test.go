//go:build windows

package cmd

import (
	"os"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/require"
)

func TestShippedServerCommandDefaultsUSBIPToIPv4Loopback(t *testing.T) {
	original := configureCurrentServerProcessPriority
	t.Cleanup(func() { configureCurrentServerProcessPriority = original })
	configureCurrentServerProcessPriority = func() serverProcessPriorityConfiguration {
		return serverProcessPriorityConfiguration{high: serverProcessPriorityAttempt{
			requested: 1, effective: 1,
		}}
	}
	originalAddress, hadAddress := os.LookupEnv("VIIPER_USB_ADDR")
	require.NoError(t, os.Unsetenv("VIIPER_USB_ADDR"))
	t.Cleanup(func() {
		if hadAddress {
			_ = os.Setenv("VIIPER_USB_ADDR", originalAddress)
		} else {
			_ = os.Unsetenv("VIIPER_USB_ADDR")
		}
	})
	var cli struct {
		Server Server `cmd:""`
	}
	parser, err := kong.New(&cli)
	require.NoError(t, err)
	_, err = parser.Parse([]string{"server"})
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:3241", cli.Server.USBServerConfig.Addr)
}

//go:build windows

package tray

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPackagedTrayCannotWriteAnUnconfiguredAutostartEntry(t *testing.T) {
	for _, value := range []string{"", "0", "true", "yes", " 1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("VIIPER_DEVELOPER_STANDALONE", value)
			require.False(t, standaloneStartupAllowed())
			require.False(t, toggleStandaloneStartup(standaloneStartupAllowed, func() bool {
				t.Fatal("production tray invoked registry mutation")
				return true
			}))
		})
	}
}

func TestDeveloperOptInRetainsLegacyStartupToggleResult(t *testing.T) {
	t.Setenv("VIIPER_DEVELOPER_STANDALONE", "1")
	require.True(t, standaloneStartupAllowed())
	for _, enabled := range []bool{true, false} {
		calls := 0
		require.Equal(t, enabled, toggleStandaloneStartup(standaloneStartupAllowed, func() bool {
			calls++
			return enabled
		}))
		require.Equal(t, 1, calls)
	}
}

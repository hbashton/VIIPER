//go:build windows

package cmd

import (
	"errors"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestConfigureServerProcessPriorityUsesVerifiedHigh(t *testing.T) {
	var requested []uint32
	configuration := configureServerProcessPriority(windows.Handle(1), serverProcessPriorityAPI{
		set: func(_ windows.Handle, priority uint32) error {
			requested = append(requested, priority)
			return nil
		},
		get: func(windows.Handle) (uint32, error) {
			return windows.HIGH_PRIORITY_CLASS, nil
		},
	})

	require.True(t, configuration.high.verified())
	require.Nil(t, configuration.fallback)
	require.Equal(t, []uint32{windows.HIGH_PRIORITY_CLASS}, requested)
}

func TestConfigureServerProcessPriorityFallsBackToAboveNormal(t *testing.T) {
	accessDenied := errors.New("access denied")
	var requested []uint32
	configuration := configureServerProcessPriority(windows.Handle(1), serverProcessPriorityAPI{
		set: func(_ windows.Handle, priority uint32) error {
			requested = append(requested, priority)
			if priority == windows.HIGH_PRIORITY_CLASS {
				return accessDenied
			}
			return nil
		},
		get: func(windows.Handle) (uint32, error) {
			return windows.ABOVE_NORMAL_PRIORITY_CLASS, nil
		},
	})

	require.ErrorIs(t, configuration.high.failure(), accessDenied)
	require.NotNil(t, configuration.fallback)
	require.True(t, configuration.fallback.verified())
	require.Equal(t, []uint32{
		windows.HIGH_PRIORITY_CLASS,
		windows.ABOVE_NORMAL_PRIORITY_CLASS,
	}, requested)
}

func TestConfigureServerProcessPriorityDoesNotDowngradeUnverifiedHigh(t *testing.T) {
	verificationBlocked := errors.New("verification blocked")
	var requested []uint32
	configuration := configureServerProcessPriority(windows.Handle(1), serverProcessPriorityAPI{
		set: func(_ windows.Handle, priority uint32) error {
			requested = append(requested, priority)
			return nil
		},
		get: func(windows.Handle) (uint32, error) {
			return 0, verificationBlocked
		},
	})

	require.ErrorIs(t, configuration.high.failure(), verificationBlocked)
	require.Nil(t, configuration.fallback)
	require.Equal(t, []uint32{windows.HIGH_PRIORITY_CLASS}, requested)
}

func TestConfigureServerProcessPriorityFallsBackWhenHighDoesNotStick(t *testing.T) {
	var requested []uint32
	configuration := configureServerProcessPriority(windows.Handle(1), serverProcessPriorityAPI{
		set: func(_ windows.Handle, priority uint32) error {
			requested = append(requested, priority)
			return nil
		},
		get: func(windows.Handle) (uint32, error) {
			if requested[len(requested)-1] == windows.HIGH_PRIORITY_CLASS {
				return windows.BELOW_NORMAL_PRIORITY_CLASS, nil
			}
			return windows.ABOVE_NORMAL_PRIORITY_CLASS, nil
		},
	})

	require.False(t, configuration.high.verified())
	require.NotNil(t, configuration.fallback)
	require.True(t, configuration.fallback.verified())
}

func TestServerPriorityHookRunsOnlyForServerCommand(t *testing.T) {
	original := configureCurrentServerProcessPriority
	t.Cleanup(func() { configureCurrentServerProcessPriority = original })

	calls := 0
	configureCurrentServerProcessPriority = func() serverProcessPriorityConfiguration {
		calls++
		return serverProcessPriorityConfiguration{high: serverProcessPriorityAttempt{
			requested: windows.HIGH_PRIORITY_CLASS,
			effective: windows.HIGH_PRIORITY_CLASS,
		}}
	}

	type otherCommand struct{}
	var otherCLI struct {
		Server Server       `cmd:""`
		Other  otherCommand `cmd:""`
	}
	parser, err := kong.New(&otherCLI)
	require.NoError(t, err)
	_, err = parser.Parse([]string{"other"})
	require.NoError(t, err)
	require.Zero(t, calls)

	var serverCLI struct {
		Server Server       `cmd:""`
		Other  otherCommand `cmd:""`
	}
	parser, err = kong.New(&serverCLI)
	require.NoError(t, err)
	_, err = parser.Parse([]string{"server"})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
}

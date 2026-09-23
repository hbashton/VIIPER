package cmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStartupExitCodesRemainStableAndPreserveUnderlyingErrors(t *testing.T) {
	for _, test := range []struct {
		code, want int
	}{
		{StartupUSBIPUnavailable, 70}, {StartupUSBIPVersion, 71},
		{StartupUSBIPTimeout, 72}, {StartupUSBIPDriver, 73},
		{StartupKey, 74}, {StartupUSBListener, 75}, {StartupAPIListener, 76},
	} {
		cause := errors.New("untrusted driver text, secret-looking data, private paths")
		err := startupFailure(test.code, cause)
		require.ErrorIs(t, err, cause)
		require.Equal(t, cause.Error(), err.Error())
		code, known := StartupExitCode(fmt.Errorf("wrapping caller: %w", err))
		require.True(t, known)
		require.Equal(t, test.want, code)
	}
}

func TestUnclassifiedFailuresAndSuccessDoNotInventStartupExitCodes(t *testing.T) {
	for _, err := range []error{nil, errors.New("USB/IP prerequisite failed: timeout"),
		startupFailure(69, errors.New("not assigned")), startupFailure(77, errors.New("not assigned")),
	} {
		code, known := StartupExitCode(err)
		require.False(t, known)
		require.Zero(t, code)
	}
	for code := StartupUSBIPUnavailable; code <= StartupAPIListener; code++ {
		require.NoError(t, startupFailure(code, nil))
	}
}

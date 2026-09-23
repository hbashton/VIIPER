package cmd

import "errors"

// These exit codes form the local launcher diagnostic contract. They identify
// a phase only: callers must not parse stderr (which can contain paths and
// driver output) or replace credentials/configuration in response to a code.
const (
	StartupUSBIPUnavailable = 70
	StartupUSBIPVersion     = 71
	StartupUSBIPTimeout     = 72
	StartupUSBIPDriver      = 73
	StartupKey              = 74
	StartupUSBListener      = 75
	StartupAPIListener      = 76
)

type startupError struct {
	exitCode int
	cause    error
}

func (e *startupError) Error() string { return e.cause.Error() }
func (e *startupError) Unwrap() error { return e.cause }

func startupFailure(exitCode int, cause error) error {
	if cause == nil {
		return nil
	}
	return &startupError{exitCode: exitCode, cause: cause}
}

// StartupExitCode returns only documented phase codes for our typed failures;
// coincidental words in another error cannot classify a failure as startup.
func StartupExitCode(err error) (int, bool) {
	var failure *startupError
	if errors.As(err, &failure) && failure.exitCode >= StartupUSBIPUnavailable &&
		failure.exitCode <= StartupAPIListener {
		return failure.exitCode, true
	}
	return 0, false
}

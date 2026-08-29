//go:build windows

package cmd

import (
	"fmt"
	"log/slog"

	"golang.org/x/sys/windows"
)

type serverProcessPriorityAPI struct {
	set func(windows.Handle, uint32) error
	get func(windows.Handle) (uint32, error)
}

type serverProcessPriorityAttempt struct {
	requested uint32
	effective uint32
	setErr    error
	verifyErr error
}

func (a serverProcessPriorityAttempt) verified() bool {
	return a.setErr == nil && a.verifyErr == nil && a.effective == a.requested
}

func (a serverProcessPriorityAttempt) failure() error {
	switch {
	case a.setErr != nil:
		return a.setErr
	case a.verifyErr != nil:
		return a.verifyErr
	case a.effective != a.requested:
		return fmt.Errorf("Windows reported priority class %#x after requesting %#x", a.effective, a.requested)
	default:
		return nil
	}
}

type serverProcessPriorityConfiguration struct {
	high     serverProcessPriorityAttempt
	fallback *serverProcessPriorityAttempt
}

var configureCurrentServerProcessPriority = func() serverProcessPriorityConfiguration {
	return configureServerProcessPriority(windows.CurrentProcess(), serverProcessPriorityAPI{
		set: windows.SetPriorityClass,
		get: windows.GetPriorityClass,
	})
}

// AfterApply is a Kong command hook. It runs only when the server command is
// selected, including a no-argument Windows GUI launch after startup injects
// the server argument. Other VIIPER commands and libVIIPER do not change the
// host process priority.
func (*Server) AfterApply() error {
	configuration := configureCurrentServerProcessPriority()
	if configuration.high.verified() {
		return nil
	}

	// A successful SetPriorityClass followed by an unreadable result is most
	// likely already High. Do not immediately downgrade it merely because the
	// verification call was blocked by host policy.
	if configuration.high.setErr == nil && configuration.high.verifyErr != nil {
		slog.Warn(
			"Windows accepted VIIPER's High server priority request, but the result could not be verified; continuing",
			"error", configuration.high.verifyErr,
		)
		return nil
	}

	if configuration.fallback != nil && configuration.fallback.verified() {
		slog.Warn(
			"VIIPER could not use High server priority; continuing at Above Normal",
			"high_error", configuration.high.failure(),
		)
		return nil
	}

	var fallbackErr error
	if configuration.fallback != nil {
		fallbackErr = configuration.fallback.failure()
	}
	slog.Warn(
		"VIIPER could not raise its server priority; continuing with the inherited Windows priority",
		"high_error", configuration.high.failure(),
		"above_normal_error", fallbackErr,
	)
	return nil
}

func configureServerProcessPriority(
	process windows.Handle,
	api serverProcessPriorityAPI,
) serverProcessPriorityConfiguration {
	high := attemptServerProcessPriority(process, windows.HIGH_PRIORITY_CLASS, api)
	configuration := serverProcessPriorityConfiguration{high: high}
	if high.verified() || (high.setErr == nil && high.verifyErr != nil) {
		return configuration
	}

	fallback := attemptServerProcessPriority(process, windows.ABOVE_NORMAL_PRIORITY_CLASS, api)
	configuration.fallback = &fallback
	return configuration
}

func attemptServerProcessPriority(
	process windows.Handle,
	requested uint32,
	api serverProcessPriorityAPI,
) serverProcessPriorityAttempt {
	attempt := serverProcessPriorityAttempt{requested: requested}
	if err := api.set(process, requested); err != nil {
		attempt.setErr = fmt.Errorf("set process priority class %#x: %w", requested, err)
		return attempt
	}

	effective, err := api.get(process)
	if err != nil {
		attempt.verifyErr = fmt.Errorf("read process priority after requesting %#x: %w", requested, err)
		return attempt
	}
	attempt.effective = effective
	return attempt
}

//go:build windows

package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/windows"
)

const nativePackageProcessJoinRetry = 10 * time.Millisecond

type nativePackageProcessJoin struct {
	handle windows.Handle
	wait   func(windows.Handle, uint32) (uint32, error)
	close  func(windows.Handle) error
	retry  func()
}

type nativePackageProcessWaitIndeterminateError struct {
	cause error
}

func (e *nativePackageProcessWaitIndeterminateError) Error() string {
	return "native package helper process result is indeterminate after independently joining termination: " +
		e.cause.Error()
}

func (e *nativePackageProcessWaitIndeterminateError) Unwrap() error {
	return e.cause
}

// retainNativePackageProcessJoin duplicates Go's exact process handle before
// exec.Cmd.Wait can release it. The duplicate is intentionally wait-only: it
// exists solely to keep the package/service transaction alive until the exact
// mutating child process is signaled.
func retainNativePackageProcessJoin(process *os.Process) (*nativePackageProcessJoin, error) {
	if process == nil {
		return nil, errors.New("native package helper has no process")
	}
	var retained windows.Handle
	var duplicateErr error
	if err := process.WithHandle(func(handle uintptr) {
		duplicateErr = windows.DuplicateHandle(
			windows.CurrentProcess(), windows.Handle(handle),
			windows.CurrentProcess(), &retained,
			windows.SYNCHRONIZE, false, 0,
		)
	}); err != nil {
		if retained != 0 {
			windows.CloseHandle(retained) //nolint:errcheck
		}
		return nil, fmt.Errorf("retain native package helper process handle: %w", err)
	}
	if duplicateErr != nil {
		if retained != 0 {
			windows.CloseHandle(retained) //nolint:errcheck
		}
		return nil, fmt.Errorf("duplicate native package helper process handle: %w", duplicateErr)
	}
	if retained == 0 {
		return nil, errors.New("duplicate native package helper process returned a null handle")
	}
	return &nativePackageProcessJoin{
		handle: retained,
		wait:   windows.WaitForSingleObject,
		close:  windows.CloseHandle,
		retry: func() {
			time.Sleep(nativePackageProcessJoinRetry)
		},
	}, nil
}

// complete independently joins the retained process object before releasing
// its handle. A non-ExitError from Cmd.Wait cannot establish an exit status,
// so it remains an indeterminate transaction failure even after the child is
// proven terminated. Wait anomalies are retried while the handle and outer
// transaction scope remain held; this function never returns unjoined.
func (j *nativePackageProcessJoin) complete(commandWaitErr error) error {
	if j == nil || j.handle == 0 || j.wait == nil || j.close == nil || j.retry == nil {
		return &nativePackageProcessWaitIndeterminateError{
			cause: errors.New("native package helper process join is unavailable"),
		}
	}

	var joinAnomaly error
	for {
		status, err := j.wait(j.handle, windows.INFINITE)
		if err == nil && status == windows.WAIT_OBJECT_0 {
			break
		}
		if joinAnomaly == nil {
			if err != nil {
				joinAnomaly = fmt.Errorf("wait for retained native package helper process: %w", err)
			} else {
				joinAnomaly = fmt.Errorf(
					"wait for retained native package helper process returned 0x%08x", status)
			}
		}
		j.retry()
	}
	closeErr := j.close(j.handle)
	j.handle = 0
	if closeErr != nil {
		closeErr = fmt.Errorf("close retained native package helper process handle: %w", closeErr)
	}

	var exitError *exec.ExitError
	if commandWaitErr != nil && !errors.As(commandWaitErr, &exitError) {
		return &nativePackageProcessWaitIndeterminateError{
			cause: errors.Join(commandWaitErr, joinAnomaly, closeErr),
		}
	}
	if joinAnomaly != nil || closeErr != nil {
		return fmt.Errorf(
			"independent native package helper process join failed (command wait: %v): %w",
			commandWaitErr, errors.Join(joinAnomaly, closeErr),
		)
	}
	return commandWaitErr
}

func waitNativePackageHelper(command *exec.Cmd) error {
	return waitNativePackageHelperWith(command, retainNativePackageProcessJoin,
		func() { time.Sleep(nativePackageProcessJoinRetry) })
}

// waitNativePackageHelperCoordinated retains the exact helper process while
// the outer package transaction services its inherited quiescence/handoff
// events. The callback must return only after the retained process is signaled
// or after a coordination anomaly; complete still joins the exact child before
// any package or service lock can unwind.
func waitNativePackageHelperCoordinated(
	command *exec.Cmd,
	coordinate func(windows.Handle) error,
) error {
	join := retainNativePackageProcessJoinWithRetry(
		command.Process, retainNativePackageProcessJoin,
		func() { time.Sleep(nativePackageProcessJoinRetry) },
	)
	coordinationErr := coordinate(join.handle)
	waitErr := join.complete(command.Wait())
	if coordinationErr != nil {
		return &nativePackageProcessWaitIndeterminateError{
			cause: errors.Join(coordinationErr, waitErr),
		}
	}
	return waitErr
}

func retainNativePackageProcessJoinWithRetry(
	process *os.Process,
	retain func(*os.Process) (*nativePackageProcessJoin, error),
	retry func(),
) *nativePackageProcessJoin {
	var join *nativePackageProcessJoin
	for join == nil {
		var err error
		join, err = retain(process)
		if err == nil {
			break
		}
		// Cmd.Wait has not run, so Go still owns the exact source handle.
		// Never unwind a mutating package transaction without an independent
		// wait handle; transient resource pressure is retried while every outer
		// lock and immutable input handle remains held.
		retry()
	}
	return join
}

func waitNativePackageHelperWith(
	command *exec.Cmd,
	retain func(*os.Process) (*nativePackageProcessJoin, error),
	retry func(),
) error {
	join := retainNativePackageProcessJoinWithRetry(command.Process, retain, retry)
	// A recovered pre-Wait duplication retry is not a transaction failure: the
	// exact handle was retained before Cmd.Wait and supplies the required join.
	return join.complete(command.Wait())
}

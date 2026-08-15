//go:build windows

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNativePackageProcessJoinPreservesSuccessfulWait(t *testing.T) {
	command := exec.Command("cmd.exe", "/d", "/c", "exit", "0")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	join, err := retainNativePackageProcessJoin(command.Process)
	if err != nil {
		_ = command.Wait()
		t.Fatalf("retain process join: %v", err)
	}
	if err := join.complete(command.Wait()); err != nil {
		t.Fatalf("complete successful process join: %v", err)
	}
}

func TestNativePackageProcessJoinPreservesExitError(t *testing.T) {
	command := exec.Command("cmd.exe", "/d", "/c", "exit", "7")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	join, err := retainNativePackageProcessJoin(command.Process)
	if err != nil {
		_ = command.Wait()
		t.Fatalf("retain process join: %v", err)
	}
	err = join.complete(command.Wait())
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 7 {
		t.Fatalf("joined error=%v, want exec.ExitError exit 7", err)
	}
}

func TestNativePackageProcessJoinRecoveredRetainRetryIsNonFatal(t *testing.T) {
	command := exec.Command("cmd.exe", "/d", "/c", "exit", "0")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	err := waitNativePackageHelperWith(
		command,
		func(process *os.Process) (*nativePackageProcessJoin, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("synthetic DuplicateHandle resource pressure")
			}
			return retainNativePackageProcessJoin(process)
		},
		func() {},
	)
	if err != nil {
		t.Fatalf("recovered retain retry overrode successful process result: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("retain attempts=%d, want 2", attempts)
	}
}

func TestNativePackageProcessJoinHoldsScopeAfterAmbiguousCommandWait(t *testing.T) {
	entered := make(chan struct{})
	signal := make(chan struct{})
	closed := make(chan struct{})
	join := &nativePackageProcessJoin{
		handle: 1,
		wait: func(windows.Handle, uint32) (uint32, error) {
			close(entered)
			<-signal
			return windows.WAIT_OBJECT_0, nil
		},
		close: func(windows.Handle) error {
			close(closed)
			return nil
		},
		retry: func() {},
	}

	done := make(chan error, 1)
	go func() {
		done <- join.complete(errors.New("synthetic Cmd.Wait failure"))
	}()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("ambiguous wait released transaction scope before process signal: %v", err)
	default:
	}
	close(signal)
	err := <-done
	var indeterminate *nativePackageProcessWaitIndeterminateError
	if !errors.As(err, &indeterminate) {
		t.Fatalf("joined ambiguous wait error=%v, want indeterminate failure", err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("retained process handle was not closed after signal")
	}
}

func TestNativePackageProcessJoinAnomalyDoesNotExposeExitError(t *testing.T) {
	commandWaitErr := exec.Command("cmd.exe", "/d", "/c", "exit", "7").Run()
	var commandExitError *exec.ExitError
	if !errors.As(commandWaitErr, &commandExitError) {
		t.Fatalf("test command error=%v, want exec.ExitError", commandWaitErr)
	}
	waits := 0
	join := &nativePackageProcessJoin{
		handle: 1,
		wait: func(windows.Handle, uint32) (uint32, error) {
			waits++
			if waits == 1 {
				return windows.WAIT_FAILED, windows.ERROR_INVALID_HANDLE
			}
			return windows.WAIT_OBJECT_0, nil
		},
		close: func(windows.Handle) error { return nil },
		retry: func() {},
	}
	err := join.complete(commandWaitErr)
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		t.Fatalf("join anomaly exposed command ExitError to proof parsing: %v", err)
	}
}

func TestNativePackageProcessJoinRetriesWaitAnomalyUntilSignal(t *testing.T) {
	waits := 0
	join := &nativePackageProcessJoin{
		handle: 1,
		wait: func(windows.Handle, uint32) (uint32, error) {
			waits++
			if waits == 1 {
				return windows.WAIT_FAILED, windows.ERROR_INVALID_HANDLE
			}
			return windows.WAIT_OBJECT_0, nil
		},
		close: func(windows.Handle) error { return nil },
		retry: func() {},
	}
	err := join.complete(errors.New("synthetic Cmd.Wait failure"))
	var indeterminate *nativePackageProcessWaitIndeterminateError
	if !errors.As(err, &indeterminate) {
		t.Fatalf("joined anomalous wait error=%v, want indeterminate failure", err)
	}
	if waits != 2 {
		t.Fatalf("retained process wait calls=%d, want 2", waits)
	}
}

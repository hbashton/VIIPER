//go:build windows

package cmd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func TestNativeServiceKeyFileUsesMachineData(t *testing.T) {
	t.Setenv("ProgramData", `C:\Users\attacker\redirected`)
	got, err := nativeServiceKeyFilePath()
	if err != nil {
		t.Fatal(err)
	}
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(programData, "VIIPER", keyFileName)
	if got != want {
		t.Fatalf("key path=%q want=%q", got, want)
	}
}

func TestNativeServiceStopsCooperatively(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	handler := &nativeBrokerService{run: func(ctx context.Context, ready func()) error {
		close(started)
		ready()
		<-ctx.Done()
		close(stopped)
		return nil
	}}
	requests := make(chan svc.ChangeRequest, 1)
	changes := make(chan svc.Status, 8)
	result := make(chan struct {
		specific bool
		code     uint32
	}, 1)
	go func() {
		specific, code := handler.Execute(nil, requests, changes)
		result <- struct {
			specific bool
			code     uint32
		}{specific, code}
	}()

	waitForServiceState(t, changes, svc.StartPending)
	waitForServiceState(t, changes, svc.Running)
	<-started
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	waitForServiceState(t, changes, svc.StopPending)
	<-stopped

	select {
	case got := <-result:
		if got.specific || got.code != 0 {
			t.Fatalf("service result=(specific=%v code=%d), want clean stop", got.specific, got.code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service did not stop after cancellation")
	}
}

func TestNativeServiceDoesNotReportRunningBeforeBrokerReady(t *testing.T) {
	releaseReady := make(chan struct{})
	handler := &nativeBrokerService{run: func(ctx context.Context, ready func()) error {
		select {
		case <-releaseReady:
			ready()
		case <-ctx.Done():
			return ctx.Err()
		}
		<-ctx.Done()
		return nil
	}}
	requests := make(chan svc.ChangeRequest, 1)
	changes := make(chan svc.Status, 8)
	result := make(chan uint32, 1)
	go func() {
		_, code := handler.Execute(nil, requests, changes)
		result <- code
	}()

	waitForServiceState(t, changes, svc.StartPending)
	select {
	case got := <-changes:
		t.Fatalf("service reported state %v before broker readiness", got.State)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseReady)
	waitForServiceState(t, changes, svc.Running)
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	waitForServiceState(t, changes, svc.StopPending)
	if code := <-result; code != 0 {
		t.Fatalf("service exit code=%d want=0", code)
	}
}

func TestNativeServiceReportsUnexpectedBrokerFailure(t *testing.T) {
	var records bytes.Buffer
	handler := &nativeBrokerService{
		logger: slog.New(slog.NewTextHandler(&records, nil)),
		run: func(context.Context, func()) error {
			return errors.New("broker failed")
		},
	}
	changes := make(chan svc.Status, 8)
	specific, code := handler.Execute(nil, make(chan svc.ChangeRequest), changes)
	if !specific || code != 1 {
		t.Fatalf("service result=(specific=%v code=%d), want service-specific failure 1", specific, code)
	}
	if logged := records.String(); !strings.Contains(logged, "VIIPER native broker stopped unexpectedly") ||
		!strings.Contains(logged, "broker failed") {
		t.Fatalf("service failure log=%q", logged)
	}
}

func TestNativeServiceLogsFailureAfterReportingRunning(t *testing.T) {
	var records bytes.Buffer
	release := make(chan struct{})
	handler := &nativeBrokerService{
		logger: slog.New(slog.NewTextHandler(&records, nil)),
		run: func(_ context.Context, ready func()) error {
			ready()
			<-release
			return errors.New("live transport failed")
		},
	}
	changes := make(chan svc.Status, 8)
	result := make(chan uint32, 1)
	go func() {
		_, code := handler.Execute(nil, make(chan svc.ChangeRequest), changes)
		result <- code
	}()
	waitForServiceState(t, changes, svc.StartPending)
	waitForServiceState(t, changes, svc.Running)
	close(release)
	waitForServiceState(t, changes, svc.StopPending)
	if code := <-result; code != 1 {
		t.Fatalf("live service failure exit code=%d want=1", code)
	}
	if logged := records.String(); !strings.Contains(logged, "live transport failed") {
		t.Fatalf("live service failure log=%q", logged)
	}
}

func waitForServiceState(t *testing.T, changes <-chan svc.Status, want svc.State) {
	t.Helper()
	select {
	case got := <-changes:
		if got.State != want {
			t.Fatalf("service state=%v want=%v", got.State, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for service state %v", want)
	}
}

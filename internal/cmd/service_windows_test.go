//go:build windows

package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

func TestNativeServiceKeyFileUsesMachineData(t *testing.T) {
	t.Setenv("ProgramData", `C:\ProgramData`)
	got, err := nativeServiceKeyFilePath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(`C:\ProgramData`, "VIIPER", keyFileName)
	if got != want {
		t.Fatalf("key path=%q want=%q", got, want)
	}
}

func TestNativeServiceStopsCooperatively(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	handler := &nativeBrokerService{run: func(ctx context.Context) error {
		close(started)
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

func TestNativeServiceReportsUnexpectedBrokerFailure(t *testing.T) {
	handler := &nativeBrokerService{run: func(context.Context) error {
		return errors.New("broker failed")
	}}
	changes := make(chan svc.Status, 8)
	specific, code := handler.Execute(nil, make(chan svc.ChangeRequest), changes)
	if !specific || code != 1 {
		t.Fatalf("service result=(specific=%v code=%d), want service-specific failure 1", specific, code)
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

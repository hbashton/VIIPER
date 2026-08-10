//go:build windows

package cmd

import (
	"context"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestNativePackageServiceTrustRequiresExactOwnedState(t *testing.T) {
	t.Parallel()
	expected := mgr.Config{
		ServiceType: 0x10, StartType: mgr.StartAutomatic, ErrorControl: mgr.ErrorNormal,
		BinaryPathName:   `"C:\Program Files\VIIPER\viiper.exe" service --transport native-ude`,
		ServiceStartName: nativeServiceAccount, DisplayName: nativeBrokerDisplayName,
		Description: nativeBrokerDescription, SidType: 1,
	}
	actions := append([]mgr.RecoveryAction(nil), nativeServiceRecoveryActions...)
	canonical := func(actual mgr.Config, dacl string, recovery []mgr.RecoveryAction,
		reset uint32, nonCrash bool) bool {
		return isCanonicalNativePackageService(
			actual, expected, dacl, recovery, reset, nonCrash,
		)
	}
	if !canonical(expected, nativeBrokerServiceSDDL, actions,
		nativeServiceRecoveryResetSecond, true) {
		t.Fatal("exact protected service was not trusted")
	}

	staleConfig := expected
	staleConfig.StartType = mgr.StartManual
	staleRecovery := append([]mgr.RecoveryAction(nil), actions...)
	staleRecovery[0].Delay = time.Millisecond
	cases := map[string]bool{
		"stale config":   canonical(staleConfig, nativeBrokerServiceSDDL, actions, nativeServiceRecoveryResetSecond, true),
		"weak DACL":      canonical(expected, "D:(A;;GA;;;WD)", actions, nativeServiceRecoveryResetSecond, true),
		"stale recovery": canonical(expected, nativeBrokerServiceSDDL, staleRecovery, nativeServiceRecoveryResetSecond, true),
		"stale reset":    canonical(expected, nativeBrokerServiceSDDL, actions, 1, true),
		"stale mode":     canonical(expected, nativeBrokerServiceSDDL, actions, nativeServiceRecoveryResetSecond, false),
	}
	for name, trusted := range cases {
		if trusted {
			t.Errorf("%s was trusted instead of delete/recreate", name)
		}
	}
}

func TestNativePackageRollbackReconcilesStoppedPriorService(t *testing.T) {
	t.Parallel()
	events := []string{}
	service := &fakeNativeService{events: &events, status: svc.Status{State: svc.Stopped}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := reconcileNativePackageServiceRunning(ctx, service); err != nil {
		t.Fatalf("reconcile prior service: %v", err)
	}
	if service.startCalls != 1 || service.status.State != svc.Running {
		t.Fatalf("startCalls=%d state=%d events=%v", service.startCalls, service.status.State, events)
	}
}

//go:build windows

package cmd

import (
	"context"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestNativePackageCoordinationTokenAllowsNestedImmutableRead(t *testing.T) {
	requireNativeMutexAdministrator(t)
	transaction := &windowsNativePackageTransaction{parent: t.TempDir()}
	if err := transaction.stageCoordinationToken(); err != nil {
		t.Fatalf("stage coordination token: %v", err)
	}
	t.Cleanup(func() {
		if err := transaction.releaseCoordinationToken(); err != nil {
			t.Errorf("release coordination token: %v", err)
		}
	})

	// This is the exact access/share combination used by the nested broker.
	// It failed live while the outer transaction retained a write-capable
	// handle, even though both opens requested FILE_SHARE_READ.
	nested, err := lockNativePackageInput(transaction.tokenPath)
	if err != nil {
		t.Fatalf("nested immutable token open: %v", err)
	}
	defer windows.CloseHandle(nested) //nolint:errcheck
	hash, err := hashNativePackageHandle(nested)
	if err != nil {
		t.Fatalf("hash nested token handle: %v", err)
	}
	if hash != transaction.tokenSHA256 {
		t.Fatalf("nested token hash = %s, want %s", hash, transaction.tokenSHA256)
	}
}

func TestNativePackageRuntimePayloadExcludesCertificationPDB(t *testing.T) {
	want := []string{"ViiperUde.inf", "ViiperUde.sys", "ViiperUde.cat"}
	if !slices.Equal(nativePackageDriverFiles, want) {
		t.Fatalf("runtime driver payload = %v, want %v", nativePackageDriverFiles, want)
	}
	if slices.Contains(nativePackageDriverFiles, "ViiperUde.pdb") {
		t.Fatal("certification PDB became a runtime installation dependency")
	}
}

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

func TestNativePackageRollbackPreservesImagesAndStoppedServiceWhenSCMRollbackIsUnsettled(t *testing.T) {
	t.Parallel()
	events := []string{}
	service := &fakeNativeService{events: &events, status: svc.Status{State: svc.Stopped}}
	transaction := &windowsNativePackageTransaction{
		nestedBrokerCommit:           true,
		nestedMutationStarted:        true,
		nestedServiceRollbackSettled: false,
		destinationPublished:         true,
		backupPath:                   `C:\Program Files\VIIPER\.prior.rollback.exe`,
		stoppedTrustedService:        true,
		serviceSnapshot:              nativePackageServiceSnapshot{wasRunning: true},
		service:                      service,
	}

	err := transaction.Rollback(context.Background())
	if err == nil {
		t.Fatal("unsettled nested SCM rollback was reported as restored")
	}
	if service.startCalls != 0 || len(events) != 0 {
		t.Fatalf("indeterminate service was restarted: startCalls=%d events=%v",
			service.startCalls, events)
	}
	if !transaction.destinationPublished {
		t.Fatal("staged broker image was removed after unsettled SCM rollback")
	}
	if transaction.backupPath == "" {
		t.Fatal("prior broker backup was consumed after unsettled SCM rollback")
	}
	if transaction.nestedRollbackSucceeded {
		t.Fatal("unsettled SCM rollback was exposed as a safe nested rollback")
	}
}

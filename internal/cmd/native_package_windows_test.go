//go:build windows

package cmd

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestNativePackageDriverCoordinationUsesDistinctInheritedEvents(t *testing.T) {
	t.Parallel()
	coordination, err := newNativePackageDriverCoordination()
	if err != nil {
		t.Fatalf("create driver coordination: %v", err)
	}
	defer coordination.close()
	seen := map[windows.Handle]bool{}
	for _, handle := range []windows.Handle{
		coordination.quiesceRequest, coordination.quiesceReady,
		coordination.quiesceAbort, coordination.brokerHandoff,
	} {
		if handle == 0 || seen[handle] {
			t.Fatalf("coordination event handle=%d is null or duplicated", handle)
		}
		seen[handle] = true
		status, err := windows.WaitForSingleObject(handle, 0)
		if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
			t.Fatalf("coordination event %d initial wait=(0x%x, %v), want timeout",
				handle, status, err)
		}
	}
	if len(coordination.inheritedHandles()) != 4 || len(coordination.arguments()) != 8 {
		t.Fatal("driver coordination did not publish exactly four inherited handle arguments")
	}
	if unsafe.Sizeof(windows.Handle(0)) != unsafe.Sizeof(uintptr(0)) {
		t.Fatal("Windows handle width no longer matches the decimal handoff contract")
	}
}

func TestNativePackageDriverCoordinationHoldsServiceMutexUntilBrokerHandoff(t *testing.T) {
	t.Parallel()
	coordination, err := newNativePackageDriverCoordination()
	if err != nil {
		t.Fatal(err)
	}
	defer coordination.close()
	process, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(process) //nolint:errcheck

	released := make(chan struct{})
	transaction := &windowsNativePackageTransaction{
		serviceSnapshot: nativePackageServiceSnapshot{disposition: nativePackageServiceAbsent},
		releaseServiceMutex: func() {
			close(released)
		},
	}
	childDone := make(chan error, 1)
	go func() {
		if err := windows.SetEvent(coordination.quiesceRequest); err != nil {
			childDone <- err
			return
		}
		if status, err := windows.WaitForSingleObject(coordination.quiesceReady, 1000); err != nil || status != windows.WAIT_OBJECT_0 {
			childDone <- errors.Join(err, errors.New("quiescence readiness was not signaled"))
			return
		}
		select {
		case <-released:
			childDone <- errors.New("service mutex released before broker handoff")
			return
		default:
		}
		if err := windows.SetEvent(coordination.brokerHandoff); err != nil {
			childDone <- err
			return
		}
		select {
		case <-released:
		case <-time.After(time.Second):
			childDone <- errors.New("service mutex remained held after broker handoff")
			return
		}
		childDone <- windows.SetEvent(process)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := transaction.coordinateDriverHelper(ctx, process, coordination); err != nil {
		t.Fatalf("coordinate driver helper: %v", err)
	}
	if err := <-childDone; err != nil {
		t.Fatal(err)
	}
	if !transaction.driverQuiesceRequested || !transaction.driverBrokerHandoff ||
		transaction.releaseServiceMutex != nil {
		t.Fatalf("coordination state request=%v handoff=%v release=%v",
			transaction.driverQuiesceRequested, transaction.driverBrokerHandoff,
			transaction.releaseServiceMutex != nil)
	}
}

func TestNativePackageDriverQuiescenceStopsTrustedRunningService(t *testing.T) {
	t.Parallel()
	events := []string{}
	service := &fakeNativeService{events: &events, status: svc.Status{State: svc.Running}}
	transaction := &windowsNativePackageTransaction{
		serviceSnapshot: nativePackageServiceSnapshot{
			disposition: nativePackageServiceTrusted,
			wasRunning:  true,
		},
		service:             service,
		releaseServiceMutex: func() {},
	}
	if err := transaction.quiescePriorServiceForDriver(context.Background()); err != nil {
		t.Fatalf("quiesce trusted service: %v", err)
	}
	if !transaction.stoppedTrustedService || service.status.State != svc.Stopped ||
		!slices.Equal(events, []string{"service-stop"}) {
		t.Fatalf("trusted service state stopped=%v status=%d events=%v",
			transaction.stoppedTrustedService, service.status.State, events)
	}

	if err := transaction.quiescePriorServiceForDriver(context.Background()); err != nil {
		t.Fatalf("repeat trusted quiescence: %v", err)
	}
}

func TestNativePackageDriverQuiescenceRemovesWeakExactOwnedService(t *testing.T) {
	t.Parallel()
	for _, running := range []bool{false, true} {
		running := running
		t.Run(map[bool]string{false: "stopped", true: "running"}[running], func(t *testing.T) {
			t.Parallel()
			events := []string{}
			state := svc.Stopped
			if running {
				state = svc.Running
			}
			service := &fakeNativeService{events: &events, status: svc.Status{State: state}}
			manager := newFakeNativeSCM(service, &events)
			transaction := &windowsNativePackageTransaction{
				serviceSnapshot: nativePackageServiceSnapshot{
					disposition: nativePackageServiceWeakExactOwned,
					wasRunning:  running,
				},
				service: service, manager: manager,
				releaseServiceMutex: func() {},
			}
			if err := transaction.quiescePriorServiceForDriver(context.Background()); err != nil {
				t.Fatalf("quiesce weak service: %v", err)
			}
			want := []string{"service-delete", "service-open"}
			if running {
				want = append([]string{"service-stop"}, want...)
			}
			if !transaction.weakServiceMutation || !transaction.weakServiceRemoved ||
				transaction.service != nil || !service.deleted || !slices.Equal(events, want) {
				t.Fatalf("weak service state mutation=%v removed=%v live=%v deleted=%v events=%v want=%v",
					transaction.weakServiceMutation, transaction.weakServiceRemoved,
					transaction.service != nil, service.deleted, events, want)
			}
			if err := transaction.quiescePriorServiceForDriver(context.Background()); err != nil {
				t.Fatalf("repeat weak quiescence: %v", err)
			}
			if !slices.Equal(events, want) {
				t.Fatalf("repeat weak quiescence mutated service again: events=%v want=%v", events, want)
			}
		})
	}
}

func TestNativePackageDriverQuiescenceDoesNotRestoreWeakServiceOnDeleteFailure(t *testing.T) {
	t.Parallel()
	events := []string{}
	service := &fakeNativeService{
		events: &events, status: svc.Status{State: svc.Running}, failDelete: errors.New("delete failed"),
	}
	weak := &windowsNativePackageTransaction{
		serviceSnapshot: nativePackageServiceSnapshot{
			disposition: nativePackageServiceWeakExactOwned,
			wasRunning:  true,
		},
		service: service, manager: newFakeNativeSCM(service, &events),
		releaseServiceMutex: func() {},
	}
	if err := weak.quiescePriorServiceForDriver(context.Background()); err == nil {
		t.Fatal("weak service delete failure was accepted")
	}
	if !weak.weakServiceMutation || weak.weakServiceRemoved || service.status.State != svc.Stopped ||
		service.deleted || !slices.Equal(events, []string{"service-stop", "service-delete"}) {
		t.Fatalf("weak failure state mutation=%v removed=%v status=%d deleted=%v events=%v",
			weak.weakServiceMutation, weak.weakServiceRemoved, service.status.State,
			service.deleted, events)
	}
	if err := weak.Rollback(context.Background()); err != nil {
		t.Fatalf("fail-closed weak rollback: %v", err)
	}
	if service.startCalls != 0 || service.status.State != svc.Stopped {
		t.Fatalf("untrusted weak service was restarted: starts=%d status=%d",
			service.startCalls, service.status.State)
	}
}

func TestNativePackageOuterRollbackLeavesServiceStoppedOnUnsettledDriverProof(t *testing.T) {
	t.Parallel()
	events := []string{}
	service := &fakeNativeService{events: &events, status: svc.Status{State: svc.Stopped}}
	transaction := &windowsNativePackageTransaction{
		serviceSnapshot: nativePackageServiceSnapshot{
			disposition: nativePackageServiceTrusted,
			wasRunning:  true,
		},
		service:               service,
		stoppedTrustedService: true,
		driverHelperSettled:   false,
	}
	err := transaction.Rollback(context.Background())
	if err == nil || service.startCalls != 0 || len(events) != 0 {
		t.Fatalf("unsettled outer rollback error=%v startCalls=%d events=%v",
			err, service.startCalls, events)
	}
}

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

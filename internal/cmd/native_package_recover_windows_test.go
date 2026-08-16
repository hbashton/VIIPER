//go:build windows

package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestNativePackageRecoveryTrustAdmission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		counts       nativePackageRecoveryTrustCounts
		allowPartial bool
		wantErr      bool
	}{
		{"initial exact", nativePackageRecoveryTrustCounts{1, 1}, false, false},
		{"initial missing root", nativePackageRecoveryTrustCounts{0, 1}, false, true},
		{"initial already absent", nativePackageRecoveryTrustCounts{0, 0}, false, true},
		{"retry untouched", nativePackageRecoveryTrustCounts{1, 1}, true, false},
		{"retry root removed", nativePackageRecoveryTrustCounts{0, 1}, true, false},
		{"retry publisher removed", nativePackageRecoveryTrustCounts{1, 0}, true, false},
		{"retry complete", nativePackageRecoveryTrustCounts{0, 0}, true, false},
		{"retry duplicate", nativePackageRecoveryTrustCounts{2, 1}, true, true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateNativePackageRecoveryTrustAdmission(test.counts, test.allowPartial)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestNativePackageFailedInstallRecoveryCapabilityBinding(t *testing.T) {
	t.Parallel()
	request := validNativePackageRecoverRequest()
	lease := `C:\ProgramData\VIIPER-TrustManager\lease-v1.lock`
	capability := nativePackageFailedInstallRecoveryCapability{
		Schema:                          nativePackageFailedInstallRecoveryCapabilitySchema,
		Nonce:                           strings.Repeat("a", 32),
		ParentPID:                       1234,
		ParentCreationFileTime:          5678,
		LeasePath:                       lease,
		SourceRevision:                  request.sourceRevision,
		HelperSHA256:                    request.expectedHelperSHA256,
		CertificateSHA256:               request.expectedCertificateSHA256,
		RecoveryAuthorizationSHA256:     request.expectedRecoveryAuthorizationSHA256,
		RecoveryRootAuthorizationSHA256: request.recoveryRootAuthorizationSHA256,
		PackageLockSHA256:               request.currentPackageLockSHA256,
		BundleManifestSHA256:            request.currentBundleManifestSHA256,
		AllowPartialCertificateState:    request.allowPartialCertificateState,
	}
	if err := validateNativePackageFailedInstallRecoveryCapability(
		capability, request, lease, 1234, 5678,
	); err != nil {
		t.Fatalf("valid recovery capability rejected: %v", err)
	}
	mutations := map[string]func(*nativePackageFailedInstallRecoveryCapability){
		"schema":          func(v *nativePackageFailedInstallRecoveryCapability) { v.Schema = "v2" },
		"nonce":           func(v *nativePackageFailedInstallRecoveryCapability) { v.Nonce = strings.Repeat("A", 32) },
		"parent PID":      func(v *nativePackageFailedInstallRecoveryCapability) { v.ParentPID++ },
		"parent creation": func(v *nativePackageFailedInstallRecoveryCapability) { v.ParentCreationFileTime++ },
		"lease":           func(v *nativePackageFailedInstallRecoveryCapability) { v.LeasePath += ".other" },
		"source":          func(v *nativePackageFailedInstallRecoveryCapability) { v.SourceRevision = strings.Repeat("3", 40) },
		"helper":          func(v *nativePackageFailedInstallRecoveryCapability) { v.HelperSHA256 = strings.Repeat("4", 64) },
		"certificate":     func(v *nativePackageFailedInstallRecoveryCapability) { v.CertificateSHA256 = strings.Repeat("5", 64) },
		"authorization": func(v *nativePackageFailedInstallRecoveryCapability) {
			v.RecoveryAuthorizationSHA256 = strings.Repeat("6", 64)
		},
		"root authority": func(v *nativePackageFailedInstallRecoveryCapability) {
			v.RecoveryRootAuthorizationSHA256 = strings.Repeat("7", 64)
		},
		"package lock": func(v *nativePackageFailedInstallRecoveryCapability) { v.PackageLockSHA256 = strings.Repeat("8", 64) },
		"bundle": func(v *nativePackageFailedInstallRecoveryCapability) {
			v.BundleManifestSHA256 = strings.Repeat("9", 64)
		},
		"retry": func(v *nativePackageFailedInstallRecoveryCapability) { v.AllowPartialCertificateState = true },
	}
	for name, mutate := range mutations {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			changed := capability
			mutate(&changed)
			if err := validateNativePackageFailedInstallRecoveryCapability(
				changed, request, lease, 1234, 5678,
			); err == nil {
				t.Fatal("mismatched recovery capability was admitted")
			}
		})
	}
}

func TestNativePackageR4RecoveryRejectsMissingRetainedEvidence(t *testing.T) {
	t.Parallel()
	request := validNativePackageRecoverRequest()
	request.expectedCertificateSHA256 = nativePackageR4CertificateSHA256
	authorization := validNativePackageR4RecoveryAuthorization(
		request, "VIIPER-R4-CONTRACT",
	)
	root := t.TempDir()
	authorization.Predecessor.StatePath = filepath.Join(root, "missing-state.json")
	authorization.Predecessor.InstallEvidenceDirectory = filepath.Join(root, "missing-step")
	directoryHandles, fileHandles, err :=
		lockAndValidateNativePackageR4RecoveryEvidence(authorization)
	if err == nil {
		closeNativePackageUninstallHandles(fileHandles)
		closeNativePackageUninstallHandles(directoryHandles)
		t.Fatal("R4 recovery admitted absent retained predecessor evidence")
	}
}

func TestNativePackageRecoveryMarkerCrashCutAdmission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		present      int
		allowPartial bool
		counts       nativePackageRecoveryTrustCounts
		wantErr      bool
	}{
		{"initial before marker", 0, false, nativePackageRecoveryTrustCounts{1, 1}, false},
		{"retry crash before marker unchanged", 0, true, nativePackageRecoveryTrustCounts{1, 1}, false},
		{"retry unexplained root missing", 0, true, nativePackageRecoveryTrustCounts{0, 1}, true},
		{"retry unexplained publisher missing", 0, true, nativePackageRecoveryTrustCounts{1, 0}, true},
		{"retry after pending root missing", 1, true, nativePackageRecoveryTrustCounts{0, 1}, false},
		{"retry after pending both missing", 1, true, nativePackageRecoveryTrustCounts{0, 0}, false},
		{"initial cannot consume old marker", 1, false, nativePackageRecoveryTrustCounts{1, 1}, true},
		{"ambiguous marker states", 2, true, nativePackageRecoveryTrustCounts{0, 0}, true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateNativePackageRecoveryMarkerAdmission(
				test.present, test.allowPartial, test.counts,
			)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestNativePackageRecoverySettledMarkerRetiresForLaterChain(t *testing.T) {
	requireNativeMutexAdministrator(t)
	path := t.TempDir() + `\failed-install-recovery-settled-v1.json`
	first := validNativePackageRecoverRequest()
	first.recoveryRootAuthorizationSHA256 = strings.Repeat("a", 64)
	firstBytes, _ := canonicalNativePackageRecoveryMarker(first)
	if err := createExactNativePackageRecoveryPreparation(path, firstBytes); err != nil {
		t.Fatalf("create first settled marker: %v", err)
	}
	if err := retireNativePackageSettledRecoveryMarker(path); err != nil {
		t.Fatalf("retire first settled marker: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("retired marker remains: %v", err)
	}
	second := first
	second.recoveryRootAuthorizationSHA256 = strings.Repeat("b", 64)
	secondBytes, _ := canonicalNativePackageRecoveryMarker(second)
	if err := createExactNativePackageRecoveryPreparation(path, secondBytes); err != nil {
		t.Fatalf("later independent recovery chain remained poisoned: %v", err)
	}
}

func TestNativePackageRecoveryPreparationPublishesOnlyCompleteBytes(t *testing.T) {
	requireNativeMutexAdministrator(t)
	request := validNativePackageRecoverRequest()
	contents, _ := canonicalNativePackageRecoveryMarker(request)
	prepublish := []string{
		"scratch-created", "scratch-written", "scratch-flushed",
		"scratch-verified", "before-publish",
	}
	for _, stage := range prepublish {
		stage := stage
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), nativePackageRecoveryMarkerPreparingName)
			cutErr := errors.New("injected cut " + stage)
			err := createExactNativePackageRecoveryPreparationWithCutpoint(
				path, contents,
				func(current string) error {
					if current == stage {
						return cutErr
					}
					return nil
				},
			)
			if !errors.Is(err, cutErr) {
				t.Fatalf("cut error=%v want=%v", err, cutErr)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("canonical marker became visible before complete publication: %v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("graceful cut retained preparation scratch: %v", entries)
			}
		})
	}

	path := filepath.Join(t.TempDir(), nativePackageRecoveryMarkerPreparingName)
	afterPublish := errors.New("injected cut after publish")
	err := createExactNativePackageRecoveryPreparationWithCutpoint(
		path, contents,
		func(current string) error {
			if current == "after-publish" {
				return afterPublish
			}
			return nil
		},
	)
	if !errors.Is(err, afterPublish) {
		t.Fatalf("post-publication cut error=%v", err)
	}
	if exists, err := readExactNativePackageRecoveryMarker(path, contents); err != nil || !exists {
		t.Fatalf("post-publication cut did not leave exact resumable canonical bytes: exists=%v error=%v", exists, err)
	}
}

func TestNativePackageRecoverySettledMarkerRejectsNoncanonicalBytes(t *testing.T) {
	requireNativeMutexAdministrator(t)
	path := t.TempDir() + `\failed-install-recovery-settled-v1.json`
	request := validNativePackageRecoverRequest()
	contents, _ := canonicalNativePackageRecoveryMarker(request)
	contents = append(contents, '\n')
	if err := createExactNativePackageRecoveryPreparation(path, contents); err != nil {
		t.Fatalf("create malformed settled marker: %v", err)
	}
	if err := retireNativePackageSettledRecoveryMarker(path); err == nil {
		t.Fatal("noncanonical settled marker was deleted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("noncanonical settled marker was not preserved fail-closed: %v", err)
	}
}

func TestNativePackageRecoveryPackageMutexExcludesSuccessor(t *testing.T) {
	requireNativeMutexAdministrator(t)
	firstRelease, err := acquireNamedNativePackageMutex(nativePackageMutexName, time.Second)
	if err != nil {
		t.Fatalf("acquire recovery package mutex: %v", err)
	}
	acquired := make(chan func(), 1)
	errors := make(chan error, 1)
	go func() {
		release, acquireErr := acquireNamedNativePackageMutex(nativePackageMutexName, 3*time.Second)
		if acquireErr != nil {
			errors <- acquireErr
			return
		}
		acquired <- release
	}()
	select {
	case release := <-acquired:
		release()
		firstRelease()
		t.Fatal("concurrent successor acquired the package mutex before recovery released it")
	case err := <-errors:
		firstRelease()
		t.Fatalf("concurrent package mutex waiter failed early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	firstRelease()
	select {
	case release := <-acquired:
		release()
	case err := <-errors:
		t.Fatalf("concurrent package mutex waiter failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent successor did not acquire the package mutex after recovery release")
	}
}

func TestNativePackageRecoveryTrustLeaseRequiresAnotherOwner(t *testing.T) {
	path := t.TempDir() + `\lease-v1.lock`
	if err := os.WriteFile(path, []byte{1}, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	overlapped := windows.Overlapped{}
	if err := windows.LockFileEx(
		windows.Handle(first.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK,
		0, 1, 0, &overlapped,
	); err != nil {
		t.Fatal(err)
	}
	if err := requireNativePackageRecoveryTrustLeaseHeld(
		windows.Handle(second.Fd()),
	); err != nil {
		t.Fatalf("another-owner lease proof rejected: %v", err)
	}
	if err := windows.UnlockFileEx(
		windows.Handle(first.Fd()), 0, 1, 0, &overlapped,
	); err != nil {
		t.Fatal(err)
	}
	err = requireNativePackageRecoveryTrustLeaseHeld(windows.Handle(second.Fd()))
	if err == nil || !strings.Contains(err.Error(), "outer manager") {
		t.Fatalf("unowned lease admitted: %v", err)
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		t.Fatalf("unowned lease leaked raw lock status: %v", err)
	}
}

func TestNativePackageRecoverySourceKeepsTrustMutationInsideLocks(t *testing.T) {
	sourceBytes, err := os.ReadFile("native_package_recover_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(sourceBytes), "\r\n", "\n")
	recoveryStart := strings.Index(source, "func recoverNativePackage(")
	if recoveryStart < 0 {
		t.Fatal("recovery implementation is missing")
	}
	recoverySource := source[recoveryStart:]
	ordered := []string{
		"lockAndVerifyNativePackageFailedInstallRecoveryCapability(request)",
		"acquireNativePackageRecoveryTrustLease(",
		"acquireNamedNativePackageMutex(nativePackageMutexName",
		"acquireNativeInstallMutex(serviceBudget)",
		"validateNativePackageR4RecoveryAuthorization(",
		"lockAndValidateNativePackageR4RecoveryEvidence(authorizationValue)",
		"lockNativePackageRecoveryBrokerJournalParent()",
		`[]string{"status"}`,
		"requireNativePackageRecoveryServiceAbsent(serviceName)",
		"requireNativePackageRecoveryNoLocalTrustOwner()",
		"inspectNativePackageLocalTestTrust(",
		"prepareNativePackageRecoveryMarker(request, trustBefore)",
		"deleteExactNativePackageRecoveryCertificate(rootStore",
		"deleteExactNativePackageRecoveryCertificate(\n\t\t\ttrustedPublisherStore",
		"settleNativePackageRecoveryMarker(markerState)",
		`"recovery-receipt operation=native-package-recover`,
	}
	position := -1
	for _, fragment := range ordered {
		next := strings.Index(recoverySource, fragment)
		if next <= position {
			t.Fatalf("recovery lock/mutation contract lost ordered fragment %q", fragment)
		}
		position = next
	}
	for _, required := range []string{
		"VIIPER-TrustManager",
		"lease-v1.lock",
		"O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)",
		"O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)",
		"LOCKFILE_FAIL_IMMEDIATELY",
		"CERT_STORE_MAXIMUM_ALLOWED_FLAG",
		"CertEnumCertificatesInStore",
		"CertDeleteCertificateFromStore",
		"expectedRecoveryAuthorizationSHA256",
		"lockNativePackageInput(item.path)",
		"hashNativePackageHandle(handle)",
		"defer closeNativePackageUninstallHandles(predecessorFileHandles)",
		"defer closeNativePackageUninstallHandles(predecessorDirectoryHandles)",
		`"recover-failed-install-recordless", "--transaction-deadline-unix-ms"`,
		"BrokerTransactions exists; exact R4 recordless recovery has no authority",
		"local-test-trust-owned-v1.json",
		"trustRootAfter=0 trustTrustedPublisherAfter=0",
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("recovery source lost %q", required)
		}
	}
	if strings.Contains(source, `[]string{"remove"}`) ||
		strings.Contains(source, `[]string{"recover"}`) ||
		strings.Contains(source, `"uninstall", "--yes"`) {
		t.Fatal("journal-only recovery gained a remove/uninstall command")
	}
	if strings.Contains(recoverySource, "inspectNativePackageRecoveryTrust(") {
		t.Fatal("recovery retained the collision-blind certificate inspector")
	}
	sharedBytes, err := os.ReadFile("native_package_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	shared := strings.ReplaceAll(string(sharedBytes), "\r\n", "\n")
	for _, collisionContract := range []string{
		"countExactNativePackageLocalTestCertificateRejectingThumbprintCollisions",
		"different certificate with the same Windows SHA-1 thumbprint",
	} {
		if !strings.Contains(shared, collisionContract) {
			t.Fatalf("shared collision-rejecting trust inspector lost %q", collisionContract)
		}
	}
}

package cmd

import (
	"os"
	"strings"
	"testing"
)

func TestNativePackageLocalTestUninstallSourceContract(t *testing.T) {
	t.Parallel()
	windowsSourceBytes, err := os.ReadFile("native_package_uninstall_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	windowsSource := string(windowsSourceBytes)
	for _, fragment := range []string{
		"func (t *windowsNativePackageUninstallTransaction) LockTrust",
		"initializeNativePackageRecoveryTrustLease()",
		"acquireNativePackageRecoveryTrustLease(ctx, deadline)",
		"prepareLocalTestTrustUninstall(ctx)",
		"inspectNativePackageLocalTestTrust(",
		"transitionNativePackageLocalTestTrustRecord(",
		"paths.uninstalling",
		"proveNativePackageLocalTestTopologyAbsent(",
		"restoreNativePackageLocalTestTrustStores(",
		"t.trustPaths.cleared",
		"release local-test trust transaction",
	} {
		if !strings.Contains(windowsSource, fragment) {
			t.Fatalf("Windows local-test uninstall lost %q", fragment)
		}
	}
	if strings.Contains(windowsSource, "inspectNativePackageRecoveryTrust(") {
		t.Fatal("local-test uninstall must reject Windows SHA-1 thumbprint collisions")
	}
	preflightStart := strings.Index(windowsSource,
		"func (t *windowsNativePackageUninstallTransaction) Preflight")
	inspectStart := strings.Index(windowsSource,
		"func (t *windowsNativePackageUninstallTransaction) InspectService")
	if preflightStart < 0 || inspectStart <= preflightStart {
		t.Fatal("Windows uninstall preflight region is malformed")
	}
	preflight := windowsSource[preflightStart:inspectStart]
	trustAdmission := strings.Index(preflight, "prepareLocalTestTrustUninstall(ctx)")
	brokerReconciliation := strings.Index(preflight,
		"reconcileNativeBrokerJournalBeforeAdmission(ctx")
	if trustAdmission < 0 || brokerReconciliation <= trustAdmission {
		t.Fatal("broker reconciliation can precede durable local-test uninstall authority")
	}
	finalizeStart := strings.Index(windowsSource,
		"func (t *windowsNativePackageUninstallTransaction) FinalizeTrust")
	deleteStart := strings.Index(windowsSource,
		"func deleteNativePackageUninstallFileHandle")
	if finalizeStart < 0 || deleteStart <= finalizeStart {
		t.Fatal("Windows uninstall trust-finalization region is malformed")
	}
	finalize := windowsSource[finalizeStart:deleteStart]
	proof := strings.Index(finalize, "proveNativePackageLocalTestTopologyAbsent(")
	restore := strings.Index(finalize, "restoreNativePackageLocalTestTrustStores(")
	cleared := strings.Index(finalize, "t.trustPaths.cleared")
	if proof < 0 || restore <= proof || cleared <= restore {
		t.Fatal("trust baseline restore/cleared publication can precede exact topology proof")
	}
	closeStart := strings.Index(windowsSource,
		"func (t *windowsNativePackageUninstallTransaction) Close")
	if closeStart < 0 {
		t.Fatal("Windows uninstall close region is missing")
	}
	closeRegion := windowsSource[closeStart:]
	serviceRelease := strings.Index(closeRegion, "if t.releaseServiceMutex != nil")
	packageRelease := strings.Index(closeRegion, "if t.releasePackageMutex != nil")
	trustRelease := strings.Index(closeRegion, "if t.releaseTrustLease != nil")
	if serviceRelease < 0 || packageRelease <= serviceRelease || trustRelease <= packageRelease {
		t.Fatal("Windows uninstall no longer releases Service -> Package -> Trust")
	}

	commandSourceBytes, err := os.ReadFile("install.go")
	if err != nil {
		t.Fatal(err)
	}
	commandSource := string(commandSourceBytes)
	for _, fragment := range []string{
		"SourceRevision",
		"LocalTestCertificatePath",
		"ExpectedLocalTestCertificateSHA256",
		"ExpectedLocalTestPackageLockSHA256",
	} {
		if !strings.Contains(commandSource, fragment) {
			t.Fatalf("public uninstall dispatch lost %q", fragment)
		}
	}
}

func TestNativePackageSettledLocalTestUninstallShortCircuitSourceContract(t *testing.T) {
	t.Parallel()
	windowsSourceBytes, err := os.ReadFile("native_package_uninstall_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	windowsSource := string(windowsSourceBytes)
	prepareStart := strings.Index(windowsSource,
		"func (t *windowsNativePackageUninstallTransaction) prepareLocalTestTrustUninstall(")
	prepareEnd := strings.Index(windowsSource,
		"func (t *windowsNativePackageUninstallTransaction) InspectService(")
	if prepareStart < 0 || prepareEnd <= prepareStart {
		t.Fatal("local-test uninstall preparation source region is missing or malformed")
	}
	prepare := windowsSource[prepareStart:prepareEnd]
	for _, fragment := range []string{
		"nativePackageProductionUninstallLocalTrustAdmission(states)",
		"proveNativePackageSettledLocalTestTopologyAbsentReadOnly(",
		`&nativePackageUninstallAlreadySettledError{state: "absent"}`,
		`&nativePackageUninstallAlreadySettledError{state: "cleared"}`,
		"nativePackageLocalTestUninstallMayMutateTopology(current.state)",
	} {
		if !strings.Contains(prepare, fragment) {
			t.Fatalf("local-test uninstall settled fence lost %q", fragment)
		}
	}
	if strings.Contains(prepare, "proveNativePackageLocalTestTopologyAbsent(") {
		t.Fatal("settled local-test uninstall still invokes mutation-capable topology recovery")
	}

	runnerSourceBytes, err := os.ReadFile("native_package_uninstall.go")
	if err != nil {
		t.Fatal(err)
	}
	runnerSource := string(runnerSourceBytes)
	runnerStart := strings.Index(runnerSource, "func runNativePackageUninstallTransaction(")
	runnerEnd := strings.Index(runnerSource, "type nativePackageUninstallRebootRequiredError")
	if runnerStart < 0 || runnerEnd <= runnerStart {
		t.Fatal("native uninstall runner source region is missing or malformed")
	}
	runner := runnerSource[runnerStart:runnerEnd]
	settledCheck := strings.Index(runner,
		"errors.As(err, &alreadySettled)")
	inspect := strings.Index(runner, "transaction.InspectService(ctx)")
	stop := strings.Index(runner, "transaction.StopService(ctx, snapshot)")
	remove := strings.Index(runner, "transaction.RemoveDriver(ctx)")
	if settledCheck < 0 || inspect <= settledCheck || stop <= inspect || remove <= stop {
		t.Fatal("settled local-test uninstall no longer short-circuits before Inspect/STOP/remove")
	}
}

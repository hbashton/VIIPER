package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNativePackageProductionSourceContract(t *testing.T) {
	t.Parallel()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
	windowsSource := readNativePackageContractFile(t,
		filepath.Join(root, "internal", "cmd", "native_package_windows.go"))
	uninstallWindowsSource := readNativePackageContractFile(t,
		filepath.Join(root, "internal", "cmd", "native_package_uninstall_windows.go"))
	helperSource := readNativePackageContractFile(t,
		filepath.Join(root, "native", "udecx", "tools", "ViiperUdeCtl.cpp"))
	transactionSource := readNativePackageContractFile(t,
		filepath.Join(root, "internal", "cmd", "native_package.go"))
	uninstallTransactionSource := readNativePackageContractFile(t,
		filepath.Join(root, "internal", "cmd", "native_package_uninstall.go"))
	serviceSource := readNativePackageContractFile(t,
		filepath.Join(root, "internal", "cmd", "native_service_install_windows.go"))

	requiredWindows := []string{
		`runDriverHelper(ctx, "verify", false)`,
		"expectedManifestSHA256",
		"--manifest-sha256",
		"nativeBrokerDirectorySDDL",
		"nativeBrokerExecutableSDDL",
		"nativePackageServiceWeakExactOwned",
		"isCanonicalNativePackageService",
		"nativeServiceConfigsEqual",
		"slices.Equal(recovery, nativeServiceRecoveryActions)",
		"service.Delete()",
		"lockNativeServiceExecutableReadOnly",
		`runDriverHelper(ctx, "install", true)`,
		"MOVEFILE_WRITE_THROUGH",
		"VerifyAuthenticatedHealth",
		"nativePackageTokenSDDL",
		"nativePackageMutexHeldByAnotherOwner",
		"runtime.LockOSThread()",
		"lockNativePackageDirectoryChain",
		"--broker-token-sha256",
		"nativePackageRebootRequiredError",
	}
	for _, fragment := range requiredWindows {
		if !strings.Contains(windowsSource, fragment) {
			t.Errorf("Windows package orchestrator lost %q", fragment)
		}
	}
	requiredUninstallWindows := []string{
		"acquireNamedNativePackageMutex(nativePackageMutexName",
		"acquireNativeInstallMutex(budget)",
		"lockNativePackageDirectoryChain(filepath.Dir(t.request.driverHelper))",
		"expectedHelperSHA256", "hashNativePackageHandle(helper)",
		"isCanonicalNativePackageService(", "nativeBrokerServiceConfiguration(",
		"nativeBrokerExecutableSDDL", "nativeCredentialDirectorySDDL(t.userSID)",
		"nativeCredentialFileSDDL(t.userSID)", "lockNativePackageUninstallFile(",
		"lockExactBrokerDirectoryChain(path)",
		"lockNativePackageUninstallLiveLog(", "promoteNativePackageUninstallLiveLog(ctx)",
		"stopNativeService(ctx, t.service", "--transaction-deadline-unix-ms",
		"exec.Command(t.request.driverHelper", "parseNativePackageRemoveProof(",
		"serviceRestoreVerified", "t.service.Delete()",
		"waitForNativePackageServiceDeletion", "deleteNativePackageUninstallFileHandle(",
		"errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE)",
		"nativePackageUninstallIsCurrentExecutable(file)",
		"windows.MOVEFILE_DELAY_UNTIL_REBOOT|windows.MOVEFILE_WRITE_THROUGH",
	}
	for _, fragment := range requiredUninstallWindows {
		if !strings.Contains(uninstallWindowsSource, fragment) {
			t.Errorf("Windows package uninstall orchestrator lost %q", fragment)
		}
	}
	if strings.Index(uninstallWindowsSource, "acquireNamedNativePackageMutex(nativePackageMutexName") >
		strings.Index(uninstallWindowsSource, "acquireNativeInstallMutex(budget)") {
		t.Error("native package uninstall no longer acquires package mutex before service mutex")
	}
	requiredHelper := []string{
		"Outcome Verify(", "ValidateCandidateInputs(", "RunBrokerInstall(",
		"--manifest-sha256", "manifest-installer-hash", "--broker-sha256",
		"--broker-token-sha256", "native-package-broker-commit",
		"RollbackInstall(prior", "broker-reboot-boundary",
		"--transaction-deadline-unix-ms", "kBrokerRollbackCeilingMs",
		"CreatePrivateNamespaceW", "WAIT_ABANDONED", "ReleaseMutex",
		"FILE_FLAG_OVERLAPPED", "CancelIoEx", "kCancelledIoDrainMs",
		"RegisterRootDeviceExact", "rollback-identity-verification",
		"CertGetEnhancedKeyUsage", "1.3.6.1.4.1.311.10.3.5.1",
		"CERT_FIND_EXT_ONLY_ENHKEY_USAGE_FLAG",
		"VerifyDriverCatalogMember", "WinVerifyTrust",
		"VerifyDriverCatalogMember(catalogPath, infPath",
		"LoadLibraryExW", "LOAD_LIBRARY_SEARCH_SYSTEM32", "GetProcAddress",
		"ValidateExactPackageDirectory", "Sha256Handle(manifest.get()",
	}
	for _, fragment := range requiredHelper {
		if !strings.Contains(helperSource, fragment) {
			t.Errorf("driver helper lost %q", fragment)
		}
	}
	requiredTransaction := []string{
		"transaction.Preflight(ctx)", "transaction.InspectService(ctx)",
		"prepared = true", "transaction.Prepare(ctx, service)",
		"transaction.InstallDriverAndBroker(ctx)",
		"transaction.VerifyAuthenticatedHealth(ctx)", "transaction.Commit(ctx)",
		"nativePackageRollbackTimeout", "context.WithoutCancel(ctx)",
	}
	for _, fragment := range requiredTransaction {
		if !strings.Contains(transactionSource, fragment) {
			t.Errorf("package transaction lost %q", fragment)
		}
	}
	for _, fragment := range []string{
		"transaction.LockPackage(ctx)", "transaction.LockService(ctx)",
		"transaction.Preflight(ctx)", "transaction.InspectService(ctx)",
		"restoreArmed = snapshot.exists", "transaction.StopService(ctx, snapshot)",
		"transaction.RemoveDriver(ctx)", "serviceRestoreVerified",
		"deliberately left stopped", "transaction.RestoreService(rollbackCtx, snapshot)",
		"driverRemovalSucceeded = true", "transaction.Cleanup(cleanupCtx, snapshot)",
		"nativePackageUninstallRebootRequiredError",
	} {
		if !strings.Contains(uninstallTransactionSource, fragment) {
			t.Errorf("package uninstall transaction lost %q", fragment)
		}
	}
	for _, fragment := range []string{
		"func acquireNativeInstallMutex(", "runtime.LockOSThread()", "runtime.UnlockOSThread()",
	} {
		if !strings.Contains(serviceSource, fragment) {
			t.Errorf("native broker service mutex lost %q", fragment)
		}
	}
	for name, source := range map[string]string{
		"Windows package orchestrator":           windowsSource,
		"Windows package uninstall orchestrator": uninstallWindowsSource,
		"driver helper":                          helperSource,
	} {
		for _, forbidden := range []string{
			"TerminateProcess(", "exec.CommandContext(", "os.RemoveAll(",
		} {
			if strings.Contains(source, forbidden) {
				t.Errorf("%s contains unsafe %q", name, forbidden)
			}
		}
	}
	if strings.Contains(uninstallWindowsSource, ".Process.Kill(") {
		t.Error("package uninstall must not hard-kill the mutating driver helper")
	}
	if strings.Contains(uninstallWindowsSource, "lockNativePriorServiceExecutable(path)") {
		t.Error("package uninstall must not retain a non-delete-shared broker leaf before its exact DELETE-capable snapshot")
	}
	for _, forbidden := range []string{
		"removeLegacy", "snapshotLegacy", "scheduled task", "RunVIIPER", "usbip",
	} {
		if strings.Contains(uninstallWindowsSource, forbidden) {
			t.Errorf("package uninstall must not mutate unrelated legacy ownership; found %q", forbidden)
		}
	}
	if strings.Contains(helperSource, "WaitForSingleObject(processHandle.get(), INFINITE)") {
		t.Error("driver helper retained an unbounded nested broker wait")
	}
	runtimeStart := strings.Index(helperSource, "bool LockPackageFiles(")
	runtimeEnd := strings.Index(helperSource, "struct InstallOptions")
	if runtimeStart < 0 || runtimeEnd <= runtimeStart {
		t.Fatal("could not isolate the helper runtime-package contract")
	}
	runtimeContract := helperSource[runtimeStart:runtimeEnd]
	if strings.Contains(runtimeContract, "ViiperUde.pdb") {
		t.Error("driver helper retained a certification-PDB runtime dependency")
	}
	if !strings.Contains(runtimeContract,
		`L"ViiperUde.inf", L"ViiperUde.sys", L"ViiperUde.cat"`) {
		t.Error("driver helper lost the exact INF/SYS/CAT runtime package contract")
	}
	if !strings.Contains(helperSource[:runtimeStart], `"ViiperUde.pdb"`) {
		t.Error("driver helper stopped binding the certification PDB in the source manifest")
	}
	for _, forbidden := range []string{"removeLegacy", "usbip"} {
		if strings.Contains(windowsSource, forbidden) {
			t.Errorf("outer package transaction must leave legacy ownership to the authenticated broker commit; found %q", forbidden)
		}
	}
}

func readNativePackageContractFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.ReplaceAll(string(content), "\r\n", "\n")
}

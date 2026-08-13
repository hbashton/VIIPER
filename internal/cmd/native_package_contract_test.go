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
	processWaitSource := readNativePackageContractFile(t,
		filepath.Join(root, "internal", "cmd", "native_package_process_windows.go"))
	mutexSource := readNativePackageContractFile(t,
		filepath.Join(root, "internal", "cmd", "native_mutex_windows.go"))
	helperSource := readNativePackageContractFile(t,
		filepath.Join(root, "native", "udecx", "tools", "ViiperUdeCtl.cpp"))
	transactionSource := readNativePackageContractFile(t,
		filepath.Join(root, "internal", "cmd", "native_package.go"))
	uninstallTransactionSource := readNativePackageContractFile(t,
		filepath.Join(root, "internal", "cmd", "native_package_uninstall.go"))
	serviceSource := readNativePackageContractFile(t,
		filepath.Join(root, "internal", "cmd", "native_service_install_windows.go"))

	requiredWindows := []string{
		"expectedManifestSHA256",
		"expectedInfSHA256", "expectedSysSHA256", "expectedCatSHA256",
		"--manifest-sha256",
		"--expected-inf-sha256", "--expected-sys-sha256", "--expected-cat-sha256",
		"nativeBrokerDirectorySDDL",
		"nativeBrokerExecutableSDDL",
		"nativePackageServiceWeakExactOwned",
		"isCanonicalNativePackageService",
		"nativeServiceConfigsEqual",
		"slices.Equal(recovery, nativeServiceRecoveryActions)",
		"service.Delete()",
		"lockNativeServiceExecutableReadOnly",
		`runDriverHelper(ctx)`,
		"nestedBrokerCommit", "nestedBrokerHealthy", "nestedMutationStarted",
		"nestedRollbackSucceeded", "verifyExactBrokerHealth(ctx)",
		"nested native broker service rollback is unsettled",
		"MOVEFILE_WRITE_THROUGH",
		"VerifyAuthenticatedHealth",
		"nativePackageTokenSDDL",
		"nativePackageMutexHeldByAnotherOwner",
		"lockNativePackageDirectoryChain",
		"--broker-token-sha256",
		"--broker-quiesce-request-handle", "--broker-quiesce-ready-handle",
		"--broker-quiesce-abort-handle", "--broker-handoff-handle",
		"AdditionalInheritedHandles", "coordinateDriverHelper(ctx",
		"quiescePriorServiceForDriver", "releaseServiceForBrokerHandoff",
		"restoreQuiescedPriorService", "driverHelperSettled",
		"nativePackageRebootRequiredError",
		"parseNativePackageInstallProof(text, processExitCode)",
	}
	for _, fragment := range requiredWindows {
		if !strings.Contains(windowsSource, fragment) {
			t.Errorf("Windows package orchestrator lost %q", fragment)
		}
	}
	stageStart := strings.Index(windowsSource,
		"func (t *windowsNativePackageTransaction) stageCoordinationToken() error {")
	stageEnd := strings.Index(windowsSource,
		"func (t *windowsNativePackageTransaction) ensureManagedPackageDirectory() error {")
	if stageStart < 0 || stageEnd <= stageStart {
		t.Fatal("native package coordination-token implementation is missing or malformed")
	}
	stageToken := windowsSource[stageStart:stageEnd]
	closeWriter := strings.Index(stageToken, "if err := windows.CloseHandle(handle); err != nil {")
	reopenSealed := strings.Index(stageToken, "sealed, err := lockNativePackageInput(path)")
	rehashSealed := strings.Index(stageToken, "sealedHash, err := hashNativePackageHandle(sealed)")
	publishSealed := strings.Index(stageToken, "t.tokenHandle = sealed")
	if closeWriter < 0 || reopenSealed <= closeWriter || rehashSealed <= reopenSealed ||
		publishSealed <= rehashSealed {
		t.Fatal("native package coordination token is published before its write handle is sealed and revalidated")
	}
	if strings.Contains(stageToken, "t.tokenHandle = handle") {
		t.Fatal("native package transaction retains a write-capable token handle across nested broker startup")
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
		"renameNativePackageUninstallFileToTombstone(file)",
	}
	for _, fragment := range requiredUninstallWindows {
		if !strings.Contains(uninstallWindowsSource, fragment) {
			t.Errorf("Windows package uninstall orchestrator lost %q", fragment)
		}
	}
	for name, source := range map[string]string{
		"package install":   windowsSource,
		"package uninstall": uninstallWindowsSource,
	} {
		if strings.Contains(source, "command.Wait()") {
			t.Errorf("%s bypasses the retained process-handle join", name)
		}
	}
	if !strings.Contains(windowsSource, "waitNativePackageHelperCoordinated(command") {
		t.Error("package install lost the retained coordinated process-handle join")
	}
	if !strings.Contains(uninstallWindowsSource, "waitNativePackageHelper(command)") {
		t.Error("package uninstall lost the retained process-handle join")
	}
	credentialStart := strings.Index(serviceSource,
		"func createProtectedNativeCredentialStagingFile(")
	credentialEnd := strings.Index(serviceSource,
		"func replaceFileAtomically(")
	if credentialStart < 0 || credentialEnd <= credentialStart {
		t.Fatal("protected native credential staging implementation is missing or malformed")
	}
	credentialStaging := serviceSource[credentialStart:credentialEnd]
	for _, fragment := range []string{
		"nativeSecurityAttributes(sddl)",
		"windows.CREATE_NEW",
		"windows.FILE_FLAG_OPEN_REPARSE_POINT",
		"windows.FILE_FLAG_WRITE_THROUGH",
		"requireSingleNativeFileLink(handle)",
		"validateNativeSecurityDescriptor(handle, sddl)",
	} {
		if !strings.Contains(credentialStaging, fragment) {
			t.Errorf("native credential staging lost %q", fragment)
		}
	}
	if strings.Contains(serviceSource, `os.CreateTemp(directory, ".viiper-key-*.tmp")`) ||
		strings.Contains(serviceSource,
			"applyNativeACLToHandle(windows.Handle(temporary.Fd())") {
		t.Fatal("native credential staging is created with a weak ACL before post-creation repair")
	}
	if strings.Contains(uninstallWindowsSource,
		"scheduleNativePackageUninstallFileAtReboot(file.path)") {
		t.Error("native uninstall schedules a reusable canonical broker path for reboot deletion")
	}
	for _, fragment := range []string{
		"process.WithHandle(", "windows.DuplicateHandle(", "windows.SYNCHRONIZE",
		"windows.WaitForSingleObject", "windows.INFINITE",
		"join.complete(command.Wait())", "nativePackageProcessWaitIndeterminateError",
	} {
		if !strings.Contains(processWaitSource, fragment) {
			t.Errorf("native package helper process join lost %q", fragment)
		}
	}
	if strings.Index(uninstallWindowsSource, "acquireNamedNativePackageMutex(nativePackageMutexName") >
		strings.Index(uninstallWindowsSource, "acquireNativeInstallMutex(budget)") {
		t.Error("native package uninstall no longer acquires package mutex before service mutex")
	}
	requiredHelper := []string{
		"Outcome Verify(", "ValidateCandidateInputs(", "RunBrokerInstall(",
		"--manifest-sha256", "manifest-installer-hash", "--broker-sha256",
		"--expected-inf-sha256", "--expected-sys-sha256", "--expected-cat-sha256",
		"--broker-token-sha256", "native-package-broker-commit",
		"BuildBrokerCommitCommandLine(", "--expected-token-sha-256",
		"--expected-broker-sha-256",
		"ParseBrokerCommitProof", "driverRollbackAuthorized", "CreatePipe(",
		"PROC_THREAD_ATTRIBUTE_HANDLE_LIST", "kMaximumBrokerProofBytes",
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
		"RequestBrokerQuiescence", "SignalBrokerHandoff",
		"--broker-quiesce-request-handle", "--broker-quiesce-ready-handle",
		"--broker-quiesce-abort-handle", "--broker-handoff-handle",
	}
	for _, fragment := range requiredHelper {
		if !strings.Contains(helperSource, fragment) {
			t.Errorf("driver helper lost %q", fragment)
		}
	}
	for _, obsolete := range []string{
		"--expected-token-sha256", "--expected-broker-sha256",
	} {
		if strings.Contains(helperSource, obsolete) {
			t.Errorf("driver helper retained obsolete nested broker option %q", obsolete)
		}
	}
	if strings.Contains(helperSource, "UpdateDriverForPlugAndPlayDevicesW(") {
		t.Error("driver helper must bind only an exact selected preinstalled package with DiInstallDevice")
	}
	if !strings.Contains(helperSource, "InstallPreinstalledDriverOnDevice(") ||
		!strings.Contains(helperSource, "DiInstallDevice(") {
		t.Error("driver helper lost exact preinstalled-driver selection and DiInstallDevice binding")
	}
	upgradeRemove := strings.Index(helperSource, `L"upgrade-deadline-before-device-removal"`)
	upgradeAbsent := strings.Index(helperSource, "CaptureSnapshot(&afterRemoval")
	upgradeStage := strings.Index(helperSource, "DiInstallDriverW(nullptr, candidate.infPath.c_str()")
	upgradeIdentity := strings.Index(helperSource, "ExactRootRegistrationMode::Upgrade")
	upgradeBind := -1
	if upgradeIdentity >= 0 {
		if relative := strings.Index(helperSource[upgradeIdentity:],
			"InstallPreinstalledDriverOnDevice("); relative >= 0 {
			upgradeBind = upgradeIdentity + relative
		}
	}
	if upgradeRemove < 0 || upgradeAbsent <= upgradeRemove ||
		upgradeStage <= upgradeAbsent || upgradeIdentity <= upgradeStage ||
		upgradeBind <= upgradeIdentity {
		t.Error("driver upgrade no longer removes and proves the captured root absent before staging, exact-identity recreation, and binding")
	}
	if strings.Contains(windowsSource, `strings.Contains(text, "result=success operation=install")`) {
		t.Error("native package install must parse one exact helper outcome instead of accepting a success substring")
	}
	backupMove := strings.Index(windowsSource,
		"moveNativePackageFile(t.destination, backupPath, false)")
	backupPublish := strings.Index(windowsSource, "t.backupPath = backupPath")
	if backupMove < 0 || backupPublish < 0 || backupPublish < backupMove {
		t.Error("native package rollback path is published before the prior broker rename succeeds")
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
	if !strings.Contains(serviceSource, "func acquireNativeInstallMutex(") {
		t.Error("native broker service mutex wrapper was removed")
	}
	for _, fragment := range []string{
		"CreateBoundaryDescriptorW", "AddSIDToBoundaryDescriptor",
		"CreatePrivateNamespaceW", "OpenPrivateNamespaceW",
		"windows.WinBuiltinAdministratorsSid", "nativeMutexObjectSDDL",
		"runtime.LockOSThread()", "runtime.UnlockOSThread()",
	} {
		if !strings.Contains(mutexSource, fragment) {
			t.Errorf("shared native private mutex namespace lost %q", fragment)
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

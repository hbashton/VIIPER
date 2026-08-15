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
		"removeWeakExactOwnedService",
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
		"RollbackInstall(", "broker-reboot-boundary",
		"--transaction-deadline-unix-ms", "kBrokerRollbackCeilingMs",
		"kDriverRollbackCeilingMs",
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
		"SetupCopyOEMInfW(", "SP_COPY_NOOVERWRITE", "ERROR_FILE_EXISTS",
		"SetupUninstallOEMInfW(", "RemoveStagedCandidateExact(",
		"VerifyPackageInventory(", "packageStagedHere", "bindingMutationStarted",
		"stage-package-inventory-verification", "stage-root-binding-verification",
		"stage-concurrent-publication", "post-quiescence-package-inventory-verification",
		"post-quiescence-root-verification", "final-pre-bind-root-topology-verification",
		"final-pre-bind-package-inventory-verification",
		"final-pre-bind-root-verification", "post-bind-package-inventory-verification",
		"PreparePreinstalledDriverOnDevice(", "CommitPreparedDriverBinding(",
		"requirePristineRuntime", "RequiresDriverMutation(",
		"RequiresPristineRuntimeProof(", "RuntimeStatsArePristine(",
		"AbiCompatibilityProfile", "{12, 29, 152, true}",
		"{11, 29, 144, false}", "{10, 13, 144, false}",
		"AbiCompatibilityProfilesAreValid()", "IsAbiRetryEligible(",
		"AbiHealthPurpose::PristineUpgrade", "AbiHealthPurpose::PristineRecheck",
		"AbiHealthPurpose::RollbackHealth", "AbiNegotiationResponseMatchesProfile(",
		"StatsRecordMatchesProfile(", "offsetof(VIIPER_UDE_STATS, ReservedPorts) == 144",
		"rollback-runtime-start-verification", "rollback-runtime-abi-profile",
		"rollback-stopped-state-verification", "RollbackLifecycleStateMatches(",
		"self-test-rollback-lifecycle",
		"transaction-deadline-before-broker-quiescence",
		"MarkTransactionMutationStarted();", "recoveredReceipt",
		"IOCTL_VIIPER_UDE_QUERY_STATS",
		"upgrade-runtime-reboot-boundary",
		"self-test-pristine-runtime-decision", "self-test-pristine-runtime-stats",
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
	if !strings.Contains(helperSource, "InstallPreinstalledDriverOnDevice(") ||
		!strings.Contains(helperSource, "DiInstallDevice(") {
		t.Error("driver helper lost exact preinstalled-driver selection and DiInstallDevice binding")
	}
	installStart := strings.Index(helperSource, "Outcome Install(const InstallOptions& options)")
	installEnd := strings.Index(helperSource, "struct PackageBackup {")
	if installStart < 0 || installEnd <= installStart {
		t.Fatal("driver helper forward install transaction is missing or malformed")
	}
	forwardInstall := helperSource[installStart:installEnd]
	for _, forbidden := range []string{
		"DiInstallDriverW(", "UpdateDriverForPlugAndPlayDevicesW(",
		`L"upgrade-deadline-before-device-removal"`,
		"ExactRootRegistrationMode", "RegisterRootDeviceExact(",
	} {
		if strings.Contains(forwardInstall, forbidden) {
			t.Errorf("forward install retained remove/recreate or device-auto-binding operation %q", forbidden)
		}
	}
	quiescenceStart := strings.Index(helperSource,
		"bool RequestBrokerQuiescence(const InstallOptions& options")
	quiescenceEnd := strings.Index(helperSource,
		"bool SignalBrokerHandoff(")
	if quiescenceStart < 0 || quiescenceEnd <= quiescenceStart {
		t.Fatal("broker quiescence implementation is missing or malformed")
	}
	quiescenceSource := helperSource[quiescenceStart:quiescenceEnd]
	quiescenceDeadline := strings.Index(quiescenceSource,
		`L"transaction-deadline-before-broker-quiescence"`)
	quiescenceSignal := strings.Index(quiescenceSource,
		"SetEvent(options.brokerQuiesceRequest)")
	if quiescenceDeadline < 0 || quiescenceSignal <= quiescenceDeadline {
		t.Error("broker quiescence can signal a healthy broker after the package deadline")
	}
	helperStageStart := strings.Index(helperSource, "bool StageCandidatePackage(")
	helperStageEnd := strings.Index(helperSource, "bool RemoveDevice(")
	if helperStageStart < 0 || helperStageEnd <= helperStageStart {
		t.Fatal("add-only package staging implementation is missing or malformed")
	}
	stageSource := helperSource[helperStageStart:helperStageEnd]
	stageMutationMark := strings.Index(stageSource, "MarkTransactionMutationStarted();")
	setupCopy := strings.Index(stageSource, "SetupCopyOEMInfW(")
	stageOwnership := strings.Index(stageSource, "*stagedHere = true;")
	receiptValidation := strings.Index(stageSource, "const size_t destinationLength")
	receiptRecovery := strings.Index(stageSource, "recoveredReceipt")
	if stageMutationMark < 0 || setupCopy <= stageMutationMark ||
		stageOwnership <= setupCopy || receiptValidation <= stageOwnership ||
		receiptRecovery <= receiptValidation {
		t.Error("SetupCopy fault interleavings no longer preserve mutation classification, success-only ownership, and exact receipt recovery")
	}
	commitStart := strings.Index(helperSource, "bool CommitPreparedDriverBinding(")
	commitEnd := strings.Index(helperSource, "bool InstallPreinstalledDriverOnDevice(")
	if commitStart < 0 || commitEnd <= commitStart {
		t.Fatal("prepared selected-device binding commit is missing or malformed")
	}
	commitSource := helperSource[commitStart:commitEnd]
	commitDeadline := strings.Index(commitSource,
		`L"transaction-deadline-before-selected-device-binding"`)
	commitMutation := strings.Index(commitSource, "MarkTransactionMutationStarted();")
	commitSelection := strings.Index(commitSource, "SetupDiSetSelectedDriverW(")
	commitInstall := strings.Index(commitSource, "DiInstallDevice(")
	if commitDeadline < 0 || commitMutation <= commitDeadline ||
		commitSelection <= commitMutation || commitInstall <= commitSelection ||
		strings.Contains(commitSource[commitSelection:commitInstall],
			"CheckTransactionDeadline(") {
		t.Error("prepared binding commit no longer performs one deadline check followed immediately by selected-driver and DiInstallDevice mutation")
	}
	abiHealthStart := strings.Index(helperSource, "bool VerifyAbiHealth(")
	abiHealthEnd := strings.Index(helperSource, "bool VerifyInstalledBinding(")
	if abiHealthStart < 0 || abiHealthEnd <= abiHealthStart {
		t.Fatal("ABI health implementation is missing or malformed")
	}
	abiHealthSource := helperSource[abiHealthStart:abiHealthEnd]
	for _, fragment := range []string{
		"profiles = kAbiCompatibilityProfiles.data();",
		"profileCount = kAbiCompatibilityProfiles.size();",
		"IssueAbiNegotiation(device.get(), deadlineUnixMs, profiles[index].minor",
		"IsAbiRetryEligible(purpose, expectedBuildIdentity, *error)",
		"AbiNegotiationResponseMatchesProfile(",
		"StatsRecordMatchesProfile(",
	} {
		if !strings.Contains(abiHealthSource, fragment) {
			t.Errorf("bounded ABI negotiation lost %q", fragment)
		}
	}
	mutationDecision := strings.Index(forwardInstall,
		"const bool driverMutation =")
	stageCall := strings.Index(forwardInstall, "if (!StageCandidatePackage(")
	stageInventory := strings.Index(forwardInstall,
		`L"stage-package-inventory-verification"`)
	stageRootProof := strings.Index(forwardInstall, `L"stage-root-binding-verification"`)
	quiesce := strings.Index(forwardInstall, "RequestBrokerQuiescence(options")
	postQuiesceInventory := strings.Index(forwardInstall,
		`L"post-quiescence-package-inventory-verification"`)
	postQuiesceRootProof := strings.Index(forwardInstall,
		`L"post-quiescence-root-verification"`)
	pristineDecision := strings.Index(forwardInstall,
		"const bool requiresPristineRuntimeProof =")
	pristineProof := -1
	if pristineDecision >= 0 {
		if relative := strings.Index(forwardInstall[pristineDecision:],
			"AbiHealthPurpose::PristineUpgrade"); relative >= 0 {
			pristineProof = pristineDecision + relative
		}
	}
	prepareBinding := strings.Index(forwardInstall,
		"PreparePreinstalledDriverOnDevice(")
	finalTopology := -1
	finalInventory := -1
	finalRootProof := -1
	finalPristineProof := -1
	commitBinding := -1
	if prepareBinding >= 0 {
		if relative := strings.Index(forwardInstall[prepareBinding:],
			`L"final-pre-bind-root-topology-verification"`); relative >= 0 {
			finalTopology = prepareBinding + relative
		}
		if relative := strings.Index(forwardInstall[prepareBinding:],
			`L"final-pre-bind-package-inventory-verification"`); relative >= 0 {
			finalInventory = prepareBinding + relative
		}
		if relative := strings.Index(forwardInstall[prepareBinding:],
			`L"final-pre-bind-root-verification"`); relative >= 0 {
			finalRootProof = prepareBinding + relative
		}
		if relative := strings.Index(forwardInstall[prepareBinding:],
			"AbiHealthPurpose::PristineRecheck"); relative >= 0 {
			finalPristineProof = prepareBinding + relative
		}
		if relative := strings.Index(forwardInstall[prepareBinding:],
			"CommitPreparedDriverBinding("); relative >= 0 {
			commitBinding = prepareBinding + relative
		}
	}
	postBindInventory := strings.Index(forwardInstall,
		`L"post-bind-package-inventory-verification"`)
	if mutationDecision < 0 || stageCall <= mutationDecision ||
		stageInventory <= stageCall || stageRootProof <= stageInventory ||
		quiesce <= stageRootProof || postQuiesceInventory <= quiesce ||
		postQuiesceRootProof <= postQuiesceInventory ||
		pristineDecision <= postQuiesceRootProof || pristineProof <= pristineDecision ||
		prepareBinding <= pristineProof || finalTopology <= prepareBinding ||
		finalInventory <= finalTopology ||
		finalRootProof <= finalInventory || finalPristineProof <= finalRootProof ||
		commitBinding <= finalPristineProof || postBindInventory <= commitBinding {
		t.Error("driver replacement no longer orders add-only stage, exact inventory/root proof, broker quiescence, pristine admission, read-only driver preparation, final inventory/root/pristine proof, immediate in-place binding, and post-bind inventory proof")
	}
	if finalPristineProof >= 0 && commitBinding > finalPristineProof {
		finalProofToCommit := forwardInstall[finalPristineProof:commitBinding]
		for _, forbidden := range []string{
			"CaptureSnapshot(", "CaptureAndVerify", "FindExactDevices(",
			"PreparePreinstalledDriverOnDevice(", "VerifyPackageInventory(",
			"SetupDiBuildDriverInfoList(",
		} {
			if strings.Contains(finalProofToCommit, forbidden) {
				t.Errorf("fallible operation %q reopened the final pristine-proof to bind window", forbidden)
			}
		}
	}
	rollbackStart := strings.Index(helperSource, "bool RollbackInstall(")
	rollbackEnd := strings.Index(helperSource, "bool LockPackageFiles(")
	if rollbackStart < 0 || rollbackEnd <= rollbackStart {
		t.Fatal("driver helper install rollback is missing or malformed")
	}
	installRollback := helperSource[rollbackStart:rollbackEnd]
	restoreBinding := strings.Index(installRollback, "RestorePriorBinding(")
	removeStaged := strings.Index(installRollback, "RemoveStagedCandidateExact(")
	verifyInventory := strings.Index(installRollback, "VerifyPackageInventory(")
	if restoreBinding < 0 || removeStaged <= restoreBinding ||
		verifyInventory <= removeStaged {
		t.Error("install rollback no longer restores a mutated binding before exact staged-here cleanup and prior-inventory proof")
	}
	rollbackStarted := strings.Index(installRollback,
		"if (prior.devices[0].started)")
	rollbackStartedProof := strings.Index(installRollback,
		`L"rollback-runtime-start-verification"`)
	rollbackProfileProof := strings.Index(installRollback,
		`L"rollback-runtime-abi-profile"`)
	rollbackHealth := strings.Index(installRollback,
		"AbiHealthPurpose::RollbackHealth")
	rollbackStoppedComparator := strings.LastIndex(installRollback,
		"RollbackLifecycleStateMatches(")
	rollbackStoppedProof := strings.Index(installRollback,
		`L"rollback-stopped-state-verification"`)
	if rollbackStarted <= verifyInventory || rollbackStartedProof <= rollbackStarted ||
		rollbackProfileProof <= rollbackStartedProof || rollbackHealth <= rollbackProfileProof ||
		rollbackStoppedComparator <= rollbackHealth ||
		rollbackStoppedProof <= rollbackStoppedComparator {
		t.Error("rollback no longer proves started/problem-zero plus ABI health for a formerly-running root and exact stopped/problem state for a captured stopped root")
	}
	for _, decision := range []string{
		"CandidateDisposition::Exact, true, true, true",
		"CandidateDisposition::InstallRequired, false, false, false",
		"CandidateDisposition::Exact, false, true, false",
	} {
		if !strings.Contains(helperSource, decision) {
			t.Errorf("driver helper lost pristine-runtime decision case %q", decision)
		}
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

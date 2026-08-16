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
	brokerJournalSource := readNativePackageContractFile(t,
		filepath.Join(root, "internal", "cmd", "native_broker_journal_windows.go"))

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
		"nativePackageInstallExitError",
		"exitCode: proof.exitCode",
		"parseNativePackageInstallProof(text, processExitCode)",
		"initializeNativePackageRecoveryTrustLease",
		"acquireNativePackageRecoveryTrustLease",
		"requireNativePackageTrustRecoveryClear",
		"verifyLocalTestTrustCapability",
		"nativePackageParentIdentity",
		"decodeCanonicalNativePackageLocalTestTrustOwnership",
		"nativePackageLocalTrustPendingName",
		"nativePackageLocalTrustOwnedName",
		"nativePackageLocalTrustUninstallingName",
		"nativePackageLocalTrustClearedName",
		"publishNativePackageLocalTestTrustPreparing",
		"transitionNativePackageLocalTestTrustRecord",
		"restoreNativePackageLocalTestTrustStores",
		"inspectNativePackageLocalTestTrust",
		"countExactNativePackageLocalTestCertificateRejectingThumbprintCollisions",
		"observedThumbprint == expectedThumbprint",
		"different certificate with the same Windows SHA-1 thumbprint",
		"proveNativePackageLocalTestTopologyAbsent",
		"commitLocalTestTrust",
		"capability.ParentPID != parentPID",
		"capability.ParentCreationFileTime != parentCreationFileTime",
		"capability.TrustJournalSchema != nativePackageLocalTestTrustOwnershipSchema",
	}
	for _, fragment := range requiredWindows {
		if !strings.Contains(windowsSource, fragment) {
			t.Errorf("Windows package orchestrator lost %q", fragment)
		}
	}
	leaseStart := strings.Index(windowsSource,
		"func initializeNativePackageRecoveryTrustLease() error {")
	leaseEnd := strings.Index(windowsSource,
		"func resolveNativePackageLocalTestTrustPaths()")
	if leaseStart < 0 || leaseEnd <= leaseStart {
		t.Fatal("native fixed trust-lease initializer is missing or malformed")
	}
	leasePublication := windowsSource[leaseStart:leaseEnd]
	leaseWrite := strings.Index(leasePublication, "windows.WriteFile(lease, marker")
	leaseFlush := strings.Index(leasePublication, "windows.FlushFileBuffers(lease)")
	leaseClose := strings.Index(leasePublication,
		"if closeErr := windows.CloseHandle(lease); closeErr != nil {")
	leaseReopen := strings.Index(leasePublication, "lockNativePackageInput(temporary)")
	leaseReadback := strings.Index(leasePublication,
		"readNativePackageRecoveryFile(prepublish, 1)")
	leasePublish := strings.Index(leasePublication,
		"moveNativePackageFile(temporary, paths.lease, false)")
	if leaseWrite < 0 || leaseFlush <= leaseWrite || leaseClose <= leaseFlush ||
		leaseReopen <= leaseClose || leaseReadback <= leaseReopen ||
		leasePublish <= leaseReadback {
		t.Fatal("native fixed trust lease is published before protected flush, close, reopen, and exact readback")
	}
	for _, fragment := range []string{
		"rand.Read(nonce[:])", "windows.CREATE_NEW", "windows.FILE_FLAG_WRITE_THROUGH",
		"validateNativeFileLinkCount(information.NumberOfLinks)",
		"validateNativeSecurityDescriptor(",
		"!bytes.Equal(readback, []byte{1})",
	} {
		if !strings.Contains(leasePublication, fragment) {
			t.Errorf("native fixed trust-lease publication lost %q", fragment)
		}
	}
	preparingStart := strings.Index(windowsSource,
		"func publishNativePackageLocalTestTrustPreparing(")
	preparingEnd := strings.Index(windowsSource,
		"func transitionNativePackageLocalTestTrustRecord(")
	if preparingStart < 0 || preparingEnd <= preparingStart {
		t.Fatal("native local-test preparing publication is missing or malformed")
	}
	preparingPublication := windowsSource[preparingStart:preparingEnd]
	prepareScratch := strings.Index(preparingPublication,
		"createExactNativePackageRecoveryPreparation(temporary, contents)")
	preparePublish := strings.Index(preparingPublication,
		"moveNativePackageFile(temporary, path, false)")
	if prepareScratch < 0 || preparePublish <= prepareScratch ||
		!strings.Contains(preparingPublication, "rand.Read(nonce[:])") {
		t.Fatal("local-test preparing authority is not scratch-written and atomically no-replace published")
	}
	for _, fragment := range []string{
		"LocalTestTrustCapability",
		"ExpectedTrustCapabilitySHA256",
		"LocalTestCertificatePath",
		"ExpectedLocalTestCertificateSHA256",
		"ExpectedLocalTestPackageLockSHA256",
		"production native package requests must not carry local-test trust capability fields",
	} {
		if !strings.Contains(transactionSource, fragment) {
			t.Errorf("native package command lost parent-bound local-test field %q", fragment)
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
		"return result, proofErr", "serviceRestoreVerified", "t.service.Delete()",
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
		"AbiCompatibilityProfile", "{14, 61, 152, true}",
		"{13, 29, 152, true}",
		"{12, 29, 152, true}",
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
		"--recovery-only", "journal-binding operation=install",
		"OuterPackageMutexWitness", "VerifyHeldByOuterOwner(",
		"AcknowledgeBrokerOuterSettlement(", "DiscardBrokerSettlementTombstone(",
		"ReconcileSettledBrokerOuterSettlement(",
		"ParseBrokerSettlementRequest(", "ParseBrokerSettlementFinal(",
		"ReadProtectedBrokerSettlementFinal(",
		"ValidateBrokerSettlementFinalBinding(",
		"ValidateBrokerSettlementFinalJournal(",
		"IsBrokerOuterSettlementContinuationPhase(",
		"BrokerOuterSettlementPendingDriverDigest(",
		"LockProtectedBrokerImage(",
		"kBrokerSettlementRequestFile", "kInstallRecoverySettledPrefix",
		"kBrokerSettlementFinalFile", "kInstallRecoveryDiscardPrefix",
		`<< " retained=" << (retained ? 1 : 0)`,
	}
	for _, fragment := range requiredHelper {
		if !strings.Contains(helperSource, fragment) {
			t.Errorf("driver helper lost %q", fragment)
		}
	}
	sourceRegion := func(name, start, end string, lastStart bool) string {
		t.Helper()
		startIndex := strings.Index(helperSource, start)
		if lastStart {
			startIndex = strings.LastIndex(helperSource, start)
		}
		endIndex := -1
		if startIndex >= 0 {
			if relative := strings.Index(helperSource[startIndex+len(start):], end); relative >= 0 {
				endIndex = startIndex + len(start) + relative
			}
		}
		if startIndex < 0 || endIndex <= startIndex {
			t.Fatalf("driver helper %s source region is missing or malformed", name)
		}
		return helperSource[startIndex:endIndex]
	}
	assertOrdered := func(name, source string, fragments ...string) {
		t.Helper()
		cursor := -1
		for _, fragment := range fragments {
			relative := strings.Index(source[cursor+1:], fragment)
			if relative < 0 {
				t.Fatalf("driver helper violates %s ordering at %q", name, fragment)
			}
			cursor += relative + 1
		}
	}

	journalRequired := []string{
		"SHGetKnownFolderPath(", "FOLDERID_ProgramData",
		`kInstallRecoveryProductDirectory[] = L"VIIPER"`,
		`kInstallRecoveryComponentDirectory[] = L"UdeCx"`,
		`kInstallRecoveryTransactionsDirectory[] = L"Transactions"`,
		`kInstallRecoveryActiveDirectory[] = L"active-v2"`,
		`kInstallRecoveryJournalPrefix[] = L"journal-"`,
		"OpenStableDirectory(", "CreateOrOpenInstallRecoveryDirectory(",
		"FILE_FLAG_OPEN_REPARSE_POINT", "FILE_ATTRIBUTE_REPARSE_POINT",
		"VerifyProtectedFileSystemSecurity(", "WriteInstallJournalRecord(",
		`\"previousSha256\"`, `\"payloadSha256\"`,
		"FILE_FLAG_WRITE_THROUGH", "FlushFileBuffers(file.get())",
		"MOVEFILE_WRITE_THROUGH", "install-journal-readback",
		"Outcome Recover(", `L"recover"`, "ReconcileInstallJournal(",
		"SynchronousMutationWatchdog", "InvokeAuthoritativeSynchronousMutation(",
		"deadlineOverrun", "ManualReconciliationRequired",
		"ForwardRebootPending", "RestoreRebootPending",
		`\"direction\"`, `\"rollbackAuthorized\"`,
		"ValidateInstallJournalTransition(", "impl_->poisoned = true",
		"ValidateAndDiscardInstallJournalTemporaryFile(",
		"OpenExistingInstallRecoveryDirectory(", "MapGenericMask(",
		"StageReceiptCaptured ownership record", "exactPreRollbackInventory",
		"RootSnapshotIsAuthorizedForInstallRollback(",
		"InstallJournalNeedsRestoreRebootPending(",
		"pendingRebootBootIdentifier", "freshRebootRequired",
		"rootRegistrationInstanceId", "RootRegistrationIntentCaptured",
		"ObservePriorEmptyInstallRecoveryRoot(",
		"ReadInstallRecoveryHardwareIds(",
		"DecodeCanonicalInstallRecoveryString(",
		"RemoveAuthorizedPriorEmptyRootAfterAdmission(",
		"PartialRootRemovalEntered", "PartialRootRemovalReturned",
		"PartialRootRemovalRebootPending", "partialRootRemovalBinding",
		"partialRootRemovalBootIdentifier",
		"InstallRecoveryChainHasActive(",
		"BuildInstallRecoveryProductDirectorySecurity(",
		"VerifyProtectedProductDirectorySecurity(",
	}
	for _, fragment := range journalRequired {
		if !strings.Contains(helperSource, fragment) {
			t.Errorf("driver helper install journal lost %q", fragment)
		}
	}
	journalPhases := []string{
		"Prepared",
		"SetupCopyEntered", "SetupCopyReturned", "StageReceiptCaptured",
		"QuiesceSignalEntered", "QuiesceSignalReturned",
		"RootRegistrationIntentCaptured",
		"RootRegistrationEntered", "RootRegistrationReturned",
		"DiInstallEntered", "DiInstallReturned", "PriorAbiProfileCaptured",
		"DriverValidated",
		"BrokerHandoffEntered", "BrokerHandoffReturned",
		"BrokerChildEntered", "BrokerChildSettled",
		"BrokerOuterSettlementPending", "BrokerOuterSettled",
		"RollbackBindingEntered", "PartialRootRemovalEntered",
		"PartialRootRemovalReturned", "PartialRootRemovalRebootPending",
		"RollbackBindingReturned",
		"SetupUninstallEntered", "SetupUninstallReturned",
		"ForwardValidated", "ExactPriorRestored",
		"ForwardRebootPending", "RestoreRebootPending",
		"ManualReconciliationRequired",
	}
	for _, phase := range journalPhases {
		qualified := "InstallJournalPhase::" + phase
		if strings.Count(helperSource, qualified) < 2 {
			t.Errorf("driver helper install journal does not both define and use phase %q", phase)
		}
	}

	recoveryPathSource := sourceRegion("fixed recovery path",
		"bool ResolveInstallRecoveryPaths(", "bool GetBootIdentifier(", false)
	assertOrdered("fixed ProgramData path", recoveryPathSource,
		"SHGetKnownFolderPath(", "FOLDERID_ProgramData",
		"*product = *programData / kInstallRecoveryProductDirectory;",
		"*component = *product / kInstallRecoveryComponentDirectory;",
		"*transactions = *component / kInstallRecoveryTransactionsDirectory;",
		"*active = *transactions / kInstallRecoveryActiveDirectory;")
	for _, fragment := range []string{
		"FILE_FLAG_OPEN_REPARSE_POINT", "FILE_ATTRIBUTE_REPARSE_POINT",
		"VerifyProtectedFileSystemSecurity(",
		"CreateOrOpenInstallRecoveryDirectory(",
		"active, false, true, &activeHandle",
	} {
		if !strings.Contains(recoveryPathSource, fragment) {
			t.Errorf("driver helper fixed recovery path lost %q", fragment)
		}
	}

	journalWriterSource := sourceRegion("append-only journal writer",
		"bool WriteInstallJournalRecord(", "bool GenerateInstallTransactionId(", false)
	assertOrdered("durable journal publication", journalWriterSource,
		"BuildInstallJournalPayload(", "Sha256Data(payload, &digest",
		`\"payloadSha256\"`, "CREATE_NEW", "FILE_FLAG_WRITE_THROUGH",
		"FlushFileBuffers(file.get())", "MoveFileExW(", "MOVEFILE_WRITE_THROUGH",
		"OPEN_EXISTING", "ReadFile(file.get(), observed.data()",
		"observed != record", "trailingRead != 0",
		"state->previousDigest = digest", "++state->sequence")
	if strings.Contains(journalWriterSource, "MOVEFILE_REPLACE_EXISTING") {
		t.Error("driver helper append-only journal can replace a published record")
	}
	journalLoadSource := sourceRegion("journal chain loader",
		"bool LoadInstallJournal(", "bool RetireLoadedInstallJournal(", false)
	assertOrdered("journal hash-chain validation", journalLoadSource,
		"std::string priorDigest(kZeroSha256)",
		"ParseInstallJournalEnvelope(",
		"parsed.sequence != expectedSequence",
		"parsed.previousDigest.c_str(), priorDigest.c_str()",
		"priorDigest = digest")

	journalPrepareSource := sourceRegion("install journal preparation",
		"bool InstallJournal::Prepare(", "bool InstallJournal::Record(", false)
	assertOrdered("protected evidence before Prepared", journalPrepareSource,
		"BackupPackagesIntoDirectory(", "CopyCandidateIntoInstallJournal(",
		"impl_->state.prior = prior;", "impl_->state.candidate = candidate;",
		"impl_->state.phase = InstallJournalPhase::Prepared;",
		"WriteInstallJournalRecord(")

	journalRecordSource := sourceRegion("atomic journal record",
		"bool InstallJournal::RecordNext(", "bool InstallJournal::RecordCutpoint(", false)
	assertOrdered("atomic journal state publication", journalRecordSource,
		"ValidateInstallJournalTransition(&impl_->state, next",
		"WriteInstallJournalRecord(", "impl_->state = std::move(next);",
		"PublishInstallRecoveryEvidence(")
	if !strings.Contains(journalRecordSource, "impl_->poisoned = true") {
		t.Error("driver helper can continue after an indeterminate journal append")
	}

	brokerRunSource := sourceRegion("broker proof publication",
		"bool RunBrokerInstall(", "Outcome Install(", false)
	assertOrdered("durable broker authority publication", brokerRunSource,
		"ParseBrokerCommitProof(", "RecordBrokerProof(proof",
		"*driverRollbackAuthorized = proof.driverRollbackAuthorized;",
		"*brokerChanged = proof.changed;")

	brokerAckSource := sourceRegion("broker settlement acknowledgement",
		"bool AcknowledgeBrokerOuterSettlement(",
		"bool DiscardBrokerSettlementTombstone(", false)
	assertOrdered("outer settlement lock and journal order", brokerAckSource,
		"outerMutex.VerifyHeldByOuterOwner(",
		"transactionMutex.Acquire(",
		"ReadProtectedBrokerSettlementRequest(",
		"ParseBrokerSettlementRequest(",
		"LoadSettlementInstallJournal(",
		"ValidateBrokerSettlementJournalBinding(",
		"AppendBrokerOuterSettled(",
		"RetireInstallRecoveryActiveDirectory(")
	for _, fragment := range []string{
		"error, true, &retiredPath", "BrokerOuterSettlementPending",
		"BrokerOuterSettled", "requestSha256",
	} {
		if !strings.Contains(brokerAckSource, fragment) {
			t.Errorf("driver helper broker settlement acknowledgement lost %q", fragment)
		}
	}
	brokerDiscardSource := sourceRegion("broker settlement discard",
		"bool DiscardBrokerSettlementTombstone(",
		"void EmitBrokerSettlementAck(", false)
	assertOrdered("authenticated atomic settlement discard", brokerDiscardSource,
		"outerMutex.VerifyHeldByOuterOwner(",
		"transactionMutex.Acquire(",
		"ReadProtectedBrokerSettlementFinal(",
		"ParseBrokerSettlementFinal(",
		"ReadProtectedBrokerSettlementRequest(",
		"ParseBrokerSettlementRequest(",
		"ValidateBrokerSettlementFinalBinding(",
		"OpenSettledInstallJournalDirectory(",
		"OpenDiscardingInstallJournalDirectory(",
		"ValidateBrokerSettlementFinalJournal(",
		"RecoveryStateMatchesForward(",
		"MoveFileExW(settled.c_str(), discarding.c_str()",
		"MOVEFILE_WRITE_THROUGH",
		"OpenStableDirectory(")
	if strings.Contains(brokerDiscardSource,
		"std::filesystem::remove_all(settled") {
		t.Error("driver settlement discard deletes its authoritative tombstone in place")
	}
	for _, fragment := range []string{
		"driverPendingDigest", "brokerPendingDigest", "settlementNonce",
	} {
		if !strings.Contains(helperSource, fragment) {
			t.Errorf("driver helper broker settlement binding lost %q", fragment)
		}
	}

	watchdogSource := sourceRegion("authoritative mutation watchdog",
		"class SynchronousMutationWatchdog final", "class DeviceInfoSet final", false)
	for _, fragment := range []string{
		"completion_.get(), waitMilliseconds",
		"timedOut_.store(true", "WaitForSingleObject(completion_.get(), INFINITE)",
		"thread_.join()", "gLastSynchronousMutationTimedOut = watchdog.Complete()",
	} {
		if !strings.Contains(watchdogSource, fragment) {
			t.Errorf("driver helper authoritative watchdog lost %q", fragment)
		}
	}
	for _, forbidden := range []string{
		"CancelIoEx(", "CancelSynchronousIo(", "TerminateThread(",
		"TerminateProcess(", ".detach()",
	} {
		if strings.Contains(watchdogSource, forbidden) {
			t.Errorf("driver helper authoritative watchdog contains forbidden cancellation %q", forbidden)
		}
	}

	installEntrySource := sourceRegion("install entry",
		"Outcome Install(const InstallOptions& options)", "struct PackageBackup {", false)
	assertOrdered("install pre-mutation reconciliation", installEntrySource,
		"mutex.Acquire(", "ReconcileInstallJournal(", "ValidateCandidateInputs(",
		"CaptureSnapshot(", "installJournal.Prepare(")
	if strings.Count(installEntrySource,
		"RemoveAuthorizedPriorEmptyRootAfterAdmission(") != 2 ||
		strings.Count(installEntrySource,
			"VerifyPriorTopologyBeforePackageRollback(") != 2 ||
		strings.Count(installEntrySource,
			"!prior.devices.empty() && bindingMutationStarted") != 2 {
		t.Error("driver helper must apply receipt-bound root cleanup and strict post-removal proof in both in-process rollback branches without generic prior-empty deletion")
	}
	removeEntrySource := sourceRegion("remove entry",
		"Outcome Remove(const RemoveOptions& options)", "Outcome Recover(", false)
	assertOrdered("remove pre-mutation reconciliation", removeEntrySource,
		"mutex.Acquire(", "ReconcileRemoveJournal(", "ReconcileInstallJournal(",
		"CaptureSnapshot(", "PrepareRemoveJournal(", "ReconcileRemoveJournal(")

	removeRequired := []string{
		`kRemoveRecoveryRootDirectory[]`, `L"VIIPER-UdeCx-RemoveTransactions"`,
		`kRemoveRecoveryActiveDirectory[] = L"active-v2"`,
		"WriteRemoveJournalRecord(", `\"previousSha256\"`, `\"payloadSha256\"`,
		"ValidateRemoveJournalTransition(", "loaded->poisoned = true",
		"PrepareRemoveJournal(", "BackupPackagesIntoDirectory(",
		"DeviceRemovalEntered", "DeviceRemovalReturned", "DeviceRemovalCommitted",
		"PackageRemovalEntered", "PackageRemovalReturned", "PackageRemovalCommitted",
		"RollbackAdmitted", "RollbackPackageEntered", "RollbackPackageReturned",
		"RollbackPackageCommitted", "RollbackBindingEntered", "RollbackBindingReturned",
		"ForwardRebootPending", "RestoreRebootPending",
		"ManualReconciliationRequired", "pendingRebootBootIdentifier",
		"ObserveRemoveRootShape(", "ObserveRemovePackagePrefix(",
		"ObserveRemovePackageSubset(", "InvokeRemovePackageMutation(",
		"InvokeRestorePackageMutation(", "CurrentRemoveStateIsUninstalled(",
		"CurrentRemoveStateMatchesPrior(", "RetireRemoveRecoveryActiveDirectory(",
		"RetireLoadedRemoveJournal(", "RunRemoveJournalModelSelfTest(",
		"RunRemoveJournalRetirementSelfTest(", "RemoveExactCapturedDevice(",
		"FreshRemoveRollbackDeadline(",
		"CrossedRemoveRebootStillPendingRequiresManual(",
		`warning=\"remove-settled-cleanup-retained\"`,
		"ReconcileRemoveJournal(",
	}
	for _, fragment := range removeRequired {
		if !strings.Contains(helperSource, fragment) {
			t.Errorf("driver helper remove journal lost %q", fragment)
		}
	}
	removePrepareSource := sourceRegion("remove journal preparation",
		"bool PrepareRemoveJournal(", "enum class RemoveRootShape", false)
	assertOrdered("remove protected evidence before Prepared", removePrepareSource,
		"OpenChain(true", "PublishRemoveRecoveryEvidence(",
		"BackupPackagesIntoDirectory(", "ValidateRemoveJournalTransition(nullptr",
		"WriteRemoveJournalRecord(")
	removeRecordSource := sourceRegion("remove atomic journal record",
		"bool AppendRemoveJournalRecord(", "bool PrepareRemoveJournal(", true)
	assertOrdered("remove atomic journal state publication", removeRecordSource,
		"ValidateRemoveJournalTransition(", "WriteRemoveJournalRecord(",
		"loaded->state = std::move(next);", "PublishRemoveRecoveryEvidence(")
	if !strings.Contains(removeRecordSource, "loaded->poisoned = true") {
		t.Error("driver helper can continue after an indeterminate remove append")
	}
	removeRetireSource := sourceRegion("loaded remove journal retirement",
		"bool RetireLoadedRemoveJournal(", "bool AppendRemoveJournalRecord(", false)
	assertOrdered("remove descendant evidence release immediately before rename",
		removeRetireSource,
		"RemoveJournalPhase::ForwardValidated",
		"RemoveJournalPhase::ExactPriorRestored",
		"const std::string transactionId",
		"loaded->priorBackups.clear();", "loaded->evidenceLocks.clear();",
		"return RetireRemoveRecoveryActiveDirectory(")
	removePriorRetireSource := sourceRegion("remove prior terminal retirement",
		"bool RetireRemoveJournalAsPrior(",
		"bool RetireRemoveJournalAsUninstalled(", false)
	assertOrdered("remove prior terminal double validation", removePriorRetireSource,
		"CurrentRemoveStateMatchesPrior(",
		"RemoveJournalPhase::ExactPriorRestored", "RecordRemoveJournalPhase(",
		"CurrentRemoveStateMatchesPrior(", "RetireLoadedRemoveJournal(")
	if !strings.Contains(removePriorRetireSource,
		"if (outcome->error.recoveryBackup.empty())") {
		t.Error("prior retirement overwrites exact tombstone failure evidence with absent active-v2")
	}
	removeForwardRetireSource := sourceRegion("remove forward terminal retirement",
		"bool RetireRemoveJournalAsUninstalled(", "bool FailRemoveJournalManual(", false)
	assertOrdered("remove forward terminal double validation", removeForwardRetireSource,
		"CurrentRemoveStateIsUninstalled(",
		"RemoveJournalPhase::ForwardValidated", "RecordRemoveJournalPhase(",
		"CurrentRemoveStateIsUninstalled(", "RetireLoadedRemoveJournal(")
	if !strings.Contains(removeForwardRetireSource,
		"if (outcome->error.recoveryBackup.empty())") {
		t.Error("forward retirement overwrites exact tombstone failure evidence with absent active-v2")
	}
	removeManualSource := sourceRegion("remove manual evidence retention",
		"bool FailRemoveJournalManual(",
		"bool ReturnRemoveJournalRebootPending(", false)
	if !strings.Contains(removeManualSource, "!cause->recoveryBackup.empty()") ||
		strings.Contains(removeManualSource, "RetireLoadedRemoveJournal(") {
		t.Error("manual recovery does not preserve callee tombstone evidence or releases terminal locks")
	}

	removeDeviceSource := sourceRegion("single captured device removal",
		"bool RemoveExactCapturedDevice(", "bool RegisterRootDevice(", false)
	assertOrdered("single captured device immutable revalidation", removeDeviceSource,
		"FindExactDevices(", "LoadOwnedPackage(",
		"IsExactCapturedRemoveTarget(", "return RemoveDevice(")
	if strings.Contains(helperSource, "RemoveAllExactDevices(") {
		t.Error("driver helper retained forbidden broad all-device remove plumbing")
	}

	removeRollbackSource := sourceRegion("remove rollback recovery",
		"bool RunRemoveRollbackRecovery(", "bool AdmitRemoveRollback(", false)
	assertOrdered("interrupted binding admission reuse", removeRollbackSource,
		"ReusesInterruptedRemoveBindingAdmission(",
		"!reusingInterruptedBindingAdmission",
		"RemoveJournalPhase::RollbackBindingEntered", "ObserveRemoveRootShape(",
		"VerifyPackageInventory(", "RestorePriorBinding(")
	assertOrdered("remove rollback uses exact-absence binding authority",
		removeRollbackSource, "ObserveRemoveRootShape(",
		"root != RemoveRootShape::Absent", "VerifyPackageInventory(",
		"RestorePriorBinding(restorable,",
		"RestorePriorBindingPolicy::RemoveJournalExactAbsence")
	restoreBindingSource := sourceRegion("prior binding restore policy",
		"bool RestorePriorBinding(", "bool RollbackInstall(", false)
	assertOrdered("remove exact-absence race fails before mutation",
		restoreBindingSource, "CaptureSnapshot(",
		"RestorePriorBindingTopologyAdmitsMutation(",
		"RestorePriorBindingPolicy::InstallRollbackReconcile &&",
		"RemoveDevice(", "RegisterRootDeviceExact(",
		"InstallPreinstalledDriverOnDevice(")
	if !strings.Contains(restoreBindingSource,
		"policy == RestorePriorBindingPolicy::RemoveJournalExactAbsence") ||
		!strings.Contains(helperSource,
			`L"self-test-remove-journal-binding-exact-absence-race"`) {
		t.Error("remove rollback lost its explicit zero-mutation exact-absence race policy/model")
	}
	removeAdmissionSource := sourceRegion("remove rollback admission",
		"bool AdmitRemoveRollback(", "bool RunRemoveForwardRecovery(", false)
	assertOrdered("durable rollback admission and fresh deadline", removeAdmissionSource,
		"RemoveJournalPhase::RestoreRebootPending", "RecordRemoveJournalPhase(",
		"FreshRemoveRollbackDeadline();", "RunRemoveRollbackRecovery(")
	if strings.Contains(removeAdmissionSource, "deadlineUnixMs") {
		t.Error("forward-to-rollback admission accepts or reuses the exhausted forward deadline")
	}
	removeForwardSource := sourceRegion("remove forward recovery",
		"bool RunRemoveForwardRecovery(", "bool ReconcileRemoveJournal(", false)
	assertOrdered("crossed reboot loop fails closed", removeForwardSource,
		"CrossedRemoveRebootStillPendingRequiresManual(",
		"FailRemoveJournalManual(", "ReturnRemoveJournalRebootPending(")
	crossedRebootSource := sourceRegion("crossed remove reboot decision",
		"bool CrossedRemoveRebootStillPendingRequiresManual(",
		"bool ReusesInterruptedRemoveBindingAdmission(", false)
	for _, fragment := range []string{
		"RemoveJournalPhase::DeviceRemovalReturned", "callSucceeded",
		"freshRebootRequired", "!samePendingBoot",
	} {
		if !strings.Contains(crossedRebootSource, fragment) {
			t.Errorf("returned-to-pending crossed reboot decision lost %q", fragment)
		}
	}
	if !strings.Contains(helperSource,
		`L"self-test-remove-journal-device-returned-pending-cut"`) {
		t.Error("driver helper lost the compiled DeviceRemovalReturned-to-pending crash cut test")
	}
	if !strings.Contains(removeForwardSource, "RemoveExactCapturedDevice(") ||
		strings.Contains(removeForwardSource, "RemoveAllExactDevices(") {
		t.Error("protected forward removal is not confined to one immutable captured root")
	}

	removeRawRetireSource := sourceRegion("remove raw terminal retirement",
		"bool RetireRemoveRecoveryActiveDirectory(",
		"struct RemoveJournalStateData {", false)
	assertOrdered("remove tombstone retirement and warning evidence", removeRawRetireSource,
		"MoveFileExW(", "error->recoveryBackup = tombstone.wstring();",
		"ClearActiveRecoveryEvidence();", "std::filesystem::remove_all(",
		"gRetainedRemoveTombstoneError", "OutputDebugStringW(")
	removeReconcileSource := sourceRegion("remove startup reconciliation",
		"bool ReconcileRemoveJournal(", "struct RemoveOptions {", true)
	for _, fragment := range []string{
		"LoadRemoveJournal(", "InstallRecoveryDirectory installDirectory",
		"installExists", "ManualReconciliationRequired", "GetBootIdentifier(",
		"RunRemoveRollbackRecovery(", "RunRemoveForwardRecovery(",
	} {
		if !strings.Contains(removeReconcileSource, fragment) {
			t.Errorf("driver helper remove reconciliation lost %q", fragment)
		}
	}

	reconcileSource := sourceRegion("startup journal reconciliation",
		"bool ReconcileInstallJournal(", "const char* RemoveJournalPhaseName(", true)
	for _, fragment := range []string{
		"ForwardRebootPending && sameBoot", "RestoreRebootPending && sameBoot",
		"return rebootPending(",
		"loaded.state.phase == InstallJournalPhase::BrokerChildEntered",
		"loaded.state.phase == InstallJournalPhase::BrokerHandoffReturned",
		"loaded.state.rollbackAuthorized", "loaded.state.hasBrokerProof",
		"loaded.state.brokerProofSuccess",
		"no mutation was attempted", "InstallJournalPhase::ManualReconciliationRequired",
		"appendPartialRootRemovalEntered",
		"install-journal-partial-root-removal-inventory",
		"install-journal-pre-package-rollback-inventory",
		"ClassifyPartialRootRemovalJournalRecovery(",
	} {
		if !strings.Contains(reconcileSource, fragment) {
			t.Errorf("driver helper startup reconciliation lost %q", fragment)
		}
	}

	type phaseWrappedAPI struct {
		name, start, end, entered, wrapper, api, returned string
	}
	phaseWrappedAPIs := []phaseWrappedAPI{
		{"SetupCopyOEMInfW", "bool StageCandidatePackage(", "bool RemoveDevice(",
			"SetupCopyEntered", "InvokeAuthoritativeSynchronousMutation(",
			"SetupCopyOEMInfW(", "SetupCopyReturned"},
		{"DiInstallDevice", "bool CommitPreparedDriverBinding(",
			"bool InstallPreinstalledDriverOnDevice(", "DiInstallEntered",
			"InvokeAuthoritativeSynchronousMutation(", "DiInstallDevice(", "DiInstallReturned"},
		{"SetupUninstallOEMInfW", "bool RemoveStagedCandidateExact(",
			"bool RestorePriorBinding(", "SetupUninstallEntered",
			"InvokeAuthoritativeSynchronousMutation(", "SetupUninstallOEMInfW(",
			"SetupUninstallReturned"},
		{"broker quiescence signal", "bool RequestBrokerQuiescence(",
			"bool SignalBrokerHandoff(", "QuiesceSignalEntered", "",
			"SetEvent(options.brokerQuiesceRequest)", "QuiesceSignalReturned"},
		{"broker handoff signal", "bool SignalBrokerHandoff(",
			"bool ValidateTransactionDeadlineBudget(", "BrokerHandoffEntered", "",
			"SetEvent(options.brokerHandoff)", "BrokerHandoffReturned"},
		{"broker child", "bool RunBrokerInstall(", "Outcome Install(",
			"BrokerChildEntered", "", "CreateProcessW(", "BrokerChildSettled"},
	}
	for _, contract := range phaseWrappedAPIs {
		region := sourceRegion("phase-wrapped "+contract.name,
			contract.start, contract.end, false)
		fragments := []string{contract.entered}
		if contract.wrapper != "" {
			fragments = append(fragments, contract.wrapper)
		}
		fragments = append(fragments, contract.api, contract.returned)
		assertOrdered("phase-wrapped "+contract.name, region, fragments...)
	}
	for _, registration := range []struct {
		name, start, end string
	}{
		{"forward root registration", "bool RegisterRootDevice(",
			"bool DriverInfoUsesPublishedPackage("},
		{"restore root registration", "bool RegisterRootDeviceExact(",
			"bool IssueAbiNegotiation("},
	} {
		region := sourceRegion(registration.name, registration.start, registration.end, false)
		entered := strings.Index(region, "RootRegistrationEntered")
		mutation := strings.Index(region, "SetupDiCallClassInstaller(")
		returned := strings.LastIndex(region, "RootRegistrationReturned")
		if entered < 0 || mutation <= entered || returned <= mutation ||
			!strings.Contains(region[entered:mutation], "InvokeAuthoritativeSynchronousMutation(") {
			t.Errorf("driver helper %s is not enclosed by authoritative entered/returned phases", registration.name)
		}
	}
	forwardRegistrationSource := sourceRegion("forward root registration receipt",
		"bool RegisterRootDevice(", "bool DriverInfoUsesPublishedPackage(", false)
	assertOrdered("generated root receipt before every registration mutation",
		forwardRegistrationSource,
		"SetupDiCreateDeviceInfoW(", "DICD_GENERATE_ID",
		"SetupDiGetDeviceInstanceIdW(",
		"RecordActiveInstallJournalRootRegistrationIntent(",
		"InstallJournalPhase::RootRegistrationEntered",
		"SetupDiSetDeviceRegistryPropertyW(",
		"SetupDiCallClassInstaller(", "DIF_REGISTERDEVICE",
		"InstallJournalPhase::RootRegistrationReturned")

	partialRootSource := sourceRegion("in-process receipt-bound partial root removal",
		"bool InstallJournal::RemoveAuthorizedPriorEmptyRootAfterAdmission(",
		"bool CurrentRootIsAuthorizedForInstallRollback(", false)
	assertOrdered("partial root removal write-ahead and authoritative return",
		partialRootSource,
		"InstallJournalPhase::PartialRootRemovalEntered",
		"VerifyPackageInventory(", "observe(false, &confirmed", "RemoveDevice(",
		"RecordAuthoritativeReturn(",
		"InstallJournalPhase::PartialRootRemovalReturned", "observe(true, &after")
	for _, fragment := range []string{
		"RemoveUnboundExactRoot", "RemoveCandidateBoundExactRoot",
		"PendingExactRootRemoval", "freshRemovalReboot",
		"rootRemovalRebootPending",
	} {
		if !strings.Contains(partialRootSource, fragment) {
			t.Errorf("driver helper partial root removal lost %q", fragment)
		}
	}

	rawRootSource := sourceRegion("broad raw root topology observer",
		"bool ObservePriorEmptyInstallRecoveryRoot(",
		"bool VerifyInstallJournalRawPriorTopology(", false)
	for _, fragment := range []string{
		"ReadInstallRecoveryHardwareIds(",
		"IsInGeneratedRootDeviceNamespace(", "hardwareIds.containsExpected",
		"related.size() == 1U", "loaded.state.hasRootRegistrationIntent",
		"loaded.state.rootRegistrationInstanceId.c_str()",
		"ReadCanonicalInstallRecoveryService(",
		"ReadCanonicalInstallRecoveryDevicePropertyString(",
		"CM_PROB_WILL_BE_REMOVED", "ClassifyPartialInstallRootRecovery(",
	} {
		if !strings.Contains(rawRootSource, fragment) {
			t.Errorf("driver helper broad raw root observer lost %q", fragment)
		}
	}

	openChainSource := sourceRegion("install recovery directory chain",
		"    bool OpenChain(", "};", false)
	assertOrdered("product-only recovery discovery", openChainSource,
		"bool productExists = false;", "bool componentExists = false;",
		"bool transactionsExist = false;", "bool activeExists = false;",
		"if (!productExists) return true;",
		"if (!componentExists) return true;",
		"if (!transactionsExist) return true;",
		"*exists = InstallRecoveryChainHasActive(")
	for _, fragment := range []string{
		"BuildInstallRecoveryProductDirectorySecurity(", "*exactTargetUserSid",
		"CreateOrOpenInstallRecoveryDirectoryWithSecurity(",
		"VerifyProtectedProductDirectorySecurity(", "exactTargetUserSid",
	} {
		if !strings.Contains(openChainSource, fragment) {
			t.Errorf("driver helper install recovery chain lost %q", fragment)
		}
	}

	forwardRetireSource := sourceRegion("forward journal retirement",
		"bool InstallJournal::RetireAfterForwardValidation(",
		"bool InstallJournal::RetireAfterPriorValidation(", false)
	forwardPendingEnd := strings.Index(forwardRetireSource, "std::string expectedBuildIdentity")
	if forwardPendingEnd < 0 {
		t.Fatal("driver helper forward reboot-pending branch is missing")
	}
	forwardPending := forwardRetireSource[:forwardPendingEnd]
	if !strings.Contains(forwardPending, "InstallJournalPhase::ForwardRebootPending") ||
		strings.Contains(forwardPending, "remove_all(") ||
		strings.Contains(forwardPending, "ClearActiveRecoveryEvidence(") {
		t.Error("driver helper forward reboot-pending path does not retain journal evidence")
	}
	priorRetireSource := sourceRegion("prior journal retirement",
		"bool InstallJournal::RetireAfterPriorValidation(",
		"bool RequireJournalObject(", false)
	priorPendingEnd := strings.Index(priorRetireSource, "const auto validatePrior")
	if priorPendingEnd < 0 {
		t.Fatal("driver helper restore reboot-pending branch is missing")
	}
	priorPending := priorRetireSource[:priorPendingEnd]
	if !strings.Contains(priorPending, "InstallJournalPhase::RestoreRebootPending") ||
		strings.Contains(priorPending, "remove_all(") ||
		strings.Contains(priorPending, "ClearActiveRecoveryEvidence(") {
		t.Error("driver helper restore reboot-pending path does not retain journal evidence")
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
	if strings.Contains(forwardInstall, "InstallJournalPhase::DiInstallReturned") {
		t.Error("forward install synthesizes DiInstallReturned outside the actual API wrapper")
	}
	if !strings.Contains(forwardInstall, "InstallJournalPhase::StageReceiptCaptured") {
		t.Error("forward install lost the distinct exact stage-receipt phase")
	}
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
	stageCall := strings.Index(forwardInstall, "StageCandidatePackage(")
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
	imageIntent := strings.Index(windowsSource, "nativeBrokerPhaseImageSwitchIntent")
	atomicReplace := strings.Index(windowsSource,
		"replaceNativePackageFileAtomically(t.temporaryPath, t.destination, priorExists)")
	imageSettled := strings.Index(windowsSource, "nativeBrokerPhaseImageSwitched")
	if imageIntent < 0 || atomicReplace <= imageIntent || imageSettled <= atomicReplace {
		t.Error("native package image replacement lost intent -> atomic replace -> settled ordering")
	}
	for _, fragment := range []string{
		"copyNativeBrokerJournalImage(", "nativeBrokerJournalPriorImageName",
		"appendPhase(nativeBrokerPhasePrepared", "REPLACEFILE_WRITE_THROUGH",
	} {
		if !strings.Contains(brokerJournalSource+windowsSource, fragment) {
			t.Errorf("native broker durable image transaction lost %q", fragment)
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
		"parseNativePackageRemoveProofFields(",
		"parseOptionalNativePackageRemoveWarning(",
		"nativePackageRemoveRetainedTombstoneMaximumRunes",
		"validateNativePackageRemoveRetainedTombstone(",
		"retainedTombstoneWin32Error",
		`logger.Warn("Native remove journal retired with a retained settled tombstone"`,
	} {
		if !strings.Contains(uninstallTransactionSource, fragment) {
			t.Errorf("native remove warning proof channel lost %q", fragment)
		}
	}
	warningParserStart := strings.Index(uninstallTransactionSource,
		"func parseOptionalNativePackageRemoveWarning(")
	warningParserEnd := strings.Index(uninstallTransactionSource,
		"func parseNativePackageRemoveProof(")
	if warningParserStart < 0 || warningParserEnd <= warningParserStart {
		t.Fatal("native remove warning parser source region is missing or malformed")
	}
	warningParserSource := uninstallTransactionSource[warningParserStart:warningParserEnd]
	assertOrdered("native remove warning tuple",
		warningParserSource, `"warning"`,
		`"warningWin32Error"`, `"retainedTombstone"`,
		"validateNativePackageRemoveRetainedTombstone(",
		"*position != len(fields)")
	if strings.Contains(uninstallTransactionSource, `(?: .*)?`) {
		t.Error("native remove proof parser still accepts an arbitrary trailing field wildcard")
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

func TestNativePackageCrossModeInstallAdmissionSourceContract(t *testing.T) {
	t.Parallel()
	windowsSource := readNativePackageContractFile(t, "native_package_windows.go")
	preflightStart := strings.Index(windowsSource,
		"func (t *windowsNativePackageTransaction) Preflight(ctx context.Context) error {")
	preflightEnd := strings.Index(windowsSource,
		"func (t *windowsNativePackageTransaction) verifyLocalTestTrustCapability() error {")
	if preflightStart < 0 || preflightEnd <= preflightStart {
		t.Fatal("native package outer preflight source region is missing or malformed")
	}
	preflight := windowsSource[preflightStart:preflightEnd]
	trustInitialize := strings.Index(preflight, "initializeNativePackageRecoveryTrustLease()")
	trustAcquire := strings.Index(preflight,
		"acquireNativePackageRecoveryTrustLease(ctx, trustDeadline)")
	packageAcquire := strings.Index(preflight,
		"acquireNamedNativePackageMutex(nativePackageMutexName, packageBudget)")
	recoveryAdmission := strings.Index(preflight, "requireNativePackageTrustRecoveryClear()")
	localAdmission := strings.Index(preflight, "t.admitProductionLocalTestTrust()")
	if trustInitialize < 0 || trustAcquire <= trustInitialize ||
		packageAcquire <= trustAcquire || recoveryAdmission <= packageAcquire ||
		localAdmission <= recoveryAdmission {
		t.Fatal("outer install no longer orders Trust -> Package -> failed-recovery/local-trust admission")
	}
	if strings.Contains(preflight[:packageAcquire], "driverValidationMode") {
		t.Fatal("outer install still conditions Trust or Package acquisition on driver validation mode")
	}
	if strings.Contains(preflight, "acquireNativeInstallMutex(") {
		t.Fatal("outer install acquired Service before completing production local-trust admission")
	}

	admissionStart := strings.Index(windowsSource,
		"func (t *windowsNativePackageTransaction) admitProductionLocalTestTrust() error {")
	admissionEnd := strings.Index(windowsSource,
		"func (t *windowsNativePackageTransaction) openLocalTestTrustStores()")
	if admissionStart < 0 || admissionEnd <= admissionStart {
		t.Fatal("production local-test trust admission source region is missing or malformed")
	}
	admission := windowsSource[admissionStart:admissionEnd]
	for _, fragment := range []string{
		"t.releaseTrustLease == nil || t.releaseMutex == nil",
		"t.releaseServiceMutex != nil",
		`{state: "preparing", path: paths.preparing}`,
		`{state: "pending", path: paths.pending}`,
		`{state: "owned", path: paths.owned}`,
		`{state: "uninstalling", path: paths.uninstalling}`,
		`{state: "cleared", path: paths.cleared}`,
		"readNativePackageLocalTestTrustRecord(candidate.path)",
		"nativePackageProductionLocalTrustAdmission(states)",
		"retireNativePackageLocalTestTrustRecord(",
	} {
		if !strings.Contains(admission, fragment) {
			t.Fatalf("production local-test trust admission lost %q", fragment)
		}
	}

	readOnlyStart := strings.Index(windowsSource,
		"func proveNativePackageSettledLocalTestTopologyAbsentReadOnly(")
	readOnlyEnd := strings.Index(windowsSource,
		"func proveNativePackageNormalBrokerJournalsQuiescent(")
	if readOnlyStart < 0 || readOnlyEnd <= readOnlyStart {
		t.Fatal("settled local-test read-only topology proof is missing or malformed")
	}
	readOnlyProof := windowsSource[readOnlyStart:readOnlyEnd]
	for _, fragment := range []string{
		"requireNativePackageRecoveryServiceAbsent(serviceName)",
		"createOrOpenProtectedNativeBrokerJournalDirectory(root, false)",
		"len(entries) != 0",
		`helperPath, []string{"status"}`,
		"validateNativePackageRecoverEmptyStatus(statusOutput, statusExitCode)",
	} {
		if !strings.Contains(readOnlyProof, fragment) {
			t.Fatalf("settled local-test read-only topology proof lost %q", fragment)
		}
	}
	for _, forbidden := range []string{
		`"recover-failed-install-recordless"`,
		"reconcileNativeBrokerJournalInactiveDirectories(",
		"discardNativeBrokerJournalDirectory(",
		"retireNativePackageLocalTestTrustRecord(",
	} {
		if strings.Contains(readOnlyProof, forbidden) {
			t.Fatalf("settled local-test read-only topology proof retained mutator %q", forbidden)
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

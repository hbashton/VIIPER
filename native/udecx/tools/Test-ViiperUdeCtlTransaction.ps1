[CmdletBinding()]
param(
    [string]$SourcePath,
    [string]$BinaryPath
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ([string]::IsNullOrWhiteSpace($SourcePath)) {
    $SourcePath = Join-Path $PSScriptRoot 'ViiperUdeCtl.cpp'
}

$source = Get-Content -LiteralPath $SourcePath -Raw
$requiredContracts = [ordered]@{
    'source-manifest preflight' = 'ValidateManifest\('
    'installer manifest hash binding' = '--manifest-sha256'
    'read-only package verification' = 'Outcome Verify\('
    'catalog signature preflight' = 'SetupVerifyInfFileW\('
    'Microsoft hardware publisher gate' = 'VerifyMicrosoftHardwareInfSigner\('
    'exact SYS catalog membership' = 'VerifyDriverCatalogMember\('
    'exact INF catalog membership' = 'VerifyDriverCatalogMember\(catalogPath, infPath'
    'Windows driver catalog policy' = 'WinVerifyTrust\('
    'System32-only catalog API loading' = 'LoadLibraryExW\([\s\S]*LOAD_LIBRARY_SEARCH_SYSTEM32'
    'documented dynamic catalog API contract' = 'GetProcAddress\('
    'production hardware verification EKU' = '1\.3\.6\.1\.4\.1\.311\.10\.3\.5'
    'production attestation rejection' = '1\.3\.6\.1\.4\.1\.311\.10\.3\.5\.1'
    'signed-certificate EKU extension only' = 'CERT_FIND_EXT_ONLY_ENHKEY_USAGE_FLAG'
    'published INF capture' = 'SetupGetInfPublishedNameW\('
    'driver-store source capture' = 'SetupGetInfDriverStoreLocationW\('
    'installed INF ownership' = 'DEVPKEY_Device_DriverInfPath'
    'installed version ownership' = 'DEVPKEY_Device_DriverVersion'
    'documented package install' = 'DiInstallDriverW\('
    'documented package removal' = 'DiUninstallDriverW\('
    'ABI health negotiation' = 'IOCTL_VIIPER_UDE_NEGOTIATE'
    'pristine upgrade statistics' = 'IOCTL_VIIPER_UDE_QUERY_STATS'
    'pristine upgrade reboot boundary' = 'upgrade-runtime-reboot-boundary'
    'stopped owned upgrade skips unavailable live ABI proof' =
        'CandidateDisposition::InstallRequired &&\s*!prior\.devices\.empty\(\) && prior\.devices\[0\]\.started &&'
    'loaded-kernel build identity negotiation' = 'response\.BuildIdentity'
    'exact negotiated capability identity' = 'response\.Capabilities != negotiatedCapabilities'
    'known previous ABI negotiation' = 'kPreviousAbiMinor = 11'
    'known previous ABI capabilities' = 'kPreviousAbiCapabilities'
    'previous ABI pristine-upgrade boundary' = 'IsPreviousAbiRetryEligible\('
    'previous ABI version-mismatch retry errors' =
        'error\.code == ERROR_REVISION_MISMATCH[\s\S]{0,120}error\.code == ERROR_INVALID_PARAMETER[\s\S]{0,180}abi-negotiate-result'
    'previous ABI stats header validation' = 'stats\.Header\.Minor != negotiatedMinor'
    'previous ABI stats wire size' = 'kPreviousAbiStatsSize = 144'
    'reserved-port wire-range validation' = 'stats\.ReservedPorts > VIIPER_UDE_MAX_DEVICES'
    'stats reserved-word validation' = 'stats\.Reserved != 0'
    'reserved-port pristine-runtime gate' =
        'negotiatedMinor == VIIPER_UDE_ABI_MINOR && stats\.ReservedPorts != 0'
    'source-bound manifest identity' = 'driverBuildIdentity'
    'same-ABI stale-kernel rejection' = 'expectedBuildIdentity'
    'install rollback' = 'RollbackInstall\('
    'broker health transaction' = 'RunBrokerInstall\('
    'canonical broker proof parser' = 'ParseBrokerCommitProof\('
    'bounded broker proof channel' = 'kMaximumBrokerProofBytes'
    'bounded sanitized broker diagnostic' = 'SanitizeBrokerDiagnostic\('
    'separate nested application exit reporting' = 'nestedExitCode='
    'broker failure Win32 mapping' = 'SetError\(error, phase, ERROR_INSTALL_FAILURE'
    'ambiguous broker diagnostic rejection' = 'diagnosticRejected = true'
    'explicit inherited broker handles' = 'PROC_THREAD_ATTRIBUTE_HANDLE_LIST'
    'indeterminate broker wait retention' = 'GetExitCodeProcess\(processHandle\.get\(\), &observedExit\)'
    'production broker requirement' = 'broker-required'
    'local test broker transaction requirement' = 'options\.production \|\| options\.localTest'
    'explicit local test route' = 'validation mode must be production, controlled-test, or local-test'
    'local test manifest separation' = '"signingRoute"[\s\S]{0,6000}"LocalTest"'
    'non-release local test enforcement' = 'else if \(localTest\)[\s\S]{0,900}\*releaseValue'
    'local test signer digest shape' = 'testSignerCertificateSha256Value->size\(\) == 64'
    'local test native signer verification' = 'VerifyLocalTestPackageSigner\('
    'local test exact signer certificate digest' =
        'actualCertificateSha256 != expectedCertificateSha256'
    'local test INF and SYS catalog membership' =
        'bool VerifyLocalTestPackageSigner\([\s\S]{0,220}Error\* error\) \{[\s\S]{0,1800}VerifyDriverCatalogMember\(catalogPath, infPath[\s\S]{0,180}infPath\.parent_path\(\) / kDriverFileName'
    'staged broker hash binding' = '--broker-sha256'
    'protected package token binding' = '--broker-token-sha256'
    'inherited broker quiescence request' = '--broker-quiesce-request-handle'
    'inherited broker quiescence readiness' = '--broker-quiesce-ready-handle'
    'inherited broker quiescence abort' = '--broker-quiesce-abort-handle'
    'inherited broker service handoff' = '--broker-handoff-handle'
    'driver mutation broker quiescence' = 'RequestBrokerQuiescence\('
    'verified binding broker handoff' = 'SignalBrokerHandoff\('
    'inherited event handle validation' = 'ParseInheritedEventHandle\('
    'nested package broker commit' = 'native-package-broker-commit'
    'nested broker expected token hash option' = '--expected-token-sha-256'
    'nested broker expected executable hash option' = '--expected-broker-sha-256'
    'cooperative package deadline' = '--transaction-deadline-unix-ms'
    'same-handle manifest binding' = 'Sha256Handle\(manifest\.get\(\)'
    'final exact package enumeration' = 'ValidateExactPackageDirectory\('
    'reboot boundary rollback' = 'broker-reboot-boundary'
    'remove rollback backup' = 'BackupPackages\('
    'protected rollback directory' = 'kRollbackDirectorySecurity'
    'inherited rollback protection' = 'O:BAD:P\(A;OICI;FA;;;SY\)\(A;OICI;FA;;;BA\)'
    'unpredictable rollback directory' = 'CryptGenRandom\('
    'verified protected rollback ACLs' = 'VerifyProtectedFileSystemSecurity\('
    'protected exact rollback file copy' = 'CopyProtectedBackupFile\('
    'exact rollback package tree' = 'ValidateExactPackageDirectory\(destination'
    'durable rollback package payloads' = 'rollback-backup-file-flush'
    'immutable rollback package files' = 'LockPackageFiles\(destination, &locks'
    'pre-mutation rollback preservation' = 'ArmPreservation\('
    'protected recovery record' = 'kRecoveryRecordSecurity'
    'private recovery record staging name' = 'kRecoveryRecordTemporaryName'
    'explicit recovery record flush' = 'FlushFileBuffers\(file\.get\(\)\)'
    'atomic recovery record publish' =
        'MoveFileExW\([\s\S]{0,180}MOVEFILE_WRITE_THROUGH'
    'recovery record read-back verification' = 'recovery-record-verify'
    'prepared write-ahead recovery state' =
        '\\"state\\":\\"prepared-remove-transaction\\"'
    'manual-only recovery policy' = '\\"automaticRestore\\":false'
    'recovery signature and hash revalidation' =
        '\\"requiredValidation\\":\[\\"inf-signature\\"[\s\S]{0,180}\\"cat-sha256\\"\]'
    'recovery record path emission' = 'recoveryRecordWritten='
    'retained backup path emission' = 'recoveryBackupRetained='
    'pre-journal retained backup reporting' = 'recovery-record-not-published'
    'recovery relative path confinement' = 'IsSafeRecoveryRelativePath\('
    'unique devnode package recovery binding' = '\\"packageIndex\\":'
    'checked rollback backup cleanup' = 'if \(!backupRoot\.Cleanup\(&backups'
    'top-level exception boundary' = 'catch \(\.\.\.\)'
    'exception-safe active recovery path' = 'gActiveRecoveryRecordWritten'
    'exception-safe mutation classification' = 'gTransactionMutationStarted'
    'remove deadline parser' = 'ParseRemoveOptions\('
    'remove mutation deadline' = 'remove-deadline-before-device'
    'finite remove rollback ceiling' = 'kDriverRollbackCeilingMs'
    'remove rollback deadline' = 'remove-rollback-deadline-package'
    'transaction mutex' = 'VIIPER_UDE_DRIVER_TRANSACTION_V1'
    'protected private transaction namespace' = 'CreatePrivateNamespaceW\('
    'protected transaction object DACL' = 'D:P\(A;;GA;;;SY\)\(A;;GA;;;BA\)'
    'acquired transaction mutex ownership' = 'WaitForSingleObject\(mutex_\.get\(\), 0\)'
    'abandoned transaction recovery' = 'WAIT_ABANDONED'
    'transaction mutex release' = 'ReleaseMutex\('
    'overlapped ABI negotiation' = 'FILE_FLAG_OVERLAPPED'
    'deadline cancellation' = 'CancelIoEx\('
    'cancelled IO drain ceiling' = 'kCancelledIoDrainMs'
    'finite broker rollback ceiling' = 'kBrokerRollbackCeilingMs'
    'nested rollback budget composition' = '3ULL \* 60ULL \* 1000ULL'
    'forward root mutation deadline' = 'transaction-deadline-before-root-registration'
    'forward root property deadline' = 'transaction-deadline-before-root-properties'
    'device binding mutation deadline' = 'transaction-deadline-before-device-binding'
    'driver package mutation deadline' = 'transaction-deadline-before-driver-install'
    'selected driver mutation deadline' = 'transaction-deadline-before-driver-selection'
    'owned generated root namespace' = 'kRootDeviceName\[\] = L"VIIPERUDE"'
    'legacy generated root rollback namespace' = 'kLegacyRootDeviceName\[\] = L"USB"'
    'exact generated root identity validation' = 'IsOwnedGeneratedRootInstanceId\('
    'forward generated root identity verification' = 'verify-generated-root-instance-id'
    'post-registration cleanup state' = 'registrationSucceeded'
    'captured root namespace validation' = 'device-instance-ownership'
    'actual remove-device mutation deadline' = 'remove-deadline-before-device-mutation'
    'rollback remove-device mutation deadline' = 'rollback-deadline-before-device-removal'
    'rollback root property deadline' = 'rollback-deadline-before-root-properties'
    'rollback root registration deadline' = 'rollback-deadline-before-root-registration'
    'exact rollback devnode identity' = 'RegisterRootDeviceExact\('
    'rollback identity verification' = 'rollback-identity-verification'
    'upgrade devnode removal boundary' = 'upgrade-deadline-before-device-removal'
    'upgrade devnode absence verification' = 'upgrade-device-removal-verification'
    'exact upgrade devnode identity' = 'ExactRootRegistrationMode::Upgrade'
    'structured reboot exit' = 'ERROR_SUCCESS_REBOOT_REQUIRED'
    'guarded downgrade' = '--allow-controlled-downgrade'
}

foreach ($entry in $requiredContracts.GetEnumerator()) {
    if ($source -notmatch $entry.Value) {
        throw "ViiperUdeCtl is missing its $($entry.Key) contract."
    }
}

$orderedMutationContracts = [ordered]@{
    'driver package deadline immediately precedes mutation' =
        'CheckTransactionDeadline\(options,[\s\S]{0,180}transaction-deadline-before-driver-install[\s\S]{0,800}DiInstallDriverW\('
    'root property deadline immediately precedes mutation' =
        'transaction-deadline-before-root-properties[\s\S]{0,240}mutationStarted[\s\S]{0,180}SetupDiSetDeviceRegistryPropertyW\('
    'root registration deadline immediately precedes mutation' =
        'transaction-deadline-before-root-registration[\s\S]{0,240}SetupDiCallClassInstaller\(DIF_REGISTERDEVICE'
    'device binding deadline immediately precedes mutation' =
        'transaction-deadline-before-device-binding[\s\S]{0,500}DiInstallDevice\('
    'selected driver deadline immediately precedes mutation' =
        'transaction-deadline-before-driver-selection[\s\S]{0,300}mutationStarted[\s\S]{0,180}SetupDiSetSelectedDriverW\('
    'remove deadline immediately precedes device mutation' =
        'CheckTransactionDeadline\(transactionDeadlineUnixMs, deadlinePhase, error\)[\s\S]{0,300}mutationStarted[\s\S]{0,180}DiUninstallDevice\('
    'first-time root creation uses the owned device name' =
        'SetupDiCreateDeviceInfoW\([\s\S]{0,120}kRootDeviceName[\s\S]{0,120}DICD_GENERATE_ID'
    'registered devnode cleanup state survives post-registration validation' =
        'bool registeredAndVerified = false;[\s\S]{0,1200}createdHere = registrationSucceeded;[\s\S]{0,160}if \(registeredAndVerified\)'
    'upgrade removes and proves the captured devnode absent before staging' =
        'upgrade-deadline-before-device-removal[\s\S]{0,800}CaptureSnapshot\(&afterRemoval[\s\S]{0,1800}DiInstallDriverW\('
    'upgrade restores exact identity before exact package binding' =
        'DiInstallDriverW\([\s\S]{0,2200}prior\.devices\[0\]\.instanceId[\s\S]{0,300}ExactRootRegistrationMode::Upgrade[\s\S]{0,700}InstallPreinstalledDriverOnDevice\('
    'broker quiescence precedes all classified driver mutation' =
        'if \(driverMutation && !options\.brokerExecutable\.empty\(\)[\s\S]{0,180}RequestBrokerQuiescence\([\s\S]{0,1200}CandidateDisposition::InstallRequired'
    'broker quiescence and pristine proof precede upgrade root removal' =
        'RequestBrokerQuiescence\([\s\S]{0,5000}&outcome\.error, true[\s\S]{0,5000}upgrade-deadline-before-device-removal'
    'broker handoff follows exact binding verification and precedes nested commit' =
        'VerifyInstalledBinding\([\s\S]{0,3000}SignalBrokerHandoff\([\s\S]{0,180}RunBrokerInstall\('
    'recovery journal is published and preservation armed before mutation' =
        'BuildRemoveRecoveryRecord\([\s\S]{0,300}WriteProtectedRecoveryRecord\([\s\S]{0,240}ArmPreservation\([\s\S]{0,700}RemoveAllExactDevices\('
    'failed remove rollback preserves published evidence before return' =
        'AttachRecoveryRecord\(&rollbackError\);[\s\S]{0,180}outcome\.rollback = L"failed";[\s\S]{0,300}return outcome;'
    'verified rollback performs checked evidence cleanup' =
        'outcome\.rollback = L"succeeded";[\s\S]{0,300}backupRoot\.Cleanup\(&backups, &cleanupError\)[\s\S]{0,300}return outcome;'
    'committed removal performs checked evidence cleanup before success' =
        'if \(!backupRoot\.Cleanup\(&backups, &cleanupError\)\)[\s\S]{0,240}ExitCode::RollbackFailed;[\s\S]{0,180}return outcome;[\s\S]{0,100}outcome\.success = true;'
    'preservation disarms only after verified evidence absence' =
        'std::filesystem::exists\(path_, presenceError\)[\s\S]{0,260}if \(removalError \|\| presenceError \|\| remains\)[\s\S]{0,900}preserve_ = false;[\s\S]{0,100}ClearActiveRecoveryEvidence\(\);'
    'exception outcome distinguishes preflight from mutation' =
        'const bool changed = gTransactionMutationStarted;[\s\S]{0,180}changed[\s\S]{0,100}ExitCode::RollbackFailed : ExitCode::PreflightRejected;'
}

foreach ($entry in $orderedMutationContracts.GetEnumerator()) {
    if ($source -notmatch $entry.Value) {
        throw "ViiperUdeCtl violates its $($entry.Key) ordering contract."
    }
}

if ($source -match 'SUOI_FORCEDELETE') {
    throw 'ViiperUdeCtl must never force-delete a published INF.'
}

if ($source -match 'TerminateProcess\(') {
    throw 'ViiperUdeCtl must never hard-terminate the mutating broker transaction.'
}

if ($source -match '--expected-(?:token|broker)-sha256') {
    throw 'ViiperUdeCtl retained obsolete nested Kong SHA-256 option spelling.'
}

if ($source -match 'SetError\(error,\s*L"broker-health",\s*exitCode') {
    throw 'Nested broker application exits must not be mislabeled as Win32 errors.'
}

if ($source -match 'std::filesystem::copy_file') {
    throw 'Rollback packages must use the protected, write-through, verified exact-file copy path.'
}

foreach ($runtimeExport in @(
    'CryptCATAdminAcquireContext2',
    'CryptCATAdminCalcHashFromFileHandle2',
    'CryptCATAdminReleaseContext'
)) {
    if ($source -match ("\b" + [regex]::Escape($runtimeExport) + "\s*\(")) {
        throw "$runtimeExport must be loaded from the protected System32 Wintrust runtime, not statically imported."
    }
}

if ($source -match 'WaitForSingleObject\(processHandle\.get\(\),\s*INFINITE\)') {
    throw 'The nested broker wait must use the cooperative package deadline contract.'
}

if ($source -match 'std::max\(CurrentUnixMilliseconds\(\),\s*options\.transactionDeadlineUnixMs\)\s*\+\s*kDriverRollbackCeilingMs') {
    throw 'Remove rollback must receive a fresh finite ceiling, not the unused forward deadline plus a rollback budget.'
}

if ([regex]::Matches($source, ',\s*DICD_GENERATE_ID\s*,').Count -ne 1) {
    throw 'Generated root identities are allowed only for first-time forward creation, never rollback.'
}

if ($source -match 'SetupDiCreateDeviceInfoW\([\s\S]{0,120}className\.c_str\(\)') {
    throw 'Forward root creation must use the VIIPER-owned device-name namespace, not the INF class name.'
}

if ([regex]::Matches($source, '\bRemoveAllExactDevices\(').Count -ne 2) {
    throw 'All-device removal is allowed only for explicit forward uninstall, never rollback.'
}

if ([regex]::Matches($source, 'VerifyDriverCatalogMember\(catalogPath').Count -ne 4) {
    throw 'Production and LocalTest validation must each bind the exact INF and SYS to the exact adjacent catalog.'
}

$forceInfUses = [regex]::Matches($source, '\bDIIRFLAG_FORCE_INF\b').Count
if ($forceInfUses -ne 1 -or
    $source -notmatch 'const DWORD installFlags = downgrade \? DIIRFLAG_FORCE_INF : 0;') {
    throw 'Forced package selection must exist only behind the validated downgrade decision.'
}

$forceBindUses = [regex]::Matches($source, '\bINSTALLFLAG_FORCE\b').Count
if ($forceBindUses -ne 0) {
    throw "Selected preinstalled package binding and rollback must not use INSTALLFLAG_FORCE; found $forceBindUses uses."
}

if (-not [string]::IsNullOrWhiteSpace($BinaryPath)) {
    $resolvedBinary = Resolve-Path -LiteralPath $BinaryPath -ErrorAction Stop
    $output = & $resolvedBinary.Path self-test 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0 -or $output -notmatch 'result=success operation=self-test') {
        throw "ViiperUdeCtl deterministic self-test failed (exit $LASTEXITCODE):`n$output"
    }
}

Write-Host 'ViiperUdeCtl transaction contract is deterministic and fail-closed.'

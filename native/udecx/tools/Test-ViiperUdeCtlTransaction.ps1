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

function Get-SourceContractRegion {
    param(
        [Parameter(Mandatory = $true)][string]$Text,
        [Parameter(Mandatory = $true)][string]$Start,
        [Parameter(Mandatory = $true)][string]$End,
        [Parameter(Mandatory = $true)][string]$Name,
        [switch]$LastStart
    )

    $startIndex = if ($LastStart) {
        $Text.LastIndexOf($Start, [StringComparison]::Ordinal)
    } else {
        $Text.IndexOf($Start, [StringComparison]::Ordinal)
    }
    $endIndex = if ($startIndex -ge 0) {
        $Text.IndexOf($End, $startIndex + $Start.Length, [StringComparison]::Ordinal)
    } else {
        -1
    }
    if ($startIndex -lt 0 -or $endIndex -le $startIndex) {
        throw "ViiperUdeCtl $Name source region is missing or malformed."
    }
    return $Text.Substring($startIndex, $endIndex - $startIndex)
}

function Assert-OrderedSourceFragments {
    param(
        [Parameter(Mandatory = $true)][string]$Text,
        [Parameter(Mandatory = $true)][string[]]$Fragments,
        [Parameter(Mandatory = $true)][string]$Name
    )

    $cursor = -1
    foreach ($fragment in $Fragments) {
        $next = $Text.IndexOf(
            $fragment, $cursor + 1, [StringComparison]::Ordinal)
        if ($next -lt 0) {
            throw "ViiperUdeCtl violates its $Name ordering contract at '$fragment'."
        }
        $cursor = $next
    }
}

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
    'shared-system Windows directory identity' = 'GetSystemWindowsDirectoryW\('
    'canonical package digest comparison' = 'SameCanonicalPackageDigest\('
    'pure published INF normalization' = 'NormalizePublishedInfPath\('
    'published INF local-file validation' = 'ValidatePublishedInfPath\('
    'compiled published INF normalization regressions' =
        'self-test-published-inf-normalization'
    'read-only live SetupAPI round trip' =
        'RunPublishedInfRoundTripLiveSelfTest\('
    'prior-rollback broker-lock cut regression' =
        'RunInstallJournalBrokerLockRetirementSelfTest\('
    'installed INF ownership' = 'DEVPKEY_Device_DriverInfPath'
    'installed version ownership' = 'DEVPKEY_Device_DriverVersion'
    'documented add-only package staging' = 'SetupCopyOEMInfW\('
    'non-overwriting package staging' = 'SP_COPY_NOOVERWRITE'
    'documented idempotent package staging result' = 'copyError != ERROR_FILE_EXISTS'
    'documented package removal' = 'DiUninstallDriverW\('
    'ABI health negotiation' = 'IOCTL_VIIPER_UDE_NEGOTIATE'
    'pristine upgrade statistics' = 'IOCTL_VIIPER_UDE_QUERY_STATS'
    'pristine upgrade reboot boundary' = 'upgrade-runtime-reboot-boundary'
    'all running-root driver mutations require pristine proof' =
        'const bool requiresPristineRuntimeProof =[\s\S]{0,180}RequiresPristineRuntimeProof\('
    'already-staged exact binding is classified as a driver mutation' =
        'RequiresDriverMutation\(disposition, exactBindingHealthy\)'
    'stopped and absent roots skip unavailable live ABI proof' =
        'self-test-pristine-runtime-decision'
    'every nonzero runtime counter is rejected' =
        'self-test-pristine-runtime-stats'
    'loaded-kernel build identity negotiation' = 'response\.BuildIdentity'
    'exact negotiated capability identity' = 'response\.Capabilities == profile\.capabilities'
    'explicit ABI 1.14 profile' = '\{14, 61, 152, true\}'
    'explicit ABI 1.13 profile' = '\{13, 29, 152, true\}'
    'explicit ABI 1.12 profile' = '\{12, 29, 152, true\}'
    'explicit ABI 1.11 profile' = '\{11, 29, 144, false\}'
    'explicit ABI 1.10 profile' = '\{10, 13, 144, false\}'
    'strict ABI profile order' = 'AbiCompatibilityProfilesAreValid\(\)'
    'legacy statistics boundary assertion' = 'offsetof\(VIIPER_UDE_STATS, ReservedPorts\) == 144'
    'previous ABI pristine-upgrade boundary' = 'IsAbiRetryEligible\('
    'previous ABI version-mismatch retry errors' =
        'error\.code == ERROR_REVISION_MISMATCH[\s\S]{0,120}error\.code == ERROR_INVALID_PARAMETER[\s\S]{0,180}abi-negotiate-result'
    'exact ABI negotiation response validation' = 'AbiNegotiationResponseMatchesProfile\('
    'exact ABI statistics response validation' = 'StatsRecordMatchesProfile\('
    'previous ABI stats header validation' = 'stats\.Header\.Minor == profile\.minor'
    'previous ABI stats wire size' = 'stats\.Header\.Size == profile\.statsSize'
    'reserved-port wire-range validation' = 'stats\.ReservedPorts <= VIIPER_UDE_MAX_DEVICES'
    'stats reserved-word validation' = 'stats\.Reserved == 0'
    'reserved-port pristine-runtime gate' =
        '!profile\.hasReservedPortFields \|\| stats\.ReservedPorts == 0'
    'source-bound manifest identity' = 'driverBuildIdentity'
    'same-ABI stale-kernel rejection' = 'expectedBuildIdentity'
    'install rollback' = 'RollbackInstall\('
    'exact staged-here rollback removal' = 'SetupUninstallOEMInfW\('
    'non-forced staged-here rollback removal' =
        'SetupUninstallOEMInfW\([\s\S]{0,120}stagedCandidate\.publishedName\.c_str\(\), 0, nullptr'
    'exact rollback package inventory proof' = 'VerifyPackageInventory\('
    'formerly-running rollback start proof' = 'rollback-runtime-start-verification'
    'formerly-running rollback ABI proof' = 'AbiHealthPurpose::RollbackHealth'
    'captured stopped rollback state proof' = 'rollback-stopped-state-verification'
    'exact rollback lifecycle comparator' = 'RollbackLifecycleStateMatches\('
    'stage mutation marked before SetupCopy' =
        'MarkTransactionMutationStarted\(\);[\s\S]{0,500}SetupCopyOEMInfW\('
    'successful stage retains cleanup ownership' = '\*stagedHere = true'
    'malformed stage receipt recovery' =
        'FindPublishedCandidate\([\s\S]{0,160}recoveredReceipt'
    'post-stage exact inventory proof' = 'stage-package-inventory-verification'
    'post-quiescence exact inventory proof' = 'post-quiescence-package-inventory-verification'
    'final pre-bind exact inventory proof' = 'final-pre-bind-package-inventory-verification'
    'post-bind exact inventory proof' = 'post-bind-package-inventory-verification'
    'post-stage full root invariance proof' = 'stage-root-binding-verification'
    'post-quiescence full root invariance proof' = 'post-quiescence-root-verification'
    'prepared-driver final root invariance proof' = 'final-pre-bind-root-verification'
    'fresh global pre-bind topology proof' = 'final-pre-bind-root-topology-verification'
    'read-only compatible-driver preparation' = 'PreparePreinstalledDriverOnDevice\('
    'immediate selected-device binding commit' = 'CommitPreparedDriverBinding\('
    'exact final pristine ABI recheck' = 'AbiHealthPurpose::PristineRecheck'
    'broker deadline before quiescence signal' =
        'transaction-deadline-before-broker-quiescence[\s\S]{0,700}SetEvent\(options\.brokerQuiesceRequest\)'
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
    'canonical protected broker digest comparison' =
        'bool LockProtectedBrokerImage\([\s\S]{0,2600}!SameCanonicalSha256Digest\(observed, expectedSha256\)'
    'compiled protected broker digest regression' = 'self-test-canonical-sha256'
    'recordless failed-install recovery command' =
        'recover-failed-install-recordless'
    'recordless failed-install recovery implementation' =
        'Outcome RecoverFailedInstallRecordless\('
    'recordless failed-install active absence proof' =
        'VerifyRecordlessRecoveryActivePathAbsent\('
    'final exact package enumeration' = 'ValidateExactPackageDirectory\('
    'reboot boundary rollback' = 'broker-reboot-boundary'
    'fixed remove recovery root' =
        'kRemoveRecoveryRootDirectory\[\][\s\S]{0,60}L"VIIPER-UdeCx-RemoveTransactions"'
    'single remove active identity' =
        'kRemoveRecoveryActiveDirectory\[\] = L"active-v2"'
    'remove protected prior backup' = 'BackupPackagesIntoDirectory\('
    'protected rollback directory' = 'kRollbackDirectorySecurity'
    'inherited rollback protection' = 'O:BAD:P\(A;OICI;FA;;;SY\)\(A;OICI;FA;;;BA\)'
    'verified protected rollback ACLs' = 'VerifyProtectedFileSystemSecurity\('
    'protected exact rollback file copy' = 'CopyProtectedBackupFile\('
    'exact rollback package tree' = 'ValidateExactPackageDirectory\(destination'
    'durable rollback package payloads' = 'rollback-backup-file-flush'
    'immutable rollback package files' = 'LockPackageFiles\(destination, &locks'
    'protected recovery record' = 'kRecoveryRecordSecurity'
    'explicit recovery record flush' = 'FlushFileBuffers\(file\.get\(\)\)'
    'atomic recovery record publish' =
        'MoveFileExW\([\s\S]{0,180}MOVEFILE_WRITE_THROUGH'
    'remove record read-back verification' = 'remove-journal-readback'
    'remove hash chain' = 'WriteRemoveJournalRecord\('
    'remove automatic recovery' = 'ReconcileRemoveJournal\('
    'remove legal transitions' = 'ValidateRemoveJournalTransition\('
    'remove manual latch' = 'RemoveJournalPhase::ManualReconciliationRequired'
    'remove deterministic model' = 'RunRemoveJournalModelSelfTest\('
    'recovery record path emission' = 'recoveryRecordWritten='
    'retained backup path emission' = 'recoveryBackupRetained='
    'recovery relative path confinement' = 'IsSafeRecoveryRelativePath\('
    'exact package cutpoint identity' = '\\"activePackageIndex\\":'
    'atomic remove terminal retirement' = 'RetireRemoveRecoveryActiveDirectory\('
    'loaded remove terminal retirement' = 'RetireLoadedRemoveJournal\('
    'remove descendant lock release regression' =
        'RunRemoveJournalRetirementSelfTest\('
    'single captured remove target' = 'RemoveExactCapturedDevice\('
    'remove rollback exact-absence restore policy' =
        'RestorePriorBindingPolicy::RemoveJournalExactAbsence'
    'fresh remove rollback deadline' = 'FreshRemoveRollbackDeadline\('
    'crossed remove reboot manual latch' =
        'CrossedRemoveRebootStillPendingRequiresManual\('
    'retained settled tombstone warning' =
        'warning=\\"remove-settled-cleanup-retained\\"'
    'top-level exception boundary' = 'catch \(\.\.\.\)'
    'exception-safe active recovery path' = 'gActiveRecoveryRecordWritten'
    'exception-safe mutation classification' = 'gTransactionMutationStarted'
    'remove deadline parser' = 'ParseRemoveOptions\('
    'remove mutation deadline' = 'remove-journal-package-deadline'
    'finite remove rollback ceiling' = 'kDriverRollbackCeilingMs'
    'remove rollback deadline' = 'remove-journal-restore-package-deadline'
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
    'device binding mutation deadline' = 'transaction-deadline-before-selected-device-binding'
    'driver package mutation deadline' = 'transaction-deadline-before-driver-stage'
    'finite install rollback deadline' = 'install-rollback-deadline-staged-package'
    'selected driver mutation deadline' = 'transaction-deadline-before-selected-device-binding'
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
    'in-place existing-root binding' = 'SameEnumeratedRootState\('
    'structured reboot exit' = 'ERROR_SUCCESS_REBOOT_REQUIRED'
    'guarded downgrade' = '--allow-controlled-downgrade'
}

foreach ($entry in $requiredContracts.GetEnumerator()) {
    if ($source -notmatch $entry.Value) {
        throw "ViiperUdeCtl is missing its $($entry.Key) contract."
    }
}

$publishedInfResolver = Get-SourceContractRegion -Text $source `
    -Start 'bool GetPublishedInfPath(' `
    -End 'bool GetDriverStoreInfPath(' `
    -Name 'published INF resolver'
if ([regex]::Matches(
        $publishedInfResolver, 'SetupGetInfPublishedNameW\s*\(').Count -ne 1) {
    throw 'Published INF resolution must use exactly one SetupGetInfPublishedNameW call.'
}
if ($publishedInfResolver -match
        'SetupGetInfPublishedNameW\([\s\S]{0,300}nullptr\s*,\s*0') {
    throw 'Published INF resolution must not use a NULL/zero sizing probe.'
}
Assert-OrderedSourceFragments -Text $publishedInfResolver `
    -Name 'single-call published INF resolution' -Fragments @(
        'std::array<wchar_t, MAX_PATH> buffer{};',
        'buffer.fill(static_cast<wchar_t>(0xffffU));',
        'SetupGetInfPublishedNameW(',
        'infPath.c_str(), buffer.data()',
        'static_cast<DWORD>(buffer.size()), nullptr',
        'DecodeFixedPublishedInfReturnBuffer(',
        'GetCanonicalSystemInfDirectory(',
        'NormalizePublishedInfPath(',
        'ValidatePublishedInfPath('
    )

$stageCandidateSource = Get-SourceContractRegion -Text $source `
    -Start 'bool StageCandidatePackage(' -End 'bool RemoveDevice(' `
    -Name 'candidate package staging'
Assert-OrderedSourceFragments -Text $stageCandidateSource `
    -Name 'documented SetupAPI package round trip' -Fragments @(
        'SetupCopyOEMInfW(',
        'DecodePublishedInfReturnBuffer(',
        'NormalizePublishedInfPath(',
        'ValidatePublishedInfPath(',
        'GetDriverStoreInfPath(',
        'GetPublishedInfPath(',
        'FindPublishedCandidate('
    )
if ($stageCandidateSource.Contains(
        'GetPublishedInfPath(candidate.infPath')) {
    throw 'Candidate source INF must never be passed to SetupGetInfPublishedNameW.'
}
foreach ($fragment in @(
        'PWSTR destinationComponent = nullptr;',
        '&required, &destinationComponent',
        'destinationComponent == destination.data() + componentOffset')) {
    if (-not $stageCandidateSource.Contains($fragment)) {
        throw "Candidate staging lost SetupCopyOEMInf's exact destination receipt '$fragment'."
    }
}

$packageDigestSource = Get-SourceContractRegion -Text $source `
    -Start 'bool SamePackageBytes(' -End 'std::string PackageBytesKey(' `
    -Name 'package digest comparison'
if ([regex]::Matches(
        $packageDigestSource, 'SameCanonicalPackageDigest\(').Count -ne 3) {
    throw 'Package identity must canonically compare exactly INF, SYS, and CAT digests.'
}
$canonicalDigestComparison = Get-SourceContractRegion -Text $source `
    -Start 'bool SameCanonicalPackageDigest(' `
    -End 'bool SamePackageBytes(' `
    -Name 'allocation-free package digest comparison'
foreach ($fragment in @(
        'std::string_view right) noexcept',
        'left.size() != 64U || right.size() != 64U',
        'canonicalCharacter', 'canonicalLeft != canonicalRight')) {
    if (-not $canonicalDigestComparison.Contains($fragment)) {
        throw "Allocation-free package digest comparison lost '$fragment'."
    }
}
if ($canonicalDigestComparison.Contains('CanonicalizePackageDigest(') -or
    $canonicalDigestComparison -match 'std::string\s+canonical') {
    throw 'Noexcept package digest comparison must remain allocation-free.'
}
$packageDigestKeySource = Get-SourceContractRegion -Text $source `
    -Start 'std::string PackageBytesKey(' `
    -End 'bool GetDriverStoreInfPath(' `
    -Name 'canonical package digest key'
if ([regex]::Matches(
        $packageDigestKeySource, 'CanonicalizePackageDigest\(').Count -ne 3 -or
    -not $packageDigestKeySource.Contains(
        'return inf + ":" + sys + ":" + cat;')) {
    throw 'Package byte keys must canonicalize exactly INF, SYS, and CAT digests.'
}
if ($source -match
        '(?:infSha256|sysSha256|catSha256)\s*(?:==|!=)|(?:==|!=)\s*[^;\r\n]*(?:infSha256|sysSha256|catSha256)') {
    throw 'Package digest identity must never use case-sensitive direct comparison.'
}
foreach ($fragment in @(
        'value.size() != 64U',
        "character >= 'A' && character <= 'F'",
        "character + ('a' - 'A')",
        'PackageInfo mixedCaseCandidate = candidate;',
        'PackageInfo mismatchedCandidate = mixedCaseCandidate;',
        'PackageInfo malformedCandidate = candidate;',
        'self-test-package-digest-canonicalization')) {
    if (-not $source.Contains($fragment)) {
        throw "Canonical package digest validation lost '$fragment'."
    }
}

$normalizerSource = Get-SourceContractRegion -Text $source `
    -Start 'bool NormalizePublishedInfPath(' `
    -End 'bool DecodePublishedInfReturnBuffer(' `
    -Name 'pure published INF normalizer'
Assert-OrderedSourceFragments -Text $normalizerSource `
    -Name 'pure published INF normalization' -Fragments @(
        "value.find(L'\0')",
        'IsDriveAbsoluteCanonicalPath(systemRoot)',
        'IsDriveAbsoluteCanonicalPath(value)',
        'component == L"." || component == L".."',
        'OrdinalPathEqualsInsensitive(parent, systemRoot)',
        'IsSafePublishedInfName(fileName)'
    )
foreach ($forbidden in @(
        'GetFileAttributesW(', 'CreateFileW(', 'GetFinalPathNameByHandleW(',
        'std::filesystem::canonical(')) {
    if ($normalizerSource.Contains($forbidden)) {
        throw "Pure published INF normalization gained filesystem access '$forbidden'."
    }
}

$publishedInfBasicAttributes = Get-SourceContractRegion -Text $source `
    -Start 'constexpr DWORD kPublishedInfBasicUnsafeAttributes' `
    -End 'constexpr DWORD kPublishedInfEnumerationRecallOnOpen' `
    -Name 'published INF basic attributes'
if ($publishedInfBasicAttributes -match '0x00040000UL\s*\|' -or
    -not $source.Contains(
        'constexpr DWORD kPublishedInfEnumerationRecallOnOpen = 0x00040000UL;')) {
    throw 'RECALL_ON_OPEN must be rejected only from directory-enumeration attributes, never handle/basic EA attributes.'
}

$publishedInfValidator = Get-SourceContractRegion -Text $source `
    -Start 'bool ValidatePublishedInfPath(' `
    -End 'bool GetPublishedInfPath(' `
    -Name 'published INF local-file validator'
Assert-OrderedSourceFragments -Text $publishedInfValidator `
    -Name 'published INF local-file validation' -Fragments @(
        'path.parent_path().native(), canonicalSystemInf.native()',
        'IsSafePublishedInfName(path.filename().wstring())',
        'FindFirstFileW(path.c_str(), &enumeration)',
        'PublishedInfEnumerationAttributesAreSafe(',
        'GetFileAttributesW(path.c_str())',
        'PublishedInfBasicAttributesAreSafe(rawAttributes, false)',
        'path.c_str(), FILE_READ_ATTRIBUTES',
        'FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE',
        'OPEN_EXISTING',
        'FILE_FLAG_OPEN_REPARSE_POINT',
        'FILE_FLAG_OPEN_NO_RECALL',
        'FileAttributeTagInfo',
        'PublishedInfBasicAttributesAreSafe(',
        'GetFileType(file.get()) != FILE_TYPE_DISK',
        'GetFinalPathNameByHandleW(',
        'OrdinalPathEqualsInsensitive(finalPath, path.native())'
    )
foreach ($forbidden in @(
        'CREATE_NEW', 'CREATE_ALWAYS', 'OPEN_ALWAYS', 'TRUNCATE_EXISTING',
        'GENERIC_WRITE', 'FILE_WRITE_DATA', 'DeleteFileW(', 'MoveFileExW(')) {
    if ($publishedInfValidator.Contains($forbidden)) {
        throw "Published INF validation gained mutation capability '$forbidden'."
    }
}

$driverInfoSource = Get-SourceContractRegion -Text $source `
    -Start 'bool DriverInfoUsesPublishedPackage(' `
    -End 'struct PreparedDriverBinding {' `
    -Name 'compatible-driver package identity'
Assert-OrderedSourceFragments -Text $driverInfoSource `
    -Name 'compatible-driver system-INF containment' -Fragments @(
        'IsSafePublishedInfName(expectedPublishedName)',
        'GetPublishedInfPath(driverInfPath, &publishedPath, &ignored)',
        'OrdinalPathEqualsInsensitive('
    )
if ($driverInfoSource.Contains(
        'IsSafePublishedInfName(driverInfPath.filename().wstring())')) {
    throw 'A compatible-driver basename must not bypass system-INF containment.'
}

$priorRetirementSource = Get-SourceContractRegion -Text $source `
    -Start 'bool InstallJournal::RetireAfterPriorValidation(' `
    -End 'bool RequireJournalObject(' `
    -Name 'prior rollback retirement'
Assert-OrderedSourceFragments -Text $priorRetirementSource `
    -Name 'prior rollback lock release before retirement' -Fragments @(
        'impl_->candidateLocks.clear();',
        'impl_->brokerLock.reset();',
        'impl_->priorBackups.clear();',
        'RetireInstallRecoveryActiveDirectory('
    )

$brokerLockCutSource = Get-SourceContractRegion -Text $source `
    -Start 'bool RunInstallJournalBrokerLockRetirementSelfTest(' `
    -End 'bool RunInstallJournalModelSelfTest(' `
    -Name 'prior rollback broker-lock cut test'
Assert-OrderedSourceFragments -Text $brokerLockCutSource `
    -Name 'prior rollback broker-lock cut test' -Fragments @(
        'std::filesystem::temp_directory_path(',
        'FILE_SHARE_READ,',
        'MoveFileExW(active.c_str(), settled.c_str(), MOVEFILE_WRITE_THROUGH)',
        'brokerLock.reset();',
        'MoveFileExW(active.c_str(), settled.c_str(), MOVEFILE_WRITE_THROUGH)'
    )

$normalizationTestSource = Get-SourceContractRegion -Text $source `
    -Start 'bool RunPublishedInfNormalizationSelfTest(' `
    -End 'bool RunPublishedInfRoundTripLiveSelfTest(' `
    -Name 'published INF normalization self-test'
foreach ($fragment in @(
        'L"oem0.inf"', 'L"OEM1.INF"', 'L"oem9.inf"',
        'LR"(\\server\share\oem1.inf)"',
        'LR"(\\?\C:\Windows\INF\oem1.inf)"',
        'L"oem1.inf:stream"', 'embeddedNul.push_back',
        'unterminated.fill', 'FILE_ATTRIBUTE_REPARSE_POINT',
        'FILE_ATTRIBUTE_OFFLINE',
        'kPublishedInfEnumerationRecallOnOpen',
        'PublishedInfEnumerationAttributesAreSafe(')) {
    if (-not $normalizationTestSource.Contains($fragment)) {
        throw "Published INF normalization self-test lost '$fragment'."
    }
}

$liveRoundTripSource = Get-SourceContractRegion -Text $source `
    -Start 'bool RunPublishedInfRoundTripLiveSelfTest(' `
    -End 'bool MultiSzContains(' `
    -Name 'live published INF round trip'
Assert-OrderedSourceFragments -Text $liveRoundTripSource `
    -Name 'read-only live published INF round trip' -Fragments @(
        'QueryProcessElevation(&elevated, error)',
        'if (!elevated) return true;',
        'EnumerateOwnedPackages(&packages, error)',
        'if (packages.empty()) return true;',
        'GetDriverStoreInfPath(',
        'GetPublishedInfPath(',
        'std::filesystem::path(package.publishedName)',
        'FindPublishedCandidate(',
        'SamePackageBytes(unique, package)'
    )
foreach ($forbidden in @(
        'SetupCopyOEMInfW(', 'SetupUninstallOEMInfW(',
        'DiInstallDevice(', 'DiUninstallDriverW(', 'DiUninstallDevice(',
        'RemoveDevice(', 'MoveFileExW(', 'DeleteFileW(')) {
    if ($liveRoundTripSource.Contains($forbidden)) {
        throw "Live SetupAPI round-trip self-test gained forbidden mutation '$forbidden'."
    }
}

$ownedPackageEnumeration = Get-SourceContractRegion -Text $source `
    -Start 'bool EnumerateOwnedPackages(' `
    -End 'bool FindPublishedCandidate(' `
    -Name 'safe owned-package enumeration'
Assert-OrderedSourceFragments -Text $ownedPackageEnumeration `
    -Name 'safe owned-package enumeration' -Fragments @(
        'GetCanonicalSystemInfDirectory(&infDirectory, error)',
        'FindFirstFileW(pattern.c_str(), &data)',
        'PublishedInfEnumerationAttributesAreSafe(',
        'ValidatePublishedInfPath(',
        'LoadOwnedPackage(publishedInf, false, false'
    )

$recordlessRecovery = Get-SourceContractRegion -Text $source `
    -Start 'Outcome RecoverFailedInstallRecordless(' `
    -End 'enum class InstallJournalRecoveryModelAction' `
    -Name 'recordless failed-install recovery'
Assert-OrderedSourceFragments -Text $recordlessRecovery -Name `
    'recordless failed-install recovery' -Fragments @(
        'ValidateTransactionDeadlineBudget(',
        'IsElevated()',
        'TransactionMutex mutex;',
        'mutex.Acquire(&outcome.error)',
        'installDirectory.OpenChain(',
        'false, nullptr, &installActive',
        'removeDirectory.OpenChain(false, &removeActive',
        'if (installActive || removeActive)',
        'VerifyRecordlessRecoveryActivePathAbsent(',
        'outcome.success = true;',
        'outcome.changed = false;',
        'outcome.rebootRequired = false;',
        'outcome.rollback = L"not-needed";',
        'outcome.exitCode = ExitCode::Success;'
    )
foreach ($forbidden in @(
        'ReconcileInstallJournal(', 'ReconcileRemoveJournal(',
        'RemoveDevice(', 'DiUninstallDriverW(', 'SetupCopyOEMInfW(',
        'CreateOrOpenInstallRecoveryDirectory(', 'remove_all(',
        'MoveFileExW(', 'DeleteFileW(')) {
    if ($recordlessRecovery.Contains($forbidden)) {
        throw "Recordless failed-install recovery gained forbidden mutation '$forbidden'."
    }
}

$installJournalRequired = [ordered]@{
    'known-folder ProgramData resolution' = 'SHGetKnownFolderPath\('
    'exact ProgramData known-folder identity' = 'FOLDERID_ProgramData'
    'fixed VIIPER recovery segment' = 'kInstallRecoveryProductDirectory\[\] = L"VIIPER"'
    'fixed UdeCx recovery segment' = 'kInstallRecoveryComponentDirectory\[\] = L"UdeCx"'
    'fixed transaction recovery segment' = 'kInstallRecoveryTransactionsDirectory\[\] = L"Transactions"'
    'single active recovery identity' = 'kInstallRecoveryActiveDirectory\[\] = L"active-v2"'
    'append-only journal prefix' = 'kInstallRecoveryJournalPrefix\[\] = L"journal-"'
    'protected recovery directory open' = 'OpenStableDirectory\('
    'reparse-safe recovery directory handles' = 'FILE_FLAG_OPEN_REPARSE_POINT'
    'reparse rejection for recovery directories' = 'FILE_ATTRIBUTE_REPARSE_POINT'
    'exact recovery ACL verification' = 'VerifyProtectedFileSystemSecurity\('
    'install journal writer' = 'WriteInstallJournalRecord\('
    'journal previous-record hash' = '\\"previousSha256\\"'
    'journal envelope hash' = '\\"payloadSha256\\"'
    'journal hash-chain comparison' = 'parsed\.previousDigest[\s\S]{0,120}priorDigest'
    'write-through journal file' = 'FILE_FLAG_WRITE_THROUGH'
    'flushed journal bytes' = 'install-journal-flush'
    'write-through journal publication' = 'MOVEFILE_WRITE_THROUGH'
    'published journal readback' = 'install-journal-readback'
    'explicit recovery command' = 'Outcome Recover\('
    'automatic pre-mutation recovery' = 'ReconcileInstallJournal\('
    'recovery CLI route' = '_wcsicmp\(argv\[1\], L"recover"\)'
    'broker-unsettled manual retention' =
        'loaded\.state\.brokerEntered[\s\S]{0,120}!rollbackWasAuthorized[\s\S]{0,500}no mutation was attempted'
    'durable rollback direction' = '\"direction\"[\s\S]{0,200}\"rollbackAuthorized\"'
    'legal journal transitions' = 'ValidateInstallJournalTransition\('
    'poisoned append latch' = 'impl_->poisoned = true'
    'canonical next-temp cleanup' = 'ValidateAndDiscardInstallJournalTemporaryFile\('
    'verified existing component walk' = 'OpenExistingInstallRecoveryDirectory\('
    'generic ACL normalization' = 'MapGenericMask\('
    'durable-only stage ownership' = 'StageReceiptCaptured ownership record'
    'exact pre-rollback inventory' = 'exactPreRollbackInventory'
    'exact root rollback authority' = 'RootSnapshotIsAuthorizedForInstallRollback\('
    'same-boot rollback reboot cutpoint' = 'InstallJournalNeedsRestoreRebootPending\('
    'durable pending reboot boot epoch' = 'pendingRebootBootIdentifier'
    'fresh reboot epoch authority' = 'freshRebootRequired'
    'durable generated root receipt' = 'rootRegistrationInstanceId'
    'pre-registration root receipt phase' = 'RootRegistrationIntentCaptured'
    'broad prior-empty root observer' = 'ObservePriorEmptyInstallRecoveryRoot\('
    'canonical raw hardware ID reader' = 'ReadInstallRecoveryHardwareIds\('
    'canonical raw string reader' = 'DecodeCanonicalInstallRecoveryString\('
    'receipt-bound partial root cleanup' = 'RemoveAuthorizedPriorEmptyRootAfterAdmission\('
    'partial root removal entered receipt' = 'PartialRootRemovalEntered'
    'partial root removal returned receipt' = 'PartialRootRemovalReturned'
    'partial root removal reboot boundary' = 'PartialRootRemovalRebootPending'
    'partial root removal exact shape' = 'partialRootRemovalBinding'
    'partial root removal attempt epoch' = 'partialRootRemovalBootIdentifier'
    'product-only chain discovery model' = 'InstallRecoveryChainHasActive\('
    'target-user product ACL builder' = 'BuildInstallRecoveryProductDirectorySecurity\('
    'exact target-user product ACL verifier' = 'VerifyProtectedProductDirectorySecurity\('
    'forward reboot-pending phase' = 'ForwardRebootPending'
    'restore reboot-pending phase' = 'RestoreRebootPending'
    'manual reconciliation phase' = 'ManualReconciliationRequired'
    'authoritative mutation watchdog' = 'SynchronousMutationWatchdog'
    'authoritative mutation wrapper' = 'InvokeAuthoritativeSynchronousMutation\('
    'deadline overrun retained in journal' = 'deadlineOverrun'
}

foreach ($entry in $installJournalRequired.GetEnumerator()) {
    if ($source -notmatch $entry.Value) {
        throw "ViiperUdeCtl is missing its $($entry.Key) install-journal contract."
    }
}

$installJournalPhases = @(
    'Prepared',
    'SetupCopyEntered',
    'SetupCopyReturned',
    'StageReceiptCaptured',
    'QuiesceSignalEntered',
    'QuiesceSignalReturned',
    'RootRegistrationIntentCaptured',
    'RootRegistrationEntered',
    'RootRegistrationReturned',
    'DiInstallEntered',
    'DiInstallReturned',
    'PriorAbiProfileCaptured',
    'DriverValidated',
    'BrokerHandoffEntered',
    'BrokerHandoffReturned',
    'BrokerChildEntered',
    'BrokerChildSettled',
    'RollbackBindingEntered',
    'PartialRootRemovalEntered',
    'PartialRootRemovalReturned',
    'PartialRootRemovalRebootPending',
    'RollbackBindingReturned',
    'SetupUninstallEntered',
    'SetupUninstallReturned',
    'ForwardValidated',
    'ExactPriorRestored',
    'ForwardRebootPending',
    'RestoreRebootPending',
    'ManualReconciliationRequired'
)
foreach ($phase in $installJournalPhases) {
    $qualified = 'InstallJournalPhase::' + $phase
    if ([regex]::Matches($source, [regex]::Escape($qualified)).Count -lt 2) {
        throw "ViiperUdeCtl install journal does not both define and use phase '$phase'."
    }
}

$recoveryPathSource = Get-SourceContractRegion -Text $source `
    -Start 'bool ResolveInstallRecoveryPaths(' -End 'bool GetBootIdentifier(' `
    -Name 'fixed recovery path'
Assert-OrderedSourceFragments -Text $recoveryPathSource -Name 'fixed ProgramData path' `
    -Fragments @(
        'SHGetKnownFolderPath(',
        'FOLDERID_ProgramData',
        '*product = *programData / kInstallRecoveryProductDirectory;',
        '*component = *product / kInstallRecoveryComponentDirectory;',
        '*transactions = *component / kInstallRecoveryTransactionsDirectory;',
        '*active = *transactions / kInstallRecoveryActiveDirectory;'
    )
foreach ($fragment in @(
    'FILE_FLAG_OPEN_REPARSE_POINT',
    'FILE_ATTRIBUTE_REPARSE_POINT',
    'VerifyProtectedFileSystemSecurity(',
    'CreateOrOpenInstallRecoveryDirectory(',
    'active, false, true, &activeHandle'
)) {
    if (-not $recoveryPathSource.Contains($fragment)) {
        throw "ViiperUdeCtl fixed recovery path lost '$fragment'."
    }
}

$journalWriterSource = Get-SourceContractRegion -Text $source `
    -Start 'bool WriteInstallJournalRecord(' -End 'bool GenerateInstallTransactionId(' `
    -Name 'append-only journal writer'
Assert-OrderedSourceFragments -Text $journalWriterSource -Name 'durable journal publication' `
    -Fragments @(
        'BuildInstallJournalPayload(',
        'Sha256Data(payload, &digest',
        '\"payloadSha256\"',
        'CREATE_NEW',
        'FILE_FLAG_WRITE_THROUGH',
        'FlushFileBuffers(file.get())',
        'MoveFileExW(',
        'MOVEFILE_WRITE_THROUGH',
        'OPEN_EXISTING',
        'ReadFile(file.get(), observed.data()',
        'observed != record',
        'trailingRead != 0',
        'state->previousDigest = digest',
        '++state->sequence'
    )
if ($journalWriterSource.Contains('MOVEFILE_REPLACE_EXISTING')) {
    throw 'ViiperUdeCtl append-only journal must never replace a published record.'
}

$journalPrepareSource = Get-SourceContractRegion -Text $source `
    -Start 'bool InstallJournal::Prepare(' -End 'bool InstallJournal::Record(' `
    -Name 'install journal preparation'
Assert-OrderedSourceFragments -Text $journalPrepareSource -Name 'pre-Prepared protected evidence' `
    -Fragments @(
        'BackupPackagesIntoDirectory(',
        'CopyCandidateIntoInstallJournal(',
        'impl_->state.prior = prior;',
        'impl_->state.candidate = candidate;',
        'impl_->state.phase = InstallJournalPhase::Prepared;',
        'WriteInstallJournalRecord('
    )

$journalRecordSource = Get-SourceContractRegion -Text $source `
    -Start 'bool InstallJournal::RecordNext(' -End 'bool InstallJournal::RecordCutpoint(' `
    -Name 'atomic install journal record'
Assert-OrderedSourceFragments -Text $journalRecordSource -Name 'atomic journal state publication' `
    -Fragments @(
        'ValidateInstallJournalTransition(&impl_->state, next',
        'WriteInstallJournalRecord(',
        'impl_->state = std::move(next);',
        'PublishInstallRecoveryEvidence('
    )
if (-not $journalRecordSource.Contains('impl_->poisoned = true')) {
    throw 'ViiperUdeCtl must poison the in-process journal after an indeterminate append or publication.'
}

$watchdogSource = Get-SourceContractRegion -Text $source `
    -Start 'class SynchronousMutationWatchdog final' -End 'class DeviceInfoSet final' `
    -Name 'authoritative synchronous mutation watchdog'
foreach ($fragment in @(
    'completion_.get(), waitMilliseconds',
    'timedOut_.store(true',
    'WaitForSingleObject(completion_.get(), INFINITE)',
    'thread_.join()',
    'gLastSynchronousMutationTimedOut = watchdog.Complete()'
)) {
    if (-not $watchdogSource.Contains($fragment)) {
        throw "ViiperUdeCtl authoritative watchdog lost '$fragment'."
    }
}
foreach ($forbidden in @(
    'CancelIoEx(', 'CancelSynchronousIo(', 'TerminateThread(',
    'TerminateProcess(', '.detach()'
)) {
    if ($watchdogSource.Contains($forbidden)) {
        throw "ViiperUdeCtl authoritative watchdog contains forbidden cancellation '$forbidden'."
    }
}

$installEntrySource = Get-SourceContractRegion -Text $source `
    -Start 'Outcome Install(const InstallOptions& options)' -End 'struct PackageBackup {' `
    -Name 'install entry'
Assert-OrderedSourceFragments -Text $installEntrySource -Name 'install pre-mutation reconciliation' `
    -Fragments @(
        'mutex.Acquire(',
        'ReconcileInstallJournal(',
        'ValidateCandidateInputs(',
        'CaptureSnapshot(',
        'installJournal.Prepare('
    )
if ([regex]::Matches($installEntrySource,
        'RemoveAuthorizedPriorEmptyRootAfterAdmission\(').Count -ne 2 -or
    [regex]::Matches($installEntrySource,
        'VerifyPriorTopologyBeforePackageRollback\(').Count -ne 2 -or
    [regex]::Matches($installEntrySource,
        '!prior\.devices\.empty\(\) && bindingMutationStarted').Count -ne 2) {
    throw 'ViiperUdeCtl must apply receipt-bound root cleanup and strict post-removal proof in both in-process rollback branches without generic prior-empty deletion.'
}

$removeEntrySource = Get-SourceContractRegion -Text $source `
    -Start 'Outcome Remove(const RemoveOptions& options)' -End 'Outcome Recover(' `
    -Name 'remove entry'
Assert-OrderedSourceFragments -Text $removeEntrySource -Name 'remove pre-mutation reconciliation' `
    -Fragments @(
        'mutex.Acquire(',
        'ReconcileRemoveJournal(',
        'ReconcileInstallJournal(',
        'CaptureSnapshot(',
        'PrepareRemoveJournal(',
        'ReconcileRemoveJournal('
    )

$removePrepareSource = Get-SourceContractRegion -Text $source `
    -Start 'bool PrepareRemoveJournal(' -End 'enum class RemoveRootShape' `
    -Name 'remove journal preparation'
Assert-OrderedSourceFragments -Text $removePrepareSource `
    -Name 'remove evidence before Prepared' -Fragments @(
        'OpenChain(true',
        'PublishRemoveRecoveryEvidence(',
        'BackupPackagesIntoDirectory(',
        'ValidateRemoveJournalTransition(nullptr',
        'WriteRemoveJournalRecord('
    )

$removeRecordSource = Get-SourceContractRegion -Text $source `
    -Start 'bool AppendRemoveJournalRecord(' -End 'bool PrepareRemoveJournal(' `
    -Name 'remove append-only record' -LastStart
Assert-OrderedSourceFragments -Text $removeRecordSource `
    -Name 'remove atomic state publication' -Fragments @(
        'ValidateRemoveJournalTransition(',
        'WriteRemoveJournalRecord(',
        'loaded->state = std::move(next);',
        'PublishRemoveRecoveryEvidence('
    )
if (-not $removeRecordSource.Contains('loaded->poisoned = true')) {
    throw 'ViiperUdeCtl must poison remove recovery after an indeterminate append.'
}

$removeRetireSource = Get-SourceContractRegion -Text $source `
    -Start 'bool RetireLoadedRemoveJournal(' -End 'bool AppendRemoveJournalRecord(' `
    -Name 'loaded remove journal retirement'
Assert-OrderedSourceFragments -Text $removeRetireSource `
    -Name 'remove descendant evidence release immediately before rename' -Fragments @(
        'RemoveJournalPhase::ForwardValidated',
        'RemoveJournalPhase::ExactPriorRestored',
        'const std::string transactionId',
        'loaded->priorBackups.clear();',
        'loaded->evidenceLocks.clear();',
        'return RetireRemoveRecoveryActiveDirectory('
    )

$removeRawRetireSource = Get-SourceContractRegion -Text $source `
    -Start 'bool RetireRemoveRecoveryActiveDirectory(' `
    -End 'struct RemoveJournalStateData {' `
    -Name 'raw remove terminal retirement'
Assert-OrderedSourceFragments -Text $removeRawRetireSource `
    -Name 'remove tombstone retirement evidence' -Fragments @(
        'MoveFileExW(',
        'error->recoveryBackup = tombstone.wstring();',
        'ClearActiveRecoveryEvidence();',
        'std::filesystem::remove_all(',
        'gRetainedRemoveTombstoneError',
        'OutputDebugStringW('
    )

$removePriorRetireSource = Get-SourceContractRegion -Text $source `
    -Start 'bool RetireRemoveJournalAsPrior(' `
    -End 'bool RetireRemoveJournalAsUninstalled(' `
    -Name 'remove prior terminal retirement'
Assert-OrderedSourceFragments -Text $removePriorRetireSource `
    -Name 'remove prior terminal double validation' -Fragments @(
        'CurrentRemoveStateMatchesPrior(',
        'RemoveJournalPhase::ExactPriorRestored',
        'RecordRemoveJournalPhase(',
        'CurrentRemoveStateMatchesPrior(',
        'RetireLoadedRemoveJournal('
    )
if (-not $removePriorRetireSource.Contains(
        'if (outcome->error.recoveryBackup.empty())')) {
    throw 'Prior retirement must preserve an exact tombstone failure path instead of overwriting it with absent active-v2.'
}

$removeForwardRetireSource = Get-SourceContractRegion -Text $source `
    -Start 'bool RetireRemoveJournalAsUninstalled(' `
    -End 'bool FailRemoveJournalManual(' `
    -Name 'remove forward terminal retirement'
Assert-OrderedSourceFragments -Text $removeForwardRetireSource `
    -Name 'remove forward terminal double validation' -Fragments @(
        'CurrentRemoveStateIsUninstalled(',
        'RemoveJournalPhase::ForwardValidated',
        'RecordRemoveJournalPhase(',
        'CurrentRemoveStateIsUninstalled(',
        'RetireLoadedRemoveJournal('
    )
if (-not $removeForwardRetireSource.Contains(
        'if (outcome->error.recoveryBackup.empty())')) {
    throw 'Forward retirement must preserve an exact tombstone failure path instead of overwriting it with absent active-v2.'
}

$removeManualSource = Get-SourceContractRegion -Text $source `
    -Start 'bool FailRemoveJournalManual(' `
    -End 'bool ReturnRemoveJournalRebootPending(' `
    -Name 'remove manual evidence retention'
if (-not $removeManualSource.Contains('!cause->recoveryBackup.empty()') -or
    $removeManualSource.Contains('RetireLoadedRemoveJournal(')) {
    throw 'Manual recovery must preserve callee tombstone evidence and must never release terminal evidence locks.'
}

$removeDeviceSource = Get-SourceContractRegion -Text $source `
    -Start 'bool RemoveExactCapturedDevice(' -End 'bool RegisterRootDevice(' `
    -Name 'single captured device removal'
Assert-OrderedSourceFragments -Text $removeDeviceSource `
    -Name 'single captured device immutable revalidation' -Fragments @(
        'FindExactDevices(',
        'LoadOwnedPackage(',
        'IsExactCapturedRemoveTarget(',
        'return RemoveDevice('
    )

$removeRollbackSource = Get-SourceContractRegion -Text $source `
    -Start 'bool RunRemoveRollbackRecovery(' -End 'bool AdmitRemoveRollback(' `
    -Name 'remove rollback recovery'
Assert-OrderedSourceFragments -Text $removeRollbackSource `
    -Name 'interrupted binding admission reuse' -Fragments @(
        'ReusesInterruptedRemoveBindingAdmission(',
        '!reusingInterruptedBindingAdmission',
        'RemoveJournalPhase::RollbackBindingEntered',
        'ObserveRemoveRootShape(',
        'VerifyPackageInventory(',
        'RestorePriorBinding('
    )
Assert-OrderedSourceFragments -Text $removeRollbackSource `
    -Name 'remove rollback exact-absence binding authority' -Fragments @(
        'ObserveRemoveRootShape(',
        'root != RemoveRootShape::Absent',
        'VerifyPackageInventory(',
        'RestorePriorBinding(restorable,',
        'RestorePriorBindingPolicy::RemoveJournalExactAbsence'
    )

$restoreBindingSource = Get-SourceContractRegion -Text $source `
    -Start 'bool RestorePriorBinding(' -End 'bool RollbackInstall(' `
    -Name 'prior binding restore policy'
Assert-OrderedSourceFragments -Text $restoreBindingSource `
    -Name 'remove exact-absence race fails before mutation' -Fragments @(
        'CaptureSnapshot(',
        'RestorePriorBindingTopologyAdmitsMutation(',
        'RestorePriorBindingPolicy::InstallRollbackReconcile &&',
        'RemoveDevice(',
        'RegisterRootDeviceExact(',
        'InstallPreinstalledDriverOnDevice('
    )
if (-not $restoreBindingSource.Contains(
        'policy == RestorePriorBindingPolicy::RemoveJournalExactAbsence') -or
    -not $source.Contains(
        'self-test-remove-journal-binding-exact-absence-race')) {
    throw 'Remove rollback must reject a concurrently appeared root before any restore mutation.'
}

$removeAdmissionSource = Get-SourceContractRegion -Text $source `
    -Start 'bool AdmitRemoveRollback(' -End 'bool RunRemoveForwardRecovery(' `
    -Name 'remove rollback admission'
Assert-OrderedSourceFragments -Text $removeAdmissionSource `
    -Name 'durable rollback admission and fresh deadline' -Fragments @(
        'RemoveJournalPhase::RestoreRebootPending',
        'RecordRemoveJournalPhase(',
        'FreshRemoveRollbackDeadline();',
        'RunRemoveRollbackRecovery('
    )
if ($removeAdmissionSource.Contains('deadlineUnixMs')) {
    throw 'Forward-to-rollback admission must not accept or reuse the exhausted forward deadline.'
}

$removeForwardSource = Get-SourceContractRegion -Text $source `
    -Start 'bool RunRemoveForwardRecovery(' -End 'bool ReconcileRemoveJournal(' `
    -Name 'remove forward recovery'
Assert-OrderedSourceFragments -Text $removeForwardSource `
    -Name 'crossed reboot loop fails closed' -Fragments @(
        'CrossedRemoveRebootStillPendingRequiresManual(',
        'FailRemoveJournalManual(',
        'ReturnRemoveJournalRebootPending('
    )
$crossedRebootSource = Get-SourceContractRegion -Text $source `
    -Start 'bool CrossedRemoveRebootStillPendingRequiresManual(' `
    -End 'bool ReusesInterruptedRemoveBindingAdmission(' `
    -Name 'crossed remove reboot decision'
foreach ($fragment in @(
    'RemoveJournalPhase::DeviceRemovalReturned',
    'callSucceeded',
    'freshRebootRequired',
    '!samePendingBoot'
)) {
    if (-not $crossedRebootSource.Contains($fragment)) {
        throw "Returned-to-pending crossed reboot decision lost '$fragment'."
    }
}
if (-not $source.Contains(
        'self-test-remove-journal-device-returned-pending-cut')) {
    throw 'ViiperUdeCtl lost the compiled DeviceRemovalReturned-to-pending crash cut test.'
}
if ($removeForwardSource.Contains('RemoveAllExactDevices(') -or
    -not $removeForwardSource.Contains('RemoveExactCapturedDevice(')) {
    throw 'Protected forward removal must target one immutable captured root and never call broad all-device removal.'
}

$removeReconcileSource = Get-SourceContractRegion -Text $source `
    -Start 'bool ReconcileRemoveJournal(' -End 'struct RemoveOptions {' `
    -Name 'remove startup reconciliation' -LastStart
foreach ($fragment in @(
    'LoadRemoveJournal(',
    'InstallRecoveryDirectory installDirectory',
    'installExists',
    'ManualReconciliationRequired',
    'GetBootIdentifier(',
    'RunRemoveRollbackRecovery(',
    'RunRemoveForwardRecovery('
)) {
    if (-not $removeReconcileSource.Contains($fragment)) {
        throw "ViiperUdeCtl remove reconciliation lost '$fragment'."
    }
}

$reconcileSource = Get-SourceContractRegion -Text $source `
    -Start 'bool ReconcileInstallJournal(' -End 'const char* RemoveJournalPhaseName(' `
    -Name 'startup journal reconciliation' -LastStart
foreach ($fragment in @(
    'ForwardRebootPending && sameBoot',
    'RestoreRebootPending && sameBoot',
    'return rebootPending(',
    'loaded.state.phase == InstallJournalPhase::BrokerChildEntered',
    'loaded.state.phase == InstallJournalPhase::BrokerHandoffReturned',
    'loaded.state.rollbackAuthorized',
    'loaded.state.hasBrokerProof',
    'loaded.state.brokerProofSuccess',
    'no mutation was attempted',
    'InstallJournalPhase::ManualReconciliationRequired'
    'appendPartialRootRemovalEntered'
    'install-journal-partial-root-removal-inventory'
    'install-journal-pre-package-rollback-inventory'
    'ClassifyPartialRootRemovalJournalRecovery('
)) {
    if (-not $reconcileSource.Contains($fragment)) {
        throw "ViiperUdeCtl startup reconciliation lost '$fragment'."
    }
}

$apiPhaseContracts = @(
    @('bool StageCandidatePackage(', 'bool RemoveDevice(', 'SetupCopyEntered', 'SetupCopyOEMInfW(', 'SetupCopyReturned'),
    @('bool CommitPreparedDriverBinding(', 'bool InstallPreinstalledDriverOnDevice(', 'DiInstallEntered', 'DiInstallDevice(', 'DiInstallReturned'),
    @('bool RemoveStagedCandidateExact(', 'bool RestorePriorBinding(', 'SetupUninstallEntered', 'SetupUninstallOEMInfW(', 'SetupUninstallReturned'),
    @('bool RequestBrokerQuiescence(', 'bool SignalBrokerHandoff(', 'QuiesceSignalEntered', 'SetEvent(options.brokerQuiesceRequest)', 'QuiesceSignalReturned'),
    @('bool SignalBrokerHandoff(', 'bool ValidateTransactionDeadlineBudget(', 'BrokerHandoffEntered', 'SetEvent(options.brokerHandoff)', 'BrokerHandoffReturned'),
    @('bool RunBrokerInstall(', 'Outcome Install(', 'BrokerChildEntered', 'CreateProcessW(', 'BrokerChildSettled')
)
foreach ($contract in $apiPhaseContracts) {
    $region = Get-SourceContractRegion -Text $source -Start $contract[0] `
        -End $contract[1] -Name ("phase-wrapped API " + $contract[3])
    Assert-OrderedSourceFragments -Text $region -Name ("phase-wrapped API " + $contract[3]) `
        -Fragments @($contract[2], $contract[3], $contract[4])
}

foreach ($registrationStart in @('bool RegisterRootDevice(', 'bool RegisterRootDeviceExact(')) {
    $registrationEnd = if ($registrationStart -eq 'bool RegisterRootDevice(') {
        'bool DriverInfoUsesPublishedPackage('
    } else {
        'bool IssueAbiNegotiation('
    }
    $region = Get-SourceContractRegion -Text $source -Start $registrationStart `
        -End $registrationEnd -Name 'phase-wrapped root registration'
    $entered = $region.IndexOf('RootRegistrationEntered', [StringComparison]::Ordinal)
    $mutation = $region.IndexOf('SetupDiCallClassInstaller(', [StringComparison]::Ordinal)
    $returned = $region.LastIndexOf('RootRegistrationReturned', [StringComparison]::Ordinal)
    if ($entered -lt 0 -or $mutation -le $entered -or $returned -le $mutation) {
        throw 'ViiperUdeCtl root registration is not enclosed by durable entered/returned phases.'
    }
}

$forwardRegistrationSource = Get-SourceContractRegion -Text $source `
    -Start 'bool RegisterRootDevice(' -End 'bool DriverInfoUsesPublishedPackage(' `
    -Name 'forward root registration receipt'
Assert-OrderedSourceFragments -Text $forwardRegistrationSource `
    -Name 'generated root receipt before every registration mutation' `
    -Fragments @(
        'SetupDiCreateDeviceInfoW(',
        'DICD_GENERATE_ID',
        'SetupDiGetDeviceInstanceIdW(',
        'RecordActiveInstallJournalRootRegistrationIntent(',
        'InstallJournalPhase::RootRegistrationEntered',
        'SetupDiSetDeviceRegistryPropertyW(',
        'SetupDiCallClassInstaller(',
        'DIF_REGISTERDEVICE',
        'InstallJournalPhase::RootRegistrationReturned'
    )

$partialRootSource = Get-SourceContractRegion -Text $source `
    -Start 'bool InstallJournal::RemoveAuthorizedPriorEmptyRootAfterAdmission(' `
    -End 'bool CurrentRootIsAuthorizedForInstallRollback(' `
    -Name 'in-process receipt-bound partial root removal'
Assert-OrderedSourceFragments -Text $partialRootSource `
    -Name 'partial root removal write-ahead and authoritative return' `
    -Fragments @(
        'InstallJournalPhase::PartialRootRemovalEntered',
        'VerifyPackageInventory(',
        'observe(false, &confirmed',
        'RemoveDevice(',
        'RecordAuthoritativeReturn(',
        'InstallJournalPhase::PartialRootRemovalReturned',
        'observe(true, &after'
    )
foreach ($fragment in @(
    'RemoveUnboundExactRoot',
    'RemoveCandidateBoundExactRoot',
    'PendingExactRootRemoval',
    'freshRemovalReboot',
    'rootRemovalRebootPending'
)) {
    if (-not $partialRootSource.Contains($fragment)) {
        throw "ViiperUdeCtl partial root removal lost '$fragment'."
    }
}

$rawRootSource = Get-SourceContractRegion -Text $source `
    -Start 'bool ObservePriorEmptyInstallRecoveryRoot(' `
    -End 'bool VerifyInstallJournalRawPriorTopology(' `
    -Name 'broad raw root topology observer'
foreach ($fragment in @(
    'ReadInstallRecoveryHardwareIds(',
    'IsInGeneratedRootDeviceNamespace(',
    'hardwareIds.containsExpected',
    'related.size() == 1U',
    'loaded.state.hasRootRegistrationIntent',
    'loaded.state.rootRegistrationInstanceId.c_str()',
    'ReadCanonicalInstallRecoveryService(',
    'ReadCanonicalInstallRecoveryDevicePropertyString(',
    'CM_PROB_WILL_BE_REMOVED',
    'ClassifyPartialInstallRootRecovery('
)) {
    if (-not $rawRootSource.Contains($fragment)) {
        throw "ViiperUdeCtl broad raw root observer lost '$fragment'."
    }
}

$openChainSource = Get-SourceContractRegion -Text $source `
    -Start '    bool OpenChain(' -End '};' -Name 'install recovery directory chain'
Assert-OrderedSourceFragments -Text $openChainSource `
    -Name 'product-only recovery discovery' `
    -Fragments @(
        'bool productExists = false;',
        'bool componentExists = false;',
        'bool transactionsExist = false;',
        'bool activeExists = false;',
        'if (!productExists) return true;',
        'if (!componentExists) return true;',
        'if (!transactionsExist) return true;',
        '*exists = InstallRecoveryChainHasActive('
    )
foreach ($fragment in @(
    'BuildInstallRecoveryProductDirectorySecurity(',
    '*exactTargetUserSid',
    'CreateOrOpenInstallRecoveryDirectoryWithSecurity(',
    'VerifyProtectedProductDirectorySecurity(',
    'exactTargetUserSid'
)) {
    if (-not $openChainSource.Contains($fragment)) {
        throw "ViiperUdeCtl install recovery chain lost '$fragment'."
    }
}

$orderedMutationContracts = [ordered]@{
    'driver package staging deadline immediately precedes add-only mutation' =
        'transaction-deadline-before-driver-stage[\s\S]{0,1800}MarkTransactionMutationStarted\(\);[\s\S]{0,500}SetupCopyOEMInfW\('
    'root property deadline immediately precedes mutation' =
        'transaction-deadline-before-root-properties[\s\S]{0,900}mutationStarted[\s\S]{0,300}SetupDiSetDeviceRegistryPropertyW\('
    'root registration deadline immediately precedes mutation' =
        'transaction-deadline-before-root-registration[\s\S]{0,900}SetupDiCallClassInstaller\([\s\S]{0,120}DIF_REGISTERDEVICE'
    'selected driver deadline immediately precedes mutation' =
        'transaction-deadline-before-selected-device-binding[\s\S]{0,700}mutationStarted[\s\S]{0,400}SetupDiSetSelectedDriverW\([\s\S]{0,1800}DiInstallDevice\('
    'broker deadline immediately precedes quiescence signal' =
        'transaction-deadline-before-broker-quiescence[\s\S]{0,700}SetEvent\(options\.brokerQuiesceRequest\)'
    'remove deadline immediately precedes device mutation' =
        'CheckTransactionDeadline\(transactionDeadlineUnixMs, deadlinePhase, error\)[\s\S]{0,300}mutationStarted[\s\S]{0,180}DiUninstallDevice\('
    'first-time root creation uses the owned device name' =
        'SetupDiCreateDeviceInfoW\([\s\S]{0,120}kRootDeviceName[\s\S]{0,120}DICD_GENERATE_ID'
    'failed registration cannot suppress receipt-authorized cleanup' =
        'bool registrationSucceeded = false[\s\S]{0,30000}RemoveAuthorizedPriorEmptyRootAfterAdmission\('
    'add-only stage inventory and exact root proof precede broker quiescence' =
        'StageCandidatePackage\([\s\S]{0,5000}stage-package-inventory-verification[\s\S]{0,1600}stage-root-binding-verification[\s\S]{0,3000}RequestBrokerQuiescence\('
    'broker quiescence inventory and fresh root proof precede pristine admission' =
        'RequestBrokerQuiescence\([\s\S]{0,1200}post-quiescence-package-inventory-verification[\s\S]{0,1000}post-quiescence-root-verification[\s\S]{0,1800}AbiHealthPurpose::PristineUpgrade'
    'driver preparation and final proofs precede immediate in-place binding' =
        'PreparePreinstalledDriverOnDevice\([\s\S]{0,800}final-pre-bind-root-topology-verification[\s\S]{0,800}final-pre-bind-package-inventory-verification[\s\S]{0,900}final-pre-bind-root-verification[\s\S]{0,1000}AbiHealthPurpose::PristineRecheck[\s\S]{0,900}CommitPreparedDriverBinding\('
    'new root registration is confined to an absent captured root' =
        'if \(prior\.devices\.empty\(\)\) \{[\s\S]{0,300}RegisterRootDevice\('
    'post-stage failure reaches exact common rollback' =
        'StageCandidatePackage\([\s\S]{0,22000}if \(outcome\.error\.code != ERROR_SUCCESS && driverMutationStarted\)[\s\S]{0,3000}packageStagedHere \? &publishedCandidate : nullptr[\s\S]{0,600}RollbackInstall\('
    'binding restore precedes exact staged cleanup and inventory proof' =
        'if \(bindingMutationStarted\)[\s\S]{0,300}RestorePriorBinding\([\s\S]{0,700}RemoveStagedCandidateExact\([\s\S]{0,500}VerifyPackageInventory\('
    'formerly-running rollback requires exact start and ABI health' =
        'if \(prior\.devices\[0\]\.started\)[\s\S]{0,500}rollback-runtime-start-verification[\s\S]{0,800}AbiHealthPurpose::RollbackHealth'
    'captured-stopped rollback requires exact stopped problem state' =
        'AbiHealthPurpose::RollbackHealth[\s\S]{0,400}RollbackLifecycleStateMatches\([\s\S]{0,300}rollback-stopped-state-verification'
    'broker handoff follows exact binding verification and precedes nested commit' =
        'VerifyInstalledBinding\([\s\S]{0,12000}SignalBrokerHandoff\([\s\S]{0,800}RunBrokerInstall\('
    'remove journal and exact backups precede first mutation' =
        'Outcome Remove\([\s\S]{0,6000}PrepareRemoveJournal\([\s\S]{0,1400}ReconcileRemoveJournal\('
    'remove package admission is revalidated before exact mutation' =
        'PackageRemovalEntered[\s\S]{0,1200}ObserveRemovePackagePrefix\([\s\S]{0,1200}InvokeRemovePackageMutation\('
    'rollback package admission is revalidated before exact restoration' =
        'RollbackPackageEntered[\s\S]{0,1200}ObserveRemovePackageSubset\([\s\S]{0,1800}InvokeRestorePackageMutation\('
    'forward removal retires only after double exact validation' =
        'CurrentRemoveStateIsUninstalled\([\s\S]{0,800}ForwardValidated[\s\S]{0,800}CurrentRemoveStateIsUninstalled\([\s\S]{0,800}RetireLoadedRemoveJournal\('
    'rollback retires only after double exact prior validation' =
        'CurrentRemoveStateMatchesPrior\([\s\S]{0,800}ExactPriorRestored[\s\S]{0,800}CurrentRemoveStateMatchesPrior\([\s\S]{0,800}RetireLoadedRemoveJournal\('
    'exception outcome distinguishes preflight from mutation' =
        'const bool changed = gTransactionMutationStarted;[\s\S]{0,180}changed[\s\S]{0,100}ExitCode::RollbackFailed : ExitCode::PreflightRejected;'
}

foreach ($entry in $orderedMutationContracts.GetEnumerator()) {
    if ($source -notmatch $entry.Value) {
        throw "ViiperUdeCtl violates its $($entry.Key) ordering contract."
    }
}

$forwardInstallStart = $source.IndexOf('Outcome Install(const InstallOptions& options)')
$forwardInstallEnd = $source.IndexOf('struct PackageBackup {', $forwardInstallStart)
if ($forwardInstallStart -lt 0 -or $forwardInstallEnd -le $forwardInstallStart) {
    throw 'ViiperUdeCtl forward install transaction is missing or malformed.'
}
$forwardInstallSource = $source.Substring(
    $forwardInstallStart, $forwardInstallEnd - $forwardInstallStart)
if ($forwardInstallSource.Contains('InstallJournalPhase::DiInstallReturned')) {
    throw 'Forward install must not synthesize a DiInstallReturned phase outside the actual API wrapper.'
}
if (-not $forwardInstallSource.Contains('InstallJournalPhase::StageReceiptCaptured')) {
    throw 'Forward install must durably separate the exact stage receipt from SetupCopyOEMInfW return.'
}
if ($forwardInstallSource -match '\b(?:DiInstallDriverW|UpdateDriverForPlugAndPlayDevicesW)\s*\(') {
    throw 'Forward install must use add-only staging plus exact selected-device binding, never a device-auto-binding package API.'
}
if ($forwardInstallSource -match 'upgrade-deadline-before-device-removal|ExactRootRegistrationMode|RegisterRootDeviceExact\(') {
    throw 'Forward install must never remove and recreate an existing root before exact in-place binding.'
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

if ($source -match '\bRemoveAllExactDevices\(') {
    throw 'Protected removal must never retain broad all-device mutation plumbing.'
}

if ([regex]::Matches($source, 'VerifyDriverCatalogMember\(catalogPath').Count -ne 4) {
    throw 'Production and LocalTest validation must each bind the exact INF and SYS to the exact adjacent catalog.'
}

$forceInfUses = [regex]::Matches($source, '\bDIIRFLAG_FORCE_INF\b').Count
if ($forceInfUses -ne 0) {
    throw 'Add-only staging and exact selected-device binding must not force global INF selection.'
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

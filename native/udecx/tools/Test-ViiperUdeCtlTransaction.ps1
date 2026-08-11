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
    'loaded-kernel build identity negotiation' = 'response\.BuildIdentity'
    'exact negotiated capability identity' = 'response\.Capabilities != VIIPER_UDE_ADVERTISED_CAPABILITIES'
    'source-bound manifest identity' = 'driverBuildIdentity'
    'same-ABI stale-kernel rejection' = 'expectedBuildIdentity'
    'install rollback' = 'RollbackInstall\('
    'broker health transaction' = 'RunBrokerInstall\('
    'production broker requirement' = 'broker-required'
    'staged broker hash binding' = '--broker-sha256'
    'protected package token binding' = '--broker-token-sha256'
    'nested package broker commit' = 'native-package-broker-commit'
    'cooperative package deadline' = '--transaction-deadline-unix-ms'
    'same-handle manifest binding' = 'Sha256Handle\(manifest\.get\(\)'
    'final exact package enumeration' = 'ValidateExactPackageDirectory\('
    'reboot boundary rollback' = 'broker-reboot-boundary'
    'remove rollback backup' = 'BackupPackages\('
    'protected rollback directory' = 'kRollbackDirectorySecurity'
    'inherited rollback protection' = 'O:BAD:P\(A;OICI;FA;;;SY\)\(A;OICI;FA;;;BA\)'
    'unpredictable rollback directory' = 'CryptGenRandom\('
    'immutable rollback package files' = 'LockPackageFiles\(destination, &locks'
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
    'exact rollback devnode identity' = 'RegisterRootDeviceExact\('
    'rollback identity verification' = 'rollback-identity-verification'
    'structured reboot exit' = 'ERROR_SUCCESS_REBOOT_REQUIRED'
    'guarded downgrade' = '--allow-controlled-downgrade'
}

foreach ($entry in $requiredContracts.GetEnumerator()) {
    if ($source -notmatch $entry.Value) {
        throw "ViiperUdeCtl is missing its $($entry.Key) contract."
    }
}

if ($source -match 'SUOI_FORCEDELETE') {
    throw 'ViiperUdeCtl must never force-delete a published INF.'
}

if ($source -match 'TerminateProcess\(') {
    throw 'ViiperUdeCtl must never hard-terminate the mutating broker transaction.'
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

if ([regex]::Matches($source, '\bRemoveAllExactDevices\(').Count -ne 2) {
    throw 'All-device removal is allowed only for explicit forward uninstall, never rollback.'
}

if ([regex]::Matches($source, 'VerifyDriverCatalogMember\(catalogPath').Count -ne 2) {
    throw 'Production validation must bind both the exact INF and SYS to the exact adjacent catalog.'
}

$forceInfUses = [regex]::Matches($source, '\bDIIRFLAG_FORCE_INF\b').Count
if ($forceInfUses -ne 1 -or
    $source -notmatch 'const DWORD installFlags = downgrade \? DIIRFLAG_FORCE_INF : 0;') {
    throw 'Forced package selection must exist only behind the validated downgrade decision.'
}

$forceBindUses = [regex]::Matches($source, '\bINSTALLFLAG_FORCE\b').Count
if ($forceBindUses -ne 2) {
    throw "Expected force binding only in controlled downgrade and the shared exact-identity rollback path; found $forceBindUses uses."
}

if (-not [string]::IsNullOrWhiteSpace($BinaryPath)) {
    $resolvedBinary = Resolve-Path -LiteralPath $BinaryPath -ErrorAction Stop
    $output = & $resolvedBinary.Path self-test 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0 -or $output -notmatch 'result=success operation=self-test') {
        throw "ViiperUdeCtl deterministic self-test failed (exit $LASTEXITCODE):`n$output"
    }
}

Write-Host 'ViiperUdeCtl transaction contract is deterministic and fail-closed.'

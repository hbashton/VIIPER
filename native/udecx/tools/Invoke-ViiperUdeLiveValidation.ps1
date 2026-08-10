[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$SignedPackageDirectory,

    [Parameter(Mandatory = $true)]
    [string]$SubmissionManifestPath,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-fA-F]{40,64}$')]
    [string]$ExpectedSourceRevision,

    [ValidateSet('ControlledTest', 'Production')]
    [string]$SignatureValidationMode = 'Production',

    [ValidateRange(1, 100)]
    [int]$Iterations = 1,

    [string]$RepositoryRoot,

    [switch]$RequireDriverVerifier,

    [string]$MediaProbePath,

    [string]$InputProbePath,

    [switch]$RestartRootDevice,

    [switch]$DisposableTestMachine,

    [ValidateRange(1, 300)]
    [int]$MediaDurationSeconds = 3,

    [switch]$ReleaseGate
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Resolve-DriverImagePath {
    param([Parameter(Mandatory = $true)][string]$ImagePath)

    $path = [Environment]::ExpandEnvironmentVariables($ImagePath.Trim().Trim('"'))
    if ($path.StartsWith('\??\', [StringComparison]::Ordinal)) {
        $path = $path.Substring(4)
    }
    if ($path.StartsWith('\SystemRoot\', [StringComparison]::OrdinalIgnoreCase)) {
        $path = Join-Path $env:SystemRoot $path.Substring('\SystemRoot\'.Length)
    }
    elseif ($path.StartsWith('System32\', [StringComparison]::OrdinalIgnoreCase)) {
        $path = Join-Path $env:SystemRoot $path
    }
    if (-not [IO.Path]::IsPathRooted($path)) {
        throw "VIIPER UDE has an unsupported relative service image path: '$ImagePath'."
    }
    return (Resolve-Path -LiteralPath $path -ErrorAction Stop).Path
}

if ([string]::IsNullOrWhiteSpace($RepositoryRoot)) {
    $RepositoryRoot = Join-Path $PSScriptRoot '..\..\..'
}

if ($ReleaseGate) {
    $releaseGateFailures = [Collections.Generic.List[string]]::new()
    if ($SignatureValidationMode -ne 'Production') {
        [void]$releaseGateFailures.Add('-SignatureValidationMode must be Production')
    }
    if (-not $RequireDriverVerifier) {
        [void]$releaseGateFailures.Add('-RequireDriverVerifier is required')
    }
    if ([string]::IsNullOrWhiteSpace($MediaProbePath)) {
        [void]$releaseGateFailures.Add('-MediaProbePath is required')
    }
    if ([string]::IsNullOrWhiteSpace($InputProbePath)) {
        [void]$releaseGateFailures.Add('-InputProbePath is required')
    }
    if (-not $RestartRootDevice) {
        [void]$releaseGateFailures.Add('-RestartRootDevice is required')
    }
    if (-not $DisposableTestMachine) {
        [void]$releaseGateFailures.Add('-DisposableTestMachine is required')
    }
    if ($Iterations -lt 3) {
        [void]$releaseGateFailures.Add('-Iterations must be at least 3')
    }
    if ($MediaDurationSeconds -lt 180) {
        [void]$releaseGateFailures.Add('-MediaDurationSeconds must be at least 180')
    }
    if ($releaseGateFailures.Count -ne 0) {
        throw "Release-gate validation is incomplete:`n - $($releaseGateFailures -join "`n - ")"
    }
}

$repository = (Resolve-Path -LiteralPath $RepositoryRoot -ErrorAction Stop).Path
$signatureGate = Join-Path $PSScriptRoot 'Test-ViiperUdeSignedPackage.ps1'
& $signatureGate `
    -PackageDirectory $SignedPackageDirectory `
    -SubmissionManifestPath $SubmissionManifestPath `
    -ExpectedSourceRevision $ExpectedSourceRevision `
    -ValidationMode $SignatureValidationMode

$packageRoot = (Resolve-Path -LiteralPath $SignedPackageDirectory -ErrorAction Stop).Path
$packageDrivers = @(Get-ChildItem -LiteralPath $packageRoot -Recurse -File -Filter 'ViiperUde.sys')
if ($packageDrivers.Count -ne 1) {
    throw "Expected exactly one signed package driver; found $($packageDrivers.Count)."
}

$service = Get-ItemProperty -LiteralPath 'HKLM:\SYSTEM\CurrentControlSet\Services\ViiperUde' -ErrorAction Stop
if ([string]::IsNullOrWhiteSpace([string]$service.ImagePath)) {
    throw 'The installed VIIPER UDE service has no ImagePath.'
}
$installedDriver = Resolve-DriverImagePath -ImagePath ([string]$service.ImagePath)
$packageHash = (Get-FileHash -LiteralPath $packageDrivers[0].FullName -Algorithm SHA256).Hash
$installedHash = (Get-FileHash -LiteralPath $installedDriver -Algorithm SHA256).Hash
if ($packageHash -ne $installedHash) {
    throw "The loaded VIIPER UDE service image does not match the verified package. Installed='$installedDriver'."
}

$devnodes = @(Get-CimInstance -ClassName Win32_PnPSignedDriver | Where-Object {
    [string]$_.DeviceID -like 'ROOT\VIIPER\UDE*'
})
if ($devnodes.Count -ne 1) {
    throw "Expected exactly one VIIPER UDE root devnode; found $($devnodes.Count)."
}
if (-not [bool]$devnodes[0].IsSigned -or [string]$devnodes[0].Signer -notmatch '(?i)Microsoft') {
    throw "The installed VIIPER UDE devnode is not backed by a Microsoft-signed driver (Signer='$($devnodes[0].Signer)')."
}

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Live VIIPER UDE validation must run from an elevated PowerShell session.'
}

if ($RestartRootDevice) {
    if (-not $DisposableTestMachine) {
        throw 'Root-device restart validation is destructive to the active native session. Pass -DisposableTestMachine on a dedicated test system.'
    }
    if ([Environment]::OSVersion.Version.Build -lt 19041) {
        throw 'PnPUtil /restart-device requires Windows 10 version 2004 (build 19041) or newer.'
    }
}

if ($RequireDriverVerifier) {
    $verifierOutput = (& verifier.exe /query 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0) {
        throw "Driver Verifier query failed with exit code $LASTEXITCODE.`n$verifierOutput"
    }
    if ($verifierOutput -notmatch '(?im)\bViiperUde\.sys\b') {
        throw 'Driver Verifier is not currently active for ViiperUde.sys. Configure one-boot verification, restart, and retry.'
    }
}

$resolvedMediaProbe = $null
if (-not [string]::IsNullOrWhiteSpace($MediaProbePath)) {
    $resolvedMediaProbe = (Resolve-Path -LiteralPath $MediaProbePath -ErrorAction Stop).Path
    if ([IO.Path]::GetExtension($resolvedMediaProbe) -ine '.exe') {
        throw "The native CoreAudio probe must be an executable: '$resolvedMediaProbe'."
    }
}

$resolvedInputProbe = $null
if (-not [string]::IsNullOrWhiteSpace($InputProbePath)) {
    $resolvedInputProbe = (Resolve-Path -LiteralPath $InputProbePath -ErrorAction Stop).Path
    if ([IO.Path]::GetExtension($resolvedInputProbe) -ine '.exe') {
        throw "The native HID input/output probe must be an executable: '$resolvedInputProbe'."
    }
}

$go = Get-Command go.exe -ErrorAction Stop
$oldLive = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE', 'Process')
$oldIterations = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE_ITERATIONS', 'Process')
$oldMediaProbe = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE_MEDIA_PROBE', 'Process')
$oldMediaSeconds = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE_MEDIA_SECONDS', 'Process')
$oldInputProbe = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE_INPUT_PROBE', 'Process')
$oldRestartInstance = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE_RESTART_INSTANCE_ID', 'Process')
try {
    $env:VIIPER_UDE_LIVE = '1'
    $env:VIIPER_UDE_LIVE_ITERATIONS = [string]$Iterations
    if ($null -ne $resolvedMediaProbe) {
        $env:VIIPER_UDE_LIVE_MEDIA_PROBE = $resolvedMediaProbe
        $env:VIIPER_UDE_LIVE_MEDIA_SECONDS = [string]$MediaDurationSeconds
    }
    else {
        [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_MEDIA_PROBE', $null, 'Process')
        [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_MEDIA_SECONDS', $null, 'Process')
    }
    if ($null -ne $resolvedInputProbe) {
        $env:VIIPER_UDE_LIVE_INPUT_PROBE = $resolvedInputProbe
    }
    else {
        [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_INPUT_PROBE', $null, 'Process')
    }
    if ($RestartRootDevice) {
        $env:VIIPER_UDE_LIVE_RESTART_INSTANCE_ID = [string]$devnodes[0].DeviceID
    }
    else {
        [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_RESTART_INSTANCE_ID', $null, 'Process')
    }
    $mediaMinutes = if ($null -ne $resolvedMediaProbe) {
        [Math]::Ceiling(($MediaDurationSeconds * 2) / 60.0)
    }
    else { 0 }
    $timeoutMinutes = ($Iterations * 5) + $mediaMinutes + $(if ($RestartRootDevice) { 5 } else { 2 })
    Push-Location $repository
    try {
        & $go.Source test -count=1 -timeout "${timeoutMinutes}m" `
            -run '^TestNativeUDELive(ProductionControllers|OwnerCrashRecovery|RootRestartRecovery)$' ./internal/server/usb
        if ($LASTEXITCODE -ne 0) {
            throw "Native UDE live validation failed with exit code $LASTEXITCODE."
        }
    }
    finally {
        Pop-Location
    }
}
finally {
    [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE', $oldLive, 'Process')
    [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_ITERATIONS', $oldIterations, 'Process')
    [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_MEDIA_PROBE', $oldMediaProbe, 'Process')
    [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_MEDIA_SECONDS', $oldMediaSeconds, 'Process')
    [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_INPUT_PROBE', $oldInputProbe, 'Process')
    [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_RESTART_INSTANCE_ID', $oldRestartInstance, 'Process')
}

$verifierSuffix = if ($RequireDriverVerifier) { ' with Driver Verifier active' } else { '' }
$mediaSuffix = if ($null -ne $resolvedMediaProbe) {
    " with $MediaDurationSeconds-second full-duplex CoreAudio media per PlayStation controller"
}
else { '' }
$inputSuffix = if ($null -ne $resolvedInputProbe) { ' with end-to-end HID input latency and output feedback' } else { '' }
$restartSuffix = if ($RestartRootDevice) { ' with active root-device restart recovery' } else { '' }
$releaseSuffix = if ($ReleaseGate) { ' under the complete production release contract' } else { '' }
Write-Host "VIIPER UDE live lifecycle/HID/media validation passed for $Iterations iteration(s)$verifierSuffix$mediaSuffix$inputSuffix$restartSuffix$releaseSuffix."

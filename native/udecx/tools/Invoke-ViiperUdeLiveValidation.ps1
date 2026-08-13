[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$SignedPackageDirectory,

    [Parameter(Mandatory = $true)]
    [string]$SubmissionManifestPath,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$')]
    [string]$ExpectedSourceRevision,

    [ValidateSet('LocalTest', 'ControlledTest', 'Production')]
    [string]$SignatureValidationMode = 'Production',

    [string]$LocalTestCertificatePath,

    [ValidateRange(1, 100)]
    [int]$Iterations = 1,

    [string]$RepositoryRoot,

    [switch]$RequireDriverVerifier,

    [string]$MediaProbePath,

    [string]$InputProbePath,

    [string]$ProbeManifestPath,

    [switch]$RestartRootDevice,

    [switch]$DisposableTestMachine,

    [switch]$ManageInstalledBrokerService,

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

function Test-LiveProbeManifest {
    param(
        [Parameter(Mandatory = $true)][string]$ManifestPath,
        [Parameter(Mandatory = $true)][string]$SourceRevision,
        [Parameter(Mandatory = $true)][string]$ResolvedMediaProbe,
        [Parameter(Mandatory = $true)][string]$ResolvedInputProbe
    )

    $resolvedManifest = (Resolve-Path -LiteralPath $ManifestPath -ErrorAction Stop).Path
    try {
        $manifest = Get-Content -LiteralPath $resolvedManifest -Raw -ErrorAction Stop |
            ConvertFrom-Json -ErrorAction Stop
    }
    catch {
        throw "The native live-probe manifest is not valid JSON: '$resolvedManifest'. $($_.Exception.Message)"
    }
    if ([int]$manifest.schemaVersion -ne 1) {
        throw "The native live-probe manifest has unsupported schemaVersion '$($manifest.schemaVersion)'."
    }
    if (-not [string]::Equals([string]$manifest.sourceRevision, $SourceRevision,
            [StringComparison]::OrdinalIgnoreCase)) {
        throw "The native live-probe manifest represents source '$($manifest.sourceRevision)', not '$SourceRevision'."
    }
    if ($null -eq $manifest.probes) {
        throw 'The native live-probe manifest has no probes object.'
    }

    $expected = [ordered]@{
        'ViiperUdeMediaProbe.exe' = $ResolvedMediaProbe
        'ViiperUdeInputProbe.exe' = $ResolvedInputProbe
    }
    $properties = @($manifest.probes.PSObject.Properties)
    $actualNames = @($properties | ForEach-Object { $_.Name } | Sort-Object)
    $expectedNames = @($expected.Keys | Sort-Object)
    if ($actualNames.Count -ne $expectedNames.Count -or
        (Compare-Object -ReferenceObject $expectedNames -DifferenceObject $actualNames).Count -ne 0) {
        throw "The native live-probe manifest must contain exactly: $($expectedNames -join ', ')."
    }

    foreach ($name in $expected.Keys) {
        $path = [string]$expected[$name]
        if ([IO.Path]::GetFileName($path) -cne $name) {
            throw "The live probe path must retain its source-built name '$name': '$path'."
        }
        $expectedHash = [string]$manifest.probes.PSObject.Properties[$name].Value
        if ($expectedHash -notmatch '^[0-9a-fA-F]{64}$') {
            throw "The native live-probe manifest has an invalid SHA-256 for '$name'."
        }
        $actualHash = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash
        if (-not [string]::Equals($actualHash, $expectedHash,
                [StringComparison]::OrdinalIgnoreCase)) {
            throw "The live probe '$name' does not match the source-bound manifest."
        }
    }
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
    if ([string]::IsNullOrWhiteSpace($ProbeManifestPath)) {
        [void]$releaseGateFailures.Add('-ProbeManifestPath is required')
    }
    if (-not $RestartRootDevice) {
        [void]$releaseGateFailures.Add('-RestartRootDevice is required')
    }
    if (-not $DisposableTestMachine) {
        [void]$releaseGateFailures.Add('-DisposableTestMachine is required')
    }
    if (-not $ManageInstalledBrokerService) {
        [void]$releaseGateFailures.Add('-ManageInstalledBrokerService is required')
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

$hasMediaProbe = -not [string]::IsNullOrWhiteSpace($MediaProbePath)
$hasInputProbe = -not [string]::IsNullOrWhiteSpace($InputProbePath)
if ($SignatureValidationMode -in @('Production', 'LocalTest') -and
    ($hasMediaProbe -or $hasInputProbe) -and
    [string]::IsNullOrWhiteSpace($ProbeManifestPath)) {
    throw '-ProbeManifestPath is required whenever a source-bound live probe is used.'
}

$repository = (Resolve-Path -LiteralPath $RepositoryRoot -ErrorAction Stop).Path
if ($SignatureValidationMode -in @('Production', 'LocalTest')) {
    $git = Get-Command git.exe -ErrorAction Stop
    $headOutput = & $git.Source -C $repository rev-parse --verify HEAD 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "The source-bound live-test harness is not an exact Git checkout.`n$($headOutput -join [Environment]::NewLine)"
    }
    $headRevision = ($headOutput | Select-Object -First 1).Trim()
    if (-not [string]::Equals($headRevision, $ExpectedSourceRevision,
            [StringComparison]::OrdinalIgnoreCase)) {
        throw "The source-bound live-test harness is source '$headRevision', not '$ExpectedSourceRevision'."
    }
    $treeStatus = @(& $git.Source -C $repository status --porcelain=v1 --untracked-files=all 2>&1)
    if ($LASTEXITCODE -ne 0) {
        throw "Could not verify the source-bound live-test source tree.`n$($treeStatus -join [Environment]::NewLine)"
    }
    if ($treeStatus.Count -ne 0) {
        throw ("The source-bound live-test source tree is not clean; refusing unreviewed test code or data:`n" +
            ($treeStatus -join [Environment]::NewLine))
    }
    $submoduleStatus = @(& $git.Source -C $repository submodule status --recursive 2>&1)
    if ($LASTEXITCODE -ne 0 -or @($submoduleStatus | Where-Object { $_ -match '^[\-+U]' }).Count -ne 0) {
        throw "The source-bound live-test source tree has an unbound submodule state.`n$($submoduleStatus -join [Environment]::NewLine)"
    }
}
$signatureGate = Join-Path $PSScriptRoot 'Test-ViiperUdeSignedPackage.ps1'
& $signatureGate `
    -PackageDirectory $SignedPackageDirectory `
    -SubmissionManifestPath $SubmissionManifestPath `
    -ExpectedSourceRevision $ExpectedSourceRevision `
    -ValidationMode $SignatureValidationMode `
    -LocalTestCertificatePath $LocalTestCertificatePath

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

$ownedRootDevices = @(Get-CimInstance -ClassName Win32_PnPEntity | Where-Object {
    @($_.HardwareID) -contains 'ROOT\VIIPER\UDE'
})
if ($ownedRootDevices.Count -ne 1) {
    throw "Expected exactly one VIIPER UDE hardware-ID owner; found $($ownedRootDevices.Count)."
}
$ownedRootInstance = [string]$ownedRootDevices[0].PNPDeviceID
$devnodes = @(Get-CimInstance -ClassName Win32_PnPSignedDriver | Where-Object {
    [string]$_.DeviceID -ieq $ownedRootInstance
})
if ($devnodes.Count -ne 1) {
    throw "Expected exactly one VIIPER UDE root devnode; found $($devnodes.Count)."
}
if ([uint32]$ownedRootDevices[0].ConfigManagerErrorCode -ne 0) {
    throw "The installed VIIPER UDE root devnode has PnP problem code '$($ownedRootDevices[0].ConfigManagerErrorCode)'."
}
$infName = [string]$devnodes[0].InfName
if ($infName -cnotmatch '^oem[0-9]+\.inf$') {
    throw "The installed VIIPER UDE root devnode has an invalid OEM INF identity '$infName'."
}
$packageInfs = @(Get-ChildItem -LiteralPath $packageRoot -File -Filter 'ViiperUde.inf')
if ($packageInfs.Count -ne 1) {
    throw "Expected exactly one signed-package INF; found $($packageInfs.Count)."
}
$installedInf = Join-Path (Join-Path $env:SystemRoot 'INF') $infName
$packageInfHash = (Get-FileHash -LiteralPath $packageInfs[0].FullName -Algorithm SHA256).Hash
$installedInfHash = (Get-FileHash -LiteralPath $installedInf -Algorithm SHA256).Hash
if ($packageInfHash -cne $installedInfHash) {
    throw "The active VIIPER UDE devnode INF does not match the verified package (InfName='$infName')."
}
if ($SignatureValidationMode -ne 'LocalTest') {
    if (-not [bool]$devnodes[0].IsSigned -or
        [string]::IsNullOrWhiteSpace([string]$devnodes[0].Signer)) {
        throw "The installed VIIPER UDE devnode is not backed by a signed driver (Signer='$($devnodes[0].Signer)')."
    }
    if ([string]$devnodes[0].Signer -notmatch '(?i)Microsoft') {
        throw "The installed VIIPER UDE devnode is not backed by a Microsoft-signed driver (Signer='$($devnodes[0].Signer)')."
    }
}

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Live VIIPER UDE validation must run from an elevated PowerShell session.'
}

if ($SignatureValidationMode -eq 'LocalTest') {
    $bcdOutput = (& bcdedit.exe /enum '{current}' 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0 -or $bcdOutput -notmatch '(?im)^\s*testsigning\s+Yes\s*$') {
        throw "LocalTest requires the current boot entry to report 'testsigning Yes'. Enable TESTSIGNING and reboot before retrying.`n$bcdOutput"
    }
}

if ($ReleaseGate) {
    $operatingSystem = Get-CimInstance -ClassName Win32_OperatingSystem -ErrorAction Stop
    $build = 0
    if (-not [int]::TryParse([string]$operatingSystem.BuildNumber, [ref]$build) -or
        [uint32]$operatingSystem.ProductType -ne 1 -or $build -lt 22000 -or
        -not [Environment]::Is64BitOperatingSystem) {
        throw "The release live gate requires a 64-bit Windows 11 client HLK target; got '$($operatingSystem.Caption)' build '$($operatingSystem.BuildNumber)'."
    }
    $secureBootCommand = Get-Command Confirm-SecureBootUEFI -ErrorAction SilentlyContinue
    if ($null -eq $secureBootCommand) {
        throw 'The release live gate could not verify Secure Boot because Confirm-SecureBootUEFI is unavailable.'
    }
    try {
        $secureBootEnabled = [bool](& $secureBootCommand -ErrorAction Stop)
    }
    catch {
        throw "The release live gate could not verify Secure Boot: $($_.Exception.Message)"
    }
    if (-not $secureBootEnabled) {
        throw 'The release live gate requires Secure Boot to be enabled.'
    }
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
    $verifiedDrivers = @([regex]::Matches(
            $verifierOutput, '(?im)\b[^\s\\/:*?"<>|]+\.sys\b') |
        ForEach-Object { $_.Value } |
        Sort-Object -Unique)
    if ($verifiedDrivers.Count -ne 1 -or $verifiedDrivers[0] -ine 'ViiperUde.sys') {
        throw "Driver Verifier must target only ViiperUde.sys; active list: $($verifiedDrivers -join ', ')."
    }
    $flagsMatch = [regex]::Match($verifierOutput, '(?i)\b0x(?<flags>[0-9a-f]{8})\b')
    if (-not $flagsMatch.Success) {
        throw "Driver Verifier did not report its current flag level.`n$verifierOutput"
    }
    $verifierFlags = [Convert]::ToUInt32($flagsMatch.Groups['flags'].Value, 16)
    # /standard is 0x209BB. Supported Windows 10/11 adds 0x100000 for KMDF
    # verification. Additional stress flags are allowed, but a subset is not.
    [uint32]$requiredVerifierFlags = 0x001209BB
    if (($verifierFlags -band $requiredVerifierFlags) -ne $requiredVerifierFlags) {
        throw (("Driver Verifier is active for ViiperUde.sys with flags 0x{0:X8}, " +
            "but /standard plus KMDF requires 0x{1:X8}.") -f
            $verifierFlags, $requiredVerifierFlags)
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

if (-not [string]::IsNullOrWhiteSpace($ProbeManifestPath)) {
    if ($null -eq $resolvedMediaProbe -or $null -eq $resolvedInputProbe) {
        throw '-ProbeManifestPath requires both -MediaProbePath and -InputProbePath.'
    }
    Test-LiveProbeManifest `
        -ManifestPath $ProbeManifestPath `
        -SourceRevision $ExpectedSourceRevision `
        -ResolvedMediaProbe $resolvedMediaProbe `
        -ResolvedInputProbe $resolvedInputProbe
}

$go = Get-Command go.exe -ErrorAction Stop
$oldLive = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE', 'Process')
$oldIterations = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE_ITERATIONS', 'Process')
$oldMediaProbe = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE_MEDIA_PROBE', 'Process')
$oldMediaSeconds = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE_MEDIA_SECONDS', 'Process')
$oldInputProbe = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE_INPUT_PROBE', 'Process')
$oldRestartInstance = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE_RESTART_INSTANCE_ID', 'Process')
$oldGoFlags = [Environment]::GetEnvironmentVariable('GOFLAGS', 'Process')
$oldGoWork = [Environment]::GetEnvironmentVariable('GOWORK', 'Process')
$oldGoEnv = [Environment]::GetEnvironmentVariable('GOENV', 'Process')
$oldGoToolchain = [Environment]::GetEnvironmentVariable('GOTOOLCHAIN', 'Process')
$oldGoOS = [Environment]::GetEnvironmentVariable('GOOS', 'Process')
$oldGoArch = [Environment]::GetEnvironmentVariable('GOARCH', 'Process')
$oldCgoEnabled = [Environment]::GetEnvironmentVariable('CGO_ENABLED', 'Process')
$brokerService = Get-Service -Name 'VIIPERNativeBroker' -ErrorAction SilentlyContinue
if ($ManageInstalledBrokerService) {
    if ($null -eq $brokerService) {
        throw '-ManageInstalledBrokerService requires the installed VIIPERNativeBroker service.'
    }
    if ($brokerService.Status -ne [ServiceProcess.ServiceControllerStatus]::Running) {
        throw "The installed VIIPERNativeBroker service must be running before validation; got '$($brokerService.Status)'."
    }
}
elseif ($null -ne $brokerService -and
    $brokerService.Status -eq [ServiceProcess.ServiceControllerStatus]::Running) {
    throw 'VIIPERNativeBroker currently owns the controller. Pass -ManageInstalledBrokerService for a controlled stop/test/restart boundary.'
}
try {
    if ($ManageInstalledBrokerService) {
        Stop-Service -Name $brokerService.Name -ErrorAction Stop
        $brokerService.WaitForStatus(
            [ServiceProcess.ServiceControllerStatus]::Stopped,
            [TimeSpan]::FromSeconds(30))
    }
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
    # The live harness is part of the certification evidence. User GOFLAGS,
    # go.work redirection, GOENV defaults, or automatic toolchain downloads
    # must not select different packages, tests, or source during the gate.
    $env:GOFLAGS = '-mod=readonly'
    $env:GOWORK = 'off'
    $env:GOENV = 'off'
    $env:GOTOOLCHAIN = 'local'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    $mediaMinutes = if ($null -ne $resolvedMediaProbe) {
        [Math]::Ceiling(($MediaDurationSeconds * 3) / 60.0)
    }
    else { 0 }
    $timeoutMinutes = ($Iterations * 5) + $mediaMinutes + $(if ($RestartRootDevice) { 5 } else { 2 })
    Push-Location $repository
    try {
        $modulePath = (& $go.Source env GOMOD 2>&1 | Select-Object -First 1)
        if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace([string]$modulePath) -or
            -not [string]::Equals(
                [IO.Path]::GetFullPath([string]$modulePath),
                [IO.Path]::GetFullPath((Join-Path $repository 'go.mod')),
                [StringComparison]::OrdinalIgnoreCase)) {
            throw "The live test selected an unexpected Go module '$modulePath'."
        }
        $nativeIdentityLdflags = '-X github.com/Alia5/VIIPER/internal/transport/udecx.nativeSourceRevision=' +
            $ExpectedSourceRevision.ToLowerInvariant()
        $savedErrorActionPreference = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        try {
            $goTestOutput = @(& $go.Source test -v -count=1 -timeout "${timeoutMinutes}m" `
                -ldflags $nativeIdentityLdflags `
                -run '^TestNativeUDELive(ProductionControllers|OwnerCrashRecovery|RootRestartRecovery)$' `
                ./internal/server/usb 2>&1
            )
            $goTestExitCode = $LASTEXITCODE
        }
        finally {
            $ErrorActionPreference = $savedErrorActionPreference
        }
        $goTestOutput | ForEach-Object { Write-Host $_ }
        if ($goTestExitCode -ne 0) {
            throw "Native UDE live validation failed with exit code $goTestExitCode."
        }
        $goTestText = $goTestOutput | Out-String
        $requiredLiveTests = @(
            'TestNativeUDELiveProductionControllers',
            'TestNativeUDELiveOwnerCrashRecovery'
        )
        if ($RestartRootDevice) {
            $requiredLiveTests += 'TestNativeUDELiveRootRestartRecovery'
        }
        foreach ($testName in $requiredLiveTests) {
            if ($goTestText -notmatch "(?m)^--- PASS: $([regex]::Escape($testName)) ") {
                throw "Go reported success without executing required live test '$testName'."
            }
        }
    }
    finally {
        Pop-Location
    }
}
finally {
    if ($ManageInstalledBrokerService) {
        $brokerService.Refresh()
        if ($brokerService.Status -ne [ServiceProcess.ServiceControllerStatus]::Running) {
            Start-Service -Name $brokerService.Name -ErrorAction Stop
            $brokerService.WaitForStatus(
                [ServiceProcess.ServiceControllerStatus]::Running,
                [TimeSpan]::FromSeconds(30))
        }
    }
    [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE', $oldLive, 'Process')
    [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_ITERATIONS', $oldIterations, 'Process')
    [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_MEDIA_PROBE', $oldMediaProbe, 'Process')
    [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_MEDIA_SECONDS', $oldMediaSeconds, 'Process')
    [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_INPUT_PROBE', $oldInputProbe, 'Process')
    [Environment]::SetEnvironmentVariable('VIIPER_UDE_LIVE_RESTART_INSTANCE_ID', $oldRestartInstance, 'Process')
    [Environment]::SetEnvironmentVariable('GOFLAGS', $oldGoFlags, 'Process')
    [Environment]::SetEnvironmentVariable('GOWORK', $oldGoWork, 'Process')
    [Environment]::SetEnvironmentVariable('GOENV', $oldGoEnv, 'Process')
    [Environment]::SetEnvironmentVariable('GOTOOLCHAIN', $oldGoToolchain, 'Process')
    [Environment]::SetEnvironmentVariable('GOOS', $oldGoOS, 'Process')
    [Environment]::SetEnvironmentVariable('GOARCH', $oldGoArch, 'Process')
    [Environment]::SetEnvironmentVariable('CGO_ENABLED', $oldCgoEnabled, 'Process')
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

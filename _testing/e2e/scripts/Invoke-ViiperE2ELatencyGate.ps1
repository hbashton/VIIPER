[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$SignedPackageDirectory,

    [Parameter(Mandatory = $true)]
    [string]$SubmissionManifestPath,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-fA-F]{40,64}$')]
    [string]$ExpectedSourceRevision,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-fA-F]{64}$')]
    [string]$SDLBinarySHA256,

    [Parameter(Mandatory = $true)]
    [string]$OutputPath,

    [Parameter(Mandatory = $true)]
    [string]$WprTracePath,

    [ValidateRange(256, 10000)]
    [int]$Samples = 256,

    [string]$RepositoryRoot,

    [string]$GoExecutable = 'go.exe'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Test-IsAdministrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Resolve-CanonicalPath {
    param([Parameter(Mandatory = $true)][string]$Path)

    return (Resolve-Path -LiteralPath $Path -ErrorAction Stop).Path
}

function Resolve-NewEvidencePath {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Repository,
        [Parameter(Mandatory = $true)][string]$Label
    )

    $full = [IO.Path]::GetFullPath($Path)
    if (Test-Path -LiteralPath $full) {
        throw "$Label already exists; refusing to overwrite source-bound evidence: '$full'."
    }
    $parent = Split-Path -Parent $full
    if (-not (Test-Path -LiteralPath $parent -PathType Container)) {
        throw "$Label parent directory must already exist: '$parent'."
    }
    $repoPrefix = $Repository.TrimEnd('\') + '\'
    if ($full.StartsWith($repoPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw "$Label must be outside the source checkout so the measured tree remains clean: '$full'."
    }
    return $full
}

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
    return Resolve-CanonicalPath -Path $path
}

if ([string]::IsNullOrWhiteSpace($RepositoryRoot)) {
    $RepositoryRoot = Join-Path $PSScriptRoot '..\..\..'
}
if (-not (Test-IsAdministrator)) {
    throw 'The source-bound latency gate and WPR capture require an elevated PowerShell session.'
}
$repository = Resolve-CanonicalPath -Path $RepositoryRoot
$git = Get-Command git.exe -ErrorAction Stop
$headOutput = @(& $git.Source -C $repository rev-parse --verify HEAD 2>&1)
if ($LASTEXITCODE -ne 0 -or $headOutput.Count -eq 0) {
    throw "The production latency harness is not an exact Git checkout.`n$($headOutput -join [Environment]::NewLine)"
}
$headRevision = ([string]$headOutput[0]).Trim().ToLowerInvariant()
if (-not [string]::Equals($headRevision, $ExpectedSourceRevision,
        [StringComparison]::OrdinalIgnoreCase)) {
    throw "The production latency harness is source '$headRevision', not '$ExpectedSourceRevision'."
}
$treeStatus = @(& $git.Source -C $repository status --porcelain=v1 --untracked-files=all 2>&1)
if ($LASTEXITCODE -ne 0) {
    throw "Could not verify the production latency source tree.`n$($treeStatus -join [Environment]::NewLine)"
}
if ($treeStatus.Count -ne 0) {
    throw ("The production latency source tree is not clean; refusing unreviewed test code or data:`n" +
        ($treeStatus -join [Environment]::NewLine))
}
$submoduleStatus = @(& $git.Source -C $repository submodule status --recursive 2>&1)
if ($LASTEXITCODE -ne 0 -or @($submoduleStatus | Where-Object { $_ -match '^[\-+U]' }).Count -ne 0) {
    throw "The production latency source tree has an unbound submodule state.`n$($submoduleStatus -join [Environment]::NewLine)"
}
$sdlRoot = Resolve-CanonicalPath -Path (Join-Path $repository '_testing\e2e\deps\SDL')
$sdlRevisionOutput = @(& $git.Source -C $sdlRoot rev-parse --verify HEAD 2>&1)
if ($LASTEXITCODE -ne 0 -or $sdlRevisionOutput.Count -eq 0) {
    throw "Could not bind the SDL source revision.`n$($sdlRevisionOutput -join [Environment]::NewLine)"
}
$sdlRevision = ([string]$sdlRevisionOutput[0]).Trim().ToLowerInvariant()
$sdlDLL = Resolve-CanonicalPath -Path (Join-Path $sdlRoot 'build\Debug\SDL3.dll')
$actualSDLHash = (Get-FileHash -LiteralPath $sdlDLL -Algorithm SHA256).Hash.ToLowerInvariant()
if (-not [string]::Equals($actualSDLHash, $SDLBinarySHA256,
        [StringComparison]::OrdinalIgnoreCase)) {
    throw "The SDL binary hash is '$actualSDLHash', not the source-build hash '$SDLBinarySHA256'."
}

$signatureGate = Join-Path $repository 'native\udecx\tools\Test-ViiperUdeSignedPackage.ps1'
& $signatureGate `
    -PackageDirectory $SignedPackageDirectory `
    -SubmissionManifestPath $SubmissionManifestPath `
    -ExpectedSourceRevision $ExpectedSourceRevision `
    -ValidationMode Production

$packageRoot = Resolve-CanonicalPath -Path $SignedPackageDirectory
$packageDriver = Resolve-CanonicalPath -Path (Join-Path $packageRoot 'ViiperUde.sys')
$service = Get-ItemProperty -LiteralPath 'HKLM:\SYSTEM\CurrentControlSet\Services\ViiperUde' -ErrorAction Stop
if ([string]::IsNullOrWhiteSpace([string]$service.ImagePath)) {
    throw 'The installed VIIPER UDE service has no ImagePath.'
}
$installedDriver = Resolve-DriverImagePath -ImagePath ([string]$service.ImagePath)
$packageDriverHash = (Get-FileHash -LiteralPath $packageDriver -Algorithm SHA256).Hash.ToLowerInvariant()
$installedDriverHash = (Get-FileHash -LiteralPath $installedDriver -Algorithm SHA256).Hash.ToLowerInvariant()
if ($packageDriverHash -ne $installedDriverHash) {
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
$manifest = Resolve-CanonicalPath -Path $SubmissionManifestPath
$manifestHash = (Get-FileHash -LiteralPath $manifest -Algorithm SHA256).Hash.ToLowerInvariant()

$output = Resolve-NewEvidencePath -Path $OutputPath -Repository $repository -Label 'Latency JSON output'
$trace = Resolve-NewEvidencePath -Path $WprTracePath -Repository $repository -Label 'WPR trace output'
if ([string]::Equals($output, $trace, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'The latency JSON and WPR trace must use different evidence paths.'
}
$go = Get-Command $GoExecutable -ErrorAction Stop
$wpr = Get-Command wpr.exe -ErrorAction Stop
$wprProfile = 'GeneralProfile.Verbose'
$profileDetailsOutput = @(& $wpr.Source -profiledetails $wprProfile 2>&1)
if ($LASTEXITCODE -ne 0) {
    throw "WPR could not describe '$wprProfile'.`n$($profileDetailsOutput -join [Environment]::NewLine)"
}
$profileDetails = $profileDetailsOutput | Out-String
if ($profileDetails -notmatch '(?im)^Profile\s*:\s*GeneralProfile\.Verbose\.Memory\s*$') {
    throw "WPR '$wprProfile' is not the required bounded-memory profile.`n$profileDetails"
}
foreach ($eventName in @('DPC', 'Interrupt', 'WDFDPC', 'WDFInterrupt')) {
    if ([regex]::Matches($profileDetails, "(?im)^\s*$eventName\s*$").Count -lt 1) {
        throw "WPR '$wprProfile' does not capture the required $eventName evidence."
    }
}
foreach ($stackName in @('CSwitch', 'ReadyThread', 'SampledProfile')) {
    if ([regex]::Matches($profileDetails, "(?im)^\s*$stackName\s*$").Count -lt 2) {
        throw "WPR '$wprProfile' does not capture the required $stackName events and stacks."
    }
}

$environmentNames = @(
    'CGO_ENABLED', 'GOENV', 'GOFLAGS', 'GOTOOLCHAIN', 'GOWORK', 'PATH',
    'VIIPER_E2E_LIVE_LATENCY', 'VIIPER_E2E_PRODUCTION_PREFLIGHT',
    'VIIPER_E2E_LATENCY_OUTPUT', 'VIIPER_E2E_LATENCY_SAMPLES',
    'VIIPER_E2E_EXPECTED_SOURCE_REVISION', 'VIIPER_E2E_SDL_SOURCE_REVISION',
    'VIIPER_E2E_SDL_DLL_PATH', 'VIIPER_E2E_SDL_DLL_SHA256',
    'VIIPER_E2E_PACKAGE_MANIFEST_SHA256', 'VIIPER_E2E_NATIVE_DRIVER_SHA256'
)
$savedEnvironment = @{}
foreach ($name in $environmentNames) {
    $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}

$wprInstance = "ViiperE2ELatency-$PID-$([guid]::NewGuid().ToString('N'))"
$wprStarted = $false
$wprFailure = $null
$testExitCode = -1
try {
    $env:CGO_ENABLED = '1'
    $env:GOENV = 'off'
    $env:GOFLAGS = '-mod=readonly'
    $env:GOTOOLCHAIN = 'local'
    $env:GOWORK = 'off'
    $env:PATH = "$(Split-Path -Parent $sdlDLL);$($savedEnvironment['PATH'])"
    $env:VIIPER_E2E_LIVE_LATENCY = '1'
    $env:VIIPER_E2E_PRODUCTION_PREFLIGHT = '1'
    $env:VIIPER_E2E_LATENCY_OUTPUT = $output
    $env:VIIPER_E2E_LATENCY_SAMPLES = [string]$Samples
    $env:VIIPER_E2E_EXPECTED_SOURCE_REVISION = $headRevision
    $env:VIIPER_E2E_SDL_SOURCE_REVISION = $sdlRevision
    $env:VIIPER_E2E_SDL_DLL_PATH = $sdlDLL
    $env:VIIPER_E2E_SDL_DLL_SHA256 = $actualSDLHash
    $env:VIIPER_E2E_PACKAGE_MANIFEST_SHA256 = $manifestHash
    $env:VIIPER_E2E_NATIVE_DRIVER_SHA256 = $installedDriverHash

    $startOutput = @(& $wpr.Source -start $wprProfile -instancename $wprInstance 2>&1)
    if ($LASTEXITCODE -ne 0) {
        throw "Could not start the bounded-memory WPR capture (exit $LASTEXITCODE).`n$($startOutput -join [Environment]::NewLine)"
    }
    $wprStarted = $true

    & $go.Source test -mod=readonly -count=1 -timeout=20m `
        -run '^TestLiveControllerToGameLatencyGate$' -v ./_testing/e2e
    $testExitCode = $LASTEXITCODE
}
finally {
    if ($wprStarted) {
        $statusOutput = @(& $wpr.Source -status -instancename $wprInstance 2>&1)
        $statusExitCode = $LASTEXITCODE
        $statusText = $statusOutput | Out-String
        if ($statusExitCode -ne 0) {
            $wprFailure = "WPR status failed with exit $statusExitCode. $($statusOutput -join ' ')"
        }
        else {
            $droppedMatch = [regex]::Match($statusText, '(?im)^\s*Dropped Event\s*:\s*(?<count>\d+)\s*$')
            if (-not $droppedMatch.Success) {
                $wprFailure = "WPR did not report its dropped-event count. $($statusOutput -join ' ')"
            }
            elseif ([uint64]$droppedMatch.Groups['count'].Value -ne 0) {
                $wprFailure = "WPR dropped $($droppedMatch.Groups['count'].Value) event(s); the trace is incomplete."
            }
        }

        $stopOutput = @(& $wpr.Source -stop $trace -instancename $wprInstance 2>&1)
        $stopExitCode = $LASTEXITCODE
        if ($stopExitCode -ne 0) {
            $stopFailure = "WPR stop failed with exit $stopExitCode. $($stopOutput -join ' ')"
            if ($null -eq $wprFailure) {
                $wprFailure = $stopFailure
            }
            else {
                $wprFailure = "$wprFailure $stopFailure"
            }
        }
        elseif (-not (Test-Path -LiteralPath $trace -PathType Leaf) -or
            (Get-Item -LiteralPath $trace).Length -le 0) {
            $traceFailure = "WPR reported success without a non-empty trace '$trace'."
            if ($null -eq $wprFailure) {
                $wprFailure = $traceFailure
            }
            else {
                $wprFailure = "$wprFailure $traceFailure"
            }
        }
    }
    foreach ($name in $environmentNames) {
        [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process')
    }
}

if ($testExitCode -ne 0) {
    $wprSuffix = if ($null -eq $wprFailure) { '' } else { " WPR integrity also failed: $wprFailure" }
    throw "The live controller-to-game latency gate failed with exit code $testExitCode. Failure evidence, if emitted, is '$output'; WPR evidence is '$trace'.$wprSuffix"
}
if ($null -ne $wprFailure) {
    throw "The live workload passed, but WPR evidence failed closed: $wprFailure"
}
if (-not (Test-Path -LiteralPath $output -PathType Leaf)) {
    throw "The latency gate exited successfully without the required JSON artifact '$output'."
}
$report = Get-Content -LiteralPath $output -Raw -ErrorAction Stop | ConvertFrom-Json -ErrorAction Stop
if ([string]$report.schema -cne 'viiper.controller-to-game.latency-suite/v1' -or
    [string]$report.provenance.source_revision -cne $headRevision -or
    [string]$report.provenance.sdl_source_revision -cne $sdlRevision -or
    [string]$report.provenance.sdl_binary_sha256 -cne $actualSDLHash -or
    [string]$report.provenance.native_package_manifest_sha256 -cne $manifestHash -or
    [string]$report.provenance.native_driver_sha256 -cne $installedDriverHash -or
    [string]$report.verdict -cne 'pass' -or
    @($report.cases).Count -ne 3) {
    throw "The latency JSON artifact is not a passing source-bound production-controller suite."
}
$requiredControllers = @('xbox360', 'dualshock4', 'dualsensegamepadv5')
for ($index = 0; $index -lt $requiredControllers.Count; $index++) {
    $case = $report.cases[$index]
    if ([string]$case.workload.controller_type -cne $requiredControllers[$index] -or
        [int]$case.workload.warmup_pairs -ne 16 -or
        [int]$case.workload.sample_pairs -ne $Samples -or
        [long]$case.workload.inter_transition_delay_ns -ne 2000000 -or
        [string]$case.workload.phase_sweep_sha256 -cne '21eee9ea71984343ebd21221df8272553d6ab369a5740a1c796380cd468abcd9' -or
        @($case.runs).Count -ne 4 -or
        [string]$case.runs[0].transport -cne 'usbip' -or
        [string]$case.runs[1].transport -cne 'native-ude' -or
        [string]$case.runs[2].transport -cne 'native-ude' -or
        [string]$case.runs[3].transport -cne 'usbip' -or
        [int]$case.runs[0].order -ne 1 -or [int]$case.runs[0].transport_block -ne 1 -or
        [int]$case.runs[1].order -ne 2 -or [int]$case.runs[1].transport_block -ne 1 -or
        [int]$case.runs[2].order -ne 3 -or [int]$case.runs[2].transport_block -ne 2 -or
        [int]$case.runs[3].order -ne 4 -or [int]$case.runs[3].transport_block -ne 2 -or
        @($case.transports).Count -ne 2 -or
        [int]$case.transports[0].statistics.press.count -ne $Samples -or
        [int]$case.transports[0].statistics.release.count -ne $Samples -or
        [int]$case.transports[1].statistics.press.count -ne $Samples -or
        [int]$case.transports[1].statistics.release.count -ne $Samples) {
        throw "The latency JSON artifact is missing the counterbalanced '$($requiredControllers[$index])' workload."
    }
}
$postStatus = @(& $git.Source -C $repository status --porcelain=v1 --untracked-files=all 2>&1)
if ($LASTEXITCODE -ne 0 -or $postStatus.Count -ne 0) {
    throw ("The production latency run changed its source checkout:`n" +
        ($postStatus -join [Environment]::NewLine))
}

Write-Host "Validated source-bound controller-to-game latency evidence: '$output'."
Write-Host "Captured bounded-memory WPR evidence: '$trace'."

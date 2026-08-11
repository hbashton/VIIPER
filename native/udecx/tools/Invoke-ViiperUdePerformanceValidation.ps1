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

    [Parameter(Mandatory = $true)]
    [string]$OutputPath,

    [ValidateRange(1, 1000)]
    [int]$Iterations = 10,

    [Parameter(Mandatory = $true)]
    [string]$MediaProbePath,

    [Parameter(Mandatory = $true)]
    [string]$InputProbePath,

    [Parameter(Mandatory = $true)]
    [string]$ProbeManifestPath,

    [switch]$RequireDriverVerifier,

    [switch]$RestartRootDevice,

    [switch]$DisposableTestMachine
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Test-IsAdministrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

if (-not (Test-IsAdministrator)) {
    throw 'Native UDE performance validation requires an elevated PowerShell session.'
}

$wprPath = Join-Path $env:SystemRoot 'System32\wpr.exe'
if (-not (Test-Path -LiteralPath $wprPath -PathType Leaf)) {
    throw "Windows Performance Recorder was not found at '$wprPath'."
}

$validationPath = Join-Path $PSScriptRoot 'Invoke-ViiperUdeLiveValidation.ps1'
if (-not (Test-Path -LiteralPath $validationPath -PathType Leaf)) {
    throw "The signed live-validation script was not found at '$validationPath'."
}

$resolvedOutput = [IO.Path]::GetFullPath($OutputPath)
if (Test-Path -LiteralPath $resolvedOutput) {
    throw "Refusing to overwrite the existing trace '$resolvedOutput'."
}
$evidencePath = "$resolvedOutput.evidence.json"
if (Test-Path -LiteralPath $evidencePath) {
    throw "Refusing to overwrite the existing trace evidence '$evidencePath'."
}
$outputDirectory = Split-Path -Parent $resolvedOutput
if ([string]::IsNullOrWhiteSpace($outputDirectory)) {
    throw 'The trace output path must include a parent directory.'
}
[void][IO.Directory]::CreateDirectory($outputDirectory)

# A unique instance name is the ownership boundary. Every WPR mutation below
# carries it as the final argument, as required by WPR, so this gate can never
# stop or cancel an unrelated recording on the test machine.
$instanceName = 'ViiperUdePerf_{0}_{1}' -f $PID, [Guid]::NewGuid().ToString('N')
$profile = 'GeneralProfile.Verbose'
$started = $false
$validationFailure = $null

# GeneralProfile.Light records scheduler events, but it intentionally omits
# the CSwitch, ReadyThread, and sampled-profile stacks needed to attribute a
# tail-latency stall to the actual user/kernel critical path. Fail closed if a
# future Windows image changes the bounded-memory verbose profile contract.
$profileDetailsOutput = & $wprPath -profiledetails $profile 2>&1
if ($LASTEXITCODE -ne 0) {
    throw "WPR could not describe '$profile' (exit $LASTEXITCODE).`n$($profileDetailsOutput -join [Environment]::NewLine)"
}
$profileDetails = $profileDetailsOutput | Out-String
if ($profileDetails -notmatch '(?im)^Profile\s*:\s*GeneralProfile\.Verbose\.Memory\s*$') {
    throw "WPR '$profile' is not the required bounded-memory profile.`n$profileDetails"
}
foreach ($eventName in @('DPC', 'Interrupt', 'WDFDPC', 'WDFInterrupt')) {
    if ([regex]::Matches($profileDetails, "(?im)^\s*$eventName\s*$").Count -lt 1) {
        throw "WPR '$profile' does not capture the required $eventName evidence."
    }
}
foreach ($stackName in @('CSwitch', 'ReadyThread', 'SampledProfile')) {
    # Each required name must appear once under System Keywords and again
    # under System Stacks. A single occurrence is event-only evidence and
    # cannot explain the ready/scheduled critical path in WPA.
    if ([regex]::Matches($profileDetails, "(?im)^\s*$stackName\s*$").Count -lt 2) {
        throw "WPR '$profile' does not capture the required $stackName events and stacks."
    }
}

$validationArguments = @{
    SignedPackageDirectory = $SignedPackageDirectory
    SubmissionManifestPath = $SubmissionManifestPath
    ExpectedSourceRevision = $ExpectedSourceRevision
    SignatureValidationMode = $SignatureValidationMode
    Iterations             = $Iterations
    MediaProbePath         = $MediaProbePath
    InputProbePath         = $InputProbePath
    ProbeManifestPath      = $ProbeManifestPath
}
if ($RequireDriverVerifier) {
    $validationArguments.RequireDriverVerifier = $true
}
if ($RestartRootDevice) {
    $validationArguments.RestartRootDevice = $true
}
if ($DisposableTestMachine) {
    $validationArguments.DisposableTestMachine = $true
}

try {
    $startOutput = & $wprPath -start $profile -instancename $instanceName 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "WPR failed to start '$profile' (exit $LASTEXITCODE).`n$($startOutput -join [Environment]::NewLine)"
    }
    $started = $true

    try {
        & $validationPath @validationArguments
    }
    catch {
        $validationFailure = $_.Exception
    }
}
finally {
    if ($started) {
        $statusOutput = & $wprPath -status -instancename $instanceName 2>&1
        $statusExitCode = $LASTEXITCODE
        $statusText = $statusOutput | Out-String
        $statusFailure = $null
        if ($statusExitCode -ne 0) {
            $statusFailure = [InvalidOperationException]::new(
                "WPR status failed with exit $statusExitCode. $($statusOutput -join ' ')")
        }
        else {
            $droppedMatch = [regex]::Match($statusText, '(?im)^\s*Dropped Event\s*:\s*(?<count>\d+)\s*$')
            if (-not $droppedMatch.Success) {
                $statusFailure = [InvalidOperationException]::new(
                    "WPR did not report its dropped-event count. $($statusOutput -join ' ')")
            }
            elseif ([uint64]$droppedMatch.Groups['count'].Value -ne 0) {
                $statusFailure = [InvalidOperationException]::new(
                    "WPR dropped $($droppedMatch.Groups['count'].Value) event(s); the performance trace is incomplete.")
            }
        }
        if ($null -ne $statusFailure) {
            if ($null -eq $validationFailure) {
                $validationFailure = $statusFailure
            }
            else {
                $validationFailure = [AggregateException]::new(
                    'Native UDE validation and WPR capture integrity both failed.',
                    @($validationFailure, $statusFailure))
            }
        }

        # Stop, rather than cancel, after a workload failure. The trace is most
        # valuable when a latency or lifecycle gate failed. GeneralProfile is
        # intentionally left in its bounded verbose memory mode; file mode is
        # never enabled by this script.
        $stopOutput = & $wprPath -stop $resolvedOutput -instancename $instanceName 2>&1
        $stopExitCode = $LASTEXITCODE
        if ($stopExitCode -ne 0) {
            if ($null -ne $validationFailure) {
                throw [AggregateException]::new(
                    'Native UDE validation and WPR trace finalization both failed.',
                    @(
                        $validationFailure,
                        [InvalidOperationException]::new(
                            "WPR stop failed with exit $stopExitCode. $($stopOutput -join ' ')")
                    ))
            }
            throw "WPR failed to save '$resolvedOutput' (exit $stopExitCode).`n$($stopOutput -join [Environment]::NewLine)"
        }
    }
}

if (-not (Test-Path -LiteralPath $resolvedOutput -PathType Leaf) -or
    (Get-Item -LiteralPath $resolvedOutput).Length -eq 0) {
    throw "WPR reported success but did not create a non-empty trace at '$resolvedOutput'."
}

# An ETL has no trustworthy provenance merely because its filename resembles a
# reviewed build. Bind the exact trace, signed-package manifest, and source-built
# probes into a sidecar before reporting completion. The live validator already
# checked that the probe manifest's declared hashes and source revision match.
$evidence = [ordered]@{
    schemaVersion = 1
    sourceRevision = $ExpectedSourceRevision.ToLowerInvariant()
    profile = 'GeneralProfile.Verbose.Memory'
    trace = [ordered]@{
        name = [IO.Path]::GetFileName($resolvedOutput)
        sha256 = (Get-FileHash -LiteralPath $resolvedOutput -Algorithm SHA256).Hash.ToLowerInvariant()
    }
    signedPackageManifestSha256 = (Get-FileHash -LiteralPath $SubmissionManifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
    probeManifestSha256 = (Get-FileHash -LiteralPath $ProbeManifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
    mediaProbeSha256 = (Get-FileHash -LiteralPath $MediaProbePath -Algorithm SHA256).Hash.ToLowerInvariant()
    inputProbeSha256 = (Get-FileHash -LiteralPath $InputProbePath -Algorithm SHA256).Hash.ToLowerInvariant()
    signatureValidationMode = $SignatureValidationMode
    iterations = $Iterations
    analysisRequired = $true
}
$evidenceJson = $evidence | ConvertTo-Json -Depth 4
[IO.File]::WriteAllText($evidencePath, $evidenceJson, [Text.UTF8Encoding]::new($false))
if (-not (Test-Path -LiteralPath $evidencePath -PathType Leaf) -or
    (Get-Item -LiteralPath $evidencePath).Length -eq 0) {
    throw "The source-bound trace evidence was not written to '$evidencePath'."
}

if ($null -ne $validationFailure) {
    throw [InvalidOperationException]::new(
        "Native UDE live validation failed; the diagnostic trace was preserved at '$resolvedOutput'.",
        $validationFailure)
}

Write-Host ("Native UDE workload and trace-integrity validation passed. " +
    "Performance acceptance still requires WPA analysis of '$resolvedOutput'. " +
    "Source-bound evidence: '$evidencePath'.")

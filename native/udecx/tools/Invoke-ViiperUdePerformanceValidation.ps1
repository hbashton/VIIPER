[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$SignedPackageDirectory,

    [Parameter(Mandatory = $true)]
    [string]$OutputPath,

    [ValidateRange(1, 1000)]
    [int]$Iterations = 10,

    [string]$MediaProbePath,

    [string]$InputProbePath,

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
$outputDirectory = Split-Path -Parent $resolvedOutput
if ([string]::IsNullOrWhiteSpace($outputDirectory)) {
    throw 'The trace output path must include a parent directory.'
}
[void][IO.Directory]::CreateDirectory($outputDirectory)

# A unique instance name is the ownership boundary. Every WPR mutation below
# carries it as the final argument, as required by WPR, so this gate can never
# stop or cancel an unrelated recording on the test machine.
$instanceName = 'ViiperUdePerf_{0}_{1}' -f $PID, [Guid]::NewGuid().ToString('N')
$profile = 'GeneralProfile.Light'
$started = $false
$validationFailure = $null

$validationArguments = @{
    SignedPackageDirectory = $SignedPackageDirectory
    Iterations             = $Iterations
}
if (-not [string]::IsNullOrWhiteSpace($MediaProbePath)) {
    $validationArguments.MediaProbePath = $MediaProbePath
}
if (-not [string]::IsNullOrWhiteSpace($InputProbePath)) {
    $validationArguments.InputProbePath = $InputProbePath
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
        $validationFailure = $_
    }
}
finally {
    if ($started) {
        # Stop, rather than cancel, after a workload failure. The trace is most
        # valuable when a latency or lifecycle gate failed. GeneralProfile is
        # intentionally left in its bounded default memory mode; file mode is
        # never enabled by this script.
        $stopOutput = & $wprPath -stop $resolvedOutput -instancename $instanceName 2>&1
        $stopExitCode = $LASTEXITCODE
        if ($stopExitCode -ne 0) {
            if ($null -ne $validationFailure) {
                throw [AggregateException]::new(
                    'Native UDE validation and WPR trace finalization both failed.',
                    @(
                        $validationFailure.Exception,
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

if ($null -ne $validationFailure) {
    throw [InvalidOperationException]::new(
        "Native UDE live validation failed; the diagnostic trace was preserved at '$resolvedOutput'.",
        $validationFailure.Exception)
}

Write-Host "Native UDE performance validation passed. Trace: '$resolvedOutput'."

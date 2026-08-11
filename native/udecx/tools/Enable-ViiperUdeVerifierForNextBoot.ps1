[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$SignedPackageDirectory,

    [Parameter(Mandatory = $true)]
    [string]$SubmissionManifestPath,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$')]
    [string]$ExpectedSourceRevision,

    [ValidateSet('ControlledTest', 'Production')]
    [string]$SignatureValidationMode = 'Production',

    [switch]$DisposableTestMachine
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

if (-not $DisposableTestMachine) {
    throw 'Driver Verifier can deliberately crash Windows. Run this only on a disposable test machine and pass -DisposableTestMachine.'
}

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Driver Verifier configuration requires an elevated PowerShell session.'
}

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
    throw "The installed VIIPER UDE driver does not match the verified Microsoft-signed package. Installed='$installedDriver'."
}

$existingOutput = (& verifier.exe /querysettings 2>&1 | Out-String)
if ($LASTEXITCODE -notin @(0, 2)) {
    throw "Could not inspect existing Driver Verifier settings (exit $LASTEXITCODE).`n$existingOutput"
}
$configuredDrivers = @([regex]::Matches($existingOutput, '(?im)\b[^\s\\/:*?"<>|]+\.sys\b') |
    ForEach-Object { $_.Value } |
    Sort-Object -Unique)
$foreignDrivers = @($configuredDrivers | Where-Object { $_ -ine 'ViiperUde.sys' })
if ($foreignDrivers.Count -gt 0) {
    throw "Refusing to replace an existing Driver Verifier configuration for: $($foreignDrivers -join ', '). Reset or preserve it manually first."
}

$configured = $false
try {
    $standardOutput = (& verifier.exe /standard /driver ViiperUde.sys 2>&1 | Out-String)
    if ($LASTEXITCODE -notin @(0, 2)) {
        throw "Could not configure standard Driver Verifier checks (exit $LASTEXITCODE).`n$standardOutput"
    }
    $configured = $true

    $bootOutput = (& verifier.exe /bootmode oneboot 2>&1 | Out-String)
    if ($LASTEXITCODE -notin @(0, 2)) {
        throw "Could not constrain Driver Verifier to one boot (exit $LASTEXITCODE).`n$bootOutput"
    }

    $queryOutput = (& verifier.exe /querysettings 2>&1 | Out-String)
    if (($LASTEXITCODE -notin @(0, 2)) -or $queryOutput -notmatch '(?im)\bViiperUde\.sys\b') {
        throw "Driver Verifier did not report ViiperUde.sys in its next-boot configuration.`n$queryOutput"
    }
}
catch {
    if ($configured -and $foreignDrivers.Count -eq 0) {
        & verifier.exe /reset 2>&1 | Out-Null
    }
    throw
}

Write-Host 'Driver Verifier standard checks are staged for ViiperUde.sys for the next boot only.'
Write-Host 'Restart this disposable test machine, then run Invoke-ViiperUdeLiveValidation.ps1 with -RequireDriverVerifier.'
Write-Host 'Recovery if needed: start Windows Safe Mode, run "verifier.exe /reset" as administrator, and restart.'

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$SignedPackageDirectory,

    [ValidateRange(1, 100)]
    [int]$Iterations = 1,

    [string]$RepositoryRoot
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
$repository = (Resolve-Path -LiteralPath $RepositoryRoot -ErrorAction Stop).Path
$signatureGate = Join-Path $PSScriptRoot 'Test-ViiperUdeSignedPackage.ps1'
& $signatureGate -PackageDirectory $SignedPackageDirectory

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

$go = Get-Command go.exe -ErrorAction Stop
$oldLive = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE', 'Process')
$oldIterations = [Environment]::GetEnvironmentVariable('VIIPER_UDE_LIVE_ITERATIONS', 'Process')
try {
    $env:VIIPER_UDE_LIVE = '1'
    $env:VIIPER_UDE_LIVE_ITERATIONS = [string]$Iterations
    $timeoutMinutes = ($Iterations * 5) + 2
    Push-Location $repository
    try {
        & $go.Source test -count=1 -timeout "${timeoutMinutes}m" `
            -run '^TestNativeUDELiveProductionControllers$' ./internal/server/usb
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
}

Write-Host "VIIPER UDE live lifecycle/input validation passed for $Iterations iteration(s)."

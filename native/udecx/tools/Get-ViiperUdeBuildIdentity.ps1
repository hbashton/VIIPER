[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$')]
    [string]$SourceRevision,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^\d+\.\d+\.\d+\.\d+$')]
    [string]$DriverPackageVersion,

    [Parameter(Mandatory = $true)]
    [ValidateRange(1, 65535)]
    [int]$ABIMajor,

    [Parameter(Mandatory = $true)]
    [ValidateRange(0, 65535)]
    [int]$ABIMinor,

    [Parameter(Mandatory = $true)]
    [ValidateRange(1, [uint32]::MaxValue)]
    [uint32]$Capabilities,

    [string]$OutputHeaderPath
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# This exact ASCII/UTF-8 preimage is a cross-language protocol. Keep it in
# lockstep with udecx.DeriveBuildIdentity and ViiperUdeCtl's derivation.
$preimage = "VIIPER-UDE-BUILD-IDENTITY/v1`n" +
    "sourceRevision=$($SourceRevision.ToLowerInvariant())`n" +
    "driverPackageVersion=$DriverPackageVersion`n" +
    "abi=$ABIMajor.$ABIMinor`n" +
    ("capabilities=0x{0:x8}`n" -f $Capabilities)
$bytes = [Text.UTF8Encoding]::new($false).GetBytes($preimage)
$sha256 = [Security.Cryptography.SHA256]::Create()
try {
    $digest = $sha256.ComputeHash($bytes)
}
finally {
    $sha256.Dispose()
}
$hex = ([BitConverter]::ToString($digest)).Replace('-', '').ToLowerInvariant()

if (-not [string]::IsNullOrWhiteSpace($OutputHeaderPath)) {
    $fullPath = [IO.Path]::GetFullPath($OutputHeaderPath)
    $directory = [IO.Path]::GetDirectoryName($fullPath)
    if ([string]::IsNullOrWhiteSpace($directory)) {
        throw 'OutputHeaderPath must include a directory.'
    }
    [IO.Directory]::CreateDirectory($directory) | Out-Null
    $initializer = ($digest | ForEach-Object { '0x{0:x2}' -f $_ }) -join ', '
    $header = @"
#pragma once

/* Generated from an explicit source/package/ABI/capability tuple. */
static const VIIPER_UDE_UINT8 ViiperUdeBuildIdentity[VIIPER_UDE_BUILD_IDENTITY_BYTES] = {
    $initializer
};
"@
    [IO.File]::WriteAllText($fullPath, $header, [Text.UTF8Encoding]::new($false))
}

$hex

[CmdletBinding()]
param(
    [string]$Destination,

    [ValidateRange(0, 100)]
    [int]$MaxMiniDumps = 5,

    [ValidatePattern('^S-1-(?:\d+-){1,14}\d+$')]
    [string]$GrantReadToSID,

    [switch]$Force
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Crash-dump collection requires an elevated PowerShell session.'
}

if ([string]::IsNullOrWhiteSpace($Destination)) {
    $Destination = Join-Path $env:ProgramData 'Viiper\crash-dumps'
}
$destinationPath = [IO.Path]::GetFullPath($Destination)
$windowsPath = [IO.Path]::GetFullPath($env:SystemRoot).TrimEnd('\')
if ($destinationPath.TrimEnd('\') -eq $windowsPath -or
        $destinationPath.StartsWith("$windowsPath\System32",
            [StringComparison]::OrdinalIgnoreCase)) {
    throw "Refusing unsafe crash-dump destination '$destinationPath'."
}

$sources = @()
$memoryDump = Join-Path $env:SystemRoot 'MEMORY.DMP'
if (Test-Path -LiteralPath $memoryDump -PathType Leaf) {
    $sources += Get-Item -LiteralPath $memoryDump
}
if ($MaxMiniDumps -gt 0) {
    $miniDumpDirectory = Join-Path $env:SystemRoot 'Minidump'
    if (Test-Path -LiteralPath $miniDumpDirectory -PathType Container) {
        $sources += @(Get-ChildItem -LiteralPath $miniDumpDirectory -File -Filter '*.dmp' |
            Sort-Object LastWriteTimeUtc -Descending |
            Select-Object -First $MaxMiniDumps)
    }
}
if ($sources.Count -eq 0) {
    throw 'Windows has no MEMORY.DMP or minidump files to collect.'
}

$requiredBytes = [uint64](($sources | Measure-Object Length -Sum).Sum)
$destinationRoot = [IO.Path]::GetPathRoot($destinationPath)
$drive = Get-CimInstance Win32_LogicalDisk -Filter `
    "DeviceID='$($destinationRoot.TrimEnd('\'))'" -ErrorAction Stop
$safetyBytes = 2GB
if ([uint64]$drive.FreeSpace -lt ($requiredBytes + $safetyBytes)) {
    throw "Destination volume needs $([Math]::Ceiling(($requiredBytes + $safetyBytes) / 1GB)) GB free to preserve dumps with safety headroom."
}

New-Item -ItemType Directory -Path $destinationPath -Force | Out-Null
$manifestFiles = @()
foreach ($source in $sources) {
    $destinationFile = Join-Path $destinationPath $source.Name
    if (Test-Path -LiteralPath $destinationFile -PathType Leaf) {
        $sourceHash = (Get-FileHash -LiteralPath $source.FullName -Algorithm SHA256).Hash
        $destinationHash = (Get-FileHash -LiteralPath $destinationFile -Algorithm SHA256).Hash
        if ($sourceHash -eq $destinationHash) {
            $copied = Get-Item -LiteralPath $destinationFile
        }
        elseif (-not $Force) {
            throw "Destination '$destinationFile' exists with different content; pass -Force to replace it."
        }
        else {
            Copy-Item -LiteralPath $source.FullName -Destination $destinationFile -Force
            $copied = Get-Item -LiteralPath $destinationFile
        }
    }
    else {
        Copy-Item -LiteralPath $source.FullName -Destination $destinationFile
        $copied = Get-Item -LiteralPath $destinationFile
    }
    $manifestFiles += [ordered]@{
        name = $copied.Name
        source = $source.FullName
        length = [uint64]$copied.Length
        lastWriteUtc = $copied.LastWriteTimeUtc.ToString('o')
        sha256 = (Get-FileHash -LiteralPath $copied.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
    }
}

if (-not [string]::IsNullOrWhiteSpace($GrantReadToSID)) {
    $aclOutput = (& icacls.exe $destinationPath /grant "*$GrantReadToSID`:(OI)(CI)RX" /T /C 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0) {
        throw "Could not grant dump-directory read access to '$GrantReadToSID'.`n$aclOutput"
    }
}

$manifest = [ordered]@{
    schema = 1
    machine = $env:COMPUTERNAME
    collectedUtc = [DateTime]::UtcNow.ToString('o')
    files = $manifestFiles
}
$manifestPath = Join-Path $destinationPath 'crash-dumps.json'
[IO.File]::WriteAllText($manifestPath, ($manifest | ConvertTo-Json -Depth 6),
    [Text.UTF8Encoding]::new($false))
Write-Host "Collected $($manifestFiles.Count) crash dump(s) in '$destinationPath'."
Write-Host "Manifest: $manifestPath"

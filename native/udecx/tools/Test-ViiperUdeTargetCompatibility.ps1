[CmdletBinding()]
param(
    [string]$ProjectPath,
    [string]$InfPath
)

$ErrorActionPreference = 'Stop'
if ([string]::IsNullOrWhiteSpace($ProjectPath)) {
    $ProjectPath = Join-Path $PSScriptRoot '..\driver\ViiperUde.vcxproj'
}
if ([string]::IsNullOrWhiteSpace($InfPath)) {
    $InfPath = Join-Path $PSScriptRoot '..\package\ViiperUde.inf'
}
$projectPathResolved = (Resolve-Path -LiteralPath $ProjectPath).Path
$infPathResolved = (Resolve-Path -LiteralPath $InfPath).Path

[xml]$project = Get-Content -LiteralPath $projectPathResolved -Raw
$namespace = New-Object System.Xml.XmlNamespaceManager($project.NameTable)
$namespace.AddNamespace('msb', 'http://schemas.microsoft.com/developer/msbuild/2003')
$minorNodes = @($project.SelectNodes('//msb:KMDF_VERSION_MINOR', $namespace))
if ($minorNodes.Count -ne 2) {
    throw "Expected Debug and Release KMDF_VERSION_MINOR nodes; found $($minorNodes.Count)."
}
$minorVersions = @($minorNodes | ForEach-Object { $_.InnerText.Trim() } | Sort-Object -Unique)
if ($minorVersions.Count -ne 1 -or $minorVersions[0] -ne '27') {
    throw "Windows 10 1809 requires the committed KMDF 1.27 contract; project targets: $($minorVersions -join ', ')."
}

$inf = Get-Content -LiteralPath $infPathResolved -Raw
if ($inf -notmatch '(?mi)^\[Standard\.NTamd64\.10\.0\.\.\.17763\]\s*$') {
    throw 'The INF no longer declares the reviewed Windows 10 1809 (build 17763) target floor.'
}
if ($inf -notmatch '(?mi)^DriverVer=\d{2}/\d{2}/\d{4},\d+\.\d+\.\d+\.\d+\s*$') {
    throw 'The INF is missing a valid DriverVer entry.'
}

Write-Host 'VIIPER UDE target contract is aligned: Windows 10 1809, KMDF 1.27.'

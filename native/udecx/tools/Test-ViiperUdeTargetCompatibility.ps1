[CmdletBinding()]
param(
    [string]$ProjectPath,
    [string]$InfPath,
    [switch]$RequireStampedInf
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

$majorNodes = @($project.SelectNodes('//msb:KMDF_VERSION_MAJOR', $namespace))
if ($majorNodes.Count -ne 2) {
    throw "Expected Debug and Release KMDF_VERSION_MAJOR nodes; found $($majorNodes.Count)."
}
$majorVersions = @($majorNodes | ForEach-Object { $_.InnerText.Trim() } | Sort-Object -Unique)
if ($majorVersions.Count -ne 1 -or $majorVersions[0] -ne '1') {
    throw "The committed driver must target KMDF major version 1; project targets: $($majorVersions -join ', ')."
}

function Get-SingleProjectValue([string]$elementName) {
    $nodes = @($project.SelectNodes("//msb:$elementName", $namespace))
    if ($nodes.Count -ne 1 -or [string]::IsNullOrWhiteSpace($nodes[0].InnerText)) {
        throw "Expected exactly one non-empty $elementName project value; found $($nodes.Count)."
    }
    return $nodes[0].InnerText.Trim()
}

$driverDate = Get-SingleProjectValue 'ViiperUdeDriverDate'
$driverVersion = Get-SingleProjectValue 'ViiperUdeDriverVersion'
$parsedDriverDate = [DateTime]::MinValue
if (-not [DateTime]::TryParseExact($driverDate, 'MM/dd/yyyy',
        [Globalization.CultureInfo]::InvariantCulture,
        [Globalization.DateTimeStyles]::None, [ref]$parsedDriverDate)) {
    throw "ViiperUdeDriverDate must use deterministic MM/dd/yyyy format; found '$driverDate'."
}
if ($driverVersion -notmatch '^\d+\.\d+\.\d+\.\d+$') {
    throw "ViiperUdeDriverVersion must be a four-part numeric version; found '$driverVersion'."
}

$infItems = @($project.SelectNodes('//msb:Inf', $namespace))
if ($infItems.Count -ne 1) {
    throw "Expected exactly one INF project item; found $($infItems.Count)."
}
$infItem = $infItems[0]
$stampContract = [ordered]@{
    'SpecifyDriverVerDirectiveDate' = 'true'
    'DateStamp' = '$(ViiperUdeDriverDate)'
    'SpecifyDriverVerDirectiveVersion' = 'true'
    'TimeStamp' = '$(ViiperUdeDriverVersion)'
}
foreach ($entry in $stampContract.GetEnumerator()) {
    $node = $infItem.SelectSingleNode("msb:$($entry.Key)", $namespace)
    if ($null -eq $node -or $node.InnerText.Trim() -cne $entry.Value) {
        throw "INF build metadata '$($entry.Key)' must be '$($entry.Value)' so StampInf cannot synthesize a date or version."
    }
}

$inf = Get-Content -LiteralPath $infPathResolved -Raw
if ($inf -notmatch '(?mi)^\[Standard\.NTamd64\.10\.0\.\.\.17763\]\s*$') {
    throw 'The INF no longer declares the reviewed Windows 10 1809 (build 17763) target floor.'
}
$driverVerPattern = '(?mi)^DriverVer\s*=\s*' +
    [regex]::Escape($driverDate) + '\s*,\s*' +
    [regex]::Escape($driverVersion) + '\s*$'
if ($inf -notmatch $driverVerPattern) {
    throw "The INF DriverVer must exactly match the deterministic project contract '$driverDate,$driverVersion'."
}
if ($inf -notmatch '(?mi)^\[ViiperUde_Install\.NT\.Wdf\]\s*$' -or
        $inf -notmatch '(?mi)^KmdfService\s*=\s*ViiperUde\s*,\s*ViiperUde_Wdf\s*$' -or
        $inf -notmatch '(?mi)^\[ViiperUde_Wdf\]\s*$') {
    throw 'The INF must bind the ViiperUde service through ViiperUde_Install.NT.Wdf.'
}
$expectedKmdfLibraryVersion = if ($RequireStampedInf) { '1.27' } else { '$KMDFVERSION$' }
$kmdfLibraryPattern = '(?mi)^KmdfLibraryVersion\s*=\s*' +
    [regex]::Escape($expectedKmdfLibraryVersion) + '\s*$'
if ($inf -notmatch $kmdfLibraryPattern) {
    throw "The INF KmdfLibraryVersion must be '$expectedKmdfLibraryVersion'."
}

$stampState = if ($RequireStampedInf) { 'stamped output' } else { 'source template' }
Write-Host "VIIPER UDE target contract is aligned: Windows 10 1809, KMDF 1.27, deterministic DriverVer ($stampState)."

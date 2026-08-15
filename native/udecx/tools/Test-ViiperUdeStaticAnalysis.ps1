[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$AnalysisDirectory,

    [ValidateRange(1, 1024)]
    [int]$ExpectedSourceCount = 6
)

$ErrorActionPreference = 'Stop'
$analysisRoot = (Resolve-Path -LiteralPath $AnalysisDirectory).Path
$results = @(Get-ChildItem -LiteralPath $analysisRoot -File -Filter '*.nativecodeanalysis.xml')
if ($results.Count -ne $ExpectedSourceCount) {
    throw "Expected $ExpectedSourceCount native code-analysis result files in '$analysisRoot'; found $($results.Count)."
}

$defects = [Collections.Generic.List[object]]::new()
foreach ($result in $results) {
    $settings = [Xml.XmlReaderSettings]::new()
    $settings.DtdProcessing = [Xml.DtdProcessing]::Prohibit
    $settings.XmlResolver = $null
    $reader = [Xml.XmlReader]::Create($result.FullName, $settings)
    try {
        $document = [Xml.XmlDocument]::new()
        $document.XmlResolver = $null
        $document.Load($reader)
    } finally {
        $reader.Dispose()
    }

    foreach ($defect in @($document.SelectNodes('/DEFECTS/*'))) {
        $defects.Add([pscustomobject]@{
                Source = $result.Name
                Detail = $defect.OuterXml
            })
    }
}

if ($defects.Count -ne 0) {
    $details = ($defects | ForEach-Object { "$($_.Source): $($_.Detail)" }) -join [Environment]::NewLine
    throw "Native driver static analysis reported $($defects.Count) defect(s):$([Environment]::NewLine)$details"
}

Write-Host "VIIPER UDE native static analysis passed for $($results.Count) translation units."

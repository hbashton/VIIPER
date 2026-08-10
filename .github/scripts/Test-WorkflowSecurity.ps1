[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$workflowDirectory = Join-Path $repositoryRoot '.github\workflows'
$workflowFiles = @(Get-ChildItem -LiteralPath $workflowDirectory -File -Filter '*.yml')

foreach ($workflow in $workflowFiles) {
    $source = Get-Content -LiteralPath $workflow.FullName -Raw
    foreach ($match in [regex]::Matches($source, '(?m)^\s*uses:\s*(?<reference>[^\s#]+)')) {
        $reference = $match.Groups['reference'].Value
        if ($reference.StartsWith('./', [StringComparison]::Ordinal)) {
            continue
        }
        if ($reference -notmatch '^[^@\s]+@[0-9a-f]{40}$') {
            throw "$($workflow.Name) uses mutable or malformed action reference '$reference'. External actions must use a full commit SHA."
        }
    }
    if ($source -match '(?m)^\s*go-version\s*:') {
        throw "$($workflow.Name) selects a floating Go toolchain. Use the exact version declared by go.mod."
    }
}

$releaseSource = Get-Content -LiteralPath (Join-Path $workflowDirectory 'release.yml') -Raw
foreach ($required in @(
        'native-validation',
        'native-package-transaction',
        'Test-WorkflowSecurity.ps1',
        'actions/attest-build-provenance@977bb373ede98d70efdf65b84cb5f73e068dcc2a')) {
    if (-not $releaseSource.Contains($required)) {
        throw "The release workflow is missing required gate '$required'."
    }
}
if ($releaseSource -notmatch '(?ms)^\s{4}create-release:\s.*?^\s{8}needs:\s*\[[^\]]*native-validation[^\]]*native-package-transaction[^\]]*\]') {
    throw 'create-release must depend on both native validation and package-transaction gates.'
}
if ($releaseSource -notmatch '(?ms)^\s{4}release-policy:\s.*?current origin/main tip') {
    throw 'Release tags must be constrained to the workflow-protected current main tip.'
}
if ($releaseSource.Contains('ViiperUde-x64-test-signed')) {
    throw 'The production release workflow must never consume the native test-signed artifact.'
}
if ([regex]::Matches($releaseSource, 'pattern:\s*"\*-Release"').Count -ne 2) {
    throw 'Release artifact downloads must use the explicit *-Release artifact allowlist.'
}

$nativeWorkflow = Get-Content -LiteralPath (Join-Path $workflowDirectory 'native-ude.yml') -Raw
if ($nativeWorkflow -notmatch '(?m)^\s*if:\s*\$\{\{\s*inputs\.upload_artifacts\s*==\s*true\s*\}\}\s*$') {
    throw 'Native test-signed artifacts may upload only through the explicit Boolean test-artifact input.'
}

$productionWorkflow = Get-Content -LiteralPath (Join-Path $workflowDirectory 'native-production-package.yml') -Raw
foreach ($required in @(
        'Test-ViiperUdeSignedPackage.ps1',
        '-ValidationMode Production',
        'Microsoft-signed',
        'signingRoute')) {
    if (-not $productionWorkflow.Contains($required)) {
        throw "The production-native workflow is missing required validation contract '$required'."
    }
}
if ($productionWorkflow -match '(?m)^\s{2}(?:push|pull_request):') {
    throw 'Production Microsoft-signed package acceptance must remain an explicit manual intake path.'
}

$goDirective = Get-Content -LiteralPath (Join-Path $repositoryRoot 'go.mod') -TotalCount 3 |
    Where-Object { $_ -match '^go\s+' } |
    Select-Object -First 1
if ($goDirective -notmatch '^go\s+\d+\.\d+\.\d+$') {
    throw "go.mod must pin a complete Go toolchain version; found '$goDirective'."
}

$packagesPath = Join-Path $repositoryRoot 'native\udecx\driver\packages.config'
[xml]$packages = Get-Content -LiteralPath $packagesPath -Raw
$expectedWdkVersion = '10.0.28000.1839'
$expectedPackages = @(
    'Microsoft.Windows.SDK.CPP',
    'Microsoft.Windows.SDK.CPP.x64',
    'Microsoft.Windows.WDK.x64'
)
foreach ($packageId in $expectedPackages) {
    $matches = @($packages.packages.package | Where-Object { $_.id -ceq $packageId })
    if ($matches.Count -ne 1 -or $matches[0].version -cne $expectedWdkVersion) {
        throw "Native package '$packageId' must be pinned exactly to $expectedWdkVersion."
    }
}

$projectSource = Get-Content -LiteralPath (Join-Path $repositoryRoot 'native\udecx\driver\ViiperUde.vcxproj') -Raw
foreach ($packageId in $expectedPackages) {
    $escapedPath = [regex]::Escape("$packageId.$expectedWdkVersion")
    if ($projectSource -notmatch $escapedPath) {
        throw "The native project does not import exact package '$packageId.$expectedWdkVersion'."
    }
}

Write-Host 'Workflow action pins, release gates, provenance, and native toolchain contracts are deterministic.'

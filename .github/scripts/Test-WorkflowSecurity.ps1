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
    if ($source -match '(?m)^\s*runs-on:\s*(?:ubuntu|windows|macos)-latest\s*$') {
        throw "$($workflow.Name) selects a floating hosted-runner generation. Pin the OS generation."
    }
    if ($source -match '(?mi)vswhere\.exe[^\r\n]*\s-latest(?:\s|$)') {
        throw "$($workflow.Name) selects a floating Visual Studio toolchain with vswhere -latest."
    }
    foreach ($pattern in @(
            '(?mi)^\s*(?:python|node|dotnet|cmake|nuget|just)-version:\s*["'']?(?:latest|stable|\d+(?:\.\d+)*\.x)["'']?\s*$',
            '(?mi)^\s*toolchain:\s*["'']?(?:stable|beta|nightly)["'']?\s*$')) {
        if ($source -match $pattern) {
            throw "$($workflow.Name) selects a floating release toolchain: '$($Matches[0].Trim())'."
        }
    }
    $justSetups = [regex]::Matches($source, 'extractions/setup-just@[0-9a-f]{40}').Count
    $justPins = [regex]::Matches($source, '(?m)^\s*just-version:\s*"1\.58\.0"\s*$').Count
    if ($justSetups -ne $justPins) {
        throw "$($workflow.Name) must pin just 1.58.0 for every setup-just action."
    }
    $msbuildSetups = [regex]::Matches($source, 'microsoft/setup-msbuild@[0-9a-f]{40}').Count
    $msbuildPins = [regex]::Matches($source, '(?m)^\s*vs-version:\s*"\[18\.0,19\.0\)"\s*$').Count
    if ($msbuildSetups -ne $msbuildPins) {
        throw "$($workflow.Name) must constrain every MSBuild setup to the Visual Studio 2026 generation."
    }
}

$releaseSource = Get-Content -LiteralPath (Join-Path $workflowDirectory 'release.yml') -Raw
foreach ($required in @(
        'native-validation',
        'native-package-transaction',
        'native-production-provenance',
        'native-user-mode-signing',
        'Protect-ViiperWindowsReleaseBinaries.ps1',
        'Test-ViiperUdeReleaseBundle.ps1',
        '-RequireAuthenticode',
        'viiper-native-udecx-windows-amd64.zip',
        'Test-WorkflowSecurity.ps1',
        'actions/attest-build-provenance@977bb373ede98d70efdf65b84cb5f73e068dcc2a')) {
    if (-not $releaseSource.Contains($required)) {
        throw "The release workflow is missing required gate '$required'."
    }
}
if ($releaseSource -notmatch '(?ms)^\s{4}create-release:\s.*?^\s{8}needs:\s*\[[^\]]*native-validation[^\]]*native-package-transaction[^\]]*\]') {
    throw 'create-release must depend on both native validation and package-transaction gates.'
}
if ($releaseSource -notmatch '(?ms)^\s{4}create-release:\s.*?^\s{8}needs:\s*\[[^\]]*native-production-provenance[^\]]*\]') {
    throw 'create-release must depend on an accepted Microsoft production-package artifact.'
}
if ($releaseSource -notmatch '(?ms)^\s{4}create-release:\s.*?^\s{8}needs:\s*\[[^\]]*native-user-mode-signing[^\]]*\]') {
    throw 'create-release must depend on the fail-closed broker/helper Authenticode signing gate.'
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
foreach ($requiredProductionBinding in @(
        '.github/workflows/native-production-package.yml',
        '.head_branch == "main"',
        '.head_sha == $sha',
        'artifact-ids: ${{ needs.native-production-provenance.outputs.artifact_id }}',
        'ViiperUdeCtl-windows-amd64-${{ github.sha }}')) {
    if (-not $releaseSource.Contains($requiredProductionBinding)) {
        throw "The release workflow is missing production provenance binding '$requiredProductionBinding'."
    }
}
if ($releaseSource -notmatch "(?ms)\`$expectedProduction\s*=\s*@\(\s*'submission-manifest\.json',\s*'ViiperUde/ViiperUde\.cat',\s*'ViiperUde/ViiperUde\.inf',\s*'ViiperUde/ViiperUde\.pdb',\s*'ViiperUde/ViiperUde\.sys'\)") {
    throw 'Release composition must allowlist the exact validated Microsoft-returned package.'
}
if ($releaseSource -notmatch '(?ms)expected_runtime=\(\s*ViiperUde\.cat\s*ViiperUde\.inf\s*ViiperUde\.sys\s*ViiperUdeCtl\.exe\s*submission-manifest\.json\s*viiper\.exe\s*\)') {
    throw 'The public native runtime archive must contain exactly broker, helper, INF, SYS, CAT, and manifest.'
}

$signingJob = [regex]::Match(
    $releaseSource,
    '(?ms)^\s{4}native-user-mode-signing:\s.*?(?=^\s{4}build:)').Value
if ([string]::IsNullOrWhiteSpace($signingJob)) {
    throw 'The release workflow is missing the mandatory native user-mode signing job.'
}
foreach ($requiredSigningGate in @(
        'WINDOWS_SIGNING_PFX_BASE64',
        'WINDOWS_SIGNING_PFX_PASSWORD',
        'WINDOWS_SIGNING_CERTIFICATE_SHA256',
        'Protect-ViiperWindowsReleaseBinaries.ps1',
        'ViiperUdeCtl.exe verify',
        'VIIPER-windows-amd64-authenticode-${{ github.sha }}',
        'VIIPER-windows-arm64-authenticode-${{ github.sha }}',
        'VIIPER-native-udecx-authenticode-${{ github.sha }}')) {
    if (-not $signingJob.Contains($requiredSigningGate)) {
        throw "The native signing job is missing fail-closed contract '$requiredSigningGate'."
    }
}
if ([regex]::Matches($signingJob, '-RequireAuthenticode').Count -lt 2 -or
        [regex]::Matches($signingJob, '-ExpectedSignerCertificateSHA256').Count -lt 2) {
    throw 'The native signing job must Authenticode-validate both composition and archive roundtrip with the pinned signer fingerprint.'
}

$createReleaseJob = [regex]::Match(
    $releaseSource,
    '(?ms)^\s{4}create-release:\s.*?(?=^\s{4}publish-client-registries:)').Value
foreach ($signedArtifact in @(
        'VIIPER-windows-amd64-authenticode-${{ github.sha }}',
        'VIIPER-windows-arm64-authenticode-${{ github.sha }}',
        'VIIPER-native-udecx-authenticode-${{ github.sha }}')) {
    if (-not $createReleaseJob.Contains($signedArtifact)) {
        throw "create-release must consume the exact signed artifact '$signedArtifact'."
    }
}
if ($createReleaseJob.Contains('path: native-helper') -or
        $createReleaseJob.Contains('path: native-production')) {
    throw 'create-release must not reconstruct the public package from unsigned helper or production-intake inputs.'
}

$nativeWorkflow = Get-Content -LiteralPath (Join-Path $workflowDirectory 'native-ude.yml') -Raw
if ($nativeWorkflow -notmatch '(?m)^\s*if:\s*\$\{\{\s*inputs\.upload_artifacts\s*==\s*true\s*\}\}\s*$') {
    throw 'Native test-signed artifacts may upload only through the explicit Boolean test-artifact input.'
}
foreach ($requiredNativeGate in @(
        'branches: [main, "feature/**"]',
        'tags: ["v*.*.*"]',
        'VIIPER_NATIVE_SOURCE_REVISION: ${{ github.sha }}',
        'Get-ViiperUdeBuildIdentity.ps1',
        '73d488839228708c594f1582a343ec07660d2d2fc4d009cb4db2d99cb9e554c9',
        'Test-ViiperUdeVersionMonotonicity.ps1',
        'x64/Release/ViiperUde/ViiperUde.inf',
        'inputs.upload_release_helper == true',
        'New-ViiperUdeLocalTestPackage.ps1',
        'ViiperUde-x64-local-test-${{ github.sha }}',
        'native/udecx/x64/Release/ViiperUdeLocalTest/**',
        'retention-days: 7',
        'internal/transport/udecx.nativeSourceRevision=$env:GITHUB_SHA')) {
    if (-not $nativeWorkflow.Contains($requiredNativeGate)) {
        throw "The native build workflow is missing gate '$requiredNativeGate'."
    }
}
if ($nativeWorkflow.Contains('native/udecx/x64/Release/**') -or
        $nativeWorkflow.Contains('native/udecx/driver/x64/Release/**') -or
        $nativeWorkflow.Contains('native/udecx/package/x64/Release/**')) {
    throw 'The local-test artifact must not upload broad compiler output trees.'
}

$baseBuildWorkflow = Get-Content -LiteralPath (Join-Path $workflowDirectory 'build_base.yml') -Raw
if (-not $baseBuildWorkflow.Contains('VIIPER_NATIVE_SOURCE_REVISION: ${{ github.sha }}')) {
    throw 'Production broker builds must inject the exact workflow source SHA.'
}
$justfile = Get-Content -LiteralPath (Join-Path $repositoryRoot 'justfile') -Raw
foreach ($requiredBuildIdentityGate in @(
        'Release builds require explicit VIIPER_NATIVE_SOURCE_REVISION.',
        'internal/transport/udecx.nativeSourceRevision=')) {
    if (-not $justfile.Contains($requiredBuildIdentityGate)) {
        throw "The release broker build is missing identity gate '$requiredBuildIdentityGate'."
    }
}

$transactionWorkflow = Get-Content -LiteralPath (Join-Path $workflowDirectory 'native-package-transaction.yml') -Raw
foreach ($requiredTransactionTrigger in @(
        'branches: [main, "feature/**"]',
        'tags: ["v*.*.*"]',
        'pull_request:')) {
    if (-not $transactionWorkflow.Contains($requiredTransactionTrigger)) {
        throw "The native transaction workflow is missing trigger '$requiredTransactionTrigger'."
    }
}
if ($transactionWorkflow.Contains('paths:')) {
    throw 'Native transaction simulations must not be bypassable through a path filter.'
}

$productionWorkflow = Get-Content -LiteralPath (Join-Path $workflowDirectory 'native-production-package.yml') -Raw
foreach ($required in @(
        'Test-ViiperUdeSignedPackage.ps1',
        '-ValidationMode Production',
        'Microsoft-signed',
        'signingRoute',
        "POLICY_REF -cne 'refs/heads/main'",
        'Test-ViiperUdeTargetCompatibility.ps1')) {
    if (-not $productionWorkflow.Contains($required)) {
        throw "The production-native workflow is missing required validation contract '$required'."
    }
}

$justfileSource = Get-Content -LiteralPath (Join-Path $repositoryRoot 'justfile') -Raw
if ($justfileSource.Contains('@latest') -or
        $justfileSource -notmatch 'goversioninfo/cmd/goversioninfo@v1\.7\.0' -or
        $justfileSource -notmatch 'go-licenses/v2@v2\.0\.1') {
    throw 'Release build helper dependencies in justfile must remain exactly pinned.'
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

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$PackageDirectory,

    [Parameter(Mandatory = $true)]
    [string]$SubmissionManifestPath,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$')]
    [string]$ExpectedSourceRevision,

    [ValidateSet('LocalTest', 'ControlledTest', 'Production')]
    [string]$ValidationMode = 'Production',

    [string]$LocalTestCertificatePath,

    [switch]$RequireLocalTestToolchainValidation
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-CertificateEkuOids {
    param(
        [Parameter(Mandatory = $true)]
        [Security.Cryptography.X509Certificates.X509Certificate2]$Certificate
    )

    $oids = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($extension in $Certificate.Extensions) {
        if ($extension.Oid.Value -ne '2.5.29.37') {
            continue
        }
        $eku = if ($extension -is [Security.Cryptography.X509Certificates.X509EnhancedKeyUsageExtension]) {
            $extension
        }
        else {
            [Security.Cryptography.X509Certificates.X509EnhancedKeyUsageExtension]::new($extension, $false)
        }
        foreach ($oid in $eku.EnhancedKeyUsages) {
            [void]$oids.Add($oid.Value)
        }
    }
    return ,$oids
}

function Get-CertificateSha256 {
    param(
        [Parameter(Mandatory = $true)]
        [Security.Cryptography.X509Certificates.X509Certificate2]$Certificate
    )

    $algorithm = [Security.Cryptography.SHA256]::Create()
    try {
        return ([BitConverter]::ToString(
            $algorithm.ComputeHash($Certificate.RawData))).Replace('-', '').ToLowerInvariant()
    }
    finally {
        $algorithm.Dispose()
    }
}

function Assert-DriverSignature {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [ValidateSet('LocalTest', 'ControlledTest', 'Production')]
        [string]$Mode,

        [string]$ExpectedLocalTestCertificateSha256
    )

    $signature = Get-AuthenticodeSignature -LiteralPath $Path
    if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid) {
        throw "'$Path' does not have a valid Authenticode signature (status '$($signature.Status)')."
    }
    if ($null -eq $signature.SignerCertificate) {
        throw "'$Path' did not expose its signing certificate."
    }
    if ($Mode -eq 'LocalTest') {
        $actual = Get-CertificateSha256 -Certificate $signature.SignerCertificate
        if ($ExpectedLocalTestCertificateSha256 -notmatch '^[0-9a-f]{64}$' -or
            $actual -cne $ExpectedLocalTestCertificateSha256) {
            throw "'$Path' is not signed by the exact source-bound local test certificate."
        }
        return
    }
    if (
        $signature.SignerCertificate.Subject -notmatch '(?i)(^|,\s*)O=Microsoft Corporation(,|$)') {
        throw "'$Path' is not signed by Microsoft Corporation."
    }

    $ekuOids = Get-CertificateEkuOids -Certificate $signature.SignerCertificate
    $hardwareVerificationOid = '1.3.6.1.4.1.311.10.3.5'
    $attestedVerificationOid = '1.3.6.1.4.1.311.10.3.5.1'
    if (-not $ekuOids.Contains($hardwareVerificationOid)) {
        throw "'$Path' lacks the Windows Hardware Driver Verification EKU."
    }
    if ($Mode -eq 'ControlledTest') {
        if (-not $ekuOids.Contains($attestedVerificationOid)) {
            throw "'$Path' is not a Microsoft attestation-signed controlled-test artifact."
        }
    }
    elseif ($ekuOids.Contains($attestedVerificationOid)) {
        throw "'$Path' is attestation signed and cannot pass the production HLK/WHCP release gate."
    }
}

$root = Resolve-Path -LiteralPath $PackageDirectory -ErrorAction Stop
if (-not (Get-Item -LiteralPath $root.Path).PSIsContainer) {
    throw 'The signed package path must be a directory.'
}

$localTestCertificate = $null
$localTestCertificateSha256 = $null
if ($ValidationMode -eq 'LocalTest') {
    if ([string]::IsNullOrWhiteSpace($LocalTestCertificatePath)) {
        throw '-LocalTestCertificatePath is required for LocalTest validation.'
    }
    $resolvedCertificate = (Resolve-Path -LiteralPath $LocalTestCertificatePath -ErrorAction Stop).Path
    $certificateItem = Get-Item -LiteralPath $resolvedCertificate -Force
    if ($certificateItem.PSIsContainer -or $certificateItem.Length -le 0 -or
        $certificateItem.Name -cne 'ViiperUdeTest.cer' -or
        ($certificateItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw 'The local test certificate must be a nonempty, case-exact, non-reparse ViiperUdeTest.cer file.'
    }
    $localTestCertificate = [Security.Cryptography.X509Certificates.X509Certificate2]::new($resolvedCertificate)
    $localTestCertificateSha256 = Get-CertificateSha256 -Certificate $localTestCertificate
}
elseif (-not [string]::IsNullOrWhiteSpace($LocalTestCertificatePath)) {
    throw '-LocalTestCertificatePath is valid only with -ValidationMode LocalTest.'
}
if ($RequireLocalTestToolchainValidation -and $ValidationMode -ne 'LocalTest') {
    throw '-RequireLocalTestToolchainValidation is valid only with -ValidationMode LocalTest.'
}

$expectedNames = @('ViiperUde.inf', 'ViiperUde.sys', 'ViiperUde.pdb', 'ViiperUde.cat')
$allEntries = @(Get-ChildItem -LiteralPath $root.Path -Force)
if (@($allEntries | Where-Object PSIsContainer).Count -ne 0) {
    throw 'The signed package files must be direct children of the package directory; subdirectories are forbidden.'
}
$allFiles = @($allEntries | Where-Object { -not $_.PSIsContainer })
if ($allFiles.Count -ne $expectedNames.Count) {
    throw "The signed package must contain exactly $($expectedNames.Count) files; found $($allFiles.Count)."
}
$files = @{}
foreach ($name in $expectedNames) {
    $matches = @($allFiles | Where-Object Name -CEQ $name)
    if ($matches.Count -ne 1) {
        throw "The signed package must contain exactly one case-exact '$name'; found $($matches.Count)."
    }
    if ($matches[0].Length -le 0) {
        throw "The signed package file '$name' is empty."
    }
    $files[$name] = $matches[0].FullName
}
if (@($allFiles | Where-Object { $_.DirectoryName -cne $root.Path }).Count -ne 0) {
    throw 'All signed package files must reside directly in the canonical package directory.'
}

$manifestFile = Resolve-Path -LiteralPath $SubmissionManifestPath -ErrorAction Stop
$manifest = Get-Content -LiteralPath $manifestFile.Path -Raw | ConvertFrom-Json
$projectPath = Join-Path $PSScriptRoot '..\driver\ViiperUde.vcxproj'
[xml]$driverProject = Get-Content -LiteralPath $projectPath -Raw
$projectNamespace = [Xml.XmlNamespaceManager]::new($driverProject.NameTable)
$projectNamespace.AddNamespace('msb', 'http://schemas.microsoft.com/developer/msbuild/2003')
$versionNodes = @($driverProject.SelectNodes('//msb:ViiperUdeDriverVersion', $projectNamespace))
if ($versionNodes.Count -ne 1) {
    throw 'The driver project must declare one deterministic ViiperUdeDriverVersion.'
}
$driverPackageVersion = $versionNodes[0].InnerText.Trim()
$expectedBuildIdentity = & (Join-Path $PSScriptRoot 'Get-ViiperUdeBuildIdentity.ps1') `
    -SourceRevision $ExpectedSourceRevision `
    -DriverPackageVersion $driverPackageVersion `
    -ABIMajor 1 -ABIMinor 10 -Capabilities 13
if ($manifest.schema -ne 2 -or
    [string]$manifest.sourceRevision -cne $ExpectedSourceRevision.ToLowerInvariant() -or
    [string]$manifest.driverPackageVersion -cne $driverPackageVersion -or
    [int]$manifest.driverABIMajor -ne 1 -or [int]$manifest.driverABIMinor -ne 10 -or
    [string]$manifest.driverCapabilities -cne '0x0000000d' -or
    [string]$manifest.driverBuildIdentity -cne $expectedBuildIdentity) {
    throw 'The submission manifest schema, source revision, or native loaded-build identity does not match the reviewed source.'
}
if ($ValidationMode -eq 'LocalTest') {
    if ([bool]$manifest.releaseEligible -or [string]$manifest.signingRoute -cne 'LocalTest' -or
        [string]$manifest.testSignerCertificateSha256 -cne $localTestCertificateSha256) {
        throw 'LocalTest validation requires a non-release manifest bound to the exact local test certificate.'
    }
}
elseif ($ValidationMode -eq 'ControlledTest') {
    if ([bool]$manifest.releaseEligible -or [string]$manifest.signingRoute -cne 'ControlledTestAttestation') {
        throw 'Controlled-test validation requires a testing-only attestation submission manifest.'
    }
}
elseif (-not [bool]$manifest.releaseEligible -or [string]$manifest.signingRoute -cne 'HLK/WHCP') {
    throw 'Production validation requires a release-eligible HLK/WHCP submission manifest.'
}

$manifestFiles = @($manifest.files)
if ($manifestFiles.Count -ne $expectedNames.Count) {
    throw "The submission manifest must describe exactly $($expectedNames.Count) files."
}
$manifestByName = @{}
foreach ($entry in $manifestFiles) {
    $name = [string]$entry.name
    if ($expectedNames -cnotcontains $name -or $manifestByName.ContainsKey($name)) {
        throw "The submission manifest contains an unexpected or duplicate file '$name'."
    }
    if ([long]$entry.length -le 0 -or [string]$entry.sha256 -cnotmatch '^[0-9A-Fa-f]{64}$') {
        throw "The submission manifest contains invalid metadata for '$name'."
    }
    $manifestByName[$name] = $entry
}
foreach ($name in @('ViiperUde.inf', 'ViiperUde.pdb')) {
    if (-not $manifestByName.ContainsKey($name)) {
        throw "The submission manifest does not describe '$name'."
    }
    $actual = Get-Item -LiteralPath $files[$name]
    $actualHash = (Get-FileHash -LiteralPath $actual.FullName -Algorithm SHA256).Hash
    if ($actual.Length -ne [long]$manifestByName[$name].length -or
        $actualHash -cne ([string]$manifestByName[$name].sha256).ToUpperInvariant()) {
        throw "The Microsoft-returned '$name' does not match the source-bound submission manifest."
    }
}

foreach ($name in @('ViiperUde.cat', 'ViiperUde.sys')) {
    Assert-DriverSignature -Path $files[$name] -Mode $ValidationMode `
        -ExpectedLocalTestCertificateSha256 $localTestCertificateSha256
}
$requireExternalTools = $ValidationMode -ne 'LocalTest' -or $RequireLocalTestToolchainValidation
if ($requireExternalTools) {
    $signTool = Get-Command signtool.exe -ErrorAction Stop
    foreach ($name in @('ViiperUde.cat', 'ViiperUde.sys')) {
        $policy = if ($ValidationMode -eq 'LocalTest') { '/pa' } else { '/kp' }
        & $signTool.Source verify $policy /v $files[$name]
        if ($LASTEXITCODE -ne 0) {
            throw "Signature policy validation failed for '$name' with exit code $LASTEXITCODE."
        }
    }
    foreach ($name in @('ViiperUde.inf', 'ViiperUde.sys')) {
        $policy = if ($ValidationMode -eq 'LocalTest') { '/pa' } else { '/kp' }
        & $signTool.Source verify $policy /v /c $files['ViiperUde.cat'] $files[$name]
        if ($LASTEXITCODE -ne 0) {
            throw "'$name' is not a verified member of the exact catalog (exit code $LASTEXITCODE)."
        }
    }

    $infVerif = Get-Command infverif.exe -ErrorAction Stop
    foreach ($mode in @('/h', '/u')) {
        & $infVerif.Source $mode $files['ViiperUde.inf']
        if ($LASTEXITCODE -ne 0) {
            throw "InfVerif $mode rejected the signed package with exit code $LASTEXITCODE."
        }
    }
}

$signatureKind = if ($ValidationMode -eq 'LocalTest') { 'local test-signed' } else { 'Microsoft-signed' }
Write-Host "Validated source-bound $signatureKind VIIPER native UDE package in $ValidationMode mode at '$($root.Path)'."

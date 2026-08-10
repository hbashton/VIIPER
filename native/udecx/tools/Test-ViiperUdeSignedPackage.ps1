[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$PackageDirectory,

    [Parameter(Mandatory = $true)]
    [string]$SubmissionManifestPath,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-fA-F]{40,64}$')]
    [string]$ExpectedSourceRevision,

    [ValidateSet('ControlledTest', 'Production')]
    [string]$ValidationMode = 'Production'
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

function Assert-MicrosoftHardwareSignature {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [ValidateSet('ControlledTest', 'Production')]
        [string]$Mode
    )

    $signature = Get-AuthenticodeSignature -LiteralPath $Path
    if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid) {
        throw "'$Path' does not have a valid Authenticode signature (status '$($signature.Status)')."
    }
    if ($null -eq $signature.SignerCertificate -or
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

$expectedNames = @('ViiperUde.inf', 'ViiperUde.sys', 'ViiperUde.pdb', 'ViiperUde.cat')
$allFiles = @(Get-ChildItem -LiteralPath $root.Path -Recurse -File)
if ($allFiles.Count -ne $expectedNames.Count) {
    throw "The signed package must contain exactly $($expectedNames.Count) files; found $($allFiles.Count)."
}
$files = @{}
foreach ($name in $expectedNames) {
    $matches = @($allFiles | Where-Object Name -CEQ $name)
    if ($matches.Count -ne 1) {
        throw "The signed package must contain exactly one case-exact '$name'; found $($matches.Count)."
    }
    $files[$name] = $matches[0].FullName
}
$packageParents = @($allFiles.DirectoryName | Sort-Object -Unique)
if ($packageParents.Count -ne 1) {
    throw 'The signed package files must share one canonical package directory.'
}

$manifestFile = Resolve-Path -LiteralPath $SubmissionManifestPath -ErrorAction Stop
$manifest = Get-Content -LiteralPath $manifestFile.Path -Raw | ConvertFrom-Json
if ($manifest.schema -ne 1 -or
    [string]$manifest.sourceRevision -cne $ExpectedSourceRevision.ToLowerInvariant()) {
    throw 'The submission manifest schema or source revision does not match the reviewed source.'
}
if ($ValidationMode -eq 'ControlledTest') {
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

$signTool = Get-Command signtool.exe -ErrorAction Stop
foreach ($name in @('ViiperUde.cat', 'ViiperUde.sys')) {
    & $signTool.Source verify /kp /v $files[$name]
    if ($LASTEXITCODE -ne 0) {
        throw "Kernel-policy signature validation failed for '$name' with exit code $LASTEXITCODE."
    }
    Assert-MicrosoftHardwareSignature -Path $files[$name] -Mode $ValidationMode
}
foreach ($name in @('ViiperUde.inf', 'ViiperUde.sys')) {
    & $signTool.Source verify /kp /v /c $files['ViiperUde.cat'] $files[$name]
    if ($LASTEXITCODE -ne 0) {
        throw "'$name' is not a verified member of the Microsoft-signed catalog (exit code $LASTEXITCODE)."
    }
}

$infVerif = Get-Command infverif.exe -ErrorAction Stop
foreach ($mode in @('/h', '/u')) {
    & $infVerif.Source $mode $files['ViiperUde.inf']
    if ($LASTEXITCODE -ne 0) {
        throw "InfVerif $mode rejected the Microsoft-signed package with exit code $LASTEXITCODE."
    }
}

Write-Host "Validated source-bound Microsoft-signed VIIPER native UDE package in $ValidationMode mode at '$($root.Path)'."

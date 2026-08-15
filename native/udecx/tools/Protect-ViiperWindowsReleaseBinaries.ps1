[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string[]]$Paths,

    [Parameter(Mandatory = $true)]
    [string]$CertificateBase64,

    [Parameter(Mandatory = $true)]
    [string]$CertificatePassword,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-fA-F]{64}$')]
    [string]$ExpectedCertificateSHA256,

    [Parameter(Mandatory = $true)]
    [string]$SignToolPath,

    [ValidatePattern('^https?://[^\s]+$')]
    [string]$TimestampUrl = 'http://timestamp.digicert.com'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-CertificateSha256 {
    param(
        [Parameter(Mandatory = $true)]
        [Security.Cryptography.X509Certificates.X509Certificate2]$Certificate
    )

    $sha256 = [Security.Cryptography.SHA256]::Create()
    try {
        return ([BitConverter]::ToString($sha256.ComputeHash($Certificate.RawData))).Replace('-', '').ToLowerInvariant()
    }
    finally {
        $sha256.Dispose()
    }
}

function Test-CodeSigningEku {
    param(
        [Parameter(Mandatory = $true)]
        [Security.Cryptography.X509Certificates.X509Certificate2]$Certificate
    )

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
        return @($eku.EnhancedKeyUsages | Where-Object Value -ceq '1.3.6.1.5.5.7.3.3').Count -eq 1
    }
    return $false
}

$signTool = (Resolve-Path -LiteralPath $SignToolPath -ErrorAction Stop).Path
if ((Get-Item -LiteralPath $signTool).PSIsContainer -or
        [IO.Path]::GetFileName($signTool) -ine 'signtool.exe') {
    throw 'SignToolPath must identify the exact restored signtool.exe.'
}

$resolvedPaths = @()
foreach ($path in $Paths) {
    $item = Get-Item -LiteralPath (Resolve-Path -LiteralPath $path -ErrorAction Stop).Path -Force
    if ($item.PSIsContainer -or $item.Length -le 0 -or $item.Extension -ine '.exe') {
        throw "Release signing accepts only nonempty .exe files; rejected '$path'."
    }
    $stream = [IO.File]::OpenRead($item.FullName)
    try {
        if ($stream.ReadByte() -ne 0x4d -or $stream.ReadByte() -ne 0x5a) {
            throw "Release signing input '$path' is not a Windows PE image."
        }
    }
    finally {
        $stream.Dispose()
    }
    $resolvedPaths += $item.FullName
}
if ($resolvedPaths.Count -eq 0 -or @($resolvedPaths | Sort-Object -Unique).Count -ne $resolvedPaths.Count) {
    throw 'Release signing requires one or more unique executable paths.'
}

$expectedDigest = $ExpectedCertificateSHA256.ToLowerInvariant()
$pfxPath = Join-Path ([IO.Path]::GetTempPath()) ("viiper-release-signing-{0}.pfx" -f [Guid]::NewGuid().ToString('N'))
$importedCertificates = @()
$preexistingThumbprints = @(
    Get-ChildItem Cert:\CurrentUser\My -ErrorAction SilentlyContinue |
        ForEach-Object Thumbprint)
try {
    try {
        $pfxBytes = [Convert]::FromBase64String($CertificateBase64)
    }
    catch {
        throw 'WINDOWS_SIGNING_PFX_BASE64 is not valid base64.'
    }
    if ($pfxBytes.Length -eq 0) {
        throw 'WINDOWS_SIGNING_PFX_BASE64 decoded to an empty file.'
    }
    [IO.File]::WriteAllBytes($pfxPath, $pfxBytes)
    $securePassword = ConvertTo-SecureString -String $CertificatePassword -AsPlainText -Force
    $importedCertificates = @(
        Import-PfxCertificate -FilePath $pfxPath -CertStoreLocation Cert:\CurrentUser\My `
            -Password $securePassword -Exportable:$false)
    $signers = @($importedCertificates | Where-Object { $_.HasPrivateKey -and (Test-CodeSigningEku $_) })
    if ($signers.Count -ne 1) {
        throw "The release PFX must contain exactly one private-key certificate with the Code Signing EKU; found $($signers.Count)."
    }
    $certificate = $signers[0]
    if ((Get-CertificateSha256 $certificate) -cne $expectedDigest) {
        throw 'The release PFX certificate does not match WINDOWS_SIGNING_CERTIFICATE_SHA256.'
    }
    if ($certificate.Subject -ceq $certificate.Issuer -or
            $certificate.Subject -match '(?i)(^|[ ,])(test|self[- ]?signed)([ ,]|$)') {
        throw 'Self-signed or test-named certificates cannot sign a public VIIPER release.'
    }
    $now = [DateTime]::UtcNow
    if ($now -lt $certificate.NotBefore.ToUniversalTime() -or
            $now -gt $certificate.NotAfter.ToUniversalTime()) {
        throw 'The release code-signing certificate is not currently valid.'
    }

    $chain = New-Object Security.Cryptography.X509Certificates.X509Chain
    try {
        $chain.ChainPolicy.RevocationMode = [Security.Cryptography.X509Certificates.X509RevocationMode]::Online
        $chain.ChainPolicy.RevocationFlag = [Security.Cryptography.X509Certificates.X509RevocationFlag]::ExcludeRoot
        $chain.ChainPolicy.VerificationFlags = [Security.Cryptography.X509Certificates.X509VerificationFlags]::NoFlag
        if (-not $chain.Build($certificate)) {
            $status = @($chain.ChainStatus | ForEach-Object StatusInformation) -join '; '
            throw "The release code-signing certificate did not build a trusted revocation-checked chain: $status"
        }
    }
    finally {
        $chain.Dispose()
    }

    foreach ($path in $resolvedPaths) {
        & $signTool sign /sha1 $certificate.Thumbprint /s My /fd SHA256 `
            /tr $TimestampUrl /td SHA256 $path
        if ($LASTEXITCODE -ne 0) {
            throw "SignTool failed to sign '$path' (exit $LASTEXITCODE)."
        }
        & $signTool verify /pa /all /v $path
        if ($LASTEXITCODE -ne 0) {
            throw "SignTool failed Authenticode policy verification for '$path' (exit $LASTEXITCODE)."
        }
        $signature = Get-AuthenticodeSignature -LiteralPath $path
        if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid -or
                $null -eq $signature.SignerCertificate -or
                $null -eq $signature.TimeStamperCertificate -or
                (Get-CertificateSha256 $signature.SignerCertificate) -cne $expectedDigest -or
                -not (Test-CodeSigningEku $signature.SignerCertificate)) {
            throw "'$path' does not have the expected trusted, timestamped production Authenticode signature."
        }
        Write-Host "Signed and verified $path with certificate SHA-256 $expectedDigest."
    }
}
finally {
    foreach ($certificate in $importedCertificates) {
        if ($preexistingThumbprints -cnotcontains $certificate.Thumbprint) {
            Remove-Item -LiteralPath "Cert:\CurrentUser\My\$($certificate.Thumbprint)" -Force -ErrorAction SilentlyContinue
        }
    }
    if (Test-Path -LiteralPath $pfxPath) {
        Remove-Item -LiteralPath $pfxPath -Force
    }
}

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$BundleDirectory,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(?:[0-9a-f]{40}|[0-9a-f]{64})$')]
    [string]$ExpectedSourceRevision,

    [string]$ProjectPath,

    [switch]$RequireAuthenticode,

    [ValidatePattern('^$|^[0-9a-fA-F]{64}$')]
    [string]$ExpectedSignerCertificateSHA256
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ([string]::IsNullOrWhiteSpace($ProjectPath)) {
    $ProjectPath = Join-Path $PSScriptRoot '..\driver\ViiperUde.vcxproj'
}

$root = (Resolve-Path -LiteralPath $BundleDirectory -ErrorAction Stop).Path
if (-not (Get-Item -LiteralPath $root).PSIsContainer) {
    throw 'The native release bundle must be a directory.'
}

$expectedNames = @(
    'viiper.exe',
    'ViiperUdeCtl.exe',
    'ViiperUde.inf',
    'ViiperUde.sys',
    'ViiperUde.cat',
    'submission-manifest.json'
)
$allEntries = @(Get-ChildItem -LiteralPath $root -Force)
if (@($allEntries | Where-Object PSIsContainer).Count -ne 0) {
    throw 'The native runtime files must be direct children of the bundle directory; subdirectories are forbidden.'
}
$allFiles = @($allEntries | Where-Object { -not $_.PSIsContainer })
if ($allFiles.Count -ne $expectedNames.Count) {
    throw "The runtime bundle must contain exactly $($expectedNames.Count) files; found $($allFiles.Count)."
}
$files = @{}
foreach ($name in $expectedNames) {
    $matches = @($allFiles | Where-Object Name -CEQ $name)
    if ($matches.Count -ne 1) {
        throw "The runtime bundle must contain exactly one case-exact '$name'; found $($matches.Count)."
    }
    if ($matches[0].Length -le 0) {
        throw "The runtime bundle file '$name' is empty."
    }
    $files[$name] = $matches[0]
}
if (@($allFiles | Where-Object { $_.DirectoryName -cne $root }).Count -ne 0) {
    throw 'All native runtime files must reside directly in the canonical bundle directory.'
}
if (@($allFiles | Where-Object Extension -ieq '.pdb').Count -ne 0) {
    throw 'Private PDB submission evidence must not be shipped in the public runtime bundle.'
}

foreach ($name in @('viiper.exe', 'ViiperUdeCtl.exe', 'ViiperUde.sys')) {
    $stream = [IO.File]::OpenRead($files[$name].FullName)
    try {
        if ($stream.ReadByte() -ne 0x4d -or $stream.ReadByte() -ne 0x5a) {
            throw "The runtime artifact '$name' is not a Windows PE image."
        }
    }
    finally {
        $stream.Dispose()
    }
}

if ($RequireAuthenticode) {
    if ([string]::IsNullOrWhiteSpace($ExpectedSignerCertificateSHA256)) {
        throw '-RequireAuthenticode also requires ExpectedSignerCertificateSHA256.'
    }
    $expectedSigner = $ExpectedSignerCertificateSHA256.ToLowerInvariant()
    foreach ($name in @('viiper.exe', 'ViiperUdeCtl.exe')) {
        $signature = Get-AuthenticodeSignature -LiteralPath $files[$name].FullName
        if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid -or
                $null -eq $signature.SignerCertificate -or
                $null -eq $signature.TimeStamperCertificate) {
            throw "The runtime artifact '$name' lacks a valid timestamped Authenticode signature."
        }
        $sha256 = [Security.Cryptography.SHA256]::Create()
        try {
            $signerDigest = ([BitConverter]::ToString(
                    $sha256.ComputeHash($signature.SignerCertificate.RawData))).Replace('-', '').ToLowerInvariant()
        }
        finally {
            $sha256.Dispose()
        }
        if ($signerDigest -cne $expectedSigner) {
            throw "The runtime artifact '$name' was not signed by the release certificate allowlist."
        }
        $codeSigningEkus = @(
            foreach ($extension in $signature.SignerCertificate.Extensions) {
                if ($extension.Oid.Value -ne '2.5.29.37') { continue }
                $eku = if ($extension -is [Security.Cryptography.X509Certificates.X509EnhancedKeyUsageExtension]) {
                    $extension
                }
                else {
                    [Security.Cryptography.X509Certificates.X509EnhancedKeyUsageExtension]::new($extension, $false)
                }
                @($eku.EnhancedKeyUsages | Where-Object Value -ceq '1.3.6.1.5.5.7.3.3')
            })
        if ($codeSigningEkus.Count -ne 1) {
            throw "The runtime artifact '$name' signer lacks the Code Signing EKU."
        }
    }
}

$manifest = Get-Content -LiteralPath $files['submission-manifest.json'].FullName -Raw |
    ConvertFrom-Json

$submissionNames = @('ViiperUde.inf', 'ViiperUde.sys', 'ViiperUde.pdb', 'ViiperUde.cat')
$manifestEntries = @($manifest.files)
if ($manifestEntries.Count -ne $submissionNames.Count) {
    throw 'The HLK/WHCP submission manifest must describe exactly INF, SYS, PDB, and CAT submission inputs.'
}
$manifestByName = @{}
foreach ($entry in $manifestEntries) {
    $name = [string]$entry.name
    if ($submissionNames -cnotcontains $name -or $manifestByName.ContainsKey($name)) {
        throw "The HLK/WHCP manifest contains an unexpected or duplicate file '$name'."
    }
    if ([long]$entry.length -le 0 -or [string]$entry.sha256 -cnotmatch '^[0-9A-Fa-f]{64}$') {
        throw "The HLK/WHCP manifest contains invalid metadata for '$name'."
    }
    $manifestByName[$name] = $entry
}

# Microsoft signing changes the SYS and CAT bytes. The stamped INF remains
# unchanged and is the source-bound runtime member that can be compared to the
# pre-submission manifest after the signed package has passed the Windows gate.
$runtimeInf = $files['ViiperUde.inf']
$runtimeInfHash = (Get-FileHash -LiteralPath $runtimeInf.FullName -Algorithm SHA256).Hash
if ($runtimeInf.Length -ne [long]$manifestByName['ViiperUde.inf'].length -or
        $runtimeInfHash -cne ([string]$manifestByName['ViiperUde.inf'].sha256).ToUpperInvariant()) {
    throw 'The runtime INF does not match the source-bound HLK/WHCP submission manifest.'
}

[xml]$project = Get-Content -LiteralPath (Resolve-Path -LiteralPath $ProjectPath).Path -Raw
$namespace = New-Object System.Xml.XmlNamespaceManager($project.NameTable)
$namespace.AddNamespace('msb', 'http://schemas.microsoft.com/developer/msbuild/2003')
$dateNodes = @($project.SelectNodes('//msb:ViiperUdeDriverDate', $namespace))
$versionNodes = @($project.SelectNodes('//msb:ViiperUdeDriverVersion', $namespace))
if ($dateNodes.Count -ne 1 -or $versionNodes.Count -ne 1) {
    throw 'The reviewed native project must declare one deterministic DriverVer date and version.'
}
$driverDate = $dateNodes[0].InnerText.Trim()
$driverVersion = $versionNodes[0].InnerText.Trim()
$expectedBuildIdentity = & (Join-Path $PSScriptRoot 'Get-ViiperUdeBuildIdentity.ps1') `
    -SourceRevision $ExpectedSourceRevision `
    -DriverPackageVersion $driverVersion `
    -ABIMajor 1 -ABIMinor 9 -Capabilities 13
if ($manifest.schema -ne 2 -or
        [string]$manifest.sourceRevision -cne $ExpectedSourceRevision -or
        [string]$manifest.driverPackageVersion -cne $driverVersion -or
        [int]$manifest.driverABIMajor -ne 1 -or [int]$manifest.driverABIMinor -ne 9 -or
        [string]$manifest.driverCapabilities -cne '0x0000000d' -or
        [string]$manifest.driverBuildIdentity -cne $expectedBuildIdentity -or
        -not [bool]$manifest.releaseEligible -or
        [string]$manifest.signingRoute -cne 'HLK/WHCP') {
    throw 'The runtime bundle requires the exact release-eligible HLK/WHCP loaded-driver build identity manifest.'
}
$infContents = Get-Content -LiteralPath $runtimeInf.FullName -Raw
$driverVerPattern = '(?mi)^DriverVer\s*=\s*' +
    [regex]::Escape($driverDate) + '\s*,\s*' +
    [regex]::Escape($driverVersion) + '\s*$'
if ($infContents -notmatch $driverVerPattern -or
        $infContents -notmatch '(?mi)^KmdfLibraryVersion\s*=\s*1\.27\s*$') {
    throw "The runtime INF is not the stamped DriverVer/KMDF output reviewed at $ExpectedSourceRevision."
}

foreach ($name in $expectedNames) {
    $hash = (Get-FileHash -LiteralPath $files[$name].FullName -Algorithm SHA256).Hash.ToLowerInvariant()
    Write-Host "$name sha256:$hash"
}
Write-Host "Validated exact six-file VIIPER native runtime bundle for $ExpectedSourceRevision (Microsoft HLK/WHCP route)."

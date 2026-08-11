[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$InfPath,
    [Parameter(Mandatory = $true)][string]$SysPath,
    [Parameter(Mandatory = $true)][string]$PdbPath,
    [Parameter(Mandatory = $true)][string]$CatalogPath,
    [Parameter(Mandatory = $true)][string]$TestCertificatePath,
    [Parameter(Mandatory = $true)][string]$BrokerPath,
    [Parameter(Mandatory = $true)][string]$HelperPath,
    [Parameter(Mandatory = $true)][string]$MediaProbePath,
    [Parameter(Mandatory = $true)][string]$InputProbePath,
    [Parameter(Mandatory = $true)][string]$ProbeManifestPath,
    [Parameter(Mandatory = $true)][string]$OutputDirectory,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$')]
    [string]$SourceRevision
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-CertificateSha256 {
    param([Parameter(Mandatory = $true)]$Certificate)

    $algorithm = [Security.Cryptography.SHA256]::Create()
    try {
        return ([BitConverter]::ToString(
            $algorithm.ComputeHash($Certificate.RawData))).Replace('-', '').ToLowerInvariant()
    }
    finally {
        $algorithm.Dispose()
    }
}

function Resolve-ExactInput {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$ExpectedName
    )

    $resolved = (Resolve-Path -LiteralPath $Path -ErrorAction Stop).Path
    $item = Get-Item -LiteralPath $resolved -Force
    if (-not $item.PSIsContainer -and $item.Length -gt 0 -and
        $item.Name -ceq $ExpectedName -and
        ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0) {
        return $resolved
    }
    throw "Local test input must be a nonempty, case-exact, non-reparse '$ExpectedName': '$Path'."
}

$inputs = [ordered]@{
    'ViiperUde.inf' = Resolve-ExactInput $InfPath 'ViiperUde.inf'
    'ViiperUde.sys' = Resolve-ExactInput $SysPath 'ViiperUde.sys'
    'ViiperUde.pdb' = Resolve-ExactInput $PdbPath 'ViiperUde.pdb'
    'ViiperUde.cat' = Resolve-ExactInput $CatalogPath 'ViiperUde.cat'
}
$helper = Resolve-ExactInput $HelperPath 'ViiperUdeCtl.exe'
$broker = Resolve-ExactInput $BrokerPath 'viiper.exe'
$mediaProbe = Resolve-ExactInput $MediaProbePath 'ViiperUdeMediaProbe.exe'
$inputProbe = Resolve-ExactInput $InputProbePath 'ViiperUdeInputProbe.exe'
$probeManifest = Resolve-ExactInput $ProbeManifestPath 'ViiperUdeLiveProbes.manifest.json'
$testCertificate = Resolve-ExactInput $TestCertificatePath 'ViiperUde.cer'

$output = [IO.Path]::GetFullPath($OutputDirectory)
if (Test-Path -LiteralPath $output) {
    throw "Refusing to overwrite local test package '$output'."
}

$expectedCertificate = [Security.Cryptography.X509Certificates.X509Certificate2]::new(
    $testCertificate)
try {
    $certificateSha256 = Get-CertificateSha256 $expectedCertificate
}
finally {
    $expectedCertificate.Dispose()
}

[void][IO.Directory]::CreateDirectory($output)
$signedDirectory = Join-Path $output 'signed-package'
$driverDirectory = Join-Path $output 'driver'
[void][IO.Directory]::CreateDirectory($signedDirectory)
[void][IO.Directory]::CreateDirectory($driverDirectory)

foreach ($entry in $inputs.GetEnumerator()) {
    [IO.File]::Copy($entry.Value, (Join-Path $signedDirectory $entry.Key), $false)
    if ($entry.Key -cne 'ViiperUde.pdb') {
        [IO.File]::Copy($entry.Value, (Join-Path $driverDirectory $entry.Key), $false)
    }
}
[IO.File]::Copy($helper, (Join-Path $output 'ViiperUdeCtl.exe'), $false)
[IO.File]::Copy($broker, (Join-Path $output 'viiper.exe'), $false)
[IO.File]::Copy($mediaProbe, (Join-Path $output 'ViiperUdeMediaProbe.exe'), $false)
[IO.File]::Copy($inputProbe, (Join-Path $output 'ViiperUdeInputProbe.exe'), $false)
[IO.File]::Copy($probeManifest, (Join-Path $output 'ViiperUdeLiveProbes.manifest.json'), $false)
$certificatePath = Join-Path $output 'ViiperUdeTest.cer'
[IO.File]::Copy($testCertificate, $certificatePath, $false)

[xml]$project = Get-Content -LiteralPath (Join-Path $PSScriptRoot '..\driver\ViiperUde.vcxproj') -Raw
$namespace = [Xml.XmlNamespaceManager]::new($project.NameTable)
$namespace.AddNamespace('msb', 'http://schemas.microsoft.com/developer/msbuild/2003')
$versionNodes = @($project.SelectNodes('//msb:ViiperUdeDriverVersion', $namespace))
if ($versionNodes.Count -ne 1) {
    throw 'The native driver project must declare one package version.'
}
$driverVersion = $versionNodes[0].InnerText.Trim()
$source = $SourceRevision.ToLowerInvariant()
$buildIdentity = & (Join-Path $PSScriptRoot 'Get-ViiperUdeBuildIdentity.ps1') `
    -SourceRevision $source -DriverPackageVersion $driverVersion `
    -ABIMajor 1 -ABIMinor 10 -Capabilities 13

$manifest = [ordered]@{
    schema = 2
    purpose = 'Local test-signed VIIPER UdeCx package; disposable test machines only'
    releaseEligible = $false
    signingRoute = 'LocalTest'
    requiredProductionRoute = 'HLK/WHCP dashboard signing'
    sourceRevision = $source
    driverPackageVersion = $driverVersion
    driverABIMajor = 1
    driverABIMinor = 10
    driverCapabilities = '0x0000000d'
    driverBuildIdentity = $buildIdentity
    testSignerCertificateSha256 = $certificateSha256
    files = @(
        foreach ($entry in $inputs.GetEnumerator()) {
            $path = Join-Path $signedDirectory $entry.Key
            [ordered]@{
                name = $entry.Key
                length = (Get-Item -LiteralPath $path).Length
                sha256 = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
            }
        }
    )
}
$manifestPath = Join-Path $output 'submission-manifest.json'
[IO.File]::WriteAllText($manifestPath, ($manifest | ConvertTo-Json -Depth 5),
    [Text.UTF8Encoding]::new($false))

$payloadNames = @(
    'viiper.exe', 'ViiperUdeCtl.exe', 'ViiperUdeMediaProbe.exe', 'ViiperUdeInputProbe.exe',
    'ViiperUdeLiveProbes.manifest.json', 'ViiperUdeTest.cer',
    'submission-manifest.json',
    'driver/ViiperUde.inf', 'driver/ViiperUde.sys', 'driver/ViiperUde.cat',
    'signed-package/ViiperUde.inf', 'signed-package/ViiperUde.sys',
    'signed-package/ViiperUde.pdb', 'signed-package/ViiperUde.cat'
)
$lockFiles = @(
    foreach ($relative in $payloadNames) {
        $path = Join-Path $output $relative.Replace('/', [IO.Path]::DirectorySeparatorChar)
        [ordered]@{
            path = $relative
            length = (Get-Item -LiteralPath $path).Length
            sha256 = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
        }
    }
)
$installerScriptPath = (Resolve-Path -LiteralPath (
    Join-Path $PSScriptRoot 'Install-ViiperUdeLocalTest.ps1') -ErrorAction Stop).Path
$installerScriptSha256 = (Get-FileHash -LiteralPath $installerScriptPath `
    -Algorithm SHA256).Hash.ToLowerInvariant()
$lock = [ordered]@{
    schema = 1
    sourceRevision = $source
    driverPackageVersion = $driverVersion
    driverBuildIdentity = $buildIdentity
    testSignerCertificateSha256 = $certificateSha256
    installerScriptSha256 = $installerScriptSha256
    files = $lockFiles
}
$lockPath = Join-Path $output 'local-test-package.lock.json'
[IO.File]::WriteAllText($lockPath,
    ($lock | ConvertTo-Json -Depth 5), [Text.UTF8Encoding]::new($false))
$lockSha256 = (Get-FileHash -LiteralPath $lockPath -Algorithm SHA256).Hash.ToLowerInvariant()

& (Join-Path $PSScriptRoot 'Test-ViiperUdeSignedPackage.ps1') `
    -PackageDirectory $signedDirectory `
    -SubmissionManifestPath $manifestPath `
    -ExpectedSourceRevision $source `
    -ValidationMode LocalTest `
    -LocalTestCertificatePath $certificatePath `
    -RequireLocalTestToolchainValidation

Write-Host "Created compact source-bound local test package at '$output'."
Write-Host "Source: $source"
Write-Host "Driver: $driverVersion / ABI 1.10 / $buildIdentity"
Write-Host "Test signer certificate SHA-256: $certificateSha256"
Write-Host "Local test package lock SHA-256: $lockSha256"

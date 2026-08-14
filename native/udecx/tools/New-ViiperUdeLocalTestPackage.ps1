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
    -ABIMajor 1 -ABIMinor 11 -Capabilities 29

$manifest = [ordered]@{
    schema = 2
    purpose = 'Local test-signed VIIPER UdeCx package; disposable test machines only'
    releaseEligible = $false
    signingRoute = 'LocalTest'
    requiredProductionRoute = 'HLK/WHCP dashboard signing'
    sourceRevision = $source
    driverPackageVersion = $driverVersion
    driverABIMajor = 1
    driverABIMinor = 11
    driverCapabilities = '0x0000001d'
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

# Bind the locked installer arguments to the compiled Kong command surface.
$brokerHelpOutput = @(& $broker native-package-install --help 2>&1)
$brokerHelpExitCode = $LASTEXITCODE
$brokerHelpText = $brokerHelpOutput -join [Environment]::NewLine
$expectedBrokerFlags = @(
    '--expected-broker-sha-256', '--expected-helper-sha-256',
    '--expected-manifest-sha-256', '--expected-inf-sha-256',
    '--expected-sys-sha-256', '--expected-cat-sha-256'
)
if ($brokerHelpExitCode -ne 0 -or
    @($expectedBrokerFlags | Where-Object {
        $brokerHelpText -notmatch [regex]::Escape($_)
    }).Count -ne 0) {
    throw "Compiled local-test broker command contract is incompatible with the locked installer.`n$brokerHelpText"
}

# The retained native helper launches this hidden broker command directly.
# Exercise its generated Kong option names so source-bound test artifacts
# cannot ship a helper/broker CLI mismatch that fails after driver mutation.
$brokerCommitHelpOutput = @(& $broker native-package-broker-commit --help 2>&1)
$brokerCommitHelpExitCode = $LASTEXITCODE
$brokerCommitHelpText = $brokerCommitHelpOutput -join [Environment]::NewLine
$expectedBrokerCommitFlags = @(
    '--token-file', '--expected-token-sha-256',
    '--expected-broker-sha-256', '--target-user-sid',
    '--transaction-deadline-unix-ms'
)
if ($brokerCommitHelpExitCode -ne 0 -or
    @($expectedBrokerCommitFlags | Where-Object {
        $brokerCommitHelpText -notmatch [regex]::Escape($_)
    }).Count -ne 0 -or
    $brokerCommitHelpText -match '--expected-(?:token|broker)-sha256') {
    throw "Compiled nested broker command contract is incompatible with the retained helper.`n$brokerCommitHelpText"
}

# Exercise the compiled helper's exact read-only SetupAPI/INF contract before
# publishing an installer artifact. Static source checks cannot prove the
# Windows API's two-call buffer-sizing behavior.
$manifestSha256 = (Get-FileHash -LiteralPath $manifestPath `
    -Algorithm SHA256).Hash.ToLowerInvariant()
$infSha256 = (Get-FileHash -LiteralPath (Join-Path $driverDirectory 'ViiperUde.inf') `
    -Algorithm SHA256).Hash.ToLowerInvariant()
$sysSha256 = (Get-FileHash -LiteralPath (Join-Path $driverDirectory 'ViiperUde.sys') `
    -Algorithm SHA256).Hash.ToLowerInvariant()
$catSha256 = (Get-FileHash -LiteralPath (Join-Path $driverDirectory 'ViiperUde.cat') `
    -Algorithm SHA256).Hash.ToLowerInvariant()
$deadline = [DateTimeOffset]::UtcNow.AddMinutes(4).ToUnixTimeMilliseconds().ToString()
$helperVerifyOutput = @(& $helper verify (Join-Path $driverDirectory 'ViiperUde.inf') `
    --manifest $manifestPath `
    --manifest-sha256 $manifestSha256 `
    --source-revision $source `
    --validation-mode local-test `
    --expected-inf-sha256 $infSha256 `
    --expected-sys-sha256 $sysSha256 `
    --expected-cat-sha256 $catSha256 `
    --transaction-deadline-unix-ms $deadline 2>&1)
$helperVerifyExitCode = $LASTEXITCODE
$helperVerifyText = $helperVerifyOutput -join [Environment]::NewLine
if ($helperVerifyExitCode -ne 0 -or
    @([regex]::Matches($helperVerifyText,
        '(?m)^result=success operation=verify changed=0 rebootRequired=0 rollback=not-needed exitCode=0\r?$')).Count -ne 1) {
    throw "Compiled local-test helper verification failed (exit $helperVerifyExitCode).`n$helperVerifyText"
}

# Run the exact elevated installer validation and protected-staging path under
# inbox Windows PowerShell 5.1 before publishing it. This route never imports
# trust, launches the broker, or changes driver/device/service state.
$windowsPowerShell = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
$preflightSid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$preflightOutput = @(& $windowsPowerShell -NoProfile -ExecutionPolicy Bypass `
    -File $installerScriptPath `
    -PackageRoot $output `
    -ExpectedSourceRevision $source `
    -ExpectedPackageLockSHA256 $lockSha256 `
    -TargetUserSID $preflightSid `
    -AcknowledgeDisposableTestMachine `
    -PreflightOnly 2>&1)
$preflightExitCode = $LASTEXITCODE
$preflightText = $preflightOutput -join [Environment]::NewLine
if ($preflightExitCode -ne 0 -or
    @([regex]::Matches($preflightText,
        '(?m)^result=success operation=local-test-preflight changed=0 rebootRequired=0 rollback=not-needed exitCode=0\r?$')).Count -ne 1) {
    throw "Windows PowerShell 5.1 local-test installer preflight failed (exit $preflightExitCode).`n$preflightText"
}

Write-Host "Created compact source-bound local test package at '$output'."
Write-Host "Source: $source"
Write-Host "Driver: $driverVersion / ABI 1.10 / $buildIdentity"
Write-Host "Test signer certificate SHA-256: $certificateSha256"
Write-Host "Local test package lock SHA-256: $lockSha256"

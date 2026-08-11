[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$PackageRoot,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$')]
    [string]$ExpectedSourceRevision,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^S-1-5-21-(?:[0-9]+-){3}[0-9]+$')]
    [string]$TargetUserSID,
    [switch]$AcknowledgeDisposableTestMachine
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not $AcknowledgeDisposableTestMachine) {
    throw 'Local test driver installation is for a disposable test machine only. Pass -AcknowledgeDisposableTestMachine.'
}
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Local VIIPER driver installation requires an elevated PowerShell session.'
}
$bcdOutput = (& bcdedit.exe /enum '{current}' 2>&1 | Out-String)
if ($LASTEXITCODE -ne 0 -or $bcdOutput -notmatch '(?im)^\s*testsigning\s+Yes\s*$') {
    throw "The current boot entry does not report 'testsigning Yes'. Enable TESTSIGNING and reboot before installation.`n$bcdOutput"
}

$root = (Resolve-Path -LiteralPath $PackageRoot -ErrorAction Stop).Path
$lockPath = Join-Path $root 'local-test-package.lock.json'
$manifestPath = Join-Path $root 'submission-manifest.json'
$certificatePath = Join-Path $root 'ViiperUdeTest.cer'
$helperPath = Join-Path $root 'ViiperUdeCtl.exe'
$brokerPath = Join-Path $root 'viiper.exe'
$signedPackage = Join-Path $root 'signed-package'
$driverDirectory = Join-Path $root 'driver'

function Assert-ExactDirectoryEntries {
    param(
        [Parameter(Mandatory = $true)][string]$Directory,
        [Parameter(Mandatory = $true)][string[]]$Expected
    )

    $directoryItem = Get-Item -LiteralPath $Directory -Force -ErrorAction Stop
    if (-not $directoryItem.PSIsContainer -or
        ($directoryItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Local test package directory is missing or unsafe: '$Directory'."
    }
    $actual = @(Get-ChildItem -LiteralPath $Directory -Force |
        ForEach-Object Name | Sort-Object -CaseSensitive)
    $wanted = @($Expected | Sort-Object -CaseSensitive)
    if ($actual.Count -ne $wanted.Count -or
        (Compare-Object -ReferenceObject $wanted -DifferenceObject $actual -CaseSensitive).Count -ne 0) {
        throw "Local test package directory has missing, extra, or case-mismatched entries: '$Directory'."
    }
}

Assert-ExactDirectoryEntries $root @(
    'viiper.exe', 'ViiperUdeCtl.exe', 'ViiperUdeMediaProbe.exe', 'ViiperUdeInputProbe.exe',
    'ViiperUdeLiveProbes.manifest.json', 'ViiperUdeTest.cer',
    'submission-manifest.json', 'local-test-package.lock.json',
    'driver', 'signed-package'
)
Assert-ExactDirectoryEntries $driverDirectory @(
    'ViiperUde.inf', 'ViiperUde.sys', 'ViiperUde.cat'
)
Assert-ExactDirectoryEntries $signedPackage @(
    'ViiperUde.inf', 'ViiperUde.sys', 'ViiperUde.pdb', 'ViiperUde.cat'
)

$lock = Get-Content -LiteralPath $lockPath -Raw -ErrorAction Stop | ConvertFrom-Json -ErrorAction Stop
$source = $ExpectedSourceRevision.ToLowerInvariant()
if ([int]$lock.schema -ne 1 -or [string]$lock.sourceRevision -cne $source -or
    [string]$lock.driverBuildIdentity -notmatch '^[0-9a-f]{64}$' -or
    [string]$lock.testSignerCertificateSha256 -notmatch '^[0-9a-f]{64}$') {
    throw 'The local test package lock does not match the requested source or schema.'
}

$expectedPaths = @(
    'viiper.exe', 'ViiperUdeCtl.exe', 'ViiperUdeMediaProbe.exe', 'ViiperUdeInputProbe.exe',
    'ViiperUdeLiveProbes.manifest.json', 'ViiperUdeTest.cer',
    'submission-manifest.json',
    'driver/ViiperUde.inf', 'driver/ViiperUde.sys', 'driver/ViiperUde.cat',
    'signed-package/ViiperUde.inf', 'signed-package/ViiperUde.sys',
    'signed-package/ViiperUde.pdb', 'signed-package/ViiperUde.cat'
)
$entries = @($lock.files)
if ($entries.Count -ne $expectedPaths.Count) {
    throw 'The local test package lock has an incomplete or extra file list.'
}
$seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
foreach ($entry in $entries) {
    $relative = [string]$entry.path
    if ($expectedPaths -cnotcontains $relative -or -not $seen.Add($relative) -or
        [long]$entry.length -le 0 -or [string]$entry.sha256 -notmatch '^[0-9a-f]{64}$') {
        throw "The local test package lock contains an invalid entry '$relative'."
    }
    $path = Join-Path $root $relative.Replace('/', [IO.Path]::DirectorySeparatorChar)
    $item = Get-Item -LiteralPath $path -Force -ErrorAction Stop
    if ($item.PSIsContainer -or
        ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or
        $item.Length -ne [long]$entry.length -or
        (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant() -cne
            [string]$entry.sha256) {
        throw "Local test package file validation failed for '$relative'."
    }
}

$certificate = [Security.Cryptography.X509Certificates.X509Certificate2]::new($certificatePath)
$algorithm = [Security.Cryptography.SHA256]::Create()
try {
    $certificateSha256 = ([BitConverter]::ToString(
        $algorithm.ComputeHash($certificate.RawData))).Replace('-', '').ToLowerInvariant()
}
finally {
    $algorithm.Dispose()
}
if ($certificateSha256 -cne [string]$lock.testSignerCertificateSha256) {
    throw 'The local test certificate does not match the source-bound package lock.'
}

$expectedCertificateBytes = [Convert]::ToBase64String($certificate.RawData)
$addedStores = [Collections.Generic.List[string]]::new()
try {
    foreach ($storeName in @('Root', 'TrustedPublisher')) {
        $store = [Security.Cryptography.X509Certificates.X509Store]::new(
            $storeName, [Security.Cryptography.X509Certificates.StoreLocation]::LocalMachine)
        try {
            $store.Open([Security.Cryptography.X509Certificates.OpenFlags]::ReadWrite)
            $present = @($store.Certificates | Where-Object {
                [Convert]::ToBase64String($_.RawData) -ceq $expectedCertificateBytes
            }).Count -ne 0
            if (-not $present) {
                $store.Add($certificate)
                $addedStores.Add($storeName)
            }
        }
        finally {
            $store.Close()
        }
    }

    & (Join-Path $PSScriptRoot 'Test-ViiperUdeSignedPackage.ps1') `
        -PackageDirectory $signedPackage `
        -SubmissionManifestPath $manifestPath `
        -ExpectedSourceRevision $source `
        -ValidationMode LocalTest `
        -LocalTestCertificatePath $certificatePath
}
catch {
    foreach ($storeName in $addedStores) {
        $store = [Security.Cryptography.X509Certificates.X509Store]::new(
            $storeName, [Security.Cryptography.X509Certificates.StoreLocation]::LocalMachine)
        try {
            $store.Open([Security.Cryptography.X509Certificates.OpenFlags]::ReadWrite)
            @($store.Certificates | Where-Object {
                [Convert]::ToBase64String($_.RawData) -ceq $expectedCertificateBytes
            }) | ForEach-Object { $store.Remove($_) }
        }
        finally {
            $store.Close()
        }
    }
    throw
}

foreach ($name in @('ViiperUde.inf', 'ViiperUde.sys', 'ViiperUde.cat')) {
    $runtime = Join-Path $driverDirectory $name
    $evidence = Join-Path $signedPackage $name
    if ((Get-FileHash -LiteralPath $runtime -Algorithm SHA256).Hash -cne
        (Get-FileHash -LiteralPath $evidence -Algorithm SHA256).Hash) {
        throw "Runtime driver file '$name' differs from its validated evidence copy."
    }
}

$manifestHash = (Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
$infHash = (Get-FileHash -LiteralPath (Join-Path $driverDirectory 'ViiperUde.inf') -Algorithm SHA256).Hash.ToLowerInvariant()
$sysHash = (Get-FileHash -LiteralPath (Join-Path $driverDirectory 'ViiperUde.sys') -Algorithm SHA256).Hash.ToLowerInvariant()
$catHash = (Get-FileHash -LiteralPath (Join-Path $driverDirectory 'ViiperUde.cat') -Algorithm SHA256).Hash.ToLowerInvariant()
$brokerHash = (Get-FileHash -LiteralPath $brokerPath -Algorithm SHA256).Hash.ToLowerInvariant()
$helperHash = (Get-FileHash -LiteralPath $helperPath -Algorithm SHA256).Hash.ToLowerInvariant()
$output = @(& $brokerPath native-package-install `
    --package-directory $driverDirectory --submission-manifest $manifestPath `
    --source-revision $source --driver-helper $helperPath `
    --expected-broker-sha256 $brokerHash --expected-helper-sha256 $helperHash `
    --expected-manifest-sha256 $manifestHash --expected-inf-sha256 $infHash `
    --expected-sys-sha256 $sysHash --expected-cat-sha256 $catHash `
    --target-user-sid $TargetUserSID --driver-validation-mode local-test 2>&1)
$exitCode = $LASTEXITCODE
$output | ForEach-Object { Write-Host $_ }
if ($exitCode -notin @(0, 3010)) {
    throw "Local VIIPER driver transaction failed with exit code $exitCode."
}
if ($exitCode -eq 3010) {
    Write-Warning 'The verified driver transaction requires a reboot. Restart before running live validation.'
    exit 3010
}

Write-Host 'The exact local test-signed VIIPER UdeCx driver and native broker are installed, authenticated, and ready.'
Write-Host 'Next: enable Driver Verifier for ViiperUde.sys, reboot, then run Invoke-ViiperUdeLiveValidation.ps1 in LocalTest mode.'

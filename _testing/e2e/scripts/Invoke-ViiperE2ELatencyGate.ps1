[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$SignedPackageDirectory,

    [Parameter(Mandatory = $true)]
    [string]$SubmissionManifestPath,

    [ValidateSet('Production', 'LocalTest')]
    [string]$PackageValidationMode = 'Production',

    [string]$LocalTestCertificatePath,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$')]
    [string]$ExpectedSourceRevision,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-fA-F]{64}$')]
    [string]$SDLBinarySHA256,

    [Parameter(Mandatory = $true)]
    [string]$OutputPath,

    [Parameter(Mandatory = $true)]
    [string]$WprTracePath,

    [ValidateRange(256, 10000)]
    [int]$Samples = 256,

    [Parameter(Mandatory = $true)]
    [ValidateSet('ABBA', 'BAAB')]
    [string]$Orientation,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-fA-F]{32}$')]
    [string]$CycleId,

    [Parameter(Mandatory = $true)]
    [ValidateRange(1, 100)]
    [int]$CycleIndex,

    [Parameter(Mandatory = $true)]
    [ValidateRange(2, 100)]
    [int]$CycleCount,

    [ValidateSet('Normal', 'High')]
    [string]$PriorityClass = 'Normal',

    [string]$RepositoryRoot,

    [Parameter(Mandatory = $true)]
    [string]$GitExecutable,

    [Parameter(Mandatory = $true)]
    [string]$GoExecutable
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Test-IsAdministrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Resolve-CanonicalPath {
    param([Parameter(Mandatory = $true)][string]$Path)

    return (Resolve-Path -LiteralPath $Path -ErrorAction Stop).Path
}

function Resolve-ExactExecutablePath {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Label
    )

    if (-not [IO.Path]::IsPathFullyQualified($Path)) {
        throw "$Label must be supplied as an absolute path; PATH lookup is forbidden."
    }
    $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or
        $item.Length -le 0) {
        throw "$Label is not a non-empty regular executable: '$Path'."
    }
    return $item.FullName
}

function Resolve-NewEvidencePath {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Repository,
        [Parameter(Mandatory = $true)][string]$Label
    )

    $full = [IO.Path]::GetFullPath($Path)
    if (Test-Path -LiteralPath $full) {
        throw "$Label already exists; refusing to overwrite source-bound evidence: '$full'."
    }
    $parent = Split-Path -Parent $full
    if (-not (Test-Path -LiteralPath $parent -PathType Container)) {
        throw "$Label parent directory must already exist: '$parent'."
    }
    $repoPrefix = $Repository.TrimEnd('\') + '\'
    if ($full.StartsWith($repoPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw "$Label must be outside the source checkout so the measured tree remains clean: '$full'."
    }
    return $full
}

function Resolve-DriverImagePath {
    param([Parameter(Mandatory = $true)][string]$ImagePath)

    $path = [Environment]::ExpandEnvironmentVariables($ImagePath.Trim().Trim('"'))
    if ($path.StartsWith('\??\', [StringComparison]::Ordinal)) {
        $path = $path.Substring(4)
    }
    if ($path.StartsWith('\SystemRoot\', [StringComparison]::OrdinalIgnoreCase)) {
        $path = Join-Path $env:SystemRoot $path.Substring('\SystemRoot\'.Length)
    }
    elseif ($path.StartsWith('System32\', [StringComparison]::OrdinalIgnoreCase)) {
        $path = Join-Path $env:SystemRoot $path
    }
    if (-not [IO.Path]::IsPathRooted($path)) {
        throw "VIIPER UDE has an unsupported relative service image path: '$ImagePath'."
    }
    return Resolve-CanonicalPath -Path $path
}

function Get-ExactUSBIPFileIdentity {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [switch]$RequireValidSignature
    )

    $item = Get-Item -LiteralPath (Resolve-CanonicalPath -Path $Path) -Force -ErrorAction Stop
    if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or
        $item.Length -le 0) {
        throw "USB/IP evidence is not a non-empty regular file: '$Path'."
    }
    $status = $null
    $subject = $null
    $thumbprint = $null
    if ($RequireValidSignature) {
        $signature = Get-AuthenticodeSignature -LiteralPath $item.FullName -ErrorAction Stop
        if ([string]$signature.Status -cne 'Valid' -or $null -eq $signature.SignerCertificate -or
            [string]$signature.SignerCertificate.Subject -notmatch '(?i)Microsoft') {
            throw "USB/IP evidence is not validly Microsoft-signed: '$($item.FullName)'."
        }
        $status = [string]$signature.Status
        $subject = [string]$signature.SignerCertificate.Subject
        $thumbprint = ([string]$signature.SignerCertificate.Thumbprint).ToLowerInvariant()
        if ($thumbprint -notmatch '^[0-9a-f]{40}$') {
            throw "USB/IP evidence has a noncanonical signer thumbprint: '$($item.FullName)'."
        }
    }
    return [ordered]@{
        path = $item.FullName
        length = [long]$item.Length
        sha256 = (Get-FileHash -LiteralPath $item.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
        file_version = [string]$item.VersionInfo.FileVersion
        product_version = [string]$item.VersionInfo.ProductVersion
        signature_status = $status
        signer_subject = $subject
        signer_thumbprint = $thumbprint
    }
}

function Get-ExactUSBIPRuntimeProvenance {
    $services = [Collections.Generic.List[object]]::new()
    foreach ($serviceName in @('usbip2_filter', 'usbip2_ude')) {
        $service = Get-ItemProperty -LiteralPath "HKLM:\SYSTEM\CurrentControlSet\Services\$serviceName" `
            -ErrorAction Stop
        if ([string]::IsNullOrWhiteSpace([string]$service.ImagePath) -or
            [string]$service.DisplayName -notmatch '^@(?<inf>oem[0-9]+\.inf),') {
            throw "USB/IP service '$serviceName' has no exact image/published-INF identity."
        }
        $publishedINFName = $Matches['inf'].ToLowerInvariant()
        $imagePath = Resolve-DriverImagePath -ImagePath ([string]$service.ImagePath)
        $packageDirectory = Split-Path -Parent $imagePath
        $infPath = Join-Path $packageDirectory "$serviceName.inf"
        $catalogPath = Join-Path $packageDirectory "$serviceName.cat"
        $publishedINFPath = Join-Path (Join-Path $env:SystemRoot 'INF') $publishedINFName
        $image = Get-ExactUSBIPFileIdentity -Path $imagePath -RequireValidSignature
        $inf = Get-ExactUSBIPFileIdentity -Path $infPath
        $publishedINF = Get-ExactUSBIPFileIdentity -Path $publishedINFPath
        $catalog = Get-ExactUSBIPFileIdentity -Path $catalogPath -RequireValidSignature
        if ([string]$inf.sha256 -cne [string]$publishedINF.sha256 -or
            [string]$image.signer_thumbprint -cne [string]$catalog.signer_thumbprint) {
            throw "USB/IP service '$serviceName' package bytes or signer identity disagree."
        }
        $services.Add([ordered]@{
            name = $serviceName
            start = [uint32]$service.Start
            type = [uint32]$service.Type
            published_inf_name = $publishedINFName
            image = $image
            inf = $inf
            published_inf = $publishedINF
            catalog = $catalog
        })
    }

    $udePublishedINF = [string]($services[1].published_inf_name)
    $rootEntities = @(Get-CimInstance -ClassName Win32_PnPEntity -ErrorAction Stop |
        Where-Object { @($_.HardwareID) -contains 'ROOT\USBIP_WIN2\UDE' } |
        Sort-Object PNPDeviceID)
    if ($rootEntities.Count -lt 1 -or $rootEntities.Count -gt 16) {
        throw "Expected 1-16 exact USB/IP root controllers; found $($rootEntities.Count)."
    }
    $signedDrivers = @(Get-CimInstance -ClassName Win32_PnPSignedDriver -ErrorAction Stop)
    $roots = [Collections.Generic.List[object]]::new()
    foreach ($root in $rootEntities) {
        $rootInstanceID = [string]($root.PNPDeviceID)
        $matches = @($signedDrivers | Where-Object {
            ([string]$_.DeviceID) -ieq $rootInstanceID
        })
        $signedDriver = $matches[0]
        if ($matches.Count -ne 1 -or -not [bool]($signedDriver.IsSigned) -or
            ([string]($signedDriver.Signer)) -notmatch '(?i)Microsoft' -or
            ([string]($signedDriver.DriverProviderName)) -ine 'USBIP-WIN2' -or
            ([string]($signedDriver.InfName)) -ine $udePublishedINF -or
            ([string]$root.Service) -ine 'usbip2_ude') {
            throw "USB/IP root '$rootInstanceID' lacks one exact signed package identity."
        }
        $roots.Add([ordered]@{
            instance_id = $rootInstanceID.ToUpperInvariant()
            hardware_ids = @(@($root.HardwareID | ForEach-Object { ([string]$_).ToUpperInvariant() }) |
                Sort-Object -Unique)
            service = [string]$root.Service
            provider = [string]($signedDriver.DriverProviderName)
            driver_version = [string]($signedDriver.DriverVersion)
            published_inf = ([string]($signedDriver.InfName)).ToLowerInvariant()
            signer = [string]($signedDriver.Signer)
            is_signed = [bool]($signedDriver.IsSigned)
        })
    }
    return [ordered]@{
        schema = 'viiper.usbip-win2.runtime-provenance/v1'
        services = @($services)
        root_controllers = @($roots)
    }
}

if ([string]::IsNullOrWhiteSpace($RepositoryRoot)) {
    $RepositoryRoot = Join-Path $PSScriptRoot '..\..\..'
}
if (-not (Test-IsAdministrator)) {
    throw 'The source-bound latency gate and WPR capture require an elevated PowerShell session.'
}
$repository = Resolve-CanonicalPath -Path $RepositoryRoot
$orientationValue = $Orientation.ToLowerInvariant()
$cycleIdValue = $CycleId.ToLowerInvariant()
if (($CycleCount % 2) -ne 0 -or $CycleIndex -gt $CycleCount) {
    throw 'CycleCount must be even and CycleIndex must identify one cycle in that balanced set.'
}
$expectedOrientation = if (($CycleIndex % 2) -eq 1) { 'abba' } else { 'baab' }
if ($orientationValue -cne $expectedOrientation) {
    throw "Cycle $CycleIndex requires orientation '$expectedOrientation', not '$orientationValue'."
}
$gitPath = Resolve-ExactExecutablePath -Path $GitExecutable -Label 'Git executable'
$git = [pscustomobject]@{ Source = $gitPath }
$gitHash = (Get-FileHash -LiteralPath $gitPath -Algorithm SHA256).Hash.ToLowerInvariant()
$headOutput = @(& $git.Source -C $repository rev-parse --verify HEAD 2>&1)
if ($LASTEXITCODE -ne 0 -or $headOutput.Count -eq 0) {
    throw "The production latency harness is not an exact Git checkout.`n$($headOutput -join [Environment]::NewLine)"
}
$headRevision = ([string]$headOutput[0]).Trim().ToLowerInvariant()
if (-not [string]::Equals($headRevision, $ExpectedSourceRevision,
        [StringComparison]::OrdinalIgnoreCase)) {
    throw "The production latency harness is source '$headRevision', not '$ExpectedSourceRevision'."
}
$treeStatus = @(& $git.Source -C $repository status --porcelain=v1 --untracked-files=all 2>&1)
if ($LASTEXITCODE -ne 0) {
    throw "Could not verify the production latency source tree.`n$($treeStatus -join [Environment]::NewLine)"
}
if ($treeStatus.Count -ne 0) {
    throw ("The production latency source tree is not clean; refusing unreviewed test code or data:`n" +
        ($treeStatus -join [Environment]::NewLine))
}
$submoduleStatus = @(& $git.Source -C $repository submodule status --recursive 2>&1)
if ($LASTEXITCODE -ne 0 -or @($submoduleStatus | Where-Object { $_ -match '^[\-+U]' }).Count -ne 0) {
    throw "The production latency source tree has an unbound submodule state.`n$($submoduleStatus -join [Environment]::NewLine)"
}
$sdlRoot = Resolve-CanonicalPath -Path (Join-Path $repository '_testing\e2e\deps\SDL')
$sdlRevisionOutput = @(& $git.Source -C $sdlRoot rev-parse --verify HEAD 2>&1)
if ($LASTEXITCODE -ne 0 -or $sdlRevisionOutput.Count -eq 0) {
    throw "Could not bind the SDL source revision.`n$($sdlRevisionOutput -join [Environment]::NewLine)"
}
$sdlRevision = ([string]$sdlRevisionOutput[0]).Trim().ToLowerInvariant()
$sdlDLL = Resolve-CanonicalPath -Path (Join-Path $sdlRoot 'build\Debug\SDL3.dll')
$actualSDLHash = (Get-FileHash -LiteralPath $sdlDLL -Algorithm SHA256).Hash.ToLowerInvariant()
if (-not [string]::Equals($actualSDLHash, $SDLBinarySHA256,
        [StringComparison]::OrdinalIgnoreCase)) {
    throw "The SDL binary hash is '$actualSDLHash', not the source-build hash '$SDLBinarySHA256'."
}

$signatureGate = Join-Path $repository 'native\udecx\tools\Test-ViiperUdeSignedPackage.ps1'
$manifest = Resolve-CanonicalPath -Path $SubmissionManifestPath
$manifestHashBeforeGate = (Get-FileHash -LiteralPath $manifest -Algorithm SHA256).Hash.ToLowerInvariant()
$packageModeValue = $PackageValidationMode.ToLowerInvariant().Replace('localtest', 'local-test')
$localTestCertificateHash = ''
$localTestCertificateThumbprint = ''
$signatureArguments = @{
    PackageDirectory = $SignedPackageDirectory
    SubmissionManifestPath = $manifest
    ExpectedSourceRevision = $ExpectedSourceRevision
    ValidationMode = $PackageValidationMode
}
if ($PackageValidationMode -eq 'LocalTest') {
    if ([string]::IsNullOrWhiteSpace($LocalTestCertificatePath)) {
        throw '-LocalTestCertificatePath is required with -PackageValidationMode LocalTest.'
    }
    $localTestCertificate = Resolve-CanonicalPath -Path $LocalTestCertificatePath
    $localTestCertificateItem = Get-Item -LiteralPath $localTestCertificate -Force -ErrorAction Stop
    if ($localTestCertificateItem.PSIsContainer -or
        ($localTestCertificateItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or
        $localTestCertificateItem.Length -le 0) {
        throw "The local-test certificate is not a non-empty regular file: '$localTestCertificate'."
    }
    $localTestCertificateHash = (Get-FileHash -LiteralPath $localTestCertificate -Algorithm SHA256).Hash.ToLowerInvariant()
    $certificate = [Security.Cryptography.X509Certificates.X509Certificate2]::new($localTestCertificate)
    try {
        $localTestCertificateThumbprint = ([string]$certificate.Thumbprint).ToLowerInvariant()
    }
    finally {
        $certificate.Dispose()
    }
    if ($localTestCertificateThumbprint -notmatch '^[0-9a-f]{40}$') {
        throw 'The local-test certificate has no canonical thumbprint.'
    }
    $signatureArguments.LocalTestCertificatePath = $localTestCertificate
}
elseif (-not [string]::IsNullOrWhiteSpace($LocalTestCertificatePath)) {
    throw '-LocalTestCertificatePath is valid only with -PackageValidationMode LocalTest.'
}
& $signatureGate @signatureArguments
$localTestCertificateArgument = if ([string]::IsNullOrEmpty($localTestCertificateHash)) {
    'none'
}
else {
    $localTestCertificateHash
}

$packageRoot = Resolve-CanonicalPath -Path $SignedPackageDirectory
$packageDriver = Resolve-CanonicalPath -Path (Join-Path $packageRoot 'ViiperUde.sys')
$service = Get-ItemProperty -LiteralPath 'HKLM:\SYSTEM\CurrentControlSet\Services\ViiperUde' -ErrorAction Stop
if ([string]::IsNullOrWhiteSpace([string]$service.ImagePath)) {
    throw 'The installed VIIPER UDE service has no ImagePath.'
}
$installedDriver = Resolve-DriverImagePath -ImagePath ([string]$service.ImagePath)
$packageDriverHash = (Get-FileHash -LiteralPath $packageDriver -Algorithm SHA256).Hash.ToLowerInvariant()
$installedDriverHash = (Get-FileHash -LiteralPath $installedDriver -Algorithm SHA256).Hash.ToLowerInvariant()
if ($packageDriverHash -ne $installedDriverHash) {
    throw "The installed VIIPER UDE service image does not match the verified package. Installed='$installedDriver'."
}
$installedSignature = Get-AuthenticodeSignature -LiteralPath $installedDriver -ErrorAction Stop
if ($PackageValidationMode -eq 'Production') {
    if ([string]$installedSignature.Status -cne 'Valid' -or
        $null -eq $installedSignature.SignerCertificate -or
        [string]$installedSignature.SignerCertificate.Subject -notmatch '(?i)Microsoft') {
        throw 'The installed VIIPER UDE image is not validly Microsoft-signed.'
    }
}
elseif ([string]$installedSignature.Status -cne 'Valid' -or
    $null -eq $installedSignature.SignerCertificate -or
    ([string]$installedSignature.SignerCertificate.Thumbprint).ToLowerInvariant() -cne $localTestCertificateThumbprint) {
    throw 'The installed VIIPER UDE image is not signed by the exact local-test certificate.'
}
$ownedRootDevices = @(Get-CimInstance -ClassName Win32_PnPEntity | Where-Object {
    @($_.HardwareID) -contains 'ROOT\VIIPER\UDE'
})
if ($ownedRootDevices.Count -ne 1) {
    throw "Expected exactly one VIIPER UDE hardware-ID owner; found $($ownedRootDevices.Count)."
}
$ownedRootInstance = [string]$ownedRootDevices[0].PNPDeviceID
$devnodes = @(Get-CimInstance -ClassName Win32_PnPSignedDriver | Where-Object {
    [string]$_.DeviceID -ieq $ownedRootInstance
})
if ($devnodes.Count -ne 1) {
    throw "Expected exactly one VIIPER UDE root devnode; found $($devnodes.Count)."
}
if (-not [bool]$devnodes[0].IsSigned -or [string]::IsNullOrWhiteSpace([string]$devnodes[0].Signer) -or
    ($PackageValidationMode -eq 'Production' -and [string]$devnodes[0].Signer -notmatch '(?i)Microsoft')) {
    throw "The installed VIIPER UDE devnode is not backed by the exact validated package (Signer='$($devnodes[0].Signer)')."
}
$manifestHash = (Get-FileHash -LiteralPath $manifest -Algorithm SHA256).Hash.ToLowerInvariant()
if ($manifestHash -ne $manifestHashBeforeGate) {
    throw 'The native submission manifest changed while its signature/package gate was running.'
}
$manifestDocument = Get-Content -LiteralPath $manifest -Raw -ErrorAction Stop | ConvertFrom-Json -ErrorAction Stop
$driverBuildIdentity = ([string]$manifestDocument.driverBuildIdentity).Trim().ToLowerInvariant()
if ($driverBuildIdentity -notmatch '^[0-9a-f]{64}$') {
    throw 'The verified submission manifest has no canonical native driver build identity.'
}
$usbipRuntime = Get-ExactUSBIPRuntimeProvenance
$usbipRuntimeJSON = ConvertTo-Json -InputObject $usbipRuntime -Depth 8 -Compress
$usbipRuntimeBytes = [Text.UTF8Encoding]::new($false).GetBytes($usbipRuntimeJSON)
$usbipHasher = [Security.Cryptography.SHA256]::Create()
try {
    $usbipRuntimeHash = -join @($usbipHasher.ComputeHash($usbipRuntimeBytes) |
        ForEach-Object { $_.ToString('x2') })
}
finally {
    $usbipHasher.Dispose()
}
$usbipRuntimeBase64 = [Convert]::ToBase64String($usbipRuntimeBytes)

$output = Resolve-NewEvidencePath -Path $OutputPath -Repository $repository -Label 'Latency JSON output'
$trace = Resolve-NewEvidencePath -Path $WprTracePath -Repository $repository -Label 'WPR trace output'
$markers = Resolve-NewEvidencePath -Path "$output.etl-markers.json" -Repository $repository -Label 'Decoded ETL marker output'
if ([string]::Equals($output, $trace, [StringComparison]::OrdinalIgnoreCase) -or
    [string]::Equals($output, $markers, [StringComparison]::OrdinalIgnoreCase) -or
    [string]::Equals($trace, $markers, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'The latency JSON, WPR trace, and decoded marker evidence must use three different paths.'
}
$goPath = Resolve-ExactExecutablePath -Path $GoExecutable -Label 'Go executable'
$go = [pscustomobject]@{ Source = $goPath }
$goHash = (Get-FileHash -LiteralPath $goPath -Algorithm SHA256).Hash.ToLowerInvariant()
$wprPath = Resolve-ExactExecutablePath `
    -Path (Join-Path ([Environment]::SystemDirectory) 'wpr.exe') `
    -Label 'System WPR executable'
$wpr = [pscustomobject]@{ Source = $wprPath }
$wprHash = (Get-FileHash -LiteralPath $wprPath -Algorithm SHA256).Hash.ToLowerInvariant()
$wprProfilePath = Resolve-CanonicalPath -Path (Join-Path $repository '_testing\e2e\latency\ViiperLatency.wprp')
$wprProfileHash = (Get-FileHash -LiteralPath $wprProfilePath -Algorithm SHA256).Hash.ToLowerInvariant()
$wprProfile = "$wprProfilePath!ViiperLatency"
$profileDetailsOutput = @(& $wpr.Source -profiledetails $wprProfile -filemode 2>&1)
if ($LASTEXITCODE -ne 0) {
    throw "WPR could not describe '$wprProfile'.`n$($profileDetailsOutput -join [Environment]::NewLine)"
}
$profileDetails = $profileDetailsOutput | Out-String
if ($profileDetails -notmatch '(?im)^Profile\s*:\s*ViiperLatency\.Verbose\.File\s*$') {
    throw "WPR '$wprProfile' is not the required source-controlled sequential-file profile.`n$profileDetails"
}
foreach ($eventName in @('DPC', 'Interrupt', 'WDFDPC', 'WDFInterrupt')) {
    if ([regex]::Matches($profileDetails, "(?im)^\s*$eventName\s*$").Count -lt 1) {
        throw "WPR '$wprProfile' does not capture the required $eventName evidence."
    }
}
foreach ($stackName in @('CSwitch', 'ReadyThread', 'SampledProfile')) {
    if ([regex]::Matches($profileDetails, "(?im)^\s*$stackName\s*$").Count -lt 2) {
        throw "WPR '$wprProfile' does not capture the required $stackName events and stacks."
    }
}

$environmentNames = @(
    'CGO_ENABLED', 'GOENV', 'GOFLAGS', 'GOTOOLCHAIN', 'GOWORK', 'PATH',
    'VIIPER_E2E_LIVE_LATENCY', 'VIIPER_E2E_PRODUCTION_PREFLIGHT',
    'VIIPER_E2E_LATENCY_OUTPUT', 'VIIPER_E2E_LATENCY_SAMPLES',
    'VIIPER_E2E_LATENCY_ORIENTATION', 'VIIPER_E2E_LATENCY_CYCLE_ID',
    'VIIPER_E2E_LATENCY_CYCLE_INDEX', 'VIIPER_E2E_LATENCY_CYCLE_COUNT',
    'VIIPER_E2E_USBIP_RUNTIME_PROVENANCE', 'VIIPER_E2E_USBIP_RUNTIME_PROVENANCE_SHA256',
    'VIIPER_E2E_EXPECTED_SOURCE_REVISION', 'VIIPER_E2E_SDL_SOURCE_REVISION',
    'VIIPER_E2E_SDL_DLL_PATH', 'VIIPER_E2E_SDL_DLL_SHA256',
    'VIIPER_E2E_PACKAGE_MANIFEST_SHA256', 'VIIPER_E2E_NATIVE_DRIVER_SHA256',
	'VIIPER_E2E_PACKAGE_VALIDATION_MODE', 'VIIPER_E2E_LOCAL_TEST_CERTIFICATE_SHA256',
    'VIIPER_E2E_TRACE_PROFILE_SHA256', 'VIIPER_E2E_NATIVE_DRIVER_BUILD_IDENTITY',
    'VIIPER_E2E_EXPECTED_PRIORITY_CLASS',
    'VIIPER_E2E_GIT_EXECUTABLE_PATH', 'VIIPER_E2E_GIT_EXECUTABLE_SHA256',
    'VIIPER_E2E_GO_EXECUTABLE_PATH', 'VIIPER_E2E_GO_EXECUTABLE_SHA256',
    'VIIPER_E2E_WPR_EXECUTABLE_PATH', 'VIIPER_E2E_WPR_EXECUTABLE_SHA256'
)
$savedEnvironment = @{}
foreach ($name in $environmentNames) {
    $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}

$wprInstance = "ViiperE2ELatency-$PID-$([guid]::NewGuid().ToString('N'))"
$nativeRevisionLDFlag = "-X github.com/Alia5/VIIPER/internal/transport/udecx.nativeSourceRevision=$headRevision"
$wprStarted = $false
$wprFailure = $null
$testExitCode = -1
$wrapperProcess = [Diagnostics.Process]::GetCurrentProcess()
$originalPriorityClass = $wrapperProcess.PriorityClass
try {
    $env:CGO_ENABLED = '1'
    $env:GOENV = 'off'
    $env:GOFLAGS = '-mod=readonly'
    $env:GOTOOLCHAIN = 'local'
    $env:GOWORK = 'off'
    $env:PATH = "$(Split-Path -Parent $sdlDLL);$($savedEnvironment['PATH'])"
    $env:VIIPER_E2E_LIVE_LATENCY = '1'
    $env:VIIPER_E2E_PRODUCTION_PREFLIGHT = '1'
    $env:VIIPER_E2E_LATENCY_OUTPUT = $output
    $env:VIIPER_E2E_LATENCY_SAMPLES = [string]$Samples
    $env:VIIPER_E2E_LATENCY_ORIENTATION = $orientationValue
    $env:VIIPER_E2E_LATENCY_CYCLE_ID = $cycleIdValue
    $env:VIIPER_E2E_LATENCY_CYCLE_INDEX = [string]$CycleIndex
    $env:VIIPER_E2E_LATENCY_CYCLE_COUNT = [string]$CycleCount
    $env:VIIPER_E2E_USBIP_RUNTIME_PROVENANCE = $usbipRuntimeBase64
    $env:VIIPER_E2E_USBIP_RUNTIME_PROVENANCE_SHA256 = $usbipRuntimeHash
    $env:VIIPER_E2E_EXPECTED_SOURCE_REVISION = $headRevision
    $env:VIIPER_E2E_SDL_SOURCE_REVISION = $sdlRevision
    $env:VIIPER_E2E_SDL_DLL_PATH = $sdlDLL
    $env:VIIPER_E2E_SDL_DLL_SHA256 = $actualSDLHash
    $env:VIIPER_E2E_PACKAGE_MANIFEST_SHA256 = $manifestHash
	$env:VIIPER_E2E_PACKAGE_VALIDATION_MODE = $packageModeValue
	$env:VIIPER_E2E_LOCAL_TEST_CERTIFICATE_SHA256 = $localTestCertificateHash
    $env:VIIPER_E2E_NATIVE_DRIVER_SHA256 = $installedDriverHash
    $env:VIIPER_E2E_TRACE_PROFILE_SHA256 = $wprProfileHash
    $env:VIIPER_E2E_NATIVE_DRIVER_BUILD_IDENTITY = $driverBuildIdentity
    $env:VIIPER_E2E_EXPECTED_PRIORITY_CLASS = $PriorityClass.ToLowerInvariant()
    $env:VIIPER_E2E_GIT_EXECUTABLE_PATH = $gitPath
    $env:VIIPER_E2E_GIT_EXECUTABLE_SHA256 = $gitHash
    $env:VIIPER_E2E_GO_EXECUTABLE_PATH = $goPath
    $env:VIIPER_E2E_GO_EXECUTABLE_SHA256 = $goHash
    $env:VIIPER_E2E_WPR_EXECUTABLE_PATH = $wprPath
    $env:VIIPER_E2E_WPR_EXECUTABLE_SHA256 = $wprHash

    $startOutput = @(& $wpr.Source -start $wprProfile -filemode -instancename $wprInstance 2>&1)
    if ($LASTEXITCODE -ne 0) {
        throw "Could not start the sequential-file WPR capture (exit $LASTEXITCODE).`n$($startOutput -join [Environment]::NewLine)"
    }
    $wprStarted = $true

    $wrapperProcess.PriorityClass = [Diagnostics.ProcessPriorityClass]::$PriorityClass
    & $go.Source -C $repository test -buildvcs=false -mod=readonly -count=1 -timeout=20m `
        -ldflags $nativeRevisionLDFlag `
        -run '^TestLiveControllerToGameLatencyGate$' -v ./_testing/e2e
    $testExitCode = $LASTEXITCODE
}
finally {
    try {
        $wrapperProcess.PriorityClass = $originalPriorityClass
    }
    catch {
        $priorityFailure = "Could not restore wrapper process priority to '$originalPriorityClass': $($_.Exception.Message)"
        if ($null -eq $wprFailure) {
            $wprFailure = $priorityFailure
        }
        else {
            $wprFailure = "$wprFailure $priorityFailure"
        }
    }
    if ($wprStarted) {
        $statusOutput = @(& $wpr.Source -status collectors -details -instancename $wprInstance 2>&1)
        $statusExitCode = $LASTEXITCODE
        $statusText = $statusOutput | Out-String
        if ($statusExitCode -ne 0) {
            $wprFailure = "WPR status failed with exit $statusExitCode. $($statusOutput -join ' ')"
        }
        else {
            $lossMatches = [regex]::Matches($statusText,
                '(?im)^\s*(?<name>(?:Dropped\s+Events?|Events?\s+Lost|Buffers?\s+Lost))\s*:\s*(?<count>\d+)\s*$')
            if ($lossMatches.Count -eq 0) {
                $wprFailure = "WPR did not report any event/buffer loss counters. $($statusOutput -join ' ')"
            }
            else {
                $nonZeroLoss = @($lossMatches | Where-Object { [uint64]$_.Groups['count'].Value -ne 0 })
                if ($nonZeroLoss.Count -ne 0) {
                    $wprFailure = "WPR reported event/buffer loss: $($nonZeroLoss.Value -join '; ')."
                }
            }
        }

        $stopOutput = @(& $wpr.Source -stop $trace -instancename $wprInstance 2>&1)
        $stopExitCode = $LASTEXITCODE
        if ($stopExitCode -ne 0) {
            $stopFailure = "WPR stop failed with exit $stopExitCode. $($stopOutput -join ' ')"
            if ($null -eq $wprFailure) {
                $wprFailure = $stopFailure
            }
            else {
                $wprFailure = "$wprFailure $stopFailure"
            }
        }
        elseif (-not (Test-Path -LiteralPath $trace -PathType Leaf) -or
            (Get-Item -LiteralPath $trace).Length -le 0) {
            $traceFailure = "WPR reported success without a non-empty trace '$trace'."
            if ($null -eq $wprFailure) {
                $wprFailure = $traceFailure
            }
            else {
                $wprFailure = "$wprFailure $traceFailure"
            }
        }
    }
    foreach ($name in $environmentNames) {
        [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process')
    }
}

if ($testExitCode -ne 0) {
    $wprSuffix = if ($null -eq $wprFailure) { '' } else { " WPR integrity also failed: $wprFailure" }
    throw "The live controller-to-game latency gate failed with exit code $testExitCode. Failure evidence, if emitted, is '$output'; WPR evidence is '$trace'.$wprSuffix"
}
if ($null -ne $wprFailure) {
    throw "The live workload passed, but WPR evidence failed closed: $wprFailure"
}
if (-not (Test-Path -LiteralPath $output -PathType Leaf)) {
    throw "The latency gate exited successfully without the required JSON artifact '$output'."
}
$report = Get-Content -LiteralPath $output -Raw -ErrorAction Stop | ConvertFrom-Json -ErrorAction Stop
if ([string]$report.schema -cne 'viiper.controller-to-game.latency-suite/v3' -or
    [string]$report.provenance.source_revision -cne $headRevision -or
    [string]$report.provenance.sdl_source_revision -cne $sdlRevision -or
    [string]$report.provenance.sdl_binary_sha256 -cne $actualSDLHash -or
    [string]$report.provenance.native_package_manifest_sha256 -cne $manifestHash -or
	[string]$report.provenance.native_package_validation_mode -cne $packageModeValue -or
	[string]$report.provenance.native_local_test_certificate_sha256 -cne $localTestCertificateHash -or
    [string]$report.provenance.native_driver_sha256 -cne $installedDriverHash -or
    [string]$report.provenance.native_driver_build_identity -cne $driverBuildIdentity -or
    [string]$report.provenance.usbip_runtime.capture_sha256 -cne $usbipRuntimeHash -or
    [string]$report.provenance.git_executable_path -cne $gitPath -or
    [string]$report.provenance.git_executable_sha256 -cne $gitHash -or
    [string]$report.provenance.go_executable_path -cne $goPath -or
    [string]$report.provenance.go_executable_sha256 -cne $goHash -or
    [string]$report.provenance.wpr_executable_path -cne $wprPath -or
    [string]$report.provenance.wpr_executable_sha256 -cne $wprHash -or
    [string]$report.provenance.machine.process_priority_class -cne $PriorityClass.ToLowerInvariant() -or
    [string]::IsNullOrWhiteSpace([string]$report.provenance.machine.hostname) -or
    [string]::IsNullOrWhiteSpace([string]$report.provenance.machine.os_version) -or
    [string]::IsNullOrWhiteSpace([string]$report.provenance.machine.cpu_model) -or
    [int]$report.provenance.machine.logical_processors -le 0 -or
    -not [bool]$report.provenance.machine.process_elevated -or
    [string]$report.verdict -cne 'pass' -or
    @($report.cases).Count -ne 3) {
    throw "The latency JSON artifact is not a passing source-bound production-controller suite."
}

$expectedMarkers = @{}
foreach ($case in @($report.cases)) {
    foreach ($run in @($case.runs)) {
        foreach ($sample in @($run.samples)) {
            $markerID = [string]$sample.trace_marker_id
            if ([string]::IsNullOrWhiteSpace($markerID) -or $expectedMarkers.ContainsKey($markerID)) {
                throw "The strictly parsed JSON contains an absent or duplicate trace marker '$markerID'."
            }
            $expectedMarkers[$markerID] = @{
                Controller = [string]$case.workload.controller_type
                Transport = [string]$run.transport
                TransportBlock = [string]$run.transport_block
                Sequence = [string]$sample.sequence
                Transition = [string]$sample.transition
                StartQPCTicks = [string]$sample.start_qpc_ticks
                EndQPCTicks = [string]$sample.end_qpc_ticks
                MarkerQPCTicks = [string]$sample.trace_marker_qpc_ticks
                LatencyNS = [string]$sample.latency_ns
                SDLEventTimestampNS = [string]$sample.sdl_event_timestamp_ns
                SDLFenceTimestampNS = [string]$sample.sdl_prewrite_fence_timestamp_ns
            }
        }
    }
}
$traceMarkers = @{}
$decodedMarkers = [Collections.Generic.List[object]]::new()
$requiredTraceFields = @(
    'MarkerID', 'Controller', 'Transport', 'TransportBlock', 'Sequence', 'Transition',
    'StartQPCTicks', 'EndQPCTicks', 'MarkerQPCTicks', 'LatencyNS',
    'SDLEventTimestampNS', 'SDLFenceTimestampNS'
)
try {
    $traceEvents = @(Get-WinEvent -FilterHashtable @{
        Path = $trace
        ProviderName = 'VIIPER-LatencyGate'
    } -Oldest -ErrorAction Stop)
}
catch {
    throw "The ETL could not be decoded for exact TraceLogging attribution: $($_.Exception.Message)"
}
foreach ($event in $traceEvents) {
    [xml]$xml = $event.ToXml()
    if (-not [string]::Equals([string]$xml.Event.System.Provider.Name,
            'VIIPER-LatencyGate', [StringComparison]::Ordinal) -or
        -not [string]::Equals([string]$xml.Event.System.Provider.Guid,
            '{e1726ef8-c2e6-4dad-bbf7-2d871b953ab1}', [StringComparison]::OrdinalIgnoreCase)) {
        throw 'A decoded latency event does not have the exact source-controlled provider name and GUID.'
    }
    $fields = @{}
    foreach ($data in @($xml.Event.EventData.Data)) {
        $name = [string]$data.Name
        if ([string]::IsNullOrWhiteSpace($name) -or $fields.ContainsKey($name)) {
            throw 'A latency ETL marker contains absent or duplicate named payload fields.'
        }
        $fields[$name] = [string]$data.InnerText
    }
    if ($fields.Count -ne $requiredTraceFields.Count -or
        @($requiredTraceFields | Where-Object { -not $fields.ContainsKey($_) }).Count -ne 0) {
        throw 'A latency ETL marker does not contain the exact source-controlled payload schema.'
    }
    $markerID = [string]$fields['MarkerID']
    if ([string]::IsNullOrWhiteSpace($markerID) -or $traceMarkers.ContainsKey($markerID)) {
        throw "The ETL contains an absent or duplicate latency marker '$markerID'."
    }
    if (-not $expectedMarkers.ContainsKey($markerID)) {
        throw "The ETL contains an unreported latency marker '$markerID'."
    }
    $expected = $expectedMarkers[$markerID]
    foreach ($fieldName in @('Controller', 'Transport', 'TransportBlock', 'Sequence', 'Transition',
            'StartQPCTicks', 'EndQPCTicks', 'MarkerQPCTicks', 'LatencyNS',
            'SDLEventTimestampNS', 'SDLFenceTimestampNS')) {
        if ([string]$fields[$fieldName] -cne [string]$expected[$fieldName]) {
            throw "ETL marker '$markerID' field '$fieldName' does not match its JSON sample."
        }
    }
    $traceMarkers[$markerID] = $true
    $decodedMarkers.Add([pscustomobject]@{
        trace_marker_id = $markerID
        controller = [string]$fields['Controller']
        transport = [string]$fields['Transport']
        transport_block = [int]$fields['TransportBlock']
        sequence = [int]$fields['Sequence']
        transition = [string]$fields['Transition']
        start_qpc_ticks = [long]$fields['StartQPCTicks']
        end_qpc_ticks = [long]$fields['EndQPCTicks']
        trace_marker_qpc_ticks = [long]$fields['MarkerQPCTicks']
        latency_ns = [long]$fields['LatencyNS']
        sdl_event_timestamp_ns = [uint64]$fields['SDLEventTimestampNS']
        sdl_prewrite_fence_timestamp_ns = [uint64]$fields['SDLFenceTimestampNS']
    })
}
if ($traceMarkers.Count -ne $expectedMarkers.Count) {
    $missingMarkers = @($expectedMarkers.Keys | Where-Object { -not $traceMarkers.ContainsKey($_) })
    throw "The ETL has $($traceMarkers.Count) exact sample markers for $($expectedMarkers.Count) JSON samples; missing: $($missingMarkers -join ', ')."
}
$traceItem = Get-Item -LiteralPath $trace -Force -ErrorAction Stop
if ($traceItem.PSIsContainer -or
    ($traceItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or
    $traceItem.Length -le 0) {
    throw "The raw WPR evidence is not a non-empty regular file: '$trace'."
}
$markerEnvelope = [ordered]@{
    schema = 'viiper.controller-to-game.latency-trace-markers/v1'
    source_trace_length = [long]$traceItem.Length
    source_trace_sha256 = (Get-FileHash -LiteralPath $traceItem.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
    markers = @($decodedMarkers)
}
$markerJSON = ConvertTo-Json -InputObject $markerEnvelope -Depth 4 -Compress
$markerBytes = [Text.UTF8Encoding]::new($false).GetBytes($markerJSON)
$markerStream = [IO.File]::Open($markers, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
try {
    $markerStream.Write($markerBytes, 0, $markerBytes.Length)
    $markerStream.Flush($true)
}
finally {
    $markerStream.Dispose()
}
$verifyExitCode = -1
try {
    $env:CGO_ENABLED = '0'
    $env:GOENV = 'off'
    $env:GOFLAGS = ''
    $env:GOTOOLCHAIN = 'local'
    $env:GOWORK = 'off'
    $verifyOutput = @(& $go.Source -C $repository run -buildvcs=false -mod=readonly `
        ./_testing/e2e/cmd/verifylatency `
        -input $output `
        -markers $markers `
		-trace $trace `
        -source $headRevision `
        -sdl-revision $sdlRevision `
        -sdl-sha256 $actualSDLHash `
        -manifest-sha256 $manifestHash `
		-package-validation-mode $packageModeValue `
		-local-test-certificate-sha256 $localTestCertificateArgument `
        -driver-sha256 $installedDriverHash `
        -driver-build-identity $driverBuildIdentity `
        -trace-profile-sha256 $wprProfileHash `
        -usbip-runtime-sha256 $usbipRuntimeHash `
        -orientation $orientationValue `
        -cycle-id $cycleIdValue `
        -cycle-index $CycleIndex `
        -cycle-count $CycleCount `
        -samples $Samples 2>&1)
    $verifyExitCode = $LASTEXITCODE
}
finally {
    foreach ($name in @('CGO_ENABLED', 'GOENV', 'GOFLAGS', 'GOTOOLCHAIN', 'GOWORK')) {
        [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process')
    }
}
if ($verifyExitCode -ne 0) {
    throw "The strict Go evidence verifier rejected the JSON/ETL evidence pair.`n$($verifyOutput -join [Environment]::NewLine)"
}
$requiredControllers = @('xbox360', 'dualshock4', 'dualsensegamepadv5')
$expectedTransports = if ($orientationValue -ceq 'abba') {
    @('usbip', 'native-ude', 'native-ude', 'usbip')
}
else {
    @('native-ude', 'usbip', 'usbip', 'native-ude')
}
for ($index = 0; $index -lt $requiredControllers.Count; $index++) {
    $case = $report.cases[$index]
    if ([string]$case.workload.controller_type -cne $requiredControllers[$index] -or
        [int]$case.workload.warmup_pairs -ne 16 -or
        [int]$case.workload.sample_pairs -ne $Samples -or
        [string]$case.workload.schedule_orientation -cne $orientationValue -or
        [string]$case.workload.cycle_id -cne $cycleIdValue -or
        [int]$case.workload.cycle_index -ne $CycleIndex -or
        [int]$case.workload.cycle_count -ne $CycleCount -or
        [long]$case.workload.inter_transition_delay_ns -ne 2000000 -or
        [string]$case.workload.phase_sweep_sha256 -cne '21eee9ea71984343ebd21221df8272553d6ab369a5740a1c796380cd468abcd9' -or
        @($case.runs).Count -ne 4 -or
        [string]$case.runs[0].transport -cne $expectedTransports[0] -or
        [string]$case.runs[1].transport -cne $expectedTransports[1] -or
        [string]$case.runs[2].transport -cne $expectedTransports[2] -or
        [string]$case.runs[3].transport -cne $expectedTransports[3] -or
        [int]$case.runs[0].order -ne 1 -or [int]$case.runs[0].transport_block -ne 1 -or
        [int]$case.runs[1].order -ne 2 -or [int]$case.runs[1].transport_block -ne 1 -or
        [int]$case.runs[2].order -ne 3 -or [int]$case.runs[2].transport_block -ne 2 -or
        [int]$case.runs[3].order -ne 4 -or [int]$case.runs[3].transport_block -ne 2 -or
        @($case.transports).Count -ne 2 -or
        [int]$case.transports[0].statistics.press.count -ne $Samples -or
        [int]$case.transports[0].statistics.release.count -ne $Samples -or
        [int]$case.transports[1].statistics.press.count -ne $Samples -or
        [int]$case.transports[1].statistics.release.count -ne $Samples) {
        throw "The latency JSON artifact is missing the counterbalanced '$($requiredControllers[$index])' workload."
    }
}
$postStatus = @(& $git.Source -C $repository status --porcelain=v1 --untracked-files=all 2>&1)
if ($LASTEXITCODE -ne 0 -or $postStatus.Count -ne 0) {
    throw ("The production latency run changed its source checkout:`n" +
        ($postStatus -join [Environment]::NewLine))
}

Write-Host "Validated source-bound controller-to-game latency evidence: '$output'."
Write-Host "Captured source-controlled sequential-file WPR evidence: '$trace'."
Write-Host "Retained the exactly decoded ETL marker evidence: '$markers'."

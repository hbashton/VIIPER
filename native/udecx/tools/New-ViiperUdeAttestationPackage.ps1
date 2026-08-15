[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$InfPath,

    [Parameter(Mandatory = $true)]
    [string]$SysPath,

    [Parameter(Mandatory = $true)]
    [string]$PdbPath,

    [Parameter(Mandatory = $true)]
    [string]$CatalogPath,

    [Parameter(Mandatory = $true)]
    [string]$OutputPath,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$')]
    [string]$SourceRevision,

    [switch]$AcknowledgeTestingOnly,

    [switch]$Force
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not $AcknowledgeTestingOnly) {
    throw 'Microsoft documents attestation signing as testing-only. Pass -AcknowledgeTestingOnly to create a controlled-test submission CAB; use HLK/WHCP for a VIIPER retail release.'
}

function Resolve-RequiredFile {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [string]$ExpectedExtension
    )

    $resolved = Resolve-Path -LiteralPath $Path -ErrorAction Stop
    $item = Get-Item -LiteralPath $resolved.Path -Force
    if (-not $item.PSIsContainer -and $item.Extension -ieq $ExpectedExtension -and $item.Length -gt 0) {
        return $item
    }
    throw "Expected a nonempty $ExpectedExtension file at '$Path'."
}

function Assert-InfContract {
    param([Parameter(Mandatory = $true)][string]$Path)

    $contents = Get-Content -LiteralPath $Path -Raw
    $required = @(
        '(?im)^\s*Class\s*=\s*USB\s*$',
        '(?im)^\s*ClassGuid\s*=\s*\{36FC9E60-C465-11CF-8056-444553540000\}\s*$',
        '(?im)^\s*CatalogFile\s*=\s*ViiperUde\.cat\s*$',
        '(?im)^\s*CopyFiles\s*=\s*@ViiperUde\.sys\s*$',
        '(?im)^\s*%DeviceName%\s*=\s*ViiperUde_Install\s*,\s*ROOT\\VIIPER\\UDE\s*$'
    )
    foreach ($pattern in $required) {
        if ($contents -notmatch $pattern) {
            throw "The INF does not satisfy the native VIIPER package contract: $pattern"
        }
    }
}

$inf = Resolve-RequiredFile -Path $InfPath -ExpectedExtension '.inf'
$sys = Resolve-RequiredFile -Path $SysPath -ExpectedExtension '.sys'
$pdb = Resolve-RequiredFile -Path $PdbPath -ExpectedExtension '.pdb'
$cat = Resolve-RequiredFile -Path $CatalogPath -ExpectedExtension '.cat'
Assert-InfContract -Path $inf.FullName

[xml]$driverProject = Get-Content -LiteralPath (Join-Path $PSScriptRoot '..\driver\ViiperUde.vcxproj') -Raw
$projectNamespace = [Xml.XmlNamespaceManager]::new($driverProject.NameTable)
$projectNamespace.AddNamespace('msb', 'http://schemas.microsoft.com/developer/msbuild/2003')
$versionNodes = @($driverProject.SelectNodes('//msb:ViiperUdeDriverVersion', $projectNamespace))
if ($versionNodes.Count -ne 1) {
    throw 'The driver project must declare one deterministic ViiperUdeDriverVersion.'
}
$driverPackageVersion = $versionNodes[0].InnerText.Trim()
$driverABIMajor = 1
$driverABIMinor = 13
$driverCapabilities = [uint32]29
$driverBuildIdentity = & (Join-Path $PSScriptRoot 'Get-ViiperUdeBuildIdentity.ps1') `
    -SourceRevision $SourceRevision `
    -DriverPackageVersion $driverPackageVersion `
    -ABIMajor $driverABIMajor `
    -ABIMinor $driverABIMinor `
    -Capabilities $driverCapabilities

$makeCab = Get-Command makecab.exe -ErrorAction Stop
$expand = Get-Command expand.exe -ErrorAction Stop
$outputFullPath = [System.IO.Path]::GetFullPath($OutputPath)
if ([System.IO.Path]::GetExtension($outputFullPath) -ine '.cab') {
    throw "The output path must end in .cab."
}
$outputDirectory = [System.IO.Path]::GetDirectoryName($outputFullPath)
if ([string]::IsNullOrWhiteSpace($outputDirectory)) {
    throw "The output path must include a directory."
}
New-Item -ItemType Directory -Path $outputDirectory -Force | Out-Null
if (Test-Path -LiteralPath $outputFullPath) {
    if (-not $Force) {
        throw "The output CAB already exists. Pass -Force to replace '$outputFullPath'."
    }
    Remove-Item -LiteralPath $outputFullPath -Force
}

$workRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("ViiperUdeCab" + [Guid]::NewGuid().ToString('N'))
$stage = Join-Path $workRoot 'stage'
$verify = Join-Path $workRoot 'verify'
$packageFolder = 'ViiperUde'
$cabName = [System.IO.Path]::GetFileName($outputFullPath)

try {
    New-Item -ItemType Directory -Path $stage, $verify -Force | Out-Null
    $sourceByName = [ordered]@{
        'ViiperUde.inf' = $inf.FullName
        'ViiperUde.sys' = $sys.FullName
        'ViiperUde.pdb' = $pdb.FullName
        'ViiperUde.cat' = $cat.FullName
    }
    foreach ($entry in $sourceByName.GetEnumerator()) {
        Copy-Item -LiteralPath $entry.Value -Destination (Join-Path $stage $entry.Key)
    }

    $infVerif = Get-Command infverif.exe -ErrorAction SilentlyContinue
    if ($null -ne $infVerif) {
        & $infVerif.Source /v (Join-Path $stage 'ViiperUde.inf')
        if ($LASTEXITCODE -ne 0) {
            throw "InfVerif rejected the staged VIIPER INF with exit code $LASTEXITCODE."
        }
    }

    $ddfPath = Join-Path $workRoot 'ViiperUde.ddf'
    $ddfLines = @(
        '.OPTION EXPLICIT',
        '.Set CabinetFileCountThreshold=0',
        '.Set FolderFileCountThreshold=0',
        '.Set FolderSizeThreshold=0',
        '.Set MaxCabinetSize=0',
        '.Set MaxDiskFileCount=0',
        '.Set MaxDiskSize=0',
        '.Set CompressionType=MSZIP',
        '.Set Cabinet=on',
        '.Set Compress=on',
        ".Set CabinetNameTemplate=$cabName",
        ".Set DiskDirectoryTemplate=$outputDirectory",
        ".Set DestinationDir=$packageFolder"
    )
    foreach ($name in $sourceByName.Keys) {
        $ddfLines += ('"{0}" "{1}"' -f (Join-Path $stage $name), $name)
    }
    Set-Content -LiteralPath $ddfPath -Value $ddfLines -Encoding ascii

    Push-Location -LiteralPath $workRoot
    try {
        & $makeCab.Source /V1 /F $ddfPath
    }
    finally {
        Pop-Location
    }
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $outputFullPath)) {
        throw "MakeCab failed to create '$outputFullPath' (exit code $LASTEXITCODE)."
    }

    & $expand.Source -R '-F:*' $outputFullPath $verify | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "Expand failed to verify '$outputFullPath' (exit code $LASTEXITCODE)."
    }
    foreach ($name in $sourceByName.Keys) {
        $expanded = @(Get-ChildItem -LiteralPath $verify -Recurse -File -Filter $name)
        if ($expanded.Count -ne 1) {
            throw "The CAB must contain exactly one '$name'; found $($expanded.Count)."
        }
        $expectedHash = (Get-FileHash -LiteralPath (Join-Path $stage $name) -Algorithm SHA256).Hash
        $actualHash = (Get-FileHash -LiteralPath $expanded[0].FullName -Algorithm SHA256).Hash
        if ($actualHash -ne $expectedHash) {
            throw "The expanded '$name' does not match the staged input."
        }
    }

    $manifest = [ordered]@{
        schema = 2
        purpose = 'Microsoft Hardware Dev Center controlled-test attestation submission; not a retail release package'
        releaseEligible = $false
        signingRoute = 'ControlledTestAttestation'
        requiredProductionRoute = 'HLK/WHCP dashboard signing'
        sourceRevision = $SourceRevision.ToLowerInvariant()
        driverPackageVersion = $driverPackageVersion
        driverABIMajor = $driverABIMajor
        driverABIMinor = $driverABIMinor
        driverCapabilities = ('0x{0:x8}' -f $driverCapabilities)
        driverBuildIdentity = $driverBuildIdentity
        cabinet = [System.IO.Path]::GetFileName($outputFullPath)
        cabinetSha256 = (Get-FileHash -LiteralPath $outputFullPath -Algorithm SHA256).Hash
        packageFolder = $packageFolder
        files = @(
            foreach ($name in $sourceByName.Keys) {
                $path = Join-Path $stage $name
                [ordered]@{
                    name = $name
                    length = (Get-Item -LiteralPath $path).Length
                    sha256 = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash
                }
            }
        )
    }
    $manifestPath = "$outputFullPath.sha256.json"
    [System.IO.File]::WriteAllText(
        $manifestPath,
        ($manifest | ConvertTo-Json -Depth 5),
        [System.Text.UTF8Encoding]::new($false))

    Write-Host "Created exact VIIPER controlled-test attestation package: $outputFullPath"
    Write-Host "Hash manifest: $manifestPath"
    Write-Warning 'This CAB and any attestation-signed result are testing-only under Microsoft current policy. Do not ship them to retail users. A release requires HLK/WHCP dashboard signing.'
}
finally {
    if (Test-Path -LiteralPath $workRoot) {
        Remove-Item -LiteralPath $workRoot -Recurse -Force
    }
}

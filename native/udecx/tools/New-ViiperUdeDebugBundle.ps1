[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$RepositoryRoot,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$')]
    [string]$SourceRevision,
    [Parameter(Mandatory = $true)][string]$DriverImagePath,
    [Parameter(Mandatory = $true)][string]$DriverPdbPath,
    [Parameter(Mandatory = $true)][string]$DriverMapPath,
    [Parameter(Mandatory = $true)][string]$BrokerPath,
    [Parameter(Mandatory = $true)][string]$BrokerBuildInfoPath,
    [Parameter(Mandatory = $true)][string]$BrokerBuildManifestPath,
    [Parameter(Mandatory = $true)][string]$HelperPath,
    [Parameter(Mandatory = $true)][string]$HelperPdbPath,
    [Parameter(Mandatory = $true)][string]$MediaProbePath,
    [Parameter(Mandatory = $true)][string]$MediaProbePdbPath,
    [Parameter(Mandatory = $true)][string]$InputProbePath,
    [Parameter(Mandatory = $true)][string]$InputProbePdbPath,
    [Parameter(Mandatory = $true)][string]$OutputDirectory
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Resolve-ExactFile {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$ExpectedName
    )

    $resolved = (Resolve-Path -LiteralPath $Path -ErrorAction Stop).Path
    $item = Get-Item -LiteralPath $resolved -Force -ErrorAction Stop
    if ($item.PSIsContainer -or $item.Name -cne $ExpectedName -or $item.Length -le 0 -or
            ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Expected a nonempty, case-exact, non-reparse '$ExpectedName' at '$Path'."
    }
    return $item
}

function Copy-DebugArtifact {
    param(
        [Parameter(Mandatory = $true)]$Source,
        [Parameter(Mandatory = $true)][string]$RelativePath,
        [Parameter(Mandatory = $true)][string]$Role,
        [Parameter(Mandatory = $true)][string]$Root
    )

    $destination = Join-Path $Root $RelativePath.Replace(
        '/', [IO.Path]::DirectorySeparatorChar)
    [void][IO.Directory]::CreateDirectory([IO.Path]::GetDirectoryName($destination))
    [IO.File]::Copy($Source.FullName, $destination, $false)
    $item = Get-Item -LiteralPath $destination -Force
    return [ordered]@{
        path = $RelativePath
        role = $Role
        length = $item.Length
        sha256 = (Get-FileHash -LiteralPath $item.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
    }
}

$root = (Resolve-Path -LiteralPath $RepositoryRoot -ErrorAction Stop).Path
$rootItem = Get-Item -LiteralPath $root -Force
if (-not $rootItem.PSIsContainer) {
    throw "Repository root is not a directory: '$RepositoryRoot'."
}
$git = Get-Command git.exe -CommandType Application -ErrorAction Stop |
    Select-Object -First 1
$source = $SourceRevision.ToLowerInvariant()
$head = (& $git.Source -C $root rev-parse HEAD 2>&1 | Out-String).Trim().ToLowerInvariant()
if ($LASTEXITCODE -ne 0 -or $head -cne $source) {
    throw "Debug source revision '$source' does not match repository HEAD '$head'."
}
$trackedStatus = @(& $git.Source -C $root status --porcelain=v1 --untracked-files=no 2>&1)
if ($LASTEXITCODE -ne 0 -or $trackedStatus.Count -ne 0) {
    throw "Refusing a debug bundle from a modified tracked source tree.`n$($trackedStatus -join [Environment]::NewLine)"
}

$inputs = [ordered]@{
    'driver-image' = Resolve-ExactFile $DriverImagePath 'ViiperUde.sys'
    'driver-pdb' = Resolve-ExactFile $DriverPdbPath 'ViiperUde.pdb'
    'driver-map' = Resolve-ExactFile $DriverMapPath 'ViiperUde.map'
    'broker-image' = Resolve-ExactFile $BrokerPath 'viiper.exe'
    'broker-build-info' = Resolve-ExactFile $BrokerBuildInfoPath 'viiper.exe.buildinfo.txt'
    'broker-build-manifest' = Resolve-ExactFile $BrokerBuildManifestPath 'viiper.exe.build.json'
    'helper-image' = Resolve-ExactFile $HelperPath 'ViiperUdeCtl.exe'
    'helper-pdb' = Resolve-ExactFile $HelperPdbPath 'ViiperUdeCtl.pdb'
    'media-probe-image' = Resolve-ExactFile $MediaProbePath 'ViiperUdeMediaProbe.exe'
    'media-probe-pdb' = Resolve-ExactFile $MediaProbePdbPath 'ViiperUdeMediaProbe.pdb'
    'input-probe-image' = Resolve-ExactFile $InputProbePath 'ViiperUdeInputProbe.exe'
    'input-probe-pdb' = Resolve-ExactFile $InputProbePdbPath 'ViiperUdeInputProbe.pdb'
}

try {
    $brokerBuildManifest = Get-Content -LiteralPath `
        $inputs['broker-build-manifest'].FullName -Raw -ErrorAction Stop |
        ConvertFrom-Json -ErrorAction Stop
}
catch {
    throw "Broker build manifest is not valid JSON. $($_.Exception.Message)"
}
$brokerHash = (Get-FileHash -LiteralPath $inputs['broker-image'].FullName `
    -Algorithm SHA256).Hash.ToLowerInvariant()
$buildInfoHash = (Get-FileHash -LiteralPath $inputs['broker-build-info'].FullName `
    -Algorithm SHA256).Hash.ToLowerInvariant()
$buildInfoText = Get-Content -LiteralPath $inputs['broker-build-info'].FullName -Raw
$declaredDwarfSections = @($brokerBuildManifest.embeddedDwarfSections |
    ForEach-Object { [string]$_ })
if ([int]$brokerBuildManifest.schema -ne 1 -or
        [string]$brokerBuildManifest.sourceRevision -cne $source -or
        [string]$brokerBuildManifest.commit -cne $source -or
        [string]::IsNullOrWhiteSpace([string]$brokerBuildManifest.version) -or
        [string]::IsNullOrWhiteSpace([string]$brokerBuildManifest.buildDate) -or
        [string]::IsNullOrWhiteSpace([string]$brokerBuildManifest.goVersion) -or
        -not [bool]$brokerBuildManifest.trimpath -or
        -not [bool]$brokerBuildManifest.embeddedDwarf -or
        @('debug_info', 'debug_line', 'debug_abbrev' |
            Where-Object { $declaredDwarfSections -cnotcontains $_ }).Count -ne 0 -or
        [string]$brokerBuildManifest.binary.name -cne 'viiper.exe' -or
        [long]$brokerBuildManifest.binary.length -ne $inputs['broker-image'].Length -or
        [string]$brokerBuildManifest.binary.sha256 -cne $brokerHash -or
        [string]$brokerBuildManifest.buildInfoSha256 -cne $buildInfoHash -or
        $buildInfoText -notmatch ('(?m)^\s*build\s+vcs\.revision=' +
            [regex]::Escape($source) + '\s*$')) {
    throw 'Broker image, embedded-DWARF policy, build metadata, and source revision are not an exact set.'
}

$output = [IO.Path]::GetFullPath($OutputDirectory)
if (Test-Path -LiteralPath $output) {
    throw "Refusing to overwrite debug bundle '$output'."
}
[void][IO.Directory]::CreateDirectory($output)

$files = [Collections.Generic.List[object]]::new()
try {
    $layout = @(
        @('driver-image', 'binaries/ViiperUde.sys', 'driver-image'),
        @('broker-image', 'binaries/viiper.exe', 'broker-image-with-embedded-go-dwarf'),
        @('helper-image', 'binaries/ViiperUdeCtl.exe', 'helper-image'),
        @('media-probe-image', 'binaries/ViiperUdeMediaProbe.exe', 'media-probe-image'),
        @('input-probe-image', 'binaries/ViiperUdeInputProbe.exe', 'input-probe-image'),
        @('driver-pdb', 'symbols/ViiperUde.pdb', 'driver-private-pdb'),
        @('driver-map', 'symbols/ViiperUde.map', 'driver-link-map'),
        @('helper-pdb', 'symbols/ViiperUdeCtl.pdb', 'helper-private-pdb'),
        @('media-probe-pdb', 'symbols/ViiperUdeMediaProbe.pdb', 'media-probe-private-pdb'),
        @('input-probe-pdb', 'symbols/ViiperUdeInputProbe.pdb', 'input-probe-private-pdb'),
        @('broker-build-info', 'symbols/viiper.exe.buildinfo.txt', 'broker-go-build-info'),
        @('broker-build-manifest', 'symbols/viiper.exe.build.json', 'broker-build-manifest')
    )
    foreach ($entry in $layout) {
        [void]$files.Add((Copy-DebugArtifact -Source $inputs[$entry[0]] `
            -RelativePath $entry[1] -Role $entry[2] -Root $output))
    }

    $archiveName = "VIIPER-source-$source.zip"
    $archiveRelative = "source/$archiveName"
    $archivePath = Join-Path $output $archiveRelative.Replace(
        '/', [IO.Path]::DirectorySeparatorChar)
    [void][IO.Directory]::CreateDirectory([IO.Path]::GetDirectoryName($archivePath))
    & $git.Source -C $root archive --format=zip "--prefix=VIIPER-$source/" `
        "--output=$archivePath" $source
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $archivePath -PathType Leaf)) {
        throw "git archive failed for exact source revision '$source'."
    }
    $archive = Get-Item -LiteralPath $archivePath -Force
    if ($archive.Length -le 0) {
        throw 'The exact source archive is empty.'
    }
    [void]$files.Add([ordered]@{
        path = $archiveRelative
        role = 'exact-git-source-archive'
        length = $archive.Length
        sha256 = (Get-FileHash -LiteralPath $archive.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
    })

    $manifest = [ordered]@{
        schema = 1
        sourceRevision = $source
        sourceArchive = $archiveRelative
        sourceArchiveFormat = 'git-archive-zip'
        symbolPolicy = 'private-source-line-type-sidecars; embedded Go DWARF'
        pairs = @(
            [ordered]@{ image = 'binaries/ViiperUde.sys'; symbols = 'symbols/ViiperUde.pdb'; map = 'symbols/ViiperUde.map' },
            [ordered]@{ image = 'binaries/ViiperUdeCtl.exe'; symbols = 'symbols/ViiperUdeCtl.pdb' },
            [ordered]@{ image = 'binaries/ViiperUdeMediaProbe.exe'; symbols = 'symbols/ViiperUdeMediaProbe.pdb' },
            [ordered]@{ image = 'binaries/ViiperUdeInputProbe.exe'; symbols = 'symbols/ViiperUdeInputProbe.pdb' },
            [ordered]@{ image = 'binaries/viiper.exe'; symbols = 'embedded-go-dwarf'; buildInfo = 'symbols/viiper.exe.buildinfo.txt'; buildManifest = 'symbols/viiper.exe.build.json' }
        )
        files = @($files)
    }
    $manifestPath = Join-Path $output 'ViiperUdeDebug.manifest.json'
    [IO.File]::WriteAllText($manifestPath,
        ($manifest | ConvertTo-Json -Depth 7 -Compress),
        [Text.UTF8Encoding]::new($false))

    $roundTrip = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    if ([int]$roundTrip.schema -ne 1 -or
            [string]$roundTrip.sourceRevision -cne $source -or
            @($roundTrip.files).Count -ne $files.Count) {
        throw 'The emitted debug bundle manifest did not round-trip exactly.'
    }
    foreach ($entry in @($roundTrip.files)) {
        $path = Join-Path $output ([string]$entry.path).Replace(
            '/', [IO.Path]::DirectorySeparatorChar)
        $item = Get-Item -LiteralPath $path -Force -ErrorAction Stop
        if ($item.PSIsContainer -or $item.Length -ne [long]$entry.length -or
                (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant() -cne
                    [string]$entry.sha256) {
            throw "Debug bundle verification failed for '$($entry.path)'."
        }
    }
}
catch {
    if (Test-Path -LiteralPath $output) {
        Remove-Item -LiteralPath $output -Recurse -Force
    }
    throw
}

Write-Host "Created exact source-bound debug bundle at '$output'."

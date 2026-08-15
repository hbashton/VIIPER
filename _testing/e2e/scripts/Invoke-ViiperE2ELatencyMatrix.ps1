[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$SignedPackageDirectory,

    [Parameter(Mandatory = $true)]
    [string]$SubmissionManifestPath,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$')]
    [string]$ExpectedSourceRevision,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-fA-F]{64}$')]
    [string]$SDLBinarySHA256,

    [Parameter(Mandatory = $true)]
    [string]$EvidenceDirectory,

    [ValidateRange(256, 10000)]
    [int]$Samples = 10000,

    [string]$RepositoryRoot,

    [Parameter(Mandatory = $true)]
    [string]$GitExecutable,

    [Parameter(Mandatory = $true)]
    [string]$GoExecutable
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-ExactEvidenceFile {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Label
    )

    $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or
        $item.Length -le 0) {
        throw "$Label is not a non-empty regular file: '$Path'."
    }
    return [pscustomobject]@{
        path = $item.FullName
        length = [long]$item.Length
        sha256 = (Get-FileHash -LiteralPath $item.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
    }
}

$matrixRoot = [IO.Path]::GetFullPath($EvidenceDirectory)
$matrixRootItem = Get-Item -LiteralPath $matrixRoot -Force -ErrorAction Stop
if (-not $matrixRootItem.PSIsContainer -or
    ($matrixRootItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
    throw "EvidenceDirectory must be an existing non-reparse directory: '$matrixRoot'."
}

$gate = Join-Path $PSScriptRoot 'Invoke-ViiperE2ELatencyGate.ps1'
$gate = (Resolve-Path -LiteralPath $gate -ErrorAction Stop).Path
$matrixPath = Join-Path $matrixRoot 'viiper-latency-priority-matrix.json'
$runs = @(
    [pscustomobject]@{
        priority = 'Normal'
        report = (Join-Path $matrixRoot 'viiper-latency-normal.json')
        trace = (Join-Path $matrixRoot 'viiper-latency-normal.etl')
    },
    [pscustomobject]@{
        priority = 'High'
        report = (Join-Path $matrixRoot 'viiper-latency-high.json')
        trace = (Join-Path $matrixRoot 'viiper-latency-high.etl')
    }
)

$allOutputs = [Collections.Generic.List[string]]::new()
$allOutputs.Add($matrixPath)
foreach ($run in $runs) {
    $allOutputs.Add([string]$run.report)
    $allOutputs.Add([string]$run.trace)
    $allOutputs.Add("$($run.report).etl-markers.json")
}
foreach ($path in $allOutputs) {
    if (Test-Path -LiteralPath $path) {
        throw "Refusing to overwrite latency-matrix evidence '$path'."
    }
}

$common = @{
    SignedPackageDirectory = $SignedPackageDirectory
    SubmissionManifestPath = $SubmissionManifestPath
    ExpectedSourceRevision = $ExpectedSourceRevision
    SDLBinarySHA256 = $SDLBinarySHA256
    Samples = $Samples
    GitExecutable = $GitExecutable
    GoExecutable = $GoExecutable
}
if (-not [string]::IsNullOrWhiteSpace($RepositoryRoot)) {
    $common.RepositoryRoot = $RepositoryRoot
}

foreach ($run in $runs) {
    & $gate @common `
        -OutputPath $run.report `
        -WprTracePath $run.trace `
        -PriorityClass $run.priority
}

$matrixRuns = [Collections.Generic.List[object]]::new()
$referenceProvenance = $null
foreach ($run in $runs) {
    $reportFile = Get-ExactEvidenceFile -Path $run.report -Label "$($run.priority) report"
    $traceFile = Get-ExactEvidenceFile -Path $run.trace -Label "$($run.priority) trace"
    $markerFile = Get-ExactEvidenceFile -Path "$($run.report).etl-markers.json" -Label "$($run.priority) markers"
    $report = Get-Content -LiteralPath $run.report -Raw -ErrorAction Stop | ConvertFrom-Json -ErrorAction Stop
    $expectedPriority = ([string]$run.priority).ToLowerInvariant()
    if ([string]$report.schema -cne 'viiper.controller-to-game.latency-suite/v2' -or
        [string]$report.verdict -cne 'pass' -or
        [string]$report.provenance.source_revision -cne $ExpectedSourceRevision.ToLowerInvariant() -or
        [string]$report.provenance.machine.process_priority_class -cne $expectedPriority -or
        @($report.cases).Count -ne 3) {
        throw "$($run.priority) report is not an exact passing priority-bound suite."
    }

    $machine = $report.provenance.machine
    $provenanceIdentity = @(
        [string]$report.provenance.source_revision,
        [string]$report.provenance.sdl_source_revision,
        [string]$report.provenance.sdl_binary_path,
        [string]$report.provenance.sdl_binary_sha256,
        [string]$report.provenance.native_package_manifest_sha256,
        [string]$report.provenance.native_driver_sha256,
        [string]$report.provenance.native_driver_build_identity,
        [string]$report.provenance.qpc_frequency,
        [string]$report.provenance.trace_provider_name,
        [string]$report.provenance.trace_provider_guid,
        [string]$report.provenance.trace_profile_sha256,
        [string]$report.provenance.usbip_baseline_mode,
        [string]$report.provenance.usbip_baseline_version,
        [string]$report.provenance.go_version,
        [string]$report.provenance.goos,
        [string]$report.provenance.goarch,
        [string]$report.provenance.git_executable_path,
        [string]$report.provenance.git_executable_sha256,
        [string]$report.provenance.go_executable_path,
        [string]$report.provenance.go_executable_sha256,
        [string]$report.provenance.wpr_executable_path,
        [string]$report.provenance.wpr_executable_sha256,
        [string]$machine.hostname,
        [string]$machine.os_product_name,
        [string]$machine.os_display_version,
        [string]$machine.os_version,
        [string]$machine.cpu_model,
        [string]$machine.logical_processors,
        [string]$machine.process_elevated
    ) -join "`n"
    if ($null -eq $referenceProvenance) {
        $referenceProvenance = $provenanceIdentity
    }
    elseif (-not [string]::Equals($referenceProvenance, $provenanceIdentity,
            [StringComparison]::Ordinal)) {
        throw 'Normal and high-priority suites do not have identical source/package/toolchain/machine provenance.'
    }

    $matrixRuns.Add([ordered]@{
        priority_class = $expectedPriority
        report = $reportFile
        trace = $traceFile
        decoded_markers = $markerFile
    })
}

$matrix = [ordered]@{
    schema = 'viiper.controller-to-game.latency-priority-matrix/v1'
    generated_at = [DateTime]::UtcNow.ToString('o')
    source_revision = $ExpectedSourceRevision.ToLowerInvariant()
    sample_pairs_per_transition = $Samples
    runs = @($matrixRuns)
}
$matrixJSON = ConvertTo-Json -InputObject $matrix -Depth 8 -Compress
$matrixBytes = [Text.UTF8Encoding]::new($false).GetBytes($matrixJSON)
$stream = [IO.File]::Open($matrixPath, [IO.FileMode]::CreateNew,
    [IO.FileAccess]::Write, [IO.FileShare]::None)
try {
    $stream.Write($matrixBytes, 0, $matrixBytes.Length)
    $stream.Flush($true)
}
finally {
    $stream.Dispose()
}

$matrixFile = Get-ExactEvidenceFile -Path $matrixPath -Label 'priority matrix manifest'
Write-Host "Validated normal/high-priority latency matrix: '$($matrixFile.path)' (SHA-256 $($matrixFile.sha256))."

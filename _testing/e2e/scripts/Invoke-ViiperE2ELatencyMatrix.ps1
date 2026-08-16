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
    [string]$EvidenceDirectory,

    [ValidateRange(256, 10000)]
    [int]$Samples = 10000,

    [ValidateRange(6, 20)]
    [int]$CyclesPerPriority = 8,

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
$superiorityPath = Join-Path $matrixRoot 'viiper-latency-superiority.json'
if (($CyclesPerPriority % 2) -ne 0) {
    throw 'CyclesPerPriority must be even so each priority has equal ABBA and BAAB cycles.'
}
if ($PackageValidationMode -eq 'LocalTest' -and
    [string]::IsNullOrWhiteSpace($LocalTestCertificatePath)) {
    throw '-LocalTestCertificatePath is required with -PackageValidationMode LocalTest.'
}
if ($PackageValidationMode -eq 'Production' -and
    -not [string]::IsNullOrWhiteSpace($LocalTestCertificatePath)) {
    throw '-LocalTestCertificatePath is valid only with -PackageValidationMode LocalTest.'
}
$cycleBytes = [byte[]]::new(16)
$random = [Security.Cryptography.RandomNumberGenerator]::Create()
try {
    $random.GetBytes($cycleBytes)
}
finally {
    $random.Dispose()
}
$cycleId = -join @($cycleBytes | ForEach-Object { $_.ToString('x2') })
$cycleCount = 2 * $CyclesPerPriority
$runs = [Collections.Generic.List[object]]::new()
$cycleIndex = 0
foreach ($priority in @('Normal', 'High')) {
    for ($priorityCycle = 1; $priorityCycle -le $CyclesPerPriority; $priorityCycle++) {
        $cycleIndex++
        $orientation = if (($cycleIndex % 2) -eq 1) { 'ABBA' } else { 'BAAB' }
        $stem = "viiper-latency-$($priority.ToLowerInvariant())-cycle-$($priorityCycle.ToString('00'))"
        $runs.Add([pscustomobject]@{
            priority = $priority
            priority_cycle = $priorityCycle
            cycle_index = $cycleIndex
            orientation = $orientation
            report = (Join-Path $matrixRoot "$stem.json")
            trace = (Join-Path $matrixRoot "$stem.etl")
        })
    }
}

$allOutputs = [Collections.Generic.List[string]]::new()
$allOutputs.Add($matrixPath)
$allOutputs.Add($superiorityPath)
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
	PackageValidationMode = $PackageValidationMode
    ExpectedSourceRevision = $ExpectedSourceRevision
    SDLBinarySHA256 = $SDLBinarySHA256
    Samples = $Samples
    GitExecutable = $GitExecutable
    GoExecutable = $GoExecutable
}
if (-not [string]::IsNullOrWhiteSpace($RepositoryRoot)) {
    $common.RepositoryRoot = $RepositoryRoot
}
if (-not [string]::IsNullOrWhiteSpace($LocalTestCertificatePath)) {
    $common.LocalTestCertificatePath = $LocalTestCertificatePath
}

foreach ($run in $runs) {
    & $gate @common `
        -OutputPath $run.report `
        -WprTracePath $run.trace `
		-PriorityClass $run.priority `
		-Orientation $run.orientation `
		-CycleId $cycleId `
		-CycleIndex $run.cycle_index `
		-CycleCount $cycleCount
}

$matrixRuns = [Collections.Generic.List[object]]::new()
$referenceProvenance = $null
$matrixPackageIdentity = $null
foreach ($run in $runs) {
    $reportFile = Get-ExactEvidenceFile -Path $run.report -Label "$($run.priority) report"
    $traceFile = Get-ExactEvidenceFile -Path $run.trace -Label "$($run.priority) trace"
    $markerFile = Get-ExactEvidenceFile -Path "$($run.report).etl-markers.json" -Label "$($run.priority) markers"
    $report = Get-Content -LiteralPath $run.report -Raw -ErrorAction Stop | ConvertFrom-Json -ErrorAction Stop
    $expectedPriority = ([string]$run.priority).ToLowerInvariant()
    if ([string]$report.schema -cne 'viiper.controller-to-game.latency-suite/v3' -or
        [string]$report.verdict -cne 'pass' -or
        [string]$report.provenance.source_revision -cne $ExpectedSourceRevision.ToLowerInvariant() -or
        [string]$report.provenance.machine.process_priority_class -cne $expectedPriority -or
		[string]$report.cases[0].workload.schedule_orientation -cne $run.orientation.ToLowerInvariant() -or
		[string]$report.cases[0].workload.cycle_id -cne $cycleId -or
		[int]$report.cases[0].workload.cycle_index -ne [int]$run.cycle_index -or
		[int]$report.cases[0].workload.cycle_count -ne $cycleCount -or
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
		[string]$report.provenance.native_package_validation_mode,
		[string]$report.provenance.native_local_test_certificate_sha256,
        [string]$report.provenance.native_driver_sha256,
        [string]$report.provenance.native_driver_build_identity,
        [string]$report.provenance.qpc_frequency,
        [string]$report.provenance.trace_provider_name,
        [string]$report.provenance.trace_provider_guid,
        [string]$report.provenance.trace_profile_sha256,
        [string]$report.provenance.usbip_baseline_mode,
        [string]$report.provenance.usbip_baseline_version,
        [string]$report.provenance.usbip_runtime.capture_sha256,
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
		$matrixPackageIdentity = [ordered]@{
			native_package_manifest_sha256 = [string]$report.provenance.native_package_manifest_sha256
			native_package_validation_mode = [string]$report.provenance.native_package_validation_mode
			native_local_test_certificate_sha256 = [string]$report.provenance.native_local_test_certificate_sha256
			native_driver_sha256 = [string]$report.provenance.native_driver_sha256
			native_driver_build_identity = [string]$report.provenance.native_driver_build_identity
		}
    }
    elseif (-not [string]::Equals($referenceProvenance, $provenanceIdentity,
            [StringComparison]::Ordinal)) {
        throw 'Normal and high-priority suites do not have identical source/package/toolchain/machine provenance.'
    }

    $matrixRuns.Add([ordered]@{
        priority_class = $expectedPriority
		priority_cycle = [int]$run.priority_cycle
		cycle_index = [int]$run.cycle_index
		orientation = $run.orientation.ToLowerInvariant()
        report = $reportFile
        trace = $traceFile
        decoded_markers = $markerFile
    })
}

$matrix = [ordered]@{
    schema = 'viiper.controller-to-game.latency-priority-matrix/v2'
    generated_at = [DateTime]::UtcNow.ToString('o')
    source_revision = $ExpectedSourceRevision.ToLowerInvariant()
	native_package_manifest_sha256 = $matrixPackageIdentity.native_package_manifest_sha256
	native_package_validation_mode = $matrixPackageIdentity.native_package_validation_mode
	native_local_test_certificate_sha256 = $matrixPackageIdentity.native_local_test_certificate_sha256
	native_driver_sha256 = $matrixPackageIdentity.native_driver_sha256
	native_driver_build_identity = $matrixPackageIdentity.native_driver_build_identity
	cycle_id = $cycleId
	cycle_count = $cycleCount
	cycles_per_priority = $CyclesPerPriority
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
$repository = if ([string]::IsNullOrWhiteSpace($RepositoryRoot)) {
    (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..\..\..') -ErrorAction Stop).Path
}
else {
    (Resolve-Path -LiteralPath $RepositoryRoot -ErrorAction Stop).Path
}
$goPath = (Resolve-Path -LiteralPath $GoExecutable -ErrorAction Stop).Path
$expectedSource = $ExpectedSourceRevision.ToLowerInvariant()
$analyzerEnvironment = @{}
foreach ($name in @('CGO_ENABLED', 'GOENV', 'GOFLAGS', 'GOTOOLCHAIN', 'GOWORK')) {
    $analyzerEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
try {
    $env:CGO_ENABLED = '0'
    $env:GOENV = 'off'
    $env:GOFLAGS = ''
    $env:GOTOOLCHAIN = 'local'
    $env:GOWORK = 'off'
    $verifyOutput = @(& $goPath -C $repository run -buildvcs=false -mod=readonly `
        ./_testing/e2e/cmd/verifylatencymatrix `
        -input $matrixPath `
        -output $superiorityPath `
        -source $expectedSource 2>&1)
    $verifyExitCode = $LASTEXITCODE
}
finally {
    foreach ($name in $analyzerEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($name, $analyzerEnvironment[$name], 'Process')
    }
}
if ($verifyExitCode -ne 0) {
    throw ("Native latency was not lower in every observed balanced matrix cycle. " +
        "The failure artifact, if structurally valid, is '$superiorityPath'.`n" +
        ($verifyOutput -join [Environment]::NewLine))
}
$superiorityFile = Get-ExactEvidenceFile -Path $superiorityPath -Label 'latency superiority evidence'
$superiority = Get-Content -LiteralPath $superiorityPath -Raw -ErrorAction Stop |
    ConvertFrom-Json -ErrorAction Stop
if ([string]$superiority.schema -cne 'viiper.controller-to-game.latency-superiority-evidence/v1' -or
    [string]$superiority.verdict -cne 'pass' -or
    [string]$superiority.analysis.verdict -cne 'pass' -or
    [string]$superiority.analysis.cycle_id -cne $cycleId -or
    [int]$superiority.analysis.cycle_count -ne $cycleCount -or
    [int]$superiority.analysis.cycles_per_priority -ne $CyclesPerPriority) {
    throw 'The strict Go analyzer returned a contradictory superiority artifact.'
}
Write-Host "Validated normal/high-priority latency matrix: '$($matrixFile.path)' (SHA-256 $($matrixFile.sha256))."
Write-Host "Observed native latency lower in every balanced cycle for this exact machine session: '$($superiorityFile.path)' (SHA-256 $($superiorityFile.sha256))."

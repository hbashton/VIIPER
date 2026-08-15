[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$SysPath,

    [Parameter(Mandatory = $true)]
    [string]$PdbPath,

    [Parameter(Mandatory = $true)]
    [string]$MapPath,

    [string]$HelperPath,

    [string]$HelperPdbPath,

    [string]$MediaProbePath,

    [string]$MediaProbePdbPath,

    [string]$InputProbePath,

    [string]$InputProbePdbPath,

    [string]$SymChkPath
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Resolve-ExactArtifact {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$ExpectedName
    )

    $resolved = Get-Item -LiteralPath $Path -ErrorAction Stop
    if (-not $resolved.PSIsContainer -and $resolved.Name -ceq $ExpectedName -and
            $resolved.Length -gt 0) {
        return $resolved
    }
    throw "Expected non-empty '$ExpectedName' artifact at '$Path'."
}

function Resolve-SymChk {
    param([string]$ExplicitPath)

    if (-not [string]::IsNullOrWhiteSpace($ExplicitPath)) {
        return (Resolve-Path -LiteralPath $ExplicitPath -ErrorAction Stop).Path
    }
    $command = Get-Command symchk.exe -CommandType Application -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if ($null -ne $command) {
        return $command.Source
    }
    $candidate = Join-Path ${env:ProgramFiles(x86)} 'Windows Kits\10\Debuggers\x64\symchk.exe'
    if (Test-Path -LiteralPath $candidate -PathType Leaf) {
        return (Resolve-Path -LiteralPath $candidate).Path
    }
    throw 'symchk.exe is required to prove that the SYS and full private line PDB match.'
}

function Test-RelocatablePdbReference {
    param(
        [Parameter(Mandatory = $true)]$Image,
        [Parameter(Mandatory = $true)]$Pdb
    )

    $embeddedImageText = [Text.Encoding]::ASCII.GetString(
        [IO.File]::ReadAllBytes($Image.FullName))
    $pdbNamePattern = '(?:^|\x00)' + [regex]::Escape($Pdb.Name) + '(?:\x00|$)'
    if ($embeddedImageText -notmatch $pdbNamePattern -or
            $embeddedImageText -match ('(?i)[A-Z]:\\[^\x00]{0,512}' +
                [regex]::Escape($Pdb.Name))) {
        throw "'$($Image.Name)' must embed only the relocatable '$($Pdb.Name)' basename."
    }
}

function Test-MatchingPrivateSymbols {
    param(
        [Parameter(Mandatory = $true)]$Image,
        [Parameter(Mandatory = $true)]$Pdb,
        [Parameter(Mandatory = $true)][string]$SymChk
    )

    $savedErrorActionPreference = $ErrorActionPreference
    try {
        # Windows PowerShell 5.1 wraps native stderr as non-terminating
        # ErrorRecord objects. Preserve the text and judge symchk by its exit
        # code and the private line/type report.
        $ErrorActionPreference = 'Continue'
        $symbolOutput = (& $SymChk /v $Image.FullName /s $Pdb.DirectoryName 2>&1 |
            ForEach-Object { $_.ToString() } | Out-String)
        $symbolExitCode = $LASTEXITCODE
    }
    finally {
        $ErrorActionPreference = $savedErrorActionPreference
    }
    if ($symbolExitCode -ne 0 -or
            $symbolOutput -notmatch '(?im)private symbols & lines' -or
            $symbolOutput -notmatch '(?im)PDB Matched:\s+TRUE' -or
            $symbolOutput -notmatch '(?im)Line numbers:\s+TRUE' -or
            $symbolOutput -notmatch '(?im)Type Info:\s+TRUE') {
        throw "'$($Image.Name)' and '$($Pdb.Name)' are not a matching private source/line/type set.`n$symbolOutput"
    }
}

$sys = Resolve-ExactArtifact -Path $SysPath -ExpectedName 'ViiperUde.sys'
$pdb = Resolve-ExactArtifact -Path $PdbPath -ExpectedName 'ViiperUde.pdb'
$map = Resolve-ExactArtifact -Path $MapPath -ExpectedName 'ViiperUde.map'
$symchk = Resolve-SymChk -ExplicitPath $SymChkPath

Test-RelocatablePdbReference -Image $sys -Pdb $pdb
Test-MatchingPrivateSymbols -Image $sys -Pdb $pdb -SymChk $symchk

$mapText = Get-Content -LiteralPath $map.FullName -Raw
foreach ($symbol in @('ViiperTraceLifecycle', 'ViiperEvtEndpointPurgeWorkItem',
        'ViiperEvtEndpointPurge', 'ViiperBeginControllerShutdown',
        'ViiperEvtEndpointIoInternalControl', 'ViiperSubmitInputReport',
        'ViiperEvtFastInputQueueReady', 'ViiperPrepareCachedInputUrb',
        'ViiperQueueUrb', 'ViiperDispatchAvailable', 'ViiperSerializeOperation',
        'ViiperReserveIsoStartFrame', 'ViiperQueueUrbCompletion',
        'ViiperEvtCompletionDpc')) {
    if ($mapText -notmatch ('\b' + [regex]::Escape($symbol) + '\b')) {
        throw "The driver link map does not contain required lifecycle/hot-path symbol '$symbol'."
    }
}

$userModeArtifacts = @(
    [pscustomobject]@{
        ImagePath = $HelperPath
        PdbPath = $HelperPdbPath
        ImageName = 'ViiperUdeCtl.exe'
        PdbName = 'ViiperUdeCtl.pdb'
    },
    [pscustomobject]@{
        ImagePath = $MediaProbePath
        PdbPath = $MediaProbePdbPath
        ImageName = 'ViiperUdeMediaProbe.exe'
        PdbName = 'ViiperUdeMediaProbe.pdb'
    },
    [pscustomobject]@{
        ImagePath = $InputProbePath
        PdbPath = $InputProbePdbPath
        ImageName = 'ViiperUdeInputProbe.exe'
        PdbName = 'ViiperUdeInputProbe.pdb'
    }
)
foreach ($artifact in $userModeArtifacts) {
    $hasImage = -not [string]::IsNullOrWhiteSpace($artifact.ImagePath)
    $hasPdb = -not [string]::IsNullOrWhiteSpace($artifact.PdbPath)
    if ($hasImage -ne $hasPdb) {
        throw "Both '$($artifact.ImageName)' and '$($artifact.PdbName)' must be supplied together."
    }
    if (-not $hasImage) {
        continue
    }
    $image = Resolve-ExactArtifact -Path $artifact.ImagePath -ExpectedName $artifact.ImageName
    $userPdb = Resolve-ExactArtifact -Path $artifact.PdbPath -ExpectedName $artifact.PdbName
    Test-RelocatablePdbReference -Image $image -Pdb $userPdb
    Test-MatchingPrivateSymbols -Image $image -Pdb $userPdb -SymChk $symchk
}

Write-Host 'VIIPER UDE debug artifacts match and contain private symbols, line tables, type information, and required lifecycle/hot-path symbols.'

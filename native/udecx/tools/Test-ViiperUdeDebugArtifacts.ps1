[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$SysPath,

    [Parameter(Mandatory = $true)]
    [string]$PdbPath,

    [Parameter(Mandatory = $true)]
    [string]$MapPath,

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

$sys = Resolve-ExactArtifact -Path $SysPath -ExpectedName 'ViiperUde.sys'
$pdb = Resolve-ExactArtifact -Path $PdbPath -ExpectedName 'ViiperUde.pdb'
$map = Resolve-ExactArtifact -Path $MapPath -ExpectedName 'ViiperUde.map'
$symchk = Resolve-SymChk -ExplicitPath $SymChkPath

$embeddedImageText = [Text.Encoding]::ASCII.GetString(
    [IO.File]::ReadAllBytes($sys.FullName))
if ($embeddedImageText -notmatch '(?:^|\x00)ViiperUde\.pdb(?:\x00|$)' -or
        $embeddedImageText -match '(?i)[A-Z]:\\[^\x00]{0,512}ViiperUde\.pdb') {
    throw 'The driver image must embed only the relocatable ViiperUde.pdb basename.'
}

$savedErrorActionPreference = $ErrorActionPreference
try {
    # Windows PowerShell 5.1 wraps native stderr as non-terminating ErrorRecord
    # objects even when the native tool succeeds. Preserve that diagnostic text
    # and judge the tool only by its exit code and matching-symbol report.
    $ErrorActionPreference = 'Continue'
    $symbolOutput = (& $symchk /v $sys.FullName /s $pdb.DirectoryName 2>&1 |
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
    throw "The driver debug artifacts are not a matching private source/line/type set.`n$symbolOutput"
}

$mapText = Get-Content -LiteralPath $map.FullName -Raw
foreach ($symbol in @('ViiperTraceLifecycle', 'ViiperEvtEndpointQueuePurged',
        'ViiperEvtEndpointPurge', 'ViiperBeginControllerShutdown')) {
    if ($mapText -notmatch ('\b' + [regex]::Escape($symbol) + '\b')) {
        throw "The driver link map does not contain required lifecycle symbol '$symbol'."
    }
}

Write-Host "VIIPER UDE debug artifacts match and contain private symbols, line tables, type information, and lifecycle map symbols."

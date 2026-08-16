[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$scriptPath = Join-Path $PSScriptRoot 'Set-ViiperCrashDiagnostics.ps1'
$scriptItem = Get-Item -LiteralPath $scriptPath -Force -ErrorAction Stop
if ($scriptItem.PSIsContainer -or
    ($scriptItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or
    $scriptItem.Length -le 0) {
    throw "Crash-diagnostic script is unsafe or empty: '$scriptPath'."
}

$tokens = $null
$parseErrors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile(
    $scriptItem.FullName, [ref]$tokens, [ref]$parseErrors)
if (@($parseErrors).Count -ne 0) {
    throw "Crash-diagnostic script parse failed: $(@($parseErrors | ForEach-Object Message) -join '; ')"
}

$writers = @($ast.FindAll({
            param($node)
            $node -is [Management.Automation.Language.FunctionDefinitionAst] -and
            $node.Name -ceq 'Write-StateFile'
        }, $true))
if ($writers.Count -ne 1) {
    throw "Expected exactly one Write-StateFile definition; found $($writers.Count)."
}
$writerSource = $writers[0].Extent.Text
if ($writerSource -notmatch
        '\[Management\.Automation\.Language\.NullString\]::Value') {
    throw 'Existing crash-policy state replacement must pass a true CLR null backup path.'
}

# Load only the state-writer function. This avoids every registry, pagefile,
# dump-policy, privilege, and reboot path in the production script.
. ([ScriptBlock]::Create($writerSource))

$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) (
    'viiper-crash-writer-contract-' + [Guid]::NewGuid().ToString('N'))
[void][IO.Directory]::CreateDirectory($temporaryRoot)
try {
    $StatePath = Join-Path $temporaryRoot 'crash-policy-backup.json'
    [IO.File]::WriteAllText(
        $StatePath, '{"schema":1,"sequence":0}',
        [Text.UTF8Encoding]::new($false))

    foreach ($sequence in 1..16) {
        Write-StateFile -State ([ordered]@{
                schema = 1
                machine = 'contract-machine'
                sequence = $sequence
            })
    }

    $state = Get-Content -LiteralPath $StatePath -Raw -Encoding UTF8 |
        ConvertFrom-Json -ErrorAction Stop
    if ([int]$state.schema -ne 1 -or [int]$state.sequence -ne 16 -or
        [string]$state.machine -cne 'contract-machine') {
        throw 'Crash-policy state replacement did not publish the final state.'
    }
    $bytes = [IO.File]::ReadAllBytes($StatePath)
    if ($bytes.Length -eq 0 -or
        ($bytes.Length -ge 3 -and $bytes[0] -eq 0xef -and
         $bytes[1] -eq 0xbb -and $bytes[2] -eq 0xbf)) {
        throw 'Crash-policy state replacement must remain non-empty UTF-8 without a BOM.'
    }
    if (Test-Path -LiteralPath "$StatePath.tmp") {
        throw 'Crash-policy state replacement retained its temporary file.'
    }
}
finally {
    $resolvedTemporary = [IO.Path]::GetFullPath($temporaryRoot)
    $systemTemporary = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if ($resolvedTemporary.StartsWith(
            $systemTemporary, [StringComparison]::OrdinalIgnoreCase) -and
        [IO.Path]::GetFileName($resolvedTemporary) -like
            'viiper-crash-writer-contract-*') {
        Remove-Item -LiteralPath $resolvedTemporary -Recurse -Force
    }
}

Write-Host 'VIIPER crash-diagnostic atomic state-writer contract passed.'

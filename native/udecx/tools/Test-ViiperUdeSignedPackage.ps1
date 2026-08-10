[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$PackageDirectory
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$root = Resolve-Path -LiteralPath $PackageDirectory -ErrorAction Stop
if (-not (Get-Item -LiteralPath $root.Path).PSIsContainer) {
    throw "The signed package path must be a directory."
}

$expected = @('ViiperUde.inf', 'ViiperUde.sys', 'ViiperUde.pdb', 'ViiperUde.cat')
$files = @{}
foreach ($name in $expected) {
    $matches = @(Get-ChildItem -LiteralPath $root.Path -Recurse -File -Filter $name)
    if ($matches.Count -ne 1) {
        throw "The signed package must contain exactly one '$name'; found $($matches.Count)."
    }
    $files[$name] = $matches[0].FullName
}

$signTool = Get-Command signtool.exe -ErrorAction Stop
foreach ($name in @('ViiperUde.sys', 'ViiperUde.cat')) {
    & $signTool.Source verify /kp /v $files[$name]
    if ($LASTEXITCODE -ne 0) {
        throw "Kernel-policy signature validation failed for '$name' with exit code $LASTEXITCODE."
    }
    $signature = Get-AuthenticodeSignature -LiteralPath $files[$name]
    if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid -or
        $null -eq $signature.SignerCertificate -or
        $signature.SignerCertificate.Subject -notmatch '(?i)Microsoft') {
        throw "'$name' does not have a valid Microsoft production signature."
    }
}

$infVerif = Get-Command infverif.exe -ErrorAction SilentlyContinue
if ($null -ne $infVerif) {
    & $infVerif.Source /v $files['ViiperUde.inf']
    if ($LASTEXITCODE -ne 0) {
        throw "InfVerif rejected the Microsoft-signed package with exit code $LASTEXITCODE."
    }
}

Write-Host "Validated Microsoft-signed VIIPER native UDE package at '$($root.Path)'."

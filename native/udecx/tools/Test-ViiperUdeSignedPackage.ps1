[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$PackageDirectory,

    [Parameter(Mandatory = $true)]
    [string]$SubmissionManifestPath,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$')]
    [string]$ExpectedSourceRevision,

    [ValidateSet('LocalTest', 'ControlledTest', 'Production')]
    [string]$ValidationMode = 'Production',

    [string]$LocalTestCertificatePath,

    [switch]$RequireLocalTestToolchainValidation
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function ConvertTo-WindowsCommandLineArgument {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Value)

    if ($Value.Length -gt 0 -and $Value -notmatch '[\s"]') {
        return $Value
    }
    $builder = [Text.StringBuilder]::new()
    [void]$builder.Append('"')
    $backslashes = 0
    foreach ($character in $Value.ToCharArray()) {
        if ($character -eq '\') {
            ++$backslashes
            continue
        }
        if ($character -eq '"') {
            [void]$builder.Append(('\' * ($backslashes * 2 + 1)))
            [void]$builder.Append('"')
            $backslashes = 0
            continue
        }
        [void]$builder.Append(('\' * $backslashes))
        $backslashes = 0
        [void]$builder.Append($character)
    }
    [void]$builder.Append(('\' * ($backslashes * 2)))
    [void]$builder.Append('"')
    return $builder.ToString()
}

function Invoke-BoundedValidationTool {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][string]$Operation,
        [int]$TimeoutMilliseconds = 120000,
        [switch]$SuppressOutput
    )

    $startInfo = [Diagnostics.ProcessStartInfo]::new()
    $startInfo.FileName = $FilePath
    $startInfo.Arguments = (($Arguments | ForEach-Object {
                ConvertTo-WindowsCommandLineArgument -Value $_
            }) -join ' ')
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true

    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $startInfo
    try {
        if (-not $process.Start()) {
            throw "$Operation did not start."
        }
        $stdoutTask = $process.StandardOutput.ReadToEndAsync()
        $stderrTask = $process.StandardError.ReadToEndAsync()
        if (-not $process.WaitForExit($TimeoutMilliseconds)) {
            try {
                $process.Kill()
                $process.WaitForExit()
            }
            catch {
                throw "$Operation exceeded $TimeoutMilliseconds ms and could not be joined: $($_.Exception.Message)"
            }
            throw "$Operation exceeded $TimeoutMilliseconds ms and was terminated before package mutation."
        }
        $stdout = $stdoutTask.GetAwaiter().GetResult()
        $stderr = $stderrTask.GetAwaiter().GetResult()
        if (-not $SuppressOutput -and $stdout) { Write-Host $stdout.TrimEnd() }
        if (-not $SuppressOutput -and $stderr) { Write-Host $stderr.TrimEnd() }
        return [pscustomobject]@{
            ExitCode = $process.ExitCode
            StandardOutput = $stdout
            StandardError = $stderr
        }
    }
    finally {
        $process.Dispose()
    }
}

function Get-CertificateEkuOids {
    param(
        [Parameter(Mandatory = $true)]
        [Security.Cryptography.X509Certificates.X509Certificate2]$Certificate
    )

    $oids = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($extension in $Certificate.Extensions) {
        if ($extension.Oid.Value -ne '2.5.29.37') {
            continue
        }
        $eku = if ($extension -is [Security.Cryptography.X509Certificates.X509EnhancedKeyUsageExtension]) {
            $extension
        }
        else {
            [Security.Cryptography.X509Certificates.X509EnhancedKeyUsageExtension]::new($extension, $false)
        }
        foreach ($oid in $eku.EnhancedKeyUsages) {
            [void]$oids.Add($oid.Value)
        }
    }
    return ,$oids
}

function Get-CertificateSha256 {
    param(
        [Parameter(Mandatory = $true)]
        [Security.Cryptography.X509Certificates.X509Certificate2]$Certificate
    )

    $algorithm = [Security.Cryptography.SHA256]::Create()
    try {
        return ([BitConverter]::ToString(
            $algorithm.ComputeHash($Certificate.RawData))).Replace('-', '').ToLowerInvariant()
    }
    finally {
        $algorithm.Dispose()
    }
}

function Get-BoundedAuthenticodeSignature {
    param([Parameter(Mandatory = $true)][string]$Path)

    $encodedPath = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($Path))
    $command = @"
`$ErrorActionPreference = 'Stop'
`$ProgressPreference = 'SilentlyContinue'
`$path = [Text.Encoding]::Unicode.GetString([Convert]::FromBase64String('$encodedPath'))
`$signature = Get-AuthenticodeSignature -LiteralPath `$path
`$certificate = if (`$null -eq `$signature.SignerCertificate) { '' } else {
    [Convert]::ToBase64String(`$signature.SignerCertificate.RawData)
}
[ordered]@{ status = `$signature.Status.ToString(); certificate = `$certificate } |
    ConvertTo-Json -Compress
"@
    $encodedCommand = [Convert]::ToBase64String(
        [Text.Encoding]::Unicode.GetBytes($command))
    $hostPath = (Get-Process -Id $PID).Path
    $result = Invoke-BoundedValidationTool -FilePath $hostPath `
        -Arguments @('-NoProfile', '-NonInteractive', '-EncodedCommand', $encodedCommand) `
        -Operation "Authenticode validation for '$Path'" -SuppressOutput
    if ($result.ExitCode -ne 0) {
        throw "Authenticode validation failed for '$Path' with exit code $($result.ExitCode)."
    }
    try {
        $value = $result.StandardOutput.Trim() | ConvertFrom-Json -ErrorAction Stop
        if ([string]$value.status -notmatch '^[A-Za-z]+$' -or
            [string]::IsNullOrEmpty([string]$value.certificate)) {
            throw 'missing status or signer certificate'
        }
        $certificateBytes = [Convert]::FromBase64String([string]$value.certificate)
        return [pscustomobject]@{
            Status = [string]$value.status
            SignerCertificate = [Security.Cryptography.X509Certificates.X509Certificate2]::new(
                $certificateBytes)
        }
    }
    catch {
        throw "Authenticode validation returned malformed evidence for '$Path': $($_.Exception.Message)"
    }
}

function Assert-DriverSignature {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [ValidateSet('LocalTest', 'ControlledTest', 'Production')]
        [string]$Mode,

        [string]$ExpectedLocalTestCertificateSha256
    )

    $signature = Get-BoundedAuthenticodeSignature -Path $Path
    try {
        if ($signature.Status -cne 'Valid') {
            throw "'$Path' does not have a valid Authenticode signature (status '$($signature.Status)')."
        }
        if ($null -eq $signature.SignerCertificate) {
            throw "'$Path' did not expose its signing certificate."
        }
        if ($Mode -eq 'LocalTest') {
            $actual = Get-CertificateSha256 -Certificate $signature.SignerCertificate
            if ($ExpectedLocalTestCertificateSha256 -notmatch '^[0-9a-f]{64}$' -or
                $actual -cne $ExpectedLocalTestCertificateSha256) {
                throw "'$Path' is not signed by the exact source-bound local test certificate."
            }
            return
        }
        if (
            $signature.SignerCertificate.Subject -notmatch '(?i)(^|,\s*)O=Microsoft Corporation(,|$)') {
            throw "'$Path' is not signed by Microsoft Corporation."
        }

        $ekuOids = Get-CertificateEkuOids -Certificate $signature.SignerCertificate
        $hardwareVerificationOid = '1.3.6.1.4.1.311.10.3.5'
        $attestedVerificationOid = '1.3.6.1.4.1.311.10.3.5.1'
        if (-not $ekuOids.Contains($hardwareVerificationOid)) {
            throw "'$Path' lacks the Windows Hardware Driver Verification EKU."
        }
        if ($Mode -eq 'ControlledTest') {
            if (-not $ekuOids.Contains($attestedVerificationOid)) {
                throw "'$Path' is not a Microsoft attestation-signed controlled-test artifact."
            }
        }
        elseif ($ekuOids.Contains($attestedVerificationOid)) {
            throw "'$Path' is attestation signed and cannot pass the production HLK/WHCP release gate."
        }
    }
    finally {
        $signature.SignerCertificate.Dispose()
    }
}

$root = Resolve-Path -LiteralPath $PackageDirectory -ErrorAction Stop
if (-not (Get-Item -LiteralPath $root.Path).PSIsContainer) {
    throw 'The signed package path must be a directory.'
}

$localTestCertificate = $null
$localTestCertificateSha256 = $null
if ($ValidationMode -eq 'LocalTest') {
    if ([string]::IsNullOrWhiteSpace($LocalTestCertificatePath)) {
        throw '-LocalTestCertificatePath is required for LocalTest validation.'
    }
    $resolvedCertificate = (Resolve-Path -LiteralPath $LocalTestCertificatePath -ErrorAction Stop).Path
    $certificateItem = Get-Item -LiteralPath $resolvedCertificate -Force
    if ($certificateItem.PSIsContainer -or $certificateItem.Length -le 0 -or
        $certificateItem.Name -cne 'ViiperUdeTest.cer' -or
        ($certificateItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw 'The local test certificate must be a nonempty, case-exact, non-reparse ViiperUdeTest.cer file.'
    }
    $localTestCertificate = [Security.Cryptography.X509Certificates.X509Certificate2]::new($resolvedCertificate)
    $localTestCertificateSha256 = Get-CertificateSha256 -Certificate $localTestCertificate
}
elseif (-not [string]::IsNullOrWhiteSpace($LocalTestCertificatePath)) {
    throw '-LocalTestCertificatePath is valid only with -ValidationMode LocalTest.'
}
if ($RequireLocalTestToolchainValidation -and $ValidationMode -ne 'LocalTest') {
    throw '-RequireLocalTestToolchainValidation is valid only with -ValidationMode LocalTest.'
}

$expectedNames = @('ViiperUde.inf', 'ViiperUde.sys', 'ViiperUde.pdb', 'ViiperUde.cat')
$allEntries = @(Get-ChildItem -LiteralPath $root.Path -Force)
if (@($allEntries | Where-Object PSIsContainer).Count -ne 0) {
    throw 'The signed package files must be direct children of the package directory; subdirectories are forbidden.'
}
$allFiles = @($allEntries | Where-Object { -not $_.PSIsContainer })
if ($allFiles.Count -ne $expectedNames.Count) {
    throw "The signed package must contain exactly $($expectedNames.Count) files; found $($allFiles.Count)."
}
$files = @{}
foreach ($name in $expectedNames) {
    $matches = @($allFiles | Where-Object Name -CEQ $name)
    if ($matches.Count -ne 1) {
        throw "The signed package must contain exactly one case-exact '$name'; found $($matches.Count)."
    }
    if ($matches[0].Length -le 0) {
        throw "The signed package file '$name' is empty."
    }
    $files[$name] = $matches[0].FullName
}
if (@($allFiles | Where-Object { $_.DirectoryName -cne $root.Path }).Count -ne 0) {
    throw 'All signed package files must reside directly in the canonical package directory.'
}

$manifestFile = Resolve-Path -LiteralPath $SubmissionManifestPath -ErrorAction Stop
$manifest = Get-Content -LiteralPath $manifestFile.Path -Raw | ConvertFrom-Json
$projectPath = Join-Path $PSScriptRoot '..\driver\ViiperUde.vcxproj'
[xml]$driverProject = Get-Content -LiteralPath $projectPath -Raw
$projectNamespace = [Xml.XmlNamespaceManager]::new($driverProject.NameTable)
$projectNamespace.AddNamespace('msb', 'http://schemas.microsoft.com/developer/msbuild/2003')
$versionNodes = @($driverProject.SelectNodes('//msb:ViiperUdeDriverVersion', $projectNamespace))
if ($versionNodes.Count -ne 1) {
    throw 'The driver project must declare one deterministic ViiperUdeDriverVersion.'
}
$driverPackageVersion = $versionNodes[0].InnerText.Trim()
$expectedBuildIdentity = & (Join-Path $PSScriptRoot 'Get-ViiperUdeBuildIdentity.ps1') `
    -SourceRevision $ExpectedSourceRevision `
    -DriverPackageVersion $driverPackageVersion `
    -ABIMajor 1 -ABIMinor 10 -Capabilities 13
if ($manifest.schema -ne 2 -or
    [string]$manifest.sourceRevision -cne $ExpectedSourceRevision.ToLowerInvariant() -or
    [string]$manifest.driverPackageVersion -cne $driverPackageVersion -or
    [int]$manifest.driverABIMajor -ne 1 -or [int]$manifest.driverABIMinor -ne 10 -or
    [string]$manifest.driverCapabilities -cne '0x0000000d' -or
    [string]$manifest.driverBuildIdentity -cne $expectedBuildIdentity) {
    throw 'The submission manifest schema, source revision, or native loaded-build identity does not match the reviewed source.'
}
if ($ValidationMode -eq 'LocalTest') {
    if ([bool]$manifest.releaseEligible -or [string]$manifest.signingRoute -cne 'LocalTest' -or
        [string]$manifest.testSignerCertificateSha256 -cne $localTestCertificateSha256) {
        throw 'LocalTest validation requires a non-release manifest bound to the exact local test certificate.'
    }
}
elseif ($ValidationMode -eq 'ControlledTest') {
    if ([bool]$manifest.releaseEligible -or [string]$manifest.signingRoute -cne 'ControlledTestAttestation') {
        throw 'Controlled-test validation requires a testing-only attestation submission manifest.'
    }
}
elseif (-not [bool]$manifest.releaseEligible -or [string]$manifest.signingRoute -cne 'HLK/WHCP') {
    throw 'Production validation requires a release-eligible HLK/WHCP submission manifest.'
}

$manifestFiles = @($manifest.files)
if ($manifestFiles.Count -ne $expectedNames.Count) {
    throw "The submission manifest must describe exactly $($expectedNames.Count) files."
}
$manifestByName = @{}
foreach ($entry in $manifestFiles) {
    $name = [string]$entry.name
    if ($expectedNames -cnotcontains $name -or $manifestByName.ContainsKey($name)) {
        throw "The submission manifest contains an unexpected or duplicate file '$name'."
    }
    if ([long]$entry.length -le 0 -or [string]$entry.sha256 -cnotmatch '^[0-9A-Fa-f]{64}$') {
        throw "The submission manifest contains invalid metadata for '$name'."
    }
    $manifestByName[$name] = $entry
}
foreach ($name in @('ViiperUde.inf', 'ViiperUde.pdb')) {
    if (-not $manifestByName.ContainsKey($name)) {
        throw "The submission manifest does not describe '$name'."
    }
    $actual = Get-Item -LiteralPath $files[$name]
    $actualHash = (Get-FileHash -LiteralPath $actual.FullName -Algorithm SHA256).Hash
    if ($actual.Length -ne [long]$manifestByName[$name].length -or
        $actualHash -cne ([string]$manifestByName[$name].sha256).ToUpperInvariant()) {
        throw "The Microsoft-returned '$name' does not match the source-bound submission manifest."
    }
}

foreach ($name in @('ViiperUde.cat', 'ViiperUde.sys')) {
    Assert-DriverSignature -Path $files[$name] -Mode $ValidationMode `
        -ExpectedLocalTestCertificateSha256 $localTestCertificateSha256
}
$requireExternalTools = $ValidationMode -ne 'LocalTest' -or $RequireLocalTestToolchainValidation
if ($requireExternalTools) {
    $signTool = Get-Command signtool.exe -ErrorAction Stop
    foreach ($name in @('ViiperUde.cat', 'ViiperUde.sys')) {
        $policy = if ($ValidationMode -eq 'LocalTest') { '/pa' } else { '/kp' }
        $exitCode = Invoke-BoundedValidationTool -FilePath $signTool.Source `
            -Arguments @('verify', $policy, '/v', $files[$name]) `
            -Operation "SignTool signature validation for '$name'"
        if ($exitCode.ExitCode -ne 0) {
            throw "Signature policy validation failed for '$name' with exit code $($exitCode.ExitCode)."
        }
    }
    foreach ($name in @('ViiperUde.inf', 'ViiperUde.sys')) {
        $policy = if ($ValidationMode -eq 'LocalTest') { '/pa' } else { '/kp' }
        $exitCode = Invoke-BoundedValidationTool -FilePath $signTool.Source `
            -Arguments @('verify', $policy, '/v', '/c', $files['ViiperUde.cat'], $files[$name]) `
            -Operation "SignTool catalog membership validation for '$name'"
        if ($exitCode.ExitCode -ne 0) {
            throw "'$name' is not a verified member of the exact catalog (exit code $($exitCode.ExitCode))."
        }
    }

    $infVerif = Get-Command infverif.exe -ErrorAction Stop
    foreach ($mode in @('/h', '/u')) {
        $exitCode = Invoke-BoundedValidationTool -FilePath $infVerif.Source `
            -Arguments @($mode, $files['ViiperUde.inf']) `
            -Operation "InfVerif $mode validation"
        if ($exitCode.ExitCode -ne 0) {
            throw "InfVerif $mode rejected the signed package with exit code $($exitCode.ExitCode)."
        }
    }
}

$signatureKind = if ($ValidationMode -eq 'LocalTest') { 'local test-signed' } else { 'Microsoft-signed' }
Write-Host "Validated source-bound $signatureKind VIIPER native UDE package in $ValidationMode mode at '$($root.Path)'."

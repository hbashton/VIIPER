[CmdletBinding()]
param(
    [string]$BaseRevision,

    [string]$HeadRevision = 'HEAD'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$projectPath = 'native/udecx/driver/ViiperUde.vcxproj'
$infPath = 'native/udecx/package/ViiperUde.inf'
$protocolPath = 'internal/transport/udecx/protocol.go'
$payloadPaths = @(
    'native/udecx/driver',
    'native/udecx/include',
    'native/udecx/package'
)

function Invoke-Git {
    param(
        [Parameter(Mandatory = $true)]
        [string[]]$Arguments,

        [switch]$AllowFailure
    )

    $output = @(& git @Arguments 2>&1)
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0 -and -not $AllowFailure) {
        throw "git $($Arguments -join ' ') failed with exit code $exitCode`n$($output -join [Environment]::NewLine)"
    }
    return [pscustomobject]@{
        ExitCode = $exitCode
        Text = ($output -join "`n").Trim()
    }
}

function Resolve-Commit {
    param([Parameter(Mandatory = $true)][string]$Revision)

    return (Invoke-Git -Arguments @('rev-parse', '--verify', "$Revision^{commit}")).Text
}

function Test-GitPath {
    param(
        [Parameter(Mandatory = $true)][string]$Revision,
        [Parameter(Mandatory = $true)][string]$Path
    )

    $result = Invoke-Git -Arguments @('cat-file', '-e', "$Revision`:$Path") -AllowFailure
    return $result.ExitCode -eq 0
}

function ConvertTo-DriverVersion {
    param(
        [Parameter(Mandatory = $true)][string]$Value,
        [Parameter(Mandatory = $true)][string]$Revision
    )

    if ($Value -notmatch '^\d+\.\d+\.\d+\.\d+$') {
        throw "Driver version at $Revision is not a four-part numeric value: '$Value'."
    }
    $parts = @($Value.Split('.') | ForEach-Object { [int64]$_ })
    if (@($parts | Where-Object { $_ -gt 65535 }).Count -ne 0) {
        throw "Driver version at $Revision exceeds the Windows 16-bit component limit: '$Value'."
    }
    return [Version]$Value
}

function Get-DriverContract {
    param(
        [Parameter(Mandatory = $true)][string]$Revision,
        [switch]$Required
    )

    if (-not (Test-GitPath -Revision $Revision -Path $projectPath)) {
        if ($Required) {
            throw "The native driver project is missing at $Revision."
        }
        return $null
    }

    [xml]$project = (Invoke-Git -Arguments @('show', "$Revision`:$projectPath")).Text
    $namespace = New-Object System.Xml.XmlNamespaceManager($project.NameTable)
    $namespace.AddNamespace('msb', 'http://schemas.microsoft.com/developer/msbuild/2003')

    $dateNodes = @($project.SelectNodes('//msb:ViiperUdeDriverDate', $namespace))
    $versionNodes = @($project.SelectNodes('//msb:ViiperUdeDriverVersion', $namespace))
    if ($dateNodes.Count -ne 1 -or $versionNodes.Count -ne 1) {
        if (-not $Required) {
            return $null
        }
        throw "The native project at $Revision must contain one driver date and version."
    }

    $dateText = $dateNodes[0].InnerText.Trim()
    $date = [DateTime]::MinValue
    if (-not [DateTime]::TryParseExact(
            $dateText,
            'MM/dd/yyyy',
            [Globalization.CultureInfo]::InvariantCulture,
            [Globalization.DateTimeStyles]::None,
            [ref]$date)) {
        throw "Driver date at $Revision is not deterministic MM/dd/yyyy: '$dateText'."
    }
    $versionText = $versionNodes[0].InnerText.Trim()
    $version = ConvertTo-DriverVersion -Value $versionText -Revision $Revision

    if (-not (Test-GitPath -Revision $Revision -Path $infPath)) {
        throw "The native INF template is missing at $Revision."
    }
    $inf = (Invoke-Git -Arguments @('show', "$Revision`:$infPath")).Text
    $driverVerPattern = '(?mi)^DriverVer\s*=\s*' +
        [regex]::Escape($dateText) + '\s*,\s*' +
        [regex]::Escape($versionText) + '\s*$'
    if ($inf -notmatch $driverVerPattern) {
        throw "The INF DriverVer at $Revision does not match the project contract '$dateText,$versionText'."
    }

    if (-not (Test-GitPath -Revision $Revision -Path $protocolPath)) {
        throw "The native broker package-version contract is missing at $Revision."
    }
    $protocol = (Invoke-Git -Arguments @('show', "$Revision`:$protocolPath")).Text
    $matches = @([regex]::Matches(
            $protocol,
            '(?m)^\s*DriverPackageVersion\s*=\s*"(?<version>\d+\.\d+\.\d+\.\d+)"\s*$'))
    if ($matches.Count -ne 1 -or $matches[0].Groups['version'].Value -cne $versionText) {
        throw "The Go broker package version at $Revision does not exactly match DriverVer '$versionText'."
    }

    $tree = (Invoke-Git -Arguments (@('ls-tree', '-r', '--full-tree', $Revision, '--') + $payloadPaths)).Text
    return [pscustomobject]@{
        Revision = $Revision
        Date = $date.Date
        DateText = $dateText
        Version = $version
        VersionText = $versionText
        PayloadTree = $tree
    }
}

$head = Resolve-Commit -Revision $HeadRevision
$headContract = Get-DriverContract -Revision $head -Required

$base = $null
if (-not [string]::IsNullOrWhiteSpace($BaseRevision) -and
        $BaseRevision -notmatch '^0{40}$') {
    $base = Resolve-Commit -Revision $BaseRevision
}
else {
    $parent = Invoke-Git -Arguments @('rev-parse', '--verify', "$head^") -AllowFailure
    if ($parent.ExitCode -eq 0) {
        $base = $parent.Text
    }
}

if ($null -eq $base) {
    Write-Host "Validated initial native DriverVer contract $($headContract.DateText),$($headContract.VersionText) at $head."
    return
}

$baseContract = Get-DriverContract -Revision $base
if ($null -eq $baseContract) {
    Write-Host "Validated initial native DriverVer contract $($headContract.DateText),$($headContract.VersionText); baseline $base predates the contract."
    return
}

if ($headContract.Date -lt $baseContract.Date) {
    throw "Native DriverVer date regressed from $($baseContract.DateText) at $base to $($headContract.DateText) at $head."
}
if ($headContract.Version -lt $baseContract.Version) {
    throw "Native DriverVer version regressed from $($baseContract.VersionText) at $base to $($headContract.VersionText) at $head."
}

$payloadChanged = $headContract.PayloadTree -cne $baseContract.PayloadTree
if ($payloadChanged -and $headContract.Version -le $baseContract.Version) {
    throw "Native driver package content changed between $base and $head without a strict DriverVer version increase above $($baseContract.VersionText)."
}

$changeState = if ($payloadChanged) { 'changed with a strict version increase' } else { 'is byte-identical' }
Write-Host "Native driver package content $changeState relative to $base; DriverVer is $($headContract.DateText),$($headContract.VersionText)."

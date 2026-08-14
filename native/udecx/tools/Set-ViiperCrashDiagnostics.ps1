[CmdletBinding()]
param(
    [ValidateSet('Status', 'Enable', 'Restore')]
    [string]$Mode = 'Status',

    [ValidateSet('Complete', 'Kernel', 'Automatic')]
    [string]$DumpType = 'Complete',

    [string]$StatePath,

    [switch]$AcknowledgeDiskUse
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$crashControlPath = 'HKLM:\SYSTEM\CurrentControlSet\Control\CrashControl'
$memoryManagementPath = 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Memory Management'
if ([string]::IsNullOrWhiteSpace($StatePath)) {
    $StatePath = Join-Path $env:ProgramData 'Viiper\diagnostics\crash-policy-backup.json'
}

function Assert-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole(
            [Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Crash-diagnostic configuration requires an elevated PowerShell session.'
    }
}

function Get-RegistryValueSnapshot {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string[]]$Names
    )

    $key = Get-Item -LiteralPath $Path -ErrorAction Stop
    $presentNames = @($key.GetValueNames())
    $values = [ordered]@{}
    foreach ($name in $Names) {
        $present = $presentNames -contains $name
        $values[$name] = [ordered]@{
            present = $present
            kind = if ($present) { $key.GetValueKind($name).ToString() } else { $null }
            value = if ($present) {
                $key.GetValue($name, $null,
                    [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
            }
            else { $null }
        }
    }
    return $values
}

function Set-RegistryValueFromSnapshot {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)]$Snapshot
    )

    if (-not [bool]$Snapshot.present) {
        Remove-ItemProperty -LiteralPath $Path -Name $Name -ErrorAction SilentlyContinue
        return
    }
    $propertyType = switch ([string]$Snapshot.kind) {
        'DWord' { 'DWord' }
        'QWord' { 'QWord' }
        'String' { 'String' }
        'ExpandString' { 'ExpandString' }
        'MultiString' { 'MultiString' }
        'Binary' { 'Binary' }
        default { throw "Unsupported saved registry kind '$($Snapshot.kind)' for '$Name'." }
    }
    $value = $Snapshot.value
    if ($propertyType -eq 'MultiString') {
        $value = @($value | ForEach-Object { [string]$_ })
    }
    New-ItemProperty -LiteralPath $Path -Name $Name -Value $value `
        -PropertyType $propertyType -Force | Out-Null
}

function Write-StateFile {
    param([Parameter(Mandatory = $true)]$State)

    $fullPath = [IO.Path]::GetFullPath($StatePath)
    $directory = Split-Path -Parent $fullPath
    New-Item -ItemType Directory -Path $directory -Force | Out-Null
    $temporary = "$fullPath.tmp"
    [IO.File]::WriteAllText($temporary, ($State | ConvertTo-Json -Depth 8),
        [Text.UTF8Encoding]::new($false))
    [IO.File]::Replace($temporary, $fullPath, $null, $true)
}

function Write-NewStateFile {
    param([Parameter(Mandatory = $true)]$State)

    $fullPath = [IO.Path]::GetFullPath($StatePath)
    $directory = Split-Path -Parent $fullPath
    New-Item -ItemType Directory -Path $directory -Force | Out-Null
    if (Test-Path -LiteralPath $fullPath) {
        throw "Crash-diagnostic backup already exists at '$fullPath'; restore it before replacing policy."
    }
    $temporary = "$fullPath.tmp"
    [IO.File]::WriteAllText($temporary, ($State | ConvertTo-Json -Depth 8),
        [Text.UTF8Encoding]::new($false))
    Move-Item -LiteralPath $temporary -Destination $fullPath
}

function Get-CurrentStatus {
    $computer = Get-CimInstance Win32_ComputerSystem -ErrorAction Stop
    $pageUsage = @(Get-CimInstance Win32_PageFileUsage -ErrorAction SilentlyContinue)
    $crash = Get-RegistryValueSnapshot -Path $crashControlPath -Names @(
        'CrashDumpEnabled', 'DumpFile', 'AlwaysKeepMemoryDump', 'Overwrite')
    $paging = Get-ItemProperty -LiteralPath $memoryManagementPath -ErrorAction Stop
    [ordered]@{
        crashDumpEnabled = if ($crash.CrashDumpEnabled.present) {
            [int]$crash.CrashDumpEnabled.value
        } else { 0 }
        dumpFile = if ($crash.DumpFile.present) {
            [string]$crash.DumpFile.value
        } else { '' }
        alwaysKeepMemoryDump = if ($crash.AlwaysKeepMemoryDump.present) {
            [int]$crash.AlwaysKeepMemoryDump.value
        } else { 0 }
        overwrite = if ($crash.Overwrite.present) {
            [int]$crash.Overwrite.value
        } else { 0 }
        automaticManagedPagefile = [bool]$computer.AutomaticManagedPagefile
        pagingFiles = @($paging.PagingFiles)
        totalPhysicalMemoryBytes = [uint64]$computer.TotalPhysicalMemory
        pagefiles = @($pageUsage | ForEach-Object {
                [ordered]@{
                    name = [string]$_.Name
                    allocatedMB = [uint32]$_.AllocatedBaseSize
                    currentUsageMB = [uint32]$_.CurrentUsage
                    peakUsageMB = [uint32]$_.PeakUsage
                }
            })
        policyBackup = [IO.Path]::GetFullPath($StatePath)
        policyBackupPresent = Test-Path -LiteralPath $StatePath -PathType Leaf
    }
}

if ($Mode -eq 'Status') {
    Get-CurrentStatus | ConvertTo-Json -Depth 6
    return
}

Assert-Administrator

$crashNames = @('CrashDumpEnabled', 'DumpFile', 'AlwaysKeepMemoryDump',
    'Overwrite', 'LogEvent', 'AutoReboot', 'FilterPages')
$memoryNames = @('PagingFiles')

if ($Mode -eq 'Restore') {
    $stateFile = Resolve-Path -LiteralPath $StatePath -ErrorAction Stop
    $state = Get-Content -LiteralPath $stateFile.Path -Raw | ConvertFrom-Json
    if ([int]$state.schema -ne 1 -or [string]$state.machine -cne $env:COMPUTERNAME) {
        throw 'Crash-diagnostic backup schema or machine identity does not match this machine.'
    }
    foreach ($name in $crashNames) {
        Set-RegistryValueFromSnapshot -Path $crashControlPath -Name $name `
            -Snapshot $state.crashControl.$name
    }
    foreach ($name in $memoryNames) {
        Set-RegistryValueFromSnapshot -Path $memoryManagementPath -Name $name `
            -Snapshot $state.memoryManagement.$name
    }
    $state | Add-Member -NotePropertyName restoredUtc -NotePropertyValue `
        ([DateTime]::UtcNow.ToString('o')) -Force
    Write-StateFile -State $state
    Write-Host 'The prior crash-dump and pagefile policy is restored. Restart Windows to apply pagefile changes.'
    return
}

if (-not $AcknowledgeDiskUse) {
    throw 'Enabling full crash diagnostics can reserve substantial disk space. Pass -AcknowledgeDiskUse.'
}

$computer = Get-CimInstance Win32_ComputerSystem -ErrorAction Stop
$physicalMB = [uint64][Math]::Ceiling(
    [double]$computer.TotalPhysicalMemory / 1MB)
$requiredPagefileMB = $physicalMB + 300
$dumpTypeValue = switch ($DumpType) {
    'Complete' { 1 }
    'Kernel' { 2 }
    'Automatic' { 7 }
}
$systemDrive = $env:SystemDrive.TrimEnd('\')
$logicalDisk = Get-CimInstance Win32_LogicalDisk -Filter `
    "DeviceID='$systemDrive'" -ErrorAction Stop
if ($DumpType -eq 'Complete') {
    $requiredFreeBytes = ([uint64]$requiredPagefileMB * 2MB) + 10GB
    if ([uint64]$logicalDisk.FreeSpace -lt $requiredFreeBytes) {
        throw "Complete-dump policy needs at least $([Math]::Ceiling($requiredFreeBytes / 1GB)) GB free on $systemDrive for pagefile, dump, and safety headroom."
    }
}

$state = [ordered]@{
    schema = 1
    machine = $env:COMPUTERNAME
    capturedUtc = [DateTime]::UtcNow.ToString('o')
    requestedDumpType = $DumpType
    totalPhysicalMemoryBytes = [uint64]$computer.TotalPhysicalMemory
    crashControl = Get-RegistryValueSnapshot -Path $crashControlPath -Names $crashNames
    memoryManagement = Get-RegistryValueSnapshot -Path $memoryManagementPath -Names $memoryNames
}
Write-NewStateFile -State $state

try {
    New-ItemProperty -LiteralPath $crashControlPath -Name CrashDumpEnabled `
        -Value $dumpTypeValue -PropertyType DWord -Force | Out-Null
    New-ItemProperty -LiteralPath $crashControlPath -Name DumpFile `
        -Value '%SystemRoot%\MEMORY.DMP' -PropertyType ExpandString -Force | Out-Null
    New-ItemProperty -LiteralPath $crashControlPath -Name AlwaysKeepMemoryDump `
        -Value 1 -PropertyType DWord -Force | Out-Null
    New-ItemProperty -LiteralPath $crashControlPath -Name Overwrite `
        -Value 1 -PropertyType DWord -Force | Out-Null
    New-ItemProperty -LiteralPath $crashControlPath -Name LogEvent `
        -Value 1 -PropertyType DWord -Force | Out-Null
    Remove-ItemProperty -LiteralPath $crashControlPath -Name FilterPages `
        -ErrorAction SilentlyContinue
    if ($DumpType -eq 'Complete') {
        $pagingFile = "$systemDrive\pagefile.sys $requiredPagefileMB $requiredPagefileMB"
        New-ItemProperty -LiteralPath $memoryManagementPath -Name PagingFiles `
            -Value @($pagingFile) -PropertyType MultiString -Force | Out-Null
    }
}
catch {
    foreach ($name in $crashNames) {
        Set-RegistryValueFromSnapshot -Path $crashControlPath -Name $name `
            -Snapshot $state.crashControl[$name]
    }
    foreach ($name in $memoryNames) {
        Set-RegistryValueFromSnapshot -Path $memoryManagementPath -Name $name `
            -Snapshot $state.memoryManagement[$name]
    }
    throw
}

Write-Host "$DumpType memory dumps are enabled at %SystemRoot%\MEMORY.DMP."
if ($DumpType -eq 'Complete') {
    Write-Host "The next boot will reserve a $requiredPagefileMB MB system-drive pagefile."
}
Write-Host "Original policy backup: $([IO.Path]::GetFullPath($StatePath))"
Write-Host 'Restart Windows before fault injection so the pagefile and dump policy are active.'

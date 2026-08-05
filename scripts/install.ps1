param(
    [switch]$Yes
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

$viiperVersion = "v0.0.7"
$usbipTargetVersion = [Version]"0.9.7.7"
$installDir = Join-Path $env:LOCALAPPDATA "VIIPER"
$usbipReplacementStatePath = Join-Path $installDir `
    "usbip-replacement-pending.json"
$usbipUninstallKeyName = "{199505b0-b93d-4521-a8c7-897818e0205a}_is1"
$programFilesRoot = if ($env:ProgramW6432) {
    $env:ProgramW6432
}
else {
    $env:ProgramFiles
}
$canonicalUsbipPath = Join-Path $programFilesRoot "USBip\usbip.exe"

Write-Host "VIIPER setup plan:" -ForegroundColor Cyan
Write-Host "  1. Download and install VIIPER first." -ForegroundColor Cyan
Write-Host "  2. Verify or install signed usbip-win2 0.9.7.7." -ForegroundColor Cyan
Write-Host "  3. Verify the USBIP ABI before starting VIIPER." -ForegroundColor Cyan
Write-Host "USB hubs may restart, and Windows may require a reboot." -ForegroundColor Yellow
if (-not $Yes) {
    Write-Host "Save work first. Replacing an incompatible USBIP driver is a two-step process and may require you to restart Windows yourself." -ForegroundColor Yellow
    $answer = Read-Host "Install VIIPER first and continue? [Y/N]"
    if ($answer -notmatch '^(?i:y|yes)$') {
        Write-Host "Setup canceled. No changes were made." -ForegroundColor Yellow
        return
    }
}

$repo = "hbashton/VIIPER"
$apiUrl = "https://api.github.com/repos/$repo/releases/tags/$viiperVersion"

Write-Host "[1/4] Fetching VIIPER release: $viiperVersion..." -ForegroundColor Cyan
$releaseData = Invoke-RestMethod -Uri $apiUrl -ErrorAction Stop
$version = $releaseData.tag_name

if (-not $version) {
    throw "Could not fetch the requested VIIPER release."
}

Write-Host "Version: $version"

$arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else {
    throw "Only 64-bit Windows is supported."
}

if ((Get-CimInstance Win32_ComputerSystem).SystemType -match "ARM") {
    throw "The current hbashton VIIPER package supports Windows x64 only."
}

$preferredAssetNames = @("viiper-windows-$arch.zip", "viiper.exe")
$asset = $null
foreach ($assetName in $preferredAssetNames) {
    $asset = $releaseData.assets | Where-Object { $_.name -eq $assetName } | Select-Object -First 1
    if ($asset) { break }
}

if (-not $asset) {
    $availableAssets = ($releaseData.assets | ForEach-Object { $_.name }) -join ", "
    throw "Release '$version' in $repo does not contain a supported Windows x64 asset. Assets found: $availableAssets"
}

$downloadUrl = $asset.browser_download_url

Write-Host "Downloading from: $downloadUrl"
$tempDir = New-TemporaryFile | ForEach-Object {
    Remove-Item $_
    New-Item -ItemType Directory -Path $_
}
$setupMutex = [Threading.Mutex]::new($false, "Local\DS4Windows-VIIPER-Setup")
$setupMutexAcquired = $false
try {
    try {
        $setupMutexAcquired = $setupMutex.WaitOne(0)
    }
    catch [Threading.AbandonedMutexException] {
        $setupMutexAcquired = $true
    }
    if (-not $setupMutexAcquired) {
        throw "Another DS4Windows/VIIPER setup is already running. Wait for it to finish, then try again."
    }
}
catch {
    $setupMutex.Dispose()
    Remove-Item -Recurse -Force $tempDir -ErrorAction SilentlyContinue
    throw
}

try {
    function Get-ViiperVersion($path) {
        try {
            $help = & $path --help -p
            $match = ($help | Select-String -Pattern "Version:\s*([^\s]+)" -AllMatches | Select-Object -First 1)
            if ($match) {
                return $match.Matches[0].Groups[1].Value
            }
        }
        catch { }
        return $null
    }

    function Parse-VersionOrNull($ver) {
        if (-not $ver) { return $null }
        $clean = $ver.Trim().TrimStart('v', 'V')
        $clean = $clean.Split('-')[0]
        try { return [Version]$clean }
        catch { return $null }
    }

    function Get-WindowsBootSessionId {
        $bootTime = (Get-CimInstance Win32_OperatingSystem -ErrorAction Stop).
            LastBootUpTime
        if ($bootTime -isnot [DateTime]) {
            $bootTime = [Management.ManagementDateTimeConverter]::ToDateTime(
                [string]$bootTime)
        }
        return $bootTime.ToUniversalTime().Ticks.ToString(
            [Globalization.CultureInfo]::InvariantCulture)
    }

    function Set-UsbipReplacementBoundary([Version]$installedVersion,
            [Version]$requiredVersion) {
        New-Item -ItemType Directory -Path $installDir -Force | Out-Null
        [ordered]@{
            BootSessionId = Get-WindowsBootSessionId
            RemovedVersion = $installedVersion.ToString()
            RequiredVersion = $requiredVersion.ToString()
            StartedUtc = [DateTime]::UtcNow.ToString("o")
        } | ConvertTo-Json | Set-Content -LiteralPath `
            $usbipReplacementStatePath -Encoding UTF8
    }

    function Assert-UsbipPostRebootState {
        $activeDevices = @(Get-CimInstance Win32_PnPEntity -ErrorAction Stop |
            Where-Object {
                $_.Service -eq 'usbip2_ude' -or
                $_.PNPDeviceID -eq 'ROOT\USB\0000'
            })
        if ($activeDevices.Count -gt 0) {
            throw "The old usbip2_ude root device is still present " +
                "after reboot. No replacement driver was installed."
        }

        $runningDrivers = @(Get-CimInstance Win32_SystemDriver `
            -ErrorAction Stop | Where-Object {
                $_.State -eq "Running" -and
                $_.Name -match '(?i)^usbip2_(?:ude|filter)$'
            })
        if ($runningDrivers.Count -gt 0) {
            $names = ($runningDrivers | ForEach-Object { $_.Name }) -join ", "
            throw "Old USBIP driver service(s) remain active after reboot: " +
                "$names. No replacement driver was installed."
        }

        $previousErrorActionPreference = $ErrorActionPreference
        $ErrorActionPreference = "Continue"
        try {
            $driverStoreOutput = @(& pnputil.exe /enum-drivers 2>&1)
            $driverStoreExitCode = $LASTEXITCODE
        }
        finally {
            $ErrorActionPreference = $previousErrorActionPreference
        }
        $driverStoreText = ($driverStoreOutput | ForEach-Object {
            [string]$_
        }) -join [Environment]::NewLine
        if ($driverStoreExitCode -ne 0) {
            throw "Could not verify the DriverStore after reboot " +
                "(pnputil exit=$driverStoreExitCode): $driverStoreText"
        }
        if ($driverStoreText -match
                '(?im)\busbip2_(?:ude|filter)\.inf\b') {
            throw "An old usbip2_ude.inf or usbip2_filter.inf package " +
                "remains in the DriverStore. Remove it safely, reboot, then " +
                "run setup again."
        }
    }

    function Resolve-UsbipReplacementBoundary {
        if (-not (Test-Path -LiteralPath $usbipReplacementStatePath)) {
            return
        }

        try {
            $state = Get-Content -LiteralPath $usbipReplacementStatePath `
                -Raw | ConvertFrom-Json
        }
        catch {
            throw "The USBIP replacement state is unreadable. Refusing to " +
                "cross the driver reboot boundary automatically."
        }

        if (-not $state.BootSessionId -or
                $state.BootSessionId -eq (Get-WindowsBootSessionId)) {
            throw "USBIP replacement phase 1 is complete. Restart Windows " +
                "yourself, then run setup again to install 0.9.7.7."
        }

        Write-Host "Validating the required reboot after USBIP removal..." `
            -ForegroundColor Cyan
        Assert-UsbipPostRebootState
        Remove-Item -LiteralPath $usbipReplacementStatePath -Force
        Write-Host "Reboot boundary validated; phase 2 may proceed." `
            -ForegroundColor Green
    }

    function Get-UsbipUninstallEntry([Version]$installedVersion) {
        if (-not $installedVersion) { return $null }

        $matches = @()
        foreach ($basePath in @(
            "HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall",
            "HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall"
        )) {
            $path = Join-Path $basePath $usbipUninstallKeyName
            if (-not (Test-Path -LiteralPath $path)) { continue }
            $entry = Get-ItemProperty -LiteralPath $path -ErrorAction Stop
            $entryVersion = Parse-VersionOrNull `
                ($entry.DisplayVersion -as [string])
            $displayName = $entry.DisplayName -as [string]
            $publisher = $entry.Publisher -as [string]
            $installLocation = $entry.InstallLocation -as [string]
            if (-not [string]::Equals($publisher, "usbip-win2",
                    [StringComparison]::OrdinalIgnoreCase) -or
                    -not [string]::Equals($displayName,
                    "USBip version $installedVersion",
                    [StringComparison]::OrdinalIgnoreCase) -or
                    -not $entryVersion -or
                    $entryVersion -ne $installedVersion) {
                throw "USBIP uninstall metadata does not exactly match " +
                    "installed version $installedVersion. No driver " +
                    "transition was started."
            }
            if ([string]::IsNullOrWhiteSpace($installLocation) -or
                    -not [string]::Equals(
                    [IO.Path]::GetFullPath($installLocation).TrimEnd('\', '/'),
                    [IO.Path]::GetFullPath(
                        (Split-Path -Parent $canonicalUsbipPath)).TrimEnd('\', '/'),
                    [StringComparison]::OrdinalIgnoreCase)) {
                throw "USBIP uninstall metadata points outside its canonical " +
                    "Program Files directory. No driver transition was started."
            }
            $matches += $entry
        }

        if ($matches.Count -gt 1) {
            throw "Multiple uninstall records match usbip-win2 " +
                "$installedVersion. Remove stale metadata manually before " +
                "running setup again."
        }
        return $matches | Select-Object -First 1
    }

    function Get-UsbipUninstallRecords {
        $records = @()
        foreach ($basePath in @(
            "HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall",
            "HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall"
        )) {
            $path = Join-Path $basePath $usbipUninstallKeyName
            if (Test-Path -LiteralPath $path) {
                $records += Get-ItemProperty -LiteralPath $path `
                    -ErrorAction Stop
            }
        }
        return $records
    }

    $tempDownload = Join-Path $tempDir $asset.name
    Invoke-WebRequest -Uri $downloadUrl -OutFile $tempDownload -ErrorAction Stop

    $assetDigest = $asset.digest -as [string]
    $digestMatch = [regex]::Match($assetDigest,
        '^(?i:sha256):(?<hash>[0-9a-f]{64})$')
    if (-not $digestMatch.Success) {
        throw "Release asset '$($asset.name)' has no verifiable SHA-256 digest."
    }
    $downloadHash = (Get-FileHash -LiteralPath $tempDownload `
        -Algorithm SHA256).Hash
    if (-not [string]::Equals($downloadHash,
            $digestMatch.Groups['hash'].Value,
            [StringComparison]::OrdinalIgnoreCase)) {
        throw "VIIPER release asset integrity check failed."
    }

    if ([IO.Path]::GetExtension($asset.name) -eq ".zip") {
        $extractDir = Join-Path $tempDir "release"
        Expand-Archive -LiteralPath $tempDownload -DestinationPath $extractDir -Force
        $tempViiper = Get-ChildItem -Path $extractDir -Recurse -Filter "viiper.exe" |
            Select-Object -First 1 -ExpandProperty FullName
        if (-not $tempViiper) {
            throw "Downloaded VIIPER archive did not contain viiper.exe"
        }
    }
    else {
        $tempViiper = $tempDownload
    }

    function Stop-ControllerBackends {
        $targets = @(Get-Process -Name "DS4Windows", "viiper" `
            -ErrorAction SilentlyContinue)
        if ($targets.Count -eq 0) { return }

        Write-Host "Stopping DS4Windows and VIIPER before the USBIP driver transition..." -ForegroundColor Yellow
        foreach ($process in $targets) {
            try { [void]$process.CloseMainWindow() } catch { }
        }
        Start-Sleep -Milliseconds 750
        foreach ($process in $targets) {
            try {
                if (-not $process.HasExited) {
                    Stop-Process -Id $process.Id -Force -ErrorAction Stop
                }
            }
            catch { }
        }
        Start-Sleep -Milliseconds 500

        $remaining = @(Get-Process -Name "DS4Windows", "viiper" `
            -ErrorAction SilentlyContinue)
        if ($remaining.Count -gt 0) {
            $owners = ($remaining | ForEach-Object {
                "$($_.ProcessName).exe PID=$($_.Id)"
            }) -join ", "
            throw "Could not stop controller backend process(es): $owners. " +
                "Close them manually, then run setup again. The usbip-win2 " +
                "driver transition was not started."
        }
    }

    function Test-ManagedViiperPath([string]$path, [string]$managedPath) {
        if ([string]::IsNullOrWhiteSpace($path)) { return $false }
        try {
            return [string]::Equals(
                [IO.Path]::GetFullPath($path).TrimEnd('\', '/'),
                [IO.Path]::GetFullPath($managedPath).TrimEnd('\', '/'),
                [StringComparison]::OrdinalIgnoreCase)
        }
        catch { return $false }
    }

    function Get-ForeignViiperProcesses([string]$managedPath) {
        return @(Get-CimInstance Win32_Process `
            -Filter "Name='viiper.exe'" -ErrorAction SilentlyContinue |
            Where-Object {
                -not [string]::IsNullOrWhiteSpace(
                    ($_.ExecutablePath -as [string])) -and
                -not (Test-ManagedViiperPath `
                    ($_.ExecutablePath -as [string]) $managedPath)
            })
    }

    function Remove-ForeignViiperInstallations([string]$managedPath,
            $knownForeign = $null) {
        $foreign = if ($null -ne $knownForeign) {
            @($knownForeign)
        }
        else {
            @(Get-ForeignViiperProcesses $managedPath)
        }
        if ($foreign.Count -eq 0) { return }

        Write-Host "Running foreign VIIPER installation(s) were detected:" `
            -ForegroundColor Yellow
        foreach ($process in $foreign) {
            $displayPath = if ($process.ExecutablePath) {
                $process.ExecutablePath
            }
            else { "<path available only after elevation>" }
            Write-Host "  PID=$($process.ProcessId) $displayPath" `
                -ForegroundColor Yellow
        }

        # Never let -Yes silently authorize deletion outside the managed
        # installation directory. This choice is always made by the user.
        $answer = Read-Host (
            "Use administrator rights to stop these processes, remove their " +
            "viiper.exe files, and continue with the managed install? [Y/N]")
        if ($answer -notmatch '^(?i:y|yes)$') {
            throw "Setup stopped. VIIPER will not fall back to a process " +
                "outside '$managedPath'. Close or remove it, then run setup again."
        }

        Disable-ViiperStartup
        $targets = @($foreign | ForEach-Object {
            [ordered]@{
                Pid = [int]$_.ProcessId
                Path = $_.ExecutablePath -as [string]
            }
        })
        $payload = [ordered]@{
            ManagedPath = [IO.Path]::GetFullPath($managedPath)
            Targets = $targets
        } | ConvertTo-Json -Depth 4 -Compress
        $payloadBase64 = [Convert]::ToBase64String(
            [Text.Encoding]::UTF8.GetBytes($payload))

        $helper = @'
$ErrorActionPreference = "Stop"
$payloadJson = [Text.Encoding]::UTF8.GetString(
    [Convert]::FromBase64String("__PAYLOAD__"))
$payload = $payloadJson | ConvertFrom-Json
$managed = [IO.Path]::GetFullPath([string]$payload.ManagedPath).TrimEnd('\', '/')
$paths = [Collections.Generic.HashSet[string]]::new(
    [StringComparer]::OrdinalIgnoreCase)
foreach ($target in @($payload.Targets)) {
    $process = Get-CimInstance Win32_Process -Filter (
        "ProcessId=" + [int]$target.Pid) -ErrorAction SilentlyContinue
    $path = if ($process -and $process.ExecutablePath) {
        [string]$process.ExecutablePath
    }
    else { [string]$target.Path }
    if ([string]::IsNullOrWhiteSpace($path)) { exit 21 }
    $path = [IO.Path]::GetFullPath($path)
    if ([IO.Path]::GetFileName($path) -ine "viiper.exe" -or
            [string]::Equals($path.TrimEnd('\', '/'), $managed,
            [StringComparison]::OrdinalIgnoreCase)) { exit 22 }
    [void]$paths.Add($path)
    if ($process) {
        & taskkill.exe /PID ([int]$target.Pid) /T /F 2>&1 | Out-Null
    }
}
for ($attempt = 1; $attempt -le 12; $attempt++) {
    $remaining = @(Get-CimInstance Win32_Process `
        -Filter "Name='viiper.exe'" -ErrorAction SilentlyContinue |
        Where-Object {
            -not $_.ExecutablePath -or
            -not [string]::Equals(
                ([IO.Path]::GetFullPath(
                    [string]$_.ExecutablePath)).TrimEnd('\', '/'),
                $managed, [StringComparison]::OrdinalIgnoreCase)
        })
    if ($remaining.Count -eq 0) { break }
    foreach ($process in $remaining) {
        if ($process.ExecutablePath) {
            $path = [IO.Path]::GetFullPath([string]$process.ExecutablePath)
            if ([IO.Path]::GetFileName($path) -ine "viiper.exe") { exit 22 }
            [void]$paths.Add($path)
        }
        & taskkill.exe /PID ([int]$process.ProcessId) /T /F 2>&1 | Out-Null
    }
    Start-Sleep -Milliseconds 250
}
$remaining = @(Get-CimInstance Win32_Process `
    -Filter "Name='viiper.exe'" -ErrorAction SilentlyContinue |
    Where-Object {
        -not $_.ExecutablePath -or
        -not [string]::Equals(
            [IO.Path]::GetFullPath([string]$_.ExecutablePath).TrimEnd('\', '/'),
            $managed, [StringComparison]::OrdinalIgnoreCase)
    })
if ($remaining.Count -gt 0) { exit 23 }
foreach ($path in $paths) {
    Remove-Item -LiteralPath $path -Force -ErrorAction Stop
    if (Test-Path -LiteralPath $path) { exit 24 }
}
'@.Replace("__PAYLOAD__", $payloadBase64)
        $encodedHelper = [Convert]::ToBase64String(
            [Text.Encoding]::Unicode.GetBytes($helper))
        Write-Host "Requesting administrator removal of foreign VIIPER..." `
            -ForegroundColor Yellow
        $result = Start-Process powershell.exe -Verb RunAs -PassThru -Wait `
            -ArgumentList @("-NoProfile", "-NonInteractive",
                "-ExecutionPolicy", "Bypass", "-EncodedCommand",
                $encodedHelper)
        if ($result.ExitCode -ne 0) {
            throw "Administrator removal of foreign VIIPER failed with " +
                "code $($result.ExitCode). No managed VIIPER was started."
        }

        $remaining = @(Get-ForeignViiperProcesses $managedPath)
        if ($remaining.Count -gt 0) {
            throw "A foreign VIIPER process returned after administrator " +
                "removal. No managed VIIPER was started."
        }
        Write-Host "Foreign VIIPER installation(s) removed." `
            -ForegroundColor Green
    }

    function Stop-ManagedViiperInstances([string]$managedPath) {
        $running = @(Get-CimInstance Win32_Process `
            -Filter "Name='viiper.exe'" -ErrorAction SilentlyContinue)
        if ($running.Count -eq 0) { return }

        $managedFullPath = [IO.Path]::GetFullPath($managedPath)
        $inventoryPath = Join-Path $tempDir (
            "viiper-inventory-" + [Guid]::NewGuid().ToString("N") + ".json")
        $payload = [ordered]@{
            ManagedPath = $managedFullPath
            InventoryPath = $inventoryPath
        } | ConvertTo-Json -Compress
        $payloadBase64 = [Convert]::ToBase64String(
            [Text.Encoding]::UTF8.GetBytes($payload))
        $helper = @'
$ErrorActionPreference = "Stop"
$payloadJson = [Text.Encoding]::UTF8.GetString(
    [Convert]::FromBase64String("__PAYLOAD__"))
$payload = $payloadJson | ConvertFrom-Json
$managed = [IO.Path]::GetFullPath(
    [string]$payload.ManagedPath).TrimEnd('\', '/')
$inventoryPath = [IO.Path]::GetFullPath([string]$payload.InventoryPath)
$running = @(Get-CimInstance Win32_Process `
    -Filter "Name='viiper.exe'" -ErrorAction Stop)

# Validate the complete process set before terminating anything. A binary from
# another location must go through the installer's explicit removal prompt.
$foreign = @()
foreach ($process in $running) {
    $path = [string]$process.ExecutablePath
    if ([string]::IsNullOrWhiteSpace($path) -or
            -not [string]::Equals(
            [IO.Path]::GetFullPath($path).TrimEnd('\', '/'), $managed,
            [StringComparison]::OrdinalIgnoreCase)) {
        $foreign += [ordered]@{
            ProcessId = [int]$process.ProcessId
            ExecutablePath = $path
        }
    }
}
if ($foreign.Count -gt 0) {
    [ordered]@{ Processes = @($foreign) } | ConvertTo-Json -Depth 4 |
        Set-Content -LiteralPath $inventoryPath -Encoding UTF8
    exit 31
}
foreach ($process in $running) {
    & taskkill.exe /PID ([int]$process.ProcessId) /T /F 2>&1 | Out-Null
}
for ($attempt = 1; $attempt -le 20; $attempt++) {
    $remaining = @(Get-CimInstance Win32_Process `
        -Filter "Name='viiper.exe'" -ErrorAction SilentlyContinue)
    if ($remaining.Count -eq 0) { exit 0 }
    $foreign = @()
    foreach ($process in $remaining) {
        $path = [string]$process.ExecutablePath
        if ([string]::IsNullOrWhiteSpace($path) -or
                -not [string]::Equals(
                [IO.Path]::GetFullPath($path).TrimEnd('\', '/'), $managed,
                [StringComparison]::OrdinalIgnoreCase)) {
            $foreign += [ordered]@{
                ProcessId = [int]$process.ProcessId
                ExecutablePath = $path
            }
        }
    }
    if ($foreign.Count -gt 0) {
        [ordered]@{ Processes = @($foreign) } | ConvertTo-Json -Depth 4 |
            Set-Content -LiteralPath $inventoryPath -Encoding UTF8
        exit 31
    }
    Start-Sleep -Milliseconds 250
}
exit 32
'@.Replace("__PAYLOAD__", $payloadBase64)
        $encodedHelper = [Convert]::ToBase64String(
            [Text.Encoding]::Unicode.GetBytes($helper))

        Write-Host "Stopping the managed VIIPER instance for replacement..." `
            -ForegroundColor Cyan
        $result = Start-Process powershell.exe -Verb RunAs -PassThru -Wait `
            -ArgumentList @("-NoProfile", "-NonInteractive",
                "-ExecutionPolicy", "Bypass", "-EncodedCommand",
                $encodedHelper)
        if ($result.ExitCode -eq 31) {
            if (-not (Test-Path -LiteralPath $inventoryPath -PathType Leaf)) {
                throw "A VIIPER process outside '$managedPath' appeared during setup, but its path could not be verified. No managed binary was replaced."
            }
            $inventory = Get-Content -LiteralPath $inventoryPath -Raw |
                ConvertFrom-Json
            Remove-ForeignViiperInstallations $managedPath `
                @($inventory.Processes)
            Remove-Item -LiteralPath $inventoryPath -Force `
                -ErrorAction SilentlyContinue
            Stop-ManagedViiperInstances $managedPath
            return
        }
        if ($result.ExitCode -ne 0) {
            throw "Administrator shutdown of managed VIIPER failed with code $($result.ExitCode). Close it manually and run setup again."
        }
        $remaining = @(Get-CimInstance Win32_Process `
            -Filter "Name='viiper.exe'" -ErrorAction SilentlyContinue)
        if ($remaining.Count -gt 0) {
            throw "VIIPER is still running after administrator shutdown. No managed binary was replaced."
        }
        Remove-Item -LiteralPath $inventoryPath -Force `
            -ErrorAction SilentlyContinue
    }

    function Get-CanonicalUsbipPath {
        if (Test-Path -LiteralPath $canonicalUsbipPath -PathType Leaf) {
            return $canonicalUsbipPath
        }
        return $null
    }

    function Test-UsbipRuntime([Version]$expectedVersion) {
        $usbipPath = Get-CanonicalUsbipPath
        if (-not $usbipPath) {
            return [pscustomobject]@{
                Healthy = $false
                Reason = "missing"
                Detail = "canonical usbip.exe is missing"
            }
        }

        $actualVersion = $null
        try { $actualVersion = [Version](Get-Item -LiteralPath $usbipPath).VersionInfo.FileVersion }
        catch { }
        if (-not $actualVersion -or $actualVersion -ne $expectedVersion) {
            return [pscustomobject]@{
                Healthy = $false
                Reason = "version"
                Detail = "usbip.exe version is '$actualVersion'; expected $expectedVersion"
            }
        }

        $previousErrorActionPreference = $ErrorActionPreference
        $ErrorActionPreference = "Continue"
        try {
            $output = & $usbipPath port 2>&1
        }
        finally {
            $ErrorActionPreference = $previousErrorActionPreference
        }
        $exitCode = $LASTEXITCODE
        $text = ($output | Out-String).Trim()
        if ($text -match "(?i)ABI\s+mismatch|unexpected\s+size|specified\s+conversion\s+is\s+not\s+valid|invalid\s+structure\s+size") {
            return [pscustomobject]@{
                Healthy = $false
                Reason = "abi-mismatch"
                Detail = "usbip.exe port failed (exit $exitCode): $text"
            }
        }

        if ($exitCode -ne 0) {
            return [pscustomobject]@{
                Healthy = $false
                Reason = "probe-failed"
                Detail = "usbip.exe port failed (exit $exitCode): $text"
            }
        }

        return [pscustomobject]@{
            Healthy = $true
            Reason = "ready"
            Detail = "0.9.7.7 ABI probe passed"
        }
    }

    function Get-UsbipPortBlocks([string]$portText) {
        $blocks = @()
        $currentPort = $null
        $currentLines = [Collections.Generic.List[string]]::new()
        foreach ($line in ($portText -split "`r?`n")) {
            $header = [regex]::Match($line, '(?i)^\s*Port\s+(\d+):')
            if ($header.Success) {
                if ($null -ne $currentPort) {
                    $blocks += [pscustomobject]@{
                        Port = $currentPort
                        Text = $currentLines -join [Environment]::NewLine
                    }
                }
                $currentPort = [int]$header.Groups[1].Value
                $currentLines = [Collections.Generic.List[string]]::new()
            }
            if ($null -ne $currentPort) { $currentLines.Add($line) }
        }
        if ($null -ne $currentPort) {
            $blocks += [pscustomobject]@{
                Port = $currentPort
                Text = $currentLines -join [Environment]::NewLine
            }
        }
        return $blocks
    }

    function Test-ViiperOwnedUsbipPort([string]$block) {
        $location = [regex]::Match(
            $block, '(?im)^\s*->\s+(?<uri>usbip://\S+)\s*$')
        if (-not $location.Success) { return $false }

        $uri = $null
        if (-not [Uri]::TryCreate($location.Groups['uri'].Value,
                [UriKind]::Absolute, [ref]$uri)) {
            return $false
        }
        $hostIsLoopback = $uri.Host -ieq "localhost"
        $address = $null
        if ([Net.IPAddress]::TryParse($uri.Host.Trim('[', ']'),
                [ref]$address)) {
            $hostIsLoopback = [Net.IPAddress]::IsLoopback($address)
        }
        $remoteBusId = $uri.AbsolutePath.Trim('/')
        if ($uri.Scheme -ine "usbip" -or $uri.Port -ne 3241 -or
                -not $hostIsLoopback -or
                $remoteBusId -notmatch '^\d+-\d+$') {
            return $false
        }

        $serialLine = [regex]::Match(
            $block, '(?im)^\s*->\s+serial\b(?<value>.*)$')
        if (-not $serialLine.Success -or
                [string]::IsNullOrWhiteSpace(
                    $serialLine.Groups['value'].Value.Trim(" '"))) {
            # usbip-win2 0.9.7.7 has no attach-time owner serial. The exact
            # loopback VIIPER endpoint and bus/device tuple are its identity.
            return $true
        }

        $serial = [regex]::Match(
            $serialLine.Groups['value'].Value,
            "^\s*'(?<serial>[^']+)'\s*$")
        return $serial.Success -and
            $serial.Groups['serial'].Value -cmatch '^DS4W[0-9A-Fa-f]{11}$'
    }

    function Disconnect-UsbipImports([string]$usbipPath) {
        if (-not $usbipPath -or
                -not (Test-Path -LiteralPath $usbipPath -PathType Leaf)) {
            return
        }

        $previousErrorActionPreference = $ErrorActionPreference
        $ErrorActionPreference = "Continue"
        try {
            $portOutput = @(& $usbipPath port 2>&1)
            $portExitCode = $LASTEXITCODE
        }
        finally { $ErrorActionPreference = $previousErrorActionPreference }
        $portText = ($portOutput | ForEach-Object { [string]$_ }) -join `
            [Environment]::NewLine
        if ($portExitCode -ne 0) {
            $detail = if ($portText) { $portText.Trim() }
                else { "no diagnostic output" }
            throw "Cannot safely inspect USBIP imports " +
                "(exit=$portExitCode): $detail. No driver transition was started."
        }

        $blocks = @(Get-UsbipPortBlocks $portText)
        $owned = @($blocks | Where-Object {
            Test-ViiperOwnedUsbipPort $_.Text
        })
        $foreign = @($blocks | Where-Object {
            -not (Test-ViiperOwnedUsbipPort $_.Text)
        })
        if ($foreign.Count -gt 0) {
            $ports = ($foreign | ForEach-Object { $_.Port }) -join ", "
            throw "USBIP port(s) $ports are not exact local VIIPER " +
                "imports. Close their owning application or detach them " +
                "manually; setup changed no imports."
        }

        foreach ($block in $owned) {
            $port = $block.Port
            Write-Host "Detaching exact local VIIPER import on port $port..." `
                -ForegroundColor Yellow
            $previousErrorActionPreference = $ErrorActionPreference
            $ErrorActionPreference = "Continue"
            try {
                $detachOutput = @(& $usbipPath detach -p $port 2>&1)
                $detachExitCode = $LASTEXITCODE
            }
            finally { $ErrorActionPreference = $previousErrorActionPreference }
            if ($detachExitCode -ne 0) {
                $detail = ($detachOutput | ForEach-Object {
                    [string]$_
                }) -join [Environment]::NewLine
                throw "Could not detach VIIPER USBIP port $port " +
                    "(exit=$detachExitCode): $detail"
            }
        }
    }

    function Remove-MismatchedUsbipPackage($entry,
            [Version]$installedVersion, [Version]$targetVersion) {
        if (-not $installedVersion -or $installedVersion -eq $targetVersion) {
            return $false
        }
        if (-not $entry) {
            throw "USBIP $installedVersion is unsupported, but no exact " +
                "uninstall record exists. Refusing to overlay " +
                "$targetVersion. Remove the package manually, reboot, then " +
                "run setup again."
        }

        $entryVersion = Parse-VersionOrNull ($entry.DisplayVersion -as [string])
        if (-not $entryVersion -or $entryVersion -ne $installedVersion) {
            throw "The selected USBIP uninstall record does not exactly " +
                "match installed version $installedVersion."
        }

        $uninstallCommand = $entry.QuietUninstallString -as [string]
        if (-not $uninstallCommand) {
            $uninstallCommand = $entry.UninstallString -as [string]
        }
        if (-not $uninstallCommand) {
            throw "USBIP $installedVersion must be replaced with " +
                "$targetVersion, but its uninstall command is unavailable."
        }

        $match = [regex]::Match($uninstallCommand,
            '^\s*(?:"(?<exe>[^"]+)"|(?<exe>\S+))(?:\s+(?<args>.*))?$')
        if (-not $match.Success) {
            throw "Could not parse the installed USBIP uninstall command."
        }
        $uninstaller = $match.Groups['exe'].Value
        if (-not (Test-Path -LiteralPath $uninstaller -PathType Leaf)) {
            throw "The installed USBIP uninstaller is missing: $uninstaller"
        }

        $canonicalUsbipDir = Split-Path -Parent $canonicalUsbipPath
        $uninstallerDir = Split-Path -Parent (
            (Resolve-Path -LiteralPath $uninstaller).Path)
        if (-not [string]::Equals(
                [IO.Path]::GetFullPath($uninstallerDir).TrimEnd('\'),
                [IO.Path]::GetFullPath($canonicalUsbipDir).TrimEnd('\'),
                [StringComparison]::OrdinalIgnoreCase)) {
            throw "The exact-version USBIP uninstall record points outside " +
                "the canonical install directory: $uninstaller"
        }

        Write-Host (
            "USBIP replacement phase 1 of 2: removing unsupported " +
            "$installedVersion. Version $targetVersion will be installed " +
            "only after you restart Windows and run setup again."
        ) -ForegroundColor Yellow
        Set-UsbipReplacementBoundary $installedVersion $targetVersion
        try {
            $uninstall = Start-Process -FilePath $uninstaller `
                -ArgumentList "/VERYSILENT /SUPPRESSMSGBOXES /NORESTART /RESTARTEXITCODE=3010" `
                -Verb RunAs -PassThru
        }
        catch {
            Remove-Item -LiteralPath $usbipReplacementStatePath -Force `
                -ErrorAction SilentlyContinue
            throw
        }
        if (-not $uninstall.WaitForExit(30000)) {
            Write-Host (
                "The USBIP uninstaller is still finishing in the background. " +
                "It was intentionally left running. Save your work, restart " +
                "Windows yourself, then run setup again."
            ) -ForegroundColor Yellow
            return $true
        }
        $uninstall.Refresh()
        if ($uninstall.ExitCode -notin @(0, 1641, 3010)) {
            throw "USBIP uninstall returned code $($uninstall.ExitCode). " +
                "Its replacement marker was retained because the driver may " +
                "be partially removed. Restart Windows before attempting " +
                "any repair."
        }

        Write-Host "USBIP removal finished. Restart Windows yourself, then run setup again for phase 2." `
            -ForegroundColor Yellow
        return $true
    }

    function Disable-ViiperStartup {
        $task = Get-ScheduledTask -TaskName "RunVIIPER" `
            -ErrorAction SilentlyContinue
        if ($task) {
            try {
                Unregister-ScheduledTask -TaskName "RunVIIPER" `
                    -Confirm:$false -ErrorAction Stop
            }
            catch {
                $delete = Start-Process -FilePath "schtasks.exe" `
                    -ArgumentList '/Delete /TN "RunVIIPER" /F' `
                    -Verb RunAs -PassThru -Wait -WindowStyle Hidden
                if ($delete.ExitCode -ne 0) {
                    throw "Could not disable the existing RunVIIPER task " +
                        "before the USBIP driver transition."
                }
            }
        }
        try {
            Remove-ItemProperty `
                -LiteralPath "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" `
                -Name "VIIPER" -ErrorAction Stop
        }
        catch [System.Management.Automation.PSArgumentException] { }
        catch [System.Management.Automation.ItemNotFoundException] { }

        if (Get-ScheduledTask -TaskName "RunVIIPER" `
                -ErrorAction SilentlyContinue) {
            throw "RunVIIPER startup remains enabled. No USBIP driver " +
                "transition was started."
        }
        $runValue = Get-ItemPropertyValue `
            -LiteralPath "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" `
            -Name "VIIPER" -ErrorAction SilentlyContinue
        if ($null -ne $runValue) {
            throw "The VIIPER Run entry remains enabled. No USBIP driver " +
                "transition was started."
        }
        Write-Host "VIIPER startup is disabled until USBIP passes its ABI check." `
            -ForegroundColor Green
    }

    function Invoke-ViiperInstallRegistration([string]$path) {
        # `viiper install` launches the persistent server before the brief
        # registration process exits. Start-Process -Wait follows descendants
        # and hangs on that server, so wait only on the registration PID.
        $registration = Start-Process -WindowStyle Hidden $path `
            -ArgumentList "install" -PassThru
        if (-not $registration.WaitForExit(15000)) {
            try {
                & taskkill.exe /PID $registration.Id /T /F | Out-Null
            }
            catch { }
            throw "VIIPER registration did not finish within 15 seconds. Its process tree was stopped safely; run setup again."
        }

        $registration.Refresh()
        if ($registration.ExitCode -ne 0) {
            throw "VIIPER registration failed with exit code $($registration.ExitCode)."
        }

        if (Get-ScheduledTask -TaskName "RunVIIPER" `
                -ErrorAction SilentlyContinue) {
            throw "RunVIIPER was recreated alongside the VIIPER Run entry. Refusing duplicate startup ownership."
        }
        $runValue = Get-ItemPropertyValue `
            -LiteralPath "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" `
            -Name "VIIPER" -ErrorAction SilentlyContinue
        $expectedPrefix = '"' + [IO.Path]::GetFullPath($path) + '" server '
        if ([string]::IsNullOrWhiteSpace($runValue) -or
                -not ([string]$runValue).StartsWith($expectedPrefix,
                [StringComparison]::OrdinalIgnoreCase)) {
            throw "VIIPER startup registration does not target the managed binary: '$runValue'"
        }

        Start-Sleep -Milliseconds 500
        $managedInstances = @(Get-CimInstance Win32_Process `
            -Filter "Name='viiper.exe'" -ErrorAction SilentlyContinue |
            Where-Object {
                Test-ManagedViiperPath `
                    ($_.ExecutablePath -as [string]) $path
            })
        if ($managedInstances.Count -ne 1) {
            throw "Expected one managed VIIPER server after registration; found $($managedInstances.Count)."
        }
    }

    $newVersion = Get-ViiperVersion $tempViiper
    if (-not $newVersion) { $newVersion = "unknown" }
    Write-Host "Downloaded VIIPER version: $newVersion"
    
    $installPath = Join-Path $installDir "viiper.exe"
    Remove-ForeignViiperInstallations $installPath
    Disable-ViiperStartup
    Stop-ManagedViiperInstances $installPath
    $isUpdate = Test-Path $installPath
    $skipInstall = $false

    $oldVersion = "unknown"
    if ($isUpdate) {
        Write-Host "Existing VIIPER installation detected."
        $oldVersionRaw = Get-ViiperVersion $installPath
        if ($oldVersionRaw) { $oldVersion = $oldVersionRaw }
        Write-Host "Installed VIIPER version: $oldVersion"

        $newV = Parse-VersionOrNull $newVersion
        $oldV = Parse-VersionOrNull $oldVersion

        $sameBinary = $false
        try {
            $sameBinary = (Get-Item -LiteralPath $tempViiper).Length -eq
                    (Get-Item -LiteralPath $installPath).Length -and
                (Get-FileHash -LiteralPath $tempViiper -Algorithm SHA256).Hash `
                    -eq (Get-FileHash -LiteralPath $installPath `
                    -Algorithm SHA256).Hash
        }
        catch { }

        if ($sameBinary) {
            Write-Host "Installed VIIPER binary already matches the release asset."
            $skipInstall = $true
        }
        elseif ($newV -and $oldV -and $newV -lt $oldV) {
            Write-Host "Detected potential downgrade (installed: $oldVersion, new: $newVersion). Skipping install." -ForegroundColor Yellow
            $skipInstall = $true
        }
    }
    
    if (-not $skipInstall) {
        Write-Host "Installing binary to $installPath..."
        New-Item -ItemType Directory -Path $installDir -Force | Out-Null

        Copy-Item $tempViiper $installPath -Force
    }

    Write-Host ""
    Write-Host "[2/4] VIIPER binary installed." -ForegroundColor Green
    Write-Host "[3/4] Checking USBIP drivers..." -ForegroundColor Cyan

    Resolve-UsbipReplacementBoundary

    $canonicalUsbipPresent = Test-Path -LiteralPath $canonicalUsbipPath `
        -PathType Leaf
    $driverPath = Join-Path $env:SystemRoot `
        "System32\drivers\usbip2_ude.sys"
    $usbipDriverPresent = Test-Path -LiteralPath $driverPath -PathType Leaf
    $usbipInstalledVersion = $null

    if ($canonicalUsbipPresent) {
        $usbipInstalledVersion = Parse-VersionOrNull (
            (Get-Item -LiteralPath $canonicalUsbipPath).
                VersionInfo.ProductVersion)
    }
    if (-not $usbipInstalledVersion -and $usbipDriverPresent) {
        $usbipInstalledVersion = Parse-VersionOrNull (
            (Get-Item -LiteralPath $driverPath).VersionInfo.FileVersion)
    }

    $usbipRecords = @(Get-UsbipUninstallRecords)
    if ($usbipRecords.Count -gt 1) {
        throw "Multiple usbip-win2 uninstall records exist. Refusing an " +
            "ambiguous driver transition; remove stale metadata manually."
    }
    $usbipEntry = $usbipRecords | Select-Object -First 1
    $metadataVersion = $null
    if ($usbipEntry) {
        $displayName = $usbipEntry.DisplayName -as [string]
        $publisher = $usbipEntry.Publisher -as [string]
        $installLocation = $usbipEntry.InstallLocation -as [string]
        $metadataVersion = Parse-VersionOrNull (
            $usbipEntry.DisplayVersion -as [string])
        if (-not $metadataVersion -or
                -not [string]::Equals($publisher, "usbip-win2",
                [StringComparison]::OrdinalIgnoreCase) -or
                -not [string]::Equals($displayName,
                "USBip version $metadataVersion",
                [StringComparison]::OrdinalIgnoreCase) -or
                [string]::IsNullOrWhiteSpace($installLocation) -or
                -not [string]::Equals(
                [IO.Path]::GetFullPath($installLocation).TrimEnd('\', '/'),
                [IO.Path]::GetFullPath(
                    (Split-Path -Parent $canonicalUsbipPath)).TrimEnd('\', '/'),
                [StringComparison]::OrdinalIgnoreCase)) {
            throw "USBIP uninstall metadata is unknown or malformed. No " +
                "driver transition was started."
        }
    }
    if (-not $usbipInstalledVersion -and $metadataVersion) {
        $usbipInstalledVersion = $metadataVersion
    }
    if (($canonicalUsbipPresent -or $usbipDriverPresent -or $usbipEntry) -and
            -not $usbipInstalledVersion) {
        throw "USBIP files or metadata exist, but their installed version " +
            "cannot be read. Refusing an unknown driver ABI."
    }
    if ($metadataVersion -and $usbipInstalledVersion -and
            $metadataVersion -ne $usbipInstalledVersion) {
        throw "USBIP uninstall metadata version $metadataVersion does not " +
            "match installed runtime version $usbipInstalledVersion."
    }

    $needsReboot = $false
    $driverReplacementPhaseOne = $false
    $usbipRuntimeBefore = Test-UsbipRuntime $usbipTargetVersion
    $sameVersionProbeFailure = -not $usbipRuntimeBefore.Healthy -and
        $usbipInstalledVersion -eq $usbipTargetVersion
    $needsUsbipInstall = -not $usbipRuntimeBefore.Healthy -and
        -not $sameVersionProbeFailure
    if ($sameVersionProbeFailure) {
        $needsReboot = $true
        Disable-ViiperStartup
        Stop-ControllerBackends
    }

    if (-not $needsUsbipInstall) {
        if ($usbipRuntimeBefore.Healthy) {
            Write-Host (
                "USBIP 0.9.7.7 userspace/driver ABI is ready at the canonical " +
                "Program Files path."
            ) -ForegroundColor Green
        }
        else {
            Write-Host (
                "USBIP 0.9.7.7 is installed, but its live ABI probe failed: " +
                "$($usbipRuntimeBefore.Detail). Setup will not overlay a " +
                "running kernel driver. Restart Windows, then run setup " +
                "again. If the probe still fails, uninstall 0.9.7.7, " +
                "restart, and rerun setup."
            ) -ForegroundColor Yellow
        }
    }
    elseif ($usbipInstalledVersion -and
            $usbipInstalledVersion -lt $usbipTargetVersion) {
        Write-Host (
            "USBIP drivers outdated (installed: $usbipInstalledVersion, " +
            "required: $usbipTargetVersion). Updating..."
        ) -ForegroundColor Yellow
    }
    elseif ($usbipInstalledVersion) {
        Write-Host (
            "USBIP reports version $usbipInstalledVersion, but its canonical " +
            "0.9.7.7 runtime is not usable ($($usbipRuntimeBefore.Detail)). " +
            "Repairing the exact 0.9.7.7 package..."
        ) -ForegroundColor Yellow
    }
    else {
        Write-Host (
            "USBIP 0.9.7.7 is missing or unusable " +
            "($($usbipRuntimeBefore.Detail)). Installing the exact package..."
        ) -ForegroundColor Yellow
    }

    if ($needsUsbipInstall) {
        Write-Host "This requires administrator privileges." -ForegroundColor Yellow

        Disable-ViiperStartup
        Stop-ControllerBackends

        try {
            if (($canonicalUsbipPresent -or $usbipDriverPresent -or
                    $usbipEntry) -and -not $canonicalUsbipPresent) {
                throw "An existing USBIP installation has no canonical " +
                    "usbip.exe, so active imports cannot be inspected safely."
            }
            Disconnect-UsbipImports (Get-CanonicalUsbipPath)

            if ($usbipInstalledVersion -and
                    $usbipInstalledVersion -ne $usbipTargetVersion) {
                $usbipEntry = Get-UsbipUninstallEntry `
                    $usbipInstalledVersion
            }
            $removedMismatchedPackage = Remove-MismatchedUsbipPackage `
                $usbipEntry $usbipInstalledVersion $usbipTargetVersion

            if ($removedMismatchedPackage) {
                $driverReplacementPhaseOne = $true
                $needsReboot = $true
                Write-Host (
                    "USBIP replacement phase 1 of 2 is complete. " +
                    "$usbipTargetVersion was intentionally not installed " +
                    "in this Windows session."
                ) -ForegroundColor Yellow
            }
            else {
                $usbipInstallerUrl = "https://github.com/vadimgrn/usbip-win2/releases/download/v.0.9.7.7/USBip-0.9.7.7-x64.exe"
                $usbipInstaller = Join-Path $tempDir "USBip-setup.exe"
                Write-Host "  Downloading exact usbip-win2 0.9.7.7..." `
                    -ForegroundColor Cyan
                Invoke-WebRequest -Uri $usbipInstallerUrl `
                    -OutFile $usbipInstaller -ErrorAction Stop
                $usbipInstallerHash = (Get-FileHash `
                    -LiteralPath $usbipInstaller -Algorithm SHA256).
                    Hash.ToLowerInvariant()
                if ($usbipInstallerHash -ne
                        "51620fa5f9f8be5932bc9d786deee557ce06d5407a99cab490dcfac71f185fea") {
                    throw "USBIP installer integrity check failed."
                }

                Write-Host "Installing USBIP drivers (UAC prompt will appear)..." `
                    -ForegroundColor Yellow
                $installerProcess = Start-Process `
                    -FilePath $usbipInstaller `
                    -ArgumentList "/VERYSILENT /SUPPRESSMSGBOXES /NORESTART /RESTARTEXITCODE=3010" `
                    -Verb RunAs -PassThru
                if (-not $installerProcess.WaitForExit(120000)) {
                    $needsReboot = $true
                    Write-Host (
                        "USBIP setup is still finishing in the background. " +
                        "It was left running; restart Windows yourself " +
                        "before starting VIIPER."
                    ) -ForegroundColor Yellow
                }
                else {
                    $installerProcess.Refresh()
                    if ($installerProcess.ExitCode -notin @(0, 1641, 3010)) {
                        throw "USBIP installer exited with code " +
                            "$($installerProcess.ExitCode)."
                    }
                    Write-Host "USBIP drivers installed successfully." `
                        -ForegroundColor Green
                    $needsReboot = $installerProcess.ExitCode -in `
                        @(1641, 3010)
                }
            }
        }
        catch {
            throw "Failed to install USBIP drivers: $($_.Exception.Message)"
        }
    }

    Write-Host "[4/4] Verifying runtime readiness..." -ForegroundColor Cyan
    $usbipRuntime = if ($driverReplacementPhaseOne -or $needsReboot) {
        [pscustomobject]@{
            Healthy = $false
            Reason = "reboot-required"
            Detail = "a user-controlled reboot is required"
        }
    }
    else {
        Test-UsbipRuntime $usbipTargetVersion
    }
    if (-not $usbipRuntime.Healthy -and -not $needsReboot) {
        $needsReboot = $true
        Write-Host "USBIP 0.9.7.7 is installed but not active yet: $($usbipRuntime.Detail)" -ForegroundColor Yellow
    }

    if ($usbipRuntime.Healthy -and -not $needsReboot) {
        if (-not $isUpdate) {
            Write-Host "Configuring system startup..."
        }
        Invoke-ViiperInstallRegistration $installPath
    }
    
    if ($usbipRuntime.Healthy -and -not $needsReboot) {
        Write-Host "VIIPER installed successfully and is ready." `
            -ForegroundColor Green
    }
    else {
        Write-Host "VIIPER binary installed; setup is not Ready yet." `
            -ForegroundColor Yellow
    }
    Write-Host "Binary installed to: $installPath"
    if ($isUpdate) {
        if ($skipInstall) {
            Write-Host "Binary already at correct version or newer. Skipping binary copy."
        }
        else {
            Write-Host "VIIPER binary update complete."
        }
        if (-not $needsReboot) {
            Write-Host "VIIPER service has been restarted."
        }
    }
    else {
        if (-not $needsReboot) {
            Write-Host "VIIPER server is now running and will start automatically on boot."
        }
    }

    if ($needsReboot) {
        Write-Host ""
        Write-Host "USBIP 0.9.7.7 requires a restart before VIIPER can run safely." -ForegroundColor Yellow
        Write-Host "VIIPER startup remains disabled. Restart Windows yourself, then run this setup again." -ForegroundColor Yellow
    }
    # `viiper install` above starts the single verified server instance.
}
finally {
    Remove-Item -Recurse -Force $tempDir -ErrorAction SilentlyContinue
    if ($setupMutexAcquired) {
        try { $setupMutex.ReleaseMutex() } catch { }
    }
    $setupMutex.Dispose()
}

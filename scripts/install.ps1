$ErrorActionPreference = "Stop"

$viiperVersion = "dev-snapshot"

$repo = "hbashton/VIIPER"
$apiUrl = if ($viiperVersion -eq "dev-snapshot" -or $viiperVersion -eq "latest") {
    "https://api.github.com/repos/$repo/releases/latest"
}
else {
    "https://api.github.com/repos/$repo/releases/tags/$viiperVersion"
}

Write-Host "Fetching VIIPER release: $viiperVersion..."
$releaseData = Invoke-RestMethod -Uri $apiUrl -ErrorAction Stop
$version = $releaseData.tag_name

if (-not $version) {
    Write-Host "Error: Could not fetch VIIPER release" -ForegroundColor Red
    exit 1
}

Write-Host "Version: $version"

$arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else {
    Write-Host "Error: Only 64-bit Windows is supported" -ForegroundColor Red
    exit 1
}

if ((Get-CimInstance Win32_ComputerSystem).SystemType -match "ARM") {
    Write-Host "Error: The current hbashton VIIPER package supports Windows x64 only" -ForegroundColor Red
    exit 1
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
$tempDir = New-TemporaryFile | ForEach-Object { Remove-Item $_; New-Item -ItemType Directory -Path $_ }

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

    $tempDownload = Join-Path $tempDir $asset.name
    Invoke-WebRequest -Uri $downloadUrl -OutFile $tempDownload -ErrorAction Stop

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

    function Get-RunningPadSenseProcesses {
        try {
            return @(Get-CimInstance Win32_Process -ErrorAction Stop |
                Where-Object { $_.Name -match "^PadSense.*\.exe$" })
        }
        catch {
            return @(Get-Process -ErrorAction SilentlyContinue |
                Where-Object { $_.ProcessName -match "^PadSense" } |
                ForEach-Object {
                    [pscustomobject]@{
                        Name = "$($_.ProcessName).exe"
                        ProcessId = $_.Id
                    }
                })
        }
    }

    function Assert-PadSenseNotRunning {
        $processes = @(Get-RunningPadSenseProcesses)
        if ($processes.Count -eq 0) { return }

        $owners = ($processes | ForEach-Object {
            "$($_.Name) PID=$($_.ProcessId)"
        }) -join ", "
        throw "PadSense is running ($owners) and may own active USBIP " +
            "imports. Close PadSense completely, including its " +
            "tray/background process, then run setup again. The usbip-win2 " +
            "driver transition was not started."
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

    function Get-CanonicalUsbipPath {
        foreach ($root in @($env:ProgramW6432, $env:ProgramFiles, ${env:ProgramFiles(x86)})) {
            if (-not $root) { continue }
            $candidate = Join-Path $root "USBip\usbip.exe"
            if (Test-Path -LiteralPath $candidate -PathType Leaf) {
                return $candidate
            }
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
        if ($text -match "(?i)ABI\s+mismatch|unexpected\s+size.*(?:input|structure)") {
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

    function Disconnect-UsbipImports([string]$usbipPath) {
        if (-not $usbipPath -or
                -not (Test-Path -LiteralPath $usbipPath -PathType Leaf)) {
            return
        }

        $previousErrorActionPreference = $ErrorActionPreference
        $ErrorActionPreference = "Continue"
        try { $portOutput = @(& $usbipPath port 2>&1) }
        finally { $ErrorActionPreference = $previousErrorActionPreference }
        $portText = ($portOutput | ForEach-Object { [string]$_ }) -join `
            [Environment]::NewLine
        foreach ($match in [regex]::Matches($portText,
                '(?im)^\s*Port\s+(\d+):')) {
            $port = [int]$match.Groups[1].Value
            Write-Host "Detaching stopped USBIP import on port $port..." `
                -ForegroundColor Yellow
            $previousErrorActionPreference = $ErrorActionPreference
            $ErrorActionPreference = "Continue"
            try { & $usbipPath detach -p $port 2>&1 | Out-Null }
            finally { $ErrorActionPreference = $previousErrorActionPreference }
        }
    }

    function Remove-MismatchedUsbipPackage($entry,
            [Version]$installedVersion, [Version]$targetVersion) {
        if (-not $entry -or -not $installedVersion -or
                $installedVersion -eq $targetVersion) {
            return $false
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

        Write-Host (
            "Removing unsupported USBIP $installedVersion before " +
            "installing pinned safe $targetVersion..."
        ) -ForegroundColor Yellow
        $uninstall = Start-Process -FilePath $uninstaller `
            -ArgumentList "/VERYSILENT /SUPPRESSMSGBOXES /NORESTART" `
            -Verb RunAs -PassThru
        if (-not $uninstall.WaitForExit(30000)) {
            try {
                Start-Process taskkill.exe -Verb RunAs `
                    -ArgumentList "/PID $($uninstall.Id) /T /F" `
                    -WindowStyle Hidden -Wait | Out-Null
            }
            catch { }
            throw "USBIP could not unload its active kernel driver within " +
                "30 seconds. Restart Windows, then run setup again; it will " +
                "resume with pinned $targetVersion."
        }
        $uninstall.Refresh()
        if ($uninstall.ExitCode -notin @(0, 1641, 3010)) {
            throw "USBIP uninstall failed with code $($uninstall.ExitCode)."
        }

        return $true
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
    }

    $newVersion = Get-ViiperVersion $tempViiper
    if (-not $newVersion) { $newVersion = "unknown" }
    Write-Host "Downloaded VIIPER version: $newVersion"
    
    $installDir = Join-Path $env:LOCALAPPDATA "VIIPER"
    $installPath = Join-Path $installDir "viiper.exe"
    $isUpdate = Test-Path $installPath
    $skipInstall = $false

    $oldVersion = "unknown"
    if ($isUpdate) {
        Write-Host "Existing VIIPER installation detected. Preserving startup/autostart configuration..."
        $oldVersionRaw = Get-ViiperVersion $installPath
        if ($oldVersionRaw) { $oldVersion = $oldVersionRaw }
        Write-Host "Installed VIIPER version: $oldVersion"

        $newV = Parse-VersionOrNull $newVersion
        $oldV = Parse-VersionOrNull $oldVersion

        if ($newVersion -eq $oldVersion -and $newVersion -ne "unknown") {
            Write-Host "Versions are identical. Skipping VIIPER install step."
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

        if ($isUpdate) {
            $procs = Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
            Where-Object { $_.ExecutablePath -eq $installPath }
            if ($procs) {
                Write-Host "Stopping running VIIPER instance(s) so the binary can be updated..."
                foreach ($p in $procs) {
                    try {
                        Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
                    }
                    catch { }
                }
                Start-Sleep -Milliseconds 500
            }
        }

        Copy-Item $tempViiper $installPath -Force
    }

    Write-Host ""
    Write-Host "Checking USBIP drivers..." -ForegroundColor Cyan

    $usbipTargetVersion = [Version]"0.9.7.7"
    $usbipInstalledVersion = $null

    $usbipEntry = Get-ItemProperty "HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*" -ErrorAction SilentlyContinue |
        Where-Object { $_.DisplayName -like 'USBip version*' } |
        Select-Object -First 1
    if (-not $usbipEntry) {
        $usbipEntry = Get-ItemProperty "HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*" -ErrorAction SilentlyContinue |
            Where-Object { $_.DisplayName -like 'USBip version*' } |
            Select-Object -First 1
    }
    if ($usbipEntry) {
        try { $usbipInstalledVersion = [Version]$usbipEntry.DisplayVersion } catch { }
    }

    if (-not $usbipInstalledVersion) {
        $driverPath = Join-Path $env:SystemRoot "System32\drivers\usbip2_ude.sys"
        if (Test-Path $driverPath) {
            try { $usbipInstalledVersion = [Version](Get-Item $driverPath).VersionInfo.FileVersion } catch { }
        }
    }

    $needsReboot = $false
    $usbipRuntimeBefore = Test-UsbipRuntime $usbipTargetVersion
    $needsUsbipInstall = -not $usbipRuntimeBefore.Healthy -and
        $usbipRuntimeBefore.Reason -ne "abi-mismatch"
    if ($usbipRuntimeBefore.Reason -eq "abi-mismatch") {
        $needsReboot = $true
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
                "USBIP 0.9.7.7 is already installed. Skipping a redundant " +
                "reinstall; restart Windows to load its matching driver."
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

        Assert-PadSenseNotRunning
        Stop-ControllerBackends

        $usbipInstallerUrl = "https://github.com/vadimgrn/usbip-win2/releases/download/v.0.9.7.7/USBip-0.9.7.7-x64.exe"
        $usbipInstaller = Join-Path $tempDir "USBip-setup.exe"

        try {
            Write-Host "  Downloading usbip-win2 installer..." -ForegroundColor Cyan
            Invoke-WebRequest -Uri $usbipInstallerUrl -OutFile $usbipInstaller -ErrorAction Stop
            $usbipInstallerHash = (Get-FileHash -LiteralPath $usbipInstaller -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($usbipInstallerHash -ne "51620fa5f9f8be5932bc9d786deee557ce06d5407a99cab490dcfac71f185fea") {
                throw "USBIP installer integrity check failed."
            }
            Assert-PadSenseNotRunning
            Disconnect-UsbipImports (Get-CanonicalUsbipPath)
            $removedMismatchedPackage = Remove-MismatchedUsbipPackage `
                $usbipEntry $usbipInstalledVersion $usbipTargetVersion
            Write-Host "Installing USBIP drivers (UAC prompt will appear)..." -ForegroundColor Yellow
            $installerProcess = Start-Process -FilePath $usbipInstaller -ArgumentList "/S" -Verb RunAs -Wait -PassThru
            if ($installerProcess.ExitCode -notin @(0, 1641, 3010)) {
                throw "USBIP installer exited with code $($installerProcess.ExitCode)."
            }
            Write-Host "USBIP drivers installed/updated successfully" -ForegroundColor Green
            $needsReboot = $removedMismatchedPackage -or
                $installerProcess.ExitCode -in @(1641, 3010)
        }
        catch {
            throw "Failed to install USBIP drivers: $($_.Exception.Message)"
        }
    }

    $usbipRuntime = Test-UsbipRuntime $usbipTargetVersion
    if (-not $usbipRuntime.Healthy) {
        $needsReboot = $true
        Write-Host "USBIP 0.9.7.7 is installed but not active yet: $($usbipRuntime.Detail)" -ForegroundColor Yellow
    }

    if (-not $needsReboot) {
        if (-not $isUpdate) {
            Write-Host "Configuring system startup..."
        }
        Invoke-ViiperInstallRegistration $installPath
    }
    
    Write-Host "VIIPER installed successfully!" -ForegroundColor Green
    Write-Host "Binary installed to: $installPath"
    if ($isUpdate) {
        if ($skipInstall) {
            Write-Host "Binary already at correct version or newer. Skipping binary copy."
        }
        else {
            Write-Host "Update complete. Startup/autostart configuration was left unchanged."
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
        Write-Host "VIIPER was intentionally left stopped; restart Windows, then launch DS4Windows." -ForegroundColor Yellow
    }
    else {
        taskkill.exe /IM "viiper.exe" /F > $null 2>&1
        Start-Process -WindowStyle Hidden "$installPath" -ArgumentList "server"
    }
}
finally {
    Remove-Item -Recurse -Force $tempDir -ErrorAction SilentlyContinue
}

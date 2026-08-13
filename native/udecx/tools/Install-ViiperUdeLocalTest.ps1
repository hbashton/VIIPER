[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$PackageRoot,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$')]
    [string]$ExpectedSourceRevision,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-fA-F]{64}$')]
    [string]$ExpectedPackageLockSHA256,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^S-1-5-21-(?:[0-9]+-){3}[0-9]+$')]
    [string]$TargetUserSID,
    [switch]$AcknowledgeDisposableTestMachine,
    [switch]$PreflightOnly
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$installerScriptPath = (Resolve-Path -LiteralPath $PSCommandPath -ErrorAction Stop).Path
$installerScriptItem = Get-Item -LiteralPath $installerScriptPath -Force -ErrorAction Stop
if ($installerScriptItem.PSIsContainer -or
    ($installerScriptItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
    throw 'The local-test installer must be a regular non-reparse file.'
}
$installerScriptStream = [IO.FileStream]::new(
    $installerScriptPath, [IO.FileMode]::Open, [IO.FileAccess]::Read,
    [IO.FileShare]::Read)
try {
$installerScriptAlgorithm = [Security.Cryptography.SHA256]::Create()
try {
    $actualInstallerScriptSha256 = ([BitConverter]::ToString(
        $installerScriptAlgorithm.ComputeHash($installerScriptStream))).Replace('-', '').ToLowerInvariant()
}
finally {
    $installerScriptAlgorithm.Dispose()
}

if (-not $AcknowledgeDisposableTestMachine) {
    throw 'Local test driver installation is for a disposable test machine only. Pass -AcknowledgeDisposableTestMachine.'
}
$source = $ExpectedSourceRevision.ToLowerInvariant()
$expectedPackageLockSha256 = $ExpectedPackageLockSHA256.ToLowerInvariant()
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Local VIIPER driver installation requires an elevated PowerShell session.'
}
if (-not $PreflightOnly) {
    $bcdeditPath = Join-Path ([Environment]::SystemDirectory) 'bcdedit.exe'
    $bcdOutput = (& $bcdeditPath /enum '{current}' 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0 -or $bcdOutput -notmatch '(?im)^\s*testsigning\s+Yes\s*$') {
        throw "The current boot entry does not report 'testsigning Yes'. Enable TESTSIGNING and reboot before installation.`n$bcdOutput"
    }
}

$root = (Resolve-Path -LiteralPath $PackageRoot -ErrorAction Stop).Path
$lockPath = Join-Path $root 'local-test-package.lock.json'
$manifestPath = Join-Path $root 'submission-manifest.json'
$certificatePath = Join-Path $root 'ViiperUdeTest.cer'
$helperPath = Join-Path $root 'ViiperUdeCtl.exe'
$packageBrokerPath = Join-Path $root 'viiper.exe'
$signedPackage = Join-Path $root 'signed-package'
$driverDirectory = Join-Path $root 'driver'

function Assert-ExactDirectoryEntries {
    param(
        [Parameter(Mandatory = $true)][string]$Directory,
        [Parameter(Mandatory = $true)][string[]]$Expected
    )

    $directoryItem = Get-Item -LiteralPath $Directory -Force -ErrorAction Stop
    if (-not $directoryItem.PSIsContainer -or
        ($directoryItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Local test package directory is missing or unsafe: '$Directory'."
    }
    $actual = @(Get-ChildItem -LiteralPath $Directory -Force |
        ForEach-Object Name | Sort-Object -CaseSensitive)
    $wanted = @($Expected | Sort-Object -CaseSensitive)
    if ($actual.Count -ne $wanted.Count -or
        @(Compare-Object -ReferenceObject $wanted -DifferenceObject $actual -CaseSensitive).Count -ne 0) {
        throw "Local test package directory has missing, extra, or case-mismatched entries: '$Directory'."
    }
}

function Assert-ProtectedStagingDirectory {
    param([Parameter(Mandatory = $true)][string]$Path)

    $directory = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    if (-not $directory.PSIsContainer -or
        ($directory.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Local-test staging directory is missing, not a directory, or a reparse point: '$Path'."
    }
    $actualSecurity = $directory.GetAccessControl(
        [Security.AccessControl.AccessControlSections]::Owner -bor
        [Security.AccessControl.AccessControlSections]::Access)
    if (-not $actualSecurity.AreAccessRulesProtected) {
        throw "Local-test staging directory inherited an unsafe DACL for '$Path'."
    }
    $owner = $actualSecurity.GetOwner([Security.Principal.SecurityIdentifier])
    if ($owner.Value -cne 'S-1-5-32-544') {
        throw "Local-test staging directory has an unexpected owner for '$Path'."
    }
    $rules = @($actualSecurity.GetAccessRules(
        $true, $true, [Security.Principal.SecurityIdentifier]))
    if ($rules.Count -ne 2) {
        throw "Local-test staging directory has an unexpected access-rule count for '$Path'."
    }
    $expectedInheritance =
        [Security.AccessControl.InheritanceFlags]::ContainerInherit -bor
        [Security.AccessControl.InheritanceFlags]::ObjectInherit
    foreach ($expectedSID in @('S-1-5-18', 'S-1-5-32-544')) {
        $matches = @($rules | Where-Object {
            $_.IdentityReference.Value -ceq $expectedSID
        })
        if ($matches.Count -ne 1) {
            throw "Local-test staging directory is missing an exact protected principal for '$Path'."
        }
        $rule = $matches[0]
        if ($rule.IsInherited -or
            $rule.AccessControlType -ne [Security.AccessControl.AccessControlType]::Allow -or
            $rule.FileSystemRights -ne [Security.AccessControl.FileSystemRights]::FullControl -or
            $rule.InheritanceFlags -ne $expectedInheritance -or
            $rule.PropagationFlags -ne [Security.AccessControl.PropagationFlags]::None) {
            throw "Local-test staging directory has an unexpected access rule for '$Path'."
        }
    }
}

function Initialize-ProtectedStagingDirectory {
    param([Parameter(Mandatory = $true)][string]$Path)

    if (Test-Path -LiteralPath $Path) {
        throw "Refusing to reuse local-test staging directory '$Path'."
    }
    $expectedSecurity = [Security.AccessControl.DirectorySecurity]::new()
    $expectedSecurity.SetSecurityDescriptorSddlForm(
        'O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)',
        [Security.AccessControl.AccessControlSections]::All)
    $directory = [IO.Directory]::CreateDirectory($Path, $expectedSecurity)
    if (($directory.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Local-test staging directory is a reparse point: '$Path'."
    }
    $directory.SetAccessControl($expectedSecurity)
    Assert-ProtectedStagingDirectory -Path $Path
}

function Copy-ExactBrokerToProtectedStage {
    param(
        [Parameter(Mandatory = $true)][string]$SourcePath,
        [Parameter(Mandatory = $true)][string]$DestinationDirectory,
        [Parameter(Mandatory = $true)][long]$ExpectedLength,
        [Parameter(Mandatory = $true)][string]$ExpectedSHA256
    )

    $destinationPath = Join-Path $DestinationDirectory 'viiper.exe'
    $sourceStream = [IO.FileStream]::new(
        $SourcePath, [IO.FileMode]::Open, [IO.FileAccess]::Read,
        [IO.FileShare]::Read)
    try {
        if ($sourceStream.Length -ne $ExpectedLength) {
            throw 'The broker changed before protected staging.'
        }
        $sourceAlgorithm = [Security.Cryptography.SHA256]::Create()
        try {
            $sourceDigest = ([BitConverter]::ToString(
                $sourceAlgorithm.ComputeHash($sourceStream))).Replace('-', '').ToLowerInvariant()
        }
        finally {
            $sourceAlgorithm.Dispose()
        }
        if ($sourceDigest -cne $ExpectedSHA256) {
            throw 'The broker changed before protected staging.'
        }
        $sourceStream.Position = 0
        $destinationStream = [IO.FileStream]::new(
            $destinationPath, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write,
            [IO.FileShare]::None, 1MB, [IO.FileOptions]::WriteThrough)
        try {
            $sourceStream.CopyTo($destinationStream)
            $destinationStream.Flush($true)
        }
        finally {
            $destinationStream.Dispose()
        }
    }
    finally {
        $sourceStream.Dispose()
    }
    $staged = Get-Item -LiteralPath $destinationPath -Force -ErrorAction Stop
    if ($staged.PSIsContainer -or
        ($staged.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or
        $staged.Length -ne $ExpectedLength -or
        (Get-FileHash -LiteralPath $destinationPath -Algorithm SHA256).Hash.ToLowerInvariant() -cne
            $ExpectedSHA256) {
        throw 'The protected staged broker failed exact verification.'
    }
    return $destinationPath
}

function Remove-ProtectedStagingDirectory {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$ProgramDataRoot
    )

    if (-not (Test-Path -LiteralPath $Path)) {
        return
    }
    $fullPath = [IO.Path]::GetFullPath($Path)
    $expectedParent = [IO.Path]::GetFullPath($ProgramDataRoot).TrimEnd(
        [IO.Path]::DirectorySeparatorChar)
    if ([IO.Path]::GetDirectoryName($fullPath) -cne $expectedParent -or
        [IO.Path]::GetFileName($fullPath) -notmatch '^VIIPER\.LocalTestStage\.[0-9a-f]{32}$') {
        throw "Refusing unsafe local-test staging cleanup '$Path'."
    }
    Assert-ProtectedStagingDirectory -Path $fullPath
    $children = @(Get-ChildItem -LiteralPath $fullPath -Force)
    if ($children.Count -gt 1 -or
        ($children.Count -eq 1 -and
            ($children[0].Name -cne 'viiper.exe' -or $children[0].PSIsContainer -or
                ($children[0].Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0))) {
        throw "Refusing local-test staging cleanup with unexpected entries in '$Path'."
    }
    if ($children.Count -eq 1) {
        [IO.File]::Delete($children[0].FullName)
    }
    [IO.Directory]::Delete($fullPath, $false)
}

function Remove-PreBootProtectedStagingDirectories {
    param([Parameter(Mandatory = $true)][string]$ProgramDataRoot)

    # A live sibling installer can own a same-boot staging directory before it
    # acquires the nested package mutex. Only reclaim exact protected stages
    # which predate this boot; Windows already terminated every possible owner.
    $bootBoundaryUtc = [DateTime]::UtcNow.Subtract(
        [TimeSpan]::FromMilliseconds([Environment]::TickCount64))
    $candidates = @(Get-ChildItem -LiteralPath $ProgramDataRoot -Force -Directory |
        Where-Object {
            $_.Name -match '^VIIPER\.LocalTestStage\.[0-9a-f]{32}$' -and
            $_.LastWriteTimeUtc -lt $bootBoundaryUtc
        })
    foreach ($candidate in $candidates) {
        Remove-ProtectedStagingDirectory `
            -Path $candidate.FullName -ProgramDataRoot $ProgramDataRoot
        Write-Host "local-test-stage action=cleanup result=removed path=$($candidate.Name)"
    }
}

function ConvertTo-WindowsProcessArgument {
    param([AllowEmptyString()][Parameter(Mandatory = $true)][string]$Value)

    if ($Value.IndexOf([char]0) -ge 0) {
        throw 'Native process argument contains NUL.'
    }
    if ($Value.Length -ne 0 -and $Value -notmatch '[\s"]') {
        return $Value
    }
    $builder = [Text.StringBuilder]::new()
    [void]$builder.Append([char]34)
    $slashes = 0
    foreach ($character in $Value.ToCharArray()) {
        if ($character -eq [char]92) {
            ++$slashes
            continue
        }
        if ($character -eq [char]34) {
            [void]$builder.Append([char]92, (2 * $slashes) + 1)
            [void]$builder.Append([char]34)
            $slashes = 0
            continue
        }
        if ($slashes -ne 0) {
            [void]$builder.Append([char]92, $slashes)
            $slashes = 0
        }
        [void]$builder.Append($character)
    }
    if ($slashes -ne 0) {
        [void]$builder.Append([char]92, 2 * $slashes)
    }
    [void]$builder.Append([char]34)
    return $builder.ToString()
}

function Set-ExactProcessArguments {
    param(
        [Parameter(Mandatory = $true)][Diagnostics.ProcessStartInfo]$StartInfo,
        [Parameter(Mandatory = $true)][string[]]$Arguments
    )

    if ($null -ne $StartInfo.PSObject.Properties['ArgumentList']) {
        foreach ($argument in $Arguments) {
            $StartInfo.ArgumentList.Add($argument)
        }
        return
    }
    $StartInfo.Arguments = (($Arguments | ForEach-Object {
        ConvertTo-WindowsProcessArgument -Value $_
    }) -join ' ')
}

function Invoke-JoinedNativeProcess {
    param(
        [Parameter(Mandatory = $true)][string]$FileName,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][string]$WorkingDirectory,
        [Parameter(Mandatory = $true)][ref]$Started
    )

    $Started.Value = $false
    $startInfo = [Diagnostics.ProcessStartInfo]::new()
    $startInfo.FileName = $FileName
    $startInfo.WorkingDirectory = $WorkingDirectory
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    Set-ExactProcessArguments -StartInfo $startInfo -Arguments $Arguments
    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $startInfo
    $joined = $false
    try {
        if (-not $process.Start()) {
            throw 'The protected native broker process was not created.'
        }
        $Started.Value = $true
        $stdoutTask = $process.StandardOutput.ReadToEndAsync()
        $stderrTask = $process.StandardError.ReadToEndAsync()
        while (-not $joined) {
            try {
                $process.WaitForExit()
                $joined = $true
            }
            catch {
                # Never unwind while the exact mutating child may remain alive.
                Start-Sleep -Milliseconds 250
            }
        }
        $stdout = $stdoutTask.GetAwaiter().GetResult()
        $stderr = $stderrTask.GetAwaiter().GetResult()
        $combined = @($stdout, $stderr) -join [Environment]::NewLine
        return [pscustomobject]@{
            ExitCode = $process.ExitCode
            Output = @($combined -split '\r?\n' | Where-Object { $_.Length -ne 0 })
        }
    }
    finally {
        if ($Started.Value -and -not $joined) {
            while (-not $joined) {
                try {
                    $process.WaitForExit()
                    $joined = $true
                }
                catch {
                    Start-Sleep -Milliseconds 250
                }
            }
        }
        $process.Dispose()
    }
}

Assert-ExactDirectoryEntries $root @(
    'viiper.exe', 'ViiperUdeCtl.exe', 'ViiperUdeMediaProbe.exe', 'ViiperUdeInputProbe.exe',
    'ViiperUdeLiveProbes.manifest.json', 'ViiperUdeTest.cer',
    'submission-manifest.json', 'local-test-package.lock.json',
    'driver', 'signed-package'
)
Assert-ExactDirectoryEntries $driverDirectory @(
    'ViiperUde.inf', 'ViiperUde.sys', 'ViiperUde.cat'
)
Assert-ExactDirectoryEntries $signedPackage @(
    'ViiperUde.inf', 'ViiperUde.sys', 'ViiperUde.pdb', 'ViiperUde.cat'
)

$lockBytes = [IO.File]::ReadAllBytes($lockPath)
$lockAlgorithm = [Security.Cryptography.SHA256]::Create()
try {
    $actualPackageLockSha256 = ([BitConverter]::ToString(
        $lockAlgorithm.ComputeHash($lockBytes))).Replace('-', '').ToLowerInvariant()
}
finally {
    $lockAlgorithm.Dispose()
}
if ($actualPackageLockSha256 -cne $expectedPackageLockSha256) {
    throw 'The local test package lock does not match the out-of-band workflow digest.'
}
$strictUtf8 = [Text.UTF8Encoding]::new($false, $true)
$lock = $strictUtf8.GetString($lockBytes) | ConvertFrom-Json -ErrorAction Stop
if ([int]$lock.schema -ne 1 -or [string]$lock.sourceRevision -cne $source -or
    [string]$lock.driverBuildIdentity -notmatch '^[0-9a-f]{64}$' -or
    [string]$lock.testSignerCertificateSha256 -notmatch '^[0-9a-f]{64}$' -or
    [string]$lock.installerScriptSha256 -notmatch '^[0-9a-f]{64}$' -or
    [string]$lock.installerScriptSha256 -cne $actualInstallerScriptSha256) {
    throw 'The local test package lock does not match the requested source or schema.'
}

$expectedPaths = @(
    'viiper.exe', 'ViiperUdeCtl.exe', 'ViiperUdeMediaProbe.exe', 'ViiperUdeInputProbe.exe',
    'ViiperUdeLiveProbes.manifest.json', 'ViiperUdeTest.cer',
    'submission-manifest.json',
    'driver/ViiperUde.inf', 'driver/ViiperUde.sys', 'driver/ViiperUde.cat',
    'signed-package/ViiperUde.inf', 'signed-package/ViiperUde.sys',
    'signed-package/ViiperUde.pdb', 'signed-package/ViiperUde.cat'
)
$entries = @($lock.files)
if ($entries.Count -ne $expectedPaths.Count) {
    throw 'The local test package lock has an incomplete or extra file list.'
}
$seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
$lockByPath = [Collections.Generic.Dictionary[string, object]]::new([StringComparer]::Ordinal)
foreach ($entry in $entries) {
    $relative = [string]$entry.path
    if ($expectedPaths -cnotcontains $relative -or -not $seen.Add($relative) -or
        [long]$entry.length -le 0 -or [string]$entry.sha256 -notmatch '^[0-9a-f]{64}$') {
        throw "The local test package lock contains an invalid entry '$relative'."
    }
    $lockByPath.Add($relative, $entry)
    $path = Join-Path $root $relative.Replace('/', [IO.Path]::DirectorySeparatorChar)
    $item = Get-Item -LiteralPath $path -Force -ErrorAction Stop
    if ($item.PSIsContainer -or
        ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0 -or
        $item.Length -ne [long]$entry.length -or
        (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant() -cne
            [string]$entry.sha256) {
        throw "Local test package file validation failed for '$relative'."
    }
}

$certificate = [Security.Cryptography.X509Certificates.X509Certificate2]::new($certificatePath)
$algorithm = [Security.Cryptography.SHA256]::Create()
try {
    $certificateSha256 = ([BitConverter]::ToString(
        $algorithm.ComputeHash($certificate.RawData))).Replace('-', '').ToLowerInvariant()
}
finally {
    $algorithm.Dispose()
}
if ($certificateSha256 -cne [string]$lock.testSignerCertificateSha256) {
    throw 'The local test certificate does not match the source-bound package lock.'
}

if ($PreflightOnly) {
    $brokerEntry = $lockByPath['viiper.exe']
    $brokerHash = [string]$brokerEntry.sha256
    $preflightProgramDataRoot = (Resolve-Path -LiteralPath $env:ProgramData -ErrorAction Stop).Path
    $programDataItem = Get-Item -LiteralPath $preflightProgramDataRoot -Force -ErrorAction Stop
    if (-not $programDataItem.PSIsContainer -or
        ($programDataItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "ProgramData is not a safe staging parent: '$preflightProgramDataRoot'."
    }
    $preflightStage = Join-Path $preflightProgramDataRoot (
        'VIIPER.LocalTestStage.' + [Guid]::NewGuid().ToString('N'))
    try {
        Initialize-ProtectedStagingDirectory -Path $preflightStage
        [void](Copy-ExactBrokerToProtectedStage `
            -SourcePath $packageBrokerPath -DestinationDirectory $preflightStage `
            -ExpectedLength ([long]$brokerEntry.length) -ExpectedSHA256 $brokerHash)
    }
    finally {
        if (Test-Path -LiteralPath $preflightStage) {
            Remove-ProtectedStagingDirectory `
                -Path $preflightStage -ProgramDataRoot $preflightProgramDataRoot
        }
    }
}

$certificateThumbprint = $certificate.Thumbprint
$expectedCertificateBytes = [Convert]::ToBase64String($certificate.RawData)
if (-not ('ViiperLocalTestCertificateStore' -as [type])) {
    Add-Type -Language CSharp -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;

public static class ViiperLocalTestCertificateStore
{
    private const int CERT_STORE_PROV_SYSTEM_W = 10;
    private const uint CERT_SYSTEM_STORE_LOCAL_MACHINE = 0x00020000;
    private const uint CERT_STORE_OPEN_EXISTING_FLAG = 0x00004000;
    private const uint CERT_STORE_MAXIMUM_ALLOWED_FLAG = 0x00001000;
    private const uint CERT_ENCODING = 0x00010001;
    private const uint CERT_STORE_ADD_NEW = 1;
    private const uint CERT_FIND_EXISTING = 0x000d0000;
    private const int CRYPT_E_NOT_FOUND = unchecked((int)0x80092004);

    [DllImport("crypt32.dll", CharSet = CharSet.Unicode, SetLastError = true,
        ExactSpelling = true)]
    private static extern IntPtr CertOpenStore(
        IntPtr provider, uint encoding, IntPtr cryptProvider,
        uint flags, string storeName);

    [DllImport("crypt32.dll", SetLastError = true)]
    private static extern bool CertAddEncodedCertificateToStore(
        IntPtr store, uint encoding, byte[] certificate, uint length,
        uint disposition, out IntPtr context);

    [DllImport("crypt32.dll", SetLastError = true)]
    private static extern IntPtr CertCreateCertificateContext(
        uint encoding, byte[] certificate, uint length);

    [DllImport("crypt32.dll", SetLastError = true)]
    private static extern IntPtr CertFindCertificateInStore(
        IntPtr store, uint encoding, uint findFlags, uint findType,
        IntPtr findParameter, IntPtr previousContext);

    [DllImport("crypt32.dll", SetLastError = true)]
    private static extern bool CertDeleteCertificateFromStore(IntPtr context);

    [DllImport("crypt32.dll")]
    private static extern bool CertFreeCertificateContext(IntPtr context);

    [DllImport("crypt32.dll", SetLastError = true)]
    private static extern bool CertCloseStore(IntPtr store, uint flags);

    private static IntPtr Open(string storeName)
    {
        IntPtr store = CertOpenStore(
            new IntPtr(CERT_STORE_PROV_SYSTEM_W), 0, IntPtr.Zero,
            CERT_SYSTEM_STORE_LOCAL_MACHINE | CERT_STORE_OPEN_EXISTING_FLAG |
                CERT_STORE_MAXIMUM_ALLOWED_FLAG,
            storeName);
        if (store == IntPtr.Zero)
            throw new Win32Exception(Marshal.GetLastWin32Error(), "CertOpenStore");
        return store;
    }

    public static void Add(string storeName, byte[] certificate)
    {
        IntPtr store = Open(storeName);
        IntPtr context = IntPtr.Zero;
        try
        {
            if (!CertAddEncodedCertificateToStore(
                    store, CERT_ENCODING, certificate, (uint)certificate.Length,
                    CERT_STORE_ADD_NEW, out context))
                throw new Win32Exception(
                    Marshal.GetLastWin32Error(), "CertAddEncodedCertificateToStore");
        }
        finally
        {
            if (context != IntPtr.Zero) CertFreeCertificateContext(context);
            CertCloseStore(store, 0);
        }
    }

    public static bool Remove(string storeName, byte[] certificate)
    {
        IntPtr store = Open(storeName);
        IntPtr search = IntPtr.Zero;
        try
        {
            search = CertCreateCertificateContext(
                CERT_ENCODING, certificate, (uint)certificate.Length);
            if (search == IntPtr.Zero)
                throw new Win32Exception(
                    Marshal.GetLastWin32Error(), "CertCreateCertificateContext");
            IntPtr found = CertFindCertificateInStore(
                store, CERT_ENCODING, 0, CERT_FIND_EXISTING, search, IntPtr.Zero);
            if (found == IntPtr.Zero)
            {
                int error = Marshal.GetLastWin32Error();
                if (error == CRYPT_E_NOT_FOUND) return false;
                throw new Win32Exception(error, "CertFindCertificateInStore");
            }
            if (!CertDeleteCertificateFromStore(found))
                throw new Win32Exception(
                    Marshal.GetLastWin32Error(), "CertDeleteCertificateFromStore");
            return true;
        }
        finally
        {
            if (search != IntPtr.Zero) CertFreeCertificateContext(search);
            CertCloseStore(store, 0);
        }
    }
}
'@
}

$certificateStoreOpenMethod = [ViiperLocalTestCertificateStore].GetMethod(
    'CertOpenStore', [Reflection.BindingFlags]'NonPublic,Static')
$certificateStoreOpenImport = $certificateStoreOpenMethod.GetCustomAttributes(
    [Runtime.InteropServices.DllImportAttribute], $false)[0]
if ($certificateStoreOpenImport.Value -cne 'crypt32.dll' -or
    -not $certificateStoreOpenImport.ExactSpelling -or
    $certificateStoreOpenImport.CharSet -ne [Runtime.InteropServices.CharSet]::Unicode) {
    throw 'The local-test certificate-store interop does not bind the exact CertOpenStore entry point.'
}

if ($PreflightOnly) {
    Write-Output 'result=success operation=local-test-preflight changed=0 rebootRequired=0 rollback=not-needed exitCode=0'
    return
}

function Get-ExactLocalTestTrustState {
    param([Parameter(Mandatory = $true)][string]$StoreName)

    $store = [Security.Cryptography.X509Certificates.X509Store]::new(
        $StoreName, [Security.Cryptography.X509Certificates.StoreLocation]::LocalMachine)
    $matches = $null
    try {
        # Reopening the store read-only makes every verification a persisted-state
        # postcondition rather than an observation through the mutating handle.
        $store.Open([Security.Cryptography.X509Certificates.OpenFlags]::ReadOnly)
        $matches = $store.Certificates.Find(
            [Security.Cryptography.X509Certificates.X509FindType]::FindByThumbprint,
            $certificateThumbprint, $false)
        $exactMatches = @($matches | Where-Object {
            [Convert]::ToBase64String($_.RawData) -ceq $expectedCertificateBytes
        })
        if ($matches.Count -ne $exactMatches.Count -or $exactMatches.Count -gt 1) {
            throw "Certificate collision in LocalMachine\$StoreName."
        }
        return [pscustomobject]@{ ExactCount = [int]$exactMatches.Count }
    }
    finally {
        if ($null -ne $matches) {
            foreach ($match in $matches) {
                $match.Dispose()
            }
        }
        $store.Close()
    }
}

$addedStores = [Collections.Generic.List[string]]::new()
function Remove-NewLocalTestTrust {
    $removalErrors = [Collections.Generic.List[Exception]]::new()
    foreach ($storeName in $addedStores) {
        $cleanupAction = 'inspect-cleanup'
        try {
            $cleanupState = Get-ExactLocalTestTrustState -StoreName $storeName
            $cleanupAction = 'remove'
            if ($cleanupState.ExactCount -eq 1) {
                $removed = [ViiperLocalTestCertificateStore]::Remove(
                    $storeName, $certificate.RawData)
                $removeResult = if ($removed) { 'removed' } else { 'already-absent' }
            }
            else {
                $removeResult = 'already-absent'
            }
            Write-Host "local-test-trust store=$storeName action=remove result=$removeResult"

            $cleanupAction = 'verify-cleanup'
            $cleanupState = Get-ExactLocalTestTrustState -StoreName $storeName
            if ($cleanupState.ExactCount -ne 0) {
                throw "Exact local-test certificate remained in LocalMachine\$storeName."
            }
            Write-Host "local-test-trust store=$storeName action=verify-cleanup result=absent"
        }
        catch {
            Write-Host "local-test-trust store=$storeName action=$cleanupAction result=error"
            $removalErrors.Add([InvalidOperationException]::new(
                "LocalMachine\$storeName trust cleanup failed during $cleanupAction.",
                $_.Exception))
        }
    }
    if ($removalErrors.Count -ne 0) {
        throw [AggregateException]::new(
            'Failed to remove one or more local-test trust anchors after a settled failure.',
            [Exception[]]$removalErrors.ToArray())
    }
}

function Test-SettledLocalTestFailure {
    param(
        [Parameter(Mandatory = $true)][object[]]$Lines,
        [Parameter(Mandatory = $true)][int]$ProcessExitCode
    )

    $pattern = '(?m)^result=error operation=install changed=(?<changed>[01]) ' +
        'rebootRequired=(?<reboot>[01]) rollback=(?<rollback>not-needed|succeeded|failed) ' +
        'exitCode=(?<exit>[0-9]+)(?: .*)?\r?$'
    # Out-String formats through the host and wraps long native proof lines at
    # the current console width. Preserve the already-delimited child output
    # byte-for-line instead: diagnostics may make the canonical proof much
    # wider than the host while the rollback fields remain authoritative.
    $proofText = [string]::Join([Environment]::NewLine, [string[]]$Lines)
    $matches = [regex]::Matches($proofText, $pattern)
    if ($matches.Count -ne 1) {
        return $false
    }
    $match = $matches[0]
    $proofExitCode = 0
    if (-not [int]::TryParse($match.Groups['exit'].Value, [ref]$proofExitCode) -or
        $proofExitCode -ne $ProcessExitCode) {
        return $false
    }
    return ($match.Groups['changed'].Value -ceq '0' -and
            $match.Groups['reboot'].Value -ceq '0' -and
            $match.Groups['rollback'].Value -ceq 'not-needed' -and
            $proofExitCode -in @(1, 4)) -or
        ($match.Groups['changed'].Value -ceq '1' -and
            $match.Groups['reboot'].Value -ceq '0' -and
            $match.Groups['rollback'].Value -ceq 'succeeded' -and
            $proofExitCode -eq 1)
}

$trustCommitted = $false
$retainTrustOnFailure = $false
$stageDirectory = $null
$programDataRoot = $null
try {
    foreach ($storeName in @('Root', 'TrustedPublisher')) {
        $trustAction = 'inspect-add'
        try {
            $trustState = Get-ExactLocalTestTrustState -StoreName $storeName
            if ($trustState.ExactCount -eq 0) {
                $trustAction = 'add'
                [ViiperLocalTestCertificateStore]::Add(
                    $storeName, $certificate.RawData)
                $addedStores.Add($storeName)
                Write-Host "local-test-trust store=$storeName action=add result=added"

                $trustAction = 'verify-add'
                $trustState = Get-ExactLocalTestTrustState -StoreName $storeName
                if ($trustState.ExactCount -ne 1) {
                    throw "Exact local-test certificate was not installed in LocalMachine\$storeName."
                }
                Write-Host "local-test-trust store=$storeName action=verify-add result=present"
            }
            else {
                Write-Host "local-test-trust store=$storeName action=add result=preexisting"
            }
        }
        catch {
            Write-Host "local-test-trust store=$storeName action=$trustAction result=error"
            throw
        }
    }

    foreach ($name in @('ViiperUde.inf', 'ViiperUde.sys', 'ViiperUde.cat')) {
        $runtime = Join-Path $driverDirectory $name
        $evidence = Join-Path $signedPackage $name
        if ((Get-FileHash -LiteralPath $runtime -Algorithm SHA256).Hash -cne
            (Get-FileHash -LiteralPath $evidence -Algorithm SHA256).Hash) {
            throw "Runtime driver file '$name' differs from its validated evidence copy."
        }
    }

    $manifestHash = [string]($lockByPath['submission-manifest.json'].sha256)
    $infHash = [string]($lockByPath['driver/ViiperUde.inf'].sha256)
    $sysHash = [string]($lockByPath['driver/ViiperUde.sys'].sha256)
    $catHash = [string]($lockByPath['driver/ViiperUde.cat'].sha256)
    $brokerEntry = $lockByPath['viiper.exe']
    $brokerHash = [string]$brokerEntry.sha256
    $helperHash = [string]($lockByPath['ViiperUdeCtl.exe'].sha256)

    $programDataRoot = (Resolve-Path -LiteralPath $env:ProgramData -ErrorAction Stop).Path
    $programDataItem = Get-Item -LiteralPath $programDataRoot -Force -ErrorAction Stop
    if (-not $programDataItem.PSIsContainer -or
        ($programDataItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "ProgramData is not a safe staging parent: '$programDataRoot'."
    }
    Remove-PreBootProtectedStagingDirectories -ProgramDataRoot $programDataRoot
    $stageDirectory = Join-Path $programDataRoot (
        'VIIPER.LocalTestStage.' + [Guid]::NewGuid().ToString('N'))
    Initialize-ProtectedStagingDirectory -Path $stageDirectory
    $brokerPath = Copy-ExactBrokerToProtectedStage `
        -SourcePath $packageBrokerPath -DestinationDirectory $stageDirectory `
        -ExpectedLength ([long]$brokerEntry.length) -ExpectedSHA256 $brokerHash

    $output = @()
    $exitCode = $null
    $launchError = $null
    $processStarted = $false
    $brokerArguments = @(
        'native-package-install',
        '--package-directory', $driverDirectory,
        '--submission-manifest', $manifestPath,
        '--source-revision', $source,
        '--driver-helper', $helperPath,
        '--expected-broker-sha-256', $brokerHash,
        '--expected-helper-sha-256', $helperHash,
        '--expected-manifest-sha-256', $manifestHash,
        '--expected-inf-sha-256', $infHash,
        '--expected-sys-sha-256', $sysHash,
        '--expected-cat-sha-256', $catHash,
        '--target-user-sid', $TargetUserSID,
        '--driver-validation-mode', 'local-test'
    )
    try {
        $processResult = Invoke-JoinedNativeProcess `
            -FileName $brokerPath -Arguments $brokerArguments `
            -WorkingDirectory $stageDirectory -Started ([ref]$processStarted)
        $retainTrustOnFailure = $processStarted
        $exitCode = [int]$processResult.ExitCode
        $output = @($processResult.Output)
    }
    catch {
        $retainTrustOnFailure = $processStarted
        $launchError = $_
    }
    $output | ForEach-Object { Write-Host $_ }
    if ($null -ne $exitCode) {
        if ($exitCode -in @(0, 3010)) {
            $trustCommitted = $true
        }
        elseif (Test-SettledLocalTestFailure `
            -Lines $output -ProcessExitCode $exitCode) {
            $retainTrustOnFailure = $false
        }
    }
    Remove-ProtectedStagingDirectory `
        -Path $stageDirectory -ProgramDataRoot $programDataRoot
    $stageDirectory = $null
    if ($null -ne $launchError) {
        throw $launchError
    }
    if ($exitCode -notin @(0, 3010)) {
        throw "Local VIIPER driver transaction failed with exit code $exitCode."
    }
    if ($exitCode -eq 3010) {
        Write-Warning 'The native transaction stopped at a safe reboot boundary before mutation or after successful rollback. Restart, rerun this identical install command before creating another virtual device, and proceed to live validation only after it returns exit 0.'
        exit 3010
    }
}
catch {
    $failure = $_
    $cleanupFailure = $null
    if ($null -ne $stageDirectory -and $null -ne $programDataRoot) {
        try {
            Remove-ProtectedStagingDirectory `
                -Path $stageDirectory -ProgramDataRoot $programDataRoot
            $stageDirectory = $null
        }
        catch {
            $cleanupFailure = $_
        }
    }
    if (-not $trustCommitted -and -not $retainTrustOnFailure) {
        Remove-NewLocalTestTrust
    }
    if ($null -ne $cleanupFailure) {
        throw [AggregateException]::new(
            'Local VIIPER installation failed and protected staging cleanup also failed.',
            [Exception[]]@($failure.Exception, $cleanupFailure.Exception))
    }
    throw $failure
}

Write-Host 'The exact local test-signed VIIPER UdeCx driver and native broker are installed, authenticated, and ready.'
Write-Host 'Next: enable Driver Verifier for ViiperUde.sys, reboot, then run Invoke-ViiperUdeLiveValidation.ps1 in LocalTest mode.'
}
finally {
    $installerScriptStream.Dispose()
}

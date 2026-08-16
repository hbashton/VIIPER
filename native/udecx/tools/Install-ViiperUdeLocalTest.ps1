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

function Get-LocalTestFileSystemSecurity {
    param(
        [Parameter(Mandatory = $true)][IO.FileSystemInfo]$Item,
        [Parameter(Mandatory = $true)]
        [Security.AccessControl.AccessControlSections]$Sections
    )

    if ($null -ne $Item.PSObject.Methods['GetAccessControl']) {
        return $Item.GetAccessControl($Sections)
    }
    if ($Item -is [IO.DirectoryInfo]) {
        return [IO.FileSystemAclExtensions]::GetAccessControl(
            [IO.DirectoryInfo]$Item, $Sections)
    }
    return [IO.FileSystemAclExtensions]::GetAccessControl(
        [IO.FileInfo]$Item, $Sections)
}

function Assert-ProtectedStagingDirectory {
    param([Parameter(Mandatory = $true)][string]$Path)

    $directory = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    if (-not $directory.PSIsContainer -or
        ($directory.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Local-test staging directory is missing, not a directory, or a reparse point: '$Path'."
    }
    $actualSecurity = Get-LocalTestFileSystemSecurity -Item $directory -Sections (
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

if (-not ('ViiperLocalTestStagingNative' -as [type])) {
    Add-Type -Language CSharp -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using Microsoft.Win32.SafeHandles;

public sealed class ViiperLocalTestProtectedFile
{
    public SafeFileHandle Handle { get; private set; }
    public bool Created { get; private set; }

    public ViiperLocalTestProtectedFile(SafeFileHandle handle, bool created)
    {
        Handle = handle;
        Created = created;
    }
}

public static class ViiperLocalTestStagingNative
{
    private const uint SDDL_REVISION_1 = 1;
    private const int ERROR_FILE_EXISTS = 80;
    private const int ERROR_ALREADY_EXISTS = 183;
    private const uint GENERIC_READ = 0x80000000;
    private const uint GENERIC_WRITE = 0x40000000;
    private const uint READ_CONTROL = 0x00020000;
    private const uint FILE_READ_ATTRIBUTES = 0x00000080;
    private const uint FILE_SHARE_READ = 0x00000001;
    private const uint FILE_SHARE_WRITE = 0x00000002;
    private const uint CREATE_NEW = 1;
    private const uint OPEN_EXISTING = 3;
    private const uint FILE_ATTRIBUTE_NORMAL = 0x00000080;
    private const uint FILE_FLAG_OPEN_REPARSE_POINT = 0x00200000;
    private const uint FILE_FLAG_BACKUP_SEMANTICS = 0x02000000;
    private const uint FILE_FLAG_WRITE_THROUGH = 0x80000000;

    [StructLayout(LayoutKind.Sequential)]
    private struct SecurityAttributes
    {
        public int Length;
        public IntPtr SecurityDescriptor;
        [MarshalAs(UnmanagedType.Bool)] public bool InheritHandle;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct FileTime
    {
        public uint Low;
        public uint High;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct ByHandleFileInformation
    {
        public uint FileAttributes;
        public FileTime CreationTime;
        public FileTime LastAccessTime;
        public FileTime LastWriteTime;
        public uint VolumeSerialNumber;
        public uint FileSizeHigh;
        public uint FileSizeLow;
        public uint NumberOfLinks;
        public uint FileIndexHigh;
        public uint FileIndexLow;
    }

    [DllImport("advapi32.dll", EntryPoint = "ConvertStringSecurityDescriptorToSecurityDescriptorW",
        CharSet = CharSet.Unicode, ExactSpelling = true, SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    private static extern bool ConvertStringSecurityDescriptorToSecurityDescriptor(
        string securityDescriptor, uint revision, out IntPtr descriptor, out uint descriptorLength);

    [DllImport("kernel32.dll", EntryPoint = "CreateDirectoryW", CharSet = CharSet.Unicode,
        ExactSpelling = true, SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    private static extern bool CreateDirectory(string path, ref SecurityAttributes attributes);

    [DllImport("kernel32.dll", EntryPoint = "CreateFileW", CharSet = CharSet.Unicode,
        ExactSpelling = true, SetLastError = true)]
    private static extern SafeFileHandle CreateFileWithSecurity(
        string path, uint desiredAccess, uint shareMode, ref SecurityAttributes attributes,
        uint creationDisposition, uint flagsAndAttributes, IntPtr templateFile);

    [DllImport("kernel32.dll", EntryPoint = "CreateFileW", CharSet = CharSet.Unicode,
        ExactSpelling = true, SetLastError = true)]
    private static extern SafeFileHandle CreateFileWithoutSecurity(
        string path, uint desiredAccess, uint shareMode, IntPtr attributes,
        uint creationDisposition, uint flagsAndAttributes, IntPtr templateFile);

    [DllImport("kernel32.dll", ExactSpelling = true, SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    private static extern bool GetFileInformationByHandle(
        SafeFileHandle handle, out ByHandleFileInformation information);

    [DllImport("kernel32.dll", ExactSpelling = true)]
    private static extern IntPtr LocalFree(IntPtr memory);

    private static SecurityAttributes ConvertSecurityDescriptor(string sddl, out IntPtr descriptor)
    {
        uint descriptorLength;
        if (!ConvertStringSecurityDescriptorToSecurityDescriptor(
                sddl, SDDL_REVISION_1, out descriptor, out descriptorLength))
            throw new Win32Exception(Marshal.GetLastWin32Error(),
                "ConvertStringSecurityDescriptorToSecurityDescriptorW");
        return new SecurityAttributes {
            Length = Marshal.SizeOf(typeof(SecurityAttributes)),
            SecurityDescriptor = descriptor,
            InheritHandle = false
        };
    }

    public static bool CreateDirectoryExact(string path, string sddl)
    {
        IntPtr descriptor = IntPtr.Zero;
        try
        {
            SecurityAttributes attributes = ConvertSecurityDescriptor(sddl, out descriptor);
            if (CreateDirectory(path, ref attributes)) return true;
            int error = Marshal.GetLastWin32Error();
            if (error == ERROR_ALREADY_EXISTS) return false;
            throw new Win32Exception(error, "CreateDirectoryW");
        }
        finally
        {
            if (descriptor != IntPtr.Zero) LocalFree(descriptor);
        }
    }

    public static SafeFileHandle OpenDirectory(string path)
    {
        SafeFileHandle handle = CreateFileWithoutSecurity(
            path, READ_CONTROL | FILE_READ_ATTRIBUTES, FILE_SHARE_READ | FILE_SHARE_WRITE,
            IntPtr.Zero, OPEN_EXISTING,
            FILE_FLAG_BACKUP_SEMANTICS | FILE_FLAG_OPEN_REPARSE_POINT, IntPtr.Zero);
        if (handle.IsInvalid)
        {
            int error = Marshal.GetLastWin32Error();
            handle.Dispose();
            throw new Win32Exception(error, "CreateFileW(directory)");
        }
        return handle;
    }

    public static ViiperLocalTestProtectedFile OpenOrCreateFileExact(
        string path, string sddl)
    {
        IntPtr descriptor = IntPtr.Zero;
        try
        {
            SecurityAttributes attributes = ConvertSecurityDescriptor(sddl, out descriptor);
            SafeFileHandle handle = CreateFileWithSecurity(
                path, GENERIC_READ | GENERIC_WRITE, FILE_SHARE_READ | FILE_SHARE_WRITE,
                ref attributes, CREATE_NEW,
                FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT | FILE_FLAG_WRITE_THROUGH,
                IntPtr.Zero);
            if (!handle.IsInvalid) return new ViiperLocalTestProtectedFile(handle, true);
            int error = Marshal.GetLastWin32Error();
            handle.Dispose();
            if (error != ERROR_FILE_EXISTS && error != ERROR_ALREADY_EXISTS)
                throw new Win32Exception(error, "CreateFileW(CREATE_NEW)");

            handle = CreateFileWithoutSecurity(
                path, GENERIC_READ | GENERIC_WRITE, FILE_SHARE_READ | FILE_SHARE_WRITE,
                IntPtr.Zero, OPEN_EXISTING,
                FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT | FILE_FLAG_WRITE_THROUGH,
                IntPtr.Zero);
            if (handle.IsInvalid)
            {
                error = Marshal.GetLastWin32Error();
                handle.Dispose();
                throw new Win32Exception(error, "CreateFileW(OPEN_EXISTING)");
            }
            return new ViiperLocalTestProtectedFile(handle, false);
        }
        finally
        {
            if (descriptor != IntPtr.Zero) LocalFree(descriptor);
        }
    }

    public static SafeFileHandle OpenFileReadOnly(string path)
    {
        SafeFileHandle handle = CreateFileWithoutSecurity(
            path, GENERIC_READ | READ_CONTROL, FILE_SHARE_READ,
            IntPtr.Zero, OPEN_EXISTING,
            FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT, IntPtr.Zero);
        if (handle.IsInvalid)
        {
            int error = Marshal.GetLastWin32Error();
            handle.Dispose();
            throw new Win32Exception(error, "CreateFileW(read-only)");
        }
        return handle;
    }

    public static uint LinkCount(SafeFileHandle handle)
    {
        ByHandleFileInformation information;
        if (!GetFileInformationByHandle(handle, out information))
            throw new Win32Exception(Marshal.GetLastWin32Error(),
                "GetFileInformationByHandle");
        return information.NumberOfLinks;
    }

}
'@
}

function Assert-ExactLocalTestStagingSecurity {
    param(
        [Parameter(Mandatory = $true)][IO.FileSystemInfo]$Item,
        [Parameter(Mandatory = $true)][bool]$Directory
    )

    if ($Item.PSIsContainer -ne $Directory -or
        ($Item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "The local-test protected staging path has an unsafe object type: '$($Item.FullName)'."
    }
    $security = Get-LocalTestFileSystemSecurity -Item $Item -Sections (
        [Security.AccessControl.AccessControlSections]::Owner -bor
        [Security.AccessControl.AccessControlSections]::Group -bor
        [Security.AccessControl.AccessControlSections]::Access)
    if (-not $security.AreAccessRulesProtected -or
        $security.GetOwner([Security.Principal.SecurityIdentifier]).Value -cne 'S-1-5-32-544' -or
        $security.GetGroup([Security.Principal.SecurityIdentifier]).Value -cne 'S-1-5-32-544') {
        throw "The local-test protected staging object has an unsafe owner, group, or inherited DACL: '$($Item.FullName)'."
    }
    $rules = @($security.GetAccessRules(
        $true, $true, [Security.Principal.SecurityIdentifier]))
    if ($rules.Count -ne 2) {
        throw "The local-test protected staging object has an unexpected access-rule count: '$($Item.FullName)'."
    }
    $expectedInheritance = if ($Directory) {
        [Security.AccessControl.InheritanceFlags]::ContainerInherit -bor
            [Security.AccessControl.InheritanceFlags]::ObjectInherit
    }
    else {
        [Security.AccessControl.InheritanceFlags]::None
    }
    foreach ($expectedSID in @('S-1-5-18', 'S-1-5-32-544')) {
        $matches = @($rules | Where-Object {
            $_.IdentityReference.Value -ceq $expectedSID
        })
        if ($matches.Count -ne 1) {
            throw "The local-test protected staging object is missing an exact protected principal: '$($Item.FullName)'."
        }
        $rule = $matches[0]
        if ($rule.IsInherited -or
            $rule.AccessControlType -ne [Security.AccessControl.AccessControlType]::Allow -or
            $rule.FileSystemRights -ne [Security.AccessControl.FileSystemRights]::FullControl -or
            $rule.InheritanceFlags -ne $expectedInheritance -or
            $rule.PropagationFlags -ne [Security.AccessControl.PropagationFlags]::None) {
            throw "The local-test protected staging object has an unexpected access rule: '$($Item.FullName)'."
        }
    }
}

function New-LocalTestTrustCapability {
    param(
        [Parameter(Mandatory = $true)][string]$StageDirectory,
        [Parameter(Mandatory = $true)][string]$SourceRevision,
        [Parameter(Mandatory = $true)][string]$CertificatePath,
        [Parameter(Mandatory = $true)][string]$CertificateSHA256,
        [Parameter(Mandatory = $true)][string]$PackageLockSHA256,
        [Parameter(Mandatory = $true)][string]$TrustJournalDirectory
    )

    $path = Join-Path $StageDirectory 'local-test-trust-capability.json'
    if ([IO.Path]::GetDirectoryName([IO.Path]::GetFullPath($path)) -cne
        [IO.Path]::GetFullPath($StageDirectory).TrimEnd(
            [IO.Path]::DirectorySeparatorChar)) {
        throw 'The local-test trust capability escaped its protected broker stage.'
    }
    $nonceBytes = [byte[]]::new(16)
    $random = [Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $random.GetBytes($nonceBytes)
        $nonce = ([BitConverter]::ToString($nonceBytes)).Replace('-', '').ToLowerInvariant()
    }
    finally {
        $random.Dispose()
        [Array]::Clear($nonceBytes, 0, $nonceBytes.Length)
    }
    $currentProcess = [Diagnostics.Process]::GetCurrentProcess()
    try {
        $parentPID = [uint32]$currentProcess.Id
        $parentCreationFileTime = [uint64]$currentProcess.StartTime.ToUniversalTime().ToFileTimeUtc()
    }
    finally {
        $currentProcess.Dispose()
    }
    $payload = [ordered]@{
        schema = 'viiper.native.local-test-trust-capability/v1'
        nonce = $nonce
        parentPid = $parentPID
        parentCreationFileTime = $parentCreationFileTime
        sourceRevision = $SourceRevision
        certificatePath = [IO.Path]::GetFullPath($CertificatePath)
        certificateSha256 = $CertificateSHA256
        packageLockSha256 = $PackageLockSHA256
        trustJournalSchema = 'viiper.native.local-test-trust-ownership/v1'
        trustJournalDirectory = [IO.Path]::GetFullPath($TrustJournalDirectory)
    }
    $json = $payload | ConvertTo-Json -Compress -Depth 2
    if ($json.IndexOf("`r") -ge 0 -or $json.IndexOf("`n") -ge 0) {
        throw 'The local-test trust capability serializer emitted noncanonical framing.'
    }
    $bytes = [Text.UTF8Encoding]::new($false, $true).GetBytes($json)
    if ($bytes.Length -eq 0 -or $bytes.Length -gt 4096) {
        throw 'The local-test trust capability exceeds its exact size bound.'
    }
    $opened = [ViiperLocalTestStagingNative]::OpenOrCreateFileExact(
        $path, 'O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)')
    if (-not $opened.Created) {
        $opened.Handle.Dispose()
        throw 'Refusing to reuse a local-test trust capability file.'
    }
    $writeStream = $null
    try {
        $writeStream = [IO.FileStream]::new(
            $opened.Handle, [IO.FileAccess]::ReadWrite, 4096, $false)
        $writeStream.Write($bytes, 0, $bytes.Length)
        $writeStream.Flush($true)
    }
    finally {
        if ($null -ne $writeStream) {
            $writeStream.Dispose()
        }
        else {
            $opened.Handle.Dispose()
        }
    }

    $readHandle = $null
    $readStream = $null
    try {
        $readHandle = [ViiperLocalTestStagingNative]::OpenFileReadOnly($path)
        $readStream = [IO.FileStream]::new(
            $readHandle, [IO.FileAccess]::Read, 4096, $false)
        $item = Get-Item -LiteralPath $path -Force -ErrorAction Stop
        Assert-ExactLocalTestStagingSecurity -Item $item -Directory $false
        if ([ViiperLocalTestStagingNative]::LinkCount($readHandle) -ne 1 -or
            $readStream.Length -ne $bytes.Length) {
            throw 'The sealed local-test trust capability has an invalid identity or length.'
        }
        $algorithm = [Security.Cryptography.SHA256]::Create()
        try {
            $sha256 = ([BitConverter]::ToString(
                $algorithm.ComputeHash($readStream))).Replace('-', '').ToLowerInvariant()
        }
        finally {
            $algorithm.Dispose()
        }
        $readStream.Position = 0
        return [pscustomobject]@{
            Path = $path
            SHA256 = $sha256
            Stream = $readStream
        }
    }
    catch {
        if ($null -ne $readStream) {
            $readStream.Dispose()
        }
        elseif ($null -ne $readHandle) {
            $readHandle.Dispose()
        }
        try { [IO.File]::Delete($path) } catch { }
        throw
    }
}

function Remove-LocalTestTrustCapability {
    param(
        [Parameter(Mandatory = $true)]$Capability,
        [Parameter(Mandatory = $true)][string]$StageDirectory
    )

    if ($null -ne $Capability.Stream) {
        $Capability.Stream.Dispose()
        $Capability.Stream = $null
    }
    $path = [IO.Path]::GetFullPath([string]$Capability.Path)
    if ([IO.Path]::GetDirectoryName($path) -cne
            [IO.Path]::GetFullPath($StageDirectory).TrimEnd(
                [IO.Path]::DirectorySeparatorChar) -or
        [IO.Path]::GetFileName($path) -cne 'local-test-trust-capability.json') {
        throw 'Refusing unsafe local-test trust capability cleanup.'
    }
    if (Test-Path -LiteralPath $path) {
        $item = Get-Item -LiteralPath $path -Force -ErrorAction Stop
        Assert-ExactLocalTestStagingSecurity -Item $item -Directory $false
        [IO.File]::Delete($path)
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
    $allowedChildren = @('viiper.exe', 'local-test-trust-capability.json')
    if ($children.Count -gt $allowedChildren.Count -or
        @($children | Where-Object {
            $allowedChildren -cnotcontains $_.Name -or $_.PSIsContainer -or
                ($_.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0
        }).Count -ne 0) {
        throw "Refusing local-test staging cleanup with unexpected entries in '$Path'."
    }
    foreach ($child in $children) {
        [IO.File]::Delete($child.FullName)
    }
    [IO.Directory]::Delete($fullPath, $false)
}

if (-not ('ViiperWindowsUptime' -as [type])) {
    Add-Type -Language CSharp -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

public static class ViiperWindowsUptime
{
    [DllImport("kernel32.dll", ExactSpelling = true)]
    public static extern ulong GetTickCount64();
}
'@
}

$uptimeMethod = [ViiperWindowsUptime].GetMethod(
    'GetTickCount64', [Reflection.BindingFlags]'Public,Static')
$uptimeImport = $uptimeMethod.GetCustomAttributes(
    [Runtime.InteropServices.DllImportAttribute], $false)[0]
if ($uptimeMethod.ReturnType -ne [uint64] -or
    $uptimeImport.Value -cne 'kernel32.dll' -or
    -not $uptimeImport.ExactSpelling) {
    throw 'The local-test installer does not bind the exact Windows uptime API.'
}

function Get-WindowsBootBoundaryUtc {
    # Windows PowerShell 5.1 runs on .NET Framework, whose Environment type
    # does not expose TickCount64. Bind the native 64-bit uptime API directly
    # so long-running systems cannot suffer Environment.TickCount wraparound.
    $uptimeMilliseconds = [ViiperWindowsUptime]::GetTickCount64()
    return [DateTime]::UtcNow.Subtract(
        [TimeSpan]::FromMilliseconds([double]$uptimeMilliseconds))
}

function Remove-PreBootProtectedStagingDirectories {
    param([Parameter(Mandatory = $true)][string]$ProgramDataRoot)

    # A live sibling installer can own a same-boot staging directory before it
    # acquires the nested package mutex. Only reclaim exact protected stages
    # which predate this boot; Windows already terminated every possible owner.
    $bootBoundaryUtc = Get-WindowsBootBoundaryUtc
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
    # Exercise both sides of the reboot-boundary cleanup under the exact
    # inbox Windows PowerShell host used for installation. This regression
    # path must execute before an artifact can be published.
    $preflightOldStage = Join-Path $preflightProgramDataRoot (
        'VIIPER.LocalTestStage.' + [Guid]::NewGuid().ToString('N'))
    $preflightCurrentStage = Join-Path $preflightProgramDataRoot (
        'VIIPER.LocalTestStage.' + [Guid]::NewGuid().ToString('N'))
    try {
        Initialize-ProtectedStagingDirectory -Path $preflightOldStage
        Initialize-ProtectedStagingDirectory -Path $preflightCurrentStage
        $preflightBootBoundaryUtc = Get-WindowsBootBoundaryUtc
        [IO.Directory]::SetLastWriteTimeUtc(
            $preflightOldStage, $preflightBootBoundaryUtc.AddSeconds(-1))
        [IO.Directory]::SetLastWriteTimeUtc(
            $preflightCurrentStage, $preflightBootBoundaryUtc.AddSeconds(1))
        Remove-PreBootProtectedStagingDirectories `
            -ProgramDataRoot $preflightProgramDataRoot
        if (Test-Path -LiteralPath $preflightOldStage) {
            throw 'Pre-boot protected staging cleanup did not remove its test directory.'
        }
        if (-not (Test-Path -LiteralPath $preflightCurrentStage)) {
            throw 'Pre-boot protected staging cleanup removed a same-boot test directory.'
        }
    }
    finally {
        foreach ($preflightCleanupStage in @(
                $preflightOldStage, $preflightCurrentStage)) {
            if (Test-Path -LiteralPath $preflightCleanupStage) {
                Remove-ProtectedStagingDirectory `
                    -Path $preflightCleanupStage `
                    -ProgramDataRoot $preflightProgramDataRoot
            }
        }
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

if ($PreflightOnly) {
    $certificate.Dispose()
    Write-Output 'result=success operation=local-test-preflight changed=0 rebootRequired=0 rollback=not-needed exitCode=0'
    return
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
$trustJournalDirectory = Join-Path $programDataRoot 'VIIPER-TrustManager'
$stageDirectory = $null
$trustCapability = $null
try {
    Remove-PreBootProtectedStagingDirectories -ProgramDataRoot $programDataRoot
    $stageDirectory = Join-Path $programDataRoot (
        'VIIPER.LocalTestStage.' + [Guid]::NewGuid().ToString('N'))
    Initialize-ProtectedStagingDirectory -Path $stageDirectory
    $brokerPath = Copy-ExactBrokerToProtectedStage `
        -SourcePath $packageBrokerPath -DestinationDirectory $stageDirectory `
        -ExpectedLength ([long]$brokerEntry.length) -ExpectedSHA256 $brokerHash
    $trustCapability = New-LocalTestTrustCapability `
        -StageDirectory $stageDirectory -SourceRevision $source `
        -CertificatePath $certificatePath -CertificateSHA256 $certificateSha256 `
        -PackageLockSHA256 $actualPackageLockSha256 `
        -TrustJournalDirectory $trustJournalDirectory

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
        '--driver-validation-mode', 'local-test',
        '--local-test-trust-capability', $trustCapability.Path,
        '--expected-trust-capability-sha-256', $trustCapability.SHA256,
        '--local-test-certificate-path', $certificatePath,
        '--expected-local-test-certificate-sha-256', $certificateSha256,
        '--expected-local-test-package-lock-sha-256', $actualPackageLockSha256
    )
    $processResult = Invoke-JoinedNativeProcess `
        -FileName $brokerPath -Arguments $brokerArguments `
        -WorkingDirectory $stageDirectory -Started ([ref]$processStarted)
    $processResult.Output | ForEach-Object { Write-Host $_ }
    $exitCode = [int]$processResult.ExitCode

    Remove-LocalTestTrustCapability `
        -Capability $trustCapability -StageDirectory $stageDirectory
    $trustCapability = $null
    Remove-ProtectedStagingDirectory `
        -Path $stageDirectory -ProgramDataRoot $programDataRoot
    $stageDirectory = $null

    if ($exitCode -notin @(0, 3010)) {
        throw "Local VIIPER driver transaction failed with exit code $exitCode."
    }
    if ($exitCode -eq 3010) {
        Write-Warning 'The native transaction retained its durable trust/package authority at a reboot or indeterminate boundary. Restart and rerun this identical install command; do not remove certificates or journal files manually.'
        exit 3010
    }
}
catch {
    $failure = $_
    $cleanupFailures = [Collections.Generic.List[Exception]]::new()
    if ($null -ne $trustCapability -and $null -ne $stageDirectory) {
        try {
            Remove-LocalTestTrustCapability `
                -Capability $trustCapability -StageDirectory $stageDirectory
            $trustCapability = $null
        }
        catch {
            $cleanupFailures.Add($_.Exception)
        }
    }
    if ($null -ne $stageDirectory) {
        try {
            Remove-ProtectedStagingDirectory `
                -Path $stageDirectory -ProgramDataRoot $programDataRoot
            $stageDirectory = $null
        }
        catch {
            $cleanupFailures.Add($_.Exception)
        }
    }
    if ($cleanupFailures.Count -ne 0) {
        throw [AggregateException]::new(
            'Local VIIPER installation failed and protected staging cleanup also failed.',
            [Exception[]](@($failure.Exception) + $cleanupFailures.ToArray()))
    }
    throw $failure
}
finally {
    $certificate.Dispose()
}

Write-Host 'The exact local test-signed VIIPER UdeCx driver and native broker are installed, authenticated, and ready.'
Write-Host 'Next: enable Driver Verifier for ViiperUde.sys, reboot, then run Invoke-ViiperUdeLiveValidation.ps1 in LocalTest mode.'
}
finally {
    $installerScriptStream.Dispose()
}

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

Write-Host 'Initializing native bounded validation runner.'
if (-not ('ViiperUdeBoundedProcessRunner' -as [type])) {
    Add-Type -Language CSharp -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Diagnostics;
using System.IO;
using System.IO.Pipes;
using System.Runtime.InteropServices;
using System.Text;
using System.Threading;
using System.Threading.Tasks;

public sealed class ViiperUdeBoundedProcessResult
{
    public int ExitCode;
    public string StandardOutput;
    public string StandardError;
}

public static class ViiperUdeBoundedProcessRunner
{
    private const uint CREATE_SUSPENDED = 0x00000004;
    private const uint CREATE_NO_WINDOW = 0x08000000;
    private const uint STARTF_USESTDHANDLES = 0x00000100;
    private const uint JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x00002000;
    private const int JobObjectBasicAccountingInformation = 1;
    private const int JobObjectExtendedLimitInformation = 9;
    private const uint WAIT_OBJECT_0 = 0;
    private const uint WAIT_TIMEOUT = 258;
    private const uint INFINITE = 0xffffffff;
    private static readonly IntPtr InvalidHandleValue = new IntPtr(-1);

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    private struct STARTUPINFO
    {
        public int cb;
        public string lpReserved;
        public string lpDesktop;
        public string lpTitle;
        public uint dwX;
        public uint dwY;
        public uint dwXSize;
        public uint dwYSize;
        public uint dwXCountChars;
        public uint dwYCountChars;
        public uint dwFillAttribute;
        public uint dwFlags;
        public ushort wShowWindow;
        public ushort cbReserved2;
        public IntPtr lpReserved2;
        public IntPtr hStdInput;
        public IntPtr hStdOutput;
        public IntPtr hStdError;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct PROCESS_INFORMATION
    {
        public IntPtr hProcess;
        public IntPtr hThread;
        public uint dwProcessId;
        public uint dwThreadId;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct JOBOBJECT_BASIC_LIMIT_INFORMATION
    {
        public long PerProcessUserTimeLimit;
        public long PerJobUserTimeLimit;
        public uint LimitFlags;
        public UIntPtr MinimumWorkingSetSize;
        public UIntPtr MaximumWorkingSetSize;
        public uint ActiveProcessLimit;
        public UIntPtr Affinity;
        public uint PriorityClass;
        public uint SchedulingClass;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct IO_COUNTERS
    {
        public ulong ReadOperationCount;
        public ulong WriteOperationCount;
        public ulong OtherOperationCount;
        public ulong ReadTransferCount;
        public ulong WriteTransferCount;
        public ulong OtherTransferCount;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct JOBOBJECT_EXTENDED_LIMIT_INFORMATION
    {
        public JOBOBJECT_BASIC_LIMIT_INFORMATION BasicLimitInformation;
        public IO_COUNTERS IoInfo;
        public UIntPtr ProcessMemoryLimit;
        public UIntPtr JobMemoryLimit;
        public UIntPtr PeakProcessMemoryUsed;
        public UIntPtr PeakJobMemoryUsed;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct JOBOBJECT_BASIC_ACCOUNTING_INFORMATION
    {
        public long TotalUserTime;
        public long TotalKernelTime;
        public long ThisPeriodTotalUserTime;
        public long ThisPeriodTotalKernelTime;
        public uint TotalPageFaultCount;
        public uint TotalProcesses;
        public uint ActiveProcesses;
        public uint TotalTerminatedProcesses;
    }

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern IntPtr CreateJobObject(IntPtr attributes, string name);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool SetInformationJobObject(
        IntPtr job, int informationClass, IntPtr information, uint informationLength);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool QueryInformationJobObject(
        IntPtr job, int informationClass, out JOBOBJECT_BASIC_ACCOUNTING_INFORMATION information,
        uint informationLength, IntPtr returnLength);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool AssignProcessToJobObject(IntPtr job, IntPtr process);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool TerminateJobObject(IntPtr job, uint exitCode);

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool CreateProcess(
        string applicationName, StringBuilder commandLine, IntPtr processAttributes,
        IntPtr threadAttributes, bool inheritHandles, uint creationFlags, IntPtr environment,
        string currentDirectory, ref STARTUPINFO startupInfo,
        out PROCESS_INFORMATION processInformation);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern uint ResumeThread(IntPtr thread);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern uint WaitForSingleObject(IntPtr handle, uint milliseconds);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool GetExitCodeProcess(IntPtr process, out uint exitCode);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool TerminateProcess(IntPtr process, uint exitCode);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool CloseHandle(IntPtr handle);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern IntPtr GetStdHandle(int standardHandle);

    private static void ThrowLastError(string operation)
    {
        throw new Win32Exception(Marshal.GetLastWin32Error(), operation);
    }

    private static void ConfigureJob(IntPtr job)
    {
        JOBOBJECT_EXTENDED_LIMIT_INFORMATION limits =
            new JOBOBJECT_EXTENDED_LIMIT_INFORMATION();
        limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
        int size = Marshal.SizeOf(typeof(JOBOBJECT_EXTENDED_LIMIT_INFORMATION));
        IntPtr buffer = Marshal.AllocHGlobal(size);
        try
        {
            Marshal.StructureToPtr(limits, buffer, false);
            if (!SetInformationJobObject(
                    job, JobObjectExtendedLimitInformation, buffer, (uint)size))
            {
                ThrowLastError("SetInformationJobObject");
            }
        }
        finally
        {
            Marshal.FreeHGlobal(buffer);
        }
    }

    private static uint RemainingMilliseconds(Stopwatch clock, int timeoutMilliseconds)
    {
        long remaining = timeoutMilliseconds - clock.ElapsedMilliseconds;
        if (remaining <= 0)
        {
            return 0;
        }
        return remaining > int.MaxValue ? (uint)int.MaxValue : (uint)remaining;
    }

    private static bool WaitForJobEmpty(IntPtr job, uint timeoutMilliseconds)
    {
        Stopwatch clock = Stopwatch.StartNew();
        int size = Marshal.SizeOf(typeof(JOBOBJECT_BASIC_ACCOUNTING_INFORMATION));
        for (;;)
        {
            JOBOBJECT_BASIC_ACCOUNTING_INFORMATION accounting;
            if (!QueryInformationJobObject(
                    job, JobObjectBasicAccountingInformation, out accounting,
                    (uint)size, IntPtr.Zero))
            {
                ThrowLastError("QueryInformationJobObject");
            }
            if (accounting.ActiveProcesses == 0)
            {
                return true;
            }
            if (clock.ElapsedMilliseconds >= timeoutMilliseconds)
            {
                return false;
            }
            Thread.Sleep(10);
        }
    }

    public static ViiperUdeBoundedProcessResult Run(
        string applicationName, string commandLine, int timeoutMilliseconds)
    {
        if (timeoutMilliseconds <= 0)
        {
            throw new ArgumentOutOfRangeException("timeoutMilliseconds");
        }

        IntPtr job = IntPtr.Zero;
        PROCESS_INFORMATION process = new PROCESS_INFORMATION();
        bool processCreated = false;
        bool processAssigned = false;
        bool jobDrained = false;
        AnonymousPipeServerStream stdoutPipe = null;
        AnonymousPipeServerStream stderrPipe = null;
        StreamReader stdoutReader = null;
        StreamReader stderrReader = null;
        Task<string> stdoutTask = null;
        Task<string> stderrTask = null;
        Stopwatch clock = Stopwatch.StartNew();
        try
        {
            job = CreateJobObject(IntPtr.Zero, null);
            if (job == IntPtr.Zero)
            {
                ThrowLastError("CreateJobObject");
            }
            ConfigureJob(job);

            stdoutPipe = new AnonymousPipeServerStream(
                PipeDirection.In, HandleInheritability.Inheritable);
            stderrPipe = new AnonymousPipeServerStream(
                PipeDirection.In, HandleInheritability.Inheritable);
            STARTUPINFO startup = new STARTUPINFO();
            startup.cb = Marshal.SizeOf(typeof(STARTUPINFO));
            startup.dwFlags = STARTF_USESTDHANDLES;
            startup.hStdInput = GetStdHandle(-10);
            startup.hStdOutput = stdoutPipe.ClientSafePipeHandle.DangerousGetHandle();
            startup.hStdError = stderrPipe.ClientSafePipeHandle.DangerousGetHandle();

            if (!CreateProcess(
                    applicationName, new StringBuilder(commandLine), IntPtr.Zero, IntPtr.Zero,
                    true, CREATE_SUSPENDED | CREATE_NO_WINDOW, IntPtr.Zero, null,
                    ref startup, out process))
            {
                ThrowLastError("CreateProcess");
            }
            processCreated = true;
            stdoutPipe.DisposeLocalCopyOfClientHandle();
            stderrPipe.DisposeLocalCopyOfClientHandle();
            stdoutReader = new StreamReader(stdoutPipe, Encoding.UTF8, true, 4096, true);
            stderrReader = new StreamReader(stderrPipe, Encoding.UTF8, true, 4096, true);
            stdoutTask = stdoutReader.ReadToEndAsync();
            stderrTask = stderrReader.ReadToEndAsync();

            if (!AssignProcessToJobObject(job, process.hProcess))
            {
                ThrowLastError("AssignProcessToJobObject");
            }
            processAssigned = true;
            if (ResumeThread(process.hThread) == UInt32.MaxValue)
            {
                ThrowLastError("ResumeThread");
            }

            uint remaining = RemainingMilliseconds(clock, timeoutMilliseconds);
            uint wait = WaitForSingleObject(process.hProcess, remaining);
            if (wait == WAIT_TIMEOUT)
            {
                throw new TimeoutException("validation process exceeded its deadline");
            }
            if (wait != WAIT_OBJECT_0)
            {
                ThrowLastError("WaitForSingleObject(process)");
            }

            remaining = RemainingMilliseconds(clock, timeoutMilliseconds);
            if (!WaitForJobEmpty(job, remaining))
            {
                throw new TimeoutException("validation process tree exceeded its deadline");
            }
            jobDrained = true;

            Task[] outputTasks = new Task[] { stdoutTask, stderrTask };
            if (!Task.WaitAll(outputTasks, 10000))
            {
                throw new TimeoutException("validation output did not drain within 10000 ms");
            }
            uint exitCode;
            if (!GetExitCodeProcess(process.hProcess, out exitCode))
            {
                ThrowLastError("GetExitCodeProcess");
            }
            return new ViiperUdeBoundedProcessResult
            {
                ExitCode = unchecked((int)exitCode),
                StandardOutput = stdoutTask.GetAwaiter().GetResult(),
                StandardError = stderrTask.GetAwaiter().GetResult()
            };
        }
        catch (Exception failure)
        {
            string cleanupFailure = null;
            try
            {
                if (processAssigned && !jobDrained)
                {
                    if (!TerminateJobObject(job, 1))
                    {
                        ThrowLastError("TerminateJobObject");
                    }
                    if (!WaitForJobEmpty(job, 10000))
                    {
                        throw new TimeoutException(
                            "terminated validation job did not drain within 10000 ms");
                    }
                    jobDrained = true;
                }
                else if (processCreated && !processAssigned)
                {
                    if (!TerminateProcess(process.hProcess, 1))
                    {
                        ThrowLastError("TerminateProcess");
                    }
                    if (WaitForSingleObject(process.hProcess, 10000) != WAIT_OBJECT_0)
                    {
                        throw new TimeoutException(
                            "unassigned suspended validation process did not terminate within 10000 ms");
                    }
                }
            }
            catch (Exception cleanup)
            {
                cleanupFailure = cleanup.Message;
            }
            if (cleanupFailure != null)
            {
                throw new InvalidOperationException(
                    failure.Message + "; validation process cleanup failed: " + cleanupFailure,
                    failure);
            }
            throw;
        }
        finally
        {
            if (process.hThread != IntPtr.Zero && process.hThread != InvalidHandleValue)
            {
                CloseHandle(process.hThread);
            }
            if (process.hProcess != IntPtr.Zero && process.hProcess != InvalidHandleValue)
            {
                CloseHandle(process.hProcess);
            }
            if (job != IntPtr.Zero && job != InvalidHandleValue)
            {
                CloseHandle(job);
            }
            if (stdoutReader != null) stdoutReader.Dispose();
            if (stderrReader != null) stderrReader.Dispose();
            if (stdoutPipe != null) stdoutPipe.Dispose();
            if (stderrPipe != null) stderrPipe.Dispose();
        }
    }
}
'@
}
Write-Host 'Initialized native bounded validation runner.'

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

    $commandLine = ConvertTo-WindowsCommandLineArgument -Value $FilePath
    if ($Arguments.Count -gt 0) {
        $commandLine += ' ' + (($Arguments | ForEach-Object {
                    ConvertTo-WindowsCommandLineArgument -Value $_
                }) -join ' ')
    }
    try {
        Write-Host "Starting bounded validation: $Operation"
        $result = [ViiperUdeBoundedProcessRunner]::Run(
            $FilePath, $commandLine, $TimeoutMilliseconds)
        Write-Host "Completed bounded validation: $Operation"
        if (-not $SuppressOutput -and $result.StandardOutput) {
            Write-Host $result.StandardOutput.TrimEnd()
        }
        if (-not $SuppressOutput -and $result.StandardError) {
            Write-Host $result.StandardError.TrimEnd()
        }
        return [pscustomobject]@{
            ExitCode = $result.ExitCode
            StandardOutput = $result.StandardOutput
            StandardError = $result.StandardError
        }
    }
    catch {
        throw "$Operation failed closed: $($_.Exception.Message)"
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

function Test-ExpectedLocalTestTrustFailure {
    param(
        [Parameter(Mandatory = $true)]$Result,
        [Parameter(Mandatory = $true)][string]$ExpectedCertificateThumbprint,
        [Parameter(Mandatory = $true)][string]$TargetPath,
        [string]$CatalogPath
    )

    if ($Result.ExitCode -ne 1 -or
        $ExpectedCertificateThumbprint -cnotmatch '^[0-9A-F]{40}$') {
        return $false
    }
    $evidence = ([string]$Result.StandardOutput) + "`n" +
        ([string]$Result.StandardError)
    $rootTrustError = '(?ims)^SignTool Error: A certificate chain processed, but terminated in a root\s*\r?\n\s*certificate which is not trusted by the trust provider\.\s*$'
    if (@([regex]::Matches($evidence, $rootTrustError)).Count -ne 1 -or
        @([regex]::Matches($evidence,
            '(?im)^\s*SHA1 hash:\s*' + [regex]::Escape($ExpectedCertificateThumbprint) + '\s*$')).Count -ne 1 -or
        @([regex]::Matches($evidence, '(?im)^Number of warnings:\s*0\s*$')).Count -ne 1 -or
        @([regex]::Matches($evidence, '(?im)^Number of errors:\s*1\s*$')).Count -ne 1 -or
        @([regex]::Matches($evidence,
            '(?im)^Verifying:\s*' + [regex]::Escape($TargetPath) + '\s*$')).Count -ne 1) {
        return $false
    }
    if (-not [string]::IsNullOrWhiteSpace($CatalogPath) -and
        @([regex]::Matches($evidence,
            '(?im)^File is signed in catalog:\s*' + [regex]::Escape($CatalogPath) + '\s*$')).Count -ne 1) {
        return $false
    }
    return $true
}

function Assert-DriverSignature {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path,

        [Parameter(Mandatory = $true)]
        [ValidateSet('LocalTest', 'ControlledTest', 'Production')]
        [string]$Mode,

        [string]$ExpectedLocalTestCertificateSha256,

        [switch]$AllowUntrustedLocalTestRoot
    )

    $signature = Get-BoundedAuthenticodeSignature -Path $Path
    try {
        if ($signature.Status -cne 'Valid' -and
            -not ($Mode -eq 'LocalTest' -and $AllowUntrustedLocalTestRoot -and
                $signature.Status -ceq 'UnknownError')) {
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
$localTestCertificateThumbprint = $null
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
    $localTestCertificateThumbprint = $localTestCertificate.Thumbprint.ToUpperInvariant()
    $chain = [Security.Cryptography.X509Certificates.X509Chain]::new()
    try {
        $chain.ChainPolicy.RevocationMode =
            [Security.Cryptography.X509Certificates.X509RevocationMode]::NoCheck
        $chain.ChainPolicy.VerificationFlags =
            [Security.Cryptography.X509Certificates.X509VerificationFlags]::AllowUnknownCertificateAuthority
        $chainValid = $chain.Build($localTestCertificate)
        $chainStatuses = @($chain.ChainStatus)
        $onlyExpectedTrustStatus = $chainStatuses.Count -eq 0 -or
            ($chainStatuses.Count -eq 1 -and
                $chainStatuses[0].Status -eq
                    [Security.Cryptography.X509Certificates.X509ChainStatusFlags]::UntrustedRoot)
        $ekuOids = Get-CertificateEkuOids -Certificate $localTestCertificate
        if (-not $chainValid -or -not $onlyExpectedTrustStatus -or
            $chain.ChainElements.Count -ne 1 -or
            $localTestCertificate.Subject -cne $localTestCertificate.Issuer -or
            $localTestCertificate.NotBefore -gt [DateTime]::Now -or
            $localTestCertificate.NotAfter -lt [DateTime]::Now -or
            -not $ekuOids.Contains('1.3.6.1.5.5.7.3.3') -or
            $localTestCertificateThumbprint -cnotmatch '^[0-9A-F]{40}$') {
            throw 'The local-test certificate is not a current, self-issued, code-signing certificate with a valid self-chain.'
        }
    }
    finally {
        $chain.Dispose()
    }
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
    -ABIMajor 1 -ABIMinor 12 -Capabilities 29
if ($manifest.schema -ne 2 -or
    [string]$manifest.sourceRevision -cne $ExpectedSourceRevision.ToLowerInvariant() -or
    [string]$manifest.driverPackageVersion -cne $driverPackageVersion -or
    [int]$manifest.driverABIMajor -ne 1 -or [int]$manifest.driverABIMinor -ne 12 -or
    [string]$manifest.driverCapabilities -cne '0x0000001d' -or
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
        -ExpectedLocalTestCertificateSha256 $localTestCertificateSha256 `
        -AllowUntrustedLocalTestRoot:$RequireLocalTestToolchainValidation
}
$requireExternalTools = $ValidationMode -ne 'LocalTest' -or $RequireLocalTestToolchainValidation
if ($requireExternalTools) {
    $signTool = Get-Command signtool.exe -ErrorAction Stop
    foreach ($name in @('ViiperUde.cat', 'ViiperUde.sys')) {
        $policy = if ($ValidationMode -eq 'LocalTest') { '/pa' } else { '/kp' }
        $exitCode = Invoke-BoundedValidationTool -FilePath $signTool.Source `
            -Arguments @('verify', $policy, '/v', $files[$name]) `
            -Operation "SignTool signature validation for '$name'"
        $expectedUntrustedRoot = $ValidationMode -eq 'LocalTest' -and
            (Test-ExpectedLocalTestTrustFailure -Result $exitCode `
                -ExpectedCertificateThumbprint $localTestCertificateThumbprint `
                -TargetPath $files[$name])
        if ($exitCode.ExitCode -ne 0 -and -not $expectedUntrustedRoot) {
            throw "Signature policy validation failed for '$name' with exit code $($exitCode.ExitCode)."
        }
    }
    foreach ($name in @('ViiperUde.inf', 'ViiperUde.sys')) {
        $policy = if ($ValidationMode -eq 'LocalTest') { '/pa' } else { '/kp' }
        $exitCode = Invoke-BoundedValidationTool -FilePath $signTool.Source `
            -Arguments @('verify', $policy, '/v', '/c', $files['ViiperUde.cat'], $files[$name]) `
            -Operation "SignTool catalog membership validation for '$name'"
        $expectedUntrustedRoot = $ValidationMode -eq 'LocalTest' -and
            (Test-ExpectedLocalTestTrustFailure -Result $exitCode `
                -ExpectedCertificateThumbprint $localTestCertificateThumbprint `
                -TargetPath $files[$name] -CatalogPath $files['ViiperUde.cat'])
        if ($exitCode.ExitCode -ne 0 -and -not $expectedUntrustedRoot) {
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

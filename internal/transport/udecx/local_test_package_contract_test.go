package udecx

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLocalTestPackageUsesNativeTrustTransaction(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	read := func(path ...string) string {
		t.Helper()
		contents, err := os.ReadFile(filepath.Join(append([]string{root}, path...)...))
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Join(path...), err)
		}
		return strings.ReplaceAll(string(contents), "\r\n", "\n")
	}

	workflow := read(".github", "workflows", "native-ude.yml")
	for _, required := range []string{
		"workflow_dispatch:",
		"New-ViiperUdeLocalTestPackage.ps1",
		"ViiperUde-x64-local-test-${{ github.sha }}",
		"path: native/udecx/x64/Release/ViiperUdeLocalTest/**",
		"retention-days: 7",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("local-test workflow omitted %q", required)
		}
	}

	composer := read("native", "udecx", "tools", "New-ViiperUdeLocalTestPackage.ps1")
	for _, required := range []string{
		"[string]$BrokerPath",
		"[string]$TestCertificatePath",
		"testSignerCertificateSha256",
		"installerScriptSha256",
		"local-test-package.lock.json",
		"$broker native-package-install --help",
		"'--local-test-trust-capability'",
		"'--expected-trust-capability-sha-256'",
		"'--local-test-certificate-path'",
		"'--expected-local-test-certificate-sha-256'",
		"'--expected-local-test-package-lock-sha-256'",
		"System32\\WindowsPowerShell\\v1.0\\powershell.exe",
		"-PreflightOnly",
	} {
		if !strings.Contains(composer, required) {
			t.Fatalf("local-test composer omitted %q", required)
		}
	}

	installer := read("native", "udecx", "tools", "Install-ViiperUdeLocalTest.ps1")
	for _, required := range []string{
		"[string]$TargetUserSID",
		"[string]$ExpectedPackageLockSHA256",
		"$installerScriptStream",
		"$lock.installerScriptSha256 -cne $actualInstallerScriptSha256",
		"out-of-band workflow digest",
		"Assert-ProtectedStagingDirectory",
		"Copy-ExactBrokerToProtectedStage",
		"[IO.FileOptions]::WriteThrough",
		"Remove-PreBootProtectedStagingDirectories",
		"Invoke-JoinedNativeProcess",
		"$process.WaitForExit()",
		"New-LocalTestTrustCapability",
		"Remove-LocalTestTrustCapability",
		"viiper.native.local-test-trust-capability/v1",
		"parentCreationFileTime",
		"certificatePath = [IO.Path]::GetFullPath($CertificatePath)",
		"trustJournalSchema = 'viiper.native.local-test-trust-ownership/v1'",
		"trustJournalDirectory = [IO.Path]::GetFullPath($TrustJournalDirectory)",
		"'--local-test-certificate-path', $certificatePath",
		"'--expected-local-test-certificate-sha-256', $certificateSha256",
		"'--expected-local-test-package-lock-sha-256', $actualPackageLockSha256",
		"-AcknowledgeDisposableTestMachine",
		"testsigning\\s+Yes",
		"[switch]$PreflightOnly",
		"operation=local-test-preflight",
		"retained its durable trust/package authority",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("local-test installer omitted %q", required)
		}
	}
	for _, forbidden := range []string{
		"[Security.Cryptography.X509Certificates.X509Store]::new(",
		"ViiperLocalTestCertificateStore",
		"CertAddEncodedCertificateToStore(",
		"CertDeleteCertificateFromStore(",
		"Open-LocalTestTrustOwnershipJournal",
		"Complete-LocalTestTrustOwnershipInstall",
		"Restore-LocalTestTrustOwnershipBaseline",
		"local-test-trust-preparing-v1.json",
		"local-test-trust-pending-v1.json",
		"local-test-trust-owned-v1.json",
		"Enter-LocalTestTrustLease",
		"Exit-LocalTestTrustLease",
		"$stream.Lock(0, 1)",
		"$stream.Unlock(0, 1)",
		"Test-SettledLocalTestFailure",
		"trustJournalState",
		"trustJournalSha256",
		"leasePath",
		"$store.Add($certificate)",
		"$store.Remove(",
	} {
		if strings.Contains(installer, forbidden) {
			t.Fatalf("PowerShell retained forbidden trust ownership operation %q", forbidden)
		}
	}

	capabilityCreate := strings.LastIndex(installer, "$trustCapability = New-LocalTestTrustCapability")
	transactionLaunch := strings.LastIndex(installer, "$processResult = Invoke-JoinedNativeProcess")
	capabilityCleanup := strings.LastIndex(installer, "Remove-LocalTestTrustCapability")
	stageCleanup := strings.LastIndex(installer, "Remove-ProtectedStagingDirectory")
	if capabilityCreate < 0 || transactionLaunch <= capabilityCreate ||
		capabilityCleanup <= transactionLaunch || stageCleanup <= capabilityCleanup {
		t.Fatal("PowerShell does not hold the sealed parent capability through the joined native child")
	}

	packageCommand := read("internal", "cmd", "native_package.go")
	packageWindows := read("internal", "cmd", "native_package_windows.go")
	for _, required := range []string{
		"LocalTestCertificatePath",
		"localTestCertificatePath",
		`default:"production" enum:"production,local-test"`,
	} {
		if !strings.Contains(packageCommand, required) {
			t.Fatalf("native package command omitted %q", required)
		}
	}
	for _, required := range []string{
		"initializeNativePackageRecoveryTrustLease",
		"acquireNativePackageRecoveryTrustLease",
		"prepareLocalTestTrust",
		"commitLocalTestTrust",
		"maybeRestoreLocalTestTrustAfterFailure",
		"publishNativePackageLocalTestTrustPreparing",
		"transitionNativePackageLocalTestTrustRecord",
		"restoreNativePackageLocalTestTrustStores",
		"proveNativePackageLocalTestTopologyAbsent",
		"topology-success-before-owned",
		"Trust -> Package -> Service",
	} {
		if !strings.Contains(packageWindows, required) {
			t.Fatalf("native trust transaction omitted %q", required)
		}
	}
	trustAcquire := strings.Index(packageWindows, "acquireNativePackageRecoveryTrustLease(ctx, trustDeadline)")
	packageAcquire := strings.Index(packageWindows, "acquireNamedNativePackageMutex(nativePackageMutexName")
	serviceAcquire := strings.Index(packageWindows, "acquireNativeInstallMutex(budget)")
	if trustAcquire < 0 || packageAcquire <= trustAcquire || serviceAcquire <= packageAcquire {
		t.Fatal("normal native transaction does not acquire Trust -> Package -> Service")
	}
}

func TestLocalTestCapabilitySerializesOnWindowsPowerShellHosts(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell contract")
	}
	root := filepath.Join("..", "..", "..")
	installer, err := filepath.Abs(filepath.Join(
		root, "native", "udecx", "tools", "Install-ViiperUdeLocalTest.ps1"))
	if err != nil {
		t.Fatalf("resolve local-test installer: %v", err)
	}
	hosts := []string{filepath.Join(
		os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")}
	if pwsh, err := exec.LookPath("pwsh.exe"); err == nil {
		hosts = append(hosts, pwsh)
	}
	const behaviorContract = `
$ErrorActionPreference = 'Stop'
$tokens = $null
$errors = $null
$source = Get-Content -LiteralPath $env:VIIPER_INSTALLER_CONTRACT_PATH -Raw
[void][Management.Automation.Language.Parser]::ParseFile(
    $env:VIIPER_INSTALLER_CONTRACT_PATH, [ref]$tokens, [ref]$errors)
if ($errors.Count -ne 0) { throw ($errors | ForEach-Object ToString | Out-String) }
foreach ($forbidden in @(
        'ViiperLocalTestCertificateStore', 'CertAddEncodedCertificateToStore',
        'CertDeleteCertificateFromStore', 'Enter-LocalTestTrustLease',
        'Open-LocalTestTrustOwnershipJournal', 'Test-SettledLocalTestFailure')) {
    if ($source.Contains($forbidden)) { throw "PowerShell retained forbidden trust writer $forbidden" }
}
$csharpBlocks = @([regex]::Matches(
    $source, "(?s)Add-Type -Language CSharp -TypeDefinition @'\r?\n(?<source>.*?)\r?\n'@") |
    ForEach-Object { $_.Groups['source'].Value } |
    Where-Object { $_ -match 'public static class ViiperLocalTestStagingNative' })
if ($csharpBlocks.Count -ne 1) { throw 'Protected-staging native source was not found exactly once.' }
Add-Type -Language CSharp -TypeDefinition $csharpBlocks[0]
$capabilityStart = $source.IndexOf('function New-LocalTestTrustCapability')
$capabilityEnd = $source.IndexOf('function Remove-LocalTestTrustCapability', $capabilityStart)
if ($capabilityStart -lt 0 -or $capabilityEnd -le $capabilityStart) {
    throw 'Capability function extent was not found.'
}
$capabilitySource = $source.Substring($capabilityStart, $capabilityEnd - $capabilityStart)
$orderedFields = @(
    'schema =', 'nonce =', 'parentPid =', 'parentCreationFileTime =',
    'sourceRevision =', 'certificatePath =', 'certificateSha256 =',
    'packageLockSha256 =', 'trustJournalSchema =', 'trustJournalDirectory =')
$previous = -1
foreach ($field in $orderedFields) {
    $matches = [regex]::Matches(
        $capabilitySource, ('(?m)^\s*' + [regex]::Escape($field)))
    if ($matches.Count -ne 1) { throw "Capability field occurrence failed at $field" }
    $position = $matches[0].Index
    if ($position -le $previous) { throw "Capability field order failed at $field" }
    $previous = $position
}
$payload = [ordered]@{
    schema = 'viiper.native.local-test-trust-capability/v1'
    nonce = '01010101010101010101010101010101'
    parentPid = [uint32]1234
    parentCreationFileTime = [uint64]134000000000000000
    sourceRevision = ('a' * 40)
    certificatePath = 'C:\package\ViiperUdeTest.cer'
    certificateSha256 = ('b' * 64)
    packageLockSha256 = ('c' * 64)
    trustJournalSchema = 'viiper.native.local-test-trust-ownership/v1'
    trustJournalDirectory = 'C:\ProgramData\VIIPER-TrustManager'
}
$json = $payload | ConvertTo-Json -Compress -Depth 2
$roundTrip = $json | ConvertFrom-Json
if ([string]$roundTrip.schema -cne 'viiper.native.local-test-trust-capability/v1' -or
    [string]$roundTrip.certificatePath -cne 'C:\package\ViiperUdeTest.cer' -or
    [string]$roundTrip.trustJournalSchema -cne 'viiper.native.local-test-trust-ownership/v1' -or
    [string]$roundTrip.trustJournalDirectory -cne 'C:\ProgramData\VIIPER-TrustManager') {
    throw "Capability JSON did not round-trip exactly: $json"
}
if ($json.IndexOf([char]13) -ge 0 -or $json.IndexOf([char]10) -ge 0) {
    throw 'Capability JSON contains noncanonical framing.'
}
`
	for _, host := range hosts {
		host := host
		t.Run(filepath.Base(filepath.Dir(host))+"-"+filepath.Base(host), func(t *testing.T) {
			command := exec.Command(host, "-NoProfile", "-NonInteractive", "-Command", behaviorContract)
			command.Env = append(os.Environ(), "VIIPER_INSTALLER_CONTRACT_PATH="+installer)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("capability host contract failed: %v\n%s", err, output)
			}
		})
	}
}
func TestLocalTestBootBoundaryRunsOnWindowsPowerShell51(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell contract")
	}

	root := filepath.Join("..", "..", "..")
	installer, err := filepath.Abs(filepath.Join(
		root, "native", "udecx", "tools", "Install-ViiperUdeLocalTest.ps1"))
	if err != nil {
		t.Fatalf("resolve local-test installer: %v", err)
	}
	powerShell := filepath.Join(
		os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(powerShell); err != nil {
		t.Fatalf("locate Windows PowerShell: %v", err)
	}

	const behaviorContract = `
$ErrorActionPreference = 'Stop'
$source = Get-Content -LiteralPath $env:VIIPER_INSTALLER_CONTRACT_PATH -Raw
$csharpBlocks = @([regex]::Matches(
    $source, "(?s)Add-Type -Language CSharp -TypeDefinition @'\r?\n(?<source>.*?)\r?\n'@") |
    ForEach-Object { $_.Groups['source'].Value } |
    Where-Object { $_ -match 'public static class ViiperWindowsUptime' })
if ($csharpBlocks.Count -ne 1) { throw 'Embedded Windows-uptime source was not found exactly once.' }
Add-Type -Language CSharp -TypeDefinition $csharpBlocks[0]
$start = $source.IndexOf('function Get-WindowsBootBoundaryUtc')
$end = $source.IndexOf('function Remove-PreBootProtectedStagingDirectories', $start)
if ($start -lt 0 -or $end -le $start) { throw 'Windows boot-boundary function was not found.' }
Invoke-Expression $source.Substring($start, $end - $start)
$before = [DateTime]::UtcNow
$boundary = Get-WindowsBootBoundaryUtc
$after = [DateTime]::UtcNow
$uptime = [TimeSpan]::FromMilliseconds([double][ViiperWindowsUptime]::GetTickCount64())
$lower = $before.Subtract($uptime).AddSeconds(-2)
$upper = $after.Subtract($uptime).AddSeconds(2)
if ($boundary.Kind -ne [DateTimeKind]::Utc -or
    $boundary -lt $lower -or $boundary -gt $upper) {
    throw "Windows boot boundary was outside the native uptime interval: $boundary"
}
if ($PSVersionTable.PSEdition -cne 'Desktop' -or $PSVersionTable.PSVersion.Major -ne 5) {
    throw "Expected Windows PowerShell 5.1, got $($PSVersionTable.PSVersion)."
}
`
	command := exec.Command(
		powerShell, "-NoProfile", "-NonInteractive", "-Command", behaviorContract)
	command.Env = append(os.Environ(), "VIIPER_INSTALLER_CONTRACT_PATH="+installer)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Windows PowerShell boot-boundary contract failed: %v\n%s", err, output)
	}
}

func TestLocalTestValidationCannotWeakenProduction(t *testing.T) {
	root := filepath.Join("..", "..", "..", "native", "udecx", "tools")
	contents, err := os.ReadFile(filepath.Join(root, "Test-ViiperUdeSignedPackage.ps1"))
	if err != nil {
		t.Fatalf("read signed-package validator: %v", err)
	}
	contract := strings.ReplaceAll(string(contents), "\r\n", "\n")
	for _, required := range []string{
		"[ValidateSet('LocalTest', 'ControlledTest', 'Production')]",
		"Invoke-BoundedValidationTool",
		"Get-BoundedAuthenticodeSignature",
		"'-NoProfile', '-NonInteractive', '-EncodedCommand'",
		"CREATE_SUSPENDED | CREATE_NO_WINDOW",
		"JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE",
		"new AnonymousPipeServerStream(",
		"QueryInformationJobObject(",
		"AssignProcessToJobObject(job, process.hProcess)",
		"ResumeThread(process.hThread)",
		"TerminateJobObject(job, 1)",
		"WaitForJobEmpty(job, remaining)",
		"Task.WaitAll(outputTasks, 10000)",
		"$ValidationMode -eq 'LocalTest'",
		"testSignerCertificateSha256",
		"Production validation requires a release-eligible HLK/WHCP",
		"HLK/WHCP",
		"Assert-DriverSignature",
		"Microsoft Corporation",
		"$requireExternalTools = $ValidationMode -ne 'LocalTest' -or $RequireLocalTestToolchainValidation",
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("signature route separation omitted %q", required)
		}
	}
	assign := strings.Index(contract, "AssignProcessToJobObject(job, process.hProcess)")
	resume := strings.Index(contract, "ResumeThread(process.hThread)")
	if assign < 0 || resume < 0 || assign > resume {
		t.Fatal("validation child is not assigned to its private job while still suspended")
	}
	for _, forbidden := range []string{
		"& $signTool.Source verify",
		"& $infVerif.Source",
	} {
		if strings.Contains(contract, forbidden) {
			t.Fatalf("signature validation retained unbounded child execution %q", forbidden)
		}
	}
}

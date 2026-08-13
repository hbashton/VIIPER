package udecx

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLocalTestPackageUsesFullTransactionalNativeBackend(t *testing.T) {
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
		"[Security.Cryptography.X509Certificates.X509Store]::new(",
		"ViiperNativeCertificateStore",
		"CertAddEncodedCertificateToStore(",
		"CertFindCertificateInStore(",
		"CertDeleteCertificateFromStore(found)",
		"CERT_STORE_ADD_NEW",
		"$addedTrust += $storeName",
		"CERT_SYSTEM_STORE_LOCAL_MACHINE",
		"[Security.Cryptography.X509Certificates.StoreName]::Root",
		"[Security.Cryptography.X509Certificates.StoreName]::TrustedPublisher",
		"[Security.Cryptography.X509Certificates.StoreLocation]::LocalMachine",
		"foreach ($storeName in $addedTrust)",
		"$cleanupErrors.Add(",
		"$certificate.Dispose()",
		"-BrokerPath native/udecx/x64/Release/viiper.exe",
		"ViiperUde-x64-local-test-${{ github.sha }}",
		"path: native/udecx/x64/Release/ViiperUdeLocalTest/**",
		"retention-days: 7",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("local-test workflow omitted %q", required)
		}
	}
	for _, forbidden := range []string{
		"native/udecx/x64/Release/**",
		"native/udecx/driver/x64/Release/**",
		"native/udecx/package/x64/Release/**",
		"$store.Add($certificate)",
		"$store.Remove($exactMatch[0])",
		"certutil.exe",
		"Invoke-BoundedCertUtil",
		"CERT_SYSTEM_STORE_CURRENT_USER",
		"[Security.Cryptography.X509Certificates.StoreLocation]::CurrentUser",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("local-test workflow uploads broad build tree %q", forbidden)
		}
	}

	composer := read("native", "udecx", "tools", "New-ViiperUdeLocalTestPackage.ps1")
	for _, required := range []string{
		"[string]$BrokerPath",
		"[string]$TestCertificatePath",
		"$certificateSha256 = Get-CertificateSha256 $expectedCertificate",
		"Resolve-ExactInput $BrokerPath 'viiper.exe'",
		"signingRoute = 'LocalTest'",
		"releaseEligible = $false",
		"testSignerCertificateSha256",
		"installerScriptSha256",
		"-ValidationMode LocalTest",
		"-RequireLocalTestToolchainValidation",
		"local-test-package.lock.json",
		"Local test package lock SHA-256: $lockSha256",
		"$broker native-package-install --help",
		"$expectedBrokerFlags",
		"$broker native-package-broker-commit --help",
		"$expectedBrokerCommitFlags",
		"'--expected-token-sha-256'",
		"'--expected-broker-sha-256'",
		"$helper verify (Join-Path $driverDirectory 'ViiperUde.inf')",
		"result=success operation=verify changed=0 rebootRequired=0 rollback=not-needed exitCode=0",
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
		"$lockAlgorithm.ComputeHash($lockBytes)",
		"@(Compare-Object -ReferenceObject $wanted -DifferenceObject $actual -CaseSensitive).Count",
		"out-of-band workflow digest",
		"O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)",
		"[IO.Directory]::CreateDirectory($Path, $expectedSecurity)",
		"$directory.SetAccessControl($expectedSecurity)",
		"Assert-ProtectedStagingDirectory",
		"$actualSecurity.AreAccessRulesProtected",
		"$actualSecurity.GetOwner([Security.Principal.SecurityIdentifier])",
		"$actualSecurity.GetAccessRules(",
		"@('S-1-5-18', 'S-1-5-32-544')",
		"[Security.AccessControl.FileSystemRights]::FullControl",
		"[Security.AccessControl.InheritanceFlags]::ContainerInherit",
		"[Security.AccessControl.InheritanceFlags]::ObjectInherit",
		"Copy-ExactBrokerToProtectedStage",
		"[IO.FileShare]::Read",
		"[IO.FileOptions]::WriteThrough",
		"$lockByPath['viiper.exe']",
		"Remove-ProtectedStagingDirectory",
		"Remove-PreBootProtectedStagingDirectories",
		"public static class ViiperWindowsUptime",
		"public static extern ulong GetTickCount64();",
		"Get-WindowsBootBoundaryUtc",
		"$_.LastWriteTimeUtc -lt $bootBoundaryUtc",
		"Invoke-JoinedNativeProcess",
		"if (-not $process.Start())",
		"$Started.Value = $true",
		"$process.WaitForExit()",
		"$retainTrustOnFailure = $processStarted",
		"'--expected-broker-sha-256', $brokerHash",
		"'--expected-helper-sha-256', $helperHash",
		"'--expected-manifest-sha-256', $manifestHash",
		"'--expected-inf-sha-256', $infHash",
		"'--expected-sys-sha-256', $sysHash",
		"'--expected-cat-sha-256', $catHash",
		"'--target-user-sid', $TargetUserSID",
		"'--driver-validation-mode', 'local-test'",
		"-AcknowledgeDisposableTestMachine",
		"testsigning\\s+Yes",
		"Restart, rerun this identical install command",
		"[switch]$PreflightOnly",
		"operation=local-test-preflight",
		"ViiperLocalTestCertificateStore",
		"CertAddEncodedCertificateToStore(",
		"CertFindCertificateInStore(",
		"CertDeleteCertificateFromStore(found)",
		"CERT_STORE_ADD_NEW",
		"CRYPT_E_NOT_FOUND",
		"Get-ExactLocalTestTrustState",
		"$addedStores.Add($storeName)",
		"[Security.Cryptography.X509Certificates.OpenFlags]::ReadOnly",
		"action=verify-add result=present",
		"action=verify-cleanup result=absent",
		"LocalMachine\\$storeName trust cleanup failed during $cleanupAction.",
		"ExactSpelling = true",
		"[Parameter(Mandatory = $true)][int]$ProcessExitCode",
		"[string]::Join([Environment]::NewLine, [string[]]$Lines)",
		"[int]::TryParse($match.Groups['exit'].Value, [ref]$proofExitCode)",
		"$proofExitCode -ne $ProcessExitCode",
		"-Lines $output -ProcessExitCode $exitCode",
		"$certificateStoreOpenMethod = [ViiperLocalTestCertificateStore].GetMethod(",
		"$certificateStoreOpenImport.ExactSpelling",
		"does not bind the exact CertOpenStore entry point",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("local-test installer omitted %q", required)
		}
	}
	for _, required := range []string{
		"System32\\WindowsPowerShell\\v1.0\\powershell.exe",
		"-PreflightOnly",
		"Windows PowerShell 5.1 local-test installer preflight failed",
	} {
		if !strings.Contains(composer, required) {
			t.Fatalf("local-test composer omitted Windows PowerShell preflight contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"& $helperPath install",
		"Test-ViiperUdeSignedPackage.ps1",
		"git.exe",
		"status --porcelain",
		"'--expected-broker-sha256'",
		"'--expected-helper-sha256'",
		"'--expected-manifest-sha256'",
		"'--expected-inf-sha256'",
		"'--expected-sys-sha256'",
		"'--expected-cat-sha256'",
		"GetSecurityDescriptorBinaryForm",
		"BinaryLength",
		"$store.Add($certificate)",
		"$store.Remove(",
		"[Environment]::TickCount64",
		"[Environment]::TickCount",
	} {
		if strings.Contains(installer, forbidden) {
			t.Fatalf("local-test elevated path retained unsafe dependency %q", forbidden)
		}
	}

	cleanupStart := strings.Index(installer, "function Remove-NewLocalTestTrust")
	cleanupEnd := strings.Index(installer, "function Test-SettledLocalTestFailure")
	if cleanupStart < 0 || cleanupEnd <= cleanupStart {
		t.Fatal("local-test installer trust cleanup function is missing or malformed")
	}
	cleanup := installer[cleanupStart:cleanupEnd]
	remove := strings.Index(cleanup, "[ViiperLocalTestCertificateStore]::Remove(")
	verify := strings.LastIndex(cleanup, "Get-ExactLocalTestTrustState -StoreName $storeName")
	absence := strings.Index(cleanup, "if ($cleanupState.ExactCount -ne 0)")
	if remove < 0 || verify <= remove || absence <= verify {
		t.Fatal("local-test installer does not verify persisted exact-certificate absence after native removal")
	}
	if strings.Count(cleanup, "catch {") != 1 ||
		strings.Index(cleanup, "$removalErrors.Add(") < strings.Index(cleanup, "catch {") {
		t.Fatal("local-test installer does not independently aggregate per-store cleanup failures")
	}
	preflightStart := strings.Index(installer, "if ($PreflightOnly) {")
	interopCompile := strings.Index(installer, "if (-not ('ViiperLocalTestCertificateStore' -as [type])) {")
	interopVerify := strings.Index(installer, "$certificateStoreOpenMethod = [ViiperLocalTestCertificateStore].GetMethod(")
	preflightSuccess := strings.Index(installer,
		"Write-Output 'result=success operation=local-test-preflight changed=0 rebootRequired=0 rollback=not-needed exitCode=0'")
	trustAddCall := strings.Index(installer, "[ViiperLocalTestCertificateStore]::Add(")
	trustRemoveCall := strings.Index(installer, "[ViiperLocalTestCertificateStore]::Remove(")
	if preflightStart < 0 || interopCompile <= preflightStart || interopVerify <= interopCompile ||
		preflightSuccess <= interopVerify || trustAddCall <= preflightSuccess ||
		trustRemoveCall <= preflightSuccess {
		t.Fatal("local-test preflight can return success before compiling and inspecting the exact certificate-store interop")
	}
	if strings.Contains(installer[preflightStart:interopCompile], "return") {
		t.Fatal("local-test preflight can return before compiling the exact certificate-store interop")
	}
	preflightCleanup := strings.Index(installer[preflightStart:preflightSuccess],
		"Remove-PreBootProtectedStagingDirectories")
	preflightOldAssertion := strings.Index(installer[preflightStart:preflightSuccess],
		"Pre-boot protected staging cleanup did not remove its test directory.")
	preflightCurrentAssertion := strings.Index(installer[preflightStart:preflightSuccess],
		"Pre-boot protected staging cleanup removed a same-boot test directory.")
	if preflightCleanup < 0 || preflightOldAssertion <= preflightCleanup ||
		preflightCurrentAssertion <= preflightOldAssertion {
		t.Fatal("local-test preflight does not execute both sides of pre-boot staging cleanup")
	}
	settledStart := strings.Index(installer, "function Test-SettledLocalTestFailure")
	settledEnd := strings.Index(installer, "$trustCommitted = $false")
	if settledStart < 0 || settledEnd <= settledStart {
		t.Fatal("local-test installer settled-failure predicate is missing or malformed")
	}
	settled := installer[settledStart:settledEnd]
	if strings.Contains(settled, "$Lines | Out-String") {
		t.Fatal("local-test installer formats and host-wraps native settled-failure proof before parsing")
	}
	joinLines := strings.Index(settled, "[string]::Join([Environment]::NewLine, [string[]]$Lines)")
	parseProof := strings.Index(settled, "[regex]::Matches($proofText, $pattern)")
	parseExit := strings.Index(settled, "[int]::TryParse($match.Groups['exit'].Value, [ref]$proofExitCode)")
	bindExit := strings.Index(settled, "$proofExitCode -ne $ProcessExitCode")
	classify := strings.Index(settled, "$match.Groups['changed'].Value -ceq '0'")
	if joinLines < 0 || parseProof <= joinLines || parseExit <= parseProof ||
		bindExit <= parseExit || classify <= bindExit {
		t.Fatal("local-test installer classifies settled proof before binding it to the observed child exit")
	}

	packageCommand := read("internal", "cmd", "native_package.go")
	packageWindows := read("internal", "cmd", "native_package_windows.go")
	helperSource := read("native", "udecx", "tools", "ViiperUdeCtl.cpp")
	for _, required := range []string{
		"BuildBrokerCommitCommandLine(",
		`L" --expected-token-sha-256 "`,
		`L" --expected-broker-sha-256 "`,
		`L"self-test-broker-command"`,
	} {
		if !strings.Contains(helperSource, required) {
			t.Fatalf("native helper omitted nested broker command contract %q", required)
		}
	}
	for _, obsolete := range []string{
		`L" --expected-token-sha256 "`,
		`L" --expected-broker-sha256 "`,
	} {
		if strings.Contains(helperSource, obsolete) {
			t.Fatalf("native helper retained obsolete nested broker option %q", obsolete)
		}
	}
	for _, required := range []string{
		`default:"production" enum:"production,local-test"`,
		`r.driverValidationMode != "production" && r.driverValidationMode != "local-test"`,
	} {
		if !strings.Contains(packageCommand, required) {
			t.Fatalf("native package command omitted %q", required)
		}
	}
	if !strings.Contains(packageWindows,
		`"--validation-mode", t.request.driverValidationMode`) {
		t.Fatal("native package transaction does not pass the validated signature route to its retained helper")
	}
	if !strings.Contains(helperSource,
		`if (!SetupGetStringFieldW(&context, field, nullptr, 0, &required) ||`) {
		t.Fatal("native helper does not honor SetupGetStringFieldW's successful size-query contract")
	}
	if strings.Contains(helperSource,
		"SetupGetStringFieldW(&context, field, nullptr, 0, &required);\n"+
			"    if (required == 0 || GetLastError() != ERROR_INSUFFICIENT_BUFFER)") {
		t.Fatal("native helper still treats a successful SetupGetStringFieldW size query as failure")
	}
	if strings.Count(helperSource,
		"code != ERROR_AUTHENTICODE_TRUSTED_PUBLISHER") != 2 {
		t.Fatal("native helper does not recognize SetupAPI's exact trusted-Authenticode success classification")
	}
	if strings.Contains(helperSource,
		"ERROR_AUTHENTICODE_TRUST_NOT_ESTABLISHED") {
		t.Fatal("native helper accepts an Authenticode publisher that is not in TrustedPublisher")
	}
	if !strings.Contains(helperSource,
		"GUID action = WINTRUST_ACTION_GENERIC_VERIFY_V2;") {
		t.Fatal("native helper does not use Authenticode policy for exact catalog-member verification")
	}
	if strings.Contains(helperSource,
		"GUID action = DRIVER_ACTION_VERIFY;") {
		t.Fatal("native helper incorrectly uses the WHQL-only policy for test catalog membership")
	}
}

func TestLocalTestSettledFailureRequiresObservedExitMatch(t *testing.T) {
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
    Where-Object { $_ -match 'public static class ViiperLocalTestCertificateStore' })
if ($csharpBlocks.Count -ne 1) { throw 'Embedded certificate-store source was not found exactly once.' }
Add-Type -Language CSharp -TypeDefinition $csharpBlocks[0]
$openStore = [ViiperLocalTestCertificateStore].GetMethod(
    'CertOpenStore', [Reflection.BindingFlags]'NonPublic,Static')
$import = $openStore.GetCustomAttributes(
    [Runtime.InteropServices.DllImportAttribute], $false)[0]
if ($import.Value -cne 'crypt32.dll' -or -not $import.ExactSpelling -or
    $import.CharSet -ne [Runtime.InteropServices.CharSet]::Unicode) {
    throw 'CertOpenStore P/Invoke metadata does not name the exact native entry point.'
}

$start = $source.IndexOf('function Test-SettledLocalTestFailure')
$end = $source.IndexOf('$trustCommitted = $false', $start)
if ($start -lt 0 -or $end -le $start) { throw 'Settled-failure predicate was not found.' }
Invoke-Expression $source.Substring($start, $end - $start)
$settled = @(
    'VIIPER: error: install native driver and broker transaction: native driver helper failed with exit 1: exit status 1:',
    ('result=error operation=install changed=1 rebootRequired=0 rollback=succeeded exitCode=1 ' +
        'phase="broker-preflight" win32Error=1603 nestedExitCode=4 ' +
        'message="nested broker transaction failed after proving a settled state; nested diagnostic: ' +
        'lock package transaction token: The process cannot access the file because it is being used by another process."')
)
if ($settled[1].Length -le 120) {
    throw 'Settled proof fixture does not exceed the live host width.'
}
if (-not (Test-SettledLocalTestFailure -Lines $settled -ProcessExitCode 1)) {
    throw 'Matching long settled proof was rejected.'
}
$retainTrustOnFailure = $true
if (Test-SettledLocalTestFailure -Lines $settled -ProcessExitCode 1) {
    $retainTrustOnFailure = $false
}
if ($retainTrustOnFailure) {
    throw 'Matching long settled proof did not authorize trust removal.'
}
$cleanupCalls = 0
$trustCommitted = $false
try {
    throw 'simulated post-process transaction failure'
}
catch {
    if (-not $trustCommitted -and -not $retainTrustOnFailure) {
        $cleanupCalls++
    }
}
if ($cleanupCalls -ne 1) {
    throw 'Settled rollback did not enter the trust-cleanup branch exactly once.'
}
$retainTrustOnFailure = $true
if (Test-SettledLocalTestFailure -Lines $settled -ProcessExitCode 4) {
    $retainTrustOnFailure = $false
}
if (-not $retainTrustOnFailure) {
    throw 'Mismatched proof exit incorrectly authorized trust removal.'
}
$preflight = @(
    'result=error operation=install changed=0 rebootRequired=0 rollback=not-needed exitCode=4 phase="preflight"'
)
if (-not (Test-SettledLocalTestFailure -Lines $preflight -ProcessExitCode 4)) {
    throw 'Matching settled preflight proof was rejected.'
}
if (Test-SettledLocalTestFailure -Lines $preflight -ProcessExitCode 1) {
    throw 'Mismatched preflight proof was accepted.'
}
`
	command := exec.Command(
		powerShell, "-NoProfile", "-NonInteractive", "-Command", behaviorContract)
	command.Env = append(os.Environ(), "VIIPER_INSTALLER_CONTRACT_PATH="+installer)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("settled-failure behavior contract failed: %v\n%s", err, output)
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

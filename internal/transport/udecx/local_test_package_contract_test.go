package udecx

import (
	"os"
	"path/filepath"
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
		"Copy-ExactBrokerToProtectedStage",
		"[IO.FileShare]::Read",
		"[IO.FileOptions]::WriteThrough",
		"$lockByPath['viiper.exe']",
		"Remove-ProtectedStagingDirectory",
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
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("local-test installer omitted %q", required)
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
	} {
		if strings.Contains(installer, forbidden) {
			t.Fatalf("local-test elevated path retained unsafe dependency %q", forbidden)
		}
	}

	packageCommand := read("internal", "cmd", "native_package.go")
	packageWindows := read("internal", "cmd", "native_package_windows.go")
	helperSource := read("native", "udecx", "tools", "ViiperUdeCtl.cpp")
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

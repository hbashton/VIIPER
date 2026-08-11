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
		"Import-Certificate -FilePath $certificatePath",
		"Remove-Item -LiteralPath $storePath -Force",
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
	} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("local-test workflow uploads broad build tree %q", forbidden)
		}
	}

	composer := read("native", "udecx", "tools", "New-ViiperUdeLocalTestPackage.ps1")
	for _, required := range []string{
		"[string]$BrokerPath",
		"[string]$TestCertificatePath",
		"The local catalog and driver do not match the exact WDK-exported test certificate.",
		"Resolve-ExactInput $BrokerPath 'viiper.exe'",
		"signingRoute = 'LocalTest'",
		"releaseEligible = $false",
		"testSignerCertificateSha256",
		"installerScriptSha256",
		"-ValidationMode LocalTest",
		"-RequireLocalTestToolchainValidation",
		"local-test-package.lock.json",
		"Local test package lock SHA-256: $lockSha256",
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
		"'--expected-broker-sha256', $brokerHash",
		"'--expected-helper-sha256', $helperHash",
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
	} {
		if strings.Contains(installer, forbidden) {
			t.Fatalf("local-test elevated path retained unsafe dependency %q", forbidden)
		}
	}

	packageCommand := read("internal", "cmd", "native_package.go")
	packageWindows := read("internal", "cmd", "native_package_windows.go")
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
}

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
		"Resolve-ExactInput $BrokerPath 'viiper.exe'",
		"signingRoute = 'LocalTest'",
		"releaseEligible = $false",
		"testSignerCertificateSha256",
		"-ValidationMode LocalTest",
		"-RequireLocalTestToolchainValidation",
		"local-test-package.lock.json",
	} {
		if !strings.Contains(composer, required) {
			t.Fatalf("local-test composer omitted %q", required)
		}
	}

	installer := read("native", "udecx", "tools", "Install-ViiperUdeLocalTest.ps1")
	for _, required := range []string{
		"[string]$TargetUserSID",
		"& $brokerPath native-package-install",
		"--expected-broker-sha256 $brokerHash",
		"--expected-helper-sha256 $helperHash",
		"--target-user-sid $TargetUserSID",
		"--driver-validation-mode local-test",
		"-AcknowledgeDisposableTestMachine",
		"testsigning\\s+Yes",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("local-test installer omitted %q", required)
		}
	}
	if strings.Contains(installer, "& $helperPath install") {
		t.Fatal("local-test installation bypasses the full broker/package transaction")
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

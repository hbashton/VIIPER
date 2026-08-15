package latency

import (
	"os"
	"strings"
	"testing"
)

func TestProductionTraceAndWrapperFailClosedContract(t *testing.T) {
	profile, err := os.ReadFile("ViiperLatency.wprp")
	if err != nil {
		t.Fatal(err)
	}
	profileText := string(profile)
	for _, want := range []string{
		`LoggingMode="File"`, TraceProviderGUID[1 : len(TraceProviderGUID)-1],
		`Value="CSwitch"`, `Value="ReadyThread"`, `Value="SampledProfile"`,
		`Value="DPC"`, `Value="Interrupt"`, `Value="WDFDPC"`, `Value="WDFInterrupt"`,
	} {
		if !strings.Contains(profileText, want) {
			t.Fatalf("source-controlled WPRP is missing %q", want)
		}
	}
	if strings.Contains(profileText, `LoggingMode="Memory"`) {
		t.Fatal("production WPRP regressed to circular memory logging")
	}

	wrapper, err := os.ReadFile("../scripts/Invoke-ViiperE2ELatencyGate.ps1")
	if err != nil {
		t.Fatal(err)
	}
	wrapperText := string(wrapper)
	for _, want := range []string{
		"-filemode", "verifylatency",
		"-C $repository test", "-C $repository run",
		"$env:GOWORK = 'off'", "$env:GOENV = 'off'", "$env:GOTOOLCHAIN = 'local'",
		"-ldflags $nativeRevisionLDFlag",
		"github.com/Alia5/VIIPER/internal/transport/udecx.nativeSourceRevision=$headRevision",
		"Get-WinEvent -FilterHashtable", "ProviderName = 'VIIPER-LatencyGate'",
		"trace_marker_id", "start_qpc_ticks", "trace_marker_qpc_ticks",
		"latency-trace-markers/v1", "source_trace_length", "source_trace_sha256",
		"-trace $trace",
		"Win32_PnPEntity", "@($_.HardwareID) -contains 'ROOT\\VIIPER\\UDE'",
		"$ownedRootDevices[0].PNPDeviceID",
		"Dropped\\s+Event", "Buffers?\\s+Lost",
		"Resolve-ExactExecutablePath", "[Environment]::SystemDirectory",
		"VIIPER_E2E_EXPECTED_PRIORITY_CLASS", "git_executable_sha256",
		"Get-ExactUSBIPRuntimeProvenance", "VIIPER_E2E_USBIP_RUNTIME_PROVENANCE",
		"VIIPER_E2E_USBIP_RUNTIME_PROVENANCE_SHA256",
		"[string]$PackageValidationMode = 'Production'", "LocalTestCertificatePath",
		"VIIPER_E2E_PACKAGE_VALIDATION_MODE", "VIIPER_E2E_LOCAL_TEST_CERTIFICATE_SHA256",
		"viiper.controller-to-game.latency-suite/v3",
		"-buildvcs=false",
	} {
		if !strings.Contains(wrapperText, want) {
			t.Fatalf("production wrapper is missing fail-closed contract %q", want)
		}
	}
	if strings.Contains(wrapperText, "GeneralProfile.Verbose") {
		t.Fatal("production wrapper regressed to an inbox circular profile")
	}
	if strings.Contains(wrapperText, "DeviceID -like 'ROOT\\VIIPER\\UDE*'") {
		t.Fatal("production wrapper confuses the INF hardware ID with the generated PnP instance ID")
	}
	liveHarness, err := os.ReadFile("../latency_gate_windows_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(liveHarness), "sdl.EnableWindowsRawInput()") {
		t.Fatal("production harness no longer enables the SDL backend that supplies exact Xbox PnP paths")
	}
	for _, want := range []string{
		"P90NS", "P999NS", "collectMachineProvenance", "GetPriorityClass",
		"ProcessorNameString", "RtlGetVersion", "ProcessElevated",
		"exec.Command(executable", "runGit(config.gitPath",
	} {
		if !strings.Contains(string(liveHarness), want) {
			t.Fatalf("production harness is missing latency provenance contract %q", want)
		}
	}
	if strings.Contains(string(liveHarness), `exec.Command("git"`) {
		t.Fatal("production harness regressed to PATH-resolved Git after verifying a pinned image")
	}
	matrix, err := os.ReadFile("../scripts/Invoke-ViiperE2ELatencyMatrix.ps1")
	if err != nil {
		t.Fatal(err)
	}
	matrixText := string(matrix)
	normalizedMatrixText := strings.ReplaceAll(matrixText, "\r\n", "\n")
	for _, want := range []string{
		"foreach ($priority in @('Normal', 'High'))", "CyclesPerPriority",
		"orientation = $orientation", "cycle_index = $cycleIndex",
		"Get-ExactEvidenceFile", "latency-priority-matrix/v2",
		"process_priority_class", "Flush($true)", "verifylatencymatrix",
		"native_package_validation_mode = $matrixPackageIdentity.native_package_validation_mode",
		"native_local_test_certificate_sha256 = $matrixPackageIdentity.native_local_test_certificate_sha256",
		"native_package_manifest_sha256 = $matrixPackageIdentity.native_package_manifest_sha256",
		"every balanced cycle for this exact machine session",
	} {
		if !strings.Contains(matrixText, want) {
			t.Fatalf("priority-matrix wrapper is missing fail-closed contract %q", want)
		}
	}
	certificateBinding := strings.Index(matrixText, "$common.LocalTestCertificatePath = $LocalTestCertificatePath")
	firstLiveCycle := strings.Index(matrixText, "& $gate @common")
	if certificateBinding < 0 || firstLiveCycle < 0 || certificateBinding > firstLiveCycle {
		t.Fatal("local-test certificate is not bound before the first expensive live matrix cycle")
	}
	if !strings.Contains(normalizedMatrixText, "$repository = if ([string]::IsNullOrWhiteSpace($RepositoryRoot)) {") ||
		!strings.Contains(normalizedMatrixText, "else {\n    (Resolve-Path -LiteralPath $RepositoryRoot") {
		t.Fatal("matrix repository default/explicit selection is not a single fail-closed if/else")
	}
	verifier, err := os.ReadFile("../cmd/verifylatency/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(verifier), "latency.ParseSuiteReport") ||
		!strings.Contains(string(verifier), "latency.RequireSuitePass") {
		t.Fatal("production verifier no longer invokes strict parsing and pass enforcement")
	}
	matrixVerifier, err := os.ReadFile("../cmd/verifylatencymatrix/main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"latency.ParseSuiteReport", "latency.ParseTraceMarkerEvidence",
		"latency.VerifyTraceMarkers", "latency.AnalyzeSuperiority",
		"suite.Provenance.NativePackageValidationMode != matrix.NativePackageValidationMode",
		"suite.Provenance.NativePackageManifestSHA256 != matrix.NativePackageManifestSHA256",
		"all %d observed balanced cycles for this exact machine session",
	} {
		if !strings.Contains(string(matrixVerifier), want) {
			t.Fatalf("matrix verifier is missing strict evidence contract %q", want)
		}
	}
}

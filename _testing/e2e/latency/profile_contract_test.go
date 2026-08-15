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
		"Win32_PnPEntity", "@($_.HardwareID) -contains 'ROOT\\VIIPER\\UDE'",
		"$ownedRootDevices[0].PNPDeviceID",
		"Dropped\\s+Event", "Buffers?\\s+Lost",
		"Resolve-ExactExecutablePath", "[Environment]::SystemDirectory",
		"VIIPER_E2E_EXPECTED_PRIORITY_CLASS", "git_executable_sha256",
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
	for _, want := range []string{
		"priority = 'Normal'", "priority = 'High'", "Get-ExactEvidenceFile",
		"latency-priority-matrix/v1", "process_priority_class", "Flush($true)",
	} {
		if !strings.Contains(matrixText, want) {
			t.Fatalf("priority-matrix wrapper is missing fail-closed contract %q", want)
		}
	}
	verifier, err := os.ReadFile("../cmd/verifylatency/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(verifier), "latency.ParseSuiteReport") ||
		!strings.Contains(string(verifier), "latency.RequireSuitePass") {
		t.Fatal("production verifier no longer invokes strict parsing and pass enforcement")
	}
}

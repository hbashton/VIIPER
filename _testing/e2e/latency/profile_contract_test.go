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
		"Dropped\\s+Event", "Buffers?\\s+Lost",
	} {
		if !strings.Contains(wrapperText, want) {
			t.Fatalf("production wrapper is missing fail-closed contract %q", want)
		}
	}
	if strings.Contains(wrapperText, "GeneralProfile.Verbose") {
		t.Fatal("production wrapper regressed to an inbox circular profile")
	}
	liveHarness, err := os.ReadFile("../latency_gate_windows_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(liveHarness), "sdl.EnableWindowsRawInput()") {
		t.Fatal("production harness no longer enables the SDL backend that supplies exact Xbox PnP paths")
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

package latency

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCalculateNearestRankDistributionAndJitter(t *testing.T) {
	values := make([]int64, 100)
	for index := range values {
		values[index] = int64(100 - index)
	}
	original := append([]int64(nil), values...)

	got, err := Calculate(values)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values, original) {
		t.Fatal("Calculate reordered the caller's individual samples")
	}
	if got.Count != 100 || got.P50NS != 50 || got.P90NS != 90 ||
		got.P95NS != 95 || got.P99NS != 99 || got.P999NS != 100 ||
		got.MaxNS != 100 {
		t.Fatalf("unexpected distribution: %+v", got)
	}
	wantJitter := math.Sqrt(833.25)
	if math.Abs(got.JitterNS-wantJitter) > 1e-12 {
		t.Fatalf("jitter=%0.15f want %0.15f", got.JitterNS, wantJitter)
	}
}

func TestQPCIntervalNSFailsClosed(t *testing.T) {
	got, err := QPCIntervalNS(10, 410, 1_000_000)
	if err != nil || got != 400_000 {
		t.Fatalf("QPCIntervalNS()=(%d, %v), want (400000, nil)", got, err)
	}
	for _, test := range []struct {
		name                  string
		start, end, frequency int64
	}{
		{name: "zero start", start: 0, end: 2, frequency: 1},
		{name: "reversed", start: 2, end: 1, frequency: 1},
		{name: "zero frequency", start: 1, end: 2, frequency: 0},
		{name: "sub nanosecond", start: 1, end: 2, frequency: 2_000_000_000},
		{name: "overflow", start: 1, end: math.MaxInt64, frequency: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := QPCIntervalNS(test.start, test.end, test.frequency); err == nil {
				t.Fatal("invalid QPC interval was accepted")
			}
		})
	}
}

func TestCalculateRejectsMissingAndNonPositiveSamples(t *testing.T) {
	if _, err := Calculate(nil); err == nil {
		t.Fatal("zero samples were accepted")
	}
	if _, err := Calculate([]int64{1, 0, 2}); err == nil {
		t.Fatal("a zero latency sample was accepted")
	}
}

func TestFinalizeRequiresExactMachineAndPriorityProvenance(t *testing.T) {
	high := validReport(t)
	high.Provenance.Machine.ProcessPriorityClass = "high"
	if err := Finalize(high); err != nil {
		t.Fatalf("high-priority production run was rejected: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*MachineProvenance)
	}{
		{name: "missing host", mutate: func(machine *MachineProvenance) { machine.Hostname = "" }},
		{name: "missing OS", mutate: func(machine *MachineProvenance) { machine.OSVersion = "" }},
		{name: "missing CPU", mutate: func(machine *MachineProvenance) { machine.CPUModel = "" }},
		{name: "zero logical processors", mutate: func(machine *MachineProvenance) { machine.LogicalProcessors = 0 }},
		{name: "unsupported priority", mutate: func(machine *MachineProvenance) { machine.ProcessPriorityClass = "realtime" }},
		{name: "unelevated", mutate: func(machine *MachineProvenance) { machine.ProcessElevated = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := validReport(t)
			test.mutate(&report.Provenance.Machine)
			if err := Finalize(report); err == nil {
				t.Fatal("incomplete or unsupported machine provenance was accepted")
			}
		})
	}
}

func TestFinalizeRequiresExactToolExecutableProvenance(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Provenance)
	}{
		{name: "missing Git path", mutate: func(p *Provenance) { p.GitExecutablePath = "" }},
		{name: "invalid Git hash", mutate: func(p *Provenance) { p.GitExecutableSHA256 = "bad" }},
		{name: "missing Go path", mutate: func(p *Provenance) { p.GoExecutablePath = "" }},
		{name: "invalid Go hash", mutate: func(p *Provenance) { p.GoExecutableSHA256 = "bad" }},
		{name: "missing WPR path", mutate: func(p *Provenance) { p.WPRExecutablePath = "" }},
		{name: "invalid WPR hash", mutate: func(p *Provenance) { p.WPRExecutableSHA256 = "bad" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := validReport(t)
			test.mutate(&report.Provenance)
			if err := Finalize(report); err == nil {
				t.Fatal("incomplete tool executable provenance was accepted")
			}
		})
	}
}

func TestUSBIPAnchorUsesINFHardwareIDAndOSAssignedInstance(t *testing.T) {
	// usbip-win2's INF binds ROOT\USBIP_WIN2\UDE to usbip2_ude, while live
	// SetupAPI/pnputil evidence exposes the present OS-assigned instance as
	// ROOT\USB\####. Preserve all three identities; none substitutes for another.
	proof := ControllerProof{
		PNPInstanceID:             `HID\VID_045E&PID_028E\1`,
		PNPContainerID:            `{11111111-2222-3333-4444-555555555555}`,
		PNPAncestorIDs:            []string{`HID\VID_045E&PID_028E\1`, `USB\VID_045E&PID_028E\1`, `ROOT\USB\0002`},
		PNPAncestorContainerIDs:   []string{`{11111111-2222-3333-4444-555555555555}`, `{11111111-2222-3333-4444-555555555555}`, ""},
		PNPAncestorServices:       []string{"HidUsb", "usbccgp", "usbip2_ude"},
		PNPAncestorHardwareIDs:    [][]string{{`HID_DEVICE_SYSTEM_GAME`}, {`USB\VID_045E&PID_028E`}, {`ROOT\USBIP_WIN2\UDE`}},
		PNPAncestorLocationInfo:   []string{"", "Port_#0007.Hub_#0001", ""},
		PNPAncestorLocationPaths:  [][]string{{}, {`USBROOT(0)#USB(7)`}, {}},
		TransportAnchorInstanceID: `ROOT\USB\0002`,
		TransportAnchorService:    "usbip2_ude",
	}
	if err := ValidateTransportAncestry(TransportUSBIP, 7, proof); err != nil {
		t.Fatalf("exact USB/IP INF anchor rejected: %v", err)
	}
	if err := ValidateTransportAncestry(TransportUSBIP, 8, proof); err == nil ||
		!strings.Contains(err.Error(), "root-hub port 8") {
		t.Fatalf("wrong USB/IP import port was not rejected: %v", err)
	}
	proof.PNPAncestorHardwareIDs[2] = []string{`ROOT\USB\0002`}
	if err := ValidateTransportAncestry(TransportUSBIP, 7, proof); err == nil {
		t.Fatal("OS-assigned instance ID was accepted as a substitute for the USB/IP INF hardware ID")
	}
}

func TestProductionBlockScheduleIsCounterbalancedAndComplete(t *testing.T) {
	want := []BlockSpec{
		{Order: 1, Transport: TransportUSBIP, TransportBlock: 1, FirstSequence: 1, SamplePairs: 128},
		{Order: 2, Transport: TransportNativeUDE, TransportBlock: 1, FirstSequence: 1, SamplePairs: 128},
		{Order: 3, Transport: TransportNativeUDE, TransportBlock: 2, FirstSequence: 129, SamplePairs: 129},
		{Order: 4, Transport: TransportUSBIP, TransportBlock: 2, FirstSequence: 129, SamplePairs: 129},
	}
	if got := ProductionBlockSchedule(257); !reflect.DeepEqual(got, want) {
		t.Fatalf("schedule=%+v want %+v", got, want)
	}
	offsets := ProductionPhaseSweepOffsetsNS()
	wantOffsets := []int64{0, 125_000, 250_000, 375_000, 500_000, 625_000, 750_000, 875_000}
	if !reflect.DeepEqual(offsets, wantOffsets) {
		t.Fatalf("phase sweep=%v want %v", offsets, wantOffsets)
	}
	if got, wantHash := PhaseSweepScheduleSHA256(offsets), "21eee9ea71984343ebd21221df8272553d6ab369a5740a1c796380cd468abcd9"; got != wantHash {
		t.Fatalf("phase sweep SHA-256=%s want %s", got, wantHash)
	}
	for sequence, wantPressOffset := range []int64{0, 250_000, 500_000, 750_000} {
		if got := ProductionPhaseOffsetNS(sequence+1, TransitionPress); got != wantPressOffset {
			t.Fatalf("sequence %d press offset=%d want %d", sequence+1, got, wantPressOffset)
		}
	}
}

func TestParseReportRecomputesSamplesStatisticsAndComparison(t *testing.T) {
	report := validReport(t)
	encoded := encodeReport(t, report)
	parsed, err := ParseReport(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if err := RequirePass(parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Transports[0].Statistics.Press.Count != MinimumProductionSamplePairs ||
		parsed.Transports[1].Statistics.Release.Count != MinimumProductionSamplePairs {
		t.Fatalf("individual press/release samples were not retained: %+v", parsed.Transports)
	}
	if parsed.Comparison.Combined.P99.NativeToUSBIPRatio == nil {
		t.Fatal("comparison ratio was not derived")
	}
}

func TestParseReportRejectsUnknownTrailingAndForgedData(t *testing.T) {
	report := validReport(t)

	t.Run("unknown field", func(t *testing.T) {
		var object map[string]any
		if err := json.Unmarshal(encodeReport(t, report), &object); err != nil {
			t.Fatal(err)
		}
		object["not_in_schema"] = true
		data, _ := json.Marshal(object)
		if _, err := ParseReport(bytes.NewReader(data)); err == nil ||
			!strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("unknown field error=%v", err)
		}
	})

	t.Run("trailing JSON", func(t *testing.T) {
		data := append(encodeReport(t, report), []byte(` {"extra":true}`)...)
		if _, err := ParseReport(bytes.NewReader(data)); err == nil ||
			!strings.Contains(err.Error(), "trailing JSON") {
			t.Fatalf("trailing data error=%v", err)
		}
	})

	t.Run("forged statistic", func(t *testing.T) {
		forged := validReport(t)
		forged.Runs[1].Statistics.Combined.P99NS++
		if _, err := ParseReport(bytes.NewReader(encodeReport(t, forged))); err == nil ||
			!strings.Contains(err.Error(), "statistics do not match") {
			t.Fatalf("forged statistic error=%v", err)
		}
	})

	t.Run("forged transport aggregate", func(t *testing.T) {
		forged := validReport(t)
		forged.Transports[1].Statistics.Press.P95NS++
		if _, err := ParseReport(bytes.NewReader(encodeReport(t, forged))); err == nil ||
			!strings.Contains(err.Error(), "aggregates") {
			t.Fatalf("forged aggregate error=%v", err)
		}
	})

	t.Run("mixed source", func(t *testing.T) {
		mixed := validReport(t)
		mixed.Runs[1].Server.Transport = TransportUSBIP
		if err := Finalize(mixed); err == nil || !strings.Contains(err.Error(), "requested live transport") {
			t.Fatalf("mixed source error=%v", err)
		}
	})

	t.Run("unauthenticated workload", func(t *testing.T) {
		unauthenticated := validReport(t)
		unauthenticated.Runs[0].UnauthenticatedRejected = false
		if err := Finalize(unauthenticated); err == nil || !strings.Contains(err.Error(), "unauthenticated") {
			t.Fatalf("unauthenticated source error=%v", err)
		}
	})

	t.Run("noncanonical source revision length", func(t *testing.T) {
		report := validReport(t)
		report.Provenance.SourceRevision = strings.Repeat("a", 41)
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "40- or 64") {
			t.Fatalf("noncanonical revision error=%v", err)
		}
	})
}

func TestParseSuiteRequiresPlayStationCasesAndWorkloadParity(t *testing.T) {
	suite := validSuite(t)
	encoded := encodeSuite(t, suite)
	parsed, err := ParseSuiteReport(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireSuitePass(parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Cases) != 3 || parsed.Cases[2].Workload.ControllerType != "dualsensegamepadv5" {
		t.Fatalf("suite does not contain required production controller cases: %+v", parsed.Cases)
	}

	t.Run("Xbox cannot substitute for DualSense", func(t *testing.T) {
		missing := validSuite(t)
		missing.Cases[2] = cloneReport(t, missing.Cases[0])
		if err := FinalizeSuite(missing); err == nil || !strings.Contains(err.Error(), "dualsensegamepadv5") {
			t.Fatalf("missing DualSense error=%v", err)
		}
	})

	t.Run("controller workload drift", func(t *testing.T) {
		drift := validSuite(t)
		drift.Cases[1].Workload.InterTransitionDelayNS++
		if err := FinalizeSuite(drift); err == nil || !strings.Contains(err.Error(), "identical authenticated") {
			t.Fatalf("workload drift error=%v", err)
		}
	})

	t.Run("DualSense cannot bind as Xbox", func(t *testing.T) {
		mismatch := validSuite(t)
		mismatch.Cases[2].Runs[1].Controller.SDLRealType = 2
		if err := FinalizeSuite(mismatch); err == nil || !strings.Contains(err.Error(), "SDL gamepad identity") {
			t.Fatalf("DualSense SDL substitution error=%v", err)
		}
	})

	t.Run("forged controller statistic", func(t *testing.T) {
		forged := validSuite(t)
		forged.Cases[2].Runs[1].Statistics.Press.P95NS++
		if _, err := ParseSuiteReport(bytes.NewReader(encodeSuite(t, forged))); err == nil ||
			!strings.Contains(err.Error(), "statistics do not match") {
			t.Fatalf("forged suite statistic error=%v", err)
		}
	})
}

func TestFinalizeRejectsTimeoutInsufficientSamplesAndDuplicates(t *testing.T) {
	report := validReport(t)
	native := &report.Runs[2]
	native.Samples = native.Samples[:len(native.Samples)-1]
	native.Misses.Release = 1
	native.Duplicates.Press = 2
	native.Failure = "release sample 256 timed out after 1s"

	if err := Finalize(report); err != nil {
		t.Fatal(err)
	}
	if report.Verdict != "fail" {
		t.Fatalf("verdict=%q want fail", report.Verdict)
	}
	joined := strings.Join(report.Failures, "\n")
	for _, want := range []string{
		"timed out", "1 missed transitions", "2 duplicate transitions", "255/256 required release samples",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("failures did not contain %q:\n%s", want, joined)
		}
	}
	if err := RequirePass(report); err == nil {
		t.Fatal("failed report was accepted by RequirePass")
	}

	encoded := encodeReport(t, report)
	parsed, err := ParseReport(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("a self-consistent failure artifact must remain parseable: %v", err)
	}
	if parsed.Verdict != "fail" {
		t.Fatalf("parsed verdict=%q", parsed.Verdict)
	}
}

func TestFinalizeRejectsWeakenedPolicyAndOutOfOrderSamples(t *testing.T) {
	t.Run("weakened policy", func(t *testing.T) {
		report := validReport(t)
		report.Policy.NativeMaxP95NS = DefaultNativeMaxP95NS + 1
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "weaker") {
			t.Fatalf("weakened policy error=%v", err)
		}
	})

	t.Run("weakened same-machine policy", func(t *testing.T) {
		report := validReport(t)
		report.Policy.NativeMaxP95OverUSBIPNS = DefaultNativeMaxP95OverUSBIPNS + 1
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "weaker") {
			t.Fatalf("weakened comparison policy error=%v", err)
		}
	})

	t.Run("non-ABBA block order", func(t *testing.T) {
		report := validReport(t)
		report.Runs[2].Transport = TransportUSBIP
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "ABBA") {
			t.Fatalf("block order error=%v", err)
		}
	})

	t.Run("phase sweep drift", func(t *testing.T) {
		report := validReport(t)
		report.Workload.PhaseSweepOffsetsNS[1]++
		report.Workload.PhaseSweepSHA256 = PhaseSweepScheduleSHA256(report.Workload.PhaseSweepOffsetsNS)
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "phase-sweep") {
			t.Fatalf("phase sweep drift error=%v", err)
		}
	})

	t.Run("duplicate sequence", func(t *testing.T) {
		report := validReport(t)
		report.Runs[0].Samples[1].Transition = TransitionPress
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "want 1/release") {
			t.Fatalf("duplicate sequence error=%v", err)
		}
	})

	t.Run("event clock regression", func(t *testing.T) {
		report := validReport(t)
		report.Runs[0].Samples[1].EventTimestampNS = report.Runs[0].Samples[0].EventTimestampNS - 1
		report.Runs[0].Samples[1].SDLFenceTimestampNS = report.Runs[0].Samples[1].EventTimestampNS - 1
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "event clock") {
			t.Fatalf("event clock error=%v", err)
		}
	})

	t.Run("pre-write SDL edge", func(t *testing.T) {
		report := validReport(t)
		report.Runs[0].Samples[0].SDLFenceTimestampNS =
			report.Runs[0].Samples[0].EventTimestampNS + 1
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "causal fence") {
			t.Fatalf("pre-write SDL edge error=%v", err)
		}
	})

	t.Run("forged trace marker", func(t *testing.T) {
		report := validReport(t)
		report.Runs[0].Samples[0].MarkerID = "another-sample"
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "trace marker") {
			t.Fatalf("forged trace marker error=%v", err)
		}
	})

	t.Run("trace marker inside measured interval", func(t *testing.T) {
		report := validReport(t)
		report.Runs[0].Samples[0].MarkerQPCTicks =
			report.Runs[0].Samples[0].EndQPCTicks - 1
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "QPC interval") {
			t.Fatalf("in-interval marker error=%v", err)
		}
	})

	t.Run("latency disagrees with raw QPC", func(t *testing.T) {
		report := validReport(t)
		report.Runs[0].Samples[0].LatencyNS++
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "inconsistent latency") {
			t.Fatalf("forged QPC latency error=%v", err)
		}
	})

	t.Run("wrong native ancestry", func(t *testing.T) {
		report := validReport(t)
		run := &report.Runs[1]
		run.Controller.PNPAncestorIDs[len(run.Controller.PNPAncestorIDs)-1] = `ROOT\USB\0002`
		run.Controller.PNPAncestorServices[len(run.Controller.PNPAncestorServices)-1] = "usbip2_ude"
		run.Controller.PNPAncestorHardwareIDs[len(run.Controller.PNPAncestorHardwareIDs)-1] = []string{`ROOT\USBIP_WIN2\UDE`}
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "anchor") {
			t.Fatalf("wrong native ancestry error=%v", err)
		}
	})

	t.Run("missing controller container", func(t *testing.T) {
		report := validReport(t)
		report.Runs[1].Controller.PNPContainerID = ""
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "ancestry proof") {
			t.Fatalf("missing controller container error=%v", err)
		}
	})

	t.Run("mismatched controller container", func(t *testing.T) {
		report := validReport(t)
		report.Runs[1].Controller.PNPAncestorContainerIDs[0] =
			`{99999999-8888-7777-6666-555555555555}`
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "ancestry proof") {
			t.Fatalf("mismatched controller container error=%v", err)
		}
	})

	t.Run("wrong loaded native build", func(t *testing.T) {
		report := validReport(t)
		report.Runs[1].Server.NativeUDE.LoadedDriverBuildIdentity = strings.Repeat("2", 64)
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "signed package manifest") {
			t.Fatalf("wrong loaded driver identity error=%v", err)
		}
	})

	t.Run("ambiguous USBIP ancestry", func(t *testing.T) {
		report := validReport(t)
		run := &report.Runs[0]
		run.Controller.PNPAncestorIDs = append(run.Controller.PNPAncestorIDs, `ROOT\USB\0003`)
		run.Controller.PNPAncestorContainerIDs = append(run.Controller.PNPAncestorContainerIDs, "")
		run.Controller.PNPAncestorServices = append(run.Controller.PNPAncestorServices, "usbip2_ude")
		run.Controller.PNPAncestorHardwareIDs = append(run.Controller.PNPAncestorHardwareIDs, []string{`ROOT\USBIP_WIN2\UDE`})
		run.Controller.PNPAncestorLocationInfo = append(run.Controller.PNPAncestorLocationInfo, "")
		run.Controller.PNPAncestorLocationPaths = append(run.Controller.PNPAncestorLocationPaths, []string{})
		if err := Finalize(report); err == nil || !strings.Contains(err.Error(), "anchor") {
			t.Fatalf("ambiguous USB/IP ancestry error=%v", err)
		}
	})
}

func TestFinalizeRejectsSameMachineNativeTailRegression(t *testing.T) {
	tests := []struct {
		name   string
		metric string
		mutate func(*Report)
	}{
		{
			name: "p95", metric: "p95",
			mutate: func(report *Report) {
				for runIndex := range report.Runs {
					run := &report.Runs[runIndex]
					if run.Transport != TransportNativeUDE {
						continue
					}
					for sampleIndex := range run.Samples {
						setSampleLatency(&run.Samples[sampleIndex], 1_500_000, report.Provenance.QPCFrequency)
					}
				}
			},
		},
		{
			name: "p99", metric: "p99",
			mutate: func(report *Report) {
				nativeSecond := &report.Runs[2]
				for index := len(nativeSecond.Samples) - 6; index < len(nativeSecond.Samples); index++ {
					setSampleLatency(&nativeSecond.Samples[index], 2_600_000, report.Provenance.QPCFrequency)
				}
			},
		},
		{
			name: "max", metric: "max",
			mutate: func(report *Report) {
				nativeSecond := &report.Runs[2]
				setSampleLatency(&nativeSecond.Samples[len(nativeSecond.Samples)-1], 5_600_000,
					report.Provenance.QPCFrequency)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := validReport(t)
			test.mutate(report)
			if err := Finalize(report); err != nil {
				t.Fatal(err)
			}
			failures := strings.Join(report.Failures, "\n")
			if report.Verdict != "fail" ||
				!strings.Contains(failures, "same-machine USB/IP") ||
				!strings.Contains(failures, test.metric) {
				t.Fatalf("same-machine %s regression was not rejected: verdict=%q failures=%v",
					test.metric, report.Verdict, report.Failures)
			}
		})
	}
}

func validReport(t *testing.T) *Report {
	t.Helper()
	report := &Report{
		Schema:      SchemaV2,
		GeneratedAt: time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
		Provenance: Provenance{
			SourceRevision:              strings.Repeat("a", 40),
			SDLSourceRevision:           strings.Repeat("b", 40),
			SDLBinaryPath:               `C:\source\SDL3.dll`,
			SDLBinarySHA256:             strings.Repeat("c", 64),
			NativePackageManifestSHA256: strings.Repeat("d", 64),
			NativeDriverSHA256:          strings.Repeat("e", 64),
			NativeDriverBuildIdentity:   strings.Repeat("1", 64),
			QPCFrequency:                1_000_000_000,
			TraceProviderName:           TraceProviderName,
			TraceProviderGUID:           TraceProviderGUID,
			TraceProfileSHA256:          strings.Repeat("f", 64),
			USBIPBaselineMode:           USBIPBaselineMode,
			USBIPBaselineVersion:        USBIPBaselineVersion,
			GoVersion:                   "go1.26.2",
			GOOS:                        "windows",
			GOARCH:                      "amd64",
			GitExecutablePath:           `C:\Program Files\Git\cmd\git.exe`,
			GitExecutableSHA256:         strings.Repeat("2", 64),
			GoExecutablePath:            `C:\Go\bin\go.exe`,
			GoExecutableSHA256:          strings.Repeat("3", 64),
			WPRExecutablePath:           `C:\Windows\System32\wpr.exe`,
			WPRExecutableSHA256:         strings.Repeat("4", 64),
			Machine: MachineProvenance{
				Hostname: "bench-host", OSProductName: "Windows 11 Pro",
				OSDisplayVersion: "24H2", OSVersion: "10.0.26100.9999",
				CPUModel: "Test CPU", LogicalProcessors: 16,
				ProcessPriorityClass: "normal", ProcessElevated: true,
			},
		},
		Workload: Workload{
			APIAddress:             "127.0.0.1:33245",
			USBIPAddress:           "127.0.0.1:33244",
			ControllerType:         "xbox360",
			ExpectedVendorID:       0x045e,
			ExpectedProductID:      0x028e,
			ExpectedSDLType:        "xbox360",
			Button:                 "south/A",
			WarmupPairs:            ProductionWarmupPairs,
			SamplePairs:            MinimumProductionSamplePairs,
			PerTransitionTimeoutNS: int64(time.Second),
			InterTransitionDelayNS: int64(2 * time.Millisecond),
			PhaseSweepOffsetsNS:    ProductionPhaseSweepOffsetsNS(),
			Authentication:         AuthenticationMode,
		},
		Policy: Policy{
			MinimumSamplePairs:      MinimumProductionSamplePairs,
			NativeMaxP95NS:          DefaultNativeMaxP95NS,
			NativeMaxP99NS:          DefaultNativeMaxP99NS,
			NativeMaxNS:             DefaultNativeMaxNS,
			NativeMaxP95OverUSBIPNS: DefaultNativeMaxP95OverUSBIPNS,
			NativeMaxP99OverUSBIPNS: DefaultNativeMaxP99OverUSBIPNS,
			NativeMaxOverUSBIPNS:    DefaultNativeMaxOverUSBIPNS,
		},
	}
	report.Workload.PhaseSweepSHA256 = PhaseSweepScheduleSHA256(report.Workload.PhaseSweepOffsetsNS)

	for runIndex, block := range ProductionBlockSchedule(MinimumProductionSamplePairs) {
		run := Run{
			Order:                   block.Order,
			TransportBlock:          block.TransportBlock,
			FirstSequence:           block.FirstSequence,
			SamplePairs:             block.SamplePairs,
			Transport:               block.Transport,
			Authentication:          AuthenticationMode,
			UnauthenticatedRejected: true,
			Server: ServerProof{
				Server: "VIIPER", Version: "0.1.0", Transport: block.Transport, Ready: true,
			},
			Device: DeviceProof{
				BusID: 1, DeviceID: "1", Type: "xbox360",
				VendorID: 0x045e, ProductID: 0x028e,
			},
			Controller: ControllerProof{
				BaselineGamepadIDs: []int32{10},
				NewGamepadIDs:      []int32{int32(20 + runIndex)},
				SDLInstanceID:      int32(20 + runIndex),
				SDLPath:            fmt.Sprintf("source-path-%s-%d", block.Transport, block.TransportBlock),
				SDLGUID:            "030000005e0400008e02000000000000",
				SDLName:            "Xbox 360 Controller",
				SDLType:            "xbox360",
				SDLReportedType:    1,
				SDLRealType:        2,
				VendorID:           0x045e,
				ProductID:          0x028e,
			},
		}
		if block.Transport == TransportUSBIP {
			run.Device.USBIPPort = 1
			run.Controller.PNPInstanceID = `HID\VID_045E&PID_028E\1`
			run.Controller.PNPContainerID = `{11111111-2222-3333-4444-555555555555}`
			run.Controller.PNPAncestorIDs = []string{run.Controller.PNPInstanceID, `USB\VID_045E&PID_028E\1`, `ROOT\USB\0002`}
			run.Controller.PNPAncestorContainerIDs = []string{run.Controller.PNPContainerID, run.Controller.PNPContainerID, ""}
			run.Controller.PNPAncestorServices = []string{"HidUsb", "usbccgp", "usbip2_ude"}
			run.Controller.PNPAncestorHardwareIDs = [][]string{{`HID_DEVICE_SYSTEM_GAME`}, {`USB\VID_045E&PID_028E`}, {`ROOT\USBIP_WIN2\UDE`}}
			run.Controller.PNPAncestorLocationInfo = []string{"", "Port_#0001.Hub_#0001", ""}
			run.Controller.PNPAncestorLocationPaths = [][]string{{}, {`USBROOT(0)#USB(1)`}, {}}
			run.Controller.TransportAnchorInstanceID = `ROOT\USB\0002`
			run.Controller.TransportAnchorService = "usbip2_ude"
		} else {
			run.Server.NativeUDE = &NativeServerProof{
				ABIMajor: 1, ABIMinor: 0, Capabilities: 1,
				ExpectedDriverPackageVersion: "0.1.0.3",
				LoadedDriverBuildIdentity:    report.Provenance.NativeDriverBuildIdentity,
			}
			run.Controller.PNPInstanceID = `HID\VID_045E&PID_028E\2`
			run.Controller.PNPContainerID = `{AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE}`
			run.Controller.PNPAncestorIDs = []string{run.Controller.PNPInstanceID, `USB\VID_045E&PID_028E\2`, `ROOT\VIIPERUDE\0000`}
			run.Controller.PNPAncestorContainerIDs = []string{run.Controller.PNPContainerID, run.Controller.PNPContainerID, ""}
			run.Controller.PNPAncestorServices = []string{"HidUsb", "WUDFRd", "ViiperUde"}
			run.Controller.PNPAncestorHardwareIDs = [][]string{{`HID_DEVICE_SYSTEM_GAME`}, {`USB\VID_045E&PID_028E`}, {`ROOT\VIIPER\UDE`}}
			run.Controller.PNPAncestorLocationInfo = []string{"", "", ""}
			run.Controller.PNPAncestorLocationPaths = [][]string{{}, {}, {}}
			run.Controller.TransportAnchorInstanceID = `ROOT\VIIPERUDE\0000`
			run.Controller.TransportAnchorService = "ViiperUde"
		}
		transportOffset := 0
		if block.Transport == TransportNativeUDE {
			transportOffset = 50_000
		}
		lastSequence := block.FirstSequence + block.SamplePairs - 1
		qpcCursor := int64(runIndex+1) * 1_000_000_000_000
		for sequence := block.FirstSequence; sequence <= lastSequence; sequence++ {
			base := int64(400_000 + transportOffset + sequence*10)
			timestamp := uint64(runIndex+1)*1_000_000_000 + uint64(sequence*2)
			pressStartQPC := qpcCursor
			pressEndQPC := pressStartQPC + base
			releaseStartQPC := pressStartQPC + 10_000_000
			releaseEndQPC := releaseStartQPC + base + 5
			run.Samples = append(run.Samples,
				Sample{Sequence: sequence, Transition: TransitionPress,
					LatencyNS: base, EventTimestampNS: timestamp, SDLFenceTimestampNS: timestamp - 1,
					StartQPCTicks: pressStartQPC, EndQPCTicks: pressEndQPC,
					MarkerQPCTicks: pressEndQPC + 1,
					MarkerID:       SampleMarkerID("xbox360", run.Transport, run.TransportBlock, sequence, TransitionPress)},
				Sample{Sequence: sequence, Transition: TransitionRelease,
					LatencyNS: base + 5, EventTimestampNS: timestamp + 1, SDLFenceTimestampNS: timestamp,
					StartQPCTicks: releaseStartQPC, EndQPCTicks: releaseEndQPC,
					MarkerQPCTicks: releaseEndQPC + 1,
					MarkerID:       SampleMarkerID("xbox360", run.Transport, run.TransportBlock, sequence, TransitionRelease)})
			qpcCursor = pressStartQPC + 20_000_000
		}
		report.Runs = append(report.Runs, run)
	}
	if err := Finalize(report); err != nil {
		t.Fatal(err)
	}
	if report.Verdict != "pass" {
		t.Fatalf("fixture failed: %v", report.Failures)
	}
	return report
}

func setSampleLatency(sample *Sample, latencyNS, qpcFrequency int64) {
	sample.LatencyNS = latencyNS
	sample.EndQPCTicks = sample.StartQPCTicks + latencyNS*qpcFrequency/int64(time.Second)
	sample.MarkerQPCTicks = sample.EndQPCTicks + 1
}

func validSuite(t *testing.T) *SuiteReport {
	t.Helper()
	xbox := validReport(t)
	suite := &SuiteReport{
		Schema: SuiteSchemaV2, GeneratedAt: xbox.GeneratedAt, Provenance: xbox.Provenance,
	}
	identities := []struct {
		controller string
		vendorID   uint16
		productID  uint16
		sdlType    string
		realType   int32
	}{
		{controller: "xbox360", vendorID: 0x045e, productID: 0x028e, sdlType: "xbox360", realType: 2},
		{controller: "dualshock4", vendorID: 0x054c, productID: 0x09cc, sdlType: "ps4", realType: 5},
		{controller: "dualsensegamepadv5", vendorID: 0x054c, productID: 0x0ce6, sdlType: "ps5", realType: 6},
	}
	for caseIndex, identity := range identities {
		report := cloneReport(t, *xbox)
		report.Workload.ControllerType = identity.controller
		report.Workload.ExpectedVendorID = identity.vendorID
		report.Workload.ExpectedProductID = identity.productID
		report.Workload.ExpectedSDLType = identity.sdlType
		for runIndex := range report.Runs {
			run := &report.Runs[runIndex]
			run.Device.Type = identity.controller
			run.Device.VendorID = identity.vendorID
			run.Device.ProductID = identity.productID
			run.Controller.SDLType = identity.sdlType
			run.Controller.SDLRealType = identity.realType
			run.Controller.VendorID = identity.vendorID
			run.Controller.ProductID = identity.productID
			run.Controller.SDLInstanceID = int32(100 + caseIndex*10 + runIndex)
			run.Controller.NewGamepadIDs = []int32{run.Controller.SDLInstanceID}
			run.Controller.SDLPath = identity.controller + "-" + run.Transport
			for sampleIndex := range run.Samples {
				sample := &run.Samples[sampleIndex]
				sample.MarkerID = SampleMarkerID(identity.controller, run.Transport,
					run.TransportBlock, sample.Sequence, sample.Transition)
			}
		}
		if err := Finalize(&report); err != nil {
			t.Fatal(err)
		}
		suite.Cases = append(suite.Cases, report)
	}
	if err := FinalizeSuite(suite); err != nil {
		t.Fatal(err)
	}
	if suite.Verdict != "pass" {
		t.Fatalf("suite fixture failed: %v", suite.Failures)
	}
	return suite
}

func cloneReport(t *testing.T, report Report) Report {
	t.Helper()
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var clone Report
	if err = json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func encodeSuite(t *testing.T, suite *SuiteReport) []byte {
	t.Helper()
	data, err := json.Marshal(suite)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func encodeReport(t *testing.T, report *Report) []byte {
	t.Helper()
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

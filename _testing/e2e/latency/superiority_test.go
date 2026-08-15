package latency

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAnalyzeSuperiorityRequiresEveryBalancedCycleStratumToBeFaster(t *testing.T) {
	cycles := superiorityFixture(t, true)
	report, err := AnalyzeSuperiority(cycles,
		time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err = RequireSuperiority(report); err != nil {
		t.Fatal(err)
	}
	want := cycles[0].Suite.Provenance
	if report.NativePackageManifestSHA256 != want.NativePackageManifestSHA256 ||
		report.NativePackageValidationMode != want.NativePackageValidationMode ||
		report.NativeLocalTestCertificateSHA256 != want.NativeLocalTestCertificateSHA256 ||
		report.NativeDriverSHA256 != want.NativeDriverSHA256 ||
		report.NativeDriverBuildIdentity != want.NativeDriverBuildIdentity {
		t.Fatalf("superiority package identity is not bound: %+v", report)
	}
	if got, want := len(report.Metrics), 2*3*2*3; got != want {
		t.Fatalf("metrics=%d want %d", got, want)
	}
	for _, metric := range report.Metrics {
		if metric.Verdict != "pass" || metric.WorstCycleNativeMinusUSBIPNS >= 0 ||
			!metric.AllObservedCyclesFaster || len(metric.Cycles) != 8 {
			t.Fatalf("non-superior metric: %+v", metric)
		}
	}
	if report.InferenceScope != SuperiorityInferenceScope ||
		strings.Contains(strings.ToLower(report.Method+report.InferenceScope), "95%") ||
		strings.Contains(strings.ToLower(report.Method+report.InferenceScope), "confidence bound") {
		t.Fatalf("unexpected inferential claim: method=%q scope=%q", report.Method, report.InferenceScope)
	}
}

func TestRequireSuperiorityRejectsPackageIdentityMutation(t *testing.T) {
	report, err := AnalyzeSuperiority(superiorityFixture(t, true),
		time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	report.NativePackageValidationMode = PackageValidationLocalTest
	report.NativeLocalTestCertificateSHA256 = ""
	if err = RequireSuperiority(report); err == nil {
		t.Fatal("mutated package validation identity passed")
	}
}

func TestCycleSuperiorityMeanUsesExactIntegerSumForVerdict(t *testing.T) {
	const samplePairs = 256
	report := &Report{Workload: Workload{SamplePairs: samplePairs}}
	usbip := Run{Transport: TransportUSBIP}
	native := Run{Transport: TransportNativeUDE}
	for sequence := 1; sequence <= samplePairs; sequence++ {
		difference := int64(-100)
		if sequence-1 == 212 || sequence-1 == 248 {
			difference = 12_700
		}
		usbip.Samples = append(usbip.Samples, Sample{
			Sequence: sequence, Transition: TransitionPress, LatencyNS: 500_000_000,
		})
		native.Samples = append(native.Samples, Sample{
			Sequence: sequence, Transition: TransitionPress, LatencyNS: 500_000_000 + difference,
		})
	}
	report.Runs = []Run{usbip, native}
	metrics, err := cycleSuperiorityMetrics(report, TransitionPress)
	if err != nil {
		t.Fatal(err)
	}
	if metrics["mean"] != 0 {
		t.Fatalf("exact mean tie drifted to %v", metrics["mean"])
	}
}

func TestAnalyzeSuperiorityRejectsPermissivePerRunPassWhenNativeIsSlower(t *testing.T) {
	cycles := superiorityFixture(t, false)
	report, err := AnalyzeSuperiority(cycles,
		time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != "fail" || len(report.Failures) == 0 {
		t.Fatalf("slower native fixture passed: %+v", report)
	}
	if err = RequireSuperiority(report); err == nil ||
		!strings.Contains(err.Error(), "every observed balanced cycle") {
		t.Fatalf("slower native RequireSuperiority error=%v", err)
	}
}

func TestAnalyzeSuperiorityRejectsCycleRelabelAndProvenanceDrift(t *testing.T) {
	t.Run("orientation", func(t *testing.T) {
		cycles := superiorityFixture(t, true)
		for caseIndex := range cycles[0].Suite.Cases {
			cycles[0].Suite.Cases[caseIndex].Workload.ScheduleOrientation = ScheduleOrientationBAAB
		}
		if _, err := AnalyzeSuperiority(cycles, time.Now().UTC()); err == nil ||
			!strings.Contains(err.Error(), "orientation") {
			t.Fatalf("orientation relabel error=%v", err)
		}
	})

	t.Run("toolchain provenance", func(t *testing.T) {
		cycles := superiorityFixture(t, true)
		cycles[7].Suite.Provenance.GoExecutableSHA256 = strings.Repeat("9", 64)
		for caseIndex := range cycles[7].Suite.Cases {
			cycles[7].Suite.Cases[caseIndex].Provenance.GoExecutableSHA256 = strings.Repeat("9", 64)
		}
		if _, err := AnalyzeSuperiority(cycles, time.Now().UTC()); err == nil ||
			!strings.Contains(err.Error(), "provenance drifted") {
			t.Fatalf("provenance drift error=%v", err)
		}
	})

	t.Run("overlapping qpc evidence", func(t *testing.T) {
		cycles := superiorityFixture(t, true)
		for caseIndex := range cycles[1].Suite.Cases {
			for runIndex := range cycles[1].Suite.Cases[caseIndex].Runs {
				for sampleIndex := range cycles[1].Suite.Cases[caseIndex].Runs[runIndex].Samples {
					left := &cycles[0].Suite.Cases[caseIndex].Runs[runIndex].Samples[sampleIndex]
					right := &cycles[1].Suite.Cases[caseIndex].Runs[runIndex].Samples[sampleIndex]
					right.StartQPCTicks = left.StartQPCTicks
					right.EndQPCTicks = left.EndQPCTicks
					right.MarkerQPCTicks = left.MarkerQPCTicks
				}
			}
		}
		if _, err := AnalyzeSuperiority(cycles, time.Now().UTC()); err == nil ||
			!strings.Contains(err.Error(), "overlaps prior") {
			t.Fatalf("overlapping QPC evidence error=%v", err)
		}
	})
}

func superiorityFixture(t *testing.T, nativeFaster bool) []SuperiorityCycle {
	t.Helper()
	base := validSuite(t)
	cycles := make([]SuperiorityCycle, 0, 16)
	for cycleIndex := 1; cycleIndex <= 16; cycleIndex++ {
		suite := cloneSuite(t, base)
		priority := "normal"
		if cycleIndex > 8 {
			priority = "high"
		}
		generatedAt := base.GeneratedAt.Add(time.Duration(cycleIndex) * time.Minute)
		suite.GeneratedAt = generatedAt
		suite.Provenance.Machine.ProcessPriorityClass = priority
		orientation := ScheduleOrientationForCycle(cycleIndex)
		for caseIndex := range suite.Cases {
			report := &suite.Cases[caseIndex]
			report.GeneratedAt = generatedAt
			report.Provenance.Machine.ProcessPriorityClass = priority
			report.Workload.ScheduleOrientation = orientation
			report.Workload.CycleID = strings.Repeat("7", 32)
			report.Workload.CycleIndex = cycleIndex
			report.Workload.CycleCount = 16
			if orientation == ScheduleOrientationBAAB {
				report.Runs = []Run{report.Runs[1], report.Runs[0], report.Runs[3], report.Runs[2]}
				for runIndex := range report.Runs {
					report.Runs[runIndex].Order = runIndex + 1
				}
			}
			if nativeFaster {
				for runIndex := range report.Runs {
					run := &report.Runs[runIndex]
					if run.Transport != TransportNativeUDE {
						continue
					}
					for sampleIndex := range run.Samples {
						setSampleLatency(&run.Samples[sampleIndex],
							run.Samples[sampleIndex].LatencyNS-200_000,
							report.Provenance.QPCFrequency)
					}
				}
			}
			for runIndex := range report.Runs {
				run := &report.Runs[runIndex]
				for sampleIndex := range run.Samples {
					sample := &run.Samples[sampleIndex]
					sample.MarkerID = SampleMarkerID(report.Workload.CycleID,
						report.Workload.CycleIndex, report.Workload.ControllerType,
						run.Transport, run.TransportBlock, sample.Sequence, sample.Transition)
					sample.EventTimestampNS = uint64(runIndex+1)*1_000_000_000 +
						uint64(sampleIndex+1)
					sample.SDLFenceTimestampNS = sample.EventTimestampNS - 1
					sample.StartQPCTicks = int64(cycleIndex)*10_000_000_000_000 +
						int64(runIndex+1)*1_000_000_000_000 +
						int64(sampleIndex+1)*20_000_000
					setSampleLatency(sample, sample.LatencyNS,
						report.Provenance.QPCFrequency)
				}
			}
		}
		if err := FinalizeSuite(suite); err != nil {
			t.Fatalf("cycle %d: %v", cycleIndex, err)
		}
		cycles = append(cycles, SuperiorityCycle{Priority: priority, Suite: suite})
	}
	return cycles
}

func cloneSuite(t *testing.T, suite *SuiteReport) *SuiteReport {
	t.Helper()
	data, err := json.Marshal(suite)
	if err != nil {
		t.Fatal(err)
	}
	var clone SuiteReport
	if err = json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return &clone
}

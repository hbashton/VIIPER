package latency

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeProbe struct {
	descriptor                TransportProvenance
	healthyNS                 int64
	recoveryNS                int64
	describeCalls             int
	captureCalls              int64
	requests                  []CaptureRequest
	driftAfterRun             bool
	fail                      func(CaptureRequest) bool
	beforeReturn              func()
	mutateProvenanceOnCapture bool
	generationByBlock         map[int]uint64
}

func (probe *fakeProbe) Describe(context.Context) (TransportProvenance, error) {
	probe.describeCalls++
	result := probe.descriptor
	if probe.driftAfterRun && probe.describeCalls > 1 {
		result.BuildIdentity += "-changed"
	}
	return result, nil
}

func (probe *fakeProbe) Capture(_ context.Context, request CaptureRequest) (CaptureEvidence, error) {
	probe.requests = append(probe.requests, request)
	probe.captureCalls++
	if probe.fail != nil && probe.fail(request) {
		return CaptureEvidence{}, errors.New("scripted capture failure")
	}
	if probe.mutateProvenanceOnCapture {
		probe.descriptor.Artifacts[0].SHA256 = strings.Repeat("d", 64)
		probe.descriptor.Configuration[0].Value = "mutated"
	}
	duration := probe.healthyNS
	if request.Path == PathRecovery {
		duration = probe.recoveryNS
	}
	ordinal := int64(request.BlockOrder)*10_000 + probe.captureCalls
	evidence := evidenceFor(request.Boundary, request.Path, request.Transition,
		probe.descriptor.Clock, duration, ordinal, request.RecoveryScenario)
	if probe.generationByBlock == nil {
		probe.generationByBlock = make(map[int]uint64)
	}
	generation := probe.generationByBlock[request.TransportBlock]
	if generation == 0 {
		generation = 1
	}
	evidence.Lifecycle.GenerationBefore = generation
	if request.Path == PathRecovery {
		generation++
	}
	evidence.Lifecycle.GenerationAfter = generation
	probe.generationByBlock[request.TransportBlock] = generation
	if probe.beforeReturn != nil {
		probe.beforeReturn()
	}
	return evidence, nil
}

func testClock() ClockProvenance {
	return ClockProvenance{Name: "test-counter", Identity: "machine-counter-1", FrequencyHz: 1_000_000_000}
}

func testDescriptor(name string) TransportProvenance {
	version := "1.0.0"
	if name == TransportUSBIP {
		version = USBIPBaselineVersion
	}
	return TransportProvenance{
		Name: name, Implementation: "deterministic-" + name, Version: version,
		BuildIdentity: strings.Repeat(name[:1], 16), Clock: testClock(),
		Artifacts: []ArtifactIdentity{{
			Name: "runtime", SHA256: strings.Repeat("b", 64), Version: "1.0.0",
		}},
		Configuration: []KeyValue{{Key: "mode", Value: "fake"}},
	}
}

func testProvenance() SessionProvenance {
	return SessionProvenance{
		SourceRevision: strings.Repeat("a", 40), HarnessVersion: HarnessVersion,
		RunLabel: "deterministic-unit-test",
		Runtime: RuntimeProvenance{
			GoVersion: "go-test", GOOS: "windows", GOARCH: "amd64", Hostname: "test-host",
		},
	}
}

func testPlan() Plan {
	plan := DefaultPlan("cycle-test", WorkloadProvenance{
		ControllerType: "dualsensegamepadv5", ExpectedVendorID: 0x054c,
		ExpectedProductID: 0x0ce6, Control: "cross",
		APIEndpoint: "127.0.0.1:3242", Authentication: AuthenticatedStreamMode,
		Observer: "deterministic-fake", ObserverVersion: "1.0.0",
		ObserverArtifactSHA256: strings.Repeat("c", 64),
		ObserverEventClock: ClockProvenance{
			Name: "fake-event-clock", Identity: "fake-event-clock-1", FrequencyHz: 1_000_000_000,
		},
	})
	plan.HealthyPairs = 256
	plan.RecoveryPairs = 2
	plan.RecoveryScenario = "scripted-reconnect"
	return plan
}

func testPolicy() Policy {
	policy := DefaultPolicy()
	policy.RequireCleanSource = true
	return policy
}

func evidenceFor(profile BoundaryProfile, path Path, transition Transition, clock ClockProvenance,
	durationNS, ordinal int64, recoveryScenario string) CaptureEvidence {
	stages := RequiredStages(profile)
	start := ordinal*int64(time.Second) + 1
	timestamps := make([]StageTimestamp, len(stages))
	for index, stage := range stages {
		ticks := start
		if len(stages) > 1 {
			ticks += durationNS * int64(index) / int64(len(stages)-1)
		}
		timestamps[index] = StageTimestamp{Stage: stage, Ticks: ticks}
	}
	lifecycle := LifecycleEvidence{GenerationBefore: 1, GenerationAfter: 1}
	if path == PathRecovery {
		lifecycle.GenerationAfter = 2
		lifecycle.RecoveryTriggered = true
		lifecycle.RecoveryReason = recoveryScenario
	}
	return CaptureEvidence{
		Timeline: Timeline{ClockIdentity: clock.Identity, Timestamps: timestamps},
		Observation: ConsumerObservation{
			ClockIdentity:        "fake-event-clock-1",
			PrePublishFenceTicks: uint64(ordinal*1_000_000 + 1),
			ObservedEventTicks:   uint64(ordinal*1_000_000 + 2),
			Control:              "cross", Transition: transition,
		},
		Lifecycle: lifecycle,
	}
}

func runFakeComparison(t *testing.T, baseline, candidate *fakeProbe,
	plan Plan, policy Policy) *Report {
	t.Helper()
	runner := Runner{now: func() time.Time { return time.Unix(1_800_000_000, 0) }}
	report, err := runner.Run(context.Background(), testProvenance(), plan, policy,
		baseline, candidate)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func cloneReportForTest(t *testing.T, report *Report) *Report {
	t.Helper()
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var cloned Report
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		t.Fatal(err)
	}
	return &cloned
}

func TestRunnerComparesUSBIPAndCandidateWithoutNativeDriver(t *testing.T) {
	baseline := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 3_000_000, recoveryNS: 90_000_000}
	candidate := &fakeProbe{descriptor: testDescriptor("future-transport"), healthyNS: 1_500_000, recoveryNS: 80_000_000}
	report := runFakeComparison(t, baseline, candidate, testPlan(), testPolicy())
	if err := RequirePass(report); err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{TransportUSBIP, "future-transport", "future-transport", TransportUSBIP}
	gotOrder := make([]string, len(report.Runs))
	for index := range report.Runs {
		gotOrder[index] = report.Runs[index].Block.Transport
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("ABBA order=%v want=%v", gotOrder, wantOrder)
	}
	if len(candidate.requests) == 0 || candidate.requests[0].Boundary != BoundaryAPIToConsumer {
		t.Fatalf("current live boundary = %q", candidate.requests[0].Boundary)
	}
	for _, request := range candidate.requests {
		if request.Boundary == BoundaryAPIToConsumer {
			for _, stage := range RequiredStages(request.Boundary) {
				if stage == StagePhysicalInputRead {
					t.Fatal("runner requested an invented physical timestamp")
				}
			}
		}
	}

	healthy := report.Paths[0]
	recovery := report.Paths[1]
	if healthy.Path != PathHealthy || healthy.Verdict != "pass" ||
		healthy.Transports[1].EndToEnd.Combined.P95NS != 1_500_000 {
		t.Fatalf("healthy result=%+v", healthy)
	}
	if recovery.Path != PathRecovery || recovery.Verdict != "diagnostic" ||
		recovery.Transports[1].EndToEnd.Combined.P95NS != 80_000_000 {
		t.Fatalf("recovery result=%+v", recovery)
	}
	if healthy.Transports[1].EndToEnd.Combined.MaxNS >= recovery.Transports[1].EndToEnd.Combined.P50NS {
		t.Fatal("recovery samples contaminated the healthy distribution")
	}
	if len(healthy.StageComparisons) != len(RequiredStages(BoundaryAPIToConsumer))-1 {
		t.Fatalf("stage comparisons=%d", len(healthy.StageComparisons))
	}
}

func TestRunnerSupportsPhysicalBoundaryOnlyWhenExplicit(t *testing.T) {
	plan := testPlan()
	plan.HealthyBoundary = BoundaryPhysicalToConsumer
	baseline := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 3_000_000, recoveryNS: 90_000_000}
	candidate := &fakeProbe{descriptor: testDescriptor("future-transport"), healthyNS: 1_500_000, recoveryNS: 80_000_000}
	report := runFakeComparison(t, baseline, candidate, plan, testPolicy())
	if report.Runs[0].Samples[0].Evidence.Timeline.Timestamps[0].Stage != StagePhysicalInputRead {
		t.Fatal("explicit physical boundary did not retain physical-read evidence")
	}
}

func TestRecoveryCanBeGatedIndependently(t *testing.T) {
	policy := testPolicy()
	policy.Recovery.Enabled = true
	plan := testPlan()
	plan.RecoveryPairs = policy.Recovery.MinimumPairs
	baseline := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 3_000_000, recoveryNS: 3_000_000}
	candidate := &fakeProbe{descriptor: testDescriptor("future-transport"), healthyNS: 1_500_000, recoveryNS: 2_000_000}
	report := runFakeComparison(t, baseline, candidate, plan, policy)
	if err := RequirePass(report); err != nil {
		t.Fatal(err)
	}
	if report.Paths[1].Verdict != "pass" || !report.Paths[1].Evaluated {
		t.Fatalf("recovery result=%+v", report.Paths[1])
	}
}

func TestThresholdFailureIsFailClosed(t *testing.T) {
	baseline := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 3_000_000, recoveryNS: 90_000_000}
	candidate := &fakeProbe{descriptor: testDescriptor("future-transport"), healthyNS: 10_000_000, recoveryNS: 80_000_000}
	report := runFakeComparison(t, baseline, candidate, testPlan(), testPolicy())
	if report.Verdict != "fail" || RequirePass(report) == nil {
		t.Fatalf("threshold breach verdict=%q failures=%v", report.Verdict, report.Failures)
	}
	if !strings.Contains(strings.Join(report.Failures, " "), "p95") {
		t.Fatalf("threshold failures=%v", report.Failures)
	}
}

func TestCaptureAndProvenanceFailuresAreRetained(t *testing.T) {
	baseline := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 3_000_000, recoveryNS: 90_000_000}
	candidate := &fakeProbe{
		descriptor: testDescriptor("future-transport"), healthyNS: 1_500_000, recoveryNS: 80_000_000,
		driftAfterRun: true,
		fail: func(request CaptureRequest) bool {
			return !request.Warmup && request.Path == PathHealthy && request.Sequence == 2 &&
				request.Transition == TransitionPress
		},
	}
	report := runFakeComparison(t, baseline, candidate, testPlan(), testPolicy())
	joined := strings.Join(report.Failures, " ")
	if report.Verdict != "fail" || !strings.Contains(joined, "provenance changed") ||
		!strings.Contains(joined, "capture failures") {
		t.Fatalf("verdict=%q failures=%v", report.Verdict, report.Failures)
	}
}

func TestTransportProvenanceSnapshotsDoNotAliasProbeBackingSlices(t *testing.T) {
	baseline := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 3_000_000, recoveryNS: 3_000_000}
	candidate := &fakeProbe{
		descriptor: testDescriptor("future-transport"), healthyNS: 1_500_000, recoveryNS: 2_000_000,
		mutateProvenanceOnCapture: true,
	}
	report := runFakeComparison(t, baseline, candidate, testPlan(), testPolicy())
	if report.Transports[1].Provenance.Artifacts[0].SHA256 != strings.Repeat("b", 64) {
		t.Fatal("admitted provenance snapshot aliased the probe's mutable artifact slice")
	}
	if report.Transports[1].Provenance.Configuration[0].Value != "fake" {
		t.Fatal("admitted provenance snapshot aliased the probe's mutable configuration slice")
	}
	if report.PostRunTransports[1].Provenance == nil ||
		report.PostRunTransports[1].Provenance.Artifacts[0].SHA256 != strings.Repeat("d", 64) {
		t.Fatal("fresh post-run provenance did not retain the mutated runtime identity")
	}
	if report.Verdict != "fail" ||
		!strings.Contains(strings.Join(report.ProvenanceFailures, " "), "provenance changed") {
		t.Fatalf("provenance drift verdict=%q failures=%v", report.Verdict, report.ProvenanceFailures)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReport(bytes.NewReader(encoded)); err != nil {
		t.Fatalf("strict parser rejected an honestly failing drift artifact: %v", err)
	}

	forged := cloneReportForTest(t, report)
	forged.ProvenanceFailures = nil
	forged.Failures = nil
	forged.Verdict = "pass"
	if err := RequirePass(forged); err == nil {
		t.Fatal("RequirePass accepted cleared provenance drift derived fields")
	}
	encoded, err = json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReport(bytes.NewReader(encoded)); err == nil {
		t.Fatal("strict parser accepted cleared provenance drift derived fields")
	}
}

func TestCaptureRejectsLateNilSuccessAndCanceledParent(t *testing.T) {
	plan := testPlan()
	probe := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 1_000_000, recoveryNS: 2_000_000}
	binding := transportBinding{
		role: RoleBaseline, probe: probe,
		descriptor: cloneTransportProvenance(probe.descriptor),
	}
	block := makeSchedule(plan, TransportUSBIP, "future-transport")[0]
	startedAt := time.Now()
	times := []time.Time{
		startedAt,
		startedAt.Add(time.Duration(plan.PerCaptureTimeoutNS)),
	}
	clockIndex := 0
	captureClock := func() time.Time {
		result := times[clockIndex]
		clockIndex++
		return result
	}
	sample, parentCanceled := captureOne(context.Background(), binding, plan, block,
		PathHealthy, 1, TransitionPress, false, captureClock)
	if parentCanceled || !strings.Contains(sample.Failure, "deadline") || sample.EndToEndNS != 0 {
		t.Fatalf("late nil-success sample=%+v parentCanceled=%v", sample, parentCanceled)
	}

	parent, cancel := context.WithCancel(context.Background())
	cancelingProbe := &fakeProbe{
		descriptor: testDescriptor(TransportUSBIP), healthyNS: 1_000_000, recoveryNS: 2_000_000,
		beforeReturn: cancel,
	}
	cancelingBinding := transportBinding{
		role: RoleBaseline, probe: cancelingProbe,
		descriptor: cloneTransportProvenance(cancelingProbe.descriptor),
	}
	_, parentCanceled = captureOne(parent, cancelingBinding, plan, block,
		PathHealthy, 1, TransitionPress, false, time.Now)
	if !parentCanceled {
		t.Fatal("probe returned nil after canceling its parent and was accepted")
	}
}

func TestStrictParserRecomputesEvidence(t *testing.T) {
	baseline := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 3_000_000, recoveryNS: 90_000_000}
	candidate := &fakeProbe{descriptor: testDescriptor("future-transport"), healthyNS: 1_500_000, recoveryNS: 80_000_000}
	report := runFakeComparison(t, baseline, candidate, testPlan(), testPolicy())
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReport(bytes.NewReader(encoded)); err != nil {
		t.Fatal(err)
	}
	report.Runs[0].Samples[2].EndToEndNS++
	tampered, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReport(bytes.NewReader(tampered)); err == nil {
		t.Fatal("tampered raw-sample duration was accepted")
	}
	unknown := append(encoded[:len(encoded)-1], []byte(`,"unknown":true}`)...)
	if _, err := ParseReport(bytes.NewReader(unknown)); err == nil {
		t.Fatal("unknown report field was accepted")
	}
}

func TestStrictParserRejectsCrossSampleStageClockRegression(t *testing.T) {
	baseline := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 3_000_000, recoveryNS: 90_000_000}
	candidate := &fakeProbe{descriptor: testDescriptor("future-transport"), healthyNS: 1_500_000, recoveryNS: 80_000_000}
	report := runFakeComparison(t, baseline, candidate, testPlan(), testPolicy())
	sample := &report.Runs[1].Samples[0]
	originalStart := sample.Evidence.Timeline.Timestamps[0].Ticks
	for index := range sample.Evidence.Timeline.Timestamps {
		sample.Evidence.Timeline.Timestamps[index].Ticks =
			1 + sample.Evidence.Timeline.Timestamps[index].Ticks - originalStart
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReport(bytes.NewReader(encoded)); err == nil ||
		!strings.Contains(err.Error(), "stage clock") {
		t.Fatalf("cross-sample stage regression error=%v", err)
	}
}

func TestStrictVerificationRejectsCrossArmObserverClockRegression(t *testing.T) {
	baseline := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 3_000_000, recoveryNS: 90_000_000}
	candidate := &fakeProbe{descriptor: testDescriptor("future-transport"), healthyNS: 1_500_000, recoveryNS: 80_000_000}
	report := runFakeComparison(t, baseline, candidate, testPlan(), testPolicy())
	sample := &report.Runs[1].Samples[0]
	sample.Evidence.Observation.PrePublishFenceTicks = 1
	sample.Evidence.Observation.ObservedEventTicks = 2
	if err := RequirePass(report); err == nil || !strings.Contains(err.Error(), "event fence") {
		t.Fatalf("cross-arm observer regression RequirePass error=%v", err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReport(bytes.NewReader(encoded)); err == nil ||
		!strings.Contains(err.Error(), "event fence") {
		t.Fatalf("cross-arm observer regression ParseReport error=%v", err)
	}
}

func TestCanonicalMethodologyCannotBeSelfCertified(t *testing.T) {
	baseline := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 3_000_000, recoveryNS: 90_000_000}
	candidate := &fakeProbe{descriptor: testDescriptor("future-transport"), healthyNS: 1_500_000, recoveryNS: 80_000_000}
	valid := runFakeComparison(t, baseline, candidate, testPlan(), testPolicy())
	tests := []struct {
		name   string
		weaken func(*Plan)
	}{
		{"zero warmup", func(plan *Plan) { plan.WarmupPairsPerBlock = 0 }},
		{"zero dwell", func(plan *Plan) { plan.BaseDwellNS = 0 }},
		{"self-hashed phase sweep", func(plan *Plan) {
			plan.PhaseSweepOffsetsNS[1]++
			plan.PhaseSweepSHA256 = PhaseSweepSHA256(plan.PhaseSweepOffsetsNS)
		}},
		{"cycle orientation", func(plan *Plan) { plan.Orientation = OrientationBAAB }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := clonePlan(testPlan())
			test.weaken(&plan)
			if err := plan.validate(); err == nil {
				t.Fatal("noncanonical release plan was accepted directly")
			}
			report := cloneReportForTest(t, valid)
			test.weaken(&report.Plan)
			if err := RequirePass(report); err == nil {
				t.Fatal("RequirePass accepted a noncanonical release plan")
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseReport(bytes.NewReader(encoded)); err == nil {
				t.Fatal("strict parser accepted a noncanonical release plan")
			}
		})
	}
}

func TestReleasePolicyCannotBeWeakened(t *testing.T) {
	tests := []struct {
		name   string
		weaken func(*Policy)
	}{
		{"dirty source", func(policy *Policy) { policy.RequireCleanSource = false }},
		{"sample count", func(policy *Policy) { policy.Healthy.MinimumPairs-- }},
		{"capture failures", func(policy *Policy) { policy.Healthy.RequireNoFailures = false }},
		{"absolute disabled", func(policy *Policy) { policy.Healthy.CandidateLimits.Enabled = false }},
		{"absolute p95", func(policy *Policy) { policy.Healthy.CandidateLimits.P95NS++ }},
		{"comparative disabled", func(policy *Policy) { policy.Healthy.Comparative.Enabled = false }},
		{"comparative p99", func(policy *Policy) { policy.Healthy.Comparative.MaxP99RegressionNS++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := DefaultPolicy()
			test.weaken(&policy)
			if err := policy.validate(); err == nil {
				t.Fatal("weakened release policy was accepted")
			}
		})
	}

	stricter := DefaultPolicy()
	stricter.Healthy.MinimumPairs++
	stricter.Healthy.CandidateLimits.P95NS--
	stricter.Healthy.Comparative.MaxP99RegressionNS--
	stricter.Healthy.Comparative.RequireLowerMean = true
	if err := stricter.validate(); err != nil {
		t.Fatalf("stricter release policy rejected: %v", err)
	}
}

func TestEnabledRecoveryPolicyCannotBeWeakenedOrPassWithoutEvidence(t *testing.T) {
	tests := []struct {
		name   string
		weaken func(*Policy)
	}{
		{"sample floor", func(policy *Policy) { policy.Recovery.MinimumPairs-- }},
		{"capture failures", func(policy *Policy) { policy.Recovery.RequireNoFailures = false }},
		{"absolute disabled", func(policy *Policy) { policy.Recovery.CandidateLimits.Enabled = false }},
		{"absolute max", func(policy *Policy) { policy.Recovery.CandidateLimits.MaxNS++ }},
		{"comparative disabled", func(policy *Policy) { policy.Recovery.Comparative.Enabled = false }},
		{"comparative p95", func(policy *Policy) { policy.Recovery.Comparative.MaxP95RegressionNS++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := DefaultPolicy()
			policy.Recovery.Enabled = true
			test.weaken(&policy)
			if err := policy.validate(); err == nil {
				t.Fatal("weakened enabled recovery policy was accepted")
			}
		})
	}

	plan := testPlan()
	plan.RecoveryPairs = 0
	baseline := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 3_000_000, recoveryNS: 3_000_000}
	candidate := &fakeProbe{descriptor: testDescriptor("future-transport"), healthyNS: 1_500_000, recoveryNS: 2_000_000}
	report := runFakeComparison(t, baseline, candidate, plan, testPolicy())
	report.Policy.Recovery.Enabled = true
	for index := range report.Paths {
		if report.Paths[index].Path == PathRecovery {
			report.Paths[index].Evaluated = true
			report.Paths[index].Verdict = "pass"
			report.Paths[index].Failures = nil
		}
	}
	report.Verdict = "pass"
	report.Failures = nil
	if err := RequirePass(report); err == nil {
		t.Fatal("RequirePass accepted enabled recovery with zero evidence")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReport(bytes.NewReader(encoded)); err == nil {
		t.Fatal("strict parser accepted enabled recovery with zero evidence")
	}

	missingScenario := cloneReportForTest(t, report)
	missingScenario.Plan.RecoveryScenario = ""
	if err := RequirePass(missingScenario); err == nil ||
		!strings.Contains(err.Error(), "recovery scenario") {
		t.Fatalf("enabled recovery without scenario RequirePass error=%v", err)
	}
	encoded, err = json.Marshal(missingScenario)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReport(bytes.NewReader(encoded)); err == nil ||
		!strings.Contains(err.Error(), "recovery scenario") {
		t.Fatalf("enabled recovery without scenario ParseReport error=%v", err)
	}
}

func TestStrictParserRejectsWeakenedEmbeddedPolicy(t *testing.T) {
	baseline := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 3_000_000, recoveryNS: 90_000_000}
	candidate := &fakeProbe{descriptor: testDescriptor("future-transport"), healthyNS: 1_500_000, recoveryNS: 80_000_000}
	report := runFakeComparison(t, baseline, candidate, testPlan(), testPolicy())
	report.Policy.Healthy.MinimumPairs = 0
	if err := RequirePass(report); err == nil {
		t.Fatal("RequirePass accepted a weakened embedded release policy")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReport(bytes.NewReader(encoded)); err == nil {
		t.Fatal("strict parser accepted a weakened embedded release policy")
	}
}

func TestRecoveryScenarioAndGenerationContinuityAreSourceBound(t *testing.T) {
	planWithoutScenario := testPlan()
	planWithoutScenario.RecoveryScenario = ""
	if err := planWithoutScenario.validate(); err == nil {
		t.Fatal("recovery workload without an expected scenario was accepted")
	}

	newReport := func(t *testing.T) *Report {
		t.Helper()
		baseline := &fakeProbe{descriptor: testDescriptor(TransportUSBIP), healthyNS: 3_000_000, recoveryNS: 3_000_000}
		candidate := &fakeProbe{descriptor: testDescriptor("future-transport"), healthyNS: 1_500_000, recoveryNS: 2_000_000}
		return runFakeComparison(t, baseline, candidate, testPlan(), testPolicy())
	}
	findRecovery := func(t *testing.T, report *Report, ordinal int) *Sample {
		t.Helper()
		seen := 0
		for runIndex := range report.Runs {
			for sampleIndex := range report.Runs[runIndex].Samples {
				sample := &report.Runs[runIndex].Samples[sampleIndex]
				if sample.Path == PathRecovery {
					if seen == ordinal {
						return sample
					}
					seen++
				}
			}
		}
		t.Fatalf("recovery sample %d not found", ordinal)
		return nil
	}

	wrongReason := newReport(t)
	findRecovery(t, wrongReason, 0).Evidence.Lifecycle.RecoveryReason = "different-recovery"
	if err := RequirePass(wrongReason); err == nil || !strings.Contains(err.Error(), "expected scenario") {
		t.Fatalf("wrong recovery scenario RequirePass error=%v", err)
	}
	encoded, err := json.Marshal(wrongReason)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReport(bytes.NewReader(encoded)); err == nil ||
		!strings.Contains(err.Error(), "expected scenario") {
		t.Fatalf("wrong recovery scenario ParseReport error=%v", err)
	}

	reusedGeneration := newReport(t)
	first := findRecovery(t, reusedGeneration, 0).Evidence.Lifecycle
	second := findRecovery(t, reusedGeneration, 1)
	second.Evidence.Lifecycle.GenerationBefore = first.GenerationBefore
	second.Evidence.Lifecycle.GenerationAfter = first.GenerationAfter
	if err := RequirePass(reusedGeneration); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("reused generation RequirePass error=%v", err)
	}
	encoded, err = json.Marshal(reusedGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReport(bytes.NewReader(encoded)); err == nil ||
		!strings.Contains(err.Error(), "generation") {
		t.Fatalf("reused generation ParseReport error=%v", err)
	}
}

func TestBAABScheduleAndPhaseSweepAreDeterministic(t *testing.T) {
	plan := testPlan()
	plan.CycleIndex = 2
	plan.CycleCount = 2
	plan.Orientation = OrientationBAAB
	blocks := makeSchedule(plan, TransportUSBIP, "future-transport")
	want := []TransportRole{RoleCandidate, RoleBaseline, RoleBaseline, RoleCandidate}
	for index := range blocks {
		if blocks[index].Role != want[index] {
			t.Fatalf("block %d role=%s want=%s", index, blocks[index].Role, want[index])
		}
	}
	offsets := DefaultPhaseSweepOffsetsNS()
	for sequence := 1; sequence <= 8; sequence++ {
		press := phaseOffset(offsets, sequence, TransitionPress)
		release := phaseOffset(offsets, sequence, TransitionRelease)
		if press != offsets[(2*(sequence-1))%len(offsets)] ||
			release != offsets[(2*(sequence-1)+1)%len(offsets)] {
			t.Fatal(fmt.Sprintf("sequence %d phase pair changed", sequence))
		}
	}
}

func TestRunnerRejectsWrongUSBIPRuntimeVersion(t *testing.T) {
	baselineDescriptor := testDescriptor(TransportUSBIP)
	baselineDescriptor.Version = "unexpected"
	baseline := &fakeProbe{descriptor: baselineDescriptor, healthyNS: 3_000_000, recoveryNS: 90_000_000}
	candidate := &fakeProbe{descriptor: testDescriptor("future-transport"), healthyNS: 1_500_000, recoveryNS: 80_000_000}
	runner := NewRunner()
	_, err := runner.Run(context.Background(), testProvenance(), testPlan(), testPolicy(), baseline, candidate)
	if err == nil || !strings.Contains(err.Error(), USBIPBaselineVersion) {
		t.Fatalf("wrong USB/IP version error=%v", err)
	}
}

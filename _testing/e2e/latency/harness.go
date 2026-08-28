package latency

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

type Orientation string

const (
	OrientationABBA Orientation = "abba"
	OrientationBAAB Orientation = "baab"

	CanonicalWarmupPairsPerBlock = 16
	CanonicalBaseDwellNS         = 2 * int64(time.Millisecond)
)

type TransportRole string

const (
	RoleBaseline  TransportRole = "baseline"
	RoleCandidate TransportRole = "candidate"
)

// Probe is the only seam a live USB/IP or future transport adapter must
// implement. Capture must return raw stage timestamps; the harness, not the
// adapter, computes latency and the verdict.
type Probe interface {
	Describe(context.Context) (TransportProvenance, error)
	Capture(context.Context, CaptureRequest) (CaptureEvidence, error)
}

type CaptureRequest struct {
	CycleID          string          `json:"cycle_id"`
	CycleIndex       int             `json:"cycle_index"`
	BlockOrder       int             `json:"block_order"`
	TransportBlock   int             `json:"transport_block"`
	Sequence         int             `json:"sequence"`
	Path             Path            `json:"path"`
	Boundary         BoundaryProfile `json:"boundary"`
	Transition       Transition      `json:"transition"`
	Warmup           bool            `json:"warmup"`
	RecoveryScenario string          `json:"recovery_scenario,omitempty"`
	BaseDwellNS      int64           `json:"base_dwell_ns"`
	PhaseOffsetNS    int64           `json:"phase_offset_ns"`
}

type Plan struct {
	CycleID              string             `json:"cycle_id"`
	CycleIndex           int                `json:"cycle_index"`
	CycleCount           int                `json:"cycle_count"`
	Orientation          Orientation        `json:"orientation"`
	HealthyBoundary      BoundaryProfile    `json:"healthy_boundary"`
	ExpectedUSBIPVersion string             `json:"expected_usbip_version"`
	Workload             WorkloadProvenance `json:"workload"`
	HealthyPairs         int                `json:"healthy_pairs"`
	RecoveryPairs        int                `json:"recovery_pairs"`
	RecoveryScenario     string             `json:"recovery_scenario,omitempty"`
	WarmupPairsPerBlock  int                `json:"warmup_pairs_per_block"`
	PerCaptureTimeoutNS  int64              `json:"per_capture_timeout_ns"`
	BaseDwellNS          int64              `json:"base_dwell_ns"`
	PhaseSweepOffsetsNS  []int64            `json:"phase_sweep_offsets_ns"`
	PhaseSweepSHA256     string             `json:"phase_sweep_sha256"`
}

func DefaultPlan(cycleID string, workload WorkloadProvenance) Plan {
	offsets := DefaultPhaseSweepOffsetsNS()
	return Plan{
		CycleID: cycleID, CycleIndex: 1, CycleCount: 1,
		Orientation:          OrientationABBA,
		HealthyBoundary:      BoundaryAPIToConsumer,
		ExpectedUSBIPVersion: USBIPBaselineVersion,
		Workload:             workload,
		HealthyPairs:         256, RecoveryPairs: 0,
		WarmupPairsPerBlock: CanonicalWarmupPairsPerBlock,
		PerCaptureTimeoutNS: int64(time.Second),
		BaseDwellNS:         CanonicalBaseDwellNS,
		PhaseSweepOffsetsNS: offsets,
		PhaseSweepSHA256:    PhaseSweepSHA256(offsets),
	}
}

func (plan Plan) validate() error {
	if strings.TrimSpace(plan.CycleID) == "" {
		return errors.New("cycle ID is required")
	}
	if plan.CycleIndex < 1 || plan.CycleCount < 1 || plan.CycleIndex > plan.CycleCount {
		return errors.New("cycle index/count are invalid")
	}
	if plan.Orientation != OrientationABBA && plan.Orientation != OrientationBAAB {
		return fmt.Errorf("unsupported schedule orientation %q", plan.Orientation)
	}
	if plan.Orientation != orientationForCycle(plan.CycleIndex) {
		return fmt.Errorf("cycle %d must use canonical %s orientation",
			plan.CycleIndex, orientationForCycle(plan.CycleIndex))
	}
	if plan.HealthyBoundary != BoundaryAPIToConsumer &&
		plan.HealthyBoundary != BoundaryPhysicalToConsumer {
		return fmt.Errorf("unsupported healthy boundary profile %q", plan.HealthyBoundary)
	}
	if plan.ExpectedUSBIPVersion != USBIPBaselineVersion {
		return fmt.Errorf("release USB/IP baseline version must be %q", USBIPBaselineVersion)
	}
	if err := plan.Workload.validate(); err != nil {
		return fmt.Errorf("workload: %w", err)
	}
	if plan.HealthyPairs < 1 || plan.RecoveryPairs < 0 {
		return errors.New("healthy, recovery, or warmup pair count is invalid")
	}
	if plan.WarmupPairsPerBlock != CanonicalWarmupPairsPerBlock {
		return fmt.Errorf("release methodology requires exactly %d warmup pairs per block",
			CanonicalWarmupPairsPerBlock)
	}
	if plan.PerCaptureTimeoutNS <= 0 || plan.BaseDwellNS != CanonicalBaseDwellNS {
		return errors.New("capture timeout must be positive and release dwell must be the canonical 2 ms")
	}
	canonicalOffsets := DefaultPhaseSweepOffsetsNS()
	if !reflect.DeepEqual(plan.PhaseSweepOffsetsNS, canonicalOffsets) ||
		plan.PhaseSweepSHA256 != PhaseSweepSHA256(canonicalOffsets) {
		return errors.New("release methodology requires the canonical eight-phase sweep")
	}
	if plan.RecoveryPairs > 0 && strings.TrimSpace(plan.RecoveryScenario) == "" {
		return errors.New("recovery samples require an expected recovery scenario")
	}
	if plan.RecoveryScenario != strings.TrimSpace(plan.RecoveryScenario) {
		return errors.New("recovery scenario must be canonical without surrounding whitespace")
	}
	if err := validateCanonicalBlockSchedule(plan); err != nil {
		return err
	}
	return nil
}

func orientationForCycle(cycleIndex int) Orientation {
	if cycleIndex%2 == 0 {
		return OrientationBAAB
	}
	return OrientationABBA
}

type BlockSpec struct {
	Order                 int           `json:"order"`
	Role                  TransportRole `json:"role"`
	Transport             string        `json:"transport"`
	TransportBlock        int           `json:"transport_block"`
	HealthyFirstSequence  int           `json:"healthy_first_sequence"`
	HealthyPairs          int           `json:"healthy_pairs"`
	RecoveryFirstSequence int           `json:"recovery_first_sequence"`
	RecoveryPairs         int           `json:"recovery_pairs"`
}

type Runner struct {
	now        func() time.Time
	captureNow func() time.Time
}

func NewRunner() Runner {
	return Runner{now: time.Now, captureNow: time.Now}
}

type transportBinding struct {
	role       TransportRole
	probe      Probe
	descriptor TransportProvenance
}

// Run captures one counterbalanced cycle. Configuration/provenance errors are
// returned because no trustworthy artifact can be produced. Individual
// capture failures are retained in a fail-closed report so recovery and tail
// diagnostics are not erased by the first timeout.
func (runner Runner) Run(ctx context.Context, provenance SessionProvenance, plan Plan,
	policy Policy, baseline, candidate Probe) (*Report, error) {
	if ctx == nil {
		return nil, errors.New("nil latency context")
	}
	if baseline == nil || candidate == nil {
		return nil, errors.New("baseline and candidate probes are required")
	}
	plan = clonePlan(plan)
	if err := provenance.validate(); err != nil {
		return nil, fmt.Errorf("session provenance: %w", err)
	}
	if err := plan.validate(); err != nil {
		return nil, fmt.Errorf("latency plan: %w", err)
	}
	if err := policy.validate(); err != nil {
		return nil, fmt.Errorf("latency policy: %w", err)
	}
	if policy.Recovery.Enabled && strings.TrimSpace(plan.RecoveryScenario) == "" {
		return nil, errors.New("enabled recovery policy requires an expected recovery scenario")
	}
	now := runner.now
	if now == nil {
		now = time.Now
	}
	captureNow := runner.captureNow
	if captureNow == nil {
		captureNow = time.Now
	}

	baselineDescriptor, err := baseline.Describe(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe USB/IP baseline: %w", err)
	}
	baselineDescriptor = cloneTransportProvenance(baselineDescriptor)
	candidateDescriptor, err := candidate.Describe(ctx)
	if err != nil {
		return nil, fmt.Errorf("describe candidate transport: %w", err)
	}
	candidateDescriptor = cloneTransportProvenance(candidateDescriptor)
	if err := validateTransportPair(plan, baselineDescriptor, candidateDescriptor); err != nil {
		return nil, err
	}

	bindings := map[TransportRole]transportBinding{
		RoleBaseline:  {role: RoleBaseline, probe: baseline, descriptor: cloneTransportProvenance(baselineDescriptor)},
		RoleCandidate: {role: RoleCandidate, probe: candidate, descriptor: cloneTransportProvenance(candidateDescriptor)},
	}
	report := &Report{
		Schema: SchemaV1, GeneratedAt: now().UTC(), Provenance: provenance,
		Plan: plan, Policy: policy,
		Transports: []TransportRecord{
			{Role: RoleBaseline, Provenance: cloneTransportProvenance(baselineDescriptor)},
			{Role: RoleCandidate, Provenance: cloneTransportProvenance(candidateDescriptor)},
		},
	}
	for _, block := range makeSchedule(plan, baselineDescriptor.Name, candidateDescriptor.Name) {
		binding := bindings[block.Role]
		run := Run{Block: block}
		for warmupSequence := 1; warmupSequence <= plan.WarmupPairsPerBlock; warmupSequence++ {
			for _, transition := range []Transition{TransitionPress, TransitionRelease} {
				sample, parentCanceled := captureOne(ctx, binding, plan, block,
					PathHealthy, warmupSequence, transition, true, captureNow)
				if parentCanceled {
					return nil, ctx.Err()
				}
				run.Samples = append(run.Samples, sample)
			}
		}
		captureMeasuredPath := func(path Path, firstSequence, pairs int) error {
			for offset := 0; offset < pairs; offset++ {
				sequence := firstSequence + offset
				for _, transition := range []Transition{TransitionPress, TransitionRelease} {
					sample, parentCanceled := captureOne(ctx, binding, plan, block,
						path, sequence, transition, false, captureNow)
					if parentCanceled {
						return ctx.Err()
					}
					run.Samples = append(run.Samples, sample)
				}
			}
			return nil
		}
		if err := captureMeasuredPath(PathHealthy, block.HealthyFirstSequence, block.HealthyPairs); err != nil {
			return nil, err
		}
		if err := captureMeasuredPath(PathRecovery, block.RecoveryFirstSequence, block.RecoveryPairs); err != nil {
			return nil, err
		}
		report.Runs = append(report.Runs, run)
	}

	for _, binding := range []transportBinding{bindings[RoleBaseline], bindings[RoleCandidate]} {
		after, describeErr := binding.probe.Describe(ctx)
		if describeErr != nil {
			report.PostRunTransports = append(report.PostRunTransports, PostRunTransportRecord{
				Role: binding.role, Transport: binding.descriptor.Name, Failure: describeErr.Error(),
			})
			continue
		}
		after = cloneTransportProvenance(after)
		report.PostRunTransports = append(report.PostRunTransports, PostRunTransportRecord{
			Role: binding.role, Transport: binding.descriptor.Name, Provenance: &after,
		})
	}
	if err := Finalize(report); err != nil {
		return nil, err
	}
	return report, nil
}

func clonePlan(plan Plan) Plan {
	cloned := plan
	cloned.PhaseSweepOffsetsNS = append([]int64(nil), plan.PhaseSweepOffsetsNS...)
	return cloned
}

func validateTransportPair(plan Plan, baseline, candidate TransportProvenance) error {
	if err := baseline.validate(); err != nil {
		return fmt.Errorf("baseline provenance: %w", err)
	}
	if err := candidate.validate(); err != nil {
		return fmt.Errorf("candidate provenance: %w", err)
	}
	if baseline.Name != TransportUSBIP {
		return fmt.Errorf("baseline transport is %q, want %q", baseline.Name, TransportUSBIP)
	}
	if baseline.Version != plan.ExpectedUSBIPVersion {
		return fmt.Errorf("USB/IP baseline version is %q, want %q", baseline.Version,
			plan.ExpectedUSBIPVersion)
	}
	if candidate.Name == TransportUSBIP || candidate.Name == baseline.Name {
		return errors.New("candidate transport must be distinct from USB/IP")
	}
	if baseline.Clock != candidate.Clock {
		return errors.New("baseline and candidate must use the same clock identity and frequency")
	}
	return nil
}

func makeSchedule(plan Plan, baselineName, candidateName string) []BlockSpec {
	firstRole, secondRole := RoleBaseline, RoleCandidate
	firstName, secondName := baselineName, candidateName
	if plan.Orientation == OrientationBAAB {
		firstRole, secondRole = secondRole, firstRole
		firstName, secondName = secondName, firstName
	}
	healthyFirst, healthySecond := splitPairs(plan.HealthyPairs)
	recoveryFirst, recoverySecond := splitPairs(plan.RecoveryPairs)
	return []BlockSpec{
		{Order: 1, Role: firstRole, Transport: firstName, TransportBlock: 1,
			HealthyFirstSequence: 1, HealthyPairs: healthyFirst,
			RecoveryFirstSequence: 1, RecoveryPairs: recoveryFirst},
		{Order: 2, Role: secondRole, Transport: secondName, TransportBlock: 1,
			HealthyFirstSequence: 1, HealthyPairs: healthyFirst,
			RecoveryFirstSequence: 1, RecoveryPairs: recoveryFirst},
		{Order: 3, Role: secondRole, Transport: secondName, TransportBlock: 2,
			HealthyFirstSequence: healthyFirst + 1, HealthyPairs: healthySecond,
			RecoveryFirstSequence: recoveryFirst + 1, RecoveryPairs: recoverySecond},
		{Order: 4, Role: firstRole, Transport: firstName, TransportBlock: 2,
			HealthyFirstSequence: healthyFirst + 1, HealthyPairs: healthySecond,
			RecoveryFirstSequence: recoveryFirst + 1, RecoveryPairs: recoverySecond},
	}
}

func validateCanonicalBlockSchedule(plan Plan) error {
	blocks := makeSchedule(plan, "baseline", "candidate")
	if len(blocks) != 4 {
		return errors.New("release schedule must contain exactly four transport blocks")
	}
	wantRoles := []TransportRole{RoleBaseline, RoleCandidate, RoleCandidate, RoleBaseline}
	if plan.Orientation == OrientationBAAB {
		wantRoles = []TransportRole{RoleCandidate, RoleBaseline, RoleBaseline, RoleCandidate}
	}
	roleBlocks := map[TransportRole]int{}
	roleSpecs := map[TransportRole][]BlockSpec{}
	for index, block := range blocks {
		roleBlocks[block.Role]++
		roleSpecs[block.Role] = append(roleSpecs[block.Role], block)
		if block.Order != index+1 || block.Role != wantRoles[index] ||
			block.TransportBlock != roleBlocks[block.Role] {
			return errors.New("release block schedule does not satisfy ABBA/BAAB invariants")
		}
	}
	if roleBlocks[RoleBaseline] != 2 || roleBlocks[RoleCandidate] != 2 {
		return errors.New("release block schedule must contain two blocks per transport")
	}
	for _, role := range []TransportRole{RoleBaseline, RoleCandidate} {
		first, second := roleSpecs[role][0], roleSpecs[role][1]
		if first.HealthyFirstSequence != 1 ||
			second.HealthyFirstSequence != first.HealthyPairs+1 ||
			first.HealthyPairs+second.HealthyPairs != plan.HealthyPairs ||
			first.RecoveryFirstSequence != 1 ||
			second.RecoveryFirstSequence != first.RecoveryPairs+1 ||
			first.RecoveryPairs+second.RecoveryPairs != plan.RecoveryPairs {
			return errors.New("release block schedule does not preserve per-transport sequence continuity")
		}
	}
	return nil
}

func splitPairs(pairs int) (int, int) {
	first := pairs / 2
	return first, pairs - first
}

func captureOne(parent context.Context, binding transportBinding, plan Plan, block BlockSpec,
	path Path, sequence int, transition Transition, warmup bool,
	captureNow func() time.Time) (Sample, bool) {
	request := CaptureRequest{
		CycleID: plan.CycleID, CycleIndex: plan.CycleIndex,
		BlockOrder: block.Order, TransportBlock: block.TransportBlock,
		Sequence: sequence, Path: path, Boundary: boundaryForPath(plan, path),
		Transition: transition, Warmup: warmup,
		RecoveryScenario: recoveryScenarioForPath(plan, path),
		BaseDwellNS:      plan.BaseDwellNS,
		PhaseOffsetNS:    phaseOffset(plan.PhaseSweepOffsetsNS, sequence, transition),
	}
	sample := Sample{
		Sequence: sequence, Transition: transition, Path: path, Warmup: warmup,
		Request: request,
	}
	if captureNow == nil {
		captureNow = time.Now
	}
	captureStartedAt := captureNow()
	requestedDeadline := captureStartedAt.Add(time.Duration(plan.PerCaptureTimeoutNS))
	captureContext, cancel := context.WithDeadline(parent, requestedDeadline)
	evidence, err := binding.probe.Capture(captureContext, request)
	captureCompletedAt := captureNow()
	captureContextErr := captureContext.Err()
	parentErr := parent.Err()
	effectiveDeadline, hasDeadline := captureContext.Deadline()
	cancel()
	if parentErr != nil {
		return Sample{}, true
	}
	if captureCompletedAt.Before(captureStartedAt) {
		sample.Failure = "capture completion clock regressed"
		return sample, false
	}
	if captureContextErr != nil || (hasDeadline && !captureCompletedAt.Before(effectiveDeadline)) {
		sample.Failure = "capture completed at or after its deadline"
		return sample, false
	}
	if err != nil {
		sample.Failure = err.Error()
		return sample, false
	}
	sample.Evidence = evidence
	total, spans, err := ValidateCapture(path, request.Boundary, transition,
		binding.descriptor.Clock, plan.Workload, request.RecoveryScenario, evidence)
	if err != nil {
		sample.Failure = err.Error()
		return sample, false
	}
	sample.EndToEndNS = total
	sample.StageSpans = spans
	return sample, false
}

func recoveryScenarioForPath(plan Plan, path Path) string {
	if path == PathRecovery {
		return plan.RecoveryScenario
	}
	return ""
}

func boundaryForPath(plan Plan, path Path) BoundaryProfile {
	if path == PathRecovery {
		return BoundaryRecoveryToConsumer
	}
	return plan.HealthyBoundary
}

func phaseOffset(offsets []int64, sequence int, transition Transition) int64 {
	if len(offsets) == 0 || sequence < 1 {
		return 0
	}
	edge := 2 * (sequence - 1)
	if transition == TransitionRelease {
		edge++
	}
	return offsets[edge%len(offsets)]
}

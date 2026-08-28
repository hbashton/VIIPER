package latency

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"
)

type DistributionLimits struct {
	Enabled bool  `json:"enabled"`
	P95NS   int64 `json:"p95_ns"`
	P99NS   int64 `json:"p99_ns"`
	MaxNS   int64 `json:"max_ns"`
}

type ComparativeLimits struct {
	Enabled              bool  `json:"enabled"`
	MaxP95RegressionNS   int64 `json:"max_p95_regression_ns"`
	MaxP99RegressionNS   int64 `json:"max_p99_regression_ns"`
	MaxWorstRegressionNS int64 `json:"max_worst_regression_ns"`
	RequireLowerMean     bool  `json:"require_lower_mean"`
	RequireLowerP95      bool  `json:"require_lower_p95"`
	RequireLowerP99      bool  `json:"require_lower_p99"`
}

type PathPolicy struct {
	Enabled           bool               `json:"enabled"`
	MinimumPairs      int                `json:"minimum_pairs"`
	RequireNoFailures bool               `json:"require_no_failures"`
	CandidateLimits   DistributionLimits `json:"candidate_limits"`
	Comparative       ComparativeLimits  `json:"comparative"`
}

type Policy struct {
	RequireCleanSource bool       `json:"require_clean_source"`
	Healthy            PathPolicy `json:"healthy"`
	Recovery           PathPolicy `json:"recovery"`
}

// DefaultPolicy carries forward the reviewed steady-state engineering gates.
// Recovery is deliberately diagnostic until callers enable the populated
// canonical floor for an exact lifecycle scenario.
func DefaultPolicy() Policy {
	canonicalPath := PathPolicy{
		Enabled: true, MinimumPairs: 256, RequireNoFailures: true,
		CandidateLimits: DistributionLimits{
			Enabled: true, P95NS: 4 * int64(time.Millisecond),
			P99NS: 8 * int64(time.Millisecond), MaxNS: 20 * int64(time.Millisecond),
		},
		Comparative: ComparativeLimits{
			Enabled: true, MaxP95RegressionNS: int64(time.Millisecond),
			MaxP99RegressionNS:   2 * int64(time.Millisecond),
			MaxWorstRegressionNS: 5 * int64(time.Millisecond),
		},
	}
	recovery := canonicalPath
	recovery.Enabled = false
	return Policy{
		RequireCleanSource: true,
		Healthy:            canonicalPath,
		Recovery:           recovery,
	}
}

func (policy Policy) validate() error {
	for name, pathPolicy := range map[string]PathPolicy{
		"healthy": policy.Healthy, "recovery": policy.Recovery,
	} {
		if pathPolicy.MinimumPairs < 0 {
			return fmt.Errorf("%s minimum pairs is negative", name)
		}
		limits := pathPolicy.CandidateLimits
		if limits.P95NS < 0 || limits.P99NS < 0 || limits.MaxNS < 0 {
			return fmt.Errorf("%s candidate limits are negative", name)
		}
		if limits.Enabled && (limits.P95NS <= 0 || limits.P99NS < limits.P95NS || limits.MaxNS < limits.P99NS) {
			return fmt.Errorf("%s candidate limits must satisfy 0 < p95 <= p99 <= max", name)
		}
		comparison := pathPolicy.Comparative
		if comparison.MaxP95RegressionNS < 0 || comparison.MaxP99RegressionNS < 0 ||
			comparison.MaxWorstRegressionNS < 0 {
			return fmt.Errorf("%s comparative limits are negative", name)
		}
	}
	if !policy.Healthy.Enabled {
		return errors.New("healthy path policy must remain enabled")
	}
	canonical := DefaultPolicy()
	if !policy.RequireCleanSource {
		return errors.New("release latency policy cannot permit a dirty source tree")
	}
	if policy.Healthy.MinimumPairs < canonical.Healthy.MinimumPairs {
		return fmt.Errorf("release healthy policy requires at least %d pairs",
			canonical.Healthy.MinimumPairs)
	}
	if !policy.Healthy.RequireNoFailures {
		return errors.New("release healthy policy must reject capture and warmup failures")
	}
	if !policy.Healthy.CandidateLimits.Enabled ||
		policy.Healthy.CandidateLimits.P95NS > canonical.Healthy.CandidateLimits.P95NS ||
		policy.Healthy.CandidateLimits.P99NS > canonical.Healthy.CandidateLimits.P99NS ||
		policy.Healthy.CandidateLimits.MaxNS > canonical.Healthy.CandidateLimits.MaxNS {
		return errors.New("release healthy absolute limits are weaker than the canonical policy")
	}
	if !policy.Healthy.Comparative.Enabled ||
		policy.Healthy.Comparative.MaxP95RegressionNS > canonical.Healthy.Comparative.MaxP95RegressionNS ||
		policy.Healthy.Comparative.MaxP99RegressionNS > canonical.Healthy.Comparative.MaxP99RegressionNS ||
		policy.Healthy.Comparative.MaxWorstRegressionNS > canonical.Healthy.Comparative.MaxWorstRegressionNS {
		return errors.New("release healthy comparative limits are weaker than the canonical policy")
	}
	if policy.Recovery.Enabled {
		if policy.Recovery.MinimumPairs < canonical.Recovery.MinimumPairs {
			return fmt.Errorf("enabled recovery policy requires at least %d pairs",
				canonical.Recovery.MinimumPairs)
		}
		if !policy.Recovery.RequireNoFailures {
			return errors.New("enabled recovery policy must reject capture failures")
		}
		if !policy.Recovery.CandidateLimits.Enabled ||
			policy.Recovery.CandidateLimits.P95NS > canonical.Recovery.CandidateLimits.P95NS ||
			policy.Recovery.CandidateLimits.P99NS > canonical.Recovery.CandidateLimits.P99NS ||
			policy.Recovery.CandidateLimits.MaxNS > canonical.Recovery.CandidateLimits.MaxNS {
			return errors.New("enabled recovery absolute limits are weaker than the canonical policy")
		}
		if !policy.Recovery.Comparative.Enabled ||
			policy.Recovery.Comparative.MaxP95RegressionNS > canonical.Recovery.Comparative.MaxP95RegressionNS ||
			policy.Recovery.Comparative.MaxP99RegressionNS > canonical.Recovery.Comparative.MaxP99RegressionNS ||
			policy.Recovery.Comparative.MaxWorstRegressionNS > canonical.Recovery.Comparative.MaxWorstRegressionNS {
			return errors.New("enabled recovery comparative limits are weaker than the canonical policy")
		}
	}
	return nil
}

type TransportRecord struct {
	Role       TransportRole       `json:"role"`
	Provenance TransportProvenance `json:"provenance"`
}

// PostRunTransportRecord retains the fresh post-capture Describe result so a
// strict parser can reproduce provenance-drift failures from source evidence.
type PostRunTransportRecord struct {
	Role       TransportRole        `json:"role"`
	Transport  string               `json:"transport"`
	Provenance *TransportProvenance `json:"provenance,omitempty"`
	Failure    string               `json:"failure,omitempty"`
}

type Sample struct {
	Sequence   int             `json:"sequence"`
	Transition Transition      `json:"transition"`
	Path       Path            `json:"path"`
	Warmup     bool            `json:"warmup"`
	Request    CaptureRequest  `json:"request"`
	Evidence   CaptureEvidence `json:"evidence"`
	EndToEndNS int64           `json:"end_to_end_ns"`
	StageSpans []StageSpan     `json:"stage_spans,omitempty"`
	Failure    string          `json:"failure,omitempty"`
}

type Run struct {
	Block   BlockSpec `json:"block"`
	Samples []Sample  `json:"samples"`
}

type Distribution struct {
	Count    int     `json:"count"`
	MeanNS   float64 `json:"mean_ns"`
	P50NS    int64   `json:"p50_ns"`
	P90NS    int64   `json:"p90_ns"`
	P95NS    int64   `json:"p95_ns"`
	P99NS    int64   `json:"p99_ns"`
	P999NS   int64   `json:"p99_9_ns"`
	MaxNS    int64   `json:"max_ns"`
	JitterNS float64 `json:"jitter_ns"`
}

type DistributionSet struct {
	Press    Distribution `json:"press"`
	Release  Distribution `json:"release"`
	Combined Distribution `json:"combined"`
}

type StageDistribution struct {
	From       Stage        `json:"from"`
	To         Stage        `json:"to"`
	Statistics Distribution `json:"statistics"`
}

type TransportPathAggregate struct {
	Role               TransportRole       `json:"role"`
	Transport          string              `json:"transport"`
	Path               Path                `json:"path"`
	CaptureFailures    int                 `json:"capture_failures"`
	WarmupFailures     int                 `json:"warmup_failures"`
	EndToEnd           DistributionSet     `json:"end_to_end"`
	StageDistributions []StageDistribution `json:"stage_distributions"`
}

type MetricComparison struct {
	Baseline       float64  `json:"baseline"`
	Candidate      float64  `json:"candidate"`
	CandidateDelta float64  `json:"candidate_minus_baseline"`
	CandidateRatio *float64 `json:"candidate_to_baseline_ratio,omitempty"`
}

type DistributionComparison struct {
	Mean   MetricComparison `json:"mean_ns"`
	P50    MetricComparison `json:"p50_ns"`
	P90    MetricComparison `json:"p90_ns"`
	P95    MetricComparison `json:"p95_ns"`
	P99    MetricComparison `json:"p99_ns"`
	P999   MetricComparison `json:"p99_9_ns"`
	Max    MetricComparison `json:"max_ns"`
	Jitter MetricComparison `json:"jitter_ns"`
}

type DistributionSetComparison struct {
	Press    DistributionComparison `json:"press"`
	Release  DistributionComparison `json:"release"`
	Combined DistributionComparison `json:"combined"`
}

type StageComparison struct {
	From       Stage                  `json:"from"`
	To         Stage                  `json:"to"`
	Statistics DistributionComparison `json:"statistics"`
}

type PathResult struct {
	Path             Path                      `json:"path"`
	Evaluated        bool                      `json:"evaluated"`
	Transports       []TransportPathAggregate  `json:"transports"`
	EndToEnd         DistributionSetComparison `json:"end_to_end"`
	StageComparisons []StageComparison         `json:"stage_comparisons"`
	Verdict          string                    `json:"verdict"`
	Failures         []string                  `json:"failures"`
}

type Report struct {
	Schema             string                   `json:"schema"`
	GeneratedAt        time.Time                `json:"generated_at"`
	Provenance         SessionProvenance        `json:"provenance"`
	Plan               Plan                     `json:"plan"`
	Policy             Policy                   `json:"policy"`
	Transports         []TransportRecord        `json:"transports"`
	PostRunTransports  []PostRunTransportRecord `json:"post_run_transports"`
	Runs               []Run                    `json:"runs"`
	ProvenanceFailures []string                 `json:"provenance_failures,omitempty"`
	Paths              []PathResult             `json:"paths"`
	Verdict            string                   `json:"verdict"`
	Failures           []string                 `json:"failures"`
}

// Calculate returns nearest-rank percentiles and population standard
// deviation without reordering the caller's samples.
func Calculate(values []int64) (Distribution, error) {
	if len(values) == 0 {
		return Distribution{}, errors.New("cannot summarize zero latency samples")
	}
	ordered := append([]int64(nil), values...)
	for index, value := range ordered {
		if value <= 0 {
			return Distribution{}, fmt.Errorf("latency sample %d must be positive", index)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	mean, m2 := 0.0, 0.0
	for index, value := range values {
		x := float64(value)
		delta := x - mean
		mean += delta / float64(index+1)
		m2 += delta * (x - mean)
	}
	return Distribution{
		Count: len(ordered), MeanNS: mean,
		P50NS: nearestRank(ordered, 0.50), P90NS: nearestRank(ordered, 0.90),
		P95NS: nearestRank(ordered, 0.95), P99NS: nearestRank(ordered, 0.99),
		P999NS: nearestRank(ordered, 0.999), MaxNS: ordered[len(ordered)-1],
		JitterNS: math.Sqrt(m2 / float64(len(ordered))),
	}, nil
}

func nearestRank(ordered []int64, percentile float64) int64 {
	rank := int(math.Ceil(percentile*float64(len(ordered)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(ordered) {
		rank = len(ordered) - 1
	}
	return ordered[rank]
}

// Finalize revalidates every source sample, recomputes all derived fields, and
// evaluates healthy and recovery policy independently.
func Finalize(report *Report) error {
	if report == nil {
		return errors.New("nil latency report")
	}
	if err := validateReportBase(report); err != nil {
		return err
	}

	report.Paths = nil
	report.ProvenanceFailures = deriveProvenanceFailures(report)
	report.Failures = append([]string(nil), report.ProvenanceFailures...)
	if report.Policy.RequireCleanSource && report.Provenance.SourceTreeDirty {
		report.Failures = append(report.Failures, "source tree was dirty during latency capture")
	}
	for _, path := range []Path{PathHealthy, PathRecovery} {
		pathPolicy := report.Policy.Healthy
		if path == PathRecovery {
			pathPolicy = report.Policy.Recovery
		}
		result, err := finalizePath(report, path, pathPolicy)
		if err != nil {
			return err
		}
		report.Paths = append(report.Paths, result)
		if result.Evaluated {
			for _, failure := range result.Failures {
				report.Failures = append(report.Failures, string(path)+": "+failure)
			}
		}
	}
	if len(report.Failures) == 0 {
		report.Verdict = "pass"
	} else {
		report.Verdict = "fail"
	}
	return nil
}

func validateReportBase(report *Report) error {
	if report.Schema != SchemaV1 || report.GeneratedAt.IsZero() {
		return errors.New("latency report schema or generation time is invalid")
	}
	if err := report.Provenance.validate(); err != nil {
		return err
	}
	if err := report.Plan.validate(); err != nil {
		return err
	}
	if err := report.Policy.validate(); err != nil {
		return err
	}
	if report.Policy.Recovery.Enabled && strings.TrimSpace(report.Plan.RecoveryScenario) == "" {
		return errors.New("enabled recovery policy lacks an expected recovery scenario")
	}
	if len(report.Transports) != 2 || report.Transports[0].Role != RoleBaseline ||
		report.Transports[1].Role != RoleCandidate {
		return errors.New("report must contain baseline then candidate provenance")
	}
	baseline := report.Transports[0].Provenance
	candidate := report.Transports[1].Provenance
	if err := validateTransportPair(report.Plan, baseline, candidate); err != nil {
		return err
	}
	if err := validatePostRunTransports(report); err != nil {
		return err
	}
	expectedBlocks := makeSchedule(report.Plan, baseline.Name, candidate.Name)
	if len(report.Runs) != len(expectedBlocks) {
		return fmt.Errorf("report has %d runs, want %d", len(report.Runs), len(expectedBlocks))
	}
	clocks := map[TransportRole]ClockProvenance{
		RoleBaseline: baseline.Clock, RoleCandidate: candidate.Clock,
	}
	var lastObserverTicks uint64
	var lastStageTicks int64
	for runIndex := range report.Runs {
		run := &report.Runs[runIndex]
		if !reflect.DeepEqual(run.Block, expectedBlocks[runIndex]) {
			return fmt.Errorf("run %d contradicts the counterbalanced schedule", runIndex)
		}
		if err := validateRunSamples(report.Plan, run, clocks[run.Block.Role],
			&lastObserverTicks, &lastStageTicks); err != nil {
			return fmt.Errorf("%s block %d: %w", run.Block.Transport, run.Block.TransportBlock, err)
		}
	}
	return nil
}

func validatePostRunTransports(report *Report) error {
	if len(report.PostRunTransports) != len(report.Transports) {
		return fmt.Errorf("report has %d post-run transport records, want %d",
			len(report.PostRunTransports), len(report.Transports))
	}
	for index, post := range report.PostRunTransports {
		expected := report.Transports[index]
		if post.Role != expected.Role || post.Transport != expected.Provenance.Name {
			return fmt.Errorf("post-run transport record %d does not match its admitted snapshot", index)
		}
		if (post.Provenance == nil) == (post.Failure == "") {
			return fmt.Errorf("post-run transport record %d must contain exactly one result", index)
		}
	}
	return nil
}

func deriveProvenanceFailures(report *Report) []string {
	var failures []string
	for index, post := range report.PostRunTransports {
		admitted := report.Transports[index].Provenance
		if post.Failure != "" {
			failures = append(failures, fmt.Sprintf("%s post-run provenance failed: %s",
				post.Transport, post.Failure))
			continue
		}
		if err := post.Provenance.validate(); err != nil {
			failures = append(failures, fmt.Sprintf("%s post-run provenance is invalid: %v",
				post.Transport, err))
			continue
		}
		if !reflect.DeepEqual(*post.Provenance, admitted) {
			failures = append(failures,
				fmt.Sprintf("%s runtime provenance changed during capture", post.Transport))
		}
	}
	return failures
}

func validateRunSamples(plan Plan, run *Run, clock ClockProvenance,
	lastObserverTicks *uint64, lastStageTicks *int64) error {
	expected := make([]CaptureRequest, 0,
		2*(plan.WarmupPairsPerBlock+run.Block.HealthyPairs+run.Block.RecoveryPairs))
	appendPair := func(path Path, sequence int, warmup bool) {
		for _, transition := range []Transition{TransitionPress, TransitionRelease} {
			expected = append(expected, CaptureRequest{
				CycleID: plan.CycleID, CycleIndex: plan.CycleIndex,
				BlockOrder: run.Block.Order, TransportBlock: run.Block.TransportBlock,
				Sequence: sequence, Path: path, Transition: transition, Warmup: warmup,
				Boundary:         boundaryForPath(plan, path),
				RecoveryScenario: recoveryScenarioForPath(plan, path),
				BaseDwellNS:      plan.BaseDwellNS,
				PhaseOffsetNS:    phaseOffset(plan.PhaseSweepOffsetsNS, sequence, transition),
			})
		}
	}
	for sequence := 1; sequence <= plan.WarmupPairsPerBlock; sequence++ {
		appendPair(PathHealthy, sequence, true)
	}
	for offset := 0; offset < run.Block.HealthyPairs; offset++ {
		appendPair(PathHealthy, run.Block.HealthyFirstSequence+offset, false)
	}
	for offset := 0; offset < run.Block.RecoveryPairs; offset++ {
		appendPair(PathRecovery, run.Block.RecoveryFirstSequence+offset, false)
	}
	if len(run.Samples) != len(expected) {
		return fmt.Errorf("has %d captures, want %d", len(run.Samples), len(expected))
	}
	var lastGeneration uint64
	for index := range run.Samples {
		sample := &run.Samples[index]
		request := expected[index]
		if !reflect.DeepEqual(sample.Request, request) || sample.Sequence != request.Sequence ||
			sample.Path != request.Path || sample.Transition != request.Transition || sample.Warmup != request.Warmup {
			return fmt.Errorf("sample %d identity or phase schedule changed", index)
		}
		if sample.Failure != "" {
			if sample.EndToEndNS != 0 || len(sample.StageSpans) != 0 {
				return fmt.Errorf("failed sample %d contains derived latency", index)
			}
			lastGeneration = 0
			continue
		}
		total, spans, err := ValidateCapture(sample.Path, sample.Request.Boundary,
			sample.Transition, clock, plan.Workload,
			sample.Request.RecoveryScenario, sample.Evidence)
		if err != nil {
			return fmt.Errorf("sample %d evidence: %w", index, err)
		}
		if sample.EndToEndNS != total || !reflect.DeepEqual(sample.StageSpans, spans) {
			return fmt.Errorf("sample %d derived latency does not match raw stage ticks", index)
		}
		firstStageTicks := sample.Evidence.Timeline.Timestamps[0].Ticks
		finalStageTicks := sample.Evidence.Timeline.Timestamps[len(sample.Evidence.Timeline.Timestamps)-1].Ticks
		if lastStageTicks != nil && firstStageTicks <= *lastStageTicks {
			return fmt.Errorf("sample %d shared stage clock overlapped or regressed", index)
		}
		if lastStageTicks != nil {
			*lastStageTicks = finalStageTicks
		}
		observation := sample.Evidence.Observation
		if lastObserverTicks != nil && observation.PrePublishFenceTicks <= *lastObserverTicks {
			return fmt.Errorf("sample %d consumer event fence did not follow the preceding observation", index)
		}
		if lastObserverTicks != nil {
			*lastObserverTicks = observation.ObservedEventTicks
		}
		lifecycle := sample.Evidence.Lifecycle
		if lastGeneration != 0 && lifecycle.GenerationBefore != lastGeneration {
			return fmt.Errorf("sample %d lifecycle generation is discontinuous: before=%d want=%d",
				index, lifecycle.GenerationBefore, lastGeneration)
		}
		lastGeneration = lifecycle.GenerationAfter
	}
	return nil
}

func finalizePath(report *Report, path Path, policy PathPolicy) (PathResult, error) {
	result := PathResult{Path: path, Evaluated: policy.Enabled}
	for _, record := range report.Transports {
		aggregate, err := aggregatePath(report.Runs, record.Role, record.Provenance.Name,
			path, boundaryForPath(report.Plan, path))
		if err != nil {
			return PathResult{}, err
		}
		result.Transports = append(result.Transports, aggregate)
	}
	baseline, candidate := result.Transports[0], result.Transports[1]
	if distributionsComparable(baseline.EndToEnd, candidate.EndToEnd) {
		result.EndToEnd = compareSets(baseline.EndToEnd, candidate.EndToEnd)
	}
	if len(baseline.StageDistributions) == len(candidate.StageDistributions) {
		for index := range baseline.StageDistributions {
			left, right := baseline.StageDistributions[index], candidate.StageDistributions[index]
			if left.From != right.From || left.To != right.To || left.Statistics.Count == 0 || right.Statistics.Count == 0 {
				continue
			}
			result.StageComparisons = append(result.StageComparisons, StageComparison{
				From: left.From, To: left.To,
				Statistics: compareDistribution(left.Statistics, right.Statistics),
			})
		}
	}
	if !policy.Enabled {
		result.Verdict = "diagnostic"
		return result, nil
	}
	for _, aggregate := range result.Transports {
		if aggregate.EndToEnd.Press.Count < policy.MinimumPairs || aggregate.EndToEnd.Release.Count < policy.MinimumPairs {
			result.Failures = append(result.Failures, fmt.Sprintf(
				"%s has press/release counts %d/%d; requires %d each",
				aggregate.Transport, aggregate.EndToEnd.Press.Count,
				aggregate.EndToEnd.Release.Count, policy.MinimumPairs))
		}
		if policy.RequireNoFailures && (aggregate.CaptureFailures != 0 || aggregate.WarmupFailures != 0) {
			result.Failures = append(result.Failures, fmt.Sprintf(
				"%s has %d measured and %d warmup capture failures",
				aggregate.Transport, aggregate.CaptureFailures, aggregate.WarmupFailures))
		}
	}
	if candidate.EndToEnd.Press.Count > 0 && baseline.EndToEnd.Press.Count > 0 {
		for _, item := range []struct {
			name      string
			baseline  Distribution
			candidate Distribution
		}{
			{"press", baseline.EndToEnd.Press, candidate.EndToEnd.Press},
			{"release", baseline.EndToEnd.Release, candidate.EndToEnd.Release},
			{"combined", baseline.EndToEnd.Combined, candidate.EndToEnd.Combined},
		} {
			result.Failures = append(result.Failures,
				checkDistributionPolicy(item.name, item.baseline, item.candidate, policy)...)
		}
	}
	if len(result.Failures) == 0 {
		result.Verdict = "pass"
	} else {
		result.Verdict = "fail"
	}
	return result, nil
}

func aggregatePath(runs []Run, role TransportRole, transport string, path Path,
	boundary BoundaryProfile) (TransportPathAggregate, error) {
	aggregate := TransportPathAggregate{Role: role, Transport: transport, Path: path}
	press, release := []int64{}, []int64{}
	spanValues := make(map[string][]int64)
	spanIdentity := make(map[string]StageSpan)
	for runIndex := range runs {
		run := &runs[runIndex]
		if run.Block.Role != role {
			continue
		}
		for sampleIndex := range run.Samples {
			sample := &run.Samples[sampleIndex]
			if sample.Path != path {
				continue
			}
			if sample.Failure != "" {
				if sample.Warmup {
					aggregate.WarmupFailures++
				} else {
					aggregate.CaptureFailures++
				}
				continue
			}
			if sample.Warmup {
				continue
			}
			if sample.Transition == TransitionPress {
				press = append(press, sample.EndToEndNS)
			} else {
				release = append(release, sample.EndToEndNS)
			}
			for _, span := range sample.StageSpans {
				key := string(span.From) + "\x00" + string(span.To)
				spanValues[key] = append(spanValues[key], span.DurationNS)
				spanIdentity[key] = span
			}
		}
	}
	aggregate.EndToEnd = calculateSet(press, release)
	for _, stages := range adjacentStages(boundary) {
		key := string(stages[0]) + "\x00" + string(stages[1])
		values := spanValues[key]
		if len(values) == 0 {
			continue
		}
		statistics, err := Calculate(values)
		if err != nil {
			return TransportPathAggregate{}, err
		}
		identity := spanIdentity[key]
		aggregate.StageDistributions = append(aggregate.StageDistributions,
			StageDistribution{From: identity.From, To: identity.To, Statistics: statistics})
	}
	return aggregate, nil
}

func adjacentStages(boundary BoundaryProfile) [][2]Stage {
	stages := RequiredStages(boundary)
	pairs := make([][2]Stage, 0, len(stages)-1)
	for index := 1; index < len(stages); index++ {
		pairs = append(pairs, [2]Stage{stages[index-1], stages[index]})
	}
	return pairs
}

func calculateSet(press, release []int64) DistributionSet {
	combined := append(append([]int64(nil), press...), release...)
	return DistributionSet{
		Press: calculateIfPresent(press), Release: calculateIfPresent(release),
		Combined: calculateIfPresent(combined),
	}
}

func calculateIfPresent(values []int64) Distribution {
	if len(values) == 0 {
		return Distribution{}
	}
	result, _ := Calculate(values)
	return result
}

func distributionsComparable(baseline, candidate DistributionSet) bool {
	return baseline.Press.Count > 0 && baseline.Release.Count > 0 &&
		candidate.Press.Count > 0 && candidate.Release.Count > 0
}

func compareSets(baseline, candidate DistributionSet) DistributionSetComparison {
	return DistributionSetComparison{
		Press:    compareDistribution(baseline.Press, candidate.Press),
		Release:  compareDistribution(baseline.Release, candidate.Release),
		Combined: compareDistribution(baseline.Combined, candidate.Combined),
	}
}

func compareDistribution(baseline, candidate Distribution) DistributionComparison {
	return DistributionComparison{
		Mean:   compareMetric(baseline.MeanNS, candidate.MeanNS),
		P50:    compareMetric(float64(baseline.P50NS), float64(candidate.P50NS)),
		P90:    compareMetric(float64(baseline.P90NS), float64(candidate.P90NS)),
		P95:    compareMetric(float64(baseline.P95NS), float64(candidate.P95NS)),
		P99:    compareMetric(float64(baseline.P99NS), float64(candidate.P99NS)),
		P999:   compareMetric(float64(baseline.P999NS), float64(candidate.P999NS)),
		Max:    compareMetric(float64(baseline.MaxNS), float64(candidate.MaxNS)),
		Jitter: compareMetric(baseline.JitterNS, candidate.JitterNS),
	}
}

func compareMetric(baseline, candidate float64) MetricComparison {
	comparison := MetricComparison{
		Baseline: baseline, Candidate: candidate, CandidateDelta: candidate - baseline,
	}
	if baseline != 0 {
		ratio := candidate / baseline
		comparison.CandidateRatio = &ratio
	}
	return comparison
}

func checkDistributionPolicy(name string, baseline, candidate Distribution, policy PathPolicy) []string {
	failures := []string{}
	if limits := policy.CandidateLimits; limits.Enabled {
		if candidate.P95NS > limits.P95NS {
			failures = append(failures, fmt.Sprintf("candidate %s p95 %dns exceeds %dns", name, candidate.P95NS, limits.P95NS))
		}
		if candidate.P99NS > limits.P99NS {
			failures = append(failures, fmt.Sprintf("candidate %s p99 %dns exceeds %dns", name, candidate.P99NS, limits.P99NS))
		}
		if candidate.MaxNS > limits.MaxNS {
			failures = append(failures, fmt.Sprintf("candidate %s max %dns exceeds %dns", name, candidate.MaxNS, limits.MaxNS))
		}
	}
	if limits := policy.Comparative; limits.Enabled {
		if candidate.P95NS-baseline.P95NS > limits.MaxP95RegressionNS {
			failures = append(failures, fmt.Sprintf("candidate %s p95 regressed by %dns; allowance %dns", name,
				candidate.P95NS-baseline.P95NS, limits.MaxP95RegressionNS))
		}
		if candidate.P99NS-baseline.P99NS > limits.MaxP99RegressionNS {
			failures = append(failures, fmt.Sprintf("candidate %s p99 regressed by %dns; allowance %dns", name,
				candidate.P99NS-baseline.P99NS, limits.MaxP99RegressionNS))
		}
		if candidate.MaxNS-baseline.MaxNS > limits.MaxWorstRegressionNS {
			failures = append(failures, fmt.Sprintf("candidate %s max regressed by %dns; allowance %dns", name,
				candidate.MaxNS-baseline.MaxNS, limits.MaxWorstRegressionNS))
		}
		if limits.RequireLowerMean && candidate.MeanNS >= baseline.MeanNS {
			failures = append(failures, fmt.Sprintf("candidate %s mean was not lower than USB/IP", name))
		}
		if limits.RequireLowerP95 && candidate.P95NS >= baseline.P95NS {
			failures = append(failures, fmt.Sprintf("candidate %s p95 was not lower than USB/IP", name))
		}
		if limits.RequireLowerP99 && candidate.P99NS >= baseline.P99NS {
			failures = append(failures, fmt.Sprintf("candidate %s p99 was not lower than USB/IP", name))
		}
	}
	return failures
}

// ParseReport is the strict artifact parser. It rejects unknown/trailing JSON
// and any aggregate, delta, verdict, or sample duration that cannot be exactly
// recomputed from the raw capture evidence.
func ParseReport(reader io.Reader) (*Report, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var report Report
	if err := decoder.Decode(&report); err != nil {
		return nil, fmt.Errorf("decode latency report: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("latency report contains trailing JSON")
		}
		return nil, fmt.Errorf("decode trailing latency data: %w", err)
	}
	reportedPaths := append([]PathResult(nil), report.Paths...)
	reportedProvenanceFailures := append([]string(nil), report.ProvenanceFailures...)
	reportedVerdict := report.Verdict
	reportedFailures := append([]string(nil), report.Failures...)
	if err := Finalize(&report); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(reportedPaths, report.Paths) || reportedVerdict != report.Verdict ||
		!reflect.DeepEqual(reportedProvenanceFailures, report.ProvenanceFailures) ||
		!reflect.DeepEqual(reportedFailures, report.Failures) {
		return nil, errors.New("latency aggregates, comparisons, or verdict do not match raw captures")
	}
	return &report, nil
}

func RequirePass(report *Report) error {
	if report == nil {
		return errors.New("nil latency report")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode latency report for strict verification: %w", err)
	}
	verified, err := ParseReport(bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("latency report does not carry canonical release evidence: %w", err)
	}
	if verified.Verdict == "pass" && len(verified.Failures) == 0 {
		return nil
	}
	if len(verified.Failures) == 0 {
		return fmt.Errorf("latency verdict is %q", verified.Verdict)
	}
	return errors.New(strings.Join(verified.Failures, "; "))
}

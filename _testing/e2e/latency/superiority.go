package latency

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"
)

const (
	SuperioritySchemaV1       = "viiper.controller-to-game.latency-superiority/v1"
	SuperiorityMinimumCycles  = 6
	SuperiorityMethod         = "cycle-level paired native-minus-usbip all-observed-cycle strict superiority"
	SuperiorityInferenceScope = "descriptive for this exact source-bound machine session; no iid or population-confidence claim"
	SuperiorityRequiredMargin = 0.0
)

// SuperiorityCycle binds one strictly parsed suite to the process priority
// under which its balanced transport order was captured.
type SuperiorityCycle struct {
	Priority string
	Suite    *SuiteReport
}

type CycleEstimate struct {
	CycleIndex         int     `json:"cycle_index"`
	Orientation        string  `json:"orientation"`
	NativeMinusUSBIPNS float64 `json:"native_minus_usbip_ns"`
}

type SuperiorityMetric struct {
	Priority                     string          `json:"priority_class"`
	Controller                   string          `json:"controller_type"`
	Transition                   Transition      `json:"transition"`
	Metric                       string          `json:"metric"`
	Cycles                       []CycleEstimate `json:"cycles"`
	MeanNativeMinusUSBIPNS       float64         `json:"mean_native_minus_usbip_ns"`
	MedianNativeMinusUSBIPNS     float64         `json:"median_native_minus_usbip_ns"`
	WorstCycleNativeMinusUSBIPNS float64         `json:"worst_cycle_native_minus_usbip_ns"`
	Wins                         int             `json:"wins"`
	LossesOrTies                 int             `json:"losses_or_ties"`
	AllObservedCyclesFaster      bool            `json:"all_observed_cycles_faster"`
	Verdict                      string          `json:"verdict"`
}

type SuperiorityReport struct {
	Schema                           string              `json:"schema"`
	GeneratedAt                      time.Time           `json:"generated_at"`
	SourceRevision                   string              `json:"source_revision"`
	NativePackageManifestSHA256      string              `json:"native_package_manifest_sha256"`
	NativePackageValidationMode      string              `json:"native_package_validation_mode"`
	NativeLocalTestCertificateSHA256 string              `json:"native_local_test_certificate_sha256,omitempty"`
	NativeDriverSHA256               string              `json:"native_driver_sha256"`
	NativeDriverBuildIdentity        string              `json:"native_driver_build_identity"`
	CycleID                          string              `json:"cycle_id"`
	CycleCount                       int                 `json:"cycle_count"`
	CyclesPerPriority                int                 `json:"cycles_per_priority"`
	SamplePairsPerCycle              int                 `json:"sample_pairs_per_cycle"`
	Method                           string              `json:"method"`
	InferenceScope                   string              `json:"inference_scope"`
	Metrics                          []SuperiorityMetric `json:"metrics"`
	Verdict                          string              `json:"verdict"`
	Failures                         []string            `json:"failures"`
}

type superiorityCycleData struct {
	priority     string
	suite        *SuiteReport
	cycleIndex   int
	orientation  string
	generatedAt  time.Time
	minStartQPC  int64
	maxMarkerQPC int64
}

// AnalyzeSuperiority treats each counterbalanced cycle as one observed unit.
// It makes no independence, population-confidence, or cross-machine claim.
// Every controller, press/release direction, and mean/p95/p99 metric must be
// lower in every observed cycle at both process priorities.
func AnalyzeSuperiority(cycles []SuperiorityCycle, generatedAt time.Time) (*SuperiorityReport, error) {
	if generatedAt.IsZero() {
		return nil, errors.New("superiority generated_at is required")
	}
	if len(cycles) < 2*SuperiorityMinimumCycles || len(cycles)%2 != 0 {
		return nil, fmt.Errorf("superiority analysis requires an even total with at least %d cycles per priority",
			SuperiorityMinimumCycles)
	}

	data := make([]superiorityCycleData, 0, len(cycles))
	seenCycle := make(map[int]bool, len(cycles))
	priorityCounts := map[string]int{"normal": 0, "high": 0}
	var sourceRevision, cycleID string
	var samplePairs int
	var reference Provenance
	for inputIndex, cycle := range cycles {
		if cycle.Priority != "normal" && cycle.Priority != "high" {
			return nil, fmt.Errorf("cycle %d has unsupported priority %q", inputIndex, cycle.Priority)
		}
		if cycle.Suite == nil {
			return nil, fmt.Errorf("cycle %d has nil suite", inputIndex)
		}
		if err := RequireSuitePass(cycle.Suite); err != nil {
			return nil, fmt.Errorf("cycle %d is not a passing source suite: %w", inputIndex, err)
		}
		if len(cycle.Suite.Cases) != 3 {
			return nil, fmt.Errorf("cycle %d has %d controller cases", inputIndex, len(cycle.Suite.Cases))
		}
		workload := cycle.Suite.Cases[0].Workload
		if workload.CycleCount != len(cycles) || seenCycle[workload.CycleIndex] ||
			workload.CycleIndex < 1 || workload.CycleIndex > len(cycles) {
			return nil, fmt.Errorf("cycle %d has duplicate or contradictory cycle identity", inputIndex)
		}
		seenCycle[workload.CycleIndex] = true
		if workload.ScheduleOrientation != ScheduleOrientationForCycle(workload.CycleIndex) {
			return nil, fmt.Errorf("cycle %d orientation is not deterministic", workload.CycleIndex)
		}
		if cycle.Suite.Provenance.Machine.ProcessPriorityClass != cycle.Priority {
			return nil, fmt.Errorf("cycle %d priority label does not match machine provenance",
				workload.CycleIndex)
		}
		wantPriority := "normal"
		if workload.CycleIndex > len(cycles)/2 {
			wantPriority = "high"
		}
		if cycle.Priority != wantPriority {
			return nil, fmt.Errorf("cycle %d priority=%s want %s for canonical matrix order",
				workload.CycleIndex, cycle.Priority, wantPriority)
		}
		if inputIndex == 0 {
			sourceRevision = cycle.Suite.Provenance.SourceRevision
			cycleID = workload.CycleID
			samplePairs = workload.SamplePairs
			reference = cycle.Suite.Provenance
			reference.Machine.ProcessPriorityClass = ""
		} else {
			candidate := cycle.Suite.Provenance
			candidate.Machine.ProcessPriorityClass = ""
			if !reflect.DeepEqual(candidate, reference) {
				return nil, fmt.Errorf("cycle %d source, package, toolchain, or machine provenance drifted",
					workload.CycleIndex)
			}
			if workload.CycleID != cycleID || workload.SamplePairs != samplePairs {
				return nil, fmt.Errorf("cycle %d workload identity drifted", workload.CycleIndex)
			}
		}
		for caseIndex := range cycle.Suite.Cases {
			caseWorkload := cycle.Suite.Cases[caseIndex].Workload
			if caseWorkload.CycleID != workload.CycleID ||
				caseWorkload.CycleIndex != workload.CycleIndex ||
				caseWorkload.CycleCount != workload.CycleCount ||
				caseWorkload.ScheduleOrientation != workload.ScheduleOrientation {
				return nil, fmt.Errorf("cycle %d controller workloads disagree", workload.CycleIndex)
			}
		}
		minStartQPC := int64(math.MaxInt64)
		maxMarkerQPC := int64(0)
		for caseIndex := range cycle.Suite.Cases {
			for runIndex := range cycle.Suite.Cases[caseIndex].Runs {
				for _, sample := range cycle.Suite.Cases[caseIndex].Runs[runIndex].Samples {
					if sample.StartQPCTicks < minStartQPC {
						minStartQPC = sample.StartQPCTicks
					}
					if sample.MarkerQPCTicks > maxMarkerQPC {
						maxMarkerQPC = sample.MarkerQPCTicks
					}
				}
			}
		}
		if minStartQPC <= 0 || maxMarkerQPC <= minStartQPC {
			return nil, fmt.Errorf("cycle %d has no canonical QPC measurement interval",
				workload.CycleIndex)
		}
		priorityCounts[cycle.Priority]++
		data = append(data, superiorityCycleData{
			priority: cycle.Priority, suite: cycle.Suite,
			cycleIndex: workload.CycleIndex, orientation: workload.ScheduleOrientation,
			generatedAt: cycle.Suite.GeneratedAt,
			minStartQPC: minStartQPC, maxMarkerQPC: maxMarkerQPC,
		})
	}
	cyclesPerPriority := len(cycles) / 2
	if cyclesPerPriority < SuperiorityMinimumCycles || cyclesPerPriority%2 != 0 ||
		priorityCounts["normal"] != cyclesPerPriority || priorityCounts["high"] != cyclesPerPriority {
		return nil, errors.New("normal and high priorities require equal, even, counterbalanced cycle counts")
	}
	sort.Slice(data, func(i, j int) bool { return data[i].cycleIndex < data[j].cycleIndex })
	for index := range data {
		if data[index].cycleIndex != index+1 {
			return nil, errors.New("latency cycle indices are not contiguous")
		}
		if index != 0 && (!data[index].generatedAt.After(data[index-1].generatedAt) ||
			data[index].minStartQPC <= data[index-1].maxMarkerQPC) {
			return nil, fmt.Errorf("cycle %d reuses or overlaps prior wall-clock/QPC evidence",
				data[index].cycleIndex)
		}
	}

	result := &SuperiorityReport{
		Schema: SuperioritySchemaV1, GeneratedAt: generatedAt,
		SourceRevision: sourceRevision, CycleID: cycleID,
		NativePackageManifestSHA256:      reference.NativePackageManifestSHA256,
		NativePackageValidationMode:      reference.NativePackageValidationMode,
		NativeLocalTestCertificateSHA256: reference.NativeLocalTestCertificateSHA256,
		NativeDriverSHA256:               reference.NativeDriverSHA256,
		NativeDriverBuildIdentity:        reference.NativeDriverBuildIdentity,
		CycleCount:                       len(cycles), CyclesPerPriority: cyclesPerPriority,
		SamplePairsPerCycle: samplePairs, Method: SuperiorityMethod,
		InferenceScope: SuperiorityInferenceScope,
	}
	for _, priority := range []string{"normal", "high"} {
		for caseIndex := 0; caseIndex < 3; caseIndex++ {
			controller := data[0].suite.Cases[caseIndex].Workload.ControllerType
			for _, transition := range []Transition{TransitionPress, TransitionRelease} {
				cycleMetrics := make(map[string][]CycleEstimate, 3)
				for _, cycle := range data {
					if cycle.priority != priority {
						continue
					}
					report := &cycle.suite.Cases[caseIndex]
					metrics, err := cycleSuperiorityMetrics(report, transition)
					if err != nil {
						return nil, fmt.Errorf("cycle %d %s/%s: %w", cycle.cycleIndex,
							controller, transition, err)
					}
					for metric, value := range metrics {
						cycleMetrics[metric] = append(cycleMetrics[metric], CycleEstimate{
							CycleIndex: cycle.cycleIndex, Orientation: cycle.orientation,
							NativeMinusUSBIPNS: value,
						})
					}
				}
				for _, metricName := range []string{"mean", "p95", "p99"} {
					metric, err := summarizeSuperiorityMetric(priority, controller, transition,
						metricName, cycleMetrics[metricName])
					if err != nil {
						return nil, err
					}
					if metric.Verdict != "pass" {
						result.Failures = append(result.Failures, fmt.Sprintf(
							"%s/%s/%s %s was not lower in every observed balanced cycle",
							priority, controller, transition, metricName))
					}
					result.Metrics = append(result.Metrics, metric)
				}
			}
		}
	}
	if len(result.Failures) == 0 {
		result.Verdict = "pass"
	} else {
		result.Verdict = "fail"
	}
	return result, nil
}

func cycleSuperiorityMetrics(report *Report, transition Transition) (map[string]float64, error) {
	values := map[string][]int64{TransportUSBIP: nil, TransportNativeUDE: nil}
	sequences := map[string]map[int]int64{
		TransportUSBIP: {}, TransportNativeUDE: {},
	}
	for runIndex := range report.Runs {
		run := &report.Runs[runIndex]
		for _, sample := range run.Samples {
			if sample.Transition != transition {
				continue
			}
			if _, exists := sequences[run.Transport][sample.Sequence]; exists {
				return nil, fmt.Errorf("duplicate %s sequence %d", run.Transport, sample.Sequence)
			}
			sequences[run.Transport][sample.Sequence] = sample.LatencyNS
		}
	}
	pairedDifferences := make([]int64, 0, report.Workload.SamplePairs)
	for sequence := 1; sequence <= report.Workload.SamplePairs; sequence++ {
		usbip, usbipOK := sequences[TransportUSBIP][sequence]
		native, nativeOK := sequences[TransportNativeUDE][sequence]
		if !usbipOK || !nativeOK {
			return nil, fmt.Errorf("missing paired sequence %d", sequence)
		}
		values[TransportUSBIP] = append(values[TransportUSBIP], usbip)
		values[TransportNativeUDE] = append(values[TransportNativeUDE], native)
		pairedDifferences = append(pairedDifferences, native-usbip)
	}
	usbipDistribution, err := Calculate(values[TransportUSBIP])
	if err != nil {
		return nil, err
	}
	nativeDistribution, err := Calculate(values[TransportNativeUDE])
	if err != nil {
		return nil, err
	}
	var exactDifferenceSum int64
	for _, value := range pairedDifferences {
		if (value > 0 && exactDifferenceSum > math.MaxInt64-value) ||
			(value < 0 && exactDifferenceSum < math.MinInt64-value) {
			return nil, errors.New("paired latency difference sum overflow")
		}
		exactDifferenceSum += value
	}
	meanDifference := float64(exactDifferenceSum) / float64(len(pairedDifferences))
	return map[string]float64{
		"mean": meanDifference,
		"p95":  float64(nativeDistribution.P95NS - usbipDistribution.P95NS),
		"p99":  float64(nativeDistribution.P99NS - usbipDistribution.P99NS),
	}, nil
}

func summarizeSuperiorityMetric(priority, controller string, transition Transition,
	metricName string, estimates []CycleEstimate) (SuperiorityMetric, error) {
	if len(estimates) < SuperiorityMinimumCycles {
		return SuperiorityMetric{}, fmt.Errorf("%s/%s/%s %s has only %d cycles",
			priority, controller, transition, metricName, len(estimates))
	}
	mean := 0.0
	wins := 0
	ordered := make([]float64, 0, len(estimates))
	worst := math.Inf(-1)
	for index, estimate := range estimates {
		mean += (estimate.NativeMinusUSBIPNS - mean) / float64(index+1)
		ordered = append(ordered, estimate.NativeMinusUSBIPNS)
		if estimate.NativeMinusUSBIPNS < SuperiorityRequiredMargin {
			wins++
		}
		if estimate.NativeMinusUSBIPNS > worst {
			worst = estimate.NativeMinusUSBIPNS
		}
	}
	sort.Float64s(ordered)
	median := ordered[len(ordered)/2]
	if len(ordered)%2 == 0 {
		median = (ordered[len(ordered)/2-1] + ordered[len(ordered)/2]) / 2
	}
	allObservedCyclesFaster := wins == len(estimates) && worst < 0
	verdict := "fail"
	if allObservedCyclesFaster {
		verdict = "pass"
	}
	return SuperiorityMetric{
		Priority: priority, Controller: controller, Transition: transition,
		Metric: metricName, Cycles: append([]CycleEstimate(nil), estimates...),
		MeanNativeMinusUSBIPNS: mean, MedianNativeMinusUSBIPNS: median,
		WorstCycleNativeMinusUSBIPNS: worst, Wins: wins,
		LossesOrTies:            len(estimates) - wins,
		AllObservedCyclesFaster: allObservedCyclesFaster,
		Verdict:                 verdict,
	}, nil
}

func RequireSuperiority(report *SuperiorityReport) error {
	if report == nil {
		return errors.New("nil superiority report")
	}
	if report.Schema != SuperioritySchemaV1 || report.Verdict != "pass" ||
		report.Method != SuperiorityMethod || report.InferenceScope != SuperiorityInferenceScope ||
		!revisionPattern.MatchString(report.SourceRevision) ||
		!hashPattern.MatchString(report.NativePackageManifestSHA256) ||
		!hashPattern.MatchString(report.NativeDriverSHA256) ||
		!hashPattern.MatchString(report.NativeDriverBuildIdentity) ||
		(report.NativePackageValidationMode != PackageValidationProduction &&
			report.NativePackageValidationMode != PackageValidationLocalTest) ||
		(report.NativePackageValidationMode == PackageValidationProduction &&
			report.NativeLocalTestCertificateSHA256 != "") ||
		(report.NativePackageValidationMode == PackageValidationLocalTest &&
			!hashPattern.MatchString(report.NativeLocalTestCertificateSHA256)) ||
		len(report.Failures) != 0 {
		if len(report.Failures) != 0 {
			return errors.New(strings.Join(report.Failures, "; "))
		}
		return fmt.Errorf("superiority report schema/verdict is %q/%q",
			report.Schema, report.Verdict)
	}
	for _, metric := range report.Metrics {
		if metric.Verdict != "pass" || !metric.AllObservedCyclesFaster ||
			metric.Wins != len(metric.Cycles) || metric.LossesOrTies != 0 ||
			metric.WorstCycleNativeMinusUSBIPNS >= SuperiorityRequiredMargin {
			return fmt.Errorf("metric %s/%s/%s/%s is not strictly faster in every observed cycle",
				metric.Priority, metric.Controller, metric.Transition, metric.Metric)
		}
	}
	return nil
}

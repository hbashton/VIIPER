package latency

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var productionPhaseSweepOffsetsNS = [...]int64{
	0,
	125 * int64(time.Microsecond),
	250 * int64(time.Microsecond),
	375 * int64(time.Microsecond),
	500 * int64(time.Microsecond),
	625 * int64(time.Microsecond),
	750 * int64(time.Microsecond),
	875 * int64(time.Microsecond),
}

const (
	SchemaV3                               = "viiper.controller-to-game.latency/v3"
	SuiteSchemaV3                          = "viiper.controller-to-game.latency-suite/v3"
	TransportUSBIP                         = "usbip"
	TransportNativeUDE                     = "native-ude"
	AuthenticationMode                     = "password-authenticated-encrypted-stream"
	TraceProviderName                      = "VIIPER-LatencyGate"
	TraceProviderGUID                      = "{e1726ef8-c2e6-4dad-bbf7-2d871b953ab1}"
	USBIPBaselineMode                      = "exact-installed-usbip-win2-runtime-and-source-bound-server"
	USBIPBaselineVersion                   = "0.9.7.7"
	USBIPRuntimeSchemaV1                   = "viiper.usbip-win2.runtime-provenance/v1"
	PackageValidationProduction            = "production"
	PackageValidationLocalTest             = "local-test"
	ScheduleOrientationABBA                = "abba"
	ScheduleOrientationBAAB                = "baab"
	MinimumProductionSamplePairs           = 256
	MaximumProductionSamplePairs           = 10_000
	ProductionWarmupPairs                  = 16
	ProductionTransportBlocks              = 2
	ProductionTransitionTimeoutNS    int64 = int64(time.Second)
	ProductionInterTransitionDelayNS int64 = 2 * int64(time.Millisecond)
	DefaultNativeMaxP95NS            int64 = 4 * int64(time.Millisecond)
	DefaultNativeMaxP99NS            int64 = 8 * int64(time.Millisecond)
	DefaultNativeMaxNS               int64 = 20 * int64(time.Millisecond)
	DefaultNativeMaxP95OverUSBIPNS   int64 = 1 * int64(time.Millisecond)
	DefaultNativeMaxP99OverUSBIPNS   int64 = 2 * int64(time.Millisecond)
	DefaultNativeMaxOverUSBIPNS      int64 = 5 * int64(time.Millisecond)
)

type Transition string

const (
	TransitionPress   Transition = "press"
	TransitionRelease Transition = "release"
)

// BlockSpec is one block in the counterbalanced ABBA transport schedule.
// Sample sequence numbers are contiguous within each transport, even though
// the two transports are interleaved in wall-clock order.
type BlockSpec struct {
	Order          int
	Transport      string
	TransportBlock int
	FirstSequence  int
	SamplePairs    int
}

// ProductionBlockSchedule preserves the original ABBA schedule for callers
// which do not yet select an orientation explicitly.
func ProductionBlockSchedule(samplePairs int) []BlockSpec {
	return ProductionBlockScheduleForOrientation(samplePairs,
		ScheduleOrientationABBA)
}

// ScheduleOrientationForCycle deterministically alternates the two balanced
// orders. An even cycle count therefore contains the same number of ABBA and
// BAAB runs, and a report cannot relabel an observed order after collection.
func ScheduleOrientationForCycle(cycleIndex int) string {
	if cycleIndex%2 == 0 {
		return ScheduleOrientationBAAB
	}
	return ScheduleOrientationABBA
}

// ProductionBlockScheduleForOrientation splits the declared samples as evenly
// as possible across two blocks per transport. ABBA and BAAB are paired in the
// production superiority matrix so first/last-run and nonlinear carryover do
// not always favor the same transport.
func ProductionBlockScheduleForOrientation(samplePairs int,
	orientation string) []BlockSpec {
	firstBlockPairs := samplePairs / ProductionTransportBlocks
	secondBlockPairs := samplePairs - firstBlockPairs
	secondFirstSequence := firstBlockPairs + 1
	first, second := TransportUSBIP, TransportNativeUDE
	if orientation == ScheduleOrientationBAAB {
		first, second = TransportNativeUDE, TransportUSBIP
	}
	return []BlockSpec{
		{Order: 1, Transport: first, TransportBlock: 1,
			FirstSequence: 1, SamplePairs: firstBlockPairs},
		{Order: 2, Transport: second, TransportBlock: 1,
			FirstSequence: 1, SamplePairs: firstBlockPairs},
		{Order: 3, Transport: second, TransportBlock: 2,
			FirstSequence: secondFirstSequence, SamplePairs: secondBlockPairs},
		{Order: 4, Transport: first, TransportBlock: 2,
			FirstSequence: secondFirstSequence, SamplePairs: secondBlockPairs},
	}
}

// ProductionPhaseSweepOffsetsNS returns a copy of the deterministic dwell
// offsets. Added to the 2 ms base dwell, the offsets span one 1 ms HID service
// interval without randomizing otherwise reproducible runs.
func ProductionPhaseSweepOffsetsNS() []int64 {
	return append([]int64(nil), productionPhaseSweepOffsetsNS[:]...)
}

// PhaseSweepScheduleSHA256 hashes the comma-separated base-10 nanosecond
// offsets. That canonical representation is recorded in every workload.
func PhaseSweepScheduleSHA256(offsets []int64) string {
	var canonical strings.Builder
	for index, offset := range offsets {
		if index != 0 {
			canonical.WriteByte(',')
		}
		canonical.WriteString(strconv.FormatInt(offset, 10))
	}
	digest := sha256.Sum256([]byte(canonical.String()))
	return fmt.Sprintf("%x", digest)
}

// ProductionPhaseOffsetNS returns the source-bound offset for an exact
// press/release sequence. Each transport resumes the same schedule in block 2.
func ProductionPhaseOffsetNS(sequence int, transition Transition) int64 {
	edgeIndex := 2 * (sequence - 1)
	if transition == TransitionRelease {
		edgeIndex++
	}
	if edgeIndex < 0 {
		return 0
	}
	return productionPhaseSweepOffsetsNS[edgeIndex%len(productionPhaseSweepOffsetsNS)]
}

type Sample struct {
	Sequence            int        `json:"sequence"`
	Transition          Transition `json:"transition"`
	LatencyNS           int64      `json:"latency_ns"`
	EventTimestampNS    uint64     `json:"sdl_event_timestamp_ns"`
	SDLFenceTimestampNS uint64     `json:"sdl_prewrite_fence_timestamp_ns"`
	StartQPCTicks       int64      `json:"start_qpc_ticks"`
	EndQPCTicks         int64      `json:"end_qpc_ticks"`
	MarkerQPCTicks      int64      `json:"trace_marker_qpc_ticks"`
	MarkerID            string     `json:"trace_marker_id"`
}

type Counters struct {
	Press   int `json:"press"`
	Release int `json:"release"`
}

func (c Counters) Total() int { return c.Press + c.Release }

type Distribution struct {
	Count    int     `json:"count"`
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

type NativeServerProof struct {
	ABIMajor                     uint16 `json:"abi_major"`
	ABIMinor                     uint16 `json:"abi_minor"`
	Capabilities                 uint32 `json:"capabilities"`
	ExpectedDriverPackageVersion string `json:"expected_driver_package_version"`
	LoadedDriverBuildIdentity    string `json:"loaded_driver_build_identity"`
}

type ServerProof struct {
	Server    string             `json:"server"`
	Version   string             `json:"version"`
	Transport string             `json:"transport"`
	Ready     bool               `json:"ready"`
	NativeUDE *NativeServerProof `json:"native_ude,omitempty"`
}

type DeviceProof struct {
	BusID     uint32 `json:"bus_id"`
	DeviceID  string `json:"device_id"`
	Type      string `json:"type"`
	VendorID  uint16 `json:"vendor_id"`
	ProductID uint16 `json:"product_id"`
	USBIPPort int32  `json:"usbip_port,omitempty"`
}

type ControllerProof struct {
	BaselineGamepadIDs        []int32    `json:"baseline_gamepad_ids"`
	NewGamepadIDs             []int32    `json:"new_gamepad_ids"`
	SDLInstanceID             int32      `json:"sdl_instance_id"`
	SDLPath                   string     `json:"sdl_path"`
	SDLGUID                   string     `json:"sdl_guid"`
	SDLName                   string     `json:"sdl_name"`
	SDLType                   string     `json:"sdl_type"`
	SDLReportedType           int32      `json:"sdl_reported_type"`
	SDLRealType               int32      `json:"sdl_real_type"`
	VendorID                  uint16     `json:"vendor_id"`
	ProductID                 uint16     `json:"product_id"`
	PNPInstanceID             string     `json:"pnp_instance_id"`
	PNPContainerID            string     `json:"pnp_container_id"`
	PNPAncestorIDs            []string   `json:"pnp_ancestor_ids"`
	PNPAncestorContainerIDs   []string   `json:"pnp_ancestor_container_ids"`
	PNPAncestorServices       []string   `json:"pnp_ancestor_services"`
	PNPAncestorHardwareIDs    [][]string `json:"pnp_ancestor_hardware_ids"`
	PNPAncestorLocationInfo   []string   `json:"pnp_ancestor_location_info"`
	PNPAncestorLocationPaths  [][]string `json:"pnp_ancestor_location_paths"`
	TransportAnchorInstanceID string     `json:"transport_anchor_instance_id"`
	TransportAnchorService    string     `json:"transport_anchor_service"`
}

type Run struct {
	Order                   int             `json:"order"`
	TransportBlock          int             `json:"transport_block"`
	FirstSequence           int             `json:"first_sequence"`
	SamplePairs             int             `json:"sample_pairs"`
	Transport               string          `json:"transport"`
	Authentication          string          `json:"authentication"`
	UnauthenticatedRejected bool            `json:"unauthenticated_rejected"`
	Server                  ServerProof     `json:"server"`
	Device                  DeviceProof     `json:"device"`
	Controller              ControllerProof `json:"controller"`
	Samples                 []Sample        `json:"samples"`
	Misses                  Counters        `json:"misses"`
	Duplicates              Counters        `json:"duplicates"`
	Statistics              DistributionSet `json:"statistics"`
	Failure                 string          `json:"failure,omitempty"`
}

type Workload struct {
	APIAddress             string  `json:"api_address"`
	USBIPAddress           string  `json:"usbip_address"`
	ControllerType         string  `json:"controller_type"`
	ExpectedVendorID       uint16  `json:"expected_vendor_id"`
	ExpectedProductID      uint16  `json:"expected_product_id"`
	ExpectedSDLType        string  `json:"expected_sdl_type"`
	Button                 string  `json:"button"`
	WarmupPairs            int     `json:"warmup_pairs"`
	SamplePairs            int     `json:"sample_pairs"`
	PerTransitionTimeoutNS int64   `json:"per_transition_timeout_ns"`
	InterTransitionDelayNS int64   `json:"inter_transition_delay_ns"`
	PhaseSweepOffsetsNS    []int64 `json:"phase_sweep_offsets_ns"`
	PhaseSweepSHA256       string  `json:"phase_sweep_sha256"`
	Authentication         string  `json:"authentication"`
	ScheduleOrientation    string  `json:"schedule_orientation"`
	CycleID                string  `json:"cycle_id"`
	CycleIndex             int     `json:"cycle_index"`
	CycleCount             int     `json:"cycle_count"`
}

type MachineProvenance struct {
	Hostname             string `json:"hostname"`
	OSProductName        string `json:"os_product_name"`
	OSDisplayVersion     string `json:"os_display_version"`
	OSVersion            string `json:"os_version"`
	CPUModel             string `json:"cpu_model"`
	LogicalProcessors    int    `json:"logical_processors"`
	ProcessPriorityClass string `json:"process_priority_class"`
	ProcessElevated      bool   `json:"process_elevated"`
}

type USBIPFileIdentity struct {
	Path             string `json:"path"`
	Length           int64  `json:"length"`
	SHA256           string `json:"sha256"`
	FileVersion      string `json:"file_version,omitempty"`
	ProductVersion   string `json:"product_version,omitempty"`
	SignatureStatus  string `json:"signature_status,omitempty"`
	SignerSubject    string `json:"signer_subject,omitempty"`
	SignerThumbprint string `json:"signer_thumbprint,omitempty"`
}

type USBIPServiceProvenance struct {
	Name             string            `json:"name"`
	Start            uint32            `json:"start"`
	Type             uint32            `json:"type"`
	PublishedINFName string            `json:"published_inf_name"`
	Image            USBIPFileIdentity `json:"image"`
	INF              USBIPFileIdentity `json:"inf"`
	PublishedINF     USBIPFileIdentity `json:"published_inf"`
	Catalog          USBIPFileIdentity `json:"catalog"`
}

type USBIPRootControllerProvenance struct {
	InstanceID    string   `json:"instance_id"`
	HardwareIDs   []string `json:"hardware_ids"`
	Service       string   `json:"service"`
	Provider      string   `json:"provider"`
	DriverVersion string   `json:"driver_version"`
	PublishedINF  string   `json:"published_inf"`
	Signer        string   `json:"signer"`
	IsSigned      bool     `json:"is_signed"`
}

type USBIPRuntimeProvenance struct {
	Schema          string                          `json:"schema"`
	CaptureSHA256   string                          `json:"capture_sha256,omitempty"`
	CaptureBase64   string                          `json:"capture_base64,omitempty"`
	Services        []USBIPServiceProvenance        `json:"services"`
	RootControllers []USBIPRootControllerProvenance `json:"root_controllers"`
}

type Provenance struct {
	SourceRevision                   string                 `json:"source_revision"`
	SDLSourceRevision                string                 `json:"sdl_source_revision"`
	SDLBinaryPath                    string                 `json:"sdl_binary_path"`
	SDLBinarySHA256                  string                 `json:"sdl_binary_sha256"`
	NativePackageManifestSHA256      string                 `json:"native_package_manifest_sha256"`
	NativePackageValidationMode      string                 `json:"native_package_validation_mode"`
	NativeLocalTestCertificateSHA256 string                 `json:"native_local_test_certificate_sha256,omitempty"`
	NativeDriverSHA256               string                 `json:"native_driver_sha256"`
	NativeDriverBuildIdentity        string                 `json:"native_driver_build_identity"`
	QPCFrequency                     int64                  `json:"qpc_frequency"`
	TraceProviderName                string                 `json:"trace_provider_name"`
	TraceProviderGUID                string                 `json:"trace_provider_guid"`
	TraceProfileSHA256               string                 `json:"trace_profile_sha256"`
	USBIPBaselineMode                string                 `json:"usbip_baseline_mode"`
	USBIPBaselineVersion             string                 `json:"usbip_baseline_version"`
	USBIPRuntime                     USBIPRuntimeProvenance `json:"usbip_runtime"`
	GoVersion                        string                 `json:"go_version"`
	GOOS                             string                 `json:"goos"`
	GOARCH                           string                 `json:"goarch"`
	GitExecutablePath                string                 `json:"git_executable_path"`
	GitExecutableSHA256              string                 `json:"git_executable_sha256"`
	GoExecutablePath                 string                 `json:"go_executable_path"`
	GoExecutableSHA256               string                 `json:"go_executable_sha256"`
	WPRExecutablePath                string                 `json:"wpr_executable_path"`
	WPRExecutableSHA256              string                 `json:"wpr_executable_sha256"`
	Machine                          MachineProvenance      `json:"machine"`
}

// SampleMarkerID is the canonical cross-artifact identity shared by JSON and
// the TraceLogging marker emitted after the SDL edge is observed.
func SampleMarkerID(cycleID string, cycleIndex int, controller, transport string,
	block, sequence int, transition Transition) string {
	return fmt.Sprintf("%s:%d:%s:%s:%d:%d:%s", cycleID, cycleIndex,
		controller, transport, block, sequence, transition)
}

// QPCIntervalNS converts a bounded raw QueryPerformanceCounter interval to
// nanoseconds. Production samples are shorter than the per-transition timeout,
// so rejecting rather than saturating on multiplication overflow is safe and
// prevents a forged or corrupted interval from becoming a plausible latency.
func QPCIntervalNS(start, end, frequency int64) (int64, error) {
	if start <= 0 || end <= start || frequency <= 0 {
		return 0, errors.New("QPC interval or frequency is invalid")
	}
	delta := end - start
	if delta > math.MaxInt64/int64(time.Second) {
		return 0, errors.New("QPC interval overflows nanosecond conversion")
	}
	nanoseconds := delta * int64(time.Second) / frequency
	if nanoseconds <= 0 {
		return 0, errors.New("QPC interval has sub-nanosecond or non-positive duration")
	}
	return nanoseconds, nil
}

type Policy struct {
	MinimumSamplePairs      int   `json:"minimum_sample_pairs"`
	NativeMaxP95NS          int64 `json:"native_max_p95_ns"`
	NativeMaxP99NS          int64 `json:"native_max_p99_ns"`
	NativeMaxNS             int64 `json:"native_max_ns"`
	NativeMaxP95OverUSBIPNS int64 `json:"native_max_p95_over_usbip_ns"`
	NativeMaxP99OverUSBIPNS int64 `json:"native_max_p99_over_usbip_ns"`
	NativeMaxOverUSBIPNS    int64 `json:"native_max_over_usbip_ns"`
}

type TransportAggregate struct {
	Transport  string          `json:"transport"`
	BlockCount int             `json:"block_count"`
	Misses     Counters        `json:"misses"`
	Duplicates Counters        `json:"duplicates"`
	Statistics DistributionSet `json:"statistics"`
}

type MetricComparison struct {
	USBIP              float64  `json:"usbip"`
	NativeUDE          float64  `json:"native_ude"`
	NativeMinusUSBIP   float64  `json:"native_minus_usbip"`
	NativeToUSBIPRatio *float64 `json:"native_to_usbip_ratio,omitempty"`
}

type DistributionComparison struct {
	P50    MetricComparison `json:"p50_ns"`
	P90    MetricComparison `json:"p90_ns"`
	P95    MetricComparison `json:"p95_ns"`
	P99    MetricComparison `json:"p99_ns"`
	P999   MetricComparison `json:"p99_9_ns"`
	Max    MetricComparison `json:"max_ns"`
	Jitter MetricComparison `json:"jitter_ns"`
}

type ComparisonSet struct {
	Press    DistributionComparison `json:"press"`
	Release  DistributionComparison `json:"release"`
	Combined DistributionComparison `json:"combined"`
}

type Report struct {
	Schema      string               `json:"schema"`
	GeneratedAt time.Time            `json:"generated_at"`
	Provenance  Provenance           `json:"provenance"`
	Workload    Workload             `json:"workload"`
	Policy      Policy               `json:"policy"`
	Runs        []Run                `json:"runs"`
	Transports  []TransportAggregate `json:"transports"`
	Comparison  ComparisonSet        `json:"comparison"`
	Verdict     string               `json:"verdict"`
	Failures    []string             `json:"failures"`
}

type SuiteReport struct {
	Schema      string     `json:"schema"`
	GeneratedAt time.Time  `json:"generated_at"`
	Provenance  Provenance `json:"provenance"`
	Cases       []Report   `json:"cases"`
	Verdict     string     `json:"verdict"`
	Failures    []string   `json:"failures"`
}

var (
	revisionPattern     = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	hashPattern         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	thumbprintPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	cycleIDPattern      = regexp.MustCompile(`^[0-9a-f]{32}$`)
	publishedINFPattern = regexp.MustCompile(`^oem[0-9]+\.inf$`)
	containerPattern    = regexp.MustCompile(
		`(?i)^\{[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\}$`)
)

// Calculate returns nearest-rank percentiles and population standard deviation.
// The input order is retained so callers can keep the original per-sample record.
func Calculate(values []int64) (Distribution, error) {
	if len(values) == 0 {
		return Distribution{}, errors.New("cannot summarize zero latency samples")
	}
	ordered := append([]int64(nil), values...)
	for index, value := range ordered {
		if value <= 0 {
			return Distribution{}, fmt.Errorf("latency sample %d must be positive, got %d", index, value)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })

	mean := 0.0
	m2 := 0.0
	for index, value := range values {
		x := float64(value)
		delta := x - mean
		mean += delta / float64(index+1)
		m2 += delta * (x - mean)
	}

	return Distribution{
		Count:    len(ordered),
		P50NS:    nearestRank(ordered, 0.50),
		P90NS:    nearestRank(ordered, 0.90),
		P95NS:    nearestRank(ordered, 0.95),
		P99NS:    nearestRank(ordered, 0.99),
		P999NS:   nearestRank(ordered, 0.999),
		MaxNS:    ordered[len(ordered)-1],
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

// Finalize recomputes every derived field and evaluates the fail-closed policy.
func Finalize(report *Report) error {
	if report == nil {
		return errors.New("nil latency report")
	}
	if err := validateBase(report); err != nil {
		return err
	}

	report.Failures = nil
	report.Comparison = ComparisonSet{}
	report.Transports = nil
	for index := range report.Runs {
		run := &report.Runs[index]
		run.Statistics = summarizeSamples(run.Samples)
		if run.Failure != "" {
			report.Failures = append(report.Failures,
				fmt.Sprintf("%s block %d failed: %s", run.Transport, run.TransportBlock, run.Failure))
		}
		if run.Misses.Total() != 0 {
			report.Failures = append(report.Failures,
				fmt.Sprintf("%s block %d observed %d missed transitions (press=%d release=%d)",
					run.Transport, run.TransportBlock, run.Misses.Total(),
					run.Misses.Press, run.Misses.Release))
		}
		if run.Duplicates.Total() != 0 {
			report.Failures = append(report.Failures,
				fmt.Sprintf("%s block %d observed %d duplicate transitions (press=%d release=%d)",
					run.Transport, run.TransportBlock, run.Duplicates.Total(),
					run.Duplicates.Press, run.Duplicates.Release))
		}
	}

	for _, transport := range []string{TransportUSBIP, TransportNativeUDE} {
		aggregate := aggregateTransport(report.Runs, transport)
		report.Transports = append(report.Transports, aggregate)
		if aggregate.Statistics.Press.Count < report.Policy.MinimumSamplePairs {
			report.Failures = append(report.Failures,
				fmt.Sprintf("%s has %d/%d required press samples across its counterbalanced blocks",
					transport, aggregate.Statistics.Press.Count, report.Policy.MinimumSamplePairs))
		}
		if aggregate.Statistics.Release.Count < report.Policy.MinimumSamplePairs {
			report.Failures = append(report.Failures,
				fmt.Sprintf("%s has %d/%d required release samples across its counterbalanced blocks",
					transport, aggregate.Statistics.Release.Count, report.Policy.MinimumSamplePairs))
		}
	}

	usbip := aggregateForTransport(report.Transports, TransportUSBIP)
	native := aggregateForTransport(report.Transports, TransportNativeUDE)
	if usbip != nil && native != nil &&
		usbip.Statistics.Press.Count != 0 && native.Statistics.Press.Count != 0 &&
		usbip.Statistics.Release.Count != 0 && native.Statistics.Release.Count != 0 {
		report.Comparison = compareSets(usbip.Statistics, native.Statistics)
		checkNativeLimits(report, "press", native.Statistics.Press)
		checkNativeLimits(report, "release", native.Statistics.Release)
		checkNativeLimits(report, "combined", native.Statistics.Combined)
		checkNativeNonRegression(report, "press", usbip.Statistics.Press, native.Statistics.Press)
		checkNativeNonRegression(report, "release", usbip.Statistics.Release, native.Statistics.Release)
		checkNativeNonRegression(report, "combined", usbip.Statistics.Combined, native.Statistics.Combined)
	}

	if len(report.Failures) == 0 {
		report.Verdict = "pass"
	} else {
		report.Verdict = "fail"
	}
	return nil
}

func aggregateTransport(runs []Run, transport string) TransportAggregate {
	aggregate := TransportAggregate{Transport: transport}
	var samples []Sample
	for index := range runs {
		run := &runs[index]
		if run.Transport != transport {
			continue
		}
		aggregate.BlockCount++
		aggregate.Misses.Press += run.Misses.Press
		aggregate.Misses.Release += run.Misses.Release
		aggregate.Duplicates.Press += run.Duplicates.Press
		aggregate.Duplicates.Release += run.Duplicates.Release
		samples = append(samples, run.Samples...)
	}
	aggregate.Statistics = summarizeSamples(samples)
	return aggregate
}

func aggregateForTransport(aggregates []TransportAggregate, transport string) *TransportAggregate {
	for index := range aggregates {
		if aggregates[index].Transport == transport {
			return &aggregates[index]
		}
	}
	return nil
}

func summarizeSamples(samples []Sample) DistributionSet {
	press := make([]int64, 0, len(samples)/2)
	release := make([]int64, 0, len(samples)/2)
	combined := make([]int64, 0, len(samples))
	for _, sample := range samples {
		combined = append(combined, sample.LatencyNS)
		switch sample.Transition {
		case TransitionPress:
			press = append(press, sample.LatencyNS)
		case TransitionRelease:
			release = append(release, sample.LatencyNS)
		}
	}
	var result DistributionSet
	if len(press) != 0 {
		result.Press, _ = Calculate(press)
	}
	if len(release) != 0 {
		result.Release, _ = Calculate(release)
	}
	if len(combined) != 0 {
		result.Combined, _ = Calculate(combined)
	}
	return result
}

func checkNativeLimits(report *Report, name string, distribution Distribution) {
	if distribution.Count == 0 {
		return
	}
	if distribution.P95NS > report.Policy.NativeMaxP95NS {
		report.Failures = append(report.Failures, fmt.Sprintf(
			"native-ude %s p95 %dns exceeds %dns", name,
			distribution.P95NS, report.Policy.NativeMaxP95NS))
	}
	if distribution.P99NS > report.Policy.NativeMaxP99NS {
		report.Failures = append(report.Failures, fmt.Sprintf(
			"native-ude %s p99 %dns exceeds %dns", name,
			distribution.P99NS, report.Policy.NativeMaxP99NS))
	}
	if distribution.MaxNS > report.Policy.NativeMaxNS {
		report.Failures = append(report.Failures, fmt.Sprintf(
			"native-ude %s max %dns exceeds %dns", name,
			distribution.MaxNS, report.Policy.NativeMaxNS))
	}
}

func checkNativeNonRegression(report *Report, name string, usbip, native Distribution) {
	if native.P95NS-usbip.P95NS > report.Policy.NativeMaxP95OverUSBIPNS {
		report.Failures = append(report.Failures, fmt.Sprintf(
			"native-ude %s p95 exceeds same-machine USB/IP by %dns (allowed %dns)",
			name, native.P95NS-usbip.P95NS, report.Policy.NativeMaxP95OverUSBIPNS))
	}
	if native.P99NS-usbip.P99NS > report.Policy.NativeMaxP99OverUSBIPNS {
		report.Failures = append(report.Failures, fmt.Sprintf(
			"native-ude %s p99 exceeds same-machine USB/IP by %dns (allowed %dns)",
			name, native.P99NS-usbip.P99NS, report.Policy.NativeMaxP99OverUSBIPNS))
	}
	if native.MaxNS-usbip.MaxNS > report.Policy.NativeMaxOverUSBIPNS {
		report.Failures = append(report.Failures, fmt.Sprintf(
			"native-ude %s max exceeds same-machine USB/IP by %dns (allowed %dns)",
			name, native.MaxNS-usbip.MaxNS, report.Policy.NativeMaxOverUSBIPNS))
	}
}

func compareSets(usbip, native DistributionSet) ComparisonSet {
	return ComparisonSet{
		Press:    compareDistribution(usbip.Press, native.Press),
		Release:  compareDistribution(usbip.Release, native.Release),
		Combined: compareDistribution(usbip.Combined, native.Combined),
	}
}

func compareDistribution(usbip, native Distribution) DistributionComparison {
	return DistributionComparison{
		P50:    compareMetric(float64(usbip.P50NS), float64(native.P50NS)),
		P90:    compareMetric(float64(usbip.P90NS), float64(native.P90NS)),
		P95:    compareMetric(float64(usbip.P95NS), float64(native.P95NS)),
		P99:    compareMetric(float64(usbip.P99NS), float64(native.P99NS)),
		P999:   compareMetric(float64(usbip.P999NS), float64(native.P999NS)),
		Max:    compareMetric(float64(usbip.MaxNS), float64(native.MaxNS)),
		Jitter: compareMetric(usbip.JitterNS, native.JitterNS),
	}
}

func compareMetric(usbip, native float64) MetricComparison {
	comparison := MetricComparison{
		USBIP:            usbip,
		NativeUDE:        native,
		NativeMinusUSBIP: native - usbip,
	}
	if usbip != 0 {
		ratio := native / usbip
		comparison.NativeToUSBIPRatio = &ratio
	}
	return comparison
}

func validateBase(report *Report) error {
	if report.Schema != SchemaV3 {
		return fmt.Errorf("unsupported report schema %q", report.Schema)
	}
	if report.GeneratedAt.IsZero() {
		return errors.New("generated_at is required")
	}
	if !revisionPattern.MatchString(report.Provenance.SourceRevision) {
		return errors.New("source_revision must be a lowercase 40- or 64-digit Git revision")
	}
	if !revisionPattern.MatchString(report.Provenance.SDLSourceRevision) {
		return errors.New("sdl_source_revision must be a lowercase 40- or 64-digit Git revision")
	}
	if report.Provenance.SDLBinaryPath == "" ||
		!hashPattern.MatchString(report.Provenance.SDLBinarySHA256) {
		return errors.New("the loaded SDL binary path and SHA-256 are required")
	}
	if !hashPattern.MatchString(report.Provenance.NativePackageManifestSHA256) ||
		!hashPattern.MatchString(report.Provenance.NativeDriverSHA256) ||
		!hashPattern.MatchString(report.Provenance.NativeDriverBuildIdentity) {
		return errors.New("source-bound native package manifest and installed driver hashes are required")
	}
	if (report.Provenance.NativePackageValidationMode != PackageValidationProduction ||
		report.Provenance.NativeLocalTestCertificateSHA256 != "") &&
		(report.Provenance.NativePackageValidationMode != PackageValidationLocalTest ||
			!hashPattern.MatchString(report.Provenance.NativeLocalTestCertificateSHA256)) {
		return errors.New("native package validation mode or local-test certificate identity is invalid")
	}
	if report.Provenance.QPCFrequency <= 0 ||
		report.Provenance.TraceProviderName != TraceProviderName ||
		report.Provenance.TraceProviderGUID != TraceProviderGUID ||
		!hashPattern.MatchString(report.Provenance.TraceProfileSHA256) {
		return errors.New("QPC and source-controlled TraceLogging provenance are incomplete")
	}
	if report.Provenance.USBIPBaselineMode != USBIPBaselineMode ||
		report.Provenance.USBIPBaselineVersion != USBIPBaselineVersion {
		return errors.New("USB/IP comparison is not the exact installed comparator")
	}
	if err := ValidateUSBIPRuntimeProvenance(report.Provenance.USBIPRuntime); err != nil {
		return err
	}
	if report.Provenance.GoVersion == "" || report.Provenance.GOOS != "windows" ||
		report.Provenance.GOARCH == "" {
		return errors.New("Windows Go toolchain provenance is incomplete")
	}
	if report.Provenance.GitExecutablePath == "" ||
		!hashPattern.MatchString(report.Provenance.GitExecutableSHA256) ||
		report.Provenance.GoExecutablePath == "" ||
		!hashPattern.MatchString(report.Provenance.GoExecutableSHA256) ||
		report.Provenance.WPRExecutablePath == "" ||
		!hashPattern.MatchString(report.Provenance.WPRExecutableSHA256) {
		return errors.New("Git, Go, and WPR executable provenance is incomplete")
	}
	machine := report.Provenance.Machine
	if machine.Hostname == "" || machine.OSProductName == "" ||
		machine.OSDisplayVersion == "" || machine.OSVersion == "" ||
		machine.CPUModel == "" || machine.LogicalProcessors <= 0 ||
		(machine.ProcessPriorityClass != "normal" &&
			machine.ProcessPriorityClass != "high") || !machine.ProcessElevated {
		return errors.New("machine, OS, CPU, elevation, and process-priority provenance are incomplete")
	}
	if report.Workload.APIAddress == "" || report.Workload.USBIPAddress == "" ||
		report.Workload.Button != "south/A" ||
		report.Workload.Authentication != AuthenticationMode {
		return errors.New("workload identity is incomplete or unsupported")
	}
	if (report.Workload.ScheduleOrientation != ScheduleOrientationABBA &&
		report.Workload.ScheduleOrientation != ScheduleOrientationBAAB) ||
		!cycleIDPattern.MatchString(report.Workload.CycleID) ||
		report.Workload.CycleCount < 2 || report.Workload.CycleCount%2 != 0 ||
		report.Workload.CycleIndex < 1 ||
		report.Workload.CycleIndex > report.Workload.CycleCount ||
		report.Workload.ScheduleOrientation !=
			ScheduleOrientationForCycle(report.Workload.CycleIndex) {
		return errors.New("balanced schedule orientation and canonical cycle identity are required")
	}
	if err := validateControllerWorkload(report.Workload); err != nil {
		return err
	}
	if report.Workload.WarmupPairs != ProductionWarmupPairs ||
		report.Workload.SamplePairs < MinimumProductionSamplePairs ||
		report.Workload.SamplePairs > MaximumProductionSamplePairs ||
		report.Workload.PerTransitionTimeoutNS != ProductionTransitionTimeoutNS ||
		report.Workload.InterTransitionDelayNS != ProductionInterTransitionDelayNS {
		return errors.New("workload warmup, sample count, timeout, or transition delay is invalid")
	}
	productionOffsets := ProductionPhaseSweepOffsetsNS()
	if !reflect.DeepEqual(report.Workload.PhaseSweepOffsetsNS, productionOffsets) ||
		report.Workload.PhaseSweepSHA256 != PhaseSweepScheduleSHA256(report.Workload.PhaseSweepOffsetsNS) {
		return errors.New("workload phase-sweep schedule or SHA-256 is not the reviewed production schedule")
	}
	if report.Policy.MinimumSamplePairs < MinimumProductionSamplePairs ||
		report.Policy.MinimumSamplePairs > report.Workload.SamplePairs {
		return errors.New("minimum sample policy is weaker than the production floor or exceeds the workload")
	}
	if report.Policy.NativeMaxP95NS <= 0 ||
		report.Policy.NativeMaxP95NS > DefaultNativeMaxP95NS ||
		report.Policy.NativeMaxP99NS <= 0 ||
		report.Policy.NativeMaxP99NS > DefaultNativeMaxP99NS ||
		report.Policy.NativeMaxNS <= 0 ||
		report.Policy.NativeMaxNS > DefaultNativeMaxNS ||
		report.Policy.NativeMaxP95OverUSBIPNS <= 0 ||
		report.Policy.NativeMaxP95OverUSBIPNS > DefaultNativeMaxP95OverUSBIPNS ||
		report.Policy.NativeMaxP99OverUSBIPNS <= 0 ||
		report.Policy.NativeMaxP99OverUSBIPNS > DefaultNativeMaxP99OverUSBIPNS ||
		report.Policy.NativeMaxOverUSBIPNS <= 0 ||
		report.Policy.NativeMaxOverUSBIPNS > DefaultNativeMaxOverUSBIPNS {
		return errors.New("native latency policy is absent or weaker than the reviewed release limits")
	}
	schedule := ProductionBlockScheduleForOrientation(
		report.Workload.SamplePairs, report.Workload.ScheduleOrientation)
	if len(report.Runs) != len(schedule) {
		return errors.New("report must contain exactly four balanced transport blocks")
	}

	for index := range report.Runs {
		if err := validateRun(&report.Runs[index], report.Workload, report.Provenance, schedule[index]); err != nil {
			return fmt.Errorf("order %d %s block %d: %w", index+1,
				report.Runs[index].Transport, report.Runs[index].TransportBlock, err)
		}
	}
	serverVersion := ""
	var priorEventTimestamp uint64
	for index := range report.Runs {
		run := &report.Runs[index]
		if len(run.Samples) != 0 {
			firstTimestamp := run.Samples[0].EventTimestampNS
			if priorEventTimestamp != 0 && firstTimestamp < priorEventTimestamp {
				return errors.New("SDL event clock regressed between balanced transport blocks")
			}
			priorEventTimestamp = run.Samples[len(run.Samples)-1].EventTimestampNS
		}
		if run.Failure != "" {
			continue
		}
		if serverVersion == "" {
			serverVersion = run.Server.Version
		} else if run.Server.Version != serverVersion {
			return errors.New("transport blocks came from different VIIPER server versions")
		}
	}
	return nil
}

func validateRun(run *Run, workload Workload, provenance Provenance, block BlockSpec) error {
	if run.Order != block.Order || run.Transport != block.Transport ||
		run.TransportBlock != block.TransportBlock ||
		run.FirstSequence != block.FirstSequence || run.SamplePairs != block.SamplePairs {
		return fmt.Errorf("block metadata does not match the production balanced schedule: %+v", block)
	}
	if run.Authentication != AuthenticationMode {
		return errors.New("API/controller stream is not authenticated identically")
	}
	if run.Misses.Press < 0 || run.Misses.Release < 0 ||
		run.Duplicates.Press < 0 || run.Duplicates.Release < 0 {
		return errors.New("negative integrity counter")
	}
	if len(run.Samples) > 2*run.SamplePairs {
		return errors.New("more samples than the declared transport block")
	}
	var priorTimestamp uint64
	var priorMarkerQPC int64
	for index, sample := range run.Samples {
		wantSequence := run.FirstSequence + index/2
		wantTransition := TransitionPress
		if index%2 != 0 {
			wantTransition = TransitionRelease
		}
		if sample.Sequence != wantSequence || sample.Transition != wantTransition {
			return fmt.Errorf("sample %d is %d/%s, want %d/%s", index,
				sample.Sequence, sample.Transition, wantSequence, wantTransition)
		}
		wantMarkerID := SampleMarkerID(workload.CycleID, workload.CycleIndex,
			workload.ControllerType, run.Transport, run.TransportBlock,
			sample.Sequence, sample.Transition)
		qpcLatencyNS, qpcErr := QPCIntervalNS(sample.StartQPCTicks,
			sample.EndQPCTicks, provenance.QPCFrequency)
		if qpcErr != nil || sample.LatencyNS != qpcLatencyNS || sample.EventTimestampNS == 0 ||
			sample.SDLFenceTimestampNS == 0 ||
			sample.EventTimestampNS <= sample.SDLFenceTimestampNS ||
			(priorMarkerQPC != 0 && sample.StartQPCTicks < priorMarkerQPC) ||
			sample.MarkerQPCTicks < sample.EndQPCTicks ||
			sample.MarkerID != wantMarkerID {
			return fmt.Errorf("sample %d has invalid or inconsistent latency, causal fence, QPC interval, or trace marker", index)
		}
		if priorTimestamp != 0 && sample.EventTimestampNS < priorTimestamp {
			return fmt.Errorf("sample %d regressed the SDL event clock", index)
		}
		priorTimestamp = sample.EventTimestampNS
		priorMarkerQPC = sample.MarkerQPCTicks
	}
	if run.Failure == "" && len(run.Samples) != 2*run.SamplePairs {
		return errors.New("successful run does not contain every press/release sample")
	}
	if run.Failure != "" {
		return nil
	}
	if !run.UnauthenticatedRejected {
		return errors.New("unauthenticated API probe was not rejected")
	}
	if run.Server.Server != "VIIPER" || run.Server.Transport != run.Transport ||
		!run.Server.Ready || run.Server.Version == "" {
		return errors.New("authenticated ping does not prove the requested live transport")
	}
	if run.Device.BusID != 1 || run.Device.DeviceID != "1" ||
		run.Device.Type != workload.ControllerType ||
		run.Device.VendorID != workload.ExpectedVendorID ||
		run.Device.ProductID != workload.ExpectedProductID {
		return errors.New("API device proof does not identify the exact controller workload")
	}
	if run.Transport == TransportNativeUDE {
		if run.Server.NativeUDE == nil || run.Server.NativeUDE.ABIMajor == 0 ||
			run.Server.NativeUDE.ExpectedDriverPackageVersion == "" ||
			run.Server.NativeUDE.LoadedDriverBuildIdentity == "" || run.Device.USBIPPort != 0 {
			return errors.New("native transport proof is absent or contradictory")
		}
		if run.Server.NativeUDE.LoadedDriverBuildIdentity != provenance.NativeDriverBuildIdentity {
			return errors.New("loaded native driver build identity does not match the signed package manifest")
		}
	} else if run.Transport == TransportUSBIP {
		if run.Server.NativeUDE != nil || run.Device.USBIPPort <= 0 {
			return errors.New("USB/IP transport proof is absent or contradictory")
		}
	} else {
		return fmt.Errorf("unsupported transport %q", run.Transport)
	}
	if len(run.Controller.NewGamepadIDs) != 1 ||
		run.Controller.NewGamepadIDs[0] != run.Controller.SDLInstanceID {
		return errors.New("SDL observer is not bound to exactly one newly enumerated gamepad")
	}
	for _, baselineID := range run.Controller.BaselineGamepadIDs {
		if baselineID == run.Controller.SDLInstanceID {
			return errors.New("SDL observer selected a gamepad that existed before DeviceAdd")
		}
	}
	if run.Controller.SDLInstanceID == 0 || run.Controller.SDLPath == "" ||
		run.Controller.SDLGUID == "" || run.Controller.SDLName == "" ||
		run.Controller.SDLType != workload.ExpectedSDLType ||
		run.Controller.SDLRealType != expectedSDLRealType(workload.ControllerType) ||
		run.Controller.VendorID != workload.ExpectedVendorID ||
		run.Controller.ProductID != workload.ExpectedProductID {
		return errors.New("new SDL gamepad identity does not match the API-created controller")
	}
	if err := ValidateTransportAncestry(run.Transport, run.Device.USBIPPort, run.Controller); err != nil {
		return err
	}
	return nil
}

// ValidateTransportAncestry rejects VID/PID-only substitutions and requires
// exactly one transport-specific root anchor in the SDL interface's PnP chain.
func ValidateTransportAncestry(transport string, usbipPort int32, proof ControllerProof) error {
	if proof.PNPInstanceID == "" || !containerPattern.MatchString(proof.PNPContainerID) ||
		len(proof.PNPAncestorIDs) == 0 ||
		len(proof.PNPAncestorIDs) != len(proof.PNPAncestorServices) ||
		len(proof.PNPAncestorIDs) != len(proof.PNPAncestorContainerIDs) ||
		len(proof.PNPAncestorIDs) != len(proof.PNPAncestorHardwareIDs) ||
		len(proof.PNPAncestorIDs) != len(proof.PNPAncestorLocationInfo) ||
		len(proof.PNPAncestorIDs) != len(proof.PNPAncestorLocationPaths) ||
		!strings.EqualFold(proof.PNPAncestorIDs[0], proof.PNPInstanceID) ||
		!strings.EqualFold(proof.PNPAncestorContainerIDs[0], proof.PNPContainerID) {
		return errors.New("SDL observer lacks an exact, internally consistent Windows PnP ancestry proof")
	}
	anchorCount := 0
	for index, instanceID := range proof.PNPAncestorIDs {
		containerID := proof.PNPAncestorContainerIDs[index]
		if containerID != "" && !containerPattern.MatchString(containerID) {
			return fmt.Errorf("PnP ancestor %q has malformed container identity %q", instanceID, containerID)
		}
		service := proof.PNPAncestorServices[index]
		hardwareIDs := proof.PNPAncestorHardwareIDs[index]
		isAnchor := false
		switch transport {
		case TransportNativeUDE:
			isAnchor = strings.EqualFold(service, "ViiperUde") &&
				containsFold(hardwareIDs, `ROOT\VIIPER\UDE`)
		case TransportUSBIP:
			// Root-enumerated devnode instance IDs are OS-assigned (for example
			// ROOT\USB\0002). The stable INF identity is the exact hardware ID.
			isAnchor = strings.EqualFold(service, "usbip2_ude") &&
				containsFold(hardwareIDs, `ROOT\USBIP_WIN2\UDE`)
		default:
			return fmt.Errorf("unsupported transport %q", transport)
		}
		if isAnchor {
			anchorCount++
			if !strings.EqualFold(proof.TransportAnchorInstanceID, instanceID) ||
				!strings.EqualFold(proof.TransportAnchorService, service) {
				return errors.New("reported transport anchor does not match the exact PnP ancestor")
			}
		}
	}
	if anchorCount != 1 {
		return fmt.Errorf("PnP ancestry contains %d exact %s transport anchors, want 1", anchorCount, transport)
	}
	if transport == TransportUSBIP {
		if usbipPort <= 0 {
			return errors.New("USB/IP transport has no positive import-port identity")
		}
		portSegment := fmt.Sprintf("USB(%d)", usbipPort)
		portMatched := false
		for index, instanceID := range proof.PNPAncestorIDs {
			if strings.EqualFold(instanceID, proof.TransportAnchorInstanceID) {
				break
			}
			for _, locationPath := range proof.PNPAncestorLocationPaths[index] {
				for _, segment := range strings.Split(locationPath, "#") {
					if strings.EqualFold(segment, portSegment) {
						portMatched = true
					}
				}
			}
		}
		if !portMatched {
			return fmt.Errorf("USB/IP PnP descendants do not prove returned root-hub port %d", usbipPort)
		}
	}
	return nil
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func ValidateUSBIPRuntimeProvenance(proof USBIPRuntimeProvenance) error {
	if proof.Schema != USBIPRuntimeSchemaV1 || !hashPattern.MatchString(proof.CaptureSHA256) {
		return errors.New("USB/IP runtime provenance schema or capture hash is invalid")
	}
	rawCapture, err := base64.StdEncoding.DecodeString(proof.CaptureBase64)
	if err != nil || len(rawCapture) == 0 || len(rawCapture) > 64*1024 ||
		base64.StdEncoding.EncodeToString(rawCapture) != proof.CaptureBase64 {
		return errors.New("USB/IP runtime provenance has no canonical bounded raw capture")
	}
	digest := sha256.Sum256(rawCapture)
	if fmt.Sprintf("%x", digest[:]) != proof.CaptureSHA256 {
		return errors.New("USB/IP runtime raw capture does not match its SHA-256")
	}
	decoder := json.NewDecoder(bytes.NewReader(rawCapture))
	decoder.DisallowUnknownFields()
	var captured USBIPRuntimeProvenance
	if err = decoder.Decode(&captured); err != nil {
		return fmt.Errorf("decode USB/IP raw capture: %w", err)
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("USB/IP raw capture contains trailing JSON")
	}
	if captured.CaptureSHA256 != "" || captured.CaptureBase64 != "" ||
		captured.Schema != proof.Schema ||
		!reflect.DeepEqual(captured.Services, proof.Services) ||
		!reflect.DeepEqual(captured.RootControllers, proof.RootControllers) {
		return errors.New("USB/IP structured provenance does not exactly match its raw capture")
	}
	expectedServices := []string{"usbip2_filter", "usbip2_ude"}
	if len(proof.Services) != len(expectedServices) {
		return fmt.Errorf("USB/IP runtime has %d exact driver services, want %d",
			len(proof.Services), len(expectedServices))
	}
	servicePublishedINF := make(map[string]string, len(proof.Services))
	for index, service := range proof.Services {
		if service.Name != expectedServices[index] || service.Type != 1 || service.Start > 4 ||
			!publishedINFPattern.MatchString(service.PublishedINFName) {
			return fmt.Errorf("USB/IP service %d registry identity is invalid", index)
		}
		if err := validateUSBIPFileIdentity(service.Image, true); err != nil {
			return fmt.Errorf("USB/IP %s image: %w", service.Name, err)
		}
		if err := validateUSBIPFileIdentity(service.Catalog, true); err != nil {
			return fmt.Errorf("USB/IP %s catalog: %w", service.Name, err)
		}
		if err := validateUSBIPFileIdentity(service.INF, false); err != nil {
			return fmt.Errorf("USB/IP %s INF: %w", service.Name, err)
		}
		if err := validateUSBIPFileIdentity(service.PublishedINF, false); err != nil {
			return fmt.Errorf("USB/IP %s published INF: %w", service.Name, err)
		}
		if service.INF.SHA256 != service.PublishedINF.SHA256 ||
			service.Image.SignerThumbprint != service.Catalog.SignerThumbprint ||
			!strings.Contains(strings.ToLower(service.Image.SignerSubject), "microsoft") ||
			!strings.Contains(strings.ToLower(service.Catalog.SignerSubject), "microsoft") {
			return fmt.Errorf("USB/IP %s package bytes or Microsoft signature identity disagree", service.Name)
		}
		servicePublishedINF[service.Name] = service.PublishedINFName
	}
	if len(proof.RootControllers) < 1 || len(proof.RootControllers) > 16 {
		return fmt.Errorf("USB/IP runtime has %d root controllers", len(proof.RootControllers))
	}
	seenRoots := make(map[string]bool, len(proof.RootControllers))
	priorInstance := ""
	for index, controller := range proof.RootControllers {
		canonicalInstance := strings.ToUpper(controller.InstanceID)
		if canonicalInstance == "" || seenRoots[canonicalInstance] ||
			(index != 0 && canonicalInstance <= priorInstance) ||
			!strings.HasPrefix(canonicalInstance, `ROOT\USB\`) ||
			!containsFold(controller.HardwareIDs, `ROOT\USBIP_WIN2\UDE`) ||
			!strings.EqualFold(controller.Service, "usbip2_ude") ||
			!strings.EqualFold(controller.Provider, "USBIP-WIN2") ||
			controller.DriverVersion == "" ||
			!strings.EqualFold(controller.PublishedINF, servicePublishedINF["usbip2_ude"]) ||
			!controller.IsSigned || !strings.Contains(strings.ToLower(controller.Signer), "microsoft") {
			return fmt.Errorf("USB/IP root controller %d identity is invalid or ambiguous", index)
		}
		seenRoots[canonicalInstance] = true
		priorInstance = canonicalInstance
	}
	return nil
}

func validateUSBIPFileIdentity(identity USBIPFileIdentity, signed bool) error {
	if identity.Path == "" || identity.Length <= 0 || !hashPattern.MatchString(identity.SHA256) {
		return errors.New("path, length, or SHA-256 is invalid")
	}
	if signed {
		if identity.SignatureStatus != "Valid" || identity.SignerSubject == "" ||
			!thumbprintPattern.MatchString(identity.SignerThumbprint) {
			return errors.New("valid Authenticode signer identity is required")
		}
	} else if identity.SignatureStatus != "" || identity.SignerSubject != "" ||
		identity.SignerThumbprint != "" {
		return errors.New("unsigned byte identity contains contradictory signature fields")
	}
	return nil
}

func expectedSDLRealType(controllerType string) int32 {
	switch controllerType {
	case "xbox360":
		return 2 // SDL_GAMEPAD_TYPE_XBOX360
	case "dualshock4":
		return 5 // SDL_GAMEPAD_TYPE_PS4
	case "dualsensegamepadv5":
		return 6 // SDL_GAMEPAD_TYPE_PS5
	default:
		return 0 // Rejected by validateControllerWorkload.
	}
}

func validateControllerWorkload(workload Workload) error {
	type identity struct {
		vendorID, productID uint16
		sdlType             string
	}
	supported := map[string]identity{
		"xbox360":            {vendorID: 0x045e, productID: 0x028e, sdlType: "xbox360"},
		"dualshock4":         {vendorID: 0x054c, productID: 0x09cc, sdlType: "ps4"},
		"dualsensegamepadv5": {vendorID: 0x054c, productID: 0x0ce6, sdlType: "ps5"},
	}
	want, ok := supported[workload.ControllerType]
	if !ok || workload.ExpectedVendorID != want.vendorID ||
		workload.ExpectedProductID != want.productID || workload.ExpectedSDLType != want.sdlType {
		return fmt.Errorf("unsupported or contradictory controller workload %q vid=%#04x pid=%#04x SDL=%q",
			workload.ControllerType, workload.ExpectedVendorID,
			workload.ExpectedProductID, workload.ExpectedSDLType)
	}
	return nil
}

// ParseReport strictly parses a finalized report and rejects stale or forged
// derived fields, unknown JSON fields, and trailing input.
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
		return nil, fmt.Errorf("decode trailing latency report data: %w", err)
	}

	reportedStatistics := make([]DistributionSet, len(report.Runs))
	for index := range report.Runs {
		reportedStatistics[index] = report.Runs[index].Statistics
	}
	reportedComparison := report.Comparison
	reportedTransports := append([]TransportAggregate(nil), report.Transports...)
	reportedFailures := append([]string(nil), report.Failures...)
	reportedVerdict := report.Verdict
	if err := Finalize(&report); err != nil {
		return nil, err
	}
	for index := range report.Runs {
		if !reflect.DeepEqual(reportedStatistics[index], report.Runs[index].Statistics) {
			return nil, fmt.Errorf("%s statistics do not match the individual samples",
				report.Runs[index].Transport)
		}
	}
	if !reflect.DeepEqual(reportedTransports, report.Transports) ||
		!reflect.DeepEqual(reportedComparison, report.Comparison) ||
		!reflect.DeepEqual(reportedFailures, report.Failures) || reportedVerdict != report.Verdict {
		return nil, errors.New("latency report aggregates, comparison, or verdict do not match its source samples")
	}
	return &report, nil
}

func RequirePass(report *Report) error {
	if report == nil {
		return errors.New("nil latency report")
	}
	if report.Verdict == "pass" && len(report.Failures) == 0 {
		return nil
	}
	if len(report.Failures) == 0 {
		return fmt.Errorf("latency gate verdict is %q", report.Verdict)
	}
	return errors.New(strings.Join(report.Failures, "; "))
}

// FinalizeSuite validates workload parity across the complete production
// controller set and recomputes each controller report from individual samples.
func FinalizeSuite(suite *SuiteReport) error {
	if suite == nil {
		return errors.New("nil latency suite")
	}
	if suite.Schema != SuiteSchemaV3 {
		return fmt.Errorf("unsupported latency suite schema %q", suite.Schema)
	}
	if suite.GeneratedAt.IsZero() {
		return errors.New("suite generated_at is required")
	}
	requiredControllers := []string{"xbox360", "dualshock4", "dualsensegamepadv5"}
	if len(suite.Cases) != len(requiredControllers) {
		return fmt.Errorf("latency suite must contain exactly %d controller cases", len(requiredControllers))
	}

	suite.Failures = nil
	var reference *Report
	serverVersion := ""
	for index := range suite.Cases {
		controllerReport := &suite.Cases[index]
		if controllerReport.Workload.ControllerType != requiredControllers[index] {
			return fmt.Errorf("controller case %d is %q, want %q", index,
				controllerReport.Workload.ControllerType, requiredControllers[index])
		}
		if controllerReport.GeneratedAt != suite.GeneratedAt ||
			!reflect.DeepEqual(controllerReport.Provenance, suite.Provenance) {
			return fmt.Errorf("%s case provenance differs from the suite",
				controllerReport.Workload.ControllerType)
		}
		if reference == nil {
			reference = controllerReport
		} else if !sameWorkloadPolicy(reference, controllerReport) {
			return fmt.Errorf("%s does not use the identical authenticated timing workload",
				controllerReport.Workload.ControllerType)
		}
		if err := Finalize(controllerReport); err != nil {
			return fmt.Errorf("%s case: %w", controllerReport.Workload.ControllerType, err)
		}
		for _, failure := range controllerReport.Failures {
			suite.Failures = append(suite.Failures,
				controllerReport.Workload.ControllerType+": "+failure)
		}
		for _, run := range controllerReport.Runs {
			if run.Failure != "" {
				continue
			}
			if serverVersion == "" {
				serverVersion = run.Server.Version
			} else if run.Server.Version != serverVersion {
				return fmt.Errorf("%s/%s used VIIPER version %q, want %q",
					controllerReport.Workload.ControllerType, run.Transport,
					run.Server.Version, serverVersion)
			}
		}
	}
	if len(suite.Failures) == 0 {
		suite.Verdict = "pass"
	} else {
		suite.Verdict = "fail"
	}
	return nil
}

func sameWorkloadPolicy(left, right *Report) bool {
	return left.Workload.APIAddress == right.Workload.APIAddress &&
		left.Workload.USBIPAddress == right.Workload.USBIPAddress &&
		left.Workload.Button == right.Workload.Button &&
		left.Workload.WarmupPairs == right.Workload.WarmupPairs &&
		left.Workload.SamplePairs == right.Workload.SamplePairs &&
		left.Workload.PerTransitionTimeoutNS == right.Workload.PerTransitionTimeoutNS &&
		left.Workload.InterTransitionDelayNS == right.Workload.InterTransitionDelayNS &&
		reflect.DeepEqual(left.Workload.PhaseSweepOffsetsNS, right.Workload.PhaseSweepOffsetsNS) &&
		left.Workload.PhaseSweepSHA256 == right.Workload.PhaseSweepSHA256 &&
		left.Workload.Authentication == right.Workload.Authentication &&
		left.Workload.ScheduleOrientation == right.Workload.ScheduleOrientation &&
		left.Workload.CycleID == right.Workload.CycleID &&
		left.Workload.CycleIndex == right.Workload.CycleIndex &&
		left.Workload.CycleCount == right.Workload.CycleCount &&
		reflect.DeepEqual(left.Policy, right.Policy)
}

type suiteCaseDerived struct {
	statistics []DistributionSet
	transports []TransportAggregate
	comparison ComparisonSet
	verdict    string
	failures   []string
}

// ParseSuiteReport is the strict artifact parser used for release evidence.
func ParseSuiteReport(reader io.Reader) (*SuiteReport, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var suite SuiteReport
	if err := decoder.Decode(&suite); err != nil {
		return nil, fmt.Errorf("decode latency suite: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("latency suite contains trailing JSON")
		}
		return nil, fmt.Errorf("decode trailing latency suite data: %w", err)
	}

	reportedCases := make([]suiteCaseDerived, len(suite.Cases))
	for caseIndex := range suite.Cases {
		controllerReport := &suite.Cases[caseIndex]
		derived := suiteCaseDerived{
			statistics: make([]DistributionSet, len(controllerReport.Runs)),
			transports: append([]TransportAggregate(nil), controllerReport.Transports...),
			comparison: controllerReport.Comparison,
			verdict:    controllerReport.Verdict,
			failures:   append([]string(nil), controllerReport.Failures...),
		}
		for runIndex := range controllerReport.Runs {
			derived.statistics[runIndex] = controllerReport.Runs[runIndex].Statistics
		}
		reportedCases[caseIndex] = derived
	}
	reportedVerdict := suite.Verdict
	reportedFailures := append([]string(nil), suite.Failures...)
	if err := FinalizeSuite(&suite); err != nil {
		return nil, err
	}
	for caseIndex := range suite.Cases {
		controllerReport := &suite.Cases[caseIndex]
		derived := reportedCases[caseIndex]
		for runIndex := range controllerReport.Runs {
			if !reflect.DeepEqual(derived.statistics[runIndex],
				controllerReport.Runs[runIndex].Statistics) {
				return nil, fmt.Errorf("%s/%s statistics do not match individual samples",
					controllerReport.Workload.ControllerType,
					controllerReport.Runs[runIndex].Transport)
			}
		}
		if !reflect.DeepEqual(derived.comparison, controllerReport.Comparison) ||
			!reflect.DeepEqual(derived.transports, controllerReport.Transports) ||
			derived.verdict != controllerReport.Verdict ||
			!reflect.DeepEqual(derived.failures, controllerReport.Failures) {
			return nil, fmt.Errorf("%s derived case verdict does not match its samples",
				controllerReport.Workload.ControllerType)
		}
	}
	if reportedVerdict != suite.Verdict || !reflect.DeepEqual(reportedFailures, suite.Failures) {
		return nil, errors.New("latency suite verdict does not match its controller cases")
	}
	return &suite, nil
}

func RequireSuitePass(suite *SuiteReport) error {
	if suite == nil {
		return errors.New("nil latency suite")
	}
	if suite.Verdict == "pass" && len(suite.Failures) == 0 {
		return nil
	}
	if len(suite.Failures) == 0 {
		return fmt.Errorf("latency suite verdict is %q", suite.Verdict)
	}
	return errors.New(strings.Join(suite.Failures, "; "))
}

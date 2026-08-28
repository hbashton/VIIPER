// Package latency contains the transport-neutral latency evidence contract.
//
// The package intentionally has no dependency on a VIIPER transport or native
// driver. Live adapters can be added later, while deterministic probes exercise
// the complete scheduling, evidence, and policy machinery today.
package latency

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	SchemaV1       = "viiper.transport-latency-landing-zone/v1"
	HarnessVersion = "transport-neutral/v1"

	TransportUSBIP          = "usbip"
	USBIPBaselineVersion    = "0.9.7.7"
	AuthenticatedStreamMode = "password-authenticated-encrypted-stream"
)

type Path string

const (
	PathHealthy  Path = "healthy"
	PathRecovery Path = "recovery"
)

// BoundaryProfile makes the evidence boundary explicit. The existing live
// gate can truthfully implement APIToConsumer. PhysicalToConsumer is reserved
// for a later DS4Windows-to-consumer adapter with a real physical-read stamp;
// the harness must never synthesize that timestamp.
type BoundaryProfile string

const (
	BoundaryAPIToConsumer      BoundaryProfile = "api-to-consumer"
	BoundaryPhysicalToConsumer BoundaryProfile = "physical-to-consumer"
	BoundaryRecoveryToConsumer BoundaryProfile = "recovery-to-consumer"
)

type Transition string

const (
	TransitionPress   Transition = "press"
	TransitionRelease Transition = "release"
)

// Stage names are an inter-process contract. Live adapters should stamp the
// same machine-wide monotonic clock at each boundary; clocks must never be
// subtracted across different clock identities.
type Stage string

const (
	StagePhysicalInputRead Stage = "physical-input-read"
	StageClientPublished   Stage = "client-published"
	StageBrokerReceived    Stage = "broker-received"
	StageTransportAdmitted Stage = "transport-admitted"
	StageVirtualObserved   Stage = "virtual-pad-observed"

	StageRecoveryTriggered Stage = "recovery-triggered"
	StageTransportReady    Stage = "transport-ready"
)

var (
	apiToConsumerStages = [...]Stage{
		StageClientPublished,
		StageBrokerReceived,
		StageTransportAdmitted,
		StageVirtualObserved,
	}
	physicalToConsumerStages = [...]Stage{
		StagePhysicalInputRead,
		StageClientPublished,
		StageBrokerReceived,
		StageTransportAdmitted,
		StageVirtualObserved,
	}
	recoveryToConsumerStages = [...]Stage{
		StageRecoveryTriggered,
		StageTransportReady,
		StageClientPublished,
		StageBrokerReceived,
		StageTransportAdmitted,
		StageVirtualObserved,
	}
	revisionPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	hashPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// RequiredStages returns a copy of the canonical sequence for a declared
// evidence boundary.
func RequiredStages(profile BoundaryProfile) []Stage {
	switch profile {
	case BoundaryAPIToConsumer:
		return append([]Stage(nil), apiToConsumerStages[:]...)
	case BoundaryPhysicalToConsumer:
		return append([]Stage(nil), physicalToConsumerStages[:]...)
	case BoundaryRecoveryToConsumer:
		return append([]Stage(nil), recoveryToConsumerStages[:]...)
	default:
		return nil
	}
}

type ClockProvenance struct {
	Name        string `json:"name"`
	Identity    string `json:"identity"`
	FrequencyHz int64  `json:"frequency_hz"`
}

func (clock ClockProvenance) validate() error {
	if strings.TrimSpace(clock.Name) == "" || strings.TrimSpace(clock.Identity) == "" {
		return errors.New("clock name and identity are required")
	}
	if clock.FrequencyHz <= 0 {
		return errors.New("clock frequency must be positive")
	}
	return nil
}

type ArtifactIdentity struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	SHA256  string `json:"sha256"`
}

type KeyValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// TransportProvenance identifies the exact runtime being sampled. Artifacts
// should be executable, driver, catalog, manifest, or source-build hashes—not
// a friendly package name standing in for the bytes actually loaded.
type TransportProvenance struct {
	Name           string             `json:"name"`
	Implementation string             `json:"implementation"`
	Version        string             `json:"version"`
	BuildIdentity  string             `json:"build_identity"`
	Clock          ClockProvenance    `json:"clock"`
	Artifacts      []ArtifactIdentity `json:"artifacts"`
	Configuration  []KeyValue         `json:"configuration,omitempty"`
}

func (descriptor TransportProvenance) validate() error {
	if strings.TrimSpace(descriptor.Name) == "" ||
		strings.TrimSpace(descriptor.Implementation) == "" ||
		strings.TrimSpace(descriptor.Version) == "" ||
		strings.TrimSpace(descriptor.BuildIdentity) == "" {
		return errors.New("transport name, implementation, version, and build identity are required")
	}
	if err := descriptor.Clock.validate(); err != nil {
		return fmt.Errorf("transport %s: %w", descriptor.Name, err)
	}
	if len(descriptor.Artifacts) == 0 {
		return fmt.Errorf("transport %s has no exact artifact identity", descriptor.Name)
	}
	lastName := ""
	for index, artifact := range descriptor.Artifacts {
		if strings.TrimSpace(artifact.Name) == "" || !hashPattern.MatchString(artifact.SHA256) {
			return fmt.Errorf("transport %s artifact %d is incomplete", descriptor.Name, index)
		}
		if artifact.Name <= lastName {
			return fmt.Errorf("transport %s artifacts must be unique and sorted by name", descriptor.Name)
		}
		lastName = artifact.Name
	}
	lastKey := ""
	for index, pair := range descriptor.Configuration {
		if strings.TrimSpace(pair.Key) == "" || pair.Key <= lastKey {
			return fmt.Errorf("transport %s configuration item %d is empty, duplicated, or unsorted", descriptor.Name, index)
		}
		lastKey = pair.Key
	}
	return nil
}

func cloneTransportProvenance(descriptor TransportProvenance) TransportProvenance {
	cloned := descriptor
	cloned.Artifacts = append([]ArtifactIdentity(nil), descriptor.Artifacts...)
	cloned.Configuration = append([]KeyValue(nil), descriptor.Configuration...)
	return cloned
}

type RuntimeProvenance struct {
	GoVersion string `json:"go_version"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	Hostname  string `json:"hostname"`
}

// WorkloadProvenance binds both transports to the same controller, input,
// authenticated API boundary, and exact consumer implementation.
type WorkloadProvenance struct {
	ControllerType         string          `json:"controller_type"`
	ExpectedVendorID       uint16          `json:"expected_vendor_id"`
	ExpectedProductID      uint16          `json:"expected_product_id"`
	Control                string          `json:"control"`
	APIEndpoint            string          `json:"api_endpoint"`
	Authentication         string          `json:"authentication"`
	Observer               string          `json:"observer"`
	ObserverVersion        string          `json:"observer_version"`
	ObserverArtifactSHA256 string          `json:"observer_artifact_sha256"`
	ObserverEventClock     ClockProvenance `json:"observer_event_clock"`
}

func (workload WorkloadProvenance) validate() error {
	if strings.TrimSpace(workload.ControllerType) == "" ||
		workload.ExpectedVendorID == 0 || workload.ExpectedProductID == 0 ||
		strings.TrimSpace(workload.Control) == "" ||
		strings.TrimSpace(workload.APIEndpoint) == "" ||
		workload.Authentication != AuthenticatedStreamMode ||
		strings.TrimSpace(workload.Observer) == "" ||
		strings.TrimSpace(workload.ObserverVersion) == "" ||
		!hashPattern.MatchString(workload.ObserverArtifactSHA256) {
		return errors.New("controller, authenticated API, and exact observer workload provenance are required")
	}
	if err := workload.ObserverEventClock.validate(); err != nil {
		return fmt.Errorf("observer event clock: %w", err)
	}
	return nil
}

type SessionProvenance struct {
	SourceRevision  string            `json:"source_revision"`
	SourceTreeDirty bool              `json:"source_tree_dirty"`
	HarnessVersion  string            `json:"harness_version"`
	RunLabel        string            `json:"run_label"`
	Runtime         RuntimeProvenance `json:"runtime"`
}

// CaptureSessionProvenance records runtime facts without invoking Git or any
// platform-specific tooling. The caller is responsible for obtaining the
// revision and dirty bit from the exact checkout used to build the harness.
func CaptureSessionProvenance(sourceRevision string, sourceTreeDirty bool, runLabel string) (SessionProvenance, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return SessionProvenance{}, fmt.Errorf("read hostname: %w", err)
	}
	provenance := SessionProvenance{
		SourceRevision: sourceRevision, SourceTreeDirty: sourceTreeDirty,
		HarnessVersion: HarnessVersion, RunLabel: runLabel,
		Runtime: RuntimeProvenance{
			GoVersion: runtime.Version(), GOOS: runtime.GOOS,
			GOARCH: runtime.GOARCH, Hostname: hostname,
		},
	}
	if err := provenance.validate(); err != nil {
		return SessionProvenance{}, err
	}
	return provenance, nil
}

func (provenance SessionProvenance) validate() error {
	if !revisionPattern.MatchString(provenance.SourceRevision) {
		return errors.New("source revision must be a lowercase 40- or 64-digit hash")
	}
	if provenance.HarnessVersion != HarnessVersion {
		return fmt.Errorf("harness version %q is not %q", provenance.HarnessVersion, HarnessVersion)
	}
	if strings.TrimSpace(provenance.RunLabel) == "" ||
		strings.TrimSpace(provenance.Runtime.GoVersion) == "" ||
		strings.TrimSpace(provenance.Runtime.GOOS) == "" ||
		strings.TrimSpace(provenance.Runtime.GOARCH) == "" ||
		strings.TrimSpace(provenance.Runtime.Hostname) == "" {
		return errors.New("runtime and run-label provenance are required")
	}
	return nil
}

type StageTimestamp struct {
	Stage Stage `json:"stage"`
	Ticks int64 `json:"ticks"`
}

type Timeline struct {
	ClockIdentity string           `json:"clock_identity"`
	Timestamps    []StageTimestamp `json:"timestamps"`
}

type LifecycleEvidence struct {
	GenerationBefore  uint64 `json:"generation_before"`
	GenerationAfter   uint64 `json:"generation_after"`
	RecoveryTriggered bool   `json:"recovery_triggered"`
	RecoveryReason    string `json:"recovery_reason,omitempty"`
}

type CaptureEvidence struct {
	Timeline    Timeline            `json:"timeline"`
	Observation ConsumerObservation `json:"observation"`
	Lifecycle   LifecycleEvidence   `json:"lifecycle"`
}

// ConsumerObservation is a second-clock stale-event exclusion fence. A live
// SDL adapter, for example, stamps its event clock immediately before publish
// and retains the exact observed button-event timestamp. This clock is never
// subtracted from the stage clock.
type ConsumerObservation struct {
	ClockIdentity        string     `json:"clock_identity"`
	PrePublishFenceTicks uint64     `json:"pre_publish_fence_ticks"`
	ObservedEventTicks   uint64     `json:"observed_event_ticks"`
	Control              string     `json:"control"`
	Transition           Transition `json:"transition"`
}

type StageSpan struct {
	From       Stage `json:"from"`
	To         Stage `json:"to"`
	DurationNS int64 `json:"duration_ns"`
}

// ValidateCapture validates clock, stage-order, and lifecycle evidence, then
// derives the end-to-end latency and every adjacent stage span from raw ticks.
func ValidateCapture(path Path, profile BoundaryProfile, transition Transition,
	clock ClockProvenance, workload WorkloadProvenance, expectedRecoveryScenario string,
	evidence CaptureEvidence) (int64, []StageSpan, error) {
	if path != PathHealthy && path != PathRecovery {
		return 0, nil, fmt.Errorf("unsupported latency path %q", path)
	}
	if transition != TransitionPress && transition != TransitionRelease {
		return 0, nil, fmt.Errorf("unsupported transition %q", transition)
	}
	if err := clock.validate(); err != nil {
		return 0, nil, err
	}
	if err := workload.validate(); err != nil {
		return 0, nil, err
	}
	if evidence.Timeline.ClockIdentity != clock.Identity {
		return 0, nil, errors.New("capture clock identity does not match transport provenance")
	}
	if (path == PathHealthy && profile == BoundaryRecoveryToConsumer) ||
		(path == PathRecovery && profile != BoundaryRecoveryToConsumer) {
		return 0, nil, fmt.Errorf("path %q cannot use boundary profile %q", path, profile)
	}
	required := RequiredStages(profile)
	if len(required) == 0 {
		return 0, nil, fmt.Errorf("unsupported latency path %q", path)
	}
	if len(evidence.Timeline.Timestamps) != len(required) {
		return 0, nil, fmt.Errorf("%s capture has %d stages, want %d", path,
			len(evidence.Timeline.Timestamps), len(required))
	}
	for index, timestamp := range evidence.Timeline.Timestamps {
		if timestamp.Stage != required[index] {
			return 0, nil, fmt.Errorf("%s stage %d is %q, want %q", path,
				index, timestamp.Stage, required[index])
		}
		if timestamp.Ticks <= 0 || (index > 0 && timestamp.Ticks <= evidence.Timeline.Timestamps[index-1].Ticks) {
			return 0, nil, fmt.Errorf("%s stage %q timestamp is absent or non-monotonic", path, timestamp.Stage)
		}
	}
	observation := evidence.Observation
	if observation.ClockIdentity != workload.ObserverEventClock.Identity ||
		observation.PrePublishFenceTicks == 0 ||
		observation.ObservedEventTicks <= observation.PrePublishFenceTicks ||
		observation.Control != workload.Control || observation.Transition != transition {
		return 0, nil, errors.New("consumer observation is absent, stale, or does not match the requested edge")
	}

	switch path {
	case PathHealthy:
		if expectedRecoveryScenario != "" || evidence.Lifecycle.RecoveryTriggered ||
			evidence.Lifecycle.RecoveryReason != "" ||
			evidence.Lifecycle.GenerationBefore == 0 ||
			evidence.Lifecycle.GenerationAfter != evidence.Lifecycle.GenerationBefore {
			return 0, nil, errors.New("healthy capture crossed or reported a transport lifecycle boundary")
		}
	case PathRecovery:
		if strings.TrimSpace(expectedRecoveryScenario) == "" ||
			!evidence.Lifecycle.RecoveryTriggered ||
			evidence.Lifecycle.RecoveryReason != expectedRecoveryScenario ||
			evidence.Lifecycle.GenerationBefore == 0 ||
			evidence.Lifecycle.GenerationAfter <= evidence.Lifecycle.GenerationBefore {
			return 0, nil, errors.New("recovery capture lacks the expected scenario and an advancing lifecycle generation")
		}
	}

	spans := make([]StageSpan, 0, len(required)-1)
	for index := 1; index < len(evidence.Timeline.Timestamps); index++ {
		previous := evidence.Timeline.Timestamps[index-1]
		current := evidence.Timeline.Timestamps[index]
		duration, err := counterIntervalNS(previous.Ticks, current.Ticks, clock.FrequencyHz)
		if err != nil {
			return 0, nil, fmt.Errorf("%s to %s: %w", previous.Stage, current.Stage, err)
		}
		spans = append(spans, StageSpan{From: previous.Stage, To: current.Stage, DurationNS: duration})
	}
	total, err := counterIntervalNS(evidence.Timeline.Timestamps[0].Ticks,
		evidence.Timeline.Timestamps[len(evidence.Timeline.Timestamps)-1].Ticks,
		clock.FrequencyHz)
	if err != nil {
		return 0, nil, err
	}
	return total, spans, nil
}

func counterIntervalNS(start, end, frequency int64) (int64, error) {
	if start <= 0 || end <= start || frequency <= 0 {
		return 0, errors.New("counter interval or frequency is invalid")
	}
	delta := end - start
	if delta > math.MaxInt64/int64(time.Second) {
		return 0, errors.New("counter interval overflows nanosecond conversion")
	}
	nanoseconds := delta * int64(time.Second) / frequency
	if nanoseconds <= 0 {
		return 0, errors.New("counter interval has sub-nanosecond or non-positive duration")
	}
	return nanoseconds, nil
}

var defaultPhaseSweepOffsetsNS = [...]int64{
	0,
	125 * int64(time.Microsecond),
	250 * int64(time.Microsecond),
	375 * int64(time.Microsecond),
	500 * int64(time.Microsecond),
	625 * int64(time.Microsecond),
	750 * int64(time.Microsecond),
	875 * int64(time.Microsecond),
}

func DefaultPhaseSweepOffsetsNS() []int64 {
	return append([]int64(nil), defaultPhaseSweepOffsetsNS[:]...)
}

func PhaseSweepSHA256(offsets []int64) string {
	var canonical strings.Builder
	for index, offset := range offsets {
		if index > 0 {
			canonical.WriteByte(',')
		}
		canonical.WriteString(strconv.FormatInt(offset, 10))
	}
	digest := sha256.Sum256([]byte(canonical.String()))
	return fmt.Sprintf("%x", digest)
}

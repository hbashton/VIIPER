package ns2pro

import (
	"sync"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
)

// ns2InputReportSequencer owns the mutable fields which are assigned at HID
// presentation rather than producer receipt. Its lock is never held while a
// device or scheduler lock is acquired. A claim therefore sees one coherent
// report mode, feature set, and metadata snapshot without inverting locks back
// into the producer or USB protocol planes.
type ns2InputReportSequencer struct {
	mu sync.Mutex

	activeReportID  uint8
	featureFlags    uint8
	meta            MetaState
	reportCounter32 uint32
	reportCounter8  uint8
	motionStart     time.Time
	lastMotionTS    uint32
}

func newNS2InputReportSequencer(meta MetaState,
	now time.Time,
) *ns2InputReportSequencer {
	if now.IsZero() {
		now = time.Now()
	}
	return &ns2InputReportSequencer{
		activeReportID: ReportIDPro,
		featureFlags:   FeatureButtons | FeatureSticks,
		meta:           meta,
		// The fixed scheduler validates its canonical idle report during
		// construction. Starting at 0xff makes that private report counter zero,
		// preserving counter one for the first externally claimed Pro report.
		reportCounter8: ^uint8(0),
		motionStart:    now,
	}
}

func (s *ns2InputReportSequencer) buildCurrentInto(state *InputState,
	destination []byte,
) int {
	return s.buildInterruptInto(state, 0, destination)
}

func (s *ns2InputReportSequencer) buildInterruptInto(state *InputState,
	reportID uint8, destination []byte,
) int {
	if s == nil || state == nil || len(destination) < InputReportSize {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if reportID == 0 {
		reportID = s.activeReportID
	}
	features := s.featureFlags
	meta := s.meta
	switch reportID {
	case ReportIDCommon:
		s.reportCounter32++
		var motionTimestamp uint32
		if features&FeatureIMU != 0 {
			motionTimestamp = uint32(time.Since(s.motionStart).Microseconds())
			s.lastMotionTS = motionTimestamp
		}
		return state.buildCommonReportInto(destination, s.reportCounter32,
			motionTimestamp, features, meta)
	case ReportIDPro:
		s.reportCounter8++
		return state.buildProReportInto(destination, s.reportCounter8,
			features, meta)
	default:
		return 0
	}
}

// buildControlInto returns a current GET_REPORT snapshot without consuming
// semantic interrupt work or advancing the periodic interrupt counters. Both
// paths share the sequencer lock, so a control request cannot race a duplicate
// counter assignment with an interrupt claim.
func (s *ns2InputReportSequencer) buildControlInto(state *InputState,
	reportID uint8, destination []byte,
) int {
	if s == nil || state == nil || len(destination) < InputReportSize {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if reportID == 0 {
		reportID = s.activeReportID
	}
	switch reportID {
	case ReportIDCommon:
		return state.buildCommonReportInto(destination, s.reportCounter32,
			s.lastMotionTS, s.featureFlags, s.meta)
	case ReportIDPro:
		return state.buildProReportInto(destination, s.reportCounter8,
			s.featureFlags, s.meta)
	default:
		return 0
	}
}

func (s *ns2InputReportSequencer) setMeta(meta MetaState) {
	s.mu.Lock()
	s.meta = meta
	s.mu.Unlock()
}

func (s *ns2InputReportSequencer) setActiveReportID(reportID uint8) bool {
	if reportID != ReportIDCommon && reportID != ReportIDPro {
		return false
	}
	s.mu.Lock()
	s.activeReportID = reportID
	s.mu.Unlock()
	return true
}

func (s *ns2InputReportSequencer) resetFeatures() {
	s.mu.Lock()
	s.featureFlags = 0
	s.mu.Unlock()
}

func (s *ns2InputReportSequencer) enableFeatures(features uint8) {
	s.mu.Lock()
	s.featureFlags |= features
	s.mu.Unlock()
}

func (s *ns2InputReportSequencer) disableFeatures(features uint8) {
	s.mu.Lock()
	s.featureFlags &^= features
	s.mu.Unlock()
}

func ns2InputTransition(previous, next InputState) bool {
	return previous.Buttons != next.Buttons
}

func (d *NS2Pro) signalInput() {
	select {
	case d.inputSignal <- struct{}{}:
	default:
	}
}

func (d *NS2Pro) currentInputState() InputState {
	d.stateMu.Lock()
	state := d.inputState
	d.stateMu.Unlock()
	return state
}

func (d *NS2Pro) storeCurrentInputState(state InputState) {
	d.stateMu.Lock()
	d.inputState = state
	d.stateMu.Unlock()
}

func (d *NS2Pro) advanceInputReportSnapshotVersionLocked() {
	d.inputReportSnapshotVersion++
	if d.inputReportSnapshotVersion == 0 {
		d.inputReportSnapshotVersion = 1
	}
}

func (d *NS2Pro) refreshNeutralInputReportSnapshotLocked() {
	neutral := InputState{
		LX: StickCenter, LY: StickCenter,
		RX: StickCenter, RY: StickCenter,
	}
	if d.inputReports.buildControlInto(&neutral, 0,
		d.inputReportSnapshot[:]) != InputReportSize {
		clear(d.inputReportSnapshot[:])
	}
	d.inputReportSnapshotState = neutral
	d.advanceInputReportSnapshotVersionLocked()
}

func (d *NS2Pro) refreshInputReportSnapshotWithStateLocked(state InputState) {
	if d.inputReports.buildControlInto(&state, 0,
		d.inputReportSnapshot[:]) != InputReportSize {
		clear(d.inputReportSnapshot[:])
	}
	d.inputReportSnapshotState = state
	d.advanceInputReportSnapshotVersionLocked()
}

func (d *NS2Pro) commitInputReportSnapshotLocked(report []byte,
	state InputState,
) {
	if len(report) < InputReportSize {
		return
	}
	copy(d.inputReportSnapshot[:], report[:InputReportSize])
	d.inputReportSnapshotState = state
	d.advanceInputReportSnapshotVersionLocked()
}

func (d *NS2Pro) acquireInputProducer() (
	inputpresentation.FixedReportProducerLease, bool,
) {
	d.producerMu.Lock()
	if d.producerActive {
		d.producerMu.Unlock()
		return inputpresentation.FixedReportProducerLease{}, false
	}
	retiredCompatibilityProducer := false
	d.inputLifecycleMu.Lock()
	lease := d.input.ProducerLease()
	if d.compatProducerUsed {
		compatibilityLease := lease
		var retired bool
		lease, retired = d.input.RetireProducerLease(lease, time.Now())
		if !retired {
			d.inputLifecycleMu.Unlock()
			d.producerMu.Unlock()
			return inputpresentation.FixedReportProducerLease{}, false
		}
		// The compatibility surface and raw stream are distinct owners. Purge
		// every compatibility claim/history and publish exactly one neutral
		// boundary before accepting a complete raw snapshot.
		d.clearInputResynchronization()
		d.activePresentationClaim = inputpresentation.Claim{}
		clear(d.activePresentationReport[:])
		d.activePresentationState = InputState{}
		d.hasDeferredPresentation = false
		neutral := *defaultInputState()
		d.storeCurrentInputState(neutral)
		d.refreshNeutralInputReportSnapshotLocked()
		d.retiredCompatibilityLease = compatibilityLease
		retiredCompatibilityProducer = true
	}
	d.producerActive = true
	d.compatProducerUsed = false
	d.inputLifecycleMu.Unlock()
	d.producerMu.Unlock()
	if retiredCompatibilityProducer {
		d.signalInput()
	}
	return lease, true
}

func (d *NS2Pro) releaseInputProducer() {
	d.producerMu.Lock()
	if !d.producerActive {
		d.producerMu.Unlock()
		return
	}
	d.inputLifecycleMu.Lock()
	d.clearInputResynchronization()
	// A USB presentation boundary can advance the scheduler lease while the
	// same raw stream remains connected. producerMu proves this connection is
	// still the sole raw owner, so retiring the current lease ends exactly that
	// owner's latest epoch without rotating the USB presentation generation.
	lease := d.input.ProducerLease()
	_, retired := d.input.RetireProducerLease(lease, time.Now())
	if retired {
		d.retiredCompatibilityLease =
			inputpresentation.FixedReportProducerLease{}
		d.activePresentationClaim = inputpresentation.Claim{}
		clear(d.activePresentationReport[:])
		d.activePresentationState = InputState{}
		d.hasDeferredPresentation = false
		d.refreshNeutralInputReportSnapshotLocked()
	}
	d.inputLifecycleMu.Unlock()
	d.producerActive = false
	d.producerMu.Unlock()
	if retired {
		d.signalInput()
	}
}

func (d *NS2Pro) publishInputStateWithLease(
	lease inputpresentation.FixedReportProducerLease, state InputState,
	receivedAt time.Time,
) (inputpresentation.FixedReportProducerLease,
	inputpresentation.FixedReportPublishDisposition) {
	d.inputLifecycleMu.Lock()
	defer d.inputLifecycleMu.Unlock()
	disposition := d.input.PublishWithLease(lease, state, receivedAt)
	currentLease := d.input.ProducerLease()
	if lease == d.retiredCompatibilityLease {
		// A callback carrying the compatibility owner's retired epoch must not
		// become a raw-owner resynchronization candidate merely because the new
		// owner is still presenting its mandatory neutral.
		return currentLease, disposition
	}
	if disposition.Accepted() {
		d.storeCurrentInputState(state)
		d.signalInput()
		return lease, disposition
	}

	switch disposition {
	case inputpresentation.FixedReportPublishFaultedOverflow:
		d.refreshNeutralInputReportSnapshotLocked()
		d.storeCurrentInputState(state)
		d.stageInputResynchronization(state, receivedAt)
		d.signalInput()
	case inputpresentation.FixedReportPublishRejectedNeutralPending:
		d.storeCurrentInputState(state)
		d.stageInputResynchronization(state, receivedAt)
		d.signalInput()
	case inputpresentation.FixedReportPublishRejectedStaleProducer,
		inputpresentation.FixedReportPublishRejectedResynchronizationRequired:
		snapshot := d.input.Snapshot()
		if snapshot.MandatoryNeutral || snapshot.Resynchronization {
			d.storeCurrentInputState(state)
			d.stageInputResynchronization(state, receivedAt)
			d.signalInput()
		}
		// With no fault state, stale producer means a USB presentation boundary.
		// The pre-boundary frame is discarded; the returned lease applies only
		// to the raw stream's next complete frame.
	}
	return currentLease, disposition
}

func (d *NS2Pro) stageInputResynchronization(state InputState,
	receivedAt time.Time,
) {
	d.resyncMu.Lock()
	if !d.hasPendingResync || !receivedAt.Before(d.pendingResyncAt) {
		d.pendingResync = state
		d.pendingResyncAt = receivedAt
		d.hasPendingResync = true
	}
	d.resyncMu.Unlock()
}

func (d *NS2Pro) clearInputResynchronization() {
	d.resyncMu.Lock()
	d.pendingResync = InputState{}
	d.pendingResyncAt = time.Time{}
	d.hasPendingResync = false
	d.resyncMu.Unlock()
}

func (d *NS2Pro) pendingInputResynchronization() (InputState, bool) {
	d.resyncMu.Lock()
	state := d.pendingResync
	pending := d.hasPendingResync
	d.resyncMu.Unlock()
	return state, pending
}

// updateInputEncodingConfiguration serializes host-visible report-mode,
// feature, metadata, and enablement changes with claim selection/admission. If
// immutable bytes are active or awaiting retry, the old producer epoch is
// fenced to a mandatory neutral and fresh current-state resynchronization. An
// unclaimed semantic journal needs no purge: it has not been encoded and will
// naturally use the new configuration at selection.
func (d *NS2Pro) updateInputEncodingConfiguration(update func()) {
	d.inputLifecycleMu.Lock()
	fenced := false
	snapshot := d.input.Snapshot()
	if d.activePresentationClaim.Valid() || d.hasDeferredPresentation ||
		snapshot.Resynchronization {
		now := time.Now()
		state, hadPendingResynchronization :=
			d.pendingInputResynchronization()
		stageCurrentState := !snapshot.MandatoryNeutral &&
			!snapshot.Resynchronization
		if !hadPendingResynchronization && stageCurrentState {
			state = d.currentInputState()
		}
		d.clearInputResynchronization()
		lease := d.input.ProducerLease()
		_, fenced = d.input.RetireProducerLease(lease, now)
		if fenced {
			d.activePresentationClaim = inputpresentation.Claim{}
			clear(d.activePresentationReport[:])
			d.activePresentationState = InputState{}
			d.hasDeferredPresentation = false
			if hadPendingResynchronization || stageCurrentState {
				d.stageInputResynchronization(state, now)
			}
		}
	}
	update()
	if fenced {
		d.refreshNeutralInputReportSnapshotLocked()
	} else {
		// No immutable claim crossed the configuration boundary. Re-encode the
		// exact last committed semantic controls with the new mode/features/meta
		// instead of fabricating a transient release on EP0.
		d.refreshInputReportSnapshotWithStateLocked(
			d.inputReportSnapshotState)
	}
	d.inputLifecycleMu.Unlock()
	if fenced {
		d.signalInput()
	}
}

// tryPublishInputResynchronizationLocked is called only by the presentation
// owner after a claim commits. Producers can stage newer complete snapshots,
// but cannot directly race a resynchronization or clear one another's state.
// inputLifecycleMu must be held.
func (d *NS2Pro) tryPublishInputResynchronizationLocked() bool {
	d.resyncMu.Lock()
	defer d.resyncMu.Unlock()
	if !d.hasPendingResync {
		return false
	}
	disposition := d.input.Resynchronize(d.input.ProducerLease(),
		d.pendingResync, d.pendingResyncAt)
	if disposition !=
		inputpresentation.FixedReportPublishAcceptedResynchronization {
		return false
	}
	d.pendingResync = InputState{}
	d.pendingResyncAt = time.Time{}
	d.hasPendingResync = false
	d.signalInput()
	return true
}

// ensureIdleSnapshotLocked turns an otherwise-idle host service opportunity
// into a newly encoded continuous report. This preserves the controller's
// existing per-report counter and motion-timestamp behavior without adding a
// timer or coalescing any pending producer state. inputLifecycleMu is held.
func (d *NS2Pro) ensureIdleSnapshotLocked(selectedAt time.Time) {
	if d.activePresentationClaim.Valid() {
		return
	}
	snapshot := d.input.Snapshot()
	if snapshot.TransitionDepth != 0 || snapshot.ContinuousPending ||
		snapshot.MandatoryNeutral || snapshot.Resynchronization {
		return
	}
	state := d.currentInputState()
	if disposition := d.input.PublishWithLease(d.input.ProducerLease(),
		state, selectedAt); disposition.Accepted() {
		d.signalInput()
	}
}

func (d *NS2Pro) ClaimInputPresentation(destination []byte,
	selectedAt time.Time,
) inputpresentation.Claim {
	if len(destination) < InputReportSize {
		return inputpresentation.Claim{}
	}
	if selectedAt.IsZero() {
		selectedAt = time.Now()
	}
	d.inputLifecycleMu.Lock()
	defer d.inputLifecycleMu.Unlock()
	if !d.reportsEnabled() || d.activePresentationClaim.Valid() {
		return inputpresentation.Claim{}
	}
	d.ensureIdleSnapshotLocked(selectedAt)
	claim := d.input.ClaimInputPresentation(destination, selectedAt)
	if claim.Valid() {
		state, stateCurrent := d.input.StateForInputPresentationClaim(claim)
		if !stateCurrent {
			d.input.ResolveInputPresentation(claim,
				inputpresentation.OutcomeDefer, selectedAt)
			return inputpresentation.Claim{}
		}
		d.activePresentationClaim = claim
		d.activePresentationState = state
		copy(d.activePresentationReport[:], destination[:InputReportSize])
	}
	return claim
}

func (d *NS2Pro) OwnsInputPresentationEndpoint(endpoint uint8) bool {
	return endpoint == EndpointHIDIn&0x0f
}

func (d *NS2Pro) InputPresentationGeneration() uint64 {
	d.inputLifecycleMu.Lock()
	generation := d.input.Generation()
	d.inputLifecycleMu.Unlock()
	return generation
}

func sameInputPresentationClaim(left,
	right inputpresentation.Claim,
) bool {
	return left.Token == right.Token && left.Generation == right.Generation &&
		left.Size == right.Size
}

func (d *NS2Pro) ResolveInputPresentation(claim inputpresentation.Claim,
	outcome inputpresentation.Outcome, completedAt time.Time,
) bool {
	d.inputLifecycleMu.Lock()
	if !sameInputPresentationClaim(d.activePresentationClaim, claim) {
		d.inputLifecycleMu.Unlock()
		return false
	}
	resolved := d.input.ResolveInputPresentation(claim, outcome, completedAt)
	// The scheduler may already have terminally revoked a claim at final
	// admission. The exact transport owner must still be released here.
	if resolved {
		switch outcome {
		case inputpresentation.OutcomeCommit:
			d.commitInputReportSnapshotLocked(d.activePresentationReport[:],
				d.activePresentationState)
			d.hasDeferredPresentation = false
			d.tryPublishInputResynchronizationLocked()
		case inputpresentation.OutcomeDefer:
			d.hasDeferredPresentation = true
		case inputpresentation.OutcomeRetire:
			d.hasDeferredPresentation = false
		}
	}
	d.activePresentationClaim = inputpresentation.Claim{}
	clear(d.activePresentationReport[:])
	d.activePresentationState = InputState{}
	d.inputLifecycleMu.Unlock()
	return resolved
}

func (d *NS2Pro) CanAdmitInputPresentation(claim inputpresentation.Claim,
	admittedAt time.Time,
) bool {
	d.inputLifecycleMu.Lock()
	accepted := sameInputPresentationClaim(d.activePresentationClaim, claim) &&
		d.input.CanAdmitInputPresentation(claim, admittedAt)
	d.inputLifecycleMu.Unlock()
	return accepted
}

func (d *NS2Pro) RetireInputPresentationGeneration(generation uint64,
	retiredAt time.Time,
) bool {
	d.inputLifecycleMu.Lock()
	currentState, retired :=
		d.input.RetireInputPresentationGenerationWithCurrentState(
			generation, retiredAt)
	if retired {
		d.clearInputResynchronization()
		d.activePresentationClaim = inputpresentation.Claim{}
		clear(d.activePresentationReport[:])
		d.activePresentationState = InputState{}
		d.hasDeferredPresentation = false
		// The scheduler returns the exact state it atomically chose while
		// collapsing the generation. Mirror that preview on EP0 without guessing
		// from a producer-side cache, selecting the successor journal, or
		// advancing periodic report counters.
		d.refreshInputReportSnapshotWithStateLocked(currentState)
	}
	d.inputLifecycleMu.Unlock()
	return retired
}

func (d *NS2Pro) InputSchedulerSnapshot() inputpresentation.FixedReportSchedulerSnapshot {
	return d.input.Snapshot()
}

func (d *NS2Pro) snapshotInputReportForIDInto(reportID uint8,
	destination []byte,
) (int, uint64) {
	d.inputLifecycleMu.Lock()
	defer d.inputLifecycleMu.Unlock()
	n := min(len(destination), InputReportSize)
	if reportID == 0 || (n > 0 && d.inputReportSnapshot[0] == reportID) {
		copy(destination[:n], d.inputReportSnapshot[:n])
		return n, d.inputReportSnapshotVersion
	}
	// Re-encode the exact last committed (or lifecycle-fenced neutral) semantic
	// snapshot for an explicitly requested inactive layout. Pending ordered work
	// remains invisible, while the shared sequencer supplies coherent
	// features/metadata without advancing interrupt counters.
	var report [InputReportSize]byte
	if d.inputReports.buildControlInto(&d.inputReportSnapshotState, reportID,
		report[:]) !=
		InputReportSize {
		return 0, d.inputReportSnapshotVersion
	}
	copy(destination[:n], report[:n])
	return n, d.inputReportSnapshotVersion
}

// SnapshotInputReportInto is the versioned EP0 GET_REPORT source. It never
// consumes the interrupt journal or advances report counters. The cache is the
// last committed interrupt report, or a canonical neutral installed at a
// producer/configuration fault fence.
func (d *NS2Pro) SnapshotInputReportInto(destination []byte) (int, uint64) {
	return d.snapshotInputReportForIDInto(0, destination)
}

// SnapshotInputReportForIDInto extends the versioned control snapshot to the
// two Switch 2 input report layouts.
func (d *NS2Pro) SnapshotInputReportForIDInto(reportID uint8,
	destination []byte,
) (int, uint64) {
	return d.snapshotInputReportForIDInto(reportID, destination)
}

func (d *NS2Pro) InputReportSnapshotCurrent(version uint64) bool {
	d.inputLifecycleMu.Lock()
	current := version != 0 && version == d.inputReportSnapshotVersion
	d.inputLifecycleMu.Unlock()
	return current
}

func (d *NS2Pro) SupportsInputReportSnapshot(reportID uint8) bool {
	return reportID == 0 || reportID == ReportIDCommon || reportID == ReportIDPro
}

// BuildInputReportInto is the allocation-free compatibility surface. The
// production USB/IP path consumes Source directly and performs admission at
// the socket serializer boundary.
func (d *NS2Pro) BuildInputReportInto(destination []byte) int {
	claim := d.ClaimInputPresentation(destination, time.Now())
	if !claim.Valid() {
		return 0
	}
	if !d.CanAdmitInputPresentation(claim, time.Now()) {
		d.ResolveInputPresentation(claim, inputpresentation.OutcomeDefer,
			time.Now())
		return 0
	}
	if !d.ResolveInputPresentation(claim, inputpresentation.OutcomeCommit,
		time.Now()) {
		return 0
	}
	return claim.Size
}

var _ inputpresentation.Source = (*NS2Pro)(nil)
var _ inputpresentation.AdmissionSource = (*NS2Pro)(nil)

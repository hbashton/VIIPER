package dualsense

import (
	"encoding/binary"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
)

const (
	dualSenseInputTransitionCapacity = 64
	dualSenseUSBConnectStateWired    = 0x08
	dualSenseEdgeActiveProfile       = 0x80
	dualSenseTriggerStatusNeutral    = 0x09

	physicalMetadataTouchTimestamp = 0  // physical report byte 41
	physicalMetadataR2Status       = 1  // physical report byte 42
	physicalMetadataL2Status       = 2  // physical report byte 43
	physicalMetadataEffectStatus   = 7  // physical report byte 48
	physicalMetadataBattery        = 12 // physical report byte 53
	physicalMetadataConnectState   = 13 // physical report byte 54
	physicalMetadataHeadsetStatus  = 14 // physical report byte 55
)

var inputQueueAgeBucketLimits = [...]time.Duration{
	50 * time.Microsecond,
	100 * time.Microsecond,
	250 * time.Microsecond,
	500 * time.Microsecond,
	time.Millisecond,
	2 * time.Millisecond,
	4 * time.Millisecond,
	8 * time.Millisecond,
	16 * time.Millisecond,
	32 * time.Millisecond,
	64 * time.Millisecond,
}

type inputTriggerEpoch struct {
	id                 uint64
	active             bool
	peak               uint8
	presentedPeak      uint8
	presentedPeakState InputState
	peakState          InputState
	peakReceivedAt     time.Time
	peakReceiveOrdinal uint64
}

type scheduledInputState struct {
	state          InputState
	receivedAt     time.Time
	generation     uint64
	sampleQueueAge bool
	ordered        bool
	l2Epoch        uint64
	r2Epoch        uint64
	l2Press        bool
	r2Press        bool
	// selectionLatencySampled follows the logical state through a failed-send
	// retry. Endpoint selection is sampled once; transport completion remains a
	// separate distribution and may occur substantially later.
	selectionLatencySampled bool
	// A shared L2/R2 press entry may represent one epoch peak by changing only
	// that trigger, as required by the pending-press strengthening rule. These
	// markers prevent a later, independently timed peak from being folded into
	// the same entry and synthesizing a two-trigger state that was never
	// received. A second peak may share the entry only when its saved physical
	// snapshot already contains the first anchored trigger value.
	l2PeakAnchored bool
	r2PeakAnchored bool
}

// InputSchedulerSnapshot is a lock-bounded diagnostic snapshot. Presentation
// buckets count successfully transported received states; selection buckets
// count each received logical state once at its first HID claim. Bucket N
// means no older than inputQueueAgeBucketLimits[N], and the final bucket counts
// older states. Repeated HID idle reports are excluded. JSON/map construction
// happens after this value has been copied and never while the scheduler lock
// is held.
type InputSchedulerSnapshot struct {
	Generation          uint64
	Received            uint64
	Selected            uint64
	TransitionDepth     int
	TransitionHighWater int
	ContinuousPending   bool
	ContinuousReplaced  uint64
	PeakUpgrades        uint64
	Overflows           uint64
	MaximumQueueAge     time.Duration
	QueueAgeBuckets     [len(inputQueueAgeBucketLimits) + 1]uint64
	MaximumSelectionAge time.Duration
	SelectionAgeBuckets [len(inputQueueAgeBucketLimits) + 1]uint64
}

// dualSenseInputScheduler is the sole owner of transition ordering, trigger
// epochs, presentation state, and the mutable HID encoder sequence. Producers
// only copy complete states under mu. Endpoint service selects and encodes one
// state under the same short critical section, so no queue slot or encoder
// buffer can be changed while it is being consumed.
type dualSenseInputScheduler struct {
	mu sync.Mutex

	edge bool // immutable report-layout variant selected at construction

	transitions                 [dualSenseInputTransitionCapacity]scheduledInputState
	head                        int
	count                       int
	latest                      scheduledInputState
	hasLatest                   bool
	retry                       scheduledInputState
	hasRetry                    bool
	retryReport                 [InputReportSize]byte
	retrySequence               uint8
	retryPacketSequence         uint32
	retryPresentationGeneration uint64
	hasRetryReport              bool

	claimed               scheduledInputState
	claimedReport         [InputReportSize]byte
	claimedSequence       uint8
	claimedPacketSequence uint32
	// claimRequiresOrderedRecovery is set when a later transition depends on
	// an uncommitted continuous claim to represent a trigger peak. The claim
	// remains immutable, but a failed send is then recovered ahead of that
	// transition exactly like ordered work.
	claimRequiresOrderedRecovery  bool
	claimToken                    uint64
	nextClaimToken                uint64
	claimedPresentationGeneration uint64
	hasClaim                      bool

	previous      InputState
	hasPrevious   bool
	lastSelected  scheduledInputState
	hasSelected   bool
	lastPresented scheduledInputState
	hasPresented  bool

	l2 inputTriggerEpoch
	r2 inputTriggerEpoch

	generation          uint64
	receiveOrdinal      uint64
	received            uint64
	selected            uint64
	highWater           int
	replaced            uint64
	peakUpgrades        uint64
	overflows           uint64
	maximumQueueAge     time.Duration
	queueAgeBuckets     [len(inputQueueAgeBucketLimits) + 1]uint64
	maximumSelectionAge time.Duration
	selectionAgeBuckets [len(inputQueueAgeBucketLimits) + 1]uint64

	sequence               uint8
	packetSequence         uint32
	timestampBase          time.Time
	lastReport             [InputReportSize]byte
	presentationVersion    uint64
	presentationGeneration uint64
	corruptReports         uint64
}

func neutralInputState() InputState {
	x, y, z := DefaultAccelRaw()
	return InputState{AccelX: x, AccelY: y, AccelZ: z}
}

func newDualSenseInputScheduler(battery byte, edge bool) *dualSenseInputScheduler {
	now := time.Now()
	neutral := neutralInputState()
	s := &dualSenseInputScheduler{
		edge:                   edge,
		generation:             1,
		presentationVersion:    1,
		presentationGeneration: 1,
		timestampBase:          now,
		lastSelected: scheduledInputState{
			state: neutral, receivedAt: now, generation: 1,
		},
		lastPresented: scheduledInputState{
			state: neutral, receivedAt: now, generation: 1,
		},
		hasSelected:  true,
		hasPresented: true,
	}
	encodeUSBInputReportInto(&neutral, battery, 0, 0, 0, edge,
		s.lastReport[:])
	return s
}

// beginReceiveGeneration displaces an older V5 socket reader without changing
// HID presentation ownership. States already accepted from that reader remain
// ordered work for the virtual endpoint, including an in-flight immutable
// claim. A TCP reconnect is not a USB device/configuration reset.
func (s *dualSenseInputScheduler) beginReceiveGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.generation++
	if s.generation == 0 {
		s.generation = 1
	}
	return s.generation
}

func (s *dualSenseInputScheduler) currentGeneration() uint64 {
	s.mu.Lock()
	generation := s.generation
	s.mu.Unlock()
	return generation
}

func (s *dualSenseInputScheduler) update(state *InputState, generation uint64) bool {
	return s.updateAt(state, generation, time.Now())
}

// updateAt keeps time injection test-only while making receive order an
// explicit scheduler property. Monotonic timestamps can legitimately compare
// equal; the ordinal remains strict and therefore owns ordering between
// independently observed trigger peaks.
func (s *dualSenseInputScheduler) updateAt(state *InputState, generation uint64,
	now time.Time) bool {
	next := neutralInputState()
	if state != nil {
		next = *state
	}
	// Analog actuation always implies the matching HID digital trigger bit.
	// A separately mapped digital trigger may remain asserted at zero analog,
	// so zero does not erase an explicit button bit.
	if next.L2 != 0 {
		next.Buttons |= ButtonL2
	} else if next.Buttons&ButtonL2 != 0 {
		// HID's analog and digital views must describe the same final mapped
		// state. Preserve an explicit mapped digital press with the smallest
		// non-zero analog actuation rather than serializing an impossible pair.
		next.L2 = 1
	}
	if next.R2 != 0 {
		next.Buttons |= ButtonR2
	} else if next.Buttons&ButtonR2 != 0 {
		next.R2 = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != 0 && generation != s.generation {
		return false
	}
	s.receiveOrdinal++
	if s.receiveOrdinal == 0 {
		s.receiveOrdinal = 1
	}
	receiveOrdinal := s.receiveOrdinal

	previous := s.previous
	hadPrevious := s.hasPrevious
	l2Press := hadPrevious && previous.L2 == 0 && next.L2 != 0
	l2Release := hadPrevious && previous.L2 != 0 && next.L2 == 0
	r2Press := hadPrevious && previous.R2 == 0 && next.R2 != 0
	r2Release := hadPrevious && previous.R2 != 0 && next.R2 == 0

	if !hadPrevious && next.L2 != 0 {
		l2Press = true
	}
	if !hadPrevious && next.R2 != 0 {
		r2Press = true
	}

	if l2Press {
		s.beginTriggerEpoch(&s.l2, next.L2, next, now, receiveOrdinal)
	} else if s.l2.active {
		s.observeTriggerEpoch(&s.l2, next.L2, next, now, receiveOrdinal, true)
	}
	if r2Press {
		s.beginTriggerEpoch(&s.r2, next.R2, next, now, receiveOrdinal)
	} else if s.r2.active {
		s.observeTriggerEpoch(&s.r2, next.R2, next, now, receiveOrdinal, false)
	}

	entry := scheduledInputState{
		state:          next,
		receivedAt:     now,
		generation:     s.generation,
		sampleQueueAge: true,
		l2Epoch:        activeEpochID(&s.l2, next.L2, l2Release),
		r2Epoch:        activeEpochID(&s.r2, next.R2, r2Release),
		l2Press:        l2Press,
		r2Press:        r2Press,
	}

	transition := inputStateHasTransition(previous, next, hadPrevious)
	if transition {
		// Preserve truthful peak snapshots before the next unrelated or release
		// transition makes an older continuous state stale. When both triggers
		// need a saved snapshot, retain the order in which the peaks occurred.
		if s.l2.active && s.r2.active &&
			s.r2.peakReceiveOrdinal < s.l2.peakReceiveOrdinal {
			s.preserveTriggerPeakBeforeTransition(&s.r2, false, &entry)
			s.preserveTriggerPeakBeforeTransition(&s.l2, true, &entry)
		} else {
			if s.l2.active {
				s.preserveTriggerPeakBeforeTransition(&s.l2, true, &entry)
			}
			if s.r2.active {
				s.preserveTriggerPeakBeforeTransition(&s.r2, false, &entry)
			}
		}
		s.enqueueTransition(entry)
	} else {
		if s.hasLatest {
			s.replaced++
		}
		s.latest = entry
		s.hasLatest = true
	}

	s.previous = next
	s.hasPrevious = true
	s.received++
	if l2Release {
		s.l2.active = false
	}
	if r2Release {
		s.r2.active = false
	}
	return true
}

func activeEpochID(epoch *inputTriggerEpoch, value uint8, release bool) uint64 {
	if epoch.active && (value != 0 || release) {
		return epoch.id
	}
	return 0
}

func (s *dualSenseInputScheduler) beginTriggerEpoch(epoch *inputTriggerEpoch,
	value uint8, state InputState, receivedAt time.Time, receiveOrdinal uint64) {
	epoch.id++
	if epoch.id == 0 {
		epoch.id = 1
	}
	epoch.active = true
	epoch.peak = value
	epoch.presentedPeak = 0
	epoch.presentedPeakState = InputState{}
	epoch.peakState = state
	epoch.peakReceivedAt = receivedAt
	epoch.peakReceiveOrdinal = receiveOrdinal
}

func (s *dualSenseInputScheduler) observeTriggerEpoch(epoch *inputTriggerEpoch,
	value uint8, state InputState, receivedAt time.Time, receiveOrdinal uint64,
	left bool) {
	if value > epoch.peak {
		epoch.peak = value
		epoch.peakState = state
		epoch.peakReceivedAt = receivedAt
		epoch.peakReceiveOrdinal = receiveOrdinal
	} else if value == epoch.peak && value != 0 {
		// Physical trigger mechanics can settle one report after the analog
		// maximum. Resonance, for example, advances L2 status 0x28 -> 0x29 at
		// analog 255. Keep only this trigger's newer coupled status; changing the
		// peak's receive ordinal or unrelated state would invent ordering.
		if physicalMetadataLayoutsMatch(&epoch.peakState, &state) {
			copyTriggerPhysicalStatus(&epoch.peakState, &state, left)
		} else {
			// A validity/layout boundary cannot be partially merged. Preserve the
			// later complete peak as its own truthful snapshot so release handling
			// can order it after any pending state from the former layout.
			epoch.peakState = state
			epoch.peakReceivedAt = receivedAt
			epoch.peakReceiveOrdinal = receiveOrdinal
		}
	}
}

func setTriggerPeak(state *InputState, peakState *InputState, left bool,
	peak uint8) {
	if left {
		state.L2 = peak
		if peak != 0 {
			state.Buttons |= ButtonL2
		}
	} else {
		state.R2 = peak
		if peak != 0 {
			state.Buttons |= ButtonR2
		}
	}
	if peakState != nil {
		copyTriggerPhysicalStatus(state, peakState, left)
	}
}

// copyTriggerPhysicalStatus couples analog peak strengthening to only the
// physical status owned by that trigger. The opposite status nibble and every
// other same-report observation remain from the pending complete state.
func copyTriggerPhysicalStatus(destination, source *InputState, left bool) {
	if destination == nil || source == nil ||
		!destination.PhysicalMetadataValid || !source.PhysicalMetadataValid ||
		destination.PhysicalMetadataEdgeLayout !=
			source.PhysicalMetadataEdgeLayout {
		return
	}
	if left {
		destination.PhysicalInputMetadata[physicalMetadataL2Status] =
			source.PhysicalInputMetadata[physicalMetadataL2Status]
		destination.PhysicalInputMetadata[physicalMetadataEffectStatus] =
			(destination.PhysicalInputMetadata[physicalMetadataEffectStatus] & 0x0F) |
				(source.PhysicalInputMetadata[physicalMetadataEffectStatus] & 0xF0)
		return
	}
	destination.PhysicalInputMetadata[physicalMetadataR2Status] =
		source.PhysicalInputMetadata[physicalMetadataR2Status]
	destination.PhysicalInputMetadata[physicalMetadataEffectStatus] =
		(destination.PhysicalInputMetadata[physicalMetadataEffectStatus] & 0xF0) |
			(source.PhysicalInputMetadata[physicalMetadataEffectStatus] & 0x0F)
}

func (s *dualSenseInputScheduler) preserveTriggerPeakBeforeTransition(
	epoch *inputTriggerEpoch, left bool, upcoming *scheduledInputState) {
	if !epoch.active || epoch.peak == 0 ||
		(epoch.presentedPeak >= epoch.peak &&
			triggerPhysicalStatusMatches(
				&epoch.presentedPeakState, &epoch.peakState, left)) {
		return
	}
	if upcoming != nil && entryRepresentsTruthfulPeak(upcoming, epoch, left) {
		return
	}

	// A claimed report is immutable and is not presented until completion owns
	// the response. If it carries the meaningful peak, make a failed completion
	// recover it ahead of the upcoming transition. This avoids an unnecessary
	// duplicate when the claim succeeds while preserving the peak on unlink or
	// a pre-send socket failure.
	if s.hasClaim && entryRepresentsTruthfulPeak(&s.claimed, epoch, left) {
		s.claimRequiresOrderedRecovery = true
		return
	}

	// A failed transition is immutable recovery work. It may satisfy this peak
	// only when the exact logical report already represents it; otherwise the
	// saved complete peak is queued behind the retry and before the release.
	if s.hasRetry && entryRepresentsTruthfulPeak(&s.retry, epoch, left) {
		return
	}
	for offset := 0; offset < s.count; offset++ {
		index := (s.head + offset) % len(s.transitions)
		pending := &s.transitions[index]
		if entryRepresentsTruthfulPeak(pending, epoch, left) {
			return
		}
	}

	// The initial press is already ordered. While it remains in the transition
	// ring and has never been claimed, it is the safest place to preserve an
	// otherwise unrepresented peak because changing only this trigger cannot
	// reorder or invent an unrelated transition. Check every immutable/exact
	// representation first so a later truthful button state is never folded
	// backward into the initial report. Retry storage is deliberately excluded:
	// a failed serialized transition must be retried as the same logical report.
	for offset := 0; offset < s.count; offset++ {
		index := (s.head + offset) % len(s.transitions)
		pending := &s.transitions[index]
		matches := pending.l2Epoch == epoch.id && pending.l2Press
		if !left {
			matches = pending.r2Epoch == epoch.id && pending.r2Press
		}
		if matches && pendingInitialCanAnchorPeak(pending, epoch, left) {
			if offset != s.count-1 &&
				!triggerPhysicalStatusMatches(
					&pending.state, &epoch.peakState, left) {
				// Later ordered controls already follow this initial press. Do
				// not move a newly settled physical status ahead of them: the
				// latest complete peak is promoted after those transitions.
				break
			}
			before := triggerValue(&pending.state, left)
			setTriggerPeak(&pending.state, &epoch.peakState, left, epoch.peak)
			if before < epoch.peak {
				s.peakUpgrades++
			}
			markPendingPeakAnchored(pending, left)
			return
		}
	}

	// A continuous peak would otherwise be discarded when the release is
	// enqueued. Promote it to ordered work immediately before that release.
	if s.hasLatest && entryRepresentsPeak(&s.latest, epoch.id, epoch.peak, left) &&
		physicalMetadataLayoutsMatch(&s.latest.state, &epoch.peakState) {
		copyTriggerPhysicalStatus(&s.latest.state, &epoch.peakState, left)
		peak := s.latest
		s.hasLatest = false
		s.latest = scheduledInputState{}
		s.enqueueTransition(peak)
		return
	}

	peak := scheduledInputState{
		state:          epoch.peakState,
		receivedAt:     epoch.peakReceivedAt,
		generation:     s.generation,
		sampleQueueAge: true,
	}
	if left {
		peak.l2Epoch = epoch.id
		if s.r2.active && peak.state.R2 != 0 {
			peak.r2Epoch = s.r2.id
		}
	} else {
		peak.r2Epoch = epoch.id
		if s.l2.active && peak.state.L2 != 0 {
			peak.l2Epoch = s.l2.id
		}
	}
	s.enqueueTransition(peak)
}

// pendingInitialCanAnchorPeak enforces complete-state truthfulness when both
// triggers began in one ordered report. Strengthening one trigger on that
// report is explicitly permitted. Strengthening the other as well is safe
// only if the second trigger's actual peak snapshot contained the already
// anchored value; otherwise the saved snapshot must be queued separately in
// receive order.
func pendingInitialCanAnchorPeak(pending *scheduledInputState,
	epoch *inputTriggerEpoch, left bool) bool {
	if !physicalMetadataLayoutsMatch(&pending.state, &epoch.peakState) {
		return false
	}
	if left {
		return !pending.r2PeakAnchored ||
			(pending.state.R2 == epoch.peakState.R2 &&
				oppositeTriggerPhysicalStatusMatches(
					&pending.state, &epoch.peakState, true))
	}
	return !pending.l2PeakAnchored ||
		(pending.state.L2 == epoch.peakState.L2 &&
			oppositeTriggerPhysicalStatusMatches(
				&pending.state, &epoch.peakState, false))
}

func oppositeTriggerPhysicalStatusMatches(a, b *InputState, left bool) bool {
	if !physicalMetadataLayoutsMatch(a, b) {
		return false
	}
	if !a.PhysicalMetadataValid {
		return true
	}
	if left {
		return a.PhysicalInputMetadata[physicalMetadataR2Status] ==
			b.PhysicalInputMetadata[physicalMetadataR2Status] &&
			a.PhysicalInputMetadata[physicalMetadataEffectStatus]&0x0F ==
				b.PhysicalInputMetadata[physicalMetadataEffectStatus]&0x0F
	}
	return a.PhysicalInputMetadata[physicalMetadataL2Status] ==
		b.PhysicalInputMetadata[physicalMetadataL2Status] &&
		a.PhysicalInputMetadata[physicalMetadataEffectStatus]&0xF0 ==
			b.PhysicalInputMetadata[physicalMetadataEffectStatus]&0xF0
}

func markPendingPeakAnchored(pending *scheduledInputState, left bool) {
	if left {
		pending.l2PeakAnchored = true
		return
	}
	pending.r2PeakAnchored = true
}

func triggerValue(state *InputState, left bool) uint8 {
	if left {
		return state.L2
	}
	return state.R2
}

func entryRepresentsPeak(entry *scheduledInputState, epochID uint64,
	peak uint8, left bool) bool {
	if left {
		return entry.l2Epoch == epochID && entry.state.L2 >= peak
	}
	return entry.r2Epoch == epochID && entry.state.R2 >= peak
}

func entryRepresentsTruthfulPeak(entry *scheduledInputState,
	epoch *inputTriggerEpoch, left bool) bool {
	return entryRepresentsPeak(entry, epoch.id, epoch.peak, left) &&
		triggerPhysicalStatusMatches(&entry.state, &epoch.peakState, left)
}

func triggerPhysicalStatusMatches(a, b *InputState, left bool) bool {
	if !physicalMetadataLayoutsMatch(a, b) {
		return false
	}
	if !a.PhysicalMetadataValid {
		return true
	}
	if left {
		return a.PhysicalInputMetadata[physicalMetadataL2Status] ==
			b.PhysicalInputMetadata[physicalMetadataL2Status] &&
			a.PhysicalInputMetadata[physicalMetadataEffectStatus]&0xF0 ==
				b.PhysicalInputMetadata[physicalMetadataEffectStatus]&0xF0
	}
	return a.PhysicalInputMetadata[physicalMetadataR2Status] ==
		b.PhysicalInputMetadata[physicalMetadataR2Status] &&
		a.PhysicalInputMetadata[physicalMetadataEffectStatus]&0x0F ==
			b.PhysicalInputMetadata[physicalMetadataEffectStatus]&0x0F
}

func physicalMetadataLayoutsMatch(a, b *InputState) bool {
	if a == nil || b == nil ||
		a.PhysicalMetadataValid != b.PhysicalMetadataValid {
		return false
	}
	return !a.PhysicalMetadataValid ||
		a.PhysicalMetadataEdgeLayout == b.PhysicalMetadataEdgeLayout
}

func (s *dualSenseInputScheduler) enqueueTransition(entry scheduledInputState) {
	// Continuous state older than this ordered boundary can never be selected
	// after it without replaying stale motion or a stale trigger level.
	s.latest = scheduledInputState{}
	s.hasLatest = false
	entry.ordered = true
	if s.count == len(s.transitions) {
		s.overflows++
		// Preserve all already accepted transitions. The newest complete state is
		// retained as bounded recovery state so the controller cannot remain
		// indefinitely stuck after the ordered ring drains.
		s.latest = entry
		s.hasLatest = true
		return
	}
	index := (s.head + s.count) % len(s.transitions)
	s.transitions[index] = entry
	s.count++
	if s.count > s.highWater {
		s.highWater = s.count
	}
}

func (s *dualSenseInputScheduler) selectState(now time.Time) scheduledInputState {
	var selected scheduledInputState
	if s.hasRetry {
		selected = s.retry
		s.retry = scheduledInputState{}
		s.hasRetry = false
	} else if s.count != 0 {
		selected = s.transitions[s.head]
		s.transitions[s.head] = scheduledInputState{}
		s.head = (s.head + 1) % len(s.transitions)
		s.count--
	} else if s.hasLatest {
		selected = s.latest
		s.latest = scheduledInputState{}
		s.hasLatest = false
	} else if s.hasPresented {
		selected = s.lastPresented
		selected.receivedAt = now
		selected.sampleQueueAge = false
	} else {
		selected = scheduledInputState{
			state: neutralInputState(), receivedAt: now, generation: s.generation,
		}
	}
	if selected.sampleQueueAge && !selected.selectionLatencySampled {
		s.recordSelectedQueueAge(now, selected.receivedAt)
		selected.selectionLatencySampled = true
	}

	s.lastSelected = selected
	s.hasSelected = true
	s.selected++
	return selected
}

func (s *dualSenseInputScheduler) beginClaim(now time.Time, battery byte,
	destination []byte) (int, uint64) {
	if len(destination) < InputReportSize || s.hasClaim {
		return 0, 0
	}
	retryingSerializedReport := s.hasRetry && s.hasRetryReport
	selected := s.selectState(now)
	sequence := s.sequence + 1
	packetSequence := s.packetSequence + 1
	if retryingSerializedReport {
		sequence = s.retrySequence
		packetSequence = s.retryPacketSequence
		copy(s.claimedReport[:], s.retryReport[:])
		clear(s.retryReport[:])
		s.retrySequence = 0
		s.retryPacketSequence = 0
		s.retryPresentationGeneration = 0
		s.hasRetryReport = false
	} else {
		timestamp := dualSenseTimestampTicks(s.timestampBase, now)
		if !encodeUSBInputReportInto(&selected.state, battery, sequence,
			packetSequence, timestamp, s.edge, s.claimedReport[:]) {
			s.corruptReports++
		}
	}
	s.nextClaimToken++
	if s.nextClaimToken == 0 {
		s.nextClaimToken = 1
	}
	s.claimed = selected
	s.claimedSequence = sequence
	s.claimedPacketSequence = packetSequence
	s.claimToken = s.nextClaimToken
	s.claimedPresentationGeneration = s.presentationGeneration
	s.hasClaim = true
	copy(destination[:InputReportSize], s.claimedReport[:])
	return InputReportSize, s.claimToken
}

func dualSenseTimestampTicks(base, now time.Time) uint32 {
	elapsed := now.Sub(base)
	if elapsed <= 0 {
		return 0
	}
	// One tick is one third of a microsecond. A time.Duration cannot exceed
	// MaxInt64 nanoseconds, so converting positive whole microseconds to uint64
	// and multiplying by three cannot overflow. The final conversion is the
	// controller clock's intentional modulo-2^32 wrap.
	microseconds := uint64(elapsed / time.Microsecond)
	return uint32(microseconds * 3)
}

func (s *dualSenseInputScheduler) completeClaimAt(token uint64, presented bool,
	completedAt time.Time) {
	outcome := inputpresentation.OutcomeDefer
	if presented {
		outcome = inputpresentation.OutcomeCommit
	}
	s.resolveClaimAt(token, s.claimedPresentationGeneration, outcome, completedAt)
}

func (s *dualSenseInputScheduler) resolveClaimAt(token, generation uint64,
	outcome inputpresentation.Outcome, completedAt time.Time) bool {
	if !outcome.Valid() || !s.hasClaim || token == 0 ||
		token != s.claimToken || generation == 0 ||
		generation != s.claimedPresentationGeneration ||
		generation != s.presentationGeneration {
		return false
	}
	if outcome == inputpresentation.OutcomeRetire {
		s.collapsePresentationGeneration(completedAt)
		return true
	}
	claimed := s.claimed
	if outcome == inputpresentation.OutcomeCommit {
		s.sequence = s.claimedSequence
		s.packetSequence = s.claimedPacketSequence
		s.lastPresented = claimed
		s.hasPresented = true
		copy(s.lastReport[:], s.claimedReport[:])
		s.presentationVersion++
		if s.presentationVersion == 0 {
			s.presentationVersion = 1
		}
		if claimed.l2Epoch == s.l2.id && claimed.state.L2 > s.l2.presentedPeak {
			s.l2.presentedPeak = claimed.state.L2
			s.l2.presentedPeakState = claimed.state
		} else if claimed.l2Epoch == s.l2.id &&
			claimed.state.L2 == s.l2.presentedPeak {
			copyTriggerPhysicalStatus(&s.l2.presentedPeakState,
				&claimed.state, true)
		}
		if claimed.r2Epoch == s.r2.id && claimed.state.R2 > s.r2.presentedPeak {
			s.r2.presentedPeak = claimed.state.R2
			s.r2.presentedPeakState = claimed.state
		} else if claimed.r2Epoch == s.r2.id &&
			claimed.state.R2 == s.r2.presentedPeak {
			copyTriggerPhysicalStatus(&s.r2.presentedPeakState,
				&claimed.state, false)
		}
		if claimed.sampleQueueAge {
			s.recordPresentedQueueAge(completedAt, claimed.receivedAt)
		}
	} else if outcome == inputpresentation.OutcomeDefer &&
		(claimed.ordered || s.claimRequiresOrderedRecovery) {
		// Failed ordered work has a dedicated immutable recovery lane ahead of
		// every subsequently accepted transition. The ring may have refilled
		// while the response waited for send ownership, so it cannot safely be
		// pushed back into that ring.
		s.retry = claimed
		// A continuous claim promoted to an ordering dependency must remain
		// byte-exact recovery work across repeated downstream deferrals.
		s.retry.ordered = true
		s.hasRetry = true
		copy(s.retryReport[:], s.claimedReport[:])
		s.retrySequence = s.claimedSequence
		s.retryPacketSequence = s.claimedPacketSequence
		s.retryPresentationGeneration = generation
		s.hasRetryReport = true
	} else if outcome == inputpresentation.OutcomeDefer &&
		!s.hasRetry && s.count == 0 && !s.hasLatest {
		// Continuous work is replaceable. Restore it only when no newer state or
		// contradictory ordered boundary arrived while it was claimed. Keep it
		// replaceable: a later edge must supersede this stale state instead of
		// waiting behind it for another downstream interrupt request. Only
		// ordered work belongs in the byte-exact retry lane above.
		s.latest = claimed
		s.hasLatest = true
	}
	s.claimed = scheduledInputState{}
	s.claimedSequence = 0
	s.claimedPacketSequence = 0
	s.claimRequiresOrderedRecovery = false
	s.claimToken = 0
	s.claimedPresentationGeneration = 0
	s.hasClaim = false
	return true
}

func (s *dualSenseInputScheduler) retirePresentationGeneration(
	generation uint64, retiredAt time.Time) bool {
	if generation == 0 || generation != s.presentationGeneration {
		return false
	}
	s.collapsePresentationGeneration(retiredAt)
	return true
}

// collapsePresentationGeneration discards the retiring transport's historical
// edges and immutable bytes, then carries only the newest complete semantic
// state into the successor. A queued press+release therefore becomes one
// current released snapshot instead of a phantom tap after reattach.
func (s *dualSenseInputScheduler) collapsePresentationGeneration(
	retiredAt time.Time,
) {
	if retiredAt.IsZero() {
		retiredAt = time.Now()
	}
	clear(s.transitions[:])
	s.head = 0
	s.count = 0
	s.retry = scheduledInputState{}
	s.hasRetry = false
	clear(s.retryReport[:])
	s.retrySequence = 0
	s.retryPacketSequence = 0
	s.retryPresentationGeneration = 0
	s.hasRetryReport = false

	s.claimed = scheduledInputState{}
	clear(s.claimedReport[:])
	s.claimedSequence = 0
	s.claimedPacketSequence = 0
	s.claimRequiresOrderedRecovery = false
	s.claimToken = 0
	s.claimedPresentationGeneration = 0
	s.hasClaim = false

	current := neutralInputState()
	if s.hasPrevious {
		current = s.previous
	}
	s.receiveOrdinal++
	if s.receiveOrdinal == 0 {
		s.receiveOrdinal = 1
	}
	snapshot := scheduledInputState{
		state: current, receivedAt: retiredAt, generation: s.generation,
	}
	snapshot.l2Epoch = s.restartPresentationTriggerEpoch(
		&s.l2, current.L2, current, retiredAt)
	snapshot.r2Epoch = s.restartPresentationTriggerEpoch(
		&s.r2, current.R2, current, retiredAt)
	s.latest = snapshot
	s.hasLatest = true
	s.advancePresentationGeneration()
}

// refreshPresentationSnapshot replaces EP0's cached GET_REPORT bytes at a
// backend lifecycle boundary without consuming the successor's first
// interrupt claim or advancing committed encoder counters. The preview uses
// the same next sequence numbers that an interrupt claim would use; its
// timestamp is sampled at retirement.
func (s *dualSenseInputScheduler) refreshPresentationSnapshot(
	battery byte, refreshedAt time.Time,
) {
	if refreshedAt.IsZero() {
		refreshedAt = time.Now()
	}
	current := neutralInputState()
	if s.hasPrevious {
		current = s.previous
	}
	timestamp := dualSenseTimestampTicks(s.timestampBase, refreshedAt)
	if !encodeUSBInputReportInto(&current, battery, s.sequence+1,
		s.packetSequence+1, timestamp, s.edge, s.lastReport[:]) {
		s.corruptReports++
	}
	s.presentationVersion++
	if s.presentationVersion == 0 {
		s.presentationVersion = 1
	}
}

func (s *dualSenseInputScheduler) restartPresentationTriggerEpoch(
	epoch *inputTriggerEpoch, value uint8, state InputState,
	receivedAt time.Time,
) uint64 {
	id := epoch.id + 1
	if id == 0 {
		id = 1
	}
	*epoch = inputTriggerEpoch{id: id}
	if value == 0 {
		return 0
	}
	epoch.active = true
	epoch.peak = value
	epoch.peakState = state
	epoch.peakReceivedAt = receivedAt
	epoch.peakReceiveOrdinal = s.receiveOrdinal
	return id
}

func (s *dualSenseInputScheduler) advancePresentationGeneration() {
	s.presentationGeneration++
	if s.presentationGeneration == 0 {
		s.presentationGeneration = 1
	}
}

func (s *dualSenseInputScheduler) recordSelectedQueueAge(selectedAt,
	receivedAt time.Time) {
	age := selectedAt.Sub(receivedAt)
	if age < 0 {
		age = 0
	}
	if age > s.maximumSelectionAge {
		s.maximumSelectionAge = age
	}
	bucket := len(inputQueueAgeBucketLimits)
	for index, limit := range inputQueueAgeBucketLimits {
		if age <= limit {
			bucket = index
			break
		}
	}
	s.selectionAgeBuckets[bucket]++
}

func (s *dualSenseInputScheduler) recordPresentedQueueAge(presentedAt,
	receivedAt time.Time) {
	age := presentedAt.Sub(receivedAt)
	if age < 0 {
		age = 0
	}
	if age > s.maximumQueueAge {
		s.maximumQueueAge = age
	}
	bucket := len(inputQueueAgeBucketLimits)
	for index, limit := range inputQueueAgeBucketLimits {
		if age <= limit {
			bucket = index
			break
		}
	}
	s.queueAgeBuckets[bucket]++
}

func (s *dualSenseInputScheduler) snapshot() InputSchedulerSnapshot {
	s.mu.Lock()
	snapshot := InputSchedulerSnapshot{
		Generation:          s.generation,
		Received:            s.received,
		Selected:            s.selected,
		TransitionDepth:     s.count + boolInt(s.hasRetry),
		TransitionHighWater: s.highWater,
		ContinuousPending:   s.hasLatest,
		ContinuousReplaced:  s.replaced,
		PeakUpgrades:        s.peakUpgrades,
		Overflows:           s.overflows,
		MaximumQueueAge:     s.maximumQueueAge,
		QueueAgeBuckets:     s.queueAgeBuckets,
		MaximumSelectionAge: s.maximumSelectionAge,
		SelectionAgeBuckets: s.selectionAgeBuckets,
	}
	s.mu.Unlock()
	return snapshot
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func inputStateHasTransition(previous, next InputState, hasPrevious bool) bool {
	if !hasPrevious {
		return inputStateNonNeutral(next)
	}
	if previous.Buttons != next.Buttons || previous.DPad != next.DPad ||
		(previous.L2 == 0) != (next.L2 == 0) ||
		(previous.R2 == 0) != (next.R2 == 0) {
		return true
	}
	if touchStateTransition(previous.Touch1Active, previous.Touch1Tracking,
		next.Touch1Active, next.Touch1Tracking) {
		return true
	}
	return touchStateTransition(previous.Touch2Active, previous.Touch2Tracking,
		next.Touch2Active, next.Touch2Tracking)
}

func touchStateTransition(previousActive bool, previousTracking uint8,
	nextActive bool, nextTracking uint8) bool {
	return previousActive != nextActive ||
		(previousActive && nextActive && previousTracking != nextTracking)
}

func inputStateNonNeutral(state InputState) bool {
	neutral := neutralInputState()
	return state.LX != 0 || state.LY != 0 || state.RX != 0 || state.RY != 0 ||
		state.Buttons != 0 || state.DPad != 0 || state.L2 != 0 || state.R2 != 0 ||
		state.Touch1Active || state.Touch2Active ||
		state.GyroX != 0 || state.GyroY != 0 || state.GyroZ != 0 ||
		state.AccelX != neutral.AccelX || state.AccelY != neutral.AccelY ||
		state.AccelZ != neutral.AccelZ
}

func encodeUSBInputReportInto(state *InputState, battery, sequence uint8,
	packetSequence, timestamp uint32, edge bool, destination []byte) bool {
	if len(destination) < InputReportSize {
		return false
	}
	b := destination[:InputReportSize]
	clear(b)
	if state == nil {
		neutral := neutralInputState()
		state = &neutral
	}
	b[0] = ReportIDInput
	b[1] = uint8(int16(state.LX) + 128)
	b[2] = uint8(int16(state.LY) + 128)
	b[3] = uint8(int16(state.RX) + 128)
	b[4] = uint8(int16(state.RY) + 128)
	b[5] = state.L2
	b[6] = state.R2
	b[7] = sequence

	usbDPad := uint8(DPadUSBNeutral)
	switch {
	case state.DPad&DPadUp != 0 && state.DPad&DPadRight != 0:
		usbDPad = DPadUSBUpRight
	case state.DPad&DPadUp != 0 && state.DPad&DPadLeft != 0:
		usbDPad = DPadUSBUpLeft
	case state.DPad&DPadDown != 0 && state.DPad&DPadRight != 0:
		usbDPad = DPadUSBDownRight
	case state.DPad&DPadDown != 0 && state.DPad&DPadLeft != 0:
		usbDPad = DPadUSBDownLeft
	case state.DPad&DPadUp != 0:
		usbDPad = DPadUSBUp
	case state.DPad&DPadDown != 0:
		usbDPad = DPadUSBDown
	case state.DPad&DPadLeft != 0:
		usbDPad = DPadUSBLeft
	case state.DPad&DPadRight != 0:
		usbDPad = DPadUSBRight
	}
	b[8] = (usbDPad & DPadMask) | (uint8(state.Buttons) & 0xF0)
	b[9] = uint8(state.Buttons >> 8)
	b[10] = uint8(state.Buttons >> 16)
	binary.LittleEndian.PutUint32(b[12:16], packetSequence)

	binary.LittleEndian.PutUint16(b[16:18], uint16(state.GyroX))
	binary.LittleEndian.PutUint16(b[18:20], uint16(state.GyroY))
	binary.LittleEndian.PutUint16(b[20:22], uint16(state.GyroZ))
	binary.LittleEndian.PutUint16(b[22:24], uint16(state.AccelX))
	binary.LittleEndian.PutUint16(b[24:26], uint16(state.AccelY))
	binary.LittleEndian.PutUint16(b[26:28], uint16(state.AccelZ))
	b[33] = normalizeTouchTracking(state.Touch1Active, state.Touch1Tracking)
	encodeTouchCoords(b[34:37], state.Touch1X, state.Touch1Y)
	b[37] = normalizeTouchTracking(state.Touch2Active, state.Touch2Tracking)
	encodeTouchCoords(b[38:41], state.Touch2X, state.Touch2Y)
	encodeUSBInputMetadata(b, state, timestamp, edge, battery)

	if inputStateControlsInvalid(state) {
		resetUSBInputReportToNeutral(b, sequence, packetSequence, timestamp,
			battery, edge, nil)
		return false
	}
	return true
}

func encodeUSBInputMetadata(report []byte, state *InputState, timestamp uint32,
	edge bool, battery byte) {
	binary.LittleEndian.PutUint32(report[28:32], timestamp)
	// A physical USB DualSense at rest reports both adaptive-trigger arms at
	// position nine with no active effect. Zero is not a truthful fallback and
	// has observable compatibility consequences in raw-input games.
	report[42] = dualSenseTriggerStatusNeutral
	report[43] = dualSenseTriggerStatusNeutral
	report[48] = 0
	encodeUSBInputStatus(report, timestamp, edge)
	report[53] = battery
	report[54] = dualSenseUSBConnectStateWired
	if state == nil || !state.PhysicalMetadataValid {
		return
	}
	binary.LittleEndian.PutUint32(report[28:32],
		state.PhysicalSensorTimestamp)
	copy(report[41:49], state.PhysicalInputMetadata[:8])
	// Base and Edge interpret bytes 49:53 differently. Preserve the physical
	// values only for a matching virtual layout; otherwise the target-specific
	// fallback written above remains authoritative.
	if state.PhysicalMetadataEdgeLayout == edge {
		copy(report[49:53], state.PhysicalInputMetadata[8:12])
	}
	report[53] = state.PhysicalInputMetadata[physicalMetadataBattery]
	// The virtual device is always presented over USB even when DS4Windows
	// normalized the authoritative metadata from a physical Bluetooth report.
	report[54] = dualSenseUSBConnectStateWired
	// Byte 55 is the third non-authenticated controller-status byte
	// (external-microphone / haptics low-pass state), common to base and Edge.
	report[55] = state.PhysicalInputMetadata[physicalMetadataHeadsetStatus]
}

func encodeUSBInputStatus(report []byte, timestamp uint32, edge bool) {
	if edge {
		// The Edge uses the four bytes that the base DualSense exposes as
		// Timer2 for profile/trigger-module status. Normal USB mode is profile
		// 0x80 with zero trigger level and module-loss padding.
		report[49] = dualSenseEdgeActiveProfile
		clear(report[50:53])
		return
	}
	binary.LittleEndian.PutUint32(report[49:53], timestamp)
}

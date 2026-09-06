package controllerfeedback

import "sync"

// PublicationOrigin identifies one fixed canonical-feedback publisher slot.
// Numeric order is deterministic winner priority: explicit test output is
// highest, then native game output, audio-derived effects, and profile effects.
// Release is a lifecycle transition and never a competing source.
type PublicationOrigin uint8

const (
	PublicationOriginInvalid       PublicationOrigin = 0
	PublicationOriginProfileEffect PublicationOrigin = 1
	PublicationOriginAudioHaptics  PublicationOrigin = 2
	PublicationOriginNativeGame    PublicationOrigin = 3
	PublicationOriginTestPreview   PublicationOrigin = 4
)

// Valid reports whether origin names one fixed publisher slot.
func (origin PublicationOrigin) Valid() bool {
	return origin >= PublicationOriginProfileEffect &&
		origin <= PublicationOriginTestPreview
}

// Publication is one typed publisher update. Each origin owns one replaceable
// ordering watermark. Frame remains the authoritative virtual-device source
// and lease identity.
type Publication struct {
	Origin PublicationOrigin
	Frame  Frame
}

// NewPublication builds one validated publisher update.
func NewPublication(origin PublicationOrigin, frame Frame) (Publication, bool) {
	publication := Publication{Origin: origin, Frame: frame}
	return publication, publication.Valid()
}

// Valid reports whether publication and its complete CFBK frame are valid.
func (publication Publication) Valid() bool {
	return publication.Origin.Valid() && publication.Frame.Valid()
}

// DeliveryDisposition identifies the transport-neutral action reserved for a
// sole physical writer. DeliveryStop contains no protocol bytes; it is an
// idempotent obligation to zero all four canonical actuators for the exact
// delivery epoch.
type DeliveryDisposition uint8

const (
	DeliveryNone DeliveryDisposition = iota
	DeliveryFrame
	DeliveryStop
)

// Delivery is one immutable writer reservation. A Stop carries its explicit
// origin, target generations, and delivery epoch while Frame remains zero.
type Delivery struct {
	Disposition         DeliveryDisposition
	Origin              PublicationOrigin
	Frame               Frame
	DeviceGeneration    uint64
	TransportGeneration uint64
	DeliveryEpoch       uint64
}

// Valid reports whether delivery is a complete frame or logical all-zero stop.
func (delivery Delivery) Valid() bool {
	if !delivery.Origin.Valid() || delivery.DeviceGeneration == 0 ||
		delivery.TransportGeneration == 0 || delivery.DeliveryEpoch == 0 {
		return false
	}
	if delivery.Disposition == DeliveryFrame {
		return delivery.Frame.Valid() &&
			delivery.Frame.DeviceGeneration == delivery.DeviceGeneration &&
			delivery.Frame.TransportGeneration == delivery.TransportGeneration
	}
	return delivery.Disposition == DeliveryStop && delivery.Frame == (Frame{})
}

type publicationSlot struct {
	publication Publication
	hasValue    bool
}

// WriterLease is one generation-bound sole-writer lease. Acquisition is an
// infrequent allocation; publication, selection, claim, admission, and
// completion allocate nothing after warm-up. A lease must not be copied.
type WriterLease struct {
	owner                  *Runtime
	self                   *WriterLease
	WriterGeneration       uint64
	DeviceGeneration       uint64
	TransportGeneration    uint64
	nextClaimToken         uint64
	inFlightClaimToken     uint64
	inFlightEventRevision  uint64
	inFlightDelivery       Delivery
	inFlightAdmitted       bool
	completedEventRevision uint64
	active                 bool
}

// Runtime performs fixed-slot, backend-independent canonical feedback
// arbitration. It invokes no callbacks, performs no translation, and does no
// I/O. The zero value is ready for use.
//
// Claim selects the highest-priority live source in the newest observed device
// and transport generation. After a frame reaches final admission, replacement
// or expiry yields one logical Stop for that delivery epoch before a successor
// becomes eligible. Failed completion retries the same Stop value and epoch
// with a new token. Exactly one writer lease can be active; final admission
// pins its generation until Complete.
type Runtime struct {
	mu sync.Mutex

	profileSlot publicationSlot
	audioSlot   publicationSlot
	gameSlot    publicationSlot
	previewSlot publicationSlot

	activeWriter         *WriterLease
	nextWriterGeneration uint64

	hasOwner             bool
	stopping             bool
	ownerMayHaveActuated bool
	owner                Publication
	ownerDeliveryEpoch   uint64
	nextDeliveryEpoch    uint64

	hasEvent             bool
	currentEvent         Delivery
	currentEventRevision uint64
}

// Publish copies a valid update into its fixed origin slot when it advances
// that slot's generation/epoch/sequence ordering watermark.
func (runtime *Runtime) Publish(publication Publication) bool {
	if runtime == nil || !publication.Valid() {
		return false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	slot := runtime.slot(publication.Origin)
	if slot.hasValue && !newerRuntimeFrame(publication.Frame,
		slot.publication.Frame) {
		return false
	}
	slot.publication = publication
	slot.hasValue = true
	return true
}

// AcquireWriter acquires the sole writer for one exact device and transport
// generation. It fails while another writer is active or generation space is
// exhausted.
func (runtime *Runtime) AcquireWriter(deviceGeneration,
	transportGeneration uint64) (*WriterLease, bool) {
	if runtime == nil || deviceGeneration == 0 || transportGeneration == 0 {
		return nil, false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.activeWriter != nil ||
		runtime.nextWriterGeneration == ^uint64(0) ||
		(runtime.hasEvent &&
			(runtime.currentEvent.DeviceGeneration != deviceGeneration ||
				runtime.currentEvent.TransportGeneration != transportGeneration)) {
		return nil, false
	}
	runtime.nextWriterGeneration++
	writer := &WriterLease{
		owner: runtime, WriterGeneration: runtime.nextWriterGeneration,
		DeviceGeneration:    deviceGeneration,
		TransportGeneration: transportGeneration, active: true,
	}
	writer.self = writer
	runtime.activeWriter = writer
	return writer, true
}

// RetireWriter retires an unadmitted writer generation. Admitted work must
// complete first because its external effect may already be underway. An
// unadmitted reservation is discarded and stays pending for a successor writer
// of the same target generation.
func (runtime *Runtime) RetireWriter(writer *WriterLease) bool {
	if runtime == nil {
		return false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !runtime.currentWriter(writer) || writer.inFlightAdmitted {
		return false
	}
	clearRuntimeClaim(writer)
	writer.active = false
	runtime.activeWriter = nil
	return true
}

// Claim reserves the current event for the exact active writer generation.
// It returns DeliveryNone when there is no eligible event or the writer target
// does not match the event target.
func (runtime *Runtime) Claim(nowMicroseconds uint64,
	writer *WriterLease) (Delivery, DeliveryDisposition, uint64) {
	if runtime == nil {
		return Delivery{}, DeliveryNone, 0
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !runtime.currentWriter(writer) {
		return Delivery{}, DeliveryNone, 0
	}
	runtime.reevaluate(nowMicroseconds)
	if !runtime.hasEvent || writer.inFlightClaimToken != 0 ||
		writer.completedEventRevision == runtime.currentEventRevision ||
		writer.DeviceGeneration != runtime.currentEvent.DeviceGeneration ||
		writer.TransportGeneration != runtime.currentEvent.TransportGeneration {
		return Delivery{}, DeliveryNone, 0
	}
	token := writer.nextClaimToken + 1
	if token == 0 {
		token = 1
	}
	writer.nextClaimToken = token
	writer.inFlightClaimToken = token
	writer.inFlightEventRevision = runtime.currentEventRevision
	writer.inFlightDelivery = runtime.currentEvent
	writer.inFlightAdmitted = false
	return runtime.currentEvent, runtime.currentEvent.Disposition, token
}

// Admit performs the final nonblocking admission check and pins the active
// writer generation until Complete. A changed winner, expiry, target, event,
// writer, or token fails closed. A later transport adapter must additionally
// bound its real write by the claimed frame deadline.
func (runtime *Runtime) Admit(writer *WriterLease, token,
	nowMicroseconds uint64) bool {
	if runtime == nil {
		return false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !runtime.exactClaim(writer, token) || writer.inFlightAdmitted {
		return false
	}
	runtime.reevaluate(nowMicroseconds)
	if !runtime.hasEvent ||
		runtime.currentEventRevision != writer.inFlightEventRevision ||
		runtime.currentEvent != writer.inFlightDelivery ||
		writer.DeviceGeneration != runtime.currentEvent.DeviceGeneration ||
		writer.TransportGeneration != runtime.currentEvent.TransportGeneration {
		return false
	}
	if runtime.currentEvent.Disposition == DeliveryFrame &&
		!runtime.currentEvent.Frame.FreshAt(nowMicroseconds) {
		return false
	}
	writer.inFlightAdmitted = true
	if runtime.currentEvent.Disposition == DeliveryFrame {
		runtime.ownerMayHaveActuated = true
	}
	return true
}

// Complete resolves one exact reservation. delivered=true requires prior
// admission. Failure clears only the reservation. Successful Stop completion
// advances to the newest currently eligible successor and never republishes
// that Stop.
func (runtime *Runtime) Complete(writer *WriterLease, token uint64,
	delivered bool, nowMicroseconds uint64) bool {
	if runtime == nil {
		return false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !runtime.exactClaim(writer, token) ||
		(delivered && !writer.inFlightAdmitted) {
		return false
	}
	claimedRevision := writer.inFlightEventRevision
	claimed := writer.inFlightDelivery
	if delivered {
		writer.completedEventRevision = claimedRevision
	}
	clearRuntimeClaim(writer)
	if delivered && claimed.Disposition == DeliveryStop && runtime.hasEvent &&
		runtime.currentEventRevision == claimedRevision &&
		runtime.currentEvent == claimed {
		runtime.hasOwner = false
		runtime.stopping = false
		runtime.ownerMayHaveActuated = false
		runtime.owner = Publication{}
		runtime.ownerDeliveryEpoch = 0
		runtime.hasEvent = false
		runtime.currentEvent = Delivery{}
	}
	runtime.reevaluate(nowMicroseconds)
	return true
}

// ReadCurrent returns the current immutable delivery event for diagnostics.
func (runtime *Runtime) ReadCurrent() (Delivery, uint64, bool) {
	if runtime == nil {
		return Delivery{}, 0, false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.currentEvent, runtime.currentEventRevision, runtime.hasEvent
}

func (runtime *Runtime) reevaluate(nowMicroseconds uint64) {
	if runtime.stopping {
		return
	}
	winner, hasWinner := runtime.selectWinner(nowMicroseconds)
	if !runtime.hasOwner {
		if hasWinner {
			runtime.startOwner(winner)
		}
		return
	}
	if hasWinner && sameRuntimeOwnership(winner, runtime.owner) {
		if winner != runtime.owner {
			runtime.owner = winner
			runtime.setFrameEvent()
		}
		return
	}
	if !runtime.ownerMayHaveActuated {
		if hasWinner {
			runtime.replaceUnadmittedOwner(winner)
		} else {
			runtime.hasOwner = false
			runtime.owner = Publication{}
			runtime.ownerDeliveryEpoch = 0
			runtime.hasEvent = false
			runtime.currentEvent = Delivery{}
		}
		return
	}
	runtime.stopping = true
	runtime.setEvent(Delivery{
		Disposition: DeliveryStop, Origin: runtime.owner.Origin,
		DeviceGeneration:    runtime.owner.Frame.DeviceGeneration,
		TransportGeneration: runtime.owner.Frame.TransportGeneration,
		DeliveryEpoch:       runtime.ownerDeliveryEpoch,
	})
}

func (runtime *Runtime) startOwner(publication Publication) {
	epoch := runtime.nextDeliveryEpoch + 1
	if epoch == 0 {
		return
	}
	runtime.nextDeliveryEpoch = epoch
	runtime.ownerDeliveryEpoch = epoch
	runtime.owner = publication
	runtime.hasOwner = true
	runtime.stopping = false
	runtime.ownerMayHaveActuated = false
	runtime.setFrameEvent()
}

func (runtime *Runtime) replaceUnadmittedOwner(publication Publication) {
	runtime.hasOwner = false
	runtime.owner = Publication{}
	runtime.ownerDeliveryEpoch = 0
	runtime.hasEvent = false
	runtime.currentEvent = Delivery{}
	runtime.startOwner(publication)
}

func (runtime *Runtime) setFrameEvent() {
	runtime.setEvent(Delivery{
		Disposition: DeliveryFrame, Origin: runtime.owner.Origin,
		Frame:               runtime.owner.Frame,
		DeviceGeneration:    runtime.owner.Frame.DeviceGeneration,
		TransportGeneration: runtime.owner.Frame.TransportGeneration,
		DeliveryEpoch:       runtime.ownerDeliveryEpoch,
	})
}

func (runtime *Runtime) setEvent(delivery Delivery) {
	revision := runtime.currentEventRevision + 1
	if revision == 0 {
		revision = 1
	}
	runtime.currentEventRevision = revision
	runtime.currentEvent = delivery
	runtime.hasEvent = true
}

func (runtime *Runtime) selectWinner(nowMicroseconds uint64) (Publication, bool) {
	deviceGeneration, transportGeneration, found := runtime.newestTarget()
	if !found {
		return Publication{}, false
	}
	var winner Publication
	selected := false
	considerRuntimeSlot(&runtime.profileSlot, nowMicroseconds,
		deviceGeneration, transportGeneration, &winner, &selected)
	considerRuntimeSlot(&runtime.audioSlot, nowMicroseconds,
		deviceGeneration, transportGeneration, &winner, &selected)
	considerRuntimeSlot(&runtime.gameSlot, nowMicroseconds,
		deviceGeneration, transportGeneration, &winner, &selected)
	considerRuntimeSlot(&runtime.previewSlot, nowMicroseconds,
		deviceGeneration, transportGeneration, &winner, &selected)
	return winner, selected
}

func considerRuntimeSlot(slot *publicationSlot, nowMicroseconds,
	deviceGeneration, transportGeneration uint64, winner *Publication,
	found *bool) {
	if !slot.hasValue ||
		slot.publication.Frame.DeviceGeneration != deviceGeneration ||
		slot.publication.Frame.TransportGeneration != transportGeneration ||
		slot.publication.Frame.Command == CommandStop ||
		!slot.publication.Frame.FreshAt(nowMicroseconds) {
		return
	}
	if !*found || slot.publication.Origin > winner.Origin {
		*winner = slot.publication
		*found = true
	}
}

func (runtime *Runtime) newestTarget() (uint64, uint64, bool) {
	var deviceGeneration, transportGeneration uint64
	found := false
	findNewestRuntimeTarget(&runtime.profileSlot, &deviceGeneration,
		&transportGeneration, &found)
	findNewestRuntimeTarget(&runtime.audioSlot, &deviceGeneration,
		&transportGeneration, &found)
	findNewestRuntimeTarget(&runtime.gameSlot, &deviceGeneration,
		&transportGeneration, &found)
	findNewestRuntimeTarget(&runtime.previewSlot, &deviceGeneration,
		&transportGeneration, &found)
	return deviceGeneration, transportGeneration, found
}

func findNewestRuntimeTarget(slot *publicationSlot, deviceGeneration,
	transportGeneration *uint64, found *bool) {
	if !slot.hasValue {
		return
	}
	frame := slot.publication.Frame
	if !*found || frame.DeviceGeneration > *deviceGeneration ||
		(frame.DeviceGeneration == *deviceGeneration &&
			frame.TransportGeneration > *transportGeneration) {
		*deviceGeneration = frame.DeviceGeneration
		*transportGeneration = frame.TransportGeneration
		*found = true
	}
}

func sameRuntimeOwnership(left, right Publication) bool {
	return left.Origin == right.Origin && left.Frame.Source == right.Frame.Source &&
		left.Frame.DeviceGeneration == right.Frame.DeviceGeneration &&
		left.Frame.TransportGeneration == right.Frame.TransportGeneration &&
		left.Frame.OwnershipEpoch == right.Frame.OwnershipEpoch
}

func newerRuntimeFrame(candidate, current Frame) bool {
	if candidate.DeviceGeneration != current.DeviceGeneration {
		return candidate.DeviceGeneration > current.DeviceGeneration
	}
	if candidate.TransportGeneration != current.TransportGeneration {
		return candidate.TransportGeneration > current.TransportGeneration
	}
	if candidate.OwnershipEpoch != current.OwnershipEpoch {
		return candidate.OwnershipEpoch > current.OwnershipEpoch
	}
	return current.Command != CommandStop && candidate.Source == current.Source &&
		candidate.Sequence > current.Sequence
}

func (runtime *Runtime) currentWriter(writer *WriterLease) bool {
	return writer != nil && writer.self == writer && writer.active &&
		writer.owner == runtime && runtime.activeWriter == writer &&
		writer.WriterGeneration == runtime.nextWriterGeneration
}

func (runtime *Runtime) exactClaim(writer *WriterLease, token uint64) bool {
	return runtime.currentWriter(writer) && token != 0 &&
		writer.inFlightClaimToken == token &&
		writer.inFlightEventRevision != 0 && writer.inFlightDelivery.Valid()
}

func clearRuntimeClaim(writer *WriterLease) {
	writer.inFlightClaimToken = 0
	writer.inFlightEventRevision = 0
	writer.inFlightDelivery = Delivery{}
	writer.inFlightAdmitted = false
}

func (runtime *Runtime) slot(origin PublicationOrigin) *publicationSlot {
	switch origin {
	case PublicationOriginProfileEffect:
		return &runtime.profileSlot
	case PublicationOriginAudioHaptics:
		return &runtime.audioSlot
	case PublicationOriginNativeGame:
		return &runtime.gameSlot
	default:
		return &runtime.previewSlot
	}
}

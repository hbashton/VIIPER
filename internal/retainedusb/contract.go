// Package retainedusb defines the transport-neutral value contract for the
// bounded retained USB/IP submission arbiter. The production command reader
// selects it only for an explicit ImportDevice and nonzero server authority;
// this package does not register or construct any product device.
package retainedusb

import "time"

const (
	// MaximumQueueDepth matches the existing bounded endpoint-worker queue.
	MaximumQueueDepth = 64
	// MaximumBufferedRequestBytes is the repository-wide USB/IP transfer cap.
	MaximumBufferedRequestBytes = 16 * 1024 * 1024
)

// Lane is one mutually ordered retained USB submission plane.
type Lane uint8

const (
	LaneControl Lane = iota + 1
	LaneInterruptIn
	LaneInterruptOut
	laneLimit
)

// Valid reports whether lane identifies one retained plane.
func (lane Lane) Valid() bool { return lane >= LaneControl && lane < laneLimit }

// Index returns the zero-based fixed-array index for a valid lane.
func (lane Lane) Index() (int, bool) {
	if !lane.Valid() {
		return 0, false
	}
	return int(lane - LaneControl), true
}

// Direction identifies the immutable USB/IP request direction.
type Direction uint8

const (
	DirectionOut Direction = iota + 1
	DirectionIn
)

// Valid reports whether direction is defined.
func (direction Direction) Valid() bool {
	return direction == DirectionOut || direction == DirectionIn
}

// Route identifies one exact interface alternate-setting endpoint binding.
// EndpointAddress includes the USB direction bit. Endpoint zero is represented
// only by LaneControl and therefore is not a valid Route.
type Route struct {
	InterfaceNumber  uint8
	AlternateSetting uint8
	EndpointAddress  uint8
}

// ValidInterruptIn reports whether route names a nonzero IN endpoint.
func (route Route) ValidInterruptIn() bool {
	return route.EndpointAddress&0x80 != 0 && route.EndpointAddress&0x0f != 0 &&
		route.EndpointAddress&0x70 == 0
}

// ValidInterruptOut reports whether route names a nonzero OUT endpoint.
func (route Route) ValidInterruptOut() bool {
	return route.EndpointAddress&0x80 == 0 && route.EndpointAddress&0x0f != 0 &&
		route.EndpointAddress&0x70 == 0
}

// Limits is immutable construction-time storage and routing policy. QueueDepth
// is indexed with Lane.Index. Fixed per-slot request slabs require the complete
// control/OUT queue maxima to fit BufferedRequestBytes.
type Limits struct {
	QueueDepth [3]uint8

	BufferedRequestBytes uint32

	MaximumControlOut      uint32
	MaximumControlResponse uint32
	MaximumInterruptIn     uint32
	MaximumInterruptOut    uint32

	InterruptInRoute  Route
	InterruptOutRoute Route
}

// Valid reports whether limits can be represented by the fixed dormant
// scheduler without exceeding the repository USB/IP transfer bound.
func (limits Limits) Valid() bool {
	for _, depth := range limits.QueueDepth {
		if depth == 0 || depth > MaximumQueueDepth {
			return false
		}
	}
	if limits.BufferedRequestBytes > MaximumBufferedRequestBytes ||
		limits.MaximumControlOut > MaximumBufferedRequestBytes ||
		limits.MaximumControlResponse > MaximumBufferedRequestBytes ||
		limits.MaximumInterruptIn == 0 ||
		limits.MaximumInterruptIn > MaximumBufferedRequestBytes ||
		limits.MaximumInterruptOut == 0 ||
		limits.MaximumInterruptOut > MaximumBufferedRequestBytes ||
		!limits.InterruptInRoute.ValidInterruptIn() ||
		!limits.InterruptOutRoute.ValidInterruptOut() {
		return false
	}
	controlIndex, _ := LaneControl.Index()
	outIndex, _ := LaneInterruptOut.Index()
	required := uint64(limits.QueueDepth[controlIndex])*
		uint64(limits.MaximumControlOut) +
		uint64(limits.QueueDepth[outIndex])*
			uint64(limits.MaximumInterruptOut)
	return required <= uint64(limits.BufferedRequestBytes)
}

// Request is the exact immutable staging view of one copied host submission.
// Data is read-only, has exact length/capacity, and is valid only during Stage.
type Request struct {
	Lane              Lane
	Direction         Direction
	SessionGeneration uint64
	BindingGeneration uint64
	IngressOrdinal    uint64
	Sequence          uint32
	TransferLength    uint32
	Setup             [8]byte
	Route             Route
	Data              []byte
}

// Ticket is an owner-authenticated, lane- and session-bound staged request.
type Ticket struct {
	OwnerID           uint64
	Token             uint64
	Generation        uint64
	SessionGeneration uint64
	Lane              Lane
}

// Valid reports whether ticket has a structurally complete capability shape.
func (ticket Ticket) Valid() bool {
	return ticket.OwnerID != 0 && ticket.Token != 0 && ticket.Generation != 0 &&
		ticket.SessionGeneration != 0 && ticket.Lane.Valid()
}

// Result is the final response kind selected only under response serialization.
type Result uint8

const (
	ResultPending Result = iota
	ResultData
	ResultSuccess
	ResultStall
)

// Valid reports whether result is defined.
func (result Result) Valid() bool { return result <= ResultStall }

// Preparation is a value-only late selection. RetryAt and ReadinessEpoch are
// meaningful only for Pending. Terminal results require both to be zero. Data
// requires a nonzero ActualLength; a control ZLP is Success with zero length.
type Preparation struct {
	Result         Result
	ActualLength   uint32
	RetryAt        time.Time
	ReadinessEpoch uint64
}

// RetireReason identifies why a staged ticket lost transport ownership before
// final admission.
type RetireReason uint8

const (
	RetireUnlink RetireReason = iota + 1
	RetireEndpointReset
	RetireInterfaceReset
	RetireConfigurationChange
	RetireDeviceReset
	RetireConnectionClose
	RetirePrepareFailure
	RetireInvariantFailure
	RetireOwnerRequested
)

// Valid reports whether reason is defined.
func (reason RetireReason) Valid() bool {
	return reason >= RetireUnlink && reason <= RetireOwnerRequested
}

// Owner is the device-side half of the dormant retained submission boundary.
// Identity is a nonzero, owner-lifetime-stable authority identifier which must
// not be shared by independent owners. The scheduler constructor's caller is
// the authority for a nonzero, nonreused, nonwrapping session generation; each
// staged ticket must echo it exactly.
//
// Readiness returns one owner-lifetime-stable, latched channel with capacity
// of at least one and a monotonically increasing, nonwrapping epoch. The owner
// increments the epoch before a nonblocking wake signal; a full channel means
// an earlier wake is already latched. Prepare reports the current epoch in
// Pending. Capture that observation before semantic selection so a publication
// arriving after an empty decision remains newer and cannot lose its wake.
// Closing the channel or reaching MaxUint64 is a terminal invariant
// failure.
//
// Every Owner method is bounded, nonblocking, and performs no I/O. Identity,
// Limits, Complete, Retire, and Readiness are never called while the scheduler
// mutex is held. Stage and Prepare are the only exceptions: both run under the
// scheduler cancellation fence, and Prepare additionally runs under response
// serialization. A Stage error or structurally invalid ticket transfers no
// per-ticket ownership to the scheduler; only the outer session boundary may
// clean up such owner-side ambiguity. No method may call back into the
// scheduler or retain a slice.
type Owner interface {
	Identity() uint64
	Limits() Limits
	Stage(Request) (Ticket, error)
	Prepare(Ticket, []byte, time.Time) (Preparation, error)
	Complete(Ticket, bool, time.Time) error
	Retire(Ticket, RetireReason, time.Time) error
	Readiness() (uint64, <-chan struct{})
}

// ImportLease is the exact outer capability for one dormant retained USB/IP
// import. It is deliberately separate from Ticket: hot submission ownership
// cannot authorize whole-device teardown or release an import. AuthorityID,
// DeviceID, and OwnerID identify the three independent authorities involved;
// ImportToken and SessionGeneration are nonzero, nonreused, and nonwrapping.
type ImportLease struct {
	AuthorityID       uint64
	DeviceID          uint64
	OwnerID           uint64
	ImportToken       uint64
	SessionGeneration uint64
}

// ImportBindState is the terminal result of admitting an issued lease into an
// outer owner before the transport exposes a successful import. Rejected
// proves that the owner retained no lease or session state. Any other failure
// shape is ambiguous and must quarantine the issued capability.
type ImportBindState uint8

const (
	ImportBindInvalid ImportBindState = iota
	ImportBindBound
	ImportBindRejected
	ImportBindQuarantined
)

// ImportBindResult echoes the complete issued lease. Bound establishes the
// capability which every later drain/disconnect callback must authenticate.
type ImportBindResult struct {
	Lease ImportLease
	State ImportBindState
}

// Valid reports whether lease has a structurally complete capability shape.
// Structural validity does not authenticate it; the issuing authority must
// still compare every field with its retained record.
func (lease ImportLease) Valid() bool {
	return lease.AuthorityID != 0 && lease.DeviceID != 0 && lease.OwnerID != 0 &&
		lease.ImportToken != 0 && lease.SessionGeneration != 0
}

// ImportCloseReason is the first, immutable reason which began closing a
// retained import. A repeated close must present the same reason.
type ImportCloseReason uint8

const (
	ImportClosePeerDisconnect ImportCloseReason = iota + 1
	ImportCloseReadFailure
	ImportCloseWriteFailure
	ImportCloseContextCanceled
	ImportCloseExplicitDetach
	ImportCloseInvariantFailure
	ImportCloseOwnerRequested
)

// Valid reports whether reason identifies an authoritative close boundary.
func (reason ImportCloseReason) Valid() bool {
	return reason >= ImportClosePeerDisconnect &&
		reason <= ImportCloseOwnerRequested
}

// ImportDrainState is the bounded result of canceling the device-local
// executor. Invalid means no authenticated drain reached the owner and proves
// no containment fact. Drained permits the later neutral boundary only after
// the retained scheduler also proved stopped. Quarantined is an exact
// containment acknowledgement which may be returned when scheduler close is
// unproven; it may accompany the owner's retained quarantine cause and never
// permits neutralization, release, or successor admission.
type ImportDrainState uint8

const (
	ImportDrainInvalid ImportDrainState = iota
	ImportDrainDrained
	ImportDrainQuarantined
)

// ImportDrainResult echoes the exact capability and close reason so the
// transport cannot accept a stale or cross-session terminal acknowledgement.
type ImportDrainResult struct {
	Lease  ImportLease
	Reason ImportCloseReason
	State  ImportDrainState
}

// ImportDisconnectState is the authoritative disconnect/neutral terminal
// result. Invalid means no authenticated disconnect reached its terminal
// boundary and proves no containment fact. Safe alone permits release of the
// import lease; Quarantined proves containment but never release.
type ImportDisconnectState uint8

const (
	ImportDisconnectInvalid ImportDisconnectState = iota
	ImportDisconnectSafe
	ImportDisconnectQuarantined
)

// ImportDisconnectResult echoes the exact capability and close reason. A Safe
// result proves that the one disconnect neutral/clear action reached its
// terminal boundary; Quarantined permanently fences this device/session.
type ImportDisconnectResult struct {
	Lease  ImportLease
	Reason ImportCloseReason
	State  ImportDisconnectState
}

// ImportResetLease is the exact outer capability for one reversible reset of
// a still-claimed retained import. ImportLease authenticates the unchanged
// import; ResetToken is authority-wide nonreused and ResetGeneration is the
// exact nonwrapping successor of that import's last issued reset generation.
// This is deliberately not a USB setup packet and cannot establish that a
// particular USB/IP client reports a bus reset.
type ImportResetLease struct {
	ImportLease     ImportLease
	ResetToken      uint64
	ResetGeneration uint64
}

// Valid reports only whether the reset capability has a complete structural
// shape. The issuing retained-import authority must still authenticate every
// field against its retained record.
func (lease ImportResetLease) Valid() bool {
	return lease.ImportLease.Valid() && lease.ResetToken != 0 &&
		lease.ResetGeneration != 0
}

// ImportResetState is the bounded result of one exact reversible reset.
// Invalid means no authenticated reset transaction reached the reset boundary
// and proves no containment fact. Safe proves the successor-generation neutral
// action was delivered before owner admission reopened. Quarantined proves
// containment only and never permits admission, disconnect neutralization, or
// import release.
type ImportResetState uint8

const (
	ImportResetInvalid ImportResetState = iota
	ImportResetSafe
	ImportResetQuarantined
)

// ImportResetResult echoes the exact capability so a retained orchestrator
// cannot accept a stale or cross-import reset acknowledgement.
type ImportResetResult struct {
	Lease ImportResetLease
	State ImportResetState
}

// ImportResetSessionOwner is the optional device-side half of the dormant
// reversible-reset boundary. It is deliberately separate from
// ImportSessionOwner: terminal close authority cannot be reinterpreted as a
// reset. The caller first latches scheduler ingress and invokes
// FenceAndDrainReset to close the owner's Stage/Prepare admission, reversibly
// cancel and join local execution, and retain the exact reset fence. The caller
// then synchronously drains every exact retained submission before invoking
// ResetAndRestart. That second phase advances exactly one device generation,
// delivers exactly one successor-generation neutral action, and reopens owner
// admission only after that delivery is terminal. Any error, panic, timeout,
// malformed result, or Quarantined result permanently fences the import.
type ImportResetSessionOwner interface {
	FenceAndDrainReset(ImportResetLease, time.Time) error
	ResetAndRestart(ImportResetLease, time.Time) (ImportResetResult, error)
}

// ImportSessionOwner is the device-side half of the dormant whole-import
// lifecycle boundary. It is intentionally separate from Owner. Identity must
// be nonzero and stable for the owner's lifetime. Every method must honor its
// deadline and must not call the USB/IP server.
//
// BindImport runs before the claim can be exposed as successful and records
// the exact issued lease in the owner. An exact Rejected result proves that no
// owner state was retained and permits claim rollback; every ambiguous result
// quarantines the burned capability. CancelAndDrain synchronously cancels and
// joins every device-local action. For failure containment it may be invoked
// after the transport has attempted, but cannot prove, its outer admission
// fence: the owner must synchronously close its own Stage/Prepare admission
// before cancelling or joining local execution. ImportDrainQuarantined proves
// containment only and can never authorize neutralization or lease release.
// DisconnectNeutral then performs the authoritative disconnect boundary and
// its one neutral/clear action, and is safe only after transport ingress and
// the retained scheduler are both proven stopped and CancelAndDrain returned
// exact ImportDrainDrained. The transport invokes each method at most once and
// validates every echoed lease and reason exactly. A callback error, panic,
// timeout, malformed result, or Quarantined result keeps the import claimed.
type ImportSessionOwner interface {
	Identity() uint64
	BindImport(ImportLease, time.Time) (ImportBindResult, error)
	CancelAndDrain(
		ImportLease, ImportCloseReason, time.Time,
	) (ImportDrainResult, error)
	DisconnectNeutral(
		ImportLease, ImportCloseReason, time.Time,
	) (ImportDisconnectResult, error)
}

// ImportOwner is the exact same object on both sides of a retained import:
// Owner owns hot submission tickets, while ImportSessionOwner owns the outer
// bind/drain/disconnect capability. Keeping the composition explicit prevents
// a transport from binding one object and dispatching URBs to another.
type ImportOwner interface {
	Owner
	ImportSessionOwner
}

// ImportDevice is the narrow, explicit device opt-in consumed by the USB/IP
// server. DeviceID is a nonzero device-resource identity selected before the
// owner is constructed; it is not a USB address and is not a GIP Device ID.
// RetainedUSBImportOwner must return one stable pointer for the lifetime of the
// registered device. A server without a retained authority rejects this
// explicitly opted-in device rather than silently routing it through legacy
// HandleTransfer; ordinary devices preserve the legacy import path exactly.
// The embedding usb.Device's GetDescriptor is also a cold retained callback:
// it must be bounded, nonblocking, perform no I/O, and return the fixed retained
// topology. The server seals only that fixed scalar topology and never copies
// caller-sized legacy descriptor collections.
type ImportDevice interface {
	// Both methods are pure, bounded, nonblocking identity/capability reads.
	// They must not perform I/O or retain transport state.
	RetainedUSBImportDeviceID() uint64
	RetainedUSBImportOwner() ImportOwner
}

// PrimaryGIPIdentityDevice exposes the immutable primary Hello DeviceID from
// the authorized persona, not the retained import lease ID or USB serial. The
// server uses this additional constraint to prevent two Windows GIP devices
// sharing a driver lookup key. It grants no registration/import authority.
// The read must be pure, bounded, nonblocking and perform no I/O.
type PrimaryGIPIdentityDevice interface {
	ImportDevice
	RetainedUSBPrimaryGIPDeviceID() uint64
}

// OneShotImportDevice marks a retained device whose local executor and
// authorization are terminal after one Safe disconnect. The server removes
// the exact device from its owning virtual bus only after the retained session
// proves scheduler drain, local drain, neutral delivery, and authority release.
// A reconnect-capable product must instead provision a fresh executor through
// an explicit successor-session policy and must not implement this marker.
// RetainedUSBRemoveAfterSafeDisconnect is a pure, bounded, nonblocking policy
// read and must not perform I/O.
type OneShotImportDevice interface {
	ImportDevice
	RetainedUSBRemoveAfterSafeDisconnect() bool
}

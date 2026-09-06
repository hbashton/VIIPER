package xboxone

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrControllerDownstreamPacketExecutorUninitialized rejects a zero owner or
	// an absent whole-packet participant.
	ErrControllerDownstreamPacketExecutorUninitialized = errors.New(
		"xboxone: downstream packet executor is uninitialized")
	// ErrControllerDownstreamPacketExecutorBusy preserves the one-packet-at-a-
	// time ownership rule. It is not a receive replay decision.
	ErrControllerDownstreamPacketExecutorBusy = errors.New(
		"xboxone: downstream packet executor already owns a packet")
	// ErrControllerDownstreamPacketNonAtomicAction rejects a decoded message
	// whose persona effect cannot yet join the fixed whole-vector transaction.
	ErrControllerDownstreamPacketNonAtomicAction = errors.New(
		"xboxone: downstream message has no atomic packet action")
	// ErrInvalidControllerDownstreamPacketClaim rejects a forged, stale,
	// copied-after-resolution, or otherwise mismatched packet capability.
	ErrInvalidControllerDownstreamPacketClaim = errors.New(
		"xboxone: invalid downstream packet claim")
	// ErrInvalidControllerDownstreamPacketExecution rejects a forged, stale,
	// duplicate, or otherwise mismatched admitted execution capability.
	ErrInvalidControllerDownstreamPacketExecution = errors.New(
		"xboxone: invalid downstream packet execution")
	// ErrControllerDownstreamPacketClaimNotAdmitted rejects Execute or a
	// delivered resolution before final whole-vector preflight.
	ErrControllerDownstreamPacketClaimNotAdmitted = errors.New(
		"xboxone: downstream packet claim was not admitted")
	// ErrControllerDownstreamPacketRetryRequired prevents a successor packet
	// from overtaking the exact immutable predecessor retry.
	ErrControllerDownstreamPacketRetryRequired = errors.New(
		"xboxone: downstream packet retry is required")
	// ErrInvalidControllerDownstreamPacketOutcome rejects an undefined or an
	// execution-inconsistent terminal outcome.
	ErrInvalidControllerDownstreamPacketOutcome = errors.New(
		"xboxone: invalid downstream packet outcome")
	// ErrControllerDownstreamPacketDeadline rejects a missing/expired deadline
	// and records a participant which returned after its promised bound.
	ErrControllerDownstreamPacketDeadline = errors.New(
		"xboxone: downstream packet execution deadline expired")
	// ErrControllerDownstreamPacketParticipantPanic records an untrusted
	// participant panic. Preflight panics and execution panics quarantine the
	// owner because their external side effects cannot be inferred.
	ErrControllerDownstreamPacketParticipantPanic = errors.New(
		"xboxone: downstream packet participant panicked")
	// ErrControllerDownstreamPacketDrainRequired fences resolution/retry after
	// a panic or deadline ambiguity until exact cancellation and drain classifies
	// containment. Drain cannot itself prove whether an earlier effect occurred.
	ErrControllerDownstreamPacketDrainRequired = errors.New(
		"xboxone: downstream packet execution requires cancellation and drain")
	// ErrControllerDownstreamPacketQuarantined is terminal. The owner cannot
	// advertise reuse after containment failed or prior delivery remains
	// unknowable even though the exact attempt was successfully drained.
	ErrControllerDownstreamPacketQuarantined = errors.New(
		"xboxone: downstream packet executor is quarantined")
	// ErrControllerDownstreamPacketTokenExhausted prevents packet/claim identity
	// wrap from creating an ABA-capable execution lease.
	ErrControllerDownstreamPacketTokenExhausted = errors.New(
		"xboxone: downstream packet execution identity exhausted")
)

// ControllerDownstreamPacketAction is one self-contained, byte-defined host
// action in its original packet position. Sequence is preserved as evidence;
// this owner deliberately does not interpret it as a replay window.
//
// Direct Motor and Guide LED can be compiled directly from decoded bytes. A
// ControllerPersonaDownstreamPacketBatchOwner additionally supplies the exact
// lifecycle cursor or reliable-ACK disposition selected by the canonical
// ControllerPersonaEngine. Standalone packet-owner Claim still rejects those
// context-dependent messages rather than manufacturing that authority.
type ControllerDownstreamPacketAction struct {
	Action                 ControllerPersonaAction
	WireIndex              uint8
	Sequence               uint8
	OutputSequence         uint8
	ClearEpoch             uint64
	ReliableACKDisposition ReliableAcknowledgementDisposition
	DirectMotor            RumbleBodyV1
	GuideLED               GuideLEDCommandV1
	wire                   [ControllerPersonaMaximumWireSize]byte
	wireSize               uint8
}

func (action ControllerDownstreamPacketAction) valid() bool {
	if action.Sequence == 0 {
		return false
	}
	localEmpty := action.OutputSequence == 0 && action.ClearEpoch == 0 &&
		action.ReliableACKDisposition == 0 && action.wireSize == 0
	switch action.Action {
	case ControllerPersonaApplyDirectMotor:
		return localEmpty && action.GuideLED == (GuideLEDCommandV1{}) &&
			action.DirectMotor.Validate() == nil
	case ControllerPersonaApplyGuideLED:
		return localEmpty && action.DirectMotor == (RumbleBodyV1{}) &&
			action.GuideLED.Validate() == nil
	case ControllerPersonaApplyMetadataAcknowledgement:
		return action.DirectMotor == (RumbleBodyV1{}) &&
			action.GuideLED == (GuideLEDCommandV1{}) &&
			action.OutputSequence == 0 && action.ClearEpoch == 0 &&
			action.wireSize == 0 &&
			action.ReliableACKDisposition >= ReliableAcknowledgementProgress &&
			action.ReliableACKDisposition <= ReliableAcknowledgementDuplicate
	case ControllerPersonaSendHello, ControllerPersonaSendMetadata,
		ControllerPersonaSendCurrentStatus,
		ControllerPersonaSendPoweringOffStatus,
		ControllerPersonaSendInitialInput, ControllerPersonaSendInput:
		return action.DirectMotor == (RumbleBodyV1{}) &&
			action.GuideLED == (GuideLEDCommandV1{}) &&
			action.OutputSequence != 0 && action.ClearEpoch == 0 &&
			action.ReliableACKDisposition == 0 && action.wireSize != 0 &&
			int(action.wireSize) <= len(action.wire)
	case ControllerPersonaClearOutputs:
		return action.DirectMotor == (RumbleBodyV1{}) &&
			action.GuideLED == (GuideLEDCommandV1{}) &&
			action.OutputSequence == 0 && action.ClearEpoch != 0 &&
			action.ReliableACKDisposition == 0 && action.wireSize == 0
	case ControllerPersonaBeginMetadata, ControllerPersonaPermitNormalUpstream,
		ControllerPersonaGateNormalUpstream,
		ControllerPersonaCompletePowerOff, ControllerPersonaPerformReset,
		ControllerPersonaIgnoreHostMessage:
		return localEmpty && action.DirectMotor == (RumbleBodyV1{}) &&
			action.GuideLED == (GuideLEDCommandV1{})
	default:
		return false
	}
}

// WireSize reports the exact persona egress bytes owned by this action. It is
// zero for local effects, downstream feedback, ACK progress, and an ignored
// source-defined no-op.
func (action ControllerDownstreamPacketAction) WireSize() int {
	return int(action.wireSize)
}

// CopyWire copies the complete private wire image. Exact length is required so
// a participant cannot accidentally publish a prefix of a lifecycle response.
func (action ControllerDownstreamPacketAction) CopyWire(dst []byte) error {
	if len(dst) != int(action.wireSize) {
		return fmt.Errorf("%w: got=%d want=%d",
			ErrInvalidControllerPersonaDestination, len(dst), action.wireSize)
	}
	copy(dst, action.wire[:action.wireSize])
	return nil
}

// ControllerDownstreamPacketExecution is the immutable, fixed-capacity
// vector presented to the atomic participant. PacketEpoch is local ownership
// identity, not a GIP field. Copies retain the same action values and order.
type ControllerDownstreamPacketExecution struct {
	packetEpoch uint64
	actions     [controllerDownstreamPacketMaximumMessages]ControllerDownstreamPacketAction
	length      uint8
}

func (execution ControllerDownstreamPacketExecution) PacketEpoch() uint64 {
	return execution.packetEpoch
}

func (execution ControllerDownstreamPacketExecution) Len() int {
	return int(execution.length)
}

func (execution ControllerDownstreamPacketExecution) Action(
	index int,
) (ControllerDownstreamPacketAction, bool) {
	if index < 0 || index >= int(execution.length) {
		return ControllerDownstreamPacketAction{}, false
	}
	return execution.actions[index], true
}

func (execution ControllerDownstreamPacketExecution) valid() bool {
	if execution.packetEpoch == 0 || execution.length == 0 ||
		int(execution.length) > len(execution.actions) {
		return false
	}
	for index := 0; index < int(execution.length); index++ {
		action := execution.actions[index]
		if action.WireIndex != uint8(index) || !action.valid() {
			return false
		}
	}
	return true
}

// ControllerDownstreamPacketAtomicParticipant is the only effect boundary.
// It receives the complete vector once, never one callback per message.
//
// PreflightControllerDownstreamPacket MUST be side-effect-free, MUST NOT retain
// execution, and MUST reject unless every action can participate in one atomic
// commit. It must perform only bounded in-process work and return synchronously;
// it has no I/O deadline because Claim and Admit serialize owner mutation while
// it runs. ExecuteControllerDownstreamPacket returns nil only after every action
// became visible atomically in vector order. A non-nil return before deadline
// proves that no action became visible and no late effect can occur. Execute
// must itself reject an expired supplied deadline before making an effect; the
// owner also checks the bound at its serialized state-transition boundary.
//
// CancelControllerDownstreamPacketAndDrain may run concurrently with Execute.
// A nil return proves the exact attempt has been joined and no further effect
// can occur; it does not prove that an earlier effect did not occur. If Execute
// already committed successfully, cancellation cannot relabel it; the owner
// still requires Delivered resolution. A panic or late non-nil Execute remains
// outcome-ambiguous after drain and permanently quarantines this owner rather
// than making the timed/ramped vector retryable. Implementations must permit
// reentrant Snapshot calls but must not call another mutating owner method from
// a participant callback.
type ControllerDownstreamPacketAtomicParticipant interface {
	PreflightControllerDownstreamPacket(ControllerDownstreamPacketExecution) error
	ExecuteControllerDownstreamPacket(
		ControllerDownstreamPacketExecution,
		time.Time,
	) error
	CancelControllerDownstreamPacketAndDrain(
		ControllerDownstreamPacketExecution,
		time.Time,
	) error
}

// ControllerDownstreamPacketClaim is an opaque capability for one exact packet
// epoch and attempt token. A retry receives a fresh token but retains Epoch.
type ControllerDownstreamPacketClaim struct {
	owner *ControllerDownstreamPacketExecutionOwner
	token uint64
	epoch uint64
	count uint8
}

func (claim ControllerDownstreamPacketClaim) Valid() bool {
	return claim.owner != nil && claim.token != 0 && claim.epoch != 0 && claim.count != 0
}

func (claim ControllerDownstreamPacketClaim) PacketEpoch() uint64 { return claim.epoch }
func (claim ControllerDownstreamPacketClaim) MessageCount() int   { return int(claim.count) }

// ControllerDownstreamPacketExecutionLease transfers the exact admitted value
// to Execute. It is copyable but resolves only once through its owner.
type ControllerDownstreamPacketExecutionLease struct {
	owner     *ControllerDownstreamPacketExecutionOwner
	token     uint64
	execution ControllerDownstreamPacketExecution
}

func (lease ControllerDownstreamPacketExecutionLease) Valid() bool {
	return lease.owner != nil && lease.token != 0 && lease.execution.valid()
}

func (lease ControllerDownstreamPacketExecutionLease) PacketEpoch() uint64 {
	return lease.execution.PacketEpoch()
}

func (lease ControllerDownstreamPacketExecutionLease) Len() int {
	return lease.execution.Len()
}

func (lease ControllerDownstreamPacketExecutionLease) Action(
	index int,
) (ControllerDownstreamPacketAction, bool) {
	return lease.execution.Action(index)
}

// ControllerDownstreamPacketOutcome is the exact terminal fact accepted by
// Resolve. Deferred means Execute never began. DeliveryFailed requires the
// participant's timely, proven no-effect error. ExecutionCancelled requires a
// successful exact cancel-and-drain plus a timely, proven no-effect Execute
// error. Panic and late-error ambiguity is quarantined even after exact drain.
type ControllerDownstreamPacketOutcome uint8

const (
	ControllerDownstreamPacketDelivered ControllerDownstreamPacketOutcome = iota + 1
	ControllerDownstreamPacketDeferred
	ControllerDownstreamPacketDeliveryFailed
	ControllerDownstreamPacketExecutionCancelled
)

type controllerDownstreamPacketOwnerState uint8

const (
	controllerDownstreamPacketIdle controllerDownstreamPacketOwnerState = iota + 1
	controllerDownstreamPacketClaimed
	controllerDownstreamPacketAdmitted
	controllerDownstreamPacketExecuting
	controllerDownstreamPacketCancelling
	controllerDownstreamPacketAwaitingResolution
	controllerDownstreamPacketRetryPending
	controllerDownstreamPacketDrainRequired
	controllerDownstreamPacketResolutionPrepared
	controllerDownstreamPacketQuarantined
)

type controllerDownstreamPacketExecutionResult uint8

const (
	controllerDownstreamPacketNoResult controllerDownstreamPacketExecutionResult = iota
	controllerDownstreamPacketResultDelivered
	controllerDownstreamPacketResultRejected
	controllerDownstreamPacketResultCancelled
)

// ControllerDownstreamPacketExecutionSnapshot is diagnostic evidence, never an
// admission capability. Counters expose non-wrapping ownership progress only.
type ControllerDownstreamPacketExecutionSnapshot struct {
	PacketEpoch        uint64
	ClaimToken         uint64
	MessageCount       uint8
	NextPacketEpoch    uint64
	NextClaimToken     uint64
	Idle               bool
	Claimed            bool
	Admitted           bool
	ExecutionInFlight  bool
	AwaitingResolution bool
	RetryPending       bool
	DrainRequired      bool
	ResolutionPrepared bool
	Quarantined        bool
	Delivered          bool
	Rejected           bool
	Cancelled          bool
}

// ControllerDownstreamPacketExecutionOwner owns one decoded packet attempt at
// a time. It is dormant and backend-independent: it does not parse wire,
// register a persona, split USB aggregates, reassemble fragments, or decide
// receive replay. A private self-fence rejects copied values after construction.
type ControllerDownstreamPacketExecutionOwner struct {
	mu          sync.Mutex
	self        *ControllerDownstreamPacketExecutionOwner
	operation   chan struct{}
	progress    chan struct{}
	participant ControllerDownstreamPacketAtomicParticipant
	state       controllerDownstreamPacketOwnerState

	nextPacketEpoch uint64
	nextClaimToken  uint64
	claimToken      uint64
	execution       ControllerDownstreamPacketExecution
	result          controllerDownstreamPacketExecutionResult
	callbackDone    bool
	// callbackParticipantErr preserves the participant's original terminal
	// fact. callbackErr is the owner-facing result after deadline/panic policy.
	// In particular, a late nil remains a delivered commit after exact drain.
	callbackParticipantErr error
	callbackErr            error
	callbackReturn         time.Time
	callbackAmbiguous      bool
	quarantine             error
}

// NewControllerDownstreamPacketExecutionOwner constructs an idle dormant
// owner. Construction and successful preflight perform no participant effect.
func NewControllerDownstreamPacketExecutionOwner(
	participant ControllerDownstreamPacketAtomicParticipant,
) (*ControllerDownstreamPacketExecutionOwner, error) {
	if participant == nil {
		return nil, ErrControllerDownstreamPacketExecutorUninitialized
	}
	owner := &ControllerDownstreamPacketExecutionOwner{
		operation: make(chan struct{}, 1), progress: make(chan struct{}, 1),
		participant: participant, state: controllerDownstreamPacketIdle,
	}
	owner.self = owner
	owner.operation <- struct{}{}
	return owner, nil
}

// Claim compiles only self-contained typed actions, then asks the participant
// to preflight the complete vector before changing any owner state or counter.
func (owner *ControllerDownstreamPacketExecutionOwner) Claim(
	packet ControllerDownstreamPacket,
) (ControllerDownstreamPacketClaim, error) {
	if !owner.initialized() {
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketExecutorUninitialized
	}
	<-owner.operation
	defer func() { owner.operation <- struct{}{} }()

	owner.mu.Lock()
	if owner.state == controllerDownstreamPacketQuarantined {
		err := owner.quarantineErrorLocked()
		owner.mu.Unlock()
		return ControllerDownstreamPacketClaim{}, err
	}
	if owner.state == controllerDownstreamPacketRetryPending {
		owner.mu.Unlock()
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketRetryRequired
	}
	if owner.state != controllerDownstreamPacketIdle {
		owner.mu.Unlock()
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketExecutorBusy
	}
	if owner.nextPacketEpoch == ^uint64(0) || owner.nextClaimToken == ^uint64(0) {
		owner.mu.Unlock()
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketTokenExhausted
	}
	proposedEpoch := owner.nextPacketEpoch + 1
	proposedToken := owner.nextClaimToken + 1
	owner.mu.Unlock()

	execution, err := compileControllerDownstreamPacketExecution(packet, proposedEpoch)
	if err != nil {
		return ControllerDownstreamPacketClaim{}, err
	}
	preflightErr, panicked := invokeControllerDownstreamPacketPreflight(
		owner.participant, execution)
	if panicked {
		owner.quarantineOwner(preflightErr)
		return ControllerDownstreamPacketClaim{}, preflightErr
	}
	if preflightErr != nil {
		return ControllerDownstreamPacketClaim{}, preflightErr
	}

	owner.mu.Lock()
	defer owner.mu.Unlock()
	// operation serializes mutators. Keep this recheck explicit so a future
	// callback policy change cannot silently weaken preflight atomicity.
	if owner.state != controllerDownstreamPacketIdle ||
		owner.nextPacketEpoch+1 != proposedEpoch ||
		owner.nextClaimToken+1 != proposedToken {
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketExecutorBusy
	}
	owner.nextPacketEpoch = proposedEpoch
	owner.nextClaimToken = proposedToken
	owner.claimToken = proposedToken
	owner.execution = execution
	owner.result = controllerDownstreamPacketNoResult
	owner.callbackDone = false
	owner.callbackParticipantErr = nil
	owner.callbackErr = nil
	owner.callbackReturn = time.Time{}
	owner.callbackAmbiguous = false
	owner.state = controllerDownstreamPacketClaimed
	return owner.claimLocked(), nil
}

// claimCanonicalExecution is package-private because only the canonical
// persona batch owner may supply lifecycle/ACK-resolved actions. The supplied
// epoch must be the exact non-wrapping successor owned by this executor. It
// otherwise follows Claim's two-phase whole-vector preflight semantics.
func (owner *ControllerDownstreamPacketExecutionOwner) claimCanonicalExecution(
	execution ControllerDownstreamPacketExecution,
) (ControllerDownstreamPacketClaim, error) {
	if !owner.initialized() {
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketExecutorUninitialized
	}
	<-owner.operation
	defer func() { owner.operation <- struct{}{} }()

	owner.mu.Lock()
	if owner.state == controllerDownstreamPacketQuarantined {
		err := owner.quarantineErrorLocked()
		owner.mu.Unlock()
		return ControllerDownstreamPacketClaim{}, err
	}
	if owner.state == controllerDownstreamPacketRetryPending {
		owner.mu.Unlock()
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketRetryRequired
	}
	if owner.state != controllerDownstreamPacketIdle {
		owner.mu.Unlock()
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketExecutorBusy
	}
	if owner.nextPacketEpoch == ^uint64(0) || owner.nextClaimToken == ^uint64(0) {
		owner.mu.Unlock()
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketTokenExhausted
	}
	proposedEpoch := owner.nextPacketEpoch + 1
	proposedToken := owner.nextClaimToken + 1
	if !execution.valid() || execution.packetEpoch != proposedEpoch {
		owner.mu.Unlock()
		return ControllerDownstreamPacketClaim{},
			ErrInvalidControllerDownstreamPacketExecution
	}
	owner.mu.Unlock()

	preflightErr, panicked := invokeControllerDownstreamPacketPreflight(
		owner.participant, execution)
	if panicked {
		owner.quarantineOwner(preflightErr)
		return ControllerDownstreamPacketClaim{}, preflightErr
	}
	if preflightErr != nil {
		return ControllerDownstreamPacketClaim{}, preflightErr
	}

	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.state != controllerDownstreamPacketIdle ||
		owner.nextPacketEpoch+1 != proposedEpoch ||
		owner.nextClaimToken+1 != proposedToken {
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketExecutorBusy
	}
	owner.nextPacketEpoch = proposedEpoch
	owner.nextClaimToken = proposedToken
	owner.claimToken = proposedToken
	owner.execution = execution
	owner.result = controllerDownstreamPacketNoResult
	owner.callbackDone = false
	owner.callbackParticipantErr = nil
	owner.callbackErr = nil
	owner.callbackReturn = time.Time{}
	owner.callbackAmbiguous = false
	owner.state = controllerDownstreamPacketClaimed
	return owner.claimLocked(), nil
}

// ClaimRetry reclaims the exact immutable action vector and packet epoch. The
// final participant preflight runs before a fresh attempt token is allocated.
func (owner *ControllerDownstreamPacketExecutionOwner) ClaimRetry() (
	ControllerDownstreamPacketClaim,
	error,
) {
	if !owner.initialized() {
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketExecutorUninitialized
	}
	<-owner.operation
	defer func() { owner.operation <- struct{}{} }()

	owner.mu.Lock()
	if owner.state == controllerDownstreamPacketQuarantined {
		err := owner.quarantineErrorLocked()
		owner.mu.Unlock()
		return ControllerDownstreamPacketClaim{}, err
	}
	if owner.state != controllerDownstreamPacketRetryPending {
		owner.mu.Unlock()
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketRetryRequired
	}
	if owner.nextClaimToken == ^uint64(0) {
		owner.mu.Unlock()
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketTokenExhausted
	}
	execution := owner.execution
	proposedToken := owner.nextClaimToken + 1
	owner.mu.Unlock()

	preflightErr, panicked := invokeControllerDownstreamPacketPreflight(
		owner.participant, execution)
	if panicked {
		owner.quarantineOwner(preflightErr)
		return ControllerDownstreamPacketClaim{}, preflightErr
	}
	if preflightErr != nil {
		return ControllerDownstreamPacketClaim{}, preflightErr
	}

	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.state != controllerDownstreamPacketRetryPending ||
		owner.nextClaimToken+1 != proposedToken || owner.execution != execution {
		return ControllerDownstreamPacketClaim{},
			ErrControllerDownstreamPacketExecutorBusy
	}
	owner.nextClaimToken = proposedToken
	owner.claimToken = proposedToken
	owner.result = controllerDownstreamPacketNoResult
	owner.callbackDone = false
	owner.callbackParticipantErr = nil
	owner.callbackErr = nil
	owner.callbackReturn = time.Time{}
	owner.callbackAmbiguous = false
	owner.state = controllerDownstreamPacketClaimed
	return owner.claimLocked(), nil
}

// Admit repeats whole-vector preflight at the final boundary, then transfers
// one immutable lease. A preflight rejection leaves the original claim intact.
func (owner *ControllerDownstreamPacketExecutionOwner) Admit(
	claim ControllerDownstreamPacketClaim,
) (ControllerDownstreamPacketExecutionLease, error) {
	if !owner.initialized() {
		return ControllerDownstreamPacketExecutionLease{},
			ErrControllerDownstreamPacketExecutorUninitialized
	}
	<-owner.operation
	defer func() { owner.operation <- struct{}{} }()

	owner.mu.Lock()
	if err := owner.validateClaimLocked(claim); err != nil {
		owner.mu.Unlock()
		return ControllerDownstreamPacketExecutionLease{}, err
	}
	if owner.state != controllerDownstreamPacketClaimed {
		owner.mu.Unlock()
		return ControllerDownstreamPacketExecutionLease{},
			ErrInvalidControllerDownstreamPacketClaim
	}
	execution := owner.execution
	owner.mu.Unlock()

	preflightErr, panicked := invokeControllerDownstreamPacketPreflight(
		owner.participant, execution)
	if panicked {
		owner.quarantineOwner(preflightErr)
		return ControllerDownstreamPacketExecutionLease{}, preflightErr
	}
	if preflightErr != nil {
		return ControllerDownstreamPacketExecutionLease{}, preflightErr
	}

	owner.mu.Lock()
	defer owner.mu.Unlock()
	if err := owner.validateClaimLocked(claim); err != nil ||
		owner.state != controllerDownstreamPacketClaimed {
		if err != nil {
			return ControllerDownstreamPacketExecutionLease{}, err
		}
		return ControllerDownstreamPacketExecutionLease{},
			ErrInvalidControllerDownstreamPacketClaim
	}
	owner.state = controllerDownstreamPacketAdmitted
	return ControllerDownstreamPacketExecutionLease{
		owner: owner, token: owner.claimToken, execution: owner.execution,
	}, nil
}

// Execute invokes the participant exactly once with the complete admitted
// vector. A timely error is a proven no-effect result. A panic or late return
// cannot be retried or resolved until CancelAndDrain supplies exact containment.
func (owner *ControllerDownstreamPacketExecutionOwner) Execute(
	lease ControllerDownstreamPacketExecutionLease,
	deadline time.Time,
) error {
	if !owner.initialized() {
		return ErrControllerDownstreamPacketExecutorUninitialized
	}
	if !controllerDownstreamPacketDeadlineValid(deadline) {
		return ErrControllerDownstreamPacketDeadline
	}
	<-owner.operation
	if !controllerDownstreamPacketDeadlineValid(deadline) {
		owner.operation <- struct{}{}
		return ErrControllerDownstreamPacketDeadline
	}
	owner.mu.Lock()
	if err := owner.validateExecutionLocked(lease); err != nil {
		owner.mu.Unlock()
		owner.operation <- struct{}{}
		return err
	}
	if owner.state != controllerDownstreamPacketAdmitted {
		owner.mu.Unlock()
		owner.operation <- struct{}{}
		return ErrControllerDownstreamPacketClaimNotAdmitted
	}
	// Snapshot is reentrant and takes mu without the operation serializer. Check
	// the bound again at the exact state-transition boundary in case that short
	// diagnostic critical section consumed the remaining budget.
	if !controllerDownstreamPacketDeadlineValid(deadline) {
		owner.mu.Unlock()
		owner.operation <- struct{}{}
		return ErrControllerDownstreamPacketDeadline
	}
	owner.state = controllerDownstreamPacketExecuting
	owner.callbackDone = false
	owner.callbackParticipantErr = nil
	owner.callbackErr = nil
	owner.callbackReturn = time.Time{}
	owner.callbackAmbiguous = false
	execution := owner.execution
	owner.mu.Unlock()
	owner.operation <- struct{}{}

	participantErr, panicked := invokeControllerDownstreamPacketExecute(
		owner.participant, execution, deadline)
	returned := time.Now()
	executeErr := participantErr
	if panicked {
		executeErr = errors.Join(executeErr,
			ErrControllerDownstreamPacketDrainRequired)
	} else if returned.After(deadline) {
		executeErr = errors.Join(executeErr,
			ErrControllerDownstreamPacketDeadline,
			ErrControllerDownstreamPacketDrainRequired)
	}

	owner.mu.Lock()
	owner.callbackDone = true
	owner.callbackParticipantErr = participantErr
	owner.callbackErr = executeErr
	owner.callbackReturn = returned
	owner.callbackAmbiguous = panicked || returned.After(deadline)
	switch owner.state {
	case controllerDownstreamPacketExecuting:
		if panicked || returned.After(deadline) {
			owner.state = controllerDownstreamPacketDrainRequired
		} else {
			owner.state = controllerDownstreamPacketAwaitingResolution
			if executeErr == nil {
				owner.result = controllerDownstreamPacketResultDelivered
			} else {
				owner.result = controllerDownstreamPacketResultRejected
			}
		}
	case controllerDownstreamPacketCancelling:
		// CancelAndDrain owns classification after its join returns.
	case controllerDownstreamPacketQuarantined:
		// Permanent containment failure remains authoritative.
	default:
		executeErr = errors.Join(executeErr,
			ErrInvalidControllerDownstreamPacketExecution)
		owner.state = controllerDownstreamPacketQuarantined
		owner.quarantine = executeErr
	}
	owner.mu.Unlock()
	owner.signalProgress()
	return executeErr
}

// CancelAndDrain contains only the exact admitted execution. Success never
// invents delivery: a completed nil Execute remains Delivered, a timely
// proven-no-effect error becomes ExecutionCancelled, and a panic or late
// non-nil result remains quarantined because prior delivery is unknowable.
// Drain failure or a participant contract violation also quarantines the owner.
func (owner *ControllerDownstreamPacketExecutionOwner) CancelAndDrain(
	lease ControllerDownstreamPacketExecutionLease,
	deadline time.Time,
) error {
	if !owner.initialized() {
		return ErrControllerDownstreamPacketExecutorUninitialized
	}
	if !controllerDownstreamPacketDeadlineValid(deadline) {
		return ErrControllerDownstreamPacketDeadline
	}
	<-owner.operation
	if !controllerDownstreamPacketDeadlineValid(deadline) {
		owner.operation <- struct{}{}
		return ErrControllerDownstreamPacketDeadline
	}
	owner.mu.Lock()
	if err := owner.validateExecutionLocked(lease); err != nil {
		owner.mu.Unlock()
		owner.operation <- struct{}{}
		return err
	}
	if owner.state != controllerDownstreamPacketExecuting &&
		owner.state != controllerDownstreamPacketDrainRequired {
		owner.mu.Unlock()
		owner.operation <- struct{}{}
		return ErrControllerDownstreamPacketDrainRequired
	}
	if !controllerDownstreamPacketDeadlineValid(deadline) {
		owner.mu.Unlock()
		owner.operation <- struct{}{}
		return ErrControllerDownstreamPacketDeadline
	}
	owner.state = controllerDownstreamPacketCancelling
	execution := owner.execution
	owner.mu.Unlock()
	owner.operation <- struct{}{}

	drainErr, panicked := invokeControllerDownstreamPacketCancelAndDrain(
		owner.participant, execution, deadline)
	returned := time.Now()
	if panicked || drainErr != nil || returned.After(deadline) {
		if returned.After(deadline) {
			drainErr = errors.Join(drainErr,
				ErrControllerDownstreamPacketDeadline)
		}
		owner.quarantineOwner(drainErr)
		return owner.quarantineError()
	}
	if err := owner.waitForCallback(deadline); err != nil {
		owner.quarantineOwner(err)
		return owner.quarantineError()
	}

	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.state != controllerDownstreamPacketCancelling || !owner.callbackDone {
		owner.state = controllerDownstreamPacketQuarantined
		owner.quarantine = ErrInvalidControllerDownstreamPacketExecution
		return owner.quarantineErrorLocked()
	}
	if owner.callbackParticipantErr == nil {
		owner.state = controllerDownstreamPacketAwaitingResolution
		owner.result = controllerDownstreamPacketResultDelivered
		return ErrInvalidControllerDownstreamPacketOutcome
	}
	if owner.callbackAmbiguous {
		owner.state = controllerDownstreamPacketQuarantined
		owner.quarantine = errors.Join(owner.quarantine, owner.callbackErr)
		return owner.quarantineErrorLocked()
	}
	owner.state = controllerDownstreamPacketAwaitingResolution
	owner.result = controllerDownstreamPacketResultCancelled
	return nil
}

// Resolve authenticates the observed terminal fact. Every non-delivered result
// retains the exact packet epoch and vector for ClaimRetry.
func (owner *ControllerDownstreamPacketExecutionOwner) Resolve(
	claim ControllerDownstreamPacketClaim,
	outcome ControllerDownstreamPacketOutcome,
) error {
	credential, err := owner.prepareResolution(claim, outcome)
	if err != nil {
		return err
	}
	return owner.commitPreparedResolution(credential)
}

// controllerDownstreamPacketResolutionCredential is an unexported final
// terminal credential. prepareResolution moves the executor into a state which
// rejects Execute, CancelAndDrain, and every other resolution. A composed owner
// may then commit its already-admitted canonical state and call
// commitPreparedResolution without a snapshot-to-resolution race.
type controllerDownstreamPacketResolutionCredential struct {
	owner         *ControllerDownstreamPacketExecutionOwner
	token         uint64
	epoch         uint64
	count         uint8
	outcome       ControllerDownstreamPacketOutcome
	previousState controllerDownstreamPacketOwnerState
	result        controllerDownstreamPacketExecutionResult
}

func (owner *ControllerDownstreamPacketExecutionOwner) prepareResolution(
	claim ControllerDownstreamPacketClaim,
	outcome ControllerDownstreamPacketOutcome,
) (controllerDownstreamPacketResolutionCredential, error) {
	if !owner.initialized() {
		return controllerDownstreamPacketResolutionCredential{},
			ErrControllerDownstreamPacketExecutorUninitialized
	}
	<-owner.operation
	defer func() { owner.operation <- struct{}{} }()
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if err := owner.validateClaimLocked(claim); err != nil {
		return controllerDownstreamPacketResolutionCredential{}, err
	}
	if outcome < ControllerDownstreamPacketDelivered ||
		outcome > ControllerDownstreamPacketExecutionCancelled {
		return controllerDownstreamPacketResolutionCredential{},
			ErrInvalidControllerDownstreamPacketOutcome
	}

	valid := false
	switch outcome {
	case ControllerDownstreamPacketDeferred:
		valid = owner.state == controllerDownstreamPacketClaimed ||
			owner.state == controllerDownstreamPacketAdmitted
	case ControllerDownstreamPacketDelivered:
		valid = owner.state == controllerDownstreamPacketAwaitingResolution &&
			owner.result == controllerDownstreamPacketResultDelivered
	case ControllerDownstreamPacketDeliveryFailed:
		valid = owner.state == controllerDownstreamPacketAwaitingResolution &&
			owner.result == controllerDownstreamPacketResultRejected
	case ControllerDownstreamPacketExecutionCancelled:
		valid = owner.state == controllerDownstreamPacketAwaitingResolution &&
			owner.result == controllerDownstreamPacketResultCancelled
	}
	if !valid {
		if outcome == ControllerDownstreamPacketDelivered &&
			owner.state == controllerDownstreamPacketClaimed {
			return controllerDownstreamPacketResolutionCredential{},
				ErrControllerDownstreamPacketClaimNotAdmitted
		}
		return controllerDownstreamPacketResolutionCredential{},
			ErrInvalidControllerDownstreamPacketOutcome
	}
	credential := controllerDownstreamPacketResolutionCredential{
		owner: owner, token: owner.claimToken,
		epoch: owner.execution.packetEpoch, count: owner.execution.length,
		outcome: outcome, previousState: owner.state, result: owner.result,
	}
	owner.state = controllerDownstreamPacketResolutionPrepared
	return credential, nil
}

func (owner *ControllerDownstreamPacketExecutionOwner) commitPreparedResolution(
	credential controllerDownstreamPacketResolutionCredential,
) error {
	if !owner.initialized() {
		return ErrControllerDownstreamPacketExecutorUninitialized
	}
	<-owner.operation
	defer func() { owner.operation <- struct{}{} }()
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if credential.owner != owner || credential.token == 0 ||
		credential.token != owner.claimToken ||
		credential.epoch != owner.execution.packetEpoch ||
		credential.count != owner.execution.length ||
		credential.outcome < ControllerDownstreamPacketDelivered ||
		credential.outcome > ControllerDownstreamPacketExecutionCancelled ||
		owner.state != controllerDownstreamPacketResolutionPrepared ||
		credential.result != owner.result {
		return ErrInvalidControllerDownstreamPacketOutcome
	}

	owner.claimToken = 0
	owner.result = controllerDownstreamPacketNoResult
	owner.callbackDone = false
	owner.callbackParticipantErr = nil
	owner.callbackErr = nil
	owner.callbackReturn = time.Time{}
	owner.callbackAmbiguous = false
	if credential.outcome == ControllerDownstreamPacketDelivered {
		owner.execution = ControllerDownstreamPacketExecution{}
		owner.state = controllerDownstreamPacketIdle
	} else {
		owner.state = controllerDownstreamPacketRetryPending
	}
	return nil
}

// Snapshot returns a value-only diagnostic view and never calls participant.
func (owner *ControllerDownstreamPacketExecutionOwner) Snapshot() (
	ControllerDownstreamPacketExecutionSnapshot,
	bool,
) {
	if !owner.initialized() {
		return ControllerDownstreamPacketExecutionSnapshot{}, false
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return ControllerDownstreamPacketExecutionSnapshot{
		PacketEpoch: owner.execution.packetEpoch, ClaimToken: owner.claimToken,
		MessageCount:    owner.execution.length,
		NextPacketEpoch: owner.nextPacketEpoch, NextClaimToken: owner.nextClaimToken,
		Idle:     owner.state == controllerDownstreamPacketIdle,
		Claimed:  owner.state == controllerDownstreamPacketClaimed,
		Admitted: owner.state == controllerDownstreamPacketAdmitted,
		ExecutionInFlight: owner.state == controllerDownstreamPacketExecuting ||
			owner.state == controllerDownstreamPacketCancelling,
		AwaitingResolution: owner.state == controllerDownstreamPacketAwaitingResolution,
		RetryPending:       owner.state == controllerDownstreamPacketRetryPending,
		DrainRequired:      owner.state == controllerDownstreamPacketDrainRequired,
		ResolutionPrepared: owner.state == controllerDownstreamPacketResolutionPrepared,
		Quarantined:        owner.state == controllerDownstreamPacketQuarantined,
		Delivered:          owner.result == controllerDownstreamPacketResultDelivered,
		Rejected:           owner.result == controllerDownstreamPacketResultRejected,
		Cancelled:          owner.result == controllerDownstreamPacketResultCancelled,
	}, true
}

// initialized checks only construction-time immutable fields. Reading mutable
// state here would race with an active execution; the serializer and mu protect
// that state after this fail-fast guard. The exported owner's zero and copied
// values must reject rather than blocking on nil/shared serializer channels.
func (owner *ControllerDownstreamPacketExecutionOwner) initialized() bool {
	return owner != nil && owner.self == owner && owner.operation != nil &&
		owner.progress != nil && owner.participant != nil
}

func compileControllerDownstreamPacketExecution(
	packet ControllerDownstreamPacket,
	packetEpoch uint64,
) (ControllerDownstreamPacketExecution, error) {
	if packetEpoch == 0 || packet.Len() == 0 ||
		packet.Len() > controllerDownstreamPacketMaximumMessages {
		return ControllerDownstreamPacketExecution{},
			ErrInvalidControllerDownstreamPacketClaim
	}
	execution := ControllerDownstreamPacketExecution{
		packetEpoch: packetEpoch, length: uint8(packet.Len()),
	}
	for index := 0; index < packet.Len(); index++ {
		message, ok := packet.Message(index)
		if !ok {
			return ControllerDownstreamPacketExecution{},
				ErrInvalidControllerDownstreamPacketClaim
		}
		action := ControllerDownstreamPacketAction{
			WireIndex: uint8(index), Sequence: message.Sequence,
		}
		switch message.Kind {
		case ControllerDownstreamDirectMotor:
			action.Action = ControllerPersonaApplyDirectMotor
			action.DirectMotor = message.DirectMotor
		case ControllerDownstreamGuideLED:
			action.Action = ControllerPersonaApplyGuideLED
			action.GuideLED = message.GuideLED
		case ControllerDownstreamLifecycle,
			ControllerDownstreamProtocolControlACK:
			return ControllerDownstreamPacketExecution{}, fmt.Errorf(
				"%w: index=%d kind=%d",
				ErrControllerDownstreamPacketNonAtomicAction, index, message.Kind)
		default:
			return ControllerDownstreamPacketExecution{}, fmt.Errorf(
				"%w: index=%d kind=%d",
				ErrControllerDownstreamPacketNonAtomicAction, index, message.Kind)
		}
		if !action.valid() {
			return ControllerDownstreamPacketExecution{},
				ErrInvalidControllerDownstreamPacketClaim
		}
		execution.actions[index] = action
	}
	if !execution.valid() {
		return ControllerDownstreamPacketExecution{},
			ErrInvalidControllerDownstreamPacketClaim
	}
	return execution, nil
}

func (owner *ControllerDownstreamPacketExecutionOwner) claimLocked() ControllerDownstreamPacketClaim {
	return ControllerDownstreamPacketClaim{
		owner: owner, token: owner.claimToken,
		epoch: owner.execution.packetEpoch, count: owner.execution.length,
	}
}

func (owner *ControllerDownstreamPacketExecutionOwner) validateClaimLocked(
	claim ControllerDownstreamPacketClaim,
) error {
	if owner.state == controllerDownstreamPacketQuarantined {
		return owner.quarantineErrorLocked()
	}
	if !claim.Valid() || claim.owner != owner || claim.token != owner.claimToken ||
		claim.epoch != owner.execution.packetEpoch ||
		claim.count != owner.execution.length || owner.claimToken == 0 {
		return ErrInvalidControllerDownstreamPacketClaim
	}
	return nil
}

func (owner *ControllerDownstreamPacketExecutionOwner) validateExecutionLocked(
	lease ControllerDownstreamPacketExecutionLease,
) error {
	if owner.state == controllerDownstreamPacketQuarantined {
		return owner.quarantineErrorLocked()
	}
	if !lease.Valid() || lease.owner != owner || lease.token != owner.claimToken ||
		lease.execution != owner.execution || owner.claimToken == 0 {
		return ErrInvalidControllerDownstreamPacketExecution
	}
	return nil
}

func (owner *ControllerDownstreamPacketExecutionOwner) waitForCallback(
	deadline time.Time,
) error {
	for {
		owner.mu.Lock()
		done := owner.callbackDone
		owner.mu.Unlock()
		if done {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ErrControllerDownstreamPacketDeadline
		}
		timer := time.NewTimer(remaining)
		select {
		case <-owner.progress:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			return ErrControllerDownstreamPacketDeadline
		}
	}
}

func (owner *ControllerDownstreamPacketExecutionOwner) signalProgress() {
	select {
	case owner.progress <- struct{}{}:
	default:
	}
}

func (owner *ControllerDownstreamPacketExecutionOwner) quarantineOwner(err error) {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if err == nil {
		err = ErrInvalidControllerDownstreamPacketExecution
	}
	owner.state = controllerDownstreamPacketQuarantined
	owner.quarantine = errors.Join(owner.quarantine, err)
}

func (owner *ControllerDownstreamPacketExecutionOwner) quarantineError() error {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return owner.quarantineErrorLocked()
}

func (owner *ControllerDownstreamPacketExecutionOwner) quarantineErrorLocked() error {
	return errors.Join(ErrControllerDownstreamPacketQuarantined, owner.quarantine)
}

func invokeControllerDownstreamPacketPreflight(
	participant ControllerDownstreamPacketAtomicParticipant,
	execution ControllerDownstreamPacketExecution,
) (err error, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = downstreamPacketParticipantPanicError(recovered)
			panicked = true
		}
	}()
	return participant.PreflightControllerDownstreamPacket(execution), false
}

func invokeControllerDownstreamPacketExecute(
	participant ControllerDownstreamPacketAtomicParticipant,
	execution ControllerDownstreamPacketExecution,
	deadline time.Time,
) (err error, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = downstreamPacketParticipantPanicError(recovered)
			panicked = true
		}
	}()
	return participant.ExecuteControllerDownstreamPacket(execution, deadline), false
}

func invokeControllerDownstreamPacketCancelAndDrain(
	participant ControllerDownstreamPacketAtomicParticipant,
	execution ControllerDownstreamPacketExecution,
	deadline time.Time,
) (err error, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = downstreamPacketParticipantPanicError(recovered)
			panicked = true
		}
	}()
	return participant.CancelControllerDownstreamPacketAndDrain(
		execution, deadline), false
}

func downstreamPacketParticipantPanicError(recovered any) error {
	if recoveredErr, ok := recovered.(error); ok {
		return fmt.Errorf("%w: %w",
			ErrControllerDownstreamPacketParticipantPanic, recoveredErr)
	}
	return fmt.Errorf("%w: %v",
		ErrControllerDownstreamPacketParticipantPanic, recovered)
}

func controllerDownstreamPacketDeadlineValid(deadline time.Time) bool {
	return !deadline.IsZero() && deadline.After(time.Now())
}

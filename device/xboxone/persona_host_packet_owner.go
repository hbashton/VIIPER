package xboxone

import (
	"errors"
	"sync"
	"time"
)

type controllerPersonaHostPacketOwnerState uint8

const (
	controllerPersonaHostPacketOwnerIdle controllerPersonaHostPacketOwnerState = iota + 1
	controllerPersonaHostPacketOwnerClaimed
	controllerPersonaHostPacketOwnerAdmitted
	controllerPersonaHostPacketOwnerRetryPending
	controllerPersonaHostPacketOwnerQuarantined
)

// ControllerPersonaDownstreamPacketBatchClaim is the combined capability for
// the canonical persona reservation and the whole-vector external executor.
// Its token changes on retry; PacketEpoch and action values do not.
type ControllerPersonaDownstreamPacketBatchClaim struct {
	owner      *ControllerPersonaDownstreamPacketBatchOwner
	token      uint64
	generation uint64
	epoch      uint64
	count      uint8
}

func (claim ControllerPersonaDownstreamPacketBatchClaim) Valid() bool {
	return claim.owner != nil && claim.token != 0 && claim.generation != 0 &&
		claim.epoch != 0 && claim.count != 0
}
func (claim ControllerPersonaDownstreamPacketBatchClaim) Generation() uint64 {
	return claim.generation
}
func (claim ControllerPersonaDownstreamPacketBatchClaim) PacketEpoch() uint64 {
	return claim.epoch
}
func (claim ControllerPersonaDownstreamPacketBatchClaim) MessageCount() int {
	return int(claim.count)
}

// ControllerPersonaDownstreamPacketBatchLease transfers the exact admitted
// whole-vector execution. It is copyable but valid only for its current batch
// attempt token.
type ControllerPersonaDownstreamPacketBatchLease struct {
	owner     *ControllerPersonaDownstreamPacketBatchOwner
	token     uint64
	execution ControllerDownstreamPacketExecutionLease
}

func (lease ControllerPersonaDownstreamPacketBatchLease) Valid() bool {
	return lease.owner != nil && lease.token != 0 && lease.execution.Valid()
}
func (lease ControllerPersonaDownstreamPacketBatchLease) PacketEpoch() uint64 {
	return lease.execution.PacketEpoch()
}
func (lease ControllerPersonaDownstreamPacketBatchLease) Len() int {
	return lease.execution.Len()
}
func (lease ControllerPersonaDownstreamPacketBatchLease) Action(
	index int,
) (ControllerDownstreamPacketAction, bool) {
	return lease.execution.Action(index)
}

// ControllerPersonaDownstreamPacketBatchSnapshot is diagnostic evidence for
// both canonical ownership layers. It is never an admission capability.
type ControllerPersonaDownstreamPacketBatchSnapshot struct {
	PacketEpoch        uint64
	ClaimToken         uint64
	MessageCount       uint8
	Idle               bool
	Claimed            bool
	Admitted           bool
	ExecutionInFlight  bool
	AwaitingResolution bool
	RetryPending       bool
	DrainRequired      bool
	Quarantined        bool
}

// ControllerPersonaDownstreamPacketBatchOwner is a dormant composition of the
// canonical ControllerPersonaEngine packet lane and the existing bounded
// whole-vector executor. It neither parses USB aggregates nor registers a
// persona. The bound engine must be mutated only through this owner while a
// batch or its retry is outstanding.
type ControllerPersonaDownstreamPacketBatchOwner struct {
	mu        sync.Mutex
	self      *ControllerPersonaDownstreamPacketBatchOwner
	operation chan struct{}
	engine    *ControllerPersonaEngine
	executor  *ControllerDownstreamPacketExecutionOwner
	state     controllerPersonaHostPacketOwnerState

	nextToken   uint64
	claimToken  uint64
	hostClaim   ControllerPersonaHostPacketClaim
	packetClaim ControllerDownstreamPacketClaim
	packetLease ControllerDownstreamPacketExecutionLease
	execution   ControllerDownstreamPacketExecution
	quarantine  error
}

func NewControllerPersonaDownstreamPacketBatchOwner(
	engine *ControllerPersonaEngine,
	participant ControllerDownstreamPacketAtomicParticipant,
) (*ControllerPersonaDownstreamPacketBatchOwner, error) {
	if engine == nil || engine.validateInitialized() != nil || participant == nil {
		return nil, ErrControllerPersonaHostPacketOwnerUninitialized
	}
	if engine.hasClaim || engine.retryPending || engine.ordinaryFeedbackPending() || engine.hostPacketActive ||
		engine.hostPacketRetryPending || engine.hostPacketQuarantined {
		return nil, ErrControllerPersonaHostPacketOwnerBusy
	}
	executor, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		return nil, err
	}
	// The combined owner may be constructed after replay-only engine use. Align
	// the fresh executor's next epoch with the persona's never-reused packet
	// epoch before either owner can escape.
	executor.nextPacketEpoch = engine.nextHostPacketEpoch
	owner := &ControllerPersonaDownstreamPacketBatchOwner{
		operation: make(chan struct{}, 1), engine: engine, executor: executor,
		state: controllerPersonaHostPacketOwnerIdle,
	}
	owner.self = owner
	owner.operation <- struct{}{}
	return owner, nil
}

// Claim reserves every canonical packet member, then performs participant
// preflight on the complete exact vector. A preflight rejection has no external
// effect, but the already-selected persona values become the mandatory batch
// retry; this prevents pretending that a lifecycle/ACK reservation rolled back.
func (owner *ControllerPersonaDownstreamPacketBatchOwner) Claim(
	packet ControllerDownstreamPacket,
	nowMS uint64,
) (ControllerPersonaDownstreamPacketBatchClaim, error) {
	if !owner.initialized() {
		return ControllerPersonaDownstreamPacketBatchClaim{},
			ErrControllerPersonaHostPacketOwnerUninitialized
	}
	<-owner.operation
	defer func() { owner.operation <- struct{}{} }()
	owner.mu.Lock()
	if err := owner.availableForClaimLocked(); err != nil {
		owner.mu.Unlock()
		return ControllerPersonaDownstreamPacketBatchClaim{}, err
	}
	if owner.nextToken == ^uint64(0) {
		owner.mu.Unlock()
		return ControllerPersonaDownstreamPacketBatchClaim{},
			ErrControllerDownstreamPacketTokenExhausted
	}
	owner.mu.Unlock()

	hostClaim, execution, err := owner.engine.claimHostPacket(nowMS, packet)
	if err != nil {
		hostSnapshot, _ := owner.engine.hostPacketSnapshot()
		if hostSnapshot.Quarantined {
			owner.mu.Lock()
			owner.quarantineLocked(err)
			owner.mu.Unlock()
			return ControllerPersonaDownstreamPacketBatchClaim{}, owner.quarantineError()
		}
		return ControllerPersonaDownstreamPacketBatchClaim{}, err
	}
	packetClaim, err := owner.executor.claimCanonicalExecution(execution)
	if err != nil {
		resolveErr := owner.engine.resolveHostPacket(
			hostClaim, ControllerPersonaDeferred, nowMS)
		owner.mu.Lock()
		if resolveErr != nil || owner.executorQuarantined() {
			owner.quarantineLocked(errors.Join(err, resolveErr))
		} else {
			owner.state = controllerPersonaHostPacketOwnerRetryPending
			owner.execution = execution
		}
		owner.mu.Unlock()
		return ControllerPersonaDownstreamPacketBatchClaim{}, errors.Join(err, resolveErr)
	}

	owner.mu.Lock()
	defer owner.mu.Unlock()
	owner.nextToken++
	owner.claimToken = owner.nextToken
	owner.hostClaim = hostClaim
	owner.packetClaim = packetClaim
	owner.execution = execution
	owner.state = controllerPersonaHostPacketOwnerClaimed
	return owner.claimLocked(), nil
}

// ClaimRetry reclaims both exact canonical owners. Participant preflight runs
// before a fresh combined token is allocated. Rejection retains the same batch
// retry and cannot make a successor packet eligible.
func (owner *ControllerPersonaDownstreamPacketBatchOwner) ClaimRetry(
	nowMS uint64,
) (ControllerPersonaDownstreamPacketBatchClaim, error) {
	if !owner.initialized() {
		return ControllerPersonaDownstreamPacketBatchClaim{},
			ErrControllerPersonaHostPacketOwnerUninitialized
	}
	<-owner.operation
	defer func() { owner.operation <- struct{}{} }()
	owner.mu.Lock()
	if owner.state == controllerPersonaHostPacketOwnerQuarantined {
		err := owner.quarantineErrorLocked()
		owner.mu.Unlock()
		return ControllerPersonaDownstreamPacketBatchClaim{}, err
	}
	if owner.state != controllerPersonaHostPacketOwnerRetryPending {
		owner.mu.Unlock()
		return ControllerPersonaDownstreamPacketBatchClaim{},
			ErrControllerPersonaHostPacketRetryRequired
	}
	if owner.nextToken == ^uint64(0) {
		owner.mu.Unlock()
		return ControllerPersonaDownstreamPacketBatchClaim{},
			ErrControllerDownstreamPacketTokenExhausted
	}
	owner.mu.Unlock()

	hostClaim, execution, err := owner.engine.claimHostPacketRetry(nowMS)
	if err != nil {
		hostSnapshot, _ := owner.engine.hostPacketSnapshot()
		if !hostSnapshot.RetryPending {
			owner.mu.Lock()
			owner.quarantineLocked(err)
			owner.mu.Unlock()
			return ControllerPersonaDownstreamPacketBatchClaim{}, owner.quarantineError()
		}
		return ControllerPersonaDownstreamPacketBatchClaim{}, err
	}
	packetSnapshot, _ := owner.executor.Snapshot()
	var packetClaim ControllerDownstreamPacketClaim
	if packetSnapshot.RetryPending {
		packetClaim, err = owner.executor.ClaimRetry()
	} else if packetSnapshot.Idle {
		packetClaim, err = owner.executor.claimCanonicalExecution(execution)
	} else {
		err = ErrControllerPersonaHostPacketOwnerBusy
	}
	if err != nil {
		resolveErr := owner.engine.resolveHostPacket(
			hostClaim, ControllerPersonaDeferred, nowMS)
		owner.mu.Lock()
		if resolveErr != nil || owner.executorQuarantined() {
			owner.quarantineLocked(errors.Join(err, resolveErr))
		} else {
			owner.state = controllerPersonaHostPacketOwnerRetryPending
		}
		owner.mu.Unlock()
		return ControllerPersonaDownstreamPacketBatchClaim{}, errors.Join(err, resolveErr)
	}
	if packetClaim.PacketEpoch() != execution.PacketEpoch() ||
		packetClaim.MessageCount() != execution.Len() {
		owner.mu.Lock()
		owner.quarantineLocked(ErrControllerPersonaInvariantViolation)
		owner.mu.Unlock()
		return ControllerPersonaDownstreamPacketBatchClaim{}, owner.quarantineError()
	}

	owner.mu.Lock()
	defer owner.mu.Unlock()
	owner.nextToken++
	owner.claimToken = owner.nextToken
	owner.hostClaim = hostClaim
	owner.packetClaim = packetClaim
	owner.execution = execution
	owner.state = controllerPersonaHostPacketOwnerClaimed
	return owner.claimLocked(), nil
}

// Admit repeats whole-vector participant preflight before the canonical engine
// crosses its final lifecycle/ACK admission fences. Any rejection retains both
// exact retries; neither participant Execute nor another external effect ran.
func (owner *ControllerPersonaDownstreamPacketBatchOwner) Admit(
	claim ControllerPersonaDownstreamPacketBatchClaim,
	nowMS uint64,
) (ControllerPersonaDownstreamPacketBatchLease, error) {
	if !owner.initialized() {
		return ControllerPersonaDownstreamPacketBatchLease{},
			ErrControllerPersonaHostPacketOwnerUninitialized
	}
	<-owner.operation
	defer func() { owner.operation <- struct{}{} }()
	owner.mu.Lock()
	if err := owner.validateClaimLocked(claim); err != nil {
		owner.mu.Unlock()
		return ControllerPersonaDownstreamPacketBatchLease{}, err
	}
	if owner.state != controllerPersonaHostPacketOwnerClaimed {
		owner.mu.Unlock()
		return ControllerPersonaDownstreamPacketBatchLease{},
			ErrControllerPersonaHostPacketOwnerBusy
	}
	packetClaim := owner.packetClaim
	hostClaim := owner.hostClaim
	execution := owner.execution
	owner.mu.Unlock()

	packetLease, err := owner.executor.Admit(packetClaim)
	if err != nil {
		return ControllerPersonaDownstreamPacketBatchLease{}, err
	}
	hostExecution, hostErr := owner.engine.admitHostPacket(hostClaim, nowMS)
	if hostErr != nil || hostExecution != execution {
		if hostErr == nil {
			hostErr = ErrControllerPersonaInvariantViolation
		}
		packetErr := owner.executor.Resolve(
			packetClaim, ControllerDownstreamPacketDeferred)
		personaErr := owner.engine.resolveHostPacket(
			hostClaim, ControllerPersonaDeferred, nowMS)
		owner.mu.Lock()
		if packetErr != nil || personaErr != nil {
			owner.quarantineLocked(errors.Join(hostErr, packetErr, personaErr))
		} else {
			owner.clearAttemptLocked()
			owner.state = controllerPersonaHostPacketOwnerRetryPending
		}
		owner.mu.Unlock()
		return ControllerPersonaDownstreamPacketBatchLease{},
			errors.Join(hostErr, packetErr, personaErr)
	}

	owner.mu.Lock()
	defer owner.mu.Unlock()
	if err := owner.validateClaimLocked(claim); err != nil {
		owner.quarantineLocked(err)
		return ControllerPersonaDownstreamPacketBatchLease{}, owner.quarantineErrorLocked()
	}
	owner.packetLease = packetLease
	owner.state = controllerPersonaHostPacketOwnerAdmitted
	return ControllerPersonaDownstreamPacketBatchLease{
		owner: owner, token: owner.claimToken, execution: packetLease,
	}, nil
}

func (owner *ControllerPersonaDownstreamPacketBatchOwner) Execute(
	lease ControllerPersonaDownstreamPacketBatchLease,
	deadline time.Time,
) error {
	if !owner.initialized() {
		return ErrControllerPersonaHostPacketOwnerUninitialized
	}
	owner.mu.Lock()
	if err := owner.validateLeaseLocked(lease); err != nil {
		owner.mu.Unlock()
		return err
	}
	packetLease := owner.packetLease
	owner.mu.Unlock()
	return owner.executor.Execute(packetLease, deadline)
}

func (owner *ControllerPersonaDownstreamPacketBatchOwner) CancelAndDrain(
	lease ControllerPersonaDownstreamPacketBatchLease,
	deadline time.Time,
) error {
	if !owner.initialized() {
		return ErrControllerPersonaHostPacketOwnerUninitialized
	}
	owner.mu.Lock()
	if err := owner.validateLeaseLocked(lease); err != nil {
		owner.mu.Unlock()
		return err
	}
	packetLease := owner.packetLease
	owner.mu.Unlock()
	return owner.executor.CancelAndDrain(packetLease, deadline)
}

// Resolve commits one identical terminal fact to both canonical owners. It
// first prepares an executor terminal credential which fences every competing
// effect/resolution; a divergence during either commit permanently quarantines
// the combined owner instead of enabling replay.
func (owner *ControllerPersonaDownstreamPacketBatchOwner) Resolve(
	claim ControllerPersonaDownstreamPacketBatchClaim,
	outcome ControllerDownstreamPacketOutcome,
	completedMS uint64,
) error {
	if !owner.initialized() {
		return ErrControllerPersonaHostPacketOwnerUninitialized
	}
	<-owner.operation
	defer func() { owner.operation <- struct{}{} }()
	owner.mu.Lock()
	if err := owner.validateClaimLocked(claim); err != nil {
		owner.mu.Unlock()
		return err
	}
	packetClaim := owner.packetClaim
	hostClaim := owner.hostClaim
	owner.mu.Unlock()

	credential, err := owner.executor.prepareResolution(packetClaim, outcome)
	if err != nil {
		return err
	}
	personaOutcome := ControllerPersonaDeliveryFailed
	switch outcome {
	case ControllerDownstreamPacketDelivered:
		personaOutcome = ControllerPersonaDelivered
	case ControllerDownstreamPacketDeferred:
		personaOutcome = ControllerPersonaDeferred
	case ControllerDownstreamPacketDeliveryFailed:
		personaOutcome = ControllerPersonaDeliveryFailed
	case ControllerDownstreamPacketExecutionCancelled:
		personaOutcome = ControllerPersonaExecutionCancelled
	default:
		return ErrInvalidControllerDownstreamPacketOutcome
	}
	personaErr := owner.engine.resolveHostPacket(hostClaim, personaOutcome, completedMS)
	if personaErr != nil {
		owner.mu.Lock()
		owner.quarantineLocked(personaErr)
		owner.mu.Unlock()
		return owner.quarantineError()
	}
	packetErr := owner.executor.commitPreparedResolution(credential)
	if packetErr != nil {
		owner.mu.Lock()
		owner.quarantineLocked(packetErr)
		owner.mu.Unlock()
		return owner.quarantineError()
	}

	owner.mu.Lock()
	defer owner.mu.Unlock()
	owner.clearAttemptLocked()
	if outcome == ControllerDownstreamPacketDelivered {
		owner.execution = ControllerDownstreamPacketExecution{}
		owner.state = controllerPersonaHostPacketOwnerIdle
	} else {
		owner.state = controllerPersonaHostPacketOwnerRetryPending
	}
	return nil
}

func (owner *ControllerPersonaDownstreamPacketBatchOwner) Snapshot() (
	ControllerPersonaDownstreamPacketBatchSnapshot,
	bool,
) {
	if !owner.initialized() {
		return ControllerPersonaDownstreamPacketBatchSnapshot{}, false
	}
	packet, _ := owner.executor.Snapshot()
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return ControllerPersonaDownstreamPacketBatchSnapshot{
		PacketEpoch: owner.execution.packetEpoch, ClaimToken: owner.claimToken,
		MessageCount:       owner.execution.length,
		Idle:               owner.state == controllerPersonaHostPacketOwnerIdle,
		Claimed:            owner.state == controllerPersonaHostPacketOwnerClaimed,
		Admitted:           owner.state == controllerPersonaHostPacketOwnerAdmitted,
		ExecutionInFlight:  packet.ExecutionInFlight,
		AwaitingResolution: packet.AwaitingResolution,
		RetryPending:       owner.state == controllerPersonaHostPacketOwnerRetryPending,
		DrainRequired:      packet.DrainRequired,
		Quarantined: owner.state == controllerPersonaHostPacketOwnerQuarantined ||
			packet.Quarantined,
	}, true
}

func (owner *ControllerPersonaDownstreamPacketBatchOwner) initialized() bool {
	return owner != nil && owner.self == owner && owner.operation != nil &&
		owner.engine != nil && owner.executor != nil
}

func (owner *ControllerPersonaDownstreamPacketBatchOwner) availableForClaimLocked() error {
	if owner.state == controllerPersonaHostPacketOwnerQuarantined {
		return owner.quarantineErrorLocked()
	}
	if owner.executorQuarantined() {
		owner.quarantineLocked(ErrControllerDownstreamPacketQuarantined)
		return owner.quarantineErrorLocked()
	}
	if owner.state == controllerPersonaHostPacketOwnerRetryPending {
		return ErrControllerPersonaHostPacketRetryRequired
	}
	if owner.state != controllerPersonaHostPacketOwnerIdle {
		return ErrControllerPersonaHostPacketOwnerBusy
	}
	return nil
}

func (owner *ControllerPersonaDownstreamPacketBatchOwner) claimLocked() ControllerPersonaDownstreamPacketBatchClaim {
	return ControllerPersonaDownstreamPacketBatchClaim{
		owner: owner, token: owner.claimToken,
		generation: owner.hostClaim.generation,
		epoch:      owner.execution.packetEpoch, count: owner.execution.length,
	}
}

func (owner *ControllerPersonaDownstreamPacketBatchOwner) validateClaimLocked(
	claim ControllerPersonaDownstreamPacketBatchClaim,
) error {
	if owner.state == controllerPersonaHostPacketOwnerQuarantined {
		return owner.quarantineErrorLocked()
	}
	if owner.executorQuarantined() {
		owner.quarantineLocked(ErrControllerDownstreamPacketQuarantined)
		return owner.quarantineErrorLocked()
	}
	if !claim.Valid() || claim.owner != owner || claim.token != owner.claimToken ||
		claim.generation != owner.hostClaim.generation ||
		claim.epoch != owner.execution.packetEpoch ||
		claim.count != owner.execution.length || owner.claimToken == 0 {
		return ErrInvalidControllerPersonaHostPacketClaim
	}
	return nil
}

func (owner *ControllerPersonaDownstreamPacketBatchOwner) validateLeaseLocked(
	lease ControllerPersonaDownstreamPacketBatchLease,
) error {
	if owner.state == controllerPersonaHostPacketOwnerQuarantined {
		return owner.quarantineErrorLocked()
	}
	if owner.executorQuarantined() {
		owner.quarantineLocked(ErrControllerDownstreamPacketQuarantined)
		return owner.quarantineErrorLocked()
	}
	if owner.state != controllerPersonaHostPacketOwnerAdmitted || !lease.Valid() ||
		lease.owner != owner || lease.token != owner.claimToken ||
		lease.execution != owner.packetLease {
		return ErrInvalidControllerDownstreamPacketExecution
	}
	return nil
}

func (owner *ControllerPersonaDownstreamPacketBatchOwner) clearAttemptLocked() {
	owner.claimToken = 0
	owner.hostClaim = ControllerPersonaHostPacketClaim{}
	owner.packetClaim = ControllerDownstreamPacketClaim{}
	owner.packetLease = ControllerDownstreamPacketExecutionLease{}
}

func (owner *ControllerPersonaDownstreamPacketBatchOwner) executorQuarantined() bool {
	snapshot, ok := owner.executor.Snapshot()
	return !ok || snapshot.Quarantined
}

func (owner *ControllerPersonaDownstreamPacketBatchOwner) quarantineLocked(err error) {
	owner.state = controllerPersonaHostPacketOwnerQuarantined
	owner.quarantine = errors.Join(owner.quarantine, err)
}

func (owner *ControllerPersonaDownstreamPacketBatchOwner) quarantineError() error {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return owner.quarantineErrorLocked()
}

func (owner *ControllerPersonaDownstreamPacketBatchOwner) quarantineErrorLocked() error {
	return errors.Join(ErrControllerPersonaHostPacketQuarantined, owner.quarantine)
}

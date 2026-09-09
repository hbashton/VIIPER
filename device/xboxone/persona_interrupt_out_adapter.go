package xboxone

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alia5/VIIPER/usb"
)

var (
	ErrControllerPersonaInterruptOutUninitialized = errors.New(
		"xboxone: controller persona interrupt OUT adapter is uninitialized")
	ErrControllerPersonaInterruptOutBusy = errors.New(
		"xboxone: controller persona interrupt OUT adapter is busy")
	ErrControllerPersonaInterruptOutClaim = errors.New(
		"xboxone: invalid controller persona interrupt OUT claim")
	ErrControllerPersonaInterruptOutWorkTicket = errors.New(
		"xboxone: invalid controller persona interrupt OUT work ticket")
	ErrControllerPersonaInterruptOutDeadline = errors.New(
		"xboxone: controller persona interrupt OUT worker deadline is invalid")
	ErrControllerPersonaInterruptOutTokenExhausted = errors.New(
		"xboxone: controller persona interrupt OUT token exhausted")
	ErrControllerPersonaInterruptOutRetryUnsupported = errors.New(
		"xboxone: controller persona interrupt OUT retry requires a proven receive replay authority")
	ErrControllerPersonaInterruptOutQuarantined = errors.New(
		"xboxone: controller persona interrupt OUT adapter is quarantined")
	// Complete is a response-writer boundary. Preconstruct its two combined
	// terminal errors so neither valid nor fatal completion allocates.
	errControllerPersonaInterruptOutTerminalClaim = errors.Join(
		ErrControllerPersonaInterruptOutQuarantined,
		ErrControllerPersonaInterruptOutClaim)
	errControllerPersonaInterruptOutTerminalBusy = errors.Join(
		ErrControllerPersonaInterruptOutQuarantined,
		ErrControllerPersonaInterruptOutBusy)
)

type controllerPersonaInterruptOutSaturatingCounter struct {
	value atomic.Uint64
}

func (counter *controllerPersonaInterruptOutSaturatingCounter) next() (
	uint64,
	bool,
) {
	for {
		current := counter.value.Load()
		if current == ^uint64(0) {
			return 0, false
		}
		if counter.value.CompareAndSwap(current, current+1) {
			return current + 1, true
		}
	}
}

var controllerPersonaInterruptOutGeneration controllerPersonaInterruptOutSaturatingCounter

type controllerPersonaInterruptOutState uint8

const (
	controllerPersonaInterruptOutIdle controllerPersonaInterruptOutState = iota + 1
	controllerPersonaInterruptOutClaiming
	controllerPersonaInterruptOutClaimed
	controllerPersonaInterruptOutAdmitting
	controllerPersonaInterruptOutAdmissionRejected
	controllerPersonaInterruptOutAdmitted
	controllerPersonaInterruptOutCompleting
	controllerPersonaInterruptOutPending
	controllerPersonaInterruptOutRunning
	controllerPersonaInterruptOutResolving
	controllerPersonaInterruptOutResolvingTerminal
	controllerPersonaInterruptOutFinishing
	controllerPersonaInterruptOutQuarantined
)

type controllerPersonaInterruptOutAdmissionShape uint8

const (
	controllerPersonaInterruptOutAdmissionShapeRejected controllerPersonaInterruptOutAdmissionShape = iota + 1
	controllerPersonaInterruptOutAdmissionShapeAccepted
	controllerPersonaInterruptOutAdmissionShapeInvalid
	controllerPersonaInterruptOutAdmissionShapeContradictory
)

type controllerPersonaInterruptOutClaimShape uint8

const (
	controllerPersonaInterruptOutClaimShapeRejected controllerPersonaInterruptOutClaimShape = iota + 1
	controllerPersonaInterruptOutClaimShapeAccepted
	controllerPersonaInterruptOutClaimShapeInvalid
	controllerPersonaInterruptOutClaimShapeContradictory
)

// ControllerPersonaInterruptOutWorkTicket is the opaque, copyable credential
// published by CompleteInterruptOutTransaction. It authenticates exactly one
// retained completion slot. A copied ticket does not authorize a second run.
type ControllerPersonaInterruptOutWorkTicket struct {
	owner      *DormantControllerPersonaInterruptOutAdapter
	token      uint64
	generation uint64
	batchEpoch uint64
}

func (ticket ControllerPersonaInterruptOutWorkTicket) Valid() bool {
	return ticket.owner != nil && ticket.token != 0 && ticket.generation != 0
}

func (ticket ControllerPersonaInterruptOutWorkTicket) BatchEpoch() uint64 {
	return ticket.batchEpoch
}

// ControllerPersonaInterruptOutSnapshot is value-only diagnostic evidence. It
// is not an admission, completion, or worker capability.
type ControllerPersonaInterruptOutSnapshot struct {
	Generation   uint64
	USBToken     uint64
	WorkToken    uint64
	BatchEpoch   uint64
	Idle         bool
	Claimed      bool
	Admitted     bool
	Pending      bool
	Running      bool
	Quarantined  bool
	RetryBlocked bool
}

type controllerPersonaInterruptOutSlot struct {
	usbClaim   usb.InterruptOutTransactionClaim
	batchClaim ControllerPersonaDownstreamPacketBatchClaim
	batchLease ControllerPersonaDownstreamPacketBatchLease
	workTicket ControllerPersonaInterruptOutWorkTicket
}

// DormantControllerPersonaInterruptOutAdapter is the packet-boundary seam
// between usb.TransactionalInterruptOutDevice and the canonical downstream
// packet-batch owner. It owns endpoint 1 only. It is deliberately not a USB
// Device, is absent from the registry and public library, and has no descriptor
// or transport construction path.
//
// Construction transfers exclusive use of batchOwner to this adapter. Claim
// and Admit perform only bounded decode, canonical reservation, and participant
// preflight. Complete performs no owner call, callback, wait, allocation, or
// I/O: it moves the fixed slot to Pending and latches workReady. RunPending is
// the sole worker-facing execution and resolution boundary.
type DormantControllerPersonaInterruptOutAdapter struct {
	mu        sync.Mutex
	self      *DormantControllerPersonaInterruptOutAdapter
	operation chan struct{}
	workReady chan struct{}
	// terminalFence is the permanent wait-free containment latch. Every
	// competing Complete sets it, including after work publication. No later
	// worker or claim may cross this fence; retained owner authority is left for
	// explicit terminal attention.
	terminalFence atomic.Bool
	// lastCompletionToken is never reset. Claim tokens are monotonic and never
	// reused, so a copied Complete from an older attempt is terminal-fenced
	// before it can inspect or interfere with a successor slot.
	lastCompletionToken atomic.Uint64

	generation uint64
	nextToken  uint64
	batchOwner *ControllerPersonaDownstreamPacketBatchOwner
	origin     time.Time
	originMS   uint64

	state atomic.Uint32
	// completionOutcome is published before state becomes Pending. The work
	// ticket itself is preallocated with the claim, so Complete writes no
	// mutex-protected slot field and diagnostics cannot obstruct publication.
	completionOutcome atomic.Uint32
	slot              controllerPersonaInterruptOutSlot
	quarantine        error
	retryBlock        bool
}

// NewDormantControllerPersonaInterruptOutAdapter constructs an offline-only
// adapter. origin/originMS define the monotonic protocol clock domain already
// used to construct the persona. Construction starts no goroutine and performs
// no participant callback or I/O.
func NewDormantControllerPersonaInterruptOutAdapter(
	batchOwner *ControllerPersonaDownstreamPacketBatchOwner,
	origin time.Time,
	originMS uint64,
) (*DormantControllerPersonaInterruptOutAdapter, error) {
	if batchOwner == nil || !batchOwner.initialized() || origin.IsZero() ||
		origin.After(time.Now()) {
		return nil, ErrControllerPersonaInterruptOutUninitialized
	}
	snapshot, ok := batchOwner.Snapshot()
	if !ok || !snapshot.Idle || snapshot.Quarantined || snapshot.RetryPending {
		return nil, ErrControllerPersonaInterruptOutBusy
	}
	generation, available := controllerPersonaInterruptOutGeneration.next()
	if !available {
		return nil, ErrControllerPersonaInterruptOutTokenExhausted
	}
	adapter := &DormantControllerPersonaInterruptOutAdapter{
		operation: make(chan struct{}, 1), workReady: make(chan struct{}, 1),
		generation: generation, batchOwner: batchOwner,
		origin: origin, originMS: originMS,
	}
	adapter.self = adapter
	adapter.storeState(controllerPersonaInterruptOutIdle)
	adapter.operation <- struct{}{}
	return adapter, nil
}

// ClaimInterruptOutTransaction decodes exactly one complete endpoint-1 USB
// packet with the existing canonical decoder. Other endpoints remain
// Unhandled. A malformed, fragmented, truncated, oversized, or unsupported
// endpoint-1 packet becomes an exact no-effect Stall claim; this adapter never
// guesses a fragment boundary or receive replay.
func (adapter *DormantControllerPersonaInterruptOutAdapter) ClaimInterruptOutTransaction(
	request usb.InterruptOutTransactionRequest,
) (usb.InterruptOutTransactionClaim, error) {
	if !adapter.initialized() {
		return usb.InterruptOutTransactionClaim{},
			ErrControllerPersonaInterruptOutUninitialized
	}
	if request.Endpoint != 1 {
		return usb.InterruptOutTransactionClaim{}, nil
	}
	if adapter.terminalFence.Load() {
		return usb.InterruptOutTransactionClaim{},
			ErrControllerPersonaInterruptOutQuarantined
	}
	if cap(request.Data) != len(request.Data) {
		return usb.InterruptOutTransactionClaim{},
			ErrControllerPersonaInterruptOutClaim
	}
	packet, decodeErr := DecodeControllerDownstreamPacket(request.Data)
	if !adapter.tryBeginOperation() {
		return usb.InterruptOutTransactionClaim{},
			ErrControllerPersonaInterruptOutBusy
	}
	defer adapter.endOperation()

	adapter.mu.Lock()
	if adapter.terminalFence.Load() ||
		adapter.loadState() != controllerPersonaInterruptOutIdle ||
		adapter.quarantine != nil || adapter.retryBlock {
		err := adapter.stateErrorLocked()
		adapter.mu.Unlock()
		return usb.InterruptOutTransactionClaim{}, err
	}
	if adapter.nextToken == ^uint64(0) {
		adapter.transitionToQuarantined()
		adapter.quarantine = ErrControllerPersonaInterruptOutTokenExhausted
		adapter.mu.Unlock()
		return usb.InterruptOutTransactionClaim{},
			adapter.quarantineError()
	}
	proposedToken := adapter.nextToken + 1
	if !adapter.compareState(controllerPersonaInterruptOutIdle,
		controllerPersonaInterruptOutClaiming) {
		err := adapter.stateErrorLocked()
		adapter.mu.Unlock()
		return usb.InterruptOutTransactionClaim{}, err
	}
	adapter.mu.Unlock()

	result := usb.InterruptOutTransactionStall
	var batchClaim ControllerPersonaDownstreamPacketBatchClaim
	if decodeErr == nil {
		nowMS, err := adapter.protocolMilliseconds(time.Now())
		if err != nil {
			adapter.quarantineClaimFailure(
				ControllerPersonaDownstreamPacketBatchClaim{}, err)
			return usb.InterruptOutTransactionClaim{}, err
		}
		batchClaim, err = adapter.batchOwner.Claim(packet, nowMS)
		shape := classifyControllerPersonaInterruptOutClaim(
			batchClaim.Valid(), err)
		if shape != controllerPersonaInterruptOutClaimShapeAccepted ||
			!adapter.exactBatchClaimForPacket(batchClaim, packet) {
			// Batch Claim can retain a mandatory canonical retry after participant
			// preflight rejects. Invalid and contradictory result shapes are also
			// retained. With no proven USB receive-replay credential, the only
			// truthful result is zero USB claim plus a terminal retained fence.
			claimErr := err
			if shape != controllerPersonaInterruptOutClaimShapeRejected {
				claimErr = errors.Join(claimErr,
					ErrControllerPersonaInterruptOutClaim)
			}
			adapter.quarantineClaimFailure(batchClaim, claimErr)
			return usb.InterruptOutTransactionClaim{},
				errors.Join(ErrControllerPersonaInterruptOutQuarantined,
					claimErr)
		}
		result = usb.InterruptOutTransactionAccepted
	}

	claim := usb.InterruptOutTransactionClaim{
		Token: proposedToken, Generation: adapter.generation, Result: result,
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.terminalFence.Load() {
		adapter.slot = controllerPersonaInterruptOutSlot{batchClaim: batchClaim}
		adapter.transitionToQuarantined()
		adapter.quarantine = errors.Join(adapter.quarantine,
			ErrControllerPersonaInterruptOutQuarantined)
		return usb.InterruptOutTransactionClaim{}, adapter.quarantineErrorLocked()
	}
	if adapter.loadState() != controllerPersonaInterruptOutClaiming ||
		adapter.nextToken+1 != proposedToken {
		adapter.transitionToQuarantined()
		adapter.quarantine = errors.Join(adapter.quarantine,
			ErrControllerPersonaInterruptOutClaim)
		return usb.InterruptOutTransactionClaim{}, adapter.quarantineErrorLocked()
	}
	adapter.nextToken = proposedToken
	workTicket := ControllerPersonaInterruptOutWorkTicket{
		owner: adapter, token: claim.Token, generation: claim.Generation,
		batchEpoch: batchClaim.PacketEpoch(),
	}
	adapter.slot = controllerPersonaInterruptOutSlot{
		usbClaim: claim, batchClaim: batchClaim, workTicket: workTicket,
	}
	adapter.completionOutcome.Store(0)
	if !adapter.compareState(controllerPersonaInterruptOutClaiming,
		controllerPersonaInterruptOutClaimed) {
		adapter.slot = controllerPersonaInterruptOutSlot{batchClaim: batchClaim}
		adapter.transitionToQuarantined()
		adapter.quarantine = errors.Join(adapter.quarantine,
			ErrControllerPersonaInterruptOutClaim)
		return usb.InterruptOutTransactionClaim{}, adapter.quarantineErrorLocked()
	}
	return claim, nil
}

// AdmitInterruptOutTransaction authenticates the exact numeric USB claim. An
// Accepted claim is additionally authenticated to its private batch claim and
// adopts the exact resulting batch lease. It performs no participant effect.
func (adapter *DormantControllerPersonaInterruptOutAdapter) AdmitInterruptOutTransaction(
	claim usb.InterruptOutTransactionClaim,
) error {
	if !adapter.initialized() {
		return ErrControllerPersonaInterruptOutUninitialized
	}
	if adapter.terminalFence.Load() {
		return ErrControllerPersonaInterruptOutQuarantined
	}
	if !adapter.tryBeginOperation() {
		return ErrControllerPersonaInterruptOutBusy
	}
	defer adapter.endOperation()

	adapter.mu.Lock()
	if adapter.terminalFence.Load() {
		adapter.recordViolationLocked(ErrControllerPersonaInterruptOutQuarantined)
		adapter.mu.Unlock()
		return ErrControllerPersonaInterruptOutQuarantined
	}
	if !adapter.exactUSBClaimLocked(claim) ||
		adapter.loadState() != controllerPersonaInterruptOutClaimed {
		adapter.recordViolationLocked(ErrControllerPersonaInterruptOutClaim)
		adapter.mu.Unlock()
		return ErrControllerPersonaInterruptOutClaim
	}
	if claim.Result == usb.InterruptOutTransactionStall {
		if !adapter.compareState(controllerPersonaInterruptOutClaimed,
			controllerPersonaInterruptOutAdmitted) {
			adapter.recordViolationLocked(ErrControllerPersonaInterruptOutClaim)
			adapter.mu.Unlock()
			return ErrControllerPersonaInterruptOutClaim
		}
		adapter.mu.Unlock()
		return nil
	}
	if claim.Result != usb.InterruptOutTransactionAccepted ||
		!adapter.slot.batchClaim.Valid() {
		adapter.recordViolationLocked(ErrControllerPersonaInterruptOutClaim)
		adapter.mu.Unlock()
		return ErrControllerPersonaInterruptOutClaim
	}
	batchClaim := adapter.slot.batchClaim
	if !adapter.compareState(controllerPersonaInterruptOutClaimed,
		controllerPersonaInterruptOutAdmitting) {
		adapter.recordViolationLocked(ErrControllerPersonaInterruptOutClaim)
		adapter.mu.Unlock()
		return ErrControllerPersonaInterruptOutClaim
	}
	adapter.mu.Unlock()

	nowMS, clockErr := adapter.protocolMilliseconds(time.Now())
	var lease ControllerPersonaDownstreamPacketBatchLease
	admitErr := clockErr
	if admitErr == nil {
		lease, admitErr = adapter.batchOwner.Admit(batchClaim, nowMS)
	}

	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.terminalFence.Load() {
		if lease.Valid() {
			adapter.slot.batchLease = lease
		}
		adapter.transitionToQuarantined()
		adapter.quarantine = errors.Join(adapter.quarantine,
			ErrControllerPersonaInterruptOutQuarantined)
		return adapter.quarantineErrorLocked()
	}
	if adapter.loadState() != controllerPersonaInterruptOutAdmitting ||
		!adapter.exactUSBClaimLocked(claim) {
		if lease.Valid() {
			adapter.slot.batchLease = lease
		}
		adapter.recordViolationLocked(ErrControllerPersonaInterruptOutClaim)
		return adapter.quarantineErrorLocked()
	}
	return adapter.finishAdmissionLocked(batchClaim, lease, admitErr)
}

// CompleteInterruptOutTransaction only publishes one retained fixed-slot work
// ticket. It never invokes the batch owner or participant and performs no I/O.
func (adapter *DormantControllerPersonaInterruptOutAdapter) CompleteInterruptOutTransaction(
	claim usb.InterruptOutTransactionClaim,
	outcome usb.InterruptOutTransactionOutcome,
) error {
	if !adapter.initialized() {
		return ErrControllerPersonaInterruptOutUninitialized
	}
	if adapter.terminalFence.Load() {
		return ErrControllerPersonaInterruptOutQuarantined
	}
	if claim.Token != 0 &&
		adapter.lastCompletionToken.Load() == claim.Token {
		return adapter.containFatalCompletion(
			ErrControllerPersonaInterruptOutClaim)
	}
	state := adapter.loadState()
	if state == controllerPersonaInterruptOutCompleting {
		return adapter.containFatalCompletion(
			ErrControllerPersonaInterruptOutClaim)
	}
	if state == controllerPersonaInterruptOutPending ||
		state == controllerPersonaInterruptOutRunning {
		return adapter.containFatalCompletion(
			ErrControllerPersonaInterruptOutClaim)
	}
	if state != controllerPersonaInterruptOutAdmitted &&
		state != controllerPersonaInterruptOutClaimed &&
		state != controllerPersonaInterruptOutAdmissionRejected {
		return adapter.containFatalCompletion(
			ErrControllerPersonaInterruptOutClaim)
	}
	if !adapter.compareState(state, controllerPersonaInterruptOutCompleting) {
		return adapter.containFatalCompletion(
			ErrControllerPersonaInterruptOutBusy)
	}
	if !claim.Valid() || !claim.Handled() ||
		outcome < usb.InterruptOutTransactionDelivered ||
		outcome > usb.InterruptOutTransactionCancelled {
		return adapter.containFatalCompletion(
			ErrControllerPersonaInterruptOutClaim)
	}
	validTerminal := state == controllerPersonaInterruptOutAdmitted &&
		(outcome == usb.InterruptOutTransactionDelivered ||
			outcome == usb.InterruptOutTransactionDeliveryFailed) ||
		(state == controllerPersonaInterruptOutClaimed ||
			state == controllerPersonaInterruptOutAdmissionRejected) &&
			outcome == usb.InterruptOutTransactionCancelled
	if !validTerminal {
		return adapter.containFatalCompletion(
			ErrControllerPersonaInterruptOutClaim)
	}
	// Completing is exclusive: no worker, Admit, or cleanup can mutate the slot
	// while the numeric claim authenticates its exact private record. A forged
	// caller may terminally seize this boundary, but can never publish work.
	if claim != adapter.slot.usbClaim || claim.Generation != adapter.generation {
		return adapter.containFatalCompletion(
			ErrControllerPersonaInterruptOutClaim)
	}
	adapter.lastCompletionToken.Store(claim.Token)
	// The exact ticket was preallocated before Claim returned. This atomic
	// outcome store followed by Pending publication is the linearization point;
	// PendingWork and Snapshot never contend with response-writer completion.
	if !adapter.publishCompletion(outcome) {
		return adapter.containFatalCompletion(
			ErrControllerPersonaInterruptOutBusy)
	}
	wake := adapter.workReady
	select {
	case wake <- struct{}{}:
	default:
	}
	return nil
}

// WorkReady returns one lifetime-stable latched notification channel. A wake
// is a hint only; PendingWork revalidates the exact retained ticket.
func (adapter *DormantControllerPersonaInterruptOutAdapter) WorkReady() <-chan struct{} {
	if !adapter.initialized() {
		return nil
	}
	return adapter.workReady
}

// PendingWork returns the one exact retained work ticket without consuming it.
// Repeated reads return the same value until RunPending acquires it.
func (adapter *DormantControllerPersonaInterruptOutAdapter) PendingWork() (
	ControllerPersonaInterruptOutWorkTicket,
	bool,
) {
	if !adapter.initialized() {
		return ControllerPersonaInterruptOutWorkTicket{}, false
	}
	if adapter.terminalFence.Load() {
		return ControllerPersonaInterruptOutWorkTicket{}, false
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.loadState() != controllerPersonaInterruptOutPending ||
		!adapter.exactWorkTicketLocked(adapter.slot.workTicket) {
		return ControllerPersonaInterruptOutWorkTicket{}, false
	}
	return adapter.slot.workTicket, true
}

// RunPending is the non-USB-callback worker boundary. executeDeadline is passed
// unchanged to Execute; drainDeadline is passed unchanged to CancelAndDrain
// when an ambiguous attempt requires containment. Both must be future bounds
// and drainDeadline must be later than executeDeadline.
func (adapter *DormantControllerPersonaInterruptOutAdapter) RunPending(
	ticket ControllerPersonaInterruptOutWorkTicket,
	executeDeadline time.Time,
	drainDeadline time.Time,
) (runErr error) {
	if !adapter.initialized() {
		return ErrControllerPersonaInterruptOutUninitialized
	}
	if adapter.terminalFence.Load() {
		return ErrControllerPersonaInterruptOutQuarantined
	}
	now := time.Now()
	if executeDeadline.IsZero() || drainDeadline.IsZero() ||
		!executeDeadline.After(now) || !drainDeadline.After(executeDeadline) {
		adapter.recordWorkerViolation(ErrControllerPersonaInterruptOutDeadline)
		return ErrControllerPersonaInterruptOutDeadline
	}
	if !adapter.tryBeginOperation() {
		adapter.recordWorkerViolation(ErrControllerPersonaInterruptOutBusy)
		return ErrControllerPersonaInterruptOutBusy
	}
	defer adapter.endOperation()

	adapter.mu.Lock()
	if adapter.terminalFence.Load() {
		adapter.recordViolationLocked(ErrControllerPersonaInterruptOutQuarantined)
		adapter.mu.Unlock()
		return ErrControllerPersonaInterruptOutQuarantined
	}
	if adapter.loadState() != controllerPersonaInterruptOutPending ||
		!adapter.exactWorkTicketLocked(ticket) {
		adapter.recordViolationLocked(ErrControllerPersonaInterruptOutWorkTicket)
		adapter.mu.Unlock()
		return ErrControllerPersonaInterruptOutWorkTicket
	}
	slot := adapter.slot
	if !adapter.compareState(controllerPersonaInterruptOutPending,
		controllerPersonaInterruptOutRunning) {
		adapter.recordViolationLocked(ErrControllerPersonaInterruptOutWorkTicket)
		adapter.mu.Unlock()
		return ErrControllerPersonaInterruptOutWorkTicket
	}
	outcome := usb.InterruptOutTransactionOutcome(adapter.completionOutcome.Load())
	adapter.mu.Unlock()
	select {
	case <-adapter.workReady:
	default:
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			runErr = errors.Join(runErr,
				ErrControllerPersonaInterruptOutQuarantined)
			runErr = adapter.finishWork(ticket, true, runErr)
		}
	}()

	retryBlocked := false
	switch slot.usbClaim.Result {
	case usb.InterruptOutTransactionAccepted:
		retryBlocked, runErr = adapter.runAcceptedWork(
			slot, outcome, executeDeadline, drainDeadline)
	case usb.InterruptOutTransactionStall:
		if !adapter.beginResolution() || !adapter.prepareFinish() {
			runErr = ErrControllerPersonaInterruptOutQuarantined
		}
	default:
		runErr = ErrControllerPersonaInterruptOutClaim
	}
	return adapter.finishWork(ticket, retryBlocked, runErr)
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) runAcceptedWork(
	slot controllerPersonaInterruptOutSlot,
	usbOutcome usb.InterruptOutTransactionOutcome,
	executeDeadline time.Time,
	drainDeadline time.Time,
) (bool, error) {
	if !slot.batchClaim.Valid() {
		return false, ErrControllerPersonaInterruptOutClaim
	}
	completedMS := func() (uint64, error) {
		return adapter.protocolMilliseconds(time.Now())
	}
	if usbOutcome != usb.InterruptOutTransactionDelivered {
		nowMS, err := completedMS()
		if err != nil {
			return false, err
		}
		if !adapter.beginResolution() {
			return false, ErrControllerPersonaInterruptOutQuarantined
		}
		err = adapter.batchOwner.Resolve(
			slot.batchClaim, ControllerDownstreamPacketDeferred, nowMS)
		if err != nil {
			return false, err
		}
		if err = adapter.validateResolvedOwner(
			ControllerDownstreamPacketDeferred); err != nil {
			return false, err
		}
		if !adapter.prepareFinish() {
			return false, ErrControllerPersonaInterruptOutQuarantined
		}
		return true, nil
	}
	if !slot.batchLease.Valid() ||
		slot.batchLease.PacketEpoch() != slot.batchClaim.PacketEpoch() {
		return false, ErrControllerPersonaInterruptOutClaim
	}

	executeErr := adapter.batchOwner.Execute(slot.batchLease, executeDeadline)
	outcome := ControllerDownstreamPacketDelivered
	retryBlocked := false
	if executeErr != nil {
		snapshot, ok := adapter.batchOwner.Snapshot()
		if !ok {
			return false, executeErr
		}
		switch {
		case snapshot.AwaitingResolution:
			outcome = ControllerDownstreamPacketDeliveryFailed
			retryBlocked = true
		case snapshot.DrainRequired || snapshot.ExecutionInFlight:
			drainErr := adapter.batchOwner.CancelAndDrain(
				slot.batchLease, drainDeadline)
			if drainErr != nil {
				return false, errors.Join(executeErr, drainErr)
			}
			outcome = ControllerDownstreamPacketExecutionCancelled
			retryBlocked = true
		case snapshot.Admitted &&
			errors.Is(executeErr, ErrControllerDownstreamPacketDeadline):
			outcome = ControllerDownstreamPacketDeferred
			retryBlocked = true
		default:
			return false, executeErr
		}
	}
	nowMS, clockErr := completedMS()
	if clockErr != nil {
		return false, errors.Join(executeErr, clockErr)
	}
	if !adapter.beginResolution() {
		return false, errors.Join(executeErr,
			ErrControllerPersonaInterruptOutQuarantined)
	}
	resolveErr := adapter.batchOwner.Resolve(slot.batchClaim, outcome, nowMS)
	if resolveErr == nil {
		resolveErr = adapter.validateResolvedOwner(outcome)
		if resolveErr == nil && !adapter.prepareFinish() {
			resolveErr = ErrControllerPersonaInterruptOutQuarantined
		}
	}
	return retryBlocked && resolveErr == nil, errors.Join(executeErr, resolveErr)
}

// beginResolution atomically orders canonical owner resolution against a
// competing Complete. If the worker wins Running -> Resolving first, its exact
// terminal fact was authorized before the violation. If the violation wins,
// the owner remains in its exact admitted/awaiting-resolution state.
func (adapter *DormantControllerPersonaInterruptOutAdapter) beginResolution() bool {
	if adapter.terminalFence.Load() ||
		!adapter.compareState(controllerPersonaInterruptOutRunning,
			controllerPersonaInterruptOutResolving) {
		return false
	}
	return true
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) prepareFinish() bool {
	for {
		switch state := adapter.loadState(); state {
		case controllerPersonaInterruptOutResolving:
			if adapter.compareState(state,
				controllerPersonaInterruptOutFinishing) {
				return true
			}
		case controllerPersonaInterruptOutResolvingTerminal:
			adapter.compareState(state,
				controllerPersonaInterruptOutQuarantined)
			return false
		default:
			adapter.transitionToQuarantined()
			return false
		}
	}
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) validateResolvedOwner(
	outcome ControllerDownstreamPacketOutcome,
) error {
	snapshot, ok := adapter.batchOwner.Snapshot()
	if !ok || !validControllerPersonaInterruptOutResolvedOwner(
		outcome, snapshot) {
		return ErrControllerPersonaInterruptOutClaim
	}
	return nil
}

func validControllerPersonaInterruptOutResolvedOwner(
	outcome ControllerDownstreamPacketOutcome,
	snapshot ControllerPersonaDownstreamPacketBatchSnapshot,
) bool {
	if snapshot.Claimed || snapshot.Admitted || snapshot.ExecutionInFlight ||
		snapshot.AwaitingResolution || snapshot.DrainRequired ||
		snapshot.Quarantined {
		return false
	}
	if outcome == ControllerDownstreamPacketDelivered {
		return snapshot.Idle && !snapshot.RetryPending
	}
	if outcome < ControllerDownstreamPacketDeferred ||
		outcome > ControllerDownstreamPacketExecutionCancelled {
		return false
	}
	return snapshot.RetryPending && !snapshot.Idle
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) finishWork(
	ticket ControllerPersonaInterruptOutWorkTicket,
	retryBlocked bool,
	runErr error,
) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.loadState() != controllerPersonaInterruptOutFinishing ||
		!adapter.exactWorkTicketLocked(ticket) {
		runErr = errors.Join(runErr,
			ErrControllerPersonaInterruptOutWorkTicket)
	}
	if retryBlocked {
		adapter.retryBlock = true
		runErr = errors.Join(runErr,
			ErrControllerPersonaInterruptOutRetryUnsupported)
	}
	if runErr != nil || adapter.quarantine != nil ||
		adapter.terminalFence.Load() {
		adapter.transitionToQuarantined()
		adapter.quarantine = errors.Join(adapter.quarantine, runErr)
		return errors.Join(ErrControllerPersonaInterruptOutQuarantined,
			adapter.quarantine)
	}
	if !adapter.compareState(controllerPersonaInterruptOutFinishing,
		controllerPersonaInterruptOutIdle) {
		adapter.transitionToQuarantined()
		adapter.quarantine = errors.Join(adapter.quarantine,
			ErrControllerPersonaInterruptOutWorkTicket)
		return adapter.quarantineErrorLocked()
	}
	if adapter.terminalFence.Load() ||
		adapter.loadState() != controllerPersonaInterruptOutIdle {
		adapter.transitionToQuarantined()
		adapter.quarantine = errors.Join(adapter.quarantine,
			ErrControllerPersonaInterruptOutClaim)
		return adapter.quarantineErrorLocked()
	}
	// Keep the retired immutable slot as exact diagnostic evidence until the
	// next Claim publishes its successor. State, token monotonicity, and the
	// retained completion token make its copied credentials unusable.
	return nil
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) Snapshot() (
	ControllerPersonaInterruptOutSnapshot,
	bool,
) {
	if !adapter.initialized() {
		return ControllerPersonaInterruptOutSnapshot{}, false
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	state := adapter.loadState()
	return ControllerPersonaInterruptOutSnapshot{
		Generation: adapter.generation, USBToken: adapter.slot.usbClaim.Token,
		WorkToken:  adapter.slot.workTicket.token,
		BatchEpoch: adapter.slot.batchClaim.PacketEpoch(),
		Idle:       state == controllerPersonaInterruptOutIdle,
		Claimed: state == controllerPersonaInterruptOutClaimed ||
			state == controllerPersonaInterruptOutAdmitting ||
			state == controllerPersonaInterruptOutAdmissionRejected,
		Admitted: state == controllerPersonaInterruptOutAdmitted,
		Pending:  state == controllerPersonaInterruptOutPending,
		Running: state == controllerPersonaInterruptOutRunning ||
			state == controllerPersonaInterruptOutResolving ||
			state == controllerPersonaInterruptOutResolvingTerminal ||
			state == controllerPersonaInterruptOutFinishing,
		Quarantined: state == controllerPersonaInterruptOutQuarantined ||
			adapter.terminalFence.Load() || adapter.quarantine != nil,
		RetryBlocked: adapter.retryBlock,
	}, true
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) initialized() bool {
	return adapter != nil && adapter.self == adapter && adapter.operation != nil &&
		adapter.workReady != nil && adapter.generation != 0 &&
		adapter.batchOwner != nil && !adapter.origin.IsZero()
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) tryBeginOperation() bool {
	select {
	case <-adapter.operation:
		return true
	default:
		return false
	}
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) endOperation() {
	adapter.operation <- struct{}{}
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) loadState() controllerPersonaInterruptOutState {
	return controllerPersonaInterruptOutState(adapter.state.Load())
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) storeState(
	state controllerPersonaInterruptOutState,
) {
	adapter.state.Store(uint32(state))
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) compareState(
	old controllerPersonaInterruptOutState,
	new controllerPersonaInterruptOutState,
) bool {
	return adapter.state.CompareAndSwap(uint32(old), uint32(new))
}

func classifyControllerPersonaInterruptOutAdmission(
	leaseValid bool,
	err error,
) controllerPersonaInterruptOutAdmissionShape {
	switch {
	case err == nil && leaseValid:
		return controllerPersonaInterruptOutAdmissionShapeAccepted
	case err == nil:
		return controllerPersonaInterruptOutAdmissionShapeInvalid
	case leaseValid:
		return controllerPersonaInterruptOutAdmissionShapeContradictory
	default:
		return controllerPersonaInterruptOutAdmissionShapeRejected
	}
}

func classifyControllerPersonaInterruptOutClaim(
	claimValid bool,
	err error,
) controllerPersonaInterruptOutClaimShape {
	switch {
	case err == nil && claimValid:
		return controllerPersonaInterruptOutClaimShapeAccepted
	case err == nil:
		return controllerPersonaInterruptOutClaimShapeInvalid
	case claimValid:
		return controllerPersonaInterruptOutClaimShapeContradictory
	default:
		return controllerPersonaInterruptOutClaimShapeRejected
	}
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) exactBatchClaimForPacket(
	claim ControllerPersonaDownstreamPacketBatchClaim,
	packet ControllerDownstreamPacket,
) bool {
	if !claim.Valid() || claim.owner != adapter.batchOwner ||
		claim.MessageCount() != packet.Len() {
		return false
	}
	owner := adapter.batchOwner
	owner.mu.Lock()
	exact := owner.state == controllerPersonaHostPacketOwnerClaimed &&
		claim == owner.claimLocked() &&
		owner.execution.packetEpoch == claim.PacketEpoch() &&
		owner.execution.Len() == packet.Len()
	executor := owner.executor
	executor.mu.Lock()
	exact = exact && executor.state == controllerDownstreamPacketClaimed &&
		owner.packetClaim == executor.claimLocked() &&
		executor.execution == owner.execution
	executor.mu.Unlock()
	owner.mu.Unlock()
	return exact
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) finishAdmissionLocked(
	batchClaim ControllerPersonaDownstreamPacketBatchClaim,
	lease ControllerPersonaDownstreamPacketBatchLease,
	admitErr error,
) error {
	shape := classifyControllerPersonaInterruptOutAdmission(lease.Valid(), admitErr)
	if lease.Valid() {
		// Retain before classifying. In particular, error+valid is not a clean
		// rejection: an admitted effect authority exists and must never be lost.
		adapter.slot.batchLease = lease
	}
	switch shape {
	case controllerPersonaInterruptOutAdmissionShapeRejected:
		if !adapter.compareState(controllerPersonaInterruptOutAdmitting,
			controllerPersonaInterruptOutAdmissionRejected) {
			adapter.transitionToQuarantined()
			adapter.quarantine = errors.Join(adapter.quarantine, admitErr,
				ErrControllerPersonaInterruptOutClaim)
			return adapter.quarantineErrorLocked()
		}
		return admitErr
	case controllerPersonaInterruptOutAdmissionShapeContradictory:
		adapter.transitionToQuarantined()
		adapter.quarantine = errors.Join(adapter.quarantine, admitErr,
			ErrControllerPersonaInterruptOutClaim)
		return adapter.quarantineErrorLocked()
	case controllerPersonaInterruptOutAdmissionShapeInvalid:
		adapter.transitionToQuarantined()
		adapter.quarantine = errors.Join(adapter.quarantine,
			ErrControllerPersonaInterruptOutClaim)
		return adapter.quarantineErrorLocked()
	case controllerPersonaInterruptOutAdmissionShapeAccepted:
		if !adapter.exactBatchLeaseForClaim(lease, batchClaim) {
			adapter.transitionToQuarantined()
			adapter.quarantine = errors.Join(adapter.quarantine,
				ErrControllerPersonaInterruptOutClaim)
			return adapter.quarantineErrorLocked()
		}
		if !adapter.compareState(controllerPersonaInterruptOutAdmitting,
			controllerPersonaInterruptOutAdmitted) {
			adapter.transitionToQuarantined()
			adapter.quarantine = errors.Join(adapter.quarantine,
				ErrControllerPersonaInterruptOutClaim)
			return adapter.quarantineErrorLocked()
		}
		return nil
	default:
		adapter.transitionToQuarantined()
		adapter.quarantine = errors.Join(adapter.quarantine,
			ErrControllerPersonaInterruptOutClaim)
		return adapter.quarantineErrorLocked()
	}
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) publishCompletion(
	outcome usb.InterruptOutTransactionOutcome,
) bool {
	adapter.completionOutcome.Store(uint32(outcome))
	return adapter.compareState(controllerPersonaInterruptOutCompleting,
		controllerPersonaInterruptOutPending)
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) transitionToQuarantined() {
	for {
		state := adapter.loadState()
		if state == controllerPersonaInterruptOutQuarantined {
			return
		}
		if adapter.compareState(state, controllerPersonaInterruptOutQuarantined) {
			return
		}
	}
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) exactBatchLeaseForClaim(
	lease ControllerPersonaDownstreamPacketBatchLease,
	claim ControllerPersonaDownstreamPacketBatchClaim,
) bool {
	if !lease.Valid() || !claim.Valid() ||
		lease.owner != adapter.batchOwner || claim.owner != adapter.batchOwner ||
		lease.token != claim.token ||
		lease.PacketEpoch() != claim.PacketEpoch() ||
		lease.Len() != claim.MessageCount() {
		return false
	}
	owner := adapter.batchOwner
	owner.mu.Lock()
	exact := owner.state == controllerPersonaHostPacketOwnerAdmitted &&
		claim == owner.claimLocked() && lease.token == owner.claimToken &&
		lease.execution == owner.packetLease &&
		lease.execution.execution == owner.execution
	executor := owner.executor
	executor.mu.Lock()
	exact = exact && executor.state == controllerDownstreamPacketAdmitted &&
		lease.execution.owner == executor &&
		lease.execution.token == executor.claimToken &&
		lease.execution.execution == executor.execution
	executor.mu.Unlock()
	owner.mu.Unlock()
	return exact
}

// containFatalCompletion is the single terminal-state protocol for every
// response-path contradiction. Running -> Resolving is the sole resolution
// authority: if the worker wins it, Resolving is irrevocable until exact owner
// resolution returns. Otherwise containment CASes the observed state to
// Quarantined. The permanent fence is then latched in either case, so no later
// work or attempt can cross the terminal boundary.
func (adapter *DormantControllerPersonaInterruptOutAdapter) containFatalCompletion(
	err error,
) error {
	adapter.latchTerminalState()
	switch err {
	case ErrControllerPersonaInterruptOutClaim:
		return errControllerPersonaInterruptOutTerminalClaim
	case ErrControllerPersonaInterruptOutBusy:
		return errControllerPersonaInterruptOutTerminalBusy
	default:
		return ErrControllerPersonaInterruptOutQuarantined
	}
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) latchTerminalState() {
	for {
		state := adapter.loadState()
		if state == controllerPersonaInterruptOutResolvingTerminal ||
			state == controllerPersonaInterruptOutQuarantined {
			adapter.terminalFence.Store(true)
			return
		}
		if state == controllerPersonaInterruptOutResolving {
			if adapter.compareState(state,
				controllerPersonaInterruptOutResolvingTerminal) {
				adapter.terminalFence.Store(true)
				return
			}
			continue
		}
		if adapter.compareState(state,
			controllerPersonaInterruptOutQuarantined) {
			adapter.terminalFence.Store(true)
			return
		}
	}
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) exactUSBClaimLocked(
	claim usb.InterruptOutTransactionClaim,
) bool {
	return claim.Valid() && claim.Handled() && claim == adapter.slot.usbClaim &&
		claim.Generation == adapter.generation
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) exactWorkTicketLocked(
	ticket ControllerPersonaInterruptOutWorkTicket,
) bool {
	return ticket.Valid() && ticket.owner == adapter &&
		ticket == adapter.slot.workTicket && ticket.generation == adapter.generation &&
		ticket.token == adapter.slot.usbClaim.Token &&
		ticket.batchEpoch == adapter.slot.batchClaim.PacketEpoch()
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) protocolMilliseconds(
	now time.Time,
) (uint64, error) {
	if now.IsZero() || now.Before(adapter.origin) {
		return 0, ErrControllerPersonaInterruptOutDeadline
	}
	delta := uint64(now.Sub(adapter.origin) / time.Millisecond)
	if delta > ^uint64(0)-adapter.originMS {
		return 0, ErrControllerPersonaInterruptOutTokenExhausted
	}
	return adapter.originMS + delta, nil
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) quarantineClaimFailure(
	claim ControllerPersonaDownstreamPacketBatchClaim,
	err error,
) {
	adapter.mu.Lock()
	if claim.Valid() {
		adapter.slot.batchClaim = claim
	}
	adapter.latchTerminalState()
	adapter.quarantine = errors.Join(adapter.quarantine, err)
	adapter.mu.Unlock()
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) recordWorkerViolation(
	err error,
) {
	adapter.mu.Lock()
	adapter.recordViolationLocked(err)
	adapter.mu.Unlock()
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) recordViolationLocked(
	err error,
) {
	adapter.quarantine = errors.Join(adapter.quarantine, err)
	adapter.latchTerminalState()
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) stateErrorLocked() error {
	if adapter.loadState() == controllerPersonaInterruptOutQuarantined ||
		adapter.quarantine != nil || adapter.terminalFence.Load() {
		return adapter.quarantineErrorLocked()
	}
	if adapter.retryBlock {
		return ErrControllerPersonaInterruptOutRetryUnsupported
	}
	return ErrControllerPersonaInterruptOutBusy
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) quarantineError() error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.quarantineErrorLocked()
}

func (adapter *DormantControllerPersonaInterruptOutAdapter) quarantineErrorLocked() error {
	return errors.Join(ErrControllerPersonaInterruptOutQuarantined,
		adapter.quarantine)
}

var _ usb.TransactionalInterruptOutDevice = (*DormantControllerPersonaInterruptOutAdapter)(nil)

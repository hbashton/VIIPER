package usb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
)

var (
	errRetainedSubmissionUninitialized = errors.New(
		"retained USB submission scheduler is uninitialized")
	errRetainedSubmissionInvalidLimits = errors.New(
		"invalid retained USB submission limits")
	errRetainedSubmissionInvalidRequest = errors.New(
		"invalid retained USB submission request")
	errRetainedSubmissionQueueFull = errors.New(
		"retained USB submission queue is full")
	errRetainedSubmissionVisibleCompletionPending = errors.New(
		"retained USB submission response is visible before slot release")
	errRetainedSubmissionLifecycleCompletionPending = errors.New(
		"retained USB lifecycle response is visible before route publication")
	errRetainedSubmissionInactiveRoute = errors.New(
		"retained USB interrupt route is inactive")
	errRetainedSubmissionDuplicateSequence = errors.New(
		"duplicate retained USB submission sequence")
	errRetainedSubmissionCounterExhausted = errors.New(
		"retained USB submission counter exhausted")
	errRetainedSubmissionInvalidTicket = errors.New(
		"invalid retained USB submission ticket")
	errRetainedSubmissionInvalidPreparation = errors.New(
		"invalid retained USB response preparation")
	errRetainedSubmissionReadiness = errors.New(
		"invalid retained USB readiness source")
	errRetainedSubmissionClosed = errors.New(
		"retained USB submission scheduler is closed")
	errRetainedSubmissionNotActivated = errors.New(
		"retained USB import scheduler is not activated")
)

type retainedSubmissionState uint8

const (
	retainedSubmissionFree retainedSubmissionState = iota
	retainedSubmissionRawQueued
	retainedSubmissionTicketed
	retainedSubmissionPending
	retainedSubmissionAdmitted
	retainedSubmissionTerminal
	retainedSubmissionLifecycleRetired
)

type retainedSubmissionEnvelope struct {
	lane              retainedusb.Lane
	direction         retainedusb.Direction
	sequence          uint32
	transferLength    uint32
	setup             [8]byte
	route             retainedusb.Route
	bindingGeneration uint64
	data              []byte
	controlLifecycle  controlLifecycleSetup
}

type retainedSubmissionSlot struct {
	state retainedSubmissionState

	lane              retainedusb.Lane
	direction         retainedusb.Direction
	sessionGeneration uint64
	bindingGeneration uint64
	ingressOrdinal    uint64
	sequence          uint32
	transferLength    uint32
	setup             [8]byte
	route             retainedusb.Route
	controlLifecycle  controlLifecycleSetup

	payload       []byte
	payloadLength int

	ticket retainedusb.Ticket

	pendingReadinessEpoch uint64
	retryAt               time.Time
}

type retainedSubmissionLaneQueue struct {
	slots []retainedSubmissionSlot
	order []int
	free  []int

	orderHead  int
	orderCount int
	freeCount  int

	payloadSlab []byte
	released    chan struct{}
}

type retainedSubmissionRef struct {
	laneIndex      int
	slotIndex      int
	ingressOrdinal uint64
}

type retainedSubmissionRetirement struct {
	ref    retainedSubmissionRef
	ticket retainedusb.Ticket
}

type retainedSubmissionScheduler struct {
	ctx    context.Context
	cancel context.CancelFunc

	owner         retainedusb.Owner
	ownerIdentity uint64
	limits        retainedusb.Limits
	session       uint64
	responses     *responseWriter

	mu          sync.Mutex
	lanes       [3]retainedSubmissionLaneQueue
	nextOrdinal uint64
	closed      bool
	failure     error
	retirement  retainedusb.ImportRetirementRequest
	// framingSequences reserves a USB/IP response sequence at header arrival,
	// before a potentially blocking OUT body/ISO tail is consumed. The command
	// reader transfers the reservation atomically into a queue slot or releases
	// it immediately before writing a synchronous rejection response.
	framingSequences map[uint32]struct{}

	// serviceAdmission is a single-token serializer shared by ordinary service
	// and the dormant reversible-reset drain. Unlike sync.Mutex, reset can
	// acquire it against an absolute deadline and quarantine rather than return
	// an unproven drain.
	serviceAdmission chan struct{}

	wake      chan struct{}
	done      chan struct{}
	started   bool
	closeOnce sync.Once

	importReservation *retainedImportReservation
	importAdmission   *retainedImportAdmission
	importActivated   bool
	resetFence        retainedusb.ImportResetLease
	resetDrained      bool
	resetActive       atomic.Bool

	controlLifecycleConfigured bool
	bindingGeneration          uint64
	publishControlLifecycle    func(controlLifecycleSetup)
	resolveControlLifecycle    func(
		[8]byte, retainedusb.Direction, uint32) controlLifecycleSetup
	lifecycleCompletionPending bool
	lifecycleReleased          chan struct{}
	activeConfiguration        uint8
	activeAlternateSetting     uint8

	readiness             <-chan struct{}
	lastReadinessEpoch    uint64
	responseScratch       []byte
	maximumResponseLength int
	// Descriptor-owned service cursors are independent of semantic readiness.
	// EP0 remains unscheduled; each interrupt direction has its own cadence.
	endpointCadenceConfigured bool
	endpointIntervals         [3]time.Duration
	endpointNextService       [3]time.Time
}

// newRetainedSubmissionScheduler constructs the dormant Stage-B.1 arbiter.
// It intentionally has no non-test call site. start controls only whether its
// private arbitration goroutine is launched; deterministic tests use false.
func newRetainedSubmissionScheduler(
	parent context.Context,
	sessionGeneration uint64,
	owner retainedusb.Owner,
	responses *responseWriter,
	start bool,
) (*retainedSubmissionScheduler, error) {
	return buildRetainedSubmissionScheduler(
		parent, sessionGeneration, owner, responses, start)
}

func buildRetainedSubmissionScheduler(
	parent context.Context,
	sessionGeneration uint64,
	owner retainedusb.Owner,
	responses *responseWriter,
	start bool,
) (*retainedSubmissionScheduler, error) {
	return buildRetainedSubmissionSchedulerWithSnapshot(
		parent, sessionGeneration, owner, responses, start, nil)
}

type retainedSchedulerConstructionSnapshot struct {
	ownerIdentity  uint64
	limits         retainedusb.Limits
	readinessEpoch uint64
	readiness      <-chan struct{}
}

func buildRetainedSubmissionSchedulerWithSnapshot(
	parent context.Context,
	sessionGeneration uint64,
	owner retainedusb.Owner,
	responses *responseWriter,
	start bool,
	snapshot *retainedSchedulerConstructionSnapshot,
) (*retainedSubmissionScheduler, error) {
	if parent == nil || sessionGeneration == 0 || owner == nil || responses == nil {
		return nil, errRetainedSubmissionUninitialized
	}
	if parent.Err() != nil {
		return nil, errRetainedSubmissionClosed
	}
	var ownerIdentity uint64
	var limits retainedusb.Limits
	var readinessEpoch uint64
	var readiness <-chan struct{}
	if snapshot != nil {
		ownerIdentity = snapshot.ownerIdentity
		limits = snapshot.limits
		readinessEpoch = snapshot.readinessEpoch
		readiness = snapshot.readiness
		if ownerIdentity == 0 || !retainedReadinessValid(
			readinessEpoch, readiness, nil, 0) {
			return nil, errRetainedSubmissionUninitialized
		}
	} else {
		var err error
		ownerIdentity, err = invokeRetainedIdentity(owner)
		if err != nil || ownerIdentity == 0 {
			if err == nil {
				err = errRetainedSubmissionUninitialized
			}
			return nil, fmt.Errorf("retained USB owner identity: %w", err)
		}
		limits, err = invokeRetainedLimits(owner)
		if err != nil {
			return nil, fmt.Errorf("retained USB owner limits: %w", err)
		}
		readinessEpoch, readiness, err = sampleRetainedReadiness(
			owner, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("retained USB owner readiness: %w", err)
		}
	}
	if !limits.Valid() || limits.BufferedRequestBytes > maximumTransferSize ||
		limits.MaximumControlOut > maximumTransferSize ||
		limits.MaximumControlResponse > maximumTransferSize ||
		limits.MaximumInterruptIn > maximumTransferSize ||
		limits.MaximumInterruptOut > maximumTransferSize {
		return nil, errRetainedSubmissionInvalidLimits
	}
	ctx, cancel := context.WithCancel(parent)
	scheduler := &retainedSubmissionScheduler{
		ctx:                ctx,
		cancel:             cancel,
		owner:              owner,
		ownerIdentity:      ownerIdentity,
		limits:             limits,
		session:            sessionGeneration,
		responses:          responses,
		wake:               make(chan struct{}, 1),
		done:               make(chan struct{}),
		serviceAdmission:   make(chan struct{}, 1),
		lifecycleReleased:  make(chan struct{}, 1),
		framingSequences:   make(map[uint32]struct{}),
		readiness:          readiness,
		lastReadinessEpoch: readinessEpoch,
		maximumResponseLength: int(max(limits.MaximumControlResponse,
			limits.MaximumInterruptIn)),
	}
	scheduler.serviceAdmission <- struct{}{}
	scheduler.responseScratch = make(
		[]byte, retSubmitHeaderSize+scheduler.maximumResponseLength)

	for laneIndex := range scheduler.lanes {
		depth := int(limits.QueueDepth[laneIndex])
		lane := &scheduler.lanes[laneIndex]
		lane.slots = make([]retainedSubmissionSlot, depth)
		lane.order = make([]int, depth)
		lane.free = make([]int, depth)
		lane.released = make(chan struct{}, 1)
		lane.freeCount = depth
		for index := range lane.free {
			lane.free[index] = depth - 1 - index
		}
	}

	controlIndex, _ := retainedusb.LaneControl.Index()
	outIndex, _ := retainedusb.LaneInterruptOut.Index()
	scheduler.initializePayloadSlab(
		controlIndex, int(limits.MaximumControlOut))
	scheduler.initializePayloadSlab(
		outIndex, int(limits.MaximumInterruptOut))

	if start {
		scheduler.started = true
		go scheduler.run()
	}
	return scheduler, nil
}

// newParkedRetainedImportScheduler is the only scheduler constructor accepted
// by the Stage-B.2 commit transaction. Its reservation-owned ingress gate is
// closed before return, so no URB can queue before exact Bind + activation +
// Claimed publication.
func newParkedRetainedImportScheduler(
	parent context.Context,
	reservation *retainedImportReservation,
	owner retainedImportCombinedOwner,
	responses *responseWriter,
	snapshots ...*retainedSchedulerConstructionSnapshot,
) (*retainedSubmissionScheduler, error) {
	if reservation == nil || reservation.authority == nil ||
		!reservation.authority.authenticatesReservation(reservation) ||
		interfaceIsNil(owner) || len(snapshots) > 1 {
		return nil, errRetainedSubmissionUninitialized
	}
	var snapshot *retainedSchedulerConstructionSnapshot
	if len(snapshots) == 1 {
		snapshot = snapshots[0]
	}
	if err := reservation.authority.beginParkedSchedulerBuild(
		reservation); err != nil {
		return nil, err
	}
	buildSucceeded := false
	defer func() {
		if !buildSucceeded {
			_ = reservation.authority.failParkedSchedulerBuild(reservation)
		}
	}()
	reference, valid := exactRetainedImportOwnerReference(owner)
	if !valid || reference != reservation.session.ownerReference {
		return nil, errRetainedImportLeaseRejected
	}
	if snapshot != nil && snapshot.ownerIdentity != reservation.lease.OwnerID {
		return nil, errRetainedImportLeaseRejected
	}
	scheduler, err := buildRetainedSubmissionSchedulerWithSnapshot(
		parent, reservation.lease.SessionGeneration, owner, responses, false,
		snapshot)
	if err != nil {
		return nil, err
	}
	scheduler.importReservation = reservation
	scheduler.importAdmission = &reservation.admission
	if err := reservation.authority.finishParkedSchedulerBuild(
		reservation, scheduler); err != nil {
		_ = scheduler.close()
		return nil, err
	}
	buildSucceeded = true
	return scheduler, nil
}

// configureControlLifecycle binds the production retained scheduler to the
// server-owned endpoint generation and route-publication boundary. It is valid
// only while the import scheduler is still parked; generic scheduler tests and
// non-production composition remain callback-free.
func (scheduler *retainedSubmissionScheduler) configureControlLifecycle(
	initialBindingGeneration uint64,
	publish func(controlLifecycleSetup),
	resolvers ...func(
		[8]byte, retainedusb.Direction, uint32) controlLifecycleSetup,
) error {
	if scheduler == nil || initialBindingGeneration == 0 || publish == nil ||
		len(resolvers) > 1 {
		return errRetainedSubmissionInvalidRequest
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.started || scheduler.closed || scheduler.importActivated ||
		scheduler.ctx.Err() != nil || scheduler.controlLifecycleConfigured {
		return errRetainedSubmissionInvalidRequest
	}
	scheduler.controlLifecycleConfigured = true
	scheduler.bindingGeneration = initialBindingGeneration
	scheduler.publishControlLifecycle = publish
	if len(resolvers) == 1 {
		scheduler.resolveControlLifecycle = resolvers[0]
	}
	return nil
}

func (scheduler *retainedSubmissionScheduler) initializePayloadSlab(
	laneIndex, bytesPerSlot int,
) {
	lane := &scheduler.lanes[laneIndex]
	if bytesPerSlot <= 0 || len(lane.slots) == 0 {
		return
	}
	lane.payloadSlab = make([]byte, len(lane.slots)*bytesPerSlot)
	for slotIndex := range lane.slots {
		start := slotIndex * bytesPerSlot
		lane.slots[slotIndex].payload = lane.payloadSlab[start : start : start+bytesPerSlot]
	}
}

// importBinding exposes only the immutable facts needed by the dormant
// Stage-B.2 commit transaction. It does not start the scheduler or expose its
// hot Owner/Ticket path. Parked means no arbitration goroutine has ever been
// launched and close has not begun.
func (scheduler *retainedSubmissionScheduler) importBinding() retainedImportSchedulerBinding {
	if scheduler == nil {
		return retainedImportSchedulerBinding{}
	}
	ownerReference, _ := exactRetainedImportOwnerReference(scheduler.owner)
	scheduler.mu.Lock()
	binding := retainedImportSchedulerBinding{
		ownerReference:     ownerReference,
		ownerIdentity:      scheduler.ownerIdentity,
		sessionGeneration:  scheduler.session,
		reservation:        scheduler.importReservation,
		activationRequired: scheduler.importAdmission != nil,
		parked: !scheduler.started && !scheduler.closed &&
			!scheduler.importActivated && scheduler.ctx.Err() == nil &&
			scheduler.importAdmission != nil &&
			scheduler.importAdmission.activation.Load() ==
				retainedImportActivationParked,
	}
	scheduler.mu.Unlock()
	return binding
}

func (scheduler *retainedSubmissionScheduler) activateImport(
	reservation *retainedImportReservation,
) error {
	if scheduler == nil || reservation == nil {
		return errRetainedImportLeaseRejected
	}
	scheduler.mu.Lock()
	if scheduler.importReservation != reservation ||
		scheduler.importAdmission != &reservation.admission ||
		scheduler.importAdmission.open.Load() || scheduler.importActivated ||
		scheduler.started || scheduler.closed || scheduler.ctx.Err() != nil ||
		!scheduler.importAdmission.activation.CompareAndSwap(
			retainedImportActivationParked,
			retainedImportActivationStarted) {
		scheduler.mu.Unlock()
		return errRetainedImportLeaseRejected
	}
	scheduler.importActivated = true
	scheduler.started = true
	scheduler.mu.Unlock()
	go scheduler.run()
	return nil
}

// openAdmission is the final claim-publication half of activation. It is
// deliberately synchronous and callback-free. The scheduler lifecycle mutex
// linearizes worker failure/close against gate-open; a failure which has
// already been recorded cannot produce a successfully Claimed import.
func (scheduler *retainedSubmissionScheduler) openAdmission(
	reservation *retainedImportReservation,
) error {
	if scheduler == nil || reservation == nil {
		return errRetainedImportLeaseRejected
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.importReservation != reservation ||
		scheduler.importAdmission != &reservation.admission ||
		!scheduler.importActivated || !scheduler.started || scheduler.closed ||
		scheduler.ctx.Err() != nil || scheduler.failure != nil ||
		scheduler.importAdmission.open.Load() ||
		scheduler.importAdmission.activation.Load() !=
			retainedImportActivationStarted {
		if scheduler.failure != nil {
			return errors.Join(errRetainedImportLeaseRejected,
				scheduler.failure)
		}
		return errRetainedImportLeaseRejected
	}
	scheduler.importAdmission.open.Store(true)
	return nil
}

// closeAdmission is the exact linearization fence shared with enqueue. Once
// it returns, any later enqueue must take scheduler.mu and observe the closed
// gate before it can reserve a slot.
func (scheduler *retainedSubmissionScheduler) closeAdmission(
	reservation *retainedImportReservation,
) error {
	if scheduler == nil || reservation == nil {
		return errRetainedImportLeaseRejected
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.importReservation != reservation ||
		scheduler.importAdmission != &reservation.admission {
		return errRetainedImportLeaseRejected
	}
	scheduler.importAdmission.open.Store(false)
	scheduler.importAdmission.activation.Store(
		retainedImportActivationRevoked)
	return nil
}

// fenceReset latches only the separately typed reversible reset gate. It never
// waits for scheduler.mu: Prepare intentionally holds that mutex, and reset
// ingress must still close immediately behind an active serializer. An enqueue
// already past its atomic gate is predecessor work drained by resetAndDrain.
func (scheduler *retainedSubmissionScheduler) fenceReset(
	reservation *retainedImportReservation,
	reset retainedusb.ImportResetLease,
) error {
	if scheduler == nil || reservation == nil || !reset.Valid() ||
		reset.ImportLease != reservation.lease ||
		scheduler.importReservation != reservation ||
		scheduler.importAdmission != &reservation.admission ||
		scheduler.importAdmission.activation.Load() !=
			retainedImportActivationStarted {
		return errRetainedImportLeaseRejected
	}
	if !scheduler.resetActive.CompareAndSwap(false, true) {
		return errRetainedImportLeaseRejected
	}
	scheduler.resetFence = reset
	scheduler.resetDrained = false
	scheduler.importAdmission.open.Store(false)
	return nil
}

// resetAndDrain joins the exact active response-serializer attempt and retires
// every remaining raw or ticketed submission for the already-fenced reset. It
// deliberately does not cancel the scheduler or revoke import activation.
func (scheduler *retainedSubmissionScheduler) resetAndDrain(
	reservation *retainedImportReservation,
	reset retainedusb.ImportResetLease,
	deadline time.Time,
) error {
	if scheduler == nil || reservation == nil || !reset.Valid() ||
		reset.ImportLease != reservation.lease ||
		scheduler.importReservation != reservation ||
		scheduler.importAdmission != &reservation.admission ||
		!scheduler.resetActive.Load() || scheduler.resetFence != reset ||
		scheduler.importAdmission.open.Load() ||
		scheduler.importAdmission.activation.Load() != retainedImportActivationStarted {
		return errRetainedImportLeaseRejected
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errRetainedImportCloseTimedOut
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-scheduler.ctx.Done():
		return errRetainedSubmissionClosed
	case <-timer.C:
		return errRetainedImportCloseTimedOut
	case <-scheduler.serviceAdmission:
	}
	defer func() { scheduler.serviceAdmission <- struct{}{} }()

	// select may choose admission when its timer is also ready. The callback
	// can also acquire scheduler.mu only after the outer reset has timed out.
	// Neither late acquisition may start a successful reversible drain.
	if !deadline.After(time.Now()) {
		return errRetainedImportCloseTimedOut
	}
	scheduler.mu.Lock()
	if !deadline.After(time.Now()) {
		scheduler.mu.Unlock()
		return errRetainedImportCloseTimedOut
	}
	if !scheduler.importActivated || !scheduler.started || scheduler.closed ||
		scheduler.ctx.Err() != nil || scheduler.failure != nil ||
		!scheduler.resetActive.Load() || scheduler.resetFence != reset || scheduler.resetDrained ||
		scheduler.importAdmission.open.Load() ||
		scheduler.importAdmission.activation.Load() != retainedImportActivationStarted {
		scheduler.mu.Unlock()
		return errRetainedImportLeaseRejected
	}
	scheduler.mu.Unlock()

	if err := scheduler.retireSession(
		scheduler.session, retainedusb.RetireDeviceReset, time.Now()); err != nil {
		return err
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	// Retire is an owner callback and may return after timeout/quarantine.
	// Retirement may finish, but that does not confer fresh reopen authority.
	if !deadline.After(time.Now()) {
		return errRetainedImportCloseTimedOut
	}
	if scheduler.closed || scheduler.ctx.Err() != nil ||
		scheduler.failure != nil || scheduler.resetFence != reset ||
		!scheduler.resetActive.Load() || scheduler.importAdmission.open.Load() ||
		scheduler.importAdmission.activation.Load() != retainedImportActivationStarted ||
		!scheduler.emptyLocked() {
		return errRetainedImportLeaseRejected
	}
	scheduler.resetDrained = true
	return nil
}

// reopenAfterReset is callback-free and nonblocking. It is valid only for the
// exact reset capability whose scheduler drain completed and only while every
// lane remains empty. The outer session calls it after owner Reset Safe.
func (scheduler *retainedSubmissionScheduler) reopenAfterReset(
	reservation *retainedImportReservation,
	reset retainedusb.ImportResetLease,
) error {
	if scheduler == nil || reservation == nil || !reset.Valid() {
		return errRetainedImportLeaseRejected
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.importReservation != reservation ||
		scheduler.importAdmission != &reservation.admission ||
		scheduler.closed || scheduler.ctx.Err() != nil ||
		scheduler.failure != nil || scheduler.resetFence != reset ||
		!scheduler.resetActive.Load() || !scheduler.resetDrained || !scheduler.emptyLocked() ||
		scheduler.importAdmission.open.Load() ||
		scheduler.importAdmission.activation.Load() !=
			retainedImportActivationStarted {
		return errRetainedImportLeaseRejected
	}
	scheduler.resetFence = retainedusb.ImportResetLease{}
	scheduler.resetDrained = false
	scheduler.endpointNextService = [3]time.Time{}
	scheduler.importAdmission.open.Store(true)
	scheduler.resetActive.Store(false)
	return nil
}

func (scheduler *retainedSubmissionScheduler) emptyLocked() bool {
	for index := range scheduler.lanes {
		if scheduler.lanes[index].orderCount != 0 ||
			scheduler.lanes[index].freeCount != len(scheduler.lanes[index].slots) {
			return false
		}
	}
	return true
}

func (scheduler *retainedSubmissionScheduler) signal() {
	if scheduler == nil {
		return
	}
	select {
	case scheduler.wake <- struct{}{}:
	default:
	}
}

func (scheduler *retainedSubmissionScheduler) enqueue(
	envelope retainedSubmissionEnvelope,
) error {
	return scheduler.enqueueWithFramingReservation(envelope, false)
}

func (scheduler *retainedSubmissionScheduler) enqueueWithFramingReservation(
	envelope retainedSubmissionEnvelope,
	requireReservation bool,
) error {
	if scheduler == nil {
		return errRetainedSubmissionUninitialized
	}
	if err := scheduler.validateEnvelope(envelope); err != nil {
		return err
	}
	laneIndex, _ := envelope.lane.Index()

	scheduler.mu.Lock()
	if scheduler.closed || scheduler.ctx.Err() != nil ||
		(scheduler.importAdmission != nil &&
			!scheduler.importAdmission.open.Load()) {
		scheduler.mu.Unlock()
		if scheduler.importAdmission != nil &&
			!scheduler.importAdmission.open.Load() {
			return errRetainedSubmissionNotActivated
		}
		return errRetainedSubmissionClosed
	}
	if requireReservation {
		if _, reserved := scheduler.framingSequences[envelope.sequence]; !reserved {
			scheduler.mu.Unlock()
			return errRetainedSubmissionInvalidRequest
		}
	} else if _, reserved := scheduler.framingSequences[envelope.sequence]; reserved {
		scheduler.mu.Unlock()
		return errRetainedSubmissionDuplicateSequence
	}
	if scheduler.sequencePendingLocked(envelope.sequence) {
		scheduler.mu.Unlock()
		return errRetainedSubmissionDuplicateSequence
	}
	if scheduler.nextOrdinal == ^uint64(0) {
		scheduler.mu.Unlock()
		return errRetainedSubmissionCounterExhausted
	}
	if scheduler.lifecycleCompletionPending {
		scheduler.mu.Unlock()
		return errRetainedSubmissionLifecycleCompletionPending
	}
	if scheduler.controlLifecycleConfigured {
		envelope.bindingGeneration = scheduler.bindingGeneration
		if envelope.lane != retainedusb.LaneControl &&
			(scheduler.activeConfiguration == 0 ||
				envelope.route.AlternateSetting !=
					scheduler.activeAlternateSetting) {
			scheduler.mu.Unlock()
			return errRetainedSubmissionInactiveRoute
		}
	}
	lane := &scheduler.lanes[laneIndex]
	if lane.freeCount == 0 {
		completionPending := false
		for position := 0; position < lane.orderCount; position++ {
			slotIndex := lane.order[(lane.orderHead+position)%len(lane.order)]
			state := lane.slots[slotIndex].state
			if state == retainedSubmissionAdmitted ||
				state == retainedSubmissionTerminal {
				completionPending = true
				break
			}
		}
		scheduler.mu.Unlock()
		if completionPending {
			return errRetainedSubmissionVisibleCompletionPending
		}
		return errRetainedSubmissionQueueFull
	}

	lane.freeCount--
	slotIndex := lane.free[lane.freeCount]
	slot := &lane.slots[slotIndex]
	scheduler.nextOrdinal++
	slot.state = retainedSubmissionRawQueued
	slot.lane = envelope.lane
	slot.direction = envelope.direction
	slot.sessionGeneration = scheduler.session
	slot.bindingGeneration = envelope.bindingGeneration
	slot.ingressOrdinal = scheduler.nextOrdinal
	slot.sequence = envelope.sequence
	slot.transferLength = envelope.transferLength
	slot.setup = envelope.setup
	slot.route = envelope.route
	slot.controlLifecycle = envelope.controlLifecycle
	slot.payloadLength = len(envelope.data)
	if slot.payloadLength > 0 {
		copy(slot.payload[:slot.payloadLength], envelope.data)
	}

	orderIndex := (lane.orderHead + lane.orderCount) % len(lane.order)
	lane.order[orderIndex] = slotIndex
	lane.orderCount++
	if requireReservation {
		delete(scheduler.framingSequences, envelope.sequence)
	}
	scheduler.mu.Unlock()
	scheduler.signal()
	return nil
}

// reserveFramingSequence acquires the connection-wide response sequence at
// header arrival. It closes the predecessor-removal race while the reader is
// blocked consuming an OUT body or ISO descriptor tail.
func (scheduler *retainedSubmissionScheduler) reserveFramingSequence(
	sequence uint32,
) error {
	if scheduler == nil {
		return errRetainedSubmissionUninitialized
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.closed || scheduler.ctx.Err() != nil {
		return errRetainedSubmissionClosed
	}
	if scheduler.importAdmission != nil &&
		!scheduler.importAdmission.open.Load() {
		return errRetainedSubmissionNotActivated
	}
	if scheduler.sequencePendingLocked(sequence) {
		return errRetainedSubmissionDuplicateSequence
	}
	if _, exists := scheduler.framingSequences[sequence]; exists {
		return errRetainedSubmissionDuplicateSequence
	}
	scheduler.framingSequences[sequence] = struct{}{}
	return nil
}

// finishImmediateFramingSequence releases an exact header reservation. The
// command reader remains the sole producer and calls this immediately before
// its serialized response write, so no later header can overtake the release.
func (scheduler *retainedSubmissionScheduler) finishImmediateFramingSequence(
	sequence uint32,
) error {
	if scheduler == nil {
		return errRetainedSubmissionUninitialized
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if _, exists := scheduler.framingSequences[sequence]; !exists {
		return errRetainedSubmissionInvalidRequest
	}
	delete(scheduler.framingSequences, sequence)
	return nil
}

func (scheduler *retainedSubmissionScheduler) abandonFramingSequence(
	sequence uint32,
) {
	if scheduler == nil {
		return
	}
	scheduler.mu.Lock()
	delete(scheduler.framingSequences, sequence)
	scheduler.mu.Unlock()
}

func (scheduler *retainedSubmissionScheduler) validateEnvelope(
	envelope retainedSubmissionEnvelope,
) error {
	if !envelope.lane.Valid() || !envelope.direction.Valid() ||
		envelope.bindingGeneration == 0 ||
		envelope.transferLength > maximumTransferSize {
		return errRetainedSubmissionInvalidRequest
	}
	switch envelope.lane {
	case retainedusb.LaneControl:
		if envelope.route != (retainedusb.Route{}) {
			return errRetainedSubmissionInvalidRequest
		}
		setupDirectionIn := envelope.setup[0]&0x80 != 0
		if setupDirectionIn !=
			(envelope.direction == retainedusb.DirectionIn) {
			return errRetainedSubmissionInvalidRequest
		}
		setupLength := uint32(binary.LittleEndian.Uint16(envelope.setup[6:8]))
		if envelope.direction == retainedusb.DirectionIn {
			if len(envelope.data) != 0 {
				return errRetainedSubmissionInvalidRequest
			}
		} else if uint64(len(envelope.data)) !=
			uint64(envelope.transferLength) ||
			envelope.transferLength != setupLength ||
			envelope.transferLength > scheduler.limits.MaximumControlOut {
			return errRetainedSubmissionInvalidRequest
		}
	case retainedusb.LaneInterruptIn:
		if envelope.direction != retainedusb.DirectionIn ||
			envelope.route != scheduler.limits.InterruptInRoute ||
			len(envelope.data) != 0 || envelope.transferLength == 0 {
			return errRetainedSubmissionInvalidRequest
		}
	case retainedusb.LaneInterruptOut:
		if envelope.direction != retainedusb.DirectionOut ||
			envelope.route != scheduler.limits.InterruptOutRoute ||
			envelope.transferLength == 0 ||
			uint64(len(envelope.data)) != uint64(envelope.transferLength) ||
			envelope.transferLength > scheduler.limits.MaximumInterruptOut {
			return errRetainedSubmissionInvalidRequest
		}
	default:
		return errRetainedSubmissionInvalidRequest
	}
	return nil
}

func (scheduler *retainedSubmissionScheduler) sequencePendingLocked(
	sequence uint32,
) bool {
	for laneIndex := range scheduler.lanes {
		lane := &scheduler.lanes[laneIndex]
		for position := 0; position < lane.orderCount; position++ {
			slotIndex := lane.order[(lane.orderHead+position)%len(lane.order)]
			if lane.slots[slotIndex].sequence == sequence {
				return true
			}
		}
	}
	return false
}

func (scheduler *retainedSubmissionScheduler) serviceRound(
	now time.Time,
) (bool, time.Time, error) {
	if scheduler == nil {
		return false, time.Time{}, errRetainedSubmissionUninitialized
	}
	select {
	case <-scheduler.ctx.Done():
		return false, time.Time{}, errRetainedSubmissionClosed
	case <-scheduler.serviceAdmission:
	}
	defer func() { scheduler.serviceAdmission <- struct{}{} }()
	if scheduler.ctx.Err() != nil {
		return false, time.Time{}, errRetainedSubmissionClosed
	}
	if err := scheduler.pollImportRetirement(); err != nil {
		return false, time.Time{}, err
	}

	readinessEpoch, err := scheduler.readReadiness()
	if err != nil {
		return false, time.Time{}, err
	}
	var attempted [3]bool
	for {
		scheduler.mu.Lock()
		if scheduler.closed || scheduler.ctx.Err() != nil {
			scheduler.mu.Unlock()
			return false, time.Time{}, errRetainedSubmissionClosed
		}
		ref, found, nextRetry := scheduler.oldestEligibleLocked(
			attempted, readinessEpoch, now)
		if !found {
			scheduler.mu.Unlock()
			return false, nextRetry, nil
		}
		slot := &scheduler.lanes[ref.laneIndex].slots[ref.slotIndex]
		if slot.state == retainedSubmissionRawQueued {
			if slot.lane == retainedusb.LaneControl &&
				scheduler.controlLifecycleConfigured {
				// EP0 is FIFO and is not retired by a predecessor lifecycle
				// transition. Refresh both its generation and lifecycle
				// classification only when it reaches service, after every
				// earlier control completion has published server/device state.
				slot.bindingGeneration = scheduler.bindingGeneration
				if scheduler.resolveControlLifecycle != nil {
					slot.controlLifecycle = scheduler.resolveControlLifecycle(
						slot.setup, slot.direction, slot.transferLength)
				}
			}
			ticket, stageErr := invokeRetainedStage(
				scheduler.owner, scheduler.requestForSlotLocked(slot))
			if stageErr != nil || !scheduler.ticketValidLocked(ticket, slot) {
				slot.state = retainedSubmissionLifecycleRetired
				scheduler.removeRefLocked(ref)
				scheduler.mu.Unlock()
				if stageErr == nil {
					stageErr = errRetainedSubmissionInvalidTicket
				}
				return false, time.Time{}, stageErr
			}
			slot.ticket = ticket
			slot.state = retainedSubmissionTicketed
		}
		scheduler.mu.Unlock()

		terminal, pending, attemptErr := scheduler.attempt(ref, now)
		if attemptErr != nil {
			// A terminal response owns and removes the job even when delivery or
			// completion reports an error. Preserve that progress bit so callers
			// never mistake an already-consumed submission for queued work.
			return terminal, time.Time{}, attemptErr
		}
		if terminal {
			return true, time.Time{}, nil
		}
		if pending {
			attempted[ref.laneIndex] = true
		}
	}
}

func (scheduler *retainedSubmissionScheduler) requestForSlotLocked(
	slot *retainedSubmissionSlot,
) retainedusb.Request {
	data := slot.payload[:slot.payloadLength:slot.payloadLength]
	return retainedusb.Request{
		Lane:              slot.lane,
		Direction:         slot.direction,
		SessionGeneration: slot.sessionGeneration,
		BindingGeneration: slot.bindingGeneration,
		IngressOrdinal:    slot.ingressOrdinal,
		Sequence:          slot.sequence,
		TransferLength:    slot.transferLength,
		Setup:             slot.setup,
		Route:             slot.route,
		Data:              data,
	}
}

func (scheduler *retainedSubmissionScheduler) ticketValidLocked(
	ticket retainedusb.Ticket,
	slot *retainedSubmissionSlot,
) bool {
	if !ticket.Valid() || ticket.OwnerID != scheduler.ownerIdentity ||
		ticket.SessionGeneration != scheduler.session ||
		ticket.Lane != slot.lane {
		return false
	}
	for laneIndex := range scheduler.lanes {
		lane := &scheduler.lanes[laneIndex]
		for position := 0; position < lane.orderCount; position++ {
			slotIndex := lane.order[(lane.orderHead+position)%len(lane.order)]
			other := &lane.slots[slotIndex]
			if other == slot || !other.ticket.Valid() {
				continue
			}
			if other.ticket.OwnerID == ticket.OwnerID &&
				other.ticket.Token == ticket.Token &&
				other.ticket.Generation == ticket.Generation &&
				other.ticket.SessionGeneration == ticket.SessionGeneration {
				return false
			}
		}
	}
	return true
}

func (scheduler *retainedSubmissionScheduler) oldestEligibleLocked(
	attempted [3]bool,
	readinessEpoch uint64,
	now time.Time,
) (retainedSubmissionRef, bool, time.Time) {
	var selected retainedSubmissionRef
	var selectedOrdinal uint64
	var nextRetry time.Time
	for laneIndex := range scheduler.lanes {
		lane := &scheduler.lanes[laneIndex]
		if lane.orderCount == 0 {
			continue
		}
		slotIndex := lane.order[lane.orderHead]
		slot := &lane.slots[slotIndex]
		if serviceAt := scheduler.endpointNextService[laneIndex]; now.Before(serviceAt) {
			if nextRetry.IsZero() || serviceAt.Before(nextRetry) {
				nextRetry = serviceAt
			}
			continue
		}
		if slot.state == retainedSubmissionPending && !slot.retryAt.IsZero() &&
			now.Before(slot.retryAt) &&
			(nextRetry.IsZero() || slot.retryAt.Before(nextRetry)) {
			nextRetry = slot.retryAt
		}
		if attempted[laneIndex] {
			continue
		}
		eligible := false
		switch slot.state {
		case retainedSubmissionRawQueued, retainedSubmissionTicketed:
			eligible = true
		case retainedSubmissionPending:
			eligible = readinessEpoch > slot.pendingReadinessEpoch ||
				(!slot.retryAt.IsZero() && !now.Before(slot.retryAt))
		}
		if !eligible {
			continue
		}
		if selectedOrdinal == 0 || slot.ingressOrdinal < selectedOrdinal {
			selectedOrdinal = slot.ingressOrdinal
			selected = retainedSubmissionRef{
				laneIndex: laneIndex, slotIndex: slotIndex,
				ingressOrdinal: slot.ingressOrdinal,
			}
		}
	}
	return selected, selectedOrdinal != 0, nextRetry
}

func (scheduler *retainedSubmissionScheduler) attempt(
	ref retainedSubmissionRef,
	attemptAt time.Time,
) (bool, bool, error) {
	var preparedTicket retainedusb.Ticket
	var lostOwnership bool
	var pendingReadinessEpoch uint64
	var lifecycleTerminalSuccess bool
	packet, written, err := scheduler.responses.writeLateRetSubmit(
		scheduler.responseScratch,
		scheduler.sequenceForRef(ref),
		scheduler.maximumResponseLength,
		attemptAt,
		func(payload []byte) (lateResponseSelection, error) {
			scheduler.mu.Lock()
			defer scheduler.mu.Unlock()
			slot, valid := scheduler.slotForRefLocked(ref)
			if !valid || scheduler.closed || scheduler.ctx.Err() != nil ||
				(slot.state != retainedSubmissionTicketed &&
					slot.state != retainedSubmissionPending) ||
				slot.sessionGeneration != scheduler.session ||
				scheduler.lanes[ref.laneIndex].order[scheduler.lanes[ref.laneIndex].orderHead] != ref.slotIndex {
				lostOwnership = true
				return lateResponseSelection{Result: lateResponsePending}, nil
			}
			preparedTicket = slot.ticket
			selectedAt := attemptAt
			if scheduler.endpointCadenceConfigured {
				// Selection can wait for response ownership. Re-sample here so
				// that overdue slots are never replayed using a pre-wait clock.
				selectedAt = time.Now()
			}
			window := scheduler.responseWindowLocked(slot)
			destination := payload[:window:window]
			preparation, preparationErr := invokeRetainedPrepare(
				scheduler.owner, slot.ticket, destination, selectedAt)
			if preparationErr != nil {
				return lateResponseSelection{}, preparationErr
			}
			if scheduler.ctx.Err() != nil {
				lostOwnership = true
				return lateResponseSelection{Result: lateResponsePending}, nil
			}
			selection, validationErr := scheduler.validatePreparationLocked(
				slot, preparation, window, selectedAt)
			if validationErr != nil {
				return lateResponseSelection{}, validationErr
			}
			if preparation.Result == retainedusb.ResultPending {
				pendingReadinessEpoch = preparation.ReadinessEpoch
				slot.state = retainedSubmissionPending
				slot.pendingReadinessEpoch = preparation.ReadinessEpoch
				slot.retryAt = preparation.RetryAt
				return selection, nil
			}
			lifecycleTerminalSuccess = slot.controlLifecycle.accepted &&
				preparation.Result == retainedusb.ResultSuccess
			if lifecycleTerminalSuccess {
				if scheduler.lifecycleCompletionPending {
					return lateResponseSelection{},
						errRetainedSubmissionInvalidPreparation
				}
				scheduler.lifecycleCompletionPending = true
			}
			slot.state = retainedSubmissionAdmitted
			scheduler.advanceEndpointCadenceLocked(ref.laneIndex, selectedAt)
			slot.pendingReadinessEpoch = 0
			slot.retryAt = time.Time{}
			return selection, nil
		},
		func(delivered bool) error {
			completedAt := time.Now()
			completionErr := invokeRetainedComplete(
				scheduler.owner, preparedTicket, delivered, completedAt)
			scheduler.mu.Lock()
			slot, valid := scheduler.slotForRefLocked(ref)
			if !valid || slot.state != retainedSubmissionAdmitted ||
				slot.ticket != preparedTicket {
				if lifecycleTerminalSuccess {
					scheduler.releaseLifecycleBarrierLocked()
				}
				scheduler.mu.Unlock()
				if completionErr != nil {
					return errors.Join(
						errRetainedSubmissionInvalidTicket, completionErr)
				}
				return errRetainedSubmissionInvalidTicket
			}
			lifecycle := slot.controlLifecycle
			applyLifecycle := delivered && completionErr == nil &&
				lifecycleTerminalSuccess &&
				scheduler.controlLifecycleConfigured
			var successorGeneration uint64
			if applyLifecycle {
				if scheduler.bindingGeneration == ^uint64(0) {
					completionErr = errRetainedSubmissionCounterExhausted
					applyLifecycle = false
				} else {
					scheduler.bindingGeneration++
					successorGeneration = scheduler.bindingGeneration
				}
			}
			slot.state = retainedSubmissionTerminal
			scheduler.removeRefLocked(ref)
			scheduler.mu.Unlock()
			lifecyclePublished := false
			if applyLifecycle {
				lifecycleErr := scheduler.retireDeliveredControlLifecycle(
					lifecycle, successorGeneration, completedAt)
				if lifecycleErr == nil {
					scheduler.publishControlLifecycle(lifecycle)
					lifecyclePublished = true
				} else {
					completionErr = errors.Join(completionErr, lifecycleErr)
				}
			}
			if lifecycleTerminalSuccess {
				scheduler.mu.Lock()
				if lifecyclePublished {
					scheduler.applyControlLifecycleRoutingLocked(lifecycle)
				}
				scheduler.releaseLifecycleBarrierLocked()
				scheduler.mu.Unlock()
			}
			scheduler.signal()
			return completionErr
		},
	)
	scheduler.responseScratch = packet
	if err != nil {
		if !written && !lostOwnership {
			retireErr := scheduler.retireRefIfOwned(
				ref, retainedusb.RetirePrepareFailure, time.Now())
			if retireErr != nil {
				return false, false, errors.Join(err, retireErr)
			}
		}
		return written, false, err
	}
	if written {
		return true, false, nil
	}
	if lostOwnership {
		return false, false, nil
	}
	if scheduler.ctx.Err() != nil {
		return false, false, nil
	}
	publishedReadinessEpoch, readinessErr := scheduler.readReadiness()
	if readinessErr != nil ||
		publishedReadinessEpoch < pendingReadinessEpoch {
		if readinessErr == nil {
			readinessErr = errRetainedSubmissionInvalidPreparation
		}
		retireErr := scheduler.retireRefIfOwned(
			ref, retainedusb.RetirePrepareFailure, time.Now())
		if retireErr != nil {
			return false, false, errors.Join(readinessErr, retireErr)
		}
		return false, false, readinessErr
	}
	return false, true, nil
}

func (scheduler *retainedSubmissionScheduler) applyControlLifecycleRoutingLocked(
	lifecycle controlLifecycleSetup,
) {
	scheduler.resetEndpointCadenceLocked(lifecycle)
	switch lifecycle.kind {
	case controlLifecycleSetInterface:
		scheduler.activeAlternateSetting = lifecycle.alternateSetting
	case controlLifecycleSetConfiguration:
		scheduler.activeConfiguration = lifecycle.configurationValue
		scheduler.activeAlternateSetting = 0
	}
}

func (scheduler *retainedSubmissionScheduler) releaseLifecycleBarrierLocked() {
	if !scheduler.lifecycleCompletionPending {
		return
	}
	scheduler.lifecycleCompletionPending = false
	select {
	case scheduler.lifecycleReleased <- struct{}{}:
	default:
	}
}

// retireDeliveredControlLifecycle retires only non-EP0 work which entered
// before the lifecycle response's generation rotation. New submissions are
// stamped with successorGeneration under scheduler.mu and cannot be swept by
// this pass even if the command reader runs before retirement callbacks finish.
func (scheduler *retainedSubmissionScheduler) retireDeliveredControlLifecycle(
	lifecycle controlLifecycleSetup,
	successorGeneration uint64,
	now time.Time,
) error {
	if scheduler == nil || !lifecycle.accepted || successorGeneration == 0 {
		return errRetainedSubmissionInvalidRequest
	}
	var reason retainedusb.RetireReason
	var routeMatches func(retainedusb.Route) bool
	switch lifecycle.kind {
	case controlLifecycleClearEndpointHalt:
		reason = retainedusb.RetireEndpointReset
		routeMatches = func(route retainedusb.Route) bool {
			return route.EndpointAddress == lifecycle.endpointAddress
		}
	case controlLifecycleSetInterface:
		reason = retainedusb.RetireInterfaceReset
		routeMatches = func(route retainedusb.Route) bool {
			return route.InterfaceNumber == lifecycle.interfaceNumber
		}
	case controlLifecycleSetConfiguration:
		reason = retainedusb.RetireConfigurationChange
		routeMatches = func(retainedusb.Route) bool { return true }
	default:
		return errRetainedSubmissionInvalidRequest
	}
	return scheduler.retireMatching(
		func(candidate *retainedSubmissionSlot) bool {
			return candidate.lane != retainedusb.LaneControl &&
				candidate.bindingGeneration < successorGeneration &&
				routeMatches(candidate.route)
		}, reason, now)
}

func (scheduler *retainedSubmissionScheduler) sequenceForRef(
	ref retainedSubmissionRef,
) uint32 {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if slot, valid := scheduler.slotForRefLocked(ref); valid {
		return slot.sequence
	}
	return 0
}

func (scheduler *retainedSubmissionScheduler) responseWindowLocked(
	slot *retainedSubmissionSlot,
) int {
	switch slot.lane {
	case retainedusb.LaneControl:
		if slot.direction != retainedusb.DirectionIn {
			return 0
		}
		setupLength := uint32(binary.LittleEndian.Uint16(slot.setup[6:8]))
		return int(min(
			scheduler.limits.MaximumControlResponse,
			slot.transferLength,
			setupLength,
		))
	case retainedusb.LaneInterruptIn:
		return int(min(
			scheduler.limits.MaximumInterruptIn,
			slot.transferLength,
		))
	default:
		return 0
	}
}

func (scheduler *retainedSubmissionScheduler) validatePreparationLocked(
	slot *retainedSubmissionSlot,
	preparation retainedusb.Preparation,
	responseWindow int,
	attemptAt time.Time,
) (lateResponseSelection, error) {
	if !preparation.Result.Valid() {
		return lateResponseSelection{}, errRetainedSubmissionInvalidPreparation
	}
	if preparation.Result == retainedusb.ResultPending {
		if preparation.ActualLength != 0 ||
			(!preparation.RetryAt.IsZero() &&
				!preparation.RetryAt.After(attemptAt)) ||
			preparation.ReadinessEpoch < scheduler.lastReadinessEpoch ||
			preparation.ReadinessEpoch == ^uint64(0) {
			return lateResponseSelection{}, errRetainedSubmissionInvalidPreparation
		}
		return lateResponseSelection{Result: lateResponsePending}, nil
	}
	if !preparation.RetryAt.IsZero() || preparation.ReadinessEpoch != 0 {
		return lateResponseSelection{}, errRetainedSubmissionInvalidPreparation
	}
	switch preparation.Result {
	case retainedusb.ResultData:
		if preparation.ActualLength == 0 ||
			slot.lane == retainedusb.LaneInterruptOut ||
			(slot.lane == retainedusb.LaneControl &&
				slot.direction != retainedusb.DirectionIn) ||
			uint64(preparation.ActualLength) > uint64(responseWindow) {
			return lateResponseSelection{}, errRetainedSubmissionInvalidPreparation
		}
		return lateResponseSelection{
			Result: lateResponseData, ActualLength: preparation.ActualLength,
		}, nil
	case retainedusb.ResultSuccess:
		expected := uint32(0)
		if slot.lane == retainedusb.LaneInterruptIn {
			return lateResponseSelection{}, errRetainedSubmissionInvalidPreparation
		}
		if slot.direction == retainedusb.DirectionOut {
			expected = slot.transferLength
		}
		if preparation.ActualLength != expected {
			return lateResponseSelection{}, errRetainedSubmissionInvalidPreparation
		}
		return lateResponseSelection{
			Result:       lateResponseSuccess,
			ActualLength: preparation.ActualLength,
		}, nil
	case retainedusb.ResultStall:
		if slot.lane == retainedusb.LaneInterruptIn ||
			preparation.ActualLength != 0 {
			return lateResponseSelection{}, errRetainedSubmissionInvalidPreparation
		}
		return lateResponseSelection{Result: lateResponseStall}, nil
	default:
		return lateResponseSelection{}, errRetainedSubmissionInvalidPreparation
	}
}

func (scheduler *retainedSubmissionScheduler) slotForRefLocked(
	ref retainedSubmissionRef,
) (*retainedSubmissionSlot, bool) {
	if ref.laneIndex < 0 || ref.laneIndex >= len(scheduler.lanes) {
		return nil, false
	}
	lane := &scheduler.lanes[ref.laneIndex]
	if ref.slotIndex < 0 || ref.slotIndex >= len(lane.slots) {
		return nil, false
	}
	slot := &lane.slots[ref.slotIndex]
	return slot, slot.state != retainedSubmissionFree &&
		slot.ingressOrdinal == ref.ingressOrdinal
}

func (scheduler *retainedSubmissionScheduler) removeRefLocked(
	ref retainedSubmissionRef,
) {
	lane := &scheduler.lanes[ref.laneIndex]
	for position := 0; position < lane.orderCount; position++ {
		orderIndex := (lane.orderHead + position) % len(lane.order)
		if lane.order[orderIndex] != ref.slotIndex {
			continue
		}
		for offset := position; offset < lane.orderCount-1; offset++ {
			to := (lane.orderHead + offset) % len(lane.order)
			from := (lane.orderHead + offset + 1) % len(lane.order)
			lane.order[to] = lane.order[from]
		}
		lane.orderCount--
		scheduler.releaseSlotLocked(ref.laneIndex, ref.slotIndex)
		return
	}
}

func (scheduler *retainedSubmissionScheduler) releaseSlotLocked(
	laneIndex, slotIndex int,
) {
	lane := &scheduler.lanes[laneIndex]
	slot := &lane.slots[slotIndex]
	if cap(slot.payload) > 0 {
		clear(slot.payload[:cap(slot.payload)])
	}
	payload := slot.payload[:0]
	*slot = retainedSubmissionSlot{payload: payload}
	lane.free[lane.freeCount] = slotIndex
	lane.freeCount++
	if lane.orderCount == 0 {
		lane.orderHead = 0
	}
	select {
	case lane.released <- struct{}{}:
	default:
	}
}

// enqueueAfterVisibleCompletion preserves the one-ticket owner invariant while
// closing the response-visibility race. It waits only when every slot is full
// because an admitted/terminal response is completing; a genuinely queued or
// pending lane still returns QueueFull immediately so the reader remains able
// to process UNLINK and other connection commands.
func (scheduler *retainedSubmissionScheduler) enqueueAfterVisibleCompletion(
	envelope retainedSubmissionEnvelope,
) error {
	laneIndex, valid := envelope.lane.Index()
	if scheduler == nil || !valid {
		return errRetainedSubmissionInvalidRequest
	}
	for {
		err := scheduler.enqueue(envelope)
		if !errors.Is(err, errRetainedSubmissionVisibleCompletionPending) &&
			!errors.Is(err, errRetainedSubmissionLifecycleCompletionPending) {
			return err
		}
		scheduler.mu.Lock()
		released := scheduler.lifecycleReleased
		if errors.Is(err, errRetainedSubmissionVisibleCompletionPending) {
			released = scheduler.lanes[laneIndex].released
		}
		scheduler.mu.Unlock()
		select {
		case <-released:
		case <-scheduler.ctx.Done():
			return errRetainedSubmissionClosed
		}
	}
}

func (scheduler *retainedSubmissionScheduler) enqueueReservedAfterVisibleCompletion(
	envelope retainedSubmissionEnvelope,
) error {
	laneIndex, valid := envelope.lane.Index()
	if scheduler == nil || !valid {
		return errRetainedSubmissionInvalidRequest
	}
	for {
		err := scheduler.enqueueWithFramingReservation(envelope, true)
		if !errors.Is(err, errRetainedSubmissionVisibleCompletionPending) &&
			!errors.Is(err, errRetainedSubmissionLifecycleCompletionPending) {
			return err
		}
		scheduler.mu.Lock()
		released := scheduler.lifecycleReleased
		if errors.Is(err, errRetainedSubmissionVisibleCompletionPending) {
			released = scheduler.lanes[laneIndex].released
		}
		scheduler.mu.Unlock()
		select {
		case <-released:
		case <-scheduler.ctx.Done():
			return errRetainedSubmissionClosed
		}
	}
}

func (scheduler *retainedSubmissionScheduler) unlink(
	sequence uint32,
	now time.Time,
) (bool, error) {
	if scheduler == nil {
		return false, errRetainedSubmissionUninitialized
	}
	scheduler.mu.Lock()
	ref, slot, found := scheduler.findSequenceLocked(sequence)
	if !found || slot.state == retainedSubmissionAdmitted ||
		slot.state == retainedSubmissionTerminal ||
		slot.state == retainedSubmissionLifecycleRetired {
		scheduler.mu.Unlock()
		return false, nil
	}
	if slot.state == retainedSubmissionRawQueued {
		slot.state = retainedSubmissionLifecycleRetired
		scheduler.removeRefLocked(ref)
		scheduler.mu.Unlock()
		scheduler.signal()
		return true, nil
	}
	ticket := slot.ticket
	slot.state = retainedSubmissionLifecycleRetired
	scheduler.mu.Unlock()
	err := scheduler.retireOne(ref, ticket, retainedusb.RetireUnlink, now)
	if err != nil {
		// A failed Retire leaves ticket ownership ambiguous. Latch it before the
		// stream returns so session close takes the containment-only path and
		// cannot neutralize or release this owner as though retirement succeeded.
		scheduler.recordFailure(err)
	}
	scheduler.signal()
	return true, err
}

func (scheduler *retainedSubmissionScheduler) findSequenceLocked(
	sequence uint32,
) (retainedSubmissionRef, *retainedSubmissionSlot, bool) {
	for laneIndex := range scheduler.lanes {
		lane := &scheduler.lanes[laneIndex]
		for position := 0; position < lane.orderCount; position++ {
			slotIndex := lane.order[(lane.orderHead+position)%len(lane.order)]
			slot := &lane.slots[slotIndex]
			if slot.sequence == sequence {
				return retainedSubmissionRef{
					laneIndex: laneIndex, slotIndex: slotIndex,
					ingressOrdinal: slot.ingressOrdinal,
				}, slot, true
			}
		}
	}
	return retainedSubmissionRef{}, nil, false
}

func (scheduler *retainedSubmissionScheduler) retireOne(
	ref retainedSubmissionRef,
	ticket retainedusb.Ticket,
	reason retainedusb.RetireReason,
	now time.Time,
) error {
	var retireErr error
	if ticket != (retainedusb.Ticket{}) {
		retireErr = invokeRetainedRetire(
			scheduler.owner, ticket, reason, now)
	}
	scheduler.mu.Lock()
	if slot, valid := scheduler.slotForRefLocked(ref); valid &&
		slot.state == retainedSubmissionLifecycleRetired {
		scheduler.removeRefLocked(ref)
	}
	scheduler.mu.Unlock()
	scheduler.signal()
	return retireErr
}

func (scheduler *retainedSubmissionScheduler) retireRefIfOwned(
	ref retainedSubmissionRef,
	reason retainedusb.RetireReason,
	now time.Time,
) error {
	scheduler.mu.Lock()
	slot, valid := scheduler.slotForRefLocked(ref)
	if !valid || (slot.state != retainedSubmissionTicketed &&
		slot.state != retainedSubmissionPending) {
		scheduler.mu.Unlock()
		return nil
	}
	ticket := slot.ticket
	slot.state = retainedSubmissionLifecycleRetired
	scheduler.mu.Unlock()
	return scheduler.retireOne(ref, ticket, reason, now)
}

func (scheduler *retainedSubmissionScheduler) retireLane(
	lane retainedusb.Lane,
	reason retainedusb.RetireReason,
	now time.Time,
) error {
	index, valid := lane.Index()
	if scheduler == nil || !valid || !reason.Valid() {
		return errRetainedSubmissionInvalidRequest
	}
	return scheduler.retireMatching(
		func(candidate *retainedSubmissionSlot) bool {
			candidateIndex, _ := candidate.lane.Index()
			return candidateIndex == index
		}, reason, now)
}

func (scheduler *retainedSubmissionScheduler) retireBinding(
	route retainedusb.Route,
	bindingGeneration uint64,
	reason retainedusb.RetireReason,
	now time.Time,
) error {
	if scheduler == nil || bindingGeneration == 0 || !reason.Valid() {
		return errRetainedSubmissionInvalidRequest
	}
	var lane retainedusb.Lane
	switch route {
	case scheduler.limits.InterruptInRoute:
		lane = retainedusb.LaneInterruptIn
	case scheduler.limits.InterruptOutRoute:
		lane = retainedusb.LaneInterruptOut
	default:
		return errRetainedSubmissionInvalidRequest
	}
	return scheduler.retireMatching(
		func(candidate *retainedSubmissionSlot) bool {
			return candidate.lane == lane && candidate.route == route &&
				candidate.bindingGeneration == bindingGeneration
		}, reason, now)
}

func (scheduler *retainedSubmissionScheduler) retireSession(
	sessionGeneration uint64,
	reason retainedusb.RetireReason,
	now time.Time,
) error {
	if scheduler == nil || sessionGeneration == 0 ||
		sessionGeneration != scheduler.session || !reason.Valid() {
		return errRetainedSubmissionInvalidRequest
	}
	return scheduler.retireMatching(
		func(candidate *retainedSubmissionSlot) bool {
			return candidate.sessionGeneration == sessionGeneration
		}, reason, now)
}

func (scheduler *retainedSubmissionScheduler) retireMatching(
	matches func(*retainedSubmissionSlot) bool,
	reason retainedusb.RetireReason,
	now time.Time,
) error {
	// FIFO staging permits only the head of each lane to own a ticket. Raw
	// followers are removed in place, so a three-lane scheduler can collect at
	// most exactly three owner retirements without allocating.
	var retirements [3]retainedSubmissionRetirement
	retirementCount := 0
	scheduler.mu.Lock()
	for laneIndex := range scheduler.lanes {
		lane := &scheduler.lanes[laneIndex]
		for position := lane.orderCount - 1; position >= 0; position-- {
			slotIndex := lane.order[(lane.orderHead+position)%len(lane.order)]
			slot := &lane.slots[slotIndex]
			if !matches(slot) || slot.state == retainedSubmissionAdmitted ||
				slot.state == retainedSubmissionTerminal ||
				slot.state == retainedSubmissionLifecycleRetired {
				continue
			}
			ref := retainedSubmissionRef{
				laneIndex: laneIndex, slotIndex: slotIndex,
				ingressOrdinal: slot.ingressOrdinal,
			}
			if slot.state == retainedSubmissionRawQueued {
				slot.state = retainedSubmissionLifecycleRetired
				scheduler.removeRefLocked(ref)
				continue
			}
			slot.state = retainedSubmissionLifecycleRetired
			retirements[retirementCount] = retainedSubmissionRetirement{
				ref: ref, ticket: slot.ticket,
			}
			retirementCount++
		}
	}
	scheduler.mu.Unlock()

	var joined error
	for index := 0; index < retirementCount; index++ {
		retirement := retirements[index]
		if err := scheduler.retireOne(
			retirement.ref, retirement.ticket, reason, now); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	scheduler.signal()
	return joined
}

func (scheduler *retainedSubmissionScheduler) readReadiness() (uint64, error) {
	epoch, _, err := sampleRetainedReadiness(
		scheduler.owner, scheduler.readiness,
		scheduler.lastReadinessEpoch)
	if err != nil {
		return 0, err
	}
	scheduler.lastReadinessEpoch = epoch
	return epoch, nil
}

// sampleRetainedReadiness makes the epoch the level-triggered source of truth
// while consuming at most one latched wake. If a signal races the first epoch
// snapshot, the mandatory refresh observes the increment which preceded that
// signal; a later signal remains buffered for the worker select. Receiving a
// closed, empty channel fails immediately instead of allowing a busy worker to
// keep admitting jobs without reaching its select.
func sampleRetainedReadiness(
	owner retainedusb.Owner,
	expected <-chan struct{},
	minimumEpoch uint64,
) (uint64, <-chan struct{}, error) {
	epoch, readiness, err := invokeRetainedReadiness(owner)
	if err != nil {
		return 0, nil, err
	}
	if !retainedReadinessValid(
		epoch, readiness, expected, minimumEpoch) {
		return 0, nil, errRetainedSubmissionReadiness
	}
	select {
	case _, open := <-readiness:
		if !open {
			return 0, nil, errRetainedSubmissionReadiness
		}
		refreshedEpoch, refreshed, refreshErr := invokeRetainedReadiness(owner)
		if refreshErr != nil {
			return 0, nil, refreshErr
		}
		if !retainedReadinessValid(
			refreshedEpoch, refreshed, readiness, epoch) {
			return 0, nil, errRetainedSubmissionReadiness
		}
		epoch = refreshedEpoch
	default:
	}
	return epoch, readiness, nil
}

func retainedReadinessValid(
	epoch uint64,
	readiness <-chan struct{},
	expected <-chan struct{},
	minimumEpoch uint64,
) bool {
	return readiness != nil && cap(readiness) >= 1 &&
		(expected == nil || readiness == expected) &&
		epoch != ^uint64(0) && epoch >= minimumEpoch
}

func (scheduler *retainedSubmissionScheduler) run() {
	defer close(scheduler.done)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		now := time.Now()
		progressed, nextRetry, err := scheduler.serviceRound(now)
		if err != nil && !errors.Is(err, errRetainedSubmissionClosed) {
			scheduler.finishWorkerFailure(err, now)
			return
		}
		if errors.Is(err, errRetainedSubmissionClosed) ||
			scheduler.ctx.Err() != nil {
			if shutdownErr := scheduler.shutdown(
				retainedusb.RetireConnectionClose, time.Now()); shutdownErr != nil {
				scheduler.recordFailure(shutdownErr)
			}
			return
		}
		if progressed {
			continue
		}
		readinessEpoch, readinessErr := scheduler.readReadiness()
		if readinessErr != nil {
			scheduler.recordFailure(readinessErr)
			if shutdownErr := scheduler.shutdown(
				retainedusb.RetireInvariantFailure, time.Now()); shutdownErr != nil {
				scheduler.recordFailure(shutdownErr)
			}
			return
		}
		// A retirement may race Prepare and therefore share the epoch of its
		// Pending result. Sample again after consuming readiness and before
		// sleeping; a later publication still owns the next latched wake.
		if retirementErr := scheduler.pollImportRetirement(); retirementErr != nil {
			if errors.Is(retirementErr, errRetainedSubmissionClosed) {
				if shutdownErr := scheduler.shutdown(retainedusb.RetireConnectionClose, time.Now()); shutdownErr != nil {
					scheduler.recordFailure(shutdownErr)
				}
			} else {
				scheduler.finishWorkerFailure(retirementErr, time.Now())
			}
			return
		}
		if scheduler.hasEligible(readinessEpoch, time.Now()) {
			continue
		}

		var timerChannel <-chan time.Time
		if !nextRetry.IsZero() {
			wait := time.Until(nextRetry)
			if wait <= 0 {
				continue
			}
			resetReusableTimer(timer, wait)
			timerChannel = timer.C
		} else if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		select {
		case <-scheduler.ctx.Done():
			if shutdownErr := scheduler.shutdown(
				retainedusb.RetireConnectionClose, time.Now()); shutdownErr != nil {
				scheduler.recordFailure(shutdownErr)
			}
			return
		case <-scheduler.wake:
		case _, open := <-scheduler.readiness:
			if !open {
				scheduler.recordFailure(errRetainedSubmissionReadiness)
				if shutdownErr := scheduler.shutdown(
					retainedusb.RetireInvariantFailure, time.Now()); shutdownErr != nil {
					scheduler.recordFailure(errors.Join(
						errRetainedSubmissionReadiness, shutdownErr))
				}
				return
			}
		case <-timerChannel:
		}
	}
}

func (scheduler *retainedSubmissionScheduler) hasEligible(
	readinessEpoch uint64,
	now time.Time,
) bool {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	_, found, _ := scheduler.oldestEligibleLocked(
		[3]bool{}, readinessEpoch, now)
	return found
}

func (scheduler *retainedSubmissionScheduler) recordFailure(err error) {
	if scheduler == nil || err == nil {
		return
	}
	scheduler.mu.Lock()
	if scheduler.failure == nil {
		scheduler.failure = err
	} else if !errors.Is(scheduler.failure, err) {
		scheduler.failure = errors.Join(scheduler.failure, err)
	}
	scheduler.mu.Unlock()
}

func (scheduler *retainedSubmissionScheduler) shutdown(
	reason retainedusb.RetireReason,
	now time.Time,
) error {
	scheduler.mu.Lock()
	scheduler.closed = true
	scheduler.mu.Unlock()
	scheduler.cancel()
	return scheduler.retireSession(scheduler.session, reason, now)
}

func (scheduler *retainedSubmissionScheduler) close() error {
	if scheduler == nil {
		return errRetainedSubmissionUninitialized
	}
	scheduler.closeOnce.Do(func() {
		scheduler.mu.Lock()
		scheduler.closed = true
		if scheduler.importAdmission != nil {
			scheduler.importAdmission.open.Store(false)
			scheduler.importAdmission.activation.Store(
				retainedImportActivationRevoked)
		}
		started := scheduler.started
		scheduler.mu.Unlock()
		scheduler.cancel()
		scheduler.signal()
		if started {
			<-scheduler.done
		} else if err := scheduler.shutdown(
			retainedusb.RetireConnectionClose, time.Now()); err != nil {
			scheduler.recordFailure(err)
		}
	})
	scheduler.mu.Lock()
	err := scheduler.failure
	scheduler.mu.Unlock()
	return err
}

func invokeRetainedIdentity(owner retainedusb.Owner) (
	identity uint64,
	err error,
) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = retainedCallbackPanic("identity", recovered)
		}
	}()
	return owner.Identity(), nil
}

func invokeRetainedLimits(owner retainedusb.Owner) (
	limits retainedusb.Limits,
	err error,
) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = retainedCallbackPanic("limits", recovered)
		}
	}()
	return owner.Limits(), nil
}

func invokeRetainedStage(
	owner retainedusb.Owner,
	request retainedusb.Request,
) (ticket retainedusb.Ticket, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = retainedCallbackPanic("stage", recovered)
		}
	}()
	return owner.Stage(request)
}

func invokeRetainedPrepare(
	owner retainedusb.Owner,
	ticket retainedusb.Ticket,
	destination []byte,
	now time.Time,
) (preparation retainedusb.Preparation, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = retainedCallbackPanic("prepare", recovered)
		}
	}()
	return owner.Prepare(ticket, destination, now)
}

func invokeRetainedComplete(
	owner retainedusb.Owner,
	ticket retainedusb.Ticket,
	delivered bool,
	now time.Time,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = retainedCallbackPanic("complete", recovered)
		}
	}()
	return owner.Complete(ticket, delivered, now)
}

func invokeRetainedRetire(
	owner retainedusb.Owner,
	ticket retainedusb.Ticket,
	reason retainedusb.RetireReason,
	now time.Time,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = retainedCallbackPanic("retire", recovered)
		}
	}()
	return owner.Retire(ticket, reason, now)
}

func invokeRetainedReadiness(owner retainedusb.Owner) (
	epoch uint64,
	readiness <-chan struct{},
	err error,
) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = retainedCallbackPanic("readiness", recovered)
		}
	}()
	epoch, readiness = owner.Readiness()
	return epoch, readiness, nil
}

func retainedCallbackPanic(stage string, recovered any) error {
	if recoveredErr, ok := recovered.(error); ok {
		return fmt.Errorf("%s callback panic: %w", stage, recoveredErr)
	}
	return fmt.Errorf("%s callback panic: %v", stage, recovered)
}

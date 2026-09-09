package xboxone

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
)

const (
	dormantRetainedUSBPendingRetry        = time.Millisecond
	dormantRetainedUSBMinimumLocalTimeout = time.Millisecond
	dormantRetainedUSBMaximumLocalTimeout = 5 * time.Second
	dormantRetainedUSBMaximumDurationMS   = uint64((1<<63 - 1) / int64(time.Millisecond))
	// The Windows Xbox GIP stack keeps a pipeline of interrupt-IN URBs active.
	// A one-entry transport queue answers every request behind the head with
	// -ENOSPC, which the inbox driver treats as a pipe fault and follows with a
	// CLEAR_FEATURE(ENDPOINT_HALT) storm. These are host request envelopes only:
	// the adapter still stages exactly one canonical interrupt-IN ticket at a
	// time, and IN envelopes own no payload slab. Keep the bounded queue at the
	// retained transport's reviewed maximum so a normal Windows read pipeline
	// parks behind the one canonical owner instead of becoming a USB error.
	dormantRetainedUSBInterruptInQueueDepth = retainedusb.MaximumQueueDepth
)

var (
	errDormantRetainedUSBUninitialized = errors.New(
		"xboxone: dormant retained USB adapter is uninitialized")
	errDormantRetainedUSBInvalidRequest = errors.New(
		"xboxone: invalid dormant retained USB request")
	errDormantRetainedUSBInvalidTicket = errors.New(
		"xboxone: invalid dormant retained USB ticket")
	errDormantRetainedUSBLaneBusy = errors.New(
		"xboxone: dormant retained USB lane is busy")
	errDormantRetainedUSBTokenExhausted = errors.New(
		"xboxone: dormant retained USB token exhausted")
	errDormantRetainedUSBReadinessExhausted = errors.New(
		"xboxone: dormant retained USB readiness epoch exhausted")
	errDormantRetainedUSBInvalidPreparation = errors.New(
		"xboxone: invalid dormant retained USB preparation")
	errDormantRetainedUSBInvalidImport = errors.New(
		"xboxone: invalid dormant retained USB import capability")
	errDormantRetainedUSBDeadline = errors.New(
		"xboxone: dormant retained USB lifecycle deadline expired")
	errDormantRetainedUSBQuarantined = errors.New(
		"xboxone: dormant retained USB adapter is quarantined")
	errDormantRetainedUSBLocalPanic = errors.New(
		"xboxone: dormant retained USB local executor panicked")
)

var dormantRetainedUSBIdentityAuthority atomic.Uint64

// ControllerPersonaLocalExecution is the immutable typed action selected by
// the one canonical ControllerPersonaEngine and transferred to the sole local
// executor. DirectMotor carries all four Xbox actuator percentages, including
// both impulse triggers. No wire parsing or controller mapping occurs here.
type ControllerPersonaLocalExecution struct {
	Action      ControllerPersonaAction
	Generation  uint64
	Order       uint64
	ClearEpoch  uint64
	DirectMotor RumbleBodyV1
	GuideLED    GuideLEDCommandV1
}

func (execution ControllerPersonaLocalExecution) valid() bool {
	if execution.Action < ControllerPersonaUSBControl ||
		execution.Action > ControllerPersonaPerformReset ||
		execution.Generation == 0 || execution.Order == 0 {
		return false
	}
	switch execution.Action {
	case ControllerPersonaApplyDirectMotor:
		return execution.ClearEpoch == 0 &&
			execution.GuideLED == (GuideLEDCommandV1{}) &&
			execution.DirectMotor.Validate() == nil
	case ControllerPersonaApplyGuideLED:
		return execution.ClearEpoch == 0 &&
			execution.DirectMotor == (RumbleBodyV1{}) &&
			execution.GuideLED.Validate() == nil
	case ControllerPersonaClearOutputs:
		return execution.ClearEpoch != 0 &&
			execution.DirectMotor == (RumbleBodyV1{}) &&
			execution.GuideLED == (GuideLEDCommandV1{})
	default:
		return execution.ClearEpoch == 0 &&
			execution.DirectMotor == (RumbleBodyV1{}) &&
			execution.GuideLED == (GuideLEDCommandV1{})
	}
}

// ControllerPersonaLocalExecutor is a synchronous, cancellation-aware effect
// boundary. Execute must return only when the action's acceptance is terminal.
// Successful Direct Motor execution may transfer a finite program to this
// exact executor, whose bounded canonical leases remain cancellable by later
// commands. Failed execution must not schedule later effects. ResetAndDrain
// joins both host calls and program publications and invalidates future
// renewals before returning. It reversibly rejects later Execute calls and
// returns only after every earlier Execute is joined. ResetNeutral is valid
// only after that drain: success terminally delivers the exact reset clear and
// reopens executor admission; failure must leave executor admission fenced.
// CancelAndDrain is the separate terminal operation: it permanently stops
// ordinary execution for the current import and joins every earlier Execute.
// DisconnectNeutral remains available after the terminal drain for the one
// separately authorized disconnect clear. These methods may perform I/O; none
// is called from Stage, Prepare, Complete, Retire, or while the USB/IP response
// serializer is owned.
type ControllerPersonaLocalExecutor interface {
	Execute(ControllerPersonaLocalExecution, time.Time) error
	ResetAndDrain(time.Time) error
	ResetNeutral(ControllerPersonaLocalExecution, time.Time) error
	CancelAndDrain(time.Time) error
	DisconnectNeutral(ControllerPersonaLocalExecution, time.Time) error
}

// dormantRetainedUSBConstructionAuthority is an exact package-private request
// token. The exported direct constructor never receives it and therefore never
// mints or retains composition authority.
type dormantRetainedUSBConstructionAuthority struct {
	marker byte
}

var authorizedRetainedUSBConstructionAuthority = &dormantRetainedUSBConstructionAuthority{marker: 1}

// dormantRetainedUSBConstructionProof is an unpublished, one-shot issuance
// returned separately only when the exact private authority requests it. It
// lets the higher-level dormant authorized composition boundary prove that an
// adapter came from the canonical implementation without retaining a
// self-referential proof on every direct adapter.
type dormantRetainedUSBConstructionProof struct {
	adapter           *DormantRetainedUSBAdapter
	engine            *ControllerPersonaEngine
	coordinator       *controllerPersonaTransportCoordinator
	local             ControllerPersonaLocalExecutor
	expectedAuthority uint64
	expectedDevice    uint64
	protocolTimeMS    uint64
	localTimeout      time.Duration
	consumed          bool
}

// DormantRetainedUSBAdapter binds the retained three-lane USB/IP value
// contract to one existing controllerPersonaTransportCoordinator. It is
// deliberately absent from the device registry and server routing. Its
// exported shape permits offline composition tests without claiming a lawful
// USB identity, Windows binding, transfer reassembly, replay policy, latency,
// or hardware conformance.
type DormantRetainedUSBAdapter struct {
	mu sync.Mutex
	// protocolMu serializes timestamp normalization with every ordinary
	// coordinator call that can observe the canonical engine clock. USB/IP
	// response preparation and the local feedback worker are independent
	// schedulers: either can sample time first and reach the coordinator second.
	// Clamping without this call fence still permits a later local completion to
	// overtake an earlier sampled host request by one millisecond.
	protocolMu sync.Mutex
	// localAdmission is a deadline-selectable single token. The worker holds it
	// from its final state check through Execute. CancelAndDrain cancels the
	// executor, then acquires this token before reporting Drained.
	localAdmission chan struct{}
	localDrainOnce sync.Once
	localDrainDone chan struct{}
	localDrainErr  error
	// quarantineContainmentErr records the first bounded containment failure.
	// It is diagnostic only: even a later successful join cannot turn a
	// quarantined import into Drained or authorize neutral/release.
	quarantineContainmentErr error
	quarantineContained      bool

	identity          uint64
	expectedAuthority uint64
	expectedDevice    uint64
	session           uint64
	lastSession       uint64
	generation        uint64
	nextTicket        uint64
	protocolTimeMS    uint64
	clockOrigin       time.Time
	clockOriginMS     uint64

	coordinator  *controllerPersonaTransportCoordinator
	local        ControllerPersonaLocalExecutor
	localTimeout time.Duration

	readinessEpoch uint64
	readiness      chan struct{}
	localWake      chan struct{}
	localStop      chan struct{}
	localDone      chan struct{}
	localStopOnce  sync.Once
	localNow       time.Time

	input             GamepadInputReportV1
	inputRevision     uint64
	inputHistoryFault bool // terminal input-only fence; never cleared by START/reset
	guideEdges        [GuideButtonEdgeQueueCapacity]GuideButtonStatusV1
	guideEdgeHead     uint16
	guideEdgeCount    uint16

	slots [3]dormantRetainedUSBSlot

	state           dormantRetainedUSBState
	boundLease      retainedusb.ImportLease
	activeReset     retainedusb.ImportResetLease
	lastReset       retainedusb.ImportResetLease
	lastResetResult retainedusb.ImportResetResult
	closeReason     retainedusb.ImportCloseReason
	lastLocalError  error
	fatalLocalError error
	quarantine      error
}

// GuideButtonEdgeQueueCapacity retains over a quarter second of adversarial
// 500 Hz alternating Guide transitions without allocation. Saturation rejects
// the publication and revision so the caller can retry; it never overwrites an
// older edge.
const GuideButtonEdgeQueueCapacity = 128

type dormantRetainedUSBState uint8

const (
	dormantRetainedUSBUnbound dormantRetainedUSBState = iota + 1
	dormantRetainedUSBBound
	dormantRetainedUSBResetDraining
	dormantRetainedUSBResetDrained
	dormantRetainedUSBResetNeutralizing
	dormantRetainedUSBDraining
	dormantRetainedUSBDrained
	dormantRetainedUSBDisconnecting
	dormantRetainedUSBDisconnected
	dormantRetainedUSBQuarantine
)

type dormantRetainedUSBSlotState uint8

const (
	dormantRetainedUSBSlotEmpty dormantRetainedUSBSlotState = iota
	dormantRetainedUSBSlotStaged
	dormantRetainedUSBSlotPreparing
	dormantRetainedUSBSlotAdmitted
)

type dormantRetainedUSBCompletionKind uint8

const (
	dormantRetainedUSBCompletionCoordinator dormantRetainedUSBCompletionKind = iota + 1
	dormantRetainedUSBCompletionStall
)

type dormantRetainedUSBSlot struct {
	state          dormantRetainedUSBSlotState
	ticket         retainedusb.Ticket
	coordinator    controllerPersonaTransportTicket
	completion     dormantRetainedUSBCompletionKind
	transferLength uint32
}

// NewDormantRetainedUSBAdapter transfers exclusive engine ownership to a
// retained adapter. The exact USB/IP session generation is deliberately not
// predicted at construction: it is adopted only from the authority-issued
// capability in BindImport. The engine's current semantic input seeds revision
// one; later reports must be published as exact successor revisions.
// Construction starts no goroutine and performs no controller I/O.
func NewDormantRetainedUSBAdapter(
	engine *ControllerPersonaEngine,
	expectedAuthorityID uint64,
	expectedDeviceID uint64,
	local ControllerPersonaLocalExecutor,
	localTimeout time.Duration,
) (*DormantRetainedUSBAdapter, error) {
	adapter, _, err := newDormantRetainedUSBAdapter(
		engine, expectedAuthorityID, expectedDeviceID, local, localTimeout, nil)
	return adapter, err
}

// newDormantRetainedUSBAdapter is the single canonical implementation. Only
// the dormant authorized composition boundary supplies the exact private
// authority and receives a separately returned one-shot proof.
func newDormantRetainedUSBAdapter(
	engine *ControllerPersonaEngine,
	expectedAuthorityID uint64,
	expectedDeviceID uint64,
	local ControllerPersonaLocalExecutor,
	localTimeout time.Duration,
	authority *dormantRetainedUSBConstructionAuthority,
) (*DormantRetainedUSBAdapter, *dormantRetainedUSBConstructionProof, error) {
	if engine == nil || expectedAuthorityID == 0 || expectedDeviceID == 0 ||
		local == nil ||
		localTimeout < dormantRetainedUSBMinimumLocalTimeout ||
		localTimeout > dormantRetainedUSBMaximumLocalTimeout {
		return nil, nil, errDormantRetainedUSBUninitialized
	}
	if err := engine.metadata.validateInputReport(engine.currentInput); err != nil {
		return nil, nil, err
	}
	coordinator, err := newControllerPersonaTransportCoordinator(engine)
	if err != nil {
		return nil, nil, err
	}
	coordinator.inputJournal, err = newRetainedInputJournal(engine.currentInput)
	if err != nil {
		return nil, nil, err
	}
	identity, err := allocateDormantRetainedUSBIdentity()
	if err != nil {
		return nil, nil, err
	}
	adapter := &DormantRetainedUSBAdapter{
		identity: identity, generation: 1, protocolTimeMS: engine.lastNowMS,
		expectedAuthority: expectedAuthorityID, expectedDevice: expectedDeviceID,
		clockOrigin: time.Now(), clockOriginMS: engine.lastNowMS,
		coordinator: coordinator, local: local, localTimeout: localTimeout,
		readinessEpoch: 1, readiness: make(chan struct{}, 1),
		localAdmission: make(chan struct{}, 1), localDrainDone: make(chan struct{}),
		input: engine.currentInput, inputRevision: 1,
		state: dormantRetainedUSBUnbound,
	}
	adapter.localAdmission <- struct{}{}
	if authority == nil ||
		authority != authorizedRetainedUSBConstructionAuthority ||
		authority.marker != 1 ||
		!validAuthorizedRetainedLocalExecutor(local) {
		return adapter, nil, nil
	}
	proof := &dormantRetainedUSBConstructionProof{
		adapter: adapter, engine: engine, coordinator: coordinator, local: local,
		expectedAuthority: expectedAuthorityID, expectedDevice: expectedDeviceID,
		protocolTimeMS: engine.lastNowMS, localTimeout: localTimeout,
	}
	return adapter, proof, nil
}

func allocateDormantRetainedUSBIdentity() (uint64, error) {
	for {
		current := dormantRetainedUSBIdentityAuthority.Load()
		if current == ^uint64(0) {
			return 0, errDormantRetainedUSBTokenExhausted
		}
		if dormantRetainedUSBIdentityAuthority.CompareAndSwap(current, current+1) {
			return current + 1, nil
		}
	}
}

func (adapter *DormantRetainedUSBAdapter) startLocalWorkerLocked() {
	adapter.localWake = make(chan struct{}, 1)
	adapter.localStop = make(chan struct{})
	adapter.localDone = make(chan struct{})
	adapter.localStopOnce = sync.Once{}
	go adapter.runLocalWorker(adapter.localWake, adapter.localStop, adapter.localDone)
}

// Identity implements both retainedusb.Owner and
// retainedusb.ImportSessionOwner.
func (adapter *DormantRetainedUSBAdapter) Identity() uint64 {
	if adapter == nil {
		return 0
	}
	return adapter.identity
}

// Limits fixes one canonical owner ticket per lane and the exact GIP USB
// interface-zero/alternate-zero routes 01 OUT and 81 IN. The transport may
// retain a bounded Windows interrupt-IN request pipeline behind that sole
// ticket; IN envelopes carry no copied payload and therefore do not enlarge
// the request slab. Control OUT carries no data in the implemented table-4
// subset. The control-response maximum is the largest even length
// representable by a USB string descriptor's one-byte bLength; it does not
// change either 64-byte interrupt lane. These descriptor facts are offline
// source facts, not Windows binding evidence.
func (adapter *DormantRetainedUSBAdapter) Limits() retainedusb.Limits {
	return retainedusb.Limits{
		QueueDepth: [3]uint8{
			1, dormantRetainedUSBInterruptInQueueDepth, 1,
		},
		BufferedRequestBytes:   64,
		MaximumControlOut:      0,
		MaximumControlResponse: usbMaximumStringDescriptorSize,
		MaximumInterruptIn:     64, MaximumInterruptOut: 64,
		InterruptInRoute: retainedusb.Route{
			InterfaceNumber: 0, AlternateSetting: 0, EndpointAddress: 0x81,
		},
		InterruptOutRoute: retainedusb.Route{
			InterfaceNumber: 0, AlternateSetting: 0, EndpointAddress: 0x01,
		},
	}
}

// BindImport authenticates and stores the complete outer capability before
// the authority can publish a successful import. Later lifecycle callbacks
// compare every field with this exact retained value.
func (adapter *DormantRetainedUSBAdapter) BindImport(
	lease retainedusb.ImportLease,
	deadline time.Time,
) (retainedusb.ImportBindResult, error) {
	result := retainedusb.ImportBindResult{
		Lease: lease, State: retainedusb.ImportBindRejected,
	}
	if adapter == nil {
		return result, errDormantRetainedUSBUninitialized
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.state == dormantRetainedUSBQuarantine {
		result.State = retainedusb.ImportBindQuarantined
		return result, adapter.quarantineErrorLocked()
	}
	if adapter.state != dormantRetainedUSBUnbound || adapter.boundLease.Valid() {
		adapter.quarantineLocked(errDormantRetainedUSBInvalidImport)
		result.State = retainedusb.ImportBindQuarantined
		return result, errDormantRetainedUSBInvalidImport
	}
	if !lease.Valid() || lease.AuthorityID != adapter.expectedAuthority ||
		lease.DeviceID != adapter.expectedDevice ||
		lease.OwnerID != adapter.identity ||
		lease.SessionGeneration <= adapter.lastSession {
		return result, nil
	}
	if !deadlineValid(deadline) {
		adapter.quarantineLocked(errDormantRetainedUSBDeadline)
		result.State = retainedusb.ImportBindQuarantined
		return result, errDormantRetainedUSBDeadline
	}
	if err := adapter.coordinator.adoptRetainedUSBIPAddress(); err != nil {
		adapter.quarantineLocked(err)
		result.State = retainedusb.ImportBindQuarantined
		return result, err
	}
	adapter.boundLease = lease
	adapter.session = lease.SessionGeneration
	adapter.lastSession = lease.SessionGeneration
	adapter.state = dormantRetainedUSBBound
	adapter.startLocalWorkerLocked()
	result.State = retainedusb.ImportBindBound
	return result, nil
}

// PublishSemanticInput retains ordered buttons/trigger boundaries in the shared
// semantic scheduler. Continuous motion coalesces. It performs no GIP encoding;
// Prepare selects only after final response serialization, including START.
func (adapter *DormantRetainedUSBAdapter) PublishSemanticInput(
	sessionGeneration uint64,
	revision uint64,
	report GamepadInputReportV1,
) error {
	if adapter == nil {
		return errDormantRetainedUSBUninitialized
	}
	if adapter.coordinator == nil || adapter.coordinator.engine == nil {
		return errDormantRetainedUSBUninitialized
	}
	wireReport := report
	wireReport.State.Guide = false
	if err := adapter.coordinator.engine.metadata.validateInputReport(wireReport); err != nil {
		return err
	}
	adapter.mu.Lock()
	if adapter.inputHistoryFault {
		adapter.mu.Unlock()
		return errRetainedInputHistoryFault
	}
	if adapter.state != dormantRetainedUSBBound ||
		sessionGeneration != adapter.session || revision == 0 ||
		adapter.inputRevision == ^uint64(0) || revision != adapter.inputRevision+1 {
		adapter.mu.Unlock()
		return errDormantRetainedUSBInvalidRequest
	}
	guideChanged := report.State.Guide != adapter.input.State.Guide
	if guideChanged && adapter.guideEdgeCount == GuideButtonEdgeQueueCapacity {
		adapter.mu.Unlock()
		return ErrGuideButtonEdgeQueueFull
	}
	if err := adapter.coordinator.inputJournal.publish(wireReport); err != nil {
		if errors.Is(err, errRetainedInputHistoryFault) {
			adapter.inputHistoryFault = true
			readinessErr := adapter.advanceReadinessLocked()
			ready := adapter.readiness
			adapter.mu.Unlock()
			latchedSignal(ready)
			return errors.Join(err, readinessErr)
		}
		adapter.mu.Unlock()
		return err
	}
	if err := adapter.advanceReadinessLocked(); err != nil {
		adapter.mu.Unlock()
		return err
	}
	if guideChanged {
		adapter.enqueueGuideEdgeLocked(GuideButtonStatusV1{Down: report.State.Guide})
	}
	adapter.input = report
	adapter.inputRevision = revision
	ready := adapter.readiness
	adapter.mu.Unlock()
	latchedSignal(ready)
	return nil
}

// PublishSemanticInputWire is the exact dormant DS4Windows broker ingress.
// It reuses DecodeSemanticInputWireV1, exact generation/revision admission,
// and the shared semantic journal; no second mapping model is created.
// Share is accepted only when the exact engine
// metadata was issued by the strict official Console Function Map compiler.
// Guide transitions enter the adapter's bounded ordered Command 7 edge owner.
func (adapter *DormantRetainedUSBAdapter) PublishSemanticInputWire(
	sessionGeneration uint64,
	revision uint64,
	wire []byte,
) error {
	if adapter == nil {
		return errDormantRetainedUSBUninitialized
	}
	state, err := DecodeSemanticInputWireV1(wire)
	if err != nil {
		return err
	}
	return adapter.PublishSemanticInput(sessionGeneration, revision,
		GamepadInputReportV1{State: state})
}

// Stage implements retainedusb.Owner. It validates and copies only immutable
// request facts into the existing coordinator; it performs no semantic input
// sample, persona selection, callback, wait, or I/O.
func (adapter *DormantRetainedUSBAdapter) Stage(
	request retainedusb.Request,
) (retainedusb.Ticket, error) {
	if adapter == nil {
		return retainedusb.Ticket{}, errDormantRetainedUSBUninitialized
	}
	if err := adapter.validateRequest(request); err != nil {
		return retainedusb.Ticket{}, err
	}
	index, _ := request.Lane.Index()
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.state != dormantRetainedUSBBound ||
		request.SessionGeneration != adapter.session ||
		request.BindingGeneration == 0 {
		return retainedusb.Ticket{}, errDormantRetainedUSBInvalidRequest
	}
	if adapter.slots[index].state != dormantRetainedUSBSlotEmpty {
		return retainedusb.Ticket{}, errDormantRetainedUSBLaneBusy
	}
	if adapter.nextTicket == ^uint64(0) {
		return retainedusb.Ticket{}, errDormantRetainedUSBTokenExhausted
	}
	coordinatorTicket, err := adapter.stageCoordinatorLocked(request)
	if err != nil {
		return retainedusb.Ticket{}, err
	}
	adapter.nextTicket++
	ticket := retainedusb.Ticket{
		OwnerID: adapter.identity, Token: adapter.nextTicket,
		Generation:        adapter.generation,
		SessionGeneration: adapter.session, Lane: request.Lane,
	}
	adapter.slots[index] = dormantRetainedUSBSlot{
		state: dormantRetainedUSBSlotStaged, ticket: ticket,
		coordinator: coordinatorTicket, transferLength: request.TransferLength,
	}
	return ticket, nil
}

func (adapter *DormantRetainedUSBAdapter) validateRequest(
	request retainedusb.Request,
) error {
	if !request.Lane.Valid() || !request.Direction.Valid() ||
		request.SessionGeneration == 0 || request.BindingGeneration == 0 ||
		request.IngressOrdinal == 0 || cap(request.Data) != len(request.Data) {
		return errDormantRetainedUSBInvalidRequest
	}
	limits := adapter.Limits()
	switch request.Lane {
	case retainedusb.LaneControl:
		setupIn := request.Setup[0]&0x80 != 0
		setupLength := uint32(request.Setup[6]) |
			uint32(request.Setup[7])<<8
		if request.Route != (retainedusb.Route{}) || len(request.Data) != 0 ||
			setupIn != (request.Direction == retainedusb.DirectionIn) ||
			setupLength != request.TransferLength ||
			(request.Direction == retainedusb.DirectionOut &&
				request.TransferLength != 0) {
			return errDormantRetainedUSBInvalidRequest
		}
	case retainedusb.LaneInterruptIn:
		if request.Direction != retainedusb.DirectionIn ||
			request.Route != limits.InterruptInRoute ||
			request.Setup != ([8]byte{}) || len(request.Data) != 0 ||
			request.TransferLength == 0 ||
			request.TransferLength > limits.MaximumInterruptIn {
			return errDormantRetainedUSBInvalidRequest
		}
	case retainedusb.LaneInterruptOut:
		if request.Direction != retainedusb.DirectionOut ||
			request.Route != limits.InterruptOutRoute ||
			request.Setup != ([8]byte{}) ||
			request.TransferLength == 0 ||
			request.TransferLength > limits.MaximumInterruptOut ||
			uint64(len(request.Data)) != uint64(request.TransferLength) {
			return errDormantRetainedUSBInvalidRequest
		}
	default:
		return errDormantRetainedUSBInvalidRequest
	}
	return nil
}

func (adapter *DormantRetainedUSBAdapter) stageCoordinatorLocked(
	request retainedusb.Request,
) (controllerPersonaTransportTicket, error) {
	switch request.Lane {
	case retainedusb.LaneControl:
		return adapter.coordinator.stageControl(request.Setup[:])
	case retainedusb.LaneInterruptIn:
		return adapter.coordinator.stageInterruptIn(int(request.TransferLength))
	case retainedusb.LaneInterruptOut:
		return adapter.coordinator.stageInterruptOut(request.Data)
	default:
		return controllerPersonaTransportTicket{}, errDormantRetainedUSBInvalidRequest
	}
}

// Prepare implements retainedusb.Owner. It is the only ordinary method which
// asks the coordinator to select persona work. For interrupt IN it copies the
// final semantic input revision only after this call has begun under the
// response serializer.
func (adapter *DormantRetainedUSBAdapter) Prepare(
	ticket retainedusb.Ticket,
	destination []byte,
	now time.Time,
) (retainedusb.Preparation, error) {
	if adapter == nil {
		return retainedusb.Preparation{}, errDormantRetainedUSBUninitialized
	}
	nowMS, err := adapter.beginProtocolCall(now, 0)
	if err != nil {
		return retainedusb.Preparation{}, err
	}
	defer adapter.protocolMu.Unlock()
	index, ok := ticket.Lane.Index()
	if !ok {
		return retainedusb.Preparation{}, errDormantRetainedUSBInvalidTicket
	}
	adapter.mu.Lock()
	slot := &adapter.slots[index]
	if adapter.state != dormantRetainedUSBBound ||
		adapter.fatalLocalError != nil ||
		!adapter.exactTicketLocked(ticket, slot) ||
		slot.state != dormantRetainedUSBSlotStaged {
		adapter.mu.Unlock()
		return retainedusb.Preparation{}, errDormantRetainedUSBInvalidTicket
	}
	slot.state = dormantRetainedUSBSlotPreparing
	selectionReadinessEpoch := adapter.readinessEpoch
	if adapter.inputHistoryFault {
		slot.state = dormantRetainedUSBSlotStaged
		adapter.mu.Unlock()
		return retainedusb.Preparation{
			Result: retainedusb.ResultPending, ReadinessEpoch: selectionReadinessEpoch,
		}, nil
	}
	coordinatorTicket := slot.coordinator
	transferLength := slot.transferLength
	input := adapter.input
	input.State.Guide = false
	guide, guidePresent := adapter.peekGuideEdgeLocked()
	adapter.mu.Unlock()

	var admission controllerPersonaTransportAdmission
	if ticket.Lane == retainedusb.LaneInterruptIn {
		if err := adapter.coordinator.inputJournal.failure(); err != nil {
			adapter.restoreStaged(ticket, index)
			return retainedusb.Preparation{
				Result: retainedusb.ResultPending, ReadinessEpoch: selectionReadinessEpoch,
			}, nil
		}
		if guidePresent {
			admission, err = adapter.coordinator.admitInputWithGuide(
				coordinatorTicket, destination, nowMS, input, &guide)
		} else {
			admission, err = adapter.coordinator.admitInput(
				coordinatorTicket, destination, nowMS, input)
		}
	} else {
		admission, err = adapter.coordinator.admit(
			coordinatorTicket, destination, nowMS)
	}
	if err != nil && ticket.Lane == retainedusb.LaneControl &&
		errors.Is(err, ErrUSBControlRequestStalled) {
		adapter.mu.Lock()
		if !adapter.exactPreparingLocked(ticket, index) {
			adapter.mu.Unlock()
			return retainedusb.Preparation{}, errDormantRetainedUSBInvalidTicket
		}
		adapter.slots[index].state = dormantRetainedUSBSlotAdmitted
		adapter.slots[index].completion = dormantRetainedUSBCompletionStall
		adapter.mu.Unlock()
		return retainedusb.Preparation{Result: retainedusb.ResultStall}, nil
	}
	if err != nil {
		adapter.restoreStaged(ticket, index)
		if ticket.Lane == retainedusb.LaneInterruptIn && errors.Is(err, errRetainedInputHistoryFault) {
			// Publication can fault between the initial check and journal
			// selection/admission. The exact retirement query owns shutdown;
			// Pending neither copies bytes nor converts the history diagnostic
			// into an invariant failure that would prohibit terminal Stop.
			return retainedusb.Preparation{
				Result: retainedusb.ResultPending, ReadinessEpoch: selectionReadinessEpoch,
			}, nil
		}
		return retainedusb.Preparation{}, err
	}

	preparation, terminal, localRequired, err :=
		adapter.translateAdmission(ticket, admission, transferLength, now)
	if err != nil {
		adapter.restoreStaged(ticket, index)
		return retainedusb.Preparation{}, err
	}
	adapter.mu.Lock()
	if !adapter.exactPreparingLocked(ticket, index) {
		adapter.mu.Unlock()
		return retainedusb.Preparation{}, errDormantRetainedUSBInvalidTicket
	}
	if admission.consumesGuideEdge {
		current, present := adapter.peekGuideEdgeLocked()
		if !guidePresent || !present || current != guide {
			adapter.quarantineLocked(ErrControllerPersonaInvariantViolation)
			adapter.mu.Unlock()
			return retainedusb.Preparation{}, ErrControllerPersonaInvariantViolation
		}
		adapter.popGuideEdgeLocked()
	}
	if terminal {
		adapter.slots[index].state = dormantRetainedUSBSlotAdmitted
		adapter.slots[index].completion = dormantRetainedUSBCompletionCoordinator
	} else {
		adapter.slots[index].state = dormantRetainedUSBSlotStaged
		// A publication after the semantic decision must remain newer than
		// this Pending observation, even if it beats us back to adapter.mu.
		preparation.ReadinessEpoch = selectionReadinessEpoch
	}
	if nowMS > adapter.protocolTimeMS {
		adapter.protocolTimeMS = nowMS
	}
	if localRequired {
		adapter.localNow = now
	}
	localWake := adapter.localWake
	adapter.mu.Unlock()
	if localRequired {
		latchedSignal(localWake)
	}
	return preparation, nil
}

func (adapter *DormantRetainedUSBAdapter) enqueueGuideEdgeLocked(
	status GuideButtonStatusV1,
) {
	index := (uint32(adapter.guideEdgeHead) + uint32(adapter.guideEdgeCount)) %
		GuideButtonEdgeQueueCapacity
	adapter.guideEdges[index] = status
	adapter.guideEdgeCount++
}

func (adapter *DormantRetainedUSBAdapter) peekGuideEdgeLocked() (
	GuideButtonStatusV1,
	bool,
) {
	if adapter.guideEdgeCount == 0 {
		return GuideButtonStatusV1{}, false
	}
	return adapter.guideEdges[adapter.guideEdgeHead], true
}

func (adapter *DormantRetainedUSBAdapter) popGuideEdgeLocked() {
	if adapter.guideEdgeCount == 0 {
		return
	}
	adapter.guideEdges[adapter.guideEdgeHead] = GuideButtonStatusV1{}
	adapter.guideEdgeHead = (adapter.guideEdgeHead + 1) % GuideButtonEdgeQueueCapacity
	adapter.guideEdgeCount--
}

func (adapter *DormantRetainedUSBAdapter) normalizeGuideEdgesLocked() {
	for index := range adapter.guideEdges {
		adapter.guideEdges[index] = GuideButtonStatusV1{}
	}
	adapter.guideEdgeHead = 0
	adapter.guideEdgeCount = 0
	if adapter.input.State.Guide {
		adapter.enqueueGuideEdgeLocked(GuideButtonStatusV1{Down: true})
	}
}

func (adapter *DormantRetainedUSBAdapter) translateAdmission(
	ticket retainedusb.Ticket,
	admission controllerPersonaTransportAdmission,
	transferLength uint32,
	now time.Time,
) (retainedusb.Preparation, bool, bool, error) {
	switch admission.disposition {
	case controllerPersonaTransportWait:
		if admission.waitReason == controllerPersonaTransportInputRequired &&
			ticket.Lane == retainedusb.LaneInterruptOut &&
			admission.action == ControllerPersonaSendInitialInput {
			// A host OUT may arrive between START's status and initial IN.
			// Retain that exact OUT without selecting input or acknowledging
			// its command. Initial IN completion advances readiness and wakes
			// this lane; no periodic retry or fabricated neutral is necessary.
			return retainedusb.Preparation{Result: retainedusb.ResultPending}, false, false, nil
		}
		if admission.waitReason == controllerPersonaTransportInputUnchanged {
			idle := retainedusb.Preparation{Result: retainedusb.ResultPending}
			if admission.retryAfterMS > controllerPersonaStatusSteadyIntervalMS {
				return retainedusb.Preparation{}, false, false, invalidRetainedPreparation(ticket, admission, transferLength)
			}
			if admission.retryAfterMS != 0 {
				idle.RetryAt = now.Add(time.Duration(admission.retryAfterMS) * time.Millisecond)
			}
			return idle, false, false, nil
		}
		if admission.waitReason != controllerPersonaTransportNoPresentation &&
			admission.waitReason != controllerPersonaTransportUpstreamPending &&
			admission.waitReason != controllerPersonaTransportLocalPending {
			return retainedusb.Preparation{}, false, false,
				invalidRetainedPreparation(ticket, admission, transferLength)
		}
		return retainedusb.Preparation{
			Result:  retainedusb.ResultPending,
			RetryAt: now.Add(dormantRetainedUSBPendingRetry),
		}, false, false, nil
	case controllerPersonaTransportLocalRequired:
		return retainedusb.Preparation{
			Result:  retainedusb.ResultPending,
			RetryAt: now.Add(dormantRetainedUSBPendingRetry),
		}, false, true, nil
	case controllerPersonaTransportControlResponse:
		if ticket.Lane != retainedusb.LaneControl || admission.size < 0 ||
			uint64(admission.size) > uint64(transferLength) {
			return retainedusb.Preparation{}, false, false,
				invalidRetainedPreparation(ticket, admission, transferLength)
		}
		if admission.size == 0 {
			return retainedusb.Preparation{Result: retainedusb.ResultSuccess},
				true, false, nil
		}
		return retainedusb.Preparation{
			Result:       retainedusb.ResultData,
			ActualLength: uint32(admission.size),
		}, true, false, nil
	case controllerPersonaTransportInterruptInResponse:
		if ticket.Lane != retainedusb.LaneInterruptIn || admission.size <= 0 ||
			uint64(admission.size) > uint64(transferLength) {
			return retainedusb.Preparation{}, false, false,
				invalidRetainedPreparation(ticket, admission, transferLength)
		}
		return retainedusb.Preparation{
			Result:       retainedusb.ResultData,
			ActualLength: uint32(admission.size),
		}, true, false, nil
	case controllerPersonaTransportInterruptOutResponse:
		if ticket.Lane != retainedusb.LaneInterruptOut {
			return retainedusb.Preparation{}, false, false,
				invalidRetainedPreparation(ticket, admission, transferLength)
		}
		return retainedusb.Preparation{
			Result:       retainedusb.ResultSuccess,
			ActualLength: transferLength,
		}, true, false, nil
	default:
		return retainedusb.Preparation{}, false, false,
			invalidRetainedPreparation(ticket, admission, transferLength)
	}
}

// Error-only facts identify the rejected internal transition without packet
// dumps, controller identifiers, per-report logging, or weakening admission.
func invalidRetainedPreparation(ticket retainedusb.Ticket, admission controllerPersonaTransportAdmission, transferLength uint32) error {
	return fmt.Errorf("%w: lane=%d disposition=%d wait=%d size=%d host_length=%d retry_ms=%d",
		errDormantRetainedUSBInvalidPreparation, ticket.Lane, admission.disposition,
		admission.waitReason, admission.size, transferLength, admission.retryAfterMS)
}

func (adapter *DormantRetainedUSBAdapter) restoreStaged(
	ticket retainedusb.Ticket,
	index int,
) {
	adapter.mu.Lock()
	if adapter.exactPreparingLocked(ticket, index) {
		adapter.slots[index].state = dormantRetainedUSBSlotStaged
	}
	adapter.mu.Unlock()
}

func (adapter *DormantRetainedUSBAdapter) exactPreparingLocked(
	ticket retainedusb.Ticket,
	index int,
) bool {
	return index >= 0 && index < len(adapter.slots) &&
		adapter.exactTicketLocked(ticket, &adapter.slots[index]) &&
		adapter.slots[index].state == dormantRetainedUSBSlotPreparing
}

// Complete resolves one and only one terminal retained response. Delivery
// includes the USB/IP writer's mandatory flush. The coordinator alone applies
// delivered-only persona effects and exact immutable retry/fencing policy.
func (adapter *DormantRetainedUSBAdapter) Complete(
	ticket retainedusb.Ticket,
	delivered bool,
	now time.Time,
) error {
	if adapter == nil {
		return errDormantRetainedUSBUninitialized
	}
	nowMS, err := adapter.beginProtocolCall(now, 0)
	if err != nil {
		return err
	}
	defer adapter.protocolMu.Unlock()
	index, ok := ticket.Lane.Index()
	if !ok {
		return errDormantRetainedUSBInvalidTicket
	}
	adapter.mu.Lock()
	slot := adapter.slots[index]
	if !adapter.exactTicketLocked(ticket, &adapter.slots[index]) ||
		slot.state != dormantRetainedUSBSlotAdmitted {
		adapter.mu.Unlock()
		return errDormantRetainedUSBInvalidTicket
	}
	adapter.slots[index].state = dormantRetainedUSBSlotPreparing
	adapter.mu.Unlock()

	var completionErr error
	switch slot.completion {
	case dormantRetainedUSBCompletionStall:
		completionErr = adapter.coordinator.retire(slot.coordinator)
	case dormantRetainedUSBCompletionCoordinator:
		completionErr = adapter.coordinator.completeResponse(
			slot.coordinator, delivered, nowMS)
	default:
		completionErr = errDormantRetainedUSBInvalidPreparation
	}
	localRequired := completionErr == nil && delivered &&
		(ticket.Lane == retainedusb.LaneControl ||
			(ticket.Lane == retainedusb.LaneInterruptOut && adapter.coordinator.pendingOrdinaryFeedback())) &&
		adapter.coordinator.pendingLocal()

	adapter.mu.Lock()
	if completionErr != nil {
		// The coordinator deliberately retains its active response when canonical
		// Resolve fails. Retain the matching outer slot as well and quarantine the
		// entire import: clearing only this ledger would make the lane appear
		// reusable while the canonical engine still owns the exact effect.
		if !adapter.exactPreparingLocked(ticket, index) {
			completionErr = errors.Join(
				completionErr, errDormantRetainedUSBInvalidTicket)
		}
		adapter.quarantineLocked(completionErr)
	} else if adapter.exactPreparingLocked(ticket, index) {
		adapter.slots[index] = dormantRetainedUSBSlot{}
	} else {
		completionErr = errDormantRetainedUSBInvalidTicket
		adapter.quarantineLocked(completionErr)
	}
	readinessErr := adapter.advanceReadinessLocked()
	if nowMS > adapter.protocolTimeMS {
		adapter.protocolTimeMS = nowMS
	}
	if localRequired {
		adapter.localNow = now
	}
	ready := adapter.readiness
	localWake := adapter.localWake
	adapter.mu.Unlock()
	latchedSignal(ready)
	if localRequired {
		latchedSignal(localWake)
	}
	return errors.Join(completionErr, readinessErr)
}

// Retire relinquishes one copied, unadmitted ticket. It never resolves a
// persona claim because Stage owns none and Pending owns only a coordinator
// selection which remains engine-managed for the next eligible lane.
func (adapter *DormantRetainedUSBAdapter) Retire(
	ticket retainedusb.Ticket,
	reason retainedusb.RetireReason,
	_ time.Time,
) error {
	if adapter == nil {
		return errDormantRetainedUSBUninitialized
	}
	if !reason.Valid() {
		return errDormantRetainedUSBInvalidRequest
	}
	index, ok := ticket.Lane.Index()
	if !ok {
		return errDormantRetainedUSBInvalidTicket
	}
	adapter.mu.Lock()
	if !adapter.exactTicketLocked(ticket, &adapter.slots[index]) ||
		adapter.slots[index].state != dormantRetainedUSBSlotStaged {
		adapter.mu.Unlock()
		return errDormantRetainedUSBInvalidTicket
	}
	adapter.slots[index].state = dormantRetainedUSBSlotPreparing
	coordinatorTicket := adapter.slots[index].coordinator
	adapter.mu.Unlock()
	err := adapter.coordinator.retire(coordinatorTicket)
	adapter.mu.Lock()
	if err != nil {
		// A failed coordinator retirement means its stage may still own the
		// ticket. Preserve the adapter slot and quarantine instead of exposing a
		// split ownership ledger to a later Stage or successor import.
		if !adapter.exactPreparingLocked(ticket, index) {
			err = errors.Join(err, errDormantRetainedUSBInvalidTicket)
		}
		adapter.quarantineLocked(err)
	} else if adapter.exactPreparingLocked(ticket, index) {
		adapter.slots[index] = dormantRetainedUSBSlot{}
	} else {
		err = errDormantRetainedUSBInvalidTicket
		adapter.quarantineLocked(err)
	}
	readinessErr := adapter.advanceReadinessLocked()
	ready := adapter.readiness
	adapter.mu.Unlock()
	latchedSignal(ready)
	return errors.Join(err, readinessErr)
}

// Readiness returns the owner-lifetime-stable latched channel and its
// monotonically increasing, never-wrapping epoch.
func (adapter *DormantRetainedUSBAdapter) Readiness() (
	uint64,
	<-chan struct{},
) {
	if adapter == nil {
		return ^uint64(0), nil
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.readinessEpoch, adapter.readiness
}

func (adapter *DormantRetainedUSBAdapter) exactTicketLocked(
	ticket retainedusb.Ticket,
	slot *dormantRetainedUSBSlot,
) bool {
	return ticket.Valid() && ticket.OwnerID == adapter.identity &&
		ticket.Generation == adapter.generation &&
		ticket.SessionGeneration == adapter.session && slot.ticket == ticket
}

func (adapter *DormantRetainedUSBAdapter) advanceReadinessLocked() error {
	if adapter.readinessEpoch >= ^uint64(0)-1 {
		adapter.quarantineLocked(errDormantRetainedUSBReadinessExhausted)
		return errDormantRetainedUSBReadinessExhausted
	}
	adapter.readinessEpoch++
	return nil
}

func latchedSignal(channel chan struct{}) {
	if channel == nil {
		return
	}
	select {
	case channel <- struct{}{}:
	default:
	}
}

func (adapter *DormantRetainedUSBAdapter) milliseconds(
	now time.Time,
) (uint64, error) {
	if adapter == nil || now.IsZero() || adapter.clockOrigin.IsZero() {
		return 0, errDormantRetainedUSBInvalidRequest
	}
	delta := now.Sub(adapter.clockOrigin)
	if delta < 0 {
		return 0, errDormantRetainedUSBInvalidRequest
	}
	deltaMS := uint64(delta / time.Millisecond)
	if deltaMS > ^uint64(0)-adapter.clockOriginMS {
		return 0, errDormantRetainedUSBTokenExhausted
	}
	return adapter.clockOriginMS + deltaMS, nil
}

// beginProtocolCall owns protocolMu on success. The caller must release it
// only after its timed coordinator call and matching adapter bookkeeping are
// complete. minimumMS preserves the selected time of an admitted local action.
func (adapter *DormantRetainedUSBAdapter) beginProtocolCall(
	now time.Time,
	minimumMS uint64,
) (uint64, error) {
	if adapter == nil {
		return 0, errDormantRetainedUSBUninitialized
	}
	adapter.protocolMu.Lock()
	nowMS, err := adapter.milliseconds(now)
	if err != nil {
		adapter.protocolMu.Unlock()
		return 0, err
	}
	if nowMS < minimumMS {
		nowMS = minimumMS
	}
	adapter.mu.Lock()
	if nowMS < adapter.protocolTimeMS {
		nowMS = adapter.protocolTimeMS
	}
	if nowMS > adapter.protocolTimeMS {
		adapter.protocolTimeMS = nowMS
	}
	adapter.mu.Unlock()
	return nowMS, nil
}

func (adapter *DormantRetainedUSBAdapter) admitLocalAt(
	now time.Time,
) (controllerPersonaLocalLease, bool, uint64, error) {
	nowMS, err := adapter.beginProtocolCall(now, 0)
	if err != nil {
		return controllerPersonaLocalLease{}, false, 0, err
	}
	defer adapter.protocolMu.Unlock()
	lease, present, err := adapter.coordinator.admitLocal(nowMS)
	if err == nil && present && isOrdinaryFeedbackAction(lease.action) {
		// Detachment makes IN eligible before Execute/consumer acknowledgement.
		// Wake a request parked behind the former primary local claim now, not
		// only when the external feedback action eventually completes.
		adapter.mu.Lock()
		readinessErr := adapter.advanceReadinessLocked()
		ready := adapter.readiness
		adapter.mu.Unlock()
		latchedSignal(ready)
		if readinessErr != nil {
			// No Execute has started. Retain an unadmitted exact retry while
			// the exhausted adapter is quarantined, not a stranded local lease.
			completionErr := adapter.coordinator.completeLocal(lease, ControllerPersonaDeferred, nowMS)
			return controllerPersonaLocalLease{}, false, nowMS, errors.Join(readinessErr, completionErr)
		}
	}
	return lease, present, nowMS, err
}

func (adapter *DormantRetainedUSBAdapter) completeLocalAt(
	lease controllerPersonaLocalLease,
	outcome ControllerPersonaOutcome,
	minimumMS uint64,
) (uint64, error) {
	completedMS, err := adapter.beginProtocolCall(time.Now(), minimumMS)
	if err != nil {
		return 0, err
	}
	defer adapter.protocolMu.Unlock()
	return completedMS,
		adapter.coordinator.completeLocal(lease, outcome, completedMS)
}

func deadlineValid(deadline time.Time) bool {
	return !deadline.IsZero() && deadline.After(time.Now())
}

func (adapter *DormantRetainedUSBAdapter) runLocalWorker(
	wake <-chan struct{},
	stop <-chan struct{},
	done chan<- struct{},
) {
	defer close(done)
	for {
		select {
		case <-stop:
			return
		case <-wake:
			adapter.runOneLocalActionSafely(stop)
		}
	}
}

func (adapter *DormantRetainedUSBAdapter) runOneLocalActionSafely(
	stop <-chan struct{},
) {
	defer func() {
		if recovered := recover(); recovered != nil {
			adapter.markLocalFatal(localPanicError(recovered))
		}
	}()
	adapter.runOneLocalAction(stop)
}

func (adapter *DormantRetainedUSBAdapter) runOneLocalAction(
	stop <-chan struct{},
) {
	adapter.mu.Lock()
	if adapter.state != dormantRetainedUSBBound {
		adapter.mu.Unlock()
		return
	}
	logicalNow := adapter.localNow
	local := adapter.local
	timeout := adapter.localTimeout
	adapter.mu.Unlock()
	select {
	case <-stop:
		return
	default:
	}
	lease, present, nowMS, err := adapter.admitLocalAt(logicalNow)
	if err != nil || !present {
		if err != nil {
			if adapter.coordinator.pendingLocalClear() {
				if errors.Is(err, ErrNonMonotonicControllerPersonaClock) {
					adapter.recordLocalProgress(err)
					adapter.rearmPendingClear(stop, 0)
				} else {
					adapter.markLocalFatal(err)
				}
			} else {
				adapter.recordLocalProgress(err)
			}
		} else if adapter.coordinator.pendingLocalClear() {
			adapter.markLocalFatal(errDormantRetainedUSBInvalidPreparation)
		}
		if adapter.coordinator.pendingOrdinaryFeedbackRetry() {
			if errors.Is(err, ErrNonMonotonicControllerPersonaClock) {
				adapter.rearmPendingLocalRetry(stop, 0)
			} else {
				// A detached retry does not contend with IN ownership. Any
				// other admission failure is an invariant/capability fault,
				// not a condition to spin on every retry interval.
				adapter.markLocalFatal(errors.Join(errDormantRetainedUSBInvalidPreparation, err))
			}
		}
		return
	}
	execution := localExecutionFromLease(lease)
	if !execution.valid() {
		_, _ = adapter.completeLocalAt(
			lease, ControllerPersonaDeliveryFailed, nowMS)
		adapter.recordLocalProgress(errDormantRetainedUSBInvalidPreparation)
		return
	}
	select {
	case <-stop:
		_, _ = adapter.completeLocalAt(
			lease, ControllerPersonaDeferred, nowMS)
		adapter.recordLocalProgress(nil)
		return
	case <-adapter.localAdmission:
	}
	defer func() { adapter.localAdmission <- struct{}{} }()
	adapter.mu.Lock()
	if adapter.state != dormantRetainedUSBBound ||
		adapter.fatalLocalError != nil {
		adapter.mu.Unlock()
		_, _ = adapter.completeLocalAt(
			lease, ControllerPersonaDeferred, nowMS)
		adapter.recordLocalProgress(nil)
		return
	}
	adapter.mu.Unlock()
	deadline := time.Now().Add(timeout)
	executeErr, panicked := invokeControllerPersonaLocalExecute(
		local, execution, deadline)
	outcome := ControllerPersonaDelivered
	if panicked {
		drainErr := adapter.drainLocalExecutor(local, deadline)
		if drainErr == nil && !time.Now().After(deadline) {
			outcome = ControllerPersonaExecutionCancelled
		} else {
			outcome = ControllerPersonaDeliveryFailed
			executeErr = errors.Join(executeErr, drainErr)
		}
	} else if executeErr != nil || time.Now().After(deadline) {
		outcome = ControllerPersonaDeliveryFailed
		if executeErr == nil {
			executeErr = errDormantRetainedUSBDeadline
		}
	}
	completionMS, completeErr := adapter.completeLocalAt(
		lease, outcome, nowMS)
	if panicked {
		adapter.markLocalFatal(errors.Join(executeErr, completeErr))
		return
	}
	if completeErr != nil {
		adapter.markLocalFatal(errors.Join(executeErr, completeErr))
		return
	}
	adapter.recordLocalProgress(executeErr)
	if outcome != ControllerPersonaDelivered &&
		(execution.Action == ControllerPersonaClearOutputs || isOrdinaryFeedbackAction(execution.Action)) {
		adapter.rearmPendingLocalRetry(stop, completionMS)
	}
}

// rearmPendingClear gives an exact failed clear retry its own bounded worker
// wake. It deliberately does not generalize the local worker into a selector:
// completeLocal has already authenticated and materialized only the immutable
// predecessor clear. No successor URB is needed, and a persistent executor
// failure is paced instead of becoming a tight retry loop.
func (adapter *DormantRetainedUSBAdapter) rearmPendingClear(
	stop <-chan struct{},
	minimumMS uint64,
) {
	if !adapter.coordinator.pendingLocalClear() {
		return
	}
	adapter.rearmPendingLocalRetry(stop, minimumMS)
}

// Reuse the bounded local-worker retry wake for an exact ordinary feedback
// retry as well as a clear. It never consumes an IN request or selects a new
// effect, and no timer is created on the successful input/feedback path.
func (adapter *DormantRetainedUSBAdapter) rearmPendingLocalRetry(
	stop <-chan struct{},
	minimumMS uint64,
) {
	if !adapter.coordinator.pendingLocalClear() && !adapter.coordinator.pendingOrdinaryFeedbackRetry() {
		return
	}
	timer := time.NewTimer(dormantRetainedUSBPendingRetry)
	defer timer.Stop()
	select {
	case <-stop:
		return
	case <-timer.C:
	}
	retryNow := time.Now()
	adapter.mu.Lock()
	if adapter.state != dormantRetainedUSBBound ||
		adapter.fatalLocalError != nil {
		adapter.mu.Unlock()
		return
	}
	if minimumMS < adapter.protocolTimeMS {
		minimumMS = adapter.protocolTimeMS
	}
	if minimumMS < adapter.clockOriginMS ||
		minimumMS-adapter.clockOriginMS > dormantRetainedUSBMaximumDurationMS {
		adapter.quarantineLocked(errDormantRetainedUSBTokenExhausted)
		adapter.mu.Unlock()
		return
	}
	minimumTime := adapter.clockOrigin.Add(
		time.Duration(minimumMS-adapter.clockOriginMS) * time.Millisecond)
	if retryNow.Before(minimumTime) {
		retryNow = minimumTime
	}
	adapter.localNow = retryNow
	wake := adapter.localWake
	adapter.mu.Unlock()
	latchedSignal(wake)
}

func invokeControllerPersonaLocalExecute(
	local ControllerPersonaLocalExecutor,
	execution ControllerPersonaLocalExecution,
	deadline time.Time,
) (err error, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = localPanicError(recovered)
			panicked = true
		}
	}()
	return local.Execute(execution, deadline), false
}

func invokeControllerPersonaLocalDrain(
	local ControllerPersonaLocalExecutor,
	deadline time.Time,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = localPanicError(recovered)
		}
	}()
	return local.CancelAndDrain(deadline)
}

func invokeControllerPersonaResetDrain(
	local ControllerPersonaLocalExecutor,
	deadline time.Time,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = localPanicError(recovered)
		}
	}()
	return local.ResetAndDrain(deadline)
}

func invokeControllerPersonaResetNeutral(
	local ControllerPersonaLocalExecutor,
	execution ControllerPersonaLocalExecution,
	deadline time.Time,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = localPanicError(recovered)
		}
	}()
	return local.ResetNeutral(execution, deadline)
}

func (adapter *DormantRetainedUSBAdapter) drainLocalExecutor(
	local ControllerPersonaLocalExecutor,
	deadline time.Time,
) error {
	adapter.localDrainOnce.Do(func() {
		go func() {
			adapter.localDrainErr = invokeControllerPersonaLocalDrain(local, deadline)
			close(adapter.localDrainDone)
		}()
	})
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errDormantRetainedUSBDeadline
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-adapter.localDrainDone:
		return adapter.localDrainErr
	case <-timer.C:
		return errDormantRetainedUSBDeadline
	}
}

func (adapter *DormantRetainedUSBAdapter) waitLocalAdmission(
	deadline time.Time,
) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errDormantRetainedUSBDeadline
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-adapter.localAdmission:
		adapter.localAdmission <- struct{}{}
		return nil
	case <-timer.C:
		return errDormantRetainedUSBDeadline
	}
}

func invokeControllerPersonaDisconnectNeutral(
	local ControllerPersonaLocalExecutor,
	execution ControllerPersonaLocalExecution,
	deadline time.Time,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = localPanicError(recovered)
		}
	}()
	return local.DisconnectNeutral(execution, deadline)
}

type controllerPersonaLocalTerminal struct {
	err        error
	finishedAt time.Time
}

func runControllerPersonaLocalTerminal(
	deadline time.Time,
	callback func() error,
) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errDormantRetainedUSBDeadline
	}
	terminal := make(chan controllerPersonaLocalTerminal, 1)
	go func() {
		terminal <- controllerPersonaLocalTerminal{
			err: callback(), finishedAt: time.Now(),
		}
	}()
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case completed := <-terminal:
		if completed.finishedAt.After(deadline) {
			return errors.Join(completed.err, errDormantRetainedUSBDeadline)
		}
		return completed.err
	case <-timer.C:
		return errDormantRetainedUSBDeadline
	}
}

func runControllerPersonaDisconnectNeutral(
	local ControllerPersonaLocalExecutor,
	execution ControllerPersonaLocalExecution,
	deadline time.Time,
) error {
	return runControllerPersonaLocalTerminal(deadline, func() error {
		return invokeControllerPersonaDisconnectNeutral(
			local, execution, deadline)
	})
}

func localPanicError(recovered any) error {
	if recoveredErr, ok := recovered.(error); ok {
		return fmt.Errorf("%w: %w", errDormantRetainedUSBLocalPanic, recoveredErr)
	}
	return fmt.Errorf("%w: %v", errDormantRetainedUSBLocalPanic, recovered)
}

func localExecutionFromLease(
	lease controllerPersonaLocalLease,
) ControllerPersonaLocalExecution {
	return ControllerPersonaLocalExecution{
		Action: lease.action, Generation: lease.generation, Order: lease.order,
		ClearEpoch: lease.clearEpoch, DirectMotor: lease.directMotor,
		GuideLED: lease.guideLED,
	}
}

func (adapter *DormantRetainedUSBAdapter) recordLocalProgress(err error) {
	adapter.mu.Lock()
	if err != nil {
		// An ordinary local failure is retryable by the canonical engine. Keep it
		// diagnostic-only; only an uncertain drain/disconnect terminal quarantines.
		adapter.lastLocalError = fmt.Errorf("local action: %w", err)
	}
	readinessErr := adapter.advanceReadinessLocked()
	ready := adapter.readiness
	adapter.mu.Unlock()
	_ = readinessErr
	latchedSignal(ready)
}

func (adapter *DormantRetainedUSBAdapter) markLocalFatal(err error) {
	adapter.mu.Lock()
	if err == nil {
		err = errDormantRetainedUSBLocalPanic
	}
	adapter.fatalLocalError = errors.Join(adapter.fatalLocalError, err)
	readinessErr := adapter.advanceReadinessLocked()
	ready := adapter.readiness
	adapter.mu.Unlock()
	_ = readinessErr
	latchedSignal(ready)
}

// FenceAndDrainReset is the first optional dormant reversible-reset owner
// phase. It synchronously closes Stage/Prepare admission, reversibly fences
// the local executor, and joins both local execution and the adapter worker.
// Existing retained tickets deliberately remain owned: the outer scheduler
// must retire or complete each exact ticket before ResetAndRestart.
func (adapter *DormantRetainedUSBAdapter) FenceAndDrainReset(
	reset retainedusb.ImportResetLease,
	deadline time.Time,
) error {
	if adapter == nil {
		return errDormantRetainedUSBUninitialized
	}
	adapter.mu.Lock()
	if adapter.state == dormantRetainedUSBResetDrained &&
		adapter.activeReset == reset {
		adapter.mu.Unlock()
		return nil
	}
	if adapter.state == dormantRetainedUSBQuarantine {
		err := adapter.quarantineErrorLocked()
		adapter.mu.Unlock()
		return err
	}
	expectedResetGeneration := uint64(1)
	if adapter.lastReset.Valid() {
		if adapter.lastReset.ResetGeneration == ^uint64(0) {
			adapter.mu.Unlock()
			return errDormantRetainedUSBTokenExhausted
		}
		expectedResetGeneration = adapter.lastReset.ResetGeneration + 1
	}
	valid := reset.Valid() && reset.ImportLease == adapter.boundLease &&
		reset.ResetGeneration == expectedResetGeneration &&
		adapter.state == dormantRetainedUSBBound
	if !valid {
		adapter.mu.Unlock()
		return errDormantRetainedUSBInvalidImport
	}
	if adapter.inputHistoryFault {
		adapter.mu.Unlock()
		return errRetainedInputHistoryFault
	}
	if !deadlineValid(deadline) || adapter.generation == ^uint64(0) ||
		adapter.readinessEpoch >= ^uint64(0)-1 ||
		adapter.fatalLocalError != nil {
		err := errors.Join(
			adapter.fatalLocalError, errDormantRetainedUSBInvalidImport)
		if !deadlineValid(deadline) {
			err = errors.Join(err, errDormantRetainedUSBDeadline)
		}
		if adapter.generation == ^uint64(0) ||
			adapter.readinessEpoch >= ^uint64(0)-1 {
			err = errors.Join(err, errDormantRetainedUSBTokenExhausted)
		}
		adapter.activeReset = reset
		adapter.finishResetQuarantinedLocked(reset, err)
		adapter.mu.Unlock()
		return err
	}
	adapter.state = dormantRetainedUSBResetDraining
	adapter.activeReset = reset
	local := adapter.local
	stop := adapter.localStop
	done := adapter.localDone
	adapter.mu.Unlock()

	resetDrainErr := runControllerPersonaLocalTerminal(deadline, func() error {
		return invokeControllerPersonaResetDrain(local, deadline)
	})
	// ResetAndDrain closes the executor-side race. The adapter admission token
	// then proves no Execute already admitted by this worker remains in flight.
	admissionErr := adapter.waitLocalAdmission(deadline)
	adapter.localStopOnce.Do(func() { close(stop) })
	joinErr := waitForLocalDone(done, deadline)
	if resetDrainErr != nil || admissionErr != nil || joinErr != nil ||
		time.Now().After(deadline) {
		err := errors.Join(resetDrainErr, admissionErr, joinErr)
		if err == nil {
			err = errDormantRetainedUSBDeadline
		}
		adapter.finishResetQuarantined(reset, err)
		return err
	}

	adapter.mu.Lock()
	if adapter.state != dormantRetainedUSBResetDraining ||
		adapter.activeReset != reset {
		err := errDormantRetainedUSBInvalidImport
		adapter.finishResetQuarantinedLocked(reset, err)
		adapter.mu.Unlock()
		return err
	}
	adapter.state = dormantRetainedUSBResetDrained
	adapter.mu.Unlock()
	return nil
}

// ResetAndRestart is the second optional dormant reversible-reset owner
// phase. The retained scheduler must have already fenced ingress and drained
// every exact retained ticket, and FenceAndDrainReset must have joined local
// execution. It advances the canonical coordinator through beginUSBReset and
// delivers its one successor-generation clear through the reset-specific
// executor path. Owner admission reopens only after that exact clear resolves
// Delivered. For direct offline tests it may invoke the first phase itself;
// the still-owned slot check then prevents bypassing the scheduler drain. No
// USB setup packet or production route calls this method.
func (adapter *DormantRetainedUSBAdapter) ResetAndRestart(
	reset retainedusb.ImportResetLease,
	deadline time.Time,
) (retainedusb.ImportResetResult, error) {
	result := retainedusb.ImportResetResult{
		Lease: reset, State: retainedusb.ImportResetInvalid,
	}
	if adapter == nil {
		return result, errDormantRetainedUSBUninitialized
	}
	adapter.mu.Lock()
	if reset == adapter.lastReset &&
		adapter.lastResetResult.Lease == reset {
		cached := adapter.lastResetResult
		var cachedErr error
		if cached.State == retainedusb.ImportResetQuarantined {
			cachedErr = adapter.quarantineErrorLocked()
		} else if cached.State != retainedusb.ImportResetSafe {
			cached = result
			cachedErr = errDormantRetainedUSBInvalidImport
		}
		adapter.mu.Unlock()
		return cached, cachedErr
	}
	state := adapter.state
	activeReset := adapter.activeReset
	if state == dormantRetainedUSBQuarantine ||
		(state != dormantRetainedUSBBound &&
			state != dormantRetainedUSBResetDrained) ||
		(state == dormantRetainedUSBResetDrained && activeReset != reset) {
		adapter.mu.Unlock()
		return result, errDormantRetainedUSBInvalidImport
	}
	adapter.mu.Unlock()
	if state == dormantRetainedUSBBound {
		if err := adapter.FenceAndDrainReset(reset, deadline); err != nil {
			adapter.mu.Lock()
			if adapter.lastReset == reset &&
				adapter.lastResetResult.Lease == reset &&
				adapter.lastResetResult.State == retainedusb.ImportResetQuarantined {
				result = adapter.lastResetResult
			}
			adapter.mu.Unlock()
			return result, err
		}
	}
	adapter.mu.Lock()
	if adapter.state != dormantRetainedUSBResetDrained ||
		adapter.activeReset != reset || !adapter.slotsEmptyLocked() ||
		!deadlineValid(deadline) {
		err := errDormantRetainedUSBInvalidImport
		if !deadlineValid(deadline) {
			err = errors.Join(err, errDormantRetainedUSBDeadline)
		}
		adapter.finishResetQuarantinedLocked(reset, err)
		result.State = retainedusb.ImportResetQuarantined
		adapter.mu.Unlock()
		return result, err
	}
	adapter.state = dormantRetainedUSBResetNeutralizing
	local := adapter.local
	adapter.mu.Unlock()

	now := time.Now()
	nowMS, err := adapter.milliseconds(now)
	adapter.mu.Lock()
	if nowMS < adapter.protocolTimeMS {
		nowMS = adapter.protocolTimeMS
	}
	adapter.mu.Unlock()
	if err == nil {
		err = adapter.coordinator.beginUSBReset(nowMS)
	}
	var localLease controllerPersonaLocalLease
	if err == nil {
		var present bool
		localLease, present, err = adapter.coordinator.admitLocal(nowMS)
		if err == nil && (!present ||
			localLease.action != ControllerPersonaClearOutputs) {
			err = errDormantRetainedUSBInvalidPreparation
		}
	}
	if err != nil {
		adapter.finishResetQuarantined(reset, err)
		result.State = retainedusb.ImportResetQuarantined
		return result, err
	}

	execution := localExecutionFromLease(localLease)
	if !execution.valid() || execution.Action != ControllerPersonaClearOutputs {
		err = errDormantRetainedUSBInvalidPreparation
	} else {
		err = runControllerPersonaLocalTerminal(deadline, func() error {
			return invokeControllerPersonaResetNeutral(
				local, execution, deadline)
		})
	}
	if err != nil || time.Now().After(deadline) {
		if err == nil {
			err = errDormantRetainedUSBDeadline
		}
		_, completeErr := adapter.completeLocalAt(
			localLease, ControllerPersonaDeliveryFailed, nowMS)
		err = errors.Join(err, completeErr)
		adapter.finishResetQuarantined(reset, err)
		result.State = retainedusb.ImportResetQuarantined
		return result, err
	}
	completionMS, err := adapter.completeLocalAt(
		localLease, ControllerPersonaDelivered, nowMS)
	if err != nil {
		adapter.finishResetQuarantined(reset, err)
		result.State = retainedusb.ImportResetQuarantined
		return result, err
	}
	snapshot, ok := adapter.coordinator.snapshot()
	if !ok || !snapshot.Attached ||
		snapshot.USBState != USBControlDeviceDefault ||
		snapshot.Generation != execution.Generation ||
		snapshot.ClaimOutstanding || snapshot.RetryPending ||
		snapshot.OrdinaryFeedbackOutstanding || snapshot.OrdinaryFeedbackRetryPending ||
		snapshot.ConfigurationLossClearPending ||
		snapshot.Feedback.ClearEpoch != execution.ClearEpoch {
		err = ErrControllerPersonaInvariantViolation
		adapter.finishResetQuarantined(reset, err)
		result.State = retainedusb.ImportResetQuarantined
		return result, err
	}
	if err = adapter.coordinator.adoptRetainedUSBIPAddress(); err != nil {
		adapter.finishResetQuarantined(reset, err)
		result.State = retainedusb.ImportResetQuarantined
		return result, err
	}
	snapshot, ok = adapter.coordinator.snapshot()
	if !ok || !snapshot.Attached ||
		snapshot.USBState != USBControlDeviceAddressed ||
		snapshot.Generation != execution.Generation ||
		snapshot.ClaimOutstanding || snapshot.RetryPending ||
		snapshot.OrdinaryFeedbackOutstanding || snapshot.OrdinaryFeedbackRetryPending ||
		snapshot.ConfigurationLossClearPending ||
		snapshot.Feedback.ClearEpoch != execution.ClearEpoch {
		err = ErrControllerPersonaInvariantViolation
		adapter.finishResetQuarantined(reset, err)
		result.State = retainedusb.ImportResetQuarantined
		return result, err
	}

	adapter.mu.Lock()
	if adapter.state != dormantRetainedUSBResetNeutralizing ||
		adapter.activeReset != reset || !adapter.slotsEmptyLocked() ||
		adapter.fatalLocalError != nil || adapter.generation == ^uint64(0) {
		err = errors.Join(
			errDormantRetainedUSBInvalidImport, adapter.fatalLocalError)
		adapter.finishResetQuarantinedLocked(reset, err)
		result.State = retainedusb.ImportResetQuarantined
		adapter.mu.Unlock()
		return result, err
	}
	adapter.generation++
	if completionMS > adapter.protocolTimeMS {
		adapter.protocolTimeMS = completionMS
	}
	adapter.lastLocalError = nil
	adapter.normalizeGuideEdgesLocked()
	adapter.lastReset = reset
	adapter.activeReset = retainedusb.ImportResetLease{}
	result.State = retainedusb.ImportResetSafe
	adapter.lastResetResult = result
	adapter.state = dormantRetainedUSBBound
	adapter.startLocalWorkerLocked()
	readinessErr := adapter.advanceReadinessLocked()
	ready := adapter.readiness
	adapter.mu.Unlock()
	latchedSignal(ready)
	if readinessErr != nil {
		adapter.finishResetQuarantined(reset, readinessErr)
		return retainedusb.ImportResetResult{
			Lease: reset, State: retainedusb.ImportResetQuarantined,
		}, readinessErr
	}
	return result, nil
}

func (adapter *DormantRetainedUSBAdapter) finishResetQuarantined(
	reset retainedusb.ImportResetLease,
	err error,
) {
	adapter.mu.Lock()
	adapter.finishResetQuarantinedLocked(reset, err)
	adapter.mu.Unlock()
}

func (adapter *DormantRetainedUSBAdapter) finishResetQuarantinedLocked(
	reset retainedusb.ImportResetLease,
	err error,
) {
	adapter.lastReset = reset
	adapter.lastResetResult = retainedusb.ImportResetResult{
		Lease: reset, State: retainedusb.ImportResetQuarantined,
	}
	adapter.quarantineLocked(err)
}

// CancelAndDrain implements the first whole-import terminal phase. It is
// accepted only for the exact capability recorded by BindImport. Its first
// Bound -> Draining/Quarantine transition occurs under adapter.mu, which
// synchronously closes this owner's Stage/Prepare admission even if the outer
// transport fence could not be proven. ResetDrained may also be upgraded to
// this irreversible terminal fence after an outer reset failure; it cannot
// reopen the reversible reset path. Ordinary local execution is cancelled and
// joined before Drained is returned.
func (adapter *DormantRetainedUSBAdapter) CancelAndDrain(
	lease retainedusb.ImportLease,
	reason retainedusb.ImportCloseReason,
	deadline time.Time,
) (retainedusb.ImportDrainResult, error) {
	result := retainedusb.ImportDrainResult{
		Lease: lease, Reason: reason, State: retainedusb.ImportDrainInvalid,
	}
	if adapter == nil {
		return result, errDormantRetainedUSBUninitialized
	}
	if !reason.Valid() {
		return result, errDormantRetainedUSBInvalidImport
	}
	adapter.mu.Lock()
	if !lease.Valid() || lease != adapter.boundLease {
		adapter.mu.Unlock()
		return result, errDormantRetainedUSBInvalidImport
	}
	if adapter.state == dormantRetainedUSBQuarantine {
		if adapter.closeReason != 0 && adapter.closeReason != reason {
			adapter.mu.Unlock()
			return result, errDormantRetainedUSBInvalidImport
		}
		if adapter.closeReason == 0 {
			adapter.closeReason = reason
		}
		local := adapter.local
		stop := adapter.localStop
		done := adapter.localDone
		result.State = retainedusb.ImportDrainQuarantined
		adapter.mu.Unlock()
		return result, adapter.containQuarantinedImport(
			lease, local, stop, done, deadline)
	}
	if adapter.state != dormantRetainedUSBBound &&
		adapter.state != dormantRetainedUSBResetDrained {
		adapter.mu.Unlock()
		return result, errDormantRetainedUSBInvalidImport
	}
	if !deadlineValid(deadline) {
		adapter.closeReason = reason
		adapter.quarantineLocked(errDormantRetainedUSBDeadline)
		local := adapter.local
		stop := adapter.localStop
		done := adapter.localDone
		result.State = retainedusb.ImportDrainQuarantined
		adapter.mu.Unlock()
		return result, adapter.containQuarantinedImport(
			lease, local, stop, done, deadline)
	}
	adapter.state = dormantRetainedUSBDraining
	adapter.closeReason = reason
	result.State = retainedusb.ImportDrainQuarantined
	local := adapter.local
	stop := adapter.localStop
	done := adapter.localDone
	adapter.mu.Unlock()

	drainErr := adapter.drainLocalExecutor(local, deadline)
	// The executor has permanently fenced future ordinary execution. Waiting
	// for this gate additionally proves that no call already admitted by the
	// adapter remains inside Execute when this method returns.
	admissionErr := adapter.waitLocalAdmission(deadline)
	adapter.localStopOnce.Do(func() { close(stop) })
	joinErr := waitForLocalDone(done, deadline)

	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if drainErr != nil || admissionErr != nil || joinErr != nil ||
		time.Now().After(deadline) ||
		!adapter.slotsEmptyLocked() {
		err := errors.Join(drainErr, admissionErr, joinErr)
		if err == nil {
			err = errDormantRetainedUSBDeadline
		}
		adapter.quarantineLocked(err)
		return result, err
	}
	adapter.state = dormantRetainedUSBDrained
	result.State = retainedusb.ImportDrainDrained
	return result, nil
}

// containQuarantinedImport is a containment-only terminal. It is authorized
// solely by the exact retained lease after a prior invariant failure. It may
// cancel and join ordinary local execution, but it deliberately retains every
// ownership ledger and always reports Quarantined. Therefore neither this
// method nor a later caller can mistake physical-executor containment for a
// canonical neutral/disconnect proof or release the import.
func (adapter *DormantRetainedUSBAdapter) containQuarantinedImport(
	lease retainedusb.ImportLease,
	local ControllerPersonaLocalExecutor,
	stop chan struct{},
	done <-chan struct{},
	deadline time.Time,
) error {
	var drainErr, admissionErr, joinErr error
	if local == nil || stop == nil || done == nil {
		drainErr = errDormantRetainedUSBInvalidImport
	} else {
		drainErr = adapter.drainLocalExecutor(local, deadline)
		admissionErr = adapter.waitLocalAdmission(deadline)
		adapter.localStopOnce.Do(func() { close(stop) })
		joinErr = waitForLocalDone(done, deadline)
	}
	containmentErr := errors.Join(drainErr, admissionErr, joinErr)

	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.state != dormantRetainedUSBQuarantine ||
		lease != adapter.boundLease {
		return errors.Join(
			errDormantRetainedUSBInvalidImport, containmentErr)
	}
	if containmentErr == nil {
		adapter.quarantineContained = true
	} else if adapter.quarantineContainmentErr == nil {
		adapter.quarantineContainmentErr = containmentErr
	}
	return errors.Join(
		adapter.quarantineErrorLocked(),
		adapter.quarantineContainmentErr,
		containmentErr,
	)
}

func waitForLocalDone(done <-chan struct{}, deadline time.Time) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errDormantRetainedUSBDeadline
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return errDormantRetainedUSBDeadline
	}
}

// DisconnectNeutral implements the second whole-import terminal phase. It
// advances the canonical persona through BeginDisconnect, admits the one clear
// epoch, and invokes the executor's separately authorized neutral method.
// Safe is returned only after that exact clear resolves delivered.
func (adapter *DormantRetainedUSBAdapter) DisconnectNeutral(
	lease retainedusb.ImportLease,
	reason retainedusb.ImportCloseReason,
	deadline time.Time,
) (retainedusb.ImportDisconnectResult, error) {
	result := retainedusb.ImportDisconnectResult{
		Lease: lease, Reason: reason,
		State: retainedusb.ImportDisconnectInvalid,
	}
	if adapter == nil {
		return result, errDormantRetainedUSBUninitialized
	}
	if !reason.Valid() {
		return result, errDormantRetainedUSBInvalidImport
	}
	adapter.mu.Lock()
	if adapter.state != dormantRetainedUSBDrained ||
		lease != adapter.boundLease || reason != adapter.closeReason ||
		!adapter.slotsEmptyLocked() {
		adapter.mu.Unlock()
		return result, errDormantRetainedUSBInvalidImport
	}
	if !deadlineValid(deadline) {
		adapter.quarantineLocked(errDormantRetainedUSBDeadline)
		result.State = retainedusb.ImportDisconnectQuarantined
		adapter.mu.Unlock()
		return result, errDormantRetainedUSBDeadline
	}
	adapter.state = dormantRetainedUSBDisconnecting
	result.State = retainedusb.ImportDisconnectQuarantined
	local := adapter.local
	adapter.mu.Unlock()

	now := time.Now()
	nowMS, err := adapter.milliseconds(now)
	adapter.mu.Lock()
	if nowMS < adapter.protocolTimeMS {
		nowMS = adapter.protocolTimeMS
	}
	adapter.mu.Unlock()
	if err == nil {
		err = adapter.coordinator.beginDisconnect(nowMS)
	}
	var localLease controllerPersonaLocalLease
	if err == nil {
		var present bool
		localLease, present, err = adapter.coordinator.admitLocal(nowMS)
		if err == nil && (!present ||
			localLease.action != ControllerPersonaClearOutputs) {
			err = errDormantRetainedUSBInvalidPreparation
		}
	}
	if err != nil {
		adapter.finishDisconnectQuarantined(err)
		return result, err
	}
	execution := localExecutionFromLease(localLease)
	if !execution.valid() || execution.Action != ControllerPersonaClearOutputs {
		err = errDormantRetainedUSBInvalidPreparation
	} else {
		err = runControllerPersonaDisconnectNeutral(local, execution, deadline)
	}
	if err != nil || time.Now().After(deadline) {
		if err == nil {
			err = errDormantRetainedUSBDeadline
		}
		_, completeErr := adapter.completeLocalAt(
			localLease, ControllerPersonaDeliveryFailed, nowMS)
		err = errors.Join(err, completeErr)
		adapter.finishDisconnectQuarantined(err)
		return result, err
	}
	completionMS, err := adapter.completeLocalAt(
		localLease, ControllerPersonaDelivered, nowMS)
	if err != nil {
		adapter.finishDisconnectQuarantined(err)
		return result, err
	}
	snapshot, ok := adapter.coordinator.snapshot()
	if !ok || snapshot.Attached ||
		snapshot.USBState != USBControlDeviceDetached ||
		snapshot.ClaimOutstanding || snapshot.RetryPending ||
		snapshot.OrdinaryFeedbackOutstanding || snapshot.OrdinaryFeedbackRetryPending ||
		snapshot.ConfigurationLossClearPending ||
		snapshot.Feedback.ClearEpoch != execution.ClearEpoch {
		err = ErrControllerPersonaInvariantViolation
		adapter.finishDisconnectQuarantined(err)
		return result, err
	}
	adapter.mu.Lock()
	if completionMS > adapter.protocolTimeMS {
		adapter.protocolTimeMS = completionMS
	}
	if adapter.fatalLocalError != nil {
		err = adapter.fatalLocalError
		adapter.quarantineLocked(err)
		adapter.mu.Unlock()
		return result, err
	}
	adapter.state = dormantRetainedUSBDisconnected
	adapter.mu.Unlock()
	result.State = retainedusb.ImportDisconnectSafe
	return result, nil
}

func (adapter *DormantRetainedUSBAdapter) finishDisconnectQuarantined(err error) {
	adapter.mu.Lock()
	adapter.quarantineLocked(err)
	adapter.mu.Unlock()
}

// ReconnectDormantSession is an explicit, still-unrouted successor seam. It
// cannot run before DisconnectNeutral Safe. It advances the same canonical
// coordinator, rotates adapter/session generations without reuse, installs a
// fresh local executor, and returns to the pre-bind state. A future outer
// orchestrator must allocate and coordinate the exact successor session. The
// current server instead removes the one-shot authorized wrapper from its bus
// after Safe disconnect; it never reuses the stopped executor.
func (adapter *DormantRetainedUSBAdapter) ReconnectDormantSession(
	local ControllerPersonaLocalExecutor,
	localTimeout time.Duration,
	now time.Time,
) error {
	if adapter == nil || local == nil ||
		localTimeout < dormantRetainedUSBMinimumLocalTimeout ||
		localTimeout > dormantRetainedUSBMaximumLocalTimeout {
		return errDormantRetainedUSBUninitialized
	}
	nowMS, err := adapter.milliseconds(now)
	if err != nil {
		return err
	}
	adapter.mu.Lock()
	if adapter.state != dormantRetainedUSBDisconnected || adapter.inputHistoryFault ||
		adapter.generation == ^uint64(0) ||
		!adapter.slotsEmptyLocked() {
		adapter.mu.Unlock()
		return errDormantRetainedUSBInvalidImport
	}
	adapter.mu.Unlock()
	adapter.mu.Lock()
	if nowMS < adapter.protocolTimeMS {
		nowMS = adapter.protocolTimeMS
	}
	adapter.mu.Unlock()
	if err := adapter.coordinator.reconnect(nowMS); err != nil {
		return err
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.state != dormantRetainedUSBDisconnected {
		adapter.quarantineLocked(errDormantRetainedUSBInvalidImport)
		return errDormantRetainedUSBInvalidImport
	}
	adapter.session = 0
	adapter.generation++
	adapter.local = local
	adapter.localTimeout = localTimeout
	adapter.localDrainOnce = sync.Once{}
	adapter.localDrainDone = make(chan struct{})
	adapter.localDrainErr = nil
	adapter.quarantineContainmentErr = nil
	adapter.quarantineContained = false
	adapter.boundLease = retainedusb.ImportLease{}
	adapter.activeReset = retainedusb.ImportResetLease{}
	adapter.lastReset = retainedusb.ImportResetLease{}
	adapter.lastResetResult = retainedusb.ImportResetResult{}
	adapter.closeReason = 0
	adapter.lastLocalError = nil
	adapter.fatalLocalError = nil
	adapter.quarantine = nil
	adapter.inputRevision = 1
	adapter.normalizeGuideEdgesLocked()
	adapter.state = dormantRetainedUSBUnbound
	if err := adapter.advanceReadinessLocked(); err != nil {
		return err
	}
	latchedSignal(adapter.readiness)
	return nil
}

func (adapter *DormantRetainedUSBAdapter) slotsEmptyLocked() bool {
	for index := range adapter.slots {
		if adapter.slots[index].state != dormantRetainedUSBSlotEmpty {
			return false
		}
	}
	return true
}

func (adapter *DormantRetainedUSBAdapter) quarantineLocked(err error) {
	adapter.state = dormantRetainedUSBQuarantine
	if err == nil {
		err = errDormantRetainedUSBQuarantined
	}
	adapter.quarantine = errors.Join(adapter.quarantine, err)
	// Closing a channel is nonblocking. State changes first so a worker which
	// wins a simultaneous wake still fails its final admission check. An action
	// already inside Execute is joined later by exact-lease containment.
	if adapter.localStop != nil {
		stop := adapter.localStop
		adapter.localStopOnce.Do(func() { close(stop) })
	}
}

func (adapter *DormantRetainedUSBAdapter) quarantineErrorLocked() error {
	if adapter.quarantine != nil {
		return errors.Join(errDormantRetainedUSBQuarantined, adapter.quarantine)
	}
	return errDormantRetainedUSBQuarantined
}

var _ retainedusb.Owner = (*DormantRetainedUSBAdapter)(nil)
var _ retainedusb.ImportSessionOwner = (*DormantRetainedUSBAdapter)(nil)
var _ retainedusb.ImportResetSessionOwner = (*DormantRetainedUSBAdapter)(nil)

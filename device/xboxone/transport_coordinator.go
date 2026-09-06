package xboxone

import (
	"errors"
	"fmt"
	"sync"
)

// controllerPersonaTransportLane is deliberately package-local. The
// coordinator is an offline adapter prerequisite, not a registrable USB
// device or a promise that the current USB/IP server can drive it.
type controllerPersonaTransportLane uint8

const (
	controllerPersonaTransportControl controllerPersonaTransportLane = iota + 1
	controllerPersonaTransportInterruptIn
	controllerPersonaTransportInterruptOut
)

type controllerPersonaTransportDisposition uint8

const (
	// controllerPersonaTransportWait means the staged request remains owned by
	// its lane and no response may be emitted. A future server adapter must
	// retain and retry that exact request rather than ACK, STALL, or replace it.
	controllerPersonaTransportWait controllerPersonaTransportDisposition = iota + 1
	controllerPersonaTransportControlResponse
	controllerPersonaTransportInterruptInResponse
	controllerPersonaTransportInterruptOutResponse
	// controllerPersonaTransportLocalRequired means a preceding zero-length
	// persona action must reach the separate local-executor boundary first.
	controllerPersonaTransportLocalRequired
)

type controllerPersonaTransportWaitReason uint8

const (
	controllerPersonaTransportNoWait controllerPersonaTransportWaitReason = iota
	controllerPersonaTransportNoPresentation
	controllerPersonaTransportUpstreamPending
	controllerPersonaTransportLocalPending
	controllerPersonaTransportResponseOutstanding
	controllerPersonaTransportBufferTooSmall
	// controllerPersonaTransportInputRequired retains an endpoint request
	// without selecting the lifecycle's mandatory initial-input claim. Only
	// admitInput may supply the latest semantic report and select that claim at
	// the final response-serializer boundary.
	controllerPersonaTransportInputRequired
	// controllerPersonaTransportBoundaryRequired fences a host command whose
	// RET_SUBMIT failed. Its engine retry must be retired by an authoritative
	// transport-generation boundary, never executed by an unrelated endpoint.
	controllerPersonaTransportBoundaryRequired
	// InputUnchanged has no periodic input retry: publication/lifecycle readiness
	// wakes it. Independently required protocol deadlines remain explicit.
	controllerPersonaTransportInputUnchanged
)

var (
	errControllerPersonaTransportUninitialized = errors.New(
		"xboxone: uninitialized persona transport coordinator")
	errControllerPersonaTransportLaneBusy = errors.New(
		"xboxone: persona transport lane already owns a staged request")
	errInvalidControllerPersonaTransportTicket = errors.New(
		"xboxone: invalid persona transport ticket")
	errControllerPersonaTransportResponseOutstanding = errors.New(
		"xboxone: persona transport response already outstanding")
	errControllerPersonaTransportLocalOutstanding = errors.New(
		"xboxone: persona local execution already outstanding")
	errInvalidControllerPersonaTransportCompletion = errors.New(
		"xboxone: invalid persona transport completion")
	errControllerPersonaTransportTokenExhausted = errors.New(
		"xboxone: persona transport token exhausted")
	errControllerPersonaTransportBoundaryRequired = errors.New(
		"xboxone: persona transport generation boundary required")
)

// controllerPersonaTransportTicket owns only a copied transport request. It
// never owns a ControllerPersonaEngine claim. Consequently EP0, interrupt-IN,
// and interrupt-OUT may all stage concurrently without their goroutine
// scheduling order deciding persona order.
type controllerPersonaTransportTicket struct {
	owner      *controllerPersonaTransportCoordinator
	token      uint64
	generation uint64
	lane       controllerPersonaTransportLane
}

func (ticket controllerPersonaTransportTicket) valid() bool {
	return ticket.owner != nil && ticket.token != 0 && ticket.generation != 0 &&
		ticket.lane >= controllerPersonaTransportControl &&
		ticket.lane <= controllerPersonaTransportInterruptOut
}

type controllerPersonaTransportStage struct {
	ticket controllerPersonaTransportTicket

	setup [usbSetupPacketSize]byte
	wire  [ControllerPersonaMaximumWireSize]byte
	size  uint8

	maximumInputSize uint8
}

type controllerPersonaTransportRoute uint8

const (
	controllerPersonaTransportUpstream controllerPersonaTransportRoute = iota + 1
	controllerPersonaTransportLocal
)

type controllerPersonaPendingAction struct {
	claim           ControllerPersonaClaim
	route           controllerPersonaTransportRoute
	selectionOrder  uint64
	fromHostCommand bool
	fromGuideEdge   bool
}

// controllerPersonaTransportAdmission describes only work selected at the
// late, serialized boundary. For response dispositions, Size bytes have been
// copied into the caller's scratch for EP0/interrupt-IN, or Size is zero for
// interrupt-OUT. Wait and LocalRequired emit no bytes and do not consume the
// staged ticket.
type controllerPersonaTransportAdmission struct {
	disposition controllerPersonaTransportDisposition
	waitReason  controllerPersonaTransportWaitReason
	order       uint64

	action            ControllerPersonaAction
	size              int
	usbResponseKind   USBControlResponseKind
	hostDisposition   ControllerPersonaHostDisposition
	consumesGuideEdge bool
	retryAfterMS      uint64
}

type controllerPersonaActiveResponse struct {
	ticket controllerPersonaTransportTicket
	claim  ControllerPersonaClaim

	hasClaim        bool
	claimAdmitted   bool
	responseOrder   uint64
	selectionOrder  uint64
	hostDisposition ControllerPersonaHostDisposition
}

// controllerPersonaLocalLease is the exact zero-length persona obligation an
// asynchronous local executor owns. Its typed values are copied out of the
// opaque engine claim; the lease itself remains coordinator-authenticated.
type controllerPersonaLocalLease struct {
	owner       *controllerPersonaTransportCoordinator
	token       uint64
	generation  uint64
	order       uint64
	action      ControllerPersonaAction
	clearEpoch  uint64
	directMotor RumbleBodyV1
	guideLED    GuideLEDCommandV1
}

func (lease controllerPersonaLocalLease) valid() bool {
	return lease.owner != nil && lease.token != 0 && lease.generation != 0 &&
		lease.action != 0
}

type controllerPersonaActiveLocal struct {
	lease            controllerPersonaLocalLease
	claim            ControllerPersonaClaim
	detachedFeedback bool
}

type controllerPersonaFailedHostAction struct {
	generation uint64
	action     ControllerPersonaAction
}

// controllerPersonaTransportCoordinator is the package-local transaction
// boundary between three concurrently scheduled USB endpoint lanes and one
// canonical ControllerPersonaEngine. The caller transfers exclusive ownership
// of engine at construction and must not call it directly afterwards.
//
// Stage methods merely copy bounded request facts. admit/admitInput are the
// only ordinary methods which may select an engine claim for a response, and
// they are designed to run while the caller owns its response serializer.
// admitLocal only consumes an already-selected local claim. beginUSBReset is a
// separate authoritative lifecycle selector. An accepted host OUT command
// transfers its still-unadmitted persona action to interrupt-IN or the local
// executor only after the ACK response completes successfully. It is never
// resolved merely because the host command was acknowledged.
//
// The legacy server interfaces cannot call this type truthfully because they
// fix response result/size before serialization. The explicit retained USB/IP
// import path reaches it only through DormantRetainedUSBAdapter, which owns all
// three lanes together. Keeping the coordinator package-local prevents a
// second or partial mapping stack.
type controllerPersonaTransportCoordinator struct {
	// Enabled by the retained production adapter. Offline codec/coordinator
	// callers can still supply a direct complete state without a producer.
	inputJournal *retainedInputJournal
	mu           sync.Mutex
	engine       *ControllerPersonaEngine

	nextTicketToken uint64
	nextLocalToken  uint64
	nextOrder       uint64

	controlStage controllerPersonaTransportStage
	inStage      controllerPersonaTransportStage
	outStage     controllerPersonaTransportStage

	pending        controllerPersonaPendingAction
	activeResponse controllerPersonaActiveResponse
	activeLocal    controllerPersonaActiveLocal
	failedHost     controllerPersonaFailedHostAction
}

func newControllerPersonaTransportCoordinator(
	engine *ControllerPersonaEngine,
) (*controllerPersonaTransportCoordinator, error) {
	if engine == nil {
		return nil, errControllerPersonaTransportUninitialized
	}
	snapshot := engine.Snapshot()
	if snapshot.Generation == 0 || engine.ordinaryFeedbackPending() || snapshot.ClaimOutstanding ||
		snapshot.ClaimAdmitted || snapshot.RetryPending ||
		snapshot.ConfigurationLossClearPending {
		return nil, errControllerPersonaTransportUninitialized
	}
	return &controllerPersonaTransportCoordinator{engine: engine}, nil
}

func (coordinator *controllerPersonaTransportCoordinator) snapshot() (
	ControllerPersonaSnapshot,
	bool,
) {
	if coordinator == nil {
		return ControllerPersonaSnapshot{}, false
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.engine == nil {
		return ControllerPersonaSnapshot{}, false
	}
	return coordinator.engine.Snapshot(), true
}

func (coordinator *controllerPersonaTransportCoordinator) stageControl(
	setup []byte,
) (controllerPersonaTransportTicket, error) {
	if coordinator == nil {
		return controllerPersonaTransportTicket{},
			errControllerPersonaTransportUninitialized
	}
	if len(setup) != usbSetupPacketSize {
		return controllerPersonaTransportTicket{}, fmt.Errorf(
			"%w: setup length %d", errInvalidControllerPersonaTransportTicket,
			len(setup))
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	stage, err := coordinator.newStageLocked(controllerPersonaTransportControl)
	if err != nil {
		return controllerPersonaTransportTicket{}, err
	}
	copy(stage.setup[:], setup)
	coordinator.controlStage = stage
	return stage.ticket, nil
}

func (coordinator *controllerPersonaTransportCoordinator) stageInterruptIn(
	maximumSize int,
) (controllerPersonaTransportTicket, error) {
	if coordinator == nil {
		return controllerPersonaTransportTicket{},
			errControllerPersonaTransportUninitialized
	}
	if maximumSize <= 0 || maximumSize > ControllerPersonaMaximumWireSize {
		return controllerPersonaTransportTicket{}, fmt.Errorf(
			"%w: interrupt-IN maximum %d",
			errInvalidControllerPersonaTransportTicket, maximumSize)
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	stage, err := coordinator.newStageLocked(controllerPersonaTransportInterruptIn)
	if err != nil {
		return controllerPersonaTransportTicket{}, err
	}
	stage.maximumInputSize = uint8(maximumSize)
	coordinator.inStage = stage
	return stage.ticket, nil
}

func (coordinator *controllerPersonaTransportCoordinator) stageInterruptOut(
	wire []byte,
) (controllerPersonaTransportTicket, error) {
	if coordinator == nil {
		return controllerPersonaTransportTicket{},
			errControllerPersonaTransportUninitialized
	}
	if len(wire) == 0 || len(wire) > ControllerPersonaMaximumWireSize {
		return controllerPersonaTransportTicket{}, fmt.Errorf(
			"%w: interrupt-OUT length %d",
			errInvalidControllerPersonaTransportTicket, len(wire))
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	stage, err := coordinator.newStageLocked(controllerPersonaTransportInterruptOut)
	if err != nil {
		return controllerPersonaTransportTicket{}, err
	}
	stage.size = uint8(len(wire))
	copy(stage.wire[:stage.size], wire)
	coordinator.outStage = stage
	return stage.ticket, nil
}

func (coordinator *controllerPersonaTransportCoordinator) newStageLocked(
	lane controllerPersonaTransportLane,
) (controllerPersonaTransportStage, error) {
	if coordinator == nil || coordinator.engine == nil {
		return controllerPersonaTransportStage{},
			errControllerPersonaTransportUninitialized
	}
	if coordinator.stageLocked(lane).ticket.valid() {
		return controllerPersonaTransportStage{},
			errControllerPersonaTransportLaneBusy
	}
	if coordinator.nextTicketToken == ^uint64(0) {
		return controllerPersonaTransportStage{},
			errControllerPersonaTransportTokenExhausted
	}
	coordinator.nextTicketToken++
	return controllerPersonaTransportStage{ticket: controllerPersonaTransportTicket{
		owner: coordinator, token: coordinator.nextTicketToken,
		generation: coordinator.engine.Snapshot().Generation, lane: lane,
	}}, nil
}

func (coordinator *controllerPersonaTransportCoordinator) stageLocked(
	lane controllerPersonaTransportLane,
) *controllerPersonaTransportStage {
	switch lane {
	case controllerPersonaTransportControl:
		return &coordinator.controlStage
	case controllerPersonaTransportInterruptIn:
		return &coordinator.inStage
	case controllerPersonaTransportInterruptOut:
		return &coordinator.outStage
	default:
		return &controllerPersonaTransportStage{}
	}
}

func (coordinator *controllerPersonaTransportCoordinator) validateTicketLocked(
	ticket controllerPersonaTransportTicket,
) (*controllerPersonaTransportStage, error) {
	if coordinator == nil || coordinator.engine == nil || !ticket.valid() ||
		ticket.owner != coordinator {
		return nil, errInvalidControllerPersonaTransportTicket
	}
	stage := coordinator.stageLocked(ticket.lane)
	if stage.ticket != ticket ||
		ticket.generation != coordinator.engine.Snapshot().Generation {
		return nil, errInvalidControllerPersonaTransportTicket
	}
	return stage, nil
}

// retire removes a request which lost endpoint/lifecycle ownership before
// final admission. Because staging owns no engine claim, this cannot consume,
// defer, or otherwise mutate persona state.
func (coordinator *controllerPersonaTransportCoordinator) retire(
	ticket controllerPersonaTransportTicket,
) error {
	if coordinator == nil {
		return errControllerPersonaTransportUninitialized
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	stage, err := coordinator.validateStoredTicketLocked(ticket)
	if err != nil {
		return err
	}
	if coordinator.activeResponse.ticket == ticket {
		return errControllerPersonaTransportResponseOutstanding
	}
	*stage = controllerPersonaTransportStage{}
	return nil
}

func (coordinator *controllerPersonaTransportCoordinator) validateStoredTicketLocked(
	ticket controllerPersonaTransportTicket,
) (*controllerPersonaTransportStage, error) {
	if coordinator == nil || coordinator.engine == nil || !ticket.valid() ||
		ticket.owner != coordinator {
		return nil, errInvalidControllerPersonaTransportTicket
	}
	stage := coordinator.stageLocked(ticket.lane)
	if stage.ticket != ticket {
		return nil, errInvalidControllerPersonaTransportTicket
	}
	return stage, nil
}

// admit performs late selection and exact copying for one staged request. It
// must be invoked only while the caller owns the response serializer. A Wait
// or LocalRequired result emits no response and retains the exact ticket.
func (coordinator *controllerPersonaTransportCoordinator) admit(
	ticket controllerPersonaTransportTicket,
	destination []byte,
	nowMS uint64,
) (controllerPersonaTransportAdmission, error) {
	if coordinator == nil {
		return controllerPersonaTransportAdmission{},
			errControllerPersonaTransportUninitialized
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	stage, err := coordinator.validateTicketLocked(ticket)
	if err != nil {
		return controllerPersonaTransportAdmission{}, err
	}
	if coordinator.activeResponse.ticket.valid() {
		return controllerPersonaTransportAdmission{
			disposition: controllerPersonaTransportWait,
			waitReason:  controllerPersonaTransportResponseOutstanding,
		}, nil
	}
	if wait, blocked := coordinator.localWaitLocked(ticket.lane); blocked {
		return wait, nil
	}
	if coordinator.failedHost.generation != 0 {
		return controllerPersonaTransportAdmission{
			disposition: controllerPersonaTransportWait,
			waitReason:  controllerPersonaTransportBoundaryRequired,
			action:      coordinator.failedHost.action,
		}, nil
	}

	switch ticket.lane {
	case controllerPersonaTransportControl:
		return coordinator.admitControlLocked(stage, destination, nowMS)
	case controllerPersonaTransportInterruptIn:
		return coordinator.admitInterruptInLocked(
			stage, destination, nowMS, nil)
	case controllerPersonaTransportInterruptOut:
		return coordinator.admitInterruptOutLocked(stage, nowMS)
	default:
		return controllerPersonaTransportAdmission{},
			errInvalidControllerPersonaTransportTicket
	}
}

// admitInput is the only semantic-input path. The report is supplied at final
// serializer ownership, not copied into the earlier host-URB ticket. It is
// used both for ordinary input and, immediately before lifecycle selection,
// for the mandatory START initial-input image. A ticket which waited behind
// feedback, lifecycle work, or another response therefore cannot publish a
// stale pre-wait snapshot. admitInputWithGuide additionally accepts only the
// current head of the adapter's ordered Guide edge owner.
func (coordinator *controllerPersonaTransportCoordinator) admitInput(
	ticket controllerPersonaTransportTicket,
	destination []byte,
	nowMS uint64,
	input GamepadInputReportV1,
) (controllerPersonaTransportAdmission, error) {
	return coordinator.admitInputWithGuide(
		ticket, destination, nowMS, input, nil)
}

func (coordinator *controllerPersonaTransportCoordinator) admitInputWithGuide(
	ticket controllerPersonaTransportTicket,
	destination []byte,
	nowMS uint64,
	input GamepadInputReportV1,
	guide *GuideButtonStatusV1,
) (controllerPersonaTransportAdmission, error) {
	if coordinator == nil {
		return controllerPersonaTransportAdmission{},
			errControllerPersonaTransportUninitialized
	}
	if err := coordinator.engine.metadata.validateInputReport(input); err != nil {
		return controllerPersonaTransportAdmission{}, err
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	stage, err := coordinator.validateTicketLocked(ticket)
	if err != nil || ticket.lane != controllerPersonaTransportInterruptIn {
		if err != nil {
			return controllerPersonaTransportAdmission{}, err
		}
		return controllerPersonaTransportAdmission{},
			errInvalidControllerPersonaTransportTicket
	}
	if coordinator.activeResponse.ticket.valid() {
		return controllerPersonaTransportAdmission{
			disposition: controllerPersonaTransportWait,
			waitReason:  controllerPersonaTransportResponseOutstanding,
		}, nil
	}
	if wait, blocked := coordinator.localWaitLocked(ticket.lane); blocked {
		return wait, nil
	}
	if coordinator.failedHost.generation != 0 {
		return controllerPersonaTransportAdmission{
			disposition: controllerPersonaTransportWait,
			waitReason:  controllerPersonaTransportBoundaryRequired,
			action:      coordinator.failedHost.action,
		}, nil
	}
	return coordinator.admitInterruptInWithGuideLocked(
		stage, destination, nowMS, &input, guide)
}

func (coordinator *controllerPersonaTransportCoordinator) admitControlLocked(
	stage *controllerPersonaTransportStage,
	destination []byte,
	nowMS uint64,
) (controllerPersonaTransportAdmission, error) {
	if err := coordinator.materializeRetryLocked(nowMS); err != nil {
		return controllerPersonaTransportAdmission{}, err
	}
	if coordinator.pending.claim.Valid() {
		return coordinator.pendingWaitLocked(), nil
	}
	order, err := coordinator.allocateOrderLocked()
	if err != nil {
		return controllerPersonaTransportAdmission{}, err
	}
	claim, err := coordinator.engine.ClaimUSBControl(stage.setup[:])
	if err != nil {
		return controllerPersonaTransportAdmission{}, err
	}
	if claim.Size() > len(destination) {
		resolveErr := coordinator.engine.Resolve(
			claim, ControllerPersonaDeliveryFailed, nowMS)
		if resolveErr != nil {
			return controllerPersonaTransportAdmission{}, resolveErr
		}
		return controllerPersonaTransportAdmission{}, fmt.Errorf(
			"%w: EP0 destination %d, response %d",
			errInvalidControllerPersonaTransportTicket, len(destination), claim.Size())
	}
	exact := destination[:claim.Size():claim.Size()]
	if err := coordinator.engine.AdmitAndCopy(claim, exact, nowMS); err != nil {
		_ = coordinator.engine.Resolve(
			claim, ControllerPersonaDeliveryFailed, nowMS)
		return controllerPersonaTransportAdmission{}, err
	}
	coordinator.activeResponse = controllerPersonaActiveResponse{
		ticket: stage.ticket, claim: claim, hasClaim: true,
		claimAdmitted: true, responseOrder: order, selectionOrder: order,
	}
	return controllerPersonaTransportAdmission{
		disposition: controllerPersonaTransportControlResponse,
		order:       order, action: claim.Action(), size: claim.Size(),
		usbResponseKind: claim.USBResponseKind(),
	}, nil
}

func (coordinator *controllerPersonaTransportCoordinator) admitInterruptInLocked(
	stage *controllerPersonaTransportStage,
	destination []byte,
	nowMS uint64,
	latestInput *GamepadInputReportV1,
) (controllerPersonaTransportAdmission, error) {
	return coordinator.admitInterruptInWithGuideLocked(
		stage, destination, nowMS, latestInput, nil)
}

func (coordinator *controllerPersonaTransportCoordinator) admitInterruptInWithGuideLocked(
	stage *controllerPersonaTransportStage,
	destination []byte,
	nowMS uint64,
	latestInput *GamepadInputReportV1,
	guide *GuideButtonStatusV1,
) (controllerPersonaTransportAdmission, error) {
	if len(destination) < int(stage.maximumInputSize) {
		return controllerPersonaTransportAdmission{}, fmt.Errorf(
			"%w: interrupt-IN scratch %d, promised %d",
			errInvalidControllerPersonaTransportTicket, len(destination),
			stage.maximumInputSize)
	}
	inputRequired, err := coordinator.materializeNextEgressLocked(
		nowMS, latestInput)
	if err != nil {
		return controllerPersonaTransportAdmission{}, err
	}
	if inputRequired {
		messageSize, sizeErr := coordinator.engine.metadata.inputMessageSize()
		if sizeErr != nil {
			return controllerPersonaTransportAdmission{}, sizeErr
		}
		return controllerPersonaTransportAdmission{
			disposition: controllerPersonaTransportWait,
			waitReason:  controllerPersonaTransportInputRequired,
			action:      ControllerPersonaSendInitialInput,
			size:        messageSize,
		}, nil
	}
	if !coordinator.pending.claim.Valid() && guide != nil {
		selectionOrder, err := coordinator.allocateOrderLocked()
		if err != nil {
			return controllerPersonaTransportAdmission{}, err
		}
		claim, err := coordinator.engine.ClaimGuideButtonStatus(nowMS, *guide)
		if err == nil {
			if err := coordinator.setPendingGuideLocked(
				claim, selectionOrder); err != nil {
				return controllerPersonaTransportAdmission{}, err
			}
		} else if !controllerPersonaTransportTemporarilyUnavailable(err) {
			return controllerPersonaTransportAdmission{}, err
		}
	}
	if !coordinator.pending.claim.Valid() && latestInput != nil && guide == nil {
		if err := coordinator.engine.ensureOrdinaryInputAvailable(); err != nil {
			if !controllerPersonaTransportTemporarilyUnavailable(err) {
				return controllerPersonaTransportAdmission{}, err
			}
			return controllerPersonaTransportAdmission{
				disposition: controllerPersonaTransportWait,
				waitReason:  controllerPersonaTransportNoPresentation,
			}, nil
		}
		selectionOrder, err := coordinator.allocateOrderLocked()
		if err != nil {
			return controllerPersonaTransportAdmission{}, err
		}
		input := *latestInput
		if coordinator.inputJournal != nil {
			var available bool
			input, available, err = coordinator.inputJournal.selectReport(false)
			if err != nil {
				return controllerPersonaTransportAdmission{}, err
			}
			if !available {
				idle := controllerPersonaTransportAdmission{
					disposition: controllerPersonaTransportWait,
					waitReason:  controllerPersonaTransportInputUnchanged,
				}
				if dueMS, scheduled := coordinator.engine.NextPeriodicStatusDeadlineMilliseconds(); scheduled {
					if dueMS <= nowMS {
						return controllerPersonaTransportAdmission{}, ErrControllerPersonaInvariantViolation
					}
					idle.retryAfterMS = dueMS - nowMS
				}
				return idle, nil
			}
		}
		claim, err := coordinator.engine.ClaimInput(nowMS, input)
		if err == nil {
			if err := coordinator.setPendingLocked(
				claim, false, selectionOrder); err != nil {
				return controllerPersonaTransportAdmission{}, err
			}
		} else if !controllerPersonaTransportTemporarilyUnavailable(err) {
			return controllerPersonaTransportAdmission{}, err
		}
	}
	if !coordinator.pending.claim.Valid() {
		return controllerPersonaTransportAdmission{
			disposition: controllerPersonaTransportWait,
			waitReason:  controllerPersonaTransportNoPresentation,
		}, nil
	}
	if coordinator.pending.route == controllerPersonaTransportLocal {
		return coordinator.pendingWaitLocked(), nil
	}
	claim := coordinator.pending.claim
	consumesGuideEdge := coordinator.pending.fromGuideEdge
	if claim.Size() > int(stage.maximumInputSize) {
		return controllerPersonaTransportAdmission{
			disposition: controllerPersonaTransportWait,
			waitReason:  controllerPersonaTransportBufferTooSmall,
			action:      claim.Action(), size: claim.Size(),
			order: coordinator.pending.selectionOrder,
		}, nil
	}
	responseOrder, err := coordinator.allocateOrderLocked()
	if err != nil {
		return controllerPersonaTransportAdmission{}, err
	}
	exact := destination[:claim.Size():claim.Size()]
	if coordinator.inputJournal != nil && isRetainedInputAction(claim.Action()) {
		if err := coordinator.inputJournal.admit(); err != nil {
			return controllerPersonaTransportAdmission{}, err
		}
	}
	if err := coordinator.engine.AdmitAndCopy(claim, exact, nowMS); err != nil {
		resolveErr := coordinator.engine.Resolve(
			claim, ControllerPersonaDeferred, nowMS)
		coordinator.pending = controllerPersonaPendingAction{}
		if resolveErr != nil {
			return controllerPersonaTransportAdmission{}, fmt.Errorf(
				"persona IN admission: %v; defer: %w", err, resolveErr)
		}
		return controllerPersonaTransportAdmission{}, err
	}
	coordinator.activeResponse = controllerPersonaActiveResponse{
		ticket: stage.ticket, claim: claim, hasClaim: true,
		claimAdmitted: true, responseOrder: responseOrder,
		selectionOrder: coordinator.pending.selectionOrder,
	}
	coordinator.pending = controllerPersonaPendingAction{}
	return controllerPersonaTransportAdmission{
		disposition: controllerPersonaTransportInterruptInResponse,
		order:       responseOrder, action: claim.Action(), size: claim.Size(),
		consumesGuideEdge: consumesGuideEdge,
	}, nil
}

func (coordinator *controllerPersonaTransportCoordinator) admitInterruptOutLocked(
	stage *controllerPersonaTransportStage,
	nowMS uint64,
) (controllerPersonaTransportAdmission, error) {
	inputRequired, err := coordinator.materializeNextEgressLocked(nowMS, nil)
	if err != nil {
		return controllerPersonaTransportAdmission{}, err
	}
	if inputRequired {
		messageSize, sizeErr := coordinator.engine.metadata.inputMessageSize()
		if sizeErr != nil {
			return controllerPersonaTransportAdmission{}, sizeErr
		}
		return controllerPersonaTransportAdmission{
			disposition: controllerPersonaTransportWait,
			waitReason:  controllerPersonaTransportInputRequired,
			action:      ControllerPersonaSendInitialInput,
			size:        messageSize,
		}, nil
	}
	if coordinator.pending.claim.Valid() {
		return coordinator.pendingWaitLocked(), nil
	}
	order, err := coordinator.allocateOrderLocked()
	if err != nil {
		return controllerPersonaTransportAdmission{}, err
	}
	claim, disposition, err := coordinator.engine.ReceiveHostMessage(
		nowMS, stage.wire[:stage.size])
	if err != nil {
		return controllerPersonaTransportAdmission{}, err
	}
	coordinator.activeResponse = controllerPersonaActiveResponse{
		ticket: stage.ticket, claim: claim, hasClaim: claim.Valid(),
		responseOrder: order, selectionOrder: order,
		hostDisposition: disposition,
	}
	action := ControllerPersonaAction(0)
	if claim.Valid() {
		action = claim.Action()
	}
	return controllerPersonaTransportAdmission{
		disposition: controllerPersonaTransportInterruptOutResponse,
		order:       order, action: action, hostDisposition: disposition,
	}, nil
}

func (coordinator *controllerPersonaTransportCoordinator) pendingWaitLocked() controllerPersonaTransportAdmission {
	reason := controllerPersonaTransportUpstreamPending
	disposition := controllerPersonaTransportWait
	if coordinator.pending.route == controllerPersonaTransportLocal {
		reason = controllerPersonaTransportLocalPending
		disposition = controllerPersonaTransportLocalRequired
	}
	return controllerPersonaTransportAdmission{
		disposition: disposition, waitReason: reason,
		order:  coordinator.pending.selectionOrder,
		action: coordinator.pending.claim.Action(),
		size:   coordinator.pending.claim.Size(),
	}
}

func (coordinator *controllerPersonaTransportCoordinator) materializeNextEgressLocked(
	nowMS uint64,
	latestInput *GamepadInputReportV1,
) (bool, error) {
	if coordinator.pending.claim.Valid() {
		return false, nil
	}
	if err := coordinator.materializeRetryLocked(nowMS); err != nil ||
		coordinator.pending.claim.Valid() {
		return false, err
	}
	lifecycle := coordinator.engine.lifecycle.Snapshot()
	if lifecycle.PendingTransition {
		if lifecycle.CurrentAction == ControllerLifecycleSendInitialInput &&
			latestInput == nil {
			return true, nil
		}
		selectionOrder, orderErr := coordinator.allocateOrderLocked()
		if orderErr != nil {
			return false, orderErr
		}
		if lifecycle.CurrentAction == ControllerLifecycleSendInitialInput {
			if err := coordinator.engine.ensureGIPUpstreamAvailable(); err != nil {
				if controllerPersonaTransportTemporarilyUnavailable(err) {
					return false, nil
				}
				return false, err
			}
			// The coordinator owns the engine's only mutation path after
			// construction. Refresh the source only at this final serialized
			// selection boundary; an earlier Wait therefore cannot freeze an
			// obsolete report into the mandatory START response.
			input := *latestInput
			if coordinator.inputJournal != nil {
				var err error
				var available bool
				input, available, err = coordinator.inputJournal.selectReport(true)
				if err != nil {
					return false, err
				}
				if !available {
					return false, ErrControllerPersonaInvariantViolation
				}
			}
			if err := coordinator.engine.SetCurrentInput(input); err != nil {
				return false, err
			}
		}
		claim, err := coordinator.engine.ClaimNextLifecycleAction(nowMS)
		if err == nil {
			return false, coordinator.setPendingLocked(
				claim, false, selectionOrder)
		}
		if controllerPersonaTransportTemporarilyUnavailable(err) {
			return false, nil
		}
		return false, err
	}
	if _, active := coordinator.engine.lifecycle.MetadataTransferFence(); active {
		metadata := coordinator.engine.metadataTransfer.Snapshot()
		ready := !coordinator.engine.metadataActive ||
			(!metadata.AwaitingAcknowledgement && !metadata.Done && !metadata.Faulted)
		if ready {
			selectionOrder, orderErr := coordinator.allocateOrderLocked()
			if orderErr != nil {
				return false, orderErr
			}
			claim, err := coordinator.engine.ClaimMetadataPacket(nowMS)
			if err == nil {
				return false, coordinator.setPendingLocked(
					claim, false, selectionOrder)
			}
			if controllerPersonaTransportTemporarilyUnavailable(err) {
				return false, nil
			}
			return false, err
		}
	}
	selectionOrder, err := coordinator.allocateOrderLocked()
	if err != nil {
		return false, err
	}
	claim, present, err := coordinator.engine.ClaimPoll(nowMS)
	if err == nil && present {
		return false, coordinator.setPendingLocked(claim, false, selectionOrder)
	}
	if err != nil && !controllerPersonaTransportTemporarilyUnavailable(err) {
		return false, err
	}
	return false, nil
}

func (coordinator *controllerPersonaTransportCoordinator) materializeRetryLocked(
	nowMS uint64,
) error {
	if coordinator.pending.claim.Valid() ||
		!coordinator.engine.Snapshot().RetryPending {
		return nil
	}
	selectionOrder, err := coordinator.allocateOrderLocked()
	if err != nil {
		return err
	}
	claim, err := coordinator.engine.ClaimRetry(nowMS)
	if err == nil {
		return coordinator.setPendingLocked(claim, false, selectionOrder)
	}
	// An ACK retry which reaches its exact deadline retires itself and faults
	// the reliable transfer. ClaimPoll, later in this same serialized boundary,
	// then owns selection of the ordinary failure Hello.
	if errors.Is(err, ErrReliableTransferTimeout) ||
		errors.Is(err, ErrMetadataTransferFaulted) {
		return nil
	}
	return err
}

func controllerPersonaTransportTemporarilyUnavailable(err error) bool {
	return errors.Is(err, ErrControllerPersonaUSBNotConfigured) ||
		errors.Is(err, ErrControllerPersonaEndpointHalted) ||
		errors.Is(err, ErrControllerPersonaUpstreamGated) ||
		errors.Is(err, ErrControllerPersonaDetached)
}

func (coordinator *controllerPersonaTransportCoordinator) setPendingLocked(
	claim ControllerPersonaClaim,
	fromHostCommand bool,
	selectionOrder uint64,
) error {
	if !claim.Valid() || coordinator.pending.claim.Valid() {
		return ErrInvalidControllerPersonaClaim
	}
	if claim.Action() == ControllerPersonaUSBControl || selectionOrder == 0 {
		return ErrInvalidControllerPersonaClaim
	}
	route := controllerPersonaTransportLocal
	if claim.Size() > 0 {
		route = controllerPersonaTransportUpstream
	}
	coordinator.pending = controllerPersonaPendingAction{
		claim: claim, route: route, selectionOrder: selectionOrder,
		fromHostCommand: fromHostCommand,
	}
	return nil
}

func (coordinator *controllerPersonaTransportCoordinator) setPendingGuideLocked(
	claim ControllerPersonaClaim,
	selectionOrder uint64,
) error {
	if err := coordinator.setPendingLocked(claim, false, selectionOrder); err != nil {
		return err
	}
	if claim.Action() != ControllerPersonaSendGuideButtonStatus || claim.Size() == 0 {
		coordinator.pending = controllerPersonaPendingAction{}
		return ErrInvalidControllerPersonaClaim
	}
	coordinator.pending.fromGuideEdge = true
	return nil
}

func (coordinator *controllerPersonaTransportCoordinator) allocateOrderLocked() (
	uint64,
	error,
) {
	if coordinator.nextOrder == ^uint64(0) {
		return 0, errControllerPersonaTransportTokenExhausted
	}
	coordinator.nextOrder++
	return coordinator.nextOrder, nil
}

// completeResponse is the post-write half of the same response-serializer
// transaction as admit. delivered must include any mandatory flush. Exactly
// one completion consumes the staged ticket.
func (coordinator *controllerPersonaTransportCoordinator) completeResponse(
	ticket controllerPersonaTransportTicket,
	delivered bool,
	nowMS uint64,
) error {
	if coordinator == nil {
		return errControllerPersonaTransportUninitialized
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	active := coordinator.activeResponse
	if !active.ticket.valid() || active.ticket != ticket {
		return errInvalidControllerPersonaTransportCompletion
	}
	stage, err := coordinator.validateStoredTicketLocked(ticket)
	if err != nil {
		return err
	}

	if active.hasClaim {
		switch ticket.lane {
		case controllerPersonaTransportControl:
			outcome := ControllerPersonaDeliveryFailed
			if delivered {
				outcome = ControllerPersonaDelivered
			}
			if err := coordinator.engine.Resolve(active.claim, outcome, nowMS); err != nil {
				return err
			}
			if delivered && coordinator.inputJournal != nil && retainedInputControlBoundary(stage.setup) {
				coordinator.inputJournal.boundary(coordinator.engine.hasLiveInputBaseline())
			}
			if delivered && active.claim.RequiresConfigurationLossClear() {
				clear, err := coordinator.engine.claimConfigurationLossClear(nowMS)
				if err != nil {
					return err
				}
				// The clear is a causally derived second half of this delivered
				// control transition, so it retains the control selection order.
				// admitLocal still allocates a distinct execution order.
				if err := coordinator.setPendingLocked(
					clear, false, active.selectionOrder); err != nil {
					return err
				}
			}
		case controllerPersonaTransportInterruptIn:
			outcome := ControllerPersonaDeferred
			if delivered {
				outcome = ControllerPersonaDelivered
			}
			if err := coordinator.engine.Resolve(active.claim, outcome, nowMS); err != nil {
				return err
			}
			if coordinator.inputJournal != nil && isRetainedInputAction(active.claim.Action()) {
				if err := coordinator.inputJournal.complete(delivered); err != nil {
					return err
				}
			}
		case controllerPersonaTransportInterruptOut:
			if delivered {
				route := controllerPersonaTransportLocal
				if active.claim.Size() > 0 {
					route = controllerPersonaTransportUpstream
				}
				coordinator.pending = controllerPersonaPendingAction{
					claim: active.claim, route: route,
					selectionOrder:  active.selectionOrder,
					fromHostCommand: true,
				}
			} else if err := coordinator.engine.Resolve(
				active.claim, ControllerPersonaDeferred, nowMS); err != nil {
				return err
			} else {
				// Transactional interrupt-OUT forbids an effect after its
				// acknowledgement fails. The engine correctly retains its
				// immutable non-EP0 retry, but the coordinator fences that retry
				// from IN/local execution until an authoritative generation
				// boundary retires it.
				coordinator.failedHost = controllerPersonaFailedHostAction{
					generation: active.claim.Generation(),
					action:     active.claim.Action(),
				}
			}
		default:
			return errInvalidControllerPersonaTransportCompletion
		}
	}
	*stage = controllerPersonaTransportStage{}
	coordinator.activeResponse = controllerPersonaActiveResponse{}
	return nil
}

// pendingLocal reports whether a completed response derived an immediate
// typed local obligation. It performs no selection or admission.
func (coordinator *controllerPersonaTransportCoordinator) pendingLocal() bool {
	if coordinator == nil {
		return false
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return (coordinator.pending.claim.Valid() &&
		coordinator.pending.route == controllerPersonaTransportLocal) ||
		coordinator.engine.ordinaryFeedbackRetryPending()
}

func (coordinator *controllerPersonaTransportCoordinator) pendingLocalClear() bool {
	if coordinator == nil {
		return false
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return coordinator.pendingLocalClearLocked()
}

func (coordinator *controllerPersonaTransportCoordinator) pendingLocalClearLocked() bool {
	return coordinator.pending.claim.Valid() &&
		coordinator.pending.route == controllerPersonaTransportLocal &&
		coordinator.pending.claim.Action() == ControllerPersonaClearOutputs
}

func (coordinator *controllerPersonaTransportCoordinator) clearRetryRequiredLocked() bool {
	return coordinator.engine != nil && coordinator.engine.retryPending &&
		coordinator.engine.retryRecord.action == ControllerPersonaClearOutputs &&
		coordinator.engine.retryRecord.clearEpoch != 0
}

// admitLocal transfers one exact zero-length persona claim to a local
// executor. It never performs the effect. The executor must later call
// completeLocal with its real delivered/deferred/failure/cancel-and-drain
// outcome.
func (coordinator *controllerPersonaTransportCoordinator) admitLocal(
	nowMS uint64,
) (controllerPersonaLocalLease, bool, error) {
	if coordinator == nil {
		return controllerPersonaLocalLease{}, false,
			errControllerPersonaTransportUninitialized
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.engine == nil {
		return controllerPersonaLocalLease{}, false,
			errControllerPersonaTransportUninitialized
	}
	if coordinator.activeResponse.ticket.valid() && !coordinator.engine.ordinaryFeedbackRetryPending() {
		return controllerPersonaLocalLease{}, false,
			errControllerPersonaTransportResponseOutstanding
	}
	if coordinator.activeLocal.lease.valid() {
		return controllerPersonaLocalLease{}, false,
			errControllerPersonaTransportLocalOutstanding
	}
	if coordinator.failedHost.generation != 0 {
		return controllerPersonaLocalLease{}, false,
			errControllerPersonaTransportBoundaryRequired
	}
	if coordinator.engine.ordinaryFeedbackRetryPending() {
		return coordinator.admitFeedbackRetryLocked(nowMS)
	}
	// Local execution is a consumer, never an ordinary selector. A response
	// serializer must first materialize the exact zero-length action and return
	// LocalRequired. This prevents a local worker's scheduling order from
	// preclaiming or freezing upstream response work.
	if !coordinator.pending.claim.Valid() ||
		coordinator.pending.route != controllerPersonaTransportLocal {
		return controllerPersonaLocalLease{}, false, nil
	}
	claim := coordinator.pending.claim
	if coordinator.nextLocalToken == ^uint64(0) {
		return controllerPersonaLocalLease{}, false,
			errControllerPersonaTransportTokenExhausted
	}
	coordinator.nextLocalToken++
	order, err := coordinator.allocateOrderLocked()
	if err != nil {
		return controllerPersonaLocalLease{}, false, err
	}
	if err := coordinator.engine.AdmitAndCopy(claim, nil, nowMS); err != nil {
		if claim.Action() == ControllerPersonaClearOutputs {
			// A failed final admission has performed no local effect. Preserve the
			// exact selected clear in place so no unrelated selector or generation
			// boundary can overtake it and the worker can retry it directly.
			return controllerPersonaLocalLease{}, false, err
		}
		resolveErr := coordinator.engine.Resolve(
			claim, ControllerPersonaDeferred, nowMS)
		coordinator.pending = controllerPersonaPendingAction{}
		if resolveErr != nil {
			return controllerPersonaLocalLease{}, false, fmt.Errorf(
				"persona local admission: %v; defer: %w", err, resolveErr)
		}
		return controllerPersonaLocalLease{}, false, err
	}
	lease := controllerPersonaLocalLease{
		owner: coordinator, token: coordinator.nextLocalToken,
		generation: claim.Generation(), order: order,
		action: claim.Action(), clearEpoch: claim.ClearEpoch(),
	}
	lease.directMotor, _ = claim.DirectMotor()
	lease.guideLED, _ = claim.GuideLED()
	detached := isOrdinaryFeedbackAction(claim.Action())
	if detached {
		if err := coordinator.engine.detachOrdinaryFeedback(claim); err != nil {
			resolveErr := coordinator.engine.Resolve(claim, ControllerPersonaDeferred, nowMS)
			coordinator.pending = controllerPersonaPendingAction{}
			return controllerPersonaLocalLease{}, false, errors.Join(err, resolveErr)
		}
	}
	coordinator.activeLocal = controllerPersonaActiveLocal{
		lease: lease, claim: claim, detachedFeedback: detached,
	}
	coordinator.pending = controllerPersonaPendingAction{}
	return lease, true, nil
}

// beginUSBReset is the authoritative lifecycle selector, not an ordinary
// response-selection path. It is the only recovery path in this coordinator
// for a failed interrupt-OUT fence: the predecessor retry is retired by
// ControllerPersonaEngine itself, generation advances, every staged endpoint
// ticket is atomically retired, and the successor clear-output action is
// routed to the local executor. No existing server lifecycle callback invokes
// this method.
func (coordinator *controllerPersonaTransportCoordinator) beginUSBReset(
	nowMS uint64,
) error {
	if coordinator == nil {
		return errControllerPersonaTransportUninitialized
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.engine == nil {
		return errControllerPersonaTransportUninitialized
	}
	if coordinator.activeResponse.ticket.valid() {
		return errControllerPersonaTransportResponseOutstanding
	}
	if coordinator.activeLocal.lease.valid() {
		return errControllerPersonaTransportLocalOutstanding
	}
	if coordinator.pendingLocalClearLocked() ||
		coordinator.clearRetryRequiredLocked() {
		// Reset may authoritatively retire ordinary unadmitted predecessor work,
		// but it may never replace an already-selected or retrying output release
		// with a new epoch.
		return ErrControllerPersonaBoundaryBlocked
	}
	selectionOrder, err := coordinator.allocateOrderLocked()
	if err != nil {
		return err
	}
	claim, err := coordinator.engine.BeginUSBReset(nowMS)
	if err != nil {
		return err
	}
	if coordinator.inputJournal != nil {
		coordinator.inputJournal.retire()
	}
	// BeginUSBReset has now authoritatively retired any ordinary unadmitted
	// predecessor claim. Clear its coordinator route only after that canonical
	// transition succeeds; failures leave both ledgers unchanged.
	coordinator.pending = controllerPersonaPendingAction{}
	if err := coordinator.setPendingLocked(
		claim, false, selectionOrder); err != nil {
		return err
	}
	// A successful authoritative generation boundary owns retirement of every
	// copied-but-unadmitted endpoint request. Their old tickets become invalid
	// immediately and cannot leave a successor-generation lane busy.
	coordinator.controlStage = controllerPersonaTransportStage{}
	coordinator.inStage = controllerPersonaTransportStage{}
	coordinator.outStage = controllerPersonaTransportStage{}
	coordinator.failedHost = controllerPersonaFailedHostAction{}
	return nil
}

// adoptRetainedUSBIPAddress publishes the address transition suppressed by
// the USB/IP host controller. It is valid only while every response, local
// action, staged endpoint request, and retry route is empty.
func (coordinator *controllerPersonaTransportCoordinator) adoptRetainedUSBIPAddress() error {
	if coordinator == nil {
		return errControllerPersonaTransportUninitialized
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.engine == nil {
		return errControllerPersonaTransportUninitialized
	}
	if coordinator.activeResponse.ticket.valid() ||
		coordinator.activeLocal.lease.valid() ||
		coordinator.pending.claim.Valid() ||
		coordinator.controlStage.ticket.valid() ||
		coordinator.inStage.ticket.valid() ||
		coordinator.outStage.ticket.valid() ||
		coordinator.failedHost.generation != 0 {
		return ErrControllerPersonaBoundaryBlocked
	}
	return coordinator.engine.adoptRetainedUSBIPAddress()
}

// beginDisconnect is the authoritative whole-import terminal selector. The
// retained USB scheduler and the device-local executor must already be
// cancelled and drained by the caller. Unlike an endpoint retirement, this
// advances the canonical persona generation, retires every unadmitted
// predecessor action and staged transport ticket, and exposes exactly the one
// clear-output obligation produced by ControllerPersonaEngine.BeginDisconnect.
// The clear is still only a pending local claim; a separately authenticated
// disconnect executor must admit and complete it before a successor may call
// reconnect.
func (coordinator *controllerPersonaTransportCoordinator) beginDisconnect(
	nowMS uint64,
) error {
	if coordinator == nil {
		return errControllerPersonaTransportUninitialized
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.engine == nil {
		return errControllerPersonaTransportUninitialized
	}
	if coordinator.activeResponse.ticket.valid() {
		return errControllerPersonaTransportResponseOutstanding
	}
	if coordinator.activeLocal.lease.valid() {
		return errControllerPersonaTransportLocalOutstanding
	}
	if coordinator.pendingLocalClearLocked() || coordinator.clearRetryRequiredLocked() {
		// A selected output release is a safety obligation, not ordinary stale
		// presentation work. Do not retire it into a successor-generation
		// disconnect clear: its exact same-generation epoch must first reach a
		// terminal delivered resolution.
		return ErrControllerPersonaBoundaryBlocked
	}
	selectionOrder, err := coordinator.allocateOrderLocked()
	if err != nil {
		return err
	}
	claim, err := coordinator.engine.BeginDisconnect(nowMS)
	if err != nil {
		return err
	}
	if !claim.Valid() || claim.Action() != ControllerPersonaClearOutputs ||
		claim.Size() != 0 {
		return ErrControllerPersonaInvariantViolation
	}
	if coordinator.inputJournal != nil {
		coordinator.inputJournal.retire()
	}
	coordinator.pending = controllerPersonaPendingAction{
		claim: claim, route: controllerPersonaTransportLocal,
		selectionOrder: selectionOrder,
	}
	coordinator.controlStage = controllerPersonaTransportStage{}
	coordinator.inStage = controllerPersonaTransportStage{}
	coordinator.outStage = controllerPersonaTransportStage{}
	coordinator.failedHost = controllerPersonaFailedHostAction{}
	return nil
}

// reconnect advances only a fully detached and cleared canonical persona into
// a fresh USB Default generation. It creates no clear action and accepts no
// staged predecessor work. The dormant retained adapter calls this only after
// its exact prior import reached DisconnectNeutral Safe.
func (coordinator *controllerPersonaTransportCoordinator) reconnect(
	nowMS uint64,
) error {
	if coordinator == nil {
		return errControllerPersonaTransportUninitialized
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.engine == nil {
		return errControllerPersonaTransportUninitialized
	}
	if coordinator.activeResponse.ticket.valid() ||
		coordinator.activeLocal.lease.valid() ||
		coordinator.pending.claim.Valid() ||
		coordinator.controlStage.ticket.valid() ||
		coordinator.inStage.ticket.valid() ||
		coordinator.outStage.ticket.valid() ||
		coordinator.failedHost.generation != 0 {
		return ErrControllerPersonaBoundaryBlocked
	}
	if err := coordinator.engine.Reconnect(nowMS); err != nil {
		return err
	}
	if coordinator.inputJournal != nil {
		coordinator.inputJournal.retire()
	}
	return nil
}

func (coordinator *controllerPersonaTransportCoordinator) completeLocal(
	lease controllerPersonaLocalLease,
	outcome ControllerPersonaOutcome,
	nowMS uint64,
) error {
	if coordinator == nil {
		return errControllerPersonaTransportUninitialized
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	active := coordinator.activeLocal
	if !lease.valid() || active.lease != lease || lease.owner != coordinator {
		return errInvalidControllerPersonaTransportCompletion
	}
	if outcome < ControllerPersonaDelivered ||
		outcome > ControllerPersonaExecutionCancelled {
		return errInvalidControllerPersonaTransportCompletion
	}
	retryClear := outcome != ControllerPersonaDelivered &&
		active.claim.Action() == ControllerPersonaClearOutputs
	selectionOrder := uint64(0)
	if retryClear {
		// Reserve the coordinator order before resolving the admitted lease. A
		// failure must not leave the mandatory clear in an engine-only retry
		// state which requires an unrelated host request to rediscover it.
		var err error
		selectionOrder, err = coordinator.allocateOrderLocked()
		if err != nil {
			return err
		}
	}
	var resolveErr error
	if active.detachedFeedback {
		resolveErr = coordinator.engine.completeOrdinaryFeedback(active.claim, outcome, nowMS)
	} else {
		resolveErr = coordinator.engine.Resolve(active.claim, outcome, nowMS)
	}
	if resolveErr != nil {
		return resolveErr
	}
	if coordinator.inputJournal != nil && outcome == ControllerPersonaDelivered {
		switch active.claim.Action() {
		case ControllerPersonaPermitNormalUpstream:
			if coordinator.engine.hasLiveInputBaseline() {
				coordinator.inputJournal.permit()
			}
		case ControllerPersonaGateNormalUpstream, ControllerPersonaPerformReset, ControllerPersonaCompletePowerOff:
			coordinator.inputJournal.retire()
		}
	}
	if retryClear {
		retry, err := coordinator.engine.ClaimRetry(nowMS)
		if err != nil {
			return err
		}
		if retry.Action() != active.claim.Action() ||
			retry.Generation() != active.claim.Generation() || retry.Size() != 0 ||
			retry.ClearEpoch() == 0 ||
			retry.ClearEpoch() != active.claim.ClearEpoch() {
			return ErrControllerPersonaInvariantViolation
		}
		if err := coordinator.setPendingLocked(
			retry, false, selectionOrder); err != nil {
			return err
		}
	}
	coordinator.activeLocal = controllerPersonaActiveLocal{}
	return nil
}

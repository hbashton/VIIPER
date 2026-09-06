package xboxone

import "fmt"

const (
	HelloIntervalMilliseconds         uint64 = 500
	PowerTransitionDelayMilliseconds  uint64 = 500
	messageNumberMetadataRequest      uint8  = 4
	messageNumberSetDeviceState       uint8  = 5
	messageNumberSecurityData         uint8  = 6
	extendedSetDeviceStatePayloadSize        = 15
	securityDataCompletePayloadSize          = 2
)

// SetDeviceStateValue is the one-byte state field from table 40.
type SetDeviceStateValue uint8

const (
	SetDeviceStateStart     SetDeviceStateValue = 0x00
	SetDeviceStateStop      SetDeviceStateValue = 0x01
	SetDeviceStateFullPower SetDeviceStateValue = 0x03
	SetDeviceStateOff       SetDeviceStateValue = 0x04
	SetDeviceStateQuiesce   SetDeviceStateValue = 0x05
	SetDeviceStateReset     SetDeviceStateValue = 0x07
)

func (state SetDeviceStateValue) validate() error {
	switch state {
	case SetDeviceStateStart, SetDeviceStateStop, SetDeviceStateFullPower,
		SetDeviceStateOff, SetDeviceStateQuiesce, SetDeviceStateReset:
		return nil
	default:
		return fmt.Errorf("%w: 0x%02x", ErrReservedDeviceState, state)
	}
}

// ControllerHostCommandKind identifies the byte-defined startup commands.
type ControllerHostCommandKind uint8

const (
	ControllerHostCommandMetadataRequest ControllerHostCommandKind = iota + 1
	ControllerHostCommandSetDeviceState
	// ControllerHostCommandExtendedSetDeviceStateInitialization identifies
	// the exact 15-byte compatibility frame sent during Windows and SDL GIP
	// initialization. Its body is not a documented lifecycle state, so it is
	// acknowledged without changing controller state; a separate one-byte
	// Set Device State: Start remains mandatory.
	ControllerHostCommandExtendedSetDeviceStateInitialization
	// ControllerHostCommandSecurityDataComplete identifies the exact
	// two-byte completion marker the Windows host sends after succeeding the
	// documented Windows-PC security opt-out. It carries no authentication
	// payload and has no controller-lifecycle effect.
	ControllerHostCommandSecurityDataComplete
)

// ControllerHostCommand is a decoded primary-controller startup command.
type ControllerHostCommand struct {
	Kind     ControllerHostCommandKind
	Sequence uint8
	State    SetDeviceStateValue
}

func (command ControllerHostCommand) validate() error {
	if command.Sequence == 0 {
		return ErrReservedSequence
	}
	switch command.Kind {
	case ControllerHostCommandMetadataRequest:
		if command.Sequence != 1 {
			return fmt.Errorf("%w: metadata request sequence=%d",
				ErrUnsupportedHostCommand, command.Sequence)
		}
		return nil
	case ControllerHostCommandSetDeviceState:
		return command.State.validate()
	case ControllerHostCommandExtendedSetDeviceStateInitialization:
		if command.State != 0 {
			return ErrUnsupportedSetDeviceStateVariant
		}
		return nil
	case ControllerHostCommandSecurityDataComplete:
		if command.State != 0 {
			return ErrUnsupportedHostCommand
		}
		return nil
	default:
		return fmt.Errorf("%w: kind=%d", ErrUnsupportedHostCommand, command.Kind)
	}
}

// DecodeControllerHostCommand decodes one exact, uncoalesced primary-device
// Metadata Request, one-byte Set Device State message, the exact 15-byte
// extended initialization frame observed from Windows and implemented by SDL,
// or the exact two-byte Security Data Complete marker documented by Microsoft
// and emitted by Windows after the PC security opt-out. Every other Security
// message and 15-byte state variant remains unsupported.
func DecodeControllerHostCommand(wire []byte) (ControllerHostCommand, error) {
	if len(wire) < SinglePacketHeaderSize {
		return ControllerHostCommand{}, exactLengthError(
			"controller host command header", len(wire), SinglePacketHeaderSize)
	}
	header, err := DecodeSinglePacketHeader(wire[:SinglePacketHeaderSize])
	if err != nil {
		return ControllerHostCommand{}, err
	}
	if len(wire) != SinglePacketHeaderSize+int(header.PayloadLength) {
		return ControllerHostCommand{}, exactLengthError(
			"controller host command", len(wire),
			SinglePacketHeaderSize+int(header.PayloadLength))
	}
	if header.DataClass != DataClassCommand || !header.System ||
		header.AcknowledgementRequested || header.ExpansionIndex != 0 {
		return ControllerHostCommand{}, ErrUnsupportedHostCommand
	}

	var command ControllerHostCommand
	switch header.MessageNumber {
	case messageNumberMetadataRequest:
		if header.PayloadLength != 0 || header.Sequence != 1 {
			return ControllerHostCommand{}, ErrUnsupportedHostCommand
		}
		command = ControllerHostCommand{
			Kind: ControllerHostCommandMetadataRequest, Sequence: header.Sequence,
		}
	case messageNumberSetDeviceState:
		if header.PayloadLength == extendedSetDeviceStatePayloadSize {
			if !isExtendedSetDeviceStateInitialization(
				wire[SinglePacketHeaderSize:]) {
				return ControllerHostCommand{}, ErrUnsupportedSetDeviceStateVariant
			}
			command = ControllerHostCommand{
				Kind:     ControllerHostCommandExtendedSetDeviceStateInitialization,
				Sequence: header.Sequence,
			}
			break
		}
		if header.PayloadLength != 1 {
			return ControllerHostCommand{}, ErrUnsupportedHostCommand
		}
		command = ControllerHostCommand{
			Kind:     ControllerHostCommandSetDeviceState,
			Sequence: header.Sequence,
			State:    SetDeviceStateValue(wire[SinglePacketHeaderSize]),
		}
	case messageNumberSecurityData:
		if header.PayloadLength != securityDataCompletePayloadSize ||
			!isSecurityDataComplete(wire[SinglePacketHeaderSize:]) {
			return ControllerHostCommand{}, ErrUnsupportedHostCommand
		}
		command = ControllerHostCommand{
			Kind:     ControllerHostCommandSecurityDataComplete,
			Sequence: header.Sequence,
		}
	default:
		return ControllerHostCommand{}, ErrUnsupportedHostCommand
	}
	if err := command.validate(); err != nil {
		return ControllerHostCommand{}, err
	}
	return command, nil
}

// isExtendedSetDeviceStateInitialization recognizes only the compatibility
// body captured from the Windows Xbox GIP driver and independently present in
// SDL's GIP initialization sequence. The two ASCII bytes spell "US"; their
// meaning and every other reserved byte remain intentionally uninterpreted.
func isExtendedSetDeviceStateInitialization(payload []byte) bool {
	if len(payload) != extendedSetDeviceStatePayloadSize {
		return false
	}
	expected := [...]byte{
		0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x55,
		0x53, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	for index := range expected {
		if payload[index] != expected[index] {
			return false
		}
	}
	return true
}

// isSecurityDataComplete recognizes only the table-49 Security Data Complete
// body sent by Windows after it succeeds the Windows-PC opt-out. Linux xpad's
// working Xbox One initialization independently emits the same 01 00 body as
// xboxone_auth_done. It is not an authentication-data decoder.
func isSecurityDataComplete(payload []byte) bool {
	return len(payload) == securityDataCompletePayloadSize &&
		payload[0] == 0x01 && payload[1] == 0x00
}

// ControllerLifecycleState is the controller-only subset of the GIP state
// diagram. Metadata is explicit even though the specification models it as an
// internal Idle sub-state.
type ControllerLifecycleState uint8

const (
	ControllerLifecycleUninitialized ControllerLifecycleState = iota
	ControllerLifecycleArrival
	ControllerLifecycleMetadata
	ControllerLifecycleIdle
	ControllerLifecycleActive
	ControllerLifecycleTerminatingOff
	ControllerLifecycleTerminatingReset
	ControllerLifecycleOff
	ControllerLifecycleReset
)

// ControllerLifecycleAction is one ordered, backend-independent obligation.
type ControllerLifecycleAction uint8

const (
	ControllerLifecycleSendHello ControllerLifecycleAction = iota + 1
	ControllerLifecycleBeginMetadata
	ControllerLifecycleSendCurrentStatus
	ControllerLifecycleSendInitialInput
	ControllerLifecyclePermitNormalUpstream
	ControllerLifecycleGateNormalUpstream
	ControllerLifecycleClearOutputs
	ControllerLifecycleSendPoweringOffStatus
	ControllerLifecycleCompletePowerOff
	ControllerLifecyclePerformReset
)

// ControllerLifecycleDecision is the private fixed-capacity transition plan.
// Only the current action is ever exposed as an executable claim.
type ControllerLifecycleDecision struct {
	actions [3]ControllerLifecycleAction
	length  uint8
}

func (decision ControllerLifecycleDecision) Len() int { return int(decision.length) }
func (decision ControllerLifecycleDecision) Action(index int) (ControllerLifecycleAction, bool) {
	if index < 0 || index >= int(decision.length) {
		return 0, false
	}
	return decision.actions[index], true
}

func lifecycleDecision1(first ControllerLifecycleAction) ControllerLifecycleDecision {
	return ControllerLifecycleDecision{actions: [3]ControllerLifecycleAction{first}, length: 1}
}
func lifecycleDecision2(first, second ControllerLifecycleAction) ControllerLifecycleDecision {
	return ControllerLifecycleDecision{
		actions: [3]ControllerLifecycleAction{first, second}, length: 2,
	}
}
func lifecycleDecision3(first, second, third ControllerLifecycleAction) ControllerLifecycleDecision {
	return ControllerLifecycleDecision{
		actions: [3]ControllerLifecycleAction{first, second, third}, length: 3,
	}
}

// ControllerLifecycleOutcome reports one action's external result. Deferred
// and DeliveryFailed assert that no late external effect remains possible;
// an admitted asynchronous attempt instead uses ExecutionCancelled only after
// synchronous cancellation and drain.
type ControllerLifecycleOutcome uint8

const (
	ControllerLifecycleDelivered ControllerLifecycleOutcome = iota + 1
	ControllerLifecycleDeferred
	ControllerLifecycleDeliveryFailed
	// ControllerLifecycleExecutionCancelled is valid only for an admitted
	// action after its executor has synchronously cancelled and drained all
	// work, guaranteeing that no late external effect can still occur.
	ControllerLifecycleExecutionCancelled
)

// ControllerLifecycleClaim is an opaque capability for exactly one action at
// one cursor in a pending transition.
type ControllerLifecycleClaim struct {
	owner                      *ControllerLifecycle
	token                      uint64
	generation                 uint64
	transitionEpoch            uint64
	metadataTransferGeneration uint64
	action                     ControllerLifecycleAction
	cursor                     uint8
	totalActions               uint8
	selectedAtMS               uint64
}

func (claim ControllerLifecycleClaim) Valid() bool                       { return claim.owner != nil && claim.token != 0 }
func (claim ControllerLifecycleClaim) Generation() uint64                { return claim.generation }
func (claim ControllerLifecycleClaim) TransitionEpoch() uint64           { return claim.transitionEpoch }
func (claim ControllerLifecycleClaim) Action() ControllerLifecycleAction { return claim.action }
func (claim ControllerLifecycleClaim) Cursor() int                       { return int(claim.cursor) }
func (claim ControllerLifecycleClaim) TotalActions() int                 { return int(claim.totalActions) }
func (claim ControllerLifecycleClaim) SelectedAtMilliseconds() uint64    { return claim.selectedAtMS }

// ControllerLifecycleExecutionFence is the backend-neutral identity of one
// action attempt. A successful Admit converts the matching claim into an
// exclusive execution lease; reset cannot begin until that lease is delivered,
// failed without a late effect, or synchronously cancelled and drained.
type ControllerLifecycleExecutionFence struct {
	TransportGeneration uint64
	TransitionEpoch     uint64
	LeaseToken          uint64
	ActionCursor        uint8
	Action              ControllerLifecycleAction
}

// ExecutionFence returns the local identity an asynchronous executor must
// carry through completion/cancellation. It is transaction context, not wire
// data.
func (claim ControllerLifecycleClaim) ExecutionFence() (ControllerLifecycleExecutionFence, bool) {
	if !claim.Valid() || claim.generation == 0 || claim.transitionEpoch == 0 {
		return ControllerLifecycleExecutionFence{}, false
	}
	return ControllerLifecycleExecutionFence{
		TransportGeneration: claim.generation,
		TransitionEpoch:     claim.transitionEpoch,
		LeaseToken:          claim.token,
		ActionCursor:        claim.cursor,
		Action:              claim.action,
	}, true
}

// MetadataTransferGeneration returns the exact transfer fence only for a
// BeginMetadata action claim.
func (claim ControllerLifecycleClaim) MetadataTransferGeneration() (uint64, bool) {
	return claim.metadataTransferGeneration, claim.metadataTransferGeneration != 0
}

// ControllerMetadataTransferFence binds an asynchronous metadata completion
// to both the USB transport incarnation and the never-reused local metadata
// transfer allocation. Neither field is an MS-GIPUSB wire field.
type ControllerMetadataTransferFence struct {
	TransportGeneration uint64
	TransferGeneration  uint64
}

// MetadataTransferFence returns the callback fence only for a BeginMetadata
// action claim.
func (claim ControllerLifecycleClaim) MetadataTransferFence() (ControllerMetadataTransferFence, bool) {
	if claim.generation == 0 || claim.metadataTransferGeneration == 0 {
		return ControllerMetadataTransferFence{}, false
	}
	return ControllerMetadataTransferFence{
		TransportGeneration: claim.generation,
		TransferGeneration:  claim.metadataTransferGeneration,
	}, true
}

type lifecycleDeadlinePolicy uint8

const (
	lifecycleNoDeadline lifecycleDeadlinePolicy = iota
	lifecycleHelloAfterDelivery
	lifecycleTerminationAfterDelivery
)

type controllerLifecycleCore struct {
	state                      ControllerLifecycleState
	generation                 uint64
	metadataTransferGeneration uint64
	nextHelloMS                uint64
	terminationMS              uint64
}

type controllerLifecycleTransition struct {
	decision ControllerLifecycleDecision
	target   controllerLifecycleCore
	deadline lifecycleDeadlinePolicy
}

// ControllerLifecycleSnapshot exposes committed state and the explicit
// pending transition cursor.
type ControllerLifecycleSnapshot struct {
	State                      ControllerLifecycleState
	Generation                 uint64
	MetadataTransferGeneration uint64
	PendingTransition          bool
	TransitionEpoch            uint64
	ActionCursor               uint8
	ActionCount                uint8
	CurrentAction              ControllerLifecycleAction
	ClaimOutstanding           bool
	ClaimAdmitted              bool
	RetryPending               bool
}

// ControllerLifecycle is a pure serialized state machine. It never sends
// packets, sleeps, registers USB, or accesses hardware. It must not be copied
// after its first claim because action claims are bound to its address.
type ControllerLifecycle struct {
	core      controllerLifecycleCore
	lastNowMS uint64

	nextTransitionEpoch            uint64
	nextMetadataTransferGeneration uint64
	pendingTransition              bool
	transitionEpoch                uint64
	transition                     controllerLifecycleTransition
	actionCursor                   uint8
	actionSelectedAtMS             uint64
	retryPending                   bool

	nextToken     uint64
	hasClaim      bool
	claimAdmitted bool
	claimToken    uint64
}

// NewControllerLifecycle begins generation one in Arrival with Hello due.
func NewControllerLifecycle(nowMS uint64) ControllerLifecycle {
	return ControllerLifecycle{
		core: controllerLifecycleCore{
			state: ControllerLifecycleArrival, generation: 1, nextHelloMS: nowMS,
		},
		lastNowMS: nowMS,
	}
}

func (lifecycle ControllerLifecycle) State() ControllerLifecycleState { return lifecycle.core.state }
func (lifecycle ControllerLifecycle) Generation() uint64              { return lifecycle.core.generation }

func (lifecycle ControllerLifecycle) Snapshot() ControllerLifecycleSnapshot {
	snapshot := ControllerLifecycleSnapshot{
		State: lifecycle.core.state, Generation: lifecycle.core.generation,
		MetadataTransferGeneration: lifecycle.core.metadataTransferGeneration,
		PendingTransition:          lifecycle.pendingTransition,
		TransitionEpoch:            lifecycle.transitionEpoch,
		ActionCursor:               lifecycle.actionCursor,
		ClaimOutstanding:           lifecycle.hasClaim, ClaimAdmitted: lifecycle.claimAdmitted,
		RetryPending: lifecycle.retryPending,
	}
	if lifecycle.pendingTransition {
		snapshot.ActionCount = lifecycle.transition.decision.length
		snapshot.CurrentAction, _ = lifecycle.transition.decision.Action(int(lifecycle.actionCursor))
	}
	return snapshot
}

// MetadataTransferGeneration returns the exact active metadata-request fence.
func (lifecycle ControllerLifecycle) MetadataTransferGeneration() (uint64, bool) {
	if lifecycle.core.state != ControllerLifecycleMetadata ||
		lifecycle.core.metadataTransferGeneration == 0 || lifecycle.pendingTransition {
		return 0, false
	}
	return lifecycle.core.metadataTransferGeneration, true
}

// MetadataTransferFence returns the exact active metadata callback fence.
func (lifecycle ControllerLifecycle) MetadataTransferFence() (ControllerMetadataTransferFence, bool) {
	transferGeneration, ok := lifecycle.MetadataTransferGeneration()
	if !ok {
		return ControllerMetadataTransferFence{}, false
	}
	return ControllerMetadataTransferFence{
		TransportGeneration: lifecycle.core.generation,
		TransferGeneration:  transferGeneration,
	}, true
}

func (lifecycle *ControllerLifecycle) validateInitialized() error {
	if lifecycle.core.state == ControllerLifecycleUninitialized || lifecycle.core.generation == 0 {
		return ErrUninitializedLifecycle
	}
	return nil
}

func (lifecycle *ControllerLifecycle) validateClock(nowMS uint64) error {
	if nowMS < lifecycle.lastNowMS {
		return fmt.Errorf("%w: now=%d previous=%d",
			ErrNonMonotonicLifecycleClock, nowMS, lifecycle.lastNowMS)
	}
	return nil
}

func (lifecycle *ControllerLifecycle) validateNoTransition(nowMS uint64) error {
	if err := lifecycle.validateInitialized(); err != nil {
		return err
	}
	if err := lifecycle.validateClock(nowMS); err != nil {
		return err
	}
	if lifecycle.pendingTransition {
		return ErrLifecycleTransitionPending
	}
	return nil
}

func saturatingLifecycleDeadline(nowMS, deltaMS uint64) uint64 {
	if ^uint64(0)-nowMS < deltaMS {
		return ^uint64(0)
	}
	return nowMS + deltaMS
}

func (lifecycle *ControllerLifecycle) allocateTransitionEpoch() (uint64, error) {
	if lifecycle.nextTransitionEpoch == ^uint64(0) {
		return 0, ErrInvalidTransferEpoch
	}
	lifecycle.nextTransitionEpoch++
	return lifecycle.nextTransitionEpoch, nil
}

func (lifecycle *ControllerLifecycle) nextClaimToken() uint64 {
	lifecycle.nextToken++
	if lifecycle.nextToken == 0 {
		lifecycle.nextToken++
	}
	return lifecycle.nextToken
}

func (lifecycle *ControllerLifecycle) currentAction() ControllerLifecycleAction {
	action, _ := lifecycle.transition.decision.Action(int(lifecycle.actionCursor))
	return action
}

func (lifecycle *ControllerLifecycle) claimCurrentAction(
	nowMS uint64,
	preserveSelectionTime bool,
) ControllerLifecycleClaim {
	if !preserveSelectionTime {
		lifecycle.actionSelectedAtMS = nowMS
	}
	token := lifecycle.nextClaimToken()
	lifecycle.lastNowMS = nowMS
	lifecycle.hasClaim = true
	lifecycle.claimAdmitted = false
	lifecycle.claimToken = token
	action := lifecycle.currentAction()
	var metadataGeneration uint64
	if action == ControllerLifecycleBeginMetadata {
		metadataGeneration = lifecycle.transition.target.metadataTransferGeneration
	}
	return ControllerLifecycleClaim{
		owner: lifecycle, token: token, generation: lifecycle.core.generation,
		transitionEpoch:            lifecycle.transitionEpoch,
		metadataTransferGeneration: metadataGeneration,
		action:                     action, cursor: lifecycle.actionCursor,
		totalActions: lifecycle.transition.decision.length,
		selectedAtMS: lifecycle.actionSelectedAtMS,
	}
}

func (lifecycle *ControllerLifecycle) beginTransition(
	transition controllerLifecycleTransition,
	nowMS uint64,
) (ControllerLifecycleClaim, error) {
	var metadataReservation uint64
	firstAction, _ := transition.decision.Action(0)
	if firstAction == ControllerLifecycleBeginMetadata {
		if lifecycle.nextMetadataTransferGeneration == ^uint64(0) ||
			transition.target.metadataTransferGeneration !=
				lifecycle.nextMetadataTransferGeneration+1 {
			return ControllerLifecycleClaim{}, ErrInvalidTransferGeneration
		}
		metadataReservation = transition.target.metadataTransferGeneration
	}
	epoch, err := lifecycle.allocateTransitionEpoch()
	if err != nil {
		return ControllerLifecycleClaim{}, err
	}
	if metadataReservation != 0 {
		// Burn the allocation before its claim can escape. An authoritative
		// reset may discard the pending target, but can never make this callback
		// identity available to a later metadata transfer.
		lifecycle.nextMetadataTransferGeneration = metadataReservation
	}
	lifecycle.pendingTransition = true
	lifecycle.transitionEpoch = epoch
	lifecycle.transition = transition
	lifecycle.actionCursor = 0
	lifecycle.actionSelectedAtMS = 0
	lifecycle.retryPending = false
	return lifecycle.claimCurrentAction(nowMS, false), nil
}

// ClaimPoll starts one due Hello or terminal transition. A false boolean means
// no transition was due and only the clock observation committed.
func (lifecycle *ControllerLifecycle) ClaimPoll(
	nowMS uint64,
) (ControllerLifecycleClaim, bool, error) {
	if err := lifecycle.validateNoTransition(nowMS); err != nil {
		return ControllerLifecycleClaim{}, false, err
	}
	transition := controllerLifecycleTransition{target: lifecycle.core}
	switch lifecycle.core.state {
	case ControllerLifecycleArrival:
		if nowMS < lifecycle.core.nextHelloMS {
			lifecycle.lastNowMS = nowMS
			return ControllerLifecycleClaim{}, false, nil
		}
		transition.decision = lifecycleDecision1(ControllerLifecycleSendHello)
		transition.deadline = lifecycleHelloAfterDelivery
	case ControllerLifecycleTerminatingOff:
		if nowMS < lifecycle.core.terminationMS {
			lifecycle.lastNowMS = nowMS
			return ControllerLifecycleClaim{}, false, nil
		}
		transition.decision = lifecycleDecision1(ControllerLifecycleCompletePowerOff)
		transition.target.state = ControllerLifecycleOff
		transition.target.terminationMS = 0
	case ControllerLifecycleTerminatingReset:
		if nowMS < lifecycle.core.terminationMS {
			lifecycle.lastNowMS = nowMS
			return ControllerLifecycleClaim{}, false, nil
		}
		transition.decision = lifecycleDecision1(ControllerLifecyclePerformReset)
		transition.target.state = ControllerLifecycleReset
		transition.target.terminationMS = 0
	default:
		lifecycle.lastNowMS = nowMS
		return ControllerLifecycleClaim{}, false, nil
	}
	claim, err := lifecycle.beginTransition(transition, nowMS)
	return claim, err == nil, err
}

// ClaimHostWire decodes and starts one exact startup-command transition.
func (lifecycle *ControllerLifecycle) ClaimHostWire(
	nowMS uint64,
	wire []byte,
) (ControllerLifecycleClaim, bool, error) {
	command, err := DecodeControllerHostCommand(wire)
	if err != nil {
		return ControllerLifecycleClaim{}, false, err
	}
	return lifecycle.ClaimHostCommand(nowMS, command)
}

// ClaimHostCommand starts a transition and claims only its first action.
func (lifecycle *ControllerLifecycle) ClaimHostCommand(
	nowMS uint64,
	command ControllerHostCommand,
) (ControllerLifecycleClaim, bool, error) {
	if err := command.validate(); err != nil {
		return ControllerLifecycleClaim{}, false, err
	}
	if err := lifecycle.validateNoTransition(nowMS); err != nil {
		return ControllerLifecycleClaim{}, false, err
	}
	transition, present, err := lifecycle.selectHostCommand(command)
	if err != nil {
		return ControllerLifecycleClaim{}, false, err
	}
	if !present {
		lifecycle.lastNowMS = nowMS
		return ControllerLifecycleClaim{}, false, nil
	}
	claim, err := lifecycle.beginTransition(transition, nowMS)
	return claim, err == nil, err
}

func (lifecycle *ControllerLifecycle) selectHostCommand(
	command ControllerHostCommand,
) (controllerLifecycleTransition, bool, error) {
	transition := controllerLifecycleTransition{target: lifecycle.core}
	if command.Kind == ControllerHostCommandExtendedSetDeviceStateInitialization {
		switch lifecycle.core.state {
		case ControllerLifecycleArrival, ControllerLifecycleMetadata,
			ControllerLifecycleIdle:
			// This is a startup compatibility probe, not state 6. Preserve the
			// entire lifecycle and wait for the ordinary one-byte START.
			return transition, false, nil
		default:
			return controllerLifecycleTransition{}, false, ErrUnexpectedLifecycleCommand
		}
	}
	if command.Kind == ControllerHostCommandSecurityDataComplete {
		if lifecycle.core.state == ControllerLifecycleActive {
			// Windows has completed the opted-out exchange. Preserve every
			// lifecycle field; there is no device response or state transition.
			return transition, false, nil
		}
		return controllerLifecycleTransition{}, false, ErrUnexpectedLifecycleCommand
	}
	if lifecycle.core.state == ControllerLifecycleTerminatingOff ||
		lifecycle.core.state == ControllerLifecycleTerminatingReset {
		if command.Kind == ControllerHostCommandSetDeviceState &&
			(command.State == SetDeviceStateOff || command.State == SetDeviceStateStop ||
				command.State == SetDeviceStateReset) {
			if lifecycle.core.state == ControllerLifecycleTerminatingOff {
				transition.target.state = ControllerLifecycleOff
				transition.decision = lifecycleDecision1(ControllerLifecycleCompletePowerOff)
			} else {
				transition.target.state = ControllerLifecycleReset
				transition.decision = lifecycleDecision1(ControllerLifecyclePerformReset)
			}
			transition.target.terminationMS = 0
			return transition, true, nil
		}
		return controllerLifecycleTransition{}, false, ErrUnexpectedLifecycleCommand
	}

	switch lifecycle.core.state {
	case ControllerLifecycleArrival:
		switch command.Kind {
		case ControllerHostCommandMetadataRequest:
			transition, err := lifecycle.metadataTransition(transition)
			return transition, err == nil, err
		case ControllerHostCommandSetDeviceState:
			switch command.State {
			case SetDeviceStateStart:
				return lifecycle.startTransition(transition), true, nil
			case SetDeviceStateOff, SetDeviceStateReset:
				return lifecycle.terminationTransition(transition, command.State), true, nil
			}
		}
	case ControllerLifecycleMetadata:
		if command.Kind == ControllerHostCommandMetadataRequest {
			transition, err := lifecycle.metadataTransition(transition)
			return transition, err == nil, err
		}
		if command.Kind == ControllerHostCommandSetDeviceState &&
			(command.State == SetDeviceStateOff || command.State == SetDeviceStateReset) {
			return lifecycle.terminationTransition(transition, command.State), true, nil
		}
	case ControllerLifecycleIdle:
		switch command.Kind {
		case ControllerHostCommandMetadataRequest:
			transition, err := lifecycle.metadataTransition(transition)
			return transition, err == nil, err
		case ControllerHostCommandSetDeviceState:
			switch command.State {
			case SetDeviceStateStart:
				return lifecycle.startTransition(transition), true, nil
			case SetDeviceStateOff, SetDeviceStateReset:
				return lifecycle.terminationTransition(transition, command.State), true, nil
			}
		}
	case ControllerLifecycleActive:
		if command.Kind == ControllerHostCommandSetDeviceState {
			switch command.State {
			case SetDeviceStateStop:
				transition.target.state = ControllerLifecycleIdle
				transition.decision = lifecycleDecision2(
					ControllerLifecycleGateNormalUpstream, ControllerLifecycleClearOutputs)
				return transition, true, nil
			case SetDeviceStateFullPower:
				return transition, false, nil
			case SetDeviceStateQuiesce:
				transition.decision = lifecycleDecision1(ControllerLifecycleClearOutputs)
				return transition, true, nil
			case SetDeviceStateOff, SetDeviceStateReset:
				return lifecycle.terminationTransition(transition, command.State), true, nil
			}
		}
	case ControllerLifecycleOff, ControllerLifecycleReset:
		return controllerLifecycleTransition{}, false, ErrUnexpectedLifecycleCommand
	case ControllerLifecycleUninitialized:
		return controllerLifecycleTransition{}, false, ErrUninitializedLifecycle
	}
	return controllerLifecycleTransition{}, false, ErrUnexpectedLifecycleCommand
}

func (lifecycle *ControllerLifecycle) metadataTransition(
	transition controllerLifecycleTransition,
) (controllerLifecycleTransition, error) {
	if lifecycle.nextMetadataTransferGeneration == ^uint64(0) {
		return controllerLifecycleTransition{}, ErrInvalidTransferGeneration
	}
	transition.target.metadataTransferGeneration = lifecycle.nextMetadataTransferGeneration + 1
	transition.target.state = ControllerLifecycleMetadata
	transition.target.nextHelloMS = 0
	transition.decision = lifecycleDecision1(ControllerLifecycleBeginMetadata)
	return transition, nil
}

func (lifecycle *ControllerLifecycle) startTransition(
	transition controllerLifecycleTransition,
) controllerLifecycleTransition {
	transition.target.state = ControllerLifecycleActive
	transition.target.nextHelloMS = 0
	transition.decision = lifecycleDecision3(
		ControllerLifecycleSendCurrentStatus,
		ControllerLifecycleSendInitialInput,
		ControllerLifecyclePermitNormalUpstream)
	return transition
}

func (lifecycle *ControllerLifecycle) terminationTransition(
	transition controllerLifecycleTransition,
	state SetDeviceStateValue,
) controllerLifecycleTransition {
	if state == SetDeviceStateOff {
		transition.target.state = ControllerLifecycleTerminatingOff
	} else {
		transition.target.state = ControllerLifecycleTerminatingReset
	}
	transition.target.nextHelloMS = 0
	transition.deadline = lifecycleTerminationAfterDelivery
	transition.decision = lifecycleDecision3(
		ControllerLifecycleGateNormalUpstream,
		ControllerLifecycleClearOutputs,
		ControllerLifecycleSendPoweringOffStatus)
	return transition
}

// MetadataTransferSucceeded commits the output-free transition to Idle only
// for the exact current metadata request generation.
func (lifecycle *ControllerLifecycle) MetadataTransferSucceeded(
	nowMS uint64,
	fence ControllerMetadataTransferFence,
) error {
	if err := lifecycle.validateNoTransition(nowMS); err != nil {
		return err
	}
	if lifecycle.core.state != ControllerLifecycleMetadata {
		return ErrUnexpectedLifecycleCommand
	}
	if fence.TransportGeneration == 0 || fence.TransferGeneration == 0 ||
		fence.TransportGeneration != lifecycle.core.generation ||
		fence.TransferGeneration != lifecycle.core.metadataTransferGeneration {
		return ErrInvalidTransferGeneration
	}
	lifecycle.lastNowMS = nowMS
	lifecycle.core.state = ControllerLifecycleIdle
	return nil
}

// ClaimMetadataTransferFailed starts the immediate Arrival-Hello transition
// for the exact current metadata request generation.
func (lifecycle *ControllerLifecycle) ClaimMetadataTransferFailed(
	nowMS uint64,
	fence ControllerMetadataTransferFence,
) (ControllerLifecycleClaim, error) {
	if err := lifecycle.validateNoTransition(nowMS); err != nil {
		return ControllerLifecycleClaim{}, err
	}
	if lifecycle.core.state != ControllerLifecycleMetadata {
		return ControllerLifecycleClaim{}, ErrUnexpectedLifecycleCommand
	}
	if fence.TransportGeneration == 0 || fence.TransferGeneration == 0 ||
		fence.TransportGeneration != lifecycle.core.generation ||
		fence.TransferGeneration != lifecycle.core.metadataTransferGeneration {
		return ControllerLifecycleClaim{}, ErrInvalidTransferGeneration
	}
	transition := controllerLifecycleTransition{
		decision: lifecycleDecision1(ControllerLifecycleSendHello),
		target:   lifecycle.core, deadline: lifecycleHelloAfterDelivery,
	}
	transition.target.state = ControllerLifecycleArrival
	transition.target.terminationMS = 0
	return lifecycle.beginTransition(transition, nowMS)
}

// ClaimRestartAfterUSBReset authoritatively interrupts predecessor work and
// starts a gate/clear/Hello transition. Arrival and the successor generation
// commit only when its final Hello action is delivered.
func (lifecycle *ControllerLifecycle) ClaimRestartAfterUSBReset(
	nowMS uint64,
) (ControllerLifecycleClaim, error) {
	if err := lifecycle.validateInitialized(); err != nil {
		return ControllerLifecycleClaim{}, err
	}
	if err := lifecycle.validateClock(nowMS); err != nil {
		return ControllerLifecycleClaim{}, err
	}
	if lifecycle.hasClaim && lifecycle.claimAdmitted {
		// Admission hands the action to an external executor. Reset must wait
		// until that executor has completed or synchronously cancelled/drained
		// and resolved the lease; otherwise a late predecessor effect could
		// overtake the reset's gate/clear sequence.
		return ControllerLifecycleClaim{}, ErrLifecycleClaimOutstanding
	}
	if lifecycle.core.generation == ^uint64(0) {
		return ControllerLifecycleClaim{}, ErrInvalidTransferGeneration
	}
	// Make every remaining local failure check before discarding an unadmitted
	// predecessor transition or retained retry. Once these fences pass,
	// beginTransition cannot fail.
	if lifecycle.nextTransitionEpoch == ^uint64(0) {
		return ControllerLifecycleClaim{}, ErrInvalidTransferEpoch
	}
	lifecycle.clearPendingTransition()
	transition := controllerLifecycleTransition{
		decision: lifecycleDecision3(
			ControllerLifecycleGateNormalUpstream,
			ControllerLifecycleClearOutputs,
			ControllerLifecycleSendHello),
		target: lifecycle.core, deadline: lifecycleHelloAfterDelivery,
	}
	transition.target.state = ControllerLifecycleArrival
	transition.target.generation++
	transition.target.terminationMS = 0
	return lifecycle.beginTransition(transition, nowMS)
}

// ClaimNextAction claims the next unresolved action after an earlier action was
// delivered. It never skips or replays a completed cursor.
func (lifecycle *ControllerLifecycle) ClaimNextAction(nowMS uint64) (ControllerLifecycleClaim, error) {
	if err := lifecycle.validateInitialized(); err != nil {
		return ControllerLifecycleClaim{}, err
	}
	if err := lifecycle.validateClock(nowMS); err != nil {
		return ControllerLifecycleClaim{}, err
	}
	if !lifecycle.pendingTransition {
		return ControllerLifecycleClaim{}, ErrLifecycleNoPendingAction
	}
	if lifecycle.hasClaim {
		return ControllerLifecycleClaim{}, ErrLifecycleClaimOutstanding
	}
	if lifecycle.retryPending {
		return ControllerLifecycleClaim{}, ErrLifecycleRetryRequired
	}
	return lifecycle.claimCurrentAction(nowMS, false), nil
}

// ClaimRetry returns only the current failed/deferred action at the same cursor.
func (lifecycle *ControllerLifecycle) ClaimRetry(nowMS uint64) (ControllerLifecycleClaim, error) {
	if err := lifecycle.validateInitialized(); err != nil {
		return ControllerLifecycleClaim{}, err
	}
	if err := lifecycle.validateClock(nowMS); err != nil {
		return ControllerLifecycleClaim{}, err
	}
	if !lifecycle.pendingTransition || !lifecycle.retryPending {
		return ControllerLifecycleClaim{}, ErrLifecycleRetryRequired
	}
	if lifecycle.hasClaim {
		return ControllerLifecycleClaim{}, ErrLifecycleClaimOutstanding
	}
	claim := lifecycle.claimCurrentAction(nowMS, true)
	lifecycle.retryPending = false
	return claim, nil
}

func (lifecycle *ControllerLifecycle) validateClaim(claim ControllerLifecycleClaim) error {
	if !lifecycle.pendingTransition || !lifecycle.hasClaim || claim.owner != lifecycle ||
		claim.token == 0 || claim.token != lifecycle.claimToken ||
		claim.generation != lifecycle.core.generation ||
		claim.transitionEpoch != lifecycle.transitionEpoch ||
		claim.action != lifecycle.currentAction() || claim.cursor != lifecycle.actionCursor ||
		claim.totalActions != lifecycle.transition.decision.length ||
		claim.selectedAtMS != lifecycle.actionSelectedAtMS {
		return ErrInvalidLifecycleClaim
	}
	var metadataGeneration uint64
	if claim.action == ControllerLifecycleBeginMetadata {
		metadataGeneration = lifecycle.transition.target.metadataTransferGeneration
	}
	if claim.metadataTransferGeneration != metadataGeneration {
		return ErrInvalidLifecycleClaim
	}
	return nil
}

// Admit performs the final serialized fence immediately before executing this
// one action. Success converts claim into the exclusive execution lease
// identified by claim.ExecutionFence().
func (lifecycle *ControllerLifecycle) Admit(
	claim ControllerLifecycleClaim,
	nowMS uint64,
) (bool, error) {
	if err := lifecycle.validateClaim(claim); err != nil {
		return false, err
	}
	if lifecycle.claimAdmitted {
		return false, ErrInvalidLifecycleClaim
	}
	if err := lifecycle.validateClock(nowMS); err != nil {
		return false, err
	}
	lifecycle.lastNowMS = nowMS
	lifecycle.claimAdmitted = true
	return true, nil
}

// Resolve advances only this action cursor. Earlier delivered actions are not
// retried. Target state, deadline, and generation commit only after the final
// action. A valid admitted Delivered resolution cannot fail after the external
// effect: completion time clamps monotonically and deadlines saturate.
func (lifecycle *ControllerLifecycle) Resolve(
	claim ControllerLifecycleClaim,
	outcome ControllerLifecycleOutcome,
	completedMS uint64,
) error {
	if err := lifecycle.validateClaim(claim); err != nil {
		return err
	}
	if outcome != ControllerLifecycleDelivered && outcome != ControllerLifecycleDeferred &&
		outcome != ControllerLifecycleDeliveryFailed &&
		outcome != ControllerLifecycleExecutionCancelled {
		return ErrInvalidLifecycleOutcome
	}
	if (outcome == ControllerLifecycleDelivered ||
		outcome == ControllerLifecycleExecutionCancelled) && !lifecycle.claimAdmitted {
		return ErrLifecycleClaimNotAdmitted
	}
	if completedMS < lifecycle.lastNowMS {
		completedMS = lifecycle.lastNowMS
	}
	lifecycle.lastNowMS = completedMS
	lifecycle.hasClaim = false
	lifecycle.claimAdmitted = false
	lifecycle.claimToken = 0
	if outcome != ControllerLifecycleDelivered {
		lifecycle.retryPending = true
		return nil
	}

	lifecycle.retryPending = false
	if lifecycle.actionCursor+1 < lifecycle.transition.decision.length {
		lifecycle.actionCursor++
		lifecycle.actionSelectedAtMS = 0
		return nil
	}
	target := lifecycle.transition.target
	switch lifecycle.transition.deadline {
	case lifecycleHelloAfterDelivery:
		target.nextHelloMS = saturatingLifecycleDeadline(
			completedMS, HelloIntervalMilliseconds)
	case lifecycleTerminationAfterDelivery:
		target.terminationMS = saturatingLifecycleDeadline(
			completedMS, PowerTransitionDelayMilliseconds)
	}
	lifecycle.core = target
	lifecycle.clearPendingTransition()
	return nil
}

func (lifecycle *ControllerLifecycle) clearPendingTransition() {
	lifecycle.pendingTransition = false
	lifecycle.transitionEpoch = 0
	lifecycle.transition = controllerLifecycleTransition{}
	lifecycle.actionCursor = 0
	lifecycle.actionSelectedAtMS = 0
	lifecycle.retryPending = false
	lifecycle.hasClaim = false
	lifecycle.claimAdmitted = false
	lifecycle.claimToken = 0
}

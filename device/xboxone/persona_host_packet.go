package xboxone

import (
	"errors"
	"fmt"
)

const controllerPersonaHostPacketNoContext = ^uint8(0)

// ControllerPersonaHostPacketClaim authenticates one complete, ordered
// downstream controller data packet selected by the canonical persona. A
// retry receives a fresh persona token but retains PacketEpoch and every
// action value.
type ControllerPersonaHostPacketClaim struct {
	owner      *ControllerPersonaEngine
	token      uint64
	generation uint64
	epoch      uint64
	count      uint8
}

func (claim ControllerPersonaHostPacketClaim) Valid() bool {
	return claim.owner != nil && claim.token != 0 && claim.generation != 0 &&
		claim.epoch != 0 && claim.count != 0
}

type controllerPersonaHostPacketRecord struct {
	execution    ControllerDownstreamPacketExecution
	personaClaim ControllerPersonaClaim
	contextIndex uint8
}

func (record controllerPersonaHostPacketRecord) valid(engine *ControllerPersonaEngine) bool {
	return engine != nil && record.execution.valid() &&
		record.personaClaim.Valid() && record.personaClaim.owner == engine &&
		(record.contextIndex == controllerPersonaHostPacketNoContext ||
			int(record.contextIndex) < record.execution.Len())
}

// claimHostPacket selects one complete packet into the persona's existing
// serialized claim lane. It supports any ordered Direct Motor / Guide LED
// vector and at most one lifecycle or Protocol Control ACK member. That exact
// context-bearing member is selected through claimDecodedHostMessage; the
// method never copies ControllerPersonaEngine or fabricates ACK identity.
//
// More than one context-bearing member fails before any persona mutation. The
// current lifecycle and reliable-transfer owners each expose one outstanding
// claim, so accepting a second would require an unproven preview/rollback or a
// prefix commit.
func (engine *ControllerPersonaEngine) claimHostPacket(
	nowMS uint64,
	packet ControllerDownstreamPacket,
) (ControllerPersonaHostPacketClaim, ControllerDownstreamPacketExecution, error) {
	if err := engine.ensureNewClaimAllowed(); err != nil {
		return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{}, err
	}
	if packet.Len() == 0 || packet.Len() > controllerDownstreamPacketMaximumMessages {
		return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
			ErrInvalidControllerPersonaHostPacketClaim
	}
	if engine.nextHostPacketEpoch == ^uint64(0) {
		return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
			ErrControllerDownstreamPacketTokenExhausted
	}

	contextIndex := controllerPersonaHostPacketNoContext
	for index := 0; index < packet.Len(); index++ {
		message, ok := packet.Message(index)
		if !ok || message.Sequence == 0 {
			return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
				ErrInvalidControllerPersonaHostPacketClaim
		}
		switch message.Kind {
		case ControllerDownstreamLifecycle:
			if message.Lifecycle.Sequence != message.Sequence {
				return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
					ErrInvalidControllerPersonaHostPacketClaim
			}
			if err := message.Lifecycle.validate(); err != nil {
				return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{}, err
			}
			// STOP, OFF, and RESET first gate normal upstream and defer their
			// mandatory output clear to a successor lifecycle cursor. Until a
			// production participant can commit that successor clear in this
			// same atomic effect, admitting any sibling would allow a later
			// feedback action to become visible in Idle/termination. Fail before
			// the clock or either canonical owner mutates. QUIESCE is different:
			// its selected action is the clear itself.
			if packet.Len() > 1 &&
				message.Lifecycle.Kind == ControllerHostCommandSetDeviceState &&
				(message.Lifecycle.State == SetDeviceStateStop ||
					message.Lifecycle.State == SetDeviceStateOff ||
					message.Lifecycle.State == SetDeviceStateReset) {
				return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
					ErrControllerPersonaHostPacketContextLimit
			}
			if contextIndex != controllerPersonaHostPacketNoContext {
				return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
					ErrControllerPersonaHostPacketContextLimit
			}
			contextIndex = uint8(index)
		case ControllerDownstreamProtocolControlACK:
			if err := message.ACK.Validate(); err != nil {
				return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{}, err
			}
			if contextIndex != controllerPersonaHostPacketNoContext {
				return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
					ErrControllerPersonaHostPacketContextLimit
			}
			contextIndex = uint8(index)
		case ControllerDownstreamDirectMotor:
			if err := message.DirectMotor.Validate(); err != nil {
				return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{}, err
			}
		case ControllerDownstreamGuideLED:
			if err := message.GuideLED.Validate(); err != nil {
				return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{}, err
			}
		default:
			return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
				ErrUnsupportedControllerPersonaHostMessage
		}
	}
	if contextIndex != controllerPersonaHostPacketNoContext && packet.Len() > 1 {
		message, _ := packet.Message(int(contextIndex))
		state := engine.lifecycle.State()
		if message.Kind == ControllerDownstreamLifecycle &&
			(state == ControllerLifecycleTerminatingOff ||
				state == ControllerLifecycleTerminatingReset) {
			return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
				ErrControllerPersonaHostPacketContextLimit
		}
	}
	if err := engine.observeClock(nowMS); err != nil {
		return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{}, err
	}
	if err := engine.ensureGIPDownstreamAvailable(); err != nil {
		return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{}, err
	}

	var personaClaim ControllerPersonaClaim
	ignoredContext := false
	if contextIndex != controllerPersonaHostPacketNoContext {
		message, _ := packet.Message(int(contextIndex))
		claim, disposition, err := engine.claimDecodedHostMessage(nowMS, message)
		if err != nil {
			return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{}, err
		}
		if disposition == ControllerPersonaHostIgnored {
			ignoredContext = true
		} else if !claim.Valid() {
			engine.quarantineClaimedHostPacket(ErrControllerPersonaInvariantViolation)
			return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
				engine.hostPacketQuarantineError()
		} else {
			personaClaim = claim
		}
	}
	if !personaClaim.Valid() {
		// Reserve the ordinary persona lane even for an all-feedback packet or
		// FULL POWER no-op. The synthetic record has no effect; whole-packet
		// resolution below applies the exact ordered local vector.
		personaClaim = engine.makeClaim(controllerPersonaRecord{
			action: ControllerPersonaIgnoreHostMessage, selectedAtMS: nowMS,
		})
	}

	epoch := engine.nextHostPacketEpoch + 1
	execution := ControllerDownstreamPacketExecution{
		packetEpoch: epoch, length: uint8(packet.Len()),
	}
	for index := 0; index < packet.Len(); index++ {
		message, _ := packet.Message(index)
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
		case ControllerDownstreamLifecycle, ControllerDownstreamProtocolControlACK:
			if uint8(index) != contextIndex {
				engine.quarantineClaimedHostPacket(ErrControllerPersonaInvariantViolation)
				return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
					engine.hostPacketQuarantineError()
			}
			if ignoredContext {
				action.Action = ControllerPersonaIgnoreHostMessage
			} else {
				action = controllerDownstreamPacketActionFromPersonaRecord(
					engine.claimRecord, uint8(index), message.Sequence)
			}
		}
		if !action.valid() {
			err := fmt.Errorf("%w: packet action %d",
				ErrControllerPersonaInvariantViolation, index)
			engine.quarantineClaimedHostPacket(err)
			return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
				engine.hostPacketQuarantineError()
		}
		execution.actions[index] = action
	}
	if !execution.valid() {
		engine.quarantineClaimedHostPacket(ErrControllerPersonaInvariantViolation)
		return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
			engine.hostPacketQuarantineError()
	}

	record := controllerPersonaHostPacketRecord{
		execution: execution, personaClaim: personaClaim,
		contextIndex: contextIndex,
	}
	engine.nextHostPacketEpoch = epoch
	engine.hostPacketActive = true
	engine.hostPacketRecord = record
	claim := engine.hostPacketClaim(record)
	return claim, execution, nil
}

func (engine *ControllerPersonaEngine) quarantineClaimedHostPacket(err error) {
	engine.hostPacketQuarantined = true
	engine.hostPacketQuarantine = errors.Join(engine.hostPacketQuarantine, err)
}

func (engine *ControllerPersonaEngine) hostPacketQuarantineError() error {
	return errors.Join(ErrControllerPersonaHostPacketQuarantined,
		engine.hostPacketQuarantine)
}

func controllerDownstreamPacketActionFromPersonaRecord(
	record controllerPersonaRecord,
	wireIndex uint8,
	hostSequence uint8,
) ControllerDownstreamPacketAction {
	action := ControllerDownstreamPacketAction{
		Action: record.action, WireIndex: wireIndex, Sequence: hostSequence,
		ClearEpoch:             record.clearEpoch,
		ReliableACKDisposition: record.reliableDisposition,
		DirectMotor:            record.directMotor, GuideLED: record.guideLED,
		wireSize: record.size,
	}
	if record.size != 0 {
		action.OutputSequence = record.sequence
	}
	copy(action.wire[:], record.wire[:record.size])
	return action
}

func (engine *ControllerPersonaEngine) hostPacketClaim(
	record controllerPersonaHostPacketRecord,
) ControllerPersonaHostPacketClaim {
	return ControllerPersonaHostPacketClaim{
		owner: engine, token: record.personaClaim.token,
		generation: engine.generation, epoch: record.execution.packetEpoch,
		count: record.execution.length,
	}
}

func (engine *ControllerPersonaEngine) validateHostPacketClaim(
	claim ControllerPersonaHostPacketClaim,
) error {
	record := engine.hostPacketRecord
	if !engine.hostPacketActive || !record.valid(engine) || !claim.Valid() ||
		claim.owner != engine || claim.token != record.personaClaim.token ||
		claim.generation != engine.generation ||
		claim.epoch != record.execution.packetEpoch ||
		claim.count != record.execution.length {
		return ErrInvalidControllerPersonaHostPacketClaim
	}
	return engine.validateClaim(record.personaClaim)
}

// admitHostPacket performs every final canonical lifecycle/ACK fence and
// returns the same immutable complete action vector. It has no external effect.
func (engine *ControllerPersonaEngine) admitHostPacket(
	claim ControllerPersonaHostPacketClaim,
	nowMS uint64,
) (ControllerDownstreamPacketExecution, error) {
	if err := engine.validateHostPacketClaim(claim); err != nil {
		return ControllerDownstreamPacketExecution{}, err
	}
	record := engine.hostPacketRecord
	var wire [ControllerPersonaMaximumWireSize]byte
	if err := engine.AdmitAndCopy(record.personaClaim,
		wire[:record.personaClaim.Size()], nowMS); err != nil {
		return ControllerDownstreamPacketExecution{}, err
	}
	return record.execution, nil
}

// resolveHostPacket commits or retains the whole canonical packet. Delivered
// resolution applies the context-bearing persona claim and then derives the
// final feedback snapshot by replaying the complete action vector in wire
// order. Every non-delivered result retains one exact immutable batch retry.
func (engine *ControllerPersonaEngine) resolveHostPacket(
	claim ControllerPersonaHostPacketClaim,
	outcome ControllerPersonaOutcome,
	completedMS uint64,
) error {
	if err := engine.validateHostPacketClaim(claim); err != nil {
		return err
	}
	if outcome < ControllerPersonaDelivered || outcome > ControllerPersonaExecutionCancelled {
		return ErrInvalidControllerPersonaOutcome
	}
	record := engine.hostPacketRecord
	direct := engine.directMotor
	guide := engine.guideLED
	clearEpoch := engine.lastClearEpoch
	if outcome == ControllerPersonaDelivered {
		for index := 0; index < record.execution.Len(); index++ {
			action, _ := record.execution.Action(index)
			switch action.Action {
			case ControllerPersonaApplyDirectMotor:
				direct = action.DirectMotor
			case ControllerPersonaApplyGuideLED:
				guide = action.GuideLED
			case ControllerPersonaClearOutputs:
				direct = NewStopRumbleBody()
				guide = GuideLEDCommandV1{Pattern: GuideLEDPatternOff}
				clearEpoch = action.ClearEpoch
			}
		}
	}
	if err := engine.Resolve(record.personaClaim, outcome, completedMS); err != nil {
		return err
	}
	engine.hostPacketActive = false
	engine.hostPacketRecord = controllerPersonaHostPacketRecord{}
	if outcome == ControllerPersonaDelivered {
		engine.directMotor = direct
		engine.guideLED = guide
		engine.lastClearEpoch = clearEpoch
		engine.hostPacketRetryPending = false
		engine.hostPacketRetryRecord = controllerPersonaHostPacketRecord{}
		return nil
	}
	engine.hostPacketRetryPending = true
	engine.hostPacketRetryRecord = record
	return nil
}

// claimHostPacketRetry reclaims the exact packet epoch and ordered action
// vector while the canonical inner persona owners allocate fresh attempt
// tokens. No message is re-decoded and no later packet can overtake it.
func (engine *ControllerPersonaEngine) claimHostPacketRetry(
	nowMS uint64,
) (ControllerPersonaHostPacketClaim, ControllerDownstreamPacketExecution, error) {
	if err := engine.validateInitialized(); err != nil {
		return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{}, err
	}
	if engine.hostPacketActive || engine.hasClaim {
		return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
			ErrControllerPersonaClaimOutstanding
	}
	if !engine.hostPacketRetryPending || !engine.retryPending ||
		!engine.hostPacketRetryRecord.execution.valid() {
		return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{},
			ErrControllerPersonaHostPacketRetryRequired
	}
	record := engine.hostPacketRetryRecord
	engine.hostPacketRetryPending = false
	personaClaim, err := engine.ClaimRetry(nowMS)
	if err != nil {
		if engine.retryPending {
			engine.hostPacketRetryPending = true
		} else {
			// The canonical ACK retry may expire and retire itself so ClaimPoll
			// can select metadata failure. Do not advertise a batch retry whose
			// inner context no longer exists.
			engine.hostPacketRetryRecord = controllerPersonaHostPacketRecord{}
		}
		return ControllerPersonaHostPacketClaim{}, ControllerDownstreamPacketExecution{}, err
	}
	record.personaClaim = personaClaim
	engine.hostPacketRetryRecord = controllerPersonaHostPacketRecord{}
	engine.hostPacketRecord = record
	engine.hostPacketActive = true
	claim := engine.hostPacketClaim(record)
	return claim, record.execution, nil
}

// controllerPersonaHostPacketSnapshot exposes only ownership facts. It is diagnostic evidence,
// never a claim or an execution capability.
type controllerPersonaHostPacketSnapshot struct {
	PacketEpoch  uint64
	MessageCount uint8
	Active       bool
	Admitted     bool
	RetryPending bool
	Quarantined  bool
}

func (engine *ControllerPersonaEngine) hostPacketSnapshot() (
	controllerPersonaHostPacketSnapshot,
	bool,
) {
	if engine == nil || engine.validateInitialized() != nil {
		return controllerPersonaHostPacketSnapshot{}, false
	}
	record := engine.hostPacketRecord
	if engine.hostPacketRetryPending {
		record = engine.hostPacketRetryRecord
	}
	return controllerPersonaHostPacketSnapshot{
		PacketEpoch:  record.execution.packetEpoch,
		MessageCount: record.execution.length,
		Active:       engine.hostPacketActive,
		Admitted:     engine.hostPacketActive && engine.claimAdmitted,
		RetryPending: engine.hostPacketRetryPending,
		Quarantined:  engine.hostPacketQuarantined,
	}, true
}

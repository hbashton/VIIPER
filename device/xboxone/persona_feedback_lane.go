package xboxone

type controllerPersonaOrdinaryFeedbackState uint8

const (
	controllerPersonaOrdinaryFeedbackEmpty controllerPersonaOrdinaryFeedbackState = iota
	controllerPersonaOrdinaryFeedbackAdmitted
	controllerPersonaOrdinaryFeedbackRetry
)

// One value-owned slot retains the exact local obligation while ordinary IN
// uses the primary lane. There is no second mapper, executor, queue, or clock.
// Every call shares the engine's existing external serialization requirement.
type controllerPersonaOrdinaryFeedbackLane struct {
	state  controllerPersonaOrdinaryFeedbackState
	claim  ControllerPersonaClaim
	record controllerPersonaRecord
}

func isOrdinaryFeedbackAction(action ControllerPersonaAction) bool {
	return action == ControllerPersonaApplyDirectMotor || action == ControllerPersonaApplyGuideLED
}

func (engine *ControllerPersonaEngine) ordinaryFeedbackPending() bool {
	return engine != nil && engine.ordinaryFeedback.state != controllerPersonaOrdinaryFeedbackEmpty
}

func (engine *ControllerPersonaEngine) ordinaryFeedbackRetryPending() bool {
	return engine != nil && engine.ordinaryFeedback.state == controllerPersonaOrdinaryFeedbackRetry
}

func (engine *ControllerPersonaEngine) ensureNoOrdinaryFeedback() error {
	if engine.ordinaryFeedbackRetryPending() {
		return ErrControllerPersonaRetryRequired
	}
	if engine.ordinaryFeedbackPending() {
		return ErrControllerPersonaClaimOutstanding
	}
	return nil
}

func (engine *ControllerPersonaEngine) ensureNewOrdinaryUpstreamClaimAllowed() error {
	if err := engine.ensurePrimaryClaimAllowed(); err != nil {
		return err
	}
	if engine.ordinaryFeedbackPending() {
		return engine.ensureOrdinaryInputAvailable()
	}
	return nil
}

// Only autonomous active-state input, Guide, or periodic status may coexist
// with the side owner. START's status/initial input belong to lifecycle and are
// deliberately excluded even though their wire message types overlap.
func isOrdinaryUpstreamRecord(record controllerPersonaRecord) bool {
	if record.usbOwned || record.lifecycleOwned || record.metadataOwned ||
		record.metadataFailureHello || record.configurationLossClear ||
		record.clearEpoch != 0 || record.size == 0 {
		return false
	}
	switch record.action {
	case ControllerPersonaSendInput:
		return record.sequencePool == controllerPersonaInputSequence
	case ControllerPersonaSendGuideButtonStatus, ControllerPersonaSendCurrentStatus:
		return record.sequencePool == controllerPersonaGlobalSequence
	default:
		return false
	}
}

// A complete value comparison excludes every hidden ownership field, not only
// the public action/size. Sequence is the incoming host message identity and
// may be nonzero; sequencePool/sequenceClaim must remain unowned and zero.
func validateOrdinaryFeedbackRecord(record controllerPersonaRecord) error {
	expected := controllerPersonaRecord{
		action: record.action, sequence: record.sequence, selectedAtMS: record.selectedAtMS,
	}
	switch record.action {
	case ControllerPersonaApplyDirectMotor:
		if err := record.directMotor.Validate(); err != nil {
			return err
		}
		expected.directMotor = record.directMotor
	case ControllerPersonaApplyGuideLED:
		if err := record.guideLED.Validate(); err != nil {
			return err
		}
		expected.guideLED = record.guideLED
	default:
		return ErrInvalidControllerPersonaClaim
	}
	if record != expected {
		return ErrControllerPersonaInvariantViolation
	}
	return nil
}

func (engine *ControllerPersonaEngine) validateOrdinaryFeedbackSlot() error {
	lane := &engine.ordinaryFeedback
	if err := validateOrdinaryFeedbackRecord(lane.record); err != nil {
		return err
	}
	if !lane.claim.Valid() {
		return ErrInvalidControllerPersonaClaim
	}
	expected := ControllerPersonaClaim{
		owner: engine, token: lane.claim.token, generation: engine.generation,
		action: lane.record.action, sequence: lane.record.sequence,
		selectedAtMS: lane.record.selectedAtMS,
		directMotor:  lane.record.directMotor, guideLED: lane.record.guideLED,
	}
	if lane.claim != expected {
		return ErrInvalidControllerPersonaClaim
	}
	return nil
}

// detachOrdinaryFeedback transfers, but never delivers, one already-admitted
// primary local obligation. The same opaque claim authenticates its eventual
// completion; primary Resolve/AdmitAndCopy no longer accept it after transfer.
func (engine *ControllerPersonaEngine) detachOrdinaryFeedback(claim ControllerPersonaClaim) error {
	if err := engine.validateInitialized(); err != nil {
		return err
	}
	if err := engine.validateClaim(claim); err != nil {
		return err
	}
	if !engine.claimAdmitted {
		return ErrControllerPersonaClaimNotAdmitted
	}
	if err := engine.ensureNoOrdinaryFeedback(); err != nil {
		return err
	}
	if engine.retryPending || engine.hostPacketActive || engine.hostPacketRetryPending ||
		engine.hostPacketQuarantined || engine.configurationLossClearPending {
		return ErrControllerPersonaBoundaryBlocked
	}
	if err := validateOrdinaryFeedbackRecord(engine.claimRecord); err != nil {
		return err
	}
	engine.ordinaryFeedback = controllerPersonaOrdinaryFeedbackLane{
		state: controllerPersonaOrdinaryFeedbackAdmitted, claim: claim, record: engine.claimRecord,
	}
	engine.clearClaim()
	return nil
}

// completeOrdinaryFeedback records the exact executor result. Non-delivery
// retains the immutable mandatory retry and requires the same no-late-effect /
// synchronous cancellation-drain promises as primary Resolve. It does not imply
// physical HID flush unless the caller's executor contract separately proves it.
func (engine *ControllerPersonaEngine) completeOrdinaryFeedback(
	claim ControllerPersonaClaim, outcome ControllerPersonaOutcome, completedMS uint64,
) error {
	if err := engine.validateInitialized(); err != nil {
		return err
	}
	lane := &engine.ordinaryFeedback
	if lane.state != controllerPersonaOrdinaryFeedbackAdmitted || !claim.Valid() ||
		claim.owner != engine || claim.generation != engine.generation || claim != lane.claim {
		return ErrInvalidControllerPersonaClaim
	}
	if outcome < ControllerPersonaDelivered || outcome > ControllerPersonaExecutionCancelled {
		return ErrInvalidControllerPersonaOutcome
	}
	if err := engine.validateOrdinaryFeedbackSlot(); err != nil {
		return err
	}
	if completedMS < engine.lastNowMS {
		completedMS = engine.lastNowMS
	}
	if outcome == ControllerPersonaDelivered {
		// The validated record can reach only the existing infallible typed
		// motor/LED state assignments, never a lifecycle or clear operation.
		if err := engine.applyDelivered(lane.record, completedMS); err != nil {
			return err
		}
		*lane = controllerPersonaOrdinaryFeedbackLane{}
	} else {
		lane.state = controllerPersonaOrdinaryFeedbackRetry
	}
	engine.lastNowMS = completedMS
	return nil
}

// admitOrdinaryFeedbackRetry reissues and finally admits only the side retry.
// It must be called at the executor admission boundary. A simultaneous primary
// IN claim/retry and its inner sequence reservation remain completely untouched.
func (engine *ControllerPersonaEngine) admitOrdinaryFeedbackRetry(
	nowMS uint64,
) (ControllerPersonaClaim, bool, error) {
	if err := engine.validateInitialized(); err != nil {
		return ControllerPersonaClaim{}, false, err
	}
	lane := &engine.ordinaryFeedback
	if lane.state == controllerPersonaOrdinaryFeedbackEmpty {
		return ControllerPersonaClaim{}, false, nil
	}
	if lane.state != controllerPersonaOrdinaryFeedbackRetry {
		return ControllerPersonaClaim{}, false, ErrControllerPersonaClaimOutstanding
	}
	if engine.hostPacketActive || engine.hostPacketRetryPending || engine.hostPacketQuarantined ||
		engine.configurationLossClearPending ||
		(engine.hasClaim && !isOrdinaryUpstreamRecord(engine.claimRecord)) ||
		(engine.retryPending && !isOrdinaryUpstreamRecord(engine.retryRecord)) {
		return ControllerPersonaClaim{}, false, ErrControllerPersonaBoundaryBlocked
	}
	if err := engine.validateOrdinaryFeedbackSlot(); err != nil {
		return ControllerPersonaClaim{}, false, err
	}
	// IN uses the same issuer while this lane is parked. Refuse wrap rather
	// than reissue a stale side capability under an apparently fresh token.
	if engine.nextToken == ^uint64(0) || engine.nextToken < lane.claim.token {
		return ControllerPersonaClaim{}, false, ErrControllerPersonaInvariantViolation
	}
	if err := engine.observeClock(nowMS); err != nil {
		return ControllerPersonaClaim{}, false, err
	}
	lane.claim.token = engine.nextClaimToken()
	lane.claim.selectedAtMS = nowMS
	lane.record.selectedAtMS = nowMS
	lane.state = controllerPersonaOrdinaryFeedbackAdmitted
	return lane.claim, true, nil
}

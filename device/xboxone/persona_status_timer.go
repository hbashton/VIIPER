package xboxone

// MS-GIPUSB 1.0 section 3.1.5.5.2 requires Status Device every second for
// the first ten seconds, then every twenty seconds, and recommends resetting
// the periodic timer whenever status is sent. This timer anchors that first
// window to delivery of START's mandatory CurrentStatus, not claim selection
// or receipt of START. Failure/retry never advances it. Delayed delivery
// schedules from the real completion; missed opportunities are not replayed.
//
// This is separate from Gamepad Input Report's Keep Alive bit. Section
// 3.1.5.6.1.1 recommends change-only input reports; periodic status must remain
// schedulable even when no semantic input changes.
const (
	controllerPersonaStatusInitialIntervalMS uint64 = 1000
	controllerPersonaStatusInitialWindowMS   uint64 = 10000
	controllerPersonaStatusSteadyIntervalMS  uint64 = 20000
)

type controllerPersonaStatusTimer struct {
	startedAtMS uint64
	nextAtMS    uint64
	started     bool
	scheduled   bool
}

func (timer *controllerPersonaStatusTimer) delivered(completedMS uint64, start bool) {
	if start {
		*timer = controllerPersonaStatusTimer{started: true, startedAtMS: completedMS}
	}
	if !timer.started {
		return
	}
	interval := controllerPersonaStatusSteadyIntervalMS
	if completedMS-timer.startedAtMS < controllerPersonaStatusInitialWindowMS {
		interval = controllerPersonaStatusInitialIntervalMS
	}
	timer.nextAtMS = saturatingLifecycleDeadline(completedMS, interval)
	// At the terminal representable timestamp no future deadline exists. Do
	// not wrap or create an always-due status loop at MaxUint64.
	timer.scheduled = timer.nextAtMS > completedMS
}

// NextPeriodicStatusDeadlineMilliseconds returns the next periodic-status
// opportunity in the engine's monotonic millisecond clock. It is read-only and
// must be called under the same serialization as other engine methods. A
// pending lifecycle transition, directional IN halt, unconfigured interface,
// or stopped/detached persona cannot expose a runnable status deadline.
//
// The transport must keep this deadline when parking an unchanged ordinary IN
// request. Input readiness alone cannot wake time-driven protocol obligations.
func (engine *ControllerPersonaEngine) NextPeriodicStatusDeadlineMilliseconds() (uint64, bool) {
	if engine == nil || !engine.initialized || !engine.statusTimer.scheduled ||
		engine.ensureOrdinaryInputAvailable() != nil {
		return 0, false
	}
	return engine.statusTimer.nextAtMS, true
}

// claimPeriodicStatus runs only inside ClaimPoll after its existing owner,
// clock, and source validation. Ordinary input and Guide keep their independent
// sequence pool / queue; status uses the existing immutable Global claim lane.
func (engine *ControllerPersonaEngine) claimPeriodicStatus(nowMS uint64) (ControllerPersonaClaim, bool, error) {
	dueMS, scheduled := engine.NextPeriodicStatusDeadlineMilliseconds()
	if !scheduled || nowMS < dueMS {
		return ControllerPersonaClaim{}, false, nil
	}
	record := controllerPersonaRecord{
		action: ControllerPersonaSendCurrentStatus, selectedAtMS: nowMS,
	}
	record, err := engine.recordStatusWire(record, engine.currentStatus)
	if err != nil {
		return ControllerPersonaClaim{}, false, err
	}
	return engine.makeClaim(record), true, nil
}

// A typed status encoder keeps the fixed record on the stack. Passing its
// wire slice through recordGlobalWire's callback makes that record escape.
// Initial, periodic, and powering-off status share this exact codec/pool path.
func (engine *ControllerPersonaEngine) recordStatusWire(
	record controllerPersonaRecord, status ExtendedStatusNoEventsBodyV1,
) (controllerPersonaRecord, error) {
	sequence, err := previewControllerPersonaSequence(&engine.global)
	if err != nil {
		return controllerPersonaRecord{}, err
	}
	record.size = ExtendedStatusNoEventsMessageSize
	record.sequence = sequence
	if err := EncodeExtendedStatusNoEventsMessageInto(record.wire[:record.size], sequence, status); err != nil {
		return controllerPersonaRecord{}, err
	}
	claim, err := engine.global.Claim()
	if err != nil {
		return controllerPersonaRecord{}, err
	}
	record.sequencePool = controllerPersonaGlobalSequence
	record.sequenceClaim = claim
	return record, nil
}

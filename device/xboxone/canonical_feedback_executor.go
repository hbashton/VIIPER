package xboxone

import (
	"errors"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
)

var (
	ErrCanonicalFeedbackExecutorUninitialized = errors.New(
		"xboxone: canonical feedback executor is uninitialized")
	ErrCanonicalFeedbackExecutorState = errors.New(
		"xboxone: canonical feedback executor lifecycle state is invalid")
	ErrCanonicalFeedbackExecutorBusy = errors.New(
		"xboxone: canonical feedback executor already owns an execution")
	ErrCanonicalFeedbackExecutorDeadline = errors.New(
		"xboxone: canonical feedback executor deadline expired")
	ErrCanonicalFeedbackClockUnavailable = errors.New(
		"xboxone: Windows QPC canonical feedback clock is unavailable")
)

// ControllerPersonaCanonicalFeedbackWireV1 is one exact CFBK v1 value. A
// value parameter prevents a publisher from retaining an executor-owned
// scratch slice after PublishControllerFeedback returns.
type ControllerPersonaCanonicalFeedbackWireV1 [controllerfeedback.FrameSize]byte

// ControllerPersonaCanonicalFeedbackPublisher is the transport-neutral,
// synchronous DS4Windows broker edge. A nil return means the exact complete
// value was accepted before deadline. A non-nil return MUST prove that no
// byte was accepted and that no late publication can occur. A socket or IPC
// implementation which can only report an uncertain outcome cannot satisfy
// this interface; it must first close/drain its exact session and resolve that
// uncertainty at a higher generation boundary.
//
// Implementations must not retain wire and must permit reentrant diagnostic
// snapshots of the executor. The executor invokes this method without its
// lifecycle monitor held.
type ControllerPersonaCanonicalFeedbackPublisher interface {
	PublishControllerFeedback(
		wire ControllerPersonaCanonicalFeedbackWireV1,
		deadline time.Time,
	) error
}

type controllerPersonaCanonicalFeedbackClock func() (uint64, bool)

type controllerPersonaCanonicalFeedbackExecutorState uint8

const (
	canonicalFeedbackExecutorActive controllerPersonaCanonicalFeedbackExecutorState = iota + 1
	canonicalFeedbackExecutorResetDraining
	canonicalFeedbackExecutorResetDrained
	canonicalFeedbackExecutorResetNeutralizing
	canonicalFeedbackExecutorTerminalDraining
	canonicalFeedbackExecutorTerminalDrained
	canonicalFeedbackExecutorTerminalStopping
	canonicalFeedbackExecutorStopped
)

// ControllerPersonaCanonicalFeedbackExecutorSnapshot is a value-only view of
// the dormant executor. It is diagnostic evidence, not an admission lease.
type ControllerPersonaCanonicalFeedbackExecutorSnapshot struct {
	PersonaGeneration           uint64
	AuthorizedPersonaGeneration uint64
	DeviceGeneration            uint64
	TransportGeneration         uint64
	OwnershipEpoch              uint64
	Active                      bool
	ResetDrained                bool
	TerminalDrained             bool
	Stopped                     bool
	ExecutionInFlight           bool
	GuideLEDObserved            bool
	GuideLEDOrder               uint64
	GuideLED                    GuideLEDCommandV1
}

// ControllerPersonaCanonicalFeedbackExecutor is the concrete dormant local
// executor for the retained Xbox persona. It creates no second mailbox or
// feedback state machine: each already-ordered persona execution is encoded
// directly into the one project-wide CFBK contract.
//
// Recoverable ClearOutputs actions are lease-retaining Neutral frames.
// ResetNeutral atomically rebinds the private persona-generation fence only
// after its successor-generation Neutral is accepted. DisconnectNeutral is
// the only terminal path and emits Stop before permanently closing admission.
// Construction performs no I/O. The production timed motor owner uses this
// same single-frame edge; it does not duplicate physical mapping or ownership.
type ControllerPersonaCanonicalFeedbackExecutor struct {
	mu        sync.Mutex
	binding   ControllerPersonaFeedbackBindingV1
	publisher ControllerPersonaCanonicalFeedbackPublisher
	clock     controllerPersonaCanonicalFeedbackClock
	progress  chan struct{}
	state     controllerPersonaCanonicalFeedbackExecutorState
	inFlight  bool
	// authorizedPersonaGeneration follows exact, already-delivered persona
	// transport-boundary actions. binding.PersonaGeneration advances only when
	// the first CFBK value in that generation is accepted. This lets output-free
	// PerformReset/CompletePowerOff actions remain local engine obligations
	// without admitting stale predecessor feedback.
	authorizedPersonaGeneration uint64
	guideLEDObserved            bool
	guideLEDOrder               uint64
	guideLED                    GuideLEDCommandV1
}

// NewControllerPersonaCanonicalFeedbackExecutor constructs one exact dormant
// binding. The retained adapter transfers execution ownership separately.
func NewControllerPersonaCanonicalFeedbackExecutor(
	binding ControllerPersonaFeedbackBindingV1,
	publisher ControllerPersonaCanonicalFeedbackPublisher,
) (*ControllerPersonaCanonicalFeedbackExecutor, error) {
	if publisher == nil || binding.validate() != nil {
		return nil, ErrCanonicalFeedbackExecutorUninitialized
	}
	return &ControllerPersonaCanonicalFeedbackExecutor{
		binding: binding, publisher: publisher,
		clock:                       controllerfeedback.HostMonotonicMicroseconds,
		progress:                    make(chan struct{}, 1),
		state:                       canonicalFeedbackExecutorActive,
		authorizedPersonaGeneration: binding.PersonaGeneration,
	}, nil
}

// Execute publishes one ordinary motor update or recoverable clear. Guide LED
// is retained as explicit local visual state and diagnostic evidence; CFBK v1
// remains a motor-only frame and does not mislabel the command as haptics.
func (executor *ControllerPersonaCanonicalFeedbackExecutor) Execute(
	execution ControllerPersonaLocalExecution,
	deadline time.Time,
) error {
	return executor.executeUntil(execution, deadline, 0)
}

// executeUntil bounds one effective state by absolute expiry in the CFBK
// clock domain. It cannot change the authenticated binding or terminal Stop
// TTL. Zero means no additional bound. Sampling at publication prevents
// executor delays from extending a finite program.
func (executor *ControllerPersonaCanonicalFeedbackExecutor) executeUntil(
	execution ControllerPersonaLocalExecution,
	deadline time.Time,
	expiresAtMicroseconds uint64,
) error {
	if executor == nil {
		return ErrCanonicalFeedbackExecutorUninitialized
	}
	intent := ControllerPersonaCanonicalFeedbackStateUpdate
	if execution.Action == ControllerPersonaClearOutputs {
		intent = ControllerPersonaCanonicalFeedbackRecoverableNeutral
	}

	executor.mu.Lock()
	if executor.state != canonicalFeedbackExecutorActive {
		executor.mu.Unlock()
		return ErrCanonicalFeedbackExecutorState
	}
	if executor.inFlight {
		executor.mu.Unlock()
		return ErrCanonicalFeedbackExecutorBusy
	}
	if !canonicalFeedbackDeadlineValid(deadline) {
		executor.mu.Unlock()
		return ErrCanonicalFeedbackExecutorDeadline
	}
	if !execution.valid() ||
		execution.Generation != executor.authorizedPersonaGeneration {
		executor.mu.Unlock()
		return ErrInvalidCanonicalFeedbackBinding
	}
	if execution.Action == ControllerPersonaApplyGuideLED {
		executor.guideLED = execution.GuideLED
		executor.guideLEDOrder = execution.Order
		executor.guideLEDObserved = true
		executor.mu.Unlock()
		return nil
	}
	if canonicalFeedbackOutputFreeAction(execution.Action) {
		if (execution.Action == ControllerPersonaCompletePowerOff ||
			execution.Action == ControllerPersonaPerformReset) &&
			executor.authorizedPersonaGeneration == ^uint64(0) {
			executor.mu.Unlock()
			return ErrInvalidCanonicalFeedbackBinding
		}
		if execution.Action == ControllerPersonaCompletePowerOff ||
			execution.Action == ControllerPersonaPerformReset {
			// The canonical engine advances its generation only after this
			// exact action resolves Delivered. A later Resolve failure
			// quarantines the retained import, so no stale authorization can
			// become externally usable.
			executor.authorizedPersonaGeneration++
		}
		executor.mu.Unlock()
		return nil
	}
	if execution.Action != ControllerPersonaApplyDirectMotor &&
		execution.Action != ControllerPersonaClearOutputs {
		executor.mu.Unlock()
		return ErrUnsupportedCanonicalFeedbackAction
	}
	binding := executor.binding
	binding.PersonaGeneration = executor.authorizedPersonaGeneration
	promoteBinding := binding.PersonaGeneration !=
		executor.binding.PersonaGeneration
	executor.inFlight = true
	executor.mu.Unlock()

	err := executor.publishUntil(binding, execution, intent,
		deadline, expiresAtMicroseconds)
	executor.finishExecution(binding, promoteBinding, err == nil)
	return err
}

// ResetAndDrain reversibly fences ordinary Execute calls and joins the one
// possible predecessor publication. It performs no publication itself.
func (executor *ControllerPersonaCanonicalFeedbackExecutor) ResetAndDrain(
	deadline time.Time,
) error {
	if executor == nil {
		return ErrCanonicalFeedbackExecutorUninitialized
	}
	if !canonicalFeedbackDeadlineValid(deadline) {
		return ErrCanonicalFeedbackExecutorDeadline
	}
	executor.mu.Lock()
	switch executor.state {
	case canonicalFeedbackExecutorResetDrained:
		executor.mu.Unlock()
		return nil
	case canonicalFeedbackExecutorActive:
		executor.state = canonicalFeedbackExecutorResetDraining
	case canonicalFeedbackExecutorResetDraining:
		// A previous bounded wait may have timed out while the synchronous
		// publisher was still inside its own earlier deadline. Admission has
		// remained fenced; resume waiting for the same in-flight execution.
	default:
		executor.mu.Unlock()
		return ErrCanonicalFeedbackExecutorState
	}
	executor.mu.Unlock()
	return executor.waitForDrain(deadline,
		canonicalFeedbackExecutorResetDraining,
		canonicalFeedbackExecutorResetDrained)
}

// ResetNeutral publishes the successor-generation recoverable Neutral. Only
// its terminal acceptance rotates the private persona-generation binding and
// reopens ordinary execution; a proven rejection leaves the executor fenced
// so the exact reset neutral can be retried.
func (executor *ControllerPersonaCanonicalFeedbackExecutor) ResetNeutral(
	execution ControllerPersonaLocalExecution,
	deadline time.Time,
) error {
	if executor == nil {
		return ErrCanonicalFeedbackExecutorUninitialized
	}
	executor.mu.Lock()
	if executor.state != canonicalFeedbackExecutorResetDrained ||
		executor.inFlight {
		executor.mu.Unlock()
		return ErrCanonicalFeedbackExecutorState
	}
	if !canonicalFeedbackDeadlineValid(deadline) {
		executor.mu.Unlock()
		return ErrCanonicalFeedbackExecutorDeadline
	}
	successor, ok := canonicalFeedbackSuccessorBinding(executor.binding,
		executor.authorizedPersonaGeneration, execution)
	if !ok {
		executor.mu.Unlock()
		return ErrInvalidCanonicalFeedbackBinding
	}
	executor.state = canonicalFeedbackExecutorResetNeutralizing
	executor.inFlight = true
	executor.mu.Unlock()

	err := executor.publish(successor, execution,
		ControllerPersonaCanonicalFeedbackRecoverableNeutral, deadline)
	executor.mu.Lock()
	executor.inFlight = false
	if executor.state == canonicalFeedbackExecutorResetNeutralizing {
		if err == nil {
			executor.binding = successor
			executor.authorizedPersonaGeneration = execution.Generation
			executor.state = canonicalFeedbackExecutorActive
		} else {
			executor.state = canonicalFeedbackExecutorResetDrained
		}
	} else if executor.state == canonicalFeedbackExecutorTerminalDraining {
		// CancelAndDrain may upgrade a timed-out reset terminal while the
		// synchronous publisher is returning. Preserve its permanent fence.
		// An accepted neutral still rotates the private generation so the
		// following disconnect Stop is bound to the exact successor.
		if err == nil {
			executor.binding = successor
			executor.authorizedPersonaGeneration = execution.Generation
		}
	} else {
		err = errors.Join(err, ErrCanonicalFeedbackExecutorState)
	}
	executor.mu.Unlock()
	executor.signalProgress()
	return err
}

// CancelAndDrain permanently fences ordinary execution and joins a possible
// in-flight publication. It does not emit Stop; the exact retained import must
// separately authorize DisconnectNeutral.
func (executor *ControllerPersonaCanonicalFeedbackExecutor) CancelAndDrain(
	deadline time.Time,
) error {
	if executor == nil {
		return ErrCanonicalFeedbackExecutorUninitialized
	}
	if !canonicalFeedbackDeadlineValid(deadline) {
		return ErrCanonicalFeedbackExecutorDeadline
	}
	executor.mu.Lock()
	switch executor.state {
	case canonicalFeedbackExecutorTerminalDrained,
		canonicalFeedbackExecutorStopped:
		executor.mu.Unlock()
		return nil
	case canonicalFeedbackExecutorTerminalDraining:
		// Idempotently resume a prior bounded terminal wait. No admission was
		// reopened by the timeout.
	case canonicalFeedbackExecutorActive,
		canonicalFeedbackExecutorResetDraining,
		canonicalFeedbackExecutorResetDrained,
		canonicalFeedbackExecutorResetNeutralizing:
		executor.state = canonicalFeedbackExecutorTerminalDraining
	default:
		executor.mu.Unlock()
		return ErrCanonicalFeedbackExecutorState
	}
	executor.mu.Unlock()
	return executor.waitForDrain(deadline,
		canonicalFeedbackExecutorTerminalDraining,
		canonicalFeedbackExecutorTerminalDrained)
}

// DisconnectNeutral publishes the successor-generation terminal Stop. A
// proven rejection leaves the terminal drain fenced for an exact retry; only
// accepted Stop transitions the executor to permanently Stopped.
func (executor *ControllerPersonaCanonicalFeedbackExecutor) DisconnectNeutral(
	execution ControllerPersonaLocalExecution,
	deadline time.Time,
) error {
	if executor == nil {
		return ErrCanonicalFeedbackExecutorUninitialized
	}
	executor.mu.Lock()
	if executor.state != canonicalFeedbackExecutorTerminalDrained ||
		executor.inFlight {
		executor.mu.Unlock()
		return ErrCanonicalFeedbackExecutorState
	}
	if !canonicalFeedbackDeadlineValid(deadline) {
		executor.mu.Unlock()
		return ErrCanonicalFeedbackExecutorDeadline
	}
	successor, ok := canonicalFeedbackSuccessorBinding(executor.binding,
		executor.authorizedPersonaGeneration, execution)
	if !ok {
		executor.mu.Unlock()
		return ErrInvalidCanonicalFeedbackBinding
	}
	executor.state = canonicalFeedbackExecutorTerminalStopping
	executor.inFlight = true
	executor.mu.Unlock()

	err := executor.publish(successor, execution,
		ControllerPersonaCanonicalFeedbackTerminalStop, deadline)
	executor.mu.Lock()
	executor.inFlight = false
	if err == nil {
		executor.binding = successor
		executor.authorizedPersonaGeneration = execution.Generation
		executor.state = canonicalFeedbackExecutorStopped
	} else {
		executor.state = canonicalFeedbackExecutorTerminalDrained
	}
	executor.mu.Unlock()
	executor.signalProgress()
	return err
}

// Snapshot returns a value copy and never calls the publisher.
func (executor *ControllerPersonaCanonicalFeedbackExecutor) Snapshot() (
	ControllerPersonaCanonicalFeedbackExecutorSnapshot,
	bool,
) {
	if executor == nil {
		return ControllerPersonaCanonicalFeedbackExecutorSnapshot{}, false
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return ControllerPersonaCanonicalFeedbackExecutorSnapshot{
		PersonaGeneration:           executor.binding.PersonaGeneration,
		AuthorizedPersonaGeneration: executor.authorizedPersonaGeneration,
		DeviceGeneration:            executor.binding.DeviceGeneration,
		TransportGeneration:         executor.binding.TransportGeneration,
		OwnershipEpoch:              executor.binding.OwnershipEpoch,
		Active:                      executor.state == canonicalFeedbackExecutorActive,
		ResetDrained:                executor.state == canonicalFeedbackExecutorResetDrained,
		TerminalDrained:             executor.state == canonicalFeedbackExecutorTerminalDrained,
		Stopped:                     executor.state == canonicalFeedbackExecutorStopped,
		ExecutionInFlight:           executor.inFlight,
		GuideLEDObserved:            executor.guideLEDObserved,
		GuideLEDOrder:               executor.guideLEDOrder,
		GuideLED:                    executor.guideLED,
	}, true
}

func (executor *ControllerPersonaCanonicalFeedbackExecutor) publish(
	binding ControllerPersonaFeedbackBindingV1,
	execution ControllerPersonaLocalExecution,
	intent ControllerPersonaCanonicalFeedbackIntent,
	deadline time.Time,
) error {
	return executor.publishUntil(binding, execution, intent,
		deadline, 0)
}

func (executor *ControllerPersonaCanonicalFeedbackExecutor) publishUntil(
	binding ControllerPersonaFeedbackBindingV1,
	execution ControllerPersonaLocalExecution,
	intent ControllerPersonaCanonicalFeedbackIntent,
	deadline time.Time,
	expiresAtMicroseconds uint64,
) (err error) {
	// Both synchronous host commands and asynchronous renewals use this edge.
	// Convert a publisher/clock panic to an uncertain failure so the caller can
	// clear inFlight and retire safely, rather than crashing the timer goroutine.
	defer func() {
		if recover() != nil {
			err = errors.Join(errDormantRetainedUSBLocalPanic,
				ErrProductionBrokerFeedbackAmbiguous)
		}
	}()
	timestamp, ok := executor.clock()
	if !ok {
		return ErrCanonicalFeedbackClockUnavailable
	}
	frame, err := ControllerPersonaCanonicalFeedbackFrame(binding, execution,
		timestamp, intent)
	if err != nil {
		return err
	}
	if expiresAtMicroseconds != 0 && frame.Command == controllerfeedback.CommandApply {
		if timestamp >= expiresAtMicroseconds {
			frame.Command = controllerfeedback.CommandNeutral
			frame.BodyLow, frame.BodyHigh = 0, 0
			frame.LeftTrigger, frame.RightTrigger = 0, 0
		} else {
			frame.TimeToLiveMicroseconds = min(frame.TimeToLiveMicroseconds,
				expiresAtMicroseconds-timestamp)
		}
	}
	var wire ControllerPersonaCanonicalFeedbackWireV1
	if err := frame.MarshalTo(wire[:]); err != nil {
		return err
	}
	return executor.publisher.PublishControllerFeedback(wire, deadline)
}

func (executor *ControllerPersonaCanonicalFeedbackExecutor) finishExecution(
	binding ControllerPersonaFeedbackBindingV1,
	promoteBinding bool,
	delivered bool,
) {
	executor.mu.Lock()
	if delivered && promoteBinding &&
		binding.PersonaGeneration == executor.authorizedPersonaGeneration {
		executor.binding = binding
	}
	executor.inFlight = false
	executor.mu.Unlock()
	executor.signalProgress()
}

func (executor *ControllerPersonaCanonicalFeedbackExecutor) signalProgress() {
	select {
	case executor.progress <- struct{}{}:
	default:
	}
}

func (executor *ControllerPersonaCanonicalFeedbackExecutor) waitForDrain(
	deadline time.Time,
	draining controllerPersonaCanonicalFeedbackExecutorState,
	drained controllerPersonaCanonicalFeedbackExecutorState,
) error {
	for {
		executor.mu.Lock()
		if executor.state != draining {
			executor.mu.Unlock()
			return ErrCanonicalFeedbackExecutorState
		}
		if !executor.inFlight {
			executor.state = drained
			executor.mu.Unlock()
			return nil
		}
		executor.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ErrCanonicalFeedbackExecutorDeadline
		}
		timer := time.NewTimer(remaining)
		select {
		case <-executor.progress:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			return ErrCanonicalFeedbackExecutorDeadline
		}
	}
}

func canonicalFeedbackSuccessorBinding(
	current ControllerPersonaFeedbackBindingV1,
	authorizedGeneration uint64,
	execution ControllerPersonaLocalExecution,
) (ControllerPersonaFeedbackBindingV1, bool) {
	if execution.Action != ControllerPersonaClearOutputs ||
		!execution.valid() || authorizedGeneration == ^uint64(0) ||
		execution.Generation != authorizedGeneration+1 {
		return ControllerPersonaFeedbackBindingV1{}, false
	}
	successor := current
	successor.PersonaGeneration = execution.Generation
	return successor, successor.validate() == nil
}

func canonicalFeedbackOutputFreeAction(action ControllerPersonaAction) bool {
	switch action {
	case ControllerPersonaBeginMetadata,
		ControllerPersonaPermitNormalUpstream,
		ControllerPersonaGateNormalUpstream,
		ControllerPersonaApplyMetadataAcknowledgement,
		ControllerPersonaCompletePowerOff,
		ControllerPersonaPerformReset:
		return true
	default:
		return false
	}
}

func canonicalFeedbackDeadlineValid(deadline time.Time) bool {
	return !deadline.IsZero() && deadline.After(time.Now())
}

var _ ControllerPersonaLocalExecutor = (*ControllerPersonaCanonicalFeedbackExecutor)(nil)

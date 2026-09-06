package xboxone

import (
	"errors"
	"sync"
	"time"
)

// controllerPersonaTimedFeedbackExecutor owns Direct Motor program time, not
// controller mapping. Every effective state still crosses the authenticated
// canonical executor and physical acknowledgement. Execute accepts a program
// after its initial state is acknowledged, not after its duration elapses.
// Renewals do not occupy the persona's serialized input lane.
//
// A zero-delay repeat is one continuous interval. Nonzero Delay phase placement
// is not inferred from a zero-delay capture and remains unsupported. Neutral
// and cancellation programs do not require a phase interpretation.
type controllerPersonaTimedFeedbackExecutor struct {
	mu        sync.Mutex
	operation chan struct{}
	core      *ControllerPersonaCanonicalFeedbackExecutor
	lease     time.Duration
	renewal   time.Duration
	timer     *time.Timer
	epoch     uint64
	sequence  uint64
	program   ControllerPersonaLocalExecution
	startedAt uint64
	endsAt    uint64
	active    bool
	reset     bool
	stopped   bool
	lastError error
	// Installed before publication. Must only record/signal failure; must not
	// synchronously drain this executor while its operation is still owned.
	onFailure func(error)
}

func newControllerPersonaTimedFeedbackExecutor(core *ControllerPersonaCanonicalFeedbackExecutor) *controllerPersonaTimedFeedbackExecutor {
	if core == nil {
		return nil
	}
	core.mu.Lock()
	binding := core.binding
	core.mu.Unlock()
	if binding.validate() != nil {
		return nil
	}
	lease := time.Duration(binding.TimeToLiveMicroseconds) * time.Microsecond
	executor := &controllerPersonaTimedFeedbackExecutor{
		operation: make(chan struct{}, 1), core: core,
		lease: lease, renewal: max(time.Microsecond, min(lease/2, 100*time.Millisecond)),
	}
	executor.operation <- struct{}{}
	return executor
}

func (executor *controllerPersonaTimedFeedbackExecutor) validateDeadline(deadline time.Time) error {
	if executor == nil || executor.core == nil || executor.operation == nil {
		return ErrCanonicalFeedbackExecutorUninitialized
	}
	if !canonicalFeedbackDeadlineValid(deadline) {
		return ErrCanonicalFeedbackExecutorDeadline
	}
	return nil
}

func (executor *controllerPersonaTimedFeedbackExecutor) acquire(deadline time.Time) error {
	if err := executor.validateDeadline(deadline); err != nil {
		return err
	}
	// The uncontended host/renewal edge needs no timer allocation.
	select {
	case <-executor.operation:
		return nil
	default:
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-executor.operation:
		if !canonicalFeedbackDeadlineValid(deadline) {
			executor.release()
			return ErrCanonicalFeedbackExecutorDeadline
		}
		return nil
	case <-timer.C:
		return ErrCanonicalFeedbackExecutorDeadline
	}
}

func (executor *controllerPersonaTimedFeedbackExecutor) release() {
	executor.operation <- struct{}{}
}

func (executor *controllerPersonaTimedFeedbackExecutor) cancelLocked() {
	executor.active = false
	if executor.epoch == ^uint64(0) {
		// Never reuse a timer identity, even at counter exhaustion.
		executor.stopped = true
	} else {
		executor.epoch++
	}
	if executor.timer != nil {
		executor.timer.Stop()
		executor.timer = nil
	}
}

// Called only while operation is owned. Renewals need distinct CFBK anti-replay
// sequences; original selection order is a lower bound, not a reusable ID.
func (executor *controllerPersonaTimedFeedbackExecutor) nextExecution(execution ControllerPersonaLocalExecution) (ControllerPersonaLocalExecution, error) {
	if !execution.valid() || executor.sequence == ^uint64(0) {
		return ControllerPersonaLocalExecution{}, ErrInvalidCanonicalFeedbackExecution
	}
	executor.sequence = max(executor.sequence+1, execution.Order)
	execution.Order = executor.sequence
	return execution, nil
}

func directMotorHasEffectiveLevel(body RumbleBodyV1) bool {
	return enabledMotorLevel(body.Enabled, MotorLeftVibration, body.LeftVibration) != 0 ||
		enabledMotorLevel(body.Enabled, MotorRightVibration, body.RightVibration) != 0 ||
		enabledMotorLevel(body.Enabled, MotorLeftImpulse, body.LeftImpulse) != 0 ||
		enabledMotorLevel(body.Enabled, MotorRightImpulse, body.RightImpulse) != 0
}

func (executor *controllerPersonaTimedFeedbackExecutor) Execute(execution ControllerPersonaLocalExecution, deadline time.Time) error {
	if !execution.valid() {
		return ErrInvalidCanonicalFeedbackExecution
	}
	if err := executor.acquire(deadline); err != nil {
		return err
	}
	defer executor.release()
	// Reject foreign work before it cancels a valid program. The core remains
	// binding authority; only this wrapper calls it in production.
	snapshot, ok := executor.core.Snapshot()
	if !ok || execution.Generation != snapshot.AuthorizedPersonaGeneration {
		return ErrInvalidCanonicalFeedbackBinding
	}
	if !snapshot.Active {
		return ErrCanonicalFeedbackExecutorState
	}
	activeMotor := execution.Action == ControllerPersonaApplyDirectMotor &&
		!execution.DirectMotor.IsCancellation() && directMotorHasEffectiveLevel(execution.DirectMotor)
	if activeMotor && execution.DirectMotor.Delay != 0 {
		return ErrUnsupportedCanonicalFeedbackTiming
	}
	var startedAt, endsAt uint64
	if activeMotor {
		startedAt, ok = executor.core.clock()
		// Duration and Repeat are bytes; this is at most 652.8 seconds.
		duration := uint64(execution.DirectMotor.Duration) * 10_000 *
			(uint64(execution.DirectMotor.Repeat) + 1)
		if !ok || startedAt > ^uint64(0)-duration {
			return ErrCanonicalFeedbackClockUnavailable
		}
		endsAt = startedAt + duration
	}
	executor.mu.Lock()
	if executor.stopped || executor.reset {
		executor.mu.Unlock()
		return ErrCanonicalFeedbackExecutorState
	}
	if execution.Action != ControllerPersonaApplyDirectMotor &&
		execution.Action != ControllerPersonaClearOutputs &&
		execution.Action != ControllerPersonaPerformReset &&
		execution.Action != ControllerPersonaCompletePowerOff {
		executor.mu.Unlock()
		return executor.core.Execute(execution, deadline)
	}
	executor.cancelLocked()
	if executor.stopped {
		executor.mu.Unlock()
		return ErrCanonicalFeedbackExecutorState
	}
	epoch := executor.epoch
	if activeMotor {
		executor.program, executor.startedAt, executor.endsAt = execution, startedAt, endsAt
		executor.active = true
	}
	executor.lastError = nil
	executor.mu.Unlock()
	if activeMotor {
		return executor.publishProgram(epoch, deadline)
	}
	if execution.Action == ControllerPersonaApplyDirectMotor {
		execution.DirectMotor = NewStopRumbleBody()
	}
	selected, err := executor.nextExecution(execution)
	if err != nil {
		return err
	}
	return executor.core.Execute(selected, deadline)
}

// Called only with operation owned. Stale timer callbacks may finish running
// after replacement/drain, but can never publish through a successor epoch.
func (executor *controllerPersonaTimedFeedbackExecutor) publishProgram(epoch uint64, deadline time.Time) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.Join(errDormantRetainedUSBLocalPanic, ErrProductionBrokerFeedbackAmbiguous)
			executor.failPublication(err)
		}
	}()
	executor.mu.Lock()
	if !executor.active || executor.epoch != epoch || executor.reset || executor.stopped {
		executor.mu.Unlock()
		return ErrCanonicalFeedbackExecutorState
	}
	execution, startedAt, endsAt := executor.program, executor.startedAt, executor.endsAt
	executor.mu.Unlock()
	now, ok := executor.core.clock()
	if !ok || now < startedAt {
		err = ErrCanonicalFeedbackClockUnavailable
	} else {
		if now >= endsAt {
			execution.DirectMotor = NewStopRumbleBody()
		} else {
			execution.DirectMotor.Duration = 255
			execution.DirectMotor.Delay, execution.DirectMotor.Repeat = 0, 0
		}
		var selected ControllerPersonaLocalExecution
		selected, err = executor.nextExecution(execution)
		if err == nil {
			err = executor.core.executeUntil(selected, deadline, endsAt)
		}
	}
	// Schedule after acknowledgement against the same absolute program end.
	// A late ACK must not extend the effect or admit a timer after a failed call.
	after, afterOK := executor.core.clock()
	if err == nil && (!afterOK || after < now) {
		err = ErrCanonicalFeedbackClockUnavailable
	}
	if err == nil && !canonicalFeedbackDeadlineValid(deadline) {
		err = errors.Join(ErrProductionBrokerFeedbackAmbiguous, ErrCanonicalFeedbackExecutorDeadline)
	}
	if err != nil {
		// An actual publication may have crossed the consumer boundary even
		// if a concurrent drain invalidated its timer epoch. Do not discard
		// that uncertainty as though this were merely an unstarted callback.
		executor.failPublication(err)
		return err
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.epoch != epoch || executor.reset || executor.stopped {
		return nil
	}
	if after >= endsAt {
		if now >= endsAt {
			executor.active = false
		} else {
			// The last frame may have been Apply. Send an explicit expiry
			// Neutral even if its acknowledgement crossed the program end;
			// not every physical backend independently implements a TTL timer.
			executor.timer = time.AfterFunc(time.Microsecond, func() { executor.renew(epoch) })
		}
		return nil
	}
	remaining := time.Duration(endsAt-after) * time.Microsecond
	// Account for time spent waiting for acceptance of this publication.
	elapsed := time.Duration(after-now) * time.Microsecond
	wait := max(time.Microsecond, min(executor.renewal-elapsed, remaining))
	executor.timer = time.AfterFunc(wait, func() { executor.renew(epoch) })
	return nil
}

func (executor *controllerPersonaTimedFeedbackExecutor) failProgram(epoch uint64, err error) {
	executor.mu.Lock()
	if executor.epoch != epoch || !executor.active {
		executor.mu.Unlock()
		return
	}
	onFailure := executor.failLocked(err)
	executor.mu.Unlock()
	if onFailure != nil {
		onFailure(err)
	}
}

// operation must still be owned: no successor program can have started while
// a predecessor publication reports its result. A drain may already be fenced.
func (executor *controllerPersonaTimedFeedbackExecutor) failPublication(err error) {
	executor.mu.Lock()
	onFailure := executor.failLocked(err)
	executor.mu.Unlock()
	if onFailure != nil {
		onFailure(err)
	}
}

func (executor *controllerPersonaTimedFeedbackExecutor) failLocked(err error) func(error) {
	executor.cancelLocked()
	executor.stopped = true
	if executor.lastError != nil {
		return nil
	}
	executor.lastError = err
	return executor.onFailure
}

func (executor *controllerPersonaTimedFeedbackExecutor) renew(epoch uint64) {
	// Clock/publisher implementations must not panic, but a background failure
	// must be contained just as a host-selected action is contained by adapter.
	defer func() {
		if recover() != nil {
			executor.failProgram(epoch, errors.Join(errDormantRetainedUSBLocalPanic,
				ErrProductionBrokerFeedbackAmbiguous))
		}
	}()
	deadline := time.Now().Add(executor.lease)
	if err := executor.acquire(deadline); err != nil {
		executor.failProgram(epoch, err)
		return
	}
	defer executor.release()
	_ = executor.publishProgram(epoch, deadline)
}

func (executor *controllerPersonaTimedFeedbackExecutor) ResetAndDrain(deadline time.Time) error {
	if err := executor.validateDeadline(deadline); err != nil {
		return err
	}
	executor.mu.Lock()
	if executor.stopped {
		executor.mu.Unlock()
		return ErrCanonicalFeedbackExecutorState
	}
	executor.reset = true
	executor.cancelLocked()
	executor.mu.Unlock()
	if err := executor.acquire(deadline); err != nil {
		return err
	}
	defer executor.release()
	return executor.core.ResetAndDrain(deadline)
}

func (executor *controllerPersonaTimedFeedbackExecutor) ResetNeutral(execution ControllerPersonaLocalExecution, deadline time.Time) error {
	if err := executor.acquire(deadline); err != nil {
		return err
	}
	defer executor.release()
	executor.mu.Lock()
	allowed := executor.reset && !executor.stopped
	executor.mu.Unlock()
	if !allowed {
		return ErrCanonicalFeedbackExecutorState
	}
	selected, err := executor.nextExecution(execution)
	if err == nil {
		err = executor.core.ResetNeutral(selected, deadline)
	}
	if err == nil {
		executor.mu.Lock()
		executor.reset = false
		executor.mu.Unlock()
	}
	return err
}

func (executor *controllerPersonaTimedFeedbackExecutor) CancelAndDrain(deadline time.Time) error {
	if err := executor.validateDeadline(deadline); err != nil {
		return err
	}
	executor.mu.Lock()
	executor.stopped = true
	executor.cancelLocked()
	executor.mu.Unlock()
	if err := executor.acquire(deadline); err != nil {
		return err
	}
	defer executor.release()
	return executor.core.CancelAndDrain(deadline)
}

func (executor *controllerPersonaTimedFeedbackExecutor) DisconnectNeutral(execution ControllerPersonaLocalExecution, deadline time.Time) error {
	if err := executor.acquire(deadline); err != nil {
		return err
	}
	defer executor.release()
	selected, err := executor.nextExecution(execution)
	if err != nil {
		return err
	}
	return executor.core.DisconnectNeutral(selected, deadline)
}

var _ ControllerPersonaLocalExecutor = (*controllerPersonaTimedFeedbackExecutor)(nil)

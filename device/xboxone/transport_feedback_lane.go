package xboxone

// localWaitLocked permits IN to proceed only after ordinary feedback ownership
// has actually moved out of the primary persona lane. Lifecycle actions,
// including ClearOutputs, never detach. EP0 and further OUT commands remain
// ordered behind both admitted ordinary feedback and its exact local retry.
func (coordinator *controllerPersonaTransportCoordinator) localWaitLocked(
	lane controllerPersonaTransportLane,
) (controllerPersonaTransportAdmission, bool) {
	active := coordinator.activeLocal
	if active.lease.valid() && (!active.detachedFeedback || lane != controllerPersonaTransportInterruptIn) {
		return controllerPersonaTransportAdmission{
			disposition: controllerPersonaTransportLocalRequired,
			waitReason:  controllerPersonaTransportLocalPending,
			action:      active.lease.action,
		}, true
	}
	if lane != controllerPersonaTransportInterruptIn && coordinator.engine.ordinaryFeedbackPending() {
		return controllerPersonaTransportAdmission{
			disposition: controllerPersonaTransportLocalRequired,
			waitReason:  controllerPersonaTransportLocalPending,
			action:      coordinator.engine.Snapshot().OrdinaryFeedbackAction,
		}, true
	}
	return controllerPersonaTransportAdmission{}, false
}

func (coordinator *controllerPersonaTransportCoordinator) pendingOrdinaryFeedbackRetry() bool {
	if coordinator == nil {
		return false
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return coordinator.engine != nil && coordinator.engine.ordinaryFeedbackRetryPending()
}

func (coordinator *controllerPersonaTransportCoordinator) pendingOrdinaryFeedback() bool {
	if coordinator == nil {
		return false
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return (coordinator.pending.claim.Valid() &&
		isOrdinaryFeedbackAction(coordinator.pending.claim.Action())) ||
		coordinator.engine.ordinaryFeedbackRetryPending()
}

// The retry is already an immutable, previously selected ordinary effect. Its
// admission is independent of an IN response/retry, never a new host command or
// lifecycle selector. No external effect occurs under the coordinator lock.
func (coordinator *controllerPersonaTransportCoordinator) admitFeedbackRetryLocked(
	nowMS uint64,
) (controllerPersonaLocalLease, bool, error) {
	if coordinator.nextLocalToken == ^uint64(0) {
		return controllerPersonaLocalLease{}, false, errControllerPersonaTransportTokenExhausted
	}
	order, err := coordinator.allocateOrderLocked()
	if err != nil {
		return controllerPersonaLocalLease{}, false, err
	}
	claim, present, err := coordinator.engine.admitOrdinaryFeedbackRetry(nowMS)
	if err != nil || !present {
		return controllerPersonaLocalLease{}, false, err
	}
	coordinator.nextLocalToken++
	lease := controllerPersonaLocalLease{
		owner: coordinator, token: coordinator.nextLocalToken,
		generation: claim.Generation(), order: order, action: claim.Action(),
	}
	lease.directMotor, _ = claim.DirectMotor()
	lease.guideLED, _ = claim.GuideLED()
	coordinator.activeLocal = controllerPersonaActiveLocal{
		lease: lease, claim: claim, detachedFeedback: true,
	}
	return lease, true, nil
}

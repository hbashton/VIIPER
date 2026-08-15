package udecx

import (
	"strings"
	"testing"
)

func TestNativeEndpointQuiescenceUsesReadOnlyUdeCxQueueState(t *testing.T) {
	broker := nativeContractSource(t, "native", "udecx", "driver", "Broker.c")
	controller := nativeContractSource(t, "native", "udecx", "driver", "Controller.c")
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")
	header := nativeContractSource(t, "native", "udecx", "driver", "ViiperUde.h")

	// UdeCx exclusively owns the associated endpoint queue's START/PURGE
	// state. VIIPER may observe that queue, but must never mutate it.
	for _, mutation := range []string{
		"WdfIoQueuePurge(",
		"WdfIoQueuePurgeSynchronously(",
		"WdfIoQueueStart(",
		"WdfIoQueueStop(",
		"WdfIoQueueStopSynchronously(",
		"WdfIoQueueDrain(",
		"WdfIoQueueDrainSynchronously(",
	} {
		if strings.Contains(device, mutation) {
			t.Fatalf("UdeCx-associated queue state is client-mutated by %s", mutation)
		}
	}
	createQueue := normalizedContract(nativeCFunction(t, device, "ViiperCreateEndpointQueue"))
	if !strings.Contains(createQueue,
		"UdecxUsbEndpointSetWdfIoQueue(Endpoint, endpointContext->Queue);") {
		t.Fatal("endpoint queue is no longer explicitly associated with UdeCx")
	}

	// A WDF callback can be delivered, then preempted before its first
	// BrokerLock acquisition. DriverNoRequests closes that otherwise invisible
	// window; ActiveOperations joins the callback's terminal DPC afterward.
	quiesce := normalizedContract(nativeCFunction(t, device, "ViiperWaitForEndpointQuiescence"))
	requireContractOrder(t, quiesce,
		"KeWaitForSingleObject( &endpointContext->OperationsDrained",
		"WdfSpinLockAcquire(controllerContext->BrokerLock);",
		"WdfIoQueueGetState(endpointContext->Queue, NULL, NULL);",
		"WdfIoQueueDriverNoRequests",
		"endpointContext->ActiveOperations",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"if (quiescent)",
		"return;",
		"KeDelayExecutionThread(")
	purgeQuiescence := normalizedContract(nativeCFunction(
		t, device, "ViiperWaitForEndpointPurgeQuiescence"))
	requireContractOrder(t, purgeQuiescence,
		"KeWaitForSingleObject( &endpointContext->OperationsDrained",
		"WdfSpinLockAcquire(controllerContext->BrokerLock);",
		"WdfIoQueueGetState( endpointContext->Queue, &queuedRequests, &driverRequests);",
		"endpointContext->PurgeOutstanding",
		"endpointContext->Purging",
		"WdfIoQueueDriverNoRequests",
		"driverRequests == 0",
		"endpointContext->ActiveOperations",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"if (quiescent)",
		"return;",
		"KeDelayExecutionThread(")
	for _, forbidden := range []string{
		"WDF_IO_QUEUE_READY",
		"WdfIoQueueNoRequests",
		"WDF_IO_QUEUE_IDLE",
		"WDF_IO_QUEUE_PURGED",
		"queuedRequests == 0",
	} {
		if strings.Contains(purgeQuiescence, forbidden) {
			t.Fatalf("purge quiescence incorrectly waits on UdeCx-owned queue state %q", forbidden)
		}
	}

	queueUrb := normalizedContract(nativeCFunction(t, broker, "ViiperQueueUrb"))
	requireContractOrder(t, queueUrb,
		"WdfSpinLockAcquire(controllerContext->BrokerLock);",
		"ViiperEndpointOperationStarted(endpoint);",
		"controllerContext->ShuttingDown",
		"controllerContext->BrokerFaulted",
		"deviceContext->InD0",
		"deviceContext->Resetting",
		"deviceContext->Purging",
		"endpointContext->Resetting",
		"endpointContext->Purging",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"if (!NT_SUCCESS(status))",
		"ViiperAllocatePendingSlot(")
	allocate := normalizedContract(nativeCFunction(t, broker, "ViiperAllocatePendingSlot"))
	requireContractOrder(t, allocate,
		"WdfSpinLockAcquire(ControllerContext->BrokerLock);",
		"ControllerContext->ShuttingDown",
		"ControllerContext->BrokerFaulted",
		"deviceContext->InD0",
		"endpointContext->Purging",
		"endpointContext->Resetting",
		"deviceContext->Resetting",
		"deviceContext->Purging",
		"pending->Request = Request;")

	purge := normalizedContract(nativeCFunction(t, device, "ViiperEvtEndpointPurge"))
	requireContractOrder(t, purge,
		"WdfSpinLockAcquire(controllerContext->BrokerLock);",
		"InterlockedExchange(&endpointContext->Purging, TRUE);",
		"InterlockedExchange(&endpointContext->StartAnnounced, FALSE);",
		"outstanding = InterlockedIncrement(&endpointContext->PurgeOutstanding);",
		"enqueueWorkItem = InterlockedCompareExchange( &endpointContext->PurgeWorkerActive, TRUE, FALSE) == FALSE;",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"ViiperPurgeEndpointOperations(Endpoint, STATUS_DEVICE_NOT_READY);",
		"ViiperQueueEndpointLifecycleEvent(Endpoint, ViiperUdeOperationEndpointPurge);",
		"if (enqueueWorkItem)",
		"WdfWorkItemEnqueue(endpointContext->PurgeWorkItem);")
	if strings.Contains(purge, "ViiperInvalidateEndpointInputReport") {
		t.Fatal("DISPATCH-level endpoint PURGE must defer wait-lock-backed input invalidation to its passive work item")
	}
	enqueueEnd := strings.Index(purge, "WdfWorkItemEnqueue(endpointContext->PurgeWorkItem);")
	if enqueueEnd < 0 || strings.Contains(purge[enqueueEnd+len("WdfWorkItemEnqueue(endpointContext->PurgeWorkItem);"):], "endpointContext") {
		t.Fatal("PURGE work-item enqueue must remain the callback's final endpoint-context access")
	}
	purgeWork := normalizedContract(nativeCFunction(t, device, "ViiperEvtEndpointPurgeWorkItem"))
	requireContractOrder(t, purgeWork,
		"WdfWorkItemGetParentObject(WorkItem)",
		"for (;;)",
		"endpointContext->PurgeOutstanding",
		"endpointContext->PurgeWorkerActive",
		"ViiperWaitForEndpointPurgeQuiescence( endpoint, &queueState, &queuedRequests, &driverRequests);",
		"ViiperInvalidateEndpointInputReport(endpoint);",
		"remaining = InterlockedDecrement(&endpointContext->PurgeOutstanding);",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"UdecxUsbEndpointPurgeComplete(endpoint);",
		"WdfSpinLockAcquire(controllerContext->BrokerLock);",
		"endpointContext->PurgeOutstanding",
		"InterlockedExchange(&endpointContext->PurgeWorkerActive, FALSE);")
	decrement := strings.Index(purgeWork,
		"remaining = InterlockedDecrement(&endpointContext->PurgeOutstanding);")
	complete := strings.Index(purgeWork, "UdecxUsbEndpointPurgeComplete(endpoint);")
	workerRelease := strings.LastIndex(purgeWork,
		"InterlockedExchange(&endpointContext->PurgeWorkerActive, FALSE);")
	if decrement < 0 || complete <= decrement || workerRelease <= complete {
		t.Fatal("PURGE worker must decrement before completion and retain worker ownership through synchronous callbacks")
	}
	if !strings.Contains(header, "WDFWORKITEM PurgeWorkItem;") ||
		!strings.Contains(header, "volatile LONG PurgeOutstanding;") ||
		!strings.Contains(header, "volatile LONG PurgeWorkerActive;") ||
		!strings.Contains(header, "EVT_WDF_WORKITEM ViiperEvtEndpointPurgeWorkItem;") {
		t.Fatal("endpoint context lost its counted passive PURGE worker state")
	}
	endpointAdd := normalizedContract(nativeCFunction(t, device, "ViiperEvtEndpointAdd"))
	requireContractOrder(t, endpointAdd,
		"KeInitializeEvent(&endpointContext->OperationsDrained, NotificationEvent, TRUE);",
		"WDF_WORKITEM_CONFIG_INIT(&workItemConfig, ViiperEvtEndpointPurgeWorkItem);",
		"workItemConfig.AutomaticSerialization = WdfFalse;",
		"attributes.ParentObject = endpoint;",
		"WdfWorkItemCreate( &workItemConfig, &attributes, &endpointContext->PurgeWorkItem);")
	requireContractOrder(t, endpointAdd,
		"WDF_WORKITEM_CONFIG_INIT(&workItemConfig, ViiperEvtEndpointResetWorkItem);",
		"workItemConfig.AutomaticSerialization = WdfFalse;",
		"attributes.ParentObject = endpoint;",
		"WdfWorkItemCreate( &workItemConfig, &attributes, &endpointContext->ResetWorkItem);")
	start := normalizedContract(nativeCFunction(t, device, "ViiperEvtEndpointStart"))
	if !strings.Contains(start, "ViiperActivateEndpoint(Endpoint);") {
		t.Fatal("explicit UdeCx START no longer opens the VIIPER endpoint admission gate")
	}
	activate := normalizedContract(nativeCFunction(t, device, "ViiperActivateEndpoint"))
	requireContractOrder(t, activate,
		"endpointContext->PurgeOutstanding",
		"InterlockedExchange(&endpointContext->Purging, FALSE);",
		"endpointContext->StartAnnounced, TRUE, FALSE",
		"ViiperQueueEndpointLifecycleEvent( Endpoint, ViiperUdeOperationEndpointStart);",
		"endpointContext->StartAnnounced, FALSE, TRUE")
	if strings.Contains(activate, "PurgeWorkerActive") {
		t.Fatal("synchronous final START must not wait for the still-executing PURGE worker to return")
	}
	reset := normalizedContract(nativeCFunction(t, device, "ViiperEvtEndpointReset"))
	if strings.Contains(reset, "ViiperInvalidateEndpointInputReport") {
		t.Fatal("DISPATCH-level endpoint RESET must defer wait-lock-backed input invalidation to its passive work item")
	}
	resetWork := normalizedContract(nativeCFunction(t, device, "ViiperEvtEndpointResetWorkItem"))
	requireContractOrder(t, resetWork,
		"resetCurrent = ViiperQuiesceResetByIdentity(",
		"deviceContext->DeviceId",
		"deviceContext->Generation",
		"endpointContext->Descriptor.bEndpointAddress",
		"FALSE, FALSE);",
		"if (!resetCurrent)",
		"WdfSpinLockAcquire(controllerContext->BrokerLock);",
		"InterlockedExchange(&endpointContext->Resetting, FALSE);",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"WdfRequestComplete(request, STATUS_DEVICE_NOT_READY);",
		"ViiperInvalidateEndpointInputReport(endpoint);",
		"ViiperQueueAcknowledgedEndpointLifecycleEvent(")

	controllerQuiesce := normalizedContract(nativeCFunction(t, device, "ViiperDrainControllerEndpointOperations"))
	requireContractOrder(t, controllerQuiesce,
		"ViiperAcquireDeviceLockShared(controllerContext);",
		"deviceContext->Endpoints[endpointIndex]",
		"ViiperWaitForEndpointQuiescence(endpoint);",
		"ViiperReleaseDeviceLockShared(controllerContext);")
	cleanup := normalizedContract(nativeCFunction(t, controller, "ViiperEvtDeviceSelfManagedIoCleanup"))
	requireContractOrder(t, cleanup,
		"InterlockedExchange(&context->ShuttingDown, TRUE);",
		"ViiperPurgeOwnerOperations(Device, STATUS_DEVICE_REMOVED);",
		"ViiperDrainControllerEndpointOperations(Device);",
		"ViiperDrainUrbCompletions(Device);",
		"context->PendingOperations",
		"context->PendingCompletions",
		"IsListEmpty(&context->CompletionQueue)",
		"context->CompletionDpcActive",
		"ViiperBeginControllerShutdown(Device);")
	shutdown := normalizedContract(nativeCFunction(t, device, "ViiperBeginControllerShutdown"))
	if strings.Contains(shutdown, "WdfIoQueueGetState") ||
		strings.Contains(shutdown, "KeWaitForSingleObject") {
		t.Fatal("controller shutdown consumes children before, or waits after, the queue proof")
	}
	if !strings.Contains(shutdown, "UdecxUsbDevicePlugOutAndDelete(devices[index]);") {
		t.Fatal("controller shutdown no longer consumes the snapshotted UdeCx children")
	}
}

func TestNativeEndpointPurgeWorkerCountsRepeatedAndReentrantCallbacks(t *testing.T) {
	type purgeState struct {
		purging          bool
		queueReady       bool
		driverNoRequest  bool
		driverRequests   int
		activeOperations int
		queuedHostPolls  int
		outstanding      int
		workerActive     bool
		enqueues         int
		callbacks        int
		completions      int
	}

	beginPurge := func(state *purgeState) {
		state.purging = true
		// The callback closes upstream delivery. The associated WDF queue may
		// retain READY bookkeeping until PurgeComplete acknowledges it.
		state.callbacks++
		state.outstanding++
		if !state.workerActive {
			state.workerActive = true
			state.enqueues++
		}
	}
	start := func(state *purgeState) bool {
		// START is allowed immediately after the final counter decrement, even
		// though the completing worker remains active until the callback returns.
		if state.outstanding != 0 {
			return false
		}
		state.purging = false
		state.queueReady = true
		return true
	}
	quiescent := func(state *purgeState) bool {
		// queuedHostPolls is intentionally excluded: those requests remain owned
		// by UdeCx while delivery is stopped.
		return state.outstanding > 0 && state.purging &&
			state.driverNoRequest && state.driverRequests == 0 &&
			state.activeOperations == 0
	}
	completeOne := func(state *purgeState, duringComplete func()) bool {
		if !state.workerActive || !quiescent(state) {
			return false
		}
		// This is the source contract's decrement-before-PurgeComplete boundary.
		state.outstanding--
		state.completions++
		if duringComplete != nil {
			duringComplete()
		}
		if state.outstanding == 0 {
			state.workerActive = false
		}
		return true
	}

	state := purgeState{queueReady: true, driverNoRequest: true, queuedHostPolls: 7}
	beginPurge(&state)
	beginPurge(&state)
	if state.outstanding != 2 || state.enqueues != 1 || !state.workerActive {
		t.Fatalf("overlapping PURGE callbacks were not coalesced onto one counted worker: %+v", state)
	}
	if !completeOne(&state, func() {
		if start(&state) {
			t.Fatal("non-final PURGE completion admitted a synchronous START")
		}
		if !state.workerActive || state.outstanding != 1 {
			t.Fatalf("worker ownership/count changed before non-final completion returned: %+v", state)
		}
	}) {
		t.Fatal("first counted PURGE did not complete after driver quiescence")
	}
	if !completeOne(&state, func() {
		if !start(&state) {
			t.Fatal("final counter decrement did not admit synchronous START")
		}
		if !state.workerActive {
			t.Fatal("worker ownership was released before synchronous completion callbacks")
		}
		beginPurge(&state) // reentrant from PurgeComplete
		if state.enqueues != 1 || state.outstanding != 1 || !state.purging {
			t.Fatalf("reentrant PURGE was lost or redundantly enqueued: %+v", state)
		}
	}) {
		t.Fatal("second counted PURGE did not complete")
	}
	if !state.workerActive || state.outstanding != 1 {
		t.Fatalf("worker did not retain a reentrant PURGE: %+v", state)
	}
	if !completeOne(&state, func() {
		if !start(&state) || !state.workerActive {
			t.Fatalf("final reentrant completion did not expose the intended START boundary: %+v", state)
		}
	}) {
		t.Fatal("reentrant PURGE did not complete")
	}
	if state.outstanding != 0 || state.workerActive || state.enqueues != 1 ||
		state.completions != state.callbacks || state.queuedHostPolls != 7 {
		t.Fatalf("counted worker lost a callback or consumed UdeCx-owned polls: %+v", state)
	}

	for _, test := range []struct {
		name  string
		state purgeState
		want  bool
	}{
		{name: "ready with queued class-owned host polls", state: purgeState{
			purging: true, queueReady: true, driverNoRequest: true,
			outstanding: 1, queuedHostPolls: 99}, want: true},
		{name: "non-ready queue is also observational", state: purgeState{
			purging: true, driverNoRequest: true, outstanding: 1, queuedHostPolls: 99}, want: true},
		{name: "framework callback delivered", state: purgeState{
			purging: true, driverNoRequest: false, outstanding: 1}},
		{name: "driver request held", state: purgeState{
			purging: true, driverNoRequest: true, driverRequests: 1, outstanding: 1}},
		{name: "VIIPER operation held", state: purgeState{
			purging: true, driverNoRequest: true, activeOperations: 1, outstanding: 1}},
		{name: "no outstanding callback", state: purgeState{
			purging: true, driverNoRequest: true}},
		{name: "START reopened gate", state: purgeState{
			driverNoRequest: true, outstanding: 1}},
	} {
		if got := quiescent(&test.state); got != test.want {
			t.Fatalf("%s: quiescent=%v want %v: %+v", test.name, got, test.want, test.state)
		}
	}
}

func TestNativeResetQuiescenceIsExactGenerationAndFailClosed(t *testing.T) {
	broker := nativeContractSource(t, "native", "udecx", "driver", "Broker.c")
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")

	identityProof := normalizedContract(nativeCFunction(t, device, "ViiperQuiesceResetByIdentity"))
	requireContractOrder(t, identityProof,
		"ViiperAcquireDeviceLockShared(controllerContext);",
		"deviceContext->DeviceId != DeviceId",
		"deviceContext->Generation != Generation",
		"ExpectedResetEpoch",
		"deviceContext->Endpoints[EndpointAddress]",
		"endpoint == ExpectedEndpoint",
		"endpointContext->Resetting",
		"ViiperWaitForEndpointQuiescence(endpoint);",
		"endpointContext->Resetting",
		"if (ReleaseGate)",
		"InterlockedExchange(&endpointContext->Resetting, FALSE);",
		"ViiperReleaseDeviceLockShared(controllerContext);",
		"return found;")

	ack := normalizedContract(nativeCFunction(t, broker, "ViiperCompleteManagementOperation"))
	requireContractOrder(t, ack,
		"resetEpoch = ControllerContext->ManagementSlots[slot].ResetEpoch;",
		"State = ViiperUdePendingCompleting;",
		"resetReleased = ViiperQuiesceResetByIdentity(",
		"Completion->DeviceId",
		"Completion->Generation",
		"device",
		"TRUE);",
		"if (!resetReleased)",
		"WdfRequestComplete(request, STATUS_DEVICE_NOT_READY);",
		"ViiperClearManagementSlotLocked(",
		"return STATUS_DEVICE_NOT_READY;",
		"WdfRequestComplete(request, (NTSTATUS)Completion->Status);")

	header := nativeContractSource(t, "native", "udecx", "driver", "ViiperUde.h")
	if !strings.Contains(header, "UDECXUSBDEVICE Device;") ||
		!strings.Contains(header, "UDECXUSBENDPOINT Endpoint;") ||
		!strings.Contains(header, "volatile LONG64 ResetDeviceEpoch;") {
		t.Fatal("management slots lost their exact WDF-object identity pins")
	}
	queueLifecycle := normalizedContract(nativeCFunction(t, broker, "ViiperQueueAcknowledgedLifecycleEvent"))
	requireContractOrder(t, queueLifecycle,
		"WdfObjectReference(Device);",
		"WdfObjectReference(Endpoint);",
		"Kind == ViiperUdeOperationEndpointReset",
		"deviceContext->ResetEpoch",
		"endpointContext->ResetDeviceEpoch",
		"pending->Device = Device;",
		"pending->Endpoint = Endpoint;",
		"pending->ResetEpoch =",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"if (!NT_SUCCESS(status))",
		"ViiperReleaseManagementSlotReferences(Device, Endpoint);")
	clearSlot := normalizedContract(nativeCFunction(t, broker, "ViiperClearManagementSlotLocked"))
	requireContractOrder(t, clearSlot,
		"*DeviceReference = pending->Device;",
		"*EndpointReference = pending->Endpoint;",
		"pending->Device = WDF_NO_HANDLE;",
		"pending->Endpoint = WDF_NO_HANDLE;")
	releasePins := normalizedContract(nativeCFunction(t, broker, "ViiperReleaseManagementSlotReferences"))
	requireContractOrder(t, releasePins,
		"WdfObjectDereference(Endpoint);",
		"WdfObjectDereference(Device);")
	if strings.Count(broker, "ViiperClearManagementSlotLocked(") != 4 ||
		strings.Count(broker, "ViiperReleaseManagementSlotReferences(") != 5 {
		t.Fatal("a management-slot terminal path can bypass exact-handle release")
	}
}

func TestNativeDeviceResetEpochSupersedesOlderEndpointResets(t *testing.T) {
	type endpointReset struct {
		capturedEpoch uint64
		gate          bool
		published     bool
	}
	type device struct {
		epoch       uint64
		resetGate   bool
		unavailable bool
	}
	admitDeviceReset := func(state *device) (uint64, bool) {
		if state.unavailable || state.resetGate {
			return state.epoch, false
		}
		state.resetGate = true
		state.epoch++
		if state.epoch == 0 {
			state.epoch++
		}
		return state.epoch, true
	}
	endpointCurrent := func(state *device, endpoint *endpointReset) bool {
		return endpoint.gate && !state.resetGate &&
			endpoint.capturedEpoch == state.epoch
	}
	failEndpoint := func(endpoint *endpointReset) {
		// This endpoint owns its gate. A device reset/purge/shutdown remains an
		// independent blocker and is not touched here.
		endpoint.gate = false
	}

	// Published endpoint reset, followed by a complete device reset, then a
	// delayed endpoint ACK: the logical ID and WDF handle can both still match,
	// but the private reset epoch makes the ACK stale.
	state := device{}
	published := endpointReset{capturedEpoch: state.epoch, gate: true, published: true}
	deviceEpoch, admitted := admitDeviceReset(&state)
	if !admitted || deviceEpoch != 1 {
		t.Fatalf("device reset was not admitted exactly once: %+v", state)
	}
	state.resetGate = false // acknowledged device reset
	if endpointCurrent(&state, &published) {
		t.Fatal("stale published endpoint ACK survived a complete device reset")
	}
	failEndpoint(&published)
	if published.gate || state.resetGate {
		t.Fatalf("stale endpoint ACK disturbed post-device-reset gates: %+v %+v", state, published)
	}

	// Endpoint worker was admitted but not yet published while a full device
	// reset starts and completes. Its initial publication proof must fail too.
	delayed := endpointReset{capturedEpoch: 4, gate: true}
	state = device{epoch: 4}
	if _, ok := admitDeviceReset(&state); !ok {
		t.Fatal("device reset did not supersede delayed endpoint worker")
	}
	state.resetGate = false
	if endpointCurrent(&state, &delayed) {
		t.Fatal("delayed endpoint worker published across a complete device reset")
	}
	failEndpoint(&delayed)

	// One device reset supersedes every older endpoint transaction, not just
	// the endpoint whose worker happened to run first.
	state = device{epoch: 9}
	left := endpointReset{capturedEpoch: 9, gate: true, published: true}
	right := endpointReset{capturedEpoch: 9, gate: true, published: true}
	if _, ok := admitDeviceReset(&state); !ok {
		t.Fatal("device reset did not supersede two endpoints")
	}
	state.resetGate = false
	for name, endpoint := range map[string]*endpointReset{"left": &left, "right": &right} {
		if endpointCurrent(&state, endpoint) {
			t.Fatalf("%s endpoint survived superseding device epoch", name)
		}
		failEndpoint(endpoint)
	}

	// Rejected device-reset admission must not invalidate otherwise-current
	// endpoint work by consuming an epoch.
	state = device{epoch: 15, unavailable: true}
	current := endpointReset{capturedEpoch: 15, gate: true}
	if epoch, ok := admitDeviceReset(&state); ok || epoch != 15 || state.epoch != 15 {
		t.Fatalf("rejected device reset advanced epoch: %+v", state)
	}
	state.unavailable = false
	if !endpointCurrent(&state, &current) {
		t.Fatal("rejected device reset incorrectly superseded endpoint work")
	}
}

func TestNativeDeviceDestroyAbortsPinnedManagementBeforeConsumingUdeHandle(t *testing.T) {
	broker := nativeContractSource(t, "native", "udecx", "driver", "Broker.c")
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")

	abortMatching := normalizedContract(nativeCFunction(
		t, broker, "ViiperAbortManagementOperationsMatching"))
	requireContractOrder(t, abortMatching,
		"for (;;)",
		"WdfSpinLockAcquire(controllerContext->BrokerLock);",
		"Device == WDF_NO_HANDLE || controllerContext->ManagementSlots[index].Device == Device",
		"matchingSlot = TRUE;",
		"ViiperUdePendingCompleting",
		"RetiredToken = token;",
		"RetiredDeviceId =",
		"RetiredDeviceGeneration =",
		"RetiredEndpointGeneration =",
		"RetiredNotificationPending =",
		"ViiperUdePendingQueued;",
		"State = ViiperUdePendingCompleting;",
		"WdfObjectReference(request);",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"WdfRequestComplete(request, Status);",
		"ViiperClearManagementSlotLocked(",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"ViiperReleaseManagementSlotReferences(deviceReference, endpointReference);",
		"if (!matchingSlot)",
		"return;",
		"KeDelayExecutionThread(KernelMode, FALSE, &retryInterval);")
	dispatch := normalizedContract(nativeCFunction(t, broker, "ViiperDispatchNotificationEvents"))
	requireContractOrder(t, dispatch,
		"event = controllerContext->Notifications[controllerContext->NotificationHead];",
		"RetiredNotificationPending",
		"RetiredToken != event.Token",
		"RetiredDeviceId != event.DeviceId",
		"RetiredDeviceGeneration != event.Generation",
		"RetiredEndpointGeneration != event.EndpointGeneration",
		"RetiredNotificationPending = FALSE;",
		"RetiredToken = 0;",
		"event.Kind = ViiperUdeOperationCancel;",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"WdfRequestComplete(dequeueRequest, STATUS_SUCCESS);")
	complete := normalizedContract(nativeCFunction(t, broker, "ViiperCompleteManagementOperation"))
	requireContractOrder(t, complete,
		"!ControllerContext->ManagementSlots[slot].RetiredNotificationPending",
		"RetiredToken == Completion->Token",
		"RetiredDeviceId == Completion->DeviceId",
		"RetiredDeviceGeneration == Completion->Generation",
		"RetiredEndpointGeneration == Completion->EndpointGeneration",
		"RetiredToken = 0;",
		"RetiredDeviceId = 0;",
		"RetiredDeviceGeneration = 0;",
		"RetiredEndpointGeneration = 0;",
		"RetiredOwnerFile = WDF_NO_HANDLE;",
		"retiredCompletion = TRUE;",
		"return retiredCompletion ? STATUS_SUCCESS : STATUS_NOT_FOUND;")
	clearSlot := normalizedContract(nativeCFunction(t, broker, "ViiperClearManagementSlotLocked"))
	if strings.Contains(clearSlot, "RetiredToken") {
		t.Fatal("terminal slot clear erases the harmless late-ACK tombstone")
	}
	queueLifecycle := normalizedContract(nativeCFunction(
		t, broker, "ViiperQueueAcknowledgedLifecycleEvent"))
	requireContractOrder(t, queueLifecycle,
		"pending->State != ViiperUdePendingEmpty || pending->RetiredToken != 0",
		"continue;",
		"pending->OwnerFile = deviceContext->OwnerFile;",
		"pending->Token = token;")
	retireOwner := normalizedContract(nativeCFunction(
		t, broker, "ViiperRetireManagementTombstonesForOwner"))
	requireContractOrder(t, retireOwner,
		"WdfSpinLockAcquire(controllerContext->BrokerLock);",
		"OwnerFile == WDF_NO_HANDLE || pending->RetiredOwnerFile == OwnerFile",
		"pending->RetiredToken = 0;",
		"pending->RetiredDeviceId = 0;",
		"pending->RetiredDeviceGeneration = 0;",
		"pending->RetiredEndpointGeneration = 0;",
		"pending->RetiredOwnerFile = WDF_NO_HANDLE;",
		"pending->RetiredNotificationPending = FALSE;",
		"WdfSpinLockRelease(controllerContext->BrokerLock);")
	controller := nativeContractSource(t, "native", "udecx", "driver", "Controller.c")
	fileConfig := normalizedContract(nativeCFunction(t, controller, "ViiperEvtDeviceAdd"))
	if !strings.Contains(fileConfig,
		"WDF_FILEOBJECT_CONFIG_INIT( &fileConfig, ViiperEvtFileCreate, ViiperEvtFileClose, ViiperEvtFileCleanup);") {
		t.Fatal("owner-session tombstones are not tied to KMDF's post-I/O file-close boundary")
	}
	fileClose := normalizedContract(nativeCFunction(t, controller, "ViiperEvtFileClose"))
	requireContractOrder(t, fileClose,
		"ShuttingDown",
		"fileContext->BrokerOwner",
		"ViiperRetireManagementTombstonesForOwner(",
		"WdfFileObjectGetDevice(FileObject), FileObject);")
	cleanup := normalizedContract(nativeCFunction(
		t, controller, "ViiperEvtDeviceSelfManagedIoCleanup"))
	requireContractOrder(t, cleanup,
		"WdfIoQueuePurgeSynchronously(context->ControlQueue);",
		"ViiperPurgeOwnerOperations(Device, STATUS_DEVICE_REMOVED);",
		"ViiperRetireManagementTombstonesForOwner(Device, WDF_NO_HANDLE);",
		"ViiperBeginControllerShutdown(Device);")
	destroy := normalizedContract(nativeCFunction(t, device, "ViiperDestroyVirtualDevice"))
	requireContractOrder(t, destroy,
		"ViiperBeginRemoveDevice(",
		"ViiperAbortDeviceManagementOperations(controller, device, STATUS_DEVICE_REMOVED);",
		"UdecxUsbDevicePlugOutAndDelete(device);")
	destroyOwned := normalizedContract(nativeCFunction(t, device, "ViiperDestroyOwnedDevices"))
	requireContractOrder(t, destroyOwned,
		"ViiperBeginRemoveDevice(",
		"plugged = deviceContext->Plugged;",
		"ViiperAbortDeviceManagementOperations(Controller, device, STATUS_FILE_CLOSED);",
		"if (plugged)",
		"UdecxUsbDevicePlugOutAndDelete(device)")

	// Deterministic no-ACK interleaving: the broker holds an endpoint-reset
	// request and both exact WDF-object pins. Removing this device must retire
	// only that slot and release both pins before the UDE handle is consumed;
	// another device's management request remains live.
	type managementSlot struct {
		devicePin   int
		endpointPin int
		pending     bool
		completed   bool
	}
	slots := []managementSlot{
		{devicePin: 7, endpointPin: 71, pending: true},
		{devicePin: 8, endpointPin: 81, pending: true},
	}
	abortDevice := func(devicePin int) {
		for index := range slots {
			slot := &slots[index]
			if !slot.pending || slot.devicePin != devicePin {
				continue
			}
			slot.completed = true
			slot.pending = false
			slot.endpointPin = 0
			slot.devicePin = 0
		}
	}
	deviceTableContainsSeven := true
	purgingSeven := false
	udeSevenConsumed := false
	purgingSeven = true
	deviceTableContainsSeven = false
	abortDevice(7) // no owner acknowledgement arrives
	if slots[0].pending || !slots[0].completed ||
		slots[0].devicePin != 0 || slots[0].endpointPin != 0 {
		t.Fatalf("destroy stranded exact management references: %+v", slots[0])
	}
	if !slots[1].pending || slots[1].completed ||
		slots[1].devicePin != 8 || slots[1].endpointPin != 81 {
		t.Fatalf("exact-device abort disturbed an unrelated child: %+v", slots[1])
	}
	if !purgingSeven || deviceTableContainsSeven {
		t.Fatal("device removal did not close admission before management abort")
	}
	udeSevenConsumed = true
	if !udeSevenConsumed || slots[0].devicePin != 0 {
		t.Fatal("UDE handle was consumed before its management pin drained")
	}

	// A queued token is retired in O(1), then dispatch converts that one record
	// to a benign cancel and preserves the unrelated child's next event. If
	// dispatch already won, the exact slot tombstone accepts one late ACK.
	type notification struct {
		token  uint64
		device int
	}
	queued := []notification{{token: 101, device: 7}, {token: 202, device: 8}}
	queuedRetiredToken := uint64(101)
	dispatched := queued[0]
	queued = queued[1:]
	dispatchedAsCancel := dispatched.token == queuedRetiredToken
	queuedRetiredToken = 0
	if !dispatchedAsCancel || queuedRetiredToken != 0 ||
		len(queued) != 1 || queued[0].token != 202 || queued[0].device != 8 {
		t.Fatalf("queued abort corrupted unrelated lifecycle FIFO: %+v", queued)
	}
	retiredToken := uint64(303) // dispatch crossed BrokerLock before abort
	acceptLate := func(token uint64) bool {
		if retiredToken != token {
			return false
		}
		retiredToken = 0
		return true
	}
	if !acceptLate(303) || retiredToken != 0 || acceptLate(303) {
		t.Fatal("already-delivered teardown token was not consumed exactly once")
	}

	// Slot tombstones are device-bound and non-reusable. B must allocate a
	// different empty slot, so removing B cannot overwrite A before A's late
	// ACK. A malformed device identity cannot consume A's proof.
	type retiredManagement struct {
		token      uint64
		deviceID   uint64
		generation uint32
		owner      int
	}
	retired := []retiredManagement{
		{token: 401, deviceID: 41, generation: 4, owner: 1},
		{},
	}
	allocateEmpty := func() int {
		for index := range retired {
			if retired[index].token == 0 {
				return index
			}
		}
		return -1
	}
	bSlot := allocateEmpty()
	if bSlot != 1 {
		t.Fatalf("allocator reused A tombstone: slot=%d state=%+v", bSlot, retired)
	}
	retired[bSlot] = retiredManagement{token: 502, deviceID: 52, generation: 5, owner: 1}
	acceptBoundLate := func(slot int, token, deviceID uint64, generation uint32) bool {
		proof := &retired[slot]
		if proof.token != token || proof.deviceID != deviceID ||
			proof.generation != generation {
			return false
		}
		*proof = retiredManagement{}
		return true
	}
	if acceptBoundLate(0, 401, 99, 4) || retired[0].token != 401 {
		t.Fatal("malformed completion consumed A's device-bound tombstone")
	}
	if !acceptBoundLate(0, 401, 41, 4) || retired[1].token != 502 {
		t.Fatalf("B removal overwrote A tombstone: %+v", retired)
	}
	if allocateEmpty() != 0 {
		t.Fatal("consuming A did not safely release only A's slot")
	}
	retired[0] = retiredManagement{token: 603, deviceID: 63, generation: 6, owner: 1}
	if allocateEmpty() != -1 {
		t.Fatal("allocator did not fail closed when every slot held a late-ACK proof")
	}
	for index := range retired {
		if retired[index].owner == 1 { // KMDF EvtFileClose: old I/O drained
			retired[index] = retiredManagement{}
		}
	}
	if allocateEmpty() != 0 {
		t.Fatal("post-I/O owner close did not release retired slot capacity")
	}
}

func TestNativeDeliveredBeforeRundownInterleavings(t *testing.T) {
	type endpoint struct {
		open             bool
		purgeCallback    bool
		queueAccepting   bool
		queueDispatching bool
		queued           int
		driverOwned      int
		driverNoRequests bool
		active           int
		terminalDPCs     int
		resetOutstanding bool
	}
	deliverByWDF := func(state *endpoint) bool {
		// The asynchronous reset request is the class-extension fence: UdeCx
		// cannot deliver a successor transfer until the client completes it.
		if state.purgeCallback || state.resetOutstanding ||
			!state.queueDispatching || state.queued == 0 {
			return false
		}
		state.queued--
		state.driverOwned++
		state.driverNoRequests = false
		return true
	}
	resumeDeliveredCallback := func(state *endpoint) bool {
		if state.driverOwned == 0 {
			t.Fatal("resumed a callback WDF does not own")
		}
		state.active++
		// Lifecycle closure is checked in the same BrokerLock transaction as
		// rundown entry. A closed request owns only its terminal DPC.
		return state.open
	}
	runTerminalDPC := func(state *endpoint) {
		if state.driverOwned == 0 || state.active == 0 {
			t.Fatal("terminal DPC released unowned WDF/rundown state")
		}
		state.terminalDPCs++
		state.active--
		state.driverOwned--
		state.driverNoRequests = state.driverOwned == 0
	}
	udeCxBeginPurge := func(state *endpoint) {
		state.open = false
		state.purgeCallback = true
		// UdeCx owns this transition. The visible queue may retain READY state
		// (0x0f) until PurgeComplete, but the callback is the upstream boundary:
		// no successor transfer may be delivered through this purge instance.
	}
	queuePurgeComplete := func(state *endpoint) bool {
		return state.purgeCallback && state.driverNoRequests &&
			state.driverOwned == 0 && state.active == 0 && !state.open
	}
	closeForShutdown := func(state *endpoint) {
		// The controller admission gate closes first. Queued host polls remain
		// owned by UdeCx until PlugOutAndDelete requests endpoint PURGE.
		state.open = false
	}
	driverQuiescent := func(state *endpoint) bool {
		return state.driverOwned == 0 && state.active == 0
	}
	closeForReset := func(state *endpoint) {
		state.open = false
		state.resetOutstanding = true
	}
	resetQuiescent := func(state *endpoint) bool {
		// A parked interrupt poll may remain queued and the associated queue
		// may remain ready. Only driver-owned callbacks plus rundown matter.
		return state.driverOwned == 0 && state.active == 0
	}

	purge := endpoint{
		open:             true,
		queueAccepting:   true,
		queueDispatching: true,
		queued:           2,
		driverNoRequests: true,
	}
	if !deliverByWDF(&purge) {
		t.Fatal("purge: WDF did not deliver the pre-boundary callback")
	}
	udeCxBeginPurge(&purge)
	if queuePurgeComplete(&purge) {
		t.Fatal("purge passed a WDF-delivered callback before rundown entry")
	}
	if resumeDeliveredCallback(&purge) {
		t.Fatal("purge callback allocated/published after lifecycle closure")
	}
	if queuePurgeComplete(&purge) {
		t.Fatal("purge completed before the terminal DPC")
	}
	runTerminalDPC(&purge)
	if !queuePurgeComplete(&purge) || purge.terminalDPCs != 1 ||
		!purge.queueAccepting || !purge.queueDispatching || purge.queued != 1 {
		t.Fatalf("purge consumed queued host polls or failed driver-rundown proof: %+v", purge)
	}

	// Direct input is admitted through the controller queue, so endpoint queue
	// state alone cannot make PURGE complete. ActiveOperations is the second
	// half of the proof and is sampled under the same BrokerLock.
	direct := endpoint{
		open:             true,
		queueAccepting:   true,
		queueDispatching: true,
		driverNoRequests: true,
		active:           1,
	}
	udeCxBeginPurge(&direct)
	if queuePurgeComplete(&direct) {
		t.Fatal("purge passed direct input while the associated queue was idle")
	}
	direct.active--
	if !queuePurgeComplete(&direct) {
		t.Fatalf("purge did not complete after direct input rundown: %+v", direct)
	}

	shutdown := endpoint{
		open:             true,
		queueAccepting:   true,
		queueDispatching: true,
		queued:           2,
		driverNoRequests: true,
	}
	if !deliverByWDF(&shutdown) {
		t.Fatal("shutdown: WDF did not deliver the pre-boundary callback")
	}
	closeForShutdown(&shutdown)
	if driverQuiescent(&shutdown) {
		t.Fatal("shutdown passed a WDF-delivered callback before rundown entry")
	}
	if resumeDeliveredCallback(&shutdown) {
		t.Fatal("shutdown callback allocated/published after lifecycle closure")
	}
	runTerminalDPC(&shutdown)
	if !driverQuiescent(&shutdown) || !shutdown.queueDispatching || shutdown.queued != 1 {
		t.Fatalf("shutdown waited on class-extension-owned queued polls: %+v", shutdown)
	}
	udeCxBeginPurge(&shutdown) // UdeCx callback after child consumption.
	if !queuePurgeComplete(&shutdown) {
		t.Fatalf("post-consumption endpoint purge did not complete: %+v", shutdown)
	}

	reset := endpoint{
		open:             true,
		queueAccepting:   true,
		queueDispatching: true,
		queued:           2,
		driverNoRequests: true,
	}
	if !deliverByWDF(&reset) {
		t.Fatal("reset: WDF did not deliver the pre-boundary callback")
	}
	closeForReset(&reset)
	if resetQuiescent(&reset) {
		t.Fatal("reset publication passed a WDF-delivered callback before rundown entry")
	}
	if resumeDeliveredCallback(&reset) {
		t.Fatal("reset callback allocated/published after reset closure")
	}
	if resetQuiescent(&reset) {
		t.Fatal("reset publication passed the callback before terminal DPC completion")
	}
	runTerminalDPC(&reset)
	if !resetQuiescent(&reset) || reset.queued != 1 || !reset.queueDispatching {
		t.Fatalf("reset failed ready-queue DriverNoRequests proof: %+v", reset)
	}
	if deliverByWDF(&reset) {
		t.Fatal("UdeCx delivered a successor callback before reset acknowledgement")
	}
	// Owner ACK repeats the proof. Only then may the exact reset gate reopen
	// and the asynchronous reset request be completed.
	if !resetQuiescent(&reset) {
		t.Fatal("reset acknowledgement missed the second quiescence proof")
	}
	reset.open = true
	reset.resetOutstanding = false
	if !deliverByWDF(&reset) {
		t.Fatal("post-reset queue did not resume after acknowledgement")
	}
}

func TestNativeResetAcknowledgementRejectsRemovalAndIdentityReuse(t *testing.T) {
	type identity struct {
		deviceID       uint64
		generation     uint32
		handle         uint64
		endpointExists bool
		resetting      bool
	}
	ack := func(current *identity, deviceID uint64, generation uint32, handle uint64) bool {
		if current == nil || current.deviceID != deviceID ||
			current.generation != generation || current.handle != handle ||
			!current.endpointExists ||
			!current.resetting {
			return false
		}
		current.resetting = false
		return true
	}

	original := identity{deviceID: 7, generation: 41, handle: 1001, endpointExists: true, resetting: true}
	removed := original
	removed.endpointExists = false
	if ack(&removed, original.deviceID, original.generation, original.handle) || !removed.resetting {
		t.Fatal("removed endpoint accepted an acknowledgement or reopened its gate")
	}

	// A hostile/raw broker can reuse both logical fields, and generation wraps
	// eventually. The pinned old WDF handle cannot be recycled until slot clear,
	// so the successor must still reject the delayed acknowledgement.
	successor := identity{deviceID: 7, generation: 41, handle: 2002, endpointExists: true, resetting: true}
	if ack(&successor, original.deviceID, original.generation, original.handle) || !successor.resetting {
		t.Fatal("stale acknowledgement reopened an exact logical identity on a successor handle")
	}
	if !ack(&successor, successor.deviceID, successor.generation, successor.handle) || successor.resetting {
		t.Fatal("exact live generation did not accept its own acknowledgement")
	}
}

func TestNativeOverlappingEndpointAndDeviceResetReleaseOnlyOwnedGate(t *testing.T) {
	type gates struct {
		deviceReset   bool
		endpointReset bool
		purging       bool
		shutdown      bool
		brokerFault   bool
	}
	admissionOpen := func(state gates) bool {
		return !state.deviceReset && !state.endpointReset && !state.purging &&
			!state.shutdown && !state.brokerFault
	}

	state := gates{endpointReset: true}
	// Device reset wins after endpoint reset admission. The endpoint worker's
	// exact proof fails because the device-wide gate is now closed; it must
	// release only the endpoint gate before failing its actual reset request.
	state.deviceReset = true
	state.endpointReset = false
	if admissionOpen(state) || !state.deviceReset {
		t.Fatalf("endpoint failure disturbed the winning device reset: %+v", state)
	}

	// The device reset's own publication proof can then lose to purge. Its
	// callback-owned gate is released, while purge remains the independent
	// admission blocker.
	state.purging = true
	state.deviceReset = false
	if admissionOpen(state) || !state.purging {
		t.Fatalf("device failure disturbed the winning purge: %+v", state)
	}
}

func TestNativeResetPublicationRejectsConcurrentRemoval(t *testing.T) {
	type resetBoundary struct {
		resetting bool
		purging   bool
		present   bool
		published bool
	}
	publish := func(state *resetBoundary) bool {
		if !state.present || !state.resetting || state.purging {
			return false
		}
		state.published = true
		return true
	}

	for _, test := range []struct {
		name  string
		state resetBoundary
	}{
		{name: "device removed", state: resetBoundary{resetting: true, present: false}},
		{name: "device purge won", state: resetBoundary{resetting: true, purging: true, present: true}},
		{name: "endpoint retired", state: resetBoundary{resetting: true, present: false}},
	} {
		if publish(&test.state) || test.state.published {
			t.Fatalf("%s published a reset for a dead lifecycle identity", test.name)
		}
	}

	live := resetBoundary{resetting: true, present: true}
	if !publish(&live) || !live.published {
		t.Fatal("live exact reset identity did not publish after quiescence")
	}
}

func TestNativeDuplicateEndpointAddCannotRetireLiveIncarnation(t *testing.T) {
	add := normalizedContract(nativeCFunction(t,
		nativeContractSource(t, "native", "udecx", "driver", "Device.c"),
		"ViiperEvtEndpointAdd"))

	requireContractOrder(t, add,
		"deviceContext->Endpoints[descriptor.bEndpointAddress] != WDF_NO_HANDLE",
		"status = STATUS_OBJECT_NAME_COLLISION;",
		"deviceContext->EndpointGenerations[ descriptor.bEndpointAddress] == MAXULONG",
		"deviceContext->EndpointGenerations[descriptor.bEndpointAddress] = generation;")
	if !strings.Contains(add,
		"descriptor.bEndpointAddress == 0 && deviceContext->DefaultEndpoint != WDF_NO_HANDLE") {
		t.Fatal("endpoint-add can advance endpoint zero while a default endpoint is live")
	}

	type endpointSlot struct {
		generation uint32
		live       bool
	}
	allocate := func(slot *endpointSlot) (uint32, bool) {
		if slot.live || slot.generation == ^uint32(0) {
			return 0, false
		}
		slot.generation++
		return slot.generation, true
	}
	slot := endpointSlot{generation: 1, live: true}
	if generation, admitted := allocate(&slot); admitted || generation != 0 || slot.generation != 1 {
		t.Fatalf("duplicate add retired live generation: admitted=%t result=%d slot=%+v",
			admitted, generation, slot)
	}
	// Once cleanup retires the exact live object, the successor gets a fresh
	// incarnation; the failed duplicate did not create an unobservable hole.
	slot.live = false
	if generation, admitted := allocate(&slot); !admitted || generation != 2 {
		t.Fatalf("successor allocation=(%d,%t) want generation 2", generation, admitted)
	}
}

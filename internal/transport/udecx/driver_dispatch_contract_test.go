package udecx

import (
	"strings"
	"testing"
)

func TestNativeBrokerDispatchUsesIndependentCursorAndEndpointFIFO(t *testing.T) {
	broker := nativeContractSource(t, "native", "udecx", "driver", "Broker.c")
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")
	header := nativeContractSource(t, "native", "udecx", "driver", "ViiperUde.h")

	for _, required := range []string{
		"LIST_ENTRY AdmissionEntry;",
		"BOOLEAN AdmissionLinked;",
		"ULONG NextDispatchSlot;",
		"LIST_ENTRY AdmissionQueue;",
	} {
		if !strings.Contains(header, required) {
			t.Fatalf("native dispatch contract lost %q", required)
		}
	}
	if !strings.Contains(device,
		"InitializeListHead(&endpointContext->AdmissionQueue);") {
		t.Fatal("endpoint admission FIFO is not initialized before broker use")
	}
	if strings.Contains(broker, "ViiperHasEarlierUnpublishedAdmissionLocked") {
		t.Fatal("native dispatch still performs the controller-wide admission-order scan")
	}

	allocate := normalizedContract(nativeCFunction(t, broker, "ViiperAllocatePendingSlot"))
	requireContractOrder(t, allocate,
		"pending->AdmissionLinked = TRUE;",
		"InsertTailList(&endpointContext->AdmissionQueue, &pending->AdmissionEntry);",
		"ControllerContext->NextPendingSlot = (index + 1) % VIIPER_UDE_MAX_PENDING_OPERATIONS;")

	head := normalizedContract(nativeCFunction(t, broker, "ViiperAdmissionCanPublishLocked"))
	requireContractOrder(t, head,
		"if (!Pending->AdmissionLinked || Pending->Endpoint == WDF_NO_HANDLE)",
		"endpointContext = ViiperGetEndpointContext(Pending->Endpoint);",
		"return endpointContext->AdmissionQueue.Flink == &Pending->AdmissionEntry;")

	dispatch := normalizedContract(nativeCFunction(t, broker, "ViiperDispatchAvailable"))
	if strings.Contains(dispatch, "NextPendingSlot") {
		t.Fatal("allocation cursor is still coupled to broker dispatch")
	}
	requireContractOrder(t, dispatch,
		"controllerContext->NextDispatchSlot + index",
		"ViiperAdmissionCanPublishLocked(pending)",
		"controllerContext->NextDispatchSlot = (candidate + 1)",
		"ViiperUnlinkAdmissionLocked(pending)")
}

func TestNativeBrokerAdmissionRetirementCannotStrandSuccessor(t *testing.T) {
	broker := nativeContractSource(t, "native", "udecx", "driver", "Broker.c")

	clear := normalizedContract(nativeCFunction(t, broker, "ViiperClearSlotLocked"))
	requireContractOrder(t, clear,
		"ViiperUnlinkAdmissionLocked(pending);",
		"pending->Request = WDF_NO_HANDLE;")

	cancel := normalizedContract(nativeCFunction(t, broker, "ViiperEvtUrbCancel"))
	requireContractOrder(t, cancel,
		"dispatchSuccessor = pending->AdmissionLinked;",
		"ViiperUnlinkAdmissionLocked(pending);",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"if (dispatchSuccessor)",
		"ViiperDispatchAvailable(controller);")

	queue := normalizedContract(nativeCFunction(t, broker, "ViiperQueueUrb"))
	requireContractOrder(t, queue,
		"pending->State = ViiperUdePendingDpcCompletion;",
		"ViiperUnlinkAdmissionLocked(pending);",
		"if (queueCancelledCompletion)",
		"ViiperQueueUrbCompletion(",
		"ViiperDispatchAvailable(deviceContext->Controller);")

	abort := normalizedContract(nativeCFunction(t, broker, "ViiperAbortMatchingOperations"))
	requireContractOrder(t, abort,
		"pending->AbortPending = TRUE;",
		"ViiperUnlinkAdmissionLocked(pending);")
}

func TestNativeBrokerPublishingCancelCannotMissDispatchWake(t *testing.T) {
	// WdfRequestUnmarkCancelable is allowed to return STATUS_CANCELLED before
	// EvtRequestCancel has run. Model the worst ordering: the old dispatcher
	// scans while the publishing admission is still linked, finds no eligible
	// successor, and returns. The later callback must both unlink and explicitly
	// wake a new dispatch pass.
	type admission struct {
		linked bool
		state  string
	}
	head := admission{linked: true, state: "publishing"}
	successor := admission{linked: true, state: "queued"}
	canPublishSuccessor := func() bool {
		return successor.linked && !head.linked
	}

	if canPublishSuccessor() {
		t.Fatal("successor published ahead of the same-endpoint head")
	}
	oldDispatchReturned := true // it scanned before EvtRequestCancel ran
	dispatchWake := false
	if head.linked {
		dispatchWake = true
		head.linked = false
		head.state = "dpc-completion"
	}
	if !oldDispatchReturned || !dispatchWake || !canPublishSuccessor() {
		t.Fatal("publishing-head cancellation failed to wake its queued successor")
	}
}

func TestNativeBrokerIndependentCursorEliminatesCommonFullWrap(t *testing.T) {
	const slots = 4096
	scan := func(start, target int) int {
		for offset := 0; offset < slots; offset++ {
			if (start+offset)%slots == target {
				return offset + 1
			}
		}
		return slots
	}

	// The old scheduler advanced the allocation cursor after choosing slot 0,
	// then reused it as the dispatch start.  Its only queued operation was the
	// last slot inspected after wrapping the entire table.
	if got := scan(1, 0); got != slots {
		t.Fatalf("coupled-cursor baseline inspected %d slots, want %d", got, slots)
	}
	if got := scan(0, 0); got != 1 {
		t.Fatalf("independent dispatch cursor inspected %d slots, want 1", got)
	}

	allocationCursor := 0
	dispatchCursor := 0
	for iteration := 0; iteration < slots*2; iteration++ {
		allocated := allocationCursor
		allocationCursor = (allocated + 1) % slots
		if got := scan(dispatchCursor, allocated); got != 1 {
			t.Fatalf("iteration %d inspected %d slots, want 1", iteration, got)
		}
		dispatchCursor = (allocated + 1) % slots
	}
}

func TestNativeBrokerEndpointFIFOModelsPublishAndCancelOrdering(t *testing.T) {
	type admission struct {
		id       int
		endpoint int
	}
	queues := map[int][]admission{}
	appendAdmission := func(item admission) {
		queues[item.endpoint] = append(queues[item.endpoint], item)
	}
	canPublish := func(item admission) bool {
		queue := queues[item.endpoint]
		return len(queue) != 0 && queue[0].id == item.id
	}
	retire := func(item admission) {
		queue := queues[item.endpoint]
		if len(queue) == 0 || queue[0].id != item.id {
			t.Fatalf("retired non-head admission %+v from %+v", item, queue)
		}
		queues[item.endpoint] = queue[1:]
	}

	a1 := admission{id: 1, endpoint: 0x01}
	a2 := admission{id: 2, endpoint: 0x01}
	b1 := admission{id: 3, endpoint: 0x82}
	appendAdmission(a1)
	appendAdmission(a2)
	appendAdmission(b1)
	if !canPublish(a1) || canPublish(a2) || !canPublish(b1) {
		t.Fatal("per-endpoint heads did not preserve FIFO order and cross-endpoint concurrency")
	}

	retire(a1) // publication
	if !canPublish(a2) {
		t.Fatal("publication did not expose the next same-endpoint admission")
	}

	a3 := admission{id: 4, endpoint: 0x01}
	appendAdmission(a3)
	retire(a2) // cancellation/abort uses the same unlink transition
	if !canPublish(a3) {
		t.Fatal("cancellation did not expose the next same-endpoint admission")
	}
}

func TestNativeFastInputSubmissionAvoidsSecondKMDFQueueHop(t *testing.T) {
	controller := nativeContractSource(t, "native", "udecx", "driver", "Controller.c")
	ioctl := nativeContractSource(t, "native", "udecx", "driver", "Ioctl.c")
	header := nativeContractSource(t, "native", "udecx", "driver", "ViiperUde.h")
	all := controller + ioctl + header
	if strings.Contains(all, "WDFQUEUE InputQueue;") ||
		strings.Contains(all, "context->InputQueue") ||
		strings.Contains(all, "ViiperEvtInputIoDeviceControl") {
		t.Fatal("native input still crosses the redundant parallel KMDF queue")
	}

	queues := normalizedContract(nativeCFunction(t, controller, "ViiperCreateQueues"))
	requireContractOrder(t, queues,
		"WDF_IO_QUEUE_CONFIG_INIT_DEFAULT_QUEUE(&queueConfig, WdfIoQueueDispatchParallel);",
		"queueConfig.EvtIoDeviceControl = ViiperEvtIoDeviceControlRoute;",
		"WDF_IO_QUEUE_CONFIG_INIT(&queueConfig, WdfIoQueueDispatchSequential);",
		"queueConfig.EvtIoDeviceControl = ViiperEvtIoDeviceControl;")

	route := normalizedContract(nativeCFunction(t, ioctl, "ViiperEvtIoDeviceControlRoute"))
	requireContractOrder(t, route,
		"InterlockedCompareExchange(&context->ShuttingDown, 0, 0)",
		"if (IoControlCode == IOCTL_VIIPER_UDE_SUBMIT_INPUT_REPORT)",
		"status = ViiperSubmitInputReport(Queue, Request);",
		"WdfRequestComplete(Request, status);",
		"return;",
		"WdfRequestForwardToIoQueue(Request, context->ControlQueue)")
	if strings.Contains(route, "WdfRequestForwardToIoQueue(Request, context->InputQueue)") {
		t.Fatal("hot input report is still forwarded before completion")
	}
}

func TestNativeCachedInputReadyUsesCompletionDPCWithoutWorkerHop(t *testing.T) {
	device := nativeContractSource(t, "native", "udecx", "driver", "Device.c")
	header := nativeContractSource(t, "native", "udecx", "driver", "ViiperUde.h")
	if strings.Contains(device+header, "InputReadyWorkItem") ||
		strings.Contains(device+header, "ViiperEvtFastInputWorkItem") {
		t.Fatal("cached input delivery still crosses a generic system work item")
	}

	createQueue := normalizedContract(nativeCFunction(t, device, "ViiperCreateEndpointQueue"))
	if !strings.Contains(createQueue, "attributes.ExecutionLevel = WdfExecutionLevelPassive;") {
		t.Fatal("manual fast-input queue no longer pins ReadyNotify to PASSIVE_LEVEL")
	}
	ready := normalizedContract(nativeCFunction(t, device, "ViiperEvtFastInputQueueReady"))
	requireContractOrder(t, ready,
		"WdfWaitLockAcquire(endpointContext->InputLock, NULL);",
		"ViiperEndpointOperationStarted(endpoint);",
		"WdfIoQueueRetrieveNextRequest(Queue, &request)",
		"ViiperCompleteCachedInputUrb(endpoint, request);",
		"WdfWaitLockRelease(endpointContext->InputLock);")
	if strings.Contains(ready, "WdfWorkItemEnqueue") ||
		strings.Contains(ready, "UdecxUrbComplete(") {
		t.Fatal("ReadyNotify either retains a worker hop or completes a UDE URB synchronously")
	}

	complete := normalizedContract(nativeCFunction(t, device, "ViiperCompleteRetrievedInputUrb"))
	if !strings.Contains(complete, "ViiperQueueUrbCompletion(") {
		t.Fatal("cached input no longer transfers terminal completion to the shared DPC")
	}
}

func TestNativeBrokerFaultFencesAdmissionAndPublication(t *testing.T) {
	broker := nativeContractSource(t, "native", "udecx", "driver", "Broker.c")

	allocate := normalizedContract(nativeCFunction(t, broker, "ViiperAllocatePendingSlot"))
	requireContractOrder(t, allocate,
		"WdfSpinLockAcquire(ControllerContext->BrokerLock);",
		"ControllerContext->BrokerFaulted",
		"pending->Request = Request;")

	dispatch := normalizedContract(nativeCFunction(t, broker, "ViiperDispatchAvailable"))
	requireContractOrder(t, dispatch,
		"ViiperDispatchNotificationEvents(Controller);",
		"controllerContext->BrokerFaulted",
		"controllerContext->NextDispatchSlot + index")

	cancel := normalizedContract(nativeCFunction(t, broker, "ViiperQueueCancelEventLocked"))
	requireContractOrder(t, cancel,
		"if (!Pending->PublishedToOwner)",
		"ControllerContext->BrokerFaulted",
		"ControllerContext->NotificationCount")

	for _, function := range []string{
		"ViiperQueueEndpointLifecycleEvent",
		"ViiperQueueDeviceLifecycleEvent",
		"ViiperQueueInterfaceLifecycleEvent",
	} {
		lifecycle := normalizedContract(nativeCFunction(t, broker, function))
		requireContractOrder(t, lifecycle,
			"active = ownerActive && InterlockedCompareExchange( &controllerContext->BrokerFaulted",
			"queued = active &&",
			"faulted = ownerActive && InterlockedCompareExchange( &controllerContext->BrokerFaulted",
			"WdfSpinLockRelease(controllerContext->BrokerLock);",
			"if (queued || faulted)",
			"ViiperDispatchNotificationEvents(deviceContext->Controller);")
	}

	acknowledged := normalizedContract(nativeCFunction(t, broker, "ViiperQueueAcknowledgedLifecycleEvent"))
	requireContractOrder(t, acknowledged,
		"controllerContext->BrokerFaulted",
		"ViiperFaultBrokerLocked(controllerContext)",
		"faulted = ownerActive && InterlockedCompareExchange( &controllerContext->BrokerFaulted",
		"WdfSpinLockRelease(controllerContext->BrokerLock);",
		"if (NT_SUCCESS(status) || faulted)",
		"ViiperDispatchNotificationEvents(deviceContext->Controller);")
}

func TestNativeManualDequeueCancellationRetiresAccounting(t *testing.T) {
	controller := nativeContractSource(t, "native", "udecx", "driver", "Controller.c")
	header := nativeContractSource(t, "native", "udecx", "driver", "ViiperUde.h")
	createQueues := normalizedContract(nativeCFunction(t, controller, "ViiperCreateQueues"))
	requireContractOrder(t, createQueues,
		"WDF_IO_QUEUE_CONFIG_INIT(&queueConfig, WdfIoQueueDispatchManual);",
		"queueConfig.EvtIoCanceledOnQueue = ViiperEvtDequeueCanceledOnQueue;",
		"WdfIoQueueCreate(Device, &queueConfig")
	if !strings.Contains(header,
		"EVT_WDF_IO_QUEUE_IO_CANCELED_ON_QUEUE ViiperEvtDequeueCanceledOnQueue;") {
		t.Fatal("manual dequeue cancellation callback lost its KMDF declaration")
	}
	cancel := normalizedContract(nativeCFunction(t, controller, "ViiperEvtDequeueCanceledOnQueue"))
	requireContractOrder(t, cancel,
		"InterlockedDecrement(&context->WaitingDequeueCount);",
		"NT_ASSERT(remaining >= 0);",
		"WdfRequestComplete(Request, STATUS_CANCELLED);")
}

func TestNativeBrokerMixedLaneFairnessModel(t *testing.T) {
	// Exercise the exact round-robin slot selection and per-endpoint-head rule
	// with control, HID/state, speaker ISO, and microphone ISO traffic from
	// several controllers.  Deterministic head cancellations model purge/reset
	// pressure while proving that an unrelated endpoint is never starved.
	const slots = 4096
	type laneKey struct {
		device   int
		endpoint byte
	}
	type admission struct {
		lane     laneKey
		sequence int
		queued   bool
		linked   bool
	}

	endpoints := []byte{0x00, 0x01, 0x02, 0x82}
	pending := make([]admission, slots)
	queues := make(map[laneKey][]int)
	allocated := 0
	for round := 1; round <= 8; round++ {
		for device := 0; device < 8; device++ {
			for _, endpoint := range endpoints {
				lane := laneKey{device: device, endpoint: endpoint}
				pending[allocated] = admission{
					lane: lane, sequence: round, queued: true, linked: true,
				}
				queues[lane] = append(queues[lane], allocated)
				allocated++
			}
		}
	}

	// Cancel selected heads before dispatch, exactly like the kernel unlink
	// transition: remove the old head and expose its same-endpoint successor.
	for lane, queue := range queues {
		if (lane.device+int(lane.endpoint))%7 == 0 {
			pending[queue[0]].linked = false
			pending[queue[0]].queued = false
			queues[lane] = queue[1:]
		}
	}

	cursor := 0
	delivered := make(map[laneKey][]int)
	remaining := 0
	for _, queue := range queues {
		remaining += len(queue)
	}
	maxInspections := 0
	totalInspections := 0
	for remaining != 0 {
		selected := -1
		inspections := 0
		for offset := 0; offset < slots; offset++ {
			inspections++
			candidate := (cursor + offset) % slots
			item := pending[candidate]
			queue := queues[item.lane]
			if item.queued && item.linked && len(queue) != 0 && queue[0] == candidate {
				selected = candidate
				break
			}
		}
		if selected < 0 {
			t.Fatalf("mixed native traffic stranded %d endpoint admissions", remaining)
		}
		if inspections > maxInspections {
			maxInspections = inspections
		}
		totalInspections += inspections
		item := pending[selected]
		delivered[item.lane] = append(delivered[item.lane], item.sequence)
		queue := queues[item.lane]
		queues[item.lane] = queue[1:]
		pending[selected].linked = false
		pending[selected].queued = false
		cursor = (selected + 1) % slots
		remaining--
	}

	for lane, sequences := range delivered {
		for index := 1; index < len(sequences); index++ {
			if sequences[index] != sequences[index-1]+1 {
				t.Fatalf("lane %+v lost FIFO order: %v", lane, sequences)
			}
		}
	}
	if len(delivered) != 8*len(endpoints) {
		t.Fatalf("only %d/%d independent lanes made progress", len(delivered), 8*len(endpoints))
	}
	// The independent allocation/dispatch cursors make every healthy admission
	// the first inspected slot.  Each of the four deliberately canceled heads
	// costs one extra inspection, never a controller-table wrap.
	if maxInspections != 2 || totalInspections != allocated {
		t.Fatalf("mixed native traffic inspected max=%d total=%d, want max=2 total=%d",
			maxInspections, totalInspections, allocated)
	}
}

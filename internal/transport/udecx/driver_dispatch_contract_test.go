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

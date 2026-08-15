/*
 * Bounded, single-owner user/kernel transfer broker.
 *
 * Every submitted URB occupies one preallocated slot. Tokens encode the slot
 * and a monotonically increasing generation, which makes completion lookup
 * O(1) and rejects stale/duplicate replies without allocating on the media
 * path. Cancellation is handed between WDF and the broker with an explicit
 * unmark/remark boundary while a request is serialized to user mode.
 */

#include "ViiperUde.h"

EVT_WDF_REQUEST_CANCEL ViiperEvtUrbCancel;

static VOID ViiperDispatchAvailable(_In_ WDFDEVICE Controller);

VOID
ViiperCompleteUnownedUrb(
    _In_ WDFDEVICE Controller,
    _In_ WDFREQUEST Request,
    _In_ NTSTATUS Status
    )
{
    VIIPER_UDE_REQUEST_CONTEXT *requestContext = ViiperGetRequestContext(Request);
    BOOLEAN queued;

    // ViiperQueueUrb starts endpoint rundown before it can reject admission,
    // so even an untracked failure remains owned until the DPC completes it.
    NT_ASSERT(requestContext->Controller == Controller);
    NT_ASSERT(requestContext->Endpoint != WDF_NO_HANDLE);
    queued = ViiperQueueUrbCompletion(
        Controller,
        requestContext->Endpoint,
        Request,
        VIIPER_UDE_MAX_PENDING_OPERATIONS,
        0,
        Status,
        USBD_STATUS_INTERNAL_HC_ERROR,
        TRUE,
        0,
        0);
    if (!queued) {
        NT_ASSERT(FALSE);
    }
}

static
BOOLEAN
ViiperFaultBrokerLocked(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext
    )
{
    VIIPER_UDE_NOTIFICATION *event;

    InterlockedIncrement64(&ControllerContext->NotificationEventOverflows);
    if (InterlockedCompareExchange(&ControllerContext->BrokerFaulted, TRUE, FALSE) != FALSE) {
        return FALSE;
    }
    if (ControllerContext->NotificationCount >= VIIPER_UDE_MAX_PENDING_OPERATIONS) {
        return FALSE;
    }

    event = &ControllerContext->Notifications[ControllerContext->NotificationTail];
    RtlZeroMemory(event, sizeof(*event));
    event->Kind = ViiperUdeOperationBrokerFault;
    ControllerContext->NotificationTail = (ControllerContext->NotificationTail + 1) %
        VIIPER_UDE_MAX_PENDING_OPERATIONS;
    ++ControllerContext->NotificationCount;
    return TRUE;
}

static
BOOLEAN
ViiperQueueCancelEventLocked(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ const VIIPER_UDE_PENDING_SLOT *Pending
    )
{
    VIIPER_UDE_NOTIFICATION *event;

    if (!Pending->PublishedToOwner) {
        return FALSE;
    }
    // BrokerFaulted is a terminal owner-session boundary.  The one broker
    // fault notification already tells user mode to tear down; admitting more
    // cancel records after it can only delay that terminal record and consume
    // the queue capacity reserved for lifecycle ordering.
    if (InterlockedCompareExchange(
            &ControllerContext->BrokerFaulted, FALSE, FALSE) != FALSE) {
        return FALSE;
    }
    // Keep one slot reserved for a broker-fault event. Losing cancellation or
    // lifecycle state is not recoverable within the current owner session.
    if (ControllerContext->NotificationCount >= VIIPER_UDE_MAX_PENDING_OPERATIONS - 1) {
        return ViiperFaultBrokerLocked(ControllerContext);
    }

    event = &ControllerContext->Notifications[ControllerContext->NotificationTail];
    event->Token = Pending->Token;
    event->DeviceId = Pending->DeviceId;
    event->EndpointSequence = 0;
    event->DeviceSequence = 0;
    event->Generation = Pending->DeviceGeneration;
    event->EndpointGeneration = Pending->EndpointGeneration;
    event->Kind = ViiperUdeOperationCancel;
    event->EndpointAddress = Pending->EndpointAddress;
    event->InterfaceNumber = 0;
    event->InterfaceSetting = 0;
    ControllerContext->NotificationTail = (ControllerContext->NotificationTail + 1) %
        VIIPER_UDE_MAX_PENDING_OPERATIONS;
    ++ControllerContext->NotificationCount;
    return TRUE;
}

static
VOID
ViiperDispatchNotificationEvents(
    _In_ WDFDEVICE Controller
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(Controller);

    for (;;) {
        WDFREQUEST dequeueRequest = WDF_NO_HANDLE;
        VIIPER_UDE_OPERATION *operation = NULL;
        VIIPER_UDE_NOTIFICATION event = {0};
        NTSTATUS status;

        if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0) {
            break;
        }
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0) {
            WdfSpinLockRelease(controllerContext->BrokerLock);
            break;
        }
        if (controllerContext->NotificationCount == 0) {
            WdfSpinLockRelease(controllerContext->BrokerLock);
            break;
        }
        status = WdfIoQueueRetrieveNextRequest(
            controllerContext->WaitingDequeues, &dequeueRequest);
        if (!NT_SUCCESS(status)) {
            WdfSpinLockRelease(controllerContext->BrokerLock);
            break;
        }
        InterlockedDecrement(&controllerContext->WaitingDequeueCount);
        status = WdfRequestRetrieveOutputBuffer(
            dequeueRequest, sizeof(*operation), (PVOID *)&operation, NULL);
        if (NT_SUCCESS(status)) {
            event = controllerContext->Notifications[controllerContext->NotificationHead];
            controllerContext->NotificationHead = (controllerContext->NotificationHead + 1) %
                VIIPER_UDE_MAX_PENDING_OPERATIONS;
            --controllerContext->NotificationCount;
            if (((ULONG)event.Token & VIIPER_UDE_MANAGEMENT_SLOT_FLAG) != 0) {
                ULONG managementSlot = ((ULONG)event.Token &
                    ~VIIPER_UDE_MANAGEMENT_SLOT_FLAG) - 1;
                if (managementSlot >= VIIPER_UDE_MAX_PENDING_MANAGEMENT ||
                    (controllerContext->ManagementSlots[managementSlot].Token != event.Token ||
                     controllerContext->ManagementSlots[managementSlot].State !=
                        ViiperUdePendingQueued ||
                     controllerContext->ManagementSlots[managementSlot].DeviceId !=
                        event.DeviceId ||
                     controllerContext->ManagementSlots[managementSlot].DeviceGeneration !=
                        event.Generation ||
                     controllerContext->ManagementSlots[managementSlot].EndpointGeneration !=
                        event.EndpointGeneration) &&
                    (!controllerContext->ManagementSlots[managementSlot].RetiredNotificationPending ||
                     controllerContext->ManagementSlots[managementSlot].RetiredToken != event.Token ||
                     controllerContext->ManagementSlots[managementSlot].RetiredDeviceId !=
                        event.DeviceId ||
                     controllerContext->ManagementSlots[managementSlot].RetiredDeviceGeneration !=
                        event.Generation ||
                     controllerContext->ManagementSlots[managementSlot].RetiredEndpointGeneration !=
                        event.EndpointGeneration)) {
                    status = STATUS_INVALID_DEVICE_STATE;
                } else if (controllerContext->ManagementSlots[
                        managementSlot].RetiredNotificationPending) {
                    // Teardown retired the held UdeCx request before this
                    // queued notification crossed to user mode. Consume its
                    // WDF-free tombstone in O(1) and publish a benign cancel
                    // record, which the host handles before lane tracking.
                    controllerContext->ManagementSlots[
                        managementSlot].RetiredNotificationPending = FALSE;
                    controllerContext->ManagementSlots[managementSlot].RetiredToken = 0;
                    controllerContext->ManagementSlots[managementSlot].RetiredDeviceId = 0;
                    controllerContext->ManagementSlots[
                        managementSlot].RetiredDeviceGeneration = 0;
                    controllerContext->ManagementSlots[
                        managementSlot].RetiredEndpointGeneration = 0;
                    controllerContext->ManagementSlots[
                        managementSlot].RetiredOwnerFile = WDF_NO_HANDLE;
                    event.Kind = ViiperUdeOperationCancel;
                } else {
                    controllerContext->ManagementSlots[managementSlot].State =
                        ViiperUdePendingInFlight;
                }
            }
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);

        if (!NT_SUCCESS(status)) {
            WdfRequestComplete(dequeueRequest, status);
            continue;
        }

        RtlZeroMemory(operation, sizeof(*operation));
        operation->Header.Magic = VIIPER_UDE_MAGIC;
        operation->Header.Major = VIIPER_UDE_ABI_MAJOR;
        operation->Header.Minor = VIIPER_UDE_ABI_MINOR;
        operation->Header.Size = sizeof(*operation);
        operation->Token = event.Token;
        operation->DeviceId = event.DeviceId;
        operation->Generation = event.Generation;
        operation->Kind = event.Kind;
        operation->EndpointAddress = event.EndpointAddress;
        operation->InterfaceNumber = event.InterfaceNumber;
        operation->InterfaceSetting = event.InterfaceSetting;
        operation->EndpointAttributes = event.EndpointAttributes;
        operation->EndpointInterval = event.EndpointInterval;
        operation->EndpointMaxPacketSize = event.EndpointMaxPacketSize;
        operation->EndpointSequence = event.EndpointSequence;
        operation->DeviceSequence = event.DeviceSequence;
        operation->EndpointGeneration = event.EndpointGeneration;
        // Lifecycle and cancel notifications have an empty canonical tail.
        // Keep both offsets at the first byte after the fixed header so the
        // same strict parser contract applies to notifications and URBs.
        operation->IsoPacketsOffset = sizeof(*operation);
        operation->PayloadOffset = sizeof(*operation);
        WdfRequestSetInformation(dequeueRequest, sizeof(*operation));
        InterlockedIncrement64(&controllerContext->NotificationEventsDelivered);
        WdfRequestComplete(dequeueRequest, STATUS_SUCCESS);
    }
}

static
VOID
ViiperPendingOperationStartedLocked(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext
    )
{
    // All callers hold BrokerLock. Clear before publishing the 0 -> 1
    // transition so teardown can never observe a stale signaled event.
    if (InterlockedCompareExchange(&ControllerContext->PendingOperations, 0, 0) == 0) {
        KeClearEvent(&ControllerContext->BrokerOperationsDrained);
    }
    (VOID)InterlockedIncrement(&ControllerContext->PendingOperations);
}

static
VOID
ViiperPendingOperationCompletedLocked(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext
    )
{
    LONG remaining = InterlockedDecrement(&ControllerContext->PendingOperations);

    NT_ASSERT(remaining >= 0);
    if (remaining == 0) {
        KeSetEvent(&ControllerContext->BrokerOperationsDrained, IO_NO_INCREMENT, FALSE);
    }
}

static
VOID
ViiperClearManagementSlotLocked(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ ULONG Slot,
    _Out_ UDECXUSBDEVICE *DeviceReference,
    _Out_ UDECXUSBENDPOINT *EndpointReference
    )
{
    VIIPER_UDE_MANAGEMENT_SLOT *pending = &ControllerContext->ManagementSlots[Slot];

    *DeviceReference = pending->Device;
    *EndpointReference = pending->Endpoint;
    pending->Request = WDF_NO_HANDLE;
    pending->Device = WDF_NO_HANDLE;
    pending->Endpoint = WDF_NO_HANDLE;
    pending->OwnerFile = WDF_NO_HANDLE;
    pending->Token = 0;
    pending->DeviceId = 0;
    pending->ResetEpoch = 0;
    pending->DeviceGeneration = 0;
    pending->EndpointGeneration = 0;
    pending->State = ViiperUdePendingEmpty;
    pending->Kind = 0;
    pending->EndpointAddress = 0;
    ViiperPendingOperationCompletedLocked(ControllerContext);
}

static
VOID
ViiperReleaseManagementSlotReferences(
    _In_opt_ UDECXUSBDEVICE Device,
    _In_opt_ UDECXUSBENDPOINT Endpoint
    )
{
    // Dereferencing can make framework cleanup runnable. Never do it under
    // BrokerLock: endpoint/device cleanup also uses that lock to close
    // admission, and WDF references postpone destruction rather than the
    // documented EvtCleanup no-access boundary.
    if (Endpoint != WDF_NO_HANDLE) {
        WdfObjectDereference(Endpoint);
    }
    if (Device != WDF_NO_HANDLE) {
        WdfObjectDereference(Device);
    }
}

static
VOID
ViiperUnlinkAdmissionLocked(
    _In_ VIIPER_UDE_PENDING_SLOT *Pending
    )
{
    if (!Pending->AdmissionLinked) {
        return;
    }
    RemoveEntryList(&Pending->AdmissionEntry);
    InitializeListHead(&Pending->AdmissionEntry);
    Pending->AdmissionLinked = FALSE;
}

static
BOOLEAN
ViiperAdmissionCanPublishLocked(
    _In_ const VIIPER_UDE_PENDING_SLOT *Pending
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext;

    if (!Pending->AdmissionLinked || Pending->Endpoint == WDF_NO_HANDLE) {
        return FALSE;
    }
    endpointContext = ViiperGetEndpointContext(Pending->Endpoint);
    if (Pending->EndpointGeneration == 0 ||
        Pending->EndpointGeneration != endpointContext->Generation) {
        return FALSE;
    }
    return endpointContext->AdmissionQueue.Flink == &Pending->AdmissionEntry;
}

static
VOID
ViiperEndpointOperationCompletedLocked(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    LONG remaining = InterlockedDecrement(&endpointContext->ActiveOperations);
    NT_ASSERT(remaining >= 0);
    if (remaining == 0) {
        KeSetEvent(&endpointContext->OperationsDrained, IO_NO_INCREMENT, FALSE);
    }
}

static
VOID
ViiperClearSlotLocked(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ ULONG Slot
    )
{
    VIIPER_UDE_PENDING_SLOT *pending = &ControllerContext->PendingSlots[Slot];
    UDECXUSBENDPOINT endpoint = pending->Endpoint;
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = NULL;

    if (endpoint != WDF_NO_HANDLE) {
        deviceContext = ViiperGetDeviceContext(ViiperGetEndpointContext(endpoint)->Device);
    }

    ViiperUnlinkAdmissionLocked(pending);
    pending->Request = WDF_NO_HANDLE;
    pending->Endpoint = WDF_NO_HANDLE;
    pending->Token = 0;
    pending->DeviceId = 0;
    pending->AdmissionSequence = 0;
    pending->DeviceGeneration = 0;
    pending->EndpointGeneration = 0;
    pending->State = ViiperUdePendingEmpty;
    pending->AbortPending = FALSE;
    pending->PublishedToOwner = FALSE;
    pending->AdmissionLinked = FALSE;
    pending->EndpointAddress = 0;
    pending->AbortStatus = STATUS_SUCCESS;
    pending->CompletionStatus = STATUS_SUCCESS;
    pending->CompletionUsbdStatus = USBD_STATUS_SUCCESS;
    pending->CompleteWithNtStatus = FALSE;
    ViiperPendingOperationCompletedLocked(ControllerContext);
    if (deviceContext != NULL) {
        InterlockedDecrement(&deviceContext->PendingOperations);
    }
    if (endpoint != WDF_NO_HANDLE) {
        ViiperEndpointOperationCompletedLocked(endpoint);
    }
}

_IRQL_requires_max_(DISPATCH_LEVEL)
VOID
ViiperEndpointOperationStarted(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    LONG active = InterlockedCompareExchange(&endpointContext->ActiveOperations, 0, 0);

    // Every caller holds the controller BrokerLock. The same lock owns the
    // final decrement below, so clearing before the 0 -> 1 increment is one
    // linearized transaction. Without BrokerLock a concurrent 1 -> 0
    // completion could signal between those steps; incrementing first instead
    // would expose active == 1 while the drain event was still signaled.
    NT_ASSERT(active >= 0);
    if (active == 0) {
        KeClearEvent(&endpointContext->OperationsDrained);
    }
    active = InterlockedIncrement(&endpointContext->ActiveOperations);
    NT_ASSERT(active > 0);
}

_IRQL_requires_max_(DISPATCH_LEVEL)
VOID
ViiperEndpointOperationCompleted(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);

    // External callers retain an admitted endpoint operation while reaching
    // this wrapper. Do not touch Endpoint after the locked decrement can expose
    // zero to the purge worker and ultimately allow UdeCx cleanup to begin.
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    ViiperEndpointOperationCompletedLocked(Endpoint);
    WdfSpinLockRelease(controllerContext->BrokerLock);
}

static
BOOLEAN
ViiperSlotMatches(
    _In_ const VIIPER_UDE_PENDING_SLOT *Pending,
    _In_ WDFREQUEST Request,
    _In_ ULONGLONG Token
    )
{
    return Pending->Request == Request && Pending->Token == Token &&
        Pending->State != ViiperUdePendingEmpty;
}

NTSTATUS
ViiperValidateBrokerOwner(
    _In_ WDFDEVICE Controller,
    _In_ WDFREQUEST Request
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(Controller);
    WDFFILEOBJECT fileObject = WdfRequestGetFileObject(Request);
    VIIPER_UDE_FILE_CONTEXT *fileContext;
    NTSTATUS status = STATUS_SUCCESS;

    if (fileObject == WDF_NO_HANDLE) {
        return STATUS_INVALID_HANDLE;
    }
    fileContext = ViiperGetFileContext(fileObject);
    // EvtFileCleanup can run with this request outstanding, but KMDF keeps the
    // request-associated file object alive. BrokerOwner and Negotiated only
    // transition to TRUE, while Closing is set through InterlockedExchange
    // before cleanup takes OwnerLock or admits a successor. A request which
    // wins before that permanent close boundary may finish against its exact
    // device owner/generation; one which loses it must fail without joining
    // unrelated input publishers at the controller-wide OwnerLock.
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0 ||
        InterlockedCompareExchange(&fileContext->BrokerOwner, 0, 0) == 0 ||
        InterlockedCompareExchange(&fileContext->Negotiated, 0, 0) == 0 ||
        InterlockedCompareExchange(&fileContext->Closing, 0, 0) != 0) {
        status = STATUS_INVALID_DEVICE_STATE;
    }
    return status;
}

NTSTATUS
ViiperInitializeBroker(
    _In_ WDFDEVICE Device
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(Device);
    WDF_OBJECT_ATTRIBUTES attributes;
    WDF_DPC_CONFIG dpcConfig;
    NTSTATUS status;

    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = Device;
    status = WdfSpinLockCreate(&attributes, &controllerContext->BrokerLock);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = Device;
    status = WdfMemoryCreate(
        &attributes,
        NonPagedPoolNx,
        0x56495542,
        sizeof(VIIPER_UDE_PENDING_SLOT) * VIIPER_UDE_MAX_PENDING_OPERATIONS,
        &controllerContext->PendingStorage,
        (PVOID *)&controllerContext->PendingSlots);
    if (!NT_SUCCESS(status)) {
        controllerContext->PendingStorage = WDF_NO_HANDLE;
        controllerContext->PendingSlots = NULL;
        return status;
    }

    RtlZeroMemory(
        controllerContext->PendingSlots,
        sizeof(VIIPER_UDE_PENDING_SLOT) * VIIPER_UDE_MAX_PENDING_OPERATIONS);

    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = Device;
    status = WdfMemoryCreate(
        &attributes,
        NonPagedPoolNx,
        0x56495543,
        sizeof(VIIPER_UDE_NOTIFICATION) * VIIPER_UDE_MAX_PENDING_OPERATIONS,
        &controllerContext->NotificationStorage,
        (PVOID *)&controllerContext->Notifications);
    if (!NT_SUCCESS(status)) {
        controllerContext->NotificationStorage = WDF_NO_HANDLE;
        controllerContext->Notifications = NULL;
        return status;
    }
    RtlZeroMemory(
        controllerContext->Notifications,
        sizeof(VIIPER_UDE_NOTIFICATION) * VIIPER_UDE_MAX_PENDING_OPERATIONS);

    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = Device;
    status = WdfMemoryCreate(
        &attributes,
        NonPagedPoolNx,
        0x56495544,
        sizeof(VIIPER_UDE_MANAGEMENT_SLOT) * VIIPER_UDE_MAX_PENDING_MANAGEMENT,
        &controllerContext->ManagementStorage,
        (PVOID *)&controllerContext->ManagementSlots);
    if (!NT_SUCCESS(status)) {
        controllerContext->ManagementStorage = WDF_NO_HANDLE;
        controllerContext->ManagementSlots = NULL;
        return status;
    }
    RtlZeroMemory(
        controllerContext->ManagementSlots,
        sizeof(VIIPER_UDE_MANAGEMENT_SLOT) * VIIPER_UDE_MAX_PENDING_MANAGEMENT);

    WDF_DPC_CONFIG_INIT(&dpcConfig, ViiperEvtCompletionDpc);
    dpcConfig.AutomaticSerialization = WdfFalse;
    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = Device;
    return WdfDpcCreate(
        &dpcConfig, &attributes, &controllerContext->CompletionDpc);
}

_IRQL_requires_max_(DISPATCH_LEVEL)
BOOLEAN
ViiperQueueUrbCompletion(
    _In_ WDFDEVICE Controller,
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ WDFREQUEST Request,
    _In_ ULONG PendingSlot,
    _In_ ULONGLONG Token,
    _In_ NTSTATUS Status,
    _In_ USBD_STATUS UsbdStatus,
    _In_ BOOLEAN CompleteWithNtStatus,
    _In_ ULONG DirectInputBytes,
    _In_ ULONGLONG DirectInputSequence
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(Controller);
    VIIPER_UDE_REQUEST_CONTEXT *requestContext = ViiperGetRequestContext(Request);
    BOOLEAN enqueueDpc = FALSE;

    NT_ASSERT(KeGetCurrentIrql() <= DISPATCH_LEVEL);
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (requestContext->CompletionQueued) {
        WdfSpinLockRelease(controllerContext->BrokerLock);
        NT_ASSERT(FALSE);
        return FALSE;
    }
    NT_ASSERT(requestContext->DeviceGeneration != 0);
    NT_ASSERT(requestContext->EndpointGeneration != 0);
    NT_ASSERT(requestContext->Endpoint == Endpoint);

    WdfObjectReference(Request);
    requestContext->CompletionRequest = Request;
    requestContext->Controller = Controller;
    requestContext->Endpoint = Endpoint;
    requestContext->PendingSlot = PendingSlot;
    requestContext->Token = Token;
    requestContext->CompletionStatus = Status;
    requestContext->CompletionUsbdStatus = UsbdStatus;
    requestContext->CompleteWithNtStatus = CompleteWithNtStatus;
    requestContext->DirectInputBytes = DirectInputBytes;
    requestContext->DirectInputSequence = DirectInputSequence;
    requestContext->CompletionQueued = TRUE;
    if (InterlockedCompareExchange(&controllerContext->PendingCompletions, 0, 0) == 0) {
        KeClearEvent(&controllerContext->CompletionOperationsDrained);
    }
    (VOID)InterlockedIncrement(&controllerContext->PendingCompletions);
    InsertTailList(&controllerContext->CompletionQueue, &requestContext->CompletionEntry);
    if (!controllerContext->CompletionDpcActive) {
        controllerContext->CompletionDpcActive = TRUE;
        enqueueDpc = TRUE;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);

    if (enqueueDpc) {
        // A running KDPC has already left the system queue, so it can be
        // requeued during its final empty-queue handoff. A FALSE result only
        // means another invocation is already queued.
        (VOID)WdfDpcEnqueue(controllerContext->CompletionDpc);
    }
    return TRUE;
}

VOID
ViiperEvtCompletionDpc(
    _In_ WDFDPC Dpc
    )
{
    WDFDEVICE controller = (WDFDEVICE)WdfDpcGetParentObject(Dpc);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(controller);

    // The authored UDE host-compatibility contract requires terminal URB
    // completion at DISPATCH_LEVEL and on a separate DPC when processing began
    // synchronously. Keep this boundary even though generated per-function
    // documentation has carried conflicting IRQL metadata: the shipped class-
    // extension helpers are nonpaged wrappers which complete at caller IRQL.
    NT_ASSERT(KeGetCurrentIrql() == DISPATCH_LEVEL);

    for (;;) {
        WDFREQUEST request = WDF_NO_HANDLE;
        UDECXUSBENDPOINT endpoint = WDF_NO_HANDLE;
        ULONGLONG token = 0;
        ULONG slot = VIIPER_UDE_MAX_PENDING_OPERATIONS;
        NTSTATUS completionStatus = STATUS_SUCCESS;
        USBD_STATUS usbdStatus = USBD_STATUS_SUCCESS;
        BOOLEAN completeWithNtStatus = FALSE;
        ULONG directInputBytes = 0;
        ULONGLONG directInputSequence = 0;
        ULONG deviceGeneration = 0;
        ULONG endpointGeneration = 0;
        BOOLEAN ownershipReleased = FALSE;
        PLIST_ENTRY entry;
        VIIPER_UDE_REQUEST_CONTEXT *requestContext;
        LONG remaining;

        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (IsListEmpty(&controllerContext->CompletionQueue)) {
            controllerContext->CompletionDpcActive = FALSE;
            WdfSpinLockRelease(controllerContext->BrokerLock);
            break;
        }
        entry = RemoveHeadList(&controllerContext->CompletionQueue);
        requestContext = CONTAINING_RECORD(
            entry, VIIPER_UDE_REQUEST_CONTEXT, CompletionEntry);
        request = requestContext->CompletionRequest;
        endpoint = requestContext->Endpoint;
        token = requestContext->Token;
        slot = requestContext->PendingSlot;
        completionStatus = requestContext->CompletionStatus;
        usbdStatus = requestContext->CompletionUsbdStatus;
        completeWithNtStatus = requestContext->CompleteWithNtStatus;
        directInputBytes = requestContext->DirectInputBytes;
        directInputSequence = requestContext->DirectInputSequence;
        deviceGeneration = requestContext->DeviceGeneration;
        endpointGeneration = requestContext->EndpointGeneration;
        requestContext->CompletionRequest = WDF_NO_HANDLE;
        requestContext->CompletionQueued = FALSE;
        if (slot < VIIPER_UDE_MAX_PENDING_OPERATIONS) {
            BOOLEAN slotOwned =
                ViiperSlotMatches(&controllerContext->PendingSlots[slot], request, token) &&
                controllerContext->PendingSlots[slot].State ==
                    ViiperUdePendingDpcCompletion;
            NT_ASSERT(slotOwned);
            if (slotOwned) {
                controllerContext->PendingSlots[slot].State = ViiperUdePendingCompleting;
            }
        } else {
            NT_ASSERT(token == 0);
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);

        if (endpoint == WDF_NO_HANDLE || deviceGeneration == 0 || endpointGeneration == 0 ||
            ViiperGetEndpointContext(endpoint)->Generation != endpointGeneration ||
            ViiperGetDeviceContext(ViiperGetEndpointContext(endpoint)->Device)->Generation !=
                deviceGeneration) {
            NT_ASSERT(FALSE);
            completionStatus = STATUS_DEVICE_NOT_READY;
            completeWithNtStatus = TRUE;
            directInputBytes = 0;
            directInputSequence = 0;
        }

        if (completeWithNtStatus) {
            UdecxUrbCompleteWithNtStatus(request, completionStatus);
        } else {
            UdecxUrbComplete(request, usbdStatus);
        }

        if (directInputBytes != 0 &&
            !completeWithNtStatus &&
            usbdStatus == USBD_STATUS_SUCCESS &&
            directInputSequence != 0) {
            InterlockedAdd64(&controllerContext->BytesFromDevice, directInputBytes);
            InterlockedIncrement64(&controllerContext->InputReportsCompleted);
        } else if (directInputBytes != 0) {
            NT_ASSERT(FALSE);
        }

        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (slot < VIIPER_UDE_MAX_PENDING_OPERATIONS &&
            ViiperSlotMatches(&controllerContext->PendingSlots[slot], request, token) &&
            controllerContext->PendingSlots[slot].State == ViiperUdePendingCompleting) {
            ViiperClearSlotLocked(controllerContext, slot);
            ownershipReleased = TRUE;
        } else if (slot >= VIIPER_UDE_MAX_PENDING_OPERATIONS &&
            endpoint != WDF_NO_HANDLE) {
            ViiperEndpointOperationCompletedLocked(endpoint);
            ownershipReleased = TRUE;
        }
        if (!ownershipReleased) {
            NT_ASSERT(FALSE);
        }
        remaining = InterlockedDecrement(&controllerContext->PendingCompletions);
        NT_ASSERT(remaining >= 0);
        if (remaining == 0) {
            KeSetEvent(
                &controllerContext->CompletionOperationsDrained,
                IO_NO_INCREMENT,
                FALSE);
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);
        // Endpoint rundown, not a WDF reference, is the lifetime fence. UdeCx
        // cannot pass PurgeComplete until the locked decrement above, and this
        // DPC performs no endpoint access after that final release.
        WdfObjectDereference(request);
    }
}

_IRQL_requires_(PASSIVE_LEVEL)
VOID
ViiperDrainUrbCompletions(
    _In_ WDFDEVICE Controller
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(Controller);
    LARGE_INTEGER watchdogWait;

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);
    watchdogWait.QuadPart =
        -(LONGLONG)VIIPER_UDE_RUNDOWN_WATCHDOG_INTERVAL_100NS;
    for (;;) {
        BOOLEAN drained;
        NTSTATUS waitStatus;

        waitStatus = KeWaitForSingleObject(
            &controllerContext->CompletionOperationsDrained,
            Executive,
            KernelMode,
            FALSE,
            &watchdogWait);
        if (waitStatus == STATUS_TIMEOUT) {
            LONG pendingCompletions;
            ULONG completionDpcActive;

            WdfSpinLockAcquire(controllerContext->BrokerLock);
            pendingCompletions = InterlockedCompareExchange(
                &controllerContext->PendingCompletions, 0, 0);
            completionDpcActive =
                controllerContext->CompletionDpcActive ? 1U : 0U;
            WdfSpinLockRelease(controllerContext->BrokerLock);
            VIIPER_TRACE_LIFECYCLE(
                Controller,
                VIIPER_UDE_TRACE_SOURCE_BROKER,
                VIIPER_UDE_TRACE_COMPLETION_RUNDOWN_WATCHDOG,
                0,
                0,
                WDF_NO_HANDLE,
                WDF_NO_HANDLE,
                0,
                STATUS_IO_TIMEOUT,
                pendingCompletions,
                completionDpcActive);
        } else {
            NT_ASSERT(waitStatus == STATUS_SUCCESS);
        }
        (VOID)WdfDpcCancel(controllerContext->CompletionDpc, TRUE);

        // Closing the device's I/O queues precedes this join. If cancellation
        // won the narrow interval before a queued DPC began, re-arm that
        // already-owned list instead of abandoning its request references.
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        drained = IsListEmpty(&controllerContext->CompletionQueue) &&
            InterlockedCompareExchange(&controllerContext->PendingCompletions, 0, 0) == 0;
        if (!drained) {
            controllerContext->CompletionDpcActive = TRUE;
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);
        if (drained) {
            break;
        }
        (VOID)WdfDpcEnqueue(controllerContext->CompletionDpc);
    }
}

static
BOOLEAN
ViiperLifecycleOwnerSessionActiveLocked(
    _In_ VIIPER_UDE_DEVICE_CONTEXT *DeviceContext
    )
{
    WDFFILEOBJECT ownerFile;
    VIIPER_UDE_FILE_CONTEXT *fileContext;

    // Device removal is asynchronous in UdeCx.  The old child can therefore
    // deliver endpoint/power callbacks after its logical table slot has been
    // released and a successor broker has connected.  The child retains its
    // creating file object until EvtCleanup, so that file's permanent Closing
    // transition is the generation fence which prevents those callbacks from
    // entering the controller-wide notification FIFO of the new session.
    //
    // BrokerLock is the lifecycle admission linearization point.  Do not take
    // OwnerLock here: cleanup takes OwnerLock before BrokerLock and reversing
    // that order would deadlock.  Closing is set before cleanup takes either
    // lock, while Purging is set under BrokerLock before the logical slot is
    // released.
    if (InterlockedCompareExchange(&DeviceContext->Purging, 0, 0) != 0 ||
        InterlockedCompareExchange(&DeviceContext->OwnerReferenced, 0, 0) == 0) {
        return FALSE;
    }
    ownerFile = DeviceContext->OwnerFile;
    if (ownerFile == WDF_NO_HANDLE) {
        return FALSE;
    }
    fileContext = ViiperGetFileContext(ownerFile);
    return InterlockedCompareExchange(&fileContext->BrokerOwner, 0, 0) != 0 &&
        InterlockedCompareExchange(&fileContext->Negotiated, 0, 0) != 0 &&
        InterlockedCompareExchange(&fileContext->Closing, 0, 0) == 0;
}

static
BOOLEAN
ViiperQueueLifecycleEventLocked(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ VIIPER_UDE_DEVICE_CONTEXT *DeviceContext,
    _In_opt_ const USB_ENDPOINT_DESCRIPTOR *EndpointDescriptor,
    _In_ ULONG EndpointGeneration,
    _In_ VIIPER_UDE_OPERATION_KIND Kind,
    _In_ UCHAR InterfaceNumber,
    _In_ UCHAR InterfaceSetting,
    _In_ ULONGLONG Token
    )
{
    VIIPER_UDE_NOTIFICATION *event;

    // Keep this defensive check in the common insertion primitive so a future
    // lifecycle producer cannot bypass the old-owner generation fence.  It is
    // deliberately before both sequence increments: a stale child must leave
    // no observable hole in its successor's lifecycle stream.
    if (!ViiperLifecycleOwnerSessionActiveLocked(DeviceContext)) {
        return FALSE;
    }
    if (ControllerContext->NotificationCount >= VIIPER_UDE_MAX_PENDING_OPERATIONS - 1) {
        (VOID)ViiperFaultBrokerLocked(ControllerContext);
        return FALSE;
    }

    event = &ControllerContext->Notifications[ControllerContext->NotificationTail];
    RtlZeroMemory(event, sizeof(*event));
    event->Token = Token;
    event->DeviceId = DeviceContext->DeviceId;
    event->Generation = DeviceContext->Generation;
    event->EndpointGeneration = EndpointGeneration;
    event->Kind = Kind;
    if (EndpointDescriptor != NULL) {
        event->EndpointAddress = EndpointDescriptor->bEndpointAddress;
        event->EndpointAttributes = EndpointDescriptor->bmAttributes;
        event->EndpointInterval = EndpointDescriptor->bInterval;
        event->EndpointMaxPacketSize = EndpointDescriptor->wMaxPacketSize;
    }
    event->InterfaceNumber = InterfaceNumber;
    event->InterfaceSetting = InterfaceSetting;
    if (EndpointGeneration != 0) {
        event->EndpointSequence = (ULONGLONG)InterlockedIncrement64(
            &DeviceContext->EndpointSequences[event->EndpointAddress]);
    } else {
        event->EndpointSequence = (ULONGLONG)InterlockedIncrement64(
            &DeviceContext->DeviceLifecycleSequence);
    }
    event->DeviceSequence = (ULONGLONG)InterlockedIncrement64(
        &DeviceContext->DeviceSequence);
    ControllerContext->NotificationTail = (ControllerContext->NotificationTail + 1) %
        VIIPER_UDE_MAX_PENDING_OPERATIONS;
    ++ControllerContext->NotificationCount;
    return TRUE;
}

NTSTATUS
ViiperQueueEndpointLifecycleEvent(
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ VIIPER_UDE_OPERATION_KIND Kind
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    BOOLEAN ownerActive;
    BOOLEAN active;
    BOOLEAN queued;
    BOOLEAN faulted;

    WdfSpinLockAcquire(controllerContext->BrokerLock);
    ownerActive = InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0 &&
        ViiperLifecycleOwnerSessionActiveLocked(deviceContext);
    active = ownerActive && InterlockedCompareExchange(
        &controllerContext->BrokerFaulted, FALSE, FALSE) == FALSE;
    queued = active &&
        ViiperQueueLifecycleEventLocked(
            controllerContext,
            deviceContext,
            &endpointContext->Descriptor,
            endpointContext->Generation,
            Kind,
            0,
            0,
            0);
    faulted = ownerActive && InterlockedCompareExchange(
        &controllerContext->BrokerFaulted, FALSE, FALSE) != FALSE;
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (queued || faulted) {
        // Queue overflow can publish the terminal broker-fault record instead
        // of this lifecycle event.  Dispatch that record even though the
        // original insertion failed, otherwise already-waiting dequeue IOCTLs
        // can remain parked forever with the fault hidden behind them.
        // Lifecycle publication has priority over ordinary URBs.  Wake only
        // the notification path here so a purge/reset callback cannot also
        // publish unrelated media merely because it reported a boundary.
        ViiperDispatchNotificationEvents(deviceContext->Controller);
    }
    if (!active) {
        return STATUS_DEVICE_NOT_READY;
    }
    if (!queued) {
        return STATUS_INSUFFICIENT_RESOURCES;
    }
    return STATUS_SUCCESS;
}

NTSTATUS
ViiperQueueDeviceLifecycleEvent(
    _In_ UDECXUSBDEVICE Device,
    _In_ VIIPER_UDE_OPERATION_KIND Kind
    )
{
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    BOOLEAN ownerActive;
    BOOLEAN active;
    BOOLEAN queued;
    BOOLEAN faulted;

    WdfSpinLockAcquire(controllerContext->BrokerLock);
    ownerActive = InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0 &&
        ViiperLifecycleOwnerSessionActiveLocked(deviceContext);
    active = ownerActive && InterlockedCompareExchange(
        &controllerContext->BrokerFaulted, FALSE, FALSE) == FALSE;
    queued = active &&
        ViiperQueueLifecycleEventLocked(
            controllerContext, deviceContext, NULL, 0, Kind, 0, 0, 0);
    faulted = ownerActive && InterlockedCompareExchange(
        &controllerContext->BrokerFaulted, FALSE, FALSE) != FALSE;
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (queued || faulted) {
        ViiperDispatchNotificationEvents(deviceContext->Controller);
    }
    if (!active) {
        return STATUS_DEVICE_NOT_READY;
    }
    if (!queued) {
        return STATUS_INSUFFICIENT_RESOURCES;
    }
    return STATUS_SUCCESS;
}

NTSTATUS
ViiperQueueInterfaceLifecycleEvent(
    _In_ UDECXUSBDEVICE Device,
    _In_ UCHAR InterfaceNumber,
    _In_ UCHAR InterfaceSetting
    )
{
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    BOOLEAN ownerActive;
    BOOLEAN active;
    BOOLEAN queued;
    BOOLEAN faulted;

    WdfSpinLockAcquire(controllerContext->BrokerLock);
    ownerActive = InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0 &&
        ViiperLifecycleOwnerSessionActiveLocked(deviceContext);
    active = ownerActive && InterlockedCompareExchange(
        &controllerContext->BrokerFaulted, FALSE, FALSE) == FALSE;
    queued = active &&
        ViiperQueueLifecycleEventLocked(
            controllerContext,
            deviceContext,
            NULL,
            0,
            ViiperUdeOperationSetInterface,
            InterfaceNumber,
            InterfaceSetting,
            0);
    faulted = ownerActive && InterlockedCompareExchange(
        &controllerContext->BrokerFaulted, FALSE, FALSE) != FALSE;
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (queued || faulted) {
        ViiperDispatchNotificationEvents(deviceContext->Controller);
    }
    if (!active) {
        return STATUS_DEVICE_NOT_READY;
    }
    if (!queued) {
        return STATUS_INSUFFICIENT_RESOURCES;
    }
    return STATUS_SUCCESS;
}

static
NTSTATUS
ViiperQueueAcknowledgedLifecycleEvent(
    _In_ UDECXUSBDEVICE Device,
    _In_opt_ UDECXUSBENDPOINT Endpoint,
    _In_ WDFREQUEST Request,
    _In_ VIIPER_UDE_OPERATION_KIND Kind,
    _In_ UCHAR InterfaceNumber,
    _In_ UCHAR InterfaceSetting
    )
{
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    const USB_ENDPOINT_DESCRIPTOR *descriptor = NULL;
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = NULL;
    ULONG offset;
    NTSTATUS status = STATUS_INSUFFICIENT_RESOURCES;
    BOOLEAN canAllocate = TRUE;
    BOOLEAN ownerActive = FALSE;
    BOOLEAN faulted = FALSE;

    // The management slot owns these generic references until every terminal
    // clear path snapshots and releases them outside BrokerLock. Besides
    // retaining the opaque values, this prevents WDF from recycling either
    // handle while a delayed acknowledgement is compared with the live table.
    WdfObjectReference(Device);
    if (Endpoint != WDF_NO_HANDLE) {
        WdfObjectReference(Endpoint);
    }
    if (Endpoint != WDF_NO_HANDLE) {
        endpointContext = ViiperGetEndpointContext(Endpoint);
        descriptor = &endpointContext->Descriptor;
    }

    WdfSpinLockAcquire(controllerContext->BrokerLock);
    ownerActive = ViiperLifecycleOwnerSessionActiveLocked(deviceContext);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0 ||
        InterlockedCompareExchange(&controllerContext->BrokerFaulted, FALSE, FALSE) != FALSE ||
        !ownerActive) {
        status = STATUS_DEVICE_NOT_READY;
        canAllocate = FALSE;
    } else if (Kind == ViiperUdeOperationDeviceReset &&
            (InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) == 0 ||
             InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0)) {
        status = STATUS_DEVICE_NOT_READY;
        canAllocate = FALSE;
    } else if (Kind == ViiperUdeOperationEndpointReset &&
            (endpointContext == NULL ||
             InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) != 0 ||
             InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0 ||
             InterlockedCompareExchange(&endpointContext->Resetting, 0, 0) == 0 ||
             InterlockedCompareExchange(&endpointContext->Purging, 0, 0) != 0 ||
             InterlockedCompareExchange64(&deviceContext->ResetEpoch, 0, 0) !=
                InterlockedCompareExchange64(&endpointContext->ResetDeviceEpoch, 0, 0))) {
        // A device reset admitted after the endpoint worker's first proof
        // supersedes that endpoint transaction before it can be published.
        status = STATUS_DEVICE_NOT_READY;
        canAllocate = FALSE;
    } else if (controllerContext->NotificationCount >=
            VIIPER_UDE_MAX_PENDING_OPERATIONS - 1) {
        (VOID)ViiperFaultBrokerLocked(controllerContext);
        status = STATUS_INSUFFICIENT_RESOURCES;
        canAllocate = FALSE;
    }
    for (offset = 0; canAllocate && status == STATUS_INSUFFICIENT_RESOURCES &&
            offset < VIIPER_UDE_MAX_PENDING_MANAGEMENT; ++offset) {
        ULONG index = (controllerContext->NextManagementSlot + offset) %
            VIIPER_UDE_MAX_PENDING_MANAGEMENT;
        VIIPER_UDE_MANAGEMENT_SLOT *pending = &controllerContext->ManagementSlots[index];
        ULONGLONG token;

        if (pending->State != ViiperUdePendingEmpty || pending->RetiredToken != 0 ||
            pending->Generation == MAXULONG) {
            continue;
        }
        ++pending->Generation;
        token = ((ULONGLONG)pending->Generation << 32) |
            VIIPER_UDE_MANAGEMENT_SLOT_FLAG | (index + 1);
        pending->Request = Request;
        pending->Device = Device;
        pending->Endpoint = Endpoint;
        pending->OwnerFile = deviceContext->OwnerFile;
        pending->Token = token;
        pending->DeviceId = deviceContext->DeviceId;
        pending->ResetEpoch = endpointContext != NULL
            ? (ULONGLONG)InterlockedCompareExchange64(
                &endpointContext->ResetDeviceEpoch, 0, 0)
            : (ULONGLONG)InterlockedCompareExchange64(
                &deviceContext->ResetEpoch, 0, 0);
        pending->DeviceGeneration = deviceContext->Generation;
        pending->EndpointGeneration = endpointContext != NULL
            ? endpointContext->Generation
            : 0;
        pending->State = ViiperUdePendingQueued;
        pending->Kind = Kind;
        pending->EndpointAddress = descriptor != NULL ? descriptor->bEndpointAddress : 0;
        if (!ViiperQueueLifecycleEventLocked(
                controllerContext,
                deviceContext,
                descriptor,
                pending->EndpointGeneration,
                Kind,
                InterfaceNumber,
                InterfaceSetting,
                token)) {
            pending->Request = WDF_NO_HANDLE;
            pending->Device = WDF_NO_HANDLE;
            pending->Endpoint = WDF_NO_HANDLE;
            pending->OwnerFile = WDF_NO_HANDLE;
            pending->Token = 0;
            pending->DeviceId = 0;
            pending->ResetEpoch = 0;
            pending->DeviceGeneration = 0;
            pending->EndpointGeneration = 0;
            pending->State = ViiperUdePendingEmpty;
            pending->Kind = 0;
            pending->EndpointAddress = 0;
            break;
        }
        controllerContext->NextManagementSlot = (index + 1) %
            VIIPER_UDE_MAX_PENDING_MANAGEMENT;
        ViiperPendingOperationStartedLocked(controllerContext);
        status = STATUS_SUCCESS;
        break;
    }
    faulted = ownerActive && InterlockedCompareExchange(
        &controllerContext->BrokerFaulted, FALSE, FALSE) != FALSE;
    WdfSpinLockRelease(controllerContext->BrokerLock);

    if (status == STATUS_INSUFFICIENT_RESOURCES) {
        InterlockedIncrement64(&controllerContext->QueueExhaustions);
    }
    if (NT_SUCCESS(status) || faulted) {
        ViiperDispatchNotificationEvents(deviceContext->Controller);
    }
    if (!NT_SUCCESS(status)) {
        // No caller-owned context access is permitted after the last generic
        // reference can make deferred WDF destruction runnable.
        ViiperReleaseManagementSlotReferences(Device, Endpoint);
    }
    return status;
}

NTSTATUS
ViiperQueueAcknowledgedEndpointLifecycleEvent(
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ WDFREQUEST Request,
    _In_ VIIPER_UDE_OPERATION_KIND Kind
    )
{
    return ViiperQueueAcknowledgedLifecycleEvent(
        ViiperGetEndpointContext(Endpoint)->Device, Endpoint, Request, Kind, 0, 0);
}

NTSTATUS
ViiperQueueAcknowledgedDeviceLifecycleEvent(
    _In_ UDECXUSBDEVICE Device,
    _In_ WDFREQUEST Request,
    _In_ VIIPER_UDE_OPERATION_KIND Kind
    )
{
    return ViiperQueueAcknowledgedLifecycleEvent(
        Device, WDF_NO_HANDLE, Request, Kind, 0, 0);
}

NTSTATUS
ViiperQueueAcknowledgedInterfaceLifecycleEvent(
    _In_ UDECXUSBDEVICE Device,
    _In_ WDFREQUEST Request,
    _In_ UCHAR InterfaceNumber,
    _In_ UCHAR InterfaceSetting
    )
{
    return ViiperQueueAcknowledgedLifecycleEvent(
        Device,
        WDF_NO_HANDLE,
        Request,
        ViiperUdeOperationSetInterface,
        InterfaceNumber,
        InterfaceSetting);
}

static
NTSTATUS
ViiperAllocatePendingSlot(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ WDFREQUEST Request,
    _In_ UDECXUSBENDPOINT Endpoint,
    _Out_ ULONG *Slot,
    _Out_ ULONGLONG *Token
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    ULONG offset;
    NTSTATUS status = STATUS_INSUFFICIENT_RESOURCES;

    WdfSpinLockAcquire(ControllerContext->BrokerLock);
    if (InterlockedCompareExchange(&ControllerContext->ShuttingDown, 0, 0) != 0 ||
        InterlockedCompareExchange(&ControllerContext->BrokerFaulted, FALSE, FALSE) != FALSE ||
        InterlockedCompareExchange(&deviceContext->InD0, 0, 0) == 0 ||
        InterlockedCompareExchange(&endpointContext->Purging, 0, 0) != 0 ||
        InterlockedCompareExchange(&endpointContext->Resetting, 0, 0) != 0 ||
        InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) != 0 ||
        InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0) {
        status = STATUS_DEVICE_NOT_READY;
    } else if ((ULONG)InterlockedCompareExchange(
            &deviceContext->PendingOperations, 0, 0) >=
            deviceContext->MaxPendingOperations) {
        status = STATUS_QUOTA_EXCEEDED;
    }
    for (offset = 0; status == STATUS_INSUFFICIENT_RESOURCES &&
            offset < VIIPER_UDE_MAX_PENDING_OPERATIONS; ++offset) {
        ULONG index = (ControllerContext->NextPendingSlot + offset) %
            VIIPER_UDE_MAX_PENDING_OPERATIONS;
        VIIPER_UDE_PENDING_SLOT *pending = &ControllerContext->PendingSlots[index];
        if (pending->State != ViiperUdePendingEmpty || pending->Generation == MAXULONG) {
            continue;
        }
        NT_ASSERT(!pending->AdmissionLinked);
        ++pending->Generation;
        pending->Request = Request;
        pending->Endpoint = Endpoint;
        pending->Token = ((ULONGLONG)pending->Generation << 32) | (index + 1);
        pending->DeviceId = deviceContext->DeviceId;
        ++endpointContext->NextAdmissionSequence;
        if (endpointContext->NextAdmissionSequence == 0) {
            ++endpointContext->NextAdmissionSequence;
        }
        pending->AdmissionSequence = endpointContext->NextAdmissionSequence;
        pending->DeviceGeneration = deviceContext->Generation;
        pending->EndpointGeneration = endpointContext->Generation;
        pending->State = ViiperUdePendingPreparing;
        pending->AbortPending = FALSE;
        pending->PublishedToOwner = FALSE;
        pending->AdmissionLinked = TRUE;
        pending->EndpointAddress = endpointContext->Descriptor.bEndpointAddress;
        pending->AbortStatus = STATUS_SUCCESS;
        InsertTailList(&endpointContext->AdmissionQueue, &pending->AdmissionEntry);
        ControllerContext->NextPendingSlot = (index + 1) % VIIPER_UDE_MAX_PENDING_OPERATIONS;
        ViiperPendingOperationStartedLocked(ControllerContext);
        InterlockedIncrement(&deviceContext->PendingOperations);
        *Slot = index;
        *Token = pending->Token;
        status = STATUS_SUCCESS;
        break;
    }
    WdfSpinLockRelease(ControllerContext->BrokerLock);

    if (status == STATUS_INSUFFICIENT_RESOURCES || status == STATUS_QUOTA_EXCEEDED) {
        InterlockedIncrement64(&ControllerContext->QueueExhaustions);
    }
    return status;
}

VOID
ViiperEvtUrbCanceledOnQueue(
    _In_ WDFQUEUE Queue,
    _In_ WDFREQUEST Request
    )
{
    WDFDEVICE controller = WdfIoQueueGetDevice(Queue);
    UDECXUSBENDPOINT endpoint = *ViiperGetQueueEndpoint(Queue);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(controller);
    VIIPER_UDE_REQUEST_CONTEXT *requestContext = ViiperGetRequestContext(Request);
    BOOLEAN queued;

    // KMDF has removed this request from the endpoint queue and transferred
    // ownership to this callback. Count that ownership before deferring the
    // terminal call so endpoint purge cannot pass the queued DPC.
    RtlZeroMemory(requestContext, sizeof(*requestContext));
    requestContext->Controller = controller;
    requestContext->Endpoint = endpoint;
    requestContext->PendingSlot = VIIPER_UDE_MAX_PENDING_OPERATIONS;
    requestContext->DeviceGeneration =
        ViiperGetDeviceContext(ViiperGetEndpointContext(endpoint)->Device)->Generation;
    requestContext->EndpointGeneration = ViiperGetEndpointContext(endpoint)->Generation;
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    ViiperEndpointOperationStarted(endpoint);
    WdfSpinLockRelease(controllerContext->BrokerLock);

    queued = ViiperQueueUrbCompletion(
        controller,
        endpoint,
        Request,
        VIIPER_UDE_MAX_PENDING_OPERATIONS,
        0,
        STATUS_CANCELLED,
        USBD_STATUS_CANCELED,
        TRUE,
        0,
        0);
    if (!queued) {
        NT_ASSERT(FALSE);
    }
    InterlockedIncrement64(&controllerContext->OperationsCancelled);
}

VOID
ViiperEvtUrbCancel(
    _In_ WDFREQUEST Request
    )
{
    VIIPER_UDE_REQUEST_CONTEXT *requestContext = ViiperGetRequestContext(Request);
    WDFDEVICE controller = requestContext->Controller;
    UDECXUSBENDPOINT endpoint = requestContext->Endpoint;
    ULONG slot = requestContext->PendingSlot;
    ULONGLONG token = requestContext->Token;
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(controller);
    BOOLEAN ownsRequest = FALSE;
    BOOLEAN notifyOwner = FALSE;
    BOOLEAN dispatchSuccessor = FALSE;

    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (slot < VIIPER_UDE_MAX_PENDING_OPERATIONS) {
        VIIPER_UDE_PENDING_SLOT *pending =
            &controllerContext->PendingSlots[slot];
        if (ViiperSlotMatches(pending, Request, token)) {
            dispatchSuccessor = pending->AdmissionLinked;
            notifyOwner = ViiperQueueCancelEventLocked(controllerContext, pending);
            pending->CompletionStatus = STATUS_CANCELLED;
            pending->CompletionUsbdStatus = USBD_STATUS_CANCELED;
            pending->CompleteWithNtStatus = TRUE;
            pending->State = ViiperUdePendingDpcCompletion;
            ViiperUnlinkAdmissionLocked(pending);
            ownsRequest = TRUE;
        }
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);

    if (ownsRequest) {
        InterlockedIncrement64(&controllerContext->OperationsCancelled);
        (VOID)ViiperQueueUrbCompletion(
            controller,
            endpoint,
            Request,
            slot,
            token,
            STATUS_CANCELLED,
            USBD_STATUS_CANCELED,
            TRUE,
            0,
            0);
        if (notifyOwner) {
            ViiperDispatchNotificationEvents(controller);
        }
        if (dispatchSuccessor) {
            // An endpoint head can be canceled without another broker IOCTL
            // arriving to restart publication. WdfRequestUnmarkCancelable may
            // return STATUS_CANCELLED before this callback runs, so even a
            // Publishing head cannot rely on its old dispatch loop to observe
            // the unlink. Wake dispatch after retiring every linked head so an
            // already-waiting dequeue cannot strand its successor.
            ViiperDispatchAvailable(controller);
        }
    }
}

PURB
ViiperGetUrb(
    _In_ WDFREQUEST Request
    )
{
    PIRP irp = WdfRequestWdmGetIrp(Request);
    if (irp == NULL) {
        return NULL;
    }
    return (PURB)URB_FROM_IRP(irp);
}

static
PMDL
ViiperGetTransferMdl(
    _In_ PURB Urb
    )
{
    switch (Urb->UrbHeader.Function) {
    case URB_FUNCTION_BULK_OR_INTERRUPT_TRANSFER:
    case URB_FUNCTION_BULK_OR_INTERRUPT_TRANSFER_USING_CHAINED_MDL:
        return Urb->UrbBulkOrInterruptTransfer.TransferBufferMDL;
    case URB_FUNCTION_ISOCH_TRANSFER:
    case URB_FUNCTION_ISOCH_TRANSFER_USING_CHAINED_MDL:
        return Urb->UrbIsochronousTransfer.TransferBufferMDL;
    case URB_FUNCTION_CONTROL_TRANSFER:
        return Urb->UrbControlTransfer.TransferBufferMDL;
    case URB_FUNCTION_CONTROL_TRANSFER_EX:
        return Urb->UrbControlTransferEx.TransferBufferMDL;
    default:
        return NULL;
    }
}

ULONG
ViiperGetTransferBufferLength(
    _In_ PURB Urb
    )
{
    switch (Urb->UrbHeader.Function) {
    case URB_FUNCTION_BULK_OR_INTERRUPT_TRANSFER:
    case URB_FUNCTION_BULK_OR_INTERRUPT_TRANSFER_USING_CHAINED_MDL:
        return Urb->UrbBulkOrInterruptTransfer.TransferBufferLength;
    case URB_FUNCTION_ISOCH_TRANSFER:
    case URB_FUNCTION_ISOCH_TRANSFER_USING_CHAINED_MDL:
        return Urb->UrbIsochronousTransfer.TransferBufferLength;
    case URB_FUNCTION_CONTROL_TRANSFER:
        return Urb->UrbControlTransfer.TransferBufferLength;
    case URB_FUNCTION_CONTROL_TRANSFER_EX:
        return Urb->UrbControlTransferEx.TransferBufferLength;
    default:
        return 0;
    }
}

static
ULONG
ViiperIsoFrameSpan(
    _In_ const VIIPER_UDE_ENDPOINT_CONTEXT *EndpointContext,
    _In_ ULONG PacketCount
    )
{
    UCHAR interval = EndpointContext->Descriptor.bInterval;
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext =
        ViiperGetDeviceContext(EndpointContext->Device);
    ULONGLONG span;

    if (PacketCount == 0 || interval == 0) {
        return PacketCount == 0 ? 1 : PacketCount;
    }
    if (deviceContext->Speed == UdecxUsbHighSpeed ||
        deviceContext->Speed == UdecxUsbSuperSpeed) {
        if (interval > 16) {
            // UdeCx should reject an invalid high-speed descriptor before an
            // URB reaches us. Keep the fallback bounded if it does not.
            return PacketCount;
        }
        // High/SuperSpeed bInterval is an exponent in 125-us microframes,
        // while URB StartFrame is expressed in one-millisecond USB frames.
        span = (ULONGLONG)PacketCount * ((ULONGLONG)1 << (interval - 1));
        span = (span + 7) / 8;
    } else {
        // Windows defines each full-speed IsoPacket entry as one 1-ms frame.
        // bInterval describes the endpoint's polling contract; it must not be
        // multiplied into the URB packet-array span a second time.  In
        // particular, doing so creates holes in the virtual StartFrame clock
        // after the USB stack has already expressed the schedule as one packet
        // entry per frame. Production DS4 audio uses bInterval=1, so this
        // correction preserves its proven cadence while making the generic
        // UdeCx clock obey the Windows full-speed URB contract.
        span = PacketCount;
    }
    if (span == 0) {
        return 1;
    }
    return span > MAXULONG ? MAXULONG : (ULONG)span;
}

static
ULONG
ViiperReserveIsoStartFrame(
    _In_ VIIPER_UDE_ENDPOINT_CONTEXT *EndpointContext,
    _In_ ULONG TransferFlags,
    _In_ ULONG RequestedStartFrame,
    _In_ ULONG PacketCount
    )
{
    LONG64 observed;
    ULONGLONG qpcTimestamp;
    ULONG currentFrame;
    LONG requestedDelta;
    ULONG startFrame;
    ULONG nextFrame;
    ULONG span;

    span = ViiperIsoFrameSpan(EndpointContext, PacketCount);
    currentFrame = (ULONG)(KeQueryInterruptTimePrecise(&qpcTimestamp) / 10000ULL);
    if ((TransferFlags & USBD_START_ISO_TRANSFER_ASAP) == 0) {
        // An explicit URB is valid only in the future 1024-frame window. Do
        // not let a rejected request advance the shared endpoint tail: doing
        // so makes the next valid ASAP URB inherit a silent hole.
        requestedDelta = (LONG)(RequestedStartFrame - currentFrame);
        if (requestedDelta <= 0 ||
            requestedDelta >= USBD_ISO_START_FRAME_RANGE) {
            return RequestedStartFrame;
        }
        for (;;) {
            observed = InterlockedCompareExchange64(
                &EndpointContext->NextIsoStartFrame, 0, 0);
            startFrame = (ULONG)observed;
            if (observed != 0 &&
                (LONG)(startFrame - currentFrame) > 0 &&
                (LONG)(RequestedStartFrame - startFrame) < 0) {
                // This explicit window overlaps a reservation already
                // published for the same endpoint. Leave the tail unchanged;
                // user mode will return USBD_STATUS_BAD_START_FRAME.
                return RequestedStartFrame;
            }
            nextFrame = RequestedStartFrame + span;
            if (InterlockedCompareExchange64(
                    &EndpointContext->NextIsoStartFrame,
                    (LONG64)(ULONGLONG)nextFrame,
                    observed) == observed) {
                return RequestedStartFrame;
            }
        }
    }

    for (;;) {
        observed = InterlockedCompareExchange64(
            &EndpointContext->NextIsoStartFrame, 0, 0);
        startFrame = (ULONG)observed;
        if (observed == 0 || (LONG)(startFrame - currentFrame) <= 0) {
            startFrame = currentFrame + 1;
        }
        nextFrame = startFrame + span;
        if (InterlockedCompareExchange64(
                &EndpointContext->NextIsoStartFrame,
                (LONG64)(ULONGLONG)nextFrame,
                observed) == observed) {
            return startFrame;
        }
    }
}

NTSTATUS
ViiperCopyTransferBuffer(
    _In_ WDFREQUEST Request,
    _In_ PURB Urb,
    _Inout_updates_bytes_(Length) UCHAR *Buffer,
    _In_ ULONG Length,
    _In_ BOOLEAN ToUrb
    )
{
    UCHAR *contiguous = NULL;
    ULONG contiguousLength = 0;
    ULONG transferBufferLength;
    PMDL mdl;
    ULONG copied = 0;
    NTSTATUS status;

    if (Length == 0) {
        return STATUS_SUCCESS;
    }

    transferBufferLength = ViiperGetTransferBufferLength(Urb);
    if (Length > transferBufferLength) {
        return STATUS_BUFFER_TOO_SMALL;
    }

    status = UdecxUrbRetrieveBuffer(Request, &contiguous, &contiguousLength);
    if (NT_SUCCESS(status) && contiguous != NULL && contiguousLength >= Length) {
        // The pointer returned by UdecxUrbRetrieveBuffer is valid for exactly
        // the reported span.  A chained MDL can legitimately expose a first
        // mapped segment that is shorter than the URB's total transfer length;
        // in that case use the MDL walk below instead of copying beyond this
        // mapping.  The URB length remains the transfer contract, but it does
        // not enlarge an individual mapped buffer.
        if (ToUrb) {
            RtlCopyMemory(contiguous, Buffer, Length);
        } else {
            RtlCopyMemory(Buffer, contiguous, Length);
        }
        return STATUS_SUCCESS;
    }

    mdl = ViiperGetTransferMdl(Urb);
    while (mdl != NULL && copied < Length) {
        ULONG mdlLength = MmGetMdlByteCount(mdl);
        ULONG chunk = min(mdlLength, Length - copied);
        UCHAR *mapped;

        if (chunk != 0) {
            mapped = (UCHAR *)MmGetSystemAddressForMdlSafe(
                mdl, (MM_PAGE_PRIORITY)(NormalPagePriority | MdlMappingNoExecute));
            if (mapped == NULL) {
                return STATUS_INSUFFICIENT_RESOURCES;
            }
            if (ToUrb) {
                RtlCopyMemory(mapped, Buffer + copied, chunk);
            } else {
                RtlCopyMemory(Buffer + copied, mapped, chunk);
            }
            copied += chunk;
        }
        mdl = mdl->Next;
    }

    if (copied != Length) {
        return NT_SUCCESS(status) ? STATUS_BUFFER_TOO_SMALL : status;
    }
    return STATUS_SUCCESS;
}

static
NTSTATUS
ViiperGetTransferMetadata(
    _In_ WDFREQUEST Request,
    _In_ PURB Urb,
    _Out_ ULONG *TransferFlags,
    _Out_ ULONG *TransferLength,
    _Out_ ULONG *StartFrame,
    _Out_ ULONG *IsoPacketCount,
    _Out_ BOOLEAN *DirectionIn,
    _Out_writes_bytes_(8) UCHAR SetupPacket[8]
    )
{
    WDF_USB_CONTROL_SETUP_PACKET setup;
    NTSTATUS status;

    *StartFrame = 0;
    *IsoPacketCount = 0;
    RtlZeroMemory(SetupPacket, 8);

    switch (Urb->UrbHeader.Function) {
    case URB_FUNCTION_BULK_OR_INTERRUPT_TRANSFER:
    case URB_FUNCTION_BULK_OR_INTERRUPT_TRANSFER_USING_CHAINED_MDL:
        *TransferFlags = Urb->UrbBulkOrInterruptTransfer.TransferFlags;
        *TransferLength = Urb->UrbBulkOrInterruptTransfer.TransferBufferLength;
        break;
    case URB_FUNCTION_ISOCH_TRANSFER:
    case URB_FUNCTION_ISOCH_TRANSFER_USING_CHAINED_MDL:
        *TransferFlags = Urb->UrbIsochronousTransfer.TransferFlags;
        *TransferLength = Urb->UrbIsochronousTransfer.TransferBufferLength;
        *StartFrame = Urb->UrbIsochronousTransfer.StartFrame;
        *IsoPacketCount = Urb->UrbIsochronousTransfer.NumberOfPackets;
        if (*IsoPacketCount > VIIPER_UDE_MAX_ISO_PACKETS) {
            return STATUS_INVALID_BUFFER_SIZE;
        }
        break;
    case URB_FUNCTION_CONTROL_TRANSFER:
        *TransferFlags = Urb->UrbControlTransfer.TransferFlags;
        *TransferLength = Urb->UrbControlTransfer.TransferBufferLength;
        status = UdecxUrbRetrieveControlSetupPacket(Request, &setup);
        if (!NT_SUCCESS(status)) {
            return status;
        }
        RtlCopyMemory(SetupPacket, &setup, 8);
        break;
    case URB_FUNCTION_CONTROL_TRANSFER_EX:
        *TransferFlags = Urb->UrbControlTransferEx.TransferFlags;
        *TransferLength = Urb->UrbControlTransferEx.TransferBufferLength;
        status = UdecxUrbRetrieveControlSetupPacket(Request, &setup);
        if (!NT_SUCCESS(status)) {
            return status;
        }
        RtlCopyMemory(SetupPacket, &setup, 8);
        break;
    default:
        return STATUS_NOT_SUPPORTED;
    }

    if (*TransferLength > VIIPER_UDE_MAX_TRANSFER_BYTES) {
        return STATUS_INVALID_BUFFER_SIZE;
    }
    *DirectionIn = ((*TransferFlags & USBD_TRANSFER_DIRECTION_IN) != 0);
    if (Urb->UrbHeader.Function == URB_FUNCTION_CONTROL_TRANSFER ||
        Urb->UrbHeader.Function == URB_FUNCTION_CONTROL_TRANSFER_EX) {
        *DirectionIn = ((SetupPacket[0] & USB_ENDPOINT_DIRECTION_MASK) != 0);
    }
    return STATUS_SUCCESS;
}

static
NTSTATUS
ViiperSerializeOperation(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ WDFREQUEST UrbRequest,
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ ULONGLONG Token,
    _In_ WDFREQUEST DequeueRequest,
    _Out_ VIIPER_UDE_OPERATION **SerializedOperation
    )
{
    PURB urb = ViiperGetUrb(UrbRequest);
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_REQUEST_CONTEXT *requestContext = ViiperGetRequestContext(UrbRequest);
    VIIPER_UDE_OPERATION *operation;
    VIIPER_UDE_ISO_PACKET *packets;
    UCHAR *payload;
    ULONG transferFlags;
    ULONG transferLength;
    ULONG startFrame;
    ULONG packetCount;
    ULONG isoBytes;
    ULONG payloadLength;
    ULONG totalLength;
    ULONG index;
    BOOLEAN directionIn;
    UCHAR setupPacket[8];
    NTSTATUS status;

    if (urb == NULL) {
        return STATUS_INVALID_DEVICE_REQUEST;
    }
    status = ViiperGetTransferMetadata(
        UrbRequest, urb, &transferFlags, &transferLength, &startFrame,
        &packetCount, &directionIn, setupPacket);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    if (urb->UrbHeader.Function != URB_FUNCTION_CONTROL_TRANSFER &&
        urb->UrbHeader.Function != URB_FUNCTION_CONTROL_TRANSFER_EX) {
        // Windows can supply stale or inconsistent direction bits in
        // TransferFlags for bulk URBs. The endpoint descriptor is authoritative
        // for every non-control pipe; only a control setup packet owns its
        // direction. Normalize both ABI fields
        // together so user mode never rejects or inverts an otherwise valid
        // media/output transfer.
        directionIn = (endpointContext->Descriptor.bEndpointAddress &
            USB_ENDPOINT_DIRECTION_MASK) != 0;
        if (directionIn) {
            transferFlags |= USBD_TRANSFER_DIRECTION_IN;
        } else {
            transferFlags &= ~USBD_TRANSFER_DIRECTION_IN;
        }
    }
    if (packetCount != 0) {
        startFrame = ViiperReserveIsoStartFrame(
            endpointContext, transferFlags, startFrame, packetCount);
    }

    isoBytes = packetCount * sizeof(VIIPER_UDE_ISO_PACKET);
    payloadLength = directionIn ? 0 : transferLength;
    if (isoBytes > MAXULONG - sizeof(*operation) ||
        payloadLength > MAXULONG - sizeof(*operation) - isoBytes) {
        return STATUS_INTEGER_OVERFLOW;
    }
    totalLength = sizeof(*operation) + isoBytes + payloadLength;
    status = WdfRequestRetrieveOutputBuffer(
        DequeueRequest, totalLength, (PVOID *)&operation, NULL);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    RtlZeroMemory(operation, totalLength);
    operation->Header.Magic = VIIPER_UDE_MAGIC;
    operation->Header.Major = VIIPER_UDE_ABI_MAJOR;
    operation->Header.Minor = VIIPER_UDE_ABI_MINOR;
    operation->Header.Size = totalLength;
    operation->Token = Token;
    operation->DeviceId = deviceContext->DeviceId;
    operation->Generation = deviceContext->Generation;
    operation->Kind = (urb->UrbHeader.Function == URB_FUNCTION_CONTROL_TRANSFER ||
        urb->UrbHeader.Function == URB_FUNCTION_CONTROL_TRANSFER_EX)
        ? ViiperUdeOperationControl : ViiperUdeOperationTransfer;
    operation->EndpointAddress = endpointContext->Descriptor.bEndpointAddress;
    operation->EndpointAttributes = endpointContext->Descriptor.bmAttributes;
    operation->EndpointInterval = endpointContext->Descriptor.bInterval;
    operation->EndpointMaxPacketSize = endpointContext->Descriptor.wMaxPacketSize;
    operation->EndpointGeneration = endpointContext->Generation;
    operation->Direction = directionIn ? 1 : 0;
    operation->UrbFunction = urb->UrbHeader.Function;
    operation->TransferFlags = transferFlags;
    operation->StartFrame = startFrame;
    operation->IsoPacketCount = packetCount;
    operation->TransferLength = transferLength;
    operation->IsoPacketsOffset = sizeof(*operation);
    operation->PayloadOffset = sizeof(*operation) + isoBytes;
    operation->PayloadLength = payloadLength;
    RtlCopyMemory(operation->SetupPacket, setupPacket, sizeof(setupPacket));

    packets = (VIIPER_UDE_ISO_PACKET *)((UCHAR *)operation + operation->IsoPacketsOffset);
    for (index = 0; index < packetCount; ++index) {
        ULONG offset = urb->UrbIsochronousTransfer.IsoPacket[index].Offset;
        ULONG nextOffset = index + 1 < packetCount
            ? urb->UrbIsochronousTransfer.IsoPacket[index + 1].Offset
            : transferLength;
        if (offset > nextOffset || nextOffset > transferLength) {
            return STATUS_INVALID_PARAMETER;
        }
        packets[index].Offset = offset;
        packets[index].Length = nextOffset - offset;
        packets[index].Status = urb->UrbIsochronousTransfer.IsoPacket[index].Status;
    }
    payload = (UCHAR *)operation + operation->PayloadOffset;
    if (payloadLength > 0) {
        status = ViiperCopyTransferBuffer(
            UrbRequest, urb, payload, payloadLength, FALSE);
        if (!NT_SUCCESS(status)) {
            return status;
        }
    }

    requestContext->TransferLength = transferLength;
    requestContext->IsoPacketCount = packetCount;
    requestContext->IsoStartFrame = startFrame;
    requestContext->DirectionIn = directionIn;
    WdfRequestSetInformation(DequeueRequest, totalLength);
    InterlockedIncrement64(&ControllerContext->OperationsDequeued);
    if (!directionIn) {
        InterlockedAdd64(&ControllerContext->BytesToDevice, transferLength);
    }
    *SerializedOperation = operation;
    return STATUS_SUCCESS;
}

static
BOOLEAN
ViiperQueueOwnedCompletion(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ ULONG Slot,
    _In_ WDFREQUEST Request,
    _In_ ULONGLONG Token,
    _In_ NTSTATUS Status,
    _In_ USBD_STATUS UsbdStatus,
    _In_ BOOLEAN CompleteWithNtStatus
    )
{
    BOOLEAN queued = FALSE;
    VIIPER_UDE_REQUEST_CONTEXT *requestContext = ViiperGetRequestContext(Request);
    WDFDEVICE controller = requestContext->Controller;
    UDECXUSBENDPOINT endpoint = requestContext->Endpoint;
    NTSTATUS completionStatus = Status;
    USBD_STATUS completionUsbdStatus = UsbdStatus;
    BOOLEAN completionWithNtStatus = CompleteWithNtStatus;

    WdfSpinLockAcquire(ControllerContext->BrokerLock);
    if (Slot < VIIPER_UDE_MAX_PENDING_OPERATIONS &&
        ViiperSlotMatches(&ControllerContext->PendingSlots[Slot], Request, Token) &&
        ControllerContext->PendingSlots[Slot].State == ViiperUdePendingCompleting) {
        VIIPER_UDE_PENDING_SLOT *pending = &ControllerContext->PendingSlots[Slot];
        if (pending->AbortPending) {
            pending->CompletionStatus = pending->AbortStatus;
            pending->CompletionUsbdStatus = USBD_STATUS_CANCELED;
            pending->CompleteWithNtStatus = TRUE;
        } else {
            pending->CompletionStatus = Status;
            pending->CompletionUsbdStatus = UsbdStatus;
            pending->CompleteWithNtStatus = CompleteWithNtStatus;
        }
        completionStatus = pending->CompletionStatus;
        completionUsbdStatus = pending->CompletionUsbdStatus;
        completionWithNtStatus = pending->CompleteWithNtStatus;
        pending->State = ViiperUdePendingDpcCompletion;
        queued = TRUE;
    }
    WdfSpinLockRelease(ControllerContext->BrokerLock);
    if (queued) {
        queued = ViiperQueueUrbCompletion(
            controller,
            endpoint,
            Request,
            Slot,
            Token,
            completionStatus,
            completionUsbdStatus,
            completionWithNtStatus,
            0,
            0);
    }
    return queued;
}

static
BOOLEAN
ViiperExpectedLateAbortLocked(
    _In_ const VIIPER_UDE_PENDING_SLOT *Pending,
    _In_ ULONGLONG Token
    )
{
    NTSTATUS abortStatus;

    if (Pending->Token != Token ||
        (Pending->State != ViiperUdePendingCompleting &&
            Pending->State != ViiperUdePendingDpcCompletion) ||
        (!Pending->AbortPending && !Pending->CompleteWithNtStatus)) {
        return FALSE;
    }

    abortStatus = Pending->AbortPending
        ? Pending->AbortStatus
        : Pending->CompletionStatus;
    return abortStatus == STATUS_CANCELLED ||
        abortStatus == STATUS_DEVICE_REMOVED ||
        abortStatus == STATUS_DEVICE_NOT_READY ||
        abortStatus == STATUS_FILE_CLOSED;
}

static
VOID
ViiperRemovePublishingRequest(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ ULONG Slot,
    _In_ WDFREQUEST Request,
    _In_ ULONGLONG Token,
    _In_ NTSTATUS Status,
    _In_ BOOLEAN NotifyOwner
    )
{
    BOOLEAN ownsRequest = FALSE;
    BOOLEAN notifyOwner = FALSE;
    VIIPER_UDE_REQUEST_CONTEXT *requestContext = ViiperGetRequestContext(Request);
    WDFDEVICE controller = requestContext->Controller;
    UDECXUSBENDPOINT endpoint = requestContext->Endpoint;

    WdfSpinLockAcquire(ControllerContext->BrokerLock);
    if (Slot < VIIPER_UDE_MAX_PENDING_OPERATIONS &&
        ViiperSlotMatches(&ControllerContext->PendingSlots[Slot], Request, Token)) {
        if (NotifyOwner) {
            notifyOwner = ViiperQueueCancelEventLocked(
                ControllerContext, &ControllerContext->PendingSlots[Slot]);
        }
        ControllerContext->PendingSlots[Slot].CompletionStatus = Status;
        ControllerContext->PendingSlots[Slot].CompletionUsbdStatus = USBD_STATUS_CANCELED;
        ControllerContext->PendingSlots[Slot].CompleteWithNtStatus = TRUE;
        ControllerContext->PendingSlots[Slot].State = ViiperUdePendingDpcCompletion;
        ViiperUnlinkAdmissionLocked(&ControllerContext->PendingSlots[Slot]);
        ownsRequest = TRUE;
    }
    WdfSpinLockRelease(ControllerContext->BrokerLock);
    if (ownsRequest) {
        (VOID)ViiperQueueUrbCompletion(
            controller,
            endpoint,
            Request,
            Slot,
            Token,
            Status,
            USBD_STATUS_CANCELED,
            TRUE,
            0,
            0);
        if (notifyOwner) {
            ViiperDispatchNotificationEvents(controller);
        }
    }
}

static
VOID
ViiperDispatchAvailable(
    _In_ WDFDEVICE Controller
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(Controller);

    for (;;) {
        WDFREQUEST urbRequest = WDF_NO_HANDLE;
        WDFREQUEST dequeueRequest = WDF_NO_HANDLE;
        UDECXUSBENDPOINT endpoint = WDF_NO_HANDLE;
        VIIPER_UDE_OPERATION *serializedOperation = NULL;
        ULONGLONG token = 0;
        ULONG slot = VIIPER_UDE_MAX_PENDING_OPERATIONS;
        ULONG index;
        NTSTATUS status;
        BOOLEAN abortPending = FALSE;
        BOOLEAN cancelClaimed = FALSE;
        NTSTATUS abortStatus = STATUS_CANCELLED;

        if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0) {
            break;
        }
        ViiperDispatchNotificationEvents(Controller);
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0) {
            WdfSpinLockRelease(controllerContext->BrokerLock);
            break;
        }
        // Once lifecycle notification loss faults the owner session, only the
        // notification FIFO may drain. Publishing another control/media URB
        // would cross a reset or power boundary which user mode can no longer
        // reconstruct.
        if (InterlockedCompareExchange(
                &controllerContext->BrokerFaulted, FALSE, FALSE) != FALSE) {
            WdfSpinLockRelease(controllerContext->BrokerLock);
            break;
        }
        for (index = 0; index < VIIPER_UDE_MAX_PENDING_OPERATIONS; ++index) {
            ULONG candidate = (controllerContext->NextDispatchSlot + index) %
                VIIPER_UDE_MAX_PENDING_OPERATIONS;
            VIIPER_UDE_PENDING_SLOT *pending = &controllerContext->PendingSlots[candidate];
            if (pending->State == ViiperUdePendingQueued &&
                ViiperAdmissionCanPublishLocked(pending)) {
                status = WdfIoQueueRetrieveNextRequest(
                    controllerContext->WaitingDequeues, &dequeueRequest);
                if (!NT_SUCCESS(status)) {
                    dequeueRequest = WDF_NO_HANDLE;
                    break;
                }
                pending->State = ViiperUdePendingPublishing;
                urbRequest = pending->Request;
                endpoint = pending->Endpoint;
                token = pending->Token;
                slot = candidate;
                controllerContext->NextDispatchSlot = (candidate + 1) %
                    VIIPER_UDE_MAX_PENDING_OPERATIONS;
                WdfObjectReference(urbRequest);
                InterlockedDecrement(&controllerContext->WaitingDequeueCount);
                break;
            }
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);

        if (urbRequest == WDF_NO_HANDLE || dequeueRequest == WDF_NO_HANDLE) {
            break;
        }

        status = WdfRequestUnmarkCancelable(urbRequest);
        if (status == STATUS_CANCELLED) {
            WdfRequestComplete(dequeueRequest, STATUS_CANCELLED);
            WdfObjectDereference(urbRequest);
            continue;
        }
        if (!NT_SUCCESS(status)) {
            ViiperRemovePublishingRequest(
                controllerContext, slot, urbRequest, token, status, FALSE);
            WdfRequestComplete(dequeueRequest, status);
            WdfObjectDereference(urbRequest);
            continue;
        }

        status = ViiperSerializeOperation(
            controllerContext, urbRequest, endpoint, token, dequeueRequest,
            &serializedOperation);

        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (slot < VIIPER_UDE_MAX_PENDING_OPERATIONS &&
            ViiperSlotMatches(&controllerContext->PendingSlots[slot], urbRequest, token)) {
            VIIPER_UDE_PENDING_SLOT *pending = &controllerContext->PendingSlots[slot];
            abortPending = pending->AbortPending;
            abortStatus = pending->AbortStatus;
            if (pending->DeviceGeneration !=
                    ViiperGetRequestContext(urbRequest)->DeviceGeneration ||
                pending->EndpointGeneration !=
                    ViiperGetRequestContext(urbRequest)->EndpointGeneration ||
                pending->EndpointGeneration !=
                    ViiperGetEndpointContext(endpoint)->Generation) {
                status = STATUS_DEVICE_NOT_READY;
            }
        } else {
            status = STATUS_CANCELLED;
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);

        if (!NT_SUCCESS(status) || abortPending) {
            NTSTATUS completionStatus = abortPending ? abortStatus : status;
            ViiperRemovePublishingRequest(
                controllerContext, slot, urbRequest, token, completionStatus, FALSE);
            WdfRequestComplete(dequeueRequest, completionStatus);
            WdfObjectDereference(urbRequest);
            continue;
        }

        status = WdfRequestMarkCancelableEx(urbRequest, ViiperEvtUrbCancel);
        if (!NT_SUCCESS(status)) {
            ViiperRemovePublishingRequest(
                controllerContext, slot, urbRequest, token, STATUS_CANCELLED, FALSE);
            WdfRequestComplete(dequeueRequest, STATUS_CANCELLED);
            WdfObjectDereference(urbRequest);
            continue;
        }

        abortPending = FALSE;
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (slot < VIIPER_UDE_MAX_PENDING_OPERATIONS &&
            ViiperSlotMatches(&controllerContext->PendingSlots[slot], urbRequest, token)) {
            VIIPER_UDE_PENDING_SLOT *pending = &controllerContext->PendingSlots[slot];
            if (pending->State != ViiperUdePendingPublishing) {
                // MarkCancelableEx may invoke the cancel callback before this
                // thread reacquires BrokerLock. That callback owns the URB and
                // its completion state must never be resurrected here.
                cancelClaimed = TRUE;
            } else {
                abortPending = pending->AbortPending;
                abortStatus = pending->AbortStatus;
                if (pending->DeviceGeneration !=
                        ViiperGetRequestContext(urbRequest)->DeviceGeneration ||
                    pending->EndpointGeneration !=
                        ViiperGetRequestContext(urbRequest)->EndpointGeneration ||
                    pending->EndpointGeneration !=
                        ViiperGetEndpointContext(endpoint)->Generation) {
                    abortPending = TRUE;
                    abortStatus = STATUS_DEVICE_NOT_READY;
                }
                pending->State = abortPending
                    ? ViiperUdePendingCompleting
                    : ViiperUdePendingInFlight;
                // Publication or terminal abort retires the FIFO head. The
                // next same-endpoint admission may now be selected without a
                // controller-wide slot scan.
                ViiperUnlinkAdmissionLocked(pending);
                if (!abortPending) {
                    serializedOperation->EndpointSequence =
                        (ULONGLONG)InterlockedIncrement64(
                            &ViiperGetDeviceContext(
                                ViiperGetEndpointContext(endpoint)->Device)->EndpointSequences[
                                    ViiperGetEndpointContext(endpoint)->Descriptor.bEndpointAddress]);
                    serializedOperation->DeviceSequence =
                        (ULONGLONG)InterlockedIncrement64(
                            &ViiperGetDeviceContext(
                                ViiperGetEndpointContext(endpoint)->Device)->DeviceSequence);
                    pending->PublishedToOwner = TRUE;
                }
            }
        } else {
            cancelClaimed = TRUE;
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);
        if (cancelClaimed) {
            WdfRequestComplete(dequeueRequest, STATUS_CANCELLED);
            WdfObjectDereference(urbRequest);
            continue;
        }
        if (!NT_SUCCESS(status) || abortPending) {
            NTSTATUS completionStatus = abortPending ? abortStatus : STATUS_CANCELLED;
            NTSTATUS unmarkStatus = WdfRequestUnmarkCancelable(urbRequest);
            if (NT_SUCCESS(unmarkStatus)) {
                ViiperRemovePublishingRequest(
                    controllerContext, slot, urbRequest, token, completionStatus, FALSE);
            }
            WdfRequestComplete(dequeueRequest, completionStatus);
            WdfObjectDereference(urbRequest);
            continue;
        }

        WdfRequestComplete(dequeueRequest, STATUS_SUCCESS);
        WdfObjectDereference(urbRequest);
    }
}

NTSTATUS
ViiperQueueDequeueOperation(
    _In_ WDFQUEUE Queue,
    _In_ WDFREQUEST Request
    )
{
    WDFDEVICE controller = WdfIoQueueGetDevice(Queue);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(controller);
    WDFFILEOBJECT fileObject = WdfRequestGetFileObject(Request);
    VIIPER_UDE_FILE_CONTEXT *fileContext;
    NTSTATUS status = STATUS_SUCCESS;

    if (fileObject == WDF_NO_HANDLE) {
        return STATUS_INVALID_HANDLE;
    }
    fileContext = ViiperGetFileContext(fileObject);

    // File cleanup closes admission and purges WaitingDequeues while holding
    // OwnerLock. Keep validation, accounting, and the manual-queue handoff in
    // that same ownership transaction so a request cannot be forwarded after
    // cleanup has already finished purging the queue.
    WdfWaitLockAcquire(controllerContext->OwnerLock, NULL);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0 ||
        controllerContext->OwnerFile != fileObject ||
        controllerContext->CleanupInProgress ||
        InterlockedCompareExchange(&fileContext->Negotiated, 0, 0) == 0 ||
        InterlockedCompareExchange(&fileContext->Closing, 0, 0) != 0) {
        status = STATUS_INVALID_DEVICE_STATE;
    } else if (InterlockedCompareExchange(
            &controllerContext->BrokerFaulted, FALSE, FALSE) != FALSE) {
        status = STATUS_DATA_ERROR;
    } else {
        InterlockedIncrement(&controllerContext->WaitingDequeueCount);
        status = WdfRequestForwardToIoQueue(Request, controllerContext->WaitingDequeues);
        if (!NT_SUCCESS(status)) {
            InterlockedDecrement(&controllerContext->WaitingDequeueCount);
        }
    }
    WdfWaitLockRelease(controllerContext->OwnerLock);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    ViiperDispatchAvailable(controller);
    return STATUS_PENDING;
}

NTSTATUS
ViiperQueueUrb(
    _In_ WDFQUEUE Queue,
    _In_ WDFREQUEST Request
    )
{
    UDECXUSBENDPOINT endpoint = *ViiperGetQueueEndpoint(Queue);
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    VIIPER_UDE_REQUEST_CONTEXT *requestContext = ViiperGetRequestContext(Request);
    ULONG slot;
    ULONGLONG token;
    NTSTATUS status;
    BOOLEAN abortPending = FALSE;
    BOOLEAN cancelClaimed = FALSE;
    BOOLEAN queueCancelledCompletion = FALSE;
    NTSTATUS abortStatus = STATUS_CANCELLED;

    RtlZeroMemory(requestContext, sizeof(*requestContext));
    requestContext->Controller = deviceContext->Controller;
    requestContext->Endpoint = endpoint;
    requestContext->PendingSlot = VIIPER_UDE_MAX_PENDING_OPERATIONS;
    requestContext->DeviceGeneration = deviceContext->Generation;
    requestContext->EndpointGeneration = endpointContext->Generation;
    // KMDF has already delivered this UdeCx request to the driver. Enter
    // endpoint rundown and decide whether it may reach the broker in the same
    // BrokerLock transaction. A request delivered immediately before PURGE,
    // reset, D0 exit, or controller shutdown still owns its mandatory terminal
    // DPC, but it must never allocate or publish a broker slot after that
    // boundary.
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    ViiperEndpointOperationStarted(endpoint);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0 ||
        InterlockedCompareExchange(&controllerContext->BrokerFaulted, FALSE, FALSE) != FALSE ||
        InterlockedCompareExchange(&deviceContext->InD0, 0, 0) == 0 ||
        InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) != 0 ||
        InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0 ||
        InterlockedCompareExchange(&endpointContext->Resetting, 0, 0) != 0 ||
        InterlockedCompareExchange(&endpointContext->Purging, 0, 0) != 0) {
        status = STATUS_DEVICE_NOT_READY;
    } else {
        status = STATUS_SUCCESS;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    status = ViiperAllocatePendingSlot(
        controllerContext, Request, endpoint, &slot, &token);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    requestContext->PendingSlot = slot;
    requestContext->Token = token;

    status = WdfRequestMarkCancelableEx(Request, ViiperEvtUrbCancel);
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (slot < VIIPER_UDE_MAX_PENDING_OPERATIONS &&
        ViiperSlotMatches(&controllerContext->PendingSlots[slot], Request, token)) {
        if (NT_SUCCESS(status)) {
            VIIPER_UDE_PENDING_SLOT *pending = &controllerContext->PendingSlots[slot];
            if (pending->State != ViiperUdePendingPreparing) {
                // An immediate cancel callback already moved this slot to its
                // DPC completion state and owns the request.
                cancelClaimed = TRUE;
            } else {
                abortPending = pending->AbortPending;
                abortStatus = pending->AbortStatus;
                pending->State = abortPending
                    ? ViiperUdePendingCompleting
                    : ViiperUdePendingQueued;
                if (abortPending) {
                    ViiperUnlinkAdmissionLocked(pending);
                }
            }
        } else {
            VIIPER_UDE_PENDING_SLOT *pending = &controllerContext->PendingSlots[slot];
            pending->CompletionStatus = STATUS_CANCELLED;
            pending->CompletionUsbdStatus = USBD_STATUS_CANCELED;
            pending->CompleteWithNtStatus = TRUE;
            pending->State = ViiperUdePendingDpcCompletion;
            ViiperUnlinkAdmissionLocked(pending);
            queueCancelledCompletion = TRUE;
        }
    } else {
        // Only the cancel callback/DPC can retire this just-allocated identity
        // before the mark handoff reacquires BrokerLock.
        cancelClaimed = TRUE;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!NT_SUCCESS(status)) {
        if (queueCancelledCompletion) {
            (VOID)ViiperQueueUrbCompletion(
                deviceContext->Controller,
                endpoint,
                Request,
                slot,
                token,
                STATUS_CANCELLED,
                USBD_STATUS_CANCELED,
                TRUE,
                0,
                0);
            InterlockedIncrement64(&controllerContext->OperationsCancelled);
            // MarkCancelableEx can reject a request before it ever reaches
            // dispatch.  Retiring that admission exposes the next endpoint
            // head, so consume any dequeue that was already waiting.
            ViiperDispatchAvailable(deviceContext->Controller);
        } else if (!cancelClaimed) {
            NT_ASSERT(FALSE);
        }
        return STATUS_PENDING;
    }
    if (cancelClaimed) {
        return STATUS_PENDING;
    }
    if (abortPending) {
        status = WdfRequestUnmarkCancelable(Request);
        if (NT_SUCCESS(status)) {
            ViiperRemovePublishingRequest(
                controllerContext, slot, Request, token, abortStatus, FALSE);
        }
        return STATUS_PENDING;
    }

    ViiperDispatchAvailable(deviceContext->Controller);
    return STATUS_PENDING;
}

static
BOOLEAN
ViiperRangeValid(
    _In_ ULONG Offset,
    _In_ ULONG Length,
    _In_ ULONG Total
    )
{
    return Offset <= Total && Length <= Total - Offset;
}

static
NTSTATUS
ViiperCompleteManagementOperation(
    _In_ WDFDEVICE Controller,
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ const VIIPER_UDE_COMPLETION *Completion
    )
{
    ULONG encodedSlot = (ULONG)Completion->Token;
    ULONG slot = (encodedSlot & ~VIIPER_UDE_MANAGEMENT_SLOT_FLAG) - 1;
    WDFREQUEST request = WDF_NO_HANDLE;
    UDECXUSBDEVICE device = WDF_NO_HANDLE;
    UDECXUSBENDPOINT endpoint = WDF_NO_HANDLE;
    UDECXUSBDEVICE deviceReference = WDF_NO_HANDLE;
    UDECXUSBENDPOINT endpointReference = WDF_NO_HANDLE;
    ULONG kind = 0;
    UCHAR endpointAddress = 0;
    ULONGLONG resetEpoch = 0;
    BOOLEAN resetReleased = TRUE;
    BOOLEAN retiredCompletion = FALSE;

    if ((encodedSlot & VIIPER_UDE_MANAGEMENT_SLOT_FLAG) == 0 ||
        slot >= VIIPER_UDE_MAX_PENDING_MANAGEMENT ||
        Completion->TransferLength != 0 || Completion->IsoPacketCount != 0 ||
        Completion->PayloadLength != 0 || Completion->UsbdStatus != 0 ||
        (NTSTATUS)Completion->Status == STATUS_PENDING) {
        InterlockedIncrement64(&ControllerContext->InvalidMessages);
        return STATUS_INVALID_PARAMETER;
    }

    WdfSpinLockAcquire(ControllerContext->BrokerLock);
    if (ControllerContext->ManagementSlots[slot].Token == Completion->Token &&
        ControllerContext->ManagementSlots[slot].State == ViiperUdePendingInFlight &&
        ControllerContext->ManagementSlots[slot].DeviceId == Completion->DeviceId &&
        ControllerContext->ManagementSlots[slot].DeviceGeneration == Completion->Generation &&
        ControllerContext->ManagementSlots[slot].EndpointGeneration ==
            Completion->EndpointGeneration) {
        request = ControllerContext->ManagementSlots[slot].Request;
        device = ControllerContext->ManagementSlots[slot].Device;
        endpoint = ControllerContext->ManagementSlots[slot].Endpoint;
        resetEpoch = ControllerContext->ManagementSlots[slot].ResetEpoch;
        kind = ControllerContext->ManagementSlots[slot].Kind;
        endpointAddress = ControllerContext->ManagementSlots[slot].EndpointAddress;
        ControllerContext->ManagementSlots[slot].State = ViiperUdePendingCompleting;
        WdfObjectReference(request);
    } else if (!ControllerContext->ManagementSlots[slot].RetiredNotificationPending &&
        ControllerContext->ManagementSlots[slot].RetiredToken ==
            Completion->Token &&
        ControllerContext->ManagementSlots[slot].RetiredDeviceId ==
            Completion->DeviceId &&
        ControllerContext->ManagementSlots[slot].RetiredDeviceGeneration ==
            Completion->Generation &&
        ControllerContext->ManagementSlots[slot].RetiredEndpointGeneration ==
            Completion->EndpointGeneration) {
        // The corresponding request and WDF-object pins were synchronously
        // retired by child teardown after this token crossed to user mode.
        // Consume the tombstone as a harmless expected-late ACK.
        ControllerContext->ManagementSlots[slot].RetiredToken = 0;
        ControllerContext->ManagementSlots[slot].RetiredDeviceId = 0;
        ControllerContext->ManagementSlots[slot].RetiredDeviceGeneration = 0;
        ControllerContext->ManagementSlots[slot].RetiredEndpointGeneration = 0;
        ControllerContext->ManagementSlots[slot].RetiredOwnerFile = WDF_NO_HANDLE;
        retiredCompletion = TRUE;
    }
    WdfSpinLockRelease(ControllerContext->BrokerLock);
    if (request == WDF_NO_HANDLE) {
        InterlockedIncrement64(&ControllerContext->LateCompletions);
        return retiredCompletion ? STATUS_SUCCESS : STATUS_NOT_FOUND;
    }

    if (kind == ViiperUdeOperationDeviceReset) {
        // Endpoint RESET is asynchronous: UdeCx cannot resume endpoint I/O
        // until this reset Request is completed. Repeat the read-only queue /
        // rundown proof for the exact device generation at owner ack so even
        // a terminal callback admitted after initial reset publication is
        // joined. Reopen kernel admission immediately before completing the
        // UdeCx request so a synchronously resumed URB sees post-reset state.
        resetReleased = ViiperQuiesceResetByIdentity(
            Controller,
            Completion->DeviceId,
            Completion->Generation,
            device,
            WDF_NO_HANDLE,
            0,
            resetEpoch,
            0,
            TRUE,
            TRUE);
    } else if (kind == ViiperUdeOperationEndpointReset) {
        // Endpoint reset is a distinct UdeCx boundary, not a purge/start
        // cycle. The second exact-generation proof closes the delivered-
        // before-rundown window without changing UdeCx-owned queue state.
        // Reopen only this endpoint immediately before completing the reset
        // request; completion is the boundary at which UdeCx may resume I/O.
        resetReleased = ViiperQuiesceResetByIdentity(
            Controller,
            Completion->DeviceId,
            Completion->Generation,
            device,
            endpoint,
            Completion->EndpointGeneration,
            resetEpoch,
            endpointAddress,
            FALSE,
            TRUE);
    }
    if (!resetReleased) {
        // Removal or identity reuse won after this management request was
        // published. Never apply an acknowledgement to a different child and
        // never reopen a missing endpoint. Fail the held UdeCx reset request,
        // then retire this completing slot here so removal cannot strand it.
        WdfRequestComplete(request, STATUS_DEVICE_NOT_READY);
        WdfSpinLockAcquire(ControllerContext->BrokerLock);
        if (ControllerContext->ManagementSlots[slot].Request == request &&
            ControllerContext->ManagementSlots[slot].Token == Completion->Token &&
            ControllerContext->ManagementSlots[slot].State == ViiperUdePendingCompleting) {
            ViiperClearManagementSlotLocked(
                ControllerContext, slot, &deviceReference, &endpointReference);
        }
        WdfSpinLockRelease(ControllerContext->BrokerLock);
        ViiperReleaseManagementSlotReferences(deviceReference, endpointReference);
        WdfObjectDereference(request);
        InterlockedIncrement64(&ControllerContext->OperationsPurged);
        return STATUS_DEVICE_NOT_READY;
    }
    WdfRequestComplete(request, (NTSTATUS)Completion->Status);
    WdfSpinLockAcquire(ControllerContext->BrokerLock);
    if (ControllerContext->ManagementSlots[slot].Request == request &&
        ControllerContext->ManagementSlots[slot].Token == Completion->Token &&
        ControllerContext->ManagementSlots[slot].State == ViiperUdePendingCompleting) {
        ViiperClearManagementSlotLocked(
            ControllerContext, slot, &deviceReference, &endpointReference);
    }
    WdfSpinLockRelease(ControllerContext->BrokerLock);
    ViiperReleaseManagementSlotReferences(deviceReference, endpointReference);
    WdfObjectDereference(request);
    InterlockedIncrement64(&ControllerContext->OperationsCompleted);
    return STATUS_SUCCESS;
}

NTSTATUS
ViiperCompleteOperation(
    _In_ WDFQUEUE Queue,
    _In_ WDFREQUEST CompletionRequest
    )
{
    WDFDEVICE controller = WdfIoQueueGetDevice(Queue);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(controller);
    VIIPER_UDE_COMPLETION *completion;
    VIIPER_UDE_REQUEST_CONTEXT *requestContext;
    VIIPER_UDE_ISO_PACKET *packets = NULL;
    UCHAR *tail = NULL;
    UCHAR *payload = NULL;
    WDFREQUEST urbRequest = WDF_NO_HANDLE;
    PURB urb;
    size_t inputLength;
    size_t tailLength = 0;
    ULONG slot;
    ULONG index;
    ULONG isoPayloadLimit;
    ULONG isoBytes;
    ULONG expectedSize;
    ULONG packetTotal = 0;
    ULONG isoErrorCount = 0;
    NTSTATUS status;
    BOOLEAN expectedLateAbort = FALSE;
    BOOLEAN identityMismatch = FALSE;
    BOOLEAN queued;

    status = ViiperValidateBrokerOwner(controller, CompletionRequest);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    status = WdfRequestRetrieveInputBuffer(
        CompletionRequest, sizeof(*completion), (PVOID *)&completion, &inputLength);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    if (inputLength != sizeof(*completion) || completion->Header.Magic != VIIPER_UDE_MAGIC ||
        completion->Header.Major != VIIPER_UDE_ABI_MAJOR ||
        completion->Header.Minor != VIIPER_UDE_ABI_MINOR ||
        completion->Header.Flags != 0 ||
        completion->Header.Size < sizeof(*completion) || completion->Token == 0 ||
        completion->DeviceId == 0 || completion->Generation == 0 ||
        completion->TransferLength > VIIPER_UDE_MAX_TRANSFER_BYTES ||
        completion->PayloadLength > VIIPER_UDE_MAX_TRANSFER_BYTES ||
        completion->IsoPacketCount > VIIPER_UDE_MAX_ISO_PACKETS ||
        (((ULONG)completion->Token & VIIPER_UDE_MANAGEMENT_SLOT_FLAG) == 0 &&
         completion->EndpointGeneration == 0) ||
        completion->Reserved != 0) {
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        return STATUS_INVALID_PARAMETER;
    }
    isoBytes = completion->IsoPacketCount * sizeof(VIIPER_UDE_ISO_PACKET);
    if (completion->PayloadLength > MAXULONG - sizeof(*completion) - isoBytes) {
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        return STATUS_INTEGER_OVERFLOW;
    }
    expectedSize = sizeof(*completion) + isoBytes + completion->PayloadLength;
    if (completion->Header.Size != expectedSize ||
        completion->IsoPacketsOffset != sizeof(*completion) ||
        completion->PayloadOffset != sizeof(*completion) + isoBytes) {
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        return STATUS_INVALID_PARAMETER;
    }
    tailLength = completion->Header.Size - sizeof(*completion);
    if (tailLength > 0) {
        status = WdfRequestRetrieveOutputBuffer(
            CompletionRequest, tailLength, (PVOID *)&tail, NULL);
        if (!NT_SUCCESS(status)) {
            return status;
        }
    }
    if (!ViiperRangeValid(
            completion->IsoPacketsOffset,
            isoBytes,
            completion->Header.Size) ||
        !ViiperRangeValid(
            completion->PayloadOffset, completion->PayloadLength, completion->Header.Size) ||
        (completion->IsoPacketCount != 0 && completion->IsoPacketsOffset < sizeof(*completion)) ||
        (completion->PayloadLength != 0 && completion->PayloadOffset < sizeof(*completion))) {
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        return STATUS_INVALID_PARAMETER;
    }
    if (completion->IsoPacketCount != 0) {
        packets = (VIIPER_UDE_ISO_PACKET *)(
            tail + completion->IsoPacketsOffset - sizeof(*completion));
    }
    if (completion->PayloadLength != 0) {
        payload = tail + completion->PayloadOffset - sizeof(*completion);
    }
    if (((ULONG)completion->Token & VIIPER_UDE_MANAGEMENT_SLOT_FLAG) != 0) {
        return ViiperCompleteManagementOperation(controller, controllerContext, completion);
    }

    slot = (ULONG)(completion->Token & MAXULONG);
    if (slot == 0 || slot > VIIPER_UDE_MAX_PENDING_OPERATIONS) {
        InterlockedIncrement64(&controllerContext->LateCompletions);
        return STATUS_NOT_FOUND;
    }
    --slot;
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (controllerContext->PendingSlots[slot].Token == completion->Token &&
        controllerContext->PendingSlots[slot].State == ViiperUdePendingInFlight) {
        if (controllerContext->PendingSlots[slot].DeviceId == completion->DeviceId &&
            controllerContext->PendingSlots[slot].DeviceGeneration == completion->Generation &&
            controllerContext->PendingSlots[slot].EndpointGeneration ==
                completion->EndpointGeneration) {
            urbRequest = controllerContext->PendingSlots[slot].Request;
            controllerContext->PendingSlots[slot].State = ViiperUdePendingCompleting;
            WdfObjectReference(urbRequest);
        } else {
            // A correct token with conflicting immutable identity must not
            // claim the request. The owner will fault on this reply and file
            // cleanup remains the authoritative retirement path.
            identityMismatch = TRUE;
        }
    } else {
        expectedLateAbort = ViiperExpectedLateAbortLocked(
            &controllerContext->PendingSlots[slot], completion->Token);
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (identityMismatch) {
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        return STATUS_INVALID_PARAMETER;
    }
    if (urbRequest == WDF_NO_HANDLE) {
        InterlockedIncrement64(&controllerContext->LateCompletions);
        return expectedLateAbort ? STATUS_SUCCESS : STATUS_NOT_FOUND;
    }

    status = WdfRequestUnmarkCancelable(urbRequest);
    if (!NT_SUCCESS(status)) {
        WdfObjectDereference(urbRequest);
        InterlockedIncrement64(&controllerContext->LateCompletions);
        return status;
    }

    requestContext = ViiperGetRequestContext(urbRequest);
    urb = ViiperGetUrb(urbRequest);
    if (urb == NULL || completion->DeviceId !=
            ViiperGetDeviceContext(ViiperGetEndpointContext(requestContext->Endpoint)->Device)->DeviceId ||
        completion->Generation !=
            ViiperGetDeviceContext(ViiperGetEndpointContext(requestContext->Endpoint)->Device)->Generation ||
        completion->Generation != requestContext->DeviceGeneration ||
        completion->EndpointGeneration != requestContext->EndpointGeneration ||
        completion->EndpointGeneration !=
            ViiperGetEndpointContext(requestContext->Endpoint)->Generation ||
        completion->TransferLength > requestContext->TransferLength) {
        status = STATUS_INVALID_PARAMETER;
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        goto CompleteWithNtStatus;
    }
    if (!NT_SUCCESS((NTSTATUS)completion->Status)) {
        status = (NTSTATUS)completion->Status;
        queued = ViiperQueueOwnedCompletion(
            controllerContext,
            slot,
            urbRequest,
            completion->Token,
            status,
            USBD_STATUS_INTERNAL_HC_ERROR,
            TRUE);
        WdfObjectDereference(urbRequest);
        if (!queued) {
            InterlockedIncrement64(&controllerContext->LateCompletions);
            return STATUS_NOT_FOUND;
        }
        InterlockedIncrement64(&controllerContext->OperationsCompleted);
        return STATUS_SUCCESS;
    }

    if (completion->IsoPacketCount != requestContext->IsoPacketCount ||
        (completion->IsoPacketCount == 0 && requestContext->DirectionIn &&
            completion->PayloadLength != completion->TransferLength) ||
        (completion->IsoPacketCount == 0 && !requestContext->DirectionIn &&
            completion->PayloadLength != 0) ||
        (completion->IsoPacketCount != 0 && requestContext->DirectionIn &&
            completion->PayloadLength > requestContext->TransferLength) ||
        (completion->IsoPacketCount != 0 && !requestContext->DirectionIn &&
            completion->PayloadLength != 0)) {
        status = STATUS_INVALID_PARAMETER;
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        goto CompleteWithNtStatus;
    }

    if (requestContext->DirectionIn && completion->PayloadLength > 0) {
        status = ViiperCopyTransferBuffer(
            urbRequest, urb, payload, completion->PayloadLength, TRUE);
        if (!NT_SUCCESS(status)) {
            goto CompleteWithNtStatus;
        }
        InterlockedAdd64(&controllerContext->BytesFromDevice, completion->TransferLength);
    }
    if (completion->IsoPacketCount != 0) {
        isoPayloadLimit = requestContext->DirectionIn
            ? completion->PayloadLength
            : requestContext->TransferLength;
        for (index = 0; index < completion->IsoPacketCount; ++index) {
            ULONG originalOffset = urb->UrbIsochronousTransfer.IsoPacket[index].Offset;
            ULONG nextOriginalOffset = index + 1 < completion->IsoPacketCount
                ? urb->UrbIsochronousTransfer.IsoPacket[index + 1].Offset
                : requestContext->TransferLength;

            if (packets[index].Reserved != 0 ||
                originalOffset > nextOriginalOffset ||
                nextOriginalOffset > requestContext->TransferLength ||
                packets[index].Offset != originalOffset ||
                packets[index].Offset > isoPayloadLimit ||
                packets[index].Length > isoPayloadLimit - packets[index].Offset ||
                packets[index].Length > nextOriginalOffset - originalOffset ||
                packets[index].Length > MAXULONG - packetTotal) {
                status = STATUS_INVALID_PARAMETER;
                InterlockedIncrement64(&controllerContext->InvalidMessages);
                goto CompleteWithNtStatus;
            }
            packetTotal += packets[index].Length;
            urb->UrbIsochronousTransfer.IsoPacket[index].Length = packets[index].Length;
            urb->UrbIsochronousTransfer.IsoPacket[index].Status = packets[index].Status;
            if ((USBD_STATUS)packets[index].Status != USBD_STATUS_SUCCESS) {
                ++isoErrorCount;
            }
        }
        if (packetTotal != completion->TransferLength) {
            status = STATUS_INVALID_PARAMETER;
            InterlockedIncrement64(&controllerContext->InvalidMessages);
            goto CompleteWithNtStatus;
        }
        urb->UrbIsochronousTransfer.ErrorCount = isoErrorCount;
        if ((urb->UrbIsochronousTransfer.TransferFlags &
                USBD_START_ISO_TRANSFER_ASAP) != 0) {
            urb->UrbIsochronousTransfer.StartFrame = requestContext->IsoStartFrame;
        }
        InterlockedAdd64(&controllerContext->IsoPackets, completion->IsoPacketCount);
    }

    UdecxUrbSetBytesCompleted(urbRequest, completion->TransferLength);
    queued = ViiperQueueOwnedCompletion(
        controllerContext,
        slot,
        urbRequest,
        completion->Token,
        STATUS_SUCCESS,
        (USBD_STATUS)completion->UsbdStatus,
        FALSE);
    WdfObjectDereference(urbRequest);
    if (!queued) {
        InterlockedIncrement64(&controllerContext->LateCompletions);
        return STATUS_NOT_FOUND;
    }
    InterlockedIncrement64(&controllerContext->OperationsCompleted);
    return STATUS_SUCCESS;

CompleteWithNtStatus:
    queued = ViiperQueueOwnedCompletion(
        controllerContext,
        slot,
        urbRequest,
        completion->Token,
        status,
        USBD_STATUS_INTERNAL_HC_ERROR,
        TRUE);
    WdfObjectDereference(urbRequest);
    if (!queued) {
        InterlockedIncrement64(&controllerContext->LateCompletions);
        return STATUS_NOT_FOUND;
    }
    return status;
}

static
VOID
ViiperAbortMatchingOperations(
    _In_ WDFDEVICE Controller,
    _In_opt_ UDECXUSBENDPOINT Endpoint,
    _In_ NTSTATUS Status
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(Controller);
    ULONG index;

    if (controllerContext->BrokerLock == WDF_NO_HANDLE ||
        controllerContext->PendingSlots == NULL) {
        return;
    }

    for (index = 0; index < VIIPER_UDE_MAX_PENDING_OPERATIONS; ++index) {
        WDFREQUEST request = WDF_NO_HANDLE;
        ULONGLONG token = 0;
        NTSTATUS unmarkStatus;

        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (controllerContext->PendingSlots[index].State != ViiperUdePendingEmpty &&
            (Endpoint == WDF_NO_HANDLE || controllerContext->PendingSlots[index].Endpoint == Endpoint)) {
            VIIPER_UDE_PENDING_SLOT *pending = &controllerContext->PendingSlots[index];
            if (pending->State == ViiperUdePendingPublishing) {
                pending->AbortPending = TRUE;
                pending->AbortStatus = Status;
            } else if (pending->State == ViiperUdePendingDpcCompletion) {
                /* The request is already owned by the completion DPC. */
            } else if (pending->State != ViiperUdePendingPreparing &&
                pending->State != ViiperUdePendingCompleting) {
                request = pending->Request;
                token = pending->Token;
                pending->AbortPending = TRUE;
                pending->AbortStatus = Status;
                pending->State = ViiperUdePendingCompleting;
                WdfObjectReference(request);
            } else {
                pending->AbortPending = TRUE;
                pending->AbortStatus = Status;
            }
            // AbortPending admissions were deliberately ignored by the old
            // full-table ordering scan. Retire the equivalent FIFO node now;
            // request/DPC ownership remains unchanged until terminal clear.
            if (pending->AbortPending) {
                ViiperUnlinkAdmissionLocked(pending);
            }
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);

        if (request == WDF_NO_HANDLE) {
            continue;
        }
        unmarkStatus = WdfRequestUnmarkCancelable(request);
        if (NT_SUCCESS(unmarkStatus)) {
            ViiperRemovePublishingRequest(
                controllerContext, index, request, token, Status, TRUE);
            InterlockedIncrement64(&controllerContext->OperationsPurged);
        }
        WdfObjectDereference(request);
    }
}

static
VOID
ViiperAbortManagementOperationsMatching(
    _In_ WDFDEVICE Controller,
    _In_opt_ UDECXUSBDEVICE Device,
    _In_ NTSTATUS Status
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(Controller);
    LARGE_INTEGER retryInterval;

    if (controllerContext->BrokerLock == WDF_NO_HANDLE ||
        controllerContext->ManagementSlots == NULL) {
        return;
    }
    retryInterval.QuadPart = -10 * 1000; // one millisecond, relative
    for (;;) {
        BOOLEAN matchingSlot = FALSE;
        ULONG index;

        for (index = 0; index < VIIPER_UDE_MAX_PENDING_MANAGEMENT; ++index) {
            WDFREQUEST request = WDF_NO_HANDLE;
            UDECXUSBDEVICE deviceReference = WDF_NO_HANDLE;
            UDECXUSBENDPOINT endpointReference = WDF_NO_HANDLE;
            ULONGLONG token = 0;

            WdfSpinLockAcquire(controllerContext->BrokerLock);
            if (controllerContext->ManagementSlots[index].State != ViiperUdePendingEmpty &&
                (Device == WDF_NO_HANDLE ||
                 controllerContext->ManagementSlots[index].Device == Device)) {
                matchingSlot = TRUE;
                if (controllerContext->ManagementSlots[index].State !=
                        ViiperUdePendingCompleting) {
                    request = controllerContext->ManagementSlots[index].Request;
                    token = controllerContext->ManagementSlots[index].Token;
                    controllerContext->ManagementSlots[index].RetiredToken = token;
                    controllerContext->ManagementSlots[index].RetiredDeviceId =
                        controllerContext->ManagementSlots[index].DeviceId;
                    controllerContext->ManagementSlots[index].RetiredDeviceGeneration =
                        controllerContext->ManagementSlots[index].DeviceGeneration;
                    controllerContext->ManagementSlots[index].RetiredEndpointGeneration =
                        controllerContext->ManagementSlots[index].EndpointGeneration;
                    controllerContext->ManagementSlots[index].RetiredOwnerFile =
                        controllerContext->ManagementSlots[index].OwnerFile;
                    controllerContext->ManagementSlots[index].RetiredNotificationPending =
                        controllerContext->ManagementSlots[index].State ==
                            ViiperUdePendingQueued;
                    controllerContext->ManagementSlots[index].State =
                        ViiperUdePendingCompleting;
                    WdfObjectReference(request);
                }
            }
            WdfSpinLockRelease(controllerContext->BrokerLock);
            if (request == WDF_NO_HANDLE) {
                continue;
            }

            WdfRequestComplete(request, Status);
            WdfSpinLockAcquire(controllerContext->BrokerLock);
            if (controllerContext->ManagementSlots[index].Request == request &&
                controllerContext->ManagementSlots[index].Token == token &&
                controllerContext->ManagementSlots[index].State ==
                    ViiperUdePendingCompleting) {
                ViiperClearManagementSlotLocked(
                    controllerContext, index, &deviceReference, &endpointReference);
            }
            WdfSpinLockRelease(controllerContext->BrokerLock);
            ViiperReleaseManagementSlotReferences(deviceReference, endpointReference);
            WdfObjectDereference(request);
            InterlockedIncrement64(&controllerContext->OperationsPurged);
        }

        if (!matchingSlot) {
            return;
        }
        // A matching Completing slot is owned by another finite kernel
        // callback. Join it before child consumption; no new slot can be
        // admitted after ShuttingDown/OwnerFile closing or Device.Purging.
        (VOID)KeDelayExecutionThread(KernelMode, FALSE, &retryInterval);
    }
}

static
VOID
ViiperAbortManagementOperations(
    _In_ WDFDEVICE Controller,
    _In_ NTSTATUS Status
    )
{
    ViiperAbortManagementOperationsMatching(Controller, WDF_NO_HANDLE, Status);
}

VOID
ViiperAbortDeviceManagementOperations(
    _In_ WDFDEVICE Controller,
    _In_ UDECXUSBDEVICE Device,
    _In_ NTSTATUS Status
    )
{
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);

    VIIPER_TRACE_LIFECYCLE(
        Controller, VIIPER_UDE_TRACE_SOURCE_BROKER,
        VIIPER_UDE_TRACE_MANAGEMENT_ABORT_BEGIN, deviceContext->DeviceId,
        deviceContext->Generation, Device, WDF_NO_HANDLE, 0, Status,
        deviceContext->PendingOperations, 0);
    // Device removal has already closed Purging and retired the DeviceLock
    // table entry. Complete every still-published management request while
    // the UDE handle is valid, then release the slot's exact device/endpoint
    // pins before PlugOutAndDelete consumes that handle. The shared abort
    // helper also stably joins a slot already owned by a completing kernel
    // callback, including file cleanup racing the serialized control queue.
    ViiperAbortManagementOperationsMatching(Controller, Device, Status);
    VIIPER_TRACE_LIFECYCLE(
        Controller, VIIPER_UDE_TRACE_SOURCE_BROKER,
        VIIPER_UDE_TRACE_MANAGEMENT_ABORT_END, deviceContext->DeviceId,
        deviceContext->Generation, Device, WDF_NO_HANDLE, 0, Status,
        deviceContext->PendingOperations, 0);
}

VOID
ViiperRetireManagementTombstonesForOwner(
    _In_ WDFDEVICE Controller,
    _In_opt_ WDFFILEOBJECT OwnerFile
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(Controller);
    ULONG index;

    if (controllerContext->BrokerLock == WDF_NO_HANDLE ||
        controllerContext->ManagementSlots == NULL) {
        return;
    }
    // EvtFileClose runs only after this exact file object's I/O is fully
    // drained. Terminal self-managed cleanup passes WDF_NO_HANDLE only after
    // synchronously purging the entire control queue. Exact owner identity in
    // the ordinary case prevents a delayed close from erasing a successor
    // broker's independent late-ACK tombstone.
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    for (index = 0; index < VIIPER_UDE_MAX_PENDING_MANAGEMENT; ++index) {
        VIIPER_UDE_MANAGEMENT_SLOT *pending =
            &controllerContext->ManagementSlots[index];

        if (OwnerFile == WDF_NO_HANDLE || pending->RetiredOwnerFile == OwnerFile) {
            pending->RetiredToken = 0;
            pending->RetiredDeviceId = 0;
            pending->RetiredDeviceGeneration = 0;
            pending->RetiredEndpointGeneration = 0;
            pending->RetiredOwnerFile = WDF_NO_HANDLE;
            pending->RetiredNotificationPending = FALSE;
        }
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
}

VOID
ViiperPurgeEndpointOperations(
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ NTSTATUS Status
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    ViiperAbortMatchingOperations(deviceContext->Controller, Endpoint, Status);
}

VOID
ViiperPurgeOwnerOperations(
    _In_ WDFDEVICE Controller,
    _In_ NTSTATUS Status
    )
{
    ViiperAbortMatchingOperations(Controller, WDF_NO_HANDLE, Status);
    ViiperAbortManagementOperations(Controller, Status);
}

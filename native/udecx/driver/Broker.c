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

typedef struct VIIPER_UDE_ORPHAN_COMPLETION_CONTEXT {
    WDFREQUEST Request;
    NTSTATUS Status;
} VIIPER_UDE_ORPHAN_COMPLETION_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(
    VIIPER_UDE_ORPHAN_COMPLETION_CONTEXT,
    ViiperGetOrphanCompletionContext)

static EVT_WDF_DPC ViiperEvtOrphanCompletionDpc;

static
VOID
ViiperEvtOrphanCompletionDpc(
    _In_ WDFDPC Dpc
    )
{
    VIIPER_UDE_ORPHAN_COMPLETION_CONTEXT *context =
        ViiperGetOrphanCompletionContext(Dpc);
    WDFREQUEST request = context->Request;

    UdecxUrbCompleteWithNtStatus(request, context->Status);
    WdfObjectDereference(request);
    WdfObjectDelete(Dpc);
}

VOID
ViiperCompleteUnownedUrbAsync(
    _In_ WDFDEVICE Controller,
    _In_ WDFREQUEST Request,
    _In_ NTSTATUS Status
    )
{
    WDF_DPC_CONFIG config;
    WDF_OBJECT_ATTRIBUTES attributes;
    VIIPER_UDE_ORPHAN_COMPLETION_CONTEXT *context;
    WDFDPC dpc;
    NTSTATUS createStatus;

    WDF_DPC_CONFIG_INIT(&config, ViiperEvtOrphanCompletionDpc);
    config.AutomaticSerialization = FALSE;
    WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(
        &attributes, VIIPER_UDE_ORPHAN_COMPLETION_CONTEXT);
    attributes.ParentObject = Controller;
    createStatus = WdfDpcCreate(&config, &attributes, &dpc);
    if (!NT_SUCCESS(createStatus)) {
        KIRQL previousIrql = KeGetCurrentIrql();
        if (previousIrql < DISPATCH_LEVEL) {
            KeRaiseIrql(DISPATCH_LEVEL, &previousIrql);
            UdecxUrbCompleteWithNtStatus(Request, Status);
            KeLowerIrql(previousIrql);
        } else {
            UdecxUrbCompleteWithNtStatus(Request, Status);
        }
        return;
    }

    context = ViiperGetOrphanCompletionContext(dpc);
    context->Request = Request;
    context->Status = Status;
    WdfObjectReference(Request);
    if (!WdfDpcEnqueue(dpc)) {
        KIRQL previousIrql = KeGetCurrentIrql();
        WdfObjectDereference(Request);
        WdfObjectDelete(dpc);
        if (previousIrql < DISPATCH_LEVEL) {
            KeRaiseIrql(DISPATCH_LEVEL, &previousIrql);
            UdecxUrbCompleteWithNtStatus(Request, Status);
            KeLowerIrql(previousIrql);
        } else {
            UdecxUrbCompleteWithNtStatus(Request, Status);
        }
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

        WdfSpinLockAcquire(controllerContext->BrokerLock);
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
                    controllerContext->ManagementSlots[managementSlot].Token != event.Token ||
                    controllerContext->ManagementSlots[managementSlot].State !=
                        ViiperUdePendingQueued) {
                    status = STATUS_INVALID_DEVICE_STATE;
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
        WdfRequestSetInformation(dequeueRequest, sizeof(*operation));
        InterlockedIncrement64(&controllerContext->NotificationEventsDelivered);
        WdfRequestComplete(dequeueRequest, STATUS_SUCCESS);
    }
}

static
VOID
ViiperClearManagementSlotLocked(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ ULONG Slot
    )
{
    VIIPER_UDE_MANAGEMENT_SLOT *pending = &ControllerContext->ManagementSlots[Slot];

    pending->Request = WDF_NO_HANDLE;
    pending->Token = 0;
    pending->DeviceId = 0;
    pending->DeviceGeneration = 0;
    pending->State = ViiperUdePendingEmpty;
    pending->Kind = 0;
    pending->EndpointAddress = 0;
    InterlockedDecrement(&ControllerContext->PendingOperations);
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

    pending->Request = WDF_NO_HANDLE;
    pending->Endpoint = WDF_NO_HANDLE;
    pending->Token = 0;
    pending->DeviceId = 0;
    pending->AdmissionSequence = 0;
    pending->DeviceGeneration = 0;
    pending->State = ViiperUdePendingEmpty;
    pending->AbortPending = FALSE;
    pending->PublishedToOwner = FALSE;
    pending->EndpointAddress = 0;
    pending->AbortStatus = STATUS_SUCCESS;
    pending->CompletionStatus = STATUS_SUCCESS;
    pending->CompletionUsbdStatus = USBD_STATUS_SUCCESS;
    pending->CompleteWithNtStatus = FALSE;
    InterlockedDecrement(&ControllerContext->PendingOperations);
    if (deviceContext != NULL) {
        InterlockedDecrement(&deviceContext->PendingOperations);
    }
    if (endpoint != WDF_NO_HANDLE) {
        ViiperEndpointOperationCompleted(endpoint);
    }
}

VOID
ViiperEndpointOperationStarted(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    // Callers serialize admission for one endpoint. Clear before publishing
    // the increment so a concurrent purge worker can never observe the old
    // signaled state between a 0 -> 1 transition and KeClearEvent.
    KeClearEvent(&endpointContext->OperationsDrained);
    (VOID)InterlockedIncrement(&endpointContext->ActiveOperations);
}

VOID
ViiperEndpointOperationCompleted(
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
    WdfWaitLockAcquire(controllerContext->OwnerLock, NULL);
    if (controllerContext->OwnerFile != fileObject || controllerContext->CleanupInProgress ||
        InterlockedCompareExchange(&fileContext->Negotiated, 0, 0) == 0 ||
        InterlockedCompareExchange(&fileContext->Closing, 0, 0) != 0) {
        status = STATUS_INVALID_DEVICE_STATE;
    }
    WdfWaitLockRelease(controllerContext->OwnerLock);
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
    dpcConfig.AutomaticSerialization = FALSE;
    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = Device;
    return WdfDpcCreate(&dpcConfig, &attributes, &controllerContext->CompletionDpc);
}

VOID
ViiperEvtCompletionDpc(
    _In_ WDFDPC Dpc
    )
{
    WDFDEVICE controller = (WDFDEVICE)WdfDpcGetParentObject(Dpc);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(controller);

    for (;;) {
        WDFREQUEST request = WDF_NO_HANDLE;
        ULONGLONG token = 0;
        ULONG slot = VIIPER_UDE_MAX_PENDING_OPERATIONS;
        NTSTATUS completionStatus = STATUS_SUCCESS;
        USBD_STATUS usbdStatus = USBD_STATUS_SUCCESS;
        BOOLEAN completeWithNtStatus = FALSE;
        ULONG index;

        WdfSpinLockAcquire(controllerContext->BrokerLock);
        for (index = 0; index < VIIPER_UDE_MAX_PENDING_OPERATIONS; ++index) {
            ULONG candidate = (controllerContext->NextCompletionSlot + index) %
                VIIPER_UDE_MAX_PENDING_OPERATIONS;
            VIIPER_UDE_PENDING_SLOT *pending = &controllerContext->PendingSlots[candidate];
            if (pending->State != ViiperUdePendingDpcCompletion) {
                continue;
            }
            request = pending->Request;
            token = pending->Token;
            slot = candidate;
            completionStatus = pending->CompletionStatus;
            usbdStatus = pending->CompletionUsbdStatus;
            completeWithNtStatus = pending->CompleteWithNtStatus;
            pending->State = ViiperUdePendingCompleting;
            controllerContext->NextCompletionSlot = (candidate + 1) %
                VIIPER_UDE_MAX_PENDING_OPERATIONS;
            WdfObjectReference(request);
            break;
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);

        if (request == WDF_NO_HANDLE) {
            break;
        }

        if (completeWithNtStatus) {
            UdecxUrbCompleteWithNtStatus(request, completionStatus);
        } else {
            UdecxUrbComplete(request, usbdStatus);
        }

        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (ViiperSlotMatches(&controllerContext->PendingSlots[slot], request, token) &&
            controllerContext->PendingSlots[slot].State == ViiperUdePendingCompleting) {
            ViiperClearSlotLocked(controllerContext, slot);
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);
        WdfObjectDereference(request);
    }
}

static
BOOLEAN
ViiperQueueLifecycleEventLocked(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ VIIPER_UDE_DEVICE_CONTEXT *DeviceContext,
    _In_opt_ const USB_ENDPOINT_DESCRIPTOR *EndpointDescriptor,
    _In_ VIIPER_UDE_OPERATION_KIND Kind,
    _In_ UCHAR InterfaceNumber,
    _In_ UCHAR InterfaceSetting,
    _In_ ULONGLONG Token
    )
{
    VIIPER_UDE_NOTIFICATION *event;

    if (ControllerContext->NotificationCount >= VIIPER_UDE_MAX_PENDING_OPERATIONS - 1) {
        (VOID)ViiperFaultBrokerLocked(ControllerContext);
        return FALSE;
    }

    event = &ControllerContext->Notifications[ControllerContext->NotificationTail];
    RtlZeroMemory(event, sizeof(*event));
    event->Token = Token;
    event->DeviceId = DeviceContext->DeviceId;
    event->Generation = DeviceContext->Generation;
    event->Kind = Kind;
    if (EndpointDescriptor != NULL) {
        event->EndpointAddress = EndpointDescriptor->bEndpointAddress;
        event->EndpointAttributes = EndpointDescriptor->bmAttributes;
        event->EndpointInterval = EndpointDescriptor->bInterval;
        event->EndpointMaxPacketSize = EndpointDescriptor->wMaxPacketSize;
    }
    event->InterfaceNumber = InterfaceNumber;
    event->InterfaceSetting = InterfaceSetting;
    event->EndpointSequence = (ULONGLONG)InterlockedIncrement64(
        &DeviceContext->EndpointSequences[event->EndpointAddress]);
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
    BOOLEAN queued;

    WdfSpinLockAcquire(controllerContext->BrokerLock);
    queued = ViiperQueueLifecycleEventLocked(
        controllerContext,
        deviceContext,
        &endpointContext->Descriptor,
        Kind,
        0,
        0,
        0);
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!queued) {
        return STATUS_INSUFFICIENT_RESOURCES;
    }
    ViiperDispatchNotificationEvents(deviceContext->Controller);
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
    BOOLEAN queued;

    WdfSpinLockAcquire(controllerContext->BrokerLock);
    queued = ViiperQueueLifecycleEventLocked(
        controllerContext, deviceContext, NULL, Kind, 0, 0, 0);
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!queued) {
        return STATUS_INSUFFICIENT_RESOURCES;
    }
    ViiperDispatchNotificationEvents(deviceContext->Controller);
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
    BOOLEAN queued;

    WdfSpinLockAcquire(controllerContext->BrokerLock);
    queued = ViiperQueueLifecycleEventLocked(
        controllerContext,
        deviceContext,
        NULL,
        ViiperUdeOperationSetInterface,
        InterfaceNumber,
        InterfaceSetting,
        0);
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!queued) {
        return STATUS_INSUFFICIENT_RESOURCES;
    }
    ViiperDispatchNotificationEvents(deviceContext->Controller);
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
    ULONG offset;
    NTSTATUS status = STATUS_INSUFFICIENT_RESOURCES;
    BOOLEAN canAllocate = TRUE;

    if (Endpoint != WDF_NO_HANDLE) {
        descriptor = &ViiperGetEndpointContext(Endpoint)->Descriptor;
    }

    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (InterlockedCompareExchange(&controllerContext->BrokerFaulted, FALSE, FALSE) != FALSE ||
        InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0) {
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

        if (pending->State != ViiperUdePendingEmpty) {
            continue;
        }
        ++pending->Generation;
        if (pending->Generation == 0) {
            ++pending->Generation;
        }
        token = ((ULONGLONG)pending->Generation << 32) |
            VIIPER_UDE_MANAGEMENT_SLOT_FLAG | (index + 1);
        pending->Request = Request;
        pending->Token = token;
        pending->DeviceId = deviceContext->DeviceId;
        pending->DeviceGeneration = deviceContext->Generation;
        pending->State = ViiperUdePendingQueued;
        pending->Kind = Kind;
        pending->EndpointAddress = descriptor != NULL ? descriptor->bEndpointAddress : 0;
        if (!ViiperQueueLifecycleEventLocked(
                controllerContext,
                deviceContext,
                descriptor,
                Kind,
                InterfaceNumber,
                InterfaceSetting,
                token)) {
            pending->Request = WDF_NO_HANDLE;
            pending->Token = 0;
            pending->DeviceId = 0;
            pending->DeviceGeneration = 0;
            pending->State = ViiperUdePendingEmpty;
            pending->Kind = 0;
            pending->EndpointAddress = 0;
            break;
        }
        controllerContext->NextManagementSlot = (index + 1) %
            VIIPER_UDE_MAX_PENDING_MANAGEMENT;
        InterlockedIncrement(&controllerContext->PendingOperations);
        status = STATUS_SUCCESS;
        break;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);

    if (status == STATUS_INSUFFICIENT_RESOURCES) {
        InterlockedIncrement64(&controllerContext->QueueExhaustions);
    }
    if (NT_SUCCESS(status)) {
        ViiperDispatchNotificationEvents(deviceContext->Controller);
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
    if (InterlockedCompareExchange(&endpointContext->Purging, 0, 0) != 0 ||
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
        if (pending->State != ViiperUdePendingEmpty) {
            continue;
        }
        ++pending->Generation;
        if (pending->Generation == 0) {
            ++pending->Generation;
        }
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
        pending->State = ViiperUdePendingPreparing;
        pending->AbortPending = FALSE;
        pending->PublishedToOwner = FALSE;
        pending->EndpointAddress = endpointContext->Descriptor.bEndpointAddress;
        pending->AbortStatus = STATUS_SUCCESS;
        ControllerContext->NextPendingSlot = (index + 1) % VIIPER_UDE_MAX_PENDING_OPERATIONS;
        ViiperEndpointOperationStarted(Endpoint);
        InterlockedIncrement(&ControllerContext->PendingOperations);
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

static
BOOLEAN
ViiperHasEarlierUnpublishedAdmissionLocked(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ ULONG CandidateSlot
    )
{
    const VIIPER_UDE_PENDING_SLOT *candidate =
        &ControllerContext->PendingSlots[CandidateSlot];
    ULONG index;

    for (index = 0; index < VIIPER_UDE_MAX_PENDING_OPERATIONS; ++index) {
        const VIIPER_UDE_PENDING_SLOT *other;
        if (index == CandidateSlot) {
            continue;
        }
        other = &ControllerContext->PendingSlots[index];
        if (other->State == ViiperUdePendingEmpty || other->PublishedToOwner ||
            other->AbortPending ||
            other->State == ViiperUdePendingCompleting ||
            other->State == ViiperUdePendingDpcCompletion ||
            other->DeviceId != candidate->DeviceId ||
            other->DeviceGeneration != candidate->DeviceGeneration ||
            other->EndpointAddress != candidate->EndpointAddress ||
            other->AdmissionSequence == 0 ||
            other->AdmissionSequence >= candidate->AdmissionSequence) {
            continue;
        }
        return TRUE;
    }
    return FALSE;
}

VOID
ViiperEvtUrbCancel(
    _In_ WDFREQUEST Request
    )
{
    VIIPER_UDE_REQUEST_CONTEXT *requestContext = ViiperGetRequestContext(Request);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(requestContext->Controller);
    BOOLEAN ownsRequest = FALSE;
    BOOLEAN notifyOwner = FALSE;

    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (requestContext->PendingSlot < VIIPER_UDE_MAX_PENDING_OPERATIONS) {
        VIIPER_UDE_PENDING_SLOT *pending =
            &controllerContext->PendingSlots[requestContext->PendingSlot];
        if (ViiperSlotMatches(pending, Request, requestContext->Token)) {
            notifyOwner = ViiperQueueCancelEventLocked(controllerContext, pending);
            pending->CompletionStatus = STATUS_CANCELLED;
            pending->CompletionUsbdStatus = USBD_STATUS_CANCELED;
            pending->CompleteWithNtStatus = TRUE;
            pending->State = ViiperUdePendingDpcCompletion;
            ownsRequest = TRUE;
        }
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);

    if (ownsRequest) {
        InterlockedIncrement64(&controllerContext->OperationsCancelled);
        (VOID)WdfDpcEnqueue(controllerContext->CompletionDpc);
        if (notifyOwner) {
            ViiperDispatchNotificationEvents(requestContext->Controller);
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
        span = (ULONGLONG)PacketCount * interval;
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
    ULONG currentFrame;
    ULONG startFrame;
    ULONG nextFrame;
    ULONG span;

    span = ViiperIsoFrameSpan(EndpointContext, PacketCount);
    if ((TransferFlags & USBD_START_ISO_TRANSFER_ASAP) == 0) {
        InterlockedExchange64(
            &EndpointContext->NextIsoStartFrame,
            (LONG64)(ULONGLONG)(RequestedStartFrame + span));
        return RequestedStartFrame;
    }

    currentFrame = (ULONG)(KeQueryInterruptTime() / 10000ULL);
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
        pending->State = ViiperUdePendingDpcCompletion;
        queued = TRUE;
    }
    WdfSpinLockRelease(ControllerContext->BrokerLock);
    if (queued) {
        (VOID)WdfDpcEnqueue(ControllerContext->CompletionDpc);
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
        ownsRequest = TRUE;
    }
    WdfSpinLockRelease(ControllerContext->BrokerLock);
    if (ownsRequest) {
        (VOID)WdfDpcEnqueue(ControllerContext->CompletionDpc);
        if (notifyOwner) {
            ViiperDispatchNotificationEvents(ViiperGetRequestContext(Request)->Controller);
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

        ViiperDispatchNotificationEvents(Controller);
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        for (index = 0; index < VIIPER_UDE_MAX_PENDING_OPERATIONS; ++index) {
            ULONG candidate = (controllerContext->NextPendingSlot + index) %
                VIIPER_UDE_MAX_PENDING_OPERATIONS;
            VIIPER_UDE_PENDING_SLOT *pending = &controllerContext->PendingSlots[candidate];
            if (pending->State == ViiperUdePendingQueued &&
                !ViiperHasEarlierUnpublishedAdmissionLocked(controllerContext, candidate)) {
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
                pending->State = abortPending
                    ? ViiperUdePendingCompleting
                    : ViiperUdePendingInFlight;
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
    if (controllerContext->OwnerFile != fileObject ||
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
    NTSTATUS abortStatus = STATUS_CANCELLED;

    if (InterlockedCompareExchange(&controllerContext->BrokerFaulted, FALSE, FALSE) != FALSE ||
        InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0 ||
        InterlockedCompareExchange(&endpointContext->Purging, 0, 0) != 0) {
        return STATUS_DEVICE_NOT_READY;
    }
    RtlZeroMemory(requestContext, sizeof(*requestContext));
    requestContext->Controller = deviceContext->Controller;
    requestContext->Endpoint = endpoint;
    requestContext->PendingSlot = VIIPER_UDE_MAX_PENDING_OPERATIONS;
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
            }
        } else {
            ViiperClearSlotLocked(controllerContext, slot);
        }
    } else if (NT_SUCCESS(status)) {
        cancelClaimed = TRUE;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!NT_SUCCESS(status)) {
        return STATUS_CANCELLED;
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
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ const VIIPER_UDE_COMPLETION *Completion
    )
{
    ULONG encodedSlot = (ULONG)Completion->Token;
    ULONG slot = (encodedSlot & ~VIIPER_UDE_MANAGEMENT_SLOT_FLAG) - 1;
    WDFREQUEST request = WDF_NO_HANDLE;

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
        ControllerContext->ManagementSlots[slot].DeviceGeneration == Completion->Generation) {
        request = ControllerContext->ManagementSlots[slot].Request;
        ControllerContext->ManagementSlots[slot].State = ViiperUdePendingCompleting;
        WdfObjectReference(request);
    }
    WdfSpinLockRelease(ControllerContext->BrokerLock);
    if (request == WDF_NO_HANDLE) {
        InterlockedIncrement64(&ControllerContext->LateCompletions);
        return STATUS_NOT_FOUND;
    }

    WdfRequestComplete(request, (NTSTATUS)Completion->Status);
    WdfSpinLockAcquire(ControllerContext->BrokerLock);
    if (ControllerContext->ManagementSlots[slot].Request == request &&
        ControllerContext->ManagementSlots[slot].Token == Completion->Token &&
        ControllerContext->ManagementSlots[slot].State == ViiperUdePendingCompleting) {
        ViiperClearManagementSlotLocked(ControllerContext, slot);
    }
    WdfSpinLockRelease(ControllerContext->BrokerLock);
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
        return ViiperCompleteManagementOperation(controllerContext, completion);
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
        urbRequest = controllerContext->PendingSlots[slot].Request;
        controllerContext->PendingSlots[slot].State = ViiperUdePendingCompleting;
        WdfObjectReference(urbRequest);
    } else {
        expectedLateAbort = ViiperExpectedLateAbortLocked(
            &controllerContext->PendingSlots[slot], completion->Token);
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
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
ViiperAbortManagementOperations(
    _In_ WDFDEVICE Controller,
    _In_ NTSTATUS Status
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(Controller);
    ULONG index;

    if (controllerContext->BrokerLock == WDF_NO_HANDLE ||
        controllerContext->ManagementSlots == NULL) {
        return;
    }
    for (index = 0; index < VIIPER_UDE_MAX_PENDING_MANAGEMENT; ++index) {
        WDFREQUEST request = WDF_NO_HANDLE;
        ULONGLONG token = 0;

        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (controllerContext->ManagementSlots[index].State != ViiperUdePendingEmpty &&
            controllerContext->ManagementSlots[index].State != ViiperUdePendingCompleting) {
            request = controllerContext->ManagementSlots[index].Request;
            token = controllerContext->ManagementSlots[index].Token;
            controllerContext->ManagementSlots[index].State = ViiperUdePendingCompleting;
            WdfObjectReference(request);
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);
        if (request == WDF_NO_HANDLE) {
            continue;
        }

        WdfRequestComplete(request, Status);
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (controllerContext->ManagementSlots[index].Request == request &&
            controllerContext->ManagementSlots[index].Token == token &&
            controllerContext->ManagementSlots[index].State == ViiperUdePendingCompleting) {
            ViiperClearManagementSlotLocked(controllerContext, index);
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);
        WdfObjectDereference(request);
        InterlockedIncrement64(&controllerContext->OperationsPurged);
    }
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

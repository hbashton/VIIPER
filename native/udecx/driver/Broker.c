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

static
VOID
ViiperClearSlotLocked(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ ULONG Slot
    )
{
    VIIPER_UDE_PENDING_SLOT *pending = &ControllerContext->PendingSlots[Slot];

    pending->Request = WDF_NO_HANDLE;
    pending->Endpoint = WDF_NO_HANDLE;
    pending->Token = 0;
    pending->State = ViiperUdePendingEmpty;
    pending->AbortPending = FALSE;
    pending->AbortStatus = STATUS_SUCCESS;
    InterlockedDecrement(&ControllerContext->PendingOperations);
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

static
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
        !fileContext->Negotiated || fileContext->Closing) {
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
    return STATUS_SUCCESS;
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
    ULONG offset;
    NTSTATUS status = STATUS_INSUFFICIENT_RESOURCES;

    WdfSpinLockAcquire(ControllerContext->BrokerLock);
    for (offset = 0; offset < VIIPER_UDE_MAX_PENDING_OPERATIONS; ++offset) {
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
        pending->State = ViiperUdePendingPreparing;
        pending->AbortPending = FALSE;
        pending->AbortStatus = STATUS_SUCCESS;
        ControllerContext->NextPendingSlot = (index + 1) % VIIPER_UDE_MAX_PENDING_OPERATIONS;
        InterlockedIncrement(&ControllerContext->PendingOperations);
        *Slot = index;
        *Token = pending->Token;
        status = STATUS_SUCCESS;
        break;
    }
    WdfSpinLockRelease(ControllerContext->BrokerLock);

    if (!NT_SUCCESS(status)) {
        InterlockedIncrement64(&ControllerContext->QueueExhaustions);
    }
    return status;
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

    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (requestContext->PendingSlot < VIIPER_UDE_MAX_PENDING_OPERATIONS) {
        VIIPER_UDE_PENDING_SLOT *pending =
            &controllerContext->PendingSlots[requestContext->PendingSlot];
        if (ViiperSlotMatches(pending, Request, requestContext->Token)) {
            ViiperClearSlotLocked(controllerContext, requestContext->PendingSlot);
            ownsRequest = TRUE;
        }
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);

    if (ownsRequest) {
        InterlockedIncrement64(&controllerContext->OperationsCancelled);
        UdecxUrbCompleteWithNtStatus(Request, STATUS_CANCELLED);
    }
}

static
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
    _In_ WDFREQUEST DequeueRequest
    )
{
    PURB urb = ViiperGetUrb(UrbRequest);
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_REQUEST_CONTEXT *requestContext = ViiperGetRequestContext(UrbRequest);
    VIIPER_UDE_OPERATION *operation;
    VIIPER_UDE_ISO_PACKET *packets;
    UCHAR *payload;
    UCHAR *transferBuffer = NULL;
    ULONG transferBufferLength = 0;
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

    isoBytes = packetCount * sizeof(VIIPER_UDE_ISO_PACKET);
    payloadLength = directionIn ? 0 : transferLength;
    if (payloadLength > 0) {
        status = UdecxUrbRetrieveBuffer(UrbRequest, &transferBuffer, &transferBufferLength);
        if (!NT_SUCCESS(status) || transferBufferLength < payloadLength) {
            return NT_SUCCESS(status) ? STATUS_BUFFER_TOO_SMALL : status;
        }
    }
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
        RtlCopyMemory(payload, transferBuffer, payloadLength);
    }

    requestContext->TransferLength = transferLength;
    requestContext->IsoPacketCount = packetCount;
    requestContext->DirectionIn = directionIn;
    WdfRequestSetInformation(DequeueRequest, totalLength);
    InterlockedIncrement64(&ControllerContext->OperationsDequeued);
    if (!directionIn) {
        InterlockedAdd64(&ControllerContext->BytesToDevice, transferLength);
    }
    return STATUS_SUCCESS;
}

static
VOID
ViiperRemovePublishingRequest(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ ULONG Slot,
    _In_ WDFREQUEST Request,
    _In_ ULONGLONG Token,
    _In_ NTSTATUS Status
    )
{
    BOOLEAN ownsRequest = FALSE;

    WdfSpinLockAcquire(ControllerContext->BrokerLock);
    if (Slot < VIIPER_UDE_MAX_PENDING_OPERATIONS &&
        ViiperSlotMatches(&ControllerContext->PendingSlots[Slot], Request, Token)) {
        ViiperClearSlotLocked(ControllerContext, Slot);
        ownsRequest = TRUE;
    }
    WdfSpinLockRelease(ControllerContext->BrokerLock);
    if (ownsRequest) {
        UdecxUrbCompleteWithNtStatus(Request, Status);
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
        ULONGLONG token = 0;
        ULONG slot = VIIPER_UDE_MAX_PENDING_OPERATIONS;
        ULONG index;
        NTSTATUS status;
        BOOLEAN abortPending = FALSE;
        NTSTATUS abortStatus = STATUS_CANCELLED;

        WdfSpinLockAcquire(controllerContext->BrokerLock);
        for (index = 0; index < VIIPER_UDE_MAX_PENDING_OPERATIONS; ++index) {
            ULONG candidate = (controllerContext->NextPendingSlot + index) %
                VIIPER_UDE_MAX_PENDING_OPERATIONS;
            VIIPER_UDE_PENDING_SLOT *pending = &controllerContext->PendingSlots[candidate];
            if (pending->State == ViiperUdePendingQueued) {
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
                controllerContext, slot, urbRequest, token, status);
            WdfRequestComplete(dequeueRequest, status);
            WdfObjectDereference(urbRequest);
            continue;
        }

        status = ViiperSerializeOperation(
            controllerContext, urbRequest, endpoint, token, dequeueRequest);

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
                controllerContext, slot, urbRequest, token, completionStatus);
            WdfRequestComplete(dequeueRequest, completionStatus);
            WdfObjectDereference(urbRequest);
            continue;
        }

        status = WdfRequestMarkCancelableEx(urbRequest, ViiperEvtUrbCancel);
        if (!NT_SUCCESS(status)) {
            ViiperRemovePublishingRequest(
                controllerContext, slot, urbRequest, token, STATUS_CANCELLED);
            WdfRequestComplete(dequeueRequest, STATUS_CANCELLED);
            WdfObjectDereference(urbRequest);
            continue;
        }

        abortPending = FALSE;
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (slot < VIIPER_UDE_MAX_PENDING_OPERATIONS &&
            ViiperSlotMatches(&controllerContext->PendingSlots[slot], urbRequest, token)) {
            VIIPER_UDE_PENDING_SLOT *pending = &controllerContext->PendingSlots[slot];
            abortPending = pending->AbortPending;
            abortStatus = pending->AbortStatus;
            pending->State = abortPending
                ? ViiperUdePendingCompleting
                : ViiperUdePendingInFlight;
        } else {
            status = STATUS_CANCELLED;
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);
        if (!NT_SUCCESS(status) || abortPending) {
            NTSTATUS completionStatus = abortPending ? abortStatus : STATUS_CANCELLED;
            NTSTATUS unmarkStatus = WdfRequestUnmarkCancelable(urbRequest);
            if (NT_SUCCESS(unmarkStatus)) {
                ViiperRemovePublishingRequest(
                    controllerContext, slot, urbRequest, token, completionStatus);
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
    NTSTATUS status = ViiperValidateBrokerOwner(controller, Request);

    if (!NT_SUCCESS(status)) {
        return status;
    }
    status = WdfRequestForwardToIoQueue(Request, controllerContext->WaitingDequeues);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    InterlockedIncrement(&controllerContext->WaitingDequeueCount);
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
    NTSTATUS abortStatus = STATUS_CANCELLED;

    if (deviceContext->Purging || endpointContext->Purging) {
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
            abortPending = controllerContext->PendingSlots[slot].AbortPending;
            abortStatus = controllerContext->PendingSlots[slot].AbortStatus;
            controllerContext->PendingSlots[slot].State = abortPending
                ? ViiperUdePendingCompleting
                : ViiperUdePendingQueued;
        } else {
            ViiperClearSlotLocked(controllerContext, slot);
        }
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!NT_SUCCESS(status)) {
        return STATUS_CANCELLED;
    }
    if (abortPending) {
        status = WdfRequestUnmarkCancelable(Request);
        if (NT_SUCCESS(status)) {
            ViiperRemovePublishingRequest(
                controllerContext, slot, Request, token, abortStatus);
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
    UCHAR *transferBuffer = NULL;
    ULONG transferBufferLength = 0;
    size_t inputLength;
    size_t tailLength = 0;
    ULONG slot;
    ULONG index;
    NTSTATUS status;
    BOOLEAN slotRemoved = FALSE;

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
        completion->Header.Size < sizeof(*completion) || completion->Token == 0 ||
        completion->DeviceId == 0 || completion->Generation == 0 ||
        completion->TransferLength > VIIPER_UDE_MAX_TRANSFER_BYTES ||
        completion->PayloadLength > VIIPER_UDE_MAX_TRANSFER_BYTES ||
        completion->IsoPacketCount > VIIPER_UDE_MAX_ISO_PACKETS) {
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
            completion->IsoPacketCount * sizeof(VIIPER_UDE_ISO_PACKET),
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
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (urbRequest == WDF_NO_HANDLE) {
        InterlockedIncrement64(&controllerContext->LateCompletions);
        return STATUS_NOT_FOUND;
    }

    status = WdfRequestUnmarkCancelable(urbRequest);
    if (!NT_SUCCESS(status)) {
        WdfObjectDereference(urbRequest);
        InterlockedIncrement64(&controllerContext->LateCompletions);
        return status;
    }
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (ViiperSlotMatches(
            &controllerContext->PendingSlots[slot], urbRequest, completion->Token)) {
        ViiperClearSlotLocked(controllerContext, slot);
        slotRemoved = TRUE;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!slotRemoved) {
        InterlockedIncrement64(&controllerContext->LateCompletions);
        WdfObjectDereference(urbRequest);
        return STATUS_NOT_FOUND;
    }

    requestContext = ViiperGetRequestContext(urbRequest);
    urb = ViiperGetUrb(urbRequest);
    if (urb == NULL || completion->DeviceId !=
            ViiperGetDeviceContext(ViiperGetEndpointContext(requestContext->Endpoint)->Device)->DeviceId ||
        completion->Generation !=
            ViiperGetDeviceContext(ViiperGetEndpointContext(requestContext->Endpoint)->Device)->Generation ||
        completion->TransferLength > requestContext->TransferLength ||
        completion->IsoPacketCount != requestContext->IsoPacketCount ||
        (requestContext->DirectionIn && completion->PayloadLength != completion->TransferLength)) {
        status = STATUS_INVALID_PARAMETER;
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        goto CompleteWithNtStatus;
    }
    if (!NT_SUCCESS((NTSTATUS)completion->Status)) {
        status = (NTSTATUS)completion->Status;
        goto CompleteWithNtStatus;
    }

    if (requestContext->DirectionIn && completion->TransferLength > 0) {
        status = UdecxUrbRetrieveBuffer(
            urbRequest, &transferBuffer, &transferBufferLength);
        if (!NT_SUCCESS(status) || transferBufferLength < completion->TransferLength) {
            status = NT_SUCCESS(status) ? STATUS_BUFFER_TOO_SMALL : status;
            goto CompleteWithNtStatus;
        }
        RtlCopyMemory(transferBuffer, payload, completion->TransferLength);
        InterlockedAdd64(&controllerContext->BytesFromDevice, completion->TransferLength);
    }
    if (completion->IsoPacketCount != 0) {
        for (index = 0; index < completion->IsoPacketCount; ++index) {
            if (packets[index].Offset > completion->TransferLength ||
                packets[index].Length > completion->TransferLength - packets[index].Offset) {
                status = STATUS_INVALID_PARAMETER;
                goto CompleteWithNtStatus;
            }
            urb->UrbIsochronousTransfer.IsoPacket[index].Offset = packets[index].Offset;
            urb->UrbIsochronousTransfer.IsoPacket[index].Length = packets[index].Length;
            urb->UrbIsochronousTransfer.IsoPacket[index].Status = packets[index].Status;
        }
        InterlockedAdd64(&controllerContext->IsoPackets, completion->IsoPacketCount);
    }

    UdecxUrbSetBytesCompleted(urbRequest, completion->TransferLength);
    UdecxUrbComplete(urbRequest, (USBD_STATUS)completion->UsbdStatus);
    InterlockedIncrement64(&controllerContext->OperationsCompleted);
    WdfObjectDereference(urbRequest);
    return STATUS_SUCCESS;

CompleteWithNtStatus:
    UdecxUrbCompleteWithNtStatus(urbRequest, status);
    WdfObjectDereference(urbRequest);
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
            } else if (pending->State != ViiperUdePendingPreparing &&
                pending->State != ViiperUdePendingCompleting) {
                request = pending->Request;
                token = pending->Token;
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
                controllerContext, index, request, token, Status);
            InterlockedIncrement64(&controllerContext->OperationsPurged);
        }
        WdfObjectDereference(request);
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
}

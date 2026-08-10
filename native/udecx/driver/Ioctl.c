#include "ViiperUde.h"

static
BOOLEAN
ViiperValidateHeader(
    _In_ const VIIPER_UDE_HEADER *Header,
    _In_ size_t BufferLength,
    _In_ size_t ExpectedSize
    )
{
    return BufferLength >= ExpectedSize &&
        Header->Magic == VIIPER_UDE_MAGIC &&
        Header->Major == VIIPER_UDE_ABI_MAJOR &&
        Header->Minor == VIIPER_UDE_ABI_MINOR &&
        Header->Size == ExpectedSize;
}

static
LONG64
ViiperReadCounter(
    _In_ volatile LONG64 *Counter
    )
{
    return InterlockedCompareExchange64(Counter, 0, 0);
}

static
NTSTATUS
ViiperHandleNegotiate(
    _In_ WDFREQUEST Request
    )
{
    NTSTATUS status;
    VIIPER_UDE_NEGOTIATE_REQUEST *input;
    VIIPER_UDE_NEGOTIATE_RESPONSE *output;
    size_t inputLength;
    size_t outputLength;
    WDFFILEOBJECT fileObject;
    VIIPER_UDE_FILE_CONTEXT *fileContext;
    LARGE_INTEGER ticks;

    status = WdfRequestRetrieveInputBuffer(
        Request, sizeof(*input), (PVOID *)&input, &inputLength);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    status = WdfRequestRetrieveOutputBuffer(
        Request, sizeof(*output), (PVOID *)&output, &outputLength);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    if (!ViiperValidateHeader(&input->Header, inputLength, sizeof(*input)) ||
        input->ClientNonce == 0) {
        return STATUS_INVALID_PARAMETER;
    }

    fileObject = WdfRequestGetFileObject(Request);
    if (fileObject == WDF_NO_HANDLE) {
        return STATUS_INVALID_HANDLE;
    }
    fileContext = ViiperGetFileContext(fileObject);
    if (fileContext->Closing) {
        return STATUS_FILE_CLOSED;
    }
    if (fileContext->Negotiated && fileContext->ClientNonce != input->ClientNonce) {
        return STATUS_INVALID_DEVICE_STATE;
    }

    if (!fileContext->Negotiated) {
        ticks = KeQueryPerformanceCounter(NULL);
        fileContext->ClientNonce = input->ClientNonce;
        fileContext->DriverNonce = ((ULONGLONG)ticks.QuadPart) ^
            ((ULONGLONG)(ULONG_PTR)fileObject << 13) ^ input->ClientNonce;
        if (fileContext->DriverNonce == 0) {
            fileContext->DriverNonce = 1;
        }
        fileContext->Negotiated = TRUE;
    }

    RtlZeroMemory(output, sizeof(*output));
    output->Header.Magic = VIIPER_UDE_MAGIC;
    output->Header.Major = VIIPER_UDE_ABI_MAJOR;
    output->Header.Minor = VIIPER_UDE_ABI_MINOR;
    output->Header.Size = sizeof(*output);
    output->ClientNonce = fileContext->ClientNonce;
    output->DriverNonce = fileContext->DriverNonce;
    output->Capabilities = VIIPER_UDE_CAP_ISOCHRONOUS | VIIPER_UDE_CAP_DEVICE_LIFECYCLE;
    output->MaxDevices = VIIPER_UDE_MAX_DEVICES;
    output->MaxDescriptorBytes = VIIPER_UDE_MAX_DESCRIPTOR_BYTES;
    output->MaxTransferBytes = VIIPER_UDE_MAX_TRANSFER_BYTES;
    output->MaxIsoPackets = VIIPER_UDE_MAX_ISO_PACKETS;
    output->MaxPendingOperations = VIIPER_UDE_MAX_PENDING_OPERATIONS;
    WdfRequestSetInformation(Request, sizeof(*output));
    return STATUS_SUCCESS;
}

static
NTSTATUS
ViiperHandleQueryStats(
    _In_ WDFQUEUE Queue,
    _In_ WDFREQUEST Request
    )
{
    NTSTATUS status;
    VIIPER_UDE_STATS *output;
    VIIPER_UDE_CONTROLLER_CONTEXT *context;
    VIIPER_UDE_FILE_CONTEXT *fileContext;
    WDFFILEOBJECT fileObject = WdfRequestGetFileObject(Request);

    if (fileObject == WDF_NO_HANDLE) {
        return STATUS_INVALID_HANDLE;
    }
    fileContext = ViiperGetFileContext(fileObject);
    if (!fileContext->Negotiated || fileContext->Closing) {
        return STATUS_INVALID_DEVICE_STATE;
    }
    status = WdfRequestRetrieveOutputBuffer(
        Request, sizeof(*output), (PVOID *)&output, NULL);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    context = ViiperGetControllerContext(WdfIoQueueGetDevice(Queue));
    RtlZeroMemory(output, sizeof(*output));
    output->Header.Magic = VIIPER_UDE_MAGIC;
    output->Header.Major = VIIPER_UDE_ABI_MAJOR;
    output->Header.Minor = VIIPER_UDE_ABI_MINOR;
    output->Header.Size = sizeof(*output);
    output->OperationsDequeued = (ULONGLONG)ViiperReadCounter(&context->OperationsDequeued);
    output->OperationsCompleted = (ULONGLONG)ViiperReadCounter(&context->OperationsCompleted);
    output->OperationsCancelled = (ULONGLONG)ViiperReadCounter(&context->OperationsCancelled);
    output->OperationsPurged = (ULONGLONG)ViiperReadCounter(&context->OperationsPurged);
    output->LateCompletions = (ULONGLONG)ViiperReadCounter(&context->LateCompletions);
    output->InvalidMessages = (ULONGLONG)ViiperReadCounter(&context->InvalidMessages);
    output->QueueExhaustions = (ULONGLONG)ViiperReadCounter(&context->QueueExhaustions);
    output->IsoPackets = (ULONGLONG)ViiperReadCounter(&context->IsoPackets);
    output->BytesToDevice = (ULONGLONG)ViiperReadCounter(&context->BytesToDevice);
    output->BytesFromDevice = (ULONGLONG)ViiperReadCounter(&context->BytesFromDevice);
    output->NotificationEvents = (ULONGLONG)ViiperReadCounter(&context->NotificationEventsDelivered);
    output->NotificationEventOverflows =
        (ULONGLONG)ViiperReadCounter(&context->NotificationEventOverflows);
    output->ActiveDevices = (ULONG)InterlockedCompareExchange(&context->ActiveDevices, 0, 0);
    output->PendingOperations = (ULONG)InterlockedCompareExchange(&context->PendingOperations, 0, 0);
    output->WaitingDequeues = (ULONG)InterlockedCompareExchange(&context->WaitingDequeueCount, 0, 0);
    output->CleanupRetries = (ULONG)InterlockedCompareExchange(&context->CleanupRetries, 0, 0);
    WdfRequestSetInformation(Request, sizeof(*output));
    return STATUS_SUCCESS;
}

VOID
ViiperEvtIoDeviceControl(
    _In_ WDFQUEUE Queue,
    _In_ WDFREQUEST Request,
    _In_ size_t OutputBufferLength,
    _In_ size_t InputBufferLength,
    _In_ ULONG IoControlCode
    )
{
    NTSTATUS status;

    UNREFERENCED_PARAMETER(OutputBufferLength);
    UNREFERENCED_PARAMETER(InputBufferLength);

    switch (IoControlCode) {
    case IOCTL_VIIPER_UDE_NEGOTIATE:
        status = ViiperHandleNegotiate(Request);
        break;
    case IOCTL_VIIPER_UDE_QUERY_STATS:
        status = ViiperHandleQueryStats(Queue, Request);
        break;
    case IOCTL_VIIPER_UDE_CREATE_DEVICE:
        status = ViiperCreateVirtualDevice(Queue, Request);
        break;
    case IOCTL_VIIPER_UDE_DESTROY_DEVICE:
        status = ViiperDestroyVirtualDevice(Queue, Request);
        break;
    case IOCTL_VIIPER_UDE_DEQUEUE_OPERATION:
        status = ViiperQueueDequeueOperation(Queue, Request);
        break;
    case IOCTL_VIIPER_UDE_COMPLETE_OPERATION:
        status = ViiperCompleteOperation(Queue, Request);
        break;
    default:
        status = UdecxWdfDeviceTryHandleUserIoctl(WdfIoQueueGetDevice(Queue), Request)
            ? STATUS_PENDING
            : STATUS_INVALID_DEVICE_REQUEST;
        break;
    }

    if (status != STATUS_PENDING) {
        WdfRequestComplete(Request, status);
    }
}

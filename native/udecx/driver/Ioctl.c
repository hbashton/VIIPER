#include "ViiperUde.h"
#include "ViiperUdeBuildIdentity.g.h"

static
BOOLEAN
ViiperValidateHeader(
    _In_ const VIIPER_UDE_HEADER *Header,
    _In_ size_t BufferLength,
    _In_ size_t ExpectedSize
    )
{
    return BufferLength == ExpectedSize &&
        Header->Magic == VIIPER_UDE_MAGIC &&
        Header->Major == VIIPER_UDE_ABI_MAJOR &&
        Header->Minor == VIIPER_UDE_ABI_MINOR &&
        Header->Flags == 0 &&
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
    if (outputLength < sizeof(*output)) {
        return STATUS_BUFFER_TOO_SMALL;
    }
    if (inputLength != sizeof(*input) ||
        input->Header.Magic != VIIPER_UDE_MAGIC ||
        input->Header.Flags != 0 ||
        input->Header.Size != sizeof(*input) ||
        input->ClientNonce == 0 || input->Reserved != 0 ||
        (input->RequestedCapabilities & ~(VIIPER_UDE_CAP_ISOCHRONOUS |
            VIIPER_UDE_CAP_STREAMS | VIIPER_UDE_CAP_DEVICE_LIFECYCLE |
            VIIPER_UDE_CAP_INPUT_REPORTS | VIIPER_UDE_CAP_LIFECYCLE_TRACE)) != 0) {
        return STATUS_INVALID_PARAMETER;
    }
    if (input->Header.Major != VIIPER_UDE_ABI_MAJOR ||
        input->Header.Minor != VIIPER_UDE_ABI_MINOR) {
        return STATUS_REVISION_MISMATCH;
    }

    fileObject = WdfRequestGetFileObject(Request);
    if (fileObject == WDF_NO_HANDLE) {
        return STATUS_INVALID_HANDLE;
    }
    fileContext = ViiperGetFileContext(fileObject);
    if (InterlockedCompareExchange(&fileContext->Closing, 0, 0) != 0) {
        return STATUS_FILE_CLOSED;
    }
    if (InterlockedCompareExchange(&fileContext->Negotiated, 0, 0) != 0 &&
        fileContext->ClientNonce != input->ClientNonce) {
        return STATUS_INVALID_DEVICE_STATE;
    }

    if (InterlockedCompareExchange(&fileContext->Negotiated, 0, 0) == 0) {
        ticks = KeQueryPerformanceCounter(NULL);
        fileContext->ClientNonce = input->ClientNonce;
        fileContext->DriverNonce = ((ULONGLONG)ticks.QuadPart) ^
            ((ULONGLONG)(ULONG_PTR)fileObject << 13) ^ input->ClientNonce;
        if (fileContext->DriverNonce == 0) {
            fileContext->DriverNonce = 1;
        }
        InterlockedExchange(&fileContext->Negotiated, TRUE);
    }

    RtlZeroMemory(output, sizeof(*output));
    output->Header.Magic = VIIPER_UDE_MAGIC;
    output->Header.Major = VIIPER_UDE_ABI_MAJOR;
    output->Header.Minor = VIIPER_UDE_ABI_MINOR;
    output->Header.Size = sizeof(*output);
    output->ClientNonce = fileContext->ClientNonce;
    output->DriverNonce = fileContext->DriverNonce;
    output->Capabilities = VIIPER_UDE_ADVERTISED_CAPABILITIES;
    output->MaxDevices = VIIPER_UDE_MAX_DEVICES;
    output->MaxDescriptorBytes = VIIPER_UDE_MAX_DESCRIPTOR_BYTES;
    output->MaxTransferBytes = VIIPER_UDE_MAX_TRANSFER_BYTES;
    output->MaxIsoPackets = VIIPER_UDE_MAX_ISO_PACKETS;
    output->MaxPendingOperations = VIIPER_UDE_MAX_PENDING_OPERATIONS;
    RtlCopyMemory(output->BuildIdentity, ViiperUdeBuildIdentity,
        sizeof(output->BuildIdentity));
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
    if (InterlockedCompareExchange(&fileContext->Negotiated, 0, 0) == 0 ||
        InterlockedCompareExchange(&fileContext->Closing, 0, 0) != 0) {
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
    output->InputReportsSubmitted =
        (ULONGLONG)ViiperReadCounter(&context->InputReportsSubmitted);
    output->InputReportsCompleted =
        (ULONGLONG)ViiperReadCounter(&context->InputReportsCompleted);
    WdfRequestSetInformation(Request, sizeof(*output));
    return STATUS_SUCCESS;
}

static
NTSTATUS
ViiperHandleQueryLifecycleTrace(
    _In_ WDFQUEUE Queue,
    _In_ WDFREQUEST Request
    )
{
    NTSTATUS status;
    VIIPER_UDE_LIFECYCLE_TRACE *output;
    VIIPER_UDE_CONTROLLER_CONTEXT *context;
    VIIPER_UDE_FILE_CONTEXT *fileContext;
    WDFFILEOBJECT fileObject = WdfRequestGetFileObject(Request);
    LARGE_INTEGER frequency;
    ULONGLONG latestSequence;
    ULONGLONG firstSequence;
    ULONGLONG sequence;

    if (fileObject == WDF_NO_HANDLE) {
        return STATUS_INVALID_HANDLE;
    }
    fileContext = ViiperGetFileContext(fileObject);
    if (InterlockedCompareExchange(&fileContext->Negotiated, 0, 0) == 0 ||
        InterlockedCompareExchange(&fileContext->Closing, 0, 0) != 0) {
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
    output->RecordSize = sizeof(output->Records[0]);
    output->Capacity = VIIPER_UDE_LIFECYCLE_TRACE_CAPACITY;
    (VOID)KeQueryPerformanceCounter(&frequency);
    output->PerformanceFrequency = (ULONGLONG)frequency.QuadPart;

    latestSequence = (ULONGLONG)ViiperReadCounter(
        &context->LifecycleTraceSequence);
    output->LatestSequence = latestSequence;
    firstSequence = latestSequence > VIIPER_UDE_LIFECYCLE_TRACE_CAPACITY
        ? latestSequence - VIIPER_UDE_LIFECYCLE_TRACE_CAPACITY + 1
        : 1;
    for (sequence = firstSequence; sequence <= latestSequence; ++sequence) {
        VIIPER_UDE_LIFECYCLE_TRACE_RECORD *source =
            &context->LifecycleTrace[
                (sequence - 1) % VIIPER_UDE_LIFECYCLE_TRACE_CAPACITY];
        ULONGLONG publishedBefore = (ULONGLONG)InterlockedCompareExchange64(
            (volatile LONG64 *)&source->PublishedSequence, 0, 0);
        ULONGLONG publishedAfter;

        if (publishedBefore != sequence) {
            continue;
        }
        KeMemoryBarrier();
        RtlCopyMemory(
            &output->Records[output->RecordCount], source, sizeof(*source));
        KeMemoryBarrier();
        publishedAfter = (ULONGLONG)InterlockedCompareExchange64(
            (volatile LONG64 *)&source->PublishedSequence, 0, 0);
        if (publishedAfter == sequence &&
            output->Records[output->RecordCount].PublishedSequence == sequence) {
            ++output->RecordCount;
        }
    }

    WdfRequestSetInformation(Request, sizeof(*output));
    return STATUS_SUCCESS;
}

VOID
ViiperEvtIoDeviceControlRoute(
    _In_ WDFQUEUE Queue,
    _In_ WDFREQUEST Request,
    _In_ size_t OutputBufferLength,
    _In_ size_t InputBufferLength,
    _In_ ULONG IoControlCode
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *context =
        ViiperGetControllerContext(WdfIoQueueGetDevice(Queue));
    NTSTATUS status;

    UNREFERENCED_PARAMETER(OutputBufferLength);
    UNREFERENCED_PARAMETER(InputBufferLength);

    if (InterlockedCompareExchange(&context->ShuttingDown, 0, 0) != 0) {
        WdfRequestComplete(Request, STATUS_DEVICE_REMOVED);
        return;
    }

    if (IoControlCode == IOCTL_VIIPER_UDE_SUBMIT_INPUT_REPORT) {
        // The default queue already has parallel/passive/no-synchronization
        // semantics. Complete the hot interrupt-IN submission here instead of
        // forwarding it through a second identically configured KMDF queue.
        // Control, lifecycle, and media IOCTLs still move to the serialized
        // control queue and therefore cannot head-of-line block fresh input.
        status = ViiperSubmitInputReport(Queue, Request);
        WdfRequestComplete(Request, status);
        return;
    }

    status = WdfRequestForwardToIoQueue(Request, context->ControlQueue);
    if (!NT_SUCCESS(status)) {
        WdfRequestComplete(Request, status);
    }
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
    VIIPER_UDE_CONTROLLER_CONTEXT *context =
        ViiperGetControllerContext(WdfIoQueueGetDevice(Queue));
    NTSTATUS status;

    UNREFERENCED_PARAMETER(OutputBufferLength);
    UNREFERENCED_PARAMETER(InputBufferLength);

    if (InterlockedCompareExchange(&context->ShuttingDown, 0, 0) != 0) {
        WdfRequestComplete(Request, STATUS_DEVICE_REMOVED);
        return;
    }

    switch (IoControlCode) {
    case IOCTL_VIIPER_UDE_NEGOTIATE:
        status = ViiperHandleNegotiate(Request);
        break;
    case IOCTL_VIIPER_UDE_QUERY_STATS:
        status = ViiperHandleQueryStats(Queue, Request);
        break;
    case IOCTL_VIIPER_UDE_QUERY_LIFECYCLE_TRACE:
        status = ViiperHandleQueryLifecycleTrace(Queue, Request);
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
    case IOCTL_VIIPER_UDE_SUBMIT_INPUT_REPORT:
        // The parallel default queue completes this hot-path IOCTL directly.
        // Reject it here rather than silently restoring head-of-line blocking.
        status = STATUS_INVALID_DEVICE_REQUEST;
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

#include <intrin.h>

#include "ViiperUde.h"

#pragma intrinsic(_ReturnAddress)

#ifdef ALLOC_PRAGMA
#pragma alloc_text(PAGE, ViiperInitializeLifecycleTrace)
#endif

NTSTATUS
ViiperInitializeLifecycleTrace(
    _In_ WDFDEVICE Controller
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext;
    WDF_OBJECT_ATTRIBUTES attributes;
    ULONG maximumProcessors;
    ULONG shardCount;
    SIZE_T storageSize;
    PVOID rawStorage;
    ULONG_PTR alignedStorage;
    NTSTATUS status;

    PAGED_CODE();
    controllerContext = ViiperGetControllerContext(Controller);
    maximumProcessors = KeQueryMaximumProcessorCountEx(ALL_PROCESSOR_GROUPS);
    if (maximumProcessors == 0) {
        return STATUS_DEVICE_CONFIGURATION_ERROR;
    }
    shardCount = maximumProcessors > VIIPER_UDE_LIFECYCLE_TRACE_MAX_SHARDS
        ? VIIPER_UDE_LIFECYCLE_TRACE_MAX_SHARDS
        : maximumProcessors;
    storageSize = sizeof(VIIPER_UDE_LIFECYCLE_TRACE_SHARD) * shardCount +
        SYSTEM_CACHE_ALIGNMENT_SIZE - 1U;

    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = Controller;
    status = WdfMemoryCreate(
        &attributes,
        NonPagedPoolNx,
        0x56495554,
        storageSize,
        &controllerContext->LifecycleTraceStorage,
        &rawStorage);
    if (!NT_SUCCESS(status)) {
        controllerContext->LifecycleTraceStorage = WDF_NO_HANDLE;
        return status;
    }
    RtlZeroMemory(rawStorage, storageSize);
    alignedStorage = ((ULONG_PTR)rawStorage + SYSTEM_CACHE_ALIGNMENT_SIZE - 1U) &
        ~((ULONG_PTR)SYSTEM_CACHE_ALIGNMENT_SIZE - 1U);
    controllerContext->LifecycleTraceShards =
        (VIIPER_UDE_LIFECYCLE_TRACE_SHARD *)alignedStorage;
    controllerContext->LifecycleTraceShardCount = shardCount;
    return STATUS_SUCCESS;
}

VOID
ViiperTraceLifecycle(
    _In_ WDFDEVICE Controller,
    _In_ UCHAR Source,
    _In_ USHORT Event,
    _In_ ULONGLONG DeviceId,
    _In_ ULONG Generation,
    _In_opt_ UDECXUSBDEVICE Device,
    _In_opt_ UDECXUSBENDPOINT Endpoint,
    _In_ UCHAR EndpointAddress,
    _In_ NTSTATUS Status,
    _In_ LONG ActiveOperations,
    _In_ ULONG QueueState,
    _In_ ULONG Line
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext;
    VIIPER_UDE_LIFECYCLE_TRACE_SHARD *shard;
    VIIPER_UDE_LIFECYCLE_TRACE_RECORD *record;
    PROCESSOR_NUMBER processorNumber;
    LARGE_INTEGER timestamp;
    ULONGLONG localSequence;
    ULONGLONG sequence;
    volatile LONG64 *slotState;
    LONG64 observedSlotState;
    LONG64 claimedSlotState;
    ULONG processorIndex;
    ULONG shardIndex;
    ULONG slotIndex;

    controllerContext = ViiperGetControllerContext(Controller);
    if (Event >= VIIPER_UDE_TRACE_ENDPOINT_QUIESCENCE_WATCHDOG &&
        Event <= VIIPER_UDE_TRACE_OWNER_RUNDOWN_WATCHDOG) {
        (VOID)InterlockedOr(
            &controllerContext->LifecycleTraceStatus,
            VIIPER_UDE_LIFECYCLE_TRACE_STATUS_WATCHDOG_FIRED);
    }
    if (controllerContext->LifecycleTraceShards == NULL ||
        controllerContext->LifecycleTraceShardCount == 0) {
        return;
    }
    KeGetCurrentProcessorNumberEx(&processorNumber);
    processorIndex = KeGetProcessorIndexFromNumber(&processorNumber);
    if (processorIndex == INVALID_PROCESSOR_INDEX) {
        processorIndex = processorNumber.Group * MAXIMUM_PROC_PER_GROUP +
            processorNumber.Number;
    }
    shardIndex = processorIndex % controllerContext->LifecycleTraceShardCount;
    shard = &controllerContext->LifecycleTraceShards[shardIndex];
    localSequence = (ULONGLONG)InterlockedIncrement64(&shard->WriteSequence);
    slotIndex = (ULONG)(
        (localSequence - 1) % VIIPER_UDE_LIFECYCLE_TRACE_CAPACITY);
    slotState = &shard->SlotStates[slotIndex];
    claimedSlotState = (LONG64)((localSequence << 1) | 1ULL);
    for (;;) {
        observedSlotState = InterlockedCompareExchange64(slotState, 0, 0);
        if ((observedSlotState & 1) != 0 ||
            ((ULONGLONG)observedSlotState >> 1) >= localSequence) {
            (VOID)InterlockedOr(
                &controllerContext->LifecycleTraceStatus,
                VIIPER_UDE_LIFECYCLE_TRACE_STATUS_DROPPED_RECORD);
            return;
        }
        if (InterlockedCompareExchange64(
                slotState, claimedSlotState, observedSlotState) ==
            observedSlotState) {
            break;
        }
    }
    sequence = (ULONGLONG)InterlockedIncrement64(
        &controllerContext->LifecycleTraceSequence);
    record = &shard->Records[slotIndex];

    (VOID)InterlockedExchange64((volatile LONG64 *)&record->PublishedSequence, 0);
    KeMemoryBarrier();

    timestamp = KeQueryPerformanceCounter(NULL);
    record->TimestampQpc = (ULONGLONG)timestamp.QuadPart;
    record->Caller = (ULONGLONG)(ULONG_PTR)_ReturnAddress();
    record->DeviceId = DeviceId;
    record->DeviceObject = (ULONGLONG)(ULONG_PTR)Device;
    record->EndpointObject = (ULONGLONG)(ULONG_PTR)Endpoint;
    record->Generation = Generation;
    record->Line = Line;
    record->Status = Status;
    record->ActiveOperations = ActiveOperations;
    record->PendingOperations = InterlockedCompareExchange(
        &controllerContext->PendingOperations, 0, 0);
    record->QueueState = QueueState;
    record->Event = Event;
    record->Processor = (VIIPER_UDE_UINT16)(
        processorNumber.Group * MAXIMUM_PROC_PER_GROUP + processorNumber.Number);
    record->Source = Source;
    record->Irql = (VIIPER_UDE_UINT8)KeGetCurrentIrql();
    record->EndpointAddress = EndpointAddress;
    record->Reserved = 0;

    KeMemoryBarrier();
    (VOID)InterlockedExchange64(
        (volatile LONG64 *)&record->PublishedSequence, (LONG64)sequence);
    KeMemoryBarrier();
    (VOID)InterlockedExchange64(slotState, (LONG64)(localSequence << 1));
}

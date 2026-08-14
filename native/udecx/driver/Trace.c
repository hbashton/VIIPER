#include <intrin.h>

#include "ViiperUde.h"

#pragma intrinsic(_ReturnAddress)

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
    VIIPER_UDE_LIFECYCLE_TRACE_RECORD *record;
    PROCESSOR_NUMBER processorNumber;
    LARGE_INTEGER timestamp;
    ULONGLONG sequence;

    controllerContext = ViiperGetControllerContext(Controller);
    sequence = (ULONGLONG)InterlockedIncrement64(
        &controllerContext->LifecycleTraceSequence);
    record = &controllerContext->LifecycleTrace[
        (sequence - 1) % VIIPER_UDE_LIFECYCLE_TRACE_CAPACITY];

    (VOID)InterlockedExchange64((volatile LONG64 *)&record->PublishedSequence, 0);
    KeMemoryBarrier();

    timestamp = KeQueryPerformanceCounter(NULL);
    KeGetCurrentProcessorNumberEx(&processorNumber);
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
}

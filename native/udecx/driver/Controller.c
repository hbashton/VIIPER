#include <initguid.h>
#include "ViiperUde.h"

DEFINE_GUID(
    GUID_DEVINTERFACE_VIIPER_UDE,
    VIIPER_UDE_INTERFACE_GUID_DATA1,
    VIIPER_UDE_INTERFACE_GUID_DATA2,
    VIIPER_UDE_INTERFACE_GUID_DATA3,
    VIIPER_UDE_INTERFACE_GUID_DATA4_0,
    VIIPER_UDE_INTERFACE_GUID_DATA4_1,
    VIIPER_UDE_INTERFACE_GUID_DATA4_2,
    VIIPER_UDE_INTERFACE_GUID_DATA4_3,
    VIIPER_UDE_INTERFACE_GUID_DATA4_4,
    VIIPER_UDE_INTERFACE_GUID_DATA4_5,
    VIIPER_UDE_INTERFACE_GUID_DATA4_6,
    VIIPER_UDE_INTERFACE_GUID_DATA4_7);

#ifdef ALLOC_PRAGMA
#pragma alloc_text(PAGE, ViiperEvtDeviceAdd)
#pragma alloc_text(PAGE, ViiperEvtDeviceSelfManagedIoInit)
#pragma alloc_text(PAGE, ViiperEvtDeviceSelfManagedIoCleanup)
#pragma alloc_text(PAGE, ViiperEvtFileCreate)
#pragma alloc_text(PAGE, ViiperEvtFileCleanup)
#pragma alloc_text(PAGE, ViiperCreateQueues)
#endif

static
BOOLEAN
ViiperFinishOwnerCleanup(
    _In_ WDFDEVICE Device,
    _In_ WDFFILEOBJECT OwnerFile
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *context = ViiperGetControllerContext(Device);
    BOOLEAN releaseOwner = FALSE;

    PAGED_CODE();
    if (InterlockedCompareExchange(&context->ShuttingDown, 0, 0) != 0) {
        return FALSE;
    }
    WdfWaitLockAcquire(context->OwnerLock, NULL);
    if (context->OwnerFile != OwnerFile || !context->CleanupInProgress) {
        WdfWaitLockRelease(context->OwnerLock);
        return TRUE;
    }
    WdfWaitLockRelease(context->OwnerLock);

    // EvtFileCleanup can run while a create/destroy IOCTL is still dispatched.
    // Closing and CleanupInProgress prevent a successor from entering; join
    // only those finite UdeCx API calls here. Child EvtCleanup is deliberately
    // not part of this rundown because PlugOutAndDelete consumes its handle
    // before KMDF necessarily destroys the object.
    (VOID)KeWaitForSingleObject(
        &context->OwnerAdmissionsDrained,
        Executive,
        KernelMode,
        FALSE,
        NULL);
    if (InterlockedCompareExchange(&context->ShuttingDown, 0, 0) != 0) {
        return FALSE;
    }

    if (!ViiperDestroyOwnedDevices(Device, OwnerFile)) {
        return FALSE;
    }

    WdfWaitLockAcquire(context->OwnerLock, NULL);
    if (context->OwnerFile == OwnerFile && context->CleanupInProgress) {
        context->OwnerFile = WDF_NO_HANDLE;
        context->CleanupInProgress = FALSE;
        releaseOwner = InterlockedExchange(&context->OwnerReferenced, FALSE) != FALSE;
    }
    WdfWaitLockRelease(context->OwnerLock);
    if (releaseOwner) {
        WdfObjectDereference(OwnerFile);
    }
    return TRUE;
}

NTSTATUS
ViiperEvtQueryUsbCapability(
    _In_ WDFDEVICE UdecxWdfDevice,
    _In_ GUID *CapabilityType,
    _In_ ULONG OutputBufferLength,
    _Out_writes_to_opt_(OutputBufferLength, *ResultLength) PVOID OutputBuffer,
    _Out_ PULONG ResultLength
    )
{
    UNREFERENCED_PARAMETER(UdecxWdfDevice);
    UNREFERENCED_PARAMETER(OutputBufferLength);
    UNREFERENCED_PARAMETER(OutputBuffer);

    *ResultLength = 0;
    if (RtlEqualMemory(CapabilityType, &GUID_USB_CAPABILITY_CHAINED_MDLS, sizeof(GUID)) ||
        RtlEqualMemory(CapabilityType, &GUID_USB_CAPABILITY_SELECTIVE_SUSPEND, sizeof(GUID)) ||
        RtlEqualMemory(
            CapabilityType,
            &GUID_USB_CAPABILITY_DEVICE_CONNECTION_HIGH_SPEED_COMPATIBLE,
            sizeof(GUID)) ||
        RtlEqualMemory(
            CapabilityType,
            &GUID_USB_CAPABILITY_DEVICE_CONNECTION_SUPER_SPEED_COMPATIBLE,
            sizeof(GUID))) {
        return STATUS_SUCCESS;
    }

    return STATUS_NOT_SUPPORTED;
}

NTSTATUS
ViiperEvtDeviceAdd(
    _In_ WDFDRIVER Driver,
    _Inout_ PWDFDEVICE_INIT DeviceInit
    )
{
    NTSTATUS status;
    WDFDEVICE device;
    WDF_OBJECT_ATTRIBUTES attributes;
    WDF_OBJECT_ATTRIBUTES fileAttributes;
    WDF_OBJECT_ATTRIBUTES requestAttributes;
    WDF_FILEOBJECT_CONFIG fileConfig;
    UDECX_WDF_DEVICE_CONFIG udeConfig;
    WDF_PNPPOWER_EVENT_CALLBACKS pnpCallbacks;
    VIIPER_UDE_CONTROLLER_CONTEXT *context;
    UNICODE_STRING sddl = RTL_CONSTANT_STRING(L"D:P(A;;GA;;;SY)(A;;GA;;;BA)");
    UNICODE_STRING brokerReference;

    PAGED_CODE();
    UNREFERENCED_PARAMETER(Driver);

    WdfDeviceInitSetCharacteristics(DeviceInit, FILE_DEVICE_SECURE_OPEN, FALSE);
    status = WdfDeviceInitAssignSDDLString(DeviceInit, &sddl);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    WDF_FILEOBJECT_CONFIG_INIT(
        &fileConfig,
        ViiperEvtFileCreate,
        WDF_NO_EVENT_CALLBACK,
        ViiperEvtFileCleanup);
    WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(&fileAttributes, VIIPER_UDE_FILE_CONTEXT);
    fileAttributes.ExecutionLevel = WdfExecutionLevelPassive;
    WdfDeviceInitSetFileObjectConfig(DeviceInit, &fileConfig, &fileAttributes);

    WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(&requestAttributes, VIIPER_UDE_REQUEST_CONTEXT);
    WdfDeviceInitSetRequestAttributes(DeviceInit, &requestAttributes);

    WDF_PNPPOWER_EVENT_CALLBACKS_INIT(&pnpCallbacks);
    pnpCallbacks.EvtDeviceSelfManagedIoInit = ViiperEvtDeviceSelfManagedIoInit;
    pnpCallbacks.EvtDeviceSelfManagedIoCleanup = ViiperEvtDeviceSelfManagedIoCleanup;
    WdfDeviceInitSetPnpPowerEventCallbacks(DeviceInit, &pnpCallbacks);

    status = UdecxInitializeWdfDeviceInit(DeviceInit);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(&attributes, VIIPER_UDE_CONTROLLER_CONTEXT);
    attributes.EvtCleanupCallback = ViiperEvtControllerCleanup;
    status = WdfDeviceCreate(&DeviceInit, &attributes, &device);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    context = ViiperGetControllerContext(device);
    RtlZeroMemory(context, sizeof(*context));
    ExInitializePushLock(&context->DeviceLock);
    InitializeListHead(&context->CompletionQueue);
    KeInitializeEvent(&context->BrokerOperationsDrained, NotificationEvent, TRUE);
    KeInitializeEvent(&context->CompletionOperationsDrained, NotificationEvent, TRUE);
    KeInitializeEvent(&context->OwnerAdmissionsDrained, NotificationEvent, TRUE);
    KeInitializeEvent(&context->FileCleanupsDrained, NotificationEvent, TRUE);

    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = device;
    status = WdfWaitLockCreate(&attributes, &context->OwnerLock);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    status = ViiperInitializeBroker(device);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    RtlInitUnicodeString(&brokerReference, VIIPER_UDE_BROKER_REFERENCE_STRING);
    status = WdfDeviceCreateDeviceInterface(
        device, &GUID_DEVINTERFACE_VIIPER_UDE, &brokerReference);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    status = WdfDeviceCreateDeviceInterface(
        device,
        (LPGUID)&GUID_DEVINTERFACE_USB_HOST_CONTROLLER,
        NULL);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    UDECX_WDF_DEVICE_CONFIG_INIT(&udeConfig, ViiperEvtQueryUsbCapability);
    udeConfig.NumberOfUsb20Ports = (USHORT)VIIPER_UDE_MAX_DEVICES;
    udeConfig.NumberOfUsb30Ports = (USHORT)VIIPER_UDE_MAX_DEVICES;
    status = UdecxWdfDeviceAddUsbDeviceEmulation(device, &udeConfig);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    return ViiperCreateQueues(device);
}

VOID
ViiperEvtControllerCleanup(
    _In_ WDFOBJECT ControllerObject
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *context;

    context = ViiperGetControllerContext((WDFDEVICE)ControllerObject);
    // Every active operation belongs in SelfManagedIoCleanup, while the
    // controller's child queues, locks, memory objects, and work item are
    // still callable. WDF invokes child cleanup before parent cleanup, so this
    // callback is deliberately limited to invariant checks over context data.
    NT_ASSERT(InterlockedCompareExchange(&context->PendingOperations, 0, 0) == 0);
    NT_ASSERT(InterlockedCompareExchange(&context->PendingCompletions, 0, 0) == 0);
    NT_ASSERT(IsListEmpty(&context->CompletionQueue));
    NT_ASSERT(!context->CompletionDpcActive);
    NT_ASSERT(InterlockedCompareExchange(&context->ActiveOwnerAdmissions, 0, 0) == 0);
    NT_ASSERT(InterlockedCompareExchange(&context->ActiveFileCleanups, 0, 0) == 0);
    NT_ASSERT(InterlockedCompareExchange(&context->ActiveDevices, 0, 0) == 0);
    NT_ASSERT(InterlockedCompareExchange(&context->OwnerReferenced, 0, 0) == 0);
    NT_ASSERT(context->InputDeviceCount == 0);
}

NTSTATUS
ViiperEvtDeviceSelfManagedIoInit(
    _In_ WDFDEVICE Device
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *context = ViiperGetControllerContext(Device);

    PAGED_CODE();
    InterlockedExchange(&context->ShuttingDown, FALSE);
    return STATUS_SUCCESS;
}

VOID
ViiperEvtDeviceSelfManagedIoCleanup(
    _In_ WDFDEVICE Device
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *context = ViiperGetControllerContext(Device);
    WDFFILEOBJECT ownerFile = WDF_NO_HANDLE;
    BOOLEAN releaseOwner = FALSE;

    PAGED_CODE();

    // Close every user/UdeCx admission path before draining work that already
    // crossed the boundary. Interlocked operations also provide the ordering
    // barrier consumed by the queue and broker callbacks.
    InterlockedExchange(&context->ShuttingDown, TRUE);

    if (context->OwnerLock != WDF_NO_HANDLE) {
        WdfWaitLockAcquire(context->OwnerLock, NULL);
        ownerFile = context->OwnerFile;
        if (ownerFile != WDF_NO_HANDLE) {
            InterlockedExchange(&ViiperGetFileContext(ownerFile)->Closing, TRUE);
            context->CleanupInProgress = TRUE;
        }
        WdfWaitLockRelease(context->OwnerLock);
    }

    // A file cleanup that crossed OwnerLock before ShuttingDown may still be
    // using the controller's queue and lock children. The gate prevents any
    // successor, so this event is a finite rundown join before those objects
    // are purged. A cleanup that reaches OwnerLock after the gate never enters.
    (VOID)KeWaitForSingleObject(
        &context->FileCleanupsDrained,
        Executive,
        KernelMode,
        FALSE,
        NULL);

    // These queues are non-power-managed. KMDF purges them before this
    // callback on normal removal, but an explicit idempotent purge also covers
    // initialization failure and documents the driver's teardown boundary.
    if (context->DefaultQueue != WDF_NO_HANDLE) {
        WdfIoQueuePurgeSynchronously(context->DefaultQueue);
    }
    if (context->ControlQueue != WDF_NO_HANDLE) {
        WdfIoQueuePurgeSynchronously(context->ControlQueue);
    }
    if (context->WaitingDequeues != WDF_NO_HANDLE) {
        WdfIoQueuePurgeSynchronously(context->WaitingDequeues);
        InterlockedExchange(&context->WaitingDequeueCount, 0);
    }
    // Create/destroy owner admissions execute on ControlQueue and therefore
    // must have returned before its synchronous purge completes.
    NT_ASSERT(InterlockedCompareExchange(&context->ActiveOwnerAdmissions, 0, 0) == 0);

    ViiperPurgeOwnerOperations(Device, STATUS_DEVICE_REMOVED);
    if (context->CompletionDpc != WDF_NO_HANDLE) {
        if (InterlockedCompareExchange(&context->PendingOperations, 0, 0) != 0) {
            (VOID)KeWaitForSingleObject(
                &context->BrokerOperationsDrained,
                Executive,
                KernelMode,
                FALSE,
                NULL);
        }
        // BrokerOperationsDrained covers tracked slots. The second join also
        // covers rejected and fast-input URBs, then cancels/joins the reusable
        // DPC only after its intrusive request list is empty.
        ViiperDrainUrbCompletions(Device);
    }

    if (context->BrokerLock != WDF_NO_HANDLE) {
        WdfSpinLockAcquire(context->BrokerLock);
        context->NotificationHead = 0;
        context->NotificationTail = 0;
        context->NotificationCount = 0;
        InterlockedExchange(&context->BrokerFaulted, FALSE);
        WdfSpinLockRelease(context->BrokerLock);
    }

    // PlugOutAndDelete owns asynchronous UdeCx cleanup. Do not wait here: a
    // synchronous wait can deadlock the same PnP/UdeCx worker that must deliver
    // the endpoint and device cleanup callbacks.
    ViiperBeginControllerShutdown(Device);

    if (context->OwnerLock != WDF_NO_HANDLE) {
        WdfWaitLockAcquire(context->OwnerLock, NULL);
        if (context->OwnerFile == ownerFile) {
            context->OwnerFile = WDF_NO_HANDLE;
        }
        context->CleanupInProgress = FALSE;
        releaseOwner = InterlockedExchange(&context->OwnerReferenced, FALSE) != FALSE;
        WdfWaitLockRelease(context->OwnerLock);
    }
    if (releaseOwner && ownerFile != WDF_NO_HANDLE) {
        WdfObjectDereference(ownerFile);
    }
}

VOID
ViiperEvtFileCreate(
    _In_ WDFDEVICE Device,
    _In_ WDFREQUEST Request,
    _In_ WDFFILEOBJECT FileObject
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *context;
    VIIPER_UDE_FILE_CONTEXT *fileContext;
    PUNICODE_STRING fileName;
    UNICODE_STRING brokerReference;
    BOOLEAN isBrokerClient = FALSE;
    NTSTATUS status = STATUS_SUCCESS;

    PAGED_CODE();
    context = ViiperGetControllerContext(Device);
    fileContext = ViiperGetFileContext(FileObject);
    RtlZeroMemory(fileContext, sizeof(*fileContext));

    fileName = WdfFileObjectGetFileName(FileObject);
    RtlInitUnicodeString(&brokerReference, VIIPER_UDE_BROKER_REFERENCE_STRING);
    if (fileName != NULL &&
        fileName->Length == brokerReference.Length + sizeof(WCHAR) &&
        fileName->Buffer[0] == L'\\' &&
        RtlEqualMemory(
            fileName->Buffer + 1,
            brokerReference.Buffer,
            brokerReference.Length)) {
        isBrokerClient = TRUE;
    }

    if (!isBrokerClient) {
        WdfRequestComplete(Request, STATUS_SUCCESS);
        return;
    }

    WdfWaitLockAcquire(context->OwnerLock, NULL);
    if (InterlockedCompareExchange(&context->ShuttingDown, 0, 0) != 0) {
        status = STATUS_DEVICE_REMOVED;
    } else if (context->OwnerFile != WDF_NO_HANDLE || context->CleanupInProgress) {
        status = STATUS_SHARING_VIOLATION;
    } else {
        InterlockedExchange(&fileContext->BrokerOwner, TRUE);
        WdfObjectReference(FileObject);
        InterlockedExchange(&context->OwnerReferenced, TRUE);
        context->OwnerFile = FileObject;
        InterlockedExchange(&context->BrokerFaulted, FALSE);
        WdfIoQueueStart(context->WaitingDequeues);
    }
    WdfWaitLockRelease(context->OwnerLock);
    WdfRequestComplete(Request, status);
}

VOID
ViiperEvtFileCleanup(
    _In_ WDFFILEOBJECT FileObject
    )
{
    WDFDEVICE device;
    VIIPER_UDE_CONTROLLER_CONTEXT *context;
    VIIPER_UDE_FILE_CONTEXT *fileContext;
    BOOLEAN ownsController = FALSE;
    BOOLEAN cleanupAdmitted = FALSE;
    LONG remainingCleanups;

    PAGED_CODE();
    device = WdfFileObjectGetDevice(FileObject);
    context = ViiperGetControllerContext(device);
    fileContext = ViiperGetFileContext(FileObject);
    InterlockedExchange(&fileContext->Closing, TRUE);

    // Self-managed cleanup owns controller-wide rundown once this gate closes.
    // In particular, do not reach through sibling WDF lock/queue children from
    // a file cleanup callback that can outlive their normal I/O lifetime.
    if (InterlockedCompareExchange(&context->ShuttingDown, 0, 0) != 0) {
        return;
    }
    if (InterlockedCompareExchange(&fileContext->BrokerOwner, 0, 0) == 0) {
        return;
    }

    WdfWaitLockAcquire(context->OwnerLock, NULL);
    if (InterlockedCompareExchange(&context->ShuttingDown, 0, 0) == 0 &&
        context->OwnerFile == FileObject) {
        if (InterlockedCompareExchange(&context->ActiveFileCleanups, 0, 0) == 0) {
            KeClearEvent(&context->FileCleanupsDrained);
        }
        (VOID)InterlockedIncrement(&context->ActiveFileCleanups);
        context->CleanupInProgress = TRUE;
        ownsController = TRUE;
        cleanupAdmitted = TRUE;
    }
    WdfWaitLockRelease(context->OwnerLock);

    if (ownsController) {
        ViiperPurgeOwnerOperations(device, STATUS_FILE_CLOSED);
        if (context->WaitingDequeues != WDF_NO_HANDLE) {
            WdfIoQueuePurgeSynchronously(context->WaitingDequeues);
            InterlockedExchange(&context->WaitingDequeueCount, 0);
        }
        WdfSpinLockAcquire(context->BrokerLock);
        context->NotificationHead = 0;
        context->NotificationTail = 0;
        context->NotificationCount = 0;
        WdfSpinLockRelease(context->BrokerLock);
    }
    if (ownsController) {
        (VOID)ViiperFinishOwnerCleanup(device, FileObject);
    }
    if (cleanupAdmitted) {
        WdfWaitLockAcquire(context->OwnerLock, NULL);
        remainingCleanups = InterlockedDecrement(&context->ActiveFileCleanups);
        NT_ASSERT(remainingCleanups >= 0);
        if (remainingCleanups == 0) {
            KeSetEvent(&context->FileCleanupsDrained, IO_NO_INCREMENT, FALSE);
        }
        WdfWaitLockRelease(context->OwnerLock);
    }
}

NTSTATUS
ViiperCreateQueues(
    _In_ WDFDEVICE Device
    )
{
    NTSTATUS status;
    WDF_IO_QUEUE_CONFIG queueConfig;
    WDF_OBJECT_ATTRIBUTES attributes;
    VIIPER_UDE_CONTROLLER_CONTEXT *context = ViiperGetControllerContext(Device);

    PAGED_CODE();
    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = Device;
    attributes.ExecutionLevel = WdfExecutionLevelPassive;
    attributes.SynchronizationScope = WdfSynchronizationScopeNone;

    WDF_IO_QUEUE_CONFIG_INIT_DEFAULT_QUEUE(&queueConfig, WdfIoQueueDispatchParallel);
    queueConfig.PowerManaged = WdfFalse;
    queueConfig.EvtIoDeviceControl = ViiperEvtIoDeviceControlRoute;
    status = WdfIoQueueCreate(Device, &queueConfig, &attributes, &context->DefaultQueue);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    WDF_IO_QUEUE_CONFIG_INIT(&queueConfig, WdfIoQueueDispatchSequential);
    queueConfig.PowerManaged = WdfFalse;
    queueConfig.EvtIoDeviceControl = ViiperEvtIoDeviceControl;
    status = WdfIoQueueCreate(Device, &queueConfig, &attributes, &context->ControlQueue);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    WDF_IO_QUEUE_CONFIG_INIT(&queueConfig, WdfIoQueueDispatchManual);
    queueConfig.PowerManaged = WdfFalse;
    // Overlapped dequeue IOCTLs are routinely cancelled when a host worker is
    // retired.  KMDF removes those requests from a manual queue without a
    // retrieve call, so account for that ownership path explicitly instead of
    // leaving WaitingDequeueCount permanently inflated for the owner session.
    queueConfig.EvtIoCanceledOnQueue = ViiperEvtDequeueCanceledOnQueue;
    return WdfIoQueueCreate(Device, &queueConfig, &attributes, &context->WaitingDequeues);
}

VOID
ViiperEvtDequeueCanceledOnQueue(
    _In_ WDFQUEUE Queue,
    _In_ WDFREQUEST Request
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *context =
        ViiperGetControllerContext(WdfIoQueueGetDevice(Queue));
    LONG remaining = InterlockedDecrement(&context->WaitingDequeueCount);

    NT_ASSERT(remaining >= 0);
    UNREFERENCED_PARAMETER(remaining);
    WdfRequestComplete(Request, STATUS_CANCELLED);
}

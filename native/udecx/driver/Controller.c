#include <initguid.h>
#include "ViiperUde.h"

DEFINE_GUID(
    GUID_DEVINTERFACE_VIIPER_UDE,
    0x32d03f48, 0x725b, 0x4baa, 0x97, 0x0f, 0x7f, 0x5d, 0xe6, 0xc4, 0x46, 0x87);

#ifdef ALLOC_PRAGMA
#pragma alloc_text(PAGE, ViiperEvtDeviceAdd)
#pragma alloc_text(PAGE, ViiperEvtControllerCleanup)
#pragma alloc_text(PAGE, ViiperEvtFileCreate)
#pragma alloc_text(PAGE, ViiperEvtFileCleanup)
#pragma alloc_text(PAGE, ViiperEvtOwnerCleanupRetry)
#pragma alloc_text(PAGE, ViiperCreateQueues)
#endif

#define VIIPER_OWNER_CLEANUP_RETRY_MS 100

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

VOID
ViiperEvtOwnerCleanupRetry(
    _In_ WDFTIMER Timer
    )
{
    WDFDEVICE device = (WDFDEVICE)WdfTimerGetParentObject(Timer);
    VIIPER_UDE_CONTROLLER_CONTEXT *context = ViiperGetControllerContext(device);
    WDFFILEOBJECT ownerFile = WDF_NO_HANDLE;

    PAGED_CODE();
    WdfWaitLockAcquire(context->OwnerLock, NULL);
    if (context->CleanupInProgress && context->OwnerFile != WDF_NO_HANDLE) {
        ownerFile = context->OwnerFile;
    }
    WdfWaitLockRelease(context->OwnerLock);
    if (ownerFile == WDF_NO_HANDLE || ViiperFinishOwnerCleanup(device, ownerFile)) {
        return;
    }

    InterlockedIncrement(&context->CleanupRetries);
    (VOID)WdfTimerStart(Timer, WDF_REL_TIMEOUT_IN_MS(VIIPER_OWNER_CLEANUP_RETRY_MS));
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
    WDF_TIMER_CONFIG timerConfig;
    UDECX_WDF_DEVICE_CONFIG udeConfig;
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
    WdfDeviceInitSetFileObjectConfig(DeviceInit, &fileConfig, &fileAttributes);

    WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(&requestAttributes, VIIPER_UDE_REQUEST_CONTEXT);
    WdfDeviceInitSetRequestAttributes(DeviceInit, &requestAttributes);

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

    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = device;
    status = WdfWaitLockCreate(&attributes, &context->OwnerLock);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    status = WdfWaitLockCreate(&attributes, &context->DeviceLock);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    status = ViiperInitializeBroker(device);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    WDF_TIMER_CONFIG_INIT(&timerConfig, ViiperEvtOwnerCleanupRetry);
    timerConfig.AutomaticSerialization = FALSE;
    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = device;
    attributes.ExecutionLevel = WdfExecutionLevelPassive;
    status = WdfTimerCreate(&timerConfig, &attributes, &context->OwnerCleanupTimer);
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

    PAGED_CODE();
    context = ViiperGetControllerContext((WDFDEVICE)ControllerObject);
    if (context->OwnerCleanupTimer != WDF_NO_HANDLE) {
        WdfTimerStop(context->OwnerCleanupTimer, TRUE);
    }
    ViiperPurgeOwnerOperations((WDFDEVICE)ControllerObject, STATUS_DEVICE_REMOVED);
    if (context->DefaultQueue != WDF_NO_HANDLE) {
        WdfIoQueuePurgeSynchronously(context->DefaultQueue);
    }
    if (context->ControlQueue != WDF_NO_HANDLE) {
        WdfIoQueuePurgeSynchronously(context->ControlQueue);
    }
    if (context->InputQueue != WDF_NO_HANDLE) {
        WdfIoQueuePurgeSynchronously(context->InputQueue);
    }
    if (context->WaitingDequeues != WDF_NO_HANDLE) {
        WdfIoQueuePurgeSynchronously(context->WaitingDequeues);
        InterlockedExchange(&context->WaitingDequeueCount, 0);
    }
    if (context->BrokerLock != WDF_NO_HANDLE) {
        WdfSpinLockAcquire(context->BrokerLock);
        context->NotificationHead = 0;
        context->NotificationTail = 0;
        context->NotificationCount = 0;
        InterlockedExchange(&context->BrokerFaulted, FALSE);
        WdfSpinLockRelease(context->BrokerLock);
    }
    if (context->OwnerLock != WDF_NO_HANDLE) {
        WDFFILEOBJECT ownerFile = WDF_NO_HANDLE;
        BOOLEAN releaseOwner = FALSE;

        WdfWaitLockAcquire(context->OwnerLock, NULL);
        ownerFile = context->OwnerFile;
        context->OwnerFile = WDF_NO_HANDLE;
        context->CleanupInProgress = FALSE;
        releaseOwner = InterlockedExchange(&context->OwnerReferenced, FALSE) != FALSE;
        WdfWaitLockRelease(context->OwnerLock);
        if (releaseOwner && ownerFile != WDF_NO_HANDLE) {
            WdfObjectDereference(ownerFile);
        }
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
    if (context->OwnerFile != WDF_NO_HANDLE || context->CleanupInProgress) {
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

    PAGED_CODE();
    device = WdfFileObjectGetDevice(FileObject);
    context = ViiperGetControllerContext(device);
    fileContext = ViiperGetFileContext(FileObject);
    InterlockedExchange(&fileContext->Closing, TRUE);

    if (InterlockedCompareExchange(&fileContext->BrokerOwner, 0, 0) == 0) {
        return;
    }

    WdfWaitLockAcquire(context->OwnerLock, NULL);
    if (context->OwnerFile == FileObject) {
        context->CleanupInProgress = TRUE;
        ownsController = TRUE;
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
        if (!ViiperFinishOwnerCleanup(device, FileObject)) {
            InterlockedIncrement(&context->CleanupRetries);
            (VOID)WdfTimerStart(
                context->OwnerCleanupTimer,
                WDF_REL_TIMEOUT_IN_MS(VIIPER_OWNER_CLEANUP_RETRY_MS));
        }
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

    WDF_IO_QUEUE_CONFIG_INIT(&queueConfig, WdfIoQueueDispatchParallel);
    queueConfig.PowerManaged = WdfFalse;
    queueConfig.EvtIoDeviceControl = ViiperEvtInputIoDeviceControl;
    status = WdfIoQueueCreate(Device, &queueConfig, &attributes, &context->InputQueue);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    WDF_IO_QUEUE_CONFIG_INIT(&queueConfig, WdfIoQueueDispatchManual);
    queueConfig.PowerManaged = WdfFalse;
    return WdfIoQueueCreate(Device, &queueConfig, &attributes, &context->WaitingDequeues);
}

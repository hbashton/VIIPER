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
#pragma alloc_text(PAGE, ViiperCreateQueues)
#endif

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
    if (IsEqualGUIDAligned(*CapabilityType, GUID_USB_CAPABILITY_CHAINED_MDLS) ||
        IsEqualGUIDAligned(*CapabilityType, GUID_USB_CAPABILITY_SELECTIVE_SUSPEND) ||
        IsEqualGUIDAligned(*CapabilityType, GUID_USB_CAPABILITY_DEVICE_CONNECTION_HIGH_SPEED_COMPATIBLE)) {
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
    WDF_FILEOBJECT_CONFIG fileConfig;
    UDECX_WDF_DEVICE_CONFIG udeConfig;
    VIIPER_UDE_CONTROLLER_CONTEXT *context;
    UNICODE_STRING sddl = RTL_CONSTANT_STRING(L"D:P(A;;GA;;;SY)(A;;GA;;;BA)");

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

    status = WdfDeviceCreateDeviceInterface(device, &GUID_DEVINTERFACE_VIIPER_UDE, NULL);
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
    if (context->DefaultQueue != WDF_NO_HANDLE) {
        WdfIoQueuePurgeSynchronously(context->DefaultQueue);
    }
    if (context->WaitingDequeues != WDF_NO_HANDLE) {
        WdfIoQueuePurgeSynchronously(context->WaitingDequeues);
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
    NTSTATUS status = STATUS_SUCCESS;

    PAGED_CODE();
    context = ViiperGetControllerContext(Device);
    WdfWaitLockAcquire(context->OwnerLock, NULL);
    if (context->OwnerFile != WDF_NO_HANDLE || context->CleanupInProgress) {
        status = STATUS_SHARING_VIOLATION;
    } else {
        fileContext = ViiperGetFileContext(FileObject);
        RtlZeroMemory(fileContext, sizeof(*fileContext));
        context->OwnerFile = FileObject;
        WdfIoQueueStart(context->DefaultQueue);
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
    fileContext->Closing = TRUE;

    WdfWaitLockAcquire(context->OwnerLock, NULL);
    if (context->OwnerFile == FileObject) {
        context->CleanupInProgress = TRUE;
        ownsController = TRUE;
    }
    WdfWaitLockRelease(context->OwnerLock);

    if (ownsController) {
        if (context->DefaultQueue != WDF_NO_HANDLE) {
            WdfIoQueuePurgeSynchronously(context->DefaultQueue);
        }
        if (context->WaitingDequeues != WDF_NO_HANDLE) {
            WdfIoQueuePurgeSynchronously(context->WaitingDequeues);
        }
    }
    if (ownsController) {
        ViiperDestroyOwnedDevices(device, FileObject);
    }

    if (ownsController) {
        WdfWaitLockAcquire(context->OwnerLock, NULL);
        context->OwnerFile = WDF_NO_HANDLE;
        context->CleanupInProgress = FALSE;
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

    WDF_IO_QUEUE_CONFIG_INIT_DEFAULT_QUEUE(&queueConfig, WdfIoQueueDispatchSequential);
    queueConfig.PowerManaged = WdfFalse;
    queueConfig.EvtIoDeviceControl = ViiperEvtIoDeviceControl;
    status = WdfIoQueueCreate(Device, &queueConfig, &attributes, &context->DefaultQueue);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    WDF_IO_QUEUE_CONFIG_INIT(&queueConfig, WdfIoQueueDispatchManual);
    queueConfig.PowerManaged = WdfFalse;
    return WdfIoQueueCreate(Device, &queueConfig, &attributes, &context->WaitingDequeues);
}

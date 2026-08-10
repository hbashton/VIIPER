/*
 * Dynamic UdeCx device and endpoint lifecycle.
 *
 * The endpoint creation and purge order follows the documented UdeCx contract
 * and the permissively licensed usbip-win2 implementation identified in
 * THIRD_PARTY_NOTICES.md. VIIPER-specific ownership, identity, and broker
 * semantics are implemented here.
 */

#include "ViiperUde.h"

#ifdef ALLOC_PRAGMA
#pragma alloc_text(PAGE, ViiperCreateVirtualDevice)
#pragma alloc_text(PAGE, ViiperDestroyVirtualDevice)
#pragma alloc_text(PAGE, ViiperDestroyOwnedDevices)
#pragma alloc_text(PAGE, ViiperEvtEndpointAdd)
#pragma alloc_text(PAGE, ViiperEvtDefaultEndpointAdd)
#pragma alloc_text(PAGE, ViiperEvtVirtualDeviceCleanup)
#endif

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
BOOLEAN
ViiperValidateCreateDevice(
    _In_reads_bytes_(InputLength) const VIIPER_UDE_CREATE_DEVICE *Input,
    _In_ size_t InputLength
    )
{
    const VIIPER_UDE_DESCRIPTOR_RECORD *records;
    ULONG recordsLength;
    ULONG index;
    BOOLEAN foundDevice = FALSE;
    BOOLEAN foundConfiguration = FALSE;

    if (InputLength < sizeof(*Input) ||
        InputLength > (size_t)VIIPER_UDE_MAX_DESCRIPTOR_BYTES * 2 + sizeof(*Input) ||
        Input->Header.Magic != VIIPER_UDE_MAGIC ||
        Input->Header.Major != VIIPER_UDE_ABI_MAJOR ||
        Input->Header.Size != InputLength ||
        Input->DeviceId == 0 || Input->Generation == 0 ||
        Input->DescriptorCount == 0 ||
        Input->DescriptorCount > VIIPER_UDE_MAX_DESCRIPTOR_BYTES / sizeof(*records) ||
        Input->DescriptorDataLength == 0 ||
        Input->DescriptorDataLength > VIIPER_UDE_MAX_DESCRIPTOR_BYTES ||
        Input->MaxPendingOperations == 0 ||
        Input->MaxPendingOperations > VIIPER_UDE_MAX_PENDING_OPERATIONS) {
        return FALSE;
    }

    if (Input->DescriptorCount > MAXULONG / sizeof(*records)) {
        return FALSE;
    }
    recordsLength = Input->DescriptorCount * sizeof(*records);
    if (!ViiperRangeValid(Input->DescriptorRecordsOffset, recordsLength, Input->Header.Size) ||
        !ViiperRangeValid(Input->DescriptorDataOffset, Input->DescriptorDataLength, Input->Header.Size)) {
        return FALSE;
    }

    records = (const VIIPER_UDE_DESCRIPTOR_RECORD *)
        ((const UCHAR *)Input + Input->DescriptorRecordsOffset);
    for (index = 0; index < Input->DescriptorCount; ++index) {
        const VIIPER_UDE_DESCRIPTOR_RECORD *record = &records[index];
        if (!ViiperRangeValid(record->Offset, record->Length, Input->DescriptorDataLength)) {
            return FALSE;
        }
        if (record->Kind == ViiperUdeDescriptorDevice && record->Length >= sizeof(USB_DEVICE_DESCRIPTOR)) {
            foundDevice = TRUE;
        }
        if (record->Kind == ViiperUdeDescriptorConfiguration && record->Length >= sizeof(USB_CONFIGURATION_DESCRIPTOR)) {
            foundConfiguration = TRUE;
        }
    }

    return foundDevice && foundConfiguration;
}

static
NTSTATUS
ViiperValidateOwner(
    _In_ WDFDEVICE Controller,
    _In_ WDFREQUEST Request,
    _Out_ WDFFILEOBJECT *OwnerFile
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(Controller);
    VIIPER_UDE_FILE_CONTEXT *fileContext;
    WDFFILEOBJECT fileObject = WdfRequestGetFileObject(Request);
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
    if (NT_SUCCESS(status)) {
        *OwnerFile = fileObject;
    }
    return status;
}

static
UDECX_USB_DEVICE_SPEED
ViiperMapSpeed(
    _In_ ULONG Speed
    )
{
    switch (Speed) {
    case 1:
        return UdecxUsbLowSpeed;
    case 2:
        return UdecxUsbFullSpeed;
    case 3:
        return UdecxUsbHighSpeed;
    case 4:
        return UdecxUsbSuperSpeed;
    default:
        return (UDECX_USB_DEVICE_SPEED)0;
    }
}

static
NTSTATUS
ViiperClaimDeviceSlot(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ UDECXUSBDEVICE Device,
    _In_ ULONGLONG DeviceId,
    _Out_ ULONG *Slot
    )
{
    ULONG index;
    ULONG freeSlot = VIIPER_UDE_MAX_DEVICES;
    NTSTATUS status = STATUS_INSUFFICIENT_RESOURCES;

    WdfWaitLockAcquire(ControllerContext->DeviceLock, NULL);
    for (index = 0; index < VIIPER_UDE_MAX_DEVICES; ++index) {
        UDECXUSBDEVICE current = ControllerContext->Devices[index];
        if (current == WDF_NO_HANDLE) {
            if (freeSlot == VIIPER_UDE_MAX_DEVICES) {
                freeSlot = index;
            }
            continue;
        }
        if (ViiperGetDeviceContext(current)->DeviceId == DeviceId) {
            status = STATUS_OBJECT_NAME_COLLISION;
            goto Exit;
        }
    }
    if (freeSlot != VIIPER_UDE_MAX_DEVICES) {
        ControllerContext->Devices[freeSlot] = Device;
        *Slot = freeSlot;
        status = STATUS_SUCCESS;
    }

Exit:
    WdfWaitLockRelease(ControllerContext->DeviceLock);
    return status;
}

static
VOID
ViiperReleaseDeviceSlot(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ UDECXUSBDEVICE Device,
    _In_ ULONG Slot
    )
{
    WdfWaitLockAcquire(ControllerContext->DeviceLock, NULL);
    if (Slot < VIIPER_UDE_MAX_DEVICES && ControllerContext->Devices[Slot] == Device) {
        ControllerContext->Devices[Slot] = WDF_NO_HANDLE;
    }
    WdfWaitLockRelease(ControllerContext->DeviceLock);
}

NTSTATUS
ViiperCreateVirtualDevice(
    _In_ WDFQUEUE Queue,
    _In_ WDFREQUEST Request
    )
{
    NTSTATUS status;
    WDFDEVICE controller = WdfIoQueueGetDevice(Queue);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(controller);
    VIIPER_UDE_CREATE_DEVICE *input;
    size_t inputLength;
    WDFFILEOBJECT ownerFile;
    PUDECXUSBDEVICE_INIT deviceInit;
    UDECX_USB_DEVICE_STATE_CHANGE_CALLBACKS callbacks;
    UDECX_USB_DEVICE_SPEED speed;
    WDF_OBJECT_ATTRIBUTES attributes;
    UDECXUSBDEVICE device = WDF_NO_HANDLE;
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext;
    UDECX_USB_DEVICE_PLUG_IN_OPTIONS plugOptions;
    ULONG slot;

    PAGED_CODE();
    status = ViiperValidateOwner(controller, Request, &ownerFile);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    status = WdfRequestRetrieveInputBuffer(Request, sizeof(*input), (PVOID *)&input, &inputLength);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    if (!ViiperValidateCreateDevice(input, inputLength)) {
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        return STATUS_INVALID_PARAMETER;
    }
    speed = ViiperMapSpeed(input->Speed);
    if (speed == (UDECX_USB_DEVICE_SPEED)0) {
        return STATUS_NOT_SUPPORTED;
    }

    deviceInit = UdecxUsbDeviceInitAllocate(controller);
    if (deviceInit == NULL) {
        return STATUS_INSUFFICIENT_RESOURCES;
    }

    UDECX_USB_DEVICE_CALLBACKS_INIT(&callbacks);
    callbacks.EvtUsbDeviceLinkPowerEntry = ViiperEvtUsbDeviceD0Entry;
    callbacks.EvtUsbDeviceLinkPowerExit = ViiperEvtUsbDeviceD0Exit;
    if (speed == UdecxUsbSuperSpeed) {
        callbacks.EvtUsbDeviceSetFunctionSuspendAndWake =
            ViiperEvtUsbDeviceSetFunctionSuspendAndWake;
    }
    callbacks.EvtUsbDeviceDefaultEndpointAdd = ViiperEvtDefaultEndpointAdd;
    callbacks.EvtUsbDeviceEndpointAdd = ViiperEvtEndpointAdd;
    callbacks.EvtUsbDeviceEndpointsConfigure = ViiperEvtEndpointsConfigure;
    UdecxUsbDeviceInitSetStateChangeCallbacks(deviceInit, &callbacks);
    UdecxUsbDeviceInitSetSpeed(deviceInit, speed);
    UdecxUsbDeviceInitSetEndpointsType(deviceInit, UdecxEndpointTypeDynamic);

    WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(&attributes, VIIPER_UDE_DEVICE_CONTEXT);
    attributes.ParentObject = controller;
    attributes.EvtCleanupCallback = ViiperEvtVirtualDeviceCleanup;
    status = UdecxUsbDeviceCreate(&deviceInit, &attributes, &device);
    if (!NT_SUCCESS(status)) {
        UdecxUsbDeviceInitFree(deviceInit);
        return status;
    }

    deviceContext = ViiperGetDeviceContext(device);
    RtlZeroMemory(deviceContext, sizeof(*deviceContext));
    deviceContext->Controller = controller;
    deviceContext->OwnerFile = ownerFile;
    deviceContext->DeviceId = input->DeviceId;
    deviceContext->Generation = input->Generation;
    deviceContext->Slot = VIIPER_UDE_MAX_DEVICES;

    status = ViiperClaimDeviceSlot(controllerContext, device, input->DeviceId, &slot);
    if (!NT_SUCCESS(status)) {
        WdfObjectDelete(device);
        return status;
    }
    deviceContext->Slot = slot;

    UDECX_USB_DEVICE_PLUG_IN_OPTIONS_INIT(&plugOptions);
    if (speed == UdecxUsbSuperSpeed) {
        plugOptions.Usb30PortNumber = slot + 1;
    } else {
        plugOptions.Usb20PortNumber = slot + 1;
    }
    status = UdecxUsbDevicePlugIn(device, &plugOptions);
    if (!NT_SUCCESS(status)) {
        ViiperReleaseDeviceSlot(controllerContext, device, slot);
        WdfObjectDelete(device);
        return status;
    }

    deviceContext->Plugged = TRUE;
    InterlockedIncrement(&controllerContext->ActiveDevices);
    WdfRequestSetInformation(Request, 0);
    return STATUS_SUCCESS;
}

static
UDECXUSBDEVICE
ViiperTakeDevice(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ WDFFILEOBJECT OwnerFile,
    _In_ ULONGLONG DeviceId,
    _In_ ULONG Generation,
    _In_ BOOLEAN MatchGeneration
    )
{
    UDECXUSBDEVICE found = WDF_NO_HANDLE;
    ULONG index;

    WdfWaitLockAcquire(ControllerContext->DeviceLock, NULL);
    for (index = 0; index < VIIPER_UDE_MAX_DEVICES; ++index) {
        UDECXUSBDEVICE current = ControllerContext->Devices[index];
        VIIPER_UDE_DEVICE_CONTEXT *deviceContext;
        if (current == WDF_NO_HANDLE) {
            continue;
        }
        deviceContext = ViiperGetDeviceContext(current);
        if (deviceContext->OwnerFile != OwnerFile || deviceContext->DeviceId != DeviceId ||
            (MatchGeneration && deviceContext->Generation != Generation)) {
            continue;
        }
        deviceContext->Purging = TRUE;
        ControllerContext->Devices[index] = WDF_NO_HANDLE;
        found = current;
        break;
    }
    WdfWaitLockRelease(ControllerContext->DeviceLock);
    return found;
}

NTSTATUS
ViiperDestroyVirtualDevice(
    _In_ WDFQUEUE Queue,
    _In_ WDFREQUEST Request
    )
{
    NTSTATUS status;
    WDFDEVICE controller = WdfIoQueueGetDevice(Queue);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(controller);
    VIIPER_UDE_DEVICE_IDENTITY *input;
    size_t inputLength;
    WDFFILEOBJECT ownerFile;
    UDECXUSBDEVICE device;

    PAGED_CODE();
    status = ViiperValidateOwner(controller, Request, &ownerFile);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    status = WdfRequestRetrieveInputBuffer(Request, sizeof(*input), (PVOID *)&input, &inputLength);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    if (inputLength < sizeof(*input) || input->Header.Magic != VIIPER_UDE_MAGIC ||
        input->Header.Major != VIIPER_UDE_ABI_MAJOR || input->Header.Size != sizeof(*input) ||
        input->DeviceId == 0 || input->Generation == 0) {
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        return STATUS_INVALID_PARAMETER;
    }

    device = ViiperTakeDevice(
        controllerContext, ownerFile, input->DeviceId, input->Generation, TRUE);
    if (device == WDF_NO_HANDLE) {
        return STATUS_NOT_FOUND;
    }
    status = UdecxUsbDevicePlugOutAndDelete(device);
    if (NT_SUCCESS(status)) {
        InterlockedDecrement(&controllerContext->ActiveDevices);
    }
    return status;
}

VOID
ViiperDestroyOwnedDevices(
    _In_ WDFDEVICE Controller,
    _In_ WDFFILEOBJECT OwnerFile
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(Controller);

    PAGED_CODE();
    for (;;) {
        UDECXUSBDEVICE device;
        VIIPER_UDE_DEVICE_CONTEXT *deviceContext;
        ULONGLONG deviceId = 0;
        ULONG index;

        WdfWaitLockAcquire(controllerContext->DeviceLock, NULL);
        for (index = 0; index < VIIPER_UDE_MAX_DEVICES; ++index) {
            device = controllerContext->Devices[index];
            if (device != WDF_NO_HANDLE && ViiperGetDeviceContext(device)->OwnerFile == OwnerFile) {
                deviceId = ViiperGetDeviceContext(device)->DeviceId;
                break;
            }
        }
        WdfWaitLockRelease(controllerContext->DeviceLock);
        if (deviceId == 0) {
            break;
        }

        device = ViiperTakeDevice(controllerContext, OwnerFile, deviceId, 0, FALSE);
        if (device == WDF_NO_HANDLE) {
            continue;
        }
        deviceContext = ViiperGetDeviceContext(device);
        if (deviceContext->Plugged) {
            if (NT_SUCCESS(UdecxUsbDevicePlugOutAndDelete(device))) {
                InterlockedDecrement(&controllerContext->ActiveDevices);
            }
        } else {
            WdfObjectDelete(device);
        }
    }
}

VOID
ViiperEvtVirtualDeviceCleanup(
    _In_ WDFOBJECT DeviceObject
    )
{
    UDECXUSBDEVICE device = (UDECXUSBDEVICE)DeviceObject;
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext;

    PAGED_CODE();
    if (deviceContext->Controller == WDF_NO_HANDLE) {
        return;
    }
    controllerContext = ViiperGetControllerContext(deviceContext->Controller);
    ViiperReleaseDeviceSlot(controllerContext, device, deviceContext->Slot);
}

NTSTATUS
ViiperEvtUsbDeviceD0Entry(
    _In_ WDFDEVICE Controller,
    _In_ UDECXUSBDEVICE Device
    )
{
    UNREFERENCED_PARAMETER(Controller);
    UNREFERENCED_PARAMETER(Device);
    return STATUS_SUCCESS;
}

NTSTATUS
ViiperEvtUsbDeviceD0Exit(
    _In_ WDFDEVICE Controller,
    _In_ UDECXUSBDEVICE Device,
    _In_ UDECX_USB_DEVICE_WAKE_SETTING WakeSetting
    )
{
    UNREFERENCED_PARAMETER(Controller);
    UNREFERENCED_PARAMETER(Device);
    UNREFERENCED_PARAMETER(WakeSetting);
    return STATUS_SUCCESS;
}

NTSTATUS
ViiperEvtUsbDeviceSetFunctionSuspendAndWake(
    _In_ WDFDEVICE Controller,
    _In_ UDECXUSBDEVICE Device,
    _In_ ULONG Interface,
    _In_ UDECX_USB_DEVICE_FUNCTION_POWER FunctionPower
    )
{
    UNREFERENCED_PARAMETER(Controller);
    UNREFERENCED_PARAMETER(Device);
    UNREFERENCED_PARAMETER(Interface);
    UNREFERENCED_PARAMETER(FunctionPower);
    return STATUS_SUCCESS;
}

static
NTSTATUS
ViiperCreateEndpointQueue(
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ WDF_IO_QUEUE_DISPATCH_TYPE DispatchType
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    WDF_IO_QUEUE_CONFIG queueConfig;
    WDF_OBJECT_ATTRIBUTES attributes;
    UDECXUSBENDPOINT *queueEndpoint;
    NTSTATUS status;

    WDF_IO_QUEUE_CONFIG_INIT(&queueConfig, DispatchType);
    queueConfig.PowerManaged = WdfFalse;
    queueConfig.EvtIoInternalDeviceControl = ViiperEvtEndpointIoInternalControl;
    WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(&attributes, UDECXUSBENDPOINT);
    attributes.ParentObject = Endpoint;
    status = WdfIoQueueCreate(deviceContext->Controller, &queueConfig, &attributes, &endpointContext->Queue);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    queueEndpoint = ViiperGetQueueEndpoint(endpointContext->Queue);
    *queueEndpoint = Endpoint;
    UdecxUsbEndpointSetWdfIoQueue(Endpoint, endpointContext->Queue);
    return STATUS_SUCCESS;
}

NTSTATUS
ViiperEvtEndpointAdd(
    _In_ UDECXUSBDEVICE Device,
    _In_ UDECX_USB_ENDPOINT_INIT_AND_METADATA *EndpointData
    )
{
    USB_ENDPOINT_DESCRIPTOR descriptor;
    UDECX_USB_ENDPOINT_CALLBACKS callbacks;
    WDF_OBJECT_ATTRIBUTES attributes;
    UDECXUSBENDPOINT endpoint;
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext;
    WDF_IO_QUEUE_DISPATCH_TYPE dispatchType;
    NTSTATUS status;

    PAGED_CODE();
    RtlZeroMemory(&descriptor, sizeof(descriptor));
    if (EndpointData->EndpointDescriptor != NULL) {
        if (EndpointData->EndpointDescriptorBufferLength < sizeof(USB_ENDPOINT_DESCRIPTOR)) {
            return STATUS_INVALID_PARAMETER;
        }
        RtlCopyMemory(&descriptor, EndpointData->EndpointDescriptor, sizeof(descriptor));
    }
    UdecxUsbEndpointInitSetEndpointAddress(
        EndpointData->UdecxUsbEndpointInit, descriptor.bEndpointAddress);

    UDECX_USB_ENDPOINT_CALLBACKS_INIT(&callbacks, ViiperEvtEndpointReset);
    callbacks.EvtUsbEndpointStart = ViiperEvtEndpointStart;
    callbacks.EvtUsbEndpointPurge = ViiperEvtEndpointPurge;
    UdecxUsbEndpointInitSetCallbacks(EndpointData->UdecxUsbEndpointInit, &callbacks);

    WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(&attributes, VIIPER_UDE_ENDPOINT_CONTEXT);
    attributes.ParentObject = Device;
    status = UdecxUsbEndpointCreate(&EndpointData->UdecxUsbEndpointInit, &attributes, &endpoint);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    endpointContext = ViiperGetEndpointContext(endpoint);
    RtlZeroMemory(endpointContext, sizeof(*endpointContext));
    endpointContext->Device = Device;
    endpointContext->Descriptor = descriptor;
    if (descriptor.bEndpointAddress == 0) {
        ViiperGetDeviceContext(Device)->DefaultEndpoint = endpoint;
        dispatchType = WdfIoQueueDispatchSequential;
    } else {
        dispatchType = WdfIoQueueDispatchParallel;
    }
    return ViiperCreateEndpointQueue(endpoint, dispatchType);
}

NTSTATUS
ViiperEvtDefaultEndpointAdd(
    _In_ UDECXUSBDEVICE Device,
    _In_ PUDECXUSBENDPOINT_INIT EndpointInit
    )
{
    UDECX_USB_ENDPOINT_INIT_AND_METADATA endpointData;

    PAGED_CODE();
    RtlZeroMemory(&endpointData, sizeof(endpointData));
    endpointData.UdecxUsbEndpointInit = EndpointInit;
    return ViiperEvtEndpointAdd(Device, &endpointData);
}

VOID
ViiperEvtEndpointReset(
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ WDFREQUEST Request
    )
{
    UNREFERENCED_PARAMETER(Endpoint);
    WdfRequestComplete(Request, STATUS_SUCCESS);
}

VOID
ViiperEvtEndpointQueuePurged(
    _In_ WDFQUEUE Queue,
    _In_ WDFCONTEXT Context
    )
{
    UDECXUSBENDPOINT endpoint = (UDECXUSBENDPOINT)Context;
    UNREFERENCED_PARAMETER(Queue);
    ViiperGetEndpointContext(endpoint)->Purging = FALSE;
    UdecxUsbEndpointPurgeComplete(endpoint);
}

VOID
ViiperEvtEndpointPurge(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    endpointContext->Purging = TRUE;
    WdfIoQueuePurge(endpointContext->Queue, ViiperEvtEndpointQueuePurged, Endpoint);
}

VOID
ViiperEvtEndpointStart(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    WdfIoQueueStart(ViiperGetEndpointContext(Endpoint)->Queue);
}

VOID
ViiperEvtEndpointsConfigure(
    _In_ UDECXUSBDEVICE Device,
    _In_ WDFREQUEST Request,
    _In_ UDECX_ENDPOINTS_CONFIGURE_PARAMS *ConfigureParams
    )
{
    UNREFERENCED_PARAMETER(Device);
    UNREFERENCED_PARAMETER(ConfigureParams);
    WdfRequestComplete(Request, STATUS_SUCCESS);
}

VOID
ViiperEvtEndpointIoInternalControl(
    _In_ WDFQUEUE Queue,
    _In_ WDFREQUEST Request,
    _In_ size_t OutputBufferLength,
    _In_ size_t InputBufferLength,
    _In_ ULONG IoControlCode
    )
{
    UNREFERENCED_PARAMETER(Queue);
    UNREFERENCED_PARAMETER(OutputBufferLength);
    UNREFERENCED_PARAMETER(InputBufferLength);
    if (IoControlCode == IOCTL_INTERNAL_USB_SUBMIT_URB) {
        UdecxUrbCompleteWithNtStatus(Request, STATUS_NOT_SUPPORTED);
    } else {
        WdfRequestComplete(Request, STATUS_INVALID_DEVICE_REQUEST);
    }
}

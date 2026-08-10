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
#pragma alloc_text(PAGE, ViiperEvtEndpointCleanup)
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
ViiperValidateDescriptorChain(
    _In_reads_bytes_(Length) const UCHAR *Descriptor,
    _In_ ULONG Length,
    _In_ UCHAR ExpectedType
    )
{
    ULONG offset = 0;

    if (Length < 2 || Descriptor[1] != ExpectedType) {
        return FALSE;
    }
    while (offset < Length) {
        ULONG itemLength;
        if (Length - offset < 2) {
            return FALSE;
        }
        itemLength = Descriptor[offset];
        if (itemLength < 2 || itemLength > Length - offset) {
            return FALSE;
        }
        offset += itemLength;
    }
    return offset == Length;
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
    BOOLEAN foundBos = FALSE;
    BOOLEAN foundLanguageTable = FALSE;
    BOOLEAN foundLocalizedString = FALSE;

    if (InputLength < sizeof(*Input) ||
        InputLength > (size_t)VIIPER_UDE_MAX_DESCRIPTOR_BYTES * 2 + sizeof(*Input) ||
        Input->Header.Magic != VIIPER_UDE_MAGIC ||
        Input->Header.Major != VIIPER_UDE_ABI_MAJOR ||
        Input->Header.Minor != VIIPER_UDE_ABI_MINOR ||
        Input->Header.Flags != 0 ||
        Input->Header.Size != InputLength ||
        Input->DeviceId == 0 || Input->Generation == 0 ||
        Input->Speed < 1 || Input->Speed > 4 ||
        Input->DescriptorCount == 0 ||
        Input->DescriptorCount > VIIPER_UDE_MAX_DESCRIPTOR_BYTES / sizeof(*records) ||
        Input->DescriptorDataLength == 0 ||
        Input->DescriptorDataLength > VIIPER_UDE_MAX_DESCRIPTOR_BYTES ||
        Input->MaxPendingOperations == 0 ||
        Input->MaxPendingOperations > VIIPER_UDE_MAX_PENDING_OPERATIONS ||
        Input->Reserved != 0) {
        return FALSE;
    }

    if (Input->DescriptorCount > MAXULONG / sizeof(*records)) {
        return FALSE;
    }
    recordsLength = Input->DescriptorCount * sizeof(*records);
    if (Input->DescriptorRecordsOffset < sizeof(*Input) ||
        !ViiperRangeValid(Input->DescriptorRecordsOffset, recordsLength, Input->Header.Size) ||
        !ViiperRangeValid(Input->DescriptorDataOffset, Input->DescriptorDataLength, Input->Header.Size) ||
        Input->DescriptorDataOffset < Input->DescriptorRecordsOffset ||
        Input->DescriptorDataOffset - Input->DescriptorRecordsOffset < recordsLength ||
        Input->DescriptorDataOffset + Input->DescriptorDataLength != Input->Header.Size) {
        return FALSE;
    }

    records = (const VIIPER_UDE_DESCRIPTOR_RECORD *)
        ((const UCHAR *)Input + Input->DescriptorRecordsOffset);
    for (index = 0; index < Input->DescriptorCount; ++index) {
        const VIIPER_UDE_DESCRIPTOR_RECORD *record = &records[index];
        const UCHAR *descriptor;
        if (record->Length < 2 || record->Length > MAXUSHORT ||
            record->Reserved != 0 ||
            !ViiperRangeValid(record->Offset, record->Length, Input->DescriptorDataLength)) {
            return FALSE;
        }
        descriptor = (const UCHAR *)Input + Input->DescriptorDataOffset + record->Offset;
        switch (record->Kind) {
        case ViiperUdeDescriptorDevice:
            if (foundDevice || record->Index != 0 ||
                record->Length != sizeof(USB_DEVICE_DESCRIPTOR) ||
                descriptor[0] != sizeof(USB_DEVICE_DESCRIPTOR) ||
                descriptor[1] != USB_DEVICE_DESCRIPTOR_TYPE) {
                return FALSE;
            }
            foundDevice = TRUE;
            break;
        case ViiperUdeDescriptorConfiguration:
            if (foundConfiguration || record->Index != 0 ||
                record->Length < sizeof(USB_CONFIGURATION_DESCRIPTOR) ||
                descriptor[0] != sizeof(USB_CONFIGURATION_DESCRIPTOR) ||
                descriptor[1] != USB_CONFIGURATION_DESCRIPTOR_TYPE ||
                ((USHORT)descriptor[2] | ((USHORT)descriptor[3] << 8)) != (USHORT)record->Length ||
                !ViiperValidateDescriptorChain(
                    descriptor, record->Length, USB_CONFIGURATION_DESCRIPTOR_TYPE)) {
                return FALSE;
            }
            foundConfiguration = TRUE;
            break;
        case ViiperUdeDescriptorBos:
            if (foundBos || record->Index != 0 ||
                record->Length < sizeof(USB_BOS_DESCRIPTOR) ||
                descriptor[0] != sizeof(USB_BOS_DESCRIPTOR) ||
                descriptor[1] != USB_BOS_DESCRIPTOR_TYPE ||
                ((USHORT)descriptor[2] | ((USHORT)descriptor[3] << 8)) != (USHORT)record->Length ||
                !ViiperValidateDescriptorChain(
                    descriptor, record->Length, USB_BOS_DESCRIPTOR_TYPE)) {
                return FALSE;
            }
            foundBos = TRUE;
            break;
        case ViiperUdeDescriptorString:
            if (record->Index > MAXUCHAR || record->Length > MAXUCHAR ||
                descriptor[0] != record->Length || descriptor[1] != USB_STRING_DESCRIPTOR_TYPE ||
                (record->Length & 1) != 0 ||
                (record->Index == 0 && record->LanguageId != 0) ||
                (record->Index == 0 && record->Length < 4) ||
                (record->Index != 0 && record->LanguageId == 0)) {
                return FALSE;
            }
            if (record->Index == 0) {
                if (foundLanguageTable) {
                    return FALSE;
                }
                foundLanguageTable = TRUE;
            } else {
                foundLocalizedString = TRUE;
            }
            break;
        default:
            return FALSE;
        }
    }

    return foundDevice && foundConfiguration &&
        (!foundLocalizedString || foundLanguageTable);
}

static
NTSTATUS
ViiperAddDeviceDescriptors(
    _Inout_ PUDECXUSBDEVICE_INIT DeviceInit,
    _In_ const VIIPER_UDE_CREATE_DEVICE *Input
    )
{
    const VIIPER_UDE_DESCRIPTOR_RECORD *records =
        (const VIIPER_UDE_DESCRIPTOR_RECORD *)
        ((const UCHAR *)Input + Input->DescriptorRecordsOffset);
    const UCHAR *data = (const UCHAR *)Input + Input->DescriptorDataOffset;
    ULONG index;

    for (index = 0; index < Input->DescriptorCount; ++index) {
        const VIIPER_UDE_DESCRIPTOR_RECORD *record = &records[index];
        PUCHAR descriptor = (PUCHAR)(data + record->Offset);
        NTSTATUS status;

        switch (record->Kind) {
        case ViiperUdeDescriptorDevice:
        case ViiperUdeDescriptorConfiguration:
        case ViiperUdeDescriptorBos:
            status = UdecxUsbDeviceInitAddDescriptor(
                DeviceInit, descriptor, (USHORT)record->Length);
            break;
        case ViiperUdeDescriptorString:
            if (record->Index == 0) {
                status = UdecxUsbDeviceInitAddDescriptorWithIndex(
                    DeviceInit, descriptor, (USHORT)record->Length, 0);
            } else {
                status = UdecxUsbDeviceInitAddStringDescriptorRaw(
                    DeviceInit,
                    descriptor,
                    (USHORT)record->Length,
                    (UCHAR)record->Index,
                    record->LanguageId);
            }
            break;
        default:
            status = STATUS_INVALID_PARAMETER;
            break;
        }
        if (!NT_SUCCESS(status)) {
            return status;
        }
    }
    return STATUS_SUCCESS;
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
        InterlockedCompareExchange(&fileContext->Negotiated, 0, 0) == 0 ||
        InterlockedCompareExchange(&fileContext->Closing, 0, 0) != 0) {
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
            if (!ControllerContext->RemovingSlots[index] &&
                freeSlot == VIIPER_UDE_MAX_DEVICES) {
                freeSlot = index;
            }
            continue;
        }
        if (ViiperGetDeviceContext(current)->DeviceId == DeviceId &&
            InterlockedCompareExchange(
                &ViiperGetDeviceContext(current)->Purging, 0, 0) == 0) {
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
    if (Slot < VIIPER_UDE_MAX_DEVICES) {
        if (ControllerContext->Devices[Slot] == Device) {
            ControllerContext->Devices[Slot] = WDF_NO_HANDLE;
        }
        if (ControllerContext->Devices[Slot] == WDF_NO_HANDLE) {
            ControllerContext->RemovingSlots[Slot] = FALSE;
        }
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
    status = ViiperAddDeviceDescriptors(deviceInit, input);
    if (!NT_SUCCESS(status)) {
        UdecxUsbDeviceInitFree(deviceInit);
        return status;
    }

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
    WdfObjectReference(ownerFile);
    InterlockedExchange(&deviceContext->OwnerReferenced, 1);

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
    InterlockedExchange(&deviceContext->ActiveCounted, 1);
    InterlockedIncrement(&controllerContext->ActiveDevices);
    WdfRequestSetInformation(Request, 0);
    return STATUS_SUCCESS;
}

static
NTSTATUS
ViiperBeginRemoveDevice(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ WDFFILEOBJECT OwnerFile,
    _In_ ULONGLONG DeviceId,
    _In_ ULONG Generation,
    _In_ BOOLEAN MatchGeneration,
    _Out_ UDECXUSBDEVICE *Device
    )
{
    NTSTATUS status = STATUS_NOT_FOUND;
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
        if (InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0) {
            continue;
        }
        InterlockedExchange(&deviceContext->Purging, TRUE);
        ControllerContext->Devices[index] = WDF_NO_HANDLE;
        ControllerContext->RemovingSlots[index] = TRUE;
        *Device = current;
        status = STATUS_SUCCESS;
        break;
    }
    WdfWaitLockRelease(ControllerContext->DeviceLock);
    return status;
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
    if (inputLength != sizeof(*input) || input->Header.Magic != VIIPER_UDE_MAGIC ||
        input->Header.Major != VIIPER_UDE_ABI_MAJOR ||
        input->Header.Minor != VIIPER_UDE_ABI_MINOR ||
        input->Header.Flags != 0 ||
        input->Header.Size != sizeof(*input) ||
        input->DeviceId == 0 || input->Generation == 0 || input->Reserved != 0) {
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        return STATUS_INVALID_PARAMETER;
    }

    status = ViiperBeginRemoveDevice(
        controllerContext, ownerFile, input->DeviceId, input->Generation, TRUE, &device);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    status = UdecxUsbDevicePlugOutAndDelete(device);
    if (!NT_SUCCESS(status)) {
        // PlugOutAndDelete consumes the UDE handle even when it reports a
        // failure. The request was nevertheless accepted at our ABI boundary;
        // attempting to restore or retry this handle would be a use-after-
        // invalidation. Restart the controller so PnP owns final recovery.
        WdfDeviceSetFailed(controller, WdfDeviceFailedAttemptRestart);
        return STATUS_SUCCESS;
    }
    return STATUS_SUCCESS;
}

BOOLEAN
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
        BOOLEAN removalPending = FALSE;
        ULONG index;

        WdfWaitLockAcquire(controllerContext->DeviceLock, NULL);
        for (index = 0; index < VIIPER_UDE_MAX_DEVICES; ++index) {
            device = controllerContext->Devices[index];
            if (device != WDF_NO_HANDLE &&
                ViiperGetDeviceContext(device)->OwnerFile == OwnerFile &&
                InterlockedCompareExchange(
                    &ViiperGetDeviceContext(device)->Purging, 0, 0) == 0) {
                deviceId = ViiperGetDeviceContext(device)->DeviceId;
                break;
            }
            if (controllerContext->RemovingSlots[index]) {
                removalPending = TRUE;
            }
        }
        WdfWaitLockRelease(controllerContext->DeviceLock);
        if (deviceId == 0) {
            return !removalPending;
        }

        if (!NT_SUCCESS(ViiperBeginRemoveDevice(
                controllerContext, OwnerFile, deviceId, 0, FALSE, &device))) {
            return FALSE;
        }
        deviceContext = ViiperGetDeviceContext(device);
        if (deviceContext->Plugged) {
            if (!NT_SUCCESS(UdecxUsbDevicePlugOutAndDelete(device))) {
                WdfDeviceSetFailed(Controller, WdfDeviceFailedAttemptRestart);
                return FALSE;
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
    if (InterlockedExchange(&deviceContext->ActiveCounted, 0) != 0) {
        InterlockedDecrement(&controllerContext->ActiveDevices);
    }
    if (InterlockedExchange(&deviceContext->OwnerReferenced, 0) != 0) {
        WdfObjectDereference(deviceContext->OwnerFile);
    }
}

NTSTATUS
ViiperEvtUsbDeviceD0Entry(
    _In_ WDFDEVICE Controller,
    _In_ UDECXUSBDEVICE Device
    )
{
    UNREFERENCED_PARAMETER(Controller);
    (VOID)ViiperQueueDeviceLifecycleEvent(Device, ViiperUdeOperationDeviceD0Entry);
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
    UNREFERENCED_PARAMETER(WakeSetting);
    (VOID)ViiperQueueDeviceLifecycleEvent(Device, ViiperUdeOperationDeviceD0Exit);
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
    if (DispatchType != WdfIoQueueDispatchManual) {
        queueConfig.EvtIoInternalDeviceControl = ViiperEvtEndpointIoInternalControl;
    }
    WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(&attributes, UDECXUSBENDPOINT);
    attributes.ParentObject = Endpoint;
    attributes.ExecutionLevel = WdfExecutionLevelPassive;
    status = WdfIoQueueCreate(deviceContext->Controller, &queueConfig, &attributes, &endpointContext->Queue);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    queueEndpoint = ViiperGetQueueEndpoint(endpointContext->Queue);
    *queueEndpoint = Endpoint;
    UdecxUsbEndpointSetWdfIoQueue(Endpoint, endpointContext->Queue);
    return STATUS_SUCCESS;
}

VOID
ViiperEvtEndpointCleanup(
    _In_ WDFOBJECT EndpointObject
    )
{
    UDECXUSBENDPOINT endpoint = (UDECXUSBENDPOINT)EndpointObject;
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext;
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext;
    UCHAR address;

    PAGED_CODE();
    if (endpointContext->Device == WDF_NO_HANDLE) {
        return;
    }
    deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    if (deviceContext->Controller == WDF_NO_HANDLE) {
        return;
    }
    controllerContext = ViiperGetControllerContext(deviceContext->Controller);
    address = endpointContext->Descriptor.bEndpointAddress;
    WdfWaitLockAcquire(controllerContext->DeviceLock, NULL);
    if (deviceContext->Endpoints[address] == endpoint) {
        deviceContext->Endpoints[address] = WDF_NO_HANDLE;
    }
    WdfWaitLockRelease(controllerContext->DeviceLock);
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
    attributes.EvtCleanupCallback = ViiperEvtEndpointCleanup;
    attributes.ExecutionLevel = WdfExecutionLevelPassive;
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
    } else if ((descriptor.bEndpointAddress & USB_ENDPOINT_DIRECTION_MASK) != 0 &&
        (descriptor.bmAttributes & USB_ENDPOINT_TYPE_MASK) == USB_ENDPOINT_TYPE_INTERRUPT) {
        endpointContext->FastInput = TRUE;
        dispatchType = WdfIoQueueDispatchManual;
        WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
        attributes.ParentObject = endpoint;
        status = WdfWaitLockCreate(&attributes, &endpointContext->InputLock);
        if (!NT_SUCCESS(status)) {
            return status;
        }
    } else {
        dispatchType = WdfIoQueueDispatchParallel;
    }
    status = ViiperCreateEndpointQueue(endpoint, dispatchType);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    {
        VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);
        VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
            ViiperGetControllerContext(deviceContext->Controller);
        WdfWaitLockAcquire(controllerContext->DeviceLock, NULL);
        deviceContext->Endpoints[descriptor.bEndpointAddress] = endpoint;
        WdfWaitLockRelease(controllerContext->DeviceLock);
    }
    return STATUS_SUCCESS;
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

static
VOID
ViiperCompleteRetrievedInputUrb(
    _In_ WDFREQUEST Request,
    _In_ NTSTATUS Status
    )
{
    KIRQL previousIrql = KeGetCurrentIrql();
    BOOLEAN raised = previousIrql < DISPATCH_LEVEL;

    // The URB was parked in a manual queue and is therefore completed from a
    // different call path than UdeCx's submit callback. Match usbip-win2's
    // documented compatibility pattern without allocating a DPC per report.
    if (raised) {
        KeRaiseIrql(DISPATCH_LEVEL, &previousIrql);
    }
    if (NT_SUCCESS(Status)) {
        UdecxUrbComplete(Request, USBD_STATUS_SUCCESS);
    } else {
        UdecxUrbCompleteWithNtStatus(Request, Status);
    }
    if (raised) {
        KeLowerIrql(previousIrql);
    }
}

NTSTATUS
ViiperSubmitInputReport(
    _In_ WDFQUEUE Queue,
    _In_ WDFREQUEST Request
    )
{
    WDFDEVICE controller = WdfIoQueueGetDevice(Queue);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(controller);
    VIIPER_UDE_INPUT_REPORT *input;
    UCHAR *payload;
    size_t inputLength;
    size_t payloadLength;
    WDFFILEOBJECT ownerFile;
    UDECXUSBENDPOINT endpoint = WDF_NO_HANDLE;
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext;
    WDFREQUEST urbRequest = WDF_NO_HANDLE;
    PURB urb;
    ULONG transferLength;
    ULONG index;
    NTSTATUS status;
    BOOLEAN lifecycleDrop = FALSE;

    status = ViiperValidateBrokerOwner(controller, Request);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    ownerFile = WdfRequestGetFileObject(Request);
    status = WdfRequestRetrieveInputBuffer(
        Request, sizeof(*input), (PVOID *)&input, &inputLength);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    status = WdfRequestRetrieveOutputBuffer(
        Request, 1, (PVOID *)&payload, &payloadLength);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    if (inputLength != sizeof(*input) ||
        input->Header.Magic != VIIPER_UDE_MAGIC ||
        input->Header.Major != VIIPER_UDE_ABI_MAJOR ||
        input->Header.Minor != VIIPER_UDE_ABI_MINOR ||
        input->Header.Flags != 0 ||
        input->Header.Size != sizeof(*input) + input->PayloadLength ||
        input->DeviceId == 0 || input->Generation == 0 || input->Sequence == 0 ||
        input->Sequence > MAXLONGLONG ||
        (input->EndpointAddress & USB_ENDPOINT_DIRECTION_MASK) == 0 ||
        input->PayloadOffset != sizeof(*input) || input->PayloadLength == 0 ||
        input->PayloadLength > VIIPER_UDE_MAX_INPUT_REPORT_BYTES ||
        payloadLength != input->PayloadLength ||
        input->Reserved1[0] != 0 || input->Reserved1[1] != 0 || input->Reserved1[2] != 0) {
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        return STATUS_INVALID_PARAMETER;
    }

    WdfWaitLockAcquire(controllerContext->DeviceLock, NULL);
    for (index = 0; index < VIIPER_UDE_MAX_DEVICES; ++index) {
        UDECXUSBDEVICE device = controllerContext->Devices[index];
        VIIPER_UDE_DEVICE_CONTEXT *deviceContext;
        if (device == WDF_NO_HANDLE) {
            continue;
        }
        deviceContext = ViiperGetDeviceContext(device);
        if (deviceContext->OwnerFile != ownerFile ||
            deviceContext->DeviceId != input->DeviceId ||
            deviceContext->Generation != input->Generation) {
            continue;
        }
        if (InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0) {
            lifecycleDrop = TRUE;
            break;
        }
        endpoint = deviceContext->Endpoints[input->EndpointAddress];
        if (endpoint != WDF_NO_HANDLE) {
            WdfObjectReference(endpoint);
        }
        break;
    }
    WdfWaitLockRelease(controllerContext->DeviceLock);
    if (lifecycleDrop) {
        // A report already submitted by the owner may cross the D0/unplug
        // boundary before the ordered lifecycle notification cancels its
        // publisher. It is stale latest-state data, not a broken owner
        // session. Acknowledge and discard it exactly at that boundary.
        return STATUS_SUCCESS;
    }
    if (endpoint == WDF_NO_HANDLE) {
        return STATUS_NOT_FOUND;
    }

    endpointContext = ViiperGetEndpointContext(endpoint);
    if (!endpointContext->FastInput ||
        endpointContext->InputLock == WDF_NO_HANDLE) {
        WdfObjectDereference(endpoint);
        return STATUS_INVALID_DEVICE_STATE;
    }

    // InputQueue is parallel so independent controllers never block one
    // another. Serialize only this endpoint, preserving report order even if
    // a faulty or hostile owner submits concurrent updates for the same pad.
    WdfWaitLockAcquire(endpointContext->InputLock, NULL);
    if (InterlockedCompareExchange(&endpointContext->Purging, 0, 0) != 0) {
        WdfWaitLockRelease(endpointContext->InputLock);
        WdfObjectDereference(endpoint);
        // Endpoint purge and restart preserve the device generation. Do not
        // turn the one report racing purge into a fatal user-mode session.
        return STATUS_SUCCESS;
    }
    if (input->Sequence <= (ULONGLONG)InterlockedCompareExchange64(
            &endpointContext->LastInputSequence, 0, 0)) {
        WdfWaitLockRelease(endpointContext->InputLock);
        WdfObjectDereference(endpoint);
        return STATUS_INVALID_DEVICE_STATE;
    }
    // Claim the sequence when the report is accepted, including when no host
    // poll is parked. That makes latest-state coalescing replay-safe.
    InterlockedExchange64(&endpointContext->LastInputSequence, (LONG64)input->Sequence);
    InterlockedIncrement64(&controllerContext->InputReportsSubmitted);
    status = WdfIoQueueRetrieveNextRequest(endpointContext->Queue, &urbRequest);
    if (!NT_SUCCESS(status)) {
        WdfWaitLockRelease(endpointContext->InputLock);
        WdfObjectDereference(endpoint);
        // A producer update is allowed to arrive before Windows posts its
        // next interrupt poll. This is normal latest-state coalescing, not a
        // session fault; the following report services the following poll.
        return status == STATUS_NO_MORE_ENTRIES ? STATUS_SUCCESS : status;
    }
    urb = ViiperGetUrb(urbRequest);
    if (urb == NULL ||
        (urb->UrbHeader.Function != URB_FUNCTION_BULK_OR_INTERRUPT_TRANSFER &&
            urb->UrbHeader.Function != URB_FUNCTION_BULK_OR_INTERRUPT_TRANSFER_USING_CHAINED_MDL) ||
        (urb->UrbBulkOrInterruptTransfer.TransferFlags & USBD_TRANSFER_DIRECTION_IN) == 0) {
        ViiperCompleteRetrievedInputUrb(urbRequest, STATUS_INVALID_DEVICE_REQUEST);
        WdfWaitLockRelease(endpointContext->InputLock);
        WdfObjectDereference(endpoint);
        return STATUS_INVALID_DEVICE_REQUEST;
    }
    transferLength = urb->UrbBulkOrInterruptTransfer.TransferBufferLength;
    if (input->PayloadLength > transferLength) {
        ViiperCompleteRetrievedInputUrb(urbRequest, STATUS_BUFFER_TOO_SMALL);
        WdfWaitLockRelease(endpointContext->InputLock);
        WdfObjectDereference(endpoint);
        return STATUS_BUFFER_TOO_SMALL;
    }
    status = ViiperCopyTransferBuffer(
        urbRequest, urb, payload, input->PayloadLength, TRUE);
    if (!NT_SUCCESS(status)) {
        ViiperCompleteRetrievedInputUrb(urbRequest, status);
        WdfWaitLockRelease(endpointContext->InputLock);
        WdfObjectDereference(endpoint);
        return status;
    }

    urb->UrbBulkOrInterruptTransfer.TransferBufferLength = input->PayloadLength;
    UdecxUrbSetBytesCompleted(urbRequest, input->PayloadLength);
    InterlockedAdd64(&controllerContext->BytesFromDevice, input->PayloadLength);
    InterlockedIncrement64(&controllerContext->InputReportsCompleted);
    ViiperCompleteRetrievedInputUrb(urbRequest, STATUS_SUCCESS);
    WdfWaitLockRelease(endpointContext->InputLock);
    WdfObjectDereference(endpoint);
    return STATUS_SUCCESS;
}

VOID
ViiperEvtEndpointReset(
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ WDFREQUEST Request
    )
{
    NTSTATUS status;

    ViiperPurgeEndpointOperations(Endpoint, STATUS_DEVICE_NOT_READY);
    status = ViiperQueueEndpointLifecycleEvent(Endpoint, ViiperUdeOperationEndpointReset);
    WdfRequestComplete(Request, status);
}

VOID
ViiperEvtEndpointQueuePurged(
    _In_ WDFQUEUE Queue,
    _In_ WDFCONTEXT Context
    )
{
    UDECXUSBENDPOINT endpoint = (UDECXUSBENDPOINT)Context;
    UNREFERENCED_PARAMETER(Queue);
    UdecxUsbEndpointPurgeComplete(endpoint);
}

VOID
ViiperEvtEndpointPurge(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    InterlockedExchange(&endpointContext->Purging, TRUE);
    ViiperPurgeEndpointOperations(Endpoint, STATUS_DEVICE_NOT_READY);
    (VOID)ViiperQueueEndpointLifecycleEvent(Endpoint, ViiperUdeOperationEndpointPurge);
    WdfIoQueuePurge(endpointContext->Queue, ViiperEvtEndpointQueuePurged, Endpoint);
}

VOID
ViiperEvtEndpointStart(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    (VOID)ViiperQueueEndpointLifecycleEvent(Endpoint, ViiperUdeOperationEndpointStart);
    InterlockedExchange(&ViiperGetEndpointContext(Endpoint)->Purging, FALSE);
    WdfIoQueueStart(ViiperGetEndpointContext(Endpoint)->Queue);
}

VOID
ViiperEvtEndpointsConfigure(
    _In_ UDECXUSBDEVICE Device,
    _In_ WDFREQUEST Request,
    _In_ UDECX_ENDPOINTS_CONFIGURE_PARAMS *ConfigureParams
    )
{
    NTSTATUS status = STATUS_SUCCESS;

    switch (ConfigureParams->ConfigureType) {
    case UdecxEndpointsConfigureTypeDeviceInitialize:
    case UdecxEndpointsConfigureTypeDeviceConfigurationChange:
        status = ViiperQueueDeviceLifecycleEvent(Device, ViiperUdeOperationDeviceReset);
        break;
    case UdecxEndpointsConfigureTypeInterfaceSettingChange:
        status = ViiperQueueInterfaceLifecycleEvent(
            Device,
            ConfigureParams->InterfaceNumber,
            ConfigureParams->NewInterfaceSetting);
        break;
    case UdecxEndpointsConfigureTypeEndpointsReleasedOnly:
        break;
    default:
        status = STATUS_INVALID_PARAMETER;
        break;
    }
    WdfRequestComplete(Request, status);
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
        NTSTATUS status = ViiperQueueUrb(Queue, Request);
        if (status != STATUS_PENDING) {
            ViiperCompleteUnownedUrbAsync(
                WdfIoQueueGetDevice(Queue), Request, status);
        }
    } else {
        WdfRequestComplete(Request, STATUS_INVALID_DEVICE_REQUEST);
    }
}

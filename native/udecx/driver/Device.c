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
#pragma alloc_text(PAGE, ViiperBeginControllerShutdown)
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
ViiperValidateEndpointSchedules(
    _In_reads_bytes_(Length) const UCHAR *Descriptor,
    _In_ ULONG Length,
    _In_ ULONG Speed
    )
{
    ULONG offset = 0;

    // The owner sends the UdeCx-facing descriptor, not the controller's
    // logical full-speed descriptor. USBHUB3 schedules every UDE endpoint
    // using high-speed interval rules. Reject an old or malformed privileged
    // owner here, before UdecxUsbDeviceInitAddDescriptor can expose an unsafe
    // ISO pipe to a client driver.
    while (offset < Length) {
        const UCHAR *item;
        ULONG itemLength;
        UCHAR transferType;

        if (Length - offset < 2) {
            return FALSE;
        }
        item = Descriptor + offset;
        itemLength = item[0];
        if (itemLength < 2 || itemLength > Length - offset) {
            return FALSE;
        }
        if (item[1] != USB_ENDPOINT_DESCRIPTOR_TYPE) {
            offset += itemLength;
            continue;
        }
        if (itemLength < sizeof(USB_ENDPOINT_DESCRIPTOR)) {
            return FALSE;
        }

        transferType = item[3] & USB_ENDPOINT_TYPE_MASK;
        if (transferType == USB_ENDPOINT_TYPE_ISOCHRONOUS) {
            if (Speed == 1) {
                return FALSE;
            }
            if (Speed == 2) {
                // Full-speed one-frame ISO is projected to the equivalent
                // high-speed exponent before crossing this ABI.
                if (item[6] != 4) {
                    return FALSE;
                }
            } else if (item[6] == 0 || item[6] > 4) {
                // Windows supports HS/SS ISO polling periods only through
                // eight microframes. Client I/O above that may bugcheck.
                return FALSE;
            }
        } else if (transferType == USB_ENDPOINT_TYPE_INTERRUPT &&
                   (item[6] == 0 || item[6] > 16)) {
            return FALSE;
        }
        offset += itemLength;
    }
    return offset == Length;
}

static const UCHAR microsoftOS10StringPrefix[] = {
    0x12, 0x03,
    0x4d, 0x00, 0x53, 0x00, 0x46, 0x00, 0x54, 0x00,
    0x31, 0x00, 0x30, 0x00, 0x30, 0x00
};

static
BOOLEAN
ViiperIsMicrosoftOS10StringDescriptor(
    _In_ const VIIPER_UDE_DESCRIPTOR_RECORD *Record,
    _In_reads_bytes_(Record->Length) const UCHAR *Descriptor
    )
{
    return Record->Index == VIIPER_UDE_MS_OS_10_STRING_INDEX &&
        Record->LanguageId == 0 &&
        Record->Length == VIIPER_UDE_MS_OS_10_STRING_LENGTH &&
        sizeof(microsoftOS10StringPrefix) == VIIPER_UDE_MS_OS_10_VENDOR_CODE_OFFSET &&
        RtlCompareMemory(
            Descriptor,
            microsoftOS10StringPrefix,
            sizeof(microsoftOS10StringPrefix)) == sizeof(microsoftOS10StringPrefix) &&
        Descriptor[VIIPER_UDE_MS_OS_10_VENDOR_CODE_OFFSET] != 0 &&
        Descriptor[VIIPER_UDE_MS_OS_10_STRING_LENGTH - 1] == 0;
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
    BOOLEAN foundMicrosoftOS10String = FALSE;

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
                    descriptor, record->Length, USB_CONFIGURATION_DESCRIPTOR_TYPE) ||
                !ViiperValidateEndpointSchedules(
                    descriptor, record->Length, Input->Speed)) {
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
            {
                BOOLEAN isMicrosoftOS10String =
                    ViiperIsMicrosoftOS10StringDescriptor(record, descriptor);
                if (record->Index > MAXUCHAR || record->Length > MAXUCHAR ||
                    descriptor[0] != record->Length || descriptor[1] != USB_STRING_DESCRIPTOR_TYPE ||
                    (record->Length & 1) != 0 ||
                    (record->Index == 0 && record->LanguageId != 0) ||
                    (record->Index == 0 && record->Length < 4) ||
                    (record->Index != 0 && record->LanguageId == 0 &&
                        !isMicrosoftOS10String)) {
                    return FALSE;
                }
                if (record->Index == 0) {
                    if (foundLanguageTable) {
                        return FALSE;
                    }
                    foundLanguageTable = TRUE;
                } else if (isMicrosoftOS10String) {
                    if (foundMicrosoftOS10String) {
                        return FALSE;
                    }
                    foundMicrosoftOS10String = TRUE;
                } else {
                    foundLocalizedString = TRUE;
                }
                break;
            }
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
ViiperBeginOwnerAdmission(
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
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0 ||
        controllerContext->OwnerFile != fileObject || controllerContext->CleanupInProgress ||
        InterlockedCompareExchange(&fileContext->Negotiated, 0, 0) == 0 ||
        InterlockedCompareExchange(&fileContext->Closing, 0, 0) != 0) {
        status = STATUS_INVALID_DEVICE_STATE;
    } else {
        // Keep both the owner object and cleanup boundary alive while a child
        // is created or destroyed. UdeCx lifecycle calls may invoke callbacks,
        // so do not hold OwnerLock across them.
        WdfObjectReference(fileObject);
        if (InterlockedIncrement(&controllerContext->ActiveOwnerAdmissions) == 1) {
            KeClearEvent(&controllerContext->OwnerAdmissionsDrained);
        }
        *OwnerFile = fileObject;
    }
    WdfWaitLockRelease(controllerContext->OwnerLock);
    return status;
}

static
VOID
ViiperEndOwnerAdmission(
    _In_ WDFDEVICE Controller,
    _In_ WDFFILEOBJECT OwnerFile
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(Controller);
    LONG remaining;

    WdfWaitLockAcquire(controllerContext->OwnerLock, NULL);
    remaining = InterlockedDecrement(&controllerContext->ActiveOwnerAdmissions);
    NT_ASSERT(remaining >= 0);
    if (remaining == 0) {
        KeSetEvent(&controllerContext->OwnerAdmissionsDrained, IO_NO_INCREMENT, FALSE);
    }
    WdfWaitLockRelease(controllerContext->OwnerLock);
    WdfObjectDereference(OwnerFile);
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
UDECXUSBDEVICE
ViiperFindInputDeviceLocked(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ ULONGLONG DeviceId
    )
{
    ULONG first = 0;
    ULONG count = ControllerContext->InputDeviceCount;

    // InputDevices is a cold-lifecycle index: mutations keep it sorted while
    // the report producer performs at most log2(32) identity comparisons.
    while (count != 0) {
        ULONG step = count / 2;
        ULONG candidate = first + step;
        UDECXUSBDEVICE device = ControllerContext->InputDevices[candidate];
        ULONGLONG candidateId = ViiperGetDeviceContext(device)->DeviceId;

        if (candidateId < DeviceId) {
            first = candidate + 1;
            count -= step + 1;
        } else {
            count = step;
        }
    }
    if (first >= ControllerContext->InputDeviceCount ||
        ViiperGetDeviceContext(ControllerContext->InputDevices[first])->DeviceId != DeviceId) {
        return WDF_NO_HANDLE;
    }
    return ControllerContext->InputDevices[first];
}

static
NTSTATUS
ViiperInsertInputDeviceLocked(
    _Inout_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ UDECXUSBDEVICE Device
    )
{
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);
    ULONG position = 0;

    if (ControllerContext->InputDeviceCount >= VIIPER_UDE_MAX_DEVICES) {
        return STATUS_INSUFFICIENT_RESOURCES;
    }
    while (position < ControllerContext->InputDeviceCount &&
        ViiperGetDeviceContext(ControllerContext->InputDevices[position])->DeviceId <
            deviceContext->DeviceId) {
        ++position;
    }
    if (position < ControllerContext->InputDeviceCount &&
        ViiperGetDeviceContext(ControllerContext->InputDevices[position])->DeviceId ==
            deviceContext->DeviceId) {
        return STATUS_OBJECT_NAME_COLLISION;
    }
    if (position < ControllerContext->InputDeviceCount) {
        RtlMoveMemory(
            &ControllerContext->InputDevices[position + 1],
            &ControllerContext->InputDevices[position],
            sizeof(ControllerContext->InputDevices[0]) *
                (ControllerContext->InputDeviceCount - position));
    }
    ControllerContext->InputDevices[position] = Device;
    ++ControllerContext->InputDeviceCount;
    return STATUS_SUCCESS;
}

static
VOID
ViiperRemoveInputDeviceLocked(
    _Inout_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ UDECXUSBDEVICE Device
    )
{
    ULONG position;

    for (position = 0; position < ControllerContext->InputDeviceCount; ++position) {
        if (ControllerContext->InputDevices[position] == Device) {
            break;
        }
    }
    if (position == ControllerContext->InputDeviceCount) {
        return;
    }
    --ControllerContext->InputDeviceCount;
    if (position < ControllerContext->InputDeviceCount) {
        RtlMoveMemory(
            &ControllerContext->InputDevices[position],
            &ControllerContext->InputDevices[position + 1],
            sizeof(ControllerContext->InputDevices[0]) *
                (ControllerContext->InputDeviceCount - position));
    }
    ControllerContext->InputDevices[ControllerContext->InputDeviceCount] = WDF_NO_HANDLE;
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

    ViiperAcquireDeviceLockExclusive(ControllerContext);
    if (InterlockedCompareExchange(&ControllerContext->ShuttingDown, 0, 0) != 0) {
        status = STATUS_DEVICE_REMOVED;
        goto Exit;
    }
    for (index = 0; index < VIIPER_UDE_MAX_DEVICES; ++index) {
        UDECXUSBDEVICE current = ControllerContext->Devices[index];
        if (current == WDF_NO_HANDLE) {
            if (freeSlot == VIIPER_UDE_MAX_DEVICES) {
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
        status = ViiperInsertInputDeviceLocked(ControllerContext, Device);
        if (NT_SUCCESS(status)) {
            ControllerContext->Devices[freeSlot] = Device;
            *Slot = freeSlot;
        }
    }

Exit:
    ViiperReleaseDeviceLockExclusive(ControllerContext);
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
    ViiperAcquireDeviceLockExclusive(ControllerContext);
    if (Slot < VIIPER_UDE_MAX_DEVICES) {
        if (ControllerContext->Devices[Slot] == Device) {
            ViiperRemoveInputDeviceLocked(ControllerContext, Device);
            ControllerContext->Devices[Slot] = WDF_NO_HANDLE;
        }
    }
    ViiperReleaseDeviceLockExclusive(ControllerContext);
}

static
VOID
ViiperRetireActiveDevice(
    _In_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext,
    _In_ VIIPER_UDE_DEVICE_CONTEXT *DeviceContext
    )
{
    LONG remaining;

    if (InterlockedExchange(&DeviceContext->ActiveCounted, 0) == 0) {
        return;
    }
    remaining = InterlockedDecrement(&ControllerContext->ActiveDevices);
    NT_ASSERT(remaining >= 0);
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
    status = ViiperBeginOwnerAdmission(controller, Request, &ownerFile);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    deviceInit = UdecxUsbDeviceInitAllocate(controller);
    if (deviceInit == NULL) {
        status = STATUS_INSUFFICIENT_RESOURCES;
        goto ExitAdmission;
    }

    UDECX_USB_DEVICE_CALLBACKS_INIT(&callbacks);
    callbacks.EvtUsbDeviceLinkPowerEntry = ViiperEvtUsbDeviceD0Entry;
    callbacks.EvtUsbDeviceLinkPowerExit = ViiperEvtUsbDeviceD0Exit;
    callbacks.EvtUsbDeviceReset = ViiperEvtUsbDeviceReset;
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
        goto ExitAdmission;
    }

    WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(&attributes, VIIPER_UDE_DEVICE_CONTEXT);
    attributes.ParentObject = controller;
    attributes.EvtCleanupCallback = ViiperEvtVirtualDeviceCleanup;
    // UdeCx permits its USB-device callbacks at <= DISPATCH_LEVEL, while
    // endpoint creation and the DeviceLock snapshot used by power/reset
    // callbacks are PASSIVE_LEVEL-only. KMDF controller objects otherwise
    // default to dispatch execution, so make the child callback contract
    // explicit instead of relying on the current UdeCx call context.
    attributes.ExecutionLevel = WdfExecutionLevelPassive;
    status = UdecxUsbDeviceCreate(&deviceInit, &attributes, &device);
    if (!NT_SUCCESS(status)) {
        UdecxUsbDeviceInitFree(deviceInit);
        goto ExitAdmission;
    }

    deviceContext = ViiperGetDeviceContext(device);
    RtlZeroMemory(deviceContext, sizeof(*deviceContext));
    deviceContext->Controller = controller;
    deviceContext->OwnerFile = ownerFile;
    deviceContext->DeviceId = input->DeviceId;
    deviceContext->Generation = input->Generation;
    deviceContext->Slot = VIIPER_UDE_MAX_DEVICES;
    deviceContext->Speed = speed;
    deviceContext->MaxPendingOperations = input->MaxPendingOperations;
    WdfObjectReference(ownerFile);
    InterlockedExchange(&deviceContext->OwnerReferenced, 1);

    status = ViiperClaimDeviceSlot(controllerContext, device, input->DeviceId, &slot);
    if (!NT_SUCCESS(status)) {
        WdfObjectDelete(device);
        goto ExitAdmission;
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
        goto ExitAdmission;
    }

    deviceContext->Plugged = TRUE;
    InterlockedExchange(&deviceContext->ActiveCounted, 1);
    InterlockedIncrement(&controllerContext->ActiveDevices);
    WdfRequestSetInformation(Request, 0);
    status = STATUS_SUCCESS;

ExitAdmission:
    ViiperEndOwnerAdmission(controller, ownerFile);
    return status;
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

    ViiperAcquireDeviceLockExclusive(ControllerContext);
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
        // DeviceLock owns the table slot; BrokerLock is the admission
        // linearization point shared with forwarded URBs and direct input.
        // Set Purging through both before revoking the table entry so no
        // request that already referenced this generation can start late.
        WdfSpinLockAcquire(ControllerContext->BrokerLock);
        InterlockedExchange(&deviceContext->Purging, TRUE);
        WdfSpinLockRelease(ControllerContext->BrokerLock);
        ViiperRemoveInputDeviceLocked(ControllerContext, current);
        ControllerContext->Devices[index] = WDF_NO_HANDLE;
        // Devices[] is the logical ownership table. Retire the slot and its
        // active count while the UDE handle is still valid; KMDF may defer the
        // object's cleanup long after PlugOutAndDelete consumes this handle.
        ViiperRetireActiveDevice(ControllerContext, deviceContext);
        *Device = current;
        status = STATUS_SUCCESS;
        break;
    }
    ViiperReleaseDeviceLockExclusive(ControllerContext);
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
    status = ViiperBeginOwnerAdmission(controller, Request, &ownerFile);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    status = WdfRequestRetrieveInputBuffer(Request, sizeof(*input), (PVOID *)&input, &inputLength);
    if (!NT_SUCCESS(status)) {
        goto ExitAdmission;
    }
    if (inputLength != sizeof(*input) || input->Header.Magic != VIIPER_UDE_MAGIC ||
        input->Header.Major != VIIPER_UDE_ABI_MAJOR ||
        input->Header.Minor != VIIPER_UDE_ABI_MINOR ||
        input->Header.Flags != 0 ||
        input->Header.Size != sizeof(*input) ||
        input->DeviceId == 0 || input->Generation == 0 || input->Reserved != 0) {
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        status = STATUS_INVALID_PARAMETER;
        goto ExitAdmission;
    }

    status = ViiperBeginRemoveDevice(
        controllerContext, ownerFile, input->DeviceId, input->Generation, TRUE, &device);
    if (!NT_SUCCESS(status)) {
        goto ExitAdmission;
    }
    status = UdecxUsbDevicePlugOutAndDelete(device);
    if (!NT_SUCCESS(status)) {
        // PlugOutAndDelete consumes the UDE handle even when it reports a
        // failure. The request was nevertheless accepted at our ABI boundary;
        // attempting to restore or retry this handle would be a use-after-
        // invalidation. Restart the controller so PnP owns final recovery.
        WdfDeviceSetFailed(controller, WdfDeviceFailedAttemptRestart);
        status = STATUS_SUCCESS;
    }

ExitAdmission:
    ViiperEndOwnerAdmission(controller, ownerFile);
    return status;
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
        ULONG index;

        ViiperAcquireDeviceLockExclusive(controllerContext);
        for (index = 0; index < VIIPER_UDE_MAX_DEVICES; ++index) {
            device = controllerContext->Devices[index];
            if (device != WDF_NO_HANDLE &&
                ViiperGetDeviceContext(device)->OwnerFile == OwnerFile &&
                InterlockedCompareExchange(
                    &ViiperGetDeviceContext(device)->Purging, 0, 0) == 0) {
                deviceId = ViiperGetDeviceContext(device)->DeviceId;
                break;
            }
        }
        ViiperReleaseDeviceLockExclusive(controllerContext);
        if (deviceId == 0) {
            return TRUE;
        }

        if (!NT_SUCCESS(ViiperBeginRemoveDevice(
                controllerContext, OwnerFile, deviceId, 0, FALSE, &device))) {
            // The logical table is authoritative. A framework-owned deletion
            // can revoke the snapshot before this claim; rescan instead of
            // pinning the exclusive owner to an object that is already gone.
            continue;
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
ViiperBeginControllerShutdown(
    _In_ WDFDEVICE Controller
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext = ViiperGetControllerContext(Controller);
    UDECXUSBDEVICE devices[VIIPER_UDE_MAX_DEVICES] = {0};
    ULONG deviceCount = 0;
    ULONG index;

    PAGED_CODE();

    // Revoke all table handles in one transaction. PlugOutAndDelete can invoke
    // asynchronous UdeCx cleanup, so no controller lock may be held across it.
    ViiperAcquireDeviceLockExclusive(controllerContext);
    for (index = 0; index < VIIPER_UDE_MAX_DEVICES; ++index) {
        UDECXUSBDEVICE device = controllerContext->Devices[index];
        VIIPER_UDE_DEVICE_CONTEXT *deviceContext;

        if (device == WDF_NO_HANDLE) {
            continue;
        }
        deviceContext = ViiperGetDeviceContext(device);
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        InterlockedExchange(&deviceContext->Purging, TRUE);
        WdfSpinLockRelease(controllerContext->BrokerLock);
        ViiperRemoveInputDeviceLocked(controllerContext, device);
        controllerContext->Devices[index] = WDF_NO_HANDLE;
        ViiperRetireActiveDevice(controllerContext, deviceContext);
        devices[deviceCount++] = device;
    }
    ViiperReleaseDeviceLockExclusive(controllerContext);

    for (index = 0; index < deviceCount; ++index) {
        VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(devices[index]);
        if (deviceContext->Plugged) {
            // A successful call starts UdeCx-owned asynchronous deletion. If
            // UdeCx rejects the request during controller removal, ordinary
            // parent teardown still owns and deletes the child object.
            (VOID)UdecxUsbDevicePlugOutAndDelete(devices[index]);
        } else {
            WdfObjectDelete(devices[index]);
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
    WDFFILEOBJECT ownerFile = WDF_NO_HANDLE;

    PAGED_CODE();
    if (deviceContext->Controller == WDF_NO_HANDLE) {
        return;
    }
    controllerContext = ViiperGetControllerContext(deviceContext->Controller);

    // Lifecycle notification admission reads OwnerFile while holding
    // BrokerLock. Revoke both that admission and the reference which pins the
    // file context under the same lock, then release the reference outside the
    // lock. An atomic OwnerReferenced test alone is not a lifetime pin: cleanup
    // could otherwise dereference the file after the test and before the
    // notifier reads its context.
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    InterlockedExchange(&deviceContext->Purging, TRUE);
    if (InterlockedExchange(&deviceContext->OwnerReferenced, 0) != 0) {
        ownerFile = deviceContext->OwnerFile;
        deviceContext->OwnerFile = WDF_NO_HANDLE;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);

    ViiperReleaseDeviceSlot(controllerContext, device, deviceContext->Slot);
    // Normal removal retired the logical count before PlugOutAndDelete. This
    // is only the fallback for an unexpected framework-owned deletion.
    ViiperRetireActiveDevice(controllerContext, deviceContext);
    if (ownerFile != WDF_NO_HANDLE) {
        WdfObjectDereference(ownerFile);
    }
}

static
VOID
ViiperInvalidateEndpointInputReport(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);

    InterlockedExchange(&endpointContext->InputReportValid, FALSE);
    InterlockedExchange(&endpointContext->CachedDeliveryPending, FALSE);
}

static
VOID
ViiperInvalidateInputIfLifecycleClosed(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);

    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (InterlockedCompareExchange(&deviceContext->InD0, 0, 0) == 0 ||
        InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0 ||
        InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) != 0 ||
        InterlockedCompareExchange(&endpointContext->Purging, 0, 0) != 0 ||
        InterlockedCompareExchange(&endpointContext->Resetting, 0, 0) != 0) {
        ViiperInvalidateEndpointInputReport(Endpoint);
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
}

static
VOID
ViiperInvalidateDeviceInputReports(
    _In_ UDECXUSBDEVICE Device
    )
{
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    ULONG index;

    // Device power/reset admission is already closed before this helper is
    // called, so no new report can become valid. Keep endpoint lookup and the
    // final atomic invalidation inside one shared index acquisition; a WDF
    // reference would postpone destruction but cannot postpone EvtCleanup.
    ViiperAcquireDeviceLockShared(controllerContext);
    for (index = 0; index < RTL_NUMBER_OF(deviceContext->Endpoints); ++index) {
        UDECXUSBENDPOINT endpoint = deviceContext->Endpoints[index];
        if (endpoint != WDF_NO_HANDLE) {
            ViiperInvalidateEndpointInputReport(endpoint);
        }
    }
    ViiperReleaseDeviceLockShared(controllerContext);
}

NTSTATUS
ViiperEvtUsbDeviceD0Entry(
    _In_ WDFDEVICE Controller,
    _In_ UDECXUSBDEVICE Device
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(Controller);
    // This callback is the exact UdeCx power boundary. Open direct input
    // admission before publishing the ordered advisory event to user mode.
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0) {
        WdfSpinLockRelease(controllerContext->BrokerLock);
        return STATUS_DEVICE_REMOVED;
    }
    InterlockedExchange(&ViiperGetDeviceContext(Device)->InD0, TRUE);
    WdfSpinLockRelease(controllerContext->BrokerLock);
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
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(Controller);
    UNREFERENCED_PARAMETER(WakeSetting);
    // Close direct input admission synchronously. Waiting for the user-mode
    // notification would leave a scheduler window in which a fresh report
    // could complete a Windows poll after the child had left D0.
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    InterlockedExchange(&ViiperGetDeviceContext(Device)->InD0, FALSE);
    WdfSpinLockRelease(controllerContext->BrokerLock);
    ViiperInvalidateDeviceInputReports(Device);
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

    // VIIPER's production controller set is low/full/high-speed, so UdeCx
    // never invokes this SuperSpeed-only callback for a supported child.  A
    // virtual child has no physical function to power down; acknowledge the
    // host's bookkeeping transition exactly as usbip-win2's UdeCx reference
    // does, without mutating endpoint/media state behind UdeCx's queue
    // lifecycle.  If VIIPER adds a SuperSpeed controller with real remote-wake
    // behavior, that device must add an explicit per-interface state contract
    // rather than repurposing endpoint purge/start implicitly.
    return STATUS_SUCCESS;
}

static
NTSTATUS
ViiperBeginAcknowledgedDeviceReset(
    _In_ UDECXUSBDEVICE Device,
    _In_ WDFREQUEST Request
    )
{
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    NTSTATUS status;

    // Post-enumeration reset and device-configuration replacement are both
    // asynchronous UdeCx reset boundaries. Close every client-owned admission
    // path synchronously and permit only one reset transaction for a child.
    // User mode stops and joins the publishers before acknowledging the
    // operation; completion then reopens this exact kernel gate.
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0 ||
        InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0 ||
        InterlockedCompareExchange(&deviceContext->Resetting, TRUE, FALSE) != FALSE) {
        status = STATUS_DEVICE_BUSY;
    } else {
        status = STATUS_SUCCESS;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!NT_SUCCESS(status)) {
        return STATUS_DEVICE_BUSY;
    }
    ViiperInvalidateDeviceInputReports(Device);
    status = ViiperQueueAcknowledgedDeviceLifecycleEvent(
        Device, Request, ViiperUdeOperationDeviceReset);
    if (!NT_SUCCESS(status)) {
        InterlockedExchange(&deviceContext->Resetting, FALSE);
    }
    return status;
}

VOID
ViiperEvtUsbDeviceReset(
    _In_ WDFDEVICE Controller,
    _In_ UDECXUSBDEVICE Device,
    _In_ WDFREQUEST Request,
    _In_ BOOLEAN AllDevicesReset
    )
{
    NTSTATUS status;

    UNREFERENCED_PARAMETER(Controller);
    if (AllDevicesReset) {
        // The controller uses UdecxWdfDeviceResetActionResetEachUsbDevice,
        // so UdeCx must deliver one callback per child. Accepting a controller-
        // wide reset here would make the owner lose the affected generation.
        WdfRequestComplete(Request, STATUS_NOT_SUPPORTED);
        return;
    }

    status = ViiperBeginAcknowledgedDeviceReset(Device, Request);
    if (!NT_SUCCESS(status)) {
        WdfRequestComplete(Request, status);
    }
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
    // KMDF's default queued-cancellation path completes synchronously. UDE
    // requires an explicit callback so even a never-dispatched URB can cross
    // the shared completion DPC.
    queueConfig.EvtIoCanceledOnQueue = ViiperEvtUrbCanceledOnQueue;
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
    ViiperAcquireDeviceLockExclusive(controllerContext);
    // Microsoft permits no ordinary object access after EvtCleanup is called,
    // even when a WDF reference postpones destruction. UdeCx therefore owns
    // the lifetime ordering: EvtEndpointPurge closes BrokerLock admission, its
    // work item drains ActiveOperations, and only then calls PurgeComplete.
    // Endpoint creation failure has no published users. Cleanup must never be
    // used as a late wait for an operation which can still access this context.
    NT_ASSERT(InterlockedCompareExchange(
        &endpointContext->ActiveOperations, 0, 0) == 0);
    ViiperInvalidateEndpointInputReport(endpoint);
    if (deviceContext->DefaultEndpoint == endpoint) {
        deviceContext->DefaultEndpoint = WDF_NO_HANDLE;
    }
    if (deviceContext->Endpoints[address] == endpoint) {
        deviceContext->Endpoints[address] = WDF_NO_HANDLE;
        // The user-mode latest-state publisher is stopped by the ordered
        // endpoint-purge notification. It can race this asynchronous object
        // cleanup by one already-built report. Preserve an address-scoped
        // tombstone so that report is distinguishable from a report for an
        // endpoint that never existed in this device generation.
        deviceContext->RetiredEndpoints[address] = TRUE;
    }
    ViiperReleaseDeviceLockExclusive(controllerContext);
}

NTSTATUS
ViiperEvtEndpointAdd(
    _In_ UDECXUSBDEVICE Device,
    _In_ UDECX_USB_ENDPOINT_INIT_AND_METADATA *EndpointData
    )
{
    USB_ENDPOINT_DESCRIPTOR descriptor;
    UDECX_USB_ENDPOINT_CALLBACKS callbacks;
    WDF_WORKITEM_CONFIG workItemConfig;
    WDF_OBJECT_ATTRIBUTES attributes;
    UDECXUSBENDPOINT endpoint;
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext;
    WDF_IO_QUEUE_DISPATCH_TYPE dispatchType;
    NTSTATUS status;

    PAGED_CODE();
    if (InterlockedCompareExchange(
            &ViiperGetControllerContext(
                ViiperGetDeviceContext(Device)->Controller)->ShuttingDown,
            0,
            0) != 0) {
        return STATUS_DEVICE_REMOVED;
    }
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
    InitializeListHead(&endpointContext->AdmissionQueue);
    KeInitializeEvent(&endpointContext->OperationsDrained, NotificationEvent, TRUE);
    WDF_WORKITEM_CONFIG_INIT(&workItemConfig, ViiperEvtEndpointPurgeWorkItem);
    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = endpoint;
    status = WdfWorkItemCreate(
        &workItemConfig, &attributes, &endpointContext->PurgeWorkItem);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    WDF_WORKITEM_CONFIG_INIT(&workItemConfig, ViiperEvtEndpointResetWorkItem);
    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = endpoint;
    status = WdfWorkItemCreate(
        &workItemConfig, &attributes, &endpointContext->ResetWorkItem);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    if (descriptor.bEndpointAddress == 0) {
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
    if (endpointContext->FastInput) {
        // A direct report can arrive just before Windows posts its interrupt
        // poll. Preserve that latest state and service the poll when the
        // manual endpoint queue changes from empty to non-empty. This mirrors
        // ViGEmBus's pending-read/cache contract without routing HID input
        // through the ordered control/media broker.
        status = WdfIoQueueReadyNotify(
            endpointContext->Queue, ViiperEvtFastInputQueueReady, endpoint);
        if (!NT_SUCCESS(status)) {
            return status;
        }
    }

    {
        VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);
        VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
            ViiperGetControllerContext(deviceContext->Controller);
        ViiperAcquireDeviceLockExclusive(controllerContext);
        if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0 &&
            InterlockedCompareExchange(&deviceContext->Purging, 0, 0) == 0) {
            if (descriptor.bEndpointAddress == 0) {
                deviceContext->DefaultEndpoint = endpoint;
            }
            deviceContext->Endpoints[descriptor.bEndpointAddress] = endpoint;
            deviceContext->RetiredEndpoints[descriptor.bEndpointAddress] = FALSE;
            status = STATUS_SUCCESS;
        } else {
            // UdeCx owns the just-created child and will reclaim it when this
            // endpoint-add callback rejects publication at the removal gate.
            status = STATUS_DEVICE_REMOVED;
        }
        ViiperReleaseDeviceLockExclusive(controllerContext);
    }
    return status;
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
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ WDFREQUEST Request,
    _In_ NTSTATUS Status
    )
{
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext =
        ViiperGetDeviceContext(ViiperGetEndpointContext(Endpoint)->Device);
    BOOLEAN queued;

    // The passive caller owns buffer validation/copying. Terminal completion
    // and the endpoint rundown release are transferred together to the DPC.
    queued = ViiperQueueUrbCompletion(
        deviceContext->Controller,
        Endpoint,
        Request,
        VIIPER_UDE_MAX_PENDING_OPERATIONS,
        0,
        Status,
        NT_SUCCESS(Status) ? USBD_STATUS_SUCCESS : USBD_STATUS_INTERNAL_HC_ERROR,
        !NT_SUCCESS(Status));
    if (!queued) {
        NT_ASSERT(FALSE);
    }
}

static
NTSTATUS
ViiperPrepareCachedInputUrb(
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ WDFREQUEST Request
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    PURB urb = ViiperGetUrb(Request);
    ULONG transferLength;
    NTSTATUS status;

    if (urb == NULL ||
        (urb->UrbHeader.Function != URB_FUNCTION_BULK_OR_INTERRUPT_TRANSFER &&
            urb->UrbHeader.Function != URB_FUNCTION_BULK_OR_INTERRUPT_TRANSFER_USING_CHAINED_MDL) ||
        (urb->UrbBulkOrInterruptTransfer.TransferFlags & USBD_TRANSFER_DIRECTION_IN) == 0) {
        return STATUS_INVALID_DEVICE_REQUEST;
    }
    transferLength = urb->UrbBulkOrInterruptTransfer.TransferBufferLength;
    if (endpointContext->InputReportLength > transferLength) {
        return STATUS_BUFFER_TOO_SMALL;
    }
    status = ViiperCopyTransferBuffer(
        Request,
        urb,
        endpointContext->InputReport,
        endpointContext->InputReportLength,
        TRUE);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    urb->UrbBulkOrInterruptTransfer.TransferBufferLength = endpointContext->InputReportLength;
    UdecxUrbSetBytesCompleted(Request, endpointContext->InputReportLength);
    InterlockedAdd64(&controllerContext->BytesFromDevice, endpointContext->InputReportLength);
    InterlockedIncrement64(&controllerContext->InputReportsCompleted);
    return STATUS_SUCCESS;
}

VOID
ViiperEvtFastInputQueueReady(
    _In_ WDFQUEUE Queue,
    _In_ WDFCONTEXT Context
    )
{
    UDECXUSBENDPOINT endpoint = (UDECXUSBENDPOINT)Context;
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    WDFREQUEST request = WDF_NO_HANDLE;
    NTSTATUS completionStatus = STATUS_SUCCESS;
    BOOLEAN admitted = FALSE;
    BOOLEAN deliveryReady = FALSE;
    BOOLEAN completionQueued = FALSE;

    PAGED_CODE();
    // KMDF explicitly permits a passive ReadyNotify callback to retrieve the
    // request that made a manual queue non-empty. Copy the already-cached
    // latest state here, then transfer terminal ownership to the driver's
    // completion DPC. The DPC is the separate DISPATCH_LEVEL boundary required
    // by the UDE/host-controller completion contract; a system work item before
    // that DPC only adds scheduler latency to the first poll after idle/resume.
    // Register endpoint rundown before any endpoint-local wait. A producer can
    // own InputLock while UdeCx begins PURGE; entering rundown first prevents
    // the passive purge worker from observing zero while this ReadyNotify
    // callback is already waiting to inspect the cached report.
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0 &&
        InterlockedCompareExchange(&controllerContext->BrokerFaulted, FALSE, FALSE) == FALSE &&
        InterlockedCompareExchange(&deviceContext->InD0, 0, 0) != 0 &&
        InterlockedCompareExchange(&deviceContext->Purging, 0, 0) == 0 &&
        InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) == 0 &&
        InterlockedCompareExchange(&endpointContext->Purging, 0, 0) == 0 &&
        InterlockedCompareExchange(&endpointContext->Resetting, 0, 0) == 0) {
        ViiperEndpointOperationStarted(endpoint);
        admitted = TRUE;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!admitted) {
        return;
    }

    WdfWaitLockAcquire(endpointContext->InputLock, NULL);
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0 &&
        InterlockedCompareExchange(&controllerContext->BrokerFaulted, FALSE, FALSE) == FALSE &&
        InterlockedCompareExchange(&deviceContext->InD0, 0, 0) != 0 &&
        InterlockedCompareExchange(&deviceContext->Purging, 0, 0) == 0 &&
        InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) == 0 &&
        InterlockedCompareExchange(&endpointContext->Purging, 0, 0) == 0 &&
        InterlockedCompareExchange(&endpointContext->Resetting, 0, 0) == 0 &&
        InterlockedCompareExchange(&endpointContext->InputReportValid, 0, 0) != 0 &&
        InterlockedCompareExchange(&endpointContext->CachedDeliveryPending, 0, 0) != 0) {
        deliveryReady = TRUE;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!deliveryReady) {
        WdfWaitLockRelease(endpointContext->InputLock);
        ViiperEndpointOperationCompleted(endpoint);
        return;
    }

    // One cached-delivery token represents one accepted publication which
    // arrived before its Windows poll. Consume exactly one parked request.
    // Completing it can cause HIDClass to post a successor; leaving that poll
    // parked prevents a cache replay loop and lets the next producer update
    // complete it on the allocation-free direct path.
    if (NT_SUCCESS(WdfIoQueueRetrieveNextRequest(Queue, &request))) {
        InterlockedExchange(&endpointContext->CachedDeliveryPending, FALSE);
        ViiperInvalidateInputIfLifecycleClosed(endpoint);
        completionStatus = ViiperPrepareCachedInputUrb(endpoint, request);
        completionQueued = TRUE;
    } else {
        ViiperInvalidateInputIfLifecycleClosed(endpoint);
    }
    WdfWaitLockRelease(endpointContext->InputLock);
    if (completionQueued) {
        // Queue only after the last endpoint-local lock access. The DPC may run
        // immediately and performs the final rundown release after UDE's
        // mandatory DISPATCH_LEVEL terminal completion.
        ViiperCompleteRetrievedInputUrb(endpoint, request, completionStatus);
    } else {
        // This is the final endpoint access: the locked decrement may let the
        // purge worker complete and permit UdeCx cleanup immediately after it.
        ViiperEndpointOperationCompleted(endpoint);
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
    UDECXUSBDEVICE device = WDF_NO_HANDLE;
    UDECXUSBENDPOINT endpoint = WDF_NO_HANDLE;
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = NULL;
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = NULL;
    WDFREQUEST urbRequest = WDF_NO_HANDLE;
    NTSTATUS status;
    BOOLEAN admitted = FALSE;
    BOOLEAN lifecycleDrop = FALSE;

    status = ViiperValidateBrokerOwner(controller, Request);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    // A broker fault means an ordered lifecycle notification was lost. The
    // completion path must remain available so already-published URBs can be
    // drained, but accepting a new direct interrupt-IN state after that point
    // could apply it to a generation whose reset/power boundary user mode did
    // not observe. Fail the producer lane and let Host terminate this one-shot
    // owner session when it dequeues ViiperUdeOperationBrokerFault.
    if (InterlockedCompareExchange(
            &controllerContext->BrokerFaulted, FALSE, FALSE) != FALSE) {
        return STATUS_DATA_ERROR;
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

    status = STATUS_NOT_FOUND;
    ViiperAcquireDeviceLockShared(controllerContext);
    device = ViiperFindInputDeviceLocked(controllerContext, input->DeviceId);
    if (device != WDF_NO_HANDLE) {
        deviceContext = ViiperGetDeviceContext(device);
        if (deviceContext->OwnerFile == ownerFile &&
            deviceContext->Generation == input->Generation) {
            if (InterlockedCompareExchange(&deviceContext->InD0, 0, 0) == 0 ||
                InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) != 0 ||
                InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0) {
                lifecycleDrop = TRUE;
                status = STATUS_SUCCESS;
            } else {
                endpoint = deviceContext->Endpoints[input->EndpointAddress];
                if (endpoint == WDF_NO_HANDLE) {
                    if (deviceContext->RetiredEndpoints[input->EndpointAddress]) {
                        lifecycleDrop = TRUE;
                        status = STATUS_SUCCESS;
                    }
                } else {
                    endpointContext = ViiperGetEndpointContext(endpoint);
                    if (!endpointContext->FastInput ||
                        endpointContext->InputLock == WDF_NO_HANDLE) {
                        status = STATUS_INVALID_DEVICE_STATE;
                    } else {
                        // The shared index pins the published endpoint through
                        // admission. BrokerLock is also the linearization point
                        // for lifecycle closure and every ActiveOperations
                        // 0 <-> 1 event transition. Once counted, UdeCx purge
                        // must drain this operation before cleanup may revoke
                        // the endpoint context.
                        WdfSpinLockAcquire(controllerContext->BrokerLock);
                        if (InterlockedCompareExchange(
                                &controllerContext->ShuttingDown, 0, 0) != 0 ||
                            InterlockedCompareExchange(&deviceContext->InD0, 0, 0) == 0 ||
                            InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0 ||
                            InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) != 0 ||
                            InterlockedCompareExchange(&endpointContext->Purging, 0, 0) != 0 ||
                            InterlockedCompareExchange(&endpointContext->Resetting, 0, 0) != 0) {
                            lifecycleDrop = TRUE;
                            status = STATUS_SUCCESS;
                        } else {
                            ViiperEndpointOperationStarted(endpoint);
                            admitted = TRUE;
                            status = STATUS_SUCCESS;
                        }
                        WdfSpinLockRelease(controllerContext->BrokerLock);
                    }
                }
            }
        }
    }
    ViiperReleaseDeviceLockShared(controllerContext);
    if (lifecycleDrop) {
        // A report already submitted by the owner may cross the D0/unplug
        // boundary before the ordered lifecycle notification cancels its
        // publisher. It is stale latest-state data, not a broken owner
        // session. Acknowledge and discard it exactly at that boundary.
        return STATUS_SUCCESS;
    }
    if (!admitted) {
        return status;
    }

    // The default IOCTL queue is parallel so independent controllers never
    // block one another. Serialize only this endpoint, preserving report order
    // even if a faulty or hostile owner submits concurrent updates for one pad.
    WdfWaitLockAcquire(endpointContext->InputLock, NULL);
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0 ||
        InterlockedCompareExchange(&deviceContext->InD0, 0, 0) == 0 ||
        InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0 ||
        InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) != 0 ||
        InterlockedCompareExchange(&endpointContext->Purging, 0, 0) != 0 ||
        InterlockedCompareExchange(&endpointContext->Resetting, 0, 0) != 0) {
        WdfSpinLockRelease(controllerContext->BrokerLock);
        WdfWaitLockRelease(endpointContext->InputLock);
        ViiperEndpointOperationCompleted(endpoint);
        // Endpoint purge/start and endpoint reset preserve the device
        // generation. A publisher can have one already-built latest-state
        // report crossing either callback; acknowledge and discard it rather
        // than faulting the otherwise valid owner session.
        return STATUS_SUCCESS;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (input->Sequence <= (ULONGLONG)InterlockedCompareExchange64(
            &endpointContext->LastInputSequence, 0, 0)) {
        WdfWaitLockRelease(endpointContext->InputLock);
        ViiperEndpointOperationCompleted(endpoint);
        return STATUS_INVALID_DEVICE_STATE;
    }
    // Claim and cache every accepted sequence, including when no Windows poll
    // is parked. The queue-ready callback will satisfy the next poll from this
    // exact latest state instead of waiting for or fabricating another feeder
    // update.
    InterlockedExchange64(&endpointContext->LastInputSequence, (LONG64)input->Sequence);
    RtlCopyMemory(endpointContext->InputReport, payload, input->PayloadLength);
    endpointContext->InputReportLength = input->PayloadLength;
    InterlockedExchange(&endpointContext->InputReportValid, TRUE);
    InterlockedIncrement64(&controllerContext->InputReportsSubmitted);
    status = WdfIoQueueRetrieveNextRequest(endpointContext->Queue, &urbRequest);
    if (!NT_SUCCESS(status)) {
        InterlockedExchange(
            &endpointContext->CachedDeliveryPending,
            status == STATUS_NO_MORE_ENTRIES ? TRUE : FALSE);
        ViiperInvalidateInputIfLifecycleClosed(endpoint);
        WdfWaitLockRelease(endpointContext->InputLock);
        ViiperEndpointOperationCompleted(endpoint);
        // The cached report now owns this state. Queue-ready delivery services
        // the next Windows poll even if the physical feeder becomes idle.
        return status == STATUS_NO_MORE_ENTRIES ? STATUS_SUCCESS : status;
    }
    InterlockedExchange(&endpointContext->CachedDeliveryPending, FALSE);
    // Lifecycle admission can close after this operation was admitted. The
    // pre-boundary poll may finish, but its cached state must never survive the
    // reset/purge/D0 boundary. Revalidate under the same admission lock so
    // either this path or the lifecycle callback performs the final clear.
    ViiperInvalidateInputIfLifecycleClosed(endpoint);
    status = ViiperPrepareCachedInputUrb(endpoint, urbRequest);
    WdfWaitLockRelease(endpointContext->InputLock);
    // This call is the active-operation handoff. It performs every remaining
    // endpoint lookup before enqueuing the DPC; the caller performs no endpoint
    // access after a concurrently running DPC can release rundown.
    ViiperCompleteRetrievedInputUrb(endpoint, urbRequest, status);
    return status;
}

VOID
ViiperEvtEndpointReset(
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ WDFREQUEST Request
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    NTSTATUS status;

    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0 ||
        InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0 ||
        InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) != 0 ||
        InterlockedCompareExchange(&endpointContext->Purging, 0, 0) != 0 ||
        InterlockedCompareExchange(&endpointContext->Resetting, TRUE, FALSE) != FALSE) {
        status = STATUS_DEVICE_BUSY;
    } else {
        status = STATUS_SUCCESS;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!NT_SUCCESS(status)) {
        WdfRequestComplete(Request, status);
        return;
    }

    InterlockedExchange64(&endpointContext->NextIsoStartFrame, 0);
    ViiperInvalidateEndpointInputReport(Endpoint);
    ViiperPurgeEndpointOperations(Endpoint, STATUS_DEVICE_NOT_READY);
    endpointContext->ResetRequest = Request;
    // A forwarded broker operation or direct input copy may have won
    // admission immediately before Resetting was raised. Defer publication of
    // the reset request until those owners have actually completed; otherwise
    // user mode could clear controller state while the old transfer is still
    // writing into the endpoint.
    WdfWorkItemEnqueue(endpointContext->ResetWorkItem);
}

VOID
ViiperEvtEndpointResetWorkItem(
    _In_ WDFWORKITEM WorkItem
    )
{
    UDECXUSBENDPOINT endpoint = (UDECXUSBENDPOINT)WdfWorkItemGetParentObject(WorkItem);
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(endpoint);
    WDFREQUEST request;
    NTSTATUS status;

    PAGED_CODE();
    (VOID)KeWaitForSingleObject(
        &endpointContext->OperationsDrained,
        Executive,
        KernelMode,
        FALSE,
        NULL);
    NT_ASSERT(InterlockedCompareExchange(
        &endpointContext->ActiveOperations, 0, 0) == 0);
    // An input publisher admitted immediately before Resetting was raised is
    // allowed to finish, then this barrier performs the final invalidation.
    ViiperInvalidateEndpointInputReport(endpoint);
    request = endpointContext->ResetRequest;
    endpointContext->ResetRequest = WDF_NO_HANDLE;
    if (InterlockedCompareExchange(&endpointContext->Purging, 0, 0) != 0) {
        InterlockedExchange(&endpointContext->Resetting, FALSE);
        WdfRequestComplete(request, STATUS_DEVICE_NOT_READY);
        return;
    }
    status = ViiperQueueAcknowledgedEndpointLifecycleEvent(
        endpoint, request, ViiperUdeOperationEndpointReset);
    if (!NT_SUCCESS(status)) {
        InterlockedExchange(&endpointContext->Resetting, FALSE);
        WdfRequestComplete(request, status);
    }
}

VOID
ViiperEvtEndpointPurgeWorkItem(
    _In_ WDFWORKITEM WorkItem
    )
{
    UDECXUSBENDPOINT endpoint = (UDECXUSBENDPOINT)WdfWorkItemGetParentObject(WorkItem);
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(endpoint);

    PAGED_CODE();
    // UdeCx requires every request forwarded out of the endpoint queue to be
    // completed before PurgeComplete. The shared completion DPC releases both
    // broker and direct-input ownership only after the terminal UdeCx call.
    (VOID)KeWaitForSingleObject(
        &endpointContext->OperationsDrained,
        Executive,
        KernelMode,
        FALSE,
        NULL);
    NT_ASSERT(InterlockedCompareExchange(
        &endpointContext->ActiveOperations, 0, 0) == 0);
    // The admission barrier is closed and all pre-boundary publishers have
    // drained, so no cached state can be republished after this clear.
    ViiperInvalidateEndpointInputReport(endpoint);
    UdecxUsbEndpointPurgeComplete(endpoint);
}

VOID
ViiperEvtEndpointPurge(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);

    // Serialize the admission gate with both pending-slot allocation and the
    // direct input fast path. This makes OperationsDrained a reliable purge
    // barrier instead of allowing a transfer to start after the work item has
    // already observed the event as signaled.
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    InterlockedExchange(&endpointContext->Purging, TRUE);
    WdfSpinLockRelease(controllerContext->BrokerLock);
    InterlockedExchange64(&endpointContext->NextIsoStartFrame, 0);
    ViiperInvalidateEndpointInputReport(Endpoint);
    ViiperPurgeEndpointOperations(Endpoint, STATUS_DEVICE_NOT_READY);
    (VOID)ViiperQueueEndpointLifecycleEvent(Endpoint, ViiperUdeOperationEndpointPurge);
    // UdeCx owns and has already stopped the associated queue before PURGE;
    // client drivers must not change that queue's state. Only callbacks already
    // forwarded to our broker/direct paths remain, and each is covered by the
    // ActiveOperations fence before this passive work item may report complete.
    WdfWorkItemEnqueue(endpointContext->PurgeWorkItem);
}

VOID
ViiperEvtEndpointStart(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);

    // UdeCx defines START as the boundary at which both the endpoint queue and
    // any client-owned forwarded paths may resume. Open the kernel admission
    // gate before publishing that boundary to user mode. Publishing first lets
    // the newly started input publisher race back through SUBMIT_INPUT_REPORT
    // while Purging is still true, consuming and discarding the first fresh
    // sequence after resume.
    InterlockedExchange64(&endpointContext->NextIsoStartFrame, 0);
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0) {
        InterlockedExchange(&endpointContext->Purging, FALSE);
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0) {
        (VOID)ViiperQueueEndpointLifecycleEvent(Endpoint, ViiperUdeOperationEndpointStart);
    }
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
        status = ViiperBeginAcknowledgedDeviceReset(Device, Request);
        break;
    case UdecxEndpointsConfigureTypeInterfaceSettingChange:
        status = ViiperQueueAcknowledgedInterfaceLifecycleEvent(
            Device,
            Request,
            ConfigureParams->InterfaceNumber,
            ConfigureParams->NewInterfaceSetting);
        break;
    case UdecxEndpointsConfigureTypeEndpointsReleasedOnly:
        WdfRequestComplete(Request, STATUS_SUCCESS);
        return;
        break;
    default:
        status = STATUS_INVALID_PARAMETER;
        break;
    }
    if (!NT_SUCCESS(status)) {
        WdfRequestComplete(Request, status);
    }
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
            ViiperCompleteUnownedUrb(
                WdfIoQueueGetDevice(Queue), Request, status);
        }
    } else {
        WdfRequestComplete(Request, STATUS_INVALID_DEVICE_REQUEST);
    }
}

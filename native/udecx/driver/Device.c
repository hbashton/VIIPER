/*
 * Dynamic UdeCx device and endpoint lifecycle.
 *
 * The endpoint creation and purge order follows the documented UdeCx contract.
 * VIIPER-specific ownership, identity, and broker semantics are implemented
 * here.
 */

#include "ViiperUde.h"

#ifdef ALLOC_PRAGMA
#pragma alloc_text(PAGE, ViiperCreateVirtualDevice)
#pragma alloc_text(PAGE, ViiperDestroyVirtualDevice)
#pragma alloc_text(PAGE, ViiperDestroyOwnedDevices)
#pragma alloc_text(PAGE, ViiperEvtEndpointAdd)
#pragma alloc_text(PAGE, ViiperEvtDefaultEndpointAdd)
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
    const UCHAR *cursor = Descriptor;
    ULONG remaining = Length;

    if (remaining < 2 || cursor[1] != ExpectedType) {
        return FALSE;
    }
    while (remaining != 0) {
        ULONG itemLength;
        if (remaining < 2) {
            return FALSE;
        }
        itemLength = cursor[0];
        if (itemLength < 2 || itemLength > remaining) {
            return FALSE;
        }
        cursor += itemLength;
        remaining -= itemLength;
    }
    return TRUE;
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
    _Out_ ULONG *Slot,
    _Out_ ULONGLONG *PortReservation
    )
{
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);
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
            if (!ControllerContext->PortReserved[index] &&
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
        status = ViiperInsertInputDeviceLocked(ControllerContext, Device);
        if (NT_SUCCESS(status)) {
            ULONGLONG reservation = ++ControllerContext->PortReservationEpochs[freeSlot];
            if (reservation == 0) {
                reservation = ++ControllerContext->PortReservationEpochs[freeSlot];
            }
            NT_ASSERT(InterlockedCompareExchange(
                &deviceContext->ActiveCounted, 0, 0) == 0);
            deviceContext->Slot = freeSlot;
            deviceContext->PortReservation = reservation;
            // Publish a complete lifecycle record before PlugIn can expose
            // the object to UdeCx. For a claimed object, Plugged means that
            // successful exposure must be unwound with PlugOutAndDelete.
            deviceContext->Plugged = TRUE;
            ControllerContext->PortReserved[freeSlot] = TRUE;
            InterlockedIncrement(&ControllerContext->ReservedPorts);
            ControllerContext->Devices[freeSlot] = Device;
            InterlockedIncrement(&ControllerContext->ActiveDevices);
            InterlockedExchange(&deviceContext->ActiveCounted, 1);
            *Slot = freeSlot;
            *PortReservation = reservation;
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
    _In_ ULONG Slot,
    _In_ ULONGLONG PortReservation
    )
{
    ViiperAcquireDeviceLockExclusive(ControllerContext);
    if (Slot < VIIPER_UDE_MAX_DEVICES && PortReservation != 0 &&
        ControllerContext->PortReserved[Slot] &&
        ControllerContext->PortReservationEpochs[Slot] == PortReservation) {
        if (ControllerContext->Devices[Slot] == Device) {
            ViiperRemoveInputDeviceLocked(ControllerContext, Device);
            ControllerContext->Devices[Slot] = WDF_NO_HANDLE;
        }
        ControllerContext->PortReserved[Slot] = FALSE;
        {
            LONG remaining = InterlockedDecrement(&ControllerContext->ReservedPorts);
            NT_ASSERT(remaining >= 0);
            (VOID)remaining;
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

_IRQL_requires_(PASSIVE_LEVEL)
static
VOID
ViiperFlushD0ExitWorkItem(
    _In_ UDECXUSBDEVICE Device
    )
{
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);
    NT_ASSERT(deviceContext->D0ExitWorkItem != WDF_NO_HANDLE);
    // Flush unconditionally: the worker clears D0ExitPending immediately
    // before its final UdeCx completion call, so a false flag does not prove
    // that the callback has returned and stopped using the device handle.
    WdfWorkItemFlush(deviceContext->D0ExitWorkItem);
    NT_ASSERT(InterlockedCompareExchange(
        &deviceContext->D0ExitPending, 0, 0) == 0);
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
    VIIPER_UDE_CREATE_DEVICE_RESULT *output;
    size_t outputLength;
    WDFFILEOBJECT ownerFile;
    PUDECXUSBDEVICE_INIT deviceInit;
    UDECX_USB_DEVICE_STATE_CHANGE_CALLBACKS callbacks;
    UDECX_USB_DEVICE_SPEED speed;
    WDF_OBJECT_ATTRIBUTES attributes;
    WDF_WORKITEM_CONFIG workItemConfig;
    UDECXUSBDEVICE device = WDF_NO_HANDLE;
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext;
    UDECX_USB_DEVICE_PLUG_IN_OPTIONS plugOptions;
    ULONG slot;
    ULONG generation;
    ULONG requestedSpeed;
    ULONGLONG deviceId;
    ULONGLONG portReservation;

    PAGED_CODE();
    status = WdfRequestRetrieveInputBuffer(Request, sizeof(*input), (PVOID *)&input, &inputLength);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    if (!ViiperValidateCreateDevice(input, inputLength)) {
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        return STATUS_INVALID_PARAMETER;
    }
    // Validate the complete output contract before acquiring ownership or
    // mutating UdeCx. METHOD_BUFFERED aliases the input and output system
    // buffer, so do not write the receipt until every input descriptor has
    // been consumed and PlugIn has committed successfully.
    status = WdfRequestRetrieveOutputBuffer(
        Request, sizeof(*output), (PVOID *)&output, &outputLength);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    deviceId = input->DeviceId;
    generation = input->Generation;
    requestedSpeed = input->Speed;
    speed = ViiperMapSpeed(input->Speed);
    if (speed == (UDECX_USB_DEVICE_SPEED)0) {
        return STATUS_NOT_SUPPORTED;
    }
    status = ViiperBeginOwnerAdmission(controller, Request, &ownerFile);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    VIIPER_TRACE_LIFECYCLE(
        controller, VIIPER_UDE_TRACE_SOURCE_DEVICE, VIIPER_UDE_TRACE_CREATE_BEGIN,
        deviceId, generation, WDF_NO_HANDLE, WDF_NO_HANDLE, 0,
        STATUS_SUCCESS, 0, 0);

    deviceInit = UdecxUsbDeviceInitAllocate(controller);
    if (deviceInit == NULL) {
        status = STATUS_INSUFFICIENT_RESOURCES;
        goto ExitAdmission;
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
        goto ExitAdmission;
    }

    WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(&attributes, VIIPER_UDE_DEVICE_CONTEXT);
    attributes.ParentObject = controller;
    attributes.EvtCleanupCallback = ViiperEvtVirtualDeviceCleanup;
    // Keep ordinary WDF cleanup and child-owned callbacks passive. This object
    // attribute does not narrow the documented <= DISPATCH_LEVEL contract of
    // UdeCx state callbacks; those paths remain independently dispatch-safe.
    attributes.ExecutionLevel = WdfExecutionLevelPassive;
    status = UdecxUsbDeviceCreate(&deviceInit, &attributes, &device);
    VIIPER_TRACE_LIFECYCLE(
        controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
        VIIPER_UDE_TRACE_DEVICE_CREATE_RETURNED, deviceId,
        generation, device, WDF_NO_HANDLE, 0, status, 0, 0);
    if (!NT_SUCCESS(status)) {
        UdecxUsbDeviceInitFree(deviceInit);
        goto ExitAdmission;
    }

    deviceContext = ViiperGetDeviceContext(device);
    RtlZeroMemory(deviceContext, sizeof(*deviceContext));
    deviceContext->Controller = controller;
    deviceContext->OwnerFile = ownerFile;
    deviceContext->DeviceId = deviceId;
    deviceContext->Generation = generation;
    deviceContext->Slot = VIIPER_UDE_MAX_DEVICES;
    deviceContext->Speed = speed;
    deviceContext->MaxPendingOperations = input->MaxPendingOperations;
    // A newly attached virtual USB device is already in working link state.
    // UdeCx invokes LinkPowerEntry only when a later request resumes the child
    // from low power, so waiting for that callback leaves the first selected
    // endpoints permanently closed on systems which never suspend them.
    InterlockedExchange(&deviceContext->InD0, TRUE);
    WdfObjectReference(ownerFile);
    InterlockedExchange(&deviceContext->OwnerReferenced, 1);

    WDF_WORKITEM_CONFIG_INIT(&workItemConfig, ViiperEvtUsbDeviceD0ExitWorkItem);
    workItemConfig.AutomaticSerialization = WdfFalse;
    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = device;
    status = WdfWorkItemCreate(
        &workItemConfig, &attributes, &deviceContext->D0ExitWorkItem);
    if (!NT_SUCCESS(status)) {
        WdfObjectDelete(device);
        goto ExitAdmission;
    }

    status = ViiperClaimDeviceSlot(
        controllerContext, device, deviceId, &slot, &portReservation);
    if (!NT_SUCCESS(status)) {
        WdfObjectDelete(device);
        goto ExitAdmission;
    }
    VIIPER_TRACE_LIFECYCLE(
        controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
        VIIPER_UDE_TRACE_DEVICE_SLOT_CLAIMED, deviceId,
        generation, device, WDF_NO_HANDLE, 0, STATUS_SUCCESS,
        InterlockedCompareExchange(&controllerContext->ReservedPorts, 0, 0), slot);

    UDECX_USB_DEVICE_PLUG_IN_OPTIONS_INIT(&plugOptions);
    if (speed == UdecxUsbSuperSpeed) {
        // UdeCx uses one controller-global namespace: USB 3 ports begin
        // immediately after NumberOfUsb20Ports, not again at port one.
        plugOptions.Usb30PortNumber =
            (USHORT)(VIIPER_UDE_USB20_PORT_COUNT + slot + 1);
    } else {
        plugOptions.Usb20PortNumber = (USHORT)(slot + 1);
    }
    VIIPER_TRACE_LIFECYCLE(
        controller, VIIPER_UDE_TRACE_SOURCE_DEVICE, VIIPER_UDE_TRACE_PLUG_IN_BEGIN,
        deviceId, generation, device, WDF_NO_HANDLE,
        0, STATUS_SUCCESS, 0, 0);
    status = UdecxUsbDevicePlugIn(device, &plugOptions);
    VIIPER_TRACE_LIFECYCLE(
        controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
        VIIPER_UDE_TRACE_PLUG_IN_RETURNED, deviceId,
        generation, device, WDF_NO_HANDLE, 0, status, 0, 0);
    if (!NT_SUCCESS(status)) {
        ViiperReleaseDeviceSlot(controllerContext, device, slot, portReservation);
        ViiperRetireActiveDevice(controllerContext, deviceContext);
        WdfObjectDelete(device);
        goto ExitAdmission;
    }

    RtlZeroMemory(output, sizeof(*output));
    output->Header.Magic = VIIPER_UDE_MAGIC;
    output->Header.Major = VIIPER_UDE_ABI_MAJOR;
    output->Header.Minor = VIIPER_UDE_ABI_MINOR;
    output->Header.Size = sizeof(*output);
    output->DeviceId = deviceId;
    output->Generation = generation;
    output->Speed = requestedSpeed;
    output->Usb20PortNumber = plugOptions.Usb20PortNumber;
    output->Usb30PortNumber = plugOptions.Usb30PortNumber;
    WdfRequestSetInformation(Request, sizeof(*output));
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
        // Revoke logical ownership immediately, but keep the physical port
        // reserved until this exact object's cleanup callback. Reusing a port
        // while its prior child is still disappearing can strand that child
        // and prevent the successor from enumerating.
        ControllerContext->Devices[index] = WDF_NO_HANDLE;
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
    VIIPER_TRACE_LIFECYCLE(
        controller, VIIPER_UDE_TRACE_SOURCE_DEVICE, VIIPER_UDE_TRACE_REMOVE_CLAIMED,
        input->DeviceId, input->Generation, device, WDF_NO_HANDLE, 0,
        STATUS_SUCCESS, 0, 0);
    ViiperFlushD0ExitWorkItem(device);
    ViiperAbortDeviceManagementOperations(controller, device, STATUS_DEVICE_REMOVED);
    VIIPER_TRACE_LIFECYCLE(
        controller, VIIPER_UDE_TRACE_SOURCE_DEVICE, VIIPER_UDE_TRACE_PLUG_OUT_BEGIN,
        input->DeviceId, input->Generation, device, WDF_NO_HANDLE, 0,
        STATUS_SUCCESS, 0, 0);
    status = UdecxUsbDevicePlugOutAndDelete(device);
    VIIPER_TRACE_LIFECYCLE(
        controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
        VIIPER_UDE_TRACE_PLUG_OUT_RETURNED, input->DeviceId, input->Generation,
        device, WDF_NO_HANDLE, 0, status, 0, 0);
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
        BOOLEAN plugged;
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
        plugged = deviceContext->Plugged;
        ViiperFlushD0ExitWorkItem(device);
        ViiperAbortDeviceManagementOperations(Controller, device, STATUS_FILE_CLOSED);
        // Completing a held UdeCx management request can make framework
        // cleanup runnable once the slot pins are released. Do not access the
        // device context after the exact-device management drain.
        if (plugged) {
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

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);

    VIIPER_TRACE_LIFECYCLE(
        Controller, VIIPER_UDE_TRACE_SOURCE_CONTROLLER,
        VIIPER_UDE_TRACE_CONTROLLER_SHUTDOWN_BEGIN, 0, 0, WDF_NO_HANDLE,
        WDF_NO_HANDLE, 0, STATUS_SUCCESS, 0, 0);

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
        BOOLEAN plugged = deviceContext->Plugged;
        ULONGLONG deviceId = deviceContext->DeviceId;
        ULONG generation = deviceContext->Generation;

        ViiperFlushD0ExitWorkItem(devices[index]);
        if (plugged) {
            NTSTATUS status;

            // A successful call starts UdeCx-owned asynchronous deletion. If
            // UdeCx rejects the request during controller removal, ordinary
            // parent teardown still owns and deletes the child object.
            VIIPER_TRACE_LIFECYCLE(
                Controller, VIIPER_UDE_TRACE_SOURCE_CONTROLLER,
                VIIPER_UDE_TRACE_PLUG_OUT_BEGIN, deviceId, generation,
                devices[index], WDF_NO_HANDLE, 0, STATUS_SUCCESS, 0, 0);
            status = UdecxUsbDevicePlugOutAndDelete(devices[index]);
            VIIPER_TRACE_LIFECYCLE(
                Controller, VIIPER_UDE_TRACE_SOURCE_CONTROLLER,
                VIIPER_UDE_TRACE_PLUG_OUT_RETURNED, deviceId, generation,
                devices[index], WDF_NO_HANDLE, 0, status, 0, 0);
        } else {
            WdfObjectDelete(devices[index]);
        }
    }
    VIIPER_TRACE_LIFECYCLE(
        Controller, VIIPER_UDE_TRACE_SOURCE_CONTROLLER,
        VIIPER_UDE_TRACE_CONTROLLER_SHUTDOWN_END, 0, 0, WDF_NO_HANDLE,
        WDF_NO_HANDLE, 0, STATUS_SUCCESS,
        InterlockedCompareExchange(&controllerContext->ReservedPorts, 0, 0), 0);
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

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);
    if (deviceContext->Controller == WDF_NO_HANDLE) {
        return;
    }
    controllerContext = ViiperGetControllerContext(deviceContext->Controller);
    VIIPER_TRACE_LIFECYCLE(
        deviceContext->Controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
        VIIPER_UDE_TRACE_DEVICE_CLEANUP_BEGIN, deviceContext->DeviceId,
        deviceContext->Generation, device, WDF_NO_HANDLE, 0, STATUS_SUCCESS,
        deviceContext->PendingOperations, 0);

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

    ViiperReleaseDeviceSlot(
        controllerContext, device, deviceContext->Slot,
        deviceContext->PortReservation);
    // Normal removal retired the logical count before PlugOutAndDelete. This
    // is only the fallback for an unexpected framework-owned deletion.
    ViiperRetireActiveDevice(controllerContext, deviceContext);
    if (ownerFile != WDF_NO_HANDLE) {
        WdfObjectDereference(ownerFile);
    }
    VIIPER_TRACE_LIFECYCLE(
        deviceContext->Controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
        VIIPER_UDE_TRACE_DEVICE_CLEANUP_END, deviceContext->DeviceId,
        deviceContext->Generation, device, WDF_NO_HANDLE, 0, STATUS_SUCCESS,
        deviceContext->PendingOperations,
        InterlockedCompareExchange(&controllerContext->ReservedPorts, 0, 0));
}

static
VOID
ViiperClearEndpointInputReportLocked(
    _In_ VIIPER_UDE_ENDPOINT_CONTEXT *EndpointContext
    )
{
    InterlockedExchange(&EndpointContext->InputReportValid, FALSE);
    InterlockedExchange(&EndpointContext->CachedDeliveryPending, FALSE);
    InterlockedExchange(&EndpointContext->InputSnapshotPending, FALSE);
    InterlockedExchange(&EndpointContext->InputTransitionHead, 0);
    InterlockedExchange(&EndpointContext->InputTransitionCount, 0);
}

_IRQL_requires_(PASSIVE_LEVEL)
static
VOID
ViiperInvalidateEndpointInputReport(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);
    if (endpointContext->InputLock != WDF_NO_HANDLE) {
        WdfWaitLockAcquire(endpointContext->InputLock, NULL);
    }
    ViiperClearEndpointInputReportLocked(endpointContext);
    if (endpointContext->InputLock != WDF_NO_HANDLE) {
        WdfWaitLockRelease(endpointContext->InputLock);
    }
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
        // Both callers already own InputLock. Clearing under that same lock
        // makes lifecycle invalidation atomic with FIFO append/dequeue.
        ViiperClearEndpointInputReportLocked(endpointContext);
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
}

_IRQL_requires_(PASSIVE_LEVEL)
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

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);
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
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);
    NTSTATUS status = STATUS_SUCCESS;

    // This callback is the exact UdeCx power boundary. Open direct input
    // admission before publishing the ordered advisory event to user mode.
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0) {
        status = STATUS_DEVICE_REMOVED;
    } else if (InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0 ||
        InterlockedCompareExchange(&deviceContext->D0ExitPending, 0, 0) != 0) {
        status = STATUS_DEVICE_BUSY;
    } else {
        InterlockedExchange(&deviceContext->InD0, TRUE);
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!NT_SUCCESS(status)) {
        return status;
    }
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
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);
    NTSTATUS status;

    UNREFERENCED_PARAMETER(WakeSetting);
    // Close direct input admission synchronously. Waiting for the user-mode
    // notification would leave a scheduler window in which a fresh report
    // could complete a Windows poll after the child had left D0.
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    InterlockedExchange(&deviceContext->InD0, FALSE);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0 ||
        InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0) {
        // Teardown already owns cache destruction and will consume the handle.
        // No asynchronous power completion is owed when this callback returns
        // success synchronously.
        status = STATUS_SUCCESS;
    } else if (InterlockedCompareExchange(
            &deviceContext->D0ExitPending, TRUE, FALSE) != FALSE) {
        NT_ASSERT(FALSE);
        status = STATUS_DEVICE_BUSY;
    } else {
        // Enqueue before releasing the same gate which removal uses to set
        // Purging. Teardown therefore either observes and flushes this work or
        // wins first and prevents a late enqueue against a consumed handle.
        WdfWorkItemEnqueue(deviceContext->D0ExitWorkItem);
        status = STATUS_PENDING;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    return status;
}

VOID
ViiperEvtUsbDeviceD0ExitWorkItem(
    _In_ WDFWORKITEM WorkItem
    )
{
    UDECXUSBDEVICE device =
        (UDECXUSBDEVICE)WdfWorkItemGetParentObject(WorkItem);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);
    ViiperInvalidateDeviceInputReports(device);
    (VOID)ViiperQueueDeviceLifecycleEvent(
        device, ViiperUdeOperationDeviceD0Exit);
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    NT_ASSERT(InterlockedCompareExchange(
        &deviceContext->D0ExitPending, 0, 0) != 0);
    InterlockedExchange(&deviceContext->D0ExitPending, FALSE);
    WdfSpinLockRelease(controllerContext->BrokerLock);
    // UdeCx may synchronously advance lifecycle or cleanup once this returns.
    // Do not access the device or any of its contexts after completion.
    UdecxUsbDeviceLinkPowerExitComplete(device, STATUS_SUCCESS);
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
    // host's bookkeeping transition without mutating endpoint/media state
    // behind UdeCx's queue lifecycle. If VIIPER adds a SuperSpeed controller
    // with real remote-wake behavior, that device must add an explicit
    // per-interface state contract
    // rather than repurposing endpoint purge/start implicitly.
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
    VIIPER_TRACE_LIFECYCLE(
        deviceContext->Controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
        VIIPER_UDE_TRACE_ENDPOINT_CLEANUP_BEGIN, deviceContext->DeviceId,
        deviceContext->Generation, endpointContext->Device, endpoint, address,
        STATUS_SUCCESS, endpointContext->ActiveOperations, 0);
    ViiperAcquireDeviceLockExclusive(controllerContext);
    // Microsoft permits no ordinary object access after EvtCleanup is called,
    // even when a WDF reference postpones destruction. UdeCx therefore owns
    // the lifetime ordering: EvtEndpointPurge closes BrokerLock admission while
    // UdeCx owns its associated queue state. The passive work item acknowledges
    // UdeCx only after all framework-delivered and VIIPER-owned requests end.
    // Endpoint creation failure has no published users. Cleanup must never be
    // used as a late wait for an operation which can still access this context.
    NT_ASSERT(InterlockedCompareExchange(
        &endpointContext->ActiveOperations, 0, 0) == 0);
    NT_ASSERT(InterlockedCompareExchange(
        &endpointContext->PurgeOutstanding, 0, 0) == 0);
    NT_ASSERT(InterlockedCompareExchange(
        &endpointContext->PurgeWorkerActive, 0, 0) == 0);
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
    VIIPER_TRACE_LIFECYCLE(
        deviceContext->Controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
        VIIPER_UDE_TRACE_ENDPOINT_CLEANUP_END, deviceContext->DeviceId,
        deviceContext->Generation, endpointContext->Device, endpoint, address,
        STATUS_SUCCESS, endpointContext->ActiveOperations, 0);
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
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    WDF_IO_QUEUE_DISPATCH_TYPE dispatchType;
    NTSTATUS status;

    PAGED_CODE();
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0) {
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
    // Allocate an address-scoped incarnation before any queue or work-item can
    // publish ownership. Failed creations deliberately consume a generation;
    // no future endpoint may reuse an identity observed by a delayed callback.
    ViiperAcquireDeviceLockExclusive(controllerContext);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) != 0 ||
        InterlockedCompareExchange(&deviceContext->Purging, 0, 0) != 0) {
        status = STATUS_DEVICE_REMOVED;
    } else if (deviceContext->Endpoints[descriptor.bEndpointAddress] != WDF_NO_HANDLE ||
        (descriptor.bEndpointAddress == 0 &&
         deviceContext->DefaultEndpoint != WDF_NO_HANDLE)) {
        // A duplicate add must not advance the address generation while the
        // published incarnation is still live. Direct-input validation treats
        // EndpointGenerations[address] as the live endpoint's exact identity.
        status = STATUS_OBJECT_NAME_COLLISION;
    } else if (deviceContext->EndpointGenerations[
            descriptor.bEndpointAddress] == MAXULONG) {
        status = STATUS_INTEGER_OVERFLOW;
    } else {
        ULONG generation = deviceContext->EndpointGenerations[
            descriptor.bEndpointAddress] + 1;
        deviceContext->EndpointGenerations[descriptor.bEndpointAddress] = generation;
        endpointContext->Generation = generation;
        status = STATUS_SUCCESS;
    }
    ViiperReleaseDeviceLockExclusive(controllerContext);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    WDF_WORKITEM_CONFIG_INIT(&workItemConfig, ViiperEvtEndpointPurgeWorkItem);
    workItemConfig.AutomaticSerialization = WdfFalse;
    WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
    attributes.ParentObject = endpoint;
    status = WdfWorkItemCreate(
        &workItemConfig, &attributes, &endpointContext->PurgeWorkItem);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    WDF_WORKITEM_CONFIG_INIT(&workItemConfig, ViiperEvtEndpointResetWorkItem);
    workItemConfig.AutomaticSerialization = WdfFalse;
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
        ULONG packetBytes = descriptor.wMaxPacketSize & 0x07ff;
        ULONG transactions = 1 + ((descriptor.wMaxPacketSize >> 11) & 0x03);
        SIZE_T transitionBytes;

        endpointContext->FastInput = TRUE;
        dispatchType = WdfIoQueueDispatchManual;
        WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
        attributes.ParentObject = endpoint;
        status = WdfWaitLockCreate(&attributes, &endpointContext->InputLock);
        if (!NT_SUCCESS(status)) {
            return status;
        }
        endpointContext->InputTransitionStride = packetBytes * transactions;
        if (endpointContext->InputTransitionStride == 0 ||
            endpointContext->InputTransitionStride > VIIPER_UDE_MAX_INPUT_REPORT_BYTES) {
            return STATUS_INVALID_PARAMETER;
        }
        endpointContext->InputTransitionCapacity = min(
            VIIPER_UDE_MAX_INPUT_TRANSITIONS,
            VIIPER_UDE_MAX_INPUT_TRANSITION_BYTES / endpointContext->InputTransitionStride);
        if (endpointContext->InputTransitionCapacity == 0) {
            return STATUS_INVALID_PARAMETER;
        }
        transitionBytes = (SIZE_T)endpointContext->InputTransitionStride *
            endpointContext->InputTransitionCapacity;
        WDF_OBJECT_ATTRIBUTES_INIT(&attributes);
        attributes.ParentObject = endpoint;
        status = WdfMemoryCreate(
            &attributes,
            NonPagedPoolNx,
            0x56495549,
            transitionBytes,
            &endpointContext->InputTransitionMemory,
            (PVOID *)&endpointContext->InputTransitionReports);
        if (!NT_SUCCESS(status)) {
            endpointContext->InputTransitionMemory = WDF_NO_HANDLE;
            endpointContext->InputTransitionReports = NULL;
            return status;
        }
        RtlZeroMemory(endpointContext->InputTransitionReports, transitionBytes);
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
        // manual endpoint queue changes from empty to non-empty. Keep this
        // pending-read/cache path separate from the ordered control/media
        // broker.
        status = WdfIoQueueReadyNotify(
            endpointContext->Queue, ViiperEvtFastInputQueueReady, endpoint);
        if (!NT_SUCCESS(status)) {
            return status;
        }
    }

    {
        UCHAR address = descriptor.bEndpointAddress;
        ViiperAcquireDeviceLockExclusive(controllerContext);
        if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0 &&
            InterlockedCompareExchange(&deviceContext->Purging, 0, 0) == 0 &&
            endpointContext->Generation != 0 &&
            deviceContext->EndpointGenerations[address] == endpointContext->Generation &&
            deviceContext->Endpoints[address] == WDF_NO_HANDLE) {
            if (descriptor.bEndpointAddress == 0) {
                deviceContext->DefaultEndpoint = endpoint;
            }
            deviceContext->Endpoints[address] = endpoint;
            deviceContext->RetiredEndpoints[address] = FALSE;
            InterlockedExchange64(&deviceContext->EndpointSequences[address], 0);
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
    _In_ NTSTATUS Status,
    _In_ ULONG DirectInputBytes,
    _In_ ULONGLONG DirectInputSequence
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext =
        ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_REQUEST_CONTEXT *requestContext = ViiperGetRequestContext(Request);
    BOOLEAN queued;

    // The passive caller owns buffer validation/copying. Terminal completion
    // and the endpoint rundown release are transferred together to the DPC.
    RtlZeroMemory(requestContext, sizeof(*requestContext));
    requestContext->Controller = deviceContext->Controller;
    requestContext->Endpoint = Endpoint;
    requestContext->PendingSlot = VIIPER_UDE_MAX_PENDING_OPERATIONS;
    requestContext->DeviceGeneration = deviceContext->Generation;
    requestContext->EndpointGeneration = endpointContext->Generation;
    queued = ViiperQueueUrbCompletion(
        deviceContext->Controller,
        Endpoint,
        Request,
        VIIPER_UDE_MAX_PENDING_OPERATIONS,
        0,
        Status,
        NT_SUCCESS(Status) ? USBD_STATUS_SUCCESS : USBD_STATUS_INTERNAL_HC_ERROR,
        !NT_SUCCESS(Status),
        NT_SUCCESS(Status) ? DirectInputBytes : 0,
        NT_SUCCESS(Status) ? DirectInputSequence : 0);
    if (!queued) {
        NT_ASSERT(FALSE);
    }
}

static
NTSTATUS
ViiperPrepareCachedInputUrb(
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ WDFREQUEST Request,
    _Out_ ULONG *BytesPrepared,
    _Out_ ULONGLONG *SequencePrepared
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    PURB urb = ViiperGetUrb(Request);
    PUCHAR report = endpointContext->InputReport;
    ULONG reportLength = endpointContext->InputReportLength;
    ULONG transitionHead = 0;
    ULONGLONG reportSequence;
    BOOLEAN transition = FALSE;
    ULONG transferLength;
    NTSTATUS status;

    *BytesPrepared = 0;
    *SequencePrepared = 0;
    reportSequence = (ULONGLONG)InterlockedCompareExchange64(
        &endpointContext->LastInputSequence, 0, 0);

    if (InterlockedCompareExchange(&endpointContext->InputTransitionCount, 0, 0) > 0) {
        transitionHead = (ULONG)InterlockedCompareExchange(
            &endpointContext->InputTransitionHead, 0, 0);
        report = endpointContext->InputTransitionReports +
            ((SIZE_T)transitionHead * endpointContext->InputTransitionStride);
        reportLength = endpointContext->InputTransitionLengths[transitionHead];
        reportSequence = endpointContext->InputTransitionSequences[transitionHead];
        transition = TRUE;
    }

    if (urb == NULL ||
        (urb->UrbHeader.Function != URB_FUNCTION_BULK_OR_INTERRUPT_TRANSFER &&
            urb->UrbHeader.Function != URB_FUNCTION_BULK_OR_INTERRUPT_TRANSFER_USING_CHAINED_MDL) ||
        (urb->UrbBulkOrInterruptTransfer.TransferFlags & USBD_TRANSFER_DIRECTION_IN) == 0) {
        return STATUS_INVALID_DEVICE_REQUEST;
    }
    transferLength = urb->UrbBulkOrInterruptTransfer.TransferBufferLength;
    if (reportLength > transferLength) {
        status = STATUS_BUFFER_TOO_SMALL;
    } else {
        status = ViiperCopyTransferBuffer(Request, urb, report, reportLength, TRUE);
    }
    if (!NT_SUCCESS(status)) {
        return status;
    }

    urb->UrbBulkOrInterruptTransfer.TransferBufferLength = reportLength;
    UdecxUrbSetBytesCompleted(Request, reportLength);
    if (transition) {
        InterlockedExchange(
            &endpointContext->InputTransitionHead,
            (LONG)((transitionHead + 1) % endpointContext->InputTransitionCapacity));
        InterlockedDecrement(&endpointContext->InputTransitionCount);
    } else {
        InterlockedExchange(&endpointContext->InputSnapshotPending, FALSE);
    }
    *BytesPrepared = reportLength;
    *SequencePrepared = reportSequence;
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
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext =
        ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    WDFREQUEST request = WDF_NO_HANDLE;
    NTSTATUS completionStatus = STATUS_SUCCESS;
    ULONG directInputBytes = 0;
    ULONGLONG directInputSequence = 0;
    BOOLEAN admitted = FALSE;
    BOOLEAN deliveryReady = FALSE;
    BOOLEAN completionAdmitted = FALSE;

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);
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

    // ReadyNotify is edge-triggered on empty -> non-empty. Microsoft requires
    // manual-queue callbacks to retrieve in a loop, otherwise multiple host
    // polls which arrived together can remain stranded while retained input
    // also remains pending. The initial operation is a callback-lifetime hold;
    // each retrieved URB receives its own rundown count transferred to the DPC.
    for (;;) {
        completionAdmitted = FALSE;
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0 &&
            InterlockedCompareExchange(&controllerContext->BrokerFaulted, FALSE, FALSE) == FALSE &&
            InterlockedCompareExchange(&deviceContext->InD0, 0, 0) != 0 &&
            InterlockedCompareExchange(&deviceContext->Purging, 0, 0) == 0 &&
            InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) == 0 &&
            InterlockedCompareExchange(&endpointContext->Purging, 0, 0) == 0 &&
            InterlockedCompareExchange(&endpointContext->Resetting, 0, 0) == 0 &&
            InterlockedCompareExchange(&endpointContext->InputReportValid, 0, 0) != 0 &&
            (InterlockedCompareExchange(&endpointContext->InputTransitionCount, 0, 0) > 0 ||
                InterlockedCompareExchange(&endpointContext->InputSnapshotPending, 0, 0) != 0)) {
            ViiperEndpointOperationStarted(endpoint);
            completionAdmitted = TRUE;
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);
        if (!completionAdmitted) {
            break;
        }

        request = WDF_NO_HANDLE;
        if (!NT_SUCCESS(WdfIoQueueRetrieveNextRequest(Queue, &request))) {
            ViiperEndpointOperationCompleted(endpoint);
            break;
        }
        ViiperInvalidateInputIfLifecycleClosed(endpoint);
        completionStatus = ViiperPrepareCachedInputUrb(
            endpoint, request, &directInputBytes, &directInputSequence);
        InterlockedExchange(
            &endpointContext->CachedDeliveryPending,
            InterlockedCompareExchange(&endpointContext->InputTransitionCount, 0, 0) > 0 ||
                InterlockedCompareExchange(&endpointContext->InputSnapshotPending, 0, 0) != 0);
        ViiperCompleteRetrievedInputUrb(
            endpoint, request, completionStatus,
            directInputBytes, directInputSequence);
    }
    WdfWaitLockRelease(endpointContext->InputLock);
    // Release the callback-lifetime hold only after the final endpoint access.
    // Every queued completion owns a separate count until its DPC completes.
    ViiperEndpointOperationCompleted(endpoint);
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
    ULONG directInputBytes = 0;
    ULONGLONG directInputSequence = 0;
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
        input->DeviceId == 0 || input->Generation == 0 ||
        input->EndpointGeneration == 0 || input->Sequence == 0 ||
        input->Sequence > MAXLONGLONG ||
        (input->EndpointAddress & USB_ENDPOINT_DIRECTION_MASK) == 0 ||
        input->PayloadOffset != sizeof(*input) || input->PayloadLength == 0 ||
        input->PayloadLength > VIIPER_UDE_MAX_INPUT_REPORT_BYTES ||
        payloadLength != input->PayloadLength ||
        (input->Flags & ~VIIPER_UDE_INPUT_REPORT_TRANSITION) != 0 ||
        input->Reserved1[0] != 0 || input->Reserved1[1] != 0) {
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
                    if (deviceContext->RetiredEndpoints[input->EndpointAddress] ||
                        (deviceContext->EndpointGenerations[input->EndpointAddress] != 0 &&
                         input->EndpointGeneration <=
                            deviceContext->EndpointGenerations[input->EndpointAddress])) {
                        lifecycleDrop = TRUE;
                        status = STATUS_SUCCESS;
                    }
                } else {
                    endpointContext = ViiperGetEndpointContext(endpoint);
                    if (endpointContext->Generation != input->EndpointGeneration ||
                        deviceContext->EndpointGenerations[input->EndpointAddress] !=
                            input->EndpointGeneration) {
                        if (input->EndpointGeneration < endpointContext->Generation) {
                            lifecycleDrop = TRUE;
                            status = STATUS_SUCCESS;
                        }
                    } else if (!endpointContext->FastInput ||
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
        endpointContext->Generation != input->EndpointGeneration ||
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
    if (input->PayloadLength > endpointContext->InputTransitionStride) {
        WdfWaitLockRelease(endpointContext->InputLock);
        ViiperEndpointOperationCompleted(endpoint);
        InterlockedIncrement64(&controllerContext->InvalidMessages);
        return STATUS_INVALID_BUFFER_SIZE;
    }
    if ((input->Flags & VIIPER_UDE_INPUT_REPORT_TRANSITION) != 0 &&
        InterlockedCompareExchange(&endpointContext->InputTransitionCount, 0, 0) >=
            (LONG)endpointContext->InputTransitionCapacity) {
        WdfWaitLockRelease(endpointContext->InputLock);
        ViiperEndpointOperationCompleted(endpoint);
        InterlockedIncrement64(&controllerContext->QueueExhaustions);
        return STATUS_DEVICE_BUSY;
    }
    // Every accepted sample refreshes the cadence snapshot. Only a newly
    // queued controller state is also appended to the bounded transition FIFO;
    // deadline-generated idle samples therefore cannot crowd out press/release
    // edges while Windows has no interrupt poll parked.
    RtlCopyMemory(endpointContext->InputReport, payload, input->PayloadLength);
    endpointContext->InputReportLength = input->PayloadLength;
    if ((input->Flags & VIIPER_UDE_INPUT_REPORT_TRANSITION) != 0) {
        ULONG count = (ULONG)InterlockedCompareExchange(
            &endpointContext->InputTransitionCount, 0, 0);
        ULONG head = (ULONG)InterlockedCompareExchange(
            &endpointContext->InputTransitionHead, 0, 0);
        ULONG tail = (head + count) % endpointContext->InputTransitionCapacity;
        RtlCopyMemory(
            endpointContext->InputTransitionReports +
                ((SIZE_T)tail * endpointContext->InputTransitionStride),
            payload,
            input->PayloadLength);
        endpointContext->InputTransitionLengths[tail] = (USHORT)input->PayloadLength;
        endpointContext->InputTransitionSequences[tail] = input->Sequence;
    }
    // The interlocked sequence publication is the release boundary for both
    // the latest snapshot and optional transition payload. Consumers take the
    // same endpoint lock, while the explicit payload-before-sequence order
    // keeps the cache contract correct if that synchronization is later
    // narrowed for latency.
    InterlockedExchange64(&endpointContext->LastInputSequence, (LONG64)input->Sequence);
    InterlockedExchange(&endpointContext->InputReportValid, TRUE);
    if ((input->Flags & VIIPER_UDE_INPUT_REPORT_TRANSITION) != 0) {
        InterlockedIncrement(&endpointContext->InputTransitionCount);
        InterlockedExchange(&endpointContext->InputSnapshotPending, FALSE);
    } else {
        InterlockedExchange(&endpointContext->InputSnapshotPending, TRUE);
    }
    InterlockedIncrement64(&controllerContext->InputReportsSubmitted);
    status = WdfIoQueueRetrieveNextRequest(endpointContext->Queue, &urbRequest);
    if (!NT_SUCCESS(status)) {
        InterlockedExchange(
            &endpointContext->CachedDeliveryPending,
            InterlockedCompareExchange(
                &endpointContext->InputTransitionCount, 0, 0) > 0 ||
                InterlockedCompareExchange(
                    &endpointContext->InputSnapshotPending, 0, 0) != 0);
        ViiperInvalidateInputIfLifecycleClosed(endpoint);
        WdfWaitLockRelease(endpointContext->InputLock);
        ViiperEndpointOperationCompleted(endpoint);
        // The cached report now owns this state. Queue-ready delivery services
        // the next Windows poll even if the physical feeder becomes idle.
        return STATUS_SUCCESS;
    }
    InterlockedExchange(&endpointContext->CachedDeliveryPending, FALSE);
    // Lifecycle admission can close after this operation was admitted. The
    // pre-boundary poll may finish, but its cached state must never survive the
    // reset/purge/D0 boundary. Revalidate under the same admission lock so
    // either this path or the lifecycle callback performs the final clear.
    ViiperInvalidateInputIfLifecycleClosed(endpoint);
    status = ViiperPrepareCachedInputUrb(
        endpoint, urbRequest, &directInputBytes, &directInputSequence);
    InterlockedExchange(
        &endpointContext->CachedDeliveryPending,
        InterlockedCompareExchange(&endpointContext->InputTransitionCount, 0, 0) > 0 ||
            InterlockedCompareExchange(&endpointContext->InputSnapshotPending, 0, 0) != 0);
    WdfWaitLockRelease(endpointContext->InputLock);
    // This call is the active-operation handoff. It performs every remaining
    // endpoint lookup before enqueuing the DPC; the caller performs no endpoint
    // access after a concurrently running DPC can release rundown.
    ViiperCompleteRetrievedInputUrb(
        endpoint, urbRequest, status, directInputBytes, directInputSequence);
    // The producer publication was accepted before servicing this host poll.
    // A malformed/short URB fails through its own DPC but does not make user
    // mode retry an already-accepted sequence or discard the retained edge.
    return STATUS_SUCCESS;
}

_IRQL_requires_(PASSIVE_LEVEL)
static
VOID
ViiperWaitForEndpointQuiescence(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    LARGE_INTEGER retryInterval;
    LARGE_INTEGER watchdogWait;
    ULONGLONG nextWatchdog;

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);
    // One millisecond is used only on the cold purge/reset path. The ordinary
    // case returns after one event wait and one read-only queue-state sample.
    retryInterval.QuadPart = -10 * 1000;
    watchdogWait.QuadPart =
        -(LONGLONG)VIIPER_UDE_RUNDOWN_WATCHDOG_INTERVAL_100NS;
    nextWatchdog = KeQueryInterruptTime() +
        VIIPER_UDE_RUNDOWN_WATCHDOG_INTERVAL_100NS;
    for (;;) {
        WDF_IO_QUEUE_STATE queueState;
        BOOLEAN quiescent;
        LONG activeOperations;
        ULONGLONG now;

        (VOID)KeWaitForSingleObject(
            &endpointContext->OperationsDrained,
            Executive,
            KernelMode,
            FALSE,
            &watchdogWait);

        // WdfIoQueueDriverNoRequests closes the interval in which a callback
        // was delivered and then preempted before its first BrokerLock
        // acquisition. The BrokerLock-owned rundown joins that callback's
        // terminal DPC. Queued host polls are intentionally allowed here:
        // reset keeps the queue active, and terminal shutdown lets UdeCx issue
        // the corresponding endpoint-purge callback after child consumption.
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        queueState = WdfIoQueueGetState(endpointContext->Queue, NULL, NULL);
        quiescent = (queueState & WdfIoQueueDriverNoRequests) != 0 &&
            InterlockedCompareExchange(
                &endpointContext->ActiveOperations, 0, 0) == 0;
        activeOperations = InterlockedCompareExchange(
            &endpointContext->ActiveOperations, 0, 0);
        WdfSpinLockRelease(controllerContext->BrokerLock);
        if (quiescent) {
            return;
        }
        now = KeQueryInterruptTime();
        if (now >= nextWatchdog) {
            VIIPER_TRACE_LIFECYCLE(
                deviceContext->Controller,
                VIIPER_UDE_TRACE_SOURCE_DEVICE,
                VIIPER_UDE_TRACE_ENDPOINT_QUIESCENCE_WATCHDOG,
                deviceContext->DeviceId,
                deviceContext->Generation,
                endpointContext->Device,
                Endpoint,
                endpointContext->Descriptor.bEndpointAddress,
                STATUS_IO_TIMEOUT,
                activeOperations,
                (ULONG)queueState);
            nextWatchdog = now + VIIPER_UDE_RUNDOWN_WATCHDOG_INTERVAL_100NS;
        }

        // A callback can be between KMDF delivery and its first BrokerLock
        // acquisition. It will either enter rundown and re-arm the event or
        // finish its terminal DPC and return the request to framework
        // ownership. Avoid spinning while that passive callback is scheduled.
        (VOID)KeDelayExecutionThread(KernelMode, FALSE, &retryInterval);
    }
}

_IRQL_requires_(PASSIVE_LEVEL)
static
VOID
ViiperWaitForEndpointPurgeQuiescence(
    _In_ UDECXUSBENDPOINT Endpoint,
    _Out_ WDF_IO_QUEUE_STATE *FinalQueueState,
    _Out_ ULONG *FinalQueuedRequests,
    _Out_ ULONG *FinalDriverRequests
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    LARGE_INTEGER retryInterval;
    LARGE_INTEGER watchdogWait;
    ULONGLONG nextWatchdog;

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);
    retryInterval.QuadPart = -10 * 1000;
    watchdogWait.QuadPart =
        -(LONGLONG)VIIPER_UDE_RUNDOWN_WATCHDOG_INTERVAL_100NS;
    nextWatchdog = KeQueryInterruptTime() +
        VIIPER_UDE_RUNDOWN_WATCHDOG_INTERVAL_100NS;
    for (;;) {
        WDF_IO_QUEUE_STATE queueState;
        ULONG queuedRequests;
        ULONG driverRequests;
        BOOLEAN quiescent;
        LONG activeOperations;
        ULONGLONG now;

        (VOID)KeWaitForSingleObject(
            &endpointContext->OperationsDrained,
            Executive,
            KernelMode,
            FALSE,
            &watchdogWait);

        // UdeCx exclusively owns the associated queue's START/PURGE state.
        // The PURGE callback is the upstream stop/cancel boundary even when
        // the associated queue retains its READY bookkeeping until the client
        // acknowledges the transition. DriverNoRequests joins callbacks
        // already delivered across that boundary; VIIPER's rundown joins
        // their forwarded and terminal-DPC ownership. Sample both under the
        // BrokerLock without waiting for UdeCx-owned queued host polls.
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        queueState = WdfIoQueueGetState(
            endpointContext->Queue, &queuedRequests, &driverRequests);
        quiescent = InterlockedCompareExchange(
                &endpointContext->PurgeOutstanding, 0, 0) > 0 &&
            InterlockedCompareExchange(
                &endpointContext->Purging, 0, 0) != 0 &&
            (queueState & WdfIoQueueDriverNoRequests) != 0 &&
            driverRequests == 0 &&
            InterlockedCompareExchange(
                &endpointContext->ActiveOperations, 0, 0) == 0;
        activeOperations = InterlockedCompareExchange(
            &endpointContext->ActiveOperations, 0, 0);
        if (quiescent) {
            *FinalQueueState = queueState;
            *FinalQueuedRequests = queuedRequests;
            *FinalDriverRequests = driverRequests;
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);
        if (quiescent) {
            return;
        }
        now = KeQueryInterruptTime();
        if (now >= nextWatchdog) {
            VIIPER_TRACE_LIFECYCLE(
                deviceContext->Controller,
                VIIPER_UDE_TRACE_SOURCE_DEVICE,
                VIIPER_UDE_TRACE_ENDPOINT_QUIESCENCE_WATCHDOG,
                deviceContext->DeviceId,
                deviceContext->Generation,
                endpointContext->Device,
                Endpoint,
                endpointContext->Descriptor.bEndpointAddress,
                STATUS_IO_TIMEOUT,
                activeOperations,
                (ULONG)queueState);
            nextWatchdog = now + VIIPER_UDE_RUNDOWN_WATCHDOG_INTERVAL_100NS;
        }

        // This runs only during an endpoint lifecycle transition. A short
        // passive wait lets any callback already dispatched by KMDF reach its
        // terminal DPC without consuming CPU or touching the input hot path.
        (VOID)KeDelayExecutionThread(KernelMode, FALSE, &retryInterval);
    }
}

VOID
ViiperDrainControllerEndpointOperations(
    _In_ WDFDEVICE Controller
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(Controller);
    ULONG deviceIndex;

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);
    // Hold the shared device index while observing every endpoint so cleanup
    // cannot invalidate a handle between lookup and the final driver-owned
    // operation proof. ShuttingDown is already set, so no broker or direct
    // input admission can reopen. UdeCx remains free to deliver its endpoint
    // purge callbacks after PlugOutAndDelete consumes the child handles.
    ViiperAcquireDeviceLockShared(controllerContext);
    for (deviceIndex = 0; deviceIndex < VIIPER_UDE_MAX_DEVICES; ++deviceIndex) {
        UDECXUSBDEVICE device = controllerContext->Devices[deviceIndex];
        VIIPER_UDE_DEVICE_CONTEXT *deviceContext;
        ULONG endpointIndex;

        if (device == WDF_NO_HANDLE) {
            continue;
        }
        deviceContext = ViiperGetDeviceContext(device);
        for (endpointIndex = 0;
             endpointIndex < RTL_NUMBER_OF(deviceContext->Endpoints);
             ++endpointIndex) {
            UDECXUSBENDPOINT endpoint = deviceContext->Endpoints[endpointIndex];

            if (endpoint != WDF_NO_HANDLE) {
                ViiperWaitForEndpointQuiescence(endpoint);
            }
        }
    }
    ViiperReleaseDeviceLockShared(controllerContext);
}

BOOLEAN
ViiperQuiesceResetByIdentity(
    _In_ WDFDEVICE Controller,
    _In_ ULONGLONG DeviceId,
    _In_ ULONG Generation,
    _In_ UDECXUSBDEVICE ExpectedDevice,
    _In_opt_ UDECXUSBENDPOINT ExpectedEndpoint,
    _In_ ULONG ExpectedEndpointGeneration,
    _In_ ULONGLONG ExpectedResetEpoch,
    _In_ UCHAR EndpointAddress,
    _In_ BOOLEAN WholeDevice,
    _In_ BOOLEAN ReleaseGate
    )
{
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(Controller);
    BOOLEAN found = FALSE;
    ULONG deviceIndex;

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);
    // The asynchronous UdeCx reset request keeps the child alive. Retain the
    // shared index as an additional cleanup fence while joining any terminal
    // callback admitted after the reset event was first published.
    ViiperAcquireDeviceLockShared(controllerContext);
    for (deviceIndex = 0; deviceIndex < VIIPER_UDE_MAX_DEVICES; ++deviceIndex) {
        UDECXUSBDEVICE device = controllerContext->Devices[deviceIndex];
        VIIPER_UDE_DEVICE_CONTEXT *deviceContext;

        if (device == WDF_NO_HANDLE || device != ExpectedDevice) {
            continue;
        }
        deviceContext = ViiperGetDeviceContext(device);
        if (deviceContext->DeviceId != DeviceId ||
            deviceContext->Generation != Generation) {
            continue;
        }

        if (WholeDevice) {
            ULONG endpointIndex;
            ULONGLONG currentResetEpoch;

            WdfSpinLockAcquire(controllerContext->BrokerLock);
            NT_ASSERT(ExpectedEndpointGeneration == 0);
            currentResetEpoch = (ULONGLONG)InterlockedCompareExchange64(
                &deviceContext->ResetEpoch, 0, 0);
            found = InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0 &&
                InterlockedCompareExchange(&controllerContext->BrokerFaulted, FALSE, FALSE) == FALSE &&
                currentResetEpoch == ExpectedResetEpoch &&
                InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) != 0 &&
                InterlockedCompareExchange(&deviceContext->Purging, 0, 0) == 0;
            if (!found && ReleaseGate && currentResetEpoch == ExpectedResetEpoch) {
                InterlockedExchange(&deviceContext->Resetting, FALSE);
            }
            WdfSpinLockRelease(controllerContext->BrokerLock);
            if (!found) {
                break;
            }

            for (endpointIndex = 0;
                 endpointIndex < RTL_NUMBER_OF(deviceContext->Endpoints);
                 ++endpointIndex) {
                UDECXUSBENDPOINT endpoint = deviceContext->Endpoints[endpointIndex];

                if (endpoint != WDF_NO_HANDLE) {
                    ViiperWaitForEndpointQuiescence(endpoint);
                }
            }
            // Revalidate the lifecycle gate after every queue/rundown proof.
            // BrokerLock is the admission linearization point; clearing here
            // cannot target a reused identity because DeviceLock still pins
            // this exact table entry and generation.
            WdfSpinLockAcquire(controllerContext->BrokerLock);
            currentResetEpoch = (ULONGLONG)InterlockedCompareExchange64(
                &deviceContext->ResetEpoch, 0, 0);
            found = InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0 &&
                InterlockedCompareExchange(&controllerContext->BrokerFaulted, FALSE, FALSE) == FALSE &&
                currentResetEpoch == ExpectedResetEpoch &&
                InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) != 0 &&
                InterlockedCompareExchange(&deviceContext->Purging, 0, 0) == 0;
            if (ReleaseGate && currentResetEpoch == ExpectedResetEpoch) {
                InterlockedExchange(&deviceContext->Resetting, FALSE);
            }
            WdfSpinLockRelease(controllerContext->BrokerLock);
        } else {
            UDECXUSBENDPOINT endpoint = deviceContext->Endpoints[EndpointAddress];

            if (endpoint != WDF_NO_HANDLE && endpoint == ExpectedEndpoint &&
                ExpectedEndpointGeneration != 0 &&
                deviceContext->EndpointGenerations[EndpointAddress] ==
                    ExpectedEndpointGeneration) {
                VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext =
                    ViiperGetEndpointContext(endpoint);

                WdfSpinLockAcquire(controllerContext->BrokerLock);
                found = InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0 &&
                    InterlockedCompareExchange(&controllerContext->BrokerFaulted, FALSE, FALSE) == FALSE &&
                    (ULONGLONG)InterlockedCompareExchange64(
                        &deviceContext->ResetEpoch, 0, 0) == ExpectedResetEpoch &&
                    endpointContext->Generation == ExpectedEndpointGeneration &&
                    InterlockedCompareExchange(&deviceContext->Purging, 0, 0) == 0 &&
                    InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) == 0 &&
                    InterlockedCompareExchange(&endpointContext->Resetting, 0, 0) != 0 &&
                    InterlockedCompareExchange(&endpointContext->Purging, 0, 0) == 0;
                if (!found && ReleaseGate) {
                    InterlockedExchange(&endpointContext->Resetting, FALSE);
                }
                WdfSpinLockRelease(controllerContext->BrokerLock);
                if (!found) {
                    break;
                }
                ViiperWaitForEndpointQuiescence(endpoint);
                WdfSpinLockAcquire(controllerContext->BrokerLock);
                found = InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0 &&
                    InterlockedCompareExchange(&controllerContext->BrokerFaulted, FALSE, FALSE) == FALSE &&
                    (ULONGLONG)InterlockedCompareExchange64(
                        &deviceContext->ResetEpoch, 0, 0) == ExpectedResetEpoch &&
                    endpointContext->Generation == ExpectedEndpointGeneration &&
                    deviceContext->EndpointGenerations[EndpointAddress] ==
                        ExpectedEndpointGeneration &&
                    InterlockedCompareExchange(&deviceContext->Purging, 0, 0) == 0 &&
                    InterlockedCompareExchange(&deviceContext->Resetting, 0, 0) == 0 &&
                    InterlockedCompareExchange(&endpointContext->Resetting, 0, 0) != 0 &&
                    InterlockedCompareExchange(&endpointContext->Purging, 0, 0) == 0;
                if (ReleaseGate) {
                    InterlockedExchange(&endpointContext->Resetting, FALSE);
                }
                WdfSpinLockRelease(controllerContext->BrokerLock);
            }
        }
        break;
    }
    ViiperReleaseDeviceLockShared(controllerContext);
    return found;
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
        InterlockedExchange64(
            &endpointContext->ResetDeviceEpoch,
            InterlockedCompareExchange64(&deviceContext->ResetEpoch, 0, 0));
        status = STATUS_SUCCESS;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!NT_SUCCESS(status)) {
        WdfRequestComplete(Request, status);
        return;
    }

    InterlockedExchange64(&endpointContext->NextIsoStartFrame, 0);
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
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    WDFREQUEST request;
    NTSTATUS status;
    BOOLEAN resetCurrent;

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);
    resetCurrent = ViiperQuiesceResetByIdentity(
        deviceContext->Controller,
        deviceContext->DeviceId,
        deviceContext->Generation,
        endpointContext->Device,
        endpoint,
        endpointContext->Generation,
        (ULONGLONG)InterlockedCompareExchange64(
            &endpointContext->ResetDeviceEpoch, 0, 0),
        endpointContext->Descriptor.bEndpointAddress,
        FALSE,
        FALSE);
    // The unresolved asynchronous reset Request keeps this endpoint unable to
    // process transfers. No successor callback can be delivered between the
    // read-only queue/rundown proof and publication; a callback delivered just
    // before reset closure is included by DriverNoRequests and the terminal
    // DPC-owned rundown. Owner acknowledgement repeats the proof. An input
    // publisher admitted immediately before Resetting was raised may finish,
    // then this barrier performs the final invalidation.
    request = endpointContext->ResetRequest;
    endpointContext->ResetRequest = WDF_NO_HANDLE;
    if (!resetCurrent) {
        // A device reset may have won after this endpoint reset closed its own
        // gate. Release only the endpoint-reset gate; the device reset, purge,
        // shutdown, or broker-fault predicate independently keeps admission
        // closed until its owner finishes recovery.
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        InterlockedExchange(&endpointContext->Resetting, FALSE);
        WdfSpinLockRelease(controllerContext->BrokerLock);
        WdfRequestComplete(request, STATUS_DEVICE_NOT_READY);
        return;
    }
    ViiperInvalidateEndpointInputReport(endpoint);
    status = ViiperQueueAcknowledgedEndpointLifecycleEvent(
        endpoint, request, ViiperUdeOperationEndpointReset);
    if (!NT_SUCCESS(status)) {
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        InterlockedExchange(&endpointContext->Resetting, FALSE);
        WdfSpinLockRelease(controllerContext->BrokerLock);
        WdfRequestComplete(request, status);
    }
}

VOID
ViiperEvtEndpointPurgeWorkItem(
    _In_ WDFWORKITEM WorkItem
    )
{
    UDECXUSBENDPOINT endpoint =
        (UDECXUSBENDPOINT)WdfWorkItemGetParentObject(WorkItem);
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext =
        ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    WDFDEVICE controller = deviceContext->Controller;
    UDECXUSBDEVICE device = endpointContext->Device;
    ULONGLONG deviceId = deviceContext->DeviceId;
    ULONG generation = deviceContext->Generation;
    UCHAR endpointAddress = endpointContext->Descriptor.bEndpointAddress;

    NT_ASSERT(KeGetCurrentIrql() == PASSIVE_LEVEL);
    for (;;) {
        WDF_IO_QUEUE_STATE queueState;
        ULONG queuedRequests;
        ULONG driverRequests;
        LONG remaining;

        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (InterlockedCompareExchange(
                &endpointContext->PurgeOutstanding, 0, 0) <= 0) {
            InterlockedExchange(&endpointContext->PurgeWorkerActive, FALSE);
            WdfSpinLockRelease(controllerContext->BrokerLock);
            return;
        }
        NT_ASSERT(InterlockedCompareExchange(
            &endpointContext->PurgeWorkerActive, 0, 0) != 0);
        WdfSpinLockRelease(controllerContext->BrokerLock);

        VIIPER_TRACE_LIFECYCLE(
            deviceContext->Controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
            VIIPER_UDE_TRACE_ENDPOINT_DRAIN_BEGIN, deviceContext->DeviceId,
            deviceContext->Generation, endpointContext->Device, endpoint,
            endpointContext->Descriptor.bEndpointAddress, STATUS_SUCCESS,
            endpointContext->ActiveOperations,
            (ULONG)WdfIoQueueGetState(endpointContext->Queue, NULL, NULL));
        ViiperWaitForEndpointPurgeQuiescence(
            endpoint, &queueState, &queuedRequests, &driverRequests);
        VIIPER_TRACE_LIFECYCLE(
            deviceContext->Controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
            VIIPER_UDE_TRACE_ENDPOINT_DRIVER_QUIESCENT, deviceContext->DeviceId,
            deviceContext->Generation, endpointContext->Device, endpoint,
            endpointContext->Descriptor.bEndpointAddress, STATUS_SUCCESS,
            endpointContext->ActiveOperations, (ULONG)queueState);
        VIIPER_TRACE_LIFECYCLE(
            deviceContext->Controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
            VIIPER_UDE_TRACE_ENDPOINT_DRAIN_END, deviceContext->DeviceId,
            deviceContext->Generation, endpointContext->Device, endpoint,
            endpointContext->Descriptor.bEndpointAddress, STATUS_SUCCESS,
            endpointContext->ActiveOperations, (ULONG)queueState);
        NT_ASSERT(InterlockedCompareExchange(
            &endpointContext->ActiveOperations, 0, 0) == 0);
        ViiperInvalidateEndpointInputReport(endpoint);
        VIIPER_TRACE_LIFECYCLE(
            deviceContext->Controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
            VIIPER_UDE_TRACE_ENDPOINT_PURGE_COMPLETE_BEGIN,
            deviceContext->DeviceId, deviceContext->Generation,
            endpointContext->Device, endpoint,
            endpointContext->Descriptor.bEndpointAddress, STATUS_SUCCESS,
            endpointContext->ActiveOperations,
            (ULONG)WdfIoQueueGetState(endpointContext->Queue, NULL, NULL));

        // Retire exactly one callback before completing it. A synchronous
        // final START observes zero and may reopen admission; an earlier START
        // sees another outstanding PURGE and remains closed. Keep worker
        // ownership through the completion so a reentrant PURGE is drained by
        // this same invocation instead of being lost to work-item coalescing.
        WdfSpinLockAcquire(controllerContext->BrokerLock);
        NT_ASSERT(InterlockedCompareExchange(
            &endpointContext->PurgeWorkerActive, 0, 0) != 0);
        remaining = InterlockedDecrement(&endpointContext->PurgeOutstanding);
        NT_ASSERT(remaining >= 0);
        WdfSpinLockRelease(controllerContext->BrokerLock);

        UdecxUsbEndpointPurgeComplete(endpoint);
        VIIPER_TRACE_LIFECYCLE(
            controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
            VIIPER_UDE_TRACE_ENDPOINT_PURGE_COMPLETE_END, deviceId, generation,
            device, endpoint, endpointAddress, STATUS_SUCCESS, remaining, 0);

        WdfSpinLockAcquire(controllerContext->BrokerLock);
        if (InterlockedCompareExchange(
                &endpointContext->PurgeOutstanding, 0, 0) == 0) {
            InterlockedExchange(&endpointContext->PurgeWorkerActive, FALSE);
            WdfSpinLockRelease(controllerContext->BrokerLock);
            return;
        }
        WdfSpinLockRelease(controllerContext->BrokerLock);
    }
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
    BOOLEAN enqueueWorkItem;
    LONG outstanding;

    VIIPER_TRACE_LIFECYCLE(
        deviceContext->Controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
        VIIPER_UDE_TRACE_ENDPOINT_PURGE_BEGIN, deviceContext->DeviceId,
        deviceContext->Generation, endpointContext->Device, Endpoint,
        endpointContext->Descriptor.bEndpointAddress, STATUS_SUCCESS,
        endpointContext->ActiveOperations,
        (ULONG)WdfIoQueueGetState(endpointContext->Queue, NULL, NULL));

    // Serialize the admission gate with pending-slot allocation and direct
    // input before the queue begins cancellation.
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    InterlockedExchange(&endpointContext->Purging, TRUE);
    InterlockedExchange(&endpointContext->StartAnnounced, FALSE);
    outstanding = InterlockedIncrement(&endpointContext->PurgeOutstanding);
    enqueueWorkItem = InterlockedCompareExchange(
        &endpointContext->PurgeWorkerActive, TRUE, FALSE) == FALSE;
    WdfSpinLockRelease(controllerContext->BrokerLock);
    NT_ASSERT(outstanding > 0);
    (VOID)outstanding;
    InterlockedExchange64(&endpointContext->NextIsoStartFrame, 0);
    ViiperPurgeEndpointOperations(Endpoint, STATUS_DEVICE_NOT_READY);
    VIIPER_TRACE_LIFECYCLE(
        deviceContext->Controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
        VIIPER_UDE_TRACE_ENDPOINT_OPERATIONS_PURGED, deviceContext->DeviceId,
        deviceContext->Generation, endpointContext->Device, Endpoint,
        endpointContext->Descriptor.bEndpointAddress, STATUS_SUCCESS,
        endpointContext->ActiveOperations,
        (ULONG)WdfIoQueueGetState(endpointContext->Queue, NULL, NULL));
    (VOID)ViiperQueueEndpointLifecycleEvent(Endpoint, ViiperUdeOperationEndpointPurge);
    // UdeCx owns the associated queue's state. Only the operations forwarded
    // into VIIPER-owned paths are ours to cancel and join. The passive work
    // item observes the class-extension state without changing it, performs a
    // final cache clear, and acknowledges each outstanding callback only after
    // all framework-delivered and VIIPER-owned work drains.
    VIIPER_TRACE_LIFECYCLE(
        deviceContext->Controller, VIIPER_UDE_TRACE_SOURCE_DEVICE,
        VIIPER_UDE_TRACE_ENDPOINT_QUEUE_PURGE_REQUESTED, deviceContext->DeviceId,
        deviceContext->Generation, endpointContext->Device, Endpoint,
        endpointContext->Descriptor.bEndpointAddress, STATUS_SUCCESS,
        endpointContext->ActiveOperations,
        (ULONG)WdfIoQueueGetState(endpointContext->Queue, NULL, NULL));
    // This must remain the final endpoint access. Once the worker completes a
    // PURGE, UdeCx can synchronously advance lifecycle or delete the endpoint.
    if (enqueueWorkItem) {
        WdfWorkItemEnqueue(endpointContext->PurgeWorkItem);
    }
}

static
VOID
ViiperActivateEndpoint(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    VIIPER_UDE_ENDPOINT_CONTEXT *endpointContext = ViiperGetEndpointContext(Endpoint);
    VIIPER_UDE_DEVICE_CONTEXT *deviceContext = ViiperGetDeviceContext(endpointContext->Device);
    VIIPER_UDE_CONTROLLER_CONTEXT *controllerContext =
        ViiperGetControllerContext(deviceContext->Controller);
    BOOLEAN active = FALSE;
    BOOLEAN announce = FALSE;
    NTSTATUS status = STATUS_SUCCESS;

    // Device configuration is the hardware-selection boundary for dynamic
    // endpoints. UdeCx does not issue a separate START callback for every
    // newly selected endpoint on all supported Windows builds, so publish that
    // selection before completing the configuration request. A later explicit
    // START still opens VIIPER's forwarded-operation admission gate, while its
    // user-mode activation is deduplicated by StartAnnounced. UdeCx alone owns
    // the associated KMDF queue transition. PURGE closes both VIIPER gates.
    InterlockedExchange64(&endpointContext->NextIsoStartFrame, 0);
    WdfSpinLockAcquire(controllerContext->BrokerLock);
    if (InterlockedCompareExchange(&controllerContext->ShuttingDown, 0, 0) == 0 &&
        InterlockedCompareExchange(&deviceContext->Purging, 0, 0) == 0 &&
        InterlockedCompareExchange(&deviceContext->InD0, 0, 0) != 0 &&
        InterlockedCompareExchange(&deviceContext->D0ExitPending, 0, 0) == 0 &&
        InterlockedCompareExchange(
            &endpointContext->PurgeOutstanding, 0, 0) == 0) {
        InterlockedExchange(&endpointContext->Purging, FALSE);
        active = TRUE;
        announce = InterlockedCompareExchange(
            &endpointContext->StartAnnounced, TRUE, FALSE) == FALSE;
    }
    WdfSpinLockRelease(controllerContext->BrokerLock);
    if (!active) {
        return;
    }
    if (announce) {
        status = ViiperQueueEndpointLifecycleEvent(
            Endpoint, ViiperUdeOperationEndpointStart);
        if (!NT_SUCCESS(status)) {
            // Permit a later explicit START to retry publication. Compare-
            // exchange preserves a PURGE which may already have cleared the
            // announcement while the notification path was being dispatched.
            (VOID)InterlockedCompareExchange(
                &endpointContext->StartAnnounced, FALSE, TRUE);
        }
    }
}

VOID
ViiperEvtEndpointStart(
    _In_ UDECXUSBENDPOINT Endpoint
    )
{
    ViiperActivateEndpoint(Endpoint);
}

VOID
ViiperEvtEndpointsConfigure(
    _In_ UDECXUSBDEVICE Device,
    _In_ WDFREQUEST Request,
    _In_ UDECX_ENDPOINTS_CONFIGURE_PARAMS *ConfigureParams
    )
{
    NTSTATUS status = STATUS_SUCCESS;
    ULONG endpointIndex;

    switch (ConfigureParams->ConfigureType) {
    case UdecxEndpointsConfigureTypeDeviceInitialize:
        // DeviceInitialize is UdeCx's endpoint-publication boundary, not a
        // post-enumeration device reset. It can run more than once while the
        // child is being initialized. Completing it synchronously avoids
        // introducing a user-mode reset dependency before Windows can finish
        // enumerating the child. Announce only the exact endpoint handles UdeCx
        // selected; this also covers Windows builds which do not follow a new
        // dynamic endpoint with a separate START callback.
        for (endpointIndex = 0;
             endpointIndex < ConfigureParams->EndpointsToConfigureCount;
             ++endpointIndex) {
            ViiperActivateEndpoint(
                ConfigureParams->EndpointsToConfigure[endpointIndex]);
        }
        WdfRequestComplete(Request, STATUS_SUCCESS);
        return;
    case UdecxEndpointsConfigureTypeDeviceConfigurationChange:
        // Selecting a configuration is the boundary that makes the newly
        // created dynamic endpoint queues eligible for START. It is not a USB
        // device reset. Holding this request for a user-mode reset round trip
        // leaves every non-default endpoint in UdeCx's preceding PURGE state.
        // Publish the selected endpoints before completing this asynchronous
        // configuration boundary. PURGE remains authoritative for release and
        // a later explicit START reopens only VIIPER-owned forwarding paths.
        for (endpointIndex = 0;
             endpointIndex < ConfigureParams->EndpointsToConfigureCount;
             ++endpointIndex) {
            ViiperActivateEndpoint(
                ConfigureParams->EndpointsToConfigure[endpointIndex]);
        }
        WdfRequestComplete(Request, STATUS_SUCCESS);
        return;
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

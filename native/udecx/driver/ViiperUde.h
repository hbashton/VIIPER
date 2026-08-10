#pragma once

#include <ntddk.h>
#include <wdf.h>
#include <usb.h>
#include <usbioctl.h>
#include <usbiodef.h>
#include <wdfusb.h>
#include <udecx.h>

#include "..\include\ViiperUdeProtocol.h"

EXTERN_C const GUID GUID_DEVINTERFACE_VIIPER_UDE;

typedef struct VIIPER_UDE_CONTROLLER_CONTEXT {
    WDFWAITLOCK OwnerLock;
    WDFWAITLOCK DeviceLock;
    WDFFILEOBJECT OwnerFile;
    WDFQUEUE DefaultQueue;
    WDFQUEUE WaitingDequeues;
    BOOLEAN CleanupInProgress;
    volatile LONG ActiveDevices;
    volatile LONG PendingOperations;
    volatile LONG WaitingDequeueCount;
    volatile LONG64 OperationsDequeued;
    volatile LONG64 OperationsCompleted;
    volatile LONG64 OperationsCancelled;
    volatile LONG64 OperationsPurged;
    volatile LONG64 LateCompletions;
    volatile LONG64 InvalidMessages;
    volatile LONG64 QueueExhaustions;
    volatile LONG64 IsoPackets;
    volatile LONG64 BytesToDevice;
    volatile LONG64 BytesFromDevice;
    UDECXUSBDEVICE Devices[VIIPER_UDE_MAX_DEVICES];
} VIIPER_UDE_CONTROLLER_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_CONTROLLER_CONTEXT, ViiperGetControllerContext)

typedef struct VIIPER_UDE_FILE_CONTEXT {
    BOOLEAN Negotiated;
    BOOLEAN Closing;
    ULONGLONG ClientNonce;
    ULONGLONG DriverNonce;
} VIIPER_UDE_FILE_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_FILE_CONTEXT, ViiperGetFileContext)

typedef struct VIIPER_UDE_DEVICE_CONTEXT {
    WDFDEVICE Controller;
    WDFFILEOBJECT OwnerFile;
    ULONGLONG DeviceId;
    ULONG Generation;
    ULONG Slot;
    BOOLEAN Plugged;
    BOOLEAN Purging;
    UDECXUSBENDPOINT DefaultEndpoint;
} VIIPER_UDE_DEVICE_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_DEVICE_CONTEXT, ViiperGetDeviceContext)

typedef struct VIIPER_UDE_ENDPOINT_CONTEXT {
    UDECXUSBDEVICE Device;
    WDFQUEUE Queue;
    USB_ENDPOINT_DESCRIPTOR Descriptor;
    BOOLEAN Purging;
} VIIPER_UDE_ENDPOINT_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_ENDPOINT_CONTEXT, ViiperGetEndpointContext)
WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(UDECXUSBENDPOINT, ViiperGetQueueEndpoint)

DRIVER_INITIALIZE DriverEntry;
EVT_WDF_DRIVER_DEVICE_ADD ViiperEvtDeviceAdd;
EVT_WDF_OBJECT_CONTEXT_CLEANUP ViiperEvtDriverCleanup;
EVT_WDF_OBJECT_CONTEXT_CLEANUP ViiperEvtControllerCleanup;
EVT_WDF_DEVICE_FILE_CREATE ViiperEvtFileCreate;
EVT_WDF_FILE_CLEANUP ViiperEvtFileCleanup;
EVT_WDF_IO_QUEUE_IO_DEVICE_CONTROL ViiperEvtIoDeviceControl;
EVT_UDECX_WDF_DEVICE_QUERY_USB_CAPABILITY ViiperEvtQueryUsbCapability;
EVT_UDECX_USB_DEVICE_D0_ENTRY ViiperEvtUsbDeviceD0Entry;
EVT_UDECX_USB_DEVICE_D0_EXIT ViiperEvtUsbDeviceD0Exit;
EVT_UDECX_USB_DEVICE_SET_FUNCTION_SUSPEND_AND_WAKE ViiperEvtUsbDeviceSetFunctionSuspendAndWake;
EVT_UDECX_USB_DEVICE_DEFAULT_ENDPOINT_ADD ViiperEvtDefaultEndpointAdd;
EVT_UDECX_USB_DEVICE_ENDPOINT_ADD ViiperEvtEndpointAdd;
EVT_UDECX_USB_DEVICE_ENDPOINTS_CONFIGURE ViiperEvtEndpointsConfigure;
EVT_UDECX_USB_ENDPOINT_RESET ViiperEvtEndpointReset;
EVT_UDECX_USB_ENDPOINT_PURGE ViiperEvtEndpointPurge;
EVT_UDECX_USB_ENDPOINT_START ViiperEvtEndpointStart;
EVT_WDF_IO_QUEUE_IO_INTERNAL_DEVICE_CONTROL ViiperEvtEndpointIoInternalControl;
EVT_WDF_IO_QUEUE_STATE ViiperEvtEndpointQueuePurged;
EVT_WDF_OBJECT_CONTEXT_CLEANUP ViiperEvtVirtualDeviceCleanup;

NTSTATUS ViiperCreateQueues(_In_ WDFDEVICE Device);
NTSTATUS ViiperCreateVirtualDevice(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request);
NTSTATUS ViiperDestroyVirtualDevice(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request);
VOID ViiperDestroyOwnedDevices(_In_ WDFDEVICE Controller, _In_ WDFFILEOBJECT OwnerFile);

#pragma once

#include <ntddk.h>
#include <wdf.h>
#include <udecx.h>
#include <usb.h>

#include "..\include\ViiperUdeProtocol.h"

EXTERN_C const GUID GUID_DEVINTERFACE_VIIPER_UDE;

typedef struct VIIPER_UDE_CONTROLLER_CONTEXT {
    WDFWAITLOCK OwnerLock;
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
} VIIPER_UDE_CONTROLLER_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_CONTROLLER_CONTEXT, ViiperGetControllerContext)

typedef struct VIIPER_UDE_FILE_CONTEXT {
    BOOLEAN Negotiated;
    BOOLEAN Closing;
    uint64_t ClientNonce;
    uint64_t DriverNonce;
} VIIPER_UDE_FILE_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_FILE_CONTEXT, ViiperGetFileContext)

DRIVER_INITIALIZE DriverEntry;
EVT_WDF_DRIVER_DEVICE_ADD ViiperEvtDeviceAdd;
EVT_WDF_OBJECT_CONTEXT_CLEANUP ViiperEvtDriverCleanup;
EVT_WDF_OBJECT_CONTEXT_CLEANUP ViiperEvtControllerCleanup;
EVT_WDF_DEVICE_FILE_CREATE ViiperEvtFileCreate;
EVT_WDF_FILE_CLEANUP ViiperEvtFileCleanup;
EVT_WDF_IO_QUEUE_IO_DEVICE_CONTROL ViiperEvtIoDeviceControl;
EVT_UDECX_WDF_DEVICE_QUERY_USB_CAPABILITY ViiperEvtQueryUsbCapability;

NTSTATUS ViiperCreateQueues(_In_ WDFDEVICE Device);


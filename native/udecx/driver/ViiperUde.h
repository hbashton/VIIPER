#pragma once

#include <ntddk.h>
#include <wdf.h>
#include <usb.h>
#include <usbioctl.h>
#include <usbiodef.h>
#include <usbdlib.h>
#include <wdfusb.h>
#include <udecx.h>

#include "..\include\ViiperUdeProtocol.h"

EXTERN_C const GUID GUID_DEVINTERFACE_VIIPER_UDE;

// Only VIIPER's private interface receives a reference string. The standard
// host-controller interface must retain UdeCx's canonical unqualified path.
#define VIIPER_UDE_BROKER_REFERENCE_STRING L"broker"

typedef enum VIIPER_UDE_PENDING_STATE {
    ViiperUdePendingEmpty = 0,
    ViiperUdePendingPreparing,
    ViiperUdePendingQueued,
    ViiperUdePendingPublishing,
    ViiperUdePendingInFlight,
    ViiperUdePendingCompleting,
    ViiperUdePendingDpcCompletion
} VIIPER_UDE_PENDING_STATE;

typedef struct VIIPER_UDE_PENDING_SLOT {
    WDFREQUEST Request;
    UDECXUSBENDPOINT Endpoint;
    ULONGLONG Token;
    ULONGLONG DeviceId;
    ULONGLONG AdmissionSequence;
    ULONG Generation;
    ULONG DeviceGeneration;
    VIIPER_UDE_PENDING_STATE State;
    BOOLEAN AbortPending;
    BOOLEAN PublishedToOwner;
    UCHAR EndpointAddress;
    NTSTATUS AbortStatus;
    NTSTATUS CompletionStatus;
    USBD_STATUS CompletionUsbdStatus;
    BOOLEAN CompleteWithNtStatus;
} VIIPER_UDE_PENDING_SLOT;

typedef struct VIIPER_UDE_NOTIFICATION {
    ULONGLONG Token;
    ULONGLONG DeviceId;
    ULONGLONG EndpointSequence;
    ULONG Generation;
    ULONG Kind;
    UCHAR EndpointAddress;
    UCHAR InterfaceNumber;
    UCHAR InterfaceSetting;
    UCHAR EndpointAttributes;
    UCHAR EndpointInterval;
    USHORT EndpointMaxPacketSize;
} VIIPER_UDE_NOTIFICATION;

typedef struct VIIPER_UDE_REQUEST_CONTEXT {
    WDFDEVICE Controller;
    UDECXUSBENDPOINT Endpoint;
    ULONG PendingSlot;
    ULONGLONG Token;
    ULONG TransferLength;
    ULONG IsoPacketCount;
    ULONG IsoStartFrame;
    BOOLEAN DirectionIn;
} VIIPER_UDE_REQUEST_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_REQUEST_CONTEXT, ViiperGetRequestContext)

typedef struct VIIPER_UDE_CONTROLLER_CONTEXT {
    WDFWAITLOCK OwnerLock;
    WDFWAITLOCK DeviceLock;
    WDFSPINLOCK BrokerLock;
    WDFMEMORY PendingStorage;
    VIIPER_UDE_PENDING_SLOT *PendingSlots;
    ULONG NextPendingSlot;
    ULONG NextCompletionSlot;
    WDFMEMORY NotificationStorage;
    VIIPER_UDE_NOTIFICATION *Notifications;
    WDFDPC CompletionDpc;
    ULONG NotificationHead;
    ULONG NotificationTail;
    ULONG NotificationCount;
    WDFFILEOBJECT OwnerFile;
    WDFQUEUE DefaultQueue;
    WDFQUEUE ControlQueue;
    WDFQUEUE InputQueue;
    WDFQUEUE WaitingDequeues;
    WDFTIMER OwnerCleanupTimer;
    BOOLEAN CleanupInProgress;
    volatile LONG BrokerFaulted;
    volatile LONG OwnerReferenced;
    volatile LONG CleanupRetries;
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
    volatile LONG64 NotificationEventsDelivered;
    volatile LONG64 NotificationEventOverflows;
    volatile LONG64 InputReportsSubmitted;
    volatile LONG64 InputReportsCompleted;
    volatile LONG64 IsoPackets;
    volatile LONG64 BytesToDevice;
    volatile LONG64 BytesFromDevice;
    UDECXUSBDEVICE Devices[VIIPER_UDE_MAX_DEVICES];
    BOOLEAN RemovingSlots[VIIPER_UDE_MAX_DEVICES];
} VIIPER_UDE_CONTROLLER_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_CONTROLLER_CONTEXT, ViiperGetControllerContext)

typedef struct VIIPER_UDE_FILE_CONTEXT {
    volatile LONG Negotiated;
    volatile LONG Closing;
    volatile LONG BrokerOwner;
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
    UDECX_USB_DEVICE_SPEED Speed;
    BOOLEAN Plugged;
    volatile LONG Purging;
    volatile LONG ActiveCounted;
    volatile LONG OwnerReferenced;
    UDECXUSBENDPOINT DefaultEndpoint;
    UDECXUSBENDPOINT Endpoints[256];
    BOOLEAN RetiredEndpoints[256];
    volatile LONG64 EndpointSequences[256];
} VIIPER_UDE_DEVICE_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_DEVICE_CONTEXT, ViiperGetDeviceContext)

typedef struct VIIPER_UDE_ENDPOINT_CONTEXT {
    UDECXUSBDEVICE Device;
    WDFQUEUE Queue;
    WDFWAITLOCK InputLock;
    USB_ENDPOINT_DESCRIPTOR Descriptor;
    volatile LONG Purging;
    volatile LONG64 LastInputSequence;
    volatile LONG64 NextIsoStartFrame;
    ULONGLONG NextAdmissionSequence;
    BOOLEAN FastInput;
} VIIPER_UDE_ENDPOINT_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_ENDPOINT_CONTEXT, ViiperGetEndpointContext)
WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(UDECXUSBENDPOINT, ViiperGetQueueEndpoint)

DRIVER_INITIALIZE DriverEntry;
EVT_WDF_DRIVER_DEVICE_ADD ViiperEvtDeviceAdd;
EVT_WDF_OBJECT_CONTEXT_CLEANUP ViiperEvtDriverCleanup;
EVT_WDF_OBJECT_CONTEXT_CLEANUP ViiperEvtControllerCleanup;
EVT_WDF_DEVICE_FILE_CREATE ViiperEvtFileCreate;
EVT_WDF_FILE_CLEANUP ViiperEvtFileCleanup;
EVT_WDF_TIMER ViiperEvtOwnerCleanupRetry;
EVT_WDF_IO_QUEUE_IO_DEVICE_CONTROL ViiperEvtIoDeviceControlRoute;
EVT_WDF_IO_QUEUE_IO_DEVICE_CONTROL ViiperEvtIoDeviceControl;
EVT_WDF_IO_QUEUE_IO_DEVICE_CONTROL ViiperEvtInputIoDeviceControl;
EVT_UDECX_WDF_DEVICE_QUERY_USB_CAPABILITY ViiperEvtQueryUsbCapability;
EVT_UDECX_USB_DEVICE_D0_ENTRY ViiperEvtUsbDeviceD0Entry;
EVT_UDECX_USB_DEVICE_D0_EXIT ViiperEvtUsbDeviceD0Exit;
EVT_UDECX_USB_DEVICE_SET_FUNCTION_SUSPEND_AND_WAKE ViiperEvtUsbDeviceSetFunctionSuspendAndWake;
EVT_UDECX_USB_DEVICE_POST_ENUMERATION_RESET ViiperEvtUsbDeviceReset;
EVT_UDECX_USB_DEVICE_DEFAULT_ENDPOINT_ADD ViiperEvtDefaultEndpointAdd;
EVT_UDECX_USB_DEVICE_ENDPOINT_ADD ViiperEvtEndpointAdd;
EVT_UDECX_USB_DEVICE_ENDPOINTS_CONFIGURE ViiperEvtEndpointsConfigure;
EVT_UDECX_USB_ENDPOINT_RESET ViiperEvtEndpointReset;
EVT_UDECX_USB_ENDPOINT_PURGE ViiperEvtEndpointPurge;
EVT_UDECX_USB_ENDPOINT_START ViiperEvtEndpointStart;
EVT_WDF_IO_QUEUE_IO_INTERNAL_DEVICE_CONTROL ViiperEvtEndpointIoInternalControl;
EVT_WDF_IO_QUEUE_STATE ViiperEvtEndpointQueuePurged;
EVT_WDF_DPC ViiperEvtCompletionDpc;
EVT_WDF_OBJECT_CONTEXT_CLEANUP ViiperEvtVirtualDeviceCleanup;
EVT_WDF_OBJECT_CONTEXT_CLEANUP ViiperEvtEndpointCleanup;

NTSTATUS ViiperCreateQueues(_In_ WDFDEVICE Device);
NTSTATUS ViiperInitializeBroker(_In_ WDFDEVICE Device);
NTSTATUS ViiperCreateVirtualDevice(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request);
NTSTATUS ViiperDestroyVirtualDevice(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request);
BOOLEAN ViiperDestroyOwnedDevices(_In_ WDFDEVICE Controller, _In_ WDFFILEOBJECT OwnerFile);
NTSTATUS ViiperQueueDequeueOperation(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request);
NTSTATUS ViiperCompleteOperation(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request);
NTSTATUS ViiperQueueUrb(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request);
VOID ViiperCompleteUnownedUrbAsync(
    _In_ WDFDEVICE Controller,
    _In_ WDFREQUEST Request,
    _In_ NTSTATUS Status);
NTSTATUS ViiperSubmitInputReport(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request);
NTSTATUS ViiperValidateBrokerOwner(_In_ WDFDEVICE Controller, _In_ WDFREQUEST Request);
PURB ViiperGetUrb(_In_ WDFREQUEST Request);
NTSTATUS ViiperCopyTransferBuffer(
    _In_ WDFREQUEST Request,
    _In_ PURB Urb,
    _Inout_updates_bytes_(Length) UCHAR *Buffer,
    _In_ ULONG Length,
    _In_ BOOLEAN ToUrb);
VOID ViiperPurgeEndpointOperations(_In_ UDECXUSBENDPOINT Endpoint, _In_ NTSTATUS Status);
VOID ViiperPurgeOwnerOperations(_In_ WDFDEVICE Controller, _In_ NTSTATUS Status);
NTSTATUS ViiperQueueEndpointLifecycleEvent(
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ VIIPER_UDE_OPERATION_KIND Kind);
NTSTATUS ViiperQueueDeviceLifecycleEvent(
    _In_ UDECXUSBDEVICE Device,
    _In_ VIIPER_UDE_OPERATION_KIND Kind);
NTSTATUS ViiperQueueInterfaceLifecycleEvent(
    _In_ UDECXUSBDEVICE Device,
    _In_ UCHAR InterfaceNumber,
    _In_ UCHAR InterfaceSetting);

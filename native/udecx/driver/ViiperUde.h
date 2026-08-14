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
#define VIIPER_UDE_MAX_PENDING_MANAGEMENT 256
#define VIIPER_UDE_MAX_INPUT_TRANSITIONS 256
#define VIIPER_UDE_MAX_INPUT_TRANSITION_BYTES 65536
#define VIIPER_UDE_MANAGEMENT_SLOT_FLAG 0x80000000UL
// UdeCx numbers USB 3 ports after every USB 2 port on the controller. Keep
// the topology constants shared by controller creation and child plug-in so
// fixed slot-to-port identity cannot drift between those two boundaries.
#define VIIPER_UDE_USB20_PORT_COUNT VIIPER_UDE_MAX_DEVICES
#define VIIPER_UDE_USB30_PORT_COUNT VIIPER_UDE_MAX_DEVICES

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
    LIST_ENTRY AdmissionEntry;
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
    BOOLEAN AdmissionLinked;
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
    ULONGLONG DeviceSequence;
    ULONG Generation;
    ULONG Kind;
    UCHAR EndpointAddress;
    UCHAR InterfaceNumber;
    UCHAR InterfaceSetting;
    UCHAR EndpointAttributes;
    UCHAR EndpointInterval;
    USHORT EndpointMaxPacketSize;
} VIIPER_UDE_NOTIFICATION;

typedef struct VIIPER_UDE_MANAGEMENT_SLOT {
    WDFREQUEST Request;
    // Private framework-object pins, never exposed on the broker ABI. They
    // prevent a deleted WDF handle value from being recycled while a delayed
    // lifecycle acknowledgement still names this slot. Callers compare these
    // only with handles in the live DeviceLock-protected table; they never
    // access an object's context after its cleanup callback.
    UDECXUSBDEVICE Device;
    UDECXUSBENDPOINT Endpoint;
    WDFFILEOBJECT OwnerFile;
    ULONGLONG Token;
    // One harmless success tombstone for an operation already delivered to
    // user mode when kernel teardown completed its held UdeCx request. This
    // never retains WDF objects and is consumed by the first late ACK.
    ULONGLONG RetiredToken;
    ULONGLONG RetiredDeviceId;
    WDFFILEOBJECT RetiredOwnerFile;
    ULONGLONG DeviceId;
    ULONGLONG ResetEpoch;
    ULONG Generation;
    ULONG DeviceGeneration;
    ULONG RetiredDeviceGeneration;
    VIIPER_UDE_PENDING_STATE State;
    BOOLEAN RetiredNotificationPending;
    ULONG Kind;
    UCHAR EndpointAddress;
} VIIPER_UDE_MANAGEMENT_SLOT;

typedef struct VIIPER_UDE_REQUEST_CONTEXT {
    WDFDEVICE Controller;
    UDECXUSBENDPOINT Endpoint;
    ULONG PendingSlot;
    ULONGLONG Token;
    ULONG TransferLength;
    ULONG IsoPacketCount;
    ULONG IsoStartFrame;
    BOOLEAN DirectionIn;
    // Protected by the controller BrokerLock. The DPC removes and snapshots
    // these fields before UdeCx may recycle this request context.
    LIST_ENTRY CompletionEntry;
    WDFREQUEST CompletionRequest;
    NTSTATUS CompletionStatus;
    USBD_STATUS CompletionUsbdStatus;
    BOOLEAN CompleteWithNtStatus;
    BOOLEAN CompletionQueued;
} VIIPER_UDE_REQUEST_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_REQUEST_CONTEXT, ViiperGetRequestContext)

typedef struct VIIPER_UDE_CONTROLLER_CONTEXT {
    WDFWAITLOCK OwnerLock;
    // UdeCx endpoint/device cleanup can run while the framework is deleting
    // sibling controller children, but the parent context remains alive until
    // every child cleanup callback has returned. A push lock is supported by
    // the driver's Windows 10 1809 floor and is optimized for this shared-heavy
    // identity index. Normal shared acquisition waits behind an exclusive
    // lifecycle writer, so continuous reports cannot starve handle revocation.
    EX_PUSH_LOCK DeviceLock;
    WDFSPINLOCK BrokerLock;
    WDFMEMORY PendingStorage;
    VIIPER_UDE_PENDING_SLOT *PendingSlots;
    ULONG NextPendingSlot;
    ULONG NextDispatchSlot;
    ULONG NextManagementSlot;
    WDFMEMORY NotificationStorage;
    VIIPER_UDE_NOTIFICATION *Notifications;
    WDFMEMORY ManagementStorage;
    VIIPER_UDE_MANAGEMENT_SLOT *ManagementSlots;
    // One nonpageable FIFO owns every terminal UdeCx URB completion. Request
    // contexts provide its entries, so the completion boundary never allocates.
    WDFDPC CompletionDpc;
    LIST_ENTRY CompletionQueue;
    BOOLEAN CompletionDpcActive;
    ULONG NotificationHead;
    ULONG NotificationTail;
    ULONG NotificationCount;
    WDFFILEOBJECT OwnerFile;
    WDFQUEUE DefaultQueue;
    WDFQUEUE ControlQueue;
    WDFQUEUE WaitingDequeues;
    KEVENT BrokerOperationsDrained;
    KEVENT CompletionOperationsDrained;
    KEVENT OwnerAdmissionsDrained;
    KEVENT FileCleanupsDrained;
    BOOLEAN CleanupInProgress;
    volatile LONG ShuttingDown;
    volatile LONG BrokerFaulted;
    volatile LONG OwnerReferenced;
    volatile LONG ActiveOwnerAdmissions;
    volatile LONG ActiveFileCleanups;
    volatile LONG CleanupRetries;
    volatile LONG ActiveDevices;
    volatile LONG PendingOperations;
    volatile LONG PendingCompletions;
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
    volatile LONG64 LifecycleTraceSequence;
    DECLSPEC_ALIGN(8) VIIPER_UDE_LIFECYCLE_TRACE_RECORD
        LifecycleTrace[VIIPER_UDE_LIFECYCLE_TRACE_CAPACITY];
    // Sorted by DeviceId and protected by DeviceLock. The input producer uses
    // a shared binary lookup while lifecycle mutations retain exclusive access
    // to the physical UDE port table below.
    ULONG InputDeviceCount;
    UDECXUSBDEVICE InputDevices[VIIPER_UDE_MAX_DEVICES];
    UDECXUSBDEVICE Devices[VIIPER_UDE_MAX_DEVICES];
} VIIPER_UDE_CONTROLLER_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_CONTROLLER_CONTEXT, ViiperGetControllerContext)

_IRQL_requires_max_(APC_LEVEL)
FORCEINLINE
VOID
ViiperAcquireDeviceLockExclusive(
    _Inout_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext
    )
{
    // Push-lock callers must suppress normal kernel APC delivery from acquire
    // through release and must run at IRQL <= APC_LEVEL.
    KeEnterCriticalRegion();
    ExAcquirePushLockExclusive(&ControllerContext->DeviceLock);
}

_IRQL_requires_max_(APC_LEVEL)
FORCEINLINE
VOID
ViiperAcquireDeviceLockShared(
    _Inout_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext
    )
{
    KeEnterCriticalRegion();
    ExAcquirePushLockShared(&ControllerContext->DeviceLock);
}

_IRQL_requires_max_(APC_LEVEL)
FORCEINLINE
VOID
ViiperReleaseDeviceLockExclusive(
    _Inout_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext
    )
{
    ExReleasePushLockExclusive(&ControllerContext->DeviceLock);
    KeLeaveCriticalRegion();
}

_IRQL_requires_max_(APC_LEVEL)
FORCEINLINE
VOID
ViiperReleaseDeviceLockShared(
    _Inout_ VIIPER_UDE_CONTROLLER_CONTEXT *ControllerContext
    )
{
    ExReleasePushLockShared(&ControllerContext->DeviceLock);
    KeLeaveCriticalRegion();
}

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
    volatile LONG InD0;
    volatile LONG Resetting;
    volatile LONG64 ResetEpoch;
    volatile LONG Purging;
    volatile LONG ActiveCounted;
    volatile LONG OwnerReferenced;
    ULONG MaxPendingOperations;
    volatile LONG PendingOperations;
    UDECXUSBENDPOINT DefaultEndpoint;
    UDECXUSBENDPOINT Endpoints[256];
    BOOLEAN RetiredEndpoints[256];
    volatile LONG64 EndpointSequences[256];
    volatile LONG64 DeviceSequence;
} VIIPER_UDE_DEVICE_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_DEVICE_CONTEXT, ViiperGetDeviceContext)

typedef struct VIIPER_UDE_ENDPOINT_CONTEXT {
    UDECXUSBDEVICE Device;
    WDFQUEUE Queue;
    WDFWAITLOCK InputLock;
    WDFWORKITEM ResetWorkItem;
    WDFREQUEST ResetRequest;
    KEVENT OperationsDrained;
    USB_ENDPOINT_DESCRIPTOR Descriptor;
    volatile LONG Purging;
    volatile LONG StartAnnounced;
    volatile LONG Resetting;
    volatile LONG64 ResetDeviceEpoch;
    volatile LONG ActiveOperations;
    volatile LONG64 LastInputSequence;
    volatile LONG64 NextIsoStartFrame;
    volatile LONG InputReportValid;
    volatile LONG CachedDeliveryPending;
    volatile LONG InputSnapshotPending;
    ULONG InputReportLength;
    UCHAR InputReport[VIIPER_UDE_MAX_INPUT_REPORT_BYTES];
    WDFMEMORY InputTransitionMemory;
    PUCHAR InputTransitionReports;
    ULONG InputTransitionStride;
    ULONG InputTransitionCapacity;
    volatile LONG InputTransitionHead;
    volatile LONG InputTransitionCount;
    USHORT InputTransitionLengths[VIIPER_UDE_MAX_INPUT_TRANSITIONS];
    // BrokerLock protects this FIFO and every slot AdmissionEntry.  It keeps
    // same-endpoint publication ordered without scanning the controller-wide
    // 4096-slot table on every USB transfer.
    LIST_ENTRY AdmissionQueue;
    ULONGLONG NextAdmissionSequence;
    BOOLEAN FastInput;
} VIIPER_UDE_ENDPOINT_CONTEXT;

WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(VIIPER_UDE_ENDPOINT_CONTEXT, ViiperGetEndpointContext)
WDF_DECLARE_CONTEXT_TYPE_WITH_NAME(UDECXUSBENDPOINT, ViiperGetQueueEndpoint)

DRIVER_INITIALIZE DriverEntry;
EVT_WDF_DRIVER_DEVICE_ADD ViiperEvtDeviceAdd;
EVT_WDF_OBJECT_CONTEXT_CLEANUP ViiperEvtDriverCleanup;
EVT_WDF_OBJECT_CONTEXT_CLEANUP ViiperEvtControllerCleanup;
EVT_WDF_DEVICE_SELF_MANAGED_IO_INIT ViiperEvtDeviceSelfManagedIoInit;
EVT_WDF_DEVICE_SELF_MANAGED_IO_CLEANUP ViiperEvtDeviceSelfManagedIoCleanup;
EVT_WDF_DEVICE_FILE_CREATE ViiperEvtFileCreate;
EVT_WDF_FILE_CLEANUP ViiperEvtFileCleanup;
EVT_WDF_FILE_CLOSE ViiperEvtFileClose;
EVT_WDF_IO_QUEUE_IO_DEVICE_CONTROL ViiperEvtIoDeviceControlRoute;
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
EVT_WDF_IO_QUEUE_IO_CANCELED_ON_QUEUE ViiperEvtUrbCanceledOnQueue;
EVT_WDF_IO_QUEUE_STATE ViiperEvtFastInputQueueReady;
EVT_WDF_IO_QUEUE_IO_CANCELED_ON_QUEUE ViiperEvtDequeueCanceledOnQueue;
EVT_WDF_IO_QUEUE_STATE ViiperEvtEndpointQueuePurged;
EVT_WDF_WORKITEM ViiperEvtEndpointResetWorkItem;
EVT_WDF_DPC ViiperEvtCompletionDpc;
EVT_WDF_OBJECT_CONTEXT_CLEANUP ViiperEvtVirtualDeviceCleanup;
EVT_WDF_OBJECT_CONTEXT_CLEANUP ViiperEvtEndpointCleanup;

NTSTATUS ViiperCreateQueues(_In_ WDFDEVICE Device);
NTSTATUS ViiperInitializeBroker(_In_ WDFDEVICE Device);
NTSTATUS ViiperCreateVirtualDevice(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request);
NTSTATUS ViiperDestroyVirtualDevice(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request);
BOOLEAN ViiperDestroyOwnedDevices(_In_ WDFDEVICE Controller, _In_ WDFFILEOBJECT OwnerFile);
VOID ViiperDrainControllerEndpointOperations(_In_ WDFDEVICE Controller);
BOOLEAN ViiperQuiesceResetByIdentity(
    _In_ WDFDEVICE Controller,
    _In_ ULONGLONG DeviceId,
    _In_ ULONG Generation,
    _In_ UDECXUSBDEVICE ExpectedDevice,
    _In_opt_ UDECXUSBENDPOINT ExpectedEndpoint,
    _In_ ULONGLONG ExpectedResetEpoch,
    _In_ UCHAR EndpointAddress,
    _In_ BOOLEAN WholeDevice,
    _In_ BOOLEAN ReleaseGate);
VOID ViiperBeginControllerShutdown(_In_ WDFDEVICE Controller);
NTSTATUS ViiperQueueDequeueOperation(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request);
NTSTATUS ViiperCompleteOperation(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request);
NTSTATUS ViiperQueueUrb(_In_ WDFQUEUE Queue, _In_ WDFREQUEST Request);
VOID ViiperCompleteUnownedUrb(
    _In_ WDFDEVICE Controller,
    _In_ WDFREQUEST Request,
    _In_ NTSTATUS Status);
_IRQL_requires_max_(DISPATCH_LEVEL)
BOOLEAN ViiperQueueUrbCompletion(
    _In_ WDFDEVICE Controller,
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ WDFREQUEST Request,
    _In_ ULONG PendingSlot,
    _In_ ULONGLONG Token,
    _In_ NTSTATUS Status,
    _In_ USBD_STATUS UsbdStatus,
    _In_ BOOLEAN CompleteWithNtStatus);
_IRQL_requires_(PASSIVE_LEVEL)
VOID ViiperDrainUrbCompletions(_In_ WDFDEVICE Controller);
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
VOID ViiperAbortDeviceManagementOperations(
    _In_ WDFDEVICE Controller,
    _In_ UDECXUSBDEVICE Device,
    _In_ NTSTATUS Status);
VOID ViiperRetireManagementTombstonesForOwner(
    _In_ WDFDEVICE Controller,
    _In_opt_ WDFFILEOBJECT OwnerFile);
_IRQL_requires_max_(DISPATCH_LEVEL)
VOID ViiperEndpointOperationStarted(_In_ UDECXUSBENDPOINT Endpoint);
_IRQL_requires_max_(DISPATCH_LEVEL)
VOID ViiperEndpointOperationCompleted(_In_ UDECXUSBENDPOINT Endpoint);
VOID ViiperPurgeOwnerOperations(_In_ WDFDEVICE Controller, _In_ NTSTATUS Status);
VOID ViiperTraceLifecycle(
    _In_ WDFDEVICE Controller,
    _In_ UCHAR Source,
    _In_ USHORT Event,
    _In_ ULONGLONG DeviceId,
    _In_ ULONG Generation,
    _In_opt_ UDECXUSBDEVICE Device,
    _In_opt_ UDECXUSBENDPOINT Endpoint,
    _In_ UCHAR EndpointAddress,
    _In_ NTSTATUS Status,
    _In_ LONG ActiveOperations,
    _In_ ULONG QueueState,
    _In_ ULONG Line);
#define VIIPER_TRACE_LIFECYCLE(Controller, Source, Event, DeviceId, Generation, Device, Endpoint, EndpointAddress, Status, ActiveOperations, QueueState) \
    ViiperTraceLifecycle((Controller), (Source), (Event), (DeviceId), (Generation), \
        (Device), (Endpoint), (EndpointAddress), (Status), (ActiveOperations), \
        (QueueState), __LINE__)
NTSTATUS ViiperQueueEndpointLifecycleEvent(
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ VIIPER_UDE_OPERATION_KIND Kind);
NTSTATUS ViiperQueueDeviceLifecycleEvent(
    _In_ UDECXUSBDEVICE Device,
    _In_ VIIPER_UDE_OPERATION_KIND Kind);
NTSTATUS ViiperQueueAcknowledgedEndpointLifecycleEvent(
    _In_ UDECXUSBENDPOINT Endpoint,
    _In_ WDFREQUEST Request,
    _In_ VIIPER_UDE_OPERATION_KIND Kind);
NTSTATUS ViiperQueueAcknowledgedDeviceLifecycleEvent(
    _In_ UDECXUSBDEVICE Device,
    _In_ WDFREQUEST Request,
    _In_ VIIPER_UDE_OPERATION_KIND Kind);
NTSTATUS ViiperQueueAcknowledgedInterfaceLifecycleEvent(
    _In_ UDECXUSBDEVICE Device,
    _In_ WDFREQUEST Request,
    _In_ UCHAR InterfaceNumber,
    _In_ UCHAR InterfaceSetting);
NTSTATUS ViiperQueueInterfaceLifecycleEvent(
    _In_ UDECXUSBDEVICE Device,
    _In_ UCHAR InterfaceNumber,
    _In_ UCHAR InterfaceSetting);

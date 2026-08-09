#pragma once

#include <stdint.h>

#if defined(_KERNEL_MODE)
#include <devioctl.h>
#elif defined(_WIN32)
#include <winioctl.h>
#endif

#define VIIPER_UDE_MAGIC UINT32_C(0x45445556) /* "VUDE" little-endian */
#define VIIPER_UDE_ABI_MAJOR UINT16_C(1)
#define VIIPER_UDE_ABI_MINOR UINT16_C(0)

#define VIIPER_UDE_MAX_DEVICES UINT32_C(32)
#define VIIPER_UDE_MAX_DESCRIPTOR_BYTES UINT32_C(262144)
#define VIIPER_UDE_MAX_TRANSFER_BYTES UINT32_C(1048576)
#define VIIPER_UDE_MAX_ISO_PACKETS UINT32_C(1024)
#define VIIPER_UDE_MAX_PENDING_OPERATIONS UINT32_C(4096)

#define VIIPER_UDE_CAP_ISOCHRONOUS UINT32_C(0x00000001)
#define VIIPER_UDE_CAP_STREAMS UINT32_C(0x00000002)
#define VIIPER_UDE_CAP_DEVICE_LIFECYCLE UINT32_C(0x00000004)

#if defined(_WIN32)
#define VIIPER_UDE_IOCTL_BASE 0x900
#define IOCTL_VIIPER_UDE_NEGOTIATE CTL_CODE(FILE_DEVICE_UNKNOWN, VIIPER_UDE_IOCTL_BASE + 0, METHOD_BUFFERED, FILE_READ_DATA | FILE_WRITE_DATA)
#define IOCTL_VIIPER_UDE_CREATE_DEVICE CTL_CODE(FILE_DEVICE_UNKNOWN, VIIPER_UDE_IOCTL_BASE + 1, METHOD_BUFFERED, FILE_READ_DATA | FILE_WRITE_DATA)
#define IOCTL_VIIPER_UDE_DESTROY_DEVICE CTL_CODE(FILE_DEVICE_UNKNOWN, VIIPER_UDE_IOCTL_BASE + 2, METHOD_BUFFERED, FILE_READ_DATA | FILE_WRITE_DATA)
#define IOCTL_VIIPER_UDE_DEQUEUE_OPERATION CTL_CODE(FILE_DEVICE_UNKNOWN, VIIPER_UDE_IOCTL_BASE + 3, METHOD_OUT_DIRECT, FILE_READ_DATA | FILE_WRITE_DATA)
#define IOCTL_VIIPER_UDE_COMPLETE_OPERATION CTL_CODE(FILE_DEVICE_UNKNOWN, VIIPER_UDE_IOCTL_BASE + 4, METHOD_IN_DIRECT, FILE_READ_DATA | FILE_WRITE_DATA)
#define IOCTL_VIIPER_UDE_QUERY_STATS CTL_CODE(FILE_DEVICE_UNKNOWN, VIIPER_UDE_IOCTL_BASE + 5, METHOD_BUFFERED, FILE_READ_DATA)
#endif

#pragma pack(push, 1)

typedef struct VIIPER_UDE_HEADER {
    uint32_t Magic;
    uint16_t Major;
    uint16_t Minor;
    uint32_t Size;
    uint32_t Flags;
} VIIPER_UDE_HEADER;

typedef struct VIIPER_UDE_NEGOTIATE_REQUEST {
    VIIPER_UDE_HEADER Header;
    uint64_t ClientNonce;
    uint32_t RequestedCapabilities;
    uint32_t Reserved;
} VIIPER_UDE_NEGOTIATE_REQUEST;

typedef struct VIIPER_UDE_NEGOTIATE_RESPONSE {
    VIIPER_UDE_HEADER Header;
    uint64_t ClientNonce;
    uint64_t DriverNonce;
    uint32_t Capabilities;
    uint32_t MaxDevices;
    uint32_t MaxDescriptorBytes;
    uint32_t MaxTransferBytes;
    uint32_t MaxIsoPackets;
    uint32_t MaxPendingOperations;
} VIIPER_UDE_NEGOTIATE_RESPONSE;

typedef enum VIIPER_UDE_DESCRIPTOR_KIND {
    ViiperUdeDescriptorDevice = 1,
    ViiperUdeDescriptorConfiguration = 2,
    ViiperUdeDescriptorBos = 3,
    ViiperUdeDescriptorString = 4
} VIIPER_UDE_DESCRIPTOR_KIND;

typedef struct VIIPER_UDE_DESCRIPTOR_RECORD {
    uint16_t Kind;
    uint16_t Index;
    uint16_t LanguageId;
    uint16_t Reserved;
    uint32_t Offset;
    uint32_t Length;
} VIIPER_UDE_DESCRIPTOR_RECORD;

typedef struct VIIPER_UDE_CREATE_DEVICE {
    VIIPER_UDE_HEADER Header;
    uint64_t DeviceId;
    uint32_t Generation;
    uint32_t Speed;
    uint32_t DescriptorCount;
    uint32_t DescriptorRecordsOffset;
    uint32_t DescriptorDataOffset;
    uint32_t DescriptorDataLength;
    uint32_t MaxPendingOperations;
    uint32_t Reserved;
} VIIPER_UDE_CREATE_DEVICE;

typedef struct VIIPER_UDE_DEVICE_IDENTITY {
    VIIPER_UDE_HEADER Header;
    uint64_t DeviceId;
    uint32_t Generation;
    uint32_t Reserved;
} VIIPER_UDE_DEVICE_IDENTITY;

typedef enum VIIPER_UDE_OPERATION_KIND {
    ViiperUdeOperationControl = 1,
    ViiperUdeOperationTransfer = 2,
    ViiperUdeOperationEndpointStart = 3,
    ViiperUdeOperationEndpointPurge = 4,
    ViiperUdeOperationEndpointReset = 5,
    ViiperUdeOperationDeviceReset = 6,
    ViiperUdeOperationSetInterface = 7,
    ViiperUdeOperationDeviceD0Entry = 8,
    ViiperUdeOperationDeviceD0Exit = 9
} VIIPER_UDE_OPERATION_KIND;

typedef struct VIIPER_UDE_ISO_PACKET {
    uint32_t Offset;
    uint32_t Length;
    int32_t Status;
    uint32_t Reserved;
} VIIPER_UDE_ISO_PACKET;

typedef struct VIIPER_UDE_OPERATION {
    VIIPER_UDE_HEADER Header;
    uint64_t Token;
    uint64_t DeviceId;
    uint32_t Generation;
    uint32_t Kind;
    uint8_t EndpointAddress;
    uint8_t Direction;
    uint16_t Reserved0;
    uint32_t UrbFunction;
    uint32_t TransferFlags;
    uint32_t StartFrame;
    uint32_t IsoPacketCount;
    uint32_t TransferLength;
    uint32_t PayloadOffset;
    uint32_t PayloadLength;
    uint32_t IsoPacketsOffset;
    uint8_t SetupPacket[8];
    uint32_t Reserved1;
} VIIPER_UDE_OPERATION;

typedef struct VIIPER_UDE_COMPLETION {
    VIIPER_UDE_HEADER Header;
    uint64_t Token;
    uint64_t DeviceId;
    uint32_t Generation;
    int32_t Status;
    uint32_t UsbdStatus;
    uint32_t TransferLength;
    uint32_t IsoPacketCount;
    uint32_t PayloadOffset;
    uint32_t PayloadLength;
    uint32_t IsoPacketsOffset;
    uint32_t Reserved;
} VIIPER_UDE_COMPLETION;

typedef struct VIIPER_UDE_STATS {
    VIIPER_UDE_HEADER Header;
    uint64_t OperationsDequeued;
    uint64_t OperationsCompleted;
    uint64_t OperationsCancelled;
    uint64_t OperationsPurged;
    uint64_t LateCompletions;
    uint64_t InvalidMessages;
    uint64_t QueueExhaustions;
    uint64_t IsoPackets;
    uint64_t BytesToDevice;
    uint64_t BytesFromDevice;
    uint32_t ActiveDevices;
    uint32_t PendingOperations;
    uint32_t WaitingDequeues;
    uint32_t Reserved;
} VIIPER_UDE_STATS;

#pragma pack(pop)

#if defined(__cplusplus)
static_assert(sizeof(VIIPER_UDE_HEADER) == 16, "VIIPER_UDE_HEADER ABI drift");
static_assert(sizeof(VIIPER_UDE_NEGOTIATE_REQUEST) == 32, "VIIPER_UDE_NEGOTIATE_REQUEST ABI drift");
static_assert(sizeof(VIIPER_UDE_NEGOTIATE_RESPONSE) == 56, "VIIPER_UDE_NEGOTIATE_RESPONSE ABI drift");
static_assert(sizeof(VIIPER_UDE_DESCRIPTOR_RECORD) == 16, "VIIPER_UDE_DESCRIPTOR_RECORD ABI drift");
static_assert(sizeof(VIIPER_UDE_CREATE_DEVICE) == 56, "VIIPER_UDE_CREATE_DEVICE ABI drift");
static_assert(sizeof(VIIPER_UDE_DEVICE_IDENTITY) == 32, "VIIPER_UDE_DEVICE_IDENTITY ABI drift");
static_assert(sizeof(VIIPER_UDE_ISO_PACKET) == 16, "VIIPER_UDE_ISO_PACKET ABI drift");
static_assert(sizeof(VIIPER_UDE_OPERATION) == 88, "VIIPER_UDE_OPERATION ABI drift");
static_assert(sizeof(VIIPER_UDE_COMPLETION) == 72, "VIIPER_UDE_COMPLETION ABI drift");
static_assert(sizeof(VIIPER_UDE_STATS) == 112, "VIIPER_UDE_STATS ABI drift");
#elif defined(__STDC_VERSION__) && __STDC_VERSION__ >= 201112L
_Static_assert(sizeof(VIIPER_UDE_HEADER) == 16, "VIIPER_UDE_HEADER ABI drift");
_Static_assert(sizeof(VIIPER_UDE_NEGOTIATE_REQUEST) == 32, "VIIPER_UDE_NEGOTIATE_REQUEST ABI drift");
_Static_assert(sizeof(VIIPER_UDE_NEGOTIATE_RESPONSE) == 56, "VIIPER_UDE_NEGOTIATE_RESPONSE ABI drift");
_Static_assert(sizeof(VIIPER_UDE_DESCRIPTOR_RECORD) == 16, "VIIPER_UDE_DESCRIPTOR_RECORD ABI drift");
_Static_assert(sizeof(VIIPER_UDE_CREATE_DEVICE) == 56, "VIIPER_UDE_CREATE_DEVICE ABI drift");
_Static_assert(sizeof(VIIPER_UDE_DEVICE_IDENTITY) == 32, "VIIPER_UDE_DEVICE_IDENTITY ABI drift");
_Static_assert(sizeof(VIIPER_UDE_ISO_PACKET) == 16, "VIIPER_UDE_ISO_PACKET ABI drift");
_Static_assert(sizeof(VIIPER_UDE_OPERATION) == 88, "VIIPER_UDE_OPERATION ABI drift");
_Static_assert(sizeof(VIIPER_UDE_COMPLETION) == 72, "VIIPER_UDE_COMPLETION ABI drift");
_Static_assert(sizeof(VIIPER_UDE_STATS) == 112, "VIIPER_UDE_STATS ABI drift");
#endif

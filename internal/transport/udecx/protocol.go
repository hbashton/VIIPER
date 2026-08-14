// Package udecx defines the user-mode half of the native VIIPER UdeCx ABI.
// It intentionally has no Windows dependency so layout and fuzz tests run on
// every supported development host.
package udecx

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
)

const (
	Magic    uint32 = 0x45445556
	ABIMajor uint16 = 1
	ABIMinor uint16 = 11
	// DriverPackageVersion is the native driver package version built and
	// shipped with this service. Runtime negotiation proves the loaded driver
	// carries this version in its source-bound build identity; package
	// installation additionally verifies DriverVer and the signed catalog.
	DriverPackageVersion = "0.1.0.32"
	BuildIdentitySize    = sha256.Size

	HeaderSize               = 16
	NegotiateRequestSize     = 32
	NegotiateResponseSize    = 88
	DescriptorRecordSize     = 16
	CreateDeviceSize         = 56
	DeviceIdentitySize       = 32
	IsoPacketSize            = 16
	OperationSize            = 104
	CompletionSize           = 72
	InputReportSize          = 48
	StatsSize                = 144
	LifecycleTraceRecordSize = 80
	LifecycleTraceSize       = 41008
	LifecycleTraceCapacity   = 512

	MaxDevices                  = 32
	MaxDescriptorBytes          = 256 * 1024
	MaxTransferBytes            = 1024 * 1024
	MaxIsoPackets               = 1024
	MaxInputReportBytes         = 4096
	MaxPendingOperations        = 4096
	InputReportTransition uint8 = 0x01
	// TransferFlagDirectionIn is the wire value of
	// USBD_TRANSFER_DIRECTION_IN from usb.h.
	TransferFlagDirectionIn uint32 = 0x00000001
	// TransferFlagStartIsoASAP is the wire value of
	// USBD_START_ISO_TRANSFER_ASAP from usb.h.
	TransferFlagStartIsoASAP uint32 = 0x00000004
	// USBDStatusBadStartFrame is the wire value of
	// USBD_STATUS_BAD_START_FRAME from the Microsoft WDK usb.h contract.
	USBDStatusBadStartFrame uint32 = 0xC0000A00

	MicrosoftOS10StringIndex      = 0x00EE
	MicrosoftOS10StringLength     = 18
	MicrosoftOS10VendorCodeOffset = 16
)

var (
	ErrShortMessage      = errors.New("native UDE message is shorter than its fixed header")
	ErrBadMagic          = errors.New("native UDE message has an invalid magic value")
	ErrIncompatibleMajor = errors.New("native UDE ABI major version is incompatible")
	ErrIncompatibleMinor = errors.New("native UDE ABI minor version is incompatible")
	ErrIncompatibleABI   = errors.New("native UDE service and driver ABIs are incompatible")
	ErrInvalidSize       = errors.New("native UDE message size is invalid")
	ErrInvalidRange      = errors.New("native UDE message contains an invalid range")
	ErrLimitExceeded     = errors.New("native UDE message exceeds a negotiated limit")
	ErrBuildIdentity     = errors.New("native UDE build identity is unavailable or invalid")
	ErrInputQueueFull    = errors.New("native UDE input transition queue is full")
)

type Capabilities uint32

const (
	CapabilityIsochronous Capabilities = 1 << iota
	CapabilityStreams
	CapabilityDeviceLifecycle
	CapabilityInputReports
	CapabilityLifecycleTrace
)

const AdvertisedCapabilities = CapabilityIsochronous | CapabilityDeviceLifecycle |
	CapabilityInputReports | CapabilityLifecycleTrace

const (
	TraceSourceDevice uint8 = iota + 1
	TraceSourceBroker
	TraceSourceController
)

const (
	TraceCreateBegin uint16 = iota + 1
	TraceDeviceCreateReturned
	TraceDeviceSlotClaimed
	TracePlugInBegin
	TracePlugInReturned
	TraceRemoveClaimed
	TraceManagementAbortBegin
	TraceManagementAbortEnd
	TracePlugOutBegin
	TracePlugOutReturned
	TraceEndpointPurgeBegin
	TraceEndpointOperationsPurged
	TraceEndpointQueuePurgeRequested
	TraceEndpointQueuePurged
	TraceEndpointDrainBegin
	TraceEndpointDrainEnd
	TraceEndpointPurgeCompleteBegin
	TraceEndpointPurgeCompleteEnd
	TraceEndpointCleanupBegin
	TraceEndpointCleanupEnd
	TraceDeviceCleanupBegin
	TraceDeviceCleanupEnd
	TraceControllerShutdownBegin
	TraceControllerShutdownEnd
)

// nativeSourceRevision must be injected by the production build. Native
// transport startup deliberately has no VCS/on-disk fallback: the broker and
// loaded kernel image must derive their identities from the same explicit
// source-bound build input.
var nativeSourceRevision string

// DeriveBuildIdentity returns the source/package/ABI/capability identity that
// is embedded in the native driver and compared during negotiation. The exact
// UTF-8 preimage is also implemented by Get-ViiperUdeBuildIdentity.ps1 and the
// package helper; changing it requires another ABI revision.
func DeriveBuildIdentity(sourceRevision, driverPackageVersion string, abiMajor, abiMinor uint16, capabilities Capabilities) ([BuildIdentitySize]byte, error) {
	var zero [BuildIdentitySize]byte
	if sourceRevision != strings.TrimSpace(sourceRevision) {
		return zero, fmt.Errorf("%w: source revision must not contain surrounding whitespace", ErrBuildIdentity)
	}
	revision := strings.ToLower(sourceRevision)
	if len(revision) != 40 && len(revision) != 64 {
		return zero, fmt.Errorf("%w: source revision must be exactly 40 or 64 hexadecimal digits", ErrBuildIdentity)
	}
	if _, err := hex.DecodeString(revision); err != nil {
		return zero, fmt.Errorf("%w: source revision: %v", ErrBuildIdentity, err)
	}
	versionParts := strings.Split(driverPackageVersion, ".")
	if len(versionParts) != 4 {
		return zero, fmt.Errorf("%w: driver package version must contain four numeric parts", ErrBuildIdentity)
	}
	for _, part := range versionParts {
		if part == "" {
			return zero, fmt.Errorf("%w: driver package version contains an empty part", ErrBuildIdentity)
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return zero, fmt.Errorf("%w: driver package version is not numeric", ErrBuildIdentity)
			}
		}
	}
	if abiMajor == 0 || capabilities == 0 {
		return zero, fmt.Errorf("%w: ABI major and capabilities must be nonzero", ErrBuildIdentity)
	}
	preimage := fmt.Sprintf(
		"VIIPER-UDE-BUILD-IDENTITY/v1\nsourceRevision=%s\ndriverPackageVersion=%s\nabi=%d.%d\ncapabilities=0x%08x\n",
		revision, driverPackageVersion, abiMajor, abiMinor, uint32(capabilities),
	)
	return sha256.Sum256([]byte(preimage)), nil
}

func ExpectedBuildIdentity() ([BuildIdentitySize]byte, error) {
	if strings.TrimSpace(nativeSourceRevision) == "" {
		return [BuildIdentitySize]byte{}, fmt.Errorf(
			"%w: production build did not inject VIIPER native source revision", ErrBuildIdentity)
	}
	return DeriveBuildIdentity(nativeSourceRevision, DriverPackageVersion,
		ABIMajor, ABIMinor, AdvertisedCapabilities)
}

func BuildIdentityHex(identity [BuildIdentitySize]byte) string {
	return hex.EncodeToString(identity[:])
}

type Header struct {
	Magic uint32
	Major uint16
	Minor uint16
	Size  uint32
	Flags uint32
}

func NewHeader(size int) (Header, error) {
	if size < HeaderSize || uint64(size) > math.MaxUint32 {
		return Header{}, ErrInvalidSize
	}
	return Header{Magic: Magic, Major: ABIMajor, Minor: ABIMinor, Size: uint32(size)}, nil
}

func ParseHeader(src []byte) (Header, error) {
	if len(src) < HeaderSize {
		return Header{}, ErrShortMessage
	}
	h := Header{
		Magic: binary.LittleEndian.Uint32(src[0:4]),
		Major: binary.LittleEndian.Uint16(src[4:6]),
		Minor: binary.LittleEndian.Uint16(src[6:8]),
		Size:  binary.LittleEndian.Uint32(src[8:12]),
		Flags: binary.LittleEndian.Uint32(src[12:16]),
	}
	if h.Magic != Magic {
		return Header{}, ErrBadMagic
	}
	if h.Major != ABIMajor {
		return Header{}, fmt.Errorf("%w: driver=%d client=%d", ErrIncompatibleMajor, h.Major, ABIMajor)
	}
	if h.Minor != ABIMinor {
		return Header{}, fmt.Errorf("%w: driver=%d client=%d", ErrIncompatibleMinor, h.Minor, ABIMinor)
	}
	if h.Flags != 0 {
		return Header{}, fmt.Errorf("%w: unsupported header flags %#x", ErrInvalidRange, h.Flags)
	}
	if h.Size < HeaderSize || uint64(h.Size) > uint64(len(src)) {
		return Header{}, ErrInvalidSize
	}
	return h, nil
}

func putHeader(dst []byte, h Header) {
	binary.LittleEndian.PutUint32(dst[0:4], h.Magic)
	binary.LittleEndian.PutUint16(dst[4:6], h.Major)
	binary.LittleEndian.PutUint16(dst[6:8], h.Minor)
	binary.LittleEndian.PutUint32(dst[8:12], h.Size)
	binary.LittleEndian.PutUint32(dst[12:16], h.Flags)
}

type NegotiateRequest struct {
	ClientNonce           uint64
	RequestedCapabilities Capabilities
}

func (m NegotiateRequest) MarshalBinary() ([]byte, error) {
	h, err := NewHeader(NegotiateRequestSize)
	if err != nil {
		return nil, err
	}
	dst := make([]byte, NegotiateRequestSize)
	putHeader(dst, h)
	binary.LittleEndian.PutUint64(dst[16:24], m.ClientNonce)
	binary.LittleEndian.PutUint32(dst[24:28], uint32(m.RequestedCapabilities))
	return dst, nil
}

type NegotiateResponse struct {
	ClientNonce          uint64
	DriverNonce          uint64
	Capabilities         Capabilities
	MaxDevices           uint32
	MaxDescriptorBytes   uint32
	MaxTransferBytes     uint32
	MaxIsoPackets        uint32
	MaxPendingOperations uint32
	BuildIdentity        [BuildIdentitySize]byte
}

func ParseNegotiateResponse(src []byte) (NegotiateResponse, error) {
	h, err := ParseHeader(src)
	if err != nil {
		return NegotiateResponse{}, err
	}
	if h.Size != NegotiateResponseSize {
		return NegotiateResponse{}, ErrInvalidSize
	}
	response := NegotiateResponse{
		ClientNonce:          binary.LittleEndian.Uint64(src[16:24]),
		DriverNonce:          binary.LittleEndian.Uint64(src[24:32]),
		Capabilities:         Capabilities(binary.LittleEndian.Uint32(src[32:36])),
		MaxDevices:           binary.LittleEndian.Uint32(src[36:40]),
		MaxDescriptorBytes:   binary.LittleEndian.Uint32(src[40:44]),
		MaxTransferBytes:     binary.LittleEndian.Uint32(src[44:48]),
		MaxIsoPackets:        binary.LittleEndian.Uint32(src[48:52]),
		MaxPendingOperations: binary.LittleEndian.Uint32(src[52:56]),
	}
	copy(response.BuildIdentity[:], src[56:88])
	return response, nil
}

type DescriptorKind uint16

const (
	DescriptorDevice DescriptorKind = iota + 1
	DescriptorConfiguration
	DescriptorBOS
	DescriptorString
)

type DescriptorRecord struct {
	Kind       DescriptorKind
	Index      uint16
	LanguageID uint16
	Offset     uint32
	Length     uint32
}

type DeviceSpeed uint32

const (
	DeviceSpeedLow DeviceSpeed = iota + 1
	DeviceSpeedFull
	DeviceSpeedHigh
	DeviceSpeedSuper
)

type DeviceIdentity struct {
	DeviceID   uint64
	Generation uint32
}

func (m DeviceIdentity) MarshalBinary() ([]byte, error) {
	if m.DeviceID == 0 || m.Generation == 0 {
		return nil, fmt.Errorf("%w: zero device identity", ErrInvalidRange)
	}
	h, err := NewHeader(DeviceIdentitySize)
	if err != nil {
		return nil, err
	}
	dst := make([]byte, DeviceIdentitySize)
	putHeader(dst, h)
	binary.LittleEndian.PutUint64(dst[16:24], m.DeviceID)
	binary.LittleEndian.PutUint32(dst[24:28], m.Generation)
	return dst, nil
}

type CreateDevice struct {
	DeviceID             uint64
	Generation           uint32
	Speed                DeviceSpeed
	MaxPendingOperations uint32
	Descriptors          []DescriptorRecord
	DescriptorData       []byte
}

func (m CreateDevice) MarshalBinary() ([]byte, error) {
	if m.DeviceID == 0 || m.Generation == 0 {
		return nil, fmt.Errorf("%w: zero device identity", ErrInvalidRange)
	}
	if len(m.Descriptors) == 0 || len(m.DescriptorData) == 0 {
		return nil, fmt.Errorf("%w: empty descriptor set", ErrInvalidRange)
	}
	if len(m.DescriptorData) > MaxDescriptorBytes || len(m.Descriptors) > MaxDescriptorBytes/DescriptorRecordSize {
		return nil, ErrLimitExceeded
	}
	recordBytes := len(m.Descriptors) * DescriptorRecordSize
	total := CreateDeviceSize + recordBytes + len(m.DescriptorData)
	if uint64(total) > math.MaxUint32 {
		return nil, ErrLimitExceeded
	}
	h, err := NewHeader(total)
	if err != nil {
		return nil, err
	}
	dst := make([]byte, total)
	putHeader(dst, h)
	binary.LittleEndian.PutUint64(dst[16:24], m.DeviceID)
	binary.LittleEndian.PutUint32(dst[24:28], m.Generation)
	binary.LittleEndian.PutUint32(dst[28:32], uint32(m.Speed))
	binary.LittleEndian.PutUint32(dst[32:36], uint32(len(m.Descriptors)))
	binary.LittleEndian.PutUint32(dst[36:40], CreateDeviceSize)
	binary.LittleEndian.PutUint32(dst[40:44], uint32(CreateDeviceSize+recordBytes))
	binary.LittleEndian.PutUint32(dst[44:48], uint32(len(m.DescriptorData)))
	binary.LittleEndian.PutUint32(dst[48:52], m.MaxPendingOperations)

	for i, record := range m.Descriptors {
		if !validRange(record.Offset, record.Length, uint32(len(m.DescriptorData))) {
			return nil, fmt.Errorf("%w: descriptor %d", ErrInvalidRange, i)
		}
		off := CreateDeviceSize + i*DescriptorRecordSize
		binary.LittleEndian.PutUint16(dst[off:off+2], uint16(record.Kind))
		binary.LittleEndian.PutUint16(dst[off+2:off+4], record.Index)
		binary.LittleEndian.PutUint16(dst[off+4:off+6], record.LanguageID)
		binary.LittleEndian.PutUint32(dst[off+8:off+12], record.Offset)
		binary.LittleEndian.PutUint32(dst[off+12:off+16], record.Length)
	}
	copy(dst[CreateDeviceSize+recordBytes:], m.DescriptorData)
	return dst, nil
}

type OperationKind uint32

const (
	OperationControl OperationKind = iota + 1
	OperationTransfer
	OperationEndpointStart
	OperationEndpointPurge
	OperationEndpointReset
	OperationDeviceReset
	OperationSetInterface
	OperationDeviceD0Entry
	OperationDeviceD0Exit
	OperationCancel
	OperationBrokerFault
)

type IsoPacket struct {
	Offset uint32
	Length uint32
	Status int32
}

type Operation struct {
	Token                 uint64
	DeviceID              uint64
	Generation            uint32
	Kind                  OperationKind
	EndpointAddress       uint8
	Direction             uint8
	InterfaceNumber       uint8
	InterfaceSetting      uint8
	EndpointAttributes    uint8
	EndpointInterval      uint8
	EndpointMaxPacketSize uint16
	URBFunction           uint32
	TransferFlags         uint32
	StartFrame            uint32
	TransferLength        uint32
	SetupPacket           [8]byte
	IsoPackets            []IsoPacket
	Payload               []byte
	EndpointSequence      uint64
	DeviceSequence        uint64
}

func ParseOperation(src []byte) (Operation, error) {
	h, err := ParseHeader(src)
	if err != nil {
		return Operation{}, err
	}
	if h.Size < OperationSize || h.Size > MaxTransferBytes+OperationSize+MaxIsoPackets*IsoPacketSize {
		return Operation{}, ErrInvalidSize
	}
	// DeviceIoControl returns an independent byte count. Accepting an embedded
	// size smaller than that count silently discards an unvalidated tail and can
	// desynchronize the operation stream. A dequeued operation is one exact
	// message, never a prefix of one.
	if uint64(h.Size) != uint64(len(src)) {
		return Operation{}, ErrInvalidSize
	}
	packetCount := binary.LittleEndian.Uint32(src[56:60])
	transferLength := binary.LittleEndian.Uint32(src[60:64])
	payloadOffset := binary.LittleEndian.Uint32(src[64:68])
	payloadLength := binary.LittleEndian.Uint32(src[68:72])
	isoOffset := binary.LittleEndian.Uint32(src[72:76])
	if packetCount > MaxIsoPackets || transferLength > MaxTransferBytes || payloadLength > MaxTransferBytes {
		return Operation{}, ErrLimitExceeded
	}
	isoBytes := packetCount * IsoPacketSize
	expectedPayloadOffset := uint32(OperationSize) + isoBytes
	// The kernel serializer emits a single canonical tail: ISO metadata first,
	// then payload, with neither gaps nor aliases. Rejecting alternate layouts
	// closes overlap/header-alias ambiguity before any slice is formed.
	if isoOffset != OperationSize || payloadOffset != expectedPayloadOffset ||
		uint64(expectedPayloadOffset)+uint64(payloadLength) != uint64(h.Size) ||
		!validArrayRange(isoOffset, packetCount, IsoPacketSize, h.Size) ||
		!validRange(payloadOffset, payloadLength, h.Size) {
		return Operation{}, ErrInvalidRange
	}
	op := Operation{
		Token:                 binary.LittleEndian.Uint64(src[16:24]),
		DeviceID:              binary.LittleEndian.Uint64(src[24:32]),
		Generation:            binary.LittleEndian.Uint32(src[32:36]),
		Kind:                  OperationKind(binary.LittleEndian.Uint32(src[36:40])),
		EndpointAddress:       src[40],
		Direction:             src[41],
		InterfaceNumber:       src[42],
		InterfaceSetting:      src[43],
		EndpointAttributes:    src[84],
		EndpointInterval:      src[85],
		EndpointMaxPacketSize: binary.LittleEndian.Uint16(src[86:88]),
		URBFunction:           binary.LittleEndian.Uint32(src[44:48]),
		TransferFlags:         binary.LittleEndian.Uint32(src[48:52]),
		StartFrame:            binary.LittleEndian.Uint32(src[52:56]),
		TransferLength:        transferLength,
		EndpointSequence:      binary.LittleEndian.Uint64(src[88:96]),
		DeviceSequence:        binary.LittleEndian.Uint64(src[96:104]),
		IsoPackets:            make([]IsoPacket, int(packetCount)),
		Payload:               append([]byte(nil), src[payloadOffset:payloadOffset+payloadLength]...),
	}
	copy(op.SetupPacket[:], src[76:84])
	for i := range op.IsoPackets {
		off := int(isoOffset) + i*IsoPacketSize
		packet := IsoPacket{
			Offset: binary.LittleEndian.Uint32(src[off : off+4]),
			Length: binary.LittleEndian.Uint32(src[off+4 : off+8]),
			Status: int32(binary.LittleEndian.Uint32(src[off+8 : off+12])),
		}
		if binary.LittleEndian.Uint32(src[off+12:off+16]) != 0 ||
			!validRange(packet.Offset, packet.Length, transferLength) {
			return Operation{}, fmt.Errorf("%w: ISO packet %d", ErrInvalidRange, i)
		}
		op.IsoPackets[i] = packet
	}
	return op, nil
}

// parseDequeuedOperation binds the kernel's independent bytes-returned value
// to the embedded wire size before ParseOperation sees the message. Keeping
// this validation platform-neutral makes malformed completion fixtures
// deterministic on every CI host.
func parseDequeuedOperation(buffer []byte, bytesReturned uint32) (Operation, error) {
	if bytesReturned < OperationSize || uint64(bytesReturned) > uint64(len(buffer)) {
		return Operation{}, ErrInvalidSize
	}
	if binary.LittleEndian.Uint32(buffer[8:12]) != bytesReturned {
		return Operation{}, ErrInvalidSize
	}
	return ParseOperation(buffer[:bytesReturned])
}

type Completion struct {
	Token      uint64
	DeviceID   uint64
	Generation uint32
	Status     int32
	USBDStatus uint32
	// TransferLength is the number of bytes completed. For ISO-IN transfers,
	// Payload may span the original gapped transfer buffer and therefore be
	// larger than this sum of packet actual lengths.
	TransferLength uint32
	IsoPackets     []IsoPacket
	Payload        []byte
}

// InputReport is the ViGEm-style fast path for interrupt-IN endpoints. The
// host parks the Windows polling request in the kernel and user mode submits
// only a fresh, already encoded report. Audio, control, output, and lifecycle
// traffic deliberately remain on the ordered operation broker.
type InputReport struct {
	DeviceID        uint64
	Generation      uint32
	EndpointAddress uint8
	Transition      bool
	Sequence        uint64
	Payload         []byte
}

func (m InputReport) marshalMetadata(dst []byte) error {
	if m.DeviceID == 0 || m.Generation == 0 || m.EndpointAddress&0x80 == 0 ||
		m.Sequence == 0 || m.Sequence > math.MaxInt64 {
		return fmt.Errorf("%w: invalid input-report identity", ErrInvalidRange)
	}
	if len(m.Payload) == 0 || len(m.Payload) > MaxInputReportBytes {
		return ErrLimitExceeded
	}
	total := InputReportSize + len(m.Payload)
	h, err := NewHeader(total)
	if err != nil {
		return err
	}
	if len(dst) != InputReportSize {
		return ErrInvalidSize
	}
	putHeader(dst, h)
	binary.LittleEndian.PutUint64(dst[16:24], m.DeviceID)
	binary.LittleEndian.PutUint32(dst[24:28], m.Generation)
	dst[28] = m.EndpointAddress
	dst[29], dst[30], dst[31] = 0, 0, 0
	if m.Transition {
		dst[29] = InputReportTransition
	}
	binary.LittleEndian.PutUint32(dst[32:36], InputReportSize)
	binary.LittleEndian.PutUint32(dst[36:40], uint32(len(m.Payload)))
	binary.LittleEndian.PutUint64(dst[40:48], m.Sequence)
	return nil
}

func (m InputReport) MarshalBinary() ([]byte, error) {
	var metadata [InputReportSize]byte
	if err := m.marshalMetadata(metadata[:]); err != nil {
		return nil, err
	}
	dst := make([]byte, InputReportSize+len(m.Payload))
	copy(dst[:InputReportSize], metadata[:])
	copy(dst[InputReportSize:], m.Payload)
	return dst, nil
}

type Stats struct {
	OperationsDequeued         uint64
	OperationsCompleted        uint64
	OperationsCancelled        uint64
	OperationsPurged           uint64
	LateCompletions            uint64
	InvalidMessages            uint64
	QueueExhaustions           uint64
	IsoPackets                 uint64
	BytesToDevice              uint64
	BytesFromDevice            uint64
	NotificationEvents         uint64
	NotificationEventOverflows uint64
	ActiveDevices              uint32
	PendingOperations          uint32
	WaitingDequeues            uint32
	CleanupRetries             uint32
	InputReportsSubmitted      uint64
	InputReportsCompleted      uint64
}

func ParseStats(src []byte) (Stats, error) {
	h, err := ParseHeader(src)
	if err != nil {
		return Stats{}, err
	}
	if h.Size != StatsSize {
		return Stats{}, ErrInvalidSize
	}
	return Stats{
		OperationsDequeued:         binary.LittleEndian.Uint64(src[16:24]),
		OperationsCompleted:        binary.LittleEndian.Uint64(src[24:32]),
		OperationsCancelled:        binary.LittleEndian.Uint64(src[32:40]),
		OperationsPurged:           binary.LittleEndian.Uint64(src[40:48]),
		LateCompletions:            binary.LittleEndian.Uint64(src[48:56]),
		InvalidMessages:            binary.LittleEndian.Uint64(src[56:64]),
		QueueExhaustions:           binary.LittleEndian.Uint64(src[64:72]),
		IsoPackets:                 binary.LittleEndian.Uint64(src[72:80]),
		BytesToDevice:              binary.LittleEndian.Uint64(src[80:88]),
		BytesFromDevice:            binary.LittleEndian.Uint64(src[88:96]),
		NotificationEvents:         binary.LittleEndian.Uint64(src[96:104]),
		NotificationEventOverflows: binary.LittleEndian.Uint64(src[104:112]),
		ActiveDevices:              binary.LittleEndian.Uint32(src[112:116]),
		PendingOperations:          binary.LittleEndian.Uint32(src[116:120]),
		WaitingDequeues:            binary.LittleEndian.Uint32(src[120:124]),
		CleanupRetries:             binary.LittleEndian.Uint32(src[124:128]),
		InputReportsSubmitted:      binary.LittleEndian.Uint64(src[128:136]),
		InputReportsCompleted:      binary.LittleEndian.Uint64(src[136:144]),
	}, nil
}

type LifecycleTraceRecord struct {
	PublishedSequence uint64
	TimestampQPC      uint64
	Caller            uint64
	DeviceID          uint64
	DeviceObject      uint64
	EndpointObject    uint64
	Generation        uint32
	Line              uint32
	Status            int32
	ActiveOperations  int32
	PendingOperations int32
	QueueState        uint32
	Event             uint16
	Processor         uint16
	Source            uint8
	IRQL              uint8
	EndpointAddress   uint8
}

type LifecycleTrace struct {
	LatestSequence       uint64
	PerformanceFrequency uint64
	Records              []LifecycleTraceRecord
}

func ParseLifecycleTrace(src []byte) (LifecycleTrace, error) {
	h, err := ParseHeader(src)
	if err != nil {
		return LifecycleTrace{}, err
	}
	if h.Size != LifecycleTraceSize || len(src) != LifecycleTraceSize ||
		binary.LittleEndian.Uint32(src[36:40]) != LifecycleTraceRecordSize ||
		binary.LittleEndian.Uint32(src[40:44]) != LifecycleTraceCapacity ||
		binary.LittleEndian.Uint32(src[44:48]) != 0 {
		return LifecycleTrace{}, ErrInvalidSize
	}
	recordCount := binary.LittleEndian.Uint32(src[32:36])
	if recordCount > LifecycleTraceCapacity {
		return LifecycleTrace{}, ErrInvalidRange
	}
	trace := LifecycleTrace{
		LatestSequence:       binary.LittleEndian.Uint64(src[16:24]),
		PerformanceFrequency: binary.LittleEndian.Uint64(src[24:32]),
		Records:              make([]LifecycleTraceRecord, 0, recordCount),
	}
	for index := uint32(0); index < recordCount; index++ {
		offset := 48 + int(index)*LifecycleTraceRecordSize
		record := src[offset : offset+LifecycleTraceRecordSize]
		trace.Records = append(trace.Records, LifecycleTraceRecord{
			PublishedSequence: binary.LittleEndian.Uint64(record[0:8]),
			TimestampQPC:      binary.LittleEndian.Uint64(record[8:16]),
			Caller:            binary.LittleEndian.Uint64(record[16:24]),
			DeviceID:          binary.LittleEndian.Uint64(record[24:32]),
			DeviceObject:      binary.LittleEndian.Uint64(record[32:40]),
			EndpointObject:    binary.LittleEndian.Uint64(record[40:48]),
			Generation:        binary.LittleEndian.Uint32(record[48:52]),
			Line:              binary.LittleEndian.Uint32(record[52:56]),
			Status:            int32(binary.LittleEndian.Uint32(record[56:60])),
			ActiveOperations:  int32(binary.LittleEndian.Uint32(record[60:64])),
			PendingOperations: int32(binary.LittleEndian.Uint32(record[64:68])),
			QueueState:        binary.LittleEndian.Uint32(record[68:72]),
			Event:             binary.LittleEndian.Uint16(record[72:74]),
			Processor:         binary.LittleEndian.Uint16(record[74:76]),
			Source:            record[76],
			IRQL:              record[77],
			EndpointAddress:   record[78],
		})
	}
	return trace, nil
}

func (m Completion) wireLayout() (transferLength uint32, isoBytes int, total int, err error) {
	if m.Token == 0 || m.DeviceID == 0 || m.Generation == 0 {
		return 0, 0, 0, fmt.Errorf("%w: zero completion identity", ErrInvalidRange)
	}
	if len(m.Payload) > MaxTransferBytes || len(m.IsoPackets) > MaxIsoPackets {
		return 0, 0, 0, ErrLimitExceeded
	}
	transferLength = m.TransferLength
	// Non-isochronous IN completions historically infer the completed byte
	// count from their contiguous payload. Isochronous payloads are different:
	// the buffer preserves the host packet offsets, including sparse gaps, while
	// TransferLength is the sum of the packets' actual lengths. In particular,
	// an all-zero ISO completion can legitimately carry a full sparse buffer and
	// still complete zero bytes.
	if transferLength == 0 && len(m.Payload) != 0 && len(m.IsoPackets) == 0 {
		transferLength = uint32(len(m.Payload))
	}
	if transferLength > MaxTransferBytes {
		return 0, 0, 0, ErrLimitExceeded
	}
	isoBytes = len(m.IsoPackets) * IsoPacketSize
	total = CompletionSize + isoBytes + len(m.Payload)
	if _, err = NewHeader(total); err != nil {
		return 0, 0, 0, err
	}
	return transferLength, isoBytes, total, nil
}

func (m Completion) marshalBinaryInto(dst []byte) error {
	transferLength, isoBytes, total, err := m.wireLayout()
	if err != nil {
		return err
	}
	if len(dst) != total {
		return ErrInvalidSize
	}
	h, err := NewHeader(total)
	if err != nil {
		return err
	}
	putHeader(dst, h)
	binary.LittleEndian.PutUint64(dst[16:24], m.Token)
	binary.LittleEndian.PutUint64(dst[24:32], m.DeviceID)
	binary.LittleEndian.PutUint32(dst[32:36], m.Generation)
	binary.LittleEndian.PutUint32(dst[36:40], uint32(m.Status))
	binary.LittleEndian.PutUint32(dst[40:44], m.USBDStatus)
	binary.LittleEndian.PutUint32(dst[44:48], transferLength)
	binary.LittleEndian.PutUint32(dst[48:52], uint32(len(m.IsoPackets)))
	binary.LittleEndian.PutUint32(dst[52:56], uint32(CompletionSize+isoBytes))
	binary.LittleEndian.PutUint32(dst[56:60], uint32(len(m.Payload)))
	binary.LittleEndian.PutUint32(dst[60:64], CompletionSize)
	// CompletionSize includes the C ABI's two explicit reserved words. Fresh
	// allocations make both zero implicitly; caller-owned and pooled buffers
	// must make that wire invariant explicit.
	clear(dst[64:CompletionSize])
	for i, packet := range m.IsoPackets {
		off := CompletionSize + i*IsoPacketSize
		binary.LittleEndian.PutUint32(dst[off:off+4], packet.Offset)
		binary.LittleEndian.PutUint32(dst[off+4:off+8], packet.Length)
		binary.LittleEndian.PutUint32(dst[off+8:off+12], uint32(packet.Status))
		binary.LittleEndian.PutUint32(dst[off+12:off+16], 0)
	}
	copy(dst[CompletionSize+isoBytes:], m.Payload)
	return nil
}

func (m Completion) MarshalBinary() ([]byte, error) {
	_, _, total, err := m.wireLayout()
	if err != nil {
		return nil, err
	}
	dst := make([]byte, total)
	if err := m.marshalBinaryInto(dst); err != nil {
		return nil, err
	}
	return dst, nil
}

func validRange(offset, length, total uint32) bool {
	return offset <= total && length <= total-offset
}

func validArrayRange(offset, count uint32, elementSize uint32, total uint32) bool {
	if count != 0 && elementSize > math.MaxUint32/count {
		return false
	}
	return validRange(offset, count*elementSize, total)
}

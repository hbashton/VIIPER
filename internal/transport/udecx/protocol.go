// Package udecx defines the user-mode half of the native VIIPER UdeCx ABI.
// It intentionally has no Windows dependency so layout and fuzz tests run on
// every supported development host.
package udecx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	Magic    uint32 = 0x45445556
	ABIMajor uint16 = 1
	ABIMinor uint16 = 6

	HeaderSize            = 16
	NegotiateRequestSize  = 32
	NegotiateResponseSize = 56
	DescriptorRecordSize  = 16
	CreateDeviceSize      = 56
	DeviceIdentitySize    = 32
	IsoPacketSize         = 16
	OperationSize         = 96
	CompletionSize        = 72
	InputReportSize       = 48
	StatsSize             = 144

	MaxDevices           = 32
	MaxDescriptorBytes   = 256 * 1024
	MaxTransferBytes     = 1024 * 1024
	MaxIsoPackets        = 1024
	MaxInputReportBytes  = 4096
	MaxPendingOperations = 4096
)

var (
	ErrShortMessage      = errors.New("native UDE message is shorter than its fixed header")
	ErrBadMagic          = errors.New("native UDE message has an invalid magic value")
	ErrIncompatibleMajor = errors.New("native UDE ABI major version is incompatible")
	ErrIncompatibleMinor = errors.New("native UDE ABI minor version is incompatible")
	ErrInvalidSize       = errors.New("native UDE message size is invalid")
	ErrInvalidRange      = errors.New("native UDE message contains an invalid range")
	ErrLimitExceeded     = errors.New("native UDE message exceeds a negotiated limit")
)

type Capabilities uint32

const (
	CapabilityIsochronous Capabilities = 1 << iota
	CapabilityStreams
	CapabilityDeviceLifecycle
	CapabilityInputReports
)

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
}

func ParseNegotiateResponse(src []byte) (NegotiateResponse, error) {
	h, err := ParseHeader(src)
	if err != nil {
		return NegotiateResponse{}, err
	}
	if h.Size != NegotiateResponseSize {
		return NegotiateResponse{}, ErrInvalidSize
	}
	return NegotiateResponse{
		ClientNonce:          binary.LittleEndian.Uint64(src[16:24]),
		DriverNonce:          binary.LittleEndian.Uint64(src[24:32]),
		Capabilities:         Capabilities(binary.LittleEndian.Uint32(src[32:36])),
		MaxDevices:           binary.LittleEndian.Uint32(src[36:40]),
		MaxDescriptorBytes:   binary.LittleEndian.Uint32(src[40:44]),
		MaxTransferBytes:     binary.LittleEndian.Uint32(src[44:48]),
		MaxIsoPackets:        binary.LittleEndian.Uint32(src[48:52]),
		MaxPendingOperations: binary.LittleEndian.Uint32(src[52:56]),
	}, nil
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
}

func ParseOperation(src []byte) (Operation, error) {
	h, err := ParseHeader(src)
	if err != nil {
		return Operation{}, err
	}
	if h.Size < OperationSize || h.Size > MaxTransferBytes+OperationSize+MaxIsoPackets*IsoPacketSize {
		return Operation{}, ErrInvalidSize
	}
	src = src[:h.Size]
	packetCount := binary.LittleEndian.Uint32(src[56:60])
	transferLength := binary.LittleEndian.Uint32(src[60:64])
	payloadOffset := binary.LittleEndian.Uint32(src[64:68])
	payloadLength := binary.LittleEndian.Uint32(src[68:72])
	isoOffset := binary.LittleEndian.Uint32(src[72:76])
	if packetCount > MaxIsoPackets || transferLength > MaxTransferBytes || payloadLength > MaxTransferBytes {
		return Operation{}, ErrLimitExceeded
	}
	if !validRange(payloadOffset, payloadLength, h.Size) || !validArrayRange(isoOffset, packetCount, IsoPacketSize, h.Size) {
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
		IsoPackets:            make([]IsoPacket, int(packetCount)),
		Payload:               append([]byte(nil), src[payloadOffset:payloadOffset+payloadLength]...),
	}
	copy(op.SetupPacket[:], src[76:84])
	for i := range op.IsoPackets {
		off := int(isoOffset) + i*IsoPacketSize
		op.IsoPackets[i] = IsoPacket{
			Offset: binary.LittleEndian.Uint32(src[off : off+4]),
			Length: binary.LittleEndian.Uint32(src[off+4 : off+8]),
			Status: int32(binary.LittleEndian.Uint32(src[off+8 : off+12])),
		}
	}
	return op, nil
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

func (m Completion) MarshalBinary() ([]byte, error) {
	if m.Token == 0 || m.DeviceID == 0 || m.Generation == 0 {
		return nil, fmt.Errorf("%w: zero completion identity", ErrInvalidRange)
	}
	if len(m.Payload) > MaxTransferBytes || len(m.IsoPackets) > MaxIsoPackets {
		return nil, ErrLimitExceeded
	}
	transferLength := m.TransferLength
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
		return nil, ErrLimitExceeded
	}
	isoBytes := len(m.IsoPackets) * IsoPacketSize
	total := CompletionSize + isoBytes + len(m.Payload)
	h, err := NewHeader(total)
	if err != nil {
		return nil, err
	}
	dst := make([]byte, total)
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
	for i, packet := range m.IsoPackets {
		off := CompletionSize + i*IsoPacketSize
		binary.LittleEndian.PutUint32(dst[off:off+4], packet.Offset)
		binary.LittleEndian.PutUint32(dst[off+4:off+8], packet.Length)
		binary.LittleEndian.PutUint32(dst[off+8:off+12], uint32(packet.Status))
	}
	copy(dst[CompletionSize+isoBytes:], m.Payload)
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

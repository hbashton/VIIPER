package udecx

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The contract mirrors are never serialized with unsafe. They give every
// development host a mechanical view of the packed C layout without requiring
// MSVC, a WDK, or a runnable Windows driver.
type contractHeader struct {
	Magic uint32
	Major uint16
	Minor uint16
	Size  uint32
	Flags uint32
}

type contractNegotiateRequest struct {
	Header                contractHeader
	ClientNonce           uint64
	RequestedCapabilities uint32
	Reserved              uint32
}

type contractNegotiateResponse struct {
	Header               contractHeader
	ClientNonce          uint64
	DriverNonce          uint64
	Capabilities         uint32
	MaxDevices           uint32
	MaxDescriptorBytes   uint32
	MaxTransferBytes     uint32
	MaxIsoPackets        uint32
	MaxPendingOperations uint32
	BuildIdentity        [BuildIdentitySize]uint8
}

type contractDescriptorRecord struct {
	Kind       uint16
	Index      uint16
	LanguageId uint16
	Reserved   uint16
	Offset     uint32
	Length     uint32
}

type contractCreateDevice struct {
	Header                  contractHeader
	DeviceId                uint64
	Generation              uint32
	Speed                   uint32
	DescriptorCount         uint32
	DescriptorRecordsOffset uint32
	DescriptorDataOffset    uint32
	DescriptorDataLength    uint32
	MaxPendingOperations    uint32
	Reserved                uint32
}

type contractCreateDeviceResult struct {
	Header          contractHeader
	DeviceId        uint64
	Generation      uint32
	Speed           uint32
	Usb20PortNumber uint32
	Usb30PortNumber uint32
}

type contractDeviceIdentity struct {
	Header     contractHeader
	DeviceId   uint64
	Generation uint32
	Reserved   uint32
}

type contractISOPacket struct {
	Offset   uint32
	Length   uint32
	Status   int32
	Reserved uint32
}

type contractOperation struct {
	Header                contractHeader
	Token                 uint64
	DeviceId              uint64
	Generation            uint32
	Kind                  uint32
	EndpointAddress       uint8
	Direction             uint8
	InterfaceNumber       uint8
	InterfaceSetting      uint8
	UrbFunction           uint32
	TransferFlags         uint32
	StartFrame            uint32
	IsoPacketCount        uint32
	TransferLength        uint32
	PayloadOffset         uint32
	PayloadLength         uint32
	IsoPacketsOffset      uint32
	SetupPacket           [8]uint8
	EndpointAttributes    uint8
	EndpointInterval      uint8
	EndpointMaxPacketSize uint16
	EndpointSequence      uint64
	DeviceSequence        uint64
	EndpointGeneration    uint32
}

type contractCompletion struct {
	Header             contractHeader
	Token              uint64
	DeviceId           uint64
	Generation         uint32
	Status             int32
	UsbdStatus         uint32
	TransferLength     uint32
	IsoPacketCount     uint32
	PayloadOffset      uint32
	PayloadLength      uint32
	IsoPacketsOffset   uint32
	EndpointGeneration uint32
	Reserved           uint32
}

type contractInputReport struct {
	Header             contractHeader
	DeviceId           uint64
	Generation         uint32
	EndpointAddress    uint8
	Flags              uint8
	Reserved1          [2]uint8
	PayloadOffset      uint32
	PayloadLength      uint32
	Sequence           uint64
	EndpointGeneration uint32
}

type contractStats struct {
	Header                     contractHeader
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
	ReservedPorts              uint32
	Reserved                   uint32
}

type contractLifecycleTraceRecord struct {
	PublishedSequence uint64
	TimestampQpc      uint64
	Caller            uint64
	DeviceId          uint64
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
	Irql              uint8
	EndpointAddress   uint8
	Reserved          uint8
}

type contractLifecycleTrace struct {
	Header               contractHeader
	LatestSequence       uint64
	PerformanceFrequency uint64
	RecordCount          uint32
	RecordSize           uint32
	Capacity             uint32
	StatusFlags          uint32
	Records              [LifecycleTraceCapacity]contractLifecycleTraceRecord
}

func protocolHeaderSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "native", "udecx", "include", "ViiperUdeProtocol.h")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read native ABI header: %v", err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

func packedContractSize(contract reflect.Type) uintptr {
	var size uintptr
	for index := 0; index < contract.NumField(); index++ {
		size += contract.Field(index).Type.Size()
	}
	return size
}

func packedContractFieldOffset(contract reflect.Type, name string) (uintptr, bool) {
	var offset uintptr
	for index := 0; index < contract.NumField(); index++ {
		field := contract.Field(index)
		if field.Name == name {
			return offset, true
		}
		offset += field.Type.Size()
	}
	return 0, false
}

func cDefineNumber(t *testing.T, source, name string) uint64 {
	t.Helper()
	pattern := `(?m)^#define\s+` + regexp.QuoteMeta(name) +
		`\s+(?:VIIPER_UDE_UINT(?:16|32)_C\()?((?:0x)?[0-9A-Fa-f]+)\)?(?:\s|$)`
	match := regexp.MustCompile(pattern).FindStringSubmatch(source)
	if match == nil {
		t.Fatalf("C ABI does not define %s", name)
	}
	value, err := strconv.ParseUint(match[1], 0, 64)
	if err != nil {
		t.Fatalf("parse C ABI %s=%q: %v", name, match[1], err)
	}
	return value
}

func TestNativeProtocolHeaderMatchesGoContract(t *testing.T) {
	header := protocolHeaderSource(t)

	numbers := map[string]uint64{
		"VIIPER_UDE_MAGIC":                       uint64(Magic),
		"VIIPER_UDE_ABI_MAJOR":                   uint64(ABIMajor),
		"VIIPER_UDE_ABI_MINOR":                   uint64(ABIMinor),
		"VIIPER_UDE_BUILD_IDENTITY_BYTES":        BuildIdentitySize,
		"VIIPER_UDE_MAX_DEVICES":                 MaxDevices,
		"VIIPER_UDE_MAX_DESCRIPTOR_BYTES":        MaxDescriptorBytes,
		"VIIPER_UDE_MAX_TRANSFER_BYTES":          MaxTransferBytes,
		"VIIPER_UDE_MAX_ISO_PACKETS":             MaxIsoPackets,
		"VIIPER_UDE_MAX_INPUT_REPORT_BYTES":      MaxInputReportBytes,
		"VIIPER_UDE_MAX_PENDING_OPERATIONS":      MaxPendingOperations,
		"VIIPER_UDE_MANAGEMENT_SLOT_FLAG":        uint64(ManagementSlotFlag),
		"VIIPER_UDE_INPUT_REPORT_TRANSITION":     uint64(InputReportTransition),
		"VIIPER_UDE_MS_OS_10_STRING_INDEX":       uint64(MicrosoftOS10StringIndex),
		"VIIPER_UDE_MS_OS_10_STRING_LENGTH":      MicrosoftOS10StringLength,
		"VIIPER_UDE_MS_OS_10_VENDOR_CODE_OFFSET": MicrosoftOS10VendorCodeOffset,
		"VIIPER_UDE_CAP_ISOCHRONOUS":             uint64(CapabilityIsochronous),
		"VIIPER_UDE_CAP_STREAMS":                 uint64(CapabilityStreams),
		"VIIPER_UDE_CAP_DEVICE_LIFECYCLE":        uint64(CapabilityDeviceLifecycle),
		"VIIPER_UDE_CAP_INPUT_REPORTS":           uint64(CapabilityInputReports),
		"VIIPER_UDE_CAP_LIFECYCLE_TRACE":         uint64(CapabilityLifecycleTrace),
		"VIIPER_UDE_CAP_DEVICE_CORRELATION":      uint64(CapabilityDeviceCorrelation),
		"VIIPER_UDE_LIFECYCLE_TRACE_CAPACITY":    LifecycleTraceCapacity,
	}
	for name, want := range numbers {
		if got := cDefineNumber(t, header, name); got != want {
			t.Errorf("%s=%#x want Go %#x", name, got, want)
		}
	}

	types := map[string]reflect.Type{
		"HEADER":                 reflect.TypeOf(contractHeader{}),
		"NEGOTIATE_REQUEST":      reflect.TypeOf(contractNegotiateRequest{}),
		"NEGOTIATE_RESPONSE":     reflect.TypeOf(contractNegotiateResponse{}),
		"DESCRIPTOR_RECORD":      reflect.TypeOf(contractDescriptorRecord{}),
		"CREATE_DEVICE":          reflect.TypeOf(contractCreateDevice{}),
		"CREATE_DEVICE_RESULT":   reflect.TypeOf(contractCreateDeviceResult{}),
		"DEVICE_IDENTITY":        reflect.TypeOf(contractDeviceIdentity{}),
		"ISO_PACKET":             reflect.TypeOf(contractISOPacket{}),
		"OPERATION":              reflect.TypeOf(contractOperation{}),
		"COMPLETION":             reflect.TypeOf(contractCompletion{}),
		"INPUT_REPORT":           reflect.TypeOf(contractInputReport{}),
		"STATS":                  reflect.TypeOf(contractStats{}),
		"LIFECYCLE_TRACE_RECORD": reflect.TypeOf(contractLifecycleTraceRecord{}),
		"LIFECYCLE_TRACE":        reflect.TypeOf(contractLifecycleTrace{}),
	}
	wantSizes := map[string]uintptr{
		"HEADER": HeaderSize, "NEGOTIATE_REQUEST": NegotiateRequestSize,
		"NEGOTIATE_RESPONSE": NegotiateResponseSize, "DESCRIPTOR_RECORD": DescriptorRecordSize,
		"CREATE_DEVICE": CreateDeviceSize, "CREATE_DEVICE_RESULT": CreateDeviceResultSize,
		"DEVICE_IDENTITY": DeviceIdentitySize, "ISO_PACKET": IsoPacketSize,
		"OPERATION": OperationSize, "COMPLETION": CompletionSize,
		"INPUT_REPORT": InputReportSize, "STATS": StatsSize,
		"LIFECYCLE_TRACE_RECORD": LifecycleTraceRecordSize,
		"LIFECYCLE_TRACE":        LifecycleTraceSize,
	}
	sizePattern := regexp.MustCompile(
		`static_assert\(sizeof\(VIIPER_UDE_([A-Z_]+)\) == ([0-9]+),`,
	)
	seenSizes := make(map[string]bool)
	for _, match := range sizePattern.FindAllStringSubmatch(header, -1) {
		name := match[1]
		wireType, ok := types[name]
		if !ok {
			t.Fatalf("C ABI added unmodeled type VIIPER_UDE_%s", name)
		}
		declared, _ := strconv.ParseUint(match[2], 10, 64)
		got := packedContractSize(wireType)
		if uint64(got) != declared || got != wantSizes[name] {
			t.Errorf("VIIPER_UDE_%s size: C=%d Go=%d constant=%d",
				name, declared, got, wantSizes[name])
		}
		seenSizes[name] = true
	}
	if len(seenSizes) != len(types) {
		t.Fatalf("C size contracts found=%d want=%d", len(seenSizes), len(types))
	}

	offsetPattern := regexp.MustCompile(
		`VIIPER_UDE_ASSERT_OFFSET\(VIIPER_UDE_([A-Z_]+),\s*([A-Za-z0-9_]+),\s*([0-9]+)\);`,
	)
	seenOffsets := make(map[string]bool)
	for _, match := range offsetPattern.FindAllStringSubmatch(header, -1) {
		wireType, ok := types[match[1]]
		if !ok {
			t.Fatalf("C ABI added offsets for unmodeled type VIIPER_UDE_%s", match[1])
		}
		fieldOffset, ok := packedContractFieldOffset(wireType, match[2])
		if !ok {
			t.Fatalf("Go contract type %s has no field %s", match[1], match[2])
		}
		want, _ := strconv.ParseUint(match[3], 10, 64)
		if uint64(fieldOffset) != want {
			t.Errorf("VIIPER_UDE_%s.%s offset: C=%d Go=%d",
				match[1], match[2], want, fieldOffset)
		}
		key := match[1] + "." + match[2]
		if seenOffsets[key] {
			t.Fatalf("duplicate C offset contract %s", key)
		}
		seenOffsets[key] = true
	}
	var modeledFields int
	for name, wireType := range types {
		for fieldIndex := 0; fieldIndex < wireType.NumField(); fieldIndex++ {
			// Every message starts with the already-pinned HEADER layout. The C
			// header asserts it once rather than repeating Header at offset zero
			// for every enclosing message.
			if name != "HEADER" && wireType.Field(fieldIndex).Name == "Header" {
				continue
			}
			modeledFields++
		}
	}
	if len(seenOffsets) != modeledFields {
		t.Fatalf("C offset contracts found=%d want every modeled field=%d",
			len(seenOffsets), modeledFields)
	}

	enums := map[string]uint64{
		"ViiperUdeDescriptorDevice":        uint64(DescriptorDevice),
		"ViiperUdeDescriptorConfiguration": uint64(DescriptorConfiguration),
		"ViiperUdeDescriptorBos":           uint64(DescriptorBOS),
		"ViiperUdeDescriptorString":        uint64(DescriptorString),
		"ViiperUdeOperationControl":        uint64(OperationControl),
		"ViiperUdeOperationTransfer":       uint64(OperationTransfer),
		"ViiperUdeOperationEndpointStart":  uint64(OperationEndpointStart),
		"ViiperUdeOperationEndpointPurge":  uint64(OperationEndpointPurge),
		"ViiperUdeOperationEndpointReset":  uint64(OperationEndpointReset),
		"ViiperUdeOperationDeviceReset":    uint64(OperationDeviceReset),
		"ViiperUdeOperationSetInterface":   uint64(OperationSetInterface),
		"ViiperUdeOperationDeviceD0Entry":  uint64(OperationDeviceD0Entry),
		"ViiperUdeOperationDeviceD0Exit":   uint64(OperationDeviceD0Exit),
		"ViiperUdeOperationCancel":         uint64(OperationCancel),
		"ViiperUdeOperationBrokerFault":    uint64(OperationBrokerFault),
	}
	enumPattern := regexp.MustCompile(
		`(?m)^\s*(ViiperUde[A-Za-z0-9]+)\s*=\s*([0-9]+)[,\s]`,
	)
	seenEnums := make(map[string]bool)
	for _, match := range enumPattern.FindAllStringSubmatch(header, -1) {
		want, ok := enums[match[1]]
		if !ok {
			t.Fatalf("C ABI added unmodeled enum %s", match[1])
		}
		got, _ := strconv.ParseUint(match[2], 10, 64)
		if got != want {
			t.Errorf("%s=%d want Go %d", match[1], got, want)
		}
		seenEnums[match[1]] = true
	}
	if len(seenEnums) != len(enums) {
		t.Fatalf("C enum contracts found=%d want=%d", len(seenEnums), len(enums))
	}

	verifyGUIDAndIOCTLContract(t, header)
	if !strings.Contains(
		header,
		`#define VIIPER_UDE_DRIVER_PACKAGE_VERSION "`+DriverPackageVersion+`"`,
	) {
		t.Fatalf("C driver package version does not match Go %q", DriverPackageVersion)
	}
	advertised := regexp.MustCompile(
		`(?s)#define\s+VIIPER_UDE_ADVERTISED_CAPABILITIES\s+\\\s*\(VIIPER_UDE_CAP_ISOCHRONOUS\s*\|\s*VIIPER_UDE_CAP_DEVICE_LIFECYCLE\s*\|\s*\\?\s*VIIPER_UDE_CAP_INPUT_REPORTS\s*\|\s*VIIPER_UDE_CAP_LIFECYCLE_TRACE\s*\|\s*\\?\s*VIIPER_UDE_CAP_DEVICE_CORRELATION\)`,
	).MatchString(header)
	if !advertised {
		t.Fatal("C advertised capability identity tuple does not match Go")
	}
}

func verifyGUIDAndIOCTLContract(t *testing.T, header string) {
	t.Helper()
	guidNames := []string{
		"VIIPER_UDE_INTERFACE_GUID_DATA1", "VIIPER_UDE_INTERFACE_GUID_DATA2",
		"VIIPER_UDE_INTERFACE_GUID_DATA3", "VIIPER_UDE_INTERFACE_GUID_DATA4_0",
		"VIIPER_UDE_INTERFACE_GUID_DATA4_1", "VIIPER_UDE_INTERFACE_GUID_DATA4_2",
		"VIIPER_UDE_INTERFACE_GUID_DATA4_3", "VIIPER_UDE_INTERFACE_GUID_DATA4_4",
		"VIIPER_UDE_INTERFACE_GUID_DATA4_5", "VIIPER_UDE_INTERFACE_GUID_DATA4_6",
		"VIIPER_UDE_INTERFACE_GUID_DATA4_7",
	}
	wantGUID := []uint64{
		0x32d03f48, 0x725b, 0x4baa, 0x97, 0x0f, 0x7f, 0x5d, 0xe6, 0xc4, 0x46, 0x87,
	}
	for index, name := range guidNames {
		if got := cDefineNumber(t, header, name); got != wantGUID[index] {
			t.Errorf("%s=%#x want %#x", name, got, wantGUID[index])
		}
	}
	if ControllerInterfaceGUID != "{32d03f48-725b-4baa-970f-7f5de6c44687}" {
		t.Fatalf("Go controller interface GUID=%q", ControllerInterfaceGUID)
	}

	type ioctlSpec struct {
		offset uint64
		method string
		access string
		value  uint32
	}
	specs := map[string]ioctlSpec{
		"NEGOTIATE":             {0, "METHOD_BUFFERED", "FILE_READ_DATA | FILE_WRITE_DATA", IOCTLNegotiate},
		"CREATE_DEVICE":         {1, "METHOD_BUFFERED", "FILE_READ_DATA | FILE_WRITE_DATA", IOCTLCreateDevice},
		"DESTROY_DEVICE":        {2, "METHOD_BUFFERED", "FILE_READ_DATA | FILE_WRITE_DATA", IOCTLDestroyDevice},
		"DEQUEUE_OPERATION":     {3, "METHOD_OUT_DIRECT", "FILE_READ_DATA | FILE_WRITE_DATA", IOCTLDequeueOperation},
		"COMPLETE_OPERATION":    {4, "METHOD_IN_DIRECT", "FILE_READ_DATA | FILE_WRITE_DATA", IOCTLCompleteOperation},
		"QUERY_STATS":           {5, "METHOD_BUFFERED", "FILE_READ_DATA", IOCTLQueryStats},
		"SUBMIT_INPUT_REPORT":   {6, "METHOD_IN_DIRECT", "FILE_READ_DATA | FILE_WRITE_DATA", IOCTLSubmitInputReport},
		"QUERY_LIFECYCLE_TRACE": {7, "METHOD_BUFFERED", "FILE_READ_DATA", IOCTLQueryLifecycleTrace},
	}
	pattern := regexp.MustCompile(
		`(?m)^#define IOCTL_VIIPER_UDE_([A-Z_]+) CTL_CODE\(FILE_DEVICE_UNKNOWN, VIIPER_UDE_IOCTL_BASE \+ ([0-9]+), (METHOD_[A-Z_]+), ([^)]+)\)$`,
	)
	methods := map[string]uint64{
		"METHOD_BUFFERED":   0,
		"METHOD_IN_DIRECT":  1,
		"METHOD_OUT_DIRECT": 2,
	}
	seen := make(map[string]bool)
	for _, match := range pattern.FindAllStringSubmatch(header, -1) {
		spec, ok := specs[match[1]]
		if !ok {
			t.Fatalf("C ABI added unmodeled IOCTL %s", match[1])
		}
		offset, _ := strconv.ParseUint(match[2], 10, 64)
		accessText := strings.Join(strings.Fields(match[4]), " ")
		if offset != spec.offset || match[3] != spec.method || accessText != spec.access {
			t.Errorf("IOCTL %s C definition=(%d,%s,%s) want=(%d,%s,%s)",
				match[1], offset, match[3], accessText,
				spec.offset, spec.method, spec.access)
		}
		access := uint64(fileReadData | fileWriteData)
		if spec.access == "FILE_READ_DATA" {
			access = uint64(fileReadData)
		}
		computed := uint64(fileDeviceUnknown)<<16 | access<<14 |
			(uint64(IOCTLBase)+offset)<<2 | methods[spec.method]
		if uint64(spec.value) != computed {
			t.Errorf("IOCTL %s Go=%#x want CTL_CODE %#x",
				match[1], spec.value, computed)
		}
		seen[match[1]] = true
	}
	if len(seen) != len(specs) {
		t.Fatalf("C IOCTL contracts found=%d want=%d", len(seen), len(specs))
	}
}

func TestNegotiationRequestGoldenWireImage(t *testing.T) {
	request, err := (NegotiateRequest{
		ClientNonce:           0x1122334455667788,
		RequestedCapabilities: AdvertisedCapabilities,
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x56, 0x55, 0x44, 0x45, 0x01, 0x00, 0x0e, 0x00,
		0x20, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11,
		0x3d, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	if !reflect.DeepEqual(request, want) {
		t.Fatalf("negotiation request=\n%x\nwant=\n%x", request, want)
	}
}

func validNegotiationFixture(t *testing.T) ([]byte, [BuildIdentitySize]byte) {
	t.Helper()
	identity, err := DeriveBuildIdentity(
		"0123456789abcdef0123456789abcdef01234567",
		DriverPackageVersion,
		ABIMajor,
		ABIMinor,
		AdvertisedCapabilities,
	)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, NegotiateResponseSize)
	header, err := NewHeader(NegotiateResponseSize)
	if err != nil {
		t.Fatal(err)
	}
	putHeader(raw, header)
	binary.LittleEndian.PutUint64(raw[16:24], 0x1122334455667788)
	binary.LittleEndian.PutUint64(raw[24:32], 0x8877665544332211)
	binary.LittleEndian.PutUint32(raw[32:36], uint32(AdvertisedCapabilities))
	binary.LittleEndian.PutUint32(raw[36:40], MaxDevices)
	binary.LittleEndian.PutUint32(raw[40:44], MaxDescriptorBytes)
	binary.LittleEndian.PutUint32(raw[44:48], MaxTransferBytes)
	binary.LittleEndian.PutUint32(raw[48:52], MaxIsoPackets)
	binary.LittleEndian.PutUint32(raw[52:56], MaxPendingOperations)
	copy(raw[56:88], identity[:])
	return raw, identity
}

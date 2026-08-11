package udecx

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Alia5/VIIPER/usb"
)

// These mirrors are never serialized through unsafe. They let CI prove that
// Go's view of every packed native field size and offset still matches the C
// header that the KMDF driver compiles.
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
}

type contractCompletion struct {
	Header           contractHeader
	Token            uint64
	DeviceId         uint64
	Generation       uint32
	Status           int32
	UsbdStatus       uint32
	TransferLength   uint32
	IsoPacketCount   uint32
	PayloadOffset    uint32
	PayloadLength    uint32
	IsoPacketsOffset uint32
	Reserved         [2]uint32
}

type contractInputReport struct {
	Header          contractHeader
	DeviceId        uint64
	Generation      uint32
	EndpointAddress uint8
	Reserved1       [3]uint8
	PayloadOffset   uint32
	PayloadLength   uint32
	Sequence        uint64
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
}

func nativeContractSource(t *testing.T, name ...string) string {
	t.Helper()
	parts := append([]string{"..", "..", ".."}, name...)
	raw, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read native contract source: %v", err)
	}
	return normalizeNativeContractSource(string(raw))
}

func normalizeNativeContractSource(source string) string {
	return strings.ReplaceAll(source, "\r\n", "\n")
}

func TestNormalizeNativeContractSource(t *testing.T) {
	t.Parallel()

	const windowsHeader = "#define FIRST 1\r\n#define SECOND 2\r\n"
	const normalizedHeader = "#define FIRST 1\n#define SECOND 2\n"
	if got := normalizeNativeContractSource(windowsHeader); got != normalizedHeader {
		t.Fatalf("normalized native source = %q, want %q", got, normalizedHeader)
	}
}

func cDefineNumber(t *testing.T, source, name string) uint64 {
	t.Helper()
	pattern := `(?m)^#define\s+` + regexp.QuoteMeta(name) +
		`\s+(?:VIIPER_UDE_UINT(?:16|32)_C\()?((?:0x)?[0-9A-Fa-f]+)\)?(?:\s|$)`
	match := regexp.MustCompile(pattern).FindStringSubmatch(source)
	if match == nil {
		t.Fatalf("C contract does not define %s", name)
	}
	value, err := strconv.ParseUint(match[1], 0, 64)
	if err != nil {
		t.Fatalf("parse C contract %s=%q: %v", name, match[1], err)
	}
	return value
}

func TestNativeProtocolHeaderMatchesGoContract(t *testing.T) {
	header := nativeContractSource(t, "native", "udecx", "include", "ViiperUdeProtocol.h")

	numbers := map[string]uint64{
		"VIIPER_UDE_MAGIC":                       uint64(Magic),
		"VIIPER_UDE_ABI_MAJOR":                   uint64(ABIMajor),
		"VIIPER_UDE_ABI_MINOR":                   uint64(ABIMinor),
		"VIIPER_UDE_MAX_DEVICES":                 MaxDevices,
		"VIIPER_UDE_MAX_DESCRIPTOR_BYTES":        MaxDescriptorBytes,
		"VIIPER_UDE_MAX_TRANSFER_BYTES":          MaxTransferBytes,
		"VIIPER_UDE_MAX_ISO_PACKETS":             MaxIsoPackets,
		"VIIPER_UDE_MAX_INPUT_REPORT_BYTES":      MaxInputReportBytes,
		"VIIPER_UDE_MAX_PENDING_OPERATIONS":      MaxPendingOperations,
		"VIIPER_UDE_MS_OS_10_STRING_INDEX":       uint64(MicrosoftOS10StringIndex),
		"VIIPER_UDE_MS_OS_10_STRING_LENGTH":      MicrosoftOS10StringLength,
		"VIIPER_UDE_MS_OS_10_VENDOR_CODE_OFFSET": MicrosoftOS10VendorCodeOffset,
		"VIIPER_UDE_CAP_ISOCHRONOUS":             uint64(CapabilityIsochronous),
		"VIIPER_UDE_CAP_STREAMS":                 uint64(CapabilityStreams),
		"VIIPER_UDE_CAP_DEVICE_LIFECYCLE":        uint64(CapabilityDeviceLifecycle),
		"VIIPER_UDE_CAP_INPUT_REPORTS":           uint64(CapabilityInputReports),
	}
	for name, want := range numbers {
		if got := cDefineNumber(t, header, name); got != want {
			t.Errorf("%s=%#x want Go %#x", name, got, want)
		}
	}

	types := map[string]reflect.Type{
		"HEADER":             reflect.TypeOf(contractHeader{}),
		"NEGOTIATE_REQUEST":  reflect.TypeOf(contractNegotiateRequest{}),
		"NEGOTIATE_RESPONSE": reflect.TypeOf(contractNegotiateResponse{}),
		"DESCRIPTOR_RECORD":  reflect.TypeOf(contractDescriptorRecord{}),
		"CREATE_DEVICE":      reflect.TypeOf(contractCreateDevice{}),
		"DEVICE_IDENTITY":    reflect.TypeOf(contractDeviceIdentity{}),
		"ISO_PACKET":         reflect.TypeOf(contractISOPacket{}),
		"OPERATION":          reflect.TypeOf(contractOperation{}),
		"COMPLETION":         reflect.TypeOf(contractCompletion{}),
		"INPUT_REPORT":       reflect.TypeOf(contractInputReport{}),
		"STATS":              reflect.TypeOf(contractStats{}),
	}
	wantSizes := map[string]uintptr{
		"HEADER": HeaderSize, "NEGOTIATE_REQUEST": NegotiateRequestSize,
		"NEGOTIATE_RESPONSE": NegotiateResponseSize, "DESCRIPTOR_RECORD": DescriptorRecordSize,
		"CREATE_DEVICE": CreateDeviceSize, "DEVICE_IDENTITY": DeviceIdentitySize,
		"ISO_PACKET": IsoPacketSize, "OPERATION": OperationSize, "COMPLETION": CompletionSize,
		"INPUT_REPORT": InputReportSize, "STATS": StatsSize,
	}
	sizePattern := regexp.MustCompile(`static_assert\(sizeof\(VIIPER_UDE_([A-Z_]+)\) == ([0-9]+),`)
	seenSizes := make(map[string]bool)
	for _, match := range sizePattern.FindAllStringSubmatch(header, -1) {
		name := match[1]
		wireType, ok := types[name]
		if !ok {
			t.Fatalf("C contract added unmodeled type VIIPER_UDE_%s", name)
		}
		declared, _ := strconv.ParseUint(match[2], 10, 64)
		if got := wireType.Size(); uint64(got) != declared || got != wantSizes[name] {
			t.Errorf("VIIPER_UDE_%s size: C=%d Go=%d contract=%d", name, declared, got, wantSizes[name])
		}
		seenSizes[name] = true
	}
	if len(seenSizes) != len(types) {
		t.Fatalf("C size contracts found=%d want=%d", len(seenSizes), len(types))
	}

	offsetPattern := regexp.MustCompile(`VIIPER_UDE_ASSERT_OFFSET\(VIIPER_UDE_([A-Z_]+),\s*([A-Za-z0-9_]+),\s*([0-9]+)\);`)
	seenOffsets := 0
	for _, match := range offsetPattern.FindAllStringSubmatch(header, -1) {
		wireType, ok := types[match[1]]
		if !ok {
			t.Fatalf("C contract added offsets for unmodeled type VIIPER_UDE_%s", match[1])
		}
		field, ok := wireType.FieldByName(match[2])
		if !ok {
			t.Fatalf("Go contract type %s has no field %s", match[1], match[2])
		}
		want, _ := strconv.ParseUint(match[3], 10, 64)
		if uint64(field.Offset) != want {
			t.Errorf("VIIPER_UDE_%s.%s offset: C=%d Go=%d", match[1], match[2], want, field.Offset)
		}
		seenOffsets++
	}
	if seenOffsets == 0 {
		t.Fatal("C field-offset contracts were not found")
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
	enumPattern := regexp.MustCompile(`(?m)^\s*(ViiperUde[A-Za-z0-9]+)\s*=\s*([0-9]+)[,\s]`)
	seenEnums := make(map[string]bool)
	for _, match := range enumPattern.FindAllStringSubmatch(header, -1) {
		want, ok := enums[match[1]]
		if !ok {
			t.Fatalf("C contract added unmodeled enum %s", match[1])
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
}

func TestKernelMicrosoftOS10StringExceptionMatchesGoContract(t *testing.T) {
	driver := nativeContractSource(t, "native", "udecx", "driver", "Device.c")
	prefixMatch := regexp.MustCompile(
		`(?s)static const UCHAR microsoftOS10StringPrefix\[\]\s*=\s*\{([^}]*)\};`,
	).FindStringSubmatch(driver)
	if prefixMatch == nil {
		t.Fatal("kernel Microsoft OS 1.0 string prefix is missing")
	}
	var prefix []byte
	for _, token := range regexp.MustCompile(`0x([0-9A-Fa-f]{2})`).FindAllStringSubmatch(prefixMatch[1], -1) {
		value, err := strconv.ParseUint(token[1], 16, 8)
		if err != nil {
			t.Fatalf("parse kernel Microsoft OS 1.0 prefix byte %q: %v", token[1], err)
		}
		prefix = append(prefix, byte(value))
	}
	want := (usb.MicrosoftOS10Descriptor{VendorCode: 0x20}).StringDescriptor()
	if len(want) != MicrosoftOS10StringLength ||
		MicrosoftOS10VendorCodeOffset != len(want)-2 ||
		!bytes.Equal(prefix, want[:MicrosoftOS10VendorCodeOffset]) {
		t.Fatalf("kernel Microsoft OS 1.0 prefix=%x want=%x", prefix, want[:MicrosoftOS10VendorCodeOffset])
	}

	// Keep the exception exact: only the reserved index, LANGID zero, the
	// canonical MSFT100 descriptor, a usable vendor code, and a zero pad pass.
	// The final check also proves all other nonzero-index/LANGID-zero strings
	// still take the original rejection path.
	normalized := strings.Join(strings.Fields(driver), " ")
	checks := []string{
		"Record->Index == VIIPER_UDE_MS_OS_10_STRING_INDEX",
		"Record->LanguageId == 0",
		"Record->Length == VIIPER_UDE_MS_OS_10_STRING_LENGTH",
		"Descriptor[VIIPER_UDE_MS_OS_10_VENDOR_CODE_OFFSET] != 0",
		"Descriptor[VIIPER_UDE_MS_OS_10_STRING_LENGTH - 1] == 0",
		"record->Index != 0 && record->LanguageId == 0 && !isMicrosoftOS10String",
		"if (foundMicrosoftOS10String) { return FALSE; }",
	}
	for _, check := range checks {
		if !strings.Contains(normalized, check) {
			t.Errorf("kernel Microsoft OS 1.0 validation lost contract %q", check)
		}
	}
}

func evalGoInteger(expr ast.Expr, values map[string]uint64) (uint64, bool) {
	switch value := expr.(type) {
	case *ast.BasicLit:
		parsed, err := strconv.ParseUint(value.Value, 0, 64)
		return parsed, err == nil
	case *ast.Ident:
		parsed, ok := values[value.Name]
		return parsed, ok
	case *ast.ParenExpr:
		return evalGoInteger(value.X, values)
	case *ast.BinaryExpr:
		left, leftOK := evalGoInteger(value.X, values)
		right, rightOK := evalGoInteger(value.Y, values)
		if !leftOK || !rightOK {
			return 0, false
		}
		switch value.Op {
		case token.ADD:
			return left + right, true
		case token.OR:
			return left | right, true
		case token.SHL:
			return left << right, true
		}
	}
	return 0, false
}

func goWindowsContract(t *testing.T) (map[string]uint64, [11]uint64) {
	t.Helper()
	sourcePath := filepath.Join("client_windows.go")
	file, err := parser.ParseFile(token.NewFileSet(), sourcePath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", sourcePath, err)
	}
	values := make(map[string]uint64)
	var guid [11]uint64
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, rawSpec := range general.Specs {
			spec, ok := rawSpec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range spec.Names {
				if index < len(spec.Values) {
					if value, ok := evalGoInteger(spec.Values[index], values); ok {
						values[name.Name] = value
					}
				}
				if name.Name != "interfaceGUID" || len(spec.Values) == 0 {
					continue
				}
				literal, ok := spec.Values[0].(*ast.CompositeLit)
				if !ok {
					t.Fatal("interfaceGUID is not a composite literal")
				}
				for _, rawElement := range literal.Elts {
					element := rawElement.(*ast.KeyValueExpr)
					key := element.Key.(*ast.Ident).Name
					switch key {
					case "Data1", "Data2", "Data3":
						value, ok := evalGoInteger(element.Value, values)
						if !ok {
							t.Fatalf("evaluate interfaceGUID.%s", key)
						}
						position := map[string]int{"Data1": 0, "Data2": 1, "Data3": 2}[key]
						guid[position] = value
					case "Data4":
						array := element.Value.(*ast.CompositeLit)
						if len(array.Elts) != 8 {
							t.Fatalf("interfaceGUID.Data4 elements=%d want=8", len(array.Elts))
						}
						for byteIndex, byteExpression := range array.Elts {
							value, ok := evalGoInteger(byteExpression, values)
							if !ok {
								t.Fatalf("evaluate interfaceGUID.Data4[%d]", byteIndex)
							}
							guid[3+byteIndex] = value
						}
					}
				}
			}
		}
	}
	return values, guid
}

func verifyGUIDAndIOCTLContract(t *testing.T, header string) {
	t.Helper()
	goValues, goGUID := goWindowsContract(t)
	guidNames := []string{
		"VIIPER_UDE_INTERFACE_GUID_DATA1", "VIIPER_UDE_INTERFACE_GUID_DATA2",
		"VIIPER_UDE_INTERFACE_GUID_DATA3", "VIIPER_UDE_INTERFACE_GUID_DATA4_0",
		"VIIPER_UDE_INTERFACE_GUID_DATA4_1", "VIIPER_UDE_INTERFACE_GUID_DATA4_2",
		"VIIPER_UDE_INTERFACE_GUID_DATA4_3", "VIIPER_UDE_INTERFACE_GUID_DATA4_4",
		"VIIPER_UDE_INTERFACE_GUID_DATA4_5", "VIIPER_UDE_INTERFACE_GUID_DATA4_6",
		"VIIPER_UDE_INTERFACE_GUID_DATA4_7",
	}
	for index, name := range guidNames {
		if got := cDefineNumber(t, header, name); got != goGUID[index] {
			t.Errorf("%s=%#x want Go interface GUID component %#x", name, got, goGUID[index])
		}
	}

	type ioctlSpec struct {
		goName string
		offset uint64
		method string
		access string
	}
	specs := map[string]ioctlSpec{
		"NEGOTIATE":           {"ioctlNegotiate", 0, "METHOD_BUFFERED", "FILE_READ_DATA | FILE_WRITE_DATA"},
		"CREATE_DEVICE":       {"ioctlCreateDevice", 1, "METHOD_BUFFERED", "FILE_READ_DATA | FILE_WRITE_DATA"},
		"DESTROY_DEVICE":      {"ioctlDestroyDevice", 2, "METHOD_BUFFERED", "FILE_READ_DATA | FILE_WRITE_DATA"},
		"DEQUEUE_OPERATION":   {"ioctlDequeueOperation", 3, "METHOD_OUT_DIRECT", "FILE_READ_DATA | FILE_WRITE_DATA"},
		"COMPLETE_OPERATION":  {"ioctlCompleteOperation", 4, "METHOD_IN_DIRECT", "FILE_READ_DATA | FILE_WRITE_DATA"},
		"QUERY_STATS":         {"ioctlQueryStats", 5, "METHOD_BUFFERED", "FILE_READ_DATA"},
		"SUBMIT_INPUT_REPORT": {"ioctlSubmitInputReport", 6, "METHOD_IN_DIRECT", "FILE_READ_DATA | FILE_WRITE_DATA"},
	}
	pattern := regexp.MustCompile(`(?m)^#define IOCTL_VIIPER_UDE_([A-Z_]+) CTL_CODE\(FILE_DEVICE_UNKNOWN, VIIPER_UDE_IOCTL_BASE \+ ([0-9]+), (METHOD_[A-Z_]+), ([^)]+)\)$`)
	seen := make(map[string]bool)
	methods := map[string]uint64{"METHOD_BUFFERED": 0, "METHOD_IN_DIRECT": 1, "METHOD_OUT_DIRECT": 2}
	for _, match := range pattern.FindAllStringSubmatch(header, -1) {
		spec, ok := specs[match[1]]
		if !ok {
			t.Fatalf("C contract added unmodeled IOCTL %s", match[1])
		}
		offset, _ := strconv.ParseUint(match[2], 10, 64)
		accessText := strings.Join(strings.Fields(match[4]), " ")
		if offset != spec.offset || match[3] != spec.method || accessText != spec.access {
			t.Errorf("IOCTL %s C definition=(%d,%s,%s) want=(%d,%s,%s)",
				match[1], offset, match[3], accessText, spec.offset, spec.method, spec.access)
		}
		access := goValues["fileReadData"] | goValues["fileWriteData"]
		if spec.access == "FILE_READ_DATA" {
			access = goValues["fileReadData"]
		}
		computed := goValues["fileDeviceUnknown"]<<16 | (access << 14) |
			(goValues["ioctlBase"]+offset)<<2 | methods[spec.method]
		if got, ok := goValues[spec.goName]; !ok || got != computed {
			t.Errorf("%s=%#x present=%v want C CTL_CODE %#x", spec.goName, got, ok, computed)
		}
		seen[match[1]] = true
	}
	if len(seen) != len(specs) {
		t.Fatalf("C IOCTL contracts found=%d want=%d", len(seen), len(specs))
	}
}

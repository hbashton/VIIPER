package udecx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestBuildIdentityCanonicalGoldenVector(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	const want = "9a8c5a75d8c54569f3a8f7e1b2c9a68b8b40bf06494285fa93b56895a98ba3fe"

	identity, err := DeriveBuildIdentity(
		revision,
		DriverPackageVersion,
		ABIMajor,
		ABIMinor,
		AdvertisedCapabilities,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := BuildIdentityHex(identity); got != want {
		t.Fatalf("build identity=%s want=%s", got, want)
	}
	upper, err := DeriveBuildIdentity(
		strings.ToUpper(revision),
		DriverPackageVersion,
		ABIMajor,
		ABIMinor,
		AdvertisedCapabilities,
	)
	if err != nil || upper != identity {
		t.Fatalf("uppercase source revision did not normalize: identity=%x error=%v", upper, err)
	}
}

func TestBuildIdentityRejectsNoncanonicalInputs(t *testing.T) {
	for name, revision := range map[string]string{
		"missing": "",
		"short":   strings.Repeat("a", 39),
		"odd":     strings.Repeat("a", 41),
		"not hex": strings.Repeat("z", 40),
		"spaced":  " " + strings.Repeat("a", 40),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DeriveBuildIdentity(
				revision,
				DriverPackageVersion,
				ABIMajor,
				ABIMinor,
				AdvertisedCapabilities,
			)
			if !errors.Is(err, ErrBuildIdentity) {
				t.Fatalf("error=%v want ErrBuildIdentity", err)
			}
		})
	}

	validRevision := strings.Repeat("a", 40)
	for name, mutate := range map[string]func() (string, uint16, Capabilities){
		"three-part package": func() (string, uint16, Capabilities) {
			return "0.1.0", ABIMajor, AdvertisedCapabilities
		},
		"nonnumeric package": func() (string, uint16, Capabilities) {
			return "0.1.x.38", ABIMajor, AdvertisedCapabilities
		},
		"zero major": func() (string, uint16, Capabilities) {
			return DriverPackageVersion, 0, AdvertisedCapabilities
		},
		"zero capabilities": func() (string, uint16, Capabilities) {
			return DriverPackageVersion, ABIMajor, 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			version, major, capabilities := mutate()
			_, err := DeriveBuildIdentity(
				validRevision,
				version,
				major,
				ABIMinor,
				capabilities,
			)
			if !errors.Is(err, ErrBuildIdentity) {
				t.Fatalf("error=%v want ErrBuildIdentity", err)
			}
		})
	}
}

func TestExpectedBuildIdentityFailsClosedWithoutBuildInjection(t *testing.T) {
	previous := nativeSourceRevision
	nativeSourceRevision = ""
	t.Cleanup(func() { nativeSourceRevision = previous })

	if _, err := ExpectedBuildIdentity(); !errors.Is(err, ErrBuildIdentity) {
		t.Fatalf("error=%v want ErrBuildIdentity", err)
	}
}

func TestParseHeaderFailsClosed(t *testing.T) {
	header, err := NewHeader(HeaderSize)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, HeaderSize)
	putHeader(raw, header)

	tests := []struct {
		name string
		edit func([]byte) []byte
		want error
	}{
		{
			"short",
			func(value []byte) []byte { return value[:HeaderSize-1] },
			ErrShortMessage,
		},
		{
			"bad magic",
			func(value []byte) []byte {
				binary.LittleEndian.PutUint32(value[0:4], 0)
				return value
			},
			ErrBadMagic,
		},
		{
			"older major",
			func(value []byte) []byte {
				binary.LittleEndian.PutUint16(value[4:6], ABIMajor-1)
				return value
			},
			ErrIncompatibleMajor,
		},
		{
			"future major",
			func(value []byte) []byte {
				binary.LittleEndian.PutUint16(value[4:6], ABIMajor+1)
				return value
			},
			ErrIncompatibleMajor,
		},
		{
			"older minor",
			func(value []byte) []byte {
				binary.LittleEndian.PutUint16(value[6:8], ABIMinor-1)
				return value
			},
			ErrIncompatibleMinor,
		},
		{
			"future minor",
			func(value []byte) []byte {
				binary.LittleEndian.PutUint16(value[6:8], ABIMinor+1)
				return value
			},
			ErrIncompatibleMinor,
		},
		{
			"unsupported flags",
			func(value []byte) []byte {
				binary.LittleEndian.PutUint32(value[12:16], 1)
				return value
			},
			ErrInvalidRange,
		},
		{
			"size below header",
			func(value []byte) []byte {
				binary.LittleEndian.PutUint32(value[8:12], HeaderSize-1)
				return value
			},
			ErrInvalidSize,
		},
		{
			"size beyond buffer",
			func(value []byte) []byte {
				binary.LittleEndian.PutUint32(value[8:12], HeaderSize+1)
				return value
			},
			ErrInvalidSize,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := append([]byte(nil), raw...)
			_, got := ParseHeader(test.edit(candidate))
			if !errors.Is(got, test.want) {
				t.Fatalf("error=%v want=%v", got, test.want)
			}
		})
	}
}

func TestNegotiationRequestRejectsUnsupportedCapabilities(t *testing.T) {
	for name, request := range map[string]NegotiateRequest{
		"zero nonce": {
			RequestedCapabilities: AdvertisedCapabilities,
		},
		"missing capability": {
			ClientNonce:           1,
			RequestedCapabilities: AdvertisedCapabilities &^ CapabilityInputReports,
		},
		"unknown capability": {
			ClientNonce:           1,
			RequestedCapabilities: AdvertisedCapabilities | Capabilities(1<<31),
		},
		"unadvertised streams": {
			ClientNonce:           1,
			RequestedCapabilities: AdvertisedCapabilities | CapabilityStreams,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := request.MarshalBinary()
			if request.ClientNonce == 0 {
				if !errors.Is(err, ErrInvalidRange) {
					t.Fatalf("error=%v want ErrInvalidRange", err)
				}
				return
			}
			if !errors.Is(err, ErrCapabilities) {
				t.Fatalf("error=%v want ErrCapabilities", err)
			}
		})
	}
}

func TestNegotiationResponseValidationFailsClosed(t *testing.T) {
	const clientNonce = 0x1122334455667788
	valid, expectedIdentity := validNegotiationFixture(t)

	response, err := ParseNegotiateResponse(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateNegotiation(response, clientNonce, expectedIdentity); err != nil {
		t.Fatalf("valid negotiation: %v", err)
	}

	t.Run("trailing bytes", func(t *testing.T) {
		trailing := append(append([]byte(nil), valid...), 0)
		if _, err := ParseNegotiateResponse(trailing); !errors.Is(err, ErrInvalidSize) {
			t.Fatalf("error=%v want ErrInvalidSize", err)
		}
	})

	parseMutation := func(t *testing.T, mutate func([]byte)) NegotiateResponse {
		t.Helper()
		candidate := append([]byte(nil), valid...)
		mutate(candidate)
		got, err := ParseNegotiateResponse(candidate)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		return got
	}
	tests := []struct {
		name   string
		mutate func([]byte)
		nonce  uint64
		want   error
	}{
		{
			"zero expected nonce",
			func([]byte) {},
			0,
			ErrInvalidRange,
		},
		{
			"nonce mismatch",
			func(value []byte) {
				binary.LittleEndian.PutUint64(value[16:24], clientNonce+1)
			},
			clientNonce,
			ErrInvalidRange,
		},
		{
			"zero driver nonce",
			func(value []byte) {
				binary.LittleEndian.PutUint64(value[24:32], 0)
			},
			clientNonce,
			ErrInvalidRange,
		},
		{
			"missing capability",
			func(value []byte) {
				binary.LittleEndian.PutUint32(
					value[32:36],
					uint32(AdvertisedCapabilities&^CapabilityInputReports),
				)
			},
			clientNonce,
			ErrCapabilities,
		},
		{
			"unknown capability",
			func(value []byte) {
				binary.LittleEndian.PutUint32(
					value[32:36],
					uint32(AdvertisedCapabilities|Capabilities(1<<31)),
				)
			},
			clientNonce,
			ErrCapabilities,
		},
		{
			"zero limit",
			func(value []byte) {
				binary.LittleEndian.PutUint32(value[36:40], 0)
			},
			clientNonce,
			ErrInvalidRange,
		},
		{
			"limit exceeds ABI",
			func(value []byte) {
				binary.LittleEndian.PutUint32(value[36:40], MaxDevices+1)
			},
			clientNonce,
			ErrLimitExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := parseMutation(t, test.mutate)
			err := ValidateNegotiation(response, test.nonce, expectedIdentity)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}

	t.Run("build identity mismatch", func(t *testing.T) {
		response := parseMutation(t, func(value []byte) { value[56] ^= 0xff })
		if err := ValidateNegotiation(
			response,
			clientNonce,
			expectedIdentity,
		); !errors.Is(err, ErrIncompatibleABI) {
			t.Fatalf("error=%v want ErrIncompatibleABI", err)
		}
	})

	t.Run("missing expected build identity", func(t *testing.T) {
		response, err := ParseNegotiateResponse(valid)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateNegotiation(
			response,
			clientNonce,
			[BuildIdentitySize]byte{},
		); !errors.Is(err, ErrBuildIdentity) {
			t.Fatalf("error=%v want ErrBuildIdentity", err)
		}
	})
}

func TestFixedResponsesRejectUnvalidatedTrailingBytes(t *testing.T) {
	negotiation, _ := validNegotiationFixture(t)
	stats := make([]byte, StatsSize)
	statsHeader, err := NewHeader(StatsSize)
	if err != nil {
		t.Fatal(err)
	}
	putHeader(stats, statsHeader)
	trace := make([]byte, LifecycleTraceSize)
	traceHeader, err := NewHeader(LifecycleTraceSize)
	if err != nil {
		t.Fatal(err)
	}
	putHeader(trace, traceHeader)
	binary.LittleEndian.PutUint32(trace[36:40], LifecycleTraceRecordSize)
	binary.LittleEndian.PutUint32(trace[40:44], LifecycleTraceCapacity)

	for name, test := range map[string]struct {
		message []byte
		parse   func([]byte) error
	}{
		"negotiation": {
			negotiation,
			func(value []byte) error {
				_, err := ParseNegotiateResponse(value)
				return err
			},
		},
		"stats": {
			stats,
			func(value []byte) error {
				_, err := ParseStats(value)
				return err
			},
		},
		"lifecycle trace": {
			trace,
			func(value []byte) error {
				_, err := ParseLifecycleTrace(value)
				return err
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := test.parse(test.message); err != nil {
				t.Fatalf("valid fixed response: %v", err)
			}
			trailing := append(append([]byte(nil), test.message...), 0)
			if err := test.parse(trailing); !errors.Is(err, ErrInvalidSize) {
				t.Fatalf("trailing byte error=%v want ErrInvalidSize", err)
			}
		})
	}
}

func dequeuedOperationGoldenWireImage() []byte {
	return []byte{
		0x56, 0x55, 0x44, 0x45, 0x01, 0x00, 0x0e, 0x00,
		0x6f, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11,
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0x40, 0x30, 0x20, 0x10, 0x02, 0x00, 0x00, 0x00,
		0x81, 0x01, 0x02, 0x03, 0x09, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 0x78, 0x56, 0x34, 0x12,
		0x00, 0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00,
		0x6c, 0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00,
		0x6c, 0x00, 0x00, 0x00, 0x80, 0x06, 0x00, 0x01,
		0x00, 0x00, 0x12, 0x00, 0x03, 0x04, 0x00, 0x02,
		0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01,
		0x18, 0x17, 0x16, 0x15, 0x14, 0x13, 0x12, 0x11,
		0x80, 0x70, 0x60, 0x50, 0xaa, 0xbb, 0xcc,
	}
}

func TestParseDequeuedOperationGoldenWireImage(t *testing.T) {
	wire := dequeuedOperationGoldenWireImage()
	buffer := append(append([]byte(nil), wire...), 0xde, 0xad)
	got, err := ParseDequeuedOperation(buffer, uint32(len(wire)))
	if err != nil {
		t.Fatal(err)
	}
	want := Operation{
		Token:                 0x1122334455667788,
		DeviceID:              0x8877665544332211,
		Generation:            0x10203040,
		Kind:                  OperationTransfer,
		EndpointAddress:       0x81,
		Direction:             1,
		InterfaceNumber:       2,
		InterfaceSetting:      3,
		EndpointAttributes:    3,
		EndpointInterval:      4,
		EndpointMaxPacketSize: 0x0200,
		URBFunction:           9,
		TransferFlags:         TransferFlagDirectionIn,
		StartFrame:            0x12345678,
		TransferLength:        3,
		SetupPacket:           [8]byte{0x80, 0x06, 0x00, 0x01, 0x00, 0x00, 0x12, 0x00},
		IsoPackets:            []IsoPacket{},
		Payload:               []byte{0xaa, 0xbb, 0xcc},
		EndpointSequence:      0x0102030405060708,
		DeviceSequence:        0x1112131415161718,
		EndpointGeneration:    0x50607080,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("operation=%+v want=%+v", got, want)
	}
}

func TestParseDequeuedOperationRejectsInvalidByteCounts(t *testing.T) {
	valid := dequeuedOperationGoldenWireImage()
	tests := []struct {
		name          string
		buffer        []byte
		bytesReturned uint32
	}{
		{
			name:          "short",
			buffer:        valid[:OperationSize-1],
			bytesReturned: OperationSize - 1,
		},
		{
			name:          "oversized",
			buffer:        valid,
			bytesReturned: uint32(len(valid) + 1),
		},
		{
			name:          "embedded size exceeds bytes returned",
			buffer:        valid,
			bytesReturned: uint32(len(valid) - 1),
		},
		{
			name:          "reported trailing byte",
			buffer:        append(append([]byte(nil), valid...), 0xff),
			bytesReturned: uint32(len(valid) + 1),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseDequeuedOperation(
				test.buffer,
				test.bytesReturned,
			); !errors.Is(err, ErrInvalidSize) {
				t.Fatalf("error=%v want ErrInvalidSize", err)
			}
		})
	}
}

func TestInputReportEndpointIdentityGoldenWireImage(t *testing.T) {
	report := InputReport{
		DeviceID:           0x1122334455667788,
		Generation:         0x10203040,
		EndpointGeneration: 0x50607080,
		EndpointAddress:    0x81,
		Transition:         true,
		Sequence:           0x0877665544332211,
		Payload:            []byte{0xaa, 0xbb, 0xcc},
	}
	got, err := report.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x56, 0x55, 0x44, 0x45, 0x01, 0x00, 0x0e, 0x00,
		0x37, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11,
		0x40, 0x30, 0x20, 0x10, 0x81, 0x01, 0x00, 0x00,
		0x34, 0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00,
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x08,
		0x80, 0x70, 0x60, 0x50, 0xaa, 0xbb, 0xcc,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("input report=\n%x\nwant=\n%x", got, want)
	}
}

func TestInputReportRejectsInvalidEndpointIdentity(t *testing.T) {
	base := InputReport{
		DeviceID:           1,
		Generation:         1,
		EndpointGeneration: 1,
		EndpointAddress:    0x81,
		Sequence:           1,
		Payload:            []byte{1},
	}
	tests := map[string]struct {
		mutate func(*InputReport)
		want   error
	}{
		"zero device":              {func(report *InputReport) { report.DeviceID = 0 }, ErrInvalidRange},
		"zero generation":          {func(report *InputReport) { report.Generation = 0 }, ErrInvalidRange},
		"zero endpoint generation": {func(report *InputReport) { report.EndpointGeneration = 0 }, ErrInvalidRange},
		"OUT endpoint":             {func(report *InputReport) { report.EndpointAddress = 0x01 }, ErrInvalidRange},
		"zero sequence":            {func(report *InputReport) { report.Sequence = 0 }, ErrInvalidRange},
		"empty payload":            {func(report *InputReport) { report.Payload = nil }, ErrLimitExceeded},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			report := base
			test.mutate(&report)
			if _, err := report.MarshalBinary(); !errors.Is(err, test.want) {
				t.Fatalf("error=%v want %v", err, test.want)
			}
		})
	}
}

func TestCreateDeviceResultRequiresExactPortIdentity(t *testing.T) {
	valid := make([]byte, CreateDeviceResultSize)
	header, err := NewHeader(CreateDeviceResultSize)
	if err != nil {
		t.Fatal(err)
	}
	putHeader(valid, header)
	binary.LittleEndian.PutUint64(valid[16:24], 7)
	binary.LittleEndian.PutUint32(valid[24:28], 2)
	binary.LittleEndian.PutUint32(valid[28:32], uint32(DeviceSpeedHigh))
	binary.LittleEndian.PutUint32(valid[32:36], 1)

	result, err := ParseCreateDeviceResult(valid)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeviceID != 7 || result.Generation != 2 ||
		result.USB20PortNumber != 1 || result.USB30PortNumber != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}

	for name, mutate := range map[string]func([]byte){
		"no port": func(value []byte) {
			binary.LittleEndian.PutUint32(value[32:36], 0)
		},
		"two ports": func(value []byte) {
			binary.LittleEndian.PutUint32(value[36:40], MaxDevices+1)
		},
		"USB2 port out of range": func(value []byte) {
			binary.LittleEndian.PutUint32(value[32:36], MaxDevices+1)
		},
		"SuperSpeed on USB2 port": func(value []byte) {
			binary.LittleEndian.PutUint32(value[28:32], uint32(DeviceSpeedSuper))
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := append([]byte(nil), valid...)
			mutate(candidate)
			if _, err := ParseCreateDeviceResult(candidate); !errors.Is(err, ErrInvalidRange) {
				t.Fatalf("error=%v want ErrInvalidRange", err)
			}
		})
	}
}

func FuzzABIFixedMessageDecoders(f *testing.F) {
	f.Add([]byte{})
	raw, _ := NewHeader(HeaderSize)
	header := make([]byte, HeaderSize)
	putHeader(header, raw)
	f.Add(header)
	negotiation := make([]byte, NegotiateResponseSize)
	negotiationHeader, _ := NewHeader(NegotiateResponseSize)
	putHeader(negotiation, negotiationHeader)
	f.Add(negotiation)
	f.Fuzz(func(t *testing.T, value []byte) {
		_, _ = ParseHeader(value)
		_, _ = ParseNegotiateResponse(value)
		_, _ = ParseCreateDeviceResult(value)
		_, _ = ParseStats(value)
		_, _ = ParseLifecycleTrace(value)
	})
}

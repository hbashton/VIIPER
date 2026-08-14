package udecx

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestBuildIdentityCanonicalVectorAndValidation(t *testing.T) {
	t.Parallel()

	const revision = "0123456789abcdef0123456789abcdef01234567"
	const wantHex = "c2fee12b34725595496b259e38b2985ba1fad35ed76606c19091fd5564366058"
	identity, err := DeriveBuildIdentity(revision, DriverPackageVersion,
		ABIMajor, ABIMinor, AdvertisedCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	if got := BuildIdentityHex(identity); got != wantHex {
		t.Fatalf("build identity=%s want canonical PowerShell/C++ vector %s", got, wantHex)
	}
	want, _ := hex.DecodeString(wantHex)
	if !bytes.Equal(identity[:], want) {
		t.Fatal("build identity bytes do not match their canonical hex encoding")
	}
	upper, err := DeriveBuildIdentity(strings.ToUpper(revision), DriverPackageVersion,
		ABIMajor, ABIMinor, AdvertisedCapabilities)
	if err != nil || upper != identity {
		t.Fatalf("uppercase source revision did not normalize canonically: identity=%x error=%v", upper, err)
	}

	for name, revision := range map[string]string{
		"missing": "",
		"short":   strings.Repeat("a", 39),
		"odd":     strings.Repeat("a", 41),
		"not hex": strings.Repeat("z", 40),
		"spaced":  " " + strings.Repeat("a", 40),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DeriveBuildIdentity(revision, DriverPackageVersion,
				ABIMajor, ABIMinor, AdvertisedCapabilities); !errors.Is(err, ErrBuildIdentity) {
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

func TestParseNegotiationReturnsLoadedKernelBuildIdentity(t *testing.T) {
	raw := make([]byte, NegotiateResponseSize)
	header, err := NewHeader(NegotiateResponseSize)
	if err != nil {
		t.Fatal(err)
	}
	putHeader(raw, header)
	for index := 0; index < BuildIdentitySize; index++ {
		raw[56+index] = byte(index)
	}

	response, err := ParseNegotiateResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	for index, got := range response.BuildIdentity {
		if got != byte(index) {
			t.Fatalf("build identity byte %d=%#x want %#x", index, got, byte(index))
		}
	}
}

func TestABISizes(t *testing.T) {
	for name, got := range map[string]int{
		"header": HeaderSize, "negotiate request": NegotiateRequestSize,
		"negotiate response": NegotiateResponseSize, "descriptor": DescriptorRecordSize,
		"create device": CreateDeviceSize, "identity": DeviceIdentitySize,
		"iso packet": IsoPacketSize, "operation": OperationSize,
		"completion": CompletionSize, "input report": InputReportSize,
		"stats": StatsSize,
	} {
		if got%8 != 0 {
			t.Fatalf("%s ABI size %d is not 8-byte aligned", name, got)
		}
	}
}

func TestHeaderRejectsMalformedInput(t *testing.T) {
	valid, err := NewHeader(HeaderSize)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, HeaderSize)
	putHeader(raw, valid)

	tests := []struct {
		name string
		edit func([]byte) []byte
		want error
	}{
		{"short", func(b []byte) []byte { return b[:15] }, ErrShortMessage},
		{"magic", func(b []byte) []byte { binary.LittleEndian.PutUint32(b, 0); return b }, ErrBadMagic},
		{"major", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[4:6], ABIMajor+1); return b }, ErrIncompatibleMajor},
		{"minor", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[6:8], ABIMinor+1); return b }, ErrIncompatibleMinor},
		{"flags", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[12:16], 1); return b }, ErrInvalidRange},
		{"size below header", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[8:12], 15); return b }, ErrInvalidSize},
		{"size beyond buffer", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[8:12], 17); return b }, ErrInvalidSize},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := append([]byte(nil), raw...)
			_, got := ParseHeader(tc.edit(candidate))
			if !errors.Is(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCreateDeviceMarshallingBoundsDescriptors(t *testing.T) {
	msg := CreateDevice{
		DeviceID: 7, Generation: 2, Speed: DeviceSpeedHigh,
		MaxPendingOperations: 128,
		DescriptorData:       []byte{0x12, 0x01, 0xaa, 0xbb},
		Descriptors: []DescriptorRecord{
			{Kind: DescriptorDevice, Offset: 0, Length: 2},
			{Kind: DescriptorConfiguration, Offset: 2, Length: 2},
		},
	}
	raw, err := msg.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(raw), CreateDeviceSize+2*DescriptorRecordSize+4; got != want {
		t.Fatalf("size=%d want=%d", got, want)
	}
	if got := binary.LittleEndian.Uint32(raw[8:12]); got != uint32(len(raw)) {
		t.Fatalf("header size=%d want=%d", got, len(raw))
	}

	msg.Descriptors[1].Offset = 4
	msg.Descriptors[1].Length = 1
	if _, err = msg.MarshalBinary(); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("invalid descriptor range: got %v", err)
	}
}

func TestParseOperationCopiesPayloadAndPackets(t *testing.T) {
	payload := []byte{1, 2, 3, 4}
	total := OperationSize + IsoPacketSize + len(payload)
	h, _ := NewHeader(total)
	raw := make([]byte, total)
	putHeader(raw, h)
	binary.LittleEndian.PutUint64(raw[16:24], 99)
	binary.LittleEndian.PutUint64(raw[24:32], 4)
	binary.LittleEndian.PutUint32(raw[32:36], 8)
	binary.LittleEndian.PutUint32(raw[36:40], uint32(OperationTransfer))
	raw[40], raw[41] = 0x84, 1
	raw[42], raw[43] = 2, 1
	raw[84], raw[85] = 0x05, 4
	binary.LittleEndian.PutUint16(raw[86:88], 196)
	binary.LittleEndian.PutUint32(raw[56:60], 1)
	binary.LittleEndian.PutUint32(raw[60:64], uint32(len(payload)))
	binary.LittleEndian.PutUint32(raw[64:68], OperationSize+IsoPacketSize)
	binary.LittleEndian.PutUint32(raw[68:72], uint32(len(payload)))
	binary.LittleEndian.PutUint32(raw[72:76], OperationSize)
	binary.LittleEndian.PutUint64(raw[88:96], 17)
	binary.LittleEndian.PutUint64(raw[96:104], 23)
	binary.LittleEndian.PutUint32(raw[OperationSize:OperationSize+4], 0)
	binary.LittleEndian.PutUint32(raw[OperationSize+4:OperationSize+8], uint32(len(payload)))
	copy(raw[OperationSize+IsoPacketSize:], payload)

	op, err := ParseOperation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if op.Token != 99 || op.DeviceID != 4 || op.Generation != 8 ||
		op.EndpointSequence != 17 || op.DeviceSequence != 23 || op.InterfaceNumber != 2 ||
		op.InterfaceSetting != 1 || op.EndpointAttributes != 0x05 ||
		op.EndpointInterval != 4 || op.EndpointMaxPacketSize != 196 ||
		len(op.IsoPackets) != 1 {
		t.Fatalf("unexpected operation: %+v", op)
	}
	raw[len(raw)-1] = 0xff
	if op.Payload[3] != 4 {
		t.Fatal("operation retained mutable caller payload")
	}
}

func TestParseOperationRejectsMalformedCanonicalTail(t *testing.T) {
	valid := dualSenseIsoOperationFixture(1, 4)
	tests := []struct {
		name string
		edit func([]byte) []byte
		want error
	}{
		{
			name: "bytes after embedded size",
			edit: func(raw []byte) []byte { return append(raw, 0xaa) },
			want: ErrInvalidSize,
		},
		{
			name: "embedded size omits returned byte",
			edit: func(raw []byte) []byte {
				binary.LittleEndian.PutUint32(raw[8:12], uint32(len(raw)-1))
				return raw
			},
			want: ErrInvalidSize,
		},
		{
			name: "ISO table aliases fixed header",
			edit: func(raw []byte) []byte {
				binary.LittleEndian.PutUint32(raw[72:76], OperationSize-4)
				return raw
			},
			want: ErrInvalidRange,
		},
		{
			name: "payload aliases fixed header",
			edit: func(raw []byte) []byte {
				binary.LittleEndian.PutUint32(raw[64:68], OperationSize-1)
				return raw
			},
			want: ErrInvalidRange,
		},
		{
			name: "payload overlaps ISO table",
			edit: func(raw []byte) []byte {
				binary.LittleEndian.PutUint32(raw[64:68], OperationSize)
				return raw
			},
			want: ErrInvalidRange,
		},
		{
			name: "gap before payload",
			edit: func(raw []byte) []byte {
				binary.LittleEndian.PutUint32(raw[64:68], OperationSize+IsoPacketSize+1)
				return raw
			},
			want: ErrInvalidRange,
		},
		{
			name: "gap before ISO table",
			edit: func(raw []byte) []byte {
				binary.LittleEndian.PutUint32(raw[72:76], OperationSize+1)
				return raw
			},
			want: ErrInvalidRange,
		},
		{
			name: "unclaimed canonical tail byte",
			edit: func(raw []byte) []byte {
				raw = append(raw, 0)
				binary.LittleEndian.PutUint32(raw[8:12], uint32(len(raw)))
				return raw
			},
			want: ErrInvalidRange,
		},
		{
			name: "nonzero ISO reserved word",
			edit: func(raw []byte) []byte {
				binary.LittleEndian.PutUint32(raw[OperationSize+12:OperationSize+16], 1)
				return raw
			},
			want: ErrInvalidRange,
		},
		{
			name: "ISO packet exceeds transfer length",
			edit: func(raw []byte) []byte {
				binary.LittleEndian.PutUint32(raw[OperationSize:OperationSize+4], 3)
				binary.LittleEndian.PutUint32(raw[OperationSize+4:OperationSize+8], 2)
				return raw
			},
			want: ErrInvalidRange,
		},
		{
			name: "ISO packet extent overflows",
			edit: func(raw []byte) []byte {
				binary.LittleEndian.PutUint32(raw[OperationSize:OperationSize+4], ^uint32(0))
				binary.LittleEndian.PutUint32(raw[OperationSize+4:OperationSize+8], 2)
				return raw
			},
			want: ErrInvalidRange,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := append([]byte(nil), valid...)
			_, err := ParseOperation(test.edit(raw))
			if !errors.Is(err, test.want) {
				t.Fatalf("ParseOperation error=%v want=%v", err, test.want)
			}
		})
	}
}

func TestParseOperationAcceptsCanonicalEmptyTail(t *testing.T) {
	raw := make([]byte, OperationSize)
	header, err := NewHeader(OperationSize)
	if err != nil {
		t.Fatal(err)
	}
	putHeader(raw, header)
	binary.LittleEndian.PutUint64(raw[16:24], 7)
	binary.LittleEndian.PutUint64(raw[24:32], 9)
	binary.LittleEndian.PutUint32(raw[32:36], 2)
	binary.LittleEndian.PutUint32(raw[36:40], uint32(OperationEndpointStart))
	binary.LittleEndian.PutUint32(raw[64:68], OperationSize)
	binary.LittleEndian.PutUint32(raw[72:76], OperationSize)

	op, err := ParseOperation(raw)
	if err != nil {
		t.Fatalf("ParseOperation canonical empty tail: %v", err)
	}
	if op.Token != 7 || op.DeviceID != 9 || op.Generation != 2 ||
		op.Kind != OperationEndpointStart || len(op.IsoPackets) != 0 || len(op.Payload) != 0 {
		t.Fatalf("unexpected empty-tail operation: %+v", op)
	}

	binary.LittleEndian.PutUint32(raw[72:76], 0)
	if _, err = ParseOperation(raw); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("ParseOperation zero ISO offset error=%v want=%v", err, ErrInvalidRange)
	}
}

func TestParseDequeuedOperationRequiresExactBytesReturned(t *testing.T) {
	valid := dualSenseIsoOperationFixture(1, 4)
	if _, err := parseDequeuedOperation(valid, uint32(len(valid))); err != nil {
		t.Fatalf("valid dequeued operation: %v", err)
	}

	tests := []struct {
		name    string
		buffer  []byte
		written uint32
	}{
		{"short return", valid, OperationSize - 1},
		{"return exceeds buffer", valid, uint32(len(valid) + 1)},
		{"return truncates embedded size", valid, uint32(len(valid) - 1)},
		{"return includes trailing byte", append(append([]byte(nil), valid...), 0), uint32(len(valid) + 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseDequeuedOperation(test.buffer, test.written); !errors.Is(err, ErrInvalidSize) {
				t.Fatalf("parseDequeuedOperation error=%v want ErrInvalidSize", err)
			}
		})
	}

	headerMismatch := append([]byte(nil), valid...)
	binary.LittleEndian.PutUint32(headerMismatch[8:12], uint32(len(headerMismatch)-1))
	if _, err := parseDequeuedOperation(headerMismatch, uint32(len(headerMismatch))); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("header/bytes-returned mismatch error=%v want ErrInvalidSize", err)
	}
}

func TestCompletionMarshalling(t *testing.T) {
	raw, err := (Completion{
		Token: 3, DeviceID: 9, Generation: 4, Status: -1, USBDStatus: 0xc0000001,
		IsoPackets: []IsoPacket{{Offset: 0, Length: 3}}, Payload: []byte{7, 8, 9},
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(raw), CompletionSize+IsoPacketSize+3; got != want {
		t.Fatalf("size=%d want=%d", got, want)
	}
	if got := binary.LittleEndian.Uint32(raw[52:56]); got != CompletionSize+IsoPacketSize {
		t.Fatalf("payload offset=%d", got)
	}
}

func TestCompletionMarshallingPreservesZeroLengthSparseISO(t *testing.T) {
	payload := make([]byte, 64)
	raw, err := (Completion{
		Token:          3,
		DeviceID:       9,
		Generation:     4,
		TransferLength: 0,
		IsoPackets:     []IsoPacket{{Offset: 0, Length: 0}},
		Payload:        payload,
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(raw[44:48]); got != 0 {
		t.Fatalf("transfer length=%d want=0", got)
	}
	if got := binary.LittleEndian.Uint32(raw[56:60]); got != uint32(len(payload)) {
		t.Fatalf("payload length=%d want=%d", got, len(payload))
	}
}

func TestCompletionEncodingIntoCallerBufferDoesNotAllocate(t *testing.T) {
	completion := Completion{
		Token: 1, DeviceID: 2, Generation: 3, TransferLength: 4 * 196,
		IsoPackets: []IsoPacket{
			{Offset: 0, Length: 196}, {Offset: 196, Length: 196},
			{Offset: 392, Length: 196}, {Offset: 588, Length: 196},
		},
		Payload: make([]byte, 4*196),
	}
	_, _, total, err := completion.wireLayout()
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, total)
	for index := range dst {
		dst[index] = 0xff
	}
	allocations := testing.AllocsPerRun(1000, func() {
		if err := completion.marshalBinaryInto(dst); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("caller-buffer completion encoding allocated %.2f objects", allocations)
	}
	for index, value := range dst[64:CompletionSize] {
		if value != 0 {
			t.Fatalf("completion reserved byte %d retained %#x", 64+index, value)
		}
	}
	for packet := range completion.IsoPackets {
		offset := CompletionSize + packet*IsoPacketSize + 12
		if value := binary.LittleEndian.Uint32(dst[offset : offset+4]); value != 0 {
			t.Fatalf("ISO packet %d reserved word retained %#x", packet, value)
		}
	}
}

func TestInputReportMarshalling(t *testing.T) {
	raw, err := (InputReport{
		DeviceID: 5, Generation: 7, EndpointAddress: 0x81,
		Transition: true, Sequence: 11, Payload: []byte{1, 2, 3},
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != InputReportSize+3 ||
		raw[29] != InputReportTransition || raw[30] != 0 || raw[31] != 0 ||
		binary.LittleEndian.Uint32(raw[32:36]) != InputReportSize ||
		binary.LittleEndian.Uint32(raw[36:40]) != 3 ||
		binary.LittleEndian.Uint64(raw[40:48]) != 11 ||
		string(raw[InputReportSize:]) != string([]byte{1, 2, 3}) {
		t.Fatalf("invalid input-report wire layout: %x", raw)
	}
}

func TestInputReportMetadataEncodingDoesNotAllocate(t *testing.T) {
	report := InputReport{
		DeviceID: 5, Generation: 7, EndpointAddress: 0x81,
		Sequence: 11, Payload: []byte{1, 2, 3},
	}
	var metadata [InputReportSize]byte
	allocations := testing.AllocsPerRun(1000, func() {
		if err := report.marshalMetadata(metadata[:]); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("input-report metadata encoding allocated %.2f objects per call", allocations)
	}
}

func TestInputReportMetadataClearsReusedTransitionFlag(t *testing.T) {
	report := InputReport{
		DeviceID: 5, Generation: 7, EndpointAddress: 0x81,
		Transition: true, Sequence: 11, Payload: []byte{1},
	}
	var metadata [InputReportSize]byte
	if err := report.marshalMetadata(metadata[:]); err != nil {
		t.Fatal(err)
	}
	report.Transition = false
	report.Sequence++
	if err := report.marshalMetadata(metadata[:]); err != nil {
		t.Fatal(err)
	}
	if metadata[29] != 0 || metadata[30] != 0 || metadata[31] != 0 {
		t.Fatalf("reused input metadata retained flags/reserved bytes: %x", metadata[29:32])
	}
}

func TestIdentityAndStatsLayout(t *testing.T) {
	identity, err := (DeviceIdentity{DeviceID: 0x1122334455667788, Generation: 7}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(identity) != DeviceIdentitySize || binary.LittleEndian.Uint64(identity[16:24]) != 0x1122334455667788 {
		t.Fatalf("invalid identity layout: %x", identity)
	}

	raw := make([]byte, StatsSize)
	h, _ := NewHeader(StatsSize)
	putHeader(raw, h)
	binary.LittleEndian.PutUint64(raw[16:24], 11)
	binary.LittleEndian.PutUint64(raw[88:96], 29)
	binary.LittleEndian.PutUint64(raw[96:104], 31)
	binary.LittleEndian.PutUint64(raw[104:112], 0)
	binary.LittleEndian.PutUint32(raw[112:116], 3)
	binary.LittleEndian.PutUint32(raw[116:120], 5)
	binary.LittleEndian.PutUint32(raw[120:124], 7)
	binary.LittleEndian.PutUint64(raw[128:136], 37)
	binary.LittleEndian.PutUint64(raw[136:144], 41)
	stats, err := ParseStats(raw)
	if err != nil {
		t.Fatal(err)
	}
	if stats.OperationsDequeued != 11 || stats.BytesFromDevice != 29 || stats.NotificationEvents != 31 ||
		stats.ActiveDevices != 3 || stats.PendingOperations != 5 || stats.WaitingDequeues != 7 ||
		stats.InputReportsSubmitted != 37 || stats.InputReportsCompleted != 41 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func FuzzParseOperation(f *testing.F) {
	f.Add([]byte{})
	valid := make([]byte, OperationSize)
	h, _ := NewHeader(OperationSize)
	putHeader(valid, h)
	binary.LittleEndian.PutUint32(valid[64:68], OperationSize)
	binary.LittleEndian.PutUint32(valid[72:76], OperationSize)
	f.Add(valid)
	iso := dualSenseIsoOperationFixture(1, 4)
	f.Add(iso)
	trailing := append(append([]byte(nil), iso...), 0xaa)
	f.Add(trailing)
	reserved := append([]byte(nil), iso...)
	binary.LittleEndian.PutUint32(reserved[OperationSize+12:OperationSize+16], 1)
	f.Add(reserved)
	extent := append([]byte(nil), iso...)
	binary.LittleEndian.PutUint32(extent[OperationSize:OperationSize+4], 4)
	binary.LittleEndian.PutUint32(extent[OperationSize+4:OperationSize+8], 1)
	f.Add(extent)
	f.Fuzz(func(t *testing.T, raw []byte) {
		op, err := ParseOperation(raw)
		if err != nil {
			return
		}
		if len(raw) != int(binary.LittleEndian.Uint32(raw[8:12])) {
			t.Fatal("accepted bytes after embedded operation size")
		}
		packetCount := binary.LittleEndian.Uint32(raw[56:60])
		isoOffset := binary.LittleEndian.Uint32(raw[72:76])
		payloadOffset := binary.LittleEndian.Uint32(raw[64:68])
		if isoOffset != OperationSize || payloadOffset != OperationSize+packetCount*IsoPacketSize {
			t.Fatal("accepted noncanonical operation tails")
		}
		for index, packet := range op.IsoPackets {
			offset := int(isoOffset) + index*IsoPacketSize
			if binary.LittleEndian.Uint32(raw[offset+12:offset+16]) != 0 ||
				!validRange(packet.Offset, packet.Length, op.TransferLength) {
				t.Fatalf("accepted invalid ISO packet %d", index)
			}
		}
	})
}

func FuzzProtocolDecoders(f *testing.F) {
	f.Add([]byte{})
	negotiation := make([]byte, NegotiateResponseSize)
	h, _ := NewHeader(len(negotiation))
	putHeader(negotiation, h)
	f.Add(negotiation)
	stats := make([]byte, StatsSize)
	h, _ = NewHeader(len(stats))
	putHeader(stats, h)
	f.Add(stats)
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = ParseHeader(raw)
		_, _ = ParseNegotiateResponse(raw)
		_, _ = ParseStats(raw)
		_, _ = ParseOperation(raw)
	})
}

func dualSenseIsoOperationFixture(packetCount, packetLength int) []byte {
	total := OperationSize + packetCount*IsoPacketSize + packetCount*packetLength
	h, _ := NewHeader(total)
	raw := make([]byte, total)
	putHeader(raw, h)
	binary.LittleEndian.PutUint64(raw[16:24], 1)
	binary.LittleEndian.PutUint64(raw[24:32], 2)
	binary.LittleEndian.PutUint32(raw[32:36], 3)
	binary.LittleEndian.PutUint32(raw[36:40], uint32(OperationTransfer))
	raw[40], raw[41], raw[84], raw[85] = 0x04, 0, 0x05, 4
	binary.LittleEndian.PutUint16(raw[86:88], uint16(packetLength))
	binary.LittleEndian.PutUint32(raw[56:60], uint32(packetCount))
	binary.LittleEndian.PutUint32(raw[60:64], uint32(packetCount*packetLength))
	binary.LittleEndian.PutUint32(raw[64:68], uint32(OperationSize+packetCount*IsoPacketSize))
	binary.LittleEndian.PutUint32(raw[68:72], uint32(packetCount*packetLength))
	binary.LittleEndian.PutUint32(raw[72:76], OperationSize)
	binary.LittleEndian.PutUint64(raw[88:96], 1)
	for index := 0; index < packetCount; index++ {
		offset := OperationSize + index*IsoPacketSize
		binary.LittleEndian.PutUint32(raw[offset:offset+4], uint32(index*packetLength))
		binary.LittleEndian.PutUint32(raw[offset+4:offset+8], uint32(packetLength))
	}
	return raw
}

func BenchmarkParseDualSenseIsoOperation(b *testing.B) {
	raw := dualSenseIsoOperationFixture(4, 196)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		operation, err := ParseOperation(raw)
		if err != nil || len(operation.Payload) != 4*196 || len(operation.IsoPackets) != 4 {
			b.Fatalf("ParseOperation: operation=%+v err=%v", operation, err)
		}
	}
}

func BenchmarkMarshalDualSenseIsoCompletion(b *testing.B) {
	completion := Completion{
		Token: 1, DeviceID: 2, Generation: 3, TransferLength: 4 * 196,
		IsoPackets: []IsoPacket{
			{Offset: 0, Length: 196}, {Offset: 196, Length: 196},
			{Offset: 392, Length: 196}, {Offset: 588, Length: 196},
		},
		Payload: make([]byte, 4*196),
	}
	b.ReportAllocs()
	b.SetBytes(int64(CompletionSize + len(completion.IsoPackets)*IsoPacketSize + len(completion.Payload)))
	b.ResetTimer()
	for range b.N {
		raw, err := completion.MarshalBinary()
		if err != nil || len(raw) != CompletionSize+4*IsoPacketSize+4*196 {
			b.Fatalf("MarshalBinary: bytes=%d err=%v", len(raw), err)
		}
	}
}

func TestDualSenseIsoProtocolAllocationBudget(t *testing.T) {
	raw := dualSenseIsoOperationFixture(4, 196)
	parseAllocations := testing.AllocsPerRun(1000, func() {
		operation, err := ParseOperation(raw)
		if err != nil || len(operation.Payload) != 4*196 {
			panic("parse representative DualSense ISO operation")
		}
	})
	if parseAllocations > 2 {
		t.Fatalf("DualSense ISO parse allocated %.2f objects, budget is 2", parseAllocations)
	}

	completion := Completion{
		Token: 1, DeviceID: 2, Generation: 3, TransferLength: 4 * 196,
		IsoPackets: []IsoPacket{
			{Offset: 0, Length: 196}, {Offset: 196, Length: 196},
			{Offset: 392, Length: 196}, {Offset: 588, Length: 196},
		},
		Payload: make([]byte, 4*196),
	}
	marshalAllocations := testing.AllocsPerRun(1000, func() {
		encoded, err := completion.MarshalBinary()
		if err != nil || len(encoded) != CompletionSize+4*IsoPacketSize+4*196 {
			panic("marshal representative DualSense ISO completion")
		}
	})
	if marshalAllocations > 1 {
		t.Fatalf("DualSense ISO completion allocated %.2f objects, budget is 1", marshalAllocations)
	}
}

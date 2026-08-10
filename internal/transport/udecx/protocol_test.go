package udecx

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestABISizes(t *testing.T) {
	for name, got := range map[string]int{
		"header": HeaderSize, "negotiate request": NegotiateRequestSize,
		"negotiate response": NegotiateResponseSize, "descriptor": DescriptorRecordSize,
		"create device": CreateDeviceSize, "identity": DeviceIdentitySize,
		"iso packet": IsoPacketSize, "operation": OperationSize,
		"completion": CompletionSize, "stats": StatsSize,
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
	binary.LittleEndian.PutUint32(raw[56:60], 1)
	binary.LittleEndian.PutUint32(raw[60:64], uint32(len(payload)))
	binary.LittleEndian.PutUint32(raw[64:68], OperationSize+IsoPacketSize)
	binary.LittleEndian.PutUint32(raw[68:72], uint32(len(payload)))
	binary.LittleEndian.PutUint32(raw[72:76], OperationSize)
	binary.LittleEndian.PutUint32(raw[OperationSize:OperationSize+4], 0)
	binary.LittleEndian.PutUint32(raw[OperationSize+4:OperationSize+8], uint32(len(payload)))
	copy(raw[OperationSize+IsoPacketSize:], payload)

	op, err := ParseOperation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if op.Token != 99 || op.DeviceID != 4 || op.Generation != 8 || len(op.IsoPackets) != 1 {
		t.Fatalf("unexpected operation: %+v", op)
	}
	raw[len(raw)-1] = 0xff
	if op.Payload[3] != 4 {
		t.Fatal("operation retained mutable caller payload")
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
	binary.LittleEndian.PutUint32(raw[96:100], 3)
	binary.LittleEndian.PutUint32(raw[100:104], 5)
	binary.LittleEndian.PutUint32(raw[104:108], 7)
	stats, err := ParseStats(raw)
	if err != nil {
		t.Fatal(err)
	}
	if stats.OperationsDequeued != 11 || stats.BytesFromDevice != 29 ||
		stats.ActiveDevices != 3 || stats.PendingOperations != 5 || stats.WaitingDequeues != 7 {
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
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = ParseOperation(raw)
	})
}

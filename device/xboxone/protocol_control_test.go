package xboxone

import (
	"errors"
	"testing"
)

func TestProtocolControlACKOfficialGoldenMessages(t *testing.T) {
	tests := []struct {
		name     string
		wire     [ProtocolControlACKMessageSize]byte
		sequence uint8
		body     ProtocolControlACKBodyV1
	}{
		{
			name: "metadata first fragment",
			wire: [ProtocolControlACKMessageSize]byte{
				0x01, 0x20, 0x01, 0x09,
				0x00, 0x04, 0x20, 0x3a, 0x00, 0x00, 0x00, 0x80, 0x00,
			},
			sequence: 1,
			body: ProtocolControlACKBodyV1{
				ReferencedDataClass:     DataClassCommand,
				ReferencedMessageNumber: 4,
				ReferencedSystem:        true,
				FragmentOffset:          58,
				RemainingBuffer:         128,
			},
		},
		{
			name: "metadata final fragment",
			wire: [ProtocolControlACKMessageSize]byte{
				0x01, 0x20, 0x01, 0x09,
				0x00, 0x04, 0x20, 0xba, 0x00, 0x00, 0x00, 0x00, 0x00,
			},
			sequence: 1,
			body: ProtocolControlACKBodyV1{
				ReferencedDataClass:     DataClassCommand,
				ReferencedMessageNumber: 4,
				ReferencedSystem:        true,
				FragmentOffset:          186,
			},
		},
		{
			name: "table trace",
			wire: [ProtocolControlACKMessageSize]byte{
				0x01, 0x20, 0x7f, 0x09,
				0x00, 0x04, 0x20, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
			},
			sequence: 0x7f,
			body: ProtocolControlACKBodyV1{
				ReferencedDataClass:     DataClassCommand,
				ReferencedMessageNumber: 4,
				ReferencedSystem:        true,
				FragmentOffset:          256,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sequence, body, err := DecodeProtocolControlACKMessage(test.wire[:])
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if sequence != test.sequence || body != test.body {
				t.Fatalf("decoded = (%d, %+v), want (%d, %+v)",
					sequence, body, test.sequence, test.body)
			}
			var encoded [ProtocolControlACKMessageSize]byte
			if err := EncodeProtocolControlACKMessageInto(
				encoded[:], sequence, body); err != nil {
				t.Fatalf("encode: %v", err)
			}
			if encoded != test.wire {
				t.Fatalf("round trip = % x, want % x", encoded, test.wire)
			}
		})
	}
}

func TestProtocolControlACKReferenceFlagsExhaustive(t *testing.T) {
	for flags := 0; flags <= 0xff; flags++ {
		wire := [ProtocolControlACKBodySize]byte{0, 0x04, byte(flags)}
		body, err := DecodeProtocolControlACKBody(wire[:])
		wantValid := flags&^int(flagSystem|flagExpansion) == 0
		if !wantValid {
			if !errors.Is(err, ErrInvalidProtocolControlReferenceFlags) {
				t.Fatalf("flags 0x%02x error = %v", flags, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("flags 0x%02x rejected: %v", flags, err)
		}
		var encoded [ProtocolControlACKBodySize]byte
		if err := EncodeProtocolControlACKBodyInto(encoded[:], body); err != nil {
			t.Fatalf("flags 0x%02x re-encode: %v", flags, err)
		}
		if encoded != wire {
			t.Fatalf("flags 0x%02x round trip = % x, want % x", flags, encoded, wire)
		}
	}
}

func TestProtocolControlACKControlCodeExhaustive(t *testing.T) {
	for code := 0; code <= 0xff; code++ {
		wire := [ProtocolControlACKBodySize]byte{byte(code), 0x04, 0x20}
		_, err := DecodeProtocolControlACKBody(wire[:])
		if code == 0 {
			if err != nil {
				t.Fatalf("ACK code rejected: %v", err)
			}
		} else if !errors.Is(err, ErrUnsupportedProtocolControlCode) {
			t.Fatalf("code 0x%02x error = %v", code, err)
		}
	}
}

func TestProtocolControlACKRejectsWrongEnvelopeAndAtomicEncodeFailure(t *testing.T) {
	valid := [ProtocolControlACKMessageSize]byte{
		0x01, 0x20, 0x01, 0x09,
		0x00, 0x04, 0x20, 0x3a, 0, 0, 0, 0x80, 0,
	}
	tests := []struct {
		name  string
		index int
		value byte
		want  error
	}{
		{name: "wrong message", index: 0, value: 0x02, want: ErrInvalidProtocolControlMessage},
		{name: "wrong class", index: 0, value: 0x21, want: ErrInvalidProtocolControlMessage},
		{name: "not system", index: 1, value: 0x00, want: ErrInvalidProtocolControlMessage},
		{name: "ack requested", index: 1, value: 0x30, want: ErrInvalidProtocolControlMessage},
		{name: "secondary", index: 1, value: 0x21, want: ErrInvalidProtocolControlMessage},
		{name: "wrong length", index: 3, value: 0x08, want: ErrInvalidProtocolControlMessage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wire := valid
			wire[test.index] = test.value
			if _, _, err := DecodeProtocolControlACKMessage(wire[:]); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}

	want := [ProtocolControlACKMessageSize]byte{}
	for index := range want {
		want[index] = 0xa5
	}
	got := want
	invalid := ProtocolControlACKBodyV1{ReferencedDataClass: 4}
	if err := EncodeProtocolControlACKMessageInto(got[:], 1, invalid); !errors.Is(err, ErrReservedDataClass) {
		t.Fatalf("invalid body error = %v", err)
	}
	if got != want {
		t.Fatalf("invalid body mutated destination: % x", got)
	}
	validBody := ProtocolControlACKBodyV1{
		ReferencedDataClass:     DataClassCommand,
		ReferencedMessageNumber: 4,
		ReferencedSystem:        true,
	}
	if err := EncodeProtocolControlACKMessageInto(got[:], 0, validBody); !errors.Is(err, ErrReservedSequence) {
		t.Fatalf("zero sequence error = %v", err)
	}
	if got != want {
		t.Fatalf("zero sequence mutated destination: % x", got)
	}
}

func TestProtocolControlACKExactLengths(t *testing.T) {
	body := ProtocolControlACKBodyV1{
		ReferencedDataClass:     DataClassCommand,
		ReferencedMessageNumber: 4,
		ReferencedSystem:        true,
	}
	for _, size := range []int{ProtocolControlACKBodySize - 1, ProtocolControlACKBodySize + 1} {
		if err := EncodeProtocolControlACKBodyInto(make([]byte, size), body); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("body encode length %d: %v", size, err)
		}
		if _, err := DecodeProtocolControlACKBody(make([]byte, size)); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("body decode length %d: %v", size, err)
		}
	}
	for _, size := range []int{ProtocolControlACKMessageSize - 1, ProtocolControlACKMessageSize + 1} {
		if err := EncodeProtocolControlACKMessageInto(make([]byte, size), 1, body); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("message encode length %d: %v", size, err)
		}
		if _, _, err := DecodeProtocolControlACKMessage(make([]byte, size)); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("message decode length %d: %v", size, err)
		}
	}
}

func TestDecodedProtocolControlACKDrivesMetadataProgress(t *testing.T) {
	transfer := newTestMetadataTransfer(t, make([]byte, 61), 1, 4, 9, 0)
	claim, err := transfer.Claim(0)
	if err != nil {
		t.Fatal(err)
	}
	var packet [64]byte
	deliverMetadataClaim(t, &transfer, claim, packet[:], 0)
	wire := [ProtocolControlACKMessageSize]byte{
		0x01, 0x20, 0x01, 0x09,
		0x00, 0x04, 0x20, 0x3a, 0, 0, 0, 0x80, 0,
	}
	ack, err := DecodeMetadataReliableAcknowledgement(wire[:], 4, 9)
	if err != nil {
		t.Fatalf("decode bridge: %v", err)
	}
	if ack.ReceiverRemainingBufferBytes != 128 {
		t.Fatalf("remaining buffer = %d", ack.ReceiverRemainingBufferBytes)
	}
	disposition, err := transfer.Acknowledge(ack, 1)
	if err != nil || disposition != ReliableAcknowledgementProgress {
		t.Fatalf("acknowledge = (%d, %v)", disposition, err)
	}
	if got := transfer.Snapshot(); got.AcknowledgedEnd != 58 || got.Offset != 58 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestMetadataACKBridgeRejectsWrongReferenceAndOffset(t *testing.T) {
	base := [ProtocolControlACKMessageSize]byte{
		0x01, 0x20, 0x01, 0x09,
		0x00, 0x04, 0x20, 0x3a, 0, 0, 0, 0x80, 0,
	}
	tests := []struct {
		name       string
		mutate     func(*[ProtocolControlACKMessageSize]byte)
		generation uint64
		epoch      uint64
	}{
		{name: "zero generation", generation: 0, epoch: 1},
		{name: "zero epoch", generation: 1, epoch: 0},
		{name: "wrong ref class", generation: 1, epoch: 1, mutate: func(w *[13]byte) { w[5] = 0x24 }},
		{name: "wrong ref message", generation: 1, epoch: 1, mutate: func(w *[13]byte) { w[5] = 0x05 }},
		{name: "not ref system", generation: 1, epoch: 1, mutate: func(w *[13]byte) { w[6] = 0x00 }},
		{name: "ref expansion", generation: 1, epoch: 1, mutate: func(w *[13]byte) { w[6] = 0x21 }},
		{name: "offset above metadata bound", generation: 1, epoch: 1, mutate: func(w *[13]byte) { w[7], w[8] = 0x00, 0x40 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wire := base
			if test.mutate != nil {
				test.mutate(&wire)
			}
			if _, err := DecodeMetadataReliableAcknowledgement(
				wire[:], test.generation, test.epoch); !errors.Is(err, ErrInvalidAcknowledgement) {
				t.Fatalf("error = %v, want ErrInvalidAcknowledgement", err)
			}
		})
	}
}

func TestProtocolControlACKHotPathAllocatesZero(t *testing.T) {
	body := ProtocolControlACKBodyV1{
		ReferencedDataClass:     DataClassCommand,
		ReferencedMessageNumber: 4,
		ReferencedSystem:        true,
		FragmentOffset:          58,
		RemainingBuffer:         128,
	}
	var wire [ProtocolControlACKMessageSize]byte
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := EncodeProtocolControlACKMessageInto(wire[:], 1, body); err != nil {
			panic(err)
		}
		if _, _, err := DecodeProtocolControlACKMessage(wire[:]); err != nil {
			panic(err)
		}
		if _, err := DecodeMetadataReliableAcknowledgement(wire[:], 1, 1); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("protocol-control ACK hot-path allocations = %v, want 0", allocs)
	}
}

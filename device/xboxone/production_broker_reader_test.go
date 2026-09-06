package xboxone

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func productionBrokerInputFixture() []byte {
	frame := make([]byte, productionBrokerHeaderSize+SemanticInputWireSize)
	copy(frame, productionBrokerMagic[:])
	frame[4] = productionBrokerWireVersion
	frame[5] = productionBrokerSemanticInput
	binary.LittleEndian.PutUint16(frame[6:8], SemanticInputWireSize)
	binary.LittleEndian.PutUint64(frame[8:16], 73)
	for i := productionBrokerHeaderSize; i < len(frame); i++ {
		frame[i] = byte(i*17 + 3)
	}
	return frame
}

func TestProductionBrokerStreamReaderWarmInputAllocatesNothing(t *testing.T) {
	wire := productionBrokerInputFixture()
	source := bytes.NewReader(wire)
	reader := newProductionBrokerFrameReader(source)
	var payload [productionBrokerMaximumPayload]byte
	var got productionBrokerFrame
	var readErr error
	allocations := testing.AllocsPerRun(10000, func() {
		source.Reset(wire)
		got, readErr = reader.read(payload[:])
	})
	if readErr != nil || got.correlation != 73 ||
		got.typeID != productionBrokerSemanticInput ||
		!bytes.Equal(got.payload, wire[productionBrokerHeaderSize:]) {
		t.Fatalf("read frame=%+v err=%v", got, readErr)
	}
	if allocations != 0 {
		t.Fatalf("warm broker input decoder allocated %.0f objects/frame, want zero", allocations)
	}
}

func BenchmarkProductionBrokerStreamReaderInput(b *testing.B) {
	wire := productionBrokerInputFixture()
	source := bytes.NewReader(wire)
	reader := newProductionBrokerFrameReader(source)
	var payload [productionBrokerMaximumPayload]byte
	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		source.Reset(wire)
		got, err := reader.read(payload[:])
		if err != nil || got.correlation != 73 {
			b.Fatalf("correlation=%d err=%v", got.correlation, err)
		}
	}
}

func TestProductionBrokerStreamReaderDoesNotReuseAnOldHeaderAfterShortRead(t *testing.T) {
	wire := productionBrokerInputFixture()
	source := bytes.NewReader(wire)
	reader := newProductionBrokerFrameReader(source)
	var payload [productionBrokerMaximumPayload]byte
	if _, err := reader.read(payload[:]); err != nil {
		t.Fatal(err)
	}
	for length := 0; length < len(wire); length++ {
		source.Reset(wire[:length])
		got, err := reader.read(payload[:])
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated length %d: frame=%+v err=%v", length, got, err)
		}
		if got.typeID != 0 || got.correlation != 0 || got.payload != nil {
			t.Fatalf("truncated length %d exposed previous frame %+v", length, got)
		}
	}
}

func TestProductionBrokerStreamReaderRejectsMalformedFramesAfterAValidOne(t *testing.T) {
	wire := productionBrokerInputFixture()
	for _, offset := range []int{0, 1, 2, 3, 4, 5, 6, 7} {
		source := bytes.NewReader(wire)
		reader := newProductionBrokerFrameReader(source)
		var payload [productionBrokerMaximumPayload]byte
		if _, err := reader.read(payload[:]); err != nil {
			t.Fatal(err)
		}
		invalid := append([]byte(nil), wire...)
		invalid[offset] ^= 0x80
		source.Reset(invalid)
		if _, err := reader.read(payload[:]); !errors.Is(err, errProductionBrokerWire) {
			t.Fatalf("invalid header byte %d: %v", offset, err)
		}
	}
}

func TestProductionBrokerStreamReaderRejectsMissingStorageAndSources(t *testing.T) {
	var absent *productionBrokerFrameReader
	if _, err := absent.read(nil); !errors.Is(err, errProductionBrokerWire) {
		t.Fatalf("nil reader: %v", err)
	}
	if _, err := newProductionBrokerFrameReader(nil).read(nil); !errors.Is(err, errProductionBrokerWire) {
		t.Fatalf("nil source: %v", err)
	}
	reader := newProductionBrokerFrameReader(bytes.NewReader(productionBrokerInputFixture()))
	if _, err := reader.read(make([]byte, SemanticInputWireSize-1)); !errors.Is(err, errProductionBrokerWire) {
		t.Fatalf("short payload storage: %v", err)
	}
}

func TestProductionBrokerStreamReaderReusesStorageAcrossAllFrameLengths(t *testing.T) {
	var wire bytes.Buffer
	writer := newProductionBrokerFrameWriter(&wire)
	types := []byte{productionBrokerCanonicalFeedback, productionBrokerSemanticInput,
		productionBrokerCanonicalAck, productionBrokerConsumerReadyAck,
		productionBrokerSemanticInputAck, productionBrokerConsumerReady}
	lengths := []int{controllerPersonaCanonicalFeedbackWireSize, SemanticInputWireSize, 1, 0, 1, 0}
	for i, kind := range types {
		if err := writer.write(kind, uint64(i+1), bytes.Repeat([]byte{byte(i + 17)}, lengths[i])); err != nil {
			t.Fatal(err)
		}
	}
	reader := newProductionBrokerFrameReader(&wire)
	var payload [productionBrokerMaximumPayload]byte
	for i, kind := range types {
		got, err := reader.read(payload[:])
		if err != nil || got.typeID != kind || got.correlation != uint64(i+1) || len(got.payload) != lengths[i] {
			t.Fatalf("frame %d: got=%+v error=%v", i, got, err)
		}
		for _, value := range got.payload {
			if value != byte(i+17) {
				t.Fatalf("frame %d contains stale payload %x", i, got.payload)
			}
		}
	}
	if wire.Len() != 0 {
		t.Fatalf("unconsumed wire bytes: %d", wire.Len())
	}
}

func TestProductionBrokerStreamReaderStorageIsPrivateToEachStream(t *testing.T) {
	for i := 0; i < 8; i++ {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			t.Parallel()
			wire := productionBrokerInputFixture()
			want := uint64(i + 1)
			binary.LittleEndian.PutUint64(wire[8:16], want)
			source := bytes.NewReader(wire)
			reader := newProductionBrokerFrameReader(source)
			var payload [productionBrokerMaximumPayload]byte
			for repetition := 0; repetition < 1000; repetition++ {
				source.Reset(wire)
				got, err := reader.read(payload[:])
				if err != nil || got.correlation != want {
					t.Fatalf("private stream %d: got=%+v error=%v", want, got, err)
				}
			}
		})
	}
}

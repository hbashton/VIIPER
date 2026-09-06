package xboxone

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestProductionBrokerWireRoundTripsClosedMessageSet(t *testing.T) {
	tests := []struct {
		name        string
		typeID      byte
		correlation uint64
		payload     []byte
	}{
		{name: "consumer ready", typeID: productionBrokerConsumerReady},
		{name: "semantic input", typeID: productionBrokerSemanticInput,
			correlation: 2, payload: make([]byte, SemanticInputWireSize)},
		{name: "canonical feedback ack", typeID: productionBrokerCanonicalAck,
			correlation: 3, payload: []byte{productionBrokerAccepted}},
		{name: "ready ack", typeID: productionBrokerConsumerReadyAck},
		{name: "semantic input ack", typeID: productionBrokerSemanticInputAck,
			correlation: 4, payload: []byte{productionBrokerRejected}},
		{name: "canonical feedback", typeID: productionBrokerCanonicalFeedback,
			correlation: 5, payload: make([]byte,
				controllerPersonaCanonicalFeedbackWireSize)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for index := range test.payload {
				test.payload[index] = byte(index*17 + 3)
			}
			var wire bytes.Buffer
			if err := newProductionBrokerFrameWriter(&wire).write(
				test.typeID, test.correlation, test.payload); err != nil {
				t.Fatal(err)
			}
			var scratch [productionBrokerMaximumPayload]byte
			got, err := readProductionBrokerFrame(&wire, scratch[:])
			if err != nil {
				t.Fatal(err)
			}
			if got.typeID != test.typeID ||
				got.correlation != test.correlation ||
				!bytes.Equal(got.payload, test.payload) || wire.Len() != 0 {
				t.Fatalf("decoded = %+v payload=% x remaining=%d",
					got, got.payload, wire.Len())
			}
		})
	}
}

func TestProductionBrokerWireRejectsMalformedEnvelopeBeforeDispatch(
	t *testing.T,
) {
	valid := []byte{
		'X', '1', 'B', 'R', productionBrokerWireVersion,
		productionBrokerConsumerReady, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
	}
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "wrong magic", mutate: func(wire []byte) []byte {
			wire[0] = 'Y'
			return wire
		}},
		{name: "wrong version", mutate: func(wire []byte) []byte {
			wire[4]++
			return wire
		}},
		{name: "unknown type", mutate: func(wire []byte) []byte {
			wire[5] = 0x7f
			return wire
		}},
		{name: "wrong payload length", mutate: func(wire []byte) []byte {
			wire[6] = 1
			return append(wire, 0)
		}},
		{name: "truncated header", mutate: func(wire []byte) []byte {
			return wire[:len(wire)-1]
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wire := test.mutate(append([]byte(nil), valid...))
			var scratch [productionBrokerMaximumPayload]byte
			if _, err := readProductionBrokerFrame(
				bytes.NewReader(wire), scratch[:]); err == nil {
				t.Fatal("malformed broker frame was accepted")
			}
		})
	}

	semanticHeader := append([]byte(nil), valid...)
	semanticHeader[5] = productionBrokerSemanticInput
	semanticHeader[6] = SemanticInputWireSize
	var scratch [productionBrokerMaximumPayload]byte
	if _, err := readProductionBrokerFrame(
		bytes.NewReader(semanticHeader), scratch[:]); !errors.Is(
		err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated payload error = %v", err)
	}
}

type productionBrokerChunkWriter struct {
	buffer bytes.Buffer
	limit  int
}

func (writer *productionBrokerChunkWriter) Write(value []byte) (int, error) {
	if len(value) > writer.limit {
		value = value[:writer.limit]
	}
	return writer.buffer.Write(value)
}

type productionBrokerZeroWriter struct{}

func (productionBrokerZeroWriter) Write([]byte) (int, error) { return 0, nil }

func TestProductionBrokerFullWriteHandlesShortWritesAndZeroProgress(
	t *testing.T,
) {
	want := []byte{1, 2, 3, 4, 5, 6, 7}
	partial := &productionBrokerChunkWriter{limit: 2}
	if err := writeProductionBrokerFull(partial, want); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(partial.buffer.Bytes(), want) {
		t.Fatalf("short-write result = % x", partial.buffer.Bytes())
	}
	if err := writeProductionBrokerFull(
		productionBrokerZeroWriter{}, want); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("zero-progress error = %v", err)
	}
}

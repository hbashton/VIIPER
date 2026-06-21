package usbip

import (
	"bytes"
	"testing"
)

func TestIsoPacketDescriptorRoundTrip(t *testing.T) {
	want := IsoPacketDescriptor{
		Offset:       384,
		Length:       384,
		ActualLength: 384,
		Status:       -104,
	}

	var wire bytes.Buffer
	if err := want.Write(&wire); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if wire.Len() != 16 {
		t.Fatalf("unexpected descriptor length: got %d want 16", wire.Len())
	}

	var got IsoPacketDescriptor
	if err := got.Read(&wire); err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if got != want {
		t.Fatalf("descriptor mismatch: got %#v want %#v", got, want)
	}
}

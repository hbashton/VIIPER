package xboxone

import (
	"errors"
	"reflect"
	"testing"
)

func semanticInputGoldenState() InputStateV1 {
	return InputStateV1{
		Menu: true, A: true, Y: true, DPadUp: true, DPadRight: true,
		RightBumper: true, LeftStickButton: true, Guide: true, Share: true,
		LeftTrigger: 0x0234, RightTrigger: 0x03cd,
		LeftStickX: 0x0102, LeftStickY: -2,
		RightStickX: -32768, RightStickY: 32767,
	}
}

func TestSemanticInputWireGoldenAndRoundTrip(t *testing.T) {
	want := [SemanticInputWireSize]byte{
		0x01, 0x00, 0x18, 0x00,
		0x65, 0xda, 0x00, 0x00,
		0x34, 0x02, 0xcd, 0x03,
		0x02, 0x01, 0xfe, 0xff,
		0x00, 0x80, 0xff, 0x7f,
		0x00, 0x00, 0x00, 0x00,
	}
	state := semanticInputGoldenState()
	var got [SemanticInputWireSize]byte
	if err := EncodeSemanticInputWireV1Into(got[:], state); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got != want {
		t.Fatalf("wire = % x, want % x", got, want)
	}
	decoded, err := DecodeSemanticInputWireV1(got[:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(decoded, state) {
		t.Fatalf("decoded = %+v, want %+v", decoded, state)
	}
}

func TestSemanticInputWireButtonBasisIsExhaustive(t *testing.T) {
	tests := []struct {
		name string
		mask uint32
		set  func(*InputStateV1)
	}{
		{"menu", semanticButtonMenu, func(s *InputStateV1) { s.Menu = true }},
		{"view", semanticButtonView, func(s *InputStateV1) { s.View = true }},
		{"a", semanticButtonA, func(s *InputStateV1) { s.A = true }},
		{"b", semanticButtonB, func(s *InputStateV1) { s.B = true }},
		{"x", semanticButtonX, func(s *InputStateV1) { s.X = true }},
		{"y", semanticButtonY, func(s *InputStateV1) { s.Y = true }},
		{"up", semanticButtonDPadUp, func(s *InputStateV1) { s.DPadUp = true }},
		{"down", semanticButtonDPadDown, func(s *InputStateV1) { s.DPadDown = true }},
		{"left", semanticButtonDPadLeft, func(s *InputStateV1) { s.DPadLeft = true }},
		{"right", semanticButtonDPadRight, func(s *InputStateV1) { s.DPadRight = true }},
		{"lb", semanticButtonLeftBumper, func(s *InputStateV1) { s.LeftBumper = true }},
		{"rb", semanticButtonRightBumper, func(s *InputStateV1) { s.RightBumper = true }},
		{"ls", semanticButtonLeftStick, func(s *InputStateV1) { s.LeftStickButton = true }},
		{"rs", semanticButtonRightStick, func(s *InputStateV1) { s.RightStickButton = true }},
		{"guide", semanticButtonGuide, func(s *InputStateV1) { s.Guide = true }},
		{"share", semanticButtonShare, func(s *InputStateV1) { s.Share = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var state InputStateV1
			test.set(&state)
			var wire [SemanticInputWireSize]byte
			if err := EncodeSemanticInputWireV1Into(wire[:], state); err != nil {
				t.Fatalf("encode: %v", err)
			}
			got := uint32(wire[4]) | uint32(wire[5])<<8 |
				uint32(wire[6])<<16 | uint32(wire[7])<<24
			if got != test.mask {
				t.Fatalf("mask = 0x%08x, want 0x%08x", got, test.mask)
			}
			decoded, err := DecodeSemanticInputWireV1(wire[:])
			if err != nil || !reflect.DeepEqual(decoded, state) {
				t.Fatalf("decode = %+v, %v; want %+v", decoded, err, state)
			}
		})
	}
}

func TestSemanticInputWireFailsClosed(t *testing.T) {
	valid := [SemanticInputWireSize]byte{}
	if err := EncodeSemanticInputWireV1Into(valid[:], InputStateV1{}); err != nil {
		t.Fatal(err)
	}
	for length := 0; length <= SemanticInputWireSize*2; length++ {
		if length == SemanticInputWireSize {
			continue
		}
		if _, err := DecodeSemanticInputWireV1(make([]byte, length)); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("decode length %d: %v", length, err)
		}
		if err := EncodeSemanticInputWireV1Into(make([]byte, length), InputStateV1{}); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("encode length %d: %v", length, err)
		}
	}
	for _, mutate := range []func(*[SemanticInputWireSize]byte){
		func(w *[SemanticInputWireSize]byte) { w[0] = 2 },
		func(w *[SemanticInputWireSize]byte) { w[2] = 23 },
		func(w *[SemanticInputWireSize]byte) { w[6] = 1 },
		func(w *[SemanticInputWireSize]byte) { w[20] = 1 },
	} {
		wire := valid
		mutate(&wire)
		if _, err := DecodeSemanticInputWireV1(wire[:]); !errors.Is(err, ErrInvalidSemanticInputContract) {
			t.Errorf("mutated wire % x: %v", wire, err)
		}
	}
	wire := valid
	wire[9] = 4
	if _, err := DecodeSemanticInputWireV1(wire[:]); !errors.Is(err, ErrTriggerOutOfRange) {
		t.Fatalf("out-of-range trigger: %v", err)
	}
}

func TestSemanticInputWireEncodeIsAtomicAndNonallocating(t *testing.T) {
	want := [SemanticInputWireSize]byte{}
	for index := range want {
		want[index] = 0xa5
	}
	got := want
	if err := EncodeSemanticInputWireV1Into(got[:], InputStateV1{
		LeftTrigger: 1024,
	}); !errors.Is(err, ErrTriggerOutOfRange) {
		t.Fatalf("invalid trigger: %v", err)
	}
	if got != want {
		t.Fatalf("destination changed: % x", got)
	}

	state := semanticInputGoldenState()
	var wire [SemanticInputWireSize]byte
	if err := EncodeSemanticInputWireV1Into(wire[:], state); err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := EncodeSemanticInputWireV1Into(wire[:], state); err != nil {
			panic(err)
		}
		if _, err := DecodeSemanticInputWireV1(wire[:]); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("encode/decode allocations = %v, want 0", allocs)
	}
}

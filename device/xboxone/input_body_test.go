package xboxone

import (
	"errors"
	"reflect"
	"testing"
)

func TestBaseInputBodyGoldenAndRoundTrip(t *testing.T) {
	state := InputStateV1{
		Menu:            true,
		A:               true,
		Y:               true,
		DPadUp:          true,
		DPadRight:       true,
		RightBumper:     true,
		LeftStickButton: true,
		LeftTrigger:     0x1234,
		RightTrigger:    0xabcd,
		LeftStickX:      0x0102,
		LeftStickY:      -2,
		RightStickX:     -32768,
		RightStickY:     32767,
	}
	want := [BaseInputBodySize]byte{
		0x94, 0x69,
		0x34, 0x12,
		0xcd, 0xab,
		0x02, 0x01,
		0xfe, 0xff,
		0x00, 0x80,
		0xff, 0x7f,
	}

	var got [BaseInputBodySize]byte
	if err := EncodeBaseInputBodyInto(got[:], state); err != nil {
		t.Fatalf("EncodeBaseInputBodyInto: %v", err)
	}
	if got != want {
		t.Fatalf("encoded body = % x, want % x", got, want)
	}

	decoded, err := DecodeBaseInputBody(got[:])
	if err != nil {
		t.Fatalf("DecodeBaseInputBody: %v", err)
	}
	if !reflect.DeepEqual(decoded, state) {
		t.Fatalf("decoded state = %+v, want %+v", decoded, state)
	}
}

func TestBaseInputButtonBasis(t *testing.T) {
	tests := []struct {
		name string
		mask uint16
		set  func(*InputStateV1)
	}{
		{name: "menu", mask: 1 << 2, set: func(state *InputStateV1) { state.Menu = true }},
		{name: "view", mask: 1 << 3, set: func(state *InputStateV1) { state.View = true }},
		{name: "a", mask: 1 << 4, set: func(state *InputStateV1) { state.A = true }},
		{name: "b", mask: 1 << 5, set: func(state *InputStateV1) { state.B = true }},
		{name: "x", mask: 1 << 6, set: func(state *InputStateV1) { state.X = true }},
		{name: "y", mask: 1 << 7, set: func(state *InputStateV1) { state.Y = true }},
		{name: "d-pad up", mask: 1 << 8, set: func(state *InputStateV1) { state.DPadUp = true }},
		{name: "d-pad down", mask: 1 << 9, set: func(state *InputStateV1) { state.DPadDown = true }},
		{name: "d-pad left", mask: 1 << 10, set: func(state *InputStateV1) { state.DPadLeft = true }},
		{name: "d-pad right", mask: 1 << 11, set: func(state *InputStateV1) { state.DPadRight = true }},
		{name: "left bumper", mask: 1 << 12, set: func(state *InputStateV1) { state.LeftBumper = true }},
		{name: "right bumper", mask: 1 << 13, set: func(state *InputStateV1) { state.RightBumper = true }},
		{
			name: "left stick button", mask: 1 << 14,
			set: func(state *InputStateV1) { state.LeftStickButton = true },
		},
		{
			name: "right stick button", mask: 1 << 15,
			set: func(state *InputStateV1) { state.RightStickButton = true },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var state InputStateV1
			test.set(&state)
			var encoded [BaseInputBodySize]byte
			if err := EncodeBaseInputBodyInto(encoded[:], state); err != nil {
				t.Fatalf("encode: %v", err)
			}
			gotMask := uint16(encoded[0]) | uint16(encoded[1])<<8
			if gotMask != test.mask {
				t.Fatalf("button mask = 0x%04x, want 0x%04x", gotMask, test.mask)
			}
			decoded, err := DecodeBaseInputBody(encoded[:])
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(decoded, state) {
				t.Fatalf("decoded = %+v, want %+v", decoded, state)
			}
		})
	}
}

func TestBaseInputBodyRequiresExactLength(t *testing.T) {
	for length := 0; length <= BaseInputBodySize*2; length++ {
		if length == BaseInputBodySize {
			continue
		}
		body := make([]byte, length)
		if err := EncodeBaseInputBodyInto(body, InputStateV1{}); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("encode length %d: error = %v, want ErrInvalidLength", length, err)
		}
		if _, err := DecodeBaseInputBody(body); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("decode length %d: error = %v, want ErrInvalidLength", length, err)
		}
	}
}

func TestBaseInputBodyRejectsUnrepresentableControls(t *testing.T) {
	var body [BaseInputBodySize]byte
	tests := []struct {
		name  string
		state InputStateV1
		want  error
	}{
		{name: "guide", state: InputStateV1{Guide: true}, want: ErrGuideRequiresVirtualKey},
		{name: "share", state: InputStateV1{Share: true}, want: ErrShareRequiresExtension},
		{
			name:  "vertical d-pad conflict",
			state: InputStateV1{DPadUp: true, DPadDown: true},
			want:  ErrConflictingDPad,
		},
		{
			name:  "horizontal d-pad conflict",
			state: InputStateV1{DPadLeft: true, DPadRight: true},
			want:  ErrConflictingDPad,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := EncodeBaseInputBodyInto(body[:], test.state)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestBaseInputBodyDecodeFailsClosed(t *testing.T) {
	t.Run("reserved button bit", func(t *testing.T) {
		body := [BaseInputBodySize]byte{0x01}
		if _, err := DecodeBaseInputBody(body[:]); !errors.Is(err, ErrReservedButtonBits) {
			t.Fatalf("error = %v, want ErrReservedButtonBits", err)
		}
	})

	t.Run("opposite d-pad bits", func(t *testing.T) {
		body := [BaseInputBodySize]byte{0x00, 0x03}
		if _, err := DecodeBaseInputBody(body[:]); !errors.Is(err, ErrConflictingDPad) {
			t.Fatalf("error = %v, want ErrConflictingDPad", err)
		}
	})
}

func TestGuideVirtualKeyBody(t *testing.T) {
	for _, down := range []bool{false, true} {
		var body [GuideVirtualKeyBodySize]byte
		if err := EncodeGuideVirtualKeyBodyInto(body[:], GuideEventV1{Down: down}); err != nil {
			t.Fatalf("encode down=%t: %v", down, err)
		}
		wantDown := byte(0)
		if down {
			wantDown = 1
		}
		want := [GuideVirtualKeyBodySize]byte{wantDown, GuideVirtualKeyCode}
		if body != want {
			t.Fatalf("body = % x, want % x", body, want)
		}
		decoded, err := DecodeGuideVirtualKeyBody(body[:])
		if err != nil {
			t.Fatalf("decode down=%t: %v", down, err)
		}
		if decoded.Down != down {
			t.Fatalf("decoded down = %t, want %t", decoded.Down, down)
		}
	}
}

func TestGuideVirtualKeyBodyRejectsMalformedInput(t *testing.T) {
	for length := 0; length <= 5; length++ {
		if length == GuideVirtualKeyBodySize {
			continue
		}
		body := make([]byte, length)
		if err := EncodeGuideVirtualKeyBodyInto(body, GuideEventV1{}); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("encode length %d: error = %v, want ErrInvalidLength", length, err)
		}
		if _, err := DecodeGuideVirtualKeyBody(body); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("decode length %d: error = %v, want ErrInvalidLength", length, err)
		}
	}

	for _, body := range [][GuideVirtualKeyBodySize]byte{
		{2, GuideVirtualKeyCode},
		{1, 0x00},
	} {
		if _, err := DecodeGuideVirtualKeyBody(body[:]); !errors.Is(err, ErrInvalidGuideBody) {
			t.Errorf("body % x: error = %v, want ErrInvalidGuideBody", body, err)
		}
	}
}

func TestBaseInputCodecsAllocateZeroOnSuccess(t *testing.T) {
	state := InputStateV1{A: true, LeftTrigger: 1, RightStickY: -1}
	var body [BaseInputBodySize]byte
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := EncodeBaseInputBodyInto(body[:], state); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("encode allocations = %v, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		if _, err := DecodeBaseInputBody(body[:]); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("decode allocations = %v, want 0", allocs)
	}
}

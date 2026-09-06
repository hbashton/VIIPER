package xboxone

import (
	"errors"
	"reflect"
	"testing"
)

func TestBaseInputBodyGoldenAndRoundTrip(t *testing.T) {
	report := GamepadInputReportV1{
		KeepAlive: true,
		State: InputStateV1{
			Menu:            true,
			A:               true,
			Y:               true,
			DPadUp:          true,
			DPadRight:       true,
			RightBumper:     true,
			LeftStickButton: true,
			LeftTrigger:     0x0234,
			RightTrigger:    0x03cd,
			LeftStickX:      0x0102,
			LeftStickY:      -2,
			RightStickX:     -32768,
			RightStickY:     32767,
		},
	}
	want := [BaseInputBodySize]byte{
		0x96, 0x69,
		0x34, 0x02,
		0xcd, 0x03,
		0x02, 0x01,
		0xfe, 0xff,
		0x00, 0x80,
		0xff, 0x7f,
	}

	var got [BaseInputBodySize]byte
	if err := EncodeBaseInputBodyInto(got[:], report); err != nil {
		t.Fatalf("EncodeBaseInputBodyInto: %v", err)
	}
	if got != want {
		t.Fatalf("encoded body = % x, want % x", got, want)
	}

	decoded, err := DecodeBaseInputBody(got[:])
	if err != nil {
		t.Fatalf("DecodeBaseInputBody: %v", err)
	}
	if !reflect.DeepEqual(decoded, report) {
		t.Fatalf("decoded report = %+v, want %+v", decoded, report)
	}
}

func TestBaseInputButtonBasis(t *testing.T) {
	tests := []struct {
		name string
		mask uint16
		set  func(*GamepadInputReportV1)
	}{
		{name: "keep alive", mask: 1 << 1, set: func(report *GamepadInputReportV1) { report.KeepAlive = true }},
		{name: "menu", mask: 1 << 2, set: func(report *GamepadInputReportV1) { report.State.Menu = true }},
		{name: "view", mask: 1 << 3, set: func(report *GamepadInputReportV1) { report.State.View = true }},
		{name: "a", mask: 1 << 4, set: func(report *GamepadInputReportV1) { report.State.A = true }},
		{name: "b", mask: 1 << 5, set: func(report *GamepadInputReportV1) { report.State.B = true }},
		{name: "x", mask: 1 << 6, set: func(report *GamepadInputReportV1) { report.State.X = true }},
		{name: "y", mask: 1 << 7, set: func(report *GamepadInputReportV1) { report.State.Y = true }},
		{name: "d-pad up", mask: 1 << 8, set: func(report *GamepadInputReportV1) { report.State.DPadUp = true }},
		{name: "d-pad down", mask: 1 << 9, set: func(report *GamepadInputReportV1) { report.State.DPadDown = true }},
		{name: "d-pad left", mask: 1 << 10, set: func(report *GamepadInputReportV1) { report.State.DPadLeft = true }},
		{name: "d-pad right", mask: 1 << 11, set: func(report *GamepadInputReportV1) { report.State.DPadRight = true }},
		{name: "left bumper", mask: 1 << 12, set: func(report *GamepadInputReportV1) { report.State.LeftBumper = true }},
		{name: "right bumper", mask: 1 << 13, set: func(report *GamepadInputReportV1) { report.State.RightBumper = true }},
		{
			name: "left stick button", mask: 1 << 14,
			set: func(report *GamepadInputReportV1) { report.State.LeftStickButton = true },
		},
		{
			name: "right stick button", mask: 1 << 15,
			set: func(report *GamepadInputReportV1) { report.State.RightStickButton = true },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var report GamepadInputReportV1
			test.set(&report)
			var encoded [BaseInputBodySize]byte
			if err := EncodeBaseInputBodyInto(encoded[:], report); err != nil {
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
			if !reflect.DeepEqual(decoded, report) {
				t.Fatalf("decoded = %+v, want %+v", decoded, report)
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
		if err := EncodeBaseInputBodyInto(body, GamepadInputReportV1{}); !errors.Is(err, ErrInvalidLength) {
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
		{name: "guide", state: InputStateV1{Guide: true}, want: ErrGuideRequiresStatusMessage},
		{name: "share", state: InputStateV1{Share: true}, want: ErrShareRequiresExtension},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := EncodeBaseInputBodyInto(body[:], GamepadInputReportV1{State: test.state})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestBaseInputBodyPreservesOpposingDPadBits(t *testing.T) {
	state := InputStateV1{
		DPadUp: true, DPadDown: true, DPadLeft: true, DPadRight: true,
		A: true,
	}
	var body [BaseInputBodySize]byte
	report := GamepadInputReportV1{State: state}
	if err := EncodeBaseInputBodyInto(body[:], report); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got, want := body[:2], []byte{0x10, 0x0f}; !reflect.DeepEqual(got, want) {
		t.Fatalf("button bytes = % x, want % x", got, want)
	}
	decoded, err := DecodeBaseInputBody(body[:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(decoded, report) {
		t.Fatalf("decoded = %+v, want %+v", decoded, report)
	}
}

func TestBaseInputBodyDecodeFailsClosed(t *testing.T) {
	t.Run("reserved button bit", func(t *testing.T) {
		body := [BaseInputBodySize]byte{0x01}
		if _, err := DecodeBaseInputBody(body[:]); !errors.Is(err, ErrReservedButtonBits) {
			t.Fatalf("error = %v, want ErrReservedButtonBits", err)
		}
	})
}

func TestBaseInputBodyRejectsTriggerOutsideTenBits(t *testing.T) {
	var body [BaseInputBodySize]byte
	if err := EncodeBaseInputBodyInto(body[:], GamepadInputReportV1{State: InputStateV1{
		LeftTrigger: 1023, RightTrigger: 1023,
	}}); err != nil {
		t.Fatalf("10-bit maximum must be valid: %v", err)
	}
	for _, state := range []InputStateV1{
		{LeftTrigger: 1024},
		{RightTrigger: 65535},
	} {
		if err := EncodeBaseInputBodyInto(body[:], GamepadInputReportV1{State: state}); !errors.Is(err, ErrTriggerOutOfRange) {
			t.Errorf("encode %+v: error = %v, want ErrTriggerOutOfRange", state, err)
		}
	}

	for _, offset := range []int{2, 4} {
		body = [BaseInputBodySize]byte{}
		body[offset] = 0x00
		body[offset+1] = 0x04
		if _, err := DecodeBaseInputBody(body[:]); !errors.Is(err, ErrTriggerOutOfRange) {
			t.Errorf("decode trigger at offset %d: error = %v, want ErrTriggerOutOfRange",
				offset, err)
		}
	}
}

func TestBaseInputEncodeFailureDoesNotMutateDestination(t *testing.T) {
	want := [BaseInputBodySize]byte{}
	for index := range want {
		want[index] = 0xa5
	}
	got := want
	if err := EncodeBaseInputBodyInto(got[:], GamepadInputReportV1{
		State: InputStateV1{LeftTrigger: 1024},
	}); !errors.Is(err, ErrTriggerOutOfRange) {
		t.Fatalf("error = %v, want ErrTriggerOutOfRange", err)
	}
	if got != want {
		t.Fatalf("destination = % x, want unchanged % x", got, want)
	}
}

func TestBaseInputCodecsAllocateZeroOnSuccess(t *testing.T) {
	report := GamepadInputReportV1{State: InputStateV1{
		A: true, LeftTrigger: 1, RightStickY: -1,
	}}
	var body [BaseInputBodySize]byte
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := EncodeBaseInputBodyInto(body[:], report); err != nil {
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

func TestConsoleFunctionMapGamepadInputShareGoldenAndRoundTrip(t *testing.T) {
	report := GamepadInputReportV1{State: InputStateV1{
		A: true, Share: true, LeftTrigger: 1023, RightTrigger: 17,
		LeftStickX: -32768, LeftStickY: 32767,
		RightStickX: -1, RightStickY: 1,
	}}
	var wire [ConsoleFunctionMapGamepadInputMessageSize]byte
	if err := EncodeConsoleFunctionMapGamepadInputMessageInto(
		wire[:], 0x5a, report); err != nil {
		t.Fatal(err)
	}
	if wire[0] != 0x20 || wire[1] != 0 || wire[2] != 0x5a ||
		wire[3] != ConsoleFunctionMapGamepadInputPayloadSize || wire[18] != 1 {
		t.Fatalf("unexpected Share wire: % x", wire)
	}
	for index, value := range wire[19:] {
		if value != 0 {
			t.Fatalf("function slot %d = 0x%02x", index+2, value)
		}
	}
	sequence, decoded, err := DecodeConsoleFunctionMapGamepadInputMessage(wire[:])
	if err != nil || sequence != 0x5a || decoded != report {
		t.Fatalf("decode = (0x%02x, %+v, %v), want %+v", sequence, decoded, err, report)
	}
}

func TestConsoleFunctionMapGamepadInputRejectsNonCanonicalExtension(t *testing.T) {
	var wire [ConsoleFunctionMapGamepadInputMessageSize]byte
	if err := EncodeConsoleFunctionMapGamepadInputMessageInto(
		wire[:], 1, GamepadInputReportV1{}); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct {
		index int
		value byte
	}{
		{index: 18, value: 2},
		{index: 19, value: 1},
		{index: 35, value: 1},
	} {
		candidate := wire
		candidate[mutation.index] = mutation.value
		if _, _, err := DecodeConsoleFunctionMapGamepadInputMessage(candidate[:]); !errors.Is(err, ErrInvalidConsoleFunctionMap) {
			t.Fatalf("mutation %+v error = %v", mutation, err)
		}
	}
	withGuide := GamepadInputReportV1{State: InputStateV1{Guide: true}}
	if err := EncodeConsoleFunctionMapGamepadInputMessageInto(
		wire[:], 1, withGuide); !errors.Is(err, ErrGuideRequiresStatusMessage) {
		t.Fatalf("Guide error = %v", err)
	}
}

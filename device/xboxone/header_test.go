package xboxone

import (
	"errors"
	"reflect"
	"testing"
)

func TestSinglePacketHeaderGoldenAndRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		header SinglePacketHeader
		want   [SinglePacketHeaderSize]byte
	}{
		{
			name:   "direct motor",
			header: DirectMotorHeader(0x7f),
			want:   [SinglePacketHeaderSize]byte{0x09, 0x00, 0x7f, 0x09},
		},
		{
			name:   "gamepad input",
			header: GamepadInputHeader(1),
			want:   [SinglePacketHeaderSize]byte{0x20, 0x00, 0x01, 0x0e},
		},
		{
			name: "all supported flags",
			header: SinglePacketHeader{
				DataClass:                DataClassCommand,
				MessageNumber:            4,
				System:                   true,
				AcknowledgementRequested: true,
				ExpansionIndex:           7,
				Sequence:                 0xff,
				PayloadLength:            60,
			},
			want: [SinglePacketHeaderSize]byte{0x04, 0x37, 0xff, 0x3c},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got [SinglePacketHeaderSize]byte
			if err := EncodeSinglePacketHeaderInto(got[:], test.header); err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got != test.want {
				t.Fatalf("encoded = % x, want % x", got, test.want)
			}
			decoded, err := DecodeSinglePacketHeader(got[:])
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(decoded, test.header) {
				t.Fatalf("decoded = %+v, want %+v", decoded, test.header)
			}
		})
	}
}

func TestImplementedMessagesHaveByteExactHeaderAndPayload(t *testing.T) {
	t.Run("direct motor", func(t *testing.T) {
		var wire [SinglePacketHeaderSize + RumbleBodySize]byte
		if err := EncodeSinglePacketHeaderInto(
			wire[:SinglePacketHeaderSize], DirectMotorHeader(1)); err != nil {
			t.Fatalf("encode header: %v", err)
		}
		body := RumbleBodyV1{
			Enabled:        MotorAll,
			LeftImpulse:    10,
			RightImpulse:   20,
			LeftVibration:  30,
			RightVibration: 40,
			Duration:       1,
			Delay:          2,
			Repeat:         3,
		}
		if err := EncodeRumbleBodyInto(wire[SinglePacketHeaderSize:], body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
		want := [SinglePacketHeaderSize + RumbleBodySize]byte{
			0x09, 0x00, 0x01, 0x09,
			0x00, 0x0f, 10, 20, 30, 40, 1, 2, 3,
		}
		if wire != want {
			t.Fatalf("message = % x, want % x", wire, want)
		}
	})

	t.Run("gamepad input", func(t *testing.T) {
		var wire [SinglePacketHeaderSize + BaseInputBodySize]byte
		if err := EncodeSinglePacketHeaderInto(
			wire[:SinglePacketHeaderSize], GamepadInputHeader(2)); err != nil {
			t.Fatalf("encode header: %v", err)
		}
		report := GamepadInputReportV1{
			KeepAlive: true,
			State: InputStateV1{
				Menu:         true,
				A:            true,
				DPadUp:       true,
				LeftBumper:   true,
				LeftTrigger:  1023,
				RightTrigger: 1,
				LeftStickX:   -32768,
				LeftStickY:   32767,
				RightStickX:  0,
				RightStickY:  -1,
			},
		}
		if err := EncodeBaseInputBodyInto(wire[SinglePacketHeaderSize:], report); err != nil {
			t.Fatalf("encode body: %v", err)
		}
		want := [SinglePacketHeaderSize + BaseInputBodySize]byte{
			0x20, 0x00, 0x02, 0x0e,
			0x16, 0x11,
			0xff, 0x03,
			0x01, 0x00,
			0x00, 0x80,
			0xff, 0x7f,
			0x00, 0x00,
			0xff, 0xff,
		}
		if wire != want {
			t.Fatalf("message = % x, want % x", wire, want)
		}
	})
}

func TestSinglePacketHeaderRequiresExactLength(t *testing.T) {
	for length := 0; length <= SinglePacketHeaderSize*2; length++ {
		if length == SinglePacketHeaderSize {
			continue
		}
		wire := make([]byte, length)
		if err := EncodeSinglePacketHeaderInto(wire, GamepadInputHeader(1)); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("encode length %d: error = %v, want ErrInvalidLength", length, err)
		}
		if _, err := DecodeSinglePacketHeader(wire); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("decode length %d: error = %v, want ErrInvalidLength", length, err)
		}
	}
}

func TestSinglePacketHeaderRejectsMalformedWire(t *testing.T) {
	tests := []struct {
		name string
		wire [SinglePacketHeaderSize]byte
		want error
	}{
		{name: "fragment", wire: [4]byte{0x20, flagFragment, 1, 0}, want: ErrFragmentedHeader},
		{name: "init fragment without fragment", wire: [4]byte{0x20, flagInitFragment, 1, 0}, want: ErrInvalidInitFragment},
		{name: "reserved flag", wire: [4]byte{0x20, flagReserved, 1, 0}, want: ErrReservedHeaderFlag},
		{name: "sequence zero", wire: [4]byte{0x20, 0, 0, 0}, want: ErrReservedSequence},
		{name: "extended length", wire: [4]byte{0x60, 0, 1, lengthExtended}, want: ErrExtendedPayloadLength},
		{name: "command payload beyond MTU", wire: [4]byte{0x00, 0, 1, 61}, want: ErrPayloadTooLarge},
	}
	for class := byte(4); class < 8; class++ {
		tests = append(tests, struct {
			name string
			wire [SinglePacketHeaderSize]byte
			want error
		}{
			name: "reserved data class",
			wire: [4]byte{class << 5, 0, 1, 0},
			want: ErrReservedDataClass,
		})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeSinglePacketHeader(test.wire[:]); !errors.Is(err, test.want) {
				t.Fatalf("wire % x: error = %v, want %v", test.wire, err, test.want)
			}
		})
	}
}

func TestSinglePacketHeaderEncodeRejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name   string
		header SinglePacketHeader
		want   error
	}{
		{
			name:   "reserved data class",
			header: SinglePacketHeader{DataClass: 4, Sequence: 1},
			want:   ErrReservedDataClass,
		},
		{
			name:   "wide message number",
			header: SinglePacketHeader{MessageNumber: 32, Sequence: 1},
			want:   ErrInvalidMessageNumber,
		},
		{
			name:   "wide expansion index",
			header: SinglePacketHeader{ExpansionIndex: 8, Sequence: 1},
			want:   ErrInvalidExpansionIndex,
		},
		{
			name:   "sequence zero",
			header: SinglePacketHeader{},
			want:   ErrReservedSequence,
		},
		{
			name:   "command payload beyond MTU",
			header: SinglePacketHeader{DataClass: DataClassCommand, Sequence: 1, PayloadLength: 61},
			want:   ErrPayloadTooLarge,
		},
		{
			name:   "low latency payload beyond MTU",
			header: SinglePacketHeader{DataClass: DataClassLowLatency, Sequence: 1, PayloadLength: 61},
			want:   ErrPayloadTooLarge,
		},
		{
			name:   "standard latency payload beyond MTU",
			header: SinglePacketHeader{DataClass: DataClassStandardLatency, Sequence: 1, PayloadLength: 61},
			want:   ErrPayloadTooLarge,
		},
		{
			name:   "audio requires extended length",
			header: SinglePacketHeader{DataClass: DataClassAudio, Sequence: 1, PayloadLength: 128},
			want:   ErrPayloadTooLarge,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var wire [SinglePacketHeaderSize]byte
			if err := EncodeSinglePacketHeaderInto(wire[:], test.header); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestSinglePacketHeaderEncodeFailureDoesNotMutateDestination(t *testing.T) {
	want := [SinglePacketHeaderSize]byte{0xa5, 0xa5, 0xa5, 0xa5}
	got := want
	if err := EncodeSinglePacketHeaderInto(got[:], GamepadInputHeader(0)); !errors.Is(err, ErrReservedSequence) {
		t.Fatalf("error = %v, want ErrReservedSequence", err)
	}
	if got != want {
		t.Fatalf("destination = % x, want unchanged % x", got, want)
	}
}

func TestSinglePacketHeaderDataClassBoundaries(t *testing.T) {
	for _, header := range []SinglePacketHeader{
		{DataClass: DataClassCommand, MessageNumber: 31, Sequence: 1, PayloadLength: 60},
		{DataClass: DataClassLowLatency, MessageNumber: 31, Sequence: 1, PayloadLength: 60},
		{DataClass: DataClassStandardLatency, MessageNumber: 31, Sequence: 1, PayloadLength: 60},
		{DataClass: DataClassAudio, MessageNumber: 31, Sequence: 1, PayloadLength: 127},
	} {
		var wire [SinglePacketHeaderSize]byte
		if err := EncodeSinglePacketHeaderInto(wire[:], header); err != nil {
			t.Errorf("header %+v: %v", header, err)
		}
	}
}

func TestSinglePacketHeaderCodecsAllocateZeroOnSuccess(t *testing.T) {
	header := GamepadInputHeader(1)
	var wire [SinglePacketHeaderSize]byte
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := EncodeSinglePacketHeaderInto(wire[:], header); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("encode allocations = %v, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		if _, err := DecodeSinglePacketHeader(wire[:]); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("decode allocations = %v, want 0", allocs)
	}
}

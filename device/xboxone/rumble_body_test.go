package xboxone

import (
	"errors"
	"reflect"
	"testing"
)

func TestRumbleBodyGoldenAndRoundTrip(t *testing.T) {
	body := RumbleBodyV1{
		Enabled:       MotorAll,
		LeftTrigger:   1,
		RightTrigger:  2,
		LowFrequency:  3,
		HighFrequency: 4,
		Duration:      5,
		Delay:         6,
		Repeat:        7,
	}
	want := [RumbleBodySize]byte{0, 0x0f, 1, 2, 3, 4, 5, 6, 7}

	var got [RumbleBodySize]byte
	if err := EncodeRumbleBodyInto(got[:], body); err != nil {
		t.Fatalf("EncodeRumbleBodyInto: %v", err)
	}
	if got != want {
		t.Fatalf("encoded body = % x, want % x", got, want)
	}
	decoded, err := DecodeRumbleBody(got[:])
	if err != nil {
		t.Fatalf("DecodeRumbleBody: %v", err)
	}
	if !reflect.DeepEqual(decoded, body) {
		t.Fatalf("decoded body = %+v, want %+v", decoded, body)
	}
}

func TestRumbleChannelBasisHasNoBleed(t *testing.T) {
	tests := []struct {
		name      string
		mask      MotorMask
		wireIndex int
		set       func(*RumbleBodyV1)
	}{
		{
			name: "left trigger", mask: MotorLeftTrigger, wireIndex: 2,
			set: func(body *RumbleBodyV1) { body.LeftTrigger = 0x7b },
		},
		{
			name: "right trigger", mask: MotorRightTrigger, wireIndex: 3,
			set: func(body *RumbleBodyV1) { body.RightTrigger = 0x7b },
		},
		{
			name: "low frequency", mask: MotorLowFrequency, wireIndex: 4,
			set: func(body *RumbleBodyV1) { body.LowFrequency = 0x7b },
		},
		{
			name: "high frequency", mask: MotorHighFrequency, wireIndex: 5,
			set: func(body *RumbleBodyV1) { body.HighFrequency = 0x7b },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := RumbleBodyV1{Enabled: test.mask}
			test.set(&body)
			var encoded [RumbleBodySize]byte
			if err := EncodeRumbleBodyInto(encoded[:], body); err != nil {
				t.Fatalf("encode: %v", err)
			}
			for index, value := range encoded {
				want := byte(0)
				switch index {
				case 1:
					want = byte(test.mask)
				case test.wireIndex:
					want = 0x7b
				}
				if value != want {
					t.Errorf("wire[%d] = 0x%02x, want 0x%02x", index, value, want)
				}
			}
			decoded, err := DecodeRumbleBody(encoded[:])
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(decoded, body) {
				t.Fatalf("decoded = %+v, want %+v", decoded, body)
			}
		})
	}
}

func TestRumbleBodyRejectsMagnitudeOnDisabledChannel(t *testing.T) {
	tests := []RumbleBodyV1{
		{LeftTrigger: 1},
		{RightTrigger: 1},
		{LowFrequency: 1},
		{HighFrequency: 1},
	}
	for _, body := range tests {
		var encoded [RumbleBodySize]byte
		if err := EncodeRumbleBodyInto(encoded[:], body); !errors.Is(err, ErrDisabledMotorMagnitude) {
			t.Errorf("body %+v: error = %v, want ErrDisabledMotorMagnitude", body, err)
		}
	}
}

func TestPinnedStopRumbleBody(t *testing.T) {
	stop := NewPinnedStopRumbleBody()
	if !stop.IsExplicitStop() {
		t.Fatal("pinned stop was not recognized as an explicit stop")
	}
	if (RumbleBodyV1{}).IsExplicitStop() {
		t.Fatal("zero enable mask must not be treated as an explicit stop")
	}

	want := [RumbleBodySize]byte{0, 0x0f, 0, 0, 0, 0, 0xff, 0, 0xeb}
	var got [RumbleBodySize]byte
	if err := EncodeRumbleBodyInto(got[:], stop); err != nil {
		t.Fatalf("encode stop: %v", err)
	}
	if got != want {
		t.Fatalf("encoded stop = % x, want % x", got, want)
	}
}

func TestRumbleBodyRequiresExactLength(t *testing.T) {
	for length := 0; length <= RumbleBodySize*2; length++ {
		if length == RumbleBodySize {
			continue
		}
		body := make([]byte, length)
		if err := EncodeRumbleBodyInto(body, RumbleBodyV1{}); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("encode length %d: error = %v, want ErrInvalidLength", length, err)
		}
		if _, err := DecodeRumbleBody(body); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("decode length %d: error = %v, want ErrInvalidLength", length, err)
		}
	}
}

func TestRumbleBodyRejectsUnknownWireFields(t *testing.T) {
	tests := []struct {
		name string
		body [RumbleBodySize]byte
		want error
	}{
		{name: "reserved byte", body: [RumbleBodySize]byte{0: 1}, want: ErrReservedRumbleField},
		{name: "unknown mask", body: [RumbleBodySize]byte{1: 0x10}, want: ErrInvalidMotorMask},
		{
			name: "disabled channel magnitude",
			body: [RumbleBodySize]byte{2: 1},
			want: ErrDisabledMotorMagnitude,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeRumbleBody(test.body[:]); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestRumbleCodecsAllocateZeroOnSuccess(t *testing.T) {
	body := RumbleBodyV1{Enabled: MotorLowFrequency, LowFrequency: 1}
	var encoded [RumbleBodySize]byte
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := EncodeRumbleBodyInto(encoded[:], body); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("encode allocations = %v, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		if _, err := DecodeRumbleBody(encoded[:]); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("decode allocations = %v, want 0", allocs)
	}
}

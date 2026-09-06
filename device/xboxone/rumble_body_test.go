package xboxone

import (
	"errors"
	"reflect"
	"testing"
)

func TestRumbleBodyGoldenAndRoundTrip(t *testing.T) {
	body := RumbleBodyV1{
		Enabled:        MotorAll,
		LeftImpulse:    1,
		RightImpulse:   2,
		LeftVibration:  3,
		RightVibration: 4,
		Duration:       5,
		Delay:          6,
		Repeat:         7,
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
			name: "left impulse", mask: MotorLeftImpulse, wireIndex: 2,
			set: func(body *RumbleBodyV1) { body.LeftImpulse = 0x64 },
		},
		{
			name: "right impulse", mask: MotorRightImpulse, wireIndex: 3,
			set: func(body *RumbleBodyV1) { body.RightImpulse = 0x64 },
		},
		{
			name: "left vibration", mask: MotorLeftVibration, wireIndex: 4,
			set: func(body *RumbleBodyV1) { body.LeftVibration = 0x64 },
		},
		{
			name: "right vibration", mask: MotorRightVibration, wireIndex: 5,
			set: func(body *RumbleBodyV1) { body.RightVibration = 0x64 },
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
					want = 0x64
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

func TestRumbleBodyDoesNotInventDisabledLevelConstraint(t *testing.T) {
	body := RumbleBodyV1{LeftImpulse: 100, Duration: 1}
	var encoded [RumbleBodySize]byte
	if err := EncodeRumbleBodyInto(encoded[:], body); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got, want := encoded, ([RumbleBodySize]byte{2: 100, 6: 1}); got != want {
		t.Fatalf("encoded = % x, want % x", got, want)
	}
}

func TestDirectMotorDurationZeroCancelsAllMotors(t *testing.T) {
	stop := NewStopRumbleBody()
	if !stop.IsCancellation() {
		t.Fatal("zero-duration cancellation was not recognized")
	}
	if !stop.IsCanonicalImmediateStop() {
		t.Fatal("zero-valued stop must be canonical immediate stop")
	}
	if (RumbleBodyV1{Duration: 1}).IsCancellation() {
		t.Fatal("non-zero duration must not be treated as cancellation")
	}
	for _, cancellation := range []RumbleBodyV1{{Delay: 1}, {Repeat: 1}, {Delay: 1, Repeat: 1}} {
		if !cancellation.IsCancellation() {
			t.Fatalf("valid duration-zero body %+v must be cancellation", cancellation)
		}
		if cancellation.IsCanonicalImmediateStop() {
			t.Fatalf("noncanonical cancellation %+v must not be canonical immediate stop", cancellation)
		}
	}

	want := [RumbleBodySize]byte{}
	var got [RumbleBodySize]byte
	if err := EncodeRumbleBodyInto(got[:], stop); err != nil {
		t.Fatalf("encode stop: %v", err)
	}
	if got != want {
		t.Fatalf("encoded stop = % x, want % x", got, want)
	}

	// The specification says levels are ignored when duration is zero, so a
	// valid non-zero level does not change cancellation semantics.
	ignoredLevels := RumbleBodyV1{LeftImpulse: 100}
	if !ignoredLevels.IsCancellation() {
		t.Fatal("valid ignored levels must still be recognized as cancellation")
	}
	if invalid := (RumbleBodyV1{LeftImpulse: 101}); invalid.IsCancellation() {
		t.Fatal("a malformed body must never be classified as cancellation")
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
		{name: "command byte", body: [RumbleBodySize]byte{0: 1}, want: ErrInvalidDirectMotorCommand},
		{name: "unknown mask", body: [RumbleBodySize]byte{1: 0x10}, want: ErrInvalidMotorMask},
		{
			name: "motor level over 100 percent",
			body: [RumbleBodySize]byte{2: 101},
			want: ErrMotorLevelOutOfRange,
		},
		{
			name: "right impulse over 100 percent",
			body: [RumbleBodySize]byte{3: 101},
			want: ErrMotorLevelOutOfRange,
		},
		{
			name: "left vibration over 100 percent",
			body: [RumbleBodySize]byte{4: 101},
			want: ErrMotorLevelOutOfRange,
		},
		{
			name: "right vibration over 100 percent",
			body: [RumbleBodySize]byte{5: 101},
			want: ErrMotorLevelOutOfRange,
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

func TestRumbleEncodeFailureDoesNotMutateDestination(t *testing.T) {
	want := [RumbleBodySize]byte{0xa5, 0xa5, 0xa5, 0xa5, 0xa5, 0xa5, 0xa5, 0xa5, 0xa5}
	got := want
	if err := EncodeRumbleBodyInto(got[:], RumbleBodyV1{LeftImpulse: 101}); !errors.Is(err, ErrMotorLevelOutOfRange) {
		t.Fatalf("error = %v, want ErrMotorLevelOutOfRange", err)
	}
	if got != want {
		t.Fatalf("destination = % x, want unchanged % x", got, want)
	}
}

func TestRumbleCodecsAllocateZeroOnSuccess(t *testing.T) {
	body := RumbleBodyV1{Enabled: MotorLeftVibration, LeftVibration: 1, Duration: 1}
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

package xboxone

import (
	"errors"
	"testing"
)

func testGamepadInputReport() GamepadInputReportV1 {
	return GamepadInputReportV1{
		KeepAlive: true,
		State: InputStateV1{
			Menu: true, View: true,
			A: true, B: true, X: true, Y: true,
			DPadUp: true, DPadDown: true, DPadLeft: true, DPadRight: true,
			LeftBumper: true, RightBumper: true,
			LeftStickButton: true, RightStickButton: true,
			LeftTrigger: 1023, RightTrigger: 0x0123,
			LeftStickX: -32768, LeftStickY: 32767,
			RightStickX: -1, RightStickY: 0x1234,
		},
	}
}

func testDirectMotorBody() RumbleBodyV1 {
	return RumbleBodyV1{
		Enabled:     MotorAll,
		LeftImpulse: 0x0b, RightImpulse: 0x16,
		LeftVibration: 0x21, RightVibration: 0x2c,
		Duration: 0x37, Delay: 0x42, Repeat: 0x4d,
	}
}

func TestGamepadInputMessageGoldenRoundTrip(t *testing.T) {
	want := [GamepadInputMessageSize]byte{
		0x20, 0x00, 0xa5, 0x0e,
		0xfe, 0xff, 0xff, 0x03, 0x23, 0x01,
		0x00, 0x80, 0xff, 0x7f, 0xff, 0xff, 0x34, 0x12,
	}
	var got [GamepadInputMessageSize]byte
	if err := EncodeGamepadInputMessageInto(got[:], 0xa5, testGamepadInputReport()); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got != want {
		t.Fatalf("message = % x, want % x", got, want)
	}
	sequence, report, err := DecodeGamepadInputMessage(got[:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sequence != 0xa5 || report != testGamepadInputReport() {
		t.Fatalf("decoded sequence/report = %02x/%+v", sequence, report)
	}
}

func TestDirectMotorMessageGoldenRoundTrip(t *testing.T) {
	want := [DirectMotorMessageSize]byte{
		0x09, 0x00, 0x5a, 0x09,
		0x00, 0x0f, 0x0b, 0x16, 0x21, 0x2c, 0x37, 0x42, 0x4d,
	}
	var got [DirectMotorMessageSize]byte
	if err := EncodeDirectMotorMessageInto(got[:], 0x5a, testDirectMotorBody()); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got != want {
		t.Fatalf("message = % x, want % x", got, want)
	}
	sequence, body, err := DecodeDirectMotorMessage(got[:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sequence != 0x5a || body != testDirectMotorBody() {
		t.Fatalf("decoded sequence/body = %02x/%+v", sequence, body)
	}
}

func TestMessageEncodersAreAtomic(t *testing.T) {
	t.Run("gamepad", func(t *testing.T) {
		original := [GamepadInputMessageSize]byte{}
		for index := range original {
			original[index] = 0xa5
		}
		tests := []struct {
			name     string
			sequence uint8
			report   GamepadInputReportV1
			want     error
		}{
			{name: "reserved sequence", sequence: 0, report: testGamepadInputReport(), want: ErrReservedSequence},
			{name: "guide", sequence: 1, report: func() GamepadInputReportV1 { value := testGamepadInputReport(); value.State.Guide = true; return value }(), want: ErrGuideRequiresStatusMessage},
			{name: "share", sequence: 1, report: func() GamepadInputReportV1 { value := testGamepadInputReport(); value.State.Share = true; return value }(), want: ErrShareRequiresExtension},
			{name: "trigger", sequence: 1, report: func() GamepadInputReportV1 {
				value := testGamepadInputReport()
				value.State.LeftTrigger = 1024
				return value
			}(), want: ErrTriggerOutOfRange},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				got := original
				if err := EncodeGamepadInputMessageInto(got[:], test.sequence, test.report); !errors.Is(err, test.want) {
					t.Fatalf("error = %v, want %v", err, test.want)
				}
				if got != original {
					t.Fatalf("destination changed: % x", got)
				}
			})
		}
	})

	t.Run("direct motor", func(t *testing.T) {
		original := [DirectMotorMessageSize]byte{}
		for index := range original {
			original[index] = 0x5a
		}
		tests := []struct {
			name     string
			sequence uint8
			body     RumbleBodyV1
			want     error
		}{
			{name: "reserved sequence", sequence: 0, body: testDirectMotorBody(), want: ErrReservedSequence},
			{name: "mask", sequence: 1, body: func() RumbleBodyV1 { value := testDirectMotorBody(); value.Enabled = 0x80; return value }(), want: ErrInvalidMotorMask},
			{name: "level", sequence: 1, body: func() RumbleBodyV1 { value := testDirectMotorBody(); value.LeftImpulse = 101; return value }(), want: ErrMotorLevelOutOfRange},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				got := original
				if err := EncodeDirectMotorMessageInto(got[:], test.sequence, test.body); !errors.Is(err, test.want) {
					t.Fatalf("error = %v, want %v", err, test.want)
				}
				if got != original {
					t.Fatalf("destination changed: % x", got)
				}
			})
		}
	})
}

func TestMessageCodecsRequireExactLength(t *testing.T) {
	for _, size := range []int{GamepadInputMessageSize - 1, GamepadInputMessageSize + 1} {
		if err := EncodeGamepadInputMessageInto(make([]byte, size), 1, GamepadInputReportV1{}); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("gamepad encode length %d: %v", size, err)
		}
		if _, _, err := DecodeGamepadInputMessage(make([]byte, size)); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("gamepad decode length %d: %v", size, err)
		}
	}
	for _, size := range []int{DirectMotorMessageSize - 1, DirectMotorMessageSize + 1} {
		if err := EncodeDirectMotorMessageInto(make([]byte, size), 1, RumbleBodyV1{}); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("motor encode length %d: %v", size, err)
		}
		if _, _, err := DecodeDirectMotorMessage(make([]byte, size)); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("motor decode length %d: %v", size, err)
		}
	}
}

func TestGamepadInputMessageHeaderGrammarIsExhaustive(t *testing.T) {
	assertExactMessageHeaderGrammar(
		t,
		func() []byte {
			wire := make([]byte, GamepadInputMessageSize)
			if err := EncodeGamepadInputMessageInto(wire, 1, GamepadInputReportV1{}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			return wire
		},
		0x20,
		0x00,
		BaseInputBodySize,
		func(wire []byte) error {
			_, _, err := DecodeGamepadInputMessage(wire)
			return err
		},
	)
}

func TestDirectMotorMessageHeaderGrammarIsExhaustive(t *testing.T) {
	assertExactMessageHeaderGrammar(
		t,
		func() []byte {
			wire := make([]byte, DirectMotorMessageSize)
			if err := EncodeDirectMotorMessageInto(wire, 1, RumbleBodyV1{}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			return wire
		},
		0x09,
		0x00,
		RumbleBodySize,
		func(wire []byte) error {
			_, _, err := DecodeDirectMotorMessage(wire)
			return err
		},
	)
}

func assertExactMessageHeaderGrammar(
	t *testing.T,
	seed func() []byte,
	wantType byte,
	wantFlags byte,
	wantLength byte,
	decode func([]byte) error,
) {
	t.Helper()
	fields := []struct {
		name       string
		index      int
		want       byte
		allowOther bool
	}{
		{name: "message type", index: 0, want: wantType},
		{name: "flags", index: 1, want: wantFlags},
		{name: "sequence", index: 2, want: 1, allowOther: true},
		{name: "payload length", index: 3, want: wantLength},
	}
	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			for value := 0; value <= 0xff; value++ {
				wire := seed()
				wire[field.index] = byte(value)
				err := decode(wire)
				accepted := value == int(field.want)
				if field.allowOther {
					accepted = value != 0
				}
				if accepted && err != nil {
					t.Fatalf("value 0x%02x rejected: %v", value, err)
				}
				if !accepted && err == nil {
					t.Fatalf("value 0x%02x accepted", value)
				}
			}
		})
	}
}

func TestMessageCodecsAllocateZero(t *testing.T) {
	gamepadReport := testGamepadInputReport()
	gamepadWire := [GamepadInputMessageSize]byte{}
	motorBody := testDirectMotorBody()
	motorWire := [DirectMotorMessageSize]byte{}
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := EncodeGamepadInputMessageInto(gamepadWire[:], 1, gamepadReport); err != nil {
			panic(err)
		}
		if _, _, err := DecodeGamepadInputMessage(gamepadWire[:]); err != nil {
			panic(err)
		}
		if err := EncodeDirectMotorMessageInto(motorWire[:], 1, motorBody); err != nil {
			panic(err)
		}
		if _, _, err := DecodeDirectMotorMessage(motorWire[:]); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("message codec allocations = %v, want 0", allocs)
	}
}

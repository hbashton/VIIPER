package xboxone

import (
	"errors"
	"testing"
)

func TestExtendedStatusNoEventsGoldenMessages(t *testing.T) {
	tests := []struct {
		name     string
		sequence uint8
		body     ExtendedStatusNoEventsBodyV1
		want     [ExtendedStatusNoEventsMessageSize]byte
	}{
		{
			name:     "wired no battery full power",
			sequence: 1,
			body:     NewWiredNoBatteryStatus(false),
			want: [ExtendedStatusNoEventsMessageSize]byte{
				0x03, 0x20, 0x01, 0x04, 0x80, 0x00, 0x00, 0x00,
			},
		},
		{
			name:     "wired no battery powering off",
			sequence: 0xff,
			body:     NewWiredNoBatteryStatus(true),
			want: [ExtendedStatusNoEventsMessageSize]byte{
				0x03, 0x20, 0xff, 0x04, 0x00, 0x00, 0x00, 0x00,
			},
		},
		{
			name:     "charge error rechargeable full active",
			sequence: 0x7a,
			body: ExtendedStatusNoEventsBodyV1{
				PowerLevel:   StatusFullPower,
				ChargeState:  StatusChargeError,
				BatteryType:  StatusBatteryRechargeable,
				BatteryLevel: StatusBatteryFull,
				DeviceActive: true,
			},
			want: [ExtendedStatusNoEventsMessageSize]byte{
				0x03, 0x20, 0x7a, 0x04, 0xab, 0x01, 0x00, 0x00,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got [ExtendedStatusNoEventsMessageSize]byte
			if err := EncodeExtendedStatusNoEventsMessageInto(
				got[:], test.sequence, test.body); err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got != test.want {
				t.Fatalf("message = % x, want % x", got, test.want)
			}
			sequence, body, err := DecodeExtendedStatusNoEventsMessage(got[:])
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if sequence != test.sequence || body != test.body {
				t.Fatalf("round trip = (%d, %+v), want (%d, %+v)",
					sequence, body, test.sequence, test.body)
			}
		})
	}
}

func TestExtendedStatusNoEventsBodyExhaustiveByteGrammar(t *testing.T) {
	for status := 0; status <= 0xff; status++ {
		for extended := 0; extended <= 0xff; extended++ {
			wire := [ExtendedStatusNoEventsBodySize]byte{byte(status), byte(extended), 0, 0}
			body, err := DecodeExtendedStatusNoEventsBody(wire[:])
			power := status >> 6
			charge := status >> 4 & 0x03
			batteryType := status >> 2 & 0x03
			wantValid := (power == 0 || power == 2) && charge != 3 &&
				batteryType != 3 && (extended == 0 || extended == 1)
			if !wantValid {
				if err == nil {
					t.Fatalf("accepted invalid body % x as %+v", wire, body)
				}
				continue
			}
			if err != nil {
				t.Fatalf("rejected valid body % x: %v", wire, err)
			}
			var encoded [ExtendedStatusNoEventsBodySize]byte
			if err := EncodeExtendedStatusNoEventsBodyInto(encoded[:], body); err != nil {
				t.Fatalf("re-encode % x: %v", wire, err)
			}
			if encoded != wire {
				t.Fatalf("round trip = % x, want % x", encoded, wire)
			}
		}
	}
}

func TestExtendedStatusNoEventsRejectsReservedAndEventForms(t *testing.T) {
	tests := []struct {
		name string
		wire [ExtendedStatusNoEventsBodySize]byte
		want error
	}{
		{name: "deprecated standby", wire: [4]byte{0x40, 0, 0, 0}, want: ErrReservedStatusValue},
		{name: "reserved power", wire: [4]byte{0xc0, 0, 0, 0}, want: ErrReservedStatusValue},
		{name: "reserved charge", wire: [4]byte{0xb0, 0, 0, 0}, want: ErrReservedStatusValue},
		{name: "reserved battery type", wire: [4]byte{0x8c, 0, 0, 0}, want: ErrReservedStatusValue},
		{name: "events present", wire: [4]byte{0x80, 0x02, 0, 0}, want: ErrUnsupportedStatusEvents},
		{name: "reserved extended bit", wire: [4]byte{0x80, 0x04, 0, 0}, want: ErrReservedExtendedStatusFlag},
		{name: "reserved byte two", wire: [4]byte{0x80, 0, 1, 0}, want: ErrReservedExtendedStatusFlag},
		{name: "reserved byte three", wire: [4]byte{0x80, 0, 0, 1}, want: ErrReservedExtendedStatusFlag},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeExtendedStatusNoEventsBody(test.wire[:]); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestExtendedStatusNoEventsMessageRejectsWrongEnvelope(t *testing.T) {
	valid := [ExtendedStatusNoEventsMessageSize]byte{
		0x03, 0x20, 0x01, 0x04, 0x80, 0, 0, 0,
	}
	tests := []struct {
		name  string
		index int
		value byte
		want  error
	}{
		{name: "wrong message", index: 0, value: 0x02, want: ErrInvalidStatusMessage},
		{name: "wrong class", index: 0, value: 0x23, want: ErrInvalidStatusMessage},
		{name: "not system", index: 1, value: 0x00, want: ErrInvalidStatusMessage},
		{name: "ack requested", index: 1, value: 0x30, want: ErrInvalidStatusMessage},
		{name: "secondary", index: 1, value: 0x21, want: ErrInvalidStatusMessage},
		{name: "wrong length", index: 3, value: 0x03, want: ErrInvalidStatusMessage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wire := valid
			wire[test.index] = test.value
			if _, _, err := DecodeExtendedStatusNoEventsMessage(wire[:]); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestExtendedStatusNoEventsExactLengthsAndAtomicFailure(t *testing.T) {
	body := NewWiredNoBatteryStatus(false)
	for _, size := range []int{
		ExtendedStatusNoEventsBodySize - 1,
		ExtendedStatusNoEventsBodySize + 1,
	} {
		if err := EncodeExtendedStatusNoEventsBodyInto(make([]byte, size), body); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("body encode length %d: %v", size, err)
		}
		if _, err := DecodeExtendedStatusNoEventsBody(make([]byte, size)); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("body decode length %d: %v", size, err)
		}
	}
	for _, size := range []int{
		ExtendedStatusNoEventsMessageSize - 1,
		ExtendedStatusNoEventsMessageSize + 1,
	} {
		if err := EncodeExtendedStatusNoEventsMessageInto(make([]byte, size), 1, body); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("message encode length %d: %v", size, err)
		}
		if _, _, err := DecodeExtendedStatusNoEventsMessage(make([]byte, size)); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("message decode length %d: %v", size, err)
		}
	}

	want := [ExtendedStatusNoEventsMessageSize]byte{}
	for index := range want {
		want[index] = 0xa5
	}
	got := want
	body.PowerLevel = 1
	if err := EncodeExtendedStatusNoEventsMessageInto(got[:], 1, body); !errors.Is(err, ErrReservedStatusValue) {
		t.Fatalf("invalid body error = %v", err)
	}
	if got != want {
		t.Fatalf("invalid encode mutated destination: % x", got)
	}
	if err := EncodeExtendedStatusNoEventsMessageInto(got[:], 0, NewWiredNoBatteryStatus(false)); !errors.Is(err, ErrReservedSequence) {
		t.Fatalf("zero sequence error = %v", err)
	}
	if got != want {
		t.Fatalf("zero sequence mutated destination: % x", got)
	}
}

func TestExtendedStatusNoEventsHotPathAllocatesZero(t *testing.T) {
	body := ExtendedStatusNoEventsBodyV1{
		PowerLevel:   StatusFullPower,
		ChargeState:  StatusCharging,
		BatteryType:  StatusBatteryRechargeable,
		BatteryLevel: StatusBatteryMedium,
	}
	var wire [ExtendedStatusNoEventsMessageSize]byte
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := EncodeExtendedStatusNoEventsMessageInto(wire[:], 1, body); err != nil {
			panic(err)
		}
		if _, _, err := DecodeExtendedStatusNoEventsMessage(wire[:]); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("extended status hot-path allocations = %v, want 0", allocs)
	}
}

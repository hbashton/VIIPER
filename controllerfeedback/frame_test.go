package controllerfeedback

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"strconv"
	"testing"
)

func TestWireEnumValuesRemainStable(t *testing.T) {
	tests := []struct {
		name string
		got  uint8
		want uint8
	}{
		{"source xbox one", uint8(SourceXboxOneVirtualDevice), 1},
		{"source xbox series", uint8(SourceXboxSeriesVirtualDevice), 2},
		{"source xbox 360", uint8(SourceXbox360VirtualDevice), 3},
		{"source dualsense", uint8(SourceDualSenseVirtualDevice), 4},
		{"source dualsense edge", uint8(SourceDualSenseEdgeVirtualDevice), 5},
		{"source dualshock 4", uint8(SourceDualShock4VirtualDevice), 6},
		{"command apply", uint8(CommandApply), 1},
		{"command neutral", uint8(CommandNeutral), 2},
		{"command stop", uint8(CommandStop), 3},
		{"actuator body low", uint8(ActuatorBodyLow), 0x01},
		{"actuator body high", uint8(ActuatorBodyHigh), 0x02},
		{"actuator left trigger", uint8(ActuatorLeftTrigger), 0x04},
		{"actuator right trigger", uint8(ActuatorRightTrigger), 0x08},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("wire value = %#x, want %#x", test.got, test.want)
			}
		})
	}
}

type goldenFixture struct {
	Contract               string `json:"contract"`
	Version                uint16 `json:"version"`
	Source                 uint8  `json:"source"`
	Command                uint8  `json:"command"`
	Actuators              uint8  `json:"actuators"`
	BodyLow                uint16 `json:"body_low"`
	BodyHigh               uint16 `json:"body_high"`
	LeftTrigger            uint16 `json:"left_trigger"`
	RightTrigger           uint16 `json:"right_trigger"`
	Sequence               string `json:"sequence"`
	DeviceGeneration       string `json:"device_generation"`
	TransportGeneration    string `json:"transport_generation"`
	OwnershipEpoch         string `json:"ownership_epoch"`
	TimestampMicroseconds  string `json:"timestamp_microseconds"`
	TimeToLiveMicroseconds string `json:"time_to_live_microseconds"`
	WireHex                string `json:"wire_hex"`
}

func TestDS4WindowsGoldenVector(t *testing.T) {
	raw, err := os.ReadFile("testdata/cfbk-v1-ds4windows-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture goldenFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Contract != "CFBK" {
		t.Fatalf("contract = %q, want CFBK", fixture.Contract)
	}

	wantBytes, err := hex.DecodeString(fixture.WireHex)
	if err != nil {
		t.Fatal(err)
	}
	if len(wantBytes) != FrameSize {
		t.Fatalf("golden length = %d, want %d", len(wantBytes), FrameSize)
	}
	want := Frame{
		Version:                fixture.Version,
		Source:                 Source(fixture.Source),
		Command:                Command(fixture.Command),
		Actuators:              ActuatorMask(fixture.Actuators),
		BodyLow:                fixture.BodyLow,
		BodyHigh:               fixture.BodyHigh,
		LeftTrigger:            fixture.LeftTrigger,
		RightTrigger:           fixture.RightTrigger,
		Sequence:               fixtureUint64(t, fixture.Sequence),
		DeviceGeneration:       fixtureUint64(t, fixture.DeviceGeneration),
		TransportGeneration:    fixtureUint64(t, fixture.TransportGeneration),
		OwnershipEpoch:         fixtureUint64(t, fixture.OwnershipEpoch),
		TimestampMicroseconds:  fixtureUint64(t, fixture.TimestampMicroseconds),
		TimeToLiveMicroseconds: fixtureUint64(t, fixture.TimeToLiveMicroseconds),
	}

	var encoded [FrameSize]byte
	if err := want.MarshalTo(encoded[:]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded[:], wantBytes) {
		t.Fatalf("VIIPER bytes = %x\nDS4Windows golden = %x", encoded, wantBytes)
	}

	var decoded Frame
	if err := decoded.UnmarshalFrom(wantBytes); err != nil {
		t.Fatal(err)
	}
	if decoded != want {
		t.Fatalf("decoded = %+v\nwant = %+v", decoded, want)
	}
}

func fixtureUint64(t *testing.T, value string) uint64 {
	t.Helper()
	parsed, err := strconv.ParseUint(value, 0, 64)
	if err != nil {
		t.Fatalf("parse fixture value %q: %v", value, err)
	}
	return parsed
}

func TestMarshalToUsesFixedPrefixAndPreservesDestinationTail(t *testing.T) {
	frame := testFrame()
	destination := bytes.Repeat([]byte{0xA5}, FrameSize+8)
	if err := frame.MarshalTo(destination); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint16(destination[6:8]); got != FrameSize {
		t.Fatalf("encoded size = %d, want %d", got, FrameSize)
	}
	if !bytes.Equal(destination[FrameSize:], bytes.Repeat([]byte{0xA5}, 8)) {
		t.Fatal("marshal changed bytes outside the fixed frame")
	}
}

func TestInvalidMarshalDoesNotMutateDestination(t *testing.T) {
	frame := testFrame()
	frame.Version = 2
	destination := bytes.Repeat([]byte{0xA5}, FrameSize)
	before := append([]byte(nil), destination...)
	if err := frame.MarshalTo(destination); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("error = %v, want ErrInvalidVersion", err)
	}
	if !bytes.Equal(destination, before) {
		t.Fatal("invalid marshal changed destination")
	}
	if err := testFrame().MarshalTo(destination[:FrameSize-1]); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("short destination error = %v, want ErrInvalidSize", err)
	}
}

func TestUnmarshalRejectsMalformedFramesAndClearsReceiver(t *testing.T) {
	var valid [FrameSize]byte
	if err := testFrame().MarshalTo(valid[:]); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"short", append([]byte(nil), valid[:FrameSize-1]...), ErrInvalidSize},
		{"long", append(append([]byte(nil), valid[:]...), 0), ErrInvalidSize},
		{"magic", mutate(valid, 0, 0), ErrInvalidMagic},
		{"version", mutate(valid, 4, 2), ErrInvalidVersion},
		{"encoded length", mutate(valid, 6, FrameSize-1), ErrInvalidSize},
		{"reserved header", mutate(valid, 11, 1), ErrReservedNonZero},
		{"reserved body 0", mutate(valid, 20, 1), ErrReservedNonZero},
		{"reserved body 1", mutate(valid, 21, 1), ErrReservedNonZero},
		{"reserved body 2", mutate(valid, 22, 1), ErrReservedNonZero},
		{"reserved body 3", mutate(valid, 23, 1), ErrReservedNonZero},
		{"source zero", mutate(valid, 8, 0), ErrInvalidSource},
		{"source unknown", mutate(valid, 8, 7), ErrInvalidSource},
		{"command zero", mutate(valid, 9, 0), ErrInvalidCommand},
		{"command unknown", mutate(valid, 9, 4), ErrInvalidCommand},
		{"actuators empty", mutate(valid, 10, 0), ErrInvalidActuators},
		{"actuators unknown", mutate(valid, 10, 0x10), ErrInvalidActuators},
		{"out of mask amplitude", mutate(valid, 10, byte(ActuatorBodyLow)), ErrInvalidAmplitude},
		{"apply is all zero", zeroAmplitudes(valid), ErrInvalidAmplitude},
		{"neutral is nonzero", mutate(valid, 9, byte(CommandNeutral)), ErrInvalidAmplitude},
		{"stop is nonzero", mutate(valid, 9, byte(CommandStop)), ErrInvalidAmplitude},
		{"sequence zero", zeroRange(valid, 24, 32), ErrInvalidSequence},
		{"device generation zero", zeroRange(valid, 32, 40), ErrInvalidGeneration},
		{"transport generation zero", zeroRange(valid, 40, 48), ErrInvalidGeneration},
		{"ownership epoch zero", zeroRange(valid, 48, 56), ErrInvalidGeneration},
		{"ttl zero", zeroRange(valid, 64, 72), ErrInvalidTTL},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoded := testFrame()
			err := decoded.UnmarshalFrom(test.data)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if decoded != (Frame{}) {
				t.Fatalf("failed decode retained state: %+v", decoded)
			}
		})
	}

	var nilFrame *Frame
	if err := nilFrame.UnmarshalFrom(valid[:]); !errors.Is(err, ErrNilFrame) {
		t.Fatalf("nil receiver error = %v, want ErrNilFrame", err)
	}
}

func TestFrameValidationInvariants(t *testing.T) {
	neutral := testFrame()
	neutral.Command = CommandNeutral
	neutral.BodyLow = 0
	neutral.BodyHigh = 0
	neutral.LeftTrigger = 0
	neutral.RightTrigger = 0
	if err := neutral.Validate(); err != nil || !neutral.IsNeutral() || neutral.IsStop() {
		t.Fatalf("neutral = %+v error=%v", neutral, err)
	}
	stop := neutral
	stop.Command = CommandStop
	if err := stop.Validate(); err != nil || !stop.IsStop() || stop.IsNeutral() {
		t.Fatalf("stop = %+v error=%v", stop, err)
	}
	if (Frame{}).Valid() {
		t.Fatal("zero frame is valid")
	}
}

func TestExpiryUsesInclusiveBoundaryWithoutOverflow(t *testing.T) {
	frame := testFrame()
	frame.TimestampMicroseconds = 1_000
	frame.TimeToLiveMicroseconds = 250
	if frame.ExpiredAt(999) || frame.ExpiredAt(1_249) || !frame.ExpiredAt(1_250) {
		t.Fatal("ordinary expiry boundary is incorrect")
	}
	frame.TimestampMicroseconds = ^uint64(0) - 20
	frame.TimeToLiveMicroseconds = 10
	if frame.ExpiredAt(5) || frame.ExpiredAt(^uint64(0)-11) ||
		!frame.ExpiredAt(^uint64(0)-10) {
		t.Fatal("near-limit expiry wrapped")
	}
}

func TestFrameRoundTripProperties(t *testing.T) {
	random := rand.New(rand.NewSource(0xCFB1))
	for iteration := 0; iteration < 10_000; iteration++ {
		mask := ActuatorMask(random.Intn(int(ActuatorAll)) + 1)
		command := Command(random.Intn(int(CommandStop)) + 1)
		frame := Frame{
			Version:                Version1,
			Source:                 Source(random.Intn(int(SourceDualShock4VirtualDevice)) + 1),
			Command:                command,
			Actuators:              mask,
			Sequence:               random.Uint64() | 1,
			DeviceGeneration:       random.Uint64() | 1,
			TransportGeneration:    random.Uint64() | 1,
			OwnershipEpoch:         random.Uint64() | 1,
			TimestampMicroseconds:  random.Uint64(),
			TimeToLiveMicroseconds: random.Uint64() | 1,
		}
		if command == CommandApply {
			frame.BodyLow = randomAmplitude(random, mask, ActuatorBodyLow)
			frame.BodyHigh = randomAmplitude(random, mask, ActuatorBodyHigh)
			frame.LeftTrigger = randomAmplitude(random, mask, ActuatorLeftTrigger)
			frame.RightTrigger = randomAmplitude(random, mask, ActuatorRightTrigger)
		}

		var encoded [FrameSize]byte
		if err := frame.MarshalTo(encoded[:]); err != nil {
			t.Fatalf("iteration %d marshal: %v frame=%+v", iteration, err, frame)
		}
		var decoded Frame
		if err := decoded.UnmarshalFrom(encoded[:]); err != nil {
			t.Fatalf("iteration %d unmarshal: %v", iteration, err)
		}
		if decoded != frame {
			t.Fatalf("iteration %d round trip mismatch\ngot=%+v\nwant=%+v",
				iteration, decoded, frame)
		}
	}
}

func randomAmplitude(random *rand.Rand, mask, actuator ActuatorMask) uint16 {
	if mask&actuator == 0 {
		return 0
	}
	return uint16(random.Uint32()) | 1
}

func TestFrameHotCodecDoesNotAllocate(t *testing.T) {
	frame := testFrame()
	var encoded [FrameSize]byte
	var decoded Frame
	allocations := testing.AllocsPerRun(1_000, func() {
		if err := frame.MarshalTo(encoded[:]); err != nil {
			panic(err)
		}
		if err := decoded.UnmarshalFrom(encoded[:]); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("codec allocated %.2f times per cycle", allocations)
	}
}

func FuzzFrameUnmarshal(f *testing.F) {
	var valid [FrameSize]byte
	if err := testFrame().MarshalTo(valid[:]); err != nil {
		f.Fatal(err)
	}
	f.Add(append([]byte(nil), valid[:]...))
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0xFF}, FrameSize))
	f.Fuzz(func(t *testing.T, data []byte) {
		decoded := testFrame()
		if err := decoded.UnmarshalFrom(data); err != nil {
			if decoded != (Frame{}) {
				t.Fatalf("failed decode retained state: %+v", decoded)
			}
			return
		}
		if err := decoded.Validate(); err != nil {
			t.Fatalf("successful decode is invalid: %v", err)
		}
		var roundTrip [FrameSize]byte
		if err := decoded.MarshalTo(roundTrip[:]); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(roundTrip[:], data) {
			t.Fatalf("round trip changed bytes: got=%x want=%x", roundTrip, data)
		}
	})
}

func testFrame() Frame {
	return Frame{
		Version:                Version1,
		Source:                 SourceXboxOneVirtualDevice,
		Command:                CommandApply,
		Actuators:              ActuatorAll,
		BodyLow:                1,
		BodyHigh:               2,
		LeftTrigger:            3,
		RightTrigger:           4,
		Sequence:               1,
		DeviceGeneration:       1,
		TransportGeneration:    1,
		OwnershipEpoch:         1,
		TimestampMicroseconds:  1_000,
		TimeToLiveMicroseconds: 50_000,
	}
}

func mutate(source [FrameSize]byte, offset int, value byte) []byte {
	result := append([]byte(nil), source[:]...)
	result[offset] = value
	return result
}

func zeroRange(source [FrameSize]byte, start, end int) []byte {
	result := append([]byte(nil), source[:]...)
	clear(result[start:end])
	return result
}

func zeroAmplitudes(source [FrameSize]byte) []byte {
	return zeroRange(source, 12, 20)
}

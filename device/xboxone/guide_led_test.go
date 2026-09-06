package xboxone

import (
	"errors"
	"testing"
)

func TestGuideLEDCommandGoldenRoundTrip(t *testing.T) {
	want := [GuideLEDCommandMessageSize]byte{
		0x0a, 0x20, 0xa5, 0x03, 0x00, 0x0d, 0x2f,
	}
	command := GuideLEDCommandV1{
		Pattern: GuideLEDPatternRampToLevel, Intensity: GuideLEDMaximumIntensity,
	}
	var got [GuideLEDCommandMessageSize]byte
	if err := EncodeGuideLEDCommandMessageInto(got[:], 0xa5, command); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got != want {
		t.Fatalf("message = % x, want % x", got, want)
	}
	sequence, decoded, err := DecodeGuideLEDCommandMessage(got[:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sequence != 0xa5 || decoded != command {
		t.Fatalf("decoded sequence/command = %02x/%+v", sequence, decoded)
	}
}

func TestGuideLEDCommandDomainIsExhaustive(t *testing.T) {
	for selector := 0; selector <= 0xff; selector++ {
		wire := []byte{byte(selector), byte(GuideLEDPatternOn), 0x00}
		_, err := DecodeGuideLEDCommandBody(wire)
		if selector == int(GuideLEDCommandValue) && err != nil {
			t.Fatalf("selector 0x%02x rejected: %v", selector, err)
		}
		if selector != int(GuideLEDCommandValue) &&
			!errors.Is(err, ErrInvalidGuideLEDCommand) {
			t.Fatalf("selector 0x%02x error = %v", selector, err)
		}
	}
	validPatterns := map[byte]bool{
		0x00: true, 0x01: true, 0x02: true,
		0x03: true, 0x04: true, 0x0d: true,
	}
	for pattern := 0; pattern <= 0xff; pattern++ {
		wire := []byte{0x00, byte(pattern), 0x00}
		_, err := DecodeGuideLEDCommandBody(wire)
		if validPatterns[byte(pattern)] && err != nil {
			t.Fatalf("pattern 0x%02x rejected: %v", pattern, err)
		}
		if !validPatterns[byte(pattern)] && !errors.Is(err, ErrReservedGuideLEDPattern) {
			t.Fatalf("pattern 0x%02x error = %v", pattern, err)
		}
	}
	for intensity := 0; intensity <= 0xff; intensity++ {
		wire := []byte{0x00, byte(GuideLEDPatternOn), byte(intensity)}
		_, err := DecodeGuideLEDCommandBody(wire)
		if intensity <= int(GuideLEDMaximumIntensity) && err != nil {
			t.Fatalf("intensity %d rejected: %v", intensity, err)
		}
		if intensity > int(GuideLEDMaximumIntensity) &&
			!errors.Is(err, ErrGuideLEDIntensityOutOfRange) {
			t.Fatalf("intensity %d error = %v", intensity, err)
		}
	}
}

func TestGuideLEDMessageHeaderGrammarIsExhaustive(t *testing.T) {
	assertExactMessageHeaderGrammar(
		t,
		func() []byte {
			wire := make([]byte, GuideLEDCommandMessageSize)
			if err := EncodeGuideLEDCommandMessageInto(
				wire, 1, GuideLEDCommandV1{Pattern: GuideLEDPatternOn}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			return wire
		},
		0x0a,
		flagSystem,
		GuideLEDCommandBodySize,
		func(wire []byte) error {
			_, _, err := DecodeGuideLEDCommandMessage(wire)
			return err
		},
	)
}

func TestGuideLEDCodecsAreAtomicExactAndNonallocating(t *testing.T) {
	command := GuideLEDCommandV1{Pattern: GuideLEDPatternOn, Intensity: 1}
	original := [GuideLEDCommandMessageSize]byte{}
	for index := range original {
		original[index] = 0xa5
	}
	got := original
	invalid := command
	invalid.Intensity = GuideLEDMaximumIntensity + 1
	if err := EncodeGuideLEDCommandMessageInto(got[:], 1, invalid); !errors.Is(err, ErrGuideLEDIntensityOutOfRange) {
		t.Fatalf("invalid intensity error = %v", err)
	}
	if got != original {
		t.Fatalf("destination changed: % x", got)
	}
	got = original
	if err := EncodeGuideLEDCommandMessageInto(got[:], 0, command); !errors.Is(err, ErrReservedSequence) {
		t.Fatalf("reserved sequence error = %v", err)
	}
	if got != original {
		t.Fatalf("destination changed: % x", got)
	}

	for _, size := range []int{GuideLEDCommandMessageSize - 1, GuideLEDCommandMessageSize + 1} {
		if err := EncodeGuideLEDCommandMessageInto(make([]byte, size), 1, command); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("encode length %d: %v", size, err)
		}
		if _, _, err := DecodeGuideLEDCommandMessage(make([]byte, size)); !errors.Is(err, ErrInvalidLength) {
			t.Errorf("decode length %d: %v", size, err)
		}
	}

	wire := [GuideLEDCommandMessageSize]byte{}
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := EncodeGuideLEDCommandMessageInto(wire[:], 1, command); err != nil {
			panic(err)
		}
		if _, _, err := DecodeGuideLEDCommandMessage(wire[:]); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("Guide LED codec allocations = %v, want 0", allocs)
	}
}

package xboxone

import (
	"errors"
	"testing"
)

func TestGuideButtonStatusGoldenVectors(t *testing.T) {
	for _, test := range []struct {
		name string
		seq  byte
		down bool
		want [GuideButtonStatusMessageSize]byte
	}{
		{name: "press", seq: 1, down: true,
			want: [GuideButtonStatusMessageSize]byte{0x07, 0x20, 0x01, 0x02, 0x01, 0x5b}},
		{name: "release", seq: 2,
			want: [GuideButtonStatusMessageSize]byte{0x07, 0x20, 0x02, 0x02, 0x00, 0x5b}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got [GuideButtonStatusMessageSize]byte
			if err := EncodeGuideButtonStatusMessageInto(
				got[:], test.seq, GuideButtonStatusV1{Down: test.down}); err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("wire = % x, want % x", got, test.want)
			}
			sequence, status, err := DecodeGuideButtonStatusMessage(got[:])
			if err != nil || sequence != test.seq || status.Down != test.down {
				t.Fatalf("decode = (%d, %+v, %v)", sequence, status, err)
			}
		})
	}
}

func TestGuideButtonStatusRejectsEveryInvalidField(t *testing.T) {
	valid := []byte{0x07, 0x20, 0x01, 0x02, 0x01, 0x5b}
	for index, values := range map[int][]byte{
		0: {0x06, 0x08, 0x27, 0x87},
		1: {0x00, 0x10, 0x21, 0x60},
		2: {0x00},
		3: {0x00, 0x01, 0x03, 0x82},
		4: {0x02, 0xff},
		5: {0x00, 0x5c, 0xff},
	} {
		for _, value := range values {
			wire := append([]byte(nil), valid...)
			wire[index] = value
			if _, _, err := DecodeGuideButtonStatusMessage(wire); err == nil {
				t.Fatalf("field %d value 0x%02x accepted", index, value)
			}
		}
	}
	if err := EncodeGuideButtonStatusMessageInto(
		make([]byte, GuideButtonStatusMessageSize), 0, GuideButtonStatusV1{}); !errors.Is(err, ErrReservedSequence) {
		t.Fatalf("zero sequence error = %v", err)
	}
}

package usb

import (
	"bytes"
	"context"
	"testing"
	"time"

	usbdesc "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

type isoInTestDevice struct {
	desc     *usbdesc.Descriptor
	payloads [][]byte
	calls    int
}

func (d *isoInTestDevice) HandleTransfer(context.Context, uint32, uint32, []byte) []byte {
	payload := d.payloads[d.calls]
	d.calls++
	return payload
}

func (d *isoInTestDevice) GetDescriptor() *usbdesc.Descriptor {
	return d.desc
}

func (d *isoInTestDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func TestCompleteIsoPacketsPreservesOffsetsAndLengths(t *testing.T) {
	completed := completeIsoPackets([]usbip.IsoPacketDescriptor{
		{Offset: 0, Length: 384},
		{Offset: 384, Length: 384},
		{Offset: 768, Length: 384},
	}, 900)

	if len(completed) != 3 {
		t.Fatalf("unexpected completed packet count: got %d want 3", len(completed))
	}
	if completed[0].ActualLength != 384 || completed[1].ActualLength != 384 || completed[2].ActualLength != 132 {
		t.Fatalf("unexpected completed ISO lengths: %#v", completed)
	}
	for _, packet := range completed {
		if packet.Status != 0 {
			t.Fatalf("unexpected ISO status: %#v", packet)
		}
	}
}

func TestBuildIsoInResponseCompactsPacketPadding(t *testing.T) {
	const (
		packetLength = 196
		actualLength = 192
	)
	desc := &usbdesc.Descriptor{
		Device: usbdesc.DeviceDescriptor{Speed: 3},
		Interfaces: []usbdesc.InterfaceConfig{{
			Endpoints: []usbdesc.EndpointDescriptor{{
				BEndpointAddress: 0x82,
				BMAttributes:     0x05,
				BInterval:        4,
			}},
		}},
	}
	dev := &isoInTestDevice{
		desc: desc,
		payloads: [][]byte{
			bytes.Repeat([]byte{0x11}, actualLength),
			bytes.Repeat([]byte{0x22}, actualLength),
			bytes.Repeat([]byte{0x33}, actualLength),
		},
	}
	submitted := []usbip.IsoPacketDescriptor{
		{Offset: 0, Length: packetLength},
		{Offset: packetLength, Length: packetLength},
		{Offset: 2 * packetLength, Length: packetLength},
	}

	response, completed := (&Server{}).buildIsoInResponse(
		context.Background(), dev, 2, usbip.DirIn, submitted)

	want := append(bytes.Repeat([]byte{0x11}, actualLength),
		bytes.Repeat([]byte{0x22}, actualLength)...)
	want = append(want, bytes.Repeat([]byte{0x33}, actualLength)...)
	if !bytes.Equal(response, want) {
		t.Fatalf("ISO IN response was not packed without descriptor padding: got %d bytes want %d",
			len(response), len(want))
	}
	for i, packet := range completed {
		if packet.Offset != submitted[i].Offset || packet.ActualLength != actualLength {
			t.Fatalf("unexpected completed ISO descriptor %d: %#v", i, packet)
		}
	}
}

func TestIsoCompletionDelayUsesHighSpeedMicroframes(t *testing.T) {
	desc := &usbdesc.Descriptor{
		Device: usbdesc.DeviceDescriptor{Speed: 3},
		Interfaces: []usbdesc.InterfaceConfig{{
			Endpoints: []usbdesc.EndpointDescriptor{{
				BEndpointAddress: 0x01,
				BMAttributes:     0x09,
				BInterval:        4,
			}},
		}},
	}

	if got := isoCompletionDelay(desc, 1, 8); got != 8*time.Millisecond {
		t.Fatalf("unexpected high-speed ISO delay: got %s want 8ms", got)
	}
}

func TestUSBServiceIntervalUsesHighSpeedMicroframes(t *testing.T) {
	if got := usbServiceInterval(3, 6); got != 4*time.Millisecond {
		t.Fatalf("unexpected high-speed interrupt interval: got %s want 4ms", got)
	}
}

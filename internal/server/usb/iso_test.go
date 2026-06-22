package usb

import (
	"testing"
	"time"

	usbdesc "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

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

	if got := isoCompletionDelay(desc, 1, 8, 0); got != 8*time.Millisecond {
		t.Fatalf("unexpected high-speed ISO delay: got %s want 8ms", got)
	}
}

func TestIsoCompletionDelayUsesDualSensePCMDuration(t *testing.T) {
	desc := &usbdesc.Descriptor{
		Device: usbdesc.DeviceDescriptor{Speed: 3},
		Interfaces: []usbdesc.InterfaceConfig{{
			Endpoints: []usbdesc.EndpointDescriptor{{
				BEndpointAddress: 0x01,
				BMAttributes:     0x09,
				WMaxPacketSize:   392,
				BInterval:        4,
			}},
		}},
	}

	if got := isoCompletionDelay(desc, 1, 3, 1176); got != 4*time.Millisecond {
		t.Fatalf("unexpected PCM-based ISO delay: got %s want 4ms", got)
	}
}

func TestUSBServiceIntervalUsesHighSpeedMicroframes(t *testing.T) {
	if got := usbServiceInterval(3, 6); got != 4*time.Millisecond {
		t.Fatalf("unexpected high-speed interrupt interval: got %s want 4ms", got)
	}
}

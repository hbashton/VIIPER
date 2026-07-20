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
	desc      *usbdesc.Descriptor
	payloads  [][]byte
	delays    []time.Duration
	calls     int
	callTimes []time.Time
}

func (d *isoInTestDevice) HandleTransfer(context.Context, uint32, uint32, []byte) []byte {
	d.callTimes = append(d.callTimes, time.Now())
	if d.calls < len(d.delays) && d.delays[d.calls] > 0 {
		time.Sleep(d.delays[d.calls])
	}
	payload := d.payloads[d.calls]
	d.calls++
	return payload
}

func TestBuildIsoInResponseConsumesPacketsAtServiceCadence(t *testing.T) {
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
			bytes.Repeat([]byte{0x11}, 32),
			bytes.Repeat([]byte{0x22}, 32),
			bytes.Repeat([]byte{0x33}, 32),
			bytes.Repeat([]byte{0x44}, 32),
		},
	}
	submitted := []usbip.IsoPacketDescriptor{
		{Offset: 0, Length: 32},
		{Offset: 32, Length: 32},
		{Offset: 64, Length: 32},
		{Offset: 96, Length: 32},
	}

	start := time.Now().Add(2 * time.Millisecond)
	response, completed, _ := (&Server{}).buildIsoInResponse(
		context.Background(), dev, 2, usbip.DirIn, submitted, start)

	if len(response) != 128 || len(completed) != len(submitted) {
		t.Fatalf("unexpected ISO response sizes: payload=%d packets=%d",
			len(response), len(completed))
	}
	if len(dev.callTimes) != len(submitted) {
		t.Fatalf("unexpected transfer calls: got %d want %d",
			len(dev.callTimes), len(submitted))
	}
	for i := range dev.callTimes {
		earlierThanSchedule := start.Add(time.Duration(i) * time.Millisecond).
			Sub(dev.callTimes[i])
		if earlierThanSchedule > 100*time.Microsecond {
			t.Fatalf("ISO packet %d was consumed before its service slot by %s",
				i, earlierThanSchedule)
		}
	}
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

	response, completed, _ := (&Server{}).buildIsoInResponse(
		context.Background(), dev, 2, usbip.DirIn, submitted, time.Now())

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

func TestReanchorIsoServiceWindowDoesNotReplayExpiredSlots(t *testing.T) {
	planned := time.Unix(100, 0)
	now := planned.Add(7 * time.Millisecond)
	start, end := reanchorIsoServiceWindow(
		planned, time.Time{}, 8*time.Millisecond, time.Millisecond, now)
	if !start.Equal(now) {
		t.Fatalf("late service window was not re-anchored: got %s want %s", start, now)
	}
	if !end.Equal(now.Add(8 * time.Millisecond)) {
		t.Fatalf("unexpected re-anchored service end: got %s", end)
	}
}

func TestReanchorIsoServiceWindowPreservesFutureSlot(t *testing.T) {
	planned := time.Unix(100, 0)
	now := planned.Add(-2 * time.Millisecond)
	start, end := reanchorIsoServiceWindow(
		planned, time.Time{}, 8*time.Millisecond, time.Millisecond, now)
	if !start.Equal(planned) || !end.Equal(planned.Add(8*time.Millisecond)) {
		t.Fatalf("future service window changed: start=%s end=%s", start, end)
	}
}

func TestReanchorIsoServiceWindowCarriesPreviousEndWithoutAddingJitter(t *testing.T) {
	planned := time.Unix(100, 0)
	previousEnd := planned.Add(7 * time.Millisecond)
	now := previousEnd.Add(100 * time.Microsecond)
	start, end := reanchorIsoServiceWindow(
		planned, previousEnd, 8*time.Millisecond, time.Millisecond, now)
	if !start.Equal(previousEnd) || !end.Equal(previousEnd.Add(8*time.Millisecond)) {
		t.Fatalf("previous service clock was not propagated: start=%s end=%s", start, end)
	}
}

func TestBuildIsoInResponseReanchorsAfterMissedPacketSlot(t *testing.T) {
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
			bytes.Repeat([]byte{0x11}, 32),
			bytes.Repeat([]byte{0x22}, 32),
			bytes.Repeat([]byte{0x33}, 32),
		},
		delays: []time.Duration{3 * time.Millisecond},
	}
	submitted := []usbip.IsoPacketDescriptor{
		{Length: 32}, {Length: 32}, {Length: 32},
	}

	_, _, _ = (&Server{}).buildIsoInResponse(
		context.Background(), dev, 2, usbip.DirIn, submitted,
		time.Now())
	if gap := dev.callTimes[2].Sub(dev.callTimes[1]); gap < 700*time.Microsecond {
		t.Fatalf("slot after a mid-URB stall was replayed in a burst: gap=%s", gap)
	}
}

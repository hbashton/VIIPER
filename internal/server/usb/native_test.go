package usb

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/Alia5/VIIPER/internal/transport/udecx"
	usbdevice "github.com/Alia5/VIIPER/usb"
)

func nativeProcessorForTest(t *testing.T) *NativeProcessor {
	t.Helper()
	processor, err := NewNativeProcessor(New(ServerConfig{}, slog.Default(), nil))
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func TestNativeProcessorServesControlDescriptor(t *testing.T) {
	dev := newNativeTransportTestDevice()
	op := udecx.Operation{
		Token: 1, DeviceID: 1, Generation: 1, Kind: udecx.OperationControl,
		Direction: 1, TransferLength: 18,
		SetupPacket: [8]byte{0x80, usbReqGetDescriptor, 0, usbDescTypeDevice, 0, 0, 18, 0},
	}
	completion, err := nativeProcessorForTest(t).Process(context.Background(), dev, op)
	if err != nil {
		t.Fatal(err)
	}
	if completion.TransferLength != 18 || len(completion.Payload) != 18 ||
		completion.Payload[1] != usbDescTypeDevice {
		t.Fatalf("unexpected device descriptor completion: %+v payload=%x", completion, completion.Payload)
	}
}

func TestNativeProcessorPreservesSparseIsoInLayout(t *testing.T) {
	desc := &usbdevice.Descriptor{
		Device: usbdevice.DeviceDescriptor{Speed: uint32(udecx.DeviceSpeedHigh)},
		Interfaces: []usbdevice.InterfaceConfig{{Endpoints: []usbdevice.EndpointDescriptor{{
			BEndpointAddress: 0x82, BMAttributes: 0x01, WMaxPacketSize: 32, BInterval: 1,
		}}}},
	}
	dev := &isoInTestDevice{desc: desc, payloads: [][]byte{
		bytes.Repeat([]byte{0x11}, 12), bytes.Repeat([]byte{0x22}, 8),
	}}
	op := udecx.Operation{
		Token: 2, DeviceID: 1, Generation: 1, Kind: udecx.OperationTransfer,
		EndpointAddress: 0x82, Direction: 1, TransferLength: 48,
		IsoPackets: []udecx.IsoPacket{{Offset: 0, Length: 16}, {Offset: 32, Length: 16}},
	}
	completion, err := nativeProcessorForTest(t).Process(context.Background(), dev, op)
	if err != nil {
		t.Fatal(err)
	}
	if completion.TransferLength != 20 || len(completion.Payload) != 48 {
		t.Fatalf("transfer=%d payload=%d want 20/48", completion.TransferLength, len(completion.Payload))
	}
	if completion.IsoPackets[0].Length != 12 || completion.IsoPackets[1].Length != 8 ||
		!bytes.Equal(completion.Payload[:12], bytes.Repeat([]byte{0x11}, 12)) ||
		!bytes.Equal(completion.Payload[32:40], bytes.Repeat([]byte{0x22}, 8)) {
		t.Fatalf("sparse ISO payload or packet actuals were not preserved: %+v", completion)
	}
}

type isoOutRecordingDevice struct {
	desc    *usbdevice.Descriptor
	payload []byte
}

func (d *isoOutRecordingDevice) HandleTransfer(_ context.Context, _ uint32, _ uint32, out []byte) []byte {
	d.payload = append(d.payload[:0], out...)
	return nil
}
func (d *isoOutRecordingDevice) GetDescriptor() *usbdevice.Descriptor { return d.desc }
func (*isoOutRecordingDevice) GetDeviceSpecificArgs() map[string]any  { return nil }

func TestNativeProcessorCompletesIsoOutWithoutEchoPayload(t *testing.T) {
	desc := &usbdevice.Descriptor{
		Device: usbdevice.DeviceDescriptor{Speed: uint32(udecx.DeviceSpeedHigh)},
		Interfaces: []usbdevice.InterfaceConfig{{Endpoints: []usbdevice.EndpointDescriptor{{
			BEndpointAddress: 0x02, BMAttributes: 0x01, WMaxPacketSize: 32, BInterval: 1,
		}}}},
	}
	dev := &isoOutRecordingDevice{desc: desc}
	payload := bytes.Repeat([]byte{0x5a}, 32)
	op := udecx.Operation{
		Token: 3, DeviceID: 1, Generation: 1, Kind: udecx.OperationTransfer,
		EndpointAddress: 0x02, TransferLength: 32, Payload: payload,
		IsoPackets: []udecx.IsoPacket{{Offset: 0, Length: 16}, {Offset: 16, Length: 16}},
	}
	completion, err := nativeProcessorForTest(t).Process(context.Background(), dev, op)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dev.payload, payload) || len(completion.Payload) != 0 ||
		completion.TransferLength != 32 || len(completion.IsoPackets) != 2 {
		t.Fatalf("unexpected ISO OUT completion: %+v captured=%x", completion, dev.payload)
	}
}

package usb

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestNativeProcessorServesSwitchMicrosoftOS10FeatureDescriptor(t *testing.T) {
	msOS := &usbdevice.MicrosoftOS10Descriptor{
		VendorCode: 0x20, InterfaceNumber: 1, CompatibleID: "WINUSB",
	}
	dev := &altSettingTestDevice{desc: &usbdevice.Descriptor{MicrosoftOS10: msOS}}
	op := udecx.Operation{
		Token: 2, DeviceID: 1, Generation: 1, Kind: udecx.OperationControl,
		Direction: 1, TransferLength: 40,
		SetupPacket: [8]byte{0xC0, msOS.EffectiveVendorCode(), 0, 0, 4, 0, 40, 0},
	}
	completion, err := nativeProcessorForTest(t).Process(context.Background(), dev, op)
	if err != nil {
		t.Fatal(err)
	}
	want := msOS.CompatibleIDDescriptor()
	if completion.TransferLength != uint32(len(want)) || !bytes.Equal(completion.Payload, want) {
		t.Fatalf("native Microsoft OS feature response=%x want=%x", completion.Payload, want)
	}
}

func TestNativeProcessorDerivesInterfaceSettingFromEndpointLifecycle(t *testing.T) {
	desc := &usbdevice.Descriptor{
		Device: usbdevice.DeviceDescriptor{Speed: uint32(udecx.DeviceSpeedHigh)},
		Interfaces: []usbdevice.InterfaceConfig{
			{Descriptor: usbdevice.InterfaceDescriptor{
				BInterfaceNumber: 2, BAlternateSetting: 0,
			}},
			{Descriptor: usbdevice.InterfaceDescriptor{
				BInterfaceNumber: 2, BAlternateSetting: 1, BNumEndpoints: 1,
			}, Endpoints: []usbdevice.EndpointDescriptor{{
				BEndpointAddress: 0x82, BMAttributes: 0x05,
				WMaxPacketSize: 196, BInterval: 4,
			}}},
		},
	}
	dev := &altSettingTestDevice{desc: desc}
	processor := nativeProcessorForTest(t)
	// UdeCx supplies incorrect numeric interface fields for some composite
	// devices. An endpoint-bearing alternate must therefore ignore this hint.
	if err := processor.Lifecycle(context.Background(), dev, udecx.Operation{
		DeviceID: 1, Generation: 1, Kind: udecx.OperationSetInterface,
		InterfaceNumber: 0, InterfaceSetting: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if got := processor.server.getInterfaceAlt(dev, 2); got != 0 {
		t.Fatalf("unreliable interface hint changed interface 2 alt to %d", got)
	}

	op := udecx.Operation{
		DeviceID: 1, Generation: 1, Kind: udecx.OperationEndpointStart,
		EndpointAddress: 0x82, EndpointAttributes: 0x05,
		EndpointInterval: 4, EndpointMaxPacketSize: 196,
	}
	if err := processor.Lifecycle(context.Background(), dev, op); err != nil {
		t.Fatal(err)
	}
	if got := processor.server.getInterfaceAlt(dev, 2); got != 1 {
		t.Fatalf("interface 2 alt=%d want 1 after endpoint start", got)
	}

	op.Kind = udecx.OperationEndpointPurge
	if err := processor.Lifecycle(context.Background(), dev, op); err != nil {
		t.Fatal(err)
	}
	if got := processor.server.getInterfaceAlt(dev, 2); got != 0 {
		t.Fatalf("interface 2 alt=%d want 0 after endpoint purge", got)
	}
	if want := [][2]uint8{{2, 1}, {2, 0}}; !bytes.Equal(flattenAltEvents(dev.altEvents), flattenAltEvents(want)) {
		t.Fatalf("device alternate-setting events=%v want %v", dev.altEvents, want)
	}
}

func flattenAltEvents(events [][2]uint8) []byte {
	result := make([]byte, 0, len(events)*2)
	for _, event := range events {
		result = append(result, event[0], event[1])
	}
	return result
}

func TestNativeProcessorFirstISOTransferClosesEndpointStartRace(t *testing.T) {
	desc := &usbdevice.Descriptor{Interfaces: []usbdevice.InterfaceConfig{
		{Descriptor: usbdevice.InterfaceDescriptor{BInterfaceNumber: 2}},
		{Descriptor: usbdevice.InterfaceDescriptor{
			BInterfaceNumber: 2, BAlternateSetting: 1, BNumEndpoints: 1,
		}, Endpoints: []usbdevice.EndpointDescriptor{{
			BEndpointAddress: 0x02, BMAttributes: 0x05,
			WMaxPacketSize: 196, BInterval: 4,
		}}},
	}}
	dev := &isoOutRecordingDevice{desc: desc}
	processor := nativeProcessorForTest(t)
	_, err := processor.Process(context.Background(), dev, udecx.Operation{
		Token: 1, DeviceID: 3, Generation: 7, Kind: udecx.OperationTransfer,
		EndpointAddress: 0x02, EndpointAttributes: 0x05,
		EndpointInterval: 4, EndpointMaxPacketSize: 196,
		TransferFlags:  udecx.TransferFlagStartIsoASAP,
		TransferLength: 4, Payload: []byte{1, 2, 3, 4},
		IsoPackets: []udecx.IsoPacket{{Offset: 0, Length: 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := processor.server.getInterfaceAlt(dev, 2); got != 1 {
		t.Fatalf("first ISO transfer left interface 2 at alt %d", got)
	}
}

func TestNativeProcessorPreservesAlternateSettingAcrossLinkPower(t *testing.T) {
	desc := &usbdevice.Descriptor{
		Device: usbdevice.DeviceDescriptor{Speed: uint32(udecx.DeviceSpeedHigh)},
		Interfaces: []usbdevice.InterfaceConfig{
			{Descriptor: usbdevice.InterfaceDescriptor{BInterfaceNumber: 2, BAlternateSetting: 0}},
			{Descriptor: usbdevice.InterfaceDescriptor{BInterfaceNumber: 2, BAlternateSetting: 1}},
		},
	}
	dev := &altSettingTestDevice{desc: desc}
	processor := nativeProcessorForTest(t)
	processor.server.setInterfaceAlt(dev, 2, 1)
	identity := udecx.DeviceIdentity{DeviceID: 4, Generation: 7}
	key := nativeLaneKey{deviceID: identity.DeviceID, generation: identity.Generation, endpoint: 0x82}
	processor.next[key] = time.Now()
	processor.lastIn[key] = []byte{1, 2, 3}

	for _, kind := range []udecx.OperationKind{
		udecx.OperationDeviceD0Exit, udecx.OperationDeviceD0Entry,
	} {
		if err := processor.Lifecycle(context.Background(), dev, udecx.Operation{
			DeviceID: identity.DeviceID, Generation: identity.Generation, Kind: kind,
		}); err != nil {
			t.Fatal(err)
		}
		if got := processor.server.getInterfaceAlt(dev, 2); got != 1 {
			t.Fatalf("link-power event %d reset interface 2 alt to %d", kind, got)
		}
		if _, ok := processor.next[key]; ok {
			t.Fatalf("link-power event %d retained stale service clock", kind)
		}
	}
}

func TestNativeProcessorSetConfigurationRetiresGenerationTransportState(t *testing.T) {
	desc := &usbdevice.Descriptor{
		Device: usbdevice.DeviceDescriptor{Speed: uint32(udecx.DeviceSpeedHigh)},
		Interfaces: []usbdevice.InterfaceConfig{
			{Descriptor: usbdevice.InterfaceDescriptor{BInterfaceNumber: 2, BAlternateSetting: 0}},
			{Descriptor: usbdevice.InterfaceDescriptor{
				BInterfaceNumber: 2, BAlternateSetting: 1, BNumEndpoints: 1,
			}, Endpoints: []usbdevice.EndpointDescriptor{{
				BEndpointAddress: 0x02, BMAttributes: 0x05,
				WMaxPacketSize: 196, BInterval: 4,
			}}},
		},
	}
	dev := &altSettingTestDevice{desc: desc}
	processor := nativeProcessorForTest(t)
	identity := udecx.DeviceIdentity{DeviceID: 5, Generation: 9}
	endpoint := udecx.Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		Kind:            udecx.OperationEndpointStart,
		EndpointAddress: 0x02, EndpointAttributes: 0x05,
		EndpointInterval: 4, EndpointMaxPacketSize: 196,
	}
	if err := processor.Lifecycle(context.Background(), dev, endpoint); err != nil {
		t.Fatal(err)
	}
	key := nativeLaneKeyFromOperation(endpoint)
	cacheOnlyKey := key
	cacheOnlyKey.endpoint = 0x83
	processor.mu.Lock()
	processor.next[key] = time.Now()
	processor.lastIn[key] = []byte{1, 2, 3}
	processor.lastIn[cacheOnlyKey] = []byte{4, 5, 6}
	processor.mu.Unlock()

	_, err := processor.Process(context.Background(), dev, udecx.Operation{
		Token: 2, DeviceID: identity.DeviceID, Generation: identity.Generation,
		Kind: udecx.OperationControl, EndpointAddress: 0,
		SetupPacket: [8]byte{usbReqTypeStandardToDevice, usbReqSetConfiguration, 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := processor.server.getInterfaceAlt(dev, 2); got != 0 {
		t.Fatalf("SET_CONFIGURATION left interface 2 at alternate %d", got)
	}
	processor.mu.Lock()
	_, hasClock := processor.next[key]
	_, hasCachedInput := processor.lastIn[key]
	_, hasCacheOnlyInput := processor.lastIn[cacheOnlyKey]
	session := processor.sessions[nativeSessionKey{
		deviceID: identity.DeviceID, generation: identity.Generation,
	}]
	processor.mu.Unlock()
	if hasClock || hasCachedInput || hasCacheOnlyInput {
		t.Fatalf("SET_CONFIGURATION retained clock=%v cachedInput=%v cacheOnlyInput=%v",
			hasClock, hasCachedInput, hasCacheOnlyInput)
	}
	if session == nil {
		t.Fatal("SET_CONFIGURATION lost the registered generation session")
	}
	session.mu.Lock()
	activeEndpoints := len(session.active)
	session.mu.Unlock()
	if activeEndpoints != 0 {
		t.Fatalf("SET_CONFIGURATION retained %d active endpoint signatures", activeEndpoints)
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
		EndpointAddress: 0x82, Direction: 1, EndpointAttributes: 0x01,
		EndpointInterval: 1, EndpointMaxPacketSize: 32,
		TransferFlags:  udecx.TransferFlagStartIsoASAP | udecx.TransferFlagDirectionIn,
		TransferLength: 48,
		IsoPackets:     []udecx.IsoPacket{{Offset: 0, Length: 16}, {Offset: 32, Length: 16}},
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
	desc      *usbdevice.Descriptor
	payload   []byte
	endpoint  uint32
	direction uint32
}

type directIsoInTestDevice struct {
	desc          *usbdevice.Descriptor
	calls         int
	fallbackCalls int
	endpoint      uint32
}

func (d *directIsoInTestDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	d.fallbackCalls++
	return nil
}

func (d *directIsoInTestDevice) ReadIsochronousInput(
	_ context.Context, endpoint uint32, dst []byte,
) (int, error) {
	d.calls++
	d.endpoint = endpoint
	actual := len(dst) - d.calls
	for index := 0; index < actual; index++ {
		dst[index] = byte(0x20*d.calls + index)
	}
	return actual, nil
}

func (d *directIsoInTestDevice) GetDescriptor() *usbdevice.Descriptor { return d.desc }
func (*directIsoInTestDevice) GetDeviceSpecificArgs() map[string]any  { return nil }

func TestNativeProcessorWritesIsoInDirectlyIntoURBPacketRegions(t *testing.T) {
	desc := &usbdevice.Descriptor{
		Device: usbdevice.DeviceDescriptor{Speed: uint32(udecx.DeviceSpeedHigh)},
		Interfaces: []usbdevice.InterfaceConfig{{Endpoints: []usbdevice.EndpointDescriptor{{
			BEndpointAddress: 0x82, BMAttributes: 0x05, WMaxPacketSize: 8, BInterval: 1,
		}}}},
	}
	dev := &directIsoInTestDevice{desc: desc}
	op := udecx.Operation{
		Token: 8, DeviceID: 4, Generation: 2, Kind: udecx.OperationTransfer,
		EndpointAddress: 0x82, Direction: 1, EndpointAttributes: 0x05,
		EndpointInterval: 1, EndpointMaxPacketSize: 8,
		TransferFlags:  udecx.TransferFlagStartIsoASAP | udecx.TransferFlagDirectionIn,
		TransferLength: 24,
		IsoPackets:     []udecx.IsoPacket{{Offset: 0, Length: 8}, {Offset: 16, Length: 8}},
	}
	completion, err := nativeProcessorForTest(t).Process(context.Background(), dev, op)
	if err != nil {
		t.Fatal(err)
	}
	if dev.calls != 2 || dev.fallbackCalls != 0 {
		t.Fatalf("direct calls=%d fallback calls=%d want 2/0", dev.calls, dev.fallbackCalls)
	}
	if completion.TransferLength != 13 || len(completion.Payload) != 24 ||
		completion.IsoPackets[0].Length != 7 || completion.IsoPackets[1].Length != 6 {
		t.Fatalf("unexpected direct ISO completion: %+v", completion)
	}
	wantFirst := []byte{0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26}
	wantSecond := []byte{0x40, 0x41, 0x42, 0x43, 0x44, 0x45}
	if !bytes.Equal(completion.Payload[:7], wantFirst) ||
		!bytes.Equal(completion.Payload[16:22], wantSecond) ||
		!bytes.Equal(completion.Payload[8:16], make([]byte, 8)) {
		t.Fatalf("direct ISO packet regions were not preserved: % x", completion.Payload)
	}
}

func (d *isoOutRecordingDevice) HandleTransfer(
	_ context.Context, endpoint, direction uint32, out []byte,
) []byte {
	d.endpoint = endpoint
	d.direction = direction
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
		EndpointAddress: 0x02, EndpointAttributes: 0x01,
		EndpointInterval: 1, EndpointMaxPacketSize: 32,
		TransferFlags: udecx.TransferFlagStartIsoASAP, TransferLength: 32, Payload: payload,
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

func TestNativeProcessorSchedulesIsoOutFromExactOperationFrame(t *testing.T) {
	base := time.Unix(500, 0)
	desc := &usbdevice.Descriptor{
		Device: usbdevice.DeviceDescriptor{Speed: uint32(udecx.DeviceSpeedHigh)},
		Interfaces: []usbdevice.InterfaceConfig{
			{Descriptor: usbdevice.InterfaceDescriptor{
				BInterfaceNumber: 2, BAlternateSetting: 1,
			}, Endpoints: []usbdevice.EndpointDescriptor{{
				BEndpointAddress: 0x02, BMAttributes: 0x01,
				WMaxPacketSize: 32, BInterval: 1,
			}}},
			{Descriptor: usbdevice.InterfaceDescriptor{
				BInterfaceNumber: 2, BAlternateSetting: 2,
			}, Endpoints: []usbdevice.EndpointDescriptor{{
				BEndpointAddress: 0x02, BMAttributes: 0x09,
				WMaxPacketSize: 64, BInterval: 4,
			}}},
		},
	}
	dev := &isoOutRecordingDevice{desc: desc}
	processor := nativeProcessorForTest(t)
	processor.clock = func() nativeClockSample {
		return nativeClockSample{now: base, frame: 100}
	}
	var waits []time.Time
	processor.wait = func(_ context.Context, deadline time.Time) bool {
		waits = append(waits, deadline)
		return true
	}
	payload := []byte{1, 2, 3, 4}
	completion, err := processor.Process(context.Background(), dev, udecx.Operation{
		Token: 20, DeviceID: 4, Generation: 7, Kind: udecx.OperationTransfer,
		EndpointAddress: 0x02, Direction: 0, EndpointAttributes: 0x09,
		EndpointInterval: 4, EndpointMaxPacketSize: 64,
		StartFrame: 103, TransferLength: uint32(len(payload)), Payload: payload,
		IsoPackets: []udecx.IsoPacket{{Offset: 0, Length: 2}, {Offset: 2, Length: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantWaits := []time.Time{base.Add(3 * time.Millisecond), base.Add(5 * time.Millisecond)}
	if len(waits) != len(wantWaits) || !waits[0].Equal(wantWaits[0]) || !waits[1].Equal(wantWaits[1]) {
		t.Fatalf("ISO OUT waits=%v want %v", waits, wantWaits)
	}
	if dev.endpoint != 2 || dev.direction != 0 || !bytes.Equal(dev.payload, payload) {
		t.Fatalf("ISO OUT routed endpoint=%d direction=%d payload=%x", dev.endpoint, dev.direction, dev.payload)
	}
	if completion.TransferLength != uint32(len(payload)) || len(completion.IsoPackets) != 2 {
		t.Fatalf("unexpected ISO OUT completion: %+v", completion)
	}
}

func TestNativeProcessorSchedulesIsoInPacketsFromExactOperationFrame(t *testing.T) {
	base := time.Unix(600, 0)
	desc := &usbdevice.Descriptor{
		Device: usbdevice.DeviceDescriptor{Speed: uint32(udecx.DeviceSpeedHigh)},
		Interfaces: []usbdevice.InterfaceConfig{{Endpoints: []usbdevice.EndpointDescriptor{{
			BEndpointAddress: 0x82, BMAttributes: 0x05,
			WMaxPacketSize: 8, BInterval: 4,
		}}}},
	}
	dev := &directIsoInTestDevice{desc: desc}
	processor := nativeProcessorForTest(t)
	processor.clock = func() nativeClockSample {
		return nativeClockSample{now: base, frame: 700}
	}
	var waits []time.Time
	processor.wait = func(_ context.Context, deadline time.Time) bool {
		waits = append(waits, deadline)
		return true
	}
	completion, err := processor.Process(context.Background(), dev, udecx.Operation{
		Token: 21, DeviceID: 4, Generation: 7, Kind: udecx.OperationTransfer,
		EndpointAddress: 0x82, Direction: 1, EndpointAttributes: 0x05,
		EndpointInterval: 4, EndpointMaxPacketSize: 8,
		TransferFlags: udecx.TransferFlagDirectionIn, StartFrame: 702, TransferLength: 16,
		IsoPackets: []udecx.IsoPacket{{Offset: 0, Length: 8}, {Offset: 8, Length: 8}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantWaits := []time.Time{base.Add(2 * time.Millisecond), base.Add(3 * time.Millisecond)}
	if len(waits) != len(wantWaits) || !waits[0].Equal(wantWaits[0]) || !waits[1].Equal(wantWaits[1]) {
		t.Fatalf("ISO IN waits=%v want %v", waits, wantWaits)
	}
	if dev.endpoint != 2 || dev.calls != 2 || completion.TransferLength != 13 {
		t.Fatalf("ISO IN endpoint=%d calls=%d completion=%+v", dev.endpoint, dev.calls, completion)
	}
}

func TestResolveNativeIsoEndpointUsesDirectionAndAlternateSignature(t *testing.T) {
	desc := &usbdevice.Descriptor{
		Device: usbdevice.DeviceDescriptor{Speed: uint32(udecx.DeviceSpeedHigh)},
		Interfaces: []usbdevice.InterfaceConfig{
			{Descriptor: usbdevice.InterfaceDescriptor{BInterfaceNumber: 1, BAlternateSetting: 1},
				Endpoints: []usbdevice.EndpointDescriptor{{
					BEndpointAddress: 0x02, BMAttributes: 0x01,
					WMaxPacketSize: 32, BInterval: 1,
				}}},
			{Descriptor: usbdevice.InterfaceDescriptor{BInterfaceNumber: 1, BAlternateSetting: 2},
				Endpoints: []usbdevice.EndpointDescriptor{{
					BEndpointAddress: 0x02, BMAttributes: 0x09,
					WMaxPacketSize: 64, BInterval: 4,
				}}},
			{Descriptor: usbdevice.InterfaceDescriptor{BInterfaceNumber: 2, BAlternateSetting: 1},
				Endpoints: []usbdevice.EndpointDescriptor{{
					BEndpointAddress: 0x82, BMAttributes: 0x05,
					WMaxPacketSize: 96, BInterval: 2,
				}}},
		},
	}
	dev := &altSettingTestDevice{desc: desc}
	out, err := resolveNativeIsoEndpoint(dev, udecx.Operation{
		DeviceID: 1, Generation: 1, EndpointAddress: 0x02, Direction: 0,
		EndpointAttributes: 0x09, EndpointInterval: 4, EndpointMaxPacketSize: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	in, err := resolveNativeIsoEndpoint(dev, udecx.Operation{
		DeviceID: 1, Generation: 1, EndpointAddress: 0x82, Direction: 1,
		EndpointAttributes: 0x05, EndpointInterval: 2, EndpointMaxPacketSize: 96,
		TransferFlags: udecx.TransferFlagDirectionIn,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.interval != time.Millisecond || out.direction != 0 || out.key == in.key {
		t.Fatalf("exact OUT endpoint=%+v IN endpoint=%+v", out, in)
	}
	if in.interval != 250*time.Microsecond || in.direction != 1 {
		t.Fatalf("exact IN endpoint=%+v", in)
	}
	_, err = resolveNativeIsoEndpoint(dev, udecx.Operation{
		EndpointAddress: 0x82, Direction: 0, EndpointAttributes: 0x05,
		EndpointInterval: 2, EndpointMaxPacketSize: 96,
		TransferFlags: udecx.TransferFlagDirectionIn,
	})
	if err == nil {
		t.Fatal("direction mismatch was accepted")
	}
}

func TestResolveNativeIsoEndpointMapsProjectedFullSpeedSignatureToLogicalCadence(t *testing.T) {
	desc := &usbdevice.Descriptor{
		Device: usbdevice.DeviceDescriptor{Speed: uint32(udecx.DeviceSpeedFull)},
		Interfaces: []usbdevice.InterfaceConfig{{
			Descriptor: usbdevice.InterfaceDescriptor{BInterfaceNumber: 1, BAlternateSetting: 1},
			Endpoints: []usbdevice.EndpointDescriptor{{
				BEndpointAddress: 0x01, BMAttributes: 0x09,
				WMaxPacketSize: 132, BInterval: 1,
			}},
		}},
	}
	dev := &altSettingTestDevice{desc: desc}
	endpoint, err := resolveNativeIsoEndpoint(dev, udecx.Operation{
		DeviceID: 1, Generation: 1, EndpointAddress: 0x01,
		EndpointAttributes: 0x09, EndpointInterval: 4, EndpointMaxPacketSize: 132,
	})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.interval != time.Millisecond {
		t.Fatalf("projected full-speed interval=%s want=1ms logical cadence", endpoint.interval)
	}
	_, err = resolveNativeIsoEndpoint(dev, udecx.Operation{
		DeviceID: 1, Generation: 1, EndpointAddress: 0x01,
		EndpointAttributes: 0x09, EndpointInterval: 1, EndpointMaxPacketSize: 132,
	})
	if err == nil {
		t.Fatal("unprojected full-speed UdeCx signature was accepted")
	}
}

func TestNativeIsoExplicitFrameRangeHandlesWrap(t *testing.T) {
	base := time.Unix(700, 0)
	processor := nativeProcessorForTest(t)
	processor.clock = func() nativeClockSample {
		return nativeClockSample{now: base, frame: 0xfffffffe}
	}
	key := nativeLaneKey{deviceID: 1, generation: 1, endpoint: 0x02, interval: 4}
	start, end, err := processor.reserveIsoServiceWindow(key, 1, 0, 2*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !start.Equal(base.Add(3*time.Millisecond)) || !end.Equal(base.Add(5*time.Millisecond)) {
		t.Fatalf("wrapped explicit frame start=%s end=%s", start, end)
	}

	processor.clock = func() nativeClockSample {
		return nativeClockSample{now: base, frame: 100}
	}
	if _, _, err := processor.reserveIsoServiceWindow(
		key, 100+uint32(usbdIsoStartFrameRange), 0, time.Millisecond); err == nil {
		t.Fatal("out-of-range explicit frame was accepted")
	}
	if _, _, err := processor.reserveIsoServiceWindow(key, 99, 0, time.Millisecond); err == nil {
		t.Fatal("past explicit frame was accepted")
	}
}

func TestNativeIsoASAPContinuityReanchorsAfterDrift(t *testing.T) {
	base := time.Unix(800, 0)
	processor := nativeProcessorForTest(t)
	sample := nativeClockSample{now: base, frame: 100}
	processor.clock = func() nativeClockSample { return sample }
	key := nativeLaneKey{deviceID: 1, generation: 1, endpoint: 0x82, interval: 4}
	start, end, err := processor.reserveIsoServiceWindow(
		key, 101, udecx.TransferFlagStartIsoASAP, 4*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !start.Equal(base.Add(time.Millisecond)) || !end.Equal(base.Add(5*time.Millisecond)) {
		t.Fatalf("first ASAP window start=%s end=%s", start, end)
	}

	start, end, err = processor.reserveIsoServiceWindow(
		key, 102, udecx.TransferFlagStartIsoASAP, 4*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !start.Equal(base.Add(5*time.Millisecond)) || !end.Equal(base.Add(9*time.Millisecond)) {
		t.Fatalf("ordered ASAP window start=%s end=%s", start, end)
	}

	sample = nativeClockSample{now: base.Add(20 * time.Millisecond), frame: 120}
	start, end, err = processor.reserveIsoServiceWindow(
		key, 103, udecx.TransferFlagStartIsoASAP, 4*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !start.Equal(sample.now) || !end.Equal(sample.now.Add(4*time.Millisecond)) {
		t.Fatalf("late ASAP drift replayed stale slots: start=%s end=%s", start, end)
	}
}

func TestNativeEndpointResetClearsAlternateReuseClocks(t *testing.T) {
	processor := nativeProcessorForTest(t)
	base := udecx.Operation{
		DeviceID: 8, Generation: 3, EndpointAddress: 0x02,
		EndpointAttributes: 0x01, EndpointInterval: 1, EndpointMaxPacketSize: 32,
	}
	first := nativeLaneKeyFromOperation(base)
	base.EndpointAttributes = 0x09
	base.EndpointInterval = 4
	base.EndpointMaxPacketSize = 64
	second := nativeLaneKeyFromOperation(base)
	other := second
	other.endpoint = 0x82
	processor.next[first] = time.Now()
	processor.next[second] = time.Now()
	processor.next[other] = time.Now()
	processor.lastIn[first] = []byte{1}
	processor.lastIn[second] = []byte{2}

	if err := processor.Lifecycle(context.Background(), &altSettingTestDevice{}, udecx.Operation{
		DeviceID: 8, Generation: 3, Kind: udecx.OperationEndpointReset,
		EndpointAddress: 0x02, EndpointAttributes: 0x09,
		EndpointInterval: 4, EndpointMaxPacketSize: 64,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := processor.next[first]; ok {
		t.Fatal("endpoint reset retained the old alternate clock")
	}
	if _, ok := processor.next[second]; ok {
		t.Fatal("endpoint reset retained the current alternate clock")
	}
	if _, ok := processor.lastIn[first]; ok {
		t.Fatal("endpoint reset retained the old alternate cache")
	}
	if _, ok := processor.next[other]; !ok {
		t.Fatal("endpoint reset cleared an independent direction")
	}
}

type concurrentNativeTestDevice struct {
	desc      *usbdevice.Descriptor
	mu        sync.Mutex
	altEvents [][2]uint8
	transfers atomic.Uint64
}

func (d *concurrentNativeTestDevice) HandleTransfer(
	_ context.Context, _, _ uint32, _ []byte,
) []byte {
	d.transfers.Add(1)
	return nil
}

func (d *concurrentNativeTestDevice) GetDescriptor() *usbdevice.Descriptor { return d.desc }
func (*concurrentNativeTestDevice) GetDeviceSpecificArgs() map[string]any  { return nil }
func (d *concurrentNativeTestDevice) SetInterfaceAltSetting(iface, alt uint8) {
	d.mu.Lock()
	d.altEvents = append(d.altEvents, [2]uint8{iface, alt})
	d.mu.Unlock()
}

type lifecycleGateDevice struct {
	desc         *usbdevice.Descriptor
	resetStarted chan struct{}
	resetRelease chan struct{}
	altChanged   chan [2]uint8
}

func (*lifecycleGateDevice) HandleTransfer(context.Context, uint32, uint32, []byte) []byte {
	return nil
}
func (d *lifecycleGateDevice) GetDescriptor() *usbdevice.Descriptor { return d.desc }
func (*lifecycleGateDevice) GetDeviceSpecificArgs() map[string]any  { return nil }
func (d *lifecycleGateDevice) ResetEndpoint(uint8) {
	close(d.resetStarted)
	<-d.resetRelease
}
func (d *lifecycleGateDevice) SetInterfaceAltSetting(iface, alt uint8) {
	d.altChanged <- [2]uint8{iface, alt}
}

func TestNativeProcessorSerializesEndpointResetWithEndpointStart(t *testing.T) {
	desc := &usbdevice.Descriptor{Interfaces: []usbdevice.InterfaceConfig{
		{Descriptor: usbdevice.InterfaceDescriptor{BInterfaceNumber: 2}},
		{Descriptor: usbdevice.InterfaceDescriptor{
			BInterfaceNumber: 2, BAlternateSetting: 1, BNumEndpoints: 1,
		}, Endpoints: []usbdevice.EndpointDescriptor{{
			BEndpointAddress: 0x02, BMAttributes: 0x05,
			WMaxPacketSize: 4, BInterval: 1,
		}}},
	}}
	dev := &lifecycleGateDevice{
		desc: desc, resetStarted: make(chan struct{}), resetRelease: make(chan struct{}),
		altChanged: make(chan [2]uint8, 1),
	}
	processor := nativeProcessorForTest(t)
	base := udecx.Operation{
		DeviceID: 92, Generation: 1, EndpointAddress: 0x02,
		EndpointAttributes: 0x05, EndpointInterval: 1, EndpointMaxPacketSize: 4,
	}

	resetDone := make(chan error, 1)
	go func() {
		op := base
		op.Kind = udecx.OperationEndpointReset
		resetDone <- processor.Lifecycle(context.Background(), dev, op)
	}()
	select {
	case <-dev.resetStarted:
	case <-time.After(time.Second):
		t.Fatal("endpoint reset did not reach the controller engine")
	}

	startDone := make(chan error, 1)
	go func() {
		op := base
		op.Kind = udecx.OperationEndpointStart
		startDone <- processor.Lifecycle(context.Background(), dev, op)
	}()
	select {
	case event := <-dev.altChanged:
		close(dev.resetRelease)
		t.Fatalf("endpoint start changed alternate setting during reset: %v", event)
	case <-time.After(25 * time.Millisecond):
	}

	close(dev.resetRelease)
	if err := <-resetDone; err != nil {
		t.Fatal(err)
	}
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-dev.altChanged:
		if event != [2]uint8{2, 1} {
			t.Fatalf("alternate setting event=%v want=[2 1]", event)
		}
	case <-time.After(time.Second):
		t.Fatal("endpoint start did not resume after reset")
	}
}

func TestNativeProcessorDoesNotGloballySerializeIndependentDevices(t *testing.T) {
	desc := &usbdevice.Descriptor{Interfaces: []usbdevice.InterfaceConfig{
		{Descriptor: usbdevice.InterfaceDescriptor{BInterfaceNumber: 2}},
		{Descriptor: usbdevice.InterfaceDescriptor{
			BInterfaceNumber: 2, BAlternateSetting: 1, BNumEndpoints: 1,
		}, Endpoints: []usbdevice.EndpointDescriptor{{
			BEndpointAddress: 0x02, BMAttributes: 0x05,
			WMaxPacketSize: 4, BInterval: 1,
		}}},
	}}
	blocked := &lifecycleGateDevice{
		desc: desc, resetStarted: make(chan struct{}), resetRelease: make(chan struct{}),
		altChanged: make(chan [2]uint8, 1),
	}
	independent := &lifecycleGateDevice{
		desc: desc, resetStarted: make(chan struct{}), resetRelease: make(chan struct{}),
		altChanged: make(chan [2]uint8, 1),
	}
	processor := nativeProcessorForTest(t)
	resetDone := make(chan error, 1)
	go func() {
		resetDone <- processor.Lifecycle(context.Background(), blocked, udecx.Operation{
			DeviceID: 101, Generation: 3, Kind: udecx.OperationEndpointReset,
			EndpointAddress: 0x02, EndpointAttributes: 0x05,
			EndpointInterval: 1, EndpointMaxPacketSize: 4,
		})
	}()
	select {
	case <-blocked.resetStarted:
	case <-time.After(time.Second):
		t.Fatal("first controller did not enter its blocked endpoint reset")
	}

	startDone := make(chan error, 1)
	go func() {
		startDone <- processor.Lifecycle(context.Background(), independent, udecx.Operation{
			DeviceID: 102, Generation: 8, Kind: udecx.OperationEndpointStart,
			EndpointAddress: 0x02, EndpointAttributes: 0x05,
			EndpointInterval: 1, EndpointMaxPacketSize: 4,
		})
	}()
	select {
	case event := <-independent.altChanged:
		if event != [2]uint8{2, 1} {
			close(blocked.resetRelease)
			t.Fatalf("independent alternate setting event=%v want=[2 1]", event)
		}
	case <-time.After(100 * time.Millisecond):
		close(blocked.resetRelease)
		t.Fatal("one controller's reset blocked an independent controller")
	}
	if err := <-startDone; err != nil {
		close(blocked.resetRelease)
		t.Fatal(err)
	}
	close(blocked.resetRelease)
	if err := <-resetDone; err != nil {
		t.Fatal(err)
	}
}

func TestNativeProcessorConcurrentMediaAndLifecycleSoak(t *testing.T) {
	desc := &usbdevice.Descriptor{Interfaces: []usbdevice.InterfaceConfig{
		{Descriptor: usbdevice.InterfaceDescriptor{BInterfaceNumber: 2}},
		{Descriptor: usbdevice.InterfaceDescriptor{
			BInterfaceNumber: 2, BAlternateSetting: 1, BNumEndpoints: 1,
		}, Endpoints: []usbdevice.EndpointDescriptor{{
			BEndpointAddress: 0x02, BMAttributes: 0x05,
			WMaxPacketSize: 4, BInterval: 1,
		}}},
	}}
	dev := &concurrentNativeTestDevice{desc: desc}
	processor := nativeProcessorForTest(t)
	identity := udecx.DeviceIdentity{DeviceID: 91, Generation: 14}
	base := udecx.Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x02, EndpointAttributes: 0x05,
		EndpointInterval: 1, EndpointMaxPacketSize: 4,
		TransferFlags: udecx.TransferFlagStartIsoASAP,
	}

	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := range 25 {
				if (worker+iteration)%2 == 0 {
					op := base
					op.Kind = udecx.OperationEndpointStart
					if err := processor.Lifecycle(context.Background(), dev, op); err != nil {
						t.Errorf("endpoint start: %v", err)
						return
					}
				} else {
					op := base
					op.Kind = udecx.OperationEndpointPurge
					if err := processor.Lifecycle(context.Background(), dev, op); err != nil {
						t.Errorf("endpoint purge: %v", err)
						return
					}
				}

				op := base
				op.Token = uint64(worker*25 + iteration + 1)
				op.Kind = udecx.OperationTransfer
				op.TransferLength = 4
				op.Payload = []byte{1, 2, 3, 4}
				op.IsoPackets = []udecx.IsoPacket{{Offset: 0, Length: 4}}
				if _, err := processor.Process(context.Background(), dev, op); err != nil {
					t.Errorf("ISO transfer: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	processor.Reset(dev, identity)

	if got := dev.transfers.Load(); got != 100 {
		t.Fatalf("processed %d transfers, want 100", got)
	}
	processor.mu.Lock()
	defer processor.mu.Unlock()
	if len(processor.next) != 0 || len(processor.lastIn) != 0 {
		t.Fatalf("reset retained clocks=%d cached-input=%d", len(processor.next), len(processor.lastIn))
	}
	if len(processor.sessions) != 0 {
		t.Fatalf("reset retained %d native sessions", len(processor.sessions))
	}
}

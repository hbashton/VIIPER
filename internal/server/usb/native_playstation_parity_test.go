package usb_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"testing"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	usbserver "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
	usbdevice "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

const (
	dualSenseHIDInterface = 3
	hidRequestTypeOut     = 0x21
	hidRequestSetReport   = 0x09
	hidReportTypeOutput   = 0x02
)

// playStationParityHarness treats the established USB/IP adapter and the
// controller engine behind it as the oracle. Native operations are applied to
// an independent controller instance and compared at the controller-stream
// boundary. The harness deliberately does not duplicate media algorithms.
type playStationParityHarness struct {
	native   *usbserver.NativeProcessor
	identity udecx.DeviceIdentity
	token    uint64
}

func newPlayStationParityHarness(t *testing.T) *playStationParityHarness {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	native, err := usbserver.NewNativeProcessor(usbserver.New(usbserver.ServerConfig{}, logger, nil))
	if err != nil {
		t.Fatal(err)
	}

	return &playStationParityHarness{
		native:   native,
		identity: udecx.DeviceIdentity{DeviceID: 0x5053, Generation: 1},
	}
}

func (h *playStationParityHarness) nextToken() uint64 {
	h.token++
	return h.token
}

func parityEndpoint(t *testing.T, dev usbdevice.Device, address uint8) usbdevice.EndpointDescriptor {
	t.Helper()
	for _, iface := range dev.GetDescriptor().Interfaces {
		for _, endpoint := range iface.Endpoints {
			if endpoint.BEndpointAddress == address {
				return endpoint
			}
		}
	}
	t.Fatalf("endpoint 0x%02x is absent from the controller descriptor", address)
	return usbdevice.EndpointDescriptor{}
}

func endpointOperation(t *testing.T, identity udecx.DeviceIdentity, kind udecx.OperationKind,
	speed uint32, endpoint usbdevice.EndpointDescriptor,
) udecx.Operation {
	t.Helper()
	endpoint, err := udecx.EndpointDescriptorForNativeUdeCx(udecx.DeviceSpeed(speed), endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return udecx.Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation, Kind: kind,
		EndpointAddress:       endpoint.BEndpointAddress,
		EndpointAttributes:    endpoint.BMAttributes,
		EndpointInterval:      endpoint.BInterval,
		EndpointMaxPacketSize: endpoint.WMaxPacketSize,
	}
}

func (h *playStationParityHarness) nativeLifecycle(t *testing.T, dev usbdevice.Device,
	kind udecx.OperationKind, address uint8,
) {
	t.Helper()
	op := udecx.Operation{
		DeviceID: h.identity.DeviceID, Generation: h.identity.Generation, Kind: kind,
	}
	if address != 0 {
		op = endpointOperation(t, h.identity, kind, dev.GetDescriptor().Device.Speed,
			parityEndpoint(t, dev, address))
	}
	if err := h.native.Lifecycle(context.Background(), dev, op); err != nil {
		t.Fatalf("native lifecycle kind %d endpoint 0x%02x: %v", kind, address, err)
	}
}

func setupPacket(bmRequestType, request uint8, value, index, length uint16) [8]byte {
	var setup [8]byte
	setup[0] = bmRequestType
	setup[1] = request
	binary.LittleEndian.PutUint16(setup[2:4], value)
	binary.LittleEndian.PutUint16(setup[4:6], index)
	binary.LittleEndian.PutUint16(setup[6:8], length)
	return setup
}

func (h *playStationParityHarness) legacySetInterface(dev usbdevice.Device, iface, alt uint8) {
	dev.(usbdevice.InterfaceAltSettingDevice).SetInterfaceAltSetting(iface, alt)
}

func (h *playStationParityHarness) legacyResetEndpoint(dev usbdevice.Device, address uint8) {
	dev.(usbdevice.EndpointResetDevice).ResetEndpoint(address)
}

func (h *playStationParityHarness) legacyOutput(dev usbdevice.Device, address uint8, payload []byte) {
	dev.HandleTransfer(context.Background(), uint32(address&0x0f),
		usbdevice.DirectionOut, payload)
}

func (h *playStationParityHarness) nativeOutput(t *testing.T, dev usbdevice.Device,
	address uint8, payload []byte,
) {
	t.Helper()
	endpoint := parityEndpoint(t, dev, address)
	op := endpointOperation(t, h.identity, udecx.OperationTransfer,
		dev.GetDescriptor().Device.Speed, endpoint)
	op.Token = h.nextToken()
	op.TransferLength = uint32(len(payload))
	op.Payload = append([]byte(nil), payload...)
	completion, err := h.native.Process(context.Background(), dev, op)
	if err != nil {
		t.Fatalf("native OUT endpoint 0x%02x: %v", address, err)
	}
	if completion.TransferLength != uint32(len(payload)) || len(completion.Payload) != 0 {
		t.Fatalf("native OUT completion length=%d payload=% x, want length=%d and no echo",
			completion.TransferLength, completion.Payload, len(payload))
	}
}

func (h *playStationParityHarness) legacyHIDSetReport(t *testing.T, dev usbdevice.Device,
	interfaceNumber, reportID uint8, payload []byte,
) {
	t.Helper()
	_, handled := dev.(usbdevice.ControlDevice).HandleControl(
		hidRequestTypeOut, hidRequestSetReport,
		uint16(hidReportTypeOutput)<<8|uint16(reportID),
		uint16(interfaceNumber), uint16(len(payload)), payload)
	if !handled {
		t.Fatal("USB/IP oracle rejected HID SET_REPORT")
	}
}

func (h *playStationParityHarness) nativeHIDSetReport(t *testing.T, dev usbdevice.Device,
	interfaceNumber, reportID uint8, payload []byte,
) {
	t.Helper()
	op := udecx.Operation{
		Token: h.nextToken(), DeviceID: h.identity.DeviceID, Generation: h.identity.Generation,
		Kind: udecx.OperationControl, Direction: 0, TransferLength: uint32(len(payload)),
		SetupPacket: setupPacket(hidRequestTypeOut, hidRequestSetReport,
			uint16(hidReportTypeOutput)<<8|uint16(reportID), uint16(interfaceNumber), uint16(len(payload))),
		Payload: append([]byte(nil), payload...),
	}
	completion, err := h.native.Process(context.Background(), dev, op)
	if err != nil {
		t.Fatalf("native HID SET_REPORT: %v", err)
	}
	if completion.TransferLength != uint32(len(payload)) || len(completion.Payload) != 0 {
		t.Fatalf("native SET_REPORT completion=%+v", completion)
	}
}

func sequentialIsoPackets(totalBytes, packetBytes int) []udecx.IsoPacket {
	packets := make([]udecx.IsoPacket, 0, (totalBytes+packetBytes-1)/packetBytes)
	for offset := 0; offset < totalBytes; offset += packetBytes {
		length := min(packetBytes, totalBytes-offset)
		packets = append(packets, udecx.IsoPacket{Offset: uint32(offset), Length: uint32(length)})
	}
	return packets
}

func sparseIsoPackets(count int, packetBytes, gap uint32) ([]udecx.IsoPacket, uint32) {
	packets := make([]udecx.IsoPacket, count)
	var transferLength uint32
	for index := range packets {
		offset := uint32(index) * (packetBytes + gap)
		packets[index] = udecx.IsoPacket{Offset: offset, Length: packetBytes}
		transferLength = offset + packetBytes
	}
	return packets, transferLength
}

func (h *playStationParityHarness) nativeISO(t *testing.T, dev usbdevice.Device,
	address uint8, transferLength uint32, payload []byte, packets []udecx.IsoPacket,
) udecx.Completion {
	t.Helper()
	endpoint := parityEndpoint(t, dev, address)
	op := endpointOperation(t, h.identity, udecx.OperationTransfer,
		dev.GetDescriptor().Device.Speed, endpoint)
	op.Token = h.nextToken()
	op.TransferLength = transferLength
	op.IsoPackets = append([]udecx.IsoPacket(nil), packets...)
	op.TransferFlags = udecx.TransferFlagStartIsoASAP
	if address&0x80 != 0 {
		op.Direction = 1
		op.TransferFlags |= udecx.TransferFlagDirectionIn
	} else {
		op.Payload = append([]byte(nil), payload...)
	}
	completion, err := h.native.Process(context.Background(), dev, op)
	if err != nil {
		t.Fatalf("native ISO endpoint 0x%02x: %v", address, err)
	}
	return completion
}

func legacyISOIn(t *testing.T, dev usbdevice.Device, address uint8,
	packets []udecx.IsoPacket,
) ([]byte, []usbip.IsoPacketDescriptor) {
	t.Helper()
	payload := make([]byte, 0)
	completed := make([]usbip.IsoPacketDescriptor, len(packets))
	for index, packet := range packets {
		packetData := dev.HandleTransfer(context.Background(), uint32(address&0x0f),
			usbdevice.DirectionIn, nil)
		actual := min(packet.Length, uint32(len(packetData)))
		payload = append(payload, packetData[:actual]...)
		completed[index] = usbip.IsoPacketDescriptor{
			Offset: packet.Offset, Length: packet.Length, ActualLength: actual,
		}
	}
	return payload, completed
}

func compactNativeISO(t *testing.T, completion udecx.Completion) ([]byte, []uint32) {
	t.Helper()
	compact := make([]byte, 0, completion.TransferLength)
	lengths := make([]uint32, len(completion.IsoPackets))
	for index, packet := range completion.IsoPackets {
		end := packet.Offset + packet.Length
		if end > uint32(len(completion.Payload)) {
			t.Fatalf("native completed packet %d exceeds payload: %+v payload=%d",
				index, packet, len(completion.Payload))
		}
		compact = append(compact, completion.Payload[packet.Offset:end]...)
		lengths[index] = packet.Length
	}
	return compact, lengths
}

func compactLegacyISOLengths(completed []usbip.IsoPacketDescriptor) []uint32 {
	lengths := make([]uint32, len(completed))
	for index, packet := range completed {
		lengths[index] = packet.ActualLength
	}
	return lengths
}

func normalizeDualSenseInput(report []byte) []byte {
	normalized := append([]byte(nil), report...)
	if len(normalized) >= dualsense.InputReportSize {
		clear(normalized[28:32])
		clear(normalized[49:53])
	}
	return normalized
}

func normalizeDualShock4Input(report []byte) []byte {
	normalized := append([]byte(nil), report...)
	if len(normalized) >= dualshock4.InputReportSize {
		clear(normalized[10:12])
	}
	return normalized
}

func patternedPCM(size int, seed byte) []byte {
	pcm := make([]byte, size)
	for index := range pcm {
		pcm[index] = seed + byte(index*29+index/7)
	}
	return pcm
}

func requireDeviceBool(t *testing.T, dev usbdevice.Device, key string, want bool) {
	t.Helper()
	got, ok := dev.GetDeviceSpecificArgs()[key].(bool)
	if !ok || got != want {
		t.Fatalf("device state %s=%v (bool=%t), want %t", key, got, ok, want)
	}
}

type dualSenseAtomicCapture struct {
	feedback dualsense.OutputState
	speaker  []byte
}

type dualSenseParityCapture struct {
	outputs  []dualsense.OutputState
	atomic   []dualSenseAtomicCapture
	realtime []dualsense.OutputState
	resets   int
	events   []string
}

func (capture *dualSenseParityCapture) attach(dev *dualsense.DualSense) {
	dev.SetOutputCallback(func(state dualsense.OutputState) {
		capture.outputs = append(capture.outputs, state)
		capture.events = append(capture.events, "output")
	})
	dev.SetAtomicAudioHapticsCallback(func(state dualsense.OutputState, speaker []byte) {
		capture.atomic = append(capture.atomic, dualSenseAtomicCapture{
			feedback: state, speaker: append([]byte(nil), speaker...),
		})
		capture.events = append(capture.events, "atomic")
	})
	dev.SetRealtimeHapticsCallback(func(state dualsense.OutputState) {
		capture.realtime = append(capture.realtime, state)
		capture.events = append(capture.events, "realtime")
	})
	dev.SetSpeakerResetCallback(func() {
		capture.resets++
		capture.events = append(capture.events, "reset")
	})
}

func dualSensePCM(frames int, bias int16) []byte {
	pcm := make([]byte, frames*dualsense.USBHapticsAudioFrameSize)
	for frame := 0; frame < frames; frame++ {
		offset := frame * dualsense.USBHapticsAudioFrameSize
		binary.LittleEndian.PutUint16(pcm[offset:offset+2], uint16(bias+int16(frame)))
		binary.LittleEndian.PutUint16(pcm[offset+2:offset+4], uint16(-bias-int16(frame)))
		binary.LittleEndian.PutUint16(pcm[offset+4:offset+6], uint16(2*bias+int16(frame*3)))
		binary.LittleEndian.PutUint16(pcm[offset+6:offset+8], uint16(-2*bias-int16(frame*3)))
	}
	return pcm
}

func dualSenseFrontStereo(pcm []byte) []byte {
	frames := len(pcm) / dualsense.USBHapticsAudioFrameSize
	front := make([]byte, frames*dualsense.USBHapticsAudioBytesPerSample*2)
	for frame := 0; frame < frames; frame++ {
		copy(front[frame*4:frame*4+4],
			pcm[frame*dualsense.USBHapticsAudioFrameSize:frame*dualsense.USBHapticsAudioFrameSize+4])
	}
	return front
}

func requireDualSenseCapturesEqual(t *testing.T, legacy, native *dualSenseParityCapture) {
	t.Helper()
	if len(legacy.outputs) != len(native.outputs) ||
		len(legacy.atomic) != len(native.atomic) ||
		len(legacy.realtime) != len(native.realtime) || legacy.resets != native.resets ||
		!bytes.Equal([]byte(joinParityEvents(legacy.events)), []byte(joinParityEvents(native.events))) {
		t.Fatalf("DualSense callback boundary mismatch:\nlegacy outputs=%d atomic=%d realtime=%d resets=%d events=%v\nnative outputs=%d atomic=%d realtime=%d resets=%d events=%v",
			len(legacy.outputs), len(legacy.atomic), len(legacy.realtime), legacy.resets, legacy.events,
			len(native.outputs), len(native.atomic), len(native.realtime), native.resets, native.events)
	}
	for index := range legacy.outputs {
		if legacy.outputs[index] != native.outputs[index] {
			t.Fatalf("DualSense output state %d differs across transports", index)
		}
	}
	for index := range legacy.atomic {
		if legacy.atomic[index].feedback != native.atomic[index].feedback ||
			!bytes.Equal(legacy.atomic[index].speaker, native.atomic[index].speaker) {
			t.Fatalf("DualSense atomic media generation %d differs across transports", index)
		}
	}
	for index := range legacy.realtime {
		if legacy.realtime[index] != native.realtime[index] {
			t.Fatalf("DualSense realtime haptics generation %d differs across transports", index)
		}
	}
}

func joinParityEvents(events []string) string {
	var joined string
	for _, event := range events {
		joined += event + "\x00"
	}
	return joined
}

func TestNativeDualSenseMatchesUSBIPOracle(t *testing.T) {
	harness := newPlayStationParityHarness(t)
	legacy, err := dualsense.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	native, err := dualsense.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyCapture := &dualSenseParityCapture{}
	nativeCapture := &dualSenseParityCapture{}
	legacyCapture.attach(legacy)
	nativeCapture.attach(native)

	t.Run("native fast HID input preserves state bytes", func(t *testing.T) {
		state := dualsense.NewInputState()
		state.LX, state.LY, state.RX, state.RY = -101, 87, 45, -32
		state.Buttons = dualsense.ButtonCross | dualsense.ButtonR1 | dualsense.ButtonPS
		state.DPad = dualsense.DPadUp | dualsense.DPadRight
		state.L2, state.R2 = 0x39, 0xe4
		state.Touch1Active, state.Touch1Tracking = true, 7
		state.Touch1X, state.Touch1Y = 1234, 567
		state.GyroX, state.GyroY, state.GyroZ = 101, -202, 303
		legacy.UpdateInputState(state)
		native.UpdateInputState(state)

		legacyReport := legacy.HandleTransfer(context.Background(),
			uint32(dualsense.EndpointIn&0x0f), usbdevice.DirectionIn, nil)
		nativeReport := make([]byte, dualsense.InputReportSize)
		written, readErr := native.ReadInterruptInput(context.Background(),
			uint32(dualsense.EndpointIn), nativeReport)
		if readErr != nil || written != dualsense.InputReportSize {
			t.Fatalf("native DualSense HID read wrote %d: %v", written, readErr)
		}
		if !bytes.Equal(normalizeDualSenseInput(legacyReport), normalizeDualSenseInput(nativeReport)) {
			t.Fatalf("DualSense HID state differs:\nlegacy=% x\nnative=% x", legacyReport, nativeReport)
		}
		if nativeReport[1] != uint8(int16(state.LX)+128) || nativeReport[5] != state.L2 ||
			nativeReport[6] != state.R2 || nativeReport[34] != byte(state.Touch1X) {
			t.Fatalf("native DualSense HID report did not encode the requested state: % x", nativeReport)
		}
	})

	t.Run("HID feedback preserves rumble lightbar player LEDs and triggers", func(t *testing.T) {
		triggers := make([]byte, dualsense.OutputReportSize)
		triggers[0] = dualsense.ReportIDOutput
		triggers[1] = 0x0c
		copy(triggers[11:21], []byte{0x21, 0xf0, 0x03, 0x04, 0x05, 0x06, 0x07, 0, 0, 0x44})
		copy(triggers[22:32], []byte{0x25, 0x40, 0x05, 0x14, 0x15, 0x16, 0x17, 0, 0, 0x55})
		harness.legacyOutput(legacy, dualsense.EndpointOut, triggers)
		harness.nativeOutput(t, native, dualsense.EndpointOut, triggers)

		visualRumble := make([]byte, dualsense.OutputReportSize)
		visualRumble[0] = dualsense.ReportIDOutput
		visualRumble[1] = 0x03
		visualRumble[2] = 0x14
		visualRumble[3], visualRumble[4] = 0x2a, 0xb4
		visualRumble[44] = 0x1f
		visualRumble[45], visualRumble[46], visualRumble[47] = 0x12, 0x67, 0xcd
		harness.legacyHIDSetReport(t, legacy, dualSenseHIDInterface, dualsense.ReportIDOutput, visualRumble)
		harness.nativeHIDSetReport(t, native, dualSenseHIDInterface,
			dualsense.ReportIDOutput, visualRumble)

		requireDualSenseCapturesEqual(t, legacyCapture, nativeCapture)
		got := nativeCapture.outputs[len(nativeCapture.outputs)-1]
		if got.RumbleSmall != 0x2a || got.RumbleLarge != 0xb4 ||
			got.LedRed != 0x12 || got.LedGreen != 0x67 || got.LedBlue != 0xcd ||
			got.PlayerLeds != 0x1f || got.TriggerR2Mode != 0x21 ||
			got.TriggerL2Mode != 0x25 || !bytes.Equal(got.RawOutputReport[:], visualRumble) {
			t.Fatalf("native DualSense feedback state lost a host field: %+v", got)
		}
	})

	t.Run("haptics OUT preserves bytes and independent 480/512-frame boundaries", func(t *testing.T) {
		harness.legacySetInterface(legacy, dualsense.InterfaceHapticsAudio, 1)
		harness.nativeLifecycle(t, native, udecx.OperationEndpointStart,
			dualsense.EndpointHapticsAudioOut)
		requireDeviceBool(t, legacy, "speakerInterfaceActive", true)
		requireDeviceBool(t, native, "speakerInterfaceActive", true)
		requireDualSenseCapturesEqual(t, legacyCapture, nativeCapture)

		pcm := dualSensePCM(512, 700)
		parts := [][2]int{{0, 240}, {240, 480}, {480, 512}}
		for index, part := range parts {
			payload := pcm[part[0]*dualsense.USBHapticsAudioFrameSize : part[1]*dualsense.USBHapticsAudioFrameSize]
			packets := sequentialIsoPackets(len(payload), dualsense.USBHapticsAudioPacketSize)
			legacy.HandleTransfer(context.Background(),
				uint32(dualsense.EndpointHapticsAudioOut&0x0f), usbdevice.DirectionOut, payload)
			completion := harness.nativeISO(t, native, dualsense.EndpointHapticsAudioOut,
				uint32(len(payload)), payload, packets)
			if completion.TransferLength != uint32(len(payload)) || len(completion.Payload) != 0 ||
				len(completion.IsoPackets) != len(packets) {
				t.Fatalf("native DualSense ISO OUT part %d completion=%+v", index, completion)
			}
			requireDualSenseCapturesEqual(t, legacyCapture, nativeCapture)
			switch index {
			case 0:
				if len(nativeCapture.atomic) != 0 || len(nativeCapture.realtime) != 0 {
					t.Fatal("DualSense emitted media before either source-clock boundary")
				}
			case 1:
				if len(nativeCapture.atomic) != 1 || len(nativeCapture.realtime) != 0 {
					t.Fatal("DualSense 480-frame speaker boundary was not independent")
				}
				wantSpeaker := dualSenseFrontStereo(pcm[:480*dualsense.USBHapticsAudioFrameSize])
				if !bytes.Equal(nativeCapture.atomic[0].speaker, wantSpeaker) {
					t.Fatal("native DualSense front-channel PCM changed byte order")
				}
			case 2:
				if len(nativeCapture.atomic) != 1 || len(nativeCapture.realtime) != 1 {
					t.Fatal("DualSense 512-frame haptics boundary was not preserved")
				}
			}
		}

		// Leave 32 old speaker frames pending, reset the pipe, then prove that
		// 448 fresh frames cannot complete a stale 480-frame generation.
		harness.legacyResetEndpoint(legacy, dualsense.EndpointHapticsAudioOut)
		harness.nativeLifecycle(t, native, udecx.OperationEndpointReset,
			dualsense.EndpointHapticsAudioOut)
		requireDualSenseCapturesEqual(t, legacyCapture, nativeCapture)
		requireDeviceBool(t, legacy, "speakerInterfaceActive", true)
		requireDeviceBool(t, native, "speakerInterfaceActive", true)

		fresh := dualSensePCM(480, 2_000)
		for _, part := range [][2]int{{0, 448}, {448, 480}} {
			payload := fresh[part[0]*dualsense.USBHapticsAudioFrameSize : part[1]*dualsense.USBHapticsAudioFrameSize]
			packets := sequentialIsoPackets(len(payload), dualsense.USBHapticsAudioPacketSize)
			legacy.HandleTransfer(context.Background(),
				uint32(dualsense.EndpointHapticsAudioOut&0x0f), usbdevice.DirectionOut, payload)
			harness.nativeISO(t, native, dualsense.EndpointHapticsAudioOut,
				uint32(len(payload)), payload, packets)
			if part[1] == 448 && len(nativeCapture.atomic) != 1 {
				t.Fatal("stale DualSense speaker PCM crossed the endpoint reset")
			}
		}
		requireDualSenseCapturesEqual(t, legacyCapture, nativeCapture)
		if len(nativeCapture.atomic) != 2 ||
			!bytes.Equal(nativeCapture.atomic[1].speaker, dualSenseFrontStereo(fresh)) {
			t.Fatal("fresh DualSense speaker generation was not byte-exact after reset")
		}

		harness.legacySetInterface(legacy, dualsense.InterfaceHapticsAudio, 0)
		harness.nativeLifecycle(t, native, udecx.OperationEndpointPurge,
			dualsense.EndpointHapticsAudioOut)
		requireDeviceBool(t, legacy, "speakerInterfaceActive", false)
		requireDeviceBool(t, native, "speakerInterfaceActive", false)
		requireDualSenseCapturesEqual(t, legacyCapture, nativeCapture)
	})

	t.Run("microphone IN preserves sparse packet bytes and reset priming", func(t *testing.T) {
		harness.legacySetInterface(legacy, dualsense.InterfaceMicrophone, 1)
		harness.nativeLifecycle(t, native, udecx.OperationEndpointStart,
			dualsense.EndpointMicrophoneIn)
		requireDeviceBool(t, legacy, "microphoneInterfaceActive", true)
		requireDeviceBool(t, native, "microphoneInterfaceActive", true)

		queued := make([]byte, 0, 6*dualsense.USBMicrophoneClientFrameSize)
		for frame := 0; frame < 6; frame++ {
			pcm := patternedPCM(dualsense.USBMicrophoneClientFrameSize, byte(0x10+frame*17))
			legacy.QueueMicrophonePCMFrame(pcm)
			native.QueueMicrophonePCMFrame(pcm)
			queued = append(queued, pcm...)
		}
		packets, transferLength := sparseIsoPackets(3, dualsense.USBMicrophoneMaxPacketSize, 11)
		legacyPayload, legacyCompleted := legacyISOIn(t, legacy,
			dualsense.EndpointMicrophoneIn, packets)
		nativeCompletion := harness.nativeISO(t, native, dualsense.EndpointMicrophoneIn,
			transferLength, nil, packets)
		nativePayload, nativeLengths := compactNativeISO(t, nativeCompletion)
		legacyLengths := compactLegacyISOLengths(legacyCompleted)
		if !bytes.Equal(legacyPayload, nativePayload) ||
			!bytes.Equal(uint32sAsBytes(legacyLengths), uint32sAsBytes(nativeLengths)) ||
			!bytes.Equal(nativePayload, queued[:len(nativePayload)]) {
			t.Fatalf("DualSense microphone packet parity failed:\nlegacy lengths=%v payload=% x\nnative lengths=%v payload=% x",
				legacyLengths, legacyPayload, nativeLengths, nativePayload)
		}

		harness.legacyResetEndpoint(legacy, dualsense.EndpointMicrophoneIn)
		harness.nativeLifecycle(t, native, udecx.OperationEndpointReset,
			dualsense.EndpointMicrophoneIn)
		requireDeviceBool(t, legacy, "microphoneInterfaceActive", true)
		requireDeviceBool(t, native, "microphoneInterfaceActive", true)
		zeroPackets, zeroLength := sparseIsoPackets(1, dualsense.USBMicrophoneMaxPacketSize, 0)
		legacyZero, legacyZeroCompleted := legacyISOIn(t, legacy,
			dualsense.EndpointMicrophoneIn, zeroPackets)
		nativeZeroCompletion := harness.nativeISO(t, native, dualsense.EndpointMicrophoneIn,
			zeroLength, nil, zeroPackets)
		nativeZero, nativeZeroLengths := compactNativeISO(t, nativeZeroCompletion)
		if !bytes.Equal(legacyZero, nativeZero) ||
			!bytes.Equal(nativeZero, make([]byte, dualsense.USBMicrophonePacketSize)) ||
			legacyZeroCompleted[0].ActualLength != uint32(dualsense.USBMicrophonePacketSize) ||
			nativeZeroLengths[0] != uint32(dualsense.USBMicrophonePacketSize) {
			t.Fatalf("DualSense reset silence differs: legacy=% x native=% x", legacyZero, nativeZero)
		}

		freshQueued := make([]byte, 0, 6*dualsense.USBMicrophoneClientFrameSize)
		for frame := 0; frame < 6; frame++ {
			pcm := patternedPCM(dualsense.USBMicrophoneClientFrameSize, byte(0x90+frame*9))
			legacy.QueueMicrophonePCMFrame(pcm)
			native.QueueMicrophonePCMFrame(pcm)
			freshQueued = append(freshQueued, pcm...)
		}
		legacyFresh, _ := legacyISOIn(t, legacy,
			dualsense.EndpointMicrophoneIn, zeroPackets)
		nativeFreshCompletion := harness.nativeISO(t, native, dualsense.EndpointMicrophoneIn,
			zeroLength, nil, zeroPackets)
		nativeFresh, _ := compactNativeISO(t, nativeFreshCompletion)
		if !bytes.Equal(legacyFresh, nativeFresh) ||
			!bytes.Equal(nativeFresh, freshQueued[:len(nativeFresh)]) {
			t.Fatal("DualSense microphone reset replayed stale capture bytes")
		}

		harness.legacySetInterface(legacy, dualsense.InterfaceMicrophone, 0)
		harness.nativeLifecycle(t, native, udecx.OperationEndpointPurge,
			dualsense.EndpointMicrophoneIn)
		requireDeviceBool(t, legacy, "microphoneInterfaceActive", false)
		requireDeviceBool(t, native, "microphoneInterfaceActive", false)
	})
}

func uint32sAsBytes(values []uint32) []byte {
	result := make([]byte, len(values)*4)
	for index, value := range values {
		binary.LittleEndian.PutUint32(result[index*4:index*4+4], value)
	}
	return result
}

type dualShock4ParityCapture struct {
	outputs []dualshock4.OutputState
	speaker [][]byte
	resets  int
	events  []string
}

func (capture *dualShock4ParityCapture) attach(dev *dualshock4.DualShock4) {
	dev.SetOutputCallback(func(state dualshock4.OutputState) {
		capture.outputs = append(capture.outputs, state)
		capture.events = append(capture.events, "output")
	})
	dev.SetSpeakerCallback(func(pcm []byte) {
		capture.speaker = append(capture.speaker, append([]byte(nil), pcm...))
		capture.events = append(capture.events, "speaker")
	})
	dev.SetSpeakerResetCallback(func() {
		capture.resets++
		capture.events = append(capture.events, "reset")
	})
}

func requireDualShock4CapturesEqual(t *testing.T, legacy, native *dualShock4ParityCapture) {
	t.Helper()
	if len(legacy.outputs) != len(native.outputs) || len(legacy.speaker) != len(native.speaker) ||
		legacy.resets != native.resets || joinParityEvents(legacy.events) != joinParityEvents(native.events) {
		t.Fatalf("DualShock 4 callback boundary mismatch:\nlegacy outputs=%d speaker=%d resets=%d events=%v\nnative outputs=%d speaker=%d resets=%d events=%v",
			len(legacy.outputs), len(legacy.speaker), legacy.resets, legacy.events,
			len(native.outputs), len(native.speaker), native.resets, native.events)
	}
	for index := range legacy.outputs {
		if legacy.outputs[index] != native.outputs[index] {
			t.Fatalf("DualShock 4 output state %d differs across transports", index)
		}
	}
	for index := range legacy.speaker {
		if !bytes.Equal(legacy.speaker[index], native.speaker[index]) {
			t.Fatalf("DualShock 4 speaker generation %d differs across transports", index)
		}
	}
}

func TestNativeDualShock4MatchesUSBIPOracle(t *testing.T) {
	harness := newPlayStationParityHarness(t)
	legacy, err := dualshock4.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	native, err := dualshock4.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyCapture := &dualShock4ParityCapture{}
	nativeCapture := &dualShock4ParityCapture{}
	legacyCapture.attach(legacy)
	nativeCapture.attach(native)

	t.Run("native fast HID input preserves state bytes", func(t *testing.T) {
		state := dualshock4.NewInputState()
		state.LX, state.LY, state.RX, state.RY = -95, 71, 44, -23
		state.Buttons = dualshock4.ButtonCircle | dualshock4.ButtonL1 |
			dualshock4.ButtonPS | dualshock4.ButtonTouchpadClick
		state.DPad = dualshock4.DPadDown | dualshock4.DPadLeft
		state.L2, state.R2 = 0x28, 0xdd
		state.Touch2Active, state.Touch2X, state.Touch2Y = true, 777, 999
		state.GyroX, state.GyroY, state.GyroZ = -111, 222, -333
		legacy.UpdateInputState(state)
		native.UpdateInputState(state)

		legacyReport := legacy.HandleTransfer(context.Background(),
			uint32(dualshock4.EndpointIn&0x0f), usbdevice.DirectionIn, nil)
		nativeReport := make([]byte, dualshock4.InputReportSize)
		written, readErr := native.ReadInterruptInput(context.Background(),
			uint32(dualshock4.EndpointIn), nativeReport)
		if readErr != nil || written != dualshock4.InputReportSize {
			t.Fatalf("native DualShock 4 HID read wrote %d: %v", written, readErr)
		}
		if !bytes.Equal(normalizeDualShock4Input(legacyReport), normalizeDualShock4Input(nativeReport)) {
			t.Fatalf("DualShock 4 HID state differs:\nlegacy=% x\nnative=% x", legacyReport, nativeReport)
		}
		if nativeReport[1] != uint8(int16(state.LX)+128) || nativeReport[8] != state.L2 ||
			nativeReport[9] != state.R2 || nativeReport[7]&0x03 != 0x03 {
			t.Fatalf("native DualShock 4 HID report did not encode the requested state: % x", nativeReport)
		}
	})

	t.Run("HID feedback preserves rumble lightbar and flash state", func(t *testing.T) {
		first := []byte{dualshock4.ReportIDOutput, 0, 0, 0, 0x12, 0xfe, 1, 2, 3, 4, 5}
		harness.legacyOutput(legacy, dualshock4.EndpointOut, first)
		harness.nativeOutput(t, native, dualshock4.EndpointOut, first)

		second := []byte{dualshock4.ReportIDOutput, 0, 0, 0, 0x39, 0xa4, 0x10, 0x20, 0x30, 0x40, 0x50}
		harness.legacyHIDSetReport(t, legacy, dualshock4.InterfaceHID,
			dualshock4.ReportIDOutput, second)
		harness.nativeHIDSetReport(t, native, dualshock4.InterfaceHID,
			dualshock4.ReportIDOutput, second)
		requireDualShock4CapturesEqual(t, legacyCapture, nativeCapture)
		got := nativeCapture.outputs[len(nativeCapture.outputs)-1]
		want := dualshock4.OutputState{
			RumbleSmall: 0x39, RumbleLarge: 0xa4,
			LedRed: 0x10, LedGreen: 0x20, LedBlue: 0x30,
			FlashOn: 0x40, FlashOff: 0x50,
		}
		if got != want {
			t.Fatalf("native DualShock 4 feedback=%+v want=%+v", got, want)
		}
	})

	t.Run("speaker OUT preserves URB byte order and reset boundaries", func(t *testing.T) {
		harness.legacySetInterface(legacy, dualshock4.InterfaceSpeaker, 1)
		harness.nativeLifecycle(t, native, udecx.OperationEndpointStart,
			dualshock4.EndpointAudioOut)
		requireDeviceBool(t, legacy, "speakerInterfaceActive", true)
		requireDeviceBool(t, native, "speakerInterfaceActive", true)
		requireDualShock4CapturesEqual(t, legacyCapture, nativeCapture)

		payloads := [][]byte{
			patternedPCM(4*128, 0x11),
			patternedPCM(3*128, 0x83),
		}
		for index, payload := range payloads {
			packets := sequentialIsoPackets(len(payload), 128)
			legacy.HandleTransfer(context.Background(),
				uint32(dualshock4.EndpointAudioOut&0x0f), usbdevice.DirectionOut, payload)
			completion := harness.nativeISO(t, native, dualshock4.EndpointAudioOut,
				uint32(len(payload)), payload, packets)
			if completion.TransferLength != uint32(len(payload)) || len(completion.IsoPackets) != len(packets) {
				t.Fatalf("native DualShock 4 ISO OUT part %d completion=%+v", index, completion)
			}
			requireDualShock4CapturesEqual(t, legacyCapture, nativeCapture)
		}
		if len(nativeCapture.speaker) != len(payloads) {
			t.Fatalf("native DualShock 4 combined or split speaker URBs: %d callbacks", len(nativeCapture.speaker))
		}
		for index := range payloads {
			if !bytes.Equal(nativeCapture.speaker[index], payloads[index]) {
				t.Fatalf("native DualShock 4 speaker payload %d changed byte order", index)
			}
		}

		harness.legacyResetEndpoint(legacy, dualshock4.EndpointAudioOut)
		harness.nativeLifecycle(t, native, udecx.OperationEndpointReset,
			dualshock4.EndpointAudioOut)
		requireDualShock4CapturesEqual(t, legacyCapture, nativeCapture)
		requireDeviceBool(t, legacy, "speakerInterfaceActive", true)
		requireDeviceBool(t, native, "speakerInterfaceActive", true)

		fresh := patternedPCM(2*128, 0x57)
		packets := sequentialIsoPackets(len(fresh), 128)
		legacy.HandleTransfer(context.Background(),
			uint32(dualshock4.EndpointAudioOut&0x0f), usbdevice.DirectionOut, fresh)
		harness.nativeISO(t, native, dualshock4.EndpointAudioOut,
			uint32(len(fresh)), fresh, packets)
		requireDualShock4CapturesEqual(t, legacyCapture, nativeCapture)
		if !bytes.Equal(nativeCapture.speaker[len(nativeCapture.speaker)-1], fresh) {
			t.Fatal("DualShock 4 endpoint reset changed the fresh speaker generation")
		}

		harness.legacySetInterface(legacy, dualshock4.InterfaceSpeaker, 0)
		harness.nativeLifecycle(t, native, udecx.OperationEndpointPurge,
			dualshock4.EndpointAudioOut)
		requireDeviceBool(t, legacy, "speakerInterfaceActive", false)
		requireDeviceBool(t, native, "speakerInterfaceActive", false)
		requireDualShock4CapturesEqual(t, legacyCapture, nativeCapture)
	})

	t.Run("microphone IN preserves sparse packet bytes and reset priming", func(t *testing.T) {
		harness.legacySetInterface(legacy, dualshock4.InterfaceMicrophone, 1)
		harness.nativeLifecycle(t, native, udecx.OperationEndpointStart,
			dualshock4.EndpointMicrophoneIn)
		requireDeviceBool(t, legacy, "microphoneInterfaceActive", true)
		requireDeviceBool(t, native, "microphoneInterfaceActive", true)

		queued := make([]byte, 0, 6*dualshock4.USBMicrophoneClientFrameSize)
		for frame := 0; frame < 6; frame++ {
			pcm := patternedPCM(dualshock4.USBMicrophoneClientFrameSize, byte(0x20+frame*13))
			legacy.QueueMicrophonePCMFrame(pcm)
			native.QueueMicrophonePCMFrame(pcm)
			queued = append(queued, pcm...)
		}
		packets, transferLength := sparseIsoPackets(4, dualshock4.USBMicrophoneMaxPacketSize, 7)
		legacyPayload, legacyCompleted := legacyISOIn(t, legacy,
			dualshock4.EndpointMicrophoneIn, packets)
		nativeCompletion := harness.nativeISO(t, native, dualshock4.EndpointMicrophoneIn,
			transferLength, nil, packets)
		nativePayload, nativeLengths := compactNativeISO(t, nativeCompletion)
		legacyLengths := compactLegacyISOLengths(legacyCompleted)
		if !bytes.Equal(legacyPayload, nativePayload) ||
			!bytes.Equal(uint32sAsBytes(legacyLengths), uint32sAsBytes(nativeLengths)) ||
			!bytes.Equal(nativePayload, queued[:len(nativePayload)]) {
			t.Fatalf("DualShock 4 microphone packet parity failed:\nlegacy lengths=%v payload=% x\nnative lengths=%v payload=% x",
				legacyLengths, legacyPayload, nativeLengths, nativePayload)
		}

		harness.legacyResetEndpoint(legacy, dualshock4.EndpointMicrophoneIn)
		harness.nativeLifecycle(t, native, udecx.OperationEndpointReset,
			dualshock4.EndpointMicrophoneIn)
		requireDeviceBool(t, legacy, "microphoneInterfaceActive", true)
		requireDeviceBool(t, native, "microphoneInterfaceActive", true)
		zeroPackets, zeroLength := sparseIsoPackets(1, dualshock4.USBMicrophoneMaxPacketSize, 0)
		legacyZero, legacyZeroCompleted := legacyISOIn(t, legacy,
			dualshock4.EndpointMicrophoneIn, zeroPackets)
		nativeZeroCompletion := harness.nativeISO(t, native, dualshock4.EndpointMicrophoneIn,
			zeroLength, nil, zeroPackets)
		nativeZero, nativeZeroLengths := compactNativeISO(t, nativeZeroCompletion)
		if !bytes.Equal(legacyZero, nativeZero) ||
			!bytes.Equal(nativeZero, make([]byte, dualshock4.USBMicrophonePacketSize)) ||
			legacyZeroCompleted[0].ActualLength != uint32(dualshock4.USBMicrophonePacketSize) ||
			nativeZeroLengths[0] != uint32(dualshock4.USBMicrophonePacketSize) {
			t.Fatalf("DualShock 4 reset silence differs: legacy=% x native=% x", legacyZero, nativeZero)
		}

		freshQueued := make([]byte, 0, 6*dualshock4.USBMicrophoneClientFrameSize)
		for frame := 0; frame < 6; frame++ {
			pcm := patternedPCM(dualshock4.USBMicrophoneClientFrameSize, byte(0xa0+frame*7))
			legacy.QueueMicrophonePCMFrame(pcm)
			native.QueueMicrophonePCMFrame(pcm)
			freshQueued = append(freshQueued, pcm...)
		}
		legacyFresh, _ := legacyISOIn(t, legacy,
			dualshock4.EndpointMicrophoneIn, zeroPackets)
		nativeFreshCompletion := harness.nativeISO(t, native, dualshock4.EndpointMicrophoneIn,
			zeroLength, nil, zeroPackets)
		nativeFresh, _ := compactNativeISO(t, nativeFreshCompletion)
		if !bytes.Equal(legacyFresh, nativeFresh) ||
			!bytes.Equal(nativeFresh, freshQueued[:len(nativeFresh)]) {
			t.Fatal("DualShock 4 microphone reset replayed stale capture bytes")
		}

		harness.legacySetInterface(legacy, dualshock4.InterfaceMicrophone, 0)
		harness.nativeLifecycle(t, native, udecx.OperationEndpointPurge,
			dualshock4.EndpointMicrophoneIn)
		requireDeviceBool(t, legacy, "microphoneInterfaceActive", false)
		requireDeviceBool(t, native, "microphoneInterfaceActive", false)
	})
}

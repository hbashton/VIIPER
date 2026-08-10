package usb_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	"github.com/Alia5/VIIPER/device/ns2pro"
	"github.com/Alia5/VIIPER/device/xbox360"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
	usbdevice "github.com/Alia5/VIIPER/usb"
)

func TestNativeProcessorPreservesProductionControllerOutputReports(t *testing.T) {
	t.Run("DualSense", func(t *testing.T) {
		dev, err := dualsense.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		var got dualsense.OutputState
		dev.SetOutputCallback(func(state dualsense.OutputState) { got = state })
		report := make([]byte, dualsense.OutputReportSize)
		report[0], report[1], report[3], report[4] = dualsense.ReportIDOutput, 0x03, 0x31, 0x92
		processNativeOutput(t, dev, dualsense.EndpointOut, report)
		if got.RumbleSmall != 0x31 || got.RumbleLarge != 0x92 ||
			!bytes.Equal(got.RawOutputReport[:], report) {
			t.Fatalf("DualSense output changed across native transport: %+v", got)
		}
	})

	t.Run("DualShock4", func(t *testing.T) {
		dev, err := dualshock4.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		var got dualshock4.OutputState
		dev.SetOutputCallback(func(state dualshock4.OutputState) { got = state })
		report := []byte{dualshock4.ReportIDOutput, 0, 0, 0, 0x22, 0xe1, 1, 2, 3, 4, 5}
		processNativeOutput(t, dev, dualshock4.EndpointOut, report)
		if got != (dualshock4.OutputState{
			RumbleSmall: 0x22, RumbleLarge: 0xe1,
			LedRed: 1, LedGreen: 2, LedBlue: 3, FlashOn: 4, FlashOff: 5,
		}) {
			t.Fatalf("DualShock 4 output changed across native transport: %+v", got)
		}
	})

	t.Run("Xbox360", func(t *testing.T) {
		dev, err := xbox360.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		var got xbox360.XRumbleState
		dev.SetRumbleCallback(func(state xbox360.XRumbleState) { got = state })
		processNativeOutput(t, dev, 0x01, []byte{0x00, 0x08, 0x00, 0x74, 0x29, 0, 0, 0})
		if got != (xbox360.XRumbleState{LeftMotor: 0x74, RightMotor: 0x29}) {
			t.Fatalf("Xbox 360 rumble changed across native transport: %+v", got)
		}
	})

	t.Run("Switch2Pro", func(t *testing.T) {
		dev, err := ns2pro.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		var got ns2pro.OutputState
		clear := dev.SetOutputCallback(func(state ns2pro.OutputState) { got = state })
		defer clear()
		report := make([]byte, ns2pro.OutputReportSize)
		report[0] = ns2pro.ReportIDOutput
		for i := range 16 {
			report[1+i] = byte(i + 1)
			report[17+i] = byte(0x80 + i)
		}
		processNativeOutput(t, dev, ns2pro.EndpointHIDOut, report)
		if got.Flags != ns2pro.OutputFlagRumble ||
			!bytes.Equal(got.LeftRumble[:], report[1:17]) ||
			!bytes.Equal(got.RightRumble[:], report[17:33]) {
			t.Fatalf("Switch 2 Pro rumble changed across native transport: %+v", got)
		}
	})
}

func processNativeOutput(t *testing.T, dev usbdevice.Device, endpoint uint8, payload []byte) {
	t.Helper()
	server := serverusb.New(serverusb.ServerConfig{}, slog.Default(), nil)
	processor, err := serverusb.NewNativeProcessor(server)
	if err != nil {
		t.Fatal(err)
	}
	op := udecx.Operation{
		Token: 99, DeviceID: 1, Generation: 1, Kind: udecx.OperationTransfer,
		EndpointAddress: endpoint, Direction: 0,
		TransferLength: uint32(len(payload)), Payload: payload,
	}
	completion, err := processor.Process(context.Background(), dev, op)
	if err != nil {
		t.Fatal(err)
	}
	if completion.TransferLength != uint32(len(payload)) || len(completion.Payload) != 0 {
		t.Fatalf("native OUT completion=%+v", completion)
	}
}

func TestNativeProcessorPreservesPlayStationIsochronousMedia(t *testing.T) {
	t.Run("DualSense", func(t *testing.T) {
		dev, err := dualsense.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		processor := newProductionProcessor(t)
		setNativeInterface(t, processor, dev, dualsense.InterfaceHapticsAudio, 1)
		setNativeInterface(t, processor, dev, dualsense.InterfaceMicrophone, 1)

		var speaker []byte
		dev.SetAtomicAudioHapticsCallback(func(_ dualsense.OutputState, pcm []byte) {
			speaker = append([]byte(nil), pcm...)
		})
		usbPCM := make([]byte, 480*dualsense.USBHapticsAudioFrameSize)
		for i := range usbPCM {
			usbPCM[i] = byte(i*37 + 11)
		}
		completion := processNativeIso(t, processor, dev, dualsense.EndpointHapticsAudioOut,
			false, usbPCM, 10, dualsense.USBHapticsAudioPacketSize)
		if completion.TransferLength != uint32(len(usbPCM)) || len(speaker) != 480*4 {
			t.Fatalf("DualSense speaker completion=%d callback=%d", completion.TransferLength, len(speaker))
		}

		microphoneFrame := make([]byte, dualsense.USBMicrophoneClientFrameSize)
		for i := range microphoneFrame {
			microphoneFrame[i] = byte(i*13 + 7)
		}
		for range 6 {
			dev.QueueMicrophonePCMFrame(microphoneFrame)
		}
		completion = processNativeIso(t, processor, dev, dualsense.EndpointMicrophoneIn,
			true, nil, 10, dualsense.USBMicrophonePacketSize)
		if !bytes.Equal(completion.Payload, microphoneFrame) {
			t.Fatal("DualSense microphone PCM changed across native transport")
		}
	})

	t.Run("DualShock4", func(t *testing.T) {
		dev, err := dualshock4.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		processor := newProductionProcessor(t)
		setNativeInterface(t, processor, dev, dualshock4.InterfaceSpeaker, 1)
		setNativeInterface(t, processor, dev, dualshock4.InterfaceMicrophone, 1)

		speakerPCM := make([]byte, 128)
		for i := range speakerPCM {
			speakerPCM[i] = byte(i*19 + 3)
		}
		var speaker []byte
		dev.SetSpeakerCallback(func(pcm []byte) { speaker = append([]byte(nil), pcm...) })
		completion := processNativeIso(t, processor, dev, dualshock4.EndpointAudioOut,
			false, speakerPCM, 1, uint32(len(speakerPCM)))
		if completion.TransferLength != uint32(len(speakerPCM)) || !bytes.Equal(speaker, speakerPCM) {
			t.Fatal("DualShock 4 speaker PCM changed across native transport")
		}

		microphoneFrame := make([]byte, dualshock4.USBMicrophoneClientFrameSize)
		for i := range microphoneFrame {
			microphoneFrame[i] = byte(i*23 + 5)
		}
		for range 6 {
			dev.QueueMicrophonePCMFrame(microphoneFrame)
		}
		completion = processNativeIso(t, processor, dev, dualshock4.EndpointMicrophoneIn,
			true, nil, 10, dualshock4.USBMicrophonePacketSize)
		if !bytes.Equal(completion.Payload, microphoneFrame) {
			t.Fatal("DualShock 4 microphone PCM changed across native transport")
		}
	})
}

func newProductionProcessor(t *testing.T) *serverusb.NativeProcessor {
	t.Helper()
	processor, err := serverusb.NewNativeProcessor(
		serverusb.New(serverusb.ServerConfig{}, slog.Default(), nil))
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func setNativeInterface(t *testing.T, processor *serverusb.NativeProcessor,
	dev usbdevice.Device, iface, alt uint8) {
	t.Helper()
	err := processor.Lifecycle(context.Background(), dev, udecx.Operation{
		DeviceID: 1, Generation: 1, Kind: udecx.OperationSetInterface,
		InterfaceNumber: iface, InterfaceSetting: alt,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func processNativeIso(t *testing.T, processor *serverusb.NativeProcessor,
	dev usbdevice.Device, endpoint uint8, input bool, payload []byte,
	packetCount int, packetLength uint32) udecx.Completion {
	t.Helper()
	packets := make([]udecx.IsoPacket, packetCount)
	for i := range packets {
		packets[i] = udecx.IsoPacket{Offset: uint32(i) * packetLength, Length: packetLength}
	}
	transferLength := uint32(packetCount) * packetLength
	op := udecx.Operation{
		Token: 100, DeviceID: 1, Generation: 1, Kind: udecx.OperationTransfer,
		EndpointAddress: endpoint, TransferLength: transferLength,
		IsoPackets: packets, Payload: payload,
	}
	if input {
		op.Direction = 1
	}
	completion, err := processor.Process(context.Background(), dev, op)
	if err != nil {
		t.Fatal(err)
	}
	if len(completion.IsoPackets) != packetCount {
		t.Fatalf("native ISO completion has %d packets, want %d", len(completion.IsoPackets), packetCount)
	}
	return completion
}

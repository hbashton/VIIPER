package usb_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
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
		startNativeEndpoint(t, processor, dev, dualsense.EndpointHapticsAudioOut)
		startNativeEndpoint(t, processor, dev, dualsense.EndpointMicrophoneIn)

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
		startNativeEndpoint(t, processor, dev, dualshock4.EndpointAudioOut)
		startNativeEndpoint(t, processor, dev, dualshock4.EndpointMicrophoneIn)

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

func TestNativeProcessorRunsDualSenseHIDSpeakerAndMicrophoneConcurrently(t *testing.T) {
	const iterations = 12
	dev, err := dualsense.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	processor := newProductionProcessor(t)
	startNativeEndpoint(t, processor, dev, dualsense.EndpointHapticsAudioOut)
	startNativeEndpoint(t, processor, dev, dualsense.EndpointMicrophoneIn)

	var outputReports atomic.Uint64
	var speakerFrames atomic.Uint64
	var callbackFailure atomic.Value
	dev.SetOutputCallback(func(dualsense.OutputState) {
		outputReports.Add(1)
	})
	dev.SetAtomicAudioHapticsCallback(func(_ dualsense.OutputState, pcm []byte) {
		if len(pcm) != 480*4 {
			callbackFailure.CompareAndSwap(nil, fmt.Errorf(
				"atomic speaker callback length=%d want=%d", len(pcm), 480*4))
			return
		}
		speakerFrames.Add(1)
	})

	microphoneFrame := make([]byte, dualsense.USBMicrophoneClientFrameSize)
	for index := range microphoneFrame {
		microphoneFrame[index] = byte(index*17 + 3)
	}
	// The production microphone contract deliberately primes six 10 ms source
	// frames before serving its first 1 ms USB packet.
	for range 6 {
		dev.QueueMicrophonePCMFrame(microphoneFrame)
	}

	errors := make(chan error, 3)
	var workers sync.WaitGroup
	workers.Add(3)

	go func() {
		defer workers.Done()
		for iteration := range iterations {
			report := make([]byte, dualsense.OutputReportSize)
			report[0], report[1] = dualsense.ReportIDOutput, 0x03
			report[3], report[4] = byte(iteration+1), byte(0x80+iteration)
			_, processErr := processor.Process(context.Background(), dev, udecx.Operation{
				Token: uint64(1000 + iteration), DeviceID: 1, Generation: 1,
				Kind: udecx.OperationTransfer, EndpointAddress: dualsense.EndpointOut,
				TransferLength: uint32(len(report)), Payload: report,
			})
			if processErr != nil {
				errors <- fmt.Errorf("HID output iteration %d: %w", iteration, processErr)
				return
			}
		}
	}()

	go func() {
		defer workers.Done()
		packetCount := 10
		packetLength := uint32(dualsense.USBHapticsAudioPacketSize)
		payload := make([]byte, packetCount*int(packetLength))
		for index := range payload {
			payload[index] = byte(index*29 + 5)
		}
		for iteration := range iterations {
			op := productionIsoOperation(
				uint64(2000+iteration), dualsense.EndpointHapticsAudioOut,
				false, payload, packetCount, packetLength)
			populateProductionEndpointMetadata(dev, &op)
			completion, processErr := processor.Process(context.Background(), dev, op)
			if processErr != nil {
				errors <- fmt.Errorf("speaker iteration %d: %w", iteration, processErr)
				return
			}
			if completion.TransferLength != uint32(len(payload)) ||
				len(completion.IsoPackets) != packetCount {
				errors <- fmt.Errorf("speaker iteration %d malformed completion: %+v",
					iteration, completion)
				return
			}
		}
	}()

	go func() {
		defer workers.Done()
		packetCount := 10
		packetLength := uint32(dualsense.USBMicrophonePacketSize)
		for iteration := range iterations {
			dev.QueueMicrophonePCMFrame(microphoneFrame)
			op := productionIsoOperation(
				uint64(3000+iteration), dualsense.EndpointMicrophoneIn,
				true, nil, packetCount, packetLength)
			populateProductionEndpointMetadata(dev, &op)
			completion, processErr := processor.Process(context.Background(), dev, op)
			if processErr != nil {
				errors <- fmt.Errorf("microphone iteration %d: %w", iteration, processErr)
				return
			}
			if completion.TransferLength == 0 || len(completion.Payload) != packetCount*int(packetLength) ||
				len(completion.IsoPackets) != packetCount {
				errors <- fmt.Errorf("microphone iteration %d malformed completion: %+v",
					iteration, completion)
				return
			}
		}
	}()

	workers.Wait()
	close(errors)
	for workerErr := range errors {
		t.Error(workerErr)
	}
	if failure := callbackFailure.Load(); failure != nil {
		t.Error(failure)
	}
	if got := outputReports.Load(); got != iterations {
		t.Errorf("DualSense output callbacks=%d want=%d", got, iterations)
	}
	if got := speakerFrames.Load(); got != iterations {
		t.Errorf("DualSense atomic speaker callbacks=%d want=%d", got, iterations)
	}
}

func TestNativeProcessorRunsDualShock4HIDSpeakerAndMicrophoneConcurrently(t *testing.T) {
	const iterations = 12
	dev, err := dualshock4.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	processor := newProductionProcessor(t)
	startNativeEndpoint(t, processor, dev, dualshock4.EndpointAudioOut)
	startNativeEndpoint(t, processor, dev, dualshock4.EndpointMicrophoneIn)

	var outputReports atomic.Uint64
	var speakerTransfers atomic.Uint64
	var callbackFailure atomic.Value
	dev.SetOutputCallback(func(dualshock4.OutputState) {
		outputReports.Add(1)
	})
	const speakerPackets = 10
	const speakerPacketLength = 128
	dev.SetSpeakerCallback(func(pcm []byte) {
		if len(pcm) != speakerPackets*speakerPacketLength {
			callbackFailure.CompareAndSwap(nil, fmt.Errorf(
				"speaker callback length=%d want=%d",
				len(pcm), speakerPackets*speakerPacketLength))
			return
		}
		speakerTransfers.Add(1)
	})

	microphoneFrame := make([]byte, dualshock4.USBMicrophoneClientFrameSize)
	for index := range microphoneFrame {
		microphoneFrame[index] = byte(index*11 + 7)
	}
	// Match the production capture contract's startup reserve before the first
	// 1 ms USB microphone packet is requested.
	for range 6 {
		dev.QueueMicrophonePCMFrame(microphoneFrame)
	}

	errors := make(chan error, 3)
	var workers sync.WaitGroup
	workers.Add(3)

	go func() {
		defer workers.Done()
		for iteration := range iterations {
			report := []byte{
				dualshock4.ReportIDOutput, 0, 0, 0,
				byte(iteration + 1), byte(0x80 + iteration),
				1, 2, 3, 0, 0,
			}
			_, processErr := processor.Process(context.Background(), dev, udecx.Operation{
				Token: uint64(4000 + iteration), DeviceID: 2, Generation: 1,
				Kind: udecx.OperationTransfer, EndpointAddress: dualshock4.EndpointOut,
				TransferLength: uint32(len(report)), Payload: report,
			})
			if processErr != nil {
				errors <- fmt.Errorf("HID output iteration %d: %w", iteration, processErr)
				return
			}
		}
	}()

	go func() {
		defer workers.Done()
		payload := make([]byte, speakerPackets*speakerPacketLength)
		for index := range payload {
			payload[index] = byte(index*31 + 9)
		}
		for iteration := range iterations {
			op := productionIsoOperation(
				uint64(5000+iteration), dualshock4.EndpointAudioOut,
				false, payload, speakerPackets, speakerPacketLength)
			op.DeviceID = 2
			populateProductionEndpointMetadata(dev, &op)
			completion, processErr := processor.Process(context.Background(), dev, op)
			if processErr != nil {
				errors <- fmt.Errorf("speaker iteration %d: %w", iteration, processErr)
				return
			}
			if completion.TransferLength != uint32(len(payload)) ||
				len(completion.IsoPackets) != speakerPackets {
				errors <- fmt.Errorf("speaker iteration %d malformed completion: %+v",
					iteration, completion)
				return
			}
		}
	}()

	go func() {
		defer workers.Done()
		const microphonePackets = 10
		for iteration := range iterations {
			dev.QueueMicrophonePCMFrame(microphoneFrame)
			op := productionIsoOperation(
				uint64(6000+iteration), dualshock4.EndpointMicrophoneIn,
				true, nil, microphonePackets, dualshock4.USBMicrophonePacketSize)
			op.DeviceID = 2
			populateProductionEndpointMetadata(dev, &op)
			completion, processErr := processor.Process(context.Background(), dev, op)
			if processErr != nil {
				errors <- fmt.Errorf("microphone iteration %d: %w", iteration, processErr)
				return
			}
			if completion.TransferLength == 0 ||
				len(completion.Payload) != microphonePackets*dualshock4.USBMicrophonePacketSize ||
				len(completion.IsoPackets) != microphonePackets {
				errors <- fmt.Errorf("microphone iteration %d malformed completion: %+v",
					iteration, completion)
				return
			}
		}
	}()

	workers.Wait()
	close(errors)
	for workerErr := range errors {
		t.Error(workerErr)
	}
	if failure := callbackFailure.Load(); failure != nil {
		t.Error(failure)
	}
	if got := outputReports.Load(); got != iterations {
		t.Errorf("DualShock 4 output callbacks=%d want=%d", got, iterations)
	}
	if got := speakerTransfers.Load(); got != iterations {
		t.Errorf("DualShock 4 speaker callbacks=%d want=%d", got, iterations)
	}
}

func productionIsoOperation(token uint64, endpoint uint8, input bool, payload []byte,
	packetCount int, packetLength uint32) udecx.Operation {
	packets := make([]udecx.IsoPacket, packetCount)
	for index := range packets {
		packets[index] = udecx.IsoPacket{
			Offset: uint32(index) * packetLength, Length: packetLength,
		}
	}
	op := udecx.Operation{
		Token: token, DeviceID: 1, Generation: 1, Kind: udecx.OperationTransfer,
		EndpointAddress: endpoint, TransferLength: uint32(packetCount) * packetLength,
		TransferFlags: udecx.TransferFlagStartIsoASAP, IsoPackets: packets, Payload: payload,
	}
	if input {
		op.Direction = 1
		op.TransferFlags |= udecx.TransferFlagDirectionIn
	}
	return op
}

func populateProductionEndpointMetadata(dev usbdevice.Device, op *udecx.Operation) {
	for _, iface := range dev.GetDescriptor().Interfaces {
		for _, endpoint := range iface.Endpoints {
			if endpoint.BEndpointAddress != op.EndpointAddress {
				continue
			}
			endpoint, err := udecx.EndpointDescriptorForNativeUdeCx(
				udecx.DeviceSpeed(dev.GetDescriptor().Device.Speed), endpoint)
			if err != nil {
				panic(err)
			}
			op.EndpointAttributes = endpoint.BMAttributes
			op.EndpointInterval = endpoint.BInterval
			op.EndpointMaxPacketSize = endpoint.WMaxPacketSize
			return
		}
	}
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

func startNativeEndpoint(t *testing.T, processor *serverusb.NativeProcessor,
	dev usbdevice.Device, endpointAddress uint8) {
	t.Helper()
	for _, iface := range dev.GetDescriptor().Interfaces {
		if iface.Descriptor.BAlternateSetting == 0 {
			continue
		}
		for _, endpoint := range iface.Endpoints {
			if endpoint.BEndpointAddress != endpointAddress {
				continue
			}
			endpoint, err := udecx.EndpointDescriptorForNativeUdeCx(
				udecx.DeviceSpeed(dev.GetDescriptor().Device.Speed), endpoint)
			if err != nil {
				t.Fatal(err)
			}
			err = processor.Lifecycle(context.Background(), dev, udecx.Operation{
				DeviceID: 1, Generation: 1, Kind: udecx.OperationEndpointStart,
				EndpointAddress:       endpoint.BEndpointAddress,
				EndpointAttributes:    endpoint.BMAttributes,
				EndpointInterval:      endpoint.BInterval,
				EndpointMaxPacketSize: endpoint.WMaxPacketSize,
			})
			if err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("endpoint %#x has no nonzero alternate setting", endpointAddress)
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
		TransferFlags: udecx.TransferFlagStartIsoASAP, IsoPackets: packets, Payload: payload,
	}
	for _, iface := range dev.GetDescriptor().Interfaces {
		for _, descEndpoint := range iface.Endpoints {
			if descEndpoint.BEndpointAddress == endpoint {
				var err error
				descEndpoint, err = udecx.EndpointDescriptorForNativeUdeCx(
					udecx.DeviceSpeed(dev.GetDescriptor().Device.Speed), descEndpoint)
				if err != nil {
					t.Fatal(err)
				}
				op.EndpointAttributes = descEndpoint.BMAttributes
				op.EndpointInterval = descEndpoint.BInterval
				op.EndpointMaxPacketSize = descEndpoint.WMaxPacketSize
			}
		}
	}
	if input {
		op.Direction = 1
		op.TransferFlags |= udecx.TransferFlagDirectionIn
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

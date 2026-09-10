package dualsense

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

func testV5Media(marker byte) ([]byte, []byte) {
	feedback := make([]byte, OutputStateV5Size)
	feedback[OutputStateCombinedBluetoothOffset] = BluetoothCombinedHapticsReportID
	feedback[0] = marker
	speaker := make([]byte, dualSenseV5SpeakerPayloadSize)
	for index := range speaker {
		speaker[index] = marker
	}
	return feedback, speaker
}

func TestDualSenseV5WriterPublishesOnlyV5AtomicFrames(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, nil, nil)
	go writer.Run()

	feedback, speaker := testV5Media(0x41)
	writer.EnqueueAtomicAudioHaptics(feedback, speaker)
	header, payload := readDualSenseOutputFrame(t, client)
	if header[4] != StreamFrameVersionV5 ||
		header[5] != StreamFrameAtomicAudioHaptics {
		t.Fatalf("unexpected V5 frame header: % x", header)
	}
	feedbackLength := int(binary.LittleEndian.Uint16(payload[:2]))
	if feedbackLength != len(feedback) {
		t.Fatalf("feedback length=%d want=%d", feedbackLength, len(feedback))
	}
	if string(payload[2:2+feedbackLength]) != string(feedback) ||
		string(payload[2+feedbackLength:]) != string(speaker) {
		t.Fatal("V5 writer changed an atomic generation")
	}

	control := make([]byte, OutputStateV5Size)
	control[0] = 0x52
	writer.EnqueueControl(StreamFrameOutputState, control)
	header, payload = readDualSenseOutputFrame(t, client)
	if header[4] != StreamFrameVersionV5 || header[5] != StreamFrameOutputState ||
		binary.LittleEndian.Uint32(header[8:12]) != 1 ||
		string(payload) != string(control) {
		t.Fatalf("unexpected V5 control frame: header=% x payload0=%02x", header, payload[0])
	}

	_ = client.Close()
	writer.Stop()
	state := writer.telemetry.snapshot()
	if state.ReceivedPayloads != 1 || state.WrittenPayloads != 1 ||
		state.DroppedPayloads != 0 || state.WriteFailures != 0 || state.Active {
		t.Fatalf("unexpected V5 telemetry: %+v", state)
	}
}

func TestDualSenseV5WriterPublishesRealtimeHapticsFrame(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, nil, nil)
	go writer.Run()

	feedback := make([]byte, OutputStateV5Size)
	feedback[OutputStateCombinedBluetoothOffset] =
		BluetoothCombinedHapticsReportID
	feedback[OutputStateCombinedBluetoothOffset+
		BluetoothCombinedHapticsOffset] = 0x5A
	writer.EnqueueRealtimeHaptics(feedback)

	header, payload := readDualSenseOutputFrame(t, client)
	if header[4] != StreamFrameVersionV5 ||
		header[5] != StreamFrameRealtimeHaptics ||
		!bytes.Equal(payload, feedback) {
		t.Fatalf("unexpected realtime haptics frame: header=% x", header)
	}

	_ = client.Close()
	writer.Stop()
}

func TestDualSenseV5WriterKeepsOnlyLatestOutputState(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, nil, nil)
	first := OutputState{RumbleSmall: 1}
	second := OutputState{RumbleSmall: 2}
	writer.EnqueueOutputState(first)
	writer.EnqueueOutputState(second)

	frame, ok := writer.claimLatestOutput()
	if !ok || frame.frameType != StreamFrameOutputState ||
		len(frame.payload) != OutputStateV5Size || frame.payload[0] != 2 {
		t.Fatalf("latest output latch retained wrong state: ok=%t frame=%+v", ok, frame)
	}
	if _, duplicate := writer.claimLatestOutput(); duplicate {
		t.Fatal("output latch retained more than one replaceable state")
	}
	writer.release(frame)
	if len(writer.latestOutputFree) != cap(writer.latestOutputFree) {
		t.Fatalf("latest output replacement leaked a fixed slot: free=%d",
			len(writer.latestOutputFree))
	}
}

func TestDualSenseV5DeviceCallbackPreservesCloselySpacedNativeCommands(t *testing.T) {
	for _, scenario := range []string{"rumble-pulse-and-stop", "trigger-then-led-only"} {
		t.Run(scenario, func(t *testing.T) {
			dev, err := New(nil)
			if err != nil {
				t.Fatal(err)
			}
			server, client := net.Pipe()
			writer := newDualSenseOutputWriter(server, nil, nil)
			dev.setV5OutputCallbacks(1, writer.EnqueueOutputState, nil, nil, nil)
			t.Cleanup(func() {
				dev.setV5OutputCallbacks(1, nil, nil, nil, nil)
				_ = client.Close()
				writer.Stop()
			})

			var first, second [OutputReportSize]byte
			first[0], second[0] = ReportIDOutput, ReportIDOutput
			if scenario == "rumble-pulse-and-stop" {
				first[1], second[1] = 0x03, 0x03
				first[3], first[4] = 90, 120
			} else {
				first[1], first[11], first[12] = 0x04, 0x25, 0x17
				second[2], second[45], second[46], second[47] = 0x04, 31, 63, 95
			}

			// Exercise the actual USB OUT -> device merge -> registered V5
			// callback boundary. Hold writer presentation until both commands
			// arrive, as happens during socket backpressure or a scheduling gap.
			// These are exact validity-bearing commands, not cumulative state.
			dev.HandleTransfer(context.Background(), EndpointOut&0x0f, usbip.DirOut, first[:])
			dev.HandleTransfer(context.Background(), EndpointOut&0x0f, usbip.DirOut, second[:])
			go writer.Run()
			if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}

			for index, expected := range [][OutputReportSize]byte{first, second} {
				header, payload := readDualSenseOutputFrame(t, client)
				if header[5] != StreamFrameOutputState ||
					binary.LittleEndian.Uint32(header[8:12]) != uint32(index) {
					t.Fatalf("command %d has wrong output framing: % x", index, header)
				}
				var delivered OutputState
				if err := delivered.UnmarshalV5Binary(payload); err != nil {
					t.Fatal(err)
				}
				if delivered.RawOutputReport != expected {
					t.Fatalf("native command %d was lost or replaced before V5 presentation:\n got % x\nwant % x",
						index, delivered.RawOutputReport, expected)
				}
			}
		})
	}
}

func TestDualSenseV5LatestOutputStorageIsIndependentOfOrderedControl(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, nil, nil)
	for index := 0; index < dualSenseOutputControlQueueCapacity; index++ {
		writer.EnqueueControl(StreamFrameMicrophoneInterfaceState, []byte{byte(index)})
	}
	if len(writer.control) != dualSenseOutputControlQueueCapacity {
		t.Fatalf("ordered control queue depth=%d", len(writer.control))
	}

	writer.EnqueueOutputState(OutputState{RumbleSmall: 99})
	frame, ok := writer.claimLatestOutput()
	if !ok || frame.payload[0] != 99 {
		t.Fatalf("ordered control traffic starved latest output: ok=%t frame=%+v",
			ok, frame)
	}
	writer.release(frame)
	for len(writer.control) != 0 {
		writer.release(<-writer.control)
	}
}

func TestDualSenseV5NativeCommandAdmissionRejectsFullWithoutCommittingState(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	admission, ok := any(dev).(interface {
		TryHandleOutputCommand(uint8, [8]byte, []byte) (bool, bool)
	})
	if !ok {
		t.Fatal("DualSense native output has no bounded USB admission result")
	}
	writer := newDualSenseOutputWriter(nil, nil, nil)
	dev.setV5OutputCallbacks(1, writer.EnqueueOutputState, nil, nil, nil)
	defer writer.drainOrderedControl()
	var report [OutputReportSize]byte
	report[0], report[1] = ReportIDOutput, 0x03
	for index := 1; index <= dualSenseOutputControlQueueCapacity; index++ {
		report[3] = byte(index)
		handled, accepted := admission.TryHandleOutputCommand(EndpointOut, [8]byte{}, report[:])
		if !handled || !accepted {
			t.Fatalf("normal burst command %d was not admitted", index)
		}
	}
	beforeOutput, beforeMedia := dev.outputState, dev.mediaOutputState
	report[3] = 0xee
	if handled, accepted := admission.TryHandleOutputCommand(EndpointOut, [8]byte{}, report[:]); !handled || accepted {
		t.Fatal("full native queue did not explicitly reject its next command")
	}
	if dev.outputState != beforeOutput || dev.mediaOutputState != beforeMedia {
		t.Fatal("rejected native command committed persistent or media state")
	}
	for index := 1; index <= dualSenseOutputControlQueueCapacity; index++ {
		frame, exists := writer.claimOrderedControl()
		if !exists {
			t.Fatalf("admitted command %d was lost", index)
		}
		var feedback OutputState
		if err := feedback.UnmarshalV5Binary(frame.payload); err != nil {
			t.Fatal(err)
		}
		writer.release(frame)
		if feedback.RawOutputReport[3] != byte(index) {
			t.Fatalf("admitted command %d was replaced with %d", index, feedback.RawOutputReport[3])
		}
	}
	if handled, accepted := admission.TryHandleOutputCommand(EndpointOut, [8]byte{}, report[:]); !handled || !accepted {
		t.Fatal("released capacity did not admit a host retry")
	}
	writer.requestStop()
	beforeOutput, beforeMedia = dev.outputState, dev.mediaOutputState
	report[3] = 0xff
	if handled, accepted := admission.TryHandleOutputCommand(EndpointOut, [8]byte{}, report[:]); !handled || accepted {
		t.Fatal("retired stream accepted a late native command")
	}
	if dev.outputState != beforeOutput || dev.mediaOutputState != beforeMedia {
		t.Fatal("retired-stream command changed persistent state")
	}
}

func TestDualSenseV5CombinedMediaSnapshotRemainsReplaceable(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, nil, nil)
	feedback := OutputState{}
	feedback.RawOutputReport[0] = ReportIDOutput
	feedback.BluetoothCombinedOutputReport[0] = BluetoothCombinedHapticsReportID
	feedback.RumbleSmall = 1
	writer.EnqueueOutputState(feedback)
	feedback.RumbleSmall = 2
	writer.EnqueueOutputState(feedback)
	frame, ok := writer.claimLatestOutput()
	if !ok || frame.payload[0] != 2 || len(writer.control) != 0 {
		t.Fatal("cumulative combined media snapshot entered the native command FIFO")
	}
	writer.release(frame)
}

func TestDualSenseV5ShortRumbleStopCannotRestartThroughCombinedMedia(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	writer := newDualSenseOutputWriter(nil, nil, nil)
	dev.setV5OutputCallbacks(1, writer.EnqueueOutputState, nil, nil, nil)
	defer writer.drainOrderedControl()
	var on [OutputReportSize]byte
	on[0], on[1], on[3], on[4] = ReportIDOutput, 0x03, 90, 120
	dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, on[:])
	dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, []byte{ReportIDOutput, 0x03, 0, 0, 0})
	dev.mediaMu.Lock()
	media, _, built := dev.buildDualSenseV5FeedbackLocked()
	dev.mediaMu.Unlock()
	if !built || media.BluetoothCombinedOutputReport[0] != BluetoothCombinedHapticsReportID {
		t.Fatal("next media callback did not produce a combined carrier")
	}
	if media.RawOutputReport[3] != 0 || media.RawOutputReport[4] != 0 ||
		media.BluetoothCombinedOutputReport[15] != 0 || media.BluetoothCombinedOutputReport[16] != 0 {
		t.Fatal("accepted short rumble stop was replayed as active rumble by cumulative media")
	}
}

func TestDualSenseV5ShortCommandsDoNotSynthesizeAbsentValidityGroups(t *testing.T) {
	for _, test := range []struct {
		name                     string
		length, flagOffset       int
		mask                     byte
		fieldOffset, fieldLength int
	}{
		{"right-trigger", 12, 1, outputFlag0RightTrigger, outputRightTriggerOffset, outputTriggerLength},
		{"left-trigger", 32, 1, outputFlag0LeftTrigger, outputLeftTriggerOffset, outputTriggerLength},
		{"lightbar", 45, 2, outputFlag1Lightbar, outputLightbarOffset, 3},
		{"brightness", 40, 39, outputFlag2LightbarBrightness, 43, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			dev, err := New(nil)
			if err != nil {
				t.Fatal(err)
			}
			var delivered OutputState
			dev.SetOutputCallback(func(state OutputState) { delivered = state })
			var initial [OutputReportSize]byte
			initial[0], initial[test.flagOffset] = ReportIDOutput, test.mask
			for index := 0; index < test.fieldLength; index++ {
				initial[test.fieldOffset+index] = byte(17 + index)
			}
			dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, initial[:])
			partial := make([]byte, test.length)
			partial[0], partial[test.flagOffset] = ReportIDOutput, test.mask
			dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, partial)
			if delivered.RawOutputReport[test.flagOffset]&test.mask != 0 {
				t.Fatal("short exact command synthesized a zero-filled incomplete validity group")
			}
			if !bytes.Equal(dev.mediaOutputState.RawOutputReport[test.fieldOffset:test.fieldOffset+test.fieldLength],
				initial[test.fieldOffset:test.fieldOffset+test.fieldLength]) {
				t.Fatal("short command cleared a field whose complete payload was absent")
			}
			complete := make([]byte, test.fieldOffset+test.fieldLength)
			complete[0], complete[test.flagOffset] = ReportIDOutput, test.mask
			for index := 0; index < test.fieldLength; index++ {
				complete[test.fieldOffset+index] = byte(100 + index)
			}
			dev.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, complete)
			if delivered.RawOutputReport[test.flagOffset]&test.mask == 0 ||
				!bytes.Equal(dev.mediaOutputState.RawOutputReport[test.fieldOffset:test.fieldOffset+test.fieldLength],
					complete[test.fieldOffset:]) {
				t.Fatal("short command lost a complete, present validity group")
			}
			if test.mask == outputFlag0RightTrigger && test.flagOffset == 1 && delivered.TriggerR2Mode != 100 ||
				test.mask == outputFlag0LeftTrigger && test.flagOffset == 1 && delivered.TriggerL2Mode != 100 {
				t.Fatal("short complete trigger command left typed state inconsistent")
			}
		})
	}
}

func TestDualSenseV5NativeCommandAdmissionAllocatesZero(t *testing.T) {
	if raceEnabled {
		t.Skip("race instrumentation allocates; allocation contract is tested without -race")
	}
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	writer := newDualSenseOutputWriter(nil, nil, nil)
	dev.setV5OutputCallbacks(1, writer.EnqueueOutputState, nil, nil, nil)
	var report [OutputReportSize]byte
	report[0], report[1] = ReportIDOutput, 0x03
	for _, length := range []int{5, OutputReportSize} {
		allocations := testing.AllocsPerRun(1000, func() {
			if handled, accepted := dev.TryHandleOutputCommand(EndpointOut, [8]byte{}, report[:length]); !handled || !accepted {
				t.Fatal("native command not admitted")
			}
			writer.drainOrderedControl()
		})
		if allocations != 0 {
			t.Fatalf("native command length %d allocated %.2f objects", length, allocations)
		}
	}
}

func TestDualSenseV5OutputAdmissionOnlyClaimsNativeHidPaths(t *testing.T) {
	for _, gamepadOnly := range []bool{false, true} {
		dev, err := New(nil)
		if err != nil {
			t.Fatal(err)
		}
		if gamepadOnly {
			dev.descriptor = makeGamepadOnlyDescriptor(false)
		}
		var hidInterface byte
		for _, iface := range dev.descriptor.Interfaces {
			if iface.Descriptor.BInterfaceClass == 0x03 {
				hidInterface = iface.Descriptor.BInterfaceNumber
				break
			}
		}
		writer := newDualSenseOutputWriter(nil, nil, nil)
		dev.setV5OutputCallbacks(1, writer.EnqueueOutputState, nil, nil, nil)
		var report [OutputReportSize]byte
		report[0], report[1], report[3] = ReportIDOutput, 0x03, 17
		setup := [8]byte{hidClassOUT, hidSetReport, ReportIDOutput,
			reportTypeOutput, hidInterface, 0, OutputReportSize, 0}
		if handled, accepted := dev.TryHandleOutputCommand(0, setup, report[:]); !handled || !accepted {
			t.Fatalf("native EP0 SET_REPORT not admitted (gamepadOnly=%t)", gamepadOnly)
		}
		writer.requestStop()
		before := dev.outputState
		report[3] = 18
		if handled, accepted := dev.TryHandleOutputCommand(0, setup, report[:]); !handled || accepted {
			t.Fatal("EP0 accepted output into a stopped stream")
		}
		for _, endpoint := range []uint8{EndpointHapticsAudioOut, EndpointMicrophoneIn & 0x0f, EndpointIn & 0x0f} {
			if handled, _ := dev.TryHandleOutputCommand(endpoint, setup, report[:]); handled {
				t.Fatalf("native output admission claimed unrelated endpoint %d", endpoint)
			}
		}
		for _, offset := range []int{0, 1, 2, 3, 4, 5, 6} {
			unrelated := setup
			unrelated[offset] ^= 0x80
			if handled, _ := dev.TryHandleOutputCommand(0, unrelated, report[:]); handled {
				t.Fatalf("native output admission claimed unrelated control setup % x", unrelated)
			}
		}
		if dev.outputState != before {
			t.Fatal("unhandled or stopped command changed native state")
		}
		writer.drainOrderedControl()
	}
}

func TestDualSenseV5NativeCommandsSurviveMediaResetAndRetireWithWriter(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, nil, nil)
	feedback := OutputState{}
	feedback.RawOutputReport[0] = ReportIDOutput
	for index := 0; index < dualSenseOutputControlQueueCapacity; index++ {
		feedback.RawOutputReport[3] = byte(index)
		if !writer.EnqueueOutputState(feedback) {
			t.Fatalf("native command %d not admitted", index)
		}
	}
	media, speaker := testV5Media(1)
	writer.EnqueueAtomicAudioHaptics(media, speaker)
	writer.EnqueueRealtimeHaptics(media)
	if len(writer.audio) != 1 || len(writer.realtimeHaptics) != 1 {
		t.Fatal("full native command queue starved independent media admission")
	}
	writer.ResetSpeaker()
	if len(writer.control) != dualSenseOutputControlQueueCapacity {
		t.Fatal("audio reset discarded admitted native commands")
	}
	go writer.Run()
	writer.Stop()
	_ = client.Close()
	if writer.EnqueueOutputState(feedback) || len(writer.control) != 0 ||
		len(writer.controlFree) != dualSenseOutputControlQueueCapacity {
		t.Fatal("session stop retained native commands or admitted a stale callback")
	}
}

func TestDualSenseV5MicrophoneLifecycleOverflowRecoversFinalState(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, nil, nil)
	const generation = uint64(73)
	for index := 0; index < dualSenseOutputControlQueueCapacity; index++ {
		writer.EnqueueMicrophoneInterfaceState(index%2 != 0, generation)
	}
	// Both events arrive after the ordered ring is full. They must coalesce in
	// one recovery slot whose final value is authoritative, never disappear.
	writer.EnqueueMicrophoneInterfaceState(true, generation)
	writer.EnqueueMicrophoneInterfaceState(false, generation)

	if len(writer.control) != dualSenseOutputControlQueueCapacity {
		t.Fatalf("ordered microphone depth=%d", len(writer.control))
	}
	if got := writer.telemetry.microphoneInterfaceOverflows.Load(); got != 2 {
		t.Fatalf("microphone lifecycle overflows=%d, want 2", got)
	}

	for index := 0; index < dualSenseOutputControlQueueCapacity; index++ {
		frame, ok := writer.claimOrderedControl()
		if !ok || frame.frameType != StreamFrameMicrophoneInterfaceState ||
			len(frame.payload) != 9 ||
			(frame.payload[0] != 0) != (index%2 != 0) ||
			binary.LittleEndian.Uint64(frame.payload[1:]) != generation {
			t.Fatalf("ordered microphone event %d: ok=%t frame=%+v",
				index, ok, frame)
		}
		writer.release(frame)
	}
	recovery, ok := writer.claimOrderedControl()
	if !ok || recovery.frameType != StreamFrameMicrophoneInterfaceState ||
		len(recovery.payload) != 9 || recovery.payload[0] != 0 ||
		binary.LittleEndian.Uint64(recovery.payload[1:]) != generation {
		t.Fatalf("final microphone recovery event: ok=%t frame=%+v", ok, recovery)
	}
	writer.release(recovery)
	if _, duplicate := writer.claimOrderedControl(); duplicate {
		t.Fatal("microphone recovery retained a stale contradictory event")
	}
	if len(writer.controlFree) != cap(writer.controlFree) ||
		len(writer.microphoneRecoveryFree) != cap(writer.microphoneRecoveryFree) {
		t.Fatalf("microphone recovery leaked fixed buffers: control=%d recovery=%d",
			len(writer.controlFree), len(writer.microphoneRecoveryFree))
	}
}

func TestDualSenseMicrophoneAttachSnapshotOrdersBeforeConcurrentTransition(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const generation = uint64(91)
	events := make(chan bool, 2)
	locksFree := make(chan bool, 2)
	initialStarted := make(chan struct{})
	releaseInitial := make(chan struct{})
	registerDone := make(chan struct{})
	first := true
	callback := func(active bool, gotGeneration uint64) {
		if gotGeneration != generation {
			t.Errorf("microphone event generation=%d, want %d",
				gotGeneration, generation)
		}
		lockFree := dev.microphoneMu.TryLock()
		locksFree <- lockFree
		if lockFree {
			dev.microphoneMu.Unlock()
		}
		events <- active
		if first {
			first = false
			close(initialStarted)
			<-releaseInitial
		}
	}
	go func() {
		dev.setMicrophoneInterfaceStateCallback(generation, callback)
		close(registerDone)
	}()
	<-initialStarted

	transitionDone := make(chan struct{})
	go func() {
		dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)
		close(transitionDone)
	}()
	select {
	case <-transitionDone:
	case <-time.After(time.Second):
		close(releaseInitial)
		t.Fatal("microphone transition blocked behind callback execution")
	}
	close(releaseInitial)
	<-registerDone

	if initial, transition := <-events, <-events; initial || !transition {
		t.Fatalf("attach/transition order=(%t,%t), want (false,true)",
			initial, transition)
	}
	if firstLockFree, secondLockFree := <-locksFree, <-locksFree; !firstLockFree || !secondLockFree {
		t.Fatalf("microphone callback ran under subsystem lock: (%t,%t)",
			firstLockFree, secondLockFree)
	}
	select {
	case unexpected := <-events:
		t.Fatalf("duplicate microphone event %t", unexpected)
	default:
	}
}

func TestDualSenseV5RealtimeQueueDropsOldestGeneration(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, nil, nil)
	for index := 1; index <= dualSenseOutputControlQueueCapacity+1; index++ {
		writer.EnqueueRealtimeHapticsStateGeneration(
			OutputState{RumbleSmall: byte(index)}, 1)
	}
	if len(writer.realtimeHaptics) != dualSenseOutputControlQueueCapacity {
		t.Fatalf("realtime queue depth=%d", len(writer.realtimeHaptics))
	}
	first := <-writer.realtimeHaptics
	if first.payload[0] != 2 {
		t.Fatalf("realtime queue retained stale oldest generation: first=%d",
			first.payload[0])
	}
	writer.release(first)
	for len(writer.realtimeHaptics) != 0 {
		writer.release(<-writer.realtimeHaptics)
	}
}

func TestDualSenseV5WriterGenerationResetDrainsEveryMediaLane(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, nil, nil)
	writer.SetSpeakerGeneration(1)
	feedback := OutputState{}
	feedback.BluetoothCombinedOutputReport[0] = BluetoothCombinedHapticsReportID
	speaker := make([]byte, dualSenseV5SpeakerPayloadSize)
	writer.EnqueueRealtimeHapticsStateGeneration(feedback, 1)
	writer.EnqueueAtomicAudioHapticsState(feedback, speaker, 1)
	if len(writer.realtimeHaptics) != 1 || len(writer.audio) != 1 {
		t.Fatal("test did not populate both media lanes")
	}

	writer.ResetSpeakerGeneration(2)
	if len(writer.realtimeHaptics) != 0 || len(writer.audio) != 0 ||
		len(writer.realtimeFree) != dualSenseOutputControlQueueCapacity ||
		len(writer.audioFree) != dualSenseOutputAudioQueueCapacity {
		t.Fatalf("generation reset retained media: realtime=%d audio=%d realtimeFree=%d audioFree=%d",
			len(writer.realtimeHaptics), len(writer.audio), len(writer.realtimeFree),
			len(writer.audioFree))
	}
	writer.EnqueueRealtimeHapticsStateGeneration(feedback, 1)
	writer.EnqueueAtomicAudioHapticsState(feedback, speaker, 1)
	if len(writer.realtimeHaptics) != 0 || len(writer.audio) != 0 {
		t.Fatal("stale media generation was accepted after reset")
	}
}

func TestDualSenseV5WriterAlternatesControlAndMedia(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, nil, nil)
	for index := 0; index < 4; index++ {
		command := OutputState{RumbleSmall: byte(index)}
		command.RawOutputReport[0] = ReportIDOutput
		if !writer.EnqueueOutputState(command) {
			t.Fatal("native command admission unexpectedly failed")
		}
		feedback, speaker := testV5Media(byte(index))
		writer.EnqueueAtomicAudioHaptics(feedback, speaker)
	}
	go writer.Run()

	for index := 0; index < 8; index++ {
		header, _ := readDualSenseOutputFrame(t, client)
		want := byte(StreamFrameOutputState)
		if index%2 != 0 {
			want = StreamFrameAtomicAudioHaptics
		}
		if header[5] != want {
			t.Fatalf("frame %d type=0x%02X want=0x%02X", index, header[5], want)
		}
	}
	writer.Stop()
	_ = client.Close()
}

func TestDualSenseV5WriterShutdownReturnsEveryMediaBuffer(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, nil, nil)
	for index := 0; index < 10; index++ {
		feedback, speaker := testV5Media(byte(index))
		writer.EnqueueAtomicAudioHaptics(feedback, speaker)
	}
	go writer.Run()
	writer.Stop()
	_ = client.Close()

	state := writer.telemetry.snapshot()
	if state.Active || state.QueueDepth != 0 || len(writer.audio) != 0 ||
		len(writer.audioFree) != dualSenseOutputAudioQueueCapacity {
		t.Fatalf("shutdown retained buffers: state=%+v queued=%d free=%d",
			state, len(writer.audio), len(writer.audioFree))
	}
}

type writeStartedConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *writeStartedConn) Write(payload []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(payload)
}

type deadlineTrackingConn struct {
	net.Conn
	started      chan struct{}
	closed       chan struct{}
	startedOnce  sync.Once
	closedOnce   sync.Once
	deadlineLock sync.Mutex
	deadlines    []time.Time
}

func newDeadlineTrackingConn(conn net.Conn) *deadlineTrackingConn {
	return &deadlineTrackingConn{
		Conn: conn, started: make(chan struct{}), closed: make(chan struct{}),
	}
}

func (c *deadlineTrackingConn) Write(payload []byte) (int, error) {
	c.startedOnce.Do(func() { close(c.started) })
	return c.Conn.Write(payload)
}

func (c *deadlineTrackingConn) SetWriteDeadline(deadline time.Time) error {
	c.deadlineLock.Lock()
	c.deadlines = append(c.deadlines, deadline)
	c.deadlineLock.Unlock()
	return c.Conn.SetWriteDeadline(deadline)
}

func (c *deadlineTrackingConn) Close() error {
	err := c.Conn.Close()
	c.closedOnce.Do(func() { close(c.closed) })
	return err
}

func TestDualSenseV5WriterWriteFailureCannotRaceFinalDrain(t *testing.T) {
	server, client := net.Pipe()
	conn := &writeStartedConn{Conn: server, started: make(chan struct{})}
	writer := newDualSenseOutputWriter(conn, nil, nil)
	writer.EnqueueControl(StreamFrameOutputState, []byte{0x01})
	go writer.Run()
	<-conn.started

	writer.enqueueLock.RLock()
	buffer := <-writer.audioFree
	buffer[0] = 0x55
	writer.audio <- dualSenseOutputFrame{
		frameType: StreamFrameAtomicAudioHaptics,
		payload:   buffer[:4], audio: true,
	}
	_ = client.Close()
	writer.enqueueLock.RUnlock()

	select {
	case <-writer.done:
	case <-time.After(time.Second):
		t.Fatal("writer did not finish after socket failure")
	}
	if len(writer.audio) != 0 ||
		len(writer.audioFree) != dualSenseOutputAudioQueueCapacity {
		t.Fatalf("shutdown retained a pooled buffer: queued=%d free=%d",
			len(writer.audio), len(writer.audioFree))
	}
}

func TestDualSenseV5WriterResetIsHardGenerationBarrier(t *testing.T) {
	server, client := net.Pipe()
	conn := newDeadlineTrackingConn(server)
	writer := newDualSenseOutputWriter(conn, nil, nil)
	oldFeedback, oldSpeaker := testV5Media(0x11)
	writer.EnqueueAtomicAudioHaptics(oldFeedback, oldSpeaker)
	go writer.Run()
	<-conn.started
	queuedFeedback, queuedSpeaker := testV5Media(0x12)
	writer.EnqueueAtomicAudioHaptics(queuedFeedback, queuedSpeaker)

	resetDone := make(chan struct{})
	go func() { writer.ResetSpeaker(); close(resetDone) }()
	select {
	case <-resetDone:
		t.Fatal("reset crossed an in-flight old-generation write")
	case <-time.After(20 * time.Millisecond):
	}
	_, oldPayload := readDualSenseOutputFrame(t, client)
	oldLength := int(binary.LittleEndian.Uint16(oldPayload[:2]))
	if oldPayload[2] != 0x11 || oldLength != OutputStateV5Size {
		t.Fatalf("unexpected in-flight generation: % x", oldPayload[:4])
	}
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("reset did not finish")
	}
	if len(writer.audio) != 0 {
		t.Fatalf("reset retained %d old media frames", len(writer.audio))
	}

	newFeedback, newSpeaker := testV5Media(0x22)
	writer.EnqueueAtomicAudioHaptics(newFeedback, newSpeaker)
	_, newPayload := readDualSenseOutputFrame(t, client)
	if newPayload[2] != 0x22 {
		t.Fatalf("post-reset frame is stale: % x", newPayload[:4])
	}
	writer.Stop()
	_ = client.Close()
}

func TestDualSenseV5BlockedWriteHoldsNoGenerationOrProducerLock(t *testing.T) {
	server, client := net.Pipe()
	conn := &writeStartedConn{Conn: server, started: make(chan struct{})}
	writer := newDualSenseOutputWriter(conn, nil, nil)
	writer.SetSpeakerGeneration(1)
	oldFeedback, oldSpeaker := testV5Media(0x41)
	writer.EnqueueAtomicAudioHaptics(oldFeedback, oldSpeaker)
	go writer.Run()
	<-conn.started

	if sequence := writer.audioInFlightSequence.Load(); sequence&1 == 0 ||
		writer.audioInFlightGeneration.Load() != 1 {
		t.Fatalf("writer did not publish the in-flight generation: sequence=%d generation=%d",
			sequence, writer.audioInFlightGeneration.Load())
	}
	if !writer.enqueueLock.TryLock() {
		t.Fatal("socket write retained the generation/queue lock")
	}
	writer.enqueueLock.Unlock()
	if !writer.audioEnqueue.TryLock() {
		t.Fatal("socket write retained the media producer lock")
	}
	writer.audioEnqueue.Unlock()
	if !writer.outputStateMu.TryLock() {
		t.Fatal("socket write retained the latest-output state lock")
	}
	writer.outputStateMu.Unlock()

	resetDone := make(chan struct{})
	go func() {
		writer.ResetSpeaker()
		close(resetDone)
	}()
	deadline := time.Now().Add(time.Second)
	for writer.audioGeneration.Load() != 2 {
		if time.Now().After(deadline) {
			t.Fatal("reset did not publish its generation")
		}
		runtime.Gosched()
	}
	select {
	case <-resetDone:
		t.Fatal("reset crossed the blocked old-generation write")
	default:
	}
	for !writer.enqueueLock.TryLock() {
		if time.Now().After(deadline) {
			t.Fatal("reset did not release the generation/queue lock before waiting")
		}
		runtime.Gosched()
	}
	writer.enqueueLock.Unlock()
	select {
	case <-resetDone:
		t.Fatal("reset crossed the blocked write while its queue lock was tested")
	default:
	}

	producerDone := make(chan struct{})
	go func() {
		writer.EnqueueControl(StreamFrameOutputState, []byte{0x52})
		writer.EnqueueOutputState(OutputState{RumbleSmall: 0x53})
		newFeedback, newSpeaker := testV5Media(0x54)
		writer.EnqueueAtomicAudioHaptics(newFeedback, newSpeaker)
		close(producerDone)
	}()
	select {
	case <-producerDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("socket backpressure or reset blocked an output producer")
	}

	_, payload := readDualSenseOutputFrame(t, client)
	if payload[2] != 0x41 {
		t.Fatalf("unexpected in-flight payload: % x", payload[:4])
	}
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("reset did not observe the old-generation completion")
	}

	writer.Stop()
	_ = client.Close()
}

func TestDualSenseV5WriterResetBoundsBlockedWrite(t *testing.T) {
	server, client := net.Pipe()
	conn := newDeadlineTrackingConn(server)
	writer := newDualSenseOutputWriter(conn, nil, nil)
	feedback, speaker := testV5Media(0x31)
	writer.EnqueueAtomicAudioHaptics(feedback, speaker)
	go writer.Run()
	<-conn.started

	resetDone := make(chan struct{})
	go func() { writer.ResetSpeaker(); close(resetDone) }()
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("reset remained blocked after write deadline")
	}
	select {
	case <-writer.done:
	case <-time.After(time.Second):
		t.Fatal("timed-out write did not stop the stream")
	}
	if writer.streamViable.Load() || len(writer.audio) != 0 ||
		len(writer.audioFree) != dualSenseOutputAudioQueueCapacity {
		t.Fatal("failed stream retained V5 transport state")
	}
	buffer := make([]byte, StreamFrameHeaderSize+4)
	if count, err := client.Read(buffer); count != 0 || err == nil {
		t.Fatalf("failed stream replayed stale media: bytes=%d err=%v", count, err)
	}
	writer.Stop()
	_ = client.Close()
}

func TestDualSenseAndEdgeV5HandlersUseAtomicV5Contract(t *testing.T) {
	for _, edge := range []bool{false, true} {
		name := "DualSense"
		var dev usb.Device
		var handler func(net.Conn, *usb.Device, *slog.Logger) error
		var err error
		if edge {
			name = "DualSense Edge"
			variant := &dsedgehandler{}
			dev, err = variant.CreateDevice(nil)
			handler = variant.StreamHandler()
		} else {
			variant := &dshandler{}
			dev, err = variant.CreateDevice(nil)
			handler = variant.StreamHandler()
		}
		if err != nil {
			t.Fatalf("%s CreateDevice: %v", name, err)
		}

		server, client := net.Pipe()
		errCh := make(chan error, 1)
		go func() {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			errCh <- handler(server, &dev, logger)
		}()
		input, _ := NewInputState().MarshalBinary()
		if _, err := client.Write(makeV5StreamFrame(StreamFrameInputState, 0, input)); err != nil {
			t.Fatalf("%s write input: %v", name, err)
		}

		controller := dev.(*DualSense)
		controller.SetInterfaceAltSetting(InterfaceHapticsAudio, 1)
		pcm := make([]byte, dualSenseV5SpeakerFrames*USBHapticsAudioFrameSize)
		for frame := 0; frame < dualSenseV5SpeakerFrames; frame++ {
			offset := frame * USBHapticsAudioFrameSize
			binary.LittleEndian.PutUint16(pcm[offset:offset+2], uint16(frame+1))
		}
		controller.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
			usbip.DirOut, pcm)
		header, payload := readDualSenseOutputFrame(t, client)
		if header[4] != StreamFrameVersionV5 ||
			header[5] != StreamFrameAtomicAudioHaptics {
			t.Fatalf("%s emitted non-V5 transport: % x", name, header)
		}
		feedbackLength := int(binary.LittleEndian.Uint16(payload[:2]))
		if feedbackLength != OutputStateV5Size ||
			len(payload[2+feedbackLength:]) != dualSenseV5SpeakerPayloadSize {
			t.Fatalf("%s emitted wrong generation sizes", name)
		}

		_ = client.Close()
		if err := <-errCh; err != nil {
			t.Fatalf("%s stream handler: %v", name, err)
		}
		controller.callbackMu.RLock()
		callbacksCleared := controller.outputFunc == nil &&
			controller.atomicAudioHapticsFunc == nil &&
			controller.speakerResetFunc == nil &&
			controller.transportOutputFunc == nil &&
			controller.transportAtomicAudioFunc == nil &&
			controller.transportRealtimeHapticsFunc == nil &&
			controller.transportSpeakerResetFunc == nil
		controller.callbackMu.RUnlock()
		controller.microphoneMu.Lock()
		callbacksCleared = callbacksCleared &&
			controller.microphoneInterfaceStateFunc == nil
		controller.microphoneMu.Unlock()
		if !callbacksCleared {
			t.Fatalf("%s retained callbacks after shutdown", name)
		}
	}
}

func TestDualSenseV5EventsAliasPublishesMicrophoneInterfaceLifecycle(t *testing.T) {
	variant := &dshandler{micInterfaceEvents: true}
	dev, err := variant.CreateDevice(nil)
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	if got := dev.(*DualSense).VIIPERDeviceType(); got != DeviceTypeCombinedAudioDuplexV5Events {
		t.Fatalf("event alias type=%q", got)
	}

	server, client := net.Pipe()
	errCh := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		errCh <- variant.StreamHandler()(server, &dev, logger)
	}()

	header, payload := readDualSenseOutputFrame(t, client)
	if header[5] != StreamFrameMicrophoneInterfaceState || len(payload) != 9 ||
		payload[0] != 0 {
		t.Fatalf("initial microphone event: header=% x payload=% x", header, payload)
	}
	generation := binary.LittleEndian.Uint64(payload[1:])
	if generation == 0 {
		t.Fatal("initial microphone event used generation zero")
	}

	dev.(*DualSense).SetInterfaceAltSetting(InterfaceMicrophone, 1)
	header, payload = readDualSenseOutputFrame(t, client)
	if header[5] != StreamFrameMicrophoneInterfaceState || payload[0] != 1 ||
		binary.LittleEndian.Uint64(payload[1:]) != generation {
		t.Fatalf("active microphone event: header=% x payload=% x", header, payload)
	}

	dev.(*DualSense).SetInterfaceAltSetting(InterfaceMicrophone, 0)
	header, payload = readDualSenseOutputFrame(t, client)
	if header[5] != StreamFrameMicrophoneInterfaceState || payload[0] != 0 ||
		binary.LittleEndian.Uint64(payload[1:]) != generation {
		t.Fatalf("inactive microphone event: header=% x payload=% x", header, payload)
	}

	_ = client.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("event stream handler: %v", err)
	}
}

func TestDualSenseGamepadRawInputAliasHasNoMicrophoneLifecycle(t *testing.T) {
	variant := &dshandler{
		gamepadOnly:           true,
		physicalInputMetadata: true,
	}
	dev, err := variant.CreateDevice(nil)
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	if got := dev.(*DualSense).VIIPERDeviceType(); got != DeviceTypeGamepadOnlyV5RawInput {
		t.Fatalf("raw gamepad alias type=%q", got)
	}

	server, client := net.Pipe()
	errCh := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		errCh <- variant.StreamHandler()(server, &dev, logger)
	}()

	if err := client.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	var firstByte [1]byte
	if _, err := client.Read(firstByte[:]); err == nil {
		t.Fatalf("raw gamepad alias received microphone event byte %#x",
			firstByte[0])
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("raw gamepad read returned non-timeout error: %v", err)
	}
	if err := client.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear read deadline: %v", err)
	}

	state := *NewInputState()
	state.PhysicalMetadataValid = true
	state.PhysicalInputMetadata[physicalMetadataR2Status] = 0x09
	state.PhysicalInputMetadata[physicalMetadataL2Status] = 0x09
	var payload [InputStateRawSize]byte
	if err := state.MarshalRawInputInto(payload[:]); err != nil {
		t.Fatalf("MarshalRawInputInto: %v", err)
	}
	if _, err := client.Write(makeV5StreamFrame(
		StreamFrameInputState, 0, payload[:])); err != nil {
		t.Fatalf("write raw input: %v", err)
	}
	_ = client.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("raw gamepad stream handler: %v", err)
	}
}

func TestDualSenseLegacyV5AliasDoesNotEmitMicrophoneInterfaceEvent(t *testing.T) {
	variant := &dshandler{}
	dev, err := variant.CreateDevice(nil)
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	server, client := net.Pipe()
	errCh := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		errCh <- variant.StreamHandler()(server, &dev, logger)
	}()

	if err := client.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	var firstByte [1]byte
	if _, err := client.Read(firstByte[:]); err == nil {
		t.Fatalf("legacy V5 alias received unsupported output frame byte %#x",
			firstByte[0])
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("legacy V5 read returned non-timeout error: %v", err)
	}
	_ = client.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("legacy stream handler: %v", err)
	}
}

func readDualSenseOutputFrame(t *testing.T, reader io.Reader) ([]byte, []byte) {
	t.Helper()
	header := make([]byte, StreamFrameHeaderSize)
	if _, err := io.ReadFull(reader, header); err != nil {
		t.Fatalf("read output frame header: %v", err)
	}
	payload := make([]byte, int(binary.LittleEndian.Uint16(header[6:8])))
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatalf("read output frame payload: %v", err)
	}
	return header, payload
}

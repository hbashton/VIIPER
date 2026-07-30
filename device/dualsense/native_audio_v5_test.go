package dualsense

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
)

type capturedV5Generation struct {
	feedback [BluetoothCombinedHapticsReportSize]byte
	speaker  []byte
}

func TestAppendDualSenseV5SpeakerPreservesRawFrontPair(t *testing.T) {
	source := make([]byte, dualSenseV5SpeakerFrames*USBHapticsAudioFrameSize)
	for frame := 0; frame < dualSenseV5SpeakerFrames; frame++ {
		offset := frame * USBHapticsAudioFrameSize
		binary.LittleEndian.PutUint16(source[offset:offset+2], uint16(int16(frame*30)))
		binary.LittleEndian.PutUint16(source[offset+2:offset+4], uint16(int16(-frame*30)))
		binary.LittleEndian.PutUint16(source[offset+4:offset+6], uint16(int16(1000+frame)))
		binary.LittleEndian.PutUint16(source[offset+6:offset+8], uint16(int16(-1000-frame)))
	}
	destination := make([]byte, 0, dualSenseV5SpeakerPayloadSize)

	if allocations := testing.AllocsPerRun(1000, func() {
		destination = appendDualSenseV5Speaker(destination[:0], source)
	}); allocations != 0 {
		t.Fatalf("V5 front-channel assembler allocated %.2f objects per generation", allocations)
	}
	destination = appendDualSenseV5Speaker(destination[:0], source)
	if len(destination) != dualSenseV5SpeakerPayloadSize {
		t.Fatalf("unexpected V5 speaker size: got %d want %d",
			len(destination), dualSenseV5SpeakerPayloadSize)
	}

	for _, outputFrame := range []int{0, 1, 14, 15, 16, 478, 479} {
		offset := outputFrame * dualSenseV5SpeakerFrameSize
		left := int16(binary.LittleEndian.Uint16(destination[offset : offset+2]))
		right := int16(binary.LittleEndian.Uint16(destination[offset+2 : offset+4]))
		wantLeft := int16(outputFrame * 30)
		if left != wantLeft || right != -wantLeft {
			t.Fatalf("V5 frame %d changed: got %d/%d want %d/%d",
				outputFrame, left, right, wantLeft, -wantLeft)
		}
	}
}

func TestDualSenseV5MaintainsIndependentGenerationsAcrossArbitraryUSBChunks(t *testing.T) {
	const sourceFrames = 16*dualSenseV5SpeakerFrames + 137
	source := make([]byte, sourceFrames*USBHapticsAudioFrameSize)
	for frame := 0; frame < sourceFrames; frame++ {
		offset := frame * USBHapticsAudioFrameSize
		left := int16((frame*37)%30000 - 15000)
		right := int16(12000 - (frame*29)%24000)
		rearLeft := int16((frame*43)%28000 - 14000)
		rearRight := int16(13000 - (frame*31)%26000)
		binary.LittleEndian.PutUint16(source[offset:offset+2], uint16(left))
		binary.LittleEndian.PutUint16(source[offset+2:offset+4], uint16(right))
		binary.LittleEndian.PutUint16(source[offset+4:offset+6], uint16(rearLeft))
		binary.LittleEndian.PutUint16(source[offset+6:offset+8], uint16(rearRight))
	}

	chunkedDevice, chunked := newV5CaptureDevice(t)
	chunkPattern := []int{1, 47, 432, 17, 495, 3, 256, 31, 509, 2, 113}
	offsetFrames := 0
	for patternIndex := 0; offsetFrames < sourceFrames; patternIndex++ {
		frames := min(chunkPattern[patternIndex%len(chunkPattern)], sourceFrames-offsetFrames)
		start := offsetFrames * USBHapticsAudioFrameSize
		offsetFrames += frames
		end := offsetFrames * USBHapticsAudioFrameSize
		chunkedDevice.HandleTransfer(context.Background(),
			EndpointHapticsAudioOut, usbip.DirOut, source[start:end])
	}
	if offsetFrames != sourceFrames {
		t.Fatalf("test chunks consumed %d frames, want %d", offsetFrames, sourceFrames)
	}

	wholeDevice, whole := newV5CaptureDevice(t)
	wholeDevice.HandleTransfer(context.Background(),
		EndpointHapticsAudioOut, usbip.DirOut, source)

	if want := sourceFrames / dualSenseV5SpeakerFrames; len(*chunked) != want || len(*whole) != want {
		t.Fatalf("unexpected V5 generation count: chunked=%d whole=%d",
			len(*chunked), len(*whole))
	}
	for generation := range *whole {
		chunkedGeneration := (*chunked)[generation]
		wholeGeneration := (*whole)[generation]
		if len(chunkedGeneration.speaker) != dualSenseV5SpeakerPayloadSize {
			t.Fatalf("generation %d speaker has %d bytes, want %d",
				generation, len(chunkedGeneration.speaker), dualSenseV5SpeakerPayloadSize)
		}
		if !bytes.Equal(chunkedGeneration.speaker, wholeGeneration.speaker) {
			t.Fatalf("generation %d changed across USB chunk boundaries", generation)
		}
		if chunkedGeneration.feedback != wholeGeneration.feedback {
			t.Fatalf("generation %d haptics/state changed across USB chunk boundaries",
				generation)
		}

		startFrame := generation * dualSenseV5SpeakerFrames
		wantSpeaker := make([]byte, dualSenseV5SpeakerPayloadSize)
		for frame := 0; frame < dualSenseV5SpeakerFrames; frame++ {
			sourceOffset := (startFrame + frame) * USBHapticsAudioFrameSize
			destinationOffset := frame * dualSenseV5SpeakerFrameSize
			copy(wantSpeaker[destinationOffset:destinationOffset+dualSenseV5SpeakerFrameSize],
				source[sourceOffset:sourceOffset+dualSenseV5SpeakerFrameSize])
		}
		if !bytes.Equal(chunkedGeneration.speaker, wantSpeaker) {
			t.Fatalf("generation %d was resampled or reordered", generation)
		}

		boundaryFrame := (generation + 1) * dualSenseV5SpeakerFrames
		completedHaptics := boundaryFrame / 512
		wantHaptics := make([]byte, BluetoothHapticsSampleSize)
		if completedHaptics > 0 {
			hapticsStart := (completedHaptics - 1) * 512 * USBHapticsAudioFrameSize
			copyUSBHapticsChannelsToBluetoothSample(wantHaptics,
				source[hapticsStart:hapticsStart+512*USBHapticsAudioFrameSize])
		}
		gotHaptics := chunkedGeneration.feedback[BluetoothCombinedHapticsOffset : BluetoothCombinedHapticsOffset+BluetoothHapticsSampleSize]
		if !bytes.Equal(gotHaptics, wantHaptics) {
			t.Fatalf("generation %d did not carry latest completed rear feedback", generation)
		}
		if wantSequence := byte(max(0, completedHaptics-1)); chunkedGeneration.feedback[10] != wantSequence {
			t.Fatalf("generation %d feedback sequence=%d want=%d",
				generation, chunkedGeneration.feedback[10], wantSequence)
		}
	}

	chunkedDevice.mtx.Lock()
	hapticsRemaining := len(chunkedDevice.hapticsPCM)
	speakerRemaining := len(chunkedDevice.v5SpeakerPCM)
	chunkedDevice.mtx.Unlock()
	if want := sourceFrames % 512 * USBHapticsAudioFrameSize; hapticsRemaining != want {
		t.Fatalf("V5 haptics assembler retained %d bytes, want %d", hapticsRemaining, want)
	}
	if want := sourceFrames % dualSenseV5SpeakerFrames * dualSenseV5SpeakerFrameSize; speakerRemaining != want {
		t.Fatalf("V5 speaker assembler retained %d bytes, want %d", speakerRemaining, want)
	}
}

func TestDualSenseV5EndpointResetIsHardBoundaryForBothMediaClocks(t *testing.T) {
	device, captured := newV5CaptureDevice(t)
	stale := makeV5USBPCM(0, dualSenseV5SpeakerFrames-1, 11000)
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, stale)
	if len(*captured) != 0 {
		t.Fatal("partial V5 speaker generation was published")
	}

	device.ResetEndpoint(EndpointHapticsAudioOut)
	fresh := makeV5USBPCM(1000, 2*dualSenseV5SpeakerFrames, 22000)
	firstBoundary := dualSenseV5SpeakerFrames * USBHapticsAudioFrameSize
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, fresh[:firstBoundary])
	if len(*captured) != 1 {
		t.Fatalf("reset generation count=%d want=1", len(*captured))
	}
	if !bytes.Equal((*captured)[0].speaker,
		frontStereoFromUSB(fresh[:firstBoundary])) {
		t.Fatal("pre-reset speaker samples crossed the endpoint boundary")
	}
	if got := (*captured)[0].feedback[BluetoothCombinedHapticsOffset : BluetoothCombinedHapticsOffset+BluetoothHapticsSampleSize]; !bytes.Equal(got, make([]byte, BluetoothHapticsSampleSize)) {
		t.Fatal("pre-reset rear haptics crossed the endpoint boundary")
	}

	// Complete the first 512-frame rear interval, then the next 480-frame
	// speaker interval using deliberately split USB submissions.
	hapticsBoundary := 512 * USBHapticsAudioFrameSize
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, fresh[firstBoundary:hapticsBoundary])
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, fresh[hapticsBoundary:])
	if len(*captured) != 2 {
		t.Fatalf("post-reset generation count=%d want=2", len(*captured))
	}
	wantHaptics := make([]byte, BluetoothHapticsSampleSize)
	copyUSBHapticsChannelsToBluetoothSample(wantHaptics, fresh[:hapticsBoundary])
	gotHaptics := (*captured)[1].feedback[BluetoothCombinedHapticsOffset : BluetoothCombinedHapticsOffset+BluetoothHapticsSampleSize]
	if !bytes.Equal(gotHaptics, wantHaptics) {
		t.Fatal("post-reset speaker did not carry the fresh complete haptics generation")
	}

	device.mtx.Lock()
	hapticsRemaining := len(device.hapticsPCM)
	speakerRemaining := len(device.v5SpeakerPCM)
	device.mtx.Unlock()
	if hapticsRemaining != (2*dualSenseV5SpeakerFrames-512)*USBHapticsAudioFrameSize ||
		speakerRemaining != 0 {
		t.Fatalf("wrong post-reset remainders: haptics=%d speaker=%d",
			hapticsRemaining, speakerRemaining)
	}
}

func TestDualSenseV5SpeakerCarriesLatestCompleteFeedbackInSourceOrder(t *testing.T) {
	device, captured := newV5CaptureDevice(t)
	setV5TestLightbar(t, device, 0x11)
	pcm := makeV5USBPCM(0, 3*dualSenseV5SpeakerFrames, 15000)

	// Speaker boundary 480 precedes the first haptics boundary 512.
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, pcm[:480*USBHapticsAudioFrameSize])
	// Complete feedback A, then change live state to B before speaker boundary
	// 960. That speaker must retain A because B has no complete rear interval.
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, pcm[480*USBHapticsAudioFrameSize:512*USBHapticsAudioFrameSize])
	setV5TestLightbar(t, device, 0x77)
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, pcm[512*USBHapticsAudioFrameSize:960*USBHapticsAudioFrameSize])
	// Boundary 1024 completes feedback B; boundary 1440 must carry B.
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, pcm[960*USBHapticsAudioFrameSize:])

	if len(*captured) != 3 {
		t.Fatalf("captured %d speaker generations, want 3", len(*captured))
	}
	const embeddedRedOffset = 13 + 44 // raw USB byte 45 becomes state byte 44.
	if got := (*captured)[0].feedback[embeddedRedOffset]; got != 0x11 {
		t.Fatalf("initial complete state red=%02x want=11", got)
	}
	if got := (*captured)[1].feedback[embeddedRedOffset]; got != 0x11 {
		t.Fatalf("incomplete state update leaked into generation 1: red=%02x", got)
	}
	if got := (*captured)[2].feedback[embeddedRedOffset]; got != 0x77 {
		t.Fatalf("latest complete state missing from generation 2: red=%02x", got)
	}
	if (*captured)[1].feedback[10] != 0 || (*captured)[2].feedback[10] != 1 {
		t.Fatalf("rear feedback order changed: sequences=%d/%d",
			(*captured)[1].feedback[10], (*captured)[2].feedback[10])
	}
}

func TestDualSenseV5WriterPublishesExactAtomicContract(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, StreamFrameVersionV5, nil, nil)
	go writer.Run()

	feedback := make([]byte, OutputStateCombinedExtSize)
	feedback[OutputStateCombinedBluetoothOffset] = BluetoothCombinedHapticsReportID
	speaker := make([]byte, dualSenseV5SpeakerPayloadSize)
	for index := range speaker {
		speaker[index] = byte(index)
	}
	writer.EnqueueAtomicAudioHaptics(feedback, speaker)

	header, payload := readDualSenseOutputFrame(t, client)
	if header[4] != StreamFrameVersionV5 ||
		header[5] != StreamFrameAtomicAudioHaptics {
		t.Fatalf("unexpected V5 frame header: % x", header)
	}
	feedbackLength := int(binary.LittleEndian.Uint16(payload[:2]))
	if feedbackLength != len(feedback) {
		t.Fatalf("unexpected V5 feedback length: got %d want %d",
			feedbackLength, len(feedback))
	}
	if got := payload[2 : 2+feedbackLength]; string(got) != string(feedback) {
		t.Fatal("V5 frame changed atomic feedback")
	}
	if got := payload[2+feedbackLength:]; len(got) != dualSenseV5SpeakerPayloadSize ||
		string(got) != string(speaker) {
		t.Fatalf("V5 frame has invalid speaker tail: got %d bytes", len(got))
	}

	_ = client.Close()
	writer.Stop()
}

func TestDualSenseV5WriterRetainsNewestGenerationWhenBounded(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, StreamFrameVersionV5, nil, nil)
	for generation := 0; generation <= dualSenseOutputAudioQueueCapacity; generation++ {
		feedback := []byte{byte(generation)}
		speaker := make([]byte, dualSenseV5SpeakerPayloadSize)
		speaker[0] = byte(generation)
		writer.EnqueueAtomicAudioHaptics(feedback, speaker)
	}

	if len(writer.audio) != dualSenseOutputAudioQueueCapacity {
		t.Fatalf("V5 queue depth=%d want=%d",
			len(writer.audio), dualSenseOutputAudioQueueCapacity)
	}
	state := writer.telemetry.snapshot()
	if state.ReceivedPayloads != dualSenseOutputAudioQueueCapacity+1 ||
		state.EnqueuedPayloads != dualSenseOutputAudioQueueCapacity+1 ||
		state.DroppedPayloads != 1 ||
		state.DroppedBytes != dualSenseV5SpeakerPayloadSize {
		t.Fatalf("unexpected V5 bounded telemetry: %+v", state)
	}

	for expected := 1; expected <= dualSenseOutputAudioQueueCapacity; expected++ {
		frame := <-writer.audio
		feedbackLength := int(binary.LittleEndian.Uint16(frame.payload[:2]))
		feedback := frame.payload[2 : 2+feedbackLength]
		speaker := frame.payload[2+feedbackLength:]
		if len(feedback) != 1 || feedback[0] != byte(expected) ||
			len(speaker) != dualSenseV5SpeakerPayloadSize || speaker[0] != byte(expected) {
			t.Fatalf("V5 queue retained wrong generation at %d: feedback=% x speaker0=%d",
				expected, feedback, speaker[0])
		}
		writer.release(frame)
	}
	if len(writer.audioFree) != dualSenseOutputAudioQueueCapacity {
		t.Fatalf("V5 bounded queue leaked buffers: free=%d", len(writer.audioFree))
	}
}

func newV5CaptureDevice(t *testing.T) (*DualSense, *[]capturedV5Generation) {
	t.Helper()
	device, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	device.combinedBluetoothFeedback = true
	device.streamFrameVersion = StreamFrameVersionV5
	captured := make([]capturedV5Generation, 0, 2)
	device.SetAtomicAudioHapticsCallback(func(feedback OutputState, speaker []byte) {
		generation := capturedV5Generation{
			speaker: append([]byte(nil), speaker...),
		}
		generation.feedback = feedback.BluetoothCombinedOutputReport
		captured = append(captured, generation)
	})
	// The production stream handler installs both callbacks. Output reports are
	// intentionally ignored when no output consumer owns the device.
	device.SetOutputCallback(func(OutputState) {})
	device.SetInterfaceAltSetting(InterfaceHapticsAudio, 1)
	return device, &captured
}

func makeV5USBPCM(firstFrame, frames int, rearBias int16) []byte {
	pcm := make([]byte, frames*USBHapticsAudioFrameSize)
	for frame := 0; frame < frames; frame++ {
		absolute := firstFrame + frame
		offset := frame * USBHapticsAudioFrameSize
		binary.LittleEndian.PutUint16(pcm[offset:offset+2], uint16(int16(absolute%30000)))
		binary.LittleEndian.PutUint16(pcm[offset+2:offset+4], uint16(int16(-absolute%30000)))
		binary.LittleEndian.PutUint16(pcm[offset+4:offset+6], uint16(rearBias+int16(absolute%1000)))
		binary.LittleEndian.PutUint16(pcm[offset+6:offset+8], uint16(-rearBias-int16(absolute%1000)))
	}
	return pcm
}

func frontStereoFromUSB(pcm []byte) []byte {
	front := make([]byte, len(pcm)/USBHapticsAudioFrameSize*dualSenseV5SpeakerFrameSize)
	copyDualSenseSpeakerChannels(front, pcm)
	return front
}

func setV5TestLightbar(t *testing.T, device *DualSense, red byte) {
	t.Helper()
	report := make([]byte, OutputReportSize)
	report[0] = ReportIDOutput
	report[2] = 0x04
	report[45] = red
	device.HandleTransfer(context.Background(), EndpointOut, usbip.DirOut, report)
}

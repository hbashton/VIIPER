package dualsense

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

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

	if !raceDetectorEnabled {
		if allocations := testing.AllocsPerRun(1000, func() {
			destination = appendDualSenseV5Speaker(destination[:0], source)
		}); allocations != 0 {
			t.Fatalf("V5 front-channel assembler allocated %.2f objects per generation", allocations)
		}
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

func TestDualSenseV5PublishesCompletedHapticsBeforeNextSpeakerBoundary(t *testing.T) {
	device, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	var speakerGenerations int
	var realtimeGenerations []OutputState
	device.SetAtomicAudioHapticsCallback(func(OutputState, []byte) {
		speakerGenerations++
	})
	device.SetRealtimeHapticsCallback(func(feedback OutputState) {
		realtimeGenerations = append(realtimeGenerations, feedback)
	})
	device.SetInterfaceAltSetting(InterfaceHapticsAudio, 1)

	pcm := makeV5USBPCM(0, 512, 12000)
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, pcm[:480*USBHapticsAudioFrameSize])
	if speakerGenerations != 1 || len(realtimeGenerations) != 0 {
		t.Fatalf("480-frame boundary published speaker=%d haptics=%d, want 1/0",
			speakerGenerations, len(realtimeGenerations))
	}

	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, pcm[480*USBHapticsAudioFrameSize:])
	if speakerGenerations != 1 || len(realtimeGenerations) != 1 {
		t.Fatalf("512-frame boundary published speaker=%d haptics=%d, want 1/1",
			speakerGenerations, len(realtimeGenerations))
	}
	want := make([]byte, BluetoothHapticsSampleSize)
	copyUSBHapticsChannelsToBluetoothSample(want, pcm)
	got := realtimeGenerations[0].BluetoothCombinedOutputReport[BluetoothCombinedHapticsOffset : BluetoothCombinedHapticsOffset+BluetoothHapticsSampleSize]
	if !bytes.Equal(got, want) {
		t.Fatal("realtime callback changed the completed rear-channel generation")
	}
}

func TestDualSenseV5RealtimeHapticsCountersFollowRearClock(t *testing.T) {
	device, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	var realtime []OutputState
	device.SetAtomicAudioHapticsCallback(func(OutputState, []byte) {})
	device.SetRealtimeHapticsCallback(func(feedback OutputState) {
		realtime = append(realtime, feedback)
	})
	device.SetInterfaceAltSetting(InterfaceHapticsAudio, 1)

	// Cross the 7,680-frame common boundary and one more rear interval. The
	// 480-frame speaker counter advances an extra time at that boundary; the
	// realtime haptics counter must remain contiguous and independent.
	const rearGenerations = 16
	pcm := makeV5USBPCM(0, rearGenerations*512, 12000)
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, pcm)
	if len(realtime) != rearGenerations {
		t.Fatalf("realtime generations=%d want=%d",
			len(realtime), rearGenerations)
	}
	for generation, feedback := range realtime {
		report := feedback.BluetoothCombinedOutputReport
		if got := report[10]; got != byte(generation) {
			t.Fatalf("generation %d packet sequence=%d", generation, got)
		}
		if got := report[1]; got != byte(generation&0x0f)<<4 {
			t.Fatalf("generation %d report tag=%02x", generation, got)
		}
	}
}

func TestDualSenseV5MaintainsIndependentGenerationsAcrossArbitraryUSBChunks(t *testing.T) {
	// Seventeen speaker generations cross the second V5 rear-lane
	// shortage at generation 16. Stop one frame before the next 512 boundary so
	// no completed rear sample remains queued after the final speaker report.
	const sourceFrames = 17*dualSenseV5SpeakerFrames + 31
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

		previousBoundary := generation * dualSenseV5SpeakerFrames
		boundaryFrame := (generation + 1) * dualSenseV5SpeakerFrames
		completedBefore := previousBoundary / 512
		completedHaptics := boundaryFrame / 512
		wantHaptics := make([]byte, BluetoothHapticsSampleSize)
		if completedHaptics > completedBefore {
			hapticsStart := (completedHaptics - 1) * 512 * USBHapticsAudioFrameSize
			copyUSBHapticsChannelsToBluetoothSample(wantHaptics,
				source[hapticsStart:hapticsStart+512*USBHapticsAudioFrameSize])
		}
		gotHaptics := chunkedGeneration.feedback[BluetoothCombinedHapticsOffset : BluetoothCombinedHapticsOffset+BluetoothHapticsSampleSize]
		if !bytes.Equal(gotHaptics, wantHaptics) {
			t.Fatalf("generation %d did not consume exactly one completed rear sample or silence", generation)
		}
		if wantSequence := byte(generation); chunkedGeneration.feedback[10] != wantSequence {
			t.Fatalf("generation %d feedback sequence=%d want=%d",
				generation, chunkedGeneration.feedback[10], wantSequence)
		}
		if wantTag := byte(generation&0x0f) << 4; chunkedGeneration.feedback[1] != wantTag {
			t.Fatalf("generation %d feedback tag=%02x want=%02x",
				generation, chunkedGeneration.feedback[1], wantTag)
		}
	}
	firstShortage := (*chunked)[0].feedback[BluetoothCombinedHapticsOffset : BluetoothCombinedHapticsOffset+BluetoothHapticsSampleSize]
	secondShortage := (*chunked)[16].feedback[BluetoothCombinedHapticsOffset : BluetoothCombinedHapticsOffset+BluetoothHapticsSampleSize]
	previousRear := (*chunked)[15].feedback[BluetoothCombinedHapticsOffset : BluetoothCombinedHapticsOffset+BluetoothHapticsSampleSize]
	if !bytes.Equal(firstShortage, make([]byte, BluetoothHapticsSampleSize)) ||
		!bytes.Equal(secondShortage, make([]byte, BluetoothHapticsSampleSize)) {
		t.Fatal("V5 replayed rear haptics when the 512-frame lane had no completed sample")
	}
	if bytes.Equal(previousRear, make([]byte, BluetoothHapticsSampleSize)) {
		t.Fatal("V5 shortage test did not distinguish silence from the prior rear sample")
	}

	chunkedDevice.mtx.Lock()
	hapticsRemaining := len(chunkedDevice.hapticsPCM)
	speakerRemaining := len(chunkedDevice.v5SpeakerPCM)
	hapticsQueued := len(chunkedDevice.v5HapticsQueue)
	chunkedDevice.mtx.Unlock()
	if want := sourceFrames % 512 * USBHapticsAudioFrameSize; hapticsRemaining != want {
		t.Fatalf("V5 haptics assembler retained %d bytes, want %d", hapticsRemaining, want)
	}
	if want := sourceFrames % dualSenseV5SpeakerFrames * dualSenseV5SpeakerFrameSize; speakerRemaining != want {
		t.Fatalf("V5 speaker assembler retained %d bytes, want %d", speakerRemaining, want)
	}
	if hapticsQueued != 0 {
		t.Fatalf("V5 retained %d completed rear samples after presentation", hapticsQueued)
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

func TestDualSenseV5RejectsPublicationFromPreResetRevision(t *testing.T) {
	device, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	device.SetInterfaceAltSetting(InterfaceHapticsAudio, 1)
	published := 0
	device.setV5MediaCallbacks(func(OutputState, []byte) {
		published++
	}, nil, nil)

	device.mtx.Lock()
	revision := device.mediaRevision
	device.mtx.Unlock()
	pending := pendingBluetoothHapticsReport{
		revision:   revision,
		feedback:   OutputState{BluetoothCombinedOutputReport: [BluetoothCombinedHapticsReportSize]byte{BluetoothCombinedHapticsReportID}},
		speakerPCM: make([]byte, dualSenseV5SpeakerPayloadSize),
	}

	device.ResetEndpoint(EndpointHapticsAudioOut)
	if device.publishV5Media(pending) {
		t.Fatal("pre-reset media revision was published")
	}
	if published != 0 {
		t.Fatalf("pre-reset media callback count=%d", published)
	}
}

func TestDualSenseV5ResetWaitsForInFlightDevicePublication(t *testing.T) {
	device, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	device.SetInterfaceAltSetting(InterfaceHapticsAudio, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	callbackDone := make(chan struct{})
	resetCalls := 0
	device.setV5MediaCallbacks(func(OutputState, []byte) {
		close(entered)
		<-release
		close(callbackDone)
	}, nil, func() {
		resetCalls++
	})

	pcm := makeV5USBPCM(0, dualSenseV5SpeakerFrames, 12000)
	transferDone := make(chan struct{})
	go func() {
		device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
			usbip.DirOut, pcm)
		close(transferDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("media callback did not start")
	}

	resetDone := make(chan struct{})
	go func() {
		device.ResetEndpoint(EndpointHapticsAudioOut)
		close(resetDone)
	}()
	select {
	case <-resetDone:
		t.Fatal("endpoint reset crossed an in-flight device publication")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("media callback did not finish")
	}
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("endpoint reset did not finish after publication")
	}
	<-transferDone
	if resetCalls != 1 {
		t.Fatalf("transport reset calls=%d want=1", resetCalls)
	}
}

func TestDualSenseV5SpeakerCombinesFreshStateWithCompletedRearSample(t *testing.T) {
	device, captured := newV5CaptureDevice(t)
	setV5TestLightbar(t, device, 0x11)
	pcm := makeV5USBPCM(0, 3*dualSenseV5SpeakerFrames, 15000)

	// Speaker boundary 480 precedes the first haptics boundary 512.
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, pcm[:480*USBHapticsAudioFrameSize])
	// Complete rear sample A, then change live state to B before speaker boundary
	// 960. V5 combines that completed sample with state B at presentation;
	// state is not frozen on the independent 512-frame rear clock.
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, pcm[480*USBHapticsAudioFrameSize:512*USBHapticsAudioFrameSize])
	setV5TestLightbar(t, device, 0x77)
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, pcm[512*USBHapticsAudioFrameSize:960*USBHapticsAudioFrameSize])
	// Boundary 1024 completes the next rear sample; state B remains current at
	// boundary 1440.
	device.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, pcm[960*USBHapticsAudioFrameSize:])

	if len(*captured) != 3 {
		t.Fatalf("captured %d speaker generations, want 3", len(*captured))
	}
	const embeddedRedOffset = 13 + 44 // raw USB byte 45 becomes state byte 44.
	if got := (*captured)[0].feedback[embeddedRedOffset]; got != 0x11 {
		t.Fatalf("initial complete state red=%02x want=11", got)
	}
	if got := (*captured)[1].feedback[embeddedRedOffset]; got != 0x77 {
		t.Fatalf("generation 1 replayed stale state: red=%02x want=77", got)
	}
	if got := (*captured)[2].feedback[embeddedRedOffset]; got != 0x77 {
		t.Fatalf("latest complete state missing from generation 2: red=%02x", got)
	}
	if (*captured)[0].feedback[10] != 0 ||
		(*captured)[1].feedback[10] != 1 ||
		(*captured)[2].feedback[10] != 2 {
		t.Fatalf("presentation counters did not advance per speaker report: sequences=%d/%d/%d",
			(*captured)[0].feedback[10], (*captured)[1].feedback[10],
			(*captured)[2].feedback[10])
	}
}

func TestDualSenseV5WriterPublishesExactAtomicContract(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, nil, nil)
	go writer.Run()

	feedback := make([]byte, OutputStateV5Size)
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
	if err := writer.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestDualSenseV5WriterRetainsNewestGenerationWhenBounded(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, nil, nil)
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
	if state.ReceivedPayloads != uint64(dualSenseOutputAudioQueueCapacity+1) ||
		state.EnqueuedPayloads != uint64(dualSenseOutputAudioQueueCapacity+1) ||
		state.DroppedPayloads != 1 ||
		state.DroppedBytes != dualSenseV5SpeakerPayloadSize {
		t.Fatalf("unexpected V5 bounded telemetry: %+v", state)
	}

	for expected := 1; expected <= dualSenseOutputAudioQueueCapacity; expected++ {
		frame := <-writer.audio
		writer.recordMediaDequeued(frame)
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
	if len(writer.audioFree) != dualSenseOutputAudioPoolCapacity {
		t.Fatalf("V5 bounded queue leaked buffers: free=%d", len(writer.audioFree))
	}
}

func newV5CaptureDevice(t *testing.T) (*DualSense, *[]capturedV5Generation) {
	t.Helper()
	device, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
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
	copyDualSenseV5SpeakerChannels(front, pcm)
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

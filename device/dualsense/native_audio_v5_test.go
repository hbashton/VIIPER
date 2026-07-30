package dualsense

import (
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

func TestResampleDualSenseV5SpeakerMatchesContinuousRationalPhase(t *testing.T) {
	source := make([]byte, dualSenseV5SourceFrames*USBHapticsAudioFrameSize)
	for frame := 0; frame < dualSenseV5SourceFrames; frame++ {
		offset := frame * USBHapticsAudioFrameSize
		binary.LittleEndian.PutUint16(source[offset:offset+2],
			uint16(int16(frame*30)))
		binary.LittleEndian.PutUint16(source[offset+2:offset+4],
			uint16(int16(-frame*30)))
	}
	destination := make([]byte, dualSenseV5SpeakerPayloadSize)

	if allocations := testing.AllocsPerRun(1000, func() {
		resampleDualSenseV5Speaker(destination, source)
	}); allocations != 0 {
		t.Fatalf("V5 rational resampler allocated %.2f objects per generation", allocations)
	}
	if written := resampleDualSenseV5Speaker(destination, source); written != dualSenseV5SpeakerPayloadSize {
		t.Fatalf("unexpected V5 speaker size: got %d want %d",
			written, dualSenseV5SpeakerPayloadSize)
	}

	for _, outputFrame := range []int{0, 1, 14, 15, 16, 478, 479} {
		positionNumerator := outputFrame * dualSenseV5SourceFrames
		sourceFrame := positionNumerator / dualSenseV5SpeakerFrames
		fraction := positionNumerator % dualSenseV5SpeakerFrames
		wantLeft := int16((int32(sourceFrame*30)*int32(dualSenseV5SpeakerFrames-fraction) +
			int32((min(sourceFrame+1, dualSenseV5SourceFrames-1))*30)*int32(fraction)) /
			int32(dualSenseV5SpeakerFrames))
		offset := outputFrame * dualSenseV5SpeakerFrameSize
		left := int16(binary.LittleEndian.Uint16(destination[offset : offset+2]))
		right := int16(binary.LittleEndian.Uint16(destination[offset+2 : offset+4]))
		if left != wantLeft || right != -wantLeft {
			t.Fatalf("V5 frame %d phase mismatch: got %d/%d want %d/%d",
				outputFrame, left, right, wantLeft, -wantLeft)
		}
	}
}

func TestDualSenseV5MaintainsGenerationsAcrossArbitraryUSBChunks(t *testing.T) {
	const sourceFrames = 2*dualSenseV5SourceFrames + 137
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
	chunkFrames := []int{7, 113, 401, 2, 509, 129}
	offsetFrames := 0
	for _, frames := range chunkFrames {
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

	if len(*chunked) != 2 || len(*whole) != 2 {
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
		if string(chunkedGeneration.speaker) != string(wholeGeneration.speaker) {
			t.Fatalf("generation %d changed across USB chunk boundaries", generation)
		}
		if chunkedGeneration.feedback != wholeGeneration.feedback {
			t.Fatalf("generation %d haptics/state changed across USB chunk boundaries",
				generation)
		}
		if chunkedGeneration.feedback[BluetoothCombinedHapticsOffset] == 0 &&
			chunkedGeneration.feedback[BluetoothCombinedHapticsOffset+1] == 0 {
			t.Fatalf("generation %d omitted independently downsampled rear haptics", generation)
		}
	}

	chunkedDevice.mtx.Lock()
	remaining := len(chunkedDevice.hapticsPCM)
	chunkedDevice.mtx.Unlock()
	if want := 137 * USBHapticsAudioFrameSize; remaining != want {
		t.Fatalf("V5 assembler retained %d bytes, want %d", remaining, want)
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
	device.SetInterfaceAltSetting(InterfaceHapticsAudio, 1)
	return device, &captured
}

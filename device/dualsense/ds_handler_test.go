package dualsense

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
)

func makeV5StreamFrame(frameType byte, sequence uint32, payload []byte) []byte {
	frame := make([]byte, StreamFrameHeaderSize+len(payload))
	frame[0] = StreamFrameMagic0
	frame[1] = StreamFrameMagic1
	frame[2] = StreamFrameMagic2
	frame[3] = StreamFrameMagic3
	frame[4] = StreamFrameVersionV5
	frame[5] = frameType
	binary.LittleEndian.PutUint16(frame[6:8], uint16(len(payload)))
	binary.LittleEndian.PutUint32(frame[8:12], sequence)
	copy(frame[StreamFrameHeaderSize:], payload)
	hash := crc32.NewIEEE()
	_, _ = hash.Write(frame[4:12])
	_, _ = hash.Write(payload)
	binary.LittleEndian.PutUint32(frame[12:16], hash.Sum32())
	return frame
}

func newV5ReaderTest(t *testing.T) (*DualSense, net.Conn, <-chan error) {
	t.Helper()
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)
	server, client := net.Pipe()
	errCh := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		errCh <- readDualSenseV5InputStream(server, dev, logger)
	}()
	return dev, client, errCh
}

func TestReadDualSenseV5InputStreamAcceptsInterleavedStateAndMicrophone(t *testing.T) {
	dev, client, errCh := newV5ReaderTest(t)
	state := NewInputState()
	state.LX = 64
	state.Buttons = ButtonCross | ButtonL1
	state.GyroX = -32768
	state.GyroY = 0x4350
	state.AccelZ = -12345
	input, _ := state.MarshalBinary()

	if _, err := client.Write(makeV5StreamFrame(StreamFrameInputState, 0, input)); err != nil {
		t.Fatalf("write state: %v", err)
	}
	for sequence := uint32(1); sequence <= microphoneTargetClientFrames; sequence++ {
		pcm := make([]byte, USBMicrophoneClientFrameSize)
		for index := range pcm {
			pcm[index] = byte(sequence)
		}
		if _, err := client.Write(makeV5StreamFrame(
			StreamFrameMicrophonePCM, sequence, pcm)); err != nil {
			t.Fatalf("write microphone frame %d: %v", sequence, err)
		}
	}
	_ = client.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("reader: %v", err)
	}

	dev.mtx.Lock()
	got := dev.inputState
	queued := dev.microphoneBuffer.State().QueuedBytes
	dev.mtx.Unlock()
	if got.LX != state.LX || got.Buttons != state.Buttons ||
		got.GyroX != state.GyroX || got.GyroY != state.GyroY ||
		got.AccelZ != state.AccelZ {
		t.Fatalf("V5 input changed: got=%+v want=%+v", got, state)
	}
	if queued != USBMicrophoneClientFrameSize*microphoneTargetClientFrames {
		t.Fatalf("queued microphone bytes=%d", queued)
	}
}

func TestReadDualSenseV5InputStreamRejectsLegacyVersion(t *testing.T) {
	_, client, errCh := newV5ReaderTest(t)
	frame := makeV5StreamFrame(StreamFrameInputState, 0,
		make([]byte, InputStateSize))
	frame[4] = 0x04
	if _, err := client.Write(frame[:StreamFrameHeaderSize]); err != nil {
		t.Fatalf("write legacy frame: %v", err)
	}
	_ = client.Close()
	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "requires V5 stream") {
		t.Fatalf("unexpected legacy version result: %v", err)
	}
}

func TestReadDualSenseV5InputStreamRejectsBadCRC(t *testing.T) {
	_, client, errCh := newV5ReaderTest(t)
	frame := makeV5StreamFrame(StreamFrameInputState, 0,
		make([]byte, InputStateSize))
	frame[12] ^= 0xFF
	if _, err := client.Write(frame); err != nil {
		t.Fatalf("write bad CRC frame: %v", err)
	}
	_ = client.Close()
	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "CRC mismatch") {
		t.Fatalf("unexpected CRC result: %v", err)
	}
}

func TestReadDualSenseV5InputStreamRejectsSequenceGap(t *testing.T) {
	_, client, errCh := newV5ReaderTest(t)
	payload := make([]byte, InputStateSize)
	if _, err := client.Write(makeV5StreamFrame(StreamFrameInputState, 7, payload)); err != nil {
		t.Fatalf("write first frame: %v", err)
	}
	if _, err := client.Write(makeV5StreamFrame(StreamFrameInputState, 9, payload)); err != nil {
		t.Fatalf("write skipped frame: %v", err)
	}
	_ = client.Close()
	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "sequence mismatch") {
		t.Fatalf("unexpected sequence result: %v", err)
	}
}

func TestReadDualSenseV5InputStreamRejectsInvalidControlBits(t *testing.T) {
	_, client, errCh := newV5ReaderTest(t)
	payload := make([]byte, InputStateSize)
	binary.LittleEndian.PutUint32(payload[4:8], 0x80000000)
	if _, err := client.Write(makeV5StreamFrame(StreamFrameInputState, 0, payload)); err != nil {
		t.Fatalf("write corrupt frame: %v", err)
	}
	_ = client.Close()
	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "invalid controls") {
		t.Fatalf("unexpected corrupt input result: %v", err)
	}
}

func TestReadDualSenseV5InputStreamRejectsUnknownFrameType(t *testing.T) {
	_, client, errCh := newV5ReaderTest(t)
	if _, err := client.Write(makeV5StreamFrame(0x7F, 0, nil)); err != nil {
		t.Fatalf("write unknown frame: %v", err)
	}
	_ = client.Close()
	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "unknown DualSense framed stream") {
		t.Fatalf("unexpected unknown-frame result: %v", err)
	}
}

func TestQueueMicrophonePCMFrameRequiresActiveInterface(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	frame := make([]byte, USBMicrophoneClientFrameSize)
	dev.QueueMicrophonePCMFrame(frame)
	if got := dev.GetDeviceSpecificArgs()["queuedMicrophoneBytes"]; got != 0 {
		t.Fatalf("inactive interface queued microphone PCM: %v", got)
	}
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)
	dev.QueueMicrophonePCMFrame(frame)
	if got := dev.GetDeviceSpecificArgs()["queuedMicrophoneBytes"]; got != USBMicrophoneClientFrameSize {
		t.Fatalf("active interface queued bytes=%v", got)
	}
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 0)
	if got := dev.GetDeviceSpecificArgs()["queuedMicrophoneBytes"]; got != 0 {
		t.Fatalf("interface close retained microphone PCM: %v", got)
	}
}

func TestDualSenseUpdateInputStateCopiesState(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	state := NewInputState()
	state.Buttons = ButtonTriangle
	dev.UpdateInputState(state)
	state.Buttons = ButtonCircle
	dev.mtx.Lock()
	got := dev.inputState.Buttons
	dev.mtx.Unlock()
	if got != ButtonTriangle {
		t.Fatalf("device retained caller-owned state: got=%#x", got)
	}
}

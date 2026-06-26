package dualsense

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
)

func makeStreamFrame(t *testing.T, frameType byte, payload []byte) []byte {
	t.Helper()
	if len(payload) > 0xFFFF {
		t.Fatalf("payload too large: %d", len(payload))
	}

	frame := make([]byte, StreamFrameHeaderSize+len(payload))
	frame[0] = StreamFrameMagic0
	frame[1] = StreamFrameMagic1
	frame[2] = StreamFrameMagic2
	frame[3] = StreamFrameMagic3
	frame[4] = StreamFrameVersion
	frame[5] = frameType
	binary.LittleEndian.PutUint16(frame[6:8], uint16(len(payload)))
	copy(frame[StreamFrameHeaderSize:], payload)
	return frame
}

func TestReadDualSenseInputStreamAcceptsVersionedMicFrames(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	server, client := net.Pipe()
	defer server.Close()

	errCh := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		errCh <- readDualSenseInputStream(server, dev, logger, true)
	}()

	state := NewInputState()
	state.LX = 42
	state.R2 = 99
	inputPayload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}
	microphonePayload := make([]byte, USBMicrophoneClientFrameSize)
	for i := range microphonePayload {
		microphonePayload[i] = byte(i)
	}

	if _, err := client.Write(makeStreamFrame(t, StreamFrameInputState, inputPayload)); err != nil {
		t.Fatalf("write input frame: %v", err)
	}
	if _, err := client.Write(makeStreamFrame(t, StreamFrameMicrophonePCM, microphonePayload)); err != nil {
		t.Fatalf("write microphone frame: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client pipe: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("readDualSenseInputStream returned error: %v", err)
	}

	dev.mtx.Lock()
	gotInput := *dev.inputState
	gotMicrophoneLen := len(dev.microphonePCM)
	dev.mtx.Unlock()

	if gotInput.LX != 42 || gotInput.R2 != 99 {
		t.Fatalf("unexpected input state: LX=%d R2=%d", gotInput.LX, gotInput.R2)
	}
	if gotMicrophoneLen != USBMicrophoneClientFrameSize {
		t.Fatalf("unexpected microphone queue length: %d", gotMicrophoneLen)
	}
}

func TestReadDualSenseInputStreamRejectsUnversionedMicFrames(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	server, client := net.Pipe()
	defer server.Close()

	errCh := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		errCh <- readDualSenseInputStream(server, dev, logger, true)
	}()

	oldStylePrefix := []byte{StreamFrameMicrophonePCM, 0, 0, 0, 0, 0, 0, 0}
	if _, err := client.Write(oldStylePrefix); err != nil {
		t.Fatalf("write old style prefix: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client pipe: %v", err)
	}

	err = <-errCh
	if err == nil || !strings.Contains(err.Error(), "invalid DualSense framed stream magic") {
		t.Fatalf("expected invalid magic error, got %v", err)
	}

	dev.mtx.Lock()
	gotMicrophoneLen := len(dev.microphonePCM)
	dev.mtx.Unlock()
	if gotMicrophoneLen != 0 {
		t.Fatalf("old style frame should not queue microphone data, got %d bytes", gotMicrophoneLen)
	}
}

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
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)

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

func TestQueueMicrophonePCMFrameRequiresActiveInterface(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	frame := make([]byte, USBMicrophoneClientFrameSize)
	dev.QueueMicrophonePCMFrame(frame)
	if len(dev.microphonePCM) != 0 {
		t.Fatalf("inactive mic interface should drop PCM, got %d bytes", len(dev.microphonePCM))
	}

	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)
	dev.QueueMicrophonePCMFrame(frame)
	if len(dev.microphonePCM) != USBMicrophoneClientFrameSize {
		t.Fatalf("active mic interface should queue PCM, got %d bytes", len(dev.microphonePCM))
	}

	dev.SetInterfaceAltSetting(InterfaceMicrophone, 0)
	if len(dev.microphonePCM) != 0 {
		t.Fatalf("closing mic interface should drop queued PCM, got %d bytes", len(dev.microphonePCM))
	}
}

func TestReadDualSenseInputStreamSanitizesTransportMagicInMotion(t *testing.T) {
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
	state.LX = 12
	state.R2 = 34
	state.Buttons = ButtonCross
	state.GyroX = 111
	state.GyroY = 222
	state.GyroZ = 333
	state.AccelX = 444
	state.AccelY = 555
	state.AccelZ = 666
	inputPayload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}
	copy(inputPayload[25:29], []byte{StreamFrameMagic0, StreamFrameMagic1, StreamFrameMagic2, StreamFrameMagic3})

	if _, err := client.Write(makeStreamFrame(t, StreamFrameInputState, inputPayload)); err != nil {
		t.Fatalf("write input frame: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client pipe: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("readDualSenseInputStream returned error: %v", err)
	}

	dev.mtx.Lock()
	gotInput := *dev.inputState
	dev.mtx.Unlock()

	if gotInput.LX != state.LX || gotInput.R2 != state.R2 || gotInput.Buttons != state.Buttons {
		t.Fatalf("non-motion input changed: got LX=%d R2=%d buttons=%#x", gotInput.LX, gotInput.R2, gotInput.Buttons)
	}
	if gotInput.GyroX != 0 || gotInput.GyroY != 0 || gotInput.GyroZ != 0 ||
		gotInput.AccelX != DefaultAccelXRaw || gotInput.AccelY != DefaultAccelYRaw || gotInput.AccelZ != DefaultAccelZRaw {
		t.Fatalf("motion was not sanitized: gyro=%d,%d,%d accel=%d,%d,%d",
			gotInput.GyroX, gotInput.GyroY, gotInput.GyroZ,
			gotInput.AccelX, gotInput.AccelY, gotInput.AccelZ)
	}
}

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
	copy(frame, makeStreamFrameHeader(frameType, len(payload)))
	copy(frame[StreamFrameHeaderSize:], payload)
	return frame
}

func makeStreamFrameHeader(frameType byte, payloadLength int) []byte {
	frame := make([]byte, StreamFrameHeaderSize)
	frame[0] = StreamFrameMagic0
	frame[1] = StreamFrameMagic1
	frame[2] = StreamFrameMagic2
	frame[3] = StreamFrameMagic3
	frame[4] = StreamFrameVersion
	frame[5] = frameType
	binary.LittleEndian.PutUint16(frame[6:8], uint16(payloadLength))
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
	gotInput := dev.inputState
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

func TestReadDualSenseInputStreamDropsCorruptedTransportMagicInput(t *testing.T) {
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
	copy(inputPayload[25:33], makeStreamFrameHeader(StreamFrameMicrophonePCM, USBMicrophoneClientFrameSize))

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
	gotInput := dev.inputState
	dev.mtx.Unlock()

	neutral := NewInputState()
	if gotInput.LX != neutral.LX || gotInput.LY != neutral.LY ||
		gotInput.RX != neutral.RX || gotInput.RY != neutral.RY ||
		gotInput.Buttons != neutral.Buttons || gotInput.DPad != neutral.DPad ||
		gotInput.L2 != neutral.L2 || gotInput.R2 != neutral.R2 {
		t.Fatalf("corrupted input was not reset to neutral controls: got LX=%d LY=%d RX=%d RY=%d buttons=%#x dpad=%#x L2=%d R2=%d",
			gotInput.LX, gotInput.LY, gotInput.RX, gotInput.RY, gotInput.Buttons, gotInput.DPad, gotInput.L2, gotInput.R2)
	}
	if gotInput.GyroX != 0 || gotInput.GyroY != 0 || gotInput.GyroZ != 0 ||
		gotInput.AccelX != DefaultAccelXRaw || gotInput.AccelY != DefaultAccelYRaw || gotInput.AccelZ != DefaultAccelZRaw {
		t.Fatalf("corrupted input motion was not reset to neutral: gyro=%d,%d,%d accel=%d,%d,%d",
			gotInput.GyroX, gotInput.GyroY, gotInput.GyroZ,
			gotInput.AccelX, gotInput.AccelY, gotInput.AccelZ)
	}
}

func TestReadDualSenseInputStreamDropsPlainTransportMagicInputBytes(t *testing.T) {
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
	state.LX = int8(StreamFrameMagic0)
	state.LY = int8(StreamFrameMagic1)
	state.RX = int8(StreamFrameMagic2)
	state.RY = int8(StreamFrameMagic3)
	state.R2 = 77
	inputPayload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}

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
	gotInput := dev.inputState
	dev.mtx.Unlock()

	neutral := NewInputState()
	if gotInput.LX != neutral.LX || gotInput.LY != neutral.LY ||
		gotInput.RX != neutral.RX || gotInput.RY != neutral.RY ||
		gotInput.R2 != neutral.R2 {
		t.Fatalf("plain transport magic bytes should reset input: got LX=%d LY=%d RX=%d RY=%d R2=%d",
			gotInput.LX, gotInput.LY, gotInput.RX, gotInput.RY, gotInput.R2)
	}
}

func TestReadDualSenseInputStreamDropsTransportMarkerFragments(t *testing.T) {
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
	state.LX = 55
	state.R2 = 88
	inputPayload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}
	copy(inputPayload[0:3], []byte{StreamFrameMagic1, StreamFrameMagic2, StreamFrameMagic3})

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
	gotInput := dev.inputState
	dev.mtx.Unlock()

	neutral := NewInputState()
	if gotInput.LX != neutral.LX || gotInput.R2 != neutral.R2 || gotInput.Buttons != neutral.Buttons {
		t.Fatalf("transport marker fragment should reset input: got LX=%d R2=%d buttons=%#x",
			gotInput.LX, gotInput.R2, gotInput.Buttons)
	}
}

func TestReadDualSenseInputStreamDropsTransportMarkerFragmentsInTouchMotion(t *testing.T) {
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
	state.LX = 55
	state.R2 = 88
	inputPayload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}
	copy(inputPayload[21:24], []byte{StreamFrameMagic0, StreamFrameMagic1, StreamFrameMagic2})

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
	gotInput := dev.inputState
	dev.mtx.Unlock()

	neutral := NewInputState()
	if gotInput.LX != neutral.LX || gotInput.R2 != neutral.R2 || gotInput.GyroX != neutral.GyroX {
		t.Fatalf("touch/motion transport marker fragment should reset input: got LX=%d R2=%d GyroX=%d",
			gotInput.LX, gotInput.R2, gotInput.GyroX)
	}
}

func TestReadDualSenseInputStreamDropsMicTransportLeakPatternInTouchMotion(t *testing.T) {
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
	state.LX = 55
	state.R2 = 88
	inputPayload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}
	copy(inputPayload[21:26], []byte{StreamFrameMagic2, StreamFrameMagic3, 0x01, 0x01, hidClassOUT})

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
	gotInput := dev.inputState
	dev.mtx.Unlock()

	neutral := NewInputState()
	if gotInput.LX != neutral.LX || gotInput.R2 != neutral.R2 || gotInput.GyroX != neutral.GyroX {
		t.Fatalf("touch/motion mic transport leak should reset input: got LX=%d R2=%d GyroX=%d",
			gotInput.LX, gotInput.R2, gotInput.GyroX)
	}
}

func TestReadDualSenseInputStreamDropsWeakMicTransportLeakPatternWhenMicrophoneActive(t *testing.T) {
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
	state.LX = 55
	state.R2 = 88
	inputPayload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}
	copy(inputPayload[21:24], []byte{0x01, 0x01, hidClassOUT})

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
	gotInput := dev.inputState
	dev.mtx.Unlock()

	neutral := NewInputState()
	if gotInput.LX != neutral.LX || gotInput.R2 != neutral.R2 || gotInput.GyroX != neutral.GyroX {
		t.Fatalf("weak touch/motion mic transport leak should reset input: got LX=%d R2=%d GyroX=%d",
			gotInput.LX, gotInput.R2, gotInput.GyroX)
	}
}

func TestInputStatePayloadPreservesShortTransportMarkerWhenMicrophoneInactive(t *testing.T) {
	state := NewInputState()
	state.LX = 55
	state.R2 = 88
	inputPayload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}
	copy(inputPayload[21:24], []byte{StreamFrameMagic0, StreamFrameMagic1, StreamFrameMagic2})

	if reason := inputStatePayloadCorruptionReason(inputPayload, false); reason != "" {
		t.Fatalf("short marker in legitimate motion data was rejected with microphone inactive: %s", reason)
	}
	if reason := inputStatePayloadCorruptionReason(inputPayload, true); reason == "" {
		t.Fatal("short marker should remain guarded while the microphone interface is active")
	}
}

func TestContainsMicTransportLeakPatternShiftedFragments(t *testing.T) {
	cases := [][]byte{
		{StreamFrameMagic2, StreamFrameMagic3, 0x01, 0x01, hidClassOUT},
		{StreamFrameMagic3, 0x01, 0x01, hidClassOUT},
		{0x01, 0x01, hidClassOUT},
		{StreamFrameMagic2, StreamFrameMagic1, 0x80, 0x87, StreamFrameMagic2},
		{StreamFrameMagic1, 0x80, 0x87, StreamFrameMagic2},
		{0x80, 0x87, StreamFrameMagic2},
	}

	for _, tc := range cases {
		if !containsMicTransportLeakPattern(tc) {
			t.Fatalf("expected shifted mic transport leak pattern to match: % x", tc)
		}
	}

	if containsMicTransportLeakPattern([]byte{0x80, 0x87, 0x42}) {
		t.Fatal("near miss should not match mic transport leak pattern")
	}
}

func TestReadDualSenseInputStreamDropsInvalidControlBits(t *testing.T) {
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
	state.LX = -32
	state.Buttons = validDualSenseInputButtons | 0x80000000
	state.DPad = validDualSenseInputDPad | 0x80
	inputPayload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}

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
	gotInput := dev.inputState
	dev.mtx.Unlock()

	neutral := NewInputState()
	if gotInput.LX != neutral.LX || gotInput.Buttons != neutral.Buttons || gotInput.DPad != neutral.DPad {
		t.Fatalf("invalid control bits should reset input: got LX=%d buttons=%#x dpad=%#x",
			gotInput.LX, gotInput.Buttons, gotInput.DPad)
	}
}

func TestDualSenseUpdateInputStateCopiesState(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	state := NewInputState()
	state.LX = 44
	state.Buttons = ButtonCross
	dev.UpdateInputState(state)

	state.LX = -91
	state.Buttons = 0xFFFFFFFF

	dev.mtx.Lock()
	gotInput := dev.inputState
	dev.mtx.Unlock()

	if gotInput.LX != 44 || gotInput.Buttons != ButtonCross {
		t.Fatalf("input state should be copied before publish: got LX=%d buttons=%#x",
			gotInput.LX, gotInput.Buttons)
	}
}

package dualsense

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"log/slog"
	"net"
	"slices"
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

func makeStreamFrameV2(frameType byte, sequence uint32, payload []byte) []byte {
	frame := make([]byte, StreamFrameV2HeaderSize+len(payload))
	frame[0] = StreamFrameMagic0
	frame[1] = StreamFrameMagic1
	frame[2] = StreamFrameMagic2
	frame[3] = StreamFrameMagic3
	frame[4] = StreamFrameVersionV2
	frame[5] = frameType
	binary.LittleEndian.PutUint16(frame[6:8], uint16(len(payload)))
	binary.LittleEndian.PutUint32(frame[8:12], sequence)
	copy(frame[StreamFrameV2HeaderSize:], payload)
	hash := crc32.NewIEEE()
	_, _ = hash.Write(frame[4:12])
	_, _ = hash.Write(payload)
	binary.LittleEndian.PutUint32(frame[12:16], hash.Sum32())
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

func TestReadDualSenseInputStreamV2AcceptsInterleavedStateAndMicrophoneFrames(t *testing.T) {
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
		errCh <- readDualSenseInputStreamVersion(server, dev, logger, true, StreamFrameVersionV2)
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

	if _, err := client.Write(makeStreamFrameV2(StreamFrameInputState, 0, inputPayload)); err != nil {
		t.Fatalf("write input frame: %v", err)
	}
	if _, err := client.Write(makeStreamFrameV2(StreamFrameMicrophonePCM, 1, microphonePayload)); err != nil {
		t.Fatalf("write microphone frame: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client pipe: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("readDualSenseInputStreamVersion returned error: %v", err)
	}

	dev.mtx.Lock()
	gotInput := dev.inputState
	gotMicrophone := append([]byte(nil), dev.microphonePCM...)
	dev.mtx.Unlock()

	if gotInput.LX != state.LX || gotInput.R2 != state.R2 {
		t.Fatalf("unexpected input state: LX=%d R2=%d", gotInput.LX, gotInput.R2)
	}
	if len(gotMicrophone) != len(microphonePayload) {
		t.Fatalf("unexpected microphone queue length: %d", len(gotMicrophone))
	}
	if !slices.Equal(gotMicrophone, microphonePayload) {
		t.Fatal("microphone payload changed in transport")
	}
}

func TestBaseDispatcherHonorsCreatedV2StreamProtocol(t *testing.T) {
	variant := &dshandler{
		combinedBluetoothFeedback: true,
		microphoneInput:           true,
		streamFrameVersion:        StreamFrameVersionV2,
	}
	dev, err := variant.CreateDevice(nil)
	if err != nil {
		t.Fatalf("CreateDevice returned error: %v", err)
	}

	server, client := net.Pipe()
	defer server.Close()

	errCh := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		// The runtime dispatcher infers the concrete Go type as plain
		// "dualsense", so exercise the base registration here as well.
		errCh <- (&dshandler{}).StreamHandler()(server, &dev, logger)
	}()

	state := NewInputState()
	state.LX = 42
	state.R2 = 99
	state.Buttons = ButtonCross
	payload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}
	if _, err := client.Write(makeStreamFrameV2(StreamFrameInputState, 0, payload)); err != nil {
		t.Fatalf("write input frame: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client pipe: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("base stream handler rejected V2 variant framing: %v", err)
	}

	dualSense := dev.(*DualSense)
	dualSense.mtx.Lock()
	got := dualSense.inputState
	dualSense.mtx.Unlock()
	if got.LX != state.LX || got.R2 != state.R2 || got.Buttons != state.Buttons {
		t.Fatalf("V2 frame was not preserved: got LX=%d R2=%d buttons=%#x",
			got.LX, got.R2, got.Buttons)
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

func TestQueueMicrophonePCMFrameKeepsNewestFourFrames(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)

	for value := byte(0); value < 5; value++ {
		frame := make([]byte, USBMicrophoneClientFrameSize)
		for i := range frame {
			frame[i] = value
		}
		dev.QueueMicrophonePCMFrame(frame)
	}

	dev.mtx.Lock()
	queued := append([]byte(nil), dev.microphonePCM...)
	dev.mtx.Unlock()

	if len(queued) != USBMicrophoneClientFrameSize*4 {
		t.Fatalf("unexpected bounded queue length: %d", len(queued))
	}
	if queued[0] != 1 || queued[len(queued)-1] != 4 {
		t.Fatalf("queue did not retain newest frames: first=%d last=%d", queued[0], queued[len(queued)-1])
	}
}

func TestReadDualSenseInputStreamV2PreservesArbitraryMotionBytes(t *testing.T) {
	patterns := map[string][]byte{
		"full stream magic": {StreamFrameMagic0, StreamFrameMagic1, StreamFrameMagic2, StreamFrameMagic3},
		"marker fragment":   {StreamFrameMagic1, StreamFrameMagic2, StreamFrameMagic3},
		"strong CM pattern": {StreamFrameMagic2, StreamFrameMagic3, 0x01, 0x01, hidClassOUT},
		"strong CP pattern": {StreamFrameMagic2, StreamFrameMagic1, 0x80, 0x87, StreamFrameMagic2},
		"weak CM pattern":   {0x01, 0x01, hidClassOUT},
		"weak CP pattern":   {0x80, 0x87, StreamFrameMagic2},
	}

	for name, pattern := range patterns {
		t.Run(name, func(t *testing.T) {
			dev, err := New(nil)
			if err != nil {
				t.Fatalf("New returned error: %v", err)
			}

			server, client := net.Pipe()
			defer server.Close()

			errCh := make(chan error, 1)
			go func() {
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				errCh <- readDualSenseInputStreamVersion(server, dev, logger, true, StreamFrameVersionV2)
			}()

			state := NewInputState()
			state.LX = 12
			state.R2 = 34
			state.Buttons = ButtonCross
			inputPayload, err := state.MarshalBinary()
			if err != nil {
				t.Fatalf("MarshalBinary returned error: %v", err)
			}
			copy(inputPayload[21:], pattern)

			if _, err := client.Write(makeStreamFrameV2(StreamFrameInputState, 0, inputPayload)); err != nil {
				t.Fatalf("write input frame: %v", err)
			}
			if err := client.Close(); err != nil {
				t.Fatalf("close client pipe: %v", err)
			}
			if err := <-errCh; err != nil {
				t.Fatalf("readDualSenseInputStreamVersion returned error: %v", err)
			}

			dev.mtx.Lock()
			gotInput := dev.inputState
			dev.mtx.Unlock()
			gotPayload, err := gotInput.MarshalBinary()
			if err != nil {
				t.Fatalf("MarshalBinary returned error: %v", err)
			}
			if !slices.Equal(gotPayload[21:21+len(pattern)], pattern) {
				t.Fatalf("valid motion bytes changed: got % x want % x", gotPayload[21:21+len(pattern)], pattern)
			}
			if gotInput.LX != state.LX || gotInput.R2 != state.R2 || gotInput.Buttons != state.Buttons {
				t.Fatalf("valid controls changed: got %+v want %+v", gotInput, *state)
			}
		})
	}
}

func TestReadDualSenseInputStreamV2RejectsBadCRC(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	server, client := net.Pipe()
	defer server.Close()

	errCh := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		errCh <- readDualSenseInputStreamVersion(server, dev, logger, true, StreamFrameVersionV2)
	}()

	state := NewInputState()
	state.LX = 12
	inputPayload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}

	frame := makeStreamFrameV2(StreamFrameInputState, 0, inputPayload)
	frame[len(frame)-1] ^= 0x80
	if _, err := client.Write(frame); err != nil {
		t.Fatalf("write input frame: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client pipe: %v", err)
	}

	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "CRC mismatch") {
		t.Fatalf("expected CRC mismatch, got %v", err)
	}
}

func TestReadDualSenseInputStreamV2RejectsSequenceGap(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	server, client := net.Pipe()
	defer server.Close()

	errCh := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		errCh <- readDualSenseInputStreamVersion(server, dev, logger, true, StreamFrameVersionV2)
	}()

	state := NewInputState()
	inputPayload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}
	if _, err := client.Write(makeStreamFrameV2(StreamFrameInputState, 10, inputPayload)); err != nil {
		t.Fatalf("write input frame: %v", err)
	}
	if _, err := client.Write(makeStreamFrameV2(StreamFrameInputState, 12, inputPayload)); err != nil {
		t.Fatalf("write second input frame: %v", err)
	}
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "sequence mismatch") {
		t.Fatalf("expected sequence mismatch, got %v", err)
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

	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "invalid controls") {
		t.Fatalf("expected invalid controls error, got %v", err)
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

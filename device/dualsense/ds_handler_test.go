package dualsense

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/Alia5/VIIPER/usbip"
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
	return makeStreamFrameWithCRC(StreamFrameVersionV2, frameType, sequence, payload)
}

func makeStreamFrameV3(frameType byte, sequence uint32, payload []byte) []byte {
	return makeStreamFrameWithCRC(StreamFrameVersionV3, frameType, sequence, payload)
}

func makeStreamFrameWithCRC(version, frameType byte, sequence uint32, payload []byte) []byte {
	frame := make([]byte, StreamFrameV2HeaderSize+len(payload))
	frame[0] = StreamFrameMagic0
	frame[1] = StreamFrameMagic1
	frame[2] = StreamFrameMagic2
	frame[3] = StreamFrameMagic3
	frame[4] = version
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
	for range microphoneTargetClientFrames {
		if _, err := client.Write(makeStreamFrame(t, StreamFrameMicrophonePCM, microphonePayload)); err != nil {
			t.Fatalf("write microphone frame: %v", err)
		}
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client pipe: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("readDualSenseInputStream returned error: %v", err)
	}

	dev.mtx.Lock()
	gotInput := dev.inputState
	gotMicrophoneState := dev.microphoneBuffer.State()
	dev.mtx.Unlock()

	if gotInput.LX != 42 || gotInput.R2 != 99 {
		t.Fatalf("unexpected input state: LX=%d R2=%d", gotInput.LX, gotInput.R2)
	}
	if gotMicrophoneState.QueuedBytes != USBMicrophoneClientFrameSize*microphoneTargetClientFrames ||
		!gotMicrophoneState.Primed {
		t.Fatalf("unexpected microphone queue state: %+v", gotMicrophoneState)
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
	if _, err := client.Write(makeStreamFrameV2(StreamFrameInputState, 2, inputPayload)); err != nil {
		t.Fatalf("write interleaved input frame: %v", err)
	}
	for frameIndex := 1; frameIndex < microphoneTargetClientFrames; frameIndex++ {
		sequence := uint32(frameIndex + 2)
		if _, err := client.Write(makeStreamFrameV2(StreamFrameMicrophonePCM, sequence, microphonePayload)); err != nil {
			t.Fatalf("write microphone frame %d: %v", frameIndex+1, err)
		}
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client pipe: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("readDualSenseInputStreamVersion returned error: %v", err)
	}

	dev.mtx.Lock()
	gotInput := dev.inputState
	gotMicrophoneState := dev.microphoneBuffer.State()
	dev.mtx.Unlock()

	if gotInput.LX != state.LX || gotInput.R2 != state.R2 {
		t.Fatalf("unexpected input state: LX=%d R2=%d", gotInput.LX, gotInput.R2)
	}
	if gotMicrophoneState.QueuedBytes != len(microphonePayload)*microphoneTargetClientFrames ||
		!gotMicrophoneState.Primed {
		t.Fatalf("unexpected microphone queue state: %+v", gotMicrophoneState)
	}
	gotMicrophone := dev.HandleTransfer(context.Background(),
		uint32(EndpointMicrophoneIn&0x0F), usbip.DirIn, nil)
	if !slices.Equal(gotMicrophone, microphonePayload[:USBMicrophonePacketSize]) {
		t.Fatal("microphone payload changed in transport")
	}
}

func TestReadDualSenseInputStreamV3RetainsInputAndMicrophoneFraming(t *testing.T) {
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
		errCh <- readDualSenseInputStreamVersion(server, dev, logger, true,
			StreamFrameVersionV3)
	}()

	state := NewInputState()
	state.LX = 21
	state.R2 = 87
	state.Buttons = ButtonTriangle
	inputPayload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}
	microphonePayload := make([]byte, USBMicrophoneClientFrameSize)
	for index := range microphonePayload {
		microphonePayload[index] = byte(index)
	}

	if _, err := client.Write(makeStreamFrameV3(StreamFrameInputState, 0,
		inputPayload)); err != nil {
		t.Fatalf("write V3 input frame: %v", err)
	}
	for frame := 0; frame < microphoneTargetClientFrames; frame++ {
		if _, err := client.Write(makeStreamFrameV3(StreamFrameMicrophonePCM,
			uint32(frame+1), microphonePayload)); err != nil {
			t.Fatalf("write V3 microphone frame %d: %v", frame, err)
		}
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client pipe: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("V3 input reader returned error: %v", err)
	}

	dev.mtx.Lock()
	gotInput := dev.inputState
	gotMicrophoneState := dev.microphoneBuffer.State()
	dev.mtx.Unlock()
	if gotInput.LX != state.LX || gotInput.R2 != state.R2 ||
		gotInput.Buttons != state.Buttons {
		t.Fatalf("V3 input state changed: got %+v want %+v", gotInput, state)
	}
	if gotMicrophoneState.QueuedBytes != USBMicrophoneClientFrameSize*
		microphoneTargetClientFrames || !gotMicrophoneState.Primed {
		t.Fatalf("unexpected V3 microphone state: %+v", gotMicrophoneState)
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

	gotMicrophoneLen := dev.GetDeviceSpecificArgs()["queuedMicrophoneBytes"].(int)
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
	if queued := dev.GetDeviceSpecificArgs()["queuedMicrophoneBytes"].(int); queued != 0 {
		t.Fatalf("inactive mic interface should drop PCM, got %d bytes", queued)
	}

	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)
	dev.QueueMicrophonePCMFrame(frame)
	if queued := dev.GetDeviceSpecificArgs()["queuedMicrophoneBytes"].(int); queued != USBMicrophoneClientFrameSize {
		t.Fatalf("active mic interface should queue PCM, got %d bytes", queued)
	}

	dev.SetInterfaceAltSetting(InterfaceMicrophone, 0)
	if queued := dev.GetDeviceSpecificArgs()["queuedMicrophoneBytes"].(int); queued != 0 {
		t.Fatalf("closing mic interface should drop queued PCM, got %d bytes", queued)
	}
}

func TestQueueMicrophonePCMFrameKeepsNewestMaximumFrames(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)

	for value := 0; value <= microphoneMaximumClientFrames; value++ {
		frame := make([]byte, USBMicrophoneClientFrameSize)
		for i := range frame {
			frame[i] = byte(value)
		}
		dev.QueueMicrophonePCMFrame(frame)
	}

	state := dev.GetDeviceSpecificArgs()
	if queued := state["queuedMicrophoneBytes"].(int); queued != USBMicrophoneClientFrameSize*microphoneMaximumClientFrames {
		t.Fatalf("unexpected bounded queue length: %d", queued)
	}
	if dropped := state["microphoneDroppedBytes"].(uint64); dropped != USBMicrophoneClientFrameSize {
		t.Fatalf("unexpected dropped byte count: %d", dropped)
	}
	packet := dev.HandleTransfer(context.Background(),
		uint32(EndpointMicrophoneIn&0x0F), usbip.DirIn, nil)
	if packet[0] != 1 || packet[len(packet)-1] != 1 {
		t.Fatalf("queue did not retain the newest maximum frames: % x", packet[:8])
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

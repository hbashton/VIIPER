package dualsense

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
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
	if err := writer.Stop(); err != nil {
		t.Fatal(err)
	}
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
	if err := writer.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestDualSenseV5WriterAlternatesControlAndMedia(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, nil, nil)
	for index := 0; index < 4; index++ {
		writer.EnqueueControl(StreamFrameOutputState, []byte{byte(index)})
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
	if err := writer.Stop(); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
}

func TestDualSenseV5WriterFaultsOnOrderedSaturationWithoutEviction(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, nil, nil)
	for marker := 0; marker < dualSenseOutputControlQueueCapacity; marker++ {
		writer.EnqueueControl(StreamFrameOutputState, []byte{byte(marker)})
	}
	writer.EnqueueControl(StreamFrameOutputState, []byte{0xFF})
	if len(writer.control) != dualSenseOutputControlQueueCapacity {
		t.Fatalf("control depth=%d want=%d", len(writer.control),
			dualSenseOutputControlQueueCapacity)
	}
	for index := 0; index < dualSenseOutputControlQueueCapacity; index++ {
		frame := <-writer.control
		decrementUint64(&writer.telemetry.orderedQueueDepth)
		want := byte(index)
		if len(frame.payload) != 1 || frame.payload[0] != want {
			t.Fatalf("control[%d]=% x want=%02x", index, frame.payload, want)
		}
	}
	state := writer.telemetry.snapshot()
	if state.OrderedReceived != uint64(dualSenseOutputControlQueueCapacity+1) ||
		state.OrderedEnqueued != uint64(dualSenseOutputControlQueueCapacity) ||
		state.OrderedRejected != 1 || state.OrderedSaturations != 1 ||
		state.Active || writer.accepting.Load() {
		t.Fatalf("unexpected saturation state: %+v", state)
	}
	buffer := make([]byte, 1)
	if count, err := client.Read(buffer); count != 0 || err == nil {
		t.Fatalf("saturation did not close owning stream: count=%d err=%v", count, err)
	}
	_ = client.Close()
}

func TestDualSenseV5WriterBoundsRealtimeMediaAndDropsOldest(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, nil, nil)
	for marker := 0; marker < dualSenseRealtimeMediaQueueCapacity; marker++ {
		writer.EnqueueRealtimeHaptics([]byte{byte(marker)})
	}
	writer.EnqueueRealtimeHaptics([]byte{0xFF})
	if len(writer.audio) != dualSenseRealtimeMediaQueueCapacity {
		t.Fatalf("media depth=%d want=%d", len(writer.audio),
			dualSenseRealtimeMediaQueueCapacity)
	}
	for index := 0; index < dualSenseRealtimeMediaQueueCapacity; index++ {
		frame := <-writer.audio
		writer.recordMediaDequeued(frame)
		want := byte(index + 1)
		if index == dualSenseRealtimeMediaQueueCapacity-1 {
			want = 0xFF
		}
		if len(frame.payload) != 1 || frame.payload[0] != want {
			t.Fatalf("realtime[%d]=% x want=%02x", index, frame.payload, want)
		}
	}
	state := writer.telemetry.snapshot()
	if state.Overruns != 1 || state.DroppedPayloads != 1 ||
		state.DroppedBytes != 1 ||
		state.QueueHighWater != uint64(dualSenseRealtimeMediaQueueCapacity) ||
		state.QueueDurationHighUS >
			dualSenseMediaMaximumBufferTime.Microseconds() {
		t.Fatalf("unexpected media overrun telemetry: %+v", state)
	}
}

func TestDualSenseV5WriterShutdownReturnsEveryMediaBuffer(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, nil, nil)
	for index := 0; index < 10; index++ {
		feedback, speaker := testV5Media(byte(index))
		writer.EnqueueAtomicAudioHaptics(feedback, speaker)
	}
	go writer.Run()
	if err := writer.Stop(); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()

	state := writer.telemetry.snapshot()
	if state.Active || state.QueueDepth != 0 || len(writer.audio) != 0 ||
		len(writer.audioFree) != dualSenseOutputAudioPoolCapacity {
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
	writer.telemetry.queueDepth.Add(1)
	writer.audio <- dualSenseOutputFrame{
		frameType: StreamFrameAtomicAudioHaptics,
		payload:   buffer[:4], media: true, audio: true, mediaBytes: 4,
	}
	_ = client.Close()
	writer.enqueueLock.RUnlock()

	select {
	case <-writer.done:
	case <-time.After(time.Second):
		t.Fatal("writer did not finish after socket failure")
	}
	if len(writer.audio) != 0 ||
		len(writer.audioFree) != dualSenseOutputAudioPoolCapacity {
		t.Fatalf("shutdown retained a pooled buffer: queued=%d free=%d",
			len(writer.audio), len(writer.audioFree))
	}
	state := writer.telemetry.snapshot()
	if state.OrderedWriteFailures != 1 || state.OrderedWritten != 0 {
		t.Fatalf("ordered write failure was not accounted: %+v", state)
	}
}

func TestDualSenseV5WriterAccountsMediaWriteFailure(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, nil, nil)
	feedback, speaker := testV5Media(0x61)
	writer.EnqueueAtomicAudioHaptics(feedback, speaker)
	_ = client.Close()
	go writer.Run()
	select {
	case <-writer.done:
	case <-time.After(time.Second):
		t.Fatal("media write failure did not stop writer")
	}
	state := writer.telemetry.snapshot()
	if state.WriteFailures != 1 || state.WrittenPayloads != 0 || state.Active {
		t.Fatalf("media write failure was not accounted: %+v", state)
	}
	if len(writer.audioFree) != dualSenseOutputAudioPoolCapacity {
		t.Fatalf("media write failure leaked pool: free=%d", len(writer.audioFree))
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
	if err := writer.Stop(); err != nil {
		t.Fatal(err)
	}
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
		len(writer.audioFree) != dualSenseOutputAudioPoolCapacity {
		t.Fatal("failed stream retained V5 transport state")
	}
	buffer := make([]byte, StreamFrameHeaderSize+4)
	if count, err := client.Read(buffer); count != 0 || err == nil {
		t.Fatalf("failed stream replayed stale media: bytes=%d err=%v", count, err)
	}
	if err := writer.Stop(); err != nil {
		t.Fatal(err)
	}
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
		controller.mtx.Lock()
		callbacksCleared := controller.outputFunc == nil &&
			controller.atomicAudioHapticsFunc == nil &&
			controller.speakerResetFunc == nil
		controller.mtx.Unlock()
		if !callbacksCleared {
			t.Fatalf("%s retained callbacks after shutdown", name)
		}
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

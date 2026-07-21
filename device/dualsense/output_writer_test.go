package dualsense

import (
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

func TestCopyDualSenseSpeakerChannelsSelectsFrontPairWithoutAllocation(t *testing.T) {
	const frames = 480
	source := make([]byte, frames*USBHapticsAudioFrameSize)
	for frame := 0; frame < frames; frame++ {
		offset := frame * USBHapticsAudioFrameSize
		binary.LittleEndian.PutUint16(source[offset:offset+2], uint16(int16(frame)))
		binary.LittleEndian.PutUint16(source[offset+2:offset+4], uint16(int16(-frame)))
		binary.LittleEndian.PutUint16(source[offset+4:offset+6], uint16(int16(1000+frame)))
		binary.LittleEndian.PutUint16(source[offset+6:offset+8], uint16(int16(-1000-frame)))
	}
	destination := make([]byte, frames*2*USBHapticsAudioBytesPerSample)

	if allocations := testing.AllocsPerRun(1000, func() {
		copyDualSenseSpeakerChannels(destination, source)
	}); allocations != 0 {
		t.Fatalf("speaker channel extraction allocated %.2f objects per run", allocations)
	}
	writer := newDualSenseOutputWriter(nil, StreamFrameVersionV3, nil, nil)
	if allocations := testing.AllocsPerRun(1000, func() {
		writer.EnqueueSpeakerFromUSB(source)
		frame := <-writer.audio
		writer.release(frame)
	}); allocations != 0 {
		t.Fatalf("speaker enqueue path allocated %.2f objects per run", allocations)
	}

	written := copyDualSenseSpeakerChannels(destination, source)
	if written != len(destination) {
		t.Fatalf("unexpected speaker byte count: got %d want %d", written, len(destination))
	}
	for frame := 0; frame < frames; frame++ {
		offset := frame * 4
		left := int16(binary.LittleEndian.Uint16(destination[offset : offset+2]))
		right := int16(binary.LittleEndian.Uint16(destination[offset+2 : offset+4]))
		if left != int16(frame) || right != int16(-frame) {
			t.Fatalf("speaker frame %d changed: left=%d right=%d", frame, left, right)
		}
	}
}

func TestDualSenseV3WriterFramesNativeSpeakerPairAndFeedback(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, StreamFrameVersionV3, nil, nil)
	go writer.Run()

	usbPCM := make([]byte, 2*USBHapticsAudioFrameSize)
	copy(usbPCM[0:8], []byte{1, 2, 3, 4, 0xA1, 0xA2, 0xA3, 0xA4})
	copy(usbPCM[8:16], []byte{5, 6, 7, 8, 0xB1, 0xB2, 0xB3, 0xB4})
	writer.EnqueueSpeakerFromUSB(usbPCM)

	header, payload := readDualSenseOutputFrame(t, client)
	if header[4] != StreamFrameVersionV3 || header[5] != StreamFrameSpeakerPCM {
		t.Fatalf("unexpected speaker frame header: % x", header)
	}
	if sequence := binary.LittleEndian.Uint32(header[8:12]); sequence != 0 {
		t.Fatalf("unexpected speaker frame sequence: %d", sequence)
	}
	wantSpeaker := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if string(payload) != string(wantSpeaker) {
		t.Fatalf("speaker payload retained haptics channels: got % x want % x", payload, wantSpeaker)
	}
	if got, want := binary.LittleEndian.Uint32(header[12:16]),
		framedStreamCRC(header[4:12], payload); got != want {
		t.Fatalf("speaker frame CRC mismatch: got %08X want %08X", got, want)
	}

	feedback := []byte{0x11, 0x22, 0x33}
	writer.EnqueueControl(StreamFrameOutputState, feedback)
	header, payload = readDualSenseOutputFrame(t, client)
	if header[5] != StreamFrameOutputState || string(payload) != string(feedback) {
		t.Fatalf("unexpected feedback frame: header=% x payload=% x", header, payload)
	}
	if sequence := binary.LittleEndian.Uint32(header[8:12]); sequence != 1 {
		t.Fatalf("feedback did not share the speaker sequence: %d", sequence)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("close client pipe: %v", err)
	}
	writer.Stop()
	state := writer.telemetry.snapshot()
	if state.ReceivedPayloads != 1 || state.ReceivedBytes != 8 ||
		state.EnqueuedPayloads != 1 || state.EnqueuedBytes != 8 ||
		state.WrittenPayloads != 1 || state.WrittenBytes != 8 ||
		state.DroppedPayloads != 0 || state.WriteFailures != 0 {
		t.Fatalf("unexpected V3 speaker telemetry: %+v", state)
	}
	if state.Active || state.QueueDepth != 0 ||
		len(writer.audioFree) != dualSenseOutputAudioQueueCapacity {
		t.Fatalf("writer did not release its speaker pool: state=%+v free=%d",
			state, len(writer.audioFree))
	}
}

func TestDualSenseV4WriterKeepsFeedbackAndSpeakerGenerationAtomic(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, StreamFrameVersionV4, nil, nil)
	go writer.Run()

	feedback := make([]byte, 474)
	feedback[76] = 0x36
	feedback[154] = 0x41
	speaker := make([]byte, 512*2*USBHapticsAudioBytesPerSample)
	for index := range speaker {
		speaker[index] = byte(index)
	}
	writer.EnqueueAtomicAudioHaptics(feedback, speaker)

	header, payload := readDualSenseOutputFrame(t, client)
	if header[4] != StreamFrameVersionV4 ||
		header[5] != StreamFrameAtomicAudioHaptics {
		t.Fatalf("unexpected atomic frame header: % x", header)
	}
	feedbackLength := int(binary.LittleEndian.Uint16(payload[:2]))
	if feedbackLength != len(feedback) {
		t.Fatalf("unexpected atomic feedback length: got %d want %d",
			feedbackLength, len(feedback))
	}
	if got := payload[2 : 2+feedbackLength]; string(got) != string(feedback) {
		t.Fatal("atomic frame changed native feedback")
	}
	if got := payload[2+feedbackLength:]; string(got) != string(speaker) {
		t.Fatal("atomic frame changed matching speaker PCM")
	}

	_ = client.Close()
	writer.Stop()
	state := writer.telemetry.snapshot()
	if state.ReceivedPayloads != 1 || state.ReceivedBytes != uint64(len(speaker)) ||
		state.EnqueuedPayloads != 1 || state.WrittenPayloads != 1 ||
		state.DroppedPayloads != 0 {
		t.Fatalf("unexpected V4 atomic telemetry: %+v", state)
	}
}

func TestDualSenseV3WriterShutdownReturnsEveryQueuedSpeakerBuffer(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, StreamFrameVersionV3, nil, nil)
	go writer.Run()

	usbPCM := make([]byte, 480*USBHapticsAudioFrameSize)
	for packet := 0; packet < 10; packet++ {
		writer.EnqueueSpeakerFromUSB(usbPCM)
	}
	writer.Stop()
	_ = client.Close()

	state := writer.telemetry.snapshot()
	if state.Active || state.QueueDepth != 0 || len(writer.audio) != 0 ||
		len(writer.audioFree) != dualSenseOutputAudioQueueCapacity {
		t.Fatalf("shutdown retained speaker buffers: state=%+v queued=%d free=%d",
			state, len(writer.audio), len(writer.audioFree))
	}
	if state.ReceivedPayloads != 10 || state.EnqueuedPayloads != 10 ||
		state.DroppedPayloads != 0 {
		t.Fatalf("unexpected shutdown telemetry: %+v", state)
	}
}

func TestDualSenseV3WriterAlternatesFeedbackAndSpeakerUnderPressure(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, StreamFrameVersionV3, nil, nil)
	usbPCM := make([]byte, USBHapticsAudioFrameSize)
	for frame := 0; frame < 4; frame++ {
		writer.EnqueueControl(StreamFrameOutputState, []byte{byte(frame)})
		writer.EnqueueSpeakerFromUSB(usbPCM)
	}
	go writer.Run()

	for frame := 0; frame < 8; frame++ {
		header, _ := readDualSenseOutputFrame(t, client)
		wantType := byte(StreamFrameOutputState)
		if frame%2 != 0 {
			wantType = StreamFrameSpeakerPCM
		}
		if header[5] != wantType {
			t.Fatalf("frame %d starved one output lane: got type 0x%02X want 0x%02X",
				frame, header[5], wantType)
		}
	}
	writer.Stop()
	_ = client.Close()
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
		Conn:    conn,
		started: make(chan struct{}),
		closed:  make(chan struct{}),
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

func (c *deadlineTrackingConn) writeDeadlines() []time.Time {
	c.deadlineLock.Lock()
	defer c.deadlineLock.Unlock()
	return append([]time.Time(nil), c.deadlines...)
}

func TestDualSenseV3WriterWriteFailureCannotRaceFinalDrain(t *testing.T) {
	server, client := net.Pipe()
	conn := &writeStartedConn{Conn: server, started: make(chan struct{})}
	writer := newDualSenseOutputWriter(conn, StreamFrameVersionV3, nil, nil)
	writer.EnqueueControl(StreamFrameOutputState, []byte{0x01})
	go writer.Run()
	<-conn.started

	// Model a producer that has already entered the enqueue critical section
	// when the socket fails. Run must wait for it, stop all later producers,
	// and only then perform its final drain.
	writer.enqueueLock.RLock()
	buffer := <-writer.audioFree
	buffer[0] = 0x55
	writer.audio <- dualSenseOutputFrame{
		frameType: StreamFrameSpeakerPCM,
		payload:   buffer[:4],
		audio:     true,
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close failed client: %v", err)
	}
	writer.enqueueLock.RUnlock()

	select {
	case <-writer.done:
	case <-time.After(time.Second):
		t.Fatal("writer did not finish after socket failure")
	}
	if len(writer.audio) != 0 ||
		len(writer.audioFree) != dualSenseOutputAudioQueueCapacity {
		t.Fatalf("write-failure shutdown retained a pooled buffer: queued=%d free=%d",
			len(writer.audio), len(writer.audioFree))
	}
	before := writer.telemetry.snapshot()
	writer.EnqueueSpeakerFromUSB(make([]byte, USBHapticsAudioFrameSize))
	after := writer.telemetry.snapshot()
	if after.ReceivedPayloads != before.ReceivedPayloads ||
		after.DroppedPayloads != before.DroppedPayloads {
		t.Fatalf("stopped writer accepted a stale callback: before=%+v after=%+v",
			before, after)
	}
}

func TestDualSenseV3WriterResetIsHardWriteGenerationBarrier(t *testing.T) {
	server, client := net.Pipe()
	conn := newDeadlineTrackingConn(server)
	writer := newDualSenseOutputWriter(conn, StreamFrameVersionV3, nil, nil)

	oldPCM := make([]byte, USBHapticsAudioFrameSize)
	oldPCM[0] = 0x11
	writer.EnqueueSpeakerFromUSB(oldPCM)
	go writer.Run()
	<-conn.started
	queuedOldPCM := make([]byte, USBHapticsAudioFrameSize)
	queuedOldPCM[0] = 0x12
	writer.EnqueueSpeakerFromUSB(queuedOldPCM)

	resetDone := make(chan struct{})
	go func() {
		writer.ResetSpeaker()
		close(resetDone)
	}()
	select {
	case <-resetDone:
		t.Fatal("speaker reset crossed an in-flight old-generation write")
	case <-time.After(20 * time.Millisecond):
	}

	_, oldPayload := readDualSenseOutputFrame(t, client)
	if len(oldPayload) != 4 || oldPayload[0] != 0x11 {
		t.Fatalf("unexpected in-flight old-generation payload: % x", oldPayload)
	}
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("speaker reset did not finish after the in-flight write")
	}
	deadlines := conn.writeDeadlines()
	if len(deadlines) != 2 || deadlines[0].IsZero() || !deadlines[1].IsZero() {
		t.Fatalf("viable reset did not arm and clear its write deadline: %v", deadlines)
	}
	if len(writer.audio) != 0 {
		t.Fatalf("speaker reset retained %d queued old-generation frames", len(writer.audio))
	}

	newPCM := make([]byte, USBHapticsAudioFrameSize)
	newPCM[0] = 0x22
	writer.EnqueueSpeakerFromUSB(newPCM)
	_, newPayload := readDualSenseOutputFrame(t, client)
	if len(newPayload) != 4 || newPayload[0] != 0x22 {
		t.Fatalf("unexpected post-reset payload: % x", newPayload)
	}

	writer.Stop()
	_ = client.Close()
}

func TestDualSenseV3WriterResetBoundsBlockedWriteAndClosesStream(t *testing.T) {
	server, client := net.Pipe()
	conn := newDeadlineTrackingConn(server)
	writer := newDualSenseOutputWriter(conn, StreamFrameVersionV3, nil, nil)

	inFlightPCM := make([]byte, USBHapticsAudioFrameSize)
	inFlightPCM[0] = 0x31
	writer.EnqueueSpeakerFromUSB(inFlightPCM)
	go writer.Run()
	<-conn.started

	queuedPCM := make([]byte, USBHapticsAudioFrameSize)
	queuedPCM[0] = 0x32
	writer.EnqueueSpeakerFromUSB(queuedPCM)

	resetDone := make(chan struct{})
	go func() {
		writer.ResetSpeaker()
		close(resetDone)
	}()
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("speaker reset remained blocked after its write deadline")
	}
	select {
	case <-writer.done:
	case <-time.After(time.Second):
		t.Fatal("timed-out speaker write did not stop the failed stream")
	}
	select {
	case <-conn.closed:
	default:
		t.Fatal("timed-out speaker write did not close the failed stream")
	}

	deadlines := conn.writeDeadlines()
	if len(deadlines) != 1 || deadlines[0].IsZero() {
		t.Fatalf("failed stream deadline was cleared or not armed: %v", deadlines)
	}
	if writer.streamViable.Load() {
		t.Fatal("timed-out speaker stream remained marked viable")
	}
	if writer.audioGeneration.Load() != 1 || len(writer.audio) != 0 ||
		len(writer.audioFree) != dualSenseOutputAudioQueueCapacity {
		t.Fatalf("timed-out reset retained stale audio: generation=%d queued=%d free=%d",
			writer.audioGeneration.Load(), len(writer.audio), len(writer.audioFree))
	}

	buffer := make([]byte, StreamFrameV2HeaderSize+4)
	if count, err := client.Read(buffer); count != 0 || err == nil {
		t.Fatalf("failed stream replayed stale audio: bytes=%d error=%v payload=% x",
			count, err, buffer[:count])
	}

	before := writer.telemetry.snapshot()
	writer.EnqueueSpeakerFromUSB(make([]byte, USBHapticsAudioFrameSize))
	after := writer.telemetry.snapshot()
	if after.ReceivedPayloads != before.ReceivedPayloads || len(writer.audio) != 0 {
		t.Fatalf("failed writer accepted audio after reconnect was required: before=%+v after=%+v queued=%d",
			before, after, len(writer.audio))
	}

	writer.Stop()
	_ = client.Close()
}

func TestDualSenseAndEdgeV3VariantsForwardSpeakerAndAcceptInput(t *testing.T) {
	for _, edge := range []bool{false, true} {
		name := "DualSense"
		if edge {
			name = "DualSense Edge"
		}
		t.Run(name, func(t *testing.T) {
			var dev usb.Device
			var err error
			var streamHandler func(net.Conn, *usb.Device, *slog.Logger) error
			if edge {
				variant := &dsedgehandler{
					combinedBluetoothFeedback: true,
					microphoneInput:           true,
					speakerOutput:             true,
					streamFrameVersion:        StreamFrameVersionV3,
				}
				dev, err = variant.CreateDevice(nil)
				streamHandler = (&dsedgehandler{}).StreamHandler()
			} else {
				variant := &dshandler{
					combinedBluetoothFeedback: true,
					microphoneInput:           true,
					speakerOutput:             true,
					streamFrameVersion:        StreamFrameVersionV3,
				}
				dev, err = variant.CreateDevice(nil)
				streamHandler = (&dshandler{}).StreamHandler()
			}
			if err != nil {
				t.Fatalf("CreateDevice returned error: %v", err)
			}

			server, client := net.Pipe()
			errCh := make(chan error, 1)
			go func() {
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				errCh <- streamHandler(server, &dev, logger)
			}()

			state := NewInputState()
			state.LX = 73
			state.Buttons = ButtonCross
			inputPayload, err := state.MarshalBinary()
			if err != nil {
				t.Fatalf("MarshalBinary returned error: %v", err)
			}
			if _, err := client.Write(makeStreamFrameV3(StreamFrameInputState,
				0, inputPayload)); err != nil {
				t.Fatalf("write V3 input state: %v", err)
			}

			dualSense := dev.(*DualSense)
			dualSense.SetInterfaceAltSetting(InterfaceHapticsAudio, 1)
			usbPCM := []byte{
				0x01, 0x02, 0x03, 0x04, 0xA1, 0xA2, 0xA3, 0xA4,
				0x05, 0x06, 0x07, 0x08, 0xB1, 0xB2, 0xB3, 0xB4,
			}
			dualSense.HandleTransfer(context.Background(),
				EndpointHapticsAudioOut, usbip.DirOut, usbPCM)

			header, payload := readDualSenseOutputFrame(t, client)
			if header[4] != StreamFrameVersionV3 ||
				header[5] != StreamFrameSpeakerPCM {
				t.Fatalf("unexpected V3 speaker frame: % x", header)
			}
			want := []byte{0x01, 0x02, 0x03, 0x04,
				0x05, 0x06, 0x07, 0x08}
			if string(payload) != string(want) {
				t.Fatalf("unexpected native speaker pair: got % x want % x",
					payload, want)
			}

			if err := client.Close(); err != nil {
				t.Fatalf("close client pipe: %v", err)
			}
			if err := <-errCh; err != nil {
				t.Fatalf("V3 stream handler returned error: %v", err)
			}

			dualSense.mtx.Lock()
			gotInput := dualSense.inputState
			callbacksCleared := dualSense.outputFunc == nil &&
				dualSense.speakerFunc == nil &&
				dualSense.speakerResetFunc == nil
			dualSense.mtx.Unlock()
			if gotInput.LX != state.LX || gotInput.Buttons != state.Buttons {
				t.Fatalf("V3 input changed: got %+v want %+v", gotInput, state)
			}
			if !callbacksCleared {
				t.Fatal("V3 handler retained a callback after shutdown")
			}
			speakerState := dualSense.GetDeviceSpecificArgs()
			if speakerState["speakerStreamActive"].(bool) ||
				speakerState["speakerPayloadsReceived"].(uint64) != 1 ||
				speakerState["speakerPayloadsDropped"].(uint64) != 0 ||
				speakerState["speakerPayloadsWritten"].(uint64) != 1 {
				t.Fatalf("unexpected exposed speaker state: %+v", speakerState)
			}
		})
	}
}

func TestDualSenseV4HandlerPairsTheSameUSBGeneration(t *testing.T) {
	variant := &dshandler{
		combinedBluetoothFeedback: true,
		microphoneInput:           true,
		speakerOutput:             true,
		streamFrameVersion:        StreamFrameVersionV4,
	}
	dev, err := variant.CreateDevice(nil)
	if err != nil {
		t.Fatalf("CreateDevice returned error: %v", err)
	}

	server, client := net.Pipe()
	errCh := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		errCh <- variant.StreamHandler()(server, &dev, logger)
	}()

	state := NewInputState()
	inputPayload, err := state.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary returned error: %v", err)
	}
	if _, err := client.Write(makeStreamFrameWithCRC(StreamFrameVersionV4,
		StreamFrameInputState, 0, inputPayload)); err != nil {
		t.Fatalf("write V4 input state: %v", err)
	}

	dualSense := dev.(*DualSense)
	dualSense.SetInterfaceAltSetting(InterfaceHapticsAudio, 1)
	usbPCM := make([]byte, 512*USBHapticsAudioFrameSize)
	negativeRearSample := int16(-12000)
	for frame := 0; frame < 512; frame++ {
		offset := frame * USBHapticsAudioFrameSize
		binary.LittleEndian.PutUint16(usbPCM[offset:offset+2],
			uint16(int16(frame+1)))
		binary.LittleEndian.PutUint16(usbPCM[offset+2:offset+4],
			uint16(int16(-frame-1)))
		binary.LittleEndian.PutUint16(usbPCM[offset+4:offset+6],
			uint16(int16(12000)))
		binary.LittleEndian.PutUint16(usbPCM[offset+6:offset+8],
			uint16(negativeRearSample))
	}
	dualSense.HandleTransfer(context.Background(),
		EndpointHapticsAudioOut, usbip.DirOut, usbPCM)

	header, payload := readDualSenseOutputFrame(t, client)
	if header[4] != StreamFrameVersionV4 ||
		header[5] != StreamFrameAtomicAudioHaptics {
		t.Fatalf("unexpected V4 atomic frame: % x", header)
	}
	feedbackLength := int(binary.LittleEndian.Uint16(payload[:2]))
	feedback := payload[2 : 2+feedbackLength]
	speaker := payload[2+feedbackLength:]
	if feedbackLength != 474 || len(speaker) != 512*4 {
		t.Fatalf("unexpected V4 generation sizes: feedback=%d speaker=%d",
			feedbackLength, len(speaker))
	}
	if feedback[76] != 0x36 || feedback[152] != 0x92 ||
		feedback[153] != BluetoothHapticsSampleSize {
		t.Fatalf("atomic feedback omitted combined haptics: % x",
			feedback[76:154])
	}
	for frame := 0; frame < 512; frame++ {
		offset := frame * 4
		left := int16(binary.LittleEndian.Uint16(speaker[offset : offset+2]))
		right := int16(binary.LittleEndian.Uint16(speaker[offset+2 : offset+4]))
		if left != int16(frame+1) || right != int16(-frame-1) {
			t.Fatalf("speaker generation diverged at frame %d: %d/%d",
				frame, left, right)
		}
	}

	_ = client.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("V4 stream handler returned error: %v", err)
	}
}

func TestDualSenseV3ReplacementOwnsTelemetryAndClearsCallbacks(t *testing.T) {
	variant := &dshandler{
		combinedBluetoothFeedback: true,
		microphoneInput:           true,
		speakerOutput:             true,
		streamFrameVersion:        StreamFrameVersionV3,
	}
	dev, err := variant.CreateDevice(nil)
	if err != nil {
		t.Fatalf("CreateDevice returned error: %v", err)
	}
	dualSense := dev.(*DualSense)
	dualSense.SetInterfaceAltSetting(InterfaceHapticsAudio, 1)
	streamHandler := (&dshandler{}).StreamHandler()

	runStream := func(speakerPayloads int) *dualSenseSpeakerStreamTelemetry {
		server, client := net.Pipe()
		errCh := make(chan error, 1)
		go func() {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			errCh <- streamHandler(server, &dev, logger)
		}()

		inputPayload, err := NewInputState().MarshalBinary()
		if err != nil {
			t.Fatalf("MarshalBinary returned error: %v", err)
		}
		if _, err := client.Write(makeStreamFrameV3(StreamFrameInputState,
			0, inputPayload)); err != nil {
			t.Fatalf("write V3 input state: %v", err)
		}

		usbPCM := make([]byte, USBHapticsAudioFrameSize)
		for payload := 0; payload < speakerPayloads; payload++ {
			usbPCM[0] = byte(payload + 1)
			dualSense.HandleTransfer(context.Background(),
				EndpointHapticsAudioOut, usbip.DirOut, usbPCM)
			header, got := readDualSenseOutputFrame(t, client)
			if header[5] != StreamFrameSpeakerPCM || len(got) != 4 ||
				got[0] != usbPCM[0] {
				t.Fatalf("unexpected replacement speaker frame: header=% x payload=% x",
					header, got)
			}
		}

		if err := client.Close(); err != nil {
			t.Fatalf("close client pipe: %v", err)
		}
		if err := <-errCh; err != nil {
			t.Fatalf("V3 stream handler returned error: %v", err)
		}

		dualSense.mtx.Lock()
		telemetry := dualSense.speakerStreamTelemetry
		callbacksCleared := dualSense.outputFunc == nil &&
			dualSense.speakerFunc == nil &&
			dualSense.speakerResetFunc == nil
		dualSense.mtx.Unlock()
		if !callbacksCleared {
			t.Fatal("finished V3 stream retained a callback")
		}
		return telemetry
	}

	first := runStream(1)
	second := runStream(2)
	if first == second {
		t.Fatal("replacement V3 stream reused the previous telemetry generation")
	}
	first.receivedPayloads.Add(100)
	state := dualSense.GetDeviceSpecificArgs()
	if state["speakerStreamActive"].(bool) ||
		state["speakerPayloadsReceived"].(uint64) != 2 ||
		state["speakerPayloadsEnqueued"].(uint64) != 2 ||
		state["speakerPayloadsWritten"].(uint64) != 2 ||
		state["speakerPayloadsDropped"].(uint64) != 0 {
		t.Fatalf("stale V3 generation changed replacement telemetry: %+v", state)
	}
}

func TestDualSenseOutputWriterResetFlushesSpeakerGeneration(t *testing.T) {
	telemetry := &dualSenseSpeakerStreamTelemetry{}
	writer := newDualSenseOutputWriter(nil, StreamFrameVersionV3, telemetry, nil)
	usbPCM := make([]byte, USBHapticsAudioFrameSize)
	usbPCM[0] = 0x55

	writer.EnqueueSpeakerFromUSB(usbPCM)
	if len(writer.audio) != 1 {
		t.Fatal("speaker reset precondition did not queue audio")
	}
	writer.ResetSpeaker()
	if len(writer.audio) != 0 || len(writer.audioFree) != dualSenseOutputAudioQueueCapacity {
		t.Fatalf("speaker reset retained pooled audio: queued=%d free=%d",
			len(writer.audio), len(writer.audioFree))
	}
	if writer.audioGeneration.Load() != 1 || telemetry.droppedPayloads.Load() != 0 {
		t.Fatalf("unexpected reset generation/telemetry: generation=%d drops=%d",
			writer.audioGeneration.Load(), telemetry.droppedPayloads.Load())
	}

	writer.EnqueueSpeakerFromUSB(usbPCM)
	frame := <-writer.audio
	if frame.generation != 1 || len(frame.payload) != 4 || frame.payload[0] != 0x55 {
		t.Fatalf("post-reset frame used stale generation or payload: generation=%d payload=% x",
			frame.generation, frame.payload)
	}
	writer.release(frame)
}

func readDualSenseOutputFrame(t *testing.T, reader io.Reader) ([]byte, []byte) {
	t.Helper()
	header := make([]byte, StreamFrameV2HeaderSize)
	if _, err := io.ReadFull(reader, header); err != nil {
		t.Fatalf("read output frame header: %v", err)
	}
	payload := make([]byte, int(binary.LittleEndian.Uint16(header[6:8])))
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatalf("read output frame payload: %v", err)
	}
	return header, payload
}

package dualsense

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

func TestDualSenseOrderedPublicationIsFIFOWithConcurrentProducers(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, nil, nil)
	const producers = 24
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(producers)
	for marker := 0; marker < producers; marker++ {
		marker := byte(marker)
		go func() {
			defer wait.Done()
			<-start
			writer.EnqueueControl(StreamFrameOutputState, []byte{marker})
		}()
	}
	close(start)
	wait.Wait()

	seen := make(map[byte]bool, producers)
	for publication := uint64(1); publication <= producers; publication++ {
		frame := <-writer.control
		decrementUint64(&writer.telemetry.orderedQueueDepth)
		if frame.publication != publication {
			t.Fatalf("publication=%d want=%d", frame.publication, publication)
		}
		if len(frame.payload) != 1 || seen[frame.payload[0]] {
			t.Fatalf("invalid or duplicate payload: % x", frame.payload)
		}
		seen[frame.payload[0]] = true
	}
	state := writer.telemetry.snapshot()
	if state.OrderedReceived != producers || state.OrderedEnqueued != producers ||
		state.OrderedRejected != 0 || state.OrderedSaturations != 0 {
		t.Fatalf("unexpected concurrent publication state: %+v", state)
	}
}

func TestDualSenseMixedMediaWindowUsesExactDurations(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, nil, nil)
	feedback, speaker := testV5Media(0x31)
	for marker := 0; marker < dualSenseOutputAudioQueueCapacity; marker++ {
		if marker%2 == 0 {
			writer.EnqueueAtomicAudioHaptics(feedback, speaker)
		} else {
			writer.EnqueueRealtimeHaptics([]byte{byte(marker)})
		}
	}
	state := writer.telemetry.snapshot()
	if state.QueueDurationUS > dualSenseMediaMaximumBufferTime.Microseconds() ||
		state.QueueDurationHighUS > dualSenseMediaMaximumBufferTime.Microseconds() {
		t.Fatalf("media time bound exceeded: %+v", state)
	}
	if state.Overruns == 0 {
		t.Fatal("mixed 10 ms/10.667 ms media did not evict the oldest frame")
	}
	var lastType byte
	for len(writer.audio) != 0 {
		frame := <-writer.audio
		writer.recordMediaDequeued(frame)
		// Input alternated, so retained FIFO order must continue alternating.
		if lastType != 0 && frame.frameType == lastType {
			t.Fatalf("mixed media FIFO reordered frame type 0x%02x", frame.frameType)
		}
		lastType = frame.frameType
		writer.release(frame)
	}
	if writer.telemetry.queueDurationNS.Load() != 0 {
		t.Fatalf("media duration accounting leaked %d ns",
			writer.telemetry.queueDurationNS.Load())
	}
}

func TestDualSenseResetCountsBothMediaClocksAsStale(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, nil, nil)
	feedback, speaker := testV5Media(0x44)
	writer.EnqueueAtomicAudioHaptics(feedback, speaker)
	writer.EnqueueRealtimeHaptics([]byte{1, 2, 3})
	writer.ResetSpeaker()
	state := writer.telemetry.snapshot()
	if state.StalePayloads != 2 ||
		state.StaleBytes != dualSenseV5SpeakerPayloadSize+3 ||
		state.QueueDepth != 0 || state.QueueDurationUS != 0 ||
		len(writer.audio) != 0 {
		t.Fatalf("reset did not retire both media clocks: %+v", state)
	}
	if len(writer.audioFree) != dualSenseOutputAudioPoolCapacity {
		t.Fatalf("reset leaked media pool: free=%d", len(writer.audioFree))
	}
}

func TestDualSenseWriterRecordsOnlyObservedGenerationGap(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, nil, nil)
	writer.telemetry.lastRealtimeEnqueueNS.Store(
		time.Now().Add(-35 * time.Millisecond).UnixNano())
	writer.EnqueueRealtimeHaptics([]byte{1})
	state := writer.telemetry.snapshot()
	if state.LateGaps != 1 || state.Underruns < 2 {
		t.Fatalf("unexpected observed cadence accounting: %+v", state)
	}
}

func TestDualSenseOutputBackpressureTelemetryIsExposed(t *testing.T) {
	controller, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	writer := newDualSenseOutputWriter(nil, controller.beginSpeakerStream(), nil)
	writer.EnqueueControl(StreamFrameOutputState, []byte{1})
	writer.EnqueueRealtimeHaptics([]byte{2, 3})
	state := controller.GetDeviceSpecificArgs()
	if state["speakerOrderedFramesEnqueued"] != uint64(1) ||
		state["speakerPayloadsEnqueued"] != uint64(1) ||
		state["speakerQueueDurationUS"] !=
			dualSenseRealtimeHapticsCadence.Microseconds() {
		t.Fatalf("transport telemetry was not exposed: %+v", state)
	}
}

func TestDualSenseOrderedFaultWakesOwningReadLoop(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualSenseOutputWriter(server, nil, nil)
	readDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1)
		_, err := server.Read(buffer)
		readDone <- err
	}()
	for marker := 0; marker <= dualSenseOutputControlQueueCapacity; marker++ {
		writer.EnqueueControl(StreamFrameOutputState, []byte{byte(marker)})
	}
	writer.EnqueueRealtimeHaptics([]byte{1, 2, 3})
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("owning read loop returned without stream fault")
		}
	case <-time.After(time.Second):
		t.Fatal("ordered saturation did not wake the owning read loop")
	}
	state := writer.telemetry.snapshot()
	if state.ReceivedPayloads != 1 || state.RejectedPayloads != 1 ||
		state.RejectedBytes != 3 || state.EnqueuedPayloads != 0 {
		t.Fatalf("media rejection after stream fault was not accounted: %+v", state)
	}
	_ = client.Close()
}

func TestDualSenseLifecycleDrainAccountsEveryAcceptedQueuedFrame(t *testing.T) {
	writer := newDualSenseOutputWriter(nil, nil, nil)
	writer.EnqueueControl(StreamFrameOutputState, []byte{1})
	writer.EnqueueControl(StreamFrameOutputState, []byte{2, 3})
	writer.EnqueueRealtimeHaptics([]byte{1, 2, 3})
	writer.EnqueueRealtimeHaptics([]byte{4, 5})
	writer.requestStop()
	go writer.Run()
	if err := writer.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writer.done:
	default:
		t.Fatal("Stop returned before writer rundown completed")
	}
	state := writer.telemetry.snapshot()
	if state.OrderedLifecycleDiscardedFrames != 2 ||
		state.OrderedLifecycleDiscardedBytes != 3 ||
		state.LifecycleDiscardedPayloads != 2 ||
		state.LifecycleDiscardedBytes != 5 ||
		state.OrderedQueueDepth != 0 || state.QueueDepth != 0 ||
		state.QueueDurationUS != 0 {
		t.Fatalf("lifecycle drain was not fully accounted: %+v", state)
	}
}

type dualSenseWriteGateConn struct {
	net.Conn
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *dualSenseWriteGateConn) Write(payload []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	<-c.release
	return len(payload), nil
}

func TestDualSenseStopLatchesTimeoutAndContinuesAuthoritativeJoin(t *testing.T) {
	server, client := net.Pipe()
	gate := &dualSenseWriteGateConn{
		Conn: server, started: make(chan struct{}), release: make(chan struct{}),
	}
	writer := newDualSenseOutputWriter(gate, nil, nil)
	writer.EnqueueRealtimeHaptics([]byte{1, 2, 3})
	go writer.Run()
	select {
	case <-gate.started:
	case <-time.After(time.Second):
		t.Fatal("media write did not become in-flight")
	}

	err := writer.Stop()
	if !errors.Is(err, errDualSenseOutputJoinTimeout) {
		t.Fatalf("Stop error=%v want=%v", err, errDualSenseOutputJoinTimeout)
	}
	select {
	case <-writer.done:
		t.Fatal("timeout was treated as completed rundown")
	default:
	}
	state := writer.telemetry.snapshot()
	if state.TeardownFailures != 1 || !state.TeardownPending {
		t.Fatalf("teardown timeout was not latched: %+v", state)
	}

	close(gate.release)
	select {
	case <-writer.done:
	case <-time.After(time.Second):
		t.Fatal("writer did not finish after in-flight write was released")
	}
	deadline := time.Now().Add(time.Second)
	for writer.telemetry.snapshot().TeardownPending && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if writer.telemetry.snapshot().TeardownPending {
		t.Fatal("continued teardown join did not clear pending state")
	}
	if err := writer.Stop(); !errors.Is(err, errDualSenseOutputJoinTimeout) {
		t.Fatalf("latched Stop error=%v want=%v", err,
			errDualSenseOutputJoinTimeout)
	}
	_ = client.Close()
}

type dualSenseUninterruptibleStreamConn struct {
	readRelease  chan struct{}
	writeStarted chan struct{}
	writeRelease chan struct{}
	writeOnce    sync.Once
}

func newDualSenseUninterruptibleStreamConn() *dualSenseUninterruptibleStreamConn {
	return &dualSenseUninterruptibleStreamConn{
		readRelease:  make(chan struct{}),
		writeStarted: make(chan struct{}),
		writeRelease: make(chan struct{}),
	}
}

func (c *dualSenseUninterruptibleStreamConn) Read([]byte) (int, error) {
	<-c.readRelease
	return 0, io.EOF
}

func (c *dualSenseUninterruptibleStreamConn) Write(payload []byte) (int, error) {
	c.writeOnce.Do(func() { close(c.writeStarted) })
	<-c.writeRelease
	return len(payload), nil
}

func (*dualSenseUninterruptibleStreamConn) Close() error { return nil }

func (*dualSenseUninterruptibleStreamConn) LocalAddr() net.Addr {
	return &net.TCPAddr{}
}

func (*dualSenseUninterruptibleStreamConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{}
}

func (*dualSenseUninterruptibleStreamConn) SetDeadline(time.Time) error {
	return nil
}

func (*dualSenseUninterruptibleStreamConn) SetReadDeadline(time.Time) error {
	return nil
}

func (*dualSenseUninterruptibleStreamConn) SetWriteDeadline(time.Time) error {
	return nil
}

func TestDualSenseHandlerDetachesBeforeAuthoritativeStop(t *testing.T) {
	controller, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var device usb.Device = controller
	conn := newDualSenseUninterruptibleStreamConn()
	streamHandler := dualSenseV5StreamHandler("DualSense")
	errCh := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		errCh <- streamHandler(conn, &device, logger)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		controller.mtx.Lock()
		callbacksReady := controller.atomicAudioHapticsFunc != nil &&
			controller.speakerResetFunc != nil
		controller.mtx.Unlock()
		if callbacksReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handler did not install media callbacks")
		}
		time.Sleep(time.Millisecond)
	}

	controller.SetInterfaceAltSetting(InterfaceHapticsAudio, 1)
	pcm := make([]byte,
		dualSenseV5SpeakerFrames*USBHapticsAudioFrameSize)
	controller.HandleTransfer(context.Background(), EndpointHapticsAudioOut,
		usbip.DirOut, pcm)
	select {
	case <-conn.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("handler writer did not enter the uninterruptible write")
	}
	close(conn.readRelease)

	select {
	case err := <-errCh:
		if !errors.Is(err, errDualSenseOutputJoinTimeout) {
			t.Fatalf("handler error=%v want=%v", err,
				errDualSenseOutputJoinTimeout)
		}
	case <-time.After(time.Second):
		t.Fatal("handler cleanup blocked in reset before Stop could report failure")
	}
	controller.mtx.Lock()
	callbacksDetached := controller.outputFunc == nil &&
		controller.atomicAudioHapticsFunc == nil &&
		controller.realtimeHapticsFunc == nil &&
		controller.speakerResetFunc == nil
	controller.mtx.Unlock()
	if !callbacksDetached {
		t.Fatal("handler retained callbacks after teardown failure")
	}
	state := controller.GetDeviceSpecificArgs()
	if state["speakerTeardownFailures"] != uint64(1) ||
		state["speakerTeardownPending"] != true {
		t.Fatalf("handler did not expose pending teardown: %+v", state)
	}

	close(conn.writeRelease)
	deadline = time.Now().Add(time.Second)
	for {
		state = controller.GetDeviceSpecificArgs()
		if state["speakerTeardownPending"] == false &&
			state["speakerStreamActive"] == false {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("continued writer join did not complete: %+v", state)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDualSenseResetCloseAndInFlightWriteCannotDeadlock(t *testing.T) {
	server, client := net.Pipe()
	conn := newDeadlineTrackingConn(server)
	writer := newDualSenseOutputWriter(conn, nil, nil)
	feedback, speaker := testV5Media(0x71)
	writer.EnqueueAtomicAudioHaptics(feedback, speaker)
	go writer.Run()
	select {
	case <-conn.started:
	case <-time.After(time.Second):
		t.Fatal("media write did not become in-flight")
	}
	resetDone := make(chan struct{})
	stopDone := make(chan error, 1)
	go func() { writer.ResetSpeaker(); close(resetDone) }()
	go func() { stopDone <- writer.Stop() }()
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("reset deadlocked with in-flight write")
	}
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop deadlocked with in-flight write")
	}
	select {
	case <-writer.done:
	default:
		t.Fatal("Stop returned before writer rundown completed")
	}
	_ = client.Close()
}

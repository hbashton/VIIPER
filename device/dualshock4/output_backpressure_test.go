package dualshock4

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

func TestDualShock4OrderedPublicationIsFIFOWithConcurrentProducers(t *testing.T) {
	writer := newDualShock4OutputWriter(nil, StreamFrameVersionV3)
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
		decrementDualShock4Uint64(&writer.telemetry.orderedQueueDepth)
		require.Equal(t, publication, frame.publication)
		require.Len(t, frame.payload, 1)
		assert.False(t, seen[frame.payload[0]], "payload was duplicated")
		seen[frame.payload[0]] = true
	}
	state := writer.telemetry.snapshot()
	assert.Equal(t, uint64(producers), state.OrderedReceived)
	assert.Equal(t, uint64(producers), state.OrderedEnqueued)
	assert.Zero(t, state.OrderedRejected)
	assert.Zero(t, state.OrderedSaturations)
}

func dualShock4PacketPayload(packetCount int, marker byte) []byte {
	payload := make([]byte, packetCount*USBSpeakerMaxPacketSize)
	for index := range payload {
		payload[index] = marker
	}
	return payload
}

func TestDualShock4MediaDurationUsesActualPCMFrames(t *testing.T) {
	writer := newDualShock4OutputWriter(nil, StreamFrameVersionV3)
	var expected time.Duration
	for packets := 2; packets <= 4; packets++ {
		payload := dualShock4PacketPayload(packets, byte(packets))
		writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, payload)
		frames := len(payload) / dualShock4SpeakerFrameBytes
		expected += time.Duration(frames) * time.Second / USBSpeakerSampleRate
	}
	require.Equal(t, 9*time.Millisecond+281250*time.Nanosecond, expected)
	assert.Equal(t, int64(expected),
		writer.telemetry.mediaQueueDurationNS.Load())
	assert.Equal(t, expected.Microseconds(),
		writer.telemetry.snapshot().MediaQueueDurationUS)

	for packets := 2; packets <= 4; packets++ {
		frame := <-writer.audio
		writer.recordMediaDequeued(frame)
		require.Equal(t, byte(packets), frame.payload[0])
	}
	assert.Zero(t, writer.telemetry.mediaQueueDurationNS.Load())
}

func TestDualShock4MediaWindowIsTwoHundredMillisecondsAndDropsOldest(t *testing.T) {
	writer := newDualShock4OutputWriter(nil, StreamFrameVersionV3)
	require.Equal(t, 20, cap(writer.audio))
	const payloadFrames = USBSpeakerSampleRate / 50
	payloadDuration := time.Duration(payloadFrames) * time.Second /
		USBSpeakerSampleRate
	frameCount := int(dualShock4SpeakerMaximumBufferTime / payloadDuration)
	require.Equal(t, 10, frameCount)
	for marker := 0; marker <= frameCount; marker++ {
		payload := make([]byte, payloadFrames*dualShock4SpeakerFrameBytes)
		for index := range payload {
			payload[index] = byte(marker)
		}
		writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, payload)
	}
	require.Len(t, writer.audio, frameCount)
	for index := 0; index < frameCount; index++ {
		frame := <-writer.audio
		writer.recordMediaDequeued(frame)
		require.Equal(t, byte(index+1), frame.payload[0])
	}
	state := writer.telemetry.snapshot()
	assert.Equal(t, uint64(frameCount+1),
		state.MediaReceivedPayloads)
	assert.Equal(t, uint64(frameCount+1),
		state.MediaEnqueuedPayloads)
	assert.Equal(t, uint64(1), state.MediaOverruns)
	assert.Equal(t, uint64(1), state.MediaDroppedPayloads)
	assert.Equal(t, uint64(payloadFrames*dualShock4SpeakerFrameBytes),
		state.MediaDroppedBytes)
	assert.Equal(t, uint64(frameCount),
		state.MediaQueueHighWater)
	assert.LessOrEqual(t, state.MediaQueueDurationHighWaterUS,
		dualShock4SpeakerMaximumBufferTime.Microseconds())
	assert.Zero(t, state.MediaQueueDepth)
}

func TestDualShock4MediaItemBoundDropsOldestBeforeAllocationsGrow(t *testing.T) {
	writer := newDualShock4OutputWriter(nil, StreamFrameVersionV3)
	for marker := 0; marker <= dualShock4OutputAudioQueueCapacity; marker++ {
		writer.EnqueueAudioOwned(StreamFrameSpeakerPCM,
			dualShock4PacketPayload(2, byte(marker)))
	}
	require.Len(t, writer.audio, dualShock4OutputAudioQueueCapacity)
	for index := 0; index < dualShock4OutputAudioQueueCapacity; index++ {
		frame := <-writer.audio
		writer.recordMediaDequeued(frame)
		require.Equal(t, byte(index+1), frame.payload[0])
	}
	state := writer.telemetry.snapshot()
	assert.Equal(t, uint64(1), state.MediaOverruns)
	assert.Equal(t, uint64(1), state.MediaDroppedPayloads)
	assert.Equal(t, uint64(dualShock4OutputAudioQueueCapacity),
		state.MediaQueueHighWater)
	assert.Less(t, state.MediaQueueDurationHighWaterUS,
		dualShock4SpeakerMaximumBufferTime.Microseconds())
	assert.Zero(t, state.MediaQueueDepth)
}

func TestDualShock4MediaRejectsMalformedAndSinglePayloadOverLimit(t *testing.T) {
	writer := newDualShock4OutputWriter(nil, StreamFrameVersionV3)
	writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, []byte{1, 2, 3})
	tooLarge := make([]byte,
		(dualShock4SpeakerMaximumBufferFrames+1)*dualShock4SpeakerFrameBytes)
	writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, tooLarge)

	state := writer.telemetry.snapshot()
	assert.Equal(t, uint64(2), state.MediaReceivedPayloads)
	assert.Equal(t, uint64(1), state.MediaMalformedPayloads)
	assert.Equal(t, uint64(3), state.MediaMalformedBytes)
	assert.Equal(t, uint64(1), state.MediaOversizePayloads)
	assert.Equal(t, uint64(len(tooLarge)), state.MediaOversizeBytes)
	assert.Zero(t, state.MediaEnqueuedPayloads)
	assert.Zero(t, state.MediaQueueDepth)
	assert.Empty(t, writer.audio)
}

func TestDualShock4ResetCountsStaleGenerationAndClearsCadence(t *testing.T) {
	writer := newDualShock4OutputWriter(nil, StreamFrameVersionV3)
	writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, []byte{1, 2, 3, 4})
	writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, []byte{5, 6, 7, 8})
	writer.ResetSpeaker()

	state := writer.telemetry.snapshot()
	assert.Equal(t, uint64(2), state.MediaStalePayloads)
	assert.Equal(t, uint64(8), state.MediaStaleBytes)
	assert.Zero(t, state.MediaQueueDepth)
	assert.Empty(t, writer.audio)
	assert.Zero(t, writer.telemetry.lastMediaEnqueueNS.Load())
}

func TestDualShock4WriterRecordsOnlyObservedProducerCadenceGap(t *testing.T) {
	writer := newDualShock4OutputWriter(nil, StreamFrameVersionV3)
	writer.telemetry.lastMediaEnqueueNS.Store(
		time.Now().Add(-35 * time.Millisecond).UnixNano())
	writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, []byte{1, 2, 3, 4})
	state := writer.telemetry.snapshot()
	assert.Equal(t, uint64(1), state.MediaLateGaps)
	assert.GreaterOrEqual(t, state.MediaUnderruns, uint64(2))
}

func TestDualShock4OutputBackpressureTelemetryIsExposed(t *testing.T) {
	controller, err := New(nil)
	require.NoError(t, err)
	writer := newDualShock4OutputWriterForStream(nil, StreamFrameVersionV3,
		controller.beginSpeakerStream(), nil)
	writer.EnqueueControl(StreamFrameOutputState, []byte{1})
	writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, []byte{2, 3, 4, 5})
	state := controller.GetDeviceSpecificArgs()
	assert.Equal(t, uint64(1), state["speakerOrderedFramesEnqueued"])
	assert.Equal(t, uint64(1), state["speakerPayloadsEnqueued"])
	assert.Equal(t, int64(31), state["speakerQueueDurationUS"])
	assert.Equal(t, int64(31),
		state["speakerQueueDurationHighWaterUS"])
}

func TestDualShock4OrderedFaultWakesOwningReadLoop(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualShock4OutputWriter(server, StreamFrameVersionV3)
	readDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1)
		_, err := server.Read(buffer)
		readDone <- err
	}()
	for marker := 0; marker <= dualShock4OutputControlQueueCapacity; marker++ {
		writer.EnqueueControl(StreamFrameOutputState, []byte{byte(marker)})
	}
	writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, []byte{1, 2, 3, 4})
	select {
	case err := <-readDone:
		assert.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("ordered saturation did not wake the owning read loop")
	}
	state := writer.telemetry.snapshot()
	assert.Equal(t, uint64(1), state.MediaReceivedPayloads)
	assert.Equal(t, uint64(1), state.MediaRejectedPayloads)
	assert.Equal(t, uint64(4), state.MediaRejectedBytes)
	assert.Zero(t, state.MediaEnqueuedPayloads)
	require.NoError(t, client.Close())
}

func TestDualShock4LifecycleDrainAccountsEveryAcceptedQueuedFrame(t *testing.T) {
	writer := newDualShock4OutputWriter(nil, StreamFrameVersionV3)
	writer.EnqueueControl(StreamFrameOutputState, []byte{1})
	writer.EnqueueControl(StreamFrameOutputState, []byte{2, 3})
	writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, []byte{1, 2, 3, 4})
	writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, []byte{5, 6, 7, 8})
	writer.requestStop()
	go writer.Run()
	require.NoError(t, writer.Stop())

	select {
	case <-writer.done:
	default:
		t.Fatal("Stop returned before writer rundown completed")
	}
	state := writer.telemetry.snapshot()
	assert.Equal(t, uint64(2), state.OrderedLifecycleDiscardedFrames)
	assert.Equal(t, uint64(3), state.OrderedLifecycleDiscardedBytes)
	assert.Equal(t, uint64(2), state.MediaLifecycleDiscardedPayloads)
	assert.Equal(t, uint64(8), state.MediaLifecycleDiscardedBytes)
	assert.Zero(t, state.OrderedQueueDepth)
	assert.Zero(t, state.MediaQueueDepth)
	assert.Zero(t, state.MediaQueueDurationUS)
}

func TestDualShock4StopLatchesTimeoutAndContinuesAuthoritativeJoin(t *testing.T) {
	server, client := net.Pipe()
	gate := &dualShock4WriteGateConn{
		Conn: server, started: make(chan struct{}), release: make(chan struct{}),
	}
	writer := newDualShock4OutputWriter(gate, StreamFrameVersionV3)
	writer.EnqueueAudio(StreamFrameSpeakerPCM, []byte{1, 2, 3, 4})
	go writer.Run()
	select {
	case <-gate.started:
	case <-time.After(time.Second):
		t.Fatal("media write did not become in-flight")
	}

	err := writer.Stop()
	require.ErrorIs(t, err, errDualShock4OutputJoinTimeout)
	select {
	case <-writer.done:
		t.Fatal("timeout was treated as completed rundown")
	default:
	}
	state := writer.telemetry.snapshot()
	assert.Equal(t, uint64(1), state.TeardownFailures)
	assert.True(t, state.TeardownPending)

	close(gate.release)
	select {
	case <-writer.done:
	case <-time.After(time.Second):
		t.Fatal("writer did not finish after in-flight write was released")
	}
	assert.Eventually(t, func() bool {
		return !writer.telemetry.snapshot().TeardownPending
	}, time.Second, time.Millisecond)
	require.ErrorIs(t, writer.Stop(), errDualShock4OutputJoinTimeout)
	require.NoError(t, client.Close())
}

type dualShock4UninterruptibleStreamConn struct {
	readRelease  chan struct{}
	writeStarted chan struct{}
	writeRelease chan struct{}
	writeOnce    sync.Once
}

func newDualShock4UninterruptibleStreamConn() *dualShock4UninterruptibleStreamConn {
	return &dualShock4UninterruptibleStreamConn{
		readRelease:  make(chan struct{}),
		writeStarted: make(chan struct{}),
		writeRelease: make(chan struct{}),
	}
}

func (c *dualShock4UninterruptibleStreamConn) Read([]byte) (int, error) {
	<-c.readRelease
	return 0, io.EOF
}

func (c *dualShock4UninterruptibleStreamConn) Write(payload []byte) (int, error) {
	c.writeOnce.Do(func() { close(c.writeStarted) })
	<-c.writeRelease
	return len(payload), nil
}

func (*dualShock4UninterruptibleStreamConn) Close() error { return nil }

func (*dualShock4UninterruptibleStreamConn) LocalAddr() net.Addr {
	return &net.TCPAddr{}
}

func (*dualShock4UninterruptibleStreamConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{}
}

func (*dualShock4UninterruptibleStreamConn) SetDeadline(time.Time) error {
	return nil
}

func (*dualShock4UninterruptibleStreamConn) SetReadDeadline(time.Time) error {
	return nil
}

func (*dualShock4UninterruptibleStreamConn) SetWriteDeadline(time.Time) error {
	return nil
}

func TestDualShock4HandlerDetachesBeforeAuthoritativeStop(t *testing.T) {
	controller, err := New(nil)
	require.NoError(t, err)
	var device usb.Device = controller
	conn := newDualShock4UninterruptibleStreamConn()
	streamHandler := (&handler{
		speakerOutput: true, streamFrameVersion: StreamFrameVersionV3,
	}).StreamHandler()
	errCh := make(chan error, 1)
	go func() {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		errCh <- streamHandler(conn, &device, logger)
	}()
	require.Eventually(t, func() bool {
		controller.mtx.Lock()
		defer controller.mtx.Unlock()
		return controller.speakerFunc != nil && controller.speakerResetFunc != nil
	}, time.Second, time.Millisecond)

	controller.SetInterfaceAltSetting(InterfaceSpeaker, 1)
	controller.HandleTransfer(context.Background(), uint32(EndpointAudioOut),
		usbip.DirOut, dualShock4PacketPayload(2, 0x5A))
	select {
	case <-conn.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("handler writer did not enter the uninterruptible write")
	}
	close(conn.readRelease)

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, errDualShock4OutputJoinTimeout)
	case <-time.After(time.Second):
		t.Fatal("handler cleanup blocked in reset before Stop could report failure")
	}
	controller.mtx.Lock()
	callbacksDetached := controller.outputFunc == nil &&
		controller.speakerFunc == nil && controller.speakerResetFunc == nil
	controller.mtx.Unlock()
	assert.True(t, callbacksDetached)
	state := controller.GetDeviceSpecificArgs()
	assert.Equal(t, uint64(1), state["speakerTeardownFailures"])
	assert.Equal(t, true, state["speakerTeardownPending"])

	close(conn.writeRelease)
	require.Eventually(t, func() bool {
		state := controller.GetDeviceSpecificArgs()
		return state["speakerTeardownPending"] == false &&
			state["speakerStreamActive"] == false
	}, time.Second, time.Millisecond)
}

func TestDualShock4ResetCloseAndInFlightWriteCannotDeadlock(t *testing.T) {
	server, client := net.Pipe()
	conn := &dualShock4DeadlineBlockConn{
		Conn: server, started: make(chan struct{}), unblock: make(chan struct{}),
	}
	writer := newDualShock4OutputWriter(conn, StreamFrameVersionV3)
	writer.EnqueueAudio(StreamFrameSpeakerPCM, []byte{1, 2, 3, 4})
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
		require.NoError(t, err)
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

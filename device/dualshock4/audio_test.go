package dualshock4

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescriptorExposesNativeDS4AudioTopology(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)

	desc := dev.GetDescriptor()
	assert.Equal(t, uint8(4), desc.NumInterfaces())
	assert.Equal(t, uint8(InterfaceHID), desc.Interfaces[len(desc.Interfaces)-1].Descriptor.BInterfaceNumber)
	require.Len(t, desc.Interfaces[0].ClassDescriptors, 7)
	assert.Len(t, desc.Interfaces[0].ClassDescriptors[2].Payload, 8)

	var speakerEndpointFound bool
	var microphoneEndpointFound bool
	for _, iface := range desc.Interfaces {
		for _, endpoint := range iface.Endpoints {
			switch endpoint.BEndpointAddress {
			case EndpointAudioOut:
				speakerEndpointFound = true
				assert.Equal(t, uint8(0x09), endpoint.BMAttributes)
				assert.Equal(t, uint16(USBSpeakerMaxPacketSize), endpoint.WMaxPacketSize)
			case EndpointMicrophoneIn:
				microphoneEndpointFound = true
				assert.Equal(t, uint8(0x05), endpoint.BMAttributes)
				assert.Equal(t, uint16(USBMicrophoneMaxPacketSize), endpoint.WMaxPacketSize)
			}
		}
	}

	assert.True(t, speakerEndpointFound)
	assert.True(t, microphoneEndpointFound)

	configurationLength := 9
	for _, iface := range desc.Interfaces {
		configurationLength += 9
		if iface.HID != nil {
			configurationLength += 9
		}
		for _, classDescriptor := range iface.ClassDescriptors {
			configurationLength += 2 + len(classDescriptor.Payload)
		}
		for _, endpoint := range iface.Endpoints {
			configurationLength += 7 + len(endpoint.Trailing)
			for _, classDescriptor := range endpoint.ClassDescriptors {
				configurationLength += 2 + len(classDescriptor.Payload)
			}
		}
	}
	assert.Equal(t, 225, configurationLength)
}

func TestAudioOnlyDescriptorKeepsAudioAndRemovesHID(t *testing.T) {
	desc := makeAudioOnlyDescriptor()
	assert.Equal(t, uint8(3), desc.NumInterfaces())

	var speakerEndpointFound bool
	var microphoneEndpointFound bool
	for _, iface := range desc.Interfaces {
		assert.Nil(t, iface.HID)
		assert.NotEqual(t, uint8(0x03),
			iface.Descriptor.BInterfaceClass)
		for _, endpoint := range iface.Endpoints {
			switch endpoint.BEndpointAddress {
			case EndpointAudioOut:
				speakerEndpointFound = true
			case EndpointMicrophoneIn:
				microphoneEndpointFound = true
			}
		}
	}

	assert.True(t, speakerEndpointFound)
	assert.True(t, microphoneEndpointFound)
}

func TestAudioSamplingFrequencyControlsMatchDS4Hardware(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)

	speaker, handled := dev.HandleControl(audioClassEndpointIn, audioClassRequestGetCurrent,
		uint16(audioControlSamplingFrequency)<<8, EndpointAudioOut, 3, nil)
	require.True(t, handled)
	assert.Equal(t, []byte{0x00, 0x7D, 0x00}, speaker)

	microphone, handled := dev.HandleControl(audioClassEndpointIn, audioClassRequestGetCurrent,
		uint16(audioControlSamplingFrequency)<<8, EndpointMicrophoneIn, 3, nil)
	require.True(t, handled)
	assert.Equal(t, []byte{0x80, 0x3E, 0x00}, microphone)
}

func TestAudioInterfacesTrackAlternateSettings(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)

	dev.SetInterfaceAltSetting(InterfaceSpeaker, 1)
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)
	state := dev.GetDeviceSpecificArgs()
	assert.Equal(t, true, state["speakerInterfaceActive"])
	assert.Equal(t, true, state["microphoneInterfaceActive"])

	microphone := dev.HandleTransfer(context.Background(), uint32(EndpointMicrophoneIn), usbip.DirIn, nil)
	assert.Len(t, microphone, USBMicrophonePacketSize)
	assert.Equal(t, make([]byte, USBMicrophonePacketSize), microphone)
}

func TestNativeMicrophoneInWritesCallerBuffer(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)
	frame := make([]byte, USBMicrophoneClientFrameSize)
	for index := range frame {
		frame[index] = byte(index*13 + 3)
	}
	for range microphoneTargetClientFrames {
		dev.QueueMicrophonePCMFrame(frame)
	}

	packet := make([]byte, USBMicrophonePacketSize)
	actual, err := dev.ReadIsochronousInput(
		context.Background(), uint32(EndpointMicrophoneIn), packet)
	require.NoError(t, err)
	require.Equal(t, len(packet), actual)
	assert.Equal(t, frame[:len(packet)], packet)
	_, err = dev.ReadIsochronousInput(context.Background(),
		uint32(EndpointMicrophoneIn), packet[:len(packet)-1])
	assert.ErrorIs(t, err, io.ErrShortBuffer)
}

func TestSpeakerTransferIsForwardedWithoutLoopbackCapture(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)
	dev.SetInterfaceAltSetting(InterfaceSpeaker, 1)

	forwarded := make(chan []byte, 1)
	dev.SetSpeakerCallback(func(pcm []byte) { forwarded <- pcm })
	pcm := make([]byte, 128)
	for index := range pcm {
		pcm[index] = byte(index)
	}

	dev.HandleTransfer(context.Background(), uint32(EndpointAudioOut),
		usbip.DirOut, pcm)
	got := <-forwarded
	assert.Equal(t, pcm, got)

	// The callback owns a copy; reusing the USB/IP buffer must not mutate it.
	pcm[0] = 0xFF
	assert.Equal(t, byte(0), got[0])
	dev.SetSpeakerCallback(nil)
}

func TestDuplexWriterFramesSpeakerPCM(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualShock4OutputWriter(server, StreamFrameVersionV3)
	go writer.Run()

	pcm := []byte{0x00, 0x01, 0xFE, 0xFF}
	writer.EnqueueAudio(StreamFrameSpeakerPCM, pcm)
	header := make([]byte, StreamFrameHeaderSize)
	_, err := io.ReadFull(client, header)
	require.NoError(t, err)
	payload := make([]byte, binary.LittleEndian.Uint16(header[6:8]))
	_, err = io.ReadFull(client, payload)
	require.NoError(t, err)

	assert.Equal(t, []byte{StreamFrameMagic0, StreamFrameMagic1,
		StreamFrameMagic2, StreamFrameMagic3}, header[:4])
	assert.Equal(t, byte(StreamFrameVersionV3), header[4])
	assert.Equal(t, byte(StreamFrameSpeakerPCM), header[5])
	assert.Equal(t, uint32(0), binary.LittleEndian.Uint32(header[8:12]))
	assert.Equal(t, dualShock4FramedStreamCRC(header[4:12], payload),
		binary.LittleEndian.Uint32(header[12:16]))
	assert.Equal(t, pcm, payload)

	require.NoError(t, client.Close())
	require.NoError(t, writer.Stop())
}

func TestAudioInterfaceTransitionsDropPreviousGeneration(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)
	server, client := net.Pipe()
	writer := newDualShock4OutputWriter(server, StreamFrameVersionV3)
	dev.SetSpeakerCallback(func(pcm []byte) {
		writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, pcm)
	})
	dev.SetSpeakerResetCallback(writer.ResetSpeaker)

	dev.SetInterfaceAltSetting(InterfaceSpeaker, 1)
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)
	stale := []byte{0x11, 0x22, 0x33, 0x44}
	dev.HandleTransfer(context.Background(), uint32(EndpointAudioOut),
		usbip.DirOut, stale)
	require.Len(t, writer.audio, 1)
	microphone := make([]byte, USBMicrophoneClientFrameSize)
	for range microphoneTargetClientFrames {
		dev.QueueMicrophonePCMFrame(microphone)
	}
	require.Equal(t, true,
		dev.GetDeviceSpecificArgs()["microphoneQueuePrimed"])

	dev.SetInterfaceAltSetting(InterfaceSpeaker, 0)
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 0)
	require.Empty(t, writer.audio,
		"closing the speaker interface retained stale PCM")
	state := dev.GetDeviceSpecificArgs()
	assert.Equal(t, 0, state["queuedMicrophoneBytes"])
	assert.Equal(t, false, state["microphoneQueuePrimed"])
	dev.SetInterfaceAltSetting(InterfaceSpeaker, 1)
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)
	state = dev.GetDeviceSpecificArgs()
	assert.Equal(t, true, state["speakerInterfaceActive"])
	assert.Equal(t, true, state["microphoneInterfaceActive"])

	go writer.Run()
	fresh := []byte{0x55, 0x66, 0x77, 0x88}
	dev.HandleTransfer(context.Background(), uint32(EndpointAudioOut),
		usbip.DirOut, fresh)
	assert.Equal(t, fresh, readDualShock4OutputPayload(t, client))

	dev.SetSpeakerCallback(nil)
	dev.SetSpeakerResetCallback(nil)
	require.NoError(t, client.Close())
	require.NoError(t, writer.Stop())
}

func TestEndpointResetDropsSpeakerAndMicrophoneWithoutChangingAlt(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)
	server, client := net.Pipe()
	writer := newDualShock4OutputWriter(server, StreamFrameVersionV3)
	dev.SetSpeakerCallback(func(pcm []byte) {
		writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, pcm)
	})
	dev.SetSpeakerResetCallback(writer.ResetSpeaker)
	dev.SetInterfaceAltSetting(InterfaceSpeaker, 1)
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)

	dev.HandleTransfer(context.Background(), uint32(EndpointAudioOut),
		usbip.DirOut, []byte{0x10, 0x20, 0x30, 0x40})
	require.Len(t, writer.audio, 1)
	microphone := make([]byte, USBMicrophoneClientFrameSize)
	for index := range microphone {
		microphone[index] = byte(index + 1)
	}
	for range microphoneTargetClientFrames {
		dev.QueueMicrophonePCMFrame(microphone)
	}
	require.Equal(t, true,
		dev.GetDeviceSpecificArgs()["microphoneQueuePrimed"])

	dev.ResetEndpoint(EndpointAudioOut)
	dev.ResetEndpoint(EndpointMicrophoneIn)
	state := dev.GetDeviceSpecificArgs()
	assert.Equal(t, true, state["speakerInterfaceActive"])
	assert.Equal(t, true, state["microphoneInterfaceActive"])
	assert.Equal(t, 0, state["queuedMicrophoneBytes"])
	assert.Equal(t, false, state["microphoneQueuePrimed"])
	require.Empty(t, writer.audio,
		"speaker endpoint reset retained stale PCM")

	go writer.Run()
	fresh := []byte{0x50, 0x60, 0x70, 0x80}
	dev.HandleTransfer(context.Background(), uint32(EndpointAudioOut),
		usbip.DirOut, fresh)
	assert.Equal(t, fresh, readDualShock4OutputPayload(t, client))

	dev.SetSpeakerCallback(nil)
	dev.SetSpeakerResetCallback(nil)
	require.NoError(t, client.Close())
	require.NoError(t, writer.Stop())
}

func TestSpeakerRejectsPublicationFromPreResetRevision(t *testing.T) {
	device, err := New(nil)
	require.NoError(t, err)
	device.SetInterfaceAltSetting(InterfaceSpeaker, 1)
	published := 0
	device.SetSpeakerCallback(func([]byte) { published++ })

	device.mtx.Lock()
	revision := device.speakerRevision
	device.mtx.Unlock()
	device.ResetEndpoint(EndpointAudioOut)
	assert.False(t, device.publishSpeakerPCM(revision, []byte{1, 2, 3, 4}))
	assert.Equal(t, 0, published)
}

func TestSpeakerResetWaitsForInFlightDevicePublication(t *testing.T) {
	device, err := New(nil)
	require.NoError(t, err)
	device.SetInterfaceAltSetting(InterfaceSpeaker, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	device.SetSpeakerCallback(func([]byte) {
		close(entered)
		<-release
	})
	resetCalls := 0
	device.SetSpeakerResetCallback(func() { resetCalls++ })

	transferDone := make(chan struct{})
	go func() {
		device.HandleTransfer(context.Background(), EndpointAudioOut,
			usbip.DirOut, []byte{1, 2, 3, 4})
		close(transferDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("speaker callback did not start")
	}
	resetDone := make(chan struct{})
	go func() {
		device.ResetEndpoint(EndpointAudioOut)
		close(resetDone)
	}()
	select {
	case <-resetDone:
		t.Fatal("endpoint reset crossed an in-flight speaker publication")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("endpoint reset did not finish after speaker publication")
	}
	<-transferDone
	assert.Equal(t, 1, resetCalls)
}

func TestDualShock4WriterFaultsOnOrderedSaturationWithoutEviction(t *testing.T) {
	server, client := net.Pipe()
	writer := newDualShock4OutputWriter(server, StreamFrameVersionV3)
	for marker := 0; marker < cap(writer.control); marker++ {
		writer.EnqueueControl(StreamFrameOutputState, []byte{byte(marker)})
	}
	writer.EnqueueControl(StreamFrameOutputState, []byte{0xFF})
	depth := cap(writer.control)
	require.Len(t, writer.control, depth)
	for index := 0; index < depth; index++ {
		frame := <-writer.control
		decrementDualShock4Uint64(&writer.telemetry.orderedQueueDepth)
		require.Equal(t, []byte{byte(index)}, frame.payload)
	}
	state := writer.telemetry.snapshot()
	assert.Equal(t, uint64(depth+1), state.OrderedReceived)
	assert.Equal(t, uint64(depth), state.OrderedEnqueued)
	assert.Equal(t, uint64(1), state.OrderedRejected)
	assert.Equal(t, uint64(1), state.OrderedSaturations)
	assert.False(t, state.Active)
	assert.False(t, writer.accepting.Load())
	buffer := make([]byte, 1)
	count, err := client.Read(buffer)
	assert.Zero(t, count)
	assert.Error(t, err, "saturation must close the owning stream")
	require.NoError(t, client.Close())
}

type dualShock4WriteGateConn struct {
	net.Conn
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *dualShock4WriteGateConn) Write(payload []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	<-c.release
	return len(payload), nil
}

func TestSpeakerResetWaitsForInFlightWrite(t *testing.T) {
	server, client := net.Pipe()
	gate := &dualShock4WriteGateConn{
		Conn: server, started: make(chan struct{}), release: make(chan struct{}),
	}
	writer := newDualShock4OutputWriter(gate, StreamFrameVersionV3)
	go writer.Run()
	writer.EnqueueAudio(StreamFrameSpeakerPCM, []byte{0x01, 0x02, 0x03, 0x04})

	select {
	case <-gate.started:
	case <-time.After(time.Second):
		t.Fatal("speaker writer did not start")
	}
	resetDone := make(chan struct{})
	go func() {
		writer.ResetSpeaker()
		close(resetDone)
	}()
	select {
	case <-resetDone:
		t.Fatal("speaker reset crossed an in-flight write")
	case <-time.After(20 * time.Millisecond):
	}

	close(gate.release)
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("speaker reset did not finish after the write completed")
	}

	require.NoError(t, client.Close())
	require.NoError(t, writer.Stop())
}

type dualShock4DeadlineBlockConn struct {
	net.Conn
	started     chan struct{}
	unblock     chan struct{}
	startedOnce sync.Once
	unblockOnce sync.Once
	mu          sync.Mutex
	writes      [][]byte
	deadlines   []time.Time
	closeCount  int
}

func (c *dualShock4DeadlineBlockConn) Write(payload []byte) (int, error) {
	c.mu.Lock()
	c.writes = append(c.writes, append([]byte(nil), payload...))
	c.mu.Unlock()
	c.startedOnce.Do(func() { close(c.started) })
	<-c.unblock
	return 0, context.DeadlineExceeded
}

func (c *dualShock4DeadlineBlockConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, deadline)
	c.mu.Unlock()
	if !deadline.IsZero() {
		c.unblockOnce.Do(func() { close(c.unblock) })
	}
	return nil
}

func (c *dualShock4DeadlineBlockConn) Close() error {
	c.mu.Lock()
	c.closeCount++
	c.mu.Unlock()
	c.unblockOnce.Do(func() { close(c.unblock) })
	return c.Conn.Close()
}

func (c *dualShock4DeadlineBlockConn) snapshot() ([][]byte, []time.Time, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	writes := make([][]byte, len(c.writes))
	for index := range c.writes {
		writes[index] = append([]byte(nil), c.writes[index]...)
	}
	return writes, append([]time.Time(nil), c.deadlines...), c.closeCount
}

func TestSpeakerResetBoundsBlockedWriteAndDropsQueuedGeneration(t *testing.T) {
	server, client := net.Pipe()
	conn := &dualShock4DeadlineBlockConn{
		Conn: server, started: make(chan struct{}), unblock: make(chan struct{}),
	}
	writer := newDualShock4OutputWriter(conn, StreamFrameVersionV3)
	writer.EnqueueAudio(StreamFrameSpeakerPCM, []byte{0x11, 0x11, 0x11, 0x11})
	go writer.Run()
	select {
	case <-conn.started:
	case <-time.After(time.Second):
		t.Fatal("speaker writer did not enter the blocked write")
	}
	writer.EnqueueAudio(StreamFrameSpeakerPCM, []byte{0x22, 0x22, 0x22, 0x22})

	resetStarted := time.Now()
	resetDone := make(chan struct{})
	go func() {
		writer.ResetSpeaker()
		close(resetDone)
	}()
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("speaker reset did not bound the blocked write")
	}
	if elapsed := time.Since(resetStarted); elapsed > time.Second {
		t.Fatalf("speaker reset exceeded its bounded wait: %s", elapsed)
	}
	select {
	case <-writer.done:
	case <-time.After(time.Second):
		t.Fatal("writer did not stop so the timed-out stream could reconnect")
	}

	writes, deadlines, closeCount := conn.snapshot()
	require.Len(t, writes, 1,
		"queued old-generation speaker PCM was replayed after reset")
	require.GreaterOrEqual(t, len(writes[0]), StreamFrameHeaderSize+1)
	assert.Equal(t, byte(0x11), writes[0][StreamFrameHeaderSize])
	require.Len(t, deadlines, 1,
		"timed-out stream must not clear its reset deadline")
	assert.False(t, deadlines[0].IsZero())
	assert.GreaterOrEqual(t, deadlines[0].Sub(resetStarted),
		dualShock4SpeakerResetWriteTimeout-50*time.Millisecond)
	assert.GreaterOrEqual(t, closeCount, 1,
		"timed-out stream was not closed for reconnect")
	assert.Empty(t, writer.audio)
	assert.Equal(t, uint64(1),
		writer.telemetry.snapshot().MediaWriteFailures)
	writer.EnqueueAudio(StreamFrameSpeakerPCM, []byte{0x33, 0x33, 0x33, 0x33})
	assert.Empty(t, writer.audio,
		"failed writer accepted audio instead of waiting for reconnect")

	require.NoError(t, client.Close())
}

func readDualShock4OutputPayload(t *testing.T, reader io.Reader) []byte {
	t.Helper()
	header := make([]byte, StreamFrameHeaderSize)
	_, err := io.ReadFull(reader, header)
	require.NoError(t, err)
	require.Equal(t, byte(StreamFrameSpeakerPCM), header[5])
	payload := make([]byte, binary.LittleEndian.Uint16(header[6:8]))
	_, err = io.ReadFull(reader, payload)
	require.NoError(t, err)
	return payload
}

func TestFramedStreamQueuesDS4MicrophonePCM(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)

	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- readDualShock4InputStream(server, dev,
			slog.New(slog.NewTextHandler(io.Discard, nil)), true,
			StreamFrameVersionV3)
	}()

	input, err := NewInputState().MarshalBinary()
	require.NoError(t, err)
	microphone := make([]byte, USBMicrophoneClientFrameSize)
	for index := range microphone {
		microphone[index] = byte(index)
	}

	_, err = client.Write(makeDualShock4StreamFrame(StreamFrameInputState, 0, input))
	require.NoError(t, err)
	for sequence := uint32(1); sequence <= microphoneTargetClientFrames; sequence++ {
		_, err = client.Write(makeDualShock4StreamFrame(StreamFrameMicrophonePCM, sequence, microphone))
		require.NoError(t, err)
	}
	require.NoError(t, client.Close())
	require.NoError(t, <-done)

	packet := dev.HandleTransfer(context.Background(),
		uint32(EndpointMicrophoneIn&0x0F), usbip.DirIn, nil)
	require.Len(t, packet, USBMicrophonePacketSize)
	assert.Equal(t, microphone[:USBMicrophonePacketSize], packet)
}

func TestDS4MicrophoneQueuePrimesAndExposesRecoveryTelemetry(t *testing.T) {
	dev, err := New(nil)
	require.NoError(t, err)
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)

	frame := make([]byte, USBMicrophoneClientFrameSize)
	for index := range frame {
		frame[index] = byte(index + 1)
	}
	for count := 0; count < microphoneTargetClientFrames-1; count++ {
		dev.QueueMicrophonePCMFrame(frame)
	}

	packet := dev.HandleTransfer(context.Background(),
		uint32(EndpointMicrophoneIn&0x0F), usbip.DirIn, nil)
	assert.Equal(t, make([]byte, USBMicrophonePacketSize), packet)
	state := dev.GetDeviceSpecificArgs()
	assert.Equal(t, USBMicrophoneClientFrameSize*microphoneTargetClientFrames,
		state["microphoneQueueTargetBytes"])
	assert.Equal(t, USBMicrophoneClientFrameSize*microphoneMaximumClientFrames,
		state["microphoneQueueMaximumBytes"])
	assert.Equal(t, false, state["microphoneQueuePrimed"])

	dev.QueueMicrophonePCMFrame(frame)
	packet = dev.HandleTransfer(context.Background(),
		uint32(EndpointMicrophoneIn&0x0F), usbip.DirIn, nil)
	assert.Equal(t, frame[:USBMicrophonePacketSize], packet)

	packetsPerClientFrame := USBMicrophoneClientFrameSize / USBMicrophonePacketSize
	for count := 1; count < microphoneTargetClientFrames*packetsPerClientFrame; count++ {
		packet = dev.HandleTransfer(context.Background(),
			uint32(EndpointMicrophoneIn&0x0F), usbip.DirIn, nil)
		assert.NotEqual(t, make([]byte, USBMicrophonePacketSize), packet)
	}
	packet = dev.HandleTransfer(context.Background(),
		uint32(EndpointMicrophoneIn&0x0F), usbip.DirIn, nil)
	assert.Equal(t, make([]byte, USBMicrophonePacketSize), packet)
	state = dev.GetDeviceSpecificArgs()
	assert.Equal(t, uint64(1), state["microphoneUnderruns"])
	assert.Equal(t, uint64(0), state["microphoneReprimes"])
	assert.Equal(t, false, state["microphoneQueuePrimed"])

	for count := 0; count < microphoneTargetClientFrames; count++ {
		dev.QueueMicrophonePCMFrame(frame)
	}
	state = dev.GetDeviceSpecificArgs()
	assert.Equal(t, uint64(1), state["microphoneUnderruns"])
	assert.Equal(t, uint64(1), state["microphoneReprimes"])
	assert.Equal(t, true, state["microphoneQueuePrimed"])
}

func makeDualShock4StreamFrame(frameType byte, sequence uint32,
	payload []byte) []byte {
	header := make([]byte, StreamFrameHeaderSize)
	header[0] = StreamFrameMagic0
	header[1] = StreamFrameMagic1
	header[2] = StreamFrameMagic2
	header[3] = StreamFrameMagic3
	header[4] = StreamFrameVersionV3
	header[5] = frameType
	binary.LittleEndian.PutUint16(header[6:8], uint16(len(payload)))
	binary.LittleEndian.PutUint32(header[8:12], sequence)
	binary.LittleEndian.PutUint32(header[12:16],
		dualShock4FramedStreamCRC(header[4:12], payload))
	return append(header, payload...)
}

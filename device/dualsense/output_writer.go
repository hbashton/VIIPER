package dualsense

import (
	"encoding/binary"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dualSenseOutputControlQueueCapacity = 32
	dualSenseOutputAudioQueueCapacity   = 64
	// Windows submits the virtual DualSense audio endpoint in ten-packet
	// USB/IP URBs. Reserve the captured maximum packet size for every packet;
	// the normal 48 kHz four-channel payload is 3,840 bytes and becomes a
	// 1,920-byte native stereo speaker block.
	dualSenseSpeakerPayloadCapacity = USBHapticsAudioMaxPacketSize * 10 / 2
	dualSenseSpeakerTraceInterval   = 10 * time.Second
	dualSenseSpeakerResetTimeout    = 250 * time.Millisecond
)

type dualSenseSpeakerStreamTelemetry struct {
	receivedPayloads atomic.Uint64
	receivedBytes    atomic.Uint64
	enqueuedPayloads atomic.Uint64
	enqueuedBytes    atomic.Uint64
	droppedPayloads  atomic.Uint64
	droppedBytes     atomic.Uint64
	writtenPayloads  atomic.Uint64
	writtenBytes     atomic.Uint64
	writeFailures    atomic.Uint64
	queueDepth       atomic.Uint64
	queueHighWater   atomic.Uint64
	lastEnqueueNS    atomic.Int64
	maxEnqueueGapNS  atomic.Int64
	lastWriteNS      atomic.Int64
	maxWriteGapNS    atomic.Int64
	active           atomic.Bool
}

type dualSenseSpeakerStreamSnapshot struct {
	ReceivedPayloads uint64
	ReceivedBytes    uint64
	EnqueuedPayloads uint64
	EnqueuedBytes    uint64
	DroppedPayloads  uint64
	DroppedBytes     uint64
	WrittenPayloads  uint64
	WrittenBytes     uint64
	WriteFailures    uint64
	QueueDepth       uint64
	QueueHighWater   uint64
	MaxEnqueueGapUS  int64
	MaxWriteGapUS    int64
	Active           bool
}

func (s *dualSenseSpeakerStreamTelemetry) snapshot() dualSenseSpeakerStreamSnapshot {
	if s == nil {
		return dualSenseSpeakerStreamSnapshot{}
	}
	return dualSenseSpeakerStreamSnapshot{
		ReceivedPayloads: s.receivedPayloads.Load(),
		ReceivedBytes:    s.receivedBytes.Load(),
		EnqueuedPayloads: s.enqueuedPayloads.Load(),
		EnqueuedBytes:    s.enqueuedBytes.Load(),
		DroppedPayloads:  s.droppedPayloads.Load(),
		DroppedBytes:     s.droppedBytes.Load(),
		WrittenPayloads:  s.writtenPayloads.Load(),
		WrittenBytes:     s.writtenBytes.Load(),
		WriteFailures:    s.writeFailures.Load(),
		QueueDepth:       s.queueDepth.Load(),
		QueueHighWater:   s.queueHighWater.Load(),
		MaxEnqueueGapUS:  s.maxEnqueueGapNS.Load() / int64(time.Microsecond),
		MaxWriteGapUS:    s.maxWriteGapNS.Load() / int64(time.Microsecond),
		Active:           s.active.Load(),
	}
}

func recordMaximumInt64(target *atomic.Int64, value int64) {
	for value > 0 {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}

func recordMaximumUint64(target *atomic.Uint64, value uint64) {
	for value > 0 {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}

type dualSenseOutputFrame struct {
	frameType  byte
	payload    []byte
	audio      bool
	generation uint64
}

// dualSenseOutputWriter serializes controller feedback and virtual speaker
// PCM on one framed stream. USB isochronous completion must never wait for TCP
// backpressure, so speaker extraction uses a fixed pool and a bounded queue.
type dualSenseOutputWriter struct {
	conn            net.Conn
	version         byte
	logger          *slog.Logger
	telemetry       *dualSenseSpeakerStreamTelemetry
	control         chan dualSenseOutputFrame
	audio           chan dualSenseOutputFrame
	audioFree       chan []byte
	stop            chan struct{}
	done            chan struct{}
	stopOnce        sync.Once
	enqueueLock     sync.RWMutex
	audioWrite      sync.Mutex
	stopped         bool
	streamViable    atomic.Bool
	audioGeneration atomic.Uint64
	sequence        uint32
	packet          []byte
	lastTrace       time.Time
}

func newDualSenseOutputWriter(conn net.Conn, version byte,
	telemetry *dualSenseSpeakerStreamTelemetry, logger *slog.Logger) *dualSenseOutputWriter {
	if telemetry == nil {
		telemetry = &dualSenseSpeakerStreamTelemetry{}
	}
	telemetry.queueDepth.Store(0)
	telemetry.lastEnqueueNS.Store(0)
	telemetry.lastWriteNS.Store(0)
	telemetry.active.Store(true)
	w := &dualSenseOutputWriter{
		conn:      conn,
		version:   version,
		logger:    logger,
		telemetry: telemetry,
		control:   make(chan dualSenseOutputFrame, dualSenseOutputControlQueueCapacity),
		audio:     make(chan dualSenseOutputFrame, dualSenseOutputAudioQueueCapacity),
		audioFree: make(chan []byte, dualSenseOutputAudioQueueCapacity),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		packet:    make([]byte, 0, StreamFrameV2HeaderSize+dualSenseSpeakerPayloadCapacity),
		lastTrace: time.Now(),
	}
	w.streamViable.Store(conn != nil)
	for range dualSenseOutputAudioQueueCapacity {
		w.audioFree <- make([]byte, dualSenseSpeakerPayloadCapacity)
	}
	return w
}

func (w *dualSenseOutputWriter) EnqueueControl(frameType byte, payload []byte) {
	if len(payload) == 0 {
		return
	}
	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped {
		return
	}
	w.enqueueFrameLocked(w.control, dualSenseOutputFrame{
		frameType: frameType,
		payload:   append([]byte(nil), payload...),
	})
}

// EnqueueSpeakerFromUSB extracts front-left/front-right from the native
// DualSense 48 kHz, four-channel S16LE endpoint. Rear-left/rear-right remain
// exclusively on the advanced-haptics lane. The extraction itself performs no
// allocation after writer construction.
func (w *dualSenseOutputWriter) EnqueueSpeakerFromUSB(usbPCM []byte) {
	const usbFrameSize = USBHapticsAudioFrameSize
	const speakerFrameSize = 2 * USBHapticsAudioBytesPerSample

	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped {
		return
	}

	framesRemaining := len(usbPCM) / usbFrameSize
	if framesRemaining == 0 {
		return
	}
	w.telemetry.receivedPayloads.Add(1)
	w.telemetry.receivedBytes.Add(uint64(framesRemaining * speakerFrameSize))
	sourceOffset := 0
	for framesRemaining > 0 {
		var buffer []byte
		select {
		case buffer = <-w.audioFree:
		default:
			w.recordSpeakerDrop(framesRemaining * speakerFrameSize)
			return
		}

		frames := min(framesRemaining, cap(buffer)/speakerFrameSize)
		length := copyDualSenseSpeakerChannels(buffer,
			usbPCM[sourceOffset:sourceOffset+frames*usbFrameSize])
		frame := dualSenseOutputFrame{
			frameType:  StreamFrameSpeakerPCM,
			payload:    buffer[:length],
			audio:      true,
			generation: w.audioGeneration.Load(),
		}
		if !w.enqueueFrameLocked(w.audio, frame) {
			w.audioFree <- buffer[:cap(buffer)]
			w.recordSpeakerDrop(framesRemaining * speakerFrameSize)
			return
		}
		w.recordSpeakerEnqueue(length)

		framesRemaining -= frames
		sourceOffset += frames * usbFrameSize
	}
}

func (w *dualSenseOutputWriter) recordSpeakerEnqueue(length int) {
	w.telemetry.enqueuedPayloads.Add(1)
	w.telemetry.enqueuedBytes.Add(uint64(length))
	now := time.Now().UnixNano()
	previous := w.telemetry.lastEnqueueNS.Swap(now)
	if previous > 0 && now > previous {
		recordMaximumInt64(&w.telemetry.maxEnqueueGapNS, now-previous)
	}
	depth := uint64(len(w.audio))
	w.telemetry.queueDepth.Store(depth)
	recordMaximumUint64(&w.telemetry.queueHighWater, depth)
}

func (w *dualSenseOutputWriter) recordSpeakerDrop(length int) {
	if length <= 0 {
		return
	}
	w.telemetry.droppedPayloads.Add(1)
	w.telemetry.droppedBytes.Add(uint64(length))
}

func (w *dualSenseOutputWriter) recordSpeakerWrite(length int) {
	w.telemetry.writtenPayloads.Add(1)
	w.telemetry.writtenBytes.Add(uint64(length))
	now := time.Now().UnixNano()
	previous := w.telemetry.lastWriteNS.Swap(now)
	if previous > 0 && now > previous {
		recordMaximumInt64(&w.telemetry.maxWriteGapNS, now-previous)
	}
}

// copyDualSenseSpeakerChannels copies the first stereo pair from interleaved
// four-channel S16LE PCM into dst and returns the number of bytes written.
func copyDualSenseSpeakerChannels(dst, src []byte) int {
	const usbFrameSize = USBHapticsAudioFrameSize
	const speakerFrameSize = 2 * USBHapticsAudioBytesPerSample

	frames := min(len(src)/usbFrameSize, len(dst)/speakerFrameSize)
	for frame := 0; frame < frames; frame++ {
		source := frame * usbFrameSize
		destination := frame * speakerFrameSize
		copy(dst[destination:destination+speakerFrameSize],
			src[source:source+speakerFrameSize])
	}
	return frames * speakerFrameSize
}

// enqueueFrameLocked requires enqueueLock to be held for reading. Shutdown
// takes the write side before draining, so no producer can publish a frame
// after the final drain has observed an empty queue.
func (w *dualSenseOutputWriter) enqueueFrameLocked(queue chan dualSenseOutputFrame,
	frame dualSenseOutputFrame) bool {
	select {
	case queue <- frame:
		return true
	default:
		// Do not let TCP backpressure delay a USB/IP isochronous completion.
		return false
	}
}

func (w *dualSenseOutputWriter) Run() {
	defer func() {
		w.requestStop()
		w.drainAudioQueue()
		w.telemetry.queueDepth.Store(0)
		w.telemetry.active.Store(false)
		w.traceSpeakerState(true)
		close(w.done)
	}()
	preferAudio := false
	for {
		// Alternate when both lanes are continuously ready. If the preferred
		// lane is empty, immediately service whichever frame arrives next.
		if preferAudio {
			select {
			case frame := <-w.audio:
				if !w.writeAndRelease(frame) {
					return
				}
				preferAudio = false
				continue
			default:
			}
		} else {
			select {
			case frame := <-w.control:
				if !w.writeAndRelease(frame) {
					return
				}
				preferAudio = true
				continue
			default:
			}
		}

		select {
		case <-w.stop:
			return
		case frame := <-w.control:
			if !w.writeAndRelease(frame) {
				return
			}
			preferAudio = true
		case frame := <-w.audio:
			if !w.writeAndRelease(frame) {
				return
			}
			preferAudio = false
		}
	}
}

func (w *dualSenseOutputWriter) writeAndRelease(frame dualSenseOutputFrame) bool {
	if frame.audio {
		w.audioWrite.Lock()
		defer w.audioWrite.Unlock()
		if frame.generation != w.audioGeneration.Load() {
			w.recordSpeakerDrop(len(frame.payload))
			w.release(frame)
			w.telemetry.queueDepth.Store(uint64(len(w.audio)))
			return true
		}
	}

	ok := w.write(frame)
	if frame.audio {
		w.telemetry.queueDepth.Store(uint64(len(w.audio)))
		if ok {
			w.recordSpeakerWrite(len(frame.payload))
		} else {
			w.telemetry.writeFailures.Add(1)
		}
	}
	w.release(frame)
	if frame.audio {
		w.traceSpeakerState(false)
	}
	return ok
}

// ResetSpeaker advances the audio generation and flushes every queued frame.
// It waits for an already-started write before returning, making interface and
// endpoint resets a hard barrier between USB presentation generations.
func (w *dualSenseOutputWriter) ResetSpeaker() {
	w.enqueueLock.Lock()
	w.audioGeneration.Add(1)
	w.drainAudioQueue()
	w.enqueueLock.Unlock()

	// A peer that has stopped reading can otherwise hold audioWrite forever.
	// Bound the old generation's in-flight write; write() closes a timed-out
	// stream so the owning handler can return and accept a replacement.
	if w.conn != nil {
		if err := w.conn.SetWriteDeadline(time.Now().Add(dualSenseSpeakerResetTimeout)); err != nil {
			w.invalidateStream()
		}
	}
	w.audioWrite.Lock()
	if w.conn != nil && w.streamViable.Load() {
		if err := w.conn.SetWriteDeadline(time.Time{}); err != nil {
			w.invalidateStream()
		}
	}
	w.audioWrite.Unlock()
	w.telemetry.queueDepth.Store(0)
}

func (w *dualSenseOutputWriter) drainAudioQueue() {
	for {
		select {
		case frame := <-w.audio:
			w.release(frame)
		default:
			return
		}
	}
}

func (w *dualSenseOutputWriter) traceSpeakerState(final bool) {
	if w.logger == nil {
		return
	}
	now := time.Now()
	if !final && now.Sub(w.lastTrace) < dualSenseSpeakerTraceInterval {
		return
	}
	w.lastTrace = now
	state := w.telemetry.snapshot()
	log := w.logger.Debug
	message := "DualSense V3 speaker stream"
	if final {
		log = w.logger.Info
		message = "DualSense V3 speaker stream stopped"
	}
	log(message,
		"receivedPayloads", state.ReceivedPayloads,
		"receivedBytes", state.ReceivedBytes,
		"enqueuedPayloads", state.EnqueuedPayloads,
		"enqueuedBytes", state.EnqueuedBytes,
		"droppedPayloads", state.DroppedPayloads,
		"droppedBytes", state.DroppedBytes,
		"writtenPayloads", state.WrittenPayloads,
		"writtenBytes", state.WrittenBytes,
		"writeFailures", state.WriteFailures,
		"queueDepth", state.QueueDepth,
		"queueHighWater", state.QueueHighWater,
		"maxEnqueueGapUS", state.MaxEnqueueGapUS,
		"maxWriteGapUS", state.MaxWriteGapUS)
}

func (w *dualSenseOutputWriter) write(frame dualSenseOutputFrame) bool {
	if len(frame.payload) > int(^uint16(0)) {
		return true
	}
	packetLength := StreamFrameV2HeaderSize + len(frame.payload)
	if cap(w.packet) < packetLength {
		w.packet = make([]byte, packetLength)
	} else {
		w.packet = w.packet[:packetLength]
	}
	header := w.packet[:StreamFrameV2HeaderSize]
	header[0] = StreamFrameMagic0
	header[1] = StreamFrameMagic1
	header[2] = StreamFrameMagic2
	header[3] = StreamFrameMagic3
	header[4] = w.version
	header[5] = frame.frameType
	binary.LittleEndian.PutUint16(header[6:8], uint16(len(frame.payload)))
	binary.LittleEndian.PutUint32(header[8:12], w.sequence)
	w.sequence++
	binary.LittleEndian.PutUint32(header[12:16],
		framedStreamCRC(header[4:12], frame.payload))
	copy(w.packet[StreamFrameV2HeaderSize:], frame.payload)

	remaining := w.packet
	for len(remaining) > 0 {
		n, err := w.conn.Write(remaining)
		if err != nil || n <= 0 {
			w.invalidateStream()
			return false
		}
		remaining = remaining[n:]
	}
	return true
}

func (w *dualSenseOutputWriter) release(frame dualSenseOutputFrame) {
	if frame.audio {
		w.audioFree <- frame.payload[:cap(frame.payload)]
	}
}

func (w *dualSenseOutputWriter) Stop() {
	w.requestStop()
	if w.conn != nil {
		_ = w.conn.SetWriteDeadline(time.Now().Add(dualSenseSpeakerResetTimeout))
		_ = w.conn.Close()
	}
	select {
	case <-w.done:
	case <-time.After(300 * time.Millisecond):
	}
}

func (w *dualSenseOutputWriter) requestStop() {
	w.stopOnce.Do(func() {
		w.streamViable.Store(false)
		w.enqueueLock.Lock()
		w.stopped = true
		close(w.stop)
		w.enqueueLock.Unlock()
	})
}

func (w *dualSenseOutputWriter) invalidateStream() {
	w.streamViable.Store(false)
	if w.conn != nil {
		_ = w.conn.Close()
	}
}

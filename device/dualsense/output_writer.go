package dualsense

import (
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dualSenseOutputControlQueueCapacity = 32
	dualSenseMediaMaximumBufferTime     = 200 * time.Millisecond
	dualSenseSpeakerGenerationCadence   = time.Second * dualSenseV5SpeakerFrames /
		USBHapticsAudioSampleRate
	dualSenseRealtimeHapticsCadence = time.Second *
		(BluetoothHapticsSampleSize / 2) / BluetoothHapticsSampleRate
	// Twenty 10 ms speaker generations are the largest possible frame count.
	// Enqueue also accounts exact per-frame duration, so the 10.667 ms realtime
	// lane and mixed media remain within the same 200 ms ceiling.
	dualSenseOutputAudioQueueCapacity = int(
		dualSenseMediaMaximumBufferTime / dualSenseSpeakerGenerationCadence)
	dualSenseRealtimeMediaQueueCapacity = int(
		dualSenseMediaMaximumBufferTime / dualSenseRealtimeHapticsCadence)
	// One additional buffer belongs to the sole in-flight socket write while
	// the full 200 ms queue remains available to producers.
	dualSenseOutputAudioPoolCapacity = dualSenseOutputAudioQueueCapacity + 1
	// V5 carries one 480-frame V5 generation: the combined feedback and
	// its matching front-channel stereo PCM.
	dualSenseSpeakerPayloadCapacity = dualSenseAtomicFeedbackPrefix +
		OutputStateV5Size + dualSenseV5SpeakerPayloadSize
	dualSenseSpeakerTraceInterval = 10 * time.Second
	dualSenseSpeakerResetTimeout  = 250 * time.Millisecond
	dualSenseOutputJoinTimeout    = 300 * time.Millisecond
	dualSenseAtomicFeedbackPrefix = 2
)

var errDualSenseOutputJoinTimeout = errors.New(
	"DualSense output writer did not stop before the join deadline")

type dualSenseSpeakerStreamTelemetry struct {
	orderedReceived                 atomic.Uint64
	orderedEnqueued                 atomic.Uint64
	orderedRejected                 atomic.Uint64
	orderedWritten                  atomic.Uint64
	orderedSaturations              atomic.Uint64
	orderedQueueDepth               atomic.Uint64
	orderedQueueHighWater           atomic.Uint64
	orderedLifecycleDiscardedFrames atomic.Uint64
	orderedLifecycleDiscardedBytes  atomic.Uint64
	receivedPayloads                atomic.Uint64
	receivedBytes                   atomic.Uint64
	enqueuedPayloads                atomic.Uint64
	enqueuedBytes                   atomic.Uint64
	rejectedPayloads                atomic.Uint64
	rejectedBytes                   atomic.Uint64
	droppedPayloads                 atomic.Uint64
	droppedBytes                    atomic.Uint64
	overruns                        atomic.Uint64
	underruns                       atomic.Uint64
	lateGaps                        atomic.Uint64
	stalePayloads                   atomic.Uint64
	staleBytes                      atomic.Uint64
	lifecycleDiscardedPayloads      atomic.Uint64
	lifecycleDiscardedBytes         atomic.Uint64
	writtenPayloads                 atomic.Uint64
	writtenBytes                    atomic.Uint64
	writeFailures                   atomic.Uint64
	orderedWriteFailures            atomic.Uint64
	queueDepth                      atomic.Uint64
	queueHighWater                  atomic.Uint64
	queueDurationNS                 atomic.Int64
	queueDurationHighNS             atomic.Int64
	lastEnqueueNS                   atomic.Int64
	lastRealtimeEnqueueNS           atomic.Int64
	maxEnqueueGapNS                 atomic.Int64
	lastWriteNS                     atomic.Int64
	maxWriteGapNS                   atomic.Int64
	active                          atomic.Bool
	teardownFailures                atomic.Uint64
	teardownPending                 atomic.Bool
}

type dualSenseSpeakerStreamSnapshot struct {
	OrderedReceived                 uint64
	OrderedEnqueued                 uint64
	OrderedRejected                 uint64
	OrderedWritten                  uint64
	OrderedSaturations              uint64
	OrderedQueueDepth               uint64
	OrderedQueueHighWater           uint64
	OrderedLifecycleDiscardedFrames uint64
	OrderedLifecycleDiscardedBytes  uint64
	ReceivedPayloads                uint64
	ReceivedBytes                   uint64
	EnqueuedPayloads                uint64
	EnqueuedBytes                   uint64
	RejectedPayloads                uint64
	RejectedBytes                   uint64
	DroppedPayloads                 uint64
	DroppedBytes                    uint64
	Overruns                        uint64
	Underruns                       uint64
	LateGaps                        uint64
	StalePayloads                   uint64
	StaleBytes                      uint64
	LifecycleDiscardedPayloads      uint64
	LifecycleDiscardedBytes         uint64
	WrittenPayloads                 uint64
	WrittenBytes                    uint64
	WriteFailures                   uint64
	OrderedWriteFailures            uint64
	QueueDepth                      uint64
	QueueHighWater                  uint64
	QueueDurationUS                 int64
	QueueDurationHighUS             int64
	MaxEnqueueGapUS                 int64
	MaxWriteGapUS                   int64
	Active                          bool
	TeardownFailures                uint64
	TeardownPending                 bool
}

func (s *dualSenseSpeakerStreamTelemetry) snapshot() dualSenseSpeakerStreamSnapshot {
	if s == nil {
		return dualSenseSpeakerStreamSnapshot{}
	}
	return dualSenseSpeakerStreamSnapshot{
		OrderedReceived:                 s.orderedReceived.Load(),
		OrderedEnqueued:                 s.orderedEnqueued.Load(),
		OrderedRejected:                 s.orderedRejected.Load(),
		OrderedWritten:                  s.orderedWritten.Load(),
		OrderedSaturations:              s.orderedSaturations.Load(),
		OrderedQueueDepth:               s.orderedQueueDepth.Load(),
		OrderedQueueHighWater:           s.orderedQueueHighWater.Load(),
		OrderedLifecycleDiscardedFrames: s.orderedLifecycleDiscardedFrames.Load(),
		OrderedLifecycleDiscardedBytes:  s.orderedLifecycleDiscardedBytes.Load(),
		ReceivedPayloads:                s.receivedPayloads.Load(),
		ReceivedBytes:                   s.receivedBytes.Load(),
		EnqueuedPayloads:                s.enqueuedPayloads.Load(),
		EnqueuedBytes:                   s.enqueuedBytes.Load(),
		RejectedPayloads:                s.rejectedPayloads.Load(),
		RejectedBytes:                   s.rejectedBytes.Load(),
		DroppedPayloads:                 s.droppedPayloads.Load(),
		DroppedBytes:                    s.droppedBytes.Load(),
		Overruns:                        s.overruns.Load(),
		Underruns:                       s.underruns.Load(),
		LateGaps:                        s.lateGaps.Load(),
		StalePayloads:                   s.stalePayloads.Load(),
		StaleBytes:                      s.staleBytes.Load(),
		LifecycleDiscardedPayloads:      s.lifecycleDiscardedPayloads.Load(),
		LifecycleDiscardedBytes:         s.lifecycleDiscardedBytes.Load(),
		WrittenPayloads:                 s.writtenPayloads.Load(),
		WrittenBytes:                    s.writtenBytes.Load(),
		WriteFailures:                   s.writeFailures.Load(),
		OrderedWriteFailures:            s.orderedWriteFailures.Load(),
		QueueDepth:                      s.queueDepth.Load(),
		QueueHighWater:                  s.queueHighWater.Load(),
		QueueDurationUS:                 s.queueDurationNS.Load() / int64(time.Microsecond),
		QueueDurationHighUS:             s.queueDurationHighNS.Load() / int64(time.Microsecond),
		MaxEnqueueGapUS:                 s.maxEnqueueGapNS.Load() / int64(time.Microsecond),
		MaxWriteGapUS:                   s.maxWriteGapNS.Load() / int64(time.Microsecond),
		Active:                          s.active.Load(),
		TeardownFailures:                s.teardownFailures.Load(),
		TeardownPending:                 s.teardownPending.Load(),
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
	frameType byte
	payload   []byte
	// media participates in the reset generation and bounded-time queue.
	// audio marks a preallocated atomic speaker buffer that must be returned.
	media         bool
	audio         bool
	mediaBytes    int
	mediaDuration time.Duration
	generation    uint64
	publication   uint64
}

// dualSenseOutputWriter serializes controller feedback and virtual speaker
// PCM on one framed stream. USB isochronous completion must never wait for TCP
// backpressure, so speaker extraction uses a fixed pool and a bounded queue.
type dualSenseOutputWriter struct {
	conn      net.Conn
	logger    *slog.Logger
	telemetry *dualSenseSpeakerStreamTelemetry
	control   chan dualSenseOutputFrame
	// audio is the single media FIFO for atomic speaker/haptics and realtime
	// haptics. One FIFO preserves callback publication order across both clocks.
	audio               chan dualSenseOutputFrame
	audioFree           chan []byte
	stop                chan struct{}
	done                chan struct{}
	stopOnce            sync.Once
	enqueueLock         sync.RWMutex
	controlEnqueue      sync.Mutex
	mediaEnqueue        sync.Mutex
	audioWrite          sync.Mutex
	stopped             bool
	streamViable        atomic.Bool
	accepting           atomic.Bool
	audioGeneration     atomic.Uint64
	orderedPublication  uint64
	sequence            uint32
	packet              []byte
	lastTrace           time.Time
	teardownMu          sync.Mutex
	teardownErr         error
	teardownFailureOnce sync.Once
	teardownJoinOnce    sync.Once
}

func newDualSenseOutputWriter(conn net.Conn,
	telemetry *dualSenseSpeakerStreamTelemetry, logger *slog.Logger) *dualSenseOutputWriter {
	if telemetry == nil {
		telemetry = &dualSenseSpeakerStreamTelemetry{}
	}
	telemetry.queueDepth.Store(0)
	telemetry.queueDurationNS.Store(0)
	telemetry.orderedQueueDepth.Store(0)
	telemetry.lastEnqueueNS.Store(0)
	telemetry.lastRealtimeEnqueueNS.Store(0)
	telemetry.lastWriteNS.Store(0)
	telemetry.active.Store(true)
	w := &dualSenseOutputWriter{
		conn:      conn,
		logger:    logger,
		telemetry: telemetry,
		control:   make(chan dualSenseOutputFrame, dualSenseOutputControlQueueCapacity),
		audio:     make(chan dualSenseOutputFrame, dualSenseOutputAudioQueueCapacity),
		audioFree: make(chan []byte, dualSenseOutputAudioPoolCapacity),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		packet:    make([]byte, 0, StreamFrameHeaderSize+dualSenseSpeakerPayloadCapacity),
		lastTrace: time.Now(),
	}
	w.streamViable.Store(conn != nil)
	w.accepting.Store(true)
	for range dualSenseOutputAudioPoolCapacity {
		w.audioFree <- make([]byte, dualSenseSpeakerPayloadCapacity)
	}
	return w
}

// EnqueueRealtimeHaptics keeps time-bearing rear-channel generations out of
// the ordinary state queue. Games can issue dense trigger/LED SET_REPORT
// traffic; that traffic must never delay or evict the 93.75 Hz haptics clock.
func (w *dualSenseOutputWriter) EnqueueRealtimeHaptics(payload []byte) {
	if len(payload) == 0 {
		return
	}
	w.mediaEnqueue.Lock()
	defer w.mediaEnqueue.Unlock()
	w.recordSpeakerReceive(len(payload), StreamFrameRealtimeHaptics)
	if !w.accepting.Load() {
		w.recordSpeakerRejected(len(payload))
		return
	}
	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped || !w.accepting.Load() {
		w.recordSpeakerRejected(len(payload))
		return
	}
	w.enqueueMediaDropOldestLocked(dualSenseOutputFrame{
		frameType:     StreamFrameRealtimeHaptics,
		payload:       append([]byte(nil), payload...),
		media:         true,
		mediaBytes:    len(payload),
		mediaDuration: dualSenseRealtimeHapticsCadence,
		generation:    w.audioGeneration.Load(),
	})
}

func (w *dualSenseOutputWriter) EnqueueControl(frameType byte, payload []byte) {
	if len(payload) == 0 {
		return
	}
	w.controlEnqueue.Lock()
	defer w.controlEnqueue.Unlock()
	w.telemetry.orderedReceived.Add(1)
	if !w.accepting.Load() {
		w.telemetry.orderedRejected.Add(1)
		return
	}
	frame := dualSenseOutputFrame{
		frameType: frameType,
		payload:   append([]byte(nil), payload...),
	}
	w.enqueueLock.RLock()
	if w.stopped || !w.accepting.Load() {
		w.telemetry.orderedRejected.Add(1)
		w.enqueueLock.RUnlock()
		return
	}
	w.orderedPublication++
	frame.publication = w.orderedPublication
	depth := w.telemetry.orderedQueueDepth.Add(1)
	select {
	case w.control <- frame:
		w.telemetry.orderedEnqueued.Add(1)
		recordMaximumUint64(&w.telemetry.orderedQueueHighWater, depth)
		w.enqueueLock.RUnlock()
		return
	default:
		decrementUint64(&w.telemetry.orderedQueueDepth)
	}
	w.telemetry.orderedRejected.Add(1)
	if !w.accepting.CompareAndSwap(true, false) {
		w.enqueueLock.RUnlock()
		return
	}
	w.telemetry.orderedSaturations.Add(1)
	w.enqueueLock.RUnlock()
	w.faultStream("ordered output queue saturated")
}

// EnqueueAtomicAudioHaptics publishes one V5 generation. A little-endian
// feedback length prefixes the native combined feedback; the remaining bytes
// are exactly 480 matching stereo PCM frames.
func (w *dualSenseOutputWriter) EnqueueAtomicAudioHaptics(feedback, speakerPCM []byte) {
	if len(feedback) == 0 || len(feedback) > int(^uint16(0)) ||
		len(speakerPCM) == 0 {
		return
	}
	if len(speakerPCM) != dualSenseV5SpeakerPayloadSize {
		return
	}

	w.mediaEnqueue.Lock()
	defer w.mediaEnqueue.Unlock()
	w.recordSpeakerReceive(len(speakerPCM), StreamFrameAtomicAudioHaptics)
	if !w.accepting.Load() {
		w.recordSpeakerRejected(len(speakerPCM))
		return
	}
	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped || !w.accepting.Load() {
		w.recordSpeakerRejected(len(speakerPCM))
		return
	}
	buffer := w.acquireAtomicAudioBuffer()
	if buffer == nil {
		w.recordSpeakerOverrun(len(speakerPCM))
		return
	}

	length := dualSenseAtomicFeedbackPrefix + len(feedback) + len(speakerPCM)
	if length > cap(buffer) {
		w.audioFree <- buffer[:cap(buffer)]
		w.recordSpeakerOverrun(len(speakerPCM))
		return
	}
	buffer = buffer[:length]
	binary.LittleEndian.PutUint16(buffer[:dualSenseAtomicFeedbackPrefix],
		uint16(len(feedback)))
	copy(buffer[dualSenseAtomicFeedbackPrefix:], feedback)
	copy(buffer[dualSenseAtomicFeedbackPrefix+len(feedback):], speakerPCM)
	frame := dualSenseOutputFrame{
		frameType:     StreamFrameAtomicAudioHaptics,
		payload:       buffer,
		media:         true,
		audio:         true,
		mediaBytes:    len(speakerPCM),
		mediaDuration: dualSenseSpeakerGenerationCadence,
		generation:    w.audioGeneration.Load(),
	}
	w.enqueueMediaDropOldestLocked(frame)
}

// acquireAtomicAudioBuffer keeps V5 realtime: when TCP momentarily falls
// behind and every fixed pool
// buffer is owned, evict the oldest queued (not in-flight) media generation so
// the newest native USB generation can still be published without growing an
// unbounded stale-audio reserve.
func (w *dualSenseOutputWriter) acquireAtomicAudioBuffer() []byte {
	for {
		select {
		case buffer := <-w.audioFree:
			return buffer
		default:
		}

		select {
		case oldest := <-w.audio:
			w.recordMediaDequeued(oldest)
			w.recordSpeakerOverrun(oldest.mediaBytes)
			w.release(oldest)
		default:
			// Every preallocated buffer is either queued or in-flight. Only a
			// queued frame may be reclaimed; never steal the in-flight buffer.
			return nil
		}
	}
}

func (w *dualSenseOutputWriter) recordSpeakerReceive(length int, frameType byte) {
	w.telemetry.receivedPayloads.Add(1)
	w.telemetry.receivedBytes.Add(uint64(length))
	now := time.Now().UnixNano()
	last := &w.telemetry.lastEnqueueNS
	cadence := dualSenseSpeakerGenerationCadence
	if frameType == StreamFrameRealtimeHaptics {
		last = &w.telemetry.lastRealtimeEnqueueNS
		cadence = dualSenseRealtimeHapticsCadence
	}
	previous := last.Swap(now)
	if previous <= 0 || now <= previous {
		return
	}
	gap := now - previous
	recordMaximumInt64(&w.telemetry.maxEnqueueGapNS, gap)
	expected := int64(cadence)
	// This is an observed producer-generation gap, not a claim about remote
	// playback. A 50% tolerance avoids classifying scheduler jitter as loss.
	if gap > expected+expected/2 {
		w.telemetry.lateGaps.Add(1)
		missing := uint64(gap/expected) - 1
		if missing == 0 {
			missing = 1
		}
		w.telemetry.underruns.Add(missing)
	}
}

func (w *dualSenseOutputWriter) recordSpeakerEnqueue(length int,
	depth uint64, duration int64) {
	w.telemetry.enqueuedPayloads.Add(1)
	w.telemetry.enqueuedBytes.Add(uint64(length))
	recordMaximumUint64(&w.telemetry.queueHighWater, depth)
	recordMaximumInt64(&w.telemetry.queueDurationHighNS, duration)
}

func (w *dualSenseOutputWriter) recordSpeakerRejected(length int) {
	w.telemetry.rejectedPayloads.Add(1)
	w.telemetry.rejectedBytes.Add(uint64(length))
}

func (w *dualSenseOutputWriter) recordSpeakerOverrun(length int) {
	if length <= 0 {
		return
	}
	w.telemetry.overruns.Add(1)
	w.telemetry.droppedPayloads.Add(1)
	w.telemetry.droppedBytes.Add(uint64(length))
}

func (w *dualSenseOutputWriter) recordSpeakerStale(length int) {
	if length <= 0 {
		return
	}
	w.telemetry.stalePayloads.Add(1)
	w.telemetry.staleBytes.Add(uint64(length))
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

// enqueueMediaDropOldestLocked retains the newest bounded media horizon. The
// frame removed from the channel is necessarily queued, never in-flight.
func (w *dualSenseOutputWriter) enqueueMediaDropOldestLocked(
	frame dualSenseOutputFrame) {
	duration := int64(frame.mediaDuration)
	for w.telemetry.queueDurationNS.Load()+duration > int64(dualSenseMediaMaximumBufferTime) ||
		w.telemetry.queueDepth.Load() >= uint64(cap(w.audio)) {
		select {
		case oldest := <-w.audio:
			w.recordMediaDequeued(oldest)
			w.recordSpeakerOverrun(oldest.mediaBytes)
			w.release(oldest)
		default:
			w.recordSpeakerOverrun(frame.mediaBytes)
			w.release(frame)
			return
		}
	}
	reservedDuration := w.telemetry.queueDurationNS.Add(duration)
	depth := w.telemetry.queueDepth.Add(1)
	select {
	case w.audio <- frame:
		w.recordSpeakerEnqueue(frame.mediaBytes, depth, reservedDuration)
	default:
		w.telemetry.queueDurationNS.Add(-duration)
		decrementUint64(&w.telemetry.queueDepth)
		w.recordSpeakerOverrun(frame.mediaBytes)
		w.release(frame)
	}
}

func (w *dualSenseOutputWriter) recordMediaDequeued(frame dualSenseOutputFrame) {
	decrementUint64(&w.telemetry.queueDepth)
	if frame.mediaDuration > 0 {
		w.telemetry.queueDurationNS.Add(-int64(frame.mediaDuration))
	}
}

func (w *dualSenseOutputWriter) recordSpeakerLifecycleDiscard(length int) {
	if length <= 0 {
		return
	}
	w.telemetry.lifecycleDiscardedPayloads.Add(1)
	w.telemetry.lifecycleDiscardedBytes.Add(uint64(length))
}

func (w *dualSenseOutputWriter) Run() {
	defer func() {
		w.requestStop()
		w.drainControlQueue()
		w.drainAudioQueue(dualSenseMediaDiscardLifecycle)
		w.telemetry.active.Store(false)
		w.traceSpeakerState(true)
		close(w.done)
	}()
	preferAudio := false
	for {
		select {
		case <-w.stop:
			return
		default:
		}
		// Alternate when both traffic classes are continuously ready. If the preferred
		// lane is empty, immediately service whichever frame arrives next.
		if preferAudio {
			select {
			case frame := <-w.audio:
				w.recordMediaDequeued(frame)
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
				decrementUint64(&w.telemetry.orderedQueueDepth)
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
			decrementUint64(&w.telemetry.orderedQueueDepth)
			if !w.writeAndRelease(frame) {
				return
			}
			preferAudio = true
		case frame := <-w.audio:
			w.recordMediaDequeued(frame)
			if !w.writeAndRelease(frame) {
				return
			}
			preferAudio = false
		}
	}
}

func (w *dualSenseOutputWriter) writeAndRelease(frame dualSenseOutputFrame) bool {
	if frame.media {
		w.audioWrite.Lock()
		defer w.audioWrite.Unlock()
		if frame.generation != w.audioGeneration.Load() {
			w.recordSpeakerStale(frame.mediaBytes)
			w.release(frame)
			return true
		}
	}

	ok := w.write(frame)
	if frame.media {
		if ok {
			w.recordSpeakerWrite(frame.mediaBytes)
		} else {
			w.telemetry.writeFailures.Add(1)
		}
	} else if !ok {
		w.telemetry.orderedWriteFailures.Add(1)
	} else {
		w.telemetry.orderedWritten.Add(1)
	}
	w.release(frame)
	if frame.media {
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
	w.drainAudioQueue(dualSenseMediaDiscardStale)
	w.telemetry.lastEnqueueNS.Store(0)
	w.telemetry.lastRealtimeEnqueueNS.Store(0)
	w.telemetry.lastWriteNS.Store(0)
	w.enqueueLock.Unlock()

	// A peer that has stopped reading can otherwise hold audioWrite forever.
	// Bound the old generation's in-flight write; write() closes a timed-out
	// stream so the owning handler can return and accept a replacement.
	if w.conn != nil {
		if err := w.conn.SetWriteDeadline(time.Now().Add(dualSenseSpeakerResetTimeout)); err != nil {
			w.faultStream("speaker reset deadline failed")
		}
	}
	w.audioWrite.Lock()
	if w.conn != nil && w.streamViable.Load() {
		if err := w.conn.SetWriteDeadline(time.Time{}); err != nil {
			w.faultStream("speaker reset deadline clear failed")
		}
	}
	w.audioWrite.Unlock()
}

type dualSenseMediaDiscardReason uint8

const (
	dualSenseMediaDiscardStale dualSenseMediaDiscardReason = iota + 1
	dualSenseMediaDiscardLifecycle
)

func (w *dualSenseOutputWriter) drainControlQueue() {
	for {
		select {
		case frame := <-w.control:
			decrementUint64(&w.telemetry.orderedQueueDepth)
			w.telemetry.orderedLifecycleDiscardedFrames.Add(1)
			w.telemetry.orderedLifecycleDiscardedBytes.Add(
				uint64(len(frame.payload)))
			w.release(frame)
		default:
			return
		}
	}
}

func (w *dualSenseOutputWriter) drainAudioQueue(
	reason dualSenseMediaDiscardReason) {
	for {
		select {
		case frame := <-w.audio:
			w.recordMediaDequeued(frame)
			switch reason {
			case dualSenseMediaDiscardStale:
				w.recordSpeakerStale(frame.mediaBytes)
			case dualSenseMediaDiscardLifecycle:
				w.recordSpeakerLifecycleDiscard(frame.mediaBytes)
			}
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
	message := "DualSense framed speaker stream"
	if final {
		log = w.logger.Info
		message = "DualSense framed speaker stream stopped"
	}
	log(message,
		"receivedPayloads", state.ReceivedPayloads,
		"receivedBytes", state.ReceivedBytes,
		"enqueuedPayloads", state.EnqueuedPayloads,
		"enqueuedBytes", state.EnqueuedBytes,
		"rejectedPayloads", state.RejectedPayloads,
		"rejectedBytes", state.RejectedBytes,
		"droppedPayloads", state.DroppedPayloads,
		"droppedBytes", state.DroppedBytes,
		"overruns", state.Overruns,
		"underruns", state.Underruns,
		"lateGaps", state.LateGaps,
		"stalePayloads", state.StalePayloads,
		"staleBytes", state.StaleBytes,
		"lifecycleDiscardedPayloads", state.LifecycleDiscardedPayloads,
		"lifecycleDiscardedBytes", state.LifecycleDiscardedBytes,
		"writtenPayloads", state.WrittenPayloads,
		"writtenBytes", state.WrittenBytes,
		"writeFailures", state.WriteFailures,
		"orderedReceived", state.OrderedReceived,
		"orderedEnqueued", state.OrderedEnqueued,
		"orderedRejected", state.OrderedRejected,
		"orderedWritten", state.OrderedWritten,
		"orderedSaturations", state.OrderedSaturations,
		"orderedWriteFailures", state.OrderedWriteFailures,
		"orderedLifecycleDiscardedFrames",
		state.OrderedLifecycleDiscardedFrames,
		"orderedLifecycleDiscardedBytes", state.OrderedLifecycleDiscardedBytes,
		"queueDepth", state.QueueDepth,
		"queueHighWater", state.QueueHighWater,
		"queueDurationUS", state.QueueDurationUS,
		"queueDurationHighUS", state.QueueDurationHighUS,
		"maxEnqueueGapUS", state.MaxEnqueueGapUS,
		"maxWriteGapUS", state.MaxWriteGapUS,
		"teardownFailures", state.TeardownFailures,
		"teardownPending", state.TeardownPending)
}

func (w *dualSenseOutputWriter) write(frame dualSenseOutputFrame) bool {
	if len(frame.payload) > int(^uint16(0)) {
		return true
	}
	packetLength := StreamFrameHeaderSize + len(frame.payload)
	if cap(w.packet) < packetLength {
		w.packet = make([]byte, packetLength)
	} else {
		w.packet = w.packet[:packetLength]
	}
	header := w.packet[:StreamFrameHeaderSize]
	header[0] = StreamFrameMagic0
	header[1] = StreamFrameMagic1
	header[2] = StreamFrameMagic2
	header[3] = StreamFrameMagic3
	header[4] = StreamFrameVersionV5
	header[5] = frame.frameType
	binary.LittleEndian.PutUint16(header[6:8], uint16(len(frame.payload)))
	binary.LittleEndian.PutUint32(header[8:12], w.sequence)
	w.sequence++
	binary.LittleEndian.PutUint32(header[12:16],
		framedStreamCRC(header[4:12], frame.payload))
	copy(w.packet[StreamFrameHeaderSize:], frame.payload)

	remaining := w.packet
	for len(remaining) > 0 {
		n, err := w.conn.Write(remaining)
		if err != nil || n <= 0 {
			w.faultStream("socket write failed")
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

func (w *dualSenseOutputWriter) Stop() error {
	w.requestStop()
	if w.conn != nil {
		_ = w.conn.SetWriteDeadline(time.Now().Add(dualSenseSpeakerResetTimeout))
		_ = w.conn.Close()
	}
	timer := time.NewTimer(dualSenseOutputJoinTimeout)
	defer timer.Stop()
	select {
	case <-w.done:
		w.telemetry.teardownPending.Store(false)
		return w.latchedTeardownError()
	case <-timer.C:
		w.latchTeardownFailure(errDualSenseOutputJoinTimeout)
		w.telemetry.teardownPending.Store(true)
		w.teardownJoinOnce.Do(func() {
			go func() {
				<-w.done
				w.telemetry.teardownPending.Store(false)
			}()
		})
		return w.latchedTeardownError()
	}
}

func (w *dualSenseOutputWriter) latchTeardownFailure(err error) {
	w.teardownFailureOnce.Do(func() {
		w.teardownMu.Lock()
		w.teardownErr = err
		w.teardownMu.Unlock()
		w.telemetry.teardownFailures.Add(1)
	})
}

func (w *dualSenseOutputWriter) latchedTeardownError() error {
	w.teardownMu.Lock()
	defer w.teardownMu.Unlock()
	return w.teardownErr
}

func (w *dualSenseOutputWriter) requestStop() {
	w.accepting.Store(false)
	w.stopOnce.Do(func() {
		w.streamViable.Store(false)
		w.enqueueLock.Lock()
		w.stopped = true
		close(w.stop)
		w.enqueueLock.Unlock()
	})
}

func (w *dualSenseOutputWriter) faultStream(reason string) {
	w.accepting.Store(false)
	w.streamViable.Store(false)
	w.requestStop()
	w.telemetry.active.Store(false)
	if w.logger != nil {
		w.logger.Error("DualSense output stream faulted", "reason", reason)
	}
	if w.conn != nil {
		_ = w.conn.Close()
	}
}

func decrementUint64(target *atomic.Uint64) uint64 {
	for {
		current := target.Load()
		if current == 0 {
			return 0
		}
		if target.CompareAndSwap(current, current-1) {
			return current - 1
		}
	}
}

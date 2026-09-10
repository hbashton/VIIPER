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
	// V5 carries one 480-frame V5 generation: the combined feedback and
	// its matching front-channel stereo PCM.
	dualSenseSpeakerPayloadCapacity = dualSenseAtomicFeedbackPrefix +
		OutputStateV5Size + dualSenseV5SpeakerPayloadSize
	dualSenseSpeakerTraceInterval = 10 * time.Second
	dualSenseSpeakerResetTimeout  = 250 * time.Millisecond
	dualSenseAtomicFeedbackPrefix = 2
)

type dualSenseSpeakerStreamTelemetry struct {
	receivedPayloads             atomic.Uint64
	receivedBytes                atomic.Uint64
	enqueuedPayloads             atomic.Uint64
	enqueuedBytes                atomic.Uint64
	droppedPayloads              atomic.Uint64
	droppedBytes                 atomic.Uint64
	writtenPayloads              atomic.Uint64
	writtenBytes                 atomic.Uint64
	writeFailures                atomic.Uint64
	queueDepth                   atomic.Uint64
	queueHighWater               atomic.Uint64
	lastEnqueueNS                atomic.Int64
	maxEnqueueGapNS              atomic.Int64
	lastWriteNS                  atomic.Int64
	maxWriteGapNS                atomic.Int64
	microphoneInterfaceOverflows atomic.Uint64
	active                       atomic.Bool
}

type dualSenseSpeakerStreamSnapshot struct {
	ReceivedPayloads             uint64
	ReceivedBytes                uint64
	EnqueuedPayloads             uint64
	EnqueuedBytes                uint64
	DroppedPayloads              uint64
	DroppedBytes                 uint64
	WrittenPayloads              uint64
	WrittenBytes                 uint64
	WriteFailures                uint64
	QueueDepth                   uint64
	QueueHighWater               uint64
	MaxEnqueueGapUS              int64
	MaxWriteGapUS                int64
	MicrophoneInterfaceOverflows uint64
	Active                       bool
}

func (s *dualSenseSpeakerStreamTelemetry) snapshot() dualSenseSpeakerStreamSnapshot {
	if s == nil {
		return dualSenseSpeakerStreamSnapshot{}
	}
	return dualSenseSpeakerStreamSnapshot{
		ReceivedPayloads:             s.receivedPayloads.Load(),
		ReceivedBytes:                s.receivedBytes.Load(),
		EnqueuedPayloads:             s.enqueuedPayloads.Load(),
		EnqueuedBytes:                s.enqueuedBytes.Load(),
		DroppedPayloads:              s.droppedPayloads.Load(),
		DroppedBytes:                 s.droppedBytes.Load(),
		WrittenPayloads:              s.writtenPayloads.Load(),
		WrittenBytes:                 s.writtenBytes.Load(),
		WriteFailures:                s.writeFailures.Load(),
		QueueDepth:                   s.queueDepth.Load(),
		QueueHighWater:               s.queueHighWater.Load(),
		MaxEnqueueGapUS:              s.maxEnqueueGapNS.Load() / int64(time.Microsecond),
		MaxWriteGapUS:                s.maxWriteGapNS.Load() / int64(time.Microsecond),
		MicrophoneInterfaceOverflows: s.microphoneInterfaceOverflows.Load(),
		Active:                       s.active.Load(),
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
	frameType        byte
	payload          []byte
	audio            bool
	generationTagged bool
	pool             byte
	generation       uint64
}

const (
	dualSenseOutputPoolNone byte = iota
	dualSenseOutputPoolControl
	dualSenseOutputPoolRealtime
	dualSenseOutputPoolLatest
	dualSenseOutputPoolMicrophoneRecovery
)

// dualSenseOutputWriter serializes controller feedback and virtual speaker
// PCM on one framed stream. USB isochronous completion must never wait for TCP
// backpressure, so speaker extraction uses a fixed pool and a bounded queue.
type dualSenseOutputWriter struct {
	conn                     net.Conn
	logger                   *slog.Logger
	telemetry                *dualSenseSpeakerStreamTelemetry
	control                  chan dualSenseOutputFrame
	realtimeHaptics          chan dualSenseOutputFrame
	audio                    chan dualSenseOutputFrame
	controlFree              chan []byte
	realtimeFree             chan []byte
	latestOutputFree         chan []byte
	microphoneRecoveryFree   chan []byte
	audioFree                chan []byte
	outputSignal             chan struct{}
	microphoneRecoverySignal chan struct{}
	stop                     chan struct{}
	done                     chan struct{}
	stopOnce                 sync.Once
	enqueueLock              sync.RWMutex
	audioEnqueue             sync.Mutex
	lifecycleWait            sync.Mutex
	outputStateMu            sync.Mutex
	latestOutput             dualSenseOutputFrame
	hasLatestOutput          bool
	microphoneRecoveryMu     sync.Mutex
	microphoneRecovery       dualSenseOutputFrame
	hasMicrophoneRecovery    bool
	stopped                  bool
	streamViable             atomic.Bool
	audioGeneration          atomic.Uint64
	audioInFlightGeneration  atomic.Uint64
	audioInFlightSequence    atomic.Uint64
	audioWriteComplete       chan struct{}
	lifecycleTimer           *time.Timer
	sequence                 uint32
	packet                   []byte
	lastTrace                time.Time
}

func newDualSenseOutputWriter(conn net.Conn,
	telemetry *dualSenseSpeakerStreamTelemetry, logger *slog.Logger) *dualSenseOutputWriter {
	if telemetry == nil {
		telemetry = &dualSenseSpeakerStreamTelemetry{}
	}
	telemetry.queueDepth.Store(0)
	telemetry.lastEnqueueNS.Store(0)
	telemetry.lastWriteNS.Store(0)
	telemetry.active.Store(true)
	lifecycleTimer := time.NewTimer(time.Hour)
	if !lifecycleTimer.Stop() {
		<-lifecycleTimer.C
	}
	w := &dualSenseOutputWriter{
		conn:      conn,
		logger:    logger,
		telemetry: telemetry,
		control:   make(chan dualSenseOutputFrame, dualSenseOutputControlQueueCapacity),
		realtimeHaptics: make(chan dualSenseOutputFrame,
			dualSenseOutputControlQueueCapacity),
		audio:                    make(chan dualSenseOutputFrame, dualSenseOutputAudioQueueCapacity),
		controlFree:              make(chan []byte, dualSenseOutputControlQueueCapacity),
		realtimeFree:             make(chan []byte, dualSenseOutputControlQueueCapacity),
		latestOutputFree:         make(chan []byte, 2),
		microphoneRecoveryFree:   make(chan []byte, 2),
		audioFree:                make(chan []byte, dualSenseOutputAudioQueueCapacity),
		outputSignal:             make(chan struct{}, 1),
		microphoneRecoverySignal: make(chan struct{}, 1),
		stop:                     make(chan struct{}),
		done:                     make(chan struct{}),
		audioWriteComplete:       make(chan struct{}, 1),
		lifecycleTimer:           lifecycleTimer,
		packet:                   make([]byte, 0, StreamFrameHeaderSize+dualSenseSpeakerPayloadCapacity),
		lastTrace:                time.Now(),
	}
	w.streamViable.Store(conn != nil)
	for range dualSenseOutputControlQueueCapacity {
		w.controlFree <- make([]byte, OutputStateV5Size)
		w.realtimeFree <- make([]byte, OutputStateV5Size)
	}
	for range 2 {
		w.latestOutputFree <- make([]byte, OutputStateV5Size)
		w.microphoneRecoveryFree <- make([]byte, OutputStateV5Size)
	}
	for range dualSenseOutputAudioQueueCapacity {
		w.audioFree <- make([]byte, dualSenseSpeakerPayloadCapacity)
	}
	return w
}

// EnqueueRealtimeHaptics keeps time-bearing rear-channel generations out of
// the ordinary state queue. Games can issue dense trigger/LED SET_REPORT
// traffic; that traffic must never delay or evict the 93.75 Hz haptics clock.
func (w *dualSenseOutputWriter) EnqueueRealtimeHaptics(payload []byte) {
	w.enqueueRealtimeHaptics(payload, w.audioGeneration.Load())
}

func (w *dualSenseOutputWriter) enqueueRealtimeHaptics(payload []byte,
	generation uint64) {
	if len(payload) == 0 {
		return
	}
	if !w.prepareMediaGeneration(generation) {
		return
	}
	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped {
		return
	}
	w.enqueueCopiedFrameLocked(w.realtimeHaptics, w.realtimeFree,
		dualSenseOutputPoolRealtime, StreamFrameRealtimeHaptics, payload,
		generation)
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
	w.enqueueCopiedFrameLocked(w.control, w.controlFree,
		dualSenseOutputPoolControl, frameType, payload, 0)
}

func (w *dualSenseOutputWriter) enqueueCopiedFrameLocked(
	queue chan dualSenseOutputFrame, free chan []byte, pool byte,
	frameType byte, payload []byte, generation uint64) bool {
	if len(payload) > OutputStateV5Size {
		return false
	}
	buffer := w.acquireCopiedFrameBuffer(queue, free, pool)
	if buffer == nil {
		return false
	}
	buffer = buffer[:len(payload)]
	copy(buffer, payload)
	frame := dualSenseOutputFrame{
		frameType:        frameType,
		payload:          buffer,
		pool:             pool,
		generation:       generation,
		generationTagged: generation != 0,
	}
	if w.enqueueFrameLocked(queue, frame) {
		return true
	}
	w.release(frame)
	return false
}

// acquireCopiedFrameBuffer keeps the ordered control lane loss-bounded while
// treating realtime haptics as a time-indexed stream. If rear-channel media
// fills every fixed slot, discard the oldest not-yet-started generation and
// reuse its storage instead of preserving a stale backlog and dropping the
// newest sample.
func (w *dualSenseOutputWriter) acquireCopiedFrameBuffer(
	queue chan dualSenseOutputFrame, free chan []byte, pool byte) []byte {
	select {
	case buffer := <-free:
		return buffer
	default:
	}
	if pool != dualSenseOutputPoolRealtime {
		return nil
	}
	select {
	case oldest := <-queue:
		return oldest.payload[:cap(oldest.payload)]
	default:
		return nil
	}
}

func (w *dualSenseOutputWriter) enqueueOutputState(frameType byte,
	feedback OutputState, realtime bool, generation uint64) bool {
	// A raw HID SET_REPORT is an exact validity-bearing command, not a
	// replaceable snapshot. Combining a pulse with its stop, or a trigger
	// update with a later LED-only report, destroys the first command.
	nativeCommand := feedback.RawOutputReport[0] == ReportIDOutput &&
		feedback.BluetoothCombinedOutputReport[0] != BluetoothCombinedHapticsReportID
	if !realtime && !nativeCommand {
		return w.enqueueLatestOutputState(feedback)
	}
	if realtime && !w.prepareMediaGeneration(generation) {
		return false
	}
	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped {
		return false
	}
	queue := w.control
	free := w.controlFree
	pool := dualSenseOutputPoolControl
	if realtime {
		queue = w.realtimeHaptics
		free = w.realtimeFree
		pool = dualSenseOutputPoolRealtime
	}
	buffer := w.acquireCopiedFrameBuffer(queue, free, pool)
	if buffer == nil {
		return false
	}
	buffer = buffer[:OutputStateV5Size]
	if err := feedback.MarshalV5Into(buffer); err != nil {
		free <- buffer[:cap(buffer)]
		return false
	}
	frame := dualSenseOutputFrame{
		frameType:        frameType,
		payload:          buffer,
		pool:             pool,
		generation:       generation,
		generationTagged: realtime && generation != 0,
	}
	if !w.enqueueFrameLocked(queue, frame) {
		w.release(frame)
		return false
	}
	return true
}

func (w *dualSenseOutputWriter) enqueueLatestOutputState(feedback OutputState) bool {
	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped {
		return false
	}
	w.outputStateMu.Lock()
	if w.hasLatestOutput {
		// The unclaimed latch is exclusively producer-owned. Reuse it in place
		// so ordered lifecycle traffic cannot starve latest state of storage.
		_ = feedback.MarshalV5Into(w.latestOutput.payload)
		w.outputStateMu.Unlock()
		select {
		case w.outputSignal <- struct{}{}:
		default:
		}
		return true
	}
	var buffer []byte
	select {
	case buffer = <-w.latestOutputFree:
	default:
		w.outputStateMu.Unlock()
		return false
	}
	buffer = buffer[:OutputStateV5Size]
	if err := feedback.MarshalV5Into(buffer); err != nil {
		w.latestOutputFree <- buffer[:cap(buffer)]
		w.outputStateMu.Unlock()
		return false
	}
	frame := dualSenseOutputFrame{
		frameType: StreamFrameOutputState,
		payload:   buffer,
		pool:      dualSenseOutputPoolLatest,
	}
	w.latestOutput = frame
	w.hasLatestOutput = true
	w.outputStateMu.Unlock()
	select {
	case w.outputSignal <- struct{}{}:
	default:
	}
	return true
}

// EnqueueOutputState never waits for socket backpressure. A false result for
// a native command must reach USB admission; it must not be acknowledged or
// committed into the device's cumulative media snapshot.
func (w *dualSenseOutputWriter) EnqueueOutputState(feedback OutputState) bool {
	return w.enqueueOutputState(StreamFrameOutputState, feedback, false, 0)
}

func (w *dualSenseOutputWriter) EnqueueRealtimeHapticsState(feedback OutputState) {
	w.enqueueOutputState(StreamFrameRealtimeHaptics, feedback, true,
		w.audioGeneration.Load())
}

func (w *dualSenseOutputWriter) EnqueueRealtimeHapticsStateGeneration(
	feedback OutputState, generation uint64) {
	w.enqueueOutputState(StreamFrameRealtimeHaptics, feedback, true, generation)
}

func (w *dualSenseOutputWriter) EnqueueMicrophoneInterfaceState(active bool,
	generation uint64) {
	var payload [9]byte
	if active {
		payload[0] = 1
	}
	binary.LittleEndian.PutUint64(payload[1:], generation)

	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped {
		return
	}
	w.enqueueMicrophoneInterfaceStateLocked(payload[:])
}

func (w *dualSenseOutputWriter) enqueueMicrophoneInterfaceStateLocked(
	payload []byte,
) {
	w.microphoneRecoveryMu.Lock()
	if w.hasMicrophoneRecovery {
		copy(w.microphoneRecovery.payload, payload)
		w.telemetry.microphoneInterfaceOverflows.Add(1)
		w.microphoneRecoveryMu.Unlock()
		w.signalMicrophoneRecovery()
		return
	}
	if w.enqueueCopiedFrameLocked(w.control, w.controlFree,
		dualSenseOutputPoolControl, StreamFrameMicrophoneInterfaceState,
		payload, 0) {
		w.microphoneRecoveryMu.Unlock()
		return
	}

	var buffer []byte
	select {
	case buffer = <-w.microphoneRecoveryFree:
	default:
		// The sole writer can own at most one recovery buffer at a time, so the
		// second fixed buffer is always available when no pending latch exists.
		// Keep this guard defensive without allocating or blocking a producer.
		w.microphoneRecoveryMu.Unlock()
		return
	}
	w.telemetry.microphoneInterfaceOverflows.Add(1)
	buffer = buffer[:len(payload)]
	copy(buffer, payload)
	w.microphoneRecovery = dualSenseOutputFrame{
		frameType: StreamFrameMicrophoneInterfaceState,
		payload:   buffer,
		pool:      dualSenseOutputPoolMicrophoneRecovery,
	}
	w.hasMicrophoneRecovery = true
	w.microphoneRecoveryMu.Unlock()
	w.signalMicrophoneRecovery()
}

func (w *dualSenseOutputWriter) signalMicrophoneRecovery() {
	select {
	case w.microphoneRecoverySignal <- struct{}{}:
	default:
	}
}

func (w *dualSenseOutputWriter) claimMicrophoneRecovery() (
	dualSenseOutputFrame, bool,
) {
	w.microphoneRecoveryMu.Lock()
	if !w.hasMicrophoneRecovery {
		w.microphoneRecoveryMu.Unlock()
		return dualSenseOutputFrame{}, false
	}
	frame := w.microphoneRecovery
	w.microphoneRecovery = dualSenseOutputFrame{}
	w.hasMicrophoneRecovery = false
	w.microphoneRecoveryMu.Unlock()
	return frame, true
}

func (w *dualSenseOutputWriter) claimOrderedControl() (
	dualSenseOutputFrame, bool,
) {
	select {
	case frame := <-w.control:
		return frame, true
	default:
	}
	return w.claimMicrophoneRecovery()
}

// EnqueueAtomicAudioHaptics publishes one V5 generation. A little-endian
// feedback length prefixes the native combined feedback; the remaining bytes
// are exactly 480 matching stereo PCM frames.
func (w *dualSenseOutputWriter) EnqueueAtomicAudioHaptics(feedback, speakerPCM []byte) {
	w.enqueueAtomicAudioHaptics(feedback, nil, speakerPCM, w.audioGeneration.Load())
}

func (w *dualSenseOutputWriter) EnqueueAtomicAudioHapticsState(feedback OutputState,
	speakerPCM []byte, generation uint64) {
	w.enqueueAtomicAudioHaptics(nil, &feedback, speakerPCM, generation)
}

func (w *dualSenseOutputWriter) enqueueAtomicAudioHaptics(feedback []byte,
	feedbackState *OutputState, speakerPCM []byte, generation uint64) {
	feedbackLength := len(feedback)
	if feedbackState != nil {
		feedbackLength = OutputStateV5Size
	}
	if feedbackLength == 0 || feedbackLength > int(^uint16(0)) ||
		len(speakerPCM) == 0 {
		return
	}
	if len(speakerPCM) != dualSenseV5SpeakerPayloadSize {
		return
	}
	if !w.prepareMediaGeneration(generation) {
		return
	}

	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped {
		return
	}
	w.audioEnqueue.Lock()
	defer w.audioEnqueue.Unlock()

	w.telemetry.receivedPayloads.Add(1)
	w.telemetry.receivedBytes.Add(uint64(len(speakerPCM)))
	buffer := w.acquireAtomicAudioBuffer()
	if buffer == nil {
		w.recordSpeakerDrop(len(speakerPCM))
		return
	}

	length := dualSenseAtomicFeedbackPrefix + feedbackLength + len(speakerPCM)
	if length > cap(buffer) {
		w.audioFree <- buffer[:cap(buffer)]
		w.recordSpeakerDrop(len(speakerPCM))
		return
	}
	buffer = buffer[:length]
	binary.LittleEndian.PutUint16(buffer[:dualSenseAtomicFeedbackPrefix],
		uint16(feedbackLength))
	if feedbackState != nil {
		if err := feedbackState.MarshalV5Into(
			buffer[dualSenseAtomicFeedbackPrefix : dualSenseAtomicFeedbackPrefix+feedbackLength],
		); err != nil {
			w.audioFree <- buffer[:cap(buffer)]
			w.recordSpeakerDrop(len(speakerPCM))
			return
		}
	} else {
		copy(buffer[dualSenseAtomicFeedbackPrefix:], feedback)
	}
	copy(buffer[dualSenseAtomicFeedbackPrefix+feedbackLength:], speakerPCM)
	frame := dualSenseOutputFrame{
		frameType:        StreamFrameAtomicAudioHaptics,
		payload:          buffer,
		audio:            true,
		generationTagged: true,
		generation:       generation,
	}
	if !w.enqueueFrameLocked(w.audio, frame) {
		w.audioFree <- buffer[:cap(buffer)]
		w.recordSpeakerDrop(len(speakerPCM))
		return
	}
	w.recordSpeakerEnqueue(len(speakerPCM))
}

// acquireAtomicAudioBuffer keeps V5 realtime: when TCP momentarily falls
// behind and every fixed pool
// buffer is owned, evict the oldest queued (not in-flight) media generation so
// the newest native USB generation can still be published without growing an
// unbounded stale-audio reserve.
func (w *dualSenseOutputWriter) acquireAtomicAudioBuffer() []byte {
	select {
	case buffer := <-w.audioFree:
		return buffer
	default:
	}

	select {
	case oldest := <-w.audio:
		w.recordSpeakerDrop(atomicSpeakerPCMBytes(oldest.payload))
		w.telemetry.queueDepth.Store(uint64(len(w.audio)))
		return oldest.payload[:cap(oldest.payload)]
	default:
		// The sole remaining pool buffer can be owned by an in-flight write.
		return nil
	}
}

// prepareMediaGeneration advances without waiting for an in-flight socket
// write. It is used only by generation-tagged producers, which must never be
// delayed by transport backpressure. The lifecycle reset callback performs
// the corresponding in-flight write barrier before reset returns.
func (w *dualSenseOutputWriter) prepareMediaGeneration(generation uint64) bool {
	if generation == 0 {
		return true
	}
	w.enqueueLock.Lock()
	current := w.audioGeneration.Load()
	if generation < current {
		w.enqueueLock.Unlock()
		return false
	}
	if generation > current {
		w.audioGeneration.Store(generation)
		w.drainMediaQueues()
	}
	w.enqueueLock.Unlock()
	return true
}

// SetSpeakerGeneration establishes the device-owned media generation before
// stream callbacks become visible. It is intentionally nonblocking with
// respect to socket I/O because no old frame can belong to a fresh writer.
func (w *dualSenseOutputWriter) SetSpeakerGeneration(generation uint64) {
	if generation == 0 {
		return
	}
	_ = w.prepareMediaGeneration(generation)
}

func atomicSpeakerPCMBytes(payload []byte) int {
	if len(payload) < dualSenseAtomicFeedbackPrefix {
		return len(payload)
	}
	feedbackLength := int(binary.LittleEndian.Uint16(
		payload[:dualSenseAtomicFeedbackPrefix]))
	speakerOffset := dualSenseAtomicFeedbackPrefix + feedbackLength
	if speakerOffset > len(payload) {
		return len(payload)
	}
	return len(payload) - speakerOffset
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
		w.drainOrderedControl()
		w.drainMediaQueues()
		w.drainLatestOutput()
		w.telemetry.queueDepth.Store(0)
		w.telemetry.active.Store(false)
		w.traceSpeakerState(true)
		close(w.done)
	}()
	const laneCount = 4
	nextLane := 0
	for {

		wrote := false
		for offset := 0; offset < laneCount; offset++ {
			lane := (nextLane + offset) % laneCount
			frame, ok := w.tryOutputLane(lane)
			if !ok {
				continue
			}
			if !w.writeAndRelease(frame) {
				return
			}
			nextLane = (lane + 1) % laneCount
			wrote = true
			break
		}
		if wrote {
			continue
		}

		select {
		case <-w.stop:
			return
		case frame := <-w.control:
			if !w.writeAndRelease(frame) {
				return
			}
			nextLane = 1
		case <-w.microphoneRecoverySignal:
			if frame, ok := w.claimOrderedControl(); ok {
				if !w.writeAndRelease(frame) {
					return
				}
				nextLane = 1
			}
		case <-w.outputSignal:
			if frame, ok := w.claimLatestOutput(); ok {
				if !w.writeAndRelease(frame) {
					return
				}
				nextLane = 2
			}
		case frame := <-w.realtimeHaptics:
			if !w.writeAndRelease(frame) {
				return
			}
			nextLane = 3
		case frame := <-w.audio:
			if !w.writeAndRelease(frame) {
				return
			}
			nextLane = 0
		}
	}
}

func (w *dualSenseOutputWriter) tryOutputLane(lane int) (dualSenseOutputFrame, bool) {
	switch lane {
	case 0:
		return w.claimOrderedControl()
	case 1:
		return w.claimLatestOutput()
	case 2:
		select {
		case frame := <-w.realtimeHaptics:
			return frame, true
		default:
		}
	case 3:
		select {
		case frame := <-w.audio:
			return frame, true
		default:
		}
	}
	return dualSenseOutputFrame{}, false
}

func (w *dualSenseOutputWriter) claimLatestOutput() (dualSenseOutputFrame, bool) {
	w.outputStateMu.Lock()
	if !w.hasLatestOutput {
		w.outputStateMu.Unlock()
		return dualSenseOutputFrame{}, false
	}
	frame := w.latestOutput
	w.latestOutput = dualSenseOutputFrame{}
	w.hasLatestOutput = false
	w.outputStateMu.Unlock()
	return frame, true
}

func (w *dualSenseOutputWriter) drainLatestOutput() {
	if frame, ok := w.claimLatestOutput(); ok {
		w.release(frame)
	}
}

func (w *dualSenseOutputWriter) drainOrderedControl() {
	for {
		select {
		case frame := <-w.control:
			w.release(frame)
		default:
			if frame, ok := w.claimMicrophoneRecovery(); ok {
				w.release(frame)
			}
			return
		}
	}
}

func (w *dualSenseOutputWriter) writeAndRelease(frame dualSenseOutputFrame) bool {
	if frame.generationTagged {
		// The stream writer is the sole owner of the in-flight marker. Publish
		// it before checking the authoritative generation so reset has two safe
		// outcomes: it either observes and waits for this write, or its generation
		// publication wins and this frame is discarded before socket I/O.
		w.audioInFlightGeneration.Store(frame.generation)
		w.audioInFlightSequence.Add(1)
		if frame.generation != w.audioGeneration.Load() {
			w.completeAudioWrite()
			if frame.audio {
				w.recordSpeakerDrop(atomicSpeakerPCMBytes(frame.payload))
			}
			w.release(frame)
			if frame.audio {
				w.telemetry.queueDepth.Store(uint64(len(w.audio)))
			}
			return true
		}
	}

	ok := w.write(frame)
	if frame.generationTagged {
		w.completeAudioWrite()
	}
	if frame.audio {
		w.telemetry.queueDepth.Store(uint64(len(w.audio)))
		if ok {
			w.recordSpeakerWrite(atomicSpeakerPCMBytes(frame.payload))
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

func (w *dualSenseOutputWriter) completeAudioWrite() {
	// Even sequence values mean idle; odd values mean one generation-tagged
	// write is in flight. There is exactly one stream writer, so no second
	// producer can overwrite this marker before completion.
	w.audioInFlightGeneration.Store(0)
	w.audioInFlightSequence.Add(1)
	select {
	case w.audioWriteComplete <- struct{}{}:
	default:
	}
}

// ResetSpeaker advances the audio generation and flushes every queued frame.
// It waits for an already-started write before returning, making interface and
// endpoint resets a hard barrier between USB presentation generations.
func (w *dualSenseOutputWriter) ResetSpeaker() {
	w.enqueueLock.Lock()
	generation := w.audioGeneration.Load() + 1
	if generation == 0 {
		generation = 1
	}
	w.audioGeneration.Store(generation)
	w.drainMediaQueues()
	w.enqueueLock.Unlock()
	w.finishSpeakerResetBarrier(generation)
}

// ResetSpeakerGeneration invalidates media using the generation captured at
// the device reset boundary. A stale reset callback cannot drain a newer
// stream. Equality means a new producer already adopted the generation; its
// queues remain valid, but the old in-flight write barrier is still required.
func (w *dualSenseOutputWriter) ResetSpeakerGeneration(generation uint64) {
	if generation == 0 {
		return
	}
	w.enqueueLock.Lock()
	current := w.audioGeneration.Load()
	if generation < current {
		w.enqueueLock.Unlock()
		return
	}
	if generation > current {
		w.audioGeneration.Store(generation)
		w.drainMediaQueues()
	}
	w.enqueueLock.Unlock()
	w.finishSpeakerResetBarrier(generation)
}

func (w *dualSenseOutputWriter) finishSpeakerResetBarrier(generation uint64) {
	// Only lifecycle/reset callers serialize on this lock. Queue and generation
	// ownership was released above, and the stream writer never acquires it, so
	// socket backpressure cannot block input or output producers.
	w.lifecycleWait.Lock()
	defer w.lifecycleWait.Unlock()
	defer w.telemetry.queueDepth.Store(0)

	w.resetLifecycleTimer(dualSenseSpeakerResetTimeout)
	defer w.stopLifecycleTimer()
	for {
		targetSequence := w.audioInFlightSequence.Load()
		if targetSequence&1 == 0 {
			break
		}
		inFlightGeneration := w.audioInFlightGeneration.Load()
		if targetSequence != w.audioInFlightSequence.Load() {
			continue
		}
		// A frame from a later generation is not stale with respect to this
		// reset. This can occur when a newer producer publishes before an older
		// reset callback reaches the transport writer. Equality still waits,
		// preserving the hard barrier when adoption and reset race.
		if mediaGenerationPrecedes(generation, inFlightGeneration) {
			break
		}

		for targetSequence == w.audioInFlightSequence.Load() {
			select {
			case <-w.audioWriteComplete:
				continue
			case <-w.done:
				return
			case <-w.lifecycleTimer.C:
				// Closing the stream bounds a peer that stopped reading and prevents
				// any old-generation bytes from being emitted after reset returns.
				w.invalidateStream()
				return
			}
		}
		break
	}
}

func mediaGenerationPrecedes(first, second uint64) bool {
	return first != second && int64(first-second) < 0
}

func (w *dualSenseOutputWriter) resetLifecycleTimer(timeout time.Duration) {
	w.stopLifecycleTimer()
	w.lifecycleTimer.Reset(timeout)
}

func (w *dualSenseOutputWriter) stopLifecycleTimer() {
	if !w.lifecycleTimer.Stop() {
		select {
		case <-w.lifecycleTimer.C:
		default:
		}
	}
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

func (w *dualSenseOutputWriter) drainRealtimeHapticsQueue() {
	for {
		select {
		case frame := <-w.realtimeHaptics:
			w.release(frame)
		default:
			return
		}
	}
}

func (w *dualSenseOutputWriter) drainMediaQueues() {
	w.drainAudioQueue()
	w.drainRealtimeHapticsQueue()
	w.telemetry.queueDepth.Store(uint64(len(w.audio)))
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
		"droppedPayloads", state.DroppedPayloads,
		"droppedBytes", state.DroppedBytes,
		"writtenPayloads", state.WrittenPayloads,
		"writtenBytes", state.WrittenBytes,
		"writeFailures", state.WriteFailures,
		"queueDepth", state.QueueDepth,
		"queueHighWater", state.QueueHighWater,
		"maxEnqueueGapUS", state.MaxEnqueueGapUS,
		"maxWriteGapUS", state.MaxWriteGapUS,
		"microphoneInterfaceOverflows", state.MicrophoneInterfaceOverflows)
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
		return
	}
	switch frame.pool {
	case dualSenseOutputPoolControl:
		w.controlFree <- frame.payload[:cap(frame.payload)]
	case dualSenseOutputPoolRealtime:
		w.realtimeFree <- frame.payload[:cap(frame.payload)]
	case dualSenseOutputPoolLatest:
		w.latestOutputFree <- frame.payload[:cap(frame.payload)]
	case dualSenseOutputPoolMicrophoneRecovery:
		w.microphoneRecoveryFree <- frame.payload[:cap(frame.payload)]
	}
}

func (w *dualSenseOutputWriter) Stop() {
	w.requestStop()
	if w.conn != nil {
		_ = w.conn.SetWriteDeadline(time.Now().Add(dualSenseSpeakerResetTimeout))
		_ = w.conn.Close()
	}
	w.lifecycleWait.Lock()
	defer w.lifecycleWait.Unlock()
	w.resetLifecycleTimer(300 * time.Millisecond)
	defer w.stopLifecycleTimer()
	select {
	case <-w.done:
	case <-w.lifecycleTimer.C:
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

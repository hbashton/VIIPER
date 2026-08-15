package dualshock4

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usb"
)

func init() {
	api.RegisterDevice("dualshock4", &handler{})
	api.RegisterDevice("dualshock4audioduplexv3", &handler{
		microphoneInput: true, speakerOutput: true,
		streamFrameVersion: StreamFrameVersionV3,
	})
	api.RegisterDevice("dualshock4audioonlyduplexv3", &handler{
		microphoneInput: true, speakerOutput: true, audioOnly: true,
		streamFrameVersion: StreamFrameVersionV3,
	})
}

type handler struct {
	microphoneInput    bool
	speakerOutput      bool
	audioOnly          bool
	streamFrameVersion byte
}

var (
	serialsMu sync.Mutex
	serials   = map[string]struct{}{}
)

func (h *handler) CreateDevice(o *device.CreateOptions) (usb.Device, error) {
	if o == nil {
		o = &device.CreateOptions{}
	}

	metaState := MetaState{}
	if o.DeviceSpecific != "" {
		if err := json.Unmarshal([]byte(o.DeviceSpecific), &metaState); err != nil {
			return nil, fmt.Errorf("invalid device specific JSON: %w", err)
		}
	}
	serial := DefaultSerialString
	if metaState.SerialNumber != "" {
		serial = metaState.SerialNumber
	}
	serial = fmt.Sprintf("%016s", serial)
	serialsMu.Lock()
	if _, ok := serials[serial]; ok {
		for i := 1; i < 16; i++ {
			newSerial := fmt.Sprintf("%s%02X", serial[:len(serial)-2], i)
			if _, ok := serials[newSerial]; !ok {
				serial = newSerial
				break
			}
		}
	}
	metaState.SerialNumber = serial
	serials[serial] = struct{}{}
	serialsMu.Unlock()
	b, err := json.Marshal(metaState)
	if err != nil {
		return nil, fmt.Errorf("marshal meta state: %w", err)
	}
	o.DeviceSpecific = string(b)
	ds4, err := New(o)
	if err != nil {
		serialsMu.Lock()
		delete(serials, serial)
		serialsMu.Unlock()
		return nil, err
	}
	ds4.microphoneInput = h.microphoneInput
	ds4.speakerOutput = h.speakerOutput
	if h.audioOnly {
		ds4.descriptor = makeAudioOnlyDescriptor()
	}
	ds4.streamFrameVersion = h.streamFrameVersion
	return ds4, nil
}

func (h *handler) StreamHandler() api.StreamHandlerFunc {
	return func(conn net.Conn, devPtr *usb.Device, logger *slog.Logger) error {
		defer func() {
			if devPtr == nil || *devPtr == nil {
				return
			}
			ds4, ok := (*devPtr).(*DualShock4)
			if !ok {
				slog.Warn("device is not DualShock4 on disconnect")
				return
			}
			ds4.mtx.Lock()
			serial := ds4.metaState.SerialNumber
			ds4.mtx.Unlock()
			serialsMu.Lock()
			delete(serials, serial)
			serialsMu.Unlock()
			slog.Debug("DS4 disconnected, serial released", "serial", serial)
		}()
		if devPtr == nil || *devPtr == nil {
			return fmt.Errorf("nil device")
		}
		ds4, ok := (*devPtr).(*DualShock4)
		if !ok {
			return fmt.Errorf("%w: expected DualShock4", device.ErrWrongDeviceType)
		}

		microphoneInput := h.microphoneInput || ds4.microphoneInput
		speakerOutput := h.speakerOutput || ds4.speakerOutput
		streamFrameVersion := h.streamFrameVersion
		if ds4.streamFrameVersion != 0 {
			streamFrameVersion = ds4.streamFrameVersion
		}
		logger.Info("DualShock 4 input stream configured",
			"microphoneInput", microphoneInput,
			"speakerOutput", speakerOutput,
			"frameVersion", streamFrameVersion)

		var writer *dualShock4OutputWriter
		if speakerOutput && streamFrameVersion == StreamFrameVersionV3 {
			writer = newDualShock4OutputWriterForStream(conn, streamFrameVersion,
				ds4.beginSpeakerStream(), logger)
			ds4.SetOutputCallback(func(feedback OutputState) {
				data, err := feedback.MarshalBinary()
				if err != nil {
					logger.Error("failed to marshal feedback", "error", err)
					return
				}
				writer.EnqueueControl(StreamFrameOutputState, data)
			})
			speakerCallback := func(pcm []byte) {
				writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, pcm)
			}
			ds4.setSpeakerCallbacks(speakerCallback, writer.ResetSpeaker)
			go writer.Run()
			streamErr := readDualShock4InputStream(conn, ds4, logger,
				microphoneInput, streamFrameVersion)
			// Detach producers before requesting writer rundown. Stop returns a
			// latched failure if the writer cannot authoritatively join.
			ds4.SetOutputCallback(nil)
			ds4.detachSpeakerStreamCallbacks()
			return errors.Join(streamErr, writer.Stop())
		}

		ds4.SetOutputCallback(func(feedback OutputState) {
			data, err := feedback.MarshalBinary()
			if err != nil {
				logger.Error("failed to marshal feedback", "error", err)
				return
			}
			if _, err := conn.Write(data); err != nil {
				logger.Error("failed to send feedback", "error", err)
			}
		})
		defer ds4.SetOutputCallback(nil)
		return readDualShock4InputStream(conn, ds4, logger,
			microphoneInput, streamFrameVersion)
	}
}

type dualShock4OutputFrame struct {
	frameType     byte
	payload       []byte
	pooledBuffer  *dualShock4AudioBuffer
	audio         bool
	mediaBytes    int
	mediaDuration time.Duration
	generation    uint64
	publication   uint64
}

type dualShock4AudioBuffer struct {
	data []byte
}

const (
	dualShock4OutputControlQueueCapacity = 32
	dualShock4SpeakerFrameBytes          = USBSpeakerChannels * USBSpeakerBytesPerSample
	// Cadence is retained solely for observed producer-gap telemetry. Queue
	// admission derives time from each callback's actual aligned PCM frames.
	dualShock4SpeakerGenerationFrames  = USBSpeakerSampleRate / 100
	dualShock4SpeakerGenerationCadence = time.Second *
		dualShock4SpeakerGenerationFrames / USBSpeakerSampleRate
	dualShock4SpeakerMaximumBufferTime   = 200 * time.Millisecond
	dualShock4SpeakerMaximumBufferFrames = int(
		int64(USBSpeakerSampleRate) * int64(dualShock4SpeakerMaximumBufferTime) /
			int64(time.Second))
	// Preserve the cadence-derived item ceiling as an independent allocation
	// bound. Exact payload duration below additionally enforces the 200 ms cap.
	dualShock4OutputAudioQueueCapacity = int(
		dualShock4SpeakerMaximumBufferTime / dualShock4SpeakerGenerationCadence)
	dualShock4SpeakerResetWriteTimeout = 250 * time.Millisecond
	dualShock4OutputJoinTimeout        = 300 * time.Millisecond
)

var errDualShock4OutputJoinTimeout = errors.New(
	"DualShock 4 output writer did not stop before the join deadline")

type dualShock4OutputStreamTelemetry struct {
	orderedReceived                 atomic.Uint64
	orderedEnqueued                 atomic.Uint64
	orderedRejected                 atomic.Uint64
	orderedWritten                  atomic.Uint64
	orderedSaturations              atomic.Uint64
	orderedQueueDepth               atomic.Uint64
	orderedQueueHighWater           atomic.Uint64
	orderedLifecycleDiscardedFrames atomic.Uint64
	orderedLifecycleDiscardedBytes  atomic.Uint64
	mediaReceivedPayloads           atomic.Uint64
	mediaReceivedBytes              atomic.Uint64
	mediaEnqueuedPayloads           atomic.Uint64
	mediaEnqueuedBytes              atomic.Uint64
	mediaRejectedPayloads           atomic.Uint64
	mediaRejectedBytes              atomic.Uint64
	mediaMalformedPayloads          atomic.Uint64
	mediaMalformedBytes             atomic.Uint64
	mediaOversizePayloads           atomic.Uint64
	mediaOversizeBytes              atomic.Uint64
	mediaDroppedPayloads            atomic.Uint64
	mediaDroppedBytes               atomic.Uint64
	mediaOverruns                   atomic.Uint64
	mediaUnderruns                  atomic.Uint64
	mediaLateGaps                   atomic.Uint64
	mediaStalePayloads              atomic.Uint64
	mediaStaleBytes                 atomic.Uint64
	mediaLifecycleDiscardedPayloads atomic.Uint64
	mediaLifecycleDiscardedBytes    atomic.Uint64
	mediaWrittenPayloads            atomic.Uint64
	mediaWrittenBytes               atomic.Uint64
	orderedWriteFailures            atomic.Uint64
	mediaWriteFailures              atomic.Uint64
	mediaQueueDepth                 atomic.Uint64
	mediaQueueHighWater             atomic.Uint64
	mediaQueueDurationNS            atomic.Int64
	mediaQueueDurationHighNS        atomic.Int64
	lastMediaEnqueueNS              atomic.Int64
	maxMediaEnqueueGapNS            atomic.Int64
	lastMediaWriteNS                atomic.Int64
	maxMediaWriteGapNS              atomic.Int64
	active                          atomic.Bool
	teardownFailures                atomic.Uint64
	teardownPending                 atomic.Bool
}

type dualShock4OutputStreamSnapshot struct {
	OrderedReceived                 uint64
	OrderedEnqueued                 uint64
	OrderedRejected                 uint64
	OrderedWritten                  uint64
	OrderedSaturations              uint64
	OrderedQueueDepth               uint64
	OrderedQueueHighWater           uint64
	OrderedLifecycleDiscardedFrames uint64
	OrderedLifecycleDiscardedBytes  uint64
	MediaReceivedPayloads           uint64
	MediaReceivedBytes              uint64
	MediaEnqueuedPayloads           uint64
	MediaEnqueuedBytes              uint64
	MediaRejectedPayloads           uint64
	MediaRejectedBytes              uint64
	MediaMalformedPayloads          uint64
	MediaMalformedBytes             uint64
	MediaOversizePayloads           uint64
	MediaOversizeBytes              uint64
	MediaDroppedPayloads            uint64
	MediaDroppedBytes               uint64
	MediaOverruns                   uint64
	MediaUnderruns                  uint64
	MediaLateGaps                   uint64
	MediaStalePayloads              uint64
	MediaStaleBytes                 uint64
	MediaLifecycleDiscardedPayloads uint64
	MediaLifecycleDiscardedBytes    uint64
	MediaWrittenPayloads            uint64
	MediaWrittenBytes               uint64
	OrderedWriteFailures            uint64
	MediaWriteFailures              uint64
	MediaQueueDepth                 uint64
	MediaQueueHighWater             uint64
	MediaQueueDurationUS            int64
	MediaQueueDurationHighWaterUS   int64
	MaxMediaEnqueueGapUS            int64
	MaxMediaWriteGapUS              int64
	Active                          bool
	TeardownFailures                uint64
	TeardownPending                 bool
}

func (s *dualShock4OutputStreamTelemetry) snapshot() dualShock4OutputStreamSnapshot {
	if s == nil {
		return dualShock4OutputStreamSnapshot{}
	}
	return dualShock4OutputStreamSnapshot{
		OrderedReceived:                 s.orderedReceived.Load(),
		OrderedEnqueued:                 s.orderedEnqueued.Load(),
		OrderedRejected:                 s.orderedRejected.Load(),
		OrderedWritten:                  s.orderedWritten.Load(),
		OrderedSaturations:              s.orderedSaturations.Load(),
		OrderedQueueDepth:               s.orderedQueueDepth.Load(),
		OrderedQueueHighWater:           s.orderedQueueHighWater.Load(),
		OrderedLifecycleDiscardedFrames: s.orderedLifecycleDiscardedFrames.Load(),
		OrderedLifecycleDiscardedBytes:  s.orderedLifecycleDiscardedBytes.Load(),
		MediaReceivedPayloads:           s.mediaReceivedPayloads.Load(),
		MediaReceivedBytes:              s.mediaReceivedBytes.Load(),
		MediaEnqueuedPayloads:           s.mediaEnqueuedPayloads.Load(),
		MediaEnqueuedBytes:              s.mediaEnqueuedBytes.Load(),
		MediaRejectedPayloads:           s.mediaRejectedPayloads.Load(),
		MediaRejectedBytes:              s.mediaRejectedBytes.Load(),
		MediaMalformedPayloads:          s.mediaMalformedPayloads.Load(),
		MediaMalformedBytes:             s.mediaMalformedBytes.Load(),
		MediaOversizePayloads:           s.mediaOversizePayloads.Load(),
		MediaOversizeBytes:              s.mediaOversizeBytes.Load(),
		MediaDroppedPayloads:            s.mediaDroppedPayloads.Load(),
		MediaDroppedBytes:               s.mediaDroppedBytes.Load(),
		MediaOverruns:                   s.mediaOverruns.Load(),
		MediaUnderruns:                  s.mediaUnderruns.Load(),
		MediaLateGaps:                   s.mediaLateGaps.Load(),
		MediaStalePayloads:              s.mediaStalePayloads.Load(),
		MediaStaleBytes:                 s.mediaStaleBytes.Load(),
		MediaLifecycleDiscardedPayloads: s.mediaLifecycleDiscardedPayloads.Load(),
		MediaLifecycleDiscardedBytes:    s.mediaLifecycleDiscardedBytes.Load(),
		MediaWrittenPayloads:            s.mediaWrittenPayloads.Load(),
		MediaWrittenBytes:               s.mediaWrittenBytes.Load(),
		OrderedWriteFailures:            s.orderedWriteFailures.Load(),
		MediaWriteFailures:              s.mediaWriteFailures.Load(),
		MediaQueueDepth:                 s.mediaQueueDepth.Load(),
		MediaQueueHighWater:             s.mediaQueueHighWater.Load(),
		MediaQueueDurationUS: s.mediaQueueDurationNS.Load() /
			int64(time.Microsecond),
		MediaQueueDurationHighWaterUS: s.mediaQueueDurationHighNS.Load() /
			int64(time.Microsecond),
		MaxMediaEnqueueGapUS: s.maxMediaEnqueueGapNS.Load() /
			int64(time.Microsecond),
		MaxMediaWriteGapUS: s.maxMediaWriteGapNS.Load() /
			int64(time.Microsecond),
		Active:           s.active.Load(),
		TeardownFailures: s.teardownFailures.Load(),
		TeardownPending:  s.teardownPending.Load(),
	}
}

// dualShock4OutputWriter keeps USB isochronous completion independent from
// local TCP backpressure. Control feedback and speaker PCM share one writer so
// their framing sequence is strictly monotonic and conn.Write is never raced.
type dualShock4OutputWriter struct {
	conn                net.Conn
	version             byte
	logger              *slog.Logger
	telemetry           *dualShock4OutputStreamTelemetry
	control             chan dualShock4OutputFrame
	audio               chan dualShock4OutputFrame
	stop                chan struct{}
	done                chan struct{}
	stopOnce            sync.Once
	enqueueLock         sync.RWMutex
	controlEnqueue      sync.Mutex
	audioEnqueue        sync.Mutex
	audioWrite          sync.Mutex
	stopped             bool
	accepting           atomic.Bool
	audioGeneration     atomic.Uint64
	orderedPublication  uint64
	sequence            uint32
	packet              []byte
	audioPool           sync.Pool
	teardownMu          sync.Mutex
	teardownErr         error
	teardownFailureOnce sync.Once
	teardownJoinOnce    sync.Once
}

func newDualShock4OutputWriter(conn net.Conn, version byte) *dualShock4OutputWriter {
	return newDualShock4OutputWriterForStream(conn, version, nil, nil)
}

func newDualShock4OutputWriterForStream(conn net.Conn, version byte,
	telemetry *dualShock4OutputStreamTelemetry,
	logger *slog.Logger) *dualShock4OutputWriter {
	if telemetry == nil {
		telemetry = &dualShock4OutputStreamTelemetry{}
	}
	telemetry.orderedQueueDepth.Store(0)
	telemetry.mediaQueueDepth.Store(0)
	telemetry.mediaQueueDurationNS.Store(0)
	telemetry.lastMediaEnqueueNS.Store(0)
	telemetry.lastMediaWriteNS.Store(0)
	telemetry.active.Store(true)
	w := &dualShock4OutputWriter{
		conn: conn, version: version, logger: logger, telemetry: telemetry,
		control: make(chan dualShock4OutputFrame,
			dualShock4OutputControlQueueCapacity),
		audio: make(chan dualShock4OutputFrame,
			dualShock4OutputAudioQueueCapacity),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	w.accepting.Store(true)
	return w
}

func (w *dualShock4OutputWriter) EnqueueControl(frameType byte, payload []byte) {
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
	frame := dualShock4OutputFrame{
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
		recordDualShock4MaximumUint64(&w.telemetry.orderedQueueHighWater, depth)
		w.enqueueLock.RUnlock()
		return
	default:
		decrementDualShock4Uint64(&w.telemetry.orderedQueueDepth)
	}
	// Ordered feedback is lossless while the stream is viable. Capacity
	// exhaustion is therefore a stream failure, never permission to evict an
	// earlier rumble/LED/media-configuration update.
	w.telemetry.orderedRejected.Add(1)
	if !w.accepting.CompareAndSwap(true, false) {
		w.enqueueLock.RUnlock()
		return
	}
	w.telemetry.orderedSaturations.Add(1)
	w.enqueueLock.RUnlock()
	w.failStream("ordered output queue saturated")
}

func (w *dualShock4OutputWriter) EnqueueAudio(frameType byte, payload []byte) {
	if len(payload) == 0 {
		return
	}
	w.audioEnqueue.Lock()
	defer w.audioEnqueue.Unlock()
	w.recordMediaReceive(len(payload))
	duration, valid := w.validateMediaPayload(payload)
	if !valid {
		return
	}
	if !w.accepting.Load() {
		w.recordMediaRejected(len(payload))
		return
	}
	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped || !w.accepting.Load() {
		w.recordMediaRejected(len(payload))
		return
	}
	var buffer *dualShock4AudioBuffer
	if value := w.audioPool.Get(); value != nil {
		buffer = value.(*dualShock4AudioBuffer)
	} else {
		buffer = &dualShock4AudioBuffer{}
	}
	owned := buffer.data
	if cap(owned) < len(payload) {
		owned = make([]byte, len(payload))
	} else {
		owned = owned[:len(payload)]
	}
	buffer.data = owned
	copy(owned, payload)
	frame := dualShock4OutputFrame{
		frameType: frameType, payload: owned, pooledBuffer: buffer, audio: true,
		mediaBytes: len(payload), mediaDuration: duration,
		generation: w.audioGeneration.Load(),
	}
	w.enqueueMediaDropOldestLocked(frame)
}

// EnqueueAudioOwned accepts the immutable buffer transferred by DualShock4's
// USB/IP callback. Keeping ownership avoids copying the 10 ms PCM block twice.
func (w *dualShock4OutputWriter) EnqueueAudioOwned(frameType byte, payload []byte) {
	if len(payload) == 0 {
		return
	}
	w.audioEnqueue.Lock()
	defer w.audioEnqueue.Unlock()
	w.recordMediaReceive(len(payload))
	duration, valid := w.validateMediaPayload(payload)
	if !valid {
		return
	}
	if !w.accepting.Load() {
		w.recordMediaRejected(len(payload))
		return
	}
	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped || !w.accepting.Load() {
		w.recordMediaRejected(len(payload))
		return
	}
	w.enqueueMediaDropOldestLocked(dualShock4OutputFrame{
		frameType: frameType, payload: payload, audio: true,
		mediaBytes: len(payload), mediaDuration: duration,
		generation: w.audioGeneration.Load(),
	})
}

// enqueueMediaDropOldestLocked keeps at most the derived 200 ms media window.
// The only removable item is read from the queue itself, so an in-flight write
// is never selected as the overrun victim.
func (w *dualShock4OutputWriter) enqueueMediaDropOldestLocked(
	frame dualShock4OutputFrame) {
	duration := int64(frame.mediaDuration)
	for w.telemetry.mediaQueueDurationNS.Load()+duration >
		int64(dualShock4SpeakerMaximumBufferTime) ||
		w.telemetry.mediaQueueDepth.Load() >= uint64(cap(w.audio)) {
		select {
		case oldest := <-w.audio:
			w.recordMediaDequeued(oldest)
			w.recordMediaOverrun(oldest)
			w.release(oldest)
		default:
			// The sole consumer may have removed the last queued item between
			// observations. Retry admission using the exact atomic totals.
			continue
		}
	}
	reservedDuration := w.telemetry.mediaQueueDurationNS.Add(duration)
	depth := w.telemetry.mediaQueueDepth.Add(1)
	select {
	case w.audio <- frame:
		w.recordMediaEnqueue(frame.mediaBytes, depth, reservedDuration)
	default:
		w.telemetry.mediaQueueDurationNS.Add(-duration)
		decrementDualShock4Uint64(&w.telemetry.mediaQueueDepth)
		w.recordMediaOverrun(frame)
		w.release(frame)
	}
}

func (w *dualShock4OutputWriter) Run() {
	defer func() {
		w.requestStop()
		w.drainControlQueue()
		w.drainAudioQueue(dualShock4MediaDiscardLifecycle)
		w.telemetry.active.Store(false)
		w.traceOutputState()
		close(w.done)
	}()
	for {
		select {
		case <-w.stop:
			return
		default:
		}
		// Give feedback priority without starving speaker packets.
		select {
		case frame := <-w.control:
			decrementDualShock4Uint64(&w.telemetry.orderedQueueDepth)
			if !w.writeAndRelease(frame) {
				return
			}
			continue
		default:
		}

		select {
		case <-w.stop:
			return
		case frame := <-w.control:
			decrementDualShock4Uint64(&w.telemetry.orderedQueueDepth)
			if !w.writeAndRelease(frame) {
				return
			}
		case frame := <-w.audio:
			w.recordMediaDequeued(frame)
			if !w.writeAndRelease(frame) {
				return
			}
		}
	}
}

func (w *dualShock4OutputWriter) writeAndRelease(frame dualShock4OutputFrame) bool {
	if frame.audio {
		w.audioWrite.Lock()
		defer w.audioWrite.Unlock()
		if frame.generation != w.audioGeneration.Load() {
			w.recordMediaStale(frame)
			w.release(frame)
			return true
		}
	}

	ok := w.write(frame)
	if frame.audio {
		if ok {
			w.recordMediaWrite(len(frame.payload))
		} else {
			w.telemetry.mediaWriteFailures.Add(1)
		}
	} else if !ok {
		w.telemetry.orderedWriteFailures.Add(1)
	} else {
		w.telemetry.orderedWritten.Add(1)
	}
	w.release(frame)
	return ok
}

// ResetSpeaker advances the audio generation, drains every queued frame, and
// waits for an already-started write. Once it returns, no speaker PCM accepted
// before the interface or endpoint reset can appear on the client stream.
func (w *dualShock4OutputWriter) ResetSpeaker() {
	w.enqueueLock.Lock()
	w.audioGeneration.Add(1)
	w.drainAudioQueue(dualShock4MediaDiscardStale)
	w.telemetry.lastMediaEnqueueNS.Store(0)
	w.telemetry.lastMediaWriteNS.Store(0)
	w.enqueueLock.Unlock()

	deadlineArmed := false
	if w.conn != nil {
		if err := w.conn.SetWriteDeadline(
			time.Now().Add(dualShock4SpeakerResetWriteTimeout)); err != nil {
			w.failStream("speaker reset deadline failed")
		} else {
			deadlineArmed = true
		}
	}

	w.audioWrite.Lock()
	if deadlineArmed {
		w.clearWriteDeadlineIfViable()
	}
	w.audioWrite.Unlock()
}

func (w *dualShock4OutputWriter) clearWriteDeadlineIfViable() {
	w.enqueueLock.RLock()
	if w.stopped {
		w.enqueueLock.RUnlock()
		return
	}
	err := w.conn.SetWriteDeadline(time.Time{})
	w.enqueueLock.RUnlock()
	if err != nil {
		w.failStream("speaker reset deadline clear failed")
	}
}

type dualShock4MediaDiscardReason uint8

const (
	dualShock4MediaDiscardStale dualShock4MediaDiscardReason = iota + 1
	dualShock4MediaDiscardLifecycle
)

func (w *dualShock4OutputWriter) drainControlQueue() {
	for {
		select {
		case frame := <-w.control:
			decrementDualShock4Uint64(&w.telemetry.orderedQueueDepth)
			w.telemetry.orderedLifecycleDiscardedFrames.Add(1)
			w.telemetry.orderedLifecycleDiscardedBytes.Add(
				uint64(len(frame.payload)))
			w.release(frame)
		default:
			return
		}
	}
}

func (w *dualShock4OutputWriter) drainAudioQueue(
	reason dualShock4MediaDiscardReason) {
	for {
		select {
		case frame := <-w.audio:
			w.recordMediaDequeued(frame)
			switch reason {
			case dualShock4MediaDiscardStale:
				w.recordMediaStale(frame)
			case dualShock4MediaDiscardLifecycle:
				w.recordMediaLifecycleDiscard(frame)
			}
			w.release(frame)
		default:
			return
		}
	}
}

func (w *dualShock4OutputWriter) write(frame dualShock4OutputFrame) bool {
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
	header[4] = w.version
	header[5] = frame.frameType
	binary.LittleEndian.PutUint16(header[6:8], uint16(len(frame.payload)))
	binary.LittleEndian.PutUint32(header[8:12], w.sequence)
	w.sequence++
	binary.LittleEndian.PutUint32(header[12:16],
		dualShock4FramedStreamCRC(header[4:12], frame.payload))
	copy(w.packet[StreamFrameHeaderSize:], frame.payload)
	remaining := w.packet
	for len(remaining) > 0 {
		n, err := w.conn.Write(remaining)
		if err != nil || n <= 0 {
			w.failStream("socket write failed")
			return false
		}
		remaining = remaining[n:]
	}
	return true
}

func (w *dualShock4OutputWriter) failStream(reason string) {
	w.accepting.Store(false)
	w.requestStop()
	w.telemetry.active.Store(false)
	if w.logger != nil {
		state := w.telemetry.snapshot()
		w.logger.Error("DualShock 4 output stream faulted",
			"reason", reason,
			"orderedRejected", state.OrderedRejected,
			"orderedSaturations", state.OrderedSaturations,
			"orderedWriteFailures", state.OrderedWriteFailures,
			"mediaOverruns", state.MediaOverruns,
			"mediaWriteFailures", state.MediaWriteFailures)
	}
	if w.conn != nil {
		_ = w.conn.Close()
	}
}

func (w *dualShock4OutputWriter) release(frame dualShock4OutputFrame) {
	if frame.pooledBuffer != nil {
		w.releaseAudioBuffer(frame.pooledBuffer)
	}
}

func (w *dualShock4OutputWriter) releaseAudioBuffer(buffer *dualShock4AudioBuffer) {
	buffer.data = buffer.data[:0]
	w.audioPool.Put(buffer)
}

func (w *dualShock4OutputWriter) Stop() error {
	w.requestStop()
	if w.conn != nil {
		_ = w.conn.SetWriteDeadline(
			time.Now().Add(dualShock4SpeakerResetWriteTimeout))
		_ = w.conn.Close()
	}
	timer := time.NewTimer(dualShock4OutputJoinTimeout)
	defer timer.Stop()
	select {
	case <-w.done:
		w.telemetry.teardownPending.Store(false)
		return w.latchedTeardownError()
	case <-timer.C:
		w.latchTeardownFailure(errDualShock4OutputJoinTimeout)
		w.telemetry.teardownPending.Store(true)
		// Keep the writer and every dependent queue/buffer alive until Run's
		// authoritative final drain closes done.
		w.teardownJoinOnce.Do(func() {
			go func() {
				<-w.done
				w.telemetry.teardownPending.Store(false)
			}()
		})
		return w.latchedTeardownError()
	}
}

func (w *dualShock4OutputWriter) latchTeardownFailure(err error) {
	w.teardownFailureOnce.Do(func() {
		w.teardownMu.Lock()
		w.teardownErr = err
		w.teardownMu.Unlock()
		w.telemetry.teardownFailures.Add(1)
	})
}

func (w *dualShock4OutputWriter) latchedTeardownError() error {
	w.teardownMu.Lock()
	defer w.teardownMu.Unlock()
	return w.teardownErr
}

func (w *dualShock4OutputWriter) requestStop() {
	w.accepting.Store(false)
	w.stopOnce.Do(func() {
		w.enqueueLock.Lock()
		w.stopped = true
		close(w.stop)
		w.enqueueLock.Unlock()
	})
}

func (w *dualShock4OutputWriter) traceOutputState() {
	if w.logger == nil {
		return
	}
	state := w.telemetry.snapshot()
	w.logger.Info("DualShock 4 output stream stopped",
		"orderedReceived", state.OrderedReceived,
		"orderedEnqueued", state.OrderedEnqueued,
		"orderedRejected", state.OrderedRejected,
		"orderedWritten", state.OrderedWritten,
		"orderedSaturations", state.OrderedSaturations,
		"orderedLifecycleDiscardedFrames", state.OrderedLifecycleDiscardedFrames,
		"orderedLifecycleDiscardedBytes", state.OrderedLifecycleDiscardedBytes,
		"mediaReceivedPayloads", state.MediaReceivedPayloads,
		"mediaEnqueuedPayloads", state.MediaEnqueuedPayloads,
		"mediaRejectedPayloads", state.MediaRejectedPayloads,
		"mediaMalformedPayloads", state.MediaMalformedPayloads,
		"mediaOversizePayloads", state.MediaOversizePayloads,
		"mediaDroppedPayloads", state.MediaDroppedPayloads,
		"mediaOverruns", state.MediaOverruns,
		"mediaUnderruns", state.MediaUnderruns,
		"mediaLateGaps", state.MediaLateGaps,
		"mediaStalePayloads", state.MediaStalePayloads,
		"mediaLifecycleDiscardedPayloads",
		state.MediaLifecycleDiscardedPayloads,
		"mediaWrittenPayloads", state.MediaWrittenPayloads,
		"mediaWriteFailures", state.MediaWriteFailures,
		"mediaQueueHighWater", state.MediaQueueHighWater,
		"mediaQueueDurationHighWaterUS", state.MediaQueueDurationHighWaterUS,
		"teardownFailures", state.TeardownFailures,
		"teardownPending", state.TeardownPending)
}

func (w *dualShock4OutputWriter) validateMediaPayload(payload []byte) (
	time.Duration, bool) {
	if len(payload)%dualShock4SpeakerFrameBytes != 0 {
		w.telemetry.mediaMalformedPayloads.Add(1)
		w.telemetry.mediaMalformedBytes.Add(uint64(len(payload)))
		return 0, false
	}
	frames := int64(len(payload) / dualShock4SpeakerFrameBytes)
	// Round up fractional nanoseconds so admission is conservative for any
	// future sample rate that does not divide one second exactly.
	durationNS := (frames*int64(time.Second) +
		int64(USBSpeakerSampleRate) - 1) / int64(USBSpeakerSampleRate)
	duration := time.Duration(durationNS)
	if duration > dualShock4SpeakerMaximumBufferTime {
		w.telemetry.mediaOversizePayloads.Add(1)
		w.telemetry.mediaOversizeBytes.Add(uint64(len(payload)))
		return 0, false
	}
	return duration, true
}

func (w *dualShock4OutputWriter) recordMediaReceive(length int) {
	w.telemetry.mediaReceivedPayloads.Add(1)
	w.telemetry.mediaReceivedBytes.Add(uint64(length))
	now := time.Now().UnixNano()
	previous := w.telemetry.lastMediaEnqueueNS.Swap(now)
	if previous <= 0 || now <= previous {
		return
	}
	gap := now - previous
	recordDualShock4MaximumInt64(&w.telemetry.maxMediaEnqueueGapNS, gap)
	cadence := int64(dualShock4SpeakerGenerationCadence)
	if gap > cadence+cadence/2 {
		w.telemetry.mediaLateGaps.Add(1)
		missing := uint64(gap/cadence) - 1
		if missing == 0 {
			missing = 1
		}
		w.telemetry.mediaUnderruns.Add(missing)
	}
}

func (w *dualShock4OutputWriter) recordMediaRejected(length int) {
	w.telemetry.mediaRejectedPayloads.Add(1)
	w.telemetry.mediaRejectedBytes.Add(uint64(length))
}

func (w *dualShock4OutputWriter) recordMediaEnqueue(length int, depth uint64,
	duration int64) {
	w.telemetry.mediaEnqueuedPayloads.Add(1)
	w.telemetry.mediaEnqueuedBytes.Add(uint64(length))
	recordDualShock4MaximumUint64(&w.telemetry.mediaQueueHighWater, depth)
	recordDualShock4MaximumInt64(&w.telemetry.mediaQueueDurationHighNS, duration)
}

func (w *dualShock4OutputWriter) recordMediaDequeued(
	frame dualShock4OutputFrame) {
	decrementDualShock4Uint64(&w.telemetry.mediaQueueDepth)
	if frame.mediaDuration > 0 {
		w.telemetry.mediaQueueDurationNS.Add(-int64(frame.mediaDuration))
	}
}

func (w *dualShock4OutputWriter) recordMediaOverrun(frame dualShock4OutputFrame) {
	w.telemetry.mediaOverruns.Add(1)
	w.telemetry.mediaDroppedPayloads.Add(1)
	w.telemetry.mediaDroppedBytes.Add(uint64(len(frame.payload)))
}

func (w *dualShock4OutputWriter) recordMediaStale(frame dualShock4OutputFrame) {
	w.telemetry.mediaStalePayloads.Add(1)
	w.telemetry.mediaStaleBytes.Add(uint64(len(frame.payload)))
}

func (w *dualShock4OutputWriter) recordMediaLifecycleDiscard(
	frame dualShock4OutputFrame) {
	w.telemetry.mediaLifecycleDiscardedPayloads.Add(1)
	w.telemetry.mediaLifecycleDiscardedBytes.Add(uint64(frame.mediaBytes))
}

func (w *dualShock4OutputWriter) recordMediaWrite(length int) {
	w.telemetry.mediaWrittenPayloads.Add(1)
	w.telemetry.mediaWrittenBytes.Add(uint64(length))
	now := time.Now().UnixNano()
	previous := w.telemetry.lastMediaWriteNS.Swap(now)
	if previous > 0 && now > previous {
		recordDualShock4MaximumInt64(&w.telemetry.maxMediaWriteGapNS,
			now-previous)
	}
}

func recordDualShock4MaximumInt64(target *atomic.Int64, value int64) {
	for value > 0 {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}

func recordDualShock4MaximumUint64(target *atomic.Uint64, value uint64) {
	for value > 0 {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}

func decrementDualShock4Uint64(target *atomic.Uint64) uint64 {
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

func readDualShock4InputStream(conn net.Conn, ds4 *DualShock4,
	logger *slog.Logger, microphoneInput bool, frameVersion byte) error {
	streamDone := api.StreamDone(conn)
	if !microphoneInput {
		buf := make([]byte, InputStateSize)
		for {
			if _, err := io.ReadFull(conn, buf); err != nil {
				if err == io.EOF {
					logger.Info("client disconnected")
					return nil
				}
				return fmt.Errorf("read input state: %w", err)
			}

			var state InputState
			if err := state.UnmarshalBinary(buf); err != nil {
				return fmt.Errorf("unmarshal input state: %w", err)
			}
			if err := ds4.UpdateInputStateUntil(streamDone, &state); err != nil {
				return fmt.Errorf("queue input state: %w", err)
			}
		}
	}

	if frameVersion != StreamFrameVersionV3 {
		return fmt.Errorf("unsupported DualShock 4 framed stream version 0x%02X",
			frameVersion)
	}

	header := make([]byte, StreamFrameHeaderSize)
	input := make([]byte, InputStateSize)
	microphonePCM := make([]byte, USBMicrophoneClientFrameSize)
	var expectedSequence uint32
	sequenceInitialized := false
	for {
		if _, err := io.ReadFull(conn, header); err != nil {
			if err == io.EOF {
				logger.Info("client disconnected")
				return nil
			}
			return fmt.Errorf("read DualShock 4 stream frame header: %w", err)
		}

		if header[0] != StreamFrameMagic0 || header[1] != StreamFrameMagic1 ||
			header[2] != StreamFrameMagic2 || header[3] != StreamFrameMagic3 {
			return fmt.Errorf("invalid DualShock 4 framed stream magic %02X %02X %02X %02X",
				header[0], header[1], header[2], header[3])
		}
		if header[4] != frameVersion {
			return fmt.Errorf("unsupported DualShock 4 framed stream version 0x%02X",
				header[4])
		}

		frameType := header[5]
		payloadLen := int(binary.LittleEndian.Uint16(header[6:8]))
		var payload []byte
		switch frameType {
		case StreamFrameInputState:
			if payloadLen != InputStateSize {
				return fmt.Errorf("invalid framed DualShock 4 input state length %d",
					payloadLen)
			}
			payload = input
		case StreamFrameMicrophonePCM:
			if payloadLen != USBMicrophoneClientFrameSize {
				return fmt.Errorf("invalid DualShock 4 microphone pcm frame length %d",
					payloadLen)
			}
			payload = microphonePCM
		default:
			return fmt.Errorf("unknown DualShock 4 framed stream packet type 0x%02X length %d",
				frameType, payloadLen)
		}

		if _, err := io.ReadFull(conn, payload); err != nil {
			return fmt.Errorf("read DualShock 4 framed packet type 0x%02X: %w",
				frameType, err)
		}

		sequence := binary.LittleEndian.Uint32(header[8:12])
		if sequenceInitialized && sequence != expectedSequence {
			return fmt.Errorf("DualShock 4 framed stream sequence mismatch: got %d expected %d",
				sequence, expectedSequence)
		}
		expectedSequence = sequence + 1
		sequenceInitialized = true

		receivedCRC := binary.LittleEndian.Uint32(header[12:16])
		calculatedCRC := dualShock4FramedStreamCRC(header[4:12], payload)
		if receivedCRC != calculatedCRC {
			return fmt.Errorf("DualShock 4 framed stream CRC mismatch for sequence %d: got %08X expected %08X",
				sequence, receivedCRC, calculatedCRC)
		}

		switch frameType {
		case StreamFrameInputState:
			var state InputState
			if err := state.UnmarshalBinary(input); err != nil {
				return fmt.Errorf("unmarshal framed DualShock 4 input state: %w", err)
			}
			if err := ds4.UpdateInputStateUntil(streamDone, &state); err != nil {
				return fmt.Errorf("queue framed DualShock 4 input state: %w", err)
			}
		case StreamFrameMicrophonePCM:
			ds4.QueueMicrophonePCMFrame(microphonePCM)
		}
	}
}

func dualShock4FramedStreamCRC(headerFields, payload []byte) uint32 {
	hash := crc32.NewIEEE()
	_, _ = hash.Write(headerFields)
	_, _ = hash.Write(payload)
	return hash.Sum32()
}

func (h *handler) UpdateMetaState(meta string, dev *usb.Device) error {
	ds4, ok := (*dev).(*DualShock4)
	if !ok {
		return fmt.Errorf("%w: expected DualShock4", device.ErrWrongDeviceType)
	}
	var metaState MetaState
	err := json.Unmarshal([]byte(meta), &metaState)
	if err != nil {
		return fmt.Errorf("unmarshal meta state: %w", err)
	}
	ds4.SetMetaState(metaState)

	return nil
}

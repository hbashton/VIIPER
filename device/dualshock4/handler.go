package dualshock4

import (
	"encoding/binary"
	"encoding/json"
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
	api.RegisterDevice("dualshock4micv2", &handler{
		microphoneInput: true, streamFrameVersion: StreamFrameVersionV2,
	})
	api.RegisterDevice("dualshock4audioduplexv3", &handler{
		microphoneInput: true, speakerOutput: true,
		streamFrameVersion: StreamFrameVersionV3,
	})
}

type handler struct {
	microphoneInput    bool
	speakerOutput      bool
	streamFrameVersion byte
}

var serials = map[string]struct{}{}

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
	b, err := json.Marshal(metaState)
	if err != nil {
		return nil, fmt.Errorf("marshal meta state: %w", err)
	}
	o.DeviceSpecific = string(b)
	ds4, err := New(o)
	if err != nil {
		return nil, err
	}
	ds4.microphoneInput = h.microphoneInput
	ds4.speakerOutput = h.speakerOutput
	ds4.streamFrameVersion = h.streamFrameVersion
	return ds4, nil
}

func (h *handler) StreamHandler() api.StreamHandlerFunc {
	return func(conn net.Conn, devPtr *usb.Device, logger *slog.Logger) error {
		defer func() {
			ds4, ok := (*devPtr).(*DualShock4)
			if !ok {
				slog.Warn("device is not DualShock4 on disconnect")
				return
			}
			delete(serials, ds4.metaState.SerialNumber)
			slog.Debug("DS4 disconnected, serial released", "serial", ds4.metaState.SerialNumber)
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
			writer = newDualShock4OutputWriter(conn, streamFrameVersion)
			ds4.SetOutputCallback(func(feedback OutputState) {
				data, err := feedback.MarshalBinary()
				if err != nil {
					logger.Error("failed to marshal feedback", "error", err)
					return
				}
				writer.EnqueueControl(StreamFrameOutputState, data)
			})
			ds4.SetSpeakerCallback(func(pcm []byte) {
				writer.EnqueueAudioOwned(StreamFrameSpeakerPCM, pcm)
			})
			ds4.SetSpeakerResetCallback(writer.ResetSpeaker)
			go writer.Run()
			defer func() {
				ds4.SetOutputCallback(nil)
				ds4.SetSpeakerCallback(nil)
				ds4.SetSpeakerResetCallback(nil)
				writer.Stop()
			}()
		} else {
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
		}

		return readDualShock4InputStream(conn, ds4, logger,
			microphoneInput, streamFrameVersion)
	}
}

type dualShock4OutputFrame struct {
	frameType  byte
	payload    []byte
	pooled     bool
	audio      bool
	generation uint64
}

const dualShock4SpeakerResetWriteTimeout = 250 * time.Millisecond

// dualShock4OutputWriter keeps USB isochronous completion independent from
// local TCP backpressure. Control feedback and speaker PCM share one writer so
// their framing sequence is strictly monotonic and conn.Write is never raced.
type dualShock4OutputWriter struct {
	conn            net.Conn
	version         byte
	control         chan dualShock4OutputFrame
	audio           chan dualShock4OutputFrame
	stop            chan struct{}
	done            chan struct{}
	stopOnce        sync.Once
	enqueueLock     sync.RWMutex
	audioWrite      sync.Mutex
	stopped         bool
	audioGeneration atomic.Uint64
	sequence        uint32
	packet          []byte
	audioPool       sync.Pool
}

func newDualShock4OutputWriter(conn net.Conn, version byte) *dualShock4OutputWriter {
	return &dualShock4OutputWriter{
		conn: conn, version: version,
		control: make(chan dualShock4OutputFrame, 32),
		audio:   make(chan dualShock4OutputFrame, 256),
		stop:    make(chan struct{}), done: make(chan struct{}),
	}
}

func (w *dualShock4OutputWriter) EnqueueControl(frameType byte, payload []byte) {
	if len(payload) == 0 {
		return
	}
	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped {
		return
	}
	w.enqueueFrameLocked(w.control, dualShock4OutputFrame{
		frameType: frameType,
		payload:   append([]byte(nil), payload...),
	})
}

func (w *dualShock4OutputWriter) EnqueueAudio(frameType byte, payload []byte) {
	if len(payload) == 0 {
		return
	}
	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped {
		return
	}
	var owned []byte
	if value := w.audioPool.Get(); value != nil {
		owned = value.([]byte)
	}
	if cap(owned) < len(payload) {
		owned = make([]byte, len(payload))
	} else {
		owned = owned[:len(payload)]
	}
	copy(owned, payload)
	frame := dualShock4OutputFrame{
		frameType: frameType, payload: owned, pooled: true, audio: true,
		generation: w.audioGeneration.Load(),
	}
	if !w.enqueueFrameLocked(w.audio, frame) {
		w.audioPool.Put(owned[:0])
	}
}

// EnqueueAudioOwned accepts the immutable buffer transferred by DualShock4's
// USB/IP callback. Keeping ownership avoids copying the 10 ms PCM block twice.
func (w *dualShock4OutputWriter) EnqueueAudioOwned(frameType byte, payload []byte) {
	if len(payload) == 0 {
		return
	}
	w.enqueueLock.RLock()
	defer w.enqueueLock.RUnlock()
	if w.stopped {
		return
	}
	w.enqueueFrameLocked(w.audio, dualShock4OutputFrame{
		frameType: frameType, payload: payload, audio: true,
		generation: w.audioGeneration.Load(),
	})
}

// enqueueFrameLocked requires enqueueLock to be held for reading. Reset and
// shutdown take the write side before draining, so a producer cannot publish a
// stale frame after the final empty-queue observation.
func (w *dualShock4OutputWriter) enqueueFrameLocked(
	queue chan dualShock4OutputFrame, frame dualShock4OutputFrame) bool {
	select {
	case queue <- frame:
		return true
	default:
		// Never block the USB/IP isochronous or HID callback. The receiver
		// bounds its own latency too, so dropping newest under pathological
		// backpressure is preferable to stalling the virtual USB device.
		return false
	}
}

func (w *dualShock4OutputWriter) Run() {
	defer func() {
		w.requestStop()
		w.drainAudioQueue()
		close(w.done)
	}()
	for {
		// Give feedback priority without starving speaker packets.
		select {
		case frame := <-w.control:
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
			if !w.writeAndRelease(frame) {
				return
			}
		case frame := <-w.audio:
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
			w.release(frame)
			return true
		}
	}

	ok := w.write(frame)
	w.release(frame)
	return ok
}

// ResetSpeaker advances the audio generation, drains every queued frame, and
// waits for an already-started write. Once it returns, no speaker PCM accepted
// before the interface or endpoint reset can appear on the client stream.
func (w *dualShock4OutputWriter) ResetSpeaker() {
	w.enqueueLock.Lock()
	w.audioGeneration.Add(1)
	w.drainAudioQueue()
	w.enqueueLock.Unlock()

	deadlineArmed := false
	if w.conn != nil {
		if err := w.conn.SetWriteDeadline(
			time.Now().Add(dualShock4SpeakerResetWriteTimeout)); err != nil {
			w.failStream()
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
		w.failStream()
	}
}

func (w *dualShock4OutputWriter) drainAudioQueue() {
	for {
		select {
		case frame := <-w.audio:
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
		dualShock4FramedStreamCRC(header[4:12], frame.payload))
	copy(w.packet[StreamFrameV2HeaderSize:], frame.payload)
	remaining := w.packet
	for len(remaining) > 0 {
		n, err := w.conn.Write(remaining)
		if err != nil || n <= 0 {
			w.failStream()
			return false
		}
		remaining = remaining[n:]
	}
	return true
}

func (w *dualShock4OutputWriter) failStream() {
	w.requestStop()
	if w.conn != nil {
		_ = w.conn.Close()
	}
}

func (w *dualShock4OutputWriter) release(frame dualShock4OutputFrame) {
	if frame.pooled {
		w.audioPool.Put(frame.payload[:0])
	}
}

func (w *dualShock4OutputWriter) Stop() {
	w.requestStop()
	_ = w.conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
	select {
	case <-w.done:
	case <-time.After(300 * time.Millisecond):
	}
}

func (w *dualShock4OutputWriter) requestStop() {
	w.stopOnce.Do(func() {
		w.enqueueLock.Lock()
		w.stopped = true
		close(w.stop)
		w.enqueueLock.Unlock()
	})
}

func readDualShock4InputStream(conn net.Conn, ds4 *DualShock4,
	logger *slog.Logger, microphoneInput bool, frameVersion byte) error {
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
			ds4.UpdateInputState(&state)
		}
	}

	if frameVersion != StreamFrameVersionV2 &&
		frameVersion != StreamFrameVersionV3 {
		return fmt.Errorf("unsupported DualShock 4 framed stream version 0x%02X",
			frameVersion)
	}

	header := make([]byte, StreamFrameV2HeaderSize)
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
			ds4.UpdateInputState(&state)
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

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
				writer.EnqueueAudio(StreamFrameSpeakerPCM, pcm)
			})
			go writer.Run()
			defer func() {
				ds4.SetOutputCallback(nil)
				ds4.SetSpeakerCallback(nil)
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
	frameType byte
	payload   []byte
}

// dualShock4OutputWriter keeps USB isochronous completion independent from
// local TCP backpressure. Control feedback and speaker PCM share one writer so
// their framing sequence is strictly monotonic and conn.Write is never raced.
type dualShock4OutputWriter struct {
	conn     net.Conn
	version  byte
	control  chan dualShock4OutputFrame
	audio    chan dualShock4OutputFrame
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	sequence uint32
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
	w.enqueue(w.control, frameType, payload)
}

func (w *dualShock4OutputWriter) EnqueueAudio(frameType byte, payload []byte) {
	w.enqueue(w.audio, frameType, payload)
}

func (w *dualShock4OutputWriter) enqueue(queue chan dualShock4OutputFrame,
	frameType byte, payload []byte) {
	frame := dualShock4OutputFrame{frameType: frameType,
		payload: append([]byte(nil), payload...)}
	select {
	case <-w.stop:
		return
	case queue <- frame:
	default:
		// Never block the USB/IP isochronous or HID callback. The receiver
		// bounds its own latency too, so dropping newest under pathological
		// backpressure is preferable to stalling the virtual USB device.
	}
}

func (w *dualShock4OutputWriter) Run() {
	defer close(w.done)
	for {
		// Give feedback priority without starving speaker packets.
		select {
		case frame := <-w.control:
			if !w.write(frame) {
				return
			}
			continue
		default:
		}

		select {
		case <-w.stop:
			return
		case frame := <-w.control:
			if !w.write(frame) {
				return
			}
		case frame := <-w.audio:
			if !w.write(frame) {
				return
			}
		}
	}
}

func (w *dualShock4OutputWriter) write(frame dualShock4OutputFrame) bool {
	if len(frame.payload) > int(^uint16(0)) {
		return true
	}
	header := make([]byte, StreamFrameV2HeaderSize)
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
	packet := append(header, frame.payload...)
	if _, err := w.conn.Write(packet); err != nil {
		_ = w.conn.Close()
		return false
	}
	return true
}

func (w *dualShock4OutputWriter) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
	_ = w.conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
	select {
	case <-w.done:
	case <-time.After(300 * time.Millisecond):
	}
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

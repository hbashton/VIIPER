package dualshock4

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usb"
)

func init() {
	api.RegisterDevice("dualshock4", &handler{})
	api.RegisterDevice("dualshock4micv2", &handler{
		microphoneInput: true, streamFrameVersion: StreamFrameVersionV2,
	})
}

type handler struct {
	microphoneInput    bool
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

		microphoneInput := h.microphoneInput || ds4.microphoneInput
		streamFrameVersion := h.streamFrameVersion
		if ds4.streamFrameVersion != 0 {
			streamFrameVersion = ds4.streamFrameVersion
		}
		logger.Info("DualShock 4 input stream configured",
			"microphoneInput", microphoneInput,
			"frameVersion", streamFrameVersion)
		return readDualShock4InputStream(conn, ds4, logger,
			microphoneInput, streamFrameVersion)
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

	if frameVersion != StreamFrameVersionV2 {
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

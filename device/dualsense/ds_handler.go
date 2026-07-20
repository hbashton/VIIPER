package dualsense

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net"
	"strings"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usb"
)

func init() {
	api.RegisterDevice("dualsense", &dshandler{})
	api.RegisterDevice("dualsenseext", &dshandler{extendedFeedback: true})
	api.RegisterDevice("dualsensecombinedext", &dshandler{combinedBluetoothFeedback: true})
	api.RegisterDevice("dualsensecombinedmicext", &dshandler{combinedBluetoothFeedback: true, microphoneInput: true, streamFrameVersion: StreamFrameVersion})
	api.RegisterDevice("dualsensecombinedmicv2", &dshandler{combinedBluetoothFeedback: true, microphoneInput: true, streamFrameVersion: StreamFrameVersionV2})
	api.RegisterDevice("dualsensecombinedaudioduplexv3", &dshandler{combinedBluetoothFeedback: true, microphoneInput: true, speakerOutput: true, streamFrameVersion: StreamFrameVersionV3})
}

type dshandler struct {
	extendedFeedback          bool
	combinedBluetoothFeedback bool
	microphoneInput           bool
	speakerOutput             bool
	streamFrameVersion        byte
}

func (h *dshandler) CreateDevice(o *device.CreateOptions) (usb.Device, error) {
	if o == nil {
		o = &device.CreateOptions{}
	}

	metaState := MetaState{
		ShellColor: DefaultShellColor,
	}
	if o.DeviceSpecific != "" {
		if err := json.Unmarshal([]byte(o.DeviceSpecific), &metaState); err != nil {
			return nil, fmt.Errorf("invalid device specific JSON: %w", err)
		}
	}

	serial := metaState.SerialNumber
	if serial == "" {
		serial = DefaultSerialNumberDS
	}
	if metaState.ShellColor != "" && len(serial) >= 6 {
		code := strings.ToUpper(metaState.ShellColor)
		if len(code) >= 2 {
			serial = serial[:4] + code[:2] + serial[6:]
		}
	}
	if _, ok := serials[serial]; ok {
		for i := 1; i < 16; i++ {
			newSerial := fmt.Sprintf("%s%02X", serial[:len(serial)-2], i)
			if _, exists := serials[newSerial]; !exists {
				serial = newSerial
				break
			}
		}
	}
	metaState.SerialNumber = serial
	serials[serial] = struct{}{}

	mac := metaState.MACAddress
	if mac == "" {
		mac = DefaultMACAddressDS
	}
	if _, ok := macs[mac]; ok {
		prefix := mac[:len(mac)-2]
		for i := 1; i <= 16; i++ {
			candidate := fmt.Sprintf("%s%02X", prefix, i)
			if _, exists := macs[candidate]; !exists {
				mac = candidate
				break
			}
		}
	}
	metaState.MACAddress = mac
	macs[mac] = struct{}{}

	b, err := json.Marshal(metaState)
	if err != nil {
		return nil, fmt.Errorf("marshal meta state: %w", err)
	}
	o.DeviceSpecific = string(b)

	dse, err := new(o, false)
	if err != nil {
		return nil, err
	}
	dse.extendedFeedback = h.extendedFeedback
	dse.combinedBluetoothFeedback = h.combinedBluetoothFeedback
	dse.microphoneInput = h.microphoneInput
	dse.speakerOutput = h.speakerOutput
	dse.streamFrameVersion = h.streamFrameVersion
	return dse, nil
}

func (h *dshandler) StreamHandler() api.StreamHandlerFunc {
	return func(conn net.Conn, devPtr *usb.Device, logger *slog.Logger) error {
		defer func() {
			if devPtr == nil || *devPtr == nil {
				return
			}
			dse, ok := (*devPtr).(*DualSense)
			if !ok {
				slog.Warn("device is not DualSenseEdge on disconnect")
				return
			}
			dse.mtx.Lock()
			serial := dse.metaState.SerialNumber
			mac := dse.metaState.MACAddress
			dse.mtx.Unlock()
			delete(serials, serial)
			delete(macs, mac)
			slog.Debug("DualSenseEdge disconnected, serial/mac released", "serial", serial, "mac", mac)
		}()

		if devPtr == nil || *devPtr == nil {
			return fmt.Errorf("nil device")
		}
		dse, ok := (*devPtr).(*DualSense)
		if !ok {
			return fmt.Errorf("%w: expected DualSenseEdge", device.ErrWrongDeviceType)
		}

		microphoneInput := h.microphoneInput || dse.microphoneInput
		speakerOutput := h.speakerOutput || dse.speakerOutput
		streamFrameVersion := h.streamFrameVersion
		if dse.streamFrameVersion != 0 {
			streamFrameVersion = dse.streamFrameVersion
		}
		logger.Info("DualSense input stream configured",
			"microphoneInput", microphoneInput,
			"speakerOutput", speakerOutput,
			"frameVersion", streamFrameVersion)

		marshalFeedback := func(feedback OutputState) ([]byte, error) {
			var data []byte
			var err error
			if h.combinedBluetoothFeedback || dse.combinedBluetoothFeedback {
				data, err = feedback.MarshalCombinedExtendedBinary()
			} else if h.extendedFeedback || dse.extendedFeedback {
				data, err = feedback.MarshalExtendedBinary()
			} else {
				data, err = feedback.MarshalBinary()
			}
			if err != nil {
				return nil, err
			}
			return data, nil
		}

		if speakerOutput {
			if streamFrameVersion != StreamFrameVersionV3 {
				return fmt.Errorf("DualSense speaker output requires framed stream version 0x%02X",
					StreamFrameVersionV3)
			}

			writer := newDualSenseOutputWriter(conn, streamFrameVersion,
				dse.beginSpeakerStream(), logger)
			go writer.Run()
			dse.SetOutputCallback(func(feedback OutputState) {
				data, err := marshalFeedback(feedback)
				if err != nil {
					logger.Error("failed to marshal feedback", "error", err)
					return
				}
				writer.EnqueueControl(StreamFrameOutputState, data)
			})
			dse.SetSpeakerCallback(writer.EnqueueSpeakerFromUSB)
			dse.SetSpeakerResetCallback(writer.ResetSpeaker)
			defer func() {
				dse.SetOutputCallback(nil)
				dse.SetSpeakerCallback(nil)
				dse.SetSpeakerResetCallback(nil)
				writer.Stop()
			}()
		} else {
			dse.SetOutputCallback(func(feedback OutputState) {
				data, err := marshalFeedback(feedback)
				if err != nil {
					logger.Error("failed to marshal feedback", "error", err)
					return
				}
				if _, err := conn.Write(data); err != nil {
					logger.Error("failed to send feedback", "error", err)
				}
			})
			defer dse.SetOutputCallback(nil)
		}

		return readDualSenseInputStreamVersion(conn, dse, logger, microphoneInput, streamFrameVersion)
	}
}

func readDualSenseInputStream(conn net.Conn, dse *DualSense, logger *slog.Logger, microphoneInput bool) error {
	return readDualSenseInputStreamVersion(conn, dse, logger, microphoneInput, StreamFrameVersion)
}

func readDualSenseInputStreamVersion(conn net.Conn, dse *DualSense, logger *slog.Logger, microphoneInput bool, frameVersion byte) error {
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
			dse.UpdateInputState(&state)
		}
	}

	if frameVersion != StreamFrameVersion &&
		frameVersion != StreamFrameVersionV2 &&
		frameVersion != StreamFrameVersionV3 {
		return fmt.Errorf("unsupported DualSense framed stream version 0x%02X",
			frameVersion)
	}

	headerSize := StreamFrameHeaderSize
	if frameVersion == StreamFrameVersionV2 ||
		frameVersion == StreamFrameVersionV3 {
		headerSize = StreamFrameV2HeaderSize
	}
	header := make([]byte, headerSize)
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
			return fmt.Errorf("read stream frame header: %w", err)
		}

		if header[0] != StreamFrameMagic0 ||
			header[1] != StreamFrameMagic1 ||
			header[2] != StreamFrameMagic2 ||
			header[3] != StreamFrameMagic3 {
			return fmt.Errorf("invalid DualSense framed stream magic %02X %02X %02X %02X",
				header[0], header[1], header[2], header[3])
		}
		if header[4] != frameVersion {
			return fmt.Errorf("unsupported DualSense framed stream version 0x%02X", header[4])
		}

		frameType := header[5]
		payloadLen := int(binary.LittleEndian.Uint16(header[6:8]))

		var payload []byte
		switch frameType {
		case StreamFrameInputState:
			if payloadLen != InputStateSize {
				return fmt.Errorf("invalid framed input state length %d", payloadLen)
			}
			payload = input
		case StreamFrameMicrophonePCM:
			if payloadLen != USBMicrophoneClientFrameSize {
				return fmt.Errorf("invalid microphone pcm frame length %d", payloadLen)
			}
			payload = microphonePCM
		default:
			return fmt.Errorf("unknown DualSense framed stream packet type 0x%02X length %d", frameType, payloadLen)
		}

		if _, err := io.ReadFull(conn, payload); err != nil {
			return fmt.Errorf("read framed packet type 0x%02X: %w", frameType, err)
		}

		if frameVersion == StreamFrameVersionV2 ||
			frameVersion == StreamFrameVersionV3 {
			sequence := binary.LittleEndian.Uint32(header[8:12])
			if sequenceInitialized && sequence != expectedSequence {
				return fmt.Errorf("DualSense framed stream sequence mismatch: got %d expected %d", sequence, expectedSequence)
			}
			expectedSequence = sequence + 1
			sequenceInitialized = true

			receivedCRC := binary.LittleEndian.Uint32(header[12:16])
			calculatedCRC := framedStreamCRC(header[4:12], payload)
			if receivedCRC != calculatedCRC {
				return fmt.Errorf("DualSense framed stream CRC mismatch for sequence %d: got %08X expected %08X", sequence, receivedCRC, calculatedCRC)
			}
		}

		switch frameType {
		case StreamFrameInputState:
			corruptReason := inputStatePayloadCorruptionReason(input)
			recordTrafficBytes("client->device", "framed-input-state",
				input,
				"summary", describeInputStatePayload(input, corruptReason))
			if corruptReason != "" {
				return fmt.Errorf("invalid framed input state: %s", corruptReason)
			}
			var state InputState
			if err := state.UnmarshalBinary(input); err != nil {
				return fmt.Errorf("unmarshal framed input state: %w", err)
			}
			dse.UpdateInputState(&state)
		case StreamFrameMicrophonePCM:
			recordTrafficSummary("client->device", "framed-microphone-pcm", len(microphonePCM),
				"summary", describeMicrophonePCMFrame(microphonePCM))
			dse.QueueMicrophonePCMFrame(microphonePCM)
		}
	}
}

func framedStreamCRC(headerFields, payload []byte) uint32 {
	hash := crc32.NewIEEE()
	_, _ = hash.Write(headerFields)
	_, _ = hash.Write(payload)
	return hash.Sum32()
}

func inputStatePayloadCorruptionReason(input []byte) string {
	if len(input) < InputStateSize {
		return fmt.Sprintf("invalid length %d", len(input))
	}

	buttons := binary.LittleEndian.Uint32(input[4:8])
	dpad := input[8]
	if buttons&^validDualSenseInputButtons != 0 ||
		dpad&^validDualSenseInputDPad != 0 {
		return fmt.Sprintf("invalid controls buttons=0x%08X dpad=0x%02X", buttons, dpad)
	}

	return ""
}

func describeInputStatePayload(input []byte, corruptReason string) string {
	if len(input) < InputStateSize {
		return fmt.Sprintf("len=%d corruptReason=%s", len(input), corruptReason)
	}

	buttons := binary.LittleEndian.Uint32(input[4:8])
	dpad := input[8]
	gyroX := int16(binary.LittleEndian.Uint16(input[21:23]))
	gyroY := int16(binary.LittleEndian.Uint16(input[23:25]))
	gyroZ := int16(binary.LittleEndian.Uint16(input[25:27]))
	accelX := int16(binary.LittleEndian.Uint16(input[27:29]))
	accelY := int16(binary.LittleEndian.Uint16(input[29:31]))
	accelZ := int16(binary.LittleEndian.Uint16(input[31:33]))

	return fmt.Sprintf(
		"lx=%d ly=%d rx=%d ry=%d buttons=0x%08X dpad=0x%02X l2=%d r2=%d touch1Status=0x%02X touch2Status=0x%02X gyro=%d,%d,%d accel=%d,%d,%d fullMagic=%t markerFragControls=%t micLeak=%t corruptReason=%s",
		int8(input[0]),
		int8(input[1]),
		int8(input[2]),
		int8(input[3]),
		buttons,
		dpad,
		input[9],
		input[10],
		input[15],
		input[20],
		gyroX,
		gyroY,
		gyroZ,
		accelX,
		accelY,
		accelZ,
		containsStreamMagic(input),
		containsStreamMarkerFragment(input, len(input)),
		containsMicTransportLeakPattern(input[11:]),
		corruptReason)
}

func containsStreamMagic(data []byte) bool {
	const magicLength = 4
	if len(data) < magicLength {
		return false
	}

	for i := 0; i+magicLength <= len(data); i++ {
		if data[i] == StreamFrameMagic0 &&
			data[i+1] == StreamFrameMagic1 &&
			data[i+2] == StreamFrameMagic2 &&
			data[i+3] == StreamFrameMagic3 {
			return true
		}
	}

	return false
}

func containsStreamMarkerFragment(data []byte, length int) bool {
	const markerLength = 3
	if len(data) < markerLength || length < markerLength {
		return false
	}

	end := min(length, len(data))
	for i := 0; i+markerLength <= end; i++ {
		if data[i] == StreamFrameMagic0 &&
			data[i+1] == StreamFrameMagic1 &&
			data[i+2] == StreamFrameMagic2 {
			return true
		}
		if data[i] == StreamFrameMagic1 &&
			data[i+1] == StreamFrameMagic2 &&
			data[i+2] == StreamFrameMagic3 {
			return true
		}
	}

	return false
}

func containsMicTransportLeakPattern(data []byte) bool {
	return containsStrongMicTransportLeakPattern(data) ||
		containsWeakMicTransportLeakPattern(data)
}

func containsStrongMicTransportLeakPattern(data []byte) bool {
	return containsByteSequence(data, []byte{StreamFrameMagic2, StreamFrameMagic3, 0x01, 0x01, hidClassOUT}) ||
		containsByteSequence(data, []byte{StreamFrameMagic3, 0x01, 0x01, hidClassOUT}) ||
		containsByteSequence(data, []byte{StreamFrameMagic2, StreamFrameMagic1, 0x80, 0x87, StreamFrameMagic2}) ||
		containsByteSequence(data, []byte{StreamFrameMagic1, 0x80, 0x87, StreamFrameMagic2})
}

func containsWeakMicTransportLeakPattern(data []byte) bool {
	return containsByteSequence(data, []byte{0x01, 0x01, hidClassOUT}) ||
		containsByteSequence(data, []byte{0x80, 0x87, StreamFrameMagic2})
}

func containsByteSequence(data []byte, sequence []byte) bool {
	if len(sequence) == 0 || len(data) < len(sequence) {
		return false
	}

	for i := 0; i+len(sequence) <= len(data); i++ {
		found := true
		for j := range sequence {
			if data[i+j] != sequence[j] {
				found = false
				break
			}
		}
		if found {
			return true
		}
	}

	return false
}

func isPowerOfTwo(value int) bool {
	return value > 0 && value&(value-1) == 0
}

func (h *dshandler) UpdateMetaState(meta string, dev *usb.Device) error {
	dse, ok := (*dev).(*DualSense)
	if !ok {
		return fmt.Errorf("%w: expected DualSenseEdge", device.ErrWrongDeviceType)
	}
	dse.mtx.Lock()
	current := *dse.metaState
	dse.mtx.Unlock()
	if err := json.Unmarshal([]byte(meta), &current); err != nil {
		return fmt.Errorf("unmarshal meta state: %w", err)
	}
	dse.SetMetaState(current)
	return nil
}

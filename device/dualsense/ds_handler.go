package dualsense

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
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
	api.RegisterDevice("dualsensecombinedmicext", &dshandler{combinedBluetoothFeedback: true, microphoneInput: true})
}

type dshandler struct {
	extendedFeedback          bool
	combinedBluetoothFeedback bool
	microphoneInput           bool
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

		dse.SetOutputCallback(func(feedback OutputState) {
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
				logger.Error("failed to marshal feedback", "error", err)
				return
			}
			if _, err := conn.Write(data); err != nil {
				logger.Error("failed to send feedback", "error", err)
			}
		})

		return readDualSenseInputStream(conn, dse, logger, h.microphoneInput)
	}
}

func readDualSenseInputStream(conn net.Conn, dse *DualSense, logger *slog.Logger, microphoneInput bool) error {
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

	header := make([]byte, StreamFrameHeaderSize)
	input := make([]byte, InputStateSize)
	microphonePCM := make([]byte, USBMicrophoneClientFrameSize)
	corruptInputFrames := 0
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
		if header[4] != StreamFrameVersion {
			return fmt.Errorf("unsupported DualSense framed stream version 0x%02X", header[4])
		}

		frameType := header[5]
		payloadLen := int(binary.LittleEndian.Uint16(header[6:8]))

		switch frameType {
		case StreamFrameInputState:
			if payloadLen != InputStateSize {
				return fmt.Errorf("invalid framed input state length %d", payloadLen)
			}
			if _, err := io.ReadFull(conn, input); err != nil {
				return fmt.Errorf("read framed input state: %w", err)
			}
			corruptReason := inputStatePayloadCorruptionReason(input)
			recordTrafficBytes("client->device", "framed-input-state",
				input,
				"summary", describeInputStatePayload(input, corruptReason))
			if corruptReason != "" {
				corruptInputFrames++
				if corruptInputFrames <= 128 || isPowerOfTwo(corruptInputFrames) {
					logger.Warn("DualSense framed input was corrupt; input reset to neutral",
						"count", corruptInputFrames,
						"reason", corruptReason)
				}
				neutralizeInputStatePayload(input)
				recordTrafficBytes("client->device", "framed-input-state-after-reset",
					input,
					"summary", describeInputStatePayload(input, "reset from "+corruptReason))
			}
			var state InputState
			if err := state.UnmarshalBinary(input); err != nil {
				return fmt.Errorf("unmarshal framed input state: %w", err)
			}
			dse.UpdateInputState(&state)
		case StreamFrameMicrophonePCM:
			if payloadLen != USBMicrophoneClientFrameSize {
				return fmt.Errorf("invalid microphone pcm frame length %d", payloadLen)
			}
			if _, err := io.ReadFull(conn, microphonePCM); err != nil {
				return fmt.Errorf("read microphone pcm frame: %w", err)
			}
			recordTrafficSummary("client->device", "framed-microphone-pcm", len(microphonePCM),
				"summary", describeMicrophonePCMFrame(microphonePCM))
			dse.QueueMicrophonePCMFrame(microphonePCM)
		default:
			return fmt.Errorf("unknown DualSense framed stream packet type 0x%02X length %d", frameType, payloadLen)
		}
	}
}

func neutralizeInputStatePayload(input []byte) {
	neutral, err := NewInputState().MarshalBinary()
	if err != nil {
		for i := range input {
			input[i] = 0
		}
		return
	}

	copy(input, neutral)
}

func inputStatePayloadCorruptionReason(input []byte) string {
	if len(input) < InputStateSize {
		return ""
	}
	if containsStreamMagic(input) {
		return "full transport magic in payload"
	}

	buttons := binary.LittleEndian.Uint32(input[4:8])
	dpad := input[8]
	if buttons&^validDualSenseInputButtons != 0 ||
		dpad&^validDualSenseInputDPad != 0 {
		return fmt.Sprintf("invalid controls buttons=0x%08X dpad=0x%02X", buttons, dpad)
	}

	// The mic storm observed in the wild leaked framed-stream markers into
	// touch and motion bytes too. A three-byte VPC/PCM fragment is specific
	// enough to reject the whole input frame before it can become a HID report.
	if containsStreamMarkerFragment(input, 11) {
		return "transport marker fragment in controls"
	}
	if containsStreamMarkerFragment(input[11:], len(input)-11) {
		return "transport marker fragment in touch/motion"
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
		"lx=%d ly=%d rx=%d ry=%d buttons=0x%08X dpad=0x%02X l2=%d r2=%d touch1Status=0x%02X touch2Status=0x%02X gyro=%d,%d,%d accel=%d,%d,%d fullMagic=%t markerFragControls=%t corruptReason=%s",
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

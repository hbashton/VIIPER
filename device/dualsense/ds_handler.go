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
			dse.QueueMicrophonePCMFrame(microphonePCM)
		default:
			return fmt.Errorf("unknown DualSense framed stream packet type 0x%02X length %d", frameType, payloadLen)
		}
	}
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

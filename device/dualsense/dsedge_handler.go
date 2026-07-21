package dualsense

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usb"
)

func init() {
	api.RegisterDevice("dualsenseedge", &dsedgehandler{})
	api.RegisterDevice("dualsenseedgeext", &dsedgehandler{extendedFeedback: true})
	api.RegisterDevice("dualsenseedgecombinedext", &dsedgehandler{combinedBluetoothFeedback: true})
	api.RegisterDevice("dualsenseedgecombinedmicext", &dsedgehandler{combinedBluetoothFeedback: true, microphoneInput: true, streamFrameVersion: StreamFrameVersion})
	api.RegisterDevice("dualsenseedgecombinedmicv2", &dsedgehandler{combinedBluetoothFeedback: true, microphoneInput: true, streamFrameVersion: StreamFrameVersionV2})
	api.RegisterDevice("dualsenseedgecombinedaudioduplexv3", &dsedgehandler{combinedBluetoothFeedback: true, microphoneInput: true, speakerOutput: true, streamFrameVersion: StreamFrameVersionV3})
	api.RegisterDevice("dualsenseedgecombinedaudioduplexv4", &dsedgehandler{combinedBluetoothFeedback: true, microphoneInput: true, speakerOutput: true, streamFrameVersion: StreamFrameVersionV4})
}

type dsedgehandler struct {
	extendedFeedback          bool
	combinedBluetoothFeedback bool
	microphoneInput           bool
	speakerOutput             bool
	streamFrameVersion        byte
}

func (h *dsedgehandler) CreateDevice(o *device.CreateOptions) (usb.Device, error) {
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
		serial = DefaultSerialNumberDSEdge
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
		mac = DefaultMACAddressDSEdge
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

	dse, err := new(o, true)
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

func (h *dsedgehandler) StreamHandler() api.StreamHandlerFunc {
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
		logger.Info("DualSense Edge input stream configured",
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
			if streamFrameVersion != StreamFrameVersionV3 &&
				streamFrameVersion != StreamFrameVersionV4 {
				return fmt.Errorf("DualSense Edge speaker output requires framed stream version 0x%02X or 0x%02X",
					StreamFrameVersionV3, StreamFrameVersionV4)
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
			if streamFrameVersion == StreamFrameVersionV4 {
				dse.SetAtomicAudioHapticsCallback(func(feedback OutputState, speakerPCM []byte) {
					data, err := marshalFeedback(feedback)
					if err != nil {
						logger.Error("failed to marshal atomic audio/haptics feedback", "error", err)
						return
					}
					writer.EnqueueAtomicAudioHaptics(data, speakerPCM)
				})
			} else {
				dse.SetSpeakerCallback(writer.EnqueueSpeakerFromUSB)
			}
			dse.SetSpeakerResetCallback(writer.ResetSpeaker)
			defer func() {
				dse.SetOutputCallback(nil)
				dse.SetSpeakerCallback(nil)
				dse.SetAtomicAudioHapticsCallback(nil)
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

func (h *dsedgehandler) UpdateMetaState(meta string, dev *usb.Device) error {
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

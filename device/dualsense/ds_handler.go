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
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usb"
)

func init() {
	api.RegisterDevice(DeviceTypeCombinedAudioDuplexV5, &dshandler{})
	api.RegisterDevice(DeviceTypeAudioOnlyDuplexV5, &dshandler{audioOnly: true})
	api.RegisterDevice(DeviceTypeGamepadOnlyV5, &dshandler{gamepadOnly: true})
	api.RegisterDevice(DeviceTypeCombinedAudioDuplexV5Events,
		&dshandler{micInterfaceEvents: true})
	api.RegisterDevice(DeviceTypeAudioOnlyDuplexV5Events,
		&dshandler{audioOnly: true, micInterfaceEvents: true})
	api.RegisterDevice(DeviceTypeCombinedAudioDuplexV5RawInputEvents,
		&dshandler{micInterfaceEvents: true, physicalInputMetadata: true})
	api.RegisterDevice(DeviceTypeAudioOnlyDuplexV5RawInputEvents,
		&dshandler{audioOnly: true, micInterfaceEvents: true,
			physicalInputMetadata: true})
	api.RegisterDevice(DeviceTypeGamepadOnlyV5RawInput,
		&dshandler{gamepadOnly: true, physicalInputMetadata: true})
}

type dshandler struct {
	audioOnly             bool
	gamepadOnly           bool
	micInterfaceEvents    bool
	physicalInputMetadata bool
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
	identityMu.Lock()
	if _, ok := serials[serial]; ok {
		if len(serial) < 2 {
			serial = DefaultSerialNumberDS
		}
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
		if len(mac) < 2 {
			mac = DefaultMACAddressDS
		}
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
	identityMu.Unlock()

	b, err := json.Marshal(metaState)
	if err != nil {
		identityMu.Lock()
		delete(serials, serial)
		delete(macs, mac)
		identityMu.Unlock()
		return nil, fmt.Errorf("marshal meta state: %w", err)
	}
	o.DeviceSpecific = string(b)

	dse, err := new(o, false)
	if err != nil {
		identityMu.Lock()
		delete(serials, serial)
		delete(macs, mac)
		identityMu.Unlock()
		return nil, err
	}
	if h.audioOnly {
		dse.descriptor = makeAudioOnlyDescriptor(false)
		if h.physicalInputMetadata {
			dse.deviceType = DeviceTypeAudioOnlyDuplexV5RawInputEvents
		} else if h.micInterfaceEvents {
			dse.deviceType = DeviceTypeAudioOnlyDuplexV5Events
		} else {
			dse.deviceType = DeviceTypeAudioOnlyDuplexV5
		}
	} else if h.gamepadOnly {
		dse.descriptor = makeGamepadOnlyDescriptor(false)
		if h.physicalInputMetadata {
			dse.deviceType = DeviceTypeGamepadOnlyV5RawInput
		} else {
			dse.deviceType = DeviceTypeGamepadOnlyV5
		}
	} else if h.physicalInputMetadata {
		dse.deviceType = DeviceTypeCombinedAudioDuplexV5RawInputEvents
	} else if h.micInterfaceEvents {
		dse.deviceType = DeviceTypeCombinedAudioDuplexV5Events
	}
	return dse, nil
}

func (h *dshandler) StreamHandler() api.StreamHandlerFunc {
	return dualSenseV5StreamHandler("DualSense", h.micInterfaceEvents,
		h.physicalInputMetadata)
}

func dualSenseV5StreamHandler(deviceName string,
	micInterfaceEvents, physicalInputMetadata bool) api.StreamHandlerFunc {
	return func(conn net.Conn, devPtr *usb.Device, logger *slog.Logger) error {
		defer releaseDualSenseIdentity(devPtr, deviceName)
		if devPtr == nil || *devPtr == nil {
			return fmt.Errorf("nil device")
		}
		dse, ok := (*devPtr).(*DualSense)
		if !ok {
			return fmt.Errorf("%w: expected %s", device.ErrWrongDeviceType, deviceName)
		}

		logger.Info(deviceName+" V5 stream configured",
			"microphoneInput", true,
			"speakerOutput", true,
			"microphoneInterfaceEvents", micInterfaceEvents,
			"physicalInputMetadata", physicalInputMetadata,
			"frameVersion", StreamFrameVersionV5)

		streamGeneration := dse.beginInputStreamGeneration()
		writer := newDualSenseOutputWriter(conn, dse.beginSpeakerStream(), logger)
		go writer.Run()
		// Capture the media generation at its owning device lock before making
		// callbacks visible. If a reset lands in the registration window, repeat
		// until the writer and device agree; subsequent resets use the tagged
		// callback and cannot relabel old media as new.
		for {
			mediaGeneration := dse.currentSpeakerMediaGeneration()
			writer.SetSpeakerGeneration(mediaGeneration)
			dse.setV5OutputCallbacks(streamGeneration,
				writer.EnqueueOutputState,
				writer.EnqueueAtomicAudioHapticsState,
				writer.EnqueueRealtimeHapticsStateGeneration,
				writer.ResetSpeakerGeneration)
			if dse.currentSpeakerMediaGeneration() == mediaGeneration {
				break
			}
		}
		if micInterfaceEvents {
			dse.setMicrophoneInterfaceStateCallback(streamGeneration,
				writer.EnqueueMicrophoneInterfaceState)
		}
		defer func() {
			if micInterfaceEvents {
				dse.setMicrophoneInterfaceStateCallback(streamGeneration, nil)
			}
			dse.setV5OutputCallbacks(streamGeneration, nil, nil, nil, nil)
			writer.Stop()
		}()

		return readDualSenseV5InputStreamGeneration(conn, dse, logger,
			streamGeneration, physicalInputMetadata)
	}
}

func releaseDualSenseIdentity(devPtr *usb.Device, deviceName string) {
	if devPtr == nil || *devPtr == nil {
		return
	}
	dse, ok := (*devPtr).(*DualSense)
	if !ok {
		slog.Warn("unexpected device type on disconnect", "expected", deviceName)
		return
	}
	dse.metaMu.Lock()
	serial := dse.metaState.SerialNumber
	mac := dse.metaState.MACAddress
	dse.metaMu.Unlock()
	identityMu.Lock()
	delete(serials, serial)
	delete(macs, mac)
	identityMu.Unlock()
	slog.Debug(deviceName+" disconnected, serial/mac released", "serial", serial, "mac", mac)
}

func readDualSenseV5InputStreamGeneration(conn net.Conn, dse *DualSense,
	logger *slog.Logger, streamGeneration uint64,
	physicalInputMetadata bool) error {
	header := make([]byte, StreamFrameHeaderSize)
	input := make([]byte, InputStateRawSize)
	microphonePCM := make([]byte, USBMicrophoneClientFrameSize)
	expectedInputSize := InputStateSize
	if physicalInputMetadata {
		expectedInputSize = InputStateRawSize
	}
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
		if header[4] != StreamFrameVersionV5 {
			return fmt.Errorf("DualSense requires V5 stream version 0x%02X; got 0x%02X",
				StreamFrameVersionV5, header[4])
		}

		frameType := header[5]
		payloadLen := int(binary.LittleEndian.Uint16(header[6:8]))

		var payload []byte
		switch frameType {
		case StreamFrameInputState:
			if payloadLen != expectedInputSize {
				return fmt.Errorf("invalid framed input state length %d, expected %d",
					payloadLen, expectedInputSize)
			}
			payload = input[:expectedInputSize]
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
		var inputReceivedAt time.Time
		var brokerReceivedTicks int64
		if frameType == StreamFrameInputState {
			// This is the first boundary at which the broker owns the complete
			// authenticated V5 frame. Reuse this timestamp as the scheduler's
			// ReceivedAt identity so an opt-in latency recorder can correlate the
			// later terminal transport commit without adding wire metadata.
			inputReceivedAt = time.Now()
			brokerReceivedTicks = inputLatencyCounter()
		}
		measureInput := frameType == StreamFrameInputState &&
			dse.inputTelemetryEnabled.Load()
		var frameReadCompleted time.Time
		if measureInput {
			frameReadCompleted = inputReceivedAt
		}

		sequence := binary.LittleEndian.Uint32(header[8:12])
		if sequenceInitialized && sequence != expectedSequence {
			return fmt.Errorf("DualSense V5 stream sequence mismatch: got %d expected %d", sequence, expectedSequence)
		}
		expectedSequence = sequence + 1
		sequenceInitialized = true

		receivedCRC := binary.LittleEndian.Uint32(header[12:16])
		calculatedCRC := framedStreamCRC(header[4:12], payload)
		if receivedCRC != calculatedCRC {
			return fmt.Errorf("DualSense V5 stream CRC mismatch for sequence %d: got %08X expected %08X", sequence, receivedCRC, calculatedCRC)
		}
		dse.inputTransportTelemetry.lastFrameSequence.Store(sequence)
		dse.inputTransportTelemetry.framesValidated.Add(1)

		switch frameType {
		case StreamFrameInputState:
			dse.inputTransportTelemetry.inputFrames.Add(1)
			corruptReason := inputStatePayloadCorruptionReason(payload)
			if corruptReason != "" {
				return fmt.Errorf("invalid framed input state: %s", corruptReason)
			}
			var state InputState
			if err := state.UnmarshalBinary(payload); err != nil {
				return fmt.Errorf("unmarshal framed input state: %w", err)
			}
			var decodeCompleted time.Time
			if measureInput {
				decodeCompleted = time.Now()
				dse.inputTransportTelemetry.frameReadToDecode.record(
					decodeCompleted.Sub(frameReadCompleted))
			}
			// Register the already-captured broker boundary before publishing the
			// state to the scheduler. An interrupt worker may otherwise commit the
			// report between updateAt returning and recorder registration, losing
			// the only truthful transport-admission correlation.
			traceInputBrokerReceived(
				dse, sequence, inputReceivedAt, brokerReceivedTicks)
			dse.updateInputStateForGenerationAt(
				streamGeneration, &state, inputReceivedAt)
			if measureInput {
				dse.inputTransportTelemetry.decodeToPublish.record(
					time.Since(decodeCompleted))
			}
		case StreamFrameMicrophonePCM:
			dse.QueueMicrophonePCMFrame(microphonePCM)
		}
	}
}

func framedStreamCRC(headerFields, payload []byte) uint32 {
	crc := crc32.Update(0, crc32.IEEETable, headerFields)
	return crc32.Update(crc, crc32.IEEETable, payload)
}

func inputStatePayloadCorruptionReason(input []byte) string {
	if len(input) != InputStateSize && len(input) != InputStateRawSize {
		return fmt.Sprintf("invalid length %d", len(input))
	}
	if len(input) == InputStateRawSize &&
		input[InputStateRawFlagsOffset]&^inputStateRawKnownFlags != 0 {
		return fmt.Sprintf("invalid flags 0x%02X",
			input[InputStateRawFlagsOffset])
	}
	if len(input) == InputStateRawSize {
		flags := input[InputStateRawFlagsOffset]
		if flags&InputStatePhysicalMetadataEdgeLayout != 0 &&
			flags&InputStatePhysicalMetadataValid == 0 {
			return fmt.Sprintf("invalid flags 0x%02X: Edge layout without valid metadata",
				flags)
		}
	}

	buttons := binary.LittleEndian.Uint32(input[4:8])
	dpad := input[8]
	if buttons&^validDualSenseInputButtons != 0 ||
		dpad&^validDualSenseInputDPad != 0 {
		return fmt.Sprintf("invalid controls buttons=0x%08X dpad=0x%02X", buttons, dpad)
	}

	return ""
}

func (h *dshandler) UpdateMetaState(meta string, dev *usb.Device) error {
	dse, ok := (*dev).(*DualSense)
	if !ok {
		return fmt.Errorf("%w: expected DualSenseEdge", device.ErrWrongDeviceType)
	}
	dse.metaMu.Lock()
	current := *dse.metaState
	dse.metaMu.Unlock()
	if err := json.Unmarshal([]byte(meta), &current); err != nil {
		return fmt.Errorf("unmarshal meta state: %w", err)
	}
	dse.SetMetaState(current)
	return nil
}

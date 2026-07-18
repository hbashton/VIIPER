package dualsense

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

var rawOutputLogEnabled = os.Getenv("VIIPER_DUALSENSE_RAW_OUTPUT_LOG") == "1"

type DualSense struct {
	inputCh    chan InputState
	inputState InputState
	metaState  *MetaState

	outputFunc                func(OutputState)
	outputState               OutputState
	descriptor                usb.Descriptor
	extendedFeedback          bool
	combinedBluetoothFeedback bool
	microphoneInput           bool
	streamFrameVersion        byte

	subcommand [2]byte

	seqCounter                uint8
	hapticsSeq                uint8
	hapticsInterval           uint8
	hapticsPCM                []byte
	microphonePCM             []byte
	microphoneSignal          chan struct{}
	speakerInterfaceActive    bool
	microphoneInterfaceActive bool
	corruptUSBInputReports    int
	usbInputReportCount       uint64
	// hapticsPCMStartedAt identifies the oldest PCM frame waiting to make a
	// complete 10.667 ms Bluetooth haptics sample. It is only used by the
	// opt-in traffic capture to expose queueing delay without affecting timing.
	hapticsPCMStartedAt time.Time
	timestampBase       time.Time

	mtx           sync.Mutex
	inputReportMu sync.Mutex
}

func New(o *device.CreateOptions) (*DualSense, error) {
	return new(o, false)
}
func NewEdge(o *device.CreateOptions) (*DualSense, error) {
	return new(o, true)
}

func new(o *device.CreateOptions, edge bool) (*DualSense, error) {
	metaState := &MetaState{
		SerialNumber:       DefaultSerialNumberDS,
		MACAddress:         DefaultMACAddressDS,
		Board:              DefaultBoardStringDS,
		BuildTime:          DefaultBuildTime,
		BatteryStatus:      DefaultBatteryStatus,
		TemperatureCelsius: DefaultTemperature,
		BatteryVoltage:     DefaultVoltage,
		ShellColor:         DefaultShellColor,
	}
	if edge {
		metaState.SerialNumber = DefaultSerialNumberDSEdge
		metaState.MACAddress = DefaultMACAddressDSEdge
		metaState.Board = DefaultBoardStringEdge
	}
	if o != nil && o.DeviceSpecific != "" {
		var newMeta MetaState
		err := json.Unmarshal([]byte(o.DeviceSpecific), &newMeta)
		if err != nil {
			return nil, fmt.Errorf("invalid JSON payload: %w", err)
		}
		if newMeta.SerialNumber != "" {
			metaState.SerialNumber = newMeta.SerialNumber
		}
		if newMeta.MACAddress != "" {
			metaState.MACAddress = newMeta.MACAddress
		}
		if newMeta.Board != "" {
			metaState.Board = newMeta.Board
		}
		if !newMeta.BuildTime.IsZero() {
			metaState.BuildTime = newMeta.BuildTime
		}
		if newMeta.BatteryStatus != 0 {
			metaState.BatteryStatus = newMeta.BatteryStatus
		}
		if newMeta.TemperatureCelsius != 0 {
			metaState.TemperatureCelsius = newMeta.TemperatureCelsius
		}
		if newMeta.BatteryVoltage != 0 {
			metaState.BatteryVoltage = newMeta.BatteryVoltage
		}
		metaState.ShellColor = newMeta.ShellColor
	}

	d := &DualSense{
		descriptor:       makeDescriptor(edge),
		metaState:        metaState,
		microphoneSignal: make(chan struct{}, 1),
	}

	if o != nil {
		if o.IDVendor != nil {
			d.descriptor.Device.IDVendor = *o.IDVendor
		}
		if o.IDProduct != nil {
			d.descriptor.Device.IDProduct = *o.IDProduct
		}
	}

	slog.Info("DualSense device instantiated",
		"edge", edge,
		"vid", d.descriptor.Device.IDVendor,
		"pid", d.descriptor.Device.IDProduct,
		"interfaces", len(d.descriptor.Interfaces))

	d.inputState = *NewInputState()
	d.inputCh = make(chan InputState, 1)
	d.inputCh <- d.inputState
	d.timestampBase = time.Now()

	return d, nil
}

func (d *DualSense) SetMetaState(meta MetaState) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	d.metaState = &meta
}

func (d *DualSense) SetOutputCallback(f func(OutputState)) {
	d.outputFunc = f
}

func (d *DualSense) UpdateInputState(state *InputState) {
	next := *NewInputState()
	if state != nil {
		next = *state
	}

	d.mtx.Lock()
	d.inputState = next
	d.mtx.Unlock()

	select {
	case <-d.inputCh:
	default:
	}
	select {
	case d.inputCh <- next:
	default:
	}
}

func (d *DualSense) GetDescriptor() *usb.Descriptor {
	return &d.descriptor
}

func (d *DualSense) GetDeviceSpecificArgs() map[string]any {
	var res map[string]any
	d.mtx.Lock()
	defer d.mtx.Unlock()

	bytes, err := json.Marshal(d.metaState)
	if err != nil {
		return map[string]any{}
	}
	err = json.Unmarshal(bytes, &res)
	if err != nil {
		return map[string]any{}
	}
	res["speakerInterfaceActive"] = d.speakerInterfaceActive
	res["microphoneInterfaceActive"] = d.microphoneInterfaceActive
	res["queuedMicrophoneBytes"] = len(d.microphonePCM)
	return res
}

func (d *DualSense) SetInterfaceAltSetting(iface, alt uint8) {
	d.mtx.Lock()
	defer d.mtx.Unlock()

	switch iface {
	case InterfaceHapticsAudio:
		d.speakerInterfaceActive = alt != 0
	case InterfaceMicrophone:
		d.microphoneInterfaceActive = alt != 0
		if !d.microphoneInterfaceActive {
			d.microphonePCM = nil
		}
	}
}

func (d *DualSense) HandleTransfer(ctx context.Context, ep uint32, dir uint32, out []byte) []byte {
	// USB/IP carries the endpoint number separately from transfer direction,
	// so an IN descriptor address such as 0x82 arrives here as endpoint 2.
	epNumber := ep & 0x0F
	if dir == usbip.DirIn {
		switch epNumber {
		case EndpointIn & 0x0F:
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					d.mtx.Lock()
					is := d.inputState
					ms := *d.metaState
					d.mtx.Unlock()
					return d.buildUSBInputReport(&is, &ms)
				}
				return nil
			case is := <-d.inputCh:
				d.mtx.Lock()
				ms := *d.metaState
				d.mtx.Unlock()
				return d.buildUSBInputReport(&is, &ms)
			}
		case EndpointMicrophoneIn & 0x0F:
			return d.handleMicrophoneIn(ctx)
		default:
			return nil
		}
	}

	if dir == usbip.DirOut && epNumber == EndpointOut&0x0F {
		recordTrafficBytes("host->device", "interrupt-out",
			out,
			"summary", fmt.Sprintf("ep=%d", ep))
		if d.handleOutputReport(out) {
			return nil
		}
	}
	if dir == usbip.DirOut && epNumber == EndpointHapticsAudioOut&0x0F {
		d.handleHapticsAudioOut(out)
		return nil
	}

	return nil
}

func (d *DualSense) QueueMicrophonePCMFrame(frame []byte) {
	if len(frame) != USBMicrophoneClientFrameSize {
		return
	}

	d.mtx.Lock()
	if !d.microphoneInterfaceActive {
		d.mtx.Unlock()
		return
	}

	const maximumBufferedBytes = USBMicrophoneClientFrameSize * 4
	if overflow := len(d.microphonePCM) + len(frame) - maximumBufferedBytes; overflow > 0 {
		copy(d.microphonePCM, d.microphonePCM[overflow:])
		d.microphonePCM = d.microphonePCM[:len(d.microphonePCM)-overflow]
	}
	d.microphonePCM = append(d.microphonePCM, frame...)
	d.mtx.Unlock()

	recordTrafficSummary("client->device", "microphone-pcm-queued", len(frame),
		"summary", describeMicrophonePCMFrame(frame))

	select {
	case d.microphoneSignal <- struct{}{}:
	default:
	}
}

func (d *DualSense) handleMicrophoneIn(ctx context.Context) []byte {
	for {
		d.mtx.Lock()
		if !d.microphoneInterfaceActive {
			d.mtx.Unlock()
			return make([]byte, USBMicrophonePacketSize)
		}

		if len(d.microphonePCM) > 0 {
			packet := make([]byte, USBMicrophonePacketSize)
			n := copy(packet, d.microphonePCM)
			copy(d.microphonePCM, d.microphonePCM[n:])
			d.microphonePCM = d.microphonePCM[:len(d.microphonePCM)-n]
			d.mtx.Unlock()
			return packet
		}
		d.mtx.Unlock()

		select {
		case <-ctx.Done():
			return make([]byte, USBMicrophonePacketSize)
		case <-d.microphoneSignal:
		case <-time.After(time.Millisecond):
			return make([]byte, USBMicrophonePacketSize)
		}
	}
}

func (d *DualSense) handleHapticsAudioOut(out []byte) {
	if len(out) == 0 {
		return
	}
	receivedAt := time.Now()

	recordTrafficBytes("host->device", "audio-haptics-out",
		out,
		"summary", fmt.Sprintf("ep=%d bytes=%d", EndpointHapticsAudioOut, len(out)))

	d.mtx.Lock()
	if len(d.hapticsPCM) == 0 {
		d.hapticsPCMStartedAt = receivedAt
	}
	d.hapticsPCM = append(d.hapticsPCM, out...)
	reports := d.drainBluetoothHapticsReportsLocked(receivedAt)
	d.mtx.Unlock()

	for _, pending := range reports {
		report := pending.data
		if len(report) == 0 {
			continue
		}

		trafficSource := "saxense-hid-0x32"
		if d.combinedBluetoothFeedback {
			trafficSource = "vds-hid-0x36"
		}

		recordTrafficBytes("device->physical", trafficSource,
			report,
			"reportType", "output",
			"reportID", fmt.Sprintf("0x%02X", report[0]),
			"summary", fmt.Sprintf("from audio ep=%d bytes=%d assemblyMs=%.3f",
				EndpointHapticsAudioOut, BluetoothHapticsSampleSize,
				float64(pending.assemblyDelay.Microseconds())/1000.0))

		if d.outputFunc != nil {
			d.mtx.Lock()
			feedback := d.outputState
			if d.combinedBluetoothFeedback {
				copy(feedback.BluetoothCombinedOutputReport[:], report)
			} else {
				copy(feedback.BluetoothHapticsOutputReport[:], report)
			}
			d.mtx.Unlock()
			dispatchStarted := time.Now()
			d.outputFunc(feedback)
			recordTrafficEvent(TrafficEvent{
				Direction: "device->bridge",
				Source:    "feedback-dispatch",
				Length:    len(report),
				Summary: fmt.Sprintf("report=0x%02X callbackMs=%.3f assemblyMs=%.3f",
					report[0],
					float64(time.Since(dispatchStarted).Microseconds())/1000.0,
					float64(pending.assemblyDelay.Microseconds())/1000.0),
			})
		}
	}
}

type pendingBluetoothHapticsReport struct {
	data          []byte
	assemblyDelay time.Duration
}

func (d *DualSense) drainBluetoothHapticsReportsLocked(now time.Time) []pendingBluetoothHapticsReport {
	const inputBytesPerReport = (BluetoothHapticsSampleSize / 2) *
		USBHapticsAudioDownsample *
		USBHapticsAudioFrameSize

	if len(d.hapticsPCM) < inputBytesPerReport {
		return nil
	}

	reports := make([]pendingBluetoothHapticsReport, 0, len(d.hapticsPCM)/inputBytesPerReport)
	for len(d.hapticsPCM) >= inputBytesPerReport {
		sample := make([]byte, BluetoothHapticsSampleSize)
		copyUSBHapticsChannelsToBluetoothSample(sample, d.hapticsPCM[:inputBytesPerReport])

		seq := d.hapticsSeq
		interval := d.hapticsInterval
		d.hapticsSeq++
		d.hapticsInterval++

		var report []byte
		var err error
		if d.combinedBluetoothFeedback {
			report, err = BuildBluetoothCombinedHapticsReport(seq, interval, sample, d.outputState.RawOutputReport[:])
		} else {
			report, err = BuildBluetoothHapticsReport(seq, interval, sample)
		}
		if err != nil {
			slog.Warn("failed to build DualSense Bluetooth haptics report", "error", err)
		} else {
			assemblyDelay := now.Sub(d.hapticsPCMStartedAt)
			if d.hapticsPCMStartedAt.IsZero() || assemblyDelay < 0 {
				assemblyDelay = 0
			}
			reports = append(reports, pendingBluetoothHapticsReport{
				data:          report,
				assemblyDelay: assemblyDelay,
			})
		}

		copy(d.hapticsPCM, d.hapticsPCM[inputBytesPerReport:])
		d.hapticsPCM = d.hapticsPCM[:len(d.hapticsPCM)-inputBytesPerReport]
		if len(d.hapticsPCM) == 0 {
			d.hapticsPCMStartedAt = time.Time{}
		}
	}

	return reports
}

func copyUSBHapticsChannelsToBluetoothSample(dst []byte, src []byte) {
	const framesPerOutputSample = BluetoothHapticsSampleSize / 2

	for sampleFrame := 0; sampleFrame < framesPerOutputSample; sampleFrame++ {
		blockStart := sampleFrame * USBHapticsAudioDownsample * USBHapticsAudioFrameSize
		var leftSum int32
		var rightSum int32

		for frame := 0; frame < USBHapticsAudioDownsample; frame++ {
			frameStart := blockStart + frame*USBHapticsAudioFrameSize
			leftSum += int32(int16(binary.LittleEndian.Uint16(src[frameStart+4 : frameStart+6])))
			rightSum += int32(int16(binary.LittleEndian.Uint16(src[frameStart+6 : frameStart+8])))
		}

		left := int16(leftSum / USBHapticsAudioDownsample)
		right := int16(rightSum / USBHapticsAudioDownsample)
		dst[sampleFrame*2] = byte(left >> 8)
		dst[sampleFrame*2+1] = byte(right >> 8)
	}
}

func (d *DualSense) HandleControl(bmRequestType, bRequest uint8, wValue, wIndex, wLength uint16, data []byte) ([]byte, bool) {
	if response, handled := handleAudioControlRequest(bmRequestType, bRequest, wValue, wIndex, wLength); handled {
		return response, true
	}

	reportType := uint8(wValue >> 8)
	reportID := uint8(wValue & 0xFF)

	switch bmRequestType {
	case hidClassIN:
		switch bRequest {
		case hidGetReport:
			if reportType == reportTypeInput && reportID == ReportIDInput {
				d.mtx.Lock()
				is := d.inputState
				ms := *d.metaState
				d.mtx.Unlock()
				b := d.buildUSBInputReport(&is, &ms)
				if wLength > 0 && int(wLength) < len(b) {
					b = b[:wLength]
				}
				recordTrafficBytes("device->host", "control-get-report",
					b,
					"request", "GET_REPORT",
					"reportType", describeReportType(reportType),
					"reportID", fmt.Sprintf("0x%02X", reportID),
					"value", fmt.Sprintf("0x%04X", wValue),
					"index", fmt.Sprintf("0x%04X", wIndex),
					"summary", "input report")
				return b, true
			}
			if reportType == reportTypeFeature {
				if fn, ok := featureGetHandlers[reportID]; ok {
					b := fn(d)
					if wLength > 0 && int(wLength) < len(b) {
						b = b[:wLength]
					}
					recordTrafficBytes("device->host", "control-get-report",
						b,
						"request", "GET_REPORT",
						"reportType", describeReportType(reportType),
						"reportID", fmt.Sprintf("0x%02X", reportID),
						"value", fmt.Sprintf("0x%04X", wValue),
						"index", fmt.Sprintf("0x%04X", wIndex),
						"summary", "feature report")
					return b, true
				}
			}
		case hidGetIdle:
			return []byte{0x00}, true
		case hidGetProtocol:
			return []byte{0x01}, true
		}
	case hidClassOUT:
		if bRequest == hidSetReport {
			recordTrafficBytes("host->device", "control-set-report",
				data,
				"request", "SET_REPORT",
				"reportType", describeReportType(reportType),
				"reportID", fmt.Sprintf("0x%02X", reportID),
				"value", fmt.Sprintf("0x%04X", wValue),
				"index", fmt.Sprintf("0x%04X", wIndex))
			switch {
			case reportType == reportTypeFeature && reportID == featureIDCommand && len(data) >= 3:
				d.subcommand[0] = data[1]
				d.subcommand[1] = data[2]
				return nil, true
			case reportType == reportTypeFeature:
				return nil, true
			case reportType == reportTypeOutput && reportID == ReportIDOutput:
				d.handleOutputReport(data)
				return nil, true
			}
		}
	}

	slog.Warn("DualSense control request unhandled",
		"bmRequestType", bmRequestType,
		"bRequest", bRequest,
		"reportType", reportType,
		"reportID", reportID,
		"wIndex", wIndex,
		"wLength", wLength,
		"dataLen", len(data))

	return nil, false
}

const (
	audioClassRequestSetCurrent    = 0x01
	audioClassRequestGetCurrent    = 0x81
	audioClassRequestGetMinimum    = 0x82
	audioClassRequestGetMaximum    = 0x83
	audioClassRequestGetResolution = 0x84

	audioClassEndpointOut = 0x22
	audioClassEndpointIn  = 0xA2

	audioControlSamplingFrequency = 0x01
)

// handleAudioControlRequest implements the UAC1 endpoint sampling-frequency
// controls advertised by the AudioStreaming format descriptor. Windows
// usbaudio validates these requests before it starts the render endpoint.
func handleAudioControlRequest(bmRequestType, bRequest uint8, wValue, wIndex, wLength uint16) ([]byte, bool) {
	endpoint := uint8(wIndex)
	if (endpoint != EndpointHapticsAudioOut && endpoint != EndpointMicrophoneIn) ||
		uint8(wValue>>8) != audioControlSamplingFrequency {
		return nil, false
	}

	switch bmRequestType {
	case audioClassEndpointIn:
		switch bRequest {
		case audioClassRequestGetCurrent, audioClassRequestGetMinimum, audioClassRequestGetMaximum:
			response := []byte{0x80, 0xBB, 0x00} // 48,000 Hz as a 24-bit little-endian value.
			if wLength < uint16(len(response)) {
				response = response[:wLength]
			}
			return response, true
		case audioClassRequestGetResolution:
			response := []byte{0x00, 0x00, 0x00} // One discrete advertised frequency.
			if wLength < uint16(len(response)) {
				response = response[:wLength]
			}
			return response, true
		}
	case audioClassEndpointOut:
		if bRequest == audioClassRequestSetCurrent {
			return nil, true
		}
	}

	return nil, false
}

func (d *DualSense) handleOutputReport(out []byte) bool {
	report, ok := normalizeOutputReport(out)
	if !ok {
		return false
	}
	logRawOutputReport(report)
	if d.outputFunc != nil {
		d.mtx.Lock()
		feedback := d.mergeOutputReport(report)
		d.mtx.Unlock()
		recordTrafficBytes("host->device", "parsed-output-report",
			report,
			"reportType", describeReportType(reportTypeOutput),
			"reportID", fmt.Sprintf("0x%02X", report[0]),
			"decodedOutput", describeOutputState(feedback))
		d.outputFunc(feedback)
	}
	return true
}

func logRawOutputReport(report []byte) {
	if !rawOutputLogEnabled {
		return
	}

	attrs := []any{
		"len", len(report),
		"hex", hex.EncodeToString(report),
	}
	if len(report) > 0 {
		attrs = append(attrs, "reportID", fmt.Sprintf("0x%02X", report[0]))
	}
	if len(report) > 4 {
		attrs = append(attrs,
			"flags0", fmt.Sprintf("0x%02X", report[1]),
			"flags1", fmt.Sprintf("0x%02X", report[2]),
			"rumbleSmall", report[3],
			"rumbleLarge", report[4])
	}
	if len(report) > 31 {
		attrs = append(attrs,
			"r2", hex.EncodeToString(report[11:21]),
			"l2", hex.EncodeToString(report[22:32]))
	}

	slog.Info("DualSense raw host output report", attrs...)
}

func describeReportType(reportType uint8) string {
	switch reportType {
	case reportTypeInput:
		return "input"
	case reportTypeOutput:
		return "output"
	case reportTypeFeature:
		return "feature"
	default:
		return fmt.Sprintf("0x%02X", reportType)
	}
}

func normalizeOutputReport(out []byte) ([]byte, bool) {
	if len(out) == 0 {
		return nil, false
	}
	if out[0] == ReportIDOutput {
		if len(out) < 5 {
			return nil, false
		}
		return out, true
	}
	// Some HID SET_REPORT paths deliver the payload without the report ID byte.
	// Add it back so the parser can use the same USB report offsets.
	if len(out) >= 4 {
		report := make([]byte, len(out)+1)
		report[0] = ReportIDOutput
		copy(report[1:], out)
		return report, true
	}
	return nil, false
}

var featureGetHandlers = map[byte]func(*DualSense) []byte{
	featureIDCalibration:     (*DualSense).featureReportCalibration,
	featureIDPairing:         (*DualSense).featureReportPairing,
	featureIDFirmware:        (*DualSense).featureReportFirmware,
	featureIDCommandResponse: (*DualSense).featureReportCommandResponse,
}

func (d *DualSense) mergeOutputReport(out []byte) OutputState {
	feedback := d.outputState
	clear(feedback.BluetoothHapticsOutputReport[:])
	clear(feedback.BluetoothCombinedOutputReport[:])
	if len(out) >= OutputReportSize {
		copy(feedback.RawOutputReport[:], out[:OutputReportSize])
	}

	if len(out) > 4 {
		flag0 := out[1]
		compatibleVibration := flag0&0x01 != 0
		if len(out) > 39 {
			compatibleVibration = compatibleVibration || out[39]&0x04 != 0
		}
		if compatibleVibration {
			feedback.RumbleSmall = out[3]
			feedback.RumbleLarge = out[4]
		}
	}
	if len(out) > 2 {
		flag1 := out[2]
		if flag1&0x04 != 0 && len(out) > 47 {
			feedback.LedRed = out[45]
			feedback.LedGreen = out[46]
			feedback.LedBlue = out[47]
		}
		if flag1&0x10 != 0 && len(out) > 44 {
			feedback.PlayerLeds = out[44]
		}
	}
	if len(out) > 31 {
		flag0 := out[1]
		if flag0&0x04 != 0 {
			feedback.TriggerR2Mode = out[11]
			feedback.TriggerR2StartResistance = out[12]
			feedback.TriggerR2EffectForce = out[13]
			feedback.TriggerR2RangeForce = out[14]
			feedback.TriggerR2NearReleaseStrength = out[15]
			feedback.TriggerR2NearMiddleStrength = out[16]
			feedback.TriggerR2PressedStrength = out[17]
			feedback.TriggerR2Frequency = out[20]
		}
		if flag0&0x08 != 0 {
			feedback.TriggerL2Mode = out[22]
			feedback.TriggerL2StartResistance = out[23]
			feedback.TriggerL2EffectForce = out[24]
			feedback.TriggerL2RangeForce = out[25]
			feedback.TriggerL2NearReleaseStrength = out[26]
			feedback.TriggerL2NearMiddleStrength = out[27]
			feedback.TriggerL2PressedStrength = out[28]
			feedback.TriggerL2Frequency = out[31]
		}
	}
	d.outputState = feedback
	return feedback
}

func (d *DualSense) featureReportCalibration() []byte {
	report := make([]byte, 41)
	report[0] = featureIDCalibration

	for i, v := range [17]int16{
		0, 0, 0,
		8192, -8192, 8192, -8192, 8192, -8192,
		500, 500,
		8192, -8192, 8192, -8192, 8192, -8192,
	} {
		binary.LittleEndian.PutUint16(report[1+i*2:], uint16(v))
	}

	report[35] = 0x0B // TODO:
	return report
}

func (d *DualSense) featureReportPairing() []byte {
	report := make([]byte, 20)
	report[0] = featureIDPairing

	d.mtx.Lock()
	mac := d.metaState.MACAddress
	d.mtx.Unlock()

	if hw, err := net.ParseMAC(mac); err == nil && len(hw) == 6 {
		for i := range 6 {
			report[1+i] = hw[5-i]
		}
	}

	// TODO:
	report[7] = 0x08
	report[8] = 0x25
	report[10] = 0x1E
	report[12] = 0xEE
	report[13] = 0x74
	report[14] = 0xD0
	report[15] = 0xBC
	return report
}

func (d *DualSense) featureReportFirmware() []byte {
	report := make([]byte, 64)
	report[0] = featureIDFirmware

	d.mtx.Lock()
	bt := d.metaState.BuildTime
	d.mtx.Unlock()

	copy(report[1:12], bt.Format("Jan 02 2006"))
	copy(report[12:20], bt.Format("15:04:05"))

	report[20] = HardwareType
	report[21] = 0x01 // TODO: unknown
	report[22] = 0x44 // TODO: put in CONST!!! // build revision from real device

	binary.LittleEndian.PutUint32(report[24:28], HwInfo)

	// TODO: unknown
	report[28] = 0x36
	report[31] = 0x01
	report[32] = 0xC1
	report[33] = 0xC8

	binary.LittleEndian.PutUint16(report[44:46], FirmwareVersion)

	// TODO: unknown
	report[48] = 0x14
	report[52] = 0x0B
	report[54] = 0x01
	report[56] = 0x06
	return report
}

func (d *DualSense) featureReportCommandResponse() []byte {
	report := make([]byte, 64)
	report[0] = featureIDCommandResponse

	d.mtx.Lock()
	sub := d.subcommand
	serial := d.metaState.SerialNumber
	voltage := d.metaState.BatteryVoltage
	temp := d.metaState.TemperatureCelsius
	d.mtx.Unlock()

	switch sub[0] {
	case subcmdSerial:
		copy(report[3:21], serial)
	case subcmdStatus:
		// nvs locked
		report[1] = 0x01
		report[4] = 0x01
	case subcmdSensors:
		vRaw := uint16(math.Round(voltage * 1000))
		report[4] = byte(vRaw)
		report[5] = byte(vRaw >> 8)
		tRaw := uint16(math.Max(0, math.Min(4095, math.Round((2470.0-temp*26.0)/0.78125))))
		report[6] = byte(tRaw)
		report[7] = byte(tRaw >> 8)
	default:
		slog.Warn("DualSense: unknown sub-command for featureIDCommandResponse",
			"sub0", sub[0], "sub1", sub[1])
		report[1] = 0x01
	}
	return report
}

func (d *DualSense) buildUSBInputReport(s *InputState, m *MetaState) []byte {
	d.inputReportMu.Lock()
	defer d.inputReportMu.Unlock()

	b := make([]byte, InputReportSize)
	d.usbInputReportCount++
	reportCount := d.usbInputReportCount

	b[0] = ReportIDInput

	b[1] = uint8(int16(s.LX) + 128)
	b[2] = uint8(int16(s.LY) + 128)
	b[3] = uint8(int16(s.RX) + 128)
	b[4] = uint8(int16(s.RY) + 128)

	b[5] = s.L2
	b[6] = s.R2

	d.seqCounter++
	b[7] = d.seqCounter

	usbDPad := uint8(DPadUSBNeutral)
	switch {
	case s.DPad&DPadUp != 0 && s.DPad&DPadRight != 0:
		usbDPad = DPadUSBUpRight
	case s.DPad&DPadUp != 0 && s.DPad&DPadLeft != 0:
		usbDPad = DPadUSBUpLeft
	case s.DPad&DPadDown != 0 && s.DPad&DPadRight != 0:
		usbDPad = DPadUSBDownRight
	case s.DPad&DPadDown != 0 && s.DPad&DPadLeft != 0:
		usbDPad = DPadUSBDownLeft
	case s.DPad&DPadUp != 0:
		usbDPad = DPadUSBUp
	case s.DPad&DPadDown != 0:
		usbDPad = DPadUSBDown
	case s.DPad&DPadLeft != 0:
		usbDPad = DPadUSBLeft
	case s.DPad&DPadRight != 0:
		usbDPad = DPadUSBRight
	}
	b[8] = (usbDPad & DPadMask) | (uint8(s.Buttons) & 0xF0)
	b[9] = uint8(s.Buttons >> 8)
	b[10] = uint8(s.Buttons >> 16)

	binary.LittleEndian.PutUint16(b[16:18], uint16(s.GyroX))
	binary.LittleEndian.PutUint16(b[18:20], uint16(s.GyroY))
	binary.LittleEndian.PutUint16(b[20:22], uint16(s.GyroZ))

	binary.LittleEndian.PutUint16(b[22:24], uint16(s.AccelX))
	binary.LittleEndian.PutUint16(b[24:26], uint16(s.AccelY))
	binary.LittleEndian.PutUint16(b[26:28], uint16(s.AccelZ))

	ts := uint32(time.Since(d.timestampBase).Microseconds() * 3)
	binary.LittleEndian.PutUint32(b[28:32], ts)

	b[33] = normalizeTouchTracking(s.Touch1Active, s.Touch1Tracking)
	encodeTouchCoords(b[34:37], s.Touch1X, s.Touch1Y)

	b[37] = normalizeTouchTracking(s.Touch2Active, s.Touch2Tracking)
	encodeTouchCoords(b[38:41], s.Touch2X, s.Touch2Y)

	b[41] = d.seqCounter
	binary.LittleEndian.PutUint32(b[49:53], ts)
	battery := byte(0)
	if m != nil {
		battery = m.BatteryStatus
	}
	b[53] = battery

	corruptReason := ""
	if inputStateControlsInvalid(s) {
		corruptReason = "invalid input control bits"
	}

	if corruptReason != "" {
		d.corruptUSBInputReports++
		count := d.corruptUSBInputReports
		if count <= 128 || isPowerOfTwo(count) {
			slog.Warn("DualSense USB input report was corrupt; report reset to neutral",
				"count", count,
				"reason", corruptReason)
		}
		recordTrafficBytes("device->host", "usb-input-report-before-reset",
			b,
			"summary", describeUSBInputReport(b, reportCount, corruptReason))
		resetUSBInputReportToNeutral(b, d.seqCounter, ts, battery)
	}

	recordTrafficBytes("device->host", "usb-input-report",
		b,
		"summary", describeUSBInputReport(b, reportCount, corruptReason))

	return b
}

func inputStateControlsInvalid(s *InputState) bool {
	if s == nil {
		return false
	}
	return s.Buttons&^validDualSenseInputButtons != 0 ||
		s.DPad&^validDualSenseInputDPad != 0
}

func describeUSBInputReport(b []byte, count uint64, resetReason string) string {
	if len(b) < 54 {
		return fmt.Sprintf("count=%d len=%d resetReason=%s", count, len(b), resetReason)
	}

	ts := binary.LittleEndian.Uint32(b[28:32])
	return fmt.Sprintf(
		"count=%d reportId=0x%02X seq=%d lx=%d ly=%d rx=%d ry=%d l2=%d r2=%d raw8=0x%02X raw9=0x%02X raw10=0x%02X dpadUsb=0x%X touch1=0x%02X touch2=0x%02X ts=%d battery=0x%02X fullMagic=%t markerFrag=%t micLeak=%t resetReason=%s",
		count,
		b[0],
		b[7],
		b[1],
		b[2],
		b[3],
		b[4],
		b[5],
		b[6],
		b[8],
		b[9],
		b[10],
		b[8]&DPadMask,
		b[33],
		b[37],
		ts,
		b[53],
		containsStreamMagic(b),
		containsStreamMarkerFragment(b, len(b)),
		containsMicTransportLeakPattern(b[16:41]),
		resetReason)
}

func describeMicrophonePCMFrame(frame []byte) string {
	const sampleWidth = 2
	if len(frame) < sampleWidth {
		return fmt.Sprintf("len=%d", len(frame))
	}

	var sumAbs uint64
	var peak uint16
	sampleCount := 0
	for i := 0; i+1 < len(frame); i += sampleWidth {
		raw := binary.LittleEndian.Uint16(frame[i : i+2])
		sample := int32(int16(raw))
		if sample < 0 {
			sample = -sample
		}
		if uint16(sample) > peak {
			peak = uint16(sample)
		}
		sumAbs += uint64(sample)
		sampleCount++
	}

	avg := uint64(0)
	if sampleCount > 0 {
		avg = sumAbs / uint64(sampleCount)
	}

	return fmt.Sprintf("pcmLen=%d samples=%d peak=%d avgAbs=%d", len(frame), sampleCount, peak, avg)
}

func resetUSBInputReportToNeutral(b []byte, seq uint8, timestamp uint32, battery byte) {
	for i := range b {
		b[i] = 0
	}

	b[0] = ReportIDInput
	b[1] = 128
	b[2] = 128
	b[3] = 128
	b[4] = 128
	b[7] = seq
	b[8] = DPadUSBNeutral

	x, y, z := DefaultAccelRaw()
	binary.LittleEndian.PutUint16(b[22:24], uint16(x))
	binary.LittleEndian.PutUint16(b[24:26], uint16(y))
	binary.LittleEndian.PutUint16(b[26:28], uint16(z))
	binary.LittleEndian.PutUint32(b[28:32], timestamp)

	b[33] = TouchInactiveMask
	b[37] = TouchInactiveMask
	b[41] = seq
	binary.LittleEndian.PutUint32(b[49:53], timestamp)
	b[53] = battery
}

func normalizeTouchTracking(active bool, tracking uint8) uint8 {
	if active {
		return tracking &^ TouchInactiveMask
	}
	if tracking == 0 {
		return TouchInactiveMask
	}
	return tracking | TouchInactiveMask
}

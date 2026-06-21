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
	inputCh    chan *InputState
	inputState *InputState
	metaState  *MetaState

	outputFunc       func(OutputState)
	outputState      OutputState
	descriptor       usb.Descriptor
	extendedFeedback bool

	subcommand [2]byte

	seqCounter      uint8
	hapticsSeq      uint8
	hapticsInterval uint8
	hapticsPCM      []byte
	timestampBase   time.Time

	mtx sync.Mutex
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
		descriptor: makeDescriptor(edge),
		metaState:  metaState,
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

	d.inputState = NewInputState()
	d.inputCh = make(chan *InputState, 1)
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
	d.mtx.Lock()
	d.inputState = state
	d.mtx.Unlock()
	select {
	case <-d.inputCh:
	default:
	}
	d.inputCh <- state
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
	return res
}

func (d *DualSense) HandleTransfer(ctx context.Context, ep uint32, dir uint32, out []byte) []byte {
	if dir == usbip.DirIn {
		switch ep {
		case 4:
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					d.mtx.Lock()
					is := d.inputState
					ms := *d.metaState
					d.mtx.Unlock()
					return d.buildUSBInputReport(is, &ms)
				}
				return nil
			case is := <-d.inputCh:
				d.mtx.Lock()
				ms := *d.metaState
				d.mtx.Unlock()
				return d.buildUSBInputReport(is, &ms)
			}
		default:
			return nil
		}
	}

	if dir == usbip.DirOut && ep == EndpointOut {
		recordTrafficBytes("host->device", "interrupt-out",
			out,
			"summary", fmt.Sprintf("ep=%d", ep))
		if d.handleOutputReport(out) {
			return nil
		}
	}
	if dir == usbip.DirOut && ep == EndpointHapticsAudioOut {
		d.handleHapticsAudioOut(out)
		return nil
	}

	return nil
}

func (d *DualSense) handleHapticsAudioOut(out []byte) {
	if len(out) == 0 {
		return
	}

	recordTrafficBytes("host->device", "audio-haptics-out",
		out,
		"summary", fmt.Sprintf("ep=%d bytes=%d", EndpointHapticsAudioOut, len(out)))

	d.mtx.Lock()
	d.hapticsPCM = append(d.hapticsPCM, out...)
	reports := d.drainBluetoothHapticsReportsLocked()
	d.mtx.Unlock()

	for _, report := range reports {
		if len(report) == 0 {
			continue
		}

		recordTrafficBytes("device->physical", "saxense-hid-0x32",
			report,
			"reportType", "output",
			"reportID", fmt.Sprintf("0x%02X", report[0]),
			"summary", fmt.Sprintf("from audio ep=%d bytes=%d", EndpointHapticsAudioOut, BluetoothHapticsSampleSize))

		if d.outputFunc != nil {
			d.mtx.Lock()
			feedback := d.outputState
			copy(feedback.BluetoothHapticsOutputReport[:], report)
			d.mtx.Unlock()
			d.outputFunc(feedback)
		}
	}
}

func (d *DualSense) drainBluetoothHapticsReportsLocked() [][]byte {
	const inputBytesPerReport = (BluetoothHapticsSampleSize / 2) *
		USBHapticsAudioDownsample *
		USBHapticsAudioFrameSize

	if len(d.hapticsPCM) < inputBytesPerReport {
		return nil
	}

	reports := make([][]byte, 0, len(d.hapticsPCM)/inputBytesPerReport)
	for len(d.hapticsPCM) >= inputBytesPerReport {
		sample := make([]byte, BluetoothHapticsSampleSize)
		copyUSBHapticsChannelsToBluetoothSample(sample, d.hapticsPCM[:inputBytesPerReport])

		seq := d.hapticsSeq
		interval := d.hapticsInterval
		d.hapticsSeq++
		d.hapticsInterval++

		report, err := BuildBluetoothHapticsReport(seq, interval, sample)
		if err != nil {
			slog.Warn("failed to build DualSense Bluetooth haptics report", "error", err)
		} else {
			reports = append(reports, report)
		}

		copy(d.hapticsPCM, d.hapticsPCM[inputBytesPerReport:])
		d.hapticsPCM = d.hapticsPCM[:len(d.hapticsPCM)-inputBytesPerReport]
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
				is := *d.inputState
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
	if uint8(wIndex) != EndpointHapticsAudioOut || uint8(wValue>>8) != audioControlSamplingFrequency {
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
	b := make([]byte, InputReportSize)

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
	b[53] = m.BatteryStatus

	return b
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

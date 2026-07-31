package dualsense

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/device/internal/microphonebuffer"
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

const (
	microphoneTargetClientFrames  = 6  // 60 ms absorbs independent radio/client and virtual USB scheduling jitter.
	microphoneMaximumClientFrames = 20 // 200 ms emergency ceiling for full-duplex BT bursts; steady state remains about 55 ms.
)

type DualSense struct {
	deviceType string
	inputCh    chan InputState
	inputState InputState
	metaState  *MetaState

	atomicAudioHapticsFunc func(OutputState, []byte)
	speakerResetFunc       func()
	outputFunc             func(OutputState)
	outputState            OutputState
	descriptor             usb.Descriptor

	subcommand [2]byte

	seqCounter                uint8
	hapticsSeq                uint8
	hapticsInterval           uint8
	hapticsPCM                []byte
	v5SpeakerPCM              []byte
	v5HapticsQueue            []dualSenseV5HapticsGeneration
	microphoneBuffer          microphonebuffer.Buffer
	microphoneSignal          chan struct{}
	speakerAudioFeature       audioFeatureState
	microphoneAudioFeature    audioFeatureState
	speakerStreamTelemetry    *dualSenseSpeakerStreamTelemetry
	speakerInterfaceActive    bool
	microphoneInterfaceActive bool
	corruptUSBInputReports    int
	// hapticsPCMStartedAt identifies the oldest PCM frame waiting to make a
	// complete 10.667 ms Bluetooth haptics sample. It feeds ordinary stream
	// health telemetry without affecting presentation timing.
	hapticsPCMStartedAt time.Time
	timestampBase       time.Time

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
		deviceType:             DeviceTypeCombinedAudioDuplexV5,
		descriptor:             makeDescriptor(edge),
		metaState:              metaState,
		speakerAudioFeature:    newSpeakerAudioFeatureState(),
		microphoneAudioFeature: newMicrophoneAudioFeatureState(),
		microphoneBuffer: microphonebuffer.New(
			USBMicrophonePacketSize,
			USBMicrophoneChannels*USBMicrophoneBytesPerSample,
			USBMicrophoneClientFrameSize,
			microphoneTargetClientFrames,
			microphoneMaximumClientFrames,
		),
		microphoneSignal: make(chan struct{}, 1),
	}
	if edge {
		d.deviceType = DeviceTypeEdgeCombinedAudioDuplexV5
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

// VIIPERDeviceType preserves the exact registered transport contract for
// stream dispatch. Multiple V5 endpoint variants share the DualSense concrete
// type, so reflection on the package name cannot distinguish them.
func (d *DualSense) VIIPERDeviceType() string {
	return d.deviceType
}

func (d *DualSense) SetMetaState(meta MetaState) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	d.metaState = &meta
}

func (d *DualSense) SetOutputCallback(f func(OutputState)) {
	d.mtx.Lock()
	d.outputFunc = f
	d.mtx.Unlock()
}

// SetAtomicAudioHapticsCallback installs the V5 transport consumer. Each
// callback contains native feedback and one front-channel PCM generation.
// The PadSense contract emits exactly 480
// raw 48 kHz speaker frames and consumes one independently completed rear
// haptics sample, or silence when that 512-frame lane has not completed yet.
func (d *DualSense) SetAtomicAudioHapticsCallback(f func(OutputState, []byte)) {
	d.mtx.Lock()
	d.atomicAudioHapticsFunc = f
	d.mtx.Unlock()
}

// SetSpeakerResetCallback installs the transport-side queue reset paired with
// SetAtomicAudioHapticsCallback. USB interface close/reopen and endpoint reset
// must discard queued speaker PCM from the previous presentation generation.
func (d *DualSense) SetSpeakerResetCallback(f func()) {
	d.mtx.Lock()
	d.speakerResetFunc = f
	d.mtx.Unlock()
}

// beginSpeakerStream gives each stream generation independent telemetry. An
// older writer can therefore finish a callback without changing the state
// exposed for a replacement connection.
func (d *DualSense) beginSpeakerStream() *dualSenseSpeakerStreamTelemetry {
	telemetry := &dualSenseSpeakerStreamTelemetry{}
	d.mtx.Lock()
	d.speakerStreamTelemetry = telemetry
	d.mtx.Unlock()
	return telemetry
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
	speakerState := d.speakerStreamTelemetry.snapshot()
	res["speakerStreamActive"] = speakerState.Active
	res["speakerPayloadsReceived"] = speakerState.ReceivedPayloads
	res["speakerBytesReceived"] = speakerState.ReceivedBytes
	res["speakerPayloadsEnqueued"] = speakerState.EnqueuedPayloads
	res["speakerBytesEnqueued"] = speakerState.EnqueuedBytes
	res["speakerPayloadsDropped"] = speakerState.DroppedPayloads
	res["speakerBytesDropped"] = speakerState.DroppedBytes
	res["speakerPayloadsWritten"] = speakerState.WrittenPayloads
	res["speakerBytesWritten"] = speakerState.WrittenBytes
	res["speakerWriteFailures"] = speakerState.WriteFailures
	res["speakerQueueDepth"] = speakerState.QueueDepth
	res["speakerQueueHighWater"] = speakerState.QueueHighWater
	res["speakerMaxEnqueueGapUS"] = speakerState.MaxEnqueueGapUS
	res["speakerMaxWriteGapUS"] = speakerState.MaxWriteGapUS
	res["microphoneInterfaceActive"] = d.microphoneInterfaceActive
	microphoneState := d.microphoneBuffer.State()
	res["queuedMicrophoneBytes"] = microphoneState.QueuedBytes
	res["microphoneQueueTargetBytes"] = microphoneState.TargetBytes
	res["microphoneQueueMaximumBytes"] = microphoneState.MaximumBytes
	res["microphoneFilteredQueueBytes"] = microphoneState.FilteredBytes
	res["microphoneQueuePrimed"] = microphoneState.Primed
	res["microphoneUnderruns"] = microphoneState.Underruns
	res["microphoneReprimes"] = microphoneState.Reprimes
	res["microphoneDroppedBytes"] = microphoneState.DroppedBytes
	res["microphonePacketsRead"] = microphoneState.PacketsRead
	res["microphoneZeroPackets"] = microphoneState.ZeroPackets
	res["microphoneOverflowEvents"] = microphoneState.OverflowEvents
	res["microphoneShortPackets"] = microphoneState.ShortPackets
	res["microphoneLongPackets"] = microphoneState.LongPackets
	res["microphoneServoRatePPM"] = microphoneState.ServoRatePPM
	res["microphoneLowWaterBytes"] = microphoneState.LowWaterBytes
	res["microphoneHighWaterBytes"] = microphoneState.HighWaterBytes
	res["microphoneQueueFrames"] = microphoneState.QueueFrames
	res["microphoneQueueFastGaps"] = microphoneState.QueueFastGaps
	res["microphoneQueueLateGaps"] = microphoneState.QueueLateGaps
	res["microphoneQueueMinGapUS"] = microphoneState.QueueMinGapUS
	res["microphoneQueueMaxGapUS"] = microphoneState.QueueMaxGapUS
	res["microphoneReadFastGaps"] = microphoneState.ReadFastGaps
	res["microphoneReadLateGaps"] = microphoneState.ReadLateGaps
	res["microphoneReadMinGapUS"] = microphoneState.ReadMinGapUS
	res["microphoneReadMaxGapUS"] = microphoneState.ReadMaxGapUS
	return res
}

func (d *DualSense) SetInterfaceAltSetting(iface, alt uint8) {
	d.mtx.Lock()
	var resetSpeaker func()
	switch iface {
	case InterfaceHapticsAudio:
		d.speakerInterfaceActive = alt != 0
		d.resetSpeakerAudioLocked()
		resetSpeaker = d.speakerResetFunc
	case InterfaceMicrophone:
		d.microphoneInterfaceActive = alt != 0
		d.resetMicrophoneAudioLocked()
	}
	d.mtx.Unlock()

	if resetSpeaker != nil {
		resetSpeaker()
	}
}

// ResetEndpoint implements usb.EndpointResetDevice. A standard endpoint pipe
// reset preserves the selected alternate setting and feature controls while
// discarding all transport data from the previous endpoint generation.
func (d *DualSense) ResetEndpoint(endpoint uint8) {
	d.mtx.Lock()
	var resetSpeaker func()
	switch endpoint {
	case EndpointHapticsAudioOut:
		d.resetSpeakerAudioLocked()
		resetSpeaker = d.speakerResetFunc
	case EndpointMicrophoneIn:
		d.resetMicrophoneAudioLocked()
	}
	d.mtx.Unlock()

	if resetSpeaker != nil {
		resetSpeaker()
	}
}

func (d *DualSense) resetSpeakerAudioLocked() {
	d.hapticsPCM = nil
	d.v5SpeakerPCM = nil
	d.v5HapticsQueue = nil
	d.hapticsPCMStartedAt = time.Time{}
	d.speakerAudioFeature.resetStreamGain()
}

func (d *DualSense) resetMicrophoneAudioLocked() {
	d.microphoneBuffer.Reset()
	d.drainMicrophoneSignal()
	d.microphoneAudioFeature.resetStreamGain()
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

	d.microphoneBuffer.QueueFrame(frame)
	d.mtx.Unlock()

	select {
	case d.microphoneSignal <- struct{}{}:
	default:
	}
}

// ResetMicrophonePCM clears capture transport state after the current API
// stream ends. The API generation coordinator suppresses this reset when that
// stream was displaced by a same-device replacement.
func (d *DualSense) ResetMicrophonePCM() {
	d.mtx.Lock()
	d.resetMicrophoneAudioLocked()
	d.mtx.Unlock()
}

func (d *DualSense) handleMicrophoneIn(ctx context.Context) []byte {
	packet := make([]byte, USBMicrophoneMaxPacketSize)
	for {
		d.mtx.Lock()
		if !d.microphoneInterfaceActive {
			d.microphoneBuffer.RecordZeroPacket()
			d.mtx.Unlock()
			return packet[:USBMicrophonePacketSize]
		}

		if actualLength, ok := d.microphoneBuffer.ReadPacket(packet); ok {
			d.microphoneAudioFeature.applyPCMInPlace(
				packet[:actualLength], USBMicrophoneChannels,
			)
			d.mtx.Unlock()
			return packet[:actualLength]
		}
		d.mtx.Unlock()

		select {
		case <-ctx.Done():
			d.mtx.Lock()
			d.microphoneBuffer.RecordZeroPacket()
			d.mtx.Unlock()
			return packet[:USBMicrophonePacketSize]
		case <-d.microphoneSignal:
		case <-time.After(time.Millisecond):
			d.mtx.Lock()
			d.microphoneBuffer.RecordZeroPacket()
			d.mtx.Unlock()
			return packet[:USBMicrophonePacketSize]
		}
	}
}

func (d *DualSense) drainMicrophoneSignal() {
	for {
		select {
		case <-d.microphoneSignal:
		default:
			return
		}
	}
}

func (d *DualSense) handleHapticsAudioOut(out []byte) {
	if len(out) == 0 {
		return
	}
	receivedAt := time.Now()

	d.mtx.Lock()
	if !d.speakerInterfaceActive {
		d.mtx.Unlock()
		return
	}

	processed, release := d.speakerAudioFeature.applyPCM(out, USBHapticsAudioChannels)
	reports := d.consumeDualSenseV5AudioLocked(processed, receivedAt)
	// The callback is deliberately completed under the device lock. This makes
	// an alternate-setting or endpoint reset a hard generation barrier: once the
	// reset acquires the lock, no pre-reset callback can enqueue stale PCM after
	// the transport queue has been flushed.
	d.mtx.Unlock()
	if release != nil {
		release()
	}

	for _, pending := range reports {
		report := pending.feedback.BluetoothCombinedOutputReport[:]
		if len(report) == 0 {
			continue
		}

		d.mtx.Lock()
		outputFunc := d.outputFunc
		atomicAudioHapticsFunc := d.atomicAudioHapticsFunc
		if outputFunc != nil || atomicAudioHapticsFunc != nil {
			feedback := pending.feedback
			d.mtx.Unlock()
			if atomicAudioHapticsFunc != nil {
				atomicAudioHapticsFunc(feedback, pending.speakerPCM)
			} else {
				outputFunc(feedback)
			}
		} else {
			d.mtx.Unlock()
		}
	}
}

type pendingBluetoothHapticsReport struct {
	speakerPCM    []byte
	assemblyDelay time.Duration
	feedback      OutputState
}

type dualSenseV5HapticsGeneration struct {
	sample        [BluetoothHapticsSampleSize]byte
	assemblyDelay time.Duration
}

// consumeDualSenseV5AudioLocked advances the native USB stream in source
// order while keeping its two media clocks independent. Front stereo is
// published every 480 frames. Rear haptics completes every 512 frames and is
// queued independently. At each speaker boundary, exactly one completed rear
// sample is consumed; if none is ready, PadSense sends silence rather than
// replaying the previous sample. State and report counters are rebuilt at that
// same 480-frame boundary so every emitted report is current and sequential.
func (d *DualSense) consumeDualSenseV5AudioLocked(src []byte,
	now time.Time) []pendingBluetoothHapticsReport {
	const hapticsFrames = (BluetoothHapticsSampleSize / 2) *
		USBHapticsAudioDownsample

	framesRemaining := len(src) / USBHapticsAudioFrameSize
	if framesRemaining == 0 {
		return nil
	}

	reports := make([]pendingBluetoothHapticsReport, 0,
		(framesRemaining+len(d.v5SpeakerPCM)/dualSenseV5SpeakerFrameSize)/
			dualSenseV5SpeakerFrames)
	sourceOffset := 0
	for framesRemaining > 0 {
		speakerFramesNeeded := dualSenseV5SpeakerFrames -
			len(d.v5SpeakerPCM)/dualSenseV5SpeakerFrameSize
		hapticsFramesNeeded := hapticsFrames -
			len(d.hapticsPCM)/USBHapticsAudioFrameSize
		frames := min(framesRemaining, speakerFramesNeeded, hapticsFramesNeeded)
		segmentBytes := frames * USBHapticsAudioFrameSize
		segment := src[sourceOffset : sourceOffset+segmentBytes]

		if len(d.hapticsPCM) == 0 {
			d.hapticsPCMStartedAt = now
		}
		d.hapticsPCM = append(d.hapticsPCM, segment...)
		d.v5SpeakerPCM = appendDualSenseV5Speaker(d.v5SpeakerPCM, segment)

		framesRemaining -= frames
		sourceOffset += segmentBytes

		// At the 7,680-frame common boundary, complete rear feedback first so
		// the simultaneous speaker generation carries that exact update.
		if len(d.hapticsPCM) == hapticsFrames*USBHapticsAudioFrameSize {
			d.completeDualSenseV5HapticsLocked(now)
		}
		if len(d.v5SpeakerPCM) == dualSenseV5SpeakerPayloadSize {
			feedback, assemblyDelay, ok :=
				d.buildDualSenseV5FeedbackLocked()
			if ok {
				reports = append(reports, pendingBluetoothHapticsReport{
					speakerPCM:    d.v5SpeakerPCM,
					assemblyDelay: assemblyDelay,
					feedback:      feedback,
				})
			}
			d.v5SpeakerPCM = nil
		}
	}

	return reports
}

func (d *DualSense) completeDualSenseV5HapticsLocked(now time.Time) {
	generation := dualSenseV5HapticsGeneration{}
	copyUSBHapticsChannelsToBluetoothSample(generation.sample[:], d.hapticsPCM)
	generation.assemblyDelay = now.Sub(d.hapticsPCMStartedAt)
	if d.hapticsPCMStartedAt.IsZero() || generation.assemblyDelay < 0 {
		generation.assemblyDelay = 0
	}
	d.v5HapticsQueue = append(d.v5HapticsQueue, generation)
	d.hapticsPCM = d.hapticsPCM[:0]
	d.hapticsPCMStartedAt = time.Time{}
}

func (d *DualSense) buildDualSenseV5FeedbackLocked() (OutputState,
	time.Duration, bool) {
	var sample [BluetoothHapticsSampleSize]byte
	var assemblyDelay time.Duration
	if len(d.v5HapticsQueue) != 0 {
		generation := d.v5HapticsQueue[0]
		sample = generation.sample
		assemblyDelay = generation.assemblyDelay
		copy(d.v5HapticsQueue, d.v5HapticsQueue[1:])
		last := len(d.v5HapticsQueue) - 1
		d.v5HapticsQueue[last] = dualSenseV5HapticsGeneration{}
		d.v5HapticsQueue = d.v5HapticsQueue[:last]
	}

	sequence := d.hapticsSeq
	interval := d.hapticsInterval
	d.hapticsSeq++
	d.hapticsInterval++

	report, err := BuildBluetoothCombinedHapticsReport(
		sequence, interval, sample[:], d.outputState.RawOutputReport[:])
	if err != nil {
		slog.Warn("failed to build DualSense V5 Bluetooth haptics report", "error", err)
		return OutputState{}, 0, false
	}
	feedback := d.outputState
	copy(feedback.BluetoothCombinedOutputReport[:], report)
	return feedback, assemblyDelay, true
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
	if response, handled := d.handleAudioControlRequest(
		bmRequestType, bRequest, wValue, wIndex, wLength, data,
	); handled {
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
				return b, true
			}
			if reportType == reportTypeFeature {
				if fn, ok := featureGetHandlers[reportID]; ok {
					b := fn(d)
					if wLength > 0 && int(wLength) < len(b) {
						b = b[:wLength]
					}
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

func (d *DualSense) handleOutputReport(out []byte) bool {
	report, ok := normalizeOutputReport(out)
	if !ok {
		return false
	}
	d.mtx.Lock()
	outputFunc := d.outputFunc
	if outputFunc != nil {
		feedback := d.mergeOutputReport(report)
		d.mtx.Unlock()
		outputFunc(feedback)
	} else {
		d.mtx.Unlock()
	}
	return true
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
		resetUSBInputReportToNeutral(b, d.seqCounter, ts, battery)
	}

	return b
}

func inputStateControlsInvalid(s *InputState) bool {
	if s == nil {
		return false
	}
	return s.Buttons&^validDualSenseInputButtons != 0 ||
		s.DPad&^validDualSenseInputDPad != 0
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

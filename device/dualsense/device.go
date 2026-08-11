package dualsense

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/device/internal/inputstatequeue"
	"github.com/Alia5/VIIPER/device/internal/microphonebuffer"
	"github.com/Alia5/VIIPER/usb"
)

const (
	microphoneTargetClientFrames  = 6  // 60 ms absorbs independent radio/client and virtual USB scheduling jitter.
	microphoneMaximumClientFrames = 20 // 200 ms emergency ceiling for full-duplex BT bursts; steady state remains about 55 ms.
)

// A DualSense output report is a set of field updates, not a complete state
// replacement. Games commonly send trigger, LED, rumble, and audio changes in
// separate reports. The V5 media carrier repeats a complete state snapshot, so
// copying the last partial report into every 0x36 frame makes those independent
// updates erase one another. Keep one validity-aware snapshot instead.
const (
	outputFlag0Offset = 1
	outputFlag1Offset = 2
	outputFlag2Offset = 39

	outputFlag0RumbleMask         = 0x03
	outputFlag0RightTrigger       = 0x04
	outputFlag0LeftTrigger        = 0x08
	outputFlag0HeadphoneVolume    = 0x10
	outputFlag0SpeakerVolume      = 0x20
	outputFlag0MicrophoneVolume   = 0x40
	outputFlag0AudioControl       = 0x80
	outputFlag1MicrophoneLed      = 0x01
	outputFlag1PowerSave          = 0x02
	outputFlag1Lightbar           = 0x04
	outputFlag1ReleaseLeds        = 0x08
	outputFlag1PlayerLeds         = 0x10
	outputFlag1HapticsLowPass     = 0x20
	outputFlag1MotorPower         = 0x40
	outputFlag1AudioControl2      = 0x80
	outputFlag2LightbarBrightness = 0x01
	outputFlag2LightbarSetup      = 0x02

	outputRightTriggerOffset     = 11
	outputLeftTriggerOffset      = 22
	outputTriggerLength          = 11
	outputPlayerLedsOffset       = 44
	outputLightbarOffset         = 45
	inputTransitionQueueCapacity = 256
)

type DualSense struct {
	deviceType string
	inputQueue *inputstatequeue.Queue[InputState]
	inputState InputState
	metaState  *MetaState

	// Output and media publication use independent gates. HID state remains
	// valid across an audio-pipe reset, while speaker/haptics data does not.
	// Keeping the gates separate prevents an audio reconfiguration from
	// discarding the game's final lightbar/trigger/rumble update.
	outputPublishMu sync.RWMutex
	mediaPublishMu  sync.RWMutex

	atomicAudioHapticsFunc func(OutputState, []byte)
	realtimeHapticsFunc    func(OutputState)
	speakerResetFunc       func()
	outputFunc             func(OutputState)
	outputState            OutputState
	latestOutputState      OutputState
	outputSeen             bool
	mediaRevision          uint64
	descriptor             usb.Descriptor

	subcommand [2]byte

	seqCounter                uint8
	hapticsSeq                uint8
	hapticsInterval           uint8
	realtimeHapticsSeq        uint8
	realtimeHapticsInterval   uint8
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

	inputReportMu sync.Mutex
	mtx           sync.Mutex
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
	d.inputQueue = inputstatequeue.New(
		d.inputState, dualSenseInputEdgeSignature(d.inputState),
		inputTransitionQueueCapacity)
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
	d.outputPublishMu.Lock()
	defer d.outputPublishMu.Unlock()

	var latest OutputState
	var replay bool
	d.mtx.Lock()
	d.outputFunc = f
	if f != nil && d.outputSeen {
		latest = d.latestOutputState
		replay = true
	}
	d.mtx.Unlock()

	// A newly attached stream must observe the last explicit game update even
	// if it arrived just before callback registration. The publication gate
	// orders this replay against live SET_REPORT callbacks.
	if replay {
		f(latest)
	}
}

// SetAtomicAudioHapticsCallback installs the V5 transport consumer. Each
// callback contains native feedback and one front-channel PCM generation.
// The V5 contract emits exactly 480
// raw 48 kHz speaker frames and consumes one independently completed rear
// haptics sample, or silence when that 512-frame lane has not completed yet.
func (d *DualSense) SetAtomicAudioHapticsCallback(f func(OutputState, []byte)) {
	d.replaceMediaCallbacks(func() {
		d.atomicAudioHapticsFunc = f
	})
}

// SetRealtimeHapticsCallback installs the V5 rear-channel consumer. A
// callback is issued as soon as one complete 512-frame haptics interval is
// available, independently of the 480-frame speaker clock.
func (d *DualSense) SetRealtimeHapticsCallback(f func(OutputState)) {
	d.replaceMediaCallbacks(func() {
		d.realtimeHapticsFunc = f
	})
}

// SetSpeakerResetCallback installs the transport-side queue reset paired with
// SetAtomicAudioHapticsCallback. USB interface close/reopen and endpoint reset
// must discard queued speaker PCM from the previous presentation generation.
func (d *DualSense) SetSpeakerResetCallback(f func()) {
	d.replaceMediaCallbacks(func() {
		d.speakerResetFunc = f
	})
}

// setV5MediaCallbacks replaces the three coupled V5 media callbacks as one
// transport generation. The stream handler uses this instead of exposing a
// partially installed callback set between three independent setter calls.
func (d *DualSense) setV5MediaCallbacks(
	atomic func(OutputState, []byte),
	realtime func(OutputState),
	reset func(),
) {
	d.replaceMediaCallbacks(func() {
		d.atomicAudioHapticsFunc = atomic
		d.realtimeHapticsFunc = realtime
		d.speakerResetFunc = reset
	})
}

// replaceMediaCallbacks is a hard lifecycle boundary. A callback already in
// progress finishes before the old transport is flushed; a callback assembled
// before this revision can never publish into the replacement transport.
func (d *DualSense) replaceMediaCallbacks(update func()) {
	d.mediaPublishMu.Lock()
	defer d.mediaPublishMu.Unlock()

	d.mtx.Lock()
	resetSpeaker := d.speakerResetFunc
	d.mediaRevision++
	d.resetSpeakerAudioLocked()
	update()
	d.mtx.Unlock()

	if resetSpeaker != nil {
		resetSpeaker()
	}
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

func (d *DualSense) UpdateInputState(state *InputState) error {
	return d.UpdateInputStateUntil(nil, state)
}

func (d *DualSense) UpdateInputStateUntil(done <-chan struct{}, state *InputState) error {
	next := *NewInputState()
	if state != nil {
		next = *state
	}
	if err := d.inputQueue.PublishUntil(
		done, next, dualSenseInputEdgeSignature(next)); err != nil {
		return err
	}
	d.mtx.Lock()
	d.inputState = next
	d.mtx.Unlock()
	return nil
}

func dualSenseInputEdgeSignature(state InputState) uint64 {
	return uint64(state.Buttons) |
		uint64(state.DPad)<<32 |
		uint64(encodeTouchStatus(state.Touch1Active, state.Touch1Tracking))<<40 |
		uint64(encodeTouchStatus(state.Touch2Active, state.Touch2Tracking))<<48
}

func (d *DualSense) InvalidateInterruptInput(endpoint uint8) {
	if endpoint == 0 || endpoint&0x0f == EndpointIn&0x0f {
		d.inputQueue.Invalidate()
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
	if iface == InterfaceHapticsAudio {
		d.resetSpeakerPresentation(func() {
			d.speakerInterfaceActive = alt != 0
		})
		return
	}

	d.mtx.Lock()
	switch iface {
	case InterfaceMicrophone:
		d.microphoneInterfaceActive = alt != 0
		d.resetMicrophoneAudioLocked()
	}
	d.mtx.Unlock()
}

// ResetEndpoint implements usb.EndpointResetDevice. A standard endpoint pipe
// reset preserves the selected alternate setting and feature controls while
// discarding all transport data from the previous endpoint generation.
func (d *DualSense) ResetEndpoint(endpoint uint8) {
	if endpoint == EndpointHapticsAudioOut {
		d.resetSpeakerPresentation(nil)
		return
	}

	d.mtx.Lock()
	switch endpoint {
	case EndpointMicrophoneIn:
		d.resetMicrophoneAudioLocked()
	}
	d.mtx.Unlock()
}

// resetSpeakerPresentation advances the device-owned revision and the framed
// writer generation under one publication barrier. The optional state update
// applies before the revision is visible to new media callbacks.
func (d *DualSense) resetSpeakerPresentation(update func()) {
	d.mediaPublishMu.Lock()
	defer d.mediaPublishMu.Unlock()

	d.mtx.Lock()
	d.mediaRevision++
	if update != nil {
		update()
	}
	d.resetSpeakerAudioLocked()
	resetSpeaker := d.speakerResetFunc
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
	// The transport-neutral device contract carries the endpoint number
	// separately from direction, so 0x82 arrives here as endpoint 2 plus IN.
	epNumber := ep & 0x0F
	if dir == usb.DirectionIn {
		switch epNumber {
		case EndpointIn & 0x0F:
			is, _, err := d.inputQueue.Wait(ctx, nil)
			if err != nil {
				return nil
			}
			d.mtx.Lock()
			ms := *d.metaState
			d.mtx.Unlock()
			return d.buildUSBInputReport(&is, &ms)
		case EndpointMicrophoneIn & 0x0F:
			return d.handleMicrophoneIn(ctx)
		default:
			return nil
		}
	}

	if dir == usb.DirectionOut && epNumber == EndpointOut&0x0F {
		if d.handleOutputReport(out) {
			return nil
		}
	}
	if dir == usb.DirectionOut && epNumber == EndpointHapticsAudioOut&0x0F {
		d.handleHapticsAudioOut(out)
		return nil
	}

	return nil
}

// ReadInterruptInput implements usb.InterruptInputDevice for the native UDE
// fast path. The caller owns dst and reuses it only after SubmitInputReport has
// completed, so encoding here removes the per-sample report allocation without
// changing USB/IP behavior.
func (d *DualSense) ReadInterruptInput(ctx context.Context, ep uint32, dst []byte) (int, error) {
	written, _, err := d.readInterruptInput(ctx, nil, ep, dst)
	return written, err
}

// ReadScheduledInterruptInput preserves the DualSense report encoder and its
// packet-counter/sensor-timestamp cadence while letting native UDE reuse one
// endpoint timer instead of allocating a context timer for every idle sample.
func (d *DualSense) ReadScheduledInterruptInput(
	ctx context.Context, deadline <-chan time.Time, ep uint32, dst []byte,
) (int, error) {
	written, _, err := d.readInterruptInput(ctx, deadline, ep, dst)
	return written, err
}

func (d *DualSense) ReadClassifiedScheduledInterruptInput(
	ctx context.Context, deadline <-chan time.Time, ep uint32, dst []byte,
) (int, bool, error) {
	return d.readInterruptInput(ctx, deadline, ep, dst)
}

func (d *DualSense) readInterruptInput(
	ctx context.Context, deadline <-chan time.Time, ep uint32, dst []byte,
) (int, bool, error) {
	if ep&0x0f != EndpointIn&0x0f {
		return 0, false, fmt.Errorf("DualSense interrupt-IN endpoint %d is unsupported", ep)
	}
	if deadline != nil && ctx.Err() != nil {
		return 0, false, ctx.Err()
	}
	is, transition, err := d.inputQueue.Wait(ctx, deadline)
	if err != nil {
		return 0, false, err
	}
	d.mtx.Lock()
	ms := *d.metaState
	d.mtx.Unlock()
	written, err := d.buildUSBInputReportInto(&is, &ms, dst)
	return written, transition, err
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

// ReadIsochronousInput implements usb.IsochronousInputDevice. Native UDE owns
// the packet service deadline and destination, so this path neither allocates a
// packet nor creates a timer per USB packet.
func (d *DualSense) ReadIsochronousInput(ctx context.Context, ep uint32, dst []byte) (int, error) {
	if ep&0x0f != EndpointMicrophoneIn&0x0f {
		return 0, fmt.Errorf("DualSense isochronous-IN endpoint %d is unsupported", ep)
	}
	if len(dst) < USBMicrophonePacketSize {
		return 0, io.ErrShortBuffer
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	packet := dst[:min(len(dst), USBMicrophoneMaxPacketSize)]
	clear(packet)
	d.mtx.Lock()
	defer d.mtx.Unlock()
	if d.microphoneInterfaceActive {
		if actualLength, ok := d.microphoneBuffer.ReadPacket(packet); ok {
			d.microphoneAudioFeature.applyPCMInPlace(
				packet[:actualLength], USBMicrophoneChannels,
			)
			return actualLength, nil
		}
	}
	d.microphoneBuffer.RecordZeroPacket()
	return USBMicrophonePacketSize, nil
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
	revision := d.mediaRevision
	reports := d.consumeDualSenseV5AudioLocked(processed, receivedAt)
	for index := range reports {
		reports[index].revision = revision
	}
	d.mtx.Unlock()
	if release != nil {
		release()
	}

	for _, pending := range reports {
		d.publishV5Media(pending)
	}
}

func (d *DualSense) publishV5Media(pending pendingBluetoothHapticsReport) bool {
	d.mediaPublishMu.RLock()
	defer d.mediaPublishMu.RUnlock()

	d.mtx.Lock()
	if pending.revision != d.mediaRevision || !d.speakerInterfaceActive {
		d.mtx.Unlock()
		return false
	}
	outputFunc := d.outputFunc
	atomicAudioHapticsFunc := d.atomicAudioHapticsFunc
	realtimeHapticsFunc := d.realtimeHapticsFunc
	d.mtx.Unlock()

	if pending.hapticsOnly {
		if realtimeHapticsFunc == nil {
			return false
		}
		realtimeHapticsFunc(pending.feedback)
		return true
	}
	if atomicAudioHapticsFunc != nil {
		atomicAudioHapticsFunc(pending.feedback, pending.speakerPCM)
		return true
	}
	if outputFunc != nil {
		outputFunc(pending.feedback)
		return true
	}
	return false
}

type pendingBluetoothHapticsReport struct {
	speakerPCM    []byte
	assemblyDelay time.Duration
	feedback      OutputState
	hapticsOnly   bool
	revision      uint64
}

type dualSenseV5HapticsGeneration struct {
	sample        [BluetoothHapticsSampleSize]byte
	assemblyDelay time.Duration
}

// consumeDualSenseV5AudioLocked advances the native USB stream in source
// order while keeping its two media clocks independent. Front stereo is
// published every 480 frames. Rear haptics completes every 512 frames and is
// queued independently. At each speaker boundary, exactly one completed rear
// sample is consumed; if none is ready, V5 sends silence rather than
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
			generation := d.v5HapticsQueue[len(d.v5HapticsQueue)-1]
			if feedback, ok := d.buildDualSenseV5RealtimeHapticsLocked(
				generation.sample[:]); ok {
				reports = append(reports, pendingBluetoothHapticsReport{
					assemblyDelay: generation.assemblyDelay,
					feedback:      feedback,
					hapticsOnly:   true,
				})
			}
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

func (d *DualSense) buildDualSenseV5RealtimeHapticsLocked(
	sample []byte) (OutputState, bool) {
	sequence := d.realtimeHapticsSeq
	interval := d.realtimeHapticsInterval
	d.realtimeHapticsSeq++
	d.realtimeHapticsInterval++
	report, err := BuildBluetoothCombinedHapticsReport(
		sequence, interval, sample,
		d.outputState.RawOutputReport[:])
	if err != nil {
		slog.Warn("failed to build realtime DualSense V5 haptics report",
			"error", err)
		return OutputState{}, false
	}
	feedback := d.outputState
	copy(feedback.BluetoothCombinedOutputReport[:], report)
	return feedback, true
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
	d.outputPublishMu.RLock()
	defer d.outputPublishMu.RUnlock()

	d.mtx.Lock()
	feedback := d.mergeOutputReport(report)
	d.latestOutputState = feedback
	d.outputSeen = true
	outputFunc := d.outputFunc
	d.mtx.Unlock()
	if outputFunc != nil {
		outputFunc(feedback)
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
		mergeRawOutputReport(&feedback.RawOutputReport, out)
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
	// The persistent snapshot above feeds VIIPER's atomic audio/haptics
	// assembler, where independently flagged USB fields must remain coherent.
	// The ordinary output callback has a different contract: it represents the
	// exact SET_REPORT update issued by the game. Returning the accumulated
	// snapshot there replayed one-shot trigger and LED-release validity bits on
	// later rumble/audio writes. Keep the two contracts separate.
	copy(feedback.RawOutputReport[:], out[:OutputReportSize])
	return feedback
}

func mergeRawOutputReport(snapshot *[OutputReportSize]byte, update []byte) {
	if snapshot == nil || len(update) < OutputReportSize ||
		update[0] != ReportIDOutput {
		return
	}

	if snapshot[0] != ReportIDOutput {
		clear(snapshot[:])
		snapshot[0] = ReportIDOutput
	}

	flag0 := update[outputFlag0Offset]
	flag1 := update[outputFlag1Offset]
	flag2 := update[outputFlag2Offset]

	// Rumble selector bits form one contract. Replace that selector only when
	// the host actually mentions it; a trigger-only report must not clear it.
	if flag0&outputFlag0RumbleMask != 0 {
		snapshot[outputFlag0Offset] =
			(snapshot[outputFlag0Offset] &^ outputFlag0RumbleMask) |
				(flag0 & outputFlag0RumbleMask)
	}
	if flag0&0x01 != 0 || flag2&0x04 != 0 {
		snapshot[3] = update[3]
		snapshot[4] = update[4]
	}

	mergeOutputField(snapshot, update, outputFlag0Offset, flag0, outputFlag0HeadphoneVolume, 5, 1)
	mergeOutputField(snapshot, update, outputFlag0Offset, flag0, outputFlag0SpeakerVolume, 6, 1)
	mergeOutputField(snapshot, update, outputFlag0Offset, flag0, outputFlag0MicrophoneVolume, 7, 1)
	mergeOutputField(snapshot, update, outputFlag0Offset, flag0, outputFlag0AudioControl, 8, 1)
	mergeOutputField(snapshot, update, outputFlag0Offset, flag0, outputFlag0RightTrigger,
		outputRightTriggerOffset, outputTriggerLength)
	mergeOutputField(snapshot, update, outputFlag0Offset, flag0, outputFlag0LeftTrigger,
		outputLeftTriggerOffset, outputTriggerLength)

	mergeOutputField(snapshot, update, outputFlag1Offset, flag1, outputFlag1MicrophoneLed, 9, 1)
	mergeOutputField(snapshot, update, outputFlag1Offset, flag1, outputFlag1PowerSave, 10, 1)
	mergeOutputField(snapshot, update, outputFlag1Offset, flag1, outputFlag1HapticsLowPass, 40, 1)
	mergeOutputField(snapshot, update, outputFlag1Offset, flag1, outputFlag1MotorPower, 37, 1)
	mergeOutputField(snapshot, update, outputFlag1Offset, flag1, outputFlag1AudioControl2, 38, 1)

	if flag1&outputFlag1ReleaseLeds != 0 {
		snapshot[outputFlag1Offset] =
			(snapshot[outputFlag1Offset] &^
				(outputFlag1Lightbar | outputFlag1PlayerLeds)) |
				outputFlag1ReleaseLeds
		snapshot[outputPlayerLedsOffset] = 0
		clear(snapshot[outputLightbarOffset : outputLightbarOffset+3])
	} else {
		if flag1&(outputFlag1Lightbar|outputFlag1PlayerLeds) != 0 {
			snapshot[outputFlag1Offset] &^= outputFlag1ReleaseLeds
		}
		mergeOutputField(snapshot, update, outputFlag1Offset, flag1, outputFlag1PlayerLeds,
			outputPlayerLedsOffset, 1)
		mergeOutputField(snapshot, update, outputFlag1Offset, flag1, outputFlag1Lightbar,
			outputLightbarOffset, 3)
	}

	mergeOutputField(snapshot, update, outputFlag2Offset, flag2,
		outputFlag2LightbarBrightness, 43, 1)
	mergeOutputField(snapshot, update, outputFlag2Offset, flag2,
		outputFlag2LightbarSetup, 42, 1)
	// Preserve the two rumble-mode controls in flag2. They have no separate
	// payload field but still belong to the persistent controller contract.
	if flag2&0x0C != 0 {
		snapshot[outputFlag2Offset] =
			(snapshot[outputFlag2Offset] &^ 0x0C) | (flag2 & 0x0C)
	}
}

func mergeOutputField(snapshot *[OutputReportSize]byte, update []byte,
	flagOffset int, flags byte, mask byte, offset int, length int) {
	if flags&mask == 0 || offset < 0 || length <= 0 ||
		offset+length > len(update) || offset+length > len(snapshot) ||
		flagOffset < 0 || flagOffset >= len(snapshot) {
		return
	}

	snapshot[flagOffset] |= mask
	copy(snapshot[offset:offset+length], update[offset:offset+length])
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
	_, _ = d.buildUSBInputReportInto(s, m, b)
	return b
}

func (d *DualSense) buildUSBInputReportInto(s *InputState, m *MetaState, dst []byte) (int, error) {
	if len(dst) < InputReportSize {
		return 0, io.ErrShortBuffer
	}
	b := dst[:InputReportSize]
	clear(b)

	// HID GET_REPORT and the native interrupt publisher can encode
	// concurrently. Sequence and corruption telemetry are one ordered report
	// stream, so serialize only encoding rather than the controller state or
	// media paths.
	d.inputReportMu.Lock()
	defer d.inputReportMu.Unlock()
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

	return InputReportSize, nil
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

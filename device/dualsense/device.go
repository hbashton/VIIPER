package dualsense

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/device/internal/microphonebuffer"
	"github.com/Alia5/VIIPER/internal/inputpresentation"
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

const (
	microphoneTargetClientFrames     = 6  // 60 ms absorbs independent radio/client and virtual USB scheduling jitter.
	microphoneMaximumClientFrames    = 20 // 200 ms emergency ceiling for full-duplex BT bursts; steady state remains about 55 ms.
	microphoneInterfaceEventCapacity = 64
)

type microphoneInterfaceEvent struct {
	callback   func(bool, uint64)
	active     bool
	generation uint64
}

var _ inputpresentation.Source = (*DualSense)(nil)
var _ inputpresentation.AdmissionSource = (*DualSense)(nil)

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

	outputRightTriggerOffset = 11
	outputLeftTriggerOffset  = 22
	outputTriggerLength      = 11
	outputPlayerLedsOffset   = 44
	outputLightbarOffset     = 45
)

type DualSense struct {
	deviceType              string
	input                   *dualSenseInputScheduler
	inputBattery            atomic.Uint32
	inputTelemetryEnabled   atomic.Bool
	inputTransportTelemetry dualSenseInputTransportTelemetry
	metaState               *MetaState

	atomicAudioHapticsFunc       func(OutputState, []byte)
	realtimeHapticsFunc          func(OutputState)
	speakerResetFunc             func()
	transportOutputFunc          func(OutputState) bool
	transportAtomicAudioFunc     func(OutputState, []byte, uint64)
	transportRealtimeHapticsFunc func(OutputState, uint64)
	transportSpeakerResetFunc    func(uint64)
	outputCallbackGeneration     uint64
	microphoneInterfaceStateFunc func(bool, uint64)
	microphoneCallbackGeneration uint64
	outputFunc                   func(OutputState)
	outputState                  OutputState
	mediaOutputState             OutputState
	descriptor                   usb.Descriptor

	subcommand [2]byte

	hapticsSeq                          uint8
	hapticsInterval                     uint8
	realtimeHapticsSeq                  uint8
	realtimeHapticsInterval             uint8
	hapticsPCM                          [BluetoothHapticsSampleSize / 2 * USBHapticsAudioDownsample * USBHapticsAudioFrameSize]byte
	hapticsPCMLength                    int
	v5SpeakerPCM                        [dualSenseV5SpeakerPayloadSize]byte
	v5SpeakerPCMLength                  int
	v5HapticsQueue                      [8]dualSenseV5HapticsGeneration
	v5HapticsQueueHead                  int
	v5HapticsQueueCount                 int
	mediaReportSlots                    [4]pendingBluetoothHapticsReport
	mediaReportFree                     chan *pendingBluetoothHapticsReport
	mediaReportDrops                    atomic.Uint64
	mediaReportBuildFailures            atomic.Uint64
	microphoneBuffer                    microphonebuffer.Buffer
	microphoneSignal                    chan struct{}
	speakerAudioFeature                 audioFeatureState
	microphoneAudioFeature              audioFeatureState
	speakerStreamTelemetry              *dualSenseSpeakerStreamTelemetry
	speakerInterfaceActive              bool
	speakerMediaGeneration              uint64
	microphoneInterfaceActive           bool
	microphoneInterfaceState            atomic.Bool
	microphoneInterfaceEvents           [microphoneInterfaceEventCapacity]microphoneInterfaceEvent
	microphoneInterfaceEventHead        int
	microphoneInterfaceEventCount       int
	microphoneInterfaceEventDispatching bool
	microphoneInterfaceRecoveryEvent    microphoneInterfaceEvent
	microphoneInterfaceRecoveryPending  bool
	microphoneInterfaceEventOverflows   atomic.Uint64
	// hapticsPCMStartedAt identifies the oldest PCM frame waiting to make a
	// complete 10.667 ms Bluetooth haptics sample. It feeds ordinary stream
	// health telemetry without affecting presentation timing.
	hapticsPCMStartedAt time.Time

	metaMu       sync.Mutex
	outputMu     sync.Mutex
	mediaMu      sync.Mutex
	microphoneMu sync.Mutex
	callbackMu   sync.RWMutex
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
		microphoneSignal:       make(chan struct{}, 1),
		mediaReportFree:        make(chan *pendingBluetoothHapticsReport, 4),
		speakerMediaGeneration: 1,
	}
	for index := range d.mediaReportSlots {
		d.mediaReportFree <- &d.mediaReportSlots[index]
	}
	d.inputBattery.Store(uint32(metaState.BatteryStatus))
	d.input = newDualSenseInputScheduler(metaState.BatteryStatus, edge)
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

	return d, nil
}

// VIIPERDeviceType preserves the exact registered transport contract for
// stream dispatch. Multiple V5 endpoint variants share the DualSense concrete
// type, so reflection on the package name cannot distinguish them.
func (d *DualSense) VIIPERDeviceType() string {
	return d.deviceType
}

func (d *DualSense) SetMetaState(meta MetaState) {
	d.metaMu.Lock()
	d.metaState = &meta
	d.metaMu.Unlock()
	d.inputBattery.Store(uint32(meta.BatteryStatus))
}

func (d *DualSense) SetOutputCallback(f func(OutputState)) {
	d.callbackMu.Lock()
	d.outputFunc = f
	d.callbackMu.Unlock()
}

// SetAtomicAudioHapticsCallback installs the V5 transport consumer. Each
// callback contains native feedback and one front-channel PCM generation.
// The V5 contract emits exactly 480
// raw 48 kHz speaker frames and consumes one independently completed rear
// haptics sample, or silence when that 512-frame lane has not completed yet.
func (d *DualSense) SetAtomicAudioHapticsCallback(f func(OutputState, []byte)) {
	d.callbackMu.Lock()
	d.atomicAudioHapticsFunc = f
	d.callbackMu.Unlock()
}

// SetRealtimeHapticsCallback installs the V5 rear-channel consumer. A
// callback is issued as soon as one complete 512-frame haptics interval is
// available, independently of the 480-frame speaker clock.
func (d *DualSense) SetRealtimeHapticsCallback(f func(OutputState)) {
	d.callbackMu.Lock()
	d.realtimeHapticsFunc = f
	d.callbackMu.Unlock()
}

// SetSpeakerResetCallback installs the transport-side queue reset paired with
// SetAtomicAudioHapticsCallback. USB interface close/reopen and endpoint reset
// must discard queued speaker PCM from the previous presentation generation.
func (d *DualSense) SetSpeakerResetCallback(f func()) {
	d.callbackMu.Lock()
	d.speakerResetFunc = f
	d.callbackMu.Unlock()
}

// setV5OutputCallbacks replaces the stream-owned output sinks as one logical
// registration. The generation guard means a displaced stream can finish its
// deferred cleanup without clearing the replacement stream's callbacks.
func (d *DualSense) setV5OutputCallbacks(streamGeneration uint64,
	output func(OutputState) bool,
	atomicAudio func(OutputState, []byte, uint64),
	realtimeHaptics func(OutputState, uint64),
	resetSpeaker func(uint64)) {
	d.callbackMu.Lock()
	if output == nil && atomicAudio == nil && realtimeHaptics == nil &&
		resetSpeaker == nil {
		if d.outputCallbackGeneration == streamGeneration {
			d.transportOutputFunc = nil
			d.transportAtomicAudioFunc = nil
			d.transportRealtimeHapticsFunc = nil
			d.transportSpeakerResetFunc = nil
			d.outputCallbackGeneration = 0
		}
		d.callbackMu.Unlock()
		return
	}
	d.transportOutputFunc = output
	d.transportAtomicAudioFunc = atomicAudio
	d.transportRealtimeHapticsFunc = realtimeHaptics
	d.transportSpeakerResetFunc = resetSpeaker
	d.outputCallbackGeneration = streamGeneration
	d.callbackMu.Unlock()
}

func (d *DualSense) currentSpeakerMediaGeneration() uint64 {
	d.mediaMu.Lock()
	generation := d.speakerMediaGeneration
	d.mediaMu.Unlock()
	return generation
}

// beginSpeakerStream gives each stream generation independent telemetry. An
// older writer can therefore finish a callback without changing the state
// exposed for a replacement connection.
func (d *DualSense) beginSpeakerStream() *dualSenseSpeakerStreamTelemetry {
	telemetry := &dualSenseSpeakerStreamTelemetry{}
	d.mediaMu.Lock()
	d.speakerStreamTelemetry = telemetry
	d.mediaMu.Unlock()
	return telemetry
}

func (d *DualSense) UpdateInputState(state *InputState) {
	d.input.update(state, 0)
}

// setMicrophoneInterfaceStateCallback installs the lifecycle-event sink for
// one V5 stream generation. Clearing an older stream cannot detach the
// replacement stream's callback. The microphone mutex owns callback
// registration and event admission together, so the attach snapshot has one
// deterministic position relative to alternate-setting transitions. Callback
// execution is drained after releasing the mutex.
func (d *DualSense) setMicrophoneInterfaceStateCallback(generation uint64,
	f func(bool, uint64)) {
	startDispatch := false
	d.microphoneMu.Lock()
	if f == nil {
		if d.microphoneCallbackGeneration == generation {
			d.microphoneInterfaceStateFunc = nil
			d.microphoneCallbackGeneration = 0
		}
		d.microphoneMu.Unlock()
		return
	}
	d.microphoneCallbackGeneration = generation
	d.microphoneInterfaceStateFunc = f
	startDispatch = d.enqueueMicrophoneInterfaceEventLocked(
		microphoneInterfaceEvent{
			callback: f, active: d.microphoneInterfaceActive,
			generation: generation,
		},
	)
	d.microphoneMu.Unlock()
	if startDispatch {
		d.dispatchMicrophoneInterfaceEvents()
	}
}

// enqueueMicrophoneInterfaceEventLocked preserves every ordinary lifecycle
// transition in a fixed ring. If an arbitrary callback stalls long enough to
// exhaust the ring, later events replace one recovery snapshot. That bounded
// fallback may coalesce pathological churn, but the final authoritative state
// can never remain stale. microphoneMu must be held.
func (d *DualSense) enqueueMicrophoneInterfaceEventLocked(
	event microphoneInterfaceEvent,
) bool {
	if event.callback == nil {
		return false
	}
	if d.microphoneInterfaceRecoveryPending {
		d.microphoneInterfaceRecoveryEvent = event
		d.microphoneInterfaceEventOverflows.Add(1)
	} else if d.microphoneInterfaceEventCount < len(d.microphoneInterfaceEvents) {
		index := (d.microphoneInterfaceEventHead +
			d.microphoneInterfaceEventCount) % len(d.microphoneInterfaceEvents)
		d.microphoneInterfaceEvents[index] = event
		d.microphoneInterfaceEventCount++
	} else {
		d.microphoneInterfaceRecoveryEvent = event
		d.microphoneInterfaceRecoveryPending = true
		d.microphoneInterfaceEventOverflows.Add(1)
	}
	if d.microphoneInterfaceEventDispatching {
		return false
	}
	d.microphoneInterfaceEventDispatching = true
	return true
}

func (d *DualSense) dispatchMicrophoneInterfaceEvents() {
	for {
		var event microphoneInterfaceEvent
		d.microphoneMu.Lock()
		if d.microphoneInterfaceEventCount > 0 {
			event = d.microphoneInterfaceEvents[d.microphoneInterfaceEventHead]
			d.microphoneInterfaceEvents[d.microphoneInterfaceEventHead] = microphoneInterfaceEvent{}
			d.microphoneInterfaceEventHead =
				(d.microphoneInterfaceEventHead + 1) %
					len(d.microphoneInterfaceEvents)
			d.microphoneInterfaceEventCount--
		} else if d.microphoneInterfaceRecoveryPending {
			event = d.microphoneInterfaceRecoveryEvent
			d.microphoneInterfaceRecoveryEvent = microphoneInterfaceEvent{}
			d.microphoneInterfaceRecoveryPending = false
		} else {
			d.microphoneInterfaceEventDispatching = false
			d.microphoneMu.Unlock()
			return
		}
		d.microphoneMu.Unlock()

		// The callback is a transport publication boundary. It must never run
		// while the microphone buffer/state lock is held.
		event.callback(event.active, event.generation)
	}
}

func (d *DualSense) updateInputStateForGeneration(generation uint64,
	state *InputState) bool {
	return d.input.update(state, generation)
}

func (d *DualSense) updateInputStateForGenerationAt(generation uint64,
	state *InputState, receivedAt time.Time) bool {
	return d.input.updateAt(state, generation, receivedAt)
}

func (d *DualSense) beginInputStreamGeneration() uint64 {
	return d.input.beginReceiveGeneration()
}

// InputSchedulerState returns an aggregate snapshot without exposing queue
// ownership or holding the input lock during JSON/map construction.
func (d *DualSense) InputSchedulerState() InputSchedulerSnapshot {
	return d.input.snapshot()
}

func (d *DualSense) GetDescriptor() *usb.Descriptor {
	return &d.descriptor
}

func (d *DualSense) GetDeviceSpecificArgs() map[string]any {
	var res map[string]any
	d.metaMu.Lock()
	meta := *d.metaState
	d.metaMu.Unlock()
	d.mediaMu.Lock()
	speakerInterfaceActive := d.speakerInterfaceActive
	speakerTelemetry := d.speakerStreamTelemetry
	d.mediaMu.Unlock()
	d.microphoneMu.Lock()
	microphoneInterfaceActive := d.microphoneInterfaceActive
	microphoneState := d.microphoneBuffer.State()
	d.microphoneMu.Unlock()

	bytes, err := json.Marshal(meta)
	if err != nil {
		return map[string]any{}
	}
	err = json.Unmarshal(bytes, &res)
	if err != nil {
		return map[string]any{}
	}
	res["speakerInterfaceActive"] = speakerInterfaceActive
	speakerState := speakerTelemetry.snapshot()
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
	res["microphoneInterfaceTransportOverflows"] =
		speakerState.MicrophoneInterfaceOverflows
	res["microphoneInterfaceActive"] = microphoneInterfaceActive
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
	res["microphoneInterfaceEventOverflows"] =
		d.microphoneInterfaceEventOverflows.Load()
	inputState := d.input.snapshot()
	res["inputGeneration"] = inputState.Generation
	res["inputStatesReceived"] = inputState.Received
	res["inputStatesSelected"] = inputState.Selected
	res["inputTransitionDepth"] = inputState.TransitionDepth
	res["inputTransitionHighWater"] = inputState.TransitionHighWater
	res["inputContinuousPending"] = inputState.ContinuousPending
	res["inputContinuousReplacements"] = inputState.ContinuousReplaced
	res["inputPeakUpgrades"] = inputState.PeakUpgrades
	res["inputTransitionOverflows"] = inputState.Overflows
	res["inputMaximumQueueAgeUS"] = inputState.MaximumQueueAge.Microseconds()
	res["inputQueueAgeBuckets"] = inputState.QueueAgeBuckets
	res["inputMaximumSelectionAgeUS"] =
		inputState.MaximumSelectionAge.Microseconds()
	res["inputSelectionAgeBuckets"] = inputState.SelectionAgeBuckets
	res["inputLatencyDistributions"] = d.InputTelemetryState()
	res["mediaReportDrops"] = d.mediaReportDrops.Load()
	res["mediaReportBuildFailures"] = d.mediaReportBuildFailures.Load()
	return res
}

// GetMicrophoneInterfaceStatus is the narrow compatibility API snapshot used
// only when a client could not attach through a V5 events alias. It never
// touches the input scheduler or serializes broad device diagnostics.
func (d *DualSense) GetMicrophoneInterfaceStatus() map[string]any {
	d.microphoneMu.Lock()
	active := d.microphoneInterfaceActive
	state := d.microphoneBuffer.State()
	d.microphoneMu.Unlock()

	return map[string]any{
		"active":                  active,
		"queuedBytes":             state.QueuedBytes,
		"targetBytes":             state.TargetBytes,
		"maximumBytes":            state.MaximumBytes,
		"primed":                  state.Primed,
		"underruns":               state.Underruns,
		"droppedBytes":            state.DroppedBytes,
		"overflowEvents":          state.OverflowEvents,
		"packetsRead":             state.PacketsRead,
		"zeroPackets":             state.ZeroPackets,
		"servoRatePPM":            state.ServoRatePPM,
		"interfaceEventOverflows": d.microphoneInterfaceEventOverflows.Load(),
	}
}

func (d *DualSense) SetInterfaceAltSetting(iface, alt uint8) {
	if iface == InterfaceMicrophone {
		microphoneActive := alt != 0
		d.microphoneMu.Lock()
		microphoneChanged := d.microphoneInterfaceActive != microphoneActive
		d.microphoneInterfaceActive = microphoneActive
		d.resetMicrophoneAudioLocked()
		d.microphoneInterfaceState.Store(microphoneActive)
		microphoneStateChanged := d.microphoneInterfaceStateFunc
		microphoneGeneration := d.microphoneCallbackGeneration
		startDispatch := false
		if microphoneChanged && microphoneStateChanged != nil {
			startDispatch = d.enqueueMicrophoneInterfaceEventLocked(
				microphoneInterfaceEvent{
					callback:   microphoneStateChanged,
					active:     microphoneActive,
					generation: microphoneGeneration,
				},
			)
		}
		d.microphoneMu.Unlock()
		if startDispatch {
			d.dispatchMicrophoneInterfaceEvents()
		}
		return
	}

	d.mediaMu.Lock()
	var speakerGeneration uint64
	switch iface {
	case InterfaceHapticsAudio:
		d.speakerInterfaceActive = alt != 0
		speakerGeneration = d.resetSpeakerAudioLocked()
	}
	d.mediaMu.Unlock()
	var resetSpeaker func()
	var resetTransportSpeaker func(uint64)
	if iface == InterfaceHapticsAudio {
		d.callbackMu.RLock()
		resetSpeaker = d.speakerResetFunc
		resetTransportSpeaker = d.transportSpeakerResetFunc
		d.callbackMu.RUnlock()
	}

	if resetTransportSpeaker != nil {
		resetTransportSpeaker(speakerGeneration)
	}
	if resetSpeaker != nil {
		resetSpeaker()
	}
}

// ResetEndpoint implements usb.EndpointResetDevice. A standard endpoint pipe
// reset preserves the selected alternate setting and feature controls while
// discarding all transport data from the previous endpoint generation.
func (d *DualSense) ResetEndpoint(endpoint uint8) {
	if endpoint == EndpointMicrophoneIn {
		d.microphoneMu.Lock()
		d.resetMicrophoneAudioLocked()
		d.microphoneMu.Unlock()
		return
	}
	if endpoint != EndpointHapticsAudioOut {
		return
	}
	d.mediaMu.Lock()
	speakerGeneration := d.resetSpeakerAudioLocked()
	d.mediaMu.Unlock()
	var resetSpeaker func()
	var resetTransportSpeaker func(uint64)
	d.callbackMu.RLock()
	resetSpeaker = d.speakerResetFunc
	resetTransportSpeaker = d.transportSpeakerResetFunc
	d.callbackMu.RUnlock()

	if resetTransportSpeaker != nil {
		resetTransportSpeaker(speakerGeneration)
	}
	if resetSpeaker != nil {
		resetSpeaker()
	}
}

func (d *DualSense) resetSpeakerAudioLocked() uint64 {
	d.speakerMediaGeneration++
	if d.speakerMediaGeneration == 0 {
		d.speakerMediaGeneration = 1
	}
	d.hapticsPCMLength = 0
	d.v5SpeakerPCMLength = 0
	for index := range d.v5HapticsQueue {
		d.v5HapticsQueue[index] = dualSenseV5HapticsGeneration{}
	}
	d.v5HapticsQueueHead = 0
	d.v5HapticsQueueCount = 0
	d.hapticsPCMStartedAt = time.Time{}
	d.speakerAudioFeature.resetStreamGain()
	return d.speakerMediaGeneration
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
			report := make([]byte, InputReportSize)
			if d.BuildInputReportInto(report) == 0 {
				return nil
			}
			return report
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

// BuildInputReportInto is the interrupt-IN hot path. The USB endpoint worker
// owns pacing; this method performs no wait and selects exactly one complete
// state immediately at the service opportunity.
func (d *DualSense) BuildInputReportInto(destination []byte) int {
	n, token := d.ClaimInputReport(destination)
	if token != 0 {
		d.CompleteInputReport(token, true)
	}
	return n
}

// ClaimInputReport selects and encodes one immutable state without committing
// it as presented. The endpoint worker owns the returned token until USB/IP
// send ownership is won or cancellation returns the claim for recovery.
func (d *DualSense) ClaimInputReport(destination []byte) (int, uint64) {
	claim := d.ClaimInputPresentation(destination, time.Now())
	return claim.Size, claim.Token
}

// CompleteInputReport commits encoder sequence, last report, and trigger peak
// presentation only after the USB/IP response owns serialization. A failed
// ordered claim is retried ahead of every later transition.
func (d *DualSense) CompleteInputReport(token uint64, presented bool) {
	// USB/IP invokes this immediately after the complete response write while
	// it still owns response serialization. Capture that boundary before input
	// lock acquisition so unrelated input publication contention is not charged
	// to socket presentation latency.
	completedAt := time.Now()
	d.input.mu.Lock()
	d.input.completeClaimAt(token, presented, completedAt)
	d.input.mu.Unlock()
}

// ClaimInputPresentation is the transport-neutral input hand-off. The caller
// owns destination; the returned claim carries only identity and timing
// metadata, so the hot path remains allocation-free.
func (d *DualSense) ClaimInputPresentation(destination []byte,
	selectedAt time.Time) inputpresentation.Claim {
	if selectedAt.IsZero() {
		selectedAt = time.Now()
	}
	battery := byte(d.inputBattery.Load())
	d.input.mu.Lock()
	n, token := d.input.beginClaim(selectedAt, battery, destination)
	claim := inputpresentation.Claim{}
	if token != 0 {
		claim = inputpresentation.Claim{
			Token: token, Generation: d.input.claimedPresentationGeneration,
			Size: n, ReceivedAt: d.input.claimed.receivedAt,
			SelectedAt: selectedAt, Ordered: d.input.claimed.ordered,
		}
	}
	d.input.mu.Unlock()
	return claim
}

// OwnsInputPresentationEndpoint confines controller state to the HID
// interrupt-IN endpoint. The composite device's microphone and other endpoint
// planes must never claim or advance controller input state.
func (d *DualSense) OwnsInputPresentationEndpoint(endpoint uint8) bool {
	return endpoint == EndpointIn&0x0f
}

func (d *DualSense) InputPresentationGeneration() uint64 {
	d.input.mu.Lock()
	generation := d.input.presentationGeneration
	d.input.mu.Unlock()
	return generation
}

// ResolveInputPresentation terminally commits, defers, or retires one claim.
// Token and generation are validated together, making duplicate completions
// and completions from a retired backend fail closed.
func (d *DualSense) ResolveInputPresentation(claim inputpresentation.Claim,
	outcome inputpresentation.Outcome, completedAt time.Time) bool {
	// A successful commit is the transport-neutral admission boundary. USB/IP
	// calls this only after the complete response write; a future UdeCx backend
	// must call it only after the driver has copied/admitted the report. Capture
	// QPC before scheduler-lock contention, but publish evidence only if the
	// token/generation/outcome resolution succeeds.
	admittedTicks := inputLatencyCounter()
	if completedAt.IsZero() {
		completedAt = time.Now()
	}
	d.input.mu.Lock()
	resolved := d.input.resolveClaimAt(
		claim.Token, claim.Generation, outcome, completedAt)
	if resolved && outcome == inputpresentation.OutcomeRetire {
		d.input.refreshPresentationSnapshot(
			byte(d.inputBattery.Load()), completedAt)
	}
	d.input.mu.Unlock()
	if resolved && outcome == inputpresentation.OutcomeCommit {
		traceInputTransportAdmitted(d, claim, admittedTicks)
	}
	return resolved
}

// CanAdmitInputPresentation closes the selection-to-write lifecycle race for
// the compatibility DualSense scheduler. It intentionally adds no age policy;
// it only proves that the exact token and presentation generation selected by
// this transport still own the immutable report immediately before USB/IP
// exposes its first byte.
func (d *DualSense) CanAdmitInputPresentation(
	claim inputpresentation.Claim, _ time.Time,
) bool {
	if !claim.Valid() || claim.Size != InputReportSize {
		return false
	}
	d.input.mu.Lock()
	admissible := d.input.hasClaim && claim.Token == d.input.claimToken &&
		claim.Generation == d.input.claimedPresentationGeneration &&
		claim.Generation == d.input.presentationGeneration
	d.input.mu.Unlock()
	return admissible
}

// RetireInputPresentationGeneration establishes a hard backend-lifecycle
// boundary. An active claim is dropped, not retried into the successor.
func (d *DualSense) RetireInputPresentationGeneration(generation uint64,
	retiredAt time.Time) bool {
	if retiredAt.IsZero() {
		retiredAt = time.Now()
	}
	d.input.mu.Lock()
	retired := d.input.retirePresentationGeneration(generation, retiredAt)
	if retired {
		d.input.refreshPresentationSnapshot(
			byte(d.inputBattery.Load()), retiredAt)
	}
	d.input.mu.Unlock()
	return retired
}

// SnapshotInputReportInto copies the current cached input report without
// selecting input or advancing committed encoder counters. Ordinarily this is
// the last successfully presented interrupt report; lifecycle retirement
// replaces it immediately with a current semantic preview so EP0 cannot expose
// stale controls before the successor's first interrupt poll. The accompanying
// version lets USB/IP serialize the copy against cache replacement.
func (d *DualSense) SnapshotInputReportInto(destination []byte) (int, uint64) {
	d.input.mu.Lock()
	n := min(len(destination), len(d.input.lastReport))
	copy(destination[:n], d.input.lastReport[:n])
	version := d.input.presentationVersion
	d.input.mu.Unlock()
	return n, version
}

// InputReportSnapshotCurrent validates a snapshot under the input-only lock.
// Callers hold response send ownership, so a true result remains current until
// that response is emitted.
func (d *DualSense) InputReportSnapshotCurrent(version uint64) bool {
	d.input.mu.Lock()
	current := version != 0 && version == d.input.presentationVersion
	d.input.mu.Unlock()
	return current
}

func (d *DualSense) QueueMicrophonePCMFrame(frame []byte) {
	if len(frame) != USBMicrophoneClientFrameSize {
		return
	}

	d.microphoneMu.Lock()
	if !d.microphoneInterfaceActive {
		d.microphoneMu.Unlock()
		return
	}

	d.microphoneBuffer.QueueFrame(frame)
	d.microphoneMu.Unlock()

	select {
	case d.microphoneSignal <- struct{}{}:
	default:
	}
}

// ResetMicrophonePCM clears capture transport state after the current API
// stream ends. The API generation coordinator suppresses this reset when that
// stream was displaced by a same-device replacement.
func (d *DualSense) ResetMicrophonePCM() {
	d.microphoneMu.Lock()
	d.resetMicrophoneAudioLocked()
	d.microphoneMu.Unlock()
}

// TryReadMicrophonePacket is the nonblocking ISO-IN source used by the
// persistent endpoint worker. n is the nominal packet length even on
// underrun; destination is zero-filled in that case so USB presents silence.
func (d *DualSense) TryReadMicrophonePacket(destination []byte) (n int, ok bool) {
	if len(destination) < USBMicrophoneMaxPacketSize {
		return 0, false
	}
	d.microphoneMu.Lock()
	if !d.microphoneInterfaceActive {
		d.microphoneBuffer.RecordZeroPacket()
		d.microphoneMu.Unlock()
		clear(destination[:USBMicrophonePacketSize])
		return USBMicrophonePacketSize, false
	}
	actualLength, available := d.microphoneBuffer.ReadPacket(destination)
	if available {
		d.microphoneAudioFeature.applyPCMInPlace(
			destination[:actualLength], USBMicrophoneChannels,
		)
		d.microphoneMu.Unlock()
		return actualLength, true
	}
	d.microphoneBuffer.RecordZeroPacket()
	d.microphoneMu.Unlock()
	clear(destination[:USBMicrophonePacketSize])
	return USBMicrophonePacketSize, false
}

func (d *DualSense) handleMicrophoneIn(_ context.Context) []byte {
	packet := make([]byte, USBMicrophoneMaxPacketSize)
	actualLength, _ := d.TryReadMicrophonePacket(packet)
	if actualLength == 0 {
		actualLength = USBMicrophonePacketSize
	}
	return packet[:actualLength]
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

// IsoOutGeneration captures the device-side media generation when the USB/IP
// reader admits an owned ISO-OUT job. The persistent endpoint worker passes
// the token back at service time so a reset cannot let old scratch data enter
// a replacement stream.
func (d *DualSense) IsoOutGeneration(endpoint uint8) uint64 {
	if endpoint != EndpointHapticsAudioOut {
		return 0
	}
	d.mediaMu.Lock()
	generation := d.speakerMediaGeneration
	d.mediaMu.Unlock()
	return generation
}

// HandleIsoOutTransfer accepts only work captured in the current media
// generation. Generation comparison and PCM consumption share the same media
// lock as reset, closing the scheduler-check-to-device-callback race.
func (d *DualSense) HandleIsoOutTransfer(endpoint uint8, generation uint64,
	out []byte) bool {
	if endpoint != EndpointHapticsAudioOut {
		return false
	}
	return d.handleHapticsAudioOutGeneration(out, generation)
}

func (d *DualSense) handleHapticsAudioOut(out []byte) {
	generation := d.IsoOutGeneration(EndpointHapticsAudioOut)
	_ = d.handleHapticsAudioOutGeneration(out, generation)
}

func (d *DualSense) handleHapticsAudioOutGeneration(out []byte,
	generation uint64) bool {
	if len(out) == 0 {
		return true
	}
	receivedAt := time.Now()

	d.mediaMu.Lock()
	if !d.speakerInterfaceActive || generation == 0 ||
		generation != d.speakerMediaGeneration {
		d.mediaMu.Unlock()
		return false
	}
	processed, lease := d.speakerAudioFeature.applyPCM(out, USBHapticsAudioChannels)
	d.mediaMu.Unlock()
	defer lease.release()

	for len(processed) >= USBHapticsAudioFrameSize {
		var reports [2]*pendingBluetoothHapticsReport
		d.mediaMu.Lock()
		if generation != d.speakerMediaGeneration || !d.speakerInterfaceActive {
			d.mediaMu.Unlock()
			return false
		}
		consumed, reportCount := d.consumeDualSenseV5AudioLocked(
			processed, receivedAt, &reports,
		)
		d.mediaMu.Unlock()
		if consumed == 0 {
			break
		}
		processed = processed[consumed:]
		for index := 0; index < reportCount; index++ {
			d.publishPendingMediaReport(reports[index])
		}
	}
	return true
}

type pendingBluetoothHapticsReport struct {
	speakerPCM    [dualSenseV5SpeakerPayloadSize]byte
	speakerLength int
	assemblyDelay time.Duration
	feedback      OutputState
	hapticsOnly   bool
	generation    uint64
}

func (d *DualSense) acquirePendingMediaReportLocked() *pendingBluetoothHapticsReport {
	select {
	case report := <-d.mediaReportFree:
		report.speakerLength = 0
		report.assemblyDelay = 0
		report.feedback = OutputState{}
		report.hapticsOnly = false
		report.generation = d.speakerMediaGeneration
		return report
	default:
		d.mediaReportDrops.Add(1)
		return nil
	}
}

func (d *DualSense) releasePendingMediaReport(report *pendingBluetoothHapticsReport) {
	if report == nil {
		return
	}
	report.speakerLength = 0
	report.feedback = OutputState{}
	report.hapticsOnly = false
	d.mediaReportFree <- report
}

func (d *DualSense) publishPendingMediaReport(pending *pendingBluetoothHapticsReport) {
	if pending == nil {
		return
	}
	d.callbackMu.RLock()
	outputFunc := d.outputFunc
	transportOutputFunc := d.transportOutputFunc
	atomicAudioHapticsFunc := d.atomicAudioHapticsFunc
	transportAtomicAudioFunc := d.transportAtomicAudioFunc
	realtimeHapticsFunc := d.realtimeHapticsFunc
	transportRealtimeHapticsFunc := d.transportRealtimeHapticsFunc
	d.callbackMu.RUnlock()
	feedback := pending.feedback
	if pending.hapticsOnly {
		if transportRealtimeHapticsFunc != nil {
			transportRealtimeHapticsFunc(feedback, pending.generation)
		} else if realtimeHapticsFunc != nil {
			realtimeHapticsFunc(feedback)
		}
	} else if transportAtomicAudioFunc != nil {
		transportAtomicAudioFunc(feedback,
			pending.speakerPCM[:pending.speakerLength], pending.generation)
	} else if atomicAudioHapticsFunc != nil {
		atomicAudioHapticsFunc(feedback,
			pending.speakerPCM[:pending.speakerLength])
	} else if transportOutputFunc != nil {
		transportOutputFunc(feedback)
	} else if outputFunc != nil {
		outputFunc(feedback)
	}
	d.releasePendingMediaReport(pending)
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
func (d *DualSense) consumeDualSenseV5AudioLocked(src []byte, now time.Time,
	reports *[2]*pendingBluetoothHapticsReport) (int, int) {
	const hapticsFrames = (BluetoothHapticsSampleSize / 2) *
		USBHapticsAudioDownsample

	framesRemaining := len(src) / USBHapticsAudioFrameSize
	if framesRemaining == 0 {
		return 0, 0
	}
	speakerFramesNeeded := dualSenseV5SpeakerFrames -
		d.v5SpeakerPCMLength/dualSenseV5SpeakerFrameSize
	hapticsFramesNeeded := hapticsFrames -
		d.hapticsPCMLength/USBHapticsAudioFrameSize
	frames := min(framesRemaining, speakerFramesNeeded, hapticsFramesNeeded)
	segmentBytes := frames * USBHapticsAudioFrameSize
	segment := src[:segmentBytes]

	if d.hapticsPCMLength == 0 {
		d.hapticsPCMStartedAt = now
	}
	copy(d.hapticsPCM[d.hapticsPCMLength:], segment)
	d.hapticsPCMLength += segmentBytes
	speakerBytes := frames * dualSenseV5SpeakerFrameSize
	copyDualSenseV5SpeakerChannels(
		d.v5SpeakerPCM[d.v5SpeakerPCMLength:d.v5SpeakerPCMLength+speakerBytes],
		segment,
	)
	d.v5SpeakerPCMLength += speakerBytes

	reportCount := 0
	// At the 7,680-frame common boundary, complete rear feedback first so the
	// simultaneous speaker generation carries that exact update.
	if d.hapticsPCMLength == len(d.hapticsPCM) {
		generation := d.completeDualSenseV5HapticsLocked(now)
		if feedback, ok := d.buildDualSenseV5RealtimeHapticsLocked(
			generation.sample[:]); ok {
			pending := d.acquirePendingMediaReportLocked()
			if pending != nil {
				pending.assemblyDelay = generation.assemblyDelay
				pending.feedback = feedback
				pending.hapticsOnly = true
				reports[reportCount] = pending
				reportCount++
			}
		}
	}
	if d.v5SpeakerPCMLength == len(d.v5SpeakerPCM) {
		feedback, assemblyDelay, ok := d.buildDualSenseV5FeedbackLocked()
		if ok {
			pending := d.acquirePendingMediaReportLocked()
			if pending != nil {
				copy(pending.speakerPCM[:], d.v5SpeakerPCM[:])
				pending.speakerLength = d.v5SpeakerPCMLength
				pending.assemblyDelay = assemblyDelay
				pending.feedback = feedback
				reports[reportCount] = pending
				reportCount++
			}
		}
		d.v5SpeakerPCMLength = 0
	}
	return segmentBytes, reportCount
}

func (d *DualSense) buildDualSenseV5RealtimeHapticsLocked(
	sample []byte) (OutputState, bool) {
	sequence := d.realtimeHapticsSeq
	interval := d.realtimeHapticsInterval
	d.realtimeHapticsSeq++
	d.realtimeHapticsInterval++
	feedback := d.mediaOutputState
	err := BuildBluetoothCombinedHapticsReportInto(
		sequence, interval, sample, feedback.RawOutputReport[:],
		feedback.BluetoothCombinedOutputReport[:])
	if err != nil {
		// The caller owns mediaMu. Keep failure handling fixed-cost and expose
		// the aggregate through diagnostics rather than invoking a logger while
		// media scheduling state is locked.
		d.mediaReportBuildFailures.Add(1)
		return OutputState{}, false
	}
	return feedback, true
}

func (d *DualSense) completeDualSenseV5HapticsLocked(
	now time.Time) dualSenseV5HapticsGeneration {
	generation := dualSenseV5HapticsGeneration{}
	copyUSBHapticsChannelsToBluetoothSample(generation.sample[:],
		d.hapticsPCM[:d.hapticsPCMLength])
	generation.assemblyDelay = now.Sub(d.hapticsPCMStartedAt)
	if d.hapticsPCMStartedAt.IsZero() || generation.assemblyDelay < 0 {
		generation.assemblyDelay = 0
	}
	if d.v5HapticsQueueCount == len(d.v5HapticsQueue) {
		// Media is time-indexed: if an impossible producer burst outruns the
		// speaker clock, discard the oldest completed rear generation rather
		// than replaying a stale backlog later.
		d.v5HapticsQueue[d.v5HapticsQueueHead] = dualSenseV5HapticsGeneration{}
		d.v5HapticsQueueHead = (d.v5HapticsQueueHead + 1) % len(d.v5HapticsQueue)
		d.v5HapticsQueueCount--
		d.mediaReportDrops.Add(1)
	}
	tail := (d.v5HapticsQueueHead + d.v5HapticsQueueCount) % len(d.v5HapticsQueue)
	d.v5HapticsQueue[tail] = generation
	d.v5HapticsQueueCount++
	d.hapticsPCMLength = 0
	d.hapticsPCMStartedAt = time.Time{}
	return generation
}

func (d *DualSense) buildDualSenseV5FeedbackLocked() (OutputState,
	time.Duration, bool) {
	var sample [BluetoothHapticsSampleSize]byte
	var assemblyDelay time.Duration
	if d.v5HapticsQueueCount != 0 {
		generation := d.v5HapticsQueue[d.v5HapticsQueueHead]
		sample = generation.sample
		assemblyDelay = generation.assemblyDelay
		d.v5HapticsQueue[d.v5HapticsQueueHead] = dualSenseV5HapticsGeneration{}
		d.v5HapticsQueueHead = (d.v5HapticsQueueHead + 1) % len(d.v5HapticsQueue)
		d.v5HapticsQueueCount--
	}

	sequence := d.hapticsSeq
	interval := d.hapticsInterval
	d.hapticsSeq++
	d.hapticsInterval++

	feedback := d.mediaOutputState
	err := BuildBluetoothCombinedHapticsReportInto(
		sequence, interval, sample[:], feedback.RawOutputReport[:],
		feedback.BluetoothCombinedOutputReport[:])
	if err != nil {
		d.mediaReportBuildFailures.Add(1)
		return OutputState{}, 0, false
	}
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
				length := InputReportSize
				if wLength > 0 && int(wLength) < length {
					length = int(wLength)
				}
				b := make([]byte, length)
				d.SnapshotInputReportInto(b)
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
				d.outputMu.Lock()
				d.subcommand[0] = data[1]
				d.subcommand[1] = data[2]
				d.outputMu.Unlock()
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

// TryHandleOutputCommand owns only native HID output admission. The USB/IP
// reader can report a full feedback queue without blocking input/ISO ingestion
// or falsely acknowledging a command whose partial fields were discarded.
func (d *DualSense) TryHandleOutputCommand(endpoint uint8,
	setup [8]byte, out []byte) (handled, accepted bool) {
	if endpoint == EndpointOut&0x0f {
		return true, d.handleOutputReport(out)
	}
	if endpoint != 0 || setup[0] != hidClassOUT || setup[1] != hidSetReport ||
		binary.LittleEndian.Uint16(setup[2:4]) != uint16(reportTypeOutput)<<8|uint16(ReportIDOutput) {
		return false, false
	}
	// Leave malformed or differently addressed control requests to the existing
	// control policy; this seam must not claim feature or enumeration traffic.
	interfaceNumber := binary.LittleEndian.Uint16(setup[4:6])
	if interfaceNumber > 0xff || int(binary.LittleEndian.Uint16(setup[6:8])) != len(out) {
		return false, false
	}
	iface, exists := d.descriptor.Interface(uint8(interfaceNumber))
	if !exists || iface.Descriptor.BInterfaceClass != 0x03 {
		return false, false
	}
	return true, d.handleOutputReport(out)
}

func (d *DualSense) handleOutputReport(out []byte) bool {
	var normalized [OutputReportSize]byte
	report, ok := normalizeOutputReportInto(out, &normalized)
	if !ok {
		return false
	}
	d.outputMu.Lock()
	feedback, mediaState := d.mergeOutputReport(report)
	d.callbackMu.RLock()
	transportOutputFunc := d.transportOutputFunc
	outputFunc := d.outputFunc
	// The transport callback is fixed-cost queue admission, never socket I/O
	// or a wait. Keep its registration current and the candidate snapshot private
	// until admission succeeds, so a rejected command cannot leak via later audio.
	if transportOutputFunc != nil && !transportOutputFunc(feedback) {
		d.callbackMu.RUnlock()
		d.outputMu.Unlock()
		return false
	}
	d.outputState = mediaState
	d.mediaMu.Lock()
	d.mediaOutputState = mediaState
	d.mediaMu.Unlock()
	d.callbackMu.RUnlock()
	d.outputMu.Unlock()
	if transportOutputFunc == nil && outputFunc != nil {
		outputFunc(feedback)
	}
	return true
}

func normalizeOutputReportInto(out []byte,
	destination *[OutputReportSize]byte) ([]byte, bool) {
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
		length := min(len(out)+1, len(destination))
		destination[0] = ReportIDOutput
		copy(destination[1:length], out)
		return destination[:length], true
	}
	return nil, false
}

var featureGetHandlers = map[byte]func(*DualSense) []byte{
	featureIDCalibration:     (*DualSense).featureReportCalibration,
	featureIDPairing:         (*DualSense).featureReportPairing,
	featureIDFirmware:        (*DualSense).featureReportFirmware,
	featureIDCommandResponse: (*DualSense).featureReportCommandResponse,
}

func (d *DualSense) mergeOutputReport(out []byte) (OutputState, OutputState) {
	// The V5 wire image has fixed size, but short HID writes only authorize
	// complete fields actually present in the source. Clear validity for an
	// incomplete group before padding, so absent trigger/LED bytes cannot become
	// an invented zero command. Keep the original bound for cumulative merging.
	var command [OutputReportSize]byte
	copy(command[:], out)
	clearIncompleteOutputValidity(&command, len(out))
	out = command[:min(len(out), OutputReportSize)]
	feedback := d.outputState
	clear(feedback.BluetoothCombinedOutputReport[:])
	mergeRawOutputReport(&feedback.RawOutputReport, out)

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
	if len(out) >= outputRightTriggerOffset+outputTriggerLength {
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
		if flag0&0x08 != 0 && len(out) >= outputLeftTriggerOffset+outputTriggerLength {
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
	mediaState := feedback
	// The persistent snapshot above feeds VIIPER's atomic audio/haptics
	// assembler, where independently flagged USB fields must remain coherent.
	// The ordinary output callback has a different contract: it represents the
	// exact SET_REPORT update issued by the game. Returning the accumulated
	// snapshot there replayed one-shot trigger and LED-release validity bits on
	// later rumble/audio writes. Keep the two contracts separate.
	feedback.RawOutputReport = command
	return feedback, mediaState
}

func clearIncompleteOutputValidity(command *[OutputReportSize]byte, length int) {
	if length >= OutputReportSize {
		return
	}
	for _, field := range [...]struct {
		flagOffset int
		mask       byte
		end        int
	}{
		{outputFlag0Offset, outputFlag0RumbleMask, 5},
		{outputFlag0Offset, outputFlag0HeadphoneVolume, 6},
		{outputFlag0Offset, outputFlag0SpeakerVolume, 7},
		{outputFlag0Offset, outputFlag0MicrophoneVolume, 8},
		{outputFlag0Offset, outputFlag0AudioControl, 9},
		{outputFlag0Offset, outputFlag0RightTrigger, outputRightTriggerOffset + outputTriggerLength},
		{outputFlag0Offset, outputFlag0LeftTrigger, outputLeftTriggerOffset + outputTriggerLength},
		{outputFlag1Offset, outputFlag1MicrophoneLed, 10},
		{outputFlag1Offset, outputFlag1PowerSave, 11},
		{outputFlag1Offset, outputFlag1HapticsLowPass, 41},
		{outputFlag1Offset, outputFlag1MotorPower, 38},
		{outputFlag1Offset, outputFlag1AudioControl2, 39},
		{outputFlag1Offset, outputFlag1PlayerLeds, outputPlayerLedsOffset + 1},
		{outputFlag1Offset, outputFlag1Lightbar, outputLightbarOffset + 3},
		{outputFlag2Offset, outputFlag2LightbarBrightness, 44},
		{outputFlag2Offset, outputFlag2LightbarSetup, 43},
	} {
		if length < field.end {
			command[field.flagOffset] &^= field.mask
		}
	}
}

func mergeRawOutputReport(snapshot *[OutputReportSize]byte, update []byte) {
	if snapshot == nil || len(update) < 5 ||
		update[0] != ReportIDOutput {
		return
	}

	if snapshot[0] != ReportIDOutput {
		clear(snapshot[:])
		snapshot[0] = ReportIDOutput
	}

	flag0 := update[outputFlag0Offset]
	flag1 := update[outputFlag1Offset]
	var flag2 byte
	if len(update) > outputFlag2Offset {
		flag2 = update[outputFlag2Offset]
	}

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

	d.metaMu.Lock()
	mac := d.metaState.MACAddress
	d.metaMu.Unlock()

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

	d.metaMu.Lock()
	bt := d.metaState.BuildTime
	d.metaMu.Unlock()

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

	d.outputMu.Lock()
	sub := d.subcommand
	d.outputMu.Unlock()
	d.metaMu.Lock()
	serial := d.metaState.SerialNumber
	voltage := d.metaState.BatteryVoltage
	temp := d.metaState.TemperatureCelsius
	d.metaMu.Unlock()

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
	battery := byte(0)
	if m != nil {
		battery = m.BatteryStatus
	}
	now := time.Now()
	d.input.mu.Lock()
	timestamp := dualSenseTimestampTicks(d.input.timestampBase, now)
	d.input.mu.Unlock()
	// Compatibility helper for non-hot tests and callers. The persistent
	// interrupt encoder is the only owner of the streaming sequence and last
	// presented report, so this isolated build deliberately uses sequence one
	// and never mutates those fields.
	if !encodeUSBInputReportInto(s, battery, 1, 1, timestamp, d.input.edge,
		b) {
		d.input.mu.Lock()
		d.input.corruptReports++
		d.input.mu.Unlock()
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

func resetUSBInputReportToNeutral(b []byte, seq uint8, packetSequence,
	timestamp uint32, battery byte, edge bool, metadata *InputState) {
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
	binary.LittleEndian.PutUint32(b[12:16], packetSequence)

	x, y, z := DefaultAccelRaw()
	binary.LittleEndian.PutUint16(b[22:24], uint16(x))
	binary.LittleEndian.PutUint16(b[24:26], uint16(y))
	binary.LittleEndian.PutUint16(b[26:28], uint16(z))
	binary.LittleEndian.PutUint32(b[28:32], timestamp)

	b[33] = TouchInactiveMask
	b[37] = TouchInactiveMask
	encodeUSBInputMetadata(b, metadata, timestamp, edge, battery)
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

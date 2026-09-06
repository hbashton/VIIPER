package e2e_bench_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/Alia5/VIIPER/_testing/e2e/sdl"
	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/internal/cmd"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/viiperclient"
	"github.com/Alia5/VIIPER/viipertypes"

	_ "github.com/Alia5/VIIPER/internal/devicecatalog" // Register all device handlers.
)

const (
	dualSenseDefaultAPIListen = "127.0.0.1:3345"
	dualSenseDefaultAPIAddr   = "127.0.0.1:3345"
	dualSenseDefaultUSBListen = "127.0.0.1:3344"
	dualSenseDeviceWait       = 15 * time.Second
	dualSenseTransitionWait   = 100 * time.Millisecond
	dualSenseFalseRepressWait = 3 * time.Millisecond
	dualSenseDefaultWarmup    = 100
)

type dualSenseHarness struct {
	ctx      context.Context
	cancel   context.CancelFunc
	client   *viiperclient.Client
	busID    uint32
	external bool
	done     chan error
	diag     *endpointDiagnosticCollector
}

type endpointDiagnosticCollector struct {
	mu     sync.Mutex
	latest any
	has    bool
}

func (c *endpointDiagnosticCollector) Enabled(context.Context, slog.Level) bool { return true }

func (c *endpointDiagnosticCollector) Handle(_ context.Context, record slog.Record) error {
	if record.Message != "USB/IP endpoint scheduling diagnostics" {
		return nil
	}
	record.Attrs(func(attribute slog.Attr) bool {
		if attribute.Key != "snapshot" {
			return true
		}
		c.mu.Lock()
		c.latest = attribute.Value.Any()
		c.has = true
		c.mu.Unlock()
		return false
	})
	return nil
}

func (c *endpointDiagnosticCollector) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *endpointDiagnosticCollector) WithGroup(string) slog.Handler { return c }

func (c *endpointDiagnosticCollector) reset() {
	c.mu.Lock()
	c.latest = nil
	c.has = false
	c.mu.Unlock()
}

func (c *endpointDiagnosticCollector) snapshot() (any, bool) {
	c.mu.Lock()
	latest, has := c.latest, c.has
	c.mu.Unlock()
	return latest, has
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func envBool(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}

func transitionTimeout() time.Duration {
	milliseconds := envInt("VIIPER_E2E_TRANSITION_TIMEOUT_MS",
		int(dualSenseTransitionWait/time.Millisecond))
	if milliseconds < 2 {
		milliseconds = 2
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func newDualSenseHarness(tb testing.TB) *dualSenseHarness {
	tb.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	harness := &dualSenseHarness{
		ctx:      ctx,
		cancel:   cancel,
		external: envBool("VIIPER_E2E_EXTERNAL"),
		done:     make(chan error, 1),
		diag:     &endpointDiagnosticCollector{},
	}

	apiAddress := envOrDefault("VIIPER_E2E_API_ADDR", dualSenseDefaultAPIAddr)
	password := os.Getenv("VIIPER_E2E_PASSWORD")
	if harness.external {
		close(harness.done)
	} else {
		usbConfig := usb.ServerConfig{
			Addr:              envOrDefault("VIIPER_E2E_USB_LISTEN", dualSenseDefaultUSBListen),
			BusCleanupTimeout: time.Second,
		}
		enableOptionalEndpointDiagnostics(&usbConfig)
		server := cmd.Server{
			USBServerConfig: usbConfig,
			APIServerConfig: api.ServerConfig{
				Addr:                        envOrDefault("VIIPER_E2E_API_LISTEN", dualSenseDefaultAPIListen),
				AutoAttachLocalClient:       true,
				DeviceHandlerConnectTimeout: 10 * time.Second,
				Password:                    password,
				PlatformOpts: api.PlatformOpts{
					AutoAttachWindowsNative: true,
				},
			},
			ConnectionTimeout: 10 * time.Second,
		}
		logger := slog.New(harness.diag)
		go func() { harness.done <- server.StartServer(ctx, logger, nil) }()
	}

	if password == "" {
		harness.client = viiperclient.New(apiAddress)
	} else {
		harness.client = viiperclient.NewWithPassword(apiAddress, password)
	}

	var response *viipertypes.BusCreateResponse
	var err error
	for attempt := 0; attempt < 40; attempt++ {
		response, err = harness.client.BusCreate(1)
		if err == nil {
			break
		}
		select {
		case serverErr, ok := <-harness.done:
			if ok && serverErr != nil {
				cancel()
				tb.Fatalf("VIIPER server exited before API readiness: %v", serverErr)
			}
		default:
		}
		time.Sleep(250 * time.Millisecond)
	}
	if response == nil {
		cancel()
		tb.Fatalf("BusCreate at %s failed: %v", apiAddress, err)
	}
	harness.busID = response.BusID
	return harness
}

func enableOptionalEndpointDiagnostics(config *usb.ServerConfig) {
	// Keep this harness source-compatible with a pristine baseline whose
	// ServerConfig predates endpoint diagnostics. Patched servers expose and
	// enable the field; old servers simply provide no local scheduler snapshot.
	value := reflect.ValueOf(config).Elem().FieldByName("EndpointDiagnostics")
	if value.IsValid() && value.CanSet() && value.Kind() == reflect.Bool {
		value.SetBool(true)
	}
}

func (h *dualSenseHarness) close(tb testing.TB) {
	tb.Helper()
	if h.busID != 0 {
		if _, err := h.client.BusRemove(h.busID); err != nil {
			tb.Logf("BusRemove(%d): %v", h.busID, err)
		}
	}
	h.cancel()
	if h.external {
		return
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case err := <-h.done:
		if err != nil && h.ctx.Err() == nil {
			tb.Logf("VIIPER server shutdown: %v", err)
		}
	case <-timer.C:
		tb.Log("VIIPER in-process server did not stop within 3 seconds")
	}
}

type triggerObserver struct {
	gamepad  *sdl.Gamepad
	id       sdl.GamepadID
	neutral  int16
	previous int16
	stopOnce sync.Once
	dropped  uint64
}

func startTriggerObserver(gamepad *sdl.Gamepad) (*triggerObserver, error) {
	return startGamepadAxisObserver(gamepad, sdl.GamepadAxisRightTrigger)
}

func startGamepadAxisObserver(gamepad *sdl.Gamepad,
	axis sdl.GamepadAxis) (*triggerObserver, error) {
	sdl.UpdateGamepads()
	observer := &triggerObserver{
		gamepad: gamepad,
		id:      gamepad.ID(),
		neutral: gamepad.GetAxis(axis),
	}
	observer.previous = observer.neutral
	if err := sdl.StartGamepadAxisWatch(observer.id, axis); err != nil {
		return nil, err
	}
	drainObservations(observer)
	return observer, nil
}

func (o *triggerObserver) pollTriggerObservation() (triggerObservation, bool) {
	for {
		sdl.UpdateGamepads()
		axis, ok := sdl.PollWatchedGamepadAxis()
		if !ok {
			return triggerObservation{}, false
		}
		if axis == o.previous {
			continue
		}
		o.previous = axis
		return triggerObservation{
			axis: axis, released: axis == o.neutral, at: time.Now(),
		}, true
	}
}

func (o *triggerObserver) close() uint64 {
	o.stopOnce.Do(func() { o.dropped = sdl.StopGamepadAxisWatch() })
	return o.dropped
}

func gamepadSet() (map[sdl.GamepadID]struct{}, error) {
	sdl.UpdateGamepads()
	ids, err := sdl.GetGamepads()
	if err != nil {
		return nil, err
	}
	result := make(map[sdl.GamepadID]struct{}, len(ids))
	for _, id := range ids {
		result[id] = struct{}{}
	}
	return result, nil
}

func waitForVirtualPS5(previous map[sdl.GamepadID]struct{}) (*sdl.Gamepad, error) {
	deadline := time.Now().Add(dualSenseDeviceWait)
	for time.Now().Before(deadline) {
		sdl.UpdateGamepads()
		ids, err := sdl.GetGamepads()
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if _, existed := previous[id]; existed {
				continue
			}
			if sdl.GetGamepadTypeForID(id) != sdl.GamepadTypePS5 {
				continue
			}
			gamepad, err := sdl.OpenGamepad(id)
			if err != nil {
				return nil, err
			}
			if gamepad.Type() != sdl.GamepadTypePS5 {
				gamepad.Close()
				continue
			}
			return gamepad, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("no new SDL3 PS5 gamepad appeared within %s", dualSenseDeviceWait)
}

func audioDeviceSet(recording bool) (map[sdl.AudioDeviceID]struct{}, error) {
	var (
		devices []sdl.AudioDeviceID
		err     error
	)
	if recording {
		devices, err = sdl.GetAudioRecordingDevices()
	} else {
		devices, err = sdl.GetAudioPlaybackDevices()
	}
	if err != nil {
		return nil, err
	}
	result := make(map[sdl.AudioDeviceID]struct{}, len(devices))
	for _, device := range devices {
		result[device] = struct{}{}
	}
	return result, nil
}

func findNewAudioDevice(previous map[sdl.AudioDeviceID]struct{}, recording bool,
	timeout time.Duration) (sdl.AudioDeviceID, string) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var devices []sdl.AudioDeviceID
		if recording {
			devices, _ = sdl.GetAudioRecordingDevices()
		} else {
			devices, _ = sdl.GetAudioPlaybackDevices()
		}
		for _, device := range devices {
			if _, existed := previous[device]; !existed {
				return device, sdl.GetAudioDeviceName(device)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, ""
}

type loadedTraffic struct {
	playback       *sdl.AudioStream
	recording      *sdl.AudioStream
	gamepad        *sdl.Gamepad
	stop           chan struct{}
	done           chan struct{}
	stopOnce       sync.Once
	playbackWrites atomic.Uint64
	recordingReads atomic.Uint64
	effectWrites   atomic.Uint64
	playbackErrors atomic.Uint64
	recordingError atomic.Uint64
	pcm            []byte
	capture        []byte
	effect         []byte
}

func newLoadedTraffic(gamepad *sdl.Gamepad, playbackID,
	recordingID sdl.AudioDeviceID) (*loadedTraffic, error, error) {
	traffic := &loadedTraffic{
		gamepad: gamepad,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		pcm:     make([]byte, 480*4*2),
		capture: make([]byte, dualsense.USBMicrophoneClientFrameSize),
		effect:  make([]byte, dualsense.OutputReportSize),
	}
	for frame := 0; frame < 480; frame++ {
		front := int16((frame%120 - 60) * 128)
		rear := int16((frame%32 - 16) * 640)
		offset := frame * 8
		binary.LittleEndian.PutUint16(traffic.pcm[offset:offset+2], uint16(front))
		binary.LittleEndian.PutUint16(traffic.pcm[offset+2:offset+4], uint16(-front))
		binary.LittleEndian.PutUint16(traffic.pcm[offset+4:offset+6], uint16(rear))
		binary.LittleEndian.PutUint16(traffic.pcm[offset+6:offset+8], uint16(-rear))
	}
	traffic.effect[0] = dualsense.ReportIDOutput
	traffic.effect[1] = 0x0C // R2 and L2 adaptive-trigger blocks are valid.
	traffic.effect[11] = 0x01
	traffic.effect[12] = 0x60
	traffic.effect[13] = 0xA0
	traffic.effect[22] = 0x01
	traffic.effect[23] = 0x60
	traffic.effect[24] = 0xA0

	var playbackErr error
	if playbackID != 0 {
		traffic.playback, playbackErr = sdl.OpenAudioDeviceStream(playbackID, sdl.AudioSpec{
			Format: sdl.AudioS16, Channels: 4, Freq: 48000,
		})
		if playbackErr == nil {
			playbackErr = traffic.playback.Resume()
		}
		if playbackErr != nil && traffic.playback != nil {
			traffic.playback.Close()
			traffic.playback = nil
		}
	}

	var recordingErr error
	if recordingID != 0 {
		traffic.recording, recordingErr = sdl.OpenAudioDeviceStream(recordingID, sdl.AudioSpec{
			Format: sdl.AudioS16, Channels: 2, Freq: 48000,
		})
		if recordingErr == nil {
			recordingErr = traffic.recording.Resume()
		}
		if recordingErr != nil && traffic.recording != nil {
			traffic.recording.Close()
			traffic.recording = nil
		}
	}

	go traffic.run()
	return traffic, playbackErr, recordingErr
}

func (l *loadedTraffic) run() {
	defer close(l.done)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var cycle uint64
	for {
		select {
		case <-ticker.C:
			cycle++
			if l.playback != nil {
				if err := l.playback.Put(l.pcm[:]); err != nil {
					l.playbackErrors.Add(1)
				} else {
					l.playbackWrites.Add(1)
				}
			}
			if l.recording != nil {
				for read := 0; read < 4 && l.recording.Available() > 0; read++ {
					n, err := l.recording.Get(l.capture[:])
					if err != nil {
						l.recordingError.Add(1)
						break
					}
					if n == 0 {
						break
					}
					l.recordingReads.Add(1)
				}
			}
			l.gamepad.Rumble(uint16(8000+cycle%12000), 24000, 25)
			l.gamepad.RumbleTriggers(14000, 22000, 25)
			l.gamepad.SetLED(uint8(cycle), uint8(cycle*3), uint8(cycle*7))
			if l.gamepad.SendEffect(unsafe.Pointer(&l.effect[0]), len(l.effect)) {
				l.effectWrites.Add(1)
			}
		case <-l.stop:
			return
		}
	}
}

func (l *loadedTraffic) close() {
	l.stopOnce.Do(func() { close(l.stop) })
	<-l.done
	if l.recording != nil {
		l.recording.Close()
	}
	if l.playback != nil {
		l.playback.Close()
	}
}

func makeDualSenseInput(index int, trigger uint8) dualsense.InputState {
	state := dualsense.InputState{
		LX:     int8(index),
		LY:     int8(index >> 1),
		RX:     int8(index * 3),
		RY:     int8(index * 5),
		GyroX:  int16(index * 17),
		GyroY:  int16(index * 31),
		GyroZ:  int16(index * 47),
		AccelX: int16(index * 13),
		AccelY: int16(index * 19),
		AccelZ: int16(8192 + index%512),
	}
	if trigger != 0 {
		state.Buttons = dualsense.ButtonR2
		state.R2 = trigger
	}
	return state
}

func watchForFalseRepress(timer *time.Timer, observations triggerObservationSource,
	results *transitionResults, trace *transitionTrace) {
	resetTimer(timer, dualSenseFalseRepressWait)
	for {
		if observation, ok := observations.pollTriggerObservation(); ok {
			if trace != nil {
				trace.record(observation, 2, transitionTraceQuiet)
			}
			if observation.axis >= sdlTriggerPeakAxis {
				results.falseRepress++
			}
			continue
		}
		select {
		case <-timer.C:
			return
		default:
			runtime.Gosched()
		}
	}
}

func sendTransitionPair(writer *v5FrameWriter, index int, count int,
	expected *[2]expectedTransition) (int, error) {
	expectedCount := min(2, count)
	states := [5]dualsense.InputState{
		makeDualSenseInput(index*5, 0),
		makeDualSenseInput(index*5+1, 1),
		makeDualSenseInput(index*5+2, 80),
		makeDualSenseInput(index*5+3, 255),
		makeDualSenseInput(index*5+4, 0),
	}
	frameCount := 4
	if !writer.enqueue(states[0]) {
		return 0, writer.terminalErr
	}
	expected[0] = expectedTransition{pressed: true, sentAt: time.Now()}
	for stateIndex := 1; stateIndex < 4; stateIndex++ {
		if !writer.enqueue(states[stateIndex]) {
			return 0, writer.terminalErr
		}
	}
	if expectedCount == 2 {
		expected[1] = expectedTransition{pressed: false, sentAt: time.Now()}
		if !writer.enqueue(states[4]) {
			return 0, writer.terminalErr
		}
		frameCount++
	}
	for result := 0; result < frameCount; result++ {
		if err := writer.waitInputResult(); err != nil {
			return 0, err
		}
	}
	return expectedCount, nil
}

func warmDualSensePath(writer *v5FrameWriter, observer *triggerObserver,
	timer *time.Timer, transitions int, timeout time.Duration) error {
	results := newTransitionResults(transitions)
	var expected [2]expectedTransition
	for index := 0; index < transitions; index += 2 {
		drainObservations(observer)
		expectedCount, err := sendTransitionPair(writer, index, transitions-index, &expected)
		if err != nil {
			return err
		}
		waitForExpectedTransitions(timer, observer, expected[:expectedCount],
			timeout, &results, nil)
		if expectedCount == 2 {
			watchForFalseRepress(timer, observer, &results, nil)
		}
	}
	if transitions%2 != 0 {
		neutral := makeDualSenseInput(transitions*5, 0)
		if !writer.enqueue(neutral) {
			return writer.terminalErr
		}
		return writer.waitInputResult()
	}
	return nil
}

// Benchmark_DualSenseV5_ConsumerObservedLatency measures the same process-local
// publication timestamp against SDL3's observation of the Windows virtual PS5
// gamepad. Run production samples with -benchtime=10000x (or higher); use a
// smaller fixed x count for smoke runs. The harness never substitutes an
// internal device callback for consumer observation.
func Benchmark_DualSenseV5_ConsumerObservedLatency(b *testing.B) {
	if err := sdl.Init(sdl.InitFlagGamepad | sdl.InitFlagAudio); err != nil {
		b.Fatalf("SDL3 gamepad/audio initialization failed: %v", err)
	}
	defer sdl.Quit()

	harness := newDualSenseHarness(b)
	defer harness.close(b)

	for _, mode := range []struct {
		name   string
		loaded bool
	}{
		{name: "Idle", loaded: false},
		{name: "Loaded", loaded: true},
	} {
		b.Run(mode.name, func(b *testing.B) {
			runDualSenseV5Benchmark(b, harness, mode.loaded)
		})
	}
}

func runDualSenseV5Benchmark(b *testing.B, harness *dualSenseHarness, loaded bool) {
	b.Helper()
	harness.diag.reset()
	if b.N < 10000 {
		b.Logf("smoke-sized run: %d transitions; production comparison requires at least 10000", b.N)
	}
	existingGamepads, err := gamepadSet()
	if err != nil {
		b.Fatalf("snapshot SDL3 gamepads: %v", err)
	}
	existingPlayback, playbackSnapshotErr := audioDeviceSet(false)
	existingRecording, recordingSnapshotErr := audioDeviceSet(true)

	deviceType := envOrDefault("VIIPER_DUALSENSE_DEVICE_TYPE",
		dualsense.DeviceTypeCombinedAudioDuplexV5RawInputEvents)
	deviceInfo, err := harness.client.DeviceAdd(harness.busID, deviceType, nil)
	if err != nil {
		b.Fatalf("DeviceAdd(%q): %v (for an older baseline set VIIPER_DUALSENSE_DEVICE_TYPE=%s)",
			deviceType, err, dualsense.DeviceTypeCombinedAudioDuplexV5)
	}
	defer func() {
		if _, removeErr := harness.client.DeviceRemove(harness.busID, deviceInfo.DevID); removeErr != nil {
			b.Logf("DeviceRemove(%s): %v", deviceInfo.DevID, removeErr)
		}
	}()

	stream, err := harness.client.OpenStream(harness.ctx, harness.busID, deviceInfo.DevID)
	if err != nil {
		b.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close() //nolint:errcheck

	outputCounters := &v5OutputCounters{}
	outputDone := make(chan struct{})
	go drainV5Output(stream, outputCounters, outputDone)

	gamepad, err := waitForVirtualPS5(existingGamepads)
	if err != nil {
		b.Fatalf("actual SDL3 virtual PS5 discovery failed: %v", err)
	}
	defer gamepad.Close()
	b.Logf("SDL3 consumer: name=%q type=%s vid=%04x pid=%04x",
		gamepad.Name(), gamepad.Type().Name(), gamepad.Vendor(), gamepad.Product())

	var traffic *loadedTraffic
	if loaded {
		var playbackID, recordingID sdl.AudioDeviceID
		var playbackName, recordingName string
		if playbackSnapshotErr == nil {
			playbackID, playbackName = findNewAudioDevice(existingPlayback, false, 5*time.Second)
		}
		if recordingSnapshotErr == nil {
			recordingID, recordingName = findNewAudioDevice(existingRecording, true, 5*time.Second)
		}
		traffic, playbackErr, recordingErr := newLoadedTraffic(gamepad, playbackID, recordingID)
		defer traffic.close()
		switch {
		case playbackSnapshotErr != nil:
			b.Logf("virtual playback activation unavailable: enumerate before attach: %v", playbackSnapshotErr)
		case playbackID == 0:
			b.Log("virtual playback activation unavailable: SDL3 did not expose a new endpoint; advanced-haptics ISO OUT is not consumer-activated in this run")
		case playbackErr != nil:
			b.Logf("virtual playback activation unavailable: endpoint %q: %v", playbackName, playbackErr)
		default:
			b.Logf("virtual four-channel 48 kHz playback active: %q", playbackName)
		}
		switch {
		case recordingSnapshotErr != nil:
			b.Logf("virtual microphone activation unavailable: enumerate before attach: %v", recordingSnapshotErr)
		case recordingID == 0:
			b.Log("virtual microphone activation unavailable: SDL3 did not expose a new recording endpoint; V5 microphone frames still load transport but ISO IN is not consumer-activated")
		case recordingErr != nil:
			b.Logf("virtual microphone activation unavailable: endpoint %q: %v", recordingName, recordingErr)
		default:
			b.Logf("virtual two-channel 48 kHz recording active: %q", recordingName)
		}
	}

	writer := newV5FrameWriter(stream, loaded,
		strings.Contains(strings.ToLower(deviceType), "rawinput"))
	defer writer.stopAndWait()
	observer, err := startTriggerObserver(gamepad)
	if err != nil {
		b.Fatalf("install SDL3 trigger-axis event watch: %v", err)
	}
	defer observer.close()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	warmup := envInt("VIIPER_E2E_WARMUP", dualSenseDefaultWarmup)
	timeout := transitionTimeout()
	if err := warmDualSensePath(writer, observer, timer, warmup, timeout); err != nil {
		b.Fatalf("DualSense V5 warmup: %v", err)
	}
	drainObservations(observer)
	results := newTransitionResults(b.N)
	var expected [2]expectedTransition
	var epochTrace, firstFailureTrace transitionTrace
	hasFailureTrace := false
	var memoryBefore, memoryAfter runtime.MemStats

	b.ReportAllocs()
	runtime.ReadMemStats(&memoryBefore)
	b.ResetTimer()
	for index := 0; index < b.N; index += 2 {
		epochTrace.reset(index / 2)
		missedBefore := results.missed
		reorderedBefore := results.reordered
		falseRepressBefore := results.falseRepress
		expectedCount, err := sendTransitionPair(writer, index, b.N-index, &expected)
		if err != nil {
			b.StopTimer()
			b.Fatalf("V5 input frame write at transition %d: %v", index, err)
		}
		waitForExpectedTransitions(timer, observer, expected[:expectedCount],
			timeout, &results, &epochTrace)
		if expectedCount == 2 {
			watchForFalseRepress(timer, observer, &results, &epochTrace)
		}
		if !hasFailureTrace && (results.missed != missedBefore ||
			results.reordered != reorderedBefore ||
			results.falseRepress != falseRepressBefore) {
			firstFailureTrace = epochTrace
			hasFailureTrace = true
		}
	}
	b.StopTimer()
	runtime.ReadMemStats(&memoryAfter)
	observerDrops := observer.close()
	deviceDiagnostics, deviceDiagnosticsErr := harness.client.DevicesList(harness.busID)

	if b.N%2 != 0 {
		neutral := makeDualSenseInput(b.N*5, 0)
		if writer.enqueue(neutral) {
			_ = writer.waitInputResult()
		}
	}
	distribution := summarizeLatencies(results.latencies[:results.latencyCount])
	b.ReportMetric(float64(results.delivered), "delivered-transitions")
	b.ReportMetric(float64(results.missed), "missed-transitions")
	b.ReportMetric(float64(results.reordered), "reordered-transitions")
	b.ReportMetric(float64(results.falseRepress), "false-represses")
	b.ReportMetric(float64(results.peakAxis), "peak-trigger-axis")
	b.ReportMetric(float64(observerDrops), "sdl-axis-events-dropped")
	b.ReportMetric(float64(distribution.count), "latency-samples")
	b.ReportMetric(float64(distribution.median)/float64(time.Microsecond), "p50-us")
	b.ReportMetric(float64(distribution.p95)/float64(time.Microsecond), "p95-us")
	b.ReportMetric(float64(distribution.p99)/float64(time.Microsecond), "p99-us")
	if distribution.count >= 1000 {
		b.ReportMetric(float64(distribution.p999)/float64(time.Microsecond), "p99.9-us")
	}
	b.ReportMetric(float64(distribution.max)/float64(time.Microsecond), "max-us")
	b.ReportMetric(float64(memoryAfter.Mallocs-memoryBefore.Mallocs), "process-mallocs")
	b.ReportMetric(float64(memoryAfter.TotalAlloc-memoryBefore.TotalAlloc), "process-allocated-bytes")
	b.ReportMetric(float64(memoryAfter.NumGC-memoryBefore.NumGC), "process-gc-cycles")
	b.ReportMetric(float64(outputCounters.outputState.Load()), "v5-output-frames")
	b.ReportMetric(float64(outputCounters.atomicAudio.Load()), "v5-audio-frames")
	b.ReportMetric(float64(outputCounters.realtimeHaptics.Load()), "v5-realtime-haptics")
	b.ReportMetric(float64(outputCounters.microphoneEvents.Load()), "v5-mic-events")
	b.ReportMetric(float64(outputCounters.invalidFrames.Load()), "v5-invalid-output-frames")
	if outputCounters.microphoneActive.Load() {
		b.ReportMetric(1, "mic-interface-active")
	} else {
		b.ReportMetric(0, "mic-interface-active")
	}
	if traffic != nil {
		b.ReportMetric(float64(traffic.playbackWrites.Load()), "audio-playback-writes")
		b.ReportMetric(float64(traffic.recordingReads.Load()), "audio-capture-reads")
		b.ReportMetric(float64(traffic.effectWrites.Load()), "adaptive-effect-writes")
		b.ReportMetric(float64(traffic.playbackErrors.Load()), "audio-playback-errors")
		b.ReportMetric(float64(traffic.recordingError.Load()), "audio-capture-errors")
	}
	if diagnostics, ok := harness.diag.snapshot(); ok {
		reportUSBIPDiagnostics(b, diagnostics)
	} else {
		b.ReportMetric(0, "usb-diagnostics-snapshot")
		if harness.external {
			b.Log("USB/IP scheduler diagnostics are unavailable from an external server process")
		} else {
			b.Log("run ended before the five-second aggregate USB/IP diagnostics interval")
		}
	}
	if deviceDiagnosticsErr != nil {
		b.Logf("DualSense input diagnostics unavailable: %v", deviceDiagnosticsErr)
	} else {
		reportDualSenseInputDiagnostics(b, deviceDiagnostics, deviceInfo.DevID)
	}
	b.Logf("consumer-observed transitions delivered=%d missed=%d reordered=%d false-repress=%d latency count=%d median=%s p95=%s p99=%s p99.9=%s max=%s",
		results.delivered, results.missed, results.reordered, results.falseRepress,
		distribution.count, distribution.median, distribution.p95, distribution.p99,
		distribution.p999, distribution.max)
	if hasFailureTrace {
		b.Logf("first correctness failure epoch=%d timed-out=%t remaining=%d observations=%+v",
			firstFailureTrace.epoch, firstFailureTrace.timedOut,
			firstFailureTrace.remaining,
			firstFailureTrace.entries[:firstFailureTrace.count])
	}

	if results.missed != 0 || results.reordered != 0 || results.falseRepress != 0 {
		b.Errorf("transition correctness failure: delivered=%d missed=%d reordered=%d false-repress=%d",
			results.delivered, results.missed, results.reordered, results.falseRepress)
	}
}

func reportUSBIPDiagnostics(b *testing.B, diagnostics any) {
	b.Helper()
	var queueHighWater int
	var queueAgeP99, queueAgeMax time.Duration
	var latenessP99, latenessMax time.Duration
	var hidQueueHighWater, hidOverflow int
	var hidQueueAge, hidLateness reflect.Value
	root := indirectValue(reflect.ValueOf(diagnostics))
	endpoints := fieldValue(root, "Endpoints")
	if endpoints.IsValid() && endpoints.Kind() == reflect.Slice {
		for index := 0; index < endpoints.Len(); index++ {
			endpoint := indirectValue(endpoints.Index(index))
			queueHighWater = max(queueHighWater, intField(endpoint, "QueueHighWater"))
			queueAge := indirectValue(fieldValue(endpoint, "QueueAge"))
			lateness := indirectValue(fieldValue(endpoint, "Lateness"))
			queueAgeP99 = max(queueAgeP99, durationField(queueAge, "P99"))
			queueAgeMax = max(queueAgeMax, durationField(queueAge, "Max"))
			latenessP99 = max(latenessP99, durationField(lateness, "P99"))
			latenessMax = max(latenessMax, durationField(lateness, "Max"))
			if stringField(endpoint, "Kind") == "interrupt-in" {
				hidQueueHighWater = max(hidQueueHighWater,
					intField(endpoint, "QueueHighWater"))
				hidOverflow += intField(endpoint, "Overflow")
				hidQueueAge = queueAge
				hidLateness = lateness
			}
		}
	}
	response := indirectValue(fieldValue(root, "Response"))
	responseQueue := indirectValue(fieldValue(response, "ResponseQueueWait"))
	sendLock := indirectValue(fieldValue(response, "SendLockWait"))
	socketWrite := indirectValue(fieldValue(response, "SocketWrite"))
	b.ReportMetric(1, "usb-diagnostics-snapshot")
	b.ReportMetric(float64(queueHighWater), "usb-queue-high-water")
	b.ReportMetric(float64(queueAgeP99)/float64(time.Microsecond), "usb-queue-age-p99-us")
	b.ReportMetric(float64(queueAgeMax)/float64(time.Microsecond), "usb-queue-age-max-us")
	b.ReportMetric(float64(latenessP99)/float64(time.Microsecond), "usb-lateness-p99-us")
	b.ReportMetric(float64(latenessMax)/float64(time.Microsecond), "usb-lateness-max-us")
	b.ReportMetric(float64(durationField(responseQueue, "P99"))/float64(time.Microsecond),
		"usb-response-queue-p99-us")
	b.ReportMetric(float64(durationField(sendLock, "P99"))/float64(time.Microsecond),
		"usb-send-lock-p99-us")
	b.ReportMetric(float64(durationField(sendLock, "Max"))/float64(time.Microsecond),
		"usb-send-lock-max-us")
	b.ReportMetric(float64(durationField(socketWrite, "P99"))/float64(time.Microsecond),
		"usb-socket-write-p99-us")
	b.ReportMetric(float64(hidQueueHighWater), "hid-interrupt-queue-high-water")
	b.ReportMetric(float64(hidOverflow), "hid-interrupt-overflows")
	reportReflectedDurationDistribution(b, "hid-interrupt-queue-age", hidQueueAge)
	reportReflectedDurationDistribution(b,
		"hid-interrupt-service-deadline-lateness", hidLateness)
}

func reportReflectedDurationDistribution(b *testing.B, prefix string,
	distribution reflect.Value) {
	b.Helper()
	b.ReportMetric(float64(uintField(distribution, "Count")), prefix+"-samples")
	b.ReportMetric(float64(durationField(distribution, "Median"))/float64(time.Microsecond),
		prefix+"-p50-us")
	b.ReportMetric(float64(durationField(distribution, "P95"))/float64(time.Microsecond),
		prefix+"-p95-us")
	b.ReportMetric(float64(durationField(distribution, "P99"))/float64(time.Microsecond),
		prefix+"-p99-us")
	b.ReportMetric(float64(durationField(distribution, "P999"))/float64(time.Microsecond),
		prefix+"-p99.9-us")
	b.ReportMetric(float64(durationField(distribution, "Max"))/float64(time.Microsecond),
		prefix+"-max-us")
}

func reportDualSenseInputDiagnostics(b *testing.B,
	response *viipertypes.DevicesListResponse, devID string) {
	b.Helper()
	if response == nil {
		return
	}
	for _, device := range response.Devices {
		if device.DevID != devID {
			continue
		}
		specific := device.DeviceSpecific
		b.ReportMetric(mapNumber(specific, "inputTransitionHighWater"),
			"input-transition-high-water")
		b.ReportMetric(mapNumber(specific, "inputTransitionOverflows"),
			"input-transition-overflows")
		latencies, _ := mapLookup(specific, "inputLatencyDistributions").(map[string]any)
		reportMappedDurationDistribution(b, "receive-to-selected",
			mapLookup(latencies, "ReceiveToSelected"))
		reportMappedDurationDistribution(b, "receive-to-presented",
			mapLookup(latencies, "ReceiveToPresented"))
		return
	}
	b.Logf("DualSense input diagnostics did not contain device %s", devID)
}

func reportMappedDurationDistribution(b *testing.B, prefix string, value any) {
	b.Helper()
	distribution, _ := value.(map[string]any)
	b.ReportMetric(mapNumber(distribution, "Count"), prefix+"-samples")
	for _, metric := range []struct {
		field string
		name  string
	}{
		{field: "P50", name: "p50"},
		{field: "P95", name: "p95"},
		{field: "P99", name: "p99"},
		{field: "P999", name: "p99.9"},
		{field: "Maximum", name: "max"},
	} {
		nanoseconds := mapNumber(distribution, metric.field)
		b.ReportMetric(nanoseconds/float64(time.Microsecond),
			prefix+"-"+metric.name+"-us")
	}
}

func mapLookup(values map[string]any, key string) any {
	if values == nil {
		return nil
	}
	if value, ok := values[key]; ok {
		return value
	}
	for candidate, value := range values {
		if strings.EqualFold(candidate, key) {
			return value
		}
	}
	return nil
}

func mapNumber(values map[string]any, key string) float64 {
	switch value := mapLookup(values, key).(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case uint64:
		return float64(value)
	case json.Number:
		number, _ := value.Float64()
		return number
	default:
		return 0
	}
}

func indirectValue(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	return value
}

func fieldValue(value reflect.Value, name string) reflect.Value {
	value = indirectValue(value)
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	return value.FieldByName(name)
}

func intField(value reflect.Value, name string) int {
	field := fieldValue(value, name)
	if !field.IsValid() {
		return 0
	}
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(field.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int(field.Uint())
	default:
		return 0
	}
}

func uintField(value reflect.Value, name string) uint64 {
	field := fieldValue(value, name)
	if !field.IsValid() {
		return 0
	}
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if field.Int() > 0 {
			return uint64(field.Int())
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return field.Uint()
	}
	return 0
}

func stringField(value reflect.Value, name string) string {
	field := fieldValue(value, name)
	if field.IsValid() && field.Kind() == reflect.String {
		return field.String()
	}
	return ""
}

func durationField(value reflect.Value, name string) time.Duration {
	field := fieldValue(value, name)
	if field.IsValid() && field.Kind() == reflect.Int64 {
		return time.Duration(field.Int())
	}
	return 0
}

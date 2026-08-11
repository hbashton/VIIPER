//go:build windows

package usb_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	"github.com/Alia5/VIIPER/device/ns2pro"
	"github.com/Alia5/VIIPER/device/xbox360"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
	usbdevice "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"golang.org/x/sys/windows"
)

const (
	liveNativeTestEnvironment = "VIIPER_UDE_LIVE"
	liveNativeTestIterations  = "VIIPER_UDE_LIVE_ITERATIONS"
	liveNativeCrashChild      = "VIIPER_UDE_LIVE_CRASH_CHILD"
	liveNativeMediaProbe      = "VIIPER_UDE_LIVE_MEDIA_PROBE"
	liveNativeMediaSeconds    = "VIIPER_UDE_LIVE_MEDIA_SECONDS"
	liveNativeInputProbe      = "VIIPER_UDE_LIVE_INPUT_PROBE"
	liveNativeRestartInstance = "VIIPER_UDE_LIVE_RESTART_INSTANCE_ID"
	liveNativeCrashExitCode   = 86
)

type liveNativeController struct {
	name              string
	vendorID          uint16
	productID         uint16
	inputMarkerOffset uint16
	feedbackProbeKind string
	feedbackReportLen uint64
	armFeedbackProbe  func(usbdevice.Device) (func(context.Context) error, error)
	new               func() (usbdevice.Device, func(uint64), func(byte), error)
}

// liveNativeMediaWitness observes the controller-engine side of the same
// CoreAudio stream exercised by ViiperUdeMediaProbe. CoreAudio frame counts and
// kernel byte counters alone can both advance while a broken broker returns
// silence or drops the payload before it reaches the preserved PlayStation
// media logic. The witness therefore proves non-silent render data arrives at
// that existing logic and feeds deterministic non-silent microphone PCM back
// through the virtual USB endpoint. It does not alter media construction.
type liveNativeMediaWitness struct {
	speakerBytes        atomic.Uint64
	speakerNonZeroBytes atomic.Uint64
	hapticsGenerations  atomic.Uint64
	hapticsNonSilent    atomic.Uint64
	queueMicrophone     func([]byte)
	microphoneFrame     []byte
	speakerBytesPerSec  uint64
	requireHaptics      bool
}

func countNonZeroBytes(data []byte) uint64 {
	var count uint64
	for _, value := range data {
		if value != 0 {
			count++
		}
	}
	return count
}

func nonZeroPCMFrame(size int) []byte {
	frame := make([]byte, size)
	for offset := 0; offset+1 < len(frame); offset += 2 {
		frame[offset] = 0x34
		frame[offset+1] = 0x12
	}
	return frame
}

func armLiveNativeMediaWitness(dev usbdevice.Device) (*liveNativeMediaWitness, error) {
	switch controller := dev.(type) {
	case *dualshock4.DualShock4:
		witness := &liveNativeMediaWitness{
			queueMicrophone: controller.QueueMicrophonePCMFrame,
			microphoneFrame: nonZeroPCMFrame(dualshock4.USBMicrophoneClientFrameSize),
			speakerBytesPerSec: dualshock4.USBSpeakerSampleRate *
				dualshock4.USBSpeakerChannels * dualshock4.USBSpeakerBytesPerSample,
		}
		controller.SetSpeakerCallback(func(pcm []byte) {
			witness.speakerBytes.Add(uint64(len(pcm)))
			witness.speakerNonZeroBytes.Add(countNonZeroBytes(pcm))
		})
		return witness, nil
	case *dualsense.DualSense:
		witness := &liveNativeMediaWitness{
			queueMicrophone: controller.QueueMicrophonePCMFrame,
			microphoneFrame: nonZeroPCMFrame(dualsense.USBMicrophoneClientFrameSize),
			speakerBytesPerSec: dualsense.USBHapticsAudioSampleRate * 2 *
				dualsense.USBHapticsAudioBytesPerSample,
			requireHaptics: true,
		}
		controller.SetAtomicAudioHapticsCallback(func(_ dualsense.OutputState, speaker []byte) {
			witness.speakerBytes.Add(uint64(len(speaker)))
			witness.speakerNonZeroBytes.Add(countNonZeroBytes(speaker))
		})
		controller.SetRealtimeHapticsCallback(func(feedback dualsense.OutputState) {
			witness.hapticsGenerations.Add(1)
			sample := feedback.BluetoothCombinedOutputReport[dualsense.BluetoothCombinedHapticsOffset:(dualsense.BluetoothCombinedHapticsOffset + dualsense.BluetoothHapticsSampleSize)]
			if countNonZeroBytes(sample) != 0 {
				witness.hapticsNonSilent.Add(1)
			}
		})
		return witness, nil
	default:
		return nil, fmt.Errorf("media witness does not support %T", dev)
	}
}

func (w *liveNativeMediaWitness) startMicrophone(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Queueing slightly ahead of the 10 ms client-frame cadence makes the
		// content assertion independent of the instant CoreAudio selects alt 1;
		// the controller's existing bounded/adaptive microphone queue remains
		// responsible for presentation cadence.
		ticker := time.NewTicker(8 * time.Millisecond)
		defer ticker.Stop()
		for {
			w.queueMicrophone(w.microphoneFrame)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

func (w *liveNativeMediaWitness) validate(duration time.Duration) error {
	seconds := uint64(duration / time.Second)
	minimumSpeakerBytes := w.speakerBytesPerSec * seconds * 9 / 10
	speakerBytes := w.speakerBytes.Load()
	if speakerBytes < minimumSpeakerBytes {
		return fmt.Errorf("controller engine received only %d speaker bytes; want at least %d",
			speakerBytes, minimumSpeakerBytes)
	}
	if nonZero := w.speakerNonZeroBytes.Load(); nonZero < speakerBytes/4 {
		return fmt.Errorf("controller engine speaker payload was silent or malformed: nonzero=%d total=%d",
			nonZero, speakerBytes)
	}
	if w.requireHaptics {
		minimumHaptics := seconds * 50
		haptics := w.hapticsGenerations.Load()
		if haptics < minimumHaptics {
			return fmt.Errorf("controller engine received only %d realtime haptics generations; want at least %d",
				haptics, minimumHaptics)
		}
		if nonSilent := w.hapticsNonSilent.Load(); nonSilent < haptics/2 {
			return fmt.Errorf("controller engine haptics payload was silent or malformed: nonSilent=%d total=%d",
				nonSilent, haptics)
		}
	}
	return nil
}

func TestLiveNativeMediaWitnessRejectsSilentOrIncompleteContent(t *testing.T) {
	valid := &liveNativeMediaWitness{speakerBytesPerSec: 100, requireHaptics: true}
	valid.speakerBytes.Store(100)
	valid.speakerNonZeroBytes.Store(30)
	valid.hapticsGenerations.Store(50)
	valid.hapticsNonSilent.Store(25)
	if err := valid.validate(time.Second); err != nil {
		t.Fatalf("complete non-silent media was rejected: %v", err)
	}

	for _, testCase := range []struct {
		name    string
		prepare func(*liveNativeMediaWitness)
	}{
		{name: "short speaker stream", prepare: func(w *liveNativeMediaWitness) {
			w.speakerBytes.Store(89)
			w.speakerNonZeroBytes.Store(89)
			w.hapticsGenerations.Store(50)
			w.hapticsNonSilent.Store(50)
		}},
		{name: "silent speaker stream", prepare: func(w *liveNativeMediaWitness) {
			w.speakerBytes.Store(100)
			w.speakerNonZeroBytes.Store(24)
			w.hapticsGenerations.Store(50)
			w.hapticsNonSilent.Store(50)
		}},
		{name: "missing realtime haptics", prepare: func(w *liveNativeMediaWitness) {
			w.speakerBytes.Store(100)
			w.speakerNonZeroBytes.Store(100)
			w.hapticsGenerations.Store(49)
			w.hapticsNonSilent.Store(49)
		}},
		{name: "silent realtime haptics", prepare: func(w *liveNativeMediaWitness) {
			w.speakerBytes.Store(100)
			w.speakerNonZeroBytes.Store(100)
			w.hapticsGenerations.Store(50)
			w.hapticsNonSilent.Store(24)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			witness := &liveNativeMediaWitness{speakerBytesPerSec: 100, requireHaptics: true}
			testCase.prepare(witness)
			if err := witness.validate(time.Second); err == nil {
				t.Fatal("incomplete or silent media was accepted")
			}
		})
	}
}

func armDualShock4FeedbackProbe(dev usbdevice.Device) (func(context.Context) error, error) {
	controller, ok := dev.(*dualshock4.DualShock4)
	if !ok {
		return nil, fmt.Errorf("feedback probe expected *dualshock4.DualShock4, got %T", dev)
	}
	want := dualshock4.OutputState{
		RumbleSmall: 0x23, RumbleLarge: 0xA7,
		LedRed: 0x11, LedGreen: 0x52, LedBlue: 0xC3,
		FlashOn: 0x04, FlashOff: 0x09,
	}
	done := make(chan struct{})
	var matched sync.Once
	controller.SetOutputCallback(func(got dualshock4.OutputState) {
		if got == want {
			matched.Do(func() { close(done) })
		}
	})
	return func(ctx context.Context) error {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("DualShock 4 feedback marker did not reach the device engine: %w", ctx.Err())
		}
	}, nil
}

func armDualSenseFeedbackProbe(dev usbdevice.Device) (func(context.Context) error, error) {
	controller, ok := dev.(*dualsense.DualSense)
	if !ok {
		return nil, fmt.Errorf("feedback probe expected *dualsense.DualSense, got %T", dev)
	}
	var want [dualsense.OutputReportSize]byte
	want[0] = dualsense.ReportIDOutput
	want[1] = 0x0F
	want[2] = 0x14
	want[3] = 0x22
	want[4] = 0x88
	want[11] = 0x21
	want[12] = 0xFC
	want[13] = 0x03
	want[20] = 0x44
	want[22] = 0x25
	want[23] = 0x40
	want[24] = 0x05
	want[31] = 0x55
	want[44] = 0x24
	want[45] = 0x11
	want[46] = 0x52
	want[47] = 0xC3

	done := make(chan struct{})
	var matched sync.Once
	controller.SetOutputCallback(func(got dualsense.OutputState) {
		if got.RawOutputReport == want &&
			got.RumbleSmall == 0x22 && got.RumbleLarge == 0x88 &&
			got.LedRed == 0x11 && got.LedGreen == 0x52 && got.LedBlue == 0xC3 &&
			got.PlayerLeds == 0x24 &&
			got.TriggerR2Mode == 0x21 && got.TriggerR2StartResistance == 0xFC &&
			got.TriggerR2EffectForce == 0x03 && got.TriggerR2Frequency == 0x44 &&
			got.TriggerL2Mode == 0x25 && got.TriggerL2StartResistance == 0x40 &&
			got.TriggerL2EffectForce == 0x05 && got.TriggerL2Frequency == 0x55 {
			matched.Do(func() { close(done) })
		}
	})
	return func(ctx context.Context) error {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("DualSense feedback marker did not reach the device engine: %w", ctx.Err())
		}
	}, nil
}

func liveNativeControllers() []liveNativeController {
	return []liveNativeController{
		{name: "Xbox360", new: func() (usbdevice.Device, func(uint64), func(byte), error) {
			dev, err := xbox360.New(nil)
			return dev, func(sequence uint64) {
				state := xbox360.NewInputState()
				state.LX = int16(sequence % 1024)
				dev.UpdateInputState(*state)
			}, nil, err
		}},
		{name: "DualShock4", vendorID: dualshock4.DefaultVID,
			productID: dualshock4.DefaultPID, inputMarkerOffset: 1,
			feedbackProbeKind: "dualshock4", feedbackReportLen: 32,
			armFeedbackProbe: armDualShock4FeedbackProbe,
			new: func() (usbdevice.Device, func(uint64), func(byte), error) {
				dev, err := dualshock4.New(nil)
				return dev, func(sequence uint64) {
						state := dualshock4.NewInputState()
						state.LX = int8(sequence % 32)
						dev.UpdateInputState(state)
					}, func(marker byte) {
						state := dualshock4.NewInputState()
						state.LX = int8(int(marker) - 128)
						dev.UpdateInputState(state)
					}, err
			}},
		{name: "DualSense", vendorID: dualsense.DefaultVID,
			productID: dualsense.DefaultPIDDS, inputMarkerOffset: 1,
			feedbackProbeKind: "dualsense", feedbackReportLen: dualsense.OutputReportSize,
			armFeedbackProbe: armDualSenseFeedbackProbe,
			new: func() (usbdevice.Device, func(uint64), func(byte), error) {
				dev, err := dualsense.New(nil)
				return dev, func(sequence uint64) {
						state := dualsense.NewInputState()
						state.LX = int8(sequence % 32)
						dev.UpdateInputState(state)
					}, func(marker byte) {
						state := dualsense.NewInputState()
						state.LX = int8(int(marker) - 128)
						dev.UpdateInputState(state)
					}, err
			}},
		{name: "DualSenseEdge", vendorID: dualsense.DefaultVID,
			productID: dualsense.DefaultPIDDSEdge, inputMarkerOffset: 3,
			feedbackProbeKind: "dualsense-edge", feedbackReportLen: dualsense.OutputReportSize,
			armFeedbackProbe: armDualSenseFeedbackProbe,
			new: func() (usbdevice.Device, func(uint64), func(byte), error) {
				dev, err := dualsense.NewEdge(nil)
				return dev, func(sequence uint64) {
						state := dualsense.NewInputState()
						state.RX = int8(sequence % 32)
						dev.UpdateInputState(state)
					}, func(marker byte) {
						state := dualsense.NewInputState()
						state.RX = int8(int(marker) - 128)
						dev.UpdateInputState(state)
					}, err
			}},
		{name: "Switch2Pro", new: func() (usbdevice.Device, func(uint64), func(byte), error) {
			dev, err := ns2pro.New(nil)
			return dev, func(sequence uint64) {
				state := ns2pro.NewInputState()
				state.LX += uint16(sequence % 32)
				dev.UpdateInputState(*state)
			}, nil, err
		}},
	}
}

func TestNativeLiveFeedbackProbeContracts(t *testing.T) {
	for _, controller := range liveNativeControllers()[1:4] {
		controller := controller
		t.Run(controller.name, func(t *testing.T) {
			dev, _, _, err := controller.new()
			if err != nil {
				t.Fatal(err)
			}
			waitForFeedback, err := controller.armFeedbackProbe(dev)
			if err != nil {
				t.Fatal(err)
			}

			var endpoint uint8
			var report []byte
			switch controller.feedbackProbeKind {
			case "dualshock4":
				endpoint = dualshock4.EndpointOut
				report = make([]byte, 32)
				report[0] = dualshock4.ReportIDOutput
				report[4], report[5] = 0x23, 0xA7
				report[6], report[7], report[8] = 0x11, 0x52, 0xC3
				report[9], report[10] = 0x04, 0x09
			case "dualsense", "dualsense-edge":
				endpoint = dualsense.EndpointOut
				report = make([]byte, dualsense.OutputReportSize)
				report[0], report[1], report[2] = dualsense.ReportIDOutput, 0x0F, 0x14
				report[3], report[4] = 0x22, 0x88
				report[11], report[12], report[13], report[20] = 0x21, 0xFC, 0x03, 0x44
				report[22], report[23], report[24], report[31] = 0x25, 0x40, 0x05, 0x55
				report[44], report[45], report[46], report[47] = 0x24, 0x11, 0x52, 0xC3
			default:
				t.Fatalf("unsupported feedback probe kind %q", controller.feedbackProbeKind)
			}

			dev.HandleTransfer(context.Background(), uint32(endpoint), usbip.DirOut, report)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err = waitForFeedback(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func liveNativeIterationCount(t *testing.T) int {
	t.Helper()
	raw := os.Getenv(liveNativeTestIterations)
	if raw == "" {
		return 1
	}
	iterations, err := strconv.Atoi(raw)
	if err != nil || iterations < 1 || iterations > 100 {
		t.Fatalf("%s must be an integer from 1 through 100, got %q",
			liveNativeTestIterations, raw)
	}
	return iterations
}

func liveNativeMediaDuration(t *testing.T) time.Duration {
	t.Helper()
	raw := os.Getenv(liveNativeMediaSeconds)
	if raw == "" {
		return 3 * time.Second
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 1 || seconds > 300 {
		t.Fatalf("%s must be an integer from 1 through 300, got %q",
			liveNativeMediaSeconds, raw)
	}
	return time.Duration(seconds) * time.Second
}

func TestNativeLiveMediaDurationContract(t *testing.T) {
	t.Setenv(liveNativeMediaSeconds, "")
	if got := liveNativeMediaDuration(t); got != 3*time.Second {
		t.Fatalf("default native media duration=%s want 3s", got)
	}
	t.Setenv(liveNativeMediaSeconds, "180")
	if got := liveNativeMediaDuration(t); got != 3*time.Minute {
		t.Fatalf("release native media duration=%s want 3m", got)
	}
}

func waitForNativeStats(ctx context.Context, client *udecx.Client, description string,
	accept func(udecx.Stats) bool) (udecx.Stats, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var last udecx.Stats
	for {
		stats, err := client.QueryStats(ctx)
		if err != nil {
			return last, fmt.Errorf("query stats while waiting for %s: %w", description, err)
		}
		last = stats
		if accept(stats) {
			return stats, nil
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("wait for %s: %w (last stats: %+v)",
				description, ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

func assertCleanNativeStatsDelta(t *testing.T, before, after udecx.Stats) {
	t.Helper()
	if after.InvalidMessages != before.InvalidMessages ||
		after.QueueExhaustions != before.QueueExhaustions ||
		after.NotificationEventOverflows != before.NotificationEventOverflows ||
		after.LateCompletions != before.LateCompletions ||
		after.CleanupRetries != before.CleanupRetries {
		t.Fatalf("native driver recorded a protocol/lifecycle fault: before=%+v after=%+v",
			before, after)
	}
}

func runLiveMediaProbe(t *testing.T, ctx context.Context, probe string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(ctx, probe, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run native CoreAudio probe %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}

type liveProbeResult struct {
	output string
	err    error
}

func startLiveProbe(ctx context.Context, probe string, arguments ...string) <-chan liveProbeResult {
	done := make(chan liveProbeResult, 1)
	go func() {
		output, err := exec.CommandContext(ctx, probe, arguments...).CombinedOutput()
		done <- liveProbeResult{output: string(output), err: err}
	}()
	return done
}

var queryPerformanceCounter = windows.NewLazySystemDLL("kernel32.dll").
	NewProc("QueryPerformanceCounter")

func performanceCounter(t *testing.T) int64 {
	t.Helper()
	var counter int64
	result, _, callErr := queryPerformanceCounter.Call(
		uintptr(unsafe.Pointer(&counter)))
	if result == 0 {
		t.Fatalf("QueryPerformanceCounter: %v", callErr)
	}
	return counter
}

func percentile(sorted []float64, percentile float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted)-1) * percentile)
	return sorted[index]
}

func runLiveInputLatencyProbe(
	t *testing.T,
	ctx context.Context,
	probe string,
	snapshot string,
	controller liveNativeController,
	publishMarker func(byte),
) {
	t.Helper()
	const samples = 256
	probeCtx, cancelProbe := context.WithTimeout(ctx, 45*time.Second)
	defer cancelProbe()
	command := exec.CommandContext(probeCtx, probe,
		"measure", snapshot,
		fmt.Sprintf("0x%04X", controller.vendorID),
		fmt.Sprintf("0x%04X", controller.productID),
		strconv.Itoa(int(controller.inputMarkerOffset)),
		strconv.Itoa(samples), "qpc-v1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("open %s input-probe stdout: %v", controller.name, err)
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	if err = command.Start(); err != nil {
		t.Fatalf("start %s input probe: %v", controller.name, err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Wait()
		}
	}()

	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		_ = command.Wait()
		waited = true
		t.Fatalf("%s input probe never became ready: scan=%v stderr=%s",
			controller.name, scanner.Err(), stderr.String())
	}
	ready := strings.Fields(scanner.Text())
	if len(ready) < 3 || ready[0] != "READY" {
		t.Fatalf("%s input probe returned an invalid ready record: %q",
			controller.name, scanner.Text())
	}
	frequency, err := strconv.ParseInt(ready[1], 10, 64)
	if err != nil || frequency <= 0 {
		t.Fatalf("%s input probe returned invalid QPC frequency %q",
			controller.name, ready[1])
	}

	latencies := make([]float64, 0, samples)
	for index := 0; index < samples; index++ {
		marker := byte(0xFD + (index & 1))
		published := performanceCounter(t)
		publishMarker(marker)
		if !scanner.Scan() {
			_ = command.Wait()
			waited = true
			t.Fatalf("%s input probe ended after %d/%d samples: scan=%v stderr=%s",
				controller.name, index, samples, scanner.Err(), stderr.String())
		}
		match := strings.Fields(scanner.Text())
		if len(match) != 3 || match[0] != "MATCH" {
			t.Fatalf("%s input probe returned an invalid match record: %q",
				controller.name, scanner.Text())
		}
		observedMarker, markerErr := strconv.ParseUint(match[1], 10, 8)
		observed, observedErr := strconv.ParseInt(match[2], 10, 64)
		if markerErr != nil || observedErr != nil || byte(observedMarker) != marker ||
			observed < published {
			t.Fatalf("%s input probe returned an invalid marker/timestamp: %q published=%d",
				controller.name, scanner.Text(), published)
		}
		latencies = append(latencies,
			float64(observed-published)*1000/float64(frequency))
	}
	if err = command.Wait(); err != nil {
		waited = true
		t.Fatalf("%s input probe failed: %v stderr=%s",
			controller.name, err, stderr.String())
	}
	waited = true
	sort.Float64s(latencies)
	p50 := percentile(latencies, 0.50)
	p95 := percentile(latencies, 0.95)
	p99 := percentile(latencies, 0.99)
	maximum := latencies[len(latencies)-1]
	t.Logf("%s native publish-to-HID latency: samples=%d p50=%.3fms p95=%.3fms p99=%.3fms max=%.3fms",
		controller.name, samples, p50, p95, p99, maximum)
	// These limits include the Go publisher, native IOCTL, UdeCx/HIDClass, and
	// the independent observer process. They deliberately gate long-tail loss
	// without pretending the host's nominal poll interval is end-to-end latency.
	if p95 > 4 || p99 > 8 || maximum > 20 {
		t.Fatalf("%s native input latency exceeded the release gate: p95=%.3fms p99=%.3fms max=%.3fms",
			controller.name, p95, p99, maximum)
	}
}

// TestNativeUDELiveProductionControllers is deliberately inert in normal CI.
// It opens an already-installed native controller and never installs, updates,
// enables, or removes a kernel driver. Release validation must first verify the
// package's Microsoft kernel-policy signature, then opt in on a disposable test
// machine with VIIPER_UDE_LIVE=1.
func TestNativeUDELiveProductionControllers(t *testing.T) {
	if os.Getenv(liveNativeTestEnvironment) != "1" {
		t.Skipf("set %s=1 after installing a verified Microsoft-signed native UDE package",
			liveNativeTestEnvironment)
	}

	iterations := liveNativeIterationCount(t)
	mediaDuration := liveNativeMediaDuration(t)
	testCtx, cancelTest := context.WithTimeout(context.Background(),
		time.Duration(iterations)*5*time.Minute+3*mediaDuration+2*time.Minute)
	defer cancelTest()

	client, err := udecx.Open(testCtx)
	if err != nil {
		t.Fatalf("open native UDE controller: %v", err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("close native UDE controller: %v", closeErr)
		}
	}()

	baseline, err := client.QueryStats(testCtx)
	if err != nil {
		t.Fatalf("query native UDE baseline: %v", err)
	}
	if baseline.ActiveDevices != 0 || baseline.PendingOperations != 0 {
		t.Fatalf("refusing a dirty native UDE session: %+v", baseline)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := serverusb.New(serverusb.ServerConfig{ConnectionTimeout: 5 * time.Second}, logger, nil)
	processor, err := serverusb.NewNativeProcessor(server)
	if err != nil {
		t.Fatal(err)
	}
	host, err := udecx.NewHost(client, processor, 0)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancelServe := context.WithCancel(testCtx)
	serveDone := make(chan error, 1)
	go func() { serveDone <- host.Serve(serveCtx) }()
	defer func() {
		cancelServe()
		host.Close()
		select {
		case serveErr := <-serveDone:
			if serveErr != nil {
				t.Errorf("native UDE host shutdown: %v", serveErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("native UDE host did not stop within 5 seconds")
		}
	}()

	const deviceIDBase uint64 = 0x5649495000000000
	mediaProbe := os.Getenv(liveNativeMediaProbe)
	inputProbe := os.Getenv(liveNativeInputProbe)
	for iteration := 1; iteration <= iterations; iteration++ {
		for controllerIndex, controller := range liveNativeControllers() {
			controller := controller
			t.Run(fmt.Sprintf("%s/generation-%d", controller.name, iteration), func(t *testing.T) {
				deviceID := deviceIDBase + uint64(controllerIndex+1)
				dev, publishInput, publishMarker, createErr := controller.new()
				if createErr != nil {
					t.Fatalf("construct %s: %v", controller.name, createErr)
				}
				feedbackController := iteration == 1 && inputProbe != "" &&
					controller.armFeedbackProbe != nil
				var waitForFeedback func(context.Context) error
				if feedbackController {
					waitForFeedback, createErr = controller.armFeedbackProbe(dev)
					if createErr != nil {
						t.Fatalf("arm %s HID feedback probe: %v", controller.name, createErr)
					}
				}
				mediaSnapshot := ""
				mediaController := iteration == 1 && mediaProbe != "" &&
					(controller.name == "DualShock4" || controller.name == "DualSense" ||
						controller.name == "DualSenseEdge")
				var mediaWitness *liveNativeMediaWitness
				if mediaController {
					mediaWitness, createErr = armLiveNativeMediaWitness(dev)
					if createErr != nil {
						t.Fatalf("arm %s media witness: %v", controller.name, createErr)
					}
					snapshot, snapshotErr := os.CreateTemp("", "viiper-ude-media-*.snapshot")
					if snapshotErr != nil {
						t.Fatalf("create media endpoint snapshot: %v", snapshotErr)
					}
					mediaSnapshot = snapshot.Name()
					if closeErr := snapshot.Close(); closeErr != nil {
						t.Fatalf("close media endpoint snapshot: %v", closeErr)
					}
					defer os.Remove(mediaSnapshot)
					runLiveMediaProbe(t, testCtx, mediaProbe, "snapshot", mediaSnapshot)
				}
				inputSnapshot := ""
				inputController := iteration == 1 && inputProbe != "" && publishMarker != nil
				if inputController || feedbackController {
					snapshot, snapshotErr := os.CreateTemp("", "viiper-ude-input-*.snapshot")
					if snapshotErr != nil {
						t.Fatalf("create input endpoint snapshot: %v", snapshotErr)
					}
					inputSnapshot = snapshot.Name()
					if closeErr := snapshot.Close(); closeErr != nil {
						t.Fatalf("close input endpoint snapshot: %v", closeErr)
					}
					defer os.Remove(inputSnapshot)
					runLiveMediaProbe(t, testCtx, inputProbe, "snapshot", inputSnapshot)
				}
				before, queryErr := client.QueryStats(testCtx)
				if queryErr != nil {
					t.Fatal(queryErr)
				}
				identity, registerErr := host.Register(testCtx, deviceID, dev)
				if registerErr != nil {
					t.Fatalf("register %s: %v", controller.name, registerErr)
				}
				if identity.Generation != uint32(iteration) {
					t.Fatalf("%s generation=%d want %d", controller.name,
						identity.Generation, iteration)
				}
				registered := true
				defer func() {
					if registered {
						cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
						defer cleanupCancel()
						if unregisterErr := host.Unregister(cleanupCtx, identity); unregisterErr != nil {
							t.Errorf("cleanup %s: %v", controller.name, unregisterErr)
						}
					}
				}()

				enumerateCtx, cancelEnumerate := context.WithTimeout(testCtx, 20*time.Second)
				_, waitErr := waitForNativeStats(enumerateCtx, client,
					controller.name+" enumeration", func(stats udecx.Stats) bool {
						return stats.ActiveDevices == 1
					})
				cancelEnumerate()
				if waitErr != nil {
					t.Fatal(waitErr)
				}

				feedbackVerified := false
				verifyFeedback := func() {
					feedbackBefore, feedbackErr := client.QueryStats(testCtx)
					if feedbackErr != nil {
						t.Fatalf("query %s feedback baseline: %v", controller.name, feedbackErr)
					}
					probeOutput := runLiveMediaProbe(t, testCtx, inputProbe,
						"feedback", inputSnapshot,
						fmt.Sprintf("0x%04X", controller.vendorID),
						fmt.Sprintf("0x%04X", controller.productID),
						controller.feedbackProbeKind, "hid-output-v1")
					feedbackCtx, cancelFeedback := context.WithTimeout(testCtx, 10*time.Second)
					defer cancelFeedback()
					if feedbackErr = waitForFeedback(feedbackCtx); feedbackErr != nil {
						t.Fatalf("%s HID output was not preserved end to end: %v; probe=%s",
							controller.name, feedbackErr, probeOutput)
					}
					feedbackAfter, feedbackWaitErr := waitForNativeStats(feedbackCtx, client,
						controller.name+" HID output completion", func(stats udecx.Stats) bool {
							return stats.OperationsDequeued > feedbackBefore.OperationsDequeued &&
								stats.OperationsCompleted > feedbackBefore.OperationsCompleted &&
								stats.BytesToDevice >= feedbackBefore.BytesToDevice+controller.feedbackReportLen
						})
					if feedbackWaitErr != nil {
						t.Fatalf("%s HID output did not complete through the native driver: %v; before=%+v after=%+v probe=%s",
							controller.name, feedbackWaitErr, feedbackBefore, feedbackAfter, probeOutput)
					}
					feedbackVerified = true
				}

				if mediaController {
					mediaBefore, mediaErr := client.QueryStats(testCtx)
					if mediaErr != nil {
						t.Fatalf("query %s media baseline: %v", controller.name, mediaErr)
					}
					mediaCtx, cancelMedia := context.WithCancel(testCtx)
					defer cancelMedia()
					microphoneDone := mediaWitness.startMicrophone(mediaCtx)
					probeDone := startLiveProbe(
						mediaCtx, mediaProbe, "exercise", mediaSnapshot,
						strconv.Itoa(int(mediaDuration/time.Second)),
						strings.ToLower(controller.name))
					stressCtx, cancelStress := context.WithCancel(testCtx)
					defer cancelStress()
					stressDone := make(chan struct{})
					go func() {
						defer close(stressDone)
						for sequence := uint64(1); ; sequence++ {
							select {
							case <-stressCtx.Done():
								return
							default:
								publishInput(sequence)
								time.Sleep(time.Millisecond)
							}
						}
					}()
					if feedbackController {
						verifyFeedback()
					}
					probeResult := <-probeDone
					cancelMedia()
					<-microphoneDone
					cancelStress()
					<-stressDone
					if probeResult.err != nil {
						t.Fatalf("run native CoreAudio probe: %v\n%s",
							probeResult.err, probeResult.output)
					}
					if witnessErr := mediaWitness.validate(mediaDuration); witnessErr != nil {
						t.Fatalf("%s media content did not survive the native bus: %v; probe=%s",
							controller.name, witnessErr, probeResult.output)
					}
					mediaAfter, mediaErr := client.QueryStats(testCtx)
					if mediaErr != nil {
						t.Fatalf("query %s media result: %v", controller.name, mediaErr)
					}
					if mediaAfter.IsoPackets <= mediaBefore.IsoPackets ||
						mediaAfter.BytesToDevice <= mediaBefore.BytesToDevice ||
						mediaAfter.BytesFromDevice <= mediaBefore.BytesFromDevice {
						t.Fatalf("%s CoreAudio did not exercise full-duplex ISO media: before=%+v after=%+v probe=%s",
							controller.name, mediaBefore, mediaAfter, probeResult.output)
					}
				}
				if inputController {
					runLiveInputLatencyProbe(
						t, testCtx, inputProbe, inputSnapshot, controller, publishMarker)
				}
				if feedbackController && !feedbackVerified {
					verifyFeedback()
				}

				inputDeadline := time.Now().Add(750 * time.Millisecond)
				for sequence := uint64(1); time.Now().Before(inputDeadline); sequence++ {
					publishInput(sequence)
					time.Sleep(time.Millisecond)
				}
				inputCtx, cancelInput := context.WithTimeout(testCtx, 20*time.Second)
				inputStats, waitErr := waitForNativeStats(inputCtx, client,
					controller.name+" direct interrupt input", func(stats udecx.Stats) bool {
						return stats.InputReportsCompleted > before.InputReportsCompleted
					})
				cancelInput()
				if waitErr != nil {
					t.Fatal(waitErr)
				}
				if inputStats.InputReportsSubmitted <= before.InputReportsSubmitted {
					t.Fatalf("%s did not publish a direct input state: before=%+v after=%+v",
						controller.name, before, inputStats)
				}

				unregisterCtx, cancelUnregister := context.WithTimeout(testCtx, 20*time.Second)
				if unregisterErr := host.Unregister(unregisterCtx, identity); unregisterErr != nil {
					cancelUnregister()
					t.Fatalf("unregister %s: %v", controller.name, unregisterErr)
				}
				cancelUnregister()
				registered = false

				teardownCtx, cancelTeardown := context.WithTimeout(testCtx, 20*time.Second)
				after, waitErr := waitForNativeStats(teardownCtx, client,
					controller.name+" teardown", func(stats udecx.Stats) bool {
						return stats.ActiveDevices == 0 && stats.PendingOperations == 0
					})
				cancelTeardown()
				if waitErr != nil {
					t.Fatal(waitErr)
				}
				assertCleanNativeStatsDelta(t, before, after)
			})
		}
	}

	t.Run("ConcurrentProductionSet", func(t *testing.T) {
		type activeController struct {
			name         string
			identity     udecx.DeviceIdentity
			publishInput func(uint64)
			err          error
		}
		controllers := liveNativeControllers()
		before, queryErr := client.QueryStats(testCtx)
		if queryErr != nil {
			t.Fatal(queryErr)
		}

		registered := make(chan activeController, len(controllers))
		var registerWG sync.WaitGroup
		for controllerIndex, controller := range controllers {
			controllerIndex, controller := controllerIndex, controller
			registerWG.Add(1)
			go func() {
				defer registerWG.Done()
				dev, publishInput, _, createErr := controller.new()
				if createErr != nil {
					registered <- activeController{name: controller.name, err: createErr}
					return
				}
				identity, registerErr := host.Register(
					testCtx, deviceIDBase+uint64(controllerIndex+1), dev)
				registered <- activeController{
					name: controller.name, identity: identity,
					publishInput: publishInput, err: registerErr,
				}
			}()
		}
		registerWG.Wait()
		close(registered)
		active := make([]activeController, 0, len(controllers))
		var registerErrors []error
		for result := range registered {
			if result.err != nil {
				registerErrors = append(registerErrors,
					fmt.Errorf("register %s: %w", result.name, result.err))
				continue
			}
			if result.identity.Generation != uint32(iterations+1) {
				registerErrors = append(registerErrors, fmt.Errorf(
					"%s concurrent generation=%d want %d", result.name,
					result.identity.Generation, iterations+1))
			}
			active = append(active, result)
		}
		defer func() {
			for _, controller := range active {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
				_ = host.Unregister(cleanupCtx, controller.identity)
				cleanupCancel()
			}
		}()
		if len(registerErrors) != 0 {
			t.Fatalf("concurrent registration errors: %v", registerErrors)
		}

		enumerateCtx, cancelEnumerate := context.WithTimeout(testCtx, 30*time.Second)
		_, waitErr := waitForNativeStats(enumerateCtx, client,
			"concurrent production enumeration", func(stats udecx.Stats) bool {
				return stats.ActiveDevices == uint32(len(controllers))
			})
		cancelEnumerate()
		if waitErr != nil {
			t.Fatal(waitErr)
		}

		var publishWG sync.WaitGroup
		for _, controller := range active {
			controller := controller
			publishWG.Add(1)
			go func() {
				defer publishWG.Done()
				deadline := time.Now().Add(2 * time.Second)
				for sequence := uint64(1); time.Now().Before(deadline); sequence++ {
					controller.publishInput(sequence)
					time.Sleep(time.Millisecond)
				}
			}()
		}
		publishWG.Wait()
		inputCtx, cancelInput := context.WithTimeout(testCtx, 20*time.Second)
		_, waitErr = waitForNativeStats(inputCtx, client,
			"concurrent direct interrupt input", func(stats udecx.Stats) bool {
				return stats.InputReportsCompleted >=
					before.InputReportsCompleted+uint64(len(controllers))
			})
		cancelInput()
		if waitErr != nil {
			t.Fatal(waitErr)
		}

		unregistered := make(chan error, len(active))
		var unregisterWG sync.WaitGroup
		for _, controller := range active {
			controller := controller
			unregisterWG.Add(1)
			go func() {
				defer unregisterWG.Done()
				unregisterCtx, cancelUnregister := context.WithTimeout(testCtx, 30*time.Second)
				defer cancelUnregister()
				unregistered <- host.Unregister(unregisterCtx, controller.identity)
			}()
		}
		unregisterWG.Wait()
		close(unregistered)
		var unregisterErrors []error
		for unregisterErr := range unregistered {
			if unregisterErr != nil {
				unregisterErrors = append(unregisterErrors, unregisterErr)
			}
		}
		if len(unregisterErrors) != 0 {
			t.Fatalf("concurrent unregister errors: %v", unregisterErrors)
		}
		active = nil

		teardownCtx, cancelTeardown := context.WithTimeout(testCtx, 30*time.Second)
		after, waitErr := waitForNativeStats(teardownCtx, client,
			"concurrent production teardown", func(stats udecx.Stats) bool {
				return stats.ActiveDevices == 0 && stats.PendingOperations == 0
			})
		cancelTeardown()
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		assertCleanNativeStatsDelta(t, before, after)
	})
}

// TestNativeUDELiveOwnerCrashRecovery proves the kernel owner's file cleanup
// contract with an actual process death. The child intentionally bypasses all
// Go defers; the parent must be able to reacquire the exclusive broker only
// after the driver has removed its children and drained forwarded URBs.
func TestNativeUDELiveOwnerCrashRecovery(t *testing.T) {
	if os.Getenv(liveNativeTestEnvironment) != "1" {
		t.Skipf("set %s=1 after installing a verified Microsoft-signed native UDE package",
			liveNativeTestEnvironment)
	}
	if os.Getenv(liveNativeCrashChild) == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		client, err := udecx.Open(ctx)
		if err != nil {
			t.Fatalf("crash child open native UDE controller: %v", err)
		}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		server := serverusb.New(serverusb.ServerConfig{ConnectionTimeout: 5 * time.Second}, logger, nil)
		processor, err := serverusb.NewNativeProcessor(server)
		if err != nil {
			t.Fatal(err)
		}
		host, err := udecx.NewHost(client, processor, 0)
		if err != nil {
			t.Fatal(err)
		}
		serveDone := make(chan error, 1)
		go func() { serveDone <- host.Serve(ctx) }()

		dev, publishInput, _, err := liveNativeControllers()[2].new()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = host.Register(ctx, 0x5649495043524153, dev); err != nil {
			t.Fatalf("crash child register DualSense: %v", err)
		}
		enumerateCtx, cancelEnumerate := context.WithTimeout(ctx, 20*time.Second)
		_, err = waitForNativeStats(enumerateCtx, client,
			"crash child enumeration", func(stats udecx.Stats) bool {
				return stats.ActiveDevices == 1
			})
		cancelEnumerate()
		if err != nil {
			t.Fatal(err)
		}
		inputDeadline := time.Now().Add(time.Second)
		for sequence := uint64(1); time.Now().Before(inputDeadline); sequence++ {
			publishInput(sequence)
			time.Sleep(time.Millisecond)
		}
		inputCtx, cancelInput := context.WithTimeout(ctx, 20*time.Second)
		_, err = waitForNativeStats(inputCtx, client,
			"crash child direct input", func(stats udecx.Stats) bool {
				return stats.InputReportsCompleted != 0
			})
		cancelInput()
		if err != nil {
			t.Fatal(err)
		}
		os.Exit(liveNativeCrashExitCode)
	}

	command := exec.Command(os.Args[0],
		"-test.run=^TestNativeUDELiveOwnerCrashRecovery$", "-test.timeout=90s")
	command.Env = append(os.Environ(), liveNativeCrashChild+"=1")
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != liveNativeCrashExitCode {
		t.Fatalf("crash child exit=%v want %d; output:\n%s",
			err, liveNativeCrashExitCode, output)
	}

	recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelRecovery()
	var recovered *udecx.Client
	for recovered == nil {
		candidate, openErr := udecx.Open(recoveryCtx)
		if openErr == nil {
			stats, queryErr := candidate.QueryStats(recoveryCtx)
			if queryErr == nil && stats.ActiveDevices == 0 && stats.PendingOperations == 0 {
				recovered = candidate
				break
			}
			_ = candidate.Close()
		}
		select {
		case <-recoveryCtx.Done():
			t.Fatalf("native UDE owner/child cleanup did not recover after broker death: %v",
				recoveryCtx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer func() {
		if closeErr := recovered.Close(); closeErr != nil {
			t.Errorf("close recovered native UDE controller: %v", closeErr)
		}
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := serverusb.New(serverusb.ServerConfig{ConnectionTimeout: 5 * time.Second}, logger, nil)
	processor, err := serverusb.NewNativeProcessor(server)
	if err != nil {
		t.Fatal(err)
	}
	host, err := udecx.NewHost(recovered, processor, 0)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancelServe := context.WithCancel(recoveryCtx)
	serveDone := make(chan error, 1)
	go func() { serveDone <- host.Serve(serveCtx) }()
	dev, _, _, err := liveNativeControllers()[2].new()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := host.Register(recoveryCtx, 0x5649495043524153, dev)
	if err != nil {
		t.Fatalf("register controller after broker-death recovery: %v", err)
	}
	if err = host.Unregister(recoveryCtx, identity); err != nil {
		t.Fatalf("unregister controller after broker-death recovery: %v", err)
	}
	cancelServe()
	host.Close()
	select {
	case err = <-serveDone:
		if err != nil {
			t.Fatalf("recovered native UDE host shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered native UDE host did not stop within 5 seconds")
	}
}

// TestNativeUDELiveRootRestartRecovery is enabled only by the signed-package
// PowerShell gate on a disposable Windows test machine. It restarts the exact
// installed root devnode while a real child and direct-input publisher are
// active, then requires the invalidated owner to terminate and a fresh broker
// session to enumerate and service input without stale kernel state.
func TestNativeUDELiveRootRestartRecovery(t *testing.T) {
	if os.Getenv(liveNativeTestEnvironment) != "1" {
		t.Skipf("set %s=1 after installing a verified Microsoft-signed native UDE package",
			liveNativeTestEnvironment)
	}
	instanceID := os.Getenv(liveNativeRestartInstance)
	if instanceID == "" {
		t.Skipf("set %s only through the signed disposable-machine validation gate",
			liveNativeRestartInstance)
	}

	testCtx, cancelTest := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelTest()
	client, err := udecx.Open(testCtx)
	if err != nil {
		t.Fatalf("open native UDE controller before root restart: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := serverusb.New(serverusb.ServerConfig{ConnectionTimeout: 5 * time.Second}, logger, nil)
	processor, err := serverusb.NewNativeProcessor(server)
	if err != nil {
		t.Fatal(err)
	}
	host, err := udecx.NewHost(client, processor, 0)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- host.Serve(testCtx) }()
	dev, publishInput, _, err := liveNativeControllers()[2].new()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.Register(testCtx, 0x56494950504e5052, dev); err != nil {
		t.Fatalf("register DualSense before root restart: %v", err)
	}
	inputDeadline := time.Now().Add(time.Second)
	for sequence := uint64(1); time.Now().Before(inputDeadline); sequence++ {
		publishInput(sequence)
		time.Sleep(time.Millisecond)
	}
	inputCtx, cancelInput := context.WithTimeout(testCtx, 20*time.Second)
	_, err = waitForNativeStats(inputCtx, client,
		"pre-restart direct input", func(stats udecx.Stats) bool {
			return stats.ActiveDevices == 1 && stats.InputReportsCompleted != 0
		})
	cancelInput()
	if err != nil {
		t.Fatal(err)
	}

	restart := exec.CommandContext(testCtx, "pnputil.exe", "/restart-device", instanceID)
	restartOutput, restartErr := restart.CombinedOutput()
	if restartErr != nil {
		host.Close()
		_ = client.Close()
		t.Fatalf("restart exact native UDE root devnode %q: %v\n%s",
			instanceID, restartErr, restartOutput)
	}
	select {
	case <-serveDone:
	case <-time.After(30 * time.Second):
		host.Close()
		_ = client.Close()
		t.Fatal("native host did not observe root-devnode restart within 30 seconds")
	}
	host.Close()
	if closeErr := client.Close(); closeErr != nil {
		t.Fatalf("close invalidated pre-restart controller handle: %v", closeErr)
	}

	var recovered *udecx.Client
	recoveryDeadline := time.Now().Add(45 * time.Second)
	for recovered == nil && time.Now().Before(recoveryDeadline) {
		candidate, openErr := udecx.Open(testCtx)
		if openErr == nil {
			stats, queryErr := candidate.QueryStats(testCtx)
			if queryErr == nil && stats.ActiveDevices == 0 && stats.PendingOperations == 0 {
				recovered = candidate
				break
			}
			_ = candidate.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if recovered == nil {
		t.Fatal("native UDE root devnode did not return as a clean exclusive broker after restart")
	}
	defer recovered.Close()

	server = serverusb.New(serverusb.ServerConfig{ConnectionTimeout: 5 * time.Second}, logger, nil)
	processor, err = serverusb.NewNativeProcessor(server)
	if err != nil {
		t.Fatal(err)
	}
	recoveredHost, err := udecx.NewHost(recovered, processor, 0)
	if err != nil {
		t.Fatal(err)
	}
	recoveredCtx, cancelRecovered := context.WithCancel(testCtx)
	recoveredDone := make(chan error, 1)
	go func() { recoveredDone <- recoveredHost.Serve(recoveredCtx) }()
	dev, publishInput, _, err = liveNativeControllers()[2].new()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := recoveredHost.Register(testCtx, 0x56494950504e5052, dev)
	if err != nil {
		t.Fatalf("register DualSense after root restart: %v", err)
	}
	for sequence := uint64(1); sequence <= 1000; sequence++ {
		publishInput(sequence)
		time.Sleep(time.Millisecond)
	}
	recoveredInputCtx, cancelRecoveredInput := context.WithTimeout(testCtx, 20*time.Second)
	_, err = waitForNativeStats(recoveredInputCtx, recovered,
		"post-restart direct input", func(stats udecx.Stats) bool {
			return stats.ActiveDevices == 1 && stats.InputReportsCompleted != 0
		})
	cancelRecoveredInput()
	if err != nil {
		t.Fatal(err)
	}
	if err = recoveredHost.Unregister(testCtx, identity); err != nil {
		t.Fatalf("unregister DualSense after root restart: %v", err)
	}
	cleanCtx, cancelClean := context.WithTimeout(testCtx, 20*time.Second)
	after, err := waitForNativeStats(cleanCtx, recovered,
		"post-restart teardown", func(stats udecx.Stats) bool {
			return stats.ActiveDevices == 0 && stats.PendingOperations == 0
		})
	cancelClean()
	if err != nil {
		t.Fatal(err)
	}
	assertCleanNativeStatsDelta(t, udecx.Stats{}, after)
	cancelRecovered()
	recoveredHost.Close()
	select {
	case err = <-recoveredDone:
		if err != nil {
			t.Fatalf("post-restart host shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-restart native host did not stop within 5 seconds")
	}
}

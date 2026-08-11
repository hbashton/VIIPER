//go:build windows

package e2e_bench_test

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/_testing/e2e/latency"
	"github.com/Alia5/VIIPER/_testing/e2e/sdl"
	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/internal/cmd"
	"github.com/Alia5/VIIPER/internal/server/api"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/testsupport/latencytrace"
	"github.com/Alia5/VIIPER/viiperclient"
	"github.com/Alia5/VIIPER/viipertypes"
	"golang.org/x/sys/windows"
)

const (
	liveLatencyEnvironment       = "VIIPER_E2E_LIVE_LATENCY"
	liveLatencyPreflight         = "VIIPER_E2E_PRODUCTION_PREFLIGHT"
	liveLatencyOutput            = "VIIPER_E2E_LATENCY_OUTPUT"
	liveLatencySamples           = "VIIPER_E2E_LATENCY_SAMPLES"
	liveLatencyExpectedRevision  = "VIIPER_E2E_EXPECTED_SOURCE_REVISION"
	liveLatencySDLRevision       = "VIIPER_E2E_SDL_SOURCE_REVISION"
	liveLatencySDLDLL            = "VIIPER_E2E_SDL_DLL_PATH"
	liveLatencySDLSHA256         = "VIIPER_E2E_SDL_DLL_SHA256"
	liveLatencyPackageManifest   = "VIIPER_E2E_PACKAGE_MANIFEST_SHA256"
	liveLatencyDriverSHA256      = "VIIPER_E2E_NATIVE_DRIVER_SHA256"
	liveLatencyTraceProfileSHA   = "VIIPER_E2E_TRACE_PROFILE_SHA256"
	liveLatencyDriverBuildID     = "VIIPER_E2E_NATIVE_DRIVER_BUILD_IDENTITY"
	liveLatencyAPIAddress        = "127.0.0.1:33245"
	liveLatencyUSBIPAddress      = "127.0.0.1:33244"
	liveLatencyPassword          = "testpassword1234"
	liveLatencyTransitionTimeout = time.Second
	liveLatencyTransitionDelay   = 2 * time.Millisecond
	liveLatencyDiscoveryTimeout  = 15 * time.Second
	liveLatencyDuplicateQuiet    = 25 * time.Millisecond
	liveLatencySourceQuiet       = 75 * time.Millisecond
)

type liveLatencyConfig struct {
	outputPath          string
	samplePairs         int
	expectedRevision    string
	sdlRevision         string
	sdlDLLPath          string
	sdlDLLSHA256        string
	packageManifestSHA  string
	driverSHA256        string
	traceProfileSHA256  string
	driverBuildIdentity string
}

type liveControllerWorkload struct {
	apiType     string
	vendorID    uint16
	productID   uint16
	sdlType     string
	sdlRealType sdl.GamepadType
	state       func(down bool) encoding.BinaryMarshaler
}

func liveControllerWorkloads() []liveControllerWorkload {
	return []liveControllerWorkload{
		{
			apiType: "xbox360", vendorID: 0x045e, productID: 0x028e,
			sdlType: "xbox360", sdlRealType: sdl.GamepadTypeXbox360,
			state: func(down bool) encoding.BinaryMarshaler {
				state := &xbox360.InputState{}
				if down {
					state.Buttons = xbox360.ButtonA
				}
				return state
			},
		},
		{
			apiType: "dualshock4", vendorID: dualshock4.DefaultVID,
			productID: dualshock4.DefaultPID,
			sdlType:   "ps4", sdlRealType: sdl.GamepadTypePS4,
			state: func(down bool) encoding.BinaryMarshaler {
				state := dualshock4.NewInputState()
				if down {
					state.Buttons = dualshock4.ButtonCross
				}
				return state
			},
		},
		{
			apiType: dualsense.DeviceTypeGamepadOnlyV5, vendorID: dualsense.DefaultVID,
			productID: dualsense.DefaultPIDDS,
			sdlType:   "ps5", sdlRealType: sdl.GamepadTypePS5,
			state: func(down bool) encoding.BinaryMarshaler {
				state := dualsense.NewInputState()
				if down {
					state.Buttons = dualsense.ButtonCross
				}
				return state
			},
		},
	}
}

type latencyServerSession struct {
	cancel context.CancelFunc
	done   <-chan error
	client *viiperclient.Client
	ping   *viipertypes.PingResponse
}

// TestLiveControllerToGameLatencyGate is deliberately inert in ordinary CI.
// The production wrapper performs read-only source, package, loaded-driver,
// and SDL provenance checks before opting in. The test then measures exactly
// the same authenticated API/south-button workload over USB/IP and native UDE
// for Xbox360, DualShock4, and DualSense, then writes every SDL-observed edge.
func TestLiveControllerToGameLatencyGate(t *testing.T) {
	if os.Getenv(liveLatencyEnvironment) != "1" {
		t.Skipf("run _testing/e2e/scripts/Invoke-ViiperE2ELatencyGate.ps1; direct opt-in requires %s=1",
			liveLatencyEnvironment)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	config, err := loadLiveLatencyConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err = validateLiveLatencySource(config); err != nil {
		t.Fatal(err)
	}
	if err = sdl.EnableWindowsRawInput(); err != nil {
		t.Fatalf("enable SDL RawInput for exact Windows source identity: %v", err)
	}
	if err = sdl.Init(sdl.InitFlagGamepad | sdl.InitFlagEvents); err != nil {
		t.Fatalf("initialize source-bound SDL event observer: %v", err)
	}
	defer sdl.Quit()
	traceProvider, err := latencytrace.NewProvider()
	if err != nil {
		t.Fatalf("initialize source-controlled latency TraceLogging provider: %v", err)
	}
	defer traceProvider.Close()
	traceEnableDeadline := time.Now().Add(time.Second)
	for !traceProvider.Enabled() && time.Now().Before(traceEnableDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !traceProvider.Enabled() {
		t.Fatal("source-controlled latency TraceLogging provider was not enabled by WPR")
	}
	qpcFrequency, err := latencytrace.Frequency()
	if err != nil {
		t.Fatalf("query QPC frequency: %v", err)
	}
	loadedSDL, err := loadedModulePath("SDL3.dll")
	if err != nil {
		t.Fatal(err)
	}
	loadedSDL, err = canonicalPath(loadedSDL)
	if err != nil {
		t.Fatalf("resolve loaded SDL module: %v", err)
	}
	if !strings.EqualFold(loadedSDL, config.sdlDLLPath) {
		t.Fatalf("loaded SDL module %q does not match source-bound module %q",
			loadedSDL, config.sdlDLLPath)
	}
	loadedHash, err := fileSHA256(loadedSDL)
	if err != nil {
		t.Fatal(err)
	}
	if loadedHash != config.sdlDLLSHA256 {
		t.Fatalf("loaded SDL SHA-256 %s does not match source-bound SHA-256 %s",
			loadedHash, config.sdlDLLSHA256)
	}

	generatedAt := time.Now().UTC()
	provenance := latency.Provenance{
		SourceRevision:              config.expectedRevision,
		SDLSourceRevision:           config.sdlRevision,
		SDLBinaryPath:               loadedSDL,
		SDLBinarySHA256:             loadedHash,
		NativePackageManifestSHA256: config.packageManifestSHA,
		NativeDriverSHA256:          config.driverSHA256,
		NativeDriverBuildIdentity:   config.driverBuildIdentity,
		QPCFrequency:                qpcFrequency,
		TraceProviderName:           latency.TraceProviderName,
		TraceProviderGUID:           latency.TraceProviderGUID,
		TraceProfileSHA256:          config.traceProfileSHA256,
		USBIPBaselineMode:           latency.USBIPBaselineMode,
		USBIPBaselineVersion:        latency.USBIPBaselineVersion,
		GoVersion:                   runtime.Version(),
		GOOS:                        runtime.GOOS,
		GOARCH:                      runtime.GOARCH,
	}
	suite := &latency.SuiteReport{
		Schema: latency.SuiteSchemaV1, GeneratedAt: generatedAt, Provenance: provenance,
	}

	gateCtx, cancelGate := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancelGate()
	for _, controller := range liveControllerWorkloads() {
		phaseSweepOffsets := latency.ProductionPhaseSweepOffsetsNS()
		report := latency.Report{
			Schema: latency.SchemaV1, GeneratedAt: generatedAt, Provenance: provenance,
			Workload: latency.Workload{
				APIAddress: liveLatencyAPIAddress, USBIPAddress: liveLatencyUSBIPAddress,
				ControllerType:    controller.apiType,
				ExpectedVendorID:  controller.vendorID,
				ExpectedProductID: controller.productID,
				ExpectedSDLType:   controller.sdlType,
				Button:            "south/A", WarmupPairs: latency.ProductionWarmupPairs,
				SamplePairs:            config.samplePairs,
				PerTransitionTimeoutNS: int64(liveLatencyTransitionTimeout),
				InterTransitionDelayNS: int64(liveLatencyTransitionDelay),
				PhaseSweepOffsetsNS:    phaseSweepOffsets,
				PhaseSweepSHA256:       latency.PhaseSweepScheduleSHA256(phaseSweepOffsets),
				Authentication:         latency.AuthenticationMode,
			},
			Policy: latency.Policy{
				MinimumSamplePairs:      latency.MinimumProductionSamplePairs,
				NativeMaxP95NS:          latency.DefaultNativeMaxP95NS,
				NativeMaxP99NS:          latency.DefaultNativeMaxP99NS,
				NativeMaxNS:             latency.DefaultNativeMaxNS,
				NativeMaxP95OverUSBIPNS: latency.DefaultNativeMaxP95OverUSBIPNS,
				NativeMaxP99OverUSBIPNS: latency.DefaultNativeMaxP99OverUSBIPNS,
				NativeMaxOverUSBIPNS:    latency.DefaultNativeMaxOverUSBIPNS,
			},
		}
		for _, block := range latency.ProductionBlockSchedule(config.samplePairs) {
			report.Runs = append(report.Runs,
				runLiveLatencyTransport(gateCtx, block, controller, traceProvider,
					qpcFrequency, config.driverBuildIdentity))
		}
		if err = latency.Finalize(&report); err != nil {
			t.Fatalf("finalize %s source-bound latency report: %v", controller.apiType, err)
		}
		suite.Cases = append(suite.Cases, report)
	}
	if err = latency.FinalizeSuite(suite); err != nil {
		t.Fatalf("finalize source-bound latency suite: %v", err)
	}
	if err = writeLatencyReportExclusive(config.outputPath, suite); err != nil {
		t.Fatalf("write latency report: %v", err)
	}
	for _, controllerReport := range suite.Cases {
		for _, transport := range controllerReport.Transports {
			t.Logf("%s/%s controller-to-SDL: press n=%d p50=%s p95=%s p99=%s max=%s jitter=%s; "+
				"release n=%d p50=%s p95=%s p99=%s max=%s jitter=%s; misses=%d duplicates=%d",
				controllerReport.Workload.ControllerType, transport.Transport,
				transport.Statistics.Press.Count,
				time.Duration(transport.Statistics.Press.P50NS),
				time.Duration(transport.Statistics.Press.P95NS),
				time.Duration(transport.Statistics.Press.P99NS),
				time.Duration(transport.Statistics.Press.MaxNS),
				time.Duration(transport.Statistics.Press.JitterNS),
				transport.Statistics.Release.Count,
				time.Duration(transport.Statistics.Release.P50NS),
				time.Duration(transport.Statistics.Release.P95NS),
				time.Duration(transport.Statistics.Release.P99NS),
				time.Duration(transport.Statistics.Release.MaxNS),
				time.Duration(transport.Statistics.Release.JitterNS),
				transport.Misses.Total(), transport.Duplicates.Total())
		}
	}
	t.Logf("source-bound latency artifact: %s", config.outputPath)
	if err = latency.RequireSuitePass(suite); err != nil {
		t.Errorf("controller-to-game latency gate failed: %v", err)
	}
}

func loadLiveLatencyConfig() (liveLatencyConfig, error) {
	if os.Getenv(liveLatencyPreflight) != "1" {
		return liveLatencyConfig{}, fmt.Errorf(
			"%s=1 is required; use the production preflight wrapper", liveLatencyPreflight)
	}
	config := liveLatencyConfig{
		outputPath:          strings.TrimSpace(os.Getenv(liveLatencyOutput)),
		expectedRevision:    strings.ToLower(strings.TrimSpace(os.Getenv(liveLatencyExpectedRevision))),
		sdlRevision:         strings.ToLower(strings.TrimSpace(os.Getenv(liveLatencySDLRevision))),
		sdlDLLPath:          strings.TrimSpace(os.Getenv(liveLatencySDLDLL)),
		sdlDLLSHA256:        strings.ToLower(strings.TrimSpace(os.Getenv(liveLatencySDLSHA256))),
		packageManifestSHA:  strings.ToLower(strings.TrimSpace(os.Getenv(liveLatencyPackageManifest))),
		driverSHA256:        strings.ToLower(strings.TrimSpace(os.Getenv(liveLatencyDriverSHA256))),
		traceProfileSHA256:  strings.ToLower(strings.TrimSpace(os.Getenv(liveLatencyTraceProfileSHA))),
		driverBuildIdentity: strings.ToLower(strings.TrimSpace(os.Getenv(liveLatencyDriverBuildID))),
	}
	if config.outputPath == "" || config.expectedRevision == "" || config.sdlRevision == "" ||
		config.sdlDLLPath == "" || config.sdlDLLSHA256 == "" ||
		config.packageManifestSHA == "" || config.driverSHA256 == "" ||
		config.traceProfileSHA256 == "" || config.driverBuildIdentity == "" {
		return liveLatencyConfig{}, errors.New("production latency provenance environment is incomplete")
	}
	if !filepath.IsAbs(config.outputPath) || !filepath.IsAbs(config.sdlDLLPath) {
		return liveLatencyConfig{}, errors.New("latency output and SDL DLL paths must be absolute")
	}
	if _, err := os.Stat(config.outputPath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return liveLatencyConfig{}, fmt.Errorf("latency output already exists: %s", config.outputPath)
		}
		return liveLatencyConfig{}, fmt.Errorf("inspect latency output: %w", err)
	}
	if info, err := os.Stat(filepath.Dir(config.outputPath)); err != nil || !info.IsDir() {
		return liveLatencyConfig{}, fmt.Errorf("latency output parent must already exist: %s", filepath.Dir(config.outputPath))
	}
	samples, err := strconv.Atoi(strings.TrimSpace(os.Getenv(liveLatencySamples)))
	if err != nil || samples < latency.MinimumProductionSamplePairs ||
		samples > latency.MaximumProductionSamplePairs {
		return liveLatencyConfig{}, fmt.Errorf("%s must be an integer in [%d, %d]",
			liveLatencySamples, latency.MinimumProductionSamplePairs,
			latency.MaximumProductionSamplePairs)
	}
	config.samplePairs = samples
	canonicalSDL, err := canonicalPath(config.sdlDLLPath)
	if err != nil {
		return liveLatencyConfig{}, fmt.Errorf("resolve source-bound SDL DLL: %w", err)
	}
	config.sdlDLLPath = canonicalSDL
	return config, nil
}

func validateLiveLatencySource(config liveLatencyConfig) error {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return err
	}
	repositoryRoot, err := runGit(workingDirectory, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("latency harness is not an exact Git checkout: %w", err)
	}
	repositoryRoot, err = canonicalPath(strings.TrimSpace(repositoryRoot))
	if err != nil {
		return err
	}
	head, err := runGit(repositoryRoot, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if strings.ToLower(strings.TrimSpace(head)) != config.expectedRevision {
		return fmt.Errorf("latency harness source is %s, expected %s", strings.TrimSpace(head), config.expectedRevision)
	}
	status, err := runGit(repositoryRoot, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("latency source tree is not clean; refusing unreviewed code or data:\n%s", status)
	}
	submodules, err := runGit(repositoryRoot, "submodule", "status", "--recursive")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(submodules), "\n") {
		if line != "" && strings.ContainsRune("-+U", rune(line[0])) {
			return fmt.Errorf("latency source has an unbound submodule: %s", line)
		}
	}
	sdlRoot := filepath.Join(repositoryRoot, "_testing", "e2e", "deps", "SDL")
	sdlRevision, err := runGit(sdlRoot, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if strings.ToLower(strings.TrimSpace(sdlRevision)) != config.sdlRevision {
		return fmt.Errorf("SDL source is %s, expected %s", strings.TrimSpace(sdlRevision), config.sdlRevision)
	}
	wantSDLPath, err := canonicalPath(filepath.Join(sdlRoot, "build", "Debug", "SDL3.dll"))
	if err != nil {
		return err
	}
	if !strings.EqualFold(config.sdlDLLPath, wantSDLPath) {
		return fmt.Errorf("SDL DLL must be the wrapper-linked submodule build %s, got %s",
			wantSDLPath, config.sdlDLLPath)
	}
	return nil
}

func runLiveLatencyTransport(
	ctx context.Context,
	block latency.BlockSpec,
	controller liveControllerWorkload,
	traceProvider *latencytrace.Provider,
	qpcFrequency int64,
	expectedDriverBuildIdentity string,
) (result latency.Run) {
	transport := block.Transport
	result.Order = block.Order
	result.TransportBlock = block.TransportBlock
	result.FirstSequence = block.FirstSequence
	result.SamplePairs = block.SamplePairs
	result.Transport = transport
	result.Authentication = latency.AuthenticationMode
	tempDir, err := os.MkdirTemp("", "viiper-e2e-latency-"+controller.apiType+"-"+transport+"-")
	if err != nil {
		result.Failure = err.Error()
		return result
	}
	defer os.RemoveAll(tempDir)

	baseline, err := snapshotGamepadIDs()
	if err != nil {
		result.Failure = fmt.Sprintf("snapshot baseline SDL gamepads: %v", err)
		return result
	}
	result.Controller.BaselineGamepadIDs = gamepadIDsAsInt32(baseline)

	server, err := startLatencyServer(ctx, transport, tempDir, expectedDriverBuildIdentity)
	if err != nil {
		result.Failure = err.Error()
		return result
	}
	var (
		busCreated bool
		deviceID   string
		gamepadID  sdl.GamepadID
		gamepad    *sdl.Gamepad
		stream     *viiperclient.DeviceStream
	)
	defer func() {
		if stream != nil {
			if closeErr := stream.Close(); closeErr != nil {
				appendLatencyFailure(&result, "close authenticated stream: %v", closeErr)
			}
		}
		if gamepad != nil {
			gamepad.Close()
		}
		if deviceID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, removeErr := server.client.DeviceRemoveCtx(cleanupCtx, 1, deviceID)
			cancel()
			if removeErr != nil {
				appendLatencyFailure(&result, "remove API device: %v", removeErr)
			}
		}
		if busCreated {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, removeErr := server.client.BusRemoveCtx(cleanupCtx, 1)
			cancel()
			if removeErr != nil {
				appendLatencyFailure(&result, "remove API bus: %v", removeErr)
			}
		}
		if gamepadID != 0 {
			if removeErr := waitForGamepadRemoval(gamepadID, 10*time.Second); removeErr != nil {
				appendLatencyFailure(&result, "wait for exact SDL gamepad removal: %v", removeErr)
			}
		}
		if closeErr := server.close(); closeErr != nil {
			appendLatencyFailure(&result, "stop %s server: %v", transport, closeErr)
		}
	}()

	result.Server = serverProof(server.ping)
	unauthenticated := viiperclient.NewWithConfig(liveLatencyAPIAddress, &viiperclient.Config{
		DialTimeout: 500 * time.Millisecond, ReadTimeout: time.Second, WriteTimeout: time.Second,
	})
	probeCtx, cancelProbe := context.WithTimeout(ctx, 2*time.Second)
	_, unauthenticatedErr := unauthenticated.PingCtx(probeCtx)
	cancelProbe()
	var unauthenticatedAPIError *viipertypes.APIError
	if !errors.As(unauthenticatedErr, &unauthenticatedAPIError) ||
		unauthenticatedAPIError.Status != 401 ||
		unauthenticatedAPIError.Title != "Unauthorized" ||
		unauthenticatedAPIError.Detail != "authentication required" {
		result.Failure = fmt.Sprintf(
			"unauthenticated API ping was not explicitly rejected by the live server: %v",
			unauthenticatedErr)
		return result
	}
	reprobeCtx, cancelReprobe := context.WithTimeout(ctx, 2*time.Second)
	reprobe, reprobeErr := server.client.PingCtx(reprobeCtx)
	cancelReprobe()
	if reprobeErr != nil {
		result.Failure = fmt.Sprintf("authenticated API failed immediately after rejection probe: %v", reprobeErr)
		return result
	}
	if err = validatePing(transport, reprobe, expectedDriverBuildIdentity); err != nil ||
		reprobe.Version != server.ping.Version {
		result.Failure = fmt.Sprintf(
			"authenticated API identity changed after rejection probe: response=%+v error=%v",
			reprobe, err)
		return result
	}
	result.UnauthenticatedRejected = true

	requestCtx, cancelRequest := context.WithTimeout(ctx, 10*time.Second)
	bus, err := server.client.BusCreateCtx(requestCtx, 1)
	cancelRequest()
	if err != nil || bus == nil || bus.BusID != 1 {
		result.Failure = fmt.Sprintf("authenticated BusCreate did not return bus 1: response=%v error=%v", bus, err)
		return result
	}
	busCreated = true
	requestCtx, cancelRequest = context.WithTimeout(ctx, 30*time.Second)
	device, err := server.client.DeviceAddCtx(requestCtx, 1, controller.apiType, nil)
	cancelRequest()
	if err != nil {
		result.Failure = fmt.Sprintf("authenticated DeviceAdd over %s: %v", transport, err)
		return result
	}
	if device == nil || device.BusID != 1 || device.DevID != "1" ||
		device.Type != controller.apiType ||
		!strings.EqualFold(device.Vid, fmt.Sprintf("0x%04x", controller.vendorID)) ||
		!strings.EqualFold(device.Pid, fmt.Sprintf("0x%04x", controller.productID)) {
		result.Failure = fmt.Sprintf("DeviceAdd source proof is not the exact %s workload: %+v",
			controller.apiType, device)
		return result
	}
	if transport == latency.TransportUSBIP && device.USBIPPort <= 0 {
		result.Failure = "USB/IP DeviceAdd did not return the exact auto-attached import port"
		return result
	}
	if transport == latency.TransportNativeUDE && device.USBIPPort != 0 {
		result.Failure = fmt.Sprintf("native DeviceAdd returned contradictory USB/IP port %d", device.USBIPPort)
		return result
	}
	deviceID = device.DevID
	result.Device = latency.DeviceProof{
		BusID: 1, DeviceID: device.DevID, Type: device.Type,
		VendorID: controller.vendorID, ProductID: controller.productID, USBIPPort: device.USBIPPort,
	}

	gamepadID, err = discoverExactNewGamepad(ctx, baseline, liveLatencyDiscoveryTimeout)
	if err != nil {
		result.Failure = err.Error()
		return result
	}
	result.Controller.NewGamepadIDs = []int32{int32(gamepadID)}
	gamepad, err = sdl.OpenGamepad(gamepadID)
	if err != nil {
		result.Failure = fmt.Sprintf("open exact newly enumerated SDL gamepad %d: %v", gamepadID, err)
		return result
	}
	result.Controller = controllerProof(result.Controller.BaselineGamepadIDs, gamepad)
	if err = validateControllerProof(result.Controller, controller); err != nil {
		result.Failure = err.Error()
		return result
	}
	if err = bindControllerPnP(&result.Controller, transport, device.USBIPPort); err != nil {
		result.Failure = err.Error()
		return result
	}

	streamCtx, cancelStream := context.WithTimeout(ctx, 10*time.Second)
	stream, err = server.client.OpenStream(streamCtx, 1, device.DevID)
	cancelStream()
	if err != nil {
		result.Failure = fmt.Sprintf("open authenticated %s stream over %s: %v",
			controller.apiType, transport, err)
		return result
	}
	lastEventTimestamp, err := settleNeutralController(gamepad, stream, controller.state(false))
	if err != nil {
		result.Failure = fmt.Sprintf("settle exact SDL source before measurement: %v", err)
		return result
	}
	lastEventTimestamp, err = warmControllerPath(gamepad, stream, controller, lastEventTimestamp, qpcFrequency)
	if err != nil {
		result.Failure = fmt.Sprintf("warm exact controller-to-SDL path: %v", err)
		return result
	}
	observedDown := false
	lastSequence := block.FirstSequence + block.SamplePairs - 1
	for sequence := block.FirstSequence; sequence <= lastSequence; sequence++ {
		lastEventTimestamp, err = waitForCausalDwell(gamepad, sequence,
			latency.TransitionPress, &observedDown, lastEventTimestamp, &result)
		if err != nil {
			result.Failure = err.Error()
			return result
		}
		lastEventTimestamp, err = measureTransition(
			gamepad, stream, sequence, latency.TransitionPress, true,
			controller.state(true), &observedDown, lastEventTimestamp, &result,
			controller.apiType, transport, block.TransportBlock, traceProvider, qpcFrequency)
		if err != nil {
			result.Failure = err.Error()
			return result
		}
		lastEventTimestamp, err = waitForCausalDwell(gamepad, sequence,
			latency.TransitionRelease, &observedDown, lastEventTimestamp, &result)
		if err != nil {
			result.Failure = err.Error()
			return result
		}
		lastEventTimestamp, err = measureTransition(
			gamepad, stream, sequence, latency.TransitionRelease, false,
			controller.state(false), &observedDown, lastEventTimestamp, &result,
			controller.apiType, transport, block.TransportBlock, traceProvider, qpcFrequency)
		if err != nil {
			result.Failure = err.Error()
			return result
		}
	}
	if err = observeDuplicateQuietWindow(gamepad, lastEventTimestamp, &observedDown, &result); err != nil {
		result.Failure = err.Error()
		return result
	}
	if observedDown || gamepad.GetButton(sdl.GamepadButtonSouth) {
		result.Failure = "exact SDL source did not finish in the commanded released state"
	}
	return result
}

func startLatencyServer(ctx context.Context, transport, tempDir,
	expectedDriverBuildIdentity string,
) (*latencyServerSession, error) {
	for _, address := range []string{liveLatencyAPIAddress, liveLatencyUSBIPAddress} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return nil, fmt.Errorf("latency endpoint %s is already occupied: %w", address, err)
		}
		_ = listener.Close()
	}
	credentialPath := filepath.Join(tempDir, "viiper.key.txt")
	if err := os.WriteFile(credentialPath, []byte(liveLatencyPassword), 0o600); err != nil {
		return nil, err
	}
	serverCtx, cancelServer := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	server := cmd.Server{
		USBServerConfig: serverusb.ServerConfig{
			Addr: liveLatencyUSBIPAddress, BusCleanupTimeout: 30 * time.Second,
		},
		APIServerConfig: api.ServerConfig{
			Addr: liveLatencyAPIAddress, AutoAttachLocalClient: true,
			RequireLocalHostAuth: true, DeviceHandlerConnectTimeout: 30 * time.Second,
			Password:     liveLatencyPassword,
			PlatformOpts: api.PlatformOpts{AutoAttachWindowsNative: true},
		},
		ConnectionTimeout: 5 * time.Second,
		Transport:         transport,
		KeyFile:           credentialPath,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { serverDone <- server.StartServer(serverCtx, logger, nil) }()
	client := viiperclient.NewWithConfig(liveLatencyAPIAddress, &viiperclient.Config{
		DialTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
		Password: liveLatencyPassword,
	})
	startupDeadline := time.Now().Add(20 * time.Second)
	var lastError error
	for time.Now().Before(startupDeadline) {
		select {
		case serverErr := <-serverDone:
			cancelServer()
			return nil, fmt.Errorf("%s server stopped during startup: %w", transport, serverErr)
		default:
		}
		pingCtx, cancelPing := context.WithTimeout(ctx, time.Second)
		ping, pingErr := client.PingCtx(pingCtx)
		cancelPing()
		if pingErr == nil {
			if err := validatePing(transport, ping, expectedDriverBuildIdentity); err != nil {
				cancelServer()
				<-serverDone
				return nil, err
			}
			return &latencyServerSession{cancel: cancelServer, done: serverDone, client: client, ping: ping}, nil
		}
		lastError = pingErr
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			cancelServer()
			<-serverDone
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	cancelServer()
	<-serverDone
	return nil, fmt.Errorf("authenticated %s API did not become ready: %v", transport, lastError)
}

func (session *latencyServerSession) close() error {
	session.cancel()
	select {
	case err := <-session.done:
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case <-time.After(15 * time.Second):
		return errors.New("server shutdown timed out")
	}
}

func validatePing(transport string, ping *viipertypes.PingResponse,
	expectedDriverBuildIdentity string,
) error {
	if ping == nil || ping.Server != "VIIPER" || ping.Transport != transport ||
		ping.Version == "" || ping.Ready == nil || !*ping.Ready {
		return fmt.Errorf("authenticated ping does not prove live %s transport: %+v", transport, ping)
	}
	if transport == latency.TransportNativeUDE {
		if ping.NativeUDE == nil || ping.NativeUDE.ABIMajor == 0 ||
			ping.NativeUDE.ExpectedDriverPackageVersion == "" ||
			ping.NativeUDE.LoadedDriverBuildIdentity == "" ||
			ping.NativeUDE.LoadedDriverBuildIdentity != expectedDriverBuildIdentity {
			return fmt.Errorf("authenticated ping lacks native ABI/package proof: %+v", ping)
		}
	} else if ping.NativeUDE != nil {
		return fmt.Errorf("USB/IP ping returned contradictory native proof: %+v", ping.NativeUDE)
	}
	return nil
}

func TestValidatePingRequiresExpectedLoadedDriverIdentity(t *testing.T) {
	ready := true
	expected := strings.Repeat("a", 64)
	ping := &viipertypes.PingResponse{
		Server: "VIIPER", Version: "0.1.0", Transport: latency.TransportNativeUDE,
		Ready: &ready,
		NativeUDE: &viipertypes.NativeUDEInfo{
			ABIMajor: 1, ExpectedDriverPackageVersion: "0.1.0.6",
			LoadedDriverBuildIdentity: expected,
		},
	}
	if err := validatePing(latency.TransportNativeUDE, ping, expected); err != nil {
		t.Fatalf("matching negotiated identity was rejected: %v", err)
	}
	if err := validatePing(latency.TransportNativeUDE, ping, strings.Repeat("b", 64)); err == nil {
		t.Fatal("mismatched negotiated identity was accepted before the native workload")
	}
	ping.NativeUDE.LoadedDriverBuildIdentity = ""
	if err := validatePing(latency.TransportNativeUDE, ping, expected); err == nil {
		t.Fatal("absent negotiated identity was accepted before the native workload")
	}
}

func serverProof(ping *viipertypes.PingResponse) latency.ServerProof {
	proof := latency.ServerProof{
		Server: ping.Server, Version: ping.Version, Transport: ping.Transport,
		Ready: ping.Ready != nil && *ping.Ready,
	}
	if ping.NativeUDE != nil {
		proof.NativeUDE = &latency.NativeServerProof{
			ABIMajor: ping.NativeUDE.ABIMajor, ABIMinor: ping.NativeUDE.ABIMinor,
			Capabilities:                 ping.NativeUDE.Capabilities,
			ExpectedDriverPackageVersion: ping.NativeUDE.ExpectedDriverPackageVersion,
			LoadedDriverBuildIdentity:    ping.NativeUDE.LoadedDriverBuildIdentity,
		}
	}
	return proof
}

func snapshotGamepadIDs() ([]sdl.GamepadID, error) {
	sdl.UpdateGamepads()
	ids, err := sdl.GetGamepads()
	if err != nil {
		return nil, err
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func discoverExactNewGamepad(ctx context.Context, baseline []sdl.GamepadID, timeout time.Duration) (sdl.GamepadID, error) {
	baselineSet := make(map[sdl.GamepadID]struct{}, len(baseline))
	for _, id := range baseline {
		baselineSet[id] = struct{}{}
	}
	deadline := time.Now().Add(timeout)
	var candidate sdl.GamepadID
	var stableSince time.Time
	for time.Now().Before(deadline) {
		current, err := snapshotGamepadIDs()
		if err != nil {
			return 0, err
		}
		currentSet := make(map[sdl.GamepadID]struct{}, len(current))
		for _, id := range current {
			currentSet[id] = struct{}{}
		}
		for id := range baselineSet {
			if _, ok := currentSet[id]; !ok {
				return 0, fmt.Errorf("baseline SDL gamepad %d disappeared during source discovery", id)
			}
		}
		added := make([]sdl.GamepadID, 0, 2)
		for _, id := range current {
			if _, exists := baselineSet[id]; !exists {
				added = append(added, id)
			}
		}
		if len(added) > 1 {
			return 0, fmt.Errorf("source discovery is ambiguous: new SDL gamepads=%v", added)
		}
		if len(added) == 1 {
			if candidate != added[0] {
				candidate = added[0]
				stableSince = time.Now()
			} else if time.Since(stableSince) >= 250*time.Millisecond {
				return candidate, nil
			}
		} else {
			candidate = 0
			stableSince = time.Time{}
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		case <-timer.C:
		}
	}
	return 0, errors.New("timed out waiting for exactly one stable newly enumerated SDL gamepad")
}

func controllerProof(
	baseline []int32,
	gamepad *sdl.Gamepad,
) latency.ControllerProof {
	return latency.ControllerProof{
		BaselineGamepadIDs: baseline,
		NewGamepadIDs:      []int32{int32(gamepad.ID())},
		SDLInstanceID:      int32(gamepad.ID()),
		SDLPath:            gamepad.Path(),
		SDLGUID:            sdl.GetGamepadGUIDForID(gamepad.ID()).String(),
		SDLName:            gamepad.Name(),
		SDLType:            sdlGamepadTypeName(gamepad.RealType()),
		SDLReportedType:    int32(gamepad.Type()),
		SDLRealType:        int32(gamepad.RealType()),
		VendorID:           gamepad.Vendor(),
		ProductID:          gamepad.Product(),
	}
}

func validateControllerProof(proof latency.ControllerProof, controller liveControllerWorkload) error {
	if proof.SDLInstanceID == 0 || proof.SDLPath == "" || proof.SDLGUID == "" || proof.SDLName == "" {
		return fmt.Errorf("new SDL gamepad identity is incomplete: %+v", proof)
	}
	if proof.SDLType != controller.sdlType ||
		proof.VendorID != controller.vendorID || proof.ProductID != controller.productID ||
		sdl.GamepadType(proof.SDLRealType) != controller.sdlRealType {
		return fmt.Errorf("new SDL gamepad is not the API-created %s: %+v", controller.apiType, proof)
	}
	return nil
}

func sdlGamepadTypeName(gamepadType sdl.GamepadType) string {
	switch gamepadType {
	case sdl.GamepadTypeXbox360:
		return "xbox360"
	case sdl.GamepadTypePS4:
		return "ps4"
	case sdl.GamepadTypePS5:
		return "ps5"
	default:
		return fmt.Sprintf("unknown(%d)", gamepadType)
	}
}

func settleNeutralController(
	gamepad *sdl.Gamepad,
	stream *viiperclient.DeviceStream,
	neutral encoding.BinaryMarshaler,
) (uint64, error) {
	if err := stream.SetWriteDeadline(time.Now().Add(liveLatencyTransitionTimeout)); err != nil {
		return 0, err
	}
	if err := stream.WriteBinary(neutral); err != nil {
		return 0, err
	}
	observedDown := gamepad.GetButton(sdl.GamepadButtonSouth)
	quietDeadline := time.Now().Add(liveLatencySourceQuiet)
	overallDeadline := time.Now().Add(2 * time.Second)
	var lastTimestamp uint64
	for time.Now().Before(overallDeadline) {
		remaining := time.Until(quietDeadline)
		if remaining <= 0 {
			if observedDown || gamepad.GetButton(sdl.GamepadButtonSouth) {
				return 0, errors.New("controller remained pressed after neutral synchronization")
			}
			return lastTimestamp, nil
		}
		event, received, err := gamepad.WaitButtonTransition(
			sdl.GamepadButtonSouth, durationMillisecondsCeiling(remaining))
		if err != nil {
			return 0, err
		}
		if !received {
			continue
		}
		if event.TimestampNS == 0 || (lastTimestamp != 0 && event.TimestampNS < lastTimestamp) {
			return 0, errors.New("SDL event timestamp was absent or regressed during source synchronization")
		}
		lastTimestamp = event.TimestampNS
		observedDown = event.Down
		quietDeadline = time.Now().Add(liveLatencySourceQuiet)
	}
	return 0, errors.New("SDL source did not reach a quiet released state")
}

func warmControllerPath(
	gamepad *sdl.Gamepad,
	stream *viiperclient.DeviceStream,
	controller liveControllerWorkload,
	lastTimestamp uint64,
	qpcFrequency int64,
) (uint64, error) {
	observedDown := false
	warmup := latency.Run{}
	var err error
	for sequence := 1; sequence <= latency.ProductionWarmupPairs; sequence++ {
		lastTimestamp, err = waitForCausalDwell(gamepad, sequence, latency.TransitionPress,
			&observedDown, lastTimestamp, &warmup)
		if err != nil {
			return lastTimestamp, err
		}
		lastTimestamp, err = measureTransition(
			gamepad, stream, sequence, latency.TransitionPress, true,
			controller.state(true), &observedDown, lastTimestamp, &warmup,
			"", "", 0, nil, qpcFrequency)
		if err != nil {
			return lastTimestamp, err
		}
		lastTimestamp, err = waitForCausalDwell(gamepad, sequence, latency.TransitionRelease,
			&observedDown, lastTimestamp, &warmup)
		if err != nil {
			return lastTimestamp, err
		}
		lastTimestamp, err = measureTransition(
			gamepad, stream, sequence, latency.TransitionRelease, false,
			controller.state(false), &observedDown, lastTimestamp, &warmup,
			"", "", 0, nil, qpcFrequency)
		if err != nil {
			return lastTimestamp, err
		}
	}
	if err = observeDuplicateQuietWindow(gamepad, lastTimestamp, &observedDown, &warmup); err != nil {
		return lastTimestamp, err
	}
	if warmup.Misses.Total() != 0 || warmup.Duplicates.Total() != 0 {
		return lastTimestamp, fmt.Errorf(
			"warmup observed misses=%d duplicates=%d", warmup.Misses.Total(), warmup.Duplicates.Total())
	}
	if observedDown || gamepad.GetButton(sdl.GamepadButtonSouth) {
		return lastTimestamp, errors.New("warmup did not finish in the commanded released state")
	}
	return lastTimestamp, nil
}

func waitForCausalDwell(
	gamepad *sdl.Gamepad,
	sequence int,
	transition latency.Transition,
	observedDown *bool,
	lastTimestamp uint64,
	result *latency.Run,
) (uint64, error) {
	delay := liveLatencyTransitionDelay +
		time.Duration(latency.ProductionPhaseOffsetNS(sequence, transition))
	deadline := time.Now().Add(delay)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		if remaining < time.Millisecond {
			time.Sleep(remaining)
			continue
		}
		event, received, err := gamepad.WaitButtonTransition(
			sdl.GamepadButtonSouth, int32(remaining/time.Millisecond))
		if err != nil {
			return lastTimestamp, fmt.Errorf("%s sample %d SDL causal dwell: %w", transition, sequence, err)
		}
		if !received {
			continue
		}
		updatedTimestamp, rejection := latency.RejectPreWriteEdge(
			lastTimestamp, event.TimestampNS, event.Down, &result.Duplicates)
		lastTimestamp = updatedTimestamp
		*observedDown = event.Down
		return lastTimestamp, fmt.Errorf("%s sample %d pre-write dwell: %w", transition, sequence, rejection)
	}
	if gamepad.GetButton(sdl.GamepadButtonSouth) != *observedDown {
		return lastTimestamp, fmt.Errorf("%s sample %d SDL state changed without an observed edge during pre-write dwell", transition, sequence)
	}
	return lastTimestamp, nil
}

func measureTransition(
	gamepad *sdl.Gamepad,
	stream *viiperclient.DeviceStream,
	sequence int,
	transition latency.Transition,
	wantDown bool,
	inputState encoding.BinaryMarshaler,
	observedDown *bool,
	lastTimestamp uint64,
	result *latency.Run,
	controllerType string,
	transport string,
	transportBlock int,
	traceProvider *latencytrace.Provider,
	qpcFrequency int64,
) (uint64, error) {
	if *observedDown == wantDown {
		return lastTimestamp, fmt.Errorf("%s sample %d started from the wrong observed state", transition, sequence)
	}
	if err := stream.SetWriteDeadline(time.Now().Add(liveLatencyTransitionTimeout)); err != nil {
		return lastTimestamp, err
	}
	started := time.Now()
	for {
		event, received, err := gamepad.PollButtonTransition(sdl.GamepadButtonSouth)
		if err != nil {
			return lastTimestamp, fmt.Errorf("%s sample %d pre-write SDL drain: %w", transition, sequence, err)
		}
		if !received {
			break
		}
		updatedTimestamp, rejection := latency.RejectPreWriteEdge(
			lastTimestamp, event.TimestampNS, event.Down, &result.Duplicates)
		lastTimestamp = updatedTimestamp
		*observedDown = event.Down
		return lastTimestamp, fmt.Errorf("%s sample %d final pre-write queue drain: %w", transition, sequence, rejection)
	}
	if gamepad.GetButton(sdl.GamepadButtonSouth) != *observedDown {
		return lastTimestamp, fmt.Errorf("%s sample %d SDL state changed before its input write", transition, sequence)
	}
	startQPC, err := latencytrace.Counter()
	if err != nil {
		return lastTimestamp, fmt.Errorf("%s sample %d query pre-write QPC: %w", transition, sequence, err)
	}
	// Keep the SDL clock admission fence adjacent to WriteBinary. There is no
	// cross-process primitive that can make these two calls atomic; requiring
	// the observed event timestamp to be strictly newer closes same-tick stale
	// edges and leaves only this irreducible function-call boundary.
	sdlFenceTimestamp := sdl.TicksNS()
	if sdlFenceTimestamp == 0 {
		return lastTimestamp, fmt.Errorf("%s sample %d could not establish its SDL pre-write timestamp fence", transition, sequence)
	}
	if err := stream.WriteBinary(inputState); err != nil {
		return lastTimestamp, fmt.Errorf("%s sample %d authenticated WriteBinary: %w", transition, sequence, err)
	}
	deadline := started.Add(liveLatencyTransitionTimeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			incrementTransitionCounter(&result.Misses, transition)
			return lastTimestamp, fmt.Errorf("%s sample %d timed out after %s", transition, sequence,
				liveLatencyTransitionTimeout)
		}
		event, received, err := gamepad.WaitButtonTransition(
			sdl.GamepadButtonSouth, durationMillisecondsCeiling(remaining))
		if err != nil {
			return lastTimestamp, fmt.Errorf("%s sample %d SDL event wait: %w", transition, sequence, err)
		}
		if !received {
			incrementTransitionCounter(&result.Misses, transition)
			return lastTimestamp, fmt.Errorf("%s sample %d timed out after %s", transition, sequence,
				liveLatencyTransitionTimeout)
		}
		if timestampErr := latency.ValidatePostWriteTimestamp(
			lastTimestamp, sdlFenceTimestamp, event.TimestampNS); timestampErr != nil {
			if event.TimestampNS != 0 && (lastTimestamp == 0 || event.TimestampNS >= lastTimestamp) &&
				event.TimestampNS <= sdlFenceTimestamp {
				incrementEdgeCounter(&result.Duplicates, event.Down)
			}
			return lastTimestamp, fmt.Errorf("%s sample %d SDL timestamp fence: %w",
				transition, sequence, timestampErr)
		}
		lastTimestamp = event.TimestampNS
		if event.Down == *observedDown {
			incrementEdgeCounter(&result.Duplicates, event.Down)
			continue
		}
		if event.Down != wantDown {
			incrementEdgeCounter(&result.Duplicates, event.Down)
			*observedDown = event.Down
			continue
		}
		*observedDown = event.Down
		endQPC, qpcErr := latencytrace.Counter()
		if qpcErr != nil {
			return lastTimestamp, fmt.Errorf("%s sample %d query observed-edge QPC: %w", transition, sequence, qpcErr)
		}
		latencyNS, qpcErr := latency.QPCIntervalNS(startQPC, endQPC, qpcFrequency)
		if qpcErr != nil {
			return lastTimestamp, fmt.Errorf("%s sample %d convert observed QPC interval: %w", transition, sequence, qpcErr)
		}
		sample := latency.Sample{
			Sequence: sequence, Transition: transition, LatencyNS: latencyNS,
			EventTimestampNS: event.TimestampNS, SDLFenceTimestampNS: sdlFenceTimestamp,
			StartQPCTicks: startQPC, EndQPCTicks: endQPC,
		}
		if traceProvider != nil {
			sample.MarkerID = latency.SampleMarkerID(controllerType, transport, transportBlock, sequence, transition)
			sample.MarkerQPCTicks, err = latencytrace.Counter()
			if err != nil {
				return lastTimestamp, fmt.Errorf("%s sample %d query pre-marker QPC: %w", transition, sequence, err)
			}
			if err = traceProvider.WriteSample(controllerType, transport, transportBlock, sample); err != nil {
				return lastTimestamp, fmt.Errorf("%s sample %d TraceLogging marker: %w", transition, sequence, err)
			}
		}
		result.Samples = append(result.Samples, sample)
		return lastTimestamp, nil
	}
}

func observeDuplicateQuietWindow(
	gamepad *sdl.Gamepad,
	lastTimestamp uint64,
	observedDown *bool,
	result *latency.Run,
) error {
	deadline := time.Now().Add(liveLatencyDuplicateQuiet)
	for time.Now().Before(deadline) {
		event, received, err := gamepad.WaitButtonTransition(
			sdl.GamepadButtonSouth, durationMillisecondsCeiling(time.Until(deadline)))
		if err != nil {
			return err
		}
		if !received {
			return nil
		}
		if event.TimestampNS == 0 || event.TimestampNS < lastTimestamp {
			return errors.New("SDL event timestamp was absent or regressed in duplicate quiet window")
		}
		lastTimestamp = event.TimestampNS
		incrementEdgeCounter(&result.Duplicates, event.Down)
		*observedDown = event.Down
	}
	return nil
}

func incrementTransitionCounter(counters *latency.Counters, transition latency.Transition) {
	if transition == latency.TransitionPress {
		counters.Press++
	} else {
		counters.Release++
	}
}

func incrementEdgeCounter(counters *latency.Counters, down bool) {
	if down {
		counters.Press++
	} else {
		counters.Release++
	}
}

func durationMillisecondsCeiling(duration time.Duration) int32 {
	if duration <= 0 {
		return 1
	}
	milliseconds := (duration + time.Millisecond - 1) / time.Millisecond
	if milliseconds > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(milliseconds)
}

func waitForGamepadRemoval(id sdl.GamepadID, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ids, err := snapshotGamepadIDs()
		if err != nil {
			return err
		}
		present := false
		for _, candidate := range ids {
			if candidate == id {
				present = true
				break
			}
		}
		if !present {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("gamepad %d still present after %s", id, timeout)
}

func gamepadIDsAsInt32(ids []sdl.GamepadID) []int32 {
	result := make([]int32, len(ids))
	for index, id := range ids {
		result[index] = int32(id)
	}
	return result
}

func appendLatencyFailure(run *latency.Run, format string, arguments ...any) {
	message := fmt.Sprintf(format, arguments...)
	if run.Failure == "" {
		run.Failure = message
	} else {
		run.Failure += "; " + message
	}
}

func runGit(directory string, arguments ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err,
			strings.TrimSpace(string(output)))
	}
	return strings.TrimRight(string(output), "\r\n"), nil
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func loadedModulePath(name string) (string, error) {
	moduleName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", err
	}
	var module windows.Handle
	if err = windows.GetModuleHandleEx(
		windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
		moduleName,
		&module,
	); err != nil {
		return "", fmt.Errorf("GetModuleHandleEx(%s): %w", name, err)
	}
	buffer := make([]uint16, 32768)
	length, err := windows.GetModuleFileName(module, &buffer[0], uint32(len(buffer)))
	if err != nil {
		return "", fmt.Errorf("GetModuleFileName(%s): %w", name, err)
	}
	if length == 0 || int(length) >= len(buffer) {
		return "", fmt.Errorf("GetModuleFileName(%s) returned invalid length %d", name, length)
	}
	return windows.UTF16ToString(buffer[:length]), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err = io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeLatencyReportExclusive(path string, report *latency.SuiteReport) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
		if !complete {
			_ = os.Remove(path)
		}
	}()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(report); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	complete = true
	return nil
}

//go:build windows

package usb_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	"github.com/Alia5/VIIPER/device/ns2pro"
	"github.com/Alia5/VIIPER/device/xbox360"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
	usbdevice "github.com/Alia5/VIIPER/usb"
)

const (
	liveNativeTestEnvironment = "VIIPER_UDE_LIVE"
	liveNativeTestIterations  = "VIIPER_UDE_LIVE_ITERATIONS"
)

type liveNativeController struct {
	name string
	new  func() (usbdevice.Device, func(uint64), error)
}

func liveNativeControllers() []liveNativeController {
	return []liveNativeController{
		{name: "Xbox360", new: func() (usbdevice.Device, func(uint64), error) {
			dev, err := xbox360.New(nil)
			return dev, func(sequence uint64) {
				state := xbox360.NewInputState()
				state.LX = int16(sequence % 1024)
				dev.UpdateInputState(*state)
			}, err
		}},
		{name: "DualShock4", new: func() (usbdevice.Device, func(uint64), error) {
			dev, err := dualshock4.New(nil)
			return dev, func(sequence uint64) {
				state := dualshock4.NewInputState()
				state.LX = int8(sequence % 32)
				dev.UpdateInputState(state)
			}, err
		}},
		{name: "DualSense", new: func() (usbdevice.Device, func(uint64), error) {
			dev, err := dualsense.New(nil)
			return dev, func(sequence uint64) {
				state := dualsense.NewInputState()
				state.LX = int8(sequence % 32)
				dev.UpdateInputState(state)
			}, err
		}},
		{name: "DualSenseEdge", new: func() (usbdevice.Device, func(uint64), error) {
			dev, err := dualsense.NewEdge(nil)
			return dev, func(sequence uint64) {
				state := dualsense.NewInputState()
				state.RX = int8(sequence % 32)
				dev.UpdateInputState(state)
			}, err
		}},
		{name: "Switch2Pro", new: func() (usbdevice.Device, func(uint64), error) {
			dev, err := ns2pro.New(nil)
			return dev, func(sequence uint64) {
				state := ns2pro.NewInputState()
				state.LX += uint16(sequence % 32)
				dev.UpdateInputState(*state)
			}, err
		}},
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
	testCtx, cancelTest := context.WithTimeout(context.Background(),
		time.Duration(iterations)*5*time.Minute)
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
	for iteration := 1; iteration <= iterations; iteration++ {
		for controllerIndex, controller := range liveNativeControllers() {
			controller := controller
			t.Run(fmt.Sprintf("%s/generation-%d", controller.name, iteration), func(t *testing.T) {
				deviceID := deviceIDBase + uint64(controllerIndex+1)
				dev, publishInput, createErr := controller.new()
				if createErr != nil {
					t.Fatalf("construct %s: %v", controller.name, createErr)
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
				if inputStats.InputReportsCompleted > inputStats.InputReportsSubmitted {
					t.Fatalf("completed input reports exceed submissions: %+v", inputStats)
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
				dev, publishInput, createErr := controller.new()
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

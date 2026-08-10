package e2e_bench_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/internal/cmd"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/viiperclient"
	"github.com/Alia5/VIIPER/viipertypes"

	_ "github.com/Alia5/VIIPER/internal/registry" // Register all device handlers

	"github.com/Alia5/VIIPER/_testing/e2e/sdl"
)

type TimeWhat int

const (
	TimeWhat_ClientWritePress TimeWhat = iota
	TimeWhat_WaitInput
	TimeWhat_ClientWriteRelease
	TimeWhat_WaitRelease
)

const e2eTransportEnvironment = "VIIPER_E2E_TRANSPORT"

func selectedE2ETransport() (string, error) {
	transport := strings.ToLower(strings.TrimSpace(os.Getenv(e2eTransportEnvironment)))
	if transport == "" {
		return "usbip", nil
	}
	if transport != "usbip" && transport != "native-ude" {
		return "", fmt.Errorf("%s must be usbip or native-ude, got %q",
			e2eTransportEnvironment, transport)
	}
	return transport, nil
}

func Benchmark_Xbox360_Delay(b *testing.B) {
	transport, err := selectedE2ETransport()
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("VIIPER end-to-end transport: %s", transport)

	type bench struct {
		name          string
		timeOn        func(tw TimeWhat, b *testing.B)
		useEncryption bool
	}
	benches := []bench{
		{
			name: "1 Go-Client-Write (PLAIN)",
			timeOn: func(tw TimeWhat, b *testing.B) {
				switch tw {
				case TimeWhat_ClientWritePress:
					b.StartTimer()
				case TimeWhat_WaitInput:
				case TimeWhat_ClientWriteRelease:
				case TimeWhat_WaitRelease:
				}
			},
		},
		{
			name: "2 InputDelay-Without-Client (PLAIN)",
			timeOn: func(tw TimeWhat, b *testing.B) {
				switch tw {
				case TimeWhat_ClientWritePress:
				case TimeWhat_WaitInput:
					b.StartTimer()
				case TimeWhat_ClientWriteRelease:
				case TimeWhat_WaitRelease:
				}
			},
		},
		{
			name: "3 E2E-InputDelay (PLAIN)",
			timeOn: func(tw TimeWhat, b *testing.B) {
				switch tw {
				case TimeWhat_ClientWritePress:
					b.StartTimer()
				case TimeWhat_WaitInput:
					b.StartTimer()
				case TimeWhat_ClientWriteRelease:
				case TimeWhat_WaitRelease:
				}
			},
		},
		{
			name: "4 E2E-PressAndRelease (PLAIN)",
			timeOn: func(tw TimeWhat, b *testing.B) {
				switch tw {
				case TimeWhat_ClientWritePress:
					b.StartTimer()
				case TimeWhat_WaitInput:
					b.StartTimer()
				case TimeWhat_ClientWriteRelease:
					b.StartTimer()
				case TimeWhat_WaitRelease:
					b.StartTimer()
				}
			},
		},
		{
			name: "1 Go-Client-Write (ENC)",
			timeOn: func(tw TimeWhat, b *testing.B) {
				switch tw {
				case TimeWhat_ClientWritePress:
					b.StartTimer()
				case TimeWhat_WaitInput:
				case TimeWhat_ClientWriteRelease:
				case TimeWhat_WaitRelease:
				}
			},
			useEncryption: true,
		},
		{
			name: "2 InputDelay-Without-Client (ENC)",
			timeOn: func(tw TimeWhat, b *testing.B) {
				switch tw {
				case TimeWhat_ClientWritePress:
				case TimeWhat_WaitInput:
					b.StartTimer()
				case TimeWhat_ClientWriteRelease:
				case TimeWhat_WaitRelease:
				}
			},
			useEncryption: true,
		},
		{
			name: "3 E2E-InputDelay (ENC)",
			timeOn: func(tw TimeWhat, b *testing.B) {
				switch tw {
				case TimeWhat_ClientWritePress:
					b.StartTimer()
				case TimeWhat_WaitInput:
					b.StartTimer()
				case TimeWhat_ClientWriteRelease:
				case TimeWhat_WaitRelease:
				}
			},
			useEncryption: true,
		},
		{
			name: "4 E2E-PressAndRelease (ENC)",
			timeOn: func(tw TimeWhat, b *testing.B) {
				switch tw {
				case TimeWhat_ClientWritePress:
					b.StartTimer()
				case TimeWhat_WaitInput:
					b.StartTimer()
				case TimeWhat_ClientWriteRelease:
					b.StartTimer()
				case TimeWhat_WaitRelease:
					b.StartTimer()
				}
			},
			useEncryption: true,
		},
	}

	b.SetParallelism(1)

	defer sdl.Quit()
	if err := sdl.Init(sdl.InitFlagGamepad); err != nil {
		b.Fatalf("SDL init failed: %v", err)
	}

	sdl.UpdateGamepads()
	existingGamepads, _ := sdl.GetGamepads()
	existingGamepadSet := make(map[sdl.GamepadID]bool)
	for _, id := range existingGamepads {
		existingGamepadSet[id] = true
	}

	s := cmd.Server{
		USBServerConfig: usb.ServerConfig{
			Addr:              ":3244",
			BusCleanupTimeout: 1 * time.Second,
		},
		APIServerConfig: api.ServerConfig{
			Addr:                        ":3245",
			AutoAttachLocalClient:       true,
			DeviceHandlerConnectTimeout: time.Second * 5,
			Password:                    "testpassword1234",
			PlatformOpts: api.PlatformOpts{
				AutoAttachWindowsNative: true,
			},
		},
		ConnectionTimeout: 5 * time.Second,
		Transport:         transport,
	}
	logger := slog.Default()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- s.StartServer(ctx, logger, nil)
	}()

	client := viiperclient.New("localhost:3245")
	var busResp *viipertypes.BusCreateResponse
	var createErr error
	for range 10 {
		select {
		case serverErr := <-serverDone:
			b.Fatalf("VIIPER %s server stopped during startup: %v", transport, serverErr)
		default:
		}
		busResp, createErr = client.BusCreate(1)
		if createErr == nil {
			break
		}
		time.Sleep(time.Second * 1)
	}
	if busResp == nil {
		b.Fatalf("BusCreate over %s failed: %v", transport, createErr)
	}
	busID := busResp.BusID
	defer client.BusRemove(busID) //nolint:errcheck

	devInfo, err := client.DeviceAdd(busID, "xbox360", nil)
	if err != nil {
		b.Fatalf("DeviceAdd failed: %v", err)
	}

	var gamepad *sdl.Gamepad
	for range 10 {
		sdl.UpdateGamepads()
		gIDs, _ := sdl.GetGamepads()
		for _, id := range gIDs {
			if !existingGamepadSet[id] {
				gamepad, err = sdl.OpenGamepad(id)
				if err != nil {
					b.Fatalf("OpenGamepad failed: %v", err)
				}
				defer gamepad.Close()
				break
			}
		}
		if gamepad != nil {
			break
		}
		time.Sleep(time.Second * 1)
	}
	if gamepad == nil {
		b.Fatalf("No new gamepad found for testing (expected VIIPER virtual device)")
	}
	padChann := make(chan bool)
	prevPadPressed := false
	go func() {
		defer close(padChann)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			sdl.UpdateGamepads()
			pressed := gamepad.GetButton(sdl.GamepadButtonSouth)
			if pressed != prevPadPressed {
				padChann <- pressed
				prevPadPressed = pressed
			}
		}
	}()

	for _, bench := range benches {
		benchClient := viiperclient.New("localhost:3245")
		if bench.useEncryption {
			benchClient = viiperclient.NewWithPassword("localhost:3245", "testpassword1234")
		}
		devStream, openErr := benchClient.OpenStream(ctx, busID, devInfo.DevID)
		if openErr != nil {
			b.Fatalf("OpenStream for %s failed: %v", bench.name, openErr)
		}
		b.Run(bench.name, func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				bench.timeOn(TimeWhat_ClientWritePress, b)
				err = devStream.WriteBinary(&xbox360.InputState{
					Buttons: xbox360.ButtonA,
				})
				b.StopTimer()
				if err != nil {
					b.Fatalf("WriteBinary failed: %v", err)
				}
				timeout := time.After(1 * time.Second)

				bench.timeOn(TimeWhat_WaitInput, b)
				if err = waitForInput(ctx, timeout, padChann, true); err != nil {
					b.Fatalf("wait for pressed input over %s: %v", transport, err)
				}

				b.StopTimer()
				bench.timeOn(TimeWhat_ClientWriteRelease, b)
				err = devStream.WriteBinary(&xbox360.InputState{})
				b.StopTimer()
				if err != nil {
					b.Fatalf("WriteBinary failed: %v", err)
				}
				timeout = time.After(1 * time.Second)
				bench.timeOn(TimeWhat_WaitRelease, b)
				if err = waitForInput(ctx, timeout, padChann, false); err != nil {
					b.Fatalf("wait for released input over %s: %v", transport, err)
				}

				b.StartTimer()
			}
		})
		if closeErr := devStream.Close(); closeErr != nil {
			b.Fatalf("Close stream for %s: %v", bench.name, closeErr)
		}
	}
}

func waitForInput(ctx context.Context, timeout <-chan time.Time, padChann <-chan bool, wantPressed bool) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return context.DeadlineExceeded
		case pressed, ok := <-padChann:
			if !ok {
				return context.Canceled
			}
			if pressed == wantPressed {
				return nil
			}
		}
	}
}

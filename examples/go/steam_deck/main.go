package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Alia5/VIIPER/apiclient"
	"github.com/Alia5/VIIPER/device/steamdeck"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: steamdeck_client <api_addr>")
		fmt.Println("Example: steamdeck_client localhost:3242")
		os.Exit(1)
	}

	addr := os.Args[1]
	ctx := context.Background()
	api := apiclient.New(addr)

	busesResp, err := api.BusListCtx(ctx)
	if err != nil {
		fmt.Printf("BusList error: %v\n", err)
		os.Exit(1)
	}
	var busID uint32
	createdBus := false
	if len(busesResp.Buses) == 0 {
		r, err := api.BusCreateCtx(ctx, 0)
		if err != nil {
			fmt.Printf("BusCreate failed: %v\n", err)
			os.Exit(1)
		}
		busID = r.BusID
		createdBus = true
		fmt.Printf("Created bus %d\n", busID)
	} else {
		busID = busesResp.Buses[0]
		for _, b := range busesResp.Buses[1:] {
			if b < busID {
				busID = b
			}
		}
		fmt.Printf("Using existing bus %d\n", busID)
	}

	stream, addResp, err := api.AddDeviceAndConnect(ctx, busID, "steamdeck", nil)
	if err != nil {
		fmt.Printf("AddDeviceAndConnect error: %v\n", err)
		if createdBus {
			_, _ = api.BusRemoveCtx(ctx, busID)
		}
		os.Exit(1)
	}
	defer stream.Close()
	fmt.Println("Added device", addResp)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	ticker := time.NewTicker(16 * time.Millisecond)
	defer ticker.Stop()

	var frame uint64
	for {
		select {
		case <-ticker.C:
			frame++
			var buttons uint64
			switch (frame / 60) % 4 {
			case 0:
				buttons = steamdeck.ButtonA
			case 1:
				buttons = 0
			case 2:
				buttons = steamdeck.ButtonX
			default:
				buttons = steamdeck.ButtonY
			}

			inputState := &steamdeck.InputState{
				Buttons:          buttons,
				PressurePadLeft:  0,
				PressurePadRight: 0,
				LeftPadX:         0,
				LeftPadY:         0,
				RightPadX:        0,
				RightPadY:        0,
				LeftStickX:       0,
				LeftStickY:       0,
				RightStickX:      0,
				RightStickY:      0,
				AccelX:           0,
				AccelY:           0,
				AccelZ:           0,
				GyroX:            0,
				GyroY:            0,
				GyroZ:            0,
				GyroQuatW:        0,
				GyroQuatX:        0,
				GyroQuatY:        0,
				GyroQuatZ:        0,
				TriggerRawL:      uint16((frame*20)%math.MaxUint16 + 1),
				TriggerRawR:      uint16((frame*20)%math.MaxUint16 + 1),
			}
			if err := stream.WriteBinary(inputState); err != nil {
				fmt.Printf("Write error: %v\n", err)
				return
			}
			if frame%60 == 0 {
				fmt.Printf("→ Sent input (frame %d): buttons=0x%04x, LT=%d, RT=%d\n", frame, inputState.Buttons, inputState.TriggerRawL, inputState.TriggerRawR)
			}

		case <-sigCh:
			fmt.Println("Signal received, stopping…")
			return
		}
	}

}

package main

import (
	"bufio"
	"context"
	"encoding"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Alia5/VIIPER/device/xboxseries"
	"github.com/Alia5/VIIPER/viiperclient"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: xboxseries_client <api_addr>")
		fmt.Println("Example: xboxseries_client localhost:3242")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt,
		syscall.SIGTERM)
	defer cancel()
	client := viiperclient.New(os.Args[1])

	buses, err := client.BusListCtx(ctx)
	if err != nil {
		fatalf("list buses: %v", err)
	}
	createdBus := len(buses.Buses) == 0
	var busID uint32
	if createdBus {
		created, createErr := client.BusCreateCtx(ctx, 0)
		if createErr != nil {
			fatalf("create bus: %v", createErr)
		}
		busID = created.BusID
	} else {
		busID = buses.Buses[0]
		for _, candidate := range buses.Buses[1:] {
			if candidate < busID {
				busID = candidate
			}
		}
	}

	stream, added, err := client.AddDeviceAndConnect(ctx, busID,
		"xboxseries", nil)
	if err != nil {
		fatalf("add Xbox Series controller: %v", err)
	}
	defer func() {
		_ = stream.Close()
		_, _ = client.DeviceRemoveCtx(context.Background(), added.BusID,
			added.DevID)
		if createdBus {
			_, _ = client.BusRemoveCtx(context.Background(), busID)
		}
	}()

	fmt.Printf("Xbox Series X|S controller %s is active on bus %d\n",
		added.DevID, added.BusID)
	motorCh, errCh := stream.StartReading(ctx, 16,
		func(reader *bufio.Reader) (encoding.BinaryUnmarshaler, error) {
			data := make([]byte, 7)
			if _, readErr := io.ReadFull(reader, data); readErr != nil {
				return nil, readErr
			}
			state := new(xboxseries.MotorState)
			return state, state.UnmarshalBinary(data)
		})

	ticker := time.NewTicker(4 * time.Millisecond)
	defer ticker.Stop()
	state := xboxseries.NewInputState()
	for {
		select {
		case <-ctx.Done():
			return
		case readErr := <-errCh:
			if readErr != nil && ctx.Err() == nil {
				fatalf("read feedback: %v", readErr)
			}
			return
		case message := <-motorCh:
			if message == nil {
				continue
			}
			motor := message.(*xboxseries.MotorState)
			fmt.Printf("motors body(L=%3d R=%3d) impulse(LT=%3d RT=%3d) timing=%d/%d/%d\n",
				motor.LeftMotor, motor.RightMotor, motor.LeftImpulse,
				motor.RightImpulse, motor.Duration, motor.Delay, motor.Repeat)
		case <-ticker.C:
			if writeErr := stream.WriteBinary(state); writeErr != nil {
				fatalf("write state: %v", writeErr)
			}
		}
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

//go:build !race

package dualshock4

import (
	"context"
	"testing"
)

func TestNativeMicrophonePacketEncodingDoesNotAllocate(t *testing.T) {
	dev, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	dev.SetInterfaceAltSetting(InterfaceMicrophone, 1)
	frame := make([]byte, USBMicrophoneClientFrameSize)
	for range microphoneMaximumClientFrames {
		dev.QueueMicrophonePCMFrame(frame)
	}
	packet := make([]byte, USBMicrophoneMaxPacketSize)
	ctx := context.Background()
	allocations := testing.AllocsPerRun(100, func() {
		if _, readErr := dev.ReadIsochronousInput(
			ctx, uint32(EndpointMicrophoneIn), packet,
		); readErr != nil {
			panic(readErr)
		}
	})
	if allocations != 0 {
		t.Fatalf("native microphone packet encoding allocated %.2f objects", allocations)
	}
}

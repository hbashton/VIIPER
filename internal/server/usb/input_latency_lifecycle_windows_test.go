//go:build windows && viiper_latency

package usb

import (
	"context"
	"log/slog"
	"testing"

	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/virtualbus"
)

type inputLatencyLifecycleDevice struct{}

func (*inputLatencyLifecycleDevice) HandleTransfer(
	context.Context, uint32, uint32, []byte,
) []byte {
	return nil
}

func (*inputLatencyLifecycleDevice) GetDescriptor() *usb.Descriptor {
	return testCompositeDescriptor()
}

func (*inputLatencyLifecycleDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func TestInputLatencyImportGenerationTracksRealImportLease(t *testing.T) {
	const busID = uint32(0x7fff0001)
	server := New(ServerConfig{}, slog.Default(), nil)
	bus, err := virtualbus.NewWithBusID(busID)
	if err != nil {
		t.Fatalf("create latency lifecycle bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	if err = server.AddBus(bus); err != nil {
		t.Fatalf("add latency lifecycle bus: %v", err)
	}
	device := &inputLatencyLifecycleDevice{}
	_, err = bus.Add(device)
	if err != nil {
		t.Fatalf("add latency lifecycle device: %v", err)
	}

	if generation, generationErr := server.InputLatencyImportGeneration(busID, 1); generationErr == nil {
		t.Fatalf("inactive import returned generation %d", generation)
	}

	releaseFirst, claimed := server.claimDeviceImport(device)
	if !claimed {
		t.Fatal("claim first latency lifecycle import")
	}
	first, err := server.InputLatencyImportGeneration(busID, 1)
	if err != nil {
		t.Fatalf("read first latency lifecycle generation: %v", err)
	}
	if first == 0 {
		t.Fatal("first latency lifecycle generation is zero")
	}
	if again, againErr := server.InputLatencyImportGeneration(busID, 1); againErr != nil || again != first {
		t.Fatalf("stable import generation = %d, %v; want %d", again, againErr, first)
	}

	releaseFirst()
	if generation, generationErr := server.InputLatencyImportGeneration(busID, 1); generationErr == nil {
		t.Fatalf("released import returned generation %d", generation)
	}

	releaseSecond, claimed := server.claimDeviceImport(device)
	if !claimed {
		t.Fatal("claim successor latency lifecycle import")
	}
	t.Cleanup(releaseSecond)
	second, err := server.InputLatencyImportGeneration(busID, 1)
	if err != nil {
		t.Fatalf("read successor latency lifecycle generation: %v", err)
	}
	if second <= first {
		t.Fatalf("successor import generation = %d, want greater than %d", second, first)
	}
	releaseFirst()
	if afterLateRelease, generationErr := server.InputLatencyImportGeneration(busID, 1); generationErr != nil || afterLateRelease != second {
		t.Fatalf("late old release changed successor generation = %d, %v; want %d",
			afterLateRelease, generationErr, second)
	}
}

func TestInputLatencyImportGenerationRejectsUnknownIdentity(t *testing.T) {
	if generation, err := (*Server)(nil).InputLatencyImportGeneration(1, 1); err == nil {
		t.Fatalf("nil server returned generation %d", generation)
	}
	server := New(ServerConfig{}, slog.Default(), nil)
	if generation, err := server.InputLatencyImportGeneration(1, 1); err == nil {
		t.Fatalf("unknown bus returned generation %d", generation)
	}
}

func TestInputLatencyImportGenerationRejectsRemovedDeviceBeforeLeaseCleanup(t *testing.T) {
	const busID = uint32(0x7fff0002)
	server := New(ServerConfig{}, slog.Default(), nil)
	bus, err := virtualbus.NewWithBusID(busID)
	if err != nil {
		t.Fatalf("create removal lifecycle bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	if err = server.AddBus(bus); err != nil {
		t.Fatalf("add removal lifecycle bus: %v", err)
	}
	device := &inputLatencyLifecycleDevice{}
	if _, err = bus.Add(device); err != nil {
		t.Fatalf("add removal lifecycle device: %v", err)
	}
	release, claimed := server.claimDeviceImport(device)
	if !claimed {
		t.Fatal("claim removal lifecycle import")
	}
	t.Cleanup(release)
	if err = bus.RemoveDeviceByID("1"); err != nil {
		t.Fatalf("remove lifecycle device: %v", err)
	}
	if generation, generationErr := server.InputLatencyImportGeneration(busID, 1); generationErr == nil {
		t.Fatalf("removed device returned lingering import generation %d", generation)
	}
}

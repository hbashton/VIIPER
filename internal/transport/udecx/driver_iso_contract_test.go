package udecx

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func nativeDriverBrokerSource(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve native driver contract test path")
	}
	path := filepath.Join(filepath.Dir(currentFile), "..", "..", "..", "native", "udecx", "driver", "Broker.c")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read native UdeCx broker source: %v", err)
	}
	return string(source)
}

func TestNativeDriverIsoFrameSpanUsesWindowsPacketUnits(t *testing.T) {
	source := nativeDriverBrokerSource(t)
	start := strings.Index(source, "ViiperIsoFrameSpan(")
	if start < 0 {
		t.Fatal("native ISO frame reservation helpers are missing")
	}
	end := strings.Index(source[start:], "ViiperReserveIsoStartFrame(")
	if end < 0 {
		t.Fatal("native ISO frame reservation helpers are missing")
	}
	span := source[start : start+end]

	// A high/super-speed IsoPacket is one service opportunity measured in
	// microframes, so the descriptor exponent is converted to 1-ms StartFrame
	// units. A full-speed IsoPacket is already one 1-ms frame according to the
	// Windows URB contract; multiplying by bInterval here schedules the same
	// polling period twice and leaves artificial holes between reservations.
	for _, required := range []string{
		"deviceContext->Speed == UdecxUsbHighSpeed",
		"deviceContext->Speed == UdecxUsbSuperSpeed",
		"PacketCount * ((ULONGLONG)1 << (interval - 1))",
		"span = (span + 7) / 8;",
		"span = PacketCount;",
	} {
		if !strings.Contains(span, required) {
			t.Fatalf("native ISO frame span is missing %q", required)
		}
	}
	if strings.Contains(span, "PacketCount * interval") {
		t.Fatal("full-speed ISO frame span still multiplies Windows packet units by bInterval")
	}
}

func TestNativeDriverIsoFrameSpanPreservesPlayStationCadence(t *testing.T) {
	frameSpan := func(highSpeed bool, interval uint8, packets uint32) uint32 {
		if packets == 0 {
			return 1
		}
		if interval == 0 {
			return packets
		}
		if !highSpeed {
			return packets
		}
		if interval > 16 {
			return packets
		}
		microframes := uint64(packets) * (uint64(1) << (interval - 1))
		return uint32((microframes + 7) / 8)
	}

	tests := []struct {
		name      string
		highSpeed bool
		interval  uint8
		packets   uint32
		want      uint32
	}{
		{name: "DualShock 4 full-speed audio", interval: 1, packets: 32, want: 32},
		{name: "DualSense high-speed one-ms audio", highSpeed: true, interval: 4, packets: 32, want: 32},
		{name: "high-speed 125-us service", highSpeed: true, interval: 1, packets: 32, want: 4},
		{name: "full-speed descriptor interval is not applied twice", interval: 4, packets: 32, want: 32},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := frameSpan(test.highSpeed, test.interval, test.packets); got != test.want {
				t.Fatalf("frame span=%d want=%d", got, test.want)
			}
		})
	}
}

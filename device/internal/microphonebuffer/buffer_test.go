package microphonebuffer

import (
	"bytes"
	"testing"
)

func readNominalPacket(buffer *Buffer, dst []byte) bool {
	packet := make([]byte, buffer.packetSize+buffer.pcmFrameSize)
	actual, ok := buffer.ReadPacket(packet)
	if ok {
		copy(dst, packet[:min(actual, len(dst))])
	}
	return ok
}

func TestBufferWaitsForTargetBeforeEmittingPCM(t *testing.T) {
	buffer := New(2, 1, 4, 3, 4)
	dst := []byte{0xAA, 0xAA}

	for value := byte(1); value <= 2; value++ {
		if !buffer.QueueFrame(bytes.Repeat([]byte{value}, 4)) {
			t.Fatal("valid frame was rejected")
		}
		if readNominalPacket(&buffer, dst) {
			t.Fatalf("PCM emitted before target after frame %d", value)
		}
		if !bytes.Equal(dst, []byte{0xAA, 0xAA}) {
			t.Fatalf("failed read modified destination: % x", dst)
		}
	}

	buffer.QueueFrame(bytes.Repeat([]byte{3}, 4))
	if !readNominalPacket(&buffer, dst) {
		t.Fatal("PCM did not start at target depth")
	}
	if !bytes.Equal(dst, []byte{1, 1}) {
		t.Fatalf("unexpected first packet: % x", dst)
	}

	state := buffer.State()
	if !state.Primed || state.QueuedBytes != 10 || state.TargetBytes != 12 || state.MaximumBytes != 16 {
		t.Fatalf("unexpected state after prime: %+v", state)
	}
}

func TestBufferUnderrunUsesBoundedRecoveryPrime(t *testing.T) {
	buffer := New(2, 1, 4, 3, 4)
	for value := byte(1); value <= 3; value++ {
		buffer.QueueFrame(bytes.Repeat([]byte{value}, 4))
	}

	dst := make([]byte, 2)
	for packet := 0; packet < 6; packet++ {
		if !readNominalPacket(&buffer, dst) {
			t.Fatalf("buffer starved while draining primed packet %d", packet)
		}
	}
	if readNominalPacket(&buffer, dst) {
		t.Fatal("empty buffer emitted PCM")
	}
	if readNominalPacket(&buffer, dst) {
		t.Fatal("starved buffer emitted PCM twice")
	}
	state := buffer.State()
	if state.Underruns != 1 || state.Reprimes != 0 || state.Primed {
		t.Fatalf("unexpected underrun state: %+v", state)
	}

	buffer.QueueFrame(bytes.Repeat([]byte{4}, 4))
	if readNominalPacket(&buffer, dst) {
		t.Fatal("PCM resumed before the recovery cushion was rebuilt")
	}
	buffer.QueueFrame(bytes.Repeat([]byte{5}, 4))
	if !readNominalPacket(&buffer, dst) || !bytes.Equal(dst, []byte{4, 4}) {
		t.Fatalf("unexpected packet after reprime: % x", dst)
	}
	state = buffer.State()
	if state.Underruns != 1 || state.Reprimes != 1 || !state.Primed {
		t.Fatalf("unexpected reprime state: %+v", state)
	}
}

func TestBufferFallsBackToLegalShortPacketBeforeUnderrun(t *testing.T) {
	buffer := New(8, 2, 16, 3, 4)
	for value := byte(1); value <= 3; value++ {
		buffer.QueueFrame(bytes.Repeat([]byte{value}, 16))
	}
	buffer.discard(buffer.size - 6)

	packet := bytes.Repeat([]byte{0xAA}, 10)
	actual, ok := buffer.ReadPacket(packet)
	if !ok || actual != 6 {
		t.Fatalf("expected legal six-byte short packet, got len=%d ok=%t state=%+v",
			actual, ok, buffer.State())
	}
	if !bytes.Equal(packet[:actual], bytes.Repeat([]byte{3}, actual)) {
		t.Fatalf("short packet changed PCM: % x", packet[:actual])
	}
	if !bytes.Equal(packet[actual:], bytes.Repeat([]byte{0xAA}, len(packet)-actual)) {
		t.Fatalf("short read modified bytes beyond actual length: % x", packet)
	}

	state := buffer.State()
	if state.Underruns != 0 || state.ShortPackets != 1 ||
		state.PacketsRead != 1 || !state.Primed {
		t.Fatalf("short fallback was counted as an underrun: %+v", state)
	}
}

func TestBufferFallsBackFromLongToNominalAndKeepsServoDebt(t *testing.T) {
	buffer := New(8, 2, 16, 3, 4)
	for value := byte(1); value <= 3; value++ {
		buffer.QueueFrame(bytes.Repeat([]byte{value}, 16))
	}
	buffer.discard(buffer.size - 8)
	buffer.servoAccumulator = servoPulseScale

	packet := make([]byte, 10)
	actual, ok := buffer.ReadPacket(packet)
	if !ok || actual != 8 {
		t.Fatalf("expected nominal fallback from unavailable long packet, got len=%d ok=%t",
			actual, ok)
	}
	state := buffer.State()
	if state.LongPackets != 0 || state.ShortPackets != 0 || state.Underruns != 0 {
		t.Fatalf("nominal fallback was accounted as a different packet: %+v", state)
	}
	if buffer.servoAccumulator < servoPulseScale {
		t.Fatalf("unserved long-packet correction was lost: accumulator=%d",
			buffer.servoAccumulator)
	}
}

func TestBufferTrueUnderrunRetainsAlignedTail(t *testing.T) {
	buffer := New(8, 2, 16, 3, 4)
	residual := []byte{0xA1, 0xA2, 0xA3, 0xA4}
	buffer.write(residual)
	buffer.primed = true

	packet := bytes.Repeat([]byte{0xCC}, 10)
	actual, ok := buffer.ReadPacket(packet)
	if ok || actual != 0 {
		t.Fatalf("sub-short tail unexpectedly emitted len=%d ok=%t", actual, ok)
	}
	if !bytes.Equal(packet, bytes.Repeat([]byte{0xCC}, len(packet))) {
		t.Fatalf("failed read modified destination: % x", packet)
	}
	state := buffer.State()
	if state.QueuedBytes != len(residual) || state.Primed || state.Underruns != 1 {
		t.Fatalf("true underrun discarded aligned PCM: %+v", state)
	}
	if _, ok := buffer.ReadPacket(packet); ok {
		t.Fatal("unprimed recovery buffer unexpectedly emitted PCM")
	}
	state = buffer.State()
	if state.Underruns != 1 || state.ZeroPackets != 0 {
		t.Fatalf("internal retry corrupted underrun/zero telemetry: %+v", state)
	}
	buffer.RecordZeroPacket()
	if state = buffer.State(); state.ZeroPackets != 1 {
		t.Fatalf("host-visible zero packet was not counted: %+v", state)
	}

	firstFrame := bytes.Repeat([]byte{0xB1}, 16)
	secondFrame := bytes.Repeat([]byte{0xB2}, 16)
	buffer.QueueFrame(firstFrame)
	if buffer.State().Primed {
		t.Fatal("one client frame did not provide the two-frame recovery cushion")
	}
	buffer.QueueFrame(secondFrame)
	state = buffer.State()
	if !state.Primed || state.Reprimes != 1 {
		t.Fatalf("two client frames did not complete bounded recovery: %+v", state)
	}

	actual, ok = buffer.ReadPacket(packet)
	if !ok || actual != 8 {
		t.Fatalf("recovered buffer did not emit nominal PCM: len=%d ok=%t", actual, ok)
	}
	want := append(append([]byte(nil), residual...), firstFrame[:4]...)
	if !bytes.Equal(packet[:actual], want) {
		t.Fatalf("recovery did not preserve PCM order: got % x want % x",
			packet[:actual], want)
	}
}

func TestBufferRecoveryCushionDoesNotImmediatelyUnderrunAgain(t *testing.T) {
	tests := []struct {
		name         string
		packetSize   int
		pcmFrameSize int
		clientSize   int
	}{
		{name: "DS4", packetSize: 32, pcmFrameSize: 2, clientSize: 320},
		{name: "DualSense", packetSize: 192, pcmFrameSize: 4, clientSize: 1920},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buffer := New(test.packetSize, test.pcmFrameSize, test.clientSize, 6, 20)
			residualSize := test.packetSize - 2*test.pcmFrameSize
			buffer.write(bytes.Repeat([]byte{0x61}, residualSize))
			buffer.primed = true
			packet := make([]byte, test.packetSize+test.pcmFrameSize)
			if _, ok := buffer.ReadPacket(packet); ok {
				t.Fatal("sub-short tail unexpectedly emitted")
			}

			frame := bytes.Repeat([]byte{0x62}, test.clientSize)
			buffer.QueueFrame(frame)
			if buffer.State().Primed {
				t.Fatal("recovery resumed with only one client frame")
			}
			buffer.QueueFrame(frame)
			if !buffer.State().Primed {
				t.Fatal("recovery did not resume with two client frames")
			}

			for millisecond := 0; millisecond < 5000; millisecond++ {
				if (millisecond+1)%10 == 0 {
					buffer.QueueFrame(frame)
				}
				actual, ok := buffer.ReadPacket(packet)
				if !ok {
					t.Fatalf("recovered stream immediately re-underran at %d ms: %+v",
						millisecond, buffer.State())
				}
				if actual != test.packetSize-test.pcmFrameSize &&
					actual != test.packetSize &&
					actual != test.packetSize+test.pcmFrameSize {
					t.Fatalf("recovery emitted illegal packet length %d", actual)
				}
			}

			state := buffer.State()
			if state.Underruns != 1 || state.Reprimes != 1 || !state.Primed {
				t.Fatalf("recovery entered an underrun loop: %+v", state)
			}
		})
	}
}

func TestBufferOverflowKeepsNewestFrames(t *testing.T) {
	buffer := New(2, 1, 4, 3, 4)
	for value := byte(0); value < 6; value++ {
		buffer.QueueFrame(bytes.Repeat([]byte{value}, 4))
	}

	state := buffer.State()
	if state.QueuedBytes != 16 || state.DroppedBytes != 8 {
		t.Fatalf("unexpected bounded queue state: %+v", state)
	}

	dst := make([]byte, 2)
	if !readNominalPacket(&buffer, dst) || !bytes.Equal(dst, []byte{2, 2}) {
		t.Fatalf("queue did not retain newest PCM: % x", dst)
	}
}

func TestBufferResetPreservesCountersAndRequiresInitialPrime(t *testing.T) {
	buffer := New(2, 1, 4, 3, 4)
	for value := byte(1); value <= 3; value++ {
		buffer.QueueFrame(bytes.Repeat([]byte{value}, 4))
	}
	dst := make([]byte, 2)
	for range 6 {
		if !readNominalPacket(&buffer, dst) {
			t.Fatal("primed buffer starved before the reset setup underrun")
		}
	}
	if readNominalPacket(&buffer, dst) {
		t.Fatal("empty buffer emitted PCM")
	}

	buffer.QueueFrame([]byte{4, 4, 4, 4})
	buffer.QueueFrame([]byte{5, 5, 5, 5})
	if buffer.State().Reprimes != 1 {
		t.Fatal("expected recovery before reset")
	}
	buffer.Reset()

	state := buffer.State()
	if state.QueuedBytes != 0 || state.Primed || state.Underruns != 1 || state.Reprimes != 1 {
		t.Fatalf("unexpected reset state: %+v", state)
	}

	buffer.QueueFrame([]byte{6, 6, 6, 6})
	buffer.QueueFrame([]byte{7, 7, 7, 7})
	if readNominalPacket(&buffer, dst) {
		t.Fatal("explicit reset resumed at the smaller recovery threshold")
	}
	buffer.QueueFrame([]byte{8, 8, 8, 8})
	if !readNominalPacket(&buffer, dst) || !bytes.Equal(dst, []byte{6, 6}) {
		t.Fatalf("explicit reset did not require a fresh full target: % x", dst)
	}
	if buffer.State().Reprimes != 1 {
		t.Fatal("initial prime after reset was incorrectly counted as a reprime")
	}
}

func TestAdaptiveClockServoTracksIndependentSourceClocksWithoutLosingPCM(t *testing.T) {
	tests := []struct {
		name         string
		packetSize   int
		pcmFrameSize int
		clientSize   int
		ppm          int
	}{
		{name: "DS4 source fast", packetSize: 32, pcmFrameSize: 2, clientSize: 320, ppm: 5000},
		{name: "DS4 source slow", packetSize: 32, pcmFrameSize: 2, clientSize: 320, ppm: -5000},
		{name: "DualSense source fast", packetSize: 192, pcmFrameSize: 4, clientSize: 1920, ppm: 5000},
		{name: "DualSense source slow", packetSize: 192, pcmFrameSize: 4, clientSize: 1920, ppm: -5000},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buffer := New(test.packetSize, test.pcmFrameSize, test.clientSize, 3, 4)
			var sourceOffset uint64
			var outputOffset uint64
			queueFrame := func() {
				frame := make([]byte, test.clientSize)
				for index := range frame {
					frame[index] = byte(sourceOffset%251 + 1)
					sourceOffset++
				}
				if !buffer.QueueFrame(frame) {
					t.Fatal("valid source frame was rejected")
				}
			}
			for range 3 {
				queueFrame()
			}

			packet := make([]byte, test.packetSize+test.pcmFrameSize)
			var sourcePhase int64
			for millisecond := 0; millisecond < 120000; millisecond++ {
				sourcePhase += int64(1000000 + test.ppm)
				for sourcePhase >= 10000000 {
					queueFrame()
					sourcePhase -= 10000000
				}

				actual, ok := buffer.ReadPacket(packet)
				if !ok {
					t.Fatalf("buffer starved at %d ms: %+v", millisecond, buffer.State())
				}
				if actual != test.packetSize-test.pcmFrameSize &&
					actual != test.packetSize && actual != test.packetSize+test.pcmFrameSize {
					t.Fatalf("invalid adaptive packet length %d", actual)
				}
				for _, sampleByte := range packet[:actual] {
					want := byte(outputOffset%251 + 1)
					if sampleByte != want {
						t.Fatalf("PCM changed at output byte %d: got %02x want %02x",
							outputOffset, sampleByte, want)
					}
					outputOffset++
				}
			}

			state := buffer.State()
			if state.Underruns != 0 || state.Reprimes != 0 ||
				state.OverflowEvents != 0 || state.DroppedBytes != 0 {
				t.Fatalf("clock servo touched a queue rail: %+v", state)
			}
			if test.ppm > 0 && state.LongPackets == 0 {
				t.Fatalf("fast source never produced a long USB packet: %+v", state)
			}
			if test.ppm < 0 && state.ShortPackets == 0 {
				t.Fatalf("slow source never produced a short USB packet: %+v", state)
			}
		})
	}
}

func TestAdaptiveClockServoAbsorbsAlternatingClientJitter(t *testing.T) {
	for _, intervals := range [][]int{{8, 12}, {9, 11}} {
		buffer := New(32, 2, 320, 3, 4)
		frame := bytes.Repeat([]byte{0x5A}, 320)
		for range 3 {
			buffer.QueueFrame(frame)
		}
		packet := make([]byte, 34)
		intervalIndex := 0
		untilFrame := intervals[intervalIndex]
		for millisecond := 0; millisecond < 120000; millisecond++ {
			untilFrame--
			if untilFrame == 0 {
				buffer.QueueFrame(frame)
				intervalIndex = (intervalIndex + 1) % len(intervals)
				untilFrame = intervals[intervalIndex]
			}
			if _, ok := buffer.ReadPacket(packet); !ok {
				t.Fatalf("%v jitter starved at %d ms: %+v", intervals, millisecond, buffer.State())
			}
		}
		state := buffer.State()
		if state.Underruns != 0 || state.OverflowEvents != 0 || state.DroppedBytes != 0 {
			t.Fatalf("%v jitter touched a queue rail: %+v", intervals, state)
		}
	}
}

func TestAdaptiveClockServoDoesNotCorrectAnExactClockAfterSettling(t *testing.T) {
	buffer := New(32, 2, 320, 3, 4)
	frame := bytes.Repeat([]byte{0x33}, 320)
	for range 3 {
		buffer.QueueFrame(frame)
	}
	packet := make([]byte, 34)
	var settledShort uint64
	var settledLong uint64
	for millisecond := 0; millisecond < 120000; millisecond++ {
		if (millisecond+1)%10 == 0 {
			buffer.QueueFrame(frame)
		}
		if _, ok := buffer.ReadPacket(packet); !ok {
			t.Fatalf("exact clock starved at %d ms: %+v", millisecond, buffer.State())
		}
		if millisecond == 59999 {
			state := buffer.State()
			settledShort = state.ShortPackets
			settledLong = state.LongPackets
		}
	}
	state := buffer.State()
	if state.ShortPackets != settledShort || state.LongPackets != settledLong {
		t.Fatalf("exact clock kept correcting after settling: before=%d/%d after=%d/%d state=%+v",
			settledShort, settledLong, state.ShortPackets, state.LongPackets, state)
	}
	if state.Underruns != 0 || state.OverflowEvents != 0 {
		t.Fatalf("exact clock touched a queue rail: %+v", state)
	}
}

func TestProductionCushionAbsorbsFortyMillisecondProducerStalls(t *testing.T) {
	tests := []struct {
		name         string
		packetSize   int
		pcmFrameSize int
		clientSize   int
	}{
		{name: "DS4", packetSize: 32, pcmFrameSize: 2, clientSize: 320},
		{name: "DualSense", packetSize: 192, pcmFrameSize: 4, clientSize: 1920},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buffer := New(test.packetSize, test.pcmFrameSize, test.clientSize, 6, 10)
			frame := bytes.Repeat([]byte{0x66}, test.clientSize)
			for range 6 {
				buffer.QueueFrame(frame)
			}
			packet := make([]byte, test.packetSize+test.pcmFrameSize)
			pendingFrames := 0
			for millisecond := 0; millisecond < 120000; millisecond++ {
				if (millisecond+1)%10 == 0 {
					pendingFrames++
				}
				stallPhase := millisecond % 2000
				if !(stallPhase >= 1000 && stallPhase < 1040) {
					for pendingFrames > 0 {
						buffer.QueueFrame(frame)
						pendingFrames--
					}
				}
				if _, ok := buffer.ReadPacket(packet); !ok {
					t.Fatalf("40 ms producer stall starved at %d ms: %+v",
						millisecond, buffer.State())
				}
			}
			state := buffer.State()
			if state.Underruns != 0 || state.OverflowEvents != 0 ||
				state.DroppedBytes != 0 {
				t.Fatalf("production cushion touched a rail: %+v", state)
			}
		})
	}
}

func TestAdaptiveClockServoResetClearsControlState(t *testing.T) {
	buffer := New(32, 2, 320, 3, 4)
	frame := bytes.Repeat([]byte{0x44}, 320)
	for range 4 {
		buffer.QueueFrame(frame)
	}
	packet := make([]byte, 34)
	for range 200 {
		if _, ok := buffer.ReadPacket(packet); !ok {
			break
		}
	}
	buffer.Reset()
	state := buffer.State()
	if state.FilteredBytes != 0 || state.ServoRatePPM != 0 || state.Primed {
		t.Fatalf("reset retained adaptive clock state: %+v", state)
	}
}

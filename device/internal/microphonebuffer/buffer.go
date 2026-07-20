// Package microphonebuffer provides the small PCM jitter buffer shared by
// VIIPER's virtual controller microphone endpoints.
package microphonebuffer

import "time"

// State is a point-in-time snapshot of a Buffer.
type State struct {
	QueuedBytes    int
	TargetBytes    int
	MaximumBytes   int
	FilteredBytes  int
	Primed         bool
	Underruns      uint64
	Reprimes       uint64
	DroppedBytes   uint64
	PacketsRead    uint64
	ZeroPackets    uint64
	OverflowEvents uint64
	ShortPackets   uint64
	LongPackets    uint64
	ServoRatePPM   int
	LowWaterBytes  int
	HighWaterBytes int
	QueueFrames    uint64
	QueueFastGaps  uint64
	QueueLateGaps  uint64
	QueueMinGapUS  int64
	QueueMaxGapUS  int64
	ReadFastGaps   uint64
	ReadLateGaps   uint64
	ReadMinGapUS   int64
	ReadMaxGapUS   int64
}

// Buffer stores complete client PCM frames and emits one USB isochronous
// packet at a time. It is intentionally not internally synchronized; each
// controller already protects it with the device mutex.
type Buffer struct {
	data                []byte
	head                int
	size                int
	packetSize          int
	pcmFrameSize        int
	frameSize           int
	targetSize          int
	recoverySize        int
	queueCenter         int
	lowRailSize         int
	highRailSize        int
	nominalPacketFrames int
	filteredQueueQ16    int64
	filterInitialized   bool
	servoAccumulator    int64
	servoRatePPM        int

	primed         bool
	needsReprime   bool
	underruns      uint64
	reprimes       uint64
	droppedBytes   uint64
	packetsRead    uint64
	zeroPackets    uint64
	overflowEvents uint64
	shortPackets   uint64
	longPackets    uint64
	lowWaterBytes  int
	highWaterBytes int
	queueFrames    uint64
	queueFastGaps  uint64
	queueLateGaps  uint64
	queueMinGap    time.Duration
	queueMaxGap    time.Duration
	readFastGaps   uint64
	readLateGaps   uint64
	readMinGap     time.Duration
	readMaxGap     time.Duration
	lastQueueTime  time.Time
	lastReadTime   time.Time
}

const (
	occupancyFilterShift = 6 // EWMA alpha = 1/64 at the 1 ms USB cadence.
	servoGainPPMPerMS    = 2000
	servoLimitPPM        = 10000
	servoPulseScale      = 1000000
	recoveryClientFrames = 2
)

// New creates a bounded PCM buffer. targetFrames is the number of complete
// client frames required before initial capture starts. Recovery after a true
// underrun uses a smaller two-frame phase cushion; explicit Reset returns to
// the full initial target. maximumFrames is the hard latency bound; overflow
// always discards the oldest audio so the endpoint presents the freshest
// capture data.
func New(packetSize, pcmFrameSize, frameSize, targetFrames, maximumFrames int) Buffer {
	if packetSize <= 0 || pcmFrameSize <= 0 ||
		packetSize%pcmFrameSize != 0 || frameSize <= 0 ||
		frameSize%packetSize != 0 {
		panic("microphonebuffer: invalid PCM, packet, or client frame size")
	}
	if targetFrames <= 0 || maximumFrames < targetFrames {
		panic("microphonebuffer: invalid target or maximum frame count")
	}

	capacity := frameSize * maximumFrames
	return Buffer{
		data:         make([]byte, capacity),
		packetSize:   packetSize,
		pcmFrameSize: pcmFrameSize,
		frameSize:    frameSize,
		targetSize:   frameSize * targetFrames,
		recoverySize: frameSize * min(targetFrames, recoveryClientFrames),
		queueCenter:  frameSize*targetFrames - frameSize/2,
		lowRailSize: max(pcmFrameSize,
			frameSize*targetFrames-frameSize*3/2),
		highRailSize: min(capacity-pcmFrameSize,
			frameSize*targetFrames+frameSize/2),
		nominalPacketFrames: packetSize / pcmFrameSize,
		lowWaterBytes:       -1,
	}
}

// QueueFrame appends one complete client PCM frame. It returns false for an
// invalid frame length and leaves the buffer unchanged.
func (b *Buffer) QueueFrame(frame []byte) bool {
	if len(frame) != b.frameSize {
		return false
	}
	b.recordQueueTime(time.Now())

	if overflow := b.size + len(frame) - len(b.data); overflow > 0 {
		b.discard(overflow)
		b.droppedBytes += uint64(overflow)
		b.overflowEvents++
	}

	b.write(frame)
	primeSize := b.targetSize
	if b.needsReprime {
		primeSize = b.recoverySize
	}
	if !b.primed && b.size >= primeSize {
		b.primed = true
		if b.needsReprime {
			b.reprimes++
			b.needsReprime = false
		}
	}
	b.recordWatermark()

	return true
}

// ReadPacket copies one adaptive USB packet into dst and returns its actual
// byte length. Packets contain exactly one fewer, the nominal number, or one
// additional interleaved PCM sample-frame. USB Audio accepts these variable
// isochronous packet lengths to reconcile the source and host clocks without
// resampling or dropping waveform samples. dst is never modified on failure.
func (b *Buffer) ReadPacket(dst []byte) (int, bool) {
	if len(dst) < b.packetSize+b.pcmFrameSize {
		return 0, false
	}
	if !b.primed {
		return 0, false
	}

	actualSize := b.nextPacketSize()
	if b.size < actualSize {
		// USB Audio accepts the nominal packet and one fewer PCM sample-frame.
		// Use the largest legal packet still available instead of turning a
		// single clock-phase deficit into a capture gap. Packet accounting is
		// committed only afterward so servo telemetry describes what reached the
		// host and any unserved long-packet correction remains owed.
		shortSize := b.packetSize - b.pcmFrameSize
		if b.size >= b.packetSize {
			actualSize = b.packetSize
		} else if b.size >= shortSize {
			actualSize = shortSize
		} else {
			b.primed = false
			b.needsReprime = true
			b.underruns++
			// Every normal write/read is PCM-frame aligned. Preserve any valid
			// tail instead of discarding fresh capture; defensively trim only a
			// malformed partial sample-frame from the logical tail.
			b.size -= b.size % b.pcmFrameSize
			if b.size == 0 {
				b.head = 0
			}
			b.resetServo()
			b.recordWatermark()
			return 0, false
		}
	}

	b.commitPacketSize(actualSize)
	b.read(dst[:actualSize])
	b.recordReadTime(time.Now())
	b.packetsRead++
	b.recordWatermark()
	return actualSize, true
}

// RecordZeroPacket records a zero-filled packet actually returned to the USB
// host. Failed internal read attempts are deliberately not counted here.
func (b *Buffer) RecordZeroPacket() {
	b.zeroPackets++
}

// Reset discards queued PCM and returns to initial priming. Lifetime telemetry
// remains intact so interface close/open cycles do not erase diagnostics.
func (b *Buffer) Reset() {
	b.head = 0
	b.size = 0
	b.primed = false
	b.needsReprime = false
	b.resetServo()
	b.lastQueueTime = time.Time{}
	b.lastReadTime = time.Time{}
	b.recordWatermark()
}

// State returns buffer depth, policy, and lifetime recovery telemetry.
func (b *Buffer) State() State {
	return State{
		QueuedBytes:    b.size,
		TargetBytes:    b.targetSize,
		MaximumBytes:   len(b.data),
		FilteredBytes:  b.filteredQueueBytes(),
		Primed:         b.primed,
		Underruns:      b.underruns,
		Reprimes:       b.reprimes,
		DroppedBytes:   b.droppedBytes,
		PacketsRead:    b.packetsRead,
		ZeroPackets:    b.zeroPackets,
		OverflowEvents: b.overflowEvents,
		ShortPackets:   b.shortPackets,
		LongPackets:    b.longPackets,
		ServoRatePPM:   b.servoRatePPM,
		LowWaterBytes:  b.lowWaterBytes,
		HighWaterBytes: b.highWaterBytes,
		QueueFrames:    b.queueFrames,
		QueueFastGaps:  b.queueFastGaps,
		QueueLateGaps:  b.queueLateGaps,
		QueueMinGapUS:  b.queueMinGap.Microseconds(),
		QueueMaxGapUS:  b.queueMaxGap.Microseconds(),
		ReadFastGaps:   b.readFastGaps,
		ReadLateGaps:   b.readLateGaps,
		ReadMinGapUS:   b.readMinGap.Microseconds(),
		ReadMaxGapUS:   b.readMaxGap.Microseconds(),
	}
}

func (b *Buffer) recordQueueTime(now time.Time) {
	b.queueFrames++
	if !b.lastQueueTime.IsZero() {
		gap := now.Sub(b.lastQueueTime)
		if b.queueMinGap == 0 || gap < b.queueMinGap {
			b.queueMinGap = gap
		}
		if gap > b.queueMaxGap {
			b.queueMaxGap = gap
		}
		if gap < 5*time.Millisecond {
			b.queueFastGaps++
		}
		if gap > 15*time.Millisecond {
			b.queueLateGaps++
		}
	}
	b.lastQueueTime = now
}

func (b *Buffer) recordReadTime(now time.Time) {
	if !b.lastReadTime.IsZero() {
		gap := now.Sub(b.lastReadTime)
		if b.readMinGap == 0 || gap < b.readMinGap {
			b.readMinGap = gap
		}
		if gap > b.readMaxGap {
			b.readMaxGap = gap
		}
		if gap < 500*time.Microsecond {
			b.readFastGaps++
		}
		if gap > 1500*time.Microsecond {
			b.readLateGaps++
		}
	}
	b.lastReadTime = now
}

func (b *Buffer) nextPacketSize() int {
	// Observe the queue at the common post-service point. At exact 10 ms input
	// and 1 ms USB output cadence this removes the nominal packet phase offset,
	// so the controller does not manufacture corrections for a healthy clock.
	projectedSize := max(0, b.size-b.packetSize)
	currentQ16 := int64(projectedSize) << 16
	if !b.filterInitialized {
		// Begin at the desired clock point so initial target priming does not
		// immediately request long packets before the filter observes a trend.
		b.filteredQueueQ16 = int64(b.queueCenter) << 16
		b.filterInitialized = true
	}
	b.filteredQueueQ16 += (currentQ16 - b.filteredQueueQ16) >> occupancyFilterShift

	filtered := b.filteredQueueBytes()
	bytesPerMillisecond := max(1, b.frameSize/10)
	deadband := bytesPerMillisecond
	ratePPM := 0
	switch {
	case filtered < b.lowRailSize:
		ratePPM = -servoLimitPPM
	case filtered > b.highRailSize:
		ratePPM = servoLimitPPM
	case filtered > b.queueCenter+deadband:
		ratePPM = (filtered - b.queueCenter - deadband) * servoGainPPMPerMS /
			bytesPerMillisecond
	case filtered < b.queueCenter-deadband:
		ratePPM = -((b.queueCenter - deadband - filtered) * servoGainPPMPerMS /
			bytesPerMillisecond)
	}
	if ratePPM > servoLimitPPM {
		ratePPM = servoLimitPPM
	} else if ratePPM < -servoLimitPPM {
		ratePPM = -servoLimitPPM
	}
	b.servoRatePPM = ratePPM
	b.servoAccumulator += int64(ratePPM * b.nominalPacketFrames)

	packetSize := b.packetSize
	if b.servoAccumulator >= servoPulseScale {
		packetSize += b.pcmFrameSize
	} else if b.servoAccumulator <= -servoPulseScale {
		packetSize -= b.pcmFrameSize
	}
	return packetSize
}

func (b *Buffer) commitPacketSize(packetSize int) {
	switch packetSize {
	case b.packetSize - b.pcmFrameSize:
		// A short packet consumes one fewer sample-frame than nominal, whether
		// selected by the servo or used as the starvation fallback.
		b.servoAccumulator += servoPulseScale
		b.shortPackets++
	case b.packetSize + b.pcmFrameSize:
		b.servoAccumulator -= servoPulseScale
		b.longPackets++
	}
}

func (b *Buffer) filteredQueueBytes() int {
	if !b.filterInitialized {
		return 0
	}
	return int((b.filteredQueueQ16 + 1<<15) >> 16)
}

func (b *Buffer) resetServo() {
	b.filteredQueueQ16 = 0
	b.filterInitialized = false
	b.servoAccumulator = 0
	b.servoRatePPM = 0
}

func (b *Buffer) recordWatermark() {
	if !b.primed {
		return
	}
	if b.lowWaterBytes < 0 || b.size < b.lowWaterBytes {
		b.lowWaterBytes = b.size
	}
	if b.size > b.highWaterBytes {
		b.highWaterBytes = b.size
	}
}

func (b *Buffer) write(src []byte) {
	tail := (b.head + b.size) % len(b.data)
	first := min(len(src), len(b.data)-tail)
	copy(b.data[tail:tail+first], src[:first])
	copy(b.data, src[first:])
	b.size += len(src)
}

func (b *Buffer) read(dst []byte) {
	first := min(len(dst), len(b.data)-b.head)
	copy(dst[:first], b.data[b.head:b.head+first])
	copy(dst[first:], b.data[:len(dst)-first])
	b.discard(len(dst))
}

func (b *Buffer) discard(count int) {
	if count <= 0 {
		return
	}
	if count >= b.size {
		b.head = 0
		b.size = 0
		return
	}
	b.head = (b.head + count) % len(b.data)
	b.size -= count
}

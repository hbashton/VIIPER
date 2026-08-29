package usb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
	usbdesc "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

const (
	endpointQueueCapacity      = 64
	preallocatedIsoPacketCount = 32
	errNoSpace                 = -28 // -ENOSPC
	maximumTransferSize        = 16 * 1024 * 1024
)

type endpointWorkerKind uint8

const (
	interruptInWorker endpointWorkerKind = iota
	isoInWorker
	isoOutWorker
	genericInWorker
)

type endpointWorkerKey struct {
	ep   uint32
	dir  uint32
	kind endpointWorkerKind
}

type endpointWaitResult uint8

const (
	endpointWaitDeadline endpointWaitResult = iota
	endpointWaitWake
	endpointWaitCancelled
)

type endpointClock interface {
	Now() time.Time
	WaitUntil(
		ctx context.Context,
		wake <-chan struct{},
		timer *time.Timer,
		deadline time.Time,
	) endpointWaitResult
}

type realtimeEndpointClock struct{}

func (realtimeEndpointClock) Now() time.Time { return time.Now() }

func (realtimeEndpointClock) WaitUntil(
	ctx context.Context,
	wake <-chan struct{},
	timer *time.Timer,
	deadline time.Time,
) endpointWaitResult {
	wait := time.Until(deadline)
	if wait <= 0 {
		return endpointWaitDeadline
	}
	resetReusableTimer(timer, wait)
	select {
	case <-ctx.Done():
		return endpointWaitCancelled
	case <-wake:
		return endpointWaitWake
	case <-timer.C:
		return endpointWaitDeadline
	}
}

// These optional fast-path interfaces deliberately use build-into operations.
// A device which implements them is sampled synchronously at its endpoint's
// service opportunity without a nested channel wait or a per-request context.
type interruptInBuilder interface {
	BuildInputReportInto(destination []byte) int
}

// interruptInClaimer is the legacy USB/IP-only claim seam. New schedulers use
// inputpresentation.Source so the same immutable claim can be consumed by any
// backend. Keep this fallback while non-DualSense devices migrate.
type interruptInClaimer interface {
	ClaimInputReport(destination []byte) (n int, token uint64)
	CompleteInputReport(token uint64, presented bool)
}

// inputReportSnapshotter provides a versioned, nonallocating control
// GET_REPORT snapshot. The response writer validates the version only after
// acquiring serialized send ownership, preventing an old control snapshot
// from being emitted after a newer interrupt presentation.
type inputReportSnapshotter interface {
	SnapshotInputReportInto(destination []byte) (n int, presentationVersion uint64)
	InputReportSnapshotCurrent(presentationVersion uint64) bool
}

type microphonePacketReader interface {
	TryReadMicrophonePacket(destination []byte) (n int, ok bool)
}

// isoOutGenerationDevice closes the check-to-callback race at endpoint reset.
// The command reader captures the media generation while admitting the owned
// job, and the endpoint worker asks the device to consume only that generation
// at its eventual service boundary.
type isoOutGenerationDevice interface {
	IsoOutGeneration(endpoint uint8) uint64
	HandleIsoOutTransfer(endpoint uint8, generation uint64, payload []byte) bool
}

type endpointJob struct {
	seq             uint32
	xferLen         uint32
	serviceAt       time.Time
	serviceEnd      time.Time
	duration        time.Duration
	queuedAt        time.Time
	generation      uint64
	mediaGeneration uint64

	payload []byte
	packets []usbip.IsoPacketDescriptor

	cancelled         bool
	responseStarted   bool
	sideEffectStarted bool
}

type endpointWorkerTelemetry struct {
	enqueued   atomic.Uint64
	completed  atomic.Uint64
	unlinked   atomic.Uint64
	overflow   atomic.Uint64
	reanchored atomic.Uint64
	queueAge   durationHistogram
	lateness   durationHistogram
}

// USBIPEndpointDiagnostics is a point-in-time, allocation-bearing diagnostic
// snapshot. It is never constructed on endpoint hot paths.
type USBIPEndpointDiagnostics struct {
	Endpoint       uint32
	Direction      uint32
	Kind           string
	Generation     uint64
	QueueDepth     int
	QueueHighWater int
	Enqueued       uint64
	Completed      uint64
	Unlinked       uint64
	Overflow       uint64
	Reanchored     uint64
	QueueAge       DurationHistogramSnapshot
	Lateness       DurationHistogramSnapshot
}

func (kind endpointWorkerKind) String() string {
	switch kind {
	case interruptInWorker:
		return "interrupt-in"
	case isoInWorker:
		return "iso-in"
	case isoOutWorker:
		return "iso-out"
	case genericInWorker:
		return "generic-in"
	default:
		return "unknown"
	}
}

type endpointWorker struct {
	ctx       context.Context
	dev       usbdesc.Device
	ep        uint32
	dir       uint32
	kind      endpointWorkerKind
	interval  time.Duration
	maxPacket int
	responses *responseWriter
	fail      func(error)
	clock     endpointClock

	mu         sync.Mutex
	slots      [endpointQueueCapacity]endpointJob
	order      [endpointQueueCapacity]int
	orderHead  int
	orderCount int
	free       [endpointQueueCapacity]int
	freeCount  int
	inFlight   int
	generation uint64
	cursor     time.Time
	highWater  int

	wake chan struct{}
	done chan struct{}

	reportBuffer   []byte
	mediaBuffer    []byte
	responseBuffer []byte
	lastResponse   []byte
	actualLengths  [maxIsoPackets]uint32
	payloadSlab    []byte
	packetSlab     []usbip.IsoPacketDescriptor

	telemetry endpointWorkerTelemetry
}

func (w *endpointWorker) snapshot() USBIPEndpointDiagnostics {
	w.mu.Lock()
	depth := w.orderCount
	if w.inFlight >= 0 {
		depth++
	}
	generation := w.generation
	highWater := w.highWater
	w.mu.Unlock()
	return USBIPEndpointDiagnostics{
		Endpoint:       w.ep,
		Direction:      w.dir,
		Kind:           w.kind.String(),
		Generation:     generation,
		QueueDepth:     depth,
		QueueHighWater: highWater,
		Enqueued:       w.telemetry.enqueued.Load(),
		Completed:      w.telemetry.completed.Load(),
		Unlinked:       w.telemetry.unlinked.Load(),
		Overflow:       w.telemetry.overflow.Load(),
		Reanchored:     w.telemetry.reanchored.Load(),
		QueueAge:       w.telemetry.queueAge.snapshot(),
		Lateness:       w.telemetry.lateness.snapshot(),
	}
}

func newEndpointWorker(
	ctx context.Context,
	dev usbdesc.Device,
	ep, dir uint32,
	kind endpointWorkerKind,
	interval time.Duration,
	maxPacket int,
	responses *responseWriter,
	fail func(error),
) *endpointWorker {
	return newEndpointWorkerWithClock(
		ctx, dev, ep, dir, kind, interval, maxPacket, responses, fail,
		realtimeEndpointClock{},
	)
}

func newEndpointWorkerWithClock(
	ctx context.Context,
	dev usbdesc.Device,
	ep, dir uint32,
	kind endpointWorkerKind,
	interval time.Duration,
	maxPacket int,
	responses *responseWriter,
	fail func(error),
	clock endpointClock,
) *endpointWorker {
	if interval <= 0 {
		interval = time.Millisecond
	}
	if clock == nil {
		clock = realtimeEndpointClock{}
	}
	w := &endpointWorker{
		ctx:        ctx,
		dev:        dev,
		ep:         ep,
		dir:        dir,
		kind:       kind,
		interval:   interval,
		maxPacket:  maxPacket,
		responses:  responses,
		fail:       fail,
		clock:      clock,
		inFlight:   -1,
		generation: 1,
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	for i := range w.free {
		w.free[i] = len(w.free) - 1 - i
	}
	w.freeCount = len(w.free)
	if maxPacket > 0 {
		w.reportBuffer = make([]byte, maxPacket)
		responseCapacity := retSubmitHeaderSize + maxPacket
		switch kind {
		case isoInWorker:
			mediaCapacity := maxPacket * preallocatedIsoPacketCount
			w.mediaBuffer = make([]byte, 0, mediaCapacity)
			responseCapacity = retSubmitHeaderSize + mediaCapacity +
				preallocatedIsoPacketCount*isoPacketDescriptorSize
		case isoOutWorker:
			responseCapacity = retSubmitHeaderSize +
				preallocatedIsoPacketCount*isoPacketDescriptorSize
		}
		w.responseBuffer = make([]byte, 0, responseCapacity)
	}
	if kind == isoInWorker || kind == isoOutWorker {
		w.packetSlab = make([]usbip.IsoPacketDescriptor,
			endpointQueueCapacity*preallocatedIsoPacketCount)
		for slotIndex := range w.slots {
			start := slotIndex * preallocatedIsoPacketCount
			w.slots[slotIndex].packets = w.packetSlab[start : start : start+preallocatedIsoPacketCount]
		}
	}
	if kind == isoOutWorker && maxPacket > 0 {
		bytesPerSlot := maxPacket * preallocatedIsoPacketCount
		w.payloadSlab = make([]byte, endpointQueueCapacity*bytesPerSlot)
		for slotIndex := range w.slots {
			start := slotIndex * bytesPerSlot
			w.slots[slotIndex].payload = w.payloadSlab[start : start : start+bytesPerSlot]
		}
	}
	go w.run()
	return w
}

func (w *endpointWorker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *endpointWorker) enqueue(
	seq, xferLen uint32,
	payload []byte,
	packets []usbip.IsoPacketDescriptor,
	now time.Time,
) bool {
	return w.enqueueWithGeneration(seq, xferLen, payload, packets, now, 0)
}

func (w *endpointWorker) enqueueWithGeneration(
	seq, xferLen uint32,
	payload []byte,
	packets []usbip.IsoPacketDescriptor,
	now time.Time,
	mediaGeneration uint64,
) bool {
	w.mu.Lock()
	if w.freeCount == 0 {
		w.telemetry.overflow.Add(1)
		w.mu.Unlock()
		return false
	}

	w.freeCount--
	idx := w.free[w.freeCount]
	job := &w.slots[idx]
	job.seq = seq
	job.xferLen = xferLen
	job.queuedAt = now
	job.generation = w.generation
	job.mediaGeneration = mediaGeneration
	job.cancelled = false
	job.responseStarted = false
	job.sideEffectStarted = false

	job.payload = resizeBytes(job.payload, len(payload))
	copy(job.payload, payload)
	job.packets = resizeIsoPackets(job.packets, len(packets))
	copy(job.packets, packets)

	duration := w.interval
	if w.kind != interruptInWorker {
		duration = time.Duration(len(packets)) * w.interval
	}
	job.duration = duration
	start := w.cursor
	if start.IsZero() || (w.interval > 0 && now.Sub(start) >= w.interval) {
		start = now
	}
	job.serviceAt = start
	job.serviceEnd = start.Add(duration)
	w.cursor = job.serviceEnd

	orderIndex := (w.orderHead + w.orderCount) % len(w.order)
	w.order[orderIndex] = idx
	w.orderCount++
	if w.orderCount > w.highWater {
		w.highWater = w.orderCount
	}
	w.telemetry.enqueued.Add(1)
	w.mu.Unlock()
	w.signal()
	return true
}

func resizeBytes(buffer []byte, size int) []byte {
	if cap(buffer) < size {
		return make([]byte, size)
	}
	return buffer[:size]
}

func resizeIsoPackets(
	buffer []usbip.IsoPacketDescriptor,
	size int,
) []usbip.IsoPacketDescriptor {
	if cap(buffer) < size {
		return make([]usbip.IsoPacketDescriptor, size)
	}
	return buffer[:size]
}

func (w *endpointWorker) unlink(seq uint32) bool {
	return w.unlinkAt(seq, time.Now())
}

func (w *endpointWorker) unlinkAt(seq uint32, now time.Time) bool {
	w.mu.Lock()
	if w.inFlight >= 0 {
		job := &w.slots[w.inFlight]
		if job.seq == seq && !job.responseStarted &&
			!job.sideEffectStarted && !job.cancelled {
			job.cancelled = true
			w.reflowQueueLocked(job.serviceAt, now)
			w.telemetry.unlinked.Add(1)
			w.mu.Unlock()
			w.signal()
			return true
		}
	}

	for position := 0; position < w.orderCount; position++ {
		orderIndex := (w.orderHead + position) % len(w.order)
		idx := w.order[orderIndex]
		job := &w.slots[idx]
		if job.seq != seq {
			continue
		}
		anchor := job.serviceAt
		w.removeQueuedLocked(position)
		w.releaseSlotLocked(idx)
		w.reflowQueueFromPositionLocked(position, anchor, now)
		w.telemetry.unlinked.Add(1)
		w.mu.Unlock()
		w.signal()
		return true
	}
	w.mu.Unlock()
	return false
}

func (w *endpointWorker) reset() {
	w.mu.Lock()
	w.generation++
	w.cursor = time.Time{}
	if w.inFlight >= 0 {
		w.slots[w.inFlight].cancelled = true
	}
	for w.orderCount > 0 {
		idx := w.popQueuedLocked()
		w.releaseSlotLocked(idx)
	}
	w.mu.Unlock()
	w.signal()
}

func (w *endpointWorker) popQueuedLocked() int {
	idx := w.order[w.orderHead]
	w.orderHead = (w.orderHead + 1) % len(w.order)
	w.orderCount--
	return idx
}

func (w *endpointWorker) removeQueuedLocked(position int) {
	for i := position; i < w.orderCount-1; i++ {
		to := (w.orderHead + i) % len(w.order)
		from := (w.orderHead + i + 1) % len(w.order)
		w.order[to] = w.order[from]
	}
	w.orderCount--
}

func (w *endpointWorker) releaseSlotLocked(idx int) {
	job := &w.slots[idx]
	job.seq = 0
	job.xferLen = 0
	job.serviceAt = time.Time{}
	job.serviceEnd = time.Time{}
	job.duration = 0
	job.queuedAt = time.Time{}
	job.generation = 0
	job.mediaGeneration = 0
	job.payload = job.payload[:0]
	job.packets = job.packets[:0]
	job.cancelled = false
	job.responseStarted = false
	job.sideEffectStarted = false
	w.free[w.freeCount] = idx
	w.freeCount++
}

func (w *endpointWorker) reflowQueueLocked(anchor time.Time, now time.Time) {
	w.reflowQueueFromPositionLocked(0, anchor, now)
}

func (w *endpointWorker) reflowQueueFromPositionLocked(
	position int,
	anchor time.Time,
	now time.Time,
) {
	if w.interval > 0 && now.Sub(anchor) >= w.interval {
		anchor = now
	}
	for i := position; i < w.orderCount; i++ {
		idx := w.order[(w.orderHead+i)%len(w.order)]
		job := &w.slots[idx]
		job.serviceAt = anchor
		job.serviceEnd = anchor.Add(job.duration)
		anchor = job.serviceEnd
	}
	w.cursor = anchor
}

func (w *endpointWorker) claimNext() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlight >= 0 {
		return w.inFlight
	}
	if w.orderCount == 0 {
		return -1
	}
	w.inFlight = w.popQueuedLocked()
	return w.inFlight
}

func (w *endpointWorker) finishCurrent(idx int, completed bool) {
	w.mu.Lock()
	if w.inFlight == idx {
		if completed {
			w.telemetry.completed.Add(1)
		}
		w.releaseSlotLocked(idx)
		w.inFlight = -1
	}
	w.mu.Unlock()
	w.signal()
}

func (w *endpointWorker) currentActive(idx int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlight != idx {
		return false
	}
	job := &w.slots[idx]
	return !job.cancelled && job.generation == w.generation
}

func (w *endpointWorker) markResponseStarted(idx int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlight != idx {
		return false
	}
	job := &w.slots[idx]
	if job.cancelled || job.generation != w.generation {
		return false
	}
	job.responseStarted = true
	return true
}

// beginSideEffect transfers ISO-OUT ownership from the cancellable pending
// queue to the endpoint worker immediately before the device callback. Once
// this succeeds, CMD_UNLINK reports already-completed success and the normal
// RET_SUBMIT remains due; an irreversible media/effect callback can no longer
// be mislabeled as cancelled.
func (w *endpointWorker) beginSideEffect(idx int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlight != idx {
		return false
	}
	job := &w.slots[idx]
	if job.cancelled || job.generation != w.generation {
		return false
	}
	job.sideEffectStarted = true
	return true
}

// phaseDeadline returns the absolute service deadline for a job phase. A
// phase is one interrupt opportunity, one ISO packet slot, or the ISO URB's
// completion boundary. Ordinary sub-interval jitter keeps the planned clock;
// lateness of a complete interval re-anchors this and all later reservations.
func (w *endpointWorker) phaseDeadline(idx, phase int, now time.Time) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlight != idx {
		return time.Time{}, false
	}
	job := &w.slots[idx]
	if job.cancelled || job.generation != w.generation {
		return time.Time{}, false
	}
	deadline := job.serviceAt.Add(time.Duration(phase) * w.interval)
	if lateness := now.Sub(deadline); lateness > 0 {
		w.telemetry.lateness.record(lateness)
	}
	if w.interval > 0 && now.Sub(deadline) >= w.interval {
		shift := now.Sub(deadline)
		job.serviceAt = job.serviceAt.Add(shift)
		job.serviceEnd = job.serviceEnd.Add(shift)
		deadline = now
		w.reflowQueueLocked(job.serviceEnd, now)
		w.telemetry.reanchored.Add(1)
	}
	return deadline, true
}

func resetReusableTimer(timer *time.Timer, wait time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(wait)
}

func (w *endpointWorker) waitForPhase(timer *time.Timer, idx, phase int) bool {
	for {
		deadline, active := w.phaseDeadline(idx, phase, w.clock.Now())
		if !active {
			return false
		}
		switch w.clock.WaitUntil(w.ctx, w.wake, timer, deadline) {
		case endpointWaitCancelled:
			return false
		case endpointWaitWake:
			// Unlink, reset, and newly reserved work all share this event. Recheck
			// the claimed job and its possibly compacted absolute deadline.
			continue
		case endpointWaitDeadline:
			// Timer delivery itself may be delayed by scheduler jitter. Re-evaluate
			// against the actual wake time so a delay of one complete interval
			// re-anchors later reservations instead of replaying an expired slot.
			wokeAt := w.clock.Now()
			adjusted, active := w.phaseDeadline(idx, phase, wokeAt)
			if !active {
				return false
			}
			if wokeAt.Before(adjusted) {
				continue
			}
			return w.currentActive(idx)
		}
	}
}

func (w *endpointWorker) run() {
	defer close(w.done)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		if w.ctx.Err() != nil {
			return
		}
		idx := w.claimNext()
		if idx < 0 {
			select {
			case <-w.ctx.Done():
				return
			case <-w.wake:
				continue
			}
		}

		var completed bool
		switch w.kind {
		case interruptInWorker:
			completed = w.processInterruptIn(timer, idx)
		case isoInWorker:
			completed = w.processIsoIn(timer, idx)
		case isoOutWorker:
			completed = w.processIsoOut(timer, idx)
		case genericInWorker:
			completed = w.processGenericIn(idx)
		}
		w.finishCurrent(idx, completed)
	}
}

func (w *endpointWorker) processGenericIn(idx int) bool {
	w.mu.Lock()
	job := &w.slots[idx]
	seq := job.seq
	xferLen := int(job.xferLen)
	queuedAt := job.queuedAt
	w.mu.Unlock()
	w.telemetry.queueAge.record(w.clock.Now().Sub(queuedAt))

	response := w.dev.HandleTransfer(w.ctx, w.ep, w.dir, nil)
	if w.ctx.Err() != nil {
		return false
	}
	if len(response) > xferLen {
		response = response[:xferLen]
	}
	w.responseBuffer = buildRetSubmitPacket(
		w.responseBuffer, seq, 0, uint32(len(response)), response, nil, false,
	)
	readyAt := time.Now()
	written, err := w.responses.writeIf(
		w.responseBuffer, true, readyAt,
		func() bool { return w.markResponseStarted(idx) },
	)
	if err != nil {
		w.fail(fmt.Errorf("IN endpoint %d seq %d: %w", w.ep, seq, err))
		return false
	}
	return written
}

func (w *endpointWorker) processInterruptIn(timer *time.Timer, idx int) bool {
	if !w.waitForPhase(timer, idx, 0) {
		return false
	}

	w.mu.Lock()
	job := &w.slots[idx]
	seq := job.seq
	xferLen := int(job.xferLen)
	queuedAt := job.queuedAt
	w.mu.Unlock()
	w.telemetry.queueAge.record(w.clock.Now().Sub(queuedAt))

	var response []byte
	presenter, presentationClaimed := w.dev.(inputpresentation.Source)
	claimer, legacyClaimed := w.dev.(interruptInClaimer)
	var presentationClaim inputpresentation.Claim
	var presentationClaimFits bool
	var claimToken uint64
	if presentationClaimed {
		destination := w.reportBuffer
		if xferLen < len(destination) {
			destination = destination[:xferLen]
		}
		presentationClaim = presenter.ClaimInputPresentation(
			destination, time.Now())
		n := presentationClaim.Size
		presentationClaimFits = presentationClaim.Valid() &&
			n <= len(destination)
		if !presentationClaimFits {
			n = 0
		}
		response = destination[:n]
	} else if legacyClaimed {
		destination := w.reportBuffer
		if xferLen < len(destination) {
			destination = destination[:xferLen]
		}
		n, token := claimer.ClaimInputReport(destination)
		claimToken = token
		if n < 0 {
			n = 0
		}
		n = min(n, len(destination))
		response = destination[:n]
	} else if builder, ok := w.dev.(interruptInBuilder); ok {
		destination := w.reportBuffer
		if xferLen < len(destination) {
			destination = destination[:xferLen]
		}
		n := builder.BuildInputReportInto(destination)
		if n < 0 {
			n = 0
		}
		n = min(n, len(destination))
		response = destination[:n]
	} else {
		attemptCtx, cancel := context.WithTimeout(w.ctx, w.interval)
		deviceResponse := w.dev.HandleTransfer(attemptCtx, w.ep, w.dir, nil)
		expired := deviceResponse == nil && errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
		cancel()
		if w.ctx.Err() != nil {
			return false
		}
		if deviceResponse != nil {
			response = deviceResponse
			w.lastResponse = resizeBytes(w.lastResponse, len(deviceResponse))
			copy(w.lastResponse, deviceResponse)
		} else if expired && len(w.lastResponse) > 0 {
			response = w.lastResponse
		}
		if len(response) > xferLen {
			response = response[:xferLen]
		}
	}

	w.responseBuffer = buildRetSubmitPacket(
		w.responseBuffer, seq, 0, uint32(len(response)), response, nil, false,
	)
	readyAt := time.Now()
	written, err := w.responses.writeIfThen(
		w.responseBuffer, true, readyAt,
		func() bool { return w.markResponseStarted(idx) },
		func(success bool) {
			if presentationClaimed && presentationClaim.Valid() {
				outcome := inputpresentation.OutcomeDefer
				if success && presentationClaimFits {
					outcome = inputpresentation.OutcomeCommit
				}
				presenter.ResolveInputPresentation(
					presentationClaim, outcome, time.Now())
			} else if legacyClaimed {
				claimer.CompleteInputReport(claimToken, success)
			}
		},
	)
	if err != nil {
		w.fail(fmt.Errorf("interrupt-IN endpoint %d seq %d: %w", w.ep, seq, err))
		return false
	}
	return written
}

func (w *endpointWorker) processIsoIn(timer *time.Timer, idx int) bool {
	w.mu.Lock()
	job := &w.slots[idx]
	seq := job.seq
	xferLen := int(job.xferLen)
	packetCount := len(job.packets)
	queuedAt := job.queuedAt
	w.mu.Unlock()
	w.telemetry.queueAge.record(w.clock.Now().Sub(queuedAt))

	w.mediaBuffer = resizeBytes(w.mediaBuffer, xferLen)
	used := 0
	clear(w.actualLengths[:packetCount])
	reader, fastPath := w.dev.(microphonePacketReader)

	for packetIndex := 0; packetIndex < packetCount; packetIndex++ {
		if !w.waitForPhase(timer, idx, packetIndex) {
			return false
		}
		w.mu.Lock()
		packetLength := int(w.slots[idx].packets[packetIndex].Length)
		w.mu.Unlock()
		if packetLength <= 0 {
			continue
		}
		if used+packetLength > len(w.mediaBuffer) {
			packetLength = len(w.mediaBuffer) - used
			if packetLength <= 0 {
				break
			}
		}
		destination := w.mediaBuffer[used : used+packetLength]
		var n int
		if fastPath {
			var ok bool
			n, ok = reader.TryReadMicrophonePacket(destination)
			if n < 0 {
				n = 0
			}
			n = min(n, len(destination))
			if !ok {
				if n == 0 {
					n = len(destination)
				}
				clear(destination[:n])
			}
		} else {
			attemptCtx, cancel := context.WithTimeout(w.ctx, w.interval)
			packetData := w.dev.HandleTransfer(attemptCtx, w.ep, w.dir, nil)
			cancel()
			if w.ctx.Err() != nil {
				return false
			}
			if len(packetData) == 0 {
				n = len(destination)
				clear(destination)
			} else {
				n = min(len(packetData), len(destination))
				copy(destination, packetData[:n])
			}
		}
		w.actualLengths[packetIndex] = uint32(n)
		used += n
	}

	// The completion boundary is one interval after the final packet slot.
	// This uses the same timer as every other service point in the worker.
	if !w.waitForPhase(timer, idx, packetCount) {
		return false
	}
	w.mu.Lock()
	job = &w.slots[idx]
	for i := range job.packets {
		job.packets[i].ActualLength = min(job.packets[i].Length, w.actualLengths[i])
		job.packets[i].Status = 0
	}
	completedPackets := job.packets
	w.mu.Unlock()

	w.responseBuffer = buildRetSubmitPacket(
		w.responseBuffer, seq, 0, uint32(used), w.mediaBuffer[:used], completedPackets, true,
	)
	readyAt := time.Now()
	written, err := w.responses.writeIf(
		w.responseBuffer, true, readyAt,
		func() bool { return w.markResponseStarted(idx) },
	)
	if err != nil {
		w.fail(fmt.Errorf("ISO-IN endpoint %d seq %d: %w", w.ep, seq, err))
		return false
	}
	return written
}

func (w *endpointWorker) processIsoOut(timer *time.Timer, idx int) bool {
	// Hand the owned time-indexed media block to the device at the start of its
	// reserved window. Waiting until the RET_SUBMIT completion boundary would
	// add an entire URB duration to haptics and trigger output.
	if !w.waitForPhase(timer, idx, 0) {
		return false
	}
	if !w.beginSideEffect(idx) {
		return false
	}

	w.mu.Lock()
	job := &w.slots[idx]
	seq := job.seq
	xferLen := job.xferLen
	packetCount := len(job.packets)
	payload := job.payload
	mediaGeneration := job.mediaGeneration
	queuedAt := job.queuedAt
	w.mu.Unlock()
	w.telemetry.queueAge.record(w.clock.Now().Sub(queuedAt))

	// The command reader has already deep-copied this payload and all packet
	// descriptors into the slot. Media processing therefore cannot alias its
	// receive scratch and cannot stop ingestion at a future audio deadline.
	if generationDevice, ok := w.dev.(isoOutGenerationDevice); ok {
		if !generationDevice.HandleIsoOutTransfer(
			uint8(w.ep&0x0f), mediaGeneration, payload,
		) {
			return false
		}
	} else {
		w.dev.HandleTransfer(w.ctx, w.ep, w.dir, payload)
	}

	// RET_SUBMIT represents completion of the whole reserved packet window.
	// Media processing is already off the command reader, so this absolute wait
	// affects only this endpoint worker and uses its one reusable timer.
	if !w.waitForPhase(timer, idx, packetCount) {
		return false
	}

	w.mu.Lock()
	job = &w.slots[idx]
	for i := range job.packets {
		packet := &job.packets[i]
		packet.ActualLength = 0
		if packet.Offset < xferLen {
			packet.ActualLength = min(packet.Length, xferLen-packet.Offset)
		}
		packet.Status = 0
	}
	completedPackets := job.packets
	w.mu.Unlock()

	w.responseBuffer = buildRetSubmitPacket(
		w.responseBuffer, seq, 0, xferLen, nil, completedPackets, true,
	)
	readyAt := time.Now()
	written, err := w.responses.writeIf(
		w.responseBuffer, true, readyAt,
		func() bool { return w.markResponseStarted(idx) },
	)
	if err != nil {
		w.fail(fmt.Errorf("ISO-OUT endpoint %d seq %d: %w", w.ep, seq, err))
		return false
	}
	return written
}

type endpointSchedulers struct {
	ctx       context.Context
	cancel    context.CancelFunc
	dev       usbdesc.Device
	desc      *usbdesc.Descriptor
	responses *responseWriter
	conn      net.Conn

	workersMu sync.Mutex
	workers   map[endpointWorkerKey]*endpointWorker
	// These arrays are populated completely during construction and immutable
	// once the schedulers are published to the command reader. Descriptor-known
	// DualSense/Edge admission therefore never takes the connection-wide map
	// lock used only by legacy lazy creation, lifecycle, and diagnostics.
	fastInterruptIn [16]*endpointWorker
	fastIsoIn       [16]*endpointWorker
	fastIsoOut      [16]*endpointWorker
	failOnce        sync.Once
	failErr         atomic.Pointer[error]
}

func newEndpointSchedulers(
	parent context.Context,
	dev usbdesc.Device,
	responses *responseWriter,
	conn net.Conn,
) *endpointSchedulers {
	ctx, cancel := context.WithCancel(parent)
	schedulers := &endpointSchedulers{
		ctx:       ctx,
		cancel:    cancel,
		dev:       dev,
		desc:      dev.GetDescriptor(),
		responses: responses,
		conn:      conn,
		workers:   make(map[endpointWorkerKey]*endpointWorker),
	}
	// DualSense and DualSense Edge expose explicit nonblocking endpoint APIs.
	// Pre-create every descriptor-known fast-path plane before command ingestion
	// so the first real URB does not allocate buffers/channels or start a worker.
	// Legacy devices without these capabilities retain lazy generic workers.
	schedulers.precreateFastPathWorkers()
	return schedulers
}

func (s *endpointSchedulers) precreateFastPathWorkers() {
	if s == nil || s.desc == nil || s.dev == nil {
		return
	}
	_, claimsInterrupt := s.dev.(interruptInClaimer)
	_, buildsInterrupt := s.dev.(interruptInBuilder)
	_, presentsInterrupt := s.dev.(inputpresentation.Source)
	_, readsMicrophone := s.dev.(microphonePacketReader)
	_, handlesIsoOutGeneration := s.dev.(isoOutGenerationDevice)
	seen := make(map[endpointWorkerKey]struct{}, 3)
	for ifaceIndex := range s.desc.Interfaces {
		iface := &s.desc.Interfaces[ifaceIndex]
		for endpointIndex := range iface.Endpoints {
			endpoint := &iface.Endpoints[endpointIndex]
			ep := uint32(endpoint.BEndpointAddress & 0x0f)
			dir := uint32(usbip.DirOut)
			if endpoint.BEndpointAddress&0x80 != 0 {
				dir = usbip.DirIn
			}
			var (
				kind endpointWorkerKind
				fast bool
			)
			switch endpoint.BMAttributes & 0x03 {
			case 0x03:
				kind = interruptInWorker
				fast = dir == usbip.DirIn &&
					(claimsInterrupt || buildsInterrupt || presentsInterrupt)
			case 0x01:
				if dir == usbip.DirIn {
					kind = isoInWorker
					fast = readsMicrophone
				} else {
					kind = isoOutWorker
					fast = handlesIsoOutGeneration
				}
			}
			key := endpointWorkerKey{ep: ep, dir: dir, kind: kind}
			if !fast {
				continue
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			worker := s.worker(ep, dir, kind)
			if ep >= 16 {
				continue
			}
			switch kind {
			case interruptInWorker:
				s.fastInterruptIn[ep] = worker
			case isoInWorker:
				s.fastIsoIn[ep] = worker
			case isoOutWorker:
				s.fastIsoOut[ep] = worker
			}
		}
	}
}

func (s *endpointSchedulers) fail(err error) {
	if err == nil {
		return
	}
	s.failOnce.Do(func() {
		errCopy := err
		s.failErr.Store(&errCopy)
		s.cancel()
		if s.conn != nil {
			_ = s.conn.Close()
		}
	})
}

func (s *endpointSchedulers) failure() error {
	if value := s.failErr.Load(); value != nil {
		return *value
	}
	return nil
}

func (s *endpointSchedulers) worker(ep, dir uint32, kind endpointWorkerKind) *endpointWorker {
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	key := endpointWorkerKey{ep: ep, dir: dir, kind: kind}
	if worker := s.workers[key]; worker != nil {
		return worker
	}
	descriptor, _ := findEndpointDescriptor(s.desc, ep, dir)
	interval := time.Millisecond
	maxPacket := 0
	if descriptor != nil {
		interval = usbServiceInterval(s.desc.Device.Speed, descriptor.BInterval)
		if capacity, err := endpointServiceCapacity(descriptor); err == nil {
			maxPacket = int(capacity)
		}
	}
	worker := newEndpointWorker(
		s.ctx, s.dev, ep, dir, kind, interval, maxPacket, s.responses, s.fail,
	)
	s.workers[key] = worker
	return worker
}

func (s *endpointSchedulers) workerList() []*endpointWorker {
	s.workersMu.Lock()
	workers := make([]*endpointWorker, 0, len(s.workers))
	for _, worker := range s.workers {
		workers = append(workers, worker)
	}
	s.workersMu.Unlock()
	return workers
}

// USBIPConnectionDiagnostics aggregates every endpoint plane sharing one
// serialized USB/IP response stream.
type USBIPConnectionDiagnostics struct {
	Endpoints []USBIPEndpointDiagnostics
	Response  USBIPResponseDiagnostics
}

func (s *endpointSchedulers) snapshot() USBIPConnectionDiagnostics {
	workers := s.workerList()
	snapshot := USBIPConnectionDiagnostics{
		Endpoints: make([]USBIPEndpointDiagnostics, 0, len(workers)),
		Response:  s.responses.snapshot(),
	}
	for _, worker := range workers {
		snapshot.Endpoints = append(snapshot.Endpoints, worker.snapshot())
	}
	return snapshot
}

func (s *endpointSchedulers) enqueueInterruptIn(seq, ep, xferLen uint32) bool {
	worker := (*endpointWorker)(nil)
	if ep < uint32(len(s.fastInterruptIn)) {
		worker = s.fastInterruptIn[ep]
	}
	if worker == nil {
		worker = s.worker(ep, usbip.DirIn, interruptInWorker)
	}
	return worker.enqueue(
		seq, xferLen, nil, nil, time.Now(),
	)
}

func (s *endpointSchedulers) enqueueGenericIn(seq, ep, xferLen uint32) bool {
	return s.worker(ep, usbip.DirIn, genericInWorker).enqueue(
		seq, xferLen, nil, nil, time.Now(),
	)
}

func (s *endpointSchedulers) enqueueIsoIn(
	seq, ep, xferLen uint32,
	packets []usbip.IsoPacketDescriptor,
) bool {
	worker := (*endpointWorker)(nil)
	if ep < uint32(len(s.fastIsoIn)) {
		worker = s.fastIsoIn[ep]
	}
	if worker == nil {
		worker = s.worker(ep, usbip.DirIn, isoInWorker)
	}
	return worker.enqueue(
		seq, xferLen, nil, packets, time.Now(),
	)
}

func (s *endpointSchedulers) enqueueIsoOut(
	seq, ep, xferLen uint32,
	payload []byte,
	packets []usbip.IsoPacketDescriptor,
) bool {
	var mediaGeneration uint64
	if generationDevice, ok := s.dev.(isoOutGenerationDevice); ok {
		mediaGeneration = generationDevice.IsoOutGeneration(uint8(ep & 0x0f))
	}
	worker := (*endpointWorker)(nil)
	if ep < uint32(len(s.fastIsoOut)) {
		worker = s.fastIsoOut[ep]
	}
	if worker == nil {
		worker = s.worker(ep, usbip.DirOut, isoOutWorker)
	}
	return worker.enqueueWithGeneration(
		seq, xferLen, payload, packets, time.Now(), mediaGeneration,
	)
}

func (s *endpointSchedulers) unlink(seq uint32) bool {
	for _, worker := range s.workerList() {
		if worker.unlink(seq) {
			return true
		}
	}
	return false
}

func (s *endpointSchedulers) resetEndpoint(endpointAddress uint8) {
	ep := uint32(endpointAddress & 0x0f)
	dir := uint32(usbip.DirOut)
	if endpointAddress&0x80 != 0 {
		dir = usbip.DirIn
	}
	for _, worker := range s.workerList() {
		if worker.ep == ep && worker.dir == dir {
			worker.reset()
		}
	}
}

func (s *endpointSchedulers) resetInterface(interfaceNumber uint8) {
	for _, iface := range s.desc.Interfaces {
		if iface.Descriptor.BInterfaceNumber != interfaceNumber {
			continue
		}
		for _, endpoint := range iface.Endpoints {
			s.resetEndpoint(endpoint.BEndpointAddress)
		}
	}
}

func (s *endpointSchedulers) resetAll() {
	for _, worker := range s.workerList() {
		worker.reset()
	}
}

func (s *endpointSchedulers) close() {
	s.cancel()
	workers := s.workerList()
	for _, worker := range workers {
		worker.signal()
	}
	for _, worker := range workers {
		<-worker.done
	}
}

func findEndpointDescriptor(
	desc *usbdesc.Descriptor,
	ep, dir uint32,
) (*usbdesc.EndpointDescriptor, bool) {
	if desc == nil || ep == 0 {
		return nil, false
	}
	address := uint8(ep & 0x0f)
	if dir == usbip.DirIn {
		address |= 0x80
	}
	for ifaceIndex := range desc.Interfaces {
		for endpointIndex := range desc.Interfaces[ifaceIndex].Endpoints {
			endpoint := &desc.Interfaces[ifaceIndex].Endpoints[endpointIndex]
			if endpoint.BEndpointAddress == address {
				return endpoint, true
			}
		}
	}
	return nil, false
}

func validateIsoSubmission(
	desc *usbdesc.Descriptor,
	ep, dir, xferLen uint32,
	packets []usbip.IsoPacketDescriptor,
) error {
	endpoint, found := findEndpointDescriptor(desc, ep, dir)
	if !found || endpoint.BMAttributes&0x03 != 0x01 {
		return fmt.Errorf("endpoint 0x%02x is not an isochronous %s endpoint",
			uint8(ep)|map[bool]uint8{true: 0x80}[dir == usbip.DirIn],
			map[bool]string{true: "IN", false: "OUT"}[dir == usbip.DirIn])
	}
	if xferLen > maximumTransferSize {
		return fmt.Errorf("ISO transfer length %d exceeds limit %d", xferLen, maximumTransferSize)
	}
	serviceCapacity, err := endpointServiceCapacity(endpoint)
	if err != nil {
		return err
	}
	var compactLength uint64
	var previousEnd uint64
	for index, packet := range packets {
		if packet.Length == 0 {
			return fmt.Errorf("ISO packet %d has zero length", index)
		}
		if packet.Length > serviceCapacity {
			return fmt.Errorf("ISO packet %d length %d exceeds endpoint service capacity %d",
				index, packet.Length, serviceCapacity)
		}
		end := uint64(packet.Offset) + uint64(packet.Length)
		if end > uint64(xferLen) {
			return fmt.Errorf("ISO packet %d range [%d,%d) exceeds transfer length %d",
				index, packet.Offset, end, xferLen)
		}
		if uint64(packet.Offset) < previousEnd {
			return fmt.Errorf("ISO packet %d overlaps or precedes the prior packet", index)
		}
		previousEnd = end
		compactLength += uint64(packet.Length)
		if compactLength > uint64(xferLen) {
			return fmt.Errorf("ISO compact payload length %d exceeds transfer length %d",
				compactLength, xferLen)
		}
	}
	return nil
}

func validateInterruptSubmission(
	desc *usbdesc.Descriptor,
	ep, dir, xferLen uint32,
) error {
	endpoint, found := findEndpointDescriptor(desc, ep, dir)
	if !found || endpoint.BMAttributes&0x03 != 0x03 {
		return fmt.Errorf("endpoint %d is not an interrupt endpoint", ep)
	}
	if _, err := endpointServiceCapacity(endpoint); err != nil {
		return err
	}
	// transfer_buffer_length is host receive capacity, not a promise that one
	// interrupt transaction carries that many bytes. usbip-win commonly asks
	// for 255 bytes on a 64-byte HID endpoint. The worker's report buffer stays
	// fixed to wMaxPacketSize and the serialized response is capped there.
	if xferLen == 0 || xferLen > maximumTransferSize {
		return fmt.Errorf("interrupt transfer buffer length %d outside limit 1..%d",
			xferLen, maximumTransferSize)
	}
	return nil
}

func endpointServiceCapacity(endpoint *usbdesc.EndpointDescriptor) (uint32, error) {
	packetBytes := uint32(endpoint.WMaxPacketSize & 0x07ff)
	additionalTransactions := uint32((endpoint.WMaxPacketSize >> 11) & 0x03)
	if packetBytes == 0 {
		return 0, fmt.Errorf("endpoint has zero wMaxPacketSize")
	}
	if additionalTransactions == 3 {
		return 0, fmt.Errorf("endpoint uses reserved high-bandwidth transaction count")
	}
	return packetBytes * (additionalTransactions + 1), nil
}

func lifecycleResetFromSetup(schedulers *endpointSchedulers, setup []byte) {
	if len(setup) < 8 {
		return
	}
	bmRequestType := setup[0]
	bRequest := setup[1]
	wValue := uint16(setup[2]) | uint16(setup[3])<<8
	wIndex := uint16(setup[4]) | uint16(setup[5])<<8

	switch {
	case bmRequestType == usbReqTypeStandardToEndpoint &&
		bRequest == usbReqClearFeature && wValue == 0:
		schedulers.resetEndpoint(uint8(wIndex))
	case bmRequestType == usbReqTypeStandardFromInterface &&
		bRequest == usbReqSetInterface && uint8(wValue) == 0:
		schedulers.resetInterface(uint8(wIndex))
	case bmRequestType == usbReqTypeStandardToDevice &&
		bRequest == usbReqSetConfiguration:
		schedulers.resetAll()
	}
}

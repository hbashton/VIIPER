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
	interfaceNumber  uint8
	alternateSetting uint8
	ep               uint32
	dir              uint32
	kind             endpointWorkerKind
}

// endpointDescriptorBinding identifies one endpoint in one exact interface
// alternate setting. Endpoint addresses can be reused by different alternate
// settings with different transfer types, intervals, and capacities; address
// alone is therefore not sufficient worker identity.
type endpointDescriptorBinding struct {
	interfaceNumber  uint8
	alternateSetting uint8
	descriptor       *usbdesc.EndpointDescriptor
}

type endpointWaitResult uint8

const (
	endpointWaitDeadline endpointWaitResult = iota
	endpointWaitWake
	endpointWaitCancelled
)

type preparedAdmissionResult uint8

const (
	preparedAdmissionRetired preparedAdmissionResult = iota
	preparedAdmissionDeferred
	preparedAdmissionAccepted
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

// inputReportIDSnapshotter extends versioned EP0 serialization to devices with
// more than the legacy report ID 0x01. The request ID is bound into each copy;
// InputReportSnapshotCurrent still validates the device-wide presentation
// version immediately under response send ownership.
type inputReportIDSnapshotter interface {
	inputReportSnapshotter
	SupportsInputReportSnapshot(reportID uint8) bool
	SnapshotInputReportForIDInto(reportID uint8,
		destination []byte) (n int, presentationVersion uint64)
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

// finishCurrentWithRetention keeps a host request pending when source
// admission rejected bytes before any RET_SUBMIT was emitted. The first job
// remains endpoint-ordered and is retried at the next service opportunity;
// unlink/reset can still cancel it because responseStarted remains false.
func (w *endpointWorker) finishCurrentWithRetention(
	idx int, completed, retain bool,
) {
	w.mu.Lock()
	if w.inFlight == idx {
		job := &w.slots[idx]
		if retain && !job.cancelled && job.generation == w.generation {
			now := w.clock.Now()
			job.serviceAt = now.Add(w.interval)
			job.serviceEnd = job.serviceAt.Add(job.duration)
			w.reflowQueueLocked(job.serviceEnd, now)
			w.mu.Unlock()
			w.signal()
			return
		}
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
	return w.markResponseStartedWithAdmission(
		idx, nil, inputpresentation.Claim{}, false,
	)
}

// markResponseStartedWithAdmission is the one transfer boundary between a
// cancellable endpoint job and a response which owns stream ordering. The
// worker lock makes endpoint cancellation/generation validation, optional
// source admission, and responseStarted publication indivisible to unlink and
// reset. The response writer already owns its send lock when it invokes this
// method, so a true return is immediately followed by the first response byte.
func (w *endpointWorker) markResponseStartedWithAdmission(
	idx int,
	source inputpresentation.AdmissionSource,
	claim inputpresentation.Claim,
	requireAdmission bool,
) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlight != idx {
		return false
	}
	job := &w.slots[idx]
	if job.cancelled || job.generation != w.generation {
		return false
	}
	if requireAdmission && (source == nil ||
		!source.CanAdmitInputPresentation(claim, w.clock.Now())) {
		return false
	}
	job.responseStarted = true
	return true
}

// admitPreparedResponse is the only prepared-source copy boundary. Lock order
// is responseWriter.mu -> endpointWorker.mu -> source. Unlink/reset take and
// release endpointWorker.mu before they can enqueue a response, and lifecycle
// retirement is invoked only after worker reset releases that lock. Prepared
// sources must not call back into the transport, so this order has no cycle.
//
// A successful return transfers the job from cancellation ownership to the
// serialized response. The worker lock is released before socket I/O and final
// source resolution.
func (w *endpointWorker) admitPreparedResponse(
	idx int,
	source inputpresentation.PreparedSource,
	claim inputpresentation.Claim,
	destination []byte,
) preparedAdmissionResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlight != idx || w.ctx.Err() != nil {
		return preparedAdmissionRetired
	}
	job := &w.slots[idx]
	if job.cancelled || job.generation != w.generation || source == nil ||
		!source.OwnsInputPresentationEndpoint(uint8(w.ep)) {
		return preparedAdmissionRetired
	}
	if len(destination) != claim.Size || cap(destination) != claim.Size ||
		!source.AdmitAndCopyInputPresentation(
			claim, destination, w.clock.Now()) {
		clear(destination)
		return preparedAdmissionDeferred
	}
	job.responseStarted = true
	return preparedAdmissionAccepted
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

		var completed, retain bool
		switch w.kind {
		case interruptInWorker:
			completed, retain = w.processInterruptIn(timer, idx)
		case isoInWorker:
			completed = w.processIsoIn(timer, idx)
		case isoOutWorker:
			completed = w.processIsoOut(timer, idx)
		case genericInWorker:
			completed = w.processGenericIn(idx)
		}
		w.finishCurrentWithRetention(idx, completed, retain)
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

func (w *endpointWorker) processInterruptIn(
	timer *time.Timer, idx int,
) (bool, bool) {
	if !w.waitForPhase(timer, idx, 0) {
		return false, false
	}

	w.mu.Lock()
	job := &w.slots[idx]
	seq := job.seq
	xferLen := int(job.xferLen)
	queuedAt := job.queuedAt
	w.mu.Unlock()
	w.telemetry.queueAge.record(w.clock.Now().Sub(queuedAt))

	// One immediate retry lets a strict age fault replace the rejected claim
	// with its mandatory neutral on this same host URB. A source which continues
	// to reject is retained and retried on a later service opportunity rather
	// than spinning or orphaning the request.
	for attempt := 0; attempt < 2; attempt++ {
		var response []byte
		preparedPresenter, hasPreparedSource :=
			w.dev.(inputpresentation.PreparedSource)
		preparedClaimed := hasPreparedSource &&
			preparedPresenter.OwnsInputPresentationEndpoint(uint8(w.ep))
		presenter, hasPresentationSource := w.dev.(inputpresentation.Source)
		admissionSource, hasAdmissionSource :=
			w.dev.(inputpresentation.AdmissionSource)
		presentationClaimed := !preparedClaimed && hasPresentationSource &&
			presenter.OwnsInputPresentationEndpoint(uint8(w.ep))
		claimer, legacyClaimed := w.dev.(interruptInClaimer)
		// A modern Source owns endpoint routing for the whole composite device.
		// Falling back to its legacy, endpoint-agnostic compatibility methods on
		// an auxiliary endpoint would consume main-controller transitions.
		legacyClaimed = legacyClaimed && !hasPresentationSource &&
			!hasPreparedSource
		if preparedClaimed {
			maximumSize := min(xferLen, len(w.reportBuffer))
			selectedAt := w.clock.Now()
			claim, available := preparedPresenter.SelectInputPresentation(
				maximumSize, selectedAt)
			if !available {
				continue
			}
			if !claim.Valid() || claim.Size > maximumSize {
				preparedPresenter.ResolveInputPresentation(
					claim, inputpresentation.OutcomeRetire, w.clock.Now())
				w.fail(fmt.Errorf(
					"interrupt-IN endpoint %d seq %d: malformed prepared claim %+v for capacity %d",
					w.ep, seq, claim, maximumSize))
				return false, false
			}

			w.responseBuffer = buildPreparedRetSubmitPacket(
				w.responseBuffer, seq, claim.Size)
			destination := w.responseBuffer[retSubmitHeaderSize : retSubmitHeaderSize+claim.Size : retSubmitHeaderSize+claim.Size]
			admission := preparedAdmissionRetired
			resolved := false
			readyAt := time.Now()
			written, err := w.responses.writeIfThen(
				w.responseBuffer, true, readyAt,
				func() bool {
					admission = w.admitPreparedResponse(
						idx, preparedPresenter, claim, destination)
					return admission == preparedAdmissionAccepted
				},
				func(success bool) {
					outcome := inputpresentation.OutcomeRetire
					switch admission {
					case preparedAdmissionDeferred:
						outcome = inputpresentation.OutcomeDefer
					case preparedAdmissionAccepted:
						outcome = inputpresentation.OutcomeDefer
						if success {
							outcome = inputpresentation.OutcomeCommit
						}
					}
					resolved = preparedPresenter.ResolveInputPresentation(
						claim, outcome, w.clock.Now())
				},
			)
			if err != nil {
				w.fail(fmt.Errorf(
					"prepared interrupt-IN endpoint %d seq %d: %w",
					w.ep, seq, err))
				return false, false
			}
			if admission != preparedAdmissionRetired && !resolved {
				w.fail(fmt.Errorf(
					"prepared interrupt-IN endpoint %d seq %d: source rejected terminal resolution",
					w.ep, seq))
				return false, false
			}
			if written {
				return true, false
			}
			if w.ctx.Err() != nil {
				return false, false
			}
			if !w.currentActive(idx) {
				return false, false
			}
			continue
		}
		var presentationClaim inputpresentation.Claim
		var presentationClaimFits bool
		var claimToken uint64
		if presentationClaimed {
			destination := w.reportBuffer
			if xferLen < len(destination) {
				destination = destination[:xferLen]
			}
			presentationClaim = presenter.ClaimInputPresentation(
				destination, w.clock.Now())
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
		} else if builder, ok := w.dev.(interruptInBuilder); ok &&
			!hasPresentationSource && !hasPreparedSource {
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
			deviceResponse := w.dev.HandleTransfer(
				attemptCtx, w.ep, w.dir, nil)
			expired := deviceResponse == nil &&
				errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
			cancel()
			if w.ctx.Err() != nil {
				return false, false
			}
			if deviceResponse != nil {
				response = deviceResponse
				w.lastResponse = resizeBytes(
					w.lastResponse, len(deviceResponse))
				copy(w.lastResponse, deviceResponse)
			} else if expired && len(w.lastResponse) > 0 {
				response = w.lastResponse
			}
			if len(response) > xferLen {
				response = response[:xferLen]
			}
		}

		w.responseBuffer = buildRetSubmitPacket(
			w.responseBuffer, seq, 0, uint32(len(response)), response, nil,
			false,
		)
		readyAt := time.Now()
		written, err := w.responses.writeIfThen(
			w.responseBuffer, true, readyAt,
			func() bool {
				return w.markResponseStartedWithAdmission(
					idx, admissionSource, presentationClaim,
					presentationClaimed && presentationClaimFits &&
						hasAdmissionSource,
				)
			},
			func(success bool) {
				if presentationClaimed && presentationClaim.Valid() {
					outcome := inputpresentation.OutcomeDefer
					if success && presentationClaimFits {
						outcome = inputpresentation.OutcomeCommit
					}
					presenter.ResolveInputPresentation(
						presentationClaim, outcome, w.clock.Now())
				} else if legacyClaimed {
					claimer.CompleteInputReport(claimToken, success)
				}
			},
		)
		if err != nil {
			w.fail(fmt.Errorf(
				"interrupt-IN endpoint %d seq %d: %w", w.ep, seq, err))
			return false, false
		}
		if written {
			return true, false
		}
		if !w.currentActive(idx) {
			return false, false
		}
	}
	return false, true
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

	// workers is populated completely during construction and immutable once
	// the schedulers are published to the command reader. Hot-path lookup is a
	// concurrent read and takes no connection-wide lock.
	workersMu sync.Mutex // retained as a diagnostic/test contention sentinel
	workers   map[endpointWorkerKey]*endpointWorker
	// Legacy test/diagnostic aliases for the first descriptor occurrence.
	// Production admission resolves the exact active binding and ignores these.
	fastInterruptIn        [16]*endpointWorker
	fastIsoIn              [16]*endpointWorker
	fastIsoOut             [16]*endpointWorker
	presentationSource     inputpresentation.EndpointSource
	presentationGeneration uint64
	presentationMu         sync.Mutex
	presentationRetireOnce sync.Once
	failOnce               sync.Once
	failErr                atomic.Pointer[error]
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
	if source, ok := dev.(inputpresentation.PreparedSource); ok {
		schedulers.presentationSource = source
		schedulers.presentationGeneration =
			source.InputPresentationGeneration()
	} else if source, ok := dev.(inputpresentation.Source); ok {
		schedulers.presentationSource = source
		schedulers.presentationGeneration =
			source.InputPresentationGeneration()
	}
	// Pre-create every descriptor-known scheduled plane before command
	// ingestion. This makes interface/alternate identity immutable and keeps the
	// first real URB from allocating buffers/channels or starting a worker.
	schedulers.precreateEndpointWorkers()
	return schedulers
}

func (s *endpointSchedulers) precreateEndpointWorkers() {
	if s == nil || s.desc == nil || s.dev == nil {
		return
	}
	for ifaceIndex := range s.desc.Interfaces {
		iface := &s.desc.Interfaces[ifaceIndex]
		for endpointIndex := range iface.Endpoints {
			endpoint := &iface.Endpoints[endpointIndex]
			ep := uint32(endpoint.BEndpointAddress & 0x0f)
			dir := uint32(usbip.DirOut)
			if endpoint.BEndpointAddress&0x80 != 0 {
				dir = usbip.DirIn
			}
			var kind endpointWorkerKind
			scheduled := false
			switch endpoint.BMAttributes & 0x03 {
			case 0x03:
				if dir == usbip.DirIn {
					kind = interruptInWorker
					scheduled = true
				}
			case 0x01:
				if dir == usbip.DirIn {
					kind = isoInWorker
				} else {
					kind = isoOutWorker
				}
				scheduled = true
			default:
				if dir == usbip.DirIn {
					kind = genericInWorker
					scheduled = true
				}
			}
			if !scheduled {
				continue
			}
			binding := endpointDescriptorBinding{
				interfaceNumber:  iface.Descriptor.BInterfaceNumber,
				alternateSetting: iface.Descriptor.BAlternateSetting,
				descriptor:       endpoint,
			}
			key := endpointWorkerKeyFor(binding, dir, kind)
			if _, duplicate := s.workers[key]; duplicate {
				continue
			}
			worker := s.newWorker(binding, ep, dir, kind)
			s.workers[key] = worker
			if ep >= 16 {
				continue
			}
			switch kind {
			case interruptInWorker:
				if s.fastInterruptIn[ep] == nil {
					s.fastInterruptIn[ep] = worker
				}
			case isoInWorker:
				if s.fastIsoIn[ep] == nil {
					s.fastIsoIn[ep] = worker
				}
			case isoOutWorker:
				if s.fastIsoOut[ep] == nil {
					s.fastIsoOut[ep] = worker
				}
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

func endpointWorkerKeyFor(binding endpointDescriptorBinding, dir uint32,
	kind endpointWorkerKind) endpointWorkerKey {
	return endpointWorkerKey{
		interfaceNumber:  binding.interfaceNumber,
		alternateSetting: binding.alternateSetting,
		ep:               uint32(binding.descriptor.BEndpointAddress & 0x0f),
		dir:              dir,
		kind:             kind,
	}
}

func (s *endpointSchedulers) newWorker(binding endpointDescriptorBinding,
	ep, dir uint32, kind endpointWorkerKind) *endpointWorker {
	interval := time.Millisecond
	maxPacket := 0
	if binding.descriptor != nil {
		interval = usbServiceInterval(s.desc.Device.Speed,
			binding.descriptor.BInterval)
		if capacity, err := endpointServiceCapacity(
			binding.descriptor); err == nil {
			maxPacket = int(capacity)
		}
	}
	return newEndpointWorker(
		s.ctx, s.dev, ep, dir, kind, interval, maxPacket, s.responses, s.fail,
	)
}

func (s *endpointSchedulers) workerForBinding(
	binding endpointDescriptorBinding,
	dir uint32,
	kind endpointWorkerKind,
) *endpointWorker {
	if binding.descriptor == nil {
		return nil
	}
	return s.workers[endpointWorkerKeyFor(binding, dir, kind)]
}

// worker retains a test/legacy lookup by address. Production admission uses
// the exact active binding and never this first-descriptor convenience.
func (s *endpointSchedulers) worker(ep, dir uint32,
	kind endpointWorkerKind) *endpointWorker {
	binding, found := findEndpointBinding(s.desc, ep, dir)
	if !found {
		return nil
	}
	return s.workerForBinding(binding, dir, kind)
}

func (s *endpointSchedulers) workerList() []*endpointWorker {
	workers := make([]*endpointWorker, 0, len(s.workers))
	for _, worker := range s.workers {
		workers = append(workers, worker)
	}
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
	binding, found := findEndpointBinding(s.desc, ep, usbip.DirIn)
	return found && s.enqueueInterruptInBinding(seq, xferLen, binding)
}

func (s *endpointSchedulers) enqueueInterruptInBinding(
	seq, xferLen uint32,
	binding endpointDescriptorBinding,
) bool {
	worker := s.workerForBinding(binding, usbip.DirIn, interruptInWorker)
	if worker == nil {
		return false
	}
	return worker.enqueue(
		seq, xferLen, nil, nil, time.Now(),
	)
}

func (s *endpointSchedulers) enqueueGenericInBinding(
	seq, xferLen uint32,
	binding endpointDescriptorBinding,
) bool {
	worker := s.workerForBinding(binding, usbip.DirIn, genericInWorker)
	if worker == nil {
		return false
	}
	return worker.enqueue(
		seq, xferLen, nil, nil, time.Now(),
	)
}

func (s *endpointSchedulers) enqueueIsoIn(
	seq, ep, xferLen uint32,
	packets []usbip.IsoPacketDescriptor,
) bool {
	binding, found := findEndpointBinding(s.desc, ep, usbip.DirIn)
	return found && s.enqueueIsoInBinding(seq, xferLen, packets, binding)
}

func (s *endpointSchedulers) enqueueIsoInBinding(
	seq, xferLen uint32,
	packets []usbip.IsoPacketDescriptor,
	binding endpointDescriptorBinding,
) bool {
	worker := s.workerForBinding(binding, usbip.DirIn, isoInWorker)
	if worker == nil {
		return false
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
	binding, found := findEndpointBinding(s.desc, ep, usbip.DirOut)
	return found && s.enqueueIsoOutBinding(seq, xferLen, payload, packets,
		binding)
}

func (s *endpointSchedulers) enqueueIsoOutBinding(
	seq, xferLen uint32,
	payload []byte,
	packets []usbip.IsoPacketDescriptor,
	binding endpointDescriptorBinding,
) bool {
	var mediaGeneration uint64
	if generationDevice, ok := s.dev.(isoOutGenerationDevice); ok {
		mediaGeneration = generationDevice.IsoOutGeneration(
			binding.descriptor.BEndpointAddress & 0x0f)
	}
	worker := s.workerForBinding(binding, usbip.DirOut, isoOutWorker)
	if worker == nil {
		return false
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
	s.resetEndpointWorkers(endpointAddress)
	if s.ownsPresentationEndpoint(endpointAddress) {
		s.rotatePresentationGeneration(time.Now())
	}
}

func (s *endpointSchedulers) resetEndpointWorkers(endpointAddress uint8) {
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
	retirePresentation := false
	for _, iface := range s.desc.Interfaces {
		if iface.Descriptor.BInterfaceNumber != interfaceNumber {
			continue
		}
		for _, endpoint := range iface.Endpoints {
			if s.ownsPresentationEndpoint(endpoint.BEndpointAddress) {
				retirePresentation = true
				break
			}
		}
	}
	for _, iface := range s.desc.Interfaces {
		if iface.Descriptor.BInterfaceNumber != interfaceNumber {
			continue
		}
		for _, endpoint := range iface.Endpoints {
			s.resetEndpointWorkers(endpoint.BEndpointAddress)
		}
	}
	if retirePresentation {
		s.rotatePresentationGeneration(time.Now())
	}
}

func (s *endpointSchedulers) resetAll() {
	for _, worker := range s.workerList() {
		worker.reset()
	}
	s.rotatePresentationGeneration(time.Now())
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
	s.presentationRetireOnce.Do(func() {
		s.retirePresentationGeneration(time.Now(), false)
	})
}

func (s *endpointSchedulers) ownsPresentationEndpoint(
	endpointAddress uint8,
) bool {
	return endpointAddress&0x80 != 0 && s.presentationSource != nil &&
		s.presentationSource.OwnsInputPresentationEndpoint(
			endpointAddress&0x0f)
}

func (s *endpointSchedulers) rotatePresentationGeneration(retiredAt time.Time) {
	s.retirePresentationGeneration(retiredAt, true)
}

// retirePresentationGeneration serializes connection-level lifecycle
// ownership. Reset boundaries retire the captured generation and adopt the
// successor for later resets/close; final close retires only its current lease.
func (s *endpointSchedulers) retirePresentationGeneration(
	retiredAt time.Time, adoptSuccessor bool,
) {
	s.presentationMu.Lock()
	defer s.presentationMu.Unlock()
	if s.presentationSource == nil || s.presentationGeneration == 0 {
		return
	}
	if !s.presentationSource.RetireInputPresentationGeneration(
		s.presentationGeneration, retiredAt) {
		return
	}
	if adoptSuccessor {
		s.presentationGeneration =
			s.presentationSource.InputPresentationGeneration()
	}
}

func findEndpointDescriptor(
	desc *usbdesc.Descriptor,
	ep, dir uint32,
) (*usbdesc.EndpointDescriptor, bool) {
	binding, found := findEndpointBinding(desc, ep, dir)
	return binding.descriptor, found
}

func findEndpointBinding(
	desc *usbdesc.Descriptor,
	ep, dir uint32,
) (endpointDescriptorBinding, bool) {
	if desc == nil || ep == 0 {
		return endpointDescriptorBinding{}, false
	}
	address := uint8(ep & 0x0f)
	if dir == usbip.DirIn {
		address |= 0x80
	}
	for ifaceIndex := range desc.Interfaces {
		for endpointIndex := range desc.Interfaces[ifaceIndex].Endpoints {
			endpoint := &desc.Interfaces[ifaceIndex].Endpoints[endpointIndex]
			if endpoint.BEndpointAddress == address {
				return endpointDescriptorBinding{
					interfaceNumber: desc.Interfaces[ifaceIndex].Descriptor.
						BInterfaceNumber,
					alternateSetting: desc.Interfaces[ifaceIndex].Descriptor.
						BAlternateSetting,
					descriptor: endpoint,
				}, true
			}
		}
	}
	return endpointDescriptorBinding{}, false
}

func validateIsoSubmission(
	desc *usbdesc.Descriptor,
	ep, dir, xferLen uint32,
	packets []usbip.IsoPacketDescriptor,
) error {
	endpoint, found := findEndpointDescriptor(desc, ep, dir)
	if !found {
		return fmt.Errorf("endpoint 0x%02x is not an isochronous %s endpoint",
			uint8(ep)|map[bool]uint8{true: 0x80}[dir == usbip.DirIn],
			map[bool]string{true: "IN", false: "OUT"}[dir == usbip.DirIn])
	}
	return validateIsoEndpointSubmission(endpoint, xferLen, packets)
}

func validateIsoEndpointSubmission(
	endpoint *usbdesc.EndpointDescriptor,
	xferLen uint32,
	packets []usbip.IsoPacketDescriptor,
) error {
	if endpoint == nil || endpoint.BMAttributes&0x03 != 0x01 {
		return fmt.Errorf("active endpoint is not isochronous")
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
	if !found {
		return fmt.Errorf("endpoint %d is not an interrupt endpoint", ep)
	}
	return validateInterruptEndpointSubmission(endpoint, xferLen)
}

func validateInterruptEndpointSubmission(
	endpoint *usbdesc.EndpointDescriptor,
	xferLen uint32,
) error {
	if endpoint == nil || endpoint.BMAttributes&0x03 != 0x03 {
		return fmt.Errorf("active endpoint is not interrupt")
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

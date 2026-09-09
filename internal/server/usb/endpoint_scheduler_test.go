package usb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	usbdesc "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

type schedulerTestDevice struct {
	desc *usbdesc.Descriptor

	mu              sync.Mutex
	isoOutPayloads  [][]byte
	isoOutTimes     []time.Time
	isoOutWake      chan struct{}
	inputBuilds     int
	microphoneReads int
}

type generationSchedulerTestDevice struct {
	*schedulerTestDevice
	generation atomic.Uint64
	rejected   atomic.Uint64
}

type fakeEndpointClock struct {
	mu      sync.Mutex
	now     time.Time
	changed chan struct{}
	waiting chan time.Time
}

func newFakeEndpointClock(now time.Time) *fakeEndpointClock {
	return &fakeEndpointClock{
		now: now, changed: make(chan struct{}, 1), waiting: make(chan time.Time, 16),
	}
}

func (c *fakeEndpointClock) Now() time.Time {
	c.mu.Lock()
	now := c.now
	c.mu.Unlock()
	return now
}

func (c *fakeEndpointClock) set(now time.Time, signal bool) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
	if signal {
		select {
		case c.changed <- struct{}{}:
		default:
		}
	}
}

func (c *fakeEndpointClock) advance(delta time.Duration) {
	c.set(c.Now().Add(delta), true)
}

func (c *fakeEndpointClock) WaitUntil(
	ctx context.Context,
	wake <-chan struct{},
	_ *time.Timer,
	deadline time.Time,
) endpointWaitResult {
	for {
		// Consume an already-published clock change before testing the new time.
		// This prevents a wake signal that raced an endpoint signal from becoming
		// a stale wake for the following service slot.
		select {
		case <-c.changed:
		default:
		}
		now := c.Now()
		if !now.Before(deadline) {
			return endpointWaitDeadline
		}
		select {
		case c.waiting <- deadline:
		default:
		}
		select {
		case <-ctx.Done():
			return endpointWaitCancelled
		case <-wake:
			return endpointWaitWake
		case <-c.changed:
		}
	}
}

func (c *fakeEndpointClock) waitForDeadline(t *testing.T, wanted time.Time) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case observed := <-c.waiting:
			if observed.Equal(wanted) {
				return
			}
		case <-deadline.C:
			t.Fatalf("worker did not wait for deadline %s", wanted)
		}
	}
}

func TestRealtimeEndpointClockBlockedWakeAllocatesZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := realtimeEndpointClock{}
	wake := make(chan struct{})
	trigger := make(chan struct{})
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		for range trigger {
			wake <- struct{}{}
		}
	}()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	allocations := testing.AllocsPerRun(1000, func() {
		trigger <- struct{}{}
		if result := clock.WaitUntil(ctx, wake, timer, time.Now().Add(time.Hour)); result != endpointWaitWake {
			panic("unexpected endpoint clock result")
		}
	})
	close(trigger)
	<-senderDone
	require.Zero(t, allocations)
}

func TestRealtimeEndpointClockDeadlineAllocatesZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := realtimeEndpointClock{}
	wake := make(chan struct{}, 1)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	// This deliberately enters the reusable timer/select path. AllocsPerRun
	// warms the runtime select waiter cache before measuring steady state.
	allocations := testing.AllocsPerRun(100, func() {
		if result := clock.WaitUntil(ctx, wake, timer, time.Now().Add(50*time.Microsecond)); result != endpointWaitDeadline {
			panic("unexpected endpoint clock result")
		}
	})
	require.Zero(t, allocations)
}

func BenchmarkRealtimeEndpointClockBlockedWake(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := realtimeEndpointClock{}
	wake := make(chan struct{})
	trigger := make(chan struct{})
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		for range trigger {
			wake <- struct{}{}
		}
	}()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		trigger <- struct{}{}
		if result := clock.WaitUntil(ctx, wake, timer, time.Now().Add(time.Hour)); result != endpointWaitWake {
			b.Fatal("unexpected endpoint clock result")
		}
	}
	b.StopTimer()
	close(trigger)
	<-senderDone
}

type claimedSchedulerTestDevice struct {
	*schedulerTestDevice
	claimReady  chan struct{}
	claimOnce   sync.Once
	completion  chan bool
	claimCount  atomic.Uint64
	commitCount atomic.Uint64
	retryCount  atomic.Uint64
}

func newClaimedSchedulerTestDevice() *claimedSchedulerTestDevice {
	return &claimedSchedulerTestDevice{
		schedulerTestDevice: &schedulerTestDevice{desc: testCompositeDescriptor()},
		claimReady:          make(chan struct{}),
		completion:          make(chan bool, 4),
	}
}

func (d *claimedSchedulerTestDevice) ClaimInputReport(destination []byte) (int, uint64) {
	token := d.claimCount.Add(1)
	if len(destination) > 0 {
		destination[0] = 0xa5
	}
	d.claimOnce.Do(func() { close(d.claimReady) })
	return min(1, len(destination)), token
}

func (d *claimedSchedulerTestDevice) CompleteInputReport(_ uint64, presented bool) {
	if presented {
		d.commitCount.Add(1)
	} else {
		d.retryCount.Add(1)
	}
	select {
	case d.completion <- presented:
	default:
	}
}

type allocationFreeClaimDevice struct {
	*schedulerTestDevice
	token     uint64
	completed uint64
	presented uint64
}

type versionedInputTestDevice struct {
	*schedulerTestDevice
	inputMu       sync.Mutex
	lastReport    [64]byte
	claimedReport [64]byte
	version       uint64
	token         uint64
	snapshotCalls atomic.Uint64
	snapshotReady chan struct{}
	snapshotOnce  sync.Once
	completion    chan bool
}

func newVersionedInputTestDevice() *versionedInputTestDevice {
	descriptor := testCompositeDescriptor()
	descriptor.Configuration.BConfigurationValue = 1
	descriptor.Interfaces[0].Descriptor.BInterfaceNumber = 0
	descriptor.Interfaces[0].Descriptor.BInterfaceClass = usbInterfaceClassHID
	descriptor.Interfaces[1].Descriptor.BInterfaceNumber = 1
	descriptor.Interfaces[1].Descriptor.BInterfaceClass = 0xff
	device := &versionedInputTestDevice{
		schedulerTestDevice: &schedulerTestDevice{desc: descriptor},
		version:             1,
		snapshotReady:       make(chan struct{}),
		completion:          make(chan bool, 2),
	}
	device.lastReport[0] = 0x7f // previously presented press
	device.claimedReport[0] = 0 // pending release
	return device
}

func (d *versionedInputTestDevice) ClaimInputReport(destination []byte) (int, uint64) {
	d.inputMu.Lock()
	d.token++
	token := d.token
	n := min(len(destination), len(d.claimedReport))
	copy(destination[:n], d.claimedReport[:n])
	d.inputMu.Unlock()
	return n, token
}

func (d *versionedInputTestDevice) CompleteInputReport(_ uint64, presented bool) {
	d.inputMu.Lock()
	if presented {
		copy(d.lastReport[:], d.claimedReport[:])
		d.version++
	}
	d.inputMu.Unlock()
	d.completion <- presented
}

func (d *versionedInputTestDevice) SnapshotInputReportInto(
	destination []byte,
) (int, uint64) {
	d.inputMu.Lock()
	n := min(len(destination), len(d.lastReport))
	copy(destination[:n], d.lastReport[:n])
	version := d.version
	d.inputMu.Unlock()
	d.snapshotCalls.Add(1)
	d.snapshotOnce.Do(func() { close(d.snapshotReady) })
	return n, version
}

func (d *versionedInputTestDevice) InputReportSnapshotCurrent(version uint64) bool {
	d.inputMu.Lock()
	current := version == d.version
	d.inputMu.Unlock()
	return current
}

type versionedInputIDTestDevice struct {
	*versionedInputTestDevice
}

func (*versionedInputIDTestDevice) SupportsInputReportSnapshot(
	reportID uint8,
) bool {
	return reportID == 0 || reportID == 0x05 || reportID == 0x09
}

func (d *versionedInputIDTestDevice) SnapshotInputReportForIDInto(
	reportID uint8, destination []byte,
) (int, uint64) {
	d.inputMu.Lock()
	n := min(len(destination), len(d.lastReport))
	copy(destination[:n], d.lastReport[:n])
	if n > 0 && reportID != 0 {
		destination[0] = reportID
	}
	version := d.version
	d.inputMu.Unlock()
	d.snapshotCalls.Add(1)
	d.snapshotOnce.Do(func() { close(d.snapshotReady) })
	return n, version
}

type blockingIsoOutDevice struct {
	*generationSchedulerTestDevice
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *blockingIsoOutDevice) HandleIsoOutTransfer(
	endpoint uint8,
	generation uint64,
	payload []byte,
) bool {
	d.once.Do(func() { close(d.started) })
	<-d.release
	return d.generationSchedulerTestDevice.HandleIsoOutTransfer(
		endpoint, generation, payload,
	)
}

func (d *allocationFreeClaimDevice) ClaimInputReport(destination []byte) (int, uint64) {
	d.token++
	if len(destination) > 0 {
		destination[0] = byte(d.token)
	}
	return min(1, len(destination)), d.token
}

func (d *allocationFreeClaimDevice) CompleteInputReport(_ uint64, presented bool) {
	d.completed++
	if presented {
		d.presented++
	}
}

type discardResponseWriter struct{}

func (discardResponseWriter) Write(packet []byte) (int, error) {
	return len(packet), nil
}

type writerFunc func([]byte) (int, error)

func (fn writerFunc) Write(packet []byte) (int, error) { return fn(packet) }

func (d *generationSchedulerTestDevice) IsoOutGeneration(uint8) uint64 {
	return d.generation.Load()
}

func (d *generationSchedulerTestDevice) HandleIsoOutTransfer(
	_ uint8,
	generation uint64,
	payload []byte,
) bool {
	if generation != d.generation.Load() {
		d.rejected.Add(1)
		return false
	}
	d.HandleTransfer(context.Background(), 1, usbip.DirOut, payload)
	return true
}

func (d *schedulerTestDevice) HandleTransfer(
	_ context.Context,
	_ uint32,
	dir uint32,
	out []byte,
) []byte {
	if dir == usbip.DirOut {
		d.mu.Lock()
		d.isoOutPayloads = append(d.isoOutPayloads, append([]byte(nil), out...))
		d.isoOutTimes = append(d.isoOutTimes, time.Now())
		d.mu.Unlock()
		if d.isoOutWake != nil {
			select {
			case d.isoOutWake <- struct{}{}:
			default:
			}
		}
	}
	return nil
}

func (d *schedulerTestDevice) BuildInputReportInto(destination []byte) int {
	d.mu.Lock()
	d.inputBuilds++
	d.mu.Unlock()
	if len(destination) == 0 {
		return 0
	}
	destination[0] = 0x5a
	return 1
}

func (d *schedulerTestDevice) TryReadMicrophonePacket(destination []byte) (int, bool) {
	d.mu.Lock()
	d.microphoneReads++
	value := byte(d.microphoneReads)
	d.mu.Unlock()
	n := min(192, len(destination))
	for i := range destination[:n] {
		destination[i] = value
	}
	return n, true
}

func (d *schedulerTestDevice) GetDescriptor() *usbdesc.Descriptor { return d.desc }

func (d *schedulerTestDevice) GetDeviceSpecificArgs() map[string]any { return nil }

type recordedWrite struct {
	packet []byte
	at     time.Time
}

type recordingWriter struct {
	mu     sync.Mutex
	writes []recordedWrite
	wake   chan struct{}
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{wake: make(chan struct{}, 16)}
}

func (w *recordingWriter) Write(packet []byte) (int, error) {
	w.mu.Lock()
	w.writes = append(w.writes, recordedWrite{
		packet: append([]byte(nil), packet...),
		at:     time.Now(),
	})
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return len(packet), nil
}

func (w *recordingWriter) waitForWrites(t *testing.T, count int) []recordedWrite {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		w.mu.Lock()
		if len(w.writes) >= count {
			result := append([]recordedWrite(nil), w.writes...)
			w.mu.Unlock()
			return result
		}
		w.mu.Unlock()
		select {
		case <-w.wake:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %d writes", count)
		}
	}
}

func testCompositeDescriptor() *usbdesc.Descriptor {
	return &usbdesc.Descriptor{
		Device: usbdesc.DeviceDescriptor{Speed: 3},
		Interfaces: []usbdesc.InterfaceConfig{
			{Endpoints: []usbdesc.EndpointDescriptor{{
				BEndpointAddress: 0x84,
				BMAttributes:     0x03,
				WMaxPacketSize:   64,
				BInterval:        4,
			}}},
			{Endpoints: []usbdesc.EndpointDescriptor{{
				BEndpointAddress: 0x01,
				BMAttributes:     0x09,
				WMaxPacketSize:   192,
				BInterval:        4,
			}}},
		},
	}
}

func TestEndpointWorkerAbsoluteCursorAndLateReanchor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	device := &schedulerTestDevice{desc: testCompositeDescriptor()}
	w := newEndpointWorker(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, newResponseWriter(bytes.NewBuffer(nil), nil), func(error) {},
	)
	<-w.done

	base := time.Unix(100, 0)
	require.True(t, w.enqueue(1, 64, nil, nil, base))
	require.True(t, w.enqueue(2, 64, nil, nil, base))
	first := w.claimNext()

	deadline, active := w.phaseDeadline(first, 0, base.Add(999*time.Microsecond))
	require.True(t, active)
	require.Equal(t, base, deadline, "sub-interval jitter must preserve the absolute phase")

	deadline, active = w.phaseDeadline(first, 0, base.Add(time.Millisecond))
	require.True(t, active)
	require.Equal(t, base.Add(time.Millisecond), deadline)
	w.mu.Lock()
	second := &w.slots[w.order[w.orderHead]]
	require.Equal(t, base.Add(2*time.Millisecond), second.serviceAt,
		"an expired slot must not be replayed in a burst")
	w.mu.Unlock()
}

func TestInterruptWorkerFakeClockServicesOneUrbPerAbsoluteOpportunity(t *testing.T) {
	base := time.Unix(150, 0)
	clock := newFakeEndpointClock(base)
	device := &schedulerTestDevice{desc: testCompositeDescriptor()}
	recorder := newRecordingWriter()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorkerWithClock(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, newResponseWriter(recorder, nil), func(error) {}, clock,
	)
	defer func() {
		cancel()
		worker.signal()
		<-worker.done
	}()

	firstService := base.Add(time.Millisecond)
	for seq := uint32(1); seq <= 4; seq++ {
		require.True(t, worker.enqueue(seq, 64, nil, nil, firstService))
	}

	clock.waitForDeadline(t, firstService)
	clock.advance(time.Millisecond)
	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, uint32(1), binary.BigEndian.Uint32(writes[0].packet[4:8]))
	device.mu.Lock()
	require.Equal(t, 1, device.inputBuilds,
		"state selection and encoding happen once at the endpoint opportunity")
	device.mu.Unlock()
	clock.waitForDeadline(t, base.Add(2*time.Millisecond))

	// Without another service opportunity, neither a second URB nor a nested
	// device-side interval may complete.
	time.Sleep(time.Millisecond)
	recorder.mu.Lock()
	require.Len(t, recorder.writes, 1)
	recorder.mu.Unlock()

	// The next reservation is now one complete interval late. It re-anchors to
	// this instant and completes once; the expired slot is not replayed as a
	// catch-up burst.
	clock.advance(2 * time.Millisecond)
	writes = recorder.waitForWrites(t, 2)
	require.Equal(t, uint32(2), binary.BigEndian.Uint32(writes[1].packet[4:8]))
	clock.waitForDeadline(t, base.Add(4*time.Millisecond))
	time.Sleep(time.Millisecond)
	recorder.mu.Lock()
	require.Len(t, recorder.writes, 2)
	recorder.mu.Unlock()
	require.Equal(t, uint64(1), worker.telemetry.reanchored.Load())

	// Unlink the next claimed-but-not-started URB. Its reservation is removed,
	// and later work proceeds at the following absolute opportunity.
	require.True(t, worker.unlinkAt(3, clock.Now()))
	clock.advance(time.Millisecond)
	writes = recorder.waitForWrites(t, 3)
	require.Equal(t, uint32(4), binary.BigEndian.Uint32(writes[2].packet[4:8]))
	time.Sleep(time.Millisecond)
	recorder.mu.Lock()
	require.Len(t, recorder.writes, 3)
	recorder.mu.Unlock()
}

func TestInterruptClaimReturnsStateWhenResetOrUnlinkWinsBeforeSend(t *testing.T) {
	for _, test := range []struct {
		name   string
		cancel func(*endpointWorker)
	}{
		{name: "unlink", cancel: func(worker *endpointWorker) {
			require.True(t, worker.unlink(610))
		}},
		{name: "reset", cancel: func(worker *endpointWorker) {
			worker.reset()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			device := newClaimedSchedulerTestDevice()
			recorder := newRecordingWriter()
			responses := newResponseWriter(recorder, nil)
			responses.mu.Lock() // hold the worker in the encode-to-send window
			ctx, cancel := context.WithCancel(context.Background())
			worker := newEndpointWorker(
				ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
				64, responses, func(error) {},
			)

			require.True(t, worker.enqueue(610, 64, nil, nil, time.Now()))
			select {
			case <-device.claimReady:
			case <-time.After(time.Second):
				t.Fatal("input state was not claimed")
			}
			test.cancel(worker)
			responses.mu.Unlock()

			select {
			case presented := <-device.completion:
				require.False(t, presented)
			case <-time.After(time.Second):
				t.Fatal("cancelled input claim was not returned")
			}
			require.Equal(t, uint64(0), device.commitCount.Load())
			require.Equal(t, uint64(1), device.retryCount.Load())
			time.Sleep(time.Millisecond)
			recorder.mu.Lock()
			require.Empty(t, recorder.writes)
			recorder.mu.Unlock()

			cancel()
			worker.signal()
			<-worker.done
		})
	}
}

func TestInterruptClaimCommitsOnlyAfterSuccessfulSocketWrite(t *testing.T) {
	device := newClaimedSchedulerTestDevice()
	writeEntered := make(chan struct{})
	releaseWrite := make(chan struct{})
	destination := writerFunc(func(packet []byte) (int, error) {
		close(writeEntered)
		<-releaseWrite
		return len(packet), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorker(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, newResponseWriter(destination, nil), func(error) {},
	)
	require.True(t, worker.enqueue(611, 64, nil, nil, time.Now()))

	select {
	case <-writeEntered:
	case <-time.After(time.Second):
		t.Fatal("socket write was not entered")
	}
	require.Equal(t, uint64(0), device.commitCount.Load(),
		"presentation cannot commit before socket I/O succeeds")
	require.False(t, worker.unlink(611),
		"unlink cannot cancel a response after send ownership starts")
	close(releaseWrite)
	select {
	case presented := <-device.completion:
		require.True(t, presented)
	case <-time.After(time.Second):
		t.Fatal("successful input claim was not committed")
	}

	cancel()
	worker.signal()
	<-worker.done
	require.Equal(t, uint64(0), device.retryCount.Load())
}

func TestInterruptSocketFailureReturnsClaimToRecovery(t *testing.T) {
	device := newClaimedSchedulerTestDevice()
	writeFailure := errors.New("synthetic socket failure")
	failureSeen := make(chan error, 1)
	destination := writerFunc(func([]byte) (int, error) {
		return 0, writeFailure
	})
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorker(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, newResponseWriter(destination, nil), func(err error) { failureSeen <- err },
	)
	require.True(t, worker.enqueue(612, 64, nil, nil, time.Now()))

	select {
	case presented := <-device.completion:
		require.False(t, presented,
			"a failed write must return the ordered state to recovery front")
	case <-time.After(time.Second):
		t.Fatal("failed input claim was not returned")
	}
	select {
	case err := <-failureSeen:
		require.ErrorIs(t, err, writeFailure)
	case <-time.After(time.Second):
		t.Fatal("socket failure was not reported")
	}
	require.Equal(t, uint64(0), device.commitCount.Load())
	require.Equal(t, uint64(1), device.retryCount.Load())

	cancel()
	worker.signal()
	<-worker.done
}

func TestVersionedGetReportCannotSerializeStalePressAfterRelease(t *testing.T) {
	device := newVersionedInputTestDevice()
	runVersionedGetReportCannotSerializeStalePressAfterRelease(
		t, device, device)
}

func TestVersionedReportIDZeroCannotSerializeStalePressAfterRelease(
	t *testing.T,
) {
	device := newVersionedInputTestDevice()
	byID := &versionedInputIDTestDevice{versionedInputTestDevice: device}
	runVersionedGetReportCannotSerializeStalePressAfterRelease(
		t, device, inputReportIDSnapshotRequest{
			inputReportIDSnapshotter: byID,
			reportID:                 0,
		})
}

func runVersionedGetReportCannotSerializeStalePressAfterRelease(
	t *testing.T, device *versionedInputTestDevice,
	snapshotter inputReportSnapshotter,
) {
	t.Helper()
	firstWriteEntered := make(chan struct{})
	releaseFirstWrite := make(chan struct{})
	var writeCount atomic.Uint64
	var writesMu sync.Mutex
	var writes [][]byte
	destination := writerFunc(func(packet []byte) (int, error) {
		ordinal := writeCount.Add(1)
		if ordinal == 1 {
			close(firstWriteEntered)
			<-releaseFirstWrite
		}
		owned := append([]byte(nil), packet...)
		writesMu.Lock()
		writes = append(writes, owned)
		writesMu.Unlock()
		return len(packet), nil
	})
	responses := newResponseWriter(destination, nil)
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorker(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, responses, func(error) {},
	)
	defer func() {
		cancel()
		worker.signal()
		<-worker.done
	}()

	require.True(t, worker.enqueue(700, 64, nil, nil, time.Now()))
	select {
	case <-firstWriteEntered:
	case <-time.After(time.Second):
		t.Fatal("release interrupt did not enter socket write")
	}

	getDone := make(chan error, 1)
	go func() {
		var report [64]byte
		_, err := writeVersionedInputReportResponse(
			responses, snapshotter, nil, report[:], 701, len(report),
		)
		getDone <- err
	}()
	select {
	case <-device.snapshotReady:
		// GET_REPORT copied the old press, then blocked behind the release's
		// response ownership. Completion must advance the version before it
		// can validate and write.
	case <-time.After(time.Second):
		t.Fatal("GET_REPORT did not snapshot the previously presented press")
	}
	close(releaseFirstWrite)
	select {
	case presented := <-device.completion:
		require.True(t, presented)
	case <-time.After(time.Second):
		t.Fatal("release interrupt did not complete")
	}
	select {
	case err := <-getDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("versioned GET_REPORT did not finish")
	}

	writesMu.Lock()
	require.Len(t, writes, 2)
	interruptPacket := writes[0]
	controlPacket := writes[1]
	writesMu.Unlock()
	require.Equal(t, uint32(700), binary.BigEndian.Uint32(interruptPacket[4:8]))
	require.Equal(t, uint32(701), binary.BigEndian.Uint32(controlPacket[4:8]))
	require.Zero(t, interruptPacket[retSubmitHeaderSize],
		"interrupt response must carry the release")
	require.Zero(t, controlPacket[retSubmitHeaderSize],
		"GET_REPORT must retry and carry the release, not the old press")
	require.GreaterOrEqual(t, device.snapshotCalls.Load(), uint64(2),
		"stale GET_REPORT snapshot was not rebuilt")
}

func TestVersionedInputReportRequestRoutesOnlyExactInputReport(t *testing.T) {
	device := newVersionedInputTestDevice()
	var setup [8]byte
	setup[0] = hidReqTypeIn
	setup[1] = hidReqGetReport
	binary.LittleEndian.PutUint16(setup[2:4], 0x0101)
	binary.LittleEndian.PutUint16(setup[6:8], 64)

	snapshotter, length, disposition := versionedInputReportRequest(
		device, 0, usbip.DirIn, setup[:], 255,
	)
	require.Equal(t, versionedInputReportSnapshot, disposition)
	require.Same(t, device, snapshotter)
	require.Equal(t, 64, length,
		"control response must be bounded by wLength before host xfer capacity")

	setup[2] = 0x02 // another report ID must retain device HandleControl semantics
	_, _, disposition = versionedInputReportRequest(
		device, 0, usbip.DirIn, setup[:], 64)
	require.Equal(t, versionedInputReportUnhandled, disposition)
	setup[2] = 0x01
	_, _, disposition = versionedInputReportRequest(
		device, 1, usbip.DirIn, setup[:], 64)
	require.Equal(t, versionedInputReportUnhandled, disposition,
		"only EP0 can use control snapshot serialization")
	_, _, disposition = versionedInputReportRequest(
		device, 0, usbip.DirOut, setup[:], 64)
	require.Equal(t, versionedInputReportUnhandled, disposition,
		"OUT requests cannot use input GET_REPORT serialization")
}

func TestVersionedInputReportRequestBindsOptedInReportID(t *testing.T) {
	device := &versionedInputIDTestDevice{
		versionedInputTestDevice: newVersionedInputTestDevice(),
	}
	var setup [8]byte
	setup[0] = hidReqTypeIn
	setup[1] = hidReqGetReport
	binary.LittleEndian.PutUint16(setup[2:4], 0x0105)
	binary.LittleEndian.PutUint16(setup[6:8], 64)
	snapshotter, length, disposition := versionedInputReportRequest(
		device, 0, usbip.DirIn, setup[:], 64,
	)
	require.Equal(t, versionedInputReportSnapshot, disposition)
	require.Equal(t, 64, length)
	var report [64]byte
	n, version := snapshotter.SnapshotInputReportInto(report[:])
	require.Equal(t, len(report), n)
	require.Equal(t, byte(0x05), report[0])
	require.True(t, snapshotter.InputReportSnapshotCurrent(version))

	binary.LittleEndian.PutUint16(setup[2:4], 0x0100)
	snapshotter, _, disposition = versionedInputReportRequest(
		device, 0, usbip.DirIn, setup[:], 64,
	)
	require.Equal(t, versionedInputReportSnapshot, disposition,
		"report ID zero must retain versioned serialization")
	n, version = snapshotter.SnapshotInputReportInto(report[:])
	require.Equal(t, len(report), n)
	require.True(t, snapshotter.InputReportSnapshotCurrent(version))

	binary.LittleEndian.PutUint16(setup[2:4], 0x0102)
	_, _, disposition = versionedInputReportRequest(
		device, 0, usbip.DirIn, setup[:], 64,
	)
	require.Equal(t, versionedInputReportStall, disposition,
		"ID-aware unsupported reports must STALL")
	binary.LittleEndian.PutUint16(setup[2:4], 0x0101)
	_, _, disposition = versionedInputReportRequest(
		device, 0, usbip.DirIn, setup[:], 64,
	)
	require.Equal(t, versionedInputReportStall, disposition,
		"ID-aware devices must not inherit the legacy report 0x01 fallback")

	binary.LittleEndian.PutUint16(setup[2:4], 0x0105)
	binary.LittleEndian.PutUint16(setup[4:6], 0x0001)
	_, _, disposition = versionedInputReportRequest(
		device, 0, usbip.DirIn, setup[:], 64,
	)
	require.Equal(t, versionedInputReportStall, disposition,
		"a supported HID report cannot target the vendor interface")
	binary.LittleEndian.PutUint16(setup[4:6], 0x0100)
	_, _, disposition = versionedInputReportRequest(
		device, 0, usbip.DirIn, setup[:], 64,
	)
	require.Equal(t, versionedInputReportStall, disposition,
		"wIndex high-byte aliases must not reach the HID interface")
	binary.LittleEndian.PutUint16(setup[4:6], 0x0000)
	_, _, disposition = versionedInputReportRequest(
		device, 0, usbip.DirIn, setup[:], 64,
	)
	require.Equal(t, versionedInputReportSnapshot, disposition,
		"valid HID interface zero must retain versioned serialization")
}

func TestEndpointWorkerUnlinkCompactsLaterReservations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newEndpointWorker(
		ctx, &schedulerTestDevice{desc: testCompositeDescriptor()},
		4, usbip.DirIn, interruptInWorker, time.Millisecond, 64,
		newResponseWriter(bytes.NewBuffer(nil), nil), func(error) {},
	)
	<-w.done
	base := time.Unix(200, 0)
	require.True(t, w.enqueue(10, 64, nil, nil, base))
	require.True(t, w.enqueue(11, 64, nil, nil, base))
	require.True(t, w.enqueue(12, 64, nil, nil, base))
	require.Equal(t, 0, w.claimNext())

	require.True(t, w.unlinkAt(10, base))
	w.mu.Lock()
	second := &w.slots[w.order[w.orderHead]]
	third := &w.slots[w.order[(w.orderHead+1)%len(w.order)]]
	require.Equal(t, base, second.serviceAt)
	require.Equal(t, base.Add(time.Millisecond), third.serviceAt)
	w.mu.Unlock()

	require.True(t, w.unlinkAt(11, base))
	w.mu.Lock()
	third = &w.slots[w.order[w.orderHead]]
	require.Equal(t, base, third.serviceAt,
		"unlink must release later work from the cancelled reservation")
	w.mu.Unlock()
}

func TestIsoOutWorkerDoesNotDelayInterruptInAndOwnsReaderData(t *testing.T) {
	device := &schedulerTestDevice{desc: testCompositeDescriptor()}
	recorder := newRecordingWriter()
	responses := newResponseWriter(recorder, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupt := newEndpointWorker(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, responses, func(error) {},
	)
	isoOut := newEndpointWorker(
		ctx, device, 1, usbip.DirOut, isoOutWorker, time.Millisecond,
		192, responses, func(error) {},
	)
	defer func() {
		cancel()
		interrupt.signal()
		isoOut.signal()
		<-interrupt.done
		<-isoOut.done
	}()

	payload := []byte{1, 2, 3, 4}
	packets := []usbip.IsoPacketDescriptor{
		{Offset: 0, Length: 1},
		{Offset: 1, Length: 1},
		{Offset: 2, Length: 1},
		{Offset: 3, Length: 1},
	}
	started := time.Now()
	require.True(t, isoOut.enqueue(100, 4, payload, packets, started))
	clear(payload)
	clear(packets)
	require.True(t, interrupt.enqueue(200, 64, nil, nil, started))

	writes := recorder.waitForWrites(t, 2)
	require.Equal(t, uint32(200), binary.BigEndian.Uint32(writes[0].packet[4:8]),
		"future ISO work must not delay an eligible interrupt response")
	require.Equal(t, uint32(100), binary.BigEndian.Uint32(writes[1].packet[4:8]))
	require.GreaterOrEqual(t, writes[1].at.Sub(started), 3*time.Millisecond)

	device.mu.Lock()
	require.Equal(t, [][]byte{{1, 2, 3, 4}}, device.isoOutPayloads,
		"queued ISO payload must not alias command-reader scratch")
	device.mu.Unlock()
}

func TestIsoOutFakeClockProcessesAtWindowStartAndCompletesAtEnd(t *testing.T) {
	base := time.Unix(250, 0)
	clock := newFakeEndpointClock(base)
	device := &schedulerTestDevice{
		desc: testCompositeDescriptor(), isoOutWake: make(chan struct{}, 1),
	}
	recorder := newRecordingWriter()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorkerWithClock(
		ctx, device, 1, usbip.DirOut, isoOutWorker, time.Millisecond,
		192, newResponseWriter(recorder, nil), func(error) {}, clock,
	)
	defer func() {
		cancel()
		worker.signal()
		<-worker.done
	}()

	packets := []usbip.IsoPacketDescriptor{
		{Offset: 0, Length: 1},
		{Offset: 1, Length: 1},
		{Offset: 2, Length: 1},
		{Offset: 3, Length: 1},
	}
	serviceStart := base.Add(time.Millisecond)
	require.True(t, worker.enqueue(250, 4, []byte{1, 2, 3, 4}, packets, serviceStart))
	clock.waitForDeadline(t, serviceStart)
	clock.advance(time.Millisecond)
	select {
	case <-device.isoOutWake:
	case <-time.After(time.Second):
		t.Fatal("ISO OUT media was not processed at its service-window start")
	}
	device.mu.Lock()
	require.Equal(t, [][]byte{{1, 2, 3, 4}}, device.isoOutPayloads)
	device.mu.Unlock()
	recorder.mu.Lock()
	require.Empty(t, recorder.writes,
		"RET_SUBMIT cannot precede the end of the reserved ISO packet window")
	recorder.mu.Unlock()

	completion := serviceStart.Add(4 * time.Millisecond)
	clock.waitForDeadline(t, completion)
	clock.advance(4 * time.Millisecond)
	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, uint32(250), binary.BigEndian.Uint32(writes[0].packet[4:8]))
}

func TestIsoInWorkerSamplesOncePerPacketWithOnePersistentTimer(t *testing.T) {
	desc := testCompositeDescriptor()
	desc.Interfaces = append(desc.Interfaces, usbdesc.InterfaceConfig{
		Endpoints: []usbdesc.EndpointDescriptor{{
			BEndpointAddress: 0x82,
			BMAttributes:     0x05,
			WMaxPacketSize:   196,
			BInterval:        4,
		}},
	})
	device := &schedulerTestDevice{desc: desc}
	recorder := newRecordingWriter()
	responses := newResponseWriter(recorder, nil)
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorker(
		ctx, device, 2, usbip.DirIn, isoInWorker, time.Millisecond,
		196, responses, func(error) {},
	)
	defer func() {
		cancel()
		worker.signal()
		<-worker.done
	}()

	packets := []usbip.IsoPacketDescriptor{
		{Offset: 0, Length: 196},
		{Offset: 196, Length: 196},
	}
	require.True(t, worker.enqueue(301, 392, nil, packets, time.Now()))
	writes := recorder.waitForWrites(t, 1)
	require.Len(t, writes, 1, "one ISO completion must be one contiguous write")
	require.Equal(t, uint32(384), binary.BigEndian.Uint32(writes[0].packet[24:28]))
	firstDescriptor := retSubmitHeaderSize + 384
	require.Equal(t, uint32(192), binary.BigEndian.Uint32(
		writes[0].packet[firstDescriptor+8:firstDescriptor+12],
	))
	require.Equal(t, uint32(192), binary.BigEndian.Uint32(
		writes[0].packet[firstDescriptor+isoPacketDescriptorSize+8:firstDescriptor+isoPacketDescriptorSize+12],
	))
	device.mu.Lock()
	require.Equal(t, 2, device.microphoneReads)
	device.mu.Unlock()
}

func TestIsoOutUnlinkReleasesLaterJobAndResetInvalidatesGeneration(t *testing.T) {
	device := &schedulerTestDevice{desc: testCompositeDescriptor()}
	recorder := newRecordingWriter()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorker(
		ctx, device, 1, usbip.DirOut, isoOutWorker, time.Millisecond,
		192, newResponseWriter(recorder, nil), func(error) {},
	)
	defer func() {
		cancel()
		worker.signal()
		<-worker.done
	}()

	eightPackets := make([]usbip.IsoPacketDescriptor, 8)
	for index := range eightPackets {
		eightPackets[index] = usbip.IsoPacketDescriptor{Offset: uint32(index), Length: 1}
	}
	reservedStart := time.Now().Add(20 * time.Millisecond)
	require.True(t, worker.enqueue(401, 8, []byte("cancelme"), eightPackets, reservedStart))
	require.True(t, worker.enqueue(402, 1, []byte{0x42},
		[]usbip.IsoPacketDescriptor{{Length: 1}}, reservedStart))
	require.True(t, worker.unlink(401))

	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, uint32(402), binary.BigEndian.Uint32(writes[0].packet[4:8]))
	time.Sleep(10 * time.Millisecond)
	recorder.mu.Lock()
	require.Len(t, recorder.writes, 1, "unlinked work must never emit RET_SUBMIT")
	recorder.mu.Unlock()

	require.True(t, worker.enqueue(403, 8, []byte("old-gen!"), eightPackets,
		time.Now().Add(20*time.Millisecond)))
	worker.reset()
	require.True(t, worker.enqueue(404, 1, []byte{0x44},
		[]usbip.IsoPacketDescriptor{{Length: 1}}, time.Now()))
	writes = recorder.waitForWrites(t, 2)
	require.Equal(t, uint32(404), binary.BigEndian.Uint32(writes[1].packet[4:8]))
	time.Sleep(10 * time.Millisecond)
	recorder.mu.Lock()
	require.Len(t, recorder.writes, 2, "old-generation work must remain invalidated")
	recorder.mu.Unlock()

	device.mu.Lock()
	require.Equal(t, [][]byte{{0x42}, {0x44}}, device.isoOutPayloads)
	device.mu.Unlock()
}

func TestIsoOutDeviceGenerationRejectsResetRace(t *testing.T) {
	base := &schedulerTestDevice{desc: testCompositeDescriptor()}
	device := &generationSchedulerTestDevice{schedulerTestDevice: base}
	device.generation.Store(1)
	recorder := newRecordingWriter()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorker(
		ctx, device, 1, usbip.DirOut, isoOutWorker, time.Millisecond,
		192, newResponseWriter(recorder, nil), func(error) {},
	)
	defer func() {
		cancel()
		worker.signal()
		<-worker.done
	}()

	packets := []usbip.IsoPacketDescriptor{{Length: 1}}
	require.True(t, worker.enqueueWithGeneration(
		501, 1, []byte{0x51}, packets, time.Now().Add(4*time.Millisecond), 1,
	))
	device.generation.Store(2) // Device reset wins the service-boundary race.
	time.Sleep(10 * time.Millisecond)
	recorder.mu.Lock()
	require.Empty(t, recorder.writes)
	recorder.mu.Unlock()
	require.Equal(t, uint64(1), device.rejected.Load())

	require.True(t, worker.enqueueWithGeneration(
		502, 1, []byte{0x52}, packets, time.Now(), 2,
	))
	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, uint32(502), binary.BigEndian.Uint32(writes[0].packet[4:8]))
	base.mu.Lock()
	require.Equal(t, [][]byte{{0x52}}, base.isoOutPayloads)
	base.mu.Unlock()
}

func TestIsoOutUnlinkCannotCancelAfterSideEffectStarts(t *testing.T) {
	base := &schedulerTestDevice{desc: testCompositeDescriptor()}
	generationDevice := &generationSchedulerTestDevice{schedulerTestDevice: base}
	generationDevice.generation.Store(1)
	device := &blockingIsoOutDevice{
		generationSchedulerTestDevice: generationDevice,
		started:                       make(chan struct{}),
		release:                       make(chan struct{}),
	}
	recorder := newRecordingWriter()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorker(
		ctx, device, 1, usbip.DirOut, isoOutWorker, time.Millisecond,
		192, newResponseWriter(recorder, nil), func(error) {},
	)
	defer func() {
		cancel()
		worker.signal()
		<-worker.done
	}()

	require.True(t, worker.enqueueWithGeneration(
		503, 1, []byte{0x53},
		[]usbip.IsoPacketDescriptor{{Length: 1}}, time.Now(), 1,
	))
	select {
	case <-device.started:
	case <-time.After(time.Second):
		t.Fatal("ISO OUT callback did not start")
	}
	require.False(t, worker.unlink(503),
		"unlink after an irreversible callback starts must report completed success")
	close(device.release)
	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, uint32(503), binary.BigEndian.Uint32(writes[0].packet[4:8]),
		"normal RET_SUBMIT must remain due after side-effect ownership")
	base.mu.Lock()
	require.Equal(t, [][]byte{{0x53}}, base.isoOutPayloads)
	base.mu.Unlock()
}

func TestEndpointAdmissionAllocatesZeroAfterSlotWarmup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newEndpointWorker(
		ctx, &schedulerTestDevice{desc: testCompositeDescriptor()},
		4, usbip.DirIn, interruptInWorker, time.Millisecond, 64,
		newResponseWriter(bytes.NewBuffer(nil), nil), func(error) {},
	)
	<-w.done

	var seq uint32
	allocations := testing.AllocsPerRun(1000, func() {
		seq++
		if !w.enqueue(seq, 64, nil, nil, time.Unix(300, 0)) {
			panic("enqueue failed")
		}
		if !w.unlink(seq) {
			panic("unlink failed")
		}
	})
	require.Zero(t, allocations)
}

func fastPathCompositeDescriptor() *usbdesc.Descriptor {
	desc := testCompositeDescriptor()
	desc.Interfaces = append(desc.Interfaces, usbdesc.InterfaceConfig{
		Endpoints: []usbdesc.EndpointDescriptor{{
			BEndpointAddress: 0x82,
			BMAttributes:     0x05,
			WMaxPacketSize:   192,
			BInterval:        4,
		}},
	})
	return desc
}

func TestEndpointSchedulersPrecreateDescriptorKnownFastPaths(t *testing.T) {
	base := &schedulerTestDevice{desc: fastPathCompositeDescriptor()}
	device := &generationSchedulerTestDevice{schedulerTestDevice: base}
	device.generation.Store(1)
	ctx, cancel := context.WithCancel(context.Background())
	schedulers := newEndpointSchedulers(
		ctx, device, newResponseWriter(discardResponseWriter{}, nil), nil,
	)
	defer func() {
		cancel()
		schedulers.close()
	}()

	wanted := []endpointWorkerKey{
		{ep: 4, dir: usbip.DirIn, kind: interruptInWorker},
		{ep: 2, dir: usbip.DirIn, kind: isoInWorker},
		{ep: 1, dir: usbip.DirOut, kind: isoOutWorker},
	}
	schedulers.workersMu.Lock()
	require.Len(t, schedulers.workers, len(wanted))
	for _, key := range wanted {
		require.NotNil(t, schedulers.workers[key], "worker %v was not pre-created", key)
	}
	schedulers.workersMu.Unlock()
	require.Same(t, schedulers.workers[wanted[0]], schedulers.fastInterruptIn[4])
	require.Same(t, schedulers.workers[wanted[1]], schedulers.fastIsoIn[2])
	require.Same(t, schedulers.workers[wanted[2]], schedulers.fastIsoOut[1])

	// Admission of the very first real URB sees an initialized worker. It does
	// not create its goroutine, timer, channels, or owned buffers on the command
	// reader.
	interrupt := schedulers.workers[wanted[0]]
	interrupt.mu.Lock()
	interrupt.cursor = time.Now().Add(time.Hour)
	interrupt.mu.Unlock()
	require.True(t, schedulers.enqueueInterruptIn(1, 4, 64))
	require.True(t, interrupt.unlink(1))
}

func TestEndpointWorkersBindExactInterfaceAlternateParameters(t *testing.T) {
	desc := &usbdesc.Descriptor{
		Device: usbdesc.DeviceDescriptor{Speed: 2},
		Interfaces: []usbdesc.InterfaceConfig{
			{
				Descriptor: usbdesc.InterfaceDescriptor{
					BInterfaceNumber: 3, BAlternateSetting: 1,
				},
				Endpoints: []usbdesc.EndpointDescriptor{{
					BEndpointAddress: 0x81, BMAttributes: 0x02,
					WMaxPacketSize: 64, BInterval: 1,
				}},
			},
			{
				Descriptor: usbdesc.InterfaceDescriptor{
					BInterfaceNumber: 3, BAlternateSetting: 2,
				},
				Endpoints: []usbdesc.EndpointDescriptor{{
					BEndpointAddress: 0x81, BMAttributes: 0x03,
					WMaxPacketSize: 8, BInterval: 4,
				}},
			},
		},
	}
	device := &schedulerTestDevice{desc: desc}
	ctx, cancel := context.WithCancel(context.Background())
	schedulers := newEndpointSchedulers(ctx, device,
		newResponseWriter(discardResponseWriter{}, nil), nil)
	defer func() {
		cancel()
		schedulers.close()
	}()
	bulkBinding := endpointDescriptorBinding{
		interfaceNumber: 3, alternateSetting: 1,
		descriptor: &desc.Interfaces[0].Endpoints[0],
	}
	interruptBinding := endpointDescriptorBinding{
		interfaceNumber: 3, alternateSetting: 2,
		descriptor: &desc.Interfaces[1].Endpoints[0],
	}
	bulkWorker := schedulers.workerForBinding(
		bulkBinding, usbip.DirIn, genericInWorker)
	interruptWorker := schedulers.workerForBinding(
		interruptBinding, usbip.DirIn, interruptInWorker)
	require.NotNil(t, bulkWorker)
	require.NotNil(t, interruptWorker)
	require.NotSame(t, bulkWorker, interruptWorker)
	require.Equal(t, 64, bulkWorker.maxPacket)
	require.Equal(t, time.Millisecond, bulkWorker.interval)
	require.Equal(t, 8, interruptWorker.maxPacket)
	require.Equal(t, 4*time.Millisecond, interruptWorker.interval)
}

func TestPrecreatedFastAdmissionsDoNotTakeWorkerMapLock(t *testing.T) {
	base := &schedulerTestDevice{desc: fastPathCompositeDescriptor()}
	device := &generationSchedulerTestDevice{schedulerTestDevice: base}
	device.generation.Store(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	schedulers := newEndpointSchedulers(
		ctx, device, newResponseWriter(discardResponseWriter{}, nil), nil,
	)
	schedulers.close()

	payload := make([]byte, 192)
	packets := []usbip.IsoPacketDescriptor{{Length: 192}}
	type admissionResult struct {
		seq uint32
		ok  bool
	}
	results := make(chan admissionResult, 3)
	schedulers.workersMu.Lock()
	go func() {
		results <- admissionResult{801,
			schedulers.enqueueInterruptIn(801, 4, 64)}
	}()
	go func() {
		results <- admissionResult{802,
			schedulers.enqueueIsoIn(802, 2, 192, packets)}
	}()
	go func() {
		results <- admissionResult{803,
			schedulers.enqueueIsoOut(803, 1, 192, payload, packets)}
	}()

	var admitted [3]admissionResult
	completed := 0
	timer := time.NewTimer(time.Second)
	for completed < len(admitted) {
		select {
		case admitted[completed] = <-results:
			completed++
		case <-timer.C:
			schedulers.workersMu.Unlock()
			t.Fatalf("only %d fast admissions completed while diagnostics map lock was held",
				completed)
		}
	}
	if !timer.Stop() {
		<-timer.C
	}
	schedulers.workersMu.Unlock()
	for _, result := range admitted {
		require.True(t, result.ok, "fast admission seq %d", result.seq)
		require.True(t, schedulers.unlink(result.seq), "unlink seq %d", result.seq)
	}

	interruptGeneration := schedulers.fastInterruptIn[4].generation
	isoInGeneration := schedulers.fastIsoIn[2].generation
	isoOutGeneration := schedulers.fastIsoOut[1].generation
	schedulers.resetAll()
	require.Equal(t, interruptGeneration+1,
		schedulers.fastInterruptIn[4].generation)
	require.Equal(t, isoInGeneration+1, schedulers.fastIsoIn[2].generation)
	require.Equal(t, isoOutGeneration+1, schedulers.fastIsoOut[1].generation)
	require.Len(t, schedulers.snapshot().Endpoints, 3,
		"diagnostics must retain every immutable fast worker")
}

func TestPrecreatedEndpointSchedulerAdmissionAllocatesZero(t *testing.T) {
	base := &schedulerTestDevice{desc: fastPathCompositeDescriptor()}
	device := &generationSchedulerTestDevice{schedulerTestDevice: base}
	device.generation.Store(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	schedulers := newEndpointSchedulers(
		ctx, device, newResponseWriter(discardResponseWriter{}, nil), nil,
	)
	schedulers.close()

	interrupt := schedulers.workers[endpointWorkerKey{
		ep: 4, dir: usbip.DirIn, kind: interruptInWorker,
	}]
	isoIn := schedulers.workers[endpointWorkerKey{
		ep: 2, dir: usbip.DirIn, kind: isoInWorker,
	}]
	isoOut := schedulers.workers[endpointWorkerKey{
		ep: 1, dir: usbip.DirOut, kind: isoOutWorker,
	}]
	payload := make([]byte, 8*192)
	packets := make([]usbip.IsoPacketDescriptor, 8)
	for index := range packets {
		packets[index] = usbip.IsoPacketDescriptor{
			Offset: uint32(index * 192), Length: 192,
		}
	}
	var seq uint32
	allocations := testing.AllocsPerRun(1000, func() {
		seq++
		if !schedulers.enqueueInterruptIn(seq, 4, 64) || !interrupt.unlink(seq) {
			panic("interrupt admission failed")
		}
		seq++
		if !schedulers.enqueueIsoIn(seq, 2, uint32(len(payload)), packets) ||
			!isoIn.unlink(seq) {
			panic("ISO-IN admission failed")
		}
		seq++
		if !schedulers.enqueueIsoOut(seq, 1, uint32(len(payload)), payload, packets) ||
			!isoOut.unlink(seq) {
			panic("ISO-OUT admission failed")
		}
	})
	require.Zero(t, allocations)
}

func TestIsoOutAdmissionAllocatesZeroWithPreallocatedDualSenseSlots(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newEndpointWorker(
		ctx, &schedulerTestDevice{desc: testCompositeDescriptor()},
		1, usbip.DirOut, isoOutWorker, time.Millisecond, 192,
		newResponseWriter(bytes.NewBuffer(nil), nil), func(error) {},
	)
	<-w.done

	payload := make([]byte, 8*192)
	packets := make([]usbip.IsoPacketDescriptor, 8)
	for index := range packets {
		packets[index] = usbip.IsoPacketDescriptor{
			Offset: uint32(index * 192), Length: 192,
		}
	}
	now := time.Unix(350, 0)
	var seq uint32
	allocations := testing.AllocsPerRun(1000, func() {
		seq++
		if !w.enqueue(seq, uint32(len(payload)), payload, packets, now) {
			panic("ISO enqueue failed")
		}
		if !w.unlinkAt(seq, now) {
			panic("ISO unlink failed")
		}
	})
	require.Zero(t, allocations)
}

func TestEndpointWorkersCloseWithoutLeaking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	workers := []*endpointWorker{
		newEndpointWorker(ctx, &schedulerTestDevice{desc: testCompositeDescriptor()},
			4, usbip.DirIn, interruptInWorker, time.Millisecond, 64,
			newResponseWriter(discardResponseWriter{}, nil), func(error) {}),
		newEndpointWorker(ctx, &schedulerTestDevice{desc: testCompositeDescriptor()},
			1, usbip.DirOut, isoOutWorker, time.Millisecond, 192,
			newResponseWriter(discardResponseWriter{}, nil), func(error) {}),
	}
	cancel()
	for _, worker := range workers {
		worker.signal()
		select {
		case <-worker.done:
		case <-time.After(time.Second):
			t.Fatal("persistent endpoint worker did not terminate")
		}
		select {
		case <-worker.done:
		default:
			t.Fatal("closed worker done channel is not stable")
		}
	}
}

func TestServerEndpointDiagnosticsSnapshotIsExternallyReachable(t *testing.T) {
	server := New(ServerConfig{}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	device := &schedulerTestDevice{desc: testCompositeDescriptor()}
	schedulers := newEndpointSchedulers(
		ctx, device, newResponseWriter(discardResponseWriter{}, nil), nil,
	)
	server.registerEndpointSchedulers(schedulers)
	worker := schedulers.worker(4, usbip.DirIn, interruptInWorker)
	worker.telemetry.queueAge.record(750 * time.Microsecond)
	worker.telemetry.lateness.record(1250 * time.Microsecond)

	snapshot := server.EndpointDiagnosticsSnapshot()
	require.Len(t, snapshot.Connections, 1)
	var endpoint USBIPEndpointDiagnostics
	found := false
	for _, candidate := range snapshot.Connections[0].Endpoints {
		if candidate.Endpoint == 4 && candidate.Direction == usbip.DirIn &&
			candidate.Kind == "interrupt-in" {
			endpoint = candidate
			found = true
			break
		}
	}
	require.True(t, found)
	require.Equal(t, uint32(4), endpoint.Endpoint)
	require.Equal(t, "interrupt-in", endpoint.Kind)
	require.Equal(t, uint64(1), endpoint.QueueAge.Count)
	require.Equal(t, time.Millisecond, endpoint.QueueAge.Median)
	require.Equal(t, uint64(1), endpoint.Lateness.Count)
	require.Equal(t, 2*time.Millisecond, endpoint.Lateness.P99)

	server.unregisterEndpointSchedulers(schedulers)
	cancel()
	schedulers.close()
	require.Empty(t, server.EndpointDiagnosticsSnapshot().Connections)
}

func BenchmarkInterruptEndpointAdmission(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newEndpointWorker(
		ctx, &schedulerTestDevice{desc: testCompositeDescriptor()},
		4, usbip.DirIn, interruptInWorker, time.Millisecond, 64,
		newResponseWriter(bytes.NewBuffer(nil), nil), func(error) {},
	)
	<-w.done
	now := time.Unix(400, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		seq := uint32(i + 1)
		if !w.enqueue(seq, 64, nil, nil, now) || !w.unlink(seq) {
			b.Fatal("admission failed")
		}
	}
}

func BenchmarkPrecreatedEndpointSchedulersAdmission(b *testing.B) {
	base := &schedulerTestDevice{desc: fastPathCompositeDescriptor()}
	device := &generationSchedulerTestDevice{schedulerTestDevice: base}
	device.generation.Store(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	schedulers := newEndpointSchedulers(
		ctx, device, newResponseWriter(discardResponseWriter{}, nil), nil,
	)
	schedulers.close()
	interrupt := schedulers.workers[endpointWorkerKey{
		ep: 4, dir: usbip.DirIn, kind: interruptInWorker,
	}]

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		seq := uint32(iteration + 1)
		if !schedulers.enqueueInterruptIn(seq, 4, 64) || !interrupt.unlink(seq) {
			b.Fatal("scheduler admission failed")
		}
	}
}

func BenchmarkInterruptClaimEncodeRetSubmitWrite(b *testing.B) {
	base := time.Unix(450, 0)
	clock := newFakeEndpointClock(base)
	device := &allocationFreeClaimDevice{
		schedulerTestDevice: &schedulerTestDevice{desc: testCompositeDescriptor()},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker := newEndpointWorkerWithClock(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, newResponseWriter(discardResponseWriter{}, nil), func(error) {}, clock,
	)
	<-worker.done
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	// Warm every worker-owned response buffer before measuring the steady path.
	require.True(b, worker.enqueue(1, 64, nil, nil, base))
	idx := worker.claimNext()
	completed, retain := worker.processInterruptIn(timer, idx)
	require.True(b, completed)
	require.False(b, retain)
	worker.finishCurrentWithRetention(idx, true, false)

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		now := base.Add(time.Duration(iteration+1) * time.Millisecond)
		clock.set(now, false)
		seq := uint32(iteration + 2)
		if !worker.enqueue(seq, 64, nil, nil, now) {
			b.Fatal("enqueue failed")
		}
		idx = worker.claimNext()
		completed, retain = worker.processInterruptIn(timer, idx)
		if !completed || retain {
			b.Fatal("interrupt service failed")
		}
		worker.finishCurrentWithRetention(idx, true, false)
	}
	b.StopTimer()
	require.Equal(b, uint64(b.N+1), device.presented)
}

func BenchmarkVersionedGetReportSnapshotRetSubmitWrite(b *testing.B) {
	device := newVersionedInputTestDevice()
	responses := newResponseWriter(discardResponseWriter{}, nil)
	responseBuffer := make([]byte, 0, retSubmitHeaderSize+64)
	var report [64]byte
	var err error
	responseBuffer, err = writeVersionedInputReportResponse(
		responses, device, responseBuffer, report[:], 1, len(report),
	)
	require.NoError(b, err)

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		responseBuffer, err = writeVersionedInputReportResponse(
			responses, device, responseBuffer, report[:],
			uint32(iteration+2), len(report),
		)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIsoOutAdmissionOwnedCopy(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker := newEndpointWorker(
		ctx, &schedulerTestDevice{desc: testCompositeDescriptor()},
		1, usbip.DirOut, isoOutWorker, time.Millisecond, 192,
		newResponseWriter(discardResponseWriter{}, nil), func(error) {},
	)
	<-worker.done
	payload := make([]byte, 8*192)
	packets := make([]usbip.IsoPacketDescriptor, 8)
	for index := range packets {
		packets[index] = usbip.IsoPacketDescriptor{
			Offset: uint32(index * 192), Length: 192,
		}
	}
	now := time.Unix(500, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		seq := uint32(iteration + 1)
		if !worker.enqueue(seq, uint32(len(payload)), payload, packets, now) ||
			!worker.unlinkAt(seq, now) {
			b.Fatal("ISO admission failed")
		}
	}
}

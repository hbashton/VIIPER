package udecx

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/usb"
)

type fakeHostDriver struct {
	operations  chan Operation
	completions chan Completion
	createErr   error
	mu          sync.Mutex
	created     []CreateDevice
	destroyed   []DeviceIdentity
	destroyErr  error
	completeErr error
}

type fastInputDriver struct {
	*fakeHostDriver
	reports   chan InputReport
	submitErr error
}

type inputSubmitGate struct {
	started chan InputReport
	release chan struct{}
}

type gatedFastInputDriver struct {
	*fastInputDriver
	gates chan *inputSubmitGate
}

type independentlyBlockingCreateDriver struct {
	*fakeHostDriver
	blockedDevice uint64
	started       chan struct{}
	release       chan struct{}
}

func (d *independentlyBlockingCreateDriver) CreateDevice(
	ctx context.Context, device CreateDevice,
) error {
	if device.DeviceID == d.blockedDevice {
		close(d.started)
		select {
		case <-d.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return d.fakeHostDriver.CreateDevice(ctx, device)
}

func (d *fastInputDriver) SubmitInputReport(ctx context.Context, report InputReport) error {
	if d.submitErr != nil {
		return d.submitErr
	}
	report.Payload = append([]byte(nil), report.Payload...)
	select {
	case d.reports <- report:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *gatedFastInputDriver) SubmitInputReport(ctx context.Context, report InputReport) error {
	select {
	case gate := <-d.gates:
		report.Payload = append([]byte(nil), report.Payload...)
		select {
		case gate.started <- report:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-gate.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	default:
	}
	return d.fastInputDriver.SubmitInputReport(ctx, report)
}

func newInputSubmitGate() *inputSubmitGate {
	return &inputSubmitGate{started: make(chan InputReport, 1), release: make(chan struct{})}
}

func TestNextInputReportSequenceFailsClosedAtABICeiling(t *testing.T) {
	last, err := nextInputReportSequence(math.MaxInt64 - 1)
	if err != nil || last != math.MaxInt64 {
		t.Fatalf("last valid sequence=(%d, %v), want (%d, nil)", last, err, uint64(math.MaxInt64))
	}
	for _, previous := range []uint64{math.MaxInt64, math.MaxUint64} {
		if next, nextErr := nextInputReportSequence(previous); next != 0 || !errors.Is(nextErr, errInputSequenceExhausted) {
			t.Fatalf("sequence after %d=(%d, %v), want (0, %v)", previous, next, nextErr, errInputSequenceExhausted)
		}
	}
}

func newFakeHostDriver() *fakeHostDriver {
	return &fakeHostDriver{
		operations: make(chan Operation, 16), completions: make(chan Completion, 16),
	}
}
func (d *fakeHostDriver) CreateDevice(_ context.Context, device CreateDevice) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.created = append(d.created, device)
	return d.createErr
}
func (d *fakeHostDriver) DestroyDevice(_ context.Context, identity DeviceIdentity) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.destroyed = append(d.destroyed, identity)
	return d.destroyErr
}
func (d *fakeHostDriver) Dequeue(ctx context.Context, _ []byte) (Operation, error) {
	select {
	case op := <-d.operations:
		return op, nil
	case <-ctx.Done():
		return Operation{}, ctx.Err()
	}
}
func (d *fakeHostDriver) Complete(ctx context.Context, completion Completion) error {
	if d.completeErr != nil {
		return d.completeErr
	}
	select {
	case d.completions <- completion:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (d *fakeHostDriver) QueryStats(context.Context) (Stats, error) { return Stats{}, nil }

type recordingProcessor struct {
	processed    chan uint64
	lifecycle    chan uint64
	resets       chan DeviceIdentity
	lifecycleErr error
}

func (p *recordingProcessor) Process(_ context.Context, _ usb.Device, op Operation) (Completion, error) {
	p.processed <- op.EndpointSequence
	return Completion{TransferLength: op.TransferLength}, nil
}
func (p *recordingProcessor) Lifecycle(_ context.Context, _ usb.Device, op Operation) error {
	if p.lifecycle != nil {
		p.lifecycle <- op.EndpointSequence
	}
	return p.lifecycleErr
}
func (p *recordingProcessor) Reset(_ usb.Device, identity DeviceIdentity) { p.resets <- identity }

type cancellableProcessor struct {
	started   chan struct{}
	cancelled chan struct{}
}

func (p *cancellableProcessor) Process(ctx context.Context, _ usb.Device, _ Operation) (Completion, error) {
	close(p.started)
	<-ctx.Done()
	close(p.cancelled)
	return Completion{}, ctx.Err()
}
func (*cancellableProcessor) Reset(usb.Device, DeviceIdentity)                       {}
func (*cancellableProcessor) Lifecycle(context.Context, usb.Device, Operation) error { return nil }

type unregisterProcessor struct {
	started   chan struct{}
	cancelled chan struct{}
	reset     chan bool
}

func (p *unregisterProcessor) Process(ctx context.Context, _ usb.Device, _ Operation) (Completion, error) {
	close(p.started)
	<-ctx.Done()
	close(p.cancelled)
	return Completion{}, ctx.Err()
}
func (p *unregisterProcessor) Reset(usb.Device, DeviceIdentity) {
	select {
	case <-p.cancelled:
		p.reset <- true
	default:
		p.reset <- false
	}
}
func (*unregisterProcessor) Lifecycle(context.Context, usb.Device, Operation) error { return nil }

type stubbornProcessor struct {
	started chan struct{}
	release chan struct{}
}

func (p *stubbornProcessor) Process(context.Context, usb.Device, Operation) (Completion, error) {
	close(p.started)
	<-p.release
	return Completion{}, context.Canceled
}
func (*stubbornProcessor) Reset(usb.Device, DeviceIdentity)                       {}
func (*stubbornProcessor) Lifecycle(context.Context, usb.Device, Operation) error { return nil }

type noopProcessor struct{}

func (*noopProcessor) Process(context.Context, usb.Device, Operation) (Completion, error) {
	return Completion{}, nil
}
func (*noopProcessor) Lifecycle(context.Context, usb.Device, Operation) error { return nil }
func (*noopProcessor) Reset(usb.Device, DeviceIdentity)                       {}

type usbdFailureProcessor struct {
	err error
}

func (p *usbdFailureProcessor) Process(context.Context, usb.Device, Operation) (Completion, error) {
	return Completion{}, p.err
}
func (*usbdFailureProcessor) Lifecycle(context.Context, usb.Device, Operation) error { return nil }
func (*usbdFailureProcessor) Reset(usb.Device, DeviceIdentity)                       {}

type testUSBDCompletionError struct {
	status uint32
}

func (e testUSBDCompletionError) Error() string                { return "USB protocol failure" }
func (e testUSBDCompletionError) USBDCompletionStatus() uint32 { return e.status }

type deviceGateProcessor struct {
	blockedDevice uint64
	started       chan struct{}
	independent   chan uint64
	startOnce     sync.Once
}

func (p *deviceGateProcessor) Process(
	ctx context.Context, _ usb.Device, op Operation,
) (Completion, error) {
	if op.DeviceID == p.blockedDevice {
		p.startOnce.Do(func() { close(p.started) })
		<-ctx.Done()
		return Completion{}, ctx.Err()
	}
	select {
	case p.independent <- op.DeviceID:
	case <-ctx.Done():
		return Completion{}, ctx.Err()
	}
	return Completion{TransferLength: op.TransferLength}, nil
}
func (*deviceGateProcessor) Lifecycle(context.Context, usb.Device, Operation) error { return nil }
func (*deviceGateProcessor) Reset(usb.Device, DeviceIdentity)                       {}

type fatalLaneProcessor struct {
	failingDevice   uint64
	started         chan struct{}
	release         chan struct{}
	independent     chan uint64
	queuedProcessed chan struct{}
	startOnce       sync.Once
}

func (*fatalLaneProcessor) Process(context.Context, usb.Device, Operation) (Completion, error) {
	return Completion{}, nil
}
func (p *fatalLaneProcessor) Lifecycle(ctx context.Context, _ usb.Device, op Operation) error {
	if op.DeviceID != p.failingDevice {
		select {
		case p.independent <- op.DeviceID:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	if op.EndpointSequence != 1 {
		select {
		case p.queuedProcessed <- struct{}{}:
		default:
		}
		return nil
	}
	p.startOnce.Do(func() { close(p.started) })
	select {
	case <-p.release:
		return errors.New("injected lane failure")
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (*fatalLaneProcessor) Reset(usb.Device, DeviceIdentity) {}

type cancellationOnlyCompletionDriver struct {
	*fakeHostDriver
}

func (*cancellationOnlyCompletionDriver) Complete(ctx context.Context, _ Completion) error {
	<-ctx.Done()
	return ctx.Err()
}

type deviceBarrierCompletionDriver struct {
	*fakeHostDriver
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

type managementBarrierDriver struct {
	*fakeHostDriver
	management chan Completion
	release    chan struct{}
}

func (d *managementBarrierDriver) Complete(ctx context.Context, completion Completion) error {
	if completion.Token != 1 {
		return d.fakeHostDriver.Complete(ctx, completion)
	}
	select {
	case d.management <- completion:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-d.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *deviceBarrierCompletionDriver) Complete(ctx context.Context, completion Completion) error {
	if completion.Token != 1 {
		return d.fakeHostDriver.Complete(ctx, completion)
	}
	close(d.started)
	<-ctx.Done()
	close(d.canceled)
	<-d.release
	return ctx.Err()
}

type deviceBarrierProcessor struct {
	targetDevice    uint64
	speakerStarted  chan struct{}
	speakerCanceled chan struct{}
	speakerRelease  chan struct{}
	barrierStarted  chan Operation
	barrierRelease  chan struct{}
	processed       chan Operation
	speakerOnce     sync.Once
}

func (p *deviceBarrierProcessor) Process(
	ctx context.Context, _ usb.Device, op Operation,
) (Completion, error) {
	if op.DeviceID != p.targetDevice {
		p.processed <- op
		return Completion{TransferLength: op.TransferLength}, nil
	}
	if isDeviceBarrierOperation(op) {
		p.barrierStarted <- op
		select {
		case <-p.barrierRelease:
			return Completion{TransferLength: op.TransferLength}, nil
		case <-ctx.Done():
			return Completion{}, ctx.Err()
		}
	}
	if op.EndpointAddress == 0x02 {
		p.speakerOnce.Do(func() { close(p.speakerStarted) })
		<-ctx.Done()
		close(p.speakerCanceled)
		<-p.speakerRelease
		return Completion{}, ctx.Err()
	}
	p.processed <- op
	return Completion{TransferLength: op.TransferLength}, nil
}

func (p *deviceBarrierProcessor) Lifecycle(ctx context.Context, _ usb.Device, op Operation) error {
	if isDeviceBarrierOperation(op) {
		p.barrierStarted <- op
		select {
		case <-p.barrierRelease:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.processed <- op
	return nil
}
func (*deviceBarrierProcessor) Reset(usb.Device, DeviceIdentity) {}

type resetGateProcessor struct {
	started chan struct{}
	release chan struct{}
	kind    OperationKind
}

func (*resetGateProcessor) Process(context.Context, usb.Device, Operation) (Completion, error) {
	return Completion{}, nil
}
func (p *resetGateProcessor) Lifecycle(ctx context.Context, _ usb.Device, op Operation) error {
	kind := p.kind
	if kind == 0 {
		kind = OperationDeviceReset
	}
	if op.Kind != kind {
		return nil
	}
	close(p.started)
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (*resetGateProcessor) Reset(usb.Device, DeviceIdentity) {}

type supersededManagementProcessor struct {
	endpointStarted  chan struct{}
	endpointCanceled chan struct{}
	endpointRelease  chan struct{}
	barrierStarted   chan struct{}
}

func (*supersededManagementProcessor) Process(
	context.Context, usb.Device, Operation,
) (Completion, error) {
	return Completion{}, nil
}

func (p *supersededManagementProcessor) Lifecycle(
	ctx context.Context, _ usb.Device, op Operation,
) error {
	switch op.Kind {
	case OperationEndpointReset:
		close(p.endpointStarted)
		<-ctx.Done()
		close(p.endpointCanceled)
		<-p.endpointRelease
		return ctx.Err()
	case OperationDeviceReset:
		close(p.barrierStarted)
	}
	return nil
}

func (*supersededManagementProcessor) Reset(usb.Device, DeviceIdentity) {}

func hostTestDevice() usb.Device {
	return &snapshotDevice{descriptor: usb.Descriptor{
		Device: usb.DeviceDescriptor{
			BcdUSB: 0x0200, BMaxPacketSize0: 64, IDVendor: 1, IDProduct: 2,
			BNumConfigurations: 1, Speed: uint32(DeviceSpeedHigh),
		},
		Interfaces: []usb.InterfaceConfig{{Descriptor: usb.InterfaceDescriptor{
			BInterfaceNumber: 0, BNumEndpoints: 1, BInterfaceClass: 3,
		}, Endpoints: []usb.EndpointDescriptor{{
			BEndpointAddress: 0x81, BMAttributes: 3, WMaxPacketSize: 64, BInterval: 4,
		}}}},
	}}
}

func TestInterruptInputServiceIntervalMatchesUSBContract(t *testing.T) {
	tests := []struct {
		name      string
		speed     uint32
		bInterval uint8
		want      time.Duration
	}{
		{name: "full-speed frames", speed: uint32(DeviceSpeedFull), bInterval: 5, want: 5 * time.Millisecond},
		{name: "high-speed microframes", speed: uint32(DeviceSpeedHigh), bInterval: 4, want: time.Millisecond},
		{name: "maximum high-speed exponent", speed: uint32(DeviceSpeedSuper), bInterval: 16, want: 4096 * time.Millisecond},
		{name: "zero is unscheduled", speed: uint32(DeviceSpeedHigh), bInterval: 0, want: 0},
		{name: "reserved high-speed exponent", speed: uint32(DeviceSpeedHigh), bInterval: 17, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := interruptInputServiceInterval(test.speed, test.bInterval); got != test.want {
				t.Fatalf("service interval=%v want=%v", got, test.want)
			}
		})
	}
}

func TestHostDoesNotSerializeIndependentControllerRegistration(t *testing.T) {
	driver := &independentlyBlockingCreateDriver{
		fakeHostDriver: newFakeHostDriver(), blockedDevice: 81,
		started: make(chan struct{}), release: make(chan struct{}),
	}
	host, err := NewHost(driver, &noopProcessor{}, 2)
	if err != nil {
		t.Fatal(err)
	}

	type registerResult struct {
		identity DeviceIdentity
		err      error
	}
	blockedDone := make(chan registerResult, 1)
	go func() {
		identity, registerErr := host.Register(context.Background(), 81, hostTestDevice())
		blockedDone <- registerResult{identity: identity, err: registerErr}
	}()
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("first controller registration did not reach the driver")
	}

	independentDone := make(chan registerResult, 1)
	go func() {
		identity, registerErr := host.Register(context.Background(), 82, hostTestDevice())
		independentDone <- registerResult{identity: identity, err: registerErr}
	}()
	var independent registerResult
	select {
	case independent = <-independentDone:
		if independent.err != nil {
			t.Fatalf("independent registration failed: %v", independent.err)
		}
	case <-time.After(time.Second):
		t.Fatal("independent controller registration was blocked by another controller")
	}

	close(driver.release)
	var blocked registerResult
	select {
	case blocked = <-blockedDone:
		if blocked.err != nil {
			t.Fatalf("blocked registration failed after release: %v", blocked.err)
		}
	case <-time.After(time.Second):
		t.Fatal("first controller registration did not finish after release")
	}

	if err = host.Unregister(context.Background(), independent.identity); err != nil {
		t.Fatal(err)
	}
	if err = host.Unregister(context.Background(), blocked.identity); err != nil {
		t.Fatal(err)
	}
	host.lifecycleMu.Lock()
	remainingGates := len(host.lifecycles)
	host.lifecycleMu.Unlock()
	if remainingGates != 0 {
		t.Fatalf("lifecycle gates=%d want 0", remainingGates)
	}
}

func TestHostSerializesSameControllerRegistration(t *testing.T) {
	driver := &independentlyBlockingCreateDriver{
		fakeHostDriver: newFakeHostDriver(), blockedDevice: 83,
		started: make(chan struct{}), release: make(chan struct{}),
	}
	host, err := NewHost(driver, &noopProcessor{}, 2)
	if err != nil {
		t.Fatal(err)
	}

	type registerResult struct {
		identity DeviceIdentity
		err      error
	}
	firstDone := make(chan registerResult, 1)
	go func() {
		identity, registerErr := host.Register(context.Background(), 83, hostTestDevice())
		firstDone <- registerResult{identity: identity, err: registerErr}
	}()
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("first same-controller registration did not reach the driver")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, registerErr := host.Register(context.Background(), 83, hostTestDevice())
		secondDone <- registerErr
	}()
	select {
	case registerErr := <-secondDone:
		t.Fatalf("same-controller registration crossed the in-flight create: %v", registerErr)
	case <-time.After(25 * time.Millisecond):
	}

	close(driver.release)
	var first registerResult
	select {
	case first = <-firstDone:
		if first.err != nil {
			t.Fatal(first.err)
		}
	case <-time.After(time.Second):
		t.Fatal("first same-controller registration did not finish")
	}
	select {
	case registerErr := <-secondDone:
		if registerErr == nil || !strings.Contains(registerErr.Error(), "already registered") {
			t.Fatalf("second same-controller registration error=%v", registerErr)
		}
	case <-time.After(time.Second):
		t.Fatal("second same-controller registration did not revalidate after serialization")
	}
	if err = host.Unregister(context.Background(), first.identity); err != nil {
		t.Fatal(err)
	}
}

func TestHostRollsBackRegistrationThatOutlivesServe(t *testing.T) {
	driver := &independentlyBlockingCreateDriver{
		fakeHostDriver: newFakeHostDriver(), blockedDevice: 84,
		started: make(chan struct{}), release: make(chan struct{}),
	}
	host, err := NewHost(driver, &noopProcessor{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancelServe := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- host.Serve(serveCtx) }()

	registerDone := make(chan error, 1)
	go func() {
		_, registerErr := host.Register(context.Background(), 84, hostTestDevice())
		registerDone <- registerErr
	}()
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("registration did not reach blocking PnP create")
	}
	cancelServe()
	select {
	case serveErr := <-serveDone:
		if serveErr != nil {
			t.Fatalf("Serve shutdown: %v", serveErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop around in-flight registration")
	}
	close(driver.release)
	select {
	case registerErr := <-registerDone:
		if registerErr == nil || !strings.Contains(registerErr.Error(), "registration was in flight") {
			t.Fatalf("registration error=%v, want terminal-session rollback", registerErr)
		}
	case <-time.After(time.Second):
		t.Fatal("registration did not finish after PnP create was released")
	}

	host.mu.RLock()
	_, leaked := host.devices[84]
	host.mu.RUnlock()
	driver.mu.Lock()
	created, destroyed := len(driver.created), len(driver.destroyed)
	driver.mu.Unlock()
	if leaked || created != 1 || destroyed != 1 {
		t.Fatalf("terminal registration leaked=%t created=%d destroyed=%d", leaked, created, destroyed)
	}
}

func TestHostRepeatedCreateRemoveLeavesOnlyGenerationHistory(t *testing.T) {
	driver := newFakeHostDriver()
	host, err := NewHost(driver, &noopProcessor{}, 4)
	if err != nil {
		t.Fatal(err)
	}
	const cycles = 512
	for cycle := 1; cycle <= cycles; cycle++ {
		identity, registerErr := host.Register(context.Background(), 72, hostTestDevice())
		if registerErr != nil {
			t.Fatalf("cycle %d register: %v", cycle, registerErr)
		}
		if identity.Generation != uint32(cycle) {
			t.Fatalf("cycle %d generation=%d", cycle, identity.Generation)
		}
		if unregisterErr := host.Unregister(context.Background(), identity); unregisterErr != nil {
			t.Fatalf("cycle %d unregister: %v", cycle, unregisterErr)
		}
	}

	host.mu.RLock()
	devices, lanes := len(host.devices), len(host.lanes)
	generation := host.generations[72]
	host.mu.RUnlock()
	host.operationMu.Lock()
	operations := len(host.operations)
	host.operationMu.Unlock()
	driver.mu.Lock()
	created, destroyed := len(driver.created), len(driver.destroyed)
	driver.mu.Unlock()
	if devices != 0 || lanes != 0 || operations != 0 || generation != cycles ||
		created != cycles || destroyed != cycles {
		t.Fatalf("devices=%d lanes=%d operations=%d generation=%d created=%d destroyed=%d",
			devices, lanes, operations, generation, created, destroyed)
	}
}

type inputPublisherTestDevice struct {
	descriptor usb.Descriptor
	reports    chan []byte
}

type directInputPublisherTestDevice struct {
	*inputPublisherTestDevice
	buffers chan *byte
}

type cachedDeadlineInputPublisherTestDevice struct {
	*inputPublisherTestDevice
	cached []byte
}

type scheduledInputPublisherTestDevice struct {
	*inputPublisherTestDevice
	deadlines    chan (<-chan time.Time)
	fallbackRead atomic.Int32
}

type staleDeadlineInputPublisherTestDevice struct {
	*inputPublisherTestDevice
	firstStarted  chan struct{}
	secondElapsed chan time.Duration
	calls         atomic.Int32
}

type controlledInputAttempt struct {
	context.Context
	deadline   time.Time
	done       chan struct{}
	once       sync.Once
	mu         sync.Mutex
	err        error
	stopParent func() bool
}

func newControlledInputAttempt(parent context.Context, interval time.Duration) *controlledInputAttempt {
	attempt := &controlledInputAttempt{
		Context: parent, deadline: time.Now().Add(interval), done: make(chan struct{}),
	}
	attempt.stopParent = context.AfterFunc(parent, func() { attempt.finish(parent.Err()) })
	return attempt
}

func (c *controlledInputAttempt) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *controlledInputAttempt) Done() <-chan struct{}       { return c.done }
func (c *controlledInputAttempt) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
func (c *controlledInputAttempt) finish(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	})
}
func (c *controlledInputAttempt) expire() { c.finish(context.DeadlineExceeded) }
func (c *controlledInputAttempt) cancel() {
	if c.stopParent != nil {
		c.stopParent()
	}
	c.finish(context.Canceled)
}

func newInputPublisherTestDevice() *inputPublisherTestDevice {
	base := hostTestDevice().GetDescriptor()
	return &inputPublisherTestDevice{descriptor: *base, reports: make(chan []byte, 4)}
}

func newDirectInputPublisherTestDevice() *directInputPublisherTestDevice {
	return &directInputPublisherTestDevice{
		inputPublisherTestDevice: newInputPublisherTestDevice(),
		buffers:                  make(chan *byte, 4),
	}
}

func newCachedDeadlineInputPublisherTestDevice(report []byte) *cachedDeadlineInputPublisherTestDevice {
	return &cachedDeadlineInputPublisherTestDevice{
		inputPublisherTestDevice: newInputPublisherTestDevice(),
		cached:                   append([]byte(nil), report...),
	}
}

func newScheduledInputPublisherTestDevice() *scheduledInputPublisherTestDevice {
	return &scheduledInputPublisherTestDevice{
		inputPublisherTestDevice: newInputPublisherTestDevice(),
		deadlines:                make(chan (<-chan time.Time), 32),
	}
}

func newStaleDeadlineInputPublisherTestDevice() *staleDeadlineInputPublisherTestDevice {
	device := &staleDeadlineInputPublisherTestDevice{
		inputPublisherTestDevice: newInputPublisherTestDevice(),
		firstStarted:             make(chan struct{}),
		secondElapsed:            make(chan time.Duration, 1),
	}
	// A high-speed bInterval of 8 is a 16 ms service period. The longer
	// interval gives this deterministic stale-tick test enough scheduling
	// margin even on a busy Windows runner.
	device.descriptor.Interfaces[0].Endpoints[0].BInterval = 8
	return device
}

func (d *directInputPublisherTestDevice) ReadInterruptInput(
	ctx context.Context, _ uint32, dst []byte,
) (int, error) {
	if len(dst) == 0 {
		return 0, errors.New("empty native input buffer")
	}
	select {
	case report := <-d.reports:
		if len(report) > len(dst) {
			return 0, errors.New("native input buffer is too short")
		}
		d.buffers <- &dst[0]
		copy(dst, report)
		return len(report), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (d *cachedDeadlineInputPublisherTestDevice) ReadInterruptInput(
	ctx context.Context, _ uint32, dst []byte,
) (int, error) {
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return 0, ctx.Err()
	}
	if len(d.cached) > len(dst) {
		return 0, errors.New("native input buffer is too short for cached report")
	}
	copy(dst, d.cached)
	return len(d.cached), nil
}

func (d *scheduledInputPublisherTestDevice) ReadInterruptInput(
	context.Context, uint32, []byte,
) (int, error) {
	d.fallbackRead.Add(1)
	return 0, errors.New("scheduled input used the timer-context fallback")
}

func (d *scheduledInputPublisherTestDevice) ReadScheduledInterruptInput(
	ctx context.Context, deadline <-chan time.Time, _ uint32, dst []byte,
) (int, error) {
	select {
	case d.deadlines <- deadline:
	default:
	}
	select {
	case report := <-d.reports:
		if len(report) > len(dst) {
			return 0, errors.New("native input buffer is too short")
		}
		copy(dst, report)
		return len(report), nil
	case <-deadline:
		return 0, context.DeadlineExceeded
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (d *staleDeadlineInputPublisherTestDevice) ReadInterruptInput(
	context.Context, uint32, []byte,
) (int, error) {
	return 0, errors.New("stale-deadline test used the timer-context fallback")
}

func (d *staleDeadlineInputPublisherTestDevice) ReadScheduledInterruptInput(
	ctx context.Context, deadline <-chan time.Time, _ uint32, dst []byte,
) (int, error) {
	if d.calls.Add(1) == 1 {
		close(d.firstStarted)
		// Deliberately leave the first deadline unread. This models the hardest
		// event/deadline race: a controller event wins after the timer's nominal
		// expiry and the host must stop/reset without leaking that old tick into
		// the next USB service interval.
		select {
		case report := <-d.reports:
			copy(dst, report)
			return len(report), nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	started := time.Now()
	select {
	case <-deadline:
		d.secondElapsed <- time.Since(started)
		dst[0] = 0x7e
		return 1, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (d *inputPublisherTestDevice) HandleTransfer(
	ctx context.Context, _ uint32, _ uint32, _ []byte,
) []byte {
	select {
	case report := <-d.reports:
		return report
	case <-ctx.Done():
		return nil
	}
}

func (d *inputPublisherTestDevice) GetDescriptor() *usb.Descriptor { return &d.descriptor }
func (*inputPublisherTestDevice) GetDeviceSpecificArgs() map[string]any {
	return nil
}

func TestHostPublishesInterruptInputDirectlyAfterEndpointStart(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 4)}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 2),
		resets: make(chan DeviceIdentity, 1),
	}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	device := newInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 44, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint start was not processed")
	}
	device.reports <- []byte{1, 2, 3, 4}
	select {
	case report := <-driver.reports:
		if report.DeviceID != identity.DeviceID || report.Generation != identity.Generation ||
			report.EndpointAddress != 0x81 || report.Sequence != 1 ||
			string(report.Payload) != string([]byte{1, 2, 3, 4}) {
			t.Fatalf("unexpected direct input report: %+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("interrupt-IN report did not use the direct publisher")
	}

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 2, Kind: OperationEndpointPurge,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint purge was not processed")
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func TestHostReusesOneDescriptorSizedDirectInputBuffer(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 4)}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 2),
		resets: make(chan DeviceIdentity, 1),
	}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	device := newDirectInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 45, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint start was not processed")
	}

	device.reports <- []byte{1, 2, 3, 4}
	first := <-driver.reports
	firstBuffer := <-device.buffers
	device.reports <- []byte{5, 6}
	second := <-driver.reports
	secondBuffer := <-device.buffers
	if firstBuffer != secondBuffer {
		t.Fatal("direct input publisher allocated a replacement endpoint buffer")
	}
	if string(first.Payload) != string([]byte{1, 2, 3, 4}) ||
		string(second.Payload) != string([]byte{5, 6}) {
		t.Fatalf("direct input payloads first=%v second=%v", first.Payload, second.Payload)
	}
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("direct input sequences first=%d second=%d", first.Sequence, second.Sequence)
	}

	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func TestHostReusesOneDeadlineTimerForScheduledInterruptInput(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 4)}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 2),
		resets: make(chan DeviceIdentity, 1),
	}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	device := newScheduledInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 451, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint start was not processed")
	}

	device.reports <- []byte{1, 2, 3}
	select {
	case <-driver.reports:
	case <-time.After(time.Second):
		t.Fatal("first scheduled input report was not submitted")
	}
	device.reports <- []byte{4, 5, 6}
	select {
	case <-driver.reports:
	case <-time.After(time.Second):
		t.Fatal("second scheduled input report was not submitted")
	}

	var first, second <-chan time.Time
	select {
	case first = <-device.deadlines:
	case <-time.After(time.Second):
		t.Fatal("scheduled input did not receive a deadline")
	}
	select {
	case second = <-device.deadlines:
	case <-time.After(time.Second):
		t.Fatal("scheduled input did not receive a second deadline")
	}
	if first != second {
		t.Fatal("scheduled input allocated a replacement endpoint timer")
	}
	if calls := device.fallbackRead.Load(); calls != 0 {
		t.Fatalf("scheduled input used fallback ReadInterruptInput %d time(s)", calls)
	}

	// Endpoint reset must synchronously cancel the blocked scheduled read,
	// dispose its timer, and start a fresh publisher only after lifecycle ACK.
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 2, DeviceSequence: 2,
		Kind: OperationEndpointReset,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint reset did not join the scheduled input publisher")
	}
	device.reports <- []byte{7, 8, 9}
	select {
	case <-driver.reports:
	case <-time.After(time.Second):
		t.Fatal("scheduled input did not resume after endpoint reset")
	}
	resetDeadline := time.After(time.Second)
	for {
		select {
		case afterReset := <-device.deadlines:
			if afterReset != first {
				goto resetTimerObserved
			}
		case <-resetDeadline:
			t.Fatal("endpoint reset retained the old publisher timer")
		}
	}

resetTimerObserved:

	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func TestHostTimerResetCannotReplayExpiredDeadlineIntoNextInput(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 4)}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 2),
		resets: make(chan DeviceIdentity, 1),
	}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	device := newStaleDeadlineInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 452, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint start was not processed")
	}
	select {
	case <-device.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first scheduled read did not start")
	}

	// Let the first 16 ms timer expire without receiving its tick, then make
	// the controller event win. Go 1.23+ Timer.Stop/Reset guarantees that the
	// expired value cannot satisfy the next receive. The host also drains a
	// buffered tick when the legacy timer implementation is forced by GODEBUG.
	time.Sleep(25 * time.Millisecond)
	device.reports <- []byte{0x11}
	select {
	case <-driver.reports:
	case <-time.After(time.Second):
		t.Fatal("controller event was not submitted")
	}
	select {
	case elapsed := <-device.secondElapsed:
		if elapsed < 12*time.Millisecond {
			t.Fatalf("expired deadline leaked into next 16 ms interval after %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("next service deadline did not fire")
	}
	select {
	case report := <-driver.reports:
		if string(report.Payload) != string([]byte{0x7e}) {
			t.Fatalf("deadline report=%x want=7e", report.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline report was not submitted")
	}

	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop after scheduled deadline race")
	}
}

func TestHostInputPublisherDoesNotWaitForGlobalRoutingLock(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 4)}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 2),
		resets: make(chan DeviceIdentity, 1),
	}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	device := newInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 441, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint start was not processed")
	}

	deadline := time.Now().Add(time.Second)
	for {
		host.mu.RLock()
		entry := host.devices[identity.DeviceID]
		publisherReady := entry != nil && entry.publishers[0x81] != nil
		host.mu.RUnlock()
		if publisherReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("direct input publisher did not start")
		}
		time.Sleep(time.Millisecond)
	}

	// Hold the host-wide routing lock exactly while a fresh state is published.
	// A per-endpoint input sequence must still reach the direct driver lane;
	// otherwise unrelated lifecycle/media work can stall every controller.
	host.mu.Lock()
	device.reports <- []byte{9, 8, 7, 6}
	select {
	case report := <-driver.reports:
		host.mu.Unlock()
		if report.Sequence != 1 || string(report.Payload) != string([]byte{9, 8, 7, 6}) {
			t.Fatalf("unexpected lock-independent input report: %+v", report)
		}
	case <-time.After(250 * time.Millisecond):
		host.mu.Unlock()
		t.Fatal("direct input waited for the global host routing lock")
	}

	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func TestHostRestoresInputPublisherAfterFailedTransactionalRemoval(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 4)}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 1),
		resets: make(chan DeviceIdentity, 1),
	}
	host, _ := NewHost(driver, processor, 2)
	device := newInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 45, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint start was not processed")
	}
	device.reports <- []byte{1}
	select {
	case report := <-driver.reports:
		if report.Sequence != 1 {
			t.Fatalf("first sequence=%d want=1", report.Sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("first input report was not submitted")
	}

	driver.mu.Lock()
	driver.destroyErr = errors.New("plug-out still pending")
	driver.mu.Unlock()
	if err = host.Unregister(context.Background(), identity); err == nil {
		t.Fatal("failed removal unexpectedly succeeded")
	}
	device.reports <- []byte{2}
	select {
	case report := <-driver.reports:
		if report.Sequence != 2 || string(report.Payload) != string([]byte{2}) {
			t.Fatalf("restored publisher report=%+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("publisher was not restored after failed removal")
	}

	driver.mu.Lock()
	driver.destroyErr = nil
	driver.mu.Unlock()
	if err = host.Unregister(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func TestHostRestartsInputPublisherAcrossD0WithoutResettingSequence(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 4)}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 3),
		resets: make(chan DeviceIdentity, 1),
	}
	host, _ := NewHost(driver, processor, 2)
	device := newScheduledInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 46, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint start was not processed")
	}
	device.reports <- []byte{1}
	select {
	case report := <-driver.reports:
		if report.Sequence != 1 {
			t.Fatalf("first sequence=%d want=1", report.Sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("first input report was not submitted")
	}

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0, EndpointSequence: 1, DeviceSequence: 2,
		Kind: OperationDeviceD0Exit,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("D0 exit was not processed")
	}
	device.reports <- []byte{2}
	select {
	case report := <-driver.reports:
		t.Fatalf("report submitted while device was outside D0: %+v", report)
	case <-time.After(25 * time.Millisecond):
	}

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0, EndpointSequence: 2, DeviceSequence: 3,
		Kind: OperationDeviceD0Entry,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("D0 entry was not processed")
	}
	select {
	case report := <-driver.reports:
		if report.Sequence != 2 || string(report.Payload) != string([]byte{2}) {
			t.Fatalf("D0-restored publisher report=%+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("publisher did not resume after D0 entry")
	}

	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func TestHostDoesNotResurrectInputFromPreD0ExitEndpointStart(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 4)}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 3),
		resets: make(chan DeviceIdentity, 1),
	}
	host, _ := NewHost(driver, processor, 4)
	device := newInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 47, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()

	// Multiple dequeue workers may deliver the device-wide D0 exit before an
	// older endpoint-start notification. The announced barrier must retire that
	// pre-D0 callback without applying it, then process the exit once the device
	// sequence is contiguous.
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0, EndpointSequence: 1, DeviceSequence: 2,
		Kind: OperationDeviceD0Exit,
	}
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("D0 exit was not processed after the older sequence was retired")
	}
	select {
	case sequence := <-processor.lifecycle:
		t.Fatalf("superseded endpoint start reached lifecycle processor as sequence %d", sequence)
	default:
	}
	device.reports <- []byte{1}
	select {
	case report := <-driver.reports:
		t.Fatalf("stale endpoint start resurrected input outside D0: %+v", report)
	case <-time.After(25 * time.Millisecond):
	}

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0, EndpointSequence: 2, DeviceSequence: 3,
		Kind: OperationDeviceD0Entry,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("D0 entry was not processed")
	}
	select {
	case report := <-driver.reports:
		if report.Sequence != 1 || string(report.Payload) != string([]byte{1}) {
			t.Fatalf("D0-restored publisher report=%+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("publisher did not resume after the ordered D0 entry")
	}

	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func TestHostPausesDirectInputAcrossAcknowledgedDeviceReset(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 4)}
	processor := &resetGateProcessor{started: make(chan struct{}), release: make(chan struct{})}
	host, _ := NewHost(driver, processor, 4)
	device := newInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 48, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationEndpointStart,
	}
	device.reports <- []byte{1}
	select {
	case report := <-driver.reports:
		if report.Sequence != 1 {
			t.Fatalf("first sequence=%d want=1", report.Sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("first input report was not submitted")
	}

	driver.operations <- Operation{
		Token:    0x0000000180000001,
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0, EndpointSequence: 1, DeviceSequence: 2,
		Kind: OperationDeviceReset,
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("device reset did not reach processor")
	}
	device.reports <- []byte{2}
	select {
	case report := <-driver.reports:
		t.Fatalf("input crossed an unacknowledged device reset: %+v", report)
	case <-time.After(25 * time.Millisecond):
	}

	close(processor.release)
	select {
	case completion := <-driver.completions:
		if completion.Token != 0x0000000180000001 || completion.Status != 0 {
			t.Fatalf("device reset acknowledgement=%+v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("device reset was not acknowledged")
	}
	select {
	case report := <-driver.reports:
		if report.Sequence != 2 || string(report.Payload) != string([]byte{2}) {
			t.Fatalf("reset-restored publisher report=%+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("publisher did not resume after device reset acknowledgement")
	}

	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func TestHostPausesDirectInputAcrossSetConfigurationBarrier(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 4)}
	processor := &deviceBarrierProcessor{
		targetDevice:    50,
		speakerStarted:  make(chan struct{}),
		speakerCanceled: make(chan struct{}),
		speakerRelease:  make(chan struct{}),
		barrierStarted:  make(chan Operation, 1),
		barrierRelease:  make(chan struct{}),
		processed:       make(chan Operation, 2),
	}
	host, _ := NewHost(driver, processor, 4)
	device := newInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), processor.targetDevice, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationEndpointStart,
	}
	select {
	case op := <-processor.processed:
		if op.Kind != OperationEndpointStart {
			t.Fatalf("processed %+v before endpoint start", op)
		}
	case <-time.After(time.Second):
		t.Fatal("endpoint start was not processed")
	}
	device.reports <- []byte{1}
	select {
	case report := <-driver.reports:
		if report.Sequence != 1 {
			t.Fatalf("first sequence=%d want=1", report.Sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("first input report was not submitted")
	}

	driver.operations <- Operation{
		Token: 2, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0, EndpointSequence: 1, DeviceSequence: 2,
		Kind: OperationControl,
		SetupPacket: [8]byte{
			usbRequestTypeStandardToDevice, usbRequestSetConfiguration, 1,
		},
	}
	select {
	case <-processor.barrierStarted:
	case <-time.After(time.Second):
		t.Fatal("SET_CONFIGURATION did not reach the processor")
	}
	device.reports <- []byte{2}
	select {
	case report := <-driver.reports:
		t.Fatalf("input crossed an active SET_CONFIGURATION barrier: %+v", report)
	case <-time.After(25 * time.Millisecond):
	}

	close(processor.barrierRelease)
	select {
	case completion := <-driver.completions:
		if completion.Token != 2 || completion.Status != 0 {
			t.Fatalf("SET_CONFIGURATION completion=%+v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("SET_CONFIGURATION was not completed")
	}
	select {
	case report := <-driver.reports:
		if report.Sequence != 2 || string(report.Payload) != string([]byte{2}) {
			t.Fatalf("configuration-restored publisher report=%+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("publisher did not resume after SET_CONFIGURATION")
	}

	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func TestHostPausesDirectInputAcrossAcknowledgedEndpointReset(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 4)}
	processor := &resetGateProcessor{
		started: make(chan struct{}), release: make(chan struct{}), kind: OperationEndpointReset,
	}
	host, _ := NewHost(driver, processor, 4)
	device := newInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 49, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationEndpointStart,
	}
	device.reports <- []byte{1}
	select {
	case report := <-driver.reports:
		if report.Sequence != 1 {
			t.Fatalf("first sequence=%d want=1", report.Sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("first input report was not submitted")
	}

	driver.operations <- Operation{
		Token:    0x0000000180000002,
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 2, DeviceSequence: 2,
		Kind: OperationEndpointReset,
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("endpoint reset did not reach processor")
	}
	device.reports <- []byte{2}
	select {
	case report := <-driver.reports:
		t.Fatalf("input crossed an unacknowledged endpoint reset: %+v", report)
	case <-time.After(25 * time.Millisecond):
	}

	close(processor.release)
	select {
	case completion := <-driver.completions:
		if completion.Token != 0x0000000180000002 || completion.Status != 0 {
			t.Fatalf("endpoint reset acknowledgement=%+v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("endpoint reset was not acknowledged")
	}
	select {
	case report := <-driver.reports:
		if report.Sequence != 2 || string(report.Payload) != string([]byte{2}) {
			t.Fatalf("reset-restored publisher report=%+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("publisher did not resume after endpoint reset acknowledgement")
	}

	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func TestHostRestartsInputPublisherAfterEndpointPurgeWithoutResettingSequence(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 4)}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 3),
		resets: make(chan DeviceIdentity, 1),
	}
	host, _ := NewHost(driver, processor, 2)
	device := newScheduledInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 47, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationEndpointStart,
	}
	<-processor.lifecycle
	device.reports <- []byte{1}
	select {
	case report := <-driver.reports:
		if report.Sequence != 1 {
			t.Fatalf("first sequence=%d want=1", report.Sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("first input report was not submitted")
	}

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 2, Kind: OperationEndpointPurge,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint purge was not processed")
	}
	device.reports <- []byte{2}
	select {
	case report := <-driver.reports:
		t.Fatalf("report submitted while endpoint was purged: %+v", report)
	case <-time.After(25 * time.Millisecond):
	}

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 3, Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint restart was not processed")
	}
	select {
	case report := <-driver.reports:
		if report.Sequence != 2 || string(report.Payload) != string([]byte{2}) {
			t.Fatalf("endpoint-restored publisher report=%+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("publisher did not resume after endpoint restart")
	}

	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func TestHostCommitsEncodedInputBeforePurgeAndResetLifecycleBoundaries(t *testing.T) {
	baseDriver := &fastInputDriver{
		fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 8),
	}
	driver := &gatedFastInputDriver{
		fastInputDriver: baseDriver, gates: make(chan *inputSubmitGate, 2),
	}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 8),
		resets: make(chan DeviceIdentity, 1),
	}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	device := newScheduledInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 472, device)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	serveDone := make(chan error, 1)
	go func() { serveDone <- host.Serve(serveCtx) }()

	endpointSequence, deviceSequence := uint64(1), uint64(1)
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: endpointSequence,
		DeviceSequence: deviceSequence, Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint start was not processed")
	}

	device.reports <- []byte{1}
	select {
	case report := <-driver.reports:
		if report.Sequence != 1 || string(report.Payload) != string([]byte{1}) {
			t.Fatalf("first accepted report=%+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("first input report was not accepted")
	}

	commitAcrossLifecycle := func(kind OperationKind, payload byte, wantSequence uint64) {
		t.Helper()
		gate := newInputSubmitGate()
		driver.gates <- gate
		device.reports <- []byte{payload}
		select {
		case candidate := <-gate.started:
			if candidate.Sequence != wantSequence || string(candidate.Payload) != string([]byte{payload}) {
				t.Fatalf("gated candidate=%+v want sequence=%d payload=%d", candidate, wantSequence, payload)
			}
		case <-time.After(time.Second):
			t.Fatalf("input %d did not reach the driver commit boundary", payload)
		}

		endpointSequence++
		deviceSequence++
		driver.operations <- Operation{
			DeviceID: identity.DeviceID, Generation: identity.Generation,
			EndpointAddress: 0x81, EndpointSequence: endpointSequence,
			DeviceSequence: deviceSequence, Kind: kind,
		}
		select {
		case sequence := <-processor.lifecycle:
			t.Fatalf("lifecycle sequence %d crossed an uncommitted encoded report", sequence)
		case <-time.After(25 * time.Millisecond):
		}
		select {
		case report := <-driver.reports:
			t.Fatalf("gated report was accepted before driver release: %+v", report)
		default:
		}

		close(gate.release)
		select {
		case report := <-driver.reports:
			if report.Sequence != wantSequence || string(report.Payload) != string([]byte{payload}) {
				t.Fatalf("committed report=%+v want sequence=%d payload=%d", report, wantSequence, payload)
			}
		case <-time.After(time.Second):
			t.Fatalf("encoded report %d was not committed", payload)
		}
		select {
		case <-processor.lifecycle:
		case <-time.After(time.Second):
			t.Fatalf("lifecycle kind %d did not resume after input commit", kind)
		}
	}

	commitAcrossLifecycle(OperationEndpointPurge, 2, 2)
	endpointSequence++
	deviceSequence++
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: endpointSequence,
		DeviceSequence: deviceSequence, Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint restart after purge was not processed")
	}
	device.reports <- []byte{3}
	select {
	case report := <-driver.reports:
		if report.Sequence != 3 || string(report.Payload) != string([]byte{3}) {
			t.Fatalf("post-purge report=%+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("publisher did not resume after purge/start")
	}

	commitAcrossLifecycle(OperationEndpointReset, 4, 4)
	device.reports <- []byte{5}
	select {
	case report := <-driver.reports:
		if report.Sequence != 5 || string(report.Payload) != string([]byte{5}) {
			t.Fatalf("post-reset report=%+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("publisher did not resume after endpoint reset")
	}

	cancelServe()
	select {
	case err = <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func TestHostOwnerCancellationBoundsWedgedEncodedInputCommit(t *testing.T) {
	baseDriver := &fastInputDriver{
		fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 2),
	}
	driver := &gatedFastInputDriver{
		fastInputDriver: baseDriver, gates: make(chan *inputSubmitGate, 1),
	}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 2),
		resets: make(chan DeviceIdentity, 1),
	}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	device := newScheduledInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 473, device)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- host.Serve(context.Background()) }()
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint start was not processed")
	}

	gate := newInputSubmitGate()
	driver.gates <- gate
	device.reports <- []byte{0x5a}
	select {
	case <-gate.started:
	case <-time.After(time.Second):
		t.Fatal("input did not reach wedged driver boundary")
	}
	unregisterDone := make(chan error, 1)
	go func() { unregisterDone <- host.Unregister(context.Background(), identity) }()
	select {
	case unregisterErr := <-unregisterDone:
		t.Fatalf("unregister crossed an uncommitted report: %v", unregisterErr)
	case <-time.After(25 * time.Millisecond):
	}
	driver.mu.Lock()
	destroyedBeforeStop := len(driver.destroyed)
	driver.mu.Unlock()
	if destroyedBeforeStop != 0 {
		t.Fatal("driver removal crossed the pending input commit")
	}

	// Endpoint lifecycle intentionally joins the commit. Owner-session
	// cancellation is the bounded escape hatch for a driver that never accepts
	// it, and must release both Serve and a waiting Unregister.
	host.Close()
	select {
	case err = <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner cancellation did not release the wedged publisher")
	}
	select {
	case err = <-unregisterDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner cancellation did not release unregister")
	}
	select {
	case report := <-driver.reports:
		t.Fatalf("owner-cancelled input was accepted: %+v", report)
	default:
	}
}

func TestHostReplaysCachedInputAtServiceDeadlineAcrossPurgeStart(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 8)}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 4),
		resets: make(chan DeviceIdentity, 1),
	}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	type attempt struct {
		context  *controlledInputAttempt
		interval time.Duration
	}
	attempts := make(chan attempt, 8)
	host.inputAttemptContext = func(parent context.Context, interval time.Duration) (context.Context, context.CancelFunc) {
		controlled := newControlledInputAttempt(parent, interval)
		attempts <- attempt{context: controlled, interval: interval}
		return controlled, controlled.cancel
	}
	device := newCachedDeadlineInputPublisherTestDevice([]byte{0x11, 0x22, 0x33})
	identity, err := host.Register(context.Background(), 471, device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint start was not processed")
	}
	var firstAttempt attempt
	select {
	case firstAttempt = <-attempts:
	case <-time.After(time.Second):
		t.Fatal("publisher did not arm its first service deadline")
	}
	if firstAttempt.interval != time.Millisecond {
		t.Fatalf("high-speed bInterval=4 deadline=%v want=1ms", firstAttempt.interval)
	}
	firstAttempt.context.expire()
	select {
	case report := <-driver.reports:
		if report.Sequence != 1 || string(report.Payload) != string([]byte{0x11, 0x22, 0x33}) {
			t.Fatalf("first cached input report=%+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("idle controller did not publish cached state at its service deadline")
	}

	// Join the next blocked read through purge. Once lifecycle processing
	// returns, the old publisher cannot submit a late report.
	select {
	case <-attempts:
	case <-time.After(time.Second):
		t.Fatal("publisher did not arm its next service deadline")
	}
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 2, Kind: OperationEndpointPurge,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint purge was not processed")
	}
	select {
	case report := <-driver.reports:
		t.Fatalf("cached report crossed completed purge: %+v", report)
	default:
	}

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 3, Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint restart was not processed")
	}
	var restartedAttempt attempt
	select {
	case restartedAttempt = <-attempts:
	case <-time.After(time.Second):
		t.Fatal("restarted publisher did not arm a service deadline")
	}
	restartedAttempt.context.expire()
	select {
	case report := <-driver.reports:
		if report.Sequence != 2 || string(report.Payload) != string([]byte{0x11, 0x22, 0x33}) {
			t.Fatalf("cached report after endpoint restart=%+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("restarted publisher did not replay cached controller state")
	}

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 4, Kind: OperationEndpointPurge,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("final endpoint purge was not processed")
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func trackAndDispatch(host *Host, op Operation) error {
	if err := host.trackOperation(op); err != nil {
		return fmt.Errorf("track token %d: %w", op.Token, err)
	}
	return host.dispatch(context.Background(), op)
}

func TestHostDeviceBarriersJoinBlockedSpeakerBeforeLaterMicAndHID(t *testing.T) {
	tests := []struct {
		name    string
		kind    OperationKind
		control bool
	}{
		{name: "device_reset", kind: OperationDeviceReset},
		{name: "D0_exit", kind: OperationDeviceD0Exit},
		{name: "D0_entry", kind: OperationDeviceD0Entry},
		{name: "set_configuration", kind: OperationControl, control: true},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver := newFakeHostDriver()
			processor := &deviceBarrierProcessor{
				targetDevice:    uint64(96 + index*2),
				speakerStarted:  make(chan struct{}),
				speakerCanceled: make(chan struct{}),
				speakerRelease:  make(chan struct{}),
				barrierStarted:  make(chan Operation, 1),
				barrierRelease:  make(chan struct{}),
				processed:       make(chan Operation, 4),
			}
			host, err := NewHost(driver, processor, 4)
			if err != nil {
				t.Fatal(err)
			}
			target, err := host.Register(context.Background(), processor.targetDevice, hostTestDevice())
			if err != nil {
				t.Fatal(err)
			}
			independent, err := host.Register(context.Background(), processor.targetDevice+1, hostTestDevice())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				host.cancelAllOperations()
				_ = host.Unregister(context.Background(), target)
				_ = host.Unregister(context.Background(), independent)
			})

			speaker := Operation{
				Token: 1, DeviceID: target.DeviceID, Generation: target.Generation,
				EndpointAddress: 0x02, EndpointSequence: 1, DeviceSequence: 1,
				Kind: OperationTransfer,
			}
			if err = trackAndDispatch(host, speaker); err != nil {
				t.Fatal(err)
			}
			select {
			case <-processor.speakerStarted:
			case <-time.After(time.Second):
				t.Fatal("speaker callback did not start")
			}

			// Multiple dequeue workers may deliver later endpoint work before the
			// device-wide boundary. These lanes must remain parked at the global
			// device sequence instead of overtaking the reset/configuration change.
			for _, op := range []Operation{
				{
					Token: 3, DeviceID: target.DeviceID, Generation: target.Generation,
					EndpointAddress: 0x83, EndpointSequence: 1, DeviceSequence: 3,
					Kind: OperationTransfer,
				},
				{
					Token: 4, DeviceID: target.DeviceID, Generation: target.Generation,
					EndpointAddress: 0x04, EndpointSequence: 1, DeviceSequence: 4,
					Kind: OperationTransfer,
				},
			} {
				if err = trackAndDispatch(host, op); err != nil {
					t.Fatal(err)
				}
			}

			barrier := Operation{
				DeviceID: target.DeviceID, Generation: target.Generation,
				EndpointAddress: 0, EndpointSequence: 1, DeviceSequence: 2,
				Kind: test.kind,
			}
			if test.control {
				barrier.Token = 2
				barrier.SetupPacket = [8]byte{
					usbRequestTypeStandardToDevice, usbRequestSetConfiguration, 1,
				}
				err = trackAndDispatch(host, barrier)
			} else {
				err = host.dispatch(context.Background(), barrier)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-processor.speakerCanceled:
			case <-time.After(time.Second):
				t.Fatal("device barrier did not cancel the older speaker callback")
			}
			select {
			case op := <-processor.barrierStarted:
				t.Fatalf("barrier sequence %d ran before the older callback joined", op.DeviceSequence)
			case <-time.After(25 * time.Millisecond):
			}

			if err = trackAndDispatch(host, Operation{
				Token: 100, DeviceID: independent.DeviceID, Generation: independent.Generation,
				EndpointAddress: 0x02, EndpointSequence: 1, DeviceSequence: 1,
				Kind: OperationTransfer,
			}); err != nil {
				t.Fatal(err)
			}
			select {
			case op := <-processor.processed:
				if op.DeviceID != independent.DeviceID {
					t.Fatalf("device barrier leaked target operation %+v before release", op)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked target device serialized an independent controller")
			}

			close(processor.speakerRelease)
			select {
			case op := <-processor.barrierStarted:
				if op.DeviceSequence != barrier.DeviceSequence {
					t.Fatalf("started barrier sequence=%d want=%d", op.DeviceSequence, barrier.DeviceSequence)
				}
			case <-time.After(time.Second):
				t.Fatal("device barrier did not start after the older callback joined")
			}
			select {
			case op := <-processor.processed:
				t.Fatalf("later endpoint 0x%02x overtook the active device barrier", op.EndpointAddress)
			case <-time.After(25 * time.Millisecond):
			}

			close(processor.barrierRelease)
			seen := make(map[uint8]bool)
			for len(seen) != 2 {
				select {
				case op := <-processor.processed:
					if op.DeviceID != target.DeviceID || (op.EndpointAddress != 0x83 && op.EndpointAddress != 0x04) {
						t.Fatalf("unexpected post-barrier operation %+v", op)
					}
					seen[op.EndpointAddress] = true
				case <-time.After(time.Second):
					t.Fatalf("post-barrier endpoints processed=%v want mic 0x83 and HID 0x04", seen)
				}
			}

			wantCompletions := 3
			if test.control {
				wantCompletions++
			}
			for range wantCompletions {
				select {
				case completion := <-driver.completions:
					if completion.Token == speaker.Token {
						t.Fatal("canceled pre-barrier speaker callback published after the boundary")
					}
				case <-time.After(time.Second):
					t.Fatal("expected post-barrier completion was not published")
				}
			}
			select {
			case completion := <-driver.completions:
				if completion.Token == speaker.Token {
					t.Fatal("canceled pre-barrier speaker callback published late")
				}
				t.Fatalf("unexpected extra completion %+v", completion)
			case <-time.After(25 * time.Millisecond):
			}
		})
	}
}

func TestHostDeviceBarrierCancelsAndJoinsBlockedCompletion(t *testing.T) {
	driver := &deviceBarrierCompletionDriver{
		fakeHostDriver: newFakeHostDriver(),
		started:        make(chan struct{}),
		canceled:       make(chan struct{}),
		release:        make(chan struct{}),
	}
	processor := &resetGateProcessor{started: make(chan struct{}), release: make(chan struct{})}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := host.Register(context.Background(), 105, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		host.cancelAllOperations()
		_ = host.Unregister(context.Background(), identity)
	})

	if err = trackAndDispatch(host, Operation{
		Token: 1, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x02, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationTransfer,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("pre-reset driver completion did not start")
	}
	if err = host.dispatch(context.Background(), Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0, EndpointSequence: 1, DeviceSequence: 2,
		Kind: OperationDeviceReset,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-driver.canceled:
	case <-time.After(time.Second):
		t.Fatal("device reset did not cancel the older blocked completion")
	}
	select {
	case <-processor.started:
		t.Fatal("device reset ran before the canceled completion callback joined")
	case <-time.After(25 * time.Millisecond):
	}

	close(driver.release)
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("device reset did not run after the completion callback joined")
	}
	close(processor.release)
	select {
	case completion := <-driver.completions:
		t.Fatalf("canceled pre-reset completion was published: %+v", completion)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestHostDeviceBarrierCompletesSupersededManagementRequest(t *testing.T) {
	driver := &managementBarrierDriver{
		fakeHostDriver: newFakeHostDriver(),
		management:     make(chan Completion, 1),
		release:        make(chan struct{}),
	}
	processor := &supersededManagementProcessor{
		endpointStarted:  make(chan struct{}),
		endpointCanceled: make(chan struct{}),
		endpointRelease:  make(chan struct{}),
		barrierStarted:   make(chan struct{}),
	}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := host.Register(context.Background(), 106, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		host.cancelAllOperations()
		_ = host.Unregister(context.Background(), identity)
	})

	if err = host.dispatch(context.Background(), Operation{
		Token: 1, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x02, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationEndpointReset,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.endpointStarted:
	case <-time.After(time.Second):
		t.Fatal("token-bearing endpoint reset did not start")
	}
	if err = host.dispatch(context.Background(), Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0, EndpointSequence: 1, DeviceSequence: 2,
		Kind: OperationDeviceReset,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.endpointCanceled:
	case <-time.After(time.Second):
		t.Fatal("device reset did not cancel the older endpoint reset callback")
	}
	select {
	case <-processor.barrierStarted:
		t.Fatal("device reset ran before the older endpoint reset callback joined")
	case <-time.After(25 * time.Millisecond):
	}

	close(processor.endpointRelease)
	select {
	case completion := <-driver.management:
		if completion.Token != 1 || completion.Status != statusUnsuccessful {
			t.Fatalf("superseded management completion=%+v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("superseded endpoint reset left its UdeCx management token stranded")
	}
	select {
	case <-processor.barrierStarted:
		t.Fatal("device reset ran before the management completion joined")
	case <-time.After(25 * time.Millisecond):
	}

	close(driver.release)
	select {
	case <-processor.barrierStarted:
	case <-time.After(time.Second):
		t.Fatal("device reset did not run after the superseded management request completed")
	}
}

func TestHostDeviceBarrierJoinsQueuedSupersededManagementRequest(t *testing.T) {
	driver := &managementBarrierDriver{
		fakeHostDriver: newFakeHostDriver(),
		management:     make(chan Completion, 1),
		release:        make(chan struct{}),
	}
	processor := &supersededManagementProcessor{
		endpointStarted:  make(chan struct{}),
		endpointCanceled: make(chan struct{}),
		endpointRelease:  make(chan struct{}),
		barrierStarted:   make(chan struct{}),
	}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := host.Register(context.Background(), 108, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		host.cancelAllOperations()
		_ = host.Unregister(context.Background(), identity)
	})

	// A multi-worker dequeue may announce the later device reset before the
	// earlier endpoint reset reaches its endpoint lane.
	if err = host.dispatch(context.Background(), Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0, EndpointSequence: 1, DeviceSequence: 2,
		Kind: OperationDeviceReset,
	}); err != nil {
		t.Fatal(err)
	}
	if err = host.dispatch(context.Background(), Operation{
		Token: 1, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x02, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationEndpointReset,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case completion := <-driver.management:
		if completion.Token != 1 || completion.Status != statusUnsuccessful {
			t.Fatalf("queued superseded management completion=%+v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("queued superseded endpoint reset left its management token stranded")
	}
	select {
	case <-processor.endpointStarted:
		t.Fatal("queued pre-barrier endpoint reset reached the processor")
	default:
	}
	select {
	case <-processor.barrierStarted:
		t.Fatal("device reset ran before queued management cancellation joined")
	case <-time.After(25 * time.Millisecond):
	}

	close(driver.release)
	select {
	case <-processor.barrierStarted:
	case <-time.After(time.Second):
		t.Fatal("device reset did not run after queued management cancellation joined")
	}
}

func TestHostWithdrawsAnnouncedBarrierWhenLaneAdmissionFails(t *testing.T) {
	driver := newFakeHostDriver()
	host, err := NewHost(driver, &noopProcessor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := host.Register(context.Background(), 107, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}

	host.mu.RLock()
	entry := host.devices[identity.DeviceID]
	host.mu.RUnlock()
	laneCtx, cancelLane := context.WithCancel(entry.ctx)
	key := laneKey{deviceID: identity.DeviceID, generation: identity.Generation, endpoint: 0}
	lane := &operationLane{
		key: key, ctx: laneCtx, cancel: cancelLane,
		input: make(chan Operation, 1), done: make(chan struct{}),
		terminalErr: errors.New("injected terminal lane"),
	}
	host.mu.Lock()
	host.lanes[key] = lane
	host.mu.Unlock()

	err = host.dispatch(context.Background(), Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0, EndpointSequence: 1, DeviceSequence: 1,
		Kind: OperationDeviceReset,
	})
	if err == nil || !strings.Contains(err.Error(), "injected terminal lane") {
		t.Fatalf("barrier admission error=%v want injected terminal lane", err)
	}
	entry.sequence.mu.Lock()
	pendingBarriers := len(entry.sequence.pendingBarriers)
	entry.sequence.mu.Unlock()
	if pendingBarriers != 0 {
		t.Fatalf("failed barrier admission retained %d pending barriers", pendingBarriers)
	}

	host.mu.Lock()
	if host.lanes[key] == lane {
		delete(host.lanes, key)
	}
	host.mu.Unlock()
	cancelLane()
	if err = host.Unregister(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
}

func TestHostSaturatedLaneDoesNotBlockIndependentController(t *testing.T) {
	if laneQueueDepth != defaultDevicePendingOperations {
		t.Fatalf("lane queue depth=%d want kernel pending contract=%d",
			laneQueueDepth, defaultDevicePendingOperations)
	}
	driver := newFakeHostDriver()
	processor := &deviceGateProcessor{
		blockedDevice: 91,
		started:       make(chan struct{}),
		independent:   make(chan uint64, 1),
	}
	host, err := NewHost(driver, processor, 1)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := host.Register(context.Background(), processor.blockedDevice, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	independent, err := host.Register(context.Background(), 92, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		host.cancelAllOperations()
		_ = host.Unregister(context.Background(), blocked)
		_ = host.Unregister(context.Background(), independent)
	})

	first := Operation{
		Token: 1, DeviceID: blocked.DeviceID, Generation: blocked.Generation,
		EndpointAddress: 0x02, EndpointSequence: 1, Kind: OperationTransfer,
	}
	if err = trackAndDispatch(host, first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("blocked lane did not start processing")
	}

	for sequence := uint64(2); sequence <= uint64(laneQueueDepth)+1; sequence++ {
		op := Operation{
			Token: sequence, DeviceID: blocked.DeviceID, Generation: blocked.Generation,
			EndpointAddress: 0x02, EndpointSequence: sequence, Kind: OperationTransfer,
		}
		if err = trackAndDispatch(host, op); err != nil {
			t.Fatalf("fill blocked lane at sequence %d: %v", sequence, err)
		}
	}
	overflow := Operation{
		Token:    uint64(laneQueueDepth) + 2,
		DeviceID: blocked.DeviceID, Generation: blocked.Generation,
		EndpointAddress: 0x02, EndpointSequence: uint64(laneQueueDepth) + 2,
		Kind: OperationTransfer,
	}
	if err = trackAndDispatch(host, overflow); err == nil || !strings.Contains(err.Error(), "lane is saturated") {
		t.Fatalf("overflow dispatch error=%v, want terminal saturation", err)
	}

	dispatched := make(chan error, 1)
	go func() {
		dispatched <- trackAndDispatch(host, Operation{
			Token: 10000, DeviceID: independent.DeviceID, Generation: independent.Generation,
			EndpointAddress: 0x02, EndpointSequence: 1, Kind: OperationTransfer,
		})
	}()
	select {
	case err = <-dispatched:
		if err != nil {
			t.Fatalf("independent dispatch failed: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("saturated controller blocked the central dispatcher")
	}
	select {
	case deviceID := <-processor.independent:
		if deviceID != independent.DeviceID {
			t.Fatalf("processed independent device=%d want=%d", deviceID, independent.DeviceID)
		}
	case <-time.After(time.Second):
		t.Fatal("independent controller was not processed")
	}
}

func TestHostFatalLaneWithQueuedWorkStaysTerminal(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &fatalLaneProcessor{
		failingDevice:   93,
		started:         make(chan struct{}),
		release:         make(chan struct{}),
		independent:     make(chan uint64, 1),
		queuedProcessed: make(chan struct{}, 1),
	}
	host, err := NewHost(driver, processor, 1)
	if err != nil {
		t.Fatal(err)
	}
	failing, err := host.Register(context.Background(), processor.failingDevice, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	independent, err := host.Register(context.Background(), 94, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = host.Unregister(context.Background(), failing)
		_ = host.Unregister(context.Background(), independent)
	})

	first := Operation{
		DeviceID: failing.DeviceID, Generation: failing.Generation,
		EndpointAddress: 0x02, EndpointSequence: 1, Kind: OperationSetInterface,
	}
	if err = host.dispatch(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("failing lane did not enter its lifecycle processor")
	}
	key := laneKey{
		deviceID: failing.DeviceID, generation: failing.Generation, endpoint: first.EndpointAddress,
	}
	host.mu.RLock()
	failedLane := host.lanes[key]
	host.mu.RUnlock()
	if failedLane == nil {
		t.Fatal("failing lane was not installed")
	}
	if err = host.dispatch(context.Background(), Operation{
		DeviceID: failing.DeviceID, Generation: failing.Generation,
		EndpointAddress: 0x02, EndpointSequence: 2, Kind: OperationSetInterface,
	}); err != nil {
		t.Fatalf("queue work behind failing operation: %v", err)
	}
	close(processor.release)
	select {
	case <-failedLane.done:
	case <-time.After(time.Second):
		t.Fatal("fatal lane did not cancel and stop")
	}
	select {
	case <-processor.queuedProcessed:
		t.Fatal("work queued behind a fatal operation was processed")
	default:
	}

	host.mu.RLock()
	routedLane := host.lanes[key]
	terminalErr := host.failedLanes[key]
	host.mu.RUnlock()
	if routedLane != nil {
		t.Fatal("fatal lane remained in the routing map")
	}
	if terminalErr == nil || !strings.Contains(terminalErr.Error(), "injected lane failure") {
		t.Fatalf("terminal lane error=%v, want injected failure", terminalErr)
	}
	if err = host.dispatch(context.Background(), Operation{
		DeviceID: failing.DeviceID, Generation: failing.Generation,
		EndpointAddress: 0x02, EndpointSequence: 3, Kind: OperationSetInterface,
	}); err == nil || !strings.Contains(err.Error(), "injected lane failure") {
		t.Fatalf("dispatch to terminal lane error=%v, want original failure", err)
	}
	host.mu.RLock()
	recreated := host.lanes[key]
	host.mu.RUnlock()
	if recreated != nil {
		t.Fatal("dispatch recreated a terminal lane")
	}

	if err = host.dispatch(context.Background(), Operation{
		DeviceID: independent.DeviceID, Generation: independent.Generation,
		EndpointAddress: 0x02, EndpointSequence: 1, Kind: OperationSetInterface,
	}); err != nil {
		t.Fatalf("independent lifecycle dispatch failed: %v", err)
	}
	select {
	case deviceID := <-processor.independent:
		if deviceID != independent.DeviceID {
			t.Fatalf("processed independent device=%d want=%d", deviceID, independent.DeviceID)
		}
	case <-time.After(time.Second):
		t.Fatal("independent lane did not run after another lane failed")
	}
}

func TestHostServeReturnsPromptlyOnLaneSaturation(t *testing.T) {
	baseDriver := newFakeHostDriver()
	baseDriver.operations = make(chan Operation, laneQueueDepth+4)
	driver := &cancellationOnlyCompletionDriver{fakeHostDriver: baseDriver}
	processor := &deviceGateProcessor{
		blockedDevice: 95,
		started:       make(chan struct{}),
		independent:   make(chan uint64, 1),
	}
	host, err := NewHost(driver, processor, 1)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := host.Register(context.Background(), processor.blockedDevice, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Serve(context.Background()) }()

	baseDriver.operations <- Operation{
		Token: 1, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x02, EndpointSequence: 1, Kind: OperationTransfer,
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("saturation test lane did not start processing")
	}
	for sequence := uint64(2); sequence <= uint64(laneQueueDepth)+2; sequence++ {
		baseDriver.operations <- Operation{
			Token: sequence, DeviceID: identity.DeviceID, Generation: identity.Generation,
			EndpointAddress: 0x02, EndpointSequence: sequence, Kind: OperationTransfer,
		}
	}

	select {
	case err = <-done:
		if err == nil || !strings.Contains(err.Error(), "lane is saturated") {
			t.Fatalf("Serve error=%v, want lane saturation failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve waited on failure completion instead of promptly observing lane fatal")
	}
}

func TestHostPreservesEndpointSequenceAcrossDequeueWorkers(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &recordingProcessor{processed: make(chan uint64, 2), resets: make(chan DeviceIdentity, 1)}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := host.Register(context.Background(), 9, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()

	driver.operations <- Operation{
		Token: 2, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 2, TransferLength: 8,
	}
	driver.operations <- Operation{
		Token: 1, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, TransferLength: 8,
	}

	for want := uint64(1); want <= 2; want++ {
		select {
		case got := <-processor.processed:
			if got != want {
				t.Fatalf("processed endpoint sequence=%d want=%d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for endpoint sequence %d", want)
		}
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop after context cancellation")
	}
}

func TestHostSessionCannotRestartAfterOperationsWereDequeued(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &recordingProcessor{processed: make(chan uint64, 1), resets: make(chan DeviceIdentity, 1)}
	host, err := NewHost(driver, processor, 1)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := host.Register(context.Background(), 19, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	driver.operations <- Operation{
		Token: 1, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationTransfer,
	}
	select {
	case <-processor.processed:
	case <-time.After(time.Second):
		t.Fatal("first host session did not process its operation")
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first host session did not stop")
	}

	if err = host.Serve(context.Background()); err == nil || !strings.Contains(err.Error(), "one-shot") {
		t.Fatalf("second Serve error=%v, want one-shot session rejection", err)
	}
	if _, err = host.Register(context.Background(), 20, hostTestDevice()); err == nil ||
		!strings.Contains(err.Error(), "fresh driver session") {
		t.Fatalf("Register after Serve error=%v, want terminal session rejection", err)
	}
}

func TestHostOrdersLifecycleBeforeFollowingTransfer(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 1),
		resets: make(chan DeviceIdentity, 1),
	}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := host.Register(context.Background(), 10, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()

	driver.operations <- Operation{
		Token: 1, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 2, Kind: OperationTransfer,
	}
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationEndpointPurge,
	}

	select {
	case got := <-processor.lifecycle:
		if got != 1 {
			t.Fatalf("lifecycle endpoint sequence=%d want=1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lifecycle operation")
	}
	select {
	case got := <-processor.processed:
		if got != 2 {
			t.Fatalf("transfer endpoint sequence=%d want=2", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for transfer after lifecycle")
	}
	cancel()
	<-done
}

func TestHostAcknowledgesLifecycleOnlyAfterProcessorAppliesIt(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 1),
		resets: make(chan DeviceIdentity, 1),
	}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := host.Register(context.Background(), 73, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()

	const token = uint64(0x0000000180000001)
	driver.operations <- Operation{
		Token: token, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationEndpointReset,
	}
	select {
	case sequence := <-processor.lifecycle:
		if sequence != 1 {
			t.Fatalf("lifecycle endpoint sequence=%d want=1", sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for acknowledged lifecycle operation")
	}
	select {
	case completion := <-driver.completions:
		if completion.Token != token || completion.DeviceID != identity.DeviceID ||
			completion.Generation != identity.Generation || completion.Status != 0 ||
			completion.TransferLength != 0 || len(completion.Payload) != 0 ||
			len(completion.IsoPackets) != 0 {
			t.Fatalf("lifecycle acknowledgement=%+v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lifecycle acknowledgement")
	}
	cancel()
	<-done
}

func TestHostDoesNotCompleteAdvisoryLifecycleNotification(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 1),
		resets: make(chan DeviceIdentity, 1),
	}
	host, _ := NewHost(driver, processor, 1)
	identity, err := host.Register(context.Background(), 74, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationEndpointPurge,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for advisory lifecycle operation")
	}
	select {
	case completion := <-driver.completions:
		t.Fatalf("advisory lifecycle notification was completed: %+v", completion)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	<-done
}

func TestHostRegisterFailureRollsBackButAdvancesGeneration(t *testing.T) {
	driver := newFakeHostDriver()
	driver.createErr = errors.New("plug failed")
	processor := &recordingProcessor{processed: make(chan uint64, 1), resets: make(chan DeviceIdentity, 1)}
	host, _ := NewHost(driver, processor, 1)
	if _, err := host.Register(context.Background(), 4, hostTestDevice()); err == nil {
		t.Fatal("register unexpectedly succeeded")
	}
	driver.createErr = nil
	identity, err := host.Register(context.Background(), 4, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	if identity.Generation != 2 {
		t.Fatalf("generation=%d want=2 after failed creation", identity.Generation)
	}
	if err := host.Unregister(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-processor.resets:
		if got != identity {
			t.Fatalf("reset identity=%+v want=%+v", got, identity)
		}
	case <-time.After(time.Second):
		t.Fatal("processor was not reset during unregister")
	}
}

func TestHostUnregisterFailureKeepsDeviceRetryable(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &recordingProcessor{processed: make(chan uint64, 1), resets: make(chan DeviceIdentity, 1)}
	host, _ := NewHost(driver, processor, 1)
	identity, err := host.Register(context.Background(), 11, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	driver.destroyErr = errors.New("plug-out failed")
	if err := host.Unregister(context.Background(), identity); err == nil {
		t.Fatal("unregister unexpectedly succeeded")
	}
	host.mu.RLock()
	entry := host.devices[identity.DeviceID]
	host.mu.RUnlock()
	if entry == nil || entry.identity != identity {
		t.Fatal("failed unregister discarded the live device generation")
	}
	driver.destroyErr = nil
	if err := host.Unregister(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
}

func TestHostRejectsStaleOperationGeneration(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &recordingProcessor{processed: make(chan uint64, 1), resets: make(chan DeviceIdentity, 1)}
	host, _ := NewHost(driver, processor, 1)
	identity, err := host.Register(context.Background(), 5, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	err = host.dispatch(context.Background(), Operation{
		Token: 3, DeviceID: identity.DeviceID, Generation: identity.Generation + 1,
		EndpointAddress: 0x81, EndpointSequence: 1,
	})
	if err == nil {
		t.Fatal("stale generation was accepted")
	}
}

func TestHostCompletesTypedUSBDFailureWithoutCollapsingToNTStatus(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &usbdFailureProcessor{err: testUSBDCompletionError{
		status: USBDStatusBadStartFrame,
	}}
	host, err := NewHost(driver, processor, 1)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := host.Register(context.Background(), 109, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()

	driver.operations <- Operation{
		Token: 1, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x02, EndpointSequence: 1, Kind: OperationTransfer,
		IsoPackets: []IsoPacket{{Offset: 0, Length: 64}, {Offset: 64, Length: 64}},
	}
	select {
	case completion := <-driver.completions:
		if completion.Status != 0 || completion.USBDStatus != USBDStatusBadStartFrame {
			t.Fatalf("typed USBD completion=%+v want NT success and BAD_START_FRAME", completion)
		}
		if len(completion.IsoPackets) != 2 ||
			completion.IsoPackets[0].Offset != 0 ||
			completion.IsoPackets[1].Offset != 64 ||
			completion.IsoPackets[0].Length != 0 ||
			uint32(completion.IsoPackets[0].Status) != USBDStatusBadStartFrame {
			t.Fatalf("typed USBD ISO packet table=%+v", completion.IsoPackets)
		}
	case <-time.After(time.Second):
		t.Fatal("typed USBD failure was not completed")
	}

	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("host returned error after clean cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop after cancellation")
	}
}

func TestHostCancelBeforeOperationSkipsProcessingAndCompletion(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &recordingProcessor{processed: make(chan uint64, 1), resets: make(chan DeviceIdentity, 1)}
	host, _ := NewHost(driver, processor, 1)
	identity, err := host.Register(context.Background(), 6, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()

	driver.operations <- Operation{
		Token: 44, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, Kind: OperationCancel,
	}
	driver.operations <- Operation{
		Token: 44, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationTransfer,
	}

	deadline := time.Now().Add(time.Second)
	for {
		host.operationMu.Lock()
		state := host.operations[44]
		finished := state != nil && state.done
		host.operationMu.Unlock()
		if finished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled operation was not retired")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case sequence := <-processor.processed:
		t.Fatalf("cancelled operation reached processor with sequence %d", sequence)
	default:
	}
	select {
	case completion := <-driver.completions:
		t.Fatalf("cancelled operation was completed twice: %+v", completion)
	default:
	}
	cancel()
	<-done
}

func TestHostCancelInterruptsActiveProcessor(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &cancellableProcessor{started: make(chan struct{}), cancelled: make(chan struct{})}
	host, _ := NewHost(driver, processor, 2)
	identity, err := host.Register(context.Background(), 7, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	driver.operations <- Operation{
		Token: 55, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationTransfer,
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not start")
	}
	driver.operations <- Operation{
		Token: 55, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, Kind: OperationCancel,
	}
	select {
	case <-processor.cancelled:
	case <-time.After(time.Second):
		t.Fatal("processor context was not cancelled")
	}
	select {
	case completion := <-driver.completions:
		t.Fatalf("cancelled operation was completed twice: %+v", completion)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	<-done
}

func TestHostUnregisterCancelsAndJoinsLanesBeforeReset(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &unregisterProcessor{
		started: make(chan struct{}), cancelled: make(chan struct{}), reset: make(chan bool, 1),
	}
	host, _ := NewHost(driver, processor, 2)
	identity, err := host.Register(context.Background(), 16, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServe := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(serveCtx) }()
	driver.operations <- Operation{
		Token: 1, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationTransfer,
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not start")
	}
	unregisterCtx, cancelUnregister := context.WithTimeout(context.Background(), time.Second)
	defer cancelUnregister()
	if err := host.Unregister(unregisterCtx, identity); err != nil {
		t.Fatal(err)
	}
	select {
	case cancelledFirst := <-processor.reset:
		if !cancelledFirst {
			t.Fatal("device reset raced ahead of its active endpoint lane")
		}
	case <-time.After(time.Second):
		t.Fatal("device was not reset after unregister")
	}
	select {
	case completion := <-driver.completions:
		t.Fatalf("unregister completed an operation after cancellation: %+v", completion)
	default:
	}
	stopServe()
	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("ordinary unregister failed the host session: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not stop")
	}
}

func TestHostUnregisterTimeoutKeepsStoppingTombstone(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &stubbornProcessor{started: make(chan struct{}), release: make(chan struct{})}
	host, _ := NewHost(driver, processor, 1)
	identity, err := host.Register(context.Background(), 17, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Serve(context.Background()) }()
	driver.operations <- Operation{
		Token: 1, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationTransfer,
	}
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not start")
	}
	unregisterCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = host.Unregister(unregisterCtx, identity)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Unregister error=%v want deadline", err)
	}
	if _, err = host.Register(context.Background(), identity.DeviceID, hostTestDevice()); err == nil {
		t.Fatal("stopping device ID was reused after irreversible plug-out")
	}
	close(processor.release)
	select {
	case err = <-done:
		if err == nil || !strings.Contains(err.Error(), "stop native UDE device") {
			t.Fatalf("Serve error=%v want teardown-timeout session failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("teardown timeout did not fail the host session")
	}
}

func TestHostDuplicateTokenFailsSessionWithoutCompletingWrongOperation(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &recordingProcessor{processed: make(chan uint64, 1), resets: make(chan DeviceIdentity, 1)}
	host, _ := NewHost(driver, processor, 1)
	identity, err := host.Register(context.Background(), 12, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Serve(context.Background()) }()

	// Sequence 1 is deliberately absent, so the first token remains a valid,
	// pending kernel request when the corrupt duplicate arrives.
	for _, endpoint := range []uint8{0x81, 0x82} {
		driver.operations <- Operation{
			Token: 77, DeviceID: identity.DeviceID, Generation: identity.Generation,
			EndpointAddress: endpoint, EndpointSequence: 2, Kind: OperationTransfer,
		}
	}

	select {
	case err = <-done:
		if err == nil || !strings.Contains(err.Error(), "reuses a completed or mismatched token") {
			t.Fatalf("Serve error=%v, want duplicate-token session failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate operation token did not fail the host session")
	}
	select {
	case completion := <-driver.completions:
		t.Fatalf("duplicate token completed an ambiguous kernel request: %+v", completion)
	default:
	}
}

func TestHostDuplicateEndpointSequenceFailsSession(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &recordingProcessor{processed: make(chan uint64, 1), resets: make(chan DeviceIdentity, 1)}
	host, _ := NewHost(driver, processor, 1)
	identity, err := host.Register(context.Background(), 13, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Serve(context.Background()) }()

	for token := uint64(1); token <= 2; token++ {
		driver.operations <- Operation{
			Token: token, DeviceID: identity.DeviceID, Generation: identity.Generation,
			EndpointAddress: 0x81, EndpointSequence: 2, Kind: OperationTransfer,
		}
	}
	select {
	case err = <-done:
		if err == nil || !strings.Contains(err.Error(), "repeated pending sequence 2") {
			t.Fatalf("Serve error=%v, want duplicate-sequence session failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate endpoint sequence did not fail the host session")
	}
}

func TestHostLifecycleFailureFailsSession(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 1),
		resets: make(chan DeviceIdentity, 1), lifecycleErr: errors.New("reset rejected"),
	}
	host, _ := NewHost(driver, processor, 1)
	identity, err := host.Register(context.Background(), 14, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Serve(context.Background()) }()
	driver.operations <- Operation{
		Token: 0x0000000180000001, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationEndpointReset,
	}
	select {
	case completion := <-driver.completions:
		if completion.Status != statusUnsuccessful {
			t.Fatalf("failed lifecycle completion status=%d want=%d",
				completion.Status, statusUnsuccessful)
		}
	case <-time.After(time.Second):
		t.Fatal("failed lifecycle was not acknowledged to the driver")
	}
	select {
	case err = <-done:
		if err == nil || !strings.Contains(err.Error(), "reset rejected") {
			t.Fatalf("Serve error=%v, want lifecycle session failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle failure did not fail the host session")
	}
}

func TestHostCompletionFailureFailsSession(t *testing.T) {
	driver := newFakeHostDriver()
	driver.completeErr = errors.New("completion handle lost")
	processor := &recordingProcessor{processed: make(chan uint64, 1), resets: make(chan DeviceIdentity, 1)}
	host, _ := NewHost(driver, processor, 1)
	identity, err := host.Register(context.Background(), 15, hostTestDevice())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Serve(context.Background()) }()
	driver.operations <- Operation{
		Token: 1, DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationTransfer,
	}
	select {
	case <-processor.processed:
	case <-time.After(time.Second):
		t.Fatal("processor did not receive transfer")
	}
	select {
	case err = <-done:
		if err == nil || !strings.Contains(err.Error(), "completion handle lost") {
			t.Fatalf("Serve error=%v, want completion session failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("completion failure did not fail the host session")
	}
}

func TestHostDirectInputFailureFailsSession(t *testing.T) {
	driver := &fastInputDriver{
		fakeHostDriver: newFakeHostDriver(),
		reports:        make(chan InputReport, 1),
		submitErr:      errors.New("direct input handle lost"),
	}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 1),
		resets: make(chan DeviceIdentity, 1),
	}
	host, err := NewHost(driver, processor, 2)
	if err != nil {
		t.Fatal(err)
	}
	device := newInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 16, device)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Serve(context.Background()) }()
	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationEndpointStart,
	}
	select {
	case <-processor.lifecycle:
	case <-time.After(time.Second):
		t.Fatal("endpoint start was not processed")
	}
	device.reports <- []byte{1, 2, 3, 4}
	select {
	case err = <-done:
		if err == nil || !strings.Contains(err.Error(), "direct input handle lost") {
			t.Fatalf("Serve error=%v, want direct-input session failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("direct input submission failure did not fail the host session")
	}
}

func TestHostBrokerFaultFailsSessionWithoutDispatchingAnOperation(t *testing.T) {
	driver := newFakeHostDriver()
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 1),
		resets: make(chan DeviceIdentity, 1),
	}
	host, _ := NewHost(driver, processor, 1)
	done := make(chan error, 1)
	go func() { done <- host.Serve(context.Background()) }()
	driver.operations <- Operation{Kind: OperationBrokerFault}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "lost lifecycle notification") {
			t.Fatalf("Serve error=%v, want broker-fault session failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("kernel broker fault did not fail the host session")
	}
	select {
	case sequence := <-processor.processed:
		t.Fatalf("broker fault reached transfer processor with sequence %d", sequence)
	case sequence := <-processor.lifecycle:
		t.Fatalf("broker fault reached lifecycle processor with sequence %d", sequence)
	default:
	}
}

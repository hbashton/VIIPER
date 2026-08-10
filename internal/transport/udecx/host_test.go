package udecx

import (
	"context"
	"errors"
	"strings"
	"sync"
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
	reports chan InputReport
}

func (d *fastInputDriver) SubmitInputReport(ctx context.Context, report InputReport) error {
	report.Payload = append([]byte(nil), report.Payload...)
	select {
	case d.reports <- report:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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

type inputPublisherTestDevice struct {
	descriptor usb.Descriptor
	reports    chan []byte
}

func newInputPublisherTestDevice() *inputPublisherTestDevice {
	base := hostTestDevice().GetDescriptor()
	return &inputPublisherTestDevice{descriptor: *base, reports: make(chan []byte, 4)}
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
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationEndpointStart,
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
	device := newInputPublisherTestDevice()
	identity, err := host.Register(context.Background(), 46, device)
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

	driver.operations <- Operation{
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0, EndpointSequence: 1, Kind: OperationDeviceD0Exit,
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
		EndpointAddress: 0, EndpointSequence: 2, Kind: OperationDeviceD0Entry,
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

func TestHostRestartsInputPublisherAfterEndpointPurgeWithoutResettingSequence(t *testing.T) {
	driver := &fastInputDriver{fakeHostDriver: newFakeHostDriver(), reports: make(chan InputReport, 4)}
	processor := &recordingProcessor{
		processed: make(chan uint64, 1), lifecycle: make(chan uint64, 3),
		resets: make(chan DeviceIdentity, 1),
	}
	host, _ := NewHost(driver, processor, 2)
	device := newInputPublisherTestDevice()
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
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: 0x81, EndpointSequence: 1, Kind: OperationEndpointReset,
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

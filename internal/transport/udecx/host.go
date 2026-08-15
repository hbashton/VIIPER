package udecx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alia5/VIIPER/usb"
)

const (
	defaultDequeueWorkers = 8
	// A child cannot expose more operations than the pending-operation
	// contract published to the kernel. Matching that bound here lets a busy
	// endpoint absorb every operation the broker can legally own without ever
	// making the central dispatcher wait for one controller.
	laneQueueDepth         = defaultDevicePendingOperations
	completionTimeout      = 2 * time.Second
	terminalCleanupTimeout = 30 * time.Second
	completedTokenHistory  = MaxPendingOperations * 2
	statusUnsuccessful     = int32(-1073741823) // STATUS_UNSUCCESSFUL
)

var errInputSequenceExhausted = errors.New("native UDE input report sequence is exhausted")

// Driver is the narrow host-side contract implemented by the overlapped
// Windows UdeCx client. Keeping it as an interface makes ordering, teardown,
// and stale-generation behavior testable without loading a kernel driver.
type Driver interface {
	CreateDevice(context.Context, CreateDevice) (DeviceRegistration, error)
	// DestroyDevice returns an error only if removal was rejected before the
	// kernel transferred ownership to UdeCx. Once accepted, any terminal
	// UdeCx removal fault is recovered by restarting the controller and the
	// call succeeds so callers never resurrect an invalid device generation.
	DestroyDevice(context.Context, DeviceIdentity) error
	Dequeue(context.Context, []byte) (Operation, error)
	Complete(context.Context, Completion) error
	QueryStats(context.Context) (Stats, error)
}

type LifecycleTraceDriver interface {
	QueryLifecycleTrace(context.Context) (LifecycleTrace, error)
}

// InputReportDriver is an optional, version-negotiated extension used only
// for interrupt-IN reports. Keeping it separate preserves the ordered broker
// contract for control, output, feedback, audio, and lifecycle traffic.
type InputReportDriver interface {
	// SubmitInputReport must honor ctx. Endpoint lifecycle cancellation stops
	// sampling but deliberately lets an already encoded report commit; ctx is
	// cancelled when the owning Host session stops or fails.
	SubmitInputReport(context.Context, InputReport) error
}

// OperationProcessor translates one native USB operation through VIIPER's
// existing usb.Device engines. Implementations must not retain operation
// payload slices after Process returns.
type OperationProcessor interface {
	Process(context.Context, usb.Device, Operation) (Completion, error)
	Lifecycle(context.Context, usb.Device, Operation) error
	Reset(usb.Device, DeviceIdentity)
}

type registeredDevice struct {
	identity           DeviceIdentity
	device             usb.Device
	sequence           *deviceSequenceBarrier
	ctx                context.Context
	cancel             context.CancelFunc
	stopping           bool
	publisherStopping  bool
	fastInput          map[uint8]fastInputEndpoint
	publishers         map[uint8]*inputPublisher
	activeInput        map[uint8]uint32
	resettingInput     map[uint8]uint32
	endpointGeneration map[uint8]uint32
	inputSequences     map[endpointIdentity]*atomic.Uint64
	inD0               bool
	resetting          bool
	powerSequence      uint64
}

type inputPublisher struct {
	endpoint           uint8
	endpointGeneration uint32
	reportSize         int
	interval           time.Duration
	sequence           *atomic.Uint64
	submitCtx          context.Context
	cancel             context.CancelFunc
	done               chan struct{}
}

type endpointIdentity struct {
	address    uint8
	generation uint32
}

type fastInputEndpoint struct {
	reportSize int
	interval   time.Duration
}

type laneKey struct {
	deviceID           uint64
	generation         uint32
	endpoint           uint8
	endpointGeneration uint32
}

type operationLane struct {
	key         laneKey
	ctx         context.Context
	cancel      context.CancelFunc
	input       chan Operation
	done        chan struct{}
	stateMu     sync.Mutex
	terminalErr error
}

type operationState struct {
	deviceID           uint64
	generation         uint32
	endpoint           uint8
	endpointGeneration uint32
	cancel             context.CancelFunc
	cancelled          bool
	received           bool
	processing         bool
	done               bool
}

type deviceLifecycleGate struct {
	mu         sync.Mutex
	references int
}

// InputPathDiagnostics makes every slower compatibility path observable.
// These counters are publisher-lifetime events rather than per-report events,
// so collecting them adds no atomic operation to the interrupt-input hot path.
type InputPathDiagnostics struct {
	PublisherStarts               uint64
	LegacyTransferFallbackStarts  uint64
	DeadlineContextFallbackStarts uint64
}

// Host owns one exclusive driver session and routes operations concurrently
// across endpoints while preserving strict FIFO within each endpoint.
type Host struct {
	driver    Driver
	input     InputReportDriver
	processor OperationProcessor
	workers   int

	lifecycleMu          sync.Mutex
	lifecycles           map[uint64]*deviceLifecycleGate
	mu                   sync.RWMutex
	devices              map[uint64]*registeredDevice
	generations          map[uint64]uint32
	controllerSessionID  uint64
	controllerInstanceID string
	lanes                map[laneKey]*operationLane
	failedLanes          map[laneKey]error
	runCtx               context.Context
	runCancel            context.CancelFunc
	fatal                chan error
	started              bool
	running              bool
	laneWG               sync.WaitGroup
	operationMu          sync.Mutex
	operations           map[uint64]*operationState
	completed            []uint64

	inputPublisherStarts          atomic.Uint64
	legacyTransferFallbackStarts  atomic.Uint64
	deadlineContextFallbackStarts atomic.Uint64

	// inputAttemptContext is a deterministic deadline seam for host tests.
	// Production hosts leave it nil and use context.WithTimeout.
	inputAttemptContext func(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

// InputDiagnostics returns a lock-free snapshot of input-publisher path
// selection. A nonzero fallback count is deliberately visible to release
// telemetry instead of silently trading latency for compatibility.
func (h *Host) InputDiagnostics() InputPathDiagnostics {
	if h == nil {
		return InputPathDiagnostics{}
	}
	return InputPathDiagnostics{
		PublisherStarts:               h.inputPublisherStarts.Load(),
		LegacyTransferFallbackStarts:  h.legacyTransferFallbackStarts.Load(),
		DeadlineContextFallbackStarts: h.deadlineContextFallbackStarts.Load(),
	}
}

func NewHost(driver Driver, processor OperationProcessor, workers int) (*Host, error) {
	if driver == nil || processor == nil {
		return nil, errors.New("native UDE host requires a driver and operation processor")
	}
	if workers <= 0 {
		workers = defaultDequeueWorkers
	}
	host := &Host{
		driver: driver, processor: processor, workers: workers,
		devices:     make(map[uint64]*registeredDevice),
		generations: make(map[uint64]uint32),
		lanes:       make(map[laneKey]*operationLane),
		failedLanes: make(map[laneKey]error),
		lifecycles:  make(map[uint64]*deviceLifecycleGate),
		operations:  make(map[uint64]*operationState),
	}
	host.input, _ = driver.(InputReportDriver)
	return host, nil
}

// lockDeviceLifecycle serializes create/remove for one stable device ID while
// allowing independent controllers to enumerate or tear down concurrently.
// References include both the holder and waiters, so a gate cannot be deleted
// and replaced while an older waiter still targets it.
func (h *Host) lockDeviceLifecycle(deviceID uint64) func() {
	h.lifecycleMu.Lock()
	gate := h.lifecycles[deviceID]
	if gate == nil {
		gate = &deviceLifecycleGate{}
		h.lifecycles[deviceID] = gate
	}
	gate.references++
	h.lifecycleMu.Unlock()

	gate.mu.Lock()
	return func() {
		gate.mu.Unlock()
		h.lifecycleMu.Lock()
		gate.references--
		if gate.references == 0 && h.lifecycles[deviceID] == gate {
			delete(h.lifecycles, deviceID)
		}
		h.lifecycleMu.Unlock()
	}
}

func interruptInputServiceInterval(speed uint32, bInterval uint8) time.Duration {
	// Match the proven USB/IP interrupt scheduler in
	// internal/server/usb.usbServiceInterval.
	if bInterval == 0 {
		return 0
	}
	if speed >= uint32(DeviceSpeedHigh) {
		// USB 2.x/3.x encode interrupt service periods as a power of two
		// microframes and reserve values above 16.
		if bInterval > 16 {
			return 0
		}
		return time.Duration(uint64(1)<<(bInterval-1)) * 125 * time.Microsecond
	}
	return time.Duration(bInterval) * time.Millisecond
}

func fastInputEndpoints(dev usb.Device) map[uint8]fastInputEndpoint {
	result := make(map[uint8]fastInputEndpoint)
	if dev == nil || dev.GetDescriptor() == nil {
		return result
	}
	selector, restrictEndpoints := dev.(usb.InterruptInputEndpointSelector)
	for _, iface := range dev.GetDescriptor().Interfaces {
		for _, endpoint := range iface.Endpoints {
			if endpoint.BEndpointAddress&0x80 != 0 && endpoint.BMAttributes&0x03 == 0x03 {
				if restrictEndpoints && !selector.SupportsInterruptInputEndpoint(
					uint32(endpoint.BEndpointAddress&0x0f)) {
					continue
				}
				// USB 2.0 wMaxPacketSize uses bits 0..10 for bytes and bits
				// 11..12 for additional high-bandwidth transactions. Allocate
				// the complete service opportunity while enforcing the native
				// ABI's hard report bound.
				packetBytes := int(endpoint.WMaxPacketSize & 0x07ff)
				transactions := 1 + int((endpoint.WMaxPacketSize>>11)&0x03)
				reportSize := packetBytes * transactions
				if reportSize > 0 && reportSize <= MaxInputReportBytes {
					result[endpoint.BEndpointAddress] = fastInputEndpoint{
						reportSize: reportSize,
						interval: interruptInputServiceInterval(
							dev.GetDescriptor().Device.Speed, endpoint.BInterval),
					}
				}
			}
		}
	}
	return result
}

// Register preserves the historical lifecycle API for callers that need only
// the exact device/generation identity.
func (h *Host) Register(ctx context.Context, deviceID uint64, dev usb.Device) (DeviceIdentity, error) {
	registration, err := h.RegisterWithCorrelation(ctx, deviceID, dev)
	return registration.DeviceIdentity, err
}

// RegisterWithCorrelation publishes a USB device using a fresh generation and
// returns the kernel-authored PnP correlation receipt. The routing entry is
// installed before the driver plugs in the child because Windows can submit
// its first descriptor request before CreateDevice returns.
func (h *Host) RegisterWithCorrelation(ctx context.Context, deviceID uint64, dev usb.Device) (DeviceRegistration, error) {
	if deviceID == 0 || dev == nil {
		return DeviceRegistration{}, ErrInvalidRange
	}
	unlockLifecycle := h.lockDeviceLifecycle(deviceID)
	defer unlockLifecycle()

	h.mu.Lock()
	// One driver file owner is one native UDE host session. Once Serve has
	// stopped, operations already dequeued into user mode cannot be replayed or
	// reconstructed safely. A fresh Client/Host pair is therefore required
	// instead of publishing a child into a terminal owner session.
	if h.started && (!h.running || h.runCtx == nil || h.runCtx.Err() != nil) {
		h.mu.Unlock()
		return DeviceRegistration{}, errors.New("native UDE host session has stopped; open a fresh driver session")
	}
	if _, exists := h.devices[deviceID]; exists {
		h.mu.Unlock()
		return DeviceRegistration{}, fmt.Errorf("native UDE device %d is already registered", deviceID)
	}
	if h.generations[deviceID] == math.MaxUint32 {
		h.mu.Unlock()
		return DeviceRegistration{}, fmt.Errorf(
			"native UDE device %d exhausted its generation space", deviceID)
	}
	generation := h.generations[deviceID] + 1
	identity := DeviceIdentity{DeviceID: deviceID, Generation: generation}
	deviceCtx, cancel := context.WithCancel(context.Background())
	entry := &registeredDevice{
		identity: identity, device: dev, sequence: newDeviceSequenceBarrier(),
		ctx: deviceCtx, cancel: cancel,
		fastInput: fastInputEndpoints(dev), publishers: make(map[uint8]*inputPublisher),
		activeInput: make(map[uint8]uint32), resettingInput: make(map[uint8]uint32),
		endpointGeneration: make(map[uint8]uint32),
		inputSequences:     make(map[endpointIdentity]*atomic.Uint64), inD0: true,
	}
	h.devices[deviceID] = entry
	h.generations[deviceID] = generation
	h.mu.Unlock()

	var registration DeviceRegistration
	driverCommitted := false
	snapshot, err := SnapshotDevice(deviceID, generation, dev)
	if err == nil {
		registration, err = h.driver.CreateDevice(ctx, snapshot)
		if err == nil {
			driverCommitted = true
			if !deviceRegistrationMatchesCreate(registration, snapshot) {
				err = errors.New("native UDE driver returned an invalid device-correlation receipt")
			} else {
				h.mu.Lock()
				if h.controllerSessionID == 0 {
					h.controllerSessionID = registration.ControllerSessionID
					h.controllerInstanceID = registration.ControllerInstanceID
				} else if h.controllerSessionID != registration.ControllerSessionID ||
					!strings.EqualFold(h.controllerInstanceID, registration.ControllerInstanceID) {
					err = errors.New("native UDE driver changed controller identity within one host session")
				}
				h.mu.Unlock()
			}
		}
	}
	if err != nil {
		if driverCommitted {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), terminalCleanupTimeout)
			cleanupErr := h.driver.DestroyDevice(cleanupCtx, identity)
			cleanupCancel()
			if cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf(
					"rollback native UDE device after invalid correlation receipt: %w", cleanupErr))
			}
		}
		h.mu.Lock()
		if h.devices[deviceID] == entry {
			delete(h.devices, deviceID)
		}
		h.mu.Unlock()
		cancel()
		return DeviceRegistration{}, err
	}

	// CreateDevice is an overlapped PnP transaction and can outlive a fatal or
	// cancelled one-shot Serve session. Revalidate after the kernel commits the
	// child. Reporting success here would publish a controller into a host that
	// can never service its USB requests. Roll the exact generation back while
	// the per-device lifecycle gate is still held.
	h.mu.RLock()
	terminal := h.started && (!h.running || h.runCtx == nil || h.runCtx.Err() != nil)
	h.mu.RUnlock()
	if terminal {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), terminalCleanupTimeout)
		cleanupErr := h.driver.DestroyDevice(cleanupCtx, identity)
		cleanupCancel()
		h.mu.Lock()
		if h.devices[deviceID] == entry {
			entry.stopping = true
			entry.publisherStopping = true
			if cleanupErr == nil {
				delete(h.devices, deviceID)
			}
		}
		h.mu.Unlock()
		cancel()
		if cleanupErr == nil {
			h.processor.Reset(dev, identity)
			return DeviceRegistration{}, errors.New(
				"native UDE host session stopped while controller registration was in flight")
		}
		return DeviceRegistration{}, errors.Join(
			errors.New("native UDE host session stopped while controller registration was in flight"),
			fmt.Errorf("rollback native UDE device %d generation %d: %w",
				identity.DeviceID, identity.Generation, cleanupErr))
	}
	return registration, nil
}

func deviceRegistrationMatchesCreate(registration DeviceRegistration, requested CreateDevice) bool {
	if registration.DeviceIdentity != (DeviceIdentity{
		DeviceID: requested.DeviceID, Generation: requested.Generation,
	}) || registration.Speed != requested.Speed || registration.ControllerSessionID == 0 ||
		!IsCanonicalControllerInstanceID(registration.ControllerInstanceID) {
		return false
	}
	if requested.Speed == DeviceSpeedSuper {
		return registration.USB20PortNumber == 0 &&
			registration.USB30PortNumber > MaxDevices &&
			registration.USB30PortNumber <= 2*MaxDevices
	}
	return registration.USB30PortNumber == 0 && registration.USB20PortNumber != 0 &&
		registration.USB20PortNumber <= MaxDevices
}

func (h *Host) Unregister(ctx context.Context, identity DeviceIdentity) error {
	if identity.DeviceID == 0 || identity.Generation == 0 {
		return ErrInvalidRange
	}
	unlockLifecycle := h.lockDeviceLifecycle(identity.DeviceID)
	defer unlockLifecycle()

	h.mu.RLock()
	entry := h.devices[identity.DeviceID]
	if entry == nil || entry.stopping || entry.identity.Generation != identity.Generation {
		h.mu.RUnlock()
		return fmt.Errorf("native UDE device %d generation %d is not registered",
			identity.DeviceID, identity.Generation)
	}
	h.mu.RUnlock()

	h.mu.Lock()
	entry.publisherStopping = true
	h.mu.Unlock()
	activePublishers := h.activeInputEndpoints(entry)
	h.stopAllInputPublishers(entry)

	// Keep routing live until the driver has transactionally accepted removal.
	// Errors occur before UdeCx consumes the device handle, so callers can
	// retry without losing the generation or its endpoint lanes.
	if err := h.driver.DestroyDevice(ctx, identity); err != nil {
		h.mu.Lock()
		entry.publisherStopping = false
		h.mu.Unlock()
		for _, endpoint := range activePublishers {
			h.startInputPublisher(entry, endpoint.address, endpoint.generation)
		}
		return err
	}
	go h.observeLifecycleRemoval(identity)

	h.mu.Lock()
	if h.devices[identity.DeviceID] != entry {
		h.mu.Unlock()
		return errors.New("native UDE device changed during serialized removal")
	}
	entry.stopping = true
	stoppingLanes := make([]*operationLane, 0, 4)
	for key, lane := range h.lanes {
		if key.deviceID == identity.DeviceID && key.generation == identity.Generation {
			stoppingLanes = append(stoppingLanes, lane)
			delete(h.lanes, key)
		}
	}
	for key := range h.failedLanes {
		if key.deviceID == identity.DeviceID && key.generation == identity.Generation {
			delete(h.failedLanes, key)
		}
	}
	h.mu.Unlock()

	// Mark operations cancelled before their processing contexts are stopped.
	// This prevents a processor waking on lane cancellation and racing a
	// completion through an intentionally cancelled driver handle.
	h.cancelDeviceOperations(identity)
	for _, lane := range stoppingLanes {
		stopLane(lane)
	}
	entry.cancel()
	for _, lane := range stoppingLanes {
		select {
		case <-lane.done:
		case <-ctx.Done():
			h.reportFatal(fmt.Errorf("stop native UDE device %d generation %d lanes: %w",
				identity.DeviceID, identity.Generation, ctx.Err()))
			return ctx.Err()
		}
	}
	h.mu.Lock()
	if h.devices[identity.DeviceID] == entry {
		delete(h.devices, identity.DeviceID)
	}
	h.mu.Unlock()
	h.processor.Reset(entry.device, identity)
	return nil
}

func (h *Host) observeLifecycleRemoval(identity DeviceIdentity) {
	driver, ok := h.driver.(LifecycleTraceDriver)
	if !ok {
		return
	}
	seen := make(map[uint64]struct{}, LifecycleTraceCapacity)
	statusReported := false
	for _, delay := range []time.Duration{0, 100 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second, 5 * time.Second} {
		if delay != 0 {
			timer := time.NewTimer(delay)
			<-timer.C
		}
		queryCtx, cancel := context.WithTimeout(context.Background(), completionTimeout)
		trace, err := driver.QueryLifecycleTrace(queryCtx)
		cancel()
		if err != nil {
			slog.Warn("native UDE lifecycle trace query failed",
				"device_id", identity.DeviceID, "generation", identity.Generation,
				"error", err)
			return
		}
		if trace.StatusFlags != 0 && !statusReported {
			statusReported = true
			slog.Error("native UDE lifecycle recorder reported sticky failure state",
				"device_id", identity.DeviceID, "generation", identity.Generation,
				"status_flags", fmt.Sprintf("%#08x", uint32(trace.StatusFlags)),
				"latest_sequence", trace.LatestSequence)
		}
		for _, record := range trace.Records {
			if record.DeviceID != identity.DeviceID || record.Generation != identity.Generation {
				continue
			}
			if _, duplicate := seen[record.PublishedSequence]; duplicate {
				continue
			}
			seen[record.PublishedSequence] = struct{}{}
			slog.Info("native UDE lifecycle",
				"sequence", record.PublishedSequence,
				"qpc", record.TimestampQPC,
				"qpc_frequency", trace.PerformanceFrequency,
				"source", lifecycleTraceSourceName(record.Source),
				"event", lifecycleTraceEventName(record.Event),
				"line", record.Line,
				"caller", fmt.Sprintf("%#x", record.Caller),
				"cpu", record.Processor,
				"irql", record.IRQL,
				"device_id", record.DeviceID,
				"generation", record.Generation,
				"device_object", fmt.Sprintf("%#x", record.DeviceObject),
				"endpoint_object", fmt.Sprintf("%#x", record.EndpointObject),
				"endpoint", fmt.Sprintf("%#02x", record.EndpointAddress),
				"status", fmt.Sprintf("%#08x", uint32(record.Status)),
				"active_operations", record.ActiveOperations,
				"pending_operations", record.PendingOperations,
				"queue_state", fmt.Sprintf("%#08x", record.QueueState))
		}
	}
}

func lifecycleTraceSourceName(source uint8) string {
	switch source {
	case TraceSourceDevice:
		return "Device.c"
	case TraceSourceBroker:
		return "Broker.c"
	case TraceSourceController:
		return "Controller.c"
	default:
		return fmt.Sprintf("source-%d", source)
	}
}

func lifecycleTraceEventName(event uint16) string {
	names := [...]string{
		"", "create-begin", "device-create-returned", "device-slot-claimed",
		"plug-in-begin", "plug-in-returned", "remove-claimed",
		"management-abort-begin", "management-abort-end", "plug-out-begin",
		"plug-out-returned", "endpoint-purge-begin", "endpoint-operations-purged",
		"endpoint-queue-purge-requested", "endpoint-driver-quiescent",
		"endpoint-drain-begin", "endpoint-drain-end",
		"endpoint-purge-complete-begin", "endpoint-purge-complete-end",
		"endpoint-cleanup-begin", "endpoint-cleanup-end", "device-cleanup-begin",
		"device-cleanup-end", "controller-shutdown-begin", "controller-shutdown-end",
		"endpoint-quiescence-watchdog",
		"completion-rundown-watchdog", "controller-rundown-watchdog",
		"owner-rundown-watchdog",
	}
	if int(event) < len(names) && names[event] != "" {
		return names[event]
	}
	return fmt.Sprintf("event-%d", event)
}

func (h *Host) startInputPublisher(
	entry *registeredDevice, endpoint uint8, endpointGeneration uint32,
) {
	if h.input == nil || endpointGeneration == 0 {
		return
	}
	h.mu.Lock()
	if !h.running || entry.stopping || entry.publisherStopping || !entry.inD0 || entry.resetting ||
		entry.activeInput[endpoint] != endpointGeneration ||
		entry.endpointGeneration[endpoint] != endpointGeneration ||
		entry.resettingInput[endpoint] == endpointGeneration ||
		h.devices[entry.identity.DeviceID] != entry {
		h.mu.Unlock()
		return
	}
	endpointContract, fast := entry.fastInput[endpoint]
	if !fast || entry.publishers[endpoint] != nil {
		h.mu.Unlock()
		return
	}
	identity := endpointIdentity{address: endpoint, generation: endpointGeneration}
	sequence := entry.inputSequences[identity]
	if sequence == nil {
		sequence = &atomic.Uint64{}
		entry.inputSequences[identity] = sequence
	}
	ctx, cancel := context.WithCancel(entry.ctx)
	publisher := &inputPublisher{
		endpoint: endpoint, endpointGeneration: endpointGeneration,
		reportSize: endpointContract.reportSize,
		interval:   endpointContract.interval, sequence: sequence,
		submitCtx: h.runCtx,
		cancel:    cancel, done: make(chan struct{}),
	}
	entry.publishers[endpoint] = publisher
	h.mu.Unlock()

	go h.runInputPublisher(ctx, entry, publisher)
}

func (h *Host) stopInputPublisher(
	entry *registeredDevice, endpoint uint8, endpointGeneration uint32,
) bool {
	h.mu.Lock()
	publisher := entry.publishers[endpoint]
	if publisher != nil && publisher.endpointGeneration == endpointGeneration {
		delete(entry.publishers, endpoint)
		publisher.cancel()
	} else {
		publisher = nil
	}
	h.mu.Unlock()
	if publisher == nil {
		return false
	}
	<-publisher.done
	return true
}

func (h *Host) stopAllInputPublishers(entry *registeredDevice) []endpointIdentity {
	h.mu.RLock()
	endpoints := make([]endpointIdentity, 0, len(entry.publishers))
	for endpoint, publisher := range entry.publishers {
		endpoints = append(endpoints, endpointIdentity{
			address: endpoint, generation: publisher.endpointGeneration,
		})
	}
	h.mu.RUnlock()
	for _, endpoint := range endpoints {
		h.stopInputPublisher(entry, endpoint.address, endpoint.generation)
	}
	return endpoints
}

func (h *Host) activeInputEndpoints(entry *registeredDevice) []endpointIdentity {
	h.mu.RLock()
	defer h.mu.RUnlock()
	endpoints := make([]endpointIdentity, 0, len(entry.activeInput))
	for endpoint, generation := range entry.activeInput {
		if generation != 0 {
			endpoints = append(endpoints, endpointIdentity{
				address: endpoint, generation: generation,
			})
		}
	}
	return endpoints
}

func (h *Host) withInputAttemptDeadline(
	ctx context.Context, interval time.Duration,
) (context.Context, context.CancelFunc) {
	if h.inputAttemptContext != nil {
		return h.inputAttemptContext(ctx, interval)
	}
	return context.WithTimeout(ctx, interval)
}

func stopInputDeadlineTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}
	// Go 1.23+ synchronous timer channels guarantee that Stop prevents a stale
	// receive. The nonblocking drain also preserves that invariant if a process
	// explicitly restores the legacy buffered timer implementation through
	// GODEBUG=asynctimerchan=1.
	select {
	case <-timer.C:
	default:
	}
}

func nextInputReportSequence(previous uint64) (uint64, error) {
	// InputReport's wire contract is signed-positive so the kernel can validate
	// it with MAXLONGLONG. Never wrap to one: reusing an accepted sequence would
	// violate the monotonic endpoint contract.
	if previous >= math.MaxInt64 {
		return 0, errInputSequenceExhausted
	}
	return previous + 1, nil
}

func (h *Host) runInputPublisher(ctx context.Context, entry *registeredDevice, publisher *inputPublisher) {
	defer close(publisher.done)
	reader, direct := entry.device.(usb.InterruptInputDevice)
	scheduledReader, scheduled := entry.device.(usb.ScheduledInterruptInputDevice)
	classifiedReader, classified := entry.device.(usb.ClassifiedScheduledInterruptInputDevice)
	h.inputPublisherStarts.Add(1)
	if !direct {
		h.legacyTransferFallbackStarts.Add(1)
		slog.Warn("native UDE interrupt input compatibility fallback activated",
			"device_id", entry.identity.DeviceID,
			"generation", entry.identity.Generation,
			"endpoint", fmt.Sprintf("%#02x", publisher.endpoint),
			"endpoint_generation", publisher.endpointGeneration,
			"fallback", "legacy-handle-transfer",
			"reason", "device does not implement InterruptInputDevice")
	} else if publisher.interval > 0 && !scheduled {
		h.deadlineContextFallbackStarts.Add(1)
		slog.Warn("native UDE interrupt input compatibility fallback activated",
			"device_id", entry.identity.DeviceID,
			"generation", entry.identity.Generation,
			"endpoint", fmt.Sprintf("%#02x", publisher.endpoint),
			"endpoint_generation", publisher.endpointGeneration,
			"fallback", "per-report-deadline-context",
			"reason", "device does not implement ScheduledInterruptInputDevice")
	}
	var reportBuffer []byte
	var deadlineTimer *time.Timer
	var retryTimer *time.Timer
	defer func() {
		if retryTimer != nil {
			stopInputDeadlineTimer(retryTimer)
		}
	}()
	if direct {
		reportBuffer = make([]byte, publisher.reportSize)
		if scheduled && publisher.interval > 0 {
			// One endpoint owns one timer for its complete lifetime. Resetting it
			// after each submitted sample preserves the established relative
			// service-deadline contract while removing a timer/context allocation
			// from every idle 1 ms controller report.
			deadlineTimer = time.NewTimer(time.Hour)
			stopInputDeadlineTimer(deadlineTimer)
			defer stopInputDeadlineTimer(deadlineTimer)
		}
	}
	for {
		var payload []byte
		transition := false
		if direct {
			var written int
			var err error
			if deadlineTimer != nil && classified {
				deadlineTimer.Reset(publisher.interval)
				written, transition, err = classifiedReader.ReadClassifiedScheduledInterruptInput(
					ctx, deadlineTimer.C, uint32(publisher.endpoint&0x0f), reportBuffer)
				stopInputDeadlineTimer(deadlineTimer)
			} else if deadlineTimer != nil {
				deadlineTimer.Reset(publisher.interval)
				written, err = scheduledReader.ReadScheduledInterruptInput(
					ctx, deadlineTimer.C, uint32(publisher.endpoint&0x0f), reportBuffer)
				stopInputDeadlineTimer(deadlineTimer)
			} else {
				attemptCtx := ctx
				attemptCancel := context.CancelFunc(func() {})
				if publisher.interval > 0 {
					attemptCtx, attemptCancel = h.withInputAttemptDeadline(ctx, publisher.interval)
				}
				written, err = reader.ReadInterruptInput(
					attemptCtx, uint32(publisher.endpoint&0x0f), reportBuffer)
				attemptCancel()
			}
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// Event-only devices may decline to synthesize an idle report.
				// Cached-state controller implementations return success on the
				// same deadline and are submitted below at the endpoint cadence.
				if errors.Is(err, context.DeadlineExceeded) {
					continue
				}
				h.reportFatal(fmt.Errorf(
					"encode native UDE input report for device %d endpoint 0x%02x: %w",
					entry.identity.DeviceID, publisher.endpoint, err))
				return
			}
			if written <= 0 || written > len(reportBuffer) {
				h.reportFatal(fmt.Errorf(
					"device %d encoded invalid interrupt-IN length %d for endpoint 0x%02x (capacity %d)",
					entry.identity.DeviceID, written, publisher.endpoint, len(reportBuffer)))
				return
			}
			payload = reportBuffer[:written]
		} else {
			payload = entry.device.HandleTransfer(
				ctx, uint32(publisher.endpoint&0x0f), usb.DirectionIn, nil)
		}
		if len(payload) == 0 {
			// The legacy HandleTransfer contract signals a cancelled wait with
			// an empty slice. No report was encoded, so there is nothing to commit.
			if ctx.Err() != nil || publisher.submitCtx.Err() != nil {
				return
			}
			h.reportFatal(fmt.Errorf(
				"device %d returned an empty interrupt-IN report for endpoint 0x%02x",
				entry.identity.DeviceID, publisher.endpoint))
			return
		}
		// Once the controller encoder has returned a report, commit that exact
		// state before an endpoint lifecycle boundary joins this publisher.
		// Only owner-session shutdown may abort the commit.
		if publisher.submitCtx.Err() != nil {
			return
		}
		// The sequence is owned by this endpoint generation and survives a
		// purge/start or reset publisher replacement. There is exactly one live
		// publisher per endpoint and stopInputPublisher joins it before a
		// replacement starts, so reserve the next value without committing it.
		// Owner-session cancellation can land between this point and driver
		// acceptance; committing the counter only after a successful submit keeps
		// accepted reports contiguous without rolling back device encoder state.
		previousSequence := publisher.sequence.Load()
		sequence, err := nextInputReportSequence(previousSequence)
		if err != nil {
			h.reportFatal(fmt.Errorf(
				"reserve native UDE input sequence for device %d endpoint 0x%02x: %w",
				entry.identity.DeviceID, publisher.endpoint, err))
			return
		}
		report := InputReport{
			DeviceID: entry.identity.DeviceID, Generation: entry.identity.Generation,
			EndpointGeneration: publisher.endpointGeneration,
			EndpointAddress:    publisher.endpoint, Transition: transition,
			Sequence: sequence, Payload: payload,
		}
		for {
			err = h.input.SubmitInputReport(publisher.submitCtx, report)
			if !errors.Is(err, ErrInputQueueFull) {
				break
			}
			// The kernel retained every earlier transition and rejected this one
			// before accepting its sequence. Wait one endpoint interval, then retry
			// this exact report. This propagates bounded backpressure without
			// dropping an edge or faulting the owner session.
			retryInterval := publisher.interval
			if retryInterval <= 0 {
				retryInterval = time.Millisecond
			}
			if retryTimer == nil {
				retryTimer = time.NewTimer(retryInterval)
			} else {
				retryTimer.Reset(retryInterval)
			}
			select {
			case <-publisher.submitCtx.Done():
				stopInputDeadlineTimer(retryTimer)
				return
			case <-retryTimer.C:
			}
		}
		if err != nil {
			if publisher.submitCtx.Err() != nil {
				return
			}
			h.reportFatal(fmt.Errorf(
				"submit native UDE input report for device %d endpoint 0x%02x: %w",
				entry.identity.DeviceID, publisher.endpoint, err))
			return
		}
		if !publisher.sequence.CompareAndSwap(previousSequence, sequence) {
			h.reportFatal(fmt.Errorf(
				"commit native UDE input sequence for device %d endpoint 0x%02x: concurrent publisher changed %d",
				entry.identity.DeviceID, publisher.endpoint, previousSequence))
			return
		}
	}
}

type dequeueResult struct {
	op  Operation
	err error
}

func (h *Host) Serve(ctx context.Context) error {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return errors.New("native UDE host is already running")
	}
	if h.started {
		h.mu.Unlock()
		return errors.New("native UDE host sessions are one-shot; open a fresh driver session")
	}
	runCtx, cancel := context.WithCancel(ctx)
	fatal := make(chan error, 1)
	h.runCtx, h.runCancel, h.fatal, h.started, h.running = runCtx, cancel, fatal, true, true
	entries := make([]*registeredDevice, 0, len(h.devices))
	for _, entry := range h.devices {
		entries = append(entries, entry)
	}
	h.mu.Unlock()
	for _, entry := range entries {
		for _, endpoint := range h.activeInputEndpoints(entry) {
			h.startInputPublisher(entry, endpoint.address, endpoint.generation)
		}
	}
	defer func() {
		cancel()
		h.mu.Lock()
		stoppingLanes := make([]*operationLane, 0, len(h.lanes))
		for key, lane := range h.lanes {
			stoppingLanes = append(stoppingLanes, lane)
			delete(h.lanes, key)
		}
		h.failedLanes = make(map[laneKey]error)
		h.mu.Unlock()
		for _, lane := range stoppingLanes {
			stopLane(lane)
		}
		h.laneWG.Wait()
		h.cancelAllOperations()
		h.mu.Lock()
		entries = entries[:0]
		for _, entry := range h.devices {
			entries = append(entries, entry)
		}
		h.running, h.runCtx, h.runCancel, h.fatal = false, nil, nil, nil
		h.mu.Unlock()
		for _, entry := range entries {
			h.stopAllInputPublishers(entry)
		}
	}()

	results := make(chan dequeueResult, h.workers*2)
	var workers sync.WaitGroup
	workers.Add(h.workers)
	for i := 0; i < h.workers; i++ {
		go func() {
			defer workers.Done()
			buffer := make([]byte, OperationSize+MaxIsoPackets*IsoPacketSize+MaxTransferBytes)
			for runCtx.Err() == nil {
				op, err := h.driver.Dequeue(runCtx, buffer)
				select {
				case results <- dequeueResult{op: op, err: err}:
				case <-runCtx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
	finishFatal := func(err error) error {
		cancel()
		workers.Wait()
		return fmt.Errorf("native UDE host session failed: %w", err)
	}

	for {
		// Once a lane or publisher reports a fatal error, do not let an always-
		// ready dequeue stream win repeated select lotteries. Cancelling here
		// also releases every worker before another result is dispatched.
		select {
		case err := <-fatal:
			return finishFatal(err)
		default:
		}
		select {
		case <-runCtx.Done():
			workers.Wait()
			return nil
		case err := <-fatal:
			return finishFatal(err)
		case result := <-results:
			// A worker result and a fatal lane notification can become ready in
			// the same scheduling turn. Fatal is terminal; observe it before
			// touching the newly dequeued operation.
			select {
			case err := <-fatal:
				return finishFatal(err)
			default:
			}
			if result.err != nil {
				cancel()
				workers.Wait()
				if ctx.Err() != nil || errors.Is(result.err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("dequeue native UDE operation: %w", result.err)
			}
			if result.op.Kind == OperationBrokerFault {
				h.reportFatal(errors.New("native UDE kernel broker reported a lost lifecycle notification"))
				continue
			}
			if result.op.Kind == OperationCancel {
				// A management-token cancel is a teardown tombstone for a held
				// lifecycle request which the kernel already retired. It has no
				// future ordinary operation to match, so accepting it must not
				// retain an unbounded cancellation entry in the session map.
				if isManagementToken(result.op.Token) {
					continue
				}
				h.cancelOperation(result.op)
				continue
			}
			if !isLifecycleOperation(result.op.Kind) {
				if err := h.trackOperation(result.op); err != nil {
					h.reportFatal(fmt.Errorf("track operation token %d: %w", result.op.Token, err))
					continue
				}
			}
			if err := h.dispatch(runCtx, result.op); err != nil {
				// Saturation terminates the affected lane and publishes fatal
				// synchronously. Do not wait up to completionTimeout trying to
				// reject that final request before cancelling the owner session.
				select {
				case fatalErr := <-fatal:
					return finishFatal(fatalErr)
				default:
				}
				if isLifecycleOperation(result.op.Kind) && result.op.Token != 0 {
					if completeErr := h.completeLifecycle(runCtx, result.op, statusUnsuccessful); completeErr != nil {
						h.reportFatal(fmt.Errorf("reject lifecycle token %d after dispatch failure %v: %w",
							result.op.Token, err, completeErr))
					}
				} else if !isLifecycleOperation(result.op.Kind) {
					if completeErr := h.completeFailure(runCtx, result.op); completeErr != nil {
						h.reportFatal(fmt.Errorf("reject operation token %d after dispatch failure %v: %w",
							result.op.Token, err, completeErr))
					}
				}
			}
		}
	}
}

func (h *Host) Close() {
	h.mu.RLock()
	cancel := h.runCancel
	h.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

func (h *Host) dispatch(ctx context.Context, op Operation) error {
	if op.EndpointSequence == 0 {
		return errors.New("native UDE operation has zero endpoint sequence")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	key := laneKey{
		deviceID: op.DeviceID, generation: op.Generation,
		endpoint: op.EndpointAddress, endpointGeneration: op.EndpointGeneration,
	}

	h.mu.Lock()
	entry := h.devices[op.DeviceID]
	if entry == nil || entry.stopping || entry.identity.Generation != op.Generation || entry.ctx.Err() != nil {
		h.mu.Unlock()
		return errors.New("native UDE operation targets a stale device generation")
	}
	if terminalErr := h.failedLanes[key]; terminalErr != nil {
		h.mu.Unlock()
		return terminalErr
	}
	lane := h.lanes[key]
	if lane == nil {
		laneCtx, cancel := context.WithCancel(entry.ctx)
		lane = &operationLane{
			key: key, ctx: laneCtx, cancel: cancel,
			input: make(chan Operation, laneQueueDepth), done: make(chan struct{}),
		}
		h.lanes[key] = lane
		h.laneWG.Add(1)
		go h.runLane(lane, entry)
	}
	h.mu.Unlock()
	announcedBarrier := false
	if isDeviceBarrierOperation(op) {
		if err := entry.sequence.announce(op.DeviceSequence); err != nil {
			h.failLane(lane, err)
			return err
		}
		announcedBarrier = op.DeviceSequence != 0
	}
	withdrawBarrier := func() {
		if announcedBarrier {
			entry.sequence.withdraw(op.DeviceSequence)
			announcedBarrier = false
		}
	}

	// Admission is deliberately nonblocking. A queue at the full kernel
	// pending-operation contract means either an ABI/driver contract violation
	// or a terminal endpoint; waiting here would let that one endpoint stall
	// cancellations, lifecycle traffic, and every other controller.
	lane.stateMu.Lock()
	if lane.terminalErr != nil {
		err := lane.terminalErr
		lane.stateMu.Unlock()
		withdrawBarrier()
		return err
	}
	if err := lane.ctx.Err(); err != nil {
		lane.stateMu.Unlock()
		withdrawBarrier()
		return err
	}
	if err := ctx.Err(); err != nil {
		lane.stateMu.Unlock()
		withdrawBarrier()
		return err
	}
	select {
	case lane.input <- op:
		lane.stateMu.Unlock()
		return nil
	default:
		err := fmt.Errorf(
			"native UDE device %d generation %d endpoint 0x%02x generation %d lane is saturated at the %d-operation pending contract",
			key.deviceID, key.generation, key.endpoint, key.endpointGeneration, laneQueueDepth)
		lane.terminalErr = err
		lane.cancel()
		lane.stateMu.Unlock()
		withdrawBarrier()
		h.removeFailedLane(lane, err)
		h.reportFatal(err)
		return err
	}
}

func stopLane(lane *operationLane) {
	lane.stateMu.Lock()
	if lane.terminalErr == nil {
		lane.terminalErr = context.Canceled
	}
	lane.cancel()
	lane.stateMu.Unlock()
}

// removeFailedLane installs a tombstone only when lane is still the exact
// routed instance. An older goroutine can therefore never remove or poison a
// replacement lane created for a later lifecycle.
func (h *Host) removeFailedLane(lane *operationLane, err error) {
	h.mu.Lock()
	if h.lanes[lane.key] == lane {
		delete(h.lanes, lane.key)
		if h.failedLanes[lane.key] == nil {
			h.failedLanes[lane.key] = err
		}
	}
	h.mu.Unlock()
}

func (h *Host) failLane(lane *operationLane, err error) {
	if err == nil {
		return
	}
	lane.stateMu.Lock()
	if lane.terminalErr != nil {
		lane.stateMu.Unlock()
		return
	}
	lane.terminalErr = err
	lane.cancel()
	lane.stateMu.Unlock()

	h.removeFailedLane(lane, err)
	h.reportFatal(err)
}

func (h *Host) retireLane(lane *operationLane) {
	stopLane(lane)
	h.mu.Lock()
	if h.lanes[lane.key] == lane {
		delete(h.lanes, lane.key)
	}
	h.mu.Unlock()
	close(lane.done)
	h.laneWG.Done()
}

func (h *Host) runLane(lane *operationLane, entry *registeredDevice) {
	defer h.retireLane(lane)
	expected := uint64(1)
	pending := make(map[uint64]Operation)
	for {
		select {
		case <-lane.ctx.Done():
			return
		case op := <-lane.input:
			if lane.ctx.Err() != nil {
				return
			}
			if op.EndpointSequence < expected {
				h.failLane(lane, fmt.Errorf("endpoint 0x%02x sequence regressed from %d to %d",
					lane.key.endpoint, expected, op.EndpointSequence))
				return
			}
			if _, duplicate := pending[op.EndpointSequence]; duplicate {
				h.failLane(lane, fmt.Errorf("endpoint 0x%02x repeated pending sequence %d",
					lane.key.endpoint, op.EndpointSequence))
				return
			}
			pending[op.EndpointSequence] = op
			if len(pending) > laneQueueDepth {
				h.failLane(lane, fmt.Errorf("endpoint 0x%02x exceeded the %d-operation reorder bound while waiting for sequence %d",
					lane.key.endpoint, laneQueueDepth, expected))
				return
			}
			for {
				current, ready := pending[expected]
				if !ready {
					break
				}
				delete(pending, expected)
				if isLifecycleOperation(current.Kind) {
					if err := h.processLifecycle(lane.ctx, entry, current); err != nil {
						h.failLane(lane, fmt.Errorf("endpoint 0x%02x lifecycle sequence %d: %w",
							lane.key.endpoint, current.EndpointSequence, err))
						return
					}
				} else {
					if err := h.process(lane.ctx, entry, current); err != nil {
						h.failLane(lane, fmt.Errorf("endpoint 0x%02x complete sequence %d: %w",
							lane.key.endpoint, current.EndpointSequence, err))
						return
					}
				}
				expected++
			}
		}
	}
}

func isLifecycleOperation(kind OperationKind) bool {
	switch kind {
	case OperationEndpointStart, OperationEndpointPurge, OperationEndpointReset,
		OperationDeviceReset, OperationSetInterface, OperationDeviceD0Entry, OperationDeviceD0Exit:
		return true
	default:
		return false
	}
}

// admitEndpointGeneration establishes the endpoint incarnation before any
// controller callback or direct-input publisher can observe it. Device-wide
// lifecycle operations carry generation zero and deliberately bypass this
// endpoint fence. A higher generation permanently retires the prior address
// incarnation; a lower generation is stale even if its endpoint sequence is
// otherwise locally valid.
func (h *Host) admitEndpointGeneration(entry *registeredDevice, op Operation) bool {
	if op.EndpointGeneration == 0 {
		return false
	}
	h.mu.Lock()
	current := entry.endpointGeneration[op.EndpointAddress]
	if current > op.EndpointGeneration {
		h.mu.Unlock()
		return true
	}
	retired := uint32(0)
	if current < op.EndpointGeneration {
		retired = current
		entry.endpointGeneration[op.EndpointAddress] = op.EndpointGeneration
		if entry.activeInput[op.EndpointAddress] != op.EndpointGeneration {
			delete(entry.activeInput, op.EndpointAddress)
		}
		if entry.resettingInput[op.EndpointAddress] != op.EndpointGeneration {
			delete(entry.resettingInput, op.EndpointAddress)
		}
	}
	h.mu.Unlock()
	if retired != 0 {
		h.stopInputPublisher(entry, op.EndpointAddress, retired)
	}
	return false
}

func (h *Host) processLifecycle(ctx context.Context, entry *registeredDevice, op Operation) error {
	gateCtx, lease, superseded, err := entry.sequence.enter(ctx, op)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if h.admitEndpointGeneration(entry, op) {
		defer lease.finish()
		if op.Token == 0 {
			return nil
		}
		return h.completeLifecycle(ctx, op, statusUnsuccessful)
	}
	if superseded {
		defer lease.finish()
		// Endpoint lifecycle notifications describe durable UdeCx state even
		// when their pre-barrier callback must not run. Preserve only the host's
		// minimal publisher bookkeeping; the device-wide barrier owns all actual
		// controller/processor state from this point forward.
		switch op.Kind {
		case OperationEndpointStart:
			h.mu.Lock()
			if entry.endpointGeneration[op.EndpointAddress] == op.EndpointGeneration {
				entry.activeInput[op.EndpointAddress] = op.EndpointGeneration
			}
			h.mu.Unlock()
		case OperationEndpointPurge:
			h.mu.Lock()
			if entry.activeInput[op.EndpointAddress] == op.EndpointGeneration {
				delete(entry.activeInput, op.EndpointAddress)
			}
			if entry.resettingInput[op.EndpointAddress] == op.EndpointGeneration {
				delete(entry.resettingInput, op.EndpointAddress)
			}
			h.mu.Unlock()
		case OperationEndpointReset:
			h.mu.Lock()
			if entry.resettingInput[op.EndpointAddress] == op.EndpointGeneration {
				delete(entry.resettingInput, op.EndpointAddress)
			}
			h.mu.Unlock()
		}
		return h.completeSupersededLifecycle(ctx, entry, op)
	}
	defer lease.finish()

	applyPowerTransition := false
	applyDeviceReset := false
	switch op.Kind {
	case OperationEndpointPurge:
		h.mu.Lock()
		if entry.activeInput[op.EndpointAddress] == op.EndpointGeneration {
			delete(entry.activeInput, op.EndpointAddress)
		}
		if entry.resettingInput[op.EndpointAddress] == op.EndpointGeneration {
			delete(entry.resettingInput, op.EndpointAddress)
		}
		h.mu.Unlock()
		h.stopInputPublisher(entry, op.EndpointAddress, op.EndpointGeneration)
	case OperationEndpointReset:
		h.mu.Lock()
		if entry.endpointGeneration[op.EndpointAddress] == op.EndpointGeneration {
			entry.resettingInput[op.EndpointAddress] = op.EndpointGeneration
		}
		h.mu.Unlock()
		h.stopInputPublisher(entry, op.EndpointAddress, op.EndpointGeneration)
	case OperationDeviceD0Exit:
		h.mu.Lock()
		if op.DeviceSequence > entry.powerSequence {
			entry.powerSequence = op.DeviceSequence
			entry.inD0 = false
			applyPowerTransition = true
		}
		h.mu.Unlock()
		if applyPowerTransition {
			h.stopAllInputPublishers(entry)
		}
	case OperationDeviceReset:
		h.mu.Lock()
		if !entry.resetting {
			entry.resetting = true
			entry.resettingInput = make(map[uint8]uint32)
			applyDeviceReset = true
		}
		h.mu.Unlock()
		if applyDeviceReset {
			h.stopAllInputPublishers(entry)
		}
	}

	lifecycleErr := h.processor.Lifecycle(gateCtx, entry.device, op)
	if errors.Is(context.Cause(gateCtx), errSupersededByDeviceBarrier) {
		return h.completeSupersededLifecycle(ctx, entry, op)
	}
	if op.Token != 0 {
		status := int32(0)
		if lifecycleErr != nil {
			status = statusUnsuccessful
		}
		if err := h.completeLifecycle(gateCtx, op, status); err != nil {
			if errors.Is(context.Cause(gateCtx), errSupersededByDeviceBarrier) {
				return h.completeSupersededLifecycle(ctx, entry, op)
			}
			return fmt.Errorf("acknowledge lifecycle: %w", err)
		}
	}
	if errors.Is(context.Cause(gateCtx), errSupersededByDeviceBarrier) {
		h.discardSupersededLifecycle(entry, op)
		return nil
	}
	if lifecycleErr != nil {
		return lifecycleErr
	}

	switch op.Kind {
	case OperationEndpointStart:
		h.mu.Lock()
		if entry.endpointGeneration[op.EndpointAddress] == op.EndpointGeneration {
			entry.activeInput[op.EndpointAddress] = op.EndpointGeneration
		}
		h.mu.Unlock()
		h.startInputPublisher(entry, op.EndpointAddress, op.EndpointGeneration)
	case OperationEndpointReset:
		h.mu.Lock()
		if entry.resettingInput[op.EndpointAddress] == op.EndpointGeneration {
			delete(entry.resettingInput, op.EndpointAddress)
		}
		restart := entry.activeInput[op.EndpointAddress] == op.EndpointGeneration &&
			entry.endpointGeneration[op.EndpointAddress] == op.EndpointGeneration
		h.mu.Unlock()
		if restart {
			h.startInputPublisher(entry, op.EndpointAddress, op.EndpointGeneration)
		}
	case OperationDeviceD0Entry:
		h.mu.Lock()
		if op.DeviceSequence > entry.powerSequence {
			entry.powerSequence = op.DeviceSequence
			entry.inD0 = true
			applyPowerTransition = true
		}
		h.mu.Unlock()
		if applyPowerTransition {
			for _, endpoint := range h.activeInputEndpoints(entry) {
				h.startInputPublisher(entry, endpoint.address, endpoint.generation)
			}
		}
	case OperationDeviceReset:
		if applyDeviceReset {
			h.mu.Lock()
			entry.resetting = false
			h.mu.Unlock()
			for _, endpoint := range h.activeInputEndpoints(entry) {
				h.startInputPublisher(entry, endpoint.address, endpoint.generation)
			}
		}
	}
	return nil
}

func (h *Host) discardSupersededLifecycle(entry *registeredDevice, op Operation) {
	if op.Kind != OperationEndpointReset {
		return
	}
	h.mu.Lock()
	if entry.resettingInput[op.EndpointAddress] == op.EndpointGeneration {
		delete(entry.resettingInput, op.EndpointAddress)
	}
	h.mu.Unlock()
}

func (h *Host) completeSupersededLifecycle(
	ctx context.Context, entry *registeredDevice, op Operation,
) error {
	h.discardSupersededLifecycle(entry, op)
	if op.Token == 0 {
		return nil
	}
	// A token-bearing lifecycle notification owns a live UdeCx management
	// request. Device barriers cancel the old processor callback, but the kernel
	// intentionally retains that request until user mode acknowledges it (owner
	// teardown is the only kernel-side bulk abort). Complete it outside the
	// canceled sequence context while the old lease is still held, so the next
	// barrier cannot start with a stranded endpoint-reset/interface request.
	if err := h.completeLifecycle(ctx, op, statusUnsuccessful); err != nil {
		return fmt.Errorf("cancel superseded lifecycle: %w", err)
	}
	return nil
}

func (h *Host) completeLifecycle(ctx context.Context, op Operation, status int32) error {
	completionCtx, cancel := context.WithTimeout(ctx, completionTimeout)
	defer cancel()
	return h.driver.Complete(completionCtx, Completion{
		Token: op.Token, DeviceID: op.DeviceID, Generation: op.Generation,
		EndpointGeneration: op.EndpointGeneration, Status: status,
	})
}

func (h *Host) process(ctx context.Context, entry *registeredDevice, op Operation) error {
	gateCtx, lease, superseded, err := entry.sequence.enter(ctx, op)
	if err != nil {
		if ctx.Err() != nil {
			h.cancelOperation(op)
			h.finishOperation(op.Token)
			return nil
		}
		h.finishOperation(op.Token)
		return err
	}
	if h.admitEndpointGeneration(entry, op) {
		defer lease.finish()
		return h.completeFailure(ctx, op)
	}
	if superseded {
		defer lease.finish()
		h.cancelOperation(op)
		h.finishOperation(op.Token)
		return nil
	}
	defer lease.finish()
	configurationBarrier := isSetConfigurationOperation(op)
	if configurationBarrier {
		// SET_CONFIGURATION replaces the child's active USB configuration. The
		// global sequence gate has already joined every brokered endpoint lane;
		// close and join the direct interrupt-IN lane as part of the same barrier
		// so an old report cannot cross the configuration request either.
		h.mu.Lock()
		applyConfigurationBarrier := !entry.resetting
		if applyConfigurationBarrier {
			entry.resetting = true
		}
		h.mu.Unlock()
		if applyConfigurationBarrier {
			h.stopAllInputPublishers(entry)
			defer func() {
				h.mu.Lock()
				entry.resetting = false
				h.mu.Unlock()
				for _, endpoint := range h.activeInputEndpoints(entry) {
					h.startInputPublisher(entry, endpoint.address, endpoint.generation)
				}
			}()
		}
	}

	opCtx, cancel, active := h.beginOperation(gateCtx, op)
	if !active {
		h.finishOperation(op.Token)
		return nil
	}
	defer cancel()

	completion, err := h.processor.Process(opCtx, entry.device, op)
	if errors.Is(context.Cause(gateCtx), errSupersededByDeviceBarrier) {
		h.cancelOperation(op)
		h.finishOperation(op.Token)
		return nil
	}
	if err != nil {
		completion = processorErrorCompletion(op, err)
	}
	if h.operationCancelled(op.Token) {
		h.finishOperation(op.Token)
		return nil
	}
	completion.Token = op.Token
	completion.DeviceID = op.DeviceID
	completion.Generation = op.Generation
	completion.EndpointGeneration = op.EndpointGeneration
	// Keep the completion inside the same cancellable device-sequence lease as
	// the controller callback. A reset announced after Process returns must be
	// able to cancel a blocked driver completion and join it before the reset is
	// applied; using the lane context here would leave that old callback outside
	// the barrier.
	completionCtx, completionCancel := context.WithTimeout(gateCtx, completionTimeout)
	defer completionCancel()
	err = h.driver.Complete(completionCtx, completion)
	if errors.Is(context.Cause(gateCtx), errSupersededByDeviceBarrier) {
		h.cancelOperation(op)
		h.finishOperation(op.Token)
		return nil
	}
	h.finishOperation(op.Token)
	return err
}

func (h *Host) completeFailure(ctx context.Context, op Operation) error {
	if h.operationCancelled(op.Token) {
		h.finishOperation(op.Token)
		return nil
	}
	completionCtx, cancel := context.WithTimeout(ctx, completionTimeout)
	defer cancel()
	err := h.driver.Complete(completionCtx, failureCompletion(op))
	h.finishOperation(op.Token)
	return err
}

func (h *Host) trackOperation(op Operation) error {
	if op.Token == 0 {
		return errors.New("native UDE operation has zero token")
	}
	h.operationMu.Lock()
	defer h.operationMu.Unlock()
	state := h.operations[op.Token]
	if state == nil {
		h.operations[op.Token] = &operationState{
			deviceID: op.DeviceID, generation: op.Generation,
			endpoint: op.EndpointAddress, endpointGeneration: op.EndpointGeneration,
			received: true,
		}
		return nil
	}
	if state.done || state.received || state.deviceID != op.DeviceID ||
		state.generation != op.Generation || state.endpoint != op.EndpointAddress ||
		state.endpointGeneration != op.EndpointGeneration {
		return errors.New("native UDE operation reuses a completed or mismatched token")
	}
	state.received = true
	return nil
}

func (h *Host) reportFatal(err error) {
	if err == nil {
		return
	}
	h.mu.RLock()
	fatal := h.fatal
	h.mu.RUnlock()
	if fatal == nil {
		return
	}
	select {
	case fatal <- err:
	default:
	}
}

func (h *Host) cancelAllOperations() {
	var cancels []context.CancelFunc
	h.operationMu.Lock()
	for _, state := range h.operations {
		if state.cancel != nil {
			cancels = append(cancels, state.cancel)
		}
	}
	h.operations = make(map[uint64]*operationState)
	h.completed = nil
	h.operationMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (h *Host) beginOperation(parent context.Context, op Operation) (context.Context, context.CancelFunc, bool) {
	h.operationMu.Lock()
	defer h.operationMu.Unlock()
	state := h.operations[op.Token]
	if state == nil || state.done || state.cancelled || state.deviceID != op.DeviceID ||
		state.generation != op.Generation || state.endpoint != op.EndpointAddress ||
		state.endpointGeneration != op.EndpointGeneration {
		return parent, func() {}, false
	}
	opCtx, cancel := context.WithCancel(parent)
	state.cancel = cancel
	state.processing = true
	return opCtx, cancel, true
}

func (h *Host) cancelOperation(op Operation) {
	if op.Token == 0 || op.DeviceID == 0 || op.Generation == 0 {
		return
	}
	h.mu.RLock()
	entry := h.devices[op.DeviceID]
	validDevice := entry != nil && entry.identity.Generation == op.Generation
	h.mu.RUnlock()
	if !validDevice {
		return
	}
	h.operationMu.Lock()
	state := h.operations[op.Token]
	if state == nil {
		state = &operationState{
			deviceID: op.DeviceID, generation: op.Generation,
			endpoint: op.EndpointAddress, endpointGeneration: op.EndpointGeneration,
			cancelled: true,
		}
		h.operations[op.Token] = state
	} else if !state.done && state.deviceID == op.DeviceID && state.generation == op.Generation &&
		state.endpoint == op.EndpointAddress &&
		state.endpointGeneration == op.EndpointGeneration {
		state.cancelled = true
	} else {
		h.operationMu.Unlock()
		return
	}
	cancel := state.cancel
	h.operationMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *Host) cancelDeviceOperations(identity DeviceIdentity) {
	var cancels []context.CancelFunc
	h.operationMu.Lock()
	for token, state := range h.operations {
		if !state.done && state.deviceID == identity.DeviceID && state.generation == identity.Generation {
			state.cancelled = true
			if state.cancel != nil {
				cancels = append(cancels, state.cancel)
			}
			if !state.processing {
				delete(h.operations, token)
			}
		}
	}
	h.operationMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (h *Host) operationCancelled(token uint64) bool {
	h.operationMu.Lock()
	defer h.operationMu.Unlock()
	state := h.operations[token]
	return state != nil && state.cancelled
}

func (h *Host) finishOperation(token uint64) {
	h.operationMu.Lock()
	defer h.operationMu.Unlock()
	state := h.operations[token]
	if state == nil || state.done {
		return
	}
	state.cancel = nil
	state.processing = false
	state.done = true
	h.completed = append(h.completed, token)
	if len(h.completed) > completedTokenHistory {
		oldest := h.completed[0]
		h.completed = h.completed[1:]
		if old := h.operations[oldest]; old != nil && old.done {
			delete(h.operations, oldest)
		}
	}
}

func failureCompletion(op Operation) Completion {
	return Completion{
		Token: op.Token, DeviceID: op.DeviceID, Generation: op.Generation,
		EndpointGeneration: op.EndpointGeneration,
		Status:             statusUnsuccessful,
	}
}

type usbdCompletionStatusError interface {
	error
	USBDCompletionStatus() uint32
}

func processorErrorCompletion(op Operation, err error) Completion {
	var usbdError usbdCompletionStatusError
	if errors.As(err, &usbdError) {
		if status := usbdError.USBDCompletionStatus(); status != 0 {
			// UdeCx consumes USBD protocol failures through UdecxUrbComplete,
			// which requires a successful NTSTATUS envelope. A generic
			// processor failure still uses the NTSTATUS failure path below. ISO
			// completions must retain the submitted packet table even when no
			// bytes were serviced; the kernel validates those offsets before it
			// can deliver the protocol status to UdeCx.
			packets := make([]IsoPacket, len(op.IsoPackets))
			for index, packet := range op.IsoPackets {
				packets[index] = IsoPacket{Offset: packet.Offset, Status: int32(status)}
			}
			return Completion{
				Token: op.Token, DeviceID: op.DeviceID, Generation: op.Generation,
				EndpointGeneration: op.EndpointGeneration,
				USBDStatus:         status, IsoPackets: packets,
			}
		}
	}
	return failureCompletion(op)
}

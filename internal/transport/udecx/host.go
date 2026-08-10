package udecx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/usb"
)

const (
	defaultDequeueWorkers = 8
	laneQueueDepth        = 128
	completionTimeout     = 2 * time.Second
	completedTokenHistory = MaxPendingOperations * 2
	statusUnsuccessful    = int32(-1073741823) // STATUS_UNSUCCESSFUL
)

// Driver is the narrow host-side contract implemented by the overlapped
// Windows UdeCx client. Keeping it as an interface makes ordering, teardown,
// and stale-generation behavior testable without loading a kernel driver.
type Driver interface {
	CreateDevice(context.Context, CreateDevice) error
	DestroyDevice(context.Context, DeviceIdentity) error
	Dequeue(context.Context, []byte) (Operation, error)
	Complete(context.Context, Completion) error
	QueryStats(context.Context) (Stats, error)
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
	identity DeviceIdentity
	device   usb.Device
	ctx      context.Context
	cancel   context.CancelFunc
}

type laneKey struct {
	deviceID   uint64
	generation uint32
	endpoint   uint8
}

type operationLane struct {
	key    laneKey
	ctx    context.Context
	cancel context.CancelFunc
	input  chan Operation
}

type operationState struct {
	deviceID   uint64
	generation uint32
	cancel     context.CancelFunc
	cancelled  bool
	received   bool
	processing bool
	done       bool
}

// Host owns one exclusive driver session and routes operations concurrently
// across endpoints while preserving strict FIFO within each endpoint.
type Host struct {
	driver    Driver
	processor OperationProcessor
	workers   int

	lifecycleMu sync.Mutex
	mu          sync.RWMutex
	devices     map[uint64]*registeredDevice
	generations map[uint64]uint32
	lanes       map[laneKey]*operationLane
	runCtx      context.Context
	runCancel   context.CancelFunc
	running     bool
	laneWG      sync.WaitGroup
	operationMu sync.Mutex
	operations  map[uint64]*operationState
	completed   []uint64
}

func NewHost(driver Driver, processor OperationProcessor, workers int) (*Host, error) {
	if driver == nil || processor == nil {
		return nil, errors.New("native UDE host requires a driver and operation processor")
	}
	if workers <= 0 {
		workers = defaultDequeueWorkers
	}
	return &Host{
		driver: driver, processor: processor, workers: workers,
		devices:     make(map[uint64]*registeredDevice),
		generations: make(map[uint64]uint32),
		lanes:       make(map[laneKey]*operationLane),
		operations:  make(map[uint64]*operationState),
	}, nil
}

// Register publishes a USB device using a fresh generation. The routing entry
// is installed before the driver plugs in the child because Windows can submit
// its first descriptor request before CreateDevice returns.
func (h *Host) Register(ctx context.Context, deviceID uint64, dev usb.Device) (DeviceIdentity, error) {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	if deviceID == 0 || dev == nil {
		return DeviceIdentity{}, ErrInvalidRange
	}

	h.mu.Lock()
	if _, exists := h.devices[deviceID]; exists {
		h.mu.Unlock()
		return DeviceIdentity{}, fmt.Errorf("native UDE device %d is already registered", deviceID)
	}
	generation := h.generations[deviceID] + 1
	if generation == 0 {
		generation = 1
	}
	identity := DeviceIdentity{DeviceID: deviceID, Generation: generation}
	deviceCtx, cancel := context.WithCancel(context.Background())
	entry := &registeredDevice{identity: identity, device: dev, ctx: deviceCtx, cancel: cancel}
	h.devices[deviceID] = entry
	h.generations[deviceID] = generation
	h.mu.Unlock()

	snapshot, err := SnapshotDevice(deviceID, generation, dev)
	if err == nil {
		err = h.driver.CreateDevice(ctx, snapshot)
	}
	if err != nil {
		h.mu.Lock()
		if h.devices[deviceID] == entry {
			delete(h.devices, deviceID)
		}
		h.mu.Unlock()
		cancel()
		return DeviceIdentity{}, err
	}
	return identity, nil
}

func (h *Host) Unregister(ctx context.Context, identity DeviceIdentity) error {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()

	h.mu.RLock()
	entry := h.devices[identity.DeviceID]
	if entry == nil || entry.identity.Generation != identity.Generation {
		h.mu.RUnlock()
		return fmt.Errorf("native UDE device %d generation %d is not registered",
			identity.DeviceID, identity.Generation)
	}
	h.mu.RUnlock()

	// Keep routing live until the driver has transactionally unplugged the
	// child. If unplug fails, callers can retry without losing the generation,
	// endpoint lanes, or the ability to complete already-issued Windows URBs.
	if err := h.driver.DestroyDevice(ctx, identity); err != nil {
		return err
	}

	h.mu.Lock()
	if h.devices[identity.DeviceID] != entry {
		h.mu.Unlock()
		return errors.New("native UDE device changed during serialized removal")
	}
	delete(h.devices, identity.DeviceID)
	entry.cancel()
	for key, lane := range h.lanes {
		if key.deviceID == identity.DeviceID && key.generation == identity.Generation {
			lane.cancel()
			delete(h.lanes, key)
		}
	}
	h.mu.Unlock()
	h.cancelDeviceOperations(identity)
	h.processor.Reset(entry.device, identity)
	return nil
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
	runCtx, cancel := context.WithCancel(ctx)
	h.runCtx, h.runCancel, h.running = runCtx, cancel, true
	h.mu.Unlock()
	defer func() {
		cancel()
		h.mu.Lock()
		for key, lane := range h.lanes {
			lane.cancel()
			delete(h.lanes, key)
		}
		h.running, h.runCtx, h.runCancel = false, nil, nil
		h.mu.Unlock()
		h.laneWG.Wait()
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

	for {
		select {
		case <-runCtx.Done():
			workers.Wait()
			return nil
		case result := <-results:
			if result.err != nil {
				cancel()
				workers.Wait()
				if ctx.Err() != nil || errors.Is(result.err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("dequeue native UDE operation: %w", result.err)
			}
			if result.op.Kind == OperationCancel {
				h.cancelOperation(result.op)
				continue
			}
			if !isLifecycleOperation(result.op.Kind) {
				if err := h.trackOperation(result.op); err != nil {
					h.completeUntrackedFailure(runCtx, result.op)
					continue
				}
			}
			if err := h.dispatch(runCtx, result.op); err != nil {
				if !isLifecycleOperation(result.op.Kind) {
					h.completeFailure(runCtx, result.op)
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
	key := laneKey{deviceID: op.DeviceID, generation: op.Generation, endpoint: op.EndpointAddress}

	h.mu.Lock()
	entry := h.devices[op.DeviceID]
	if entry == nil || entry.identity.Generation != op.Generation {
		h.mu.Unlock()
		return errors.New("native UDE operation targets a stale device generation")
	}
	lane := h.lanes[key]
	if lane == nil {
		laneCtx, cancel := context.WithCancel(entry.ctx)
		lane = &operationLane{key: key, ctx: laneCtx, cancel: cancel, input: make(chan Operation, laneQueueDepth)}
		h.lanes[key] = lane
		h.laneWG.Add(1)
		go h.runLane(lane, entry)
	}
	h.mu.Unlock()

	select {
	case lane.input <- op:
		return nil
	case <-lane.ctx.Done():
		return lane.ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Host) runLane(lane *operationLane, entry *registeredDevice) {
	defer h.laneWG.Done()
	expected := uint64(1)
	pending := make(map[uint64]Operation)
	for {
		select {
		case <-lane.ctx.Done():
			return
		case op := <-lane.input:
			if op.EndpointSequence < expected {
				if !isLifecycleOperation(op.Kind) {
					h.completeFailure(lane.ctx, op)
				}
				continue
			}
			if _, duplicate := pending[op.EndpointSequence]; duplicate {
				if !isLifecycleOperation(op.Kind) {
					h.completeFailure(lane.ctx, op)
				}
				continue
			}
			pending[op.EndpointSequence] = op
			if len(pending) > laneQueueDepth {
				for _, queued := range pending {
					if !isLifecycleOperation(queued.Kind) {
						h.completeFailure(lane.ctx, queued)
					}
				}
				return
			}
			for {
				current, ready := pending[expected]
				if !ready {
					break
				}
				delete(pending, expected)
				if isLifecycleOperation(current.Kind) {
					_ = h.processor.Lifecycle(lane.ctx, entry.device, current)
				} else {
					h.process(lane.ctx, entry.device, current)
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

func (h *Host) process(ctx context.Context, dev usb.Device, op Operation) {
	opCtx, cancel, active := h.beginOperation(ctx, op)
	if !active {
		h.finishOperation(op.Token)
		return
	}
	defer cancel()

	completion, err := h.processor.Process(opCtx, dev, op)
	if err != nil {
		completion = failureCompletion(op)
	}
	if h.operationCancelled(op.Token) {
		h.finishOperation(op.Token)
		return
	}
	completion.Token = op.Token
	completion.DeviceID = op.DeviceID
	completion.Generation = op.Generation
	completionCtx, completionCancel := context.WithTimeout(ctx, completionTimeout)
	defer completionCancel()
	_ = h.driver.Complete(completionCtx, completion)
	h.finishOperation(op.Token)
}

func (h *Host) completeFailure(ctx context.Context, op Operation) {
	if h.operationCancelled(op.Token) {
		h.finishOperation(op.Token)
		return
	}
	completionCtx, cancel := context.WithTimeout(ctx, completionTimeout)
	defer cancel()
	_ = h.driver.Complete(completionCtx, failureCompletion(op))
	h.finishOperation(op.Token)
}

func (h *Host) completeUntrackedFailure(ctx context.Context, op Operation) {
	completionCtx, cancel := context.WithTimeout(ctx, completionTimeout)
	defer cancel()
	_ = h.driver.Complete(completionCtx, failureCompletion(op))
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
			deviceID: op.DeviceID, generation: op.Generation, received: true,
		}
		return nil
	}
	if state.done || state.received || state.deviceID != op.DeviceID || state.generation != op.Generation {
		return errors.New("native UDE operation reuses a completed or mismatched token")
	}
	state.received = true
	return nil
}

func (h *Host) beginOperation(parent context.Context, op Operation) (context.Context, context.CancelFunc, bool) {
	h.operationMu.Lock()
	defer h.operationMu.Unlock()
	state := h.operations[op.Token]
	if state == nil || state.done || state.cancelled {
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
			deviceID: op.DeviceID, generation: op.Generation, cancelled: true,
		}
		h.operations[op.Token] = state
	} else if !state.done && state.deviceID == op.DeviceID && state.generation == op.Generation {
		state.cancelled = true
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
		Status: statusUnsuccessful,
	}
}

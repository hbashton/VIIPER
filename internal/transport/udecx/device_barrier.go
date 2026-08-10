package udecx

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var errSupersededByDeviceBarrier = errors.New("native UDE operation superseded by device lifecycle barrier")

const (
	usbRequestTypeStandardToDevice = 0x00
	usbRequestSetConfiguration     = 0x09
)

// deviceSequenceBarrier preserves the kernel's generation-scoped
// DeviceSequence while still allowing operations on independent endpoints to
// execute concurrently. Device-wide lifecycle boundaries announce themselves
// as soon as they are dispatched, cancel every earlier callback, join those
// callbacks, and hold every later sequence until the boundary is applied.
type deviceSequenceBarrier struct {
	mu              sync.Mutex
	next            uint64
	changed         chan struct{}
	active          map[uint64]*deviceSequenceLease
	pendingBarriers map[uint64]struct{}
	activeBarrier   *deviceSequenceLease
}

type deviceSequenceLease struct {
	gate     *deviceSequenceBarrier
	sequence uint64
	barrier  bool
	cancel   context.CancelCauseFunc
	once     sync.Once
}

func newDeviceSequenceBarrier() *deviceSequenceBarrier {
	return &deviceSequenceBarrier{
		next:            1,
		changed:         make(chan struct{}),
		active:          make(map[uint64]*deviceSequenceLease),
		pendingBarriers: make(map[uint64]struct{}),
	}
}

func isDeviceBarrierOperation(op Operation) bool {
	switch op.Kind {
	case OperationDeviceReset, OperationDeviceD0Entry, OperationDeviceD0Exit:
		return true
	case OperationControl:
		return isSetConfigurationOperation(op)
	default:
		return false
	}
}

func isSetConfigurationOperation(op Operation) bool {
	return op.Kind == OperationControl && op.EndpointAddress == 0 &&
		op.SetupPacket[0] == usbRequestTypeStandardToDevice &&
		op.SetupPacket[1] == usbRequestSetConfiguration
}

func (g *deviceSequenceBarrier) signalLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

func (g *deviceSequenceBarrier) firstPendingBarrierLocked() uint64 {
	var first uint64
	for sequence := range g.pendingBarriers {
		if first == 0 || sequence < first {
			first = sequence
		}
	}
	return first
}

func (g *deviceSequenceBarrier) announce(sequence uint64) error {
	if sequence == 0 {
		return nil
	}
	var cancels []context.CancelCauseFunc
	g.mu.Lock()
	if _, announced := g.pendingBarriers[sequence]; announced {
		g.mu.Unlock()
		return nil
	}
	if sequence < g.next {
		next := g.next
		g.mu.Unlock()
		return fmt.Errorf("device lifecycle sequence %d arrived after sequence %d was admitted", sequence, next-1)
	}
	g.pendingBarriers[sequence] = struct{}{}
	for activeSequence, lease := range g.active {
		if activeSequence < sequence {
			cancels = append(cancels, lease.cancel)
		}
	}
	g.signalLocked()
	g.mu.Unlock()
	for _, cancel := range cancels {
		cancel(errSupersededByDeviceBarrier)
	}
	return nil
}

func (g *deviceSequenceBarrier) withdraw(sequence uint64) {
	g.mu.Lock()
	if g.activeBarrier == nil || g.activeBarrier.sequence != sequence {
		if _, pending := g.pendingBarriers[sequence]; pending {
			delete(g.pendingBarriers, sequence)
			g.signalLocked()
		}
	}
	g.mu.Unlock()
}

func (g *deviceSequenceBarrier) enter(
	parent context.Context, op Operation,
) (context.Context, *deviceSequenceLease, bool, error) {
	if op.DeviceSequence == 0 {
		return parent, &deviceSequenceLease{}, false, nil
	}
	barrier := isDeviceBarrierOperation(op)
	if barrier {
		if err := g.announce(op.DeviceSequence); err != nil {
			return parent, nil, false, err
		}
	}

	for {
		g.mu.Lock()
		if op.DeviceSequence < g.next {
			next := g.next
			g.mu.Unlock()
			if barrier {
				g.withdraw(op.DeviceSequence)
			}
			return parent, nil, false, fmt.Errorf(
				"device sequence regressed from %d to %d", next, op.DeviceSequence)
		}
		if op.DeviceSequence != g.next || g.activeBarrier != nil {
			changed := g.changed
			g.mu.Unlock()
			select {
			case <-changed:
				continue
			case <-parent.Done():
				if barrier {
					g.withdraw(op.DeviceSequence)
				}
				return parent, nil, false, parent.Err()
			}
		}

		if !barrier {
			firstBarrier := g.firstPendingBarrierLocked()
			if firstBarrier != 0 && op.DeviceSequence <= firstBarrier {
				if op.DeviceSequence == firstBarrier {
					g.mu.Unlock()
					return parent, nil, false, fmt.Errorf(
						"device sequence %d was announced as both lifecycle barrier and ordinary work",
						op.DeviceSequence)
				}
				ctx, cancel := context.WithCancelCause(parent)
				lease := &deviceSequenceLease{
					gate: g, sequence: op.DeviceSequence, cancel: cancel,
				}
				g.active[op.DeviceSequence] = lease
				g.next++
				g.signalLocked()
				g.mu.Unlock()
				// Announcement predated admission, so this callback must never
				// enter the processor. Keep a canceled lease active until its host-
				// side token/management cleanup joins; the barrier waits on it just
				// like a callback that was already running when announced.
				cancel(errSupersededByDeviceBarrier)
				return ctx, lease, true, nil
			}
			ctx, cancel := context.WithCancelCause(parent)
			lease := &deviceSequenceLease{
				gate: g, sequence: op.DeviceSequence, cancel: cancel,
			}
			g.active[op.DeviceSequence] = lease
			g.next++
			g.signalLocked()
			g.mu.Unlock()
			return ctx, lease, false, nil
		}

		ctx, cancel := context.WithCancelCause(parent)
		lease := &deviceSequenceLease{
			gate: g, sequence: op.DeviceSequence, barrier: true, cancel: cancel,
		}
		g.activeBarrier = lease
		g.next++
		g.signalLocked()
		for len(g.active) != 0 {
			changed := g.changed
			g.mu.Unlock()
			<-changed
			g.mu.Lock()
		}
		g.mu.Unlock()
		if err := parent.Err(); err != nil {
			lease.finish()
			return parent, nil, false, err
		}
		return ctx, lease, false, nil
	}
}

func (l *deviceSequenceLease) finish() {
	if l == nil || l.gate == nil {
		return
	}
	l.once.Do(func() {
		l.cancel(context.Canceled)
		g := l.gate
		g.mu.Lock()
		if l.barrier {
			if g.activeBarrier == l {
				g.activeBarrier = nil
				delete(g.pendingBarriers, l.sequence)
				g.signalLocked()
			}
		} else if g.active[l.sequence] == l {
			delete(g.active, l.sequence)
			g.signalLocked()
		}
		g.mu.Unlock()
	})
}

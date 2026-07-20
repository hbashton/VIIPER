package api

import (
	"context"
	"net"
	"sync"
	"time"
)

// deviceStreamKey identifies the lifetime of one virtual device. Bus and
// device identifiers can eventually be reused, so the monotonically increasing
// generation in deviceStreamOwnership remains authoritative across reconnects.
type deviceStreamKey struct {
	busID uint32
	devID string
}

// deviceStreamCoordinator gives each virtual device exactly one current API
// stream. A replacement that overlaps the old transport claims ownership first,
// closes that transport, and waits for its handler to finish. A replacement
// opened just after the old transport closes cancels its deferred finalization
// during the reconnect grace instead.
//
// Besides preventing two clients from concurrently mutating one device, this
// ordering is important for audio devices: an old handler must not clear the
// callback or reset the microphone buffer after its replacement has started.
type deviceStreamCoordinator struct {
	mu      sync.Mutex
	streams map[deviceStreamKey]*deviceStreamOwnership
}

type deviceStreamOwnership struct {
	generation    uint64
	active        bool
	conn          net.Conn
	done          chan struct{}
	finalizeTimer *time.Timer
	cleanupTimer  *time.Timer
	finalized     bool
}

type deviceStreamLease struct {
	coordinator  *deviceStreamCoordinator
	key          deviceStreamKey
	generation   uint64
	done         chan struct{}
	previousDone <-chan struct{}
	finishOnce   sync.Once
}

func (c *deviceStreamCoordinator) claim(key deviceStreamKey,
	conn net.Conn) *deviceStreamLease {
	c.mu.Lock()
	if c.streams == nil {
		c.streams = make(map[deviceStreamKey]*deviceStreamOwnership)
	}
	state := c.streams[key]
	if state == nil {
		state = &deviceStreamOwnership{}
		c.streams[key] = state
	}

	if state.cleanupTimer != nil {
		state.cleanupTimer.Stop()
		state.cleanupTimer = nil
	}
	if state.finalizeTimer != nil {
		state.finalizeTimer.Stop()
		state.finalizeTimer = nil
	}

	previousConn := state.conn
	previousDone := state.done
	state.generation++
	done := make(chan struct{})
	state.active = true
	state.conn = conn
	state.done = done
	state.finalized = false
	lease := &deviceStreamLease{
		coordinator: c,
		key:         key, generation: state.generation,
		done: done, previousDone: previousDone,
	}
	c.mu.Unlock()

	// Closing the displaced connection unblocks every built-in handler's read
	// loop. Do this outside the coordinator lock because Close can enter an
	// authentication wrapper or operating-system transport.
	if previousConn != nil && previousDone != nil {
		_ = previousConn.Close()
	}

	return lease
}

// waitForTurn waits until the displaced handler has returned. It reports false
// when an even newer stream superseded this lease while it was waiting.
func (l *deviceStreamLease) waitForTurn(ctx context.Context) bool {
	if l.previousDone != nil {
		select {
		case <-l.previousDone:
		case <-ctx.Done():
			return false
		}
	}

	c := l.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.streams[l.key]
	return state != nil && state.active &&
		state.generation == l.generation && state.done == l.done
}

// finish closes this lease's completion signal exactly once. Only the current
// generation is allowed to finalize stream-owned device state and arm device
// cleanup; a superseded handler merely releases the next waiter.
//
// finalizeCurrent is deferred for reconnectGrace so the common close-old then
// open-new reconnect ordering retains buffered microphone audio. A same-device
// claim cancels both timers and advances the generation, so even a timer that
// has already fired but is waiting on the lock cannot finalize replacement
// state. Cleanup forces any still-pending finalization before device removal.
// Both callbacks run while the coordinator lock is held and must not call back
// into this coordinator.
func (l *deviceStreamLease) finish(reconnectGrace, cleanupDelay time.Duration,
	deviceContext context.Context, finalizeCurrent, cleanup func()) {
	l.finishOnce.Do(func() {
		c := l.coordinator
		c.mu.Lock()
		state := c.streams[l.key]
		current := state != nil && state.active &&
			state.generation == l.generation && state.done == l.done
		if !current {
			close(l.done)
			c.mu.Unlock()
			return
		}

		state.active = false
		state.conn = nil
		state.done = nil
		generation := state.generation
		close(l.done)
		state.finalizeTimer = time.AfterFunc(reconnectGrace, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			currentState := c.streams[l.key]
			if currentState == nil || currentState.active ||
				currentState.generation != generation || currentState.finalized {
				return
			}
			currentState.finalizeTimer = nil
			currentState.finalized = true
			if deviceContext != nil {
				select {
				case <-deviceContext.Done():
					return
				default:
				}
			}
			if finalizeCurrent != nil {
				finalizeCurrent()
			}
		})
		state.cleanupTimer = time.AfterFunc(cleanupDelay, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			currentState := c.streams[l.key]
			if currentState == nil || currentState.active ||
				currentState.generation != generation {
				return
			}
			currentState.cleanupTimer = nil
			if !currentState.finalized {
				if currentState.finalizeTimer != nil {
					currentState.finalizeTimer.Stop()
					currentState.finalizeTimer = nil
				}
				currentState.finalized = true
				if finalizeCurrent != nil {
					finalizeCurrent()
				}
			}
			if deviceContext != nil {
				select {
				case <-deviceContext.Done():
					return
				default:
				}
			}
			if cleanup != nil {
				cleanup()
			}
		})
		c.mu.Unlock()
	})
}

// abandon releases waiters without scheduling cleanup. It is used by a stream
// that was superseded before its device handler began, or whose device context
// was already removed.
func (l *deviceStreamLease) abandon() {
	l.finishOnce.Do(func() {
		c := l.coordinator
		c.mu.Lock()
		state := c.streams[l.key]
		current := state != nil && state.active &&
			state.generation == l.generation && state.done == l.done
		close(l.done)
		if current {
			state.active = false
			state.conn = nil
			state.done = nil
		}
		c.mu.Unlock()
	})
}

// scheduleCleanup schedules the initial no-client cleanup using the same
// generation gate as reconnect cleanup. A stream claim cancels this timer.
func (c *deviceStreamCoordinator) scheduleCleanup(key deviceStreamKey,
	delay time.Duration, deviceContext context.Context, cleanup func()) {
	c.mu.Lock()
	if c.streams == nil {
		c.streams = make(map[deviceStreamKey]*deviceStreamOwnership)
	}
	state := c.streams[key]
	if state == nil {
		state = &deviceStreamOwnership{}
		c.streams[key] = state
	}
	if state.active {
		c.mu.Unlock()
		return
	}
	if state.cleanupTimer != nil {
		state.cleanupTimer.Stop()
	}
	generation := state.generation
	state.cleanupTimer = time.AfterFunc(delay, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		current := c.streams[key]
		if current == nil || current.active ||
			current.generation != generation {
			return
		}
		current.cleanupTimer = nil
		if deviceContext != nil {
			select {
			case <-deviceContext.Done():
				return
			default:
			}
		}
		if cleanup != nil {
			cleanup()
		}
	})
	c.mu.Unlock()
}

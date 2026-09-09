package api

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

// A cleanup record is issued before native submission for one authenticated
// registration. Public aliases and returned hub ports alone cannot issue one.
// Tombstones also catch a driver retry enqueued AFTER the initial stop sweep.
type xboxOneRetryCleanup struct {
	mu      sync.Mutex
	entries map[string]*xboxOneRetryEntry
	stop    func(context.Context, usbip.ExportMeta, uint16) (int32, error)
	logger  *slog.Logger
	delays  []time.Duration
}

type xboxOneRetryEntry struct {
	meta       usbip.ExportMeta
	retired    <-chan struct{}
	port       uint16
	attachDone chan struct{}
	finish     sync.Once
	running    bool
	again      bool
}

func (entry *xboxOneRetryEntry) isRetired() bool {
	select {
	case <-entry.retired:
		return true
	default:
		return false
	}
}

type xboxOneRetryContextKey struct{}
type xboxOneRetryContext struct {
	server       *Server
	registration virtualbus.DeviceMeta
}

// WithXboxOneRetryCleanup supplies the exact registration consumed by the
// actual native/command attach boundary.
// Test attach implementations do not acquire a real driver cleanup obligation.
func WithXboxOneRetryCleanup(ctx context.Context, s *Server, registration virtualbus.DeviceMeta) context.Context {
	return context.WithValue(ctx, xboxOneRetryContextKey{}, xboxOneRetryContext{s, registration})
}

func newXboxOneRetryCleanup(logger *slog.Logger) *xboxOneRetryCleanup {
	return &xboxOneRetryCleanup{entries: make(map[string]*xboxOneRetryEntry),
		stop: stopLocalhostXboxOneRetries, logger: logger,
		delays: []time.Duration{0, 100 * time.Millisecond, 400 * time.Millisecond,
			time.Second, 3 * time.Second, 10 * time.Second, 20 * time.Second}}
}

// ArmXboxOneRetryCleanup revalidates the exact activation registration before
// submission. Completion must be signalled even on failed/cancelled
// native operations. Registration cancellation, not request cancellation,
// authorizes cleanup; a timed-out HTTP/API caller cannot stop a live pad.
func (s *Server) ArmXboxOneRetryCleanup(registration virtualbus.DeviceMeta) (func(), error) {
	return s.xboxRetries.arm(registration, s.usbs.GetListenPort())
}

func (c *xboxOneRetryCleanup) arm(registration virtualbus.DeviceMeta, port uint16) (func(), error) {
	alias, err := validatedAutoAttachBusID(&registration.Meta, port)
	if err != nil || !usbip.ValidProductionXboxOneBusID(alias) ||
		registration.Context == nil || registration.Context.Err() != nil ||
		registration.Bus == nil || !registration.Bus.AuthenticatesRegistration(registration) {
		return nil, fmt.Errorf("invalid production retry cleanup registration")
	}
	c.mu.Lock()
	if len(c.entries) >= 65536 || c.entries[alias] != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("production retry cleanup identity unavailable")
	}
	// Retain only immutable export metadata and a cancellation signal, never
	// the retired device, its buffers, native import or VirtualBus ownership.
	entry := &xboxOneRetryEntry{meta: registration.Meta, retired: registration.Context.Done(), port: port, attachDone: make(chan struct{})}
	c.entries[alias] = entry
	c.mu.Unlock()
	context.AfterFunc(registration.Context, func() { c.observe(alias) })
	return func() { entry.finish.Do(func() { close(entry.attachDone) }) }, nil
}

// Called only at failed import selection. It never grants authority from the
// request: a matching, already-retired, previously armed entry is required.
// No native call, wait or goroutine per duplicate request on the USB hot path.
func (c *xboxOneRetryCleanup) observe(alias string) {
	c.mu.Lock()
	entry := c.entries[alias]
	if entry == nil || !entry.isRetired() {
		c.mu.Unlock()
		return
	}
	if entry.running {
		entry.again = true
		c.mu.Unlock()
		return
	}
	entry.running = true
	c.mu.Unlock()
	go c.sweep(entry)
}

func (c *xboxOneRetryCleanup) sweep(entry *xboxOneRetryEntry) {
	// Cancellation is not native completion. In particular, a late successful
	// attach must settle before the retirement worker touches its retry location.
	<-entry.attachDone
	for {
		var lastErr error
		for _, delay := range c.delays {
			if delay > 0 {
				time.Sleep(delay)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			count, err := c.stop(ctx, entry.meta, entry.port)
			cancel()
			lastErr = err
			if err == nil && count > 0 {
				c.logger.Debug("Stopped retired Xbox One USB/IP retry", "count", count)
				break
			}
		}
		if lastErr != nil {
			c.logger.Warn("Retired Xbox One USB/IP retry cleanup incomplete", "error", lastErr)
		}
		c.mu.Lock()
		if entry.again {
			entry.again = false
			c.mu.Unlock()
			continue
		}
		entry.running = false
		c.mu.Unlock()
		return
	}
}

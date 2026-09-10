package cmd

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestServerAPIFailureClosesAndJoinsOnlyAttemptResources(t *testing.T) {
	for _, apiCreated := range []bool{false, true} {
		name := "missing-api-address"
		if apiCreated {
			name = "api-listen-failed"
		}
		t.Run(name, func(t *testing.T) {
			attempt := newServerLifecycleFixture(t)
			untouched := newServerLifecycleFixture(t)
			startupError := errors.New("original API startup failure")
			cleanupError := errors.New("independent USB cleanup failure")
			result := make(chan error, 1)
			go func() {
				result <- runOwnedServers(context.Background(), attempt.serve, attempt.ready,
					func() error { _ = attempt.closeUSB(); return cleanupError },
					func() (func(), error) {
						if apiCreated {
							return attempt.closeAPI, startupError
						}
						return nil, startupError
					})
			}()
			require.Same(t, startupError, waitServerLifecycleResult(t, result),
				"Cleanup must preserve the actual startup failure.")
			require.Equal(t, int32(1), attempt.usbCloses.Load(), "API startup failure leaked the USB listener")
			require.Zero(t, untouched.usbCloses.Load(), "An unrelated attempt is not owned here")
			require.Zero(t, untouched.apiCloses.Load())
			select {
			case <-attempt.done:
			default:
				t.Fatal("The listener owner returned before its USB goroutine completed")
			}
			if apiCreated {
				require.Equal(t, []string{"api", "usb"}, attempt.order)
				require.Equal(t, int32(1), attempt.apiCloses.Load())
			} else {
				require.Equal(t, []string{"usb"}, attempt.order)
				require.Zero(t, attempt.apiCloses.Load())
			}
		})
	}
}

func TestServerUSBBindFailureReturnsWithoutWaitingForReadyOrStartingAPI(t *testing.T) {
	attempt := newServerLifecycleFixture(t)
	bindError := errors.New("USB bind failed")
	var apiStarts atomic.Int32
	result := make(chan error, 1)
	go func() {
		result <- runOwnedServers(context.Background(), func() error {
			defer close(attempt.done)
			return bindError
		}, attempt.ready, attempt.closeUSB, func() (func(), error) {
			apiStarts.Add(1)
			return attempt.closeAPI, nil
		})
	}()
	require.Same(t, bindError, waitServerLifecycleResult(t, result))
	require.Zero(t, apiStarts.Load())
	require.Equal(t, int32(1), attempt.usbCloses.Load())
	require.Zero(t, attempt.apiCloses.Load())
}

func TestServerCancellationBeforeReadyClosesAndJoinsWithoutStartingAPI(t *testing.T) {
	attempt := newServerLifecycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	var apiStarts atomic.Int32
	result := make(chan error, 1)
	go func() {
		result <- runOwnedServers(ctx, func() error {
			defer close(attempt.done)
			close(started)
			<-attempt.stop
			return nil
		}, attempt.ready, attempt.closeUSB, func() (func(), error) {
			apiStarts.Add(1)
			return attempt.closeAPI, nil
		})
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("The synthetic USB owner did not start")
	}
	cancel()
	require.NoError(t, waitServerLifecycleResult(t, result))
	require.Zero(t, apiStarts.Load())
	require.Equal(t, int32(1), attempt.usbCloses.Load())
	select {
	case <-attempt.done:
	default:
		t.Fatal("Cancellation returned before the USB owner finished")
	}
}

func TestServerNormalCancellationClosesAPIThenUSBExactlyOnce(t *testing.T) {
	attempt := newServerLifecycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runOwnedServers(ctx, attempt.serve, attempt.ready, attempt.closeUSB,
			func() (func(), error) { cancel(); return attempt.closeAPI, nil })
	}()
	require.NoError(t, waitServerLifecycleResult(t, result))
	require.Equal(t, []string{"api", "usb"}, attempt.order)
	require.Equal(t, int32(1), attempt.usbCloses.Load())
	require.Equal(t, int32(1), attempt.apiCloses.Load())
}

func TestServerUnexpectedUSBExitStillClosesAttemptAndPreservesServeError(t *testing.T) {
	attempt := newServerLifecycleFixture(t)
	serveError := errors.New("USB listener failed after startup")
	exit := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- runOwnedServers(context.Background(), func() error {
			defer close(attempt.done)
			close(attempt.ready)
			<-exit
			return serveError
		}, attempt.ready, attempt.closeUSB, func() (func(), error) {
			close(exit)
			return attempt.closeAPI, nil
		})
	}()
	require.Same(t, serveError, waitServerLifecycleResult(t, result))
	require.Equal(t, []string{"api", "usb"}, attempt.order)
	require.Equal(t, int32(1), attempt.usbCloses.Load())
	require.Equal(t, int32(1), attempt.apiCloses.Load())
}

func TestServerCanRetryAfterFailedAttemptWithoutRetainedListenerOwnership(t *testing.T) {
	for index := 0; index < 2; index++ {
		attempt := newServerLifecycleFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			result <- runOwnedServers(ctx, attempt.serve, attempt.ready, attempt.closeUSB,
				func() (func(), error) {
					if index == 0 {
						return attempt.closeAPI, errors.New("first API startup failed")
					}
					cancel()
					return attempt.closeAPI, nil
				})
		}()
		err := waitServerLifecycleResult(t, result)
		cancel()
		if index == 0 {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		require.Equal(t, []string{"api", "usb"}, attempt.order)
		select {
		case <-attempt.done:
		default:
			t.Fatal("A previous attempt retained its listener ownership")
		}
	}
}

type serverLifecycleFixture struct {
	ready     chan struct{}
	stop      chan struct{}
	done      chan struct{}
	stopOnce  sync.Once
	usbCloses atomic.Int32
	apiCloses atomic.Int32
	order     []string
}

func newServerLifecycleFixture(t *testing.T) *serverLifecycleFixture {
	f := &serverLifecycleFixture{
		ready: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{}),
	}
	// The fake never owns a socket, controller, process or production address.
	// Always unblock a failed-baseline goroutine before the test returns.
	t.Cleanup(func() { f.stopOnce.Do(func() { close(f.stop) }) })
	return f
}

func (f *serverLifecycleFixture) serve() error {
	defer close(f.done)
	close(f.ready)
	<-f.stop
	return nil
}

func (f *serverLifecycleFixture) closeUSB() error {
	f.usbCloses.Add(1)
	f.order = append(f.order, "usb")
	f.stopOnce.Do(func() { close(f.stop) })
	return nil
}

func (f *serverLifecycleFixture) closeAPI() {
	f.apiCloses.Add(1)
	f.order = append(f.order, "api")
}

func waitServerLifecycleResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("The attempt-owned startup/shutdown did not finish")
		return nil
	}
}

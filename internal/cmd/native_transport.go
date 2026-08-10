package cmd

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/Alia5/VIIPER/viipertypes"
)

type nativeUDETransport interface {
	Done() <-chan error
	Close() error
	Status() (bool, *viipertypes.NativeUDEInfo)
}

// nativeUDETransportSession owns the lifetime boundary between the Go host
// and the kernel broker handle. Cancellation is session-owned rather than
// delegated to Host.Close so shutdown is safe even if it races the Serve
// goroutine's first instruction. The broker handle is closed only after every
// dequeue worker, endpoint lane, and input publisher has stopped using it.
type nativeUDETransportSession struct {
	cancel      context.CancelFunc
	closeClient func() error
	done        chan error
	ready       atomic.Bool
	info        viipertypes.NativeUDEInfo
	closeOnce   sync.Once
	closeErr    error
}

func (s *nativeUDETransportSession) Done() <-chan error { return s.done }

func (s *nativeUDETransportSession) Status() (bool, *viipertypes.NativeUDEInfo) {
	info := s.info
	return s.ready.Load(), &info
}

func (s *nativeUDETransportSession) Close() error {
	s.closeOnce.Do(func() {
		s.ready.Store(false)
		s.cancel()
		serveErr := <-s.done
		s.closeErr = errors.Join(serveErr, s.closeClient())
	})
	return s.closeErr
}

package cmd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeUDETransportCloseWaitsForHostBeforeClosingClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var hostStopped atomic.Bool
	var clientClosed atomic.Bool
	orderingErr := errors.New("kernel client closed before native host stopped")
	session := &nativeUDETransportSession{
		cancel: cancel,
		done:   done,
		closeClient: func() error {
			if !hostStopped.Load() {
				return orderingErr
			}
			clientClosed.Store(true)
			return nil
		},
	}
	session.ready.Store(true)
	go func() {
		<-ctx.Done()
		hostStopped.Store(true)
		done <- nil
		close(done)
	}()

	closed := make(chan error, 1)
	go func() { closed <- session.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("native transport shutdown did not complete")
	}
	if !clientClosed.Load() {
		t.Fatal("kernel client was not closed")
	}
	if err := session.Close(); err != nil {
		t.Fatalf("idempotent Close returned %v", err)
	}
}

func TestNativeUDETransportStatusIsSnapshot(t *testing.T) {
	session := &nativeUDETransportSession{}
	session.info.ABIMajor = 1
	session.info.ABIMinor = 9
	session.ready.Store(true)

	ready, first := session.Status()
	if !ready || first.ABIMajor != 1 || first.ABIMinor != 9 {
		t.Fatalf("unexpected native status: ready=%v info=%+v", ready, first)
	}
	first.ABIMinor = 99
	_, second := session.Status()
	if second.ABIMinor != 9 {
		t.Fatal("Status exposed mutable session state")
	}
}

func TestNativeUDETransportClosePreservesHostAndClientErrors(t *testing.T) {
	hostErr := errors.New("host failed")
	clientErr := errors.New("client close failed")
	done := make(chan error, 1)
	done <- hostErr
	close(done)
	session := &nativeUDETransportSession{
		cancel:      func() {},
		done:        done,
		closeClient: func() error { return clientErr },
	}
	err := session.Close()
	if !errors.Is(err, hostErr) || !errors.Is(err, clientErr) {
		t.Fatalf("Close error=%v, want joined host and client errors", err)
	}
}

package api

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/log"
	"github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/stretchr/testify/require"
)

func lifecycleServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	usbServer := usb.New(usb.ServerConfig{Addr: "127.0.0.1:0"}, logger, log.NewRaw(nil))
	t.Cleanup(func() { require.NoError(t, usbServer.Close()) })
	server := New(usbServer, "127.0.0.1:0", ServerConfig{}, logger)
	t.Cleanup(server.Close)
	return server
}

func TestAPICloseCancelsAcceptedManagementRequest(t *testing.T) {
	server := lifecycleServer(t)
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	server.Router().Register("waiting", func(req *Request, _ *Response, _ *slog.Logger) error {
		close(started)
		select {
		case <-req.Ctx.Done():
			close(cancelled)
		case <-release:
		}
		return nil
	})
	require.NoError(t, server.Start())
	client, err := net.Dial("tcp", server.Addr())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	_, err = client.Write([]byte("waiting\x00"))
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("synthetic management handler did not start")
	}
	server.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("API Close left an accepted request context alive")
	}
	assertLifecyclePeerClosed(t, client)
}

func assertLifecyclePeerClosed(t *testing.T, client net.Conn) {
	t.Helper()
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		// net.Pipe reports its peer's already-completed close here, whereas
		// TCP reports it on the subsequent read.
		require.ErrorIs(t, err, io.ErrClosedPipe)
		return
	}
	_, err := client.Read(make([]byte, 1))
	require.Error(t, err)
	if netError, ok := err.(net.Error); ok {
		require.False(t, netError.Timeout(), "listener shutdown must close already accepted connections")
	}
}

func TestAPICloseUnblocksAcceptedHandshakeAndCanRepeat(t *testing.T) {
	server := lifecycleServer(t)
	require.NoError(t, server.Start())
	client, err := net.Dial("tcp", server.Addr())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.Eventually(t, func() bool {
		server.lifecycleMu.Lock()
		defer server.lifecycleMu.Unlock()
		return len(server.connections) == 1
	}, time.Second, time.Millisecond)
	server.Close()
	server.Close()
	assertLifecyclePeerClosed(t, client)
	require.Eventually(t, func() bool {
		server.lifecycleMu.Lock()
		defer server.lifecycleMu.Unlock()
		return len(server.connections) == 0
	}, time.Second, time.Millisecond)
}

func TestAPIRejectsConnectionAcceptedAcrossCloseBoundary(t *testing.T) {
	server := lifecycleServer(t)
	accepted, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = accepted.Close() })
	listener := &lateAcceptedListener{
		conn: accepted, entered: make(chan struct{}), release: make(chan struct{}),
	}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(listener.release) }) })
	server.ln = listener
	done := make(chan struct{})
	go func() { defer close(done); server.serve(listener) }()
	<-listener.entered
	server.Close()
	release.Do(func() { close(listener.release) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("late accepted client reopened a closing server")
	}
	assertLifecyclePeerClosed(t, client)
	server.lifecycleMu.Lock()
	defer server.lifecycleMu.Unlock()
	require.Empty(t, server.connections)
}

type lateAcceptedListener struct {
	conn             net.Conn
	entered, release chan struct{}
	calls            atomic.Int32
}

func (l *lateAcceptedListener) Accept() (net.Conn, error) {
	if l.calls.Add(1) != 1 {
		return nil, net.ErrClosed
	}
	close(l.entered)
	<-l.release
	return l.conn, nil
}
func (*lateAcceptedListener) Close() error   { return nil }
func (*lateAcceptedListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func TestAPICloseBeforeStartDoesNotCreateListener(t *testing.T) {
	server := lifecycleServer(t)
	server.Close()
	require.ErrorIs(t, server.Start(), net.ErrClosed)
	require.Nil(t, server.ln)
}

func TestAPIRestartUsesFreshOwnerAndDoesNotCloseAnotherServer(t *testing.T) {
	foreign := lifecycleServer(t)
	require.NoError(t, foreign.Start())
	var address string
	for attempt := 0; attempt < 5; attempt++ {
		server := lifecycleServer(t)
		if address != "" {
			server.addr = address
		}
		require.NoError(t, server.Start())
		address = server.Addr()
		require.Error(t, server.Start(), "one owner cannot start a second listener")
		server.Close()
		peer, err := net.DialTimeout("tcp", foreign.Addr(), time.Second)
		require.NoError(t, err, "retiring this attempt touched an unrelated listener")
		_ = peer.Close()
	}
}

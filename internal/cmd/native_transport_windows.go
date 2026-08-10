//go:build windows

package cmd

import (
	"context"
	"sync"

	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
)

type windowsNativeUDETransport struct {
	host      *udecx.Host
	client    *udecx.Client
	done      chan error
	closeOnce sync.Once
	closeErr  error
}

func startNativeUDETransport(ctx context.Context, server *serverusb.Server) (nativeUDETransport, error) {
	client, err := udecx.Open(ctx)
	if err != nil {
		return nil, err
	}
	processor, err := serverusb.NewNativeProcessor(server)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	host, err := udecx.NewHost(client, processor, 0)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	if err := server.EnableNativeTransport(host); err != nil {
		_ = client.Close()
		return nil, err
	}
	session := &windowsNativeUDETransport{
		host: host, client: client, done: make(chan error, 1),
	}
	go func() {
		session.done <- host.Serve(ctx)
		close(session.done)
	}()
	return session, nil
}

func (s *windowsNativeUDETransport) Done() <-chan error { return s.done }

func (s *windowsNativeUDETransport) Close() error {
	s.closeOnce.Do(func() {
		s.host.Close()
		s.closeErr = s.client.Close()
	})
	return s.closeErr
}

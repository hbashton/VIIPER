//go:build windows

package cmd

import (
	"context"

	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
)

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
	sessionCtx, cancel := context.WithCancel(ctx)
	session := &nativeUDETransportSession{
		cancel: cancel, closeClient: client.Close, done: make(chan error, 1),
	}
	go func() {
		session.done <- host.Serve(sessionCtx)
		close(session.done)
	}()
	return session, nil
}

//go:build windows

package cmd

import (
	"context"

	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
	"github.com/Alia5/VIIPER/viipertypes"
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
	limits := client.Limits()
	session := &nativeUDETransportSession{
		cancel: cancel, closeClient: client.Close, done: make(chan error, 1),
		info: viipertypes.NativeUDEInfo{
			ABIMajor: udecx.ABIMajor, ABIMinor: udecx.ABIMinor,
			Capabilities:                 uint32(client.Capabilities()),
			ExpectedDriverPackageVersion: udecx.DriverPackageVersion,
			MaxDevices:                   limits.MaxDevices, MaxDescriptorBytes: limits.MaxDescriptorBytes,
			MaxTransferBytes: limits.MaxTransferBytes, MaxIsoPackets: limits.MaxIsoPackets,
			MaxPendingOperations: limits.MaxPendingOperations,
		},
	}
	session.ready.Store(true)
	go func() {
		err := host.Serve(sessionCtx)
		session.ready.Store(false)
		session.done <- err
		close(session.done)
	}()
	return session, nil
}

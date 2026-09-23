//go:build !windows

package cmd

import "context"

func requireUSBIPRuntime() error {
	return nil
}

func requireUSBIPRuntimeContext(ctx context.Context) error { return ctx.Err() }

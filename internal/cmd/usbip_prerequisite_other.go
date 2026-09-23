//go:build !windows

package cmd

import "context"

func requireUSBIPRuntimeContext(ctx context.Context) error { return ctx.Err() }

//go:build !windows

package cmd

func requireUSBIPRuntime() error {
	return nil
}

//go:build windows

package cmd

import (
	"errors"

	"golang.org/x/sys/windows"
)

// createNamedNativeMutex normalizes the Win32 CreateMutex contract. A named
// mutex that already exists is a successful open: Windows returns its valid
// handle and ERROR_ALREADY_EXISTS, and WaitForSingleObject decides ownership.
func createNamedNativeMutex(
	attributes *windows.SecurityAttributes,
	name *uint16,
) (windows.Handle, error) {
	handle, err := windows.CreateMutex(attributes, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return 0, err
	}
	if handle == 0 {
		if err != nil {
			return 0, err
		}
		return 0, windows.ERROR_INVALID_HANDLE
	}
	return handle, nil
}

//go:build windows && viiper_latency

package usb

import "fmt"

// InputLatencyImportGeneration returns the server-assigned lease of the
// currently active USB/IP import for one exact virtual device. The lease is a
// real transport-lifecycle generation: claimDeviceImport advances it for every
// successful import and removes it when that import releases ownership.
//
// This observer exists only in opt-in latency builds. It does not create,
// attach, detach, or otherwise alter an import, and normal release binaries do
// not contain it.
func (s *Server) InputLatencyImportGeneration(busID, deviceID uint32) (uint64, error) {
	if s == nil {
		return 0, fmt.Errorf("USB/IP latency lifecycle server is nil")
	}
	bus := s.GetBus(busID)
	if bus == nil {
		return 0, fmt.Errorf("USB/IP latency lifecycle bus %d is absent", busID)
	}
	dev, ok := bus.GetDeviceByID(deviceID)
	if !ok || dev == nil {
		return 0, fmt.Errorf(
			"USB/IP latency lifecycle device %d-%d is absent", busID, deviceID)
	}

	s.importsMu.Lock()
	generation := s.activeImports[dev]
	s.importsMu.Unlock()
	if generation == 0 {
		return 0, fmt.Errorf(
			"USB/IP latency lifecycle device %d-%d has no active import",
			busID, deviceID)
	}
	// Bus removal cancels the import connection before its lease cleanup is
	// necessarily observed. Re-check the exact identity so that cleanup lag
	// cannot be reported as a still-live association.
	current, stillRegistered := bus.GetDeviceByID(deviceID)
	if !stillRegistered || current != dev {
		return 0, fmt.Errorf(
			"USB/IP latency lifecycle device %d-%d changed during observation",
			busID, deviceID)
	}
	return generation, nil
}

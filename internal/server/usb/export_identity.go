package usb

import (
	"fmt"
	"time"

	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

const MaximumProductionRemovalTimeoutMilliseconds = uint32(300_000)

// ProductionRemovalTimeoutMilliseconds advertises the actual normal retained
// close budget, rounded upward. Reject unsupported/overflowing configurations
// before production creation; never clamp a larger owner budget to a lie.
func (s *Server) ProductionRemovalTimeoutMilliseconds() (uint32, error) {
	if s == nil {
		return 0, errRetainedImportInvalid
	}
	if s.config != nil && s.config.ConnectionTimeout < 0 {
		return 0, fmt.Errorf("invalid negative production lifecycle timeout")
	}
	lifecycle := s.retainedLifecycleTimeout()
	maximum := time.Duration(MaximumProductionRemovalTimeoutMilliseconds) * time.Millisecond
	if lifecycle <= 0 || lifecycle > maximum/3 {
		return 0, fmt.Errorf("production retained close timeout exceeds supported bound")
	}
	budget := 3 * lifecycle
	milliseconds := (budget + time.Millisecond - 1) / time.Millisecond
	if milliseconds <= 0 || milliseconds > time.Duration(MaximumProductionRemovalTimeoutMilliseconds) {
		return 0, fmt.Errorf("invalid production retained close timeout")
	}
	return uint32(milliseconds), nil
}

func (s *Server) lookupUSBIPImportRegistration(busID string) (virtualbus.DeviceMeta, error) {
	alias := usbip.ValidProductionXboxOneBusID(busID)
	var numericBus uint32
	if !alias {
		var err error
		numericBus, _, err = parseUSBIPBusAddress(busID)
		if err != nil {
			return virtualbus.DeviceMeta{}, err
		}
	}
	s.busesMu.Lock()
	defer s.busesMu.Unlock()
	var selected virtualbus.DeviceMeta
	if alias {
		for _, bus := range s.busses {
			if candidate, found := bus.GetDeviceImportSnapshotByBusID(busID); found {
				if selected.Bus != nil {
					return virtualbus.DeviceMeta{}, fmt.Errorf("ambiguous USB/IP export alias")
				}
				selected = candidate
			}
		}
	} else if bus := s.busses[numericBus]; bus != nil {
		selected, _ = bus.GetDeviceImportSnapshotByBusID(busID)
	}
	if selected.Bus == nil || selected.Dev == nil || selected.Context == nil {
		return virtualbus.DeviceMeta{}, fmt.Errorf("no device matches busid %s", busID)
	}
	return selected, nil
}

package xboxone

import "github.com/Alia5/VIIPER/internal/retainedusb"

// RetainedImportRetirement asks the existing retained transport to stop this
// exact incarnation without discarding the feedback consumer needed by its
// terminal neutral. It is not a successful drain, Stop ACK, or recovery proof.
// The transport still authenticates the lease, retires every ticket, joins the
// executor and requires DisconnectNeutral Safe before releasing registration.
func (adapter *DormantRetainedUSBAdapter) RetainedImportRetirement() (retainedusb.ImportRetirementRequest, error) {
	if adapter == nil {
		return retainedusb.ImportRetirementRequest{}, errDormantRetainedUSBUninitialized
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.state == dormantRetainedUSBQuarantine {
		return retainedusb.ImportRetirementRequest{}, adapter.quarantineErrorLocked()
	}
	if adapter.fatalLocalError != nil {
		return retainedusb.ImportRetirementRequest{}, adapter.fatalLocalError
	}
	if !adapter.inputHistoryFault {
		return retainedusb.ImportRetirementRequest{}, nil
	}
	if !adapter.boundLease.Valid() || adapter.state != dormantRetainedUSBBound {
		return retainedusb.ImportRetirementRequest{}, errDormantRetainedUSBInvalidImport
	}
	return retainedusb.ImportRetirementRequest{
		Lease: adapter.boundLease, Reason: retainedusb.ImportRetirementInputHistoryOverflow,
	}, nil
}

var _ retainedusb.ImportRetirementOwner = (*DormantRetainedUSBAdapter)(nil)

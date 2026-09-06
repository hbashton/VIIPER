package usb

import (
	"fmt"

	"github.com/Alia5/VIIPER/internal/retainedusb"
	"github.com/Alia5/VIIPER/usb"
)

// A bounded cold-path tombstone set. Exhaustion rejects registration rather
// than evicting an identity whose Windows device removal is unproven. A broker
// restart starts a new set, not a claim that OS-level identities are gone.
const maxPrimaryGIPIdentityReservations = 65536

func retainedPrimaryGIPIdentity(dev usb.Device, required bool) (id uint64, err error) {
	identity, present := dev.(retainedusb.PrimaryGIPIdentityDevice)
	if !present {
		if required {
			return 0, fmt.Errorf("%w: production Xbox registration requires primary GIP identity", errRetainedImportInvalid)
		}
		return 0, nil
	}
	defer func() {
		if recover() != nil {
			id = 0
			err = fmt.Errorf("%w: primary GIP identity callback failed", errRetainedImportInvalid)
		}
	}()
	id = identity.RetainedUSBPrimaryGIPDeviceID()
	if id&0xffffffff00000000 != 0x0000fffb00000000 {
		return 0, fmt.Errorf("%w: invalid primary GIP identity", errRetainedImportInvalid)
	}
	return id, nil
}

func (s *Server) validateUnusedPrimaryGIPIdentityLocked(id uint64) error {
	if _, used := s.usedPrimaryGIPDeviceIDs[id]; used {
		return fmt.Errorf("%w: primary GIP identity was already registered; allocate a fresh Hello DeviceID and matching USB serial", errRetainedImportInvalid)
	}
	if len(s.usedPrimaryGIPDeviceIDs) >= maxPrimaryGIPIdentityReservations {
		return fmt.Errorf("%w: primary GIP identity reservation capacity exhausted", errRetainedImportInvalid)
	}
	return nil
}

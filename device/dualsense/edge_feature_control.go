package dualsense

import (
	"encoding/binary"
	"errors"
	"sync"

	"github.com/Alia5/VIIPER/usb"
)

var _ usb.TransactionalControlDevice = (*DualSense)(nil)

var errEdgeFeatureClaim = errors.New("invalid or exhausted Edge feature transaction")

// Edge profile reports are declared for descriptor fidelity, but there is no
// virtual onboard-profile store or configuration-preview implementation.
// Use a real USB STALL, not HandleControl's
// unhandled result (which can fall through to a successful generic HID reply).
// This fixed-size bookkeeping is independent of input, audio and output queues.
type edgeFeatureControlTransactions struct {
	mu    sync.Mutex
	next  uint64
	slots [32]edgeFeatureControlSlot
}

type edgeFeatureControlSlot struct {
	claim    usb.ControlTransactionClaim
	admitted bool
}

func (d *DualSense) ClaimControlTransaction(request usb.ControlTransactionRequest) (usb.ControlTransactionClaim, error) {
	s := request.Setup
	if !d.input.edge {
		return usb.ControlTransactionClaim{}, nil
	}
	unsupportedProfile := s[3] == reportTypeFeature && isEdgeFeatureReport(s[2]) &&
		((s[0] == hidClassIN && s[1] == hidGetReport) || (s[0] == hidClassOUT && s[1] == hidSetReport))
	unsupportedConfiguration := s[0] == hidClassOUT && s[1] == hidSetReport &&
		s[3] == reportTypeOutput && s[2] == ReportIDOutput && d.rejectsEdgeConfigurationOutput(request.Data)
	if !unsupportedProfile && !unsupportedConfiguration {
		return usb.ControlTransactionClaim{}, nil
	}
	index := binary.LittleEndian.Uint16(s[4:6])
	if index > 255 {
		return usb.ControlTransactionClaim{}, nil
	}
	iface, exists := d.descriptor.Interface(uint8(index))
	if !exists || iface.HID == nil {
		return usb.ControlTransactionClaim{}, nil
	}
	// Both valid requests and malformed envelopes for this unsupported feature
	// fail closed. No payload is retained or forwarded to physical hardware.
	c := &d.edgeFeatureControl
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.next == ^uint64(0) {
		return usb.ControlTransactionClaim{}, errEdgeFeatureClaim
	}
	for i := range c.slots {
		if c.slots[i].claim.Token == 0 {
			c.next++
			claim := usb.ControlTransactionClaim{Token: c.next, Generation: 1, Result: usb.ControlTransactionStall}
			c.slots[i] = edgeFeatureControlSlot{claim: claim}
			return claim, nil
		}
	}
	return usb.ControlTransactionClaim{}, errEdgeFeatureClaim
}

// Configuration previews and Edge extension controls are not the common native
// effect prefix. The tester enables previews with USB byte 39 bit 7 and stores
// curve/deadzone parameters from byte 50; Titania additionally enables its Edge
// extension with USB byte 41 bit 7. Truncating either to the V5 48-byte prefix
// could forward the authorization with missing parameters to physical hardware.
// Reject the whole command, including bundled effects, before admission or any
// persistent media state mutation. Ordinary DualSense high bits are unaffected.
// References: daidr/dualsense-tester f6e6247, JoystickSensitivity.vue /
// TriggerDeadZone.vue; neptuwunium/titania 9904458, structures.h / hid.c.
func hasEdgeConfigurationOutput(report []byte) bool {
	return len(report) > 39 && report[39]&0x80 != 0 ||
		len(report) > 41 && report[41]&0x80 != 0
}

func (d *DualSense) rejectsEdgeConfigurationOutput(out []byte) bool {
	if !d.input.edge {
		return false
	}
	var normalized [OutputReportSize]byte
	report, ok := normalizeOutputReportInto(out, &normalized)
	return ok && hasEdgeConfigurationOutput(report)
}

func (d *DualSense) AdmitControlTransaction(claim usb.ControlTransactionClaim, destination []byte) error {
	c := &d.edgeFeatureControl
	c.mu.Lock()
	defer c.mu.Unlock()
	if !claim.Handled() || len(destination) != 0 {
		return errEdgeFeatureClaim
	}
	for i := range c.slots {
		if c.slots[i].claim == claim && !c.slots[i].admitted {
			c.slots[i].admitted = true
			return nil
		}
	}
	return errEdgeFeatureClaim
}

func (d *DualSense) CompleteControlTransaction(claim usb.ControlTransactionClaim, outcome usb.ControlTransactionOutcome) error {
	c := &d.edgeFeatureControl
	c.mu.Lock()
	defer c.mu.Unlock()
	if !claim.Handled() || (outcome != usb.ControlTransactionDelivered &&
		outcome != usb.ControlTransactionDeliveryFailed && outcome != usb.ControlTransactionCancelled) {
		return errEdgeFeatureClaim
	}
	for i := range c.slots {
		slot := &c.slots[i]
		if slot.claim == claim {
			if (outcome == usb.ControlTransactionCancelled) == slot.admitted {
				return errEdgeFeatureClaim
			}
			*slot = edgeFeatureControlSlot{}
			return nil
		}
	}
	return errEdgeFeatureClaim
}

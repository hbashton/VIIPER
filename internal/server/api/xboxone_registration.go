package api

import (
	"strconv"

	"github.com/Alia5/VIIPER/device/xboxone"
	apierror "github.com/Alia5/VIIPER/internal/server/api/error"
	"github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/virtualbus"
)

// XboxOneRegistrationAdmission holds only the captured creation incarnation.
// The token stays private and is never formatted into diagnostics or routes.
type XboxOneRegistrationAdmission struct {
	registration virtualbus.DeviceMeta
	device       *xboxone.AuthorizedDormantRetainedUSBDevice
	token        string
}

func SelectAuthorizedXboxOneRegistration(server *usb.Server, busText, deviceText, payload string) (*XboxOneRegistrationAdmission, error) {
	token, err := xboxone.DecodeProductionRegistrationRequest(payload)
	if err != nil {
		return nil, apierror.ErrBadRequest("invalid Xbox One registration payload")
	}
	busID, busErr := strconv.ParseUint(busText, 10, 16)
	deviceID, deviceErr := strconv.ParseUint(deviceText, 10, 16)
	if busErr != nil || deviceErr != nil || busID == 0 || deviceID == 0 ||
		strconv.FormatUint(busID, 10) != busText || strconv.FormatUint(deviceID, 10) != deviceText {
		return nil, apierror.ErrBadRequest("invalid Xbox One registration address")
	}
	if server == nil {
		return nil, apierror.ErrConflict("Xbox One registration is unavailable")
	}
	bus := server.GetBus(uint32(busID))
	if bus != nil {
		for _, registration := range bus.GetAllDeviceMetas() {
			if registration.Meta.DevID != uint32(deviceID) {
				continue
			}
			device, ok := registration.Dev.(*xboxone.AuthorizedDormantRetainedUSBDevice)
			if !ok {
				break
			}
			admission := &XboxOneRegistrationAdmission{registration: registration, device: device, token: token}
			if err := admission.Run(func() error { return nil }); err != nil {
				return nil, err
			}
			return admission, nil
		}
	}
	return nil, apierror.ErrConflict("Xbox One registration is unavailable")
}

func (admission *XboxOneRegistrationAdmission) Registration() virtualbus.DeviceMeta {
	return admission.registration
}

func (admission *XboxOneRegistrationAdmission) Device() *xboxone.AuthorizedDormantRetainedUSBDevice {
	return admission.device
}

// Run is a short synchronous admission boundary, never a network-I/O lease.
// Removal may wait for it, but it cannot resume against a reused address.
func (admission *XboxOneRegistrationAdmission) Run(operation func() error) error {
	if admission == nil || operation == nil || admission.registration.Bus == nil {
		return apierror.ErrConflict("Xbox One registration is unavailable")
	}
	lease, acquired := admission.registration.Bus.AcquireRegistrationOperation(admission.registration)
	if !acquired {
		return apierror.ErrConflict("Xbox One registration is unavailable")
	}
	defer lease.Release()
	if !admission.device.AuthorizesProductionRemoval(admission.registration.Context.Done(), admission.token) {
		return apierror.ErrConflict("Xbox One registration is unavailable")
	}
	return operation()
}

// claimStream releases the short registration fence BEFORE entering the
// stream coordinator. Existing coordinator cleanup callbacks may hold their
// mutex while removing this registration, so the inverse lock order deadlocks.
// The claim's exact-incarnation key cannot displace a reused-address successor;
// broker acquisition and ConsumerReady revalidate the captured admission.
func (admission *XboxOneRegistrationAdmission) claimStream(claim func() *deviceStreamLease) (*deviceStreamLease, error) {
	if err := admission.Run(func() error { return nil }); err != nil {
		return nil, err
	}
	lease := claim()
	if lease == nil {
		return nil, apierror.ErrConflict("Xbox One registration already has its stream owner")
	}
	return lease, nil
}

type xboxOneRegistrationConn struct {
	*bufferedReadConn
	admission *XboxOneRegistrationAdmission
}

func (conn *xboxOneRegistrationConn) VIIPERAuthorizeXboxOneRegistration(device *xboxone.AuthorizedDormantRetainedUSBDevice, operation func() error) error {
	if conn.admission == nil || conn.admission.device != device {
		return xboxone.ErrProductionBrokerUnavailable
	}
	return conn.admission.Run(operation)
}

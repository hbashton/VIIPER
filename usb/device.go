package usb

import "context"

// Device is the minimal interface a device must implement.
// It only handles non-EP0 (interrupt/bulk) transfers.
type Device interface {
	// HandleTransfer processes a non-EP0 transfer (interrupt/bulk).
	// ep is the endpoint number (without direction). dir is usbip.DirIn or usbip.DirOut.
	// For IN transfers the implementation should block until data is available or ctx is
	// cancelled, then return the payload. For OUT transfers, consume 'out' and return nil.
	HandleTransfer(ctx context.Context, ep uint32, dir uint32, out []byte) []byte
	GetDescriptor() *Descriptor
	GetDeviceSpecificArgs() map[string]any
}

// InterruptInputDevice is an optional allocation-free interrupt-IN contract.
// Native transports may keep one endpoint-sized buffer and ask the device to
// encode directly into it instead of allocating a new report for every input
// sample. Implementations must block until input is available or ctx is
// cancelled, must not retain dst, and must be safe when different endpoints
// are read concurrently. Native transports impose the endpoint's USB service
// interval as a deadline. Stateful controllers may encode their cached state
// when that deadline expires; event-only devices may return DeadlineExceeded
// and keep waiting. A successful call returns the number of bytes written to
// dst; zero-length successful reports are invalid.
//
// HandleTransfer remains the compatibility contract for USB/IP and for devices
// which do not implement this interface.
type InterruptInputDevice interface {
	ReadInterruptInput(ctx context.Context, ep uint32, dst []byte) (int, error)
}

// IsochronousInputDevice is the corresponding optional caller-buffer contract
// for isochronous IN packets. The transport supplies exactly the packet region
// owned by the current URB. The native scheduler invokes this at the packet's
// service time, so implementations must not wait for source data: they return
// a legal zero packet when capture has not arrived. Implementations may return
// a shorter legal packet, must not retain dst, and must honor cancellation.
type IsochronousInputDevice interface {
	ReadIsochronousInput(ctx context.Context, ep uint32, dst []byte) (int, error)
}

// ControlDevice is an optional interface for devices that need to handle
// control transfers on endpoint 0 (EP0).
//
// This is primarily used for class-specific requests that are not covered by
// the server's built-in standard request handling (e.g. HID GET_REPORT/
// SET_REPORT).
type ControlDevice interface {
	// HandleControl handles a control request.
	//
	// - bmRequestType, bRequest, wValue, wIndex, wLength are the raw setup packet fields.
	// - data is the OUT data stage payload (for host-to-device requests), and is nil for
	//   device-to-host requests.
	//
	// If handled is false, the server will fall back to its default behavior.
	// If handled is true, the returned bytes (if any) will be used as the IN data stage.
	HandleControl(bmRequestType, bRequest uint8, wValue, wIndex, wLength uint16, data []byte) (resp []byte, handled bool)
}

// InterfaceAltSettingDevice is an optional interface for devices that need to
// react when the host opens or closes alternate USB interfaces.
type InterfaceAltSettingDevice interface {
	SetInterfaceAltSetting(iface, alt uint8)
}

// EndpointResetDevice is notified after the host clears the halt feature on a
// known endpoint. Windows uses this standard request as part of pipe reset and
// audio stream teardown even for virtual isochronous endpoints.
type EndpointResetDevice interface {
	ResetEndpoint(endpointAddress uint8)
}

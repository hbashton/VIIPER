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

// ControlTransactionDirection identifies the USB/IP envelope direction of an
// endpoint-zero submission independently of bmRequestType. A transactional
// device must validate that the two agree for the request it claims.
type ControlTransactionDirection uint8

const (
	ControlTransactionHostToDevice ControlTransactionDirection = iota
	ControlTransactionDeviceToHost
)

// ControlTransactionRequest is the complete immutable selection input for one
// endpoint-zero submission. Data aliases the server's receive scratch and is
// read-only and valid only for the duration of ClaimControlTransaction. It is
// empty for a device-to-host request. TransferLength is the USB/IP envelope
// length, not merely wLength from Setup.
type ControlTransactionRequest struct {
	Setup          [8]byte
	Direction      ControlTransactionDirection
	TransferLength uint32
	Data           []byte
}

// ControlTransactionResult is the exact wire disposition selected by a
// transactional endpoint-zero owner.
type ControlTransactionResult uint8

const (
	// ControlTransactionUnhandled acquires no claim and preserves the server's
	// existing generic and legacy ControlDevice handling byte-for-byte.
	ControlTransactionUnhandled ControlTransactionResult = iota
	// ControlTransactionData returns ResponseLength bytes on a successful IN
	// transfer. The bytes are copied only by final admission.
	ControlTransactionData
	// ControlTransactionNoData successfully completes without response data.
	// For an OUT transfer the request data stage remains acknowledged normally.
	ControlTransactionNoData
	// ControlTransactionStall completes the request with an endpoint-zero STALL.
	ControlTransactionStall
)

// ControlTransactionClaim is one device-owned immutable endpoint-zero
// transaction. Token is unique within Generation. A device must reject a
// forged, stale, duplicate, or already-terminal claim in both
// admission and completion. Unhandled is represented only by the all-zero
// claim. ResponseLength is meaningful only for ControlTransactionData and
// must fit both the USB/IP TransferLength and the setup packet's wLength.
type ControlTransactionClaim struct {
	Token          uint64
	Generation     uint64
	Result         ControlTransactionResult
	ResponseLength uint32
}

// Valid reports whether the claim has a structurally exact result shape. The
// owning device remains responsible for capability validation.
func (claim ControlTransactionClaim) Valid() bool {
	switch claim.Result {
	case ControlTransactionUnhandled:
		return claim.Token == 0 && claim.Generation == 0 &&
			claim.ResponseLength == 0
	case ControlTransactionData:
		return claim.Token != 0 && claim.Generation != 0 &&
			claim.ResponseLength != 0
	case ControlTransactionNoData, ControlTransactionStall:
		return claim.Token != 0 && claim.Generation != 0 &&
			claim.ResponseLength == 0
	default:
		return false
	}
}

// Handled reports whether a structurally valid claim owns the request.
func (claim ControlTransactionClaim) Handled() bool {
	return claim.Valid() && claim.Result != ControlTransactionUnhandled
}

// ControlTransactionOutcome is the exactly-once terminal disposition of one
// handled endpoint-zero claim.
type ControlTransactionOutcome uint8

const (
	// ControlTransactionDelivered means the complete RET_SUBMIT, including any
	// required flush, was accepted by the connection writer.
	ControlTransactionDelivered ControlTransactionOutcome = iota + 1
	// ControlTransactionDeliveryFailed means admission succeeded but response
	// delivery or its required flush failed.
	ControlTransactionDeliveryFailed
	// ControlTransactionCancelled means final admission rejected the claim and
	// no response byte or device effect may become visible later.
	ControlTransactionCancelled
)

// TransactionalControlDevice optionally owns complete endpoint-zero requests
// before the server's generic descriptor/configuration/HID handling. Existing
// devices do not implement this interface and retain their current behavior.
//
// ClaimControlTransaction must be bounded and nonblocking. It must copy any
// request data it needs to retain after the method returns, acquire no claim
// for Unhandled, and make no externally visible state change. A returned error
// must likewise own no claim.
//
// AdmitControlTransaction runs under serialized response-stream ownership
// immediately before the first response byte can be written. Destination is a
// server-owned exact-length/exact-capacity view which is valid only during the
// call and must not be retained, read, or written after return. The method must
// validate the exact claim and destination length, copy a Data result into
// destination, and make no externally visible effect. A failure is
// synchronously followed by one Cancelled completion and no write.
//
// CompleteControlTransaction is invoked exactly once for every structurally
// valid handled claim returned to the server. Delivered is the only outcome
// which may commit a request effect. DeliveryFailed and Cancelled must retire
// it with no effect that can become visible later. Completion must be bounded,
// nonblocking, and designed not to fail for a claim which admission accepted;
// an error is a fatal device/transport contract violation.
type TransactionalControlDevice interface {
	ClaimControlTransaction(request ControlTransactionRequest) (ControlTransactionClaim, error)
	AdmitControlTransaction(claim ControlTransactionClaim, destination []byte) error
	CompleteControlTransaction(claim ControlTransactionClaim, outcome ControlTransactionOutcome) error
}

// InterruptOutTransactionRequest is the complete immutable selection input
// for one non-isochronous interrupt OUT submission. Data aliases the server's
// receive scratch, is read-only, has exact length/capacity, and is valid only
// for the duration of ClaimInterruptOutTransaction. Endpoint is the endpoint
// number without its direction bit. The server has already verified that the
// endpoint is active, OUT-directed, and interrupt-typed for the selected USB
// configuration and alternate setting.
type InterruptOutTransactionRequest struct {
	Endpoint uint8
	Data     []byte
}

// InterruptOutTransactionResult is the exact USB/IP disposition selected by a
// transactional interrupt-OUT owner.
type InterruptOutTransactionResult uint8

const (
	// InterruptOutTransactionUnhandled acquires no claim and preserves the
	// device's existing HandleTransfer behavior byte-for-byte.
	InterruptOutTransactionUnhandled InterruptOutTransactionResult = iota
	// InterruptOutTransactionAccepted acknowledges the complete OUT payload.
	// The command effect may commit only after the acknowledgement is delivered.
	InterruptOutTransactionAccepted
	// InterruptOutTransactionStall completes the submission with a pipe error
	// and never commits a command effect.
	InterruptOutTransactionStall
)

// InterruptOutTransactionClaim is one device-owned immutable interrupt OUT
// transaction. Token is unique within Generation. The all-zero value is the
// only valid Unhandled claim. A device must reject a forged, stale, duplicate,
// or already-terminal claim during both admission and completion.
type InterruptOutTransactionClaim struct {
	Token      uint64
	Generation uint64
	Result     InterruptOutTransactionResult
}

// Valid reports whether the claim has a structurally exact result shape. The
// owning device remains responsible for capability validation.
func (claim InterruptOutTransactionClaim) Valid() bool {
	switch claim.Result {
	case InterruptOutTransactionUnhandled:
		return claim.Token == 0 && claim.Generation == 0
	case InterruptOutTransactionAccepted, InterruptOutTransactionStall:
		return claim.Token != 0 && claim.Generation != 0
	default:
		return false
	}
}

// Handled reports whether a structurally valid claim owns the submission.
func (claim InterruptOutTransactionClaim) Handled() bool {
	return claim.Valid() && claim.Result != InterruptOutTransactionUnhandled
}

// InterruptOutTransactionOutcome is the exactly-once terminal disposition of
// one handled interrupt OUT claim.
type InterruptOutTransactionOutcome uint8

const (
	// InterruptOutTransactionDelivered means the complete RET_SUBMIT, including
	// its mandatory interrupt flush, was accepted by the connection writer.
	InterruptOutTransactionDelivered InterruptOutTransactionOutcome = iota + 1
	// InterruptOutTransactionDeliveryFailed means admission succeeded but the
	// RET_SUBMIT or its mandatory flush failed.
	InterruptOutTransactionDeliveryFailed
	// InterruptOutTransactionCancelled means final admission rejected the claim
	// and no response byte or command effect may become visible later.
	InterruptOutTransactionCancelled
)

// TransactionalInterruptOutDevice optionally owns complete non-isochronous
// interrupt OUT submissions before HandleTransfer. Existing devices do not
// implement this interface and retain their current behavior.
//
// ClaimInterruptOutTransaction must be bounded and nonblocking. It must copy
// any request data it needs to retain after the method returns, acquire no
// claim for Unhandled, and make no externally visible state change. A returned
// error must likewise own no claim.
//
// AdmitInterruptOutTransaction runs under serialized response-stream ownership
// immediately before the acknowledgement can emit its first byte. It must
// validate the exact claim and make no externally visible effect. A failure is
// synchronously followed by one Cancelled completion and no write.
//
// CompleteInterruptOutTransaction is invoked exactly once for every
// structurally valid handled claim returned to the server. Delivered is the
// only outcome which may commit an Accepted command effect; a Stall must never
// commit one. DeliveryFailed and Cancelled must retire the claim without an
// effect that can become visible later. Completion runs while the response
// stream is serialized, must be fixed-cost and nonblocking, and must not do
// I/O. It should publish work to a separate device-owned worker when physical
// output is required. An error after successful admission is a fatal contract
// violation.
type TransactionalInterruptOutDevice interface {
	ClaimInterruptOutTransaction(request InterruptOutTransactionRequest) (InterruptOutTransactionClaim, error)
	AdmitInterruptOutTransaction(claim InterruptOutTransactionClaim) error
	CompleteInterruptOutTransaction(claim InterruptOutTransactionClaim, outcome InterruptOutTransactionOutcome) error
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

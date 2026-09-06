package registry

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/device/xboxone"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/virtualbus"
)

var (
	// ErrAuthorizedXboxOneRegistration reports failure at the explicit,
	// non-generic retained Xbox factory boundary.
	ErrAuthorizedXboxOneRegistration = errors.New(
		"registry: authorized Xbox One retained USB registration failed")
	// ErrAuthorizedXboxOneRegistrationInactive reports a stale, removed, or
	// explicitly closed broker handle.
	ErrAuthorizedXboxOneRegistrationInactive = errors.New(
		"registry: authorized Xbox One retained USB registration is inactive")
)

// AuthorizedXboxOneRetainedUSBRequest contains only already-established,
// typed authority. It is deliberately not a JSON/API DTO: the one-shot
// authorization and exact local feedback executor cannot be reconstructed
// from an untrusted generic deviceSpecific payload.
type AuthorizedXboxOneRetainedUSBRequest struct {
	BusID                    uint32
	AuthorityID              uint64
	DeviceID                 uint64
	ProtocolTimeMilliseconds uint64
	Authorization            xboxone.AuthorizedControllerPersonaConfig
	LocalExecutor            xboxone.ControllerPersonaLocalExecutor
	LocalTimeout             time.Duration
}

// ProductionXboxOneRetainedUSBRequest is the authenticated broker factory
// request. Unlike DeviceCreateRequest it contains the closed, typed identity,
// metadata-profile, feedback-generation, and retained-authority facts required
// to compose the production persona.
type ProductionXboxOneRetainedUSBRequest struct {
	BusID   uint32
	Options xboxone.ProductionRetainedUSBDeviceOptions
}

// AuthorizedXboxOneRetainedUSBRegistration is the broker's narrow ownership
// handle. It exposes semantic input publication and exact removal, but not the
// raw USB device or retained owner. There is therefore no second input,
// feedback, lifecycle, or mapping path around the canonical Xbox engine.
type AuthorizedXboxOneRetainedUSBRegistration struct {
	server       *serverusb.Server
	device       *xboxone.AuthorizedDormantRetainedUSBDevice
	registration virtualbus.DeviceMeta
	state        *authorizedXboxOneRetainedUSBRegistrationState
	removalToken string // create response only; never generic metadata
}

// authorizedXboxOneRetainedUSBRegistrationState is shared by every ordinary
// value copy of the exported handle. Copying a handle before first use cannot
// create a second close owner or a second publication lane.
type authorizedXboxOneRetainedUSBRegistrationState struct {
	mu        sync.Mutex
	closed    bool
	closeDone chan struct{}
	closeErr  error
	// beforeSemanticPublish is nil in production and exists only so package
	// tests can hold an acquired exact-registration operation deterministically.
	beforeSemanticPublish func()
}

type authorizedXboxOneConstruction struct {
	busID                    uint32
	authorityID              uint64
	deviceID                 uint64
	protocolTimeMilliseconds uint64
	authorization            xboxone.AuthorizedControllerPersonaConfig
	localExecutor            xboxone.ControllerPersonaLocalExecutor
	localTimeout             time.Duration
	afterConstruction        func(*xboxone.AuthorizedDormantRetainedUSBDevice) error
	productionExportAlias    bool
}

// RegisterAuthorizedXboxOneRetainedUSB consumes one explicit Xbox identity
// authorization only after the server's default-off retained authority and
// target bus, bounded address capacity, and registration-token capacity have
// passed a non-consuming preflight. The server repeats those checks atomically
// with registration. If topology changes in between, the one-shot
// authorization remains safely consumed but the constructed device has
// started no goroutine and performed no I/O.
func RegisterAuthorizedXboxOneRetainedUSB(
	server *serverusb.Server,
	request AuthorizedXboxOneRetainedUSBRequest,
) (*AuthorizedXboxOneRetainedUSBRegistration, error) {
	if server == nil {
		return nil, ErrAuthorizedXboxOneRegistration
	}
	if err := server.ValidateRetainedDeviceRegistrationTarget(
		request.BusID, request.AuthorityID); err != nil {
		return nil, fmt.Errorf("%w: preflight: %w",
			ErrAuthorizedXboxOneRegistration, err)
	}

	return registerPreparedAuthorizedXboxOneRetainedUSB(server,
		authorizedXboxOneConstruction{
			busID: request.BusID, authorityID: request.AuthorityID,
			deviceID:                 request.DeviceID,
			protocolTimeMilliseconds: request.ProtocolTimeMilliseconds,
			authorization:            request.Authorization,
			localExecutor:            request.LocalExecutor,
			localTimeout:             request.LocalTimeout,
		})
}

// RegisterProductionXboxOneRetainedUSB is the sole production factory for the
// full-duplex Xbox persona. It keeps generic deviceSpecific JSON incapable of
// reconstructing either the one-shot external identity authorization or the
// exact canonical feedback executor.
func RegisterProductionXboxOneRetainedUSB(
	server *serverusb.Server,
	request ProductionXboxOneRetainedUSBRequest,
) (*AuthorizedXboxOneRetainedUSBRegistration, error) {
	if server == nil {
		return nil, ErrAuthorizedXboxOneRegistration
	}
	if _, err := server.ProductionRemovalTimeoutMilliseconds(); err != nil {
		return nil, fmt.Errorf("%w: lifecycle budget: %w", ErrAuthorizedXboxOneRegistration, err)
	}
	if err := server.ValidateRetainedDeviceRegistrationTarget(
		request.BusID, request.Options.AuthorityID); err != nil {
		return nil, fmt.Errorf("%w: preflight: %w",
			ErrAuthorizedXboxOneRegistration, err)
	}
	preparation, err := xboxone.PrepareProductionRetainedUSBDevice(request.Options)
	if err != nil {
		return nil, fmt.Errorf("%w: prepare: %w",
			ErrAuthorizedXboxOneRegistration, err)
	}
	authorization, protocolTime, authorityID, deviceID, local, timeout, ok :=
		preparation.AuthorizedConstructionInputs()
	if !ok {
		return nil, fmt.Errorf("%w: invalid production preparation",
			ErrAuthorizedXboxOneRegistration)
	}
	registration, err := registerPreparedAuthorizedXboxOneRetainedUSB(server,
		authorizedXboxOneConstruction{
			busID: request.BusID, authorityID: authorityID,
			deviceID: deviceID, protocolTimeMilliseconds: protocolTime,
			authorization: authorization, localExecutor: local,
			localTimeout:          timeout,
			afterConstruction:     preparation.AttachConstructed,
			productionExportAlias: true,
		})
	if err != nil {
		return nil, err
	}
	token, err := registration.device.BindProductionRemovalRegistration(
		registration.registration.Context.Done())
	if err != nil {
		return nil, errors.Join(err, registration.Close())
	}
	registration.removalToken = token
	return registration, nil
}

// ProductionRemovalToken returns the creation-only secret for this exact
// production registration. Generic authorized test/dev registrations have no
// such capability. The value is not suitable for logs or discovery metadata.
func (registration *AuthorizedXboxOneRetainedUSBRegistration) ProductionRemovalToken() (string, bool) {
	if registration == nil || registration.state == nil {
		return "", false
	}
	registration.state.mu.Lock()
	defer registration.state.mu.Unlock()
	if registration.state.closed || !xboxone.ValidProductionRemovalToken(registration.removalToken) {
		return "", false
	}
	return registration.removalToken, true
}

func registerPreparedAuthorizedXboxOneRetainedUSB(
	server *serverusb.Server,
	construction authorizedXboxOneConstruction,
) (*AuthorizedXboxOneRetainedUSBRegistration, error) {
	device, err := xboxone.NewAuthorizedDormantRetainedUSBDevice(
		construction.authorization,
		construction.protocolTimeMilliseconds,
		construction.authorityID,
		construction.deviceID,
		construction.localExecutor,
		construction.localTimeout,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: construct: %w",
			ErrAuthorizedXboxOneRegistration, err)
	}
	if construction.afterConstruction != nil {
		if err := construction.afterConstruction(device); err != nil {
			return nil, fmt.Errorf("%w: bind production broker: %w",
				ErrAuthorizedXboxOneRegistration, err)
		}
	}
	var registration virtualbus.DeviceMeta
	if construction.productionExportAlias {
		registration, err = server.AddProductionXboxOneRetainedDeviceRegistration(construction.busID, construction.authorityID, device)
	} else {
		registration, err = server.AddRetainedDeviceRegistration(construction.busID, construction.authorityID, device)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: register: %w",
			ErrAuthorizedXboxOneRegistration, err)
	}
	return newAuthorizedXboxOneRetainedUSBRegistration(
		server, device, registration), nil
}

func newAuthorizedXboxOneRetainedUSBRegistration(
	server *serverusb.Server,
	device *xboxone.AuthorizedDormantRetainedUSBDevice,
	registration virtualbus.DeviceMeta,
) *AuthorizedXboxOneRetainedUSBRegistration {
	return &AuthorizedXboxOneRetainedUSBRegistration{
		server: server, device: device, registration: registration,
		state: &authorizedXboxOneRetainedUSBRegistrationState{
			closeDone: make(chan struct{}),
		},
	}
}

// BusID returns the immutable USB/IP bus component assigned at registration.
func (registration *AuthorizedXboxOneRetainedUSBRegistration) BusID() uint32 {
	if registration == nil {
		return 0
	}
	return registration.registration.Meta.BusID
}

// DeviceAddress returns the immutable USB/IP device-address component. This is
// distinct from the Xbox GIP DeviceID carried by the one-shot authorization.
func (registration *AuthorizedXboxOneRetainedUSBRegistration) DeviceAddress() uint32 {
	if registration == nil {
		return 0
	}
	return registration.registration.Meta.DevID
}

// DeviceMeta returns the exact immutable Add incarnation for API cleanup and
// descriptor snapshotting. Callers receive a value copy; registration-token
// checks still fence every server mutation against a successor device.
func (registration *AuthorizedXboxOneRetainedUSBRegistration) DeviceMeta() (
	virtualbus.DeviceMeta,
	bool,
) {
	if registration == nil || registration.state == nil ||
		registration.registration.Bus == nil {
		return virtualbus.DeviceMeta{}, false
	}
	registration.state.mu.Lock()
	defer registration.state.mu.Unlock()
	if registration.state.closed {
		return virtualbus.DeviceMeta{}, false
	}
	return registration.registration, true
}

// PublishSemanticInputWire is the sole broker input entry. It authenticates
// the exact VirtualBus.Add incarnation before delegating to the device's
// generation-fenced semantic input wire.
func (registration *AuthorizedXboxOneRetainedUSBRegistration) PublishSemanticInputWire(
	revision uint64,
	wire []byte,
) error {
	if registration == nil || registration.state == nil {
		return ErrAuthorizedXboxOneRegistrationInactive
	}
	state := registration.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed || registration.device == nil ||
		registration.registration.Bus == nil {
		return ErrAuthorizedXboxOneRegistrationInactive
	}
	lease, active := registration.registration.Bus.AcquireRegistrationOperation(
		registration.registration)
	if !active {
		return ErrAuthorizedXboxOneRegistrationInactive
	}
	defer lease.Release()
	if state.beforeSemanticPublish != nil {
		state.beforeSemanticPublish()
	}
	if err := registration.device.PublishSemanticInputWire(revision, wire); err != nil {
		return fmt.Errorf("%w: publish: %w",
			ErrAuthorizedXboxOneRegistration, err)
	}
	return nil
}

// Close fences later publications and removes only this exact Add
// incarnation. Concurrent/repeated calls join the first removal and replay its
// exact result. A server-side one-shot retirement or explicit bus/device
// removal makes that result an idempotent success and cannot select a
// successor.
func (registration *AuthorizedXboxOneRetainedUSBRegistration) Close() error {
	if registration == nil {
		return nil
	}
	state := registration.state
	if state == nil {
		return ErrAuthorizedXboxOneRegistrationInactive
	}
	state.mu.Lock()
	if state.closed {
		done := state.closeDone
		state.mu.Unlock()
		if done != nil {
			<-done
		}
		state.mu.Lock()
		err := state.closeErr
		state.mu.Unlock()
		return err
	}
	state.closed = true
	if state.closeDone == nil {
		state.closeDone = make(chan struct{})
	}
	done := state.closeDone
	server := registration.server
	exact := registration.registration
	state.mu.Unlock()
	var closeErr error
	if server == nil {
		closeErr = ErrAuthorizedXboxOneRegistrationInactive
	} else {
		_, err := server.RemoveDeviceRegistrationIfPresent(exact)
		if err != nil {
			closeErr = fmt.Errorf("%w: remove: %w",
				ErrAuthorizedXboxOneRegistration, err)
		}
	}
	state.mu.Lock()
	state.closeErr = closeErr
	close(done)
	state.mu.Unlock()
	return closeErr
}

package xboxone

import (
	"errors"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
)

var (
	// ErrProductionBrokerUnavailable reports a missing, stale, or duplicated
	// DS4Windows broker stream. Only one producer may own a persona at once.
	ErrProductionBrokerUnavailable = errors.New(
		"xboxone: production broker stream is unavailable")
	// ErrProductionFactoryRequired keeps the authorized retained persona out of
	// the generic deviceSpecific factory. The internal authenticated endpoint
	// must construct every identity and feedback binding explicitly.
	ErrProductionFactoryRequired = errors.New(
		"xboxone: explicit authorized production factory is required")
	// ErrProductionBrokerAuthenticationRequired rejects a broker lane which
	// did not complete VIIPER's authenticated session handshake.
	ErrProductionBrokerAuthenticationRequired = errors.New(
		"xboxone: authenticated production broker stream is required")
	ErrProductionBrokerFeedbackRejected = errors.New(
		"xboxone: DS4Windows rejected canonical feedback")
	ErrProductionBrokerFeedbackAmbiguous = errors.New(
		"xboxone: canonical feedback acknowledgement is ambiguous")
	ErrProductionBrokerInputRevision = errors.New(
		"xboxone: semantic input revision is not the exact successor")
)

// ProductionRetainedUSBDeviceOptions contains the already-decided facts for
// one authenticated DS4Windows broker lifetime. It deliberately provides no
// Xbox VID/PID, strings, firmware, or authorization defaults.
type ProductionRetainedUSBDeviceOptions struct {
	Identity              ControllerIdentity
	USB                   ControllerUSBConfig
	Strings               ControllerUSBIdentityStrings
	IdentityAuthorization ControllerIdentityAuthorizationDecision
	FeedbackBinding       ControllerPersonaFeedbackBindingV1
	ProtocolTimeMS        uint64
	AuthorityID           uint64
	ImportDeviceID        uint64
	LocalTimeout          time.Duration
}

// ProductionRetainedUSBPreparation is a one-use, pointer-owned composition
// credential prepared before the retained registration consumes external
// identity authorization. The internal registry alone invokes the canonical
// device constructor and then binds this exact executor/bridge pair.
type ProductionRetainedUSBPreparation struct {
	mu             sync.Mutex
	authorization  AuthorizedControllerPersonaConfig
	executor       *ControllerPersonaCanonicalFeedbackExecutor
	timedExecutor  *controllerPersonaTimedFeedbackExecutor
	bridge         *controllerPersonaFeedbackStreamBridge
	protocolTimeMS uint64
	authorityID    uint64
	importDeviceID uint64
	localTimeout   time.Duration
	attached       bool
	removal        productionRemovalCapability
}

// controllerPersonaFeedbackStreamBridge is the exact synchronous acceptance
// boundary required by ControllerPersonaCanonicalFeedbackPublisher. One CFBK
// value remains pending until the authenticated DS4Windows consumer confirms
// that its generation-bound physical feedback runtime accepted the complete
// frame. A lost acknowledgement retires the broker session as ambiguous; the
// retained persona's caller then quarantines that one-shot incarnation.
type controllerPersonaFeedbackStreamBridge struct {
	mu           sync.Mutex
	revision     uint64
	sessionToken uint64
	ready        bool
	pending      *controllerPersonaFeedbackDelivery
	wake         chan struct{}
}

type controllerPersonaFeedbackDelivery struct {
	wire     ControllerPersonaCanonicalFeedbackWireV1
	revision uint64
	result   chan error
}

type controllerPersonaBrokerStreamLease struct {
	device *AuthorizedDormantRetainedUSBDevice
	token  uint64
}

func newControllerPersonaFeedbackStreamBridge() *controllerPersonaFeedbackStreamBridge {
	return &controllerPersonaFeedbackStreamBridge{wake: make(chan struct{}, 1)}
}

func (bridge *controllerPersonaFeedbackStreamBridge) PublishControllerFeedback(
	wire ControllerPersonaCanonicalFeedbackWireV1,
	deadline time.Time,
) error {
	if bridge == nil || deadline.IsZero() {
		return ErrCanonicalFeedbackExecutorUninitialized
	}
	var frame controllerfeedback.Frame
	if err := frame.UnmarshalFrom(wire[:]); err != nil {
		return err
	}

	bridge.mu.Lock()
	if !bridge.ready || bridge.sessionToken == 0 || bridge.pending != nil {
		bridge.mu.Unlock()
		return ErrProductionBrokerUnavailable
	}
	if !time.Now().Before(deadline) || bridge.revision == ^uint64(0) {
		bridge.mu.Unlock()
		return ErrCanonicalFeedbackExecutorDeadline
	}
	bridge.revision++
	delivery := &controllerPersonaFeedbackDelivery{
		wire: wire, revision: bridge.revision, result: make(chan error, 1),
	}
	bridge.pending = delivery
	bridge.mu.Unlock()
	latchedSignal(bridge.wake)

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case err := <-delivery.result:
		return err
	case <-timer.C:
		bridge.mu.Lock()
		if bridge.pending == delivery {
			bridge.pending = nil
			bridge.ready = false
			bridge.sessionToken = 0
			bridge.mu.Unlock()
			latchedSignal(bridge.wake)
			return errors.Join(ErrProductionBrokerFeedbackAmbiguous,
				ErrCanonicalFeedbackExecutorDeadline)
		}
		bridge.mu.Unlock()
		// An acknowledgement or session retirement won the exact bridge
		// lock concurrently with the deadline. Its buffered result is now
		// authoritative and cannot arrive late into a successor.
		return <-delivery.result
	}
}

func (bridge *controllerPersonaFeedbackStreamBridge) beginSession(
	token uint64,
) error {
	if bridge == nil || token == 0 {
		return ErrProductionBrokerUnavailable
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.ready || bridge.sessionToken != 0 || bridge.pending != nil {
		return ErrProductionBrokerUnavailable
	}
	bridge.sessionToken = token
	bridge.ready = true
	return nil
}

func (bridge *controllerPersonaFeedbackStreamBridge) endSession(
	token uint64,
	reason error,
) {
	if bridge == nil || token == 0 {
		return
	}
	if reason == nil {
		reason = ErrProductionBrokerUnavailable
	}
	bridge.mu.Lock()
	if bridge.sessionToken != token {
		bridge.mu.Unlock()
		return
	}
	bridge.ready = false
	bridge.sessionToken = 0
	pending := bridge.pending
	bridge.pending = nil
	bridge.mu.Unlock()
	if pending != nil {
		pending.result <- reason
	}
	latchedSignal(bridge.wake)
}

func (bridge *controllerPersonaFeedbackStreamBridge) pendingAfter(
	token uint64,
	after uint64,
	stop <-chan struct{},
) (ControllerPersonaCanonicalFeedbackWireV1, uint64, bool) {
	for bridge != nil {
		bridge.mu.Lock()
		if !bridge.ready || bridge.sessionToken != token {
			bridge.mu.Unlock()
			return ControllerPersonaCanonicalFeedbackWireV1{}, 0, false
		}
		if bridge.pending != nil && bridge.pending.revision > after {
			wire, revision := bridge.pending.wire, bridge.pending.revision
			bridge.mu.Unlock()
			return wire, revision, true
		}
		wake := bridge.wake
		bridge.mu.Unlock()
		select {
		case <-wake:
		case <-stop:
			return ControllerPersonaCanonicalFeedbackWireV1{}, 0, false
		}
	}
	return ControllerPersonaCanonicalFeedbackWireV1{}, 0, false
}

func (bridge *controllerPersonaFeedbackStreamBridge) acknowledge(
	token uint64,
	revision uint64,
	accepted bool,
) error {
	if bridge == nil || token == 0 || revision == 0 {
		return ErrProductionBrokerUnavailable
	}
	bridge.mu.Lock()
	if !bridge.ready || bridge.sessionToken != token ||
		bridge.pending == nil || bridge.pending.revision != revision {
		bridge.mu.Unlock()
		return ErrProductionBrokerUnavailable
	}
	pending := bridge.pending
	bridge.pending = nil
	bridge.mu.Unlock()
	if accepted {
		pending.result <- nil
		return nil
	}
	pending.result <- ErrProductionBrokerFeedbackRejected
	return ErrProductionBrokerFeedbackRejected
}

func (device *AuthorizedDormantRetainedUSBDevice) acquireBrokerStream() (
	controllerPersonaBrokerStreamLease,
	bool,
) {
	if device == nil || device.adapter == nil || device.feedbackBridge == nil {
		return controllerPersonaBrokerStreamLease{}, false
	}
	device.brokerMu.Lock()
	defer device.brokerMu.Unlock()
	if device.brokerFailed || device.brokerStreamActive || device.brokerStreamToken == ^uint64(0) {
		return controllerPersonaBrokerStreamLease{}, false
	}
	device.brokerStreamToken++
	if device.brokerStreamToken == 0 {
		device.brokerStreamToken = 1
	}
	device.brokerStreamActive = true
	device.brokerConsumerReady = false
	return controllerPersonaBrokerStreamLease{
		device: device, token: device.brokerStreamToken,
	}, true
}

func (lease controllerPersonaBrokerStreamLease) release() {
	device := lease.device
	if device == nil || lease.token == 0 {
		return
	}
	device.brokerMu.Lock()
	if device.brokerStreamActive && device.brokerStreamToken == lease.token {
		device.brokerStreamActive = false
		device.brokerConsumerReady = false
	}
	device.brokerMu.Unlock()
	device.feedbackBridge.endSession(lease.token,
		ErrProductionBrokerUnavailable)
}

func (lease controllerPersonaBrokerStreamLease) consumerReady() error {
	device := lease.device
	if device == nil || lease.token == 0 || device.feedbackBridge == nil {
		return ErrProductionBrokerUnavailable
	}
	device.brokerMu.Lock()
	defer device.brokerMu.Unlock()
	if !device.brokerStreamActive || device.brokerStreamToken != lease.token ||
		device.brokerConsumerReady {
		return ErrProductionBrokerUnavailable
	}
	if err := device.feedbackBridge.beginSession(lease.token); err != nil {
		return err
	}
	device.brokerConsumerReady = true
	return nil
}

func (lease controllerPersonaBrokerStreamLease) publishInput(
	revision uint64,
	wire []byte,
) error {
	device := lease.device
	if device == nil || lease.token == 0 || revision == 0 {
		return ErrProductionBrokerUnavailable
	}
	device.brokerMu.Lock()
	if !device.brokerStreamActive || device.brokerStreamToken != lease.token ||
		!device.brokerConsumerReady ||
		device.brokerInputRevision == ^uint64(0) {
		device.brokerMu.Unlock()
		return ErrProductionBrokerUnavailable
	}
	if device.brokerInputRetired {
		device.brokerMu.Unlock()
		return errRetainedInputHistoryFault
	}
	if revision != device.brokerInputRevision+1 {
		device.brokerMu.Unlock()
		return ErrProductionBrokerInputRevision
	}
	if err := device.PublishSemanticInputWire(revision, wire); err != nil {
		terminal := errors.Is(err, errRetainedInputHistoryFault)
		if terminal {
			// Revoke input/reconnect, not the original feedback consumer. The
			// retained owner must still deliver its exact terminal Stop and get
			// that consumer's ACK before the one-shot incarnation can retire.
			device.brokerFailed = true
			device.brokerInputRetired = true
		}
		device.brokerMu.Unlock()
		return err
	}
	device.brokerInputRevision = revision
	device.brokerMu.Unlock()
	return nil
}

func (lease controllerPersonaBrokerStreamLease) acknowledgeFeedback(
	revision uint64,
	accepted bool,
) error {
	if lease.device == nil || lease.device.feedbackBridge == nil {
		return ErrProductionBrokerUnavailable
	}
	return lease.device.feedbackBridge.acknowledge(
		lease.token, revision, accepted)
}

// ProductionBrokerConsumerReady is a cold-path activation predicate used by
// the authenticated factory transaction. USB/IP attach must not begin until
// the exact broker stream has installed its DS4Windows feedback consumer.
func (device *AuthorizedDormantRetainedUSBDevice) ProductionBrokerConsumerReady() bool {
	if device == nil {
		return false
	}
	device.brokerMu.Lock()
	defer device.brokerMu.Unlock()
	return !device.brokerFailed && device.brokerStreamActive && device.brokerConsumerReady
}

// TryBeginProductionBrokerActivation reserves the one USB/IP activation for
// the exact ready broker stream. Construction alone is deliberately dormant:
// Windows cannot enumerate the persona until the generation-bound feedback
// consumer is already able to acknowledge canonical feedback.
func (device *AuthorizedDormantRetainedUSBDevice) TryBeginProductionBrokerActivation() bool {
	if device == nil {
		return false
	}
	device.brokerMu.Lock()
	defer device.brokerMu.Unlock()
	if device.brokerFailed || !device.brokerStreamActive || !device.brokerConsumerReady ||
		device.brokerActivating || device.brokerActivated {
		return false
	}
	device.brokerActivating = true
	return true
}

// CompleteProductionBrokerActivation publishes the activation result. A
// failed cold-path attach may be retried only while the same authenticated
// ready stream still owns the persona; successful activation is one-shot.
// The result proves this call committed successful activation, not merely
// that native attach returned a port before its consumer disappeared.
func (device *AuthorizedDormantRetainedUSBDevice) CompleteProductionBrokerActivation(
	activated bool,
) bool {
	if device == nil {
		return false
	}
	device.brokerMu.Lock()
	completed := false
	if device.brokerActivating {
		device.brokerActivating = false
		if activated && !device.brokerFailed && device.brokerStreamActive &&
			device.brokerConsumerReady {
			device.brokerActivated = true
			completed = true
		}
	}
	device.brokerMu.Unlock()
	return completed
}

// PrepareProductionRetainedUSBDevice constructs the strict official Console
// Function Map authorization and generation-fenced feedback executor. It does
// not construct or register a USB device, start a goroutine, or perform
// controller or transport I/O. It reads cryptographic randomness for the cold
// registration removal capability.
func PrepareProductionRetainedUSBDevice(
	options ProductionRetainedUSBDeviceOptions,
) (*ProductionRetainedUSBPreparation, error) {
	// A newly constructed canonical persona begins in generation one. The
	// request's PersonaGeneration is a local lifecycle fence, not an external
	// nonce; accepting a foreign starting value would make the first local GIP
	// action (BeginMetadata) impossible to execute. OwnershipEpoch is the fresh
	// cross-process feedback-lease identity.
	if options.FeedbackBinding.PersonaGeneration != 1 {
		return nil, ErrInvalidCanonicalFeedbackBinding
	}
	profile, err := NewUnregisteredControllerProfile(options.Identity, options.USB)
	if err != nil {
		return nil, err
	}
	metadata, err := profile.BindOfficialGamepadMetadataV1(
		OfficialGamepadMetadataConsoleFunctionMap)
	if err != nil {
		return nil, err
	}
	authorization, err := NewAuthorizedControllerPersonaConfig(
		ControllerPersonaConfig{
			Profile: profile, Metadata: metadata,
			CurrentInput:      GamepadInputReportV1{},
			CurrentStatus:     NewWiredNoBatteryStatus(false),
			PoweringOffStatus: NewWiredNoBatteryStatus(true),
		},
		options.Strings,
		options.IdentityAuthorization,
	)
	if err != nil {
		return nil, err
	}
	bridge := newControllerPersonaFeedbackStreamBridge()
	executor, err := NewControllerPersonaCanonicalFeedbackExecutor(
		options.FeedbackBinding, bridge)
	if err != nil {
		return nil, err
	}
	removal, err := newProductionRemovalCapability()
	if err != nil {
		return nil, err
	}
	return &ProductionRetainedUSBPreparation{
		authorization: authorization, executor: executor, bridge: bridge,
		timedExecutor:  newControllerPersonaTimedFeedbackExecutor(executor),
		protocolTimeMS: options.ProtocolTimeMS,
		authorityID:    options.AuthorityID, importDeviceID: options.ImportDeviceID,
		localTimeout: options.LocalTimeout, removal: removal,
	}, nil
}

// AuthorizedConstructionInputs returns the immutable inputs for the sole
// internal registry constructor. The one-shot authorization itself still
// prevents copied preparation values from constructing a second engine.
func (preparation *ProductionRetainedUSBPreparation) AuthorizedConstructionInputs() (
	AuthorizedControllerPersonaConfig,
	uint64,
	uint64,
	uint64,
	ControllerPersonaLocalExecutor,
	time.Duration,
	bool,
) {
	if preparation == nil {
		return AuthorizedControllerPersonaConfig{}, 0, 0, 0, nil, 0, false
	}
	preparation.mu.Lock()
	defer preparation.mu.Unlock()
	if preparation.attached || preparation.executor == nil || preparation.timedExecutor == nil ||
		preparation.bridge == nil {
		return AuthorizedControllerPersonaConfig{}, 0, 0, 0, nil, 0, false
	}
	return preparation.authorization, preparation.protocolTimeMS,
		preparation.authorityID, preparation.importDeviceID,
		preparation.timedExecutor, preparation.localTimeout, true
}

// AttachConstructed authenticates that the internal registry constructed the
// exact device from this preparation and then installs the sole broker lanes.
func (preparation *ProductionRetainedUSBPreparation) AttachConstructed(
	device *AuthorizedDormantRetainedUSBDevice,
) error {
	if preparation == nil || device == nil || device.adapter == nil {
		return ErrInvalidAuthorizedRetainedUSBComposition
	}
	preparation.mu.Lock()
	defer preparation.mu.Unlock()
	if preparation.attached || preparation.executor == nil || preparation.timedExecutor == nil ||
		preparation.bridge == nil || device.feedbackBridge != nil ||
		device.adapter.expectedAuthority != preparation.authorityID ||
		device.adapter.expectedDevice != preparation.importDeviceID ||
		!sameAuthorizedRetainedLocalExecutor(
			device.adapter.local, preparation.timedExecutor) {
		return ErrInvalidAuthorizedRetainedUSBComposition
	}
	device.feedbackBridge = preparation.bridge
	device.removal = preparation.removal
	preparation.timedExecutor.onFailure = device.failTimedFeedback
	device.brokerInputRevision = 1
	preparation.attached = true
	return nil
}

// failTimedFeedback records asynchronous renewal failure at the retained
// owner's existing fatal edge and wakes the broker writer. It never drains
// synchronously: the failing publication still owns the executor operation.
func (device *AuthorizedDormantRetainedUSBDevice) failTimedFeedback(err error) {
	device.brokerMu.Lock()
	token := device.brokerStreamToken
	// Releasing a socket is normally recoverable, but a timed publication
	// failure has permanently fenced this executor. Do not let a replacement
	// socket advertise a ready consumer for that failed persona incarnation.
	device.brokerFailed = true
	device.brokerStreamActive = false
	device.brokerConsumerReady = false
	device.brokerMu.Unlock()
	device.adapter.markLocalFatal(err)
	device.feedbackBridge.endSession(token, err)
}

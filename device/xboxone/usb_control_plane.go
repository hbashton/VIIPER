package xboxone

import (
	"encoding/binary"
	"fmt"
)

const (
	usbSetupPacketSize            = 8
	usbControlMaximumResponseSize = usbMaximumStringDescriptorSize

	usbRequestGetStatus        byte = 0x00
	usbRequestClearFeature     byte = 0x01
	usbRequestSetFeature       byte = 0x03
	usbRequestSetAddress       byte = 0x05
	usbRequestGetDescriptor    byte = 0x06
	usbRequestGetConfiguration byte = 0x08
	usbRequestSetConfiguration byte = 0x09

	usbRequestTypeDeviceIn          byte = 0x80
	usbRequestTypeEndpointIn        byte = 0x82
	usbRequestTypeDeviceOut         byte = 0x00
	usbRequestTypeEndpointOut       byte = 0x02
	usbRequestTypeVendorIn          byte = 0xc0
	usbRequestTypeVendorInterfaceIn byte = 0xc1

	usbDescriptorTypeDevice        byte = 0x01
	usbDescriptorTypeConfiguration byte = 0x02
	usbDescriptorTypeString        byte = 0x03

	usbFeatureEndpointHalt     uint16 = 0x0000
	usbFeatureRemoteWakeup     uint16 = 0x0001
	usbConfigurationUnselected byte   = 0
	usbConfigurationGIP        byte   = 1
)

// USBControlDeviceState is the controller-only USB state domain represented
// by the Def, Adr, and Cfg columns in MS-GIPUSB 1.0 table 4. Detached is a
// local transport boundary, not a fourth USB state.
type USBControlDeviceState uint8

const (
	USBControlDeviceDetached USBControlDeviceState = iota
	USBControlDeviceDefault
	USBControlDeviceAddressed
	USBControlDeviceConfigured
)

// USBControlResponseKind identifies the immutable EP0 data image owned by a
// claim. USBControlResponseNone still requires final admission of an empty
// destination before a no-data request can be resolved as delivered.
type USBControlResponseKind uint8

const (
	USBControlResponseNone USBControlResponseKind = iota + 1
	USBControlResponseDeviceStatus
	USBControlResponseEndpointStatus
	USBControlResponseDeviceDescriptor
	USBControlResponseConfigurationDescriptor
	USBControlResponseLanguageIDDescriptor
	USBControlResponseMicrosoftOSStringDescriptor
	USBControlResponseMicrosoftCompatibleIDDescriptor
	USBControlResponseMicrosoftExtendedPropertiesDescriptor
	USBControlResponseConfigurationValue
	USBControlResponseManufacturerStringDescriptor
	USBControlResponseProductStringDescriptor
	USBControlResponseSerialStringDescriptor
)

// USBControlOutcome is the terminal result of one EP0 claim. DeliveryFailed
// and ExecutionCancelled assert that no late externally visible effect can
// still occur. An admitted asynchronous transaction may use
// ExecutionCancelled only after synchronous cancellation and drain.
type USBControlOutcome uint8

const (
	USBControlDelivered USBControlOutcome = iota + 1
	USBControlDeliveryFailed
	USBControlExecutionCancelled
)

// USBControlClaim is an opaque capability for one immutable EP0 response and
// state effect.
type USBControlClaim struct {
	owner        *USBControlPlane
	token        uint64
	generation   uint64
	responseKind USBControlResponseKind
	responseSize uint8
}

func (claim USBControlClaim) Valid() bool        { return claim.owner != nil && claim.token != 0 }
func (claim USBControlClaim) Generation() uint64 { return claim.generation }
func (claim USBControlClaim) ResponseKind() USBControlResponseKind {
	return claim.responseKind
}
func (claim USBControlClaim) ResponseSize() int { return int(claim.responseSize) }

// USBControlPlaneSnapshot is a value-only diagnostic view. EndpointHalt has
// slots for control endpoint zero, data OUT 01, and data IN 81.
type USBControlPlaneSnapshot struct {
	Generation       uint64
	State            USBControlDeviceState
	Address          byte
	Configuration    byte
	RemoteWakeup     bool
	EndpointHalt     [3]bool
	ClaimOutstanding bool
	ClaimAdmitted    bool
}

type usbControlEffectKind uint8

const (
	usbControlNoEffect usbControlEffectKind = iota
	usbControlSetAddress
	usbControlSetConfiguration
	usbControlSetRemoteWakeup
	usbControlClearRemoteWakeup
	usbControlSetEndpointHalt
	usbControlClearEndpointHalt
)

type usbControlEffect struct {
	kind     usbControlEffectKind
	value    byte
	endpoint byte
}

type usbControlPlan struct {
	response     [usbControlMaximumResponseSize]byte
	responseKind USBControlResponseKind
	responseSize uint8
	effect       usbControlEffect
}

// USBControlPlane is a pure, offline EP0 transaction owner for the
// controller-only request matrix in MS-GIPUSB 1.0 table 4. It composes the
// profile's fixed descriptors but does not register a USB device, synthesize
// identity strings, or call a backend.
//
// Calls must be serialized. The value must not be copied after construction,
// because outstanding claims are bound to its address. A copy of the dormant
// authorized plane loses external-string authority even before its first
// claim; only the exact plane embedded in the authorized engine is bound.
type USBControlPlane struct {
	profile         UnregisteredControllerProfile
	identityBinding *controllerPersonaExternalIdentityBinding
	initialized     bool
	generation      uint64
	state           USBControlDeviceState
	address         byte
	configuration   byte
	remoteWakeup    bool
	endpointHalt    [3]bool

	nextToken     uint64
	hasClaim      bool
	claimAdmitted bool
	claimToken    uint64
	claimPlan     usbControlPlan
}

// NewUSBControlPlane constructs an attached device in USB Default state and
// generation one. Successful construction is not registration or Windows
// binding evidence.
func NewUSBControlPlane(profile UnregisteredControllerProfile) (USBControlPlane, error) {
	return newUSBControlPlane(profile, nil)
}

func newUSBControlPlane(
	profile UnregisteredControllerProfile,
	binding *controllerPersonaExternalIdentityBinding,
) (USBControlPlane, error) {
	if err := profile.validate(); err != nil {
		return USBControlPlane{}, err
	}
	if binding != nil && !binding.authenticatesProfile(profile) {
		return USBControlPlane{}, ErrInvalidControllerIdentityAuthorization
	}
	return USBControlPlane{
		profile: profile, identityBinding: binding,
		initialized: true, generation: 1,
		state: USBControlDeviceDefault,
	}, nil
}

func (plane USBControlPlane) Snapshot() USBControlPlaneSnapshot {
	return USBControlPlaneSnapshot{
		Generation: plane.generation, State: plane.state,
		Address: plane.address, Configuration: plane.configuration,
		RemoteWakeup: plane.remoteWakeup, EndpointHalt: plane.endpointHalt,
		ClaimOutstanding: plane.hasClaim, ClaimAdmitted: plane.claimAdmitted,
	}
}

func (plane *USBControlPlane) nextClaimToken() uint64 {
	plane.nextToken++
	if plane.nextToken == 0 {
		plane.nextToken++
	}
	return plane.nextToken
}

// Claim validates one exact eight-byte SETUP packet against the current USB
// state and privately builds its complete response before exposing a claim.
func (plane *USBControlPlane) Claim(setup []byte) (USBControlClaim, error) {
	if plane == nil || !plane.initialized {
		return USBControlClaim{}, ErrUninitializedUSBControlPlane
	}
	if plane.state == USBControlDeviceDetached {
		return USBControlClaim{}, ErrUSBControlPlaneDetached
	}
	if plane.hasClaim {
		return USBControlClaim{}, ErrUSBControlClaimOutstanding
	}
	if len(setup) != usbSetupPacketSize {
		return USBControlClaim{}, fmt.Errorf(
			"%w: got %d want %d", ErrInvalidUSBSetupPacket, len(setup), usbSetupPacketSize)
	}
	plan, err := plane.plan(setup)
	if err != nil {
		return USBControlClaim{}, err
	}
	token := plane.nextClaimToken()
	plane.hasClaim = true
	plane.claimAdmitted = false
	plane.claimToken = token
	plane.claimPlan = plan
	return USBControlClaim{
		owner: plane, token: token, generation: plane.generation,
		responseKind: plan.responseKind, responseSize: plan.responseSize,
	}, nil
}

func (plane *USBControlPlane) validateClaim(claim USBControlClaim) error {
	if plane == nil || !plane.initialized || !plane.hasClaim ||
		claim.owner != plane || claim.token == 0 || claim.token != plane.claimToken ||
		claim.generation != plane.generation ||
		claim.responseKind != plane.claimPlan.responseKind ||
		claim.responseSize != plane.claimPlan.responseSize {
		return ErrInvalidUSBControlClaim
	}
	return nil
}

// claimLosesConfiguration reports only the exact delivered state transition
// from Configured to Addressed selected by SET_CONFIGURATION(0). The claim is
// still only planned here; a failed or cancelled status completion does not
// lose the configuration.
func (plane *USBControlPlane) claimLosesConfiguration(
	claim USBControlClaim,
) bool {
	if plane.validateClaim(claim) != nil ||
		plane.state != USBControlDeviceConfigured {
		return false
	}
	effect := plane.claimPlan.effect
	return effect.kind == usbControlSetConfiguration &&
		effect.value == usbConfigurationUnselected
}

// AdmitAndCopy is the final serialized boundary before the response or status
// completion becomes visible to the backend. dst must be exactly ResponseSize;
// a mismatch leaves it unchanged and does not admit the claim.
func (plane *USBControlPlane) AdmitAndCopy(claim USBControlClaim, dst []byte) error {
	if err := plane.validateClaim(claim); err != nil {
		return err
	}
	if plane.claimAdmitted {
		return ErrInvalidUSBControlClaim
	}
	if len(dst) != int(plane.claimPlan.responseSize) {
		return fmt.Errorf("%w: got %d want %d",
			ErrInvalidUSBControlDestination, len(dst), plane.claimPlan.responseSize)
	}
	copy(dst, plane.claimPlan.response[:plane.claimPlan.responseSize])
	plane.claimAdmitted = true
	return nil
}

// Resolve applies a state effect only after admitted delivery. Failure or
// synchronous cancel/drain consumes the claim without applying it.
func (plane *USBControlPlane) Resolve(
	claim USBControlClaim,
	outcome USBControlOutcome,
) error {
	if err := plane.validateClaim(claim); err != nil {
		return err
	}
	if outcome != USBControlDelivered && outcome != USBControlDeliveryFailed &&
		outcome != USBControlExecutionCancelled {
		return ErrInvalidUSBControlOutcome
	}
	if outcome == USBControlDelivered && !plane.claimAdmitted {
		return ErrUSBControlClaimNotAdmitted
	}
	if outcome == USBControlDelivered {
		plane.apply(plane.claimPlan.effect)
	}
	plane.clearClaim()
	return nil
}

func (plane *USBControlPlane) clearClaim() {
	plane.hasClaim = false
	plane.claimAdmitted = false
	plane.claimToken = 0
	plane.claimPlan = usbControlPlan{}
}

// Reset moves an attached device to Default and clears every volatile EP0
// state under an exact successor generation. A pending transaction must first
// be completed or synchronously cancelled and drained.
func (plane *USBControlPlane) Reset(successorGeneration uint64) error {
	if err := plane.validateBoundary(successorGeneration, false); err != nil {
		return err
	}
	plane.resetState(successorGeneration, USBControlDeviceDefault)
	return nil
}

// Disconnect clears every volatile EP0 state and enters Detached under an
// exact successor generation.
func (plane *USBControlPlane) Disconnect(successorGeneration uint64) error {
	if err := plane.validateBoundary(successorGeneration, false); err != nil {
		return err
	}
	plane.resetState(successorGeneration, USBControlDeviceDetached)
	return nil
}

// Reconnect is valid only from Detached and starts a fresh Default-state
// generation.
func (plane *USBControlPlane) Reconnect(successorGeneration uint64) error {
	if err := plane.validateBoundary(successorGeneration, true); err != nil {
		return err
	}
	plane.resetState(successorGeneration, USBControlDeviceDefault)
	return nil
}

// adoptRetainedUSBIPAddress records the address boundary that a USB/IP host
// controller owns locally. USB/IP never forwards SET_ADDRESS to the exported
// device, so the retained transport must perform this one explicit transition
// after import admission and after every completed USB reset. It deliberately
// does not relax the ordinary setup-packet state machine.
func (plane *USBControlPlane) adoptRetainedUSBIPAddress() error {
	if plane == nil || !plane.initialized || plane.hasClaim ||
		plane.state != USBControlDeviceDefault || plane.address != 0 ||
		plane.configuration != 0 || plane.remoteWakeup ||
		plane.endpointHalt != [3]bool{} {
		return ErrInvalidUSBControlTransition
	}
	// The remote device never observes the host-controller address value. A
	// stable nonzero sentinel is sufficient for the USB state machine while the
	// vhci controller retains the real address.
	plane.address = 1
	plane.state = USBControlDeviceAddressed
	return nil
}

func (plane *USBControlPlane) validateBoundary(
	successorGeneration uint64,
	requireDetached bool,
) error {
	if plane == nil || !plane.initialized {
		return ErrUninitializedUSBControlPlane
	}
	if plane.hasClaim {
		return ErrUSBControlBoundaryBlocked
	}
	if plane.generation == ^uint64(0) || successorGeneration == 0 ||
		successorGeneration != plane.generation+1 ||
		(requireDetached && plane.state != USBControlDeviceDetached) ||
		(!requireDetached && plane.state == USBControlDeviceDetached) {
		return ErrInvalidUSBControlTransition
	}
	return nil
}

func (plane *USBControlPlane) resetState(
	generation uint64,
	state USBControlDeviceState,
) {
	plane.generation = generation
	plane.state = state
	plane.address = 0
	plane.configuration = 0
	plane.remoteWakeup = false
	plane.endpointHalt = [3]bool{}
}

func (plane *USBControlPlane) plan(setup []byte) (usbControlPlan, error) {
	bmRequestType := setup[0]
	bRequest := setup[1]
	wValue := binary.LittleEndian.Uint16(setup[2:4])
	wIndex := binary.LittleEndian.Uint16(setup[4:6])
	wLength := binary.LittleEndian.Uint16(setup[6:8])

	switch {
	case bmRequestType == usbRequestTypeDeviceIn && bRequest == usbRequestGetStatus &&
		wValue == 0 && wIndex == 0 && wLength == 2 && plane.addressedOrConfigured():
		plan := usbControlPlan{responseKind: USBControlResponseDeviceStatus, responseSize: 2}
		if plane.remoteWakeup {
			plan.response[0] = 0x02
		}
		return plan, nil

	case bmRequestType == usbRequestTypeEndpointIn && bRequest == usbRequestGetStatus &&
		wValue == 0 && wLength == 2:
		endpoint, slot, ok := plane.activeEndpoint(wIndex)
		if !ok {
			break
		}
		plan := usbControlPlan{responseKind: USBControlResponseEndpointStatus, responseSize: 2}
		if plane.endpointHalt[slot] {
			plan.response[0] = 0x01
		}
		_ = endpoint
		return plan, nil

	case bmRequestType == usbRequestTypeDeviceOut && bRequest == usbRequestClearFeature &&
		wValue == usbFeatureRemoteWakeup && wIndex == 0 && wLength == 0 &&
		plane.addressedOrConfigured():
		return noDataUSBControlPlan(usbControlEffect{kind: usbControlClearRemoteWakeup}), nil

	case bmRequestType == usbRequestTypeDeviceOut && bRequest == usbRequestSetFeature &&
		wValue == usbFeatureRemoteWakeup && wIndex == 0 && wLength == 0 &&
		plane.addressedOrConfigured():
		return noDataUSBControlPlan(usbControlEffect{kind: usbControlSetRemoteWakeup}), nil

	case bmRequestType == usbRequestTypeEndpointOut &&
		(bRequest == usbRequestClearFeature || bRequest == usbRequestSetFeature) &&
		wValue == usbFeatureEndpointHalt && wLength == 0:
		endpoint, slot, ok := plane.activeEndpoint(wIndex)
		if !ok {
			break
		}
		// USB 2.0 section 9.4.5 neither requires nor recommends the Halt
		// feature for the Default Control Pipe. This offline persona does not
		// implement it: status and an idempotent clear remain valid, while a
		// request to set the unsupported feature fails closed.
		if bRequest == usbRequestSetFeature && slot == 0 {
			break
		}
		effect := usbControlEffect{kind: usbControlSetEndpointHalt, endpoint: endpoint}
		if bRequest == usbRequestClearFeature {
			effect.kind = usbControlClearEndpointHalt
		}
		return noDataUSBControlPlan(effect), nil

	case bmRequestType == usbRequestTypeDeviceOut && bRequest == usbRequestSetAddress &&
		wValue <= 127 && wIndex == 0 && wLength == 0 &&
		(plane.state == USBControlDeviceDefault || plane.state == USBControlDeviceAddressed):
		return noDataUSBControlPlan(usbControlEffect{
			kind: usbControlSetAddress, value: byte(wValue),
		}), nil

	case bmRequestType == usbRequestTypeDeviceIn && bRequest == usbRequestGetDescriptor:
		return plane.planDescriptor(wValue, wIndex, wLength)

	case bmRequestType == usbRequestTypeDeviceIn && bRequest == usbRequestGetConfiguration &&
		wValue == 0 && wIndex == 0 && wLength == 1 && plane.addressedOrConfigured():
		plan := usbControlPlan{
			responseKind: USBControlResponseConfigurationValue, responseSize: 1,
		}
		plan.response[0] = plane.configuration
		return plan, nil

	case bmRequestType == usbRequestTypeDeviceOut && bRequest == usbRequestSetConfiguration &&
		wValue <= uint16(usbConfigurationGIP) && wIndex == 0 && wLength == 0 &&
		plane.addressedOrConfigured():
		return noDataUSBControlPlan(usbControlEffect{
			kind: usbControlSetConfiguration, value: byte(wValue),
		}), nil

	case bmRequestType == usbRequestTypeVendorIn && bRequest == MicrosoftOSVendorCode &&
		wValue == 0 && wIndex == 0x0004 && wLength > 0 &&
		plane.addressedOrConfigured():
		var full [MicrosoftExtendedCompatibleIDDescriptorSize]byte
		if err := plane.profile.EncodeMicrosoftExtendedCompatibleIDDescriptorInto(
			full[:]); err != nil {
			return usbControlPlan{}, err
		}
		return truncatedUSBControlPlan(
			USBControlResponseMicrosoftCompatibleIDDescriptor, full[:], wLength), nil

	case bmRequestType == usbRequestTypeVendorInterfaceIn &&
		bRequest == MicrosoftOSVendorCode && wValue == 0 &&
		wIndex == 0x0005 && wLength > 0 && plane.addressedOrConfigured():
		var full [MicrosoftExtendedPropertiesDescriptorSize]byte
		if err := plane.profile.EncodeMicrosoftExtendedPropertiesDescriptorInto(
			full[:]); err != nil {
			return usbControlPlan{}, err
		}
		return truncatedUSBControlPlan(
			USBControlResponseMicrosoftExtendedPropertiesDescriptor,
			full[:], wLength), nil
	}

	return usbControlPlan{}, ErrUSBControlRequestStalled
}

func (plane *USBControlPlane) planDescriptor(
	wValue uint16,
	wIndex uint16,
	wLength uint16,
) (usbControlPlan, error) {
	descriptorType := byte(wValue >> 8)
	descriptorIndex := byte(wValue)
	switch {
	case descriptorType == usbDescriptorTypeDevice && descriptorIndex == 0 && wIndex == 0:
		var full [USBDeviceDescriptorSize]byte
		if err := plane.profile.EncodeUSBDeviceDescriptorInto(full[:]); err != nil {
			return usbControlPlan{}, err
		}
		return truncatedUSBControlPlan(
			USBControlResponseDeviceDescriptor, full[:], wLength), nil

	case descriptorType == usbDescriptorTypeConfiguration && descriptorIndex == 0 && wIndex == 0:
		var full [USBControllerConfigurationDescriptorSize]byte
		if err := plane.profile.EncodeUSBControllerConfigurationDescriptorInto(full[:]); err != nil {
			return usbControlPlan{}, err
		}
		return truncatedUSBControlPlan(
			USBControlResponseConfigurationDescriptor, full[:], wLength), nil

	case descriptorType == usbDescriptorTypeString && descriptorIndex == 0 && wIndex == 0:
		var full [USBLanguageIDDescriptorSize]byte
		if err := plane.profile.EncodeUSBLanguageIDDescriptorInto(full[:]); err != nil {
			return usbControlPlan{}, err
		}
		return truncatedUSBControlPlan(
			USBControlResponseLanguageIDDescriptor, full[:], wLength), nil

	case descriptorType == usbDescriptorTypeString &&
		descriptorIndex >= 1 && descriptorIndex <= 3 &&
		wIndex == USBEnglishUnitedStatesLanguageID:
		if plane.identityBinding == nil {
			return usbControlPlan{}, fmt.Errorf("%w: %w",
				ErrUSBControlRequestStalled, ErrUSBStringDescriptorUnavailable)
		}
		if !plane.identityBinding.authenticatesControlPlane(plane) {
			return usbControlPlan{}, fmt.Errorf("%w: %w",
				ErrUSBControlRequestStalled, ErrInvalidControllerIdentityAuthorization)
		}
		descriptor, responseKind, ok :=
			plane.identityBinding.descriptors.descriptor(descriptorIndex)
		if !ok || !descriptor.validate() {
			return usbControlPlan{}, fmt.Errorf("%w: %w",
				ErrUSBControlRequestStalled, ErrInvalidExternalUSBIdentityStrings)
		}
		return truncatedUSBControlPlan(
			responseKind, descriptor.wire[:descriptor.size], wLength), nil

	case descriptorType == usbDescriptorTypeString &&
		descriptorIndex == MicrosoftOSStringIndex && wIndex == 0 &&
		wLength == MicrosoftOSStringDescriptorSize && plane.addressedOrConfigured():
		plan := usbControlPlan{
			responseKind: USBControlResponseMicrosoftOSStringDescriptor,
			responseSize: MicrosoftOSStringDescriptorSize,
		}
		if err := plane.profile.EncodeMicrosoftOSStringDescriptorInto(
			plan.response[:MicrosoftOSStringDescriptorSize]); err != nil {
			return usbControlPlan{}, err
		}
		return plan, nil
	}
	return usbControlPlan{}, ErrUSBControlRequestStalled
}

func truncatedUSBControlPlan(
	kind USBControlResponseKind,
	full []byte,
	wLength uint16,
) usbControlPlan {
	size := min(len(full), int(wLength))
	plan := usbControlPlan{responseKind: kind, responseSize: uint8(size)}
	copy(plan.response[:size], full[:size])
	return plan
}

func noDataUSBControlPlan(effect usbControlEffect) usbControlPlan {
	return usbControlPlan{responseKind: USBControlResponseNone, effect: effect}
}

func (plane USBControlPlane) addressedOrConfigured() bool {
	return plane.state == USBControlDeviceAddressed ||
		plane.state == USBControlDeviceConfigured
}

func (plane USBControlPlane) activeEndpoint(wIndex uint16) (byte, int, bool) {
	if wIndex>>8 != 0 {
		return 0, 0, false
	}
	endpoint := byte(wIndex)
	if plane.state == USBControlDeviceAddressed {
		if endpoint == 0x00 || endpoint == 0x80 {
			return endpoint, 0, true
		}
		return 0, 0, false
	}
	if plane.state != USBControlDeviceConfigured {
		return 0, 0, false
	}
	switch endpoint {
	case 0x00, 0x80:
		return endpoint, 0, true
	case 0x01:
		return endpoint, 1, true
	case 0x81:
		return endpoint, 2, true
	default:
		return 0, 0, false
	}
}

func (plane *USBControlPlane) apply(effect usbControlEffect) {
	switch effect.kind {
	case usbControlNoEffect:
	case usbControlSetAddress:
		plane.address = effect.value
		plane.configuration = 0
		if effect.value == 0 {
			plane.state = USBControlDeviceDefault
			plane.remoteWakeup = false
			plane.endpointHalt = [3]bool{}
		} else {
			plane.state = USBControlDeviceAddressed
		}
	case usbControlSetConfiguration:
		plane.configuration = effect.value
		plane.endpointHalt = [3]bool{}
		if effect.value == usbConfigurationUnselected {
			plane.state = USBControlDeviceAddressed
		} else {
			plane.state = USBControlDeviceConfigured
		}
	case usbControlSetRemoteWakeup:
		plane.remoteWakeup = true
	case usbControlClearRemoteWakeup:
		plane.remoteWakeup = false
	case usbControlSetEndpointHalt, usbControlClearEndpointHalt:
		_, slot, ok := plane.activeEndpoint(uint16(effect.endpoint))
		if ok {
			plane.endpointHalt[slot] = effect.kind == usbControlSetEndpointHalt
		}
	}
}

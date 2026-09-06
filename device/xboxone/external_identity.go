package xboxone

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync"
	"unicode/utf8"
)

const (
	usbMaximumStringDescriptorSize  = 254
	usbMaximumStringSourceUTF8Bytes = 378
)

// ControllerUSBIdentityStrings is the exact external identity text supplied
// by a caller. The package provides no defaults and does not infer a vendor,
// product, serial, trademark, or authorization right.
type ControllerUSBIdentityStrings struct {
	Manufacturer string
	Product      string
	Serial       string
}

// ControllerIdentityAuthorizationDecision is an explicit external caller
// decision. Granted is an attestation by that caller only: VIIPER does not
// validate USB-IF allocation ownership, trademarks, serial uniqueness, or any
// other legal right.
type ControllerIdentityAuthorizationDecision uint8

const (
	ControllerIdentityAuthorizationDenied ControllerIdentityAuthorizationDecision = iota + 1
	ControllerIdentityAuthorizationGranted
)

type externalUSBStringDescriptor struct {
	wire [usbMaximumStringDescriptorSize]byte
	size uint8
}

func (descriptor externalUSBStringDescriptor) validate() bool {
	size := int(descriptor.size)
	if size < 4 || size > len(descriptor.wire) || size&1 != 0 ||
		descriptor.wire[0] != descriptor.size ||
		descriptor.wire[1] != usbDescriptorTypeString {
		return false
	}
	for offset := 2; offset < size; offset += 2 {
		unit := binary.LittleEndian.Uint16(descriptor.wire[offset : offset+2])
		if unit == 0 {
			return false
		}
		if unit >= 0xd800 && unit <= 0xdbff {
			if offset+3 >= size {
				return false
			}
			low := binary.LittleEndian.Uint16(descriptor.wire[offset+2 : offset+4])
			if low < 0xdc00 || low > 0xdfff {
				return false
			}
			offset += 2
		} else if unit >= 0xdc00 && unit <= 0xdfff {
			return false
		}
	}
	return true
}

type externalUSBIdentityDescriptorSet struct {
	manufacturer externalUSBStringDescriptor
	product      externalUSBStringDescriptor
	serial       externalUSBStringDescriptor
	valid        bool
}

func (set externalUSBIdentityDescriptorSet) validate() bool {
	return set.valid && set.manufacturer.validate() && set.product.validate() &&
		set.serial.validate()
}

func (set externalUSBIdentityDescriptorSet) descriptor(
	index byte,
) (externalUSBStringDescriptor, USBControlResponseKind, bool) {
	switch index {
	case 1:
		return set.manufacturer, USBControlResponseManufacturerStringDescriptor, true
	case 2:
		return set.product, USBControlResponseProductStringDescriptor, true
	case 3:
		return set.serial, USBControlResponseSerialStringDescriptor, true
	default:
		return externalUSBStringDescriptor{}, USBControlResponseNone, false
	}
}

func encodeExternalUSBStringDescriptor(
	name string,
	value string,
) (externalUSBStringDescriptor, error) {
	// A 254-byte descriptor has at most 126 UTF-16 code units. The largest
	// UTF-8 source capable of fitting is therefore 126 three-byte BMP scalars.
	// Reject the byte-impossible shape before utf8.ValidString can scan it.
	if len(value) > usbMaximumStringSourceUTF8Bytes {
		return externalUSBStringDescriptor{}, fmt.Errorf(
			"%w: %s UTF-8 source exceeds %d bytes",
			ErrInvalidExternalUSBIdentityStrings, name,
			usbMaximumStringSourceUTF8Bytes)
	}
	if value == "" || !utf8.ValidString(value) {
		return externalUSBStringDescriptor{}, fmt.Errorf(
			"%w: %s is empty or malformed UTF-8",
			ErrInvalidExternalUSBIdentityStrings, name)
	}
	var descriptor externalUSBStringDescriptor
	offset := 2
	for _, codePoint := range value {
		if codePoint == 0 || codePoint >= 0xd800 && codePoint <= 0xdfff {
			return externalUSBStringDescriptor{}, fmt.Errorf(
				"%w: %s contains a forbidden Unicode scalar",
				ErrInvalidExternalUSBIdentityStrings, name)
		}
		switch {
		case codePoint <= 0xffff:
			if offset+2 > len(descriptor.wire) {
				return externalUSBStringDescriptor{}, fmt.Errorf(
					"%w: %s UTF-16LE descriptor exceeds %d bytes",
					ErrInvalidExternalUSBIdentityStrings, name,
					usbMaximumStringDescriptorSize)
			}
			binary.LittleEndian.PutUint16(descriptor.wire[offset:offset+2], uint16(codePoint))
			offset += 2
		case codePoint <= utf8.MaxRune:
			if offset+4 > len(descriptor.wire) {
				return externalUSBStringDescriptor{}, fmt.Errorf(
					"%w: %s UTF-16LE descriptor exceeds %d bytes",
					ErrInvalidExternalUSBIdentityStrings, name,
					usbMaximumStringDescriptorSize)
			}
			value := uint32(codePoint) - 0x10000
			binary.LittleEndian.PutUint16(
				descriptor.wire[offset:offset+2], uint16(0xd800+(value>>10)))
			binary.LittleEndian.PutUint16(
				descriptor.wire[offset+2:offset+4], uint16(0xdc00+(value&0x3ff)))
			offset += 4
		default:
			return externalUSBStringDescriptor{}, fmt.Errorf(
				"%w: %s contains an invalid Unicode scalar",
				ErrInvalidExternalUSBIdentityStrings, name)
		}
	}
	descriptor.size = uint8(offset)
	descriptor.wire[0] = descriptor.size
	descriptor.wire[1] = usbDescriptorTypeString
	if !descriptor.validate() {
		return externalUSBStringDescriptor{}, fmt.Errorf(
			"%w: %s did not form a canonical UTF-16LE descriptor",
			ErrInvalidExternalUSBIdentityStrings, name)
	}
	return descriptor, nil
}

func serialContainsDeviceID(serial string, deviceID uint64) bool {
	if len(serial) != 32 {
		return false
	}
	var idBytes [8]byte
	var idHex [16]byte
	binary.BigEndian.PutUint64(idBytes[:], deviceID)
	hex.Encode(idHex[:], idBytes[:])
	for start := 0; start+len(idHex) <= len(serial); start++ {
		matches := true
		for index, expected := range idHex {
			actual := serial[start+index]
			if actual >= 'A' && actual <= 'F' {
				actual += 'a' - 'A'
			}
			if actual != expected {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func buildExternalUSBIdentityDescriptorSet(
	profile UnregisteredControllerProfile,
	strings ControllerUSBIdentityStrings,
) (externalUSBIdentityDescriptorSet, error) {
	if err := profile.validate(); err != nil {
		return externalUSBIdentityDescriptorSet{}, err
	}
	for _, value := range []struct {
		name  string
		value string
	}{
		{name: "manufacturer", value: strings.Manufacturer},
		{name: "product", value: strings.Product},
		{name: "serial", value: strings.Serial},
	} {
		if value.name == "serial" {
			if len(value.value) != 32 {
				return externalUSBIdentityDescriptorSet{}, fmt.Errorf(
					"%w: serial must contain exactly 32 hexadecimal digits",
					ErrInvalidExternalUSBIdentityStrings)
			}
			for index := range value.value {
				character := value.value[index]
				if !((character >= '0' && character <= '9') ||
					(character >= 'a' && character <= 'f') ||
					(character >= 'A' && character <= 'F')) {
					return externalUSBIdentityDescriptorSet{}, fmt.Errorf(
						"%w: serial contains a non-hexadecimal byte",
						ErrInvalidExternalUSBIdentityStrings)
				}
			}
			if !serialContainsDeviceID(value.value, profile.identity.DeviceID) {
				return externalUSBIdentityDescriptorSet{}, fmt.Errorf(
					"%w: %w", ErrInvalidExternalUSBIdentityStrings,
					ErrUSBSerialIdentityMismatch)
			}
		}
	}
	manufacturer, err := encodeExternalUSBStringDescriptor(
		"manufacturer", strings.Manufacturer)
	if err != nil {
		return externalUSBIdentityDescriptorSet{}, err
	}
	product, err := encodeExternalUSBStringDescriptor("product", strings.Product)
	if err != nil {
		return externalUSBIdentityDescriptorSet{}, err
	}
	serial, err := encodeExternalUSBStringDescriptor("serial", strings.Serial)
	if err != nil {
		return externalUSBIdentityDescriptorSet{}, err
	}
	set := externalUSBIdentityDescriptorSet{
		manufacturer: manufacturer,
		product:      product,
		serial:       serial,
		valid:        true,
	}
	if !set.validate() {
		return externalUSBIdentityDescriptorSet{}, ErrInvalidExternalUSBIdentityStrings
	}
	return set, nil
}

type controllerPersonaIdentityCredential struct {
	marker byte
}

type controllerPersonaExternalIdentityBinding struct {
	credential                     *controllerPersonaIdentityCredential
	profileIssuance                *controllerProfileIssuance
	metadataIssuance               *boundMetadataIssuance
	engine                         *ControllerPersonaEngine
	controlPlane                   *USBControlPlane
	identity                       ControllerIdentity
	usb                            ControllerUSBConfig
	metadataIdentity               ControllerIdentity
	metadataDigest                 [sha256.Size]byte
	metadataLength                 int
	metadataOfficialGamepadVariant OfficialGamepadMetadataVariant
	descriptors                    externalUSBIdentityDescriptorSet
	valid                          bool
}

func (binding *controllerPersonaExternalIdentityBinding) authenticatesProfile(
	profile UnregisteredControllerProfile,
) bool {
	return binding != nil && binding.valid && binding.credential != nil &&
		binding.credential.marker == 1 && binding.profileIssuance != nil &&
		binding.profileIssuance.marker == 1 && profile.validate() == nil &&
		profile.issuance == binding.profileIssuance &&
		profile.identity == binding.identity && profile.usb == binding.usb &&
		binding.descriptors.validate()
}

func (binding *controllerPersonaExternalIdentityBinding) authenticatesConfig(
	config ControllerPersonaConfig,
) bool {
	if !binding.authenticatesProfile(config.Profile) ||
		binding.metadataIssuance == nil ||
		binding.metadataIssuance.marker != 1 || !config.Metadata.valid ||
		config.Metadata.issuance != binding.metadataIssuance ||
		config.Metadata.profileIssuance != binding.profileIssuance ||
		config.Metadata.identity != binding.metadataIdentity ||
		config.Metadata.officialGamepadVariant !=
			binding.metadataOfficialGamepadVariant ||
		len(config.Metadata.data) != binding.metadataLength {
		return false
	}
	return sha256.Sum256(config.Metadata.data) == binding.metadataDigest
}

func (binding *controllerPersonaExternalIdentityBinding) bindExactEngine(
	engine *ControllerPersonaEngine,
) bool {
	if binding == nil || engine == nil || binding.engine != nil ||
		binding.controlPlane != nil || engine.identityBinding != binding ||
		engine.usb.identityBinding != binding ||
		!binding.authenticatesConfig(ControllerPersonaConfig{
			Profile: engine.profile, Metadata: engine.metadata,
			CurrentInput: engine.currentInput, CurrentStatus: engine.currentStatus,
			PoweringOffStatus: engine.poweringOffStatus,
		}) {
		return false
	}
	binding.engine = engine
	binding.controlPlane = &engine.usb
	return true
}

func (binding *controllerPersonaExternalIdentityBinding) authenticatesEngine(
	engine *ControllerPersonaEngine,
) bool {
	return binding != nil && engine != nil && binding.engine == engine &&
		binding.controlPlane == &engine.usb && engine.identityBinding == binding &&
		engine.usb.identityBinding == binding &&
		binding.authenticatesProfile(engine.usb.profile) &&
		binding.authenticatesConfig(ControllerPersonaConfig{
			Profile: engine.profile, Metadata: engine.metadata,
			CurrentInput: engine.currentInput, CurrentStatus: engine.currentStatus,
			PoweringOffStatus: engine.poweringOffStatus,
		})
}

func (binding *controllerPersonaExternalIdentityBinding) authenticatesControlPlane(
	plane *USBControlPlane,
) bool {
	if binding == nil || plane == nil || binding.controlPlane != plane ||
		binding.engine == nil || &binding.engine.usb != plane ||
		binding.engine.identityBinding != binding ||
		plane.identityBinding != binding ||
		!binding.authenticatesProfile(plane.profile) {
		return false
	}
	engine := binding.engine
	return binding.authenticatesConfig(ControllerPersonaConfig{
		Profile: engine.profile, Metadata: engine.metadata,
		CurrentInput: engine.currentInput, CurrentStatus: engine.currentStatus,
		PoweringOffStatus: engine.poweringOffStatus,
	})
}

type controllerPersonaIdentityAuthorizationState uint8

const (
	controllerPersonaIdentityAuthorizationIssued controllerPersonaIdentityAuthorizationState = iota + 1
	controllerPersonaIdentityAuthorizationConsuming
	controllerPersonaIdentityAuthorizationConsumed
	controllerPersonaIdentityAuthorizationQuarantined
)

type controllerPersonaIdentityAuthorizationOwner struct {
	mu         sync.Mutex
	state      controllerPersonaIdentityAuthorizationState
	credential *controllerPersonaIdentityCredential
	config     ControllerPersonaConfig
	binding    *controllerPersonaExternalIdentityBinding
}

// AuthorizedControllerPersonaConfig is a private one-shot capability. Copying
// the value does not duplicate authority: at most one exact copy can construct
// an engine. Its zero value and every stale, forged, or cross-owner value fail
// closed.
type AuthorizedControllerPersonaConfig struct {
	owner            *controllerPersonaIdentityAuthorizationOwner
	credential       *controllerPersonaIdentityCredential
	profileIssuance  *controllerProfileIssuance
	metadataIssuance *boundMetadataIssuance
}

// NewAuthorizedControllerPersonaConfig validates and privately pre-encodes
// the exact caller-supplied strings, then records the caller's explicit
// authorization decision against this exact profile and metadata issuance.
// Granted is caller attestation, not proof supplied or verified by VIIPER.
func NewAuthorizedControllerPersonaConfig(
	config ControllerPersonaConfig,
	strings ControllerUSBIdentityStrings,
	decision ControllerIdentityAuthorizationDecision,
) (AuthorizedControllerPersonaConfig, error) {
	switch decision {
	case ControllerIdentityAuthorizationDenied:
		return AuthorizedControllerPersonaConfig{}, ErrControllerIdentityAuthorizationDenied
	case ControllerIdentityAuthorizationGranted:
	case 0:
		fallthrough
	default:
		return AuthorizedControllerPersonaConfig{},
			ErrInvalidControllerIdentityAuthorizationDecision
	}
	if err := validateControllerPersonaConfig(config); err != nil {
		return AuthorizedControllerPersonaConfig{}, err
	}
	if config.Profile.issuance == nil || config.Metadata.issuance == nil ||
		config.Metadata.profileIssuance != config.Profile.issuance {
		return AuthorizedControllerPersonaConfig{}, ErrInvalidControllerIdentityAuthorization
	}
	descriptors, err := buildExternalUSBIdentityDescriptorSet(config.Profile, strings)
	if err != nil {
		return AuthorizedControllerPersonaConfig{}, err
	}

	// The authorization owner holds an immutable private metadata image. This
	// is construction-only allocation; no string/control warm path allocates.
	metadata := make([]byte, len(config.Metadata.data))
	copy(metadata, config.Metadata.data)
	config.Metadata.data = metadata
	credential := &controllerPersonaIdentityCredential{marker: 1}
	binding := &controllerPersonaExternalIdentityBinding{
		credential:                     credential,
		profileIssuance:                config.Profile.issuance,
		metadataIssuance:               config.Metadata.issuance,
		identity:                       config.Profile.identity,
		usb:                            config.Profile.usb,
		metadataIdentity:               config.Metadata.identity,
		metadataDigest:                 sha256.Sum256(config.Metadata.data),
		metadataLength:                 len(config.Metadata.data),
		metadataOfficialGamepadVariant: config.Metadata.officialGamepadVariant,
		descriptors:                    descriptors,
		valid:                          true,
	}
	owner := &controllerPersonaIdentityAuthorizationOwner{
		state:      controllerPersonaIdentityAuthorizationIssued,
		credential: credential,
		config:     config,
		binding:    binding,
	}
	return AuthorizedControllerPersonaConfig{
		owner:            owner,
		credential:       credential,
		profileIssuance:  config.Profile.issuance,
		metadataIssuance: config.Metadata.issuance,
	}, nil
}

// NewAuthorizedControllerPersonaEngine consumes one exact authorization and
// constructs the same canonical persona engine with only the external-string
// and caller-authorization blockers cleared. Consumption linearizes before
// the engine escapes. Any post-consumption construction contradiction leaves
// the authorization quarantined and permanently unusable.
func NewAuthorizedControllerPersonaEngine(
	authorization AuthorizedControllerPersonaConfig,
	nowMS uint64,
) (*ControllerPersonaEngine, error) {
	owner := authorization.owner
	if owner == nil || authorization.credential == nil ||
		authorization.profileIssuance == nil ||
		authorization.metadataIssuance == nil {
		return nil, ErrInvalidControllerIdentityAuthorization
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.binding == nil || authorization.credential != owner.credential ||
		authorization.profileIssuance != owner.binding.profileIssuance ||
		authorization.metadataIssuance != owner.binding.metadataIssuance {
		return nil, ErrInvalidControllerIdentityAuthorization
	}
	switch owner.state {
	case controllerPersonaIdentityAuthorizationConsumed,
		controllerPersonaIdentityAuthorizationConsuming:
		return nil, ErrStaleControllerIdentityAuthorization
	case controllerPersonaIdentityAuthorizationQuarantined:
		return nil, ErrInvalidControllerIdentityAuthorization
	case controllerPersonaIdentityAuthorizationIssued:
	default:
		owner.state = controllerPersonaIdentityAuthorizationQuarantined
		return nil, ErrInvalidControllerIdentityAuthorization
	}
	if owner.binding == nil || !owner.binding.authenticatesConfig(owner.config) {
		owner.state = controllerPersonaIdentityAuthorizationQuarantined
		return nil, ErrInvalidControllerIdentityAuthorization
	}

	owner.state = controllerPersonaIdentityAuthorizationConsuming
	completed := false
	defer func() {
		if !completed && owner.state == controllerPersonaIdentityAuthorizationConsuming {
			owner.state = controllerPersonaIdentityAuthorizationQuarantined
		}
	}()
	engineValue, err := newControllerPersonaEngine(owner.config, owner.binding, nowMS)
	if err != nil {
		return nil, err
	}
	engine := &engineValue
	if !owner.binding.bindExactEngine(engine) {
		return nil, ErrInvalidControllerIdentityAuthorization
	}
	owner.state = controllerPersonaIdentityAuthorizationConsumed
	completed = true
	return engine, nil
}

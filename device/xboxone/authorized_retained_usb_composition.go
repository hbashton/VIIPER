package xboxone

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"time"
)

// ErrInvalidAuthorizedRetainedUSBComposition reports a contradiction after an
// exact external-identity authorization began composition. The authorization
// and any engine constructed from it are terminally quarantined; callers must
// not retry with a copied capability.
var ErrInvalidAuthorizedRetainedUSBComposition = errors.New(
	"xboxone: invalid authorized dormant retained USB composition")

type authorizedRetainedUSBPostConstructionHook func(*ControllerPersonaEngine) error

type authorizedRetainedUSBCompositionGuard struct {
	owner                         *controllerPersonaIdentityAuthorizationOwner
	credential                    *controllerPersonaIdentityCredential
	profileIssuance               *controllerProfileIssuance
	metadataIssuance              *boundMetadataIssuance
	binding                       *controllerPersonaExternalIdentityBinding
	engine                        *ControllerPersonaEngine
	config                        ControllerPersonaConfig
	configMetadata                []byte
	configProfileIssuance         *controllerProfileIssuance
	configMetadataProfileIssuance *controllerProfileIssuance
	configMetadataIssuance        *boundMetadataIssuance
}

// authorizedRetainedUSBEngineColdSeal is an exact pre-adapter value image of
// the canonical engine. ControllerPersonaEngine contains no mutexes; its
// identity-owned slices/pointers are authenticated independently by guard.
type authorizedRetainedUSBEngineColdSeal struct {
	owner                   *ControllerPersonaEngine
	state                   ControllerPersonaEngine
	binding                 *controllerPersonaExternalIdentityBinding
	bindingState            controllerPersonaExternalIdentityBinding
	metadata                []byte
	metadataTransferData    []byte
	profileIssuance         *controllerProfileIssuance
	metadataProfileIssuance *controllerProfileIssuance
	metadataIssuance        *boundMetadataIssuance
	usbProfileIssuance      *controllerProfileIssuance
}

// NewAuthorizedDormantRetainedUSBAdapter is the dormant one-shot composition
// boundary between external USB identity authorization and retained USB/IP
// ownership. It consumes authorization internally, constructs the exact
// authorized ControllerPersonaEngine, and immediately transfers that pointer
// to the retained adapter. The engine is never returned to the caller.
//
// Construction starts no goroutine and performs no controller I/O. Invalid
// adapter parameters are rejected before authorization is consumed. Once the
// engine has been constructed, every error, contradiction, or panic terminally
// quarantines the exact authorization and invalidates that engine before the
// failure is returned or propagated. This adapter constructor does not
// register a generic API product; NewAuthorizedDormantRetainedUSBDevice wraps
// the same composition for the explicit default-off retained USB/IP factory.
func NewAuthorizedDormantRetainedUSBAdapter(
	authorization AuthorizedControllerPersonaConfig,
	nowMS uint64,
	expectedAuthorityID uint64,
	expectedDeviceID uint64,
	local ControllerPersonaLocalExecutor,
	localTimeout time.Duration,
) (*DormantRetainedUSBAdapter, error) {
	return newAuthorizedDormantRetainedUSBAdapter(
		authorization,
		nowMS,
		expectedAuthorityID,
		expectedDeviceID,
		local,
		localTimeout,
		nil,
	)
}

// newAuthorizedDormantRetainedUSBAdapter always invokes the canonical adapter
// constructor. Its optional package-local hook runs only after that constructor
// and receives only the still-private engine, allowing deterministic failure
// and corruption tests without permitting adapter substitution.
func newAuthorizedDormantRetainedUSBAdapter(
	authorization AuthorizedControllerPersonaConfig,
	nowMS uint64,
	expectedAuthorityID uint64,
	expectedDeviceID uint64,
	local ControllerPersonaLocalExecutor,
	localTimeout time.Duration,
	afterConstruction authorizedRetainedUSBPostConstructionHook,
) (
	adapter *DormantRetainedUSBAdapter,
	err error,
) {
	if expectedAuthorityID == 0 || expectedDeviceID == 0 ||
		!validAuthorizedRetainedLocalExecutor(local) ||
		localTimeout < dormantRetainedUSBMinimumLocalTimeout ||
		localTimeout > dormantRetainedUSBMaximumLocalTimeout {
		return nil, errDormantRetainedUSBUninitialized
	}

	engine, err := NewAuthorizedControllerPersonaEngine(authorization, nowMS)
	if err != nil {
		return nil, err
	}
	guard := issueAuthorizedRetainedUSBCompositionGuard(authorization, engine)
	coldSeal := engine.issueAuthorizedRetainedUSBEngineColdSeal()
	completed := false
	defer func() {
		if !completed {
			guard.quarantine(authorization)
		}
	}()

	var constructionProof *dormantRetainedUSBConstructionProof
	adapter, constructionProof, err = newDormantRetainedUSBAdapter(
		engine,
		expectedAuthorityID,
		expectedDeviceID,
		local,
		localTimeout,
		authorizedRetainedUSBConstructionAuthority,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: %w", ErrInvalidAuthorizedRetainedUSBComposition, err)
	}
	if afterConstruction != nil {
		if hookErr := afterConstruction(engine); hookErr != nil {
			return nil, fmt.Errorf(
				"%w: %w", ErrInvalidAuthorizedRetainedUSBComposition, hookErr)
		}
	}
	if !guard.authenticates(authorization) ||
		!engine.authenticatesAuthorizedRetainedUSBEngineColdSeal(coldSeal) ||
		!adapter.consumeAuthorizedRetainedUSBConstructionProof(
			constructionProof,
			engine,
			expectedAuthorityID,
			expectedDeviceID,
			nowMS,
			local,
			localTimeout,
		) {
		return nil, ErrInvalidAuthorizedRetainedUSBComposition
	}

	completed = true
	return adapter, nil
}

func issueAuthorizedRetainedUSBCompositionGuard(
	authorization AuthorizedControllerPersonaConfig,
	engine *ControllerPersonaEngine,
) authorizedRetainedUSBCompositionGuard {
	guard := authorizedRetainedUSBCompositionGuard{
		owner: authorization.owner, credential: authorization.credential,
		profileIssuance:  authorization.profileIssuance,
		metadataIssuance: authorization.metadataIssuance, engine: engine,
	}
	if engine != nil {
		guard.binding = engine.identityBinding
	}
	if guard.owner != nil {
		guard.owner.mu.Lock()
		guard.config = guard.owner.config
		guard.configProfileIssuance = guard.owner.config.Profile.issuance
		guard.configMetadataProfileIssuance =
			guard.owner.config.Metadata.profileIssuance
		guard.configMetadataIssuance = guard.owner.config.Metadata.issuance
		guard.configMetadata = append(
			[]byte(nil), guard.owner.config.Metadata.data...)
		guard.owner.mu.Unlock()
	}
	return guard
}

func (guard authorizedRetainedUSBCompositionGuard) authenticates(
	authorization AuthorizedControllerPersonaConfig,
) bool {
	owner := guard.owner
	if owner == nil || guard.engine == nil || guard.binding == nil ||
		authorization.owner != owner ||
		authorization.credential != guard.credential ||
		authorization.profileIssuance != guard.profileIssuance ||
		authorization.metadataIssuance != guard.metadataIssuance {
		return false
	}

	owner.mu.Lock()
	defer owner.mu.Unlock()
	return guard.credential != nil && guard.profileIssuance != nil &&
		guard.metadataIssuance != nil && owner.credential == guard.credential &&
		owner.binding == guard.binding &&
		owner.state == controllerPersonaIdentityAuthorizationConsumed &&
		reflect.DeepEqual(owner.config, guard.config) &&
		owner.config.Profile.issuance == guard.configProfileIssuance &&
		owner.config.Metadata.profileIssuance ==
			guard.configMetadataProfileIssuance &&
		owner.config.Metadata.issuance == guard.configMetadataIssuance &&
		sameAuthorizedRetainedByteStorage(
			owner.config.Metadata.data, guard.config.Metadata.data) &&
		bytes.Equal(owner.config.Metadata.data, guard.configMetadata) &&
		guard.binding.credential == guard.credential &&
		guard.binding.profileIssuance == guard.profileIssuance &&
		guard.binding.metadataIssuance == guard.metadataIssuance &&
		guard.binding.engine == guard.engine &&
		guard.binding.authenticatesEngine(guard.engine)
}

func (engine *ControllerPersonaEngine) issueAuthorizedRetainedUSBEngineColdSeal() (
	seal authorizedRetainedUSBEngineColdSeal,
) {
	if engine == nil || engine.ordinaryFeedbackPending() {
		return seal
	}
	seal = authorizedRetainedUSBEngineColdSeal{
		owner: engine, state: *engine, binding: engine.identityBinding,
		metadata: append([]byte(nil), engine.metadata.data...),
		metadataTransferData: append(
			[]byte(nil), engine.metadataTransfer.metadata.data...),
		profileIssuance:         engine.profile.issuance,
		metadataProfileIssuance: engine.metadata.profileIssuance,
		metadataIssuance:        engine.metadata.issuance,
		usbProfileIssuance:      engine.usb.profile.issuance,
	}
	if seal.binding != nil {
		seal.bindingState = *seal.binding
	}
	return seal
}

func (engine *ControllerPersonaEngine) authenticatesAuthorizedRetainedUSBEngineColdSeal(
	seal authorizedRetainedUSBEngineColdSeal,
) bool {
	return engine != nil && !engine.ordinaryFeedbackPending() && seal.owner == engine && seal.binding != nil &&
		engine.identityBinding == seal.binding &&
		reflect.DeepEqual(*engine, seal.state) &&
		*seal.binding == seal.bindingState &&
		engine.profile.issuance == seal.profileIssuance &&
		engine.metadata.profileIssuance == seal.metadataProfileIssuance &&
		engine.metadata.issuance == seal.metadataIssuance &&
		engine.usb.profile.issuance == seal.usbProfileIssuance &&
		sameAuthorizedRetainedByteStorage(
			engine.metadata.data, seal.state.metadata.data) &&
		sameAuthorizedRetainedByteStorage(
			engine.metadataTransfer.metadata.data,
			seal.state.metadataTransfer.metadata.data) &&
		bytes.Equal(engine.metadata.data, seal.metadata) &&
		bytes.Equal(engine.metadataTransfer.metadata.data,
			seal.metadataTransferData)
}

// sameAuthorizedRetainedByteStorage authenticates the exact private backing
// store as well as its bounds. Identity metadata is non-empty; the only empty
// cold-path case is an uninitialized transfer and must remain nil. Content is
// compared separately against an independent deep image.
func sameAuthorizedRetainedByteStorage(left []byte, right []byte) bool {
	if len(left) != len(right) || cap(left) != cap(right) {
		return false
	}
	if len(left) == 0 {
		return left == nil && right == nil
	}
	return &left[0] == &right[0]
}

func (adapter *DormantRetainedUSBAdapter) consumeAuthorizedRetainedUSBConstructionProof(
	proof *dormantRetainedUSBConstructionProof,
	engine *ControllerPersonaEngine,
	expectedAuthorityID uint64,
	expectedDeviceID uint64,
	nowMS uint64,
	local ControllerPersonaLocalExecutor,
	localTimeout time.Duration,
) bool {
	if adapter == nil || engine == nil || engine.ordinaryFeedbackPending() {
		return false
	}
	if proof == nil || proof.consumed || proof.adapter != adapter ||
		proof.engine != engine ||
		proof.coordinator == nil || proof.coordinator != adapter.coordinator ||
		proof.coordinator.engine != engine ||
		proof.expectedAuthority != expectedAuthorityID ||
		proof.expectedDevice != expectedDeviceID || proof.protocolTimeMS != nowMS ||
		proof.localTimeout != localTimeout ||
		!sameAuthorizedRetainedLocalExecutor(proof.local, local) ||
		!sameAuthorizedRetainedLocalExecutor(adapter.local, local) ||
		adapter.identity == 0 ||
		adapter.expectedAuthority != expectedAuthorityID ||
		adapter.expectedDevice != expectedDeviceID ||
		adapter.protocolTimeMS != nowMS || adapter.clockOriginMS != nowMS ||
		adapter.localTimeout != localTimeout || adapter.generation != 1 ||
		adapter.state != dormantRetainedUSBUnbound || adapter.boundLease.Valid() ||
		adapter.activeReset.Valid() || adapter.localWake != nil ||
		adapter.localStop != nil || adapter.localDone != nil {
		return false
	}
	// The proof is one-shot and does not remain as ambient authority on the
	// successfully composed adapter.
	proof.consumed = true
	return true
}

// validAuthorizedRetainedLocalExecutor requires a non-nil pointer before
// consuming the one-shot authorization. This stateful effect boundary needs
// exact object identity: value equality could collapse two distinct executor
// owners, and even a statically comparable struct can panic during interface
// equality when one of its interface fields contains an uncomparable value.
// Go may also assign distinct pointers to zero-sized pointees the same address,
// so those pointers cannot authenticate distinct executor objects.
func validAuthorizedRetainedLocalExecutor(
	local ControllerPersonaLocalExecutor,
) bool {
	if local == nil {
		return false
	}
	value := reflect.ValueOf(local)
	return value.Kind() == reflect.Pointer && !value.IsNil() &&
		value.Type().Elem().Size() != 0
}

func sameAuthorizedRetainedLocalExecutor(
	left ControllerPersonaLocalExecutor,
	right ControllerPersonaLocalExecutor,
) bool {
	if !validAuthorizedRetainedLocalExecutor(left) ||
		!validAuthorizedRetainedLocalExecutor(right) {
		return false
	}
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	// Pointer type plus address authenticates the exact executor object. Both
	// interface values keep their referents live for the comparison.
	return leftValue.Type() == rightValue.Type() &&
		leftValue.Pointer() == rightValue.Pointer()
}

func (guard authorizedRetainedUSBCompositionGuard) quarantine(
	authorization AuthorizedControllerPersonaConfig,
) {
	// engine is the exact local pointer returned by the successful authorized
	// constructor before any fallible adapter work. Never follow a mutable
	// binding.engine pointer here: a contradiction may have replaced it.
	engine := guard.engine
	if engine != nil {
		engine.initialized = false
		engine.capabilityBlockers = controllerPersonaKnownBlockers
		engine.usb.initialized = false
	}
	if guard.binding != nil {
		guard.binding.valid = false
	}

	owner := guard.owner
	if owner == nil {
		return
	}

	owner.mu.Lock()
	defer owner.mu.Unlock()
	// guard was minted only after this exact authorization successfully
	// constructed engine. Mutable owner.binding/state are deliberately not part
	// of this terminal test; corruption of those fields must not prevent
	// quarantine. The historical capability pointers remain exact and cannot be
	// guessed or substituted by another authorization owner.
	if authorization.owner != owner ||
		authorization.credential != guard.credential ||
		authorization.profileIssuance != guard.profileIssuance ||
		authorization.metadataIssuance != guard.metadataIssuance {
		return
	}
	owner.state = controllerPersonaIdentityAuthorizationQuarantined
}

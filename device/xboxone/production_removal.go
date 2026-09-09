package xboxone

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
)

var errProductionRemovalCapability = errors.New("xboxone: invalid production removal capability")

// This capability is management-only. It is never part of USB descriptors,
// generic device metadata, feedback, or the semantic input hot path.
type productionRemovalCapability struct {
	nonce            [32]byte
	issued           bool
	registrationDone <-chan struct{}
}

func newProductionRemovalCapability() (productionRemovalCapability, error) {
	var capability productionRemovalCapability
	if _, err := rand.Read(capability.nonce[:]); err != nil {
		return productionRemovalCapability{}, errProductionRemovalCapability
	}
	capability.issued = true
	return capability, nil
}

// BindProductionRemovalRegistration binds the cold production nonce exactly
// once to the unique lifetime channel of the successful VirtualBus.Add. Even a
// re-add of the same device pointer cannot inherit the old removal authority.
// Only the production registry calls this; it returns the token to the create
// response, never to device-list or descriptor callers.
func (device *AuthorizedDormantRetainedUSBDevice) BindProductionRemovalRegistration(
	registrationDone <-chan struct{},
) (string, error) {
	if device == nil || registrationDone == nil {
		return "", errProductionRemovalCapability
	}
	device.brokerMu.Lock()
	defer device.brokerMu.Unlock()
	if !device.removal.issued || device.feedbackBridge == nil ||
		device.removal.registrationDone != nil {
		return "", errProductionRemovalCapability
	}
	select {
	case <-registrationDone:
		return "", errProductionRemovalCapability
	default:
	}
	device.removal.registrationDone = registrationDone
	return hex.EncodeToString(device.removal.nonce[:]), nil
}

// AuthorizesProductionRemoval authenticates the token against the caller's
// captured exact registration. Cancellation does not revoke this comparison:
// a concurrent remover may still join that same removal, but cannot select a
// successor. Malformed tokens are rejected without including them in errors.
func (device *AuthorizedDormantRetainedUSBDevice) AuthorizesProductionRemoval(
	registrationDone <-chan struct{}, token string,
) bool {
	if device == nil || registrationDone == nil || !ValidProductionRemovalToken(token) {
		return false
	}
	var decoded [32]byte
	if _, err := hex.Decode(decoded[:], []byte(token)); err != nil {
		return false
	}
	device.brokerMu.Lock()
	defer device.brokerMu.Unlock()
	return device.removal.issued && device.feedbackBridge != nil &&
		device.removal.registrationDone == registrationDone &&
		subtle.ConstantTimeCompare(device.removal.nonce[:], decoded[:]) == 1
}

// ValidProductionRemovalToken accepts only the canonical 256-bit lowercase
// hexadecimal wire representation; it does not authenticate a capability.
func ValidProductionRemovalToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	for i := range len(token) {
		if (token[i] < '0' || token[i] > '9') && (token[i] < 'a' || token[i] > 'f') {
			return false
		}
	}
	return true
}

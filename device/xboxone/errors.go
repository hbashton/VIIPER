package xboxone

import "errors"

var (
	// ErrInvalidLength reports that a fixed-size message body was not exact.
	ErrInvalidLength = errors.New("xboxone: invalid message body length")
	// ErrConflictingDPad reports two opposite directions on one D-pad axis.
	ErrConflictingDPad = errors.New("xboxone: conflicting d-pad directions")
	// ErrReservedButtonBits reports set bits outside the pinned base-button map.
	ErrReservedButtonBits = errors.New("xboxone: reserved base-button bits are set")
	// ErrGuideRequiresVirtualKey prevents silently dropping Guide from a base
	// input body. Guide travels in a separate virtual-key message.
	ErrGuideRequiresVirtualKey = errors.New("xboxone: guide requires a virtual-key message")
	// ErrShareRequiresExtension prevents inventing the model-dependent Share
	// extension layout.
	ErrShareRequiresExtension = errors.New("xboxone: share requires a captured input extension")
	// ErrInvalidGuideBody reports a non-boolean state or non-Guide key code.
	ErrInvalidGuideBody = errors.New("xboxone: invalid guide virtual-key body")
	// ErrReservedRumbleField reports a non-zero value in the pinned unknown byte.
	ErrReservedRumbleField = errors.New("xboxone: reserved rumble byte is non-zero")
	// ErrInvalidMotorMask reports rumble enable bits outside the four pinned motors.
	ErrInvalidMotorMask = errors.New("xboxone: invalid rumble motor mask")
	// ErrDisabledMotorMagnitude prevents a disabled actuator from retaining a
	// non-zero magnitude in VIIPER's strict semantic representation.
	ErrDisabledMotorMagnitude = errors.New("xboxone: disabled rumble motor has a magnitude")
	// ErrAuthenticationUnavailable is the default authentication result. No
	// caller may mistake an absent provider for successful authentication.
	ErrAuthenticationUnavailable = errors.New("xboxone: authentication provider unavailable")
	// ErrInvalidAuthOutputLength protects the session boundary from a provider
	// returning a count outside the supplied destination.
	ErrInvalidAuthOutputLength = errors.New("xboxone: authentication provider returned an invalid output length")
)

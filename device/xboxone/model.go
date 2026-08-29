package xboxone

import "fmt"

// InputStateVersion is the semantic contract implemented by InputStateV1.
// Versioning the Go type avoids an in-band version field on the controller hot
// path and keeps the zero value a valid neutral state.
const InputStateVersion uint16 = 1

// InputStateV1 is the transport-neutral Xbox controller input model.
//
// Guide and Share are explicit so callers cannot accidentally lose them while
// translating from a physical controller. They are not members of the pinned
// 14-byte base input body; EncodeBaseInputBodyInto rejects them rather than
// silently discarding them.
type InputStateV1 struct {
	Menu bool
	View bool

	A bool
	B bool
	X bool
	Y bool

	DPadUp    bool
	DPadDown  bool
	DPadLeft  bool
	DPadRight bool

	LeftBumper  bool
	RightBumper bool

	LeftStickButton  bool
	RightStickButton bool

	Guide bool
	Share bool

	LeftTrigger  uint16
	RightTrigger uint16

	LeftStickX  int16
	LeftStickY  int16
	RightStickX int16
	RightStickY int16
}

// Validate rejects semantic states that cannot represent one physical D-pad.
func (state InputStateV1) Validate() error {
	if state.DPadUp && state.DPadDown {
		return fmt.Errorf("%w: up and down", ErrConflictingDPad)
	}
	if state.DPadLeft && state.DPadRight {
		return fmt.Errorf("%w: left and right", ErrConflictingDPad)
	}
	return nil
}

// ValidateBaseInputBody rejects states that cannot be represented losslessly
// by the pinned 14-byte base body.
func (state InputStateV1) ValidateBaseInputBody() error {
	if err := state.Validate(); err != nil {
		return err
	}
	if state.Guide {
		return ErrGuideRequiresVirtualKey
	}
	if state.Share {
		return ErrShareRequiresExtension
	}
	return nil
}

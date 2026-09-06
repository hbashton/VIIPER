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
// standard 14-byte input payload; EncodeBaseInputBodyInto rejects them rather than
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

// Validate enforces constraints from MS-GIPUSB 1.0 section 3.1.5.6.1.1. All
// four D-pad bits are independent wire facts; opposing directions are valid at
// this boundary and any SOCD policy belongs in profile mapping.
func (state InputStateV1) Validate() error {
	if state.LeftTrigger > 1023 {
		return fmt.Errorf("%w: left=%d", ErrTriggerOutOfRange, state.LeftTrigger)
	}
	if state.RightTrigger > 1023 {
		return fmt.Errorf("%w: right=%d", ErrTriggerOutOfRange, state.RightTrigger)
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
		return ErrGuideRequiresStatusMessage
	}
	if state.Share {
		return ErrShareRequiresExtension
	}
	return nil
}

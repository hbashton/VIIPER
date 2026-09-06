package xboxone

import (
	"errors"
	"fmt"

	"github.com/Alia5/VIIPER/controllerfeedback"
)

var (
	// ErrInvalidCanonicalFeedbackBinding rejects a broker binding which could
	// not fence one exact persona, physical-device, transport, and ownership
	// lifetime. The binding is supplied by the future outer orchestrator; this
	// dormant adapter never guesses any of those identities.
	ErrInvalidCanonicalFeedbackBinding = errors.New(
		"xboxone: invalid canonical feedback binding")
	// ErrInvalidCanonicalFeedbackExecution rejects a malformed or zero-order
	// persona-local action before it can become a canonical publication.
	ErrInvalidCanonicalFeedbackExecution = errors.New(
		"xboxone: invalid canonical feedback execution")
	// ErrUnsupportedCanonicalFeedbackAction prevents this motor-only frame
	// encoder from silently reclassifying a visual or unrelated obligation.
	// The production executor records Guide LED through its separate local
	// visual-state seam and never sends it through CFBK.
	ErrUnsupportedCanonicalFeedbackAction = errors.New(
		"xboxone: persona action has no canonical motor-feedback representation")
	// ErrUnsupportedCanonicalFeedbackTiming records the exact v1 limitation:
	// CFBK is a latest-state lease, not the Direct Motor delay/repeat program.
	// A later transport-local timing engine may call this adapter only after it
	// has made the program effective; this adapter must not invent that engine.
	ErrUnsupportedCanonicalFeedbackTiming = errors.New(
		"xboxone: direct-motor delay or repeat is not representable by CFBK v1")
	// ErrInvalidCanonicalFeedbackIntent prevents a recoverable output clear
	// (configuration loss or USB reset) from being encoded as the terminal
	// Stop which retires DS4Windows' ownership epoch. The caller must name the
	// exact lifecycle meaning of every local execution.
	ErrInvalidCanonicalFeedbackIntent = errors.New(
		"xboxone: canonical feedback lifecycle intent does not match action")
)

// ControllerPersonaCanonicalFeedbackIntent gives a zero-amplitude frame its
// exact lifecycle meaning. Direct Motor is always StateUpdate. ClearOutputs
// must be classified by the already-authenticated caller as either a
// recoverable neutralization or the final ownership release. This distinction
// cannot be inferred from the byte-identical ControllerPersonaLocalExecution.
type ControllerPersonaCanonicalFeedbackIntent uint8

const (
	ControllerPersonaCanonicalFeedbackIntentInvalid ControllerPersonaCanonicalFeedbackIntent = iota
	ControllerPersonaCanonicalFeedbackStateUpdate
	ControllerPersonaCanonicalFeedbackRecoverableNeutral
	ControllerPersonaCanonicalFeedbackTerminalStop
)

// ControllerPersonaFeedbackBindingV1 binds persona-local output to one exact
// canonical DS4Windows feedback lifetime. PersonaGeneration fences a delayed
// local action from a reset/reconnect. DeviceGeneration and
// TransportGeneration identify the physical target lifetime selected by
// DS4Windows, while OwnershipEpoch identifies this native-game lease.
//
// TimeToLiveMicroseconds is a fail-safe ceiling, not a Direct Motor duration.
// For a non-cancelling effective state the emitted TTL is additionally capped
// at its duration. The production timed executor renews finite zero-delay
// programs and further caps each lease at their absolute CFBK-clock expiry.
type ControllerPersonaFeedbackBindingV1 struct {
	Source                 controllerfeedback.Source
	PersonaGeneration      uint64
	DeviceGeneration       uint64
	TransportGeneration    uint64
	OwnershipEpoch         uint64
	TimeToLiveMicroseconds uint64
}

func (binding ControllerPersonaFeedbackBindingV1) validate() error {
	if (binding.Source != controllerfeedback.SourceXboxOneVirtualDevice &&
		binding.Source != controllerfeedback.SourceXboxSeriesVirtualDevice) ||
		binding.PersonaGeneration == 0 || binding.DeviceGeneration == 0 ||
		binding.TransportGeneration == 0 || binding.OwnershipEpoch == 0 ||
		binding.TimeToLiveMicroseconds == 0 ||
		binding.TimeToLiveMicroseconds >
			controllerfeedback.MaxTimeToLiveMicroseconds {
		return ErrInvalidCanonicalFeedbackBinding
	}
	return nil
}

// ControllerPersonaCanonicalFeedbackFrame maps one already serialized
// persona-local motor or clear action into the single project-wide CFBK v1
// value. It performs no arbitration, physical translation, scheduling, or I/O.
// ControllerPersonaLocalExecution.Order is reused as the non-zero source-local
// sequence. The production timed executor allocates a new publication order
// for each renewal; ownership still belongs to this one canonical binding.
//
// Direct Motor cancellation is Neutral, not Stop: a later host motor command
// in the same ownership epoch may legitimately resume. ClearOutputs is not
// sufficient to choose Neutral versus Stop because the persona uses it for
// both recoverable configuration/reset safety and terminal disconnect. The
// authenticated outer lifecycle supplies intent explicitly.
func ControllerPersonaCanonicalFeedbackFrame(
	binding ControllerPersonaFeedbackBindingV1,
	execution ControllerPersonaLocalExecution,
	timestampMicroseconds uint64,
	intent ControllerPersonaCanonicalFeedbackIntent,
) (controllerfeedback.Frame, error) {
	if err := binding.validate(); err != nil {
		return controllerfeedback.Frame{}, err
	}
	if !execution.valid() {
		return controllerfeedback.Frame{},
			ErrInvalidCanonicalFeedbackExecution
	}
	if execution.Generation != binding.PersonaGeneration {
		return controllerfeedback.Frame{}, fmt.Errorf(
			"%w: persona generation=%d action generation=%d",
			ErrInvalidCanonicalFeedbackBinding,
			binding.PersonaGeneration, execution.Generation)
	}

	frame := controllerfeedback.Frame{
		Version:                controllerfeedback.Version1,
		Source:                 binding.Source,
		Actuators:              controllerfeedback.ActuatorAll,
		Sequence:               execution.Order,
		DeviceGeneration:       binding.DeviceGeneration,
		TransportGeneration:    binding.TransportGeneration,
		OwnershipEpoch:         binding.OwnershipEpoch,
		TimestampMicroseconds:  timestampMicroseconds,
		TimeToLiveMicroseconds: binding.TimeToLiveMicroseconds,
	}

	switch execution.Action {
	case ControllerPersonaApplyDirectMotor:
		if intent != ControllerPersonaCanonicalFeedbackStateUpdate {
			return controllerfeedback.Frame{},
				ErrInvalidCanonicalFeedbackIntent
		}
		body := execution.DirectMotor
		if body.IsCancellation() {
			// Duration zero normatively ignores levels, Delay, and Repeat.
			// Preserve that exact host meaning as a lease-retaining neutral.
			frame.Command = controllerfeedback.CommandNeutral
			break
		}
		if body.Delay != 0 || body.Repeat != 0 {
			return controllerfeedback.Frame{},
				ErrUnsupportedCanonicalFeedbackTiming
		}

		frame.BodyLow = enabledMotorLevel(body.Enabled,
			MotorLeftVibration, body.LeftVibration)
		frame.BodyHigh = enabledMotorLevel(body.Enabled,
			MotorRightVibration, body.RightVibration)
		frame.LeftTrigger = enabledMotorLevel(body.Enabled,
			MotorLeftImpulse, body.LeftImpulse)
		frame.RightTrigger = enabledMotorLevel(body.Enabled,
			MotorRightImpulse, body.RightImpulse)
		if frame.BodyLow == 0 && frame.BodyHigh == 0 &&
			frame.LeftTrigger == 0 && frame.RightTrigger == 0 {
			frame.Command = controllerfeedback.CommandNeutral
		} else {
			frame.Command = controllerfeedback.CommandApply
		}

		// Duration uses 10 ms units. Never let the canonical lease outlive
		// the byte-defined command even when the configured safety TTL is
		// longer. Longer effects require explicit bounded renewal.
		durationMicroseconds := uint64(body.Duration) * 10_000
		if durationMicroseconds < frame.TimeToLiveMicroseconds {
			frame.TimeToLiveMicroseconds = durationMicroseconds
		}

	case ControllerPersonaClearOutputs:
		switch intent {
		case ControllerPersonaCanonicalFeedbackRecoverableNeutral:
			frame.Command = controllerfeedback.CommandNeutral
		case ControllerPersonaCanonicalFeedbackTerminalStop:
			frame.Command = controllerfeedback.CommandStop
		default:
			return controllerfeedback.Frame{},
				ErrInvalidCanonicalFeedbackIntent
		}

	default:
		return controllerfeedback.Frame{},
			ErrUnsupportedCanonicalFeedbackAction
	}

	if err := frame.Validate(); err != nil {
		return controllerfeedback.Frame{}, err
	}
	return frame, nil
}

// EncodeControllerPersonaCanonicalFeedbackInto is the allocation-free wire
// edge. Validation is atomic with respect to dst: every error leaves it
// unchanged. The destination must be exactly one CFBK frame so a local action
// cannot be confused with another broker lane.
func EncodeControllerPersonaCanonicalFeedbackInto(
	dst []byte,
	binding ControllerPersonaFeedbackBindingV1,
	execution ControllerPersonaLocalExecution,
	timestampMicroseconds uint64,
	intent ControllerPersonaCanonicalFeedbackIntent,
) error {
	if len(dst) != controllerfeedback.FrameSize {
		return exactLengthError("canonical controller feedback", len(dst),
			controllerfeedback.FrameSize)
	}
	frame, err := ControllerPersonaCanonicalFeedbackFrame(binding, execution,
		timestampMicroseconds, intent)
	if err != nil {
		return err
	}
	var wire [controllerfeedback.FrameSize]byte
	if err := frame.MarshalTo(wire[:]); err != nil {
		return err
	}
	copy(dst, wire[:])
	return nil
}

func enabledMotorLevel(enabled MotorMask, motor MotorMask, level byte) uint16 {
	if enabled&motor == 0 {
		return 0
	}
	// Direct Motor levels are exact integer percentages. Round to nearest
	// in the normalized 0..65535 CFBK domain; endpoints remain exact.
	return uint16((uint32(level)*65535 + 50) / 100)
}

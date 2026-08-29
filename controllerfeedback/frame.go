// Package controllerfeedback defines the transport-neutral canonical feedback
// contract shared by VIIPER and DS4Windows. It deliberately contains no USB,
// Bluetooth, virtual-device, or physical-controller integration.
package controllerfeedback

import (
	"encoding/binary"
	"errors"
)

const (
	// Version1 is the only accepted CFBK contract version.
	Version1 uint16 = 1
	// FrameSize is the exact encoded size of one CFBK v1 frame.
	FrameSize = 72
	// MaxFutureSkewMicroseconds bounds producer/consumer sampling races in the
	// shared host-monotonic clock domain. A timestamp farther in the future is
	// invalid for application and must drive the same logical release as expiry.
	MaxFutureSkewMicroseconds uint64 = 5_000
	// MaxTimeToLiveMicroseconds bounds a producer's actuator lease. Producers
	// must refresh unchanged effects; an unbounded TTL would defeat fail-safe
	// release after producer failure.
	MaxTimeToLiveMicroseconds uint64 = 250_000

	wireMagic uint32 = 0x4B424643 // Little-endian bytes spell "CFBK".
)

// Source identifies the virtual controller contract which authored feedback.
// It does not identify the eventual physical target.
type Source uint8

const (
	SourceInvalid                    Source = 0
	SourceXboxOneVirtualDevice       Source = 1
	SourceXboxSeriesVirtualDevice    Source = 2
	SourceXbox360VirtualDevice       Source = 3
	SourceDualSenseVirtualDevice     Source = 4
	SourceDualSenseEdgeVirtualDevice Source = 5
	SourceDualShock4VirtualDevice    Source = 6
)

// Valid reports whether source is defined by CFBK v1.
func (source Source) Valid() bool {
	return source >= SourceXboxOneVirtualDevice &&
		source <= SourceDualShock4VirtualDevice
}

// Command gives zero actuator values explicit lifecycle meaning.
type Command uint8

const (
	CommandInvalid Command = 0
	// CommandApply applies one or more non-zero actuator values.
	CommandApply Command = 1
	// CommandNeutral zeros selected actuators while retaining ownership.
	CommandNeutral Command = 2
	// CommandStop zeros selected actuators and retires ownership.
	CommandStop Command = 3
)

// Valid reports whether command is defined by CFBK v1.
func (command Command) Valid() bool {
	return command >= CommandApply && command <= CommandStop
}

// ActuatorMask names the canonical Xbox-semantic channels. CFBK v1 is a full
// four-channel snapshot contract, so every valid frame carries ActuatorAll;
// zero amplitudes represent unsupported or inactive source channels.
type ActuatorMask uint8

const (
	ActuatorNone         ActuatorMask = 0x00
	ActuatorBodyLow      ActuatorMask = 0x01
	ActuatorBodyHigh     ActuatorMask = 0x02
	ActuatorLeftTrigger  ActuatorMask = 0x04
	ActuatorRightTrigger ActuatorMask = 0x08
	ActuatorAll          ActuatorMask = 0x0F
)

// Valid reports whether mask declares the required complete v1 snapshot.
func (mask ActuatorMask) Valid() bool {
	return mask == ActuatorAll
}

var (
	ErrInvalidSize       = errors.New("controller feedback frame size is invalid")
	ErrInvalidMagic      = errors.New("controller feedback frame magic is invalid")
	ErrInvalidVersion    = errors.New("controller feedback frame version is invalid")
	ErrReservedNonZero   = errors.New("controller feedback frame reserved bytes are nonzero")
	ErrInvalidSource     = errors.New("controller feedback frame source is invalid")
	ErrInvalidCommand    = errors.New("controller feedback frame command is invalid")
	ErrInvalidActuators  = errors.New("controller feedback frame actuator mask is invalid")
	ErrInvalidAmplitude  = errors.New("controller feedback frame actuator amplitudes are invalid")
	ErrInvalidSequence   = errors.New("controller feedback frame sequence is invalid")
	ErrInvalidGeneration = errors.New("controller feedback frame lifecycle generation is invalid")
	ErrInvalidTTL        = errors.New("controller feedback frame TTL is invalid")
	ErrNilFrame          = errors.New("controller feedback frame receiver is nil")
)

// Frame is one complete CFBK v1 feedback snapshot. Amplitudes are normalized
// unsigned 0..65535 values; protocol-specific adapters scale only at their
// device boundary. TimestampMicroseconds uses ClockDomainWindowsQPCV1;
// TimeToLiveMicroseconds uses the same converted microsecond unit.
type Frame struct {
	Version                uint16
	Source                 Source
	Command                Command
	Actuators              ActuatorMask
	BodyLow                uint16
	BodyHigh               uint16
	LeftTrigger            uint16
	RightTrigger           uint16
	Sequence               uint64
	DeviceGeneration       uint64
	TransportGeneration    uint64
	OwnershipEpoch         uint64
	TimestampMicroseconds  uint64
	TimeToLiveMicroseconds uint64
}

// Valid reports whether the complete frame satisfies every CFBK v1 invariant.
func (frame Frame) Valid() bool {
	return frame.Validate() == nil
}

// Validate checks the semantic and lifecycle invariants of a CFBK v1 frame.
// It returns package-level sentinel errors and does not allocate.
func (frame Frame) Validate() error {
	if frame.Version != Version1 {
		return ErrInvalidVersion
	}
	if !frame.Source.Valid() {
		return ErrInvalidSource
	}
	if !frame.Command.Valid() {
		return ErrInvalidCommand
	}
	if !frame.Actuators.Valid() {
		return ErrInvalidActuators
	}
	if frame.Sequence == 0 {
		return ErrInvalidSequence
	}
	if frame.DeviceGeneration == 0 || frame.TransportGeneration == 0 ||
		frame.OwnershipEpoch == 0 {
		return ErrInvalidGeneration
	}
	if frame.TimeToLiveMicroseconds == 0 ||
		frame.TimeToLiveMicroseconds > MaxTimeToLiveMicroseconds {
		return ErrInvalidTTL
	}
	if frame.Actuators&ActuatorBodyLow == 0 && frame.BodyLow != 0 ||
		frame.Actuators&ActuatorBodyHigh == 0 && frame.BodyHigh != 0 ||
		frame.Actuators&ActuatorLeftTrigger == 0 && frame.LeftTrigger != 0 ||
		frame.Actuators&ActuatorRightTrigger == 0 && frame.RightTrigger != 0 {
		return ErrInvalidAmplitude
	}

	hasAmplitude := frame.BodyLow != 0 || frame.BodyHigh != 0 ||
		frame.LeftTrigger != 0 || frame.RightTrigger != 0
	if frame.Command == CommandApply {
		if !hasAmplitude {
			return ErrInvalidAmplitude
		}
	} else if hasAmplitude {
		return ErrInvalidAmplitude
	}
	return nil
}

// IsNeutral reports whether this frame explicitly preserves a neutral lease.
func (frame Frame) IsNeutral() bool { return frame.Command == CommandNeutral }

// IsStop reports whether this frame explicitly retires its ownership lease.
func (frame Frame) IsStop() bool { return frame.Command == CommandStop }

// FreshAt reports whether a frame may be applied at now. Expiry is inclusive at
// timestamp-plus-TTL. A bounded future timestamp tolerates only the small race
// between producer and consumer clock samples; a farther-future timestamp is
// rejected so it cannot remain fresh indefinitely.
func (frame Frame) FreshAt(nowMicroseconds uint64) bool {
	if frame.TimestampMicroseconds > nowMicroseconds {
		return frame.TimestampMicroseconds-nowMicroseconds <=
			MaxFutureSkewMicroseconds
	}
	return nowMicroseconds-frame.TimestampMicroseconds <
		frame.TimeToLiveMicroseconds
}

// ExpiredAt reports both ordinary expiry and an invalid far-future timestamp.
func (frame Frame) ExpiredAt(nowMicroseconds uint64) bool {
	return !frame.FreshAt(nowMicroseconds)
}

// MarshalTo writes one complete CFBK v1 frame into the first FrameSize bytes
// of dst. It leaves dst unchanged when the frame or destination is invalid.
func (frame Frame) MarshalTo(dst []byte) error {
	if len(dst) < FrameSize {
		return ErrInvalidSize
	}
	if err := frame.Validate(); err != nil {
		return err
	}

	dst = dst[:FrameSize]
	clear(dst)
	binary.LittleEndian.PutUint32(dst[0:4], wireMagic)
	binary.LittleEndian.PutUint16(dst[4:6], frame.Version)
	binary.LittleEndian.PutUint16(dst[6:8], FrameSize)
	dst[8] = byte(frame.Source)
	dst[9] = byte(frame.Command)
	dst[10] = byte(frame.Actuators)
	binary.LittleEndian.PutUint16(dst[12:14], frame.BodyLow)
	binary.LittleEndian.PutUint16(dst[14:16], frame.BodyHigh)
	binary.LittleEndian.PutUint16(dst[16:18], frame.LeftTrigger)
	binary.LittleEndian.PutUint16(dst[18:20], frame.RightTrigger)
	binary.LittleEndian.PutUint64(dst[24:32], frame.Sequence)
	binary.LittleEndian.PutUint64(dst[32:40], frame.DeviceGeneration)
	binary.LittleEndian.PutUint64(dst[40:48], frame.TransportGeneration)
	binary.LittleEndian.PutUint64(dst[48:56], frame.OwnershipEpoch)
	binary.LittleEndian.PutUint64(dst[56:64], frame.TimestampMicroseconds)
	binary.LittleEndian.PutUint64(dst[64:72], frame.TimeToLiveMicroseconds)
	return nil
}

// UnmarshalFrom decodes exactly one CFBK v1 frame. On every failure it clears
// the receiver, matching DS4Windows TryReadFrom's fail-closed output contract.
func (frame *Frame) UnmarshalFrom(src []byte) error {
	if frame == nil {
		return ErrNilFrame
	}
	*frame = Frame{}
	if len(src) != FrameSize {
		return ErrInvalidSize
	}
	if binary.LittleEndian.Uint32(src[0:4]) != wireMagic {
		return ErrInvalidMagic
	}
	if binary.LittleEndian.Uint16(src[4:6]) != Version1 {
		return ErrInvalidVersion
	}
	if binary.LittleEndian.Uint16(src[6:8]) != FrameSize {
		return ErrInvalidSize
	}
	if src[11] != 0 || src[20] != 0 || src[21] != 0 ||
		src[22] != 0 || src[23] != 0 {
		return ErrReservedNonZero
	}

	candidate := Frame{
		Version:                Version1,
		Source:                 Source(src[8]),
		Command:                Command(src[9]),
		Actuators:              ActuatorMask(src[10]),
		BodyLow:                binary.LittleEndian.Uint16(src[12:14]),
		BodyHigh:               binary.LittleEndian.Uint16(src[14:16]),
		LeftTrigger:            binary.LittleEndian.Uint16(src[16:18]),
		RightTrigger:           binary.LittleEndian.Uint16(src[18:20]),
		Sequence:               binary.LittleEndian.Uint64(src[24:32]),
		DeviceGeneration:       binary.LittleEndian.Uint64(src[32:40]),
		TransportGeneration:    binary.LittleEndian.Uint64(src[40:48]),
		OwnershipEpoch:         binary.LittleEndian.Uint64(src[48:56]),
		TimestampMicroseconds:  binary.LittleEndian.Uint64(src[56:64]),
		TimeToLiveMicroseconds: binary.LittleEndian.Uint64(src[64:72]),
	}
	if err := candidate.Validate(); err != nil {
		return err
	}
	*frame = candidate
	return nil
}

package xboxone

import (
	"errors"
	"sync/atomic"
)

var (
	// ErrControllerDownstreamReceiveIdentity reports a zero generation or
	// receive epoch. This dormant gate has no production issuer; accepting an
	// anonymous lifetime would make copied gates indistinguishable.
	ErrControllerDownstreamReceiveIdentity = errors.New(
		"xboxone: invalid downstream receive framing identity")
	// ErrControllerDownstreamReceiveReplayUnprovable reports any second use of
	// one receive lifetime. MS-GIPUSB does not provide a general downstream
	// replay rule which can distinguish a delayed duplicate from a lawful later
	// command after sequence wrap, so this one-shot seam never guesses.
	ErrControllerDownstreamReceiveReplayUnprovable = errors.New(
		"xboxone: downstream receive replay cannot be proven")
	// ErrControllerDownstreamReceiveCredential reports a forged, stale,
	// copied-after-use, or cross-owner framing credential.
	ErrControllerDownstreamReceiveCredential = errors.New(
		"xboxone: invalid downstream receive framing credential")
)

// ControllerDownstreamReceiveBlocker is the closed reason that a receive
// framing lifetime did not produce a canonical complete-packet credential.
// It deliberately distinguishes protocol work which is not implemented from
// malformed or unsupported complete messages.
type ControllerDownstreamReceiveBlocker uint8

const (
	ControllerDownstreamReceiveNotBlocked ControllerDownstreamReceiveBlocker = iota
	ControllerDownstreamReceiveMalformed
	ControllerDownstreamReceiveUnsupportedMessage
	ControllerDownstreamReceiveReliableFragment
	ControllerDownstreamReceiveExtendedFraming
	ControllerDownstreamReceiveReplayUnprovable
	ControllerDownstreamReceiveCredentialContradiction
)

// ControllerDownstreamReceiveGateState is the monotonic one-shot lifetime of
// DormantControllerDownstreamReceiveFramingGate.
type ControllerDownstreamReceiveGateState uint8

const (
	ControllerDownstreamReceiveGateFresh ControllerDownstreamReceiveGateState = iota + 1
	ControllerDownstreamReceiveGateClaiming
	ControllerDownstreamReceiveGateAccepted
	ControllerDownstreamReceiveGateTaken
	ControllerDownstreamReceiveGateBlocked
	ControllerDownstreamReceiveGateQuarantined
)

// ControllerDownstreamReceiveFramingClaim is an opaque credential for one
// typed ControllerDownstreamPacket held by its exact gate. It contains no raw
// receive bytes and cannot be used with another gate or lifetime.
type ControllerDownstreamReceiveFramingClaim struct {
	owner      *DormantControllerDownstreamReceiveFramingGate
	generation uint64
	epoch      uint64
	token      uint64
	count      uint8
}

func (claim ControllerDownstreamReceiveFramingClaim) Valid() bool {
	return claim.owner != nil && claim.generation != 0 && claim.epoch != 0 &&
		claim.token != 0 && claim.count != 0
}

func (claim ControllerDownstreamReceiveFramingClaim) Generation() uint64 {
	return claim.generation
}

func (claim ControllerDownstreamReceiveFramingClaim) Epoch() uint64 {
	return claim.epoch
}

func (claim ControllerDownstreamReceiveFramingClaim) MessageCount() int {
	return int(claim.count)
}

// ControllerDownstreamReceiveFramingSnapshot exposes only immutable lifetime
// facts. It never exposes the retained canonical packet or source bytes.
type ControllerDownstreamReceiveFramingSnapshot struct {
	Generation   uint64
	Epoch        uint64
	State        ControllerDownstreamReceiveGateState
	Blocker      ControllerDownstreamReceiveBlocker
	MessageCount int
	Quarantined  bool
}

// DormantControllerDownstreamReceiveFramingGate is the smallest truthful
// boundary in front of DecodeControllerDownstreamPacket. One externally
// identified lifetime may accept exactly one complete, non-fragmented packet
// already supported by the canonical decoder. Every second claim is blocked
// as replay-unprovable; there is intentionally no reset or successor issuer.
//
// Rejected input is never retained. Successful input is reduced to the
// fixed-capacity typed ControllerDownstreamPacket, then handed off at most once
// through TakeExactPacket. This type performs no I/O, mapping, output, USB
// registration, fragment reassembly, or production construction.
type DormantControllerDownstreamReceiveFramingGate struct {
	generation uint64
	epoch      uint64
	state      atomic.Uint32
	blocker    atomic.Uint32

	// packet and messageCount are written before the release publication of
	// Accepted and are immutable afterward. Take's successful CAS is the only
	// authority to read packet.
	packet       ControllerDownstreamPacket
	messageCount uint8
}

// NewDormantControllerDownstreamReceiveFramingGate creates one one-shot,
// externally identified receive lifetime. Construction is deliberately not
// wired to a production registry, USB device, or interrupt-OUT adapter.
func NewDormantControllerDownstreamReceiveFramingGate(
	generation uint64,
	epoch uint64,
) (*DormantControllerDownstreamReceiveFramingGate, error) {
	if generation == 0 || epoch == 0 {
		return nil, ErrControllerDownstreamReceiveIdentity
	}
	gate := &DormantControllerDownstreamReceiveFramingGate{
		generation: generation,
		epoch:      epoch,
	}
	gate.state.Store(uint32(ControllerDownstreamReceiveGateFresh))
	return gate, nil
}

// ClaimExactCompletePacket failure-atomically delegates to the existing
// canonical complete-packet decoder. Fragment and extended-header errors are
// surfaced as explicit implementation blockers; no fragment prefix, extended
// header field, unsupported message, malformed bytes, or source slice is kept.
//
// The first caller owns this receive lifetime even when decode rejects it. A
// concurrent or later caller receives ReplayUnprovable rather than getting a
// second chance to reinterpret bytes under the same generation and epoch.
func (gate *DormantControllerDownstreamReceiveFramingGate) ClaimExactCompletePacket(
	wire []byte,
) (ControllerDownstreamReceiveFramingClaim, ControllerDownstreamReceiveBlocker, error) {
	if gate == nil || gate.generation == 0 || gate.epoch == 0 {
		return ControllerDownstreamReceiveFramingClaim{},
			ControllerDownstreamReceiveCredentialContradiction,
			ErrControllerDownstreamReceiveIdentity
	}
	if !gate.state.CompareAndSwap(
		uint32(ControllerDownstreamReceiveGateFresh),
		uint32(ControllerDownstreamReceiveGateClaiming),
	) {
		return ControllerDownstreamReceiveFramingClaim{},
			ControllerDownstreamReceiveReplayUnprovable,
			ErrControllerDownstreamReceiveReplayUnprovable
	}

	packet, err := DecodeControllerDownstreamPacket(wire)
	if err != nil {
		blocker := classifyControllerDownstreamReceiveError(err)
		gate.blocker.Store(uint32(blocker))
		gate.state.Store(uint32(ControllerDownstreamReceiveGateBlocked))
		return ControllerDownstreamReceiveFramingClaim{}, blocker, err
	}

	count := packet.Len()
	// The canonical decoder cannot return a successful empty packet. Preserve
	// that invariant as a terminal dependency contradiction in case it changes.
	if count <= 0 || count > controllerDownstreamPacketMaximumMessages {
		gate.blocker.Store(uint32(
			ControllerDownstreamReceiveCredentialContradiction))
		gate.state.Store(uint32(
			ControllerDownstreamReceiveGateQuarantined))
		return ControllerDownstreamReceiveFramingClaim{},
			ControllerDownstreamReceiveCredentialContradiction,
			ErrControllerDownstreamReceiveCredential
	}

	gate.packet = packet
	gate.messageCount = uint8(count)
	claim := ControllerDownstreamReceiveFramingClaim{
		owner: gate, generation: gate.generation, epoch: gate.epoch,
		token: 1, count: uint8(count),
	}
	gate.state.Store(uint32(ControllerDownstreamReceiveGateAccepted))
	return claim, ControllerDownstreamReceiveNotBlocked, nil
}

// TakeExactPacket consumes the exact accepted credential once and returns the
// fixed-capacity canonical packet by value. A credential contradiction before
// handoff permanently quarantines the gate. Once another caller has taken the
// packet, stale or copied calls cannot revoke or duplicate that handoff.
func (gate *DormantControllerDownstreamReceiveFramingGate) TakeExactPacket(
	claim ControllerDownstreamReceiveFramingClaim,
) (ControllerDownstreamPacket, error) {
	if gate == nil || gate.state.Load() !=
		uint32(ControllerDownstreamReceiveGateAccepted) {
		return ControllerDownstreamPacket{},
			ErrControllerDownstreamReceiveCredential
	}

	exact := claim.Valid() && claim.owner == gate &&
		claim.generation == gate.generation && claim.epoch == gate.epoch &&
		claim.token == 1 && claim.count == gate.messageCount
	if !exact {
		gate.blocker.Store(uint32(
			ControllerDownstreamReceiveCredentialContradiction))
		gate.state.CompareAndSwap(
			uint32(ControllerDownstreamReceiveGateAccepted),
			uint32(ControllerDownstreamReceiveGateQuarantined),
		)
		// packet and messageCount remain immutable after Accepted. Keeping
		// the bounded typed value avoids racing a concurrent exact taker;
		// Quarantined makes it permanently unreachable.
		return ControllerDownstreamPacket{},
			ErrControllerDownstreamReceiveCredential
	}

	if !gate.state.CompareAndSwap(
		uint32(ControllerDownstreamReceiveGateAccepted),
		uint32(ControllerDownstreamReceiveGateTaken),
	) {
		return ControllerDownstreamPacket{},
			ErrControllerDownstreamReceiveCredential
	}
	return gate.packet, nil
}

// Snapshot returns a lock-free diagnostic view. Claiming exposes no partially
// decoded packet; Accepted and Taken expose only the message count.
func (gate *DormantControllerDownstreamReceiveFramingGate) Snapshot() ControllerDownstreamReceiveFramingSnapshot {
	if gate == nil {
		return ControllerDownstreamReceiveFramingSnapshot{}
	}
	state := ControllerDownstreamReceiveGateState(gate.state.Load())
	snapshot := ControllerDownstreamReceiveFramingSnapshot{
		Generation: gate.generation,
		Epoch:      gate.epoch,
		State:      state,
	}
	switch state {
	case ControllerDownstreamReceiveGateAccepted,
		ControllerDownstreamReceiveGateTaken:
		snapshot.MessageCount = int(gate.messageCount)
	case ControllerDownstreamReceiveGateBlocked,
		ControllerDownstreamReceiveGateQuarantined:
		snapshot.Blocker = ControllerDownstreamReceiveBlocker(
			gate.blocker.Load())
	}
	snapshot.Quarantined = state ==
		ControllerDownstreamReceiveGateQuarantined
	return snapshot
}

func classifyControllerDownstreamReceiveError(
	err error,
) ControllerDownstreamReceiveBlocker {
	switch {
	case errors.Is(err, ErrFragmentedHeader):
		return ControllerDownstreamReceiveReliableFragment
	case errors.Is(err, ErrExtendedPayloadLength):
		return ControllerDownstreamReceiveExtendedFraming
	case errors.Is(err, ErrUnsupportedControllerPersonaHostMessage):
		return ControllerDownstreamReceiveUnsupportedMessage
	default:
		return ControllerDownstreamReceiveMalformed
	}
}

// Package inputpresentation defines the transport-neutral hand-off between a
// controller input scheduler and a backend which presents reports to Windows.
package inputpresentation

import "time"

// Outcome is the one terminal disposition of an immutable Claim.
type Outcome uint8

const (
	// OutcomeCommit means the downstream transport accepted and copied the
	// complete report. Only this outcome may advance presented controller state.
	OutcomeCommit Outcome = iota + 1
	// OutcomeDefer means the transport did not accept the report. Ordered work
	// must retain its recovery position and any retried serialized report must
	// remain byte-for-byte identical.
	OutcomeDefer
	// OutcomeRetire means the report belongs to a presentation generation which
	// is ending. It is discarded rather than leaking into the next generation.
	OutcomeRetire
)

// Valid reports whether outcome is a defined terminal disposition.
func (outcome Outcome) Valid() bool {
	return outcome >= OutcomeCommit && outcome <= OutcomeRetire
}

// Claim identifies one immutable report selected from a scheduler. Token is
// unique within Generation. ReceivedAt and SelectedAt let transports and
// deterministic tests measure queue/edge age without reaching into a device's
// scheduler. Ordered marks transition work which must not be coalesced with
// continuous motion.
//
// A valid claim must be resolved exactly once. Source rejects duplicate or
// stale resolutions, including a completion from a retired generation.
type Claim struct {
	Token      uint64
	Generation uint64
	Size       int
	ReceivedAt time.Time
	SelectedAt time.Time
	Ordered    bool
}

// Valid reports whether claim owns a non-empty report in a live generation.
func (claim Claim) Valid() bool {
	return claim.Token != 0 && claim.Generation != 0 && claim.Size > 0
}

// AgeAt returns the non-negative receive-to-boundary age of a claim.
func (claim Claim) AgeAt(boundary time.Time) time.Duration {
	if claim.ReceivedAt.IsZero() || boundary.Before(claim.ReceivedAt) {
		return 0
	}
	return boundary.Sub(claim.ReceivedAt)
}

// Source is implemented by a controller input scheduler. Claim must copy one
// complete immutable report into destination without waiting on transport I/O.
// Resolve is terminal and returns false for an invalid, duplicate, or stale
// claim. RetireGeneration advances an otherwise-idle generation and terminally
// retires an active claim from that generation when one exists.
//
// selectedAt is the transport's report-selection boundary. completedAt is the
// downstream acceptance/failure boundary. Callers should capture each before
// acquiring scheduler locks so contention remains visible in diagnostics.
type Source interface {
	ClaimInputPresentation(destination []byte, selectedAt time.Time) Claim
	ResolveInputPresentation(claim Claim, outcome Outcome, completedAt time.Time) bool
	RetireInputPresentationGeneration(generation uint64, retiredAt time.Time) bool
}

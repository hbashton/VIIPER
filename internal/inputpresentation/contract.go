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

// EndpointSource is the endpoint ownership and lifecycle portion shared by
// Source and PreparedSource. Resolve is terminal and returns false for an
// invalid, duplicate, or stale claim. RetireGeneration advances an otherwise-
// idle generation and terminally retires an active claim from that generation
// when one exists.
//
// OwnsInputPresentationEndpoint makes the semantic source explicitly
// endpoint-scoped. A composite USB device can expose several interrupt-IN
// endpoints; polling an auxiliary endpoint must never consume the main
// controller report journal.
// InputPresentationGeneration lets a backend capture the generation it owns
// when the transport connection is created, then retire exactly that
// generation at its lifecycle boundary without guessing from an active claim.
//
// selectedAt is the transport's report-selection boundary. completedAt is the
// downstream acceptance/failure boundary. Callers should capture each before
// acquiring scheduler locks so contention remains visible in diagnostics.
type EndpointSource interface {
	OwnsInputPresentationEndpoint(endpoint uint8) bool
	InputPresentationGeneration() uint64
	ResolveInputPresentation(claim Claim, outcome Outcome, completedAt time.Time) bool
	RetireInputPresentationGeneration(generation uint64, retiredAt time.Time) bool
}

// Source is implemented by a controller input scheduler which materializes a
// complete immutable report at selection time, before transport serialization.
// PreparedSource is the stricter alternative for sources whose bytes must not
// be copied until the final serialized admission boundary.
type Source interface {
	EndpointSource
	ClaimInputPresentation(destination []byte, selectedAt time.Time) Claim
}

// AdmissionSource is the optional final-boundary extension implemented by a
// Source which can revoke a previously selected Claim before any downstream
// byte is exposed. A transport must call CanAdmitInputPresentation only after
// it owns its output serializer and immediately before the first byte. False
// means none of the claim bytes may be written.
//
// Implementations must be bounded, nonblocking, and must not call back into
// the transport. The method may terminally revoke the claim (for example when
// strict ordered-age policy faults history), so the transport must still issue
// its ordinary non-commit resolution after a rejected admission. Source may
// reject that resolution when revocation already consumed the claim.
type AdmissionSource interface {
	Source
	CanAdmitInputPresentation(claim Claim, admittedAt time.Time) bool
}

// PreparedSource is an optional interrupt-IN presentation contract. Selection
// returns only an immutable capability and performs no report copy. available
// false means no claim was selected and therefore requires no resolution. If
// available is true, the claim must be valid, generation-bound, and no larger
// than maximumSize; a malformed result is a transport-fatal contract breach.
//
// The transport calls AdmitAndCopyInputPresentation exactly once for a
// well-formed selected claim, only while it owns both response serialization
// and the endpoint's final cancellation fence. destination has length and
// capacity exactly Claim.Size. False must leave the claim unadmitted and must
// not make an external effect visible; no destination bytes are emitted.
// Implementations must be bounded, nonblocking, and must not call back into the
// transport.
//
// Every available selection is followed by exactly one transport call to
// ResolveInputPresentation: Commit after a complete write and flush, Defer on
// admission or delivery failure, or Retire when endpoint/lifecycle ownership
// is lost before admission. A deferred source must preserve the identical
// immutable retry. Existing Source and AdmissionSource behavior is unchanged.
type PreparedSource interface {
	EndpointSource
	SelectInputPresentation(maximumSize int, selectedAt time.Time) (
		claim Claim, available bool,
	)
	AdmitAndCopyInputPresentation(
		claim Claim, destination []byte, admittedAt time.Time,
	) bool
}

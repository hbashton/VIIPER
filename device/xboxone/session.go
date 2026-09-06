package xboxone

import "context"

// SequenceOutcome reports the backend result for one sequence claim.
type SequenceOutcome uint8

const (
	SequenceDelivered SequenceOutcome = iota + 1
	SequenceDeferred
	SequenceDeliveryFailed
)

// SequenceClaim is an opaque capability for one immutable sequence value.
type SequenceClaim struct {
	owner      *SequenceCounter
	token      uint64
	generation uint64
	value      uint8
}

func (claim SequenceClaim) Valid() bool        { return claim.owner != nil && claim.token != 0 }
func (claim SequenceClaim) Generation() uint64 { return claim.generation }
func (claim SequenceClaim) Value() uint8       { return claim.value }

// SequenceCounter transactionally allocates the non-zero uint8 sequence
// domain defined by MS-GIPUSB 1.0 section 2.2.10.3. The zero value starts in
// generation one; 255 wraps to one.
//
// Each instance represents exactly one sequence pool. Callers must maintain
// separate instances for the global pool and every message-type-specific
// unique pool; in particular Direct Motor and Gamepad Input do not share one.
// This is an egress allocator only, not a receive replay window. Calls to one
// counter must be serialized; SequenceCounter is not safe for concurrent use.
// A value advances only after claim, final admission, and delivered resolution.
// A counter must not be copied after its first method call because claims are
// bound to its address.
type SequenceCounter struct {
	last          uint8
	generation    uint64
	nextToken     uint64
	hasClaim      bool
	claimAdmitted bool
	claimToken    uint64
	claimValue    uint8
	retryPending  bool
	retryValue    uint8
}

func (counter SequenceCounter) effectiveGeneration() uint64 {
	if counter.generation == 0 {
		return 1
	}
	return counter.generation
}

// Generation returns the current reset fence. The zero value is generation one.
func (counter SequenceCounter) Generation() uint64 { return counter.effectiveGeneration() }

// LastCommitted returns the last successfully delivered value, or zero before
// the first delivery in a generation.
func (counter SequenceCounter) LastCommitted() uint8 { return counter.last }

func (counter *SequenceCounter) nextClaimToken() uint64 {
	counter.nextToken++
	if counter.nextToken == 0 {
		counter.nextToken++
	}
	return counter.nextToken
}

func (counter *SequenceCounter) makeClaim(value uint8) SequenceClaim {
	token := counter.nextClaimToken()
	counter.hasClaim = true
	counter.claimAdmitted = false
	counter.claimToken = token
	counter.claimValue = value
	return SequenceClaim{
		owner: counter, token: token,
		generation: counter.effectiveGeneration(), value: value,
	}
}

// Claim selects the next value without advancing the committed sequence.
func (counter *SequenceCounter) Claim() (SequenceClaim, error) {
	if counter.hasClaim {
		return SequenceClaim{}, ErrSequenceClaimOutstanding
	}
	if counter.retryPending {
		return SequenceClaim{}, ErrSequenceRetryRequired
	}
	value := counter.last + 1
	if value == 0 {
		value = 1
	}
	return counter.makeClaim(value), nil
}

// ClaimRetry returns the exact value retained after defer or delivery failure.
func (counter *SequenceCounter) ClaimRetry() (SequenceClaim, error) {
	if counter.hasClaim {
		return SequenceClaim{}, ErrSequenceClaimOutstanding
	}
	if !counter.retryPending {
		return SequenceClaim{}, ErrSequenceRetryRequired
	}
	value := counter.retryValue
	claim := counter.makeClaim(value)
	counter.retryPending = false
	return claim, nil
}

func (counter *SequenceCounter) validateClaim(claim SequenceClaim) error {
	if !counter.hasClaim || claim.owner != counter || claim.token == 0 ||
		claim.token != counter.claimToken || claim.generation != counter.effectiveGeneration() ||
		claim.value != counter.claimValue {
		return ErrInvalidSequenceClaim
	}
	return nil
}

// CanAdmit performs the final serialized fence immediately before publication.
func (counter *SequenceCounter) CanAdmit(claim SequenceClaim) (bool, error) {
	if err := counter.validateClaim(claim); err != nil {
		return false, err
	}
	if counter.claimAdmitted {
		return false, ErrInvalidSequenceClaim
	}
	counter.claimAdmitted = true
	return true, nil
}

// Resolve advances only an admitted Delivered claim. Non-delivery retains the
// exact value as a mandatory retry.
func (counter *SequenceCounter) Resolve(claim SequenceClaim, outcome SequenceOutcome) error {
	if err := counter.validateClaim(claim); err != nil {
		return err
	}
	if outcome != SequenceDelivered && outcome != SequenceDeferred &&
		outcome != SequenceDeliveryFailed {
		return ErrInvalidSequenceOutcome
	}
	if outcome == SequenceDelivered && !counter.claimAdmitted {
		return ErrSequenceClaimNotAdmitted
	}
	if outcome == SequenceDelivered {
		counter.last = counter.claimValue
	} else {
		counter.retryValue = counter.claimValue
		counter.retryPending = true
	}
	counter.hasClaim = false
	counter.claimAdmitted = false
	counter.claimToken = 0
	counter.claimValue = 0
	return nil
}

// Reset invalidates every old claim/retry and restarts at one under the strict
// non-zero successor generation.
func (counter *SequenceCounter) Reset(successorGeneration uint64) error {
	current := counter.effectiveGeneration()
	if current == ^uint64(0) || successorGeneration == 0 ||
		successorGeneration != current+1 {
		return ErrInvalidTransferGeneration
	}
	nextToken := counter.nextToken
	*counter = SequenceCounter{generation: successorGeneration, nextToken: nextToken}
	return nil
}

// AuthProvider is the narrow, transport-neutral boundary for an independently
// supplied Xbox authentication implementation. packet and dst are opaque auth
// bodies; this package defines no handshake, certificate, or key semantics.
// Providers may retain handshake state between calls.
type AuthProvider interface {
	ProcessAuthenticationPacket(
		ctx context.Context,
		dst []byte,
		packet []byte,
	) (written int, complete bool, err error)
}

// UnavailableAuthProvider is the fail-closed default.
type UnavailableAuthProvider struct{}

// ProcessAuthenticationPacket always reports ErrAuthenticationUnavailable.
func (UnavailableAuthProvider) ProcessAuthenticationPacket(
	context.Context,
	[]byte,
	[]byte,
) (int, bool, error) {
	return 0, false, ErrAuthenticationUnavailable
}

// Session owns only the replaceable authentication boundary. Sequence pools
// remain explicit, caller-owned SequenceCounter instances so distinct pools
// cannot be accidentally collapsed into a single session counter.
type Session struct {
	auth AuthProvider
}

// NewSession creates a session. A nil provider selects the unavailable,
// fail-closed implementation.
func NewSession(provider AuthProvider) Session {
	if provider == nil {
		provider = UnavailableAuthProvider{}
	}
	return Session{auth: provider}
}

// ProcessAuthenticationPacket delegates an opaque auth packet and verifies
// that the provider's reported output is inside dst. A zero-value Session also
// fails closed. dst is fully defined on return: it is cleared on error and its
// unused tail is cleared after a successful partial write.
func (session *Session) ProcessAuthenticationPacket(
	ctx context.Context,
	dst []byte,
	packet []byte,
) (int, bool, error) {
	provider := session.auth
	if provider == nil {
		provider = UnavailableAuthProvider{}
	}
	written, complete, err := provider.ProcessAuthenticationPacket(ctx, dst, packet)
	if err != nil {
		clear(dst)
		return 0, false, err
	}
	if written < 0 || written > len(dst) {
		clear(dst)
		return 0, false, ErrInvalidAuthOutputLength
	}
	clear(dst[written:])
	return written, complete, nil
}

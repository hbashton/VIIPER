package xboxone

import "context"

// SequenceCounter emits the non-zero uint8 sequence domain observed for GIP
// data packets. The zero value starts at one and 255 wraps to one.
//
// This is an egress allocator only. It deliberately does not claim a replay or
// receive-window policy, which remains unproven without owned captures.
type SequenceCounter struct {
	last uint8
}

// Next returns the next non-zero sequence value.
func (counter *SequenceCounter) Next() uint8 {
	counter.last++
	if counter.last == 0 {
		counter.last = 1
	}
	return counter.last
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

// Session groups only the pre-capture state whose behavior is pinned: a
// non-zero egress sequence allocator and a replaceable authentication boundary.
// It is caller-owned and not safe for concurrent use.
type Session struct {
	sequences SequenceCounter
	auth      AuthProvider
}

// NewSession creates a session. A nil provider selects the unavailable,
// fail-closed implementation.
func NewSession(provider AuthProvider) Session {
	if provider == nil {
		provider = UnavailableAuthProvider{}
	}
	return Session{auth: provider}
}

// NextSequence returns the next non-zero egress sequence value.
func (session *Session) NextSequence() uint8 {
	return session.sequences.Next()
}

// ProcessAuthenticationPacket delegates an opaque auth packet and verifies
// that the provider's reported output is inside dst. A zero-value Session also
// fails closed.
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
		return 0, false, err
	}
	if written < 0 || written > len(dst) {
		return 0, false, ErrInvalidAuthOutputLength
	}
	return written, complete, nil
}

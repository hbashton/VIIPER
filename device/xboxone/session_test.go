package xboxone

import (
	"context"
	"errors"
	"testing"
)

type authProviderFunc func(context.Context, []byte, []byte) (int, bool, error)

func (fn authProviderFunc) ProcessAuthenticationPacket(
	ctx context.Context,
	dst []byte,
	packet []byte,
) (int, bool, error) {
	return fn(ctx, dst, packet)
}

func TestSequenceCounterNeverEmitsZeroAndWraps(t *testing.T) {
	var counter SequenceCounter
	for want := 1; want <= 255; want++ {
		if got := counter.Next(); got != uint8(want) {
			t.Fatalf("Next at %d = %d, want %d", want, got, want)
		}
	}
	if got := counter.Next(); got != 1 {
		t.Fatalf("Next after wrap = %d, want 1", got)
	}
}

func TestSessionAuthenticationDefaultsFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		session Session
	}{
		{name: "zero value"},
		{name: "nil provider", session: NewSession(nil)},
		{name: "explicit unavailable", session: NewSession(UnavailableAuthProvider{})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dst := make([]byte, 8)
			written, complete, err := test.session.ProcessAuthenticationPacket(
				context.Background(), dst, []byte{1, 2, 3})
			if written != 0 || complete || !errors.Is(err, ErrAuthenticationUnavailable) {
				t.Fatalf("got (%d, %t, %v), want (0, false, ErrAuthenticationUnavailable)",
					written, complete, err)
			}
		})
	}
}

func TestSessionDelegatesAuthenticationWithoutInterpretingPacket(t *testing.T) {
	provider := authProviderFunc(func(
		ctx context.Context,
		dst []byte,
		packet []byte,
	) (int, bool, error) {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		if string(packet) != "opaque" {
			t.Fatalf("packet = %q, want opaque", packet)
		}
		copy(dst, "ok")
		return 2, true, nil
	})
	session := NewSession(provider)
	dst := make([]byte, 2)
	written, complete, err := session.ProcessAuthenticationPacket(
		context.Background(), dst, []byte("opaque"))
	if err != nil {
		t.Fatalf("ProcessAuthenticationPacket: %v", err)
	}
	if written != 2 || !complete || string(dst) != "ok" {
		t.Fatalf("got (%d, %t, %q), want (2, true, ok)", written, complete, dst)
	}
}

func TestSessionRejectsProviderOutputOutsideDestination(t *testing.T) {
	for _, written := range []int{-1, 2} {
		provider := authProviderFunc(func(context.Context, []byte, []byte) (int, bool, error) {
			return written, true, nil
		})
		session := NewSession(provider)
		gotWritten, complete, err := session.ProcessAuthenticationPacket(
			context.Background(), make([]byte, 1), nil)
		if gotWritten != 0 || complete || !errors.Is(err, ErrInvalidAuthOutputLength) {
			t.Errorf("provider count %d: got (%d, %t, %v)",
				written, gotWritten, complete, err)
		}
	}
}

func TestSessionSequenceUsesPinnedCounter(t *testing.T) {
	session := NewSession(nil)
	if first, second := session.NextSequence(), session.NextSequence(); first != 1 || second != 2 {
		t.Fatalf("sequences = (%d, %d), want (1, 2)", first, second)
	}
}

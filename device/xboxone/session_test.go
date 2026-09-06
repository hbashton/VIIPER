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

func TestSequenceCounterCommitsOnlyDeliveredClaimsAndWraps(t *testing.T) {
	var counter SequenceCounter
	for want := 1; want <= 255; want++ {
		claim, err := counter.Claim()
		if err != nil {
			t.Fatal(err)
		}
		if got := claim.Value(); got != uint8(want) {
			t.Fatalf("Claim at %d = %d, want %d", want, got, want)
		}
		if got := counter.LastCommitted(); got != uint8(want-1) {
			t.Fatalf("pre-resolution committed at %d = %d", want, got)
		}
		if ok, err := counter.CanAdmit(claim); err != nil || !ok {
			t.Fatalf("CanAdmit at %d = (%t, %v)", want, ok, err)
		}
		if got := counter.LastCommitted(); got != uint8(want-1) {
			t.Fatalf("admission consumed sequence at %d = %d", want, got)
		}
		if err := counter.Resolve(claim, SequenceDelivered); err != nil {
			t.Fatal(err)
		}
	}
	claim, err := counter.Claim()
	if err != nil || claim.Value() != 1 {
		t.Fatalf("Claim after wrap = (%d, %v), want (1, nil)", claim.Value(), err)
	}
}

func TestSequenceCounterDeferredFailureRetryAndResetFence(t *testing.T) {
	var counter SequenceCounter
	claim, err := counter.Claim()
	if err != nil {
		t.Fatal(err)
	}
	if err := counter.Resolve(claim, SequenceDeferred); err != nil {
		t.Fatal(err)
	}
	if counter.LastCommitted() != 0 {
		t.Fatal("defer advanced sequence")
	}
	if _, err := counter.Claim(); !errors.Is(err, ErrSequenceRetryRequired) {
		t.Fatalf("new claim before retry error = %v", err)
	}
	retry, err := counter.ClaimRetry()
	if err != nil || retry.Value() != claim.Value() || retry.Generation() != claim.Generation() {
		t.Fatalf("retry = (%+v, %v), want exact value/generation", retry, err)
	}
	if err := counter.Resolve(retry, SequenceDeliveryFailed); err != nil {
		t.Fatal(err)
	}
	retry, err = counter.ClaimRetry()
	if err != nil || retry.Value() != 1 {
		t.Fatalf("write-failure retry = (%d, %v)", retry.Value(), err)
	}
	if ok, err := counter.CanAdmit(retry); err != nil || !ok {
		t.Fatalf("retry admission = (%t, %v)", ok, err)
	}
	if err := counter.Resolve(retry, SequenceDelivered); err != nil {
		t.Fatal(err)
	}
	old, err := counter.Claim()
	if err != nil {
		t.Fatal(err)
	}
	if err := counter.Reset(2); err != nil {
		t.Fatal(err)
	}
	if counter.Generation() != 2 || counter.LastCommitted() != 0 {
		t.Fatalf("reset counter = generation %d last %d", counter.Generation(), counter.LastCommitted())
	}
	if _, err := counter.CanAdmit(old); !errors.Is(err, ErrInvalidSequenceClaim) {
		t.Fatalf("predecessor claim error = %v", err)
	}
	if err := counter.Reset(2); !errors.Is(err, ErrInvalidTransferGeneration) {
		t.Fatalf("non-successor reset error = %v", err)
	}
}

func TestSequenceCountersRemainIndependentPerPool(t *testing.T) {
	var directMotor SequenceCounter
	var gamepadInput SequenceCounter
	first, err := directMotor.Claim()
	if err != nil || first.Value() != 1 {
		t.Fatalf("direct motor first = (%d, %v), want (1, nil)", first.Value(), err)
	}
	if ok, err := directMotor.CanAdmit(first); err != nil || !ok {
		t.Fatal(err)
	}
	if err := directMotor.Resolve(first, SequenceDelivered); err != nil {
		t.Fatal(err)
	}
	second, err := directMotor.Claim()
	if err != nil || second.Value() != 2 {
		t.Fatalf("direct motor second = (%d, %v), want (2, nil)", second.Value(), err)
	}
	independent, err := gamepadInput.Claim()
	if err != nil || independent.Value() != 1 {
		t.Fatalf("gamepad input first = (%d, %v), want independent 1", independent.Value(), err)
	}
}

func TestSequenceCounterHotPathAllocatesZero(t *testing.T) {
	if allocs := testing.AllocsPerRun(1000, func() {
		var counter SequenceCounter
		claim, err := counter.Claim()
		if err != nil {
			panic(err)
		}
		if ok, err := counter.CanAdmit(claim); err != nil || !ok {
			panic("admission")
		}
		if err := counter.Resolve(claim, SequenceDelivered); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("sequence hot-path allocations = %v, want 0", allocs)
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
			for index := range dst {
				dst[index] = 0xa5
			}
			written, complete, err := test.session.ProcessAuthenticationPacket(
				context.Background(), dst, []byte{1, 2, 3})
			if written != 0 || complete || !errors.Is(err, ErrAuthenticationUnavailable) {
				t.Fatalf("got (%d, %t, %v), want (0, false, ErrAuthenticationUnavailable)",
					written, complete, err)
			}
			if got := string(dst); got != "\x00\x00\x00\x00\x00\x00\x00\x00" {
				t.Fatalf("dst after failure = % x, want cleared", dst)
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
		provider := authProviderFunc(func(_ context.Context, dst []byte, _ []byte) (int, bool, error) {
			dst[0] = 0xa5
			return written, true, nil
		})
		session := NewSession(provider)
		dst := []byte{0xff}
		gotWritten, complete, err := session.ProcessAuthenticationPacket(
			context.Background(), dst, nil)
		if gotWritten != 0 || complete || !errors.Is(err, ErrInvalidAuthOutputLength) {
			t.Errorf("provider count %d: got (%d, %t, %v)",
				written, gotWritten, complete, err)
		}
		if dst[0] != 0 {
			t.Errorf("provider count %d: dst = % x, want cleared", written, dst)
		}
	}
}

func TestSessionClearsProviderMutationOnError(t *testing.T) {
	wantErr := errors.New("malicious provider error")
	provider := authProviderFunc(func(_ context.Context, dst []byte, _ []byte) (int, bool, error) {
		for index := range dst {
			dst[index] = 0xa5
		}
		return len(dst), true, wantErr
	})
	session := NewSession(provider)
	dst := []byte{1, 2, 3, 4}
	written, complete, err := session.ProcessAuthenticationPacket(context.Background(), dst, nil)
	if written != 0 || complete || !errors.Is(err, wantErr) {
		t.Fatalf("got (%d, %t, %v), want (0, false, %v)",
			written, complete, err, wantErr)
	}
	if got := dst; got[0] != 0 || got[1] != 0 || got[2] != 0 || got[3] != 0 {
		t.Fatalf("dst = % x, want cleared", got)
	}
}

func TestSessionClearsUnusedTailAfterPartialSuccess(t *testing.T) {
	provider := authProviderFunc(func(_ context.Context, dst []byte, _ []byte) (int, bool, error) {
		copy(dst, "ok")
		return 2, true, nil
	})
	session := NewSession(provider)
	dst := []byte{0xff, 0xff, 0xff, 0xff, 0xff}
	written, complete, err := session.ProcessAuthenticationPacket(context.Background(), dst, nil)
	if err != nil || written != 2 || !complete {
		t.Fatalf("got (%d, %t, %v), want (2, true, nil)", written, complete, err)
	}
	want := []byte{'o', 'k', 0, 0, 0}
	for index := range want {
		if dst[index] != want[index] {
			t.Fatalf("dst = % x, want % x", dst, want)
		}
	}
}

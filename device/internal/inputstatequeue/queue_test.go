package inputstatequeue

import (
	"context"
	"errors"
	"testing"
	"time"
)

type testState struct {
	edge   uint64
	analog int
}

func TestQueuePreservesEdgesAndCoalescesLatestSnapshot(t *testing.T) {
	q := New(testState{}, 0, 4)
	for _, state := range []testState{
		{edge: 1, analog: 10},
		{edge: 1, analog: 20},
		{edge: 0, analog: 30},
	} {
		if err := q.Publish(state, state.edge); err != nil {
			t.Fatal(err)
		}
	}

	state, transition, err := q.take(false)
	if err != nil || !transition || state.edge != 1 || state.analog != 10 {
		t.Fatalf("press=(%+v,%t,%v)", state, transition, err)
	}
	state, transition, err = q.take(false)
	if err != nil || !transition || state.edge != 0 || state.analog != 30 {
		t.Fatalf("release=(%+v,%t,%v)", state, transition, err)
	}
	state, transition, err = q.take(false)
	if err != nil || transition || state.edge != 0 || state.analog != 30 {
		t.Fatalf("latest=(%+v,%t,%v)", state, transition, err)
	}
}

func TestQueueDeadlineConsumptionDrainsStaleWakeToken(t *testing.T) {
	q := New(testState{}, 0, 2)
	if err := q.Publish(testState{analog: 7}, 0); err != nil {
		t.Fatal(err)
	}
	state, transition, err := q.take(true)
	if err != nil || transition || state.analog != 7 {
		t.Fatalf("deadline take=(%+v,%t,%v)", state, transition, err)
	}
	select {
	case <-q.signal:
		t.Fatal("deadline consumption left a stale immediate wake token")
	default:
	}
}

func TestQueueInvalidationCancelsBlockedGeneration(t *testing.T) {
	q := New(testState{}, 0, 1)
	if err := q.Publish(testState{edge: 1}, 1); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- q.PublishUntil(nil, testState{edge: 2}, 2)
	}()

	select {
	case err := <-result:
		t.Fatalf("blocked publication returned early: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	q.Invalidate()
	select {
	case err := <-result:
		if !errors.Is(err, ErrGenerationChanged) {
			t.Fatalf("generation result=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("generation invalidation did not release producer")
	}

	deadline := make(chan time.Time, 1)
	deadline <- time.Now()
	state, transition, err := q.Wait(context.Background(), deadline)
	if err != nil || transition || state.edge != 1 {
		t.Fatalf("post-boundary latest=(%+v,%t,%v)", state, transition, err)
	}
}

func TestQueueCloseCancelsBlockedProducer(t *testing.T) {
	q := New(testState{}, 0, 1)
	if err := q.Publish(testState{edge: 1}, 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- q.PublishUntil(done, testState{edge: 2}, 2)
	}()
	close(done)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("close result=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream close did not release producer")
	}
}

func TestQueueBoundsBackpressureWhenPeerCloseIsUnobservable(t *testing.T) {
	q := New(testState{}, 0, 1)
	q.backpressureTimeout = 10 * time.Millisecond
	if err := q.Publish(testState{edge: 1}, 1); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err := q.PublishUntil(make(chan struct{}), testState{edge: 2}, 2)
	if !errors.Is(err, ErrBackpressureTimeout) {
		t.Fatalf("backpressure result=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("bounded backpressure did not release the stream handler")
	}
}

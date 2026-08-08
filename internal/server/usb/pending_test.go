package usb

import (
	"context"
	"sync"
	"testing"
)

// Exactly one party may reply for a given seqnum. USBIP_CMD_UNLINK and the async IN
// completion goroutines both race to claim it, and claimPending is what decides.

func TestClaimPendingGrantsOwnershipToExactlyOneCaller(t *testing.T) {
	var mu sync.Mutex
	pending := map[uint32]context.CancelFunc{}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	pending[7] = cancel

	if _, owned := claimPending(&mu, pending, 7); !owned {
		t.Fatal("first claim must win")
	}
	if _, owned := claimPending(&mu, pending, 7); owned {
		t.Fatal("second claim must lose: two replies for one seqnum is the bug")
	}
	if _, present := pending[7]; present {
		t.Fatal("a claimed seqnum must be removed from the table")
	}
}

func TestClaimPendingReturnsTheCancelFuncToTheWinner(t *testing.T) {
	// UNLINK needs the cancel func, and only when it actually won the claim -
	// cancelling a request somebody else already completed is not harmless.
	var mu sync.Mutex
	pending := map[uint32]context.CancelFunc{}
	cancelled := false
	pending[3] = func() { cancelled = true }

	cancel, owned := claimPending(&mu, pending, 3)
	if !owned || cancel == nil {
		t.Fatal("winner must receive the cancel func")
	}
	cancel()
	if !cancelled {
		t.Fatal("the returned cancel func must be the stored one")
	}

	if cancel, owned := claimPending(&mu, pending, 3); owned || cancel != nil {
		t.Fatal("loser must receive neither ownership nor a cancel func")
	}
}

func TestClaimPendingOnUnknownSeqnum(t *testing.T) {
	// A completion for a seqnum that was never registered, or an UNLINK for one that
	// already completed, must both report "not mine" rather than panicking.
	var mu sync.Mutex
	pending := map[uint32]context.CancelFunc{}
	if cancel, owned := claimPending(&mu, pending, 99); owned || cancel != nil {
		t.Fatal("unknown seqnum must not be claimable")
	}
}

func TestClaimPendingIsRaceFree(t *testing.T) {
	// The real interleaving: many goroutines contend for the same seqnum, as UNLINK
	// and a completion do. Exactly one may come away with the right to reply.
	// Meaningful under -race, which the repo's CI runs.
	const contenders = 64
	for round := 0; round < 200; round++ {
		var mu sync.Mutex
		pending := map[uint32]context.CancelFunc{}
		_, cancel := context.WithCancel(context.Background())
		pending[1] = cancel

		var wins int64
		var winsMu sync.Mutex
		var start, done sync.WaitGroup
		start.Add(1)
		done.Add(contenders)
		for i := 0; i < contenders; i++ {
			go func() {
				defer done.Done()
				start.Wait()
				if _, owned := claimPending(&mu, pending, 1); owned {
					winsMu.Lock()
					wins++
					winsMu.Unlock()
				}
			}()
		}
		start.Done()
		done.Wait()
		cancel()

		if wins != 1 {
			t.Fatalf("round %d: %d winners, want exactly 1", round, wins)
		}
	}
}

package usb

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
	"github.com/stretchr/testify/require"
)

type scriptedRetirementImportOwner struct {
	*scriptedImportSessionOwner
	retirementMu    sync.Mutex
	request         retainedusb.ImportRetirementRequest
	queryErr        error
	queryPanic      bool
	retireOnPrepare bool
	retirePrepared  atomic.Bool
}

func newScriptedRetirementImportOwner() *scriptedRetirementImportOwner {
	return &scriptedRetirementImportOwner{scriptedImportSessionOwner: newScriptedCombinedImportOwner(0x8d01, 2)}
}

func (owner *scriptedRetirementImportOwner) RetainedImportRetirement() (retainedusb.ImportRetirementRequest, error) {
	owner.retirementMu.Lock()
	defer owner.retirementMu.Unlock()
	if owner.queryPanic {
		panic("retirement query failed")
	}
	return owner.request, owner.queryErr
}

func (owner *scriptedRetirementImportOwner) publishRetirement(mutate func(*retainedusb.ImportRetirementRequest)) {
	owner.mu.Lock()
	lease := owner.boundLease
	owner.mu.Unlock()
	request := retainedusb.ImportRetirementRequest{Lease: lease, Reason: retainedusb.ImportRetirementInputHistoryOverflow}
	if mutate != nil {
		mutate(&request)
	}
	owner.retirementMu.Lock()
	owner.request = request
	owner.retirementMu.Unlock()
	owner.advanceReadiness()
}

func (owner *scriptedRetirementImportOwner) Prepare(ticket retainedusb.Ticket, destination []byte, now time.Time) (retainedusb.Preparation, error) {
	if owner.retireOnPrepare && owner.retirePrepared.CompareAndSwap(false, true) {
		// The returned Pending already observes the new epoch. Only the
		// post-readiness retirement query can prevent this wake being swallowed.
		owner.publishRetirement(nil)
	}
	return owner.scriptedImportSessionOwner.Prepare(ticket, destination, now)
}

func awaitRetirementScheduler(t *testing.T, scheduler *retainedSubmissionScheduler) {
	t.Helper()
	select {
	case <-scheduler.done:
	case <-time.After(time.Second):
		t.Fatal("retirement did not terminate the scheduler without another URB")
	}
}

func assertRetirementSafeClose(t *testing.T, session *retainedImportSession, lease retainedusb.ImportLease, scheduler *retainedSubmissionScheduler, owner *scriptedRetirementImportOwner) {
	t.Helper()
	reason, diagnostic := retainedReadFailure(context.Background(), scheduler, io.EOF)
	require.Equal(t, retainedusb.ImportCloseOwnerRequested, reason)
	require.ErrorContains(t, diagnostic, "input presentation history overflow")
	require.NoError(t, scheduler.close(), "the diagnostic must not masquerade as a failed ownership drain")
	result, err := session.close(lease, reason, time.Now().Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, retainedusb.ImportDisconnectSafe, result.State)
	require.True(t, session.released.Load())
	require.False(t, session.quarantined.Load())
	require.Equal(t, uint32(1), owner.drainCalls.Load())
	require.Equal(t, uint32(1), owner.disconnectCalls.Load())
	require.False(t, session.reservation.admission.open.Load())
	require.Equal(t, retainedImportActivationRevoked, session.reservation.admission.activation.Load())
}

func TestRetainedOwnerRetirementClosesWithoutQueuedURB(t *testing.T) {
	owner := newScriptedRetirementImportOwner()
	session, lease, scheduler := newRetainedResetContainmentScheduler(t, owner)
	owner.publishRetirement(nil)
	awaitRetirementScheduler(t, scheduler)
	require.Zero(t, owner.prepareCalls.Load())
	assertRetirementSafeClose(t, session, lease, scheduler, owner)
}

func TestRetainedOwnerRetirementIdleQueryAllocatesZero(t *testing.T) {
	owner := newScriptedRetirementImportOwner()
	_, _, scheduler := newRetainedResetContainmentScheduler(t, owner)
	require.Zero(t, testing.AllocsPerRun(1000, func() {
		if err := scheduler.pollImportRetirement(); err != nil {
			panic(err)
		}
	}))
}

func TestRetainedOwnerRetirementRetiresAllExactPendingTickets(t *testing.T) {
	owner := newScriptedRetirementImportOwner()
	session, lease, scheduler := newRetainedResetContainmentScheduler(t, owner)
	require.NoError(t, scheduler.enqueue(retainedControlInEnvelope(0x8d02, 8)))
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(0x8d03)))
	require.NoError(t, scheduler.enqueue(retainedInterruptOutEnvelope(0x8d04, []byte{1})))
	require.Eventually(t, func() bool { return owner.prepareCalls.Load() == 3 }, time.Second, time.Millisecond)
	owner.publishRetirement(nil)
	awaitRetirementScheduler(t, scheduler)
	owner.scriptedRetainedOwner.mu.Lock()
	retirements := append([]retainedOwnerRetirement(nil), owner.retirements...)
	owner.scriptedRetainedOwner.mu.Unlock()
	require.Len(t, retirements, 3)
	seen := make(map[retainedusb.Ticket]bool)
	for _, retirement := range retirements {
		require.Equal(t, retainedusb.RetireOwnerRequested, retirement.reason)
		require.False(t, seen[retirement.ticket])
		seen[retirement.ticket] = true
	}
	assertRetirementSafeClose(t, session, lease, scheduler, owner)
}

func TestRetainedOwnerRetirementRacingPendingEpochCannotLoseWake(t *testing.T) {
	owner := newScriptedRetirementImportOwner()
	owner.retireOnPrepare = true
	session, lease, scheduler := newRetainedResetContainmentScheduler(t, owner)
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(0x8d05)))
	awaitRetirementScheduler(t, scheduler)
	require.Equal(t, uint32(1), owner.prepareCalls.Load())
	assertRetirementSafeClose(t, session, lease, scheduler, owner)
}

func TestRetainedOwnerRetirementNeverMasksFailedContainment(t *testing.T) {
	for _, phase := range []string{"retire", "retire-panic", "drain", "disconnect", "query", "query-private-marker", "query-panic", "prepare"} {
		t.Run(phase, func(t *testing.T) {
			owner := newScriptedRetirementImportOwner()
			cause := errors.New("injected " + phase + " uncertainty")
			switch phase {
			case "retire":
				owner.retireErr = cause
			case "retire-panic":
				owner.panicAt = "retire"
			case "drain":
				owner.drainErr = cause
			case "disconnect":
				owner.disconnectErr = cause
			case "prepare":
				owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{err: cause})
			}
			session, lease, scheduler := newRetainedResetContainmentScheduler(t, owner)
			require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(0x8d06)))
			require.Eventually(t, func() bool { return owner.prepareCalls.Load() == 1 }, time.Second, time.Millisecond)
			if phase != "prepare" {
				owner.retirementMu.Lock()
				if phase == "query" {
					owner.queryErr = cause
				}
				if phase == "query-private-marker" {
					owner.queryErr = errRetainedSubmissionOwnerRetirement
				}
				if phase == "query-panic" {
					owner.queryPanic = true
				}
				owner.retirementMu.Unlock()
				owner.publishRetirement(nil)
			}
			awaitRetirementScheduler(t, scheduler)
			reason, diagnostic := retainedReadFailure(context.Background(), scheduler, io.EOF)
			require.Error(t, diagnostic)
			if phase == "drain" || phase == "disconnect" {
				require.Equal(t, retainedusb.ImportCloseOwnerRequested, reason)
			} else {
				require.Equal(t, retainedusb.ImportCloseInvariantFailure, reason)
			}
			result, err := session.close(lease, reason, time.Now().Add(time.Second))
			require.Error(t, err)
			require.Equal(t, retainedusb.ImportDisconnectQuarantined, result.State)
			require.True(t, session.quarantined.Load())
			require.False(t, session.released.Load())
			if phase != "disconnect" {
				require.Zero(t, owner.disconnectCalls.Load())
			}
		})
	}
}

func TestRetainedOwnerRetirementRejectsForgedCapabilities(t *testing.T) {
	for name, mutate := range map[string]func(*retainedusb.ImportRetirementRequest){
		"authority":      func(r *retainedusb.ImportRetirementRequest) { r.Lease.AuthorityID++ },
		"device":         func(r *retainedusb.ImportRetirementRequest) { r.Lease.DeviceID++ },
		"owner":          func(r *retainedusb.ImportRetirementRequest) { r.Lease.OwnerID++ },
		"import":         func(r *retainedusb.ImportRetirementRequest) { r.Lease.ImportToken++ },
		"session":        func(r *retainedusb.ImportRetirementRequest) { r.Lease.SessionGeneration++ },
		"reason":         func(r *retainedusb.ImportRetirementRequest) { r.Reason = 255 },
		"missing-reason": func(r *retainedusb.ImportRetirementRequest) { r.Reason = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			owner := newScriptedRetirementImportOwner()
			session, lease, scheduler := newRetainedResetContainmentScheduler(t, owner)
			owner.publishRetirement(mutate)
			awaitRetirementScheduler(t, scheduler)
			reason, diagnostic := retainedReadFailure(context.Background(), scheduler, io.EOF)
			require.Equal(t, retainedusb.ImportCloseInvariantFailure, reason)
			require.ErrorIs(t, diagnostic, errRetainedImportLeaseRejected)
			result, err := session.close(lease, reason, time.Now().Add(time.Second))
			require.Error(t, err)
			require.Equal(t, retainedusb.ImportDisconnectQuarantined, result.State)
			require.False(t, session.released.Load())
			require.Zero(t, owner.disconnectCalls.Load())
		})
	}
}

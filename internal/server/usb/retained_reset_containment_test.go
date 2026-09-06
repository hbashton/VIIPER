package usb

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
	"github.com/stretchr/testify/require"
)

func TestRetainedResetQuarantineRevokesAdmissionBeforeExpiredCleanup(t *testing.T) {
	owner := newScriptedCombinedImportOwner(0x8b01, 1)
	session, lease, scheduler := newLiveRetainedImportScheduler(t, 0x8b02, 0x8b03, owner)
	reset, err := session.issueReset(lease, time.Now().Add(time.Second))
	require.NoError(t, err)
	require.NoError(t, scheduler.fenceReset(session.reservation, reset))
	require.NoError(t, scheduler.resetAndDrain(session.reservation, reset, time.Now().Add(time.Second)))

	// The scheduler has completed its reversible half, but a later reset phase
	// has failed after consuming the cleanup budget. No cleanup callback can
	// run, so only synchronous authority revocation can prevent a late reopen.
	cause := errors.New("owner reset exceeded its deadline")
	result, err := session.finishResetQuarantined(reset, time.Now().Add(-time.Second), cause)
	require.ErrorIs(t, err, cause)
	require.Equal(t, retainedusb.ImportResetQuarantined, result.State)
	require.True(t, session.quarantined.Load())
	require.False(t, session.released.Load())
	require.Zero(t, owner.drainCalls.Load(), "expired cleanup must not invoke the owner")
	scheduler.mu.Lock()
	closed, drained := scheduler.closed, scheduler.resetDrained
	scheduler.mu.Unlock()
	require.False(t, closed, "test must not rely on scheduler.close revoking admission")
	require.True(t, drained, "test must retain the otherwise-reopenable drain proof")
	require.Equal(t, retainedImportActivationRevoked, session.reservation.admission.activation.Load())
	require.False(t, session.reservation.admission.open.Load())
	require.ErrorIs(t, scheduler.reopenAfterReset(session.reservation, reset), errRetainedImportLeaseRejected)
	require.ErrorIs(t, scheduler.enqueue(retainedInterruptInEnvelope(0x8b04)), errRetainedSubmissionNotActivated)
	require.False(t, session.reservation.admission.open.Load())
}

type retainedResetBlockingRetireOwner struct {
	*scriptedImportSessionOwner
	retireEntered chan struct{}
	retireRelease <-chan struct{}
}

func (owner *retainedResetBlockingRetireOwner) Retire(ticket retainedusb.Ticket, reason retainedusb.RetireReason, now time.Time) error {
	close(owner.retireEntered)
	<-owner.retireRelease
	return owner.scriptedImportSessionOwner.Retire(ticket, reason, now)
}

func newRetainedResetContainmentScheduler(t *testing.T, owner retainedImportCombinedOwner) (*retainedImportSession, retainedusb.ImportLease, *retainedSubmissionScheduler) {
	t.Helper()
	authority, err := newRetainedImportAuthority(0x8c01, 0, 0)
	require.NoError(t, err)
	reservation, lease, err := authority.reserve(0x8c02, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	scheduler, err := newParkedRetainedImportScheduler(context.Background(), reservation, owner, newResponseWriter(io.Discard, nil))
	require.NoError(t, err)
	session, err := authority.commit(reservation, &scriptedImportIngress{}, scheduler, time.Now().Add(time.Second), time.Now().Add(2*time.Second), time.Now().Add(3*time.Second))
	require.NoError(t, err)
	t.Cleanup(func() { _ = scheduler.close() })
	return session, lease, scheduler
}

func TestRetainedResetDrainCannotPublishLateReopenProof(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		name := "deadline-expires-during-retire"
		if revoked {
			name = "activation-revoked-during-retire"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			owner := &retainedResetBlockingRetireOwner{
				scriptedImportSessionOwner: newScriptedCombinedImportOwner(0x8c03, 1),
				retireEntered:              make(chan struct{}), retireRelease: release,
			}
			owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{result: retainedusb.ResultPending, currentEpoch: true})
			session, lease, scheduler := newRetainedResetContainmentScheduler(t, owner)
			releaseRetire := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseRetire)
			require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(0x8c04)))
			require.Eventually(t, func() bool { return owner.prepareCalls.Load() == 1 }, time.Second, time.Millisecond)
			reset, err := session.issueReset(lease, time.Now().Add(time.Second))
			require.NoError(t, err)
			require.NoError(t, scheduler.fenceReset(session.reservation, reset))
			deadline := time.Now().Add(100 * time.Millisecond)
			if revoked {
				deadline = time.Now().Add(time.Second)
			}
			drained := make(chan error, 1)
			go func() { drained <- scheduler.resetAndDrain(session.reservation, reset, deadline) }()
			select {
			case <-owner.retireEntered:
			case <-time.After(time.Second):
				t.Fatal("reset did not enter exact pending-ticket retirement")
			}
			if revoked {
				// An expired outer failure cleanup must revoke while the late
				// retirement callback still owns the serializer, without waiting.
				_, err := session.finishResetQuarantined(reset, time.Now().Add(-time.Second), errors.New("reset failed"))
				require.Error(t, err)
			} else {
				<-time.After(time.Until(deadline))
			}
			releaseRetire()
			select {
			case err := <-drained:
				if revoked {
					require.ErrorIs(t, err, errRetainedImportLeaseRejected)
				} else {
					require.ErrorIs(t, err, errRetainedImportCloseTimedOut)
				}
			case <-time.After(time.Second):
				t.Fatal("late reset drain did not return")
			}
			scheduler.mu.Lock()
			resetDrained, empty := scheduler.resetDrained, scheduler.emptyLocked()
			scheduler.mu.Unlock()
			require.True(t, empty, "retirement can finish without granting reopen authority")
			require.False(t, resetDrained)
			require.ErrorIs(t, scheduler.reopenAfterReset(session.reservation, reset), errRetainedImportLeaseRejected)
			require.False(t, session.reservation.admission.open.Load())
		})
	}
}

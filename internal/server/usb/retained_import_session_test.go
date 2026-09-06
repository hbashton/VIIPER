package usb

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
	"github.com/stretchr/testify/require"
)

type importEventLog struct {
	mu     sync.Mutex
	events []string
}

func (log *importEventLog) add(event string) {
	log.mu.Lock()
	log.events = append(log.events, event)
	log.mu.Unlock()
}

func (log *importEventLog) snapshot() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.events...)
}

type scriptedImportIngress struct {
	events  *importEventLog
	before  func()
	entered chan struct{}
	release <-chan struct{}
	err     error
	panic   any
	calls   atomic.Uint32
}

func (ingress *scriptedImportIngress) Close() error {
	ingress.calls.Add(1)
	if ingress.events != nil {
		ingress.events.add("ingress")
	}
	if ingress.before != nil {
		ingress.before()
	}
	if ingress.entered != nil {
		close(ingress.entered)
	}
	if ingress.release != nil {
		<-ingress.release
	}
	if ingress.panic != nil {
		panic(ingress.panic)
	}
	return ingress.err
}

type scriptedImportScheduler struct {
	events  *importEventLog
	label   string
	before  func()
	entered chan struct{}
	release <-chan struct{}
	err     error
	panic   any
	calls   atomic.Uint32

	binding retainedImportSchedulerBinding

	activationErr     error
	activationPanic   any
	activationEntered chan struct{}
	activationRelease <-chan struct{}
	activationExited  chan struct{}
	activationCalls   atomic.Uint32
	activated         atomic.Bool
	openErr           error
	openPanic         any
	openNoop          bool
	openCalls         atomic.Uint32

	admissionErr     error
	admissionPanic   any
	admissionEntered chan struct{}
	admissionRelease <-chan struct{}
	admissionCalls   atomic.Uint32

	resetErr     error
	resetPanic   any
	resetEntered chan struct{}
	resetRelease <-chan struct{}
	resetCalls   atomic.Uint32
	resetLease   retainedusb.ImportResetLease
	resetFenced  atomic.Bool
	reopenErr    error
	reopenPanic  any
	reopenCalls  atomic.Uint32
}

func (scheduler *scriptedImportScheduler) fenceReset(
	reservation *retainedImportReservation,
	reset retainedusb.ImportResetLease,
) error {
	if scheduler.events != nil {
		scheduler.events.add("reset-fence")
	}
	if scheduler.binding.reservation != reservation ||
		reset.ImportLease != reservation.lease ||
		!scheduler.resetFenced.CompareAndSwap(false, true) {
		return errRetainedImportResetRejected
	}
	reservation.admission.open.Store(false)
	scheduler.resetLease = reset
	return nil
}

func (scheduler *scriptedImportScheduler) close() error {
	scheduler.calls.Add(1)
	if scheduler.events != nil {
		event := "scheduler"
		if scheduler.label != "" {
			event += ":" + scheduler.label
		}
		scheduler.events.add(event)
	}
	if scheduler.before != nil {
		scheduler.before()
	}
	if scheduler.entered != nil {
		close(scheduler.entered)
	}
	if scheduler.release != nil {
		<-scheduler.release
	}
	if scheduler.panic != nil {
		panic(scheduler.panic)
	}
	return scheduler.err
}

func (scheduler *scriptedImportScheduler) importBinding() retainedImportSchedulerBinding {
	return scheduler.binding
}

func (scheduler *scriptedImportScheduler) activateImport(
	reservation *retainedImportReservation,
) error {
	scheduler.activationCalls.Add(1)
	if scheduler.activationExited != nil {
		defer close(scheduler.activationExited)
	}
	if scheduler.activationEntered != nil {
		close(scheduler.activationEntered)
	}
	if scheduler.activationRelease != nil {
		<-scheduler.activationRelease
	}
	if scheduler.activationPanic != nil {
		panic(scheduler.activationPanic)
	}
	if scheduler.activationErr != nil {
		return scheduler.activationErr
	}
	if scheduler.binding.reservation != reservation {
		return errRetainedImportLeaseRejected
	}
	if !reservation.admission.activation.CompareAndSwap(
		retainedImportActivationParked,
		retainedImportActivationStarted) {
		return errRetainedImportLeaseRejected
	}
	scheduler.activated.Store(true)
	return nil
}

func (scheduler *scriptedImportScheduler) openAdmission(
	reservation *retainedImportReservation,
) error {
	scheduler.openCalls.Add(1)
	if scheduler.openPanic != nil {
		panic(scheduler.openPanic)
	}
	if scheduler.openErr != nil {
		return scheduler.openErr
	}
	if scheduler.openNoop {
		return nil
	}
	if scheduler.binding.reservation != reservation ||
		!scheduler.activated.Load() ||
		reservation.admission.activation.Load() !=
			retainedImportActivationStarted ||
		reservation.admission.open.Load() {
		return errRetainedImportLeaseRejected
	}
	reservation.admission.open.Store(true)
	return nil
}

func (scheduler *scriptedImportScheduler) closeAdmission(
	reservation *retainedImportReservation,
) error {
	scheduler.admissionCalls.Add(1)
	if scheduler.binding.reservation != reservation {
		return errRetainedImportLeaseRejected
	}
	if scheduler.admissionEntered != nil {
		close(scheduler.admissionEntered)
	}
	if scheduler.admissionRelease != nil {
		<-scheduler.admissionRelease
	}
	if scheduler.admissionPanic != nil {
		panic(scheduler.admissionPanic)
	}
	reservation.admission.open.Store(false)
	reservation.admission.activation.Store(
		retainedImportActivationRevoked)
	return scheduler.admissionErr
}

func (scheduler *scriptedImportScheduler) resetAndDrain(
	reservation *retainedImportReservation,
	reset retainedusb.ImportResetLease,
	_ time.Time,
) error {
	scheduler.resetCalls.Add(1)
	if scheduler.events != nil {
		scheduler.events.add("reset-scheduler")
	}
	if scheduler.binding.reservation != reservation ||
		reset.ImportLease != reservation.lease {
		return errRetainedImportResetRejected
	}
	if !scheduler.resetFenced.Load() || scheduler.resetLease != reset ||
		reservation.admission.open.Load() {
		return errRetainedImportResetRejected
	}
	if scheduler.resetEntered != nil {
		close(scheduler.resetEntered)
	}
	if scheduler.resetRelease != nil {
		<-scheduler.resetRelease
	}
	if scheduler.resetPanic != nil {
		panic(scheduler.resetPanic)
	}
	return scheduler.resetErr
}

func (scheduler *scriptedImportScheduler) reopenAfterReset(
	reservation *retainedImportReservation,
	reset retainedusb.ImportResetLease,
) error {
	scheduler.reopenCalls.Add(1)
	if scheduler.events != nil {
		scheduler.events.add("reset-reopen")
	}
	if scheduler.reopenPanic != nil {
		panic(scheduler.reopenPanic)
	}
	if scheduler.reopenErr != nil {
		return scheduler.reopenErr
	}
	if scheduler.binding.reservation != reservation ||
		scheduler.resetLease != reset || reservation.admission.open.Load() ||
		reservation.admission.activation.Load() !=
			retainedImportActivationStarted {
		return errRetainedImportResetRejected
	}
	reservation.admission.open.Store(true)
	scheduler.resetFenced.Store(false)
	return nil
}

type scriptedImportSessionOwner struct {
	*scriptedRetainedOwner

	id uint64

	events *importEventLog

	identityPanic   any
	identityEntered chan struct{}
	identityRelease <-chan struct{}
	limitsPanic     any
	limitsEntered   chan struct{}
	limitsRelease   <-chan struct{}
	limitsCalls     atomic.Uint32
	bindErr         error
	bindPanic       any
	bindEntered     chan struct{}
	bindRelease     <-chan struct{}
	bindState       retainedusb.ImportBindState
	bindMutate      func(*retainedusb.ImportBindResult)
	bindBefore      func()
	drainErr        error
	drainPanic      any
	drainEntered    chan struct{}
	drainRelease    <-chan struct{}
	drainMutate     func(*retainedusb.ImportDrainResult)
	drainBefore     func()

	disconnectErr     error
	disconnectPanic   any
	disconnectEntered chan struct{}
	disconnectRelease <-chan struct{}
	disconnectState   retainedusb.ImportDisconnectState
	disconnectMutate  func(*retainedusb.ImportDisconnectResult)
	disconnectBefore  func()

	resetErr          error
	resetPanic        any
	resetEntered      chan struct{}
	resetRelease      <-chan struct{}
	resetMutate       func(*retainedusb.ImportResetResult)
	resetCalls        atomic.Uint32
	resetFenceErr     error
	resetFencePanic   any
	resetFenceEntered chan struct{}
	resetFenceRelease <-chan struct{}
	resetFenceCalls   atomic.Uint32

	bindCalls       atomic.Uint32
	drainCalls      atomic.Uint32
	disconnectCalls atomic.Uint32

	mu                sync.Mutex
	boundLease        retainedusb.ImportLease
	observedLease     retainedusb.ImportLease
	observedReason    retainedusb.ImportCloseReason
	observedDeadlines []time.Time
}

func (owner *scriptedImportSessionOwner) Identity() uint64 {
	if owner.identityEntered != nil {
		close(owner.identityEntered)
	}
	if owner.identityRelease != nil {
		<-owner.identityRelease
	}
	if owner.identityPanic != nil {
		panic(owner.identityPanic)
	}
	return owner.id
}

func (owner *scriptedImportSessionOwner) Limits() retainedusb.Limits {
	owner.limitsCalls.Add(1)
	if owner.limitsEntered != nil {
		close(owner.limitsEntered)
	}
	if owner.limitsRelease != nil {
		<-owner.limitsRelease
	}
	if owner.limitsPanic != nil {
		panic(owner.limitsPanic)
	}
	return owner.scriptedRetainedOwner.Limits()
}

func (owner *scriptedImportSessionOwner) BindImport(
	lease retainedusb.ImportLease,
	_ time.Time,
) (retainedusb.ImportBindResult, error) {
	owner.bindCalls.Add(1)
	owner.mu.Lock()
	owner.boundLease = lease
	owner.mu.Unlock()
	if owner.events != nil {
		owner.events.add("bind")
	}
	if owner.bindBefore != nil {
		owner.bindBefore()
	}
	if owner.bindEntered != nil {
		close(owner.bindEntered)
	}
	if owner.bindRelease != nil {
		<-owner.bindRelease
	}
	if owner.bindPanic != nil {
		panic(owner.bindPanic)
	}
	state := owner.bindState
	if state == retainedusb.ImportBindInvalid {
		state = retainedusb.ImportBindBound
	}
	result := retainedusb.ImportBindResult{Lease: lease, State: state}
	if owner.bindMutate != nil {
		owner.bindMutate(&result)
	}
	return result, owner.bindErr
}

func (owner *scriptedImportSessionOwner) CancelAndDrain(
	lease retainedusb.ImportLease,
	reason retainedusb.ImportCloseReason,
	deadline time.Time,
) (retainedusb.ImportDrainResult, error) {
	owner.drainCalls.Add(1)
	owner.mu.Lock()
	bound := owner.boundLease
	owner.mu.Unlock()
	if lease != bound {
		return retainedusb.ImportDrainResult{}, errors.New(
			"cancel/drain received an unbound import lease")
	}
	owner.observe(lease, reason, deadline)
	if owner.events != nil {
		owner.events.add("drain")
	}
	if owner.drainBefore != nil {
		owner.drainBefore()
	}
	if owner.drainEntered != nil {
		close(owner.drainEntered)
	}
	if owner.drainRelease != nil {
		<-owner.drainRelease
	}
	if owner.drainPanic != nil {
		panic(owner.drainPanic)
	}
	result := retainedusb.ImportDrainResult{
		Lease: lease, Reason: reason, State: retainedusb.ImportDrainDrained,
	}
	if owner.drainMutate != nil {
		owner.drainMutate(&result)
	}
	return result, owner.drainErr
}

func (owner *scriptedImportSessionOwner) DisconnectNeutral(
	lease retainedusb.ImportLease,
	reason retainedusb.ImportCloseReason,
	deadline time.Time,
) (retainedusb.ImportDisconnectResult, error) {
	owner.disconnectCalls.Add(1)
	owner.mu.Lock()
	bound := owner.boundLease
	owner.mu.Unlock()
	if lease != bound {
		return retainedusb.ImportDisconnectResult{}, errors.New(
			"disconnect received an unbound import lease")
	}
	owner.observe(lease, reason, deadline)
	if owner.events != nil {
		owner.events.add("neutral")
	}
	if owner.disconnectBefore != nil {
		owner.disconnectBefore()
	}
	if owner.disconnectEntered != nil {
		close(owner.disconnectEntered)
	}
	if owner.disconnectRelease != nil {
		<-owner.disconnectRelease
	}
	if owner.disconnectPanic != nil {
		panic(owner.disconnectPanic)
	}
	state := owner.disconnectState
	if state == retainedusb.ImportDisconnectInvalid {
		state = retainedusb.ImportDisconnectSafe
	}
	result := retainedusb.ImportDisconnectResult{
		Lease: lease, Reason: reason, State: state,
	}
	if owner.disconnectMutate != nil {
		owner.disconnectMutate(&result)
	}
	return result, owner.disconnectErr
}

func (owner *scriptedImportSessionOwner) FenceAndDrainReset(
	reset retainedusb.ImportResetLease,
	_ time.Time,
) error {
	owner.resetFenceCalls.Add(1)
	owner.mu.Lock()
	bound := owner.boundLease
	owner.mu.Unlock()
	if reset.ImportLease != bound {
		return errors.New("reset fence received an unbound import lease")
	}
	if owner.events != nil {
		owner.events.add("reset-owner-fence")
	}
	if owner.resetFenceEntered != nil {
		close(owner.resetFenceEntered)
	}
	if owner.resetFenceRelease != nil {
		<-owner.resetFenceRelease
	}
	if owner.resetFencePanic != nil {
		panic(owner.resetFencePanic)
	}
	return owner.resetFenceErr
}

func (owner *scriptedImportSessionOwner) ResetAndRestart(
	reset retainedusb.ImportResetLease,
	_ time.Time,
) (retainedusb.ImportResetResult, error) {
	owner.resetCalls.Add(1)
	owner.mu.Lock()
	bound := owner.boundLease
	owner.mu.Unlock()
	if reset.ImportLease != bound {
		return retainedusb.ImportResetResult{}, errors.New(
			"reset received an unbound import lease")
	}
	if owner.events != nil {
		owner.events.add("reset-owner")
	}
	if owner.resetEntered != nil {
		close(owner.resetEntered)
	}
	if owner.resetRelease != nil {
		<-owner.resetRelease
	}
	if owner.resetPanic != nil {
		panic(owner.resetPanic)
	}
	result := retainedusb.ImportResetResult{
		Lease: reset, State: retainedusb.ImportResetSafe,
	}
	if owner.resetMutate != nil {
		owner.resetMutate(&result)
	}
	return result, owner.resetErr
}

func (owner *scriptedImportSessionOwner) observe(
	lease retainedusb.ImportLease,
	reason retainedusb.ImportCloseReason,
	deadline time.Time,
) {
	owner.mu.Lock()
	owner.observedLease = lease
	owner.observedReason = reason
	owner.observedDeadlines = append(owner.observedDeadlines, deadline)
	owner.mu.Unlock()
}

func claimScriptedImport(
	t *testing.T,
	authority *retainedImportAuthority,
	deviceID uint64,
	owner *scriptedImportSessionOwner,
	ingress *scriptedImportIngress,
	scheduler retainedImportScheduler,
) (*retainedImportSession, retainedusb.ImportLease) {
	t.Helper()
	reservation, lease, err := authority.reserve(
		deviceID, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	if scripted, ok := scheduler.(*scriptedImportScheduler); ok {
		configureScriptedImportScheduler(t, scripted, owner, reservation)
	}
	session, err := authority.commit(
		reservation, ingress, scheduler,
		time.Now().Add(time.Second), time.Now().Add(2*time.Second),
		time.Now().Add(3*time.Second))
	require.NoError(t, err)
	require.True(t, lease.Valid())
	return session, lease
}

func configureScriptedImportScheduler(
	t *testing.T,
	scheduler *scriptedImportScheduler,
	owner retainedImportCombinedOwner,
	reservation *retainedImportReservation,
) {
	t.Helper()
	require.NoError(t,
		reservation.authority.beginParkedSchedulerBuild(reservation))
	lease := reservation.lease
	reference, valid := exactRetainedImportOwnerReference(owner)
	require.True(t, valid)
	scheduler.binding = retainedImportSchedulerBinding{
		ownerReference: reference, ownerIdentity: lease.OwnerID,
		sessionGeneration: lease.SessionGeneration,
		reservation:       reservation, activationRequired: true, parked: true,
	}
	require.NoError(t, reservation.authority.finishParkedSchedulerBuild(
		reservation, scheduler))
}

func TestExactRetainedImportOwnerReferenceRejectsZeroSizedPointers(t *testing.T) {
	one := &struct{}{}
	two := &struct{}{}
	if _, valid := exactRetainedImportOwnerReference(one); valid {
		t.Fatal("first zero-sized owner pointer was accepted")
	}
	if _, valid := exactRetainedImportOwnerReference(two); valid {
		t.Fatal("second zero-sized owner pointer was accepted")
	}
	owner := &scriptedImportSessionOwner{id: 1}
	reference, valid := exactRetainedImportOwnerReference(owner)
	if !valid || reference.typeOf == nil || reference.pointer == 0 {
		t.Fatal("nonzero combined owner pointer was rejected")
	}
}

func newScriptedCombinedImportOwner(
	identity uint64,
	depth uint8,
) *scriptedImportSessionOwner {
	hot := newScriptedRetainedOwner(depth)
	hot.identity = identity
	return &scriptedImportSessionOwner{
		scriptedRetainedOwner: hot,
		id:                    identity,
	}
}

func TestRetainedImportSessionClosesInExactOrderAndReleasesLast(t *testing.T) {
	authority, err := newRetainedImportAuthority(101, 40, 80)
	require.NoError(t, err)
	events := &importEventLog{}
	owner := &scriptedImportSessionOwner{id: 201, events: events}
	ingress := &scriptedImportIngress{events: events}
	scheduler := &scriptedImportScheduler{events: events}
	session, lease := claimScriptedImport(
		t, authority, 301, owner, ingress, scheduler)
	require.Equal(t, uint64(41), lease.ImportToken)
	require.Equal(t, uint64(81), lease.SessionGeneration)

	deadline := time.Now().Add(time.Second)
	result, err := session.close(
		lease, retainedusb.ImportClosePeerDisconnect, deadline)
	require.NoError(t, err)
	require.Equal(t, retainedusb.ImportDisconnectResult{
		Lease: lease, Reason: retainedusb.ImportClosePeerDisconnect,
		State: retainedusb.ImportDisconnectSafe,
	}, result)
	require.Equal(t, []string{"bind", "ingress", "scheduler", "drain", "neutral"},
		events.snapshot())
	require.Equal(t, uint32(1), ingress.calls.Load())
	require.Equal(t, uint32(1), scheduler.calls.Load())
	require.Equal(t, uint32(1), owner.bindCalls.Load())
	require.Equal(t, uint32(1), owner.drainCalls.Load())
	require.Equal(t, uint32(1), owner.disconnectCalls.Load())
	owner.mu.Lock()
	require.Equal(t, lease, owner.boundLease)
	require.Equal(t, lease, owner.observedLease)
	require.Equal(t, retainedusb.ImportClosePeerDisconnect,
		owner.observedReason)
	require.Equal(t, []time.Time{deadline, deadline},
		owner.observedDeadlines)
	owner.mu.Unlock()
	state, err := session.stateFor(lease)
	require.NoError(t, err)
	require.Equal(t, retainedImportReleased, state)

	// Close and release are idempotent after the exact terminal result.
	repeated, err := session.close(
		lease, retainedusb.ImportClosePeerDisconnect, time.Time{})
	require.NoError(t, err)
	require.Equal(t, result, repeated)
	require.NoError(t, session.releaseExact(lease))
	require.Equal(t, []string{"bind", "ingress", "scheduler", "drain", "neutral"},
		events.snapshot())

	// A successor receives both new capabilities. A stale release from the old
	// owner is a no-op and cannot delete that successor.
	successorOwner := &scriptedImportSessionOwner{id: 202}
	successor, successorLease := claimScriptedImport(
		t, authority, 301, successorOwner,
		&scriptedImportIngress{}, &scriptedImportScheduler{})
	require.Equal(t, uint64(42), successorLease.ImportToken)
	require.Equal(t, uint64(82), successorLease.SessionGeneration)
	require.NoError(t, session.releaseExact(lease))
	_, _, err = authority.reserve(
		301, &scriptedImportSessionOwner{id: 203},
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportBusy)
	_, err = successor.close(
		successorLease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(time.Second))
	require.NoError(t, err)
}

func TestRetainedImportResetCapabilityIsExactIdempotentAndNonwrapping(
	t *testing.T,
) {
	authority, err := newRetainedImportAuthority(102, 0, 0)
	require.NoError(t, err)
	events := &importEventLog{}
	owner := &scriptedImportSessionOwner{id: 202, events: events}
	scheduler := &scriptedImportScheduler{events: events}
	session, lease := claimScriptedImport(
		t, authority, 302, owner, &scriptedImportIngress{}, scheduler)

	deadline := time.Now().Add(time.Second)
	first, err := session.issueReset(lease, deadline)
	require.NoError(t, err)
	require.Equal(t, uint64(1), first.ResetToken)
	require.Equal(t, uint64(1), first.ResetGeneration)
	result, err := session.reset(first, deadline)
	require.NoError(t, err)
	require.Equal(t, retainedusb.ImportResetResult{
		Lease: first, State: retainedusb.ImportResetSafe,
	}, result)
	require.True(t, session.reservation.admission.open.Load())
	require.Equal(t, []string{
		"bind", "reset-fence", "reset-owner-fence", "reset-scheduler",
		"reset-owner", "reset-reopen",
	}, events.snapshot())

	// The exact cached terminal observation is callback-free and remains
	// available after the owning deadline expires.
	repeated, err := session.reset(first, time.Time{})
	require.NoError(t, err)
	require.Equal(t, result, repeated)
	require.Equal(t, uint32(1), scheduler.resetCalls.Load())
	require.Equal(t, uint32(1), owner.resetCalls.Load())
	require.Equal(t, uint32(1), scheduler.reopenCalls.Load())

	forged := first
	forged.ResetToken++
	forgedResult, err := session.reset(forged, time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportResetRejected)
	require.Equal(t, retainedusb.ImportResetResult{
		Lease: forged, State: retainedusb.ImportResetInvalid,
	}, forgedResult)
	require.False(t, session.quarantined.Load())
	require.True(t, session.reservation.admission.open.Load())
	second, err := session.issueReset(lease, time.Now().Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, uint64(2), second.ResetToken)
	require.Equal(t, uint64(2), second.ResetGeneration)
	_, err = session.reset(second, time.Now().Add(time.Second))
	require.NoError(t, err)
	_, err = session.reset(first, time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportResetRejected)

	authority.mu.Lock()
	authority.nextResetToken = ^uint64(0)
	authority.mu.Unlock()
	_, err = session.issueReset(lease, time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportCounterExhausted)
	session.mu.Lock()
	session.nextResetGeneration = ^uint64(0)
	session.mu.Unlock()
	_, err = session.issueReset(lease, time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportCounterExhausted)
}

func TestRetainedImportResetAmbiguityQuarantinesWithoutNeutralOrRelease(
	t *testing.T,
) {
	authority, err := newRetainedImportAuthority(103, 0, 0)
	require.NoError(t, err)
	release := make(chan struct{})
	scheduler := &scriptedImportScheduler{
		resetEntered: make(chan struct{}), resetRelease: release,
	}
	owner := &scriptedImportSessionOwner{id: 203}
	session, lease := claimScriptedImport(
		t, authority, 303, owner, &scriptedImportIngress{}, scheduler)
	reset, err := session.issueReset(lease, time.Now().Add(time.Second))
	require.NoError(t, err)

	deadline := time.Now().Add(25 * time.Millisecond)
	result, err := session.reset(reset, deadline)
	require.Error(t, err)
	require.Equal(t, retainedusb.ImportResetQuarantined, result.State)
	require.False(t, session.reservation.admission.open.Load())
	require.True(t, session.quarantined.Load())
	require.False(t, session.released.Load())
	require.Equal(t, uint32(1), owner.resetFenceCalls.Load())
	require.Equal(t, uint32(0), owner.resetCalls.Load())
	require.Equal(t, uint32(0), owner.disconnectCalls.Load())
	// The transaction deadline is already exhausted, so containment cannot
	// truthfully claim the owner drain callback ran. Quarantine and the closed
	// admission gate remain the only safe result.
	require.Equal(t, uint32(0), owner.drainCalls.Load())
	require.Equal(t, uint32(0), scheduler.reopenCalls.Load())
	close(release)

	_, err = session.issueReset(lease, time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportResetRejected)
	_, err = session.close(
		lease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(time.Second))
	require.Error(t, err)
}

func TestRetainedImportCloseWaitsForExactResetBeforeTerminalDrain(t *testing.T) {
	authority, err := newRetainedImportAuthority(104, 0, 0)
	require.NoError(t, err)
	events := &importEventLog{}
	release := make(chan struct{})
	owner := &scriptedImportSessionOwner{
		id: 204, events: events, resetEntered: make(chan struct{}),
		resetRelease: release,
	}
	scheduler := &scriptedImportScheduler{events: events}
	session, lease := claimScriptedImport(
		t, authority, 304, owner,
		&scriptedImportIngress{events: events}, scheduler)
	reset, err := session.issueReset(lease, time.Now().Add(time.Second))
	require.NoError(t, err)

	resetDone := make(chan error, 1)
	go func() {
		_, resetErr := session.reset(reset, time.Now().Add(time.Second))
		resetDone <- resetErr
	}()
	select {
	case <-owner.resetEntered:
	case <-time.After(time.Second):
		t.Fatal("owner reset did not begin")
	}
	closeDone := make(chan error, 1)
	go func() {
		_, closeErr := session.close(
			lease, retainedusb.ImportCloseExplicitDetach,
			time.Now().Add(time.Second))
		closeDone <- closeErr
	}()
	select {
	case err := <-closeDone:
		t.Fatalf("close overtook reset: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-resetDone)
	require.NoError(t, <-closeDone)
	require.Equal(t, []string{
		"bind", "reset-fence", "reset-owner-fence", "reset-scheduler",
		"reset-owner", "reset-reopen",
		"ingress", "scheduler", "drain", "neutral",
	}, events.snapshot())
	require.True(t, session.released.Load())
}

func TestRetainedImportShortCloseWaiterCannotQuarantineOwningReset(t *testing.T) {
	authority, err := newRetainedImportAuthority(106, 0, 0)
	require.NoError(t, err)
	release := make(chan struct{})
	owner := &scriptedImportSessionOwner{
		id: 206, resetEntered: make(chan struct{}), resetRelease: release,
	}
	session, lease := claimScriptedImport(
		t, authority, 306, owner, &scriptedImportIngress{},
		&scriptedImportScheduler{})
	reset, err := session.issueReset(lease, time.Now().Add(time.Second))
	require.NoError(t, err)
	resetDone := make(chan error, 1)
	go func() {
		_, resetErr := session.reset(reset, time.Now().Add(time.Second))
		resetDone <- resetErr
	}()
	select {
	case <-owner.resetEntered:
	case <-time.After(time.Second):
		t.Fatal("owner reset did not begin")
	}

	closeResult, err := session.close(
		lease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(5*time.Millisecond))
	require.ErrorIs(t, err, errRetainedImportCloseTimedOut)
	require.Equal(t, retainedusb.ImportDisconnectResult{}, closeResult)
	require.False(t, session.quarantined.Load())
	require.False(t, session.released.Load())

	close(release)
	require.NoError(t, <-resetDone)
	result, err := session.close(
		lease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, retainedusb.ImportDisconnectSafe, result.State)
	require.True(t, session.released.Load())
}

func TestRetainedImportCloseIntentDuringResetLatchesReasonAndBlocksAnotherReset(
	t *testing.T,
) {
	authority, err := newRetainedImportAuthority(107, 0, 0)
	require.NoError(t, err)
	release := make(chan struct{})
	owner := &scriptedImportSessionOwner{
		id: 207, resetEntered: make(chan struct{}), resetRelease: release,
	}
	session, lease := claimScriptedImport(
		t, authority, 307, owner, &scriptedImportIngress{},
		&scriptedImportScheduler{})
	reset, err := session.issueReset(lease, time.Now().Add(time.Second))
	require.NoError(t, err)
	resetDone := make(chan error, 1)
	go func() {
		_, resetErr := session.reset(reset, time.Now().Add(time.Second))
		resetDone <- resetErr
	}()
	select {
	case <-owner.resetEntered:
	case <-time.After(time.Second):
		t.Fatal("owner reset did not begin")
	}

	// The wait budget belongs only to this observer. It may expire without
	// quarantining the owning reset, but its first authoritative reason remains
	// latched so a conflicting close and another reset cannot overtake it.
	closeResult, err := session.close(
		lease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(5*time.Millisecond))
	require.ErrorIs(t, err, errRetainedImportCloseTimedOut)
	require.Equal(t, retainedusb.ImportDisconnectResult{}, closeResult)
	require.False(t, session.quarantined.Load())

	_, err = session.close(
		lease, retainedusb.ImportClosePeerDisconnect,
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportReasonMismatch)

	close(release)
	require.NoError(t, <-resetDone)
	_, err = session.issueReset(lease, time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportResetRejected)

	result, err := session.close(
		lease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, retainedusb.ImportDisconnectSafe, result.State)
	require.True(t, session.released.Load())
}

func TestRetainedImportCloseIntentCachesResetQuarantineTerminal(t *testing.T) {
	authority, err := newRetainedImportAuthority(108, 0, 0)
	require.NoError(t, err)
	release := make(chan struct{})
	owner := &scriptedImportSessionOwner{
		id: 208, resetEntered: make(chan struct{}), resetRelease: release,
		resetErr: errors.New("injected reset failure"),
	}
	session, lease := claimScriptedImport(
		t, authority, 308, owner, &scriptedImportIngress{},
		&scriptedImportScheduler{})
	reset, err := session.issueReset(lease, time.Now().Add(time.Second))
	require.NoError(t, err)
	resetDone := make(chan error, 1)
	go func() {
		_, resetErr := session.reset(reset, time.Now().Add(time.Second))
		resetDone <- resetErr
	}()
	select {
	case <-owner.resetEntered:
	case <-time.After(time.Second):
		t.Fatal("owner reset did not begin")
	}

	_, err = session.close(
		lease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(5*time.Millisecond))
	require.ErrorIs(t, err, errRetainedImportCloseTimedOut)
	close(release)
	require.Error(t, <-resetDone)

	// The reset failure terminally contains the already-latched close intent.
	// Re-observation must be immediate even with an expired waiter deadline;
	// closeDone cannot remain orphaned in a quarantined session.
	result, err := session.close(
		lease, retainedusb.ImportCloseExplicitDetach, time.Time{})
	require.Error(t, err)
	require.Equal(t, retainedusb.ImportDisconnectResult{
		Lease: lease, Reason: retainedusb.ImportCloseExplicitDetach,
		State: retainedusb.ImportDisconnectQuarantined,
	}, result)
	require.True(t, session.quarantined.Load())
	require.False(t, session.released.Load())
}

func TestRetainedImportBindAdmissionIsExactAndBurnsRejectedCapability(t *testing.T) {
	authority, err := newRetainedImportAuthority(105, 50, 90)
	require.NoError(t, err)
	var rejectedSession *retainedImportSession
	var bindingState retainedImportLifecycleState
	rejectedIngress := &scriptedImportIngress{}
	rejectedScheduler := &scriptedImportScheduler{}
	rejectedOwner := &scriptedImportSessionOwner{
		id: 205, bindState: retainedusb.ImportBindRejected,
		bindBefore: func() {
			authority.mu.Lock()
			rejectedSession = authority.active[305]
			authority.mu.Unlock()
			rejectedSession.mu.Lock()
			bindingState = rejectedSession.state
			rejectedSession.mu.Unlock()
		},
	}
	reservation, rejectedLease, err := authority.reserve(
		305, rejectedOwner, time.Now().Add(time.Second))
	require.NoError(t, err)
	configureScriptedImportScheduler(
		t, rejectedScheduler, rejectedOwner, reservation)
	session, err := authority.commit(
		reservation, rejectedIngress, rejectedScheduler,
		time.Now().Add(time.Second), time.Now().Add(2*time.Second),
		time.Now().Add(3*time.Second))
	require.Nil(t, session)
	require.ErrorIs(t, err, errRetainedImportBindRejected)
	require.True(t, rejectedLease.Valid())
	require.Equal(t, uint64(51), rejectedLease.ImportToken)
	require.Equal(t, uint64(91), rejectedLease.SessionGeneration)
	require.Equal(t, uint32(1), rejectedOwner.bindCalls.Load())
	require.Empty(t, authority.active)
	require.NotNil(t, rejectedSession)
	rejectedState, stateErr := rejectedSession.stateFor(rejectedLease)
	require.NoError(t, stateErr)
	require.Equal(t, retainedImportBindRejected, rejectedState)
	require.Equal(t, retainedImportCommitting, bindingState)
	require.True(t, rejectedSession.released.Load())
	_, err = rejectedSession.close(
		rejectedLease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportLeaseRejected)
	require.Equal(t, uint32(1), rejectedIngress.calls.Load())
	require.Equal(t, uint32(1), rejectedScheduler.calls.Load())
	require.Zero(t, rejectedOwner.drainCalls.Load())
	require.Zero(t, rejectedOwner.disconnectCalls.Load())

	// Exact rejection proves no owner state was retained, but both counters
	// stay burned. A successor cannot receive either rejected capability.
	successorOwner := &scriptedImportSessionOwner{id: 206}
	successor, successorLease := claimScriptedImport(
		t, authority, 305, successorOwner,
		&scriptedImportIngress{}, &scriptedImportScheduler{})
	require.Equal(t, uint64(52), successorLease.ImportToken)
	require.Equal(t, uint64(92), successorLease.SessionGeneration)
	require.NotEqual(t, rejectedLease, successorLease)
	_, err = successor.close(
		successorLease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(time.Second))
	require.NoError(t, err)
}

func TestRetainedImportAmbiguousBindFailuresQuarantineBeforeClaimSuccess(t *testing.T) {
	type scenario struct {
		configure func(*scriptedImportSessionOwner)
	}
	scenarios := map[string]scenario{
		"error": {configure: func(owner *scriptedImportSessionOwner) {
			owner.bindErr = errors.New("bind failed ambiguously")
		}},
		"panic": {configure: func(owner *scriptedImportSessionOwner) {
			owner.bindPanic = errors.New("bind panic")
		}},
		"owner-quarantine": {configure: func(owner *scriptedImportSessionOwner) {
			owner.bindState = retainedusb.ImportBindQuarantined
		}},
		"forged-authority": {configure: func(owner *scriptedImportSessionOwner) {
			owner.bindMutate = func(result *retainedusb.ImportBindResult) {
				result.Lease.AuthorityID++
			}
		}},
		"invalid-result": {configure: func(owner *scriptedImportSessionOwner) {
			owner.bindMutate = func(result *retainedusb.ImportBindResult) {
				result.State = retainedusb.ImportBindInvalid
			}
		}},
	}
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			authority, err := newRetainedImportAuthority(106, 0, 0)
			require.NoError(t, err)
			owner := &scriptedImportSessionOwner{id: 207}
			ingress := &scriptedImportIngress{}
			scheduler := &scriptedImportScheduler{}
			scenario.configure(owner)
			reservation, lease, reserveErr := authority.reserve(
				306, owner, time.Now().Add(time.Second))
			require.NoError(t, reserveErr)
			configureScriptedImportScheduler(t, scheduler, owner, reservation)
			session, claimErr := authority.commit(
				reservation, ingress, scheduler,
				time.Now().Add(time.Second), time.Now().Add(2*time.Second),
				time.Now().Add(3*time.Second))
			require.Nil(t, session)
			require.Error(t, claimErr)
			require.True(t, lease.Valid())
			require.Equal(t, uint32(1), owner.bindCalls.Load())
			retained := authority.active[306]
			require.NotNil(t, retained)
			require.True(t, retained.quarantined.Load())
			state, stateErr := retained.stateFor(lease)
			require.NoError(t, stateErr)
			require.Equal(t, retainedImportQuarantined, state)
			_, closeErr := retained.close(
				lease, retainedusb.ImportCloseExplicitDetach,
				time.Now().Add(time.Second))
			require.ErrorIs(t, closeErr, errRetainedImportLeaseRejected)
			require.Equal(t, uint32(1), owner.drainCalls.Load())
			require.Equal(t, uint32(1), owner.disconnectCalls.Load())
			require.Equal(t, uint32(1), ingress.calls.Load())
			require.Equal(t, uint32(1), scheduler.calls.Load())

			// Quarantine rejects before invoking any candidate-owner callback.
			_, _, successorErr := authority.reserve(
				306, &scriptedImportSessionOwner{
					id: 208, identityPanic: "successor identity invoked",
				},
				time.Now().Add(time.Second))
			require.ErrorIs(t, successorErr,
				errRetainedImportQuarantined)
		})
	}
}

func TestRetainedImportAmbiguousBindTransportFailureUsesContainmentDrain(t *testing.T) {
	authority, err := newRetainedImportAuthority(1061, 0, 0)
	require.NoError(t, err)
	events := &importEventLog{}
	bindFailure := errors.New("bind outcome ambiguous")
	owner := &scriptedImportSessionOwner{
		id: 208, events: events, bindErr: bindFailure,
	}
	owner.drainMutate = func(result *retainedusb.ImportDrainResult) {
		result.State = retainedusb.ImportDrainQuarantined
	}
	reservation, lease, err := authority.reserve(
		3061, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	ingress := &scriptedImportIngress{events: events}
	scheduler := &scriptedImportScheduler{
		events: events,
		err:    errors.New("unpublished scheduler close ambiguous"),
	}
	configureScriptedImportScheduler(t, scheduler, owner, reservation)
	session, commitErr := authority.commit(
		reservation, ingress, scheduler,
		time.Now().Add(time.Second), time.Now().Add(2*time.Second),
		time.Now().Add(3*time.Second))
	require.Nil(t, session)
	require.ErrorIs(t, commitErr, bindFailure)
	require.Equal(t, []string{
		"bind", "ingress", "scheduler", "drain",
	}, events.snapshot())
	require.Equal(t, uint32(1), owner.drainCalls.Load())
	require.Zero(t, owner.disconnectCalls.Load())
	require.False(t, reservation.session.released.Load())
	require.True(t, reservation.session.quarantined.Load())
	state, stateErr := reservation.session.stateFor(lease)
	require.NoError(t, stateErr)
	require.Equal(t, retainedImportQuarantined, state)
}

func TestRetainedImportBindTimeoutCannotResumeOrPermitSuccessor(t *testing.T) {
	authority, err := newRetainedImportAuthority(107, 0, 0)
	require.NoError(t, err)
	entered := make(chan struct{})
	release := make(chan struct{})
	owner := &scriptedImportSessionOwner{
		id: 209, bindEntered: entered, bindRelease: release,
	}
	ingress := &scriptedImportIngress{}
	scheduler := &scriptedImportScheduler{}
	reservation, lease, err := authority.reserve(
		307, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	configureScriptedImportScheduler(t, scheduler, owner, reservation)
	type claimResult struct {
		session *retainedImportSession
		err     error
	}
	completed := make(chan claimResult, 1)
	go func() {
		session, claimErr := authority.commit(
			reservation, ingress, scheduler,
			time.Now().Add(150*time.Millisecond),
			time.Now().Add(500*time.Millisecond),
			time.Now().Add(time.Second))
		completed <- claimResult{session: session, err: claimErr}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("bind admission callback was not entered")
	}
	// The binding record is already exclusive while admission is in flight.
	_, _, err = authority.reserve(
		307, &scriptedImportSessionOwner{
			id: 210, identityPanic: "binding successor identity invoked",
		},
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportBusy)
	result := <-completed
	require.Nil(t, result.session)
	require.True(t, lease.Valid())
	require.ErrorIs(t, result.err, errRetainedImportCloseTimedOut)
	retained := authority.active[307]
	require.NotNil(t, retained)
	require.True(t, retained.quarantined.Load())
	close(release)
	time.Sleep(10 * time.Millisecond)
	require.Equal(t, uint32(1), owner.bindCalls.Load())
	require.Equal(t, uint32(1), ingress.calls.Load())
	require.Equal(t, uint32(1), scheduler.calls.Load())
	state, stateErr := retained.stateFor(lease)
	require.NoError(t, stateErr)
	require.Equal(t, retainedImportQuarantined, state)
	_, _, err = authority.reserve(
		307, &scriptedImportSessionOwner{id: 211},
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportQuarantined)
}

func TestRetainedImportReservationAbortBurnsCountersAndCannotDeleteSuccessor(t *testing.T) {
	authority, err := newRetainedImportAuthority(108, 60, 100)
	require.NoError(t, err)
	owner := &scriptedImportSessionOwner{id: 212}
	reservation, lease, err := authority.reserve(
		308, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	state, stateErr := reservation.session.stateFor(lease)
	require.NoError(t, stateErr)
	require.Equal(t, retainedImportReserved, state)
	require.NoError(t, authority.abort(reservation))
	state, stateErr = reservation.session.stateFor(lease)
	require.NoError(t, stateErr)
	require.Equal(t, retainedImportReservationAborted, state)
	require.True(t, reservation.session.released.Load())
	require.Equal(t, uint64(61), authority.nextImportToken)
	require.Equal(t, uint64(101), authority.nextSessionGeneration)

	// Exact repeats are idempotent. Reconstructing every authentication field
	// under a different pointer cannot forge the opaque capability.
	require.NoError(t, authority.abort(reservation))
	forged := &retainedImportReservation{
		authority: reservation.authority,
		session:   reservation.session,
		lease:     reservation.lease,
	}
	require.ErrorIs(t, authority.abort(forged),
		errRetainedImportLeaseRejected)

	successor, successorLease, err := authority.reserve(
		308, &scriptedImportSessionOwner{id: 213},
		time.Now().Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, uint64(62), successorLease.ImportToken)
	require.Equal(t, uint64(102), successorLease.SessionGeneration)
	require.NoError(t, authority.abort(reservation))
	_, _, err = authority.reserve(
		308, &scriptedImportSessionOwner{id: 214},
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportBusy)
	require.NoError(t, authority.abort(successor))
}

func TestRetainedImportCommitRejectsForgedAndCrossAuthorityReservation(t *testing.T) {
	authority, err := newRetainedImportAuthority(109, 0, 0)
	require.NoError(t, err)
	owner := &scriptedImportSessionOwner{id: 215}
	reservation, lease, err := authority.reserve(
		309, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	scheduler := &scriptedImportScheduler{}
	configureScriptedImportScheduler(t, scheduler, owner, reservation)
	ingress := &scriptedImportIngress{}
	forged := &retainedImportReservation{
		authority: reservation.authority,
		session:   reservation.session,
		lease:     reservation.lease,
	}
	_, err = authority.commit(
		forged, ingress, scheduler,
		time.Now().Add(time.Second), time.Now().Add(2*time.Second),
		time.Now().Add(3*time.Second))
	require.ErrorIs(t, err, errRetainedImportLeaseRejected)
	require.Zero(t, owner.bindCalls.Load())
	require.Zero(t, ingress.calls.Load())
	require.Zero(t, scheduler.calls.Load())

	otherAuthority, err := newRetainedImportAuthority(110, 0, 0)
	require.NoError(t, err)
	_, err = otherAuthority.commit(
		reservation, ingress, scheduler,
		time.Now().Add(time.Second), time.Now().Add(2*time.Second),
		time.Now().Add(3*time.Second))
	require.ErrorIs(t, err, errRetainedImportLeaseRejected)
	state, stateErr := reservation.session.stateFor(lease)
	require.NoError(t, stateErr)
	require.Equal(t, retainedImportPrepared, state)
	require.NoError(t, authority.abortPrepared(
		reservation, time.Now().Add(time.Second)))
	require.NoError(t, authority.abortPrepared(reservation, time.Time{}))
	successor, _, err := authority.reserve(
		311, &scriptedImportSessionOwner{id: 218},
		time.Now().Add(time.Second))
	require.NoError(t, err)
	require.NoError(t, authority.abortPrepared(reservation, time.Time{}))
	require.NoError(t, authority.abort(reservation))
	require.NoError(t, authority.abort(successor))
}

func TestRetainedImportCommitRejectsSplitOwnerGenerationAndRunningScheduler(t *testing.T) {
	type scenario struct {
		mutate func(
			*scriptedImportScheduler,
			*scriptedImportSessionOwner,
			*retainedImportReservation,
		)
	}
	scenarios := map[string]scenario{
		"same-id-split-owner": {mutate: func(
			scheduler *scriptedImportScheduler,
			owner *scriptedImportSessionOwner,
			reservation *retainedImportReservation,
		) {
			other := &scriptedImportSessionOwner{id: owner.id}
			reference, valid := exactRetainedImportOwnerReference(other)
			require.True(t, valid)
			scheduler.binding.ownerReference = reference
		}},
		"wrong-owner-id": {mutate: func(
			scheduler *scriptedImportScheduler,
			_ *scriptedImportSessionOwner,
			_ *retainedImportReservation,
		) {
			scheduler.binding.ownerIdentity++
		}},
		"predicted-generation": {mutate: func(
			scheduler *scriptedImportScheduler,
			_ *scriptedImportSessionOwner,
			_ *retainedImportReservation,
		) {
			scheduler.binding.sessionGeneration++
		}},
		"already-running": {mutate: func(
			scheduler *scriptedImportScheduler,
			_ *scriptedImportSessionOwner,
			_ *retainedImportReservation,
		) {
			scheduler.binding.parked = false
		}},
	}
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			authority, err := newRetainedImportAuthority(1110, 0, 0)
			require.NoError(t, err)
			owner := &scriptedImportSessionOwner{id: 216}
			reservation, lease, err := authority.reserve(
				310, owner, time.Now().Add(time.Second))
			require.NoError(t, err)
			ingress := &scriptedImportIngress{}
			scheduler := &scriptedImportScheduler{}
			configureScriptedImportScheduler(t, scheduler, owner, reservation)
			scenario.mutate(scheduler, owner, reservation)
			session, commitErr := authority.commit(
				reservation, ingress, scheduler,
				time.Now().Add(time.Second), time.Now().Add(2*time.Second),
				time.Now().Add(3*time.Second))
			require.Nil(t, session)
			require.ErrorIs(t, commitErr,
				errRetainedImportLeaseRejected)
			require.Zero(t, owner.bindCalls.Load())
			require.Equal(t, uint32(1), ingress.calls.Load())
			require.Equal(t, uint32(1), scheduler.calls.Load())
			require.True(t, reservation.session.quarantined.Load())
			state, stateErr := reservation.session.stateFor(lease)
			require.NoError(t, stateErr)
			require.Equal(t, retainedImportQuarantined, state)
		})
	}
}

func TestRetainedImportCommitRequiresSeparateLaterCleanupDeadline(t *testing.T) {
	authority, err := newRetainedImportAuthority(1120, 0, 0)
	require.NoError(t, err)
	owner := &scriptedImportSessionOwner{id: 217}
	reservation, lease, err := authority.reserve(
		311, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	ingress := &scriptedImportIngress{}
	scheduler := &scriptedImportScheduler{}
	configureScriptedImportScheduler(t, scheduler, owner, reservation)
	bindDeadline := time.Now().Add(time.Second)
	activationDeadline := bindDeadline.Add(time.Second)
	_, err = authority.commit(
		reservation, ingress, scheduler,
		bindDeadline, activationDeadline, activationDeadline)
	require.ErrorIs(t, err, errRetainedImportInvalid)
	require.Zero(t, owner.bindCalls.Load())
	require.Zero(t, ingress.calls.Load())
	require.Zero(t, scheduler.calls.Load())
	state, stateErr := reservation.session.stateFor(lease)
	require.NoError(t, stateErr)
	require.Equal(t, retainedImportPrepared, state)
	require.NoError(t, authority.abortPrepared(
		reservation, time.Now().Add(time.Second)))
}

func TestRetainedImportRejectedBindCleanupFailureQuarantinesWithoutRollback(t *testing.T) {
	authority, err := newRetainedImportAuthority(1130, 0, 0)
	require.NoError(t, err)
	events := &importEventLog{}
	owner := &scriptedImportSessionOwner{
		id: 218, events: events, bindState: retainedusb.ImportBindRejected,
	}
	reservation, _, err := authority.reserve(
		312, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	ingress := &scriptedImportIngress{events: events}
	scheduler := &scriptedImportScheduler{
		events: events, err: errors.New("dedicated scheduler close failed"),
	}
	configureScriptedImportScheduler(t, scheduler, owner, reservation)
	session, commitErr := authority.commit(
		reservation, ingress, scheduler,
		time.Now().Add(time.Second), time.Now().Add(2*time.Second),
		time.Now().Add(3*time.Second))
	require.Nil(t, session)
	require.Error(t, commitErr)
	require.Equal(t, []string{"bind", "ingress", "scheduler"},
		events.snapshot())
	require.True(t, reservation.session.quarantined.Load())
	require.False(t, reservation.session.released.Load())
	require.Same(t, reservation.session, authority.active[312])
	_, _, err = authority.reserve(
		312, &scriptedImportSessionOwner{id: 219},
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportQuarantined)
}

func TestRetainedImportLateBoundCannotPublishOrStartParkedScheduler(t *testing.T) {
	authority, err := newRetainedImportAuthority(1140, 0, 0)
	require.NoError(t, err)
	entered := make(chan struct{})
	release := make(chan struct{})
	owner := newScriptedCombinedImportOwner(220, 1)
	owner.bindEntered = entered
	owner.bindRelease = release
	reservation, lease, err := authority.reserve(
		313, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	scheduler, err := newParkedRetainedImportScheduler(
		context.Background(), reservation, owner,
		newResponseWriter(io.Discard, nil))
	require.NoError(t, err)
	ingress := &scriptedImportIngress{}
	committed := make(chan error, 1)
	go func() {
		_, commitErr := authority.commit(
			reservation, ingress, scheduler,
			time.Now().Add(100*time.Millisecond),
			time.Now().Add(500*time.Millisecond),
			time.Now().Add(time.Second))
		committed <- commitErr
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("combined owner did not enter BindImport")
	}
	commitErr := <-committed
	require.ErrorIs(t, commitErr, errRetainedImportCloseTimedOut)
	require.True(t, reservation.session.quarantined.Load())
	require.Equal(t, uint32(1), ingress.calls.Load())
	scheduler.mu.Lock()
	require.False(t, scheduler.started)
	require.True(t, scheduler.closed)
	scheduler.mu.Unlock()
	close(release)
	time.Sleep(10 * time.Millisecond)
	state, stateErr := reservation.session.stateFor(lease)
	require.NoError(t, stateErr)
	require.Equal(t, retainedImportQuarantined, state)
	require.Equal(t, uint32(1), owner.bindCalls.Load())
}

func TestRetainedImportBoundActivationFailuresRunTerminalOwnerCleanup(t *testing.T) {
	activationFailure := errors.New("activation failed")
	activationPanic := errors.New("activation panicked")
	type scenario struct {
		configure func(*scriptedImportScheduler, *scriptedImportSessionOwner)
		cause     error
	}
	scenarios := map[string]scenario{
		"error": {
			configure: func(
				scheduler *scriptedImportScheduler,
				_ *scriptedImportSessionOwner,
			) {
				scheduler.activationErr = activationFailure
			},
			cause: activationFailure,
		},
		"panic": {
			configure: func(
				scheduler *scriptedImportScheduler,
				_ *scriptedImportSessionOwner,
			) {
				scheduler.activationPanic = activationPanic
			},
			cause: activationPanic,
		},
		"failed-neutral": {
			configure: func(
				scheduler *scriptedImportScheduler,
				owner *scriptedImportSessionOwner,
			) {
				scheduler.activationErr = activationFailure
				owner.disconnectErr = errors.New("neutral proof failed")
			},
			cause: activationFailure,
		},
	}
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			authority, err := newRetainedImportAuthority(1141, 0, 0)
			require.NoError(t, err)
			events := &importEventLog{}
			owner := &scriptedImportSessionOwner{id: 221, events: events}
			ingress := &scriptedImportIngress{events: events}
			scheduler := &scriptedImportScheduler{events: events}
			scenario.configure(scheduler, owner)
			reservation, lease, err := authority.reserve(
				314, owner, time.Now().Add(time.Second))
			require.NoError(t, err)
			configureScriptedImportScheduler(
				t, scheduler, owner, reservation)

			session, commitErr := authority.commit(
				reservation, ingress, scheduler,
				time.Now().Add(time.Second), time.Now().Add(2*time.Second),
				time.Now().Add(3*time.Second))
			require.Nil(t, session)
			require.ErrorIs(t, commitErr, scenario.cause)
			require.Equal(t, []string{
				"bind", "ingress", "scheduler", "drain", "neutral",
			}, events.snapshot())
			require.Equal(t, uint32(1), scheduler.activationCalls.Load())
			require.Equal(t, uint32(1), scheduler.admissionCalls.Load())
			require.Equal(t, uint32(1), scheduler.calls.Load())
			require.Equal(t, uint32(1), owner.bindCalls.Load())
			require.Equal(t, uint32(1), owner.drainCalls.Load())
			require.Equal(t, uint32(1), owner.disconnectCalls.Load())
			require.False(t, reservation.admission.open.Load())
			require.Equal(t, retainedImportActivationRevoked,
				reservation.admission.activation.Load())
			require.False(t, scheduler.activated.Load())
			state, stateErr := reservation.session.stateFor(lease)
			require.NoError(t, stateErr)
			require.Equal(t, retainedImportQuarantined, state)
			require.True(t, reservation.session.quarantined.Load())
			require.False(t, reservation.session.released.Load())
			owner.mu.Lock()
			require.Equal(t, retainedusb.ImportCloseInvariantFailure,
				owner.observedReason)
			owner.mu.Unlock()
			_, closeErr := reservation.session.close(
				lease, retainedusb.ImportCloseExplicitDetach,
				time.Now().Add(time.Second))
			require.ErrorIs(t, closeErr, errRetainedImportLeaseRejected)
			_, _, successorErr := authority.reserve(
				314, &scriptedImportSessionOwner{id: 222},
				time.Now().Add(time.Second))
			require.ErrorIs(t, successorErr, errRetainedImportQuarantined)
		})
	}
}

func TestRetainedImportClaimPublicationFailureCannotReturnSuccess(t *testing.T) {
	claimFailure := errors.New("scheduler died before claim publication")
	tests := map[string]struct {
		configure func(*scriptedImportScheduler)
		cause     error
	}{
		"error": {
			configure: func(scheduler *scriptedImportScheduler) {
				scheduler.openErr = claimFailure
			},
			cause: claimFailure,
		},
		"malformed-no-op": {
			configure: func(scheduler *scriptedImportScheduler) {
				scheduler.openNoop = true
			},
			cause: errRetainedImportLeaseRejected,
		},
		"panic": {
			configure: func(scheduler *scriptedImportScheduler) {
				scheduler.openPanic = claimFailure
			},
			cause: claimFailure,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			authority, err := newRetainedImportAuthority(11415, 0, 0)
			require.NoError(t, err)
			events := &importEventLog{}
			owner := &scriptedImportSessionOwner{id: 2215, events: events}
			ingress := &scriptedImportIngress{events: events}
			scheduler := &scriptedImportScheduler{events: events}
			test.configure(scheduler)
			reservation, lease, err := authority.reserve(
				3145, owner, time.Now().Add(time.Second))
			require.NoError(t, err)
			configureScriptedImportScheduler(
				t, scheduler, owner, reservation)

			session, commitErr := authority.commit(
				reservation, ingress, scheduler,
				time.Now().Add(time.Second),
				time.Now().Add(2*time.Second),
				time.Now().Add(3*time.Second))
			require.Nil(t, session)
			require.ErrorIs(t, commitErr, test.cause)
			require.False(t, reservation.admission.open.Load())
			require.Equal(t, retainedImportActivationRevoked,
				reservation.admission.activation.Load())
			require.Equal(t, uint32(1), scheduler.openCalls.Load())
			require.Equal(t, uint32(1), scheduler.admissionCalls.Load())
			require.Equal(t, uint32(1), scheduler.calls.Load())
			require.Equal(t, uint32(1), owner.drainCalls.Load())
			require.Equal(t, uint32(1), owner.disconnectCalls.Load())
			require.Equal(t, []string{
				"bind", "ingress", "scheduler", "drain", "neutral",
			}, events.snapshot())
			state, stateErr := reservation.session.stateFor(lease)
			require.NoError(t, stateErr)
			require.Equal(t, retainedImportQuarantined, state)
			require.True(t, reservation.session.quarantined.Load())
			require.False(t, reservation.session.released.Load())
			_, _, successorErr := authority.reserve(
				3145, &scriptedImportSessionOwner{id: 2216},
				time.Now().Add(time.Second))
			require.ErrorIs(t, successorErr,
				errRetainedImportQuarantined)
		})
	}
}

func TestRetainedImportRealSchedulerCannotOpenAfterWorkerExit(t *testing.T) {
	authority, err := newRetainedImportAuthority(11416, 0, 0)
	require.NoError(t, err)
	owner := newScriptedCombinedImportOwner(2217, 1)
	reservation, _, err := authority.reserve(
		3146, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	scheduler, err := newParkedRetainedImportScheduler(
		ctx, reservation, owner, newResponseWriter(io.Discard, nil))
	require.NoError(t, err)
	require.NoError(t, scheduler.activateImport(reservation))
	cancel()
	require.NoError(t, scheduler.close())
	require.ErrorIs(t, scheduler.openAdmission(reservation),
		errRetainedImportLeaseRejected)
	require.False(t, reservation.admission.open.Load())
	require.Equal(t, retainedImportActivationRevoked,
		reservation.admission.activation.Load())
	require.NoError(t, authority.abortPrepared(
		reservation, time.Now().Add(time.Second)))
}

func TestRetainedImportActivationTimeoutRevokesLateReturn(t *testing.T) {
	authority, err := newRetainedImportAuthority(1142, 0, 0)
	require.NoError(t, err)
	owner := &scriptedImportSessionOwner{id: 223}
	entered := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	scheduler := &scriptedImportScheduler{
		activationEntered: entered,
		activationRelease: release,
		activationExited:  exited,
	}
	ingress := &scriptedImportIngress{}
	reservation, lease, err := authority.reserve(
		315, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	configureScriptedImportScheduler(t, scheduler, owner, reservation)
	committed := make(chan error, 1)
	start := time.Now()
	go func() {
		_, commitErr := authority.commit(
			reservation, ingress, scheduler,
			start.Add(100*time.Millisecond),
			start.Add(200*time.Millisecond),
			start.Add(time.Second))
		committed <- commitErr
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("activation callback was not entered")
	}
	commitErr := <-committed
	require.ErrorIs(t, commitErr, errRetainedImportCloseTimedOut)
	require.False(t, reservation.admission.open.Load())
	require.Equal(t, retainedImportActivationRevoked,
		reservation.admission.activation.Load())
	require.Equal(t, uint32(1), owner.drainCalls.Load())
	require.Equal(t, uint32(1), owner.disconnectCalls.Load())
	close(release)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("late activation callback did not return")
	}
	require.False(t, scheduler.activated.Load())
	require.False(t, reservation.admission.open.Load())
	state, stateErr := reservation.session.stateFor(lease)
	require.NoError(t, stateErr)
	require.Equal(t, retainedImportQuarantined, state)
	_, _, err = authority.reserve(
		315, &scriptedImportSessionOwner{id: 224},
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportQuarantined)
}

func TestRetainedImportSchedulerConstructionFailureRestoresExactAbort(t *testing.T) {
	authority, err := newRetainedImportAuthority(1143, 90, 190)
	require.NoError(t, err)
	owner := newScriptedCombinedImportOwner(225, 1)
	owner.limitsPanic = errors.New("limits unavailable")
	reservation, lease, err := authority.reserve(
		316, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	scheduler, buildErr := newParkedRetainedImportScheduler(
		context.Background(), reservation, owner,
		newResponseWriter(io.Discard, nil))
	require.Nil(t, scheduler)
	require.Error(t, buildErr)
	state, stateErr := reservation.session.stateFor(lease)
	require.NoError(t, stateErr)
	require.Equal(t, retainedImportReserved, state)
	require.Nil(t, reservation.session.scheduler)
	require.Equal(t, uint32(1), owner.limitsCalls.Load())
	require.NoError(t, authority.abort(reservation))

	successor, successorLease, err := authority.reserve(
		316, &scriptedImportSessionOwner{id: 226},
		time.Now().Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, lease.ImportToken+1, successorLease.ImportToken)
	require.Equal(t, lease.SessionGeneration+1,
		successorLease.SessionGeneration)
	require.NoError(t, authority.abort(successor))
}

func TestRetainedImportAbortVersusSchedulerBuildHasSingleOwner(t *testing.T) {
	for iteration := 0; iteration < 64; iteration++ {
		authority, err := newRetainedImportAuthority(
			uint64(1200+iteration), 0, 0)
		require.NoError(t, err)
		owner := newScriptedCombinedImportOwner(
			uint64(1300+iteration), 1)
		reservation, _, err := authority.reserve(
			uint64(1400+iteration), owner, time.Now().Add(time.Second))
		require.NoError(t, err)
		start := make(chan struct{})
		abortResult := make(chan error, 1)
		type buildResult struct {
			scheduler *retainedSubmissionScheduler
			err       error
		}
		built := make(chan buildResult, 1)
		go func() {
			<-start
			abortResult <- authority.abort(reservation)
		}()
		go func() {
			<-start
			scheduler, buildErr := newParkedRetainedImportScheduler(
				context.Background(), reservation, owner,
				newResponseWriter(io.Discard, nil))
			built <- buildResult{scheduler: scheduler, err: buildErr}
		}()
		close(start)
		abortErr := <-abortResult
		build := <-built
		switch {
		case abortErr == nil:
			require.Error(t, build.err)
			require.Nil(t, build.scheduler)
			require.Zero(t, owner.limitsCalls.Load(),
				"abort winner allowed scheduler construction callbacks")
		case build.err == nil:
			require.ErrorIs(t, abortErr, errRetainedImportLeaseRejected)
			require.NotNil(t, build.scheduler)
			build.scheduler.mu.Lock()
			require.False(t, build.scheduler.started)
			require.False(t, build.scheduler.importActivated)
			build.scheduler.mu.Unlock()
			require.ErrorIs(t, authority.abort(reservation),
				errRetainedImportLeaseRejected)
			require.NoError(t, authority.abortPrepared(
				reservation, time.Now().Add(time.Second)))
		default:
			t.Fatalf("abort and build both failed: abort=%v build=%v",
				abortErr, build.err)
		}
		require.Empty(t, authority.active)
	}
}

func TestRetainedImportPreparedAbortIsConcurrentAndIdempotent(t *testing.T) {
	authority, err := newRetainedImportAuthority(1145, 0, 0)
	require.NoError(t, err)
	owner := &scriptedImportSessionOwner{id: 228}
	reservation, lease, err := authority.reserve(
		318, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	entered := make(chan struct{})
	release := make(chan struct{})
	scheduler := &scriptedImportScheduler{
		entered: entered,
		release: release,
	}
	configureScriptedImportScheduler(t, scheduler, owner, reservation)

	const callers = 17
	results := make(chan error, callers)
	go func() {
		results <- authority.abortPrepared(
			reservation, time.Now().Add(2*time.Second))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("prepared scheduler close did not start")
	}
	state, stateErr := reservation.session.stateFor(lease)
	require.NoError(t, stateErr)
	require.Equal(t, retainedImportPreparedAborting, state)
	for caller := 1; caller < callers; caller++ {
		go func() {
			results <- authority.abortPrepared(
				reservation, time.Now().Add(2*time.Second))
		}()
	}
	require.Equal(t, uint32(1), scheduler.admissionCalls.Load())
	require.Equal(t, uint32(1), scheduler.calls.Load())
	close(release)
	for caller := 0; caller < callers; caller++ {
		require.NoError(t, <-results)
	}
	require.NoError(t, authority.abortPrepared(reservation, time.Time{}))
	require.NoError(t, authority.abort(reservation))
	require.Empty(t, authority.active)
}

func TestRetainedImportAdmissionFenceRejectsEveryLaterEnqueue(t *testing.T) {
	authority, err := newRetainedImportAuthority(1144, 0, 0)
	require.NoError(t, err)
	owner := newScriptedCombinedImportOwner(227, 2)
	reservation, lease, err := authority.reserve(
		317, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	scheduler, err := newParkedRetainedImportScheduler(
		context.Background(), reservation, owner,
		newResponseWriter(io.Discard, nil))
	require.NoError(t, err)
	session, err := authority.commit(
		reservation, &scriptedImportIngress{}, scheduler,
		time.Now().Add(time.Second), time.Now().Add(2*time.Second),
		time.Now().Add(3*time.Second))
	require.NoError(t, err)
	require.True(t, reservation.admission.open.Load())

	forged := &retainedImportReservation{
		authority: reservation.authority,
		session:   reservation.session,
		lease:     reservation.lease,
	}
	require.ErrorIs(t, scheduler.closeAdmission(forged),
		errRetainedImportLeaseRejected)
	require.True(t, reservation.admission.open.Load())
	require.NoError(t, scheduler.closeAdmission(reservation))
	require.False(t, reservation.admission.open.Load())

	const attempts = 64
	errorsSeen := make(chan error, attempts)
	var enqueues sync.WaitGroup
	for attempt := 0; attempt < attempts; attempt++ {
		enqueues.Add(1)
		go func(sequence uint32) {
			defer enqueues.Done()
			errorsSeen <- scheduler.enqueue(
				retainedInterruptInEnvelope(sequence))
		}(uint32(2000 + attempt))
	}
	enqueues.Wait()
	close(errorsSeen)
	for enqueueErr := range errorsSeen {
		require.ErrorIs(t, enqueueErr,
			errRetainedSubmissionNotActivated)
	}
	require.Zero(t, owner.stageCalls.Load())
	_, err = session.close(
		lease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(time.Second))
	require.NoError(t, err)
}

func TestRetainedImportSessionRejectsForgedCrossAndStaleCapabilities(t *testing.T) {
	authority, err := newRetainedImportAuthority(111, 0, 10)
	require.NoError(t, err)
	session, lease := claimScriptedImport(
		t, authority, 401, &scriptedImportSessionOwner{id: 501},
		&scriptedImportIngress{}, &scriptedImportScheduler{})

	for name, forge := range map[string]func(*retainedusb.ImportLease){
		"zero": func(candidate *retainedusb.ImportLease) {
			*candidate = retainedusb.ImportLease{}
		},
		"authority": func(candidate *retainedusb.ImportLease) {
			candidate.AuthorityID++
		},
		"device": func(candidate *retainedusb.ImportLease) {
			candidate.DeviceID++
		},
		"owner": func(candidate *retainedusb.ImportLease) {
			candidate.OwnerID++
		},
		"token": func(candidate *retainedusb.ImportLease) {
			candidate.ImportToken++
		},
		"session": func(candidate *retainedusb.ImportLease) {
			candidate.SessionGeneration++
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := lease
			forge(&candidate)
			_, err := session.close(
				candidate, retainedusb.ImportCloseReadFailure,
				time.Now().Add(time.Second))
			require.ErrorIs(t, err, errRetainedImportLeaseRejected)
		})
	}

	other, otherLease := claimScriptedImport(
		t, authority, 402, &scriptedImportSessionOwner{id: 502},
		&scriptedImportIngress{}, &scriptedImportScheduler{})
	_, err = session.close(
		otherLease, retainedusb.ImportCloseReadFailure,
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportLeaseRejected)
	_, err = other.close(
		lease, retainedusb.ImportCloseReadFailure,
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportLeaseRejected)

	_, err = session.close(
		lease, retainedusb.ImportCloseReadFailure,
		time.Now().Add(time.Second))
	require.NoError(t, err)
	_, err = session.close(
		lease, retainedusb.ImportCloseWriteFailure,
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportReasonMismatch)
	_, err = other.close(
		otherLease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(time.Second))
	require.NoError(t, err)
}

func TestRetainedImportCountersExhaustWithoutWrapOrPartialAdvance(t *testing.T) {
	for name, seeds := range map[string][2]uint64{
		"import":  {^uint64(0), 17},
		"session": {23, ^uint64(0)},
	} {
		t.Run(name, func(t *testing.T) {
			importSeed, sessionSeed := seeds[0], seeds[1]
			authority, err := newRetainedImportAuthority(
				121, importSeed, sessionSeed)
			require.NoError(t, err)
			_, _, err = authority.reserve(
				601, &scriptedImportSessionOwner{id: 701},
				time.Now().Add(time.Second))
			require.ErrorIs(t, err, errRetainedImportCounterExhausted)
			require.Equal(t, importSeed, authority.nextImportToken)
			require.Equal(t, sessionSeed, authority.nextSessionGeneration)
			require.Empty(t, authority.active)
		})
	}

	authority, err := newRetainedImportAuthority(
		122, ^uint64(0)-1, ^uint64(0)-1)
	require.NoError(t, err)
	session, lease := claimScriptedImport(
		t, authority, 602, &scriptedImportSessionOwner{id: 702},
		&scriptedImportIngress{}, &scriptedImportScheduler{})
	require.Equal(t, ^uint64(0), lease.ImportToken)
	require.Equal(t, ^uint64(0), lease.SessionGeneration)
	_, err = session.close(
		lease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(time.Second))
	require.NoError(t, err)
	_, _, err = authority.reserve(
		602, &scriptedImportSessionOwner{id: 703},
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportCounterExhausted)
	require.Equal(t, ^uint64(0), authority.nextImportToken)
	require.Equal(t, ^uint64(0), authority.nextSessionGeneration)
}

func TestRetainedImportCloseIsExactlyOnceAcrossConcurrentRaces(t *testing.T) {
	authority, err := newRetainedImportAuthority(131, 0, 0)
	require.NoError(t, err)
	events := &importEventLog{}
	entered := make(chan struct{})
	release := make(chan struct{})
	ingress := &scriptedImportIngress{
		events: events, entered: entered, release: release,
	}
	scheduler := &scriptedImportScheduler{events: events}
	owner := &scriptedImportSessionOwner{id: 801, events: events}
	session, lease := claimScriptedImport(
		t, authority, 701, owner, ingress, scheduler)

	const callers = 32
	start := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			_, closeErr := session.close(
				lease, retainedusb.ImportCloseContextCanceled,
				time.Now().Add(2*time.Second))
			results <- closeErr
		}()
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owning close did not enter ingress stop")
	}
	_, err = session.close(
		lease, retainedusb.ImportCloseWriteFailure,
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportReasonMismatch)
	close(release)
	for range callers {
		require.NoError(t, <-results)
	}
	require.Equal(t, []string{"bind", "ingress", "scheduler", "drain", "neutral"},
		events.snapshot())
	require.Equal(t, uint32(1), ingress.calls.Load())
	require.Equal(t, uint32(1), scheduler.calls.Load())
	require.Equal(t, uint32(1), owner.drainCalls.Load())
	require.Equal(t, uint32(1), owner.disconnectCalls.Load())
}

func TestRetainedImportShortWaiterCannotQuarantineOwningClose(t *testing.T) {
	authority, err := newRetainedImportAuthority(141, 0, 0)
	require.NoError(t, err)
	entered := make(chan struct{})
	release := make(chan struct{})
	session, lease := claimScriptedImport(
		t, authority, 901, &scriptedImportSessionOwner{id: 902},
		&scriptedImportIngress{entered: entered, release: release},
		&scriptedImportScheduler{})
	ownerResult := make(chan error, 1)
	go func() {
		_, closeErr := session.close(
			lease, retainedusb.ImportCloseExplicitDetach,
			time.Now().Add(time.Second))
		ownerResult <- closeErr
	}()
	<-entered
	_, err = session.close(
		lease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(5*time.Millisecond))
	require.ErrorIs(t, err, errRetainedImportCloseTimedOut)
	require.False(t, session.quarantined.Load())
	close(release)
	require.NoError(t, <-ownerResult)
	require.False(t, session.quarantined.Load())
}

func TestRetainedImportCloseDelegatesEveryRetainedJobStateBeforeDrain(t *testing.T) {
	jobStates := []string{
		"free", "raw-queued", "ticketed", "pending", "admitted", "terminal",
		"lifecycle-retired",
	}
	for index, jobState := range jobStates {
		t.Run(jobState, func(t *testing.T) {
			authority, err := newRetainedImportAuthority(
				uint64(151+index), 0, 0)
			require.NoError(t, err)
			events := &importEventLog{}
			session, lease := claimScriptedImport(
				t, authority, uint64(1001+index),
				&scriptedImportSessionOwner{
					id: uint64(1101 + index), events: events,
				},
				&scriptedImportIngress{events: events},
				&scriptedImportScheduler{events: events, label: jobState})
			_, err = session.close(
				lease, retainedusb.ImportClosePeerDisconnect,
				time.Now().Add(time.Second))
			require.NoError(t, err)
			require.Equal(t, []string{
				"bind", "ingress", "scheduler:" + jobState, "drain", "neutral",
			}, events.snapshot())
		})
	}
}

func TestRetainedImportCloseJoinsRealPendingSchedulerBeforeLocalDrain(t *testing.T) {
	authority, err := newRetainedImportAuthority(161, 0, 0)
	require.NoError(t, err)
	events := &importEventLog{}
	owner := newScriptedCombinedImportOwner(1203, 1)
	owner.events = events
	owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultPending, currentEpoch: true,
	})
	reservation, lease, err := authority.reserve(
		1204, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	hotScheduler, err := newParkedRetainedImportScheduler(
		context.Background(), reservation, owner,
		newResponseWriter(io.Discard, nil))
	require.NoError(t, err)
	session, err := authority.commit(
		reservation, &scriptedImportIngress{events: events}, hotScheduler,
		time.Now().Add(time.Second), time.Now().Add(2*time.Second),
		time.Now().Add(3*time.Second))
	require.NoError(t, err)
	require.NoError(t, hotScheduler.enqueue(
		retainedInterruptInEnvelope(1202)))
	require.Eventually(t, func() bool {
		return owner.prepareCalls.Load() == 1
	}, time.Second, time.Millisecond)
	result, err := session.close(
		lease, retainedusb.ImportClosePeerDisconnect,
		time.Now().Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, retainedusb.ImportDisconnectSafe, result.State)
	owner.scriptedRetainedOwner.mu.Lock()
	require.Len(t, owner.retirements, 1)
	require.Equal(t, retainedusb.RetireConnectionClose,
		owner.retirements[0].reason)
	owner.scriptedRetainedOwner.mu.Unlock()
	require.Equal(t, []string{"bind", "ingress", "drain", "neutral"},
		events.snapshot())
	require.Equal(t, uint32(1), owner.drainCalls.Load())
}

func TestRetainedImportParkedSchedulerRejectsIngressUntilClaimPublication(t *testing.T) {
	authority, err := newRetainedImportAuthority(164, 0, 0)
	require.NoError(t, err)
	owner := newScriptedCombinedImportOwner(1234, 1)
	owner.appendPlan(retainedusb.LaneInterruptOut, retainedOwnerPlan{
		result: retainedusb.ResultSuccess, actualLength: 1,
	})
	reservation, lease, err := authority.reserve(
		1233, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	hotScheduler, err := newParkedRetainedImportScheduler(
		context.Background(), reservation, owner,
		newResponseWriter(io.Discard, nil))
	require.NoError(t, err)
	require.ErrorIs(t, hotScheduler.enqueue(
		retainedInterruptOutEnvelope(1232, []byte{0x7a})),
		errRetainedSubmissionNotActivated)
	require.False(t, reservation.admission.open.Load())
	require.Zero(t, owner.stageCalls.Load())
	outIndex, _ := retainedusb.LaneInterruptOut.Index()
	hotScheduler.mu.Lock()
	require.False(t, hotScheduler.started)
	require.False(t, hotScheduler.importActivated)
	require.Zero(t, hotScheduler.lanes[outIndex].orderCount)
	hotScheduler.mu.Unlock()

	session, err := authority.commit(
		reservation, &scriptedImportIngress{}, hotScheduler,
		time.Now().Add(time.Second), time.Now().Add(2*time.Second),
		time.Now().Add(3*time.Second))
	require.NoError(t, err)
	require.True(t, reservation.admission.open.Load())
	hotScheduler.mu.Lock()
	require.True(t, hotScheduler.started)
	require.True(t, hotScheduler.importActivated)
	hotScheduler.mu.Unlock()
	require.NoError(t, hotScheduler.enqueue(
		retainedInterruptOutEnvelope(1232, []byte{0x7a})))
	require.Eventually(t, func() bool {
		return owner.stageCalls.Load() == 1
	}, time.Second, time.Millisecond)
	_, err = session.close(
		lease, retainedusb.ImportClosePeerDisconnect,
		time.Now().Add(time.Second))
	require.NoError(t, err)
	require.False(t, reservation.admission.open.Load())
	hotScheduler.mu.Lock()
	require.Zero(t, hotScheduler.lanes[outIndex].orderCount)
	hotScheduler.mu.Unlock()
}

func TestRetainedImportLifecycleVisitsEverySessionStateInOrder(t *testing.T) {
	authority, err := newRetainedImportAuthority(165, 0, 0)
	require.NoError(t, err)
	var session *retainedImportSession
	var lease retainedusb.ImportLease
	var mu sync.Mutex
	states := []retainedImportLifecycleState{retainedImportClaimed}
	recordState := func() {
		state, stateErr := session.stateFor(lease)
		if stateErr != nil {
			panic(stateErr)
		}
		mu.Lock()
		states = append(states, state)
		mu.Unlock()
	}
	owner := &scriptedImportSessionOwner{
		id: 1251, drainBefore: recordState, disconnectBefore: recordState,
	}
	neutralEntered := make(chan struct{})
	neutralRelease := make(chan struct{})
	owner.disconnectEntered = neutralEntered
	owner.disconnectRelease = neutralRelease
	ingress := &scriptedImportIngress{before: recordState}
	scheduler := &scriptedImportScheduler{before: recordState}
	session, lease = claimScriptedImport(
		t, authority, 1252, owner, ingress, scheduler)
	closed := make(chan error, 1)
	go func() {
		_, closeErr := session.close(
			lease, retainedusb.ImportCloseExplicitDetach,
			time.Now().Add(2*time.Second))
		closed <- closeErr
	}()
	select {
	case <-neutralEntered:
	case <-time.After(time.Second):
		t.Fatal("disconnect-neutral state was not entered")
	}
	// Hold exact release so the otherwise transient proven-neutral state is
	// observable without adding a production-only test hook.
	authority.mu.Lock()
	close(neutralRelease)
	neutralized := false
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		state, stateErr := session.stateFor(lease)
		if stateErr == nil && state == retainedImportNeutralized {
			neutralized = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if neutralized {
		recordState()
	}
	authority.mu.Unlock()
	require.True(t, neutralized)
	require.NoError(t, <-closed)
	recordState()
	mu.Lock()
	require.Equal(t, []retainedImportLifecycleState{
		retainedImportClaimed,
		retainedImportClosing,
		retainedImportIngressStopped,
		retainedImportSchedulerClosed,
		retainedImportLocalDrained,
		retainedImportNeutralized,
		retainedImportReleased,
	}, states)
	mu.Unlock()
}

func TestRetainedImportLifecycleFailuresQuarantineExactSession(t *testing.T) {
	type scenario struct {
		configure func(
			*scriptedImportIngress,
			*scriptedImportScheduler,
			*scriptedImportSessionOwner,
		)
		events []string
	}
	scenarios := map[string]scenario{
		"ingress-error": {
			configure: func(
				ingress *scriptedImportIngress,
				_ *scriptedImportScheduler,
				_ *scriptedImportSessionOwner,
			) {
				ingress.err = errors.New("ingress close failed")
			},
			events: []string{"bind", "ingress", "scheduler", "drain"},
		},
		"ingress-panic": {
			configure: func(
				ingress *scriptedImportIngress,
				_ *scriptedImportScheduler,
				_ *scriptedImportSessionOwner,
			) {
				ingress.panic = errors.New("ingress close panic")
			},
			events: []string{"bind", "ingress", "scheduler", "drain"},
		},
		"scheduler-error": {
			configure: func(
				_ *scriptedImportIngress,
				scheduler *scriptedImportScheduler,
				_ *scriptedImportSessionOwner,
			) {
				scheduler.err = errors.New("scheduler close failed")
			},
			events: []string{"bind", "ingress", "scheduler", "drain"},
		},
		"scheduler-panic": {
			configure: func(
				_ *scriptedImportIngress,
				scheduler *scriptedImportScheduler,
				_ *scriptedImportSessionOwner,
			) {
				scheduler.panic = errors.New("scheduler close panic")
			},
			events: []string{"bind", "ingress", "scheduler", "drain"},
		},
		"drain-error": {
			configure: func(
				_ *scriptedImportIngress,
				_ *scriptedImportScheduler,
				owner *scriptedImportSessionOwner,
			) {
				owner.drainErr = errors.New("local drain failed")
			},
			events: []string{"bind", "ingress", "scheduler", "drain"},
		},
		"drain-panic": {
			configure: func(
				_ *scriptedImportIngress,
				_ *scriptedImportScheduler,
				owner *scriptedImportSessionOwner,
			) {
				owner.drainPanic = errors.New("local drain panic")
			},
			events: []string{"bind", "ingress", "scheduler", "drain"},
		},
		"drain-forged-session": {
			configure: func(
				_ *scriptedImportIngress,
				_ *scriptedImportScheduler,
				owner *scriptedImportSessionOwner,
			) {
				owner.drainMutate = func(result *retainedusb.ImportDrainResult) {
					result.Lease.SessionGeneration++
				}
			},
			events: []string{"bind", "ingress", "scheduler", "drain"},
		},
		"drain-forged-reason": {
			configure: func(
				_ *scriptedImportIngress,
				_ *scriptedImportScheduler,
				owner *scriptedImportSessionOwner,
			) {
				owner.drainMutate = func(result *retainedusb.ImportDrainResult) {
					result.Reason = retainedusb.ImportCloseWriteFailure
				}
			},
			events: []string{"bind", "ingress", "scheduler", "drain"},
		},
		"drain-not-drained": {
			configure: func(
				_ *scriptedImportIngress,
				_ *scriptedImportScheduler,
				owner *scriptedImportSessionOwner,
			) {
				owner.drainMutate = func(result *retainedusb.ImportDrainResult) {
					result.State = retainedusb.ImportDrainQuarantined
				}
			},
			events: []string{"bind", "ingress", "scheduler", "drain"},
		},
		"neutral-error": {
			configure: func(
				_ *scriptedImportIngress,
				_ *scriptedImportScheduler,
				owner *scriptedImportSessionOwner,
			) {
				owner.disconnectErr = errors.New("neutral failed")
			},
			events: []string{"bind", "ingress", "scheduler", "drain", "neutral"},
		},
		"neutral-panic": {
			configure: func(
				_ *scriptedImportIngress,
				_ *scriptedImportScheduler,
				owner *scriptedImportSessionOwner,
			) {
				owner.disconnectPanic = errors.New("neutral panic")
			},
			events: []string{"bind", "ingress", "scheduler", "drain", "neutral"},
		},
		"neutral-owner-quarantine": {
			configure: func(
				_ *scriptedImportIngress,
				_ *scriptedImportScheduler,
				owner *scriptedImportSessionOwner,
			) {
				owner.disconnectState = retainedusb.ImportDisconnectQuarantined
			},
			events: []string{"bind", "ingress", "scheduler", "drain", "neutral"},
		},
		"neutral-forged-token": {
			configure: func(
				_ *scriptedImportIngress,
				_ *scriptedImportScheduler,
				owner *scriptedImportSessionOwner,
			) {
				owner.disconnectMutate = func(
					result *retainedusb.ImportDisconnectResult,
				) {
					result.Lease.ImportToken++
				}
			},
			events: []string{"bind", "ingress", "scheduler", "drain", "neutral"},
		},
		"neutral-forged-reason": {
			configure: func(
				_ *scriptedImportIngress,
				_ *scriptedImportScheduler,
				owner *scriptedImportSessionOwner,
			) {
				owner.disconnectMutate = func(
					result *retainedusb.ImportDisconnectResult,
				) {
					result.Reason = retainedusb.ImportCloseReadFailure
				}
			},
			events: []string{"bind", "ingress", "scheduler", "drain", "neutral"},
		},
	}
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			authority, err := newRetainedImportAuthority(166, 0, 0)
			require.NoError(t, err)
			events := &importEventLog{}
			ingress := &scriptedImportIngress{events: events}
			scheduler := &scriptedImportScheduler{events: events}
			owner := &scriptedImportSessionOwner{id: 1261, events: events}
			scenario.configure(ingress, scheduler, owner)
			session, lease := claimScriptedImport(
				t, authority, 1262, owner, ingress, scheduler)
			result, closeErr := session.close(
				lease, retainedusb.ImportCloseInvariantFailure,
				time.Now().Add(time.Second))
			require.Error(t, closeErr)
			require.Equal(t, lease, result.Lease)
			require.Equal(t, retainedusb.ImportCloseInvariantFailure,
				result.Reason)
			require.Equal(t, retainedusb.ImportDisconnectQuarantined,
				result.State)
			require.True(t, session.quarantined.Load())
			state, stateErr := session.stateFor(lease)
			require.NoError(t, stateErr)
			require.Equal(t, retainedImportQuarantined, state)
			require.Equal(t, scenario.events, events.snapshot())
			require.ErrorIs(t, session.releaseExact(lease),
				errRetainedImportUnsafeRelease)

			// The same result/error is immutable and no callback repeats.
			repeated, repeatedErr := session.close(
				lease, retainedusb.ImportCloseInvariantFailure, time.Time{})
			require.Equal(t, result, repeated)
			require.EqualError(t, repeatedErr, closeErr.Error())
			require.Equal(t, scenario.events, events.snapshot())
			_, _, successorErr := authority.reserve(
				1262, &scriptedImportSessionOwner{
					id: 1263, identityPanic: "quarantined successor called",
				},
				time.Now().Add(time.Second))
			require.ErrorIs(t, successorErr,
				errRetainedImportQuarantined)
		})
	}
}

func TestRetainedImportSchedulerFailureRunsContainmentDrainOnly(t *testing.T) {
	authority, err := newRetainedImportAuthority(1661, 0, 0)
	require.NoError(t, err)
	events := &importEventLog{}
	owner := &scriptedImportSessionOwner{id: 1264, events: events}
	containmentCause := errors.New("owner already quarantined")
	owner.drainErr = containmentCause
	owner.drainMutate = func(result *retainedusb.ImportDrainResult) {
		result.State = retainedusb.ImportDrainQuarantined
	}
	schedulerFailure := errors.New("retained completion quarantined owner")
	scheduler := &scriptedImportScheduler{
		events: events,
		err:    schedulerFailure,
	}
	session, lease := claimScriptedImport(
		t, authority, 1265, owner,
		&scriptedImportIngress{events: events}, scheduler)

	result, closeErr := session.close(
		lease, retainedusb.ImportCloseInvariantFailure,
		time.Now().Add(time.Second))
	require.ErrorIs(t, closeErr, schedulerFailure)
	require.ErrorIs(t, closeErr, containmentCause)
	require.Equal(t, retainedusb.ImportDisconnectQuarantined, result.State)
	require.Equal(t, []string{
		"bind", "ingress", "scheduler", "drain",
	}, events.snapshot())
	require.Equal(t, uint32(1), owner.drainCalls.Load())
	require.Zero(t, owner.disconnectCalls.Load())
	require.False(t, session.released.Load())
	require.Same(t, session, authority.active[lease.DeviceID])
	require.True(t, session.quarantined.Load())
	state, stateErr := session.stateFor(lease)
	require.NoError(t, stateErr)
	require.Equal(t, retainedImportQuarantined, state)
	_, _, err = authority.reserve(
		lease.DeviceID, &scriptedImportSessionOwner{id: 1266},
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportQuarantined)

	repeated, repeatedErr := session.close(
		lease, retainedusb.ImportCloseInvariantFailure, time.Time{})
	require.Equal(t, result, repeated)
	require.EqualError(t, repeatedErr, closeErr.Error())
	require.Equal(t, uint32(1), owner.drainCalls.Load())
	require.Zero(t, owner.disconnectCalls.Load())
}

func TestRetainedImportLifecycleTimeoutsStopAtExactPhaseAndQuarantine(t *testing.T) {
	type timeoutScenario struct {
		configure func(
			*scriptedImportIngress,
			*scriptedImportScheduler,
			*scriptedImportSessionOwner,
			chan struct{}, <-chan struct{},
		)
		events []string
	}
	scenarios := map[string]timeoutScenario{
		"ingress": {
			configure: func(
				ingress *scriptedImportIngress,
				_ *scriptedImportScheduler,
				_ *scriptedImportSessionOwner,
				entered chan struct{}, release <-chan struct{},
			) {
				ingress.entered, ingress.release = entered, release
			},
			events: []string{"bind", "ingress"},
		},
		"scheduler": {
			configure: func(
				_ *scriptedImportIngress,
				scheduler *scriptedImportScheduler,
				_ *scriptedImportSessionOwner,
				entered chan struct{}, release <-chan struct{},
			) {
				scheduler.entered, scheduler.release = entered, release
			},
			events: []string{"bind", "ingress", "scheduler"},
		},
		"drain": {
			configure: func(
				_ *scriptedImportIngress,
				_ *scriptedImportScheduler,
				owner *scriptedImportSessionOwner,
				entered chan struct{}, release <-chan struct{},
			) {
				owner.drainEntered, owner.drainRelease = entered, release
			},
			events: []string{"bind", "ingress", "scheduler", "drain"},
		},
		"neutral": {
			configure: func(
				_ *scriptedImportIngress,
				_ *scriptedImportScheduler,
				owner *scriptedImportSessionOwner,
				entered chan struct{}, release <-chan struct{},
			) {
				owner.disconnectEntered = entered
				owner.disconnectRelease = release
			},
			events: []string{"bind", "ingress", "scheduler", "drain", "neutral"},
		},
	}
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			authority, err := newRetainedImportAuthority(167, 0, 0)
			require.NoError(t, err)
			events := &importEventLog{}
			ingress := &scriptedImportIngress{events: events}
			scheduler := &scriptedImportScheduler{events: events}
			owner := &scriptedImportSessionOwner{id: 1271, events: events}
			entered := make(chan struct{})
			release := make(chan struct{})
			scenario.configure(ingress, scheduler, owner, entered, release)
			session, lease := claimScriptedImport(
				t, authority, 1272, owner, ingress, scheduler)
			closeResult := make(chan struct {
				result retainedusb.ImportDisconnectResult
				err    error
			}, 1)
			go func() {
				result, closeErr := session.close(
					lease, retainedusb.ImportCloseContextCanceled,
					time.Now().Add(200*time.Millisecond))
				closeResult <- struct {
					result retainedusb.ImportDisconnectResult
					err    error
				}{result: result, err: closeErr}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("target lifecycle phase was not entered")
			}
			completed := <-closeResult
			require.ErrorIs(t, completed.err,
				errRetainedImportCloseTimedOut)
			require.Equal(t, retainedusb.ImportDisconnectQuarantined,
				completed.result.State)
			require.Equal(t, scenario.events, events.snapshot())
			close(release)
			time.Sleep(10 * time.Millisecond)
			// A late callback return cannot resume later phases.
			require.Equal(t, scenario.events, events.snapshot())
			_, _, successorErr := authority.reserve(
				1272, &scriptedImportSessionOwner{id: 1273},
				time.Now().Add(time.Second))
			require.ErrorIs(t, successorErr,
				errRetainedImportQuarantined)
		})
	}
}

func TestRetainedImportDoesNotReleaseBeforeNeutralTerminal(t *testing.T) {
	authority, err := newRetainedImportAuthority(168, 0, 0)
	require.NoError(t, err)
	neutralEntered := make(chan struct{})
	neutralRelease := make(chan struct{})
	owner := &scriptedImportSessionOwner{
		id: 1281, disconnectEntered: neutralEntered,
		disconnectRelease: neutralRelease,
	}
	session, lease := claimScriptedImport(
		t, authority, 1282, owner,
		&scriptedImportIngress{}, &scriptedImportScheduler{})
	closed := make(chan error, 1)
	go func() {
		_, closeErr := session.close(
			lease, retainedusb.ImportCloseExplicitDetach,
			time.Now().Add(time.Second))
		closed <- closeErr
	}()
	<-neutralEntered
	_, _, err = authority.reserve(
		1282, &scriptedImportSessionOwner{id: 1283},
		time.Now().Add(time.Second))
	require.ErrorIs(t, err, errRetainedImportBusy)
	require.ErrorIs(t, session.releaseExact(lease),
		errRetainedImportUnsafeRelease)
	close(neutralRelease)
	require.NoError(t, <-closed)
	successor, successorLease := claimScriptedImport(
		t, authority, 1282, &scriptedImportSessionOwner{id: 1283},
		&scriptedImportIngress{}, &scriptedImportScheduler{})
	_, err = successor.close(
		successorLease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(time.Second))
	require.NoError(t, err)
}

func TestRetainedImportCloseDoesNotDuplicateRealTerminalCompletion(t *testing.T) {
	authority, err := newRetainedImportAuthority(169, 0, 0)
	require.NoError(t, err)
	owner := newScriptedCombinedImportOwner(1294, 1)
	owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultData, data: []byte{0x11, 0x22},
	})
	reservation, lease, err := authority.reserve(
		1293, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	recorder := newRecordingWriter()
	hotScheduler, err := newParkedRetainedImportScheduler(
		context.Background(), reservation, owner,
		newResponseWriter(recorder, nil))
	require.NoError(t, err)
	session, err := authority.commit(
		reservation, &scriptedImportIngress{}, hotScheduler,
		time.Now().Add(time.Second), time.Now().Add(2*time.Second),
		time.Now().Add(3*time.Second))
	require.NoError(t, err)
	require.NoError(t, hotScheduler.enqueue(
		retainedInterruptInEnvelope(1292)))
	recorder.waitForWrites(t, 1)
	require.Eventually(t, func() bool {
		owner.scriptedRetainedOwner.mu.Lock()
		defer owner.scriptedRetainedOwner.mu.Unlock()
		return len(owner.completions) == 1
	}, time.Second, time.Millisecond)
	owner.scriptedRetainedOwner.mu.Lock()
	require.Len(t, owner.completions, 1)
	require.Empty(t, owner.retirements)
	owner.scriptedRetainedOwner.mu.Unlock()
	_, err = session.close(
		lease, retainedusb.ImportClosePeerDisconnect,
		time.Now().Add(time.Second))
	require.NoError(t, err)
	owner.scriptedRetainedOwner.mu.Lock()
	require.Len(t, owner.completions, 1)
	require.Empty(t, owner.retirements)
	owner.scriptedRetainedOwner.mu.Unlock()
}

func TestRetainedImportCompletedCloseAndLeaseValidationAllocateZero(t *testing.T) {
	authority, err := newRetainedImportAuthority(171, 0, 0)
	require.NoError(t, err)
	session, lease := claimScriptedImport(
		t, authority, 1301, &scriptedImportSessionOwner{id: 1302},
		&scriptedImportIngress{}, &scriptedImportScheduler{})
	_, err = session.close(
		lease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(time.Second))
	require.NoError(t, err)
	leaseAllocs := testing.AllocsPerRun(1000, func() {
		if !lease.Valid() {
			panic("valid retained import lease rejected")
		}
	})
	require.Zero(t, leaseAllocs)
	closeAllocs := testing.AllocsPerRun(1000, func() {
		result, closeErr := session.close(
			lease, retainedusb.ImportCloseExplicitDetach, time.Time{})
		if closeErr != nil || result.State != retainedusb.ImportDisconnectSafe {
			panic("completed retained close changed")
		}
	})
	require.Zero(t, closeAllocs)
}

func TestRetainedImportAuthorityProductionCompositionIsExplicitlyGated(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	files := token.NewFileSet()
	constructorCounts := map[string]int{
		"newRetainedImportAuthority":       0,
		"newParkedRetainedImportScheduler": 0,
	}
	constructorLocations := map[string][]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Clean(name)
		parsed, parseErr := parser.ParseFile(files, path, nil, parser.ImportsOnly)
		require.NoError(t, parseErr)
		for _, imported := range parsed.Imports {
			pathValue := strings.Trim(imported.Path.Value, "\"")
			require.NotContains(t, pathValue, "/registry")
			require.NotContains(t, pathValue, "/device/xbox")
		}
		parsed, parseErr = parser.ParseFile(files, path, nil, 0)
		require.NoError(t, parseErr)
		ast.Inspect(parsed, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			if _, tracked := constructorCounts[identifier.Name]; !tracked {
				return true
			}
			constructorCounts[identifier.Name]++
			constructorLocations[identifier.Name] = append(
				constructorLocations[identifier.Name],
				files.Position(identifier.Pos()).String())
			return true
		})
	}
	for constructor, count := range constructorCounts {
		require.Equal(t, 2, count,
			"retained constructor %s must have one declaration and one production composition point: %v",
			constructor, constructorLocations[constructor])
	}
	serverSource, readErr := os.ReadFile("server.go")
	require.NoError(t, readErr)
	require.Contains(t, string(serverSource),
		"config.RetainedImportAuthorityID != 0")
	require.Contains(t, string(serverSource),
		"retainedImportCapability(")
	legacyStreamSource, readErr := os.ReadFile("urb_stream.go")
	require.NoError(t, readErr)
	require.NotContains(t, string(legacyStreamSource),
		"retainedImportAuthority")
	require.NotContains(t, string(legacyStreamSource),
		"retainedImportSession")
}

func TestRetainedImportOwnerIdentityPanicAndTimeoutDoNotPublishClaim(t *testing.T) {
	for name, scenario := range map[string]struct {
		owner    *scriptedImportSessionOwner
		deadline time.Time
	}{
		"panic": {
			owner: &scriptedImportSessionOwner{
				id: 1401, identityPanic: errors.New("identity boom"),
			},
			deadline: time.Now().Add(time.Second),
		},
		"expired": {
			owner:    &scriptedImportSessionOwner{id: 1402},
			deadline: time.Now().Add(-time.Second),
		},
	} {
		t.Run(name, func(t *testing.T) {
			authority, err := newRetainedImportAuthority(181, 9, 19)
			require.NoError(t, err)
			_, lease, err := authority.reserve(
				1403, scenario.owner, scenario.deadline)
			require.Error(t, err)
			require.Zero(t, lease)
			require.Empty(t, authority.active)
			require.Equal(t, uint64(9), authority.nextImportToken)
			require.Equal(t, uint64(19), authority.nextSessionGeneration)
		})
	}
}

func TestRetainedImportOwnerIdentityCallbackTimeoutIsContained(t *testing.T) {
	authority, err := newRetainedImportAuthority(182, 29, 39)
	require.NoError(t, err)
	entered := make(chan struct{})
	release := make(chan struct{})
	owner := &scriptedImportSessionOwner{
		id: 1411, identityEntered: entered, identityRelease: release,
	}
	claimResult := make(chan error, 1)
	go func() {
		_, _, claimErr := authority.reserve(
			1412, owner, time.Now().Add(100*time.Millisecond))
		claimResult <- claimErr
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owner identity callback was not entered")
	}
	err = <-claimResult
	require.ErrorIs(t, err, errRetainedImportCloseTimedOut)
	require.Empty(t, authority.active)
	require.Equal(t, uint64(29), authority.nextImportToken)
	require.Equal(t, uint64(39), authority.nextSessionGeneration)
	close(release)
}

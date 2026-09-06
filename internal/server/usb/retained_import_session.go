package usb

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
)

var (
	errRetainedImportInvalid = errors.New(
		"invalid retained USB import session")
	errRetainedImportBusy = errors.New(
		"retained USB device is already imported")
	errRetainedImportQuarantined = errors.New(
		"retained USB device/session is quarantined")
	errRetainedImportCounterExhausted = errors.New(
		"retained USB import/session counter exhausted")
	errRetainedImportLeaseRejected = errors.New(
		"retained USB import lease rejected")
	errRetainedImportReasonMismatch = errors.New(
		"retained USB import close reason mismatch")
	errRetainedImportCloseTimedOut = errors.New(
		"retained USB import close timed out")
	errRetainedImportUnsafeRelease = errors.New(
		"retained USB import release is not safe")
	errRetainedImportLifecycle = errors.New(
		"retained USB import lifecycle failed")
	errRetainedImportBindRejected = errors.New(
		"retained USB import owner rejected lease")
	errRetainedImportResetRejected = errors.New(
		"retained USB import reset capability rejected")
)

// retainedImportIngress is intentionally the smallest possible connection
// boundary. A net.Conn satisfies it, but this dormant tranche has no net.Conn
// or handleConn call site.
type retainedImportIngress interface {
	Close() error
}

// retainedImportScheduler is satisfied by retainedSubmissionScheduler. The
// whole-import owner can therefore join Stage-B.1 without learning its job
// representation or gaining access to hot tickets.
type retainedImportScheduler interface {
	close() error
	closeAdmission(*retainedImportReservation) error
	openAdmission(*retainedImportReservation) error
	importBinding() retainedImportSchedulerBinding
	activateImport(*retainedImportReservation) error
}

// retainedImportResetScheduler is an optional, separately typed reversible
// boundary. Terminal closeAdmission/close can never satisfy it.
type retainedImportResetScheduler interface {
	fenceReset(
		*retainedImportReservation, retainedusb.ImportResetLease,
	) error
	resetAndDrain(
		*retainedImportReservation, retainedusb.ImportResetLease, time.Time,
	) error
	reopenAfterReset(
		*retainedImportReservation, retainedusb.ImportResetLease,
	) error
}

type retainedImportCombinedOwner interface {
	retainedusb.Owner
	retainedusb.ImportSessionOwner
}

type retainedImportOwnerReference struct {
	typeOf  reflect.Type
	pointer uintptr
}

func exactRetainedImportOwnerReference(
	owner any,
) (retainedImportOwnerReference, bool) {
	if interfaceIsNil(owner) {
		return retainedImportOwnerReference{}, false
	}
	value := reflect.ValueOf(owner)
	if value.Kind() != reflect.Pointer || value.IsNil() ||
		value.Type().Elem().Size() == 0 {
		return retainedImportOwnerReference{}, false
	}
	reference := retainedImportOwnerReference{
		typeOf: value.Type(), pointer: value.Pointer(),
	}
	return reference, reference.pointer != 0
}

type retainedImportSchedulerBinding struct {
	ownerReference     retainedImportOwnerReference
	ownerIdentity      uint64
	sessionGeneration  uint64
	reservation        *retainedImportReservation
	activationRequired bool
	parked             bool
}

type retainedImportLifecycleState uint8

const (
	retainedImportReserved retainedImportLifecycleState = iota + 1
	retainedImportBuilding
	retainedImportPrepared
	retainedImportPreparedAborting
	retainedImportCommitting
	retainedImportClaimed
	retainedImportResetting
	retainedImportReservationAborted
	retainedImportBindRejected
	retainedImportClosing
	retainedImportIngressStopped
	retainedImportSchedulerClosed
	retainedImportLocalDrained
	retainedImportNeutralized
	retainedImportReleased
	retainedImportQuarantined
)

// retainedImportAuthority is a dormant exact-generation import registry. It
// does not share Server.activeImports: the legacy closure deliberately wraps,
// hides its token, and releases before a retained owner can prove neutral.
// Production routing must not use this authority until a separately reviewed
// opt-in import path owns it end to end.
type retainedImportAuthority struct {
	identity uint64

	mu                    sync.Mutex
	active                map[uint64]*retainedImportSession
	nextImportToken       uint64
	nextSessionGeneration uint64
	nextResetToken        uint64
}

// newRetainedImportAuthority constructs the dormant Stage-B.2 authority. The
// seed values are the last issued values, exposed only so exhaustion and
// nonwrapping behavior can be tested without billions of claims.
func newRetainedImportAuthority(
	identity uint64,
	lastImportToken uint64,
	lastSessionGeneration uint64,
) (*retainedImportAuthority, error) {
	if identity == 0 {
		return nil, errRetainedImportInvalid
	}
	return &retainedImportAuthority{
		identity:              identity,
		active:                make(map[uint64]*retainedImportSession),
		nextImportToken:       lastImportToken,
		nextSessionGeneration: lastSessionGeneration,
	}, nil
}

// retainedImportReservation is the opaque, exact first half of the dormant
// import transaction. Its pointer identity is part of authentication; copying
// the value cannot forge a reservation.
type retainedImportReservation struct {
	authority *retainedImportAuthority
	session   *retainedImportSession
	lease     retainedusb.ImportLease
	admission retainedImportAdmission
}

type retainedImportAdmission struct {
	open       atomic.Bool
	activation atomic.Uint32
}

const (
	retainedImportActivationParked uint32 = iota
	retainedImportActivationStarted
	retainedImportActivationRevoked
)

// reserve issues and publishes one exact capability before an owner is bound
// or a scheduler exists. The caller uses lease.SessionGeneration to construct
// a parked scheduler from the exact same combined owner, then calls commit.
// Once issued, both counters remain burned even if build, BindImport, or commit
// later rejects or fails.
func (authority *retainedImportAuthority) reserve(
	deviceID uint64,
	owner retainedImportCombinedOwner,
	deadline time.Time,
) (*retainedImportReservation, retainedusb.ImportLease, error) {
	return authority.reserveWithIdentityObserver(
		deviceID, owner, deadline, nil)
}

func (authority *retainedImportAuthority) reserveWithIdentityObserver(
	deviceID uint64,
	owner retainedImportCombinedOwner,
	deadline time.Time,
	identityObserver func(uint64) error,
) (*retainedImportReservation, retainedusb.ImportLease, error) {
	if authority == nil || authority.identity == 0 || deviceID == 0 ||
		interfaceIsNil(owner) || !deadline.After(time.Now()) {
		return nil, retainedusb.ImportLease{}, errRetainedImportInvalid
	}
	ownerReference, referenceValid := exactRetainedImportOwnerReference(owner)
	if !referenceValid {
		return nil, retainedusb.ImportLease{}, errRetainedImportInvalid
	}
	// Reject a retained predecessor or exhausted authority before invoking a
	// candidate owner. A quarantined device must not let an untrusted successor
	// perform even its Identity callback.
	authority.mu.Lock()
	availabilityErr := authority.claimAvailabilityLocked(deviceID)
	authority.mu.Unlock()
	if availabilityErr != nil {
		return nil, retainedusb.ImportLease{}, availabilityErr
	}
	ownerIdentity, err := invokeRetainedImportStep(
		"owner identity", deadline, func() (uint64, error) {
			return owner.Identity(), nil
		})
	if err != nil || ownerIdentity == 0 {
		if err == nil {
			err = errRetainedImportInvalid
		}
		return nil, retainedusb.ImportLease{}, fmt.Errorf(
			"%w: owner identity: %w", errRetainedImportLifecycle, err)
	}
	// Publish the sampled owner-lifetime identity before the second availability
	// check. A contender which loses the DeviceID race after Identity therefore
	// cannot mutate Identity and present a new value on retry.
	if identityObserver != nil {
		if err := identityObserver(ownerIdentity); err != nil {
			return nil, retainedusb.ImportLease{}, err
		}
	}

	authority.mu.Lock()
	if availabilityErr = authority.claimAvailabilityLocked(deviceID); availabilityErr != nil {
		authority.mu.Unlock()
		return nil, retainedusb.ImportLease{}, availabilityErr
	}
	authority.nextImportToken++
	authority.nextSessionGeneration++
	lease := retainedusb.ImportLease{
		AuthorityID:       authority.identity,
		DeviceID:          deviceID,
		OwnerID:           ownerIdentity,
		ImportToken:       authority.nextImportToken,
		SessionGeneration: authority.nextSessionGeneration,
	}
	session := &retainedImportSession{
		authority:      authority,
		lease:          lease,
		owner:          owner,
		ownerReference: ownerReference,
		state:          retainedImportReserved,
		closeDone:      make(chan struct{}),
	}
	reservation := &retainedImportReservation{
		authority: authority, session: session, lease: lease,
	}
	session.reservation = reservation
	authority.active[deviceID] = session
	authority.mu.Unlock()
	return reservation, lease, nil
}

// commit attaches exact transport resources, validates that the parked
// scheduler was constructed from the reserved combined owner and generation,
// then invokes BindImport. cleanupDeadline is separate and strictly later so a
// Bind timeout cannot consume the only budget available to close owned
// ingress/scheduler resources.
func (authority *retainedImportAuthority) commit(
	reservation *retainedImportReservation,
	ingress retainedImportIngress,
	scheduler retainedImportScheduler,
	bindDeadline time.Time,
	activationDeadline time.Time,
	cleanupDeadline time.Time,
) (*retainedImportSession, error) {
	if authority == nil || interfaceIsNil(ingress) ||
		interfaceIsNil(scheduler) || !bindDeadline.After(time.Now()) ||
		!activationDeadline.After(bindDeadline) ||
		!cleanupDeadline.After(activationDeadline) {
		return nil, errRetainedImportInvalid
	}
	session, err := authority.beginCommit(reservation, ingress, scheduler)
	if err != nil {
		return nil, err
	}
	binding, bindingErr := invokeRetainedImportStep(
		"authenticate parked scheduler", bindDeadline,
		func() (retainedImportSchedulerBinding, error) {
			return scheduler.importBinding(), nil
		})
	if bindingErr != nil || binding.ownerReference != session.ownerReference ||
		binding.ownerIdentity != session.lease.OwnerID ||
		binding.sessionGeneration != session.lease.SessionGeneration ||
		binding.reservation != reservation || !binding.activationRequired ||
		!binding.parked {
		if bindingErr == nil {
			bindingErr = errRetainedImportLeaseRejected
		}
		return nil, session.failCommit(
			"authenticate parked scheduler", bindingErr, cleanupDeadline, false)
	}

	bind, bindErr := invokeRetainedImportStep(
		"bind import lease", bindDeadline,
		func() (retainedusb.ImportBindResult, error) {
			return session.owner.BindImport(session.lease, bindDeadline)
		})
	if bindErr == nil && bind.Lease == session.lease &&
		bind.State == retainedusb.ImportBindBound {
		_, activationErr := invokeRetainedImportStep(
			"activate retained scheduler", activationDeadline,
			func() (struct{}, error) {
				return struct{}{}, scheduler.activateImport(reservation)
			})
		if activationErr == nil &&
			reservation.admission.activation.Load() !=
				retainedImportActivationStarted {
			activationErr = errRetainedImportLeaseRejected
		}
		if activationErr != nil {
			return nil, session.failBoundCommit(
				activationErr, cleanupDeadline)
		}
		if publishErr := session.publishClaimed(); publishErr != nil {
			return nil, session.failBoundCommit(
				publishErr, cleanupDeadline)
		}
		return session, nil
	}
	if bindErr == nil && bind.Lease == session.lease &&
		bind.State == retainedusb.ImportBindRejected {
		session.setState(retainedImportBindRejected)
		if cleanupErr := session.cleanupUnpublished(cleanupDeadline); cleanupErr != nil {
			session.quarantineBind()
			return nil, fmt.Errorf(
				"%w: rejected bind cleanup: %w",
				errRetainedImportLifecycle, cleanupErr)
		}
		if rollbackErr := session.rollbackRejectedBind(); rollbackErr != nil {
			session.quarantineBind()
			return nil, fmt.Errorf(
				"%w: bind rejection rollback: %w",
				errRetainedImportLifecycle, rollbackErr)
		}
		return nil, errRetainedImportBindRejected
	}
	if bindErr == nil {
		if bind.State == retainedusb.ImportBindQuarantined &&
			bind.Lease == session.lease {
			bindErr = errRetainedImportQuarantined
		} else {
			bindErr = errRetainedImportLeaseRejected
		}
	}
	return nil, session.failCommit(
		"bind import lease", bindErr, cleanupDeadline, true)
}

func (authority *retainedImportAuthority) beginCommit(
	reservation *retainedImportReservation,
	ingress retainedImportIngress,
	scheduler retainedImportScheduler,
) (*retainedImportSession, error) {
	if !authority.authenticatesReservation(reservation) {
		return nil, errRetainedImportLeaseRejected
	}
	session := reservation.session
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.active[reservation.lease.DeviceID] != session {
		return nil, errRetainedImportLeaseRejected
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.state != retainedImportPrepared ||
		session.scheduler != scheduler {
		return nil, errRetainedImportLeaseRejected
	}
	session.ingress = ingress
	session.state = retainedImportCommitting
	return session, nil
}

// beginParkedSchedulerBuild transfers the exact reservation from abortable to
// builder-owned before any scheduler callback or allocation. If abort wins the
// authority lock first, construction rejects without touching the owner. If
// build wins, plain abort rejects until build either fails back to Reserved or
// publishes a Prepared scheduler owned by the authority.
func (authority *retainedImportAuthority) beginParkedSchedulerBuild(
	reservation *retainedImportReservation,
) error {
	if !authority.authenticatesReservation(reservation) {
		return errRetainedImportLeaseRejected
	}
	session := reservation.session
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.active[reservation.lease.DeviceID] != session {
		return errRetainedImportLeaseRejected
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.state != retainedImportReserved || session.scheduler != nil {
		return errRetainedImportLeaseRejected
	}
	session.state = retainedImportBuilding
	return nil
}

func (authority *retainedImportAuthority) finishParkedSchedulerBuild(
	reservation *retainedImportReservation,
	scheduler retainedImportScheduler,
) error {
	if !authority.authenticatesReservation(reservation) ||
		interfaceIsNil(scheduler) {
		return errRetainedImportLeaseRejected
	}
	session := reservation.session
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.active[reservation.lease.DeviceID] != session {
		return errRetainedImportLeaseRejected
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.state != retainedImportBuilding || session.scheduler != nil {
		return errRetainedImportLeaseRejected
	}
	session.scheduler = scheduler
	session.state = retainedImportPrepared
	return nil
}

func (authority *retainedImportAuthority) failParkedSchedulerBuild(
	reservation *retainedImportReservation,
) error {
	if !authority.authenticatesReservation(reservation) {
		return errRetainedImportLeaseRejected
	}
	session := reservation.session
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.active[reservation.lease.DeviceID] != session {
		return errRetainedImportLeaseRejected
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.state != retainedImportBuilding || session.scheduler != nil {
		return errRetainedImportLeaseRejected
	}
	session.state = retainedImportReserved
	return nil
}

// abort rolls back only an exact reservation which has not transferred to a
// scheduler builder. Counters remain burned. Once a scheduler is Prepared the
// authority owns it and callers must commit or use abortPrepared; plain abort
// cannot orphan that resource. Same-reservation repeats are idempotent and
// cannot delete a successor which later owns the same device key.
func (authority *retainedImportAuthority) abort(
	reservation *retainedImportReservation,
) error {
	if !authority.authenticatesReservation(reservation) {
		return errRetainedImportLeaseRejected
	}
	session := reservation.session
	authority.mu.Lock()
	defer authority.mu.Unlock()
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.state == retainedImportReservationAborted &&
		session.released.Load() {
		return nil
	}
	if authority.active[reservation.lease.DeviceID] != session ||
		session.state != retainedImportReserved {
		return errRetainedImportLeaseRejected
	}
	delete(authority.active, reservation.lease.DeviceID)
	session.state = retainedImportReservationAborted
	session.released.Store(true)
	return nil
}

// abortPrepared is the exact bounded cancellation path after a parked
// scheduler has transferred to authority ownership but before ingress/Bind.
func (authority *retainedImportAuthority) abortPrepared(
	reservation *retainedImportReservation,
	deadline time.Time,
) error {
	if !authority.authenticatesReservation(reservation) {
		return errRetainedImportLeaseRejected
	}
	session := reservation.session
	session.mu.Lock()
	if session.state == retainedImportReservationAborted &&
		session.released.Load() {
		session.mu.Unlock()
		return nil
	}
	if session.preparedAbortDone != nil &&
		(session.state == retainedImportPreparedAborting ||
			session.state == retainedImportQuarantined) {
		done := session.preparedAbortDone
		session.mu.Unlock()
		return session.awaitPreparedAbort(done, deadline)
	}
	session.mu.Unlock()
	if !deadline.After(time.Now()) {
		return errRetainedImportLeaseRejected
	}
	authority.mu.Lock()
	session.mu.Lock()
	if session.state == retainedImportReservationAborted &&
		session.released.Load() {
		session.mu.Unlock()
		authority.mu.Unlock()
		return nil
	}
	if authority.active[reservation.lease.DeviceID] != session {
		session.mu.Unlock()
		authority.mu.Unlock()
		return errRetainedImportLeaseRejected
	}
	if session.state == retainedImportPreparedAborting {
		done := session.preparedAbortDone
		session.mu.Unlock()
		authority.mu.Unlock()
		return session.awaitPreparedAbort(done, deadline)
	}
	if session.state == retainedImportQuarantined &&
		session.preparedAbortDone != nil {
		done := session.preparedAbortDone
		session.mu.Unlock()
		authority.mu.Unlock()
		return session.awaitPreparedAbort(done, deadline)
	}
	if session.state != retainedImportPrepared ||
		interfaceIsNil(session.scheduler) {
		session.mu.Unlock()
		authority.mu.Unlock()
		return errRetainedImportLeaseRejected
	}
	session.state = retainedImportPreparedAborting
	session.preparedAbortDone = make(chan struct{})
	scheduler := session.scheduler
	session.mu.Unlock()
	authority.mu.Unlock()

	var cleanupErr error
	if err := session.runVoidStep(
		"fence prepared scheduler admission", deadline,
		func() error { return scheduler.closeAdmission(reservation) }); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if err := session.runVoidStep(
		"close prepared scheduler", deadline, scheduler.close); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if cleanupErr != nil {
		session.quarantined.Store(true)
		session.mu.Lock()
		session.state = retainedImportQuarantined
		session.preparedAbortErr = cleanupErr
		close(session.preparedAbortDone)
		session.mu.Unlock()
		return cleanupErr
	}

	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.active[reservation.lease.DeviceID] != session {
		session.quarantined.Store(true)
		session.mu.Lock()
		session.state = retainedImportQuarantined
		session.preparedAbortErr = errRetainedImportLeaseRejected
		close(session.preparedAbortDone)
		session.mu.Unlock()
		return errRetainedImportLeaseRejected
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.state != retainedImportPreparedAborting {
		session.quarantined.Store(true)
		session.state = retainedImportQuarantined
		session.preparedAbortErr = errRetainedImportLeaseRejected
		close(session.preparedAbortDone)
		return errRetainedImportLeaseRejected
	}
	delete(authority.active, reservation.lease.DeviceID)
	session.state = retainedImportReservationAborted
	session.released.Store(true)
	close(session.preparedAbortDone)
	return nil
}

func (session *retainedImportSession) awaitPreparedAbort(
	done <-chan struct{},
	deadline time.Time,
) error {
	if done == nil {
		return errRetainedImportLeaseRejected
	}
	select {
	case <-done:
		session.mu.Lock()
		err := session.preparedAbortErr
		session.mu.Unlock()
		return err
	default:
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errRetainedImportCloseTimedOut
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		session.mu.Lock()
		err := session.preparedAbortErr
		session.mu.Unlock()
		return err
	case <-timer.C:
		return errRetainedImportCloseTimedOut
	}
}

func (authority *retainedImportAuthority) authenticatesReservation(
	reservation *retainedImportReservation,
) bool {
	return reservation != nil && reservation.authority == authority &&
		reservation.session != nil && reservation.session.authority == authority &&
		reservation.session.reservation == reservation &&
		reservation.lease.Valid() &&
		reservation.lease == reservation.session.lease
}

func (authority *retainedImportAuthority) claimAvailabilityLocked(
	deviceID uint64,
) error {
	if current := authority.active[deviceID]; current != nil {
		if current.quarantined.Load() {
			return errRetainedImportQuarantined
		}
		return errRetainedImportBusy
	}
	if authority.nextImportToken == ^uint64(0) ||
		authority.nextSessionGeneration == ^uint64(0) {
		return errRetainedImportCounterExhausted
	}
	return nil
}

type retainedImportSession struct {
	authority      *retainedImportAuthority
	reservation    *retainedImportReservation
	lease          retainedusb.ImportLease
	owner          retainedImportCombinedOwner
	ownerReference retainedImportOwnerReference
	ingress        retainedImportIngress
	scheduler      retainedImportScheduler

	mu           sync.Mutex
	state        retainedImportLifecycleState
	closeStarted bool
	closeReason  retainedusb.ImportCloseReason
	closeResult  retainedusb.ImportDisconnectResult
	closeErr     error
	closeDone    chan struct{}

	nextResetGeneration uint64
	issuedReset         retainedusb.ImportResetLease
	activeReset         retainedusb.ImportResetLease
	lastReset           retainedusb.ImportResetLease
	lastResetResult     retainedusb.ImportResetResult
	lastResetErr        error
	resetDone           chan struct{}

	preparedAbortDone chan struct{}
	preparedAbortErr  error

	quarantined atomic.Bool
	released    atomic.Bool
}

func (session *retainedImportSession) quarantineBind() {
	session.quarantined.Store(true)
	session.setState(retainedImportQuarantined)
}

func (session *retainedImportSession) failCommit(
	stage string,
	cause error,
	cleanupDeadline time.Time,
	ownerMayBeBound bool,
) error {
	session.quarantineBind()
	failure := fmt.Errorf(
		"%w: %s: %w", errRetainedImportLifecycle, stage, cause)
	cleanupErr := session.cleanupUnpublished(cleanupDeadline)
	if ownerMayBeBound {
		if cleanupErr != nil {
			cleanupErr = errors.Join(cleanupErr,
				session.containBoundOwner(
					retainedusb.ImportCloseInvariantFailure,
					cleanupDeadline))
		} else {
			cleanupErr = session.cleanupBoundOwner(cleanupDeadline)
		}
	}
	if cleanupErr != nil {
		return errors.Join(failure, cleanupErr)
	}
	return failure
}

func (session *retainedImportSession) failBoundCommit(
	cause error,
	cleanupDeadline time.Time,
) error {
	session.quarantineBind()
	failure := fmt.Errorf(
		"%w: activate retained scheduler: %w",
		errRetainedImportLifecycle, cause)
	cleanupErr := session.cleanupUnpublished(cleanupDeadline)
	if cleanupErr != nil {
		cleanupErr = errors.Join(cleanupErr,
			session.containBoundOwner(
				retainedusb.ImportCloseInvariantFailure,
				cleanupDeadline))
	} else {
		cleanupErr = session.cleanupBoundOwner(cleanupDeadline)
	}
	if cleanupErr != nil {
		return errors.Join(failure, cleanupErr)
	}
	return failure
}

func (session *retainedImportSession) cleanupBoundOwner(
	deadline time.Time,
) error {
	drain, drainErr := session.cancelAndDrainBoundOwner(
		retainedusb.ImportCloseInvariantFailure, deadline)
	if drain.State == retainedusb.ImportDrainQuarantined {
		return errors.Join(drainErr, fmt.Errorf(
			"%w: bound owner drain remained quarantined",
			errRetainedImportQuarantined))
	}
	if drainErr != nil {
		return drainErr
	}
	disconnect, disconnectErr := invokeRetainedImportStep(
		"disconnect neutral bound owner", deadline,
		func() (retainedusb.ImportDisconnectResult, error) {
			return session.owner.DisconnectNeutral(
				session.lease, retainedusb.ImportCloseInvariantFailure, deadline)
		})
	if disconnectErr != nil || disconnect.Lease != session.lease ||
		disconnect.Reason != retainedusb.ImportCloseInvariantFailure ||
		disconnect.State != retainedusb.ImportDisconnectSafe {
		if disconnectErr == nil {
			disconnectErr = errRetainedImportLeaseRejected
		}
		return fmt.Errorf(
			"%w: bound owner disconnect: %w",
			errRetainedImportLifecycle, disconnectErr)
	}
	return nil
}

// containBoundOwner is the failure-only cancellation boundary. It is valid
// after the transport has attempted its outer admission fence even when that
// fence or scheduler close/join is unproven: the ImportSessionOwner contract
// requires CancelAndDrain to synchronously fence its own Stage/Prepare
// admission. Exact Drained and Quarantined results both prove the cancellation
// request reached the owner, but neither permits neutralization or release on
// this path.
func (session *retainedImportSession) containBoundOwner(
	reason retainedusb.ImportCloseReason,
	deadline time.Time,
) error {
	_, err := session.cancelAndDrainBoundOwner(reason, deadline)
	return err
}

func (session *retainedImportSession) cancelAndDrainBoundOwner(
	reason retainedusb.ImportCloseReason,
	deadline time.Time,
) (retainedusb.ImportDrainResult, error) {
	drain, drainErr := invokeRetainedImportStep(
		"cancel and drain bound owner", deadline,
		func() (retainedusb.ImportDrainResult, error) {
			return session.owner.CancelAndDrain(
				session.lease, reason, deadline)
		})
	if drain.Lease != session.lease || drain.Reason != reason ||
		(drain.State != retainedusb.ImportDrainDrained &&
			drain.State != retainedusb.ImportDrainQuarantined) {
		drainErr = errors.Join(drainErr, errRetainedImportLeaseRejected)
		return retainedusb.ImportDrainResult{}, fmt.Errorf(
			"%w: bound owner drain: %w",
			errRetainedImportLifecycle, drainErr)
	}
	if drainErr != nil {
		return drain, fmt.Errorf(
			"%w: bound owner drain callback: %w",
			errRetainedImportLifecycle, drainErr)
	}
	return drain, nil
}

func (session *retainedImportSession) cleanupUnpublished(
	deadline time.Time,
) error {
	if session == nil || interfaceIsNil(session.ingress) ||
		interfaceIsNil(session.scheduler) {
		return errRetainedImportInvalid
	}
	var cleanupErr error
	if err := session.runVoidStep(
		"fence unpublished admission", deadline,
		func() error {
			return session.scheduler.closeAdmission(session.reservation)
		}); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if err := session.runVoidStep(
		"stop unpublished ingress", deadline, session.ingress.Close); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if err := session.runVoidStep(
		"close unpublished retained scheduler", deadline,
		session.scheduler.close); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	return cleanupErr
}

func (session *retainedImportSession) rollbackRejectedBind() error {
	if session == nil || session.authority == nil {
		return errRetainedImportInvalid
	}
	authority := session.authority
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.active[session.lease.DeviceID] != session {
		return errRetainedImportLeaseRejected
	}
	delete(authority.active, session.lease.DeviceID)
	session.released.Store(true)
	return nil
}

func (session *retainedImportSession) authenticates(
	lease retainedusb.ImportLease,
) bool {
	return session != nil && lease.Valid() && lease == session.lease &&
		session.authority != nil &&
		lease.AuthorityID == session.authority.identity
}

func (session *retainedImportSession) stateFor(
	lease retainedusb.ImportLease,
) (retainedImportLifecycleState, error) {
	if !session.authenticates(lease) {
		return 0, errRetainedImportLeaseRejected
	}
	session.mu.Lock()
	state := session.state
	session.mu.Unlock()
	return state, nil
}

// issueReset allocates one authority-wide nonreused token and one exact
// per-import successor reset generation. Issuance does not fence admission or
// mutate device state; reset owns that transition. This dormant seam has no
// production USB/IP call site.
func (session *retainedImportSession) issueReset(
	lease retainedusb.ImportLease,
	deadline time.Time,
) (retainedusb.ImportResetLease, error) {
	if !session.authenticates(lease) || !deadline.After(time.Now()) {
		return retainedusb.ImportResetLease{}, errRetainedImportResetRejected
	}
	authority := session.authority
	authority.mu.Lock()
	defer authority.mu.Unlock()
	session.mu.Lock()
	defer session.mu.Unlock()
	if authority.active[lease.DeviceID] != session ||
		session.state != retainedImportClaimed || session.closeStarted ||
		session.quarantined.Load() || session.released.Load() ||
		session.issuedReset.Valid() || session.activeReset.Valid() ||
		authority.nextResetToken == ^uint64(0) ||
		session.nextResetGeneration == ^uint64(0) {
		if authority.nextResetToken == ^uint64(0) ||
			session.nextResetGeneration == ^uint64(0) {
			return retainedusb.ImportResetLease{},
				errRetainedImportCounterExhausted
		}
		return retainedusb.ImportResetLease{}, errRetainedImportResetRejected
	}
	authority.nextResetToken++
	session.nextResetGeneration++
	reset := retainedusb.ImportResetLease{
		ImportLease: lease, ResetToken: authority.nextResetToken,
		ResetGeneration: session.nextResetGeneration,
	}
	session.issuedReset = reset
	return reset, nil
}

// reset performs the exact dormant reversible transaction. Terminal import
// close callbacks are not reused: scheduler and owner must both implement
// their separately typed reset contracts. A same-capability repeat waits for
// or returns the one cached terminal result; stale and cross-import values are
// rejected without effects.
func (session *retainedImportSession) reset(
	reset retainedusb.ImportResetLease,
	deadline time.Time,
) (retainedusb.ImportResetResult, error) {
	invalid := retainedusb.ImportResetResult{
		Lease: reset, State: retainedusb.ImportResetInvalid,
	}
	if !reset.Valid() || !session.authenticates(reset.ImportLease) {
		return invalid, errRetainedImportResetRejected
	}

	session.mu.Lock()
	if reset == session.lastReset && session.lastResetResult.Lease == reset {
		result, err := session.lastResetResult, session.lastResetErr
		session.mu.Unlock()
		return result, err
	}
	if !deadline.After(time.Now()) {
		session.mu.Unlock()
		return invalid, errRetainedImportResetRejected
	}
	if session.activeReset.Valid() {
		if session.activeReset != reset {
			session.mu.Unlock()
			return invalid, errRetainedImportResetRejected
		}
		done := session.resetDone
		session.mu.Unlock()
		return session.awaitReset(done, deadline)
	}
	if session.state != retainedImportClaimed || session.closeStarted ||
		session.quarantined.Load() || session.released.Load() ||
		session.issuedReset != reset {
		session.mu.Unlock()
		return invalid, errRetainedImportResetRejected
	}
	resetScheduler, schedulerOK := session.scheduler.(retainedImportResetScheduler)
	resetOwner, ownerOK := session.owner.(retainedusb.ImportResetSessionOwner)
	if !schedulerOK || !ownerOK || interfaceIsNil(resetScheduler) ||
		interfaceIsNil(resetOwner) {
		session.mu.Unlock()
		return invalid, errRetainedImportResetRejected
	}
	session.issuedReset = retainedusb.ImportResetLease{}
	session.activeReset = reset
	session.resetDone = make(chan struct{})
	session.state = retainedImportResetting
	session.mu.Unlock()

	if err := invokeRetainedResetFence(
		resetScheduler, session.reservation, reset); err != nil {
		return session.finishResetQuarantined(
			reset, deadline, fmt.Errorf(
				"%w: fence scheduler reset ingress: %w",
				errRetainedImportLifecycle, err))
	}
	if err := session.runVoidStep(
		"fence and drain owner for reset", deadline,
		func() error {
			return resetOwner.FenceAndDrainReset(reset, deadline)
		}); err != nil {
		return session.finishResetQuarantined(
			reset, deadline, err)
	}
	if err := session.runVoidStep(
		"drain retained scheduler for reset", deadline,
		func() error {
			return resetScheduler.resetAndDrain(
				session.reservation, reset, deadline)
		}); err != nil {
		return session.finishResetQuarantined(
			reset, deadline, err)
	}
	ownerResult, err := invokeRetainedImportStep(
		"reset and restart owner", deadline,
		func() (retainedusb.ImportResetResult, error) {
			return resetOwner.ResetAndRestart(reset, deadline)
		})
	if err != nil || ownerResult.Lease != reset ||
		ownerResult.State != retainedusb.ImportResetSafe {
		if err == nil {
			err = errRetainedImportResetRejected
		}
		return session.finishResetQuarantined(
			reset, deadline, fmt.Errorf(
				"%w: owner reset: %w", errRetainedImportLifecycle, err))
	}
	if err := invokeRetainedResetReopen(
		resetScheduler, session.reservation, reset); err != nil {
		return session.finishResetQuarantined(
			reset, deadline, fmt.Errorf(
				"%w: reopen after reset: %w",
				errRetainedImportLifecycle, err))
	}

	session.mu.Lock()
	if session.state != retainedImportResetting ||
		session.activeReset != reset || session.resetDone == nil {
		session.mu.Unlock()
		return session.finishResetQuarantined(
			reset, deadline, errRetainedImportResetRejected)
	}
	result := retainedusb.ImportResetResult{
		Lease: reset, State: retainedusb.ImportResetSafe,
	}
	done := session.resetDone
	session.state = retainedImportClaimed
	session.lastReset = reset
	session.lastResetResult = result
	session.lastResetErr = nil
	session.activeReset = retainedusb.ImportResetLease{}
	close(done)
	session.mu.Unlock()
	return result, nil
}

func invokeRetainedResetFence(
	scheduler retainedImportResetScheduler,
	reservation *retainedImportReservation,
	reset retainedusb.ImportResetLease,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = retainedImportCallbackPanic(
				"fence scheduler reset ingress", recovered)
		}
	}()
	return scheduler.fenceReset(reservation, reset)
}

func (session *retainedImportSession) awaitReset(
	done <-chan struct{},
	deadline time.Time,
) (retainedusb.ImportResetResult, error) {
	if done == nil {
		return retainedusb.ImportResetResult{}, errRetainedImportResetRejected
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return retainedusb.ImportResetResult{}, errRetainedImportCloseTimedOut
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		session.mu.Lock()
		result, err := session.lastResetResult, session.lastResetErr
		session.mu.Unlock()
		return result, err
	case <-timer.C:
		return retainedusb.ImportResetResult{}, errRetainedImportCloseTimedOut
	}
}

func invokeRetainedResetReopen(
	scheduler retainedImportResetScheduler,
	reservation *retainedImportReservation,
	reset retainedusb.ImportResetLease,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = retainedImportCallbackPanic(
				"reopen after reset", recovered)
		}
	}()
	return scheduler.reopenAfterReset(reservation, reset)
}

func (session *retainedImportSession) finishResetQuarantined(
	reset retainedusb.ImportResetLease,
	deadline time.Time,
	cause error,
) (retainedusb.ImportResetResult, error) {
	// Revoke the exact reservation synchronously, before any callback or lock
	// which can outlive the reset budget. In particular, close may never start
	// once deadline expires, and a late resetAndDrain callback must not retain
	// authority to reopen this quarantined import. This fence cannot wait for
	// scheduler.mu, which a predecessor Prepare may still own.
	session.reservation.admission.activation.Store(retainedImportActivationRevoked)
	session.reservation.admission.open.Store(false)
	session.quarantined.Store(true)
	containmentErr := session.runVoidStep(
		"close scheduler after reset failure", deadline, session.scheduler.close)
	containmentErr = errors.Join(containmentErr, session.containBoundOwner(
		retainedusb.ImportCloseInvariantFailure, deadline))
	err := errors.Join(cause, containmentErr)
	result := retainedusb.ImportResetResult{
		Lease: reset, State: retainedusb.ImportResetQuarantined,
	}
	session.mu.Lock()
	done := session.resetDone
	session.state = retainedImportQuarantined
	session.lastReset = reset
	session.lastResetResult = result
	session.lastResetErr = err
	session.activeReset = retainedusb.ImportResetLease{}
	if session.closeStarted {
		session.closeResult = retainedusb.ImportDisconnectResult{
			Lease: session.lease, Reason: session.closeReason,
			State: retainedusb.ImportDisconnectQuarantined,
		}
		session.closeErr = err
		close(session.closeDone)
	}
	if done != nil {
		close(done)
	}
	session.mu.Unlock()
	return result, err
}

// publishClaimed makes the exact lifecycle state visible before opening the
// admission gate while holding the same lock which starts close. The
// scheduler performs the gate-open synchronously under its own lifecycle
// mutex, so a worker failure which already closed or recorded failure wins the
// same linearization and rejects commit. A failure after gate-open is an
// ordinary post-claim transport failure. Thus close cannot interleave between
// Claimed publication and gate-open, and an enqueue which observes the open
// gate can never precede Claimed.
func (session *retainedImportSession) publishClaimed() error {
	if session == nil || session.reservation == nil {
		return errRetainedImportInvalid
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.state != retainedImportCommitting ||
		session.closeStarted || session.quarantined.Load() ||
		session.reservation.admission.open.Load() {
		return errRetainedImportLeaseRejected
	}
	session.state = retainedImportClaimed
	err := invokeRetainedOpenAdmission(
		session.scheduler, session.reservation)
	if err == nil && (!session.reservation.admission.open.Load() ||
		session.reservation.admission.activation.Load() !=
			retainedImportActivationStarted) {
		err = errRetainedImportLeaseRejected
	}
	if err != nil {
		// A malformed internal implementation cannot leave a partially open
		// capability behind while the outer failure cleanup is scheduled.
		session.reservation.admission.open.Store(false)
		session.reservation.admission.activation.Store(
			retainedImportActivationRevoked)
		// No observer can see this tentative state while session.mu is held,
		// and openAdmission guarantees the gate remains closed on failure.
		session.state = retainedImportCommitting
		return err
	}
	return nil
}

// invokeRetainedOpenAdmission catches an internal scheduler implementation
// panic but intentionally supplies no timeout goroutine. openAdmission is a
// trusted, synchronous, nonblocking mutex/validation operation; allowing a
// timed-out invocation to return later and open the gate would recreate the
// exact late-publication race this boundary prevents.
func invokeRetainedOpenAdmission(
	scheduler retainedImportScheduler,
	reservation *retainedImportReservation,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = retainedCallbackPanic("open import admission", recovered)
		}
	}()
	return scheduler.openAdmission(reservation)
}

// close executes the only permitted whole-import close order. The first valid
// call latches reason. Same-reason repeats return or wait for that exact
// result; a different reason is rejected and cannot rewrite history.
func (session *retainedImportSession) close(
	lease retainedusb.ImportLease,
	reason retainedusb.ImportCloseReason,
	deadline time.Time,
) (retainedusb.ImportDisconnectResult, error) {
	if !session.authenticates(lease) || !reason.Valid() {
		return retainedusb.ImportDisconnectResult{},
			errRetainedImportLeaseRejected
	}

	session.mu.Lock()
	if session.state == retainedImportResetting &&
		session.activeReset.Valid() {
		// A close which arrives behind an authoritative reset must still win
		// against every later reset and close reason. Latch the immutable intent
		// before waiting, but do not make the caller's wait deadline the owning
		// reset's deadline: a short waiter may time out without quarantining or
		// otherwise mutating that reset. The same reason can resume the terminal
		// close after reset reaches its exact Safe boundary.
		if session.closeStarted {
			if session.closeReason != reason {
				session.mu.Unlock()
				return retainedusb.ImportDisconnectResult{},
					errRetainedImportReasonMismatch
			}
		} else {
			if !deadline.After(time.Now()) {
				session.mu.Unlock()
				return retainedusb.ImportDisconnectResult{},
					errRetainedImportCloseTimedOut
			}
			session.closeStarted = true
			session.closeReason = reason
		}
		done := session.resetDone
		session.mu.Unlock()
		resetResult, resetErr := session.awaitReset(done, deadline)
		if resetErr != nil || resetResult.State != retainedusb.ImportResetSafe {
			if errors.Is(resetErr, errRetainedImportCloseTimedOut) &&
				resetResult.State == retainedusb.ImportResetInvalid {
				return retainedusb.ImportDisconnectResult{}, resetErr
			}
			if resetErr == nil {
				resetErr = errRetainedImportQuarantined
			}
			return retainedusb.ImportDisconnectResult{
				Lease: lease, Reason: reason,
				State: retainedusb.ImportDisconnectQuarantined,
			}, resetErr
		}
		return session.close(lease, reason, deadline)
	}
	if session.closeStarted {
		if session.closeReason != reason {
			session.mu.Unlock()
			return retainedusb.ImportDisconnectResult{},
				errRetainedImportReasonMismatch
		}
		if session.state == retainedImportClaimed {
			if !deadline.After(time.Now()) {
				session.mu.Unlock()
				return retainedusb.ImportDisconnectResult{},
					errRetainedImportCloseTimedOut
			}
			// This is the exact continuation of a close intent which first arrived
			// during reset. Only one same-reason caller can change Claimed to
			// Closing; every concurrent repeat observes Closing and waits below.
			session.state = retainedImportClosing
			session.mu.Unlock()
			return session.executeClose(lease, reason, deadline)
		}
		done := session.closeDone
		session.mu.Unlock()
		return session.awaitClose(done, deadline)
	}
	if session.state != retainedImportClaimed {
		session.mu.Unlock()
		return retainedusb.ImportDisconnectResult{},
			errRetainedImportLeaseRejected
	}
	if !deadline.After(time.Now()) {
		session.mu.Unlock()
		return retainedusb.ImportDisconnectResult{},
			errRetainedImportInvalid
	}
	session.closeStarted = true
	session.closeReason = reason
	session.state = retainedImportClosing
	session.mu.Unlock()
	return session.executeClose(lease, reason, deadline)
}

func (session *retainedImportSession) executeClose(
	lease retainedusb.ImportLease,
	reason retainedusb.ImportCloseReason,
	deadline time.Time,
) (retainedusb.ImportDisconnectResult, error) {
	var transportErr error
	if err := session.runVoidStep(
		"fence import admission", deadline,
		func() error {
			return session.scheduler.closeAdmission(session.reservation)
		}); err != nil {
		transportErr = errors.Join(transportErr, err)
	}
	if err := session.runVoidStep(
		"stop ingress", deadline, session.ingress.Close); err != nil {
		transportErr = errors.Join(transportErr, err)
	} else {
		session.setState(retainedImportIngressStopped)
	}
	if err := session.runVoidStep(
		"close retained scheduler", deadline, session.scheduler.close); err != nil {
		transportErr = errors.Join(transportErr, err)
	} else if transportErr == nil {
		session.setState(retainedImportSchedulerClosed)
	}
	if transportErr != nil {
		containmentErr := session.containBoundOwner(reason, deadline)
		return session.finishQuarantined(
			reason, errors.Join(transportErr, containmentErr))
	}

	drain, err := invokeRetainedImportStep(
		"cancel and drain local executor", deadline,
		func() (retainedusb.ImportDrainResult, error) {
			return session.owner.CancelAndDrain(lease, reason, deadline)
		})
	if err != nil || drain.Lease != lease || drain.Reason != reason ||
		drain.State != retainedusb.ImportDrainDrained {
		if err == nil {
			err = errRetainedImportLeaseRejected
		}
		return session.finishQuarantined(reason, fmt.Errorf(
			"%w: cancel and drain local executor: %w",
			errRetainedImportLifecycle, err))
	}
	session.setState(retainedImportLocalDrained)

	disconnect, err := invokeRetainedImportStep(
		"disconnect neutral", deadline,
		func() (retainedusb.ImportDisconnectResult, error) {
			return session.owner.DisconnectNeutral(lease, reason, deadline)
		})
	if err != nil || disconnect.Lease != lease ||
		disconnect.Reason != reason ||
		disconnect.State != retainedusb.ImportDisconnectSafe {
		if err == nil {
			if disconnect.State == retainedusb.ImportDisconnectQuarantined &&
				disconnect.Lease == lease && disconnect.Reason == reason {
				err = errRetainedImportQuarantined
			} else {
				err = errRetainedImportLeaseRejected
			}
		}
		return session.finishQuarantined(reason, fmt.Errorf(
			"%w: disconnect neutral: %w",
			errRetainedImportLifecycle, err))
	}
	session.setState(retainedImportNeutralized)
	if err := session.releaseExact(lease); err != nil {
		return session.finishQuarantined(reason, fmt.Errorf(
			"%w: release import: %w", errRetainedImportLifecycle, err))
	}
	return session.finishReleased(disconnect)
}

func (session *retainedImportSession) awaitClose(
	done <-chan struct{},
	deadline time.Time,
) (retainedusb.ImportDisconnectResult, error) {
	select {
	case <-done:
		session.mu.Lock()
		result, err := session.closeResult, session.closeErr
		session.mu.Unlock()
		return result, err
	default:
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return retainedusb.ImportDisconnectResult{},
			errRetainedImportCloseTimedOut
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		session.mu.Lock()
		result, err := session.closeResult, session.closeErr
		session.mu.Unlock()
		return result, err
	case <-timer.C:
		// This waiter does not mutate the lifecycle. The owning close call has
		// its own absolute deadline and will either release or quarantine.
		return retainedusb.ImportDisconnectResult{},
			errRetainedImportCloseTimedOut
	}
}

func (session *retainedImportSession) runVoidStep(
	name string,
	deadline time.Time,
	callback func() error,
) error {
	_, err := invokeRetainedImportStep(
		name, deadline, func() (struct{}, error) {
			return struct{}{}, callback()
		})
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errRetainedImportLifecycle, name, err)
	}
	return nil
}

func (session *retainedImportSession) setState(
	state retainedImportLifecycleState,
) {
	session.mu.Lock()
	session.state = state
	session.mu.Unlock()
}

func (session *retainedImportSession) finishQuarantined(
	reason retainedusb.ImportCloseReason,
	err error,
) (retainedusb.ImportDisconnectResult, error) {
	result := retainedusb.ImportDisconnectResult{
		Lease: session.lease, Reason: reason,
		State: retainedusb.ImportDisconnectQuarantined,
	}
	session.quarantined.Store(true)
	session.mu.Lock()
	session.state = retainedImportQuarantined
	session.closeResult = result
	session.closeErr = err
	close(session.closeDone)
	session.mu.Unlock()
	return result, err
}

func (session *retainedImportSession) finishReleased(
	result retainedusb.ImportDisconnectResult,
) (retainedusb.ImportDisconnectResult, error) {
	session.mu.Lock()
	session.state = retainedImportReleased
	session.closeResult = result
	session.closeErr = nil
	close(session.closeDone)
	session.mu.Unlock()
	return result, nil
}

// releaseExact is intentionally callable only after the neutral terminal
// state. A repeated release of this retired session is a no-op. In particular,
// it never removes a successor which now owns the same DeviceID.
func (session *retainedImportSession) releaseExact(
	lease retainedusb.ImportLease,
) error {
	if !session.authenticates(lease) {
		return errRetainedImportLeaseRejected
	}
	session.mu.Lock()
	state := session.state
	session.mu.Unlock()
	if state == retainedImportReleased || session.released.Load() {
		return nil
	}
	if state != retainedImportNeutralized {
		return errRetainedImportUnsafeRelease
	}

	authority := session.authority
	authority.mu.Lock()
	current := authority.active[lease.DeviceID]
	if current != session {
		authority.mu.Unlock()
		if session.released.Load() {
			return nil
		}
		return errRetainedImportLeaseRejected
	}
	delete(authority.active, lease.DeviceID)
	session.released.Store(true)
	authority.mu.Unlock()
	return nil
}

type retainedImportStepResult[T any] struct {
	value      T
	err        error
	finishedAt time.Time
}

// invokeRetainedImportStep contains both panic and timeout at the outer
// boundary. A timed-out callback may still return into its buffered result
// channel, but the exact session remains strongly referenced and quarantined;
// no later phase or successor session can overtake it.
func invokeRetainedImportStep[T any](
	name string,
	deadline time.Time,
	callback func() (T, error),
) (T, error) {
	var zero T
	if callback == nil || !deadline.After(time.Now()) {
		return zero, errRetainedImportCloseTimedOut
	}
	result := make(chan retainedImportStepResult[T], 1)
	go func() {
		completed := retainedImportStepResult[T]{}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					completed.err = retainedImportCallbackPanic(name, recovered)
				}
			}()
			completed.value, completed.err = callback()
		}()
		completed.finishedAt = time.Now()
		result <- completed
	}()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case completed := <-result:
		if completed.finishedAt.After(deadline) {
			return zero, fmt.Errorf(
				"%w: %s", errRetainedImportCloseTimedOut, name)
		}
		return completed.value, completed.err
	case <-timer.C:
		return zero, fmt.Errorf(
			"%w: %s", errRetainedImportCloseTimedOut, name)
	}
}

func retainedImportCallbackPanic(name string, recovered any) error {
	if recoveredErr, ok := recovered.(error); ok {
		return fmt.Errorf("%s callback panic: %w", name, recoveredErr)
	}
	return fmt.Errorf("%s callback panic: %v", name, recovered)
}

func interfaceIsNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

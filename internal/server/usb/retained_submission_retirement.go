package usb

import (
	"errors"
	"fmt"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
)

var errRetainedSubmissionOwnerRetirement = errors.New("retained USB owner requested import retirement")

// pollImportRetirement runs outside scheduler.mu. Only this authenticated
// query can create a drainable stop; matching text, a wrapped marker, or an
// ordinary Stage/Prepare/Complete error never proves safe ownership.
func (scheduler *retainedSubmissionScheduler) pollImportRetirement() error {
	owner, supported := scheduler.owner.(retainedusb.ImportRetirementOwner)
	if !supported {
		return nil
	}
	request, err := invokeRetainedImportRetirement(owner)
	if err != nil {
		// No callback error can impersonate the private successful-query
		// marker, even if it retained an error from an earlier lifecycle.
		return fmt.Errorf("retained owner retirement query: %w", err)
	}
	if request == (retainedusb.ImportRetirementRequest{}) {
		return nil
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if !request.Valid() || scheduler.importReservation == nil ||
		scheduler.importAdmission != &scheduler.importReservation.admission ||
		request.Lease != scheduler.importReservation.lease ||
		request.Lease.OwnerID != scheduler.ownerIdentity ||
		request.Lease.SessionGeneration != scheduler.session ||
		!scheduler.importActivated || !scheduler.started {
		return errRetainedImportLeaseRejected
	}
	if scheduler.failure != nil {
		return scheduler.failure
	}
	if scheduler.retirement.Valid() && scheduler.retirement != request {
		return errRetainedImportLeaseRejected
	}
	if scheduler.closed || scheduler.ctx.Err() != nil {
		return errRetainedSubmissionClosed
	}
	scheduler.retirement = request
	// No successor can enter while the worker retires the exact queued work.
	// Do not latch failure: only a failed drain/neutral boundary makes this
	// otherwise-known input-history terminal into a quarantine.
	scheduler.importAdmission.activation.Store(retainedImportActivationRevoked)
	scheduler.importAdmission.open.Store(false)
	return errRetainedSubmissionOwnerRetirement
}

func invokeRetainedImportRetirement(owner retainedusb.ImportRetirementOwner) (request retainedusb.ImportRetirementRequest, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			request = retainedusb.ImportRetirementRequest{}
			err = retainedCallbackPanic("import retirement", recovered)
		}
	}()
	return owner.RetainedImportRetirement()
}

// finishWorkerFailure preserves the distinction between a diagnostic reason
// to end a presentation lifetime and failed ownership containment. A ticket
// retirement failure always enters failure, even after an owner request.
func (scheduler *retainedSubmissionScheduler) finishWorkerFailure(err error, now time.Time) {
	reason := retainedusb.RetireInvariantFailure
	scheduler.mu.Lock()
	requested := scheduler.retirement.Valid()
	scheduler.mu.Unlock()
	if err == errRetainedSubmissionOwnerRetirement && requested {
		reason = retainedusb.RetireOwnerRequested
	} else {
		scheduler.recordFailure(err)
	}
	if shutdownErr := scheduler.shutdown(reason, now); shutdownErr != nil {
		scheduler.recordFailure(shutdownErr)
	}
}

func retainedImportRetirementDiagnostic(request retainedusb.ImportRetirementRequest) error {
	if !request.Valid() {
		return nil
	}
	return fmt.Errorf("%w: %s", errRetainedSubmissionOwnerRetirement, request.Reason)
}

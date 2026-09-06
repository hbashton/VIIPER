package xboxone

import (
	"errors"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
)

var errRetainedInputHistoryFault = errors.New("xboxone: retained input history fault; import must retire")

// retainedInputJournal is the semantic half of the engine's exact USB input
// transaction. The shared scheduler owns button/trigger ordering; the persona
// engine alone owns GIP bytes and sequence numbers. A failed USB write retains
// BOTH claims until the exact engine retry delivers, or a lifecycle boundary
// retires them. Broker acceptance never commits a presentation.
//
// Lock order is adapter.mu -> journal.mu or coordinator.mu -> journal.mu.
// The journal never calls either owner, performs I/O, or allocates on its hot path.
type retainedInputJournal struct {
	mu       sync.Mutex
	source   *inputpresentation.FixedReportScheduler[GamepadInputReportV1]
	latest   GamepadInputReportV1
	claim    inputpresentation.Claim
	active   bool
	admitted bool
	faulted  bool
}

// This private scheduler image includes KeepAlive. It is not a broker wire
// revision and is never sent to Windows; GIP encoding remains metadata-owned.
const retainedInputSemanticSize = SemanticInputWireSize + 1

func encodeRetainedInputSemantic(state *GamepadInputReportV1, dst []byte) int {
	if len(dst) != retainedInputSemanticSize ||
		EncodeSemanticInputWireV1Into(dst[:SemanticInputWireSize], state.State) != nil {
		return 0
	}
	dst[SemanticInputWireSize] = 0
	if state.KeepAlive {
		dst[SemanticInputWireSize] = 1
	}
	return retainedInputSemanticSize
}

func retainedInputTransition(previous, next GamepadInputReportV1) bool {
	return encodeSemanticInputButtons(previous.State) != encodeSemanticInputButtons(next.State) ||
		(previous.State.LeftTrigger == 0) != (next.State.LeftTrigger == 0) ||
		(previous.State.RightTrigger == 0) != (next.State.RightTrigger == 0)
}

func newRetainedInputJournal(current GamepadInputReportV1) (*retainedInputJournal, error) {
	current.State.Guide = false // Command 7 has its separate ordered owner.
	source, err := inputpresentation.NewFixedReportSchedulerWithOverflowFault(
		retainedInputSemanticSize, GamepadInputReportV1{},
		encodeRetainedInputSemantic, retainedInputTransition, time.Now())
	if err != nil {
		return nil, err
	}
	return &retainedInputJournal{source: source, latest: current}, nil
}

func (journal *retainedInputJournal) publish(report GamepadInputReportV1) error {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.faulted {
		return errRetainedInputHistoryFault
	}
	if report == journal.latest && !report.KeepAlive {
		return nil
	}
	if journal.active {
		if result := journal.source.PublishWithLease(journal.source.ProducerLease(), report, time.Now()); !result.Accepted() {
			journal.faulted = true
			return errRetainedInputHistoryFault
		}
	}
	journal.latest = report
	return nil
}

func (journal *retainedInputJournal) selectReport(initial bool) (GamepadInputReportV1, bool, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.faulted {
		return GamepadInputReportV1{}, false, errRetainedInputHistoryFault
	}
	if initial || !journal.active {
		// Collapse only at selection. Publications arriving during the initial
		// USB write must remain behind this exact baseline, including on retry.
		if !journal.source.RetireInputPresentationGenerationWithBaseline(
			journal.source.Generation(), journal.latest, time.Now()) {
			return GamepadInputReportV1{}, false, ErrControllerPersonaInvariantViolation
		}
		journal.claim = inputpresentation.Claim{}
		journal.admitted = false
		journal.active = true
	}
	if !journal.claim.Valid() {
		// Publication and selection both own journal.mu, so this query and
		// claim are one atomic decision with respect to incoming input.
		if !journal.source.HasPendingInputPresentation() {
			return GamepadInputReportV1{}, false, nil
		}
		var scratch [retainedInputSemanticSize]byte
		journal.claim = journal.source.ClaimInputPresentation(scratch[:], time.Now())
	}
	state, ok := journal.source.StateForInputPresentationClaim(journal.claim)
	if !ok {
		return GamepadInputReportV1{}, false, ErrControllerPersonaInvariantViolation
	}
	return state, true, nil
}

func (journal *retainedInputJournal) admit() error {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.faulted {
		return errRetainedInputHistoryFault
	}
	// CanAdmit is intentionally one-shot. An engine-owned retry still belongs
	// to the same admitted semantic transaction; validate exact live ownership
	// again without selecting a newer image or committing the failed write.
	if _, ok := journal.source.StateForInputPresentationClaim(journal.claim); !ok {
		return ErrControllerPersonaInvariantViolation
	}
	if !journal.admitted && !journal.source.CanAdmitInputPresentation(journal.claim, time.Now()) {
		return ErrControllerPersonaInvariantViolation
	}
	journal.admitted = true
	return nil
}

func (journal *retainedInputJournal) failure() error {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.faulted {
		return errRetainedInputHistoryFault
	}
	return nil
}

func (journal *retainedInputJournal) complete(delivered bool) error {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.faulted {
		// Overflow revokes the Source claim, but cannot undo a GIP report
		// already admitted and copied before that fault. The coordinator has
		// authenticated and resolved that exact engine claim first. Retire
		// this paired bookkeeping once; never commit/resynchronize the faulted
		// Source or make an exact failed-write retry eligible again.
		if !journal.admitted || !journal.claim.Valid() {
			return ErrControllerPersonaInvariantViolation
		}
		journal.claim = inputpresentation.Claim{}
		journal.admitted = false
		return nil
	}
	if !delivered {
		return nil // engine retains the exact GIP retry and this Source claim
	}
	if !journal.source.ResolveInputPresentation(journal.claim, inputpresentation.OutcomeCommit, time.Now()) {
		return ErrControllerPersonaInvariantViolation
	}
	journal.claim = inputpresentation.Claim{}
	journal.admitted = false
	return nil
}

// retire is called only after the engine commits the corresponding lifecycle
// fence. A fault is terminal for this import and is not erased by START/reset.
func (journal *retainedInputJournal) retire() {
	journal.boundary(false)
}

func (journal *retainedInputJournal) boundary(resume bool) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.boundaryLocked(resume)
}

func (journal *retainedInputJournal) boundaryLocked(resume bool) {
	journal.source.RetireInputPresentationGenerationWithBaseline(journal.source.Generation(), journal.latest, time.Now())
	journal.claim = inputpresentation.Claim{}
	journal.admitted = false
	// Reconfiguration / IN halt-clear can leave the persona Active. Preserve
	// successor transitions immediately, not only after its first host poll.
	journal.active = resume
}

func (journal *retainedInputJournal) permit() {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if !journal.active {
		// A control boundary between initial delivery and Permit can suspend
		// journaling. Resume at Permit, but never erase an already-live initial
		// claim's successor transitions merely because permission completed.
		journal.boundaryLocked(true)
	}
}

func isRetainedInputAction(action ControllerPersonaAction) bool {
	return action == ControllerPersonaSendInitialInput || action == ControllerPersonaSendInput
}

// The caller invokes this only after the engine accepted AND delivered the
// exact EP0 operation, so malformed/failed/stalled requests never retire input.
func retainedInputControlBoundary(setup [usbSetupPacketSize]byte) bool {
	return (setup[0] == usbRequestTypeDeviceOut && setup[1] == usbRequestSetConfiguration) ||
		setup == [usbSetupPacketSize]byte{usbRequestTypeEndpointOut, usbRequestClearFeature, 0, 0, 0x81, 0, 0, 0}
}

package xboxone

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/usb"
)

func newTestPersonaInterruptOutAdapter(
	t *testing.T,
	participant ControllerDownstreamPacketAtomicParticipant,
) (*DormantControllerPersonaInterruptOutAdapter,
	*ControllerPersonaDownstreamPacketBatchOwner) {
	t.Helper()
	engine := newTestControllerPersonaEngine(t, []byte{0xaa}, 0)
	configurePersonaUSB(t, engine, 0)
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(
		engine, participant)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewDormantControllerPersonaInterruptOutAdapter(
		owner, time.Now().Add(-time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	return adapter, owner
}

func testPersonaInterruptOutWire(t *testing.T) []byte {
	t.Helper()
	return testDirectMotorWire(t, 0x31, RumbleBodyV1{
		Enabled:       MotorLeftVibration | MotorRightImpulse,
		LeftVibration: 23, RightImpulse: 47, Duration: 8,
	})
}

func claimAdmitCompletePersonaInterruptOut(
	t *testing.T,
	adapter *DormantControllerPersonaInterruptOutAdapter,
	wire []byte,
	outcome usb.InterruptOutTransactionOutcome,
) (usb.InterruptOutTransactionClaim, ControllerPersonaInterruptOutWorkTicket) {
	t.Helper()
	claim, err := adapter.ClaimInterruptOutTransaction(
		usb.InterruptOutTransactionRequest{Endpoint: 1, Data: wire})
	if err != nil {
		t.Fatal(err)
	}
	if !claim.Valid() || !claim.Handled() {
		t.Fatalf("claim = %+v", claim)
	}
	if err := adapter.AdmitInterruptOutTransaction(claim); err != nil {
		t.Fatal(err)
	}
	if err := adapter.CompleteInterruptOutTransaction(claim, outcome); err != nil {
		t.Fatal(err)
	}
	ticket, present := adapter.PendingWork()
	if !present || !ticket.Valid() {
		t.Fatalf("pending = (%+v, %t)", ticket, present)
	}
	return claim, ticket
}

func runPersonaInterruptOutWork(
	t *testing.T,
	adapter *DormantControllerPersonaInterruptOutAdapter,
	ticket ControllerPersonaInterruptOutWorkTicket,
) error {
	t.Helper()
	now := time.Now()
	return adapter.RunPending(
		ticket, now.Add(time.Second), now.Add(2*time.Second))
}

func TestPersonaInterruptOutDeliveredPublishesBeforeWorkerEffect(t *testing.T) {
	participant := &recordingDownstreamPacketParticipant{}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
	wire := testPersonaInterruptOutWire(t)
	wantWire := append([]byte(nil), wire...)

	unhandled, err := adapter.ClaimInterruptOutTransaction(
		usb.InterruptOutTransactionRequest{Endpoint: 2, Data: wire})
	if err != nil || unhandled != (usb.InterruptOutTransactionClaim{}) {
		t.Fatalf("endpoint 2 = (%+v, %v)", unhandled, err)
	}
	claim, err := adapter.ClaimInterruptOutTransaction(
		usb.InterruptOutTransactionRequest{Endpoint: 1, Data: wire})
	if err != nil || claim.Result != usb.InterruptOutTransactionAccepted {
		t.Fatalf("Claim = (%+v, %v)", claim, err)
	}
	clear(wire)
	if preflight, execute, cancel := participant.counts(); preflight != 1 || execute != 0 || cancel != 0 {
		t.Fatalf("after Claim calls = (%d, %d, %d)", preflight, execute, cancel)
	}
	if err := adapter.AdmitInterruptOutTransaction(claim); err != nil {
		t.Fatal(err)
	}
	if preflight, execute, cancel := participant.counts(); preflight != 2 || execute != 0 || cancel != 0 {
		t.Fatalf("after Admit calls = (%d, %d, %d)", preflight, execute, cancel)
	}
	if err := adapter.CompleteInterruptOutTransaction(
		claim, usb.InterruptOutTransactionDelivered); err != nil {
		t.Fatal(err)
	}
	if preflight, execute, cancel := participant.counts(); preflight != 2 || execute != 0 || cancel != 0 {
		t.Fatalf("Complete ran work = (%d, %d, %d)", preflight, execute, cancel)
	}
	select {
	case <-adapter.WorkReady():
	default:
		t.Fatal("Complete did not latch work readiness")
	}
	ticketOne, present := adapter.PendingWork()
	if !present {
		t.Fatal("missing pending work")
	}
	ticketTwo, present := adapter.PendingWork()
	if !present || ticketOne != ticketTwo || ticketOne.BatchEpoch() == 0 {
		t.Fatalf("retained tickets = (%+v, %+v, %t)", ticketOne, ticketTwo, present)
	}
	if err := runPersonaInterruptOutWork(t, adapter, ticketOne); err != nil {
		t.Fatal(err)
	}
	if _, present := adapter.PendingWork(); present {
		t.Fatal("completed ticket remained pending")
	}
	adapterSnapshot, _ := adapter.Snapshot()
	ownerSnapshot, _ := owner.Snapshot()
	if !adapterSnapshot.Idle || !ownerSnapshot.Idle {
		t.Fatalf("snapshots = adapter %+v owner %+v", adapterSnapshot, ownerSnapshot)
	}
	participant.mu.Lock()
	applied := participant.applied
	participant.mu.Unlock()
	packet, decodeErr := DecodeControllerDownstreamPacket(wantWire)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	want, _ := packet.Message(0)
	got, ok := applied.Action(0)
	if !ok || got.DirectMotor != want.DirectMotor {
		t.Fatalf("applied = (%+v, %t), want %+v", got, ok, want.DirectMotor)
	}
}

func TestPersonaInterruptOutMalformedPacketStallsWithoutBatchClaim(t *testing.T) {
	participant := &recordingDownstreamPacketParticipant{}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)

	malformed := []byte{MessageNumberDirectMotor}
	claim, err := adapter.ClaimInterruptOutTransaction(
		usb.InterruptOutTransactionRequest{Endpoint: 1, Data: malformed})
	if err != nil || claim.Result != usb.InterruptOutTransactionStall {
		t.Fatalf("malformed claim = (%+v, %v)", claim, err)
	}
	if err := adapter.AdmitInterruptOutTransaction(claim); err != nil {
		t.Fatal(err)
	}
	if err := adapter.CompleteInterruptOutTransaction(
		claim, usb.InterruptOutTransactionDelivered); err != nil {
		t.Fatal(err)
	}
	ticket, present := adapter.PendingWork()
	if !present || ticket.BatchEpoch() != 0 {
		t.Fatalf("stall work = (%+v, %t)", ticket, present)
	}
	if err := runPersonaInterruptOutWork(t, adapter, ticket); err != nil {
		t.Fatal(err)
	}
	if preflight, execute, cancel := participant.counts(); preflight != 0 || execute != 0 || cancel != 0 {
		t.Fatalf("stall called participant = (%d, %d, %d)",
			preflight, execute, cancel)
	}
	ownerSnapshot, _ := owner.Snapshot()
	if !ownerSnapshot.Idle {
		t.Fatalf("stall touched batch owner: %+v", ownerSnapshot)
	}

	badCapacity := make([]byte, 1, 2)
	returned, err := adapter.ClaimInterruptOutTransaction(
		usb.InterruptOutTransactionRequest{Endpoint: 1, Data: badCapacity})
	if !errors.Is(err, ErrControllerPersonaInterruptOutClaim) ||
		returned != (usb.InterruptOutTransactionClaim{}) {
		t.Fatalf("capacity mismatch = (%+v, %v)", returned, err)
	}
}

func TestPersonaInterruptOutDeliveryFailureRetiresWithoutEffectAndFencesReplay(
	t *testing.T,
) {
	for _, outcome := range []usb.InterruptOutTransactionOutcome{
		usb.InterruptOutTransactionDeliveryFailed,
		usb.InterruptOutTransactionCancelled,
	} {
		t.Run(interruptOutOutcomeName(outcome), func(t *testing.T) {
			participant := &recordingDownstreamPacketParticipant{}
			adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
			wire := testPersonaInterruptOutWire(t)
			claim, err := adapter.ClaimInterruptOutTransaction(
				usb.InterruptOutTransactionRequest{Endpoint: 1, Data: wire})
			if err != nil {
				t.Fatal(err)
			}
			if outcome == usb.InterruptOutTransactionCancelled {
				participant.setPreflightError(errors.New("reject final preflight"))
				if err := adapter.AdmitInterruptOutTransaction(claim); err == nil {
					t.Fatal("Admit unexpectedly succeeded")
				}
			} else if err := adapter.AdmitInterruptOutTransaction(claim); err != nil {
				t.Fatal(err)
			}
			if err := adapter.CompleteInterruptOutTransaction(claim, outcome); err != nil {
				t.Fatal(err)
			}
			ticket, present := adapter.PendingWork()
			if !present {
				t.Fatal("missing retirement work")
			}
			err = runPersonaInterruptOutWork(t, adapter, ticket)
			if !errors.Is(err, ErrControllerPersonaInterruptOutRetryUnsupported) ||
				!errors.Is(err, ErrControllerPersonaInterruptOutQuarantined) {
				t.Fatalf("RunPending = %v", err)
			}
			_, execute, _ := participant.counts()
			if execute != 0 {
				t.Fatalf("failed USB delivery executed %d effects", execute)
			}
			ownerSnapshot, _ := owner.Snapshot()
			adapterSnapshot, _ := adapter.Snapshot()
			if !ownerSnapshot.RetryPending || !adapterSnapshot.Quarantined ||
				!adapterSnapshot.RetryBlocked {
				t.Fatalf("snapshots = owner %+v adapter %+v",
					ownerSnapshot, adapterSnapshot)
			}
			next, nextErr := adapter.ClaimInterruptOutTransaction(
				usb.InterruptOutTransactionRequest{Endpoint: 1, Data: wire})
			if next != (usb.InterruptOutTransactionClaim{}) || nextErr == nil {
				t.Fatalf("unproven replay = (%+v, %v)", next, nextErr)
			}
		})
	}
}

func TestPersonaInterruptOutClaimPreflightFailureReturnsZeroAndRetainsFence(
	t *testing.T,
) {
	participant := &recordingDownstreamPacketParticipant{
		preflightErr: errors.New("preflight unavailable"),
	}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
	claim, err := adapter.ClaimInterruptOutTransaction(
		usb.InterruptOutTransactionRequest{
			Endpoint: 1, Data: testPersonaInterruptOutWire(t),
		})
	if claim != (usb.InterruptOutTransactionClaim{}) ||
		!errors.Is(err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("Claim = (%+v, %v)", claim, err)
	}
	ownerSnapshot, _ := owner.Snapshot()
	adapterSnapshot, _ := adapter.Snapshot()
	if !ownerSnapshot.RetryPending || !adapterSnapshot.Quarantined ||
		adapterSnapshot.USBToken != 0 {
		t.Fatalf("snapshots = owner %+v adapter %+v", ownerSnapshot, adapterSnapshot)
	}
}

func TestPersonaInterruptOutTimelyParticipantFailureIsNotReplayed(t *testing.T) {
	participant := &recordingDownstreamPacketParticipant{
		executeErr: errors.New("proven no effect"),
	}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
	_, ticket := claimAdmitCompletePersonaInterruptOut(t, adapter,
		testPersonaInterruptOutWire(t), usb.InterruptOutTransactionDelivered)
	err := runPersonaInterruptOutWork(t, adapter, ticket)
	if !errors.Is(err, ErrControllerPersonaInterruptOutRetryUnsupported) {
		t.Fatalf("RunPending = %v", err)
	}
	ownerSnapshot, _ := owner.Snapshot()
	if !ownerSnapshot.RetryPending || ownerSnapshot.Quarantined {
		t.Fatalf("owner = %+v", ownerSnapshot)
	}
}

type lateInterruptOutParticipant struct {
	mu           sync.Mutex
	executeCalls int
	cancelCalls  int
}

type blockingPreflightInterruptOutParticipant struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (participant *blockingPreflightInterruptOutParticipant) PreflightControllerDownstreamPacket(
	ControllerDownstreamPacketExecution,
) error {
	participant.once.Do(func() { close(participant.started) })
	<-participant.release
	return nil
}

func (*blockingPreflightInterruptOutParticipant) ExecuteControllerDownstreamPacket(
	ControllerDownstreamPacketExecution,
	time.Time,
) error {
	return nil
}

func (*blockingPreflightInterruptOutParticipant) CancelControllerDownstreamPacketAndDrain(
	ControllerDownstreamPacketExecution,
	time.Time,
) error {
	return nil
}

func (*lateInterruptOutParticipant) PreflightControllerDownstreamPacket(
	ControllerDownstreamPacketExecution,
) error {
	return nil
}

func (participant *lateInterruptOutParticipant) ExecuteControllerDownstreamPacket(
	_ ControllerDownstreamPacketExecution,
	deadline time.Time,
) error {
	participant.mu.Lock()
	participant.executeCalls++
	participant.mu.Unlock()
	time.Sleep(time.Until(deadline) + 2*time.Millisecond)
	return errors.New("late no-effect report")
}

func (participant *lateInterruptOutParticipant) CancelControllerDownstreamPacketAndDrain(
	ControllerDownstreamPacketExecution,
	time.Time,
) error {
	participant.mu.Lock()
	participant.cancelCalls++
	participant.mu.Unlock()
	return nil
}

func TestPersonaInterruptOutLateExecutionQuarantinesAfterExactDrain(t *testing.T) {
	participant := &lateInterruptOutParticipant{}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
	_, ticket := claimAdmitCompletePersonaInterruptOut(t, adapter,
		testPersonaInterruptOutWire(t), usb.InterruptOutTransactionDelivered)
	now := time.Now()
	err := adapter.RunPending(ticket,
		now.Add(2*time.Millisecond), now.Add(100*time.Millisecond))
	if !errors.Is(err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("RunPending = %v", err)
	}
	participant.mu.Lock()
	executeCalls, cancelCalls := participant.executeCalls, participant.cancelCalls
	participant.mu.Unlock()
	ownerSnapshot, _ := owner.Snapshot()
	if executeCalls != 1 || cancelCalls != 1 {
		t.Fatalf("calls = execute %d cancel %d; RunPending = %v; owner = %+v",
			executeCalls, cancelCalls, err, ownerSnapshot)
	}
	if !ownerSnapshot.Quarantined {
		t.Fatalf("ambiguous owner = %+v", ownerSnapshot)
	}
}

func TestPersonaInterruptOutDuplicateCompleteFencesAndRetainsExactWork(t *testing.T) {
	participant := &recordingDownstreamPacketParticipant{}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
	claim, _ := claimAdmitCompletePersonaInterruptOut(t, adapter,
		testPersonaInterruptOutWire(t), usb.InterruptOutTransactionDelivered)
	if err := adapter.CompleteInterruptOutTransaction(
		claim, usb.InterruptOutTransactionDelivered); !errors.Is(
		err, ErrControllerPersonaInterruptOutClaim) {
		t.Fatalf("duplicate Complete = %v", err)
	}
	if retained, present := adapter.PendingWork(); present || retained.Valid() {
		t.Fatalf("terminal work escaped = (%+v, %t)", retained, present)
	}
	_, execute, _ := participant.counts()
	ownerSnapshot, _ := owner.Snapshot()
	if execute != 0 || !ownerSnapshot.Admitted ||
		ownerSnapshot.AwaitingResolution {
		t.Fatalf("execute calls = %d", execute)
	}
}

func TestPersonaInterruptOutDuplicateDuringRunningRetainsAmbiguousOwner(
	t *testing.T,
) {
	participant := &recordingDownstreamPacketParticipant{
		executeStarted: make(chan struct{}),
		executeRelease: make(chan struct{}),
	}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
	claim, ticket := claimAdmitCompletePersonaInterruptOut(t, adapter,
		testPersonaInterruptOutWire(t), usb.InterruptOutTransactionDelivered)

	runResult := make(chan error, 1)
	now := time.Now()
	go func() {
		runResult <- adapter.RunPending(
			ticket, now.Add(time.Second), now.Add(2*time.Second))
	}()
	select {
	case <-participant.executeStarted:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter participant Execute")
	}
	if err := adapter.CompleteInterruptOutTransaction(
		claim, usb.InterruptOutTransactionDelivered); !errors.Is(
		err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("duplicate Complete = %v", err)
	}
	participant.releaseOnce.Do(func() { close(participant.executeRelease) })
	if err := <-runResult; !errors.Is(
		err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("RunPending = %v", err)
	}
	ownerSnapshot, _ := owner.Snapshot()
	if !ownerSnapshot.Admitted || !ownerSnapshot.AwaitingResolution ||
		ownerSnapshot.Idle || ownerSnapshot.RetryPending {
		t.Fatalf("running ambiguity was falsely resolved: %+v", ownerSnapshot)
	}
	if next, err := adapter.ClaimInterruptOutTransaction(
		usb.InterruptOutTransactionRequest{
			Endpoint: 1, Data: testPersonaInterruptOutWire(t),
		}); next != (usb.InterruptOutTransactionClaim{}) || !errors.Is(
		err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("successor Claim = (%+v, %v)", next, err)
	}
}

func TestPersonaInterruptOutRetainedCompletionTokenFencesDelayedCopy(
	t *testing.T,
) {
	participant := &recordingDownstreamPacketParticipant{}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
	claim, ticket := claimAdmitCompletePersonaInterruptOut(t, adapter,
		testPersonaInterruptOutWire(t), usb.InterruptOutTransactionDelivered)
	if err := runPersonaInterruptOutWork(t, adapter, ticket); err != nil {
		t.Fatal(err)
	}
	ownerSnapshot, _ := owner.Snapshot()
	if !ownerSnapshot.Idle {
		t.Fatalf("first generation did not retire: %+v", ownerSnapshot)
	}
	if err := adapter.CompleteInterruptOutTransaction(
		claim, usb.InterruptOutTransactionDelivered); !errors.Is(
		err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("delayed copied Complete = %v", err)
	}
	beforePreflight, beforeExecute, _ := participant.counts()
	next, err := adapter.ClaimInterruptOutTransaction(
		usb.InterruptOutTransactionRequest{
			Endpoint: 1, Data: testPersonaInterruptOutWire(t),
		})
	afterPreflight, afterExecute, _ := participant.counts()
	if next != (usb.InterruptOutTransactionClaim{}) || !errors.Is(err,
		ErrControllerPersonaInterruptOutQuarantined) ||
		afterPreflight != beforePreflight || afterExecute != beforeExecute {
		t.Fatalf("successor escaped: claim=(%+v,%v) calls=(%d,%d)->(%d,%d)",
			next, err, beforePreflight, beforeExecute,
			afterPreflight, afterExecute)
	}
}

func TestPersonaInterruptOutCompletingViolationCannotPublishPending(t *testing.T) {
	t.Run("invalid worker deadline", func(t *testing.T) {
		participant := &recordingDownstreamPacketParticipant{}
		adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
		claim, err := adapter.ClaimInterruptOutTransaction(
			usb.InterruptOutTransactionRequest{
				Endpoint: 1, Data: testPersonaInterruptOutWire(t),
			})
		if err != nil {
			t.Fatal(err)
		}
		if err := adapter.AdmitInterruptOutTransaction(claim); err != nil {
			t.Fatal(err)
		}
		if !adapter.compareState(controllerPersonaInterruptOutAdmitted,
			controllerPersonaInterruptOutCompleting) {
			t.Fatal("could not establish Complete publication boundary")
		}
		if err := adapter.RunPending(
			ControllerPersonaInterruptOutWorkTicket{}, time.Time{}, time.Time{}); !errors.Is(
			err, ErrControllerPersonaInterruptOutDeadline) {
			t.Fatalf("invalid RunPending = %v", err)
		}
		if adapter.publishCompletion(
			usb.InterruptOutTransactionDelivered) {
			t.Fatal("Complete overwrote terminal violation with Pending")
		}
		ownerSnapshot, _ := owner.Snapshot()
		if adapter.loadState() != controllerPersonaInterruptOutQuarantined ||
			!ownerSnapshot.Admitted {
			t.Fatalf("state=%d owner=%+v", adapter.loadState(), ownerSnapshot)
		}
	})

	t.Run("admit mismatch", func(t *testing.T) {
		participant := &recordingDownstreamPacketParticipant{}
		adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
		claim, err := adapter.ClaimInterruptOutTransaction(
			usb.InterruptOutTransactionRequest{
				Endpoint: 1, Data: testPersonaInterruptOutWire(t),
			})
		if err != nil {
			t.Fatal(err)
		}
		if !adapter.compareState(controllerPersonaInterruptOutClaimed,
			controllerPersonaInterruptOutCompleting) {
			t.Fatal("could not establish Complete publication boundary")
		}
		if err := adapter.AdmitInterruptOutTransaction(claim); !errors.Is(
			err, ErrControllerPersonaInterruptOutClaim) {
			t.Fatalf("Admit mismatch = %v", err)
		}
		if adapter.publishCompletion(
			usb.InterruptOutTransactionCancelled) {
			t.Fatal("Complete overwrote terminal violation with Pending")
		}
		ownerSnapshot, _ := owner.Snapshot()
		if adapter.loadState() != controllerPersonaInterruptOutQuarantined ||
			!ownerSnapshot.Claimed {
			t.Fatalf("state=%d owner=%+v", adapter.loadState(), ownerSnapshot)
		}
	})
}

func TestPersonaInterruptOutResolvingCASIsIrrevocable(t *testing.T) {
	participant := &recordingDownstreamPacketParticipant{}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
	claim, ticket := claimAdmitCompletePersonaInterruptOut(t, adapter,
		testPersonaInterruptOutWire(t),
		usb.InterruptOutTransactionDeliveryFailed)
	adapter.mu.Lock()
	slot := adapter.slot
	if !adapter.compareState(controllerPersonaInterruptOutPending,
		controllerPersonaInterruptOutRunning) {
		adapter.mu.Unlock()
		t.Fatal("could not acquire exact pending ticket")
	}
	adapter.mu.Unlock()
	if !adapter.beginResolution() {
		t.Fatal("worker did not win Running -> Resolving")
	}
	if err := adapter.RunPending(ticket, time.Time{}, time.Time{}); !errors.Is(
		err, ErrControllerPersonaInterruptOutDeadline) {
		t.Fatalf("copied worker during Resolve = %v", err)
	}
	if adapter.loadState() != controllerPersonaInterruptOutResolvingTerminal ||
		!adapter.terminalFence.Load() {
		t.Fatalf("copied worker revoked resolution: state=%d fence=%t",
			adapter.loadState(), adapter.terminalFence.Load())
	}
	if err := adapter.CompleteInterruptOutTransaction(
		claim, usb.InterruptOutTransactionDeliveryFailed); !errors.Is(
		err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("competing Complete = %v", err)
	}
	if adapter.loadState() != controllerPersonaInterruptOutResolvingTerminal ||
		!adapter.terminalFence.Load() {
		t.Fatalf("resolution authority was revoked: state=%d fence=%t",
			adapter.loadState(), adapter.terminalFence.Load())
	}
	nowMS, err := adapter.protocolMilliseconds(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(slot.batchClaim,
		ControllerDownstreamPacketDeferred, nowMS); err != nil {
		t.Fatal(err)
	}
	if err := adapter.validateResolvedOwner(
		ControllerDownstreamPacketDeferred); err != nil {
		t.Fatal(err)
	}
	if adapter.prepareFinish() {
		t.Fatal("terminal resolution prepared a clean finish")
	}
	if err := adapter.finishWork(ticket, true, nil); !errors.Is(
		err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("terminal finish = %v", err)
	}
	ownerSnapshot, _ := owner.Snapshot()
	if !ownerSnapshot.RetryPending || ownerSnapshot.Idle {
		t.Fatalf("exact resolution was not retained: %+v", ownerSnapshot)
	}
}

func TestPersonaInterruptOutFinishingCASCannotReopenIdle(t *testing.T) {
	participant := &recordingDownstreamPacketParticipant{}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
	claim, ticket := claimAdmitCompletePersonaInterruptOut(t, adapter,
		testPersonaInterruptOutWire(t),
		usb.InterruptOutTransactionDeliveryFailed)
	adapter.mu.Lock()
	slot := adapter.slot
	if !adapter.compareState(controllerPersonaInterruptOutPending,
		controllerPersonaInterruptOutRunning) {
		adapter.mu.Unlock()
		t.Fatal("could not acquire exact pending ticket")
	}
	adapter.mu.Unlock()
	if !adapter.beginResolution() {
		t.Fatal("could not begin exact resolution")
	}
	nowMS, err := adapter.protocolMilliseconds(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(slot.batchClaim,
		ControllerDownstreamPacketDeferred, nowMS); err != nil {
		t.Fatal(err)
	}
	if err := adapter.validateResolvedOwner(
		ControllerDownstreamPacketDeferred); err != nil {
		t.Fatal(err)
	}
	if !adapter.prepareFinish() ||
		adapter.loadState() != controllerPersonaInterruptOutFinishing {
		t.Fatalf("state=%d did not prepare finish", adapter.loadState())
	}
	if err := adapter.CompleteInterruptOutTransaction(
		claim, usb.InterruptOutTransactionDeliveryFailed); !errors.Is(
		err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("competing Complete = %v", err)
	}
	if adapter.loadState() != controllerPersonaInterruptOutQuarantined {
		t.Fatalf("Finishing reopened as state %d", adapter.loadState())
	}
	if err := adapter.finishWork(ticket, true, nil); !errors.Is(
		err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("terminal finish = %v", err)
	}
	ownerSnapshot, _ := owner.Snapshot()
	if !ownerSnapshot.RetryPending || ownerSnapshot.Idle {
		t.Fatalf("owner terminal shape = %+v", ownerSnapshot)
	}
}

func TestPersonaInterruptOutAdmissionResultShapes(t *testing.T) {
	synthetic := errors.New("synthetic Admit contradiction")
	for _, test := range []struct {
		name       string
		validLease bool
		admitErr   error
		wantState  controllerPersonaInterruptOutState
		wantQ      bool
	}{
		{name: "nil invalid", wantState: controllerPersonaInterruptOutQuarantined, wantQ: true},
		{name: "nil valid", validLease: true, wantState: controllerPersonaInterruptOutAdmitted},
		{name: "error invalid", admitErr: synthetic, wantState: controllerPersonaInterruptOutAdmissionRejected},
		{name: "error valid", validLease: true, admitErr: synthetic, wantState: controllerPersonaInterruptOutQuarantined, wantQ: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			participant := &recordingDownstreamPacketParticipant{}
			adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
			claim, err := adapter.ClaimInterruptOutTransaction(
				usb.InterruptOutTransactionRequest{
					Endpoint: 1, Data: testPersonaInterruptOutWire(t),
				})
			if err != nil {
				t.Fatal(err)
			}
			batchClaim := adapter.slot.batchClaim
			if !adapter.compareState(controllerPersonaInterruptOutClaimed,
				controllerPersonaInterruptOutAdmitting) {
				t.Fatal("could not stage admission result")
			}
			var lease ControllerPersonaDownstreamPacketBatchLease
			if test.validLease {
				nowMS, clockErr := adapter.protocolMilliseconds(time.Now())
				if clockErr != nil {
					t.Fatal(clockErr)
				}
				lease, err = owner.Admit(batchClaim, nowMS)
				if err != nil {
					t.Fatal(err)
				}
			}
			adapter.mu.Lock()
			gotErr := adapter.finishAdmissionLocked(
				batchClaim, lease, test.admitErr)
			retained := adapter.slot.batchLease
			adapter.mu.Unlock()
			if adapter.loadState() != test.wantState {
				t.Fatalf("state=%d, want %d", adapter.loadState(), test.wantState)
			}
			if test.wantQ != errors.Is(gotErr,
				ErrControllerPersonaInterruptOutQuarantined) {
				t.Fatalf("finishAdmission = %v", gotErr)
			}
			if test.admitErr != nil && !errors.Is(gotErr, test.admitErr) {
				t.Fatalf("finishAdmission lost error: %v", gotErr)
			}
			if test.validLease && retained != lease {
				t.Fatalf("admitted lease was not retained: %+v != %+v",
					retained, lease)
			}
			if !test.validLease && retained.Valid() {
				t.Fatalf("invented retained lease: %+v", retained)
			}
			_ = claim
		})
	}
}

func TestPersonaInterruptOutAdmittedLeaseRequiresExactOwnerAndToken(t *testing.T) {
	for _, mutation := range []string{"foreign owner", "stale token"} {
		t.Run(mutation, func(t *testing.T) {
			participant := &recordingDownstreamPacketParticipant{}
			adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
			claim, err := adapter.ClaimInterruptOutTransaction(
				usb.InterruptOutTransactionRequest{
					Endpoint: 1, Data: testPersonaInterruptOutWire(t),
				})
			if err != nil {
				t.Fatal(err)
			}
			batchClaim := adapter.slot.batchClaim
			if !adapter.compareState(controllerPersonaInterruptOutClaimed,
				controllerPersonaInterruptOutAdmitting) {
				t.Fatal("could not stage forged lease")
			}
			nowMS, err := adapter.protocolMilliseconds(time.Now())
			if err != nil {
				t.Fatal(err)
			}
			lease, err := owner.Admit(batchClaim, nowMS)
			if err != nil {
				t.Fatal(err)
			}
			forged := lease
			switch mutation {
			case "foreign owner":
				otherAdapter, _ := newTestPersonaInterruptOutAdapter(
					t, &recordingDownstreamPacketParticipant{})
				forged.owner = otherAdapter.batchOwner
			case "stale token":
				forged.token++
			}
			if !forged.Valid() || forged.PacketEpoch() != lease.PacketEpoch() ||
				forged.Len() != lease.Len() {
				t.Fatalf("mutation did not preserve superficial shape: %+v", forged)
			}
			adapter.mu.Lock()
			gotErr := adapter.finishAdmissionLocked(batchClaim, forged, nil)
			retained := adapter.slot.batchLease
			adapter.mu.Unlock()
			if !errors.Is(gotErr,
				ErrControllerPersonaInterruptOutQuarantined) ||
				adapter.loadState() != controllerPersonaInterruptOutQuarantined ||
				retained != forged {
				t.Fatalf("forged lease escaped: err=%v state=%d retained=%+v",
					gotErr, adapter.loadState(), retained)
			}
			_ = claim
		})
	}
}

func TestPersonaInterruptOutClaimResultShapesAndContradictionRetention(
	t *testing.T,
) {
	synthetic := errors.New("synthetic Claim contradiction")
	for _, test := range []struct {
		name  string
		valid bool
		err   error
		want  controllerPersonaInterruptOutClaimShape
	}{
		{name: "nil invalid", want: controllerPersonaInterruptOutClaimShapeInvalid},
		{name: "nil valid", valid: true, want: controllerPersonaInterruptOutClaimShapeAccepted},
		{name: "error invalid", err: synthetic, want: controllerPersonaInterruptOutClaimShapeRejected},
		{name: "error valid", valid: true, err: synthetic, want: controllerPersonaInterruptOutClaimShapeContradictory},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyControllerPersonaInterruptOutClaim(
				test.valid, test.err); got != test.want {
				t.Fatalf("shape=%d, want %d", got, test.want)
			}
		})
	}

	participant := &recordingDownstreamPacketParticipant{}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
	packet, err := DecodeControllerDownstreamPacket(
		testPersonaInterruptOutWire(t))
	if err != nil {
		t.Fatal(err)
	}
	if !adapter.compareState(controllerPersonaInterruptOutIdle,
		controllerPersonaInterruptOutClaiming) {
		t.Fatal("could not stage Claim contradiction")
	}
	nowMS, err := adapter.protocolMilliseconds(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	batchClaim, err := owner.Claim(packet, nowMS)
	if err != nil || !batchClaim.Valid() {
		t.Fatalf("owner Claim = (%+v, %v)", batchClaim, err)
	}
	adapter.quarantineClaimFailure(batchClaim, synthetic)
	ownerSnapshot, _ := owner.Snapshot()
	if adapter.loadState() != controllerPersonaInterruptOutQuarantined ||
		adapter.slot.batchClaim != batchClaim || !ownerSnapshot.Claimed {
		t.Fatalf("contradiction lost authority: state=%d slot=%+v owner=%+v",
			adapter.loadState(), adapter.slot.batchClaim, ownerSnapshot)
	}
}

func TestPersonaInterruptOutResolvedOwnerTerminalShapes(t *testing.T) {
	for _, test := range []struct {
		name     string
		outcome  ControllerDownstreamPacketOutcome
		snapshot ControllerPersonaDownstreamPacketBatchSnapshot
		want     bool
	}{
		{name: "delivered idle", outcome: ControllerDownstreamPacketDelivered,
			snapshot: ControllerPersonaDownstreamPacketBatchSnapshot{Idle: true}, want: true},
		{name: "delivered retry contradiction", outcome: ControllerDownstreamPacketDelivered,
			snapshot: ControllerPersonaDownstreamPacketBatchSnapshot{RetryPending: true}},
		{name: "deferred retry", outcome: ControllerDownstreamPacketDeferred,
			snapshot: ControllerPersonaDownstreamPacketBatchSnapshot{RetryPending: true}, want: true},
		{name: "failed idle contradiction", outcome: ControllerDownstreamPacketDeliveryFailed,
			snapshot: ControllerPersonaDownstreamPacketBatchSnapshot{Idle: true}},
		{name: "cancelled active contradiction", outcome: ControllerDownstreamPacketExecutionCancelled,
			snapshot: ControllerPersonaDownstreamPacketBatchSnapshot{
				RetryPending: true, AwaitingResolution: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validControllerPersonaInterruptOutResolvedOwner(
				test.outcome, test.snapshot); got != test.want {
				t.Fatalf("valid=%t, want %t for %+v", got, test.want, test.snapshot)
			}
		})
	}
}

func TestPersonaInterruptOutSnapshotReportsRetainedQuarantineError(t *testing.T) {
	adapter, _ := newTestPersonaInterruptOutAdapter(
		t, &recordingDownstreamPacketParticipant{})
	adapter.mu.Lock()
	adapter.quarantine = errors.New("retained diagnostic contradiction")
	adapter.mu.Unlock()
	snapshot, ok := adapter.Snapshot()
	if !ok || !snapshot.Quarantined || !snapshot.Idle ||
		adapter.terminalFence.Load() {
		t.Fatalf("snapshot=%+v ok=%t fence=%t",
			snapshot, ok, adapter.terminalFence.Load())
	}
}

func TestPersonaInterruptOutConcurrentCopiedWorkerTicketExecutesOnce(t *testing.T) {
	participant := &recordingDownstreamPacketParticipant{
		executeStarted: make(chan struct{}),
		executeRelease: make(chan struct{}),
	}
	adapter, _ := newTestPersonaInterruptOutAdapter(t, participant)
	_, ticket := claimAdmitCompletePersonaInterruptOut(t, adapter,
		testPersonaInterruptOutWire(t), usb.InterruptOutTransactionDelivered)

	firstErr := make(chan error, 1)
	now := time.Now()
	go func() {
		firstErr <- adapter.RunPending(
			ticket, now.Add(time.Second), now.Add(2*time.Second))
	}()
	select {
	case <-participant.executeStarted:
	case <-time.After(time.Second):
		t.Fatal("first worker did not start")
	}
	secondErr := adapter.RunPending(
		ticket, now.Add(time.Second), now.Add(2*time.Second))
	if !errors.Is(secondErr, ErrControllerPersonaInterruptOutBusy) {
		t.Fatalf("second worker = %v", secondErr)
	}
	participant.releaseOnce.Do(func() { close(participant.executeRelease) })
	if err := <-firstErr; !errors.Is(
		err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("first worker = %v", err)
	}
	_, execute, _ := participant.counts()
	if execute != 1 {
		t.Fatalf("execute calls = %d", execute)
	}
}

func TestPersonaInterruptOutFatalCompleteDuringClaimRetainsAcquiredBatch(
	t *testing.T,
) {
	participant := &blockingPreflightInterruptOutParticipant{
		started: make(chan struct{}), release: make(chan struct{}),
	}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
	wire := testPersonaInterruptOutWire(t)
	result := make(chan struct {
		claim usb.InterruptOutTransactionClaim
		err   error
	}, 1)
	go func() {
		claim, err := adapter.ClaimInterruptOutTransaction(
			usb.InterruptOutTransactionRequest{
				Endpoint: 1, Data: wire,
			})
		result <- struct {
			claim usb.InterruptOutTransactionClaim
			err   error
		}{claim: claim, err: err}
	}()
	select {
	case <-participant.started:
	case <-time.After(time.Second):
		t.Fatal("Claim did not reach preflight")
	}
	forged := usb.InterruptOutTransactionClaim{
		Token: 1, Generation: adapter.generation,
		Result: usb.InterruptOutTransactionAccepted,
	}
	if err := adapter.CompleteInterruptOutTransaction(
		forged, usb.InterruptOutTransactionCancelled); !errors.Is(
		err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("fatal Complete = %v", err)
	}
	close(participant.release)
	claimed := <-result
	if claimed.claim != (usb.InterruptOutTransactionClaim{}) ||
		!errors.Is(claimed.err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("Claim = (%+v, %v)", claimed.claim, claimed.err)
	}
	adapter.mu.Lock()
	retained := adapter.slot.batchClaim.Valid()
	adapter.mu.Unlock()
	ownerSnapshot, _ := owner.Snapshot()
	if !retained || !ownerSnapshot.Claimed {
		t.Fatalf("retained=%t owner=%+v", retained, ownerSnapshot)
	}
}

func waitPersonaInterruptOutOperationOwned(
	t *testing.T,
	adapter *DormantControllerPersonaInterruptOutAdapter,
) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(adapter.operation) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("operation token was not acquired")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPersonaInterruptOutAdmitRechecksFatalFenceUnderSlotMutex(t *testing.T) {
	participant := &recordingDownstreamPacketParticipant{}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
	claim, err := adapter.ClaimInterruptOutTransaction(
		usb.InterruptOutTransactionRequest{
			Endpoint: 1, Data: testPersonaInterruptOutWire(t),
		})
	if err != nil {
		t.Fatal(err)
	}
	adapter.mu.Lock()
	locked := true
	defer func() {
		if locked {
			adapter.mu.Unlock()
		}
	}()
	admitResult := make(chan error, 1)
	go func() { admitResult <- adapter.AdmitInterruptOutTransaction(claim) }()
	waitPersonaInterruptOutOperationOwned(t, adapter)
	forged := claim
	forged.Token++
	completeErr := adapter.CompleteInterruptOutTransaction(
		forged, usb.InterruptOutTransactionCancelled)
	adapter.mu.Unlock()
	locked = false
	if !errors.Is(completeErr, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("forged Complete = %v", completeErr)
	}
	if err := <-admitResult; !errors.Is(
		err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("Admit = %v", err)
	}
	preflight, execute, _ := participant.counts()
	ownerSnapshot, _ := owner.Snapshot()
	if preflight != 1 || execute != 0 || !ownerSnapshot.Claimed {
		t.Fatalf("calls=(%d,%d) owner=%+v", preflight, execute, ownerSnapshot)
	}
}

func TestPersonaInterruptOutRunRechecksFatalFenceUnderSlotMutex(t *testing.T) {
	participant := &recordingDownstreamPacketParticipant{}
	adapter, owner := newTestPersonaInterruptOutAdapter(t, participant)
	_, ticket := claimAdmitCompletePersonaInterruptOut(t, adapter,
		testPersonaInterruptOutWire(t), usb.InterruptOutTransactionDelivered)
	adapter.mu.Lock()
	locked := true
	defer func() {
		if locked {
			adapter.mu.Unlock()
		}
	}()
	runResult := make(chan error, 1)
	now := time.Now()
	go func() {
		runResult <- adapter.RunPending(
			ticket, now.Add(time.Second), now.Add(2*time.Second))
	}()
	waitPersonaInterruptOutOperationOwned(t, adapter)
	adapter.terminalFence.Store(true)
	adapter.mu.Unlock()
	locked = false
	if err := <-runResult; !errors.Is(
		err, ErrControllerPersonaInterruptOutQuarantined) {
		t.Fatalf("RunPending = %v", err)
	}
	_, execute, _ := participant.counts()
	ownerSnapshot, _ := owner.Snapshot()
	if execute != 0 || !ownerSnapshot.Admitted {
		t.Fatalf("execute=%d owner=%+v", execute, ownerSnapshot)
	}
}

func TestPersonaInterruptOutStaleTicketAndDeadlineFailClosed(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*DormantControllerPersonaInterruptOutAdapter,
			ControllerPersonaInterruptOutWorkTicket) error
	}{
		{
			name: "stale ticket",
			run: func(adapter *DormantControllerPersonaInterruptOutAdapter,
				ticket ControllerPersonaInterruptOutWorkTicket) error {
				ticket.token++
				now := time.Now()
				return adapter.RunPending(ticket,
					now.Add(time.Second), now.Add(2*time.Second))
			},
		},
		{
			name: "expired deadline",
			run: func(adapter *DormantControllerPersonaInterruptOutAdapter,
				ticket ControllerPersonaInterruptOutWorkTicket) error {
				now := time.Now()
				return adapter.RunPending(ticket,
					now.Add(-time.Millisecond), now.Add(time.Second))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			participant := &recordingDownstreamPacketParticipant{}
			adapter, _ := newTestPersonaInterruptOutAdapter(t, participant)
			_, ticket := claimAdmitCompletePersonaInterruptOut(t, adapter,
				testPersonaInterruptOutWire(t),
				usb.InterruptOutTransactionDelivered)
			if err := test.run(adapter, ticket); err == nil {
				t.Fatal("RunPending unexpectedly succeeded")
			}
			if retained, present := adapter.PendingWork(); present || retained.Valid() {
				t.Fatalf("terminal work escaped = (%+v, %t)",
					retained, present)
			}
			_, execute, _ := participant.counts()
			if execute != 0 {
				t.Fatalf("execute calls = %d", execute)
			}
		})
	}
}

func TestPersonaInterruptOutZeroCopiedAndNonDeviceShapesFailClosed(t *testing.T) {
	zero := &DormantControllerPersonaInterruptOutAdapter{}
	if _, err := zero.ClaimInterruptOutTransaction(
		usb.InterruptOutTransactionRequest{}); !errors.Is(
		err, ErrControllerPersonaInterruptOutUninitialized) {
		t.Fatalf("zero Claim = %v", err)
	}
	participant := &recordingDownstreamPacketParticipant{}
	adapter, _ := newTestPersonaInterruptOutAdapter(t, participant)
	copiedShape := &DormantControllerPersonaInterruptOutAdapter{
		self: adapter, operation: adapter.operation, workReady: adapter.workReady,
		generation: adapter.generation, batchOwner: adapter.batchOwner,
		origin: adapter.origin,
	}
	if _, err := copiedShape.ClaimInterruptOutTransaction(
		usb.InterruptOutTransactionRequest{}); !errors.Is(
		err, ErrControllerPersonaInterruptOutUninitialized) {
		t.Fatalf("copied Claim = %v", err)
	}
	if _, productionDevice := any(adapter).(usb.Device); productionDevice {
		t.Fatal("dormant packet adapter unexpectedly implements usb.Device")
	}
}

func TestPersonaInterruptOutGenerationSaturatesWithoutReuse(t *testing.T) {
	var counter controllerPersonaInterruptOutSaturatingCounter
	counter.value.Store(^uint64(0) - 1)
	last, available := counter.next()
	if !available || last != ^uint64(0) {
		t.Fatalf("last generation = (%d, %t)", last, available)
	}
	for attempt := 0; attempt < 2; attempt++ {
		generation, available := counter.next()
		if available || generation != 0 || counter.value.Load() != ^uint64(0) {
			t.Fatalf("exhausted attempt %d = (%d, %t), counter=%d",
				attempt, generation, available, counter.value.Load())
		}
	}
}

func TestPersonaInterruptOutCompleteIgnoresDiagnosticMutexAndAllocatesNothing(
	t *testing.T,
) {
	participant := &recordingDownstreamPacketParticipant{}
	adapter, _ := newTestPersonaInterruptOutAdapter(t, participant)
	wire := testPersonaInterruptOutWire(t)
	claim, err := adapter.ClaimInterruptOutTransaction(
		usb.InterruptOutTransactionRequest{Endpoint: 1, Data: wire})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.AdmitInterruptOutTransaction(claim); err != nil {
		t.Fatal(err)
	}

	// Snapshot and PendingWork use mu only for a consistent diagnostic copy.
	// Holding that mutex here proves Complete's publication path does not wait
	// for or fail because of either diagnostic reader.
	adapter.mu.Lock()
	started := time.Now()
	err = adapter.CompleteInterruptOutTransaction(
		claim, usb.InterruptOutTransactionDelivered)
	elapsed := time.Since(started)
	adapter.mu.Unlock()
	if err != nil {
		t.Fatalf("Complete under diagnostic mutex: %v", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("Complete blocked for %s", elapsed)
	}
	ticket, present := adapter.PendingWork()
	if !present {
		t.Fatal("mutex-free Complete did not publish work")
	}
	if err := runPersonaInterruptOutWork(t, adapter, ticket); err != nil {
		t.Fatal(err)
	}

	// Exercise Complete alone. Reset only the adapter's publication ledger;
	// the isolated test object intentionally retains its exact admitted slot.
	claim, err = adapter.ClaimInterruptOutTransaction(
		usb.InterruptOutTransactionRequest{Endpoint: 1, Data: wire})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.AdmitInterruptOutTransaction(claim); err != nil {
		t.Fatal(err)
	}
	completeOnly := func() {
		adapter.lastCompletionToken.Store(0)
		if completeErr := adapter.CompleteInterruptOutTransaction(
			claim, usb.InterruptOutTransactionDelivered); completeErr != nil {
			panic(completeErr)
		}
		select {
		case <-adapter.workReady:
		default:
		}
		adapter.completionOutcome.Store(0)
		adapter.storeState(controllerPersonaInterruptOutAdmitted)
	}
	completeOnly()
	if allocations := testing.AllocsPerRun(1000, completeOnly); allocations != 0 {
		t.Fatalf("Complete allocated %.2f objects/run", allocations)
	}
}

func TestPersonaInterruptOutFatalCompletePathsAllocateNothing(t *testing.T) {
	participant := &recordingDownstreamPacketParticipant{}
	adapter, _ := newTestPersonaInterruptOutAdapter(t, participant)
	claim, _ := claimAdmitCompletePersonaInterruptOut(t, adapter,
		testPersonaInterruptOutWire(t), usb.InterruptOutTransactionDelivered)

	duplicate := func() {
		adapter.terminalFence.Store(false)
		adapter.storeState(controllerPersonaInterruptOutPending)
		adapter.lastCompletionToken.Store(claim.Token)
		if err := adapter.CompleteInterruptOutTransaction(
			claim, usb.InterruptOutTransactionDelivered); err !=
			errControllerPersonaInterruptOutTerminalClaim {
			panic(err)
		}
	}
	duplicate()
	if allocations := testing.AllocsPerRun(1000, duplicate); allocations != 0 {
		t.Fatalf("fatal duplicate Complete allocated %.2f objects/run", allocations)
	}

	// A lost eligible-state CAS reaches this exact Busy containment path. The
	// helper is exercised directly so the allocation measurement introduces no
	// goroutine/channel coordination of its own.
	casLoss := func() {
		adapter.terminalFence.Store(false)
		adapter.storeState(controllerPersonaInterruptOutCompleting)
		if err := adapter.containFatalCompletion(
			ErrControllerPersonaInterruptOutBusy); err !=
			errControllerPersonaInterruptOutTerminalBusy {
			panic(err)
		}
	}
	casLoss()
	if allocations := testing.AllocsPerRun(1000, casLoss); allocations != 0 {
		t.Fatalf("fatal CAS-loss containment allocated %.2f objects/run", allocations)
	}
}

func TestPersonaInterruptOutSuccessfulPathAllocations(t *testing.T) {
	participant := &recordingDownstreamPacketParticipant{}
	adapter, _ := newTestPersonaInterruptOutAdapter(t, participant)
	wire := testPersonaInterruptOutWire(t)
	run := func() {
		claim, err := adapter.ClaimInterruptOutTransaction(
			usb.InterruptOutTransactionRequest{Endpoint: 1, Data: wire})
		if err != nil {
			panic(err)
		}
		if err = adapter.AdmitInterruptOutTransaction(claim); err != nil {
			panic(err)
		}
		if err = adapter.CompleteInterruptOutTransaction(
			claim, usb.InterruptOutTransactionDelivered); err != nil {
			panic(err)
		}
		ticket, present := adapter.PendingWork()
		if !present {
			panic("missing work")
		}
		now := time.Now()
		if err = adapter.RunPending(
			ticket, now.Add(time.Second), now.Add(2*time.Second)); err != nil {
			panic(err)
		}
	}
	run()
	if allocations := testing.AllocsPerRun(500, run); allocations != 0 {
		t.Fatalf("successful packet path allocated %.2f objects/run", allocations)
	}
}

func interruptOutOutcomeName(outcome usb.InterruptOutTransactionOutcome) string {
	switch outcome {
	case usb.InterruptOutTransactionDeliveryFailed:
		return "delivery-failed"
	case usb.InterruptOutTransactionCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

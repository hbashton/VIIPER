package xboxone

import (
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
	"github.com/Alia5/VIIPER/internal/retainedusb"
)

var errCanonicalFeedbackTestRejected = errors.New(
	"canonical feedback test publisher proven rejection")

type recordingCanonicalFeedbackPublisher struct {
	mu        sync.Mutex
	mailbox   controllerfeedback.Mailbox
	frames    []controllerfeedback.Frame
	failNext  error
	entered   chan struct{}
	release   chan struct{}
	reenter   *ControllerPersonaCanonicalFeedbackExecutor
	reentered bool
}

func (publisher *recordingCanonicalFeedbackPublisher) PublishControllerFeedback(
	wire ControllerPersonaCanonicalFeedbackWireV1,
	deadline time.Time,
) error {
	if publisher.entered != nil {
		select {
		case publisher.entered <- struct{}{}:
		default:
		}
	}
	if publisher.release != nil {
		select {
		case <-publisher.release:
		case <-time.After(time.Until(deadline)):
			return errCanonicalFeedbackTestRejected
		}
	}
	if publisher.reenter != nil {
		_, publisher.reentered = publisher.reenter.Snapshot()
	}
	var frame controllerfeedback.Frame
	if err := frame.UnmarshalFrom(wire[:]); err != nil {
		return err
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.failNext != nil {
		err := publisher.failNext
		publisher.failNext = nil
		return err
	}
	if !publisher.mailbox.Publish(frame) {
		return errCanonicalFeedbackTestRejected
	}
	publisher.frames = append(publisher.frames, frame)
	return nil
}

func (publisher *recordingCanonicalFeedbackPublisher) snapshotFrames() []controllerfeedback.Frame {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	return append([]controllerfeedback.Frame(nil), publisher.frames...)
}

type discardCanonicalFeedbackPublisher struct{}

func (discardCanonicalFeedbackPublisher) PublishControllerFeedback(
	ControllerPersonaCanonicalFeedbackWireV1,
	time.Time,
) error {
	return nil
}

func newCanonicalFeedbackTestExecutor(
	t *testing.T,
	publisher ControllerPersonaCanonicalFeedbackPublisher,
) *ControllerPersonaCanonicalFeedbackExecutor {
	t.Helper()
	executor, err := NewControllerPersonaCanonicalFeedbackExecutor(
		canonicalFeedbackTestBinding(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := uint64(1_000_000)
	executor.clock = func() (uint64, bool) {
		timestamp++
		return timestamp, true
	}
	return executor
}

func canonicalFeedbackClear(generation, order uint64) ControllerPersonaLocalExecution {
	return ControllerPersonaLocalExecution{
		Action: ControllerPersonaClearOutputs, Generation: generation,
		Order: order, ClearEpoch: order + 100,
	}
}

func canonicalFeedbackMotor(generation, order uint64) ControllerPersonaLocalExecution {
	return ControllerPersonaLocalExecution{
		Action: ControllerPersonaApplyDirectMotor, Generation: generation,
		Order: order,
		DirectMotor: RumbleBodyV1{
			Enabled: MotorAll, LeftVibration: 25, RightVibration: 50,
			LeftImpulse: 75, RightImpulse: 100, Duration: 25,
		},
	}
}

func TestCanonicalFeedbackExecutorUsesRequiredHostClock(t *testing.T) {
	publisher := &recordingCanonicalFeedbackPublisher{}
	// Use the production constructor's actual clock, not the deterministic
	// clock substituted by the portable executor unit-test helper.
	executor, err := NewControllerPersonaCanonicalFeedbackExecutor(
		canonicalFeedbackTestBinding(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	applyErr := executor.Execute(canonicalFeedbackMotor(5, 1), deadline)
	if err := executor.CancelAndDrain(deadline); err != nil {
		t.Fatal(err)
	}
	stopErr := executor.DisconnectNeutral(canonicalFeedbackClear(6, 2), deadline)
	frames := publisher.snapshotFrames()
	snapshot, ok := executor.Snapshot()
	if !ok {
		t.Fatal("executor lost its lifecycle snapshot")
	}
	if runtime.GOOS != "windows" {
		if !errors.Is(applyErr, ErrCanonicalFeedbackClockUnavailable) ||
			!errors.Is(stopErr, ErrCanonicalFeedbackClockUnavailable) {
			t.Fatalf("non-Windows publication must fail closed: apply=%v stop=%v", applyErr, stopErr)
		}
		if len(frames) != 0 || snapshot.Stopped || !snapshot.TerminalDrained ||
			snapshot.PersonaGeneration != 5 || snapshot.ExecutionInFlight {
			t.Fatalf("unavailable clock published feedback or advanced terminal proof: frames=%+v state=%+v", frames, snapshot)
		}
		return
	}
	if applyErr != nil || stopErr != nil {
		t.Fatalf("Windows production clock must publish: apply=%v stop=%v", applyErr, stopErr)
	}
	if len(frames) != 2 || frames[0].Command != controllerfeedback.CommandApply ||
		frames[1].Command != controllerfeedback.CommandStop || !snapshot.Stopped ||
		snapshot.PersonaGeneration != 6 {
		t.Fatalf("Windows production Apply/Stop: frames=%+v state=%+v", frames, snapshot)
	}
}

func TestCanonicalFeedbackExecutorRetainsGuideLEDOutsideMotorLane(t *testing.T) {
	publisher := &recordingCanonicalFeedbackPublisher{}
	executor := newCanonicalFeedbackTestExecutor(t, publisher)
	command := GuideLEDCommandV1{
		Pattern: GuideLEDPatternRampToLevel, Intensity: 31,
	}
	if err := executor.Execute(ControllerPersonaLocalExecution{
		Action: ControllerPersonaApplyGuideLED, Generation: 5, Order: 17,
		GuideLED: command,
	}, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("Guide LED execution: %v", err)
	}
	if frames := publisher.snapshotFrames(); len(frames) != 0 {
		t.Fatalf("Guide LED was mislabeled as %d motor frames", len(frames))
	}
	snapshot, ok := executor.Snapshot()
	if !ok || !snapshot.GuideLEDObserved || snapshot.GuideLEDOrder != 17 ||
		snapshot.GuideLED != command || !snapshot.Active {
		t.Fatalf("Guide LED diagnostic snapshot = %+v, ok=%t", snapshot, ok)
	}
}

func TestCanonicalFeedbackExecutorRecoverableClearResetRebindAndResume(
	t *testing.T,
) {
	publisher := &recordingCanonicalFeedbackPublisher{}
	executor := newCanonicalFeedbackTestExecutor(t, publisher)
	deadline := func() time.Time { return time.Now().Add(time.Second) }

	if err := executor.Execute(canonicalFeedbackMotor(5, 1), deadline()); err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(canonicalFeedbackClear(5, 2), deadline()); err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(canonicalFeedbackMotor(5, 3), deadline()); err != nil {
		t.Fatalf("configuration-loss neutral prevented recovery: %v", err)
	}
	if err := executor.ResetAndDrain(deadline()); err != nil {
		t.Fatal(err)
	}
	if err := executor.ResetNeutral(canonicalFeedbackClear(6, 4), deadline()); err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(canonicalFeedbackMotor(5, 5), deadline()); !errors.Is(err, ErrInvalidCanonicalFeedbackBinding) {
		t.Fatalf("stale predecessor generation = %v", err)
	}
	if err := executor.Execute(canonicalFeedbackMotor(6, 6), deadline()); err != nil {
		t.Fatalf("successor four-actuator update did not resume: %v", err)
	}

	frames := publisher.snapshotFrames()
	if len(frames) != 5 {
		t.Fatalf("frames = %d, want 5", len(frames))
	}
	wantCommands := []controllerfeedback.Command{
		controllerfeedback.CommandApply,
		controllerfeedback.CommandNeutral,
		controllerfeedback.CommandApply,
		controllerfeedback.CommandNeutral,
		controllerfeedback.CommandApply,
	}
	for index, frame := range frames {
		if frame.Command != wantCommands[index] {
			t.Fatalf("frame[%d] command = %d, want %d", index,
				frame.Command, wantCommands[index])
		}
	}
	wantSequences := [...]uint64{1, 2, 3, 4, 6}
	for index, frame := range frames {
		if frame.Sequence != wantSequences[index] {
			t.Fatalf("frame[%d] sequence = %d, want %d", index,
				frame.Sequence, wantSequences[index])
		}
		if index != 0 && frame.Sequence <= frames[index-1].Sequence {
			t.Fatalf("frame sequence regressed across generation: %d then %d",
				frames[index-1].Sequence, frame.Sequence)
		}
	}
	resumed := frames[len(frames)-1]
	if resumed.BodyLow != 0x4000 || resumed.BodyHigh != 0x8000 ||
		resumed.LeftTrigger != 0xbfff || resumed.RightTrigger != 0xffff {
		t.Fatalf("resumed channels = %+v", resumed)
	}
	snapshot, ok := executor.Snapshot()
	if !ok || !snapshot.Active || snapshot.PersonaGeneration != 6 ||
		snapshot.AuthorizedPersonaGeneration != 6 ||
		snapshot.OwnershipEpoch != canonicalFeedbackTestBinding().OwnershipEpoch {
		t.Fatalf("snapshot = %+v ok=%v", snapshot, ok)
	}
}

func TestCanonicalFeedbackExecutorOutputFreeResetBoundaryRebindsOnFirstSuccessorFeedback(
	t *testing.T,
) {
	publisher := &recordingCanonicalFeedbackPublisher{}
	executor := newCanonicalFeedbackTestExecutor(t, publisher)
	deadline := func() time.Time { return time.Now().Add(time.Second) }

	if err := executor.Execute(canonicalFeedbackClear(5, 1), deadline()); err != nil {
		t.Fatal(err)
	}
	performReset := ControllerPersonaLocalExecution{
		Action: ControllerPersonaPerformReset, Generation: 5, Order: 2,
	}
	if err := executor.Execute(performReset, deadline()); err != nil {
		t.Fatalf("output-free reset boundary = %v", err)
	}
	snapshot, _ := executor.Snapshot()
	if !snapshot.Active || snapshot.PersonaGeneration != 5 ||
		snapshot.AuthorizedPersonaGeneration != 6 {
		t.Fatalf("boundary did not fence predecessor generation: %+v", snapshot)
	}
	if err := executor.Execute(canonicalFeedbackMotor(5, 3), deadline()); !errors.Is(
		err, ErrInvalidCanonicalFeedbackBinding) {
		t.Fatalf("stale predecessor after boundary = %v", err)
	}
	if err := executor.Execute(canonicalFeedbackMotor(6, 4), deadline()); err != nil {
		t.Fatalf("first successor feedback = %v", err)
	}

	frames := publisher.snapshotFrames()
	if len(frames) != 2 ||
		frames[0].Command != controllerfeedback.CommandNeutral ||
		frames[0].Sequence != 1 ||
		frames[1].Command != controllerfeedback.CommandApply ||
		frames[1].Sequence != 4 {
		t.Fatalf("frames = %+v", frames)
	}
	snapshot, _ = executor.Snapshot()
	if !snapshot.Active || snapshot.PersonaGeneration != 6 ||
		snapshot.AuthorizedPersonaGeneration != 6 {
		t.Fatalf("successor binding was not promoted: %+v", snapshot)
	}
}

func TestCanonicalFeedbackExecutorDisconnectStopIsTerminal(t *testing.T) {
	publisher := &recordingCanonicalFeedbackPublisher{}
	executor := newCanonicalFeedbackTestExecutor(t, publisher)
	deadline := func() time.Time { return time.Now().Add(time.Second) }
	if err := executor.Execute(canonicalFeedbackMotor(5, 1), deadline()); err != nil {
		t.Fatal(err)
	}
	if err := executor.CancelAndDrain(deadline()); err != nil {
		t.Fatal(err)
	}
	if err := executor.DisconnectNeutral(canonicalFeedbackClear(6, 2),
		deadline()); err != nil {
		t.Fatal(err)
	}
	frames := publisher.snapshotFrames()
	if len(frames) != 2 || frames[1].Command != controllerfeedback.CommandStop {
		t.Fatalf("frames = %+v", frames)
	}
	if err := executor.Execute(canonicalFeedbackMotor(6, 3), deadline()); !errors.Is(err, ErrCanonicalFeedbackExecutorState) {
		t.Fatalf("post-stop execution = %v", err)
	}

	resurrection := frames[0]
	resurrection.Sequence = 3
	resurrection.TimestampMicroseconds++
	if publisher.mailbox.Publish(resurrection) {
		t.Fatal("canonical mailbox resurrected a terminal ownership epoch")
	}
	snapshot, ok := executor.Snapshot()
	if !ok || !snapshot.Stopped || snapshot.PersonaGeneration != 6 {
		t.Fatalf("snapshot = %+v ok=%v", snapshot, ok)
	}
}

func TestCanonicalFeedbackExecutorRejectedResetAndStopRemainFencedAndRetryable(
	t *testing.T,
) {
	publisher := &recordingCanonicalFeedbackPublisher{}
	executor := newCanonicalFeedbackTestExecutor(t, publisher)
	deadline := func() time.Time { return time.Now().Add(time.Second) }
	if err := executor.ResetAndDrain(deadline()); err != nil {
		t.Fatal(err)
	}
	publisher.failNext = errCanonicalFeedbackTestRejected
	reset := canonicalFeedbackClear(6, 1)
	if err := executor.ResetNeutral(reset, deadline()); !errors.Is(err, errCanonicalFeedbackTestRejected) {
		t.Fatalf("reset rejection = %v", err)
	}
	if frames := publisher.snapshotFrames(); len(frames) != 0 {
		t.Fatalf("proven reset rejection accepted frames: %+v", frames)
	}
	if _, _, present := publisher.mailbox.ReadLatest(); present {
		t.Fatal("proven reset rejection advanced the mailbox")
	}
	time.Sleep(10 * time.Millisecond)
	if frames := publisher.snapshotFrames(); len(frames) != 0 {
		t.Fatalf("proven reset rejection published late: %+v", frames)
	}
	snapshot, _ := executor.Snapshot()
	if !snapshot.ResetDrained || snapshot.PersonaGeneration != 5 {
		t.Fatalf("rejected reset changed binding: %+v", snapshot)
	}
	if err := executor.Execute(canonicalFeedbackMotor(5, 2), deadline()); !errors.Is(err, ErrCanonicalFeedbackExecutorState) {
		t.Fatalf("rejected reset reopened execution: %v", err)
	}
	if err := executor.ResetNeutral(reset, deadline()); err != nil {
		t.Fatalf("exact proven-rejected reset retry = %v", err)
	}
	if err := executor.CancelAndDrain(deadline()); err != nil {
		t.Fatal(err)
	}
	publisher.failNext = errCanonicalFeedbackTestRejected
	stop := canonicalFeedbackClear(7, 2)
	if err := executor.DisconnectNeutral(stop, deadline()); !errors.Is(err, errCanonicalFeedbackTestRejected) {
		t.Fatalf("stop rejection = %v", err)
	}
	if frames := publisher.snapshotFrames(); len(frames) != 1 {
		t.Fatalf("proven stop rejection accepted bytes: %+v", frames)
	}
	snapshot, _ = executor.Snapshot()
	if !snapshot.TerminalDrained || snapshot.Stopped ||
		snapshot.PersonaGeneration != 6 {
		t.Fatalf("rejected stop changed binding: %+v", snapshot)
	}
	if err := executor.DisconnectNeutral(stop, deadline()); err != nil {
		t.Fatalf("exact proven-rejected stop retry = %v", err)
	}
}

func TestCanonicalFeedbackExecutorProvenPublisherFailureHasNoAcceptanceOrLatePublication(
	t *testing.T,
) {
	publisher := &recordingCanonicalFeedbackPublisher{
		failNext: errCanonicalFeedbackTestRejected,
	}
	executor := newCanonicalFeedbackTestExecutor(t, publisher)
	execution := canonicalFeedbackMotor(5, 1)
	deadline := func() time.Time { return time.Now().Add(time.Second) }

	if err := executor.Execute(execution, deadline()); !errors.Is(
		err, errCanonicalFeedbackTestRejected) {
		t.Fatalf("publisher rejection = %v", err)
	}
	if frames := publisher.snapshotFrames(); len(frames) != 0 {
		t.Fatalf("rejected publication accepted bytes: %+v", frames)
	}
	if _, _, present := publisher.mailbox.ReadLatest(); present {
		t.Fatal("rejected publication advanced the mailbox")
	}
	time.Sleep(10 * time.Millisecond)
	if frames := publisher.snapshotFrames(); len(frames) != 0 {
		t.Fatalf("rejected publication occurred late: %+v", frames)
	}
	if err := executor.Execute(execution, deadline()); err != nil {
		t.Fatalf("exact retry after proven rejection = %v", err)
	}
	frames := publisher.snapshotFrames()
	if len(frames) != 1 || frames[0].Sequence != execution.Order ||
		frames[0].Command != controllerfeedback.CommandApply {
		t.Fatalf("retry frames = %+v", frames)
	}
}

func TestCanonicalFeedbackExecutorGenerationIntentDeadlineAndReentrancyFailClosed(
	t *testing.T,
) {
	publisher := &recordingCanonicalFeedbackPublisher{}
	executor := newCanonicalFeedbackTestExecutor(t, publisher)
	publisher.reenter = executor
	deadline := time.Now().Add(time.Second)
	if err := executor.Execute(canonicalFeedbackMotor(5, 1), deadline); err != nil {
		t.Fatal(err)
	}
	if !publisher.reentered {
		t.Fatal("publisher could not take reentrant diagnostic snapshot")
	}
	if err := executor.ResetAndDrain(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for name, clear := range map[string]ControllerPersonaLocalExecution{
		"same generation":    canonicalFeedbackClear(5, 2),
		"skipped generation": canonicalFeedbackClear(7, 2),
		"wrong action":       canonicalFeedbackMotor(6, 2),
	} {
		t.Run(name, func(t *testing.T) {
			if err := executor.ResetNeutral(clear, time.Now().Add(time.Second)); !errors.Is(err, ErrInvalidCanonicalFeedbackBinding) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if err := executor.ResetNeutral(canonicalFeedbackClear(6, 2),
		time.Now().Add(-time.Millisecond)); !errors.Is(err, ErrCanonicalFeedbackExecutorDeadline) {
		t.Fatalf("expired deadline = %v", err)
	}
}

func TestCanonicalFeedbackExecutorResetDrainFencesConcurrentExecute(t *testing.T) {
	publisher := &recordingCanonicalFeedbackPublisher{
		entered: make(chan struct{}, 1), release: make(chan struct{}),
	}
	executor := newCanonicalFeedbackTestExecutor(t, publisher)
	executeDone := make(chan error, 1)
	go func() {
		executeDone <- executor.Execute(canonicalFeedbackMotor(5, 1),
			time.Now().Add(time.Second))
	}()
	select {
	case <-publisher.entered:
	case <-time.After(time.Second):
		t.Fatal("publisher did not enter")
	}
	drainDone := make(chan error, 1)
	go func() {
		drainDone <- executor.ResetAndDrain(time.Now().Add(time.Second))
	}()
	for deadline := time.Now().Add(time.Second); ; {
		snapshot, _ := executor.Snapshot()
		if snapshot.ExecutionInFlight && !snapshot.Active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reset did not fence admission")
		}
		time.Sleep(time.Millisecond)
	}
	if err := executor.Execute(canonicalFeedbackMotor(5, 2),
		time.Now().Add(time.Second)); !errors.Is(err, ErrCanonicalFeedbackExecutorState) {
		t.Fatalf("concurrent post-fence Execute = %v", err)
	}
	close(publisher.release)
	if err := <-executeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-drainDone; err != nil {
		t.Fatal(err)
	}
	snapshot, _ := executor.Snapshot()
	if !snapshot.ResetDrained || snapshot.ExecutionInFlight {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestCanonicalFeedbackExecutorDrainTimeoutsRemainFencedAndExactlyRetryable(
	t *testing.T,
) {
	t.Run("reset drain", func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{
			entered: make(chan struct{}, 1), release: make(chan struct{}),
		}
		executor := newCanonicalFeedbackTestExecutor(t, publisher)
		executeDone := make(chan error, 1)
		go func() {
			executeDone <- executor.Execute(canonicalFeedbackMotor(5, 1),
				time.Now().Add(2*time.Second))
		}()
		select {
		case <-publisher.entered:
		case <-time.After(time.Second):
			t.Fatal("publisher did not enter")
		}
		if err := executor.ResetAndDrain(time.Now().Add(20 * time.Millisecond)); !errors.Is(err, ErrCanonicalFeedbackExecutorDeadline) {
			t.Fatalf("first reset drain = %v", err)
		}
		snapshot, _ := executor.Snapshot()
		if snapshot.Active || !snapshot.ExecutionInFlight || snapshot.ResetDrained {
			t.Fatalf("reset timeout did not preserve fence: %+v", snapshot)
		}
		close(publisher.release)
		if err := <-executeDone; err != nil {
			t.Fatal(err)
		}
		if err := executor.ResetAndDrain(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("resumed reset drain = %v", err)
		}
		snapshot, _ = executor.Snapshot()
		if !snapshot.ResetDrained || snapshot.Active || snapshot.ExecutionInFlight {
			t.Fatalf("resumed reset snapshot = %+v", snapshot)
		}
	})

	t.Run("terminal drain", func(t *testing.T) {
		publisher := &recordingCanonicalFeedbackPublisher{
			entered: make(chan struct{}, 1), release: make(chan struct{}),
		}
		executor := newCanonicalFeedbackTestExecutor(t, publisher)
		executeDone := make(chan error, 1)
		go func() {
			executeDone <- executor.Execute(canonicalFeedbackMotor(5, 1),
				time.Now().Add(2*time.Second))
		}()
		select {
		case <-publisher.entered:
		case <-time.After(time.Second):
			t.Fatal("publisher did not enter")
		}
		if err := executor.CancelAndDrain(time.Now().Add(20 * time.Millisecond)); !errors.Is(err, ErrCanonicalFeedbackExecutorDeadline) {
			t.Fatalf("first terminal drain = %v", err)
		}
		snapshot, _ := executor.Snapshot()
		if snapshot.Active || !snapshot.ExecutionInFlight || snapshot.TerminalDrained {
			t.Fatalf("terminal timeout did not preserve fence: %+v", snapshot)
		}
		close(publisher.release)
		if err := <-executeDone; err != nil {
			t.Fatal(err)
		}
		if err := executor.CancelAndDrain(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("resumed terminal drain = %v", err)
		}
		snapshot, _ = executor.Snapshot()
		if !snapshot.TerminalDrained || snapshot.Active || snapshot.ExecutionInFlight {
			t.Fatalf("resumed terminal snapshot = %+v", snapshot)
		}
	})
}

func TestCanonicalFeedbackExecutorTerminalDrainWinsDuringResetNeutral(
	t *testing.T,
) {
	publisher := &recordingCanonicalFeedbackPublisher{
		entered: make(chan struct{}, 1), release: make(chan struct{}),
	}
	executor := newCanonicalFeedbackTestExecutor(t, publisher)
	if err := executor.ResetAndDrain(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	resetDone := make(chan error, 1)
	go func() {
		resetDone <- executor.ResetNeutral(canonicalFeedbackClear(6, 1),
			time.Now().Add(2*time.Second))
	}()
	select {
	case <-publisher.entered:
	case <-time.After(time.Second):
		t.Fatal("reset neutral did not enter publisher")
	}
	cancelDone := make(chan error, 1)
	go func() {
		cancelDone <- executor.CancelAndDrain(time.Now().Add(2 * time.Second))
	}()
	for deadline := time.Now().Add(time.Second); ; {
		executor.mu.Lock()
		terminalDraining :=
			executor.state == canonicalFeedbackExecutorTerminalDraining
		inFlight := executor.inFlight
		executor.mu.Unlock()
		if terminalDraining && inFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal drain did not preserve the reset fence")
		}
		time.Sleep(time.Millisecond)
	}
	close(publisher.release)
	if err := <-resetDone; err != nil {
		t.Fatalf("accepted reset neutral = %v", err)
	}
	if err := <-cancelDone; err != nil {
		t.Fatalf("terminal drain = %v", err)
	}
	snapshot, _ := executor.Snapshot()
	if !snapshot.TerminalDrained || snapshot.Active ||
		snapshot.ExecutionInFlight || snapshot.PersonaGeneration != 6 ||
		snapshot.AuthorizedPersonaGeneration != 6 {
		t.Fatalf("terminal drain lost reset completion: %+v", snapshot)
	}
	if err := executor.DisconnectNeutral(canonicalFeedbackClear(7, 2),
		time.Now().Add(time.Second)); err != nil {
		t.Fatalf("terminal stop = %v", err)
	}
	frames := publisher.snapshotFrames()
	if len(frames) != 2 ||
		frames[0].Command != controllerfeedback.CommandNeutral ||
		frames[0].Sequence != 1 ||
		frames[1].Command != controllerfeedback.CommandStop ||
		frames[1].Sequence != 2 {
		t.Fatalf("frames = %+v", frames)
	}
}

func TestCanonicalFeedbackExecutorComposesWithDormantRetainedAdapterResetAndDisconnect(
	t *testing.T,
) {
	publisher := &recordingCanonicalFeedbackPublisher{}
	binding := canonicalFeedbackTestBinding()
	binding.PersonaGeneration = 1
	executor, err := NewControllerPersonaCanonicalFeedbackExecutor(
		binding, publisher)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := uint64(2_000_000)
	executor.clock = func() (uint64, bool) {
		timestamp++
		return timestamp, true
	}
	adapter, err := NewDormantRetainedUSBAdapter(
		newTestControllerPersonaEngine(t, []byte{1}, 0),
		11, 22, executor, 250*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopRetainedUSBAdapterTestWorker(adapter) })
	lease := adapterTestLease(adapter, 41)
	if result, err := adapter.BindImport(
		lease, time.Now().Add(time.Second)); err != nil ||
		result.State != retainedusb.ImportBindBound {
		t.Fatalf("bind = (%+v, %v)", result, err)
	}

	configureRetainedTestPersona(t, adapter)
	if hello, _ := retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 3, 3, 64), 1); hello.Result != retainedusb.ResultData {
		t.Fatalf("predecessor hello = %+v", hello)
	}
	motor := RumbleBodyV1{
		Enabled: MotorAll, LeftVibration: 25, RightVibration: 50,
		LeftImpulse: 75, RightImpulse: 100, Duration: 25,
	}
	writeMotor := func(ordinal uint64, nowMS uint64) {
		t.Helper()
		wire := make([]byte, DirectMotorMessageSize)
		if err := EncodeDirectMotorMessageInto(
			wire, byte(ordinal), motor); err != nil {
			t.Fatal(err)
		}
		retainedPrepareAndComplete(t, adapter,
			retainedOUTRequest(41, ordinal, uint32(ordinal), wire), nowMS)
		triggerAndRetireLocalAction(t, adapter, ordinal+1, nowMS+1)
	}
	waitForFrames := func(count int) []controllerfeedback.Frame {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for {
			frames := publisher.snapshotFrames()
			if len(frames) >= count {
				return frames
			}
			if time.Now().After(deadline) {
				t.Fatalf("frames = %d, want at least %d", len(frames), count)
			}
			time.Sleep(time.Millisecond)
		}
	}

	writeMotor(4, 2)
	frames := waitForFrames(1)
	if frames[0].Command != controllerfeedback.CommandApply ||
		frames[0].BodyLow != 0x4000 || frames[0].BodyHigh != 0x8000 ||
		frames[0].LeftTrigger != 0xbfff || frames[0].RightTrigger != 0xffff {
		t.Fatalf("predecessor feedback = %+v", frames[0])
	}

	reset := adapterTestReset(lease, 71, 1)
	resetResult, err := adapter.ResetAndRestart(
		reset, time.Now().Add(time.Second))
	if err != nil || resetResult.State != retainedusb.ImportResetSafe {
		t.Fatalf("reset = (%+v, %v)", resetResult, err)
	}
	frames = waitForFrames(2)
	if frames[1].Command != controllerfeedback.CommandNeutral ||
		frames[1].Sequence <= frames[0].Sequence {
		t.Fatalf("reset feedback = %+v", frames)
	}
	executorSnapshot, _ := executor.Snapshot()
	if !executorSnapshot.Active || executorSnapshot.PersonaGeneration != 2 ||
		executorSnapshot.AuthorizedPersonaGeneration != 2 {
		t.Fatalf("reset executor = %+v", executorSnapshot)
	}
	if err := executor.Execute(canonicalFeedbackMotor(1,
		frames[1].Sequence+1), time.Now().Add(time.Second)); !errors.Is(
		err, ErrInvalidCanonicalFeedbackBinding) {
		t.Fatalf("stale direct predecessor after composed reset = %v", err)
	}

	for _, request := range []retainedusb.Request{
		retainedControlRequest(41, 6, 6,
			testUSBSetup(usbRequestTypeDeviceOut,
				usbRequestSetAddress, 1, 0, 0)),
		retainedControlRequest(41, 7, 7,
			testUSBSetup(usbRequestTypeDeviceOut,
				usbRequestSetConfiguration, uint16(usbConfigurationGIP), 0, 0)),
	} {
		if result, _ := retainedPrepareAndComplete(
			t, adapter, request, 4); result.Result != retainedusb.ResultSuccess {
			t.Fatalf("successor configuration = %+v", result)
		}
	}
	if hello, _ := retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 8, 8, 64), 5); hello.Result != retainedusb.ResultData {
		t.Fatalf("successor hello = %+v", hello)
	}
	writeMotor(9, 6)
	frames = waitForFrames(3)
	if frames[2].Command != controllerfeedback.CommandApply ||
		frames[2].Sequence <= frames[1].Sequence ||
		frames[2].BodyLow != 0x4000 || frames[2].BodyHigh != 0x8000 ||
		frames[2].LeftTrigger != 0xbfff || frames[2].RightTrigger != 0xffff {
		t.Fatalf("successor feedback = %+v", frames)
	}

	deadline := time.Now().Add(time.Second)
	drain, err := adapter.CancelAndDrain(
		lease, retainedusb.ImportCloseExplicitDetach, deadline)
	if err != nil || drain.State != retainedusb.ImportDrainDrained {
		t.Fatalf("terminal drain = (%+v, %v)", drain, err)
	}
	disconnect, err := adapter.DisconnectNeutral(
		lease, retainedusb.ImportCloseExplicitDetach, deadline)
	if err != nil || disconnect.State != retainedusb.ImportDisconnectSafe {
		t.Fatalf("disconnect = (%+v, %v)", disconnect, err)
	}
	frames = waitForFrames(4)
	if frames[3].Command != controllerfeedback.CommandStop ||
		frames[3].Sequence <= frames[2].Sequence {
		t.Fatalf("terminal feedback = %+v", frames)
	}
	executorSnapshot, _ = executor.Snapshot()
	if !executorSnapshot.Stopped || executorSnapshot.PersonaGeneration != 3 ||
		executorSnapshot.AuthorizedPersonaGeneration != 3 {
		t.Fatalf("stopped executor = %+v", executorSnapshot)
	}
}

func TestCanonicalFeedbackExecutorSteadyStateAllocatesZero(t *testing.T) {
	executor := newCanonicalFeedbackTestExecutor(t,
		discardCanonicalFeedbackPublisher{})
	execution := canonicalFeedbackMotor(5, 1)
	deadline := time.Now().Add(time.Hour)
	if err := executor.Execute(execution, deadline); err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(10_000, func() {
		if err := executor.Execute(execution, deadline); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("allocations = %v, want 0", allocs)
	}
}

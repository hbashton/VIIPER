package xboxone

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordingDownstreamPacketParticipant struct {
	mu sync.Mutex

	preflightCalls int
	executeCalls   int
	cancelCalls    int
	preflights     [8]ControllerDownstreamPacketExecution
	executions     [8]ControllerDownstreamPacketExecution
	cancellations  [8]ControllerDownstreamPacketExecution
	applied        ControllerDownstreamPacketExecution

	preflightErr   error
	executeErr     error
	cancelErr      error
	preflightPanic any
	executePanic   any
	cancelPanic    any
	applyThenPanic bool

	executeStarted chan struct{}
	executeRelease chan struct{}
	executeExited  chan struct{}
	startOnce      sync.Once
	releaseOnce    sync.Once
	exitOnce       sync.Once
}

func (participant *recordingDownstreamPacketParticipant) PreflightControllerDownstreamPacket(
	execution ControllerDownstreamPacketExecution,
) error {
	participant.mu.Lock()
	index := participant.preflightCalls
	participant.preflightCalls++
	if index < len(participant.preflights) {
		participant.preflights[index] = execution
	}
	panicValue := participant.preflightPanic
	err := participant.preflightErr
	participant.mu.Unlock()
	if panicValue != nil {
		panic(panicValue)
	}
	return err
}

func (participant *recordingDownstreamPacketParticipant) ExecuteControllerDownstreamPacket(
	execution ControllerDownstreamPacketExecution,
	_ time.Time,
) error {
	participant.mu.Lock()
	index := participant.executeCalls
	participant.executeCalls++
	if index < len(participant.executions) {
		participant.executions[index] = execution
	}
	panicValue := participant.executePanic
	applyThenPanic := participant.applyThenPanic
	err := participant.executeErr
	started := participant.executeStarted
	release := participant.executeRelease
	exited := participant.executeExited
	participant.mu.Unlock()
	if started != nil {
		participant.startOnce.Do(func() { close(started) })
	}
	if release != nil {
		<-release
	}
	if exited != nil {
		defer participant.exitOnce.Do(func() { close(exited) })
	}
	if panicValue != nil {
		if applyThenPanic {
			participant.mu.Lock()
			participant.applied = execution
			participant.mu.Unlock()
		}
		panic(panicValue)
	}
	if err == nil {
		participant.mu.Lock()
		participant.applied = execution
		participant.mu.Unlock()
	}
	return err
}

func (participant *recordingDownstreamPacketParticipant) CancelControllerDownstreamPacketAndDrain(
	execution ControllerDownstreamPacketExecution,
	_ time.Time,
) error {
	participant.mu.Lock()
	index := participant.cancelCalls
	participant.cancelCalls++
	if index < len(participant.cancellations) {
		participant.cancellations[index] = execution
	}
	panicValue := participant.cancelPanic
	err := participant.cancelErr
	release := participant.executeRelease
	exited := participant.executeExited
	participant.mu.Unlock()
	if release != nil {
		participant.releaseOnce.Do(func() { close(release) })
	}
	if exited != nil {
		<-exited
	}
	if panicValue != nil {
		panic(panicValue)
	}
	return err
}

func (participant *recordingDownstreamPacketParticipant) setPreflightError(err error) {
	participant.mu.Lock()
	participant.preflightErr = err
	participant.mu.Unlock()
}

func (participant *recordingDownstreamPacketParticipant) setExecuteError(err error) {
	participant.mu.Lock()
	participant.executeErr = err
	participant.mu.Unlock()
}

func (participant *recordingDownstreamPacketParticipant) counts() (int, int, int) {
	participant.mu.Lock()
	defer participant.mu.Unlock()
	return participant.preflightCalls, participant.executeCalls, participant.cancelCalls
}

func testAtomicControllerDownstreamPacket(
	t *testing.T,
) (ControllerDownstreamPacket, [3]ControllerDownstreamPacketAction) {
	t.Helper()
	firstMotor := RumbleBodyV1{
		Enabled:       MotorLeftVibration | MotorRightVibration | MotorLeftImpulse,
		LeftVibration: 21, RightVibration: 32, LeftImpulse: 43,
		Duration: 5,
	}
	secondMotor := RumbleBodyV1{
		Enabled:        MotorRightVibration | MotorRightImpulse,
		RightVibration: 67, RightImpulse: 89, Duration: 7,
	}
	led := GuideLEDCommandV1{
		Pattern: GuideLEDPatternRampToLevel, Intensity: 41,
	}
	firstWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(firstWire, 0x21, firstMotor); err != nil {
		t.Fatal(err)
	}
	ledWire := make([]byte, GuideLEDCommandMessageSize)
	if err := EncodeGuideLEDCommandMessageInto(ledWire, 0x22, led); err != nil {
		t.Fatal(err)
	}
	secondWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(secondWire, 0x23, secondMotor); err != nil {
		t.Fatal(err)
	}
	packet, err := DecodeControllerDownstreamPacket(
		bytes.Join([][]byte{firstWire, ledWire, secondWire}, nil))
	if err != nil {
		t.Fatal(err)
	}
	return packet, [3]ControllerDownstreamPacketAction{
		{Action: ControllerPersonaApplyDirectMotor, WireIndex: 0,
			Sequence: 0x21, DirectMotor: firstMotor},
		{Action: ControllerPersonaApplyGuideLED, WireIndex: 1,
			Sequence: 0x22, GuideLED: led},
		{Action: ControllerPersonaApplyDirectMotor, WireIndex: 2,
			Sequence: 0x23, DirectMotor: secondMotor},
	}
}

func TestControllerDownstreamPacketExecutionOwnerZeroValueFailsFast(
	t *testing.T,
) {
	owner := &ControllerDownstreamPacketExecutionOwner{}
	deadline := time.Now().Add(time.Second)
	operations := map[string]func() error{
		"claim": func() error {
			_, err := owner.Claim(ControllerDownstreamPacket{})
			return err
		},
		"claim retry": func() error {
			_, err := owner.ClaimRetry()
			return err
		},
		"admit": func() error {
			_, err := owner.Admit(ControllerDownstreamPacketClaim{})
			return err
		},
		"execute": func() error {
			return owner.Execute(ControllerDownstreamPacketExecutionLease{}, deadline)
		},
		"cancel and drain": func() error {
			return owner.CancelAndDrain(
				ControllerDownstreamPacketExecutionLease{}, deadline)
		},
		"resolve": func() error {
			return owner.Resolve(ControllerDownstreamPacketClaim{},
				ControllerDownstreamPacketDeferred)
		},
	}

	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			result := make(chan error, 1)
			go func() { result <- operation() }()
			select {
			case err := <-result:
				if !errors.Is(err,
					ErrControllerDownstreamPacketExecutorUninitialized) {
					t.Fatalf("error = %v, want uninitialized", err)
				}
			case <-time.After(time.Second):
				t.Fatal("zero-value owner operation blocked")
			}
		})
	}

	if snapshot, ok := owner.Snapshot(); ok ||
		snapshot != (ControllerDownstreamPacketExecutionSnapshot{}) {
		t.Fatalf("zero-value snapshot = (%+v, %t)", snapshot, ok)
	}
}

func TestControllerDownstreamPacketExecutionOwnerCopyFailsFast(t *testing.T) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	owner, err := NewControllerDownstreamPacketExecutionOwner(
		&recordingDownstreamPacketParticipant{})
	if err != nil {
		t.Fatal(err)
	}

	copied := &ControllerDownstreamPacketExecutionOwner{
		self: owner, operation: owner.operation, progress: owner.progress,
		participant: owner.participant, state: owner.state,
	}
	if snapshot, ok := copied.Snapshot(); ok ||
		snapshot != (ControllerDownstreamPacketExecutionSnapshot{}) {
		t.Fatalf("copied-owner snapshot = (%+v, %t)", snapshot, ok)
	}
	if _, err := copied.Claim(packet); !errors.Is(
		err, ErrControllerDownstreamPacketExecutorUninitialized) {
		t.Fatalf("copied-owner claim error = %v", err)
	}

	claim, err := owner.Claim(packet)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDeferred); err != nil {
		t.Fatal(err)
	}
}

func TestControllerDownstreamPacketExecutionWholeVectorOrder(t *testing.T) {
	packet, want := testAtomicControllerDownstreamPacket(t)
	participant := &recordingDownstreamPacketParticipant{}
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}

	claim, err := owner.Claim(packet)
	if err != nil {
		t.Fatal(err)
	}
	if !claim.Valid() || claim.PacketEpoch() != 1 || claim.MessageCount() != len(want) {
		t.Fatalf("claim = %+v", claim)
	}
	lease, err := owner.Admit(claim)
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Valid() || lease.PacketEpoch() != 1 || lease.Len() != len(want) {
		t.Fatalf("lease = %+v", lease)
	}
	for index := range want {
		action, ok := lease.Action(index)
		if !ok || action != want[index] {
			t.Fatalf("action %d = (%+v, %t), want %+v", index, action, ok, want[index])
		}
	}
	if action, ok := lease.Action(-1); ok || action != (ControllerDownstreamPacketAction{}) {
		t.Fatalf("negative action = (%+v, %t)", action, ok)
	}
	if action, ok := lease.Action(lease.Len()); ok || action != (ControllerDownstreamPacketAction{}) {
		t.Fatalf("past-end action = (%+v, %t)", action, ok)
	}

	if err := owner.Execute(lease, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDelivered); err != nil {
		t.Fatal(err)
	}
	snapshot, ok := owner.Snapshot()
	if !ok || !snapshot.Idle || snapshot.PacketEpoch != 0 ||
		snapshot.NextPacketEpoch != 1 || snapshot.NextClaimToken != 1 {
		t.Fatalf("terminal snapshot = %+v, %t", snapshot, ok)
	}

	participant.mu.Lock()
	defer participant.mu.Unlock()
	if participant.preflightCalls != 2 || participant.executeCalls != 1 ||
		participant.cancelCalls != 0 {
		t.Fatalf("participant calls = preflight %d execute %d cancel %d",
			participant.preflightCalls, participant.executeCalls, participant.cancelCalls)
	}
	if participant.preflights[0] != participant.preflights[1] ||
		participant.executions[0] != participant.preflights[0] ||
		participant.applied != participant.preflights[0] {
		t.Fatal("participant did not receive one immutable whole-vector value")
	}
}

func TestControllerDownstreamPacketPreparedResolutionFencesExecuteAndCancel(
	t *testing.T,
) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	participant := &recordingDownstreamPacketParticipant{}
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := owner.prepareResolution(
		claim, ControllerDownstreamPacketDeferred)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, _ := owner.Snapshot(); !snapshot.ResolutionPrepared ||
		snapshot.ExecutionInFlight || snapshot.RetryPending {
		t.Fatalf("prepared snapshot = %+v", snapshot)
	}
	if err := owner.Execute(lease, time.Now().Add(time.Second)); !errors.Is(
		err, ErrControllerDownstreamPacketClaimNotAdmitted) {
		t.Fatalf("prepared Execute error = %v", err)
	}
	if err := owner.CancelAndDrain(lease, time.Now().Add(time.Second)); !errors.Is(
		err, ErrControllerDownstreamPacketDrainRequired) {
		t.Fatalf("prepared CancelAndDrain error = %v", err)
	}
	if preflight, execute, cancel := participant.counts(); preflight != 2 || execute != 0 || cancel != 0 {
		t.Fatalf("participant calls = %d %d %d", preflight, execute, cancel)
	}
	forged := credential
	forged.token++
	if err := owner.commitPreparedResolution(forged); !errors.Is(
		err, ErrInvalidControllerDownstreamPacketOutcome) {
		t.Fatalf("forged credential error = %v", err)
	}
	if snapshot, _ := owner.Snapshot(); !snapshot.ResolutionPrepared {
		t.Fatalf("forged credential retired preparation: %+v", snapshot)
	}
	if err := owner.commitPreparedResolution(credential); err != nil {
		t.Fatal(err)
	}
	if err := owner.commitPreparedResolution(credential); !errors.Is(
		err, ErrInvalidControllerDownstreamPacketOutcome) {
		t.Fatalf("duplicate credential error = %v", err)
	}
	if snapshot, _ := owner.Snapshot(); !snapshot.RetryPending ||
		snapshot.ResolutionPrepared {
		t.Fatalf("terminal snapshot = %+v", snapshot)
	}
}

func TestControllerDownstreamPacketExecutionFullPreflightBeforeMutation(t *testing.T) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	direct, _ := packet.Message(0)
	unsupported := ControllerDownstreamPacket{
		messages: [controllerDownstreamPacketMaximumMessages]ControllerDownstreamMessage{
			direct,
			{Kind: ControllerDownstreamLifecycle, Sequence: 0x44,
				Lifecycle: ControllerHostCommand{
					Kind:     ControllerHostCommandSetDeviceState,
					Sequence: 0x44, State: SetDeviceStateQuiesce,
				}},
		},
		length: 2,
	}
	participant := &recordingDownstreamPacketParticipant{}
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Claim(unsupported); !errors.Is(
		err, ErrControllerDownstreamPacketNonAtomicAction) {
		t.Fatalf("unsupported error = %v", err)
	}
	preflightCalls, executeCalls, cancelCalls := participant.counts()
	if preflightCalls != 0 || executeCalls != 0 || cancelCalls != 0 {
		t.Fatalf("unsupported packet reached participant: %d %d %d",
			preflightCalls, executeCalls, cancelCalls)
	}
	snapshot, _ := owner.Snapshot()
	if !snapshot.Idle || snapshot.NextPacketEpoch != 0 || snapshot.NextClaimToken != 0 {
		t.Fatalf("unsupported packet mutated owner: %+v", snapshot)
	}

	preflightFailure := errors.New("atomic sink cannot reserve vector")
	participant.setPreflightError(preflightFailure)
	if _, err := owner.Claim(packet); !errors.Is(err, preflightFailure) {
		t.Fatalf("preflight error = %v", err)
	}
	snapshot, _ = owner.Snapshot()
	if !snapshot.Idle || snapshot.NextPacketEpoch != 0 || snapshot.NextClaimToken != 0 {
		t.Fatalf("failed preflight mutated owner: %+v", snapshot)
	}
	participant.setPreflightError(nil)
	claim, err := owner.Claim(packet)
	if err != nil {
		t.Fatal(err)
	}
	if claim.PacketEpoch() != 1 {
		t.Fatalf("first accepted epoch = %d", claim.PacketEpoch())
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDelivered); !errors.Is(
		err, ErrControllerDownstreamPacketClaimNotAdmitted) {
		t.Fatalf("unadmitted delivered resolution = %v", err)
	}

	participant.setPreflightError(preflightFailure)
	if _, err := owner.Admit(claim); !errors.Is(err, preflightFailure) {
		t.Fatalf("final preflight error = %v", err)
	}
	snapshot, _ = owner.Snapshot()
	if !snapshot.Claimed || snapshot.Admitted || snapshot.ClaimToken != 1 {
		t.Fatalf("failed final preflight changed claim: %+v", snapshot)
	}
	participant.setPreflightError(nil)
	if _, err := owner.Admit(claim); err != nil {
		t.Fatal(err)
	}
}

func TestControllerDownstreamPacketExecutionImmutableRetry(t *testing.T) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	participant := &recordingDownstreamPacketParticipant{}
	participantFailure := errors.New("whole vector rejected without effect")
	participant.setExecuteError(participantFailure)
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Execute(lease, time.Now().Add(time.Second)); !errors.Is(
		err, participantFailure) {
		t.Fatalf("execute error = %v", err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDelivered); !errors.Is(
		err, ErrInvalidControllerDownstreamPacketOutcome) {
		t.Fatalf("false delivered resolution = %v", err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDeliveryFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Claim(packet); !errors.Is(
		err, ErrControllerDownstreamPacketRetryRequired) {
		t.Fatalf("successor packet error = %v", err)
	}

	retry, err := owner.ClaimRetry()
	if err != nil {
		t.Fatal(err)
	}
	if retry.PacketEpoch() != claim.PacketEpoch() || retry.token == claim.token {
		t.Fatalf("retry identity = old %+v new %+v", claim, retry)
	}
	retryLease, err := owner.Admit(retry)
	if err != nil {
		t.Fatal(err)
	}
	if retryLease.execution != lease.execution {
		t.Fatal("retry changed immutable packet vector")
	}
	participant.setExecuteError(nil)
	if err := owner.Execute(retryLease, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(retry, ControllerDownstreamPacketDelivered); err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDeliveryFailed); !errors.Is(
		err, ErrInvalidControllerDownstreamPacketClaim) {
		t.Fatalf("stale claim resolution = %v", err)
	}
}

func TestControllerDownstreamPacketExecutionCancelAndDrain(t *testing.T) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	participantFailure := errors.New("execution cancelled")
	participant := &recordingDownstreamPacketParticipant{
		executeErr:     participantFailure,
		executeStarted: make(chan struct{}), executeRelease: make(chan struct{}),
		executeExited: make(chan struct{}),
	}
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim)
	if err != nil {
		t.Fatal(err)
	}
	executeResult := make(chan error, 1)
	go func() {
		executeResult <- owner.Execute(lease, time.Now().Add(2*time.Second))
	}()
	select {
	case <-participant.executeStarted:
	case <-time.After(time.Second):
		t.Fatal("participant execution did not start")
	}
	if err := owner.CancelAndDrain(lease, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := <-executeResult; !errors.Is(err, participantFailure) {
		t.Fatalf("execute result = %v", err)
	}
	snapshot, _ := owner.Snapshot()
	if !snapshot.AwaitingResolution || !snapshot.Cancelled ||
		snapshot.ExecutionInFlight || snapshot.Quarantined {
		t.Fatalf("cancelled snapshot = %+v", snapshot)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketExecutionCancelled); err != nil {
		t.Fatal(err)
	}
	retry, err := owner.ClaimRetry()
	if err != nil {
		t.Fatal(err)
	}
	if retry.PacketEpoch() != claim.PacketEpoch() {
		t.Fatalf("cancelled retry epoch = %d, want %d",
			retry.PacketEpoch(), claim.PacketEpoch())
	}
}

func TestControllerDownstreamPacketExecutionDrainFailureQuarantines(t *testing.T) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	drainFailure := errors.New("drain could not prove containment")
	participant := &recordingDownstreamPacketParticipant{
		executeErr: errors.New("cancelled execution"), cancelErr: drainFailure,
		executeStarted: make(chan struct{}), executeRelease: make(chan struct{}),
		executeExited: make(chan struct{}),
	}
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim)
	if err != nil {
		t.Fatal(err)
	}
	executeResult := make(chan error, 1)
	go func() {
		executeResult <- owner.Execute(lease, time.Now().Add(2*time.Second))
	}()
	<-participant.executeStarted
	if err := owner.CancelAndDrain(lease, time.Now().Add(time.Second)); !errors.Is(err, ErrControllerDownstreamPacketQuarantined) ||
		!errors.Is(err, drainFailure) {
		t.Fatalf("drain error = %v", err)
	}
	<-executeResult
	snapshot, _ := owner.Snapshot()
	if !snapshot.Quarantined || snapshot.RetryPending || snapshot.Idle {
		t.Fatalf("quarantine snapshot = %+v", snapshot)
	}
	if _, err := owner.ClaimRetry(); !errors.Is(
		err, ErrControllerDownstreamPacketQuarantined) {
		t.Fatalf("quarantined retry error = %v", err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketExecutionCancelled); !errors.Is(err, ErrControllerDownstreamPacketQuarantined) {
		t.Fatalf("quarantined resolution error = %v", err)
	}
}

func TestControllerDownstreamPacketExecutionRechecksDeadlineAfterSerialization(
	t *testing.T,
) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	participant := &recordingDownstreamPacketParticipant{}
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the mutator serializer until the initially valid bound expires.
	<-owner.operation
	deadline := time.Now().Add(25 * time.Millisecond)
	result := make(chan error, 1)
	go func() { result <- owner.Execute(lease, deadline) }()
	waitPastControllerDownstreamPacketDeadline(deadline)
	owner.operation <- struct{}{}
	if err := <-result; !errors.Is(err, ErrControllerDownstreamPacketDeadline) {
		t.Fatalf("serialized execute error = %v", err)
	}
	_, executeCalls, _ := participant.counts()
	if executeCalls != 0 {
		t.Fatalf("expired serialized execute reached participant %d times", executeCalls)
	}
	snapshot, _ := owner.Snapshot()
	if !snapshot.Admitted || snapshot.ExecutionInFlight ||
		snapshot.AwaitingResolution || snapshot.DrainRequired {
		t.Fatalf("expired serialized execute changed state: %+v", snapshot)
	}

	if err := owner.Execute(lease, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDelivered); err != nil {
		t.Fatal(err)
	}
}

func TestControllerDownstreamPacketCancelRechecksDeadlineAfterSerialization(
	t *testing.T,
) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	participantFailure := errors.New("execution cancelled")
	participant := &recordingDownstreamPacketParticipant{
		executeErr:     participantFailure,
		executeStarted: make(chan struct{}), executeRelease: make(chan struct{}),
		executeExited: make(chan struct{}),
	}
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim)
	if err != nil {
		t.Fatal(err)
	}
	executeResult := make(chan error, 1)
	go func() {
		executeResult <- owner.Execute(lease, time.Now().Add(2*time.Second))
	}()
	<-participant.executeStarted

	<-owner.operation
	deadline := time.Now().Add(25 * time.Millisecond)
	cancelResult := make(chan error, 1)
	go func() { cancelResult <- owner.CancelAndDrain(lease, deadline) }()
	waitPastControllerDownstreamPacketDeadline(deadline)
	owner.operation <- struct{}{}
	if err := <-cancelResult; !errors.Is(err, ErrControllerDownstreamPacketDeadline) {
		t.Fatalf("serialized cancel error = %v", err)
	}
	_, _, cancelCalls := participant.counts()
	if cancelCalls != 0 {
		t.Fatalf("expired serialized cancel reached participant %d times", cancelCalls)
	}
	snapshot, _ := owner.Snapshot()
	if !snapshot.ExecutionInFlight || snapshot.DrainRequired || snapshot.Quarantined {
		t.Fatalf("expired serialized cancel changed state: %+v", snapshot)
	}

	if err := owner.CancelAndDrain(lease, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := <-executeResult; !errors.Is(err, participantFailure) {
		t.Fatalf("execute result = %v", err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketExecutionCancelled); err != nil {
		t.Fatal(err)
	}
}

func TestControllerDownstreamPacketLateSuccessfulExecuteRemainsDeliveredAfterDrain(
	t *testing.T,
) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	participant := &recordingDownstreamPacketParticipant{
		executeStarted: make(chan struct{}), executeRelease: make(chan struct{}),
		executeExited: make(chan struct{}),
	}
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(25 * time.Millisecond)
	executeResult := make(chan error, 1)
	go func() { executeResult <- owner.Execute(lease, deadline) }()
	<-participant.executeStarted
	waitPastControllerDownstreamPacketDeadline(deadline)
	participant.releaseOnce.Do(func() { close(participant.executeRelease) })
	if err := <-executeResult; !errors.Is(err, ErrControllerDownstreamPacketDeadline) ||
		!errors.Is(err, ErrControllerDownstreamPacketDrainRequired) {
		t.Fatalf("late successful execute error = %v", err)
	}
	snapshot, _ := owner.Snapshot()
	if !snapshot.DrainRequired || snapshot.Delivered || snapshot.Cancelled {
		t.Fatalf("pre-drain late snapshot = %+v", snapshot)
	}
	if err := owner.CancelAndDrain(lease, time.Now().Add(time.Second)); !errors.Is(
		err, ErrInvalidControllerDownstreamPacketOutcome) {
		t.Fatalf("late-success drain classification = %v", err)
	}
	snapshot, _ = owner.Snapshot()
	if !snapshot.AwaitingResolution || !snapshot.Delivered || snapshot.Cancelled ||
		snapshot.Quarantined {
		t.Fatalf("post-drain late snapshot = %+v", snapshot)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketExecutionCancelled); !errors.Is(err, ErrInvalidControllerDownstreamPacketOutcome) {
		t.Fatalf("late success relabeled cancelled: %v", err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDelivered); err != nil {
		t.Fatal(err)
	}
}

func TestControllerDownstreamPacketCommitThenPanicQuarantinesAfterDrain(
	t *testing.T,
) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	participant := &recordingDownstreamPacketParticipant{
		executePanic: "panic after atomic commit", applyThenPanic: true,
	}
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Execute(lease, time.Now().Add(time.Second)); !errors.Is(err, ErrControllerDownstreamPacketParticipantPanic) ||
		!errors.Is(err, ErrControllerDownstreamPacketDrainRequired) {
		t.Fatalf("post-commit panic error = %v", err)
	}
	participant.mu.Lock()
	applied := participant.applied
	participant.mu.Unlock()
	if applied != lease.execution {
		t.Fatal("test participant did not commit before panic")
	}
	if err := owner.CancelAndDrain(lease, time.Now().Add(time.Second)); !errors.Is(err, ErrControllerDownstreamPacketQuarantined) ||
		!errors.Is(err, ErrControllerDownstreamPacketParticipantPanic) {
		t.Fatalf("post-commit panic drain error = %v", err)
	}
	snapshot, _ := owner.Snapshot()
	if !snapshot.Quarantined || snapshot.RetryPending || snapshot.Cancelled ||
		snapshot.AwaitingResolution {
		t.Fatalf("post-commit panic snapshot = %+v", snapshot)
	}
	if _, err := owner.ClaimRetry(); !errors.Is(
		err, ErrControllerDownstreamPacketQuarantined) {
		t.Fatalf("post-commit panic retry error = %v", err)
	}
}

func TestControllerDownstreamPacketLateErrorQuarantinesAfterDrain(t *testing.T) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	participantFailure := errors.New("late outcome cannot prove no effect")
	participant := &recordingDownstreamPacketParticipant{
		executeErr:     participantFailure,
		executeStarted: make(chan struct{}), executeRelease: make(chan struct{}),
		executeExited: make(chan struct{}),
	}
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(25 * time.Millisecond)
	executeResult := make(chan error, 1)
	go func() { executeResult <- owner.Execute(lease, deadline) }()
	<-participant.executeStarted
	waitPastControllerDownstreamPacketDeadline(deadline)
	participant.releaseOnce.Do(func() { close(participant.executeRelease) })
	if err := <-executeResult; !errors.Is(err, participantFailure) ||
		!errors.Is(err, ErrControllerDownstreamPacketDeadline) ||
		!errors.Is(err, ErrControllerDownstreamPacketDrainRequired) {
		t.Fatalf("late execute error = %v", err)
	}
	if err := owner.CancelAndDrain(lease, time.Now().Add(time.Second)); !errors.Is(err, ErrControllerDownstreamPacketQuarantined) ||
		!errors.Is(err, participantFailure) {
		t.Fatalf("late error drain = %v", err)
	}
	snapshot, _ := owner.Snapshot()
	if !snapshot.Quarantined || snapshot.RetryPending || snapshot.Cancelled ||
		snapshot.AwaitingResolution {
		t.Fatalf("late error snapshot = %+v", snapshot)
	}
}

func waitPastControllerDownstreamPacketDeadline(deadline time.Time) {
	remaining := time.Until(deadline)
	if remaining > 0 {
		time.Sleep(remaining)
	}
	time.Sleep(10 * time.Millisecond)
}

func TestControllerDownstreamPacketExecutionPreflightPanicQuarantinesWithoutClaim(
	t *testing.T,
) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	participant := &recordingDownstreamPacketParticipant{
		preflightPanic: "bad preflight",
	}
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Claim(packet); !errors.Is(
		err, ErrControllerDownstreamPacketParticipantPanic) {
		t.Fatalf("panic error = %v", err)
	}
	snapshot, _ := owner.Snapshot()
	if !snapshot.Quarantined || snapshot.NextPacketEpoch != 0 ||
		snapshot.NextClaimToken != 0 || snapshot.ClaimToken != 0 {
		t.Fatalf("preflight panic snapshot = %+v", snapshot)
	}
}

func TestControllerDownstreamPacketExecutionConcurrentClaimSingleOwner(t *testing.T) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	participant := &recordingDownstreamPacketParticipant{}
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}
	const contenders = 24
	start := make(chan struct{})
	claims := make(chan ControllerDownstreamPacketClaim, contenders)
	errorsSeen := make(chan error, contenders)
	var wait sync.WaitGroup
	wait.Add(contenders)
	for index := 0; index < contenders; index++ {
		go func() {
			defer wait.Done()
			<-start
			claim, claimErr := owner.Claim(packet)
			if claimErr != nil {
				errorsSeen <- claimErr
				return
			}
			claims <- claim
		}()
	}
	close(start)
	wait.Wait()
	close(claims)
	close(errorsSeen)
	if len(claims) != 1 || len(errorsSeen) != contenders-1 {
		t.Fatalf("claim results = success %d errors %d", len(claims), len(errorsSeen))
	}
	claim := <-claims
	for claimErr := range errorsSeen {
		if !errors.Is(claimErr, ErrControllerDownstreamPacketExecutorBusy) {
			t.Fatalf("contender error = %v", claimErr)
		}
	}
	preflightCalls, _, _ := participant.counts()
	if preflightCalls != 1 {
		t.Fatalf("concurrent preflight calls = %d, want 1", preflightCalls)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDeferred); err != nil {
		t.Fatal(err)
	}
}

type allocationDownstreamPacketParticipant struct{}

func (*allocationDownstreamPacketParticipant) PreflightControllerDownstreamPacket(
	ControllerDownstreamPacketExecution,
) error {
	return nil
}

func (*allocationDownstreamPacketParticipant) ExecuteControllerDownstreamPacket(
	ControllerDownstreamPacketExecution,
	time.Time,
) error {
	return nil
}

func (*allocationDownstreamPacketParticipant) CancelControllerDownstreamPacketAndDrain(
	ControllerDownstreamPacketExecution,
	time.Time,
) error {
	return nil
}

func TestControllerDownstreamPacketExecutionAllocations(t *testing.T) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	owner, err := NewControllerDownstreamPacketExecutionOwner(
		&allocationDownstreamPacketParticipant{})
	if err != nil {
		t.Fatal(err)
	}
	run := func() {
		claim, claimErr := owner.Claim(packet)
		if claimErr != nil {
			panic(claimErr)
		}
		lease, admitErr := owner.Admit(claim)
		if admitErr != nil {
			panic(admitErr)
		}
		if executeErr := owner.Execute(lease, time.Now().Add(time.Hour)); executeErr != nil {
			panic(executeErr)
		}
		if resolveErr := owner.Resolve(
			claim, ControllerDownstreamPacketDelivered); resolveErr != nil {
			panic(resolveErr)
		}
	}
	run()
	allocations := testing.AllocsPerRun(1000, run)
	if allocations != 0 {
		t.Fatalf("downstream packet execution allocations = %v, want 0", allocations)
	}
}

func TestControllerDownstreamPacketExecutionConcurrentSnapshot(t *testing.T) {
	packet, _ := testAtomicControllerDownstreamPacket(t)
	participant := &recordingDownstreamPacketParticipant{}
	owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
	if err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var snapshots atomic.Uint64
	done := make(chan struct{})
	progress := make(chan struct{}, 1)
	go func() {
		defer close(done)
		for !stop.Load() {
			if _, ok := owner.Snapshot(); !ok {
				return
			}
			snapshots.Add(1)
			select {
			case progress <- struct{}{}:
			default:
			}
		}
	}()
	waitForSnapshotAfter := func(after uint64) bool {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for snapshots.Load() <= after {
			select {
			case <-progress:
			case <-timer.C:
				return false
			}
		}
		return true
	}
	if !waitForSnapshotAfter(0) {
		stop.Store(true)
		<-done
		t.Fatal("snapshot reader did not start")
	}
	for index := 0; index < 100; index++ {
		claim, claimErr := owner.Claim(packet)
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		lease, admitErr := owner.Admit(claim)
		if admitErr != nil {
			t.Fatal(admitErr)
		}
		if executeErr := owner.Execute(
			lease, time.Now().Add(time.Second)); executeErr != nil {
			t.Fatal(executeErr)
		}
		if resolveErr := owner.Resolve(
			claim, ControllerDownstreamPacketDelivered); resolveErr != nil {
			t.Fatal(resolveErr)
		}
		if (index+1)%25 == 0 {
			before := snapshots.Load()
			if !waitForSnapshotAfter(before) {
				stop.Store(true)
				<-done
				t.Fatalf("snapshot reader made no progress after mutation %d",
					index+1)
			}
		}
	}
	stop.Store(true)
	<-done
	if snapshots.Load() == 0 {
		t.Fatal("snapshot reader made no progress")
	}
}

package xboxone

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

func testPersonaHostPacket(
	t *testing.T,
	wires ...[]byte,
) ControllerDownstreamPacket {
	t.Helper()
	packet, err := DecodeControllerDownstreamPacket(bytes.Join(wires, nil))
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

func testDirectMotorWire(
	t *testing.T,
	sequence uint8,
	body RumbleBodyV1,
) []byte {
	t.Helper()
	wire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(wire, sequence, body); err != nil {
		t.Fatal(err)
	}
	return wire
}

func testGuideLEDWire(
	t *testing.T,
	sequence uint8,
	body GuideLEDCommandV1,
) []byte {
	t.Helper()
	wire := make([]byte, GuideLEDCommandMessageSize)
	if err := EncodeGuideLEDCommandMessageInto(wire, sequence, body); err != nil {
		t.Fatal(err)
	}
	return wire
}

func testSetDeviceStateWire(sequence uint8, state SetDeviceStateValue) []byte {
	return []byte{messageNumberSetDeviceState, flagSystem, sequence, 1, byte(state)}
}

func TestControllerPersonaDownstreamPacketBatchLifecycleAndFeedbackOrder(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, []byte{0xaa, 0xbb}, 0)
	configurePersonaUSB(t, engine, 0)
	hello, present, err := engine.ClaimPoll(0)
	if err != nil || !present {
		t.Fatalf("Hello = (%+v, %t, %v)", hello, present, err)
	}
	deliverPersonaClaim(t, engine, hello, 1)

	motor := RumbleBodyV1{
		Enabled:       MotorLeftVibration | MotorRightImpulse,
		LeftVibration: 31, RightImpulse: 47, Duration: 8,
	}
	led := GuideLEDCommandV1{Pattern: GuideLEDPatternOn, Intensity: 17}
	packet := testPersonaHostPacket(t,
		testDirectMotorWire(t, 0x31, motor),
		testSetDeviceStateWire(0x32, SetDeviceStateStart),
		testGuideLEDWire(t, 0x33, led))
	participant := &recordingDownstreamPacketParticipant{}
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(engine, participant)
	if err != nil {
		t.Fatal(err)
	}

	claim, err := owner.Claim(packet, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !claim.Valid() || claim.PacketEpoch() != 1 || claim.MessageCount() != 3 {
		t.Fatalf("claim = %+v", claim)
	}
	before := engine.Snapshot()
	if before.LifecycleState != ControllerLifecycleArrival ||
		before.Feedback.DirectMotor != (RumbleBodyV1{}) ||
		before.Feedback.GuideLED.Pattern != GuideLEDPatternOff {
		t.Fatalf("claim leaked an effect: %+v", before)
	}

	lease, err := owner.Admit(claim, 3)
	if err != nil {
		t.Fatal(err)
	}
	wantActions := []ControllerPersonaAction{
		ControllerPersonaApplyDirectMotor,
		ControllerPersonaSendCurrentStatus,
		ControllerPersonaApplyGuideLED,
	}
	for index, want := range wantActions {
		action, ok := lease.Action(index)
		if !ok || action.Action != want || action.WireIndex != uint8(index) {
			t.Fatalf("action %d = (%+v, %t), want %d", index, action, ok, want)
		}
		if action.Sequence != uint8(0x31+index) {
			t.Fatalf("action %d host sequence = %d", index, action.Sequence)
		}
	}
	statusAction, _ := lease.Action(1)
	if statusAction.OutputSequence == 0 ||
		statusAction.WireSize() != ExtendedStatusNoEventsMessageSize {
		t.Fatalf("status action = %+v", statusAction)
	}
	statusWire := make([]byte, statusAction.WireSize())
	if err := statusAction.CopyWire(statusWire); err != nil {
		t.Fatal(err)
	}
	outputSequence, _, err := DecodeExtendedStatusNoEventsMessage(statusWire)
	if err != nil {
		t.Fatalf("status wire: %v", err)
	}
	if outputSequence != statusAction.OutputSequence || outputSequence == statusAction.Sequence {
		t.Fatalf("status sequence = %d, action=%+v", outputSequence, statusAction)
	}
	if err := owner.Execute(lease, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDelivered, 4); err != nil {
		t.Fatal(err)
	}
	after := engine.Snapshot()
	if after.Feedback.DirectMotor != motor || after.Feedback.GuideLED != led ||
		after.LifecycleState != ControllerLifecycleArrival {
		t.Fatalf("delivered snapshot = %+v", after)
	}
	next, err := engine.ClaimNextLifecycleAction(5)
	if err != nil || next.Action() != ControllerPersonaSendInitialInput {
		t.Fatalf("next lifecycle = (%+v, %v)", next, err)
	}

	participant.mu.Lock()
	applied := participant.applied
	participant.mu.Unlock()
	for index, want := range wantActions {
		action, ok := applied.Action(index)
		if !ok || action.Action != want {
			t.Fatalf("applied action %d = (%+v, %t)", index, action, ok)
		}
	}
}

func TestControllerPersonaDownstreamPacketBatchClearPreservesWireOrder(
	t *testing.T,
) {
	engine, nowMS := makePersonaActive(t, []byte{0xaa})
	first := RumbleBodyV1{Enabled: MotorLeftVibration, LeftVibration: 20, Duration: 1}
	last := RumbleBodyV1{Enabled: MotorRightVibration, RightVibration: 80, Duration: 2}
	packet := testPersonaHostPacket(t,
		testDirectMotorWire(t, 0x41, first),
		testSetDeviceStateWire(0x42, SetDeviceStateQuiesce),
		testDirectMotorWire(t, 0x43, last))
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(
		engine, &recordingDownstreamPacketParticipant{})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet, nowMS+1)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim, nowMS+2)
	if err != nil {
		t.Fatal(err)
	}
	clear, _ := lease.Action(1)
	if clear.Action != ControllerPersonaClearOutputs || clear.ClearEpoch == 0 {
		t.Fatalf("clear action = %+v", clear)
	}
	if err := owner.Execute(lease, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDelivered, nowMS+3); err != nil {
		t.Fatal(err)
	}
	snapshot := engine.Snapshot()
	if snapshot.Feedback.DirectMotor != last ||
		snapshot.Feedback.GuideLED.Pattern != GuideLEDPatternOff ||
		snapshot.Feedback.ClearEpoch != clear.ClearEpoch {
		t.Fatalf("ordered feedback = %+v", snapshot.Feedback)
	}
}

func TestControllerPersonaDownstreamPacketBatchPreservesFullPowerNoOpOrder(
	t *testing.T,
) {
	engine, nowMS := makePersonaActive(t, []byte{0xaa})
	motor := RumbleBodyV1{
		Enabled: MotorRightVibration, RightVibration: 33, Duration: 1,
	}
	packet := testPersonaHostPacket(t,
		testSetDeviceStateWire(0x44, SetDeviceStateFullPower),
		testDirectMotorWire(t, 0x45, motor))
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(
		engine, &recordingDownstreamPacketParticipant{})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet, nowMS+1)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim, nowMS+2)
	if err != nil {
		t.Fatal(err)
	}
	ignored, _ := lease.Action(0)
	if ignored.Action != ControllerPersonaIgnoreHostMessage ||
		ignored.WireIndex != 0 || ignored.Sequence != 0x44 {
		t.Fatalf("ignored action = %+v", ignored)
	}
	if err := owner.Execute(lease, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(
		claim, ControllerDownstreamPacketDelivered, nowMS+3); err != nil {
		t.Fatal(err)
	}
	if got := engine.Snapshot(); got.LifecycleState != ControllerLifecycleActive ||
		got.Feedback.DirectMotor != motor {
		t.Fatalf("FULL POWER packet result = %+v", got)
	}
}

func TestControllerPersonaDownstreamPacketBatchACKUsesCanonicalTransferIdentity(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, bytes.Repeat([]byte{0xaa}, 70), 0)
	configurePersonaUSB(t, engine, 0)
	hello, _, _ := engine.ClaimPoll(0)
	deliverPersonaClaim(t, engine, hello, 1)
	begin := claimPersonaHostAction(t, engine, 2,
		[]byte{messageNumberMetadataRequest, flagSystem, 1, 0})
	deliverPersonaClaim(t, engine, begin, 3)
	metadata, err := engine.ClaimMetadataPacket(4)
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, metadata, 5)
	identity, ok := engine.metadataTransfer.PendingAcknowledgement()
	if !ok {
		t.Fatal("metadata transfer has no pending ACK identity")
	}
	body := ProtocolControlACKBodyV1{
		ReferencedDataClass:     DataClassCommand,
		ReferencedMessageNumber: messageNumberMetadataRequest,
		ReferencedSystem:        true,
		FragmentOffset:          uint32(identity.ContiguousPayloadBytes),
		RemainingBuffer:         0x1234,
	}
	ackWire := make([]byte, ProtocolControlACKMessageSize)
	if err := EncodeProtocolControlACKMessageInto(ackWire, identity.Sequence, body); err != nil {
		t.Fatal(err)
	}
	motor := RumbleBodyV1{Enabled: MotorLeftImpulse, LeftImpulse: 25, Duration: 4}
	packet := testPersonaHostPacket(t, ackWire, testDirectMotorWire(t, 0x55, motor))
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(
		engine, &recordingDownstreamPacketParticipant{})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet, 6)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim, 7)
	if err != nil {
		t.Fatal(err)
	}
	ackAction, _ := lease.Action(0)
	if ackAction.Action != ControllerPersonaApplyMetadataAcknowledgement ||
		ackAction.ReliableACKDisposition != ReliableAcknowledgementProgress {
		t.Fatalf("ACK action = %+v", ackAction)
	}
	if err := owner.Execute(lease, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDelivered, 8); err != nil {
		t.Fatal(err)
	}
	if got := engine.metadataTransfer.Snapshot(); got.AcknowledgedEnd != identity.ContiguousPayloadBytes ||
		got.AwaitingAcknowledgement {
		t.Fatalf("metadata after ACK = %+v", got)
	}
	if got := engine.Snapshot().Feedback.DirectMotor; got != motor {
		t.Fatalf("motor = %+v", got)
	}
}

func TestControllerPersonaDownstreamPacketBatchACKRetryExpiryQuarantines(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, bytes.Repeat([]byte{0xaa}, 70), 0)
	configurePersonaUSB(t, engine, 0)
	hello, _, _ := engine.ClaimPoll(0)
	deliverPersonaClaim(t, engine, hello, 1)
	begin := claimPersonaHostAction(t, engine, 2,
		[]byte{messageNumberMetadataRequest, flagSystem, 1, 0})
	deliverPersonaClaim(t, engine, begin, 3)
	metadata, err := engine.ClaimMetadataPacket(4)
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, metadata, 5)
	identity, ok := engine.metadataTransfer.PendingAcknowledgement()
	if !ok {
		t.Fatal("metadata transfer has no pending ACK identity")
	}
	body := ProtocolControlACKBodyV1{
		ReferencedDataClass:     DataClassCommand,
		ReferencedMessageNumber: messageNumberMetadataRequest,
		ReferencedSystem:        true,
		FragmentOffset:          uint32(identity.ContiguousPayloadBytes),
	}
	ackWire := make([]byte, ProtocolControlACKMessageSize)
	if err := EncodeProtocolControlACKMessageInto(ackWire, identity.Sequence, body); err != nil {
		t.Fatal(err)
	}
	packet := testPersonaHostPacket(t, ackWire, testDirectMotorWire(t, 0x65,
		RumbleBodyV1{Enabled: MotorLeftImpulse, LeftImpulse: 30, Duration: 2}))
	participant := &recordingDownstreamPacketParticipant{executeErr: errors.New("no effect")}
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(engine, participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet, 6)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Execute(lease, time.Now().Add(time.Second)); err == nil {
		t.Fatal("Execute succeeded unexpectedly")
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDeliveryFailed, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ClaimRetry(1005); !errors.Is(
		err, ErrControllerPersonaHostPacketQuarantined) ||
		!errors.Is(err, ErrReliableTransferTimeout) {
		t.Fatalf("expired retry error = %v", err)
	}
	snapshot, _ := owner.Snapshot()
	if !snapshot.Quarantined || snapshot.RetryPending {
		t.Fatalf("expired retry snapshot = %+v", snapshot)
	}
	host, _ := engine.hostPacketSnapshot()
	if host.RetryPending || host.Active {
		t.Fatalf("expired host retry = %+v", host)
	}
	if got := engine.Snapshot().Feedback.DirectMotor; got != (RumbleBodyV1{}) {
		t.Fatalf("expired packet committed motor = %+v", got)
	}
}

func TestControllerPersonaDownstreamPacketBatchContextLimitIsFailureAtomic(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, []byte{0xaa}, 0)
	configurePersonaUSB(t, engine, 0)
	hello, _, _ := engine.ClaimPoll(0)
	deliverPersonaClaim(t, engine, hello, 1)
	packet := testPersonaHostPacket(t,
		testSetDeviceStateWire(0x61, SetDeviceStateStart),
		testSetDeviceStateWire(0x62, SetDeviceStateReset))
	ackWire := make([]byte, ProtocolControlACKMessageSize)
	if err := EncodeProtocolControlACKMessageInto(ackWire, 0x63,
		ProtocolControlACKBodyV1{
			ReferencedDataClass:     DataClassCommand,
			ReferencedMessageNumber: messageNumberMetadataRequest,
			ReferencedSystem:        true,
		}); err != nil {
		t.Fatal(err)
	}
	mixedContextPacket := testPersonaHostPacket(t,
		testSetDeviceStateWire(0x64, SetDeviceStateStart), ackWire)
	participant := &recordingDownstreamPacketParticipant{}
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(engine, participant)
	if err != nil {
		t.Fatal(err)
	}
	before := engine.Snapshot()
	for name, rejected := range map[string]ControllerDownstreamPacket{
		"two lifecycle":     packet,
		"lifecycle and ACK": mixedContextPacket,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := owner.Claim(rejected, 2); !errors.Is(
				err, ErrControllerPersonaHostPacketContextLimit) {
				t.Fatalf("context-limit error = %v", err)
			}
		})
	}
	after := engine.Snapshot()
	if after != before {
		t.Fatalf("context-limit mutation: before=%+v after=%+v", before, after)
	}
	preflight, execute, cancel := participant.counts()
	if preflight != 0 || execute != 0 || cancel != 0 {
		t.Fatalf("participant calls = %d %d %d", preflight, execute, cancel)
	}
	snapshot, _ := owner.Snapshot()
	if !snapshot.Idle || snapshot.PacketEpoch != 0 {
		t.Fatalf("owner snapshot = %+v", snapshot)
	}
}

func TestControllerPersonaDownstreamPacketBatchDelayedClearIsFailureAtomic(
	t *testing.T,
) {
	states := []struct {
		name  string
		state SetDeviceStateValue
	}{
		{name: "stop", state: SetDeviceStateStop},
		{name: "off", state: SetDeviceStateOff},
		{name: "reset", state: SetDeviceStateReset},
	}
	orders := []struct {
		name           string
		lifecycleFirst bool
	}{
		{name: "lifecycle then feedback", lifecycleFirst: true},
		{name: "feedback then lifecycle", lifecycleFirst: false},
	}
	for _, state := range states {
		for _, order := range orders {
			t.Run(state.name+"/"+order.name, func(t *testing.T) {
				engine, nowMS := makePersonaActive(t, []byte{0xaa})
				participant := &recordingDownstreamPacketParticipant{}
				owner, err := NewControllerPersonaDownstreamPacketBatchOwner(
					engine, participant)
				if err != nil {
					t.Fatal(err)
				}
				lifecycle := testSetDeviceStateWire(0x65, state.state)
				feedback := testDirectMotorWire(t, 0x66, RumbleBodyV1{
					Enabled: MotorLeftVibration, LeftVibration: 50, Duration: 4,
				})
				wires := [][]byte{feedback, lifecycle}
				if order.lifecycleFirst {
					wires = [][]byte{lifecycle, feedback}
				}
				packet := testPersonaHostPacket(t, wires...)
				before := engine.Snapshot()

				if _, err := owner.Claim(packet, nowMS+1); !errors.Is(
					err, ErrControllerPersonaHostPacketContextLimit) {
					t.Fatalf("delayed-clear context-limit error = %v", err)
				}
				if after := engine.Snapshot(); after != before {
					t.Fatalf("delayed-clear rejection mutated engine: before=%+v after=%+v",
						before, after)
				}
				preflight, execute, cancel := participant.counts()
				if preflight != 0 || execute != 0 || cancel != 0 {
					t.Fatalf("participant calls = %d %d %d", preflight, execute, cancel)
				}
				if snapshot, ok := owner.Snapshot(); !ok || !snapshot.Idle ||
					snapshot.PacketEpoch != 0 {
					t.Fatalf("owner snapshot = (%+v, %t)", snapshot, ok)
				}
			})
		}
	}
}

func TestControllerPersonaDownstreamPacketBatchRetryRetainsExactVector(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, []byte{0xaa}, 0)
	configurePersonaUSB(t, engine, 0)
	hello, _, _ := engine.ClaimPoll(0)
	deliverPersonaClaim(t, engine, hello, 1)
	motor := RumbleBodyV1{Enabled: MotorRightImpulse, RightImpulse: 45, Duration: 3}
	packet := testPersonaHostPacket(t,
		testSetDeviceStateWire(0x71, SetDeviceStateStart),
		testDirectMotorWire(t, 0x72, motor))
	participant := &recordingDownstreamPacketParticipant{executeErr: errors.New("no effect")}
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(engine, participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet, 2)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim, 3)
	if err != nil {
		t.Fatal(err)
	}
	firstExecution := lease.execution.execution
	if err := owner.Execute(lease, time.Now().Add(time.Second)); err == nil {
		t.Fatal("Execute succeeded unexpectedly")
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDeliveryFailed, 4); err != nil {
		t.Fatal(err)
	}
	if got := engine.Snapshot(); got.LifecycleState != ControllerLifecycleArrival ||
		got.Feedback.DirectMotor != (RumbleBodyV1{}) {
		t.Fatalf("failed attempt committed prefix: %+v", got)
	}

	participant.setExecuteError(nil)
	retry, err := owner.ClaimRetry(5)
	if err != nil {
		t.Fatal(err)
	}
	if retry.PacketEpoch() != claim.PacketEpoch() || retry.token == claim.token {
		t.Fatalf("retry identity = old %+v new %+v", claim, retry)
	}
	retryLease, err := owner.Admit(retry, 6)
	if err != nil {
		t.Fatal(err)
	}
	if retryLease.execution.execution != firstExecution {
		t.Fatal("retry changed immutable execution vector")
	}
	if err := owner.Execute(retryLease, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(retry, ControllerDownstreamPacketDelivered, 7); err != nil {
		t.Fatal(err)
	}
	if got := engine.Snapshot().Feedback.DirectMotor; got != motor {
		t.Fatalf("delivered retry motor = %+v", got)
	}
	if _, err := owner.Admit(claim, 8); !errors.Is(
		err, ErrInvalidControllerPersonaHostPacketClaim) {
		t.Fatalf("stale claim error = %v", err)
	}
}

func TestControllerPersonaDownstreamPacketBatchInitialPreflightBecomesExactRetry(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, []byte{0xaa}, 0)
	configurePersonaUSB(t, engine, 0)
	hello, _, _ := engine.ClaimPoll(0)
	deliverPersonaClaim(t, engine, hello, 1)
	packet := testPersonaHostPacket(t,
		testSetDeviceStateWire(0x21, SetDeviceStateStart),
		testGuideLEDWire(t, 0x22, GuideLEDCommandV1{Pattern: GuideLEDPatternOn}))
	preflightErr := errors.New("whole vector unavailable")
	participant := &recordingDownstreamPacketParticipant{preflightErr: preflightErr}
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(engine, participant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Claim(packet, 2); !errors.Is(err, preflightErr) {
		t.Fatalf("Claim error = %v", err)
	}
	snapshot, _ := owner.Snapshot()
	if !snapshot.RetryPending || snapshot.PacketEpoch != 1 {
		t.Fatalf("retry snapshot = %+v", snapshot)
	}
	if host, _ := engine.hostPacketSnapshot(); !host.RetryPending || host.PacketEpoch != 1 {
		t.Fatalf("host retry snapshot = %+v", host)
	}
	participant.setPreflightError(nil)
	retry, err := owner.ClaimRetry(3)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(retry, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Execute(lease, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(retry, ControllerDownstreamPacketDelivered, 5); err != nil {
		t.Fatal(err)
	}
}

func TestControllerPersonaDownstreamPacketBatchCancellationAndDrainRetainsRetry(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, []byte{0xaa}, 0)
	configurePersonaUSB(t, engine, 0)
	motor := RumbleBodyV1{Enabled: MotorLeftVibration, LeftVibration: 50, Duration: 2}
	packet := testPersonaHostPacket(t, testDirectMotorWire(t, 0x31, motor))
	participant := &recordingDownstreamPacketParticipant{
		executeErr:     errors.New("cancelled before effect"),
		executeStarted: make(chan struct{}), executeRelease: make(chan struct{}),
		executeExited: make(chan struct{}),
	}
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(engine, participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet, 1)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim, 2)
	if err != nil {
		t.Fatal(err)
	}
	executeResult := make(chan error, 1)
	go func() {
		executeResult <- owner.Execute(lease, time.Now().Add(time.Second))
	}()
	<-participant.executeStarted
	if err := owner.CancelAndDrain(lease, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := <-executeResult; err == nil {
		t.Fatal("cancelled Execute returned nil")
	}
	if err := owner.Resolve(
		claim, ControllerDownstreamPacketExecutionCancelled, 3); err != nil {
		t.Fatal(err)
	}
	if got := engine.Snapshot().Feedback.DirectMotor; got != (RumbleBodyV1{}) {
		t.Fatalf("cancel committed motor = %+v", got)
	}
	snapshot, _ := owner.Snapshot()
	if !snapshot.RetryPending || snapshot.Quarantined {
		t.Fatalf("cancel snapshot = %+v", snapshot)
	}
}

func TestControllerPersonaDownstreamPacketBatchConcurrentClaimSerializes(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, []byte{0xaa}, 0)
	configurePersonaUSB(t, engine, 0)
	packet := testPersonaHostPacket(t, testDirectMotorWire(t, 1,
		RumbleBodyV1{Enabled: MotorLeftVibration, LeftVibration: 1, Duration: 1}))
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(
		engine, &recordingDownstreamPacketParticipant{})
	if err != nil {
		t.Fatal(err)
	}
	const callers = 16
	var wait sync.WaitGroup
	wait.Add(callers)
	results := make(chan error, callers)
	claims := make(chan ControllerPersonaDownstreamPacketBatchClaim, callers)
	for index := 0; index < callers; index++ {
		go func() {
			defer wait.Done()
			claim, err := owner.Claim(packet, 1)
			if err == nil {
				claims <- claim
			}
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	close(claims)
	successes := 0
	busy := 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrControllerPersonaHostPacketOwnerBusy) {
			busy++
		} else {
			t.Fatalf("unexpected Claim error = %v", err)
		}
	}
	if successes != 1 || busy != callers-1 {
		t.Fatalf("results = success %d busy %d", successes, busy)
	}
	claim := <-claims
	if err := owner.Resolve(claim, ControllerDownstreamPacketDeferred, 1); err != nil {
		t.Fatal(err)
	}
}

func TestControllerPersonaDownstreamPacketBatchZeroAndCopiedOwnerFailFast(
	t *testing.T,
) {
	zero := &ControllerPersonaDownstreamPacketBatchOwner{}
	deadline := time.Now().Add(time.Second)
	operations := map[string]func() error{
		"claim": func() error {
			_, err := zero.Claim(ControllerDownstreamPacket{}, 0)
			return err
		},
		"claim retry": func() error {
			_, err := zero.ClaimRetry(0)
			return err
		},
		"admit": func() error {
			_, err := zero.Admit(ControllerPersonaDownstreamPacketBatchClaim{}, 0)
			return err
		},
		"execute": func() error {
			return zero.Execute(ControllerPersonaDownstreamPacketBatchLease{}, deadline)
		},
		"cancel": func() error {
			return zero.CancelAndDrain(
				ControllerPersonaDownstreamPacketBatchLease{}, deadline)
		},
		"resolve": func() error {
			return zero.Resolve(ControllerPersonaDownstreamPacketBatchClaim{},
				ControllerDownstreamPacketDeferred, 0)
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			result := make(chan error, 1)
			go func() { result <- operation() }()
			select {
			case err := <-result:
				if !errors.Is(err, ErrControllerPersonaHostPacketOwnerUninitialized) {
					t.Fatalf("error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("zero-value operation blocked")
			}
		})
	}
	if snapshot, ok := zero.Snapshot(); ok ||
		snapshot != (ControllerPersonaDownstreamPacketBatchSnapshot{}) {
		t.Fatalf("zero snapshot = (%+v, %t)", snapshot, ok)
	}

	engine := newTestControllerPersonaEngine(t, []byte{0xaa}, 0)
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(
		engine, &recordingDownstreamPacketParticipant{})
	if err != nil {
		t.Fatal(err)
	}
	copied := &ControllerPersonaDownstreamPacketBatchOwner{
		self: owner, operation: owner.operation, engine: owner.engine,
		executor: owner.executor, state: owner.state,
	}
	if _, err := copied.Claim(ControllerDownstreamPacket{}, 0); !errors.Is(
		err, ErrControllerPersonaHostPacketOwnerUninitialized) {
		t.Fatalf("copied Claim error = %v", err)
	}
}

func TestControllerPersonaDownstreamPacketBatchAmbiguousEffectQuarantines(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, []byte{0xaa}, 0)
	configurePersonaUSB(t, engine, 0)
	packet := testPersonaHostPacket(t, testDirectMotorWire(t, 1,
		RumbleBodyV1{Enabled: MotorLeftVibration, LeftVibration: 9, Duration: 1}))
	participant := &recordingDownstreamPacketParticipant{
		executePanic: "commit then panic", applyThenPanic: true,
	}
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(engine, participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet, 1)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Execute(lease, time.Now().Add(time.Second)); !errors.Is(
		err, ErrControllerDownstreamPacketDrainRequired) {
		t.Fatalf("Execute error = %v", err)
	}
	if err := owner.CancelAndDrain(lease, time.Now().Add(time.Second)); !errors.Is(
		err, ErrControllerDownstreamPacketQuarantined) {
		t.Fatalf("CancelAndDrain error = %v", err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDelivered, 3); !errors.Is(
		err, ErrControllerPersonaHostPacketQuarantined) {
		t.Fatalf("Resolve error = %v", err)
	}
	snapshot, _ := owner.Snapshot()
	if !snapshot.Quarantined || snapshot.RetryPending {
		t.Fatalf("ambiguous snapshot = %+v", snapshot)
	}
	if got := engine.Snapshot().Feedback.DirectMotor; got != (RumbleBodyV1{}) {
		t.Fatalf("canonical state guessed ambiguous delivery = %+v", got)
	}
}

func TestControllerPersonaDownstreamPacketBatchDeferredResolutionWinsExecuteRace(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, []byte{0xaa}, 0)
	configurePersonaUSB(t, engine, 0)
	packet := testPersonaHostPacket(t, testDirectMotorWire(t, 1,
		RumbleBodyV1{Enabled: MotorLeftVibration, LeftVibration: 12, Duration: 1}))
	participant := &recordingDownstreamPacketParticipant{}
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(engine, participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet, 1)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDeferred, 2); err != nil {
		t.Fatal(err)
	}
	if err := owner.Execute(lease, time.Now().Add(time.Second)); err == nil {
		t.Fatal("stale admitted lease executed after prepared Deferred resolution")
	}
	_, execute, cancel := participant.counts()
	if execute != 0 || cancel != 0 {
		t.Fatalf("stale lease reached participant: execute=%d cancel=%d", execute, cancel)
	}
	if got := engine.Snapshot().Feedback.DirectMotor; got != (RumbleBodyV1{}) {
		t.Fatalf("Deferred resolution committed motor = %+v", got)
	}
}

func TestControllerPersonaDownstreamPacketBatchExecuteWinsDeferredRace(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, []byte{0xaa}, 0)
	configurePersonaUSB(t, engine, 0)
	motor := RumbleBodyV1{
		Enabled: MotorLeftVibration, LeftVibration: 13, Duration: 1,
	}
	packet := testPersonaHostPacket(t, testDirectMotorWire(t, 1, motor))
	participant := &recordingDownstreamPacketParticipant{
		executeStarted: make(chan struct{}), executeRelease: make(chan struct{}),
		executeExited: make(chan struct{}),
	}
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(engine, participant)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := owner.Claim(packet, 1)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.Admit(claim, 2)
	if err != nil {
		t.Fatal(err)
	}
	executeResult := make(chan error, 1)
	go func() {
		executeResult <- owner.Execute(lease, time.Now().Add(time.Second))
	}()
	<-participant.executeStarted
	if err := owner.Resolve(claim, ControllerDownstreamPacketDeferred, 2); !errors.Is(
		err, ErrInvalidControllerDownstreamPacketOutcome) {
		t.Fatalf("in-flight Deferred error = %v", err)
	}
	participant.releaseOnce.Do(func() { close(participant.executeRelease) })
	if err := <-executeResult; err != nil {
		t.Fatal(err)
	}
	if err := owner.Resolve(claim, ControllerDownstreamPacketDelivered, 3); err != nil {
		t.Fatal(err)
	}
	if got := engine.Snapshot().Feedback.DirectMotor; got != motor {
		t.Fatalf("delivered motor = %+v", got)
	}
}

func TestControllerPersonaHostPacketInvariantQuarantineRetainsClaim(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, []byte{0xaa}, 0)
	configurePersonaUSB(t, engine, 0)
	claim := engine.makeClaim(controllerPersonaRecord{
		action: ControllerPersonaIgnoreHostMessage, selectedAtMS: 1,
	})
	if !claim.Valid() {
		t.Fatal("synthetic persona claim is invalid")
	}
	engine.quarantineClaimedHostPacket(ErrControllerPersonaInvariantViolation)
	host, ok := engine.hostPacketSnapshot()
	if !ok || !host.Quarantined || !engine.hasClaim {
		t.Fatalf("quarantine did not retain exact claim: host=%+v engine=%+v",
			host, engine.Snapshot())
	}
	packet := testPersonaHostPacket(t, testDirectMotorWire(t, 1,
		RumbleBodyV1{Enabled: MotorLeftVibration, LeftVibration: 1, Duration: 1}))
	if _, _, err := engine.claimHostPacket(2, packet); !errors.Is(
		err, ErrControllerPersonaHostPacketQuarantined) {
		t.Fatalf("successor claim error = %v", err)
	}
	if _, err := NewControllerPersonaDownstreamPacketBatchOwner(
		engine, &recordingDownstreamPacketParticipant{}); !errors.Is(
		err, ErrControllerPersonaHostPacketOwnerBusy) {
		t.Fatalf("owner construction error = %v", err)
	}
}

func TestControllerPersonaDownstreamPacketBatchSuccessfulPathAllocations(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, []byte{0xaa}, 0)
	configurePersonaUSB(t, engine, 0)
	packet := testPersonaHostPacket(t,
		testDirectMotorWire(t, 1, RumbleBodyV1{
			Enabled: MotorLeftVibration, LeftVibration: 10, Duration: 1,
		}),
		testGuideLEDWire(t, 2, GuideLEDCommandV1{Pattern: GuideLEDPatternOff}))
	owner, err := NewControllerPersonaDownstreamPacketBatchOwner(
		engine, &recordingDownstreamPacketParticipant{})
	if err != nil {
		t.Fatal(err)
	}
	nowMS := uint64(1)
	run := func() {
		claim, claimErr := owner.Claim(packet, nowMS)
		if claimErr != nil {
			panic(claimErr)
		}
		lease, admitErr := owner.Admit(claim, nowMS)
		if admitErr != nil {
			panic(admitErr)
		}
		if executeErr := owner.Execute(lease, time.Now().Add(time.Second)); executeErr != nil {
			panic(executeErr)
		}
		if resolveErr := owner.Resolve(
			claim, ControllerDownstreamPacketDelivered, nowMS); resolveErr != nil {
			panic(resolveErr)
		}
		nowMS++
	}
	run()
	if allocs := testing.AllocsPerRun(100, run); allocs != 0 {
		t.Fatalf("successful batch path allocations = %v, want 0", allocs)
	}
}

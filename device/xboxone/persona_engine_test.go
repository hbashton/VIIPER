package xboxone

import (
	"errors"
	"testing"
)

func newTestControllerPersonaEngine(
	t *testing.T,
	metadataBlob []byte,
	nowMS uint64,
) *ControllerPersonaEngine {
	t.Helper()
	profile := testOnlySyntheticControllerProfile(t)
	metadata, err := profile.BindExternallyCompiledMetadata(metadataBlob)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewControllerPersonaEngine(ControllerPersonaConfig{
		Profile: profile, Metadata: metadata,
		CurrentInput:      GamepadInputReportV1{},
		CurrentStatus:     NewWiredNoBatteryStatus(false),
		PoweringOffStatus: NewWiredNoBatteryStatus(true),
	}, nowMS)
	if err != nil {
		t.Fatal(err)
	}
	return &engine
}

func deliverPersonaClaim(
	t *testing.T,
	engine *ControllerPersonaEngine,
	claim ControllerPersonaClaim,
	nowMS uint64,
) []byte {
	t.Helper()
	wire := make([]byte, claim.Size())
	if err := engine.AdmitAndCopy(claim, wire, nowMS); err != nil {
		t.Fatalf("AdmitAndCopy action %d: %v", claim.Action(), err)
	}
	if err := engine.Resolve(claim, ControllerPersonaDelivered, nowMS); err != nil {
		t.Fatalf("Resolve action %d: %v", claim.Action(), err)
	}
	return wire
}

func configurePersonaUSB(
	t *testing.T,
	engine *ControllerPersonaEngine,
	nowMS uint64,
) {
	t.Helper()
	for _, setup := range [][]byte{
		testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetAddress, 1, 0, 0),
		testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration,
			uint16(usbConfigurationGIP), 0, 0),
	} {
		claim, err := engine.ClaimUSBControl(setup)
		if err != nil {
			t.Fatal(err)
		}
		deliverPersonaClaim(t, engine, claim, nowMS)
	}
}

func claimPersonaHostAction(
	t *testing.T,
	engine *ControllerPersonaEngine,
	nowMS uint64,
	wire []byte,
) ControllerPersonaClaim {
	t.Helper()
	claim, disposition, err := engine.ReceiveHostMessage(nowMS, wire)
	if err != nil {
		t.Fatal(err)
	}
	if disposition != ControllerPersonaHostActionClaimed || !claim.Valid() {
		t.Fatalf("host result = disposition %d claim %+v", disposition, claim)
	}
	return claim
}

func makePersonaActive(
	t *testing.T,
	metadataBlob []byte,
) (*ControllerPersonaEngine, uint64) {
	t.Helper()
	engine := newTestControllerPersonaEngine(t, metadataBlob, 0)
	configurePersonaUSB(t, engine, 0)
	hello, present, err := engine.ClaimPoll(0)
	if err != nil || !present || hello.Action() != ControllerPersonaSendHello {
		t.Fatalf("Hello = (%+v, %t, %v)", hello, present, err)
	}
	deliverPersonaClaim(t, engine, hello, 1)

	start := claimPersonaHostAction(t, engine, 2,
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
	if start.Action() != ControllerPersonaSendCurrentStatus {
		t.Fatalf("START first action = %d", start.Action())
	}
	deliverPersonaClaim(t, engine, start, 3)
	initial, err := engine.ClaimNextLifecycleAction(4)
	if err != nil || initial.Action() != ControllerPersonaSendInitialInput {
		t.Fatalf("initial input = (%+v, %v)", initial, err)
	}
	deliverPersonaClaim(t, engine, initial, 5)
	permit, err := engine.ClaimNextLifecycleAction(6)
	if err != nil || permit.Action() != ControllerPersonaPermitNormalUpstream {
		t.Fatalf("permit = (%+v, %v)", permit, err)
	}
	deliverPersonaClaim(t, engine, permit, 7)
	return engine, 7
}

func TestControllerPersonaVerticalSliceLifecycleMetadataInputAndFeedback(t *testing.T) {
	engine := newTestControllerPersonaEngine(t, []byte{0xaa, 0xbb, 0xcc}, 0)
	if got := engine.Snapshot(); got.Generation != 1 || !got.Attached ||
		got.USBState != USBControlDeviceDefault ||
		got.LifecycleState != ControllerLifecycleArrival || got.NormalUpstream {
		t.Fatalf("initial snapshot = %+v", got)
	}
	if _, present, err := engine.ClaimPoll(0); err != nil || present {
		t.Fatalf("pre-configuration Hello = (%t, %v)", present, err)
	}
	configurePersonaUSB(t, engine, 1)

	hello, present, err := engine.ClaimPoll(1)
	if err != nil || !present || hello.Action() != ControllerPersonaSendHello ||
		hello.Sequence() != 1 || hello.Size() != HelloMessageSize {
		t.Fatalf("Hello = (%+v, %t, %v)", hello, present, err)
	}
	helloWire := deliverPersonaClaim(t, engine, hello, 2)
	decodedHello, err := DecodeHelloMessage(helloWire)
	if err != nil || decodedHello.Sequence != 1 {
		t.Fatalf("decoded Hello = (%+v, %v)", decodedHello, err)
	}

	begin := claimPersonaHostAction(t, engine, 3, []byte{0x04, 0x20, 0x01, 0x00})
	if begin.Action() != ControllerPersonaBeginMetadata || begin.Size() != 0 {
		t.Fatalf("metadata begin = %+v", begin)
	}
	deliverPersonaClaim(t, engine, begin, 4)
	metadata, err := engine.ClaimMetadataPacket(5)
	if err != nil || metadata.Action() != ControllerPersonaSendMetadata ||
		metadata.Sequence() != 2 || metadata.MetadataKind() != MetadataPacketSingle {
		t.Fatalf("metadata claim = (%+v, %v)", metadata, err)
	}
	metadataWire := deliverPersonaClaim(t, engine, metadata, 6)
	wantMetadata := []byte{0x04, 0x20, 0x02, 0x03, 0xaa, 0xbb, 0xcc}
	if string(metadataWire) != string(wantMetadata) {
		t.Fatalf("metadata = % x, want % x", metadataWire, wantMetadata)
	}
	if engine.Snapshot().LifecycleState != ControllerLifecycleIdle ||
		engine.Snapshot().GlobalSequence != 2 {
		t.Fatalf("post-metadata snapshot = %+v", engine.Snapshot())
	}

	current := testGamepadInputReport()
	if err := engine.SetCurrentInput(current); err != nil {
		t.Fatal(err)
	}
	start := claimPersonaHostAction(t, engine, 7,
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
	if start.Action() != ControllerPersonaSendCurrentStatus || start.Sequence() != 3 {
		t.Fatalf("current status claim = %+v", start)
	}
	deliverPersonaClaim(t, engine, start, 8)
	initial, err := engine.ClaimNextLifecycleAction(9)
	if err != nil || initial.Action() != ControllerPersonaSendInitialInput ||
		initial.Sequence() != 1 {
		t.Fatalf("initial input = (%+v, %v)", initial, err)
	}
	initialWire := deliverPersonaClaim(t, engine, initial, 10)
	_, initialReport, err := DecodeGamepadInputMessage(initialWire)
	if err != nil || initialReport != current {
		t.Fatalf("initial report = (%+v, %v)", initialReport, err)
	}
	permit, err := engine.ClaimNextLifecycleAction(11)
	if err != nil || permit.Action() != ControllerPersonaPermitNormalUpstream {
		t.Fatalf("permit = (%+v, %v)", permit, err)
	}
	deliverPersonaClaim(t, engine, permit, 12)

	inputReport := GamepadInputReportV1{State: InputStateV1{A: true}}
	input, err := engine.ClaimInput(13, inputReport)
	if err != nil || input.Sequence() != 2 {
		t.Fatalf("input = (%+v, %v)", input, err)
	}
	firstInputWire := make([]byte, input.Size())
	if err := engine.AdmitAndCopy(input, firstInputWire, 13); err != nil {
		t.Fatal(err)
	}
	if err := engine.Resolve(input, ControllerPersonaExecutionCancelled, 14); err != nil {
		t.Fatal(err)
	}
	retry, err := engine.ClaimRetry(15)
	if err != nil || retry.Action() != ControllerPersonaSendInput || retry.Sequence() != 2 {
		t.Fatalf("input retry = (%+v, %v)", retry, err)
	}
	retryWire := deliverPersonaClaim(t, engine, retry, 16)
	if string(retryWire) != string(firstInputWire) {
		t.Fatalf("input retry changed: first=% x retry=% x", firstInputWire, retryWire)
	}

	motorBody := testDirectMotorBody()
	motorWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(motorWire, 0x40, motorBody); err != nil {
		t.Fatal(err)
	}
	motor := claimPersonaHostAction(t, engine, 17, motorWire)
	if got, ok := motor.DirectMotor(); !ok || got != motorBody || motor.Sequence() != 0x40 {
		t.Fatalf("motor classification = (%+v, %t), claim=%+v", got, ok, motor)
	}
	deliverPersonaClaim(t, engine, motor, 18)
	ledBody := GuideLEDCommandV1{Pattern: GuideLEDPatternSlowBlink, Intensity: 20}
	ledWire := make([]byte, GuideLEDCommandMessageSize)
	if err := EncodeGuideLEDCommandMessageInto(ledWire, 0x41, ledBody); err != nil {
		t.Fatal(err)
	}
	led := claimPersonaHostAction(t, engine, 19, ledWire)
	if got, ok := led.GuideLED(); !ok || got != ledBody {
		t.Fatalf("LED classification = (%+v, %t)", got, ok)
	}
	deliverPersonaClaim(t, engine, led, 20)
	if got := engine.Snapshot().Feedback; got.DirectMotor != motorBody || got.GuideLED != ledBody {
		t.Fatalf("feedback snapshot = %+v", got)
	}

	stop := claimPersonaHostAction(t, engine, 21,
		[]byte{0x05, 0x20, 0x03, 0x01, byte(SetDeviceStateStop)})
	if stop.Action() != ControllerPersonaGateNormalUpstream {
		t.Fatalf("STOP first action = %d", stop.Action())
	}
	deliverPersonaClaim(t, engine, stop, 22)
	clearClaim, err := engine.ClaimNextLifecycleAction(23)
	if err != nil || clearClaim.Action() != ControllerPersonaClearOutputs ||
		clearClaim.ClearEpoch() == 0 {
		t.Fatalf("clear claim = (%+v, %v)", clearClaim, err)
	}
	clearEpoch := clearClaim.ClearEpoch()
	if err := engine.AdmitAndCopy(clearClaim, nil, 23); err != nil {
		t.Fatal(err)
	}
	if err := engine.Resolve(clearClaim, ControllerPersonaExecutionCancelled, 24); err != nil {
		t.Fatal(err)
	}
	clearRetry, err := engine.ClaimRetry(25)
	if err != nil || clearRetry.ClearEpoch() != clearEpoch {
		t.Fatalf("clear retry = (%+v, %v), want epoch %d", clearRetry, err, clearEpoch)
	}
	deliverPersonaClaim(t, engine, clearRetry, 26)
	got := engine.Snapshot()
	if got.LifecycleState != ControllerLifecycleIdle || got.NormalUpstream ||
		got.Feedback.DirectMotor != (RumbleBodyV1{}) ||
		got.Feedback.GuideLED != (GuideLEDCommandV1{Pattern: GuideLEDPatternOff}) ||
		got.Feedback.ClearEpoch != clearEpoch {
		t.Fatalf("STOP snapshot = %+v", got)
	}
}

func TestControllerPersonaReliableMetadataACKProgression(t *testing.T) {
	blob := make([]byte, 61)
	for index := range blob {
		blob[index] = byte(index)
	}
	engine := newTestControllerPersonaEngine(t, blob, 0)
	configurePersonaUSB(t, engine, 0)
	hello, present, err := engine.ClaimPoll(0)
	if err != nil || !present {
		t.Fatalf("Hello = (%t, %v)", present, err)
	}
	deliverPersonaClaim(t, engine, hello, 1)
	begin := claimPersonaHostAction(t, engine, 2, []byte{0x04, 0x20, 0x01, 0x00})
	deliverPersonaClaim(t, engine, begin, 3)

	initial, err := engine.ClaimMetadataPacket(4)
	if err != nil || initial.Sequence() != 2 ||
		initial.MetadataKind() != MetadataPacketInitialFragment ||
		!initial.MetadataAcknowledgementRequested() {
		t.Fatalf("initial metadata = (%+v, %v)", initial, err)
	}
	deliverPersonaClaim(t, engine, initial, 5)
	ackBody := ProtocolControlACKBodyV1{
		ReferencedDataClass: DataClassCommand, ReferencedMessageNumber: messageNumberMetadataRequest,
		ReferencedSystem: true, FragmentOffset: 58, RemainingBuffer: 512,
	}
	ackWire := make([]byte, ProtocolControlACKMessageSize)
	if err := EncodeProtocolControlACKMessageInto(ackWire, 2, ackBody); err != nil {
		t.Fatal(err)
	}
	ack, disposition, err := engine.ReceiveHostMessage(6, ackWire)
	if err != nil || disposition != ControllerPersonaHostAcknowledgementProgress {
		t.Fatalf("initial ACK = (%d, %v)", disposition, err)
	}
	if predicted, ok := ack.ReliableAcknowledgementDisposition(); !ok ||
		predicted != ReliableAcknowledgementProgress {
		t.Fatalf("initial ACK claim = (%d, %t)", predicted, ok)
	}
	deliverPersonaClaim(t, engine, ack, 6)
	final, err := engine.ClaimMetadataPacket(7)
	if err != nil || final.MetadataKind() != MetadataPacketFinalFragment ||
		final.Sequence() != 2 || !final.MetadataAcknowledgementRequested() {
		t.Fatalf("final metadata = (%+v, %v)", final, err)
	}
	deliverPersonaClaim(t, engine, final, 8)
	ackBody.FragmentOffset = 61
	if err := EncodeProtocolControlACKMessageInto(ackWire, 2, ackBody); err != nil {
		t.Fatal(err)
	}
	ack, disposition, err = engine.ReceiveHostMessage(9, ackWire)
	if err != nil || disposition != ControllerPersonaHostAcknowledgementProgress {
		t.Fatalf("final ACK = (%d, %v)", disposition, err)
	}
	deliverPersonaClaim(t, engine, ack, 9)
	complete, err := engine.ClaimMetadataPacket(10)
	if err != nil || complete.MetadataKind() != MetadataPacketComplete ||
		complete.Sequence() != 2 {
		t.Fatalf("metadata complete = (%+v, %v)", complete, err)
	}
	deliverPersonaClaim(t, engine, complete, 11)
	if got := engine.Snapshot(); got.MetadataActive ||
		got.LifecycleState != ControllerLifecycleIdle || got.GlobalSequence != 2 {
		t.Fatalf("completed metadata snapshot = %+v", got)
	}
}

func TestControllerPersonaMetadataACKCommitsOnlyAfterDeliveredRetry(t *testing.T) {
	engine := newTestControllerPersonaEngine(t, make([]byte, 61), 0)
	configurePersonaUSB(t, engine, 0)
	hello, present, err := engine.ClaimPoll(0)
	if err != nil || !present {
		t.Fatalf("Hello = (%t, %v)", present, err)
	}
	deliverPersonaClaim(t, engine, hello, 1)
	begin := claimPersonaHostAction(t, engine, 2,
		[]byte{0x04, 0x20, 0x01, 0x00})
	deliverPersonaClaim(t, engine, begin, 3)
	initial, err := engine.ClaimMetadataPacket(4)
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, initial, 5)

	body := ProtocolControlACKBodyV1{
		ReferencedDataClass:     DataClassCommand,
		ReferencedMessageNumber: messageNumberMetadataRequest,
		ReferencedSystem:        true, FragmentOffset: 58, RemainingBuffer: 512,
	}
	var wire [ProtocolControlACKMessageSize]byte
	if err := EncodeProtocolControlACKMessageInto(wire[:], 2, body); err != nil {
		t.Fatal(err)
	}
	claim, disposition, err := engine.ReceiveHostMessage(6, wire[:])
	if err != nil || disposition != ControllerPersonaHostAcknowledgementProgress {
		t.Fatalf("ACK selection = (%d, %v)", disposition, err)
	}
	if got := engine.metadataTransfer.Snapshot(); got.AcknowledgedEnd != 0 ||
		!got.AwaitingAcknowledgement {
		t.Fatalf("selection applied ACK: %+v", got)
	}
	if err := engine.Resolve(claim, ControllerPersonaDeliveryFailed, 7); err != nil {
		t.Fatal(err)
	}
	if got := engine.metadataTransfer.Snapshot(); got.AcknowledgedEnd != 0 ||
		!got.AwaitingAcknowledgement {
		t.Fatalf("delivery failure applied ACK: %+v", got)
	}
	retry, err := engine.ClaimRetry(8)
	if err != nil || retry.Action() != ControllerPersonaApplyMetadataAcknowledgement {
		t.Fatalf("ACK retry = (%+v, %v)", retry, err)
	}
	deliverPersonaClaim(t, engine, retry, 8)
	if got := engine.metadataTransfer.Snapshot(); got.AcknowledgedEnd != 58 ||
		got.AwaitingAcknowledgement {
		t.Fatalf("delivered retry did not apply ACK: %+v", got)
	}
}

func TestControllerPersonaExpiredMetadataACKRetryYieldsFailureHello(t *testing.T) {
	engine := newTestControllerPersonaEngine(t, make([]byte, 61), 0)
	configurePersonaUSB(t, engine, 0)
	hello, present, err := engine.ClaimPoll(0)
	if err != nil || !present {
		t.Fatalf("Hello = (%t, %v)", present, err)
	}
	deliverPersonaClaim(t, engine, hello, 1)
	begin := claimPersonaHostAction(t, engine, 2,
		[]byte{0x04, 0x20, 0x01, 0x00})
	deliverPersonaClaim(t, engine, begin, 3)
	initial, err := engine.ClaimMetadataPacket(4)
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, initial, 5)

	body := ProtocolControlACKBodyV1{
		ReferencedDataClass:     DataClassCommand,
		ReferencedMessageNumber: messageNumberMetadataRequest,
		ReferencedSystem:        true, FragmentOffset: 58, RemainingBuffer: 512,
	}
	var wire [ProtocolControlACKMessageSize]byte
	if err := EncodeProtocolControlACKMessageInto(wire[:], 2, body); err != nil {
		t.Fatal(err)
	}
	claim, _, err := engine.ReceiveHostMessage(6, wire[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.AdmitAndCopy(claim, nil, 6); err != nil {
		t.Fatal(err)
	}
	if err := engine.Resolve(claim, ControllerPersonaDeliveryFailed, 7); err != nil {
		t.Fatal(err)
	}

	deadline := uint64(5) + ReliableACKTimeoutMilliseconds
	if _, err := engine.ClaimRetry(deadline); !errors.Is(
		err, ErrReliableTransferTimeout) {
		t.Fatalf("expired ACK retry error = %v", err)
	}
	if engine.retryPending || !engine.metadataTransfer.Faulted() {
		t.Fatalf("expired ACK retry not retired: personaRetry=%t metadata=%+v",
			engine.retryPending, engine.metadataTransfer.Snapshot())
	}
	failureHello, present, err := engine.ClaimPoll(deadline)
	if err != nil || !present ||
		failureHello.Action() != ControllerPersonaSendHello {
		t.Fatalf("failure Hello = (%+v, %t, %v)", failureHello, present, err)
	}
}

func TestControllerPersonaMetadataTimeoutHelloRetiresFaultAndKeepsCadence(t *testing.T) {
	blob := make([]byte, 61)
	engine := newTestControllerPersonaEngine(t, blob, 0)
	configurePersonaUSB(t, engine, 0)
	hello, present, err := engine.ClaimPoll(0)
	if err != nil || !present || hello.Sequence() != 1 {
		t.Fatalf("arrival Hello = (%+v, %t, %v)", hello, present, err)
	}
	deliverPersonaClaim(t, engine, hello, 1)
	begin := claimPersonaHostAction(t, engine, 2, []byte{0x04, 0x20, 0x01, 0x00})
	deliverPersonaClaim(t, engine, begin, 3)
	packet, err := engine.ClaimMetadataPacket(4)
	if err != nil || packet.Sequence() != 2 ||
		packet.MetadataKind() != MetadataPacketInitialFragment ||
		!packet.MetadataAcknowledgementRequested() {
		t.Fatalf("initial metadata = (%+v, %v)", packet, err)
	}
	deliverPersonaClaim(t, engine, packet, 5)

	failureHello, present, err := engine.ClaimPoll(5 + ReliableACKTimeoutMilliseconds)
	if err != nil || !present || failureHello.Action() != ControllerPersonaSendHello ||
		failureHello.Sequence() != 3 {
		t.Fatalf("failure Hello = (%+v, %t, %v)", failureHello, present, err)
	}
	if err := engine.AdmitAndCopy(
		failureHello, make([]byte, failureHello.Size()), 1005); err != nil {
		t.Fatal(err)
	}
	if err := engine.Resolve(
		failureHello, ControllerPersonaExecutionCancelled, 1006); err != nil {
		t.Fatal(err)
	}
	retry, err := engine.ClaimRetry(1007)
	if err != nil || retry.Action() != ControllerPersonaSendHello ||
		retry.Sequence() != 3 {
		t.Fatalf("failure Hello retry = (%+v, %v)", retry, err)
	}
	deliverPersonaClaim(t, engine, retry, 1008)
	if got := engine.Snapshot(); got.MetadataActive ||
		got.LifecycleState != ControllerLifecycleArrival || got.GlobalSequence != 3 {
		t.Fatalf("post-failure snapshot = %+v", got)
	}
	if _, present, err := engine.ClaimPoll(1507); err != nil || present {
		t.Fatalf("early cadence = (%t, %v)", present, err)
	}
	nextHello, present, err := engine.ClaimPoll(1508)
	if err != nil || !present || nextHello.Action() != ControllerPersonaSendHello ||
		nextHello.Sequence() != 4 {
		t.Fatalf("next cadence Hello = (%+v, %t, %v)", nextHello, present, err)
	}
}

func TestControllerPersonaResetDisconnectReconnectAndStaleClaims(t *testing.T) {
	engine, nowMS := makePersonaActive(t, []byte{1, 2, 3})
	input, err := engine.ClaimInput(nowMS+1, GamepadInputReportV1{State: InputStateV1{B: true}})
	if err != nil {
		t.Fatal(err)
	}
	reset, err := engine.BeginUSBReset(nowMS + 2)
	if err != nil || reset.Generation() != 2 ||
		reset.Action() != ControllerPersonaClearOutputs {
		t.Fatalf("reset = (%+v, %v)", reset, err)
	}
	if err := engine.AdmitAndCopy(input, make([]byte, input.Size()), nowMS+2); !errors.Is(
		err, ErrInvalidControllerPersonaClaim) {
		t.Fatalf("stale input admission error = %v", err)
	}
	if got := engine.Snapshot(); got.Generation != 2 ||
		got.USBState != USBControlDeviceDefault || got.NormalUpstream ||
		got.GlobalSequence != 0 || got.InputSequence != 0 {
		t.Fatalf("reset successor snapshot = %+v", got)
	}
	deliverPersonaClaim(t, engine, reset, nowMS+3)
	if _, present, err := engine.ClaimPoll(nowMS + 4); err != nil || present {
		t.Fatalf("pre-config reset Hello = (%t, %v)", present, err)
	}
	configurePersonaUSB(t, engine, nowMS+5)
	hello, present, err := engine.ClaimPoll(nowMS + 5)
	if err != nil || !present || hello.Generation() != 2 || hello.Sequence() != 1 {
		t.Fatalf("successor Hello = (%+v, %t, %v)", hello, present, err)
	}
	deliverPersonaClaim(t, engine, hello, nowMS+6)

	start := claimPersonaHostAction(t, engine, nowMS+7,
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
	deliverPersonaClaim(t, engine, start, nowMS+8)
	initial, _ := engine.ClaimNextLifecycleAction(nowMS + 9)
	deliverPersonaClaim(t, engine, initial, nowMS+10)
	permit, _ := engine.ClaimNextLifecycleAction(nowMS + 11)
	deliverPersonaClaim(t, engine, permit, nowMS+12)
	admitted, err := engine.ClaimInput(nowMS+13,
		GamepadInputReportV1{State: InputStateV1{X: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.AdmitAndCopy(admitted, make([]byte, admitted.Size()), nowMS+13); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.BeginUSBReset(nowMS + 14); !errors.Is(
		err, ErrControllerPersonaBoundaryBlocked) {
		t.Fatalf("reset overtook admitted input: %v", err)
	}
	if err := engine.Resolve(admitted, ControllerPersonaExecutionCancelled, nowMS+14); err != nil {
		t.Fatal(err)
	}
	reset, err = engine.BeginUSBReset(nowMS + 15)
	if err != nil || reset.Generation() != 3 {
		t.Fatalf("post-drain reset = (%+v, %v)", reset, err)
	}
	if err := engine.Resolve(admitted, ControllerPersonaDelivered, nowMS+15); !errors.Is(
		err, ErrInvalidControllerPersonaClaim) {
		t.Fatalf("late input completion error = %v", err)
	}
	deliverPersonaClaim(t, engine, reset, nowMS+16)

	disconnect, err := engine.BeginDisconnect(nowMS + 17)
	if err != nil || disconnect.Generation() != 4 {
		t.Fatalf("disconnect = (%+v, %v)", disconnect, err)
	}
	disconnectEpoch := disconnect.ClearEpoch()
	deliverPersonaClaim(t, engine, disconnect, nowMS+18)
	if got := engine.Snapshot(); got.Attached ||
		got.USBState != USBControlDeviceDetached || got.Generation != 4 {
		t.Fatalf("detached snapshot = %+v", got)
	}
	if _, err := engine.ClaimUSBControl(testUSBSetup(0x80, 0x06, 0x0100, 0, 8)); !errors.Is(err, ErrUSBControlPlaneDetached) {
		t.Fatalf("detached EP0 error = %v", err)
	}
	if err := engine.Reconnect(nowMS + 19); err != nil {
		t.Fatal(err)
	}
	if got := engine.Snapshot(); !got.Attached || got.Generation != 5 ||
		got.USBState != USBControlDeviceDefault ||
		got.Feedback.ClearEpoch != disconnectEpoch {
		t.Fatalf("reconnected snapshot = %+v", got)
	}
}

func TestControllerPersonaConfigurationLossBlocksWireLifecycleCursor(t *testing.T) {
	engine := newTestControllerPersonaEngine(t, []byte{1}, 0)
	configurePersonaUSB(t, engine, 0)
	hello, _, _ := engine.ClaimPoll(0)
	deliverPersonaClaim(t, engine, hello, 1)
	start := claimPersonaHostAction(t, engine, 2,
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
	deliverPersonaClaim(t, engine, start, 3)

	unconfigure, err := engine.ClaimUSBControl(testUSBSetup(
		usbRequestTypeDeviceOut, usbRequestSetConfiguration,
		uint16(usbConfigurationUnselected), 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, unconfigure, 4)
	if got := engine.Snapshot(); !got.ConfigurationLossClearPending ||
		got.USBState != USBControlDeviceAddressed {
		t.Fatalf("configuration-loss snapshot = %+v", got)
	}
	clear, present, err := engine.ClaimPoll(5)
	if err != nil || !present || clear.Action() != ControllerPersonaClearOutputs ||
		clear.ClearEpoch() == 0 {
		t.Fatalf("configuration-loss clear = (%+v, %t, %v)", clear, present, err)
	}
	deliverPersonaClaim(t, engine, clear, 5)
	if _, err := engine.ClaimNextLifecycleAction(5); !errors.Is(
		err, ErrControllerPersonaUSBNotConfigured) {
		t.Fatalf("unconfigured lifecycle cursor error = %v", err)
	}
	if got := engine.Snapshot(); got.ClaimOutstanding ||
		!engine.lifecycle.Snapshot().PendingTransition ||
		engine.input.hasClaim || engine.input.retryPending {
		t.Fatalf("unconfigured cursor mutated state: %+v", got)
	}

	reconfigure, err := engine.ClaimUSBControl(testUSBSetup(
		usbRequestTypeDeviceOut, usbRequestSetConfiguration,
		uint16(usbConfigurationGIP), 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, reconfigure, 6)
	haltIN, err := engine.ClaimUSBControl(testUSBSetup(
		usbRequestTypeEndpointOut, usbRequestSetFeature,
		usbFeatureEndpointHalt, 0x81, 0))
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, haltIN, 7)
	if _, err := engine.ClaimNextLifecycleAction(8); !errors.Is(
		err, ErrControllerPersonaEndpointHalted) {
		t.Fatalf("halted lifecycle cursor error = %v", err)
	}
	clearIN, err := engine.ClaimUSBControl(testUSBSetup(
		usbRequestTypeEndpointOut, usbRequestClearFeature,
		usbFeatureEndpointHalt, 0x81, 0))
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, clearIN, 9)
	initial, err := engine.ClaimNextLifecycleAction(10)
	if err != nil || initial.Action() != ControllerPersonaSendInitialInput ||
		initial.Sequence() != 1 {
		t.Fatalf("reconfigured lifecycle cursor = (%+v, %v)", initial, err)
	}
}

func TestControllerPersonaConfigurationLossClearIsDeliveredOnlyAndExact(t *testing.T) {
	engine, nowMS := makePersonaActive(t, []byte{1})
	motorBody := testDirectMotorBody()
	motorWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(motorWire, 0x61, motorBody); err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine,
		claimPersonaHostAction(t, engine, nowMS+1, motorWire), nowMS+2)
	ledBody := GuideLEDCommandV1{Pattern: GuideLEDPatternSlowBlink, Intensity: 19}
	ledWire := make([]byte, GuideLEDCommandMessageSize)
	if err := EncodeGuideLEDCommandMessageInto(ledWire, 0x62, ledBody); err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine,
		claimPersonaHostAction(t, engine, nowMS+3, ledWire), nowMS+4)

	setup := testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration,
		uint16(usbConfigurationUnselected), 0, 0)
	failed, err := engine.ClaimUSBControl(setup)
	if err != nil || !failed.RequiresConfigurationLossClear() {
		t.Fatalf("failed transition claim = (%+v, %v)", failed, err)
	}
	if err := engine.AdmitAndCopy(failed, nil, nowMS+5); err != nil {
		t.Fatal(err)
	}
	if err := engine.Resolve(
		failed, ControllerPersonaDeliveryFailed, nowMS+6); err != nil {
		t.Fatal(err)
	}
	if got := engine.Snapshot(); got.USBState != USBControlDeviceConfigured ||
		got.ConfigurationLossClearPending || got.Feedback.DirectMotor != motorBody ||
		got.Feedback.GuideLED != ledBody {
		t.Fatalf("failed control mutated state = %+v", got)
	}

	delivered, err := engine.ClaimUSBControl(setup)
	if err != nil || !delivered.RequiresConfigurationLossClear() {
		t.Fatalf("delivered transition claim = (%+v, %v)", delivered, err)
	}
	deliverPersonaClaim(t, engine, delivered, nowMS+7)
	if got := engine.Snapshot(); got.USBState != USBControlDeviceAddressed ||
		!got.ConfigurationLossClearPending || got.Feedback.DirectMotor != motorBody ||
		got.Feedback.GuideLED != ledBody || got.Feedback.ClearEpoch != 0 {
		t.Fatalf("delivered control applied clear early = %+v", got)
	}
	if _, err := engine.ClaimUSBControl(setup); !errors.Is(
		err, ErrControllerPersonaConfigurationLossClearRequired) {
		t.Fatalf("configuration clear did not fence unrelated work: %v", err)
	}
	clear, present, err := engine.ClaimPoll(nowMS + 8)
	if err != nil || !present || clear.Action() != ControllerPersonaClearOutputs ||
		clear.ClearEpoch() == 0 {
		t.Fatalf("clear = (%+v, %t, %v)", clear, present, err)
	}
	beforeBoundary := engine.Snapshot()
	if _, err := engine.BeginUSBReset(nowMS + 8); !errors.Is(
		err, ErrControllerPersonaBoundaryBlocked) {
		t.Fatalf("selected clear reset fence = %v", err)
	}
	if _, err := engine.BeginDisconnect(nowMS + 8); !errors.Is(
		err, ErrControllerPersonaBoundaryBlocked) {
		t.Fatalf("selected clear disconnect fence = %v", err)
	}
	if afterBoundary := engine.Snapshot(); afterBoundary != beforeBoundary {
		t.Fatalf("selected-clear boundary changed state: before=%+v after=%+v",
			beforeBoundary, afterBoundary)
	}
	clearEpoch := clear.ClearEpoch()
	if err := engine.AdmitAndCopy(clear, nil, nowMS+8); err != nil {
		t.Fatal(err)
	}
	if err := engine.Resolve(
		clear, ControllerPersonaDeliveryFailed, nowMS+9); err != nil {
		t.Fatal(err)
	}
	if got := engine.Snapshot(); !got.RetryPending ||
		got.Feedback.DirectMotor != motorBody || got.Feedback.GuideLED != ledBody {
		t.Fatalf("failed clear mutated feedback = %+v", got)
	}
	beforeBoundary = engine.Snapshot()
	if _, err := engine.BeginUSBReset(nowMS + 10); !errors.Is(
		err, ErrControllerPersonaBoundaryBlocked) {
		t.Fatalf("clear retry reset fence = %v", err)
	}
	if _, err := engine.BeginDisconnect(nowMS + 10); !errors.Is(
		err, ErrControllerPersonaBoundaryBlocked) {
		t.Fatalf("clear retry disconnect fence = %v", err)
	}
	if afterBoundary := engine.Snapshot(); afterBoundary != beforeBoundary {
		t.Fatalf("clear-retry boundary changed state: before=%+v after=%+v",
			beforeBoundary, afterBoundary)
	}
	retry, err := engine.ClaimRetry(nowMS + 10)
	if err != nil || retry.Action() != ControllerPersonaClearOutputs ||
		retry.ClearEpoch() != clearEpoch {
		t.Fatalf("clear retry = (%+v, %v)", retry, err)
	}
	deliverPersonaClaim(t, engine, retry, nowMS+11)
	if got := engine.Snapshot(); got.ConfigurationLossClearPending || got.RetryPending ||
		got.Feedback.DirectMotor != (RumbleBodyV1{}) ||
		got.Feedback.GuideLED != (GuideLEDCommandV1{Pattern: GuideLEDPatternOff}) ||
		got.Feedback.ClearEpoch != clearEpoch {
		t.Fatalf("delivered clear snapshot = %+v", got)
	}

	idempotent, err := engine.ClaimUSBControl(setup)
	if err != nil || idempotent.RequiresConfigurationLossClear() {
		t.Fatalf("idempotent zero claim = (%+v, %v)", idempotent, err)
	}
	deliverPersonaClaim(t, engine, idempotent, nowMS+12)
	if got := engine.Snapshot(); got.ConfigurationLossClearPending ||
		got.Feedback.ClearEpoch != clearEpoch {
		t.Fatalf("idempotent zero manufactured clear = %+v", got)
	}
}

func TestControllerPersonaConfigurationLossEpochExhaustionFencesEP0Admission(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, []byte{1}, 0)
	configurePersonaUSB(t, engine, 0)
	engine.nextClearEpoch = ^uint64(0)
	claim, err := engine.ClaimUSBControl(testUSBSetup(
		usbRequestTypeDeviceOut, usbRequestSetConfiguration,
		uint16(usbConfigurationUnselected), 0, 0))
	if err != nil || !claim.RequiresConfigurationLossClear() {
		t.Fatalf("configuration-loss claim = (%+v, %v)", claim, err)
	}
	if err := engine.AdmitAndCopy(claim, nil, 1); !errors.Is(
		err, ErrInvalidTransferEpoch) {
		t.Fatalf("epoch exhaustion admission = %v", err)
	}
	if err := engine.Resolve(
		claim, ControllerPersonaDeliveryFailed, 1); err != nil {
		t.Fatal(err)
	}
	if got := engine.Snapshot(); got.USBState != USBControlDeviceConfigured ||
		got.ConfigurationLossClearPending || got.ClaimOutstanding {
		t.Fatalf("failed admission changed configuration = %+v", got)
	}
}

func TestControllerPersonaEndpointHaltGatesOnlyItsDirection(t *testing.T) {
	engine, nowMS := makePersonaActive(t, []byte{1})
	haltOUT, err := engine.ClaimUSBControl(testUSBSetup(
		usbRequestTypeEndpointOut, usbRequestSetFeature,
		usbFeatureEndpointHalt, 0x01, 0))
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, haltOUT, nowMS+1)
	motorWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(
		motorWire, 1, testDirectMotorBody()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.ReceiveHostMessage(nowMS+2, motorWire); !errors.Is(
		err, ErrControllerPersonaEndpointHalted) {
		t.Fatalf("halted OUT error = %v", err)
	}
	input, err := engine.ClaimInput(
		nowMS+2, GamepadInputReportV1{State: InputStateV1{A: true}})
	if err != nil {
		t.Fatalf("OUT halt blocked IN: %v", err)
	}
	deliverPersonaClaim(t, engine, input, nowMS+3)
	clearOUT, err := engine.ClaimUSBControl(testUSBSetup(
		usbRequestTypeEndpointOut, usbRequestClearFeature,
		usbFeatureEndpointHalt, 0x01, 0))
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, clearOUT, nowMS+4)
	claim := claimPersonaHostAction(t, engine, nowMS+5, motorWire)
	if claim.Action() != ControllerPersonaApplyDirectMotor {
		t.Fatalf("post-clear motor action = %d", claim.Action())
	}
}

func TestControllerPersonaINHaltHoldsImmediateStartWireWithoutConsumingState(t *testing.T) {
	engine := newTestControllerPersonaEngine(t, []byte{1}, 0)
	configurePersonaUSB(t, engine, 0)
	hello, present, err := engine.ClaimPoll(0)
	if err != nil || !present {
		t.Fatalf("Hello = (%+v, %t, %v)", hello, present, err)
	}
	deliverPersonaClaim(t, engine, hello, 1)

	haltIN, err := engine.ClaimUSBControl(testUSBSetup(
		usbRequestTypeEndpointOut, usbRequestSetFeature,
		usbFeatureEndpointHalt, 0x81, 0))
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, haltIN, 2)
	startWire := []byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)}
	if _, _, err := engine.ReceiveHostMessage(3, startWire); !errors.Is(
		err, ErrControllerPersonaEndpointHalted) {
		t.Fatalf("START under IN Halt error = %v", err)
	}
	if got := engine.Snapshot(); got.ClaimOutstanding ||
		engine.lifecycle.Snapshot().PendingTransition || engine.global.hasClaim ||
		engine.global.retryPending || got.GlobalSequence != 1 {
		t.Fatalf("IN Halt consumed START state: %+v", got)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		_, _, receiveErr := engine.ReceiveHostMessage(3, startWire)
		if receiveErr != ErrControllerPersonaEndpointHalted {
			panic(receiveErr)
		}
	}); allocs != 0 {
		t.Fatalf("halted START allocations = %v, want 0", allocs)
	}

	// OUT remains available: directional IN Halt must not block typed host
	// feedback which has no upstream response.
	motorWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(
		motorWire, 3, testDirectMotorBody()); err != nil {
		t.Fatal(err)
	}
	motor := claimPersonaHostAction(t, engine, 3, motorWire)
	deliverPersonaClaim(t, engine, motor, 3)

	clearIN, err := engine.ClaimUSBControl(testUSBSetup(
		usbRequestTypeEndpointOut, usbRequestClearFeature,
		usbFeatureEndpointHalt, 0x81, 0))
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, clearIN, 4)
	start := claimPersonaHostAction(t, engine, 5, startWire)
	if start.Action() != ControllerPersonaSendCurrentStatus ||
		start.Sequence() != 2 {
		t.Fatalf("post-Halt START = %+v", start)
	}
}

func TestControllerPersonaHostResetProducesOneClearBeforeSuccessor(t *testing.T) {
	engine, nowMS := makePersonaActive(t, []byte{1})
	motorWire := make([]byte, DirectMotorMessageSize)
	body := testDirectMotorBody()
	if err := EncodeDirectMotorMessageInto(motorWire, 0x20, body); err != nil {
		t.Fatal(err)
	}
	motor := claimPersonaHostAction(t, engine, nowMS+1, motorWire)
	deliverPersonaClaim(t, engine, motor, nowMS+2)

	reset := claimPersonaHostAction(t, engine, nowMS+3,
		[]byte{0x05, 0x20, 0x03, 0x01, byte(SetDeviceStateReset)})
	if reset.Action() != ControllerPersonaGateNormalUpstream {
		t.Fatalf("reset first action = %d", reset.Action())
	}
	deliverPersonaClaim(t, engine, reset, nowMS+4)
	clearClaim, err := engine.ClaimNextLifecycleAction(nowMS + 5)
	if err != nil || clearClaim.Action() != ControllerPersonaClearOutputs {
		t.Fatalf("reset clear = (%+v, %v)", clearClaim, err)
	}
	clearEpoch := clearClaim.ClearEpoch()
	deliverPersonaClaim(t, engine, clearClaim, nowMS+6)
	status, err := engine.ClaimNextLifecycleAction(nowMS + 7)
	if err != nil || status.Action() != ControllerPersonaSendPoweringOffStatus {
		t.Fatalf("powering-off status = (%+v, %v)", status, err)
	}
	deliverPersonaClaim(t, engine, status, nowMS+8)
	perform, present, err := engine.ClaimPoll(nowMS + 508)
	if err != nil || !present || perform.Action() != ControllerPersonaPerformReset {
		t.Fatalf("perform reset = (%+v, %t, %v)", perform, present, err)
	}
	deliverPersonaClaim(t, engine, perform, nowMS+509)
	got := engine.Snapshot()
	if got.Generation != 2 || got.USBState != USBControlDeviceDefault ||
		got.Feedback.ClearEpoch != clearEpoch ||
		got.Feedback.DirectMotor != (RumbleBodyV1{}) {
		t.Fatalf("host reset successor = %+v", got)
	}
}

func TestControllerPersonaAdmissionIsExactAndClaimsAreOpaque(t *testing.T) {
	engine, nowMS := makePersonaActive(t, []byte{1})
	claim, err := engine.ClaimInput(
		nowMS+1, GamepadInputReportV1{State: InputStateV1{Y: true}})
	if err != nil {
		t.Fatal(err)
	}
	exact := make([]byte, claim.Size())
	for _, size := range []int{claim.Size() - 1, claim.Size() + 1} {
		dst := make([]byte, size)
		for index := range dst {
			dst[index] = 0xa5
		}
		before := append([]byte(nil), dst...)
		if err := engine.AdmitAndCopy(claim, dst, nowMS+1); !errors.Is(
			err, ErrInvalidControllerPersonaDestination) {
			t.Fatalf("destination size %d error = %v", size, err)
		}
		if string(dst) != string(before) || engine.Snapshot().ClaimAdmitted {
			t.Fatalf("destination size %d mutated state or bytes", size)
		}
	}
	for name, forge := range map[string]func(ControllerPersonaClaim) ControllerPersonaClaim{
		"token": func(value ControllerPersonaClaim) ControllerPersonaClaim {
			value.token++
			return value
		},
		"generation": func(value ControllerPersonaClaim) ControllerPersonaClaim {
			value.generation++
			return value
		},
		"action": func(value ControllerPersonaClaim) ControllerPersonaClaim {
			value.action++
			return value
		},
		"size": func(value ControllerPersonaClaim) ControllerPersonaClaim {
			value.size++
			return value
		},
		"sequence": func(value ControllerPersonaClaim) ControllerPersonaClaim {
			value.sequence++
			return value
		},
		"selection clock": func(value ControllerPersonaClaim) ControllerPersonaClaim {
			value.selectedAtMS++
			return value
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := engine.AdmitAndCopy(forge(claim), exact, nowMS+1); !errors.Is(
				err, ErrInvalidControllerPersonaClaim) {
				t.Fatalf("forged claim error = %v", err)
			}
			if engine.Snapshot().ClaimAdmitted {
				t.Fatal("forged claim crossed admission")
			}
		})
	}
	if err := engine.AdmitAndCopy(claim, exact, nowMS+1); err != nil {
		t.Fatal(err)
	}
	if err := engine.AdmitAndCopy(claim, exact, nowMS+1); !errors.Is(
		err, ErrInvalidControllerPersonaClaim) {
		t.Fatalf("duplicate admission error = %v", err)
	}
	if err := engine.Resolve(claim, ControllerPersonaDelivered, nowMS+2); err != nil {
		t.Fatal(err)
	}
	if err := engine.Resolve(claim, ControllerPersonaDelivered, nowMS+2); !errors.Is(
		err, ErrInvalidControllerPersonaClaim) {
		t.Fatalf("duplicate resolution error = %v", err)
	}
}

func TestControllerPersonaCompositionPrevalidationDoesNotWedge(t *testing.T) {
	t.Run("metadata construction before sequence reservation", func(t *testing.T) {
		engine := newTestControllerPersonaEngine(t, []byte{1, 2, 3}, 0)
		configurePersonaUSB(t, engine, 0)
		hello, _, _ := engine.ClaimPoll(0)
		deliverPersonaClaim(t, engine, hello, 1)
		begin := claimPersonaHostAction(t, engine, 2,
			[]byte{0x04, 0x20, 0x01, 0x00})
		deliverPersonaClaim(t, engine, begin, 3)

		metadata := engine.metadata
		engine.metadata.valid = false
		if _, err := engine.ClaimMetadataPacket(4); !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("invalid metadata error = %v", err)
		}
		if engine.hasClaim || engine.global.hasClaim || engine.global.retryPending ||
			engine.metadataTransfer.Snapshot().ClaimOutstanding {
			t.Fatalf("failed metadata construction wedged state: %+v", engine.Snapshot())
		}
		engine.metadata = metadata
		claim, err := engine.ClaimMetadataPacket(4)
		if err != nil || claim.Sequence() != 2 {
			t.Fatalf("metadata recovery = (%+v, %v)", claim, err)
		}
	})

	t.Run("lifecycle source validation before lifecycle claim", func(t *testing.T) {
		engine := newTestControllerPersonaEngine(t, []byte{1}, 0)
		configurePersonaUSB(t, engine, 0)
		hello, _, _ := engine.ClaimPoll(0)
		deliverPersonaClaim(t, engine, hello, 1)
		engine.currentStatus.PowerLevel = StatusPoweringOff
		if _, _, err := engine.ReceiveHostMessage(2,
			[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)}); !errors.Is(
			err, ErrInvalidControllerPersonaConfig) {
			t.Fatalf("invalid status error = %v", err)
		}
		if engine.hasClaim || engine.lifecycle.Snapshot().PendingTransition ||
			engine.global.hasClaim || engine.global.retryPending {
			t.Fatalf("failed lifecycle composition wedged state: %+v", engine.Snapshot())
		}
		if err := engine.SetCurrentStatus(NewWiredNoBatteryStatus(false)); err != nil {
			t.Fatal(err)
		}
		claim := claimPersonaHostAction(t, engine, 2,
			[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
		if claim.Action() != ControllerPersonaSendCurrentStatus || claim.Sequence() != 2 {
			t.Fatalf("lifecycle recovery claim = %+v", claim)
		}
	})

	t.Run("admit and resolve prevalidate every inner owner", func(t *testing.T) {
		engine := newTestControllerPersonaEngine(t, []byte{1}, 0)
		configurePersonaUSB(t, engine, 0)
		hello, _, _ := engine.ClaimPoll(0)
		deliverPersonaClaim(t, engine, hello, 1)
		claim := claimPersonaHostAction(t, engine, 2,
			[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
		sequenceToken := engine.global.claimToken
		engine.global.claimToken++
		if err := engine.AdmitAndCopy(
			claim, make([]byte, claim.Size()), 2); !errors.Is(err, ErrInvalidSequenceClaim) {
			t.Fatalf("admission fence error = %v", err)
		}
		if engine.lifecycle.claimAdmitted || engine.global.claimAdmitted ||
			engine.claimAdmitted {
			t.Fatalf("partial inner admission: %+v", engine.Snapshot())
		}
		engine.global.claimToken = sequenceToken
		if err := engine.AdmitAndCopy(claim, make([]byte, claim.Size()), 2); err != nil {
			t.Fatal(err)
		}
		engine.global.claimToken++
		if err := engine.Resolve(claim, ControllerPersonaDelivered, 3); !errors.Is(
			err, ErrInvalidSequenceClaim) {
			t.Fatalf("resolution fence error = %v", err)
		}
		if !engine.lifecycle.hasClaim || !engine.lifecycle.claimAdmitted ||
			!engine.global.hasClaim || !engine.global.claimAdmitted {
			t.Fatalf("partial inner resolution: %+v", engine.Snapshot())
		}
		engine.global.claimToken = sequenceToken
		if err := engine.Resolve(claim, ControllerPersonaDelivered, 3); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("retry prevalidates every inner owner", func(t *testing.T) {
		engine := newTestControllerPersonaEngine(t, []byte{1}, 0)
		configurePersonaUSB(t, engine, 0)
		hello, _, _ := engine.ClaimPoll(0)
		deliverPersonaClaim(t, engine, hello, 1)
		claim := claimPersonaHostAction(t, engine, 2,
			[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
		if err := engine.Resolve(
			claim, ControllerPersonaDeliveryFailed, 2); err != nil {
			t.Fatal(err)
		}
		engine.global.retryPending = false
		if _, err := engine.ClaimRetry(3); !errors.Is(err, ErrSequenceRetryRequired) {
			t.Fatalf("retry fence error = %v", err)
		}
		if engine.lifecycle.hasClaim || !engine.lifecycle.retryPending ||
			!engine.retryPending {
			t.Fatalf("partial inner retry claim: %+v", engine.Snapshot())
		}
		engine.global.retryPending = true
		retry, err := engine.ClaimRetry(3)
		if err != nil || retry.Action() != ControllerPersonaSendCurrentStatus ||
			retry.Sequence() != claim.Sequence() {
			t.Fatalf("retry recovery = (%+v, %v)", retry, err)
		}
	})

	t.Run("boundary alignment failure preserves predecessor", func(t *testing.T) {
		engine, nowMS := makePersonaActive(t, []byte{1})
		input, err := engine.ClaimInput(
			nowMS+1, GamepadInputReportV1{State: InputStateV1{A: true}})
		if err != nil {
			t.Fatal(err)
		}
		engine.global.generation = 2
		if _, err := engine.BeginUSBReset(nowMS + 2); !errors.Is(
			err, ErrInvalidTransferGeneration) {
			t.Fatalf("split-generation boundary error = %v", err)
		}
		if !engine.hasClaim || engine.claimToken != input.token ||
			engine.usb.Snapshot().Generation != 1 || engine.input.Generation() != 1 {
			t.Fatalf("failed boundary retired predecessor: %+v", engine.Snapshot())
		}
		engine.global.generation = 1
		reset, err := engine.BeginUSBReset(nowMS + 2)
		if err != nil || reset.Generation() != 2 {
			t.Fatalf("boundary recovery = (%+v, %v)", reset, err)
		}
		if engine.usb.Snapshot().Generation != 2 || engine.global.Generation() != 2 ||
			engine.input.Generation() != 2 {
			t.Fatalf("boundary generations = %+v", engine.Snapshot())
		}
	})

	t.Run("clear epoch exhaustion precedes boundary", func(t *testing.T) {
		engine, nowMS := makePersonaActive(t, []byte{1})
		engine.nextClearEpoch = ^uint64(0)
		if _, err := engine.BeginUSBReset(nowMS + 1); !errors.Is(
			err, ErrInvalidTransferEpoch) {
			t.Fatalf("clear epoch error = %v", err)
		}
		if got := engine.Snapshot(); got.Generation != 1 ||
			got.USBState != USBControlDeviceConfigured || got.ClaimOutstanding {
			t.Fatalf("clear exhaustion partially advanced boundary: %+v", got)
		}
	})
}

func TestControllerPersonaTerminalBoundaryOverflowFailsBeforeAdmission(t *testing.T) {
	engine, nowMS := makePersonaActive(t, []byte{1})
	reset := claimPersonaHostAction(t, engine, nowMS+1,
		[]byte{0x05, 0x20, 0x03, 0x01, byte(SetDeviceStateReset)})
	deliverPersonaClaim(t, engine, reset, nowMS+2)
	clearClaim, _ := engine.ClaimNextLifecycleAction(nowMS + 3)
	deliverPersonaClaim(t, engine, clearClaim, nowMS+4)
	status, _ := engine.ClaimNextLifecycleAction(nowMS + 5)
	deliverPersonaClaim(t, engine, status, nowMS+6)

	engine.generation = ^uint64(0)
	engine.usb.generation = ^uint64(0)
	engine.global.generation = ^uint64(0)
	engine.input.generation = ^uint64(0)
	engine.lifecycle.core.generation = ^uint64(0)
	perform, present, err := engine.ClaimPoll(nowMS + 506)
	if err != nil || !present || perform.Action() != ControllerPersonaPerformReset {
		t.Fatalf("terminal action = (%+v, %t, %v)", perform, present, err)
	}
	if err := engine.AdmitAndCopy(perform, nil, nowMS+506); !errors.Is(
		err, ErrInvalidTransferGeneration) {
		t.Fatalf("overflow admission error = %v", err)
	}
	if got := engine.Snapshot(); got.ClaimAdmitted ||
		engine.lifecycle.Snapshot().ClaimAdmitted {
		t.Fatalf("overflow crossed admission: %+v", got)
	}
}

func TestControllerPersonaErrorsBlockersAndAllocation(t *testing.T) {
	var zero ControllerPersonaEngine
	if _, _, err := zero.ClaimPoll(0); !errors.Is(err, ErrUninitializedControllerPersona) {
		t.Fatalf("zero engine error = %v", err)
	}
	engine, _ := makePersonaActive(t, []byte{1, 2, 3})
	wantBlockers := controllerPersonaKnownBlockers
	if got := engine.CapabilityBlockers(); got != wantBlockers {
		t.Fatalf("blockers = 0x%x, want 0x%x", got, wantBlockers)
	}
	if _, _, err := engine.ReceiveHostMessage(8, []byte{0x06, 0x20, 0x01, 0x00}); !errors.Is(err, ErrUnsupportedControllerPersonaHostMessage) {
		t.Fatalf("unsupported message error = %v", err)
	}

	report := GamepadInputReportV1{State: InputStateV1{A: true}}
	wire := [GamepadInputMessageSize]byte{}
	if allocs := testing.AllocsPerRun(1000, func() {
		claim, err := engine.ClaimInput(8, report)
		if err != nil {
			panic(err)
		}
		if err := engine.AdmitAndCopy(claim, wire[:], 8); err != nil {
			panic(err)
		}
		if err := engine.Resolve(claim, ControllerPersonaDelivered, 8); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("persona input allocations = %v, want 0", allocs)
	}

	motorWire := [DirectMotorMessageSize]byte{}
	if err := EncodeDirectMotorMessageInto(
		motorWire[:], 1, testDirectMotorBody()); err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		claim, disposition, err := engine.ReceiveHostMessage(8, motorWire[:])
		if err != nil || disposition != ControllerPersonaHostActionClaimed {
			panic(err)
		}
		if err := engine.AdmitAndCopy(claim, nil, 8); err != nil {
			panic(err)
		}
		if err := engine.Resolve(claim, ControllerPersonaDelivered, 8); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("persona feedback allocations = %v, want 0", allocs)
	}
}

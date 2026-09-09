package xboxone

import (
	"errors"
	"testing"
	"time"
)

func newTestControllerPersonaTransportCoordinator(
	t *testing.T,
	metadata []byte,
) *controllerPersonaTransportCoordinator {
	t.Helper()
	engine := newTestControllerPersonaEngine(t, metadata, 0)
	coordinator, err := newControllerPersonaTransportCoordinator(engine)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func admitAndCompleteTestControl(
	t *testing.T,
	coordinator *controllerPersonaTransportCoordinator,
	setup []byte,
	nowMS uint64,
) {
	t.Helper()
	ticket, err := coordinator.stageControl(setup)
	if err != nil {
		t.Fatal(err)
	}
	var scratch [ControllerPersonaMaximumWireSize]byte
	admission, err := coordinator.admit(ticket, scratch[:], nowMS)
	if err != nil {
		t.Fatal(err)
	}
	if admission.disposition != controllerPersonaTransportControlResponse {
		t.Fatalf("control admission = %+v", admission)
	}
	if err := coordinator.completeResponse(ticket, true, nowMS); err != nil {
		t.Fatal(err)
	}
}

func configureTestControllerPersonaTransport(
	t *testing.T,
	coordinator *controllerPersonaTransportCoordinator,
) {
	t.Helper()
	const nowMS uint64 = 0
	admitAndCompleteTestControl(t, coordinator,
		testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetAddress, 1, 0, 0),
		nowMS)
	admitAndCompleteTestControl(t, coordinator,
		testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration,
			uint16(usbConfigurationGIP), 0, 0), nowMS)
}

func admitAndCompleteTestIN(
	t *testing.T,
	coordinator *controllerPersonaTransportCoordinator,
	nowMS uint64,
) (controllerPersonaTransportAdmission, []byte) {
	t.Helper()
	ticket, err := coordinator.stageInterruptIn(
		ControllerPersonaMaximumWireSize)
	if err != nil {
		t.Fatal(err)
	}
	var scratch [ControllerPersonaMaximumWireSize]byte
	admission, err := coordinator.admit(ticket, scratch[:], nowMS)
	if err != nil {
		t.Fatal(err)
	}
	if admission.disposition != controllerPersonaTransportInterruptInResponse {
		t.Fatalf("interrupt-IN admission = %+v", admission)
	}
	wire := append([]byte(nil), scratch[:admission.size]...)
	if err := coordinator.completeResponse(ticket, true, nowMS); err != nil {
		t.Fatal(err)
	}
	return admission, wire
}

func admitAndCompleteTestSemanticIN(
	t *testing.T,
	coordinator *controllerPersonaTransportCoordinator,
	nowMS uint64,
	input GamepadInputReportV1,
) (controllerPersonaTransportAdmission, []byte) {
	t.Helper()
	ticket, err := coordinator.stageInterruptIn(
		ControllerPersonaMaximumWireSize)
	if err != nil {
		t.Fatal(err)
	}
	var scratch [ControllerPersonaMaximumWireSize]byte
	admission, err := coordinator.admitInput(ticket, scratch[:], nowMS, input)
	if err != nil {
		t.Fatal(err)
	}
	if admission.disposition != controllerPersonaTransportInterruptInResponse {
		t.Fatalf("semantic interrupt-IN admission = %+v", admission)
	}
	wire := append([]byte(nil), scratch[:admission.size]...)
	if err := coordinator.completeResponse(ticket, true, nowMS); err != nil {
		t.Fatal(err)
	}
	return admission, wire
}

func deliverTestCoordinatorHello(
	t *testing.T,
	coordinator *controllerPersonaTransportCoordinator,
) {
	t.Helper()
	const nowMS uint64 = 1
	admission, wire := admitAndCompleteTestIN(t, coordinator, nowMS)
	if admission.action != ControllerPersonaSendHello {
		t.Fatalf("first IN action = %d", admission.action)
	}
	if _, err := DecodeHelloMessage(wire); err != nil {
		t.Fatalf("Hello wire: %v", err)
	}
}

func makeTestCoordinatorActive(
	t *testing.T,
	coordinator *controllerPersonaTransportCoordinator,
) {
	t.Helper()
	configureTestControllerPersonaTransport(t, coordinator)
	deliverTestCoordinatorHello(t, coordinator)
	startTicket, err := coordinator.stageInterruptOut(
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.admit(startTicket, nil, 2); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.completeResponse(startTicket, true, 2); err != nil {
		t.Fatal(err)
	}
	current, _ := admitAndCompleteTestIN(t, coordinator, 3)
	if current.action != ControllerPersonaSendCurrentStatus {
		t.Fatalf("current status = %+v", current)
	}
	initial, _ := admitAndCompleteTestSemanticIN(
		t, coordinator, 4, GamepadInputReportV1{})
	if initial.action != ControllerPersonaSendInitialInput {
		t.Fatalf("initial input = %+v", initial)
	}
	localTrigger, err := coordinator.stageInterruptIn(
		ControllerPersonaMaximumWireSize)
	if err != nil {
		t.Fatal(err)
	}
	var scratch [ControllerPersonaMaximumWireSize]byte
	localRequired, err := coordinator.admit(localTrigger, scratch[:], 5)
	if err != nil || localRequired.disposition !=
		controllerPersonaTransportLocalRequired ||
		localRequired.action != ControllerPersonaPermitNormalUpstream {
		t.Fatalf("permit trigger = (%+v, %v)", localRequired, err)
	}
	if err := coordinator.retire(localTrigger); err != nil {
		t.Fatal(err)
	}
	permit, present, err := coordinator.admitLocal(5)
	if err != nil || !present ||
		permit.action != ControllerPersonaPermitNormalUpstream {
		t.Fatalf("permit = (%+v, %t, %v)", permit, present, err)
	}
	if err := coordinator.completeLocal(
		permit, ControllerPersonaDelivered, 5); err != nil {
		t.Fatal(err)
	}
	if got, _ := coordinator.snapshot(); !got.NormalUpstream ||
		got.LifecycleState != ControllerLifecycleActive {
		t.Fatalf("active coordinator = %+v", got)
	}
}

func testCoordinatorDirectMotorWire(t *testing.T, sequence uint8, body RumbleBodyV1) []byte {
	t.Helper()
	wire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(wire, sequence, body); err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestControllerPersonaTransportStagingCannotPreclaimOrChooseOrder(t *testing.T) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	before, ok := coordinator.snapshot()
	if !ok {
		t.Fatal("coordinator snapshot unavailable")
	}
	inTicket, err := coordinator.stageInterruptIn(
		ControllerPersonaMaximumWireSize)
	if err != nil {
		t.Fatal(err)
	}
	outTicket, err := coordinator.stageInterruptOut(
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
	if err != nil {
		t.Fatal(err)
	}
	controlTicket, err := coordinator.stageControl(
		testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetAddress, 1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	afterStage, _ := coordinator.snapshot()
	if afterStage != before || afterStage.ClaimOutstanding {
		t.Fatalf("staging changed canonical engine: before=%+v after=%+v",
			before, afterStage)
	}

	var scratch [ControllerPersonaMaximumWireSize]byte
	control, err := coordinator.admit(controlTicket, scratch[:], 0)
	if err != nil {
		t.Fatal(err)
	}
	if control.disposition != controllerPersonaTransportControlResponse ||
		control.action != ControllerPersonaUSBControl || control.order == 0 {
		t.Fatalf("serializer-first control = %+v", control)
	}
	if err := coordinator.completeResponse(controlTicket, true, 0); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.retire(inTicket); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.retire(outTicket); err != nil {
		t.Fatal(err)
	}
}

func TestControllerPersonaTransportOUTWaitsBehindMandatoryEgress(t *testing.T) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	configureTestControllerPersonaTransport(t, coordinator)
	motor := testDirectMotorBody()
	outTicket, err := coordinator.stageInterruptOut(
		testCoordinatorDirectMotorWire(t, 0x41, motor))
	if err != nil {
		t.Fatal(err)
	}

	admission, err := coordinator.admit(outTicket, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if admission.disposition != controllerPersonaTransportWait ||
		admission.waitReason != controllerPersonaTransportUpstreamPending ||
		admission.action != ControllerPersonaSendHello {
		t.Fatalf("OUT did not retain behind Hello: %+v", admission)
	}
	if got, _ := coordinator.snapshot(); !got.ClaimOutstanding ||
		got.Feedback.DirectMotor != (RumbleBodyV1{}) {
		t.Fatalf("blocked OUT state = %+v", got)
	}

	hello, _ := admitAndCompleteTestIN(t, coordinator, 2)
	if hello.action != ControllerPersonaSendHello {
		t.Fatalf("IN did not own predecessor: %+v", hello)
	}
	admission, err = coordinator.admit(outTicket, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if admission.disposition != controllerPersonaTransportInterruptOutResponse ||
		admission.action != ControllerPersonaApplyDirectMotor {
		t.Fatalf("retained OUT admission = %+v", admission)
	}
	if err := coordinator.completeResponse(outTicket, true, 3); err != nil {
		t.Fatal(err)
	}
	if got, _ := coordinator.snapshot(); got.Feedback.DirectMotor != (RumbleBodyV1{}) ||
		!got.ClaimOutstanding || got.ClaimAdmitted {
		t.Fatalf("OUT ACK incorrectly delivered local work: %+v", got)
	}
	lease, present, err := coordinator.admitLocal(4)
	if err != nil || !present || lease.action != ControllerPersonaApplyDirectMotor ||
		lease.directMotor != motor {
		t.Fatalf("motor lease = (%+v, %t, %v)", lease, present, err)
	}
	if err := coordinator.completeLocal(
		lease, ControllerPersonaDelivered, 5); err != nil {
		t.Fatal(err)
	}
	if got, _ := coordinator.snapshot(); got.Feedback.DirectMotor != motor ||
		got.ClaimOutstanding {
		t.Fatalf("delivered motor state = %+v", got)
	}
}

func TestControllerPersonaTransportLocalConsumerCannotPreclaimUpstream(
	t *testing.T,
) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	configureTestControllerPersonaTransport(t, coordinator)
	before, _ := coordinator.snapshot()
	orderBefore := coordinator.nextOrder
	if lease, present, err := coordinator.admitLocal(1); err != nil || present ||
		lease.valid() {
		t.Fatalf("unprompted local admission = (%+v, %t, %v)", lease, present, err)
	}
	afterLocal, _ := coordinator.snapshot()
	if afterLocal != before || coordinator.pending.claim.Valid() ||
		coordinator.nextOrder != orderBefore {
		t.Fatalf("local consumer selected upstream: before=%+v after=%+v pending=%+v",
			before, afterLocal, coordinator.pending)
	}

	outTicket, err := coordinator.stageInterruptOut(
		testCoordinatorDirectMotorWire(t, 0x40, testDirectMotorBody()))
	if err != nil {
		t.Fatal(err)
	}
	selected, err := coordinator.admit(outTicket, nil, 1)
	if err != nil || selected.disposition != controllerPersonaTransportWait ||
		selected.waitReason != controllerPersonaTransportUpstreamPending ||
		selected.action != ControllerPersonaSendHello ||
		!coordinator.pending.claim.Valid() {
		t.Fatalf("serializer did not select Hello: (%+v, %v)", selected, err)
	}
	if err := coordinator.retire(outTicket); err != nil {
		t.Fatal(err)
	}
	hello, _ := admitAndCompleteTestIN(t, coordinator, 2)
	if hello.action != ControllerPersonaSendHello {
		t.Fatalf("selected upstream action = %+v", hello)
	}
}

func TestControllerPersonaTransportSTARTTransfersWireToINWithoutACKResolution(
	t *testing.T,
) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	configureTestControllerPersonaTransport(t, coordinator)
	deliverTestCoordinatorHello(t, coordinator)
	startTicket, err := coordinator.stageInterruptOut(
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
	if err != nil {
		t.Fatal(err)
	}
	startAdmission, err := coordinator.admit(startTicket, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if startAdmission.disposition != controllerPersonaTransportInterruptOutResponse ||
		startAdmission.action != ControllerPersonaSendCurrentStatus {
		t.Fatalf("START admission = %+v", startAdmission)
	}
	if err := coordinator.completeResponse(startTicket, true, 3); err != nil {
		t.Fatal(err)
	}
	afterACK, _ := coordinator.snapshot()
	if !afterACK.ClaimOutstanding || afterACK.ClaimAdmitted ||
		afterACK.LifecycleState != ControllerLifecycleArrival ||
		afterACK.GlobalSequence != 1 {
		t.Fatalf("START ACK resolved upstream action: %+v", afterACK)
	}

	inAdmission, wire := admitAndCompleteTestIN(t, coordinator, 4)
	if inAdmission.action != ControllerPersonaSendCurrentStatus ||
		len(wire) != ExtendedStatusNoEventsMessageSize {
		t.Fatalf("START upstream delivery = %+v, %d bytes", inAdmission, len(wire))
	}
	afterIN, _ := coordinator.snapshot()
	if afterIN.ClaimOutstanding || afterIN.GlobalSequence != 2 ||
		afterIN.LifecycleState != ControllerLifecycleArrival {
		t.Fatalf("post-current-status state = %+v", afterIN)
	}
}

func TestControllerPersonaTransportINFailureRetainsByteExactRetryAheadOfEP0(
	t *testing.T,
) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	configureTestControllerPersonaTransport(t, coordinator)
	deliverTestCoordinatorHello(t, coordinator)
	startTicket, err := coordinator.stageInterruptOut(
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.admit(startTicket, nil, 2); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.completeResponse(startTicket, true, 2); err != nil {
		t.Fatal(err)
	}

	firstTicket, err := coordinator.stageInterruptIn(
		ControllerPersonaMaximumWireSize)
	if err != nil {
		t.Fatal(err)
	}
	var firstScratch [ControllerPersonaMaximumWireSize]byte
	first, err := coordinator.admit(firstTicket, firstScratch[:], 3)
	if err != nil {
		t.Fatal(err)
	}
	if first.action != ControllerPersonaSendCurrentStatus {
		t.Fatalf("first IN = %+v", first)
	}
	firstWire := append([]byte(nil), firstScratch[:first.size]...)
	if err := coordinator.completeResponse(firstTicket, false, 4); err != nil {
		t.Fatal(err)
	}
	if got, _ := coordinator.snapshot(); !got.RetryPending || got.GlobalSequence != 1 {
		t.Fatalf("failed IN did not preserve retry: %+v", got)
	}

	controlTicket, err := coordinator.stageControl(
		testUSBSetup(usbRequestTypeDeviceIn, usbRequestGetConfiguration, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	var controlScratch [ControllerPersonaMaximumWireSize]byte
	blocked, err := coordinator.admit(controlTicket, controlScratch[:], 5)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.disposition != controllerPersonaTransportWait ||
		blocked.waitReason != controllerPersonaTransportUpstreamPending ||
		blocked.action != ControllerPersonaSendCurrentStatus {
		t.Fatalf("EP0 overtook retry: %+v", blocked)
	}
	if err := coordinator.retire(controlTicket); err != nil {
		t.Fatal(err)
	}

	retryTicket, err := coordinator.stageInterruptIn(
		ControllerPersonaMaximumWireSize)
	if err != nil {
		t.Fatal(err)
	}
	var retryScratch [ControllerPersonaMaximumWireSize]byte
	retry, err := coordinator.admit(retryTicket, retryScratch[:], 6)
	if err != nil {
		t.Fatal(err)
	}
	if retry.action != first.action || retry.size != len(firstWire) ||
		string(retryScratch[:retry.size]) != string(firstWire) {
		t.Fatalf("retry changed: first=% x retry=% x",
			firstWire, retryScratch[:retry.size])
	}
	if err := coordinator.completeResponse(retryTicket, true, 7); err != nil {
		t.Fatal(err)
	}
}

func TestControllerPersonaTransportLocalRetryCannotBeOvertaken(t *testing.T) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	configureTestControllerPersonaTransport(t, coordinator)
	deliverTestCoordinatorHello(t, coordinator)
	firstMotor := testDirectMotorBody()
	firstTicket, err := coordinator.stageInterruptOut(
		testCoordinatorDirectMotorWire(t, 0x31, firstMotor))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.admit(firstTicket, nil, 2); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.completeResponse(firstTicket, true, 2); err != nil {
		t.Fatal(err)
	}
	firstLease, present, err := coordinator.admitLocal(3)
	if err != nil || !present {
		t.Fatalf("first local = (%t, %v)", present, err)
	}
	if err := coordinator.completeLocal(
		firstLease, ControllerPersonaDeferred, 4); err != nil {
		t.Fatal(err)
	}

	secondMotor := firstMotor
	secondMotor.LeftVibration = firstMotor.LeftVibration + 1
	secondTicket, err := coordinator.stageInterruptOut(
		testCoordinatorDirectMotorWire(t, 0x32, secondMotor))
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := coordinator.admit(secondTicket, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.disposition != controllerPersonaTransportLocalRequired ||
		blocked.waitReason != controllerPersonaTransportLocalPending ||
		blocked.action != ControllerPersonaApplyDirectMotor {
		t.Fatalf("second OUT overtook local retry: %+v", blocked)
	}
	retryLease, present, err := coordinator.admitLocal(6)
	if err != nil || !present || retryLease.directMotor != firstMotor {
		t.Fatalf("local retry = (%+v, %t, %v)", retryLease, present, err)
	}
	if err := coordinator.completeLocal(
		retryLease, ControllerPersonaDelivered, 7); err != nil {
		t.Fatal(err)
	}

	secondAdmission, err := coordinator.admit(secondTicket, nil, 8)
	if err != nil || secondAdmission.disposition !=
		controllerPersonaTransportInterruptOutResponse {
		t.Fatalf("second OUT after retry = (%+v, %v)", secondAdmission, err)
	}
	if err := coordinator.completeResponse(secondTicket, true, 8); err != nil {
		t.Fatal(err)
	}
	secondLease, present, err := coordinator.admitLocal(9)
	if err != nil || !present || secondLease.directMotor != secondMotor {
		t.Fatalf("second local = (%+v, %t, %v)", secondLease, present, err)
	}
	if err := coordinator.completeLocal(
		secondLease, ControllerPersonaDelivered, 10); err != nil {
		t.Fatal(err)
	}
}

func TestControllerPersonaTransportMetadataACKUsesLocalDeliveryBoundary(t *testing.T) {
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
	begin := claimPersonaHostAction(t, engine, 2,
		[]byte{0x04, 0x20, 0x01, 0x00})
	deliverPersonaClaim(t, engine, begin, 3)
	packet, err := engine.ClaimMetadataPacket(4)
	if err != nil || packet.MetadataKind() != MetadataPacketInitialFragment {
		t.Fatalf("metadata packet = (%+v, %v)", packet, err)
	}
	deliverPersonaClaim(t, engine, packet, 5)
	coordinator, err := newControllerPersonaTransportCoordinator(engine)
	if err != nil {
		t.Fatal(err)
	}
	body := ProtocolControlACKBodyV1{
		ReferencedDataClass: DataClassCommand, ReferencedMessageNumber: messageNumberMetadataRequest,
		ReferencedSystem: true, FragmentOffset: 58, RemainingBuffer: 512,
	}
	var wire [ProtocolControlACKMessageSize]byte
	if err := EncodeProtocolControlACKMessageInto(wire[:], 2, body); err != nil {
		t.Fatal(err)
	}
	ticket, err := coordinator.stageInterruptOut(wire[:])
	if err != nil {
		t.Fatal(err)
	}
	admission, err := coordinator.admit(ticket, nil, 6)
	if err != nil {
		t.Fatal(err)
	}
	if admission.hostDisposition != ControllerPersonaHostAcknowledgementProgress ||
		admission.action != ControllerPersonaApplyMetadataAcknowledgement {
		t.Fatalf("ACK admission = %+v", admission)
	}
	if err := coordinator.completeResponse(ticket, true, 6); err != nil {
		t.Fatal(err)
	}
	coordinator.mu.Lock()
	afterOUT := coordinator.engine.metadataTransfer.Snapshot()
	coordinator.mu.Unlock()
	if afterOUT.AcknowledgedEnd != 0 || !afterOUT.AwaitingAcknowledgement {
		t.Fatalf("OUT ACK advanced metadata: %+v", afterOUT)
	}
	lease, present, err := coordinator.admitLocal(6)
	if err != nil || !present ||
		lease.action != ControllerPersonaApplyMetadataAcknowledgement {
		t.Fatalf("ACK local lease = (%+v, %t, %v)", lease, present, err)
	}
	if err := coordinator.completeLocal(
		lease, ControllerPersonaDelivered, 6); err != nil {
		t.Fatal(err)
	}
	coordinator.mu.Lock()
	afterLocal := coordinator.engine.metadataTransfer.Snapshot()
	coordinator.mu.Unlock()
	if afterLocal.AcknowledgedEnd != 58 || afterLocal.AwaitingAcknowledgement {
		t.Fatalf("local ACK did not advance metadata: %+v", afterLocal)
	}
}

func TestControllerPersonaTransportMetadataPacketPrecedesNewOUT(t *testing.T) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{0xaa})
	configureTestControllerPersonaTransport(t, coordinator)
	deliverTestCoordinatorHello(t, coordinator)
	request, err := coordinator.stageInterruptOut([]byte{0x04, 0x20, 0x01, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	requestAdmission, err := coordinator.admit(request, nil, 2)
	if err != nil || requestAdmission.action != ControllerPersonaBeginMetadata {
		t.Fatalf("metadata request = (%+v, %v)", requestAdmission, err)
	}
	if err := coordinator.completeResponse(request, true, 2); err != nil {
		t.Fatal(err)
	}
	begin, present, err := coordinator.admitLocal(3)
	if err != nil || !present || begin.action != ControllerPersonaBeginMetadata {
		t.Fatalf("begin metadata = (%+v, %t, %v)", begin, present, err)
	}
	if err := coordinator.completeLocal(
		begin, ControllerPersonaDelivered, 3); err != nil {
		t.Fatal(err)
	}

	motorTicket, err := coordinator.stageInterruptOut(
		testCoordinatorDirectMotorWire(t, 0x61, testDirectMotorBody()))
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := coordinator.admit(motorTicket, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.disposition != controllerPersonaTransportWait ||
		blocked.waitReason != controllerPersonaTransportUpstreamPending ||
		blocked.action != ControllerPersonaSendMetadata {
		t.Fatalf("OUT overtook metadata packet: %+v", blocked)
	}
	metadata, wire := admitAndCompleteTestIN(t, coordinator, 5)
	if metadata.action != ControllerPersonaSendMetadata || len(wire) == 0 {
		t.Fatalf("metadata IN = %+v, wire=% x", metadata, wire)
	}
	accepted, err := coordinator.admit(motorTicket, nil, 6)
	if err != nil || accepted.disposition !=
		controllerPersonaTransportInterruptOutResponse ||
		accepted.action != ControllerPersonaApplyDirectMotor {
		t.Fatalf("OUT after metadata = (%+v, %v)", accepted, err)
	}
	if err := coordinator.completeResponse(motorTicket, true, 6); err != nil {
		t.Fatal(err)
	}
}

func TestControllerPersonaTransportFailedOUTIsFencedUntilGenerationReset(
	t *testing.T,
) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	configureTestControllerPersonaTransport(t, coordinator)
	deliverTestCoordinatorHello(t, coordinator)
	motor := testDirectMotorBody()
	ticket, err := coordinator.stageInterruptOut(
		testCoordinatorDirectMotorWire(t, 0x55, motor))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.admit(ticket, nil, 2); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.completeResponse(ticket, false, 3); err != nil {
		t.Fatal(err)
	}
	afterFailure, _ := coordinator.snapshot()
	orderAfterFailure := coordinator.nextOrder
	if !afterFailure.RetryPending || afterFailure.ClaimOutstanding ||
		afterFailure.Feedback.DirectMotor != (RumbleBodyV1{}) {
		t.Fatalf("failed OUT state = %+v", afterFailure)
	}

	inTicket, err := coordinator.stageInterruptIn(
		ControllerPersonaMaximumWireSize)
	if err != nil {
		t.Fatal(err)
	}
	var scratch [ControllerPersonaMaximumWireSize]byte
	for index := range scratch {
		scratch[index] = 0xa5
	}
	blocked, err := coordinator.admitInput(
		inTicket, scratch[:], 4,
		GamepadInputReportV1{State: InputStateV1{Y: true}})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.disposition != controllerPersonaTransportWait ||
		blocked.waitReason != controllerPersonaTransportBoundaryRequired ||
		blocked.action != ControllerPersonaApplyDirectMotor {
		t.Fatalf("failed OUT retry was not fenced: %+v", blocked)
	}
	controlTicket, err := coordinator.stageControl(
		testUSBSetup(usbRequestTypeDeviceIn,
			usbRequestGetConfiguration, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	blockedControl, err := coordinator.admit(controlTicket, scratch[:], 4)
	if err != nil || blockedControl.disposition != controllerPersonaTransportWait ||
		blockedControl.waitReason != controllerPersonaTransportBoundaryRequired ||
		blockedControl.action != ControllerPersonaApplyDirectMotor {
		t.Fatalf("failed OUT fence allowed EP0: (%+v, %v)", blockedControl, err)
	}
	if err := coordinator.retire(controlTicket); err != nil {
		t.Fatal(err)
	}
	secondOUT, err := coordinator.stageInterruptOut(
		testCoordinatorDirectMotorWire(t, 0x56, motor))
	if err != nil {
		t.Fatal(err)
	}
	blockedOUT, err := coordinator.admit(secondOUT, nil, 4)
	if err != nil || blockedOUT.disposition != controllerPersonaTransportWait ||
		blockedOUT.waitReason != controllerPersonaTransportBoundaryRequired ||
		blockedOUT.action != ControllerPersonaApplyDirectMotor {
		t.Fatalf("failed OUT fence allowed second OUT: (%+v, %v)", blockedOUT, err)
	}
	if err := coordinator.retire(secondOUT); err != nil {
		t.Fatal(err)
	}
	if _, present, err := coordinator.admitLocal(4); present || !errors.Is(
		err, errControllerPersonaTransportBoundaryRequired) {
		t.Fatalf("failed OUT local retry escaped: present=%t err=%v", present, err)
	}
	for index, value := range scratch {
		if value != 0xa5 {
			t.Fatalf("fenced admission wrote scratch byte %d = %02x", index, value)
		}
	}
	afterFences, _ := coordinator.snapshot()
	if afterFences != afterFailure || coordinator.nextOrder != orderAfterFailure ||
		coordinator.pending.claim.Valid() {
		t.Fatalf("fenced admission mutated state: before=%+v after=%+v pending=%+v",
			afterFailure, afterFences, coordinator.pending)
	}

	if err := coordinator.beginUSBReset(5); err != nil {
		t.Fatal(err)
	}
	afterReset, _ := coordinator.snapshot()
	if afterReset.Generation != 2 || afterReset.RetryPending ||
		!afterReset.ClaimOutstanding ||
		afterReset.Feedback.DirectMotor != (RumbleBodyV1{}) {
		t.Fatalf("reset did not retire failed command: %+v", afterReset)
	}
	if _, err := coordinator.admit(inTicket, scratch[:], 5); !errors.Is(
		err, errInvalidControllerPersonaTransportTicket) {
		t.Fatalf("pre-reset ticket admission error = %v", err)
	}
	if err := coordinator.retire(inTicket); !errors.Is(
		err, errInvalidControllerPersonaTransportTicket) {
		t.Fatalf("pre-reset ticket retirement error = %v", err)
	}
	clearLease, present, err := coordinator.admitLocal(6)
	if err != nil || !present || clearLease.action != ControllerPersonaClearOutputs {
		t.Fatalf("reset clear = (%+v, %t, %v)", clearLease, present, err)
	}
	if err := coordinator.completeLocal(
		clearLease, ControllerPersonaDelivered, 7); err != nil {
		t.Fatal(err)
	}
	if got, _ := coordinator.snapshot(); got.Feedback.DirectMotor !=
		(RumbleBodyV1{}) || got.RetryPending || got.ClaimOutstanding {
		t.Fatalf("failed motor became visible after reset: %+v", got)
	}
}

func TestControllerPersonaTransportConfigurationLossRoutesOneDeliveredClear(
	t *testing.T,
) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	makeTestCoordinatorActive(t, coordinator)
	motorBody := testDirectMotorBody()
	motorTicket, err := coordinator.stageInterruptOut(
		testCoordinatorDirectMotorWire(t, 0x67, motorBody))
	if err != nil {
		t.Fatal(err)
	}
	motorAdmission, err := coordinator.admit(motorTicket, nil, 6)
	if err != nil || motorAdmission.disposition !=
		controllerPersonaTransportInterruptOutResponse {
		t.Fatalf("motor admission = (%+v, %v)", motorAdmission, err)
	}
	if err := coordinator.completeResponse(motorTicket, true, 6); err != nil {
		t.Fatal(err)
	}
	motorLease, present, err := coordinator.admitLocal(6)
	if err != nil || !present ||
		motorLease.action != ControllerPersonaApplyDirectMotor {
		t.Fatalf("motor lease = (%+v, %t, %v)", motorLease, present, err)
	}
	if err := coordinator.completeLocal(
		motorLease, ControllerPersonaDelivered, 6); err != nil {
		t.Fatal(err)
	}

	controlTicket, err := coordinator.stageControl(testUSBSetup(
		usbRequestTypeDeviceOut, usbRequestSetConfiguration,
		uint16(usbConfigurationUnselected), 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	controlAdmission, err := coordinator.admit(controlTicket, nil, 7)
	if err != nil || controlAdmission.disposition !=
		controllerPersonaTransportControlResponse {
		t.Fatalf("unconfigure admission = (%+v, %v)", controlAdmission, err)
	}
	if coordinator.pendingLocal() {
		t.Fatal("configuration clear became pending before EP0 delivery")
	}
	if err := coordinator.completeResponse(controlTicket, true, 7); err != nil {
		t.Fatal(err)
	}
	if !coordinator.pendingLocal() {
		t.Fatal("delivered configuration loss did not route a local clear")
	}
	if got, _ := coordinator.snapshot(); got.USBState != USBControlDeviceAddressed ||
		got.Feedback.DirectMotor != motorBody || !got.ClaimOutstanding ||
		got.Feedback.ClearEpoch != 0 {
		t.Fatalf("configuration loss committed output early = %+v", got)
	}
	if staleLease, present, err := coordinator.admitLocal(6); !errors.Is(
		err, ErrNonMonotonicControllerPersonaClock) || present || staleLease.valid() {
		t.Fatalf("stale clear admission = (%+v, %t, %v)", staleLease, present, err)
	}
	if !coordinator.pendingLocalClear() {
		t.Fatal("stale local clock discarded the selected clear")
	}
	beforeBoundary, _ := coordinator.snapshot()
	if err := coordinator.beginUSBReset(8); !errors.Is(
		err, ErrControllerPersonaBoundaryBlocked) {
		t.Fatalf("pending configuration clear reset fence = %v", err)
	}
	if err := coordinator.beginDisconnect(8); !errors.Is(
		err, ErrControllerPersonaBoundaryBlocked) {
		t.Fatalf("pending configuration clear disconnect fence = %v", err)
	}
	if err := coordinator.reconnect(8); !errors.Is(
		err, ErrControllerPersonaBoundaryBlocked) {
		t.Fatalf("pending configuration clear reconnect fence = %v", err)
	}
	if afterBoundary, _ := coordinator.snapshot(); afterBoundary != beforeBoundary {
		t.Fatalf("rejected boundary changed pending clear: before=%+v after=%+v",
			beforeBoundary, afterBoundary)
	}
	unrelated, err := coordinator.stageControl(testUSBSetup(
		usbRequestTypeDeviceIn, usbRequestGetConfiguration, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	var unrelatedScratch [1]byte
	unrelatedAdmission, err := coordinator.admit(unrelated, unrelatedScratch[:], 8)
	if err != nil || unrelatedAdmission.disposition !=
		controllerPersonaTransportLocalRequired ||
		unrelatedAdmission.action != ControllerPersonaClearOutputs {
		t.Fatalf("pending clear did not fence unrelated EP0 = (%+v, %v)",
			unrelatedAdmission, err)
	}
	clearLease, present, err := coordinator.admitLocal(8)
	if err != nil || !present || clearLease.action != ControllerPersonaClearOutputs ||
		clearLease.clearEpoch == 0 {
		t.Fatalf("configuration clear lease = (%+v, %t, %v)",
			clearLease, present, err)
	}
	if err := coordinator.completeLocal(
		clearLease, ControllerPersonaDeliveryFailed, 8); err != nil {
		t.Fatal(err)
	}
	if !coordinator.pendingLocal() {
		t.Fatal("failed clear was not immediately rematerialized")
	}
	retryLease, present, err := coordinator.admitLocal(9)
	if err != nil || !present || retryLease.action != ControllerPersonaClearOutputs ||
		retryLease.generation != clearLease.generation ||
		retryLease.clearEpoch != clearLease.clearEpoch ||
		retryLease.order == clearLease.order {
		t.Fatalf("configuration clear retry = (%+v, %t, %v), predecessor %+v",
			retryLease, present, err, clearLease)
	}
	if err := coordinator.completeLocal(
		retryLease, ControllerPersonaDelivered, 9); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.retire(unrelated); err != nil {
		t.Fatal(err)
	}
	if got, _ := coordinator.snapshot(); got.Feedback.DirectMotor !=
		(RumbleBodyV1{}) || got.Feedback.ClearEpoch != retryLease.clearEpoch ||
		got.ClaimOutstanding || got.RetryPending {
		t.Fatalf("configuration clear terminal = %+v", got)
	}
}

func TestControllerPersonaTransportSTARTInitialInputUsesLatestFinalAdmissionValue(
	t *testing.T,
) {
	initial := GamepadInputReportV1{State: InputStateV1{A: true}}
	waited := GamepadInputReportV1{State: InputStateV1{B: true}}
	latest := GamepadInputReportV1{State: InputStateV1{X: true}}
	engine := newTestControllerPersonaEngine(t, []byte{1}, 0)
	if err := engine.SetCurrentInput(initial); err != nil {
		t.Fatal(err)
	}
	coordinator, err := newControllerPersonaTransportCoordinator(engine)
	if err != nil {
		t.Fatal(err)
	}
	configureTestControllerPersonaTransport(t, coordinator)
	deliverTestCoordinatorHello(t, coordinator)
	startTicket, err := coordinator.stageInterruptOut(
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.admit(startTicket, nil, 2); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.completeResponse(startTicket, true, 2); err != nil {
		t.Fatal(err)
	}
	current, _ := admitAndCompleteTestIN(t, coordinator, 3)
	if current.action != ControllerPersonaSendCurrentStatus {
		t.Fatalf("current status = %+v", current)
	}
	inTicket, err := coordinator.stageInterruptIn(
		ControllerPersonaMaximumWireSize)
	if err != nil {
		t.Fatal(err)
	}
	var inputScratch [ControllerPersonaMaximumWireSize]byte
	for index := range inputScratch {
		inputScratch[index] = 0xcc
	}
	inputRequired, err := coordinator.admit(inTicket, inputScratch[:], 4)
	if err != nil || inputRequired.disposition != controllerPersonaTransportWait ||
		inputRequired.waitReason != controllerPersonaTransportInputRequired ||
		inputRequired.action != ControllerPersonaSendInitialInput {
		t.Fatalf("missing semantic input wait = (%+v, %v)", inputRequired, err)
	}

	controlTicket, err := coordinator.stageControl(
		testUSBSetup(usbRequestTypeDeviceIn,
			usbRequestGetConfiguration, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	var controlScratch [ControllerPersonaMaximumWireSize]byte
	control, err := coordinator.admit(controlTicket, controlScratch[:], 4)
	if err != nil || control.disposition != controllerPersonaTransportControlResponse {
		t.Fatalf("outstanding control = (%+v, %v)", control, err)
	}
	wait, err := coordinator.admitInput(
		inTicket, inputScratch[:], 4, waited)
	if err != nil || wait.disposition != controllerPersonaTransportWait ||
		wait.waitReason != controllerPersonaTransportResponseOutstanding {
		t.Fatalf("initial input wait = (%+v, %v)", wait, err)
	}
	for index, value := range inputScratch {
		if value != 0xcc {
			t.Fatalf("wait wrote scratch byte %d = %02x", index, value)
		}
	}
	coordinator.mu.Lock()
	lifecycleAfterWait := coordinator.engine.lifecycle.Snapshot()
	coordinator.mu.Unlock()
	if lifecycleAfterWait.ClaimOutstanding {
		t.Fatalf("wait preselected initial input: %+v", lifecycleAfterWait)
	}
	if err := coordinator.completeResponse(controlTicket, true, 4); err != nil {
		t.Fatal(err)
	}

	presentation, err := coordinator.admitInput(
		inTicket, inputScratch[:], 5, latest)
	if err != nil || presentation.disposition !=
		controllerPersonaTransportInterruptInResponse ||
		presentation.action != ControllerPersonaSendInitialInput ||
		presentation.size != GamepadInputMessageSize {
		t.Fatalf("initial input admission = (%+v, %v)", presentation, err)
	}
	sequence, decoded, err := DecodeGamepadInputMessage(
		inputScratch[:presentation.size])
	if err != nil || decoded != latest || decoded == initial || decoded == waited {
		t.Fatalf("initial report = (sequence %d, %+v, %v)", sequence, decoded, err)
	}
	want := make([]byte, GamepadInputMessageSize)
	if err := EncodeGamepadInputMessageInto(want, sequence, latest); err != nil {
		t.Fatal(err)
	}
	if string(inputScratch[:presentation.size]) != string(want) {
		t.Fatalf("initial wire = % x, want % x",
			inputScratch[:presentation.size], want)
	}
	if err := coordinator.completeResponse(inTicket, true, 5); err != nil {
		t.Fatal(err)
	}
}

func TestControllerPersonaTransportResetRetiresEveryStagedLaneAtomically(
	t *testing.T,
) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	configureTestControllerPersonaTransport(t, coordinator)
	deliverTestCoordinatorHello(t, coordinator)
	controlTicket, err := coordinator.stageControl(
		testUSBSetup(usbRequestTypeDeviceIn,
			usbRequestGetConfiguration, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	inTicket, err := coordinator.stageInterruptIn(
		ControllerPersonaMaximumWireSize)
	if err != nil {
		t.Fatal(err)
	}
	outTicket, err := coordinator.stageInterruptOut(
		testCoordinatorDirectMotorWire(t, 0x70, testDirectMotorBody()))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := coordinator.snapshot()
	orderBefore := coordinator.nextOrder
	coordinator.nextOrder = ^uint64(0)
	if err := coordinator.beginUSBReset(2); !errors.Is(
		err, errControllerPersonaTransportTokenExhausted) {
		t.Fatalf("reset order exhaustion = %v", err)
	}
	afterFailedReset, _ := coordinator.snapshot()
	if afterFailedReset != before || coordinator.controlStage.ticket != controlTicket ||
		coordinator.inStage.ticket != inTicket ||
		coordinator.outStage.ticket != outTicket || coordinator.pending.claim.Valid() {
		t.Fatalf("failed reset retired staged work: before=%+v after=%+v",
			before, afterFailedReset)
	}
	coordinator.nextOrder = orderBefore
	if err := coordinator.beginUSBReset(2); err != nil {
		t.Fatal(err)
	}
	after, _ := coordinator.snapshot()
	if after.Generation != before.Generation+1 || after.RetryPending ||
		!after.ClaimOutstanding || after.ClaimAdmitted {
		t.Fatalf("reset snapshot = %+v, before %+v", after, before)
	}

	var scratch [ControllerPersonaMaximumWireSize]byte
	if _, err := coordinator.admit(controlTicket, scratch[:], 2); !errors.Is(
		err, errInvalidControllerPersonaTransportTicket) {
		t.Fatalf("stale control admission = %v", err)
	}
	if _, err := coordinator.admitInput(
		inTicket, scratch[:], 2, GamepadInputReportV1{}); !errors.Is(
		err, errInvalidControllerPersonaTransportTicket) {
		t.Fatalf("stale IN admission = %v", err)
	}
	if _, err := coordinator.admit(outTicket, nil, 2); !errors.Is(
		err, errInvalidControllerPersonaTransportTicket) {
		t.Fatalf("stale OUT admission = %v", err)
	}
	for name, ticket := range map[string]controllerPersonaTransportTicket{
		"control": controlTicket,
		"IN":      inTicket,
		"OUT":     outTicket,
	} {
		if err := coordinator.retire(ticket); !errors.Is(
			err, errInvalidControllerPersonaTransportTicket) {
			t.Fatalf("stale %s retirement = %v", name, err)
		}
	}

	replacementControl, err := coordinator.stageControl(
		testUSBSetup(usbRequestTypeDeviceIn,
			usbRequestGetConfiguration, 0, 0, 1))
	if err != nil {
		t.Fatalf("replacement control: %v", err)
	}
	replacementIN, err := coordinator.stageInterruptIn(
		ControllerPersonaMaximumWireSize)
	if err != nil {
		t.Fatalf("replacement IN: %v", err)
	}
	replacementOUT, err := coordinator.stageInterruptOut(
		testCoordinatorDirectMotorWire(t, 0x71, testDirectMotorBody()))
	if err != nil {
		t.Fatalf("replacement OUT: %v", err)
	}
	seenTokens := make(map[uint64]bool)
	for name, ticket := range map[string]controllerPersonaTransportTicket{
		"control": replacementControl,
		"IN":      replacementIN,
		"OUT":     replacementOUT,
	} {
		if ticket.owner != coordinator || ticket.generation != after.Generation ||
			ticket.token == 0 || seenTokens[ticket.token] {
			t.Fatalf("replacement %s ticket = %+v", name, ticket)
		}
		seenTokens[ticket.token] = true
		if err := coordinator.retire(ticket); err != nil {
			t.Fatalf("replacement %s retirement: %v", name, err)
		}
	}
	clearLease, present, err := coordinator.admitLocal(3)
	if err != nil || !present || clearLease.action != ControllerPersonaClearOutputs {
		t.Fatalf("reset clear = (%+v, %t, %v)", clearLease, present, err)
	}
	if err := coordinator.completeLocal(
		clearLease, ControllerPersonaDelivered, 3); err != nil {
		t.Fatal(err)
	}
}

func TestControllerPersonaTransportInputIsSelectedAfterWaitNotAtStage(
	t *testing.T,
) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	makeTestCoordinatorActive(t, coordinator)
	inTicket, err := coordinator.stageInterruptIn(
		ControllerPersonaMaximumWireSize)
	if err != nil {
		t.Fatal(err)
	}
	motorTicket, err := coordinator.stageInterruptOut(
		testCoordinatorDirectMotorWire(t, 0x44, testDirectMotorBody()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.admit(motorTicket, nil, 6); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.completeResponse(motorTicket, true, 6); err != nil {
		t.Fatal(err)
	}
	stale := GamepadInputReportV1{State: InputStateV1{A: true}}
	var scratch [ControllerPersonaMaximumWireSize]byte
	wait, err := coordinator.admitInput(inTicket, scratch[:], 7, stale)
	if err != nil {
		t.Fatal(err)
	}
	if wait.disposition != controllerPersonaTransportLocalRequired {
		t.Fatalf("input did not wait behind local action: %+v", wait)
	}
	lease, present, err := coordinator.admitLocal(7)
	if err != nil || !present {
		t.Fatalf("motor local = (%t, %v)", present, err)
	}
	if err := coordinator.completeLocal(
		lease, ControllerPersonaDelivered, 7); err != nil {
		t.Fatal(err)
	}
	latest := GamepadInputReportV1{State: InputStateV1{B: true}}
	presentation, err := coordinator.admitInput(inTicket, scratch[:], 8, latest)
	if err != nil {
		t.Fatal(err)
	}
	if presentation.disposition != controllerPersonaTransportInterruptInResponse ||
		presentation.action != ControllerPersonaSendInput {
		t.Fatalf("latest input admission = %+v", presentation)
	}
	_, decoded, err := DecodeGamepadInputMessage(scratch[:presentation.size])
	if err != nil {
		t.Fatal(err)
	}
	if decoded != latest || decoded == stale {
		t.Fatalf("presented stale input: decoded=%+v stale=%+v latest=%+v",
			decoded, stale, latest)
	}
	if err := coordinator.completeResponse(inTicket, true, 8); err != nil {
		t.Fatal(err)
	}
}

func TestControllerPersonaTransportExhaustionNeverCreatesUntrackedClaim(
	t *testing.T,
) {
	t.Run("endpoint ticket token before staging", func(t *testing.T) {
		coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
		before, _ := coordinator.snapshot()
		coordinator.nextTicketToken = ^uint64(0)
		if _, err := coordinator.stageControl(
			testUSBSetup(usbRequestTypeDeviceIn,
				usbRequestGetConfiguration, 0, 0, 1)); !errors.Is(
			err, errControllerPersonaTransportTokenExhausted) {
			t.Fatalf("ticket token exhaustion = %v", err)
		}
		after, _ := coordinator.snapshot()
		if after != before || coordinator.controlStage.ticket.valid() ||
			coordinator.inStage.ticket.valid() || coordinator.outStage.ticket.valid() {
			t.Fatalf("ticket exhaustion changed state: before=%+v after=%+v",
				before, after)
		}
	})

	t.Run("selection order before poll", func(t *testing.T) {
		coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
		configureTestControllerPersonaTransport(t, coordinator)
		ticket, err := coordinator.stageInterruptIn(
			ControllerPersonaMaximumWireSize)
		if err != nil {
			t.Fatal(err)
		}
		coordinator.nextOrder = ^uint64(0)
		var scratch [ControllerPersonaMaximumWireSize]byte
		if _, err := coordinator.admit(ticket, scratch[:], 1); !errors.Is(
			err, errControllerPersonaTransportTokenExhausted) {
			t.Fatalf("order exhaustion error = %v", err)
		}
		got, _ := coordinator.snapshot()
		if got.ClaimOutstanding || got.ClaimAdmitted || got.RetryPending ||
			coordinator.pending.claim.Valid() {
			t.Fatalf("poll exhaustion leaked claim: %+v pending=%+v",
				got, coordinator.pending)
		}
	})

	t.Run("response order before IN admission", func(t *testing.T) {
		coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
		configureTestControllerPersonaTransport(t, coordinator)
		deliverTestCoordinatorHello(t, coordinator)
		start, err := coordinator.stageInterruptOut(
			[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.admit(start, nil, 2); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.completeResponse(start, true, 2); err != nil {
			t.Fatal(err)
		}
		coordinator.nextOrder = ^uint64(0)
		ticket, err := coordinator.stageInterruptIn(
			ControllerPersonaMaximumWireSize)
		if err != nil {
			t.Fatal(err)
		}
		var scratch [ControllerPersonaMaximumWireSize]byte
		if _, err := coordinator.admit(ticket, scratch[:], 3); !errors.Is(
			err, errControllerPersonaTransportTokenExhausted) {
			t.Fatalf("IN order exhaustion error = %v", err)
		}
		got, _ := coordinator.snapshot()
		if !got.ClaimOutstanding || got.ClaimAdmitted ||
			!coordinator.pending.claim.Valid() ||
			coordinator.activeResponse.ticket.valid() {
			t.Fatalf("IN exhaustion lost ownership: %+v pending=%+v active=%+v",
				got, coordinator.pending, coordinator.activeResponse)
		}
	})

	t.Run("local token before local admission", func(t *testing.T) {
		coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
		configureTestControllerPersonaTransport(t, coordinator)
		deliverTestCoordinatorHello(t, coordinator)
		ticket, err := coordinator.stageInterruptOut(
			testCoordinatorDirectMotorWire(t, 0x20, testDirectMotorBody()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.admit(ticket, nil, 2); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.completeResponse(ticket, true, 2); err != nil {
			t.Fatal(err)
		}
		coordinator.nextLocalToken = ^uint64(0)
		if _, present, err := coordinator.admitLocal(3); present || !errors.Is(
			err, errControllerPersonaTransportTokenExhausted) {
			t.Fatalf("local token exhaustion = (present %t, %v)", present, err)
		}
		got, _ := coordinator.snapshot()
		if !got.ClaimOutstanding || got.ClaimAdmitted ||
			!coordinator.pending.claim.Valid() ||
			coordinator.activeLocal.lease.valid() {
			t.Fatalf("local exhaustion lost ownership: %+v pending=%+v active=%+v",
				got, coordinator.pending, coordinator.activeLocal)
		}
	})

	t.Run("local order before local admission", func(t *testing.T) {
		coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
		configureTestControllerPersonaTransport(t, coordinator)
		deliverTestCoordinatorHello(t, coordinator)
		ticket, err := coordinator.stageInterruptOut(
			testCoordinatorDirectMotorWire(t, 0x21, testDirectMotorBody()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.admit(ticket, nil, 2); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.completeResponse(ticket, true, 2); err != nil {
			t.Fatal(err)
		}
		coordinator.nextOrder = ^uint64(0)
		if _, present, err := coordinator.admitLocal(3); present || !errors.Is(
			err, errControllerPersonaTransportTokenExhausted) {
			t.Fatalf("local order exhaustion = (present %t, %v)", present, err)
		}
		got, _ := coordinator.snapshot()
		if !got.ClaimOutstanding || got.ClaimAdmitted ||
			!coordinator.pending.claim.Valid() ||
			coordinator.activeLocal.lease.valid() {
			t.Fatalf("local order exhaustion lost ownership: %+v pending=%+v active=%+v",
				got, coordinator.pending, coordinator.activeLocal)
		}
	})

	t.Run("selection order before immutable retry claim", func(t *testing.T) {
		coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
		configureTestControllerPersonaTransport(t, coordinator)
		deliverTestCoordinatorHello(t, coordinator)
		ticket, err := coordinator.stageInterruptOut(
			testCoordinatorDirectMotorWire(t, 0x22, testDirectMotorBody()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.admit(ticket, nil, 2); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.completeResponse(ticket, true, 2); err != nil {
			t.Fatal(err)
		}
		lease, present, err := coordinator.admitLocal(3)
		if err != nil || !present {
			t.Fatalf("local lease = (%t, %v)", present, err)
		}
		if err := coordinator.completeLocal(
			lease, ControllerPersonaDeferred, 4); err != nil {
			t.Fatal(err)
		}
		coordinator.nextOrder = ^uint64(0)
		control, err := coordinator.stageControl(
			testUSBSetup(usbRequestTypeDeviceIn,
				usbRequestGetConfiguration, 0, 0, 1))
		if err != nil {
			t.Fatal(err)
		}
		var scratch [ControllerPersonaMaximumWireSize]byte
		if admission, err := coordinator.admit(control, scratch[:], 5); err != nil ||
			admission.disposition != controllerPersonaTransportLocalRequired {
			t.Fatalf("control must wait for the separate feedback retry: %+v, %v", admission, err)
		}
		if _, present, err := coordinator.admitLocal(5); present || !errors.Is(
			err, errControllerPersonaTransportTokenExhausted) {
			t.Fatalf("retry order exhaustion = present %t, %v", present, err)
		}
		got, _ := coordinator.snapshot()
		if got.ClaimOutstanding || got.ClaimAdmitted || got.RetryPending ||
			!got.OrdinaryFeedbackRetryPending || got.OrdinaryFeedbackAdmitted ||
			coordinator.pending.claim.Valid() {
			t.Fatalf("retry exhaustion leaked claim: %+v pending=%+v",
				got, coordinator.pending)
		}
	})
}

func TestControllerPersonaTransportRejectsForgedDuplicateAndBusyTickets(t *testing.T) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	ticket, err := coordinator.stageInterruptIn(
		ControllerPersonaMaximumWireSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.stageInterruptIn(
		ControllerPersonaMaximumWireSize); !errors.Is(
		err, errControllerPersonaTransportLaneBusy) {
		t.Fatalf("second lane selection error = %v", err)
	}
	forged := ticket
	forged.token++
	var scratch [ControllerPersonaMaximumWireSize]byte
	if _, err := coordinator.admit(forged, scratch[:], 0); !errors.Is(
		err, errInvalidControllerPersonaTransportTicket) {
		t.Fatalf("forged admission error = %v", err)
	}
	if err := coordinator.retire(ticket); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.admit(ticket, scratch[:], 0); !errors.Is(
		err, errInvalidControllerPersonaTransportTicket) {
		t.Fatalf("retired admission error = %v", err)
	}
	if got, _ := coordinator.snapshot(); got.ClaimOutstanding || got.RetryPending {
		t.Fatalf("invalid tickets changed engine: %+v", got)
	}
}

func TestControllerPersonaTransportAuthenticatesLocalLeaseCompletion(t *testing.T) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	configureTestControllerPersonaTransport(t, coordinator)
	deliverTestCoordinatorHello(t, coordinator)
	motor := testDirectMotorBody()
	ticket, err := coordinator.stageInterruptOut(
		testCoordinatorDirectMotorWire(t, 0x72, motor))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.admit(ticket, nil, 2); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.completeResponse(ticket, true, 2); err != nil {
		t.Fatal(err)
	}
	lease, present, err := coordinator.admitLocal(3)
	if err != nil || !present {
		t.Fatalf("local lease = (%+v, %t, %v)", lease, present, err)
	}
	before, _ := coordinator.snapshot()
	if before.ClaimOutstanding || before.ClaimAdmitted ||
		!before.OrdinaryFeedbackOutstanding || !before.OrdinaryFeedbackAdmitted ||
		before.Feedback.DirectMotor != (RumbleBodyV1{}) {
		t.Fatalf("pre-completion snapshot = %+v", before)
	}
	activeBefore := coordinator.activeLocal
	other := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	forgedToken := lease
	forgedToken.token++
	wrongOwner := lease
	wrongOwner.owner = other
	altered := lease
	altered.directMotor.LeftVibration++
	for name, attempt := range map[string]func() error{
		"forged token": func() error {
			return coordinator.completeLocal(
				forgedToken, ControllerPersonaDelivered, 3)
		},
		"wrong owner field": func() error {
			return coordinator.completeLocal(
				wrongOwner, ControllerPersonaDelivered, 3)
		},
		"foreign coordinator": func() error {
			return other.completeLocal(lease, ControllerPersonaDelivered, 3)
		},
		"altered payload": func() error {
			return coordinator.completeLocal(
				altered, ControllerPersonaDelivered, 3)
		},
	} {
		if err := attempt(); !errors.Is(
			err, errInvalidControllerPersonaTransportCompletion) {
			t.Fatalf("%s completion = %v", name, err)
		}
		afterAttempt, _ := coordinator.snapshot()
		if afterAttempt != before || coordinator.activeLocal != activeBefore {
			t.Fatalf("%s changed ownership: before=%+v after=%+v active=%+v",
				name, before, afterAttempt, coordinator.activeLocal)
		}
	}
	if err := coordinator.completeLocal(
		lease, ControllerPersonaDelivered, 4); err != nil {
		t.Fatal(err)
	}
	after, _ := coordinator.snapshot()
	if after.ClaimOutstanding || after.Feedback.DirectMotor != motor {
		t.Fatalf("delivered lease snapshot = %+v", after)
	}
	if err := coordinator.completeLocal(
		lease, ControllerPersonaDelivered, 4); !errors.Is(
		err, errInvalidControllerPersonaTransportCompletion) {
		t.Fatalf("duplicate local completion = %v", err)
	}
	if duplicate, _ := coordinator.snapshot(); duplicate != after {
		t.Fatalf("duplicate completion changed state: before=%+v after=%+v",
			after, duplicate)
	}
}

func TestControllerPersonaTransportRejectsUnownedConfigurationLossClear(
	t *testing.T,
) {
	engine := newTestControllerPersonaEngine(t, []byte{1}, 0)
	configurePersonaUSB(t, engine, 0)
	claim, err := engine.ClaimUSBControl(testUSBSetup(
		usbRequestTypeDeviceOut, usbRequestSetConfiguration,
		uint16(usbConfigurationUnselected), 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	deliverPersonaClaim(t, engine, claim, 1)
	if _, err := newControllerPersonaTransportCoordinator(engine); !errors.Is(
		err, errControllerPersonaTransportUninitialized) {
		t.Fatalf("coordinator adopted an unowned configuration clear: %v", err)
	}
}

func TestControllerPersonaTransportNilReceiverReturnsUninitialized(t *testing.T) {
	var coordinator *controllerPersonaTransportCoordinator
	requireUninitialized := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, errControllerPersonaTransportUninitialized) {
			t.Fatalf("%s error = %v", name, err)
		}
	}
	_, err := coordinator.stageControl(
		testUSBSetup(usbRequestTypeDeviceIn,
			usbRequestGetConfiguration, 0, 0, 1))
	requireUninitialized("stage control", err)
	_, err = coordinator.stageInterruptIn(ControllerPersonaMaximumWireSize)
	requireUninitialized("stage IN", err)
	_, err = coordinator.stageInterruptOut([]byte{1})
	requireUninitialized("stage OUT", err)
	requireUninitialized("retire", coordinator.retire(
		controllerPersonaTransportTicket{}))
	_, err = coordinator.admit(
		controllerPersonaTransportTicket{}, nil, 0)
	requireUninitialized("admit", err)
	_, err = coordinator.admitInput(
		controllerPersonaTransportTicket{}, nil, 0, GamepadInputReportV1{})
	requireUninitialized("admit input", err)
	requireUninitialized("complete response", coordinator.completeResponse(
		controllerPersonaTransportTicket{}, false, 0))
	_, _, err = coordinator.admitLocal(0)
	requireUninitialized("admit local", err)
	requireUninitialized("begin reset", coordinator.beginUSBReset(0))
	requireUninitialized("complete local", coordinator.completeLocal(
		controllerPersonaLocalLease{}, ControllerPersonaDelivered, 0))
	if _, ok := coordinator.snapshot(); ok {
		t.Fatal("nil coordinator snapshot reported available")
	}
}

func TestControllerPersonaTransportConcurrentSameInstanceInterleavings(
	t *testing.T,
) {
	t.Run("stage and retire every lane", func(t *testing.T) {
		coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
		before, _ := coordinator.snapshot()
		type stageResult struct {
			lane   controllerPersonaTransportLane
			ticket controllerPersonaTransportTicket
			err    error
		}
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for round := 0; round < 64; round++ {
			start := make(chan struct{})
			results := make(chan stageResult, 6)
			lanes := [...]controllerPersonaTransportLane{
				controllerPersonaTransportControl,
				controllerPersonaTransportControl,
				controllerPersonaTransportInterruptIn,
				controllerPersonaTransportInterruptIn,
				controllerPersonaTransportInterruptOut,
				controllerPersonaTransportInterruptOut,
			}
			for _, lane := range lanes {
				lane := lane
				go func() {
					<-start
					var ticket controllerPersonaTransportTicket
					var err error
					switch lane {
					case controllerPersonaTransportControl:
						ticket, err = coordinator.stageControl(
							testUSBSetup(usbRequestTypeDeviceIn,
								usbRequestGetConfiguration, 0, 0, 1))
					case controllerPersonaTransportInterruptIn:
						ticket, err = coordinator.stageInterruptIn(
							ControllerPersonaMaximumWireSize)
					case controllerPersonaTransportInterruptOut:
						ticket, err = coordinator.stageInterruptOut(
							[]byte{0x01})
					}
					results <- stageResult{lane: lane, ticket: ticket, err: err}
				}()
			}
			close(start)
			winners := make(map[controllerPersonaTransportLane]controllerPersonaTransportTicket)
			busy := make(map[controllerPersonaTransportLane]int)
			seenTokens := make(map[uint64]bool)
			for range lanes {
				var result stageResult
				select {
				case result = <-results:
				case <-deadline.C:
					t.Fatal("concurrent staging timed out")
				}
				if result.err == nil {
					if _, exists := winners[result.lane]; exists ||
						result.ticket.owner != coordinator ||
						result.ticket.lane != result.lane ||
						result.ticket.token == 0 ||
						seenTokens[result.ticket.token] {
						t.Fatalf("round %d malformed winner: %+v", round, result)
					}
					winners[result.lane] = result.ticket
					seenTokens[result.ticket.token] = true
					continue
				}
				if !errors.Is(result.err, errControllerPersonaTransportLaneBusy) {
					t.Fatalf("round %d staging error = %v", round, result.err)
				}
				busy[result.lane]++
			}
			if len(winners) != 3 {
				t.Fatalf("round %d winners = %+v", round, winners)
			}
			for _, lane := range [...]controllerPersonaTransportLane{
				controllerPersonaTransportControl,
				controllerPersonaTransportInterruptIn,
				controllerPersonaTransportInterruptOut,
			} {
				if busy[lane] != 1 {
					t.Fatalf("round %d lane %d busy count = %d", round, lane, busy[lane])
				}
			}

			retireStart := make(chan struct{})
			retired := make(chan error, len(winners))
			for _, ticket := range winners {
				ticket := ticket
				go func() {
					<-retireStart
					retired <- coordinator.retire(ticket)
				}()
			}
			close(retireStart)
			for range winners {
				select {
				case err := <-retired:
					if err != nil {
						t.Fatalf("round %d retirement = %v", round, err)
					}
				case <-deadline.C:
					t.Fatal("concurrent retirement timed out")
				}
			}
		}
		after, _ := coordinator.snapshot()
		if after != before || coordinator.controlStage.ticket.valid() ||
			coordinator.inStage.ticket.valid() || coordinator.outStage.ticket.valid() {
			t.Fatalf("concurrent stage/retire changed state: before=%+v after=%+v",
				before, after)
		}
	})

	t.Run("control admission races retirement", func(t *testing.T) {
		coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
		configureTestControllerPersonaTransport(t, coordinator)
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		type admitResult struct {
			admission controllerPersonaTransportAdmission
			err       error
		}
		for round := 0; round < 128; round++ {
			ticket, err := coordinator.stageControl(
				testUSBSetup(usbRequestTypeDeviceIn,
					usbRequestGetConfiguration, 0, 0, 1))
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			admitted := make(chan admitResult, 1)
			retired := make(chan error, 1)
			go func() {
				<-start
				var scratch [ControllerPersonaMaximumWireSize]byte
				admission, err := coordinator.admit(ticket, scratch[:], 0)
				admitted <- admitResult{admission: admission, err: err}
			}()
			go func() {
				<-start
				retired <- coordinator.retire(ticket)
			}()
			close(start)
			var admission admitResult
			var retireErr error
			for received := 0; received < 2; received++ {
				select {
				case admission = <-admitted:
					admitted = nil
				case retireErr = <-retired:
					retired = nil
				case <-deadline.C:
					t.Fatal("admission/retirement race timed out")
				}
			}
			switch {
			case admission.err == nil:
				if admission.admission.disposition !=
					controllerPersonaTransportControlResponse ||
					!errors.Is(retireErr,
						errControllerPersonaTransportResponseOutstanding) {
					t.Fatalf("round %d admission won: admission=%+v retire=%v",
						round, admission, retireErr)
				}
				if err := coordinator.completeResponse(ticket, true, 0); err != nil {
					t.Fatalf("round %d completion: %v", round, err)
				}
			case retireErr == nil:
				if !errors.Is(admission.err,
					errInvalidControllerPersonaTransportTicket) {
					t.Fatalf("round %d retirement won: admission=%+v error=%v",
						round, admission.admission, admission.err)
				}
			default:
				t.Fatalf("round %d invalid race result: admission=%+v retire=%v",
					round, admission, retireErr)
			}
		}
	})
}

func TestControllerPersonaTransportStageAndRetireDoNotAllocate(t *testing.T) {
	coordinator := newTestControllerPersonaTransportCoordinator(t, []byte{1})
	allocations := testing.AllocsPerRun(1000, func() {
		ticket, err := coordinator.stageInterruptIn(
			ControllerPersonaMaximumWireSize)
		if err != nil {
			panic(err)
		}
		if err := coordinator.retire(ticket); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("stage/retire allocations = %f", allocations)
	}
}

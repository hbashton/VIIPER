package xboxone

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/controllerfeedback"
)

func testProductionRetainedUSBOptions() ProductionRetainedUSBDeviceOptions {
	return ProductionRetainedUSBDeviceOptions{
		Identity: testOnlySyntheticControllerIdentity(),
		USB: ControllerUSBConfig{
			MaxPower2mA: 0x32, OUTIntervalMS: 4, INIntervalMS: 4,
		},
		Strings:               testOnlyIdentityStrings(),
		IdentityAuthorization: ControllerIdentityAuthorizationGranted,
		FeedbackBinding: ControllerPersonaFeedbackBindingV1{
			Source:            controllerfeedback.SourceXboxOneVirtualDevice,
			PersonaGeneration: 1, DeviceGeneration: 2,
			TransportGeneration: 3, OwnershipEpoch: 4,
			TimeToLiveMicroseconds: 250_000,
		},
		ProtocolTimeMS: 10, AuthorityID: 11,
		ImportDeviceID: testOnlySyntheticControllerIdentity().DeviceID,
		LocalTimeout:   100 * time.Millisecond,
	}
}

func testProductionRetainedUSBDevice(
	t *testing.T,
) (*AuthorizedDormantRetainedUSBDevice, *ProductionRetainedUSBPreparation) {
	t.Helper()
	preparation, err := PrepareProductionRetainedUSBDevice(
		testProductionRetainedUSBOptions())
	if err != nil {
		t.Fatal(err)
	}
	authorization, protocolTime, authorityID, deviceID, local, timeout, ok :=
		preparation.AuthorizedConstructionInputs()
	if !ok {
		t.Fatal("production preparation did not expose construction inputs")
	}
	device, err := NewAuthorizedDormantRetainedUSBDevice(
		authorization, protocolTime, authorityID, deviceID, local, timeout)
	if err != nil {
		t.Fatal(err)
	}
	if err := preparation.AttachConstructed(device); err != nil {
		t.Fatal(err)
	}
	return device, preparation
}

func TestProductionPreparationBindsOfficialShareAndOneBrokerStream(t *testing.T) {
	device, preparation := testProductionRetainedUSBDevice(t)
	if device.VIIPERDeviceType() != "xboxone" || device.feedbackBridge == nil ||
		device.adapter.coordinator.engine.metadata.officialGamepadVariant !=
			OfficialGamepadMetadataConsoleFunctionMap {
		t.Fatalf("production device was not strict CFM persona: %+v", device)
	}
	if err := preparation.AttachConstructed(device); err == nil {
		t.Fatal("production preparation attached twice")
	}
	first, ok := device.acquireBrokerStream()
	if !ok {
		t.Fatal("first broker stream was rejected")
	}
	if _, duplicate := device.acquireBrokerStream(); duplicate {
		t.Fatal("duplicate broker stream was admitted")
	}
	first.release()
	second, ok := device.acquireBrokerStream()
	if !ok || second.token == first.token {
		t.Fatal("successor broker stream did not receive a fresh token")
	}
	second.release()
}

func TestProductionPreparationRejectsForeignInitialPersonaGeneration(t *testing.T) {
	options := testProductionRetainedUSBOptions()
	options.FeedbackBinding.PersonaGeneration = 2
	if _, err := PrepareProductionRetainedUSBDevice(options); !errors.Is(
		err, ErrInvalidCanonicalFeedbackBinding) {
		t.Fatalf("foreign initial persona generation = %v", err)
	}
}

func TestProductionActivationRequiresReadyConsumerAndIsOneShot(t *testing.T) {
	device, _ := testProductionRetainedUSBDevice(t)
	if device.TryBeginProductionBrokerActivation() {
		t.Fatal("activation began without a broker stream")
	}
	lease, ok := device.acquireBrokerStream()
	if !ok {
		t.Fatal("broker stream was rejected")
	}
	defer lease.release()
	if device.TryBeginProductionBrokerActivation() {
		t.Fatal("activation began before ConsumerReady")
	}
	if err := lease.consumerReady(); err != nil {
		t.Fatal(err)
	}
	if !device.TryBeginProductionBrokerActivation() {
		t.Fatal("ready consumer could not reserve activation")
	}
	if device.TryBeginProductionBrokerActivation() {
		t.Fatal("concurrent activation was admitted")
	}
	device.CompleteProductionBrokerActivation(false)
	if !device.TryBeginProductionBrokerActivation() {
		t.Fatal("clean failed activation could not retry")
	}
	device.CompleteProductionBrokerActivation(true)
	if device.TryBeginProductionBrokerActivation() {
		t.Fatal("successful activation was repeated")
	}
}

func TestProductionFeedbackBridgeCommitsOnlyAfterExactConsumerAck(t *testing.T) {
	device, preparation := testProductionRetainedUSBDevice(t)
	preparation.mu.Lock()
	executor := preparation.executor
	preparation.mu.Unlock()
	executor.clock = func() (uint64, bool) { return 1_000_000, true }
	lease, ok := device.acquireBrokerStream()
	if !ok {
		t.Fatal("broker stream was rejected")
	}
	defer lease.release()
	if err := lease.consumerReady(); err != nil {
		t.Fatal(err)
	}

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	stop := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- writeCanonicalFeedbackStream(
			newProductionBrokerFrameWriter(server),
			device.feedbackBridge, lease.token, stop)
	}()
	executeDone := make(chan error, 1)
	go func() {
		executeDone <- executor.Execute(ControllerPersonaLocalExecution{
			Action:     ControllerPersonaApplyDirectMotor,
			Generation: 1, Order: 7,
			DirectMotor: RumbleBodyV1{
				Enabled: MotorAll, LeftVibration: 25, RightVibration: 50,
				LeftImpulse: 75, RightImpulse: 100, Duration: 25,
			},
		}, time.Now().Add(time.Second))
	}()
	var scratch [productionBrokerMaximumPayload]byte
	brokerFrame, err := readProductionBrokerFrame(client, scratch[:])
	if err != nil {
		t.Fatal(err)
	}
	if brokerFrame.typeID != productionBrokerCanonicalFeedback ||
		brokerFrame.correlation != 1 {
		t.Fatalf("broker frame = %+v", brokerFrame)
	}
	var frame controllerfeedback.Frame
	if err := frame.UnmarshalFrom(brokerFrame.payload); err != nil {
		t.Fatal(err)
	}
	if frame.Sequence != 7 || frame.BodyLow != 0x4000 ||
		frame.BodyHigh != 0x8000 || frame.LeftTrigger != 0xbfff ||
		frame.RightTrigger != 0xffff {
		t.Fatalf("feedback frame = %+v", frame)
	}
	select {
	case err := <-executeDone:
		t.Fatalf("executor completed before consumer ACK: %v", err)
	default:
	}
	if err := lease.acknowledgeFeedback(1, true); err != nil {
		t.Fatal(err)
	}
	if err := <-executeDone; err != nil {
		t.Fatal(err)
	}
	close(stop)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

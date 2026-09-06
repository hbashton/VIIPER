package xboxone

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
)

func newOfficialInputPersonaEngine(
	t *testing.T,
	variant OfficialGamepadMetadataVariant,
	current GamepadInputReportV1,
) *ControllerPersonaEngine {
	t.Helper()
	profile := testOnlySyntheticControllerProfile(t)
	metadata, err := profile.BindOfficialGamepadMetadataV1(variant)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewControllerPersonaEngine(ControllerPersonaConfig{
		Profile: profile, Metadata: metadata, CurrentInput: current,
		CurrentStatus:     NewWiredNoBatteryStatus(false),
		PoweringOffStatus: NewWiredNoBatteryStatus(true),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &engine
}

func makeOfficialInputPersonaActive(
	t *testing.T,
	variant OfficialGamepadMetadataVariant,
	current GamepadInputReportV1,
) (*ControllerPersonaEngine, []byte, uint64) {
	t.Helper()
	engine := newOfficialInputPersonaEngine(t, variant, current)
	configurePersonaUSB(t, engine, 0)
	hello, present, err := engine.ClaimPoll(0)
	if err != nil || !present {
		t.Fatalf("Hello = (%+v, %t, %v)", hello, present, err)
	}
	deliverPersonaClaim(t, engine, hello, 1)
	start := claimPersonaHostAction(t, engine, 2,
		[]byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)})
	deliverPersonaClaim(t, engine, start, 3)
	initial, err := engine.ClaimNextLifecycleAction(4)
	if err != nil || initial.Action() != ControllerPersonaSendInitialInput {
		t.Fatalf("initial input = (%+v, %v)", initial, err)
	}
	initialWire := deliverPersonaClaim(t, engine, initial, 5)
	permit, err := engine.ClaimNextLifecycleAction(6)
	if err != nil || permit.Action() != ControllerPersonaPermitNormalUpstream {
		t.Fatalf("permit = (%+v, %v)", permit, err)
	}
	deliverPersonaClaim(t, engine, permit, 7)
	return engine, initialWire, 7
}

func TestOfficialInputVariantCapabilityIsCompilerIssued(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	share := GamepadInputReportV1{State: InputStateV1{Share: true}}

	base, err := profile.BindOfficialGamepadMetadataV1(OfficialGamepadMetadataBase)
	if err != nil {
		t.Fatal(err)
	}
	if base.officialGamepadVariant != OfficialGamepadMetadataBase {
		t.Fatalf("base variant = %d", base.officialGamepadVariant)
	}
	baseConfig := ControllerPersonaConfig{
		Profile: profile, Metadata: base, CurrentInput: share,
		CurrentStatus:     NewWiredNoBatteryStatus(false),
		PoweringOffStatus: NewWiredNoBatteryStatus(true),
	}
	if _, err := NewControllerPersonaEngine(baseConfig, 0); !errors.Is(
		err, ErrShareRequiresExtension) {
		t.Fatalf("base Share error = %v", err)
	}

	shape, _ := OfficialGamepadMetadataConsoleFunctionMap.shape()
	opaque, err := profile.BindExternallyCompiledMetadata(
		compileOfficialGamepadMetadataV1(profile.identity.Firmware, shape))
	if err != nil {
		t.Fatal(err)
	}
	if opaque.officialGamepadVariant != 0 {
		t.Fatalf("opaque bytes acquired variant %d", opaque.officialGamepadVariant)
	}
	baseConfig.Metadata = opaque
	if _, err := NewControllerPersonaEngine(baseConfig, 0); !errors.Is(
		err, ErrShareRequiresExtension) {
		t.Fatalf("opaque official-looking Share error = %v", err)
	}

	functionMap, err := profile.BindOfficialGamepadMetadataV1(
		OfficialGamepadMetadataConsoleFunctionMap)
	if err != nil {
		t.Fatal(err)
	}
	baseConfig.Metadata = functionMap
	functionMapEngine, err := NewControllerPersonaEngine(baseConfig, 0)
	if err != nil {
		t.Fatalf("strict function-map Share error = %v", err)
	}
	if blockers := functionMapEngine.CapabilityBlockers(); blockers&ControllerPersonaBlockerMetadataSemanticValidation != 0 ||
		blockers&ControllerPersonaBlockerGuideInput != 0 {
		t.Fatalf("strict function-map blockers = 0x%x", blockers)
	}
	baseConfig.CurrentInput.State.Guide = true
	if _, err := NewControllerPersonaEngine(baseConfig, 0); !errors.Is(
		err, ErrGuideRequiresStatusMessage) {
		t.Fatalf("function-map Guide error = %v", err)
	}
}

func TestOfficialConsoleFunctionMapPersonaPresentsShareAndRetriesExactWire(t *testing.T) {
	initial := GamepadInputReportV1{State: InputStateV1{
		Share: true, A: true, LeftTrigger: 321, RightStickX: -1234,
	}}
	engine, initialWire, nowMS := makeOfficialInputPersonaActive(
		t, OfficialGamepadMetadataConsoleFunctionMap, initial)
	if len(initialWire) != ConsoleFunctionMapGamepadInputMessageSize {
		t.Fatalf("initial size = %d", len(initialWire))
	}
	sequence, decoded, err := DecodeConsoleFunctionMapGamepadInputMessage(initialWire)
	if err != nil || sequence != 1 || decoded != initial {
		t.Fatalf("initial decode = (%d, %+v, %v)", sequence, decoded, err)
	}

	next := GamepadInputReportV1{State: InputStateV1{
		Share: true, B: true, RightTrigger: 777, LeftStickY: 2345,
	}}
	claim, err := engine.ClaimInput(nowMS+1, next)
	if err != nil || claim.Size() != ConsoleFunctionMapGamepadInputMessageSize ||
		claim.Sequence() != 2 {
		t.Fatalf("Share claim = (%+v, %v)", claim, err)
	}
	first := make([]byte, claim.Size())
	if err := engine.AdmitAndCopy(claim, first, nowMS+1); err != nil {
		t.Fatal(err)
	}
	if err := engine.Resolve(
		claim, ControllerPersonaExecutionCancelled, nowMS+2); err != nil {
		t.Fatal(err)
	}
	retry, err := engine.ClaimRetry(nowMS + 3)
	if err != nil || retry.Size() != ConsoleFunctionMapGamepadInputMessageSize ||
		retry.Sequence() != 2 {
		t.Fatalf("Share retry = (%+v, %v)", retry, err)
	}
	retryWire := deliverPersonaClaim(t, engine, retry, nowMS+4)
	if !bytes.Equal(retryWire, first) {
		t.Fatalf("Share retry changed: first=% x retry=% x", first, retryWire)
	}
	sequence, decoded, err = DecodeConsoleFunctionMapGamepadInputMessage(retryWire)
	if err != nil || sequence != 2 || decoded != next {
		t.Fatalf("retry decode = (%d, %+v, %v)", sequence, decoded, err)
	}
}

func TestOfficialConsoleFunctionMapBrokerIngressAdmitsShareAndQueuesGuide(t *testing.T) {
	engine := newOfficialInputPersonaEngine(
		t, OfficialGamepadMetadataConsoleFunctionMap, GamepadInputReportV1{})
	executor := newScriptedControllerPersonaLocalExecutor()
	adapter, err := NewDormantRetainedUSBAdapter(
		engine, 11, 22, executor, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	lease := adapterTestLease(adapter, 41)
	result, err := adapter.BindImport(lease, time.Now().Add(time.Second))
	if err != nil || result.State != retainedusb.ImportBindBound {
		t.Fatalf("bind = (%+v, %v)", result, err)
	}
	t.Cleanup(func() { stopRetainedUSBAdapterTestWorker(adapter) })

	var wire [SemanticInputWireSize]byte
	if err := EncodeSemanticInputWireV1Into(
		wire[:], InputStateV1{Guide: true}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.PublishSemanticInputWire(
		lease.SessionGeneration, 2, wire[:]); err != nil {
		t.Fatalf("Guide publish = %v", err)
	}

	share := InputStateV1{Share: true, X: true, LeftTrigger: 456}
	if err := EncodeSemanticInputWireV1Into(wire[:], share); err != nil {
		t.Fatal(err)
	}
	if err := adapter.PublishSemanticInputWire(
		lease.SessionGeneration, 3, wire[:]); err != nil {
		t.Fatalf("Share publish = %v", err)
	}
	adapter.mu.Lock()
	got := adapter.input
	revision := adapter.inputRevision
	edges := adapter.guideEdgeCount
	adapter.mu.Unlock()
	if got.State != share || revision != 3 || edges != 2 {
		t.Fatalf("published = (%+v, revision %d, edges %d)", got, revision, edges)
	}
}

func TestControllerPersonaGuideStatusUsesGlobalSequenceAndExactRetry(t *testing.T) {
	engine, nowMS := makePersonaActive(t, []byte{1, 2, 3})
	press, err := engine.ClaimGuideButtonStatus(
		nowMS+1, GuideButtonStatusV1{Down: true})
	if err != nil || press.Action() != ControllerPersonaSendGuideButtonStatus ||
		press.Sequence() != 3 || press.Size() != GuideButtonStatusMessageSize {
		t.Fatalf("press claim = (%+v, %v)", press, err)
	}
	wire := make([]byte, press.Size())
	if err := engine.AdmitAndCopy(press, wire, nowMS+1); err != nil {
		t.Fatal(err)
	}
	want := []byte{0x07, 0x20, 0x03, 0x02, 0x01, 0x5b}
	if !bytes.Equal(wire, want) {
		t.Fatalf("press = % x, want % x", wire, want)
	}
	if err := engine.Resolve(
		press, ControllerPersonaExecutionCancelled, nowMS+2); err != nil {
		t.Fatal(err)
	}
	retry, err := engine.ClaimRetry(nowMS + 3)
	if err != nil || retry.Action() != ControllerPersonaSendGuideButtonStatus ||
		retry.Sequence() != 3 {
		t.Fatalf("Guide retry = (%+v, %v)", retry, err)
	}
	if retryWire := deliverPersonaClaim(t, engine, retry, nowMS+4); !bytes.Equal(retryWire, want) {
		t.Fatalf("Guide retry = % x, want % x", retryWire, want)
	}
	release, err := engine.ClaimGuideButtonStatus(
		nowMS+5, GuideButtonStatusV1{Down: false})
	if err != nil || release.Sequence() != 4 {
		t.Fatalf("release claim = (%+v, %v)", release, err)
	}
	sequence, status, err := DecodeGuideButtonStatusMessage(
		deliverPersonaClaim(t, engine, release, nowMS+6))
	if err != nil || sequence != 4 || status.Down {
		t.Fatalf("release decode = (%d, %+v, %v)", sequence, status, err)
	}
}

func TestRetainedBrokerPreservesGuideEdgesAcrossPollAndRetry(t *testing.T) {
	adapter, executor, _ := newBoundTestRetainedUSBAdapter(t, []byte{1, 2, 3})
	configureRetainedTestPersona(t, adapter)
	retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	startWire := []byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)}
	retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 4, 4, startWire), 2)
	retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 5, 5, 64), 3)

	if err := adapter.PublishSemanticInput(
		41, 2, GamepadInputReportV1{State: InputStateV1{Guide: true}}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.PublishSemanticInput(
		41, 3, GamepadInputReportV1{}); err != nil {
		t.Fatal(err)
	}
	_, initialWire := retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 6, 6, 64), 4)
	if _, initial, err := DecodeGamepadInputMessage(initialWire); err != nil || initial.State.Guide {
		t.Fatalf("initial ordinary input = (%+v, %v)", initial, err)
	}
	triggerAndRetireLocalAction(t, adapter, 7, 5)
	waitForLocalExecution(t, executor, ControllerPersonaPermitNormalUpstream)

	pressTicket, err := adapter.Stage(retainedINRequest(41, 8, 8, 64))
	if err != nil {
		t.Fatal(err)
	}
	var scratch [64]byte
	pressPreparation, err := adapter.Prepare(
		pressTicket, scratch[:], adapterTestTime(6))
	if err != nil || pressPreparation.Result != retainedusb.ResultData {
		t.Fatalf("press Prepare = (%+v, %v)", pressPreparation, err)
	}
	pressWire := append([]byte(nil), scratch[:pressPreparation.ActualLength]...)
	if _, status, err := DecodeGuideButtonStatusMessage(pressWire); err != nil || !status.Down {
		t.Fatalf("press decode = (%+v, %v)", status, err)
	}
	adapter.mu.Lock()
	remainingAfterAdmission := adapter.guideEdgeCount
	adapter.mu.Unlock()
	if remainingAfterAdmission != 1 {
		t.Fatalf("edges after press admission = %d", remainingAfterAdmission)
	}
	if err := adapter.Complete(
		pressTicket, false, adapterTestTime(7)); err != nil {
		t.Fatal(err)
	}

	_, retryWire := retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 9, 9, 64), 8)
	if !bytes.Equal(retryWire, pressWire) {
		t.Fatalf("Guide retry changed: first=% x retry=% x", pressWire, retryWire)
	}
	adapter.mu.Lock()
	remainingAfterRetry := adapter.guideEdgeCount
	adapter.mu.Unlock()
	if remainingAfterRetry != 1 {
		t.Fatalf("retry consumed successor edge: %d remain", remainingAfterRetry)
	}

	_, releaseWire := retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 10, 10, 64), 9)
	if _, status, err := DecodeGuideButtonStatusMessage(releaseWire); err != nil || status.Down {
		t.Fatalf("release decode = (%+v, %v)", status, err)
	}
	adapter.mu.Lock()
	remaining := adapter.guideEdgeCount
	adapter.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("edges after release = %d", remaining)
	}
}

func TestGuideEdgeQueueSaturationBackpressuresWithoutMutation(t *testing.T) {
	adapter, _, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	down := false
	revision := uint64(1)
	for index := 0; index < GuideButtonEdgeQueueCapacity; index++ {
		down = !down
		revision++
		if err := adapter.PublishSemanticInput(
			41, revision, GamepadInputReportV1{
				State: InputStateV1{Guide: down},
			}); err != nil {
			t.Fatalf("edge %d: %v", index, err)
		}
	}
	before := adapter.input
	if err := adapter.PublishSemanticInput(
		41, revision+1, GamepadInputReportV1{
			State: InputStateV1{Guide: !down},
		}); !errors.Is(err, ErrGuideButtonEdgeQueueFull) {
		t.Fatalf("saturation error = %v", err)
	}
	adapter.mu.Lock()
	got := adapter.input
	gotRevision := adapter.inputRevision
	count := adapter.guideEdgeCount
	adapter.mu.Unlock()
	if got != before || gotRevision != revision ||
		count != GuideButtonEdgeQueueCapacity {
		t.Fatalf("saturation mutated state: input=%+v revision=%d count=%d",
			got, gotRevision, count)
	}
}

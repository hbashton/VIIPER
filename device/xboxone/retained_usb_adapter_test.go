package xboxone

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
)

var errScriptedLocalExecutorCanceled = errors.New("scripted local executor canceled")

type scriptedControllerPersonaLocalExecutor struct {
	mu sync.Mutex

	executions        []ControllerPersonaLocalExecution
	neutrals          []ControllerPersonaLocalExecution
	resetNeutrals     []ControllerPersonaLocalExecution
	executeErr        error
	executeOnceErr    error
	executePanic      any
	drainErr          error
	neutralErr        error
	neutralPanic      any
	resetDrainErr     error
	resetNeutralErr   error
	resetNeutralPanic any

	executeEntered      chan struct{}
	executeRelease      <-chan struct{}
	ignoreCancel        bool
	cancel              chan struct{}
	cancelOnce          sync.Once
	resetCancel         chan struct{}
	resetCancelOnce     sync.Once
	resetFenced         bool
	resetDrainCount     int
	drainCount          int
	canceled            bool
	neutralEntered      chan struct{}
	neutralRelease      <-chan struct{}
	neutralExited       chan struct{}
	resetNeutralEntered chan struct{}
	resetNeutralRelease <-chan struct{}
	resetDrainEntered   chan struct{}
	resetDrainRelease   <-chan struct{}
}

func newScriptedControllerPersonaLocalExecutor() *scriptedControllerPersonaLocalExecutor {
	return &scriptedControllerPersonaLocalExecutor{
		cancel: make(chan struct{}), resetCancel: make(chan struct{}),
	}
}

func (executor *scriptedControllerPersonaLocalExecutor) Execute(
	execution ControllerPersonaLocalExecution,
	_ time.Time,
) error {
	executor.mu.Lock()
	if executor.canceled || executor.resetFenced {
		executor.mu.Unlock()
		return errScriptedLocalExecutorCanceled
	}
	executor.executions = append(executor.executions, execution)
	entered := executor.executeEntered
	release := executor.executeRelease
	ignoreCancel := executor.ignoreCancel
	resetCancel := executor.resetCancel
	err := executor.executeErr
	if executor.executeOnceErr != nil {
		err = executor.executeOnceErr
		executor.executeOnceErr = nil
	}
	panicValue := executor.executePanic
	executor.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		resetCanceled := false
		if ignoreCancel {
			<-release
		} else {
			select {
			case <-release:
			case <-executor.cancel:
			case <-resetCancel:
				resetCanceled = true
			}
		}
		if resetCanceled {
			return errScriptedLocalExecutorCanceled
		}
	}
	if panicValue != nil {
		panic(panicValue)
	}
	return err
}

func (executor *scriptedControllerPersonaLocalExecutor) ResetAndDrain(
	_ time.Time,
) error {
	executor.mu.Lock()
	executor.resetDrainCount++
	executor.resetFenced = true
	entered := executor.resetDrainEntered
	release := executor.resetDrainRelease
	err := executor.resetDrainErr
	executor.mu.Unlock()
	executor.resetCancelOnce.Do(func() { close(executor.resetCancel) })
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	return err
}

func (executor *scriptedControllerPersonaLocalExecutor) ResetNeutral(
	execution ControllerPersonaLocalExecution,
	_ time.Time,
) error {
	executor.mu.Lock()
	executor.resetNeutrals = append(executor.resetNeutrals, execution)
	entered := executor.resetNeutralEntered
	release := executor.resetNeutralRelease
	err := executor.resetNeutralErr
	panicValue := executor.resetNeutralPanic
	executor.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if panicValue != nil {
		panic(panicValue)
	}
	if err != nil {
		return err
	}
	executor.mu.Lock()
	executor.resetFenced = false
	executor.resetCancel = make(chan struct{})
	executor.resetCancelOnce = sync.Once{}
	executor.mu.Unlock()
	return nil
}

func (executor *scriptedControllerPersonaLocalExecutor) CancelAndDrain(
	_ time.Time,
) error {
	executor.mu.Lock()
	executor.drainCount++
	executor.canceled = true
	err := executor.drainErr
	executor.mu.Unlock()
	executor.cancelOnce.Do(func() { close(executor.cancel) })
	return err
}

func (executor *scriptedControllerPersonaLocalExecutor) DisconnectNeutral(
	execution ControllerPersonaLocalExecution,
	_ time.Time,
) error {
	executor.mu.Lock()
	executor.neutrals = append(executor.neutrals, execution)
	entered := executor.neutralEntered
	release := executor.neutralRelease
	exited := executor.neutralExited
	err := executor.neutralErr
	panicValue := executor.neutralPanic
	executor.mu.Unlock()
	defer func() {
		if exited != nil {
			select {
			case exited <- struct{}{}:
			default:
			}
		}
	}()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if panicValue != nil {
		panic(panicValue)
	}
	return err
}

func (executor *scriptedControllerPersonaLocalExecutor) snapshot() (
	[]ControllerPersonaLocalExecution,
	[]ControllerPersonaLocalExecution,
	int,
) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]ControllerPersonaLocalExecution(nil), executor.executions...),
		append([]ControllerPersonaLocalExecution(nil), executor.neutrals...),
		executor.drainCount
}

func adapterTestTime(milliseconds uint64) time.Time {
	return time.Now().Add(time.Duration(milliseconds) * time.Millisecond)
}

func adapterTestLease(adapter *DormantRetainedUSBAdapter, session uint64) retainedusb.ImportLease {
	return retainedusb.ImportLease{
		AuthorityID: 11, DeviceID: 22, OwnerID: adapter.Identity(),
		ImportToken: 33, SessionGeneration: session,
	}
}

func adapterTestReset(
	lease retainedusb.ImportLease,
	token uint64,
	generation uint64,
) retainedusb.ImportResetLease {
	return retainedusb.ImportResetLease{
		ImportLease: lease, ResetToken: token, ResetGeneration: generation,
	}
}

func newBoundTestRetainedUSBAdapter(
	t *testing.T,
	metadata []byte,
) (*DormantRetainedUSBAdapter, *scriptedControllerPersonaLocalExecutor,
	retainedusb.ImportLease) {
	t.Helper()
	engine := newTestControllerPersonaEngine(t, metadata, 0)
	executor := newScriptedControllerPersonaLocalExecutor()
	adapter, err := NewDormantRetainedUSBAdapter(
		engine, 11, 22, executor, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	lease := adapterTestLease(adapter, 41)
	result, err := adapter.BindImport(lease, time.Now().Add(time.Second))
	if err != nil || result.Lease != lease ||
		result.State != retainedusb.ImportBindBound {
		t.Fatalf("bind = (%+v, %v)", result, err)
	}
	t.Cleanup(func() { stopRetainedUSBAdapterTestWorker(adapter) })
	return adapter, executor, lease
}

func stopRetainedUSBAdapterTestWorker(adapter *DormantRetainedUSBAdapter) {
	if adapter == nil {
		return
	}
	adapter.mu.Lock()
	local := adapter.local
	stop := adapter.localStop
	done := adapter.localDone
	adapter.mu.Unlock()
	if stop == nil || done == nil {
		return
	}
	if local != nil {
		_ = adapter.drainLocalExecutor(local, time.Now().Add(time.Second))
	}
	adapter.localStopOnce.Do(func() { close(stop) })
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func retainedControlRequest(
	session uint64,
	ordinal uint64,
	sequence uint32,
	setup []byte,
) retainedusb.Request {
	request := retainedusb.Request{
		Lane:              retainedusb.LaneControl,
		SessionGeneration: session, BindingGeneration: 1,
		IngressOrdinal: ordinal, Sequence: sequence,
	}
	copy(request.Setup[:], setup)
	request.TransferLength = uint32(setup[6]) | uint32(setup[7])<<8
	if setup[0]&0x80 != 0 {
		request.Direction = retainedusb.DirectionIn
	} else {
		request.Direction = retainedusb.DirectionOut
	}
	return request
}

func retainedINRequest(
	session uint64,
	ordinal uint64,
	sequence uint32,
	maximum uint32,
) retainedusb.Request {
	return retainedusb.Request{
		Lane: retainedusb.LaneInterruptIn, Direction: retainedusb.DirectionIn,
		SessionGeneration: session, BindingGeneration: 1,
		IngressOrdinal: ordinal, Sequence: sequence, TransferLength: maximum,
		Route: retainedusb.Route{
			InterfaceNumber: 0, AlternateSetting: 0, EndpointAddress: 0x81,
		},
	}
}

func retainedOUTRequest(
	session uint64,
	ordinal uint64,
	sequence uint32,
	wire []byte,
) retainedusb.Request {
	return retainedusb.Request{
		Lane: retainedusb.LaneInterruptOut, Direction: retainedusb.DirectionOut,
		SessionGeneration: session, BindingGeneration: 1,
		IngressOrdinal: ordinal, Sequence: sequence,
		TransferLength: uint32(len(wire)), Data: wire,
		Route: retainedusb.Route{
			InterfaceNumber: 0, AlternateSetting: 0, EndpointAddress: 0x01,
		},
	}
}

func retainedPrepareAndComplete(
	t *testing.T,
	adapter *DormantRetainedUSBAdapter,
	request retainedusb.Request,
	nowMS uint64,
) (retainedusb.Preparation, []byte) {
	t.Helper()
	ticket, err := adapter.Stage(request)
	if err != nil {
		t.Fatal(err)
	}
	destination := make([]byte, request.TransferLength)
	preparation, err := adapter.Prepare(ticket, destination, adapterTestTime(nowMS))
	if err != nil {
		t.Fatal(err)
	}
	if preparation.Result == retainedusb.ResultPending {
		t.Fatalf("unexpected Pending for request %+v", request)
	}
	if err := adapter.Complete(ticket, true, adapterTestTime(nowMS)); err != nil {
		t.Fatal(err)
	}
	return preparation, destination[:preparation.ActualLength]
}

func configureRetainedTestPersona(
	t *testing.T,
	adapter *DormantRetainedUSBAdapter,
) {
	t.Helper()
	setAddress := retainedControlRequest(41, 1, 1,
		testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetAddress, 1, 0, 0))
	if result, _ := retainedPrepareAndComplete(t, adapter, setAddress, 0); result.Result != retainedusb.ResultSuccess {
		t.Fatalf("SET_ADDRESS = %+v", result)
	}
	setConfiguration := retainedControlRequest(41, 2, 2,
		testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration,
			uint16(usbConfigurationGIP), 0, 0))
	if result, _ := retainedPrepareAndComplete(t, adapter, setConfiguration, 0); result.Result != retainedusb.ResultSuccess {
		t.Fatalf("SET_CONFIGURATION = %+v", result)
	}
}

func TestDormantRetainedUSBAdapterBindAdoptsUSBIPAddressBeforeFirstEP0(
	t *testing.T,
) {
	adapter, _, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	snapshot, ok := adapter.coordinator.snapshot()
	if !ok || snapshot.USBState != USBControlDeviceAddressed {
		t.Fatalf("retained USB/IP bind state = (%+v, %t)", snapshot, ok)
	}

	osString := retainedControlRequest(41, 1, 1,
		testUSBSetup(usbRequestTypeDeviceIn, usbRequestGetDescriptor,
			uint16(usbDescriptorTypeString)<<8|uint16(MicrosoftOSStringIndex),
			0, MicrosoftOSStringDescriptorSize))
	result, wire := retainedPrepareAndComplete(t, adapter, osString, 0)
	if result.Result != retainedusb.ResultData ||
		len(wire) != MicrosoftOSStringDescriptorSize {
		t.Fatalf("MS OS string before SET_ADDRESS = (%+v, % x)", result, wire)
	}

	compatibleID := retainedControlRequest(41, 2, 2,
		testUSBSetup(usbRequestTypeVendorIn, MicrosoftOSVendorCode, 0,
			0x0004, 16))
	result, wire = retainedPrepareAndComplete(t, adapter, compatibleID, 0)
	if result.Result != retainedusb.ResultData ||
		len(wire) != 16 {
		t.Fatalf("XGIP10 descriptor before SET_ADDRESS = (%+v, % x)",
			result, wire)
	}
	properties := retainedControlRequest(41, 3, 3,
		testUSBSetup(usbRequestTypeVendorInterfaceIn, MicrosoftOSVendorCode, 0,
			0x0005, MicrosoftExtendedPropertiesDescriptorSize))
	result, wire = retainedPrepareAndComplete(t, adapter, properties, 0)
	if result.Result != retainedusb.ResultData ||
		len(wire) != MicrosoftExtendedPropertiesDescriptorSize {
		t.Fatalf("extended properties before SET_ADDRESS = (%+v, % x)",
			result, wire)
	}

	setConfiguration := retainedControlRequest(41, 4, 4,
		testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration,
			uint16(usbConfigurationGIP), 0, 0))
	result, _ = retainedPrepareAndComplete(t, adapter, setConfiguration, 0)
	if result.Result != retainedusb.ResultSuccess {
		t.Fatalf("SET_CONFIGURATION after retained USB/IP bind = %+v", result)
	}
}

func TestDormantRetainedUSBAdapterClampsStaleTransportSampleAtCanonicalCall(
	t *testing.T,
) {
	adapter, _, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	configureRetainedTestPersona(t, adapter)

	query := func(ordinal uint64, sequence uint32, at time.Time) {
		t.Helper()
		request := retainedControlRequest(lease.SessionGeneration, ordinal,
			sequence, testUSBSetup(usbRequestTypeDeviceIn,
				usbRequestGetConfiguration, 0, 0, 1))
		ticket, err := adapter.Stage(request)
		if err != nil {
			t.Fatal(err)
		}
		var destination [1]byte
		preparation, err := adapter.Prepare(ticket, destination[:], at)
		if err != nil || preparation.Result != retainedusb.ResultData ||
			preparation.ActualLength != 1 || destination[0] != 1 {
			t.Fatalf("GET_CONFIGURATION at %v = (%+v, % x, %v)",
				at, preparation, destination, err)
		}
		if err := adapter.Complete(ticket, true, at); err != nil {
			t.Fatal(err)
		}
	}

	later := adapter.clockOrigin.Add(100 * time.Millisecond)
	query(20, 20, later)
	// A local worker or another endpoint can finish after the scheduler sampled
	// this older time but before its serialized callback reaches the canonical
	// engine. The retained adapter owns that scheduling boundary and clamps the
	// stale sample; the strict coordinator/engine contract remains unchanged.
	query(21, 21, later.Add(-time.Millisecond))
}

func waitForLocalExecution(
	t *testing.T,
	executor *scriptedControllerPersonaLocalExecutor,
	action ControllerPersonaAction,
) ControllerPersonaLocalExecution {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		executions, _, _ := executor.snapshot()
		for _, execution := range executions {
			if execution.Action == action {
				return execution
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("local action %d was not executed", action)
	return ControllerPersonaLocalExecution{}
}

func triggerAndRetireLocalAction(
	t *testing.T,
	adapter *DormantRetainedUSBAdapter,
	ordinal uint64,
	nowMS uint64,
) {
	t.Helper()
	ticket, err := adapter.Stage(retainedINRequest(41, ordinal, uint32(ordinal), 64))
	if err != nil {
		t.Fatal(err)
	}
	var scratch [64]byte
	preparation, err := adapter.Prepare(ticket, scratch[:], adapterTestTime(nowMS))
	if err != nil {
		t.Fatal(err)
	}
	if preparation.Result != retainedusb.ResultPending ||
		preparation.ReadinessEpoch == 0 || preparation.RetryAt.IsZero() {
		t.Fatalf("local trigger preparation = %+v", preparation)
	}
	if err := adapter.Retire(
		ticket, retainedusb.RetireUnlink, adapterTestTime(nowMS)); err != nil {
		t.Fatal(err)
	}
}

func TestDormantRetainedUSBAdapterIdentityLimitsAndExactImportBind(t *testing.T) {
	engineOne := newTestControllerPersonaEngine(t, []byte{1}, 0)
	engineTwo := newTestControllerPersonaEngine(t, []byte{1}, 0)
	oneExecutor := newScriptedControllerPersonaLocalExecutor()
	twoExecutor := newScriptedControllerPersonaLocalExecutor()
	one, err := NewDormantRetainedUSBAdapter(
		engineOne, 11, 22, oneExecutor, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewDormantRetainedUSBAdapter(
		engineTwo, 11, 22, twoExecutor, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopRetainedUSBAdapterTestWorker(one)
		stopRetainedUSBAdapterTestWorker(two)
	})
	if one.Identity() == 0 || two.Identity() == 0 || one.Identity() == two.Identity() {
		t.Fatalf("owner identities = %d, %d", one.Identity(), two.Identity())
	}
	if one.localStop != nil || one.localDone != nil ||
		two.localStop != nil || two.localDone != nil {
		t.Fatal("construction started an unbound local worker")
	}
	limits := one.Limits()
	if !limits.Valid() || limits.QueueDepth != [3]uint8{
		1, dormantRetainedUSBInterruptInQueueDepth, 1,
	} ||
		limits.InterruptInRoute.EndpointAddress != 0x81 ||
		limits.InterruptOutRoute.EndpointAddress != 0x01 {
		t.Fatalf("limits = %+v", limits)
	}
	forged := adapterTestLease(one, 7)
	forged.OwnerID = two.Identity()
	result, err := one.BindImport(forged, time.Now().Add(time.Second))
	if err != nil || result.State != retainedusb.ImportBindRejected {
		t.Fatalf("forged bind = (%+v, %v)", result, err)
	}
	if one.localStop != nil || one.localDone != nil {
		t.Fatal("rejected bind left an unowned local worker")
	}
	forged = adapterTestLease(one, 7)
	forged.AuthorityID++
	result, err = one.BindImport(forged, time.Now().Add(time.Second))
	if err != nil || result.State != retainedusb.ImportBindRejected ||
		one.localStop != nil {
		t.Fatalf("cross-authority bind = (%+v, %v)", result, err)
	}
	forged = adapterTestLease(one, 7)
	forged.DeviceID++
	result, err = one.BindImport(forged, time.Now().Add(time.Second))
	if err != nil || result.State != retainedusb.ImportBindRejected ||
		one.localStop != nil {
		t.Fatalf("cross-device bind = (%+v, %v)", result, err)
	}
	lease := adapterTestLease(one, 7)
	result, err = one.BindImport(lease, time.Now().Add(time.Second))
	if err != nil || result.State != retainedusb.ImportBindBound || result.Lease != lease {
		t.Fatalf("exact bind = (%+v, %v)", result, err)
	}
	if one.localStop == nil || one.localDone == nil {
		t.Fatal("bound import did not start its local worker")
	}
	result, err = one.BindImport(lease, time.Now().Add(time.Second))
	if !errors.Is(err, errDormantRetainedUSBInvalidImport) ||
		result.State != retainedusb.ImportBindQuarantined {
		t.Fatalf("duplicate bind = (%+v, %v)", result, err)
	}
}

func TestDormantRetainedUSBAdapterControlINHostLengthIsMaximum(t *testing.T) {
	product := strings.Repeat("\u0800", 126)
	authorization, err := NewAuthorizedControllerPersonaConfig(
		testOnlyControllerPersonaConfig(t),
		ControllerUSBIdentityStrings{
			Manufacturer: "Synthetic Test Lab",
			Product:      product,
			Serial:       testOnlySyntheticSerial,
		},
		ControllerIdentityAuthorizationGranted)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewAuthorizedControllerPersonaEngine(authorization, 0)
	if err != nil {
		t.Fatal(err)
	}
	executor := newScriptedControllerPersonaLocalExecutor()
	adapter, err := NewDormantRetainedUSBAdapter(
		engine, 11, 22, executor, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	lease := adapterTestLease(adapter, 41)
	if result, bindErr := adapter.BindImport(
		lease, time.Now().Add(time.Second)); bindErr != nil ||
		result.State != retainedusb.ImportBindBound || result.Lease != lease {
		t.Fatalf("bind = (%+v, %v)", result, bindErr)
	}
	t.Cleanup(func() { stopRetainedUSBAdapterTestWorker(adapter) })

	want := testOnlyEncodeUSBStringDescriptor(product)
	if len(want) != usbMaximumStringDescriptorSize {
		t.Fatalf("maximum string descriptor size = %d", len(want))
	}
	for index, hostMaximum := range []uint16{255, ^uint16(0)} {
		setup := testUSBSetup(
			usbRequestTypeDeviceIn, usbRequestGetDescriptor,
			uint16(usbDescriptorTypeString)<<8|2,
			USBEnglishUnitedStatesLanguageID, hostMaximum)
		request := retainedControlRequest(
			41, uint64(index+1), uint32(index+1), setup)
		ticket, stageErr := adapter.Stage(request)
		if stageErr != nil {
			t.Fatalf("Stage wLength %d: %v", hostMaximum, stageErr)
		}
		var destination [usbMaximumStringDescriptorSize]byte
		preparation, prepareErr := adapter.Prepare(
			ticket, destination[:], adapterTestTime(uint64(index+1)))
		if prepareErr != nil {
			t.Fatalf("Prepare wLength %d: %v", hostMaximum, prepareErr)
		}
		if preparation.Result != retainedusb.ResultData ||
			preparation.ActualLength != usbMaximumStringDescriptorSize ||
			string(destination[:]) != string(want) {
			t.Fatalf("Prepare wLength %d = (%+v, % x), want 254 bytes",
				hostMaximum, preparation, destination[:])
		}
		if completeErr := adapter.Complete(
			ticket, true, adapterTestTime(uint64(index+1))); completeErr != nil {
			t.Fatalf("Complete wLength %d: %v", hostMaximum, completeErr)
		}
	}
}

func TestDormantRetainedUSBAdapterGoldenLifecycleLatestInputAndFeedback(t *testing.T) {
	adapter, executor, _ := newBoundTestRetainedUSBAdapter(t, []byte{1, 2, 3})
	configureRetainedTestPersona(t, adapter)

	hello, helloWire := retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	if hello.Result != retainedusb.ResultData {
		t.Fatalf("Hello = %+v", hello)
	}
	if _, err := DecodeHelloMessage(helloWire); err != nil {
		t.Fatalf("Hello wire: %v", err)
	}

	startWire := []byte{0x05, 0x20, 0x02, 0x01, byte(SetDeviceStateStart)}
	start, _ := retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 4, 4, startWire), 2)
	if start.Result != retainedusb.ResultSuccess ||
		start.ActualLength != uint32(len(startWire)) {
		t.Fatalf("START = %+v", start)
	}
	status, statusWire := retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 5, 5, 64), 3)
	if status.Result != retainedusb.ResultData || len(statusWire) == 0 {
		t.Fatalf("current status = %+v wire=% x", status, statusWire)
	}

	latest := GamepadInputReportV1{State: InputStateV1{
		A: true, Y: true, LeftTrigger: 777, RightTrigger: 888,
		LeftStickX: -1234, RightStickY: 2345,
	}}
	initialTicket, err := adapter.Stage(retainedINRequest(41, 6, 6, 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.PublishSemanticInput(41, 2, latest); err != nil {
		t.Fatal(err)
	}
	var initialScratch [64]byte
	initial, err := adapter.Prepare(
		initialTicket, initialScratch[:], adapterTestTime(4))
	if err != nil {
		t.Fatal(err)
	}
	if initial.Result != retainedusb.ResultData {
		t.Fatalf("initial input = %+v", initial)
	}
	initialWire := initialScratch[:initial.ActualLength]
	if err := adapter.Complete(
		initialTicket, true, adapterTestTime(4)); err != nil {
		t.Fatal(err)
	}
	_, decoded, err := DecodeGamepadInputMessage(initialWire)
	if err != nil || decoded != latest {
		t.Fatalf("initial input = (%+v, %v), want %+v", decoded, err, latest)
	}

	triggerAndRetireLocalAction(t, adapter, 7, 5)
	if permit := waitForLocalExecution(
		t, executor, ControllerPersonaPermitNormalUpstream); !permit.valid() {
		t.Fatalf("permit = %+v", permit)
	}

	motor := RumbleBodyV1{
		Enabled: MotorLeftVibration | MotorRightVibration |
			MotorLeftImpulse | MotorRightImpulse,
		LeftVibration: 11, RightVibration: 22,
		LeftImpulse: 33, RightImpulse: 44,
		Duration: 8, Delay: 2, Repeat: 3,
	}
	motorWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(motorWire, 0x31, motor); err != nil {
		t.Fatal(err)
	}
	retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 8, 8, motorWire), 6)
	triggerAndRetireLocalAction(t, adapter, 9, 7)
	direct := waitForLocalExecution(t, executor, ControllerPersonaApplyDirectMotor)
	if direct.DirectMotor != motor {
		t.Fatalf("four-actuator motor = %+v, want %+v", direct.DirectMotor, motor)
	}

	guide := GuideLEDCommandV1{Pattern: GuideLEDPatternSlowBlink, Intensity: 23}
	guideWire := make([]byte, GuideLEDCommandMessageSize)
	if err := EncodeGuideLEDCommandMessageInto(guideWire, 0x32, guide); err != nil {
		t.Fatal(err)
	}
	retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 10, 10, guideWire), 8)
	triggerAndRetireLocalAction(t, adapter, 11, 9)
	led := waitForLocalExecution(t, executor, ControllerPersonaApplyGuideLED)
	if led.GuideLED != guide {
		t.Fatalf("Guide LED = %+v, want %+v", led.GuideLED, guide)
	}

	snapshot, ok := adapter.coordinator.snapshot()
	if !ok || snapshot.Feedback.DirectMotor != motor ||
		snapshot.Feedback.GuideLED != guide || !snapshot.NormalUpstream {
		t.Fatalf("persona snapshot = %+v", snapshot)
	}
}

func TestDormantRetainedUSBAdapterAcceptsExtendedInitializationBeforeStart(t *testing.T) {
	adapter, _, _ := newBoundTestRetainedUSBAdapter(t, []byte{1, 2, 3})
	configureRetainedTestPersona(t, adapter)

	hello, helloWire := retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	if hello.Result != retainedusb.ResultData {
		t.Fatalf("Hello = %+v", hello)
	}
	if _, err := DecodeHelloMessage(helloWire); err != nil {
		t.Fatalf("Hello wire: %v", err)
	}

	before := adapter.coordinator.engine.Snapshot()
	probeWire := []byte{
		0x05, 0x20, 0x02, 0x0f,
		0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x55,
		0x53, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	probe, _ := retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 4, 4, probeWire), 2)
	if probe.Result != retainedusb.ResultSuccess ||
		probe.ActualLength != uint32(len(probeWire)) {
		t.Fatalf("extended initialization = %+v", probe)
	}
	if after := adapter.coordinator.engine.Snapshot(); after != before {
		t.Fatalf("extended initialization changed persona: before=%+v after=%+v",
			before, after)
	}

	startWire := []byte{0x05, 0x20, 0x03, 0x01, byte(SetDeviceStateStart)}
	start, _ := retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 5, 5, startWire), 3)
	if start.Result != retainedusb.ResultSuccess ||
		start.ActualLength != uint32(len(startWire)) {
		t.Fatalf("START = %+v", start)
	}
	status, statusWire := retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 6, 6, 64), 4)
	if status.Result != retainedusb.ResultData || len(statusWire) == 0 {
		t.Fatalf("current status = %+v wire=% x", status, statusWire)
	}
	if _, _, err := DecodeExtendedStatusNoEventsMessage(statusWire); err != nil {
		t.Fatalf("decode current status: %v (wire=% x)", err, statusWire)
	}
}

func TestDormantRetainedUSBAdapterConfigurationLossExecutesClearWithoutNewURB(
	t *testing.T,
) {
	adapter, executor, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	configureRetainedTestPersona(t, adapter)
	hello, _ := retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	if hello.Result != retainedusb.ResultData {
		t.Fatalf("Hello = %+v", hello)
	}
	motorBody := testDirectMotorBody()
	motorWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(motorWire, 0x71, motorBody); err != nil {
		t.Fatal(err)
	}
	retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 4, 4, motorWire), 2)
	triggerAndRetireLocalAction(t, adapter, 5, 3)
	if execution := waitForLocalExecution(
		t, executor, ControllerPersonaApplyDirectMotor); execution.DirectMotor != motorBody {
		t.Fatalf("motor execution = %+v", execution)
	}
	deadline := time.Now().Add(time.Second)
	for {
		snapshot, ok := adapter.coordinator.snapshot()
		if !ok {
			t.Fatal("coordinator snapshot unavailable")
		}
		if snapshot.Feedback.DirectMotor == motorBody && !snapshot.ClaimOutstanding {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("motor did not reach terminal delivery: %+v", snapshot)
		}
		time.Sleep(time.Millisecond)
	}

	setup := testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration,
		uint16(usbConfigurationUnselected), 0, 0)
	failedTicket, err := adapter.Stage(retainedControlRequest(41, 6, 6, setup))
	if err != nil {
		t.Fatal(err)
	}
	failedPreparation, err := adapter.Prepare(
		failedTicket, nil, adapterTestTime(4))
	if err != nil || failedPreparation.Result != retainedusb.ResultSuccess {
		t.Fatalf("failed unconfigure preparation = (%+v, %v)",
			failedPreparation, err)
	}
	if err := adapter.Complete(
		failedTicket, false, adapterTestTime(4)); err != nil {
		t.Fatal(err)
	}
	if got, ok := adapter.coordinator.snapshot(); !ok ||
		got.USBState != USBControlDeviceConfigured ||
		got.Feedback.DirectMotor != motorBody || got.ClaimOutstanding {
		t.Fatalf("failed unconfigure snapshot = (%+v, available=%t)", got, ok)
	}
	executions, _, _ := executor.snapshot()
	for _, execution := range executions {
		if execution.Action == ControllerPersonaClearOutputs {
			t.Fatalf("failed EP0 completion executed clear: %+v", execution)
		}
	}

	executor.mu.Lock()
	executor.executeOnceErr = errors.New("injected one-shot clear failure")
	executor.mu.Unlock()
	result, _ := retainedPrepareAndComplete(
		t, adapter, retainedControlRequest(41, 7, 7, setup), 5)
	if result.Result != retainedusb.ResultSuccess {
		t.Fatalf("delivered unconfigure = %+v", result)
	}
	// No subsequent retained request is staged. Complete must wake the already
	// selected local clear itself.
	clear := waitForLocalExecution(t, executor, ControllerPersonaClearOutputs)
	if clear.ClearEpoch == 0 || clear.Generation != 1 {
		t.Fatalf("configuration-loss clear execution = %+v", clear)
	}
	deadline = time.Now().Add(time.Second)
	for {
		snapshot, ok := adapter.coordinator.snapshot()
		if !ok {
			t.Fatal("coordinator snapshot unavailable")
		}
		if snapshot.USBState == USBControlDeviceAddressed &&
			snapshot.Feedback.DirectMotor == (RumbleBodyV1{}) &&
			snapshot.Feedback.ClearEpoch == clear.ClearEpoch &&
			!snapshot.ClaimOutstanding && !snapshot.RetryPending {
			break
		}
		if time.Now().After(deadline) {
			adapter.mu.Lock()
			localErr := adapter.lastLocalError
			fatalErr := adapter.fatalLocalError
			adapter.mu.Unlock()
			t.Fatalf("configuration-loss clear did not commit: %+v; local=%v fatal=%v",
				snapshot, localErr, fatalErr)
		}
		time.Sleep(time.Millisecond)
	}
	executions, _, _ = executor.snapshot()
	var clears []ControllerPersonaLocalExecution
	for _, execution := range executions {
		if execution.Action == ControllerPersonaClearOutputs {
			clears = append(clears, execution)
		}
	}
	if len(clears) != 2 || clears[0].Generation != clears[1].Generation ||
		clears[0].ClearEpoch != clears[1].ClearEpoch ||
		clears[0].ClearEpoch != clear.ClearEpoch {
		t.Fatalf("configuration-loss retry changed or duplicated epoch: %+v", clears)
	}
}

func TestDormantRetainedUSBAdapterClearAdmissionExhaustionQuarantinesWithoutNeutral(
	t *testing.T,
) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	configureRetainedTestPersona(t, adapter)
	adapter.coordinator.mu.Lock()
	adapter.coordinator.nextLocalToken = ^uint64(0)
	adapter.coordinator.mu.Unlock()

	setup := testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration,
		uint16(usbConfigurationUnselected), 0, 0)
	result, _ := retainedPrepareAndComplete(
		t, adapter, retainedControlRequest(41, 3, 3, setup), 1)
	if result.Result != retainedusb.ResultSuccess {
		t.Fatalf("delivered unconfigure = %+v", result)
	}
	deadline := time.Now().Add(time.Second)
	for {
		adapter.mu.Lock()
		state := adapter.state
		fatalErr := adapter.fatalLocalError
		adapter.mu.Unlock()
		if errors.Is(fatalErr, errControllerPersonaTransportTokenExhausted) {
			break
		}
		if time.Now().After(deadline) {
			adapter.mu.Lock()
			localErr := adapter.lastLocalError
			wakeLength := len(adapter.localWake)
			adapter.mu.Unlock()
			snapshot, _ := adapter.coordinator.snapshot()
			t.Fatalf("clear admission exhaustion was not latched fatal: state=%d local=%v fatal=%v wake=%d snapshot=%+v",
				state, localErr, fatalErr, wakeLength, snapshot)
		}
		time.Sleep(time.Millisecond)
	}
	if got, ok := adapter.coordinator.snapshot(); !ok ||
		got.USBState != USBControlDeviceAddressed || !got.ClaimOutstanding ||
		got.Feedback.ClearEpoch != 0 {
		t.Fatalf("exhausted clear ownership = (%+v, available=%t)", got, ok)
	}
	executions, neutrals, _ := executor.snapshot()
	for _, execution := range executions {
		if execution.Action == ControllerPersonaClearOutputs {
			t.Fatalf("exhausted admission executed clear: %+v", execution)
		}
	}
	if len(neutrals) != 0 {
		t.Fatalf("admission exhaustion falsely neutralized: %+v", neutrals)
	}
	terminalDeadline := time.Now().Add(time.Second)
	drain, err := adapter.CancelAndDrain(
		lease, retainedusb.ImportCloseInvariantFailure, terminalDeadline)
	if drain.State != retainedusb.ImportDrainDrained || err != nil {
		t.Fatalf("exhausted clear containment = (%+v, %v)", drain, err)
	}
	disconnect, disconnectErr := adapter.DisconnectNeutral(
		lease, retainedusb.ImportCloseInvariantFailure, terminalDeadline)
	if disconnect.State != retainedusb.ImportDisconnectQuarantined ||
		disconnectErr == nil {
		t.Fatalf("quarantined clear disconnect = (%+v, %v)",
			disconnect, disconnectErr)
	}
	_, neutrals, _ = executor.snapshot()
	if len(neutrals) != 0 {
		t.Fatalf("quarantine falsely authorized neutral: %+v", neutrals)
	}
}

func TestDormantRetainedUSBAdapterClearWakeIsLatchedWithoutDuplicateExecution(
	t *testing.T,
) {
	adapter, executor, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	configureRetainedTestPersona(t, adapter)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	executor.mu.Lock()
	executor.executeEntered = entered
	executor.executeRelease = release
	executor.mu.Unlock()

	setup := testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration,
		uint16(usbConfigurationUnselected), 0, 0)
	retainedPrepareAndComplete(
		t, adapter, retainedControlRequest(41, 3, 3, setup), 1)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("configuration clear did not reach the local executor")
	}
	var wakes sync.WaitGroup
	for index := 0; index < 32; index++ {
		wakes.Add(1)
		go func() {
			defer wakes.Done()
			latchedSignal(adapter.localWake)
		}()
	}
	wakes.Wait()
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		snapshot, _ := adapter.coordinator.snapshot()
		if snapshot.Feedback.ClearEpoch != 0 && !snapshot.ClaimOutstanding {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("latched clear did not complete: %+v", snapshot)
		}
		time.Sleep(time.Millisecond)
	}
	// Let the worker consume any one coalesced stale wake. With no pending
	// claim it must be a no-op, not a second physical clear.
	time.Sleep(5 * time.Millisecond)
	executions, _, _ := executor.snapshot()
	var clears int
	for _, execution := range executions {
		if execution.Action == ControllerPersonaClearOutputs {
			clears++
		}
	}
	if clears != 1 {
		t.Fatalf("coalesced worker wakes executed clear %d times: %+v",
			clears, executions)
	}
}

func TestDormantRetainedUSBAdapterMetadataACKCommitsOnlyAfterLocalDelivery(t *testing.T) {
	blob := make([]byte, 61)
	for index := range blob {
		blob[index] = byte(index)
	}
	engine := newTestControllerPersonaEngine(t, blob, 0)
	executor := newScriptedControllerPersonaLocalExecutor()
	adapter, err := NewDormantRetainedUSBAdapter(
		engine, 11, 22, executor, 100*time.Millisecond)
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
		t.Fatalf("Hello = %+v", hello)
	}
	beginWire := []byte{0x04, 0x20, 0x01, 0x00}
	if begin, _ := retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 4, 4, beginWire), 2); begin.Result != retainedusb.ResultSuccess {
		t.Fatalf("metadata begin = %+v", begin)
	}
	triggerAndRetireLocalAction(t, adapter, 5, 3)
	waitForLocalExecution(t, executor, ControllerPersonaBeginMetadata)
	if packet, _ := retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 6, 6, 64), 4); packet.Result != retainedusb.ResultData {
		t.Fatalf("metadata packet = %+v", packet)
	}
	body := ProtocolControlACKBodyV1{
		ReferencedDataClass:     DataClassCommand,
		ReferencedMessageNumber: messageNumberMetadataRequest,
		ReferencedSystem:        true, FragmentOffset: 58, RemainingBuffer: 512,
	}
	var ackWire [ProtocolControlACKMessageSize]byte
	if err := EncodeProtocolControlACKMessageInto(ackWire[:], 2, body); err != nil {
		t.Fatal(err)
	}
	retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 7, 7, ackWire[:]), 6)
	adapter.coordinator.mu.Lock()
	before := adapter.coordinator.engine.metadataTransfer.Snapshot()
	adapter.coordinator.mu.Unlock()
	if before.AcknowledgedEnd != 0 || !before.AwaitingAcknowledgement {
		t.Fatalf("OUT response committed ACK early: %+v", before)
	}
	triggerAndRetireLocalAction(t, adapter, 8, 7)
	waitForLocalExecution(t, executor,
		ControllerPersonaApplyMetadataAcknowledgement)
	deadline := time.Now().Add(time.Second)
	for {
		adapter.coordinator.mu.Lock()
		after := adapter.coordinator.engine.metadataTransfer.Snapshot()
		adapter.coordinator.mu.Unlock()
		if after.AcknowledgedEnd == 58 && !after.AwaitingAcknowledgement {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("local ACK did not commit: %+v", after)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDormantRetainedUSBAdapterForgedStaleMalformedAndExactlyOnce(t *testing.T) {
	adapter, _, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	other, _, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})

	request := retainedINRequest(41, 1, 0, 64)
	ticket, err := adapter.Stage(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Stage(request); !errors.Is(err, errDormantRetainedUSBLaneBusy) {
		t.Fatalf("duplicate lane stage = %v", err)
	}
	forged := ticket
	forged.OwnerID = other.Identity()
	if _, err := adapter.Prepare(forged, make([]byte, 64), adapterTestTime(0)); !errors.Is(err, errDormantRetainedUSBInvalidTicket) {
		t.Fatalf("cross-owner prepare = %v", err)
	}
	stale := ticket
	stale.SessionGeneration++
	if err := adapter.Retire(stale, retainedusb.RetireUnlink, adapterTestTime(0)); !errors.Is(err, errDormantRetainedUSBInvalidTicket) {
		t.Fatalf("cross-session retire = %v", err)
	}
	if err := adapter.Retire(ticket, retainedusb.RetireUnlink, adapterTestTime(0)); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Retire(ticket, retainedusb.RetireUnlink, adapterTestTime(0)); !errors.Is(err, errDormantRetainedUSBInvalidTicket) {
		t.Fatalf("duplicate retire = %v", err)
	}

	badRoute := retainedOUTRequest(41, 2, 2, []byte{1, 2, 3, 4})
	badRoute.Route.EndpointAddress = 0x02
	if _, err := adapter.Stage(badRoute); !errors.Is(
		err, errDormantRetainedUSBInvalidRequest) {
		t.Fatalf("bad route = %v", err)
	}
	aliasedStorage := make([]byte, 4, 8)
	if _, err := adapter.Stage(retainedOUTRequest(
		41, 2, 2, aliasedStorage)); !errors.Is(
		err, errDormantRetainedUSBInvalidRequest) {
		t.Fatalf("non-exact OUT slice = %v", err)
	}
	dirtySetup := retainedOUTRequest(41, 2, 2, []byte{1, 2, 3, 4})
	dirtySetup.Setup[0] = 1
	if _, err := adapter.Stage(dirtySetup); !errors.Is(
		err, errDormantRetainedUSBInvalidRequest) {
		t.Fatalf("interrupt request with setup bytes = %v", err)
	}
	badControlLength := retainedControlRequest(41, 2, 2,
		testUSBSetup(usbRequestTypeDeviceIn, usbRequestGetDescriptor,
			uint16(usbDescriptorTypeDevice)<<8, 0, USBDeviceDescriptorSize))
	badControlLength.TransferLength--
	if _, err := adapter.Stage(badControlLength); !errors.Is(
		err, errDormantRetainedUSBInvalidRequest) {
		t.Fatalf("EP0 setup/transfer mismatch = %v", err)
	}
	malformed := retainedOUTRequest(41, 3, 3, []byte{1, 2, 3, 4})
	malformedTicket, err := adapter.Stage(malformed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Prepare(
		malformedTicket, nil, adapterTestTime(0)); err == nil {
		t.Fatal("malformed GIP OUT was accepted")
	}
	if err := adapter.Retire(
		malformedTicket, retainedusb.RetirePrepareFailure,
		adapterTestTime(0)); err != nil {
		t.Fatal(err)
	}

	unsupported := retainedControlRequest(41, 4, 4,
		testUSBSetup(usbRequestTypeDeviceIn, 0xff, 0, 0, 1))
	unsupportedTicket, err := adapter.Stage(unsupported)
	if err != nil {
		t.Fatal(err)
	}
	preparation, err := adapter.Prepare(
		unsupportedTicket, make([]byte, 1), adapterTestTime(0))
	if err != nil || preparation.Result != retainedusb.ResultStall {
		t.Fatalf("unsupported EP0 = (%+v, %v)", preparation, err)
	}
	if err := adapter.Complete(
		unsupportedTicket, true, adapterTestTime(0)); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Complete(
		unsupportedTicket, true, adapterTestTime(0)); !errors.Is(
		err, errDormantRetainedUSBInvalidTicket) {
		t.Fatalf("duplicate complete = %v", err)
	}
}

func TestDormantRetainedUSBAdapterRetiresEveryCancelAndResetReasonExactlyOnce(t *testing.T) {
	adapter, _, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	reasons := []retainedusb.RetireReason{
		retainedusb.RetireUnlink,
		retainedusb.RetireEndpointReset,
		retainedusb.RetireInterfaceReset,
		retainedusb.RetireConfigurationChange,
		retainedusb.RetireConnectionClose,
		retainedusb.RetirePrepareFailure,
		retainedusb.RetireInvariantFailure,
	}
	for index, reason := range reasons {
		ordinal := uint64(index + 1)
		ticket, err := adapter.Stage(retainedINRequest(
			41, ordinal, uint32(index), 64))
		if err != nil {
			t.Fatalf("stage for retire reason %d: %v", reason, err)
		}
		if err := adapter.Retire(
			ticket, reason, adapterTestTime(ordinal)); err != nil {
			t.Fatalf("retire reason %d: %v", reason, err)
		}
		if err := adapter.Retire(
			ticket, reason, adapterTestTime(ordinal)); !errors.Is(
			err, errDormantRetainedUSBInvalidTicket) {
			t.Fatalf("duplicate retire reason %d: %v", reason, err)
		}
	}
}

func TestDormantRetainedUSBAdapterCompletionAndRetireErrorsQuarantineOwnership(t *testing.T) {
	t.Run("canonical resolve failure", func(t *testing.T) {
		adapter, _, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
		configureRetainedTestPersona(t, adapter)
		ticket, err := adapter.Stage(retainedINRequest(41, 3, 3, 64))
		if err != nil {
			t.Fatal(err)
		}
		if preparation, err := adapter.Prepare(
			ticket, make([]byte, 64), adapterTestTime(1)); err != nil ||
			preparation.Result != retainedusb.ResultData {
			t.Fatalf("prepare = (%+v, %v)", preparation, err)
		}
		adapter.coordinator.mu.Lock()
		adapter.coordinator.activeResponse.claim.token++
		adapter.coordinator.mu.Unlock()
		if err := adapter.Complete(
			ticket, true, adapterTestTime(1)); err == nil {
			t.Fatal("injected canonical Resolve failure was accepted")
		}
		assertRetainedUSBAdapterOwnershipQuarantined(
			t, adapter, lease, retainedusb.LaneInterruptIn, ticket)
	})

	t.Run("coordinator retire failure", func(t *testing.T) {
		adapter, _, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
		ticket, err := adapter.Stage(retainedINRequest(41, 1, 1, 64))
		if err != nil {
			t.Fatal(err)
		}
		adapter.coordinator.mu.Lock()
		adapter.coordinator.inStage.ticket.token++
		adapter.coordinator.mu.Unlock()
		if err := adapter.Retire(
			ticket, retainedusb.RetireEndpointReset,
			adapterTestTime(1)); err == nil {
			t.Fatal("injected coordinator retire failure was accepted")
		}
		assertRetainedUSBAdapterOwnershipQuarantined(
			t, adapter, lease, retainedusb.LaneInterruptIn, ticket)
	})
}

func TestDormantRetainedUSBAdapterQuarantineContainmentIsExactConcurrentAndOnce(t *testing.T) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	configureRetainedTestPersona(t, adapter)
	ticket, err := adapter.Stage(retainedINRequest(41, 3, 3, 64))
	if err != nil {
		t.Fatal(err)
	}
	if preparation, err := adapter.Prepare(
		ticket, make([]byte, 64), adapterTestTime(1)); err != nil ||
		preparation.Result != retainedusb.ResultData {
		t.Fatalf("prepare = (%+v, %v)", preparation, err)
	}
	adapter.coordinator.mu.Lock()
	adapter.coordinator.activeResponse.claim.token++
	adapter.coordinator.mu.Unlock()
	terminalErr := adapter.Complete(ticket, true, adapterTestTime(1))
	if !errors.Is(terminalErr, ErrInvalidControllerPersonaClaim) {
		t.Fatalf("injected completion failure = %v", terminalErr)
	}

	forged := lease
	forged.ImportToken++
	if result, err := adapter.CancelAndDrain(
		forged, retainedusb.ImportCloseInvariantFailure,
		time.Now().Add(time.Second)); !errors.Is(
		err, errDormantRetainedUSBInvalidImport) ||
		result.State != retainedusb.ImportDrainInvalid {
		t.Fatalf("forged drain rejection = (%+v, %v)", result, err)
	}
	_, _, drainCount := executor.snapshot()
	if drainCount != 0 {
		t.Fatalf("forged lease invoked containment drain %d times", drainCount)
	}

	type containmentOutcome struct {
		result retainedusb.ImportDrainResult
		err    error
	}
	const contenders = 32
	deadline := time.Now().Add(2 * time.Second)
	outcomes := make(chan containmentOutcome, contenders)
	var wait sync.WaitGroup
	for range contenders {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := adapter.CancelAndDrain(
				lease, retainedusb.ImportCloseInvariantFailure, deadline)
			outcomes <- containmentOutcome{result: result, err: err}
		}()
	}
	wait.Wait()
	close(outcomes)
	for outcome := range outcomes {
		if outcome.result.State != retainedusb.ImportDrainQuarantined ||
			outcome.result.Lease != lease ||
			outcome.result.Reason != retainedusb.ImportCloseInvariantFailure ||
			!errors.Is(outcome.err, errDormantRetainedUSBQuarantined) ||
			!errors.Is(outcome.err, ErrInvalidControllerPersonaClaim) {
			t.Fatalf("exact quarantine containment = (%+v, %v)",
				outcome.result, outcome.err)
		}
	}
	_, neutrals, drainCount := executor.snapshot()
	if drainCount != 1 || len(neutrals) != 0 {
		t.Fatalf("containment side effects = drains %d, neutrals %+v",
			drainCount, neutrals)
	}
	inIndex, _ := retainedusb.LaneInterruptIn.Index()
	adapter.mu.Lock()
	state := adapter.state
	retainedSlot := adapter.slots[inIndex]
	retainedLease := adapter.boundLease
	contained := adapter.quarantineContained
	done := adapter.localDone
	adapter.mu.Unlock()
	if state != dormantRetainedUSBQuarantine ||
		retainedSlot.state != dormantRetainedUSBSlotPreparing ||
		retainedSlot.ticket != ticket || retainedLease != lease || !contained {
		t.Fatalf("containment released quarantined ownership: state=%d slot=%+v lease=%+v contained=%t",
			state, retainedSlot, retainedLease, contained)
	}
	select {
	case <-done:
	default:
		t.Fatal("quarantine containment did not join the idle worker")
	}
	if _, err := adapter.DisconnectNeutral(
		lease, retainedusb.ImportCloseInvariantFailure,
		time.Now().Add(time.Second)); !errors.Is(
		err, errDormantRetainedUSBInvalidImport) {
		t.Fatalf("quarantine containment authorized neutral: %v", err)
	}
}

func TestDormantRetainedUSBAdapterQuarantineContainmentCancelsInFlightLocal(t *testing.T) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	release := startBlockedRetainedMotorExecution(t, adapter, executor, false)
	defer close(release)
	ticket, terminalErr := forceRetainedINRetireFailure(t, adapter)
	if !errors.Is(terminalErr, errInvalidControllerPersonaTransportTicket) {
		t.Fatalf("injected retirement failure = %v", terminalErr)
	}

	result, err := adapter.CancelAndDrain(
		lease, retainedusb.ImportCloseInvariantFailure,
		time.Now().Add(time.Second))
	if result.State != retainedusb.ImportDrainQuarantined ||
		!errors.Is(err, errDormantRetainedUSBQuarantined) ||
		!errors.Is(err, errInvalidControllerPersonaTransportTicket) {
		t.Fatalf("in-flight quarantine containment = (%+v, %v)", result, err)
	}
	_, neutrals, drainCount := executor.snapshot()
	if drainCount != 1 || len(neutrals) != 0 {
		t.Fatalf("in-flight containment = drains %d neutrals %+v",
			drainCount, neutrals)
	}
	inIndex, _ := retainedusb.LaneInterruptIn.Index()
	adapter.mu.Lock()
	done := adapter.localDone
	state := adapter.state
	retainedSlot := adapter.slots[inIndex]
	retainedLease := adapter.boundLease
	adapter.mu.Unlock()
	select {
	case <-done:
	default:
		t.Fatal("in-flight containment did not join the worker")
	}
	if state != dormantRetainedUSBQuarantine ||
		retainedSlot.ticket != ticket || retainedLease != lease {
		t.Fatalf("in-flight containment released ownership: state=%d slot=%+v lease=%+v",
			state, retainedSlot, retainedLease)
	}
}

func TestDormantRetainedUSBAdapterQuarantineContainmentBoundsNoncooperativeLocal(t *testing.T) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	release := startBlockedRetainedMotorExecution(t, adapter, executor, true)
	ticket, terminalErr := forceRetainedINRetireFailure(t, adapter)
	if !errors.Is(terminalErr, errInvalidControllerPersonaTransportTicket) {
		close(release)
		t.Fatalf("injected retirement failure = %v", terminalErr)
	}

	started := time.Now()
	result, err := adapter.CancelAndDrain(
		lease, retainedusb.ImportCloseInvariantFailure,
		started.Add(40*time.Millisecond))
	elapsed := time.Since(started)
	if result.State != retainedusb.ImportDrainQuarantined ||
		!errors.Is(err, errDormantRetainedUSBDeadline) ||
		!errors.Is(err, errInvalidControllerPersonaTransportTicket) {
		close(release)
		t.Fatalf("noncooperative quarantine containment = (%+v, %v)", result, err)
	}
	if elapsed > 250*time.Millisecond {
		close(release)
		t.Fatalf("noncooperative containment returned after %v", elapsed)
	}
	_, neutrals, drainCount := executor.snapshot()
	if drainCount != 1 || len(neutrals) != 0 {
		close(release)
		t.Fatalf("noncooperative containment = drains %d neutrals %+v",
			drainCount, neutrals)
	}
	inIndex, _ := retainedusb.LaneInterruptIn.Index()
	adapter.mu.Lock()
	state := adapter.state
	retainedSlot := adapter.slots[inIndex]
	retainedLease := adapter.boundLease
	adapter.mu.Unlock()
	if state != dormantRetainedUSBQuarantine ||
		retainedSlot.ticket != ticket || retainedLease != lease {
		close(release)
		t.Fatalf("noncooperative containment released ownership: state=%d slot=%+v lease=%+v",
			state, retainedSlot, retainedLease)
	}
	if err := adapter.ReconnectDormantSession(
		newScriptedControllerPersonaLocalExecutor(), 100*time.Millisecond,
		time.Now()); !errors.Is(err, errDormantRetainedUSBInvalidImport) {
		close(release)
		t.Fatalf("noncooperative containment allowed successor: %v", err)
	}

	close(release)
	adapter.mu.Lock()
	done := adapter.localDone
	adapter.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("noncooperative worker did not finish after release")
	}
	result, err = adapter.CancelAndDrain(
		lease, retainedusb.ImportCloseInvariantFailure,
		time.Now().Add(time.Second))
	if result.State != retainedusb.ImportDrainQuarantined ||
		!errors.Is(err, errDormantRetainedUSBDeadline) {
		t.Fatalf("repeated noncooperative containment = (%+v, %v)", result, err)
	}
	_, _, drainCount = executor.snapshot()
	if drainCount != 1 {
		t.Fatalf("repeated containment drained %d times", drainCount)
	}
}

func startBlockedRetainedMotorExecution(
	t *testing.T,
	adapter *DormantRetainedUSBAdapter,
	executor *scriptedControllerPersonaLocalExecutor,
	ignoreCancel bool,
) chan struct{} {
	t.Helper()
	configureRetainedTestPersona(t, adapter)
	retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	motorWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(
		motorWire, 1, testDirectMotorBody()); err != nil {
		t.Fatal(err)
	}
	executor.mu.Lock()
	executor.executeEntered = make(chan struct{}, 1)
	release := make(chan struct{})
	executor.executeRelease = release
	executor.ignoreCancel = ignoreCancel
	executor.mu.Unlock()
	retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 4, 4, motorWire), 2)
	triggerAndRetireLocalAction(t, adapter, 5, 3)
	select {
	case <-executor.executeEntered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("blocked local motor execution did not start")
	}
	return release
}

func forceRetainedINRetireFailure(
	t *testing.T,
	adapter *DormantRetainedUSBAdapter,
) (retainedusb.Ticket, error) {
	t.Helper()
	ticket, err := adapter.Stage(retainedINRequest(41, 6, 6, 64))
	if err != nil {
		t.Fatal(err)
	}
	adapter.coordinator.mu.Lock()
	adapter.coordinator.inStage.ticket.token++
	adapter.coordinator.mu.Unlock()
	err = adapter.Retire(
		ticket, retainedusb.RetireInvariantFailure, adapterTestTime(4))
	if err == nil {
		t.Fatal("injected coordinator retirement failure was accepted")
	}
	return ticket, err
}

func assertRetainedUSBAdapterOwnershipQuarantined(
	t *testing.T,
	adapter *DormantRetainedUSBAdapter,
	lease retainedusb.ImportLease,
	lane retainedusb.Lane,
	ticket retainedusb.Ticket,
) {
	t.Helper()
	index, _ := lane.Index()
	adapter.mu.Lock()
	state := adapter.state
	retainedSlot := adapter.slots[index]
	retainedLease := adapter.boundLease
	adapter.mu.Unlock()
	if state != dormantRetainedUSBQuarantine ||
		retainedSlot.state != dormantRetainedUSBSlotPreparing ||
		retainedSlot.ticket != ticket || retainedLease != lease {
		t.Fatalf("failed terminal ownership was not retained: state=%d slot=%+v lease=%+v",
			state, retainedSlot, retainedLease)
	}
	if _, err := adapter.Stage(retainedINRequest(
		41, 99, 99, 64)); !errors.Is(err, errDormantRetainedUSBInvalidRequest) {
		t.Fatalf("quarantined lane was reused: %v", err)
	}
	successor := lease
	successor.ImportToken++
	successor.SessionGeneration++
	result, err := adapter.BindImport(successor, time.Now().Add(time.Second))
	if !errors.Is(err, errDormantRetainedUSBQuarantined) ||
		result.State != retainedusb.ImportBindQuarantined {
		t.Fatalf("quarantined successor bind = (%+v, %v)", result, err)
	}
}

func TestDormantRetainedUSBAdapterReadinessAndConcurrentSingleLane(t *testing.T) {
	adapter, _, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	epoch, ready := adapter.Readiness()
	if epoch == 0 || ready == nil || cap(ready) != 1 {
		t.Fatalf("readiness = (%d, %v cap=%d)", epoch, ready, cap(ready))
	}
	report := GamepadInputReportV1{State: InputStateV1{B: true}}
	if err := adapter.PublishSemanticInput(41, 2, report); err != nil {
		t.Fatal(err)
	}
	advanced, same := adapter.Readiness()
	if same != ready || advanced != epoch+1 {
		t.Fatalf("readiness after input = (%d, same=%t), want %d",
			advanced, same == ready, epoch+1)
	}
	select {
	case <-ready:
	default:
		t.Fatal("readiness wake was not latched")
	}

	var success atomic.Int32
	var winnerMu sync.Mutex
	var winner retainedusb.Ticket
	var wait sync.WaitGroup
	for index := 0; index < 64; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			ticket, err := adapter.Stage(retainedINRequest(
				41, uint64(index+1), uint32(index), 64))
			if err == nil {
				success.Add(1)
				winnerMu.Lock()
				winner = ticket
				winnerMu.Unlock()
			} else if !errors.Is(err, errDormantRetainedUSBLaneBusy) {
				t.Errorf("stage %d = %v", index, err)
			}
		}(index)
	}
	wait.Wait()
	if success.Load() != 1 {
		t.Fatalf("successful same-lane stages = %d", success.Load())
	}
	if err := adapter.Retire(
		winner, retainedusb.RetireUnlink, adapterTestTime(0)); err != nil {
		t.Fatal(err)
	}
}

func TestDormantRetainedUSBAdapterResetRetiresOrdinarySelectionAndClearsWithoutURB(
	t *testing.T,
) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	configureRetainedTestPersona(t, adapter)
	retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 3, 3, 64), 1)

	motor := testDirectMotorBody()
	wire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(wire, 0x61, motor); err != nil {
		t.Fatal(err)
	}
	retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 4, 4, wire), 2)
	if !adapter.coordinator.pendingLocal() {
		t.Fatal("motor command did not leave its exact local selection pending")
	}

	reset := adapterTestReset(lease, 71, 1)
	result, err := adapter.ResetAndRestart(
		reset, time.Now().Add(time.Second))
	if err != nil || result != (retainedusb.ImportResetResult{
		Lease: reset, State: retainedusb.ImportResetSafe,
	}) {
		t.Fatalf("reset = (%+v, %v)", result, err)
	}
	executor.mu.Lock()
	resetNeutrals := append(
		[]ControllerPersonaLocalExecution(nil), executor.resetNeutrals...)
	resetDrains := executor.resetDrainCount
	executions := append(
		[]ControllerPersonaLocalExecution(nil), executor.executions...)
	executor.mu.Unlock()
	if resetDrains != 1 || len(resetNeutrals) != 1 ||
		resetNeutrals[0].Action != ControllerPersonaClearOutputs ||
		resetNeutrals[0].Generation != 2 ||
		resetNeutrals[0].ClearEpoch == 0 {
		t.Fatalf("reset local boundary: drains=%d neutral=%+v",
			resetDrains, resetNeutrals)
	}
	for _, execution := range executions {
		if execution.Action == ControllerPersonaApplyDirectMotor {
			t.Fatalf("predecessor motor replayed across reset: %+v", execution)
		}
	}
	snapshot, ok := adapter.coordinator.snapshot()
	if !ok || snapshot.Generation != 2 || !snapshot.Attached ||
		snapshot.USBState != USBControlDeviceAddressed ||
		snapshot.Feedback.ClearEpoch != resetNeutrals[0].ClearEpoch ||
		snapshot.ClaimOutstanding || snapshot.RetryPending {
		t.Fatalf("reset snapshot = (%+v, %t)", snapshot, ok)
	}

	// An exact replay is observational only and cannot manufacture another
	// generation or clear epoch. The cached result does not require a fresh
	// execution deadline because it invokes no callback.
	repeated, err := adapter.ResetAndRestart(
		reset, time.Time{})
	if err != nil || repeated != result {
		t.Fatalf("repeated reset = (%+v, %v)", repeated, err)
	}
	executor.mu.Lock()
	if executor.resetDrainCount != 1 || len(executor.resetNeutrals) != 1 {
		t.Fatalf("repeated reset duplicated local work: drains=%d neutral=%+v",
			executor.resetDrainCount, executor.resetNeutrals)
	}
	executor.mu.Unlock()

	stale := reset
	stale.ResetToken++
	staleResult, err := adapter.ResetAndRestart(
		stale, time.Now().Add(time.Second))
	if !errors.Is(err, errDormantRetainedUSBInvalidImport) {
		t.Fatalf("forged reset = %v", err)
	}
	if staleResult != (retainedusb.ImportResetResult{
		Lease: stale, State: retainedusb.ImportResetInvalid,
	}) {
		t.Fatalf("forged reset result = %+v", staleResult)
	}
	ticket, err := adapter.Stage(retainedINRequest(41, 5, 5, 64))
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Generation != 2 {
		t.Fatalf("successor ticket generation = %d", ticket.Generation)
	}
	if err := adapter.Retire(
		ticket, retainedusb.RetireDeviceReset, adapterTestTime(3)); err != nil {
		t.Fatal(err)
	}
}

func TestDormantRetainedUSBAdapterResetRequiresDrainedTicketsAndFencesClears(
	t *testing.T,
) {
	t.Run("undrained staged ticket", func(t *testing.T) {
		adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
		oldTicket, err := adapter.Stage(retainedINRequest(41, 1, 1, 64))
		if err != nil {
			t.Fatal(err)
		}
		reset := adapterTestReset(lease, 81, 1)
		result, err := adapter.ResetAndRestart(
			reset, time.Now().Add(time.Second))
		if err == nil || result.State != retainedusb.ImportResetQuarantined {
			t.Fatalf("undrained reset = (%+v, %v)", result, err)
		}
		if _, err := adapter.Prepare(
			oldTicket, make([]byte, 64), adapterTestTime(1)); err == nil {
			t.Fatal("quarantined predecessor ticket remained admissible")
		}
		executor.mu.Lock()
		if executor.resetDrainCount != 1 || len(executor.resetNeutrals) != 0 {
			t.Fatalf("undrained reset touched executor: drains=%d neutral=%+v",
				executor.resetDrainCount, executor.resetNeutrals)
		}
		executor.mu.Unlock()
	})

	for _, retry := range []bool{false, true} {
		name := "selected clear"
		if retry {
			name = "retry clear"
		}
		t.Run(name, func(t *testing.T) {
			adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
			if err := adapter.coordinator.beginUSBReset(1); err != nil {
				t.Fatal(err)
			}
			var clearEpoch uint64
			if retry {
				localLease, present, err := adapter.coordinator.admitLocal(1)
				if err != nil || !present {
					t.Fatalf("admit clear = (%+v, %t, %v)",
						localLease, present, err)
				}
				clearEpoch = localLease.clearEpoch
				if err := adapter.coordinator.completeLocal(
					localLease, ControllerPersonaDeliveryFailed, 1); err != nil {
					t.Fatal(err)
				}
			}
			reset := adapterTestReset(lease, 82, 1)
			result, err := adapter.ResetAndRestart(
				reset, time.Now().Add(time.Second))
			if !errors.Is(err, ErrControllerPersonaBoundaryBlocked) ||
				result.State != retainedusb.ImportResetQuarantined {
				t.Fatalf("clear-fenced reset = (%+v, %v)", result, err)
			}
			snapshot, ok := adapter.coordinator.snapshot()
			if !ok || !snapshot.ClaimOutstanding ||
				snapshot.Feedback.ClearEpoch != 0 {
				t.Fatalf("clear fence snapshot = (%+v, %t)", snapshot, ok)
			}
			if retry && clearEpoch == 0 {
				t.Fatal("retry lost its original clear epoch")
			}
			if retry {
				reclaimed, present, reclaimErr :=
					adapter.coordinator.admitLocal(1)
				if reclaimErr != nil || !present ||
					reclaimed.action != ControllerPersonaClearOutputs ||
					reclaimed.clearEpoch != clearEpoch {
					t.Fatalf("retry clear changed across rejected reset: (%+v, %t, %v)",
						reclaimed, present, reclaimErr)
				}
			}
			executor.mu.Lock()
			if len(executor.resetNeutrals) != 0 {
				t.Fatalf("fenced clear was replaced: %+v", executor.resetNeutrals)
			}
			executor.mu.Unlock()
		})
	}
}

func TestDormantRetainedUSBAdapterResetCancelsAndJoinsActiveLocalBeforeBoundary(
	t *testing.T,
) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	configureRetainedTestPersona(t, adapter)
	retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	motor := testDirectMotorBody()
	wire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(wire, 0x62, motor); err != nil {
		t.Fatal(err)
	}
	executor.mu.Lock()
	executor.executeEntered = make(chan struct{}, 1)
	executor.executeRelease = make(chan struct{})
	executor.mu.Unlock()
	retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 4, 4, wire), 2)
	select {
	case <-executor.executeEntered:
	case <-time.After(time.Second):
		t.Fatal("ordinary local execution did not begin")
	}

	reset := adapterTestReset(lease, 86, 1)
	result, err := adapter.ResetAndRestart(
		reset, time.Now().Add(time.Second))
	if err != nil || result.State != retainedusb.ImportResetSafe {
		t.Fatalf("reset over active local = (%+v, %v)", result, err)
	}
	executor.mu.Lock()
	if executor.resetDrainCount != 1 || len(executor.resetNeutrals) != 1 {
		t.Fatalf("active-local reset boundary: drains=%d neutral=%+v",
			executor.resetDrainCount, executor.resetNeutrals)
	}
	executor.mu.Unlock()
	snapshot, ok := adapter.coordinator.snapshot()
	if !ok || snapshot.Generation != 2 ||
		snapshot.Feedback.DirectMotor != (RumbleBodyV1{}) ||
		snapshot.ClaimOutstanding || snapshot.RetryPending {
		t.Fatalf("active-local successor snapshot = (%+v, %t)", snapshot, ok)
	}
}

func TestDormantRetainedUSBAdapterResetNeutralTimeoutAndExhaustionQuarantine(
	t *testing.T,
) {
	t.Run("blocked reset fence", func(t *testing.T) {
		adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
		release := make(chan struct{})
		executor.mu.Lock()
		executor.resetDrainEntered = make(chan struct{}, 1)
		executor.resetDrainRelease = release
		executor.mu.Unlock()
		reset := adapterTestReset(lease, 90, 1)
		result, err := adapter.ResetAndRestart(
			reset, time.Now().Add(25*time.Millisecond))
		if !errors.Is(err, errDormantRetainedUSBDeadline) ||
			result.State != retainedusb.ImportResetQuarantined {
			t.Fatalf("blocked reset fence = (%+v, %v)", result, err)
		}
		executor.mu.Lock()
		resetDrains := executor.resetDrainCount
		resetNeutrals := len(executor.resetNeutrals)
		executor.mu.Unlock()
		if resetDrains != 1 || resetNeutrals != 0 {
			t.Fatalf("blocked reset fence effects: drains=%d neutral=%d",
				resetDrains, resetNeutrals)
		}
		close(release)
		time.Sleep(time.Millisecond)
		if _, err := adapter.Stage(
			retainedINRequest(41, 1, 1, 64)); err == nil {
			t.Fatal("late reset-fence return reopened adapter admission")
		}
	})

	t.Run("blocked reset neutral", func(t *testing.T) {
		adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
		release := make(chan struct{})
		executor.mu.Lock()
		executor.resetNeutralEntered = make(chan struct{}, 1)
		executor.resetNeutralRelease = release
		executor.mu.Unlock()
		reset := adapterTestReset(lease, 91, 1)
		started := time.Now()
		result, err := adapter.ResetAndRestart(
			reset, time.Now().Add(25*time.Millisecond))
		if !errors.Is(err, errDormantRetainedUSBDeadline) ||
			result.State != retainedusb.ImportResetQuarantined ||
			time.Since(started) > 250*time.Millisecond {
			t.Fatalf("blocked neutral = (%+v, %v) after %s",
				result, err, time.Since(started))
		}
		close(release)
		time.Sleep(time.Millisecond)
		if _, err := adapter.Stage(
			retainedINRequest(41, 1, 1, 64)); err == nil {
			t.Fatal("late reset-neutral return reopened adapter admission")
		}
		if err := adapter.ReconnectDormantSession(
			newScriptedControllerPersonaLocalExecutor(),
			100*time.Millisecond, time.Now()); err == nil {
			t.Fatal("reset quarantine admitted reconnect")
		}
	})

	for _, test := range []struct {
		name   string
		mutate func(*DormantRetainedUSBAdapter)
	}{
		{name: "adapter generation", mutate: func(adapter *DormantRetainedUSBAdapter) {
			adapter.mu.Lock()
			adapter.generation = ^uint64(0)
			adapter.mu.Unlock()
		}},
		{name: "coordinator order", mutate: func(adapter *DormantRetainedUSBAdapter) {
			adapter.coordinator.mu.Lock()
			adapter.coordinator.nextOrder = ^uint64(0)
			adapter.coordinator.mu.Unlock()
		}},
		{name: "clear epoch", mutate: func(adapter *DormantRetainedUSBAdapter) {
			adapter.coordinator.mu.Lock()
			adapter.coordinator.engine.nextClearEpoch = ^uint64(0)
			adapter.coordinator.mu.Unlock()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
			test.mutate(adapter)
			before, _ := adapter.coordinator.snapshot()
			result, err := adapter.ResetAndRestart(
				adapterTestReset(lease, 92, 1), time.Now().Add(time.Second))
			if err == nil || result.State != retainedusb.ImportResetQuarantined {
				t.Fatalf("exhausted reset = (%+v, %v)", result, err)
			}
			after, _ := adapter.coordinator.snapshot()
			if after.Generation != before.Generation {
				t.Fatalf("exhaustion advanced generation: %d -> %d",
					before.Generation, after.Generation)
			}
			executor.mu.Lock()
			if len(executor.resetNeutrals) != 0 {
				t.Fatalf("exhaustion delivered neutral: %+v",
					executor.resetNeutrals)
			}
			executor.mu.Unlock()
		})
	}
}

func TestDormantRetainedUSBAdapterTerminalContainmentUpgradesResetDrained(
	t *testing.T,
) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	reset := adapterTestReset(lease, 93, 1)
	deadline := time.Now().Add(time.Second)
	if err := adapter.FenceAndDrainReset(reset, deadline); err != nil {
		t.Fatal(err)
	}

	drain, err := adapter.CancelAndDrain(
		lease, retainedusb.ImportCloseInvariantFailure, deadline)
	if err != nil || drain != (retainedusb.ImportDrainResult{
		Lease: lease, Reason: retainedusb.ImportCloseInvariantFailure,
		State: retainedusb.ImportDrainDrained,
	}) {
		t.Fatalf("terminal containment = (%+v, %v)", drain, err)
	}
	executor.mu.Lock()
	resetDrainCount := executor.resetDrainCount
	drainCount := executor.drainCount
	resetNeutrals := len(executor.resetNeutrals)
	neutrals := len(executor.neutrals)
	executor.mu.Unlock()
	if resetDrainCount != 1 || drainCount != 1 ||
		resetNeutrals != 0 || neutrals != 0 {
		t.Fatalf(
			"terminal containment effects: reset-drain=%d drain=%d reset-neutral=%d neutral=%d",
			resetDrainCount, drainCount, resetNeutrals, neutrals)
	}
	if _, err := adapter.Stage(
		retainedINRequest(41, 1, 1, 64)); err == nil {
		t.Fatal("terminal containment reopened reset-drained admission")
	}
}

func TestDormantRetainedUSBAdapterCancelDrainDisconnectNeutralAndReconnectFence(t *testing.T) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	if err := adapter.ReconnectDormantSession(
		newScriptedControllerPersonaLocalExecutor(), 100*time.Millisecond,
		adapterTestTime(1)); !errors.Is(err, errDormantRetainedUSBInvalidImport) {
		t.Fatalf("reconnect before neutral = %v", err)
	}

	deadline := time.Now().Add(time.Second)
	drain, err := adapter.CancelAndDrain(
		lease, retainedusb.ImportClosePeerDisconnect, deadline)
	if err != nil || drain.State != retainedusb.ImportDrainDrained ||
		drain.Lease != lease || drain.Reason != retainedusb.ImportClosePeerDisconnect {
		t.Fatalf("drain = (%+v, %v)", drain, err)
	}
	beforeLate, _, _ := executor.snapshot()
	late := ControllerPersonaLocalExecution{
		Action: ControllerPersonaPermitNormalUpstream, Generation: 1, Order: 1,
	}
	if err := executor.Execute(late, time.Now().Add(time.Second)); !errors.Is(
		err, errScriptedLocalExecutorCanceled) {
		t.Fatalf("late ordinary Execute after drain = %v", err)
	}
	afterLate, _, _ := executor.snapshot()
	if len(afterLate) != len(beforeLate) {
		t.Fatal("drained executor accepted a late ordinary effect")
	}
	forged := lease
	forged.ImportToken++
	forgedDisconnect, forgedErr := adapter.DisconnectNeutral(
		forged, retainedusb.ImportClosePeerDisconnect, deadline)
	if !errors.Is(forgedErr, errDormantRetainedUSBInvalidImport) ||
		forgedDisconnect.State != retainedusb.ImportDisconnectInvalid {
		t.Fatalf("forged disconnect = (%+v, %v)",
			forgedDisconnect, forgedErr)
	}
	_, forgedNeutrals, _ := executor.snapshot()
	if len(forgedNeutrals) != 0 {
		t.Fatalf("forged disconnect invoked neutral: %+v", forgedNeutrals)
	}
	disconnect, err := adapter.DisconnectNeutral(
		lease, retainedusb.ImportClosePeerDisconnect, deadline)
	if err != nil || disconnect.State != retainedusb.ImportDisconnectSafe ||
		disconnect.Lease != lease {
		t.Fatalf("disconnect = (%+v, %v)", disconnect, err)
	}
	_, neutrals, drainCount := executor.snapshot()
	if drainCount != 1 || len(neutrals) != 1 ||
		neutrals[0].Action != ControllerPersonaClearOutputs ||
		neutrals[0].ClearEpoch == 0 {
		t.Fatalf("terminal executor = drain %d neutral %+v", drainCount, neutrals)
	}
	if _, err := adapter.DisconnectNeutral(
		lease, retainedusb.ImportClosePeerDisconnect,
		time.Now().Add(time.Second)); !errors.Is(
		err, errDormantRetainedUSBInvalidImport) {
		t.Fatalf("duplicate disconnect = %v", err)
	}

	successorExecutor := newScriptedControllerPersonaLocalExecutor()
	if err := adapter.ReconnectDormantSession(
		successorExecutor, 100*time.Millisecond,
		time.Now()); err != nil {
		t.Fatal(err)
	}
	successorLease := adapterTestLease(adapter, 42)
	successorLease.ImportToken++
	result, err := adapter.BindImport(
		successorLease, time.Now().Add(time.Second))
	if err != nil || result.State != retainedusb.ImportBindBound {
		t.Fatalf("successor bind = (%+v, %v)", result, err)
	}
	if _, err := adapter.Stage(retainedINRequest(41, 1, 1, 64)); !errors.Is(err, errDormantRetainedUSBInvalidRequest) {
		t.Fatalf("predecessor session stage = %v", err)
	}
	snapshot, ok := adapter.coordinator.snapshot()
	if !ok || !snapshot.Attached || snapshot.USBState != USBControlDeviceAddressed ||
		snapshot.Generation != 3 {
		t.Fatalf("reconnected persona = %+v", snapshot)
	}
}

func TestDormantRetainedUSBAdapterCancelJoinsInFlightLocalBeforeDisconnect(t *testing.T) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	configureRetainedTestPersona(t, adapter)
	retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	motor := testDirectMotorBody()
	motorWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(motorWire, 1, motor); err != nil {
		t.Fatal(err)
	}
	executor.mu.Lock()
	executor.executeEntered = make(chan struct{}, 1)
	release := make(chan struct{})
	executor.executeRelease = release
	executor.mu.Unlock()
	retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 4, 4, motorWire), 2)
	select {
	case <-executor.executeEntered:
	case <-time.After(time.Second):
		t.Fatal("local execution did not start")
	}

	deadline := time.Now().Add(time.Second)
	drain, err := adapter.CancelAndDrain(
		lease, retainedusb.ImportCloseExplicitDetach, deadline)
	if err != nil || drain.State != retainedusb.ImportDrainDrained {
		t.Fatalf("drain in-flight = (%+v, %v)", drain, err)
	}
	disconnect, err := adapter.DisconnectNeutral(
		lease, retainedusb.ImportCloseExplicitDetach, deadline)
	if err != nil || disconnect.State != retainedusb.ImportDisconnectSafe {
		t.Fatalf("disconnect after in-flight = (%+v, %v)", disconnect, err)
	}
	_, neutrals, _ := executor.snapshot()
	if len(neutrals) != 1 || neutrals[0].Action != ControllerPersonaClearOutputs {
		t.Fatalf("neutral actions = %+v", neutrals)
	}
}

func TestDormantRetainedUSBAdapterLocalFailureRetriesExactTypedEffect(t *testing.T) {
	adapter, executor, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	configureRetainedTestPersona(t, adapter)
	retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	motor := testDirectMotorBody()
	motorWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(motorWire, 1, motor); err != nil {
		t.Fatal(err)
	}
	executor.mu.Lock()
	executor.executeErr = errors.New("injected terminal local failure")
	executor.mu.Unlock()
	retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 4, 4, motorWire), 2)
	waitForLocalExecution(t, executor, ControllerPersonaApplyDirectMotor)
	deadline := time.Now().Add(time.Second)
	for {
		snapshot, _ := adapter.coordinator.snapshot()
		if snapshot.OrdinaryFeedbackRetryPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed local action did not retain retry: %+v", snapshot)
		}
		time.Sleep(time.Millisecond)
	}
	executor.mu.Lock()
	executor.executeErr = nil
	executor.mu.Unlock()
	// The exact local retry must make progress without another host IN/OUT.
	for {
		executions, _, _ := executor.snapshot()
		var motors []ControllerPersonaLocalExecution
		for _, execution := range executions {
			if execution.Action == ControllerPersonaApplyDirectMotor {
				motors = append(motors, execution)
			}
		}
		if len(motors) >= 2 {
			if motors[0].DirectMotor != motor || motors[1].DirectMotor != motor ||
				motors[0].Generation != motors[1].Generation {
				t.Fatalf("local retry changed typed effect: %+v", motors)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("local retry did not execute: %+v", executions)
		}
		time.Sleep(time.Millisecond)
	}
	for {
		snapshot, _ := adapter.coordinator.snapshot()
		if !snapshot.OrdinaryFeedbackRetryPending && !snapshot.OrdinaryFeedbackOutstanding && snapshot.Feedback.DirectMotor == motor {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("local retry did not commit: %+v", snapshot)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDormantRetainedUSBAdapterFailedNeutralIsStickyAndNotRepeated(t *testing.T) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	executor.mu.Lock()
	executor.neutralErr = errors.New("injected neutral failure")
	executor.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	if drain, err := adapter.CancelAndDrain(
		lease, retainedusb.ImportCloseWriteFailure, deadline); err != nil ||
		drain.State != retainedusb.ImportDrainDrained {
		t.Fatalf("drain = (%+v, %v)", drain, err)
	}
	disconnect, err := adapter.DisconnectNeutral(
		lease, retainedusb.ImportCloseWriteFailure, deadline)
	if err == nil || disconnect.State != retainedusb.ImportDisconnectQuarantined {
		t.Fatalf("failed neutral = (%+v, %v)", disconnect, err)
	}
	_, neutrals, _ := executor.snapshot()
	if len(neutrals) != 1 {
		t.Fatalf("neutral attempts = %d", len(neutrals))
	}
	if _, err := adapter.DisconnectNeutral(
		lease, retainedusb.ImportCloseWriteFailure,
		time.Now().Add(time.Second)); !errors.Is(
		err, errDormantRetainedUSBInvalidImport) {
		t.Fatalf("repeated failed neutral = %v", err)
	}
	_, neutrals, _ = executor.snapshot()
	if len(neutrals) != 1 {
		t.Fatalf("failed neutral repeated: %+v", neutrals)
	}
	if err := adapter.ReconnectDormantSession(
		newScriptedControllerPersonaLocalExecutor(), 100*time.Millisecond,
		time.Now()); !errors.Is(err, errDormantRetainedUSBInvalidImport) {
		t.Fatalf("reconnect after failed neutral = %v", err)
	}
}

func TestDormantRetainedUSBAdapterNoncooperativeNeutralReturnsBoundedAndQuarantines(t *testing.T) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	if drain, err := adapter.CancelAndDrain(
		lease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(time.Second)); err != nil ||
		drain.State != retainedusb.ImportDrainDrained {
		t.Fatalf("drain = (%+v, %v)", drain, err)
	}
	executor.mu.Lock()
	executor.neutralEntered = make(chan struct{}, 1)
	release := make(chan struct{})
	executor.neutralRelease = release
	executor.neutralExited = make(chan struct{}, 1)
	executor.mu.Unlock()
	started := time.Now()
	disconnect, err := adapter.DisconnectNeutral(
		lease, retainedusb.ImportCloseExplicitDetach,
		started.Add(40*time.Millisecond))
	elapsed := time.Since(started)
	if err == nil || !errors.Is(err, errDormantRetainedUSBDeadline) ||
		disconnect.State != retainedusb.ImportDisconnectQuarantined {
		close(release)
		t.Fatalf("noncooperative neutral = (%+v, %v)", disconnect, err)
	}
	if elapsed > 250*time.Millisecond {
		close(release)
		t.Fatalf("deadline-bounded neutral returned after %v", elapsed)
	}
	_, neutrals, _ := executor.snapshot()
	if len(neutrals) != 1 ||
		neutrals[0].Action != ControllerPersonaClearOutputs {
		close(release)
		t.Fatalf("neutral attempts = %+v", neutrals)
	}
	if err := adapter.ReconnectDormantSession(
		newScriptedControllerPersonaLocalExecutor(), 100*time.Millisecond,
		time.Now()); !errors.Is(err, errDormantRetainedUSBInvalidImport) {
		close(release)
		t.Fatalf("successor after ambiguous neutral = %v", err)
	}
	close(release)
	select {
	case <-executor.neutralExited:
	case <-time.After(time.Second):
		t.Fatal("noncooperative neutral did not exit after release")
	}
	_, neutrals, _ = executor.snapshot()
	if len(neutrals) != 1 {
		t.Fatalf("ambiguous neutral repeated: %+v", neutrals)
	}
}

func TestDormantRetainedUSBAdapterContainsLocalPanicAndQuarantinesAfterNeutral(t *testing.T) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	configureRetainedTestPersona(t, adapter)
	retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	motorWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(
		motorWire, 1, testDirectMotorBody()); err != nil {
		t.Fatal(err)
	}
	executor.mu.Lock()
	executor.executeEntered = make(chan struct{}, 1)
	release := make(chan struct{})
	executor.executeRelease = release
	executor.ignoreCancel = true
	executor.executePanic = "injected local panic"
	executor.mu.Unlock()
	retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 4, 4, motorWire), 2)
	triggerAndRetireLocalAction(t, adapter, 5, 3)

	select {
	case <-executor.executeEntered:
	case <-time.After(time.Second):
		t.Fatal("local execution did not reach the panic race")
	}
	type drainOutcome struct {
		result retainedusb.ImportDrainResult
		err    error
	}
	drainDone := make(chan drainOutcome, 1)
	go func() {
		result, err := adapter.CancelAndDrain(
			lease, retainedusb.ImportCloseInvariantFailure,
			time.Now().Add(time.Second))
		drainDone <- drainOutcome{result: result, err: err}
	}()
	for {
		_, _, drainCount := executor.snapshot()
		if drainCount == 1 {
			break
		}
		select {
		case outcome := <-drainDone:
			t.Fatalf("outer drain returned before the blocked panic: (%+v, %v)",
				outcome.result, outcome.err)
		case <-time.After(time.Millisecond):
		}
	}
	close(release)

	deadline := time.Now().Add(time.Second)
	for {
		adapter.mu.Lock()
		fatal := adapter.fatalLocalError
		adapter.mu.Unlock()
		if errors.Is(fatal, errDormantRetainedUSBLocalPanic) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("local panic was not contained and recorded: %v", fatal)
		}
		time.Sleep(time.Millisecond)
	}
	drain := retainedusb.ImportDrainResult{}
	var err error
	select {
	case outcome := <-drainDone:
		drain, err = outcome.result, outcome.err
	case <-time.After(time.Second):
		t.Fatal("panic/outer drain race deadlocked")
	}
	if err != nil || drain.State != retainedusb.ImportDrainDrained {
		t.Fatalf("panic drain = (%+v, %v)", drain, err)
	}

	executions, _, _ := executor.snapshot()
	var motorAttempts int
	for _, execution := range executions {
		if execution.Action == ControllerPersonaApplyDirectMotor {
			motorAttempts++
		}
	}
	if motorAttempts != 1 {
		t.Fatalf("panicked motor execution was retried %d times", motorAttempts)
	}

	disconnect, err := adapter.DisconnectNeutral(
		lease, retainedusb.ImportCloseInvariantFailure,
		time.Now().Add(time.Second))
	if !errors.Is(err, errDormantRetainedUSBLocalPanic) ||
		disconnect.State != retainedusb.ImportDisconnectQuarantined {
		t.Fatalf("panic disconnect = (%+v, %v)", disconnect, err)
	}
	_, neutrals, _ := executor.snapshot()
	if len(neutrals) != 1 || neutrals[0].Action != ControllerPersonaClearOutputs {
		t.Fatalf("panic terminal neutral = %+v", neutrals)
	}
	_, _, panicDrainCount := executor.snapshot()
	if panicDrainCount != 1 {
		t.Fatalf("panic/outer drain invoked executor %d times", panicDrainCount)
	}
	snapshot, ok := adapter.coordinator.snapshot()
	if !ok || snapshot.Attached || snapshot.USBState != USBControlDeviceDetached {
		t.Fatalf("panic terminal persona = %+v", snapshot)
	}
}

func TestDormantRetainedUSBAdapterNoncooperativeExecuteTimesOutBoundedAndRetainsSession(t *testing.T) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	configureRetainedTestPersona(t, adapter)
	retainedPrepareAndComplete(
		t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	motorWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(
		motorWire, 1, testDirectMotorBody()); err != nil {
		t.Fatal(err)
	}
	executor.mu.Lock()
	executor.executeEntered = make(chan struct{}, 1)
	release := make(chan struct{})
	executor.executeRelease = release
	executor.ignoreCancel = true
	executor.mu.Unlock()
	retainedPrepareAndComplete(
		t, adapter, retainedOUTRequest(41, 4, 4, motorWire), 2)
	triggerAndRetireLocalAction(t, adapter, 5, 3)
	select {
	case <-executor.executeEntered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("noncooperative local execution did not start")
	}

	started := time.Now()
	closeDeadline := started.Add(40 * time.Millisecond)
	drain, err := adapter.CancelAndDrain(
		lease, retainedusb.ImportCloseExplicitDetach, closeDeadline)
	elapsed := time.Since(started)
	if err == nil || !errors.Is(err, errDormantRetainedUSBDeadline) ||
		drain.State != retainedusb.ImportDrainQuarantined {
		close(release)
		t.Fatalf("noncooperative drain = (%+v, %v)", drain, err)
	}
	if elapsed > 250*time.Millisecond {
		close(release)
		t.Fatalf("deadline-bounded drain returned after %v", elapsed)
	}
	_, _, drainCount := executor.snapshot()
	if drainCount != 1 {
		close(release)
		t.Fatalf("noncooperative executor drain calls = %d", drainCount)
	}
	adapter.mu.Lock()
	state := adapter.state
	retainedLease := adapter.boundLease
	adapter.mu.Unlock()
	if state != dormantRetainedUSBQuarantine || retainedLease != lease {
		close(release)
		t.Fatalf("timed-out session was not retained: state=%d lease=%+v",
			state, retainedLease)
	}
	if _, err := adapter.DisconnectNeutral(
		lease, retainedusb.ImportCloseExplicitDetach,
		time.Now().Add(time.Second)); !errors.Is(
		err, errDormantRetainedUSBInvalidImport) {
		close(release)
		t.Fatalf("disconnect after ambiguous drain = %v", err)
	}
	if err := adapter.ReconnectDormantSession(
		newScriptedControllerPersonaLocalExecutor(), 100*time.Millisecond,
		time.Now()); !errors.Is(err, errDormantRetainedUSBInvalidImport) {
		close(release)
		t.Fatalf("successor after ambiguous drain = %v", err)
	}
	close(release)
	adapter.mu.Lock()
	localDone := adapter.localDone
	adapter.mu.Unlock()
	select {
	case <-localDone:
	case <-time.After(time.Second):
		t.Fatal("retained noncooperative worker did not terminate after release")
	}
	_, _, drainCount = executor.snapshot()
	if drainCount != 1 {
		t.Fatalf("cleanup repeated noncooperative drain: %d", drainCount)
	}
}

func TestDormantRetainedUSBAdapterExpiredDrainDeadlineStillFencesIdleWorkerOnce(t *testing.T) {
	adapter, executor, lease := newBoundTestRetainedUSBAdapter(t, []byte{1})
	drain, err := adapter.CancelAndDrain(
		lease, retainedusb.ImportCloseContextCanceled,
		time.Now().Add(-time.Millisecond))
	if !errors.Is(err, errDormantRetainedUSBDeadline) ||
		drain.State != retainedusb.ImportDrainQuarantined {
		t.Fatalf("expired drain = (%+v, %v)", drain, err)
	}
	adapter.mu.Lock()
	done := adapter.localDone
	state := adapter.state
	adapter.mu.Unlock()
	if state != dormantRetainedUSBQuarantine {
		t.Fatalf("expired drain state = %d", state)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expired drain left the idle worker running")
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, _, drainCount := executor.snapshot()
		if drainCount == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expired drain did not fence executor: calls=%d", drainCount)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDormantRetainedUSBAdapterStageRetireWarmPathAllocations(t *testing.T) {
	adapter, _, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	request := retainedINRequest(41, 1, 0, 64)
	allocations := testing.AllocsPerRun(1000, func() {
		ticket, err := adapter.Stage(request)
		if err != nil {
			panic(err)
		}
		if err := adapter.Retire(
			ticket, retainedusb.RetireUnlink, adapterTestTime(0)); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("warm Stage/Retire allocations = %f", allocations)
	}
}

func TestDormantRetainedUSBAdapterCountersFailBeforeWrap(t *testing.T) {
	adapter, _, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	adapter.mu.Lock()
	adapter.nextTicket = ^uint64(0)
	adapter.mu.Unlock()
	if _, err := adapter.Stage(retainedINRequest(41, 1, 1, 64)); !errors.Is(
		err, errDormantRetainedUSBTokenExhausted) {
		t.Fatalf("ticket exhaustion = %v", err)
	}
	adapter.mu.Lock()
	if !adapter.slotsEmptyLocked() {
		adapter.mu.Unlock()
		t.Fatal("ticket exhaustion left a staged lane")
	}
	adapter.nextTicket = 0
	adapter.readinessEpoch = ^uint64(0) - 1
	adapter.mu.Unlock()
	if err := adapter.PublishSemanticInput(
		41, 2, GamepadInputReportV1{State: InputStateV1{A: true}}); !errors.Is(
		err, errDormantRetainedUSBReadinessExhausted) {
		t.Fatalf("readiness exhaustion = %v", err)
	}
	epoch, ready := adapter.Readiness()
	if epoch != ^uint64(0)-1 || ready == nil {
		t.Fatalf("readiness wrapped or channel changed: epoch=%d ready=%v", epoch, ready)
	}
	adapter.mu.Lock()
	state := adapter.state
	adapter.mu.Unlock()
	if state != dormantRetainedUSBQuarantine {
		t.Fatalf("readiness exhaustion state = %d", state)
	}

	generationAdapter, _, generationLease := newBoundTestRetainedUSBAdapter(
		t, []byte{1})
	generationAdapter.mu.Lock()
	generationAdapter.generation = ^uint64(0)
	generationAdapter.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	if drain, err := generationAdapter.CancelAndDrain(
		generationLease, retainedusb.ImportCloseExplicitDetach, deadline); err != nil ||
		drain.State != retainedusb.ImportDrainDrained {
		t.Fatalf("generation drain = (%+v, %v)", drain, err)
	}
	if disconnect, err := generationAdapter.DisconnectNeutral(
		generationLease, retainedusb.ImportCloseExplicitDetach, deadline); err != nil ||
		disconnect.State != retainedusb.ImportDisconnectSafe {
		t.Fatalf("generation disconnect = (%+v, %v)", disconnect, err)
	}
	if err := generationAdapter.ReconnectDormantSession(
		newScriptedControllerPersonaLocalExecutor(), 100*time.Millisecond,
		time.Now()); !errors.Is(err, errDormantRetainedUSBInvalidImport) {
		t.Fatalf("generation wrap was accepted: %v", err)
	}
}

func TestDormantRetainedUSBAdapterHasOnlyDormantCompositionCallSite(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	var publicCallSites []string
	var privateCallSites []string
	var publicReferences []string
	var privateReferences []string
	err := filepath.WalkDir(repositoryRoot, func(
		path string,
		entry fs.DirEntry,
		walkErr error,
	) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, parseErr := parser.ParseFile(
			token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			if identifier, ok := node.(*ast.Ident); ok {
				switch identifier.Name {
				case "NewDormantRetainedUSBAdapter":
					publicReferences = append(publicReferences, path)
				case "newDormantRetainedUSBAdapter":
					privateReferences = append(privateReferences, path)
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			called := ""
			switch function := call.Fun.(type) {
			case *ast.Ident:
				called = function.Name
			case *ast.SelectorExpr:
				called = function.Sel.Name
			}
			if called == "NewDormantRetainedUSBAdapter" {
				publicCallSites = append(publicCallSites, path)
			}
			if called == "newDormantRetainedUSBAdapter" {
				privateCallSites = append(privateCallSites, path)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(publicCallSites) != 0 {
		t.Fatalf("exported dormant adapter has production call sites: %v",
			publicCallSites)
	}
	if len(publicReferences) != 1 || !strings.HasSuffix(
		filepath.ToSlash(publicReferences[0]),
		"device/xboxone/retained_usb_adapter.go") {
		t.Fatalf("exported dormant adapter has alias references: %v",
			publicReferences)
	}
	wantPrivate := map[string]int{
		"device/xboxone/retained_usb_adapter.go":                1,
		"device/xboxone/authorized_retained_usb_composition.go": 1,
	}
	for _, path := range privateCallSites {
		matched := false
		for suffix := range wantPrivate {
			if strings.HasSuffix(filepath.ToSlash(path), suffix) {
				wantPrivate[suffix]--
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("private canonical adapter has foreign call site: %s", path)
		}
	}
	for suffix, remaining := range wantPrivate {
		if remaining != 0 {
			t.Fatalf("private canonical adapter call count for %s = %d",
				suffix, 1-remaining)
		}
	}
	// Declaration plus the exported wrapper and the dormant composition call
	// are the only private identifier references. An alias assignment would add
	// another occurrence even if no CallExpr names the constructor directly.
	if len(privateReferences) != 3 {
		t.Fatalf("private canonical adapter has alias references: %v",
			privateReferences)
	}
}

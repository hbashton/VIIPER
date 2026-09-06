package usb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/retainedusb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

type retainedOwnerPlan struct {
	result retainedusb.Result
	data   []byte

	actualLength uint32
	retryAt      time.Time
	epoch        uint64
	currentEpoch bool

	err              error
	signalOnComplete bool
	prepareEntered   chan struct{}
	prepareRelease   <-chan struct{}
	completeEntered  chan struct{}
	completeRelease  <-chan struct{}
}

type retainedOwnerRequest struct {
	request retainedusb.Request
	data    []byte
}

type retainedOwnerCompletion struct {
	ticket    retainedusb.Ticket
	delivered bool
}

type retainedOwnerRetirement struct {
	ticket retainedusb.Ticket
	reason retainedusb.RetireReason
}

type scriptedRetainedOwner struct {
	mu sync.Mutex

	identity uint64
	limits   retainedusb.Limits
	epoch    uint64
	ready    chan struct{}

	nextToken uint64
	requests  []retainedOwnerRequest
	plans     [3][]retainedOwnerPlan
	planIndex [3]int

	completions []retainedOwnerCompletion
	retirements []retainedOwnerRetirement

	stageErr    error
	completeErr error
	retireErr   error
	panicAt     string
	panicValue  any

	overrideOwner   uint64
	overrideSession uint64
	overrideLane    retainedusb.Lane
	overrideToken   uint64

	stageCalls   atomic.Uint32
	prepareCalls atomic.Uint32
}

func retainedTestLimits(depth uint8) retainedusb.Limits {
	return retainedusb.Limits{
		QueueDepth:             [3]uint8{depth, depth, depth},
		BufferedRequestBytes:   uint32(depth) * 16,
		MaximumControlOut:      8,
		MaximumControlResponse: 64,
		MaximumInterruptIn:     64,
		MaximumInterruptOut:    8,
		InterruptInRoute: retainedusb.Route{
			InterfaceNumber: 0, AlternateSetting: 0, EndpointAddress: 0x81,
		},
		InterruptOutRoute: retainedusb.Route{
			InterfaceNumber: 0, AlternateSetting: 0, EndpointAddress: 0x01,
		},
	}
}

func newScriptedRetainedOwner(depth uint8) *scriptedRetainedOwner {
	return &scriptedRetainedOwner{
		identity: 0x72555342,
		limits:   retainedTestLimits(depth),
		ready:    make(chan struct{}, 1),
	}
}

func TestRetainedSchedulerConstructionSnapshotAvoidsLivePolicyCallbacks(t *testing.T) {
	owner := newScriptedRetainedOwner(1)
	owner.panicAt = "identity"
	snapshot := &retainedSchedulerConstructionSnapshot{
		ownerIdentity: owner.identity,
		limits:        owner.limits, readinessEpoch: owner.epoch,
		readiness: owner.ready,
	}
	scheduler, err := buildRetainedSubmissionSchedulerWithSnapshot(
		context.Background(), 1, owner,
		newResponseWriter(io.Discard, nil), false, snapshot)
	require.NoError(t, err)
	require.Equal(t, owner.identity, scheduler.ownerIdentity)
	require.Equal(t, owner.limits, scheduler.limits)
	require.NoError(t, scheduler.close())
}

func (owner *scriptedRetainedOwner) Identity() uint64 {
	owner.panicIf("identity")
	return owner.identity
}

func (owner *scriptedRetainedOwner) Limits() retainedusb.Limits {
	owner.panicIf("limits")
	return owner.limits
}

func (owner *scriptedRetainedOwner) Stage(
	request retainedusb.Request,
) (retainedusb.Ticket, error) {
	owner.panicIf("stage")
	owner.stageCalls.Add(1)
	owner.mu.Lock()
	defer owner.mu.Unlock()
	requestCopy := request
	requestCopy.Data = nil
	owner.requests = append(owner.requests, retainedOwnerRequest{
		request: requestCopy,
		data:    append([]byte(nil), request.Data...),
	})
	if owner.stageErr != nil {
		return retainedusb.Ticket{}, owner.stageErr
	}
	owner.nextToken++
	ticket := retainedusb.Ticket{
		OwnerID: owner.identity, Token: owner.nextToken, Generation: 1,
		SessionGeneration: request.SessionGeneration, Lane: request.Lane,
	}
	if owner.overrideOwner != 0 {
		ticket.OwnerID = owner.overrideOwner
	}
	if owner.overrideSession != 0 {
		ticket.SessionGeneration = owner.overrideSession
	}
	if owner.overrideLane != 0 {
		ticket.Lane = owner.overrideLane
	}
	if owner.overrideToken != 0 {
		ticket.Token = owner.overrideToken
	}
	return ticket, nil
}

func (owner *scriptedRetainedOwner) Prepare(
	ticket retainedusb.Ticket,
	destination []byte,
	_ time.Time,
) (retainedusb.Preparation, error) {
	owner.panicIf("prepare")
	owner.prepareCalls.Add(1)
	index, _ := ticket.Lane.Index()
	owner.mu.Lock()
	var plan retainedOwnerPlan
	if owner.planIndex[index] < len(owner.plans[index]) {
		plan = owner.plans[index][owner.planIndex[index]]
		owner.planIndex[index]++
	} else {
		plan = retainedOwnerPlan{
			result: retainedusb.ResultPending, currentEpoch: true,
		}
	}
	if plan.currentEpoch {
		plan.epoch = owner.epoch
	}
	owner.mu.Unlock()
	if plan.prepareEntered != nil {
		close(plan.prepareEntered)
	}
	if plan.prepareRelease != nil {
		<-plan.prepareRelease
	}
	if plan.err != nil {
		return retainedusb.Preparation{}, plan.err
	}
	copy(destination, plan.data)
	actualLength := plan.actualLength
	if plan.result == retainedusb.ResultData && actualLength == 0 {
		actualLength = uint32(len(plan.data))
	}
	return retainedusb.Preparation{
		Result: plan.result, ActualLength: actualLength,
		RetryAt: plan.retryAt, ReadinessEpoch: plan.epoch,
	}, nil
}

func (owner *scriptedRetainedOwner) Complete(
	ticket retainedusb.Ticket,
	delivered bool,
	_ time.Time,
) error {
	owner.panicIf("complete")
	owner.mu.Lock()
	owner.completions = append(owner.completions, retainedOwnerCompletion{
		ticket: ticket, delivered: delivered,
	})
	index, _ := ticket.Lane.Index()
	planIndex := owner.planIndex[index] - 1
	signal := planIndex >= 0 && planIndex < len(owner.plans[index]) &&
		owner.plans[index][planIndex].signalOnComplete
	var completeEntered chan struct{}
	var completeRelease <-chan struct{}
	if planIndex >= 0 && planIndex < len(owner.plans[index]) {
		completeEntered = owner.plans[index][planIndex].completeEntered
		completeRelease = owner.plans[index][planIndex].completeRelease
	}
	err := owner.completeErr
	owner.mu.Unlock()
	if completeEntered != nil {
		close(completeEntered)
	}
	if completeRelease != nil {
		<-completeRelease
	}
	if signal {
		owner.advanceReadiness()
	}
	return err
}

func (owner *scriptedRetainedOwner) Retire(
	ticket retainedusb.Ticket,
	reason retainedusb.RetireReason,
	_ time.Time,
) error {
	owner.panicIf("retire")
	owner.mu.Lock()
	owner.retirements = append(owner.retirements, retainedOwnerRetirement{
		ticket: ticket, reason: reason,
	})
	err := owner.retireErr
	owner.mu.Unlock()
	return err
}

func (owner *scriptedRetainedOwner) Readiness() (
	uint64,
	<-chan struct{},
) {
	owner.panicIf("readiness")
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return owner.epoch, owner.ready
}

func (owner *scriptedRetainedOwner) panicIf(stage string) {
	if owner.panicAt != stage {
		return
	}
	if owner.panicValue != nil {
		panic(owner.panicValue)
	}
	panic(stage + " retained callback panic")
}

func (owner *scriptedRetainedOwner) advanceReadiness() {
	owner.mu.Lock()
	owner.epoch++
	owner.mu.Unlock()
	select {
	case owner.ready <- struct{}{}:
	default:
	}
}

func (owner *scriptedRetainedOwner) appendPlan(
	lane retainedusb.Lane,
	plan retainedOwnerPlan,
) {
	index, _ := lane.Index()
	owner.plans[index] = append(owner.plans[index], plan)
}

func TestRetainedLifecycleResponseVisibilityBarrierRotatesBeforeNewEnqueue(
	t *testing.T,
) {
	owner := newScriptedRetainedOwner(2)
	completeEntered := make(chan struct{})
	completeRelease := make(chan struct{})
	owner.appendPlan(retainedusb.LaneControl, retainedOwnerPlan{
		result:          retainedusb.ResultSuccess,
		completeEntered: completeEntered,
		completeRelease: completeRelease,
	})
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})
	scheduler, err := newRetainedSubmissionScheduler(
		context.Background(), 11, owner,
		newResponseWriter(serverConn, nil), false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = scheduler.close() })
	published := make(chan controlLifecycleSetup, 2)
	require.NoError(t, scheduler.configureControlLifecycle(11,
		func(lifecycle controlLifecycleSetup) { published <- lifecycle }))

	lifecycle := retainedSubmissionEnvelope{
		lane: retainedusb.LaneControl, direction: retainedusb.DirectionOut,
		sequence: 401, bindingGeneration: 1,
		setup: [8]byte{usbReqTypeStandardToDevice,
			usbReqSetConfiguration, 1, 0, 0, 0, 0, 0},
		controlLifecycle: controlLifecycleSetup{
			kind: controlLifecycleSetConfiguration, recognized: true,
			accepted: true, configurationValue: 1,
		},
	}
	require.NoError(t, scheduler.enqueue(lifecycle))
	serviceDone := make(chan error, 1)
	go func() {
		_, _, serviceErr := scheduler.serviceRound(time.Now())
		serviceDone <- serviceErr
	}()
	var response [retSubmitHeaderSize]byte
	require.NoError(t, usbip.ReadExactly(clientConn, response[:]))
	select {
	case <-completeEntered:
	case <-time.After(time.Second):
		t.Fatal("lifecycle completion did not enter")
	}

	postReply := retainedInterruptInEnvelope(402)
	require.ErrorIs(t, scheduler.enqueue(postReply),
		errRetainedSubmissionLifecycleCompletionPending)
	retryDone := make(chan error, 1)
	go func() { retryDone <- scheduler.enqueueAfterVisibleCompletion(postReply) }()
	select {
	case retryErr := <-retryDone:
		t.Fatalf("post-reply enqueue escaped before lifecycle publication: %v", retryErr)
	case <-time.After(20 * time.Millisecond):
	}
	close(completeRelease)
	require.NoError(t, <-serviceDone)
	select {
	case lifecycle := <-published:
		require.Equal(t, uint8(1), lifecycle.configurationValue)
	case <-time.After(time.Second):
		t.Fatal("lifecycle route publication did not complete")
	}
	require.NoError(t, <-retryDone)

	scheduler.mu.Lock()
	_, slot, found := scheduler.findSequenceLocked(402)
	require.True(t, found)
	require.Equal(t, uint64(12), slot.bindingGeneration)
	scheduler.mu.Unlock()

	removed, unlinkErr := scheduler.unlink(402, time.Now())
	require.NoError(t, unlinkErr)
	require.True(t, removed)
	unconfigureEntered := make(chan struct{})
	unconfigureRelease := make(chan struct{})
	owner.appendPlan(retainedusb.LaneControl, retainedOwnerPlan{
		result:          retainedusb.ResultSuccess,
		completeEntered: unconfigureEntered,
		completeRelease: unconfigureRelease,
	})
	unconfigure := retainedSubmissionEnvelope{
		lane: retainedusb.LaneControl, direction: retainedusb.DirectionOut,
		sequence: 403, bindingGeneration: 1,
		setup: [8]byte{usbReqTypeStandardToDevice,
			usbReqSetConfiguration, 0, 0, 0, 0, 0, 0},
		controlLifecycle: controlLifecycleSetup{
			kind: controlLifecycleSetConfiguration, recognized: true,
			accepted: true, configurationValue: 0,
		},
	}
	require.NoError(t, scheduler.enqueue(unconfigure))
	serviceDone = make(chan error, 1)
	go func() {
		_, _, serviceErr := scheduler.serviceRound(time.Now())
		serviceDone <- serviceErr
	}()
	require.NoError(t, usbip.ReadExactly(clientConn, response[:]))
	select {
	case <-unconfigureEntered:
	case <-time.After(time.Second):
		t.Fatal("unconfigure completion did not enter")
	}
	postUnconfigure := retainedInterruptInEnvelope(404)
	require.ErrorIs(t, scheduler.enqueue(postUnconfigure),
		errRetainedSubmissionLifecycleCompletionPending)
	retryDone = make(chan error, 1)
	go func() {
		retryDone <- scheduler.enqueueAfterVisibleCompletion(postUnconfigure)
	}()
	close(unconfigureRelease)
	require.NoError(t, <-serviceDone)
	select {
	case lifecycle := <-published:
		require.Zero(t, lifecycle.configurationValue)
	case <-time.After(time.Second):
		t.Fatal("unconfigure route publication did not complete")
	}
	require.ErrorIs(t, <-retryDone, errRetainedSubmissionInactiveRoute)
}

func TestRetainedSetConfigurationAlwaysResetsAlternateRouting(t *testing.T) {
	scheduler := &retainedSubmissionScheduler{
		activeConfiguration:    1,
		activeAlternateSetting: 7,
	}
	scheduler.applyControlLifecycleRoutingLocked(controlLifecycleSetup{
		kind: controlLifecycleSetConfiguration, accepted: true,
		configurationValue: 1,
	})
	require.Equal(t, uint8(1), scheduler.activeConfiguration)
	require.Zero(t, scheduler.activeAlternateSetting)
}

func newManualRetainedScheduler(
	t *testing.T,
	owner *scriptedRetainedOwner,
	destination io.Writer,
) (*retainedSubmissionScheduler, *recordingWriter) {
	t.Helper()
	var recorder *recordingWriter
	if destination == nil {
		recorder = newRecordingWriter()
		destination = recorder
	}
	scheduler, err := newRetainedSubmissionScheduler(
		context.Background(), 17, owner,
		newResponseWriter(destination, nil), false,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = scheduler.close() })
	return scheduler, recorder
}

func retainedInterruptInEnvelope(sequence uint32) retainedSubmissionEnvelope {
	return retainedSubmissionEnvelope{
		lane: retainedusb.LaneInterruptIn, direction: retainedusb.DirectionIn,
		sequence: sequence, transferLength: 64,
		route:             retainedTestLimits(1).InterruptInRoute,
		bindingGeneration: 3,
	}
}

func retainedInterruptOutEnvelope(
	sequence uint32,
	data []byte,
) retainedSubmissionEnvelope {
	return retainedSubmissionEnvelope{
		lane: retainedusb.LaneInterruptOut, direction: retainedusb.DirectionOut,
		sequence: sequence, transferLength: uint32(len(data)),
		route:             retainedTestLimits(1).InterruptOutRoute,
		bindingGeneration: 3, data: data,
	}
}

func retainedControlInEnvelope(
	sequence uint32,
	wLength uint16,
) retainedSubmissionEnvelope {
	var setup [8]byte
	setup[0] = 0x80
	binary.LittleEndian.PutUint16(setup[6:8], wLength)
	return retainedSubmissionEnvelope{
		lane: retainedusb.LaneControl, direction: retainedusb.DirectionIn,
		sequence: sequence, transferLength: uint32(wLength), setup: setup,
		bindingGeneration: 2,
	}
}

func retainedControlOutEnvelope(
	sequence uint32,
	data []byte,
) retainedSubmissionEnvelope {
	var setup [8]byte
	binary.LittleEndian.PutUint16(setup[6:8], uint16(len(data)))
	return retainedSubmissionEnvelope{
		lane: retainedusb.LaneControl, direction: retainedusb.DirectionOut,
		sequence: sequence, transferLength: uint32(len(data)), setup: setup,
		bindingGeneration: 2, data: data,
	}
}

func newLiveRetainedImportScheduler(
	t *testing.T,
	identity uint64,
	deviceID uint64,
	owner *scriptedImportSessionOwner,
) (*retainedImportSession, retainedusb.ImportLease,
	*retainedSubmissionScheduler) {
	t.Helper()
	authority, err := newRetainedImportAuthority(identity, 0, 0)
	require.NoError(t, err)
	reservation, lease, err := authority.reserve(
		deviceID, owner, time.Now().Add(time.Second))
	require.NoError(t, err)
	scheduler, err := newParkedRetainedImportScheduler(
		context.Background(), reservation, owner,
		newResponseWriter(newRecordingWriter(), nil))
	require.NoError(t, err)
	session, err := authority.commit(
		reservation, &scriptedImportIngress{}, scheduler,
		time.Now().Add(time.Second), time.Now().Add(2*time.Second),
		time.Now().Add(3*time.Second))
	require.NoError(t, err)
	t.Cleanup(func() { _ = scheduler.close() })
	return session, lease, scheduler
}

func TestRetainedSubmissionConstructorRejectsInvalidAuthorityAndLimits(
	t *testing.T,
) {
	owner := newScriptedRetainedOwner(1)
	responses := newResponseWriter(io.Discard, nil)
	_, err := newRetainedSubmissionScheduler(
		context.Background(), 0, owner, responses, false)
	require.ErrorIs(t, err, errRetainedSubmissionUninitialized)

	owner.identity = 0
	_, err = newRetainedSubmissionScheduler(
		context.Background(), 1, owner, responses, false)
	require.Error(t, err)

	owner = newScriptedRetainedOwner(1)
	owner.limits.QueueDepth[0] = 0
	_, err = newRetainedSubmissionScheduler(
		context.Background(), 1, owner, responses, false)
	require.ErrorIs(t, err, errRetainedSubmissionInvalidLimits)

	owner = newScriptedRetainedOwner(1)
	owner.ready = nil
	_, err = newRetainedSubmissionScheduler(
		context.Background(), 1, owner, responses, false)
	require.ErrorIs(t, err, errRetainedSubmissionReadiness)

	owner = newScriptedRetainedOwner(1)
	owner.ready = make(chan struct{})
	_, err = newRetainedSubmissionScheduler(
		context.Background(), 1, owner, responses, false)
	require.ErrorIs(t, err, errRetainedSubmissionReadiness)

	owner = newScriptedRetainedOwner(1)
	close(owner.ready)
	_, err = newRetainedSubmissionScheduler(
		context.Background(), 1, owner, responses, false)
	require.ErrorIs(t, err, errRetainedSubmissionReadiness)

	owner = newScriptedRetainedOwner(1)
	owner.epoch = ^uint64(0)
	_, err = newRetainedSubmissionScheduler(
		context.Background(), 1, owner, responses, false)
	require.ErrorIs(t, err, errRetainedSubmissionReadiness)
}

func TestRetainedSubmissionRejectsControlDirectionMismatchBeforeStage(
	t *testing.T,
) {
	owner := newScriptedRetainedOwner(1)
	scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)

	deviceToHost := retainedControlInEnvelope(90, 1)
	deviceToHost.setup[0] &^= 0x80
	require.ErrorIs(t, scheduler.enqueue(deviceToHost),
		errRetainedSubmissionInvalidRequest)

	hostToDevice := retainedControlOutEnvelope(91, []byte{1})
	hostToDevice.setup[0] |= 0x80
	require.ErrorIs(t, scheduler.enqueue(hostToDevice),
		errRetainedSubmissionInvalidRequest)

	setupShorterThanData := retainedControlOutEnvelope(92, []byte{1, 2})
	binary.LittleEndian.PutUint16(setupShorterThanData.setup[6:8], 1)
	require.ErrorIs(t, scheduler.enqueue(setupShorterThanData),
		errRetainedSubmissionInvalidRequest)

	setupLongerThanData := retainedControlOutEnvelope(93, []byte{1})
	binary.LittleEndian.PutUint16(setupLongerThanData.setup[6:8], 2)
	require.ErrorIs(t, scheduler.enqueue(setupLongerThanData),
		errRetainedSubmissionInvalidRequest)
	require.Zero(t, owner.stageCalls.Load())
}

func TestRetainedSubmissionReadinessExhaustionFailsClosed(t *testing.T) {
	owner := newScriptedRetainedOwner(1)
	scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
	owner.mu.Lock()
	owner.epoch = ^uint64(0)
	owner.mu.Unlock()

	progressed, nextRetry, err := scheduler.serviceRound(time.Unix(91, 0))
	require.ErrorIs(t, err, errRetainedSubmissionReadiness)
	require.False(t, progressed)
	require.True(t, nextRetry.IsZero())
	require.Zero(t, owner.stageCalls.Load())
}

func TestRetainedSubmissionReadinessChannelMustRemainStable(t *testing.T) {
	owner := newScriptedRetainedOwner(1)
	scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
	owner.mu.Lock()
	owner.ready = make(chan struct{}, 1)
	owner.mu.Unlock()

	progressed, nextRetry, err := scheduler.serviceRound(time.Unix(92, 0))
	require.ErrorIs(t, err, errRetainedSubmissionReadiness)
	require.False(t, progressed)
	require.True(t, nextRetry.IsZero())
	require.Zero(t, owner.stageCalls.Load())
}

func TestRetainedSubmissionClosedReadinessFailsBeforeStage(t *testing.T) {
	owner := newScriptedRetainedOwner(1)
	scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
	close(owner.ready)

	progressed, nextRetry, err := scheduler.serviceRound(time.Unix(92, 1))
	require.ErrorIs(t, err, errRetainedSubmissionReadiness)
	require.False(t, progressed)
	require.True(t, nextRetry.IsZero())
	require.Zero(t, owner.stageCalls.Load())
}

func TestRetainedSubmissionParentCancellationFencesEveryAdmissionPhase(
	t *testing.T,
) {
	t.Run("enqueue", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		owner := newScriptedRetainedOwner(1)
		scheduler, err := newRetainedSubmissionScheduler(
			ctx, 17, owner, newResponseWriter(io.Discard, nil), false)
		require.NoError(t, err)
		t.Cleanup(func() { _ = scheduler.close() })
		cancel()
		require.ErrorIs(t,
			scheduler.enqueue(retainedInterruptInEnvelope(97)),
			errRetainedSubmissionClosed)
		require.Zero(t, owner.stageCalls.Load())
	})

	t.Run("before stage", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		owner := newScriptedRetainedOwner(1)
		recorder := newRecordingWriter()
		scheduler, err := newRetainedSubmissionScheduler(
			ctx, 17, owner, newResponseWriter(recorder, nil), false)
		require.NoError(t, err)
		t.Cleanup(func() { _ = scheduler.close() })
		require.NoError(t,
			scheduler.enqueue(retainedInterruptInEnvelope(98)))
		cancel()
		_, _, err = scheduler.serviceRound(time.Unix(98, 0))
		require.ErrorIs(t, err, errRetainedSubmissionClosed)
		require.Zero(t, owner.stageCalls.Load())
		recorder.mu.Lock()
		require.Empty(t, recorder.writes)
		recorder.mu.Unlock()
	})

	t.Run("behind response serialization", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		owner := newScriptedRetainedOwner(1)
		owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
			result: retainedusb.ResultData, data: []byte{1},
		})
		recorder := newRecordingWriter()
		responses := newResponseWriter(recorder, nil)
		responses.mu.Lock()
		locked := true
		defer func() {
			if locked {
				responses.mu.Unlock()
			}
		}()
		scheduler, err := newRetainedSubmissionScheduler(
			ctx, 17, owner, responses, true)
		require.NoError(t, err)
		t.Cleanup(func() { _ = scheduler.close() })
		require.NoError(t,
			scheduler.enqueue(retainedInterruptInEnvelope(99)))
		require.Eventually(t, func() bool {
			return owner.stageCalls.Load() == 1
		}, time.Second, time.Millisecond)
		cancel()
		responses.mu.Unlock()
		locked = false
		select {
		case <-scheduler.done:
		case <-time.After(time.Second):
			t.Fatal("retained worker did not stop after parent cancellation")
		}
		require.Zero(t, owner.prepareCalls.Load())
		require.Empty(t, owner.completions)
		require.Len(t, owner.retirements, 1)
		require.Equal(t, retainedusb.RetireConnectionClose,
			owner.retirements[0].reason)
		recorder.mu.Lock()
		require.Empty(t, recorder.writes)
		recorder.mu.Unlock()
	})

	t.Run("during prepare", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		owner := newScriptedRetainedOwner(1)
		prepareEntered := make(chan struct{})
		prepareRelease := make(chan struct{})
		owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
			result: retainedusb.ResultData, data: []byte{1},
			prepareEntered: prepareEntered, prepareRelease: prepareRelease,
		})
		recorder := newRecordingWriter()
		scheduler, err := newRetainedSubmissionScheduler(
			ctx, 17, owner, newResponseWriter(recorder, nil), true)
		require.NoError(t, err)
		t.Cleanup(func() { _ = scheduler.close() })
		require.NoError(t,
			scheduler.enqueue(retainedInterruptInEnvelope(100)))
		select {
		case <-prepareEntered:
		case <-time.After(time.Second):
			t.Fatal("retained worker did not enter Prepare")
		}
		cancel()
		close(prepareRelease)
		select {
		case <-scheduler.done:
		case <-time.After(time.Second):
			t.Fatal("retained worker did not stop after Prepare cancellation")
		}
		require.Equal(t, uint32(1), owner.prepareCalls.Load())
		require.Empty(t, owner.completions)
		require.Len(t, owner.retirements, 1)
		require.Equal(t, retainedusb.RetireConnectionClose,
			owner.retirements[0].reason)
		recorder.mu.Lock()
		require.Empty(t, recorder.writes)
		recorder.mu.Unlock()
	})
}

func TestRetainedSubmissionContainsOwnerCallbackPanics(t *testing.T) {
	panicErr := errors.New("scripted retained owner panic")
	responses := newResponseWriter(io.Discard, nil)
	for _, stage := range []string{"identity", "limits", "readiness"} {
		t.Run("constructor "+stage, func(t *testing.T) {
			owner := newScriptedRetainedOwner(1)
			owner.panicAt = stage
			owner.panicValue = panicErr
			_, err := newRetainedSubmissionScheduler(
				context.Background(), 1, owner, responses, false)
			require.ErrorIs(t, err, panicErr)
		})
	}

	t.Run("runtime readiness", func(t *testing.T) {
		owner := newScriptedRetainedOwner(1)
		scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
		owner.panicAt = "readiness"
		owner.panicValue = panicErr
		_, _, err := scheduler.serviceRound(time.Unix(93, 0))
		require.ErrorIs(t, err, panicErr)
	})

	t.Run("stage", func(t *testing.T) {
		owner := newScriptedRetainedOwner(1)
		scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
		owner.panicAt = "stage"
		owner.panicValue = panicErr
		require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(93)))
		_, _, err := scheduler.serviceRound(time.Unix(93, 0))
		require.ErrorIs(t, err, panicErr)
		require.Empty(t, owner.retirements)
	})

	t.Run("prepare", func(t *testing.T) {
		owner := newScriptedRetainedOwner(1)
		scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
		owner.panicAt = "prepare"
		owner.panicValue = panicErr
		require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(94)))
		_, _, err := scheduler.serviceRound(time.Unix(94, 0))
		require.ErrorIs(t, err, panicErr)
		require.Len(t, owner.retirements, 1)
	})

	t.Run("complete", func(t *testing.T) {
		owner := newScriptedRetainedOwner(1)
		owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
			result: retainedusb.ResultData, data: []byte{1},
		})
		scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
		owner.panicAt = "complete"
		owner.panicValue = panicErr
		require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(95)))
		progressed, _, err := scheduler.serviceRound(time.Unix(95, 0))
		require.ErrorIs(t, err, panicErr)
		require.True(t, progressed)
		require.Empty(t, owner.retirements)
	})

	t.Run("retire", func(t *testing.T) {
		owner := newScriptedRetainedOwner(1)
		owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
			result: retainedusb.ResultPending, currentEpoch: true,
		})
		scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
		require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(96)))
		_, _, err := scheduler.serviceRound(time.Unix(96, 0))
		require.NoError(t, err)
		owner.panicAt = "retire"
		owner.panicValue = panicErr
		removed, err := scheduler.unlink(96, time.Unix(96, 1))
		require.ErrorIs(t, err, panicErr)
		require.True(t, removed)
	})
}

func TestRetainedSubmissionDeepCopiesAndScrubsFixedRequestStorage(t *testing.T) {
	owner := newScriptedRetainedOwner(2)
	owner.appendPlan(retainedusb.LaneInterruptOut, retainedOwnerPlan{
		result: retainedusb.ResultSuccess, actualLength: 4,
	})
	scheduler, recorder := newManualRetainedScheduler(t, owner, nil)
	original := []byte{1, 2, 3, 4}
	require.NoError(t, scheduler.enqueue(
		retainedInterruptOutEnvelope(100, original)))
	clear(original)

	progressed, _, err := scheduler.serviceRound(time.Unix(0, 100))
	require.NoError(t, err)
	require.True(t, progressed)
	require.Equal(t, []byte{1, 2, 3, 4}, owner.requests[0].data)
	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, uint32(100), binary.BigEndian.Uint32(writes[0].packet[4:8]))
	require.Equal(t, uint32(4), binary.BigEndian.Uint32(writes[0].packet[24:28]))

	outIndex, _ := retainedusb.LaneInterruptOut.Index()
	scheduler.mu.Lock()
	lane := &scheduler.lanes[outIndex]
	require.Zero(t, lane.orderCount)
	require.Equal(t, len(lane.slots), lane.freeCount)
	require.Equal(t, make([]byte, len(lane.payloadSlab)), lane.payloadSlab)
	for index := range lane.slots {
		slot := &lane.slots[index]
		require.Equal(t, retainedSubmissionFree, slot.state)
		require.Zero(t, slot.sequence)
		require.Zero(t, slot.ingressOrdinal)
		require.Empty(t, slot.payload)
	}
	scheduler.mu.Unlock()
}

func TestRetainedSubmissionRejectsOverflowDuplicateAndExhaustion(t *testing.T) {
	owner := newScriptedRetainedOwner(1)
	scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(200)))
	require.ErrorIs(t,
		scheduler.enqueue(retainedInterruptInEnvelope(201)),
		errRetainedSubmissionQueueFull)
	require.ErrorIs(t,
		scheduler.enqueue(retainedControlInEnvelope(200, 8)),
		errRetainedSubmissionDuplicateSequence)

	scheduler.mu.Lock()
	scheduler.nextOrdinal = ^uint64(0)
	scheduler.mu.Unlock()
	require.ErrorIs(t,
		scheduler.enqueue(retainedInterruptOutEnvelope(202, []byte{1})),
		errRetainedSubmissionCounterExhausted)
	require.Equal(t, uint32(0), owner.stageCalls.Load())
}

func TestRetainedSubmissionFencesCrossOwnerSessionAndDuplicateTickets(
	t *testing.T,
) {
	tests := []struct {
		name   string
		mutate func(*scriptedRetainedOwner)
	}{
		{"cross owner", func(owner *scriptedRetainedOwner) {
			owner.overrideOwner = owner.identity + 1
		}},
		{"cross session", func(owner *scriptedRetainedOwner) {
			owner.overrideSession = 18
		}},
		{"cross lane", func(owner *scriptedRetainedOwner) {
			owner.overrideLane = retainedusb.LaneControl
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			owner := newScriptedRetainedOwner(1)
			test.mutate(owner)
			scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
			require.NoError(t,
				scheduler.enqueue(retainedInterruptInEnvelope(210)))
			_, _, err := scheduler.serviceRound(time.Unix(0, 210))
			require.ErrorIs(t, err, errRetainedSubmissionInvalidTicket)
			require.Empty(t, owner.retirements,
				"an unaccepted forged ticket reached per-ticket Retire")
		})
	}

	owner := newScriptedRetainedOwner(2)
	owner.overrideToken = 9
	owner.appendPlan(retainedusb.LaneControl, retainedOwnerPlan{
		result: retainedusb.ResultPending, currentEpoch: true,
	})
	owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultPending, currentEpoch: true,
	})
	scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
	require.NoError(t, scheduler.enqueue(retainedControlInEnvelope(220, 8)))
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(221)))
	_, _, err := scheduler.serviceRound(time.Unix(0, 220))
	require.ErrorIs(t, err, errRetainedSubmissionInvalidTicket)
	require.Empty(t, owner.retirements,
		"the alias retired the predecessor capability")
	require.NoError(t, scheduler.retireSession(
		17, retainedusb.RetireInvariantFailure, time.Unix(0, 221)))
	require.Len(t, owner.retirements, 1,
		"session cleanup did not retire the predecessor exactly once")
	require.Equal(t, retainedusb.LaneControl,
		owner.retirements[0].ticket.Lane)
}

func TestRetainedSubmissionPendingNeedsReadinessOrExactDeadline(t *testing.T) {
	owner := newScriptedRetainedOwner(2)
	base := time.Unix(100, 0)
	deadline := base.Add(10 * time.Millisecond)
	owner.appendPlan(retainedusb.LaneInterruptOut, retainedOwnerPlan{
		result: retainedusb.ResultPending, currentEpoch: true,
		retryAt: deadline,
	})
	owner.appendPlan(retainedusb.LaneInterruptOut, retainedOwnerPlan{
		result: retainedusb.ResultSuccess, actualLength: 1,
	})
	scheduler, recorder := newManualRetainedScheduler(t, owner, nil)
	require.NoError(t, scheduler.enqueue(
		retainedInterruptOutEnvelope(300, []byte{0x33})))

	progressed, next, err := scheduler.serviceRound(base)
	require.NoError(t, err)
	require.False(t, progressed)
	require.Equal(t, deadline, next)
	require.Equal(t, uint32(1), owner.prepareCalls.Load())

	progressed, next, err = scheduler.serviceRound(deadline.Add(-time.Nanosecond))
	require.NoError(t, err)
	require.False(t, progressed)
	require.Equal(t, deadline, next)
	require.Equal(t, uint32(1), owner.prepareCalls.Load())

	progressed, _, err = scheduler.serviceRound(deadline)
	require.NoError(t, err)
	require.True(t, progressed)
	require.Equal(t, uint32(2), owner.prepareCalls.Load())
	recorder.waitForWrites(t, 1)
}

func TestRetainedSubmissionReadinessUnblocksWithoutBusySpin(t *testing.T) {
	owner := newScriptedRetainedOwner(1)
	owner.appendPlan(retainedusb.LaneInterruptOut, retainedOwnerPlan{
		result: retainedusb.ResultPending, currentEpoch: true,
	})
	owner.appendPlan(retainedusb.LaneInterruptOut, retainedOwnerPlan{
		result: retainedusb.ResultSuccess, actualLength: 1,
	})
	scheduler, recorder := newManualRetainedScheduler(t, owner, nil)
	base := time.Unix(200, 0)
	require.NoError(t, scheduler.enqueue(
		retainedInterruptOutEnvelope(310, []byte{1})))

	progressed, next, err := scheduler.serviceRound(base)
	require.NoError(t, err)
	require.False(t, progressed)
	require.True(t, next.IsZero())
	progressed, _, err = scheduler.serviceRound(base.Add(time.Second))
	require.NoError(t, err)
	require.False(t, progressed)
	require.Equal(t, uint32(1), owner.prepareCalls.Load())

	owner.advanceReadiness()
	progressed, _, err = scheduler.serviceRound(base.Add(time.Second))
	require.NoError(t, err)
	require.True(t, progressed)
	require.Equal(t, uint32(2), owner.prepareCalls.Load())
	recorder.waitForWrites(t, 1)
}

func TestRetainedSubmissionRejectsInvalidPendingAndTerminalShapes(t *testing.T) {
	base := time.Unix(300, 0)
	tests := []struct {
		name string
		lane retainedusb.Lane
		plan retainedOwnerPlan
		env  retainedSubmissionEnvelope
	}{
		{
			name: "pending deadline is not future", lane: retainedusb.LaneInterruptIn,
			plan: retainedOwnerPlan{result: retainedusb.ResultPending,
				currentEpoch: true, retryAt: base},
			env: retainedInterruptInEnvelope(320),
		},
		{
			name: "pending epoch exhausted", lane: retainedusb.LaneInterruptIn,
			plan: retainedOwnerPlan{result: retainedusb.ResultPending,
				epoch: ^uint64(0)},
			env: retainedInterruptInEnvelope(321),
		},
		{
			name: "pending epoch is not published", lane: retainedusb.LaneInterruptIn,
			plan: retainedOwnerPlan{result: retainedusb.ResultPending,
				epoch: 1},
			env: retainedInterruptInEnvelope(325),
		},
		{
			name: "IN success", lane: retainedusb.LaneInterruptIn,
			plan: retainedOwnerPlan{result: retainedusb.ResultSuccess},
			env:  retainedInterruptInEnvelope(322),
		},
		{
			name: "IN zero-length data", lane: retainedusb.LaneInterruptIn,
			plan: retainedOwnerPlan{result: retainedusb.ResultData},
			env:  retainedInterruptInEnvelope(327),
		},
		{
			name: "control zero-length data", lane: retainedusb.LaneControl,
			plan: retainedOwnerPlan{result: retainedusb.ResultData},
			env:  retainedControlInEnvelope(328, 8),
		},
		{
			name: "IN stall", lane: retainedusb.LaneInterruptIn,
			plan: retainedOwnerPlan{result: retainedusb.ResultStall},
			env:  retainedInterruptInEnvelope(326),
		},
		{
			name: "OUT data", lane: retainedusb.LaneInterruptOut,
			plan: retainedOwnerPlan{result: retainedusb.ResultData,
				data: []byte{1}},
			env: retainedInterruptOutEnvelope(323, []byte{1}),
		},
		{
			name: "OUT short acknowledgement", lane: retainedusb.LaneInterruptOut,
			plan: retainedOwnerPlan{result: retainedusb.ResultSuccess},
			env:  retainedInterruptOutEnvelope(324, []byte{1}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			owner := newScriptedRetainedOwner(1)
			owner.appendPlan(test.lane, test.plan)
			scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
			require.NoError(t, scheduler.enqueue(test.env))
			_, _, err := scheduler.serviceRound(base)
			require.ErrorIs(t, err, errRetainedSubmissionInvalidPreparation)
			require.Len(t, owner.retirements, 1)
			require.Equal(t, retainedusb.RetirePrepareFailure,
				owner.retirements[0].reason)
		})
	}
}

func TestRetainedSubmissionCrossLaneArbiterProgressesPrerequisiteFirst(
	t *testing.T,
) {
	owner := newScriptedRetainedOwner(2)
	owner.appendPlan(retainedusb.LaneInterruptOut, retainedOwnerPlan{
		result: retainedusb.ResultPending, currentEpoch: true,
	})
	owner.appendPlan(retainedusb.LaneInterruptOut, retainedOwnerPlan{
		result: retainedusb.ResultSuccess, actualLength: 1,
	})
	owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultData, data: []byte{0xa1, 0xa2},
		signalOnComplete: true,
	})
	scheduler, recorder := newManualRetainedScheduler(t, owner, nil)
	require.NoError(t, scheduler.enqueue(
		retainedInterruptOutEnvelope(400, []byte{0x40})))
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(401)))

	progressed, _, err := scheduler.serviceRound(time.Unix(400, 0))
	require.NoError(t, err)
	require.True(t, progressed)
	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, uint32(401), binary.BigEndian.Uint32(writes[0].packet[4:8]),
		"pending older OUT prevented the prerequisite IN response")

	progressed, _, err = scheduler.serviceRound(time.Unix(400, 1))
	require.NoError(t, err)
	require.True(t, progressed)
	writes = recorder.waitForWrites(t, 2)
	require.Equal(t, uint32(400), binary.BigEndian.Uint32(writes[1].packet[4:8]))
}

func TestRetainedSubmissionPreservesSameLaneFIFO(t *testing.T) {
	owner := newScriptedRetainedOwner(2)
	owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultData, data: []byte{1},
	})
	owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultData, data: []byte{2},
	})
	scheduler, recorder := newManualRetainedScheduler(t, owner, nil)
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(410)))
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(411)))
	progressed, _, err := scheduler.serviceRound(time.Unix(410, 0))
	require.NoError(t, err)
	require.True(t, progressed)
	progressed, _, err = scheduler.serviceRound(time.Unix(410, 1))
	require.NoError(t, err)
	require.True(t, progressed)
	writes := recorder.waitForWrites(t, 2)
	require.Equal(t, uint32(410), binary.BigEndian.Uint32(writes[0].packet[4:8]))
	require.Equal(t, uint32(411), binary.BigEndian.Uint32(writes[1].packet[4:8]))
}

func TestRetainedSubmissionUsesExactLateResponseShapes(t *testing.T) {
	tests := []struct {
		name       string
		lane       retainedusb.Lane
		envelope   retainedSubmissionEnvelope
		plan       retainedOwnerPlan
		wantStatus int32
		wantActual uint32
		wantData   []byte
	}{
		{
			name: "control data capped by wLength", lane: retainedusb.LaneControl,
			envelope: retainedControlInEnvelope(500, 3),
			plan: retainedOwnerPlan{result: retainedusb.ResultData,
				data: []byte{1, 2, 3}},
			wantActual: 3, wantData: []byte{1, 2, 3},
		},
		{
			name: "control OUT acknowledgement", lane: retainedusb.LaneControl,
			envelope: retainedControlOutEnvelope(501, []byte{4, 5}),
			plan: retainedOwnerPlan{result: retainedusb.ResultSuccess,
				actualLength: 2},
			wantActual: 2,
		},
		{
			name: "control ZLP", lane: retainedusb.LaneControl,
			envelope: retainedControlInEnvelope(502, 0),
			plan:     retainedOwnerPlan{result: retainedusb.ResultSuccess},
		},
		{
			name: "interrupt IN data", lane: retainedusb.LaneInterruptIn,
			envelope: retainedInterruptInEnvelope(503),
			plan: retainedOwnerPlan{result: retainedusb.ResultData,
				data: []byte{6, 7}},
			wantActual: 2, wantData: []byte{6, 7},
		},
		{
			name: "interrupt OUT acknowledgement", lane: retainedusb.LaneInterruptOut,
			envelope: retainedInterruptOutEnvelope(504, []byte{8}),
			plan: retainedOwnerPlan{result: retainedusb.ResultSuccess,
				actualLength: 1},
			wantActual: 1,
		},
		{
			name: "stall", lane: retainedusb.LaneControl,
			envelope:   retainedControlInEnvelope(505, 8),
			plan:       retainedOwnerPlan{result: retainedusb.ResultStall},
			wantStatus: errPipe,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			owner := newScriptedRetainedOwner(1)
			owner.appendPlan(test.lane, test.plan)
			scheduler, recorder := newManualRetainedScheduler(t, owner, nil)
			require.NoError(t, scheduler.enqueue(test.envelope))
			progressed, _, err := scheduler.serviceRound(time.Unix(500, 0))
			require.NoError(t, err)
			require.True(t, progressed)
			writes := recorder.waitForWrites(t, 1)
			packet := writes[0].packet
			require.Equal(t, uint32(usbip.RetSubmitCode),
				binary.BigEndian.Uint32(packet[0:4]))
			require.Equal(t, test.wantStatus,
				int32(binary.BigEndian.Uint32(packet[20:24])))
			require.Equal(t, test.wantActual,
				binary.BigEndian.Uint32(packet[24:28]))
			require.Equal(t, len(test.wantData),
				len(packet[retSubmitHeaderSize:]))
			require.Equal(t, test.wantData,
				append([]byte(nil), packet[retSubmitHeaderSize:]...))
			require.Len(t, owner.completions, 1)
			require.True(t, owner.completions[0].delivered)
		})
	}
}

func TestRetainedSubmissionDeliveryAndCompletionFailuresAreTerminal(t *testing.T) {
	deliveryFailure := errors.New("retained delivery failed")
	owner := newScriptedRetainedOwner(1)
	owner.appendPlan(retainedusb.LaneInterruptOut, retainedOwnerPlan{
		result: retainedusb.ResultSuccess, actualLength: 1,
	})
	scheduler, _ := newManualRetainedScheduler(
		t, owner, lateFailWriter{err: deliveryFailure})
	require.NoError(t, scheduler.enqueue(
		retainedInterruptOutEnvelope(520, []byte{1})))
	progressed, _, err := scheduler.serviceRound(time.Unix(520, 0))
	require.ErrorIs(t, err, deliveryFailure)
	require.True(t, progressed)
	require.Len(t, owner.completions, 1)
	require.False(t, owner.completions[0].delivered)
	require.Empty(t, owner.retirements)

	completionFailure := errors.New("retained completion failed")
	owner = newScriptedRetainedOwner(1)
	owner.completeErr = completionFailure
	owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultData, data: []byte{1},
	})
	scheduler, _ = newManualRetainedScheduler(t, owner, io.Discard)
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(521)))
	progressed, _, err = scheduler.serviceRound(time.Unix(521, 0))
	require.ErrorIs(t, err, completionFailure)
	require.True(t, progressed)
	require.Len(t, owner.completions, 1)
	require.Empty(t, owner.retirements)
}

func TestRetainedSubmissionPrepareFailureRetiresWithoutCompletion(t *testing.T) {
	prepareFailure := errors.New("retained prepare failed")
	owner := newScriptedRetainedOwner(1)
	owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		err: prepareFailure,
	})
	scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(530)))
	_, _, err := scheduler.serviceRound(time.Unix(530, 0))
	require.ErrorIs(t, err, prepareFailure)
	require.Empty(t, owner.completions)
	require.Len(t, owner.retirements, 1)
	require.Equal(t, retainedusb.RetirePrepareFailure,
		owner.retirements[0].reason)
}

func TestRetainedSubmissionUnlinkRawPendingAndAdmitted(t *testing.T) {
	t.Run("raw", func(t *testing.T) {
		owner := newScriptedRetainedOwner(1)
		scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
		require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(600)))
		removed, err := scheduler.unlink(600, time.Unix(600, 0))
		require.NoError(t, err)
		require.True(t, removed)
		require.Empty(t, owner.retirements)
	})

	t.Run("pending", func(t *testing.T) {
		owner := newScriptedRetainedOwner(1)
		owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
			result: retainedusb.ResultPending, currentEpoch: true,
		})
		scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
		require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(601)))
		progressed, _, err := scheduler.serviceRound(time.Unix(601, 0))
		require.NoError(t, err)
		require.False(t, progressed)
		removed, err := scheduler.unlink(601, time.Unix(601, 1))
		require.NoError(t, err)
		require.True(t, removed)
		require.Len(t, owner.retirements, 1)
		require.Equal(t, retainedusb.RetireUnlink,
			owner.retirements[0].reason)
	})

	t.Run("failed pending retirement is terminal", func(t *testing.T) {
		retireFailure := errors.New("unlink ownership remained ambiguous")
		owner := newScriptedRetainedOwner(1)
		owner.retireErr = retireFailure
		owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
			result: retainedusb.ResultPending, currentEpoch: true,
		})
		scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
		require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(603)))
		progressed, _, err := scheduler.serviceRound(time.Unix(603, 0))
		require.NoError(t, err)
		require.False(t, progressed)
		removed, err := scheduler.unlink(603, time.Unix(603, 1))
		require.True(t, removed)
		require.ErrorIs(t, err, retireFailure)
		require.ErrorIs(t, scheduler.close(), retireFailure)
	})

	t.Run("admitted", func(t *testing.T) {
		owner := newScriptedRetainedOwner(1)
		owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
			result: retainedusb.ResultData, data: []byte{1},
		})
		destination := newLateResponseGateWriter()
		scheduler, _ := newManualRetainedScheduler(t, owner, destination)
		require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(602)))
		done := make(chan error, 1)
		go func() {
			_, _, err := scheduler.serviceRound(time.Unix(602, 0))
			done <- err
		}()
		select {
		case <-destination.entered:
		case <-time.After(time.Second):
			t.Fatal("retained response did not reach admitted write")
		}
		removed, err := scheduler.unlink(602, time.Unix(602, 1))
		require.NoError(t, err)
		require.False(t, removed)
		close(destination.release)
		require.NoError(t, <-done)
		require.Len(t, owner.completions, 1)
		require.Empty(t, owner.retirements)
	})
}

func TestRetainedSubmissionUnlinkBeforeLatePreparationWinsExactly(t *testing.T) {
	owner := newScriptedRetainedOwner(1)
	owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultData, data: []byte{1},
	})
	responses := newResponseWriter(io.Discard, nil)
	scheduler, err := newRetainedSubmissionScheduler(
		context.Background(), 17, owner, responses, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = scheduler.close() })
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(610)))

	responses.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, _, err := scheduler.serviceRound(time.Unix(610, 0))
		done <- err
	}()
	require.Eventually(t, func() bool {
		return owner.stageCalls.Load() == 1
	}, time.Second, time.Millisecond)
	removed, err := scheduler.unlink(610, time.Unix(610, 1))
	require.NoError(t, err)
	require.True(t, removed)
	responses.mu.Unlock()
	require.NoError(t, <-done)
	require.Zero(t, owner.prepareCalls.Load())
	require.Empty(t, owner.completions)
	require.Len(t, owner.retirements, 1)
}

func TestRetainedSubmissionScopedRetirementIsGenerationExact(t *testing.T) {
	owner := newScriptedRetainedOwner(2)
	for _, lane := range []retainedusb.Lane{
		retainedusb.LaneControl,
		retainedusb.LaneInterruptIn,
		retainedusb.LaneInterruptOut,
	} {
		owner.appendPlan(lane, retainedOwnerPlan{
			result: retainedusb.ResultPending, currentEpoch: true,
		})
	}
	scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
	require.NoError(t, scheduler.enqueue(retainedControlInEnvelope(700, 8)))
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(701)))
	require.NoError(t, scheduler.enqueue(
		retainedInterruptOutEnvelope(702, []byte{1})))
	progressed, _, err := scheduler.serviceRound(time.Unix(700, 0))
	require.NoError(t, err)
	require.False(t, progressed)
	require.Len(t, owner.requests, 3)

	err = scheduler.retireBinding(
		retainedusb.Route{}, 2,
		retainedusb.RetireEndpointReset, time.Unix(700, 1))
	require.ErrorIs(t, err, errRetainedSubmissionInvalidRequest)
	require.Empty(t, owner.retirements,
		"endpoint binding retirement reached endpoint zero")

	err = scheduler.retireBinding(
		owner.limits.InterruptInRoute, 99,
		retainedusb.RetireEndpointReset, time.Unix(700, 2))
	require.NoError(t, err)
	require.Empty(t, owner.retirements,
		"stale binding generation retired a ticket")
	err = scheduler.retireBinding(
		owner.limits.InterruptInRoute, 3,
		retainedusb.RetireEndpointReset, time.Unix(700, 3))
	require.NoError(t, err)
	require.Len(t, owner.retirements, 1)
	require.Equal(t, retainedusb.LaneInterruptIn,
		owner.retirements[0].ticket.Lane)

	require.NoError(t, scheduler.retireLane(
		retainedusb.LaneControl, retainedusb.RetireConfigurationChange,
		time.Unix(700, 4)))
	require.Len(t, owner.retirements, 2)
	require.NoError(t, scheduler.retireSession(
		17, retainedusb.RetireConnectionClose, time.Unix(700, 5)))
	require.Len(t, owner.retirements, 3)
	require.ErrorIs(t, scheduler.retireSession(
		18, retainedusb.RetireConnectionClose, time.Unix(700, 6)),
		errRetainedSubmissionInvalidRequest)
}

func TestRetainedSubmissionSessionRetirementCollectsAtMostOneTicketPerLane(
	t *testing.T,
) {
	owner := newScriptedRetainedOwner(2)
	for _, lane := range []retainedusb.Lane{
		retainedusb.LaneControl,
		retainedusb.LaneInterruptIn,
		retainedusb.LaneInterruptOut,
	} {
		owner.appendPlan(lane, retainedOwnerPlan{
			result: retainedusb.ResultPending, currentEpoch: true,
		})
	}
	scheduler, _ := newManualRetainedScheduler(t, owner, io.Discard)
	require.NoError(t, scheduler.enqueue(retainedControlInEnvelope(730, 8)))
	require.NoError(t, scheduler.enqueue(retainedControlInEnvelope(731, 8)))
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(732)))
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(733)))
	require.NoError(t, scheduler.enqueue(
		retainedInterruptOutEnvelope(734, []byte{1})))
	require.NoError(t, scheduler.enqueue(
		retainedInterruptOutEnvelope(735, []byte{2})))

	progressed, _, err := scheduler.serviceRound(time.Unix(730, 0))
	require.NoError(t, err)
	require.False(t, progressed)
	require.Len(t, owner.requests, 3,
		"a raw follower acquired a second same-lane ticket")

	require.NoError(t, scheduler.retireSession(
		17, retainedusb.RetireConnectionClose, time.Unix(730, 1)))
	require.Len(t, owner.retirements, 3)
	for laneIndex := range scheduler.lanes {
		lane := &scheduler.lanes[laneIndex]
		require.Zero(t, lane.orderCount)
		require.Equal(t, len(lane.slots), lane.freeCount)
		for _, value := range lane.payloadSlab {
			require.Zero(t, value)
		}
	}
}

type retainedCallbackLockProbe struct {
	base      *scriptedRetainedOwner
	scheduler *retainedSubmissionScheduler

	readinessOutside bool
	stageUnderLock   bool
	prepareUnderLock bool
	completeOutside  bool
	retireOutside    bool
}

func (probe *retainedCallbackLockProbe) schedulerLockHeld() bool {
	if probe.scheduler == nil {
		return false
	}
	if probe.scheduler.mu.TryLock() {
		probe.scheduler.mu.Unlock()
		return false
	}
	return true
}

func (probe *retainedCallbackLockProbe) Identity() uint64 {
	return probe.base.Identity()
}

func (probe *retainedCallbackLockProbe) Limits() retainedusb.Limits {
	return probe.base.Limits()
}

func (probe *retainedCallbackLockProbe) Stage(
	request retainedusb.Request,
) (retainedusb.Ticket, error) {
	probe.stageUnderLock = probe.schedulerLockHeld()
	return probe.base.Stage(request)
}

func (probe *retainedCallbackLockProbe) Prepare(
	ticket retainedusb.Ticket,
	destination []byte,
	now time.Time,
) (retainedusb.Preparation, error) {
	probe.prepareUnderLock = probe.schedulerLockHeld()
	return probe.base.Prepare(ticket, destination, now)
}

func (probe *retainedCallbackLockProbe) Complete(
	ticket retainedusb.Ticket,
	delivered bool,
	now time.Time,
) error {
	probe.completeOutside = !probe.schedulerLockHeld()
	return probe.base.Complete(ticket, delivered, now)
}

func (probe *retainedCallbackLockProbe) Retire(
	ticket retainedusb.Ticket,
	reason retainedusb.RetireReason,
	now time.Time,
) error {
	probe.retireOutside = !probe.schedulerLockHeld()
	return probe.base.Retire(ticket, reason, now)
}

func (probe *retainedCallbackLockProbe) Readiness() (
	uint64,
	<-chan struct{},
) {
	if probe.scheduler != nil {
		probe.readinessOutside = !probe.schedulerLockHeld()
	}
	return probe.base.Readiness()
}

func TestRetainedSubmissionOwnerCallbacksOutsideLockExceptStageAndPrepare(
	t *testing.T,
) {
	base := newScriptedRetainedOwner(1)
	base.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultData, data: []byte{1},
	})
	base.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultPending, currentEpoch: true,
	})
	owner := &retainedCallbackLockProbe{base: base}
	scheduler, err := newRetainedSubmissionScheduler(
		context.Background(), 17, owner,
		newResponseWriter(io.Discard, nil), false)
	require.NoError(t, err)
	owner.scheduler = scheduler
	t.Cleanup(func() { _ = scheduler.close() })

	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(710)))
	progressed, _, err := scheduler.serviceRound(time.Unix(710, 0))
	require.NoError(t, err)
	require.True(t, progressed)
	require.True(t, owner.readinessOutside)
	require.True(t, owner.stageUnderLock)
	require.True(t, owner.prepareUnderLock)
	require.True(t, owner.completeOutside)

	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(711)))
	progressed, _, err = scheduler.serviceRound(time.Unix(711, 0))
	require.NoError(t, err)
	require.False(t, progressed)
	require.NoError(t, scheduler.retireLane(
		retainedusb.LaneInterruptIn, retainedusb.RetireEndpointReset,
		time.Unix(711, 1)))
	require.True(t, owner.retireOutside)
}

type allocationRetainedOwner struct {
	identity uint64
	limits   retainedusb.Limits
	ready    chan struct{}
	token    uint64
}

func (owner *allocationRetainedOwner) Identity() uint64 { return owner.identity }
func (owner *allocationRetainedOwner) Limits() retainedusb.Limits {
	return owner.limits
}
func (owner *allocationRetainedOwner) Stage(
	request retainedusb.Request,
) (retainedusb.Ticket, error) {
	owner.token++
	return retainedusb.Ticket{
		OwnerID: owner.identity, Token: owner.token, Generation: 1,
		SessionGeneration: request.SessionGeneration, Lane: request.Lane,
	}, nil
}
func (owner *allocationRetainedOwner) Prepare(
	_ retainedusb.Ticket,
	destination []byte,
	_ time.Time,
) (retainedusb.Preparation, error) {
	destination[0] = 0x5a
	return retainedusb.Preparation{
		Result: retainedusb.ResultData, ActualLength: 1,
	}, nil
}
func (*allocationRetainedOwner) Complete(
	retainedusb.Ticket, bool, time.Time,
) error {
	return nil
}
func (*allocationRetainedOwner) Retire(
	retainedusb.Ticket, retainedusb.RetireReason, time.Time,
) error {
	return nil
}
func (owner *allocationRetainedOwner) Readiness() (
	uint64,
	<-chan struct{},
) {
	return 0, owner.ready
}

func TestRetainedSubmissionWarmedTerminalPathAllocatesZero(t *testing.T) {
	owner := &allocationRetainedOwner{
		identity: 99, limits: retainedTestLimits(1),
		ready: make(chan struct{}, 1),
	}
	scheduler, err := newRetainedSubmissionScheduler(
		context.Background(), 23, owner,
		newResponseWriter(io.Discard, nil), false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = scheduler.close() })
	now := time.Unix(800, 0)
	var sequence uint32
	allocations := testing.AllocsPerRun(1000, func() {
		sequence++
		if err := scheduler.enqueue(
			retainedInterruptInEnvelope(sequence)); err != nil {
			panic(err)
		}
		progressed, _, err := scheduler.serviceRound(now)
		if err != nil || !progressed {
			panic("retained allocation run did not complete")
		}
	})
	require.Zero(t, allocations)
}

func TestRetainedSubmissionStartedWorkerObservesReadinessBeforeSleep(t *testing.T) {
	owner := newScriptedRetainedOwner(1)
	prepareEntered := make(chan struct{})
	prepareRelease := make(chan struct{})
	owner.appendPlan(retainedusb.LaneInterruptOut, retainedOwnerPlan{
		result: retainedusb.ResultPending, currentEpoch: true,
		prepareEntered: prepareEntered, prepareRelease: prepareRelease,
	})
	owner.appendPlan(retainedusb.LaneInterruptOut, retainedOwnerPlan{
		result: retainedusb.ResultSuccess, actualLength: 1,
	})
	recorder := newRecordingWriter()
	scheduler, err := newRetainedSubmissionScheduler(
		context.Background(), 31, owner,
		newResponseWriter(recorder, nil), true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = scheduler.close() })
	require.NoError(t, scheduler.enqueue(
		retainedInterruptOutEnvelope(900, []byte{1})))
	select {
	case <-prepareEntered:
	case <-time.After(time.Second):
		t.Fatal("retained worker did not enter Pending preparation")
	}
	// Advance and latch readiness while the Pending callback is still inside
	// the response/scheduler boundary. The post-attempt epoch check must observe
	// it before sleeping, even if the wake signal is already buffered.
	owner.advanceReadiness()
	close(prepareRelease)
	recorder.waitForWrites(t, 1)
	require.Equal(t, uint32(2), owner.prepareCalls.Load())
}

func TestRetainedSubmissionCloseRetiresPendingTicketAndScrubs(t *testing.T) {
	owner := newScriptedRetainedOwner(1)
	owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultPending, currentEpoch: true,
	})
	scheduler, err := newRetainedSubmissionScheduler(
		context.Background(), 41, owner,
		newResponseWriter(io.Discard, nil), true)
	require.NoError(t, err)
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(910)))
	require.Eventually(t, func() bool {
		return owner.prepareCalls.Load() == 1
	}, time.Second, time.Millisecond)
	require.NoError(t, scheduler.close())
	require.Len(t, owner.retirements, 1)
	require.Equal(t, retainedusb.RetireConnectionClose,
		owner.retirements[0].reason)
	inIndex, _ := retainedusb.LaneInterruptIn.Index()
	scheduler.mu.Lock()
	require.Zero(t, scheduler.lanes[inIndex].orderCount)
	scheduler.mu.Unlock()
}

func TestRetainedSubmissionPayloadAndResponseRemainByteExact(t *testing.T) {
	owner := newScriptedRetainedOwner(1)
	data := bytes.Repeat([]byte{0xa5}, 64)
	owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultData, data: data,
	})
	scheduler, recorder := newManualRetainedScheduler(t, owner, nil)
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(920)))
	progressed, _, err := scheduler.serviceRound(time.Unix(920, 0))
	require.NoError(t, err)
	require.True(t, progressed)
	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, data, writes[0].packet[retSubmitHeaderSize:])
}

func TestRetainedSubmissionResetDrainsEveryExactLaneBeforeReopen(t *testing.T) {
	owner := newScriptedCombinedImportOwner(0x7301, 2)
	session, lease, scheduler := newLiveRetainedImportScheduler(
		t, 0x7401, 0x7501, owner)
	require.NoError(t, scheduler.enqueue(retainedControlInEnvelope(1001, 8)))
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(1002)))
	require.NoError(t, scheduler.enqueue(
		retainedInterruptOutEnvelope(1003, []byte{1})))
	require.Eventually(t, func() bool {
		return owner.stageCalls.Load() == 3
	}, time.Second, time.Millisecond)

	reset, err := session.issueReset(lease, time.Now().Add(time.Second))
	require.NoError(t, err)
	result, err := session.reset(reset, time.Now().Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, retainedusb.ImportResetSafe, result.State)
	require.True(t, session.reservation.admission.open.Load())
	scheduler.mu.Lock()
	require.True(t, scheduler.emptyLocked())
	require.False(t, scheduler.resetFence.Valid())
	scheduler.mu.Unlock()
	owner.mu.Lock()
	require.Len(t, owner.retirements, 3)
	seen := make(map[retainedusb.Lane]int)
	for _, retirement := range owner.retirements {
		require.Equal(t, retainedusb.RetireDeviceReset, retirement.reason)
		seen[retirement.ticket.Lane]++
	}
	owner.mu.Unlock()
	require.Equal(t, map[retainedusb.Lane]int{
		retainedusb.LaneControl:      1,
		retainedusb.LaneInterruptIn:  1,
		retainedusb.LaneInterruptOut: 1,
	}, seen)

	// No successor URB was needed to drive the reset owner or reopen. The next
	// ordinary submission is merely proof that admission is live afterward.
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(1004)))
}

func TestRetainedSubmissionResetJoinsActiveControlINAndOUT(t *testing.T) {
	tests := []struct {
		name     string
		lane     retainedusb.Lane
		envelope retainedSubmissionEnvelope
		plan     retainedOwnerPlan
	}{
		{
			name: "control", lane: retainedusb.LaneControl,
			envelope: retainedControlInEnvelope(1101, 8),
			plan: retainedOwnerPlan{
				result: retainedusb.ResultData, data: []byte{1},
			},
		},
		{
			name: "interrupt in", lane: retainedusb.LaneInterruptIn,
			envelope: retainedInterruptInEnvelope(1102),
			plan: retainedOwnerPlan{
				result: retainedusb.ResultData, data: []byte{2},
			},
		},
		{
			name: "interrupt out", lane: retainedusb.LaneInterruptOut,
			envelope: retainedInterruptOutEnvelope(1103, []byte{3}),
			plan: retainedOwnerPlan{
				result: retainedusb.ResultSuccess, actualLength: 1,
			},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			owner := newScriptedCombinedImportOwner(
				uint64(0x7601+index), 1)
			entered := make(chan struct{})
			release := make(chan struct{})
			test.plan.prepareEntered = entered
			test.plan.prepareRelease = release
			owner.appendPlan(test.lane, test.plan)
			session, lease, scheduler := newLiveRetainedImportScheduler(
				t, uint64(0x7701+index), uint64(0x7801+index), owner)
			require.NoError(t, scheduler.enqueue(test.envelope))
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("active preparation did not begin")
			}
			reset, err := session.issueReset(
				lease, time.Now().Add(time.Second))
			require.NoError(t, err)
			resetDone := make(chan error, 1)
			go func() {
				_, resetErr := session.reset(
					reset, time.Now().Add(time.Second))
				resetDone <- resetErr
			}()
			require.Eventually(t, func() bool {
				return !session.reservation.admission.open.Load()
			}, time.Second, time.Millisecond)
			select {
			case err := <-resetDone:
				t.Fatalf("reset overtook active preparation: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
			require.Equal(t, uint32(0), owner.resetCalls.Load())
			close(release)
			require.NoError(t, <-resetDone)
			require.Equal(t, uint32(1), owner.resetCalls.Load())
			require.True(t, session.reservation.admission.open.Load())
			scheduler.mu.Lock()
			require.True(t, scheduler.emptyLocked())
			scheduler.mu.Unlock()
		})
	}
}

func TestRetainedSubmissionResetSerializerTimeoutNeverReopens(t *testing.T) {
	owner := newScriptedCombinedImportOwner(0x7901, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	owner.appendPlan(retainedusb.LaneInterruptIn, retainedOwnerPlan{
		result: retainedusb.ResultData, data: []byte{1},
		prepareEntered: entered, prepareRelease: release,
	})
	session, lease, scheduler := newLiveRetainedImportScheduler(
		t, 0x7a01, 0x7b01, owner)
	require.NoError(t, scheduler.enqueue(retainedInterruptInEnvelope(1201)))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("active preparation did not begin")
	}
	reset, err := session.issueReset(lease, time.Now().Add(time.Second))
	require.NoError(t, err)
	result, err := session.reset(reset, time.Now().Add(25*time.Millisecond))
	require.Error(t, err)
	require.Equal(t, retainedusb.ImportResetQuarantined, result.State)
	require.False(t, session.reservation.admission.open.Load())
	require.True(t, session.quarantined.Load())
	require.False(t, session.released.Load())
	require.Equal(t, uint32(1), owner.resetFenceCalls.Load())
	require.Equal(t, uint32(0), owner.resetCalls.Load())
	require.Equal(t, uint32(0), owner.disconnectCalls.Load())
	close(release)
	require.Eventually(t, func() bool {
		return owner.prepareCalls.Load() == 1
	}, time.Second, time.Millisecond)
	require.False(t, session.reservation.admission.open.Load())
	require.Error(t, scheduler.reopenAfterReset(
		session.reservation, reset))
	require.False(t, session.reservation.admission.open.Load())
}

func TestRetainedSubmissionResetReopenRequiresExactCapability(t *testing.T) {
	owner := newScriptedCombinedImportOwner(0x7c01, 1)
	session, lease, scheduler := newLiveRetainedImportScheduler(
		t, 0x7d01, 0x7e01, owner)
	reset, err := session.issueReset(lease, time.Now().Add(time.Second))
	require.NoError(t, err)
	require.NoError(t, scheduler.fenceReset(session.reservation, reset))
	require.False(t, session.reservation.admission.open.Load())
	require.NoError(t, owner.FenceAndDrainReset(
		reset, time.Now().Add(time.Second)))
	require.NoError(t, scheduler.resetAndDrain(
		session.reservation, reset, time.Now().Add(time.Second)))

	forged := reset
	forged.ResetToken++
	require.Error(t, scheduler.reopenAfterReset(
		session.reservation, forged))
	require.False(t, session.reservation.admission.open.Load())
	require.True(t, scheduler.resetActive.Load())

	ownerResult, err := owner.ResetAndRestart(
		reset, time.Now().Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, retainedusb.ImportResetResult{
		Lease: reset, State: retainedusb.ImportResetSafe,
	}, ownerResult)
	require.NoError(t, scheduler.reopenAfterReset(
		session.reservation, reset))
	require.True(t, session.reservation.admission.open.Load())
	require.False(t, scheduler.resetActive.Load())

	// Neither the consumed capability nor its forged sibling can roll the
	// scheduler back after exact reopen.
	require.Error(t, scheduler.reopenAfterReset(
		session.reservation, reset))
	require.Error(t, scheduler.reopenAfterReset(
		session.reservation, forged))
	require.True(t, session.reservation.admission.open.Load())
}

func TestRetainedSubmissionResetSchedulerCancellationQuarantines(t *testing.T) {
	owner := newScriptedCombinedImportOwner(0x7f01, 1)
	session, lease, scheduler := newLiveRetainedImportScheduler(
		t, 0x8001, 0x8101, owner)
	reset, err := session.issueReset(lease, time.Now().Add(time.Second))
	require.NoError(t, err)
	scheduler.cancel()

	result, err := session.reset(reset, time.Now().Add(time.Second))
	require.Error(t, err)
	require.Equal(t, retainedusb.ImportResetQuarantined, result.State)
	require.False(t, session.reservation.admission.open.Load())
	require.True(t, session.quarantined.Load())
	require.False(t, session.released.Load())
	require.Equal(t, uint32(1), owner.resetFenceCalls.Load())
	require.Equal(t, uint32(0), owner.resetCalls.Load())
	require.Equal(t, uint32(0), owner.disconnectCalls.Load())
	require.Equal(t, uint32(1), owner.drainCalls.Load())
	require.Error(t, scheduler.reopenAfterReset(
		session.reservation, reset))
	require.False(t, session.reservation.admission.open.Load())
}

func TestRetainedSubmissionConstructorRemainsDormant(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	files := token.NewFileSet()
	identifierCount := 0
	var locations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Clean(name)
		parsed, parseErr := parser.ParseFile(files, path, nil, 0)
		require.NoError(t, parseErr)
		ast.Inspect(parsed, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if !ok || identifier.Name != "newRetainedSubmissionScheduler" {
				return true
			}
			identifierCount++
			locations = append(locations,
				files.Position(identifier.Pos()).String())
			return true
		})
	}
	require.Equal(t, 1, identifierCount,
		"constructor must have only its declaration in production Go files: %v",
		locations)
}

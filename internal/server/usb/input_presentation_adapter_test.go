package usb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
	usbdesc "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

type presentationResolution struct {
	claim     inputpresentation.Claim
	outcome   inputpresentation.Outcome
	completed time.Time
}

type presentationTestDevice struct {
	*schedulerTestDevice
	presentationEndpoint uint8
	generation           atomic.Uint64
	token                atomic.Uint64
	claimReady           chan struct{}
	resolution           chan presentationResolution
	legacyClaims         atomic.Uint64
	admissions           atomic.Uint64
	rejectAdmission      atomic.Bool
}

type resetPresentationState struct {
	pressed bool
}

type resetPresentationTestDevice struct {
	*schedulerTestDevice
	input      *inputpresentation.FixedReportScheduler[resetPresentationState]
	claimReady chan inputpresentation.Claim
	resolution chan strictPresentationResolution
}

type strictPresentationResolution struct {
	claim    inputpresentation.Claim
	outcome  inputpresentation.Outcome
	accepted bool
}

type strictAdmissionPresentationTestDevice struct {
	*schedulerTestDevice
	input      *inputpresentation.FixedReportScheduler[resetPresentationState]
	claimReady chan inputpresentation.Claim
	admissions chan bool
	resolution chan strictPresentationResolution
}

func newStrictAdmissionPresentationTestDevice(
	t testing.TB, maximumOrderedAge time.Duration, now time.Time,
) *strictAdmissionPresentationTestDevice {
	t.Helper()
	input, err := inputpresentation.NewFixedReportSchedulerWithMaximumOrderedAge(
		1, resetPresentationState{},
		func(state *resetPresentationState, destination []byte) int {
			if len(destination) < 1 {
				return 0
			}
			destination[0] = 0
			if state.pressed {
				destination[0] = 1
			}
			return 1
		},
		func(previous, next resetPresentationState) bool {
			return previous.pressed != next.pressed
		}, maximumOrderedAge, now,
	)
	if err != nil {
		t.Fatalf("create strict presentation scheduler: %v", err)
	}
	return &strictAdmissionPresentationTestDevice{
		schedulerTestDevice: &schedulerTestDevice{desc: testCompositeDescriptor()},
		input:               input,
		claimReady:          make(chan inputpresentation.Claim, 4),
		admissions:          make(chan bool, 4),
		resolution:          make(chan strictPresentationResolution, 4),
	}
}

func (*strictAdmissionPresentationTestDevice) OwnsInputPresentationEndpoint(
	endpoint uint8,
) bool {
	return endpoint == 4
}

func (device *strictAdmissionPresentationTestDevice) InputPresentationGeneration() uint64 {
	return device.input.Generation()
}

func (device *strictAdmissionPresentationTestDevice) ClaimInputPresentation(
	destination []byte, selectedAt time.Time,
) inputpresentation.Claim {
	claim := device.input.ClaimInputPresentation(destination, selectedAt)
	device.claimReady <- claim
	return claim
}

func (device *strictAdmissionPresentationTestDevice) CanAdmitInputPresentation(
	claim inputpresentation.Claim, admittedAt time.Time,
) bool {
	accepted := device.input.CanAdmitInputPresentation(claim, admittedAt)
	device.admissions <- accepted
	return accepted
}

func (device *strictAdmissionPresentationTestDevice) ResolveInputPresentation(
	claim inputpresentation.Claim, outcome inputpresentation.Outcome,
	completedAt time.Time,
) bool {
	accepted := device.input.ResolveInputPresentation(claim, outcome, completedAt)
	device.resolution <- strictPresentationResolution{
		claim: claim, outcome: outcome, accepted: accepted,
	}
	return accepted
}

func (device *strictAdmissionPresentationTestDevice) RetireInputPresentationGeneration(
	generation uint64, retiredAt time.Time,
) bool {
	return device.input.RetireInputPresentationGeneration(generation, retiredAt)
}

type blockingAdmissionPresentationTestDevice struct {
	*presentationTestDevice
	admissionStarted chan struct{}
	releaseAdmission chan struct{}
	started          atomic.Bool
}

type countedAdmissionPresentationTestDevice struct {
	*presentationTestDevice
	rejectionsRemaining atomic.Int64
}

func (device *blockingAdmissionPresentationTestDevice) CanAdmitInputPresentation(
	claim inputpresentation.Claim, admittedAt time.Time,
) bool {
	if device.started.CompareAndSwap(false, true) {
		close(device.admissionStarted)
	}
	<-device.releaseAdmission
	return device.presentationTestDevice.CanAdmitInputPresentation(
		claim, admittedAt)
}

func (device *countedAdmissionPresentationTestDevice) CanAdmitInputPresentation(
	claim inputpresentation.Claim, _ time.Time,
) bool {
	if !claim.Valid() || claim.Generation != device.generation.Load() {
		return false
	}
	device.admissions.Add(1)
	return device.rejectionsRemaining.Add(-1) < 0
}

func newResetPresentationTestDevice(t testing.TB) *resetPresentationTestDevice {
	t.Helper()
	input, err := inputpresentation.NewFixedReportScheduler(
		1, resetPresentationState{},
		func(state *resetPresentationState, destination []byte) int {
			if len(destination) < 1 {
				return 0
			}
			destination[0] = 0
			if state.pressed {
				destination[0] = 1
			}
			return 1
		},
		func(previous, next resetPresentationState) bool {
			return previous.pressed != next.pressed
		}, time.Now())
	if err != nil {
		t.Fatalf("create reset presentation scheduler: %v", err)
	}
	return &resetPresentationTestDevice{
		schedulerTestDevice: &schedulerTestDevice{desc: testCompositeDescriptor()},
		input:               input,
		claimReady:          make(chan inputpresentation.Claim, 4),
		resolution:          make(chan strictPresentationResolution, 4),
	}
}

func (*resetPresentationTestDevice) OwnsInputPresentationEndpoint(
	endpoint uint8,
) bool {
	return endpoint == 4
}

func (device *resetPresentationTestDevice) InputPresentationGeneration() uint64 {
	return device.input.Generation()
}

func (device *resetPresentationTestDevice) ClaimInputPresentation(
	destination []byte, selectedAt time.Time,
) inputpresentation.Claim {
	claim := device.input.ClaimInputPresentation(destination, selectedAt)
	device.claimReady <- claim
	return claim
}

func (device *resetPresentationTestDevice) ResolveInputPresentation(
	claim inputpresentation.Claim, outcome inputpresentation.Outcome,
	completedAt time.Time,
) bool {
	accepted := device.input.ResolveInputPresentation(claim, outcome, completedAt)
	device.resolution <- strictPresentationResolution{
		claim: claim, outcome: outcome, accepted: accepted,
	}
	return accepted
}

func (device *resetPresentationTestDevice) CanAdmitInputPresentation(
	claim inputpresentation.Claim, admittedAt time.Time,
) bool {
	return device.input.CanAdmitInputPresentation(claim, admittedAt)
}

func (device *resetPresentationTestDevice) RetireInputPresentationGeneration(
	generation uint64, retiredAt time.Time,
) bool {
	return device.input.RetireInputPresentationGeneration(generation, retiredAt)
}

func newPresentationTestDevice() *presentationTestDevice {
	device := &presentationTestDevice{
		schedulerTestDevice:  &schedulerTestDevice{desc: testCompositeDescriptor()},
		presentationEndpoint: 4,
		claimReady:           make(chan struct{}, 1),
		resolution:           make(chan presentationResolution, 2),
	}
	device.generation.Store(1)
	return device
}

func (device *presentationTestDevice) ClaimInputPresentation(
	destination []byte, selectedAt time.Time,
) inputpresentation.Claim {
	token := device.token.Add(1)
	if len(destination) == 0 {
		return inputpresentation.Claim{}
	}
	destination[0] = 0xa6
	select {
	case device.claimReady <- struct{}{}:
	default:
	}
	return inputpresentation.Claim{
		Token: token, Generation: device.generation.Load(), Size: 1,
		ReceivedAt: selectedAt, SelectedAt: selectedAt, Ordered: true,
	}
}

func (device *presentationTestDevice) OwnsInputPresentationEndpoint(
	endpoint uint8,
) bool {
	return endpoint == device.presentationEndpoint
}

func (device *presentationTestDevice) InputPresentationGeneration() uint64 {
	return device.generation.Load()
}

func (device *presentationTestDevice) ResolveInputPresentation(
	claim inputpresentation.Claim, outcome inputpresentation.Outcome,
	completedAt time.Time,
) bool {
	device.resolution <- presentationResolution{
		claim: claim, outcome: outcome, completed: completedAt,
	}
	return true
}

func (device *presentationTestDevice) CanAdmitInputPresentation(
	claim inputpresentation.Claim, _ time.Time,
) bool {
	if !claim.Valid() || claim.Generation != device.generation.Load() {
		return false
	}
	device.admissions.Add(1)
	return !device.rejectAdmission.Load()
}

func (device *presentationTestDevice) RetireInputPresentationGeneration(
	generation uint64, _ time.Time,
) bool {
	return device.generation.CompareAndSwap(generation, generation+1)
}

// Implement the legacy seam as well: modern presentation must win when both
// are available, preventing accidental fallback during migration.
func (device *presentationTestDevice) ClaimInputReport([]byte) (int, uint64) {
	device.legacyClaims.Add(1)
	return 0, 0
}

func (*presentationTestDevice) CompleteInputReport(uint64, bool) {}

func TestInterruptPresentationContractResolvesAfterSocketBoundary(t *testing.T) {
	tests := []struct {
		name        string
		write       func(<-chan struct{}) writerFunc
		wantOutcome inputpresentation.Outcome
	}{
		{
			name: "commit",
			write: func(release <-chan struct{}) writerFunc {
				return func(packet []byte) (int, error) {
					<-release
					return len(packet), nil
				}
			},
			wantOutcome: inputpresentation.OutcomeCommit,
		},
		{
			name: "defer",
			write: func(release <-chan struct{}) writerFunc {
				return func([]byte) (int, error) {
					<-release
					return 0, errors.New("synthetic presentation failure")
				}
			},
			wantOutcome: inputpresentation.OutcomeDefer,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device := newPresentationTestDevice()
			releaseWrite := make(chan struct{})
			failureSeen := make(chan error, 1)
			ctx, cancel := context.WithCancel(context.Background())
			worker := newEndpointWorker(
				ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
				64, newResponseWriter(test.write(releaseWrite), nil),
				func(err error) { failureSeen <- err },
			)
			require.True(t, worker.enqueue(701, 64, nil, nil, time.Now()))
			select {
			case <-device.claimReady:
			case <-time.After(time.Second):
				t.Fatal("modern input presentation was not claimed")
			}
			select {
			case resolution := <-device.resolution:
				t.Fatalf("claim resolved before socket boundary: %+v", resolution)
			default:
			}
			close(releaseWrite)
			select {
			case resolution := <-device.resolution:
				require.Equal(t, test.wantOutcome, resolution.outcome)
				require.True(t, resolution.claim.Valid())
				require.False(t, resolution.completed.IsZero())
			case <-time.After(time.Second):
				t.Fatal("claim was not terminally resolved")
			}
			require.Zero(t, device.legacyClaims.Load())
			require.Equal(t, uint64(1), device.admissions.Load(),
				"modern claim did not pass the final source-admission boundary")
			if test.wantOutcome == inputpresentation.OutcomeDefer {
				select {
				case <-failureSeen:
				case <-time.After(time.Second):
					t.Fatal("socket failure was not reported")
				}
			}
			cancel()
			worker.signal()
			<-worker.done
		})
	}
}

func TestInterruptPresentationAdmissionAndUnlinkHaveOneOwnershipBoundary(
	t *testing.T,
) {
	baseDevice := newPresentationTestDevice()
	device := &blockingAdmissionPresentationTestDevice{
		presentationTestDevice: baseDevice,
		admissionStarted:       make(chan struct{}),
		releaseAdmission:       make(chan struct{}),
	}
	recorder := newRecordingWriter()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorker(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, newResponseWriter(recorder, nil), func(error) {},
	)
	defer func() {
		cancel()
		worker.signal()
		<-worker.done
	}()

	const sequence = uint32(706)
	require.True(t, worker.enqueue(sequence, 64, nil, nil, time.Now()))
	select {
	case <-device.admissionStarted:
	case <-time.After(time.Second):
		t.Fatal("final source admission did not start")
	}

	unlinked := make(chan bool, 1)
	go func() { unlinked <- worker.unlink(sequence) }()
	close(device.releaseAdmission)
	require.False(t, <-unlinked,
		"unlink cancelled a response after source admission won ownership")

	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, sequence, binary.BigEndian.Uint32(writes[0].packet[4:8]))
	select {
	case resolution := <-device.resolution:
		require.Equal(t, inputpresentation.OutcomeCommit, resolution.outcome)
	case <-time.After(time.Second):
		t.Fatal("admitted claim was not terminally committed")
	}
}

func TestInterruptPresentationRejectedAdmissionDefersWithoutWriting(
	t *testing.T,
) {
	device := newPresentationTestDevice()
	device.rejectAdmission.Store(true)
	recorder := newRecordingWriter()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorker(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, newResponseWriter(recorder, nil), func(error) {},
	)
	defer func() {
		cancel()
		worker.signal()
		<-worker.done
	}()

	require.True(t, worker.enqueue(707, 64, nil, nil, time.Now()))
	select {
	case resolution := <-device.resolution:
		require.Equal(t, inputpresentation.OutcomeDefer, resolution.outcome)
		require.True(t, resolution.claim.Valid())
	case <-time.After(time.Second):
		t.Fatal("admission-rejected claim was not terminally deferred")
	}
	// No RET_SUBMIT was emitted, so the host request must remain unlinkable
	// rather than disappearing from the endpoint queue.
	require.True(t, worker.unlink(707))
	require.GreaterOrEqual(t, device.admissions.Load(), uint64(2),
		"the retained request did not make its bounded immediate retry")
	recorder.mu.Lock()
	require.Empty(t, recorder.writes,
		"source-rejected claim bytes reached the response stream")
	recorder.mu.Unlock()
}

func TestInterruptPresentationRetainedURBEventuallyReusesSequenceAndPreservesFIFO(
	t *testing.T,
) {
	baseDevice := newPresentationTestDevice()
	baseDevice.resolution = make(chan presentationResolution, 16)
	device := &countedAdmissionPresentationTestDevice{
		presentationTestDevice: baseDevice,
	}
	device.rejectionsRemaining.Store(3)
	recorder := newRecordingWriter()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorker(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, newResponseWriter(recorder, nil), func(error) {},
	)
	defer func() {
		cancel()
		worker.signal()
		<-worker.done
	}()

	const firstSequence = uint32(708)
	const secondSequence = uint32(709)
	now := time.Now()
	require.True(t, worker.enqueue(firstSequence, 64, nil, nil, now))
	require.True(t, worker.enqueue(secondSequence, 64, nil, nil, now))

	writes := recorder.waitForWrites(t, 2)
	require.Equal(t, firstSequence,
		binary.BigEndian.Uint32(writes[0].packet[4:8]),
		"a retained host request lost its original USB/IP sequence")
	require.Equal(t, secondSequence,
		binary.BigEndian.Uint32(writes[1].packet[4:8]),
		"a queued successor bypassed the retained endpoint head")
	require.GreaterOrEqual(t, device.admissions.Load(), uint64(5))

	deferred := 0
	committed := 0
	deadline := time.After(time.Second)
	for committed < 2 {
		select {
		case resolution := <-device.resolution:
			switch resolution.outcome {
			case inputpresentation.OutcomeDefer:
				deferred++
			case inputpresentation.OutcomeCommit:
				committed++
			}
		case <-deadline:
			t.Fatalf("timed out waiting for resolutions: defer=%d commit=%d",
				deferred, committed)
		}
	}
	require.GreaterOrEqual(t, deferred, 3)
	require.False(t, worker.unlink(firstSequence))
	require.False(t, worker.unlink(secondSequence))
}

func TestInterruptPresentationContractDefersWhenCancellationWinsBeforeSend(
	t *testing.T,
) {
	tests := []struct {
		name   string
		cancel func(*endpointWorker, uint32) bool
	}{
		{
			name: "unlink",
			cancel: func(worker *endpointWorker, sequence uint32) bool {
				return worker.unlink(sequence)
			},
		},
		{
			name: "reset",
			cancel: func(worker *endpointWorker, _ uint32) bool {
				worker.reset()
				return true
			},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device := newPresentationTestDevice()
			recorder := newRecordingWriter()
			responses := newResponseWriter(recorder, nil)
			// Hold response serialization after the report is claimed but before
			// markResponseStarted can transfer send ownership.
			responses.mu.Lock()
			ctx, cancel := context.WithCancel(context.Background())
			worker := newEndpointWorker(
				ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
				64, responses, func(error) {},
			)
			sequence := uint32(710 + index)
			require.True(t, worker.enqueue(
				sequence, 64, nil, nil, time.Now()))
			select {
			case <-device.claimReady:
			case <-time.After(time.Second):
				responses.mu.Unlock()
				t.Fatal("modern presentation was not claimed")
			}
			require.True(t, test.cancel(worker, sequence))
			responses.mu.Unlock()

			select {
			case resolution := <-device.resolution:
				require.Equal(t,
					inputpresentation.OutcomeDefer, resolution.outcome)
				require.True(t, resolution.claim.Valid())
			case <-time.After(time.Second):
				t.Fatal("cancelled claim was not deferred")
			}
			select {
			case resolution := <-device.resolution:
				t.Fatalf("claim resolved more than once: %+v", resolution)
			default:
			}
			require.Zero(t, device.legacyClaims.Load())
			recorder.mu.Lock()
			require.Empty(t, recorder.writes)
			recorder.mu.Unlock()

			cancel()
			worker.signal()
			<-worker.done
		})
	}
}

func TestModernPresentationSourceDoesNotConsumeAuxiliaryInterruptEndpoints(
	t *testing.T,
) {
	for _, endpoint := range []uint32{2, 3, 4} {
		t.Run(fmt.Sprintf("endpoint-%d", endpoint), func(t *testing.T) {
			device := newPresentationTestDevice()
			device.presentationEndpoint = 1
			recorder := newRecordingWriter()
			ctx, cancel := context.WithCancel(context.Background())
			worker := newEndpointWorker(
				ctx, device, endpoint, usbip.DirIn, interruptInWorker,
				time.Millisecond, 64, newResponseWriter(recorder, nil),
				func(error) {},
			)

			require.True(t, worker.enqueue(
				800+endpoint, 64, nil, nil, time.Now()))
			recorder.waitForWrites(t, 1)
			require.Zero(t, device.token.Load(),
				"auxiliary endpoint consumed the main presentation journal")
			device.schedulerTestDevice.mu.Lock()
			require.Zero(t, device.schedulerTestDevice.inputBuilds,
				"auxiliary endpoint fell back to the endpoint-agnostic builder")
			device.schedulerTestDevice.mu.Unlock()
			select {
			case resolution := <-device.resolution:
				t.Fatalf("auxiliary endpoint resolved a main claim: %+v", resolution)
			default:
			}

			cancel()
			worker.signal()
			<-worker.done
		})
	}
}

func TestUSBIPConnectionCloseRetiresTheGenerationCapturedAtAttach(t *testing.T) {
	device := newPresentationTestDevice()
	ctx, cancel := context.WithCancel(context.Background())
	schedulers := newEndpointSchedulers(
		ctx, device, newResponseWriter(discardResponseWriter{}, nil), nil,
	)
	require.Equal(t, uint64(1), schedulers.presentationGeneration)

	schedulers.close()
	require.Equal(t, uint64(2), device.generation.Load())
	// Close can be reached through more than one cleanup path. Retirement is
	// exactly once and cannot accidentally retire the successor generation.
	schedulers.close()
	require.Equal(t, uint64(2), device.generation.Load())
	cancel()
}

func TestUSBIPLifecycleResetsRotateOnlyOwnedPresentationPlane(t *testing.T) {
	device := newPresentationTestDevice()
	ctx, cancel := context.WithCancel(context.Background())
	schedulers := newEndpointSchedulers(
		ctx, device, newResponseWriter(discardResponseWriter{}, nil), nil,
	)

	// Auxiliary or output endpoint lifecycle must not displace controller input.
	schedulers.resetEndpoint(0x81)
	require.Equal(t, uint64(1), device.generation.Load())
	require.Equal(t, uint64(1), schedulers.presentationGeneration)

	// The owned 0x84 endpoint rotates the source and the connection adopts the
	// successor. Interface/configuration resets then rotate that successor, not
	// the stale generation captured at initial attach.
	schedulers.resetEndpoint(0x84)
	require.Equal(t, uint64(2), device.generation.Load())
	require.Equal(t, uint64(2), schedulers.presentationGeneration)
	schedulers.resetInterface(0)
	require.Equal(t, uint64(3), device.generation.Load())
	require.Equal(t, uint64(3), schedulers.presentationGeneration)
	schedulers.resetAll()
	require.Equal(t, uint64(4), device.generation.Load())
	require.Equal(t, uint64(4), schedulers.presentationGeneration)

	schedulers.close()
	require.Equal(t, uint64(5), device.generation.Load())
	schedulers.close()
	require.Equal(t, uint64(5), device.generation.Load())
	cancel()
}

func TestMalformedControlLifecycleSetupPreservesPendingPresentation(t *testing.T) {
	tests := []struct {
		name  string
		setup []byte
	}{
		{name: "truncated clear halt", setup: []byte{0x02, 0x01, 0, 0, 0x84, 0, 0}},
		{name: "oversized clear halt", setup: []byte{0x02, 0x01, 0, 0, 0x84, 0, 0, 0, 0}},
		{name: "clear halt wrong direction", setup: []byte{0x82, 0x01, 0, 0, 0x84, 0, 0, 0}},
		{name: "clear halt wrong feature", setup: []byte{0x02, 0x01, 1, 0, 0x84, 0, 0, 0}},
		{name: "clear halt high endpoint byte", setup: []byte{0x02, 0x01, 0, 0, 0x84, 1, 0, 0}},
		{name: "clear halt reserved endpoint bit 4", setup: []byte{0x02, 0x01, 0, 0, 0x94, 0, 0, 0}},
		{name: "clear halt reserved endpoint bit 5", setup: []byte{0x02, 0x01, 0, 0, 0xa4, 0, 0, 0}},
		{name: "clear halt reserved endpoint bit 6", setup: []byte{0x02, 0x01, 0, 0, 0xc4, 0, 0, 0}},
		{name: "clear halt isochronous endpoint", setup: []byte{0x02, 0x01, 0, 0, 0x01, 0, 0, 0}},
		{name: "clear halt nonzero length", setup: []byte{0x02, 0x01, 0, 0, 0x84, 0, 1, 0}},
		{name: "clear halt unknown endpoint", setup: []byte{0x02, 0x01, 0, 0, 0x85, 0, 0, 0}},
		{name: "set interface wrong direction", setup: []byte{0x81, 0x0b, 0, 0, 0, 0, 0, 0}},
		{name: "set interface high alternate byte", setup: []byte{0x01, 0x0b, 0, 1, 0, 0, 0, 0}},
		{name: "set interface high index byte", setup: []byte{0x01, 0x0b, 0, 0, 0, 1, 0, 0}},
		{name: "set interface nonzero length", setup: []byte{0x01, 0x0b, 0, 0, 0, 0, 1, 0}},
		{name: "set interface unknown interface", setup: []byte{0x01, 0x0b, 0, 0, 0x7f, 0, 0, 0}},
		{name: "set interface unknown alternate", setup: []byte{0x01, 0x0b, 1, 0, 0, 0, 0, 0}},
		{name: "set configuration wrong direction", setup: []byte{0x80, 0x09, 1, 0, 0, 0, 0, 0}},
		{name: "set configuration unknown value", setup: []byte{0x00, 0x09, 2, 0, 0, 0, 0, 0}},
		{name: "set configuration high value byte", setup: []byte{0x00, 0x09, 1, 1, 0, 0, 0, 0}},
		{name: "set configuration nonzero index", setup: []byte{0x00, 0x09, 1, 0, 1, 0, 0, 0}},
		{name: "set configuration high index byte", setup: []byte{0x00, 0x09, 1, 0, 0, 1, 0, 0}},
		{name: "set configuration nonzero length", setup: []byte{0x00, 0x09, 1, 0, 0, 0, 1, 0}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device := newResetPresentationTestDevice(t)
			server := New(ServerConfig{}, nil, nil)
			ctx, cancel := context.WithCancel(context.Background())
			schedulers := newEndpointSchedulers(
				ctx, device, newResponseWriter(discardResponseWriter{}, nil), nil,
			)
			require.True(t, device.input.Publish(
				resetPresentationState{pressed: true}, time.Now()))
			before := device.input.Snapshot()

			lifecycle := server.parseControlLifecycleSetup(device, test.setup)
			require.False(t, lifecycle.accepted)
			applyControlLifecycleToSchedulers(schedulers, lifecycle)
			require.Equal(t, before, device.input.Snapshot(),
				"rejected setup changed generation or pending state")

			var report [1]byte
			claim := device.ClaimInputPresentation(report[:], time.Now())
			require.True(t, claim.Valid())
			require.Equal(t, byte(1), report[0],
				"rejected setup discarded the queued press")
			require.True(t, device.ResolveInputPresentation(
				claim, inputpresentation.OutcomeCommit, time.Now()))

			schedulers.close()
			cancel()
		})
	}
}

func TestExactControlLifecycleSetupRotatesPresentation(t *testing.T) {
	tests := []struct {
		name  string
		setup []byte
		kind  controlLifecycleKind
	}{
		{name: "clear endpoint halt", setup: []byte{0x02, 0x01, 0, 0, 0x84, 0, 0, 0}, kind: controlLifecycleClearEndpointHalt},
		{name: "set interface", setup: []byte{0x01, 0x0b, 0, 0, 0, 0, 0, 0}, kind: controlLifecycleSetInterface},
		{name: "unconfigure", setup: []byte{0x00, 0x09, 0, 0, 0, 0, 0, 0}, kind: controlLifecycleSetConfiguration},
		{name: "set configuration", setup: []byte{0x00, 0x09, 1, 0, 0, 0, 0, 0}, kind: controlLifecycleSetConfiguration},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device := newResetPresentationTestDevice(t)
			server := New(ServerConfig{}, nil, nil)
			ctx, cancel := context.WithCancel(context.Background())
			schedulers := newEndpointSchedulers(
				ctx, device, newResponseWriter(discardResponseWriter{}, nil), nil,
			)
			require.True(t, device.input.Publish(
				resetPresentationState{pressed: true}, time.Now()))

			lifecycle := server.parseControlLifecycleSetup(device, test.setup)
			require.True(t, lifecycle.recognized)
			require.True(t, lifecycle.accepted)
			require.Equal(t, test.kind, lifecycle.kind)
			applyControlLifecycleToSchedulers(schedulers, lifecycle)
			require.Equal(t, uint64(2), device.InputPresentationGeneration())
			require.Zero(t, device.input.Snapshot().TransitionDepth,
				"accepted lifecycle reset retained old transitions")

			schedulers.close()
			cancel()
		})
	}
}

func TestControlLifecycleUsesDescriptorConfigurationAndActiveAlternate(t *testing.T) {
	device := newResetPresentationTestDevice(t)
	device.desc.Configuration.BConfigurationValue = 2
	device.desc.Interfaces = append(device.desc.Interfaces,
		usbdesc.InterfaceConfig{
			Descriptor: usbdesc.InterfaceDescriptor{
				BInterfaceNumber: 0, BAlternateSetting: 1,
			},
			Endpoints: []usbdesc.EndpointDescriptor{{
				BEndpointAddress: 0x85, BMAttributes: 0x03,
				WMaxPacketSize: 64, BInterval: 4,
			}},
		})
	server := New(ServerConfig{}, nil, nil)

	setConfigurationTwo := []byte{0x00, 0x09, 2, 0, 0, 0, 0, 0}
	setConfigurationOne := []byte{0x00, 0x09, 1, 0, 0, 0, 0, 0}
	require.True(t, server.parseControlLifecycleSetup(
		device, setConfigurationTwo).accepted)
	require.False(t, server.parseControlLifecycleSetup(
		device, setConfigurationOne).accepted,
		"a value absent from the descriptor was accepted")

	clearEndpoint84 := []byte{0x02, 0x01, 0, 0, 0x84, 0, 0, 0}
	clearEndpoint85 := []byte{0x02, 0x01, 0, 0, 0x85, 0, 0, 0}
	require.True(t, server.parseControlLifecycleSetup(
		device, clearEndpoint84).accepted)
	require.False(t, server.parseControlLifecycleSetup(
		device, clearEndpoint85).accepted,
		"inactive alternate-setting endpoint was accepted")

	setAlternateOne := []byte{0x01, 0x0b, 1, 0, 0, 0, 0, 0}
	server.processSubmit(context.Background(), device, 0, 0,
		setAlternateOne, nil)
	require.False(t, server.parseControlLifecycleSetup(
		device, clearEndpoint84).accepted,
		"old alternate-setting endpoint remained active")
	require.True(t, server.parseControlLifecycleSetup(
		device, clearEndpoint85).accepted)
}

func TestOwnedEndpointResetCollapsesClaimedPressAndQueuedRelease(t *testing.T) {
	device := newResetPresentationTestDevice(t)
	recorder := newRecordingWriter()
	responses := newResponseWriter(recorder, nil)
	// Stop the old worker after source selection but before response ownership.
	responses.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	schedulers := newEndpointSchedulers(ctx, device, responses, nil)

	require.True(t, device.input.Publish(
		resetPresentationState{pressed: true}, time.Now()))
	require.True(t, schedulers.enqueueInterruptIn(901, 4, 1))
	oldClaim := <-device.claimReady
	require.True(t, oldClaim.Valid(), "press was not claimed by old worker")
	require.True(t, device.input.Publish(resetPresentationState{}, time.Now()))
	retiredGeneration := oldClaim.Generation

	schedulers.resetEndpoint(0x84)
	require.NotEqual(t, retiredGeneration,
		device.InputPresentationGeneration())
	var report [1]byte
	successor := device.ClaimInputPresentation(report[:], time.Now())
	require.Equal(t, successor, <-device.claimReady)
	require.True(t, successor.Valid())
	require.NotEqual(t, retiredGeneration, successor.Generation)
	require.Zero(t, report[0],
		"successor replayed a pre-reset press")
	require.True(t, device.CanAdmitInputPresentation(successor, time.Now()),
		"successor claim remained wedged after old-generation retirement")
	require.True(t, device.ResolveInputPresentation(
		successor, inputpresentation.OutcomeCommit, time.Now()))
	successorResolution := <-device.resolution
	require.True(t, successorResolution.accepted)
	require.Equal(t, inputpresentation.OutcomeCommit,
		successorResolution.outcome)

	responses.mu.Unlock()
	oldResolution := <-device.resolution
	require.Equal(t, oldClaim, oldResolution.claim)
	require.Equal(t, inputpresentation.OutcomeDefer, oldResolution.outcome)
	require.False(t, oldResolution.accepted,
		"retired old-generation claim remained resolvable")
	cancel()
	schedulers.close()
	recorder.mu.Lock()
	require.Empty(t, recorder.writes,
		"cancelled old-generation response reached the socket")
	recorder.mu.Unlock()
}

func TestStrictAdmissionFaultDuringPausedWriterEmitsNoStaleBytesAndRecovers(
	t *testing.T,
) {
	base := time.Unix(902, 0)
	clock := newFakeEndpointClock(base)
	device := newStrictAdmissionPresentationTestDevice(
		t, 5*time.Millisecond, base,
	)
	oldLease := device.input.ProducerLease()
	require.Equal(t, inputpresentation.FixedReportPublishAcceptedOrdered,
		device.input.PublishWithLease(
			oldLease, resetPresentationState{pressed: true}, base))

	recorder := newRecordingWriter()
	responses := newResponseWriter(recorder, nil)
	// Pause after immutable source selection but before the response writer's
	// final endpoint/source admission predicate.
	responses.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorkerWithClock(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, responses, func(error) {}, clock,
	)
	defer func() {
		cancel()
		worker.signal()
		<-worker.done
	}()

	require.True(t, worker.enqueue(910, 1, nil, nil, base))
	staleClaim := <-device.claimReady
	require.True(t, staleClaim.Valid())
	require.True(t, staleClaim.Ordered)
	clock.set(base.Add(6*time.Millisecond), false)
	responses.mu.Unlock()

	require.False(t, <-device.admissions,
		"claim older than the strict policy was admitted")
	staleResolution := <-device.resolution
	require.Equal(t, inputpresentation.OutcomeDefer, staleResolution.outcome)
	require.False(t, staleResolution.accepted,
		"age-faulted claim should already be terminally revoked")
	// The fault's canonical neutral reuses sequence 910. The rejected admission
	// did not orphan that one pending host URB or require a second poll.
	neutralClaim := <-device.claimReady
	require.True(t, neutralClaim.Valid())
	require.True(t, neutralClaim.Ordered)
	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, uint32(910), binary.BigEndian.Uint32(writes[0].packet[4:8]))
	require.Zero(t, writes[0].packet[retSubmitHeaderSize])
	require.True(t, <-device.admissions)
	neutralResolution := <-device.resolution
	require.True(t, neutralResolution.accepted)
	require.Equal(t, inputpresentation.OutcomeCommit, neutralResolution.outcome)
	snapshot := device.input.Snapshot()
	require.False(t, snapshot.MandatoryNeutral)
	require.True(t, snapshot.Resynchronization)
	require.Equal(t, inputpresentation.FixedReportFaultOrderedAge,
		snapshot.LastFault)
	require.Equal(t, uint64(1), snapshot.StaleFaults)

	clock.advance(time.Millisecond)
	recoveryLease := device.input.ProducerLease()
	require.Equal(t,
		inputpresentation.FixedReportPublishAcceptedResynchronization,
		device.input.Resynchronize(
			recoveryLease, resetPresentationState{pressed: true}, clock.Now()))
	require.Equal(t,
		inputpresentation.FixedReportPublishRejectedStaleProducer,
		device.input.PublishWithLease(
			oldLease, resetPresentationState{}, clock.Now()))

	require.True(t, worker.enqueue(911, 1, nil, nil, clock.Now()))
	freshClaim := <-device.claimReady
	require.True(t, freshClaim.Valid())
	require.False(t, freshClaim.Ordered)
	writes = recorder.waitForWrites(t, 2)
	require.Equal(t, uint32(911), binary.BigEndian.Uint32(writes[1].packet[4:8]))
	require.Equal(t, byte(1), writes[1].packet[retSubmitHeaderSize])
	require.True(t, <-device.admissions)
	freshResolution := <-device.resolution
	require.True(t, freshResolution.accepted)
	require.Equal(t, inputpresentation.OutcomeCommit, freshResolution.outcome)
	require.False(t, device.input.Snapshot().Resynchronization)
}

func TestStrictPresentationCompletionSurvivesNewerProducerAtUSBWriterBoundary(
	t *testing.T,
) {
	base := time.Unix(903, 0)
	clock := newFakeEndpointClock(base.Add(time.Millisecond))
	device := newStrictAdmissionPresentationTestDevice(
		t, 100*time.Millisecond, base,
	)
	lease := device.input.ProducerLease()
	require.Equal(t, inputpresentation.FixedReportPublishAcceptedOrdered,
		device.input.PublishWithLease(
			lease, resetPresentationState{pressed: true}, base))

	writeEntered := make(chan struct{})
	releaseWrite := make(chan struct{})
	var writeCount atomic.Uint32
	destination := writerFunc(func(packet []byte) (int, error) {
		if writeCount.Add(1) == 1 {
			close(writeEntered)
			<-releaseWrite
		}
		return len(packet), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorkerWithClock(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, newResponseWriter(destination, nil), func(error) {}, clock,
	)
	defer func() {
		cancel()
		worker.signal()
		<-worker.done
	}()

	require.True(t, worker.enqueue(913, 1, nil, nil, base))
	first := <-device.claimReady
	require.True(t, first.Valid())
	select {
	case <-writeEntered:
	case <-time.After(time.Second):
		t.Fatal("socket writer was not entered after final admission")
	}
	require.True(t, <-device.admissions)

	// The producer callback owns a later capture timestamp but wins the source
	// lock before the already-admitted socket completion is delivered.
	require.Equal(t, inputpresentation.FixedReportPublishAcceptedContinuous,
		device.input.PublishWithLease(
			lease, resetPresentationState{pressed: true},
			base.Add(4*time.Millisecond)))
	clock.set(base.Add(3*time.Millisecond), false)
	close(releaseWrite)
	firstResolution := <-device.resolution
	require.True(t, firstResolution.accepted,
		"newer producer wedged the matching USB completion")
	require.Equal(t, inputpresentation.OutcomeCommit,
		firstResolution.outcome)

	clock.set(base.Add(5*time.Millisecond), true)
	require.True(t, worker.enqueue(914, 1, nil, nil, clock.Now()))
	next := <-device.claimReady
	require.True(t, next.Valid())
	require.True(t, <-device.admissions)
	nextResolution := <-device.resolution
	require.True(t, nextResolution.accepted)
	require.Equal(t, inputpresentation.OutcomeCommit,
		nextResolution.outcome)
	require.Equal(t, uint32(2), writeCount.Load())
	require.Zero(t, device.input.Snapshot().InvalidTimestamps)
}

func TestUSBIPLifecycleRetirementSurvivesFutureOldEpochProducer(
	t *testing.T,
) {
	base := time.Now()
	device := newStrictAdmissionPresentationTestDevice(t, time.Hour, base)
	oldLease := device.input.ProducerLease()
	require.Equal(t, inputpresentation.FixedReportPublishAcceptedOrdered,
		device.input.PublishWithLease(
			oldLease, resetPresentationState{pressed: true},
			base.Add(24*time.Hour)))

	ctx, cancel := context.WithCancel(context.Background())
	schedulers := newEndpointSchedulers(
		ctx, device, newResponseWriter(discardResponseWriter{}, nil), nil,
	)
	retiredGeneration := device.InputPresentationGeneration()
	schedulers.resetEndpoint(0x84)
	require.NotEqual(t, retiredGeneration,
		device.InputPresentationGeneration(),
		"USB lifecycle adapter left the poisoned generation owned")
	require.Equal(t, uint64(1),
		device.input.Snapshot().InvalidTimestamps)
	require.Equal(t,
		inputpresentation.FixedReportPublishRejectedStaleProducer,
		device.input.PublishWithLease(
			oldLease, resetPresentationState{}, base.Add(time.Millisecond)))
	require.Equal(t, inputpresentation.FixedReportPublishAcceptedOrdered,
		device.input.PublishWithLease(
			device.input.ProducerLease(), resetPresentationState{},
			base.Add(time.Millisecond)),
		"future timestamp from the retired producer epoch poisoned its successor")

	schedulers.close()
	cancel()
}

type allocationFreePresentationDevice struct {
	*schedulerTestDevice
	token     uint64
	completed uint64
}

func (device *allocationFreePresentationDevice) ClaimInputPresentation(
	destination []byte, selectedAt time.Time,
) inputpresentation.Claim {
	device.token++
	if len(destination) > 0 {
		destination[0] = byte(device.token)
	}
	return inputpresentation.Claim{
		Token: device.token, Generation: 1, Size: min(1, len(destination)),
		ReceivedAt: selectedAt, SelectedAt: selectedAt,
	}
}

func (*allocationFreePresentationDevice) OwnsInputPresentationEndpoint(
	endpoint uint8,
) bool {
	return endpoint == 4
}

func (*allocationFreePresentationDevice) InputPresentationGeneration() uint64 {
	return 1
}

func (device *allocationFreePresentationDevice) ResolveInputPresentation(
	_ inputpresentation.Claim, outcome inputpresentation.Outcome, _ time.Time,
) bool {
	if outcome == inputpresentation.OutcomeCommit {
		device.completed++
	}
	return true
}

func (*allocationFreePresentationDevice) CanAdmitInputPresentation(
	claim inputpresentation.Claim, _ time.Time,
) bool {
	return claim.Valid()
}

func (*allocationFreePresentationDevice) RetireInputPresentationGeneration(
	uint64, time.Time,
) bool {
	return true
}

func BenchmarkInterruptPresentationClaimRetSubmitWrite(b *testing.B) {
	base := time.Unix(451, 0)
	clock := newFakeEndpointClock(base)
	device := &allocationFreePresentationDevice{
		schedulerTestDevice: &schedulerTestDevice{desc: testCompositeDescriptor()},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker := newEndpointWorkerWithClock(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, newResponseWriter(discardResponseWriter{}, nil), func(error) {}, clock,
	)
	<-worker.done
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	require.True(b, worker.enqueue(1, 64, nil, nil, base))
	idx := worker.claimNext()
	completed, retain := worker.processInterruptIn(timer, idx)
	require.True(b, completed)
	require.False(b, retain)
	worker.finishCurrentWithRetention(idx, true, false)

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		now := base.Add(time.Duration(iteration+1) * time.Millisecond)
		clock.set(now, false)
		seq := uint32(iteration + 2)
		if !worker.enqueue(seq, 64, nil, nil, now) {
			b.Fatal("enqueue failed")
		}
		idx = worker.claimNext()
		completed, retain = worker.processInterruptIn(timer, idx)
		if !completed || retain {
			b.Fatal("interrupt service failed")
		}
		worker.finishCurrentWithRetention(idx, true, false)
	}
	b.StopTimer()
	require.Equal(b, uint64(b.N+1), device.completed)
}

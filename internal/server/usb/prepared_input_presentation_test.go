package usb

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/stretchr/testify/require"
)

type preparedTestResolution struct {
	claim    inputpresentation.Claim
	outcome  inputpresentation.Outcome
	accepted bool
}

type preparedPresentationTestDevice struct {
	*schedulerTestDevice

	generation atomic.Uint64
	selects    atomic.Uint64
	admissions atomic.Uint64
	resolves   atomic.Uint64
	retires    atomic.Uint64

	mu                  sync.Mutex
	nextToken           uint64
	pending             inputpresentation.Claim
	hasPending          bool
	admitted            bool
	available           bool
	invalidClaim        bool
	advertisedSize      int
	rejectionsRemaining int
	payload             [4]byte
	exactDestinations   bool
	maximumSizes        []int
	destinationSizes    [][2]int

	selected          chan inputpresentation.Claim
	resolved          chan preparedTestResolution
	admissionStarted  chan struct{}
	releaseAdmission  chan struct{}
	admissionStartOne sync.Once
}

var _ inputpresentation.PreparedSource = (*preparedPresentationTestDevice)(nil)

func newPreparedPresentationTestDevice() *preparedPresentationTestDevice {
	device := &preparedPresentationTestDevice{
		schedulerTestDevice: &schedulerTestDevice{
			desc: testCompositeDescriptor(),
		},
		available:         true,
		advertisedSize:    4,
		payload:           [4]byte{0x20, 0x01, 0x7f, 0x55},
		exactDestinations: true,
		selected:          make(chan inputpresentation.Claim, 32),
		resolved:          make(chan preparedTestResolution, 32),
	}
	device.generation.Store(1)
	return device
}

func (*preparedPresentationTestDevice) OwnsInputPresentationEndpoint(
	endpoint uint8,
) bool {
	return endpoint == 4
}

func (device *preparedPresentationTestDevice) InputPresentationGeneration() uint64 {
	return device.generation.Load()
}

func (device *preparedPresentationTestDevice) SelectInputPresentation(
	maximumSize int, selectedAt time.Time,
) (inputpresentation.Claim, bool) {
	device.selects.Add(1)
	device.mu.Lock()
	device.maximumSizes = append(device.maximumSizes, maximumSize)
	if !device.available {
		device.mu.Unlock()
		return inputpresentation.Claim{}, false
	}
	if !device.hasPending {
		device.nextToken++
		device.pending = inputpresentation.Claim{
			Token: device.nextToken, Generation: device.generation.Load(),
			Size: device.advertisedSize, ReceivedAt: selectedAt,
			SelectedAt: selectedAt, Ordered: true,
		}
		if device.invalidClaim {
			device.pending.Token = 0
		}
		device.hasPending = true
	}
	claim := device.pending
	device.mu.Unlock()
	device.selected <- claim
	return claim, true
}

func (device *preparedPresentationTestDevice) AdmitAndCopyInputPresentation(
	claim inputpresentation.Claim,
	destination []byte,
	_ time.Time,
) bool {
	device.admissions.Add(1)
	if device.admissionStarted != nil {
		device.admissionStartOne.Do(func() { close(device.admissionStarted) })
		<-device.releaseAdmission
	}

	device.mu.Lock()
	defer device.mu.Unlock()
	device.destinationSizes = append(device.destinationSizes,
		[2]int{len(destination), cap(destination)})
	if len(destination) != claim.Size || cap(destination) != claim.Size {
		device.exactDestinations = false
		return false
	}
	if !device.hasPending || claim != device.pending {
		return false
	}
	if device.rejectionsRemaining > 0 {
		device.rejectionsRemaining--
		return false
	}
	if len(destination) > len(device.payload) {
		return false
	}
	copy(destination, device.payload[:len(destination)])
	device.admitted = true
	return true
}

func (device *preparedPresentationTestDevice) ResolveInputPresentation(
	claim inputpresentation.Claim,
	outcome inputpresentation.Outcome,
	_ time.Time,
) bool {
	device.resolves.Add(1)
	device.mu.Lock()
	accepted := device.hasPending && claim == device.pending
	if accepted {
		switch outcome {
		case inputpresentation.OutcomeCommit:
			accepted = device.admitted
			if accepted {
				device.hasPending = false
				device.admitted = false
			}
		case inputpresentation.OutcomeDefer:
			device.admitted = false
		case inputpresentation.OutcomeRetire:
			device.hasPending = false
			device.admitted = false
		default:
			accepted = false
		}
	}
	device.mu.Unlock()
	device.resolved <- preparedTestResolution{
		claim: claim, outcome: outcome, accepted: accepted,
	}
	return accepted
}

func (device *preparedPresentationTestDevice) RetireInputPresentationGeneration(
	generation uint64,
	_ time.Time,
) bool {
	if !device.generation.CompareAndSwap(generation, generation+1) {
		return false
	}
	device.retires.Add(1)
	device.mu.Lock()
	if device.hasPending && device.pending.Generation == generation {
		device.hasPending = false
		device.admitted = false
	}
	device.mu.Unlock()
	return true
}

func (device *preparedPresentationTestDevice) waitSelection(
	t *testing.T,
) inputpresentation.Claim {
	t.Helper()
	select {
	case claim := <-device.selected:
		return claim
	case <-time.After(time.Second):
		t.Fatal("prepared source was not selected")
		return inputpresentation.Claim{}
	}
}

func (device *preparedPresentationTestDevice) waitResolution(
	t *testing.T,
) preparedTestResolution {
	t.Helper()
	select {
	case resolution := <-device.resolved:
		return resolution
	case <-time.After(time.Second):
		t.Fatal("prepared claim was not resolved")
		return preparedTestResolution{}
	}
}

func startPreparedTestWorker(
	t *testing.T,
	device *preparedPresentationTestDevice,
	responses *responseWriter,
	fail func(error),
) (*endpointWorker, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorker(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, responses, fail,
	)
	t.Cleanup(func() {
		cancel()
		worker.signal()
		<-worker.done
	})
	return worker, cancel
}

func TestPreparedPresentationCopiesOnlyAtSerializedFinalAdmission(
	t *testing.T,
) {
	device := newPreparedPresentationTestDevice()
	recorder := newRecordingWriter()
	responses := newResponseWriter(recorder, nil)
	responses.mu.Lock()
	worker, _ := startPreparedTestWorker(t, device, responses, func(error) {})

	require.True(t, worker.enqueue(1201, 64, nil, nil, time.Now()))
	claim := device.waitSelection(t)
	require.True(t, claim.Valid())
	require.Zero(t, device.admissions.Load(),
		"selection copied bytes before response serialization ownership")
	require.Zero(t, device.resolves.Load())
	recorder.mu.Lock()
	require.Empty(t, recorder.writes)
	recorder.mu.Unlock()

	responses.mu.Unlock()
	writes := recorder.waitForWrites(t, 1)
	resolution := device.waitResolution(t)
	require.Equal(t, inputpresentation.OutcomeCommit, resolution.outcome)
	require.True(t, resolution.accepted)
	require.Equal(t, claim, resolution.claim)
	require.Equal(t, uint64(1), device.admissions.Load())
	require.Equal(t, uint64(1), device.resolves.Load())
	require.Equal(t, uint32(4), binary.BigEndian.Uint32(
		writes[0].packet[24:28]))
	require.Equal(t, device.payload[:],
		writes[0].packet[retSubmitHeaderSize:])
	device.mu.Lock()
	require.True(t, device.exactDestinations)
	require.Equal(t, [][2]int{{4, 4}}, device.destinationSizes)
	require.Equal(t, []int{64}, device.maximumSizes)
	device.mu.Unlock()
}

func TestPreparedPresentationCancellationBeforeAdmissionRetiresWithoutCopy(
	t *testing.T,
) {
	tests := []struct {
		name   string
		cancel func(*endpointWorker, context.CancelFunc, uint32) bool
	}{
		{
			name: "unlink",
			cancel: func(worker *endpointWorker, _ context.CancelFunc,
				sequence uint32) bool {
				return worker.unlink(sequence)
			},
		},
		{
			name: "reset",
			cancel: func(worker *endpointWorker, _ context.CancelFunc,
				_ uint32) bool {
				worker.reset()
				return true
			},
		},
		{
			name: "disconnect",
			cancel: func(_ *endpointWorker, cancel context.CancelFunc,
				_ uint32) bool {
				cancel()
				return true
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device := newPreparedPresentationTestDevice()
			recorder := newRecordingWriter()
			responses := newResponseWriter(recorder, nil)
			responses.mu.Lock()
			worker, cancel := startPreparedTestWorker(
				t, device, responses, func(error) {})
			sequence := uint32(1210 + index)
			require.True(t, worker.enqueue(
				sequence, 64, nil, nil, time.Now()))
			claim := device.waitSelection(t)
			require.True(t, test.cancel(worker, cancel, sequence))
			responses.mu.Unlock()

			resolution := device.waitResolution(t)
			require.Equal(t, claim, resolution.claim)
			require.Equal(t, inputpresentation.OutcomeRetire,
				resolution.outcome)
			require.True(t, resolution.accepted)
			require.Zero(t, device.admissions.Load())
			require.Equal(t, uint64(1), device.resolves.Load())
			recorder.mu.Lock()
			require.Empty(t, recorder.writes)
			recorder.mu.Unlock()
		})
	}
}

func TestPreparedPresentationConfigurationResetBeforeAdmissionDoesNotCopy(
	t *testing.T,
) {
	device := newPreparedPresentationTestDevice()
	recorder := newRecordingWriter()
	responses := newResponseWriter(recorder, nil)
	responses.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	schedulers := newEndpointSchedulers(ctx, device, responses, nil)

	require.True(t, schedulers.enqueueInterruptIn(1220, 4, 64))
	claim := device.waitSelection(t)
	schedulers.resetAll()
	require.Equal(t, uint64(2), device.generation.Load())
	responses.mu.Unlock()

	resolution := device.waitResolution(t)
	require.Equal(t, claim, resolution.claim)
	require.Equal(t, inputpresentation.OutcomeRetire, resolution.outcome)
	require.False(t, resolution.accepted,
		"generation retirement may consume the claim before worker resolution")
	require.Zero(t, device.admissions.Load())
	require.Equal(t, uint64(1), device.resolves.Load())
	recorder.mu.Lock()
	require.Empty(t, recorder.writes)
	recorder.mu.Unlock()
	schedulers.close()
}

func TestPreparedPresentationAdmissionDeferRetriesSameClaimOnSameURB(
	t *testing.T,
) {
	device := newPreparedPresentationTestDevice()
	device.rejectionsRemaining = 1
	recorder := newRecordingWriter()
	worker, _ := startPreparedTestWorker(t, device,
		newResponseWriter(recorder, nil), func(error) {})

	require.True(t, worker.enqueue(1230, 64, nil, nil, time.Now()))
	first := device.waitSelection(t)
	second := device.waitSelection(t)
	require.Equal(t, first, second,
		"deferred selection was not an immutable retry")
	firstResolution := device.waitResolution(t)
	secondResolution := device.waitResolution(t)
	require.Equal(t, inputpresentation.OutcomeDefer,
		firstResolution.outcome)
	require.Equal(t, inputpresentation.OutcomeCommit,
		secondResolution.outcome)
	require.Equal(t, first, firstResolution.claim)
	require.Equal(t, first, secondResolution.claim)
	require.Equal(t, uint64(2), device.admissions.Load())
	require.Equal(t, uint64(2), device.resolves.Load())
	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, uint32(1230), binary.BigEndian.Uint32(
		writes[0].packet[4:8]))
	require.Equal(t, device.payload[:],
		writes[0].packet[retSubmitHeaderSize:])
}

func TestPreparedPresentationMalformedClaimFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*preparedPresentationTestDevice)
	}{
		{
			name: "invalid capability",
			configure: func(device *preparedPresentationTestDevice) {
				device.invalidClaim = true
			},
		},
		{
			name: "oversized claim",
			configure: func(device *preparedPresentationTestDevice) {
				device.advertisedSize = 65
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device := newPreparedPresentationTestDevice()
			test.configure(device)
			recorder := newRecordingWriter()
			failure := make(chan error, 1)
			worker, _ := startPreparedTestWorker(t, device,
				newResponseWriter(recorder, nil),
				func(err error) { failure <- err })
			require.True(t, worker.enqueue(
				uint32(1240+index), 64, nil, nil, time.Now()))
			claim := device.waitSelection(t)

			select {
			case err := <-failure:
				require.ErrorContains(t, err, "malformed prepared claim")
			case <-time.After(time.Second):
				t.Fatal("malformed prepared claim did not fail closed")
			}
			resolution := device.waitResolution(t)
			require.Equal(t, claim, resolution.claim)
			require.Equal(t, inputpresentation.OutcomeRetire,
				resolution.outcome)
			require.Zero(t, device.admissions.Load())
			require.Equal(t, uint64(1), device.resolves.Load())
			recorder.mu.Lock()
			require.Empty(t, recorder.writes)
			recorder.mu.Unlock()
		})
	}
}

func TestPreparedPresentationWriteFailureDefersByteExactRetry(
	t *testing.T,
) {
	device := newPreparedPresentationTestDevice()
	firstFailure := make(chan error, 1)
	firstWriter := writerFunc(func(packet []byte) (int, error) {
		require.Equal(t, device.payload[:],
			packet[retSubmitHeaderSize:])
		return 0, errors.New("synthetic prepared write failure")
	})
	firstWorker, firstCancel := startPreparedTestWorker(t, device,
		newResponseWriter(firstWriter, nil),
		func(err error) { firstFailure <- err })
	require.True(t, firstWorker.enqueue(1250, 64, nil, nil, time.Now()))
	firstClaim := device.waitSelection(t)
	firstResolution := device.waitResolution(t)
	require.Equal(t, inputpresentation.OutcomeDefer,
		firstResolution.outcome)
	require.True(t, firstResolution.accepted)
	select {
	case <-firstFailure:
	case <-time.After(time.Second):
		t.Fatal("prepared write failure was not reported")
	}
	firstCancel()
	firstWorker.signal()

	recorder := newRecordingWriter()
	secondWorker, _ := startPreparedTestWorker(t, device,
		newResponseWriter(recorder, nil), func(error) {})
	require.True(t, secondWorker.enqueue(1251, 64, nil, nil, time.Now()))
	secondClaim := device.waitSelection(t)
	require.Equal(t, firstClaim, secondClaim,
		"delivery failure did not preserve the immutable claim")
	secondResolution := device.waitResolution(t)
	require.Equal(t, inputpresentation.OutcomeCommit,
		secondResolution.outcome)
	require.True(t, secondResolution.accepted)
	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, uint32(1251), binary.BigEndian.Uint32(
		writes[0].packet[4:8]))
	require.Equal(t, device.payload[:],
		writes[0].packet[retSubmitHeaderSize:])
}

func TestPreparedPresentationFlushFailureDefersImmutableClaim(
	t *testing.T,
) {
	device := newPreparedPresentationTestDevice()
	flushFailure := errors.New("synthetic prepared flush failure")
	batcher := newBatchingWriter(
		writerFunc(func([]byte) (int, error) { return 0, flushFailure }),
		1024, 0, 0,
	)
	defer func() { _ = batcher.Close() }()
	failure := make(chan error, 1)
	worker, _ := startPreparedTestWorker(t, device,
		newResponseWriter(batcher, batcher),
		func(err error) { failure <- err })

	require.True(t, worker.enqueue(1255, 64, nil, nil, time.Now()))
	claim := device.waitSelection(t)
	resolution := device.waitResolution(t)
	require.Equal(t, claim, resolution.claim)
	require.Equal(t, inputpresentation.OutcomeDefer, resolution.outcome)
	require.True(t, resolution.accepted)
	select {
	case err := <-failure:
		require.ErrorIs(t, err, flushFailure)
	case <-time.After(time.Second):
		t.Fatal("prepared flush failure was not reported")
	}

	retry, available := device.SelectInputPresentation(64, time.Now())
	require.True(t, available)
	require.Equal(t, claim, retry,
		"flush failure did not retain the immutable prepared claim")
}

type orderedBlockingWriter struct {
	mu      sync.Mutex
	packets [][]byte
	entered chan struct{}
	release chan struct{}
	wake    chan struct{}
	calls   atomic.Uint64
}

func newOrderedBlockingWriter() *orderedBlockingWriter {
	return &orderedBlockingWriter{
		entered: make(chan struct{}), release: make(chan struct{}),
		wake: make(chan struct{}, 4),
	}
}

func (writer *orderedBlockingWriter) Write(packet []byte) (int, error) {
	call := writer.calls.Add(1)
	writer.mu.Lock()
	writer.packets = append(writer.packets, append([]byte(nil), packet...))
	writer.mu.Unlock()
	if call == 1 {
		close(writer.entered)
		<-writer.release
	}
	writer.wake <- struct{}{}
	return len(packet), nil
}

func (writer *orderedBlockingWriter) waitCalls(t *testing.T, count uint64) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for writer.calls.Load() < count {
		select {
		case <-writer.wake:
		case <-deadline.C:
			t.Fatalf("writer did not reach %d calls", count)
		}
	}
}

func TestPreparedPresentationAdmissionMakesUnlinkOrderAfterRetSubmit(
	t *testing.T,
) {
	device := newPreparedPresentationTestDevice()
	destination := newOrderedBlockingWriter()
	responses := newResponseWriter(destination, nil)
	worker, _ := startPreparedTestWorker(t, device, responses, func(error) {})

	const sequence = uint32(1260)
	require.True(t, worker.enqueue(sequence, 64, nil, nil, time.Now()))
	device.waitSelection(t)
	select {
	case <-destination.entered:
	case <-time.After(time.Second):
		t.Fatal("admitted RET_SUBMIT did not enter writer")
	}
	require.Equal(t, uint64(1), device.admissions.Load())
	require.False(t, worker.unlink(sequence),
		"unlink cancelled after prepared admission transferred ownership")

	unlinkPacket := buildRetUnlinkPacket(nil, 1261, 0)
	unlinkDone := make(chan error, 1)
	go func() {
		unlinkDone <- responses.write(unlinkPacket, true, time.Now())
	}()
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, uint64(1), destination.calls.Load(),
		"RET_UNLINK bypassed the admitted RET_SUBMIT serializer owner")
	close(destination.release)
	destination.waitCalls(t, 2)
	require.NoError(t, <-unlinkDone)
	resolution := device.waitResolution(t)
	require.Equal(t, inputpresentation.OutcomeCommit, resolution.outcome)

	destination.mu.Lock()
	require.Len(t, destination.packets, 2)
	require.Equal(t, uint32(usbip.RetSubmitCode),
		binary.BigEndian.Uint32(destination.packets[0][0:4]))
	require.Equal(t, sequence,
		binary.BigEndian.Uint32(destination.packets[0][4:8]))
	require.Equal(t, uint32(usbip.RetUnlinkCode),
		binary.BigEndian.Uint32(destination.packets[1][0:4]))
	destination.mu.Unlock()
}

func TestPreparedPresentationLifecycleUsesEndpointOwnershipAndGeneration(
	t *testing.T,
) {
	device := newPreparedPresentationTestDevice()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	schedulers := newEndpointSchedulers(ctx, device,
		newResponseWriter(discardResponseWriter{}, nil), nil)
	require.Equal(t, uint64(1), schedulers.presentationGeneration)

	schedulers.resetEndpoint(0x81)
	require.Equal(t, uint64(1), device.generation.Load(),
		"auxiliary endpoint retired prepared presentation")
	schedulers.resetEndpoint(0x84)
	require.Equal(t, uint64(2), device.generation.Load())
	require.Equal(t, uint64(2), schedulers.presentationGeneration)
	schedulers.resetAll()
	require.Equal(t, uint64(3), device.generation.Load())
	require.Equal(t, uint64(3), schedulers.presentationGeneration)
	schedulers.close()
	require.Equal(t, uint64(4), device.generation.Load())
	schedulers.close()
	require.Equal(t, uint64(4), device.generation.Load())
}

func TestPreparedSourceDoesNotConsumeAuxiliaryInterruptEndpoint(
	t *testing.T,
) {
	device := newPreparedPresentationTestDevice()
	recorder := newRecordingWriter()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorker(
		ctx, device, 2, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, newResponseWriter(recorder, nil), func(error) {},
	)
	require.True(t, worker.enqueue(1270, 64, nil, nil, time.Now()))
	recorder.waitForWrites(t, 1)
	require.Zero(t, device.selects.Load())
	device.schedulerTestDevice.mu.Lock()
	require.Zero(t, device.schedulerTestDevice.inputBuilds)
	device.schedulerTestDevice.mu.Unlock()
	cancel()
	worker.signal()
	<-worker.done
}

func TestPreparedInterfaceLeavesLegacySourceAdmissionPathByteExact(
	t *testing.T,
) {
	device := newPresentationTestDevice()
	_, optedIn := any(device).(inputpresentation.PreparedSource)
	require.False(t, optedIn)
	recorder := newRecordingWriter()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newEndpointWorker(
		ctx, device, 4, usbip.DirIn, interruptInWorker, time.Millisecond,
		64, newResponseWriter(recorder, nil), func(error) {},
	)

	require.True(t, worker.enqueue(1280, 64, nil, nil, time.Now()))
	writes := recorder.waitForWrites(t, 1)
	require.Equal(t, []byte{0xa6},
		writes[0].packet[retSubmitHeaderSize:])
	select {
	case resolution := <-device.resolution:
		require.Equal(t, inputpresentation.OutcomeCommit,
			resolution.outcome)
	case <-time.After(time.Second):
		t.Fatal("legacy Source was not resolved")
	}
	require.Equal(t, uint64(1), device.admissions.Load())
	require.Zero(t, device.legacyClaims.Load())
	cancel()
	worker.signal()
	<-worker.done
}

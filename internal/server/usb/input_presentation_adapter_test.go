package usb

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
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
	generation           uint64
	token                atomic.Uint64
	claimReady           chan struct{}
	resolution           chan presentationResolution
	legacyClaims         atomic.Uint64
}

func newPresentationTestDevice() *presentationTestDevice {
	return &presentationTestDevice{
		schedulerTestDevice:  &schedulerTestDevice{desc: testCompositeDescriptor()},
		presentationEndpoint: 4,
		generation:           1,
		claimReady:           make(chan struct{}, 1),
		resolution:           make(chan presentationResolution, 2),
	}
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
		Token: token, Generation: device.generation, Size: 1,
		ReceivedAt: selectedAt, SelectedAt: selectedAt, Ordered: true,
	}
}

func (device *presentationTestDevice) OwnsInputPresentationEndpoint(
	endpoint uint8,
) bool {
	return endpoint == device.presentationEndpoint
}

func (device *presentationTestDevice) InputPresentationGeneration() uint64 {
	return device.generation
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

func (device *presentationTestDevice) RetireInputPresentationGeneration(
	generation uint64, _ time.Time,
) bool {
	if generation != device.generation {
		return false
	}
	device.generation++
	return true
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
				uint32(800+endpoint), 64, nil, nil, time.Now()))
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
	require.Equal(t, uint64(2), device.generation)
	// Close can be reached through more than one cleanup path. Retirement is
	// exactly once and cannot accidentally retire the successor generation.
	schedulers.close()
	require.Equal(t, uint64(2), device.generation)
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
	require.True(b, worker.processInterruptIn(timer, idx))
	worker.finishCurrent(idx, true)

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
		if !worker.processInterruptIn(timer, idx) {
			b.Fatal("interrupt service failed")
		}
		worker.finishCurrent(idx, true)
	}
	b.StopTimer()
	require.Equal(b, uint64(b.N+1), device.completed)
}

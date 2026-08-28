//go:build windows && viiper_latency

package usbipprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/_testing/e2e/latency"
	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/internal/testsupport/inputlatency"
)

type fakeStream struct {
	t          *testing.T
	source     uintptr
	receivedAt time.Time
	writes     int
	deadline   time.Time
}

func (stream *fakeStream) SetWriteDeadline(deadline time.Time) error {
	stream.deadline = deadline
	return nil
}

func (stream *fakeStream) Write(frame []byte) (int, error) {
	stream.t.Helper()
	if len(frame) != frameSize {
		stream.t.Fatalf("frame length=%d, want %d", len(frame), frameSize)
	}
	if frame[0] != dualsense.StreamFrameMagic0 ||
		frame[1] != dualsense.StreamFrameMagic1 ||
		frame[2] != dualsense.StreamFrameMagic2 ||
		frame[3] != dualsense.StreamFrameMagic3 ||
		frame[4] != dualsense.StreamFrameVersionV5 ||
		frame[5] != dualsense.StreamFrameInputState {
		stream.t.Fatalf("invalid V5 frame header: % x", frame[:6])
	}
	wantCRC := crc32.Update(0, crc32.IEEETable, frame[4:12])
	wantCRC = crc32.Update(wantCRC, crc32.IEEETable, frame[frameHeaderSize:])
	if got := binary.LittleEndian.Uint32(frame[12:16]); got != wantCRC {
		stream.t.Fatalf("frame CRC=%08x, want %08x", got, wantCRC)
	}
	sequence := binary.LittleEndian.Uint32(frame[8:12])
	inputlatency.RecordBrokerReceived(
		stream.source, sequence, stream.receivedAt, 20)
	inputlatency.RecordTransportAdmitted(
		stream.source, stream.receivedAt, 9, 4, 30)
	stream.writes++
	return len(frame), nil
}

type fakeConsumer struct {
	current bool
	fence   uint64
	event   ConsumerEvent
	present bool
	polled  bool
}

func (consumer *fakeConsumer) ClockIdentity() string  { return "sdl-test-clock" }
func (consumer *fakeConsumer) Current() (bool, error) { return consumer.current, nil }
func (consumer *fakeConsumer) Fence() (uint64, error) { return consumer.fence, nil }
func (consumer *fakeConsumer) Poll() (ConsumerEvent, bool, error) {
	consumer.polled = true
	return ConsumerEvent{}, false, nil
}
func (consumer *fakeConsumer) Wait(context.Context, time.Duration) (ConsumerEvent, bool, error) {
	return consumer.event, consumer.present, nil
}

func newTestProbe(t *testing.T, stream FrameStream,
	consumer Consumer) *Probe {
	t.Helper()
	identity, err := inputlatency.ClockIdentity()
	if err != nil {
		t.Fatal(err)
	}
	frequency, err := inputlatency.Frequency()
	if err != nil {
		t.Fatal(err)
	}
	probe, err := New(Config{
		Stream: stream, Consumer: consumer,
		Lifecycle: LifecycleFunc(func(context.Context) (uint64, error) {
			return 6, nil
		}),
		Transport: latency.TransportProvenance{
			Name: latency.TransportUSBIP, Implementation: "test-usbip",
			Version:       latency.USBIPBaselineVersion,
			BuildIdentity: "0123456789abcdef0123456789abcdef01234567",
			Clock: latency.ClockProvenance{
				Name: "QueryPerformanceCounter", Identity: identity,
				FrequencyHz: frequency,
			},
			Artifacts: []latency.ArtifactIdentity{{
				Name:   "viiper.exe",
				SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			}},
		},
		CaptureTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return probe
}

func TestProbeReturnsFourRealCorrelatedBoundaries(t *testing.T) {
	stream := &fakeStream{t: t, source: 0x51, receivedAt: time.Unix(7, 8)}
	consumer := &fakeConsumer{
		fence: 100, present: true,
		event: ConsumerEvent{Down: true, EventTicks: 101, ObservedTicks: 40},
	}
	probe := newTestProbe(t, stream, consumer)
	probe.counter = func() (int64, error) { return 10, nil }

	evidence, err := probe.Capture(context.Background(), latency.CaptureRequest{
		Path: latency.PathHealthy, Boundary: latency.BoundaryAPIToConsumer,
		Transition: latency.TransitionPress,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !consumer.polled || stream.writes != 1 || stream.deadline.IsZero() {
		t.Fatalf("consumer/stream were not fenced: polled=%t writes=%d deadline=%v",
			consumer.polled, stream.writes, stream.deadline)
	}
	wantStages := []latency.StageTimestamp{
		{Stage: latency.StageClientPublished, Ticks: 10},
		{Stage: latency.StageBrokerReceived, Ticks: 20},
		{Stage: latency.StageTransportAdmitted, Ticks: 30},
		{Stage: latency.StageVirtualObserved, Ticks: 40},
	}
	if len(evidence.Timeline.Timestamps) != len(wantStages) {
		t.Fatalf("timeline=%+v", evidence.Timeline)
	}
	for index := range wantStages {
		if evidence.Timeline.Timestamps[index] != wantStages[index] {
			t.Fatalf("stage %d=%+v, want %+v", index,
				evidence.Timeline.Timestamps[index], wantStages[index])
		}
	}
	if evidence.Observation.ClockIdentity != "sdl-test-clock" ||
		evidence.Observation.PrePublishFenceTicks != 100 ||
		evidence.Observation.ObservedEventTicks != 101 ||
		evidence.Observation.Control != "south/A" ||
		evidence.Lifecycle.GenerationBefore != 6 ||
		evidence.Lifecycle.GenerationAfter != 6 {
		t.Fatalf("evidence=%+v", evidence)
	}
}

func TestProbeRejectsNonCanonicalAndStaleCapturesBeforePublish(t *testing.T) {
	for _, test := range []struct {
		name     string
		request  latency.CaptureRequest
		consumer *fakeConsumer
	}{
		{
			name: "recovery",
			request: latency.CaptureRequest{
				Path:             latency.PathRecovery,
				Boundary:         latency.BoundaryRecoveryToConsumer,
				Transition:       latency.TransitionPress,
				RecoveryScenario: "disconnect",
			},
			consumer: &fakeConsumer{},
		},
		{
			name: "already-pressed",
			request: latency.CaptureRequest{
				Path:       latency.PathHealthy,
				Boundary:   latency.BoundaryAPIToConsumer,
				Transition: latency.TransitionPress,
			},
			consumer: &fakeConsumer{current: true},
		},
		{
			name: "absent-fence",
			request: latency.CaptureRequest{
				Path:       latency.PathHealthy,
				Boundary:   latency.BoundaryAPIToConsumer,
				Transition: latency.TransitionPress,
			},
			consumer: &fakeConsumer{},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := &fakeStream{t: t, source: 0x61, receivedAt: time.Unix(9, 10)}
			probe := newTestProbe(t, stream, test.consumer)
			probe.counter = func() (int64, error) { return 10, nil }
			if _, err := probe.Capture(context.Background(), test.request); err == nil {
				t.Fatal("capture unexpectedly succeeded")
			}
			if stream.writes != 0 {
				t.Fatalf("invalid capture published %d frames", stream.writes)
			}
		})
	}
}

func TestProbeRejectsNonMonotonicCrossBoundaryEvidence(t *testing.T) {
	stream := &fakeStream{t: t, source: 0x71, receivedAt: time.Unix(11, 12)}
	consumer := &fakeConsumer{
		fence: 100, present: true,
		event: ConsumerEvent{Down: true, EventTicks: 101, ObservedTicks: 29},
	}
	probe := newTestProbe(t, stream, consumer)
	probe.counter = func() (int64, error) { return 10, nil }
	if _, err := probe.Capture(context.Background(), latency.CaptureRequest{
		Path: latency.PathHealthy, Boundary: latency.BoundaryAPIToConsumer,
		Transition: latency.TransitionPress,
	}); err == nil {
		t.Fatal("non-monotonic evidence succeeded")
	}
	if _, err := probe.Capture(context.Background(), latency.CaptureRequest{
		Path: latency.PathHealthy, Boundary: latency.BoundaryAPIToConsumer,
		Transition: latency.TransitionRelease,
	}); err == nil {
		t.Fatal("probe reused a stream after unresolved published evidence")
	}
	if stream.writes != 1 {
		t.Fatalf("terminal probe wrote %d frames, want 1", stream.writes)
	}
}

func TestWriteFullRejectsNoProgress(t *testing.T) {
	writer := writerFunc(func([]byte) (int, error) { return 0, nil })
	if err := writeFull(writer, []byte{1}); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("write result=%v", err)
	}
}

type writerFunc func([]byte) (int, error)

func (function writerFunc) Write(payload []byte) (int, error) {
	return function(payload)
}

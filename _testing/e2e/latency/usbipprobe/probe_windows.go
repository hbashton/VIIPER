//go:build windows

// Package usbipprobe adapts an already source-bound, in-process VIIPER USB/IP
// session to latency.Probe. Constructing a Probe does not start a server,
// create a bus/device, attach USB/IP, initialize SDL, or otherwise mutate the
// host. A live wrapper must supply those explicitly verified resources.
package usbipprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/_testing/e2e/latency"
	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/internal/testsupport/inputlatency"
)

const (
	frameHeaderSize = dualsense.StreamFrameHeaderSize
	frameSize       = frameHeaderSize + dualsense.InputStateSize

	defaultCaptureTimeout = time.Second
)

// FrameStream is the already authenticated, source-bound DualSense V5 device
// stream. viiperclient.DeviceStream implements this interface. It must be a
// newly opened stream owned exclusively by this Probe: Probe emits the first
// legal V5 frame at sequence zero and is the sole owner of every later byte.
type FrameStream interface {
	Write([]byte) (int, error)
	SetWriteDeadline(time.Time) error
}

// Lifecycle supplies the real USB/IP association generation. Healthy evidence
// is accepted only when the same non-zero generation surrounds the capture.
// The probe never invents a lifecycle generation.
type Lifecycle interface {
	Generation(context.Context) (uint64, error)
}

type LifecycleFunc func(context.Context) (uint64, error)

func (function LifecycleFunc) Generation(ctx context.Context) (uint64, error) {
	return function(ctx)
}

// ConsumerEvent retains SDL's event-clock timestamp separately from the QPC
// stamp taken immediately after the consumer observed the edge. EventTicks is
// only a stale-event fence; it is never subtracted from stage QPC.
type ConsumerEvent struct {
	Down          bool
	EventTicks    uint64
	ObservedTicks int64
}

// Consumer is an exact, already-opened virtual-controller observer. The live
// SDL adapter is available in viiper_latency+cgo builds; deterministic tests
// implement this interface without loading SDL or a device.
type Consumer interface {
	ClockIdentity() string
	Current() (bool, error)
	Fence() (uint64, error)
	Poll() (ConsumerEvent, bool, error)
	Wait(context.Context, time.Duration) (ConsumerEvent, bool, error)
}

type Config struct {
	// Stream must be a fresh, exclusively owned V5 stream. Sharing it with a
	// microphone or input writer would make sequence and publication evidence
	// ambiguous and is unsupported.
	Stream         FrameStream
	Consumer       Consumer
	Lifecycle      Lifecycle
	Transport      latency.TransportProvenance
	CaptureTimeout time.Duration
}

type Probe struct {
	mu sync.Mutex

	stream         FrameStream
	consumer       Consumer
	lifecycle      Lifecycle
	transport      latency.TransportProvenance
	captureTimeout time.Duration
	sequence       uint32
	terminal       error
	counter        func() (int64, error)
}

func New(config Config) (*Probe, error) {
	if !inputlatency.InstrumentationEnabled() {
		return nil, errors.New(
			"USB/IP latency probe requires a Windows build with -tags viiper_latency")
	}
	if config.Stream == nil || config.Consumer == nil || config.Lifecycle == nil {
		return nil, errors.New("USB/IP probe requires an authenticated stream, consumer, and lifecycle source")
	}
	if config.Transport.Name != latency.TransportUSBIP ||
		config.Transport.Version != latency.USBIPBaselineVersion {
		return nil, fmt.Errorf("USB/IP baseline must be named %q at version %q",
			latency.TransportUSBIP, latency.USBIPBaselineVersion)
	}
	if strings.TrimSpace(config.Transport.Implementation) == "" ||
		strings.TrimSpace(config.Transport.BuildIdentity) == "" ||
		len(config.Transport.Artifacts) == 0 {
		return nil, errors.New("USB/IP probe transport provenance is incomplete")
	}
	identity, err := inputlatency.ClockIdentity()
	if err != nil {
		return nil, fmt.Errorf("resolve QPC identity: %w", err)
	}
	frequency, err := inputlatency.Frequency()
	if err != nil {
		return nil, fmt.Errorf("resolve QPC frequency: %w", err)
	}
	if config.Transport.Clock.Name != "QueryPerformanceCounter" ||
		config.Transport.Clock.Identity != identity ||
		config.Transport.Clock.FrequencyHz != frequency {
		return nil, errors.New("USB/IP transport provenance does not name this process's exact QPC identity")
	}
	if strings.TrimSpace(config.Consumer.ClockIdentity()) == "" {
		return nil, errors.New("consumer event-clock identity is empty")
	}
	timeout := config.CaptureTimeout
	if timeout == 0 {
		timeout = defaultCaptureTimeout
	}
	if timeout < 0 {
		return nil, errors.New("capture timeout must be positive")
	}
	transport := config.Transport
	transport.Artifacts = append([]latency.ArtifactIdentity(nil), transport.Artifacts...)
	transport.Configuration = append([]latency.KeyValue(nil), transport.Configuration...)
	return &Probe{
		stream: config.Stream, consumer: config.Consumer,
		lifecycle: config.Lifecycle, transport: transport,
		captureTimeout: timeout, counter: inputlatency.Counter,
	}, nil
}

func (probe *Probe) Describe(context.Context) (latency.TransportProvenance, error) {
	if probe == nil {
		return latency.TransportProvenance{}, errors.New("nil USB/IP probe")
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	result := probe.transport
	result.Artifacts = append([]latency.ArtifactIdentity(nil), probe.transport.Artifacts...)
	result.Configuration = append([]latency.KeyValue(nil), probe.transport.Configuration...)
	return result, nil
}

func (probe *Probe) Capture(parent context.Context,
	request latency.CaptureRequest) (latency.CaptureEvidence, error) {
	if probe == nil {
		return latency.CaptureEvidence{}, errors.New("nil USB/IP probe")
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.terminal != nil {
		return latency.CaptureEvidence{}, fmt.Errorf("USB/IP stream is unusable after a framing failure: %w", probe.terminal)
	}
	if err := validateRequest(request); err != nil {
		return latency.CaptureEvidence{}, err
	}

	ctx, cancel := context.WithTimeout(parent, probe.captureTimeout)
	defer cancel()
	if err := wait(ctx, time.Duration(request.BaseDwellNS+request.PhaseOffsetNS)); err != nil {
		return latency.CaptureEvidence{}, fmt.Errorf("USB/IP capture dwell: %w", err)
	}

	generationBefore, err := probe.lifecycle.Generation(ctx)
	if err != nil {
		return latency.CaptureEvidence{}, fmt.Errorf(
			"read pre-capture USB/IP generation: %w", err)
	}
	if generationBefore == 0 {
		return latency.CaptureEvidence{}, errors.New(
			"pre-capture USB/IP generation is zero")
	}
	wantDown := request.Transition == latency.TransitionPress
	current, err := probe.consumer.Current()
	if err != nil {
		return latency.CaptureEvidence{}, fmt.Errorf("read consumer state: %w", err)
	}
	if current == wantDown {
		return latency.CaptureEvidence{}, fmt.Errorf(
			"consumer already has requested %s state", request.Transition)
	}
	if event, present, pollErr := probe.consumer.Poll(); pollErr != nil {
		return latency.CaptureEvidence{}, fmt.Errorf("drain pre-publish consumer event: %w", pollErr)
	} else if present {
		return latency.CaptureEvidence{}, fmt.Errorf(
			"consumer had a pre-publish edge: down=%t event_ticks=%d",
			event.Down, event.EventTicks)
	}
	fence, err := probe.consumer.Fence()
	if err != nil {
		return latency.CaptureEvidence{}, fmt.Errorf(
			"capture consumer event fence: %w", err)
	}
	if fence == 0 {
		return latency.CaptureEvidence{}, errors.New(
			"consumer event fence is zero")
	}

	session, err := inputlatency.Start()
	if err != nil {
		return latency.CaptureEvidence{}, err
	}
	defer session.Close()

	sequence := probe.sequence
	frame, err := buildFrame(sequence, wantDown)
	if err != nil {
		return latency.CaptureEvidence{}, err
	}
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		if err = probe.stream.SetWriteDeadline(deadline); err != nil {
			return latency.CaptureEvidence{}, fmt.Errorf("set V5 publish deadline: %w", err)
		}
	}
	clientPublishedTicks, err := probe.counter()
	if err != nil {
		return latency.CaptureEvidence{}, fmt.Errorf("stamp V5 client publication: %w", err)
	}
	if err = writeFull(probe.stream, frame[:]); err != nil {
		probe.terminal = err
		return latency.CaptureEvidence{}, fmt.Errorf("publish V5 input frame %d: %w", sequence, err)
	}
	probe.sequence++
	published := true
	succeeded := false
	defer func() {
		// Once a frame is published, an absent/wrong consumer edge or missing
		// admission leaves controller state causally unresolved. Do not reuse the
		// stream and accidentally treat a later edge as clean evidence.
		if published && !succeeded && probe.terminal == nil {
			probe.terminal = fmt.Errorf(
				"V5 frame %d was published without complete causal evidence", sequence)
		}
	}()

	remaining := probe.captureTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	event, observed, err := probe.consumer.Wait(ctx, remaining)
	if err != nil {
		return latency.CaptureEvidence{}, fmt.Errorf("wait for consumer edge: %w", err)
	}
	if !observed {
		return latency.CaptureEvidence{}, fmt.Errorf("consumer did not observe V5 frame %d", sequence)
	}
	if event.EventTicks <= fence {
		return latency.CaptureEvidence{}, fmt.Errorf(
			"consumer event %d did not follow pre-publish fence %d",
			event.EventTicks, fence)
	}
	if event.Down != wantDown {
		return latency.CaptureEvidence{}, fmt.Errorf(
			"consumer observed wrong edge for %s: down=%t",
			request.Transition, event.Down)
	}
	if event.ObservedTicks <= 0 {
		return latency.CaptureEvidence{}, errors.New("consumer observation QPC stamp is absent")
	}

	admission, err := session.Await(ctx, sequence)
	if err != nil {
		return latency.CaptureEvidence{}, err
	}
	generationAfter, err := probe.lifecycle.Generation(ctx)
	if err != nil {
		return latency.CaptureEvidence{}, fmt.Errorf(
			"read post-capture USB/IP generation: %w", err)
	}
	if generationAfter == 0 {
		return latency.CaptureEvidence{}, errors.New(
			"post-capture USB/IP generation is zero")
	}
	if generationAfter != generationBefore {
		return latency.CaptureEvidence{}, fmt.Errorf(
			"USB/IP generation changed during healthy capture: %d -> %d",
			generationBefore, generationAfter)
	}
	if clientPublishedTicks >= admission.BrokerReceivedTicks ||
		admission.BrokerReceivedTicks >= admission.TransportAdmittedTicks ||
		admission.TransportAdmittedTicks >= event.ObservedTicks {
		return latency.CaptureEvidence{}, fmt.Errorf(
			"QPC stages are absent or non-monotonic: client=%d broker=%d admitted=%d observed=%d",
			clientPublishedTicks, admission.BrokerReceivedTicks,
			admission.TransportAdmittedTicks, event.ObservedTicks)
	}

	evidence := latency.CaptureEvidence{
		Timeline: latency.Timeline{
			ClockIdentity: probe.transport.Clock.Identity,
			Timestamps: []latency.StageTimestamp{
				{Stage: latency.StageClientPublished, Ticks: clientPublishedTicks},
				{Stage: latency.StageBrokerReceived, Ticks: admission.BrokerReceivedTicks},
				{Stage: latency.StageTransportAdmitted, Ticks: admission.TransportAdmittedTicks},
				{Stage: latency.StageVirtualObserved, Ticks: event.ObservedTicks},
			},
		},
		Observation: latency.ConsumerObservation{
			ClockIdentity:        probe.consumer.ClockIdentity(),
			PrePublishFenceTicks: fence, ObservedEventTicks: event.EventTicks,
			Control: "south/A", Transition: request.Transition,
		},
		Lifecycle: latency.LifecycleEvidence{
			GenerationBefore: generationBefore, GenerationAfter: generationAfter,
		},
	}
	succeeded = true
	return evidence, nil
}

func validateRequest(request latency.CaptureRequest) error {
	if request.Path != latency.PathHealthy ||
		request.Boundary != latency.BoundaryAPIToConsumer ||
		request.RecoveryScenario != "" {
		return fmt.Errorf("USB/IP probe supports only healthy %q captures",
			latency.BoundaryAPIToConsumer)
	}
	if request.Transition != latency.TransitionPress &&
		request.Transition != latency.TransitionRelease {
		return fmt.Errorf("unsupported transition %q", request.Transition)
	}
	if request.BaseDwellNS < 0 || request.PhaseOffsetNS < 0 ||
		request.BaseDwellNS > int64(^uint64(0)>>1)-request.PhaseOffsetNS {
		return errors.New("capture dwell is negative or overflows")
	}
	return nil
}

func wait(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func buildFrame(sequence uint32, down bool) ([frameSize]byte, error) {
	var frame [frameSize]byte
	state := dualsense.NewInputState()
	if down {
		state.Buttons = dualsense.ButtonCross
	}
	payload := frame[frameHeaderSize:]
	if err := state.MarshalInto(payload); err != nil {
		return frame, fmt.Errorf("marshal DualSense input state: %w", err)
	}
	header := frame[:frameHeaderSize]
	header[0] = dualsense.StreamFrameMagic0
	header[1] = dualsense.StreamFrameMagic1
	header[2] = dualsense.StreamFrameMagic2
	header[3] = dualsense.StreamFrameMagic3
	header[4] = dualsense.StreamFrameVersionV5
	header[5] = dualsense.StreamFrameInputState
	binary.LittleEndian.PutUint16(header[6:8], uint16(len(payload)))
	binary.LittleEndian.PutUint32(header[8:12], sequence)
	crc := crc32.Update(0, crc32.IEEETable, header[4:12])
	crc = crc32.Update(crc, crc32.IEEETable, payload)
	binary.LittleEndian.PutUint32(header[12:16], crc)
	return frame, nil
}

func writeFull(writer io.Writer, payload []byte) error {
	for len(payload) != 0 {
		written, err := writer.Write(payload)
		if written < 0 || written > len(payload) {
			return fmt.Errorf("invalid stream write count %d for %d bytes", written, len(payload))
		}
		payload = payload[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

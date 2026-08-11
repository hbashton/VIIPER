package usb_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
	usbdevice "github.com/Alia5/VIIPER/usb"
)

const nativePlayStationSoakTimeout = 5 * time.Second

type nativePlayStationEndpointKey struct {
	deviceID uint64
	address  uint8
}

// nativePlayStationSoakDriver is an in-memory model of the exclusive UdeCx
// broker session. It deliberately assigns endpoint and device sequences at the
// submission boundary, then lets several Host dequeue workers observe them out
// of order. Every token gets exactly one waiter, so a missing, duplicate, stale,
// or cross-device completion fails the transport gate instead of being hidden
// by callback counts.
type nativePlayStationSoakDriver struct {
	operations chan udecx.Operation

	mu                sync.Mutex
	nextToken         uint64
	endpointSequences map[nativePlayStationEndpointKey]uint64
	deviceSequences   map[uint64]uint64
	waiters           map[uint64]chan udecx.Completion
	completed         map[uint64]struct{}
	inputs            map[udecx.DeviceIdentity][]udecx.InputReport
	created           []udecx.CreateDevice
	destroyed         []udecx.DeviceIdentity
	failures          []error
}

func newNativePlayStationSoakDriver() *nativePlayStationSoakDriver {
	return &nativePlayStationSoakDriver{
		operations:        make(chan udecx.Operation, 4096),
		nextToken:         1,
		endpointSequences: make(map[nativePlayStationEndpointKey]uint64),
		deviceSequences:   make(map[uint64]uint64),
		waiters:           make(map[uint64]chan udecx.Completion),
		completed:         make(map[uint64]struct{}),
		inputs:            make(map[udecx.DeviceIdentity][]udecx.InputReport),
	}
}

func (d *nativePlayStationSoakDriver) CreateDevice(_ context.Context, device udecx.CreateDevice) error {
	d.mu.Lock()
	d.created = append(d.created, device)
	d.mu.Unlock()
	return nil
}

func (d *nativePlayStationSoakDriver) DestroyDevice(_ context.Context, identity udecx.DeviceIdentity) error {
	d.mu.Lock()
	d.destroyed = append(d.destroyed, identity)
	d.mu.Unlock()
	return nil
}

func (d *nativePlayStationSoakDriver) Dequeue(ctx context.Context, _ []byte) (udecx.Operation, error) {
	select {
	case op := <-d.operations:
		return op, nil
	case <-ctx.Done():
		return udecx.Operation{}, ctx.Err()
	}
}

func cloneNativeCompletion(completion udecx.Completion) udecx.Completion {
	completion.Payload = append([]byte(nil), completion.Payload...)
	completion.IsoPackets = append([]udecx.IsoPacket(nil), completion.IsoPackets...)
	return completion
}

func (d *nativePlayStationSoakDriver) Complete(
	ctx context.Context, completion udecx.Completion,
) error {
	completion = cloneNativeCompletion(completion)
	d.mu.Lock()
	waiter := d.waiters[completion.Token]
	_, duplicate := d.completed[completion.Token]
	if waiter == nil {
		d.failures = append(d.failures, fmt.Errorf(
			"completion token %d had no live UdeCx request", completion.Token))
	} else if duplicate {
		d.failures = append(d.failures, fmt.Errorf(
			"completion token %d was delivered more than once", completion.Token))
	} else {
		d.completed[completion.Token] = struct{}{}
	}
	d.mu.Unlock()
	if waiter == nil || duplicate {
		return nil
	}
	select {
	case waiter <- completion:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *nativePlayStationSoakDriver) QueryStats(context.Context) (udecx.Stats, error) {
	return udecx.Stats{}, nil
}

func (d *nativePlayStationSoakDriver) SubmitInputReport(
	ctx context.Context, report udecx.InputReport,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	report.Payload = append([]byte(nil), report.Payload...)
	identity := udecx.DeviceIdentity{DeviceID: report.DeviceID, Generation: report.Generation}
	d.mu.Lock()
	d.inputs[identity] = append(d.inputs[identity], report)
	d.mu.Unlock()
	return nil
}

func (d *nativePlayStationSoakDriver) submit(
	identity udecx.DeviceIdentity, op udecx.Operation, acknowledged bool,
) (uint64, <-chan udecx.Completion) {
	d.mu.Lock()
	op.DeviceID, op.Generation = identity.DeviceID, identity.Generation
	if op.Kind != udecx.OperationCancel {
		key := nativePlayStationEndpointKey{deviceID: identity.DeviceID, address: op.EndpointAddress}
		d.endpointSequences[key]++
		d.deviceSequences[identity.DeviceID]++
		op.EndpointSequence = d.endpointSequences[key]
		op.DeviceSequence = d.deviceSequences[identity.DeviceID]
	}
	if op.Kind == udecx.OperationTransfer || op.Kind == udecx.OperationControl || acknowledged {
		op.Token = d.nextToken
		d.nextToken++
		waiter := make(chan udecx.Completion, 1)
		d.waiters[op.Token] = waiter
		d.mu.Unlock()
		d.operations <- op
		return op.Token, waiter
	}
	d.mu.Unlock()
	d.operations <- op
	return 0, nil
}

// submitCancellable models a kernel-owned request: it has a stable token and
// ordered endpoint/device sequences, but cancellation retires it in the driver
// and therefore no user-mode completion waiter may ever observe it.
func (d *nativePlayStationSoakDriver) submitCancellable(
	identity udecx.DeviceIdentity, op udecx.Operation,
) uint64 {
	d.mu.Lock()
	op.DeviceID, op.Generation = identity.DeviceID, identity.Generation
	key := nativePlayStationEndpointKey{deviceID: identity.DeviceID, address: op.EndpointAddress}
	d.endpointSequences[key]++
	d.deviceSequences[identity.DeviceID]++
	op.EndpointSequence = d.endpointSequences[key]
	op.DeviceSequence = d.deviceSequences[identity.DeviceID]
	op.Token = d.nextToken
	d.nextToken++
	d.mu.Unlock()
	d.operations <- op
	return op.Token
}

func (d *nativePlayStationSoakDriver) cancel(
	identity udecx.DeviceIdentity, token uint64, endpoint uint8,
) {
	d.operations <- udecx.Operation{
		Kind: udecx.OperationCancel, Token: token,
		DeviceID: identity.DeviceID, Generation: identity.Generation,
		EndpointAddress: endpoint,
	}
}

func (d *nativePlayStationSoakDriver) wait(
	t *testing.T, token uint64, waiter <-chan udecx.Completion,
) udecx.Completion {
	t.Helper()
	select {
	case completion := <-waiter:
		if completion.Token != token {
			t.Fatalf("completion token=%d want=%d", completion.Token, token)
		}
		return completion
	case <-time.After(nativePlayStationSoakTimeout):
		t.Fatalf("timed out waiting for native completion token %d", token)
		return udecx.Completion{}
	}
}

func (d *nativePlayStationSoakDriver) waitFromWorker(
	token uint64, waiter <-chan udecx.Completion,
) (udecx.Completion, error) {
	select {
	case completion := <-waiter:
		if completion.Token != token {
			return completion, fmt.Errorf("completion token=%d want=%d", completion.Token, token)
		}
		return completion, nil
	case <-time.After(nativePlayStationSoakTimeout):
		return udecx.Completion{}, fmt.Errorf("timed out waiting for native completion token %d", token)
	}
}

func (d *nativePlayStationSoakDriver) inputSnapshot(
	identity udecx.DeviceIdentity,
) []udecx.InputReport {
	d.mu.Lock()
	defer d.mu.Unlock()
	reports := make([]udecx.InputReport, len(d.inputs[identity]))
	for index, report := range d.inputs[identity] {
		reports[index] = report
		reports[index].Payload = append([]byte(nil), report.Payload...)
	}
	return reports
}

func (d *nativePlayStationSoakDriver) requireClean(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.failures) != 0 {
		t.Fatalf("native broker failures: %v", d.failures)
	}
	for token := range d.waiters {
		if _, ok := d.completed[token]; !ok {
			t.Fatalf("native request token %d never reached a terminal completion", token)
		}
	}
}

// nativePlayStationCancelGate creates the real scheduling race in a controlled
// place: a dequeued media request is held immediately before NativeProcessor,
// then the kernel cancel notification wins ownership. NativeProcessor is still
// invoked with the cancelled context so this gate proves the adapter itself
// does not consume or publish media after cancellation.
type nativePlayStationCancelGate struct {
	inner udecx.OperationProcessor

	mu       sync.Mutex
	armed    bool
	identity udecx.DeviceIdentity
	endpoint uint8
	started  chan struct{}
	result   chan error
}

func (g *nativePlayStationCancelGate) arm(
	identity udecx.DeviceIdentity, endpoint uint8,
) (<-chan struct{}, <-chan error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.armed {
		panic("native PlayStation cancellation gate was armed twice")
	}
	g.armed = true
	g.identity = identity
	g.endpoint = endpoint
	g.started = make(chan struct{})
	g.result = make(chan error, 1)
	return g.started, g.result
}

func (g *nativePlayStationCancelGate) Process(
	ctx context.Context, dev usbdevice.Device, op udecx.Operation,
) (udecx.Completion, error) {
	g.mu.Lock()
	blocked := g.armed && op.Kind == udecx.OperationTransfer &&
		op.DeviceID == g.identity.DeviceID && op.Generation == g.identity.Generation &&
		op.EndpointAddress == g.endpoint
	started, result := g.started, g.result
	if blocked {
		g.armed = false
	}
	g.mu.Unlock()
	if !blocked {
		return g.inner.Process(ctx, dev, op)
	}
	close(started)
	<-ctx.Done()
	completion, err := g.inner.Process(ctx, dev, op)
	result <- err
	return completion, err
}

func (g *nativePlayStationCancelGate) Lifecycle(
	ctx context.Context, dev usbdevice.Device, op udecx.Operation,
) error {
	return g.inner.Lifecycle(ctx, dev, op)
}

func (g *nativePlayStationCancelGate) Reset(
	dev usbdevice.Device, identity udecx.DeviceIdentity,
) {
	g.inner.Reset(dev, identity)
}

type synchronizedDualSenseCapture struct {
	mu       sync.Mutex
	outputs  []dualsense.OutputState
	atomic   []dualSenseAtomicCapture
	realtime []dualsense.OutputState
	resets   int
}

type dualSenseCaptureSnapshot struct {
	outputs  []dualsense.OutputState
	atomic   []dualSenseAtomicCapture
	realtime []dualsense.OutputState
	resets   int
}

func (capture *synchronizedDualSenseCapture) attach(dev *dualsense.DualSense) {
	dev.SetOutputCallback(func(state dualsense.OutputState) {
		capture.mu.Lock()
		capture.outputs = append(capture.outputs, state)
		capture.mu.Unlock()
	})
	dev.SetAtomicAudioHapticsCallback(func(state dualsense.OutputState, speaker []byte) {
		capture.mu.Lock()
		capture.atomic = append(capture.atomic, dualSenseAtomicCapture{
			feedback: state, speaker: append([]byte(nil), speaker...),
		})
		capture.mu.Unlock()
	})
	dev.SetRealtimeHapticsCallback(func(state dualsense.OutputState) {
		capture.mu.Lock()
		capture.realtime = append(capture.realtime, state)
		capture.mu.Unlock()
	})
	dev.SetSpeakerResetCallback(func() {
		capture.mu.Lock()
		capture.resets++
		capture.mu.Unlock()
	})
}

func (capture *synchronizedDualSenseCapture) snapshot() dualSenseCaptureSnapshot {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	result := dualSenseCaptureSnapshot{
		outputs:  append([]dualsense.OutputState(nil), capture.outputs...),
		atomic:   append([]dualSenseAtomicCapture(nil), capture.atomic...),
		realtime: append([]dualsense.OutputState(nil), capture.realtime...),
		resets:   capture.resets,
	}
	for index := range result.atomic {
		result.atomic[index].speaker = append([]byte(nil), result.atomic[index].speaker...)
	}
	return result
}

type synchronizedDualShock4Capture struct {
	mu      sync.Mutex
	outputs []dualshock4.OutputState
	speaker [][]byte
	resets  int
}

type dualShock4CaptureSnapshot struct {
	outputs []dualshock4.OutputState
	speaker [][]byte
	resets  int
}

func (capture *synchronizedDualShock4Capture) attach(dev *dualshock4.DualShock4) {
	dev.SetOutputCallback(func(state dualshock4.OutputState) {
		capture.mu.Lock()
		capture.outputs = append(capture.outputs, state)
		capture.mu.Unlock()
	})
	dev.SetSpeakerCallback(func(pcm []byte) {
		capture.mu.Lock()
		capture.speaker = append(capture.speaker, append([]byte(nil), pcm...))
		capture.mu.Unlock()
	})
	dev.SetSpeakerResetCallback(func() {
		capture.mu.Lock()
		capture.resets++
		capture.mu.Unlock()
	})
}

func (capture *synchronizedDualShock4Capture) snapshot() dualShock4CaptureSnapshot {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	result := dualShock4CaptureSnapshot{
		outputs: append([]dualshock4.OutputState(nil), capture.outputs...),
		resets:  capture.resets,
	}
	for _, pcm := range capture.speaker {
		result.speaker = append(result.speaker, append([]byte(nil), pcm...))
	}
	return result
}

type nativePlayStationSoakCase struct {
	name           string
	identity       udecx.DeviceIdentity
	native         usbdevice.Device
	legacy         usbdevice.Device
	speakerEP      uint8
	microphoneEP   uint8
	hidInEP        uint8
	hidOutEP       uint8
	speakerMeta    udecx.Operation
	microphoneMeta udecx.Operation
	hidInMeta      udecx.Operation
	hidOutMeta     udecx.Operation

	setLegacyAudioActive func(bool)
	resetLegacyEndpoint  func(uint8)
	queueMicrophone      func([]byte)
	oracleMicrophone     func([]udecx.IsoPacket) nativeSoakIsoExpectation
	legacySpeaker        func([]byte)
	legacyHID            func([]byte)
	makeSpeaker          func(int) ([]byte, int, uint32)
	makeMicrophone       func(int) ([]byte, int, uint32)
	makeHID              func() []byte
	setInputMarker       func(int8)
	inputMarker          func([]byte) int8
	requireParity        func(*testing.T)
	requireFinalCounts   func(*testing.T, int, int)
}

func nativeSoakEndpointMetadata(t *testing.T, dev usbdevice.Device, address uint8) udecx.Operation {
	t.Helper()
	return endpointOperation(t, udecx.DeviceIdentity{DeviceID: 1, Generation: 1},
		udecx.OperationTransfer, dev.GetDescriptor().Device.Speed,
		parityEndpoint(t, dev, address))
}

func copyNativeEndpointMetadata(dst *udecx.Operation, source udecx.Operation) {
	dst.EndpointAddress = source.EndpointAddress
	dst.EndpointAttributes = source.EndpointAttributes
	dst.EndpointInterval = source.EndpointInterval
	dst.EndpointMaxPacketSize = source.EndpointMaxPacketSize
}

func makeNativePlayStationSoakCases(t *testing.T) []*nativePlayStationSoakCase {
	t.Helper()
	cases := make([]*nativePlayStationSoakCase, 0, 3)

	for index, edge := range []bool{false, true} {
		var native, legacy *dualsense.DualSense
		var err error
		if edge {
			native, err = dualsense.NewEdge(nil)
			if err == nil {
				legacy, err = dualsense.NewEdge(nil)
			}
		} else {
			native, err = dualsense.New(nil)
			if err == nil {
				legacy, err = dualsense.New(nil)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		nativeCapture, legacyCapture := &synchronizedDualSenseCapture{}, &synchronizedDualSenseCapture{}
		nativeCapture.attach(native)
		legacyCapture.attach(legacy)
		name := "DualSense"
		if edge {
			name = "DualSense Edge"
		}
		soakCase := &nativePlayStationSoakCase{
			name: name, identity: udecx.DeviceIdentity{DeviceID: uint64(index + 1)},
			native: native, legacy: legacy,
			speakerEP:    dualsense.EndpointHapticsAudioOut,
			microphoneEP: dualsense.EndpointMicrophoneIn,
			hidInEP:      dualsense.EndpointIn, hidOutEP: dualsense.EndpointOut,
			setLegacyAudioActive: func(active bool) {
				alt := uint8(0)
				if active {
					alt = 1
				}
				legacy.SetInterfaceAltSetting(dualsense.InterfaceHapticsAudio, alt)
				legacy.SetInterfaceAltSetting(dualsense.InterfaceMicrophone, alt)
			},
			resetLegacyEndpoint: legacy.ResetEndpoint,
			queueMicrophone: func(frame []byte) {
				legacy.QueueMicrophonePCMFrame(frame)
				native.QueueMicrophonePCMFrame(frame)
			},
			oracleMicrophone: func(packets []udecx.IsoPacket) nativeSoakIsoExpectation {
				return readOracleIsoPackets(t, legacy, dualsense.EndpointMicrophoneIn, packets)
			},
			legacySpeaker: func(payload []byte) {
				legacy.HandleTransfer(context.Background(), dualsense.EndpointHapticsAudioOut&0x0f,
					usbdevice.DirectionOut, payload)
			},
			legacyHID: func(payload []byte) {
				legacy.HandleTransfer(context.Background(), dualsense.EndpointOut&0x0f,
					usbdevice.DirectionOut, payload)
			},
			makeSpeaker: func(iteration int) ([]byte, int, uint32) {
				return dualSensePCM(480, int16(300+iteration*7)), 10,
					dualsense.USBHapticsAudioPacketSize
			},
			makeMicrophone: func(iteration int) ([]byte, int, uint32) {
				return patternedPCM(dualsense.USBMicrophoneClientFrameSize,
					byte(0x31+iteration*13)), 10, dualsense.USBMicrophoneMaxPacketSize
			},
			makeHID: func() []byte {
				report := make([]byte, dualsense.OutputReportSize)
				report[0], report[1], report[2] = dualsense.ReportIDOutput, 0x03, 0x14
				report[3], report[4] = 0x39, 0xa7
				report[44], report[45], report[46], report[47] = 0x1f, 0x24, 0x68, 0xb2
				return report
			},
			setInputMarker: func(marker int8) {
				state := dualsense.NewInputState()
				state.LX = marker
				state.Buttons = dualsense.ButtonCross | dualsense.ButtonR3
				native.UpdateInputState(state)
			},
			inputMarker: func(payload []byte) int8 { return int8(int16(payload[1]) - 128) },
			requireParity: func(t *testing.T) {
				requireSynchronizedDualSenseParity(t, legacyCapture.snapshot(), nativeCapture.snapshot())
			},
			requireFinalCounts: func(t *testing.T, mediaFrames, stateReports int) {
				for label, snapshot := range map[string]dualSenseCaptureSnapshot{
					"oracle": legacyCapture.snapshot(), "native": nativeCapture.snapshot(),
				} {
					if len(snapshot.atomic) != mediaFrames || len(snapshot.outputs) != stateReports {
						t.Fatalf("%s %s final delivery totals: atomic=%d/%d state=%d/%d",
							name, label, len(snapshot.atomic), mediaFrames,
							len(snapshot.outputs), stateReports)
					}
				}
			},
		}
		soakCase.speakerMeta = nativeSoakEndpointMetadata(t, native, soakCase.speakerEP)
		soakCase.microphoneMeta = nativeSoakEndpointMetadata(t, native, soakCase.microphoneEP)
		soakCase.hidInMeta = nativeSoakEndpointMetadata(t, native, soakCase.hidInEP)
		soakCase.hidOutMeta = nativeSoakEndpointMetadata(t, native, soakCase.hidOutEP)
		cases = append(cases, soakCase)
	}

	native, err := dualshock4.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := dualshock4.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	nativeCapture, legacyCapture := &synchronizedDualShock4Capture{}, &synchronizedDualShock4Capture{}
	nativeCapture.attach(native)
	legacyCapture.attach(legacy)
	ds4Case := &nativePlayStationSoakCase{
		name: "DualShock 4", identity: udecx.DeviceIdentity{DeviceID: 3},
		native: native, legacy: legacy,
		speakerEP:    dualshock4.EndpointAudioOut,
		microphoneEP: dualshock4.EndpointMicrophoneIn,
		hidInEP:      dualshock4.EndpointIn, hidOutEP: dualshock4.EndpointOut,
		setLegacyAudioActive: func(active bool) {
			alt := uint8(0)
			if active {
				alt = 1
			}
			legacy.SetInterfaceAltSetting(dualshock4.InterfaceSpeaker, alt)
			legacy.SetInterfaceAltSetting(dualshock4.InterfaceMicrophone, alt)
		},
		resetLegacyEndpoint: legacy.ResetEndpoint,
		queueMicrophone: func(frame []byte) {
			legacy.QueueMicrophonePCMFrame(frame)
			native.QueueMicrophonePCMFrame(frame)
		},
		oracleMicrophone: func(packets []udecx.IsoPacket) nativeSoakIsoExpectation {
			return readOracleIsoPackets(t, legacy, dualshock4.EndpointMicrophoneIn, packets)
		},
		legacySpeaker: func(payload []byte) {
			legacy.HandleTransfer(context.Background(), dualshock4.EndpointAudioOut&0x0f,
				usbdevice.DirectionOut, payload)
		},
		legacyHID: func(payload []byte) {
			legacy.HandleTransfer(context.Background(), dualshock4.EndpointOut&0x0f,
				usbdevice.DirectionOut, payload)
		},
		makeSpeaker: func(iteration int) ([]byte, int, uint32) {
			const packetLength = 128
			return patternedPCM(10*packetLength, byte(0x47+iteration*17)), 10, packetLength
		},
		makeMicrophone: func(iteration int) ([]byte, int, uint32) {
			return patternedPCM(dualshock4.USBMicrophoneClientFrameSize,
				byte(0x19+iteration*11)), 10, dualshock4.USBMicrophoneMaxPacketSize
		},
		makeHID: func() []byte {
			return []byte{dualshock4.ReportIDOutput, 0, 0, 0, 0x29, 0xc8, 0x12, 0x56, 0x9a, 4, 8}
		},
		setInputMarker: func(marker int8) {
			state := dualshock4.NewInputState()
			state.LX = marker
			state.Buttons = dualshock4.ButtonCross | dualshock4.ButtonR3
			native.UpdateInputState(state)
		},
		inputMarker: func(payload []byte) int8 { return int8(int16(payload[1]) - 128) },
		requireParity: func(t *testing.T) {
			requireSynchronizedDualShock4Parity(t, legacyCapture.snapshot(), nativeCapture.snapshot())
		},
		requireFinalCounts: func(t *testing.T, mediaFrames, stateReports int) {
			for label, snapshot := range map[string]dualShock4CaptureSnapshot{
				"oracle": legacyCapture.snapshot(), "native": nativeCapture.snapshot(),
			} {
				if len(snapshot.speaker) != mediaFrames || len(snapshot.outputs) != stateReports {
					t.Fatalf("DualShock 4 %s final delivery totals: speaker=%d/%d state=%d/%d",
						label, len(snapshot.speaker), mediaFrames,
						len(snapshot.outputs), stateReports)
				}
			}
		},
	}
	ds4Case.speakerMeta = nativeSoakEndpointMetadata(t, native, ds4Case.speakerEP)
	ds4Case.microphoneMeta = nativeSoakEndpointMetadata(t, native, ds4Case.microphoneEP)
	ds4Case.hidInMeta = nativeSoakEndpointMetadata(t, native, ds4Case.hidInEP)
	ds4Case.hidOutMeta = nativeSoakEndpointMetadata(t, native, ds4Case.hidOutEP)
	return append(cases, ds4Case)
}

type nativeSoakIsoExpectation struct {
	payload        []byte
	transferLength uint32
	packets        []udecx.IsoPacket
}

func readOracleIsoPackets(t *testing.T, dev usbdevice.Device, endpoint uint8,
	packets []udecx.IsoPacket,
) nativeSoakIsoExpectation {
	t.Helper()
	reader, ok := dev.(usbdevice.IsochronousInputDevice)
	if !ok {
		t.Fatalf("%T does not expose its immutable caller-buffer ISO-IN contract", dev)
	}
	expectation := nativeSoakIsoExpectation{packets: make([]udecx.IsoPacket, len(packets))}
	for _, packet := range packets {
		end := packet.Offset + packet.Length
		if uint32(len(expectation.payload)) < end {
			expectation.payload = append(expectation.payload,
				make([]byte, int(end)-len(expectation.payload))...)
		}
	}
	for index, packet := range packets {
		region := expectation.payload[packet.Offset : packet.Offset+packet.Length]
		written, err := reader.ReadIsochronousInput(
			context.Background(), uint32(endpoint&0x0f), region)
		if err != nil {
			t.Fatalf("%T oracle ISO-IN packet %d: %v", dev, index, err)
		}
		if written < 0 || written > len(region) {
			t.Fatalf("%T oracle ISO-IN packet %d wrote %d bytes into %d bytes",
				dev, index, written, len(region))
		}
		expectation.packets[index] = udecx.IsoPacket{
			Offset: packet.Offset, Length: uint32(written),
		}
		expectation.transferLength += uint32(written)
	}
	return expectation
}

func nativeSoakIsoPacketsEqual(got, want []udecx.IsoPacket) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func nativeSoakByteDifference(got, want []byte) string {
	limit := min(len(got), len(want))
	index := 0
	for index < limit && got[index] == want[index] {
		index++
	}
	if index == limit {
		if len(got) == len(want) {
			return "none"
		}
		return fmt.Sprintf("length boundary %d (got=%d want=%d)", index, len(got), len(want))
	}
	start := max(0, index-8)
	end := min(limit, index+9)
	return fmt.Sprintf("offset %d got[%d:%d]=%x want[%d:%d]=%x",
		index, start, end, got[start:end], start, end, want[start:end])
}

func requireSynchronizedDualSenseParity(
	t *testing.T, legacy, native dualSenseCaptureSnapshot,
) {
	t.Helper()
	if legacy.resets != native.resets || len(legacy.outputs) != len(native.outputs) ||
		len(legacy.atomic) != len(native.atomic) || len(legacy.realtime) != len(native.realtime) {
		t.Fatalf("DualSense transport callbacks differ: legacy outputs=%d atomic=%d realtime=%d resets=%d; native outputs=%d atomic=%d realtime=%d resets=%d",
			len(legacy.outputs), len(legacy.atomic), len(legacy.realtime), legacy.resets,
			len(native.outputs), len(native.atomic), len(native.realtime), native.resets)
	}
	for index := range legacy.outputs {
		if legacy.outputs[index] != native.outputs[index] {
			t.Fatalf("DualSense HID output %d changed across native transport", index)
		}
	}
	for index := range legacy.atomic {
		if legacy.atomic[index].feedback != native.atomic[index].feedback ||
			!bytes.Equal(legacy.atomic[index].speaker, native.atomic[index].speaker) {
			t.Fatalf("DualSense atomic media frame %d changed or was reordered", index)
		}
	}
	for index := range legacy.realtime {
		if legacy.realtime[index] != native.realtime[index] {
			t.Fatalf("DualSense realtime haptics frame %d changed or was reordered", index)
		}
	}
}

func requireSynchronizedDualShock4Parity(
	t *testing.T, legacy, native dualShock4CaptureSnapshot,
) {
	t.Helper()
	if legacy.resets != native.resets || len(legacy.outputs) != len(native.outputs) ||
		len(legacy.speaker) != len(native.speaker) {
		t.Fatalf("DualShock 4 transport callbacks differ: legacy outputs=%d speaker=%d resets=%d; native outputs=%d speaker=%d resets=%d",
			len(legacy.outputs), len(legacy.speaker), legacy.resets,
			len(native.outputs), len(native.speaker), native.resets)
	}
	for index := range legacy.outputs {
		if legacy.outputs[index] != native.outputs[index] {
			t.Fatalf("DualShock 4 HID output %d changed across native transport", index)
		}
	}
	for index := range legacy.speaker {
		if !bytes.Equal(legacy.speaker[index], native.speaker[index]) {
			t.Fatalf("DualShock 4 speaker frame %d changed or was reordered", index)
		}
	}
}

func nativeSoakIsoOperation(meta udecx.Operation, payload []byte, packetCount int,
	packetLength uint32, input bool,
) udecx.Operation {
	op := udecx.Operation{
		Kind: udecx.OperationTransfer, TransferLength: uint32(packetCount) * packetLength,
		TransferFlags: udecx.TransferFlagStartIsoASAP,
		IsoPackets:    make([]udecx.IsoPacket, packetCount), Payload: append([]byte(nil), payload...),
	}
	copyNativeEndpointMetadata(&op, meta)
	for index := range op.IsoPackets {
		op.IsoPackets[index] = udecx.IsoPacket{
			Offset: uint32(index) * packetLength, Length: packetLength,
		}
	}
	if input {
		op.Direction = 1
		op.TransferFlags |= udecx.TransferFlagDirectionIn
	}
	return op
}

func submitAndWaitNativeSoakOperation(t *testing.T, driver *nativePlayStationSoakDriver,
	identity udecx.DeviceIdentity, op udecx.Operation, acknowledged bool,
) udecx.Completion {
	t.Helper()
	token, waiter := driver.submit(identity, op, acknowledged)
	if waiter == nil {
		return udecx.Completion{}
	}
	return driver.wait(t, token, waiter)
}

func setNativeSoakEndpointState(t *testing.T, driver *nativePlayStationSoakDriver,
	soakCase *nativePlayStationSoakCase, kind udecx.OperationKind,
) {
	t.Helper()
	for _, meta := range []udecx.Operation{
		soakCase.speakerMeta, soakCase.microphoneMeta, soakCase.hidInMeta, soakCase.hidOutMeta,
	} {
		op := udecx.Operation{Kind: kind}
		copyNativeEndpointMetadata(&op, meta)
		completion := submitAndWaitNativeSoakOperation(t, driver, soakCase.identity, op, true)
		if completion.Status != 0 || completion.USBDStatus != 0 {
			t.Fatalf("%s endpoint 0x%02x lifecycle %d failed: %+v",
				soakCase.name, op.EndpointAddress, kind, completion)
		}
	}
}

func primeNativeSoakMicrophone(soakCase *nativePlayStationSoakCase, seed int) {
	for frame := 0; frame < 6; frame++ {
		payload, _, _ := soakCase.makeMicrophone(seed + frame)
		soakCase.queueMicrophone(payload)
	}
}

func runNativePlayStationMediaPhase(t *testing.T, driver *nativePlayStationSoakDriver,
	cases []*nativePlayStationSoakCase, phase, cycles int,
) {
	t.Helper()
	for _, soakCase := range cases {
		primeNativeSoakMicrophone(soakCase, phase*1000)
		report := soakCase.makeHID()
		soakCase.legacyHID(report)
		op := udecx.Operation{Kind: udecx.OperationTransfer,
			TransferLength: uint32(len(report)), Payload: append([]byte(nil), report...)}
		copyNativeEndpointMetadata(&op, soakCase.hidOutMeta)
		completion := submitAndWaitNativeSoakOperation(t, driver, soakCase.identity, op, false)
		if completion.TransferLength != uint32(len(report)) || len(completion.Payload) != 0 {
			t.Fatalf("%s initial HID completion=%+v", soakCase.name, completion)
		}
	}

	start := make(chan struct{})
	errorsCh := make(chan error, len(cases)*3)
	var workers sync.WaitGroup
	for caseIndex, soakCase := range cases {
		caseIndex, soakCase := caseIndex, soakCase
		workers.Add(3)
		go func() {
			defer workers.Done()
			<-start
			for iteration := 0; iteration < cycles; iteration++ {
				absolute := phase*cycles + iteration + caseIndex*97
				payload, packetCount, packetLength := soakCase.makeSpeaker(absolute)
				soakCase.legacySpeaker(payload)
				op := nativeSoakIsoOperation(soakCase.speakerMeta, payload,
					packetCount, packetLength, false)
				token, waiter := driver.submit(soakCase.identity, op, false)
				completion, err := driver.waitFromWorker(token, waiter)
				if err != nil {
					errorsCh <- fmt.Errorf("%s speaker %d: %w", soakCase.name, iteration, err)
					return
				}
				if completion.TransferLength != uint32(len(payload)) ||
					len(completion.Payload) != 0 || len(completion.IsoPackets) != packetCount {
					errorsCh <- fmt.Errorf("%s speaker %d malformed completion: %+v",
						soakCase.name, iteration, completion)
					return
				}
			}
		}()
		go func() {
			defer workers.Done()
			<-start
			for iteration := 0; iteration < cycles; iteration++ {
				absolute := phase*cycles + iteration + caseIndex*131
				frame, packetCount, packetLength := soakCase.makeMicrophone(absolute + 6)
				soakCase.queueMicrophone(frame)
				packets := make([]udecx.IsoPacket, packetCount)
				for index := range packets {
					packets[index] = udecx.IsoPacket{Offset: uint32(index) * packetLength, Length: packetLength}
				}
				want := soakCase.oracleMicrophone(packets)
				op := nativeSoakIsoOperation(soakCase.microphoneMeta, nil,
					packetCount, packetLength, true)
				token, waiter := driver.submit(soakCase.identity, op, false)
				completion, err := driver.waitFromWorker(token, waiter)
				if err != nil {
					errorsCh <- fmt.Errorf("%s microphone %d: %w", soakCase.name, iteration, err)
					return
				}
				if completion.TransferLength != want.transferLength ||
					!bytes.Equal(completion.Payload, want.payload) ||
					!nativeSoakIsoPacketsEqual(completion.IsoPackets, want.packets) {
					errorsCh <- fmt.Errorf("%s microphone %d changed packet contract: got=%d/%v want=%d/%v; %s",
						soakCase.name, iteration, completion.TransferLength, completion.IsoPackets,
						want.transferLength, want.packets,
						nativeSoakByteDifference(completion.Payload, want.payload))
					return
				}
			}
		}()
		go func() {
			defer workers.Done()
			<-start
			report := soakCase.makeHID()
			for iteration := 0; iteration < cycles; iteration++ {
				soakCase.legacyHID(report)
				op := udecx.Operation{Kind: udecx.OperationTransfer,
					TransferLength: uint32(len(report)), Payload: append([]byte(nil), report...)}
				copyNativeEndpointMetadata(&op, soakCase.hidOutMeta)
				token, waiter := driver.submit(soakCase.identity, op, false)
				completion, err := driver.waitFromWorker(token, waiter)
				if err != nil {
					errorsCh <- fmt.Errorf("%s HID %d: %w", soakCase.name, iteration, err)
					return
				}
				if completion.TransferLength != uint32(len(report)) || len(completion.Payload) != 0 {
					errorsCh <- fmt.Errorf("%s HID %d malformed completion: %+v",
						soakCase.name, iteration, completion)
					return
				}
			}
		}()
	}
	close(start)
	workers.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	for _, soakCase := range cases {
		soakCase.requireParity(t)
	}
}

func requireNativeInputContinuity(t *testing.T, driver *nativePlayStationSoakDriver,
	soakCase *nativePlayStationSoakCase,
) {
	t.Helper()
	reports := driver.inputSnapshot(soakCase.identity)
	if len(reports) < 2 {
		t.Fatalf("%s published only %d native HID input reports", soakCase.name, len(reports))
	}
	for index, report := range reports {
		wantSequence := uint64(index + 1)
		if report.DeviceID != soakCase.identity.DeviceID ||
			report.Generation != soakCase.identity.Generation ||
			report.EndpointAddress != soakCase.hidInEP || report.Sequence != wantSequence ||
			len(report.Payload) != 64 {
			t.Fatalf("%s native input report %d=%+v len=%d want sequence=%d endpoint=0x%02x",
				soakCase.name, index, report, len(report.Payload), wantSequence, soakCase.hidInEP)
		}
	}
}

func waitForNativeInputMarker(t *testing.T, driver *nativePlayStationSoakDriver,
	soakCase *nativePlayStationSoakCase, marker int8,
) {
	t.Helper()
	deadline := time.Now().Add(nativePlayStationSoakTimeout)
	for time.Now().Before(deadline) {
		reports := driver.inputSnapshot(soakCase.identity)
		if len(reports) != 0 && soakCase.inputMarker(reports[len(reports)-1].Payload) == marker {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s never published post-lifecycle input marker %d", soakCase.name, marker)
}

func exerciseNativeD0Boundary(t *testing.T, driver *nativePlayStationSoakDriver,
	soakCase *nativePlayStationSoakCase,
) {
	t.Helper()
	exit := submitAndWaitNativeSoakOperation(t, driver, soakCase.identity,
		udecx.Operation{Kind: udecx.OperationDeviceD0Exit}, true)
	if exit.Status != 0 {
		t.Fatalf("%s D0 exit failed: %+v", soakCase.name, exit)
	}
	before := len(driver.inputSnapshot(soakCase.identity))
	soakCase.setInputMarker(63)
	time.Sleep(4 * time.Millisecond)
	after := len(driver.inputSnapshot(soakCase.identity))
	if after != before {
		t.Fatalf("%s published %d stale input reports after acknowledged D0 exit",
			soakCase.name, after-before)
	}
	entry := submitAndWaitNativeSoakOperation(t, driver, soakCase.identity,
		udecx.Operation{Kind: udecx.OperationDeviceD0Entry}, true)
	if entry.Status != 0 {
		t.Fatalf("%s D0 entry failed: %+v", soakCase.name, entry)
	}
	waitForNativeInputMarker(t, driver, soakCase, 63)
}

func exerciseNativeCancellationBoundary(t *testing.T, driver *nativePlayStationSoakDriver,
	gate *nativePlayStationCancelGate, soakCase *nativePlayStationSoakCase,
) {
	t.Helper()
	for _, meta := range []udecx.Operation{
		soakCase.speakerMeta, soakCase.microphoneMeta, soakCase.hidOutMeta,
	} {
		packetCount, packetLength := 10, uint32(0)
		var op udecx.Operation
		if meta.EndpointAddress == soakCase.speakerEP {
			payload, count, length := soakCase.makeSpeaker(0x513)
			op = nativeSoakIsoOperation(meta, payload, count, length, false)
		} else if meta.EndpointAddress == soakCase.microphoneEP {
			_, packetCount, packetLength = soakCase.makeMicrophone(0x517)
			op = nativeSoakIsoOperation(meta, nil, packetCount, packetLength, true)
		} else {
			payload := soakCase.makeHID()
			op = udecx.Operation{Kind: udecx.OperationTransfer,
				TransferLength: uint32(len(payload)), Payload: payload}
			copyNativeEndpointMetadata(&op, meta)
		}
		started, result := gate.arm(soakCase.identity, meta.EndpointAddress)
		token := driver.submitCancellable(soakCase.identity, op)
		select {
		case <-started:
		case <-time.After(nativePlayStationSoakTimeout):
			t.Fatalf("%s cancelled endpoint 0x%02x never reached native adapter gate",
				soakCase.name, meta.EndpointAddress)
		}
		driver.cancel(soakCase.identity, token, meta.EndpointAddress)
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s cancelled endpoint 0x%02x returned %v",
					soakCase.name, meta.EndpointAddress, err)
			}
		case <-time.After(nativePlayStationSoakTimeout):
			t.Fatalf("%s cancelled endpoint 0x%02x did not leave native adapter",
				soakCase.name, meta.EndpointAddress)
		}
	}

	// A valid state write on the same device proves both canceled endpoint
	// sequences retired and no stale completion or media callback blocked the
	// following generation. Callback parity proves the canceled speaker frame
	// itself was not published.
	report := soakCase.makeHID()
	soakCase.legacyHID(report)
	op := udecx.Operation{Kind: udecx.OperationTransfer,
		TransferLength: uint32(len(report)), Payload: append([]byte(nil), report...)}
	copyNativeEndpointMetadata(&op, soakCase.hidOutMeta)
	completion := submitAndWaitNativeSoakOperation(t, driver, soakCase.identity, op, false)
	if completion.TransferLength != uint32(len(report)) || len(completion.Payload) != 0 {
		t.Fatalf("%s post-cancel HID completion=%+v", soakCase.name, completion)
	}
	soakCase.requireParity(t)
}

func runNativePlayStationSoakSession(t *testing.T, session int) {
	t.Helper()
	driver := newNativePlayStationSoakDriver()
	processor, err := serverusb.NewNativeProcessor(
		serverusb.New(serverusb.ServerConfig{}, slog.Default(), nil))
	if err != nil {
		t.Fatal(err)
	}
	cancelGate := &nativePlayStationCancelGate{inner: processor}
	host, err := udecx.NewHost(driver, cancelGate, 8)
	if err != nil {
		t.Fatal(err)
	}
	cases := makeNativePlayStationSoakCases(t)
	for _, soakCase := range cases {
		identity, registerErr := host.Register(context.Background(), soakCase.identity.DeviceID,
			soakCase.native)
		if registerErr != nil {
			t.Fatal(registerErr)
		}
		soakCase.identity = identity
	}
	serveCtx, stopServe := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- host.Serve(serveCtx) }()

	for _, soakCase := range cases {
		soakCase.setLegacyAudioActive(true)
		setNativeSoakEndpointState(t, driver, soakCase, udecx.OperationEndpointStart)
		soakCase.setInputMarker(int8(10 + session))
		waitForNativeInputMarker(t, driver, soakCase, int8(10+session))
	}

	const phaseCycles = 12
	mediaStarted := time.Now()
	runNativePlayStationMediaPhase(t, driver, cases, 0, phaseCycles)
	for _, soakCase := range cases {
		exerciseNativeCancellationBoundary(t, driver, cancelGate, soakCase)
	}

	// Endpoint reset must retire the previous audio generation without losing
	// the selected interfaces or allowing stale PCM across the boundary.
	for _, soakCase := range cases {
		for _, endpoint := range []uint8{soakCase.speakerEP, soakCase.microphoneEP} {
			soakCase.resetLegacyEndpoint(endpoint)
			meta := soakCase.speakerMeta
			if endpoint == soakCase.microphoneEP {
				meta = soakCase.microphoneMeta
			}
			op := udecx.Operation{Kind: udecx.OperationEndpointReset}
			copyNativeEndpointMetadata(&op, meta)
			completion := submitAndWaitNativeSoakOperation(t, driver, soakCase.identity, op, true)
			if completion.Status != 0 {
				t.Fatalf("%s endpoint reset 0x%02x failed: %+v", soakCase.name, endpoint, completion)
			}
		}
		soakCase.requireParity(t)
		exerciseNativeD0Boundary(t, driver, soakCase)
	}
	runNativePlayStationMediaPhase(t, driver, cases, 1, phaseCycles)

	// A real device reset closes every selected audio interface. Re-open the
	// exact descriptors and prove fresh frames cannot inherit the old media or
	// microphone generation.
	for _, soakCase := range cases {
		soakCase.setLegacyAudioActive(false)
		completion := submitAndWaitNativeSoakOperation(t, driver, soakCase.identity,
			udecx.Operation{Kind: udecx.OperationDeviceReset}, true)
		if completion.Status != 0 {
			t.Fatalf("%s device reset failed: %+v", soakCase.name, completion)
		}
		soakCase.requireParity(t)
		soakCase.setLegacyAudioActive(true)
		setNativeSoakEndpointState(t, driver, soakCase, udecx.OperationEndpointStart)
	}
	runNativePlayStationMediaPhase(t, driver, cases, 2, phaseCycles)

	// Purge completion is the documented UdeCx boundary: all old forwarded I/O
	// is terminal before start. Exercise it after sustained duplex traffic and
	// require the new generation to continue with no stale or duplicate bytes.
	for _, soakCase := range cases {
		soakCase.setLegacyAudioActive(false)
		setNativeSoakEndpointState(t, driver, soakCase, udecx.OperationEndpointPurge)
		soakCase.requireParity(t)
		soakCase.setLegacyAudioActive(true)
		setNativeSoakEndpointState(t, driver, soakCase, udecx.OperationEndpointStart)
	}
	runNativePlayStationMediaPhase(t, driver, cases, 3, phaseCycles)
	mediaElapsed := time.Since(mediaStarted)
	minimumCadence := time.Duration(4*phaseCycles*9) * time.Millisecond
	if mediaElapsed < minimumCadence {
		t.Fatalf("native media soak collapsed USB service cadence: elapsed=%s minimum=%s",
			mediaElapsed, minimumCadence)
	}
	if mediaElapsed > 3*time.Second {
		t.Fatalf("native media soak exceeded continuity deadline: elapsed=%s", mediaElapsed)
	}

	for _, soakCase := range cases {
		requireNativeInputContinuity(t, driver, soakCase)
		soakCase.requireFinalCounts(t, 4*phaseCycles, 4*(phaseCycles+1)+1)
	}
	driver.requireClean(t)

	for _, soakCase := range cases {
		unregisterCtx, cancel := context.WithTimeout(context.Background(), nativePlayStationSoakTimeout)
		if err = host.Unregister(unregisterCtx, soakCase.identity); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
	}
	stopServe()
	select {
	case err = <-serveDone:
		if err != nil {
			t.Fatalf("native host session shutdown: %v", err)
		}
	case <-time.After(nativePlayStationSoakTimeout):
		t.Fatal("native host session did not stop after broker close")
	}

	// No publisher from the retired owner may survive into the next broker
	// session. The next outer iteration uses the same stable device IDs and must
	// begin again at input sequence one with fresh controller objects.
	for _, soakCase := range cases {
		before := len(driver.inputSnapshot(soakCase.identity))
		soakCase.setInputMarker(-51)
		time.Sleep(2 * time.Millisecond)
		if after := len(driver.inputSnapshot(soakCase.identity)); after != before {
			t.Fatalf("%s retired broker published %d zombie input reports",
				soakCase.name, after-before)
		}
	}
}

func TestNativePlayStationTransportZeroDropoutFaultSoak(t *testing.T) {
	// Four complete owner sessions cover reconnect and generation teardown while
	// carrying 192 ten-millisecond speaker/haptics and microphone intervals per
	// controller through resets, D0, purge/start, HID feedback, and fast input.
	// The established USB/IP engines are the content oracle throughout.
	for session := 0; session < 4; session++ {
		t.Run(fmt.Sprintf("broker_session_%d", session+1), func(t *testing.T) {
			runNativePlayStationSoakSession(t, session)
		})
	}
}

func TestNativePlayStationCancelledLifecycleDoesNotMutateController(t *testing.T) {
	processor, err := serverusb.NewNativeProcessor(
		serverusb.New(serverusb.ServerConfig{}, slog.Default(), nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, soakCase := range makeNativePlayStationSoakCases(t) {
		t.Run(soakCase.name, func(t *testing.T) {
			soakCase.identity.Generation = 1
			soakCase.setLegacyAudioActive(true)
			for _, meta := range []udecx.Operation{soakCase.speakerMeta, soakCase.microphoneMeta} {
				op := udecx.Operation{Kind: udecx.OperationEndpointStart,
					DeviceID: soakCase.identity.DeviceID, Generation: soakCase.identity.Generation}
				copyNativeEndpointMetadata(&op, meta)
				if lifecycleErr := processor.Lifecycle(context.Background(), soakCase.native, op); lifecycleErr != nil {
					t.Fatal(lifecycleErr)
				}
			}
			soakCase.requireParity(t)

			cancelledCtx, cancel := context.WithCancel(context.Background())
			cancel()
			reset := udecx.Operation{Kind: udecx.OperationEndpointReset,
				DeviceID: soakCase.identity.DeviceID, Generation: soakCase.identity.Generation}
			copyNativeEndpointMetadata(&reset, soakCase.speakerMeta)
			if lifecycleErr := processor.Lifecycle(cancelledCtx, soakCase.native, reset); !errors.Is(lifecycleErr, context.Canceled) {
				t.Fatalf("cancelled lifecycle returned %v", lifecycleErr)
			}
			soakCase.requireParity(t)

			soakCase.resetLegacyEndpoint(soakCase.speakerEP)
			if lifecycleErr := processor.Lifecycle(context.Background(), soakCase.native, reset); lifecycleErr != nil {
				t.Fatal(lifecycleErr)
			}
			soakCase.requireParity(t)
		})
	}
}

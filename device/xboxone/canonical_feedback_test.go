package xboxone

import (
	"errors"
	"reflect"
	"testing"

	"github.com/Alia5/VIIPER/controllerfeedback"
)

func canonicalFeedbackTestBinding() ControllerPersonaFeedbackBindingV1 {
	return ControllerPersonaFeedbackBindingV1{
		Source:                 controllerfeedback.SourceXboxSeriesVirtualDevice,
		PersonaGeneration:      5,
		DeviceGeneration:       0x1112131415161718,
		TransportGeneration:    0x2122232425262728,
		OwnershipEpoch:         0x3132333435363738,
		TimeToLiveMicroseconds: 250_000,
	}
}

func canonicalFeedbackMotorExecution() ControllerPersonaLocalExecution {
	return ControllerPersonaLocalExecution{
		Action:     ControllerPersonaApplyDirectMotor,
		Generation: 5,
		Order:      0x0102030405060708,
		DirectMotor: RumbleBodyV1{
			Enabled: MotorAll,
			// Wire order differs from CFBK semantic order. Distinct basis
			// values prove every channel crosses exactly once and sided.
			LeftImpulse: 75, RightImpulse: 100,
			LeftVibration: 25, RightVibration: 50,
			Duration: 25,
		},
	}
}

func TestControllerPersonaCanonicalFeedbackGoldenAndRoundTrip(t *testing.T) {
	binding := canonicalFeedbackTestBinding()
	execution := canonicalFeedbackMotorExecution()
	frame, err := ControllerPersonaCanonicalFeedbackFrame(binding, execution,
		0x4142434445464748,
		ControllerPersonaCanonicalFeedbackStateUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Command != controllerfeedback.CommandApply ||
		frame.BodyLow != 0x4000 || frame.BodyHigh != 0x8000 ||
		frame.LeftTrigger != 0xbfff || frame.RightTrigger != 0xffff ||
		frame.Sequence != execution.Order ||
		frame.DeviceGeneration != binding.DeviceGeneration ||
		frame.TransportGeneration != binding.TransportGeneration ||
		frame.OwnershipEpoch != binding.OwnershipEpoch ||
		frame.TimeToLiveMicroseconds != 250_000 {
		t.Fatalf("frame = %+v", frame)
	}

	want := [controllerfeedback.FrameSize]byte{
		0x43, 0x46, 0x42, 0x4b, 0x01, 0x00, 0x48, 0x00,
		0x02, 0x01, 0x0f, 0x00, 0x00, 0x40, 0x00, 0x80,
		0xff, 0xbf, 0xff, 0xff, 0x00, 0x00, 0x00, 0x00,
		0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01,
		0x18, 0x17, 0x16, 0x15, 0x14, 0x13, 0x12, 0x11,
		0x28, 0x27, 0x26, 0x25, 0x24, 0x23, 0x22, 0x21,
		0x38, 0x37, 0x36, 0x35, 0x34, 0x33, 0x32, 0x31,
		0x48, 0x47, 0x46, 0x45, 0x44, 0x43, 0x42, 0x41,
		0x90, 0xd0, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	got := [controllerfeedback.FrameSize]byte{}
	if err := EncodeControllerPersonaCanonicalFeedbackInto(got[:], binding,
		execution, 0x4142434445464748,
		ControllerPersonaCanonicalFeedbackStateUpdate); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("wire = % x, want % x", got, want)
	}
	var decoded controllerfeedback.Frame
	if err := decoded.UnmarshalFrom(got[:]); err != nil {
		t.Fatal(err)
	}
	if decoded != frame {
		t.Fatalf("decoded = %+v, want %+v", decoded, frame)
	}
}

func TestControllerPersonaCanonicalFeedbackChannelMaskAndPercentageBasis(
	t *testing.T,
) {
	binding := canonicalFeedbackTestBinding()
	tests := []struct {
		name string
		mask MotorMask
		body RumbleBodyV1
		want [4]uint16
	}{
		{"body low", MotorLeftVibration,
			RumbleBodyV1{LeftVibration: 1}, [4]uint16{655, 0, 0, 0}},
		{"body high", MotorRightVibration,
			RumbleBodyV1{RightVibration: 25}, [4]uint16{0, 16384, 0, 0}},
		{"left trigger", MotorLeftImpulse,
			RumbleBodyV1{LeftImpulse: 50}, [4]uint16{0, 0, 32768, 0}},
		{"right trigger", MotorRightImpulse,
			RumbleBodyV1{RightImpulse: 100}, [4]uint16{0, 0, 0, 65535}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.body.Enabled = test.mask
			test.body.Duration = 1
			execution := ControllerPersonaLocalExecution{
				Action: ControllerPersonaApplyDirectMotor, Generation: 5,
				Order: 1, DirectMotor: test.body,
			}
			frame, err := ControllerPersonaCanonicalFeedbackFrame(binding,
				execution, 1,
				ControllerPersonaCanonicalFeedbackStateUpdate)
			if err != nil {
				t.Fatal(err)
			}
			got := [4]uint16{frame.BodyLow, frame.BodyHigh,
				frame.LeftTrigger, frame.RightTrigger}
			if got != test.want || frame.TimeToLiveMicroseconds != 10_000 {
				t.Fatalf("channels = %v ttl=%d, want %v ttl=10000",
					got, frame.TimeToLiveMicroseconds, test.want)
			}
		})
	}

	// A valid level whose bitmap bit is clear is ignored by the semantic
	// snapshot, matching the official Direct Motor bitmap behavior.
	disabled := ControllerPersonaLocalExecution{
		Action: ControllerPersonaApplyDirectMotor, Generation: 5, Order: 2,
		DirectMotor: RumbleBodyV1{LeftImpulse: 100, Duration: 1},
	}
	frame, err := ControllerPersonaCanonicalFeedbackFrame(binding, disabled, 2,
		ControllerPersonaCanonicalFeedbackStateUpdate)
	if err != nil || frame.Command != controllerfeedback.CommandNeutral ||
		frame.LeftTrigger != 0 {
		t.Fatalf("disabled level = (%+v, %v)", frame, err)
	}
}

func TestControllerPersonaCanonicalFeedbackExhaustiveMaskAndLevelDomain(
	t *testing.T,
) {
	binding := canonicalFeedbackTestBinding()
	for mask := MotorMask(0); mask <= MotorAll; mask++ {
		for level := byte(0); level <= DirectMotorMaximumLevel; level++ {
			execution := ControllerPersonaLocalExecution{
				Action: ControllerPersonaApplyDirectMotor, Generation: 5,
				Order: uint64(mask)*101 + uint64(level) + 1,
				DirectMotor: RumbleBodyV1{
					Enabled: mask, LeftImpulse: level,
					RightImpulse: level, LeftVibration: level,
					RightVibration: level, Duration: 1,
				},
			}
			frame, err := ControllerPersonaCanonicalFeedbackFrame(binding,
				execution, 1,
				ControllerPersonaCanonicalFeedbackStateUpdate)
			if err != nil {
				t.Fatalf("mask=%02x level=%d: %v", mask, level, err)
			}
			want := uint16((uint32(level)*65535 + 50) / 100)
			channels := [...]struct {
				motor MotorMask
				got   uint16
			}{
				{MotorLeftVibration, frame.BodyLow},
				{MotorRightVibration, frame.BodyHigh},
				{MotorLeftImpulse, frame.LeftTrigger},
				{MotorRightImpulse, frame.RightTrigger},
			}
			hasAmplitude := false
			for _, channel := range channels {
				wantChannel := uint16(0)
				if mask&channel.motor != 0 {
					wantChannel = want
				}
				if channel.got != wantChannel {
					t.Fatalf("mask=%02x level=%d motor=%02x: got=%d want=%d",
						mask, level, channel.motor, channel.got, wantChannel)
				}
				hasAmplitude = hasAmplitude || wantChannel != 0
			}
			wantCommand := controllerfeedback.CommandNeutral
			if hasAmplitude {
				wantCommand = controllerfeedback.CommandApply
			}
			if frame.Command != wantCommand ||
				frame.TimeToLiveMicroseconds != 10_000 {
				t.Fatalf("mask=%02x level=%d command=%d ttl=%d",
					mask, level, frame.Command,
					frame.TimeToLiveMicroseconds)
			}
		}
	}
}

func TestControllerPersonaCanonicalFeedbackNeutralAndLifecycleStop(
	t *testing.T,
) {
	binding := canonicalFeedbackTestBinding()
	cancel := canonicalFeedbackMotorExecution()
	cancel.Order = 8
	cancel.DirectMotor = RumbleBodyV1{
		Enabled: MotorAll, LeftImpulse: 100, RightVibration: 100,
		Duration: 0, Delay: 7, Repeat: 9,
	}
	neutral, err := ControllerPersonaCanonicalFeedbackFrame(binding, cancel, 10,
		ControllerPersonaCanonicalFeedbackStateUpdate)
	if err != nil || neutral.Command != controllerfeedback.CommandNeutral ||
		neutral.BodyLow != 0 || neutral.BodyHigh != 0 ||
		neutral.LeftTrigger != 0 || neutral.RightTrigger != 0 {
		t.Fatalf("cancellation = (%+v, %v)", neutral, err)
	}

	clear := ControllerPersonaLocalExecution{
		Action: ControllerPersonaClearOutputs, Generation: 5, Order: 9,
		// ClearEpoch is the persona-local execution ledger. Deliberately make
		// it unrelated to the canonical DS4Windows ownership lease below.
		ClearEpoch: 0xa1a2a3a4a5a6a7a8,
	}
	recoverable, err := ControllerPersonaCanonicalFeedbackFrame(binding, clear,
		11, ControllerPersonaCanonicalFeedbackRecoverableNeutral)
	if err != nil || recoverable.Command != controllerfeedback.CommandNeutral ||
		recoverable.Sequence != 9 ||
		recoverable.OwnershipEpoch != binding.OwnershipEpoch {
		t.Fatalf("recoverable clear = (%+v, %v)", recoverable, err)
	}
	stop, err := ControllerPersonaCanonicalFeedbackFrame(binding, clear, 12,
		ControllerPersonaCanonicalFeedbackTerminalStop)
	if err != nil || stop.Command != controllerfeedback.CommandStop ||
		stop.Sequence != 9 || stop.OwnershipEpoch != binding.OwnershipEpoch {
		t.Fatalf("clear = (%+v, %v)", stop, err)
	}
	if stop.OwnershipEpoch == clear.ClearEpoch {
		t.Fatal("persona ClearEpoch leaked into canonical OwnershipEpoch")
	}
}

func TestControllerPersonaCanonicalFeedbackRequiresExplicitMatchingIntent(
	t *testing.T,
) {
	binding := canonicalFeedbackTestBinding()
	motor := canonicalFeedbackMotorExecution()
	clear := ControllerPersonaLocalExecution{
		Action: ControllerPersonaClearOutputs, Generation: 5, Order: 9,
		ClearEpoch: 1,
	}
	tests := []struct {
		name      string
		execution ControllerPersonaLocalExecution
		intent    ControllerPersonaCanonicalFeedbackIntent
	}{
		{"motor as recoverable clear", motor,
			ControllerPersonaCanonicalFeedbackRecoverableNeutral},
		{"motor as terminal stop", motor,
			ControllerPersonaCanonicalFeedbackTerminalStop},
		{"clear as state update", clear,
			ControllerPersonaCanonicalFeedbackStateUpdate},
		{"clear with invalid intent", clear,
			ControllerPersonaCanonicalFeedbackIntentInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want := [controllerfeedback.FrameSize]byte{}
			for index := range want {
				want[index] = 0xa5
			}
			got := want
			err := EncodeControllerPersonaCanonicalFeedbackInto(got[:],
				binding, test.execution, 1, test.intent)
			if !errors.Is(err, ErrInvalidCanonicalFeedbackIntent) {
				t.Fatalf("error = %v", err)
			}
			if got != want {
				t.Fatalf("destination changed: % x", got)
			}
		})
	}
}

func TestControllerPersonaCanonicalFeedbackFailsClosedAndIsAtomic(
	t *testing.T,
) {
	binding := canonicalFeedbackTestBinding()
	execution := canonicalFeedbackMotorExecution()
	tests := []struct {
		name    string
		binding ControllerPersonaFeedbackBindingV1
		action  ControllerPersonaLocalExecution
		want    error
	}{
		{"invalid source", func() ControllerPersonaFeedbackBindingV1 {
			b := binding
			b.Source = controllerfeedback.SourceXbox360VirtualDevice
			return b
		}(), execution, ErrInvalidCanonicalFeedbackBinding},
		{"zero device generation", func() ControllerPersonaFeedbackBindingV1 {
			b := binding
			b.DeviceGeneration = 0
			return b
		}(), execution, ErrInvalidCanonicalFeedbackBinding},
		{"zero ttl", func() ControllerPersonaFeedbackBindingV1 {
			b := binding
			b.TimeToLiveMicroseconds = 0
			return b
		}(), execution, ErrInvalidCanonicalFeedbackBinding},
		{"excessive ttl", func() ControllerPersonaFeedbackBindingV1 {
			b := binding
			b.TimeToLiveMicroseconds =
				controllerfeedback.MaxTimeToLiveMicroseconds + 1
			return b
		}(), execution, ErrInvalidCanonicalFeedbackBinding},
		{"zero sequence", binding, func() ControllerPersonaLocalExecution {
			e := execution
			e.Order = 0
			return e
		}(), ErrInvalidCanonicalFeedbackExecution},
		{"invalid motor level", binding, func() ControllerPersonaLocalExecution {
			e := execution
			e.DirectMotor.LeftImpulse = 101
			return e
		}(), ErrInvalidCanonicalFeedbackExecution},
		{"generation mismatch", binding, func() ControllerPersonaLocalExecution {
			e := execution
			e.Generation++
			return e
		}(), ErrInvalidCanonicalFeedbackBinding},
		{"timed repeat", binding, func() ControllerPersonaLocalExecution {
			e := execution
			e.DirectMotor.Repeat = 1
			return e
		}(), ErrUnsupportedCanonicalFeedbackTiming},
		{"timed delay", binding, func() ControllerPersonaLocalExecution {
			e := execution
			e.DirectMotor.Delay = 1
			return e
		}(), ErrUnsupportedCanonicalFeedbackTiming},
		{"guide led", binding, ControllerPersonaLocalExecution{
			Action: ControllerPersonaApplyGuideLED, Generation: 5, Order: 1,
			GuideLED: GuideLEDCommandV1{Pattern: GuideLEDPatternOn},
		}, ErrUnsupportedCanonicalFeedbackAction},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wantWire := [controllerfeedback.FrameSize]byte{}
			for index := range wantWire {
				wantWire[index] = 0xa5
			}
			gotWire := wantWire
			err := EncodeControllerPersonaCanonicalFeedbackInto(gotWire[:],
				test.binding, test.action, 1,
				ControllerPersonaCanonicalFeedbackStateUpdate)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if gotWire != wantWire {
				t.Fatalf("destination changed: % x", gotWire)
			}
		})
	}
	if err := EncodeControllerPersonaCanonicalFeedbackInto(
		make([]byte, controllerfeedback.FrameSize-1), binding, execution, 1,
		ControllerPersonaCanonicalFeedbackStateUpdate); !errors.Is(err, ErrInvalidLength) {
		t.Fatalf("short destination = %v", err)
	}
}

func TestControllerPersonaCanonicalFeedbackHotPathAllocatesZero(t *testing.T) {
	binding := canonicalFeedbackTestBinding()
	execution := canonicalFeedbackMotorExecution()
	var wire [controllerfeedback.FrameSize]byte
	if err := EncodeControllerPersonaCanonicalFeedbackInto(wire[:], binding,
		execution, 1,
		ControllerPersonaCanonicalFeedbackStateUpdate); err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := EncodeControllerPersonaCanonicalFeedbackInto(wire[:],
			binding, execution, 1,
			ControllerPersonaCanonicalFeedbackStateUpdate); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("allocations = %v, want 0", allocs)
	}
}

func TestDormantRetainedUSBAdapterSemanticWireIngressUsesExactExistingCell(
	t *testing.T,
) {
	var nilAdapter *DormantRetainedUSBAdapter
	if err := nilAdapter.PublishSemanticInputWire(1, 1, nil); !errors.Is(
		err, errDormantRetainedUSBUninitialized) {
		t.Fatalf("nil adapter = %v", err)
	}
	adapter, _, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	want := InputStateV1{
		Menu: true, B: true, DPadLeft: true, RightBumper: true,
		LeftTrigger: 321, RightTrigger: 654,
		LeftStickX: -1234, RightStickY: 2345,
	}
	var wire [SemanticInputWireSize]byte
	if err := EncodeSemanticInputWireV1Into(wire[:], want); err != nil {
		t.Fatal(err)
	}
	if err := adapter.PublishSemanticInputWire(41, 2, wire[:]); err != nil {
		t.Fatal(err)
	}
	adapter.mu.Lock()
	got, revision := adapter.input.State, adapter.inputRevision
	adapter.mu.Unlock()
	if revision != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("input = %+v revision=%d, want %+v revision=2",
			got, revision, want)
	}

	bad := wire
	bad[20] = 1
	if err := adapter.PublishSemanticInputWire(41, 3, bad[:]); !errors.Is(err, ErrInvalidSemanticInputContract) {
		t.Fatalf("reserved tail = %v", err)
	}
	adapter.mu.Lock()
	got, revision = adapter.input.State, adapter.inputRevision
	adapter.mu.Unlock()
	if revision != 2 || !reflect.DeepEqual(got, want) {
		t.Fatal("malformed wire mutated the existing semantic input cell")
	}

	guide := InputStateV1{Guide: true}
	if err := EncodeSemanticInputWireV1Into(wire[:], guide); err != nil {
		t.Fatal(err)
	}
	if err := adapter.PublishSemanticInputWire(41, 3, wire[:]); err != nil {
		t.Fatalf("Guide publish = %v", err)
	}
	adapter.mu.Lock()
	got, revision = adapter.input.State, adapter.inputRevision
	edges := adapter.guideEdgeCount
	adapter.mu.Unlock()
	if revision != 3 || got != guide || edges != 1 {
		t.Fatalf("Guide input = %+v revision=%d edges=%d", got, revision, edges)
	}

	if err := EncodeSemanticInputWireV1Into(
		wire[:], InputStateV1{Share: true}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.PublishSemanticInputWire(41, 4, wire[:]); !errors.Is(
		err, ErrShareRequiresExtension) {
		t.Fatalf("Share error = %v, want %v", err, ErrShareRequiresExtension)
	}
}

func TestDormantRetainedUSBAdapterSemanticWireIngressHotPathAllocatesZero(
	t *testing.T,
) {
	adapter, _, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	var wire [SemanticInputWireSize]byte
	if err := EncodeSemanticInputWireV1Into(wire[:], InputStateV1{A: true}); err != nil {
		t.Fatal(err)
	}
	revision := uint64(1)
	if allocs := testing.AllocsPerRun(1000, func() {
		revision++
		if err := adapter.PublishSemanticInputWire(41, revision, wire[:]); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("allocations = %v, want 0", allocs)
	}
}

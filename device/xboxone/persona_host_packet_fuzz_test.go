package xboxone

import (
	"bytes"
	"testing"
	"time"
)

// FuzzControllerPersonaDownstreamPacketBatch exercises the combined decoder,
// canonical selection, fixed-capacity action vector, final admission, and
// delivered resolution. Unsupported lifecycle combinations are valid
// fail-closed results; no fuzz case reaches hardware or production registry.
func FuzzControllerPersonaDownstreamPacketBatch(f *testing.F) {
	motor := RumbleBodyV1{
		Enabled: MotorLeftVibration, LeftVibration: 25, Duration: 2,
	}
	motorWire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(motorWire, 0x21, motor); err != nil {
		f.Fatal(err)
	}
	ledWire := make([]byte, GuideLEDCommandMessageSize)
	if err := EncodeGuideLEDCommandMessageInto(ledWire, 0x23,
		GuideLEDCommandV1{Pattern: GuideLEDPatternOn, Intensity: 12}); err != nil {
		f.Fatal(err)
	}
	start := testSetDeviceStateWire(0x22, SetDeviceStateStart)
	f.Add(bytes.Join([][]byte{motorWire, start, ledWire}, nil))
	f.Add(bytes.Join([][]byte{start,
		testSetDeviceStateWire(0x24, SetDeviceStateReset)}, nil))
	f.Add(motorWire)
	f.Add([]byte{0xff})

	f.Fuzz(func(t *testing.T, wire []byte) {
		if len(wire) == 0 || len(wire) > ControllerDownstreamPacketMaximumSize {
			return
		}
		packet, err := DecodeControllerDownstreamPacket(wire)
		if err != nil {
			return
		}
		engine := newTestControllerPersonaEngine(t, []byte{0xaa}, 0)
		configurePersonaUSB(t, engine, 0)
		hello, present, err := engine.ClaimPoll(0)
		if err != nil || !present {
			t.Fatalf("Hello = (%+v, %t, %v)", hello, present, err)
		}
		deliverPersonaClaim(t, engine, hello, 1)
		participant := &recordingDownstreamPacketParticipant{}
		owner, err := NewControllerPersonaDownstreamPacketBatchOwner(engine, participant)
		if err != nil {
			t.Fatal(err)
		}
		claim, err := owner.Claim(packet, 2)
		if err != nil {
			if _, executeCalls, cancelCalls := participant.counts(); executeCalls != 0 || cancelCalls != 0 {
				t.Fatalf("rejected packet executed: %d %d", executeCalls, cancelCalls)
			}
			return
		}
		lease, err := owner.Admit(claim, 3)
		if err != nil {
			// Admission is allowed to reject a clock/context fence. The claim
			// remains exact and can be deferred without an external effect.
			if resolveErr := owner.Resolve(
				claim, ControllerDownstreamPacketDeferred, 3); resolveErr != nil {
				t.Fatalf("admission=%v deferred resolution=%v", err, resolveErr)
			}
			return
		}
		for index := 0; index < lease.Len(); index++ {
			action, ok := lease.Action(index)
			if !ok || action.WireIndex != uint8(index) || !action.valid() {
				t.Fatalf("action %d = (%+v, %t)", index, action, ok)
			}
		}
		if err := owner.Execute(lease, time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := owner.Resolve(
			claim, ControllerDownstreamPacketDelivered, 4); err != nil {
			t.Fatal(err)
		}
	})
}

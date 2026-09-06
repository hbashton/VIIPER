package xboxone

import (
	"errors"
	"testing"
	"time"
)

// FuzzControllerDownstreamPacketExecution keeps the execution owner downstream
// of the exact decoder. Decoder rejection exposes no prefix; successful
// classification either becomes one ordered Direct Motor/Guide LED vector or
// fails closed before participant preflight.
func FuzzControllerDownstreamPacketExecution(f *testing.F) {
	direct := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(direct, 0x31, RumbleBodyV1{
		Enabled:       MotorLeftVibration | MotorRightImpulse,
		LeftVibration: 71, RightImpulse: 29, Duration: 4,
	}); err != nil {
		f.Fatal(err)
	}
	led := make([]byte, GuideLEDCommandMessageSize)
	if err := EncodeGuideLEDCommandMessageInto(led, 0x32,
		GuideLEDCommandV1{Pattern: GuideLEDPatternOn, Intensity: 47}); err != nil {
		f.Fatal(err)
	}
	f.Add(append(append([]byte(nil), direct...), led...))
	f.Add(direct)
	f.Add([]byte{0x04, 0x20, 0x01, 0x00})
	f.Add([]byte{})
	f.Add(make([]byte, ControllerDownstreamPacketMaximumSize+1))

	f.Fuzz(func(t *testing.T, wire []byte) {
		packet, decodeErr := DecodeControllerDownstreamPacket(wire)
		if decodeErr != nil {
			if packet.Len() != 0 {
				t.Fatalf("failed decode exposed %d messages", packet.Len())
			}
			return
		}

		supported := true
		for index := 0; index < packet.Len(); index++ {
			message, ok := packet.Message(index)
			if !ok {
				t.Fatalf("decoded packet lost message %d", index)
			}
			if message.Kind != ControllerDownstreamDirectMotor &&
				message.Kind != ControllerDownstreamGuideLED {
				supported = false
			}
		}

		participant := &recordingDownstreamPacketParticipant{}
		owner, err := NewControllerDownstreamPacketExecutionOwner(participant)
		if err != nil {
			t.Fatal(err)
		}
		claim, claimErr := owner.Claim(packet)
		if !supported {
			if !errors.Is(claimErr, ErrControllerDownstreamPacketNonAtomicAction) {
				t.Fatalf("non-atomic packet error = %v", claimErr)
			}
			snapshot, _ := owner.Snapshot()
			preflights, executions, cancellations := participant.counts()
			if !snapshot.Idle || snapshot.NextPacketEpoch != 0 ||
				snapshot.NextClaimToken != 0 || preflights != 0 ||
				executions != 0 || cancellations != 0 {
				t.Fatalf("non-atomic packet escaped preflight: snapshot=%+v calls=%d/%d/%d",
					snapshot, preflights, executions, cancellations)
			}
			return
		}
		if claimErr != nil {
			t.Fatalf("typed packet rejected: %v; wire=% x", claimErr, wire)
		}
		lease, err := owner.Admit(claim)
		if err != nil {
			t.Fatal(err)
		}
		if lease.Len() != packet.Len() {
			t.Fatalf("execution length = %d, want %d", lease.Len(), packet.Len())
		}
		for index := 0; index < lease.Len(); index++ {
			action, ok := lease.Action(index)
			message, messageOK := packet.Message(index)
			if !ok || !messageOK || action.WireIndex != uint8(index) ||
				action.Sequence != message.Sequence {
				t.Fatalf("order %d action=(%+v,%t) message=(%+v,%t)",
					index, action, ok, message, messageOK)
			}
			switch message.Kind {
			case ControllerDownstreamDirectMotor:
				if action.Action != ControllerPersonaApplyDirectMotor ||
					action.DirectMotor != message.DirectMotor {
					t.Fatalf("direct action %d = %+v", index, action)
				}
			case ControllerDownstreamGuideLED:
				if action.Action != ControllerPersonaApplyGuideLED ||
					action.GuideLED != message.GuideLED {
					t.Fatalf("LED action %d = %+v", index, action)
				}
			}
		}
		if err := owner.Execute(lease, time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := owner.Resolve(claim, ControllerDownstreamPacketDelivered); err != nil {
			t.Fatal(err)
		}
		snapshot, _ := owner.Snapshot()
		if !snapshot.Idle || snapshot.Quarantined || snapshot.RetryPending {
			t.Fatalf("successful terminal snapshot = %+v", snapshot)
		}
	})
}

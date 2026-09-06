package xboxone

import (
	"errors"
	"testing"
)

func FuzzControllerDownstreamReceiveFramingGate(f *testing.F) {
	direct := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(
		direct, 0x71, testDirectMotorBody(),
	); err != nil {
		f.Fatalf("encode direct motor seed: %v", err)
	}
	led := make([]byte, GuideLEDCommandMessageSize)
	if err := EncodeGuideLEDCommandMessageInto(
		led, 0x72,
		GuideLEDCommandV1{Pattern: GuideLEDPatternRampToLevel, Intensity: 47},
	); err != nil {
		f.Fatalf("encode Guide LED seed: %v", err)
	}
	f.Add(direct)
	f.Add(led)
	f.Add(append(append([]byte(nil), direct...), led...))
	f.Add([]byte{0x04,
		flagFragment | flagInitFragment | flagSystem | flagAcknowledge,
		0x73, 0x00})
	f.Add([]byte{0x09, 0x00, 0x74, lengthExtended, 0x00})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, source []byte) {
		// Fuzz input is mutable transport scratch. The gate may inspect it only
		// during Claim and may never retain the slice.
		wire := append([]byte(nil), source...)
		gate, err := NewDormantControllerDownstreamReceiveFramingGate(101, 202)
		if err != nil {
			t.Fatalf("new gate: %v", err)
		}
		claim, blocker, claimErr := gate.ClaimExactCompletePacket(wire)
		expected, decodeErr := DecodeControllerDownstreamPacket(wire)

		if decodeErr == nil {
			if claimErr != nil || blocker != ControllerDownstreamReceiveNotBlocked ||
				!claim.Valid() {
				t.Fatalf("successful decode claim=%+v blocker=%d err=%v",
					claim, blocker, claimErr)
			}
			clear(wire)
			packet, takeErr := gate.TakeExactPacket(claim)
			if takeErr != nil || packet != expected {
				t.Fatalf("take packet=%+v want=%+v err=%v",
					packet, expected, takeErr)
			}
		} else {
			if claim.Valid() || claimErr == nil ||
				blocker == ControllerDownstreamReceiveNotBlocked {
				t.Fatalf("rejected decode claim=%+v blocker=%d err=%v decode=%v",
					claim, blocker, claimErr, decodeErr)
			}
			wantBlocker := classifyControllerDownstreamReceiveError(decodeErr)
			if blocker != wantBlocker {
				t.Fatalf("blocker=%d want=%d decode=%v",
					blocker, wantBlocker, decodeErr)
			}
			clear(wire)
			if snapshot := gate.Snapshot(); snapshot.State != ControllerDownstreamReceiveGateBlocked ||
				snapshot.Blocker != wantBlocker || snapshot.MessageCount != 0 {
				t.Fatalf("blocked snapshot = %+v", snapshot)
			}
		}

		second, secondBlocker, secondErr := gate.ClaimExactCompletePacket(source)
		if second.Valid() ||
			secondBlocker != ControllerDownstreamReceiveReplayUnprovable ||
			!errors.Is(secondErr,
				ErrControllerDownstreamReceiveReplayUnprovable) {
			t.Fatalf("second claim=%+v blocker=%d err=%v",
				second, secondBlocker, secondErr)
		}
	})
}

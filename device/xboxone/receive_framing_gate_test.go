package xboxone

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func receiveFramingDirectWire(t testing.TB, sequence uint8) []byte {
	t.Helper()
	wire := make([]byte, DirectMotorMessageSize)
	if err := EncodeDirectMotorMessageInto(
		wire, sequence, testDirectMotorBody(),
	); err != nil {
		t.Fatalf("encode direct motor: %v", err)
	}
	return wire
}

func receiveFramingLEDWire(t testing.TB, sequence uint8) []byte {
	t.Helper()
	wire := make([]byte, GuideLEDCommandMessageSize)
	if err := EncodeGuideLEDCommandMessageInto(
		wire, sequence,
		GuideLEDCommandV1{Pattern: GuideLEDPatternOn, Intensity: 31},
	); err != nil {
		t.Fatalf("encode Guide LED: %v", err)
	}
	return wire
}

func newReceiveFramingGate(t testing.TB, generation, epoch uint64) *DormantControllerDownstreamReceiveFramingGate {
	t.Helper()
	gate, err := NewDormantControllerDownstreamReceiveFramingGate(
		generation, epoch)
	if err != nil {
		t.Fatalf("new receive framing gate: %v", err)
	}
	return gate
}

func TestControllerDownstreamReceiveFramingGateExactOneShotHandoff(t *testing.T) {
	gate := newReceiveFramingGate(t, 7, 19)
	wire := receiveFramingDirectWire(t, 0x42)

	claim, blocker, err := gate.ClaimExactCompletePacket(wire)
	if err != nil || blocker != ControllerDownstreamReceiveNotBlocked ||
		!claim.Valid() {
		t.Fatalf("claim = %+v blocker=%d err=%v", claim, blocker, err)
	}
	if claim.Generation() != 7 || claim.Epoch() != 19 ||
		claim.MessageCount() != 1 {
		t.Fatalf("claim identity = generation=%d epoch=%d count=%d",
			claim.Generation(), claim.Epoch(), claim.MessageCount())
	}

	// The source aliases transport scratch. Mutation after Claim must not alter
	// the typed value retained by the gate.
	clear(wire)
	packet, err := gate.TakeExactPacket(claim)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	message, ok := packet.Message(0)
	if !ok || message.Kind != ControllerDownstreamDirectMotor ||
		message.Sequence != 0x42 || message.DirectMotor != testDirectMotorBody() {
		t.Fatalf("taken message = %+v present=%t", message, ok)
	}

	snapshot := gate.Snapshot()
	if snapshot.Generation != 7 || snapshot.Epoch != 19 ||
		snapshot.State != ControllerDownstreamReceiveGateTaken ||
		snapshot.Blocker != ControllerDownstreamReceiveNotBlocked ||
		snapshot.MessageCount != 1 || snapshot.Quarantined {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if _, err := gate.TakeExactPacket(claim); !errors.Is(
		err, ErrControllerDownstreamReceiveCredential,
	) {
		t.Fatalf("copied take error = %v", err)
	}
	if next, gotBlocker, err := gate.ClaimExactCompletePacket(
		receiveFramingDirectWire(t, 0x43),
	); next.Valid() || gotBlocker != ControllerDownstreamReceiveReplayUnprovable ||
		!errors.Is(err, ErrControllerDownstreamReceiveReplayUnprovable) {
		t.Fatalf("second claim = %+v blocker=%d err=%v", next, gotBlocker, err)
	}
}

func TestControllerDownstreamReceiveFramingGatePreservesCoalescedOrderAndSequence(t *testing.T) {
	direct := receiveFramingDirectWire(t, 0xfe)
	led := receiveFramingLEDWire(t, 0xff)
	wire := append(append([]byte(nil), direct...), led...)
	gate := newReceiveFramingGate(t, 3, 5)

	claim, blocker, err := gate.ClaimExactCompletePacket(wire)
	if err != nil || blocker != ControllerDownstreamReceiveNotBlocked ||
		claim.MessageCount() != 2 {
		t.Fatalf("claim blocker=%d count=%d err=%v",
			blocker, claim.MessageCount(), err)
	}
	clear(wire)
	packet, err := gate.TakeExactPacket(claim)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	first, firstOK := packet.Message(0)
	second, secondOK := packet.Message(1)
	if !firstOK || !secondOK ||
		first.Kind != ControllerDownstreamDirectMotor || first.Sequence != 0xfe ||
		second.Kind != ControllerDownstreamGuideLED || second.Sequence != 0xff {
		t.Fatalf("messages = first=%+v/%t second=%+v/%t",
			first, firstOK, second, secondOK)
	}
}

func TestControllerDownstreamReceiveFramingGateTypedFailureAtomicBlockers(t *testing.T) {
	validPrefix := receiveFramingDirectWire(t, 0x21)
	fragment := []byte{0x04,
		flagFragment | flagInitFragment | flagSystem | flagAcknowledge,
		0x22, 0x00}
	fragmentAfterPrefix := append(append([]byte(nil), validPrefix...), fragment...)
	extended := []byte{0x09, 0x00, 0x23, lengthExtended, 0x00}

	tests := []struct {
		name        string
		wire        []byte
		wantBlocker ControllerDownstreamReceiveBlocker
		wantError   error
	}{
		{name: "reliable fragment", wire: fragment,
			wantBlocker: ControllerDownstreamReceiveReliableFragment,
			wantError:   ErrFragmentedHeader},
		{name: "fragment after valid prefix retains no prefix", wire: fragmentAfterPrefix,
			wantBlocker: ControllerDownstreamReceiveReliableFragment,
			wantError:   ErrFragmentedHeader},
		{name: "extended framing", wire: extended,
			wantBlocker: ControllerDownstreamReceiveExtendedFraming,
			wantError:   ErrExtendedPayloadLength},
		{name: "unsupported complete family", wire: []byte{0x0b, 0x00, 0x24, 0x00},
			wantBlocker: ControllerDownstreamReceiveUnsupportedMessage,
			wantError:   ErrUnsupportedControllerPersonaHostMessage},
		{name: "InitFrag without Fragment", wire: []byte{0x09, flagInitFragment, 0x25, 0x00},
			wantBlocker: ControllerDownstreamReceiveMalformed,
			wantError:   ErrInvalidInitFragment},
		{name: "truncated", wire: []byte{0x09, 0x00, 0x26, 0x09, 0x00},
			wantBlocker: ControllerDownstreamReceiveMalformed,
			wantError:   ErrInvalidLength},
		{name: "empty", wire: nil,
			wantBlocker: ControllerDownstreamReceiveMalformed,
			wantError:   ErrInvalidLength},
		{name: "oversized", wire: make([]byte, ControllerDownstreamPacketMaximumSize+1),
			wantBlocker: ControllerDownstreamReceiveMalformed,
			wantError:   ErrInvalidLength},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gate := newReceiveFramingGate(t, 11, uint64(index+1))
			claim, blocker, err := gate.ClaimExactCompletePacket(test.wire)
			if claim.Valid() || blocker != test.wantBlocker ||
				!errors.Is(err, test.wantError) {
				t.Fatalf("claim=%+v blocker=%d err=%v", claim, blocker, err)
			}
			if len(test.wire) != 0 {
				clear(test.wire)
			}
			snapshot := gate.Snapshot()
			if snapshot.State != ControllerDownstreamReceiveGateBlocked ||
				snapshot.Blocker != test.wantBlocker ||
				snapshot.MessageCount != 0 || snapshot.Quarantined {
				t.Fatalf("snapshot = %+v", snapshot)
			}
			second, secondBlocker, secondErr :=
				gate.ClaimExactCompletePacket(validPrefix)
			if second.Valid() ||
				secondBlocker != ControllerDownstreamReceiveReplayUnprovable ||
				!errors.Is(secondErr,
					ErrControllerDownstreamReceiveReplayUnprovable) {
				t.Fatalf("second=%+v blocker=%d err=%v",
					second, secondBlocker, secondErr)
			}
		})
	}
}

func TestControllerDownstreamReceiveFramingGateInvalidCredentialQuarantinesExactLifetime(t *testing.T) {
	first := newReceiveFramingGate(t, 4, 1)
	second := newReceiveFramingGate(t, 4, 2)
	firstClaim, _, err := first.ClaimExactCompletePacket(
		receiveFramingDirectWire(t, 0x31))
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	secondClaim, _, err := second.ClaimExactCompletePacket(
		receiveFramingDirectWire(t, 0x32))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}

	if packet, err := second.TakeExactPacket(firstClaim); packet.Len() != 0 ||
		!errors.Is(err, ErrControllerDownstreamReceiveCredential) {
		t.Fatalf("foreign take packet=%+v err=%v", packet, err)
	}
	snapshot := second.Snapshot()
	if snapshot.State != ControllerDownstreamReceiveGateQuarantined ||
		snapshot.Blocker != ControllerDownstreamReceiveCredentialContradiction ||
		!snapshot.Quarantined || snapshot.MessageCount != 0 {
		t.Fatalf("quarantined snapshot = %+v", snapshot)
	}
	if _, err := second.TakeExactPacket(secondClaim); !errors.Is(
		err, ErrControllerDownstreamReceiveCredential,
	) {
		t.Fatalf("take after quarantine error = %v", err)
	}
	if packet, err := first.TakeExactPacket(firstClaim); err != nil ||
		packet.Len() != 1 {
		t.Fatalf("unrelated exact owner packet=%+v err=%v", packet, err)
	}
}

func TestControllerDownstreamReceiveFramingGateConstructorAndZeroValueFailClosed(t *testing.T) {
	for _, identity := range [][2]uint64{{0, 1}, {1, 0}, {0, 0}} {
		if gate, err := NewDormantControllerDownstreamReceiveFramingGate(
			identity[0], identity[1],
		); gate != nil || !errors.Is(err, ErrControllerDownstreamReceiveIdentity) {
			t.Fatalf("identity=%v gate=%p err=%v", identity, gate, err)
		}
	}
	var zero DormantControllerDownstreamReceiveFramingGate
	claim, blocker, err := zero.ClaimExactCompletePacket(
		receiveFramingDirectWire(t, 1))
	if claim.Valid() ||
		blocker != ControllerDownstreamReceiveCredentialContradiction ||
		!errors.Is(err, ErrControllerDownstreamReceiveIdentity) {
		t.Fatalf("zero claim=%+v blocker=%d err=%v", claim, blocker, err)
	}
	if got := (*DormantControllerDownstreamReceiveFramingGate)(nil).Snapshot(); got != (ControllerDownstreamReceiveFramingSnapshot{}) {
		t.Fatalf("nil snapshot = %+v", got)
	}
}

func TestControllerDownstreamReceiveFramingGateConcurrentClaimAndTakeAreAtMostOnce(t *testing.T) {
	const contenders = 64
	gate := newReceiveFramingGate(t, 20, 30)
	wire := receiveFramingDirectWire(t, 0x40)
	start := make(chan struct{})
	claims := make(chan ControllerDownstreamReceiveFramingClaim, contenders)
	var accepted atomic.Int32
	var replayBlocked atomic.Int32
	var unexpected atomic.Int32
	var group sync.WaitGroup

	for index := 0; index < contenders; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			claim, blocker, err := gate.ClaimExactCompletePacket(wire)
			switch {
			case err == nil && blocker == ControllerDownstreamReceiveNotBlocked && claim.Valid():
				accepted.Add(1)
				claims <- claim
			case errors.Is(err, ErrControllerDownstreamReceiveReplayUnprovable) &&
				blocker == ControllerDownstreamReceiveReplayUnprovable && !claim.Valid():
				replayBlocked.Add(1)
			default:
				unexpected.Add(1)
			}
		}()
	}
	close(start)
	group.Wait()
	close(claims)
	if accepted.Load() != 1 || replayBlocked.Load() != contenders-1 ||
		unexpected.Load() != 0 {
		t.Fatalf("accepted=%d replay=%d unexpected=%d",
			accepted.Load(), replayBlocked.Load(), unexpected.Load())
	}
	winner := <-claims

	startTake := make(chan struct{})
	var took atomic.Int32
	var rejected atomic.Int32
	for index := 0; index < contenders; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-startTake
			packet, err := gate.TakeExactPacket(winner)
			if err == nil && packet.Len() == 1 {
				took.Add(1)
			} else if errors.Is(err, ErrControllerDownstreamReceiveCredential) &&
				packet.Len() == 0 {
				rejected.Add(1)
			} else {
				unexpected.Add(1)
			}
		}()
	}
	close(startTake)
	group.Wait()
	if took.Load() != 1 || rejected.Load() != contenders-1 ||
		unexpected.Load() != 0 {
		t.Fatalf("took=%d rejected=%d unexpected=%d",
			took.Load(), rejected.Load(), unexpected.Load())
	}
}

func TestControllerDownstreamReceiveFramingGateConcurrentSnapshotNeverPublishesPrefix(t *testing.T) {
	gate := newReceiveFramingGate(t, 50, 60)
	wire := receiveFramingDirectWire(t, 0x51)
	stop := make(chan struct{})
	var invalid atomic.Int32
	var group sync.WaitGroup
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snapshot := gate.Snapshot()
				switch snapshot.State {
				case ControllerDownstreamReceiveGateFresh,
					ControllerDownstreamReceiveGateClaiming:
					if snapshot.MessageCount != 0 ||
						snapshot.Blocker != ControllerDownstreamReceiveNotBlocked {
						invalid.Add(1)
					}
				case ControllerDownstreamReceiveGateAccepted,
					ControllerDownstreamReceiveGateTaken:
					if snapshot.MessageCount != 1 ||
						snapshot.Blocker != ControllerDownstreamReceiveNotBlocked {
						invalid.Add(1)
					}
				default:
					invalid.Add(1)
				}
			}
		}()
	}
	claim, blocker, err := gate.ClaimExactCompletePacket(wire)
	if err != nil || blocker != ControllerDownstreamReceiveNotBlocked {
		t.Fatalf("claim blocker=%d err=%v", blocker, err)
	}
	if _, err := gate.TakeExactPacket(claim); err != nil {
		t.Fatalf("take: %v", err)
	}
	close(stop)
	group.Wait()
	if invalid.Load() != 0 {
		t.Fatalf("invalid snapshots = %d", invalid.Load())
	}
}

func TestControllerDownstreamReceiveFramingGateWarmPathAllocatesZero(t *testing.T) {
	wire := receiveFramingDirectWire(t, 0x61)
	gate := &DormantControllerDownstreamReceiveFramingGate{
		generation: 80,
		epoch:      90,
	}
	allocations := testing.AllocsPerRun(1000, func() {
		// Reset only this test-owned, non-concurrent gate so AllocsPerRun can
		// measure Claim+Take without including one-time construction.
		gate.packet = ControllerDownstreamPacket{}
		gate.messageCount = 0
		gate.blocker.Store(0)
		gate.state.Store(uint32(ControllerDownstreamReceiveGateFresh))
		claim, blocker, err := gate.ClaimExactCompletePacket(wire)
		if err != nil || blocker != ControllerDownstreamReceiveNotBlocked {
			panic("unexpected framing claim result")
		}
		packet, err := gate.TakeExactPacket(claim)
		if err != nil || packet.Len() != 1 {
			panic("unexpected framing take result")
		}
	})
	if allocations != 0 {
		t.Fatalf("warm Claim+Take allocations = %f", allocations)
	}

	fragment := [4]byte{0x04,
		flagFragment | flagInitFragment | flagSystem | flagAcknowledge,
		0x62, 0x00}
	allocations = testing.AllocsPerRun(1000, func() {
		gate.packet = ControllerDownstreamPacket{}
		gate.messageCount = 0
		gate.blocker.Store(0)
		gate.state.Store(uint32(ControllerDownstreamReceiveGateFresh))
		claim, blocker, err := gate.ClaimExactCompletePacket(fragment[:])
		if claim.Valid() || blocker != ControllerDownstreamReceiveReliableFragment ||
			!errors.Is(err, ErrFragmentedHeader) {
			panic("unexpected fragment blocker")
		}
	})
	if allocations != 0 {
		t.Fatalf("fragment blocker allocations = %f", allocations)
	}

	extended := [5]byte{0x09, 0x00, 0x63, lengthExtended, 0x00}
	allocations = testing.AllocsPerRun(1000, func() {
		gate.packet = ControllerDownstreamPacket{}
		gate.messageCount = 0
		gate.blocker.Store(0)
		gate.state.Store(uint32(ControllerDownstreamReceiveGateFresh))
		claim, blocker, err := gate.ClaimExactCompletePacket(extended[:])
		if claim.Valid() || blocker != ControllerDownstreamReceiveExtendedFraming ||
			!errors.Is(err, ErrExtendedPayloadLength) {
			panic("unexpected extended blocker")
		}
	})
	if allocations != 0 {
		t.Fatalf("extended blocker allocations = %f", allocations)
	}
}

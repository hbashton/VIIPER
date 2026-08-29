package controllerfeedback

import (
	"sync/atomic"
	"testing"
)

func TestMailboxRejectsStaleFencesAndRequiresNewEpochForSourceChange(t *testing.T) {
	var mailbox Mailbox
	first := testFrame()
	first.Sequence = 10
	if !mailbox.Publish(first) {
		t.Fatal("initial publish failed")
	}
	stale := first
	stale.Sequence = 9
	if mailbox.Publish(stale) {
		t.Fatal("stale sequence was accepted")
	}
	differentSource := first
	differentSource.Sequence = 11
	differentSource.Source = SourceDualSenseVirtualDevice
	if mailbox.Publish(differentSource) {
		t.Fatal("source changed without a new ownership epoch")
	}
	differentSource.Sequence = 1
	differentSource.OwnershipEpoch = 2
	if !mailbox.Publish(differentSource) {
		t.Fatal("new ownership epoch was rejected")
	}
	oldEpoch := first
	oldEpoch.Sequence = 99
	if mailbox.Publish(oldEpoch) {
		t.Fatal("old ownership epoch was accepted")
	}
	newDevice := first
	newDevice.Sequence = 1
	newDevice.DeviceGeneration = 2
	if !mailbox.Publish(newDevice) {
		t.Fatal("new device generation was rejected")
	}

	latest, revision, ok := mailbox.ReadLatest()
	if !ok || latest.DeviceGeneration != 2 || revision != 3 {
		t.Fatalf("latest=%+v revision=%d ok=%t", latest, revision, ok)
	}
}

func TestMailboxGenerationOrderingIsLexicographic(t *testing.T) {
	var mailbox Mailbox
	current := testFrame()
	current.DeviceGeneration = 5
	current.TransportGeneration = 7
	current.OwnershipEpoch = 11
	current.Sequence = 13
	if !mailbox.Publish(current) {
		t.Fatal("initial publish failed")
	}

	newTransport := current
	newTransport.TransportGeneration++
	newTransport.OwnershipEpoch = 1
	newTransport.Sequence = 1
	if !mailbox.Publish(newTransport) {
		t.Fatal("new transport generation could not reset subordinate fences")
	}
	oldTransport := current
	oldTransport.OwnershipEpoch = ^uint64(0)
	oldTransport.Sequence = ^uint64(0)
	if mailbox.Publish(oldTransport) {
		t.Fatal("old transport generation overrode its superior fence")
	}
	newDevice := current
	newDevice.DeviceGeneration++
	newDevice.TransportGeneration = 1
	newDevice.OwnershipEpoch = 1
	newDevice.Sequence = 1
	if !mailbox.Publish(newDevice) {
		t.Fatal("new device generation could not reset subordinate fences")
	}
}

func TestMailboxStopIsTerminalUntilReplacementOwnershipEpoch(t *testing.T) {
	var mailbox Mailbox
	apply := testFrame()
	if !mailbox.Publish(apply) {
		t.Fatal("apply failed")
	}
	stop := apply
	stop.Command = CommandStop
	stop.BodyLow = 0
	stop.BodyHigh = 0
	stop.LeftTrigger = 0
	stop.RightTrigger = 0
	stop.Sequence++
	if !mailbox.Publish(stop) {
		t.Fatal("stop failed")
	}
	resurrect := apply
	resurrect.Sequence = stop.Sequence + 1
	if mailbox.Publish(resurrect) {
		t.Fatal("stopped ownership epoch was resurrected")
	}
	resurrect.Sequence = 1
	resurrect.OwnershipEpoch++
	if !mailbox.Publish(resurrect) {
		t.Fatal("replacement ownership epoch was rejected")
	}
}

func TestMailboxClaimEmitsOneReleaseWhenAppliedRevisionExpires(t *testing.T) {
	var mailbox Mailbox
	var cursor ClaimCursor
	first := testFrame()
	first.TimestampMicroseconds = 1_000
	first.TimeToLiveMicroseconds = 50
	if !mailbox.Publish(first) {
		t.Fatal("initial publish failed")
	}
	claimed, disposition := mailbox.Claim(1_049, &cursor)
	if disposition != ClaimFrame || claimed.Sequence != 1 ||
		cursor.Revision != 1 || cursor.ReleaseRevision != 0 {
		t.Fatalf("claim=%+v cursor=%+v disposition=%d", claimed, cursor, disposition)
	}
	if duplicate, disposition := mailbox.Claim(1_049, &cursor); disposition != ClaimNone || duplicate != (Frame{}) {
		t.Fatal("same revision was claimed twice")
	}
	if released, disposition := mailbox.Claim(1_050, &cursor); disposition != ClaimRelease || released != (Frame{}) ||
		cursor.Revision != 1 || cursor.ReleaseRevision != 1 {
		t.Fatalf("expiry release=%+v cursor=%+v disposition=%d",
			released, cursor, disposition)
	}
	if duplicate, disposition := mailbox.Claim(2_000, &cursor); disposition != ClaimNone || duplicate != (Frame{}) {
		t.Fatal("same expiry released twice")
	}

	expired := first
	expired.Sequence = 2
	expired.TimestampMicroseconds = 1_050
	if !mailbox.Publish(expired) {
		t.Fatal("second publish failed")
	}
	if got, disposition := mailbox.Claim(1_100, &cursor); disposition != ClaimRelease || got != (Frame{}) ||
		cursor.Revision != 2 || cursor.ReleaseRevision != 2 {
		t.Fatalf("expired claim=%+v cursor=%+v disposition=%d", got, cursor, disposition)
	}
	if got, revision, ok := mailbox.ReadFresh(1_100); ok || got != (Frame{}) || revision != 2 {
		t.Fatalf("expired read=%+v revision=%d ok=%t", got, revision, ok)
	}
	latest, revision, ok := mailbox.ReadLatest()
	if !ok || latest != expired || revision != 2 {
		t.Fatal("expired ordering watermark was not retained")
	}

	recovered := expired
	recovered.Sequence = 3
	recovered.TimestampMicroseconds = 1_100
	if !mailbox.Publish(recovered) {
		t.Fatal("fresh successor failed")
	}
	if got, disposition := mailbox.Claim(1_100, &cursor); disposition != ClaimFrame || got != recovered || cursor.Revision != 3 {
		t.Fatalf("recovery claim=%+v cursor=%+v disposition=%d", got, cursor, disposition)
	}
}

func TestMailboxClaimRejectsFarFutureFrameWithOneRelease(t *testing.T) {
	var mailbox Mailbox
	var cursor ClaimCursor
	frame := testFrame()
	frame.TimestampMicroseconds = 10_001
	if !mailbox.Publish(frame) {
		t.Fatal("publish failed")
	}
	if got, disposition := mailbox.Claim(5_000, &cursor); disposition != ClaimRelease || got != (Frame{}) {
		t.Fatalf("far-future claim=%+v disposition=%d", got, disposition)
	}
	if got, disposition := mailbox.Claim(5_000, &cursor); disposition != ClaimNone || got != (Frame{}) {
		t.Fatal("far-future revision released more than once")
	}
}

func TestMailboxConcurrentPublicationNeverExposesHybridSnapshot(t *testing.T) {
	const finalSequence = uint64(30_000)
	var mailbox Mailbox
	if !mailbox.Publish(patternFrame(1)) {
		t.Fatal("initial publish failed")
	}
	var done atomic.Bool
	var publishFailed atomic.Bool
	go func() {
		for sequence := uint64(2); sequence <= finalSequence; sequence++ {
			if !mailbox.Publish(patternFrame(sequence)) {
				publishFailed.Store(true)
				break
			}
		}
		done.Store(true)
	}()

	for !done.Load() {
		observed, _, ok := mailbox.ReadLatest()
		if !ok {
			t.Fatal("published mailbox became empty")
		}
		assertPattern(t, observed)
	}
	if publishFailed.Load() {
		t.Fatal("ordered writer publication failed")
	}
	final, _, ok := mailbox.ReadLatest()
	if !ok || final.Sequence != finalSequence {
		t.Fatalf("final=%+v ok=%t", final, ok)
	}
	assertPattern(t, final)
}

func TestMailboxAndCodecHotPathDoesNotAllocate(t *testing.T) {
	var mailbox Mailbox
	var cursor ClaimCursor
	var sequence uint64
	var encoded [FrameSize]byte
	var decoded Frame
	allocations := testing.AllocsPerRun(1_000, func() {
		sequence++
		frame := patternFrame(sequence)
		if err := frame.MarshalTo(encoded[:]); err != nil {
			panic(err)
		}
		if err := decoded.UnmarshalFrom(encoded[:]); err != nil {
			panic(err)
		}
		if !mailbox.Publish(decoded) {
			panic("publish failed")
		}
		if _, disposition := mailbox.Claim(frame.TimestampMicroseconds,
			&cursor); disposition != ClaimFrame {
			panic("claim failed")
		}
		if _, _, ok := mailbox.ReadLatest(); !ok {
			panic("read failed")
		}
	})
	if allocations != 0 {
		t.Fatalf("feedback hot path allocated %.2f times per cycle", allocations)
	}
}

func TestNilMailboxAndNilClaimRevisionFailClosed(t *testing.T) {
	var mailbox *Mailbox
	if mailbox.Publish(testFrame()) {
		t.Fatal("nil mailbox published")
	}
	if got, revision, ok := mailbox.ReadLatest(); ok || revision != 0 || got != (Frame{}) {
		t.Fatal("nil mailbox returned latest state")
	}
	if got, revision, ok := mailbox.ReadFresh(0); ok || revision != 0 || got != (Frame{}) {
		t.Fatal("nil mailbox returned fresh state")
	}
	if got, disposition := mailbox.Claim(0, nil); disposition != ClaimNone || got != (Frame{}) {
		t.Fatal("nil mailbox returned claimed state")
	}
	var live Mailbox
	if !live.Publish(testFrame()) {
		t.Fatal("live mailbox publish failed")
	}
	if got, disposition := live.Claim(1_000, nil); disposition != ClaimNone || got != (Frame{}) {
		t.Fatal("nil claim revision returned state")
	}
}

func patternFrame(sequence uint64) Frame {
	frame := testFrame()
	frame.BodyLow = pattern(sequence, 1)
	frame.BodyHigh = pattern(sequence, 2)
	frame.LeftTrigger = pattern(sequence, 3)
	frame.RightTrigger = pattern(sequence, 4)
	frame.Sequence = sequence
	frame.TimestampMicroseconds = 100_000 + sequence
	return frame
}

func assertPattern(t *testing.T, frame Frame) {
	t.Helper()
	if frame.BodyLow != pattern(frame.Sequence, 1) ||
		frame.BodyHigh != pattern(frame.Sequence, 2) ||
		frame.LeftTrigger != pattern(frame.Sequence, 3) ||
		frame.RightTrigger != pattern(frame.Sequence, 4) ||
		frame.TimestampMicroseconds != 100_000+frame.Sequence ||
		frame.Actuators != ActuatorAll {
		t.Fatalf("hybrid frame observed: %+v", frame)
	}
}

func pattern(sequence uint64, salt uint16) uint16 {
	return uint16((sequence*uint64(salt*257)+uint64(salt))%
		uint64(^uint16(0)) + 1)
}

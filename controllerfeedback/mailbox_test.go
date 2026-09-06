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

func TestMailboxClaimAdvancesOnlyAfterSuccessfulCompletionAndRetriesFailure(t *testing.T) {
	var mailbox Mailbox
	var cursor ClaimCursor
	first := testFrame()
	first.TimestampMicroseconds = 1_000
	first.TimeToLiveMicroseconds = 50
	if !mailbox.Publish(first) {
		t.Fatal("initial publish failed")
	}
	claimed, disposition, firstToken := mailbox.Claim(1_049, &cursor)
	if disposition != ClaimFrame || claimed.Sequence != 1 ||
		firstToken == 0 || cursor.AppliedRevision != 0 ||
		cursor.ReleasedRevision != 0 {
		t.Fatalf("claim=%+v cursor=%+v disposition=%d", claimed, cursor, disposition)
	}
	if duplicate, disposition, token := mailbox.Claim(1_049, &cursor); disposition != ClaimNone || duplicate != (Frame{}) || token != 0 {
		t.Fatal("an in-flight claim did not block a second claim")
	}
	var wrongMailbox Mailbox
	if !wrongMailbox.Publish(testFrame()) {
		t.Fatal("wrong-mailbox setup publish failed")
	}
	if wrongMailbox.Complete(&cursor, firstToken, true) {
		t.Fatal("another mailbox completed the claim")
	}
	if _, disposition, _ := wrongMailbox.Claim(1_000, &cursor); disposition != ClaimNone {
		t.Fatal("cursor was silently reused by another mailbox")
	}
	var wrongMailboxCursor ClaimCursor
	if _, disposition, token := wrongMailbox.Claim(1_000, &wrongMailboxCursor); disposition != ClaimFrame ||
		token == 0 || !wrongMailbox.Complete(&wrongMailboxCursor, token, true) {
		t.Fatal("fresh cursor could not claim from second mailbox")
	}
	if mailbox.Complete(&cursor, firstToken+1, true) {
		t.Fatal("a stale token disturbed the in-flight claim")
	}
	if _, disposition, _ := mailbox.Claim(1_049, &cursor); disposition != ClaimNone {
		t.Fatal("an invalid completion cleared the in-flight claim")
	}
	if !mailbox.Complete(&cursor, firstToken, false) ||
		cursor.AppliedRevision != 0 {
		t.Fatal("failed delivery advanced the application watermark")
	}
	retried, disposition, retryToken := mailbox.Claim(1_049, &cursor)
	if disposition != ClaimFrame || retried != first ||
		retryToken == 0 || retryToken == firstToken {
		t.Fatalf("retry=%+v disposition=%d token=%d", retried, disposition, retryToken)
	}
	if mailbox.Complete(&cursor, firstToken, true) {
		t.Fatal("the first token completed an active retry claim")
	}
	if _, disposition, _ := mailbox.Claim(1_049, &cursor); disposition != ClaimNone {
		t.Fatal("a stale token disturbed the active retry claim")
	}
	if !mailbox.Complete(&cursor, retryToken, true) ||
		cursor.AppliedRevision != 1 || cursor.ReleasedRevision != 0 {
		t.Fatalf("successful frame completion did not advance cursor: %+v", cursor)
	}
	if mailbox.Complete(&cursor, retryToken, true) {
		t.Fatal("duplicate completion was accepted")
	}
	if duplicate, disposition, token := mailbox.Claim(1_049, &cursor); disposition != ClaimNone || duplicate != (Frame{}) || token != 0 {
		t.Fatal("completed frame was claimed twice")
	}

	released, disposition, releaseToken := mailbox.Claim(1_050, &cursor)
	if disposition != ClaimRelease || released != (Frame{}) || releaseToken == 0 {
		t.Fatalf("expiry release=%+v disposition=%d token=%d",
			released, disposition, releaseToken)
	}
	if !mailbox.Complete(&cursor, releaseToken, false) ||
		cursor.ReleasedRevision != 0 {
		t.Fatal("failed release advanced the release watermark")
	}
	_, disposition, releaseRetryToken := mailbox.Claim(1_050, &cursor)
	if disposition != ClaimRelease || releaseRetryToken == 0 ||
		releaseRetryToken == releaseToken {
		t.Fatal("failed release was not retried with a new token")
	}
	if !mailbox.Complete(&cursor, releaseRetryToken, true) ||
		cursor.ReleasedRevision != 1 {
		t.Fatalf("release completion did not advance cursor: %+v", cursor)
	}
	if duplicate, disposition, token := mailbox.Claim(2_000, &cursor); disposition != ClaimNone || duplicate != (Frame{}) || token != 0 {
		t.Fatal("completed expiry released twice")
	}

	expired := first
	expired.Sequence = 2
	expired.TimestampMicroseconds = 1_050
	if !mailbox.Publish(expired) {
		t.Fatal("second publish failed")
	}
	if got, disposition, token := mailbox.Claim(1_100, &cursor); disposition != ClaimRelease || got != (Frame{}) ||
		token == 0 || !mailbox.Complete(&cursor, token, true) ||
		cursor.AppliedRevision != 2 || cursor.ReleasedRevision != 2 {
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
	if got, disposition, token := mailbox.Claim(1_100, &cursor); disposition != ClaimFrame || got != recovered ||
		token == 0 || !mailbox.Complete(&cursor, token, true) ||
		cursor.AppliedRevision != 3 {
		t.Fatalf("recovery claim=%+v cursor=%+v disposition=%d", got, cursor, disposition)
	}
}

func TestMailboxBindsAnEmptyCursorPermanently(t *testing.T) {
	var empty Mailbox
	var other Mailbox
	var cursor ClaimCursor
	if got, disposition, token := empty.Claim(1_000, &cursor); disposition != ClaimNone || got != (Frame{}) || token != 0 {
		t.Fatal("empty mailbox returned a claim")
	}
	if cursor.ownerMailbox != &empty || cursor.self != &cursor {
		t.Fatal("empty mailbox did not bind cursor identity")
	}
	if !other.Publish(testFrame()) {
		t.Fatal("other mailbox setup failed")
	}
	if _, disposition, _ := other.Claim(1_000, &cursor); disposition != ClaimNone {
		t.Fatal("bound cursor moved to another mailbox")
	}
	var otherCursor ClaimCursor
	if _, disposition, token := other.Claim(1_000, &otherCursor); disposition != ClaimFrame || token == 0 ||
		!other.Complete(&otherCursor, token, true) {
		t.Fatal("fresh cursor could not complete other mailbox claim")
	}
	if _, disposition, _ := empty.Claim(1_000, &otherCursor); disposition != ClaimNone {
		t.Fatal("completed cursor changed mailbox ownership")
	}
}

func TestMailboxNewerPublicationSupersedesInFlightRevision(t *testing.T) {
	first := testFrame()
	second := first
	second.Sequence = 2

	t.Run("successful older delivery", func(t *testing.T) {
		var mailbox Mailbox
		var cursor ClaimCursor
		if !mailbox.Publish(first) {
			t.Fatal("first publish failed")
		}
		_, disposition, firstToken := mailbox.Claim(1_000, &cursor)
		if disposition != ClaimFrame || firstToken == 0 ||
			!mailbox.Publish(second) ||
			!mailbox.Complete(&cursor, firstToken, true) {
			t.Fatal("older delivery setup failed")
		}
		if cursor.AppliedRevision != 1 {
			t.Fatalf("older completion=%+v", cursor)
		}
		newest, disposition, newestToken := mailbox.Claim(1_000, &cursor)
		if disposition != ClaimFrame || newest != second || newestToken == 0 ||
			!mailbox.Complete(&cursor, newestToken, true) ||
			cursor.AppliedRevision != 2 {
			t.Fatalf("newest=%+v disposition=%d cursor=%+v", newest,
				disposition, cursor)
		}
	})

	t.Run("failed older delivery", func(t *testing.T) {
		var mailbox Mailbox
		var cursor ClaimCursor
		if !mailbox.Publish(first) {
			t.Fatal("first publish failed")
		}
		_, disposition, firstToken := mailbox.Claim(1_000, &cursor)
		if disposition != ClaimFrame || firstToken == 0 ||
			!mailbox.Publish(second) ||
			!mailbox.Complete(&cursor, firstToken, false) {
			t.Fatal("failed older delivery setup failed")
		}
		newest, disposition, newestToken := mailbox.Claim(1_000, &cursor)
		if disposition != ClaimFrame || newest != second || newestToken == 0 ||
			!mailbox.Complete(&cursor, newestToken, true) {
			t.Fatalf("newest=%+v disposition=%d cursor=%+v", newest,
				disposition, cursor)
		}
	})
}

func TestMailboxPreAdmissionRevalidationPreventsStaleActuation(t *testing.T) {
	apply := testFrame()
	apply.TimestampMicroseconds = 1_000
	apply.TimeToLiveMicroseconds = 50
	stop := apply
	stop.Command = CommandStop
	stop.BodyLow = 0
	stop.BodyHigh = 0
	stop.LeftTrigger = 0
	stop.RightTrigger = 0
	stop.Sequence = 2
	stop.TimestampMicroseconds = 1_001

	t.Run("already admitted apply is followed by stop", func(t *testing.T) {
		var mailbox Mailbox
		var cursor ClaimCursor
		if !mailbox.Publish(apply) {
			t.Fatal("apply publish failed")
		}
		_, disposition, applyToken := mailbox.Claim(1_000, &cursor)
		if disposition != ClaimFrame ||
			!mailbox.CanDeliver(&cursor, applyToken, 1_000) ||
			!mailbox.Publish(stop) ||
			!mailbox.Complete(&cursor, applyToken, true) {
			t.Fatal("already-admitted setup failed")
		}
		claimedStop, disposition, stopToken := mailbox.Claim(1_001, &cursor)
		if disposition != ClaimFrame || claimedStop.Command != CommandStop ||
			!mailbox.CanDeliver(&cursor, stopToken, 1_001) ||
			!mailbox.Complete(&cursor, stopToken, true) {
			t.Fatal("stop did not follow admitted apply")
		}
		if _, disposition, _ := mailbox.Claim(1_051, &cursor); disposition != ClaimNone {
			t.Fatal("completed stop produced duplicate expiry release")
		}
	})

	t.Run("new stop invalidates unadmitted apply", func(t *testing.T) {
		var mailbox Mailbox
		var cursor ClaimCursor
		if !mailbox.Publish(apply) {
			t.Fatal("apply publish failed")
		}
		_, disposition, applyToken := mailbox.Claim(1_000, &cursor)
		if disposition != ClaimFrame || !mailbox.Publish(stop) {
			t.Fatal("stop setup failed")
		}
		if mailbox.CanDeliver(&cursor, applyToken, 1_001) {
			t.Fatal("superseded apply remained eligible for admission")
		}
		if !mailbox.Complete(&cursor, applyToken, false) {
			t.Fatal("stale apply cleanup failed")
		}
		claimedStop, disposition, stopToken := mailbox.Claim(1_001, &cursor)
		if disposition != ClaimFrame || claimedStop.Command != CommandStop ||
			!mailbox.CanDeliver(&cursor, stopToken, 1_001) ||
			!mailbox.Complete(&cursor, stopToken, true) {
			t.Fatal("stop was not delivered directly")
		}
	})

	t.Run("expired apply becomes one release", func(t *testing.T) {
		var mailbox Mailbox
		var cursor ClaimCursor
		if !mailbox.Publish(apply) {
			t.Fatal("apply publish failed")
		}
		_, disposition, applyToken := mailbox.Claim(1_049, &cursor)
		if disposition != ClaimFrame {
			t.Fatal("apply claim failed")
		}
		if mailbox.CanDeliver(&cursor, applyToken, 1_050) {
			t.Fatal("expired apply remained eligible for admission")
		}
		if !mailbox.Complete(&cursor, applyToken, false) {
			t.Fatal("expired apply cleanup failed")
		}
		_, disposition, releaseToken := mailbox.Claim(1_050, &cursor)
		if disposition != ClaimRelease ||
			!mailbox.CanDeliver(&cursor, releaseToken, 1_050) ||
			!mailbox.Complete(&cursor, releaseToken, true) {
			t.Fatal("expired apply did not become release")
		}
		if _, disposition, _ := mailbox.Claim(2_000, &cursor); disposition != ClaimNone {
			t.Fatal("expiry release was delivered more than once")
		}
	})
}

func TestMailboxPreAdmissionRevalidationRejectsInvalidClaimIdentity(t *testing.T) {
	var mailbox Mailbox
	var other Mailbox
	var cursor ClaimCursor
	if !mailbox.Publish(testFrame()) {
		t.Fatal("publish failed")
	}
	_, disposition, token := mailbox.Claim(1_000, &cursor)
	if disposition != ClaimFrame || token == 0 {
		t.Fatal("claim failed")
	}

	copyOfCursor := cursor
	checks := []struct {
		name    string
		mailbox *Mailbox
		cursor  *ClaimCursor
		token   uint64
	}{
		{name: "zero token", mailbox: &mailbox, cursor: &cursor},
		{name: "wrong token", mailbox: &mailbox, cursor: &cursor, token: token + 1},
		{name: "wrong mailbox", mailbox: &other, cursor: &cursor, token: token},
		{name: "copied cursor", mailbox: &mailbox, cursor: &copyOfCursor, token: token},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if check.mailbox.CanDeliver(check.cursor, check.token, 1_000) {
				t.Fatal("invalid claim identity was admitted")
			}
		})
	}

	if !mailbox.CanDeliver(&cursor, token, 1_000) ||
		!mailbox.Complete(&cursor, token, false) {
		t.Fatal("invalid checks disturbed the original claim")
	}
	_, disposition, retryToken := mailbox.Claim(1_000, &cursor)
	if disposition != ClaimFrame || retryToken == 0 || retryToken == token {
		t.Fatal("retry claim failed")
	}
	if mailbox.CanDeliver(&cursor, token, 1_000) {
		t.Fatal("stale token admitted retry claim")
	}
	if !mailbox.CanDeliver(&cursor, retryToken, 1_000) ||
		!mailbox.Complete(&cursor, retryToken, true) {
		t.Fatal("valid retry claim was not admitted")
	}
	if mailbox.CanDeliver(&cursor, retryToken, 1_000) {
		t.Fatal("completed token remained admissible")
	}
}

func TestMailboxTokenWrapAndBoundCursorCopyFailClosed(t *testing.T) {
	var mailbox Mailbox
	var cursor ClaimCursor
	cursor.nextToken = ^uint64(0)
	if !mailbox.Publish(testFrame()) {
		t.Fatal("publish failed")
	}
	_, disposition, token := mailbox.Claim(1_000, &cursor)
	if disposition != ClaimFrame || token != 1 {
		t.Fatalf("wrapped token=%d disposition=%d", token, disposition)
	}
	copyOfCursor := cursor
	if mailbox.Complete(&copyOfCursor, token, true) {
		t.Fatal("copied cursor completed original claim")
	}
	if _, copyDisposition, copyToken := mailbox.Claim(1_000, &copyOfCursor); copyDisposition != ClaimNone || copyToken != 0 {
		t.Fatal("copied cursor claimed from its apparent mailbox")
	}
	if !mailbox.Complete(&cursor, token, true) {
		t.Fatal("copied cursor disturbed original claim")
	}
}

func TestMailboxFailedDeliveryDeferMakesClaimRetryable(t *testing.T) {
	var mailbox Mailbox
	var cursor ClaimCursor
	if !mailbox.Publish(testFrame()) {
		t.Fatal("publish failed")
	}
	var firstToken uint64
	func() {
		delivered := false
		_, disposition, token := mailbox.Claim(1_000, &cursor)
		firstToken = token
		if disposition != ClaimFrame || token == 0 {
			t.Fatal("claim failed")
		}
		defer func() {
			if !mailbox.Complete(&cursor, token, delivered) {
				t.Fatal("deferred failed-delivery completion failed")
			}
		}()
		// A transport write returns or panics before delivered becomes true.
	}()
	_, disposition, retryToken := mailbox.Claim(1_000, &cursor)
	if disposition != ClaimFrame || retryToken == 0 ||
		retryToken == firstToken ||
		!mailbox.Complete(&cursor, retryToken, true) {
		t.Fatal("deferred cleanup did not make the claim retryable")
	}
}

func TestMailboxCompletedStopSuppressesExpiryReleaseButNeutralDoesNot(t *testing.T) {
	stop := testFrame()
	stop.Command = CommandStop
	stop.BodyLow = 0
	stop.BodyHigh = 0
	stop.LeftTrigger = 0
	stop.RightTrigger = 0
	stop.TimestampMicroseconds = 1_000
	stop.TimeToLiveMicroseconds = 50
	var stopMailbox Mailbox
	var stopCursor ClaimCursor
	if !stopMailbox.Publish(stop) {
		t.Fatal("stop publish failed")
	}
	claimed, disposition, token := stopMailbox.Claim(1_000, &stopCursor)
	if disposition != ClaimFrame || claimed != stop || token == 0 ||
		!stopMailbox.Complete(&stopCursor, token, true) {
		t.Fatal("stop completion failed")
	}
	if stopCursor.AppliedRevision != 1 || stopCursor.ReleasedRevision != 1 {
		t.Fatalf("stop did not complete both watermarks: %+v", stopCursor)
	}
	if _, disposition, _ := stopMailbox.Claim(1_050, &stopCursor); disposition != ClaimNone {
		t.Fatal("completed stop produced a redundant expiry release")
	}

	neutral := stop
	neutral.Command = CommandNeutral
	var neutralMailbox Mailbox
	var neutralCursor ClaimCursor
	if !neutralMailbox.Publish(neutral) {
		t.Fatal("neutral publish failed")
	}
	_, disposition, token = neutralMailbox.Claim(1_000, &neutralCursor)
	if disposition != ClaimFrame || token == 0 ||
		!neutralMailbox.Complete(&neutralCursor, token, true) {
		t.Fatal("neutral completion failed")
	}
	if neutralCursor.AppliedRevision != 1 || neutralCursor.ReleasedRevision != 0 {
		t.Fatalf("neutral incorrectly retired the lease: %+v", neutralCursor)
	}
	_, disposition, token = neutralMailbox.Claim(1_050, &neutralCursor)
	if disposition != ClaimRelease || token == 0 ||
		!neutralMailbox.Complete(&neutralCursor, token, true) {
		t.Fatal("neutral lease did not release on expiry")
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
	if got, disposition, token := mailbox.Claim(5_000, &cursor); disposition != ClaimRelease || got != (Frame{}) ||
		token == 0 || !mailbox.Complete(&cursor, token, true) {
		t.Fatalf("far-future claim=%+v disposition=%d", got, disposition)
	}
	if got, disposition, token := mailbox.Claim(5_000, &cursor); disposition != ClaimNone || got != (Frame{}) || token != 0 {
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
		if _, disposition, token := mailbox.Claim(frame.TimestampMicroseconds,
			&cursor); disposition != ClaimFrame || token == 0 ||
			!mailbox.Complete(&cursor, token, true) {
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
	if got, disposition, token := mailbox.Claim(0, nil); disposition != ClaimNone || got != (Frame{}) || token != 0 {
		t.Fatal("nil mailbox returned claimed state")
	}
	if mailbox.Complete(nil, 1, true) {
		t.Fatal("nil mailbox completed a claim")
	}
	var live Mailbox
	if !live.Publish(testFrame()) {
		t.Fatal("live mailbox publish failed")
	}
	if got, disposition, token := live.Claim(1_000, nil); disposition != ClaimNone || got != (Frame{}) || token != 0 {
		t.Fatal("nil claim revision returned state")
	}
	if live.Complete(nil, 1, true) {
		t.Fatal("nil cursor completed a claim")
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

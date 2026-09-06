package xboxone

import (
	"errors"
	"testing"
	"time"
)

func activateFeedbackColdOwnerTestEngine(t *testing.T, engine *ControllerPersonaEngine) {
	t.Helper()
	configurePersonaUSB(t, engine, 0)
	hello, present, err := engine.ClaimPoll(0)
	if err != nil || !present {
		t.Fatalf("Hello = (%+v, %t, %v)", hello, present, err)
	}
	deliverPersonaClaim(t, engine, hello, 1)
	start := claimPersonaHostAction(t, engine, 2, testSetDeviceStateWire(2, SetDeviceStateStart))
	deliverPersonaClaim(t, engine, start, 3)
	for _, now := range []uint64{4, 5} {
		claim, err := engine.ClaimNextLifecycleAction(now)
		if err != nil {
			t.Fatal(err)
		}
		deliverPersonaClaim(t, engine, claim, now)
	}
}

func detachFeedbackColdOwnerTestClaim(t *testing.T, engine *ControllerPersonaEngine, retry bool) ControllerPersonaClaim {
	t.Helper()
	claim := claimPersonaHostAction(t, engine, 10, testDirectMotorWire(t, 0x71, testDirectMotorBody()))
	if err := engine.AdmitAndCopy(claim, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := engine.detachOrdinaryFeedback(claim); err != nil {
		t.Fatal(err)
	}
	if retry {
		if err := engine.completeOrdinaryFeedback(claim, ControllerPersonaDeferred, 11); err != nil {
			t.Fatal(err)
		}
	}
	if engine.hasClaim || engine.retryPending || !engine.ordinaryFeedbackPending() {
		t.Fatalf("fixture did not isolate side ownership: %+v", engine.Snapshot())
	}
	return claim
}

func deliverFeedbackColdOwnerTestClaim(t *testing.T, engine *ControllerPersonaEngine, claim ControllerPersonaClaim, retry bool) {
	t.Helper()
	if retry {
		var present bool
		var err error
		claim, present, err = engine.admitOrdinaryFeedbackRetry(12)
		if err != nil || !present {
			t.Fatalf("feedback retry admission = (%+v, %t, %v)", claim, present, err)
		}
	}
	if err := engine.completeOrdinaryFeedback(claim, ControllerPersonaDelivered, 13); err != nil {
		t.Fatal(err)
	}
}

func TestPersonaFeedbackColdOwnerBatchConstructorRejectsSideClaimAndRetry(t *testing.T) {
	for _, retry := range []bool{false, true} {
		name := "admitted"
		if retry {
			name = "retry"
		}
		t.Run(name, func(t *testing.T) {
			engine, _ := makePersonaActive(t, []byte{1})
			claim := detachFeedbackColdOwnerTestClaim(t, engine, retry)
			before := engine.Snapshot()
			participant := &recordingDownstreamPacketParticipant{}
			owner, err := NewControllerPersonaDownstreamPacketBatchOwner(engine, participant)
			if owner != nil || !errors.Is(err, ErrControllerPersonaHostPacketOwnerBusy) {
				t.Fatalf("batch constructor accepted pending feedback: owner=%p err=%v", owner, err)
			}
			if engine.Snapshot() != before {
				t.Fatal("rejected construction changed engine ownership")
			}
			deliverFeedbackColdOwnerTestClaim(t, engine, claim, retry)
			if owner, err := NewControllerPersonaDownstreamPacketBatchOwner(engine, participant); err != nil || owner == nil {
				t.Fatalf("batch constructor remained blocked after delivery: owner=%p err=%v", owner, err)
			}
		})
	}
}

func TestPersonaFeedbackColdOwnerSealRejectsSideClaimAndRetry(t *testing.T) {
	for _, retry := range []bool{false, true} {
		name := "admitted"
		if retry {
			name = "retry"
		}
		t.Run(name, func(t *testing.T) {
			engine, err := NewAuthorizedControllerPersonaEngine(testOnlyAuthorizedPersonaConfig(t), 0)
			if err != nil {
				t.Fatal(err)
			}
			activateFeedbackColdOwnerTestEngine(t, engine)
			seal := engine.issueAuthorizedRetainedUSBEngineColdSeal()
			if !engine.authenticatesAuthorizedRetainedUSBEngineColdSeal(seal) {
				t.Fatal("fixture's exact seal was not accepted before side ownership")
			}
			claim := detachFeedbackColdOwnerTestClaim(t, engine, retry)
			if issued := engine.issueAuthorizedRetainedUSBEngineColdSeal(); issued.owner != nil {
				t.Fatal("issued a cold seal while side feedback was pending")
			}
			// Matching the busy value image must not turn in-flight/retry
			// ownership into an eligible cold engine, even for package-local code.
			seal.state = *engine
			if engine.authenticatesAuthorizedRetainedUSBEngineColdSeal(seal) {
				t.Fatal("authenticated a matching busy engine image as cold")
			}
			deliverFeedbackColdOwnerTestClaim(t, engine, claim, retry)
			if fresh := engine.issueAuthorizedRetainedUSBEngineColdSeal(); !engine.authenticatesAuthorizedRetainedUSBEngineColdSeal(fresh) {
				t.Fatal("exact seal remained blocked after side delivery")
			}
		})
	}
}

func TestPersonaFeedbackColdOwnerConstructionProofRejectsPendingSideWork(t *testing.T) {
	for _, retry := range []bool{false, true} {
		name := "admitted"
		if retry {
			name = "retry"
		}
		t.Run(name, func(t *testing.T) {
			engine, err := NewAuthorizedControllerPersonaEngine(testOnlyAuthorizedPersonaConfig(t), 0)
			if err != nil {
				t.Fatal(err)
			}
			local := newScriptedControllerPersonaLocalExecutor()
			adapter, proof, err := newDormantRetainedUSBAdapter(engine,
				testAuthorizedRetainedAuthorityID, testAuthorizedRetainedDeviceID,
				local, 100*time.Millisecond, authorizedRetainedUSBConstructionAuthority)
			if err != nil {
				t.Fatal(err)
			}
			activateFeedbackColdOwnerTestEngine(t, engine)
			detachFeedbackColdOwnerTestClaim(t, engine, retry)
			if adapter.consumeAuthorizedRetainedUSBConstructionProof(proof, engine,
				testAuthorizedRetainedAuthorityID, testAuthorizedRetainedDeviceID, 0,
				local, 100*time.Millisecond) || proof.consumed {
				t.Fatal("construction proof accepted or consumed pending side work")
			}
		})
	}
}

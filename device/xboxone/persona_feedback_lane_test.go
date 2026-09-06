package xboxone

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func selectTestOrdinaryFeedback(t *testing.T, engine *ControllerPersonaEngine, action ControllerPersonaAction, nowMS uint64) ControllerPersonaClaim {
	t.Helper()
	var wire [ControllerPersonaMaximumWireSize]byte
	size := 0
	switch action {
	case ControllerPersonaApplyDirectMotor:
		size = DirectMotorMessageSize
		require.NoError(t, EncodeDirectMotorMessageInto(wire[:size], 0x31, testDirectMotorBody()))
	case ControllerPersonaApplyGuideLED:
		size = GuideLEDCommandMessageSize
		require.NoError(t, EncodeGuideLEDCommandMessageInto(wire[:size], 0x71,
			GuideLEDCommandV1{Pattern: GuideLEDPatternSlowBlink, Intensity: 20}))
	default:
		t.Fatal("unexpected test feedback action")
	}
	claim, disposition, err := engine.ReceiveHostMessage(nowMS, wire[:size])
	require.NoError(t, err)
	require.Equal(t, ControllerPersonaHostActionClaimed, disposition)
	return claim
}

func detachTestOrdinaryFeedback(t *testing.T, engine *ControllerPersonaEngine, action ControllerPersonaAction, nowMS uint64) ControllerPersonaClaim {
	t.Helper()
	claim := selectTestOrdinaryFeedback(t, engine, action, nowMS)
	require.NoError(t, engine.AdmitAndCopy(claim, nil, nowMS))
	require.NoError(t, engine.detachOrdinaryFeedback(claim))
	return claim
}

type ordinaryFeedbackPrimaryTestSnapshot struct {
	hasClaim, admitted, retryPending bool
	claimToken                       uint64
	claim, retry                     controllerPersonaRecord
	global, input                    SequenceCounter
}

func snapshotOrdinaryFeedbackPrimary(engine *ControllerPersonaEngine) ordinaryFeedbackPrimaryTestSnapshot {
	return ordinaryFeedbackPrimaryTestSnapshot{
		hasClaim: engine.hasClaim, admitted: engine.claimAdmitted, retryPending: engine.retryPending,
		claimToken: engine.claimToken, claim: engine.claimRecord, retry: engine.retryRecord,
		global: engine.global, input: engine.input,
	}
}

func TestControllerPersonaOrdinaryFeedbackDetachRequiresAdmissionAndCommitsOnlyAtCompletion(t *testing.T) {
	for _, action := range []ControllerPersonaAction{ControllerPersonaApplyDirectMotor, ControllerPersonaApplyGuideLED} {
		t.Run(fmt.Sprint(action), func(t *testing.T) {
			engine, now := makePersonaActive(t, []byte{1, 2, 3})
			before := engine.Snapshot().Feedback
			claim := selectTestOrdinaryFeedback(t, engine, action, now+1)
			require.NotZero(t, claim.Sequence(), "incoming host sequence is retained, not owned by an egress pool")
			require.ErrorIs(t, engine.detachOrdinaryFeedback(claim), ErrControllerPersonaClaimNotAdmitted)
			require.False(t, engine.ordinaryFeedbackPending())
			require.NoError(t, engine.AdmitAndCopy(claim, nil, now+1))
			require.NoError(t, engine.detachOrdinaryFeedback(claim))
			snapshot := engine.Snapshot()
			require.False(t, snapshot.ClaimOutstanding)
			require.False(t, snapshot.ClaimAdmitted)
			require.False(t, snapshot.RetryPending)
			require.True(t, snapshot.OrdinaryFeedbackOutstanding)
			require.True(t, snapshot.OrdinaryFeedbackAdmitted)
			require.False(t, snapshot.OrdinaryFeedbackRetryPending)
			require.Equal(t, action, snapshot.OrdinaryFeedbackAction)
			require.Equal(t, before, snapshot.Feedback, "moving ownership is not delivery")
			require.ErrorIs(t, engine.Resolve(claim, ControllerPersonaDelivered, now+2), ErrInvalidControllerPersonaClaim)
			require.ErrorIs(t, engine.AdmitAndCopy(claim, nil, now+2), ErrInvalidControllerPersonaClaim)
			require.Error(t, engine.detachOrdinaryFeedback(claim))

			input, err := engine.ClaimInput(now+3, GamepadInputReportV1{State: InputStateV1{A: true, LeftStickX: 123}})
			require.NoError(t, err)
			var wire [ControllerPersonaMaximumWireSize]byte
			require.NoError(t, engine.AdmitAndCopy(input, wire[:input.Size()], now+4))
			primary := snapshotOrdinaryFeedbackPrimary(engine)
			require.NoError(t, engine.completeOrdinaryFeedback(claim, ControllerPersonaDelivered, now+2))
			require.Equal(t, primary, snapshotOrdinaryFeedbackPrimary(engine), "completion must not release or overwrite admitted IN")
			require.Equal(t, now+4, engine.lastNowMS, "out-of-order completion timestamps clamp to the shared clock")
			require.False(t, engine.ordinaryFeedbackPending())
			require.Zero(t, engine.Snapshot().OrdinaryFeedbackAction)
			if action == ControllerPersonaApplyDirectMotor {
				require.Equal(t, testDirectMotorBody(), engine.Snapshot().Feedback.DirectMotor)
				require.Equal(t, before.GuideLED, engine.Snapshot().Feedback.GuideLED)
			} else {
				require.Equal(t, claim.guideLED, engine.Snapshot().Feedback.GuideLED)
				require.Equal(t, before.DirectMotor, engine.Snapshot().Feedback.DirectMotor)
			}
			require.NoError(t, engine.Resolve(input, ControllerPersonaDelivered, now+5))
			_, decoded, err := DecodeGamepadInputMessage(wire[:input.Size()])
			require.NoError(t, err)
			require.True(t, decoded.State.A)
			require.EqualValues(t, 123, decoded.State.LeftStickX)
			require.ErrorIs(t, engine.completeOrdinaryFeedback(claim, ControllerPersonaDelivered, now+6), ErrInvalidControllerPersonaClaim)
		})
	}
}

func TestControllerPersonaOrdinaryFeedbackRetryNeverMutatesPrimaryInputRetry(t *testing.T) {
	for _, outcome := range []ControllerPersonaOutcome{ControllerPersonaDeferred, ControllerPersonaDeliveryFailed, ControllerPersonaExecutionCancelled} {
		t.Run(fmt.Sprint(outcome), func(t *testing.T) {
			engine, now := makePersonaActive(t, []byte{1})
			feedback := detachTestOrdinaryFeedback(t, engine, ControllerPersonaApplyDirectMotor, now+1)
			input, err := engine.ClaimInput(now+2, GamepadInputReportV1{State: InputStateV1{B: true}})
			require.NoError(t, err)
			var original [ControllerPersonaMaximumWireSize]byte
			require.NoError(t, engine.AdmitAndCopy(input, original[:input.Size()], now+2))
			require.NoError(t, engine.Resolve(input, ControllerPersonaDeliveryFailed, now+3))
			primary := snapshotOrdinaryFeedbackPrimary(engine)
			require.NoError(t, engine.completeOrdinaryFeedback(feedback, outcome, now+4))
			require.Equal(t, primary, snapshotOrdinaryFeedbackPrimary(engine))
			require.True(t, engine.Snapshot().RetryPending)
			require.True(t, engine.Snapshot().OrdinaryFeedbackRetryPending)
			require.False(t, engine.Snapshot().OrdinaryFeedbackAdmitted)
			require.Equal(t, RumbleBodyV1{}, engine.Snapshot().Feedback.DirectMotor)
			retained := engine.ordinaryFeedback.record
			retry, present, err := engine.admitOrdinaryFeedbackRetry(now + 5)
			require.NoError(t, err)
			require.True(t, present)
			require.NotEqual(t, feedback.token, retry.token)
			require.Equal(t, feedback.directMotor, retry.directMotor)
			require.Equal(t, feedback.Sequence(), retry.Sequence())
			require.Equal(t, now+5, retry.SelectedAtMilliseconds())
			retained.selectedAtMS = now + 5
			require.Equal(t, retained, engine.ordinaryFeedback.record)
			require.Equal(t, primary, snapshotOrdinaryFeedbackPrimary(engine))
			require.ErrorIs(t, engine.completeOrdinaryFeedback(feedback, ControllerPersonaDelivered, now+5), ErrInvalidControllerPersonaClaim)
			require.ErrorIs(t, engine.AdmitAndCopy(retry, nil, now+5), ErrInvalidControllerPersonaClaim)

			inputRetry, err := engine.ClaimRetry(now + 6)
			require.NoError(t, err)
			require.Equal(t, ControllerPersonaSendInput, inputRetry.Action())
			var repeated [ControllerPersonaMaximumWireSize]byte
			require.NoError(t, engine.AdmitAndCopy(inputRetry, repeated[:inputRetry.Size()], now+6))
			require.Equal(t, original, repeated)
			primary = snapshotOrdinaryFeedbackPrimary(engine)
			require.NoError(t, engine.completeOrdinaryFeedback(retry, ControllerPersonaDelivered, now+7))
			require.Equal(t, primary, snapshotOrdinaryFeedbackPrimary(engine))
			require.NoError(t, engine.Resolve(inputRetry, ControllerPersonaDelivered, now+8))
			require.False(t, engine.Snapshot().RetryPending)
			require.False(t, engine.ordinaryFeedbackPending())
			require.Equal(t, testDirectMotorBody(), engine.Snapshot().Feedback.DirectMotor)
		})
	}
}

func TestControllerPersonaOrdinaryFeedbackAllowsInputGuideStatusAndExactRetries(t *testing.T) {
	for _, failedFeedback := range []bool{false, true} {
		for _, kind := range []string{"input", "guide", "status"} {
			t.Run(fmt.Sprintf("feedbackRetry=%t/%s", failedFeedback, kind), func(t *testing.T) {
				engine, now := makePersonaActive(t, []byte{1})
				feedback := detachTestOrdinaryFeedback(t, engine, ControllerPersonaApplyGuideLED, now+1)
				if failedFeedback {
					require.NoError(t, engine.completeOrdinaryFeedback(feedback, ControllerPersonaDeferred, now+2))
				}
				const selectedAt = uint64(1003)
				var claim ControllerPersonaClaim
				var err error
				switch kind {
				case "input":
					claim, err = engine.ClaimInput(selectedAt, GamepadInputReportV1{State: InputStateV1{X: true}})
				case "guide":
					claim, err = engine.ClaimGuideButtonStatus(selectedAt, GuideButtonStatusV1{Down: true})
				case "status":
					var present bool
					claim, present, err = engine.ClaimPoll(selectedAt)
					require.True(t, present)
					require.Equal(t, ControllerPersonaSendCurrentStatus, claim.Action())
					require.False(t, engine.claimRecord.lifecycleOwned)
				}
				require.NoError(t, err)
				var first [ControllerPersonaMaximumWireSize]byte
				require.NoError(t, engine.AdmitAndCopy(claim, first[:claim.Size()], selectedAt))
				require.NoError(t, engine.Resolve(claim, ControllerPersonaDeferred, selectedAt))
				require.True(t, engine.retryPending)
				require.NoError(t, engine.SetCurrentInput(GamepadInputReportV1{State: InputStateV1{Y: true}}))
				retainedSide := engine.ordinaryFeedback
				retry, err := engine.ClaimRetry(selectedAt + 1)
				require.NoError(t, err)
				var second [ControllerPersonaMaximumWireSize]byte
				require.NoError(t, engine.AdmitAndCopy(retry, second[:retry.Size()], selectedAt+1))
				require.Equal(t, first, second)
				require.NoError(t, engine.Resolve(retry, ControllerPersonaDelivered, selectedAt+2))
				require.Equal(t, retainedSide, engine.ordinaryFeedback)
				if failedFeedback {
					var present bool
					feedback, present, err = engine.admitOrdinaryFeedbackRetry(selectedAt + 3)
					require.NoError(t, err)
					require.True(t, present)
				}
				require.NoError(t, engine.completeOrdinaryFeedback(feedback, ControllerPersonaDelivered, selectedAt+4))
			})
		}
	}
}

func TestControllerPersonaOrdinaryFeedbackFencesNonInputEntryPoints(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint(retry), func(t *testing.T) {
			engine, now := makePersonaActive(t, []byte{1})
			feedback := detachTestOrdinaryFeedback(t, engine, ControllerPersonaApplyDirectMotor, now+1)
			wantErr := ErrControllerPersonaClaimOutstanding
			if retry {
				require.NoError(t, engine.completeOrdinaryFeedback(feedback, ControllerPersonaDeferred, now+2))
				wantErr = ErrControllerPersonaRetryRequired
			}
			before := engine.Snapshot()
			_, err := engine.ClaimUSBControl(testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration, 0, 0, 0))
			require.ErrorIs(t, err, wantErr)
			_, _, err = engine.ReceiveHostMessage(now+3, []byte{0x05, 0x20, 1, 1, byte(SetDeviceStateReset)})
			require.ErrorIs(t, err, wantErr)
			_, err = engine.ClaimNextLifecycleAction(now + 3)
			require.ErrorIs(t, err, wantErr)
			_, err = engine.ClaimMetadataPacket(now + 3)
			require.ErrorIs(t, err, wantErr)
			_, _, err = engine.claimHostPacket(now+3, ControllerDownstreamPacket{})
			require.ErrorIs(t, err, wantErr)
			_, err = engine.claimConfigurationLossClear(now + 3)
			require.ErrorIs(t, err, wantErr)
			require.ErrorIs(t, engine.adoptRetainedUSBIPAddress(), ErrControllerPersonaBoundaryBlocked)
			require.ErrorIs(t, engine.Reconnect(now+3), ErrControllerPersonaBoundaryBlocked)
			require.Equal(t, before, engine.Snapshot())
		})
	}
}

func TestControllerPersonaOrdinaryFeedbackNeverPermitsLifecyclePoll(t *testing.T) {
	engine := newTestControllerPersonaEngine(t, []byte{1}, 0)
	configurePersonaUSB(t, engine, 0)
	feedback := detachTestOrdinaryFeedback(t, engine, ControllerPersonaApplyDirectMotor, 1)
	for _, retry := range []bool{false, true} {
		if retry {
			require.NoError(t, engine.completeOrdinaryFeedback(feedback, ControllerPersonaDeferred, 2))
		}
		before := engine.lifecycle.Snapshot()
		claim, present, err := engine.ClaimPoll(3)
		require.ErrorIs(t, err, ErrControllerPersonaUpstreamGated)
		require.False(t, present)
		require.False(t, claim.Valid())
		require.Equal(t, before, engine.lifecycle.Snapshot())
	}
}

func TestControllerPersonaOrdinaryFeedbackRejectsHiddenOwnershipAtDetach(t *testing.T) {
	mutations := map[string]func(*controllerPersonaRecord){
		"clear-action":             func(r *controllerPersonaRecord) { r.action = ControllerPersonaClearOutputs },
		"wire":                     func(r *controllerPersonaRecord) { r.wire[0] = 1 },
		"size":                     func(r *controllerPersonaRecord) { r.size = 1 },
		"clear-epoch":              func(r *controllerPersonaRecord) { r.clearEpoch = 1 },
		"usb-claim":                func(r *controllerPersonaRecord) { r.usbClaim.token = 1 },
		"lifecycle-claim":          func(r *controllerPersonaRecord) { r.lifecycleClaim.token = 1 },
		"metadata-claim":           func(r *controllerPersonaRecord) { r.metadataClaim.token = 1 },
		"sequence-claim":           func(r *controllerPersonaRecord) { r.sequenceClaim.token = 1 },
		"sequence-pool":            func(r *controllerPersonaRecord) { r.sequencePool = controllerPersonaGlobalSequence },
		"usb-owner":                func(r *controllerPersonaRecord) { r.usbOwned = true },
		"lifecycle-owner":          func(r *controllerPersonaRecord) { r.lifecycleOwned = true },
		"metadata-owner":           func(r *controllerPersonaRecord) { r.metadataOwned = true },
		"metadata-failure":         func(r *controllerPersonaRecord) { r.metadataFailureHello = true },
		"usb-response":             func(r *controllerPersonaRecord) { r.usbResponseKind = 1 },
		"metadata-kind":            func(r *controllerPersonaRecord) { r.metadataKind = 1 },
		"metadata-ack":             func(r *controllerPersonaRecord) { r.metadataAcknowledgement = true },
		"reliable-ack":             func(r *controllerPersonaRecord) { r.reliableAcknowledgement.Sequence = 1 },
		"reliable-disposition":     func(r *controllerPersonaRecord) { r.reliableDisposition = 1 },
		"reliable-admission-clock": func(r *controllerPersonaRecord) { r.reliableAdmittedAtMS = 1 },
		"reliable-admission":       func(r *controllerPersonaRecord) { r.reliableAdmitted = true },
		"configuration-loss":       func(r *controllerPersonaRecord) { r.configurationLossClear = true },
		"unrelated-led":            func(r *controllerPersonaRecord) { r.guideLED.Intensity = 1 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			engine, now := makePersonaActive(t, []byte{1})
			selectTestOrdinaryFeedback(t, engine, ControllerPersonaApplyDirectMotor, now+1)
			record := engine.claimRecord
			mutate(&record)
			// Adversarial internal state: a matching opaque wrapper must not
			// hide wire or inner ownership under an ordinary action number.
			claim := engine.makeClaim(record)
			engine.claimAdmitted = true
			before := snapshotOrdinaryFeedbackPrimary(engine)
			require.Error(t, engine.detachOrdinaryFeedback(claim))
			require.Equal(t, before, snapshotOrdinaryFeedbackPrimary(engine))
			require.False(t, engine.ordinaryFeedbackPending())
		})
	}
	for _, kind := range []string{"active-packet", "packet-retry", "quarantine", "clear-pending"} {
		t.Run(kind, func(t *testing.T) {
			engine, now := makePersonaActive(t, []byte{1})
			claim := selectTestOrdinaryFeedback(t, engine, ControllerPersonaApplyGuideLED, now+1)
			require.NoError(t, engine.AdmitAndCopy(claim, nil, now+1))
			switch kind {
			case "active-packet":
				engine.hostPacketActive = true
			case "packet-retry":
				engine.hostPacketRetryPending = true
			case "quarantine":
				engine.hostPacketQuarantined = true
			case "clear-pending":
				engine.configurationLossClearPending = true
			}
			require.ErrorIs(t, engine.detachOrdinaryFeedback(claim), ErrControllerPersonaBoundaryBlocked)
			require.False(t, engine.ordinaryFeedbackPending())
		})
	}
}

func TestControllerPersonaOrdinaryFeedbackCompletionAuthenticatesWholeCapability(t *testing.T) {
	mutations := map[string]func(*ControllerPersonaClaim){
		"owner":                func(c *ControllerPersonaClaim) { c.owner = nil },
		"token":                func(c *ControllerPersonaClaim) { c.token++ },
		"generation":           func(c *ControllerPersonaClaim) { c.generation++ },
		"action":               func(c *ControllerPersonaClaim) { c.action = ControllerPersonaApplyGuideLED },
		"size":                 func(c *ControllerPersonaClaim) { c.size++ },
		"sequence":             func(c *ControllerPersonaClaim) { c.sequence++ },
		"clear":                func(c *ControllerPersonaClaim) { c.clearEpoch++ },
		"selected-time":        func(c *ControllerPersonaClaim) { c.selectedAtMS++ },
		"usb-kind":             func(c *ControllerPersonaClaim) { c.usbResponseKind++ },
		"metadata-kind":        func(c *ControllerPersonaClaim) { c.metadataKind++ },
		"metadata-ack":         func(c *ControllerPersonaClaim) { c.metadataAcknowledgement = true },
		"reliable-ack":         func(c *ControllerPersonaClaim) { c.reliableAcknowledgement.Sequence++ },
		"reliable-disposition": func(c *ControllerPersonaClaim) { c.reliableDisposition++ },
		"motor":                func(c *ControllerPersonaClaim) { c.directMotor.LeftVibration++ },
		"led":                  func(c *ControllerPersonaClaim) { c.guideLED.Intensity++ },
		"configuration":        func(c *ControllerPersonaClaim) { c.configurationLossClear = true },
	}
	engine, now := makePersonaActive(t, []byte{1})
	claim := detachTestOrdinaryFeedback(t, engine, ControllerPersonaApplyDirectMotor, now+1)
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			forged := claim
			mutate(&forged)
			before := engine.Snapshot()
			require.ErrorIs(t, engine.completeOrdinaryFeedback(forged, ControllerPersonaDelivered, now+2), ErrInvalidControllerPersonaClaim)
			require.Equal(t, before, engine.Snapshot())
		})
	}
	for _, invalid := range []ControllerPersonaOutcome{0, ControllerPersonaExecutionCancelled + 1} {
		require.ErrorIs(t, engine.completeOrdinaryFeedback(claim, invalid, now+2), ErrInvalidControllerPersonaOutcome)
	}
	require.NoError(t, engine.completeOrdinaryFeedback(claim, ControllerPersonaDelivered, now+2))
}

func TestControllerPersonaOrdinaryFeedbackBoundaryRequiresDrainedCompletion(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprint(reset), func(t *testing.T) {
			engine, now := makePersonaActive(t, []byte{1})
			feedback := detachTestOrdinaryFeedback(t, engine, ControllerPersonaApplyDirectMotor, now+1)
			input, err := engine.ClaimInput(now+2, GamepadInputReportV1{State: InputStateV1{A: true}})
			require.NoError(t, err)
			boundary := engine.BeginDisconnect
			if reset {
				boundary = engine.BeginUSBReset
			}
			before := engine.Snapshot()
			_, err = boundary(now + 3)
			require.ErrorIs(t, err, ErrControllerPersonaBoundaryBlocked)
			require.Equal(t, before, engine.Snapshot(), "admitted side must block before retiring even unadmitted IN")
			require.NoError(t, engine.Resolve(input, ControllerPersonaDeferred, now+3))
			require.NoError(t, engine.completeOrdinaryFeedback(feedback, ControllerPersonaExecutionCancelled, now+4))
			require.True(t, engine.retryPending)
			require.True(t, engine.ordinaryFeedbackRetryPending())
			before = engine.Snapshot()
			_, err = boundary(now + 3)
			require.ErrorIs(t, err, ErrNonMonotonicControllerPersonaClock)
			require.Equal(t, before, engine.Snapshot())
			clear, err := boundary(now + 5)
			require.NoError(t, err)
			require.Equal(t, ControllerPersonaClearOutputs, clear.Action())
			require.NotZero(t, clear.ClearEpoch())
			require.Equal(t, feedback.Generation()+1, engine.Snapshot().Generation)
			require.False(t, engine.ordinaryFeedbackPending())
			require.False(t, engine.retryPending)
			require.ErrorIs(t, engine.completeOrdinaryFeedback(feedback, ControllerPersonaDelivered, now+6), ErrInvalidControllerPersonaClaim)
			deliverPersonaClaim(t, engine, clear, now+6)
			if !reset {
				require.NoError(t, engine.Reconnect(now+7))
			}
			require.False(t, engine.ordinaryFeedbackPending())
			require.Equal(t, clear.ClearEpoch(), engine.Snapshot().Feedback.ClearEpoch)
		})
	}
}

func TestControllerPersonaOrdinaryFeedbackRetryFailsClosedOnClockTokenAndRecordCorruption(t *testing.T) {
	for _, kind := range []string{"clock", "issuer-exhausted", "issuer-wrapped", "record-mismatch"} {
		t.Run(kind, func(t *testing.T) {
			engine, now := makePersonaActive(t, []byte{1})
			claim := detachTestOrdinaryFeedback(t, engine, ControllerPersonaApplyDirectMotor, now+1)
			require.NoError(t, engine.completeOrdinaryFeedback(claim, ControllerPersonaDeferred, now+2))
			admitAt := now + 3
			switch kind {
			case "clock":
				admitAt = now + 1
			case "issuer-exhausted":
				engine.nextToken = ^uint64(0)
			case "issuer-wrapped":
				engine.nextToken = 0
			case "record-mismatch":
				engine.ordinaryFeedback.record.directMotor.LeftVibration++
			}
			before := engine.ordinaryFeedback
			beforeClock, beforeToken := engine.lastNowMS, engine.nextToken
			retry, present, err := engine.admitOrdinaryFeedbackRetry(admitAt)
			require.Error(t, err)
			require.False(t, present)
			require.False(t, retry.Valid())
			require.Equal(t, before, engine.ordinaryFeedback)
			require.Equal(t, beforeClock, engine.lastNowMS)
			require.Equal(t, beforeToken, engine.nextToken)
		})
	}
}

func TestControllerPersonaOrdinaryFeedbackNilAndEmptyLane(t *testing.T) {
	var absent *ControllerPersonaEngine
	require.False(t, absent.ordinaryFeedbackPending())
	require.False(t, absent.ordinaryFeedbackRetryPending())
	require.ErrorIs(t, absent.detachOrdinaryFeedback(ControllerPersonaClaim{}), ErrUninitializedControllerPersona)
	require.ErrorIs(t, absent.completeOrdinaryFeedback(ControllerPersonaClaim{}, ControllerPersonaDelivered, 0), ErrUninitializedControllerPersona)
	_, _, err := absent.admitOrdinaryFeedbackRetry(0)
	require.ErrorIs(t, err, ErrUninitializedControllerPersona)
	engine, now := makePersonaActive(t, []byte{1})
	claim, present, err := engine.admitOrdinaryFeedbackRetry(now)
	require.NoError(t, err)
	require.False(t, present)
	require.False(t, claim.Valid())
}

func TestControllerPersonaOrdinaryFeedbackWarmPathAllocatesZero(t *testing.T) {
	engine, now := makePersonaActive(t, []byte{1})
	var motor [DirectMotorMessageSize]byte
	require.NoError(t, EncodeDirectMotorMessageInto(motor[:], 0x31, testDirectMotorBody()))
	var inputWire [ControllerPersonaMaximumWireSize]byte
	allocations := testing.AllocsPerRun(1000, func() {
		now += 5
		feedback, _, err := engine.ReceiveHostMessage(now, motor[:])
		if err != nil {
			panic(err)
		}
		if err = engine.AdmitAndCopy(feedback, nil, now); err != nil {
			panic(err)
		}
		if err = engine.detachOrdinaryFeedback(feedback); err != nil {
			panic(err)
		}
		input, err := engine.ClaimInput(now+1, GamepadInputReportV1{State: InputStateV1{A: true}})
		if err != nil {
			panic(err)
		}
		if err = engine.AdmitAndCopy(input, inputWire[:input.Size()], now+1); err != nil {
			panic(err)
		}
		if err = engine.completeOrdinaryFeedback(feedback, ControllerPersonaDeferred, now+2); err != nil {
			panic(err)
		}
		retry, present, err := engine.admitOrdinaryFeedbackRetry(now + 3)
		if err != nil || !present {
			panic("ordinary retry not admitted")
		}
		if err = engine.completeOrdinaryFeedback(retry, ControllerPersonaDelivered, now+4); err != nil {
			panic(err)
		}
		if err = engine.Resolve(input, ControllerPersonaDelivered, now+4); err != nil {
			panic(err)
		}
	})
	require.Zero(t, allocations)
}

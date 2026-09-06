package xboxone

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
	"github.com/Alia5/VIIPER/internal/retainedusb"
)

func activeRetainedJournalLifecyclePersona(t *testing.T) *DormantRetainedUSBAdapter {
	t.Helper()
	adapter, executor := startRetainedJournalTestPersona(t)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
	permitRetainedJournalTestInput(t, adapter, executor)
	return adapter
}

func retainedJournalLifecycleControl(t *testing.T, adapter *DormantRetainedUSBAdapter,
	ordinal uint64, setup []byte, delivered bool,
) {
	t.Helper()
	ticket, err := adapter.Stage(retainedControlRequest(41, ordinal, uint32(ordinal), setup))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := adapter.Prepare(ticket, nil, adapterTestTime(ordinal))
	if err != nil || prepared.Result != retainedusb.ResultSuccess {
		t.Fatalf("control preparation = (%+v, %v)", prepared, err)
	}
	if err := adapter.Complete(ticket, delivered, adapterTestTime(ordinal)); err != nil {
		t.Fatal(err)
	}
}

func retainedJournalLifecyclePollPending(t *testing.T, adapter *DormantRetainedUSBAdapter, ordinal uint64) {
	t.Helper()
	ticket, err := adapter.Stage(retainedINRequest(41, ordinal, uint32(ordinal), 64))
	if err != nil {
		t.Fatal(err)
	}
	var scratch [64]byte
	prepared, err := adapter.Prepare(ticket, scratch[:], adapterTestTime(ordinal))
	if err != nil || prepared.Result != retainedusb.ResultPending {
		t.Fatalf("gated poll = (%+v, %v)", prepared, err)
	}
	if err := adapter.Retire(ticket, retainedusb.RetireUnlink, adapterTestTime(ordinal)); err != nil {
		t.Fatal(err)
	}
}

func retainedJournalLifecycleState(adapter *DormantRetainedUSBAdapter) (bool, inputpresentation.Claim, uint64) {
	journal := adapter.coordinator.inputJournal
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return journal.active, journal.claim, journal.source.Generation()
}

func publishRetainedJournalLifecycleBurst(t *testing.T, adapter *DormantRetainedUSBAdapter, firstRevision uint64) {
	t.Helper()
	// More boundaries than the bounded scheduler can retain must still be
	// harmless while no START baseline has made input presentation active.
	for i := uint64(0); i < 2*inputpresentation.FixedReportTransitionCapacity+2; i++ {
		state := GamepadInputReportV1{State: InputStateV1{A: i%2 == 0}}
		if err := adapter.PublishSemanticInput(41, firstRevision+i, state); err != nil {
			t.Fatalf("gated publication %d = %v", i, err)
		}
	}
}

func TestRetainedInputJournalLifecyclePreStartPollDoesNotActivateHistory(t *testing.T) {
	adapter, _, _ := newBoundTestRetainedUSBAdapter(t, []byte{1})
	configureRetainedTestPersona(t, adapter)
	retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 3, 3, 64), 1)
	_, _, generation := retainedJournalLifecycleState(adapter)
	retainedJournalLifecyclePollPending(t, adapter, 4)
	publishRetainedJournalLifecycleBurst(t, adapter, 2)
	active, claim, after := retainedJournalLifecycleState(adapter)
	if active || claim.Valid() || after != generation {
		t.Fatalf("pre-START poll mutated Source ownership: active=%t claim=%+v generation=%d -> %d", active, claim, generation, after)
	}
	if snapshot := adapter.coordinator.inputJournal.source.Snapshot(); snapshot.Received != 0 || snapshot.TransitionDepth != 0 || snapshot.Overflows != 0 {
		t.Fatalf("pre-START input unexpectedly journaled: %+v", snapshot)
	}
}

func TestRetainedInputJournalLifecycleHaltedInitialPollDoesNotCutBaseline(t *testing.T) {
	adapter, _ := startRetainedJournalTestPersona(t)
	retainedJournalLifecycleControl(t, adapter, 10,
		testUSBSetup(usbRequestTypeEndpointOut, usbRequestSetFeature, usbFeatureEndpointHalt, 0x81, 0), true)
	_, _, generation := retainedJournalLifecycleState(adapter)
	retainedJournalLifecyclePollPending(t, adapter, 11)
	publishRetainedJournalLifecycleBurst(t, adapter, 2)
	active, claim, after := retainedJournalLifecycleState(adapter)
	if active || claim.Valid() || after != generation {
		t.Fatalf("halted initial selection mutated Source: active=%t claim=%+v generation=%d -> %d", active, claim, generation, after)
	}
	retainedJournalLifecycleControl(t, adapter, 12,
		testUSBSetup(usbRequestTypeEndpointOut, usbRequestClearFeature, usbFeatureEndpointHalt, 0x81, 0), true)
	if initial := readRetainedJournalTestInput(t, adapter, 13); initial != (GamepadInputReportV1{}) {
		t.Fatalf("initial image replayed gated transitions: %+v", initial)
	}
}

func TestRetainedInputJournalLifecycleControlBoundariesAreDeliveredAndScoped(t *testing.T) {
	for _, test := range []struct {
		name      string
		setup     []byte
		delivered bool
		retired   bool
	}{
		{"repeated configuration delivered", testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration, 1, 0, 0), true, true},
		{"repeated configuration failed", testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration, 1, 0, 0), false, false},
		{"IN clear delivered", testUSBSetup(usbRequestTypeEndpointOut, usbRequestClearFeature, usbFeatureEndpointHalt, 0x81, 0), true, true},
		{"IN clear failed", testUSBSetup(usbRequestTypeEndpointOut, usbRequestClearFeature, usbFeatureEndpointHalt, 0x81, 0), false, false},
		{"OUT clear delivered", testUSBSetup(usbRequestTypeEndpointOut, usbRequestClearFeature, usbFeatureEndpointHalt, 0x01, 0), true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := activeRetainedJournalLifecyclePersona(t)
			press := GamepadInputReportV1{State: InputStateV1{X: true}}
			if err := adapter.PublishSemanticInput(41, 2, press); err != nil {
				t.Fatal(err)
			}
			if err := adapter.PublishSemanticInput(41, 3, GamepadInputReportV1{}); err != nil {
				t.Fatal(err)
			}
			_, _, generation := retainedJournalLifecycleState(adapter)
			retainedJournalLifecycleControl(t, adapter, 10, test.setup, test.delivered)
			_, _, after := retainedJournalLifecycleState(adapter)
			if (after != generation) != test.retired {
				t.Fatalf("history retirement=%t, want %t (generation %d -> %d)", after != generation, test.retired, generation, after)
			}
			want := press
			if test.retired {
				want = GamepadInputReportV1{}
			}
			if got := readRetainedJournalTestInput(t, adapter, 11); got != want {
				t.Fatalf("post-control input = %+v, want %+v", got, want)
			}
		})
	}
}

func TestRetainedInputJournalLifecycleLiveControlBoundaryRetainsFollowingEdges(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup []byte
	}{
		{"repeated configuration", testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration, 1, 0, 0)},
		{"IN clear", testUSBSetup(usbRequestTypeEndpointOut, usbRequestClearFeature, usbFeatureEndpointHalt, 0x81, 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := activeRetainedJournalLifecyclePersona(t)
			if err := adapter.PublishSemanticInput(41, 2, GamepadInputReportV1{State: InputStateV1{X: true}}); err != nil {
				t.Fatal(err)
			}
			if err := adapter.PublishSemanticInput(41, 3, GamepadInputReportV1{}); err != nil {
				t.Fatal(err)
			}
			retainedJournalLifecycleControl(t, adapter, 10, test.setup, true)
			press := GamepadInputReportV1{State: InputStateV1{B: true}}
			if err := adapter.PublishSemanticInput(41, 4, press); err != nil {
				t.Fatal(err)
			}
			if err := adapter.PublishSemanticInput(41, 5, GamepadInputReportV1{}); err != nil {
				t.Fatal(err)
			}
			seenPress, seenRelease := false, false
			for ordinal := uint64(11); ordinal < 15; ordinal++ {
				got := readRetainedJournalTestInput(t, adapter, ordinal)
				if got.State.X {
					t.Fatal("pre-boundary press replayed")
				}
				if got == press {
					seenPress = true
				}
				if seenPress && got == (GamepadInputReportV1{}) {
					seenRelease = true
					break
				}
			}
			if !seenPress || !seenRelease {
				t.Fatalf("lost live successor edges before first IN: press=%t release=%t", seenPress, seenRelease)
			}
		})
	}
}

func TestRetainedInputJournalLifecycleSmallInitialAndRetryRetainOneBaseline(t *testing.T) {
	adapter, executor := startRetainedJournalTestPersona(t)
	initial := GamepadInputReportV1{State: InputStateV1{A: true}}
	if err := adapter.PublishSemanticInput(41, 2, initial); err != nil {
		t.Fatal(err)
	}
	ticket, err := adapter.Stage(retainedINRequest(41, 10, 10, 1))
	if err != nil {
		t.Fatal(err)
	}
	var small [1]byte
	// The existing retained contract rejects too-small host requests; their
	// retirement must not discard the independently materialized initial image.
	if _, err = adapter.Prepare(ticket, small[:], adapterTestTime(10)); !errors.Is(err, errDormantRetainedUSBInvalidPreparation) {
		t.Fatalf("small initial preparation = %v", err)
	}
	active, sourceClaim, generation := retainedJournalLifecycleState(adapter)
	if !active || !sourceClaim.Valid() {
		t.Fatal("initial Source pair was not retained")
	}
	press := GamepadInputReportV1{State: InputStateV1{B: true}}
	if err := adapter.PublishSemanticInput(41, 3, press); err != nil {
		t.Fatal(err)
	}
	if err := adapter.PublishSemanticInput(41, 4, GamepadInputReportV1{}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Retire(ticket, retainedusb.RetireUnlink, adapterTestTime(10)); err != nil {
		t.Fatal(err)
	}
	ticket, err = adapter.Stage(retainedINRequest(41, 11, 11, 64))
	if err != nil {
		t.Fatal(err)
	}
	var wire [64]byte
	prepared, err := adapter.Prepare(ticket, wire[:], adapterTestTime(11))
	if err != nil || prepared.Result != retainedusb.ResultData {
		t.Fatalf("initial preparation = (%+v, %v)", prepared, err)
	}
	_, got, err := DecodeGamepadInputMessage(wire[:prepared.ActualLength])
	if err != nil || got != initial {
		t.Fatalf("initial baseline changed: %+v, %v", got, err)
	}
	if err := adapter.Complete(ticket, false, adapterTestTime(11)); err != nil {
		t.Fatal(err)
	}
	_, stillClaim, stillGeneration := retainedJournalLifecycleState(adapter)
	if stillClaim != sourceClaim || stillGeneration != generation {
		t.Fatal("failed write changed the semantic claim or baseline generation")
	}
	_, retry := retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 12, 12, 64), 12)
	if !bytes.Equal(retry, wire[:prepared.ActualLength]) {
		t.Fatalf("initial retry changed wire: % x -> % x", wire[:prepared.ActualLength], retry)
	}
	permitRetainedJournalTestInput(t, adapter, executor)
	if got := readRetainedJournalTestInput(t, adapter, 13); got != press {
		t.Fatalf("post-initial press lost: %+v", got)
	}
	if got := readRetainedJournalTestInput(t, adapter, 14); got != (GamepadInputReportV1{}) {
		t.Fatalf("post-initial release lost: %+v", got)
	}
}

func TestRetainedInputJournalLifecycleUSBResetRetiresEngineRetryAndSourceClaim(t *testing.T) {
	adapter := activeRetainedJournalLifecyclePersona(t)
	press := GamepadInputReportV1{State: InputStateV1{Y: true}}
	if err := adapter.PublishSemanticInput(41, 2, press); err != nil {
		t.Fatal(err)
	}
	ticket, err := adapter.Stage(retainedINRequest(41, 10, 10, 64))
	if err != nil {
		t.Fatal(err)
	}
	var wire [64]byte
	prepared, err := adapter.Prepare(ticket, wire[:], adapterTestTime(10))
	if err != nil || prepared.Result != retainedusb.ResultData {
		t.Fatalf("prepare = (%+v, %v)", prepared, err)
	}
	if err := adapter.Complete(ticket, false, adapterTestTime(10)); err != nil {
		t.Fatal(err)
	}
	_, oldClaim, generation := retainedJournalLifecycleState(adapter)
	if err := adapter.PublishSemanticInput(41, 3, GamepadInputReportV1{}); err != nil {
		t.Fatal(err)
	}
	reset := adapterTestReset(adapterTestLease(adapter, 41), 91, 1)
	result, err := adapter.ResetAndRestart(reset, time.Now().Add(time.Second))
	if err != nil || result.State != retainedusb.ImportResetSafe {
		t.Fatalf("reset = (%+v, %v)", result, err)
	}
	active, claim, after := retainedJournalLifecycleState(adapter)
	if active || claim.Valid() || after == generation {
		t.Fatalf("reset retained old Source ownership: active=%t claim=%+v generation=%d -> %d", active, claim, generation, after)
	}
	if adapter.coordinator.inputJournal.source.ResolveInputPresentation(oldClaim, inputpresentation.OutcomeCommit, time.Now()) {
		t.Fatal("stale Source completion committed after reset")
	}
	snapshot, ok := adapter.coordinator.snapshot()
	if !ok || snapshot.RetryPending || snapshot.ClaimOutstanding {
		t.Fatalf("reset retained engine retry/claim: %+v", snapshot)
	}
	publishRetainedJournalLifecycleBurst(t, adapter, 4)
}

func TestRetainedInputJournalLifecycleStopRetiresButQuiesceKeepsInput(t *testing.T) {
	for _, state := range []SetDeviceStateValue{SetDeviceStateStop, SetDeviceStateQuiesce} {
		name := "stop"
		if state == SetDeviceStateQuiesce {
			name = "quiesce"
		}
		t.Run(name, func(t *testing.T) {
			adapter, executor := startRetainedJournalTestPersona(t)
			retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
			permitRetainedJournalTestInput(t, adapter, executor)
			press := GamepadInputReportV1{State: InputStateV1{X: true}}
			if err := adapter.PublishSemanticInput(41, 2, press); err != nil {
				t.Fatal(err)
			}
			if err := adapter.PublishSemanticInput(41, 3, GamepadInputReportV1{}); err != nil {
				t.Fatal(err)
			}
			_, _, generation := retainedJournalLifecycleState(adapter)
			retainedPrepareAndComplete(t, adapter, retainedOUTRequest(41, 10, 10,
				[]byte{0x05, 0x20, 0x03, 0x01, byte(state)}), 10)
			triggerAndRetireLocalAction(t, adapter, 11, 11)
			action := ControllerPersonaClearOutputs
			if state == SetDeviceStateStop {
				action = ControllerPersonaGateNormalUpstream
			}
			waitForLocalExecution(t, executor, action)
			deadline := time.Now().Add(time.Second)
			for {
				snapshot, _ := adapter.coordinator.snapshot()
				if !snapshot.ClaimOutstanding {
					break
				}
				if !time.Now().Before(deadline) {
					t.Fatal("local lifecycle action did not complete")
				}
				time.Sleep(time.Millisecond)
			}
			active, claim, after := retainedJournalLifecycleState(adapter)
			if state == SetDeviceStateStop {
				if active || claim.Valid() || after == generation {
					t.Fatalf("STOP retained history: active=%t claim=%+v generation=%d -> %d", active, claim, generation, after)
				}
				publishRetainedJournalLifecycleBurst(t, adapter, 4)
				return
			}
			if !active || after != generation {
				t.Fatalf("QUIESCE retired input: active=%t generation=%d -> %d", active, generation, after)
			}
			if got := readRetainedJournalTestInput(t, adapter, 12); got != press {
				t.Fatalf("QUIESCE lost pending press: %+v", got)
			}
			if got := readRetainedJournalTestInput(t, adapter, 13); got != (GamepadInputReportV1{}) {
				t.Fatalf("QUIESCE lost pending release: %+v", got)
			}
		})
	}
}

func TestRetainedInputJournalLifecycleControlBeforePermitKeepsLaterLiveEdges(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup []byte
	}{
		{"repeated configuration", testUSBSetup(usbRequestTypeDeviceOut, usbRequestSetConfiguration, 1, 0, 0)},
		{"IN clear", testUSBSetup(usbRequestTypeEndpointOut, usbRequestClearFeature, usbFeatureEndpointHalt, 0x81, 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, beforePermit := range []bool{false, true} {
				name := "after Permit"
				if beforePermit {
					name = "before Permit"
				}
				t.Run(name, func(t *testing.T) {
					adapter, executor := startRetainedJournalTestPersona(t)
					retainedPrepareAndComplete(t, adapter, retainedINRequest(41, 6, 6, 64), 4)
					// Initial input has committed, but the local Permit action has not.
					// EP0 may still complete an endpoint presentation boundary here.
					retainedJournalLifecycleControl(t, adapter, 10, test.setup, true)
					if !beforePermit {
						permitRetainedJournalTestInput(t, adapter, executor)
					}
					press := GamepadInputReportV1{State: InputStateV1{B: true}}
					if err := adapter.PublishSemanticInput(41, 2, press); err != nil {
						t.Fatal(err)
					}
					if err := adapter.PublishSemanticInput(41, 3, GamepadInputReportV1{}); err != nil {
						t.Fatal(err)
					}
					if beforePermit {
						permitRetainedJournalTestInput(t, adapter, executor)
					}
					seenPress, seenRelease := false, false
					for ordinal := uint64(11); ordinal < 15; ordinal++ {
						got := readRetainedJournalTestInput(t, adapter, ordinal)
						if got == press {
							seenPress = true
						}
						if seenPress && got == (GamepadInputReportV1{}) {
							seenRelease = true
							break
						}
					}
					if !seenPress || !seenRelease {
						t.Fatalf("successor input after established initial/control baseline was coalesced: press=%t release=%t", seenPress, seenRelease)
					}
				})
			}
		})
	}
}

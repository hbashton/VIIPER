package xboxone

import (
	"errors"
	"testing"
)

func metadataRequestCommand() ControllerHostCommand {
	return ControllerHostCommand{Kind: ControllerHostCommandMetadataRequest, Sequence: 1}
}

func setStateCommand(sequence uint8, state SetDeviceStateValue) ControllerHostCommand {
	return ControllerHostCommand{
		Kind: ControllerHostCommandSetDeviceState, Sequence: sequence, State: state,
	}
}

func claimLifecycleCommand(
	t *testing.T,
	lifecycle *ControllerLifecycle,
	nowMS uint64,
	command ControllerHostCommand,
) ControllerLifecycleClaim {
	t.Helper()
	claim, present, err := lifecycle.ClaimHostCommand(nowMS, command)
	if err != nil || !present {
		t.Fatalf("ClaimHostCommand = (present %t, %v)", present, err)
	}
	return claim
}

func admitLifecycleAction(
	t *testing.T,
	lifecycle *ControllerLifecycle,
	claim ControllerLifecycleClaim,
	nowMS uint64,
) {
	t.Helper()
	admitted, err := lifecycle.Admit(claim, nowMS)
	if err != nil || !admitted {
		t.Fatalf("Admit = (%t, %v), want (true, nil)", admitted, err)
	}
}

func deliverLifecycleAction(
	t *testing.T,
	lifecycle *ControllerLifecycle,
	claim ControllerLifecycleClaim,
	nowMS uint64,
) {
	t.Helper()
	admitLifecycleAction(t, lifecycle, claim, nowMS)
	if err := lifecycle.Resolve(claim, ControllerLifecycleDelivered, nowMS); err != nil {
		t.Fatalf("Resolve delivered: %v", err)
	}
}

func nextLifecycleAction(
	t *testing.T,
	lifecycle *ControllerLifecycle,
	nowMS uint64,
) ControllerLifecycleClaim {
	t.Helper()
	claim, err := lifecycle.ClaimNextAction(nowMS)
	if err != nil {
		t.Fatalf("ClaimNextAction: %v", err)
	}
	return claim
}

func requireLifecycleAction(
	t *testing.T,
	claim ControllerLifecycleClaim,
	cursor int,
	total int,
	want ControllerLifecycleAction,
) {
	t.Helper()
	if claim.Action() != want || claim.Cursor() != cursor || claim.TotalActions() != total {
		t.Fatalf("claim = action %d cursor %d/%d, want %d cursor %d/%d",
			claim.Action(), claim.Cursor(), claim.TotalActions(), want, cursor, total)
	}
}

func TestControllerHostCommandDecodeExactMessages(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
		want ControllerHostCommand
	}{
		{name: "metadata request", wire: []byte{0x04, 0x20, 0x01, 0x00}, want: metadataRequestCommand()},
		{name: "start", wire: []byte{0x05, 0x20, 0x02, 0x01, 0x00}, want: setStateCommand(2, SetDeviceStateStart)},
		{name: "reset", wire: []byte{0x05, 0x20, 0xff, 0x01, 0x07}, want: setStateCommand(0xff, SetDeviceStateReset)},
		{name: "extended initialization", wire: []byte{
			0x05, 0x20, 0x02, 0x0f,
			0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x55,
			0x53, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		}, want: ControllerHostCommand{
			Kind:     ControllerHostCommandExtendedSetDeviceStateInitialization,
			Sequence: 2,
		}},
		{name: "security data complete", wire: []byte{
			0x06, 0x20, 0x17, 0x02, 0x01, 0x00,
		}, want: ControllerHostCommand{
			Kind: ControllerHostCommandSecurityDataComplete, Sequence: 0x17,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := DecodeControllerHostCommand(test.wire)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("command = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestControllerHostCommandSecurityDataCompleteFailsClosed(t *testing.T) {
	valid := []byte{0x06, 0x20, 0x17, 0x02, 0x01, 0x00}
	for index := SinglePacketHeaderSize; index < len(valid); index++ {
		mutated := append([]byte(nil), valid...)
		mutated[index] ^= 0xff
		if _, err := DecodeControllerHostCommand(mutated); !errors.Is(
			err, ErrUnsupportedHostCommand) {
			t.Fatalf("mutated payload byte %d error = %v, want %v",
				index-SinglePacketHeaderSize, err, ErrUnsupportedHostCommand)
		}
	}
	for _, wire := range [][]byte{
		{0x06, 0x30, 0x17, 0x02, 0x01, 0x00},
		{0x06, 0x20, 0x17, 0x01, 0x01},
		{0x06, 0x20, 0x17, 0x03, 0x01, 0x00, 0x00},
	} {
		if _, err := DecodeControllerHostCommand(wire); err == nil {
			t.Fatalf("unsupported security wire accepted: % x", wire)
		}
	}
}

func TestControllerHostCommandRejectsMalformedAndUnknown(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
		want error
	}{
		{name: "short", wire: []byte{0x04, 0x20, 0x01}, want: ErrInvalidLength},
		{name: "metadata wrong sequence", wire: []byte{0x04, 0x20, 0x02, 0x00}, want: ErrUnsupportedHostCommand},
		{name: "metadata with payload", wire: []byte{0x04, 0x20, 0x01, 0x01, 0x00}, want: ErrUnsupportedHostCommand},
		{name: "non-system", wire: []byte{0x05, 0x00, 0x01, 0x01, 0x00}, want: ErrUnsupportedHostCommand},
		{name: "ACK requested", wire: []byte{0x05, 0x30, 0x01, 0x01, 0x00}, want: ErrUnsupportedHostCommand},
		{name: "expansion", wire: []byte{0x05, 0x21, 0x01, 0x01, 0x00}, want: ErrUnsupportedHostCommand},
		{name: "reserved state", wire: []byte{0x05, 0x20, 0x01, 0x01, 0x06}, want: ErrReservedDeviceState},
		{name: "unknown command", wire: []byte{0x08, 0x20, 0x01, 0x00}, want: ErrUnsupportedHostCommand},
		{name: "trailing byte", wire: []byte{0x04, 0x20, 0x01, 0x00, 0x00}, want: ErrInvalidLength},
	}
	variant15 := []byte{0x05, 0x20, 0x01, 0x0f}
	variant15 = append(variant15, make([]byte, 15)...)
	tests = append(tests, struct {
		name string
		wire []byte
		want error
	}{name: "unknown fifteen-byte state variant", wire: variant15, want: ErrUnsupportedSetDeviceStateVariant})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeControllerHostCommand(test.wire); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestControllerHostCommandExtendedInitializationFailsClosed(t *testing.T) {
	valid := []byte{
		0x05, 0x20, 0x02, 0x0f,
		0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x55,
		0x53, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	for index := SinglePacketHeaderSize; index < len(valid); index++ {
		mutated := append([]byte(nil), valid...)
		mutated[index] ^= 0xff
		if _, err := DecodeControllerHostCommand(mutated); !errors.Is(
			err, ErrUnsupportedSetDeviceStateVariant) {
			t.Fatalf("mutated payload byte %d error = %v, want %v",
				index-SinglePacketHeaderSize, err, ErrUnsupportedSetDeviceStateVariant)
		}
	}
	for _, size := range []byte{14, 16} {
		mutated := append([]byte(nil), valid...)
		mutated[3] = size
		if _, err := DecodeControllerHostCommand(mutated); !errors.Is(err, ErrInvalidLength) {
			t.Fatalf("declared payload size %d error = %v, want %v",
				size, err, ErrInvalidLength)
		}
	}
	for _, wire := range [][]byte{
		{0x05, 0x20, 0x02, 0x01, 0x06},
		{0x05, 0x20, 0x02, 0x0f,
			0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x55,
			0x53, 0x00, 0x00, 0x00, 0x00, 0x00},
	} {
		if _, err := DecodeControllerHostCommand(wire); err == nil {
			t.Fatalf("malformed wire accepted: % x", wire)
		}
	}
}

func TestControllerLifecycleExtendedInitializationDoesNotReplaceStart(t *testing.T) {
	lifecycle := NewControllerLifecycle(0)
	before := lifecycle.Snapshot()
	wire := []byte{
		0x05, 0x20, 0x02, 0x0f,
		0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x55,
		0x53, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	claim, present, err := lifecycle.ClaimHostWire(1, wire)
	if err != nil || present || claim.Valid() {
		t.Fatalf("extended initialization = (claim %+v, present %t, err %v)",
			claim, present, err)
	}
	if after := lifecycle.Snapshot(); after != before {
		t.Fatalf("extended initialization changed lifecycle: before=%+v after=%+v",
			before, after)
	}
	start := claimLifecycleCommand(t, &lifecycle, 2,
		setStateCommand(3, SetDeviceStateStart))
	requireLifecycleAction(t, start, 0, 3, ControllerLifecycleSendCurrentStatus)
}

func TestControllerLifecycleSecurityDataCompleteIsActiveNoOp(t *testing.T) {
	lifecycle, nowMS := makeActiveLifecycle(t, 0)
	before := lifecycle.Snapshot()
	command := ControllerHostCommand{
		Kind: ControllerHostCommandSecurityDataComplete, Sequence: 1,
	}
	claim, present, err := lifecycle.ClaimHostCommand(nowMS+1, command)
	if err != nil || present || claim.Valid() {
		t.Fatalf("security complete = (claim %+v, present %t, err %v)",
			claim, present, err)
	}
	if after := lifecycle.Snapshot(); after != before {
		t.Fatalf("security complete changed lifecycle: before=%+v after=%+v",
			before, after)
	}

	idle := NewControllerLifecycle(0)
	if _, _, err := idle.ClaimHostCommand(1, command); !errors.Is(
		err, ErrUnexpectedLifecycleCommand) {
		t.Fatalf("idle security complete error = %v", err)
	}
}

func TestControllerLifecycleHelloRetriesOnlyCurrentAction(t *testing.T) {
	lifecycle := NewControllerLifecycle(100)
	claim, present, err := lifecycle.ClaimPoll(100)
	if err != nil || !present {
		t.Fatalf("initial ClaimPoll = (%t, %v)", present, err)
	}
	requireLifecycleAction(t, claim, 0, 1, ControllerLifecycleSendHello)
	if err := lifecycle.Resolve(claim, ControllerLifecycleDeferred, 101); err != nil {
		t.Fatal(err)
	}
	if got := lifecycle.Snapshot(); !got.PendingTransition || !got.RetryPending || got.State != ControllerLifecycleArrival {
		t.Fatalf("deferred snapshot = %+v", got)
	}
	retry, err := lifecycle.ClaimRetry(102)
	if err != nil {
		t.Fatal(err)
	}
	requireLifecycleAction(t, retry, 0, 1, ControllerLifecycleSendHello)
	admitLifecycleAction(t, &lifecycle, retry, 102)
	if err := lifecycle.Resolve(retry, ControllerLifecycleDeliveryFailed, 103); err != nil {
		t.Fatal(err)
	}
	retry, err = lifecycle.ClaimRetry(104)
	if err != nil {
		t.Fatal(err)
	}
	deliverLifecycleAction(t, &lifecycle, retry, 105)
	if got := lifecycle.Snapshot(); got.PendingTransition || got.RetryPending {
		t.Fatalf("delivered Hello left pending state: %+v", got)
	}
	if _, present, err := lifecycle.ClaimPoll(604); err != nil || present {
		t.Fatalf("pre-cadence Poll = (%t, %v)", present, err)
	}
	if _, present, err := lifecycle.ClaimPoll(605); err != nil || !present {
		t.Fatalf("due cadence Poll = (%t, %v)", present, err)
	}
}

func TestControllerLifecycleMetadataStartAndStopCommitAfterFinalAction(t *testing.T) {
	lifecycle := NewControllerLifecycle(0)
	metadataClaim := claimLifecycleCommand(t, &lifecycle, 0, metadataRequestCommand())
	requireLifecycleAction(t, metadataClaim, 0, 1, ControllerLifecycleBeginMetadata)
	metadataGeneration, ok := metadataClaim.MetadataTransferGeneration()
	if !ok || metadataGeneration != 1 {
		t.Fatalf("metadata generation = (%d, %t)", metadataGeneration, ok)
	}
	metadataFence, ok := metadataClaim.MetadataTransferFence()
	if !ok || metadataFence.TransportGeneration != 1 ||
		metadataFence.TransferGeneration != metadataGeneration {
		t.Fatalf("metadata fence = (%+v, %t)", metadataFence, ok)
	}
	if lifecycle.State() != ControllerLifecycleArrival {
		t.Fatal("BeginMetadata claim advanced state")
	}
	deliverLifecycleAction(t, &lifecycle, metadataClaim, 1)
	if lifecycle.State() != ControllerLifecycleMetadata {
		t.Fatalf("metadata state = %d", lifecycle.State())
	}
	if err := lifecycle.MetadataTransferSucceeded(2, metadataFence); err != nil {
		t.Fatal(err)
	}

	start := claimLifecycleCommand(t, &lifecycle, 3, setStateCommand(2, SetDeviceStateStart))
	requireLifecycleAction(t, start, 0, 3, ControllerLifecycleSendCurrentStatus)
	deliverLifecycleAction(t, &lifecycle, start, 4)
	if got := lifecycle.Snapshot(); got.State != ControllerLifecycleIdle || !got.PendingTransition || got.ActionCursor != 1 {
		t.Fatalf("after status = %+v", got)
	}
	initialInput := nextLifecycleAction(t, &lifecycle, 5)
	requireLifecycleAction(t, initialInput, 1, 3, ControllerLifecycleSendInitialInput)
	deliverLifecycleAction(t, &lifecycle, initialInput, 6)
	if lifecycle.State() != ControllerLifecycleIdle {
		t.Fatal("initial input committed Active before permit")
	}
	permit := nextLifecycleAction(t, &lifecycle, 7)
	requireLifecycleAction(t, permit, 2, 3, ControllerLifecyclePermitNormalUpstream)
	deliverLifecycleAction(t, &lifecycle, permit, 8)
	if lifecycle.State() != ControllerLifecycleActive || lifecycle.Snapshot().PendingTransition {
		t.Fatalf("START final state = %+v", lifecycle.Snapshot())
	}

	stop := claimLifecycleCommand(t, &lifecycle, 9, setStateCommand(3, SetDeviceStateStop))
	requireLifecycleAction(t, stop, 0, 2, ControllerLifecycleGateNormalUpstream)
	deliverLifecycleAction(t, &lifecycle, stop, 10)
	if lifecycle.State() != ControllerLifecycleActive {
		t.Fatal("gate committed Idle before clear")
	}
	clearOutputs := nextLifecycleAction(t, &lifecycle, 11)
	requireLifecycleAction(t, clearOutputs, 1, 2, ControllerLifecycleClearOutputs)
	deliverLifecycleAction(t, &lifecycle, clearOutputs, 12)
	if lifecycle.State() != ControllerLifecycleIdle {
		t.Fatalf("STOP final state = %d", lifecycle.State())
	}
}

func makeActiveLifecycle(t *testing.T, nowMS uint64) (ControllerLifecycle, uint64) {
	t.Helper()
	lifecycle := NewControllerLifecycle(nowMS)
	claim := claimLifecycleCommand(t, &lifecycle, nowMS, setStateCommand(1, SetDeviceStateStart))
	for index := 0; index < 3; index++ {
		deliverLifecycleAction(t, &lifecycle, claim, nowMS)
		if index < 2 {
			claim = nextLifecycleAction(t, &lifecycle, nowMS)
		}
	}
	return lifecycle, nowMS
}

func TestControllerLifecycleFailureAndDeferAtEveryActionCursor(t *testing.T) {
	tests := []struct {
		name       string
		newMachine func(*testing.T) ControllerLifecycle
		command    ControllerHostCommand
		actions    [3]ControllerLifecycleAction
		before     ControllerLifecycleState
		after      ControllerLifecycleState
	}{
		{
			name: "start", newMachine: func(*testing.T) ControllerLifecycle { return NewControllerLifecycle(0) },
			command: setStateCommand(1, SetDeviceStateStart),
			actions: [3]ControllerLifecycleAction{
				ControllerLifecycleSendCurrentStatus,
				ControllerLifecycleSendInitialInput,
				ControllerLifecyclePermitNormalUpstream,
			},
			before: ControllerLifecycleArrival, after: ControllerLifecycleActive,
		},
		{
			name: "reset", newMachine: func(t *testing.T) ControllerLifecycle {
				lifecycle, _ := makeActiveLifecycle(t, 0)
				return lifecycle
			},
			command: setStateCommand(2, SetDeviceStateReset),
			actions: [3]ControllerLifecycleAction{
				ControllerLifecycleGateNormalUpstream,
				ControllerLifecycleClearOutputs,
				ControllerLifecycleSendPoweringOffStatus,
			},
			before: ControllerLifecycleActive, after: ControllerLifecycleTerminatingReset,
		},
	}
	for _, test := range tests {
		for failedCursor := 0; failedCursor < len(test.actions); failedCursor++ {
			t.Run(test.name+"/cursor"+string(rune('0'+failedCursor)), func(t *testing.T) {
				lifecycle := test.newMachine(t)
				nowMS := lifecycle.lastNowMS + 1
				claim := claimLifecycleCommand(t, &lifecycle, nowMS, test.command)
				for cursor, action := range test.actions {
					requireLifecycleAction(t, claim, cursor, len(test.actions), action)
					if cursor == failedCursor {
						nowMS++
						if err := lifecycle.Resolve(claim, ControllerLifecycleDeferred, nowMS); err != nil {
							t.Fatal(err)
						}
						nowMS++
						retry, err := lifecycle.ClaimRetry(nowMS)
						if err != nil {
							t.Fatal(err)
						}
						requireLifecycleAction(t, retry, cursor, len(test.actions), action)
						admitLifecycleAction(t, &lifecycle, retry, nowMS)
						nowMS++
						if err := lifecycle.Resolve(retry, ControllerLifecycleDeliveryFailed, nowMS); err != nil {
							t.Fatal(err)
						}
						nowMS++
						claim, err = lifecycle.ClaimRetry(nowMS)
						if err != nil {
							t.Fatal(err)
						}
						requireLifecycleAction(t, claim, cursor, len(test.actions), action)
					}
					nowMS++
					deliverLifecycleAction(t, &lifecycle, claim, nowMS)
					if cursor < len(test.actions)-1 {
						if lifecycle.State() != test.before {
							t.Fatalf("cursor %d committed state %d before final", cursor, lifecycle.State())
						}
						nowMS++
						claim = nextLifecycleAction(t, &lifecycle, nowMS)
					}
				}
				if lifecycle.State() != test.after || lifecycle.Snapshot().PendingTransition {
					t.Fatalf("final lifecycle = %+v", lifecycle.Snapshot())
				}
			})
		}
	}
}

func TestControllerLifecycleTerminationDeadlineStartsAtFinalAction(t *testing.T) {
	lifecycle, _ := makeActiveLifecycle(t, 100)
	claim := claimLifecycleCommand(t, &lifecycle, 120, setStateCommand(2, SetDeviceStateReset))
	deliverLifecycleAction(t, &lifecycle, claim, 125)
	claim = nextLifecycleAction(t, &lifecycle, 130)
	deliverLifecycleAction(t, &lifecycle, claim, 135)
	claim = nextLifecycleAction(t, &lifecycle, 140)
	deliverLifecycleAction(t, &lifecycle, claim, 150)
	if lifecycle.State() != ControllerLifecycleTerminatingReset {
		t.Fatalf("termination state = %d", lifecycle.State())
	}
	if _, present, err := lifecycle.ClaimPoll(649); err != nil || present {
		t.Fatalf("pre-deadline Poll = (%t, %v)", present, err)
	}
	perform, present, err := lifecycle.ClaimPoll(650)
	if err != nil || !present {
		t.Fatalf("deadline Poll = (%t, %v)", present, err)
	}
	requireLifecycleAction(t, perform, 0, 1, ControllerLifecyclePerformReset)
	deliverLifecycleAction(t, &lifecycle, perform, 651)
	if lifecycle.State() != ControllerLifecycleReset {
		t.Fatalf("perform reset state = %d", lifecycle.State())
	}
}

func TestControllerLifecycleUSBResetInterruptsPendingActionCursor(t *testing.T) {
	lifecycle := NewControllerLifecycle(0)
	start := claimLifecycleCommand(t, &lifecycle, 0, setStateCommand(1, SetDeviceStateStart))
	deliverLifecycleAction(t, &lifecycle, start, 1)
	old := nextLifecycleAction(t, &lifecycle, 2)
	requireLifecycleAction(t, old, 1, 3, ControllerLifecycleSendInitialInput)
	restart, err := lifecycle.ClaimRestartAfterUSBReset(3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Admit(old, 3); !errors.Is(err, ErrInvalidLifecycleClaim) {
		t.Fatalf("interrupted action error = %v", err)
	}
	if lifecycle.Generation() != 1 || lifecycle.State() != ControllerLifecycleArrival {
		t.Fatal("restart claim committed state/generation")
	}
	requireLifecycleAction(t, restart, 0, 3, ControllerLifecycleGateNormalUpstream)
	deliverLifecycleAction(t, &lifecycle, restart, 4)
	clearOutputs := nextLifecycleAction(t, &lifecycle, 5)
	requireLifecycleAction(t, clearOutputs, 1, 3, ControllerLifecycleClearOutputs)
	deliverLifecycleAction(t, &lifecycle, clearOutputs, 6)
	if lifecycle.Generation() != 1 {
		t.Fatal("reset generation advanced before final Hello")
	}
	hello := nextLifecycleAction(t, &lifecycle, 7)
	requireLifecycleAction(t, hello, 2, 3, ControllerLifecycleSendHello)
	deliverLifecycleAction(t, &lifecycle, hello, 8)
	if lifecycle.Generation() != 2 || lifecycle.State() != ControllerLifecycleArrival {
		t.Fatalf("restart final = %+v", lifecycle.Snapshot())
	}
}

func TestControllerLifecycleUSBResetWaitsForAdmittedActionDrain(t *testing.T) {
	lifecycle := NewControllerLifecycle(0)
	claim := claimLifecycleCommand(t, &lifecycle, 0, setStateCommand(1, SetDeviceStateStart))
	deliverLifecycleAction(t, &lifecycle, claim, 1)
	claim = nextLifecycleAction(t, &lifecycle, 2)
	deliverLifecycleAction(t, &lifecycle, claim, 3)
	permit := nextLifecycleAction(t, &lifecycle, 4)
	requireLifecycleAction(t, permit, 2, 3, ControllerLifecyclePermitNormalUpstream)
	executionFence, ok := permit.ExecutionFence()
	if !ok || executionFence.Action != ControllerLifecyclePermitNormalUpstream ||
		executionFence.ActionCursor != 2 || executionFence.LeaseToken == 0 {
		t.Fatalf("permit execution fence = (%+v, %t)", executionFence, ok)
	}
	admitLifecycleAction(t, &lifecycle, permit, 4)

	before := lifecycle.Snapshot()
	if _, err := lifecycle.ClaimRestartAfterUSBReset(5); !errors.Is(err, ErrLifecycleClaimOutstanding) {
		t.Fatalf("reset overtook admitted action: %v", err)
	}
	if got := lifecycle.Snapshot(); got != before {
		t.Fatalf("blocked reset changed admitted predecessor: before=%+v after=%+v", before, got)
	}
	if err := lifecycle.Resolve(permit, ControllerLifecycleExecutionCancelled, 5); err != nil {
		t.Fatalf("cancel/drain admitted predecessor: %v", err)
	}
	restart, err := lifecycle.ClaimRestartAfterUSBReset(6)
	if err != nil {
		t.Fatal(err)
	}
	requireLifecycleAction(t, restart, 0, 3, ControllerLifecycleGateNormalUpstream)
	if err := lifecycle.Resolve(permit, ControllerLifecycleDelivered, 6); !errors.Is(err, ErrInvalidLifecycleClaim) {
		t.Fatalf("late predecessor completion error = %v", err)
	}
}

func TestControllerLifecycleMetadataFenceNeverReusedAcrossReset(t *testing.T) {
	lifecycle := NewControllerLifecycle(0)
	predecessor := claimLifecycleCommand(t, &lifecycle, 0, metadataRequestCommand())
	predecessorFence, ok := predecessor.MetadataTransferFence()
	if !ok {
		t.Fatal("predecessor metadata fence unavailable")
	}
	admitLifecycleAction(t, &lifecycle, predecessor, 0)
	if err := lifecycle.Resolve(predecessor, ControllerLifecycleDeliveryFailed, 1); err != nil {
		t.Fatal(err)
	}

	restart, err := lifecycle.ClaimRestartAfterUSBReset(2)
	if err != nil {
		t.Fatal(err)
	}
	for cursor := 0; cursor < 3; cursor++ {
		deliverLifecycleAction(t, &lifecycle, restart, uint64(3+cursor*2))
		if cursor < 2 {
			restart = nextLifecycleAction(t, &lifecycle, uint64(4+cursor*2))
		}
	}

	successor := claimLifecycleCommand(t, &lifecycle, 8, metadataRequestCommand())
	successorFence, ok := successor.MetadataTransferFence()
	if !ok {
		t.Fatal("successor metadata fence unavailable")
	}
	if successorFence.TransportGeneration != predecessorFence.TransportGeneration+1 ||
		successorFence.TransferGeneration != predecessorFence.TransferGeneration+1 {
		t.Fatalf("metadata fence reused: predecessor=%+v successor=%+v",
			predecessorFence, successorFence)
	}
	deliverLifecycleAction(t, &lifecycle, successor, 9)
	before := lifecycle.Snapshot()
	for _, stale := range []ControllerMetadataTransferFence{
		predecessorFence,
		{TransportGeneration: predecessorFence.TransportGeneration,
			TransferGeneration: successorFence.TransferGeneration},
		{TransportGeneration: successorFence.TransportGeneration,
			TransferGeneration: predecessorFence.TransferGeneration},
	} {
		if err := lifecycle.MetadataTransferSucceeded(10, stale); !errors.Is(err, ErrInvalidTransferGeneration) {
			t.Fatalf("stale metadata success fence %+v error = %v", stale, err)
		}
		if _, err := lifecycle.ClaimMetadataTransferFailed(10, stale); !errors.Is(err, ErrInvalidTransferGeneration) {
			t.Fatalf("stale metadata failure fence %+v error = %v", stale, err)
		}
	}
	if got := lifecycle.Snapshot(); got != before {
		t.Fatalf("stale metadata callback changed lifecycle: before=%+v after=%+v", before, got)
	}
	if err := lifecycle.MetadataTransferSucceeded(10, successorFence); err != nil {
		t.Fatal(err)
	}
	if lifecycle.State() != ControllerLifecycleIdle {
		t.Fatalf("successor metadata success state = %d", lifecycle.State())
	}
}

func TestControllerLifecycleMetadataFaultArrivalAfterHelloAction(t *testing.T) {
	lifecycle := NewControllerLifecycle(0)
	begin := claimLifecycleCommand(t, &lifecycle, 0, metadataRequestCommand())
	fence, _ := begin.MetadataTransferFence()
	deliverLifecycleAction(t, &lifecycle, begin, 0)
	fault, err := lifecycle.ClaimMetadataTransferFailed(1000, fence)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.State() != ControllerLifecycleMetadata {
		t.Fatal("fault claim advanced to Arrival")
	}
	admitLifecycleAction(t, &lifecycle, fault, 1000)
	if err := lifecycle.Resolve(fault, ControllerLifecycleDeliveryFailed, 1001); err != nil {
		t.Fatal(err)
	}
	retry, err := lifecycle.ClaimRetry(1002)
	if err != nil {
		t.Fatal(err)
	}
	deliverLifecycleAction(t, &lifecycle, retry, 1003)
	if lifecycle.State() != ControllerLifecycleArrival {
		t.Fatalf("fault final state = %d", lifecycle.State())
	}
}

func TestControllerLifecycleSaturatedDeliveredResolutionAndEpochPreflight(t *testing.T) {
	maximum := ^uint64(0)
	lifecycle := NewControllerLifecycle(maximum - 10)
	hello, present, err := lifecycle.ClaimPoll(maximum - 10)
	if err != nil || !present {
		t.Fatalf("Hello claim = (%t, %v)", present, err)
	}
	admitLifecycleAction(t, &lifecycle, hello, maximum-10)
	if err := lifecycle.Resolve(hello, ControllerLifecycleDelivered, maximum-20); err != nil {
		t.Fatalf("admitted delivered resolution failed: %v", err)
	}
	if got := lifecycle.Snapshot(); got.PendingTransition || got.State != ControllerLifecycleArrival {
		t.Fatalf("saturated resolution snapshot = %+v", got)
	}
	if _, present, err := lifecycle.ClaimPoll(maximum - 1); err != nil || present {
		t.Fatalf("pre-saturated cadence = (%t, %v)", present, err)
	}
	if _, present, err := lifecycle.ClaimPoll(maximum); err != nil || !present {
		t.Fatalf("saturated cadence = (%t, %v)", present, err)
	}

	blocked := NewControllerLifecycle(0)
	blocked.nextTransitionEpoch = maximum
	if _, _, err := blocked.ClaimPoll(0); !errors.Is(err, ErrInvalidTransferEpoch) {
		t.Fatalf("transition epoch overflow error = %v", err)
	}
	if blocked.Snapshot().PendingTransition {
		t.Fatal("epoch preflight created transition")
	}

	interrupt := NewControllerLifecycle(0)
	predecessor, present, err := interrupt.ClaimPoll(0)
	if err != nil || !present {
		t.Fatalf("predecessor Hello claim = (%t, %v)", present, err)
	}
	interrupt.nextTransitionEpoch = maximum
	before := interrupt.Snapshot()
	if _, err := interrupt.ClaimRestartAfterUSBReset(0); !errors.Is(err, ErrInvalidTransferEpoch) {
		t.Fatalf("reset transition epoch overflow error = %v", err)
	}
	if got := interrupt.Snapshot(); got != before {
		t.Fatalf("reset epoch preflight changed predecessor: before=%+v after=%+v", before, got)
	}
	if ok, err := interrupt.Admit(predecessor, 0); err != nil || !ok {
		t.Fatalf("predecessor claim invalidated by failed reset = (%t, %v)", ok, err)
	}

	metadataOverflow := NewControllerLifecycle(0)
	metadataOverflow.core.state = ControllerLifecycleIdle
	metadataOverflow.core.metadataTransferGeneration = maximum
	metadataOverflow.nextMetadataTransferGeneration = maximum
	metadataBefore := metadataOverflow
	if _, _, err := metadataOverflow.ClaimHostCommand(0, metadataRequestCommand()); !errors.Is(err, ErrInvalidTransferGeneration) {
		t.Fatalf("metadata generation overflow error = %v", err)
	}
	if metadataOverflow != metadataBefore {
		t.Fatal("metadata generation overflow changed lifecycle")
	}
}

func TestControllerLifecycleErrorsAndPendingTransitionAreAtomic(t *testing.T) {
	var zero ControllerLifecycle
	if _, _, err := zero.ClaimPoll(0); !errors.Is(err, ErrUninitializedLifecycle) {
		t.Fatalf("zero ClaimPoll error = %v", err)
	}
	lifecycle := NewControllerLifecycle(100)
	before := lifecycle
	if _, _, err := lifecycle.ClaimHostCommand(200, setStateCommand(1, SetDeviceStateStop)); !errors.Is(err, ErrUnexpectedLifecycleCommand) {
		t.Fatalf("unexpected Stop error = %v", err)
	}
	if lifecycle != before {
		t.Fatal("unexpected command mutated lifecycle")
	}
	if _, err := lifecycle.ClaimNextAction(100); !errors.Is(err, ErrLifecycleNoPendingAction) {
		t.Fatalf("no pending action error = %v", err)
	}
	claim, present, err := lifecycle.ClaimPoll(100)
	if err != nil || !present {
		t.Fatalf("Hello claim = (%t, %v)", present, err)
	}
	if _, _, err := lifecycle.ClaimHostCommand(100, metadataRequestCommand()); !errors.Is(err, ErrLifecycleTransitionPending) {
		t.Fatalf("overtaking command error = %v", err)
	}
	if err := lifecycle.Resolve(claim, ControllerLifecycleDelivered, 100); !errors.Is(err, ErrLifecycleClaimNotAdmitted) {
		t.Fatalf("unadmitted delivery error = %v", err)
	}
	if lifecycle.State() != ControllerLifecycleArrival {
		t.Fatal("unadmitted delivery changed state")
	}
}

func TestControllerLifecycleHotPathAllocatesZero(t *testing.T) {
	if allocs := testing.AllocsPerRun(1000, func() {
		lifecycle := NewControllerLifecycle(0)
		claim, present, err := lifecycle.ClaimPoll(0)
		if err != nil || !present {
			panic("Hello claim")
		}
		if ok, err := lifecycle.Admit(claim, 0); err != nil || !ok {
			panic("Hello admission")
		}
		if err := lifecycle.Resolve(claim, ControllerLifecycleDelivered, 0); err != nil {
			panic(err)
		}
		claim, present, err = lifecycle.ClaimHostCommand(1, metadataRequestCommand())
		if err != nil || !present {
			panic("metadata claim")
		}
		metadataFence, ok := claim.MetadataTransferFence()
		if !ok {
			panic("metadata fence")
		}
		if ok, err := lifecycle.Admit(claim, 1); err != nil || !ok {
			panic("metadata admission")
		}
		if err := lifecycle.Resolve(claim, ControllerLifecycleDelivered, 1); err != nil {
			panic(err)
		}
		if err := lifecycle.MetadataTransferSucceeded(2, metadataFence); err != nil {
			panic(err)
		}
		claim, present, err = lifecycle.ClaimHostCommand(3, setStateCommand(2, SetDeviceStateStart))
		if err != nil || !present {
			panic("START claim")
		}
		for index := 0; index < 3; index++ {
			if ok, err := lifecycle.Admit(claim, 3); err != nil || !ok {
				panic("START admission")
			}
			if err := lifecycle.Resolve(claim, ControllerLifecycleDelivered, 3); err != nil {
				panic(err)
			}
			if index < 2 {
				claim, err = lifecycle.ClaimNextAction(3)
				if err != nil {
					panic(err)
				}
			}
		}
	}); allocs != 0 {
		t.Fatalf("lifecycle hot-path allocations = %v, want 0", allocs)
	}
}

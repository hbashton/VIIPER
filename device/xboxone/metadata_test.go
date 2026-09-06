package xboxone

import (
	"errors"
	"testing"
)

func newTestMetadataTransfer(
	t *testing.T,
	blob []byte,
	sequence uint8,
	generation uint64,
	epoch uint64,
	nowMS uint64,
) MetadataTransfer {
	t.Helper()
	profile := testOnlySyntheticControllerProfile(t)
	metadata, err := profile.BindExternallyCompiledMetadata(blob)
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := profile.NewMetadataTransfer(metadata, sequence, generation, epoch, nowMS)
	if err != nil {
		t.Fatal(err)
	}
	return transfer
}

func admitMetadataClaim(
	t *testing.T,
	transfer *MetadataTransfer,
	claim MetadataPacketClaim,
	dst []byte,
	nowMS uint64,
) MetadataPacketAdmission {
	t.Helper()
	admission, err := transfer.AdmitAndCopy(claim, dst, nowMS)
	if err != nil {
		t.Fatalf("AdmitAndCopy: %v", err)
	}
	return admission
}

func deliverMetadataClaim(
	t *testing.T,
	transfer *MetadataTransfer,
	claim MetadataPacketClaim,
	dst []byte,
	nowMS uint64,
) MetadataPacketAdmission {
	t.Helper()
	admission := admitMetadataClaim(t, transfer, claim, dst, nowMS)
	if err := transfer.Resolve(claim, MetadataPacketDelivered, nowMS); err != nil {
		t.Fatalf("Resolve delivered: %v", err)
	}
	return admission
}

func metadataACK(
	t *testing.T,
	transfer *MetadataTransfer,
	contiguous uint16,
	nowMS uint64,
) (ReliableAcknowledgement, ReliableAcknowledgementDisposition) {
	t.Helper()
	ack, ok := transfer.AcknowledgementIdentity()
	if !ok {
		t.Fatal("no acknowledgement identity")
	}
	ack.ContiguousPayloadBytes = contiguous
	disposition, err := transfer.Acknowledge(ack, nowMS)
	if err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	return ack, disposition
}

func TestMetadataSinglePacketOwnsBlobAndFinalAdmissionCopiesExactBytes(t *testing.T) {
	blob := []byte{0xaa, 0xbb, 0xcc}
	transfer := newTestMetadataTransfer(t, blob, 0x33, 7, 11, 100)
	blob[0], blob[1], blob[2] = 1, 2, 3

	claim, err := transfer.Claim(100)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Kind() != MetadataPacketSingle || claim.Size() != 7 {
		t.Fatalf("claim = kind %d size %d", claim.Kind(), claim.Size())
	}
	var dst [7]byte
	for index := range dst {
		dst[index] = 0xa5
	}
	admission := admitMetadataClaim(t, &transfer, claim, dst[:], 101)
	want := [7]byte{0x04, 0x20, 0x33, 0x03, 0xaa, 0xbb, 0xcc}
	if dst != want || admission.Kind() != MetadataPacketSingle || admission.Size() != len(want) {
		t.Fatalf("admission = kind %d size %d bytes % x; want % x",
			admission.Kind(), admission.Size(), dst, want)
	}
	if got := transfer.Snapshot(); got.Offset != 0 || got.Done || !got.ClaimAdmitted {
		t.Fatalf("admission committed state: %+v", got)
	}
	if err := transfer.Resolve(claim, MetadataPacketDelivered, 102); err != nil {
		t.Fatal(err)
	}
	if got := transfer.Snapshot(); got.Offset != 3 || !got.Done || got.ClaimOutstanding {
		t.Fatalf("delivered snapshot = %+v", got)
	}
}

func TestMetadataResetBeforeAdmissionInvalidatesClaimWithoutCopy(t *testing.T) {
	transfer := newTestMetadataTransfer(t, make([]byte, 61), 1, 4, 8, 0)
	claim, err := transfer.Claim(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := transfer.Reset(2, 5, 9, 1); err != nil {
		t.Fatal(err)
	}
	var dst [64]byte
	for index := range dst {
		dst[index] = 0xa5
	}
	want := dst
	if _, err := transfer.AdmitAndCopy(claim, dst[:], 1); !errors.Is(err, ErrInvalidMetadataClaim) {
		t.Fatalf("stale admission error = %v", err)
	}
	if dst != want {
		t.Fatalf("stale admission mutated destination: % x", dst)
	}
	if got := transfer.Snapshot(); got.Generation != 5 || got.TransferEpoch != 9 || got.Offset != 0 {
		t.Fatalf("reset snapshot = %+v", got)
	}
}

func TestMetadataWriteFailureAndDeferRetryPrivateAdmittedBytes(t *testing.T) {
	blob := make([]byte, 61)
	for index := range blob {
		blob[index] = byte(index)
	}
	transfer := newTestMetadataTransfer(t, blob, 0x12, 1, 1, 0)
	claim, err := transfer.Claim(0)
	if err != nil {
		t.Fatal(err)
	}
	var first [64]byte
	admitMetadataClaim(t, &transfer, claim, first[:], 0)
	want := first
	for index := range first {
		first[index] ^= 0xff
	}
	if err := transfer.Resolve(claim, MetadataPacketDeliveryFailed, 1); err != nil {
		t.Fatal(err)
	}
	if got := transfer.Snapshot(); got.Offset != 0 || !got.RetryPending {
		t.Fatalf("failure snapshot = %+v", got)
	}
	retry, err := transfer.ClaimRetry(2)
	if err != nil {
		t.Fatal(err)
	}
	var second [64]byte
	for index := range second {
		second[index] = 0x5a
	}
	admitMetadataClaim(t, &transfer, retry, second[:], 2)
	if second != want {
		t.Fatalf("write-failure retry = % x, want % x", second, want)
	}
	if err := transfer.Resolve(retry, MetadataPacketDeferred, 3); err != nil {
		t.Fatal(err)
	}
	retry, err = transfer.ClaimRetry(4)
	if err != nil {
		t.Fatal(err)
	}
	var third [64]byte
	deliverMetadataClaim(t, &transfer, retry, third[:], 4)
	if third != want {
		t.Fatalf("deferred retry = % x, want % x", third, want)
	}
	if got := transfer.Snapshot(); got.Offset != 58 || !got.AwaitingAcknowledgement {
		t.Fatalf("delivered retry snapshot = %+v", got)
	}
}

func TestMetadataFragmentedHandshakeIsByteExact(t *testing.T) {
	blob := make([]byte, 61)
	for index := range blob {
		blob[index] = byte(index)
	}
	transfer := newTestMetadataTransfer(t, blob, 0x12, 9, 12, 0)

	claim, err := transfer.Claim(0)
	if err != nil {
		t.Fatal(err)
	}
	var initial [64]byte
	deliverMetadataClaim(t, &transfer, claim, initial[:], 0)
	if got := [6]byte(initial[:6]); got != [6]byte{0x04, 0xf0, 0x12, 0x3a, 0xbd, 0x00} {
		t.Fatalf("initial header = % x", got)
	}
	if _, disposition := metadataACK(t, &transfer, 58, 1); disposition != ReliableAcknowledgementProgress {
		t.Fatalf("initial ACK disposition = %d", disposition)
	}

	claim, err = transfer.Claim(2)
	if err != nil {
		t.Fatal(err)
	}
	var final [9]byte
	deliverMetadataClaim(t, &transfer, claim, final[:], 2)
	wantFinal := [9]byte{0x04, 0xb0, 0x12, 0x03, 0xba, 0x00, 58, 59, 60}
	if final != wantFinal {
		t.Fatalf("final = % x, want % x", final, wantFinal)
	}
	metadataACK(t, &transfer, 61, 3)

	claim, err = transfer.Claim(3)
	if err != nil {
		t.Fatal(err)
	}
	var complete [6]byte
	deliverMetadataClaim(t, &transfer, claim, complete[:], 3)
	wantComplete := [6]byte{0x04, 0xa0, 0x12, 0x00, 0xbd, 0x00}
	if complete != wantComplete || !transfer.Done() {
		t.Fatalf("complete = % x done=%t", complete, transfer.Done())
	}
}

func TestMetadataAcknowledgementPreviewIsPureAndMatchesDeliveredEffect(t *testing.T) {
	transfer := newTestMetadataTransfer(t, make([]byte, 61), 0x12, 9, 12, 0)
	claim, err := transfer.Claim(0)
	if err != nil {
		t.Fatal(err)
	}
	var initial [64]byte
	deliverMetadataClaim(t, &transfer, claim, initial[:], 0)
	ack, ok := transfer.AcknowledgementIdentity()
	if !ok {
		t.Fatal("missing acknowledgement identity")
	}
	ack.ContiguousPayloadBytes = 58
	before := transfer.Snapshot()
	disposition, err := transfer.PreviewAcknowledgement(ack, 1)
	if err != nil || disposition != ReliableAcknowledgementProgress {
		t.Fatalf("preview = (%d, %v)", disposition, err)
	}
	if after := transfer.Snapshot(); after != before {
		t.Fatalf("preview mutated transfer: %+v -> %+v", before, after)
	}
	disposition, err = transfer.Acknowledge(ack, 1)
	if err != nil || disposition != ReliableAcknowledgementProgress {
		t.Fatalf("apply = (%d, %v)", disposition, err)
	}
	if after := transfer.Snapshot(); after.AcknowledgedEnd != 58 ||
		after.AwaitingAcknowledgement {
		t.Fatalf("delivered ACK state = %+v", after)
	}
}

func TestMetadataFinalAdmissionUpgradesDeferredACMEAfterCadence(t *testing.T) {
	transfer := newTestMetadataTransfer(t, make([]byte, 180), 0x44, 1, 1, 0)
	initial, err := transfer.Claim(0)
	if err != nil {
		t.Fatal(err)
	}
	var packet [64]byte
	deliverMetadataClaim(t, &transfer, initial, packet[:], 0)
	metadataACK(t, &transfer, 58, 0)

	claim, err := transfer.Claim(59)
	if err != nil {
		t.Fatal(err)
	}
	if claim.AcknowledgementRequested() {
		t.Fatal("59 ms selection unexpectedly requested ACK")
	}
	if err := transfer.Resolve(claim, MetadataPacketDeferred, 59); err != nil {
		t.Fatal(err)
	}
	retry, err := transfer.ClaimRetry(101)
	if err != nil {
		t.Fatal(err)
	}
	if retry.AcknowledgementRequested() {
		t.Fatal("never-admitted retry was rewritten before final admission")
	}
	admission := admitMetadataClaim(t, &transfer, retry, packet[:], 101)
	if !admission.AcknowledgementRequested() || packet[1] != 0xb0 {
		t.Fatalf("late final admission omitted ACME: admission=%+v header=% x",
			admission, packet[:6])
	}
	if packet[2] != 0x44 || packet[4] != 0xba || packet[5] != 0x00 {
		t.Fatalf("ACME upgrade changed identity/offset: % x", packet[:6])
	}
	if err := transfer.Resolve(retry, MetadataPacketDelivered, 101); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataUnsolicitedGapProgressDelayedAndDuplicateACKs(t *testing.T) {
	transfer := newTestMetadataTransfer(t, make([]byte, 240), 3, 5, 7, 0)
	var packet [64]byte
	claim, _ := transfer.Claim(0)
	deliverMetadataClaim(t, &transfer, claim, packet[:], 0)
	metadataACK(t, &transfer, 58, 1)

	claim, _ = transfer.Claim(2)
	deliverMetadataClaim(t, &transfer, claim, packet[:], 2)
	if transfer.AwaitingAcknowledgement() {
		t.Fatal("non-ACME middle unexpectedly armed deadline")
	}
	staleSelection, err := transfer.Claim(3)
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := transfer.AcknowledgementIdentity()
	if !ok {
		t.Fatal("missing in-flight ACK identity")
	}
	ack.ContiguousPayloadBytes = 58
	disposition, err := transfer.Acknowledge(ack, 3)
	if err != nil || disposition != ReliableAcknowledgementRewind {
		t.Fatalf("gap ACK = (%d, %v), want rewind", disposition, err)
	}
	if got := transfer.Snapshot(); got.Offset != 58 || got.SentEnd != 58 || got.AcknowledgedEnd != 58 || got.ClaimOutstanding {
		t.Fatalf("gap rewind snapshot = %+v", got)
	}
	var untouched [64]byte
	for index := range untouched {
		untouched[index] = 0xa5
	}
	wantUntouched := untouched
	if _, err := transfer.AdmitAndCopy(staleSelection, untouched[:], 3); !errors.Is(err, ErrInvalidMetadataClaim) {
		t.Fatalf("rewound selection error = %v", err)
	}
	if untouched != wantUntouched {
		t.Fatal("rewound selection mutated destination")
	}

	claim, _ = transfer.Claim(4)
	deliverMetadataClaim(t, &transfer, claim, packet[:], 4)
	ack, _ = transfer.AcknowledgementIdentity()
	ack.ContiguousPayloadBytes = 116
	disposition, err = transfer.Acknowledge(ack, 5)
	if err != nil || disposition != ReliableAcknowledgementProgress {
		t.Fatalf("unsolicited progress ACK = (%d, %v)", disposition, err)
	}
	disposition, err = transfer.Acknowledge(ack, 6)
	if err != nil || disposition != ReliableAcknowledgementDuplicate {
		t.Fatalf("duplicate ACK = (%d, %v)", disposition, err)
	}

	claim, _ = transfer.Claim(7)
	deliverMetadataClaim(t, &transfer, claim, packet[:], 7)
	delayed := ack
	delayed.ContiguousPayloadBytes = 58
	before := transfer.Snapshot()
	if _, err := transfer.Acknowledge(delayed, 8); !errors.Is(err, ErrInvalidAcknowledgement) {
		t.Fatalf("delayed predecessor ACK error = %v", err)
	}
	if got := transfer.Snapshot(); got.Offset != before.Offset || got.AcknowledgedEnd != before.AcknowledgedEnd {
		t.Fatalf("delayed ACK changed progress: %+v -> %+v", before, got)
	}
	gap := ack
	gap.ContiguousPayloadBytes = 116
	disposition, err = transfer.Acknowledge(gap, 9)
	if err != nil || disposition != ReliableAcknowledgementRewind {
		t.Fatalf("same-progress gap ACK = (%d, %v)", disposition, err)
	}
	disposition, err = transfer.Acknowledge(gap, 10)
	if err != nil || disposition != ReliableAcknowledgementDuplicate {
		t.Fatalf("post-rewind duplicate ACK = (%d, %v)", disposition, err)
	}
}

func TestMetadataACKSerializesAgainstAdmittedWrite(t *testing.T) {
	transfer := newTestMetadataTransfer(t, make([]byte, 180), 1, 1, 1, 0)
	var packet [64]byte
	claim, _ := transfer.Claim(0)
	deliverMetadataClaim(t, &transfer, claim, packet[:], 0)
	metadataACK(t, &transfer, 58, 1)
	claim, _ = transfer.Claim(2)
	admitMetadataClaim(t, &transfer, claim, packet[:], 2)
	ack, _ := transfer.AcknowledgementIdentity()
	ack.ContiguousPayloadBytes = 58
	if _, err := transfer.Acknowledge(ack, 2); !errors.Is(err, ErrMetadataClaimOutstanding) {
		t.Fatalf("ACK during admitted write error = %v", err)
	}
	if err := transfer.Resolve(claim, MetadataPacketDelivered, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := transfer.Acknowledge(ack, 3); err != nil {
		t.Fatalf("ACK after write resolution: %v", err)
	}
}

func TestMetadataProgressACKSurvivesOutstandingSelectionAndRetry(t *testing.T) {
	for _, test := range []struct {
		name      string
		makeRetry bool
	}{
		{name: "selection"},
		{name: "retry", makeRetry: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			transfer := newTestMetadataTransfer(t, make([]byte, 240), 1, 1, 1, 0)
			var packet [64]byte
			claim, _ := transfer.Claim(0)
			deliverMetadataClaim(t, &transfer, claim, packet[:], 0)
			metadataACK(t, &transfer, 58, 1)
			claim, _ = transfer.Claim(2)
			deliverMetadataClaim(t, &transfer, claim, packet[:], 2)

			claim, _ = transfer.Claim(3)
			if test.makeRetry {
				if err := transfer.Resolve(claim, MetadataPacketDeferred, 3); err != nil {
					t.Fatal(err)
				}
			}
			ack, ok := transfer.AcknowledgementIdentity()
			if !ok {
				t.Fatal("missing unsolicited ACK identity")
			}
			ack.ContiguousPayloadBytes = 116
			if disposition, err := transfer.Acknowledge(ack, 4); err != nil ||
				disposition != ReliableAcknowledgementProgress {
				t.Fatalf("progress ACK = (%d, %v)", disposition, err)
			}
			if test.makeRetry {
				var err error
				claim, err = transfer.ClaimRetry(5)
				if err != nil {
					t.Fatal(err)
				}
			}
			deliverMetadataClaim(t, &transfer, claim, packet[:], 5)
			if got := transfer.Snapshot(); got.AcknowledgedEnd != 116 || got.Offset != 174 {
				t.Fatalf("delivery restored stale ACK progress: %+v", got)
			}
		})
	}
}

func TestMetadataACKIdentityTimeoutResetAndSaturatedResolution(t *testing.T) {
	maximum := ^uint64(0)
	transfer := newTestMetadataTransfer(t, make([]byte, 61), 1, 4, maximum, maximum-10)
	claim, err := transfer.Claim(maximum - 10)
	if err != nil {
		t.Fatal(err)
	}
	var packet [64]byte
	admitMetadataClaim(t, &transfer, claim, packet[:], maximum-10)
	if err := transfer.Resolve(claim, MetadataPacketDelivered, maximum-20); err != nil {
		t.Fatalf("admitted delivered resolution failed: %v", err)
	}
	if got := transfer.Snapshot(); got.AcknowledgementDeadlineMS != maximum || !got.AwaitingAcknowledgement {
		t.Fatalf("saturated deadline snapshot = %+v", got)
	}
	ack, _ := transfer.PendingAcknowledgement()
	for _, invalid := range []ReliableAcknowledgement{
		{TransferGeneration: 3, TransferEpoch: maximum, MessageNumber: 4, Sequence: 1, ContiguousPayloadBytes: 58},
		{TransferGeneration: 4, TransferEpoch: maximum - 1, MessageNumber: 4, Sequence: 1, ContiguousPayloadBytes: 58},
		{TransferGeneration: 4, TransferEpoch: maximum, MessageNumber: 5, Sequence: 1, ContiguousPayloadBytes: 58},
		{TransferGeneration: 4, TransferEpoch: maximum, MessageNumber: 4, Sequence: 2, ContiguousPayloadBytes: 58},
	} {
		if _, err := transfer.Acknowledge(invalid, maximum-9); !errors.Is(err, ErrInvalidAcknowledgement) {
			t.Errorf("invalid ACK %+v error = %v", invalid, err)
		}
	}
	if err := transfer.Poll(maximum - 1); err != nil {
		t.Fatalf("pre-saturated-deadline Poll: %v", err)
	}
	if err := transfer.Poll(maximum); !errors.Is(err, ErrReliableTransferTimeout) {
		t.Fatalf("saturated deadline Poll error = %v", err)
	}
	if _, err := transfer.Acknowledge(ack, maximum); !errors.Is(err, ErrMetadataTransferFaulted) {
		t.Fatalf("post-timeout ACK error = %v", err)
	}
	if err := transfer.Reset(2, 5, 1, maximum); !errors.Is(err, ErrInvalidTransferEpoch) {
		t.Fatalf("maximum-epoch reset error = %v", err)
	}
}

func TestMetadataSeamsFailClosedAndShortAdmissionDoesNotMutate(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	if _, err := profile.BindExternallyCompiledMetadata(nil); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("empty metadata error = %v", err)
	}
	metadata, err := profile.BindExternallyCompiledMetadata(make([]byte, 61))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.NewMetadataTransfer(metadata, 0, 1, 1, 0); !errors.Is(err, ErrReservedSequence) {
		t.Fatalf("zero sequence error = %v", err)
	}
	if _, err := profile.NewMetadataTransfer(metadata, 1, 0, 1, 0); !errors.Is(err, ErrInvalidTransferGeneration) {
		t.Fatalf("zero generation error = %v", err)
	}
	if _, err := profile.NewMetadataTransfer(metadata, 1, 1, 0, 0); !errors.Is(err, ErrInvalidTransferEpoch) {
		t.Fatalf("zero epoch error = %v", err)
	}

	transfer, err := profile.NewMetadataTransfer(metadata, 1, 1, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := transfer.Claim(0)
	if err != nil {
		t.Fatal(err)
	}
	var short [63]byte
	for index := range short {
		short[index] = 0xa5
	}
	want := short
	if _, err := transfer.AdmitAndCopy(claim, short[:], 0); !errors.Is(err, ErrInvalidLength) {
		t.Fatalf("short destination error = %v", err)
	}
	if short != want {
		t.Fatal("short admission mutated destination")
	}
	if got := transfer.Snapshot(); got.Offset != 0 || got.ClaimAdmitted {
		t.Fatalf("short admission changed state: %+v", got)
	}
}

func TestMetadataTransferHotPathAllocatesZero(t *testing.T) {
	profile := testOnlySyntheticControllerProfile(t)
	metadata, err := profile.BindExternallyCompiledMetadata(make([]byte, 61))
	if err != nil {
		t.Fatal(err)
	}
	var initial [64]byte
	var final [9]byte
	var complete [6]byte
	if allocs := testing.AllocsPerRun(1000, func() {
		transfer, err := profile.NewMetadataTransfer(metadata, 1, 1, 1, 0)
		if err != nil {
			panic(err)
		}
		claim, err := transfer.Claim(0)
		if err != nil {
			panic(err)
		}
		if _, err := transfer.AdmitAndCopy(claim, initial[:], 0); err != nil {
			panic(err)
		}
		if err := transfer.Resolve(claim, MetadataPacketDelivered, 0); err != nil {
			panic(err)
		}
		ack, _ := transfer.PendingAcknowledgement()
		ack.ContiguousPayloadBytes = 58
		if _, err := transfer.Acknowledge(ack, 1); err != nil {
			panic(err)
		}
		claim, err = transfer.Claim(1)
		if err != nil {
			panic(err)
		}
		if _, err := transfer.AdmitAndCopy(claim, final[:], 1); err != nil {
			panic(err)
		}
		if err := transfer.Resolve(claim, MetadataPacketDelivered, 1); err != nil {
			panic(err)
		}
		ack, _ = transfer.PendingAcknowledgement()
		ack.ContiguousPayloadBytes = 61
		if _, err := transfer.Acknowledge(ack, 2); err != nil {
			panic(err)
		}
		claim, err = transfer.Claim(2)
		if err != nil {
			panic(err)
		}
		if _, err := transfer.AdmitAndCopy(claim, complete[:], 2); err != nil {
			panic(err)
		}
		if err := transfer.Resolve(claim, MetadataPacketDelivered, 2); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("metadata transfer hot-path allocations = %v, want 0", allocs)
	}
}

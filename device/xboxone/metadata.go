package xboxone

import "fmt"

const (
	MetadataFragmentHeaderSize                    = 6
	MetadataFragmentPayloadCapacity               = 58
	MetadataMaximumBoundLength                    = 0x3fff
	ReliableACKRequestIntervalMilliseconds uint64 = 60
	ReliableACKTimeoutMilliseconds         uint64 = 1000
)

// BoundCompiledMetadata is an identity-bound metadata seam. External binding
// owns a private opaque copy and does not parse or semantically validate it.
// The separate official compiler may attach its private validated variant.
type boundMetadataIssuance struct {
	marker byte
}

type BoundCompiledMetadata struct {
	identity        ControllerIdentity
	profileIssuance *controllerProfileIssuance
	issuance        *boundMetadataIssuance
	data            []byte
	// officialGamepadVariant is non-zero only when the strict in-package
	// official compiler produced and independently validated data. Opaque
	// externally compiled bytes never acquire semantic input capabilities by
	// merely matching an official byte vector.
	officialGamepadVariant OfficialGamepadMetadataVariant
	valid                  bool
}

func (metadata BoundCompiledMetadata) validateInputReport(
	report GamepadInputReportV1,
) error {
	switch metadata.officialGamepadVariant {
	case 0, OfficialGamepadMetadataBase:
		return report.Validate()
	case OfficialGamepadMetadataConsoleFunctionMap:
		if err := report.State.Validate(); err != nil {
			return err
		}
		if report.State.Guide {
			return ErrGuideRequiresStatusMessage
		}
		return nil
	default:
		return ErrInvalidMetadata
	}
}

func (metadata BoundCompiledMetadata) inputMessageSize() (int, error) {
	switch metadata.officialGamepadVariant {
	case 0, OfficialGamepadMetadataBase:
		return GamepadInputMessageSize, nil
	case OfficialGamepadMetadataConsoleFunctionMap:
		return ConsoleFunctionMapGamepadInputMessageSize, nil
	default:
		return 0, ErrInvalidMetadata
	}
}

func (metadata BoundCompiledMetadata) encodeInputMessageInto(
	dst []byte,
	sequence uint8,
	report GamepadInputReportV1,
) error {
	if err := metadata.validateInputReport(report); err != nil {
		return err
	}
	switch metadata.officialGamepadVariant {
	case 0, OfficialGamepadMetadataBase:
		return EncodeGamepadInputMessageInto(dst, sequence, report)
	case OfficialGamepadMetadataConsoleFunctionMap:
		return EncodeConsoleFunctionMapGamepadInputMessageInto(
			dst, sequence, report)
	default:
		return ErrInvalidMetadata
	}
}

// BindExternallyCompiledMetadata binds and copies caller-supplied compiled
// bytes to this exact descriptor/Hello identity.
func (profile UnregisteredControllerProfile) BindExternallyCompiledMetadata(
	blob []byte,
) (BoundCompiledMetadata, error) {
	if err := profile.validate(); err != nil {
		return BoundCompiledMetadata{}, err
	}
	if len(blob) == 0 || len(blob) > MetadataMaximumBoundLength {
		return BoundCompiledMetadata{}, fmt.Errorf("%w: length=%d", ErrInvalidMetadata, len(blob))
	}
	owned := make([]byte, len(blob))
	copy(owned, blob)
	return BoundCompiledMetadata{
		identity:        profile.identity,
		profileIssuance: profile.issuance,
		issuance:        &boundMetadataIssuance{marker: 1},
		data:            owned,
		valid:           true,
	}, nil
}

// MetadataPacketKind identifies the exact stage emitted by MetadataTransfer.
type MetadataPacketKind uint8

const (
	MetadataPacketSingle MetadataPacketKind = iota + 1
	MetadataPacketInitialFragment
	MetadataPacketMiddleFragment
	MetadataPacketFinalFragment
	MetadataPacketComplete
)

// MetadataPacketOutcome reports the backend result for one packet admission.
type MetadataPacketOutcome uint8

const (
	MetadataPacketDelivered MetadataPacketOutcome = iota + 1
	MetadataPacketDeferred
	MetadataPacketDeliveryFailed
)

// MetadataPacketClaim is an opaque selection capability. It contains no wire
// buffer; AdmitAndCopy materializes the final private record immediately before
// the serialized backend write.
type MetadataPacketClaim struct {
	owner              *MetadataTransfer
	token              uint64
	transferGeneration uint64
	transferEpoch      uint64
	messageNumber      uint8
	sequence           uint8
	kind               MetadataPacketKind
	size               uint8
	selectedACME       bool
	selectedAtMS       uint64
}

func (claim MetadataPacketClaim) Valid() bool                { return claim.owner != nil && claim.token != 0 }
func (claim MetadataPacketClaim) TransferGeneration() uint64 { return claim.transferGeneration }
func (claim MetadataPacketClaim) TransferEpoch() uint64      { return claim.transferEpoch }
func (claim MetadataPacketClaim) MessageNumber() uint8       { return claim.messageNumber }
func (claim MetadataPacketClaim) Sequence() uint8            { return claim.sequence }
func (claim MetadataPacketClaim) Kind() MetadataPacketKind   { return claim.kind }
func (claim MetadataPacketClaim) Size() int                  { return int(claim.size) }

// AcknowledgementRequested reports the selection-time ACME value. Final
// admission can only upgrade a never-delivered middle fragment when its
// monotonic cadence becomes due.
func (claim MetadataPacketClaim) AcknowledgementRequested() bool { return claim.selectedACME }
func (claim MetadataPacketClaim) SelectedAtMilliseconds() uint64 { return claim.selectedAtMS }

// MetadataPacketAdmission describes the exact private bytes copied by the
// successful final admission operation.
type MetadataPacketAdmission struct {
	size            uint8
	kind            MetadataPacketKind
	acknowledgement bool
}

func (admission MetadataPacketAdmission) Size() int                { return int(admission.size) }
func (admission MetadataPacketAdmission) Kind() MetadataPacketKind { return admission.kind }
func (admission MetadataPacketAdmission) AcknowledgementRequested() bool {
	return admission.acknowledgement
}

// ReliableAcknowledgement is the identity-fenced semantic form of an exact
// Protocol Control ACK. Generation and local reliable-transfer epoch are
// out-of-band transaction identity; message, sequence, contiguous progress,
// and receiver-buffer space are decoded from the official wire body.
type ReliableAcknowledgement struct {
	TransferGeneration           uint64
	TransferEpoch                uint64
	MessageNumber                uint8
	Sequence                     uint8
	ContiguousPayloadBytes       uint16
	ReceiverRemainingBufferBytes uint16
}

// ReliableAcknowledgementDisposition describes the accepted semantic effect.
type ReliableAcknowledgementDisposition uint8

const (
	ReliableAcknowledgementProgress ReliableAcknowledgementDisposition = iota + 1
	ReliableAcknowledgementRewind
	ReliableAcknowledgementDuplicate
)

// MetadataTransferSnapshot exposes committed progress without mutable bytes.
type MetadataTransferSnapshot struct {
	Generation                uint64
	TransferEpoch             uint64
	Offset                    uint16
	SentEnd                   uint16
	AcknowledgedEnd           uint16
	AcknowledgementDeadlineMS uint64
	AwaitingAcknowledgement   bool
	Done                      bool
	Faulted                   bool
	ClaimOutstanding          bool
	ClaimAdmitted             bool
	RetryPending              bool
}

type metadataTransferPostState struct {
	offset            uint16
	sentEnd           uint16
	acknowledgedEnd   uint16
	finalAcknowledged bool
	done              bool
}

type metadataPacketRecord struct {
	wire            [64]byte
	size            uint8
	kind            MetadataPacketKind
	acknowledgement bool
	selectedAtMS    uint64
	post            metadataTransferPostState
}

// MetadataTransfer is a serialized claim/admit/copy/resolve state machine. It
// is transport-neutral and not safe for concurrent use. Offset, completion,
// and ACK state change only after a delivered resolution. It must not be copied
// after its first claim because claims are bound to its address.
type MetadataTransfer struct {
	metadata          BoundCompiledMetadata
	sequence          uint8
	generation        uint64
	transferEpoch     uint64
	offset            uint16
	sentEnd           uint16
	acknowledgedEnd   uint16
	finalAcknowledged bool
	done              bool
	faulted           bool
	valid             bool

	lastNowMS        uint64
	nextACKRequestMS uint64
	ackDeadlineMS    uint64
	ackDeadlineArmed bool

	nextToken         uint64
	hasClaim          bool
	claimAdmitted     bool
	claimAdmittedAtMS uint64
	claimToken        uint64
	claimRecord       metadataPacketRecord
	retryPending      bool
	retryRecord       metadataPacketRecord
}

// NewMetadataTransfer starts one reliable-message epoch for this exact bound
// identity. generation fences transport reset; transferEpoch fences reuse of
// the same message number and sequence by another reliable transfer.
func (profile UnregisteredControllerProfile) NewMetadataTransfer(
	metadata BoundCompiledMetadata,
	sequence uint8,
	generation uint64,
	transferEpoch uint64,
	nowMS uint64,
) (MetadataTransfer, error) {
	if err := profile.validate(); err != nil {
		return MetadataTransfer{}, err
	}
	if !metadata.valid || metadata.issuance == nil || metadata.issuance.marker != 1 ||
		len(metadata.data) == 0 || len(metadata.data) > MetadataMaximumBoundLength {
		return MetadataTransfer{}, ErrInvalidMetadata
	}
	if metadata.identity != profile.identity || metadata.issuance == nil ||
		metadata.profileIssuance != profile.issuance {
		return MetadataTransfer{}, ErrMetadataIdentityMismatch
	}
	if sequence == 0 {
		return MetadataTransfer{}, ErrReservedSequence
	}
	if generation == 0 {
		return MetadataTransfer{}, ErrInvalidTransferGeneration
	}
	if transferEpoch == 0 {
		return MetadataTransfer{}, ErrInvalidTransferEpoch
	}
	return MetadataTransfer{
		metadata: metadata, sequence: sequence, generation: generation,
		transferEpoch: transferEpoch, valid: true, lastNowMS: nowMS,
		nextACKRequestMS: nowMS,
	}, nil
}

func (transfer MetadataTransfer) Snapshot() MetadataTransferSnapshot {
	return MetadataTransferSnapshot{
		Generation: transfer.generation, TransferEpoch: transfer.transferEpoch,
		Offset: transfer.offset, SentEnd: transfer.sentEnd,
		AcknowledgedEnd:           transfer.acknowledgedEnd,
		AcknowledgementDeadlineMS: transfer.ackDeadlineMS,
		AwaitingAcknowledgement:   transfer.ackDeadlineArmed,
		Done:                      transfer.done, Faulted: transfer.faulted,
		ClaimOutstanding: transfer.hasClaim, ClaimAdmitted: transfer.claimAdmitted,
		RetryPending: transfer.retryPending,
	}
}

func (transfer MetadataTransfer) AwaitingAcknowledgement() bool { return transfer.ackDeadlineArmed }
func (transfer MetadataTransfer) Done() bool                    { return transfer.done }
func (transfer MetadataTransfer) Faulted() bool                 { return transfer.faulted }

func (transfer *MetadataTransfer) validateClock(nowMS uint64) error {
	if nowMS < transfer.lastNowMS {
		return fmt.Errorf("%w: now=%d previous=%d",
			ErrNonMonotonicMetadataClock, nowMS, transfer.lastNowMS)
	}
	return nil
}

func saturatingMetadataDeadline(nowMS, deltaMS uint64) uint64 {
	if ^uint64(0)-nowMS < deltaMS {
		return ^uint64(0)
	}
	return nowMS + deltaMS
}

func (transfer *MetadataTransfer) observeAndCheckTimeout(nowMS uint64) error {
	if !transfer.valid {
		return ErrInvalidMetadata
	}
	if err := transfer.validateClock(nowMS); err != nil {
		return err
	}
	transfer.lastNowMS = nowMS
	if transfer.faulted {
		return ErrMetadataTransferFaulted
	}
	if transfer.ackDeadlineArmed && nowMS >= transfer.ackDeadlineMS {
		transfer.faulted = true
		transfer.ackDeadlineArmed = false
		return ErrReliableTransferTimeout
	}
	return nil
}

// Poll observes the monotonic clock and faults exactly at an armed ACK deadline.
func (transfer *MetadataTransfer) Poll(nowMS uint64) error {
	return transfer.observeAndCheckTimeout(nowMS)
}

func (transfer *MetadataTransfer) nextClaimToken() uint64 {
	transfer.nextToken++
	if transfer.nextToken == 0 {
		transfer.nextToken++
	}
	return transfer.nextToken
}

func (transfer *MetadataTransfer) claimFromRecord(record metadataPacketRecord) MetadataPacketClaim {
	token := transfer.nextClaimToken()
	transfer.hasClaim = true
	transfer.claimAdmitted = false
	transfer.claimAdmittedAtMS = 0
	transfer.claimToken = token
	transfer.claimRecord = record
	return MetadataPacketClaim{
		owner: transfer, token: token,
		transferGeneration: transfer.generation, transferEpoch: transfer.transferEpoch,
		messageNumber: messageNumberMetadataRequest, sequence: transfer.sequence,
		kind: record.kind, size: record.size, selectedACME: record.acknowledgement,
		selectedAtMS: record.selectedAtMS,
	}
}

// Claim selects one private packet record without exposing bytes or committing
// progress. A retained retry must be reclaimed with ClaimRetry.
func (transfer *MetadataTransfer) Claim(nowMS uint64) (MetadataPacketClaim, error) {
	if err := transfer.observeAndCheckTimeout(nowMS); err != nil {
		return MetadataPacketClaim{}, err
	}
	if transfer.hasClaim {
		return MetadataPacketClaim{}, ErrMetadataClaimOutstanding
	}
	if transfer.retryPending {
		return MetadataPacketClaim{}, ErrMetadataRetryRequired
	}
	if transfer.done {
		return MetadataPacketClaim{}, ErrMetadataTransferComplete
	}
	if transfer.ackDeadlineArmed {
		return MetadataPacketClaim{}, ErrAcknowledgementRequired
	}
	record, err := transfer.selectPacket(nowMS)
	if err != nil {
		return MetadataPacketClaim{}, err
	}
	return transfer.claimFromRecord(record), nil
}

// ClaimRetry selects the retained payload/offset/sequence of a never-delivered
// packet. Final admission may upgrade only its time-dependent ACME flag.
func (transfer *MetadataTransfer) ClaimRetry(nowMS uint64) (MetadataPacketClaim, error) {
	if err := transfer.observeAndCheckTimeout(nowMS); err != nil {
		return MetadataPacketClaim{}, err
	}
	if transfer.hasClaim {
		return MetadataPacketClaim{}, ErrMetadataClaimOutstanding
	}
	if !transfer.retryPending {
		return MetadataPacketClaim{}, ErrMetadataRetryRequired
	}
	record := transfer.retryRecord
	claim := transfer.claimFromRecord(record)
	transfer.retryPending = false
	return claim, nil
}

func (transfer MetadataTransfer) selectPacket(nowMS uint64) (metadataPacketRecord, error) {
	var record metadataPacketRecord
	record.selectedAtMS = nowMS
	record.post = metadataTransferPostState{
		offset: transfer.offset, sentEnd: transfer.sentEnd,
		acknowledgedEnd:   transfer.acknowledgedEnd,
		finalAcknowledged: transfer.finalAcknowledged, done: transfer.done,
	}
	total := len(transfer.metadata.data)
	if total <= 60 {
		record.kind = MetadataPacketSingle
		record.size = uint8(SinglePacketHeaderSize + total)
		header := SinglePacketHeader{
			DataClass: DataClassCommand, MessageNumber: messageNumberMetadataRequest,
			System: true, Sequence: transfer.sequence, PayloadLength: uint16(total),
		}
		if err := EncodeSinglePacketHeaderInto(record.wire[:SinglePacketHeaderSize], header); err != nil {
			return metadataPacketRecord{}, err
		}
		copy(record.wire[SinglePacketHeaderSize:record.size], transfer.metadata.data)
		record.post.offset = uint16(total)
		record.post.sentEnd = uint16(total)
		record.post.done = true
		return record, nil
	}
	if int(transfer.offset) == total {
		if !transfer.finalAcknowledged {
			return metadataPacketRecord{}, ErrAcknowledgementRequired
		}
		record.kind = MetadataPacketComplete
		record.size = MetadataFragmentHeaderSize
		encodeMetadataCompleteHeader(record.wire[:record.size], transfer.sequence, uint16(total))
		record.post.done = true
		return record, nil
	}

	start := int(transfer.offset)
	payloadLength := total - start
	if payloadLength > MetadataFragmentPayloadCapacity {
		payloadLength = MetadataFragmentPayloadCapacity
	}
	end := start + payloadLength
	record.kind = MetadataPacketMiddleFragment
	if start == 0 {
		record.kind = MetadataPacketInitialFragment
	} else if end == total {
		record.kind = MetadataPacketFinalFragment
	}
	record.size = uint8(MetadataFragmentHeaderSize + payloadLength)
	record.acknowledgement = record.kind == MetadataPacketInitialFragment ||
		record.kind == MetadataPacketFinalFragment || nowMS >= transfer.nextACKRequestMS
	totalLengthOrOffset := uint16(start)
	if record.kind == MetadataPacketInitialFragment {
		totalLengthOrOffset = uint16(total)
	}
	encodeMetadataFragmentHeader(
		record.wire[:MetadataFragmentHeaderSize], transfer.sequence, uint8(payloadLength),
		record.kind == MetadataPacketInitialFragment, record.acknowledgement, totalLengthOrOffset,
	)
	copy(record.wire[MetadataFragmentHeaderSize:record.size], transfer.metadata.data[start:end])
	record.post.offset = uint16(end)
	record.post.sentEnd = uint16(end)
	record.post.finalAcknowledged = false
	return record, nil
}

func (transfer *MetadataTransfer) validateClaim(claim MetadataPacketClaim) error {
	if !transfer.hasClaim || claim.owner != transfer || claim.token == 0 ||
		claim.token != transfer.claimToken || claim.transferGeneration != transfer.generation ||
		claim.transferEpoch != transfer.transferEpoch ||
		claim.messageNumber != messageNumberMetadataRequest || claim.sequence != transfer.sequence ||
		claim.kind != transfer.claimRecord.kind || claim.size != transfer.claimRecord.size ||
		(!transfer.claimAdmitted && claim.selectedACME != transfer.claimRecord.acknowledgement) ||
		claim.selectedAtMS != transfer.claimRecord.selectedAtMS {
		return ErrInvalidMetadataClaim
	}
	return nil
}

// AdmitAndCopy performs the final serialized validity/cadence check and then
// overwrites dst with the exact private packet image. No caller-owned packet
// bytes exist before this boundary. A successful return is consumed
// immediately by the backend write and later resolved exactly once.
func (transfer *MetadataTransfer) AdmitAndCopy(
	claim MetadataPacketClaim,
	dst []byte,
	nowMS uint64,
) (MetadataPacketAdmission, error) {
	if err := transfer.validateClaim(claim); err != nil {
		return MetadataPacketAdmission{}, err
	}
	if transfer.claimAdmitted {
		return MetadataPacketAdmission{}, ErrInvalidMetadataClaim
	}
	if len(dst) < int(transfer.claimRecord.size) {
		return MetadataPacketAdmission{}, exactLengthError(
			"GIP metadata packet destination", len(dst), int(transfer.claimRecord.size))
	}
	if err := transfer.observeAndCheckTimeout(nowMS); err != nil {
		return MetadataPacketAdmission{}, err
	}
	if transfer.claimRecord.kind == MetadataPacketMiddleFragment &&
		!transfer.claimRecord.acknowledgement && nowMS >= transfer.nextACKRequestMS {
		transfer.claimRecord.acknowledgement = true
		transfer.claimRecord.wire[1] |= flagAcknowledge
	}
	copy(dst[:transfer.claimRecord.size], transfer.claimRecord.wire[:transfer.claimRecord.size])
	transfer.claimAdmitted = true
	transfer.claimAdmittedAtMS = nowMS
	return MetadataPacketAdmission{
		size: transfer.claimRecord.size, kind: transfer.claimRecord.kind,
		acknowledgement: transfer.claimRecord.acknowledgement,
	}, nil
}

// Resolve commits only an admitted Delivered outcome. A valid admitted
// Delivered resolution is non-failing: a backward completion timestamp is
// clamped to admission, and an unrepresentable ACK deadline saturates.
func (transfer *MetadataTransfer) Resolve(
	claim MetadataPacketClaim,
	outcome MetadataPacketOutcome,
	completedMS uint64,
) error {
	if err := transfer.validateClaim(claim); err != nil {
		return err
	}
	if outcome != MetadataPacketDelivered && outcome != MetadataPacketDeferred &&
		outcome != MetadataPacketDeliveryFailed {
		return ErrInvalidMetadataOutcome
	}
	if outcome == MetadataPacketDelivered && !transfer.claimAdmitted {
		return ErrMetadataClaimNotAdmitted
	}
	if completedMS < transfer.lastNowMS {
		completedMS = transfer.lastNowMS
	}
	transfer.lastNowMS = completedMS
	if outcome != MetadataPacketDelivered {
		transfer.retryRecord = transfer.claimRecord
		transfer.retryPending = true
		transfer.clearClaim()
		return nil
	}

	post := transfer.claimRecord.post
	transfer.offset = post.offset
	transfer.sentEnd = post.sentEnd
	transfer.acknowledgedEnd = post.acknowledgedEnd
	transfer.finalAcknowledged = post.finalAcknowledged
	transfer.done = post.done
	if transfer.claimRecord.acknowledgement {
		transfer.ackDeadlineMS = saturatingMetadataDeadline(
			completedMS, ReliableACKTimeoutMilliseconds)
		transfer.ackDeadlineArmed = true
	}
	transfer.clearClaim()
	return nil
}

func (transfer *MetadataTransfer) clearClaim() {
	transfer.hasClaim = false
	transfer.claimAdmitted = false
	transfer.claimAdmittedAtMS = 0
	transfer.claimToken = 0
	transfer.claimRecord = metadataPacketRecord{}
}

func (transfer MetadataTransfer) acknowledgementIdentity() (ReliableAcknowledgement, bool) {
	if !transfer.valid || transfer.faulted || transfer.done ||
		len(transfer.metadata.data) <= 60 || transfer.sentEnd == 0 {
		return ReliableAcknowledgement{}, false
	}
	return ReliableAcknowledgement{
		TransferGeneration: transfer.generation, TransferEpoch: transfer.transferEpoch,
		MessageNumber: messageNumberMetadataRequest, Sequence: transfer.sequence,
		ContiguousPayloadBytes: transfer.sentEnd,
	}, true
}

// AcknowledgementIdentity returns the current typed reliable-transfer identity
// for source-valid requested or unsolicited host progress ACKs.
func (transfer MetadataTransfer) AcknowledgementIdentity() (ReliableAcknowledgement, bool) {
	return transfer.acknowledgementIdentity()
}

// PendingAcknowledgement returns the same identity only while an ACME deadline
// is armed. It is a convenience, not an ACK admissibility restriction.
func (transfer MetadataTransfer) PendingAcknowledgement() (ReliableAcknowledgement, bool) {
	if !transfer.ackDeadlineArmed {
		return ReliableAcknowledgement{}, false
	}
	return transfer.acknowledgementIdentity()
}

// PreviewAcknowledgement performs the same complete validation without
// observing the clock or mutating transfer state. A transport can therefore
// select and finally admit a downstream ACK before acknowledging its USB OUT
// transaction, then call Acknowledge only after delivery. Calls must remain
// serialized; a successful preview is not a reservation.
func (transfer *MetadataTransfer) PreviewAcknowledgement(
	ack ReliableAcknowledgement,
	nowMS uint64,
) (ReliableAcknowledgementDisposition, error) {
	if transfer == nil || !transfer.valid {
		return 0, ErrInvalidMetadata
	}
	if err := transfer.validateClock(nowMS); err != nil {
		return 0, err
	}
	if transfer.faulted {
		return 0, ErrMetadataTransferFaulted
	}
	if transfer.ackDeadlineArmed && nowMS >= transfer.ackDeadlineMS {
		return 0, ErrReliableTransferTimeout
	}
	return transfer.validateAcknowledgement(ack)
}

// Acknowledge accepts requested and source-valid in-flight unsolicited ACKs.
// The host-reported total sequential contiguous byte count can advance progress
// or rewind a gap. A rewind invalidates every never-delivered selection beyond
// the new offset. An admitted packet must resolve before ACK processing so the
// endpoint write and host progress remain serialized.
func (transfer *MetadataTransfer) Acknowledge(
	ack ReliableAcknowledgement,
	nowMS uint64,
) (ReliableAcknowledgementDisposition, error) {
	if err := transfer.observeAndCheckTimeout(nowMS); err != nil {
		return 0, err
	}
	disposition, err := transfer.validateAcknowledgement(ack)
	if err != nil {
		return 0, err
	}

	rewind := disposition == ReliableAcknowledgementRewind
	transfer.lastNowMS = nowMS
	transfer.ackDeadlineArmed = false
	transfer.ackDeadlineMS = 0
	transfer.nextACKRequestMS = saturatingMetadataDeadline(
		nowMS, ReliableACKRequestIntervalMilliseconds)
	if rewind {
		transfer.offset = ack.ContiguousPayloadBytes
		transfer.sentEnd = ack.ContiguousPayloadBytes
		if transfer.hasClaim {
			transfer.clearClaim()
		}
		transfer.retryPending = false
		transfer.retryRecord = metadataPacketRecord{}
	}
	transfer.acknowledgedEnd = ack.ContiguousPayloadBytes
	transfer.finalAcknowledged = int(ack.ContiguousPayloadBytes) == len(transfer.metadata.data)
	if !rewind {
		// An ACK can race only a selected-but-not-admitted packet. Preserve
		// accepted host progress in that packet's proposed post-state (or in a
		// retained retry), otherwise its later delivery would restore the stale
		// acknowledgement high-water mark captured at selection time.
		if transfer.hasClaim {
			transfer.claimRecord.post.acknowledgedEnd = transfer.acknowledgedEnd
			transfer.claimRecord.post.finalAcknowledged = transfer.finalAcknowledged
		}
		if transfer.retryPending {
			transfer.retryRecord.post.acknowledgedEnd = transfer.acknowledgedEnd
			transfer.retryRecord.post.finalAcknowledged = transfer.finalAcknowledged
		}
	}
	return disposition, nil
}

func (transfer *MetadataTransfer) validateAcknowledgement(
	ack ReliableAcknowledgement,
) (ReliableAcknowledgementDisposition, error) {
	if transfer.claimAdmitted {
		return 0, ErrMetadataClaimOutstanding
	}
	if len(transfer.metadata.data) <= 60 || transfer.done || transfer.sentEnd == 0 ||
		ack.TransferGeneration != transfer.generation ||
		ack.TransferEpoch != transfer.transferEpoch ||
		ack.MessageNumber != messageNumberMetadataRequest ||
		ack.Sequence != transfer.sequence ||
		ack.ContiguousPayloadBytes < transfer.acknowledgedEnd ||
		ack.ContiguousPayloadBytes > transfer.sentEnd ||
		int(ack.ContiguousPayloadBytes) > len(transfer.metadata.data) {
		return 0, fmt.Errorf(
			"%w: generation=%d epoch=%d message=%d sequence=%d bytes=%d",
			ErrInvalidAcknowledgement, ack.TransferGeneration, ack.TransferEpoch,
			ack.MessageNumber, ack.Sequence, ack.ContiguousPayloadBytes)
	}

	progress := ack.ContiguousPayloadBytes > transfer.acknowledgedEnd
	rewind := ack.ContiguousPayloadBytes < transfer.offset
	if rewind {
		return ReliableAcknowledgementRewind, nil
	}
	if progress {
		return ReliableAcknowledgementProgress, nil
	}
	return ReliableAcknowledgementDuplicate, nil
}

// Reset invalidates every old claim, retry, and ACK. Both the transport
// generation and reliable-transfer epoch must be strict non-zero successors.
func (transfer *MetadataTransfer) Reset(
	sequence uint8,
	successorGeneration uint64,
	successorTransferEpoch uint64,
	nowMS uint64,
) error {
	if !transfer.valid {
		return ErrInvalidMetadata
	}
	if err := transfer.validateClock(nowMS); err != nil {
		return err
	}
	if sequence == 0 {
		return ErrReservedSequence
	}
	if transfer.generation == ^uint64(0) || successorGeneration == 0 ||
		successorGeneration != transfer.generation+1 {
		return ErrInvalidTransferGeneration
	}
	if transfer.transferEpoch == ^uint64(0) || successorTransferEpoch == 0 ||
		successorTransferEpoch != transfer.transferEpoch+1 {
		return ErrInvalidTransferEpoch
	}
	metadata := transfer.metadata
	nextToken := transfer.nextToken
	*transfer = MetadataTransfer{
		metadata: metadata, sequence: sequence,
		generation: successorGeneration, transferEpoch: successorTransferEpoch,
		valid: true, lastNowMS: nowMS, nextACKRequestMS: nowMS, nextToken: nextToken,
	}
	return nil
}

func encodeMetadataFragmentHeader(
	dst []byte,
	sequence uint8,
	payloadLength uint8,
	initial bool,
	requestACK bool,
	totalLengthOrOffset uint16,
) {
	dst[0] = messageNumberMetadataRequest
	dst[1] = flagFragment | flagSystem
	if initial {
		dst[1] |= flagInitFragment
	}
	if requestACK {
		dst[1] |= flagAcknowledge
	}
	dst[2] = sequence
	dst[3] = payloadLength
	dst[4] = 0x80 | byte(totalLengthOrOffset&0x7f)
	dst[5] = byte(totalLengthOrOffset >> 7)
}

func encodeMetadataCompleteHeader(dst []byte, sequence uint8, totalLength uint16) {
	dst[0] = messageNumberMetadataRequest
	dst[1] = flagFragment | flagSystem
	dst[2] = sequence
	dst[3] = 0x00
	dst[4] = 0x80 | byte(totalLength&0x7f)
	dst[5] = byte(totalLength >> 7)
}

package controllerfeedback

import "sync"

// Mailbox owns one replaceable, complete canonical feedback snapshot. Its
// monitor protects value copies and ordering state only; translation, logging,
// callbacks, waits, and I/O belong after a successful claim.
//
// The zero value is ready for use.
type Mailbox struct {
	mu       sync.Mutex
	latest   Frame
	revision uint64
	hasValue bool
}

// ClaimDisposition describes the state transition a consumer must perform.
// ClaimRelease is an obligation to locally zero every physical actuator; it
// is not merely the absence of a fresh frame.
type ClaimDisposition uint8

const (
	ClaimNone ClaimDisposition = iota
	ClaimFrame
	ClaimRelease
)

// ClaimCursor independently remembers completed application and release
// delivery. Exactly one claim may be in flight. Claim and Complete form a
// serialized single-consumer contract. On first use a cursor binds its pointer
// identity and mailbox; a bound cursor must not be copied or reused for another
// mailbox. The zero value is ready.
type ClaimCursor struct {
	AppliedRevision          uint64
	ReleasedRevision         uint64
	nextToken                uint64
	inFlightToken            uint64
	inFlightRevision         uint64
	inFlightDisposition      ClaimDisposition
	inFlightCompletesRelease bool
	ownerMailbox             *Mailbox
	self                     *ClaimCursor
}

// Publish admits frame when it is valid and newer than the current ordering
// watermark. Expiration never removes that watermark.
func (mailbox *Mailbox) Publish(frame Frame) bool {
	if mailbox == nil || !frame.Valid() {
		return false
	}

	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if mailbox.hasValue && !newer(frame, mailbox.latest) {
		return false
	}

	mailbox.latest = frame
	mailbox.hasValue = true
	mailbox.revision++
	if mailbox.revision == 0 {
		mailbox.revision = 1
	}
	return true
}

// ReadLatest returns the complete ordering watermark even when it is expired.
// The third result is false only when no frame has ever been published.
func (mailbox *Mailbox) ReadLatest() (Frame, uint64, bool) {
	if mailbox == nil {
		return Frame{}, 0, false
	}

	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	return mailbox.latest, mailbox.revision, mailbox.hasValue
}

// ReadFresh returns the latest frame only while its TTL remains live. Revision
// is still returned for expired state so diagnostics can identify the retained
// ordering watermark. It is diagnostic only; Claim drives one-shot release.
func (mailbox *Mailbox) ReadFresh(nowMicroseconds uint64) (Frame, uint64, bool) {
	if mailbox == nil {
		return Frame{}, 0, false
	}

	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if !mailbox.hasValue || mailbox.latest.ExpiredAt(nowMicroseconds) {
		return Frame{}, mailbox.revision, false
	}
	return mailbox.latest, mailbox.revision, true
}

// Claim reserves each fresh revision as ClaimFrame. If that revision is
// expired or implausibly future-dated, it reserves ClaimRelease, including
// when the frame was previously completed while fresh. A claim does not
// advance either completion watermark. Translators perform physical I/O
// outside the mailbox lock and then call Complete with the exact non-zero
// token. One cursor is for one serialized consumer.
func (mailbox *Mailbox) Claim(nowMicroseconds uint64,
	cursor *ClaimCursor) (Frame, ClaimDisposition, uint64) {
	if mailbox == nil || cursor == nil {
		return Frame{}, ClaimNone, 0
	}
	if cursor.self == nil {
		cursor.self = cursor
		cursor.ownerMailbox = mailbox
	} else if cursor.self != cursor || cursor.ownerMailbox != mailbox {
		return Frame{}, ClaimNone, 0
	}

	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if !mailbox.hasValue || cursor.inFlightToken != 0 {
		return Frame{}, ClaimNone, 0
	}
	if mailbox.latest.FreshAt(nowMicroseconds) {
		if cursor.AppliedRevision == mailbox.revision {
			return Frame{}, ClaimNone, 0
		}
		token := beginClaim(cursor, mailbox.revision, ClaimFrame,
			mailbox.latest.Command == CommandStop)
		return mailbox.latest, ClaimFrame, token
	}
	if cursor.ReleasedRevision == mailbox.revision {
		return Frame{}, ClaimNone, 0
	}
	token := beginClaim(cursor, mailbox.revision, ClaimRelease, true)
	return Frame{}, ClaimRelease, token
}

// CanDeliver revalidates the exact claim immediately before a bounded,
// nonblocking physical-output admission. A newer publication, an expired
// ClaimFrame, or a ClaimRelease whose revision became fresh makes the old
// claim ineligible. The caller must still Complete(false) in a defer and
// retry. This check cannot make blocking I/O safe past the frame deadline;
// admission needs deadline-derived cancellation and one serialized writer.
func (mailbox *Mailbox) CanDeliver(cursor *ClaimCursor, token uint64,
	nowMicroseconds uint64) bool {
	if mailbox == nil || cursor == nil || token == 0 ||
		cursor.self != cursor || cursor.inFlightToken != token ||
		cursor.ownerMailbox != mailbox ||
		(cursor.inFlightDisposition != ClaimFrame &&
			cursor.inFlightDisposition != ClaimRelease) {
		return false
	}

	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if !mailbox.hasValue || mailbox.revision != cursor.inFlightRevision {
		return false
	}
	if cursor.inFlightDisposition == ClaimFrame {
		return mailbox.latest.FreshAt(nowMicroseconds)
	}
	return mailbox.latest.ExpiredAt(nowMicroseconds)
}

// Complete resolves the exact in-flight claim. Failed delivery clears only
// the reservation, so unchanged feedback remains retryable. Successful
// delivery advances the relevant completion watermarks. Invalid, zero, stale,
// and duplicate tokens fail closed without disturbing a valid in-flight claim.
func (mailbox *Mailbox) Complete(cursor *ClaimCursor, token uint64,
	delivered bool) bool {
	if mailbox == nil || cursor == nil || token == 0 ||
		cursor.self != cursor ||
		cursor.inFlightToken != token ||
		cursor.ownerMailbox != mailbox ||
		(cursor.inFlightDisposition != ClaimFrame &&
			cursor.inFlightDisposition != ClaimRelease) {
		return false
	}

	if delivered {
		cursor.AppliedRevision = cursor.inFlightRevision
		if cursor.inFlightCompletesRelease {
			cursor.ReleasedRevision = cursor.inFlightRevision
		}
	}

	cursor.inFlightToken = 0
	cursor.inFlightRevision = 0
	cursor.inFlightDisposition = ClaimNone
	cursor.inFlightCompletesRelease = false
	return true
}

func beginClaim(cursor *ClaimCursor, revision uint64,
	disposition ClaimDisposition, completesRelease bool) uint64 {
	token := cursor.nextToken + 1
	if token == 0 {
		token = 1
	}
	cursor.nextToken = token
	cursor.inFlightToken = token
	cursor.inFlightRevision = revision
	cursor.inFlightDisposition = disposition
	cursor.inFlightCompletesRelease = completesRelease
	return token
}

func newer(candidate, current Frame) bool {
	if candidate.DeviceGeneration != current.DeviceGeneration {
		return candidate.DeviceGeneration > current.DeviceGeneration
	}
	if candidate.TransportGeneration != current.TransportGeneration {
		return candidate.TransportGeneration > current.TransportGeneration
	}
	if candidate.OwnershipEpoch != current.OwnershipEpoch {
		return candidate.OwnershipEpoch > current.OwnershipEpoch
	}

	// Stop is terminal for an ownership epoch. A later source must acquire a
	// new epoch instead of resurrecting a retired lease with only a new packet
	// sequence.
	return current.Command != CommandStop && candidate.Source == current.Source &&
		candidate.Sequence > current.Sequence
}

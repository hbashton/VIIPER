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

// ClaimCursor independently remembers application and release delivery. A
// frame can be claimed while fresh and must still produce one release when
// that same revision later expires. The zero value is ready for use.
type ClaimCursor struct {
	Revision        uint64
	ReleaseRevision uint64
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

// Claim returns each fresh revision once as ClaimFrame. If that revision is
// expired or implausibly future-dated, it returns ClaimRelease exactly once,
// including when the frame was previously claimed while fresh. Translators
// must handle ClaimRelease by locally zeroing all physical actuators outside
// the mailbox lock.
func (mailbox *Mailbox) Claim(nowMicroseconds uint64,
	cursor *ClaimCursor) (Frame, ClaimDisposition) {
	if mailbox == nil || cursor == nil {
		return Frame{}, ClaimNone
	}

	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if !mailbox.hasValue {
		return Frame{}, ClaimNone
	}
	if mailbox.latest.FreshAt(nowMicroseconds) {
		if cursor.Revision == mailbox.revision {
			return Frame{}, ClaimNone
		}
		cursor.Revision = mailbox.revision
		return mailbox.latest, ClaimFrame
	}
	if cursor.ReleaseRevision == mailbox.revision {
		return Frame{}, ClaimNone
	}
	cursor.Revision = mailbox.revision
	cursor.ReleaseRevision = mailbox.revision
	return Frame{}, ClaimRelease
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

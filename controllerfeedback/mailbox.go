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
// ordering watermark.
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

// ClaimFresh returns a new, non-expired revision and advances
// claimedRevision. An expired frame does not consume the caller's revision.
func (mailbox *Mailbox) ClaimFresh(nowMicroseconds uint64,
	claimedRevision *uint64) (Frame, bool) {
	if mailbox == nil || claimedRevision == nil {
		return Frame{}, false
	}

	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if !mailbox.hasValue || *claimedRevision == mailbox.revision ||
		mailbox.latest.ExpiredAt(nowMicroseconds) {
		return Frame{}, false
	}
	*claimedRevision = mailbox.revision
	return mailbox.latest, true
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

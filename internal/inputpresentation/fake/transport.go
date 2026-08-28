// Package fake provides a deterministic, allocation-insensitive presentation
// transport for scheduler contract tests. It never starts goroutines or waits
// on wall-clock time.
package fake

import (
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
)

// Record is one terminal fake-transport observation. Report is a stable copy
// of the exact bytes owned by Claim.
type Record struct {
	Claim    inputpresentation.Claim
	Outcome  inputpresentation.Outcome
	At       time.Time
	Age      time.Duration
	Report   []byte
	Accepted bool
}

// Transport owns one fixed report buffer and a manually advanced clock. Like
// the production scheduler contract, it permits at most one unresolved claim.
type Transport struct {
	now           time.Time
	buffer        []byte
	pending       inputpresentation.Claim
	pendingReport []byte
	hasPending    bool
	records       []Record
	maximumAge    time.Duration
}

// New constructs a fake transport with a fixed maximum report size.
func New(maximumReportSize int, start time.Time) *Transport {
	if maximumReportSize < 0 {
		maximumReportSize = 0
	}
	return &Transport{
		now:           start,
		buffer:        make([]byte, maximumReportSize),
		pendingReport: make([]byte, maximumReportSize),
	}
}

// Now returns the deterministic transport clock.
func (transport *Transport) Now() time.Time { return transport.now }

// Advance moves the deterministic clock forward. Negative durations are
// ignored so observed claim age remains monotonic.
func (transport *Transport) Advance(elapsed time.Duration) {
	if elapsed > 0 {
		transport.now = transport.now.Add(elapsed)
	}
}

// Claim synchronously selects one report. It returns false while another claim
// is unresolved or when the source cannot produce a valid report.
func (transport *Transport) Claim(source inputpresentation.Source) (
	inputpresentation.Claim, bool,
) {
	if source == nil || transport.hasPending || len(transport.buffer) == 0 {
		return inputpresentation.Claim{}, false
	}
	claim := source.ClaimInputPresentation(transport.buffer, transport.now)
	if !claim.Valid() || claim.Size > len(transport.buffer) {
		return inputpresentation.Claim{}, false
	}
	transport.pending = claim
	copy(transport.pendingReport[:claim.Size], transport.buffer[:claim.Size])
	transport.hasPending = true
	return claim, true
}

// Resolve applies exactly one terminal outcome to the pending claim. Accepted
// reflects whether the source accepted that resolution; stale/duplicate source
// outcomes are therefore directly observable by tests.
func (transport *Transport) Resolve(source inputpresentation.Source,
	outcome inputpresentation.Outcome) (Record, bool) {
	if source == nil || !transport.hasPending || !outcome.Valid() {
		return Record{}, false
	}
	claim := transport.pending
	accepted := source.ResolveInputPresentation(claim, outcome, transport.now)
	record := Record{
		Claim: claim, Outcome: outcome, At: transport.now,
		Age: claim.AgeAt(transport.now), Accepted: accepted,
		Report: append([]byte(nil), transport.pendingReport[:claim.Size]...),
	}
	transport.records = append(transport.records, record)
	if record.Age > transport.maximumAge {
		transport.maximumAge = record.Age
	}
	transport.pending = inputpresentation.Claim{}
	clear(transport.pendingReport[:claim.Size])
	transport.hasPending = false
	return record, accepted
}

// RetireGeneration advances a source generation without requiring an active
// claim. If the fake owns a claim from that generation, it records its terminal
// retirement and releases the local claim as well.
func (transport *Transport) RetireGeneration(source inputpresentation.Source,
	generation uint64) bool {
	if source == nil || generation == 0 {
		return false
	}
	accepted := source.RetireInputPresentationGeneration(
		generation, transport.now)
	if accepted && transport.hasPending &&
		transport.pending.Generation == generation {
		claim := transport.pending
		record := Record{
			Claim: claim, Outcome: inputpresentation.OutcomeRetire,
			At: transport.now, Age: claim.AgeAt(transport.now), Accepted: true,
			Report: append([]byte(nil), transport.pendingReport[:claim.Size]...),
		}
		transport.records = append(transport.records, record)
		if record.Age > transport.maximumAge {
			transport.maximumAge = record.Age
		}
		transport.pending = inputpresentation.Claim{}
		clear(transport.pendingReport[:claim.Size])
		transport.hasPending = false
	}
	return accepted
}

// Pending returns the currently unresolved claim.
func (transport *Transport) Pending() (inputpresentation.Claim, bool) {
	return transport.pending, transport.hasPending
}

// Records returns a stable copy of all terminal observations.
func (transport *Transport) Records() []Record {
	return append([]Record(nil), transport.records...)
}

// MaximumAge returns the greatest receive-to-terminal age observed so far.
func (transport *Transport) MaximumAge() time.Duration {
	return transport.maximumAge
}

package retainedusb

// ImportRetirementReason identifies a terminal presentation fault which does
// not, by itself, make outstanding USB ownership or local output uncertain.
// It requests the ordinary irreversible drain + acknowledged neutral boundary;
// it is never permission to reset, reconnect, skip neutral, or report Safe.
type ImportRetirementReason uint8

const (
	ImportRetirementInputHistoryOverflow ImportRetirementReason = iota + 1
)

func (reason ImportRetirementReason) Valid() bool {
	return reason == ImportRetirementInputHistoryOverflow
}

func (reason ImportRetirementReason) String() string {
	if reason == ImportRetirementInputHistoryOverflow {
		return "input presentation history overflow"
	}
	return "invalid import retirement reason"
}

// ImportRetirementRequest is authenticated against the scheduler's exact
// bound reservation. The all-zero value means no request. Once published, a
// nonzero request is immutable for the import and must not be cleared by START
// or reversible reset. A request is not a proof that teardown has succeeded.
type ImportRetirementRequest struct {
	Lease  ImportLease
	Reason ImportRetirementReason
}

func (request ImportRetirementRequest) Valid() bool {
	return request.Lease.Valid() && request.Reason.Valid()
}

// ImportRetirementOwner optionally exposes a terminal-but-drainable owner
// fault without needing a host IN poll. The query is bounded, nonblocking,
// performs no I/O, and must not call back into the scheduler. Publishing the
// request increments and signals the existing Owner.Readiness epoch/channel.
//
// A non-nil error always wins over a request and is a fatal owner failure. In
// particular, fatal local feedback, quarantined ownership, and ambiguous USB
// completion must not be downgraded to a retirement request. While a request
// is latched, Stage/Prepare must admit no new effect; retained unadmitted
// tickets remain retireable, and exact already-admitted Complete remains
// mandatory. CancelAndDrain and DisconnectNeutral retain all their existing
// guarantees; only acknowledged neutralization permits import release.
type ImportRetirementOwner interface {
	RetainedImportRetirement() (ImportRetirementRequest, error)
}

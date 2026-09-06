# Dormant Stage-B retained USB submission boundary

Status update (2026-09-01): the Stage-B.1 arbiter, Stage-B.2 lifecycle, and
Xbox three-lane owner are now connected to the production USB/IP command reader
only through explicit retained-device plus nonzero-authority opt-in. The Xbox
wrapper remains absent from registry/API product creation. No driver, Windows
binding, hardware conformance, or latency claim follows from transport routing.

## Decision

The smallest safe boundary is one opt-in, connection-scoped retained
submission arbiter covering all three mutually ordered lanes:

1. endpoint zero;
2. one exact interrupt-IN descriptor binding; and
3. one exact interrupt-OUT descriptor binding.

For an opted-in device, endpoint zero and interrupt OUT must move off the sole
USB/IP command reader together. Interrupt IN is already asynchronous, but it
must leave the independent endpoint worker and join the same arbiter. This is
an all-or-nothing route selection for those exact bindings.

Either endpoint zero or interrupt OUT may reach final admission and discover
that a preceding upstream or local action must finish first. If either remains
synchronous, the command reader cannot ingest the interrupt-IN URB or
`CMD_UNLINK` needed to make progress. Moving only OUT does not fix a waiting
EP0 request; moving only EP0 does not fix a waiting OUT request. Giving the
three lanes independent workers would also let goroutine scheduling decide
which lane first acquires `responseWriter.mu`. One arbiter makes the actual
serialized selection order explicit.

Devices which do not opt in retain the current transactional EP0,
transactional interrupt-OUT, prepared interrupt-IN, generic, and ISO behavior
byte-for-byte. An opted-in device must not simultaneously claim an overlapping
lane through `TransactionalControlDevice`, `TransactionalInterruptOutDevice`,
or `PreparedSource`; import must reject that ambiguous ownership rather than
choose a precedence.

## Boundary roles

The command reader remains the connection's only reader. It performs only
wire framing, existing envelope/descriptor validation, exact active-binding
lookup, immutable copying into bounded scheduler storage, enqueue, and
`CMD_UNLINK` dispatch. It never calls the retained device owner and never waits
for device protocol progress.

The retained arbiter is the only code which stages a device ticket, chooses a
lane for a late admission attempt, or calls the late response serializer. A
future owner contract needs five value-oriented operations:

- construction-time route and storage limits;
- pure `Stage`, which copies the request facts it needs and returns an opaque,
  generation-bound ticket without selecting a device action;
- late `Prepare`, which runs under response serialization and returns Pending,
  Data, successful no-payload completion, or STALL;
- exactly-once `Complete(ticket, delivered)` for a terminal preparation; and
- exactly-once `Retire(ticket, reason)` when endpoint or session ownership is
  lost before admission.

Names above describe roles rather than a public Go interface. The internal
value types reject zero, forged, stale, duplicate, cross-owner, cross-lane, and
cross-session tickets. `Stage`, `Prepare`, `Complete`, and `Retire` must be
bounded, nonblocking, perform no I/O, retain no server slice, and never call
back into the server. `Prepare` alone may choose a protocol action. An IN job
stores only host receive capacity; latest/ordered semantic input is sampled by
the future adapter inside `Prepare`, never when the host URB is enqueued.

Pending also needs an explicit level-triggered readiness contract. The owner
must expose a session-lifetime latched wake channel with capacity at least one
plus a monotonically increasing readiness epoch. A pending result may
additionally supply one absolute retry
deadline. The owner increments the epoch before signaling input arrival,
local-executor completion, or protocol progress. The arbiter compares epochs
before sleeping, so a signal racing the transition to sleep cannot be lost.
A timer deadline must be strictly later than the attempt time; immediate
Pending without a changed epoch is not retryable in the same scheduling round.

### Concrete dormant contract shape

The smallest reviewable contract lives in the repository-internal neutral
`internal/retainedusb` package, which future `device/...` adapters and
`internal/server/usb` may import. It is not part of the broad `usb.Device`
interface. The following is a condensed view of the implemented shape:

```go
type Lane uint8 // Control, InterruptIn, InterruptOut

type Limits struct {
    QueueDepth             [3]uint8 // each 1..endpointQueueCapacity
    BufferedRequestBytes   uint32   // aggregate, <= maximumTransferSize
    MaximumControlOut      uint32
    MaximumControlResponse uint32
    MaximumInterruptIn     uint32
    MaximumInterruptOut    uint32
    InterruptInRoute       Route
    InterruptOutRoute      Route
}

type Request struct {
    Lane              Lane
    Direction         Direction
    SessionGeneration uint64
    BindingGeneration uint64
    IngressOrdinal    uint64
    Sequence          uint32
    TransferLength    uint32
    Setup             [8]byte
    Route             Route
    Data              []byte // exact read-only view, valid only during Stage
}

type Ticket struct {
    OwnerID           uint64
    Token             uint64
    Generation        uint64
    SessionGeneration uint64
    Lane              Lane
}

type Preparation struct {
    Result         Result // Pending, Data, Success, Stall
    ActualLength   uint32
    RetryAt        time.Time
    ReadinessEpoch uint64
}

type Owner interface {
    Identity() uint64
    Limits() Limits
    Stage(Request) (Ticket, error)
    Prepare(Ticket, []byte, time.Time) (Preparation, error)
    Complete(Ticket, bool, time.Time) error
    Retire(Ticket, RetireReason, time.Time) error
    Readiness() (uint64, <-chan struct{})
}
```

`Route` is an exact interface/alternate-setting/endpoint-address tuple, not
merely an endpoint number. `Result` shapes mirror the already-reviewed late
writer. The scheduler validates generic structure; the one Owner instance
authenticates ticket identity and capability. There is no Unhandled or legacy
fallback result after opt-in routing. Every Stage error is fatal: because the
scheduler itself enforces one ticket per lane, an owner reporting lane busy
means the two ownership ledgers disagree and must not be retried indefinitely.
An errored, forged, or colliding Stage result never becomes transport-owned and
must not receive per-ticket `Retire`: doing so could retire an accepted
predecessor with the same tuple. The outer session-fatal cleanup owns any such
owner-side ambiguity and retires already-accepted predecessor tickets once.

`Identity` is nonzero, stable for the owner lifetime, and unique across
independent owner authorities. The exact import authority issues the nonzero
session generation before the parked scheduler is constructed; it must never
reuse or wrap it. `Readiness` returns the same
session-lifetime, latched channel with capacity at least one on every call and
a monotonically increasing, nonwrapping epoch; the owner advances the epoch
before a nonblocking signal, where a full channel means an earlier wake is
already pending, and Pending reports the current snapshot. A nil, unbuffered,
closed, or changed channel, epoch regression, or `MaxUint64` epoch is
connection-fatal.

The whole-import lifecycle remains a separate contract. Folding it into
`Owner` would encourage a hot-path ticket callback to perform teardown I/O.
Stage-B.2 implements this repository-internal value shape:

```go
type ImportLease struct {
    AuthorityID       uint64
    DeviceID          uint64
    OwnerID           uint64
    ImportToken       uint64
    SessionGeneration uint64
}

type ImportSessionOwner interface {
    Identity() uint64
    BindImport(ImportLease, time.Time) (ImportBindResult, error)
    CancelAndDrain(ImportLease, ImportCloseReason, time.Time) (
        ImportDrainResult, error)
    DisconnectNeutral(ImportLease, ImportCloseReason, time.Time) (
        ImportDisconnectResult, error)
}

type ImportResetLease struct {
    ImportLease     ImportLease
    ResetToken      uint64 // authority-wide, never reused
    ResetGeneration uint64 // exact per-import successor
}

type ImportResetSessionOwner interface {
    FenceAndDrainReset(ImportResetLease, time.Time) error
    ResetAndRestart(ImportResetLease, time.Time) (ImportResetResult, error)
}
```

The exact lease binds an issuing authority, an internal device key, the outer
owner identity, a nonzero import token, and a nonzero session generation. The
two counters never wrap; exhaustion rejects the reservation without advancing
either one. `DeviceID` is only a key supplied to this dormant authority and is
not a registry entry, USB identity, or public device identifier. The authority
first reserves the lease against one exact pointer-backed object implementing
both owner contracts. Before any scheduler callback or allocation, the
reservation atomically transfers from Reserved to Building. Abort wins first
and construction touches no owner, or build wins and plain abort can no longer
orphan the resource. A failed build returns to exact abortable Reserved state;
a successful build publishes one authority-owned Prepared scheduler which must
be committed or cancelled through bounded `abortPrepared`.

Commit authenticates the opaque reservation pointer, exact owner reference and
identity, issued session generation, same authority-owned scheduler, and its
never-started state before mandatory bounded `BindImport`. Exact Bound is
followed by a separate bounded one-shot scheduler activation. Activation only
permits the final claim attempt: while the session still owns the exact
reservation, the scheduler synchronously rechecks that activation is Started,
the worker is started and live, context is not cancelled, no terminal failure
has been recorded, and the gate is still closed. Claimed is published under the
session mutex before that scheduler-mutex-linearized check opens the gate. A
worker which exits before this point therefore rejects commit rather than
publishing a dead claimed session. Enqueue checks the gate while holding the
scheduler mutex; close fences it under the same mutex before closing ingress,
so no enqueue begun after the fence can acquire a slot. An exact Rejected result
rolls back only after the gate, ingress, and scheduler prove terminal, while
both counters remain burned.
Bind or activation error, panic, timeout, malformed echo, cleanup ambiguity, or
Quarantined retains the exact unpublished record in quarantine. If the owner
may have reached Bound, failure cleanup additionally runs exact
`CancelAndDrain` then `DisconnectNeutral`; a failed neutral remains
quarantined. If scheduler stop/join itself is unproven, the outer boundary
instead issues containment-only `CancelAndDrain`. That operation is valid after
the outer fence was attempted but could not be proven because the owner first
closes its own Stage/Prepare admission. The boundary accepts only an exact
`ImportDrainQuarantined` acknowledgement and never calls neutral or releases.
Drain and disconnect results echo the complete lease and immutable
close reason. Safe disconnect is the only result which permits import release.

Reversible whole-device reset is intentionally a third, optional contract; it
cannot be satisfied by either terminal close method. The authority issues one
exact nonwrapping reset token and successor reset generation only while the
import is Claimed. The transaction directly latches the scheduler's atomic
admission gate before waiting for any serializer work, invokes
`FenceAndDrainReset` to close owner Stage/Prepare and join local execution,
deadline-drains the response serializer and all retained tickets, then invokes
`ResetAndRestart` to advance the canonical owner generation and deliver one
successor-generation clear. Only an exact echoed Safe result followed by an
exact callback-free scheduler revalidation can reopen admission. Exact repeats
return the cached terminal result; stale/cross-import values are rejected. Any
failure or ambiguity quarantines the retained session, and terminal containment
may fence the local executor but cannot neutralize, release, or reopen it.

The dormant server-side coordinator owns one ingress closer, one Stage-B.1
scheduler, and the same combined owner object used by its hot tickets. It
contains callback panic and absolute-deadline timeout at every outer phase.
A callback which outlives its deadline cannot publish Claimed or reopen the
revoked admission gate; the quarantined session strongly retains every exact
reference and rejects a successor. Same-reason close, release, reservation
abort, and prepared cancellation are idempotent at their permitted states. A
different close reason, zero/forged/stale/cross-owner/cross-device/
cross-session lease or reservation, malformed echoed result, split owner,
predicted generation, or premature release is rejected without changing
ownership.

## Fixed ownership and queue bounds

The opt-in scheduler is created before command ingestion and owns all of its
slots, payload storage, response scratch, wake signal, timer, and one arbiter
goroutine. The hard bounds are:

- at most `endpointQueueCapacity` (currently 64) raw jobs per lane;
- at most one device ticket per lane;
- at most one admitted response across the connection;
- a construction-time host-to-device byte budget no larger than
  `maximumTransferSize` (currently 16 MiB) across EP0 and interrupt OUT; and
- construction-time per-route request and response maxima, each no larger than
  the descriptor/envelope limit and the 16-MiB server ceiling.

The future adapter must choose smaller protocol-backed maxima where possible.
The scheduler preallocates its selected slot rings, byte arena, and largest
response scratch before the first URB. Raw EP0 data and interrupt-OUT data are
deep-copied; setup bytes, sequence, direction, transfer length, exact endpoint
binding, binding generation, session generation, and one monotonic ingress
ordinal are stored by value. IN consumes no request-payload budget.

If a lane has no free slot or the byte budget cannot hold the complete request,
the command reader emits one immediate `RET_SUBMIT` with `-ENOSPC`, zero actual
length, and no owner callback. It never blocks waiting for capacity, silently
drops a request, partially copies it, or falls through to legacy handling. A
duplicate still-pending USB/IP sequence number or any counter exhaustion is a
connection-fatal protocol/invariant error.

## Job states

Each slot has one of these externally meaningful states:

| State | Owner | Meaning |
|---|---|---|
| Free | scheduler | no request or ticket |
| RawQueued | scheduler | the complete host request is copied; no device capability exists |
| Ticketed | scheduler + device | the head of this lane has one pure staged ticket and has not returned Pending |
| Pending | scheduler + device | late preparation emitted nothing and retained the exact ticket |
| Admitted | response serializer + device | final cancellation/generation checks won and a terminal image was selected |
| Terminal | none | completion ran exactly once; the slot may be scrubbed and reused |
| LifecycleRetired | none | raw ownership was discarded or the exact ticket was retired before admission |

Allowed transitions are:

```text
Free -> RawQueued
RawQueued -> Ticketed
RawQueued -> LifecycleRetired
Ticketed -> Pending
Pending -> Ticketed                 (only after readiness changes/deadline)
Ticketed|Pending -> Admitted
Ticketed|Pending -> LifecycleRetired
Admitted -> Terminal                (delivered or delivery-failed)
Terminal|LifecycleRetired -> Free
```

Only the FIFO head of a lane may become Ticketed. `Stage` runs under the
scheduler lock, so unlink cannot observe a half-created capability. Later jobs
in the same lane remain RawQueued and cannot overtake it. The arbiter examines
the oldest ingress ordinal among the three lane heads, but a Pending head does
not block a different lane. In one scheduling round, each unchanged pending
ticket is attempted at most once. A terminal response, a new raw job, a changed
readiness epoch, a due retry deadline, a local completion, or a lifecycle event
starts another round. This prevents both head-of-line deadlock and busy-spin.

## Lock and callback order

The only permitted nested order is:

```text
responseWriter.mu -> retainedScheduler.mu -> retained owner
```

Pure staging uses `retainedScheduler.mu -> retained owner`. The owner never
acquires a server lock or calls the transport. Unlink/reset may acquire the
scheduler lock to mark a job retired, but must release it before acquiring the
response writer to send its own reply. Whole-session callbacks run only after
the arbiter and all response callbacks have drained.

For one candidate job the arbiter calls
`responseWriter.writeLateRetSubmit` with preallocated scratch and the job's
validated maximum response:

1. The preparation callback revalidates slot identity, lane FIFO position,
   exact endpoint binding generation, session generation, and cancellation
   after any response-lock wait while `responseWriter.mu` and the scheduler
   lock are held. Cancellation is sampled again after bounded owner Prepare
   and before terminal admission.
2. It calls owner `Prepare` with an exact-length/exact-capacity payload window.
3. Pending leaves the job Pending and returns `lateResponsePending`; it writes,
   flushes, and completes nothing. After releasing both locks, the scheduler
   verifies that the owner's published readiness epoch has reached the epoch
   reported by Pending; a future/unpublished snapshot is fatal.
4. Data, Success/ZLP, or STALL is checked against the original lane and host
   envelope, then changes the job to Admitted before the callback returns.
5. `writeLateRetSubmit` writes the exact `RET_SUBMIT`, performs its mandatory
   flush, and calls completion exactly once with the real delivery result while
   serializer ownership remains held.
6. Completion calls owner `Complete`. An owner error is connection-fatal. A
   successful callback makes the slot Terminal and wakes the arbiter.

Lane shapes are strict: interrupt IN may return Data only; interrupt OUT may
return Success acknowledging exactly the copied OUT length or STALL with zero;
EP0 may return Data, Success/ZLP, or STALL as allowed by its setup direction and
host capacities. Data always has a nonzero length; control ZLP uses Success.
EP0 OUT is accepted only when setup `wLength`, the copied data
length, and USB/IP transfer length agree exactly. No terminal image may exceed
the original USB/IP transfer
length, setup `wLength`, endpoint capacity, owner maximum, or server maximum.

A preparation error cannot fall through. The connection fails, the exact
ticket is retired if it never became Admitted, and session teardown owns the
remaining protocol boundary. A successful terminal preparation remains
Terminal even when socket write or flush fails because its exactly-once
completion already received `delivered=false`.

## Unlink and endpoint lifecycle

`CMD_UNLINK` must search both the retained scheduler and existing non-owned
endpoint workers. USB/IP sequence numbers are unique within the live session.

- RawQueued, Ticketed, or Pending: atomically mark LifecycleRetired. Ticketed
  and Pending call owner `Retire` exactly once. Emit no target `RET_SUBMIT`, then
  emit `RET_UNLINK` with the existing removed status (`-ECONNRESET`).
- Admitted or Terminal: unlink has lost. The target response and completion
  remain first under `responseWriter.mu`; `RET_UNLINK` follows with the existing
  not-removed status (zero).
- A race where the arbiter owns `responseWriter.mu` but has not passed its
  scheduler revalidation is decided by the scheduler state: unlink may mark
  retirement, causing late preparation to return Pending/no-write; otherwise
  admission wins and unlink cannot cancel it.

A delivered, accepted transactional lifecycle control response keeps the
current ordering: owner completion commits first; the current EP0 job becomes
Terminal; affected raw/ticketed/pending endpoint jobs are retired; presentation
generations and endpoint generations advance; and only then are server
configuration/alternate-setting routes published. These operations remain
under response serialization. STALL, admission error, write/flush failure, or
completion error changes no server routing.

`CLEAR_FEATURE(ENDPOINT_HALT)` retires only the exact endpoint direction.
`SET_INTERFACE` retires all bindings of the affected interface before the new
alternate route is visible. `SET_CONFIGURATION` retires non-EP0 endpoint jobs
before the new configuration is visible; later queued EP0 requests remain
valid because endpoint zero persists. None of these requests is a USB bus
reset, disconnect, or reconnect, and none may call an authoritative
whole-device reset merely to simplify cleanup.

## Connection and whole-device lifecycle

The current production import lease remains unchanged: its token is hidden in
an unconditional release closure and its counter wraps to one. Stage-B.2 does
not reuse or route that legacy mechanism. Its dormant exact authority and
session coordinator implement this required order:

1. Reserve one exact nonwrapping import/session capability for one combined
   owner object.
2. Build and authenticate a parked scheduler from the issued generation, bind
   the owner, activate the scheduler once, publish Claimed, then perform the
   synchronous liveness-linearized ingress open described above.
3. On read failure, write failure, context cancellation, or explicit detach,
   fence scheduler admission under its enqueue mutex and advance to Closing.
4. Close ingress to unblock any active read/write, then close and join the arbiter,
   let every Admitted job complete once with `delivered=false` where needed,
   and retire every remaining RawQueued/Ticketed/Pending job.
5. Synchronously cancel and drain any device-local executor. Only after no
   response, local lease, or staged ticket remains may the owner begin the
   authoritative Disconnect boundary and deliver its one neutral/clear action.
   A failed ingress/scheduler boundary still solicits bounded containment drain,
   but must skip neutralization and release even when that drain is acknowledged.
6. Release the exact import lease only after the disconnect/neutral boundary reaches
   its proven terminal state. Failure quarantines the device/session and must
   not permit a successor import to overtake an uncleared predecessor.

The coordinator states are Reserved, Building, Prepared, PreparedAborting,
Committing, Claimed, ReservationAborted, BindRejected, Closing, IngressStopped,
SchedulerClosed, LocalDrained, Neutralized, Released, and Quarantined. Only
Claimed can execute terminal close; a close which first arrives while an exact
reset is active latches its immutable reason, blocks another reset, and resumes
from Claimed only after that reset reaches Safe. A short observer deadline may
expire without quarantining the owning reset, while a reset failure completes
the already-latched close as Quarantined. An internal stale holder cannot
resurrect an aborted, rejected, or quarantined transaction. All
uncertainty retains the exact active record; an old idempotent release compares
the full capability and session object, so it cannot delete a successor after
a safe close.

The dormant Xbox owner now exposes exact cancel-and-drain,
DisconnectNeutral/one-clear, and reconnect behavior over its one canonical
coordinator. It also exposes the optional two-phase exact reset owner described
above. The reset flow is separate from terminal close:

1. `fenceReset` atomically closes import admission without waiting for
   `scheduler.mu` or the response serializer; a submission already past the
   gate is predecessor work.
2. `FenceAndDrainReset` closes owner admission, reversibly cancels and joins
   local execution, and stops the owner worker while leaving exact retained
   tickets owned.
3. `resetAndDrain` deadline-acquires the serializer, lets an already-active
   response reach its exact completion, and retires all remaining control,
   interrupt-IN, and interrupt-OUT jobs with `RetireDeviceReset`.
4. `ResetAndRestart` advances the owner exactly once and requires terminal
   delivery of its one successor-generation neutral/clear before owner
   admission can reopen.
5. `reopenAfterReset` validates the same reservation, reset capability, empty
   lanes, live scheduler, and closed gate before reopening. It performs no
   callback and no rollback is possible after quarantine.

A forged, stale, cross-import, unissued, or pre-start expired reset returns the
echoed capability with `ImportResetInvalid` and performs no containment or
admission change. `ImportResetQuarantined` is reserved for a transaction which
actually entered the reset boundary and then proved containment without Safe;
the two states are not interchangeable even when both carry an error.
The same distinction applies to terminal owner callbacks: rejected drain and
disconnect credentials return their `Invalid` states, while `Quarantined`
requires the exact import to have reached its containment boundary.

The reversible bus-reset seam remains deliberately unrouted. The pinned
usbip-win2 0.9.7.7 UdeCx source
defines a post-enumeration reset callback, but
`drivers/ude/device.cpp:493` comments out its registration because the server
returns `EPIPE`. Its `device_ioctl.cpp:506-515,558-574` special EP0 OUT
`SET_FEATURE(PORT_RESET)` encoding is therefore not a usable reset event and is
not treated as a generic USB/IP or MS-GIPUSB wire requirement. The registered
link-power/function-suspend callbacks and VHCI D0 callbacks only trace and
return success; they send no server notification. USB/IP currently provides no
usable bus-reset or suspend/resume callback to VIIPER, so none is synthesized
from configuration, interface, pipe-clear, connection-close, or cancellation.

## Required tests before any adapter binding

All tests use a scripted fake retained owner and in-memory USB/IP streams. No
registry or hardware is involved.

### Queue and ownership

- Deep-copy EP0 setup/data and interrupt-OUT data, then overwrite the command
  reader scratch before staging.
- Prove per-lane FIFO, one ticket per lane, immutable ingress ordinals, exact
  binding/session generations, slot/byte overflow `-ENOSPC`, duplicate sequence
  rejection, counter exhaustion, scrubbing, and zero steady-state allocation.
- Inject every Stage error, including an owner-side lane-busy mismatch, and
  prove fail-closed connection retirement with no fallback or later staging.

### Reader progress and cross-lane ordering

- Hold an OUT job Pending on an upstream prerequisite, send a later IN URB, and
  prove the reader ingests it, IN carries the prerequisite, and OUT ACK follows.
- Repeat with a waiting EP0 job. These are the regression proofs that EP0 and
  OUT must move together.
- Prove an unchanged Pending ticket is attempted once per round, another lane
  can progress, same-lane FIFO is preserved, readiness-before-wait is not lost,
  and timer wake does not spin or reorder.

### Serializer and unlink races

- Cover exact Data, ZLP, OUT acknowledgement, and STALL images; bounds and stale
  tail clearing; mandatory flush; short/no-progress writes; delivery plus
  completion failure; preparation error; and panic containment.
- Unlink RawQueued, Ticketed, and Pending jobs and prove one retirement, no
  target response, and removed `RET_UNLINK`.
- Gate immediately before scheduler revalidation and immediately after
  Admitted, proving unlink respectively wins or loses and response-stream order
  is exact.

### Endpoint and whole-session lifecycle

- For delivered CLEAR_HALT, SET_INTERFACE, and SET_CONFIGURATION, assert the
  observable order: owner completion, current EP0 terminal, affected ticket
  retirement, generation rotation, route publication.
- For STALL and every delivery/completion failure, assert no route mutation.
- Prove endpoint/interface/configuration retirement scopes and that none calls
  the whole-device reset hook.
- For the dormant reset capability, prove the atomic ingress latch cannot wait
  behind blocked control/IN/OUT preparation; every raw, staged, pending,
  admitted, and terminal predecessor is completed or retired exactly once;
  selected and retrying clears fence reset; and no cross-generation ticket or
  local action replays.
- Prove reset waits for local execution and serializer drain, delivers exactly
  one successor clear without a successor URB, and reopens only for the exact
  capability after terminal delivery. Inject scheduler cancellation ambiguity,
  blocked local reset callbacks, stale and repeated reset,
  reset/sequence/clear-epoch exhaustion, first-reason close intent during reset,
  a second-reset overtake attempt, and reset/close/import races; every
  ambiguity must retain quarantine without neutral or release.
- Close the connection from every job state and during local execution; prove
  exactly-once failed completion/retirement, worker join, one disconnect clear,
  and delayed import release. A failed neutral must quarantine and reject a
  successor import.
- Stress unlink/reset/close/admission races under `-race`, shuffled repeated
  runs, forged/stale tickets, and a successor session rejecting every old
  generation artifact.
- Race exact abort against parked scheduler construction and prove one winner:
  abort-before-build performs no scheduler callback, while build-before-abort
  transfers cleanup ownership explicitly. Inject activation error, panic, and
  timeout after Bound; prove admission revocation, transport cleanup, terminal
  owner drain/neutral attempt, late-return fencing, and successor rejection.

### Compatibility

- Preserve byte-equivalence for every non-opt-in EP0, transactional OUT,
  prepared IN, generic endpoint, and ISO path.
- Reject overlapping old/new ownership at import.
- Assert the only production call site is the explicit retained import branch;
  registry imports, product identities, automatic attach paths, and hardware
  operations remain absent.

Passing these tests would establish only the generic dormant job and lifecycle
boundary and scripted device-owner behavior. It would not establish production
Xbox routing, Windows API visibility, GIP framing/reassembly, latency, physical
feedback delivery, or hardware conformance.

## Implementation status and recommendation

**Implemented dormant Stage-B.1:** the repository-internal hot-owner value
contract, bounded three-lane state machine/arbiter, scripted fake-owner tests,
direct use of `writeLateRetSubmit`, and a dormancy test proving the constructor
has no non-test call site. It accepts an externally supplied nonzero session
generation and exposes scope-retirement operations without knowing about
import registration.

**Implemented dormant Stage-B.2:** the separate `ImportLease` and
`ImportSessionOwner` value contract; one pointer-exact combined owner;
nonwrapping exact reservation; exclusive Building/Prepared ownership; bounded
prepared cancellation; mandatory Bind; one-shot scheduler activation; a
synchronous failure/close-linearized admission open; and an enqueue-linearized
admission fence before ordered close. Scripted and real-scheduler tests cover
abort/build races and failed construction; bind acceptance, rejection, and
ambiguity; activation error, panic, timeout, late return, pre-publication worker
exit, and error/panic/malformed admission open; bound-owner drain/neutral
cleanup; every outer state and
represented job state; concurrent close; exact reason; failed neutral; delayed
release; successor quarantine; stale/forged/cross-session capabilities;
counter exhaustion; exactly-once outer callbacks; one pending-ticket
retirement; and no duplicate terminal completion. A source scan proves both
Stage-B.2 constructors have only their declarations in production Go files and
that `server.go`/`urb_stream.go` do not reference the boundary.

**Implemented dormant reversible reset:** an optional `ImportResetLease`,
reset scheduler, and two-phase reset owner form one exact offline transaction.
The ingress latch is atomic and independent of serializer ownership; local
execution is fenced before serializer/ticket drain; exact predecessor tickets
are retired before the canonical generation boundary; and one authenticated
successor clear must reach terminal delivery before exact reopen. Timeout,
panic, malformed echo, cancellation ambiguity, late return, or exhaustion
quarantines without release. This is a local lifecycle policy with no
production reset-notification caller.

**Implemented dormant Xbox owner:** `DormantRetainedUSBAdapter` implements both
contracts on one owner reference for exactly EP0, interrupt IN, and interrupt
OUT. It adopts only the reserved session lease, delegates all selection to the
existing persona coordinator, samples semantic input only during final
Prepare, routes typed feedback to a separate local executor, uses one bounded
drain and one disconnect clear, implements the optional reversible-reset
phases, and quarantines every ambiguous terminal. Its tests cover golden
protocol flow, forged/stale capabilities, reset retirement, active/staged/
pending reset races, exact successor clear, feedback, readiness, allocation,
completion/retire faults, panic/close races, noncooperative executor deadlines,
disconnect, and reconnect fencing.

The production slice now adds a narrow pre-legacy import branch in `server.go`.
It rejects overlapping ownership contracts, authenticates one exact bus/device
snapshot, and owns the retained authority from reservation through terminal
release. Conclusively legacy imports transfer back to the historical path
before device callbacks, preserving their ownership and response behavior.

The retained management/import cold path preflights the fixed one-interface,
two-endpoint shape before allocating, copies only the bounded scalar topology,
and uses that sealed value for the import reply, exact topology validation,
and all later lifecycle classification. It never copies caller-sized legacy
string, Microsoft OS, HID, IAD, class, or trailing descriptor collections. One
per-device plus per-owner
single-flight admission spans descriptor/capability/limits/identity/readiness,
the live stream, and terminal teardown. A callback timeout, panic, ambiguous
commit, or failed teardown permanently rejects both exact referents; competing
aliases cannot enter callbacks. Successful per-registration device admission
is released only after exact removal and the active lease join. Failed device
proofs and all owner identity/limits/quarantine proofs deliberately remain
latched until `Server.Close`; this prevents removal plus same-pointer re-add or
a fresh wrapper around the same owner from erasing a contradiction. Exact safe
BindRejected remains retryable.

DEVLIST remains joined to server connection shutdown through its retained
descriptor callbacks. API stream timeout, explicit device removal, and whole
bus removal all route through the server-owned admission cleanup. Whole-bus
removal atomically fences `Add`, drains exact device objects, and releases their
admissions. Authorized one-shot retirement is exact and idempotent: a device
already removed by an explicit device/bus operation is success, while an
address-reuse successor is never selected. A failed UNLINK ticket retirement
is latched as scheduler failure, forcing containment-only close rather than an
unsafe neutral/release path. Owner claim and device association publish under
one admission-map fence, so cleanup cannot orphan an active owner record.

Virtual-bus ownership is incarnation-based rather than address- or
pointer-based. Every successful `Add` receives a nonzero, nonwrapping opaque
registration token bound to the bus pointer, context, device, and export
metadata. Removal, API cleanup, descriptor snapshots, and one-shot retirement
must authenticate that complete tuple. Empty-bus cleanup likewise owns one
exact empty context, so an old timer cannot close a bus which became nonempty
and empty again. API stream state keys include the exact bus pointer and
registration token; terminal states are removed after their timers drain, and
timer callbacks authenticate the captured state object plus generation before
acting. Callback panics are contained at that asynchronous boundary.

Explicit retained registration adds a private bus-bound lifecycle capability
to that tuple. A bounded provisional Add is owned for duplicate/removal/close
purposes but is undiscoverable to DEVLIST, address lookup, import selection,
device enumeration, and context lookup until descriptor admission and exact
authority revalidation publish it. The non-consuming preflight and bounded Add
both reject unavailable nonzero 16-bit device-address or registration-token
capacity. Warm operation acquisition authenticates the capability in O(1) with
zero measured allocations. Removal marks it undiscoverable under the bus lock,
then drains its operation fence without holding bus/server map locks; exact
duplicate removal and concurrent bus/server close join the same completion.
Typed-nil or dynamically non-comparable generic device identities are rejected
before Add mutation.

One cold server lifecycle watcher links exact registration-context cancellation
back to retained admission cleanup when callers use the still-exported lower
VirtualBus removal/Close APIs. Immediate same-pointer admission additionally
reconciles a recognized inactive predecessor synchronously, so scheduler timing
cannot manufacture Busy before the watcher runs. Rollback retires the exact
recognized admission even if concurrent server bus removal already removed the
topology mapping.

Retained USB/IP framing validates the exact 16-bit bus and device components
of the wire `devid` before any callback, body read, or sequence reservation.
Unrepresentable retained topology is rejected before descriptor/capability
admission. For a valid command, the sequence is reserved when its fixed header
arrives and transferred atomically to the queued submission only after the
remaining OUT/ISO body is consumed. A partial body therefore cannot allow a
completed predecessor to free and reuse the same sequence. Endpoint topology
also enforces the interrupt attribute byte and speed-specific EP0/interrupt
packet and interval limits; unsupported SuperSpeed topology remains rejected
because this fixed descriptor shape has no companion descriptors.

Direct descriptor leases, DEVLIST leases, imports, and tracked connections all
enter the server shutdown join before callback admission. `Close` first fences
new buses and callbacks, then joins those leases. The connection wrapper runs
the underlying `Close` exactly once and replays its complete result, while
leaf-wise terminal classification suppresses only ordinary closed-connection
leaves and preserves joined transport or teardown failures.

EP0 lifecycle classification occurs when the control-lane FIFO reaches
service, not when the command reader first sees the setup bytes. This makes a
pipelined request observe the delivered configuration/alternate/halt state of
its predecessor and stamps the current binding generation before owner Stage.
Only a delivered successful lifecycle result rotates routing and retires the
predecessor generation.

**No-go** remains for generic registry/API product exposure or a release claim.
One explicit internal factory can now register exactly one caller-authorized
retained Xbox wrapper only when the server's default-off authority, a
representable existing bus, a free representable device address, and token
capacity match; it exposes only semantic input and joining exact removal.
Windows
binding/API visibility, end-to-end latency, physical feedback, and reconnect
provisioning are not established. The current authorized Xbox wrapper is
explicitly one-shot and is removed from its bus after Safe disconnect rather
than reusing a stopped executor.

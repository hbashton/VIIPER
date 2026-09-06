# Xbox persona late-admission transport coordinator

Status: dormant and unregistered. The package-local coordinator remains the
only mutation path for the canonical persona. A separate dormant retained-USB
owner now adapts its three lanes and typed local actions, but no production
USB/IP server interface, registry entry, identity/metadata/string source,
discovery path, Windows binding, hardware executor, or authenticated backend
reset notification uses it.

`device/xboxone/transport_coordinator.go` owns one canonical
`ControllerPersonaEngine`. Ownership is transferred to the coordinator at
construction; callers must not retain a second mutation path to the engine.
The coordinator exists to prove the ordering contract which an eventual USB/IP
adapter and server interface must satisfy.

## Proven transaction boundary

EP0, interrupt-IN, and interrupt-OUT may each stage one bounded request. A
staged ticket contains only copied host-request facts. It never calls a persona
claim method and therefore cannot win the persona lane merely because its
goroutine ran first. Semantic input is intentionally absent from an
interrupt-IN ticket. The latest validated input value is supplied only to
`admitInput` at final serialized admission, after any wait behind lifecycle,
feedback, or another active response. That same boundary refreshes the source
used to encode the mandatory START initial-input action immediately before the
lifecycle claim is selected. `admit` without semantic input retains the ticket
with `InputRequired`; it cannot freeze an older initial report.

Only `admit`/`admitInput`, called while a response serializer is owned, may
select an ordinary endpoint-response persona claim. `admitLocal` is strictly a
consumer of an already-pending zero-length claim: it never calls an engine
`Claim*` method and cannot preselect upstream work according to local-worker
scheduling. `beginUSBReset` is the one separate authoritative lifecycle
selector. It runs only with no active response or local lease. An ordinary
unadmitted predecessor claim may be retired only by the canonical engine's
successful generation boundary; an already selected or retrying ClearOutputs
blocks it. After that boundary succeeds, the coordinator clears the matching
ordinary pending route, atomically retires all three staged lanes, and selects
the successor clear-output obligation. A failed preflight leaves both ledgers
unchanged. The coordinator then enforces these rules:

- an immutable persona retry precedes every new endpoint action;
- an already selected upstream or local action blocks EP0 and interrupt-OUT;
- a pending lifecycle cursor, ready metadata packet, or due poll action is
  materialized before a new interrupt-OUT host command;
- interrupt-IN may admit and copy only an upstream action, into an exact slice;
- the lifecycle's mandatory initial-input claim is selectable only by
  `admitInput` carrying the latest validated semantic report;
- a zero-length action is transferred to a separately authenticated local
  execution lease and is never performed by the coordinator;
- a successfully acknowledged host command transfers its still-unadmitted
  first action to interrupt-IN or the local executor; acknowledging the OUT
  request does not resolve that action;
- a metadata ACK remains a preview until its local lease is actually delivered;
- failed IN/local work retains the engine's byte/value/sequence/epoch-exact
  retry and that retry cannot be overtaken; and
- if an interrupt-OUT RET_SUBMIT fails, its uncommitted host action is fenced.
  The engine retains the immutable retry, but normal IN/local/EP0/OUT admission
  cannot execute it. `beginUSBReset` retires it through the authoritative
  engine generation boundary and routes the successor clear-output action.

Coordinator ticket, local-lease, and ordering-token exhaustion is checked
before engine final admission. Selection-order exhaustion is reserved before
every engine `Claim*` call. Thus an exhaustion error cannot leave an admitted
or untracked engine claim. Unused order values are harmless gaps.

The internal lock order is coordinator mutex then canonical engine. No method
calls the server, an endpoint worker, an output executor, or a response writer,
so this package-local layer introduces no reverse edge.

## Implemented dormant retained-owner boundary

`retained_usb_adapter.go` transfers one engine into this same coordinator and
implements `retainedusb.Owner`, `retainedusb.ImportSessionOwner`, and the
optional `retainedusb.ImportResetSessionOwner` on one pointer-backed owner. It
does not introduce another mapping or protocol engine.
Its fixed bounds are one retained ticket for each of EP0, interrupt IN `81`,
and interrupt OUT `01`, a 254-byte EP0 response window, unchanged 64-byte
interrupt bounds, and no EP0 OUT data for the implemented table-4 subset. The
larger response window is separate from the fixed request slabs.

Construction is sessionless and starts no goroutine. The adapter accepts only
an exact authority/device/owner lease, adopts the authority-issued nonwrapping
session generation in `BindImport`, and starts its local worker only after an
exact Bound result. `Stage` copies request facts without semantic input or I/O;
`Prepare` is the sole ordinary selection boundary and samples the final semantic
input there. Terminal completion and retirement are exactly once. A canonical
completion/retirement error retains the matching adapter slot and quarantines
the import rather than exposing a reusable split ledger.

A host OUT arriving between START's current status and mandatory initial IN
remains staged with `ResultPending` when the coordinator reports
`InputRequired/SendInitialInput`. This exception is limited to interrupt OUT;
it does not acknowledge or execute that host command, preselect the input
baseline, or relax other rejected wait states. The eventual initial IN samples
the latest semantic input and its completion advances the readiness epoch and
latches a wake. No polling deadline is added for this wait. The exact OUT can
also be unlinked safely before IN arrives, without deferred feedback execution.

The local executor receives only typed actions selected by this coordinator,
including the four independent Direct Motor actuators and Guide LED. One
per-import drain arbiter permanently fences ordinary execution, joins the
adapter admission token by an absolute deadline, and is shared by panic and
outer-close paths. Disconnect then advances this coordinator through exactly
one `BeginDisconnect` clear and invokes the separately authorized neutral
method. A timeout, panic, failed clear, or ambiguous local terminal quarantines
the exact lease; reconnect is possible only after a proven detached/cleared
terminal and an exact higher session lease.

The dormant path also closes the narrower configuration-loss safety gap. A
control claim records whether it represents the actual Configured-to-Addressed
transition. Only after delivered EP0 completion does the engine reserve one
clear epoch; the coordinator immediately transfers that derived action to its
local lane, and the retained owner wakes the worker without waiting for a new
URB. A failed local clear is immediately reclaimed with the identical epoch
and receives its own paced worker wake, still without relying on unrelated host
traffic. While that clear is selected, ordinary endpoint admission and
reset/disconnect/reconnect boundaries are fenced rather than allowed to retire
it into a different generation. Failed EP0 completion and idempotent
configuration zero do not manufacture a clear. This does not implement
suspend/resume or a backend bus-reset notification.

The optional dormant retained reset transaction now gives `beginUSBReset` one
correct offline caller. A separately typed reset capability first latches
scheduler ingress without waiting for serializer ownership, then closes owner
Stage/Prepare and reversibly drains local execution. The scheduler joins any
active control/IN/OUT preparation and retires every exact retained ticket before
the adapter invokes `beginUSBReset`. The resulting one successor clear is
delivered through a reset-specific authenticated executor; owner and scheduler
admission reopen only after exact terminal delivery. Selected/retry clears,
staged-ticket ambiguity, deadline/panic/counter exhaustion, and any unproven
drain quarantine. Terminal connection close is not reused as reset authority,
and no successor URB is required.

This proves only the transaction supplied an authenticated reset event. In
usbip-win2 0.9.7.7, `drivers/ude/device.cpp:493` comments out the UdeCx reset
callback registration because its special server request returned `EPIPE`;
registered power callbacks send no server notification. VIIPER therefore has
no production reset caller and does not reinterpret SET_CONFIGURATION, endpoint
reset, connection close, or the disabled special setup request as reset proof.

These are package and scripted-executor proofs only. The descriptor routes are
official source facts, not evidence that Windows bound a device. No controller
or USB/IP connection is opened by this adapter.

## Production server boundary still missing

The legacy generic seams still cannot drive this coordinator without violating
its contract. A separate retained scheduler/import authority exists for offline
composition, but remains deliberately absent from command-reader and registry
routing:

1. `TransactionalControlDevice.ClaimControlTransaction`,
   `PreparedSource.SelectInputPresentation`, and
   `TransactionalInterruptOutDevice.ClaimInterruptOutTransaction` all run
   before `responseWriter` ownership. They also fix result and/or response size
   at that early selection point. The canonical persona cannot be claimed there
   without reintroducing cross-endpoint overtaking.
2. A truthful shared late-selection interface must run under response-stream
   ownership and return the final response kind, status, and variable payload
   length before the first byte. It must then provide an exact payload slice and
   exactly one post-write/flush completion while retaining current endpoint and
   generation fences. Existing devices must remain on their current paths.
3. Interrupt-OUT is processed synchronously by the command reader. The future
   opt-in needs a bounded, retainable OUT job: when the coordinator returns
   `Wait`, the exact request must remain pending while an earlier upstream or
   local action runs. It must not be ACKed, STALLed, passed to legacy handling,
   or retried while holding the response-writer lock.
4. The late IN boundary needs a latest/ordered semantic-input source queried at
   admission. Capturing `GamepadInputReportV1` when the host URB is staged would
   make a request which waited behind OUT/local work publish stale input and is
   explicitly not the production ABI.
5. The dormant import owner supplies exact cancel/drain, disconnect/neutral,
   reconnect, and reversible reset behavior, but the production server has no
   opt-in claim path which owns that lifecycle from import reservation through
   safe release and no authenticated reset/power event source. A failed OUT
   fence and reset transaction therefore remain unavailable to legacy routing.

These pieces should be introduced together. Adding only variable-length IN
selection would leave synchronous OUT unable to wait; adding only retainable
OUT would still let EP0/IN preselection acquire the engine lane early. No Xbox
adapter should be registered until the combined boundary, receive framing and
replay policy, external identity authorization, lifecycle neutralization, and
hardware conformance gates are complete.

## Focused verification

`transport_coordinator_test.go` covers staging purity and serializer-selected
order, local-consumer non-selection, mandatory egress before OUT, START wire
transfer to IN, the byte-exact latest START input selected after a wait,
byte-exact IN retry ahead of EP0, typed local retry ahead of a second OUT,
delivered-only metadata ACK application, failed-OUT fencing of every admission
lane, atomic three-lane reset retirement, post-wait ordinary-input selection,
forged/duplicate tickets and local leases, injected token/order exhaustion,
delivered-only configuration-loss clear routing, nil-receiver failure,
bounded same-instance concurrency, and allocation-free
interrupt-IN stage/retire.

`retained_usb_adapter_test.go` additionally covers exact lease binding, all
three lanes, final semantic-input sampling, mandatory START, metadata ACK,
four-actuator and Guide feedback, Pending readiness, forged/stale/cross-session
artifacts, every retirement scope, exactly-once completion, local retry,
panic-versus-close ordering, noncooperative execution/neutral deadlines, one
disconnect clear, delivered-only configuration-loss clear execution without a
successor URB, same-epoch self-woken clear retry, boundary fencing, reconnect
fencing, exact reset capability/replay, active local reset drain, staged-ticket
and selected/retry-clear fencing, one successor clear without a successor URB,
reset timeout/exhaustion quarantine, terminal containment, counter exhaustion,
and allocation-free warm Stage/Retire. The retained scheduler/import tests add
blocked control/IN/OUT serializer races, exact all-lane reset retirement,
reset/close waiting, and no false neutral/release. None is a production USB/IP
reset-notification test.

# Xbox One/Series offline persona to USB/IP adapter audit

Status: generic transactional EP0, prepared interrupt-IN, transactional
interrupt-OUT, late RET_SUBMIT serialization, and a narrow three-lane Xbox
retained owner are implemented and dormant. A separately typed exact reset
transaction is also implemented only behind the dormant retained-import
authority. usbip-win2 0.9.7.7 does not expose a usable reset notification to
this server, so production routing remains blocked fail-closed. No Xbox device
handler, registry import, bus device, attach path, or Windows binding claim was
added.

This audit asks only whether the existing offline `device/xboxone` persona can
be connected to VIIPER's current canonical USB/IP hot path without weakening
its transaction, lifecycle, identity, or output-neutralization contracts. The
answer at this source revision is no for production routing. The offline owner
now composes the canonical persona with the dormant import transaction at the
value-contract level, while production import routing, receive framing/replay,
identity, Windows, and hardware gates remain separately unresolved.

## Existing compatible primitives

- `usb.Device` provides one descriptor and non-EP0 transfer surface.
- `internal/inputpresentation.Source`, `AdmissionSource`, and the optional
  `PreparedSource` provide endpoint-scoped immutable input claims, a transport
  generation, terminal disposition, and final admission/retirement. A prepared
  source keeps bytes private until the USB/IP response serializer is owned.
- `internal/server/usb.endpointSchedulers` pre-creates endpoint workers and
  report buffers from descriptor capacities. A 64-byte full-speed interrupt
  endpoint therefore does not require a first-URB worker or report-buffer
  allocation.
- The scheduler owns endpoint-scoped routing, USB/IP unlink, endpoint reset,
  connection close, and presentation-generation retirement.
- `ControllerPersonaEngine` already owns the exact GIP lifecycle, nonzero
  sequence pools, private immutable wire images, metadata transfer, input,
  typed Direct Motor and Guide LED classification, and one clear-output epoch
  across reset/disconnect/reconnect.

These are necessary pieces. They are not a complete adapter seam.

## Implemented generic prerequisite: dormant late RET_SUBMIT serialization

`internal/server/usb.responseWriter.writeLateRetSubmit` is an unexported,
opt-in serializer primitive. No production call site uses it. A caller reserves
and validates an early maximum payload length, capped by the server's 16-MiB
USB/IP transfer limit, before final selection. Only
after the response writer owns the stream does a typed callback receive an
exact-capacity, zeroed payload window and select Pending, Data, successful
no-payload completion (including ZLP), or STALL. Data is trimmed to its final
actual length, and all unused scratch capacity is cleared before return.

Pending emits and flushes nothing and invokes no terminal callback. A valid
terminal result remains under the same serializer lock through `writeFull`, a
mandatory batch flush, and exactly one delivered/failed completion callback.
Short writes, flush failures, invalid results, oversized final Data lengths,
and callback failures are returned. Unexpected preparation and completion
panics are contained as errors and cannot strand serializer ownership. The
reusable typed-callback fast path is allocation-free after scratch warmup.

This primitive is not a USB transaction/job seam. It adds no retained EP0 or
interrupt-OUT worker, no cancellation or reset generation, no shared latest or
ordered semantic-input source, no import-session lifecycle owner, and no Xbox
coordinator binding. Existing response APIs and call sites are unchanged.

## Implemented generic prerequisite: transactional EP0 ownership

`usb.TransactionalControlDevice` is now an optional whole-request EP0 seam.
It is consulted in `handleUrbStream` before versioned HID snapshots, generic
descriptor/configuration handling, or legacy `usb.ControlDevice` dispatch.
No existing device implements it, so the absent-interface path is unchanged.

One value-only claim distinguishes a non-empty data response, successful no-data
completion, STALL, and unhandled fallback. Unhandled is the all-zero claim,
acquires no capability, and continues through the existing server path. Every
handled claim has a nonzero token and generation. The device must reject
forged, stale, duplicate, and already-terminal claims.

The USB/IP response is allocated and zeroed before serializer ownership. The
device's final admission runs only after the response-writer lock is held and
copies any data directly into the reserved RET_SUBMIT payload immediately
before `writeFull`. The post-write callback then reports exactly one of
Delivered, DeliveryFailed, or Cancelled while serializer ownership is still
held. A data result must fit both the USB/IP transfer buffer and the setup
packet's `wLength`. Only Delivered may commit a device effect. Invalid result envelopes,
admission failure, connection-write failure, and terminal-callback failure all
close the connection rather than falling through to generic handling.

For the server-recognized SET_CONFIGURATION, SET_INTERFACE, and
CLEAR_FEATURE(ENDPOINT_HALT) lifecycle requests, a transactional owner may
return successful no-data or STALL. Any other handled result fails closed.
After a successfully written no-data response, the device completion commits
first, then the server updates only its routing/worker state under the same
response-serializer ownership. It does not repeat legacy device callbacks.
STALL, admission failure, and write failure mutate neither server routing nor
workers.

Connection loss after admission is representable and regression-tested as one
DeliveryFailed completion with no committed device effect. USB/IP unlink is
not representable for synchronous EP0 in the current command reader: it cannot
read the later unlink command while it is processing/writing the control
response, and no EP0 job is enqueued in `endpointSchedulers`. This seam makes
no false unlink-cancellation claim.

## Source-level blockers

### 1. Externally supplied identity is incomplete

`UnregisteredControllerProfile` accepts caller-owned numeric identity and USB
power/interval values. It deliberately does not accept Manufacturer, Product,
or Serial Number strings. Its device descriptor advertises string indexes one
through three, while `usb.Descriptor` requires the corresponding string map
for the canonical server. `device.CreateOptions` can override only VID/PID and
passes an untyped JSON string to a registered handler; it is not an
authorization or validated string-identity seam.

Consequently an adapter would have to invent a string schema, string values,
serial construction, or authorization assertion. This audit does none of
those things.

### 2. Generic EP0 fallback remains separate from transactional ownership

`internal/server/usb.(*Server).processSubmitWithLifecycle` handles standard
GET_STATUS, SET_ADDRESS, GET/SET_CONFIGURATION, descriptors, and Microsoft OS
requests before consulting `usb.ControlDevice`. In particular:

- generic GET_STATUS always returns two zero bytes and cannot expose remote
  wake or endpoint Halt owned by `USBControlPlane`;
- SET_ADDRESS succeeds without advancing the persona's Default/Addressed
  state;
- descriptor requests are rebuilt from `usb.Descriptor`, bypassing the
  persona's claim/admit/resolve transaction;
- SET_CONFIGURATION updates server state without a configuration callback to
  the device; and
- a control-device fallback has only `(response, handled)`. It cannot express
  the required difference between STALL, a successful zero-length response,
  and unhandled fallback. Unrecognized requests currently reach a successful
  empty USB/IP response unless a server-owned lifecycle parser happens to
  reject them.

The strict MS-GIPUSB table-4 dispatcher must therefore opt into the new
transactional interface rather than `usb.ControlDevice`. That resolves
request/response/STALL transaction ownership. The delivered-only bridge in the
next section composes the three server-recognized lifecycle requests with the
server's active-configuration, interface-alt, and worker-generation state. No
Xbox adapter opts in yet.

### 3. Configuration and endpoint-Halt transaction ordering is now composable

The delivered-only bridge now keeps server configuration, interface-alt
routing, endpoint-worker retirement, and a transactional device's successful
EP0 effect in one serialized terminal callback. STALL and failed delivery do
not mutate either side. A transactional device owns its own Endpoint Halt bit
and GET_STATUS response; successful CLEAR retires the matching server worker
without invoking the legacy `EndpointResetDevice` callback a second time.

This resolves the three server-recognized lifecycle requests, not the full USB
power lifecycle. Bus reset, suspend/resume, and production
disconnect/reconnect still lack the one generation-fenced whole-device
notification required by the persona. The dormant Xbox owner now derives its
own output-clear obligation from delivered Configured-to-Addressed EP0
completion; that package-local proof is not a generic server notification or a
production route. Mapping a pipe clear to `BeginUSBReset` would remain wrong:
it would replace an endpoint-local Halt transition with a whole-device reset
and extra clear epoch.

The initial configuration mismatch is resolved generically for an opted-in
transactional EP0 owner: `handleUrbStream` seeds server routing at
configuration zero, matching USB Default state, while legacy devices retain
the historical descriptor-selected configuration. Nonzero endpoints stay
inactive until a delivered transactional SET_CONFIGURATION selects them.
Admission failure, STALL, and write failure leave both persona and server at
configuration zero.

### 4. Prepared interrupt-IN final admission is now available

`inputpresentation.PreparedSource` is the optional shared contract for a
source whose bytes must remain private at selection. It returns only an exact
capability and size. The endpoint worker reserves a zeroed RET_SUBMIT payload,
then calls `AdmitAndCopyInputPresentation` with an exact-length/exact-capacity
slice only while it owns both the response serializer and endpoint
cancellation fence. Exactly one Commit, Defer, or Retire resolution follows.

Malformed claims, cancellation before admission, rejected admission, socket
failure, flush failure, endpoint reset, configuration change, and connection
retirement are covered by adversarial real-worker tests. Existing Source and
AdmissionSource behavior is retained and byte-equivalence tested. No Xbox
persona implements PreparedSource yet.

### 5. The dormant whole-device transaction exists; notification is absent

Connection close retires the `inputpresentation.Source` generation, but
`usb.Device` has no disconnect callback. `EndpointResetDevice` covers only an
accepted endpoint pipe clear. The dormant retained boundary now has a separate
exact reset capability and a complete offline transaction: latch import
admission without waiting behind the response serializer; fence and join owner
local execution; drain the serializer and retire every retained lane with
`RetireDeviceReset`; advance the canonical persona once; deliver its one
successor-generation `ClearOutputs`; and reopen scheduler admission only for
the exact capability after terminal delivery. Failure or uncertain drain
quarantines without neutralization, release, or successor admission. This path
does not need a successor URB and has no production caller.

A terminal close which arrives while that exact reset owns the lifecycle now
latches the first close reason before waiting. That intent rejects both a
conflicting close reason and issuance of another reset, but a short waiter does
not change the owning reset's deadline or quarantine it. Reset Safe permits the
same reason to resume terminal close; reset quarantine completes the latched
close as the same contained terminal instead of orphaning its waiter channel.

The pinned usbip-win2 0.9.7.7 source explains why it cannot be routed yet:

- `drivers/ude/device.cpp:113-133` defines a UdeCx post-enumeration reset
  callback which calls `reset_port`, but `device.cpp:493` leaves
  `EvtUsbDeviceReset` registration commented out with `server returns EPIPE
  always`.
- `drivers/ude/device_ioctl.cpp:506-515,558-574` encodes that disabled path as
  an EP0 OUT class/other `SET_FEATURE(PORT_RESET)` request intended for the
  Linux-side usbip reset hook. That is usbip-win2 implementation behavior, not
  an MS-GIPUSB wire requirement and not an event VIIPER currently receives.
- `include/usbip/proto.h:20-23` exposes only SUBMIT/UNLINK and their replies;
  there is no independent USB/IP reset opcode.
- The registered link-power and function-suspend callbacks at
  `drivers/ude/device.cpp:211-264,486-490`, and the VHCI D0 callbacks at
  `drivers/ude/vhci.cpp:430-447`, only trace/validate and return success. They
  send no reset, suspend, resume, or power notification to the server.

Therefore neither the disabled special setup request, connection close,
SET_CONFIGURATION, nor endpoint reset is treated as reset proof. The persona
still cannot guarantee production-wide USB power/reset synchronization until a
separately authenticated backend callback exists. Suspend/resume and production
whole-device power notification remain absent as well.

### 6. Transactional interrupt-OUT completion is now available

`usb.TransactionalInterruptOutDevice` is the optional shared receive seam for
an active, non-isochronous interrupt OUT endpoint. Claim copies any payload it
needs to retain but makes no effect. Final admission validates the immutable
token/generation under response-serializer ownership. Exactly one Delivered,
DeliveryFailed, or Cancelled completion follows; only a delivered Accepted
claim may publish an effect. STALL is distinct from Unhandled legacy fallback.

The real-stream tests cover Accepted, STALL, legacy fallback, exact payload
ownership, admission failure, socket failure, forged/stale claims, and strict
interrupt-endpoint scoping. No existing device opts in. Like synchronous EP0,
the command reader cannot observe a later CMD_UNLINK while processing this
OUT transaction, so this seam makes no unlink-cancellation claim.

The seam does not itself solve GIP fragment reassembly/coalescing or receive
replay policy. Treating every USB OUT payload as exactly one message would
still contradict the persona's explicit blocker set.

### 7. The persona action lane now has a dormant late-admission coordinator

`ControllerPersonaEngine` intentionally has one ordered action claim lane.
EP0, interrupt IN, and interrupt OUT execute concurrently in the USB/IP
backend. A prepared IN selection must not hold an engine claim which causes a
later OUT final admission to fail merely because OUT obtained the response
serializer first; nor may OUT overtake a mandatory lifecycle or metadata
egress action. A plain mutex cannot establish this ordering.

Metadata ACK receipt formerly exposed a narrower version of the same error:
it advanced reliable state synchronously when bytes were decoded. It now uses
a pure `PreviewAcknowledgement`, a persona claim, final revalidation, and
delivered-only application.

`device/xboxone/transport_coordinator.go` is the package-local, dormant
ordering prerequisite for the broader adapter. Staging copies bounded host
facts but never claims the persona. Final admission serializes EP0, interrupt
IN, interrupt OUT, immutable retry, metadata, and local-feedback actions on
the one engine lane. It obtains the current semantic input only at final IN
admission, transfers delivered host-selected wire/local actions without
resolving them early, and fences an OUT action whose RET_SUBMIT was not
delivered until the authoritative USB-reset boundary retires it. Token and
order exhaustion are reserved before engine admission, so those failures
cannot leave an untracked claim.

This coordinator is not a USB adapter and no existing generic server path can
opt into it. The dormant writer primitive can serialize variable-result late
selection, but no EP0, IN, or OUT job calls it. The shared server still needs
one combined job boundary that retains waiting EP0 and asynchronous OUT work,
supplies latest ordered semantic input at final admission, and reports
whole-device lifecycle transitions. The writer primitive alone does not
establish the cross-endpoint ordering contract.

### 8. A three-lane retained owner adapter is now implemented but dormant

`device/xboxone/retained_usb_adapter.go` is the narrow adapter which the prior
audit intentionally left missing. It implements the repository-internal
retained hot-owner and whole-import lifecycle contracts on the same exact
pointer, and delegates every persona decision to the existing
`controllerPersonaTransportCoordinator`. There is no second mapping, feedback,
sequence, lifecycle, or protocol stack.

The adapter fixes one slot for each of EP0, interrupt IN `81`, and interrupt
OUT `01`; caps its relevant request/response storage at the proven 64-byte GIP
routes; stages immutable request facts without I/O; and performs final persona
selection and semantic-input sampling only in `Prepare`. Mandatory Hello/START,
current input, metadata, ACK, status, four-actuator Direct Motor, and Guide LED
therefore traverse the same canonical engine and coordinator as the offline
persona tests.

Construction starts no worker and predicts no session counter. An exact
authority-issued lease binds the authority, internal device key, owner, import
token, and session generation before the worker starts. Every later callback
compares the complete retained lease. Local execution is typed and occurs
outside Stage/Prepare/Complete/Retire and outside response serialization. One
per-import drain arbiter is shared by executor-panic and outer-close paths;
deadline-bounded admission joining prevents a hung executor from blocking an
owner callback forever. Disconnect authorizes exactly one separate neutral
clear. Only a delivered clear permits a detached predecessor to reconnect;
every ambiguous terminal retains and quarantines the exact lease.

The same adapter optionally implements the separately typed reversible reset
owner contract. Its first phase closes Stage/Prepare admission, reversibly
fences the local executor, and joins the local worker while preserving exact
retained tickets for scheduler retirement. Only after the outer scheduler
proves its serializer and all three lane queues drained may the second phase
invoke the coordinator's authoritative reset, deliver exactly one typed
successor clear through the reset-specific executor, and start the successor
worker. Exact repeat is observational; stale capabilities, selected/retry
clears, ticket ambiguity, executor ambiguity, and counter exhaustion quarantine
without reopening. Terminal containment can irreversibly upgrade a reset-drained
owner but cannot report neutral or release by itself.

Credential rejection is kept distinct from containment evidence. Forged,
stale, cross-import, unissued, and otherwise pre-boundary reset values return
`ImportResetInvalid` without closing healthy admission; rejected terminal drain
and disconnect values likewise return their `Invalid` states. Quarantined is
reported only after the exact import/reset has entered a containment boundary.

The tests deliberately inject canonical Resolve and retirement failures. The
adapter retains the exact slot and quarantines instead of clearing only one of
the two ownership ledgers. They also exercise noncooperative execution and
neutral methods, panic/close races, all reset/retirement reasons, final input
sampling, typed feedback, readiness, exhaustion, and warm-path allocation.

This is not a production adapter claim. There is no registry or `handleConn`
call site, no Windows descriptor binding result, no receive reassembly/replay
policy, and no hardware executor. The fixed endpoint routes are protocol source
facts only.

## Smallest standards-preserving integration sequence

The three shared transfer seams, package-local persona coordinator, retained
submission/import contracts, and dormant device owner are now implemented. The
following prerequisites remain before production routing is justified:

1. Add a source-proven and independently authenticated production backend
   callback for bus reset; do not infer it from the disabled usbip-win2 special
   setup, connection close, SET_CONFIGURATION, or endpoint reset. The dormant
   retained authority and owner already prove the exact reset transaction once
   such a callback exists: ingress latch, local drain, serializer/ticket drain,
   one successor-generation clear, and exact reopen. Suspend/resume and power
   notification still need their own reviewed lifecycle policy. The dormant
   owner also proves the narrower
   Configured-to-Addressed case: only a delivered `SET_CONFIGURATION(0)` derives
   one same-generation typed clear, and `Complete` wakes that local obligation
   without requiring another URB. A failed local delivery is reclaimed with
   the identical epoch and a paced self-wake; the selected clear fences
   unrelated admission and generation boundaries. Failed or idempotent EP0
   requests derive none.
2. Add a separately reviewed opt-in import path around the now-implemented
   dormant Reserve -> exclusive parked build -> exact Bind -> one-shot
   activation transaction. The production owner must retain the authority from
   reservation through terminal release, and legacy jobs must remain untouched.
3. Add bounded GIP receive framing/reassembly and a source-justified replay
   policy before connecting typed feedback to hardware.
4. Separately add a reviewed external identity input containing exact strings
   and an authorization decision. Do not infer it from VID/PID, metadata, a
   reference controller, or another project's descriptor.

Only after those gates pass should the dormant adapter be exercised through a
composed fake USB/IP stream. Registration, attach, Windows binding, and hardware
conformance remain later and separately authorized steps.

## Regression guard and verification scope

`internal/registry/xboxone_dormancy_test.go` asserts that importing the
canonical registry does not make common Xbox One/Series type names creatable.
The test protects the present fail-closed decision; it is not evidence that a
future persona will bind on Windows.

`internal/server/usb/control_transaction_test.go` uses a fake opt-in device and
a real USB/IP stream to prove preemption of generic descriptor/config/status
handling, data/ZLP/STALL/unhandled separation, exact final admission,
delivered-only configuration/alt/Halt server synchronization, STALL/write-
failure non-mutation, connection-loss completion, malformed/forged/stale
fail-closed behavior, and no partial configuration effect.
`internal/server/usb/response_writer_test.go` proves late variable-length Data,
ZLP, successful OUT acknowledgement, and STALL images; exact zeroed scratch;
Pending non-emission; invalid and oversized rejection before serializer
ownership; short-write and mandatory-flush failure; callback panic containment;
completion ordering against competing late and legacy responses; and a
zero-allocation warmed fast path. These are direct writer tests only: no
endpoint or device opts in.
`usb/device_test.go` exhausts the public claim shapes.
`internal/server/usb/prepared_input_presentation_test.go` proves final-copy,
cancellation, retry, write/flush, endpoint-generation, malformed-claim, and
legacy-equivalence behavior. `interrupt_out_transaction_test.go` proves the
transactional OUT outcomes and that no Accepted command commits before a
delivered RET_SUBMIT. Xbox metadata/persona tests prove ACK preview is pure and
delivery failure cannot advance reliable progress.
`device/xboxone/transport_coordinator_test.go` proves staging purity, final
serializer admission, mandatory-action ordering, current-input selection after
a wait, wire/local ownership transfer, immutable retry, delivered-only ACK,
failed-OUT fencing and reset retirement, forged-ticket rejection, and
pre-admission exhaustion behavior. It also proves delivered-only
configuration-loss clear routing and exact local completion. These are
package-level proofs only; they
do not exercise a USB/IP stream through the coordinator.
`device/xboxone/retained_usb_adapter_test.go` proves the exact dormant adapter
contract with a scripted local executor, including that successful
configuration loss wakes one clear without a successor request while failed
EP0 delivery does not, and that one failed local clear self-retries with the
same epoch while generation boundaries remain fenced. It does not constitute a
composed server stream, Windows, latency, or hardware result.
`internal/server/usb/retained_import_session_test.go` independently composes
the same combined-owner shape with the real parked scheduler and proves exact
abort/build ownership, bind/activation failure cleanup, late-return admission
revocation, close fencing, terminal owner drain/neutral, successor quarantine,
source-level dormancy, exact reset issuance/replay, reset/close races, and
quarantine without false release. `retained_submission_test.go` proves atomic
reset ingress fencing while control/IN/OUT preparation is active, exact
three-lane retirement, serializer timeout containment, exact reopen, and no
successor-URB dependency. It still does not route a USB/IP connection.

This audit uses only repository source and the already pinned official
protocol facts recorded in `device/xboxone/PROVENANCE.md`. It adds no new
external protocol fact or copied implementation. No device was registered or
attached, no driver or service was changed, and no hardware I/O occurred.

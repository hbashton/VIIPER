# Native VIIPER UdeCx bus

## Objective

Replace VIIPER's localhost USB/IP attachment path on Windows with a native
KMDF/UdeCx bus while retaining the existing, tested Go controller engines.
The native bus must expose the same HID, audio, microphone, isochronous, state,
and feedback contracts without a TCP loopback or an external USB/IP driver.

The first correctness target is feature parity. Performance work follows only
after transfer ordering, cancellation, teardown, and recovery are proven.

## Evidence and reference points

- Microsoft's UdeCx contract owns USB device creation, endpoint queues, reset,
  start, purge, and power lifecycle. Purge is asynchronous: pending work must be
  cancelled before `UdecxUsbEndpointPurgeComplete` is called.
- Released UdeCx behavior demonstrates that Windows can expose VIIPER's
  bidirectional isochronous PlayStation audio topology. Its normal-response URB
  completion behavior also supports using a real WDF DPC for every terminal
  path; the Microsoft UDE contract remains the authority for completion IRQL.
- The controller supports chained MDLs plus high-speed and SuperSpeed devices
  on separate USB 2 and USB 3 ports. UdeCx owns the mandatory
  post-enumeration child reset; configuration replacement enters VIIPER's
  generation-owned lifecycle stream only after Windows finishes enumerating
  the child.
- The lifecycle model uses explicit protocol negotiation, handle-scoped
  ownership, bounded manual queues, cancel-safe requests, generation-aware
  target teardown, and per-target synchronization rather than one global lock.

Reference code is used for architecture and documented protocol behavior. New
VIIPER code is independently named and implemented. See
`native/udecx/THIRD_PARTY_NOTICES.md`.

## Architecture

```text
DS4Windows
    | existing VIIPER API
VIIPER Go service
    | usb.Device controller engines (unchanged)
native UDE broker
    | versioned IOCTL ABI, overlapped inverted calls
VIIPER UdeCx KMDF driver
    | UDE endpoint queues and lifecycle
Windows USB/HID/Audio stacks
```

The Go service remains the controller model. It already owns descriptors,
control requests, HID reports, audio media, alternate settings, endpoint reset,
and the state rules learned while stabilizing DualSense and DualShock 4.
The kernel driver owns only Windows USB presentation and transfer lifecycle.

## Non-negotiable invariants

1. Every device is owned by exactly one open broker handle.
2. Every device identity includes a monotonically increasing generation.
3. Every endpoint incarnation includes a monotonically increasing generation;
   an address reused within one device generation cannot inherit requests,
   publications, workers, cancels, or completions from its predecessor.
4. Every operation token completes exactly once or is cancelled exactly once.
5. A completion from an old device or endpoint generation is rejected without
   touching a replacement that reused the numeric identifier or address.
6. Purge stops admission, cancels queued and in-flight work, waits for ownership
   to settle, then acknowledges UdeCx.
7. Driver unload and file cleanup leave no UDE device, request, or worker alive.
8. Endpoint queues are bounded. The broker enforces both its controller-wide
   ceiling and each child's negotiated pending-operation quota, so one busy
   media device cannot starve another controller. Saturation is observable and
   never overwrites live media or state silently.
9. Shared report state is snapshotted atomically before encoding. Media and
   state never share mutable buffers.
10. No raw user pointer crosses the ABI.
11. The ABI is size- and version-negotiated before any mutating operation.
    A revision mismatch has a distinct status that directs the service or
    installer to the exact matching native-driver package. The service also
    recognizes the parameter/length errors returned by native previews from
    before that distinct status existed, so an upgrade cannot strand ABI 1.7.
    ABI 1.9+ additionally returns the 32-byte identity compiled into the loaded
    kernel image: SHA-256 over the canonical source revision, driver-package
    version, ABI, and exact-capability tuple. The broker binary and schema-2
    accepted-package manifest derive the same value from their protected build
    inputs. A stale
    loaded image is rejected even when its on-disk replacement, ABI, and
    capability mask otherwise look correct.
12. Every packed wire structure has a compiler-independent size guard. ABI
    1.13 carries endpoint generation in the 108-byte operation, 72-byte
    completion, and 52-byte input-report records; the completion's final
    32-bit word remains explicitly reserved. Sizes never depend on compiler
    tail padding, and every field offset is guarded so a same-size reorder
    cannot silently desynchronize the C and Go layouts.
13. ABI 1.14 returns a fixed 40-byte receipt only after the exact
    `UdecxUsbDevicePlugIn` succeeds. The receipt echoes device identity and
    speed and carries the authoritative USB 2 or USB 3 controller port. User
    mode binds it to the exact controller instance obtained from the opened
    SetupAPI interface and to that file session's nonzero negotiated driver
    nonce. Authenticated add/list/ping responses expose these values as an
    opaque ownership tuple; consumers must not infer ownership from VID/PID,
    enumeration order, or stream generation.

## Kernel/user transport

The transport is intentionally split by USB semantics:

- interrupt-IN input reports use a manual-queue fast path. The Windows poll
  stays parked in the endpoint queue; one versioned `SUBMIT_INPUT_REPORT` call
  completes a waiting URB without an allocation or broker round trip. ABI 1.10
  classifies newly queued controller states separately from deadline-generated
  cadence snapshots. ABI 1.13 additionally binds every report, operation,
  cancel, completion, publisher, and worker to the exact endpoint incarnation.
  Each endpoint holds a bounded preallocated transition
  FIFO and one latest-state snapshot: Windows consumes every accepted edge in
  order, while idle 1 ms DS4/DualSense reports update only the snapshot and
  cannot crowd edges out. The passive ready callback copies one report directly
  and transfers terminal ownership to the required completion DPC. The Go
  publisher allocates one descriptor-sized buffer at endpoint start.
  Each active publisher also owns one reusable service-deadline timer. The
  controller receives that timer's channel separately from the lifecycle
  context, so an idle deadline still replays cached state while purge, reset,
  D0 exit, and owner shutdown still cancel the read. This preserves the exact
  DualSense/DualShock report builders, counters, and sensor timestamps while
  removing the timer-backed context allocation from every service interval.
  The serial overlapped IOCTL copies the report before that buffer is reused,
  eliminating per-sample Go heap work without shared-memory lifetime hazards.
  Input timing is therefore host-poll-driven rather than dependent on a second
  physical report arriving after the poll;
- control, interrupt-OUT, isochronous speaker/microphone/haptics, feedback, and
  every lifecycle transition use the cancel-safe ordered inverted-call broker.
  VIIPER posts multiple `DEQUEUE_OPERATION` requests, processes each immutable
  operation through the existing `usb.Device` interface, then submits
  `COMPLETE_OPERATION`. Native microphone engines encode directly into the
  host URB's packet regions at each reserved USB service point. They neither
  allocate a packet nor create a per-packet timer; an unavailable source frame
  becomes the legal nominal zero packet immediately. The adaptive PCM buffer
  also observes the actual capacity reserved for each URB. If Windows reserves
  only nominal capacity, a pending long clock-correction packet remains owed
  instead of being consumed and silently truncated. The USB/IP microphone path
  retains its existing allocation and timeout ownership contract.

The input counters intentionally measure opposite sides of the fast path:
`InputReportsSubmitted` counts accepted publications, while
`InputReportsCompleted` counts Windows interrupt-IN polls. Idle publications
may coalesce into the latest snapshot, but transition publications do not.

Microsoft's UDE programming guide and host-controller I/O guide require URB
completion at `DISPATCH_LEVEL` for USB-client compatibility. They additionally
require synchronously handled URBs, `EvtIoCanceledOnQueue`, and request-cancel
paths to complete from a separate DPC. The individual
`UdecxUrbComplete`/`UdecxUrbCompleteWithNtStatus` API pages conflict with that
guidance by listing `PASSIVE_LEVEL`; the current WDK declarations carry no IRQL
SAL annotation that resolves the conflict (verified against the project's
pinned WDK 10.0.28000.1839). VIIPER follows the UDE-specific
compatibility rule because it explicitly covers terminal and cancellation
behavior and agrees with released UdeCx behavior. VIIPER uses a real WDF DPC
and never synthesizes an IRQL raise.

One preallocated controller WDF DPC is the only function that calls either
UdeCx URB completion API. A request-context intrusive queue holds request and
endpoint references without per-transfer allocation. Broker replies,
cancellation, admission rejection, and direct interrupt-IN all claim exactly
one queue entry. Every endpoint queue registers `EvtIoCanceledOnQueue`,
overriding KMDF's synchronous default so a URB canceled before dispatch follows
the same path. The DPC runs at asserted `DISPATCH_LEVEL`, makes the terminal
call, then retires the broker slot or direct endpoint operation and signals the
drain event. Teardown closes admission, waits the tracked broker count, joins
the completion count, and uses `WdfDpcCancel(..., TRUE)` only after the list is
empty; a canceled pre-dispatch invocation is re-armed rather than abandoned.

Buffer lookup, validation, copies, and user-mode publication remain on the
explicitly passive queues and work items. A late cached poll still crosses the
preallocated endpoint work-item boundary because `WdfIoQueueReadyNotify` can
run inline on the UdeCx submitter thread; that work item consumes one cache
token and prepares the buffer, then transfers terminal ownership to the shared
DPC. No payload, endpoint ordering, or isochronous scheduling behavior changes
at this execution-level boundary.

Input publishers start and stop from UdeCx endpoint lifecycle notifications,
retain their sequence across a purge/start cycle, and are cancelled before
device removal. Removal rejected before UdeCx takes the child restores the
active publishers, so a retry does not strand the current generation. Once
ownership transfers, a terminal UdeCx removal fault restarts the controller
and the removed generation remains closed.

Dynamic endpoint cleanup leaves an address-scoped retirement tombstone for
the current device generation. This lets the kernel acknowledge and discard
the final admitted input report that can cross asynchronous cleanup before
user mode consumes the ordered purge event, without accepting reports for an
endpoint that was never configured.

The broker owner session is deliberately one-shot. Stopping the user-mode host
cancels endpoint lanes that may already own dequeued kernel requests; those
requests cannot be reconstructed safely in a restarted goroutine. VIIPER must
close that driver handle and negotiate a fresh `Client`/`Host` session. It never
guesses a new endpoint sequence baseline or risks abandoning a USB request from
the retired file session.

Session shutdown owns a cancellation context before `Serve` is scheduled, so
even an immediate stop cannot miss host cancellation. The client waits for all
dequeue workers, endpoint lanes, input publishers, and their completions to
finish before cancelling overlapped kernel I/O and closing the exclusive broker
handle. The handle is therefore always the last object released.

This deliberately removes TCP, WSK, USB/IP framing, and attach bookkeeping.
The direct input lane removes the highest-frequency HID broker path without
mixing report ownership into the proven PlayStation media/state transport.
The authenticated DS4Windows-to-service API which feeds that lane keeps its
wire format but reuses one bounded receive slab per connection and decrypts
records in place before copying into the caller's buffer. Full-duplex access
uses independent read/write locks; concurrent writers are serialized into
whole records. Client and server records use separate 32-bit nonce domains and
monotonic 64-bit counters; receivers enforce both the direction and exact next
counter. A partially emitted record closes the now-unrecoverable stream instead
of permitting a corrupt retry.
Once correctness gates pass, high-rate media payloads may move to a
preallocated ring while keeping the same token/generation lifecycle. Control
and lifecycle operations remain IOCTL based.

### Operation identity

Every operation carries:

- device ID and generation;
- a globally unique token for that generation;
- endpoint address, endpoint-incarnation generation, and transfer direction;
- endpoint attributes, interval, and maximum packet size copied from the
  UdeCx endpoint descriptor;
- operation kind and URB function;
- transfer flags, setup packet, and start frame where applicable;
- ordered isochronous packet metadata;
- a bounded payload.

### Device creation

VIIPER serializes the exact descriptor set returned by the controller engine:
device, configuration, BOS, language/string records, and device-speed policy.
The driver validates all offsets and lengths before constructing a UDE device.
Descriptor normalization required by UdeCx is a named policy, not an implicit
mutation: high-speed bulk packets are 512 bytes and interval conversion is
covered by descriptor tests. The reserved Microsoft OS 1.0 `0xEE` string is
published explicitly when a controller exposes one, preserving WinUSB binding
for vendor interfaces such as the Switch 2 Pro path.

## Lifecycle

```text
Absent -> Creating -> Enumerating -> Active
                              |       |
                              v       v
                           Failed <- Purging -> Removed
```

- **Creating:** validate ABI, descriptors, limits, and owner handle.
- **Enumerating:** create UDE device/endpoints and plug it into UdeCx.
- **Active:** accept transfers and endpoint lifecycle notifications.
- **Purging:** close admission, cancel all tokens, drain queues, acknowledge
  endpoint purge, and invalidate the generation.
- **Removed:** delete the UDE object and release all references.

Power loss, owner process exit, DS4Windows restart, VIIPER restart, and explicit
unplug all converge on the same idempotent purge path.

### Composite alternate-setting identity

UdeCx can report unreliable `InterfaceNumber` and `NewInterfaceSetting` values
for composite-device alternate-setting changes. VIIPER does not install a
system-wide root-hub upper filter to compensate.

Every endpoint callback already supplies the authoritative endpoint descriptor.
The kernel copies its address, attributes, interval, and maximum packet size
into the versioned broker operation. User mode matches that complete signature
against the immutable controller descriptor and derives the owning interface
and alternate setting. Endpoint start activates it; purge returns it to zero
only after the last endpoint belonging to the active alternate is gone. The
first ISO URB is also authoritative activation, closing the cross-worker race
where media reaches user mode before its start notification. Numeric UdeCx
interface fields are only hints for alternates that contain no endpoints.

### Broker service migration transaction

Windows native mode is owned by the `VIIPERNativeBroker` Service Control
Manager service, never by an HKCU Run entry or tray process. Installation and
update use one machine-wide mutex and the following fail-closed transaction:

1. Resolve Program Files and ProgramData through Windows Known Folder APIs.
   Resolve one target interactive-user SID before the first mutation and use
   that same identity for the credential ACL and every legacy HKU/task/process
   operation. An elevated caller may supply the bootstrapper-origin SID;
   otherwise VIIPER proves the shell or active-console token and fails closed
   when no unambiguous interactive user exists.
   Native installation is accepted only from the managed
   `Program Files\VIIPER\viiper.exe` or
   `Program Files\DS4Windows\VIIPER\viiper.exe` layout. Every component is
   opened as a non-reparse point and retained without delete sharing through
   authenticated startup. The executable and credential must each have one
   hard link, and their retained file handles also deny write sharing. Every
   managed directory and the PE executable must already carry the exact
   protected, administrator-owned package ACL before the service command will
   register the executable as LocalSystem code. The command validates and
   retains those objects read-only; it never treats an in-place ACL rewrite as
   proof because that cannot revoke handles opened under an older weak ACL.
2. Provision a freshly rotated, nonempty credential under
   `%ProgramData%\VIIPER` only after every prior broker owner is stopped. An
   existing value is retained solely for rollback and is never trusted as the
   new secret, preventing a standard user from pre-seeding a known key. The directory
   is held open without delete sharing while the key is staged and published
   with `MoveFileExW(REPLACE_EXISTING | WRITE_THROUGH)`. Its protected DACL
   grants full control to SYSTEM and built-in administrators and read access
   to the installing user's SID; no localized account name is parsed.
3. Snapshot the prior service configuration, stable running state, failure
   actions, SCM object owner/DACL, and legacy startup commands. The prior
   service executable is parsed from the SCM command line, validated against
   the already-protected managed-file ACL contract without mutating it, and
   every path component remains locked against replacement until commit or
   exact rollback. Before a LocalSystem command is installed, the service object is
   required to already have a protected DACL granting control only to SYSTEM
   and built-in administrators. The transaction rejects a permissive existing
   service rather than relying on a DACL rewrite that cannot revoke previously
   opened service handles; rollback restores and verifies the exact prior
   descriptor before any prior service restart. Task Scheduler enumeration is
   fail-closed and distinguishes a missing root task from provider/access
   failure. Stop the old SCM instance and the exact snapshotted scheduled-task
   instance before considering residual HKU Run processes. Only processes
   whose full executable path and token-user SID match the target registration
   are terminated. Process handles remain open from identity verification
   through termination, preventing PID-reuse and cross-user mistakes.
4. Negotiate the packaged native driver, then create or update an automatic,
   own-process LocalSystem service with explicit `native-ude`, credential, and
   log arguments. Arguments are escaped with Windows command-line rules rather
   than passed through a shell. The few legacy Task Scheduler operations use
   the absolute, non-reparse system PowerShell path rather than process `PATH`.
5. Apply bounded recovery (two restart attempts followed by no action), start
   the service, and require an authenticated ping proving `Ready=true`, exact
   native transport, ABI, and negotiated capabilities. The broker's current
   package-version field is compile-time metadata rather than installed-driver
   attestation and is intentionally not treated as verification.
6. Only after that proof, compare-and-remove the legacy HKCU registration. The
   exact `HKU\<SID>` hive and existing Run key handles stay open for the entire
   transaction, preventing logoff/unload from turning rollback into an orphan
   registry subtree. Run ownership is compared as data plus `REG_SZ` versus
   `REG_EXPAND_SZ`; both originally present and originally absent Run/task
   states are CAS-checked immediately before commit. Task names are matched
   with Task Scheduler's case-insensitive identity rules. The
   exact legacy scheduled task stays registered but disabled: exported task XML
   omits its registered ACL and cannot recreate Password-logon credentials, so
   delete/re-register would not be an exact transaction. Disable, stop, wait,
   and validation occur in one bounded provider operation; rollback re-enables
   only the same retained task. Task XML is transported as explicit UTF-8
   through an ASCII base64 envelope. Re-authenticate the broker after legacy
   ownership is disabled, closing a restart race. The service command runs
   without a tray in session 0.

Any failure receives a fresh rollback deadline: a newly created service is
stopped and deleted, or the previous configuration, recovery policy, and
running state are restored and verified before the prior service can restart.
The credential rollback uses the same atomic publication path. A legacy process
is restarted only when it was actually running before migration: scheduled-task
processes are restarted by Task Scheduler in their original security context,
and HKCU processes use the interactive shell token rather than the elevated
installer token. Full scheduled-task XML is restored after partial removal.
Uninstall holds the same mutex across service, startup-registration, and process
cleanup. It resolves and snapshots every target before mutation, stops the
service and exact legacy owners, compare-removes HKU Run ownership while
retaining any exact scheduled task disabled, and marks
the service for deletion only as the final fallible operation. If anything
before successful deletion fails, it restores registrations, the exact service
configuration/recovery policy/stable state, and only the legacy owners that
were previously running. Every Task Scheduler subprocess is context-bounded so
a wedged provider cannot retain the installer mutex indefinitely.

## Synchronization model

- Separate controller locks protect the device table and broker-owner
  registration; the broker spin lock is the operation-admission boundary.
- Broker file callbacks explicitly run at `WdfExecutionLevelPassive`, matching
  their wait-lock, synchronous-queue-purge, and pageable cleanup operations.
- Every UdeCx USB-device and endpoint object explicitly requests
  `WdfExecutionLevelPassive`. Microsoft permits the device power/reset,
  endpoint-configuration, start, purge, and reset callbacks at up to
  `DISPATCH_LEVEL`, but VIIPER's callbacks create WDF/UdeCx objects and acquire
  the embedded device-table `FAST_MUTEX`, operations whose contract is below
  dispatch level. The KMDF controller default is dispatch execution, so relying
  on inherited or presently observed callback context is not a valid safety
  contract.
- Controller removal closes a single `ShuttingDown` admission gate in
  `EvtDeviceSelfManagedIoCleanup`, while the controller's queues, timer,
  completion DPC, locks, and broker storage are still valid.
  Cleanup first joins any file cleanup that crossed the owner lock before the
  gate, then purges user-mode queues, aborts every admitted broker operation,
  and uses KMDF's preceding non-power-managed queue purge as its terminal
  endpoint fence. While a shared device-index lock still pins every endpoint,
  it requires `WdfIoQueueDriverNoRequests` and `ActiveOperations == 0` under
  the broker lock. The former closes the callback-delivered/pre-first-driver-
  instruction window; the latter joins forwarded work and its terminal DPC.
  Queued host polls are deliberately not part of this predicate because UdeCx
  owns those requests and issues the endpoint-purge transition while consuming
  the child. Only after that proof does cleanup join tracked and untracked
  completion counts and the final DPC, then revoke and consume UDE handles.
  The final controller `EvtCleanupCallback` performs only invariant checks
  because KMDF has already cleaned up child objects by then.
- UdeCx USB-device deletion remains asynchronous. Shutdown snapshots and
  revokes each device under the embedded shared/exclusive push lock, invokes
  `UdecxUsbDevicePlugOutAndDelete` after dropping the lock, and never waits for
  child cleanup from the PnP cleanup callback. Embedding the push lock in the
  controller context keeps endpoint/device cleanup independent of sibling WDF
  child deletion order.
- Removal atomically revokes the UDE handle from the device table and retires
  its logical active count before `UdecxUsbDevicePlugOutAndDelete`. `Devices[]`
  is the sole slot-ownership table, so a slot can be reused after the consuming
  UdeCx call returns even when KMDF defers the old child's object cleanup. No
  path dereferences or restores that invalidated UDE handle.
- File cleanup first closes create/destroy admission, then joins only those
  finite UdeCx API calls before removing the owner's remaining logical devices
  and releasing the exclusive controller owner. Each retired child keeps its
  own reference on the old file object until `EvtCleanupCallback`; that late
  physical rundown cannot block a successor owner or clear a reused slot.
- Child teardown aborts every exact-device reset/configuration request before
  consuming its UdeCx handle. A reset notification still queued for user mode
  is retired in O(1) and emitted as a benign cancel; an already delivered reset
  keeps only a `(token, device ID, generation, owner)` tombstone, so its late
  acknowledgement cannot reopen or mutate a replacement child. Management-slot
  reuse remains closed until that acknowledgement arrives or KMDF invokes the
  old owner's `EvtFileClose`, the documented post-I/O boundary. No teardown
  performs a global notification-ring scan or blocks unrelated controllers.
- A post-transfer UdeCx removal failure is terminal for the controller, not
  retryable for the child. The kernel accepts the broker's removal request and
  requests a PnP controller restart; user mode can retry only failures returned
  before ownership reached UdeCx.
- Each device has a short-held state lock and independent endpoint queues.
- User mode serializes controller-engine lifecycle mutations as one complete
  transaction. Endpoint reset cannot overlap an endpoint start/purge, device
  reset, or alternate-setting change, while ordinary HID and media transfers
  remain concurrent on their independent endpoint lanes. The serialization
  object belongs to one `(device ID, generation)` session; a blocked reset on
  one controller cannot stall lifecycle or media activation on another.
- Each endpoint owns a drain event covering both broker-forwarded URBs and the
  direct interrupt-IN fast path. UdeCx itself owns and purges the framework
  endpoint queue; VIIPER never starts or purges that queue. The purge callback
  closes admission and cancels only the requests already forwarded into
  VIIPER-owned paths. A passive work item only observes the associated queue:
  `WdfIoQueueDriverNoRequests` proves that every WDF-delivered request has
  returned to framework ownership, while the broker-lock rundown proves every
  forwarded request and terminal DPC has released the endpoint. It does not
  wait for UdeCx-owned queued host polls or for the queue's READY bookkeeping
  to clear. Only then may the work item call
  `UdecxUsbEndpointPurgeComplete`. A pipe can therefore never restart or
  disappear across a live or pre-callback-delivery request, and the client
  never mutates UdeCx-owned queue state.
- Endpoint reset and endpoint-configuration callbacks are asynchronous UdeCx
  management requests, not notifications. ABI 1.10 preserves the
  generation-bound management tokens introduced in ABI 1.8 and adds the
  source-bound loaded-kernel identity to negotiation. Windows receives the
  request completion only after the Go controller engine has applied the reset
  or alternate-setting transition. Start, purge, and power notifications
  remain unacknowledged and cannot add a media round trip.
- Endpoint reset owns a gate separate from endpoint purge. The UdeCx reset
  callback closes both broker and direct-input admission under the broker lock,
  cancels forwarded work, and defers its acknowledged lifecycle event until a
  read-only queue sample reports `WdfIoQueueDriverNoRequests` and the endpoint
  rundown reaches zero. Unlike purge, reset may leave a parked interrupt poll
  queued and the queue ready. This weaker queue predicate is stable because the
  UdeCx reset is asynchronous: the endpoint cannot process successor transfers
  until VIIPER completes the reset request. User mode stops and joins that
  endpoint's direct-input publisher before applying recovery. At owner
  acknowledgement the kernel repeats the exact `(device ID, generation,
  pinned WDF device/endpoint, reset epoch)` proof, clears only that live reset
  gate under the admission lock, and immediately completes the UdeCx request.
  Generic framework references prevent opaque handle recycling until every
  management-slot terminal path has cleared the slot outside the broker lock.
  Missing, purged, removed, or reused identities fail closed and receive no
  stale reset publication or successor gate change. Reset never calls
  purge-complete or waits for a later start callback, matching UdeCx's distinct
  reset and purge contracts.
- Device configuration replacement closes direct input admission in the
  kernel callback and pauses
  every user-mode publisher before controller state is cleared. Every endpoint
  first passes the same reset-specific driver-owned-request/rundown proof; if
  purge or removal wins, the actual reset request fails without publishing a
  dead generation. Successful device-reset admission also advances a private
  64-bit epoch. Endpoint reset admission captures that epoch, so a later device
  reset deterministically supersedes every older endpoint reset even when user
  mode acknowledges them out of order. Admission and active publishers reopen
  only after a second exact-generation/object/epoch proof at acknowledgement,
  so no HID snapshot or late terminal callback can cross the reset boundary.
  Configuration replacement alone uses this one-child-at-a-time device gate;
  concurrent reset transactions are rejected instead of interleaving two
  controller resets. The mandatory post-enumeration reset stays entirely in
  UdeCx, because an emulated descriptor has no backing physical reset to
  coordinate and Windows cannot continue child enumeration while that reset is
  waiting on user mode. Device initialization also completes synchronously.
- The controller's default KMDF queue is parallel and completes interrupt-IN
  submissions directly. Mutation, broker, and lifecycle IOCTLs alone move to
  the serialized control queue. This removes a redundant KMDF forwarding and
  dispatch boundary from every fresh report while large media completions still
  cannot head-of-line block controller input.
- Each fast interrupt-IN endpoint has its own passive lock. Different
  controllers publish concurrently, while accidental concurrent submissions
  for one endpoint cannot reorder reports or replay a coalesced sequence.
- If reports arrive before Windows posts its next HID poll, the passive
  manual-queue ready callback copies the oldest transition (or latest idle
  snapshot when no transition is pending) and hands
  terminal ownership to the shared completion DPC. The required asynchronous
  DISPATCH_LEVEL completion remains intact without an intervening system-worker
  scheduling hop on first poll, resume, or idle recovery.
- A lost ordered lifecycle notification faults both the broker and the direct
  interrupt-IN producer lane. Already-published broker completions remain
  drainable, but no new controller state is admitted into a generation whose
  power/reset history is no longer trustworthy.
- Endpoint start opens the kernel admission gate before publishing the ordered
  start notification. The first fresh input snapshot after resume therefore
  cannot consume its sequence against a still-purged kernel endpoint; the
  callback itself is the single UdeCx restart boundary for both paths.
- Every published operation also carries a per-device publication sequence.
  Endpoint lanes remain independent, but device-wide D0 transitions use that
  sequence to reject a delayed pre-exit start notification. Multiple overlapped
  dequeue workers therefore cannot resurrect an input publisher outside D0 or
  consume a physical state snapshot behind a power boundary.
- Parallel media callbacks receive a per-endpoint admission sequence under the
  broker lock. An URB cannot publish ahead of an earlier live unpublished
  admission; cancellation retires the admission before dispatch resumes, so
  the public endpoint sequence remains contiguous without limiting media to
  one in-flight URB.
- Media callbacks do not take the controller lock.
- Transfer buffers obey both dimensions of the Windows USB contract: the URB
  declares the total transfer length, while a pointer returned by
  `UdecxUrbRetrieveBuffer` is used only within its separately reported mapped
  span. Chained or short mappings fall through to a bounded MDL-chain walk;
  the driver never treats the URB length as permission to overrun one mapping.
- Interrupt-IN queues are manual and completed from a generation-owned,
  sequence-checked transition FIFO plus latest-state snapshot. The passive
  queue-ready callback copies directly, then the shared DPC performs the only
  terminal UdeCx completion. A synchronously replenished Windows poll drains at
  most one queued transition and is otherwise left parked for the next
  producer. Endpoint purge/reset and device reset/D0 exit invalidate the FIFO,
  snapshot, and delivery token after closing admission, so no held button can
  cross a lifecycle boundary.
  Output and media endpoints retain independent ordered queues.
- A direct input report that was already submitted when D0 exit, device reset,
  unplug, or endpoint purge begins is acknowledged and discarded at that exact
  lifecycle boundary. The kernel closes admission in the UdeCx callback itself
  rather than waiting for the user-mode notification. Stale generations and
  replayed sequences remain hard failures, so normal teardown cannot fault the
  exclusive broker session.
- UDE callbacks never wait on user mode while holding a WDF lock.
- Blocking work is represented by cancelable WDF requests, not sleeping kernel
  threads.
- Every mark-cancelable transition revalidates its prior state under the broker
  lock. If KMDF invokes cancellation before that lock is reacquired, the cancel
  callback's DPC-completion ownership is final and cannot be overwritten
  by admission or publication.
- Broker dequeue validation, wait-count admission, and transfer into the
  manual inverted-call queue share the owner lock with file cleanup. No close
  can finish purging that queue and then have an already-validated request
  appear behind the purge boundary.
- Child creation is protected by an owner-admission barrier. Cleanup closes
  admission under the owner lock and waits for every admitted UdeCx create and
  PlugIn transaction before enumerating owned children. UdeCx calls run without
  the owner lock held, avoiding callback deadlocks while preventing an orphaned
  child from being published behind cleanup's enumeration boundary.
- Overlapped cancellation is outcome-based rather than intent-based. After
  `CancelIoEx`, the completion packet decides whether the operation completed
  normally, was actually aborted, or failed. A successful create/destroy can
  therefore never be reported as cancelled merely because the context deadline
  raced its completion.
- Completion lookup is keyed by `(device ID, generation, token)`.
- Failed transfers do not need to fabricate a successful ISO packet table.
  Successful completions are canonical: OUT replies carry no payload, every
  ISO reserved field is zero, packet extents stay inside the transfer buffer,
  and the sum of actual packet lengths equals the reported completed bytes.
  The host-owned packet offsets are immutable across the broker boundary;
  user mode may return only each packet's actual length and status. Sparse IN
  payload span is independent of the completed-byte total, and the kernel
  derives the URB error count from the returned per-packet statuses.
- Each isochronous endpoint owns a virtual USB frame reservation clock. ASAP
  URBs reserve the first frame after the current or previously queued window,
  and the driver returns that actual frame in `StartFrame` as required by the
  Windows USB contract. Explicit schedules advance the same endpoint clock;
  reset, purge, and start clear it so an old pipe lifetime cannot skew a new
  media stream.

This uses per-target ownership and manual request queues while accounting for
UdeCx's endpoint-specific purge contract.
Host-side create/remove gates are keyed by stable device ID: generations of
one controller cannot cross, while a slow PnP transition for one pad cannot
stall an independent pad's registration or removal.
- PnP creation is revalidated after the overlapped kernel transaction. If a
  one-shot host session stopped while creation was in flight, that exact child
  generation is transactionally destroyed and cannot be published into the
  terminal host.

## Delivery checkpoints

1. Versioned ABI, independent C/Go layout tests, architecture notes.
2. Installable root-enumerated KMDF/UdeCx controller with negotiation, owner
   cleanup, diagnostics, and no virtual child.
3. Dynamic HID-only child, control endpoint, interrupt IN/OUT, reset and purge.
4. VIIPER Go broker implementing the existing controller interface.
5. Xbox, DualShock 4, and DualSense HID/state parity.
6. Bidirectional isochronous audio and microphone parity, alternate settings,
   haptics, lightbar, triggers, and reconnect recovery.
7. Fault injection, soak, latency, CPU, install/update/rollback, and signing.

## Release gates

- No verifier findings under KMDF/USB/UdeCx stress.
- The completion-execution contract has a two-machine signed-live gate. On a
  clean disposable Windows 10 1809 x64 machine, stage
  `Enable-ViiperUdeVerifierForNextBoot.ps1` (which selects only
  `ViiperUde.sys`, Microsoft's `/standard` checks, and `oneboot`), restart, and
  run `Invoke-ViiperUdeLiveValidation.ps1` in `Production` mode with
  `-RequireDriverVerifier`, at least three iterations, both media/input probes,
  and at least 180 seconds of media. On the current Windows 11 x64 HLK target,
  run the same command with `-RestartRootDevice -ReleaseGate`. Both runs must
  exercise normal broker replies, mark-cancel races, owner-process death,
  endpoint reset/purge, root removal, and concurrent control/interrupt/ISO
  traffic; `verifier /query` must show the reviewed image and there may be no
  verifier violation, bugcheck, stuck request, nonzero terminal pending count,
  or late duplicate completion.
- The HLK gate is the complete Studio-generated applicable playlist, without
  manually suppressing tests, for the VIIPER root controller and every
  enumerated USB/HID/audio child on Windows 10 1809 x64 and the current Windows
  11 x64 certification target. This includes every applicable Device
  Fundamentals I/O, PnP, power, reliability and security test plus USB, HID,
  and Audio tests. Every result must pass, or carry a Microsoft-approved
  erratum recorded in the source-bound HLKX evidence; a locally filtered or
  waived cancellation/IRQL failure is not a pass.
- Repeated create/remove, service kill, process crash, sleep/resume, and device
  reconnect leave zero stale children and zero stuck requests.
- The driver retains a bounded, nonpaged lifecycle recorder partitioned by
  processor. Each shard can retain the full public 512-record window, and the
  query path merges only stable published records into the global latest
  suffix. Monotonic per-slot claims prevent a preempted writer from overwriting
  a newer wrap when processors collide on a shard; an active-slot collision is
  dropped rather than waited on and sets a sticky failure flag. Lifecycle
  writers take no locks, allocate no memory, and never wait. Any endpoint,
  completion, controller, or owner rundown watchdog also sets a sticky status
  flag, so rolling its record out of the public window cannot hide it from the
  release gate. Retained records include the active count and queue state needed
  to diagnose the stalled ownership boundary.
- Descriptor and protocol fuzzing rejects malformed inputs without a bugcheck.
- HID report ordering has no duplication or regression across generations.
- DualSense and DualShock 4 media survive concurrent state and feedback traffic.
- Native latency and CPU are measured against the current USB/IP path and a
  comparable virtual-input baseline under the same workload.
- The overlapped owner handle uses Microsoft's
  `FILE_SKIP_COMPLETION_PORT_ON_SUCCESS` contract. A direct input IOCTL which
  the kernel completes inline returns on its publisher goroutine without an
  otherwise redundant IOCP-pump/channel scheduling hop; operations that return
  `ERROR_IO_PENDING` retain the existing cancellation-safe completion path.
- Native completion encoding writes into bounded buffers recycled by the
  client. Continuous control and isochronous traffic no longer allocates a new
  wire buffer for every URB; an allocation gate protects the caller-buffer
  encoder while the existing public marshal API remains available for tooling.
- Allocation microbenchmarks cover both authenticated stream directions and
  input-deadline scheduling. On the development Windows x64 host, replacing a
  timer-backed context per service interval reduced the isolated scheduling
  cost from 4 allocations/272 bytes to zero. Authenticated 512-byte record
  writes moved from 3 allocations/592 bytes to zero, and reads from 3
  allocations/1092 bytes to zero. These are GC/jitter controls, not substitutes
  for the signed HID latency gate.
- Product changes to scheduling, thread priority, DPC behavior, or queue depth
  require a named, bounded-memory WPR capture of the signed live gate. CPU
  sampled/precise, ready-thread, context-switch, WDF DPC, interrupt, and ISR
  evidence must identify the actual critical path; a polling benchmark or Task
  Manager percentage alone is not a valid basis for such a change.
- Signed live input validation discovers the exact newly created HID gamepad,
  continuously reads reports through HIDClass, and correlates 256 unique
  publication markers with cross-process QPC timestamps. DualShock 4,
  DualSense, and DualSense Edge must remain at or below 4 ms p95, 8 ms p99,
  and 20 ms maximum publisher-to-HID latency. These gates include user-mode
  scheduling and prevent a nominal polling-rate claim from hiding tail stalls.
- That same signed live HID gate writes a full output report through the newly
  enumerated HIDClass collection. The exact marker must survive UdeCx and the
  native broker into DualShock 4 rumble/lightbar feedback and DualSense rumble,
  player/lightbar LED, and both adaptive-trigger blocks; kernel operation,
  completion, and host-to-device byte counters must advance. Unit-level
  processor tests alone are not accepted as proof of Windows game-feedback
  delivery.
- Installation is signed, reversible, version-gated, and never replaces a live
  kernel driver across an unsafe reboot boundary.
- The INF's Windows 10 1809 floor and the linked KMDF contract remain aligned:
  the driver targets KMDF 1.27, the framework version Microsoft ships in
  Windows 10 1809. CI rejects a newer KMDF target unless the INF floor is also
  intentionally raised.

The exact attestation/HLK boundary, CAB construction, and Microsoft-signature
validation contract is documented in
[`native-udecx-signing.md`](native-udecx-signing.md).
The protected broker staging, cross-component driver/service rollback, and
authenticated commit order are documented in
[`native-udecx-package-install.md`](native-udecx-package-install.md).

## Primary documentation

- Microsoft, *Write a UDE client driver*
  <https://learn.microsoft.com/windows-hardware/drivers/usbcon/writing-a-ude-client-driver>
- Microsoft, *Handling I/O Requests in a USB Host Controller Driver*
  <https://learn.microsoft.com/windows-hardware/drivers/usbcon/handling-i-o-requests-in-a-host-controller-driver>
- Microsoft, `WdfRequestRetrieveOutputBuffer` (the buffered negotiation
  response remains framework-owned until request completion)
  <https://learn.microsoft.com/windows-hardware/drivers/ddi/wdfrequest/nf-wdfrequest-wdfrequestretrieveoutputbuffer>
- Microsoft, `EVT_UDECX_USB_ENDPOINT_PURGE`
- Microsoft, `EVT_UDECX_USB_ENDPOINT_RESET` (asynchronous reset request)
  <https://learn.microsoft.com/windows-hardware/drivers/ddi/udecxusbendpoint/nc-udecxusbendpoint-evt_udecx_usb_endpoint_reset>
- Microsoft, `WdfIoQueueGetState`, `WDF_IO_QUEUE_STATE`, and
  `WdfIoQueueDriverNoRequests` (no requests are owned by driver callbacks)
  <https://learn.microsoft.com/windows-hardware/drivers/ddi/wdfio/nf-wdfio-wdfioqueuegetstate>
  <https://learn.microsoft.com/windows-hardware/drivers/ddi/wdfio/ne-wdfio-_wdf_io_queue_state>
- Microsoft, `UdecxUrbComplete` and `UdecxUrbCompleteWithNtStatus`
  <https://learn.microsoft.com/windows-hardware/drivers/ddi/udecxurb/nf-udecxurb-udecxurbcomplete>
  <https://learn.microsoft.com/windows-hardware/drivers/ddi/udecxurb/nf-udecxurb-udecxurbcompletewithntstatus>
- Microsoft, `EvtDeviceSelfManagedIoCleanup`
  <https://learn.microsoft.com/windows-hardware/drivers/ddi/wdfdevice/nc-wdfdevice-evt_wdf_device_self_managed_io_cleanup>
- Microsoft, `EVT_WDF_DPC`, `WdfDpcEnqueue`, and `WdfDpcCancel`
  <https://learn.microsoft.com/windows-hardware/drivers/ddi/wdfdpc/nc-wdfdpc-evt_wdf_dpc>
  <https://learn.microsoft.com/windows-hardware/drivers/ddi/wdfdpc/nf-wdfdpc-wdfdpcenqueue>
  <https://learn.microsoft.com/windows-hardware/drivers/ddi/wdfdpc/nf-wdfdpc-wdfdpccancel>
- Microsoft, *KMDF Version History*
- Microsoft, *Install the WDK using NuGet*
- Microsoft Windows Driver Samples CI guidance
- Microsoft, *Finding and Opening a HID Collection*
  <https://learn.microsoft.com/en-us/windows-hardware/drivers/hid/finding-and-opening-a-hid-collection>
- Microsoft, *Obtaining HID Reports*
  <https://learn.microsoft.com/en-us/windows-hardware/drivers/hid/obtaining-hid-reports>
- Microsoft, *Acquiring high-resolution time stamps*
  <https://learn.microsoft.com/en-us/windows/win32/sysinfo/acquiring-high-resolution-time-stamps>
- Microsoft, *WPR Command-Line Options*
  <https://learn.microsoft.com/en-us/windows-hardware/test/wpt/wpr-command-line-options>
- Microsoft, *CPU Analysis*
  <https://learn.microsoft.com/en-us/windows-hardware/test/wpt/cpu-analysis>
- Microsoft, `SetFileCompletionNotificationModes`
  <https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-setfilecompletionnotificationmodes>
- Microsoft, `CreateService`
  <https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nf-winsvc-createservicew>
- Microsoft, `ChangeServiceConfig`
  <https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nf-winsvc-changeserviceconfigw>
- Microsoft, `ChangeServiceConfig2`
  <https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nf-winsvc-changeserviceconfig2w>
- Microsoft, `SERVICE_FAILURE_ACTIONS`
  <https://learn.microsoft.com/en-us/windows/win32/api/winsvc/ns-winsvc-service_failure_actionsw>
- Microsoft, `DeleteService`
  <https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nf-winsvc-deleteservice>
- Microsoft, `SetSecurityInfo`
  <https://learn.microsoft.com/en-us/windows/win32/api/aclapi/nf-aclapi-setsecurityinfo>
- Microsoft, `MoveFileExW`
  <https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-movefileexw>
- Microsoft, `CreateProcessAsUserW`
  <https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-createprocessasuserw>

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
- The local usbip-win2 0.9.7.8 reference proves that UdeCx can expose VIIPER's
  bidirectional isochronous PlayStation audio topology on Windows.
- Its controller contract also reports chained-MDL, high-speed, and SuperSpeed
  compatibility for a root controller with USB 2 and USB 3 ports. VIIPER
  mirrors that capability set and explicitly forwards post-enumeration child
  resets into the generation-owned lifecycle stream.
- ViGEmBus provides the lifecycle north star: explicit protocol negotiation,
  handle-scoped ownership, bounded manual queues, cancel-safe requests,
  generation-aware target teardown, and synchronization per target rather than
  one global lock.

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
3. Every operation token completes exactly once or is cancelled exactly once.
4. A completion from an old generation is rejected without touching a new
   device that reused the numeric identifier.
5. Purge stops admission, cancels queued and in-flight work, waits for ownership
   to settle, then acknowledges UdeCx.
6. Driver unload and file cleanup leave no UDE device, request, or worker alive.
7. Endpoint queues are bounded. The broker enforces both its controller-wide
   ceiling and each child's negotiated pending-operation quota, so one busy
   media device cannot starve another controller. Saturation is observable and
   never overwrites live media or state silently.
8. Shared report state is snapshotted atomically before encoding. Media and
   state never share mutable buffers.
9. No raw user pointer crosses the ABI.
10. The ABI is size- and version-negotiated before any mutating operation.

## Kernel/user transport

The transport is intentionally split by USB semantics:

- interrupt-IN input reports use the ViGEm-style manual-queue fast path. The
  Windows poll stays parked in the endpoint queue; one versioned
  `SUBMIT_INPUT_REPORT` call copies the already encoded report into that URB
  and completes it without an allocation or broker round trip;
- control, interrupt-OUT, isochronous speaker/microphone/haptics, feedback, and
  every lifecycle transition use the cancel-safe ordered inverted-call broker.
  VIIPER posts multiple `DEQUEUE_OPERATION` requests, processes each immutable
  operation through the existing `usb.Device` interface, then submits
  `COMPLETE_OPERATION`.

Input publishers start and stop from UdeCx endpoint lifecycle notifications,
retain their sequence across a purge/start cycle, and are cancelled before
device removal. Removal rejected before UdeCx takes the child restores the
active publishers, so a retry does not strand the current generation. Once
ownership transfers, a terminal UdeCx removal fault restarts the controller
and the removed generation remains closed.

Dynamic endpoint cleanup leaves an address-scoped retirement tombstone for
the current device generation. This lets the kernel acknowledge and discard
the single latest-state input report that can cross asynchronous cleanup before
user mode consumes the ordered purge event, without accepting reports for an
endpoint that was never configured.

The broker owner session is deliberately one-shot. Stopping the user-mode host
cancels endpoint lanes that may already own dequeued kernel requests; those
requests cannot be reconstructed safely in a restarted goroutine. VIIPER must
close that driver handle and negotiate a fresh `Client`/`Host` session, matching
ViGEmBus's file-session ownership model, rather than guessing a new endpoint
sequence baseline and risking an abandoned USB request.

Session shutdown owns a cancellation context before `Serve` is scheduled, so
even an immediate stop cannot miss host cancellation. The client waits for all
dequeue workers, endpoint lanes, input publishers, and their completions to
finish before cancelling overlapped kernel I/O and closing the exclusive broker
handle. The handle is therefore always the last object released.

This deliberately removes TCP, WSK, USB/IP framing, and attach bookkeeping.
The direct input lane removes the highest-frequency HID broker path without
mixing report ownership into the proven PlayStation media/state transport.
Once correctness gates pass, high-rate media payloads may move to a
preallocated ring while keeping the same token/generation lifecycle. Control
and lifecycle operations remain IOCTL based.

### Operation identity

Every operation carries:

- device ID and generation;
- a globally unique token for that generation;
- endpoint address and transfer direction;
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

The usbip-win2 0.9.7.8 UdeCx implementation documents that UdeCx can report
incorrect `InterfaceNumber` and `NewInterfaceSetting` values for composite
device alternate-setting changes. usbip-win2 compensates with an upper filter
on every USB 3 root hub. VIIPER does not install that system-wide filter.

Every endpoint callback already supplies the authoritative endpoint descriptor.
The kernel copies its address, attributes, interval, and maximum packet size
into the versioned broker operation. User mode matches that complete signature
against the immutable controller descriptor and derives the owning interface
and alternate setting. Endpoint start activates it; purge returns it to zero
only after the last endpoint belonging to the active alternate is gone. The
first ISO URB is also authoritative activation, closing the cross-worker race
where media reaches user mode before its start notification. Numeric UdeCx
interface fields are only hints for alternates that contain no endpoints.

## Synchronization model

- A controller-level lock protects the device table and owner registration.
- Removal atomically revokes the UDE handle from the device table before
  `UdecxUsbDevicePlugOutAndDelete`; that slot remains reserved until the
  asynchronous object cleanup runs. Once that API returns, success or failure,
  no path dereferences or restores the invalidated UDE handle and the broker
  owner cannot be released while its reserved removal slot remains.
- A post-transfer UdeCx removal failure is terminal for the controller, not
  retryable for the child. The kernel accepts the broker's removal request,
  requests a PnP controller restart, and keeps owner cleanup closed until
  object teardown completes. User mode can retry only failures returned before
  ownership reached UdeCx.
- Each device has a short-held state lock and independent endpoint queues.
- Each endpoint owns a drain event covering both broker-forwarded URBs and the
  direct interrupt-IN fast path. UdeCx itself owns and purges the framework
  endpoint queue; VIIPER never starts or purges that queue. The purge callback
  closes admission and cancels only the requests already forwarded into
  VIIPER-owned paths; a passive work item calls
  `UdecxUsbEndpointPurgeComplete` only after the last forwarded URB has actually
  completed. A pipe can therefore never restart or disappear across a live
  request.
- Endpoint reset and endpoint-configuration callbacks are asynchronous UdeCx
  management requests, not notifications. ABI 1.7 gives only those lifecycle
  operations a generation-bound management token. Windows receives the request
  completion only after the Go controller engine has applied the reset or
  alternate-setting transition. Start, purge, and power notifications remain
  unacknowledged and cannot add a media round trip.
- Device reset closes direct input admission in the kernel callback and pauses
  every user-mode publisher before controller state is cleared. Admission and
  the active publishers reopen only after the generation-bound reset request
  has been acknowledged, so no HID snapshot can cross the reset boundary.
  Post-enumeration reset, device initialization, and configuration replacement
  share this one-child-at-a-time gate; concurrent reset transactions are
  rejected instead of interleaving two controller resets.
- The controller's default KMDF queue only routes requests: interrupt-IN
  submissions run on an independent parallel queue, while mutation, broker,
  and lifecycle IOCTLs retain their serialized control queue. Large media
  completions therefore cannot head-of-line block fresh controller input.
- Each fast interrupt-IN endpoint has its own passive lock. Different
  controllers publish concurrently, while accidental concurrent submissions
  for one endpoint cannot reorder reports or replay a coalesced sequence.
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
- Interrupt-IN queues are manual and completed from fresh input snapshots;
  output and media endpoints retain independent ordered queues.
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
  callback's DPC ownership is final and cannot be overwritten by admission or
  publication.
- Broker dequeue validation, wait-count admission, and transfer into the
  manual inverted-call queue share the owner lock with file cleanup. No close
  can finish purging that queue and then have an already-validated request
  appear behind the purge boundary.
- The process-death cleanup timer takes its own temporary reference to the
  owner file object before dropping the owner lock. Concurrent cleanup can
  release the controller's long-lived reference without leaving the retry path
  with a stale WDF handle.
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

This follows the useful ViGEmBus pattern of per-target ownership and manual
request queues while accounting for UdeCx's endpoint-specific purge contract.

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
- Repeated create/remove, service kill, process crash, sleep/resume, and device
  reconnect leave zero stale children and zero stuck requests.
- Descriptor and protocol fuzzing rejects malformed inputs without a bugcheck.
- HID report ordering has no duplication or regression across generations.
- DualSense and DualShock 4 media survive concurrent state and feedback traffic.
- Native latency and CPU are measured against the current USB/IP path and
  ViGEmBus-style virtual input under the same workload.
- Installation is signed, reversible, version-gated, and never replaces a live
  kernel driver across an unsafe reboot boundary.

The exact attestation/HLK boundary, CAB construction, and Microsoft-signature
validation contract is documented in
[`native-udecx-signing.md`](native-udecx-signing.md).

## Primary documentation

- Microsoft, *Write a UDE client driver*
- Microsoft, `EVT_UDECX_USB_ENDPOINT_PURGE`
- Microsoft, *Install the WDK using NuGet*
- Microsoft Windows Driver Samples CI guidance

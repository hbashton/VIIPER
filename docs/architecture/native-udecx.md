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
7. Endpoint queues are bounded. Saturation is observable and never overwrites
   live media or state silently.
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

The broker owner session is deliberately one-shot. Stopping the user-mode host
cancels endpoint lanes that may already own dequeued kernel requests; those
requests cannot be reconstructed safely in a restarted goroutine. VIIPER must
close that driver handle and negotiate a fresh `Client`/`Host` session, matching
ViGEmBus's file-session ownership model, rather than guessing a new endpoint
sequence baseline and risking an abandoned USB request.

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
covered by descriptor tests.

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
- The controller's default KMDF queue only routes requests: interrupt-IN
  submissions run on an independent parallel queue, while mutation, broker,
  and lifecycle IOCTLs retain their serialized control queue. Large media
  completions therefore cannot head-of-line block fresh controller input.
- Each fast interrupt-IN endpoint has its own passive lock. Different
  controllers publish concurrently, while accidental concurrent submissions
  for one endpoint cannot reorder reports or replay a coalesced sequence.
- Parallel media callbacks receive a per-endpoint admission sequence under the
  broker lock. An URB cannot publish ahead of an earlier live unpublished
  admission; cancellation retires the admission before dispatch resumes, so
  the public endpoint sequence remains contiguous without limiting media to
  one in-flight URB.
- Media callbacks do not take the controller lock.
- Interrupt-IN queues are manual and completed from fresh input snapshots;
  output and media endpoints retain independent ordered queues.
- A direct input report that was already submitted when D0 exit, unplug, or
  endpoint purge begins is acknowledged and discarded at that exact lifecycle
  boundary. Stale generations and replayed sequences remain hard failures, so
  normal teardown cannot fault the exclusive broker session.
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
- Completion lookup is keyed by `(device ID, generation, token)`.
- Failed transfers do not need to fabricate a successful ISO packet table.
  Successful completions are canonical: OUT replies carry no payload, every
  ISO reserved field is zero, packet extents stay inside the transfer buffer,
  and the sum of actual packet lengths equals the reported completed bytes.

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

## Primary documentation

- Microsoft, *Write a UDE client driver*
- Microsoft, `EVT_UDECX_USB_ENDPOINT_PURGE`
- Microsoft, *Install the WDK using NuGet*
- Microsoft Windows Driver Samples CI guidance

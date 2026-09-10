# DualSense scheduling and ownership

## Input

`DualSense.input` is the only virtual-input state and encoder owner. Its fixed
64-entry ring contains complete transition-bearing `InputState` values. A
single replaceable slot contains continuous motion/state, and a dedicated
front retry slot preserves a failed ordered claim even if the transition ring
refills while a response waits for socket ownership.

The semantic lanes are:

| Work | Semantics | Owner |
| --- | --- | --- |
| Button, D-pad, touch contact/ID and trigger zero-edge state | bounded ordered complete-state ring | input scheduler |
| Stick, motion, contact coordinates, in-epoch trigger movement, and physical raw metadata | one latest snapshot | input scheduler |
| Claimed interrupt report | one immutable report/token | input scheduler until `CompleteInputReport` |
| HID sequence, virtual timestamp, last presented report | commit-on-success state | input scheduler |

The endpoint worker calls `ClaimInputReport` once at an interrupt service
opportunity. Claiming selects and encodes but does not make the state visible to
`GET_REPORT`, advance the committed HID sequence, or mark a trigger peak as
presented. `CompleteInputReport(token, true)` performs those commits only after
the complete USB/IP response is written successfully. Cancellation, reset, or
write failure completes false: ordered work moves to the front retry slot;
continuous work is restored only if no newer or contradictory state exists.

Receive-to-selection latency is recorded once when a received logical state is
first claimed at an endpoint opportunity. Its sampled marker follows the state
through failed-send recovery, so a retry does not double-count or replace the
first-opportunity measurement. Receive-to-presentation is retained separately
and ends only after the complete USB/IP response write succeeds. Both use
fixed buckets and exclude repeated HID idle reports.

A V5 TCP attach advances only the receive-generation token used to reject a
displaced reader. It does not clear accepted transition/latest/retry work, an
in-flight HID claim, the committed encoder sequence, or the previous final
state used for edge classification. Those belong to the virtual USB device
lifecycle, not to a recoverable transport reconnect.

Trigger epochs retain the complete state that actually accompanied each new
analog peak. An unclaimed press may be strengthened only in its own trigger
field. Physical trigger status is coupled at the same boundary: strengthening
L2 may copy only physical byte 43 and the high nibble of byte 48, while R2 may
copy only byte 42 and the low nibble of byte 48. Equal-analog settling such as
L2 `0x28` to `0x29` refreshes that trigger-specific peak status without
changing unrelated state or peak order. A physical base/Edge layout change is
never partially merged; its complete peak remains a separate truthful
snapshot. Once claimed, a state is immutable; an unrepresented later peak is
promoted before release. A failed ordered claim is retried as the same logical
state and is never strengthened in recovery storage; any newer saved peak is
ordered immediately after that retry and before the contradictory release.

Physical metadata does not create ordered work on its own. Mechanical arm
movement, effect/status evolution, host timestamps, battery, and layout-valid
changes replace the one continuous snapshot. They ride any real
button/D-pad/touch/trigger-edge transition as part of that complete state.
This prevents the normal `0x02` -> `0x12` -> ... -> `0x29` trigger-mechanism
sequence from becoming a stale multi-report FIFO.

### V5 raw-input capability

Legacy V5 aliases and the pre-existing `...v5events` aliases require the exact
33-byte input payload. The events suffix opts in only to ordered output
lifecycle frame `0x85`; it is not an input-size negotiation.

The enhanced 53-byte input payload is accepted only by these exact aliases:

- `dualsensecombinedaudioduplexv5rawinputevents`
- `dualsenseaudioonlyduplexv5rawinputevents`
- `dualsensegamepadv5rawinput`
- `dualsenseedgecombinedaudioduplexv5rawinputevents`
- `dualsenseedgegamepadv5rawinput`

Its fixed layout is:

| Payload bytes | Meaning |
| --- | --- |
| `0:33` | legacy mapped `InputState` |
| `33` | flags: bit 0 metadata valid; bit 1 physical source uses Edge layout |
| `34:38` | normalized physical input report bytes `28:32` (sensor timestamp) |
| `38:53` | normalized physical input report bytes `41:56` |

Unknown flag bits are invalid, and Edge-layout bit 1 is invalid without the
metadata-valid bit. Decode of a legacy state clears all physical validity,
layout, timestamp, and status fields so a reused object cannot retain a newer
alias's metadata.

When metadata is valid, the input encoder owns its selection under `input.mu`:

- physical sensor timestamp and report bytes 41 through 48 are copied;
- physical battery byte `53` is copied;
- physical common headset/filter status byte `55` is copied;
- report bytes 49 through 52 are copied only when physical and virtual base/Edge
  layouts match, otherwise the virtual target layout is synthesized;
- virtual USB connection byte `54` is always forced to `0x08`;
- physical bytes 56 through 63 are never transported or copied into the virtual
  report.

Physical bytes 56 through 63 are an eight-byte AES-CMAC over the physical
report.
VIIPER changes counters, presentation state, and connection state and does not
possess the controller key, so reusing that tag would falsely present an
unauthenticated report. The virtual tail remains zero. Without valid physical
metadata, the encoder emits confirmed neutral trigger status `0x09/0x09`, no
effect (`0x00`), and its generated base/Edge clock/status layout.

## Output and media

The V5 framed writer is the sole socket and frame-sequence owner. Its lanes are:

| Work | Semantics |
| --- | --- |
| Adaptive trigger/rumble/light/LED output state | one replaceable latest validity-aware state |
| Microphone-interface lifecycle event | bounded ordered control queue |
| Realtime rear haptics and atomic speaker/haptics | generation-tagged time-indexed queues |

The writer round-robins ready lanes, so sustained realtime haptics cannot
starve lifecycle, latest output, or speaker work. Every payload is marshaled
into a writer-owned fixed slot before enqueue. The completed frame is built in
one reusable buffer and written under the writer's sole socket ownership. The
latest-state latch has storage independent of the ordered-control pool. When a
realtime lane is saturated, its oldest unstarted generation is replaced so a
stale media backlog is never replayed after transport backpressure.

Speaker assembly uses fixed per-device accumulators, a bounded rear-generation
ring, and four fixed callback slots that each own their 1,920-byte speaker
payload. No callback aliases the mutable assembly buffer. Endpoint reset and
alternate-setting changes advance `speakerMediaGeneration` while holding
`mediaMu`; admitted ISO work carries that token. The device rejects stale work
under the same lock, and the writer invalidates/drains both realtime and atomic
media lanes before its lifecycle barrier returns.

Active speaker gain uses an explicit pooled scratch-buffer lease. The media
consumer releases that lease after synchronous assembly; no per-block closure
or heap object is created, and no media/generation lock remains held while the
consumer callback executes.

V5 microphone-interface events are opt-in. Only these device aliases emit
frame `0x85`:

- `dualsensecombinedaudioduplexv5events`
- `dualsenseaudioonlyduplexv5events`
- `dualsenseedgecombinedaudioduplexv5events`
- `dualsensecombinedaudioduplexv5rawinputevents`
- `dualsenseaudioonlyduplexv5rawinputevents`
- `dualsenseedgecombinedaudioduplexv5rawinputevents`

The payload is an active byte followed by a little-endian 64-bit stream
generation. Legacy aliases never receive this frame. A patched client that
cannot create an events alias can query the narrow
`bus/{busId}/{devId}/microphone-interface` endpoint.

## Lock partition and ordering

Subsystem locks are acquired one at a time except for the bounded native HID
command-admission boundary described below:

| Lock | State |
| --- | --- |
| `input.mu` | input queues, epochs, claim/retry, encoder and input telemetry |
| `metaMu` | immutable-style metadata pointer |
| `outputMu` | output/effect merge state and feature subcommand |
| `mediaMu` | speaker feature state, media accumulators/ring and media generation |
| `microphoneMu` | microphone feature state and bounded jitter buffer |
| `callbackMu` | callback registrations and stream-generation ownership |

Native HID admission holds `outputMu`, then `callbackMu` for reading, while its
internal V5 sink performs fixed-cost queue admission. After admission succeeds,
it briefly acquires `mediaMu` to publish the same cumulative snapshot. No other
path acquires these locks in reverse order. A rejected command changes neither
the persistent output state nor the media snapshot. This internal sink must
never do I/O, wait for capacity, log, or invoke an external callback.

All arbitrary/external callbacks, including legacy `SetOutputCallback`, are
copied under `callbackMu`, then invoked after it and every subsystem lock have
been released. Diagnostics snapshot each subsystem under its own short lock
and only then construct maps or JSON. No device lock is held during USB/IP
response I/O, V5 socket I/O, waits, logging, or arbitrary/external callbacks.

Exact native HID commands use the fixed 32-buffer ordered control lane; plain
cumulative snapshots retain latest-state behavior, and media lanes retain their
independent scheduling. Short writes authorize only complete validity groups
present in their source bytes; padding must not synthesize absent fields. The
shared USB/IP reader never waits for output capacity. Native interrupt OUT and
exact HID SET_REPORT admission failures complete with ENOSPC and zero actual
length, without disconnecting the stream or committing the rejected command.
Host retry is not guaranteed; overload is an explicit failed submission, not
silent success or a promise of eventual physical delivery.

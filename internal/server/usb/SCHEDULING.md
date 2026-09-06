# USB/IP endpoint scheduling

## Retained Xbox persona (2026-09-02)

The retained import has its own three-lane arbiter rather than the legacy
endpoint workers described below. It now snapshots validated IN/OUT descriptor
intervals before activation and applies independent absolute service cursors.
EP0 is immediate; readiness cannot bypass a consumed interrupt opportunity.
Pending does not consume a service slot, and the arbiter's existing reusable
timer wakes the next due opportunity. Full-interval lateness re-anchors instead
of replaying expired slots. Small lateness preserves phase, so this is not a
strict minimum inter-completion-gap guarantee. Final preparation samples time
after acquiring response ownership, not before serializer contention.

This corrects unbounded same-state URB response traffic. The retained Xbox
semantic ingress now separately uses the shared fixed-report journal for
ordinary buttons/Share/trigger transitions, paired with the engine's exact
USB retry. Guide retains its separate queue. This is software-test evidence,
not an end-to-end latency result. See the [dated validation ledger](../../../docs/architecture/controller-platform-validation-2026-09-02.md)
for terminal-overflow teardown evidence and Windows/hardware validation gates.

An optional `ImportRetirementOwner` now requests irreversible, exact-lease
retirement for a terminal input-history overflow. The worker checks it before
service and after consuming readiness, including with no queued URB. It records
the retirement reason independently of `failure`, revokes admission, and retires
all exact unadmitted tickets. A successful scheduler close then permits the
existing executor drain and acknowledged disconnect-neutral transaction. A
forged request, query panic/error, failed ticket retirement, or uncertain drain/
neutral remains fatal; a retirement request never itself proves Safe. This is
not a reset/reconnect or silent history-resynchronization mechanism.

Descriptor-derived cadence is the present default policy, not a generic USB
minimum spacing requirement. USB 2.0 section 5.7.4 permits shorter host service
(1 ms minimum at full speed). The experimental
`--usb.retained-input-service-ms` / `VIIPER_USB_RETAINED_INPUT_SERVICE_MS`
accepts only `0`, `1`, `2`, or `4`; default `0` preserves descriptor timing.
The import validates and snapshots this policy before owner activation. It
overrides retained IN service only, without changing speed, descriptor bytes,
OUT, or EP0. Scripted and real authorized-Xbox virtual-time tests pass, including
decoded distinct states through the shared journal. Windows-consumer rate,
idle/load CPU, and end-to-end latency remain unmeasured for this change.

Unchanged ordinary Xbox input now parks without selecting the journal's idle
image or polling every millisecond. New semantic work wakes it by readiness;
the pending token captures the epoch before selection so publication racing
with that decision cannot be swallowed. Explicit KeepAlive publications remain
report requests. The owner retains the next periodic Status Device deadline
while parked, so change-only input does not suppress required status traffic.
This removes duplicate USB reports, not all ingress wake-ups or physical stick
noise; no new deadzone is imposed by this transport policy.

Ordinary motor/LED executor acceptance now has one separate engine-owned claim
slot, so it does not retain the primary IN claim lane. Input/Guide/status and
their retries may proceed while feedback acceptance waits; EP0/OUT/lifecycle
ordering and mandatory output clears stay fenced. Delivered ordinary OUT wakes
the selected local action immediately, detachment signals IN readiness, and
failed feedback has its own exact local retry wake. No extra input poller or
unbounded queue is introduced. This is deterministic/race-test evidence, not a
measured explanation or elimination of historical Windows latency tails.

## Ordinary endpoint workers

Each attached USB/IP connection has three independent scheduling planes:

- interrupt IN: one persistent worker per endpoint, one ordered bounded URB
  ring, and one absolute service cursor derived from the endpoint descriptor;
- isochronous IN: one persistent worker per endpoint, with microphone packets
  sampled at individual service slots and one contiguous completion per URB;
- isochronous OUT: one persistent worker per endpoint, with payload and packet
  descriptors copied into owned bounded slots before the command reader
  resumes. The worker publishes the time-indexed media block at the reserved
  window start and emits RET_SUBMIT only at the absolute window end; haptics do
  not acquire an extra URB-duration delay.

The command reader only parses, validates, copies, enqueues, and signals these
workers. It never waits for an endpoint deadline and never processes ISO media.
Queue capacity is fixed. A full queue returns `-ENOSPC` rather than growing or
blocking command ingestion.

When a device exposes the DualSense/Edge nonblocking input, microphone, and
generation-tokenized ISO-OUT APIs, every matching descriptor-known endpoint
worker is constructed during connection scheduler initialization. Duplicate
endpoint declarations across alternate settings are folded into one
endpoint/direction/kind owner. Thus the first real URB cannot create a worker,
goroutine, timer, channel, or buffer on the command-reader path. Generic legacy
device workers remain lazy. The precreated workers are also published through
immutable endpoint-indexed arrays, so interrupt/ISO admission does not contend
on the connection-wide worker map lock used by diagnostics and lifecycle work.

## Clock and generation ownership

An endpoint worker reserves every job against its absolute cursor when the job
is admitted. Ordinary sub-interval lateness preserves that phase. Lateness of
at least one complete interval re-anchors the current service opportunity to
the current monotonic time and moves later reservations with it; expired slots
are never replayed as a burst. Socket completion time never updates an endpoint
cursor.

The reusable timer's actual wake time is checked again before service. If the
timer itself is delivered one full interval late, the worker re-anchors before
selecting state and shifts later reservations. This prevents expired URBs from
being replayed back-to-back after scheduler jitter.

Unlink removes a queued job, compacts later reservations, and reports
`-ECONNRESET`. Once a response has claimed serialized send ownership, unlink
reports success and follows that response on the wire. ISO OUT additionally
transfers ownership immediately before its irreversible device callback; an
unlink arriving after that boundary reports already-completed success and
cannot suppress the still-due RET_SUBMIT. Endpoint reset,
alternate-setting zero, configuration reset, and disconnect increment or end
the worker generation and invalidate old queued work.

## Buffer ownership

The command reader owns its header, OUT-payload, and ISO-descriptor scratch.
ISO worker slots own deep copies. A slot is immutable after a worker claims it,
apart from its cancellation/generation flags and completion descriptors. It is
returned to the worker-local free list only after completion or cancellation.
For devices exposing tokenized ISO OUT, the command reader also captures the
device media generation at admission. The device validates that token under
its media lock at service, so a reset cannot relabel or publish old PCM.

Interrupt report, microphone compact-payload, ISO descriptor, and complete
USB/IP response buffers are worker-owned and reused. A response buffer remains
owned by that worker through `writeFull`. Each ISO worker preallocates 64 job
slots with descriptor capacity for 32 packets; ISO OUT also preallocates each
slot's common DualSense payload capacity. Larger valid jobs may grow a slot
once and retain that storage, while queue depth remains bounded.

DualSense interrupt input uses a tokenized claim. Selection and encoding claim
one immutable device state before response construction. The response writer's
send-lock callback only marks endpoint response ownership, making unlink order
exact. After `writeFull` and required flush finish, the response writer commits
or rejects the device claim before releasing send ownership. Reset, unlink, or
any socket error completes it as unpresented so ordered work returns to the
device's recovery front; partial-write ambiguity prefers a possible duplicate
after reconnect over a missed contradictory transition. Exact input
`GET_REPORT` requests use a versioned nonallocating snapshot: the complete EP0
response is built outside send ownership, then its presentation version is
validated under send ownership and rebuilt if an interrupt completed first.

## Lock order

There is no nested queue/send lock order:

1. endpoint queue locks protect only slot, cursor, generation, and telemetry
   state;
2. endpoint queue locks are released before device callbacks, waits, response
   construction, logging, and socket I/O;
3. the response writer takes exclusive send ownership only after a complete
   response exists;
4. its optional claim callback briefly checks endpoint generation while send
   ownership is held, releases the endpoint lock, and only then starts I/O;
5. after I/O, its completion callback briefly advances the device input
   presentation version before response ownership is released.

No code acquires response ownership while holding an endpoint lock. The short
claim callback is the only send-to-endpoint direction; the after-write callback
takes only the device input lock. This makes unlink vs. completion and
interrupt vs. GET_REPORT ordering atomic without carrying either device or
endpoint locks across I/O.

## Legacy fallback boundary

DualSense and DualSense Edge use the nonblocking claim/build-into and
microphone read-into interfaces, so their interrupt and ISO-IN workers create
no per-URB context, goroutine, channel, or timer. Devices that have not yet
adopted those interfaces retain a compatibility-only bounded timeout around
their historical blocking `HandleTransfer` callbacks. That fallback preserves
their existing event-driven/idle behavior and is not used by either target
DualSense path; migrating those unrelated devices is intentionally separate.

## Diagnostics

Hot paths update fixed atomic histogram buckets only.
`Server.EndpointDiagnosticsSnapshot` exposes active connection snapshots with
queue depth/high-water, overflow, unlink and re-anchor counters, endpoint queue
age/lateness, and response queue wait/send-lock wait/socket-write
distributions. Each duration reports sample count, median, p95, p99, p99.9,
and exact maximum. Setting `VIIPER_USB_ENDPOINT_DIAGNOSTICS=true` emits that
aggregate no more often than once every five seconds; no report-level logging
is performed.

# Xbox One dormant interrupt-OUT packet adapter

Audit and implementation date: 2026-08-31.

## Outcome

VIIPER now has the smallest dormant packet-boundary composition between
`usb.TransactionalInterruptOutDevice` and
`ControllerPersonaDownstreamPacketBatchOwner`. It owns only a complete USB
packet delivered on endpoint number `1`, reuses
`DecodeControllerDownstreamPacket`, and never creates another GIP parser,
mapping engine, feedback owner, or transport owner.

The adapter is not a `usb.Device`. No registry, descriptor, CLI, public-library,
retained-import, or production-device path constructs it. This is an offline
ownership prerequisite, not evidence of a lawful Xbox identity, Windows
binding, enumeration, XInput/GameInput visibility, hardware feedback, or
latency.

## Exact USB/IP boundary

The server contract is preserved without executing controller work under the
response writer:

1. `ClaimInterruptOutTransaction` returns the all-zero Unhandled claim for
   every endpoint except `1`.
2. Endpoint `1` is decoded as one exact 1-through-64-byte USB packet. The
   decoder already walks every declared message boundary. Empty, oversized,
   truncated, fragmented, or unsupported input becomes a private no-effect
   Stall claim. The adapter does not infer fragments or replay.
3. An Accepted numeric USB claim is backed by one private exact
   `ControllerPersonaDownstreamPacketBatchClaim`. Its concrete owner, current
   private claim, packet epoch, attempt token, and message count must all match
   the decoded packet. Claim performs only decode, canonical selection, and
   whole-vector preflight.
4. Admit authenticates the numeric claim again and adopts the exact private
   `ControllerPersonaDownstreamPacketBatchLease`. Owner, claim token, admitted
   owner state, private execution lease, epoch, count, and execution value must
   all match. It repeats preflight but does not call participant Execute.
5. Complete is mutex-free, fixed-slot, allocation-free, and I/O-free. It CASes
   the one eligible source state to Completing, authenticates the immutable
   private slot, stores the terminal USB outcome, publishes the preallocated
   work ticket only by winning `Completing -> Pending`, and performs one
   nonblocking latched wake. No diagnostic lock or competing terminal writer
   can overwrite Quarantined with Pending.
6. `PendingWork` returns the same opaque ticket until `RunPending` acquires it.
   The ticket contains the adapter identity, never-reused USB token, generation,
   and canonical packet epoch. A stale, forged, copied-after-use, or concurrently
   acquired ticket cannot authorize a second Execute.

Snapshot and PendingWork may take the diagnostic mutex for a consistent copy,
but Complete does not. A focused test holds that mutex across Complete and
proves publication still returns immediately. A saturating CAS allocator latches
at `math.MaxUint64`; it cannot wrap and reuse generation one.

## Worker terminal policy

`RunPending(ticket, executeDeadline, drainDeadline)` is the only execution
surface. Both deadlines must be future exact bounds, and the drain bound must be
later than the execution bound.

| USB terminal fact | Worker action | Canonical result |
|---|---|---|
| Delivered Accepted | Execute exact lease, then Resolve | Delivered only after timely nil Execute |
| Delivered Accepted, timely proven-no-effect error | Resolve | DeliveryFailed, retained canonical retry, adapter fenced |
| Delivered Accepted, late/panic/ambiguous result | exact CancelAndDrain, then Resolve only if the owner proves a valid terminal | otherwise quarantine |
| DeliveryFailed or Cancelled Accepted | never Execute; Resolve | Deferred, retained canonical retry, adapter fenced |
| any Stall terminal | never touch batch owner | retire the no-effect fixed slot |

Every non-delivered canonical result intentionally leaves the batch owner in
RetryPending. This adapter has no evidence-backed USB receive replay authority,
so it does not compare a later byte string, call `ClaimRetry`, or guess that a
host retransmission represents the same transfer. It permanently fences at that
point and retains the owner for explicit terminal attention.

Both owner Claim and Admit classify all four `(credential.Valid(), error)`
shapes. Nil plus a valid exact credential is the only accepted shape. Error plus
invalid is a proven rejection. Nil plus invalid and error plus valid are
dependency contradictions: any supplied capability is retained and the adapter
quarantines. Claim can also fail after canonical selection when participant
preflight rejects; that owner retains a mandatory retry while the adapter
returns a zero USB claim plus an error. No contradictory shape can fabricate a
numeric claim, lose an admitted lease, or be called a clean rejection.

After a nil owner Resolve, the adapter reads the exact owner terminal shape
before it may return to Idle. Delivered must produce only Idle. Deferred,
DeliveryFailed, and ExecutionCancelled must produce only RetryPending. Any
active, draining, awaiting-resolution, quarantined, or contradictory terminal
shape quarantines the adapter.

## Concurrency and failure closure

- A completion caller must win the eligible-state CAS before reading the exact
  immutable slot. A forged winner terminally fences without publishing work.
- Every competing completion permanently latches the terminal fence, including
  while work is Pending or Running. PendingWork and RunPending then reject and
  retain the canonical owner; they do not guess that global terminal state
  belongs to a safely replayable generation. A monotonic last-completion token
  is retained across Idle, so a delayed copied Complete is fenced before it can
  inspect or interfere with a successor slot.
- Worker resolution has one atomic authority chain. Running can become either
  Resolving or Quarantined. A worker which wins Resolving may perform only its
  exact Resolve; a later violation changes it to ResolvingTerminal, never
  revokes it mid-call, and forces quarantine afterward. After terminal-shape
  validation the worker must win Resolving-to-Finishing, and Finishing-to-Idle
  competes directly with terminal containment. No blind state store can reopen
  Idle or Pending.
- Claim final publication, Admit final admission, and RunPending acquisition
  recheck the terminal fence under the slot mutex. If a concurrent fatal
  completion won, any acquired batch claim/lease remains retained and no new
  authority escapes.
- Worker ambiguity, duplicate completion, contradictory Claim/Admit result,
  foreign or stale same-shape lease, stale generation/token/ticket,
  deadline rejection, batch-owner divergence, and owner quarantine all fail
  closed. The fixed slot is not silently cleared on an uncertain result.
- Work readiness is lifetime-stable and latched. A wake is only a hint; the
  exact ticket remains the authority, so neither a consumed nor duplicate wake
  can lose or duplicate work.

## Validation scope

Focused unit and server-integration tests cover endpoint gating, complete packet
decode, malformed Stall, exact private claim/lease adoption, ACK delivery before
effect, all four Claim and Admit result shapes, foreign/stale lease rejection,
post-Resolve terminal-shape validation, delivery failure/cancellation,
claim-time retained failure, timely participant failure, late-result
drain/quarantine, delayed and duplicate claims, copied worker tickets during
Resolve, deadline rejection, atomic generation exhaustion, diagnostic-lock
independence, zero allocations, and Completing/Resolving/Finishing fatal-fence
races. Repeated and race-enabled runs are offline evidence only.

## Remaining production blockers

- no production device constructs or exposes this adapter;
- no proven receive duplicate/replay authority or fragment reassembler exists;
- no production whole-vector participant exists;
- no device identity, descriptor registration, authentication, metadata
  conformance, or supported Windows Xbox binding has been established;
- no physical feedback transport or four-actuator conformance has been tested;
  and
- no enumeration, consumer-API, polling-rate, or end-to-end latency claim
  follows from this dormant seam.

# Xbox One canonical downstream packet batch (dormant)

Audit and implementation date: 2026-08-31.

## Outcome

VIIPER now has a dormant, backend-independent composition which can carry one
complete decoded Xbox controller data packet through the canonical persona and
one whole-vector atomic participant. It supports an ordered Direct Motor /
Guide LED vector and at most one lifecycle or Protocol Control ACK member in
that vector. The implementation is replay-tested only. It is not registered by
the device registry, USB/IP server, retained adapter, CLI, or public library.

This tranche did not add a VID/PID, descriptors, Windows binding, authentication,
metadata generation, hardware I/O, native driver, or receive replay policy.

## Source boundary

The wire facts remain pinned to Microsoft MS-GIPUSB revision 1.0 and the
official downloadable Original GIP Specification already recorded in
`device/xboxone/PROVENANCE.md`:

- MS-GIPUSB section 3.1.5.3 permits several small messages in one controller
  data packet;
- sections 3.1.5.5.3 through 3.1.5.5.5 define the modeled lifecycle commands;
- Original GIP Specification table 4-14 defines the Protocol Control ACK body;
- MS-GIPUSB tables 56 and 42 define Direct Motor and Guide LED respectively.

The official local MS-GIPUSB document pin remains SHA-256
`810F2841AF3FD832E5EBBDFB581D67413DDA5914364E713AEB35D63483D6D9B0`.
No PadForge, HIDMaestro, Switch2Connect, xone, xpad, or SDL expression was used
to implement this batch authority. Those repositories remain audit or
corroboration inputs under the repository-wide provenance ledger.

## Ownership composition

The package-private canonical persona packet lane validates the complete
fixed-capacity packet before selecting any context-bearing member. It then:

1. reserves one never-reused persona packet epoch;
2. selects zero or one lifecycle/ACK member through the existing canonical
   decoded-host selector;
3. reserves the ordinary persona claim lane even for an all-feedback packet;
4. builds one immutable action vector in original wire order; and
5. retains that exact vector for retry.

The context action carries the exact canonical obligation: private egress bytes
and output sequence for a wire-producing lifecycle action, clear epoch for an
output release, or reliable-transfer disposition for an ACK. Direct Motor and
Guide LED retain their typed source values. A source-defined FULL POWER no-op
gets an explicit order-preserving no-effect action.

`ControllerPersonaDownstreamPacketBatchOwner` composes that persona reservation
with `ControllerDownstreamPacketExecutionOwner`. Claim-time participant
preflight sees the complete exact vector. Final admission repeats participant
preflight before the persona crosses its final lifecycle/ACK fences. Execute is
one participant call; it is never a callback loop over messages.

Both owners receive the same terminal fact. Before persona resolution, the
executor issues one unexported terminal credential and enters a prepared state
which rejects Execute, CancelAndDrain, duplicate resolution, and every stale
lease. The credential authenticates the exact packet token, epoch, count,
terminal outcome, and executor result. The persona can then commit its already
admitted state without a snapshot race; consuming the credential performs only
the executor's prevalidated terminal state update. A concurrent Execute either
wins before preparation (so Deferred preparation fails without persona
mutation) or loses after preparation (so it cannot reach the participant).

Delivered resolution commits the canonical context and derives the final
feedback snapshot by walking the whole vector in wire order. Deferred, timely
proven-no-effect failure, and exact
cancel-and-drain retain the same packet epoch and vector with fresh attempt
tokens. A panic, late return, uncertain prior effect, cancellation/drain
failure, owner divergence, or expired ACK context quarantines instead of
making the vector replayable.

Canonical vector construction after lifecycle/ACK selection contains only
fixed-array copies and closed action-domain checks. Those checks are retained
as executable invariants. If any ever fails after the inner claim exists, the
persona records a permanent packet quarantine and deliberately retains that
exact claim; it does not pretend to roll back or make a successor packet
eligible. Combined-owner construction/claim observes and propagates that
quarantine. Tests cover the containment path alongside lifecycle, ACK,
feedback, clear, and no-op packet paths.

## Deliberate fail-closed limit

The lifecycle and reliable-transfer implementations each expose one canonical
outstanding claim. Consequently, more than one lifecycle/ACK-governed message
in a packet fails before persona or participant mutation. STOP, OFF, and RESET
are also accepted only as single-member packets. Their first selected cursor
gates upstream while the mandatory output clear is a successor cursor; a later
feedback action in the same vector could otherwise become visible in Idle or a
terminating state, and terminal completion can additionally change transport
generation. QUIESCE is different and remains eligible for a mixed packet
because its selected action is the clear itself.

This is the smallest truthful batch seam. Supporting multiple context-bearing
members requires canonical multi-claim state and ordered preview/commit within
the engine. Copying `ControllerPersonaEngine`, applying a prefix, or fabricating
ACK generation/epoch facts remains forbidden.

## Validation scope

Deterministic tests cover:

- lifecycle plus feedback whole-vector ordering and private response bytes;
- ACK plus feedback using the active metadata generation/transfer identity;
- `ClearOutputs` ordering between two motor values;
- preflight-before-effect, two-context failure atomicity, and both wire orders
  of STOP/OFF/RESET plus feedback rejecting before clock/owner/participant
  mutation;
- exact packet epoch and action-vector retry with fresh ABA-safe tokens;
- concurrent claim serialization;
- the terminal credential's Execute/Cancel fencing and both sides of the
  admitted-Deferred versus Execute race;
- deadline-bounded cancellation and drain;
- commit-then-panic ambiguity quarantine;
- ACK retry expiry quarantine;
- post-selection invariant quarantine retaining the acquired persona claim;
- zero and copied owner rejection; and
- zero allocations on the ordinary successful batch path after warm-up.

The fuzz target crosses decode, canonical selection, admission, whole-vector
execution, and resolution for supported seeds and arbitrary one-through-64-byte
inputs. The package, repository, race, vet, and fuzz command results are
recorded in the dated controller-platform validation report; they are offline
evidence, not Windows or hardware conformance.

## Remaining production blockers

- a dormant USB/IP interrupt-OUT packet-boundary adapter now calls this owner,
  but no registry, descriptor, device, retained-import, CLI, or public-library
  production path constructs that adapter;
- no receive duplicate/replay window or fragment reassembler exists;
- more than one context-bearing member is deliberately unsupported;
- the participant contract requires a real all-or-none heterogeneous effect
  implementation; no production participant exists;
- the persona capability blockers remain non-zero, including identity,
  metadata semantics, authentication, Windows binding, and hardware
  conformance; and
- no latency, polling-rate, Windows enumeration, XInput visibility, impulse
  trigger, or physical feedback claim follows from this dormant seam.

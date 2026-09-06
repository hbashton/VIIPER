# Dormant authorized Xbox retained-USB composition (2026-08-31)

## Scope and status

`NewAuthorizedDormantRetainedUSBAdapter` remains the private-engine
construction boundary. The production USB/IP reader can route its retained
device wrapper only through `RegisterAuthorizedXboxOneRetainedUSB`, an explicit
internal factory which requires the server's exact nonzero retained authority
and a representable existing bus with an available representable device address
and registration token. No `xboxone`/`xboxseries` generic API device type is
registered. Device construction starts no device worker and performs no
controller I/O. Successful server registration installs one cold lifecycle
watcher which exits on exact registration cancellation or server shutdown; it
does not process transfers. This does not establish Windows binding, protocol
conformance, latency, or hardware support.

The boundary closes a narrow ownership gap between two existing constructors:

1. `NewAuthorizedControllerPersonaEngine` consumes the exact caller-authorized
   external identity and returns the bound engine.
2. `NewDormantRetainedUSBAdapter` accepts an engine under a documented exclusive
   ownership transfer, but a two-step caller can retain the returned engine
   pointer.

The composition factory invokes both constructors internally and returns only
the dormant retained adapter. No second persona, mapping, feedback, control, or
USB/IP stack is introduced.

## Authority and failure contract

- Adapter identifiers, the executor, and timeout are preflighted before the
  one-shot authorization is consumed. The stateful executor must be a non-nil
  pointer to a nonzero-sized pointee. Typed-nil, zero-size-pointer, and value
  implementations are rejected: static type comparability neither proves exact
  object ownership nor prevents an interface equality panic when a nested
  interface contains an uncomparable value. Go may assign distinct zero-sized
  objects the same address, so Type+Pointer cannot authenticate them. Nonzero
  struct and array pointees remain admissible exact objects.
- The existing authorization owner remains the sole copy/ABA/concurrency
  authority. Copied capabilities race through the same owner mutex, and exactly
  one can consume an issued authorization.
- After engine construction, an engine-owned cold-state seal covers the whole
  canonical engine value. It detects clock, transport generation, USB, GIP
  lifecycle, claim/retry, sequence, feedback, and other engine-state mutation
  before publication. The seal independently snapshots the complete external
  descriptor value, binding value, original profile/metadata issuance pointer
  identities, and deep metadata bytes. It also authenticates the exact private
  metadata backing store, so neither in-place mutation plus digest rewriting
  nor equal-byte slice substitution can self-validate through shared aliases.
- The factory invokes the same private implementation used by
  `NewDormantRetainedUSBAdapter`; its package-test hook runs only after canonical
  construction and receives no adapter pointer. The exported direct constructor
  supplies no construction authority and neither receives nor retains a proof.
  Only the exact private composition token makes the canonical implementation
  separately return a one-shot proof authenticating the exact adapter,
  engine/coordinator, executor pointer, authority/device identifiers, timeout,
  protocol time, generation, and dormant state. A hand-built field lookalike or
  foreign token has no proof. Successful adoption consumes the local proof; it
  is not reachable from the returned adapter.
- Any adapter-construction error, post-construction hook error, contradiction,
  or panic after engine construction terminally quarantines that exact
  authorization. Quarantine retains the historical guard issued immediately
  after authorized construction: it invalidates that known local engine, EP0
  plane, and exact binding without following a mutable `binding.engine` or
  substituted owner binding. Copied capabilities cannot retry it.
- A stale or forged capability cannot quarantine a different exact owner. A
  preflight rejection consumes no authority and can be corrected by the caller.

The package-local post-construction hook exists only to make partial-failure and
engine-corruption tests deterministic. It cannot substitute or mutate the
adapter. Repository-wide AST identifier and call-site tests prove that the
adapter constructor remains reachable only through its exact retained-device
wrapper and that the device constructor has exactly one production call site:
the explicit internal factory. Function aliases or a second construction
surface fail that test.

The internal factory performs a non-consuming server/topology preflight before
construction. That preflight covers exact authority, shutdown, a representable
live bus, an available nonzero 16-bit device address, and non-exhausted
registration-token capacity. The bounded provisional Add repeats allocation
and token checks under the bus lock. It is an exact lifecycle owner but remains
absent from DEVLIST, address lookup, import selection, device enumeration, and
context lookup while the retained callback seal admits the descriptor. Only an
exact server/bus/authority revalidation publishes that same incarnation;
failure joins exact rollback without making it discoverable.

Every exact registration carries a private bus-bound lifecycle capability.
Warm semantic publication authenticates that capability in O(1), without a
device-list scan or allocation, and holds a single-owner operation lease across
the canonical adapter call. Removal first marks the lifecycle undiscoverable,
then drains the lease without holding virtual-bus or server bus-map locks, and
finally cancels/removes it. Exact duplicate removal, broker Close, whole-bus
Close, and duplicate server RemoveBus join the already-marked operation drain
through context cancellation. Server RemoveBus additionally scans and marks
the bus's retained admissions before releasing duplicate server removers;
direct lower VirtualBus Close relies on the cold watcher or synchronous lazy
reconciliation described below for admission cleanup. A cross-bus
`DeviceMeta.Bus` substitution cannot authenticate the private capability.

The returned broker handle exposes no raw USB device or retained owner. It can
only publish through `PublishSemanticInputWire` while the exact registration is
active, or close by exact registration identity. Same-pointer re-registration
is therefore not removable or publishable by a stale handle. Generic
VirtualBus Add also rejects typed-nil and dynamically non-comparable device
identities before mutation so later exact comparisons cannot panic.
Ordinary value copies of the exported broker handle share one private
publication/close state, so they cannot create a second close owner; concurrent
copied-handle Close calls join and replay the first exact result.

Because the lower VirtualBus teardown API remains exported, the server links
each retained registration context back to its exact admission record. A cold
lifecycle watcher retires it after direct bus removal/close, immediate
same-pointer admission also reconciles a provably inactive predecessor before
reporting Busy, and rollback retires the recognized exact key even if a
concurrent server bus removal already removed the topology mapping.

## Identity and allocation boundary

The factory accepts only an existing `AuthorizedControllerPersonaConfig`; it
does not invent, default, normalize, or substitute identity strings or USB
identity data. Descriptor capacity and response framing remain properties of
the canonical persona/control-plane and retained-USB implementations.

Composition is a cold path and allocates the canonical engine and adapter.
Tests establish zero allocations only for repeated rejection of an already
consumed capability and for the returned adapter's warm immutable diagnostic
accessors. No allocation or latency claim is made for construction, binding,
transfer execution, or production routing.

## Deterministic evidence

`authorized_retained_usb_composition_test.go` covers:

- exact engine/control-plane ownership without a public engine return;
- one winner across copied concurrent authorization values;
- stale, forged, old-constructor, and delayed-use rejection;
- non-consuming argument preflight, including typed-nil, value, statically
  non-comparable, statically-comparable/runtime-uncomparable, zero-size-pointer,
  and nonzero-array-pointer executors;
- exact pointer ownership for equal-but-distinct executors;
- proof-on-request consumption/replay rejection, foreign-token rejection,
  no ambient proof on direct adapters, and hand-built fake rejection;
- valid descriptor substitution, metadata mutation plus digest substitution,
  and equal-byte foreign metadata-storage substitution;
- post-construction error, engine clock/lifecycle-generation split, mutable
  binding corruption, owner-binding corruption, and panic quarantine;
- partial-engine and EP0 revocation; and
- warm rejection/diagnostic allocation counts plus exact call-site AST checks.

`internal/registry/authorized_xboxone_test.go` additionally covers default-off
and mismatched-authority preflight without consuming authorization,
unrepresentable bus and saturated bounded-address rejection without consuming
authorization, generic API dormancy, hidden provisional descriptor admission,
descriptor-sealed exact publication, input rejection before retained binding,
idempotent close, marked-drain joining, and same-pointer successor preservation.
VirtualBus/server tests additionally cover token exhaustion, malformed dynamic
device identity, forged cross-bus capability rejection, zero-allocation O(1)
operation acquisition, operation-time bus-status reentry, exact/concurrent
close joining, copied-handle close joining, direct lower-bus teardown and
immediate re-add, active-operation overlap rejection, early watcher/rollback
cleanup, and duplicate server RemoveBus joining through admission cleanup.
The focused packages pass repeated race-enabled tests.

These are dormant software tests. No hardware was accessed by this tranche.

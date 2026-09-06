# MS-GIPUSB codec subset contract

Status: the codec/persona core is composed into VIIPER's retained USB/IP
backend by the authenticated, explicit-identity production factory. The
composition and duplex broker are verified by offline tests; real Windows
enumeration/game compatibility and hardware timing are not yet claimed.

## 2026-09-01 production composition update

The original sections below describe the individual codec, identity,
lifecycle, retained-transfer, and feedback components at the time each was
introduced, and therefore retain names such as “dormant” where that is part of
the type's safety boundary. The current production composition is narrower
than the generic device factory and consists of:

- authenticated `bus/{id}/add-authorized-xboxone` creation;
- `RegisterProductionXboxOneRetainedUSB`, which consumes one explicit external
  identity authorization and registers the retained USB device;
- optional localhost USB/IP auto-attach through the existing server policy;
- the authenticated `xboxone` stream, carrying 24-byte semantic input toward
  the persona and fixed 72-byte canonical feedback toward DS4Windows; and
- exact-registration removal and generation/ownership fencing on teardown.

There is still no Microsoft/project VID/PID, manufacturer, product, serial,
firmware, or Device ID fallback. The caller must supply a lawful identity
bundle; the authenticated factory validates the attestation but cannot prove
external ownership. Generic `deviceSpecific` construction remains unable to
create this persona.

This package implements the parts of Microsoft MS-GIPUSB revision 1.0 that can
be encoded from an explicit caller-owned identity without choosing a default
identity or inventing security behavior. The one-shot gate binds caller-
supplied identity strings and the caller's explicit authorization attestation
to one exact engine. The profile/codec alone is not a virtual device; the
authenticated retained-USB production composition above is the only path that
registers it as one.

## 2026-09-02 retained input ownership update

The retained production adapter now uses the shared `FixedReportScheduler`
for ordinary buttons, Share, and trigger zero/nonzero transitions. Continuous
axis updates coalesce; the last continuous state before an ordered boundary
is retained, including a trigger peak before release. Guide keeps its separate
Command 7 queue. No broker wire format or canonical mapping model changed.

The semantic claim is paired with the engine's immutable GIP report and input
sequence. Failed USB delivery preserves both until exact retry or lifecycle
retirement; only complete USB write/flush commits the semantic claim. START
establishes its current baseline at actual selection, and publications during
that write remain ordered behind it. Gated polls cannot activate the journal.
Delivered reconfiguration and IN halt-clear establish fresh presentation
ownership; when the persona remains active, successor publications are
journaled immediately. Permit resumes a suspended journal without resetting
one already active. Ordinary unlink and OUT halt-clear do not discard input.

Overflow is an explicit terminal history fault, not silent edge replacement.
It fences new preparation and prevents same-incarnation broker reconnect or
reset recovery. The adapter signals an exact-lease `ImportRetirementRequest`,
including when no host IN request is queued. The retained worker records that
diagnostic separately from ownership failure, revokes admission, and retires
each pending ticket before the ordinary drain/acknowledged Stop boundary.
Already-admitted GIP completions remain accountable once; faulted Source
history is not committed, replayed, or resynchronized by that bookkeeping.
Any ticket-retirement, feedback, or ownership uncertainty still quarantines.

X1BR v1 framing and accepted/rejected bytes are unchanged. A history-fault
rejection revokes further input, activation and replacement streams, but keeps
the original feedback/ACK reader alive for teardown. Its absolute 30-second
socket deadline bounds both directions and never extends with traffic. A Stop
may precede the input rejection on the duplex stream; clients must demultiplex
both. Only the exact Stop ACK permits safe retirement; timeout/socket closure
is never a substitute. These are software-tested semantics, not hardware or
installer readiness.

The 4 ms descriptor requirement below is not proof of a hard 4 ms service
floor. USB 2.0 section 5.7.4 permits shorter host service, down to 1 ms at full
speed. A bounded faster virtual-host input policy is a separate experiment;
the current default still follows the descriptor. Do not change GIP speed or
descriptor intervals to disguise that experiment, or equate response counts
with distinct input readings. See `PROVENANCE.md` and the dated validation ledger.

The experiment now accepts a cold retained-IN service policy of `0` (descriptor,
the unchanged default), `1`, `2`, or `4` milliseconds. Deterministic integration
decodes uniquely changing GIP states through the actual authorized persona,
shared journal, and USB/IP import. It does not measure Windows or game-visible
input timing, and does not change advertised speed, descriptors, OUT, or EP0.

Identical ordinary publications no longer generate duplicate USB input. When
the journal has no pending work, IN parks on readiness plus the next protocol
status deadline. Capturing readiness before selection prevents a publication
racing with an empty decision from being missed. Explicit KeepAlive reports
remain allowed; retries and mandatory START input remain exact.

Periodic Status Device reporting uses the existing status codec and Global
sequence pool. Its first window begins at delivered START status, with one-
second intervals for ten seconds, followed by twenty-second intervals. Failed
delivery retains exact bytes and deadline; late delivery schedules forward
without catch-up bursts. STOP/reset/disconnect retire the timer. This closes
periodic reporting only: charging/battery-type change-triggered status remains
unimplemented, and the broader status-events capability blocker remains set.

## Unregistered controller profile and USB descriptors

`NewUnregisteredControllerProfile` is the only descriptor/Hello construction
root. It requires all of the following from its caller:

- a non-zero VID and PID;
- a primary Device ID with the section 2.2.1.3 `0000FFFB` high-word prefix;
- a valid USB `bcdDevice` value;
- a non-all-zero four-part firmware version and explicit hardware version;
- actual peak power in 2 mA units; and
- explicit interrupt OUT and IN intervals, both at least 4 ms.

The package supplies none of those values. A profile's zero value cannot emit
anything. Validation only checks the local wire grammar; it cannot establish
that the caller owns the VID/PID or that a Device ID was generated with the
required hardware randomness.

For the controller-only, non-audio descriptor shape, the profile writes:

- the exact 18-byte USB 2.0 device descriptor from table 7, including device
  class/subclass/protocol `FF/47/D0`, EP0 size 64, and string indexes 1/2/3;
- the exact 32-byte configuration tree from tables 8 through 11: one
  `FF/47/D0` interface, endpoint `01` interrupt OUT and endpoint `81`
  interrupt IN, each with a 64-byte maximum packet;
- the `0409` LANGID descriptor required by section 2.2.4;
- the exact table 5 `MSFT100` UTF-16LE descriptor with vendor code `0x90`;
  and
- the exact 40-byte table 6 extended compatible ID descriptor with one data
  interface and `XGIP10` padded to eight bytes.

The configuration is fixed to the controller value `bmAttributes=0xA0`
(bus-powered plus remote wake). The caller supplies `bMaxPower` and both
polling intervals because neither can be inferred from controller semantics.
No audio interface descriptors are produced.

The device descriptor refers to Manufacturer, Product, and Serial Number
string indexes, but this package does not invent those strings. The legacy
profile/control-plane path therefore still has no values for indexes 1/2/3.
The separate dormant external-identity gate accepts all three strings only
from its caller, encodes non-empty valid Unicode as bounded UTF-16LE, and
requires the serial to be exactly 32 ASCII hexadecimal digits containing the
profile's 16-digit numeric Device ID. Treating `%016x` as the fixed-width
textual form of that 64-bit numeric ID is a deliberately restrictive local
normalization inference: it may reject another convention, and it does not
generate a serial or establish uniqueness. The caller's `Granted` decision is
an attestation only; the package does not prove VID/PID ownership, trademark
rights, manufacturer identity, serial uniqueness, or any other legal right.

The authorized configuration is opaque and one-shot. Profile construction
and metadata binding carry separate private issuance credentials, so equal
numeric fields or equal metadata bytes from a later construction cannot pass
as the same lifetime. Construction consumes the authorization before an
engine can escape. The resulting binding authenticates the exact engine
pointer and its embedded canonical control plane; a copied authorization is
stale after the sole successful construction, while copied engine or control
plane values lose string authority. The generic registry/factory cannot
construct or attach this gate; only the typed authenticated production factory
listed above can consume it.

Before Unicode validation, each raw source string is capped at 378 UTF-8
bytes: no longer input can fit the 126 UTF-16 code units available in a
254-byte descriptor. This bounds construction scanning as well as the emitted
record.

### Offline EP0 control plane

`USBControlPlane` implements the controller-only state/admission matrix in
MS-GIPUSB table 4 without registering a device or calling a backend. It starts
in USB Default state and transactionally models Default, Addressed, and
Configured, plus a local Detached boundary. The implemented request surface is
limited to the exact table forms needed by the fixed controller profile:

- Device and endpoint `GET_STATUS`;
- Device Remote Wakeup and Endpoint Halt `SET_FEATURE`/`CLEAR_FEATURE`;
- `SET_ADDRESS`, `GET_CONFIGURATION`, and `SET_CONFIGURATION` for
  configuration zero or the single GIP configuration one;
- truncated-to-`wLength` Device, Configuration, and LANGID descriptors;
- on the exact authorized engine only, caller-bound Manufacturer, Product,
  and Serial descriptors at indexes 1/2/3 and LANGID `0x0409`;
- the exact addressed/configured `MSFT100` and `XGIP10` requests.

Every interface status/feature request, Device Qualifier, SET_DESCRIPTOR,
controller-only GET/SET_INTERFACE, SYNC_FRAME, Extended Properties request,
unlisted request, malformed field, inactive endpoint, and state-illegal
request stalls. On the legacy plane, Manufacturer, Product, and Serial
descriptor requests return the more specific
`ErrUSBStringDescriptorUnavailable` while also classifying as a stall. The
authorized plane serves only indexes 1/2/3 with LANGID `0x0409`; wrong
language, index, malformed private descriptor, stale binding, or a copied
plane stalls without a claim. Standard `wLength` prefix truncation still
applies. The control plane never substitutes placeholder identity strings.

Each accepted request uses claim, exact-size `AdmitAndCopy`, and terminal
resolve. A mutating request changes Address, Configuration, Remote Wakeup, or
Endpoint Halt only after admitted delivery. Failed or synchronously
cancelled/drained attempts have no state effect. Reset, disconnect, and
reconnect require an exact successor generation and cannot overtake an
outstanding claim; each clears all volatile EP0 state. Reconfiguration clears
endpoint-halt state. Response construction is fixed-capacity and allocation
free after construction on the fixed descriptor path.

Endpoint-recipient requests follow the table-4 state matrix. In Addressed
state only EP0 is in scope; in Configured state EP0 and the declared `01`/`81`
data endpoints are in scope. USB 2.0 permits a device to accept either
direction-bit spelling of a control-pipe endpoint address, so `0000` and
`0080` name the same EP0 state. This persona does not implement the optional,
not-recommended Default Control Pipe Halt feature: EP0 `GET_STATUS` and an
idempotent `CLEAR_FEATURE(ENDPOINT_HALT)` are accepted where table 4 permits,
but `SET_FEATURE(ENDPOINT_HALT)` for EP0 stalls. The declared interrupt
endpoints implement set, clear, and status normally while Configured.

This state machine is not the USB/IP dispatcher. It does not assign a lawful
VID/PID, complete the missing string/metadata/security gates, own GIP message
sequence pools, apply feedback neutralization, or coordinate the existing GIP
lifecycle with the backend. A future integration must execute one reset fence
across this EP0 generation, `ControllerLifecycle.ClaimRestartAfterUSBReset`,
every message-specific `SequenceCounter`, input-presentation generation, and
feedback output revocation; none may reset independently and resurrect
predecessor work.

The dormant retained USB adapter advertises a 254-byte maximum control
response, matching the largest even `bLength` representable by a USB string
descriptor. The retained scheduler preallocates that one response window while
leaving its one-slot queues, zero-byte EP0-OUT slab, and 64-byte interrupt
IN/OUT bounds unchanged. Offline end-to-end tests cover the exact 254-byte
authorized descriptor, smaller host `wLength` truncation, rejection of an
inconsistent setup/transfer length, complete 254-byte replies when a lawful
host maximum is 255 or 65,535, unchanged request-slab geometry, and zero warmed
allocations. A host `wLength` is a response maximum, so it is not rejected
merely because it exceeds the descriptor's actual size. This is still dormant
serializer capacity, not production identity or transport activation.

## Primary Hello

`EncodeHelloMessageInto` writes the exact 32-byte table 27 primary Hello:
header `02 20 SS 1C`, the 28-byte little-endian payload, fixed RF/Security/GIP
versions `1.0`, and the caller's firmware and hardware versions. It is a method
of the validated profile, so the Hello VID/PID always match the USB device
descriptor from that same profile as section 2.2.1.1 requires. Sequence zero,
an invalid primary Device ID prefix, all-zero firmware, wrong protocol
versions, non-primary flags, and non-exact lengths fail closed.

The profile does not generate the random low Device ID bytes. Arrival cadence
is a lifecycle responsibility described below.

## Extended Status without events

`ExtendedStatusNoEventsBodyV1` implements the exact four-byte payload required
by section 3.1.5.5.2.2, table 29 when `Events Present` is clear:

| Offset | Width | Meaning |
| ---: | ---: | --- |
| 0 | 1 | power level, charge state, battery type, and battery level |
| 1 | 1 | reserved bits 7:2 zero, Events Present zero, Device Active bit 0 |
| 2 | 2 | reserved zero |

The status sub-fields follow table 30. Powering Off/Resetting and Full Power
are the only accepted power values; the former Standby value and the reserved
value fail closed. Reserved charge and battery-type values, non-zero reserved
extended bits/bytes, and widths outside their declared fields also fail
closed. A zero composite Status byte remains representable because the source
explicitly permits it for a powering-off USB-only device without batteries.

`ExtendedStatusNoEventsHeader` and the full-message codec use system Command
3, a caller-supplied non-zero value from the global sequence pool, no ACME,
primary expansion index zero, and payload length four. The codec rejects the
deprecated one-byte Legacy Status form and rejects `Events Present` rather
than interpreting the 35-through-55-byte event-bearing grammar as a short
message. It does not invent fault/disconnect/performance/power event records.

`NewWiredNoBatteryStatus` implements only the explicit table-31 policy for a
USB-bus-powered controller with no battery: Full Power (or Powering Off), Not
Charging, Battery Absent, Critically Low, and Device Active clear. Other
battery/connection policies remain caller inputs and must reflect actual state.

## Four-byte single-packet header

`SinglePacketHeader` implements the non-extended, non-fragmented header in
section 2.2.10, table 12:

| Offset | Width | Meaning |
| ---: | ---: | --- |
| 0 | 1 | data class in bits 7:5; message number in bits 4:0 |
| 1 | 1 | flags: system, acknowledgement requested, expansion index |
| 2 | 1 | non-zero sequence ID |
| 3 | 1 | payload length, excluding the header |

Only data classes 0 through 3 (Command, Low Latency, Standard Latency, and
Audio) are accepted. Fragment, InitFrag, the mandatory-zero flag bit, sequence
zero, and an extended length byte all fail closed. Command, Low Latency, and
Standard Latency messages have a 64-byte MTU inclusive of this header, so their
maximum payload is 60 bytes. A four-byte Audio header can represent at most 127
payload bytes; larger Audio messages need the deliberately absent extended
header grammar.

The generic codec accepts all five-bit message numbers. MS-GIPUSB section
2.2.10.1 says Command message number 31 is reserved, while section
3.1.5.5.9.2 defines a Debug Command using message type `0x1f`. That source
contradiction is not resolved by silently rejecting a syntactically valid
five-bit value; any message-specific layer must apply the policy for the exact
message it implements.

`DirectMotorHeader` and `GamepadInputHeader` produce the exact class, message
number, flags, and payload length from tables 56 and 57. They do not allocate a
sequence or write a USB transfer.

A four-byte GIP header frames one message, not necessarily one USB transfer.
Section 3.1.5.3 permits multiple small messages to be coalesced into one
packet. `DecodeSinglePacketHeader` therefore consumes an exact four-byte header
slice and does not claim that a transport buffer ends after the declared
payload. `DecodeControllerDownstreamPacket` walks every declared boundary in a
one-through-64-byte controller data packet and classifies only the exact host
message families implemented here. An empty or oversized packet, partial
later header/body, unsupported family, fragment, extended header, or malformed
later body rejects the entire packet and returns no prefix. Its fixed-capacity
result contains typed values rather than caller-owned slices, preserves wire
order, and allocates no memory after warm-up.

That packet codec is deliberately not a fragment reassembler or replay window.
`ControllerDownstreamPacketExecutionOwner` is a separate dormant transaction.
Its standalone `Claim` compiles the self-contained Direct Motor and Guide LED
effects. It first compiles the complete fixed-capacity vector, then requires
one participant to preflight the whole vector before any owner counter or claim
changes. Preflight is contractually bounded synchronous in-process work and
cannot wait on I/O while holding the owner serializer. Final admission repeats
that whole-vector preflight. Execution is one atomic participant call, never
one call per message; success means every action became visible in wire order,
while a timely error must prove no action and no late effect. Deferred, proven
failure, and synchronously cancelled/drained
attempts retain the exact packet epoch and immutable vector for retry. Panic,
deadline ambiguity, or drain failure fences or quarantines rather than guessing
an outcome.

`ControllerPersonaDownstreamPacketBatchOwner` is the dormant canonical bridge
for mixed packets. It reserves the complete packet through the existing
persona claim lane, resolves one exact lifecycle cursor or reliable ACK
disposition from the authoritative engine, and supplies that context alongside
every ordered Direct Motor / Guide LED value to the same whole-vector executor.
The action contains any exact persona egress wire image, its independent output
sequence, clear epoch, or reliable-ACK disposition; no second mapper interprets
the host bytes. Final persona admission follows the participant's second
whole-vector preflight. Delivered, deferred, proven-no-effect failure, and
synchronously cancelled/drained outcomes are committed identically to both
owners. The executor first issues an unexported terminal credential and enters
a resolution-prepared state which rejects Execute, CancelAndDrain, and duplicate
resolution. Persona commit therefore cannot race an admitted Deferred outcome
against a newly starting participant effect; consuming the credential performs
only the already-prevalidated executor state transition. Divergence, panic,
late-effect ambiguity, and ACK retry expiry quarantine instead of enabling
replay. Any closed-domain vector invariant which unexpectedly fails after the
persona context claim was selected also permanently quarantines while retaining
that exact claim; it never advertises rollback or successor eligibility.

The canonical lifecycle and reliable-transfer components still expose one
context-bearing claim at a time. A packet with more than one lifecycle/ACK
member therefore fails before persona or participant mutation. STOP, OFF, or
RESET with any sibling is likewise admitted only as a single-member packet:
their first selected cursor gates upstream while the mandatory output clear is
a successor cursor, and terminal completion may also advance the transport
generation. Allowing later feedback in the same atomic vector would therefore
leave output active in Idle/termination or replay predecessor feedback after a
boundary. QUIESCE remains eligible for a mixed packet because its selected
cursor is the clear itself. Supporting a larger context set requires real
canonical multi-claim authority and a heterogeneous participant which includes
the successor clear; copying the engine, committing a prefix, delaying the
clear, or inventing ACK context remains forbidden. Neither owner splits a USB aggregate nor interprets wire
sequence as a duplicate or replay policy. The ACME flag is represented because
it is valid header
syntax. The generic header alone does not identify Protocol Control semantics.
The exact ACK codec below validates system Command 1 and bridges its contiguous
progress into the metadata transaction. Generic receive reassembly remains
absent. The two ordinary message helpers request no acknowledgement, matching
tables 56 and 57.

## Protocol Control ACK

The consolidated MS-GIPUSB 1.0 document summarizes Protocol Control but omits
its body table. The same pinned official download contains
`H001419 - Original GIP Spec.docx`; table 4-14 and its USB traces establish the
exact 13-byte ACK message:

| Offset | Width | Meaning |
| ---: | ---: | --- |
| 0 | 4 | `01 20 SS 09`: primary system Command 1, matching sequence |
| 4 | 1 | ControlCode `0x00` (ACK; every non-zero code unused/reserved) |
| 5 | 1 | referenced MessageType |
| 6 | 1 | referenced Flags with only System and Expansion Index retained |
| 7 | 4 | total sequential bytes received, little-endian |
| 11 | 2 | receiver remaining buffer, little-endian |

`ProtocolControlACKBodyV1` preserves the full 32-bit offset and 16-bit receiver
buffer. Fragment, InitFrag, ACME, and Reserved bits in referenced Flags fail
closed. The full-message codec also rejects non-primary, non-system, ACME,
wrong-class/message, and wrong-length envelopes. The ACK header sequence is
returned to the caller because it must match the acknowledged message.

`DecodeMetadataReliableAcknowledgement` accepts local transport generation and
transfer epoch out of band, requires a primary system Command-4 reference, and
narrows the 32-bit offset only after enforcing the metadata length ceiling.
The receiver-buffer field is retained for observation but is not treated as a
sender-side progress grant. `MetadataTransfer.Acknowledge` remains the final
generation/epoch/sequence/sent-range authority.

## Standard Gamepad Input Report

`InputStateV1` is the transport-neutral controller state and its zero value is
neutral. `GamepadInputReportV1` wraps that state with the protocol-only Keep
Alive bit, so lifecycle signaling does not become an application button in the
canonical controller model. The standard input payload in section
3.1.5.6.1.1, table 57 is exactly 14 bytes, little-endian:

| Offset | Width | Meaning |
| ---: | ---: | --- |
| 0 | 2 | digital buttons and Keep Alive |
| 2 | 2 | left trigger, 0 through 1023 |
| 4 | 2 | right trigger, 0 through 1023 |
| 6 | 2 | left stick X, signed |
| 8 | 2 | left stick Y, signed |
| 10 | 2 | right stick X, signed |
| 12 | 2 | right stick Y, signed |

The low button byte uses bit 0 as reserved, bit 1 as Keep Alive, and bits 2
through 7 for Menu, View, A, B, X, and Y. The high byte maps D-pad Up, Down,
Left, Right, left/right bumpers, and left/right stick buttons to bits 0 through
7. Opposing D-pad bits are preserved because the wire table defines four
independent bits and does not declare those combinations malformed. Any SOCD
policy belongs in profile mapping.

Guide and Share are explicit semantic values so a translator cannot silently
lose them. Neither is in the standard payload. Base-form encoding rejects
both. The strict official Console Function Map compiler binds Share-capable
metadata to the matching 36-byte input form; opaque external metadata remains
base-only even if its bytes resemble that profile. Guide uses the separately
validated system Command `0x07` status codec. The retained broker owns a fixed
128-entry ordered edge ring: admission transfers one edge into the engine's
immutable retry owner, and saturation rejects the publication and revision
instead of overwriting an older transition.

### DS4Windows broker semantic input v1

`EncodeSemanticInputWireV1Into` and `DecodeSemanticInputWireV1` define the
private, 24-byte DS4Windows-to-VIIPER state seam. This is not GIP wire data.
It exists so profile mapping does not move into the USB persona and so GIP
sequence, Keep Alive, Guide-status, Share-extension, lifecycle, and endpoint
presentation remain VIIPER-owned.

The little-endian fields are version `u16` (`1`), embedded size `u16` (`24`),
semantic buttons `u32`, left/right ten-bit triggers, four signed 16-bit sticks,
and a mandatory-zero four-byte tail. Button bits 0 through 15 are Menu, View,
A, B, X, Y, D-pad Up/Down/Left/Right, left/right bumper, left/right stick,
Guide, and Share. Every higher bit is reserved. The shared cross-language
golden vector is:

```text
01 00 18 00 65 DA 00 00 34 02 CD 03
02 01 FE FF 00 80 FF 7F 00 00 00 00
```

Wrong lengths, contract fields, reserved data, and triggers above 1023 fail
closed. The codec is atomic on encode failure and allocation-free on successful
warm paths. Codec tests alone do not satisfy production registration, Windows
binding, or hardware gates.

`DormantRetainedUSBAdapter.PublishSemanticInputWire` is the retained transport
adapter for this frame. The default-off internal factory exposes only the
wrapper's allocation-free-on-the-warm-focused-test,
exact-registration-fenced form of that method; the generic API device registry
remains closed. This is not an end-to-end latency result. It decodes into the
adapter's generation/revision-fenced shared semantic journal, without a second
mapping model or GIP sequence pool. Guide uses its distinct ordered Command 7
queue. Share is admitted only for an engine carrying the strict
compiler-issued Console Function Map metadata capability.

`EncodeGamepadInputMessageInto` and `DecodeGamepadInputMessage` own the exact
18-byte, uncoalesced envelope `20 00 SS 0E || payload`. They reject a system,
ACME, expansion, wrong-class, wrong-message, wrong-length, fragmented,
extended-length, or zero-sequence header. Encoding first builds a private
fixed-size image, so a body or header error cannot partially modify the
caller destination. Sequence allocation and delivery commit remain the
caller-owned unique Gamepad Input sequence-pool transaction.

MS-GIPUSB says input reports should be sent only when a field changes and that
the first report after Set Device State: Start is mandatory and must reflect
current device state. Those are future lifecycle/scheduling requirements, not
side effects of this stateless body codec.

## Direct Motor Command

`RumbleBodyV1` implements the exact nine-byte payload in section 3.1.5.6.1,
table 56:

| Offset | Width | Meaning |
| ---: | ---: | --- |
| 0 | 1 | fixed Direct Motor Command value `0x00` |
| 1 | 1 | motor bitmap; bits 7:4 must be zero |
| 2 | 1 | left impulse level, 0 through 100 percent |
| 3 | 1 | right impulse level, 0 through 100 percent |
| 4 | 1 | left vibration level, 0 through 100 percent |
| 5 | 1 | right vibration level, 0 through 100 percent |
| 6 | 1 | duration; zero cancels all motors, otherwise 10 ms units |
| 7 | 1 | delay; zero means no delay, otherwise 10 ms units |
| 8 | 1 | repeat; zero means play once, otherwise repeat count |

The bitmap maps right vibration, left vibration, right impulse, and left
impulse to bits 0 through 3. A non-zero level whose bitmap bit is clear is not
invented as a protocol error because the Microsoft table does not make that
combination malformed. Duration zero is the normative all-motor cancellation
and levels are ignored. `IsCancellation` implements that exact rule after
validating the entire body, including valid duration-zero bodies with non-zero
Delay or Repeat. `IsCanonicalImmediateStop` is the narrower local generation
policy requiring zero Delay and Repeat; feedback consumers must use
`IsCancellation` when interpreting host commands.

No percentage-to-actuator transfer function is defined here. Translation to a
DualSense adaptive trigger, Switch 2 HD rumble, or a two-motor device is a
separate capability/fidelity policy.

### Canonical DS4Windows feedback bridge

`ControllerPersonaCanonicalFeedbackFrame` is a dormant transport-boundary
translation from an already serialized persona-local action to the existing
project-wide CFBK v1 value. It is not a feedback executor or arbitrator.
`ControllerPersonaLocalExecution.Order` is the CFBK source-local sequence; the
adapter does not allocate a second counter. A caller-supplied binding must
match the exact persona generation and provide an Xbox One/Series source plus
non-zero physical-device generation, transport generation, ownership epoch,
and bounded CFBK TTL. None is inferred from the raw persona.

The four Direct Motor percentages are independently enabled and mapped as
left/right vibration to body-low/body-high and left/right impulse to
left/right trigger. Each enabled percentage is rounded to the nearest
normalized unsigned 16-bit value. Duration zero becomes lease-retaining CFBK
`Neutral` because a later host motor command in the same persona epoch may
resume. `ClearOutputs` alone cannot choose `Neutral` versus `Stop`: the persona
uses the same typed action for recoverable configuration loss or USB reset and
for terminal disconnect. The already-authenticated outer lifecycle must pass
an explicit intent. Recoverable clears become `Neutral`; only the separately
authorized disconnect clear becomes terminal CFBK `Stop`. Non-cancelling Delay
or Repeat is rejected because CFBK v1 cannot represent the timed program
without changing semantics. An active frame's TTL is capped by both the
binding's maximum 250 ms value and Direct Motor's duration; a future bounded
renewal service is required for a longer effect.

`ControllerPersonaLocalExecution.ClearEpoch` and CFBK `OwnershipEpoch` are
intentionally different identities. `ClearEpoch` is VIIPER's local exactly-once
clear-execution ledger. It is validated as non-zero by the persona action but
is neither copied into nor compared with CFBK. `OwnershipEpoch` is the separate
DS4Windows NativeGame lease supplied by the outer physical-target binding.
Persona generation plus execution order fence the local action; the canonical
source/device/transport/ownership tuple fences the broker publication.

Encoding is exact 72-byte, failure-atomic, and allocation-free after warm-up.
DS4Windows validates the same source/device/transport/ownership binding and
then publishes into its existing canonical NativeGame arbitration slot. No
second mailbox, ownership machine, expiry policy, physical-device mapper, or
hardware output path is introduced here.

`ControllerPersonaCanonicalFeedbackExecutor` is the dormant concrete local
executor for this boundary. Reset drain is reversible and terminal drain is
permanent. A reset clear rotates its private persona-generation binding only
after the successor `Neutral` is synchronously accepted, so stale predecessor
feedback is rejected and later successor feedback can resume in the same CFBK
ownership epoch. A disconnect clear emits `Stop` and closes that executor
permanently. Drain wait timeouts retain their fence and may resume the exact
wait; they never reopen admission. The publisher's error contract is stronger
than an ordinary socket error: it must prove no bytes were accepted and no
late publication can occur. An uncertain IPC result requires higher-level
session close/drain and quarantine and cannot implement this interface
directly.

`EncodeDirectMotorMessageInto` and `DecodeDirectMotorMessage` own the exact
13-byte, uncoalesced envelope `09 00 SS 09 || payload`. The full-message
decoder admits only the non-system, non-ACME, primary-device Direct Motor
form. Encoding is atomic with respect to its destination. Sequence
allocation, receive replay policy, timed playback, cancellation ownership,
and feedback arbitration remain separate boundaries.

## Guide LED Command

`GuideLEDCommandV1` implements only the Guide-button form of system Command
10 from section 3.1.5.5.7, tables 41 and 42. Its exact seven-byte envelope is
`0A 20 SS 03 00 PP II`, where `PP` is one of Off (`00`), On (`01`), Fast Blink
(`02`), Slow Blink (`03`), Charging Blink (`04`), or Ramp to Level (`0D`), and
`II` is the inclusive 0-through-47 percent intensity. Every other pattern,
non-zero command selector, and intensity above 47 fails closed.

The codec preserves a validated host command; it does not animate a physical
LED, choose ownership priority, or translate brightness. The deprecated
six-byte IR LED form under the same message number has a different grammar
and is not accepted by this type. The lifecycle machine does not consume LED
messages: a future downstream dispatcher must route this typed command to the
feedback plane without treating it as a state transition.

## Sequence and lifecycle boundary

`SequenceCounter` claims values 1 through 255 and wraps to 1. Claim does not
consume the value: the caller performs a final `CanAdmit` check and resolves the
claim as delivered before the committed sequence advances. Defer or write
failure retains the exact value as a mandatory retry. A strict-successor
generation reset invalidates predecessor claims and restarts at one.

Each counter represents one sequence pool. Section 2.2.10.3 requires a global
pool for specified system messages and unique pools for Security, Extended
Command, Audio, and vendor messages including Direct Motor and Gamepad Input.
A caller must therefore use separate counters for Direct Motor and Gamepad
Input; `Session` intentionally does not hide them behind one ambiguous counter.
Each counter is caller-owned, serialized, and not safe for concurrent calls.

`AuthProvider` remains an opaque, unavailable-by-default boundary. No security
messages, credentials, handshake, bypass, or success policy are implemented.
Provider output is fail-closed: the destination is cleared on provider error or
an invalid count, and its unused tail is cleared after partial success.

## Metadata and reliable-transfer boundary

The generic metadata seam does not generate or interpret caller bytes. A caller
can bind an externally compiled blob to one validated profile with
`BindExternallyCompiledMetadata`. The resulting type has no public fields and
cannot be reused with a different descriptor/Hello identity. Binding owns a
private copy, so later caller mutation cannot alter a selected packet or retry.
That external path checks only a non-zero length and the selected two-byte TLO
ceiling of 16,383 bytes; it does not claim to parse or validate metadata
contents. An external metadata compiler/reviewer must still establish at
minimum:

- matching firmware Major/Minor in `SupportedDeviceFirmwareVersions`;
- the required system command declarations;
- the exact Gamepad Input and Direct Motor message declarations and lengths;
- the intended controller interfaces and type handler;
- the desired security policy; and
- the absence of fields or interfaces not implemented by the device.

The separate in-package official gamepad compiler implements a closed, strictly
validated subset for the production persona. The pinned official `GIP Metadata
Compiler.docx` establishes the binary object layout and includes the 182-byte
ordinary-gamepad example also printed by MS-GIPUSB. Its
`ValidateJsonMetadataHeader`, `ValidateJsonDeviceMetadata`, and
`ValidateJsonMessages` methods are empty, so VIIPER independently validates
every generated byte and the coupled firmware, commands, message lengths and
directions, interfaces, type, and security policy. The base Windows-PC variant
is 198 bytes; the Console Function Map variant is 214 bytes. Both add the
official `IDevAuthPCOptOut` GUID
`7a34ce77-7de2-45c6-8ca4-0042c08bd94a`, as directed by the "Security" section
of `H001419 - Original GIP Spec.docx` for controllers talking to Windows PC over
USB. The latter additionally advertises Console Function Map and expands input
from 14 to 32 bytes.

The PC opt-out is a metadata capability, not a cryptographic Security-message
handler. After Windows succeeds that opted-out exchange, it sends the exact
table-49 Security Data Complete marker `06 20 SS 02 01 00`. VIIPER accepts that
non-ACME two-byte completion only while Active and makes no lifecycle change or
device response. Authentication data, fragments, ACME-bearing Security
messages, mutated completion bodies, and every other Security form still fail
closed; VIIPER never fabricates a challenge response.

`MetadataTransfer` provides only source-established framing and ordering:

- blobs of 1 through 60 bytes use a single system Command 4 response;
- larger blobs use a six-byte even header and at most 58 payload bytes per
  64-byte Command packet;
- every fragment uses one fixed non-zero sequence;
- the initial fragment is `04 F0 SS LL TL TH` and requests ACK;
- middle fragments use `04 A0 SS LL OL OH`, or `B0` when the caller's
  monotonic clock reaches the 60 ms ACK-request cadence;
- the final fragment requests ACK with `04 B0 SS LL OL OH`; and
- after acknowledgement of all bytes, the transfer emits the zero-payload
  table 38 Metadata Complete header with the same sequence and total length.

`TL/TH` and `OL/OH` use the exact two-byte base-128 extension form: the low
byte has bit 7 set and the high byte has bit 7 clear. The first and final
fragments always request acknowledgement. For a long transfer, a middle
fragment requests ACK once 60 ms have elapsed since the last accepted ACK,
following section 3.1.5.1's current-library cadence. That time-dependent bit is
re-evaluated at final admission: a never-delivered middle-fragment selection
can only be upgraded to request ACK. Its payload, offset, sequence, and every
other header field remain unchanged, and no retained fragment can be overtaken.

Metadata egress is transactional. `Claim` exposes no wire bytes; it retains a
fixed 64-byte private image and proposed post-state. `AdmitAndCopy` performs the
serialized final generation/identity/cadence check and overwrites the backend
destination from that private record immediately before the write. Only
`Resolve` with an admitted delivered outcome commits offset, completion, or ACK
state. Defer and write failure preserve an exact private mandatory retry, so
caller-buffer mutation cannot corrupt it and new fragments cannot overtake it.
A reset requires strict non-zero successor transport-generation and local
transfer-epoch values and invalidates predecessor claims, retries, and ACKs.

`ReliableAcknowledgement` is the identity-fenced semantic form of the exact
Protocol Control body. Transfer generation and local reliable-transfer epoch
remain out-of-band transaction identity. Message, sequence, contiguous-byte
progress, and receiver-buffer space come from the strict decoder.
`MetadataTransfer.Acknowledge` accepts the exact in-flight identity for both
requested ACKs and the source-defined unsolicited progress/gap ACKs. A count
below the accepted high-water mark, beyond the delivered sent end, or carrying
a predecessor generation/epoch/message/sequence fails closed. An equal count
is an idempotent duplicate. Forward progress advances the high-water mark; a
count below the current send offset rewinds to the first missing byte and
invalidates any never-delivered selection/retry beyond it. ACK handling is
serialized against an admitted endpoint write, and accepted progress is
rebased into any selected-but-not-admitted record so later delivery cannot
restore stale state.

`PreviewAcknowledgement` performs the same identity, range, clock, deadline,
and admitted-egress validation without observing the clock or changing any
transfer field. The composed persona uses that pure preview to select a
zero-byte `ApplyMetadataAcknowledgement` claim, re-previews at final admission,
and calls `Acknowledge` only after the host's receive transaction is delivered.
Progress, rewind, and duplicate therefore remain predicted dispositions until
delivery. A failed receive retains the exact ACK retry. If that retry reaches
the reliable deadline, `ClaimRetry` retires it by faulting the metadata
transfer so the ordinary metadata-failure Hello can proceed instead of looping
an ACK which can never be admitted.

The one-second ACK window starts only when an ACME packet resolves as delivered.
`Poll`, `Claim`, and `Acknowledge` all observe the same caller-supplied monotonic
millisecond clock; reaching the deadline faults the transfer deterministically.
Only an explicit successor-generation reset recovers it. This package does not
own a timer thread: integration must poll or otherwise call the transfer with a
monotonic clock. Deadlines saturate at the maximum clock value, so a valid
admitted delivered resolution cannot fail after the endpoint write succeeded.

## Controller startup lifecycle

`ControllerLifecycle` is a pure controller-only interpretation of section
3.1.1 and table 40. It accepts a caller-owned monotonic millisecond clock and
retains a fixed-capacity pending transition while exposing exactly one ordered
action cursor at a time. It does not sleep or perform those actions itself.
Each action has its own claim/admit/resolve boundary: defer or write failure
retries only the current action, successful intermediate actions advance only
the pending cursor, and target state/deadlines/reset generation commit only
after the final action is delivered. The pending transition and cursor are
explicit in `ControllerLifecycleSnapshot`. A claim's local execution fence
contains transport generation, transition epoch, claim token, cursor,
and action. Successful admission makes it an exclusive execution lease. USB
reset cannot overtake that lease: an asynchronous executor must complete it,
fail with no possible late effect, or synchronously cancel and drain it before
resolving `ControllerLifecycleExecutionCancelled` and retrying reset.

- Arrival claims one Hello immediately and schedules the next Hello 500 ms
  after successful delivery. Failed/deferred delivery does not advance cadence.
  No other upstream action is produced there.
- An exact primary Metadata Request (`04 20 01 00`) enters the explicit
  Metadata sub-state and requests metadata transmission. Repeated requests
  restart that obligation under a new never-reused metadata-transfer
  generation. That allocation burns before its BeginMetadata claim escapes,
  even though the active lifecycle value commits only after delivered
  resolution. Asynchronous success/failure carries both the USB transport
  generation and metadata-transfer generation, so a reset or repeated request
  rejects predecessor callbacks.
- Successful metadata transmission enters Idle. The optional lost-START
  assumption is deliberately not used; an explicit START is required.
- START can arrive directly in Arrival or from Idle. It enters Active, orders
  current Status before the mandatory first current-state Gamepad Input
  Report, and only then permits normal upstream publication.
- STOP from Active gates normal upstream traffic before entering Idle. The
  lifecycle additionally orders a bounded logical output clear as a local
  fail-safe policy; MS-GIPUSB itself specifies the state transition, not that
  extra STOP behavior.
- QUIESCE remains Active but orders all output state cleared. FULL POWER has no
  wired-controller action.
- OFF or RESET first gates normal upstream publication, performs a bounded
  logical output revocation, and emits the mandatory powering-off Status over
  the privileged system-message path. The 500 ms completion wait starts when
  the final powering-off Status action is successfully delivered. A
  follow-up OFF, STOP, or RESET during that window expedites teardown without
  changing the original OFF-versus-RESET target.
- A reported metadata/reliable failure claims an immediate Arrival Hello and
  reaches Arrival only after that claim is delivered. A completed USB reset
  has a separate gate/clear/Hello action sequence; its final Hello delivery
  advances a strict generation fence. The authoritative restart invalidates
  predecessor unadmitted claims and retries, but cannot discard an admitted
  execution lease before deterministic completion or cancel/drain resolution.

`DecodeControllerHostCommand` accepts the exact zero-payload Metadata Request
and one-byte Set Device State message established by tables 34, 39, and 40.
It also recognizes one exact 15-byte extended initialization body captured from
the Windows GIP host and corroborated by SDL: `06 00 00 00 00 00 00 55 53 00
00 00 00 00 00`. That compatibility probe is acknowledged only in Arrival,
Metadata, or Idle and makes no lifecycle transition; the ordinary one-byte
Start remains mandatory. The extended fields are not defined by MS-GIPUSB, so
the decoder assigns no meaning to byte `06`, `55 53`, or the reserved bytes.
Every other 15-byte body, reserved one-byte state, non-system flag, expansion
target, ACME request, trailing byte, and unsupported command fails closed.

Lifecycle status actions can use the no-events codec above once their caller
supplies actual status state and a transactional global-pool sequence. The
lifecycle still does not choose a battery policy, fabricate event records,
perform authentication, apply LED commands, or apply controller state.

The output clear on STOP and before OFF/RESET is an explicit local fail-safe,
not an additional wire claim. Integration must execute a decision serially
with normal upstream scheduling: gating takes effect before local revocation;
the powering-off Status and startup Status/input actions use a privileged
path while the gate is closed; local revocation is bounded and cannot perform
a blocking hardware write; and START opens normal publication only after the
mandatory current-state report. A completed USB reset repeats gate, logical
revocation, and Hello in that order. This pure package performs none of those
integration actions itself.

## Offline composed controller persona

`ControllerPersonaEngine` composes the fixed profile, `USBControlPlane`,
`ControllerLifecycle`, reliable metadata transfer, one Global sequence pool,
one Gamepad Input sequence pool, exact input encoding, and the strict
downstream classifier under one serialized adapter-facing action lane. It is
still pure offline code: it neither registers a device nor performs a USB/IP
write, hardware output, timer wait, or Windows binding operation.

Construction requires one already validated unregistered profile, an opaque
identity-bound metadata blob, a current Gamepad Input report, a Full Power
event-free Status value, and a Powering Off event-free Status value. No
identity, string, metadata, battery policy, or status event is fabricated.
Hello, Metadata Response, and Status consume the same non-zero Global sequence
pool. Standard Gamepad Input consumes its own non-zero unique pool. A reliable
metadata response reserves its Global value only after its exact transfer and
first packet have been validated; every fragment and Metadata Complete packet
then reuses that committed identity.

One opaque `ControllerPersonaClaim` owns each EP0 response, lifecycle action,
metadata packet, reliable ACK application, input packet, Direct Motor
application, Guide LED application, or logical clear. `AdmitAndCopy` requires
an exactly sized destination, runs
all inner generation/claim/clock/callback fences before any inner admission,
and copies from a private fixed 64-byte maximum image. `Resolve` prevalidates
every participating inner owner before advancing any of them. Delivery alone
commits protocol state. Deferred, failed, or synchronously cancelled/drained
non-EP0 work retains the same immutable mandatory retry; sequence, wire bytes,
metadata packet identity, typed feedback value, and clear epoch cannot change.
EP0 failure is consumed because the USB host owns control-request retry.

The composition prevents partial inner ownership:

- sequence-dependent encoders and metadata construction run before a sequence
  claim is made;
- every possible lifecycle emission source and hidden inner-idle invariant is
  validated before a lifecycle claim is obtained;
- an immutable retry proves every participating lifecycle, metadata, and
  sequence retry before reclaiming any inner owner;
- lifecycle-plus-sequence and metadata-plus-sequence admissions are fully
  prevalidated before either inner owner mutates;
- delivered metadata completion and terminal transport transitions prove their
  callback/generation fences at admission and again before resolution; and
- reset/disconnect/reconnect verify USB, lifecycle, Global, and Gamepad Input
  generations before retiring predecessor work or advancing any component.

USB reset and disconnect advance the authoritative transport generation
immediately after all fallible preflight checks, invalidate selected-but-not-
admitted predecessor work and retained retries, reset both egress pools to
sequence one, reconstruct lifecycle Arrival while preserving never-reused
local epochs/tokens, and return exactly one `ClearOutputs` claim with a new
logical clear epoch. An admitted external execution lease blocks the boundary
until completion or synchronous cancel/drain. Disconnect cannot reconnect
until its clear is delivered; reconnect creates a fresh attached Default-state
generation without manufacturing a second clear. A host-requested protocol
RESET already performs gate/clear/powering-off Status and therefore advances
the transport generation without a second clear. Clear failure/cancellation
retries the same epoch, so it is not counted as another logical stop. Clear
epoch exhaustion fails before a lifecycle claim or transport-generation
advance instead of wrapping into an ABA identity.

Only exact Metadata Request, supported Set Device State (including the one
strict lifecycle-neutral extended initialization body described above),
Protocol Control ACK, Direct Motor, and Guide LED messages are classified
downstream. The standalone decoder accepts one exact message. The packet
decoder additionally accepts an ordered coalescing of those complete messages
within one 64-byte data packet, but does not itself apply them to the persona.
The standalone atomic owner can
execute a coalesced packet when every member is Direct Motor or Guide LED. The
persona-backed batch owner additionally accepts at most one canonical lifecycle
or Protocol Control ACK member alongside those effects. Every successful
execution still requires one participant to accept the complete exact vector.
A second context-bearing member fails before any claim. An initial participant
preflight rejection after canonical selection retains the exact persona batch
retry because lifecycle/ACK reservation is not misrepresented as rollback.
Short, oversized, partially valid, fragmented, reserved, and unsupported
packets fail closed as a whole.
Protocol Control ACK classification is delivered-only transactional as
described above; merely receiving or previewing its bytes cannot advance the
metadata transfer.
Accepted Direct Motor and Guide LED values become typed adapter obligations;
they affect the feedback snapshot only after delivered resolution. A reliable
metadata timeout selects one failure Hello; delivery retires the exact faulted
transfer and resumes ordinary 500 ms Arrival cadence. Every lifecycle cursor
which emits GIP wire bytes requires USB Configured at selection; losing the
configuration cannot consume the cursor or sequence, while local gate, clear,
and terminal actions remain eligible. Interrupt IN and OUT Halt state gates
only its corresponding GIP direction and consumes no protocol claim while
halted. In particular, a downstream START is preflighted against interrupt IN
before its immediate Current Status action can own a lifecycle cursor or Global
sequence; downstream-only typed feedback remains available under an IN Halt.

A delivered `SET_CONFIGURATION(0)` which actually moves the device from
Configured to Addressed now creates exactly one same-generation typed
`ClearOutputs` obligation after the EP0 response/status completion is known to
have flushed. The transition does not apply the clear early: typed feedback
remains unchanged until the local clear itself reaches delivered resolution,
and failure retries the same clear epoch through a paced worker self-wake,
without requiring another host URB. A selected clear fences unrelated endpoint
admission and reset/disconnect/reconnect boundaries until delivery. Failed EP0
delivery and an idempotent configuration-zero request while already Addressed
create no clear. Clear-epoch exhaustion fails final EP0 admission before a
response can become visible.
This is a local output-safety policy composed through the dormant coordinator
and retained owner, not a claim that MS-GIPUSB defines a new wire message or
that a production backend reports every USB power event.

The dormant retained import boundary also defines a separately typed, exact
whole-device reset transaction. An authority-issued nonwrapping reset token and
per-import successor generation first latch scheduler ingress without waiting
behind the response serializer. The owner then closes Stage/Prepare, reversibly
fences and joins local execution, and stops its worker while the scheduler
completes or retires every exact predecessor control/IN/OUT submission. Only
after those proofs may `beginUSBReset` advance the canonical persona and expose
one successor-generation `ClearOutputs`; a reset-specific authenticated local
executor must deliver that exact clear before owner and scheduler admission
reopen. Exact repeats are cached observations. Selected/retry clears, stale or
cross-import capabilities, deadline/panic/drain ambiguity, and generation/
order/clear-epoch exhaustion quarantine without neutralization, release, or
cross-generation replay. No successor URB is needed.

That transaction is local integration policy, not an MS-GIPUSB wire rule and
not evidence of a backend event. usbip-win2 0.9.7.7 leaves its UdeCx reset
callback registration disabled because the special server request returned
`EPIPE`; its registered power callbacks send no server notification. VIIPER has
no production caller and does not reinterpret configuration, endpoint reset,
connection close, cancellation, or that disabled special setup encoding as
whole-device reset proof.

`CapabilityBlockers` remains a residual-risk diagnostic and is non-zero for
every constructed engine. The legacy constructor reports every blocker. The
production composition clears strict official metadata validation plus the
externally supplied string/identity-attestation bits; the latter is caller
attestation, not package-verified VID/PID ownership. Guide and Share now have
their separate exact forms. Remaining bits continue to record unsupported
generic fragment reassembly/multi-context coalescing, receive replay policy,
status events, USB suspend/reset notification, Windows binding, and hardware
conformance. The explicit authenticated retained USB/IP factory deliberately
does not turn those verification flags into a compatibility claim.

Post-construction ordinary input and typed feedback claim/admit/resolve paths
are fixed-capacity and are regression-tested at zero allocations after warmup.
The dormant standalone and canonical persona-backed whole-packet owners are
likewise fixed-capacity and zero-allocation on their ordinary successful paths.
Descriptor, codec,
lifecycle, generation, retry, timeout, downstream classification, and atomic
participant results remain offline test evidence only.

## Production zero-delay Direct Motor programs (2026-09-02)

The authenticated production composition now wraps its canonical single-frame
executor with a bounded motor-program owner. Windows' captured Duration `FF`,
Delay `00`, Repeat `EB` is a valid finite repeat, not an unsupported frame:
zero-delay runs are contiguous for `Duration * 10 ms * (Repeat + 1)`.
Initial acceptance waits for the canonical consumer acknowledgement, not the
whole program duration. Subsequent effective states retain all four actuator
channels, use monotonically increasing publication sequences, and renew with
leases no longer than the authenticated ceiling (at most 250 ms). Each lease
is also capped against absolute expiry in the cross-process CFBK clock domain.
Expiry/cancellation is recoverable Neutral, not ownership-retiring Stop.

Replacement, output clear, persona-generation advance, reversible reset, and
terminal drain invalidate scheduled callbacks. Drains join any active
publication; callbacks queued before a drain cannot publish afterward. A late
acknowledgement, panic, unavailable clock, or failed renewal stops the program
and is reported to the retained owner's fatal path and broker session. This
is containment, not evidence that physical neutral has been delivered.

Nonzero Delay with active motor levels remains unsupported: the inspected
Microsoft table defines its unit/range but not the placement of that phase.
The pure CFBK encoder continues to reject delay/repeat programs; only the
production timing owner projects an established effective state into it.
Physical ACK waits can still delay a subsequent serialized host motor action;
they no longer hold ordinary input, Guide, or periodic status after final
feedback admission. No end-to-end latency claim follows from software tests.

## Independent ordinary feedback ownership (2026-09-02)

The retained coordinator now transfers admitted Direct Motor and Guide LED
obligations into one bounded engine-owned side slot. Transfer requires an exact
zero-length ordinary action without USB, lifecycle, metadata, sequence-pool,
clear, or host-packet ownership. It is ownership transfer, not successful
execution: the canonical feedback snapshot changes only after the real local
executor reports success.

Ordinary IN, Guide, periodic status, and their exact retries can progress while
that slot awaits feedback acceptance. Feedback non-delivery retains its own
immutable retry; either lane may retry first without changing the other lane's
claim, bytes, or sequence. EP0, further host OUT, and lifecycle work remain
serialized. ClearOutputs can never use this side slot. Admitted feedback blocks
reset/disconnect; the existing authoritative boundary may retire only its
unadmitted retry after executor cancellation/drain.

Delivered ordinary OUT wakes its already-selected local action without waiting
for another IN request. Detachment advances and signals readiness before
Execute starts. Exact failed local retries use the bounded local-worker wake,
not a new input polling loop. Cold-owner construction and reset/disconnect Safe
checks recognize both lanes. Software tests include blocked executor input,
simultaneous lane retries, direct detachment wake/exhaustion, lifecycle fencing,
forged completion rejection, and zero-allocation warmed paths. Windows timing
and physical feedback delivery have not been measured for this change.

## Deliberately absent

- a default USB VID/PID, a claimed Microsoft/manufacturer identity, USB
  registration, a backend descriptor callback, or a Windows binding claim
- default, generated, or inferred Manufacturer, Product, and Serial Number
  strings; the authorized factory accepts
  only exact external values
- audio descriptors and audio startup
- fabricated or resemblance-authorized metadata; the production factory uses
  only the strictly compiled and independently validated official ordinary
  gamepad Console Function Map variant
- a dedicated timer thread, a generic receive reassembler, generic fragmented headers,
  and canonical multi-claim authority for more than one lifecycle/ACK member
  in a coalesced downstream packet
- legacy/event-bearing Status bodies, deprecated IR LED commands, physical
  LED application, USB suspend/resume and production bus-reset notification,
  and actual production teardown/reset execution
- authentication or console security; the Windows-PC persona advertises the
  official USB security opt-out metadata interface and accepts only Windows'
  exact post-opt-out Security Data Complete marker
- player, battery, LED, audio/headset, expansion, Elite/paddle, and firmware
  messages
- UDE/UdeCx, XInputHID, XUSB, or another presentation backend in this phase;
  the authorized retained USB/IP composition is the current candidate
- a generic received-message duplicate/replay policy outside the typed
  reliable-ACK progress seam

Production selection remains blocked without a lawful caller-authorized
identity and exact strings. Release claims remain blocked on supported Windows
binding evidence, USB lifecycle coverage, representative compatibility tests,
and hardware conformance. The explicit authenticated USB/IP factory crosses
the former code-registration gap without claiming that those validation gates
have run.

See [PROVENANCE.md](PROVENANCE.md) for exact official sources, implemented
facts, and residual unknowns.

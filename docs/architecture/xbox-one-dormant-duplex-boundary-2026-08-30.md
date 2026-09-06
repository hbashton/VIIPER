# Xbox One dormant duplex boundary audit

Audit date: 2026-08-30.

Status: the semantic input contract, persona-local feedback translation, and
local feedback executor are offline-only. The Xbox One/Series raw persona
remains unregistered. This work does not establish a lawful device identity,
Windows enumeration or API visibility, hardware feedback, USB/IP conformance,
or latency parity.

## Audited vertical path

The closest coherent path already present across the two repositories is:

1. DS4Windows maps through its existing compatibility projection into the
   exact 24-byte `XboxOneEgressState` v1 value.
2. `XboxOneEgressScheduler` applies the existing ordered-egress policy.
3. A missing `ViiperOutDevice` Xbox One/Series route would have to carry that
   value into `DormantRetainedUSBAdapter.PublishSemanticInputWire`.
4. The retained adapter updates the existing persona semantic-input cell; the
   persona remains the sole owner of GIP message construction, sequence pools,
   lifecycle, and endpoint admission.
5. Host Direct Motor and clear actions leave the persona through the concrete
   dormant `ControllerPersonaCanonicalFeedbackExecutor` as exact 72-byte CFBK
   v1 values.
6. A still-missing synchronous broker publisher would deliver those values to
   DS4Windows' `XboxOneBrokerFeedbackIngress`, which validates the exact source,
   physical-device generation, transport generation, and ownership epoch and
   publishes into the existing `NativeGame` slot of the one
   `ControllerFeedbackRuntime`.

This is one mapping stack in each direction. The executor adds no mailbox,
arbitration policy, physical-controller translation, or second sequence
counter. `ControllerPersonaLocalExecution.Order` remains the CFBK source-local
sequence.

## Lifecycle correction implemented by this tranche

The persona uses the same `ClearOutputs` action for three different causes.
Treating every clear as CFBK `Stop` retired DS4Windows' ownership epoch during
recoverable configuration loss or USB reset, so a later four-actuator update in
the same physical lease could not resume. The boundary now requires explicit
authenticated intent:

- configuration loss and USB reset emit lease-retaining `Neutral`;
- only the separately authorized disconnect clear emits terminal `Stop`;
- Direct Motor is always a state update, with duration-zero cancellation also
  represented as `Neutral`.

For a reset, ordinary execution is first fenced and drained. The successor
generation's exact neutral is then published. Only synchronous acceptance of
that value rotates the private persona-generation binding and reopens ordinary
execution. A predecessor-generation motor action is rejected; a later
successor-generation four-actuator action is accepted without allocating a new
CFBK ownership epoch. Output-free `PerformReset` and `CompletePowerOff` actions
advance a separate authorized generation, and the binding is promoted only
when the first successor CFBK value is accepted.

For disconnect, terminal drain permanently fences ordinary execution. The
successor-generation disconnect clear publishes `Stop`; acceptance makes the
executor permanently stopped. A later state update cannot resurrect that
ownership epoch, including if it bypasses the executor and reaches the
canonical mailbox.

## Deadline, retry, and uncertainty contract

`ResetAndDrain` and `CancelAndDrain` are bounded waits, but timeout does not
discard lifecycle ownership. Their draining state and admission fence remain
in place. Repeating the same method resumes waiting for the exact in-flight
publication and transitions to drained only after it is joined. A concurrent
terminal drain may upgrade reset draining or reset neutralization; it never
reopens reset admission. If the reset neutral was accepted before the terminal
upgrade completes, its generation is retained so the following disconnect
`Stop` binds the exact successor.

The publisher interface is intentionally synchronous and strict:

- `nil` means the complete exact value was accepted before the supplied
  deadline;
- an error must prove that no byte was accepted and that no late publication
  can occur;
- after that proven rejection, the exact execution is retryable;
- a socket, pipe, or shared-memory implementation with an uncertain outcome
  cannot satisfy this interface directly. It must close and drain its exact
  session and quarantine the outer import before any successor generation is
  admitted.

This distinction prevents both duplicate terminal effects and the more serious
failure mode where a bounded wait returns while a late old-generation publish
can still escape.

## Independent implementation comparison

The following local reference trees were inspected at exact commits:

| Reference | Pin | What it establishes | Treatment |
| --- | --- | --- | --- |
| PadForge | `0794fd01bd19f4c096b982ffc824b88bce5ed743` | Its Xbox virtual slots call HIDMaestro `SubmitState` on the consumer cadence; changed frames are submitted immediately and only identical idle frames are deduplicated behind a 16 ms keepalive. Its virtual-device implementation is HIDMaestro, not USB/IP. | Audit only. PadForge is CC BY-NC-SA 4.0; no source expression was copied. |
| HIDMaestro | `9df50410230c11b410f43909ede0e5fc8b23d15b` | Input uses a shared-memory seqlock followed by named-event wakeup. Xbox XInput/WGI presentation uses a separate XUSB companion reading a 14-byte GIP-like state cell. Output uses an event-signalled ring. | Behavioral and negative-control evidence only. The companion depends on undocumented/reverse-engineered XUSB IOCTL layouts and WGI admission behavior; those are not a lawful raw Xbox USB persona or a source for invented Windows metadata. No source expression was copied. |
| Switch2Connect | `4487322a306f04efa27682e3f3a508635a84fd98` | Its Python wrapper exposes a four-value Xbox callback and maps main and impulse percentages, but device creation and Xbox presentation are inside bundled opaque `WinUHid` DLLs. | Corroboration only. The visible Python layer does not establish the driver/persona implementation, metadata, identity, or USB/IP behavior. No source expression was copied. |
| SDL upstream | `c71abd08605b8bb7078372307a93274725c99fe0` | Public APIs preserve body rumble and independent trigger-rumble channels. | API/capability corroboration only. |
| SDL fork | `d98c5804a9d20b0d96e993741797878c86b8f1e1` | The reviewed fork supplies comparison behavior used by PadForge. | Audit only; no source expression was copied. |

The comparison supports an architectural lesson, not a performance claim:
PadForge/HIDMaestro avoid the USB/IP presentation path for Xbox by publishing a
latest state into a same-host shared section and waking a dedicated Windows
device stack. That cannot be transplanted into VIIPER as an unverified raw
persona, and their published benchmark is not evidence for this dormant path.

## Remaining production blockers

The following gates remain closed:

- DS4Windows has no Xbox One/Series `OutContType`, profile/UI selection, or
  `ViiperOutDevice` route.
- VIIPER has no live registry entry or server route for the persona, and the
  dormancy regression must continue to pass.
- No broker transport yet implements the synchronous exact-acceptance/no-late-
  publication contract or constructs the immutable feedback binding from one
  authenticated physical-target lifetime.
- A lawful USB identity, manufacturer/product/serial policy, compiled metadata,
  Windows binding/API conformance, and a real wired-device positive control are
  still missing. Values must not be invented from PadForge, HIDMaestro,
  Switch2Connect, or opaque binaries.
- Guide status, Share/Console Function Map, Guide LED delivery, timed Direct
  Motor delay/repeat, bounded renewal for long effects, and complete hardware
  capability translation remain explicit gaps.
- No hardware test, Windows enumeration test, usbip-win2 test, or end-to-end
  latency measurement was performed by this tranche.

Until all of those gates have evidence, the raw persona must remain dormant and
no Xbox One/Series completeness or latency claim is justified.

## Focused executable evidence

`canonical_feedback_test.go` and `canonical_feedback_executor_test.go` cover:

- exact golden CFBK bytes and all four independent actuator channels;
- explicit intent mismatch rejection with atomic destination preservation;
- recoverable clear, reset neutral, private generation rebind, stale
  predecessor rejection, and successor four-actuator resumption;
- output-free reset authorization followed by first-successor binding
  promotion;
- monotonic CFBK order across a persona generation boundary;
- terminal drain, disconnect `Stop`, permanent executor finality, and mailbox
  non-resurrection;
- reset and terminal drain timeout with the exact wait resumed;
- terminal drain racing reset neutralization;
- proven publisher rejection with no accepted bytes, no mailbox advance, no
  late publication, and exact retry; and
- reentrant diagnostics and zero-allocation steady-state encoding/execution.

The repository-wide dormancy test separately asserts that common Xbox One and
Series device names remain unregistered.

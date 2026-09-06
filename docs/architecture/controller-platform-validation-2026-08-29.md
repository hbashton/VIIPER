# Controller platform validation status, 2026-08-29

This is an evidence/status snapshot for the in-progress DS4Windows + VIIPER
controller-platform work. It is not a release note and does not claim product
completion, Windows Xbox One/Series visibility, live Switch 2 support, latency
parity, or 500 Hz physical delivery.

The source-level audit and provenance detail remain in
[`controller-platform-audit-2026-08-29.md`](controller-platform-audit-2026-08-29.md)
and
[`controller-platform-provenance-ledger-2026-08-29.md`](controller-platform-provenance-ledger-2026-08-29.md).

## Repository state

| Repository | Branch | Baseline HEAD | State |
|---|---|---|---|
| DS4Windows | `feature/native-udecx-landing-zone` | `061fab1304e77c995ce9451b7ef20e51cc870070` | dirty; current platform work is uncommitted |
| VIIPER | `feature/native-udecx-landing-zone` | `54f7853aa4f97394298fc8f4c3ebb865237a3456` | dirty and ahead of origin; current platform work is uncommitted |

No commit, push, tag, release, installer, driver installation, association,
pairing, firmware operation, calibration-memory operation, or system setting
change was performed for this tranche. Existing user changes and dirty working
trees were preserved.

## Implemented and offline verified

### Ordered presentation foundations

- DS4Windows Xbox 360 and virtual Switch 2 egress use semantic journals,
  writer leases, final-admission fences, mandatory neutral/resync behavior,
  generation retirement, and deterministic overflow/reset behavior.
- VIIPER Xbox 360 and virtual Switch 2 use lease-bound
  `internal/inputpresentation.Source` scheduling through the existing USB/IP
  endpoint path. Exact HID-interface ownership, committed GET_REPORT
  snapshots, unsupported-ID stalls, configuration fences, and compat/raw
  owner handoff are covered by wire-level tests.
- These are virtual-output foundations. They are not physical Switch 2 input
  support and do not establish application-observed latency.

### Feedback ownership foundation

- One per-device `ControllerFeedbackStateLanePump` composes the typed feedback
  runtime, generation/sequence-fenced mailbox, physical translator, sole
  writer, and acknowledgement callback. Nested lanes publish values only; they
  do not create competing writers.
- Transport quiescence waits for an in-progress physical begin/cancel
  operation before retiring ownership. Neutral/stop remains a first-class
  state rather than an inferred zero-magnitude side effect.
- The foundation is unit verified but not instantiated by a live physical
  controller factory. It therefore proves neither real actuator delivery nor
  neutralization.

### Switch 2 canonical input foundation

- Exact USB/BLE protocol identity, device/transport generations, QPC
  completion timestamps, and strict existing parser reuse are implemented.
- The canonical value owns its 63-byte body and retains 12-bit sticks,
  counters, unknown fields, and opaque motion without early `DS4State`
  quantization.
- Calibration adoption is fail-closed and bound to device generation;
  transport-only reset cannot silently change a calibration snapshot.
- The pure Joy-Con reducer has explicit pair epochs, skew limits, stale-half
  behavior, generation-fenced loss/replacement, post-loss tombstones, and
  terminal split semantics.
- The exact Pro USB composite-admission boundary validates HID MI_00 and WinUSB
  command MI_01 topology. A dormant Windows adapter now groups complete
  SetupAPI snapshots by opaque container, reserves each admitted container
  process-wide before revalidation, opens only read-only MI_00 plus MI_01
  presence, and retains the reservation until both resources terminally
  dispose. Distinct containers coexist; ambiguous or unattributable snapshots
  fail closed.
- A separate dormant USB pump owns one transport, blocks on exact overlapped-
  read retirement, and rearms without a timer, sleep, or polling cadence. Its
  owner/pump fence prevents a second manual reader. It is not connected to the
  DS4Windows runtime or legacy enumerator.
- The existing DS4Windows profile pipeline now has one Common05 projection for
  the exact Pro USB and BLE identities, plus source-pinned joined and
  standalone-horizontal Joy-Con 2 projections. Raw 12-bit axes, independent
  half lifetimes, the C button, and four rail/paddle controls survive the
  legacy `DS4State` compatibility copy as explicit sidecars.
- A bounded BLE discovery-candidate registry uses per-scan keyed pseudonyms,
  exact generation fences, explicit scan retirement, stale/conflict ordering,
  wake rejection, capacity limits, and foreign-host quarantine behavior. A
  current-scan `RememberedThisHost` observation may issue one opaque admission,
  and copied admissions share one atomic single-use reservation.
- A dormant BLE input owner validates exactly one Nintendo service and one
  Common05 characteristic with properties exactly `Read | Notify`, subscribes
  only to CCCD Notify, copies exact 63-byte notifications into bounded
  non-overwriting storage, and feeds the existing canonical session. Overflow,
  disconnect, sink failure, and stop retire the generation before unsubscribe;
  a selected publication completes before its one clear/half-loss callback.
  No sink callback runs under the queue/session lock.
- A dormant no-HID DS4Windows runtime boundary now represents Pro USB/BLE,
  standalone Joy-Con 2 L/R, and an explicitly paired joined Joy-Con set without
  inserting any of them into `DS4Devices`. Runtime types are append-only and
  rejected by the legacy HID factory. Exact device/transport generations,
  model, transport, protocol revision, mode, and pair epoch are revalidated at
  the one existing `DS4State`/`Report` mapping seam. C and rail controls remain
  in the same-report sidecars. Report callbacks run outside ownership locks,
  one bad subscriber cannot suppress later subscribers, and one terminal
  neutral has separate accepted-pending, completed, and delivered states.
  Reentrant or timed profile actions are queued and drained rather than
  silently discarded. The immutable registration value authenticates an exact
  owner/reference/generation and normalizes hostile owner failures closed.
- There is still no ControlService registration table, PID `2069` runtime
  registration, production HID worker, WinRT
  watcher/GATT lease, association/reconnect owner, controller-memory
  calibration acquisition, profile UI for C/rail controls, or live runtime
  pair coordinator.

### Xbox One/Series protocol foundation

- Strict headers, base input, Direct Motor, descriptor, Hello, event-free
  Extended Status, opaque metadata framing, authentication seam, and sequence
  allocation are implemented as an isolated, unregistered package.
- Metadata owns its blob, performs final `AdmitAndCopy`, retries exact private
  images, re-evaluates time-dependent ACME at admission, and models requested
  and unsolicited reliable-transfer progress/gap ACK semantics through the
  exact official nine-byte Protocol Control body decoder.
- ACK processing is fenced by transport/transfer generation, epoch, message,
  sequence, and progress. Rewind invalidates pending work; non-rewind progress
  rebases selections and retries.
- Lifecycle transitions expose one external action at a time. An admitted
  action is an exclusive execution lease; USB reset must wait for terminal
  resolution or synchronous cancel/drain. Metadata transfer generations are
  burned before exposure and never reused across reset.
- Valid admitted delivery resolution does not fail after the external effect;
  completion clocks clamp and deadlines saturate.
- `ControllerPersonaEngine` now composes EP0, lifecycle, metadata/ACK, global
  and gamepad sequence pools, exact input emission, Direct Motor, Guide LED,
  retry/cancel, USB configuration and endpoint-Halt gating, reset, disconnect,
  reconnect, and one non-reused clear epoch behind one generation-fenced
  claim/admit/copy/resolve lane. Prevalidation prevents partial ownership when
  nested claims fail.
- The downstream classifier admits only exact Direct Motor and Guide LED
  controller messages. Ordinary input and feedback paths are allocation-free
  in the offline tests.
- A package-local, dormant transport coordinator now serializes copied EP0,
  interrupt-IN, and interrupt-OUT tickets against the one persona engine.
  Selection occurs only at final response ownership; mandatory START input is
  sampled there rather than when the URB first waits. Local effects consume
  only already-selected zero-length claims, failed host-output delivery fences
  every lane until an authoritative generation boundary, and USB reset
  atomically invalidates every staged old-generation ticket.
- This package is not registered, wired to a backend, or validated through
  XInput, GameInput, WGI, SDL, Steam, UWP, a game, USB/IP, or hardware.
- Generic EP0 transaction, prepared interrupt-IN late-copy, transactional
  interrupt-OUT, and input-presentation production paths still do not compose
  into the persona's whole-device transaction contract. Their result/size
  decisions remain on separate legacy paths and only IN workers have existing
  unlink/reset retry ownership. Two dormant generic prerequisites now exist:
  the bounded three-lane retained EP0/IN/OUT arbiter with final serialized
  Data/ZLP/success/STALL selection, and a separate nonwrapping exact-import
  authority. The latter now uses an opaque Reserve -> exclusive Building ->
  Prepared scheduler -> exact Bind -> bounded one-shot activation -> Claimed/
  gate-open transaction on one combined owner object. Enqueue and the terminal
  admission fence share the scheduler mutex; close then orders ingress close,
  arbiter join, local drain, disconnect-neutral, and release. Bind or activation
  error/panic/timeout, malformed terminal result, cleanup ambiguity, or failed
  neutral quarantines the exact device/session. Scripted tests plus real
  pending/terminal arbiter composition prove abort/build exclusion, late-
  activation fencing, retirement/completion, bound-owner cleanup, and no
  premature release. Neither constructor has a production call site; no
  registry, import, device identity, or production route uses these seams.
  The exact generic seams are recorded in
  [`xbox-one-usbip-adapter-audit-2026-08-29.md`](xbox-one-usbip-adapter-audit-2026-08-29.md).
  A registry regression keeps `xboxone`/`xbox-series` names unregistered until
  those seams and the external identity gate are satisfied.

## Hardware-observed status

Earlier passive read-only captures established one Switch 2 Pro USB device as
`057E:2069`, `bcdDevice 0x0201`, with 64-byte report `0x05` input and host read
completion cadence near four milliseconds. Those captures validate only the
observed Pro USB input framing/cadence, not calibration, output, interrupt
rate, transport latency, or alternate modes.

After 59 offline verifier tests, an independent adversarial review, and a
warnings-as-errors Release build, one user-authorized schema-4 run
re-established the exact target but failed the mandatory exclusive MI_00
writer lease with `HidReadOpenFailed` / Win32 error 32 (sharing violation). It
stopped before opening the command interface and before every battery, LED,
input-capture, haptic, or cleanup mutation. No controller state was changed.
The result is bound to the exact procedure, assembly, and executable hashes:

- file:
  `_results/switch2-usb-hardware-2026-08-29-schema4-sole-writer-v2.json`
- result SHA-256:
  `0DFC1F5240FF3BBBFD9DD8CC0F8689039981EEEA3EF9856E9E626A099E7F8F44`
- verifier DLL SHA-256:
  `87D2A053DD6FF7A447D71E27B13D2715B188547E8A33E3816EF95164184F4A4E`
- verifier EXE SHA-256:
  `B68998F98B5FA3A95E9050A0F055456169E16E6B0D678F29C1D4A2044786AA2C`

The owning process was not attributable from the available read-only evidence.
Several controller-related processes were present, but presence does not prove
handle ownership. No process was named as the owner and no application or
service was stopped.

The verifier's live nonzero-haptic gate remains fixed closed. Host HID write
completion cannot prove physical actuator neutralization after a
noncooperative kernel write, and no admitted device watchdog or independent
physical stop measurement exists. The schema-4 run made zero haptic attempts,
zero haptic writes, and zero neutral writes.

## Validation commands and results

### DS4Windows

```text
dotnet test .\DS4WindowsTests\DS4WindowsTests.csproj -c Release --no-restore -p:Platform=x64 --verbosity minimal
1315 passed, 3 pre-existing opt-in live-audio tests skipped, 0 failed

dotnet build .\DS4WindowsWPF.sln -c Release -p:Platform=x64 --no-restore --nologo
build succeeded, 6 pre-existing unrelated warnings, 0 errors

dotnet test .\utils\Switch2UsbHardwareVerify.Tests\Switch2UsbHardwareVerify.Tests.csproj -c Release -p:Platform=x64 --no-restore --nologo
59 passed, 0 skipped, 0 failed

dotnet build .\utils\Switch2UsbHardwareVerify\Switch2UsbHardwareVerify.csproj -c Release -p:Platform=x64 --no-restore --nologo -warnaserror
build succeeded, 0 warnings, 0 errors

git diff --check
passed; line-ending conversion notices only
```

The Switch 2 focused set contains 236 passing tests. This includes 63 focused
Pro USB owner/pump and Windows-adapter tests. In addition to canonical raw
ownership, 12-bit preservation, calibration, replay, and Joy-Con pair fences,
the tests cover per-container reservation, single-use BLE admission, bounded
non-overwriting BLE ingress, callback reentrancy, exact native retirement,
cancel/retire/reuse ABA fences, asynchronous no-premature-rearm behavior, and
zero-allocation steady-state paths. The 63 USB tests also passed 20 repeated
no-build runs (1,260 executions).
The new no-HID runtime/registration class contains 10 focused adversarial tests;
an independent run passed it 10 consecutive times after the first focused pass.

### VIIPER

The workspace-pinned Go 1.27.0 toolchain and w64devkit 2.9.1 compiler were used
with `CGO_ENABLED=1`; no toolchain was installed.

```text
go test -count=1 ./...
passed

go vet ./...
passed

go test ./device/xboxone -run ControllerPersonaTransport -count=50 -shuffle=on
passed

go test -race ./device/xboxone -count=20 -shuffle=on
passed
```

The Xbox One package also passed the final coordinator-focused tests repeated
50 times, its complete package under the race detector 20 times, earlier
corrected lifecycle/metadata boundary scenarios repeated 200 times, five fuzz
targets, and independent adversarial P1/P2 re-review. The earlier corrected
package coverage run reported 85.9%.

`git diff --check` is clean for the platform tranche. The full command still
reports pre-existing trailing spaces in `docs/cli/configuration.md:70` and
`docs/cli/server.md:47`; those unrelated user lines were not changed.

## Source reuse and provenance

- PadForge stable and `v4-dev` were audited as separate CC BY-NC-SA behavioral
  references. No PadForge source expression was copied.
- Switch2Connect was used for corroborating facts where independently pinned;
  inherited/opaque components with unclear provenance were not copied or
  redistributed.
- HIDMaestro's authored code is MIT and was audited as a distinct architecture,
  but this tranche did not need to import its implementation.
- Licensed SDL and official Microsoft protocol/documentation facts are the
  preferred implementation evidence. The provenance ledger records exact
  component boundaries and protocol-facts-only sources.

## Designed or still blocked

- Xbox One/Series persona registration: blocked by lawful project identity,
  exact strings/semantic metadata compiler, event/security behavior, transfer
  reassembly/coalescing and replay policy, Guide/Share/status input, Windows
  binding/API visibility, physical neutralization on suspend/configuration
  loss, the unified late USB transaction/job boundary described above, and an
  actual USB/IP vertical slice. The offline persona/lifecycle engine and its
  transport coordinator are not a registered `usb.Device` and have no default
  identity.
- Physical Switch 2 Pro USB/BLE and Joy-Con 2 L/R: canonical processing,
  exact profile projections, a dormant concrete Windows USB discovery/read
  adapter and completion-driven pump, and a dormant transport-neutral BLE
  admission/notification owner exist. Production registration, atomic legacy
  HID handoff, a concrete WinRT watcher/GATT lease, association/reconnect,
  calibration acquisition, profile UI/runtime removal binding, and feedback
  writers do not.
- Switch 2 HD rumble: raw codecs are offline verified; semantic translation,
  BLE envelope, cadence, thermal/stop policy, sole writer, and physical
  measurement remain blocked.
- Four-actuator routing: canonical values, mailbox/arbitration, and sole-writer
  foundations exist, but no live physical factory instantiates that owner and
  DualSense/Switch 2/generic physical translation remains unwired.
- XInputHID and raw GIP presentation remain decision/vertical-slice work. No
  native UdeCx driver was implemented, installed, packaged, or authorized.
- Windows 10/11, multiple Bluetooth radios, Joy-Con hardware, real Xbox
  references, DualSense/Edge feedback, generic ERM, game/API, installer, and
  long-soak matrices remain open.

## Latency and rate conclusion

No new application-visible, physical-to-consumer, publication-to-USB/IP, or
feedback-onset latency run was completed. There is no same-hardware PadForge /
HIDMaestro non-inferiority result and no parity or lower-latency claim.

PadForge's named 500 Hz setting is a two-millisecond host-loop target, not
proof of physical Switch 2 delivery. The strongest current physical evidence
remains approximately 250 reports/second (roughly four milliseconds) for the
observed Switch 2 Pro modes. A 500 Hz physical mode remains a controlled
experiment, not an established capability.

## Publication and rollback boundary

Do not push the current branches. Unpushed DS4Windows commit `942b3e6` and
VIIPER commit `3af2ce8` retain linkable evidence in recoverable history. The
audit requires a backup, a rewrite limited to the unpushed ranges, `git log
--all -S` privacy searches, and independent review of the exact outbound set.
That history operation must not begin while these dirty worktrees contain
unresolved work.

Because current changes are uncommitted and interleaved with preserved user
work, rollback must be file-scoped and preceded by a recoverable patch/backup.
Do not use a repository-wide reset or checkout to remove this tranche.

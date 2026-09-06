# Controller platform validation status, 2026-08-31

This is a dated evidence snapshot for the in-progress DS4Windows + VIIPER
Xbox One/Series output and physical Switch 2 work. It is not a release note.
It does not claim a production-registered Xbox One/Series persona, live
Switch 2 product support, 500 Hz delivery, latency parity, or hardware-verified
haptics.

The source audit and provenance ledger remain in
[controller-platform-audit-2026-08-29.md](controller-platform-audit-2026-08-29.md)
and
[controller-platform-provenance-ledger-2026-08-29.md](controller-platform-provenance-ledger-2026-08-29.md).
The preceding evidence snapshot is
[controller-platform-validation-2026-08-30.md](controller-platform-validation-2026-08-30.md).

## Repository and mutation boundary

| Repository | Branch | HEAD | State |
|---|---|---|---|
| DS4Windows | `feature/native-udecx-landing-zone` | `061fab1304e77c995ce9451b7ef20e51cc870070` | dirty; platform work and preserved user work are uncommitted |
| VIIPER | `feature/native-udecx-landing-zone` | `54f7853aa4f97394298fc8f4c3ebb865237a3456` | dirty; platform work and preserved user work are uncommitted |

No commit, reset, push, tag, release, installer, signing change, driver
installation, Bluetooth association change, firmware operation, or production
registration was performed in this tranche. Existing dirty-tree work was
preserved. Authorized hardware execution used only
`C:\Users\hbash\Desktop\Controller-Platform-Portable-Lab-2026-08-31`;
the installed DS4Windows and VIIPER under Program Files were neither removed
nor replaced.

The current implementation path remains VIIPER over usbip-win2 0.9.7.7.
Native UdeCx work remains an audit-only future backend seam; it was not ported,
completed, installed, packaged, or productized.

## Implemented and replay/offline verified

### Shared presentation and retained USB/IP foundations

The existing transport-neutral presentation scheduler and retained USB/IP
submission/session work now provide fixed-capacity ownership for input,
control, interrupt output, lifecycle, reset, unlink, and terminal drain. The
hot paths use claim/admit/commit or cancel/retire rather than optimistic
delivery. Generation, session, ticket, endpoint, and owner identities are
validated before effects; ambiguous failures quarantine instead of replaying
possibly committed effects.

This foundation is used by the current Xbox 360 and Switch 2 semantic paths and
by the dormant Xbox One persona work. It does not itself make the Xbox One
persona registrable.

### Switch 2 Pro USB owned full-duplex composition

DS4Windows has a dormant exact-composition boundary for one Switch 2 Pro USB
session:

- one physical/session identity and one MI_00/MI_01 lifetime;
- one MI_00 input reader and one narrow adopted output lease;
- one startup transaction owner, one input projection/read pump, and one
  canonical physical output writer;
- exact registration, activation, abort, retirement, and cleanup records;
- no second mapping or feedback stack.

The output lease can be adopted only once while the output lane is untouched.
Full aliases are rejected after adoption. Construction and activation failures
retain the exact inverse needed for cleanup rather than manufacturing success.
The production `ControlService` registration remains dormant until the
remaining hardware/protocol gates are met.

### Switch 2 ControlService exact slot prerequisites

The dormant ControlService slot host now owns these mutations as one retained,
reverse-order transaction:

1. the exact `DS4Controllers[slot]` occupant and runtime slot number;
2. all three `ControllerSlotManager` indexes; and
3. the exact `touchPad[slot]` `Mouse` instance required by the existing mapping
   pipeline.

The Mouse is constructed before the array mutation. Dispatch rejects a missing
touch object. Cleanup removes only the retained instance while the exact table
epoch and runtime device still occupy the slot, so it cannot clear a newer
controller's object. Remaining profile globals, hook delegates, virtual-output
creation, and device effect/audio options still require exact stage/inverse
facets before the host may be constructed by production ControlService.

### Switch 2 Pro USB HD-rumble bridge and feedback lifetime

The owned USB composite has a bounded bridge from the canonical feedback lane
to the one physical HD-rumble writer. It preserves exact epoch and retained
operation identity, serializes nonzero and neutral work through one path, and
does not let a stale producer bypass retirement.

Terminal retirement:

1. seals producers;
2. drains any retained operation;
3. submits one canonical nonzero-epoch Stop through the same writer;
4. retries the identical Stop bytes only when the retained outcome permits it;
5. retires the writer and proves exact quiescence.

A pre-commit abort does not invent a Stop. A retained Stop is drained before a
byte-identical retry. These are offline ownership and codec results, not
physical actuator-onset or actuator-stop evidence.

Focused activation review passed 139 tests. The combined Switch 2 and feedback
suite passed 779 tests. The complete DS4Windows suite and build are recorded
below.

### Switch 2 USB private startup-evidence mode

`Switch2UsbHardwareVerify` now has an explicit laboratory-only mode:

```text
--capture-startup-evidence --output <new-file.json>
```

It is not reachable through normal DS4Windows registration. On one exclusive
MI_01 command handle it fixes this single-flight order:

```text
03:03 -> 0C:02/0x27 -> 0C:04/0x27 -> 03:0A
```

Each step is one host-side flush, exact write, and one bounded packet read.
The authorized 2026-08-31 run established these exact bcdDevice `0x0201`
feature responses:

```text
0C:02 -> 0C01000200F8000000000000
0C:04 -> 0C01000400F8000000000000
```

`Switch2UsbCommandCodec.TryValidateFeatureResponse` now admits only those
step-specific 12-byte tuples. Exhaustive single-byte mutation tests reject
every changed byte, cross-step substitution fails, and the production Windows
startup lease now performs bounded I/O for all four steps. A malformed or
ambiguous feature completion fences replay until retirement. This validation
is pinned to `057E:2069`, bcdDevice `0x0201` and is not generalized to another
firmware or transport. The wire protocol still has no transaction identifier.

Player LED 1 arms `AllOff` cleanup before I/O. The mode never opens a HID
output handle and cannot emit nonzero or zero-amplitude haptic reports. Opaque
feature bytes are no longer admitted: the closed artifact schema independently
revalidates the exact tuples through the production codec. The artifact remains
local and ineligible for automatic commit/share. No source-backed inverse
exists for the two feature commands; their session state may remain until
another owner reconfigures the device or it disconnects.

Offline validation:

```text
focused utility tests: 75 passed, 0 failed
Release x64 utility build: 0 warnings, 0 errors
```

The built verifier DLL SHA-256 is
`C5F63A56E4647F5A91BD42719B34940735B36714CCB80F266413A39C88B87EFA`.
The executable SHA-256 is
`B68998F98B5FA3A95E9050A0F055456169E16E6B0D678F29C1D4A2044786AA2C`.

Independent review found and the implementation corrected a closed-schema
bug: undefined numeric enum values and inconsistent outer/procedure failure
codes were accepted by an earlier validator. Runtime and validator now share
one outcome policy, every serialized enum domain is closed, and cleanup versus
procedure failure relationships are exact. The reviewer replayed both original
reflection attacks against the frozen DLL; both were rejected while the
canonical control was accepted.

### Exact externally authorized Xbox USB identity

The dormant Xbox persona no longer invents manufacturer, product, or serial
strings. It requires caller-supplied exact strings and a one-shot external
authorization attestation. That attestation is not a VIIPER determination of
USB VID/PID ownership, trademark rights, Windows binding, or legal authority.

The serial is exactly 32 hexadecimal digits and contains the fixed-width
64-bit device identity. UTF-8 source and UTF-16LE string descriptors are
strictly bounded and encoded. Descriptor indices, language, truncation, and
the maximum legal 254-byte USB string descriptor are tested. Metadata and
profile issuances, identity, descriptors, engine, and EP0 control-plane
pointers are bound to one authorization owner. Copied capabilities have one
winner and cannot replay after consumption or quarantine.

This identity work remains dormant and unregistered.

### Xbox retained EP0 response capacity

The retained adapter now owns a 254-byte control-response scratch capacity,
separate from the fixed request slabs and the 64-byte interrupt lanes.
Host `wLength` and USB/IP transfer length may lawfully be 255 or 65535 while
the device returns the actual 254-byte descriptor; the adapter no longer
rejects those host maxima merely because its response is shorter.

Tests cover:

- exact 254-byte response through direct owner and retained server paths;
- host lengths 255 and 65535 returning 254 bytes;
- odd smaller host length 17 truncating to 17 bytes;
- setup/USB-IP transfer-length mismatch rejection;
- zero warm-path allocations at host length 65535; and
- unchanged request-slot and interrupt-lane geometry.

The independently reviewed final adapter SHA-256 for this capacity change is
`0852031454EF6A31730BEBE171C3FBB71053F36B85B9EF733D2A90EDFF6AC076`.

### Xbox downstream and replay policy

The dormant Xbox downstream path parses complete packet vectors before effects
and uses one bounded executor for the supported vector. Direct Motor and Guide
LED members preserve packet order. Mixed lifecycle/ACK-governed packets remain
fail-closed until one canonical multi-claim authority can cover the whole
packet; prefix commit is forbidden.

No generic GIP sequence replay cache was added. The audited USB path is not a
generic sequenced transport: lawful metadata retransmission may repeat the
same sequence, typed ACK state owns its own progress, and duplicate Direct
Motor commands may intentionally restart effect timing. Any future transport
with different replay semantics requires its own evidence-backed policy.

### Dormant authorized Xbox retained-adapter composition

A one-shot factory now consumes the exact external identity authorization
internally, constructs the canonical persona engine, transfers it to the one
canonical retained adapter, and returns no public engine alias. It has no
ambient production call site; device construction starts no device worker or
I/O. Successful explicit server registration owns one cold lifecycle watcher
solely to reconcile the still-exported lower VirtualBus teardown surface.

Independent adversarial review rejected successive candidates and drove four
material corrections:

- stateful executors must be non-nil pointers to nonzero-sized pointees;
  value, typed-nil, runtime-uncomparable, and zero-size-pointer shapes are
  rejected before authorization consumption;
- the engine seal snapshots binding/descriptors by value, issuance pointers by
  identity, metadata bytes independently, and exact slice backing/bounds;
- direct exported adapter construction neither requests nor retains a proof;
  only the authorized composition receives a separate one-shot proof from the
  canonical private implementation; and
- any post-engine contradiction unconditionally invalidates the known exact
  engine/control plane/binding and quarantines the authorization without
  following a mutable substituted pointer.

The final independent review replayed descriptor substitution, in-place
metadata mutation plus digest rewrite, equal-byte foreign backing substitution,
zero-size pointer address collapse, clock/lifecycle split, binding/owner
corruption, proof replay, and panic. All failed closed or quarantined as
required. Focused tests passed 100 repeats; focused race tests passed 10 repeats;
vet passed. Final SHA-256 values:

- composition: `245C4E86DDE42D5267549006CB1C48D57CDFA9032D1D63AD216FC1CE009DC88E`;
- composition tests: `2D8E470C36511CDB40CBF59D1276E4C9B8505E25F958241E0C5546E1E34D5D8D`;
- retained adapter: `A25C15D81EF970F0568BCE962C32610D24653E952797CC1CF31FC93C59FCCB2C`;
- retained tests: `1B450F4B711E2B6C1A0C24A00F5DDBE9E8C3411758D744DCF15110B01D0B8FB9`;

The source hashes above reflect the later explicit-factory call-site guard and
the current retained adapter source. The living validation document does not
embed a self-invalidating hash of itself.

2026-09-01 update: the seam is now routed by the production USB/IP reader only
for explicit retained devices under a nonzero authority. It still does not
register/create the Xbox product, prove Windows binding/API visibility, or
establish end-to-end latency.

The routed seam now also seals one descriptor across discovery, validation,
and lifecycle routing; holds exact per-device/per-owner callback admission
through teardown; attributes joined transport versus authoritative teardown
errors independently; cleans strong admission references on hotplug removal;
and resolves pipelined EP0 lifecycle requests at FIFO service time against the
delivered predecessor state. Full Go tests and vet, focused race tests, and 20
single-thread scheduler/transport repetitions pass. No controller or installed
program was touched for this update.

The subsequent adversarial pass made that seal allocation-bounded by rejecting
non-fixed retained topology before copying and retaining only scalar fields.
It also joined retained DEVLIST callbacks to shutdown, routed API/whole-bus
removal through exact admission cleanup, fenced `Add` against bus closure,
made explicit removal race safely with one-shot disconnect, made failed UNLINK
retirement terminal before teardown can neutralize or release, and atomically
published owner claim plus device association against cleanup.

The final lifecycle/framing pass assigned every virtual-bus registration and
empty-bus period an exact incarnation, including same-pointer re-add; carried
that identity through API stream ownership and timer callbacks; removed
terminal coordinator state; and contained callback panics. It latched
descriptor, limits, identity, and quarantine proof for the required server
lifetime, joined direct retained callbacks to `Server.Close`, and preserved
complete joined connection-close failures. It also rejected unrepresentable or
wrong USB/IP `devid` values before callbacks/body reads, reserved command
sequences at fixed-header arrival, and enforced speed-specific fixed endpoint
topology. Deterministic tests cover stale fired timers after exact key reuse,
header-only duplicate and wrong-device commands, owner-identity contention,
same-pointer one-shot re-registration on both the same and a replacement bus,
and actual loopback TCP import/shutdown/removal.

An independent source review signed off with no remaining correctness or
security blocker in this retained seam after focused coordinator repetitions,
framing repetitions, race-enabled USB/API/virtualbus/retainedusb tests, vet,
and the focused package suite. A separate repeated API race run exposed two
tests which failed to release fixed bus IDs between `-count` iterations; their
cleanup now removes the exact buses before closing the servers, and ten full
API package repetitions pass under the race detector. These are offline
software results only; no controller or installed program was touched.

The follow-on explicit Xbox factory pass added non-consuming device-address and
registration-token capacity preflight, repeated by a bounded atomic provisional
Add. Provisional devices remain wholly undiscoverable while descriptor
admission runs and publish only after exact server/bus/authority revalidation.
Generic Add now rejects typed-nil and dynamically non-comparable identities
before mutation. A private owning-bus-bound lifecycle capability makes warm
semantic-operation acquisition O(1) and zero-allocation in the focused test;
two-phase removal waits outside bus/server map locks, and exact removal,
broker Close, concurrent bus Close, and duplicate server RemoveBus join the
same marked operation drain through context cancellation. Server RemoveBus
also scans/marks retained admissions before duplicate server removers return;
direct lower-bus Close uses the watcher/lazy reconciliation below. Focused
normal, vet, and ten-repeat race suites cover capacity saturation without
authorization consumption, token exhaustion, provisional visibility,
cross-bus forgery, same-pointer re-add, operation-time status reentry, and
teardown joins. Direct lower-bus removal/close is context-linked back to exact
retained-admission retirement, with synchronous stale-admission reconciliation
for immediate same-pointer re-add and exact cleanup after a concurrent server
bus removal. These are correctness and allocation results, not an end-to-end
latency measurement.

The broker handle's mutable publication/close state is shared behind a private
pointer, so an ordinary pre-use value copy cannot create a second close owner.
Copied concurrent Close calls join the same exact removal and cached result.

Final independent adversarial review found no remaining correctness, security,
or capacity blocker in this retained-registration tranche. It independently
reproduced the former copied-handle, direct-teardown, early-watcher,
marked-predecessor, preclosed-RemoveBus, and successor-identity failures against
their regressions. Single-processor targeted stress passed 100 repetitions;
focused race suites passed 10 repetitions (plus an independent three-repeat
race pass); full normal, vet, and race-enabled repository suites pass. Frozen
source SHA-256 values are `EBE6A974BADD782AAE0C3E166F6904008151BAE5FBECB8621B2AE78E7DA6B852`
for `virtualbus.go`, `DE9EE7F5368D9D49F0660FE1BF6C0EAE3C10D96F36CA295DF654326197C82EE3`
for the USB server, and `8F5C8C863EEB34D8474B1ACDE67269C0AC68BF2133CE8CC065060994C8B39394`
for the explicit Xbox registry factory. No hardware or installed program was
touched.

## Combined software validation

Commands were run from the dirty trees without commit or installation.

```text
dotnet test DS4WindowsTests/DS4WindowsTests.csproj -c Release -p:Platform=x64
  1,894 passed, 3 existing live-audio skips, 0 failed

dotnet build DS4Windows/DS4WinWPF.csproj -c Release -p:Platform=x64
  0 warnings, 0 errors

dotnet test utils/Switch2UsbHardwareVerify.Tests/... -c Release -p:Platform=x64
  75 passed, 0 failed

dotnet build utils/Switch2UsbHardwareVerify/... -c Release -p:Platform=x64
  0 warnings, 0 errors

go test -count=1 ./...
  all packages passed

go vet ./...
  passed

CGO_ENABLED=1 go test -race -count=1 ./...
  all packages passed
```

The Go race run used the repository-local w64devkit GCC rather than relying on
the process PATH.

## Hardware evidence and current gate

The connected target is a Nintendo Switch 2 Pro controller over USB,
`057E:2069`, `bcdDevice 0x0201`.

The final authorized portable result is
`switch2-usb-startup-validated-v2-2026-08-31.json`, SHA-256
`9FECCE4A9A53BFEFD4AD42967BD1606462162FD958439E5980C615C7F94F9824`.
It records all six exact command validators accepted: the four startup steps,
Player LED 1, and `AllOff`. It also records 256/256 exact Common05 input reports
at 250.0002696 host completions/second, 3.9999957 ms mean interval, 4.0175 ms
p95, 4.0457 ms p99, and all 255 forward counter deltas exactly `+4`.
Player LED cleanup completed exactly. No HID output handle was opened and zero
haptic writes were attempted.

The first current-verifier run failed read-only MI_00 acquisition with Win32
error 32. The utility had requested read access while advertising only
`FILE_SHARE_READ`; an already open write-capable handle therefore made the
open incompatible even if that holder shared. The verifier-only input channel
now advertises `FILE_SHARE_READ | FILE_SHARE_WRITE` while still requesting no
write access. MI_01 remains exclusive. This corrected a passive-reader sharing
policy; it does not weaken or prove the production full-duplex output lease.

During the portable run, DS4Windows and VIIPER processes were stopped and the
Program Files installation was left unchanged. The successful MI_00 handle was
read-only and share-compatible, so it does not prove the production requirement
to own one MI_00 reader plus the sole physical output writer. Nonzero haptics
remain disabled because there is still no independent onset/neutralization
measurement or proven bounded physical stop after a noncooperative write.

## Designed, dormant, or blocked

- **Xbox production presentation:** the persona is transport-routed behind an
  explicit opt-in and has one non-generic internal registration factory, but
  it remains absent from generic API creation. Windows enumeration,
  `xinputhid`/XInput/GameInput/WGI
  visibility, representative games, and four-actuator feedback remain
  unverified.
- **Xbox composition:** the ownership seam is independently validated and has
  an explicit retained USB/IP call site. The exact default-off factory and
  semantic-input-only broker handle are simulation-verified, including hidden
  descriptor admission, bounded address/token capacity, O(1) exact operation
  authentication, and joining teardown; public product exposure remains
  intentionally absent.
- **Xbox mixed packet authority:** lifecycle/ACK mixtures still need one
  canonical whole-packet claim/batch owner.
- **Switch 2 USB startup:** the exact pinned feature response tuples are now
  validated in production code. A source-backed inverse for the volatile
  feature-session configuration remains unknown.
- **Switch 2 production registration:** the USB composite, registration
  participant, ControlService slot/mouse prerequisites, and feedback lifetime
  are dormant. Remaining profile/hooks/output facets and the production
  full-duplex MI_00 ownership/teardown proof are not complete.
- **Switch 2 BLE and Joy-Con 2:** discovery, input, pairing/association, joined
  ownership, and codec foundations exist in replay-tested form, but the
  mandatory radio/firmware/hardware matrix has not run and output remains
  experimental.
- **Physical haptics:** no accelerometer/contact-mic/oscilloscope evidence exists
  for onset or stop. Nonzero Switch 2 hardware output remains closed.
- **Latency:** no same-span, same-machine live USB/IP Xbox result and no
  PadForge/HIDMaestro non-inferiority experiment exist. No parity, superiority,
  500 Hz, or end-to-end latency claim is justified.
- **Native backend:** no USB/IP correctness/latency gate has yet authorized a
  separate driver project.

## Residual-risk rule

Offline tests establish byte, state-machine, ownership, and failure behavior
only for the modeled inputs. They do not establish Windows driver binding,
consumer API visibility, physical transport timing, actuator behavior, radio
security, firmware breadth, or competitor parity. Unknown packets and
unsupported transports remain fail-closed; evidence gaps are not filled by
README claims or inferred packet lengths.

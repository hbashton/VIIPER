# Native presentation decision package, 2026-08-29

Decision: do not implement, merge, install, or package the native driver in the
current Xbox/Switch 2 task. Preserve ABI 1.14 and the current
`inputpresentation.Source` seam so a separately authorized backend can be
built without creating another mapping or feedback stack.

This decision changes only if the exact usbip-win2 0.9.7.7 path fails a
predeclared compatibility or latency gate and the user authorizes a new driver
phase. Authentication or Xbox persona failure is not automatically a USB/IP
latency failure.

## Compared architectures

| Candidate | What Windows sees | Strength | Unresolved cost/risk | Current decision |
|---|---|---|---|---|
| VIIPER USB persona through usbip-win2 0.9.7.7 | genuine virtual USB tree through the pinned UDE/UdeCx importer | already installed, signed, composite Sony topology proven, one production pipeline | USB endpoint/loopback phase; Xbox persona/auth and exact latency unproven | first production candidate |
| VIIPER-owned native UdeCx backend | genuine virtual USB tree from a purpose-built UDE client | could remove TCP/USB-IP framing and permit tighter completion instrumentation | large kernel/user lifecycle, security, signing, installer, recovery, HLK, and compatibility surface | decision seam only; no implementation authorized |
| UMDF HID plus supported inbox `xinputhid` binding | HID/Windows class-driver surface, if the documented relationship is actually supported | resembles HIDMaestro's fast Xbox One/Series route | descriptor alone does not establish XInput admission; supportability and four-channel output are unresolved | experimental alternative pending official/repeatable evidence |
| HID plus separate XUSB-facing companion | multiple API surfaces fed from one state | can expose a surface absent from ordinary HID | private GUID/IOCTL or undocumented XUSB relationships, duplicate identity/lifecycle, another driver/package | reject unless a documented requirement is proven and separately authorized |

Microsoft UDE documentation establishes that UdeCx emulates USB devices and
endpoints. It does not establish that UdeCx is equivalent to an inbox
`xinputhid` relationship or to an XUSB-facing companion. Each Windows surface
needs its own evidence.

## Current landing zone

The current branch contains only a platform-neutral native contract:

- ABI major/minor `1.14`;
- required package version `0.1.0.38`;
- exact C/Go structure sizes and offsets;
- fail-closed negotiation and capability matching;
- source/package/ABI/capability build identity;
- kernel-authored device-correlation receipt;
- device, endpoint, generation, sequence, completion, statistics, and
  lifecycle-trace value definitions.

The fixed contract lives in
`native/udecx/include/ViiperUdeProtocol.h` and
`internal/transport/udecx`. Its tests compile and run without a WDK. No current
driver runtime, Windows client, service installer, broker host, or live
backend selection exists on this branch.

The machine has an inactive legacy staged package `0.1.0.35`; no active VIIPER
native device/service is present. It cannot satisfy ABI 1.14's `0.1.0.38`
identity and must never be selected or silently downgraded into service.

## Donor branch audit

### VIIPER donor

`origin/feature/native-udecx-bus` is pinned at
`cece30774e9df8183dc74fe6cd25950f1a74d021`; merge base
`fd298a04d7d229293be15b2af664405c9e68114c`. At this audit the current branch
has 18 unique commits and the donor has 240. A merge-base diff is approximately
233 files and 94,091 added lines. The donor includes:

- KMDF/UdeCx device, controller, broker, IOCTL, trace, and descriptor code;
- Go native client/host, device and endpoint lifecycle, recovery, journaling,
  and backend selection;
- direct interrupt-IN submission;
- control/bulk/interrupt/isochronous operation handling;
- INF, package identity, local test tools, validation, signing, verifier,
  crash-dump, performance, and attestation scripts;
- extensive contract, stress, lifecycle, teardown, and live tests.

The donor predates the current generalized Source scheduler and recent
controller input/feedback ownership changes. Its size is implementation
evidence, not a merge recommendation.

### DS4Windows donor

`origin/feature/native-udecx-bus-integration` is pinned at
`0b68dc0e18387deffef65439da0f0832f639bdda`; merge base
`3579450dc8f50a74d9532e711249589c732c460b`. At this audit the current branch
has 19 unique commits and the donor has 38. It contains backend negotiation,
broker-instance identity, package install/repair, native input scheduling,
installer payload and signature checks, UI/setup integration, and more than
eleven thousand changed lines across its merge-base diff.

That branch predates current DS4Windows feedback ownership, DualSense V5
ordering, scheduler, installer, and package pins. A direct merge would import
stale policy and duplicate code.

## Reuse map

| Donor material | Reuse decision | Required reconciliation |
|---|---|---|
| ABI 1.14 header and Go value contract | already extracted | remain byte-identical; bump ABI/package monotonically for any change |
| build identity and exact negotiation | reuse design and tests | bind to newly built driver, broker, and package sources; fail closed |
| kernel device-correlation receipt | reuse design | validate against current broker/device generations and teardown |
| UdeCx descriptor/device construction | candidate donor | re-review every ownership, cancellation, bounds, and WDK contract |
| control/bulk/interrupt/iso operation engine | candidate donor | fuzz malformed requests; prove cancel, reset, suspend, and surprise removal |
| direct interrupt-IN IOCTL | candidate mechanism | adapt beneath `inputpresentation.Source`; claim immutable data only at service boundary and resolve at documented copy/admission completion |
| donor scheduler/coalescer | do not port | current Source transition journal/latest-state scheduler is authoritative |
| donor feedback/profile policy | do not port | current canonical feedback and DS4Windows ownership plane is authoritative |
| lifecycle trace and diagnostic tools | candidate donor | remove sensitive values; rate-limit production logs; align event schema |
| package/signing/installer scripts | design/test donor | rebase onto current package ownership, rollback, HVCI, signature and version rules |
| DS4 backend selection/integration | rewrite against current code | retain one VIIPER backend abstraction; no profile or mapping fork |

## Required future architecture

```text
DS4Windows mapping and target semantic state
  -> current VIIPER API/publication contract
  -> device protocol engine
  -> inputpresentation.Source claim
       -> USB/IP endpoint backend (current)
       -> native UdeCx admission backend (future, separately authorized)

Windows output completion
  -> device protocol decoder
  -> canonical CFBK feedback value
  -> DS4Windows arbitration and one physical writer
```

The future native backend must not add profile mapping, device-specific
physical translation, feedback priorities, or another controller-state queue.
Control, input, feedback, and audio/isochronous ownership remain separate.

## Security and lifecycle requirements

A future implementation must prove:

- exact ACLs on the control device and authenticated broker identity;
- fail-closed ABI/capability/build/package negotiation;
- integer/buffer/descriptor validation before allocation or kernel access;
- one device and endpoint generation domain with no cross-generation retry;
- cancel-safe pending operations, process death, broker restart, surprise
  removal, suspend/resume, hibernation, driver update, and rollback;
- bounded queues, zero hot-path allocation after warmup, no callback under a
  device/endpoint lock, and no unbounded logging;
- one logical neutral/release per feedback ownership epoch;
- HVCI/Memory Integrity, Secure Boot, verifier, static analysis, crash dump,
  and multi-device stress results;
- uninstall/repair that removes only installer-owned state and never leaves a
  ghost controller, stale filter, task, service, or package selection.

No test-signing, Secure Boot/HVCI change, certificate provisioning, driver
install, or package replacement belongs to the current task.

## Test and signing matrix for a separately authorized phase

At minimum:

- Windows 10 supported build and current Windows 11, with HVCI off/on and
  Secure Boot off/on where legitimately available;
- clean install, upgrade from every shipped package, repair, rollback,
  interrupted install, reboot boundaries, uninstall, and stale incompatible
  driver rejection;
- WDK static analysis, CodeQL or equivalent source analysis, Driver Verifier,
  kernel/user stress, surprise removal, sleep/hibernate, process kill, and
  fault injection;
- one, four, and eight devices where supported, including control,
  interrupt, bulk, and isochronous traffic;
- Xbox 360, DS4, DualSense/Edge composite audio, Switch 2 Pro, and any
  evidence-approved Xbox One/Series persona;
- API visibility, feedback, guide/share, audio, lifecycle, and same-method
  latency against USB/IP on the same machine;
- production EV/attestation signing and catalog verification. A locally
  test-signed result is never a release gate pass.

## Measurement gate

Before changing architecture, measure the untouched and candidate USB/IP
paths at identical boundaries. A future native prototype should initially use
the non-release stretch target from the task specification for VIIPER
publication to native terminal completion: p95 at most 0.5 ms, p99 at most
1.0 ms, and p99.9 at most 2.0 ms. These numbers are a proposed experiment,
not a current completion gate or a parity result.

Native work is justified only if USB/IP fails an agreed correctness or
same-span latency gate and the failure remains attributable after endpoint
interval, consumer cadence, Windows preemption, logging, and queue behavior
are separated.

## Effort and risk estimate

For one experienced Windows driver engineer, a responsible production phase
is likely a multi-month project, not a small backend toggle. A planning range
before external signing/attestation queues is:

| Workstream | Engineering range |
|---|---:|
| current-code reconciliation, threat model, design and ABI review | 1--2 weeks |
| driver/client core, Source adaptation, interrupt/control lifecycle | 3--5 weeks |
| bulk/iso/audio, feedback, reset/power/recovery and multi-device hardening | 4--8 weeks |
| installer, versioning, rollback, diagnostics, verifier and test matrix | 4--8 weeks |

The ranges overlap with multiple engineers but do not simply add in parallel.
For one engineer, approximately 3--6 calendar months to a serious release
candidate is more realistic than completing the old branch. Xbox
authentication/presentation feasibility, signing service delays, HVCI defects,
or hardware/OS matrix failures can extend that range or invalidate the chosen
persona.

## Authorization checkpoint

The smallest item needed before revisiting this decision is a completed real
Xbox direct-versus-USB/IP positive control and a same-span USB/IP latency run.
If either fails its predeclared gate, present the raw evidence and request a
separately scoped native-driver authorization. Until then, the existing ABI
and Source seam are the only native deliverables.

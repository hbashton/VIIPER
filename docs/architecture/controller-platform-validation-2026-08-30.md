# Controller platform validation status, 2026-08-30

This is the current evidence snapshot for the in-progress DS4Windows + VIIPER
Xbox One/Series and physical Switch 2 work. It is not a release note. It does
not claim a registered Xbox One/Series device, live Switch 2 product support,
500 Hz delivery, latency parity, or hardware-verified haptics.

The full audit and source ledger are maintained in
[`controller-platform-audit-2026-08-29.md`](controller-platform-audit-2026-08-29.md)
and
[`controller-platform-provenance-ledger-2026-08-29.md`](controller-platform-provenance-ledger-2026-08-29.md).
The previous broader validation snapshot remains in
[`controller-platform-validation-2026-08-29.md`](controller-platform-validation-2026-08-29.md).

## Repository and mutation boundary

| Repository | Branch | current HEAD | state |
|---|---|---|---|
| DS4Windows | `feature/native-udecx-landing-zone` | `061fab1304e77c995ce9451b7ef20e51cc870070` | dirty; platform work and preserved user work are uncommitted |
| VIIPER | `feature/native-udecx-landing-zone` | `54f7853aa4f97394298fc8f4c3ebb865237a3456` | dirty; platform work and preserved user work are uncommitted |

No commit, reset, push, tag, release, installer, driver installation, Bluetooth
association change, firmware operation, or system signing change was
performed. The Xbox persona remains absent from the production registry and
the Switch 2 runtime remains absent from `ControlService`.

## Implemented and replay/simulation verified

### Xbox canonical feedback execution

The dormant retained Xbox USB owner now has one concrete local executor for
the project-wide CFBK v1 contract. It does not create another mailbox,
scheduler, mapping model, or output owner.

- Four Xbox actuator percentages map atomically to body-low, body-high,
  left-impulse, and right-impulse channels.
- Configuration loss and USB reset clear output with lease-retaining
  `Neutral`; only authenticated terminal disconnect emits `Stop`.
- Reversible reset drains ordinary execution, publishes the exact successor-
  generation neutral, rejects stale predecessor work, and reopens only after
  accepted publication.
- Terminal drain is permanent. A successful successor-generation `Stop`
  prevents mailbox resurrection in the retired ownership epoch.
- Publisher failure is usable only when it proves no byte was accepted and no
  late publication can occur. An uncertain IPC result must instead close,
  drain, and quarantine the higher session boundary.
- One composition test exercises the real dormant retained adapter through
  generation-one four-actuator output, USB reset, generation-two neutral and
  rebind, stale-generation rejection, resumed four-actuator output, terminal
  drain, and generation-three `Stop`.

The exact source and blocker report is
[`xbox-one-dormant-duplex-boundary-2026-08-30.md`](xbox-one-dormant-duplex-boundary-2026-08-30.md).
The raw persona is still deliberately unregistered.

### Shared legacy/runtime slot authority prerequisite

DS4Windows now has a dormant exact-generation registration owner for an
already-discovered legacy HID lifetime. The existing lifecycle host must issue
an opaque lease from its private issuer and retain the exact `DS4Device`
reference and nonzero connection generation. A MAC address, copied numeric
generation, slot number, or foreign issuer cannot recreate ownership.

Offline tests place a legacy HID registration in slot zero and a Switch 2
runtime registration in slot one through the same
`InputControllerRegistrationTable`. This proves the shared authority shape,
not live `ControlService` ownership. Startup, hot-plug, removal, and service
stop still bypass the table. Their current independently selected array slots,
anonymous report delegate, MAC-based removal lookup, partially startable
workers, unbounded joins, and swallowed stop exceptions prevent truthful live
wiring. The exact prerequisite and required tests are recorded in the
DS4Windows document `docs/protocols/legacy-hid-shared-slot-authority.md`.

### Reference conclusion affecting the hot path

The pinned PadForge, HIDMaestro, Switch2Connect, SDL, and Microsoft sources do
not establish a Switch 2 500 Hz protocol mode or a comparable end-to-end
latency result. Switch2Connect's current wired input reader handles each
completed report immediately but its virtual publication remains newest-only;
its active physical writers add explicit 12--15 ms pacing. PadForge's named
500 Hz option is an application-loop target and has no committed physical or
consumer trace. The DS4Windows USB and BLE input pumps therefore remain
completion-driven with no artificial polling sleep, while physical rate and
application-visible latency remain measurement gates.

Current HIDMaestro source also includes a source-reported Windows 11
investigation in which direct XInput motor bytes reached its experimental
ROOT-enumerated XUSB virtual while WGI/Chromium did not produce motor-bearing
bytes at the observed layers. Its USB-parent explanation is an inference and
the document conflicts internally about GameInput. This is a negative control
for API visibility; it is not proof that VIIPER USB/IP will bind or receive
four channels.

## Independent validation completed in this snapshot

### DS4Windows focused checks

```text
dotnet test DS4WindowsTests/DS4WindowsTests.csproj -c Release -p:Platform=x64 \
  --filter FullyQualifiedName~LegacyHidInputControllerRegistrationOwnerTests \
  --no-restore
19 passed, 0 skipped, 0 failed

dotnet test utils/Switch2UsbHardwareVerify.Tests/Switch2UsbHardwareVerify.Tests.csproj \
  -c Release -p:Platform=x64 --no-restore --nologo
59 passed, 0 skipped, 0 failed

dotnet build utils/Switch2UsbHardwareVerify/Switch2UsbHardwareVerify.csproj \
  -c Release -p:Platform=x64 --no-restore --nologo -warnaserror
build succeeded, 0 warnings, 0 errors
```

The independently recomputed verifier hashes remain:

- DLL:
  `87D2A053DD6FF7A447D71E27B13D2715B188547E8A33E3816EF95164184F4A4E`
- EXE:
  `B68998F98B5FA3A95E9050A0F055456169E16E6B0D678F29C1D4A2044786AA2C`

### VIIPER full and race checks

```text
go test ./...
passed

go vet ./...
passed

CGO_ENABLED=1 go test -race \
  ./device/xboxone ./controllerfeedback ./internal/retainedusb \
  ./internal/server/usb -count=1
all four packages passed
```

The complete Xbox package passed in the full run. The focused canonical-
feedback run also passed independently. Repo-wide `git diff --check` remains
unclean only because preserved unrelated trailing spaces exist in
`docs/cli/configuration.md:70` and `docs/cli/server.md:47`; scoped platform
files are clean.

## Hardware status

The exact connected Switch 2 Pro composite remains present as `057E:2069`,
`bcdDevice 0x0201`, with its HID, WinUSB, audio, and composite nodes healthy.
The source-fixed verifier has not been rerun in this snapshot yet. Its nonzero-
haptic gate is closed, so an authorized run can only exercise passive input
cadence, volatile startup, battery query, Player LED 1, and explicit Player LED
`AllOff` cleanup. It cannot emit either nonzero or zero-amplitude haptic
reports and cannot return overall success.

Earlier evidence remains narrower: a non-writer-isolated run observed about
250 host read completions per second with four-count controller-counter
movement, while the later sole-writer procedure failed before I/O with Win32
sharing violation 32. Neither result proves interrupt rate, 500 Hz operation,
input latency, haptic delivery, or the identity of another handle owner.

## Designed, unimplemented, or blocked

- **Xbox production presentation:** no DS4Windows Xbox One/Series output type,
  live broker route, concrete production publisher/binding constructor,
  registry entry, lawful project USB identity, compiled semantic metadata,
  Guide/Share input, Windows API visibility result, or hardware feedback basis
  exists. The dormant code cannot be called product support.
- **Switch 2 production registration:** legacy HID lifetimes do not yet use the
  shared table. A typed bounded start/stop proof and exact retained report
  delegate are prerequisites before `ControlService` can attach USB, BLE, or
  joined Joy-Con runtimes without slot reuse and teardown races.
- **Owned Pro USB composition:** the current Windows lease is read-only and the
  startup, input, and output foundations are not yet coordinated under one
  admitted full-duplex physical lifetime. Startup-before-input, sealed
  feedback activation, exact neutral-before-retire, and whole-composite
  quarantine remain mandatory.
- **Switch 2 feedback:** raw Pro USB HD-rumble encoding and a pure four-channel
  compatibility translator are offline verified, but no live factory owns a
  physical writer. BLE output framing, cadence, watchdog/neutralization,
  thermal behavior, and actuator measurement remain open.
- **Hardware matrix:** no Joy-Con 2 L/R hardware, Switch 2 BLE run, real Xbox
  reference, Windows 10 result, second Bluetooth radio, DualSense/Edge feedback
  comparison, generic ERM result, game/API matrix, long soak, or same-hardware
  competitor benchmark has run.
- **Latency:** no same-span PadForge/HIDMaestro non-inferiority experiment or
  live USB/IP Xbox result exists. No lower-latency or parity claim is justified.

The native UdeCx branches remain audit-only donors beneath the same semantic
and scheduling contracts. No driver implementation or installation is
authorized by these unresolved USB/IP gates.

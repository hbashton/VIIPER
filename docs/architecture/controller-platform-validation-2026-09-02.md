# Controller platform validation status, 2026-09-02

For the later coordinated auth v2 repair and current acceptance gaps, see the
[September 3 continuation](controller-platform-validation-2026-09-03.md).

This is a dated evidence snapshot, **not a completion or release certificate**.
The full Switch2Connect parity, wireless Pro/Joy-Con matrix, every physical
source/virtual target, game usability, feedback fidelity, latency comparison,
and test-installer requirements remain unchanged. Earlier dated ledgers remain
historical evidence; a test count does not supersede their unrun gates.

## Latest continuation: live b43 USB proof, disconnect defect and b44 repair

The user closed installed VIIPER PID 5656. The isolated b43 broker/app then ran
with the hash-pinned Desktop launcher; its child environment now clears
`DS4WINDOWS_` as well as `DS4W_` and `VIIPER_` experiment overrides.

Live b43 observations (local 2026-09-02 evening):

- Switch 2 Pro USB reached **Ready** and the synthetic authorized F00D:BEED
  virtual Xbox One enumerated as Xbox Gaming Device. Observed physical input
  cadence was about 4 ms / 250 Hz, not an end-to-end latency measurement.
- A 30.01-second XInput check had 1,894 successful reads, no errors and 291
  changed packet observations during user button/stick activity. Both sticks,
  button press/release and trigger 0/255 transitions were observed. The
  PowerShell polling interval was not precise enough to measure report loss,
  cadence or latency. Raw output was truncated; the local summary says so.
- XInput body pulse: requested 200 ms at 10,000/65,535; set and final zero both
  returned success. User confirmed "Yes, it vibrated and stopped."
- New source-built `utils/ControllerWindowsApiProbe` is only a WGI consumer,
  matched to one F00D:BEED Gamepad via RawGameController. User-operated controls
  separately exercised left/right body and left/right impulse channels at
  20% for nominal 200 ms, always zeroing the captured Gamepad in finally.
  Recorded 11 channel pulses and 11 neutral writes, no API exceptions; user
  confirmed "all good." This is tactile functional evidence, not actuator-side,
  spectral fidelity or onset timing proof. Changing WGI input remains unproved.
- Startup uses the existing LED command, but independent live LED selection
  and observation are still pending. No Bluetooth association was attempted.

The unplug report exposed a real lifecycle gap. The saved runtime's final
physical QPC maps to approximately **22:21:55**, before the **22:22:25** Stop
attempt. Discovery never retired missing active USB registrations. Stop then
required a physical terminal HD-rumble write after unplug; its proven rejection
prevented input/slot cleanup. The UI only re-enabled Stop on ServiceChanged,
which this rejected operation did not emit, leaving a stale Ready row. Windows
later showed neither physical Pro nor F00D:BEED present. The native rejection's
specific Win32 error was not retained in b43, so it is not inferred here.

A local heap captured only after unplug/stalled cleanup supported this trace:
feedback NeutralizeInProgress, sink TransportRejected with no uncertain delivery,
native output no pending operation/claim/quarantine. The dump contains private
process memory and stays outside the repositories; do not upload or publish it.

b44 repair: successful USB scans now reconcile missing exact tokens through the
existing removal transaction. Adopted native output can separately prove and
seal definite device removal (433/1167 only); pending I/O must still drain.
`ExactDisconnectedAndQuiescent` never claims a delivered rumble Stop. Input
drain, virtual neutral acknowledgement, composite disposal and exact slot
removal remain mandatory. UI Start/Stop recovers after rejected or faulted
operations without pretending the service stopped. No latency pacing change.

Verification: **2,964 passed, 3 opt-in live-audio skips, 0 failures** in
`DS4WindowsTests/TestResults/usb-disconnect-retirement-full-20260902.trx`.
Thirteen added executions cover strict native removal codes, retained-I/O
fencing, sealed output, authenticated disconnected feedback, blocked old/new
publications, virtual terminal neutral and token reconciliation.

Portable b44 was published and launched at about 22:43 into
`runtime/DS4Windows-current-2026-09-02-b44-usb-disconnect` in the Desktop lab.
DS4Windows.dll SHA256:
`DA03C46693258621D2D558983F152EEBE0AFC124B1AB298853258A09FE14D7BD`.
VIIPER unchanged:
`E7DF0CA7C66757AC6AB1138345C9808AFDAC2B004181ADD14C79C2DE7EFA1B8F`.
Empty controller UI and running service observed. User reconnected USB at about
22:44. Physical input returned at 4 ms, but Xbox output closed after a semantic
input ACK timeout. b44's feedback-reader catch omitted the exception message,
so this particular closure is not attributed to a specific read error.
Controlled Stop at 22:47:10 **did** clear the row, remove its association and
return the button to Start. A subsequent Start failed native attach because
the broker's retained-import state was quarantined. Neither a successful
Xbox restart nor live surprise-unplug retirement is claimed.

Follow-up source test identified a deterministic, separate feedback race:
VIIPER can consume ACK N and send N+1 before DS4Windows' ACK call or completion
observer returns. The old one-slot dispatcher rejected that valid successor
while still marking N outstanding. A blocked-completion regression failed on
the original implementation. The repair keeps one worker, an active payload,
and one bounded successor slot opened only after successful physical delivery
at the ACK phase. Failed ACK discards the successor without physical output;
payload ownership and idle status remain exact, and replayed correlations are
rejected. Broker input handling/pacing and USB descriptors are unchanged.
The feedback-reader error now includes its exception message instead of hiding
the first failure behind a later input timeout.

Full follow-up suite: **2,967 passed, 3 skips, 0 failures**, artifact
`DS4WindowsTests/TestResults/usb-disconnect-xbox-ack-handoff-full-20260902.trx`.
b44 app closed normally; its exact idle broker stopped. A fresh b45 portable
candidate is being prepared; this is not yet a live-success claim.

### b45 live failure and START-ordering regression coverage

b45 launched normally with DS4Windows.dll
`3D5EC38F030A0FECED68D311B95FD861E3A8E81070CEF7E8DC13F8952FB92106`
and unchanged broker `E7DF0CA7C66757AC6AB1138345C9808AFDAC2B004181ADD14C79C2DE7EFA1B8F`.
Physical Pro USB attached at 22:52:56.678. At 22:52:57.471 the broker reported
`prepare late USB/IP response: xboxone: invalid dormant retained USB preparation`,
then failed retained scheduler close. The API rejected subsequent semantic input.
DS4Windows recorded device-stream closure at 22:52:57.473 and semantic-input
rejection at 22:52:57.994. Thus the feedback dispatcher race is **not established
as the cause of this live failure**, and neither a stable Xbox pad nor reconnect
success is claimed. Physical USB remained active at the later read-only check.

Added error-only broker facts for rejected preparation: lane, disposition,
wait reason, prepared size, host transfer length and retry delay. The sentinel
remains wrapped for `errors.Is`; no packet dump, device identifier, per-report
logging or changed admission policy was added by that instrumentation.
Diagnostic-only b46 was staged, not launched, with broker
`FE38D8E8C0F763D75436BC96BB37FA24260E5E0B50CF4EAE76C9B2E7E38ADB5D`.

Independent deterministic test then reproduced one concrete adapter defect:
OUT after START current status but before mandatory initial IN produced
`lane=3 disposition=1 wait=6 size=18 host_length=13 retry_ms=0` and was treated
as fatal. The coordinator intentionally uses that wait to preserve initial
input ordering; the retained adapter omitted it. A narrow OUT/initial-input
translation now retains the exact request as Pending until IN completion
signals readiness. It does not introduce an input snapshot, host ACK, physical
feedback, periodic retry, or permission for other rejected states.

`retained_usb_start_ordering_test.go` covers latest initial input, latched
readiness, exact successor command delivery, duplicate completion rejection,
unlink without feedback, and six invalid-state exclusions. The original
ordering test failed before the fix and passed afterward. Validation:

- `go test ./...`: all packages passed.
- `go test ./device/xboxone -run '^TestRetainedUSBStart' -count=100`: passed.
- `go test -race ./device/xboxone -run '^TestRetainedUSBStart' -count=20`:
  passed using existing w64devkit 2.9.1 with child-only CGO/CC/PATH settings.
  The initial attempts lacked CGO, then the linker PATH; those were build
  configuration failures, not passing race runs.

Fixed b47 was staged, not launched, at Desktop lab
`runtime/DS4Windows-current-2026-09-02-b47-start-ordering`, app hash unchanged
from b45 and broker
`26766644B50C0A753EAE3154E569D0B8EE5B0EDC90800FA16B7612337224249B`.
The live b45 error predates detailed fields, so the START wait is a proven
source defect but still only a **candidate explanation** for that live failure.
The b45 app remains running for the requested physical-unplug test. The last
read-only PnP/log/UI check still had Pro USB present; automatic row cleanup and
Bluetooth association remain unrun, not passed. No installed component was
replaced and no app was closed to mask the unplug result.

b43 app and probe closed normally; its exact idle portable broker was stopped.
The 34-item before/after snapshot (installed executable hashes, startup/task
hashes, roaming XML/JSON hashes/timestamps) was unchanged. Fresh b44 baseline
stored locally. No Program Files replacement, security change, task edit,
commit, push, release or reboot in this continuation. Earlier entries below
are historical and do not override this section.

### b46 live START-ordering root cause and b48 typed UI retirement

This entry supersedes the running/staged state above, not its historical
observations. b45 closed normally. At 23:07 its old input binding had retired
while the controller list retained the old object; a later registration failed
against the quarantined broker. Physical USB was present at the later check.
No user-confirmed unplug/replug sequence is inferred from those events.

Diagnostic-only b46 launched, reached Ready, and completed a controlled Stop.
Its first restart failed at 23:22:21.935 with:
`lane=3 disposition=1 wait=6 size=36 host_length=7 retry_ms=0`.
This **confirms the START initial-IN wait defect in a live failure**: the host
sent an OUT while the mandatory initial IN was still pending. The prepared
36-byte input length and independent seven-byte OUT length are not a buffer
mismatch. The OUT payload was not captured, so its command is not inferred
from its length. b46 then closed normally; b47 remained staged, never launched.

Production USB/IP integration coverage now sends an early host command before
initial IN through the real retained scheduler/server, then checks latest
initial input, deferred OUT completion and subsequent input. Both the normal
and early-OUT integration variants passed 50 repetitions and 20 race-enabled
repetitions; all `go test ./...` packages passed afterward.

The C# runtime now publishes a typed exact-token removal notification only
after successful terminal removal. It covers USB, BLE and joined runtimes,
runs outside transaction locks, isolates observer failures, and does not fire
on quarantine. ControlService forwards the old device object and slot to the
UI without invoking legacy HID teardown. The controller list removes only an
exact object/slot match, so delayed old notifications cannot erase a successor
or reset its linked-profile state. Collection locks are released before
dispatching persistence work. Regression tests cover terminal notification,
observer reentry, failure/no-notification, sparse slots and late old removal.

Full C# result: **2,973 passed, 3 opt-in live-audio skips, 0 failures** in
`DS4WindowsTests/TestResults/typed-controller-removal-full-20260902.trx`.
b48 launched from Desktop `runtime/DS4Windows-current-2026-09-02-b48-typed-removal`:

- DS4Windows.dll: `D6E8C6D7DE89D96676AB960C4EB35DEC4A00E23FF450737040AEFA4EA539F4B4`.
- Fixed broker: `26766644B50C0A753EAE3154E569D0B8EE5B0EDC90800FA16B7612337224249B`.
- Initial USB Pro to Xbox One association reached Ready at 23:23:56.
- Controlled Stop at 23:25:10 removed the association and cleared the UI row.
  This alone does not prove surprise-unplug notification: Stop also clears
  the list through the existing service event.
- The first restart's authenticated broker probe failed at 23:25:26, despite
  the broker still running. The broker recorded an incomplete handshake/EOF,
  not proof of a bad key. The app's generic "server not running" message was
  misleading; the root cause of this one probe failure remains unresolved.
- A second Stop/Start reached Ready at 23:28:29. A private exception trace
  captured that successful cycle outside git; it did not reproduce the failed
  probe and does not establish the original exception.
- At 23:31:41 the prior binding retired and at 23:31:44 a new USB binding
  attached. A later visual check showed one Ready USB Pro row, Xbox One / Series
  output, 90% battery and observed input interval about 4 ms / 250 Hz. This is
  neither an end-to-end latency measurement nor a confirmed unplug test.

Error-only source diagnostics now distinguish a running broker's failed
connection check and record a fixed phase plus exception type, without
exception text, keys, key paths or peer bytes. Authentication and timeout
behavior are unchanged. The 59 focused prerequisite/portable tests passed;
this diagnostic source change is not present in b48 and is not an auth fix.

The final diagnostic suite passed **2,975 tests, 3 opt-in live-audio skips,
0 failures**, artifact `DS4WindowsTests/TestResults/probe-status-full-20260902.trx`.
It additionally verifies that runtime connection failure is not mislabeled as
startup-task maintenance, independent prerequisite failures retain precedence,
and a stale diagnostic cannot override current readiness. A self-contained
b49 diagnostic candidate is staged, **not launched**, at Desktop
`runtime/DS4Windows-current-2026-09-02-b49-probe-diagnostics`; DS4Windows.dll
SHA256 `76992FCA4F1B094F316ECAA0BB315C7D5D42EEB5CEC6B497B9F25021EE38FC50`,
broker unchanged from b48. b48 stays running awaiting the requested physical
unplug, rather than restarting the application in place of that test.

The 34-item before/after installed/startup/roaming snapshots for b45 and b46
were unchanged. A b48 interim comparison also found 34 items and zero changes.
b48 final snapshot, explicit surprise-unplug/UI cleanup,
Bluetooth association and the broader production matrix remain pending.
Old public Xbox USB/IP locations continue background reimport attempts after
removal, as documented in `xbox-one-registration-removal-v1.md` in DS4Windows.
No bare-port detach, driver modification or false protocol reply was used to
silence those retries. All testing remains portable; no installed replacement,
security/task change, release, commit, push or reboot was performed.

### Follow-up: START failure boundaries and authenticated-key ownership

Independent source review found no concrete defect in the narrow initial-IN
wait repair, but identified three specific test gaps. Added directed adapter
tests now cover:

- an already-waiting OUT surviving failed initial-IN delivery, immutable
  initial retry despite a newer publication, no motor action before its own
  successful OUT completion, exactly one motor execution and later input;
- connection close and device reset while initial IN never arrives: exact
  retirement, real drain/neutral/reset transactions, one terminal clear,
  stale-ticket rejection and no motor execution;
- the official Share-capable 36-byte input persona with a independently
  specified seven-byte Guide LED command. This fixture deliberately does not
  identify the uncaptured live OUT by its length.

`go test ./device/xboxone -run '^TestRetainedUSBStart' -count=100` and the same
selection with `-race -count=20` passed, followed by `go test ./...` passing.
The first new retry test overconstrained an existing upstream retry wait to
have no timer; that assertion was corrected after inspecting the coordinator
and adapter contract. No production retry policy or pacing changed.

A separate deterministic C# test proved that the old deployment-key cache
lent its mutable array to handshakes: refreshing the cache during an in-flight
handshake cleared its key before session derivation. The original test failed
with `AuthenticationTagMismatchException`. Each handshake now owns a copy
made under the cache lock and clears it/scratch on success or failure. Stream
disposal also guarantees key/buffer clearing when transport close throws;
that original failure was reproduced separately. These are connection/cleanup
changes, not report-path allocation or scheduling changes.

Tests cover cache-refresh overlap, a failed-handshake successor, successful,
rejected, truncated, read-failed and write-failed handshakes, and throwing
transport disposal. Full final C# result: **2,983 passed, 3 opt-in live-audio
skips, 0 failures**, artifact
`DS4WindowsTests/TestResults/auth-key-lifetime-full-final-20260902.trx`.
The preceding full run exposed an existing runtime-owner fixture race:
`MaximumSuccessfulBegins=0` could stop its pump before the test injected a
stale frame. The fixture now permits one pending native read until explicit
Stop; its exact rejection/state assertions remain unchanged. This was a test
setup correction, not a production lifecycle-policy change.

The same independent review found and parent source inspection confirmed
**AUTH-V1-DIRECTION-REUSE**, an inherited encrypted-transport protocol defect.
Client/server directions share a session key and zero-based nonce sequence.
The local key-lifetime fixes do not repair it. A coordinated, versioned
client/server/generated-client repair is required before release; see
`authenticated-transport-direction-review-2026-09-02.md` for evidence,
compatibility scope and acceptance tests. No unilateral wire change,
authentication disable, plaintext downgrade or live credential change was made.
This finding does not explain b48's isolated incomplete probe handshake.

The new C# ownership changes are source/test verified only, not staged into
b48 or b49. b48 remains running; the latest PnP check still showed the Pro's
five USB interface nodes present. No new hardware output, application restart,
Bluetooth association, installed-file change, commit, push or release occurred
in this follow-up. The user-requested unplug and the full wireless/game matrix
remain pending. All earlier completion exclusions remain in force.

## Earlier continuation: universal exact-source report diagnostics

Implemented in DS4Windows on the existing dirty branch, without controller IO
or changes to installed applications. `On_Report` now defers its direct error,
lag-log, initial profile, startup and battery diagnostics for all source/output
types, not only physical DualSense to virtual DualSense/Edge. Canonical mapping,
virtual submission, synthetic input commit and immediate lightbar state remain
on the existing path. Secondary joined-controller early returns also publish
their own diagnostics.

Replaced the slot-keyed shared-monitor merge with cold exact-source handles and
preallocated SPSC triple buffers. Facet revisions preserve one-shot observations
without replay; initial/current battery and lag/startup latency remain separate.
Old same-device-object callbacks cannot borrow a successor's diagnostics source.
Typed legacy binding/detach and Switch 2 full-token StageRecord retirement own
their exact handles. Pause/Dispose are nonblocking; publisher/worker lifetime
accounting protects the wake event. The canonical input journal is unchanged.

Independent source review caught the tray policy re-entry edge case and the
existing Switch 2 state-battery mismatch. Fixed both: a cold icon-policy revision
re-arms an unchanged percentage, and report snapshots use runtime device battery.
Queued battery UI actions recheck source identity and current icon policy.
Already-admitted historical logs may finish after retirement, as documented.

Verification:

- Release application build: succeeded, existing warnings only.
- `report-diagnostics-focused-20260902.trx`: 78 passed, before the final runtime
  battery capture test/fix.
- **`report-diagnostics-full-20260902.trx`: 2,949 passed, 3 opt-in live-audio
  skips, 0 failures**, default runtime settings; 26 added test executions.
- Tests include blocked callbacks, slot reuse/ABA, facet preservation, closure
  races, failure recovery, warmed zero-allocation publication, 25,000-publication
  coherence, actual secondary report return, Switch 2 host terminal/cleanup and
  optional refusal, stale tray delivery and runtime battery capture.
- Independent reviewer found no additional concurrency/lifecycle blocker in
  this bounded source review. No hardware or latency result is implied.

Read-only recheck still showed elevated installed VIIPER PID 5656, while old
DS4Windows was gone. No second broker or controller test was started. No attempt
to bypass the previous stop denial, install/replace Program Files, change tasks,
commit, push, release or reboot. The unresolved hardware gate remains exiting
that broker. Broader production/feature-parity gates remain unchanged.

Details: DS4Windows `docs/protocols/report-diagnostics-ownership.md`. Separate
legacy touch-toggle/mapping-action notifications, other device events, legacy
DSU and OSC lifecycle paths are not certified by this change.

### b43 portable staging and reported Connecting status

Published a fresh self-contained x64 app into
`runtime/DS4Windows-current-2026-09-02-b43-report-diagnostics` under the Desktop
lab. DLL SHA256:
`E26ECB7EA3B44B4F273B3BDF5E7CFE5811CE11AAA209CF8B67A4BF98C7E24045`.
Copied and rehashed the previously built isolated VIIPER:
`E7DF0CA7C66757AC6AB1138345C9808AFDAC2B004181ADD14C79C2DE7EFA1B8F`.
New lab-data copies only the reviewed empty configuration/actions/auto-profiles
and explicit Xbox One lab profile. Same synthetic authorized persona. The new
hash-pinned launcher was parsed successfully but **not executed**; b42 and its
negative-startup evidence remain untouched. No live b43 readiness is claimed.

The user reported the repeated Connecting display. Read-only PE metadata
inspection proved the previously observed b40 DLL
(`96674916610EEDB06B5727CD8874A180FD20BCFF93B6702E09A2C647C26B13E2`)
does not declare Switch2RuntimeInputDevice.IsAlive or ObservePhysicalInputNoLock.
It therefore inherits the DualShock priorInputReport30 sentinel check, which
the Switch 2 runtime never updates. This is a concrete cause for the old build's
Connecting display despite input. b43 declares both existing source fixes;
validated physical frames establish life, activation/foreign frames/terminal
neutral do not. The last roaming log confirms USB Pro→Xbox360 association and
normal shutdown; no DS4Windows process remained at this recheck, only VIIPER
PID 5656. This diagnosis is not proof of current Xbox One/game/physical output.

Added USB/BLE synthetic-runtime-to-status regressions: activation and a foreign
frame remain Connecting; a validated physical frame permits Ready only if the
virtual path is also ready; terminal neutral revokes readiness. Final suite
`report-diagnostics-readiness-full-20260902.trx`: **2,951 passed, 3 opt-in
live-audio skips, 0 failures** (28 added executions relative to the prior lab
policy suite). These test-only additions do not alter the staged b43 app DLL.

The user additionally requested the Switch 2 profile controls leave Advanced
and become their own section immediately after Audio Haptics/Trigger Lab,
**after backend stabilization**. Preserve this as required delivery work:
coherent feature groups, clear terminology/defaults and explanations, applicable
Pro/standalone/joined-Joy-Con options, and visual/interaction validation against
Switch2Connect's usability. The current large Advanced group is not accepted as
the final UI. No claim of already meeting or exceeding that benchmark.

The user reiterated interest in a **0.125 ms virtual interrupt-IN service
interval**. Retain it as an experiment target, not achieved controller-to-Xbox
latency. Descriptor/service interval, physical report arrival, broker queueing,
host presentation and game consumption must be measured separately, including
loss/reordering and tail stalls. Do not substitute a displayed polling rate for
the production reliability and latency gates.

## Previous continuation: explicit portable lab startup isolation

Implemented a cold-start DS4Windows `--portable-lab <expected-VIIPER-SHA256>`
policy. It fixes configuration to the app's `lab-data`, pins exactly its
`viiper.exe` open against write/replacement, uses a lab-local authentication
key, and probes the broker through the existing encrypted/authenticated wire
protocol. It never falls back to a production broker, starts one implicitly,
repairs/installs packages, updates startup tasks, migrates to roaming, runs an
updater, changes HidHide policy, or accepts shared legacy IPC. The usual usbip
version/hash/driver/probe/conflict gates remain. Production VIIPER's compiled
package pin is unchanged. The ordinary global single-instance event remains
authoritative, including creation-race rejection without activating its owner.

Independent review found an additional automatic exclusive-open HID recovery
path that could launch a helper without lab context. Fixed the effective
exclusive-mode getter without changing its stored setting; guarded direct
SetupAPI recovery, elevated helper dispatch and Steam reclaim too. Added a
regression test. Reviewer rechecked those fixes and the nonzero refusal exit
codes/HidHide UI guard; no remaining blocker in that bounded source review.
The exclusive-mode test sets the stored config in memory; it is not a full
XML-load/application/hardware recovery test.

VIIPER now accepts an explicit absolute `server --key-file` (no fallback on
explicit read/validation failure; exclusive creation only when missing) and
CLI-only `--config-only --config <absolute-file>` (exact in-memory config,
no cwd/user/system search). Omitted options retain previous behavior and
cryptography. Explicitly generated secrets are not printed. Windows ACL
inheritance is unchanged. Environment overrides still require launcher control;
this is a startup policy, not a sandbox for arbitrary profiles/plugins or a
hostile same-user process. See DS4Windows `docs/portable-controller-lab.md`
and VIIPER CLI documentation for scope and limitations.

Automated verification:

- `portable-lab-policy-initial-20260902.trx`: 64 focused passed.
- `portable-lab-policy-auth-20260902.trx` and
  `portable-lab-policy-final-20260902.trx`: 66 focused passed each.
- `portable-lab-policy-reviewed-20260902.trx`: 67 focused passed.
- `portable-lab-policy-full-20260902.trx`: 2,922 passed, 3 opt-in live-audio
  skips, no failures, before the last recovery guard.
- **Final** `portable-lab-policy-reviewed-full-20260902.trx`: **2,923 passed,
  3 opt-in live-audio skips, 0 failures**, default runtime/test settings.
  Nineteen new lab-policy test executions are included. Existing intermittent
  stick-filter allocation attribution remains open; no threshold was changed.
- `go test ./cmd/viiper ./internal/cmd ./internal/config
  ./internal/configpaths ./internal/server/api/auth -count=1` passed, independently
  repeated by main with `_toolchains/go1.27.0/go/bin/go.exe`. `internal/config`
  has no tests. Agent also ran the 21 new top-level Go tests verbosely, including
  Windows sharing-denied reads and link checks; none skipped.
- Source `git diff --check` passed (existing line-ending notices only). No
  commits, pushes, release tags, package installation, or reboot in this work.

### Desktop b42 and actual negative startup test

Fresh complete self-contained x64 publish staged at
`Desktop/Controller-Platform-Portable-Lab-2026-08-31/runtime/
DS4Windows-current-2026-09-02-b42-isolated-startup-lab` (one directory path).
DS4Windows used ordinary `dotnet publish -c Release -p:Platform=x64 -r win-x64
--self-contained true --no-restore`; VIIPER used `go build -trimpath -o
<lab>/viiper.exe ./cmd/viiper`. Build hashes:

- DS4Windows.dll:
  `38B318D7CA8CFD7F508BD500DE60BDE4E0FEBCC49754DB7E6CD49C6DE82D7692`
- viiper.exe:
  `E7DF0CA7C66757AC6AB1138345C9808AFDAC2B004181ADD14C79C2DE7EFA1B8F`

Ran `DS4Windows/utils/portable-lab-refusal-smoke.ps1` against b42 while b40
was still running. New PID 3080 returned an exact b42 `Portable controller lab`
window. The user confirmed the message said another instance owned the
controllers. It was dismissed before screenshot capture; do not claim a
screenshot or automated dismissal. Retained `refusal-evidence/result.json`:
exit **1**, **no lab-data directory created**, and exact before/after compared
state unchanged: three installed executable hashes, RunDS4Windows/RunVIIPER
task XML, Startup file hashes, roaming DS4Windows XML/JSON hashes/timestamps,
and existing DS4Windows/VIIPER process IDs/start times. This comparison covers
that completed test interval, not subsequent user actions or all machine state.
HidHide registry reading was denied; no HidHide snapshot equality is claimed.

After the negative test the user closed old DS4Windows. Its roaming log records
normal virtual Xbox 360 disassociation and usbip port 1 detach at 21:20:55,
then service/handler/UDP shutdown at 21:20:57. A limited-process-image query
identified still-running PID 5656 as **Program Files/DS4Windows/VIIPER/viiper.exe**,
SHA `B4C55DAD27CAB5FFE8BDAA1762F251B9CBF7C7DD82B63D45D71C000076CA45B0`.
It had listeners only, no active TCP connections; `usbip.exe port` returned 0
with no imports, and the queried lab VID/PID had no present virtual PnP device.
This establishes its current path, not who previously replaced that file.

Stopping that exact idle broker from the non-elevated session was denied by
Windows. Asked the user to exit VIIPER from its tray/menu or Task Manager; no
permission bypass, startup task change or installed-file replacement attempted.

Following the completed refusal test, deliberately created fresh lab-data
settings/profile/empty auto-profiles, reused the previously authorized synthetic
lab persona (F00D:BEED, 4 ms descriptors), and added `Start-IsolatedLab.ps1`.
It checks the exact build hashes, refuses existing mappers/brokers/listeners,
clears inherited VIIPER/DS4W experiment variables **in child processes only**,
uses one empty explicit JSON config with reviewed CLI settings, authenticated
loopback listeners and local key/log paths, and keeps descriptor service timing.
Its syntax was checked; it has **not yet been executed**. The actual CLI help
corrected a documentation spelling drift: `--api.require-local-host-auth`.
No b42 controller input, feedback, physical LED/haptics, or latency claim yet.

## Latest continuation: automatic Switch 2 DSU observation

Implemented on the same dirty source revisions below; no new hardware run or
installed controller/application change. The reversible runtime host now owns
an optional exact-source DSU handle, captures after canonical mapping/virtual
submission under the existing report borrow, and retires before terminal input
or profile undo. A dedicated worker owns filters, packet building and network
sends. Preallocated SPSC three-buffer ownership prevents producer/consumer
aliasing and does not wait for networking. Optional DSU overload is explicitly
coalesced/counted; the canonical virtual-input journal is not coalesced by it.

Switch 2 port metadata now uses registration ownership rather than a missing
Sony serial. A random locally administered unicast DSU address survives UDP
restart but changes with a registration; MAC-selected clients must rediscover
after reconnect/pair replacement. Pending old sessions are discarded; already-
admitted sends may finish only with their exact old source/session. No UDP
arrival-order guarantee or reliable terminal DSU packet is claimed.
See `DS4Windows/docs/protocols/switch2-udp-observation.md` for the full contract,
four-slot limitation, identity tradeoff, and remaining legacy/UI gates.

Evidence from exact commands using the DS4Windows test command below and the
named `--logger 'trx;LogFileName=...'` artifacts:

- `switch2-udp-observer-initial-20260902.trx`: 30 passed.
- `switch2-udp-observer-production-20260902.trx`: 55 passed.
- `switch2-udp-observer-adversarial-20260902.trx`: 57 passed, no failures/skips;
  filter includes `UdpMotionObservationWorkerTests`,
  `Switch2ProductionGyroMappingIntegrationTests`, `UdpServerSessionTests`, and
  `Switch2ControlServiceReversibleProfile`. New observer coverage comprises 10
  worker tests plus five production-host/mapper integration executions.
- `switch2-udp-observer-full-20260902.trx`: full `--no-build` run after the
  final focused build: **2,904 passed, 3 opt-in live-audio skips, 0 failed**.
- Independent read-only reviewer found no production SPSC/disposal/session
  defect and identified three missing proofs. Added capture-time mapper-order
  assertions, blocked dispatch during session replacement, and disposal before
  unblocking dispatch. Earlier yaw-level type mismatch and missing-motion
  filter re-priming were corrected before this final verification.

The pre-existing intermittent `WarmFilterOwnerAndReducersAllocateNothing`
failure remains **unattributed**, not fixed by a passing full run. One prior
unmodified/default-path full run with scoped `DOTNET_JitDisasm` diagnostics
passed 2,889 plus 3 skips (`udp-motion-original-filter-jit-20260902.trx`). Native
code inspection found no ordinary allocation/boxing in Step or the measured
optimized loop and did observe Tier1 OSR inside the measured method. Runtime
OSR/JIT work is an unexcluded possibility, not a proven cause. No allocation
threshold, production filter, tiering/PGO setting, or default test was relaxed.
The JIT text artifact remains under DS4WindowsTests/TestResults locally.

At the user's separate request the existing Windows Startup `ChatGPT -
Shortcut.lnk` was changed from thread `019ffbee-9cc2-7a10-b392-8ac6250a8a99` to
this thread `01a03f5e-65b2-78a0-826b-253929a6f176`, retaining explorer.exe and
all unrelated shortcut properties. Original shortcut is backed up outside
Startup at `.codex/startup-backups/ChatGPT - Shortcut.before-thread-change.
20260902-204153-136.lnk` (one filename, without the displayed line break).
This proves saved launch configuration, not automatic execution resumption
after reboot. The user subsequently authorized reboots when needed; none was
performed. Program Files and the unrelated RunDS4Windows task were not changed.

### Later read-only hardware preflight in this continuation

The connected physical Pro is present on the expected `057E:2069` MI_00 HID,
MI_01 command, and MI_02 media topology. Process IDs 17048 (DS4Windows) and
5656 (VIIPER) were already running; the ordinary process APIs could not read
their executable paths. Computer Use's returned window identity independently
identified DS4Windows as the old Desktop b40 build, elevated. Its visible
Overview showed USB Pro, 90% battery, the roaming `Default` profile, Xbox 360
output selection, and `Connecting` with no displayed report interval. That
display does not prove absence of input or establish the running VIIPER path.

The current roaming log begins at 20:37:09 and records admin startup, UDP at
127.0.0.1:26760, and exact full-duplex Pro attachment with Default/Xbox360 at
20:37:21. The b40 folder's own log ends at 11:39 and is historical, not this
session's log. Its older retirement failure must not be attributed to the new
session based on that file alone. RunDS4Windows still selects the b40 executable;
its returned last-run metadata is not useful evidence of which launch started
this instance.

Read-only hash revalidation found installed DS4Windows and usbip.exe unchanged
at `2D8DBAD4A4CFB31ADDCAE6CDF8ADA534E35EEAA44344E05E4601272400BCD063` and
`FC1660E3759D8AF4CEDE48DBE194285A5A1DE85CE6E3216724499AFD32BE92E8`.
Installed VIIPER now hashes
`B4C55DAD27CAB5FFE8BDAA1762F251B9CBF7C7DD82B63D45D71C000076CA45B0`,
matching the b40 lab viiper.exe and differing from the prior observed
`AD14F2C9048D61B3447F2F79D7A122EDEA81E5DB52A1AC803D294E5BC9CD2324`.
No action in this continuation wrote or installed it; when/by whom it changed
has not been attributed. Do not claim the whole machine remained unchanged.

No controller command, haptic/LED test, process stop, reboot, app installation,
task change beyond the requested ChatGPT shortcut, or Bluetooth mutation
followed this preflight. The next portable run must explicitly isolate config
and suppress startup-task/installer mutations. Current ordinary startup calls
`FindConfigLocation`, `RefreshSelectedStartupTaskOnLaunch`, and
`EnsureReadyWithPrompt`; a Desktop executable alone does not provide isolation.
The standalone USB verifier's haptic gate also remains source-closed. Use
current source/production feedback contracts and exact owner retirement before
hardware validation, not competing writers or a repeat of temporary profile
file moves/build-source patches from older labs.

## Priority and acceptance scope

The user's latest priority clarification is complete controller functionality
and fresh physical input reaching the virtual pad as soon as it can be safely
accepted. Optimize and measure that path, including loss/reordering and load
tails, rather than treating publication frequency as success. The experimental
1-ms service setting remains simulator-verified only; this clarification is
neither a sub-1-ms latency result nor a reason to start a separate driver stack.

## Source and portable boundaries

Both repositories are on `feature/native-udecx-landing-zone` with substantial
pre-existing uncommitted work:

- DS4Windows HEAD: `061fab1304e77c995ce9451b7ef20e51cc870070`.
- VIIPER HEAD: `54f7853aa4f97394298fc8f4c3ebb865237a3456`.
- Go tests use the workspace Go 1.27.0 toolchain. Race tests use the workspace
  w64devkit 2.9.1 GCC with `CGO_ENABLED=1`.
- Portable lab: `Desktop/Controller-Platform-Portable-Lab-2026-08-31`.
- No Program Files application replacement, driver installation, security
  setting change, push, release, or installer was performed in this tranche.
- The production VIIPER SHA pin remains
  `2EB92FF3E82ABE292E531B6D35B10341396BF2A83FFDE6532FAEC8374B48FB6A`.
  `App.xaml.cs` does not retain the temporary
  `DS4WINDOWS_PORTABLE_LAB_SKIP_STARTUP_TASKS` hook used while publishing b41.

## b41 hardware observations (older than the final source changes below)

Runtime folder: `runtime/DS4Windows-current-2026-09-02-b41-xbox-timed-feedback-lab`.
Re-read and hashed after shutdown:

| Artifact | SHA-256 |
| --- | --- |
| DS4Windows.dll | `9D084B1A7E2DD090521183AC5D672D283EFA3CCD02C4C2E63963E62646F5B639` |
| viiper.exe | `EA0E701B1508E8B3F965FAE9DFE6B738B37EFF544499316BF6ACEA5B6716C181` |
| Logs/ds4windows_log.txt | `14EC1B965DB724AEA374C1DC3CF2D9D550315A5970E819F59A25A776CF1E8413` |

- Switch 2 Pro USB attached at 12:08:28, with the lab Xbox profile and battery
  90%. UI showed a 4.00 ms / 250 Hz physical report interval. This is not a
  500 Hz hardware claim or an end-to-end latency measurement.
- The synthetic lab `F00D:BEED` virtual Xbox enumerated; XInput slot 0 returned
  success. A bounded nominal 200 ms, approximately 15.3% two-body-motor probe
  returned success for both start and stop. Other XInput slots were absent.
- A later 25-second read-only poll observed 1,597 successful XInput queries,
  no errors, and only the initial packet-state observation. No physical
  button/stick change or real-game response was established by that poll.
- A local heap snapshot showed 4 accepted Switch 2 canonical feedback frames
  and 0 rejected frames. **Acceptance is not physical delivery proof**:
  `TryPublishAndPump` can accept ownership while the physical writer remains
  `RetryPending`. No measured actuator onset, felt effect confirmation, LED
  effect confirmation, or fidelity claim follows. The raw heap dump stays
  local and must not be committed or uploaded.
- Runtime disassociate/reconnect sequences occurred at 12:11:36–12:11:41 and
  12:12:56–12:13:02. A diagnostic heap pause is associated with one interruption;
  causal attribution of both is incomplete. This is not a successful soak.
- UI Stop at 12:16:18 detached USB/IP port 1 and completed virtual-controller,
  physical-controller, and FakerInput cleanup by 12:16:19.2919. The earlier
  b40 removal rejection did not recur in this one shutdown.
- Both exact portable processes were subsequently stopped. Revalidation found
  no running DS4Windows/VIIPER and no imported USB/IP port. The normal roaming
  `Auto Profiles.xml` is restored (11,343 bytes); the b41 hold file is absent.

Residual host state: the pre-existing `RunDS4Windows` startup task still points
to the b40 portable path and is Ready. It was inspected, not changed, in this
tranche. Old lab HidHide whitelist entries may also remain. Do not describe
the whole host as restored to a pristine pre-lab snapshot.

## Implemented; verified by software tests, not new hardware

### Finite Xbox motor programs and failure containment

The captured Windows Direct Motor forms include:

```text
09 00 01 09 00 0f 00 00 00 00 ff 00 eb
09 00 05 09 00 0f 00 00 0f 0f ff 00 eb
```

The former is neutral; the latter activates the body channels. The old direct
CFBK projection rejected nonzero Repeat, leaving local feedback acceptance
pending and obstructing persona progress. The production timed executor now
owns zero-delay finite runs and projects their current state through the same
canonical feedback executor. The [Microsoft Direct Motor definition](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-gipusb/ee8c5b28-e8da-4cc4-bb48-17781b8371af)
provides duration/delay units and repeat count. Contiguous zero-delay duration
is `Duration * 10 ms * (Repeat + 1)`; active nonzero Delay remains unsupported
because its phase placement has not been established by inspected evidence.

Bounded leases preserve all four channels and absolute expiry; replacement,
reset, clear, and drain fence old callbacks. Late acknowledgement, callback
panic, clock failure, and renewal rejection cannot silently resume a program.
Additional post-b41 tests cover acknowledgement after effect expiry, a drain
racing an uncertain in-flight publication, and rejection of fenced initial
publication. A reproduced bug allowed a new broker socket to advertise ready
after a fatal timed-feedback failure; a permanent per-persona broker failure
fence now prevents that resurrection. Ordinary clean socket release retains
its existing fresh-token reconnect behavior.

### Physical feedback watchdog and output policy

DS4Windows' generic physical Xbox feedback owner now uses the existing
`ControllerFeedbackStateLanePump` and ingress, with a one-shot expiry wakeup.
It neutralizes expired actuator state, releases trigger overrides, contains
failed/throwing callbacks, and never retries rejected nonzero output later.
Failed neutral remains synchronously retryable; retirement fences queued
timers. Reader teardown retires its captured session rather than a successor
installed in the same field/slot. This boundary is **physical state-setter
acceptance**, not a completed HID write.

Disabling physical output turns a valid Apply into canonical Neutral while
preserving its binding, timestamp, sequence, and TTL; Stop remains Stop.
Foreign, stale, and replayed messages still reject. Switch 2 suppression does
not retain a delayed rumble/release envelope that could reappear later.

The runtime audio-status policy now shares physical applicability with the
UI: inherited Sony audio settings do not falsely mark Switch 2 unavailable,
while genuine supported Sony audio failures remain visible. Saved profile
choices are not rewritten.

### Descriptor cadence: confirmed source-level cause of excessive traffic

The b41 five-second idle sample used 6.25 VIIPER CPU-seconds and 3.265625
DS4Windows CPU-seconds over 5.0216373 wall-clock seconds. This is excessive;
it is not an input latency measurement or a post-fix CPU result.

Independent source review confirmed that retained interrupt-IN repeatedly
claimed the same input image, while retained import validated but did not
apply `bInterval`. Immediate host URB refills could therefore drive a tight
report/sequence loop. The ordinary endpoint scheduler already had cadence,
but the retained path bypassed it.

The retained scheduler now snapshots validated descriptor intervals before
activation and gates each interrupt direction independently. EP0 is not
paced. It uses the existing reusable timer, selects fresh input only after
obtaining response ownership, and samples a fresh time at that boundary.
Pending semantic work can wake immediately at an unused service opportunity;
readiness cannot bypass a consumed opportunity. Full-interval lateness
re-anchors, and successful scoped reset/configuration changes retire the
corresponding cursor. No new polling thread or per-report timer is added.

Eight tests cover a readiness flood, 250 completions over one virtual second
for a 4 ms descriptor, delayed admission, Pending readiness, high-speed interval
conversion, descriptor immutability, scoped reset, the running timer (including
a timer-only second completion), parked unlink/close, and zero allocations on
the warmed complete scheduler path. The independent reviewer found no blocking
cadence defect. **Phase preservation permits close completions after small
lateness; this is not a strict minimum 4 ms inter-completion guarantee.**

The DS4Windows log now calls its 1,000 Hz setting a broker **publication limit**,
not virtual USB presentation rate. The current Xbox persona's minimum
**descriptor** IN interval is 4 ms. Do not reinterpret this as a measured
1,000 Hz endpoint or a proven minimum host-service spacing.

### Ordinary input journal: implemented and software verified

Three new regressions first reproduced lost ordinary taps, lost input during
the START write, and lost transitions behind an exact engine retry. The
retained adapter now composes the shared `FixedReportScheduler` with the
existing GIP engine: ordinary buttons/Share and trigger zero/nonzero boundaries
are journaled; continuous motion coalesces and its last pre-edge value is
retained. The scheduler image preserves `KeepAlive` without changing the
24-byte broker contract. Guide's existing Command 7 queue is unchanged.

The semantic claim commits only after the corresponding complete USB response
write/flush. Failed writes keep that claim paired with the engine's identical
GIP bytes and sequence. START cuts the baseline at selection, never completion;
updates during the write therefore survive. A new shared atomic lifecycle
baseline operation invalidates old producer/claim capabilities without encoding
an unpresented report. Gated pre-START/initial polls perform no Source selection.

Independent review found and corrected premature journaling behind a lifecycle
gate and lost successor edges between delivered reconfiguration/IN halt-clear
and the next poll. Live boundaries resume journaling immediately; Permit resumes
a suspended journal without resetting an already-active one. Reset/disconnect
retire both ownership layers; ordinary unlink and OUT halt-clear do not erase
history. Tests include all ordinary buttons plus Share, trigger peak/release,
1,000 coalesced motion publications, concurrent producer and USB completion,
exact retries, lifecycle/endpoint scope, and a warmed publish -> Stage ->
Prepare -> Complete cycle with **zero allocations**.

Overflow faults rather than inventing missing history. It rejects new IN
admission and permanently blocks same-incarnation broker reconnect. The current
broker closes before a later teardown Stop can necessarily be acknowledged,
so clean recoverable overflow teardown is **not yet proven**; that path can
quarantine the incarnation. No hardware artifact includes this work yet.

### Requested faster Xbox service: deterministic experiment implemented

Microsoft's full-speed GIP descriptors require IN/OUT interval values >=4 ms.
However USB 2.0 section 5.7.4 (printed page 51) permits shorter host service,
down to 1 ms for a full-speed interrupt endpoint. The independent audit fetched
the official USB-IF package anew; ZIP and PDF hashes match `PROVENANCE.md`.
The engine itself imposes no independent 4 ms input spacing; the present limit
is the descriptor-derived retained transport policy.

The experimental `--usb.retained-input-service-ms` /
`VIIPER_USB_RETAINED_INPUT_SERVICE_MS` accepts only `0`, `1`, `2`, and `4`.
Default `0` preserves descriptor timing. Invalid values fail before owner
bind/activation, and the scheduler snapshots the setting before activation.
Only retained IN service changes; descriptors, speed, identity, mapper, driver,
OUT, and EP0 remain unchanged.

Real authorized-Xbox virtual-time integration now decodes 32/64/128/256 distinct
GIP input states for its descriptor-default 8 ms / experimental 4/2/1 ms
policies. These are sample counts sized for 256 ms windows, with exact
`(sampleCount - 1) * interval` first-to-last spans. Every input's discrete and
analog values and ordering are checked through the shared journal and USB/IP
import. Descriptors remain byte-identical and one-shot teardown completes.
This is simulator evidence, not 1,000 Hz observed at a Windows consumer.

Windows measurement remains pending: observe uniquely changing states, lost/
reordered edges, p50/p95/p99/max latency, and idle/load CPU. URB counts, broker
ACKs and changing packet counters alone are insufficient. A 1 ms service period
is not a minimum processing delay; sub-1 ms software-added latency is a target
to measure, not an established result or an end-to-end guarantee.

### Change-only input, missed-wake protection, and periodic status

Microsoft MS-GIPUSB section 3.1.5.6.1.1 recommends sending input only when a
field changes. The ordinary journal now suppresses identical publications and
does not materialize the shared scheduler's unchanged idle image. Empty IN
requests park on readiness without a one-millisecond poll; explicit KeepAlive
publications and exact failed-delivery retries remain supported. Readiness is
captured before selection, preventing a publication racing with an empty
decision from being absorbed into the returned pending epoch. No new deadzone
is imposed, and this is not proof of zero idle CPU or zero ingress wake-ups.

MS-GIPUSB section 3.1.5.5.2 separately requires periodic Status Device reports.
The engine now anchors this timer to delivered START status: every second for
ten seconds, then every twenty seconds. Successful status delivery resets the
next deadline; failure retains exact bytes/sequence and does not advance it.
Late delivery does not create catch-up bursts. Idle parking retains that
deadline, and STOP/reset/disconnect retire it. Charging/battery-type immediate
status events remain open; the status-events capability blocker stays set.
Neither change has been run on hardware in this tranche.

### Ordinary feedback no longer owns the primary input lane

The source-confirmed `activeLocal` / single-engine-claim coupling is removed for
admitted Direct Motor and Guide LED actions. An exact, bounded side owner keeps
the original obligation; transfer never reports feedback delivered. Ordinary
input, Guide, periodic status, and their exact retries proceed independently.
Feedback failure retains a separate immutable retry, even with an IN response
admitted or an IN retry pending. EP0, further OUT, metadata, STOP/reset and
mandatory clears retain their ordering/fences. This does not attribute or claim
to eliminate historical 50–124 ms Windows stalls.

Delivered ordinary OUT now wakes its already-selected local action without an
unrelated IN request. Detachment signals readiness before Execute, and exact
local retries have an independent bounded wake. Review caught an epoch-only
notification that failed to signal its channel; the correction is covered by a
direct no-publication wake test plus readiness-exhaustion containment. Cold
owner construction and reset/disconnect Safe assertions recognize both owners.

Seven new transport/adapter tests cover motor and LED, 256 distinct input states
while the actual test executor is blocked, Guide/status progress, feedback
completion with IN admitted, both retries in either order, STOP/control/reset
fences, no-publication wake/exhaustion, and a zero-allocation warmed combined
path. Eleven engine tests include hidden-owner/opaque-capability mutation
matrices and the two-lane zero-allocation path. Three cold-owner tests each cover
admitted and retry states. These are software results; no new hardware build,
Windows latency measurement, physical feedback, or installer result follows.

### Reset-timeout quarantine cannot retain reopen authority

A complete-suite run failed
`TestRetainedSubmissionResetSerializerTimeoutNeverReopens`: after the outer
reset had timed out and quarantined, a delayed scheduler drain could still
finish and leave enough authority for `reopenAfterReset` to succeed. This was
not dismissed as a timing-test flake. `finishResetQuarantined` previously only
set the session flag before attempting deadline-bound cleanup; when that
deadline was already exhausted, scheduler close could be skipped entirely.

The failure path now synchronously revokes the exact reservation's activation
and closes admission before any cleanup callback. The scheduler rechecks its
deadline and activation after acquiring serialization and after retirement,
so late completion cannot publish a successful reset-drain proof. Three new
regression cases failed before the fix and pass afterward: cleanup skipped at
an expired deadline, retirement finishing past its deadline, and retirement
finishing after quarantine revocation. Successful reversible reset remains
covered separately. This repairs the dormant reset transaction; it does not
create a missing usbip-win2 whole-device-reset notification or prove hardware
reset/suspend support.

### Terminal input-history retirement: preserve feedback until acknowledged Stop

The next audit confirmed three independent teardown cuts: the Xbox broker
ended its feedback bridge on input overflow; the retained worker treated the
history error as uncertain ownership and skipped disconnect-neutral; and
DS4Windows closed the broker on rejected input, then cleared `connected`
before its existing feedback-preserving Disconnect path could run.

The VIIPER side now latches input-only retirement and signals the existing
readiness channel. An optional exact-lease `ImportRetirementOwner` query wakes
the retained worker even with no queued IN request. The request is a diagnostic
reason for irreversible teardown, not proof of Safe: every pending ticket must
retire, the executor must drain, and the original consumer must acknowledge
the exact terminal Stop. Fatal feedback, malformed capabilities, callback
panic/error, failed ticket retirement, and ambiguous completion still quarantine.
Already-admitted GIP reports complete once; the invalidated shared history is
not replayed/resynchronized, and reset/reconnect cannot reuse that incarnation.

X1BR framing/version/status bytes are unchanged. Input overflow rejects all
successor input and activation/replacement while preserving the original ACK
lane. The fault installs an absolute 30-second read/write deadline that traffic
cannot extend. Stop may precede the rejected input ACK on the duplex stream.
Neither socket expiry nor rejected/missing/wrong-correlation Stop ACK proves
neutralization.

New Xbox tests exercise the production persona, executor, X1BR reader/writer
and bridge over test-authenticated `net.Pipe` peers. They cover accepted,
rejected, wrong-correlation and lost-socket Stop; unchanged input revision;
activation/replacement/reset fences; already-admitted success/failure and
duplicate completion; all three pending lanes; fatal precedence; and an
absolute deadline test using virtual time, not a real 30-second sleep.

The independent server test combines the actual retained USB/IP handler with
the production Xbox broker/executor: after START and one changed input, it
overflows with no queued IN, observes the terminal Stop, proves registration
remains before ACK, then verifies Safe release and automatic one-shot removal
after the exact ACK. Separate server cases cover all three queued lanes,
readiness races, forged lease fields, query/retire/drain/neutral failures and a
zero-allocation warmed retirement query. This is in-process/replay evidence,
not Windows enumeration, a Bluetooth test, or physical actuator evidence.

Read-only installed-file snapshot for this follow-up (these files were not
changed): DS4Windows.exe SHA-256
`2D8DBAD4A4CFB31ADDCAE6CDF8ADA534E35EEAA44344E05E4601272400BCD063`;
`DS4Windows/VIIPER/viiper.exe`
`AD14F2C9048D61B3447F2F79D7A122EDEA81E5DB52A1AC803D294E5BC9CD2324`;
`USBip/usbip.exe` version 0.9.7.7,
`FC1660E3759D8AF4CEDE48DBE194285A5A1DE85CE6E3216724499AFD32BE92E8`.
These observations are separate from the unchanged source download pin above.

DS4Windows now handles exact negative input ACKs with a typed input-only
failure. The original writer thread and both writer/stream generations are
authenticated before electing its teardown. The existing Connect path joins
that writer before replacing the stream; concurrent Disconnect coverage tests
this join barrier. Stop already acknowledged before the rejection is retained,
while malformed, duplicated, missing or out-of-order ACKs and rejected feedback
retain their fatal behavior. Focused tests use actual writer/reader/dispatcher
code and a test physical-state owner, not actual HID/BLE writes.

**Newly confirmed prerequisite before live recovery:** the legacy DS4Windows
`ViiperClient.RemoveDevice` calls address-only `bus/{id}/remove`, then
unconditionally `bus/remove`. `VirtualBus.nextAvailableDeviceIDLocked` and
`Server.NextFreeBusID` reuse freed numeric addresses. After automatic one-shot
retirement, delayed old cleanup could therefore select a newer registration.
The internal VIIPER retirement/removal uses exact registrations and is safe;
the external legacy cleanup request does not carry that proof. The next change
must add an Xbox-specific exact-registration removal credential/operation,
preserve idempotent already-removed handling, and avoid address-only fallback
or deletion of a successor bus. Do not claim the whole recovery gate closed.

Diagnostic nuance retained intentionally: a safely removed overflow still
returns a history-loss diagnostic, which can make server shutdown report an
error. Successful cleanup is not evidence that the input run was loss-free.

### Exact registration cleanup contract and native reconnect audit

Historical snapshot before the startup integration below. Its unwired/next-
change statements are superseded by **Exact startup and public USB/IP aliases**;
the recorded prior test results and native retry warnings remain evidence.

The Xbox factory now issues a private random 256-bit `removalToken`, bound once
to the exact Add lifetime. The authenticated
`bus/{busId}/{devId}/remove-authorized-xboxone` endpoint validates its closed
version-one JSON payload and removes only the captured, matching registration.
Its server close primitive fences late import admission, cancels only matching
streams, joins the existing retained close/Stop path, and does not report
uncertain neutral or quarantine as successful removal. Dormant and completed
removal remain distinct from proof of physical Windows port disappearance.

DS4Windows has a strict matching factory-receipt parser and a real client
management primitive. Its reply buffer is limited to fewer than 1,024 bytes;
the post-connect absolute deadline includes authentication/write/all reads.
It sanitizes malformed/error responses, including echoed capability material,
without retaining a raw inner exception, and never retries or falls back to
numeric device/bus removal. The client budget is configurable; its five-second
default is **not** the server close bound (three times ConnectionTimeout,
15-second fallback). Safe live integration must reconcile those budgets.

**Live DS4Windows lifetime wiring is intentionally not changed yet.** Root and
independent audit found additional required isolation:

- Local `ActivePorts` bookkeeping and legacy detach/global/duplicate scans
  identify ports by reusable numbers, not captured lifetimes.
- Stream opening and activation still select by numeric bus/device address.
- Pinned usbip-win2 0.9.7.7 source
  `7c219953101cc5d0ec9a0bcb3eb87259cf72bedd` requests automatic reattachment on
  receiver exit (`wsk_receive.cpp:703`); the delayed attempt uses the saved
  location after 30 seconds (`device.cpp:792`, `persistent.cpp:454`). Initial
  `--once` attach does not disable that later path. Socket closure alone cannot
  prove permanent retirement, and queued cancellation is not a future-enqueue
  fence.

The next transport change is a fresh, nonsecret per-registration USB/IP alias,
distinct from the removal capability. Pinned source transports the string and
requires exact import-response equality (`vhci_ioctl.cpp:83,121`), independently
of numeric transfer IDs (`:155`). The alias must be assigned before publication,
fit the 32-byte NUL-terminated field, be used consistently by devlist/import/
native and CLI attach, and never permit stale-alias or numeric fallback to a
protected successor. This has source support without a driver change; it is
**not implemented or hardware-proven by this tranche**. Stale native retry work
and immediate port removal remain separate validation gates even with aliases.

Detailed contract, source pointers, and remaining integration invariants:
`DS4Windows/docs/protocols/xbox-one-registration-removal-v1.md`.

Final C# validation after bounded I/O: 82 new focused cases passed (620 ms);
full Release/x64 suite passed 2,467, skipped 3, failed 0 (2,470 total; 22 s).
The 82 focused cases also passed five consecutive `--no-build` repetitions.
These include actual loopback requests, identity/schema cases, no fallback,
socket reset, and dribbling data across an absolute deadline. No controller,
portable executable, installer, association, driver or Program Files mutation
was exercised for this tranche. Goal remains incomplete; this turn is progress
through implementation plus evidence that changes the required next action.
Read-only SHA-256 rechecks of installed DS4Windows, VIIPER and usbip.exe exactly
match the three installed-binary hashes recorded earlier in this ledger.

Final server verification after the completed-failed-close bookkeeping fix:

- `go test ./... -count=1 -timeout=90s`: all packages passed.
- `go vet ./...`: passed.
- `go test -race ./device/xboxone ./internal/server/api/handler ./internal/server/usb -run 'ProductionRemoval|XboxOneRemoval|RetainedExactRegistration' -count=20 -timeout=90s`:
  passed (Xbox 5.484 s, API handler 1.915 s, USB 2.962 s).
- Independent affected-package full race run passed once. Its broader
  three-repetition run was **not clean**: generic handler fixtures reuse global
  bus allocations without releasing them between repetitions (including
  unchanged `bus_devices_list_test.go` cases 60008/60009/60010). No clean full
  repeated-suite claim is made; the new exact-removal cases pass twenty race
  repetitions independently of those fixtures.

Root review found and deterministically reproduced retained cleanup-record
leaks after failed close followed by registration cancellation, in both
cancellation orderings. Completed close records now retire only after exact
registration cancellation and all matching streams have joined; pending
closers remain protected. Actual rejected/wrong-correlation/socket-loss tests
also verify that removing bookkeeping does **not** release the same quarantined
owner session. These are server bookkeeping guarantees, not hardware neutral
or native reattachment guarantees.

### Exact startup and public USB/IP aliases: integrated, software verified

The later integration closes the source-level gaps above without changing the
canonical input/feedback engines or implementing a new driver:

- Production registration assigns a fresh public `x1-` + 26-character canonical
  lowercase base32 alias from 16 random bytes, independent of the 32-byte secret
  removal token. It is installed before descriptor admission/publication.
  DEVLIST, IMPORT reply and native/CLI attach preserve it; numeric bus/dev fields
  continue to define transfer identity. Old aliases and numeric imports cannot
  select a protected successor, including after bus reuse or server restart.
- Factory receipts include the exact alias and bounded actual close budget
  (`ceil(3 * effective ConnectionTimeout)` ms, 1..300000). Unsupported budgets
  fail before production construction rather than being clamped. Positive
  configured empty-bus cleanup now includes the initial empty incarnation;
  lost/rejected creation needs no numeric client bus deletion.
- Stream opening and activation require the same closed version-one token
  payload as removal. Duplicate streams cannot displace the sole consumer.
  Exact-registration checks guard broker acquisition, ConsumerReady and both
  sides of native activation. Buffered/paused old requests fail after removal
  or reuse. Activation returns version, exact alias, positive port and empty
  owner serial for the pinned native ABI.
- Independent review found a real registration/coordinator lock-order hazard
  during implementation. The short registration lease is now released before
  claiming the stream coordinator; exact incarnation keys and final admission
  checks preserve stale safety. A deterministic test runs a real registration
  removal callback while the competing claim advances. No registration lease
  spans network/native attach I/O.
- DS4Windows now uses the captured receipt for startup, activation and lifetime
  removal. Bus/create, factory receipt and broker-ready handshakes have bounded
  absolute post-connect I/O. The lifetime uses the advertised removal budget
  plus 2000 ms. Strict bus/receipt/activation parsers reject ambiguous identity;
  the management reader rejects replies reaching 1024 bytes and sanitizes
  echoed token/error data. A failed unreturned stream releases its authenticated
  wrapper and wait handle as well as the socket.
- Concurrent lifetime disposers join the original cleanup before closing the
  feedback broker. Conditional local port leases cannot unregister a successor.
  Xbox cleanup never issues bare-port detach or numeric device/bus removal;
  the entire `x1-` namespace stays outside legacy stale/duplicate scans even
  before local binding or after local disposal. A cold-path lock coordinates
  explicit attach and legacy detach within this DS4Windows process only.
  No new registration/token checks, topology scans, clocks or locks were added
  to the per-report input hot path.

Root review and independent C# review found no remaining confirmed integration
blocker in this change. This is **not** proof of permanent native retirement:
the pinned driver's PLUGOUT compares only the reusable port; STOP_ATTACH_ATTEMPTS
matches a 32-bit location hash, cancels currently queued jobs only and returns
no producer/timer drain fence. The unplugged event precedes retry enqueue. The
exact source locations and licensing remain in `device/xboxone/PROVENANCE.md`.
No bare-port, hash-only cancel or VHCI-wide workaround was added. Aliases keep
old retries from acquiring a successor; their native resource retirement is
still unproven. A guaranteed prompt native retirement policy would need a
separately reviewed narrow driver primitive/lifetime change, not completion of
the unrelated native presentation backend.

Verification on the combined current worktree:

- Root `go test ./...` passed all packages; `go vet ./...` passed.
- Independent full affected race run passed:
  `go test -race ./usbip ./virtualbus ./device/xboxone ./internal/registry ./internal/server/usb ./internal/server/api/... ./internal/cmd -count=1 -timeout=90s`.
  Startup/activation/lock-order race tests passed ten repetitions, and focused
  alias/reuse/receipt/attach tests passed twenty race repetitions. The earlier
  synthetic GIP-ID mistake in the new lock test was corrected before these
  final runs; it did not represent a production change.
- Root focused C# registration/startup tests passed 160/160. These include
  actual loopback bus/create, token stream + X1BR Ready, activation and removal;
  invalid bus/factory/activation responses; authentication/Ready failure;
  dribbling Ready bounded by the five-second absolute deadline; disposal racing
  activation; concurrent cleanup; exact local port lease protection; and no
  numeric fallback or in-place reopening.
- Full Release/x64 DS4Windows passed **2545, skipped 3, failed 0 (2548 total)**
  with the saved result `DS4WindowsTests/TestResults/xbox-exact-startup-full-20260902.trx`
  (28 s). Three additional full `--no-build` runs passed the same counts (27 s
  each), with `xbox-exact-startup-repeat-{1,2,3}-20260902.trx` results.
  The initial quiet full run reported **one unidentified failure** (2544 passed,
  3 skipped); it emitted no failed test name/details and is not retrospectively
  called green or attributed to an unrelated test. The unchanged compiled
  suite passed the four subsequent saved full runs and focused rerun. The
  unidentified failure remains a flakiness risk, not proof that every run is
  stable. Future full verification should retain TRX/visible failure details.

Full client verification command (use a fresh log name for later runs):

```text
dotnet test DS4WindowsTests/DS4WindowsTests.csproj -c Release -p:Platform=x64 --no-build --no-restore --nologo --verbosity minimal --logger "console;verbosity=minimal" --logger "trx;LogFileName=xbox-exact-startup-full-20260902.trx"
```

No hardware was opened, no portable app was run, and no installer, driver,
association or Program Files state changed in this integration pass. Read-only
hash checks still match all three installed binary hashes recorded above.
This is progress through implementation and verification; the full active goal
is unchanged and incomplete.

### DJG IR/multiple-input activation and profile-editor refresh

Implemented and software verified, not hardware verified. Both repository
HEADs and branches above are unchanged; pre-existing dirty/untracked work was
preserved. No VIIPER runtime code, installed application, controller connection,
driver, Bluetooth association, security policy or installer was changed in
this follow-up. The installed `DS4Windows/DS4Windows.exe`,
`DS4Windows/VIIPER/viiper.exe` and `USBip/usbip.exe` hashes were read again and
match the three values in the input-history follow-up above; USB/IP reports
0.9.7.7. There is also a separate `Program Files/VIIPER/viiper.exe`; it is not
the DS4Windows-bundled VIIPER artifact and was not modified.

The independent parity audit found missing per-side IR and multi-input DJG
activation against Switch2Connect `61ac6642ce12fe7217e38a860b14863b18ca7e28`
(GPL-3.0), `src/controller.py` mapping pairs, aggregate `trigger_djg`, and
`prev_djg` transition handling at 4369-4405, 4934 and 5356-5360. The canonical
DJG resolver now accepts known physical/IR flag combinations as a per-side OR
gate, not a chord. Existing enum values and XML field names are unchanged.
The runtime derives IR only from the present, matching Common05 source and
existing activation-threshold predicate, without optical pointer motion
verification. It clears supplied IR bits before synthesis and never changes
the physical source sidecars or consumes game buttons. No worker, mapper or
polling loop was added.

An additional reproduced defect allowed an inherited Hold release to switch
gyro mode although its press had only been baselined. Per-side admitted-press
tracking now prevents that unmatched release. Relevant IR threshold changes,
profile-switch revisions (even identical DJG settings), pair epochs, and
invalid-configuration recovery baseline held input. Other-side presses remain
independent, including simultaneous changes.

The DJG editor is a dedicated notifying child under Switch 2 Controls, with
multiple activation checkboxes per half and corrected dominant-side refresh.
Adversarial review then found constructor-cached selections survived saved
profile/preset loads: the real editor creates its child models before loading
the profile and reuses them afterward. `UpdateLateProperties`, called while
bindings are detached, now refreshes DJG, gyro lock, mode shift, trigger tuning,
and the four direct IR tuning checkbox lists. Reads/refreshes do not create
missing lock/tuning tables; explicit user edits still create them. Tests cover
existing-editor refresh/rebind, independent masks, loaded table identity and
contents, null tables, cleared presets, and retained mode/trigger selection.
The independent reviewer found no further blocker in these changes.

Evidence (Release x64, no hardware/portable application launch):

- Before edits, the focused mode/runtime/schema suite passed 67 tests.
- Five added reproductions failed before their fixes: two unsupported OR/IR
  cases and inherited-Hold release in all three modes. Results retained in
  `DS4WindowsTests/TestResults/djg-red-20260902.trx`.
- Expanded DJG/runtime/schema/STA binding suite passed 93 tests, including all
  three modes x Hold/Toggle x left/right, handoffs, simultaneous sides,
  stationary IR with pointer disabled, button overlap, profile/threshold
  boundaries, foreign/absent IR and XML single/combined masks.
- The final focused set including adjacent profile editors passed 129 tests
  (`djg-refresh-20260902.trx`), with actual L/R raw-bit overlap replacing the
  earlier synthetic paddle case. IR observation plus mode resolution allocated
  zero bytes across 20,000 warmed iterations; that is not a latency benchmark.
- Full `dotnet test DS4WindowsTests/DS4WindowsTests.csproj -c Release
  -p:Platform=x64 --no-restore --nologo --verbosity quiet
  --logger 'trx;LogFileName=djg-final-full-20260902.trx'` passed **2,572**, with
  **3 skipped, 0 failed (2,575 total; 28 s)** after the shared refresh fix.
  Compiler warnings remain. Full-UI visual and live-controller tests are not
  implied by the STA binding test.
- A second complete run after adding nondefault mode/trigger refresh assertions
  again passed **2,572, with 3 skipped and 0 failed (28 s)**; retained results:
  `djg-final-repeat-20260902.trx`. Scoped tracked-file `git diff --check` passed.

This audit also disproved broader input-parity assumptions (subsequently
corrected in the rail follow-up below): joined/vertical
Joy-Con source mapping reads Pro GL/GR bits 25/24 and omits Joy-Con rail bits
21/20 (left) and 5/4 (right). Horizontal mode instead remaps these into normal
shoulders and reuses paddle semantics for L/ZL; renaming labels cannot correct
physical provenance. See Switch2Connect `controller.py:4265-4273` versus
`Switch2JoyConProfileInput.MapCombinedLeftButtons`, `MapCombinedRightButtons`
and `MapMiniLeftButtons`. This required a canonical correction with
raw-bit golden tests and preservation of horizontal legacy output. The new
runtime overlap tests use actual L/R bits 22/6; they do not claim rail support.

### Physical rail identity, gyro activation and command aliases

Source-pinned follow-up on the same DS4Windows HEAD
`061fab1304e77c995ce9451b7ef20e51cc870070` plus preserved dirty worktree;
VIIPER HEAD remains `54f7853aa4f97394298fc8f4c3ebb865237a3456`. No VIIPER runtime,
driver, installation or active-controller change was made in this follow-up.
The last question-only turn made no implementation progress; continuation
resumed the unfinished rail correction rather than treating that explanation
as completion.

Switch2Connect `61ac6642ce12fe7217e38a860b14863b18ca7e28`,
`src/controller.py:4265-4273` (GPL-3.0) is the explicit physical-rail authority:
left SL/SR raw bits 21/20, right SL/SR 5/4, Pro-only GL/GR 25/24. Pinned SDL
mini-controller roles remain compatibility evidence. No PadForge code was
copied. New in-process Joy-Con contract version 3 carries append-only physical
rail flags and `DS4Controls` IDs 63..66. Legacy horizontal shoulder/paddle
bindings remain intact; joined/vertical no longer invent paddles from Pro bits.

C and rails now have ordinary gyro activation tokens 30..34, with 64-bit edge
masks. Always On remains activation -1 and tuning ID 29. All four gyro menus
persist explicit tokens, refresh checked states on reload and expose invalid
saved tokens as unsupported without silently rewriting the profile. Direct
shift triggers append rails 38..41 and C42, preserving synthetic Mode Shift37.
DJG editors filter opposite-side rails; shared tuning/lock settings retain both
sides. Exact-version source selection also repairs stale Joy-Con metadata
winning over a current Pro source in the gyro modifier reader.

Independent adversarial review found a real new alias interaction: an explicit
horizontal rail Mode Shift command suppressed its dedicated mapping but leaked
the legacy shoulder. Two real-mapper regressions failed before the correction
(`rail-command-alias-red-20260902.trx`). Command consumption now treats
horizontal SL/L1 and SR/R1 as one physical family in either direction, only
under an exclusive current standalone-horizontal source with valid topology
and generation identities. Hold wins over Toggle across that family without
altering the saved settings. Ordinary custom rail mappings still coexist with
L1/R1; the editor explains how to unbind those defaults. Raw source data is not
mutated, and no worker, sleep or second mapper was introduced.

Verification in Release x64:

- Corrected raw rail/source/runtime/schema/gyro/editor suite: **151 passed**,
  `rail-gyro-integration-20260902.trx`. Earlier failing raw-rail and integration
  results remain retained rather than rewritten as passing history.
- Real Common05 bytes -> canonical projection -> sidecar -> `Mapping.MapCustom`
  -> exact semantic packet button bits covers all six virtual types, four
  rails, three orientations, baseline/press/held/release/repress and six ordinary/
  Hold/Toggle/alias-conflict configurations. No actual virtual device is created;
  a rejecting system-input stub prevents mouse/keyboard output.
- Actual Mouse/MouseJoystick high-ID Hold/Toggle and tuning edges; XML tuning
  IDs 29..34 in both scopes; all four menu token-save/reload paths; absent,
  stale, ambiguous and wrong-topology source checks. Warm gyro evaluation and
  command alias normalization each allocate **0 bytes / 20,000 iterations**.
- Focused independent run: **60 passed**, `rail-command-gyro-independent-20260902.trx`.
  A test-only WPF initialization-order problem was exposed when the menu tests
  ran alone; constructing the WPF control before its resource-loading view model
  removed that dependency on other tests. Intermediate failures are retained.
- Initial full run found one additional coordinator fixture using raw Pro GL;
  that fixture now uses actual left SL. Final complete command:
  `dotnet test DS4WindowsTests/DS4WindowsTests.csproj -c Release -p:Platform=x64
  --no-restore --nologo --verbosity quiet --logger
  'trx;LogFileName=rail-gyro-final-full-20260902.trx'`: **2,611 passed, 3 skipped,
  0 failed (2,614 total; 27 s)**. Compiler warnings remain. `git diff --check`
  passed for tracked changes; existing line-ending notices remain.
- Read-only hashes of the installed DS4Windows executable, its bundled VIIPER
  executable and USBip executable still match the earlier snapshot. No portable
  app, hardware probe, driver installation, pairing operation or game was run.

These are software correctness assertions, not radio, physical actuator,
consumer-latency or game-compatibility evidence. The full objective is active.

### IR command consistency and orientation-boundary corrections

The intervening sub-1-ms question turn made no implementation progress; it
rechecked existing evidence and did not establish a new latency result. This
continuation revalidated the two outstanding source-review findings before
editing. Repository HEADs and installed executable hashes remain the values
above; all existing dirty changes are preserved. No hardware or app was started,
and no driver, association, installed binary or VIIPER runtime was changed.

- IR Mode Shift previously read the mutable field map, so consuming an IR
  command changed later bindings within the same report and manufactured held
  Toggle represses. `rail-ir-order-red-20260902.trx` reproduced all four
  left/right Hold/Toggle cases. `Mapping.ResolveSwitch2ModeShift` now reads the
  immutable source through the shared current-version/presence/threshold
  validator. Balanced/Relaxed per-side settings and stale/ambiguous/absent
  source rejection have explicit tests.
- Source identity omitted Joy-Con orientation. Same raw held SL/SR acquired a
  logical L1/R1 alias on rotation and could create a Toggle edge. Four raw
  Common05 -> profile projection -> real `Mapping.ShiftTrigger` cases failed
  in `rail-orientation-red-ir-green-20260902.trx` (the four IR cases passed).
  The in-process identity now includes layout, making existing Mode Shift,
  gyro-lock and tuning reducers baseline/reset at that boundary. Transport
  lifetimes and packet contracts do not change.
- Ordinary Controls/Mouse/Mouse Joystick toggles did not use that identity and
  needed their own correction. Four raw-source cases failed in
  `rail-ordinary-gyro-orientation-red-20260902.trx`. Fixed-size per-mode
  observers now intercept only a proven same-lifetime standalone layout switch,
  baseline raw activation and pressed masks, and refresh tuning selection.
  They retain the user's gyro activation latch, immediate Hold and Always On
  behavior. They do not claim to change unrelated reconnect/lifecycle policy.
  Directional swipes have no toggle latch and were not changed.
- The raw-input matrices cover both sides and rails, horizontal/vertical
  transitions while held, actual release/repress, repeated per-report queries,
  interleaved output modes, active/inactive toggle latches, Hold and Always On.
  Additional tests cover tuning freeze/release and gyro-lock boundaries,
  invalid/unproven observer topology and lifetime changes. The high-ID gyro
  tests now represent releases by clearing only button fields on one valid
  identity, not by deleting and recreating the source sidecar.
- Focused Release x64 run `rail-gyro-boundaries-hardened-20260902.trx`:
  **123 passed, 0 failed**. Warm actual ordinary activation with repeated layout
  changes allocated **0 bytes across 20,000 iterations** in each of the three
  toggle-capable modes. No worker, timer, sleep, I/O or second mapper was added.
- Independent read-only review confirmed both original defects were addressed
  and found no further reachable production blocker in this change. Its final
  test/guard suggestions were incorporated: standalone observers reject
  generation fields on an absent half; eight mixed-token tests assert actual
  new-layout tuning indices in Mouse/Mouse Joystick; IR fixtures declare joined
  mode. Actual IR observation -> command consumption -> later observation also
  allocates **0 bytes / 20,000 iterations** for each side's Hold configuration.
- Final focused run `rail-gyro-boundaries-final-focused-20260902.trx`:
  **131 passed, 0 failed**. Full run before final defensive hardening:
  `rail-gyro-boundaries-full-20260902.trx`, **2,635 passed, 3 skipped**.
  Final complete command from DS4Windows:
  `dotnet test DS4WindowsTests/DS4WindowsTests.csproj -c Release -p:Platform=x64
  --no-restore --nologo --verbosity quiet --logger
  'trx;LogFileName=rail-gyro-boundaries-final-full-20260902.trx'`:
  **2,643 passed, 3 skipped, 0 failed (2,646 total; 28 s)**. Existing compiler
  warnings remain. Scoped tracked `git diff --check` and a trailing-whitespace
  scan of the changed untracked code/tests/docs found no whitespace errors.

These results establish software behavior only. The full physical/Windows/game,
latency, fidelity, feature-parity and installer gates below remain open.

### Mapping-owned stick precision and temporal filters

The earlier sub-1-ms question-only turn rechecked evidence: **no implementation
progress or new Windows latency result**. This continuation revalidated the
current source and continued the explicit no-12-to-8-to-16 input requirement.
The preceding implementation turn was progress: its mapped-axis/source/copy,
rotation and drift changes remain in the worktree and its TRX counters were
re-read below rather than assumed from the earlier response.
HEADs remain DS4Windows `061fab1304e77c995ce9451b7ef20e51cc870070` and VIIPER
`54f7853aa4f97394298fc8f4c3ebb865237a3456`, both on
`feature/native-udecx-landing-zone`, with origin remotes under
`https://github.com/hbashton/`. Existing dirty work is preserved. Installed
DS4Windows/VIIPER/USBip executable SHA-256 values re-read at this continuation
match the recorded baseline. No application, hardware test, association,
installed-file change, driver work, commit, push or release was performed.

The source gap is real: Pro/JoyCon parsers retain raw 12-bit and signed-16
values, but the existing byte-only profile mapper reduces those before Xbox
and virtual Switch output expand them again. Recovering raw metadata at
egress would bypass mappings and is explicitly prohibited. The current
foundation is **not yet an end-to-end precision fix**:

- `DS4MappedStickAxis` owns the mapped fractional coordinate. Pro/JoyCon
  projection seeds the logical calibrated value. Byte setters deliberately
  replace precision even on equal-byte writes; constructor/CopyTo retain it,
  while CopyExtrasTo cannot restore source controls over a remap.
- Existing rotation and profile drift-offset equations preserve fractions
  for precise input and historical integer behavior for legacy input.
- Sensitivity and square-stick wrappers now consume the same typed values;
  the square-stick trigonometric core is unchanged. Legacy truncation remains
  at each historical write, not merely at pipeline exit. New tests cover all
  256-by-256 byte coordinates for three square roundness settings, every
  signed-16 input across five sensitivities, mixed/coupled precision,
  sub-byte center movement, chained legacy truncation and zero warm allocation.
  Joined JoyCon projection additionally preserves every raw 12-bit position
  across all four typed mapped axes, including positions sharing a byte.
- Existing anti-snapback/fuzz now use the shared typed value. The former
  closure/expanding queue is replaced with a bounded 8,192-sample per-stick
  history. Overflow bypasses suppression until lost history expires, while
  fresh samples keep collecting; another overflow extends the boundary.
- The sole mapping owner applies source-object, profile, generation, pair,
  presence, orientation and rotation fences. UI/profile resets publish an
  atomic request; they do not mutate the input-owned history. Controller
  identity is weakly held, and both the production report and readings-preview
  caller pass the actual owner. Timing is QPC-derived monotonic milliseconds.
- Independent review caught and corrected an intermediate overflow-recovery
  hole, a coarse-clock choice, and the missing preview source-owner argument.
  Their source tests cover the corrected boundaries. No byte truncation was
  found inside the final filter reducers. Invalid filter settings are bounded
  to the UI contract, deliberately replacing unchecked malformed XML behavior.
- Wider egress remains unchanged. Deadzones, output/custom Bezier curves,
  field-map/remap, gyro/macro/OSC composition and
  target encoding still require the full migration documented in
  `DS4Windows/docs/protocols/mapped-stick-axis-precision.md`. The legacy
  signed-output rounding matrix is a mandatory pre-adoption gate.

Revalidated earlier artifacts: `mapped-axis-ownership-full-20260902.trx`
reported 2,654 passed/3 skipped before drift/filter additions, and
`mapped-axis-transforms-focused-20260902.trx` reported 125 passed. The first
filter run `mapped-stick-filters-first-20260902.trx` had 88 passed/1 failed:
the failure compared a decimal double with exact equality (60.99 versus
60.99000000000001), corrected with a 1e-12 test tolerance. Production behavior
was not changed to satisfy that assertion.

`mapped-stick-filters-green-20260902.trx`: **140 passed, 0 failed** using
Release x64, with filters/carrier/Pro/JoyCon/runtime/VIIPER packet-builder
filters. Coverage includes all signed-16 carrier values, adjacent source
positions, all 256-by-256 legacy fuzz inputs across five thresholds,
independent original anti-snapback sequence oracle, fractional thresholds and
holds, inclusive expiry, overflow recovery, enable/reset/generation boundaries,
real `SetCurveAndDeadzone` filter integration, and zero managed bytes across
20,000 warmed owner/filter iterations.

CPU-only maximum-window observations are intentionally not controller latency
or a performance guarantee. The first run measured mean anti-snapback-only
costs of 5.43/12.60/20.49 microseconds per report for 500/1,000/8,000-sample/s
synthetic histories, each with a 1,000-ms window and 256 measured reports.
The retained history still entails linear scans. Full Windows/consumer and
load-tail measurements remain required.

Source/API compatibility caveat: the public stick byte fields became
properties, and unused public filter-history implementation fields were
removed. Rebuild consumers; old compiled field consumers are not binary
compatible. No supported repository consumer was found relying on that ABI.

### Independent DualSense-to-Switch-2 conversion controls

The profile editor now exposes separate default-on audio-haptic and adaptive-
trigger HD-rumble conversion preferences, following the authored settings in
Switch2Connect `61ac6642ce12fe7217e38a860b14863b18ca7e28` (GPL-3.0-or-later).
XML defaults, absent-key migration, reset and view-model properties are covered.
This reuses the existing DS4Windows translators and sole physical writers; no
donor transport, bundled binary or PadForge code was copied. See
[`switch2-dualsense-conversion-policy.md`](../../../DS4Windows/docs/protocols/switch2-dualsense-conversion-policy.md).

Live edits retain ordinary body rumble and any still-enabled component within
the original absolute expiry, without resurrecting removed media on re-enable.
PCM cannot bypass its disabled preference through the compatibility fallback.
Supported adaptive-trigger effects remain an approximation on Switch 2, which
has no resistance actuator; the setting does not establish lossless fidelity.

Review found and corrected stale-cache/publication, stream-recovery, delayed
queue and failed-admission boundaries. The final lane uses an atomic exact
session/publication watermark, profile and stream fences, and cleanup-only
receipts after failed clock/source/refresh attempts. Such receipts can request
Neutral but cannot replay old media or affect newer unrelated publications.
Delayed media has a final no-I/O admission guard; a new policy/stream selection
replaces the old queue before fresh media is appended. Thus rejecting a stale
predecessor cannot discard a valid post-change successor. Already-admitted
transport writes are not retroactively cancelled.

Profile application can run on the input queue, so its hook only wakes the
existing feedback worker. Cleanup and publication remain on feedback/control
paths, outside the callback-admission lock. No new input worker, polling timer,
per-report closure, feedback wait or controller I/O is introduced on input.
Final independent source review found no remaining blocker in these changes or
the sensitivity/square-stick wrappers. This is not hardware certification.

### Central verification of the mapping and conversion changes

All runs use Release x64. The expanded focused suite covers carrier/source
projection, temporal filters, sensitivity/square transforms, conversion policy,
rich haptics, BLE/owned-USB feedback owners and rumble delay:

- `mapped-transforms-feedback-focused-20260902.trx`: 167 passed/1 failed.
  The new owned-USB expired-refresh assertion incorrectly required fully zero
  groups. The decoded report had zero amplitudes with the existing legal SDL
  compatibility carrier/control fields. The test now checks all six subframe
  amplitudes and the exact expected neutral encoding; terminal Stop assertions
  remain unchanged. No wire production change was made for this assertion.
- `mapped-transforms-feedback-finalfocused-20260902.trx`: 169 passed, before
  the final failure-receipt and delayed-queue regressions.
- `mapped-transforms-feedback-reviewed-20260902.trx`: **178 passed, 0 failed**
  after all reviewed production/test changes were frozen.
- `mapped-sticks-conversion-policy-full-20260902.trx`: 2,706 passed/1 failed/
  3 skipped. The failure was the existing
  `Switch2PcmTranslationAllocatesNothingAfterWarmup` assertion: 3,912 bytes
  observed against an expected zero. Source inspection found no explicit
  allocating operation on its valid translation path, but the allocation's
  origin has **not been established**.
- `pcm-allocation-isolated-20260902.trx`: 6 passed using the unchanged built
  translator test class. No assertion was relaxed and no production code was
  changed in response to the allocation failure.
- `mapped-sticks-conversion-policy-full-repeat-20260902.trx`: **2,707 passed,
  0 failed, 3 skipped** using the same built binaries. The isolated and full
  repeats did not reproduce the allocation failure; they do not explain it.

The three skips are the opt-in live process-loopback activation, audible-PCM
and sustained-delivery tests. They were not enabled for this CPU/simulation
pass. The non-reproduced allocation observation remains an evidence caveat,
not a proven runtime cause or a fixed regression. Independent read-only review
found no reachable allocation or mutable test-order dependency in this valid
scalar/value-type translation path. Keep the zero-byte assertion; if the
failure recurs, collect allocation-stack/runtime evidence on the same binary.
Installed DS4Windows, VIIPER
and USBip executable hashes were rechecked and still match the baseline.
These tests do not establish new Windows consumer latency, actuator fidelity,
game usability or installer readiness.

### Independently verified remaining Switch2Connect parity gaps

Read-only source audit and main-agent verification use Switch2Connect
`61ac6642ce12fe7217e38a860b14863b18ca7e28`, `LICENSE.md` (GPL v3); no PadForge
code was copied. These are explicit remaining requirements, not optional
substitutes for the user's full-parity objective:

- **Per-physical-unit raw stick center/travel calibration**:
  `src/gui.py:2349` (`JoystickCalibrationWizard._finish`) collects center and
  separate positive/negative X/Y travel, stores physical-controller keys and
  applies them; `src/controller.py:3961` adopts these overrides. DS4Windows
  currently has factory/user calibration records (`Switch2InputSession`) and
  profile byte drift offsets (`StickCalibrationWindow`), but lacks that local
  raw-travel collection/persistence feature. Implement bounded raw-12-bit
  sampling and identity-bound local overrides at the existing calibration
  boundary, with no controller flash writes or second mapper.
- **Controller-operated confirm/cancel profile picker**:
  `src/controller.py:6754` navigates with stick/D-pad under held orientation,
  with A/B confirmation/cancellation. Existing DS4Windows direct/cycle profile
  actions are not that feature. Reuse generation-fenced profile switching and
  suppress picker input from game output during the interaction.
- Independent native DualSense audio-haptic/adaptive-trigger conversion
  controls were another verified customization gap. Their source implementation
  and software verification are now recorded above; physical output/fidelity
  validation remains open with the rest of the controller matrix.

## Shared mapping-to-egress precision continuation

The preceding continuation was **progress**: it implemented and verified the
temporal filters, sensitivity/square wrappers and independent DualSense-to-HD
conversion controls. This continuation carries those mapping-owned values
through the remaining profile equations and the virtual-pad encoders. The
user's priority remains correct operation and prompt delivery of each fresh
update, not a nominal rate claim or an unrelated native-backend project.

Both repositories remain on `feature/native-udecx-landing-zone`, DS4Windows
`061fab1304e77c995ce9451b7ef20e51cc870070` and VIIPER
`54f7853aa4f97394298fc8f4c3ebb865237a3456`, with the existing uncommitted work
preserved. No installed application/driver/configuration, controller hardware,
Git history, remote branch or release was changed during this source/test pass.

### Profile, routing and wire ownership

- One `DS4StickProfileTransform` helper now implements the original left/right
  radial/axial deadzone, anti-deadzone, max-zone/output, vertical-scale, outer
  binding and output-curve math. Each side retains its own settings and original
  stage order. Legacy casts remain at their historical sites; precise values
  stay fractional, with coupled precision for radial/square operations.
- A compiled immutable continuous Bezier evaluator is captured once per coupled
  precise pair. The existing byte LUT is not interpolated. Review exposed a
  near-flat X derivative numerical error; analytic cube-root inverses and stable
  centered residual/Y evaluation address the demonstrated case. Legacy LUT
  generation remains unchanged, including its existing concurrent-edit caveat.
- `DS4StateFieldMapping.axisdirs` now owns typed axes in one backing store.
  Population, directional remapping, explicit reset/suppression, ordinary byte
  macro/button replacements and final population preserve ownership. Precise
  direction reversal uses signed magnitude with asymmetric ranges around 128;
  legacy reversal keeps `255-value`. Rebuild consumers of the changed public
  field/property/store ABI; indexer/Length compatibility is not array ABI parity.
- Neutral OSC no longer rewrites an existing stick through its byte projection.
  Actual-coordinate angle/activation/mouse reads and Switch2 stick-scroll,
  direction-tap and assist lane inputs retain sub-byte movement.
- Review found that retaining a physical winner in the deferred gyro lane could
  replay that physical position after release. The lane now retains only its
  own gyro contribution; strongest-contributor comparisons use actual mapped
  coordinates. Pending accumulation/reset concurrency is reviewed separately
  because legacy joined Joy-Cons can produce into a shared slot from two input
  threads; it must not be treated as single-threaded Switch2 runtime state.
- Xbox360/One and Switch2 encoders now quantize the final mapped coordinate to
  signed-16/unsigned-12 respectively, with exact historical conversion for legacy
  byte inputs and unchanged final steering precedence. Sony targets retain their
  actual 8-bit output. Raw physical metadata cannot restore overridden input.

This introduces no input polling timer, intentional batch delay, native driver,
or feedback I/O on input. Small updates that share a compatibility byte now
remain distinct semantic Xbox states. That is correctness/precision evidence,
not a new host-service, physical latency or game-consumption result.

### Verification history for this continuation

- `wide-profile-first-20260902.trx`: 56 passed, before the numerical edge-case,
  fractional lane, routing and egress follow-ups.
- `wide-profile-routing-20260902.trx`: 76 passed, 0 failed, including corrected
  continuous Bezier, fractional lanes and initial routing coverage.
- `typed-egress-first-20260902.trx`: 134 passed, 1 failed. The new all-12-bit egress
  integration test passed a null `DS4StateExposed` into field mapping, causing a
  fixture NullReferenceException. It now supplies the normal exposed-state
  wrapper. No production null-handling change was made for that test defect.
  This run predates final accumulator concurrency changes and pipeline tests.

- `typed-egress-reviewed-20260902.trx`: **166 passed, 0 failed**, after the
  production/test set was frozen. This includes 14 production profile-chain
  cases, 17 concurrent/real gyro-touch cases, every signed-16 Xbox wire value,
  the repaired all-12-bit field-map/egress fixture with enabled debounce copy,
  all legacy byte encoder values, valid stale raw metadata rejection-by-
  ownership, and unchanged unselected steering axes.
- VIIPER `../_toolchains/go1.27.0/go/bin/go.exe test ./... -count=1
  -timeout=90s`: every package passed on the current source (9.0 s wall time).
  No VIIPER Go production code changed in this continuation.
- `typed-egress-full-20260902.trx`: **2,797 passed, 0 failed, 3 skipped**
  (2,800 total; 29 s), Release x64, after the final source/test freeze. The
  skipped tests remain the three opt-in live process-loopback audio tests.
  The prior PCM allocation observation did not reproduce in this run; its
  original cause is still not established. Targeted tracked-file
  `git diff --check` passed. These results are software-only evidence.
- `postmap-stress-1-20260902.trx` through `postmap-stress-5-20260902.trx`:
  five unchanged-binary repetitions of the 17 accumulator/real-producer tests
  all passed (85 total executions, no failures). This includes adversarial
  producer/reset ordering, not an end-to-end latency stress measurement.

The accumulator now captures an immutable operation epoch before gyro profile
activation/calculation; its final admission cannot be relabeled by another
producer. `CaptureEpoch` is a volatile scalar read. Reset/submit/consume use a
dedicated, scalar-only gate needed for the demonstrated legacy two-writer
sharing. It holds no profile calculation, callbacks, I/O, logging or allocation.
Public gyro arrays are compatibility mirrors only; private current gyro and
pending contributors share the same authority. Old submit/neutral operations
cannot overwrite a post-reset successor. Slot-specific yaw/roll selection also
now reads `deviceNum`, correcting a hard-coded slot-zero lookup.

This gate does **not** have a proven wall-clock acquisition bound: preemption
or contention can extend it, and tail measurements remain required. It also
does not repair the separate pre-existing legacy joined-controller shared
`MappedState` construction/presentation race outside the accumulator. Do not
use these focused tests as proof of whole-report concurrency or hardware
correctness. Installed executable hashes were rechecked and still match the
baseline. Prior non-reproduced PCM allocation evidence remains open as recorded
above; no zero-allocation assertion has been weakened.

Final independent read-only review found no confirmed blocker in the frozen
accumulator change, continuous curve correction, profile pipeline, or typed
encoder adoption. It explicitly did not certify upstream source-lifecycle
delivery before callback entry, the separate legacy whole-report race, or
contention-tail latency. No production source changed after the final central
test freeze. The full goal remains active and incomplete; this continuation
is **progress**, not release readiness.

## Joined-controller source-lifetime continuation

The user's priority remains functional controls and prompt controller-to-virtual
pad delivery, not an unmeasured polling-rate headline. This continuation is
source implementation/audit plus software tests only; it does not install or
launch controller applications or change devices, Bluetooth, drivers or the
Program Files installation.

### Legacy peer association

The original Joy-Con `JointDeviceSlotNumber` getter could dereference a peer
field after concurrent removal nulled it. It now captures a volatile reference
once. The secondary report also reuses its captured `jointInd` for gyro-mode
lookup instead of reading the association again. The six legacy pairing-removal
callbacks now compare/exchange only their exact expected peer: a late old-peer
removal cannot detach a different successor at the same numeric slot.

`joycon-peer-association-20260902.trx` passed **4/4**, including exact/idempotent
detach, null rejection, same-slot successor preservation, concurrent
detach/replacement/lookup, and warmed zero-allocation access. Independent
read-only review found no blocker in this narrow change or the six callback
captures. Same-object re-pair generations, queued companion Bluetooth
disconnects and whole-report ownership remain outside this contract.

### Captured Mouse callback ownership

The legacy `TouchPadOn` path now uses an exact callback owner for its ten touch
and SixAxis subscriptions. It binds the Mouse/logical owner, physical source,
original event surfaces and a monotonic owner generation. An atomic
admission-count/retired-bit word rejects copied stale delegates and prevents
successful replacement/reset before admitted Mouse callbacks have finished.
The normal wrapper path takes no monitor and introduces no polling timer,
worker or transport operation. Cold retirement exactly unsubscribes and drains;
an undrained owner remains unavailable for slot reuse.

Source retirement and Mouse replacement are separate identity-checked
operations, so an old source cannot retire a different successor merely
because it has reused the slot. Removing a primary retires surviving-peer
callbacks that target its Mouse. Lifecycle entry points reject reentrant Mouse
callback requests before their service lock; shutdown cannot continue after a
failed Stop. These are callback-lifetime fixes, not whole-report serialization
or same-Mouse concurrent profile-edit safety.

The first build plus existing focused peer/typed-authority/post-map tests
passed **33/33** (`callback-owner-build-20260902.trx`). The first new callback
suite plus that existing group passed **45/45**
(`callback-owner-focused-20260902.trx`). Further logical-slot accumulator and
PreTouch regression coverage was then completed. After draining the exact
owner, the registry resets its logical-slot accumulator before releasing the
incarnation. This prevents a departed secondary's stronger gyro contribution
from suppressing a weaker new peer at the surviving primary slot. Replayed
stale cleanup cannot reset a successor. Direct PreTouch state-mutation
assertions verify both old and successor Mouse objects remain untouched by
copied retired delegates. The earlier counts are verification history, not
final freeze claims.

Final central verification after source/test freeze:

- `callback-owner-final-focused-20260902.trx`: **47 passed, 0 failed**.
- `callback-owner-full-20260902.trx`: **2,815 passed, 0 failed, 3 skipped**
  (2,818 total; 30 s), Release x64. The skips remain the three opt-in live
  application-loopback audio tests, not callback/gyro tests.
- `callback-owner-stress-1..5-20260902.trx`: five runs of all 14 new callback
  tests plus the 4 peer tests, **18 passed per run / 90 executions** on the
  same built binaries.
- VIIPER `test ./... -count=1 -timeout=90s` passed all packages. No Go
  production source changed in this continuation.
- Targeted tracked-file `git diff --check` passed. Independent read-only
  review found no remaining blocker in the bounded callback patch; it did
  not certify whole-report ownership, profile mutation races or hardware.
- Program Files SHA-256 hashes remain the existing baseline:
  DS4Windows `2D8DBAD4A4CFB31ADDCAE6CDF8ADA534E35EEAA44344E05E4601272400BCD063`,
  VIIPER `AD14F2C9048D61B3447F2F79D7A122EDEA81E5DB52A1AC803D294E5BC9CD2324`,
  USBip `FC1660E3759D8AF4CEDE48DBE194285A5A1DE85CE6E3216724499AFD32BE92E8`.

This continuation made verified source/software progress. The full goal stays
active and incomplete, with no installer/production readiness or improved
hardware-latency claim.

### Newly confirmed Switch 2 production gyro hook gap

Historical finding below; the source-level fix and verification are recorded
in the following continuation. Hardware behavior remains a separate gate.

The production `Switch2ControlServiceReversibleProfileSlotHost` composes
`Switch2ControlServiceExactMouseSlotStage`, which creates the per-slot Mouse
but does not install its event hooks. The production profile stage resets and
configures that Mouse without calling `TouchPadOn`. Repository-wide source
search found no alternate production call/subscription to `Mouse.sixaxisMoved`.
The runtime emits projected SixAxis before Report, but that event returns when
there are no subscribers. Consequently decoded motion alone does not prove the
normal Mouse gyro activation/mapping modes are operational in that registration
path. Tests that directly subscribe a Mouse or manually call `TouchPadOn` do
not close this integration gap.

This is a **functional implementation gate**, ahead of polling-rate tuning.
The next fix must add a reversible, exact-lifetime callback stage to the
existing Switch 2 host, activate it only when profile staging is ready, and
retire/drain it before profile reset/slot cleanup. It needs a production-stage
test that publishes a real decoded/projected runtime frame through an actual
Mouse and canonical mapping, including rollback, terminal/reconnect and
successor isolation. It must not acquire the legacy service lock in the wrong
order relative to the Switch 2 lifecycle/publication gates. No hardware gyro
support or fix of this missing hook is claimed here yet.

The reviewed minimal seam is a retained callback facet after successful
profile-stage validation and before `record.Prepared = true` in
`Switch2ControlServiceReversibleProfileSlotHost`, storing the exact registration
token/device/Mouse owner. Retire this facet first in `TryCleanupRecordNoLock`,
before profile inverse reset and Mouse-slot removal. Normal removal already
drains runtime publication and delivers terminal neutral before host cleanup;
unexpected active callbacks can make `TryRetire(0)` reject cleanup without
blocking under the lifecycle gate. Do not route this through legacy
`service.TouchPadOn`, which acquires a different lifecycle lock.

For tests, use actual registration/host/Mouse plus the in-memory profile and
no-system-input mapper fixture pattern. Inject raw-derived IMU runtime frames
through USB, Bluetooth and joined/fused owners. Require same-report activation,
nonzero mapped gyro output, neutral release, rollback/removal ordering and
stale-delegate successor isolation. Existing motion tests install their own
observer and therefore could not catch the missing production subscription.

### Production gyro dispatch continuation: implemented and software verified

The missing producer is now connected through the existing host. After profile
staging succeeds, the host retains an exact device/Mouse direct callback owner.
During an admitted Report, it invokes real `Mouse.sixaxisMoved` immediately
before the existing canonical mapping pipeline. It does **not** subscribe to
raw SixAxis: that event precedes registration report admission. The original
table lease and original runtime report envelope remain in use; no new worker,
poll timer, second mapper, per-report log or table lease was added.

The runtime now permits borrowed state only on the owning thread inside actual
Report callbacks. Profile/queued actions also reserve runtime publication, but
cannot borrow report state. Wrong-thread and fabricated-envelope attempts are
rejected. The cached envelope is reusable, not a unique freshness token;
current callback scope and registration admission provide that authority.

Terminal dispatch retires direct gyro admission and clears pending stick gyro,
activation and current swipe state before canonical neutral mapping, retaining
swipe release edges for retry. Valid no-motion input releases transient gyro
without clearing the user's toggle latch; invalid authority is rejected.
Cleanup retires the producer first, never waits under the lifecycle gate, and
rechecks exact Mouse identity before every profile inverse retry. A partially
cleaned host cannot return idempotent prepare success. The shared legacy
callback registry also no longer treats a partial, still-closed subscription
as successful activation.

Evidence on the final implementation:

- `switch2-gyro-authority-20260902.trx`: **56 passed**. Includes 12 actual
  registration/host/Mouse/raw Common05/canonical mapper/Xbox-and-Switch encoder
  cases, five publication-authority cases, and the host/callback lifetime suite.
  USB Pro, BLE Pro and joined/fused Joy-Con same-report activation/release pass.
  Pending gyro, directional-swipe terminal release, exact successor isolation
  and table retirement before runtime stop are covered. OS transports and
  profile persistence are fakes; no manual normal Mouse event wiring is used.
- `switch2-gyro-stress-1-20260902.trx` through `-5-20260902.trx`: **56 passed
  each**, 280 additional executions, including the concurrency boundaries.
- `switch2-gyro-full-20260902.trx`: **2,838 passed, 3 skipped, 0 failed,
  2,841 total** (29 s). Skips are the existing opt-in live process-audio tests.
- Warmed borrow/publication and host-dispatch allocation checks passed. These
  use synthetic cached input and do not establish real cadence or latency.
- Initial test compiles exposed missing test namespace and lambda-discard
  naming mistakes; those harness defects were corrected before the passing
  focused/full runs. No hardware result is inferred from these runs.

This closes the identified source-level gyro producer gap, **not** the complete
feature, physical input/feedback, game or installer gates. No Program Files
replacement, hardware probe or controller-configuration change was performed.
See `DS4Windows/docs/protocols/switch2-controlservice-reversible-profile-staging.md`.

### Whole-report ownership finding

The audit confirmed that legacy Joy-Con raw merge uses different per-device
locks for a shared `JointState`, while the secondary gyro path can mutate and
submit primary `MappedState` concurrently with primary mapping. Copying at the
encoder would still permit an old secondary snapshot to publish after a newer
primary. `DS4State.CopyTo` aliases motion, and `SixAxis.copy` is not an exact
deep scalar snapshot. This is an original Joy-Con path finding; the Switch 2
runtime has a separate publication owner.

The cohesive next boundary is a pair-owned fixed journal with inline single
draining across owned snapshot, merge, canonical mapping and submission, with
callbacks outside its short queue gate. Both halves must drive output promptly;
button edges and exact pair/source retirement cannot be replaced by a
latest-analog-only queue. No new polling worker or unrelated mapper is proposed.
See `DS4Windows/docs/protocols/legacy-joycon-report-ownership.md` for the exact
source findings, reuse candidates and deterministic integration test matrix.
This whole-report fix is **not implemented or validated yet**.

### Raw-stick calibration parity audit

Switch2Connect remains pinned to
`61ac6642ce12fe7217e38a860b14863b18ca7e28`; its inspected `gui.py` and
`controller.py` headers identify TommyWabg and GPL-3.0-or-later. Its wizard
collects physical stick extrema and stationary centers and persists PC-side
overrides. Our generic window currently saves byte-scale profile drift, not
per-physical raw travel/center calibration.

Keep factory/user-SPI transport calibration immutable within a physical
generation. An application override should be bound per exact persistent peer
and applied at the existing source-to-profile projection, before canonical
mapping, using the existing axis/orientation code. Collect validated raw frames
in bounded report-owned storage; persist off the report lane with ordered save
and reset operations. USB/Bluetooth identity equivalence is not proven, so do
not copy a single-candidate alias heuristic. This is a reviewed implementation
seam, **not a completed calibration feature**.

## Owned-motion continuation: physical history and UDP isolation

The preceding goal turn made source changes and completed the gyro integration
tests; it was progress, not a wait or no-progress turn. This continuation
revalidated DS4Windows HEAD `061fab1304e77c995ce9451b7ef20e51cc870070` and the
current joined/report sources. Existing dirty work was preserved.

The joined-report audit confirmed more than a final-send race: initial/hotplug
pair references become visible before the second profile is prepared; the
constructor removal callback clears shared state before service retirement;
raw gyro precedes mapping admission; profile pause/queued actions fence only
one reader; and delayed peer disconnect lacks a pair-incarnation receipt.
The journal must include these boundaries. It is **not implemented yet**.

Two prerequisite ownership bugs were fixed in production callers now:

- Original Joy-Con physical history no longer shallow-aliases current Motion.
  `PreservePhysicalStateData` uses a preallocated `DS4StateOwnedSnapshot`,
  preserving every motion scalar and at most one independent previous sample.
  New decoding cannot overwrite that committed previous frame or turn current
  motion into its own predecessor. This does not serialize shared JointState.
- Each `CreateDevUDPMotionHandler` owns independent state/motion scratch.
  Smoothing and Switch 2 Cemuhook yaw policy no longer write into source Motion
  or the mapper's TempState, and repeated observations do not compound yaw.
  Exact sender/replaced-slot checks reject obsolete handlers; null Motion is
  preserved safely. Those checks do not replace a registration/drainer lease.

The Joy-Con reader now reuses the existing projected SixAxis envelope instead
of allocating one per sample. Review found and fixed that publisher's separate
check/invoke event-field race: it now captures one delegate snapshot so removing
the last subscriber cannot cause a null invocation. Exact callback wrappers
still reject callbacks whose owner has retired.

`DS4StateOwnedSnapshot` captures both motion scalar images before writing its
owned slots, handles self/cyclic/cross-aliased graphs without traversal, preserves
typed axes and sidecars, and reuses fixed storage. Callers must own a stable
source and finish consuming before reusing the snapshot. It is not a lock or
pair journal; the legacy shallow-copy API elsewhere is unchanged.

New integration gap: automatic Switch 2 UDP observation is not wired by the
profile/attach path, and UDP status/port controls enumerate legacy DS4Devices.
The raw-runtime/actual-handler test verifies isolation only, not automatic
registration or UI availability. Finish that integration using the admitted
Switch 2 report pipeline, without putting slow networking before virtual input
submission or granting a separate unleased raw event authority. UDP metadata
conversion/client-list/network allocations remain separate hot-path work.

Initial `owned-motion-first-20260902.trx` passed **17** snapshot/physical-history
tests. After the event-race fix and UDP tests, the final focused
`owned-motion-integration-20260902.trx` passed **42/42**: 13 snapshot, 5 physical
Joy-Con, 7 UDP, 5 runtime authority and 12 admitted gyro integration cases.
The helper and physical gyro/history warmed allocation checks pass; no complete
controller-to-network allocation or latency claim follows from that.

The first full run, `owned-motion-full-20260902.trx`, had **2,862 passed,
3 skipped, 1 failed**. The existing `WarmFilterOwnerAndReducersAllocateNothing`
measured 864 bytes rather than zero. Source review found no direct call from
that local filter workload to the changed snapshot/UDP code, but did not
attribute the allocation. The zero threshold and filter source/test were left
unchanged. Ten isolated `filter-allocation-repeat-N-20260902.trx` runs passed,
then the unchanged `owned-motion-full-repeat-20260902.trx` passed **2,863 tests,
3 skipped, 0 failed** (2,866 total; 30 s). The initial result remains an
unattributed allocation anomaly, not a diagnosed/fixed defect or proof of a
runtime-only cause. If it repeats, use preallocated per-operation allocation
measurements and exact allocation-stack evidence; do not loosen the threshold
or rely on sampled traces that may miss such a small allocation total.
One further unchanged full confirmation,
`owned-motion-full-confirmation-20260902.trx`, also passed **2,863 tests,
3 skipped, 0 failed** (29 s). It does not remove the attribution caveat above.

No hardware probes, sockets, application startup, driver installation, profile
files or Bluetooth associations were changed. Installed Program Files hashes
remain the same as the prior baseline (DS4Windows `2D8DBAD4…`, VIIPER
`AD14F2C9…`, USBip `FC1660E3…`). See
`DS4Windows/docs/protocols/owned-motion-observations.md` and the expanded
`legacy-joycon-report-ownership.md` for the remaining exact boundaries.

## UDP sender continuation: bounded send ownership and exact sessions

DS4Windows remains at `061fab1304e77c995ce9451b7ef20e51cc870070`; VIIPER remains
at `54f7853aa4f97394298fc8f4c3ebb865237a3456`, both on the landing-zone branch.
Existing dirty work was preserved. This continuation edits DS4Windows UDP
sender source/tests and documentation, not VIIPER executable/driver code.

Before adding automatic Switch 2 observation, sender review confirmed three
existing faults: `_pool.Wait()` can block the controller report thread;
round-robin buffers can still belong to an older async operation despite an
available semaphore credit; and 24/32-byte control replies were always sent
as 100 bytes. Start/Stop also reused mutable socket/server/client fields while
old receive callbacks still referenced them.

Implemented `UdpDatagramSendPool`: fixed buffer/argument entries, bounded CAS
reservation without capacity waiting, exact send lengths, recipient/byte
ownership, synchronous/async/failure return, deferred in-flight disposal, and
per-pool saturation/failure counters. Saturation rejects an optional DSU
datagram, not canonical virtual-pad input. This is not a guaranteed-delivery
protocol and adds no virtual input batching, coalescing or rate limit.

`UdpServer` now creates a separate session for each Start. Socket, receive
buffer, server ID, client registrations and send pool never migrate to a new
session. Cold request versions reject superseded startup. A session-only cold
retirement gate makes every Stop caller observe socket close before rebinding;
initial receive-arm failure propagates, and subsequent arm failures retire
instead of recursively rearming a closed socket. Independent review caught
the concurrent-Stop completion gap and swallowed initial arm failure before
verification; both were corrected. Report publication takes neither cold gate.

The first focused run, `udp-send-ownership-focused-20260902.trx`, passed 24 and
failed 6 CRC checks. A cold standalone server used the slicing CRC table before
ControlService initialized it, producing `0xffffffff`. The UDP encoder now
uses the self-initialized ordinary CRC table and writes its packet/CRC into
the destination span without boxed pinned structs or CRC byte arrays. Normal
ControlService already performs the old initialization: this result does not
prove installed sessions generally had bad CRCs. Tests now verify CRC with an
independent scalar reference.

`udp-send-ownership-focused-crc-20260902.trx` passed **30/30**: 16 pool, 7
session and 7 existing motion-isolation cases. Tests cover saturation and
arbitrary/early completion, exact packet lengths, retained payloads, disposal
races and old-pool isolation, actual loopback control/state responses, blocked
old request completion, restart/client isolation, superseded Start and failed
bind/retry. The pool's fake synchronous sender passes warmed zero allocation;
this is not an entire report-to-socket allocation or latency claim.

Both `udp-send-ownership-full-20260902.trx` and the unchanged
`udp-send-ownership-full-repeat-20260902.trx` had **2,885 passed, 3 skipped,
1 failed** (2,889 total). The sole failure was again the existing
`WarmFilterOwnerAndReducersAllocateNothing`: **864 bytes**, unchanged zero
threshold. UDP cases passed. This recurrence remains unattributed.

Added opt-in `DS4W_TRACE_STICK_FILTER_ALLOCATIONS=1` instrumentation to that
test only. The default Step, warm 2,000 / measured 20,000 workload and zero
assertion remain unchanged. Diagnostic mode preallocates per-operation counter
records and formats them only after measurement. The full instrumented run,
`udp-send-ownership-full-allocation-trace-20260902.trx`, passed **2,886 tests,
3 skipped, 0 failed**. It reported zero bytes across all recorded windows on
thread 4, so it did **not** capture or explain the prior 864 bytes. Instrumented
success must not replace normal-path verification or be called a filter fix.
The subsequent normal (trace flag absent) full run,
`udp-send-ownership-full-default-20260902.trx`, also passed **2,886 tests,
3 skipped, 0 failed**. The repeated 864-byte failures are still unexplained.

Final sender hardening handles null/non-six-byte MAC metadata consistently
with the port-info zero-address fallback and guards null MAC subscription
lookups. Three additional loopback cases include a MAC-only subscriber, so
these optional observations cannot throw on absent identity. This does not
define Switch 2 identity or connected-state policy.
`udp-send-ownership-final-focused-20260902.trx` passed **33/33** after this change.
The final-source normal full run,
`udp-send-ownership-final-full-20260902.trx`, had **2,888 passed, 3 skipped,
1 failed** (2,892 total). Its sole failure was the same filter zero-allocation
test, this time **816 bytes**. Thus the ordinary suite is not consistently
green, and the filter allocation must not be called fixed by instrumentation
or by earlier successful repeats. No production filter change or threshold
relaxation has been made.
Two additional opt-in diagnostic modes keep the original Step body for warmup
and measurement: `step` samples whole iterations; `boundary` records a raw
loop-end counter before the original inline assertion and logs only afterward.
Both `udp-send-ownership-full-step-trace-20260902.trx` and
`udp-send-ownership-full-boundary-trace-20260902.trx` passed **2,889 tests,
3 skipped, 0 failed**. Whole-step windows reported zero; the boundary mode
reported `rawLoopDelta=0` and the original assertion passed. These modes did
not reproduce the issue either and do not attribute it to filtering, JIT,
runtime, or assertion setup. Retain the original strict normal-path gate and
capture a failing run's allocation windows/stacks before assigning a cause.
The subsequent normal-path final verification,
`udp-send-ownership-default-after-traces-20260902.trx`, again had **2,888 passed,
3 skipped, 1 failed**, with **768 bytes** in the unchanged zero-allocation
assertion. This is the latest normal-suite result. Keep this as an unresolved
release gate, not an all-green result based on successful diagnostic modes.

Automatic Switch 2 UDP registration remains unimplemented. A further metadata
gate is now explicit: Switch 2 runtime inherits blank legacy Sony serial data,
which `GetPadDetailForIdx` currently marks disconnected. The next integration
must use exact runtime registration for connected state, a nonsecret DSU
identifier, and bounded owned observations after canonical submission.
Remaining client locks, metadata/list allocations, smoothing and socket calls
are still on the current report path. Facade lifecycle versions do not fix
upstream UI delays or queued ControlService request races.

Only ephemeral loopback sockets were used. No controller/radio tests, app
startup, configured DSU port, external interface, installation, profile change,
or association change occurred. Program Files hashes rechecked unchanged:
DS4Windows `2D8DBAD4…`, VIIPER `AD14F2C9…`, USBip `FC1660E3…`.
See `DS4Windows/docs/protocols/udp-send-ownership.md` for precise contracts and
remaining gates. The full production goal remains incomplete.

## Remaining validation and implementation gates

The confirmed Switch 2 production gyro producer gap is now source-fixed with
production-composition software coverage above. Physical history/UDP mutation
isolation is implemented, but automatic Switch 2 UDP registration/toggles and
coherent legacy joined-report ownership remain implementation gates. Real
controller/game validation of the Switch 2 path also remains open.

1. **Recoverable history-fault teardown hardware validation remains open.**
   Source-level retirement and Stop-ACK coverage above repair the coupled
   VIIPER/DS4Windows fault path. Exact cleanup/startup integration is now
   source-wired and software verified. Native retry-job resource retirement,
   Windows reattachment isolation and portable Windows removal/reconnect
   behavior must pass before production recovery.
2. Active nonzero motor Delay, actual four-channel API/game feedback, physical
   actuator/LED validation, BLE association/radio/firmware matrix, Joy-Con
   single/joined modes and feature parity, same-span latency comparison, soaks,
   every source/target combination, and installer gates remain unproven.
3. **Full source-level feature/input parity still requires the remaining
   inventory and integration matrix.** Physical rail identity and ordinary
   C/rail gyro activation are corrected above. Extend raw physical packet ->
   canonical mapper -> each meaningful virtual target coverage across the
   remaining controls/axes/motion/profile combinations; the exact rail-button
   matrix is not a substitute for the complete product matrix or live input.
4. Complete the explicit raw-travel calibration/profile-picker parity gaps,
   the full source/profile/target integration inventory and remaining mouse
   precision paths. Flick-stick and right-stick delta-acceleration still read
   byte source coordinates; generated gyro/touch/OSC byte outputs and analog
   trigger bindings retain their existing 8-bit vocabulary. Do not describe
   target-domain expansion as recovered source precision.

Remaining transport work must preserve the shared claim/commit/lifecycle
contract and establish measured faster-host-service behavior with active
feedback. Hardware
validation remains pending; do not substitute simulator throughput for actual
consumer observations.

## Verification commands and results

From VIIPER, using `../_toolchains/go1.27.0/go/bin/go.exe`:

- Terminal input-retirement follow-up: `test ./... -count=1 -timeout=90s`
  passed on the final server/broker/adapter integration, including the actual
  retained USB/IP plus X1BR Stop-ACK/removal test; `vet ./...` passed.
- `test -race ./device/xboxone ./internal/server/usb -run 'InputRetirement|OwnerRetirement|RetainedInputJournal|FeedbackLane|OrdinaryFeedback|RetainedReset' -count=20 -timeout=90s`:
  Xbox 5.929 s and USB server 4.674 s, both passed. This includes the deadline,
  already-admitted-completion, all-lane, fatal-precedence and real production
  broker/retained-handler tests. Independent full USB server race runs passed
  three repetitions; focused retirement race runs passed twenty repetitions.
- `test ./... -count=1 -timeout=90s`: every package passed on the final ordinary-
  feedback, cold-owner, readiness, and reset-quarantine implementation.
- `test -race ./device/xboxone ./internal/server/usb -run 'FeedbackLane|OrdinaryFeedback|FeedbackCold|FeedbackDetachment|JournalContinuesWhileFeedback|LocalFailureRetries|CancelJoinsInFlight|RetainedXbox|RetainedInputServicePolicy|RetainedEndpointCadence|RetainedSubmissionReset|RetainedReset|RetainedUSBAdapter.*(Reset|Quarantine|Cancel|Panic)' -count=10 -timeout=90s`:
  both packages passed on that final state (Xbox 5.124 s; USB 8.952 s).
- `test -race ./device/xboxone -count=3 -timeout=90s`: complete Xbox package
  passed after the final feedback retry-fault containment change (7.647 s).
- Independent reset reviewer: focused reset race tests passed 50 repetitions;
  the complete normal USB server package passed. The original full-suite reset
  failure and three deliberately failing pre-fix cases are documented above,
  rather than replaced by a claim that all historical runs were green.
- `test -race ./device/xboxone ./internal/server/usb -run 'FeedbackLane|OrdinaryFeedback|FeedbackCold|FeedbackDetachment|JournalContinuesWhileFeedback|LocalFailureRetries|CancelJoinsInFlight|RetainedXbox|RetainedInputServicePolicy|RetainedEndpointCadence|RetainedUSBAdapter.*(Reset|Quarantine|Cancel|Panic)' -count=10 -timeout=90s`:
  both packages passed after ordinary-feedback separation and detachment wake
  correction, before final reset-timeout containment verification above.
- `test ./... -timeout=90s`: all packages passed again after change-only input,
  pre-selection readiness capture, periodic status, and experimental service
  policy integration.
- `test -race ./internal/inputpresentation ./device/xboxone ./internal/server/usb -run 'FixedReportPending|FixedReportLifecycleBaseline|RetainedInputJournal|RetainedInputServicePolicy|RetainedXboxInputServicePolicy|RetainedEndpointCadence|ControllerPersonaPeriodicStatus' -count=10 -timeout=90s`:
  all three packages passed on the final integrated state. Nine periodic-status
  tests include exact retry, sequence isolation, lifecycle/gates, timestamp
  saturation, and a warmed zero-allocation claim/admit/resolve path. Three new
  idle tests cover readiness wake, identical suppression/explicit KeepAlive,
  and timer-only status while input remains unchanged.
- `test ./... -timeout=90s`: all packages passed after the final journal,
  lifecycle preflight, and pre-/post-Permit control-boundary fixes.
- `test ./device/xboxone -run RetainedInputJournal -count=10 -timeout=45s`:
  all 16 top-level journal tests passed, including table-driven subcases.
- `test -race ./internal/inputpresentation ./device/xboxone ./internal/server/usb -run 'FixedReportLifecycleBaseline|RetainedInputJournal|RetainedEndpointCadence|GuideEdgesAcrossPollAndRetry' -count=10 -timeout=60s`:
  all three packages passed after the final fixes. This includes the new
  shared baseline-capability test and concurrent producer/USB completion test.
- Independent reviewer: `test -race ./device/xboxone -run RetainedInputJournal -count=10`
  passed after both Permit-window regressions were fixed. No additional
  journal blocker found; overflow quarantine and feedback coupling remain open.
- `test ./internal/server/usb -run RetainedEndpointCadence -count=10 -timeout=45s`:
  all eight tests passed after timer-only wake and parked unlink/close coverage.
- `test -race ./device/xboxone ./internal/server/usb -run 'TimedFeedback|AbsoluteExpiry|ProductionPreparation|RetainedEndpointCadence|RetainedSubmission.*(Reset|Pending|Readiness|Warm)' -count=10 -timeout=60s`:
  passed for both packages before the final timer-only/unlink test additions.
- Independent cadence/lifecycle/reset/pending/readiness review: ordinary
  `-count=10` and race `-count=3` passed.

From DS4Windows:

```text
dotnet test DS4WindowsTests/DS4WindowsTests.csproj -c Release -p:Platform=x64 --no-restore --nologo --verbosity quiet
```

2,373 passed, 3 skipped, 0 failed (2,376 total) after watchdog and audio-status
changes; existing compiler warnings remain. The later publication-log wording
edit changes no scheduling behavior. The focused watchdog suite passed 105
tests, including 15 new deterministic watchdog cases and the real reader-finally
successor-isolation path.

The later input-rejection follow-up adds 12 focused cases. The final focused
`--filter 'FullyQualifiedName~XboxOne|FullyQualifiedName~ControllerAudioEndpointTests'`
suite passed 129/129. Ten repeated focused runs passed before a final CAS-order
hardening that keeps duplicate ACKs from setting the input-only fence; the
129-test suite then passed again. The complete DS4Windows test command above
was rerun after all reviewers stopped editing: **2,385 passed, 3 skipped,
0 failed (2,388 total; 21 s)**. Its 250 ms input ACK wait and 500 ms additional
terminal ACK wait are unchanged. Those are not an absolute total Disconnect
deadline; existing API/USB-IP operations and worker joins retain their bounds.

One new concurrency test initially hung because Go synctest does not consider
mutex waits durably blocked. It was corrected to retain an explicitly stale
attempt timestamp and test final-boundary clock sampling deterministically;
the production timer loop has separate coverage. This was a test-harness
defect, not evidence of a production lock deadlock.

No post-cadence hardware CPU/latency numbers, physical fidelity result,
competitor non-inferiority result, or installer readiness is claimed. The
active goal remains incomplete.

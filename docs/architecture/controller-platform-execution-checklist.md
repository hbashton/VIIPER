# Controller platform execution checklist

Updated September 4, 2026 following the user's instruction to replace the prior
implementation pathway with itemized, calculated priorities. The full requested
end state is unchanged. This is the working order, not a completion claim.

Current September 5 priority: fix b67 Bluetooth disconnect leaving a stale row
and virtual pad. Heap proves physical Disconnected, input worker exit and
TerminalDeliveryRejected quarantine, not a UI-thread deadlock. b68 source adds
exact disconnected/native-release proof and local feedback retirement without
false Stop ACKs; skips new CCCD writes only after definite disconnect. Five red
regressions now pass; 387 focused and 90 adapter/runtime tests pass. Final full
suite3644pass/3existing audio skips/0fail. Agent ended only exact old portable
27368/33284 after UI helper found no window, then hash-copied eight saved files.
b68 launched11:10:54-56, app34356/broker30516. UI no controller/Disconnected;
discovery active. Next: user wake with A/Ready/disconnect/row cleanup/reconnect.
Joined-pair and ambiguous native failure recovery remain separate open gates.

Prior September 5 priority: improve drowned-out Xbox/DualSense haptics and replace
portable (explicitly authorized). b67 source adds HD quiet-signal preservation,
packet-local frequency analysis, source-priority carriers and soft overlap
mixing.9red->9green,165focused pass,2.867us offline mean/zero warm allocations.
First full3632pass/3skips/1unrelated yaw-allocation failure448bytes; isolated13
pass, full repeat3633pass/3existing audio skips/0fail. b66 closed by user; latest
DualSense profile and seven other explicit files hash-copied to b67, launched
10:47:57-58. Ready was independently confirmed before the disconnect failure
above. Haptic A/B remains pending. See
DS4Windows/docs/protocols/switch2-haptic-detail-rendering.md for approximations.
No installed/driver/startup/security/broker-source/release changes.

Prior September 5 b66 checkpoint:3617 full tests pass/3 existing audio skips.
Desktop b66-ble-counter-continuity launched10:15:24-26, broker15432/app34488,
after user closed b65 and eight exact saved files were hash-verified forward.
Contains BLE counter and DualSense native-media corrections. Pro BLE connected
10:15:40.881 with Switch2LabXboxOne/90%; Windows XboxComposite OK and both
broker connections established, no fresh feedback failure through10:16.
UI card confirms Bluetooth; exact Ready-label capture not obtained because
window bounds changed during user profile editing (one refresh/retry, then
UI input stopped). Next: user game input, physical feedback and real counter-
boundary continuity. Hades II and terminal-feedback retirement/Stop cleanup
remain open. No Program Files/driver/startup/security/broker-source/release changes.

Latest September 5 failure takes priority: b65 lost the Xbox feedback stream
at09:48:44. The private runtime snapshot proves Pro BLE counter1431640->1
with forward host time, mapper BackwardOrOutOfOrder,94738 reports and zero
queue overflows. This retires physical input and feedback; the Xbox socket
warning is downstream. Pro BLE now shares the evidenced USB arrival-order
policy, with exact identity/lease/clock fences unchanged. Three red regressions
now pass (9 tests); full-suite has since passed as recorded above. Separate Stop/
terminal-feedback quarantine cleanup is not claimed fixed. The older b65 Ready
snapshot below is historical, not current status.

September 5 user priority: investigate Hades II native DualSense haptics working
in released RC4.3 but failing in released RC4.4. A native-media rejection ->
compatibility-rumble fallthrough was reproduced (4 red cases) and corrected
in DS4Windows source (5 green; 546 related and 3610 full pass/3 existing skips). Exact release
commits and limitations are in DS4Windows/docs/dualsense-rc43-rc44-native-haptics-regression.md.
Physical USB/BT and actual Hades II acceptance remain unconfirmed. Running b66
now contains the source correction; b65 did not.

Prior candidate (now closed): Desktop b65-ble-output-handoff, app35472/broker27708,
launched09:18:27-28, discovery09:18:36, BLE connected09:19:35. User completed
XboxOne->Xbox360->XboxOne round trip09:20:25-40 and confirmed "tested, fixed".
Independent UI Ready and no fresh feedback-stream rejection; ~15ms remains.
Changing Windows API input, physical feedback and game acceptance are still
separate pending gates (WGI neutral-only so far). Staged from exact b63
configuration including Auto Profiles.xml. b64 is closed. The b64 heap proves
the virtual-type switch blocked the BLE consumer, causing ACTIVE QueueOverflow
after5017reports, then feedback rejection by the retired physical owner. New
explicit cold-output scope retains latest baseline during detach/attach and
resumes normal FIFO; stale pre-transition snapshot is not replayed to new pad.
439 focused and3605full tests pass,3 existing audio skips. Historical allocation
failure remains unattributed. Verify changing Windows Xbox input, feedback and
cleanup next; cold start/output-switch hardware acceptance is recorded above.
No rate improvement is
claimed: b64 accepted the corrected native WinRT request but still showed~15ms.
See validation ledger/b65 portable status for staging correction and exact pins.

Prior live candidate: Desktop b63-ble-startup-state, app35304/broker33368.
Now Bluetooth Ready after A wake (08:30:25), one Windows/WGI lab Xbox, USB
absent. Observed report interval~15ms. WGI/XInput only neutral observed so far;
user cued to hold a button/move stick now. Changing input/feedback/game
acceptance remains pending; Ready is not full functional proof.
b62's private snapshot proved pre-activation QueueOverflow with zero publication.
Latest-initial-state buffering fixes that startup failure while preserving the
active ordered queue. 395 focused and 3594 full tests pass (3 existing audio
skips); earlier intermittent allocation failure remains unattributed. User cued
to press A without USB and exercise input. Verify real BLE Ready, Windows Xbox
input, cleanup/reconnect and feedback next; do not infer acceptance from tests.

Previous live candidate (September 5): Desktop b62-ble-host-order, app31924 /
broker18100. The b61 host-address wire-order defect is fixed and b62 successfully
associates/reconnects over BLE. It now fails at virtual-slot activation with
QuarantineRequired; investigate this actual hardware failure next. b61 was
normally stopped/closed by the user and its verified idle broker retired.
See the latest validation checkpoint for exact logs, hashes and test evidence.
Full suite has one recurring allocation-test failure (7448 vs0), not fully green.

Historical live candidate: Desktop b60-feedback-cards, app 16260 /
broker 29968, Ready USB Pro and one Windows/WGI Xbox pad. b56/b58/b59 were retired
with normal service Stop and exact virtual removal; their folders remain intact.
Those shutdown checks are not physical unplug/reconnect acceptance. Historical
process IDs and cues below are retained only as dated evidence.

Historical staged candidate: Desktop b61-ble-connect includes the Bluetooth
first-link/retry fixes and legacy Nintendo output worker. Not launched; b60
remains live. Use b61's hash-pinned isolated launcher only after normal old-app
shutdown and exact virtual cleanup. Staging is not hardware acceptance.

## How priorities are decided

1. A failure preventing real play outranks optional interaction/UI parity.
2. Validate the existing implementation before building another prerequisite.
3. Fix the specific failed gate, then rerun that gate and related regressions.
4. When hardware is blocked, take the highest independent task on this list;
   do not silently substitute auxiliary framework work for end-to-end progress.
5. Software tests, physical observations and game acceptance are separate evidence.
   No aggregate percentage or speculative completion date substitutes for them.

## 1. Get the connected Pro working on the correct portable build

Dependency: exclusive application/broker ownership, not a new driver.

- [x] Windows currently enumerates the USB Pro, VID 057E / PID 2069, with all five
  PnP interfaces reporting OK. This proves enumeration only.
- [x] Identify the ownership conflict. DS4Windows PID 31316 and VIIPER PID 33580
  started September 3 around 07:11. The ordinary Roaming DS4Windows log begins at
  07:11:14 with version `VIIPERRC4.3` and `Running as Admin`. b48 logged shutdown
  at 07:10:24. PID 33580 currently listens on ports 3241 and 3242. No targetable
  DS4Windows window is exposed. Task Manager is higher-integrity than the UI helper.
- [x] Old owners exited after the user's confirmation. No integrity bypass used.
- [x] Launch b52 through its isolated launcher: DS4Windows 21240 / VIIPER 20376,
  from the verified Desktop directory, running as User.
- [x] Repair the observed missing Xbox identity bundle in b52. Restore the identical
  reviewed F00D:BEED lab identity and add its hash to the launcher checks. Service
  restart then successfully associates the Pro with the Xbox One virtual output.
- [x] One Pro row reaches Ready, USB observed interval 4.00ms / 250Hz, battery 90%.
  Exact binary/log paths are recorded in b52's PORTABLE-TEST-STATUS.md.
- [x] Confirm changing controller readings and neutral releases in the focused
  Windows API reader after the user's physical exercise. Both triggers reach
  0/1; button union 4095 covers face, D-pad, shoulders and menu/view. XInput
  independently records packet 836 with buttons 20480. Full stick ranges and
  stick clicks remain separate checks, not inferred from this partial inventory.
- [ ] Cue unplug; confirm row/output removal and neutral release. Cue reconnect;
  confirm one replacement lifetime and Ready again, without restarting the app.
  b53 failed the user's physical unplug check: native input completed with
  NativeFailure, but the pump suppressed lifecycle attention after the transport
  closed itself. The outer slot/output remained active. The repair is included
  in b56, now Ready after normal initialization; the user has been cued to unplug
  USB and leave it unplugged without entering pairing mode.
  September 5: the user reports unplug did not remove the row. Current b56
  inspection instead finds a Running USB pump with 1,492,265 completed reads
  and 64 fresh independent USB reports. Whether the cable remains unplugged
  or was reconnected is awaiting clarification; this is not a passed gate or
  evidence that the earlier Stopped/NativeFailure defect recurred. See the
  latest validation checkpoint for exact evidence and timestamps.
- [x] The live b52 capture contains 100,000 successful 64-byte 0x05 reports,
  including counter 1,431,652 -> 2 at ordinal 73,404. Same app/controller lifetime
  remains Ready, with changing virtual input observed afterward. No firmware
  modulus is inferred. Host read-completion timing is not end-to-end latency.
- [x] Stop/close b52 cleanly, observe Windows gamepad removal, then launch the
  hash-verified b53 GL/GR candidate: DS4Windows 26604 / VIIPER 34168. The USB
  runtime and one Windows Xbox gamepad return at 22:25:03 local. This application
  replacement is not a physical unplug/reconnect test.

Candidate: `Desktop/Controller-Platform-Portable-Lab-2026-08-31/runtime/`
`DS4Windows-current-2026-09-04-b56-discovery-ui`.
Its existing `PORTABLE-TEST-STATUS.md` and `Start-IsolatedLab.ps1` define hashes,
isolated data, environment and process/port refusal checks. b56 corresponds to
the 3,246-test source build; b52/b53 hardware results remain separately identified.
b54 was closed while stopped; b55 was staged but never launched. b56 app 35880 /
broker 16976 is now running. Normal startup produced Ready USB input, an observed
4.00ms / 250Hz interval, 1,024 successful passive reads, and one matching Windows
Xbox gamepad at 23:40:14. The earlier passive timeout did not prove USB absence.

## 2. Pro USB -> VIIPER Xbox One -> Windows/game, including feedback

Dependency: item 1 Ready. This validates the central requested use case first.

- [x] Select the reviewed lab Xbox One profile and confirm a unique Windows Xbox
  gamepad through the existing USB/IP/VIIPER path: PnP Xbox Gaming Device OK,
  Windows.Gaming.Input sees one F00D:BEED target, XInput reads slot 0 successfully
  (packet 1, neutral); slots 1-3 are disconnected. No pulse was sent.
- [ ] Check every button, both stick axes/clicks, D-pad, shoulders, triggers,
  menu/view/guide semantics and neutral release in a Windows API consumer.
  User reports everything except GL/GR showed up. This lab profile leaves BLP/BRP
  initially unassigned; after explicit bindings were saved, raw HID proves both
  rear buttons and releases but the mapper's extra-button registration is missing.
  Source repair, 78 related passing tests and the full passing suite are recorded
  in the ledger. b53 physical GL/GR acceptance is separately established below;
  remaining stick/click/guide inventory is still open.
- [x] On b53, the user confirms GL -> A / GR -> B works. The Windows API reader
  records 44 b53 input states: A (4), B (8), both (12), and neutral (0), with
  repeated releases and final neutral at 22:33:31 local. The earlier read-only
  capture established the physical masks; it did not overlap this later exercise.
- [x] With impulse-to-HD conversion enabled, exercise left/right body and impulse
  independently through Windows.Gaming.Input on b52. All four 20% / nominal 200ms
  API pulses have matching neutral records; the user confirmed all four work.
  This is tactile delivery/stop evidence, not a measured waveform fidelity claim.
- [ ] Verify the conversion toggle off, master output gate, gain and disconnect stop.
- [ ] Verify LED commands and reported battery/charging/native input data.
- [ ] Play an actual game, then close it with and without Game Bar. Confirm no
  stuck input, lingering feedback, stale output or profile-selection mismatch.

Reuse `DS4Windows/utils/ControllerWindowsApiProbe`: it reads the Windows gamepad
API and permits only explicit 20% / nominal 200ms pulses to the unique F00D:BEED
lab persona. Its 30Hz UI poll is **not** a latency or controller-rate instrument.
No new mapper or direct physical output test stack is required.

## 3. Pro Bluetooth through DS4Windows settings

Dependency: a known-working source/target baseline from items 1-2.

- [x] Source repair: allow uncached GATT discovery to establish the first link
  before checking Connected, in both association and remembered input/duplex.
  Nine cases reproduce the old rejection; all 296 Bluetooth tests pass with the
  fix. Windows' API contract and pinned Switch2Connect/SDL source support the
  corrected ordering. Not deployed in b60; not physical pairing acceptance.
- [x] Source/staging: reconcile the SDL donor's transient/empty GATT discovery
  retries (up to ten attempts / 500ms spacing) with the existing startup deadline.
  Only Unreachable/empty-success retry; access-denied, protocol and malformed
  results still fail. Late native service queries retain device ownership and
  cancellation prevents further attempts. All 322 Bluetooth cases pass; b61 is
  staged, not launched. Native/physical acceptance and complete donor parity
  remain open; characteristic-query cancellation is unchanged.

- [x] Repair reproduced coordinator Stop timeout/retry/concurrent-Start ownership
  failures and unsuccessful-start cleanup ownership in source. The 257 related
  simulated Bluetooth tests pass. Full regression: 3,228 passed, 3 existing
  live-audio skips, zero failures. Portable b56 includes these follow-ups and is
  now running; b55 was staged only. No wireless hardware gate is checked off by
  these tests or by an active empty advertisement scan.

- [ ] Explicitly cue pairing mode only when the DS4Windows pairing workflow is
  ready; complete discovery/association from Settings, with usable progress/error UI.
- [x] In source, make Settings distinguish an empty active scan from discovery
  unavailable, failed, interrupted, stopped, starting or still cleaning up.
  Preserve association results separately during refresh and gate association
  on a current active scan/selection. Full suite: 3,246 passed, 3 existing
  live-audio skips, zero failures. This follow-up is deployed in b56.
- [x] On b56, visually verify stopped discovery, Refresh retaining stopped state,
  disabled association without a selection, Controllers/Settings navigation,
  and the transition to active empty discovery after normal service Start.
- [ ] Verify candidate selection preservation, real association result retention,
  busy gating and failure/recovery presentation with actual wireless candidates.
- [ ] Verify input, gyro, HD rumble/impulse conversion, LED and native features.
- [ ] Verify USB/Bluetooth handover, off/on, loss/reconnect, application restart,
  stale association failure and recovery without duplicate rows or stuck Connecting.
- [ ] Repeat Windows API and real game acceptance wirelessly.

## 4. Joy-Con 2 standalone and joined wireless use

Dependency: controllers available for physical acceptance; code tests alone are
not evidence that unavailable hardware works.

- [ ] Pair left and right from Settings; test each standalone orientation/layout.
- [ ] Test joined input, gyro selection/fusion, both HD-rumble sides and player LED.
- [ ] Verify the requested automatic pairing setting: ordered formation, stable
  existing pairs, disconnect/reconnect and orphan re-pairing behavior.
- [ ] Verify manual link buttons, selected highlight, second-controller joining,
  cancellation/unhighlight and separation without held-input leakage.
- [ ] Test physical inputs/feedback and game use in standalone and joined modes.

## 5. Complete the source/target and feedback matrices

Dependency: the central Xbox path is verified, not a separate mapping stack.

- [ ] Every supported physical source, not only Nintendo, can select and drive
  Xbox One output; capabilities/unsupported outputs are represented truthfully.
- [ ] Pro and Joy-Con 2 standalone/joined can drive every designated virtual type.
- [ ] Native DualSense feedback, Xbox body/impulse and other available rumble
  preserve representable detail through the canonical feedback frame to HD rumble.
- [ ] DualSense receives supported impulse-trigger conversions; other physical
  controllers receive their supported feedback type with explicit profile controls.
- [ ] Verify arbitration, gains, toggles, neutral, overload, source/target removal,
  profile changes and stop/restart for each applicable route.

## 6. Finish reference-backed feature and UI parity

September 5: the dedicated Switch 2 Controls sidebar page below Trigger Lab is
now implemented and visually checked on b59. All existing settings/bindings were
moved intact. Impulse tuning enables/disables with its checkbox; Advanced and Log
still route correctly. This closes the navigation-placement gap only, not the
remaining visual polish, feature parity or physical delivery gates.

Subsequent source-only polish groups all 33 original control blocks into five
theme-aware cards (feedback, motion, Joy-Con mouse/dual gyro, layout/connection,
calibration) with descriptive headings. Existing bindings/handlers remain intact.
All 110 related tests pass. Card rendering, themes, resize and keyboard behavior
are not yet visually accepted; b59 remains running without these source changes
for the outstanding physical-unplug check.

Dependency: no unresolved core connection/play blocker that this work would hide.

- [ ] Reconcile an itemized Switch2Connect feature inventory with actual DS4Windows
  settings/runtime behavior, using pinned source references and license constraints.
- [ ] Verify Joy-Con mouse/IR, gun/aiming modes, gyro/dual gyro, orientation/button
  layouts, calibration and all other donor controls, not just the named examples.
- [ ] Complete the controller-operated profile picker only after core play works.
  The reducer and guarded named worker exist, but catalog/runtime/opener/overlay
  integration is unfinished. It is not a current user-facing feature.
- [ ] Place controller-specific settings under Switch 2 Controls and verify clear
  labels, defaults, dependencies, validation, scaling, themes and disconnected UI.
  b60 now contains the five feature cards. Actual default-size Dark/Light views,
  card expansion/scrolling and Tab/Space expansion are checked; impulse-to-HD
  dependent controls disable/re-enable. Original theme/profile retained after
  Cancel. Narrow-window/DPI, screen-reader, disconnected/full reference parity
  remain open; markup tests alone do not close them.
- [ ] Verify ordinary profile switching, touch/swipe/tab interactions and persistence
  have no regression across supported controllers.

## 7. Measure and improve latency without regressing behavior

Dependency: correct input and feedback/lifecycle baseline.

- [ ] Measure separately physical arrival, canonical mapper/broker publication,
  USB/IP presentation and game/API observation; retain tail distributions under load.
- [ ] Remove avoidable waits/coalescing stalls on the existing path while preserving
  ordered button/trigger edges, bounded work and neutral/lifecycle guarantees.
  September 5 source follow-up moves original Switch Pro/Joy-Con native rumble
  submission to a dedicated per-device writer. Input publication uses fixed
  latest-packet storage without waiting for native I/O. All 58 targeted cases
  pass, including blocked-write input progress and real-worker neutral retirement.
  The native 100ms wait/cancellation retirement and synchronous cold subcommands
  remain; worker Join is not a hard bounded shutdown. Not deployed in b60.
  Physical validation and end-to-end low-latency parity remain open.
- [ ] Recheck sustained USB/Bluetooth rates and end-to-end latency after changes.
  A descriptor interrupt interval or 1kHz publication cap is not measured latency.
- [x] Remove the measured per-frame Xbox broker header allocation using one
  reader per production stream. The isolated decoder benchmark moves from one
  16-byte allocation to zero; framing, ordered input ACKs and feedback semantics
  remain intact. This source-only optimization does not close the latency gate.
- [ ] Resolve the intermittent zero-allocation-test failure with attributable
  evidence; do not loosen its threshold or call passing repeats an explanation.

## 8. Package and hand over the requested test installer

Dependency: all applicable earlier gates, including game/hardware evidence.

- [ ] Run full .NET/VIIPER regression suites and independent source review of
  the completed production composition, not merely helper components.
- [ ] Run cold-start, reconnect, profile-change, stop, game exit, sleep/resume,
  multiple-controller and sustained-load acceptance; record remaining limitations.
- [ ] Build a reproducible local test installer and verify payload/provenance and
  install/upgrade behavior in an appropriate isolated validation environment.
- [ ] Put the requested installer on the Desktop with exact version/hash and
  concise usage notes. Do not publish a release or alter installed software now.

## Current source evidence

- September 5 deployment: b60 is now the live Desktop app/broker, including
  feature cards, DS4 profile-stop and Xbox framed-reader follow-ups. Normal
  b59 Stop/Close removed its exact Xbox first; no overlapping mapper owners.
  b60 startup reached Ready and one Windows/WGI Xbox without installed changes.
  Default-size Dark/Light card views and bounded interactions are checked, not
  full physical, wireless, game, accessibility or latency acceptance. Full
  provenance and remaining gates are in the latest validation checkpoint.
- September 5 Xbox broker reader: six new top-level tests pass, including
  partial/malformed frames, changing payload lengths and eight independent
  parallel streams. Go 1.27.0 Windows amd64 in-memory decoder benchmark (three
  runs): 29.79-33.74ns / 16B / one allocation before; 18.38-21.43ns / zero bytes
  and allocations after. These are decoder timings, not controller latency.
  `go test ./...` and targeted Xbox/API/USB race suites pass. All eight real
  C#/Go cases pass without skips in `xbox-broker-reader-interop-20260905.trx`,
  using the new hash-pinned test-only peer documented in the validation ledger.
  Native attach/host/physical output remain simulated. Not deployed in b59.
- Latest ordinary .NET full suite, with process interop enabled:
  `switch2-bluetooth-service-retry-full-20260905.trx`, 3,585 passed, 3 existing
  live-audio skips, zero failures (63s), including all eight C#/Go process cases
  with the AB42697C... test-only peer. The prior `switch2-settings-cards-full-20260905.trx`
  failed the known intermittent filter-allocation assertion (2,312 bytes against
  zero). Passing diagnostic and subsequent ordinary runs do not attribute that
  failure. Threshold and filter code remain unchanged; investigation is still an
  open release gate, not a consistently green-suite claim. No physical acceptance
  is inferred from these tests.
- September 5 source: original Switch Pro/Joy-Con rumble writers retain failed
  neutral writes until accepted and retry the newest mailbox, preserving normal
  active refresh and idle suppression. Fifteen regression cases failed before
  the fix; all thirty new cases and sixty related cases pass. No protocol,
  controller, broker or installed-data change; not in live b56 or staged b58.
  The subsequent DS4-specific correction below addresses profile suppression.
  The latest legacy Nintendo writer follow-up above removes native rumble I/O
  from input in source; historical runtime results do not validate that change.
- Subsequent September 5 source: DS4 profile suppression is separate from
  hardware NoOutputData. Possibly active motors require a final rumble-only stop
  through the current writer; startup-disabled/unproven interfaces are not probed.
  Rejection/throw preserves the stop and re-enable requires fresh feedback. Audio
  mode/volume and CRC remain intact; mailbox acceptance is not physical flush.
  All 34 new cases pass, including three zero-allocation warmed transition checks.
  Non-audio DS4 HID still has its pre-existing synchronous 3,000ms wait and native
  cancellation retirement. This is not low-latency or physical acceptance, and
  does not repair every custom DualSense/Nintendo master-output policy. Not in b59.
- September 5 DS4Windows source: terminal CFBK Stop bypasses profile effect delay
  and tuning, cancels delayed effects after canonical admission, and uses the
  existing USB/Bluetooth owner and physical retry rules. Stale/foreign packets
  cannot mutate the accepted queue/configuration. Twelve new tests and 106
  related tests pass. Not in b56.
- Subsequent September 5 source: Xbox output/impulse enablement edits wake the
  existing Xbox feedback worker without a new game packet. Exact-frame rendering
  restrictions preserve sequence/expiry, body channels and other origins; stale
  or rapid off/on edits cannot resurrect an old effect. Eighteen new tests include
  the production profile-setter path, USB/Bluetooth output, retained writes,
  expiry and nonblocking profile capture. Not in b56; physical/UI acceptance is
  pending. Frequency/strength/delay tuning still applies on new game feedback.
- September 5 VIIPER source: production Xbox native activation uses the pinned
  one-attempt operation so an initial failed call cannot schedule unowned retries.
  Five boundary tests and `go test ./...` pass. Cleanup of driver retries after
  a successful retired import is still open. This source is not in live b56.
- Subsequent September 5 VIIPER source: native attach owns exact overlapped
  completion/cancellation; canceled activation cannot commit late success or
  remove a numeric-address successor. Six native and five handler tests, full
  uncached Go suite, vet, and ten race-enabled native/activation repetitions pass.
  Deadline requests cancellation, not a hard kernel-return guarantee. Successful
  retired-import retry cleanup remains open. Not in b56; hardware acceptance pending.
- Staged, not launched: Desktop candidate `DS4Windows-current-2026-09-05-b57-feedback-activation`
  contains the accumulated source fixes above. Its local status/launcher pin the
  payload and isolate lab data. No credentials or prior evidence copied; b56's
  live unplug investigation remains undisturbed. Staging is not hardware acceptance.
- Post-b57 source: exact Xbox lifetime disposal aborts its pending activation
  management request while preserving the broker for Stop/ACK and exact removal.
  Four new lifetime/socket cases and 183 related tests pass. Not in b56/b57.
- Subsequent source: exact activation scope now covers waiting for shared native
  admission too. Reentrant attach/detach ordering remains, but a canceled waiter
  exits without waiting for or releasing another controller's lease. One real
  client red/green regression and eleven gate cases cover this change, including
  100 cancellation/release race iterations; the full suite is listed above.
  No input-path synchronization or native completion guarantee is added.
  This source is not in b56/b57 and is not physical unplug validation.
- Combined-process evidence: real DS4Windows client/auth/ConsumerReady and real
  Go API factory/activation/exact retirement pass for success, client disposal
  during pending native activation, and server deadline (native attach is an
  explicit lifetime-long test stub). Test-only Desktop peer; no controller,
  USB/IP listener, installed key or driver access. Full Go suite and vet pass.
  Five further runs against the Go race-instrumented peer pass (15 cases).
  These management-only cases are opt-in and skip without a prepared peer.
- Subsequent combined-process evidence adds actual retained USB/IP (private
  loopback simulated host), mapped GIP input/neutral, four canonical feedback
  channels through the real Switch 2 HD encoder, impulse conversion on/off,
  and exact removal held until terminal Stop delivery/ACK. The physical writer
  is a recording BLE lease, not hardware. Five opt-in cases and the corrected
  full suite above pass; five additional race-peer repetitions pass 25 cases.
  The initial observation race and misclassification of permitted unchanged
  Apply refreshes are recorded in the ledger. Distinct-effect ACKs and exact
  terminal Stop ACKs are checked separately; refreshes may avoid redundant
  physical writes. Final startup-task cleanup changes also pass the rebuilt
  full suite and five race-peer cases; no creation/rollback is intentionally
  abandoned at the assertion timeout. Production code was unchanged.
- Further combined-process evidence covers rejected physical Stop, failed ACK
  write after neutral and withheld ACK with the broker left open. Each requires
  a real exact-removal conflict, zero positive Stop ACKs, a fenced registration
  refusing activation/reimport, and old USB socket closure. The recording
  physical lease can recover to local neutral/retirement without retroactively
  acknowledging the failed broker Stop. Eight opt-in process cases are included
  in the full suite above; three race-peer repetitions pass all 24 cases.
  The initial fixture's 24-second-versus-ten-second
  deadline mismatch and cleanup correction are retained in the ledger.
  Permanent physical failure, Windows/UI recovery, tactile output, games and
  latency remain separate gates; this is not physical unplug acceptance.
- New ordinary-physical Xbox feedback fix: disabling profile output now wakes
  the existing feedback worker without waiting for a new game frame or expiry.
  Exact sequence/session/slot/stream restrictions, original TTL, neutral retry,
  off/on no-resurrection and fresh identical-frame resumption are covered by
  four new tests and the full suite above. The profile-reload path uses the same
  common wake. This proves state-setter behavior, not a physical HID flush;
  live b56 and staged b57 are unchanged. Complete source/target routing, output
  flags, persistent TriggerLab effects and hardware feedback remain to validate.
- Switch 2 publication-time follow-up closes the reproduced concurrent profile
  disable gap in the actual Xbox callback. The session exposes its publication
  revision before a final bounded live-policy read; stale queued requests cannot
  overwrite newer publication/owner work. Twelve new production-callback, USB,
  delay/Stop and CAS-ordering cases pass with the full suite above. Initial
  compile and aliased-result failures were corrected before staging; details
  are retained in the ledger. Source only: b56/b57 and hardware gates unchanged.
- Latest staged candidate (not launched):
  `DS4Windows-current-2026-09-05-b58-activation-feedback` includes the post-b57
  client activation cancellation/admission and ordinary/Switch 2 live-policy
  fixes. Fresh .NET publish and regular Go broker build succeeded; full Go suite
  and vet pass. Its isolated launcher parses and all four payload pins match.
  The six lab config/persona copies match b56, with GL/GR bindings retained and
  no copied credentials/evidence. This prepares the next core connection test;
  b56 stays live and all physical acceptance remains separate.
- Registered input matrix now checks decoded Pro USB/BLE, joined Joy-Con and
  standalone L/R vertical/horizontal through the actual registration host and
  canonical mapper into all six target broker encoders. 110 new cases verify
  button/trigger/release fields, explicit extra bindings without native leakage,
  raw-stick precision/orientation and Pro-only rear-bit rejection on Joy-Cons.
  All-source physical acceptance remains open: the matrix fakes OS transport
  and profile loading and does not observe a virtual device or game. No runtime
  code changed; b56 and staged b58 remain untouched. See the dedicated input
  matrix protocol note and dated ledger for exact scope/results.
- Detailed dated evidence and known caveats remain in
  `controller-platform-validation-2026-09-04.md` and the referenced protocol notes.
- Current next hardware action: reconcile the user's failed unplug report with
  the current Running b56 pump and fresh USB reports; cable-state clarification
  is pending. Confirm automatic row/output removal and physical reconnect before
  completing input inventory and Pro Bluetooth. b56 remains
  app 35880 / broker 16976; b54 was stopped/closed and b55 never launched.
  A later read-only sample still received 64 valid USB reports. b52 counter
  continuity and all four feedback pulse/neutral pairs are already recorded.
  September 5 03:03 local PnP ancestry places the present Pro on physical AMD
  xHCI root-hub port 5, not the virtual USB/IP bus. This is current enumeration,
  not proof about the cable state at the time of the failed unplug report.
  Later log readback shows exact virtual removal and reattachment at 01:19:52
  and 01:19:55 local, coinciding with diagnostic dump completion. This is not
  physical unplug acceptance; the heap snapshot describes the earlier lifetime.
- Installed-state comparison at `2026-09-05T04:43:31.4950924Z` after b56 startup matches all 34
  captured pre-launch entries: installed hashes, task definitions, startup files
  and normal profile/configuration hashes/timestamps.
  This is the measured inventory, not a claim about every machine setting.

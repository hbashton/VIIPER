# Controller platform provenance ledger, 2026-08-29

This ledger applies to the Xbox One/Series and physical Switch 2 work. A fact
may be independently implemented only when its destination license and the
source's grant permit it, or when it is a protocol fact recovered through a
documented clean-room process. No external binary is a code donor by default.

## 2026-09-01 retained Xbox transport update

September 5 native activation follow-up is project-authored lifecycle code,
not donor controller-protocol code. It uses the existing pinned usbip-win2
v.0.9.7.7 ABI from `7c219953101cc5d0ec9a0bcb3eb87259cf72bedd` (BSD-2-Clause)
and Microsoft's documented overlapped operation/cancellation contracts:
[DeviceIoControl](https://learn.microsoft.com/en-us/windows/win32/api/ioapiset/nf-ioapiset-deviceiocontrol),
[CancelIoEx](https://learn.microsoft.com/en-us/windows/win32/api/ioapiset/nf-ioapiset-cancelioex),
[GetOverlappedResult](https://learn.microsoft.com/en-us/windows/win32/api/ioapiset/nf-ioapiset-getoverlappedresult).
No source implementation from those documents was copied. Payload/operation
pinning and exact registration cleanup are VIIPER-local implementation. Tests
substitute native calls and use local authenticated test streams; neither driver
cancellation timing nor permanent successful-import retry cleanup is proven by
those tests. Details/evidence are in the September 4 validation ledger's dated
September 5 follow-up and the Xbox exact-removal API contract.

The project-authored Xbox owner is now connected to VIIPER's production USB/IP
reader through a narrow, explicit `retainedusb.ImportDevice` branch guarded by
a nonzero server authority. This is transport composition, not product
registration: no registry/API route creates the Xbox device, and no installer,
driver, Windows binding, physical feedback, or hardware test was added here.

The routed seam now proves offline:

- exact bus/device snapshot selection and descriptor/owner topology agreement;
- one retained owner for EP0 plus the exact interrupt IN/OUT pair, with legacy
  ownership overlap rejected before import;
- complete request consumption, connection-wide live sequence uniqueness,
  bounded queues, and one response/completion per admitted submission;
- delivered-only configuration/interface/halt publication with a cross-lane
  response-visibility barrier and generation-bound retirement;
- inactive interrupt routes stall before owner dispatch, including USB Default
  state and delivered unconfiguration;
- shutdown fences accepted preclassification and retained connections, joins
  retained teardown, and returns drain/neutral failures while conclusively
  legacy imports retain their historical ownership; and
- the currently authorized Xbox wrapper is one-shot and is removed from its
  virtual bus only after Safe disconnect, so a stopped executor is not
  re-imported or silently quarantined.

The 2026-09-01 adversarial follow-up additionally closed joined-error masking,
descriptor mutation/TOCTOU, concurrent pre-session callback entry, same-owner
alias entry, pointer-address ABA, hotplug retention, and dependent pipelined
EP0 lifecycle classification. The retained descriptor topology now requires
one explicit nonzero configuration, interface 0/alternate 0, exactly the two
declared interrupt routes, nonzero intervals, and no hidden IAD/HID/class or
trailing descriptor payload. These are transport correctness results; they do
not establish Windows Xbox binding or product registration.

The follow-up capacity/lifecycle audit replaced the nominal descriptor deep
copy with a fixed-shape preflight and scalar-only seal; kept retained DEVLIST
callbacks inside the server shutdown join; routed API and whole-bus removal
through exact admission cleanup; added an atomic closed-bus/Add fence; made
safe one-shot absence idempotent without address-based successor removal;
latched failed UNLINK retirement as a scheduler terminal fault; and made owner
claim plus device association one cleanup-atomic publication.

The final retained-seam audit added opaque exact registration and empty-bus
incarnations, exact API stream/timer ownership with terminal-state deletion and
panic containment, server-lifetime owner/failure proof latches, and shutdown
joining for direct descriptor callbacks. It validates representable and exact
USB/IP `devid` before callbacks or variable-length reads, reserves sequences at
header arrival, enforces speed-specific fixed endpoint topology, preserves
joined connection-close error leaves, and proves one-shot same-pointer
re-registration plus actual loopback TCP shutdown/removal. These changes are
project-local transport/lifecycle engineering; they add no external code or
new donor dependency.

The next default-off integration step adds one explicit internal Xbox factory,
not a generic API registration. It preflights the exact retained authority and
representable bus plus free 16-bit device-address and registration-token
capacity before consuming the caller's one-shot identity authorization. A
bounded provisional Add repeats the capacity checks atomically, remains
undiscoverable while the retained seal admits its descriptor, and publishes
only after exact authority/bus revalidation. A private bus-bound lifecycle
capability provides O(1) warm operation authentication; two-phase marked
removal and duplicate close/remove joins avoid holding bus/server map locks
while operations drain. The broker handle returns only semantic input and
joining exact removal. Generic Add rejects malformed dynamic identities before
mutation. This is project-local composition and adds no external code or
provenance dependency. Windows binding and product exposure remain gated.

Current offline evidence: `go test -count=1 ./...`, `go vet ./...`, and the
complete repository `go test -race -count=1 ./...` pass; the production retained
Xbox/lifecycle/shutdown sequence passes 20 repetitions with `GOMAXPROCS=1`;
and the complete API package passes 10 race-enabled repetitions after exact
test bus teardown was added. The explicit factory/registration boundary also
passes focused race-enabled repetitions and targeted single-processor stress;
an independent adversarial review found no remaining correctness, security, or
capacity blocker in the frozen retained seam and factory tranche. No hardware
was exercised.
End-to-end USB/IP latency, 500 Hz delivery, Windows API visibility, physical
four-actuator feedback, reconnect provisioning, and release readiness remain
unproven.

| Source or artifact | Exact pin / identity | License observed | Material used | Allowed treatment | Remaining risk |
|---|---|---|---|---|---|
| DS4Windows | `c216e9d71ed17c45bc07bb6d34e264c50d61cd69` baseline | GPL version 3 (`LICENSE.txt`) | local architecture and destination code | modify under project license | preserve upstream notices and user changes |
| VIIPER core/server/lib | `9806b5a2dcbafe3dea161f255249779c4ab1124e` baseline | GPL-3.0-or-later project grant | destination code and current USB/IP architecture | modify under project license | generated/TCP clients have separate MIT grants; do not assume GPL code can enter them |
| usbip-win2 | `7c219953101cc5d0ec9a0bcb3eb87259cf72bedd`, tag `v.0.9.7.7` | BSD-2-Clause | UDE architecture, receive/completion behavior | source reuse is possible with notice; current work uses facts only | keep exact installer/driver hashes and BSD notice with any redistributed source |
| PadForge stable | `0794fd01bd19f4c096b982ffc824b88bce5ed743` | CC BY-NC-SA 4.0 | behavior, scheduling, comparison claims | clean-room behavioral evidence only | noncommercial/share-alike terms are not accepted for project code |
| PadForge development | branch `origin/v4-dev` at `b7f58cf852b4028eae582b14d2173b4b716a73ee`; focused feedback change `49377fb4`; per-profile host-loop changes `88686582`/`b7f58cf8` | CC BY-NC-SA 4.0 | development-only false-trigger release test, payload-sensitive diagnostic/jitter risk, and explicit 1/2/4/8/16 ms application-loop targets | clean-room behavioral evidence only | do not attribute development behavior to v4.3.2 or interpret a named 500 Hz loop option as physical/report/end-to-end evidence; fetched `latest-v4-dev` tag conflicted with the existing local tag, so the branch commit is authoritative here |
| HIDMaestro stable/current authored source | `46054b862830fcec7bc98d72ccb7c4f0c0179fb1`; `9df50410230c11b410f43909ede0e5fc8b23d15b`; driver/companion/shared-memory/Microsoft-profile/latency components byte-identical, while common `HMController` changed for Valve/Triton raw routing; current WGI investigation document SHA-256 `DABC00BA3D424D4758E7A5B3163EFF19BB884761370D23193BDC7A920F3E1D18` | MIT | two-motor/raw XUSB behavior, one latest seqlock snapshot with separate HID/XUSB doorbells, lossy 64-slot output ring, narrow latency harness, and source-reported Windows feedback compatibility limits | no donor import: local typed feedback and scheduler contracts supersede it; patterns and compatibility results remain behavioral test references | profiles, descriptors, reverse-engineered inbox bindings, global IPC ACLs, signed binaries, and third-party payloads need separate provenance; the WGI document's USB-parent explanation is an inference and its GameInput statements conflict internally |
| PadForge stable/development SDL3.dll | git blob `407c96df...`; SHA-256 `AE1FFD8C537ADDC190F7C874430488AE501665AFDCAC0297682F2B2F33243487`; file version 3.5.0.0 | binary grant not inferred solely from fork license | identifies exact Switch 2 transport used by both branches | audit/reference only | source relationship is pinned to fork `d98c580...`, but custom BLE file has transcribed-source risk |
| HIDMaestro stable DLL | SHA-256 `BD42A99BCB260435CE25796C54A4B792F8A2CED6AB78659C0CF926011663938E` | not inferred from repository license alone | identifies PadForge 4.3.2 payload | audit/reference only | binary build provenance and signing chain remain separate |
| HIDMaestro development DLL | SHA-256 `FA0517F4BDAABFE3EE93B8E6393215DAFC5622602430C244748BBC0FFBBF6055` | not inferred from repository license alone | identifies PadForge development payload | audit/reference only | same risk as stable binary |
| OpenXInput DLL bundled by PadForge | SHA-256 `EF0124DF707564CB64AEE909374B67B429EC1B132DBC508F30155FB0091DDF47` | no normal OSS grant found | competitor dependency identification | do not redistribute, link, derive, or depend on it | legal and supportability blocker |
| Switch2Connect inherited protocol core | current `4487322a306f04efa27682e3f3a508635a84fd98`; initial source `25f631fccc75df849b368e516f272890b40aad4a`; closely matching upstream `switch2-controllers` `e79720b6a3042f710b3a1c7470dc9575c42e560a` | current tree has GPL headers, but the matching upstream and initial core had no identified license | behavior and corroborating Switch 2 facts from `controller.py`/`discoverer.py` | protocol facts only; do not copy implementation, static association payloads, control flow, or tables | a later repository header cannot by itself establish rights to inherited expression; core also mixes protocol, UI, logging, I/O, and sensitive material |
| Switch2Connect v2.6.1 discovery file | `688f8149ff5441efad713997def484c3cc5e90cc`, `src/discoverer.py:42,1606-1661` | explicit GPL-3.0-or-later file header | BLE company ID `0x0553`, little-endian VID/PID offsets, minimum 16-byte manufacturer value, remembered-host bytes 10..15, and zero/local/foreign discovery classification | facts independently expressed in DS4Windows' GPL destination; no callback/control-flow or bundled artifact copied | advertisement host data is a discovery hint, not authentication, encryption, bond proof, or authority to change controller association |
| Switch2Connect later file-level authored Python | current `4487322a306f04efa27682e3f3a508635a84fd98`; `usb_hid_controller.py` introduced `430a3dd7ffbdedeab9eb116024d62ca92d3fd827`; `esp32_rumble_dispatcher.py` introduced `83fb9bf885b791ed5dc8dad2d99bca0f6d83b212` | explicit GPL-3.0-or-later file headers | USB topology/lifecycle and newest-state/rolling-ID behavioral patterns | deliberate adaptation is license-compatible with these GPL destinations only after preserving notice and independently validating every protocol/capture-derived fact; current implementation copies no donor expression | do not inherit broadcast-to-all-PID writes, callback-blocking reads, host-timed cadence guesses, or packed-field limiter; `system_bt_pair_rumble.py` and `windows_ble_parameters.py` have only repository-level GPL evidence and remain reference-only pending clarification |
| Switch2Connect bundled WinUHid DLLs/drivers | tree at `4487322a...`; DLL SHA-256 `4678BA3ACF80A0D7A3857F93A4400CA02742B56789BEC34A8F575CC776053FB4`; Devs `4877A17D97A485E0E0176DBA7B47ADEA1A1FA1C6218D776160FEC73DA1722CF4`; Driver `0667CF90F1ED5E47D2CA5A58B99435EF0555045D5D6E87606548A09396CBF3EE` | no component-separated provenance located | identifies an opaque Xbox backend | do not execute, redistribute, or depend on it | unsigned/no version resource; no manifest ties these files to an upstream source revision |
| WinUHid upstream authored source | `d880a9f3a42580ea7f6b96d37201f965feb4b310` | MIT | Xbox-compatible HID design and conformance-test evidence | possible donor only after file/history review and notice; current work uses facts | does not establish that Switch2Connect's bundled binaries were built from this revision |
| Microsoft GIP/XInputHID documentation package | official `https://dlassets-ssl.xboxlive.com/public/content/GIP/GIPDocumentation.zip`; ZIP SHA-256 `BFE0E08D5915A09375CB7909F3ACF35A1BF35A9E48C293969C62879FDDD517CF`; XInputHID DOCX SHA-256 `AAF46FC046BF46F730313FBC00757D76F800246A9683166AB5EEE36BFF55829B`; Original GIP DOCX SHA-256 `27A664B206FC4B2C5B0DB8E4DDA7C7575ECA2DDA18318376CDEFC984526ED37` | official Microsoft documentation; no standalone open-source grant inferred from the package | standardized XInputHID descriptor/input/output report facts, GIP facts, and documented custom-INF architecture | independently implement facts; do not reproduce package prose or the internally inconsistent sample INF wholesale | no-INF compatible-ID sentence conflicts with official ID rules; custom-INF runtime behavior and arbitrary-VID binding remain unproven |
| Microsoft Open Specifications MS-GIPUSB 1.0 | revision 1.0, published 2024-09-16; official DOCX SHA-256 `810F2841AF3FD832E5EBBDFB581D67413DDA5914364E713AEB35D63483D6D9B0` | Open Specifications notice permits implementation copies/necessary excerpts and included schemas/samples; patent/trademark reservations remain | exact USB descriptors, `XGIP10` binding, framing, lifecycle, metadata, input, motor, ACK and security-opt-out facts | primary implementation specification with notice/citation; use a lawfully assigned VID/PID | Windows-version behavior and physical/API latency still require tests; spec requires IN interval at least 4 ms |
| Microsoft OS 1.0 and HIDClass ID documentation | exact URLs in trace table; accessed 2026-08-29 | Microsoft documentation terms; OS-descriptor specification has its own implementation license | eight-byte CompatibleID rule and `HID_DEVICE_SYSTEM_GAME` special-hardware-ID classification | facts and citations only | establishes a contradiction, not an encoding; no guessed truncation or alias is allowed |
| Microsoft Learn UDE/UdeCx/KMDF/UMDF/HID/VHF pages | exact URLs in trace table; accessed 2026-08-29 | Microsoft documentation terms; WDK headers/libraries remain under their own terms | supported virtual USB/HID architecture, endpoint/lifecycle APIs, and framework/mode boundaries | facts and documented APIs only; do not clone sample implementation structure | UdeCx/VHF are transport backends, not Xbox promotion; production driver remains unimplemented and unsigned |
| Microsoft Learn XInput/GameInput/WGI pages | exact URLs in trace table; GameInput view `gdk-2604`, WGI view `winrt-26100`, accessed 2026-08-29 | Microsoft documentation/API terms | two-channel XInput and four-channel GameInput/WGI value shapes | facts and documented API contracts only | four independent application-to-virtual-report delivery remains a vertical-slice unknown |
| Microsoft Learn Bluetooth GATT pages | exact URLs in trace table; accessed 2026-08-29 | Microsoft documentation/API terms | WinRT service/characteristic/property/notify/write primitives | facts and documented APIs only | generic GATT docs establish no Switch 2 UUID, association, encryption, report, HD-rumble, interval, or rate fact |
| Microsoft Learn driver-signing pages | exact URLs in trace table; accessed 2026-08-29 | Microsoft documentation terms | packaging and production-signing constraints | facts and citations only | attestation is testing-only; any custom INF/UdeCx/VHF production path needs a separately authorized signing/release plan |
| Windows inbox INF observations | Windows `10.0.26100.8972`; `dc1-controller.inf` SHA-256 `34C417A8A35BE801D8A16B230E5968B6F932DA89E3DF83FFF6C6BE2EAACA0975`; `xboxgip.inf` `04B74435E7F23576DD9652BA04C5071891B999674695B8779C89BF0F94A317EB`; `xinputhid.inf` `DA1F33CDE2926A673F02DAF72617253464DA487A30E2FD404E3ADA41FD619D48`; `input.inf` `66D408138C0914CDCF77B846042215E9DEFCF8ED3116C6FC54C7643F5516CEF4` | Microsoft Windows components; no redistribution grant inferred | build-specific binding-section and hardware-ID observations | facts only; never copy or redistribute inbox files | other Windows builds must be inspected/tested; local files do not prove USB/IP runtime binding |
| Switch2Connect bundled firmware/binaries/PDF | tree at `4487322a...` | no component-separated provenance located | none | do not ship or use as implementation inputs | third-party rights and embedded payloads unknown |
| SDL current/release | `c71abd08605b8bb7078372307a93274725c99fe0`; `release-3.4.x` tip `58ff755ea58cd4c64e72678f4b3e0d720aa79206` | zlib | Switch 2 USB behavior, calibration parser, HD-rumble encoder facts | permissible donor with notice; new code remains independently tested | SDL Bluetooth path is a stub; unknown commands are not specifications |
| hifihedgehog SDL BLE experiment | `d98c5804a9d20b0d96e993741797878c86b8f1e1` | top-level zlib; file says portions were transcribed | negative-control behavior only | facts-only until the transcribed portions are provenance-cleared | unverified offsets, single-slot loss, and WinRT lifecycle defects |
| xone | `3484f603484782dd7551c64e5a33fc602b127051` | examined files GPL-2.0-or-later; GPLv2 root text | wired GIP framing, input/rumble body and auth behavior facts | clean-room protocol-facts implementation; no code/structure copying | destination-specific GPL compatibility and generated-client grants need legal review before any reuse |
| xow | current `master` `d335d6024f8380f52767a7de67727d9b2f867871`, 2022-04-24 | root GPLv2 text; audited source headers say GPL-2.0-or-later | physical Xbox Wireless Adapter receiver/GIP/uinput facts and negative-control evidence | protocol facts only; no code/structure copying is needed | not virtual Xbox presentation; its two-motor `FF_RUMBLE` heuristic and forced 10 ms delay are unsuitable donors |
| xpadneo | `3acca9f5e211edb601000bb64767b78b2468f787` | GPL-3.0 (`LICENSE.md`) | Xbox Bluetooth corroboration | facts-only for this phase | Bluetooth, receiver, and wired packet families must stay distinct |
| Linux xpad | `cf72cbb39da84b6f02f90c07f33b102fc10b16f0`, `drivers/input/joystick/xpad.c` | file SPDX GPL-2.0-or-later; kernel root license context must be preserved | GIP constants, independent D-pad bits, public two-motor FF mapping | protocol facts only; no code/structure copying | public FF path leaves impulse-trigger bytes zero and does not prove four-channel app delivery |
| ndeadly Switch 2 research | `d1c5a7f7ba298f83017fae84952a4e6d2ef8fc92` | no license found | capture-backed protocol facts | facts-only, independently implemented | repository contains raw LTKs and sensitive captures; never copy or redistribute them |
| switch2-controllers | `e79720b6a3042f710b3a1c7470dc9575c42e560a` | no license found | corroborating protocol facts | facts-only | no code copying |
| joycon2py | `9d80b764682a171580fd134b96f00866df6ae398` | MIT | prototype corroboration | possible donor after file review; current work uses facts | prototype behavior is not a production contract |
| joycon2cpp | `1db6999a17a36e24e9d5d09f9fafb4fb0adce6f1` | MIT | Windows/WinRT wiring and benchmark-scope evidence | possible donor after file review; current work uses facts | its Pro rumble is explicitly untested |
| BlueRetro | `e1a9831a875f5313a923160a1379a7ebbfaa2b11` | Apache-2.0; archived project | independent calibration order, model IDs, association lifecycle and output-envelope corroboration | facts now; code reuse only after file-level review and notice | unsafe length assumptions, fixed handles, incomplete Joy-Con mapping, key/address persistence and no cadence evidence |
| JoyShockLibrary | `023dfb6f83d27134bf0cb0be085bfab4a251ffa5` | MIT | negative-control inventory | no Switch 2 implementation input | no Switch 2 PIDs/protocol; README says Nintendo HD rumble unsupported |
| project-owned passive Switch 2 Pro USB captures | two out-of-tree sessions, 2,048 and 4,096 reports; exact raw identities deliberately omitted from git | project-owned, subject to device/user data policy | framing, strict-decoder and passive-cadence evidence | raw evidence stays operator-controlled; commit only fully synthetic or independently reviewed sanitized fixtures | remove MAC, serial, path derivatives, raw digests that link a fixture to a session, host identity, and environmental sensor values |
| committed Switch 2 common-`0x05` golden fixture | fully synthetic public-layout vectors; no raw capture ancestry | project-authored synthetic facts | strict decode/replay and counter-order tests | safe to commit and redistribute with project tests | must remain algorithmically constructed; never replace values with a hardware fingerprint |
| project-authored four-actuator-to-Switch-2 synthesis policy | local `Switch2HdRumbleFeedbackTranslator`; licensed constants traced to SDL `c71abd08605b8bb7078372307a93274725c99fe0` | project code under DS4Windows license; SDL fact source is zlib | exact SDL body-rumble compatibility arithmetic plus an explicitly approximate side-local impulse policy | offline translator/tests only; no PadForge or unlicensed implementation input | physical sidedness, energy, cadence, stop, thermal behavior, and actuator onset remain hardware gates; the translator authorizes no I/O |
| project-authored DualSense-trigger-to-Switch-2 approximation | local `DualSenseAdaptiveTriggerHdRumbleTranslator`; DS4Windows' own `TriggerLabEffectEncoder`; behavioral comparison against Switch2Connect `61ac6642ce12fe7217e38a860b14863b18ca7e28`, `src/virtual_controller.py:2399-2420,4641-4737` | project code and DS4Windows source are GPL-3.0; cited Switch2Connect file is explicitly GPL-3.0-or-later | Switch2Connect supplies evidence for a side-local, trigger-held tactile substitute and a bounded 150 ms program-change punch; the project implementation independently decodes supported effect modes and preserves a bounded three-region strength/frequency envelope in the existing owner | compatible behavioral evidence only; no Python expression, timing state, logging, threading, or write path copied | Switch 2 has no adaptive resistance actuator; this is replay-verified approximation only, and physical onset, energy, perceptual value, and thermal behavior remain hardware gates |
| project-owned future active captures | not yet collected | project-owned, subject to device/user data policy | rate, volatile LED, rumble basis and cleanup evidence after gates pass | sanitize and commit only after redaction review | no pairing/key/NVM commands; remove device/host identity and environmental data |

## Mandatory source trace

| Source | Exact pin, document hash, or access identity | Symbol / line | Fact | Inference / unknown |
|---|---|---|---|---|
| [xow](https://github.com/medusalix/xow/tree/d335d6024f8380f52767a7de67727d9b2f867871) | `d335d6024f8380f52767a7de67727d9b2f867871` | `xow.cpp:44-63`; `dongle/dongle.cpp:20-36,51-72,137-228,272-290`; `controller/input.cpp:25-35,94-113,141-220`; `controller/controller.cpp:118-145,214-255,260-346`; `controller/gip.h:106-121` | opens a physical Microsoft wireless adapter, unwraps receiver WLAN/GIP, publishes Linux uinput, accepts two-magnitude `FF_RUMBLE`, heuristically derives trigger values, and forces a 10 ms rumble delay | **Fact:** physical receiver input only. **Unknown/not established:** any Windows virtual Xbox persona or genuine four-channel application delivery; do not use it for those claims |
| [MS-GIPUSB 1.0](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-gipusb/e7c90904-5e21-426e-b9ad-d82adeee0dbc) | revision 1.0, 2024-09-16; DOCX SHA-256 `810F2841AF3FD832E5EBBDFB581D67413DDA5914364E713AEB35D63483D6D9B0` | section 2.2.6 tables 5-6; 2.2.9.1 table 9; 2.2.9.1.2 table 11; 3.1.5.4; 3.1.5.6.1 table 56; Appendix B note 1 | exact `MSFT100`/`0x90`/`XGIP10` binding, `FF/47/D0` 64-byte interrupt interface, IN `bInterval >= 4 ms`, lifecycle/ACK/input, and four-motor Direct Motor command; modern Windows removes USB authentication | **Inference:** VIIPER can host this persona over existing USB/IP if every descriptor/control/lifecycle/message rule is implemented. **Unknown:** usbip-win2 behavior, API visibility, feedback return, supported-build matrix, and latency until the vertical slice |
| Local Windows inbox GIP binding | Windows `10.0.26100.8972`; hashes in ledger above | `dc1-controller.inf:14,40-63,85-89`; `xboxgip.inf:14,26-56` | `USB\MS_COMP_XGIP10` selects the inbox Xbox controller stack on this build without a Microsoft VID/PID match | **Inference:** exact no-new-package candidate. **Unknown:** other builds and actual USB/IP enumeration/runtime behavior |
| [Official GIP/XInputHID package](https://dlassets-ssl.xboxlive.com/public/content/GIP/GIPDocumentation.zip) | ZIP `BFE0E08D5915A09375CB7909F3ACF35A1BF35A9E48C293969C62879FDDD517CF`; XInputHID DOCX `AAF46FC046BF46F730313FBC00757D76F800246A9683166AB5EEE36BFF55829B` | XInputHID DOCX paragraphs 121-171, 174-194, 195-214, 236-274 | report 2 carries left impulse, right impulse, left body, right body plus duration/delay/repeat; xinputhid is a HID upper filter; a custom-INF Include/Needs mechanism is documented | **Fact:** report and custom-INF mechanism. **Unknown:** exact runtime flags/version support. **Unknown and contradicted as written:** para 194 no-INF `HID_DEVICE_SYSTEM_GAME` CompatibleID; never truncate or guess it |
| [Microsoft OS 1.0 descriptor](https://learn.microsoft.com/en-us/windows-hardware/drivers/usbcon/microsoft-os-1-0-descriptors-specification) and [HIDClass IDs](https://learn.microsoft.com/en-us/windows-hardware/drivers/hid/hidclass-hardware-ids-for-top-level-collections) | living pages, accessed 2026-08-29; no immutable document hash | OS 1.0 “all IDs must be eight bytes”; HIDClass “Special purpose hardware ID” and “Important notes” | `HID_DEVICE_SYSTEM_GAME` is a special HID hardware ID; HIDClass generates no compatible IDs | **Fact:** the XInputHID DOCX no-INF sentence conflicts with these rules. **Unknown:** a corrected Microsoft-supported arbitrary-VID no-INF recipe; absence of one is a gate, not permission to invent it |
| [UDE client model](https://learn.microsoft.com/en-us/windows-hardware/drivers/usbcon/writing-a-ude-client-driver), [UdeCx endpoint type](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/udecxusbdevice/nf-udecxusbdevice-udecxusbdeviceinitsetendpointstype), [UdeCx endpoint callback](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/udecxusbdevice/nc-udecxusbdevice-evt_udecx_usb_device_endpoints_configure), [KMDF development](https://learn.microsoft.com/en-us/windows-hardware/drivers/wdf/using-the-framework-to-develop-a-driver), [UMDF FAQ](https://learn.microsoft.com/en-us/windows-hardware/drivers/wdf/user-mode-driver-framework-frequently-asked-questions), [HID architecture](https://learn.microsoft.com/en-us/windows-hardware/drivers/hid/hid-architecture), and [VHF](https://learn.microsoft.com/en-us/windows-hardware/drivers/hid/virtual-hid-framework--vhf-) | living pages, accessed 2026-08-29; no immutable document hash | `UdecxUsbDeviceInitSetEndpointsType`; `EVT_UDECX_USB_DEVICE_ENDPOINTS_CONFIGURE`; `WdfDeviceCreate`; `WdfIoQueueCreate`; `VhfCreate` | UdeCx creates USB descriptors/endpoints/lifecycle and requires at least KMDF 1.15 for the cited DDI; UMDF is the preferred starting point only when its feature set suffices; VHF HID source is currently kernel-only; none supplies Xbox semantics | **Inference:** UdeCx or VHF can later sit below the existing VIIPER semantic seam. **Unknown:** latency benefit and product value until measured; no native phase is authorized by these facts |
| [XInput](https://learn.microsoft.com/en-us/windows/win32/api/xinput/ns-xinput-xinput_vibration), [GameInput](https://learn.microsoft.com/en-us/gaming/gdk/docs/reference/input/gameinput/structs/gameinputrumbleparams?view=gdk-2604), and [WGI](https://learn.microsoft.com/en-us/uwp/api/windows.gaming.input.gamepadvibration?view=winrt-26100) | versioned views/accessed 2026-08-29; no immutable document hash | `XINPUT_VIBRATION`; `GameInputRumbleParams`; `GamepadVibration` | public XInput supplies two body values; GameInput/WGI name four independent body/trigger values | **Inference:** retain one atomic four-lane feedback value end to end and accept zero trigger lanes from XInput-only titles. **Unknown:** which representative games/APIs deliver four independent basis vectors through the eventual persona |
| HIDMaestro current WGI silent-sink investigation | `9df50410230c11b410f43909ede0e5fc8b23d15b`; document SHA-256 `DABC00BA3D424D4758E7A5B3163EFF19BB884761370D23193BDC7A920F3E1D18` | `docs/investigations/wgi-silent-sink-2026-04/finding.md`: executive summary, post-fix evidence matrix, architectural explanation, open questions, caveats, consumer impact | **Source-reported observation, not independently reproduced here:** on Windows 11 `26200.8115`, direct XInput motor bytes reached the experiment's ROOT-enumerated UMDF2 XUSB virtual, while WGI/Chromium produced probes and idle clears but no motor-bearing bytes at three instrumented layers; physical positive controls were tactile only | **Inference in source:** WGI may require a USB-bus/xusb22 parent. **Contradiction in source:** the evidence matrix says GameInput `SetRumbleState` reached the virtual, while later open-question and consumer-impact sections say GameInput was blocked. Treat GameInput and the proposed cause as unknown. This does not prove that VIIPER USB/IP will work, but it rules out treating HIDMaestro's ROOT presentation as a complete compatibility oracle |
| [Windows GATT client](https://learn.microsoft.com/en-us/windows/apps/develop/devices-sensors/gatt-client) and [characteristic properties](https://learn.microsoft.com/en-us/uwp/api/windows.devices.bluetooth.genericattributeprofile.gattcharacteristicproperties?view=winrt-26100) | accessed 2026-08-29; no immutable document hash | `BluetoothLEDevice`; `GattSession.MaintainConnection`; `GattCharacteristicProperties`; CCCD/`ValueChanged` | WinRT exposes service/characteristic objects and read/write/notify/indicate primitives, not raw transport; a vendor profile must already be known | **Inference:** use capture-pinned UUID/property roles in a serialized generation-bound adapter. **Unknown:** Switch 2 association/security, interval, report cadence, and haptics; generic GATT docs prove none of them |
| [Windows driver signing](https://learn.microsoft.com/en-us/windows-hardware/drivers/install/windows-driver-signing-tutorial) and [signing options](https://learn.microsoft.com/en-us/windows-hardware/drivers/dashboard/driver-signing-offerings) | accessed 2026-08-29; no immutable document hash | “Overview”; “Hardware Lab Kit tested”; “Attestation signed drivers for testing scenarios” | kernel drivers must be signed; HLK/dashboard is the recommended production path; attestation is testing-only and not Windows certification | **Inference:** raw GIP over the already supported importer avoids a new project driver package. **Unknown/separate authority:** production custom-INF, UdeCx, or VHF signing and distribution |

The official ZIP and nested DOCX files above were downloaded, hashed, and
inspected entirely in memory. No copy was written to the repository, a temp
directory, or the desktop. Windows inbox INFs were inspected read-only and are
not project artifacts.

## September 5: native Xbox activation attempt policy

Source: `vadimgrn/usbip-win2@7c219953101cc5d0ec9a0bcb3eb87259cf72bedd`
(`v.0.9.7.7`), BSD-2-Clause. Examined `include/usbip/vhci.h`,
`userspace/libusbip/src/vhci.cpp`, `drivers/ude/vhci_ioctl.cpp`,
`drivers/ude/device.cpp` and `drivers/ude/persistent.cpp`. The upstream
[reattachment documentation](https://github.com/vadimgrn/usbip-win2/wiki/How-automatic-reattachment-works)
was checked separately on September 5.

Facts: `PLUGIN_HARDWARE_ONCE` uses function `0x806` / IOCTL `0x0022e018`
with the same 1,100-byte request and 8-byte response as normal attach. Its
`one_attempt` flag prevents background retries of that initial call when it
fails. It does not disable receiver-loss reattachment after a successful import.
`STOP_ATTACH_ATTEMPTS` accepts a location, but the driver matches that location
by its 32-bit hash and cancels currently queued requests; it is not a durable
per-device no-retry setting or proof that later detach work cannot enqueue again.

VIIPER's original implementation selects the documented one-attempt IOCTL only
for canonical production Xbox export aliases, using the existing exact payload
builder. Legacy numeric imports retain their previous operation. Five original
fake-native boundary tests cover operation selection, preserved payload/port,
failure without fallback, invalid requests and returned-byte validation. No donor
code, driver binary, new fixture, native stop/detach, registry change or global
retry setting was copied or introduced. Tests do not call an installed driver.

## Clean-room rules

1. Cite the exact source revision and file for every imported fact or constant.
2. Write destination code from a short fact specification, not while copying
   the donor's control flow, naming, comments, or test vectors.
3. Prefer licensed SDL and official Microsoft documentation where they
   corroborate the same fact.
4. Treat PadForge as a behavioral black-box/architecture comparison only.
5. Keep raw GIP, Xbox Bluetooth, receiver, Switch 2 USB, and Switch 2 GATT
   packet families in distinct codecs. Length is never a transport detector.
6. Do not commit third-party encrypted sessions, LTKs, credentials, device
   certificates, MACs, serials, or remembered-host addresses.
7. Generated test fixtures must state whether every byte is synthetic,
   licensed, or newly project-captured, and include a redaction manifest.
8. Any later source reuse into VIIPER's MIT-generated clients requires a
   separate license review; permission for GPL core code is not assumed.

## Capture fixture manifest requirement

Every future fixture must record:

- schema version and SHA-256 of the exact byte payload;
- source type (`synthetic`, `project-captured`, or licensed donor);
- evidence status (`fact`, `inference`, or `unknown-field-preserved`);
- controller model, transport, direction, and firmware revision;
- report ID or characteristic UUID and lifecycle generation;
- host monotonic delta, never a wall-clock identifier unless needed;
- every redaction and the replacement rule;
- reviewer and collection procedure;
- explicit confirmation that no key, certificate, MAC, serial, console
  address, or account-linked identifier remains.

This ledger must be updated before adding a new external fixture, descriptor,
protocol constant, algorithm, binary, or copied source fragment.

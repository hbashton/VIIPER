# Controller platform provenance ledger, 2026-08-29

This ledger applies to the Xbox One/Series and physical Switch 2 work. A fact
may be independently implemented only when its destination license and the
source's grant permit it, or when it is a protocol fact recovered through a
documented clean-room process. No external binary is a code donor by default.

| Source or artifact | Exact pin / identity | License observed | Material used | Allowed treatment | Remaining risk |
|---|---|---|---|---|---|
| DS4Windows | `c216e9d71ed17c45bc07bb6d34e264c50d61cd69` baseline | GPL version 3 (`LICENSE.txt`) | local architecture and destination code | modify under project license | preserve upstream notices and user changes |
| VIIPER core/server/lib | `9806b5a2dcbafe3dea161f255249779c4ab1124e` baseline | GPL-3.0-or-later project grant | destination code and current USB/IP architecture | modify under project license | generated/TCP clients have separate MIT grants; do not assume GPL code can enter them |
| usbip-win2 | `7c219953101cc5d0ec9a0bcb3eb87259cf72bedd`, tag `v.0.9.7.7` | BSD-2-Clause | UDE architecture, receive/completion behavior | source reuse is possible with notice; current work uses facts only | keep exact installer/driver hashes and BSD notice with any redistributed source |
| PadForge stable | `0794fd01bd19f4c096b982ffc824b88bce5ed743` | CC BY-NC-SA 4.0 | behavior, scheduling, comparison claims | clean-room behavioral evidence only | noncommercial/share-alike terms are not accepted for project code |
| PadForge development | `474d890db12c71d3a825ffd16c8d9dc1223aaff2` | CC BY-NC-SA 4.0 | development behavior kept separate from stable | clean-room behavioral evidence only | do not attribute development behavior to v4.3.2 |
| HIDMaestro stable/current authored source | `46054b862830fcec7bc98d72ccb7c4f0c0179fb1`; `9df50410230c11b410f43909ede0e5fc8b23d15b` | MIT | Windows presentation architecture and benchmark definition | may reuse MIT code only after file-level provenance review; current work uses facts | profiles, descriptors, captures, signed binaries, and third-party payloads need separate provenance |
| HIDMaestro stable DLL | SHA-256 `BD42A99B...3938E` | not inferred from repository license alone | identifies PadForge 4.3.2 payload | audit/reference only | binary build provenance and signing chain remain separate |
| HIDMaestro development DLL | SHA-256 `FA0517F4...6055` | not inferred from repository license alone | identifies PadForge development payload | audit/reference only | same risk as stable binary |
| OpenXInput DLL bundled by PadForge | SHA-256 `EF0124DF...DDF47` | no normal OSS grant found | competitor dependency identification | do not redistribute, link, derive, or depend on it | legal and supportability blocker |
| Switch2Connect authored Python | `4487322a306f04efa27682e3f3a508635a84fd98` | GPL-3.0-or-later headers and GPLv3 root text | behavior and corroborating Switch 2 facts | facts-only clean-room implementation unless deliberate GPL reuse is reviewed | code mixes protocol, UI, logging, I/O, and opaque dependencies |
| Switch2Connect bundled WinUHid DLLs/drivers | tree at `4487322a...` | no component-separated provenance located | identifies an opaque Xbox backend | do not execute, redistribute, or depend on it | source, rights, signing, and actual Xbox protocol are unknown |
| Switch2Connect bundled firmware/binaries/PDF | tree at `4487322a...` | no component-separated provenance located | none | do not ship or use as implementation inputs | third-party rights and embedded payloads unknown |
| SDL current/stable | `c71abd08605b8bb7078372307a93274725c99fe0`; `147a8ee32dbf9ac02f3794964490687b6bbda1bc` | zlib | Switch 2 USB behavior, calibration parser, HD-rumble encoder facts | permissible donor with notice; new code remains independently tested | SDL Bluetooth path is a stub; unknown commands are not specifications |
| hifihedgehog SDL BLE experiment | `d98c5804a9d20b0d96e993741797878c86b8f1e1` | top-level zlib; file says portions were transcribed | negative-control behavior only | facts-only until the transcribed portions are provenance-cleared | unverified offsets, single-slot loss, and WinRT lifecycle defects |
| xone | `3484f603484782dd7551c64e5a33fc602b127051` | examined files GPL-2.0-or-later; GPLv2 root text | wired GIP framing, input/rumble body and auth behavior facts | clean-room protocol-facts implementation; no code/structure copying | destination-specific GPL compatibility and generated-client grants need legal review before any reuse |
| xpadneo | `3acca9f5e211edb601000bb64767b78b2468f787` | GPL-3.0 (`LICENSE.md`) | Xbox Bluetooth corroboration | facts-only for this phase | Bluetooth, receiver, and wired packet families must stay distinct |
| Linux xpad | exact kernel revision must be pinned before use | GPL-2.0-only kernel source | potential USB device tables/protocol corroboration | facts-only; not yet an implementation input | no material fact may cite an unpinned moving branch |
| ndeadly Switch 2 research | `d1c5a7f7ba298f83017fae84952a4e6d2ef8fc92` | no license found | capture-backed protocol facts | facts-only, independently implemented | repository contains raw LTKs and sensitive captures; never copy or redistribute them |
| switch2-controllers | `e79720b6a3042f710b3a1c7470dc9575c42e560a` | no license found | corroborating protocol facts | facts-only | no code copying |
| joycon2py | `9d80b764682a171580fd134b96f00866df6ae398` | MIT | prototype corroboration | possible donor after file review; current work uses facts | prototype behavior is not a production contract |
| joycon2cpp | `1db6999a17a36e24e9d5d09f9fafb4fb0adce6f1` | MIT | Windows/WinRT wiring and benchmark-scope evidence | possible donor after file review; current work uses facts | its Pro rumble is explicitly untested |
| project-owned future captures | not yet collected | project-owned, subject to device/user data policy | golden protocol evidence | sanitize and commit only after redaction review | remove MAC, serial, LTK, console address, host identity, account/game data |

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

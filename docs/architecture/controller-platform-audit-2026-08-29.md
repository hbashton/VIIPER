# Controller platform audit, 2026-08-29

Status: the pinned PadForge/HIDMaestro, Switch2Connect, mandatory Microsoft,
and xow source audits are complete enough to select the current offline Phase
1/2 work. This is not a completed full-platform audit: Windows binding,
supported-version, hardware, and latency vertical slices remain open. Offline
implementation and adversarial review continue before the user-authorized,
bounded hardware-output phase. Xbox One/Series presentation and physical
Switch 2 support are not production-ready. This document keeps direct
observations, inferences, and unknowns separate.

## Scope and exact pins

| Component | Revision audited | Role |
|---|---|---|
| DS4Windows | baseline `c216e9d71ed17c45bc07bb6d34e264c50d61cd69`; feedback foundation `16803a6ba3e4168a13c7c58232f8fe1a3bc10b95` | physical input, mapping, feedback ownership |
| VIIPER | baseline `9806b5a2dcbafe3dea161f255249779c4ab1124e`; scheduler foundation through `e965b50` | virtual device/protocol owner and USB/IP server |
| usbip-win2 | tag `v.0.9.7.7`, `7c219953101cc5d0ec9a0bcb3eb87259cf72bedd` | supported Windows USB/IP importer |
| Switch2Connect | `main`/`v2.7`, `4487322a306f04efa27682e3f3a508635a84fd98` | Switch 2 research implementation, not a specification |
| Switch2Connect diagnostic branch | `161448949a96cf0432f73c27d07982e092ee41a0` | divergent experiment only |
| Switch2Connect NSO branch | `71731df98cdf8af8aeb810ccfa1a25ab38da2a86` | divergent experiment only |
| PadForge stable | `0794fd01bd19f4c096b982ffc824b88bce5ed743` (`v4.3.2`) | competitor behavior evidence |
| PadForge development | `b7f58cf852b4028eae582b14d2173b4b716a73ee` | development behavior kept separate from stable; fetched branch head, not the conflicting local `latest-v4-dev` tag |
| HIDMaestro stable payload | `46054b862830fcec7bc98d72ccb7c4f0c0179fb1` (`1.7.0`) | PadForge stable virtual backend |
| HIDMaestro current | `9df50410230c11b410f43909ede0e5fc8b23d15b` (`1.7.1`) | current competitor backend |
| SDL | `c71abd08605b8bb7078372307a93274725c99fe0`; `release-3.4.x` tip `58ff755ea58cd4c64e72678f4b3e0d720aa79206` | permissive Switch 2 USB reference |
| PadForge SDL fork and exact shipped DLL | `d98c5804a9d20b0d96e993741797878c86b8f1e1`; SHA-256 `AE1FFD8C537ADDC190F7C874430488AE501665AFDCAC0297682F2B2F33243487` | USB plus custom WinRT BLE behavior; same binary in stable/development |
| WinUHid upstream | `d880a9f3a42580ea7f6b96d37201f965feb4b310` | MIT Xbox-HID implementation and conformance-test evidence; Switch2Connect binaries are not assumed to match it |
| Microsoft GIP/XInputHID documentation package | official `GIPDocumentation.zip`, SHA-256 `BFE0E08D5915A09375CB7909F3ACF35A1BF35A9E48C293969C62879FDDD517CF`; `XInputHID Documentation.docx` SHA-256 `AAF46FC046BF46F730313FBC00757D76F800246A9683166AB5EEE36BFF55829B`; `H001419 - Original GIP Spec.docx` SHA-256 `27A664B206FC4B2C5B0DB8E4DDA7C7575ECA2DDA18318376CDEFC984526ED37` | official standardized HID/GIP reports and driver-stack guidance; XInputHID no-INF binding recipe contains a documented conflict |
| Microsoft MS-GIPUSB | Open Specifications revision 1.0 (2024-09-16); official DOCX SHA-256 `810F2841AF3FD832E5EBBDFB581D67413DDA5914364E713AEB35D63483D6D9B0` | primary raw-GIP USB protocol and inbox-binding specification |
| xone | `3484f603484782dd7551c64e5a33fc602b127051` | wired/dongle GIP protocol facts |
| xow | current `master` `d335d6024f8380f52767a7de67727d9b2f867871` (2022-04-24) | physical Xbox Wireless Adapter receiver input and a negative control for virtual Xbox presentation |
| xpadneo | `3acca9f5e211edb601000bb64767b78b2468f787` | Xbox Bluetooth protocol corroboration only |
| Linux xpad | `cf72cbb39da84b6f02f90c07f33b102fc10b16f0` | GPL kernel USB/Xbox protocol corroboration; facts only |
| Windows inbox binding evidence | Windows `10.0.26100.8972`; `dc1-controller.inf` SHA-256 `34C417A8A35BE801D8A16B230E5968B6F932DA89E3DF83FFF6C6BE2EAACA0975`; `xboxgip.inf` `04B74435E7F23576DD9652BA04C5071891B999674695B8779C89BF0F94A317EB`; `xinputhid.inf` `DA1F33CDE2926A673F02DAF72617253464DA487A30E2FD404E3ADA41FD619D48`; `input.inf` `66D408138C0914CDCF77B846042215E9DEFCF8ED3116C6FC54C7643F5516CEF4` | read-only, build-specific binding evidence; Windows files are not redistribution inputs |
| switch2 controller research | `d1c5a7f7ba298f83017fae84952a4e6d2ef8fc92` | capture-backed Switch 2 facts; no code reuse |
| switch2-controllers | `e79720b6a3042f710b3a1c7470dc9575c42e560a` | corroborating facts; no code reuse |
| joycon2py | `9d80b764682a171580fd134b96f00866df6ae398` | MIT prototype evidence |
| joycon2cpp | `1db6999a17a36e24e9d5d09f9fafb4fb0adce6f1` | MIT WinRT wiring evidence |
| BlueRetro | `e1a9831a875f5313a923160a1379a7ebbfaa2b11` | Apache-2.0 Switch 2 calibration/association/output corroboration; archived and incomplete |
| JoyShockLibrary | `023dfb6f83d27134bf0cb0be085bfab4a251ffa5` | MIT negative control; no Switch 2 protocol support found |

Installed usbip-win2 0.9.7.7 hashes and the untouched test baselines are in
`_results/controller-platform-phase0-baseline-20260829.md` at the workspace
root. The current live DualSense proves that this exact importer can carry a
composite USB topology; it does not prove Xbox compatibility or latency.

## Current dataflow and ownership

```text
physical HID/Bluetooth completion
  -> DS4Windows physical-device parser
  -> DS4Windows profile/mapping state
  -> target-specific VIIPER state publication
  -> VIIPER bounded Source scheduler
  -> interrupt endpoint selection and immutable report
  -> USB/IP RET_SUBMIT response
  -> usbip-win2 UDE/UdeCx completion
  -> Windows class driver and game API

game output
  -> virtual endpoint decoder in VIIPER
  -> DS4Windows callback/dispatch path
  -> physical-controller output owner
```

The intended ownership rule is one semantic mapping stack, one virtual-device
protocol owner in VIIPER, and one writer per physical transport. A possible
future native presentation backend belongs below the same VIIPER Source
claim/resolve seam. It must not fork profile mapping or feedback policy.

The current working tree extends ordered presentation across the Xbox 360
path: DS4Windows journals button and trigger-zero boundaries while replacing
continuous state, and VIIPER receives those states through a lease-bound
producer into the shared fixed-report scheduler. Stream-producer retirement
now purges that producer's pending history, invalidates its lease, commits one
canonical neutral, and requires a post-retirement complete-state
resynchronization without rotating the still-live USB presentation generation.
Capacity overflow uses that fail-closed contract even when no ordered-age
deadline was selected. Final source admission reuses the same still-pending
host URB to carry a mandatory neutral instead of dropping a sequence for which
no `RET_SUBMIT` was sent. Device-level lifecycle serialization also prevents a
pre-reset raw-stream sample from adopting the successor producer epoch, and a
successful USB generation retirement purges its staged recovery snapshot.
Focused and repository-wide offline tests cover claim/admit/commit/defer,
overflow, age fault, neutral/resynchronization, stream handoff, admission
rejection, and generation retirement. This remains uncommitted and has not yet
passed live USB/IP or consumer-observation validation.

Replay freshness is explicit rather than invented: DS4Windows sends
`maximumOrderedAgeMilliseconds` only when
`DS4W_VIIPER_X360_MAX_ORDERED_AGE_MS` is a valid positive policy value, and
VIIPER enables strict ordered-age faulting only when that field is present.
Omission preserves a documented no-age mode with a bounded journal and
fail-closed overflow; it does not protect ordered history from wall-clock age.
A successful claim commit means only that the backend accepted and copied one
complete report. It does not prove that an application observed it. The raw
20-byte producer protocol also has no sender capture timestamp, so a strict age
policy measures VIIPER receipt-to-presentation, not backlog before `ReadFull`.

The mirrored `CFBK` v1 feedback value contains body-low, body-high,
left-trigger, and right-trigger amplitudes plus capability, lifecycle,
ownership, timestamp, TTL, neutral, and stop semantics. It is not wired to a
runtime transport yet. Its mailbox now uses an exact claim-token/complete
contract: completion watermarks advance only after reported delivery, failed
delivery remains eligible, successful Stop also completes release, and a
cursor is lifetime-bound to one mailbox. Runtime writers must still guarantee
completion in `finally`; abandoned-claim recovery is an open integration gate.

## Capability and gap matrix

| Capability | Direct observation | Status and gate |
|---|---|---|
| Existing Xbox 360 virtual input | working tree has ordered DS4Windows egress plus VIIPER lease-bound Source journal and explicit strict/compatibility age policy | offline replay/unit verified; live USB/IP, consumer observation, selected production age, and latency remain unresolved |
| Existing Switch 2 Pro virtual output | VIIPER has a 64-byte USB persona and 24-byte feeder state | implemented; not evidence of physical Switch 2 input |
| Xbox One/Series semantic input | base GIP body is publicly corroborated | implemented in isolated codecs and one generation-fenced persona engine; official spec-vector/unit/property/fuzz verified, with no capture replay or hardware validation |
| Xbox One/Series-class XInputHID USB persona | Microsoft publishes a standardized descriptor with input report 1 and four-actuator output report 2 | isolated descriptor/codec work is evidence-backed; arbitrary-VID inbox binding is unresolved and no persona is registered |
| Raw Xbox One/Series GIP USB enumeration | MS-GIPUSB 1.0 specifies descriptors, `XGIP10`, lifecycle, metadata, input, motor, Windows-PC security opt-out, and Security Data Complete | authorized USB/IP registration and live Windows enumeration now reach Start and continuous input; the exact post-opt-out completion is implemented and offline verified, while post-completion live stability, consumer input/feedback, latency, and lifecycle teardown remain gates |
| Xbox USB security | MS-GIPUSB documents a Windows-PC metadata opt-out and modern-Windows removal of the USB authentication requirement | modern PC slice is implementable without credentials; older Windows/console paths remain out of scope |
| Four independent virtual Xbox actuators | official XInputHID report 2 contains four ordered magnitudes; WGI and GameInput expose four fields | codec work is evidence-backed; actual binding and four independent application-to-report basis vectors remain gated |
| Switch 2 Pro USB input | descriptor and SDL USB path are corroborated; one `0x0201` device was passively captured | dormant per-container Windows discovery/reservation, read-only composite lease, exact-retirement owner, completion-driven pump, and no-HID profile-runtime device are offline verified; a standalone exact-generation slot table, ControlService attach, calibration acquisition, and live validation remain gated |
| Switch 2 Pro BLE input | custom GATT facts are capture-backed; official SDL path is a stub | dormant single-use admission, concrete read-only WinRT watcher/GATT lease, bounded exact-Common05 input owner, completion-driven drain pump, and no-HID profile-runtime device are offline verified; selected-radio affinity on multi-radio systems, encrypted-link proof, reconnect ownership, registration/attach, and the hardware matrix remain gated |
| Joy-Con 2 L/R BLE | advertisements, UUIDs, and basic reports are partly known | the same concrete read-only WinRT adapter, shared BLE owner/pump, standalone profile projections, generation/skew/loss-fenced joined reducer, and dormant standalone/joined no-HID devices are offline verified; association, encrypted-link proof, registration/attach, motion validation, runtime pair ownership, physical output, and hardware validation remain gated |
| Charging Grip/wired Joy-Con 2 | no complete audited transport | unsupported |
| Switch 2 application association | controller NVM mutation and an AES-based exchange are described | blocked; explicit consent, recovery, and Windows key-provisioning evidence required |
| Switch 2 factory stick calibration | packed format and primary/secondary addresses agree; a single right Joy-Con stores its logical-right record in the primary slot | model-aware metadata implemented; replay/unit verified only |
| Switch 2 user calibration | right-stick address conflicts (`0x1FC060` versus `0x1FC080`) | live use blocked pending fresh before/after dumps |
| Switch 2 HD-rumble envelope | four lossless 10-bit fields, 16-byte group stride, and strict 64-byte USB reports are corroborated | raw subframe/group/USB codec implemented and offline verified; BLE length, band/tone meaning, cadence, stop, thermal and routing need measurement |
| Xbox to DualSense feedback | four semantic lanes can be mapped to body rumble and trigger-capable effects | policy can be designed; physical transfer remains unverified |
| Xbox to ordinary rumble | deterministic saturating downmix is possible | approximate by definition; policy/tests still required |
| Xbox/DualSense four-lane compatibility rumble to Switch 2 HD rumble | no native trigger actuator exists; synthesis is required | allocation-free pure translation is implemented and offline verified against the pinned SDL body-rumble arithmetic plus exhaustive project-authored impulse-policy tests; transport delivery, physical sidedness/energy/cadence/stop/thermal behavior, adaptive-trigger programs, and audio haptics remain unimplemented or hardware-gated |
| Native DualSense audio haptics to Switch 2 | requires bounded DSP and physical transfer measurements | designed only |
| Latency parity with PadForge/HIDMaestro | spans have not been measured with the same hardware/method | unverified; no parity claim |

## Xbox presentation findings

### Facts

- usbip-win2 uses Microsoft's UDE/UdeCx model to present device,
  configuration, interface, endpoint, and URB behavior. The pinned receive and
  completion paths are
  [`wsk_receive.cpp`](https://github.com/vadimgrn/usbip-win2/blob/7c219953101cc5d0ec9a0bcb3eb87259cf72bedd/drivers/ude/wsk_receive.cpp)
  and
  [`device_ioctl.cpp`](https://github.com/vadimgrn/usbip-win2/blob/7c219953101cc5d0ec9a0bcb3eb87259cf72bedd/drivers/ude/device_ioctl.cpp).
- Microsoft's
  [UDE client-driver model](https://learn.microsoft.com/en-us/windows-hardware/drivers/usbcon/writing-a-ude-client-driver)
  and [UdeCx USB-device DDI](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/udecxusbdevice/)
  cover virtual USB descriptors, configurations, interfaces, alternate
  settings, endpoints, lifecycle, and URB completion. They do not state that
  an arbitrary Xbox-shaped USB device automatically becomes XInput-visible.
- Microsoft's current
  [GameInput hardware-interface taxonomy](https://learn.microsoft.com/en-us/xbox/gdk/docs/features/common/input/hardware/input-hardware-interfaces?view=gdk-2604)
  distinguishes XUSB, XInputHID, GIP, and generic HID. On PC it directs an
  XInput-compatible device path toward XInputHID; that is evidence against
  treating a generic HID descriptor, raw GIP, and XUSB as interchangeable.
  The generic [Windows HID architecture](https://learn.microsoft.com/en-us/windows-hardware/drivers/hid/hid-architecture)
  likewise describes HID class/transport layering, not Xbox API promotion.
- UdeCx is a transport backend, not an Xbox mapping or API-visibility layer.
  `UdecxUsbDeviceInitSetEndpointsType` requires at least KMDF 1.15 and selects
  simple or dynamic endpoints; `EVT_UDECX_USB_DEVICE_ENDPOINTS_CONFIGURE`
  handles configuration/alternate-setting endpoint lifecycle. Microsoft's
  [UMDF guidance](https://learn.microsoft.com/en-us/windows-hardware/drivers/wdf/user-mode-driver-framework-frequently-asked-questions)
  recommends starting in UMDF when the required feature exists there, but the
  audited UdeCx DDI is KMDF and the
  [Virtual HID Framework](https://learn.microsoft.com/en-us/windows-hardware/drivers/hid/virtual-hid-framework--vhf-)
  currently supports HID source drivers only in kernel mode. Either future
  native route would therefore be a signed installed-driver project below the
  existing VIIPER semantic seam, not a second mapping stack.
- Windows requires kernel-mode code to be signed. Microsoft describes
  HLK-tested dashboard signing as the recommended production path and
  [attestation signing](https://learn.microsoft.com/en-us/windows-hardware/drivers/dashboard/driver-signing-offerings)
  as testing-only, not Windows certification and not a retail Windows Update
  path. This keeps UdeCx/VHF and any custom XInputHID package outside the
  current no-new-driver USB/IP phase.
- The official [GIP/XInputHID documentation download](https://dlassets-ssl.xboxlive.com/public/content/GIP/GIPDocumentation.zip)
  was independently hashed as recorded above. `XInputHID Documentation.docx`
  paragraphs 5--174 define one standardized gamepad descriptor. Input report
  ID 1 has four unsigned 16-bit stick axes, independent 10-bit triggers, a
  null-capable hat, fifteen button bits, and a Consumer/Record Share bit.
  Output report ID 2 has a four-bit actuator-enable mask; four 0--100
  magnitudes ordered left impulse, right impulse, left body, right body; then
  10-ms duration, start delay, and loop-count bytes. This makes strict isolated
  XInputHID descriptor/input/output codecs evidence-backed without borrowing a
  competitor descriptor.
- The same document's claimed no-custom-INF USB binding is **not yet an
  implementable fact**. Paragraph 194 says to put the 22-character
  `HID_DEVICE_SYSTEM_GAME` value in a Microsoft OS compatible-ID field, while
  Microsoft's [OS 1.0 descriptor specification](https://learn.microsoft.com/en-us/windows-hardware/drivers/usbcon/microsoft-os-1-0-descriptors-specification)
  requires every compatible ID to be exactly eight bytes. Microsoft's
  [HIDClass ID documentation](https://learn.microsoft.com/en-us/windows-hardware/drivers/hid/hidclass-hardware-ids-for-top-level-collections)
  identifies `HID_DEVICE_SYSTEM_GAME` as a HIDClass-generated special hardware
  ID and says HIDClass generates no compatible IDs. No encoding may be guessed.
  Read-only inspection of this host's Windows `10.0.26100.8972` INFs found the
  generic xinputhid match commented out and active matches limited to explicit
  Microsoft VID/PID `_IG_00` IDs. Those vendor IDs are not ours to emulate.
- Paragraphs 195--272 do document a custom signed INF that matches the
  vendor's own PDO, includes stable sections from `input.inf` and
  `xinputhid.inf`, and loads `xinputhid.sys` as a HID upper filter. That is the
  evidenced mechanism, but its sample IDs are internally inconsistent: the
  model line uses `045E:0B1C`, the string key says `0123:4567`, and the prose
  says `045E:02E6`. Microsoft also says its `045E` VID is instructional and
  unauthorized for vendors. On this build the referenced
  `HID_Inst.NT{,.HW,.Services}` and `GIP_Hid.{HW,Services}` sections exist, but
  the sample IDs and property flags cannot be copied blindly. Installing and
  productizing a package for the project's own lawful hardware ID is a
  separate driver/signing scope and is not authorized here. Its exact runtime
  binding, Windows-version support, API visibility, and performance remain a
  Phase 3 vertical-slice unknown.
- The xone wired match is interface zero with class/subclass/protocol
  `FF/47/D0`; data uses 64-byte interrupt IN/OUT buffers. Endpoint addresses,
  intervals, the complete configuration, strings, BOS data, EP0 behavior, and
  audio topology still require a physical capture.
- MS-GIPUSB 1.0 is now the primary raw-USB specification. It defines the OS
  1.0 string `MSFT100`, vendor request code `0x90`, and exact eight-byte
  Extended Compatible ID `XGIP10`. Read-only inspection on this Windows build
  found `dc1-controller.inf` matching `USB\MS_COMP_XGIP10` and including
  `xboxgip.inf`, so a documented inbox binding exists without imitating a
  Microsoft VID/PID. The same specification requires a manufacturer-owned
  valid USB VID and matching VID/PID in the USB descriptor and GIP Hello; a
  production identity cannot be invented.
- The raw GIP data interface is `FF/47/D0` with 64-byte interrupt OUT and IN.
  MS-GIPUSB requires the IN endpoint `bInterval` to be at least 4 ms. Thus the
  fastest specification-conforming raw-GIP USB service interval is nominally
  250 Hz; neither USB/IP nor a future native backend may honestly advertise a
  1-ms raw-GIP endpoint by violating that descriptor requirement.
- The Windows-PC security path is also documented: metadata can include
  opt-out GUID `7a34ce77-7de2-45c6-8ca4-0042c08bd94a`, after which the host
  succeeds the security exchange by default. MS-GIPUSB product behavior says
  USB authentication was removed for Windows 10 v21H2, Windows 11, and later.
  The existing xone cryptographic hardware-client path is therefore not a
  prerequisite for the supported modern-Windows PC virtual-device slice.
- On the validated Windows host, opt-out replaces the 58-byte ACME security
  request with exact `06 20 01 02 01 00`. Microsoft defines this as Security
  Data Complete, and Linux xpad independently emits the same `01 00` body as
  `xboxone_auth_done`. Accepting that exact Active-state completion is not
  challenge-response emulation; all other Security data remains unsupported.
- GIP command identifiers include ACK `0x01`, announce `0x02`, authenticate
  `0x06`, virtual key `0x07`, rumble `0x09`, HID report `0x0B`, and input
  `0x20`. See the pinned
  [framing implementation](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/bus/protocol.c).
- The base input body is fourteen little-endian bytes: buttons, two 16-bit
  triggers, and four signed 16-bit sticks. Guide is a separate virtual-key
  command. Share is capability-dependent.
- The rumble body is nine bytes: fixed Direct Motor command byte `0x00`, enable mask,
  left-trigger, right-trigger, left-body, right-body, duration, delay, and
  repeat. See the pinned
  [gamepad structures](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/driver/gamepad.c).
- The wired reference starts a randomized certificate/key-exchange and
  transcript verification during probe. See
  [`auth.c`](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/auth/auth.c).
- xow is not an Xbox One virtual-output implementation. At
  `d335d6024f8380f52767a7de67727d9b2f867871`,
  [`xow.cpp:44-63`](https://github.com/medusalix/xow/blob/d335d6024f8380f52767a7de67727d9b2f867871/xow.cpp#L44-L63)
  opens physical Microsoft `045E` wireless adapters, while
  [`dongle/dongle.cpp:137-228`](https://github.com/medusalix/xow/blob/d335d6024f8380f52767a7de67727d9b2f867871/dongle/dongle.cpp#L137-L228)
  unwraps receiver 802.11/GIP traffic and
  [`controller/input.cpp:25-113`](https://github.com/medusalix/xow/blob/d335d6024f8380f52767a7de67727d9b2f867871/controller/input.cpp#L25-L113)
  creates Linux `/dev/uinput`. Its compatibility mode only changes the uinput
  name/PID to Xbox 360; it supplies no USB-device descriptor, Windows binding,
  XInputHID, or VIIPER presentation backend.
- xow corroborates the physical four-actuator GIP command layout in
  [`controller/gip.h:106-121`](https://github.com/medusalix/xow/blob/d335d6024f8380f52767a7de67727d9b2f867871/controller/gip.h#L106-L121),
  but its application-facing feedback source is Linux's two-magnitude
  `FF_RUMBLE`. It derives trigger levels heuristically from effect direction
  and inserts a forced 10 ms firmware-workaround delay in
  [`controller/controller.cpp:242-346`](https://github.com/medusalix/xow/blob/d335d6024f8380f52767a7de67727d9b2f867871/controller/controller.cpp#L242-L346).
  It therefore proves neither virtual impulse-trigger delivery nor a suitable
  low-latency scheduler; MS-GIPUSB and XInputHID remain the primary sources.
- Public
  [`XINPUT_VIBRATION`](https://learn.microsoft.com/en-us/windows/win32/api/xinput/ns-xinput-xinput_vibration)
  contains only two 16-bit body-motor values. WGI
  [`GamepadVibration`](https://learn.microsoft.com/en-us/uwp/api/windows.gaming.input.gamepadvibration?view=winrt-26100)
  and GameInput
  [`GameInputRumbleParams`](https://learn.microsoft.com/en-us/gaming/gdk/docs/reference/input/gameinput/structs/gameinputrumbleparams)
  expose four named fields. WGI documents that a driver can combine values
  when the physical device has fewer motors. Therefore the API value shape is
  fact, while delivery of four independent fields through a proposed virtual
  persona remains a capture/compatibility unknown.
- Linux xpad at `cf72cbb...`,
  [`drivers/input/joystick/xpad.c`](https://github.com/torvalds/linux/blob/cf72cbb39da84b6f02f90c07f33b102fc10b16f0/drivers/input/joystick/xpad.c#L595-L628),
  independently corroborates GIP command constants. Its input parser preserves
  opposing D-pad bits as independent wire states. Its public force-feedback
  path sends a four-bit motor-enable mask but maps Linux's two ordinary FF
  magnitudes only into body motors, leaving trigger values zero; it therefore
  does not prove a public four-actuator application path.

### Binding verdict

| Route | Classification | Consequence for DS4Windows -> VIIPER -> USB/IP |
|---|---|---|
| Raw GIP with exact eight-byte `XGIP10` | **Fact** in MS-GIPUSB; corroborated on this build by `dc1-controller.inf:40-63` and `xboxgip.inf:26-56` | strongest inbox/no-new-package candidate; requires lawful VID/PID, exact Hello/metadata/ACK/lifecycle/input/output, a 4 ms-or-slower IN interval, and a disposable vertical slice |
| XInputHID with a custom INF matching the project's own PDO hardware ID | **Fact as an installation mechanism; runtime result unknown** | official sample's Include/Needs shape is documented and current inbox sections exist, but the inconsistent sample IDs/flags, signing, supported-version behavior, API visibility, and latency require a separately authorized signed-package test |
| XInputHID no-custom-INF Microsoft OS CompatibleID `HID_DEVICE_SYSTEM_GAME` | **Unknown and contradicted as written** | the field is eight bytes, the named value is a special HID hardware ID, and current inbox evidence does not supply the claimed generic match; do not truncate, encode, or otherwise guess this route |

### Competitor comparison

PadForge does not use HIDMaestro's USB/IP backend for Xbox. Its Xbox
One/Series path creates a UMDF HID surface and relies on inbox `xinputhid`; its
Xbox 360/non-xinputhid path also uses an XUSB-facing companion. Input is copied
through shared memory and signaled with an event. This is a different
presentation architecture, so its HID descriptor is not a captured physical
Xbox USB persona.

HIDMaestro's approximately tens-of-microseconds benchmark starts at its state
submission API and ends at a busy-polled consumer in the same high-priority
process. The published harness serializes 10,000 button toggles, waits for each
toggle to be observed before submitting the next, and therefore cannot expose
overwritten transitions. It excludes physical I/O, parsing, mapping, normal
consumer cadence, and the UMDF HID event path.

PadForge stable and development use the same SDL binary. Stable has a nominal
one-millisecond application loop; development retains that default and can
retune the one loop per profile. Each ordinary iteration performs SDL update,
current-state snapshot and mapping/combine/macros, then calls the virtual-device
stage. Changed state is submitted on that loop, but an unchanged report may be
deduplicated for up to 16 ms rather than published every iteration. Enumeration
shares the thread but is gated to startup and approximately every two seconds,
not every frame. The watchdog logs SDL/enumeration work only at 25 ms and full
cycles only at 50 ms, so smaller tails are not reported. Stable to development
adds raw extended-report handling and HIDMaestro 1.7.1, not a new latency
architecture.

The refreshed `v4-dev` branch now ends at `b7f58cf8...`. Commit
[`49377fb4`](https://github.com/hifihedgehog/PadForge/commit/49377fb4)
is development-only and fixes a concrete false-trigger-feedback bug:
caller-padded XUSB buffers of at least seven bytes let the canonical flags byte
at offset four be misread as left-trigger amplitude; the canonical five-byte
`SET_STATE` packet itself does not enter that trigger branch. A virtual-slot
connect must also explicitly release prior DualSense trigger programs. This is
a valuable clean-room negative test for our typed decoder and ownership
release. The same change adds unconditional,
payload-sensitive `FFBANY`/`FFBXIN` callback logging. PadForge's diagnostic
sink locks and enqueues and may synchronously append to disk when diagnostics
are active, so the development branch has a plausible feedback-induced jitter
mechanism that must be disabled when collecting parity data. This behavior is
not present in `v4.3.2` and no PadForge code is copied. Fetch also found a
local/remote `latest-v4-dev` tag conflict; the audited identity is the fetched
branch commit above, not either unverified tag object.

Two later development-only commits, `88686582` and `b7f58cf8`, add a
per-profile selector for the application's single global loop and disclose in
the settings UI when a profile overrides the global interval. The available
values include 1, 2, 4, 8 and 16 ms; the active profile retunes that same loop
at its normal profile-switch choke point. This is useful evidence that
PadForge's named 500 Hz option is a two-millisecond host-loop target, not a
measurement of Switch 2 USB/BLE arrival cadence or game-visible delivery.
These commits change no shipped SDL transport binary, HIDMaestro handoff, or
physical-rate capture, and remain clean-room behavioral evidence only.

HIDMaestro `1.7.0` and current `1.7.1` have byte-identical driver, companion,
shared-memory IPC, Microsoft/Xbox profile, and latency-harness files. The common
`HMController` changed for Valve/Triton always-armed raw routing, not the Xbox
semantic/IPC path. Its companion capability surface advertises two body motors,
then republishes raw `SET_STATE` bytes; the test merely prints candidate bytes
and does not implement a typed four-actuator decoder. Input IPC is one latest
362-byte seqlock snapshot plus two separate coalescing auto-reset doorbells,
one for the HID driver and one for the XUSB companion, while output is a 64-slot
ring whose reader skips overwritten history. The reusable ideas are
publish-before-doorbell ordering and shared reservation/drain tests, not code:
VIIPER's official `RumbleBodyV1`, producer leases, bounded ordered journal,
final admission, mandatory neutral and explicit resynchronization already
provide the stronger contract. HIDMaestro's current source is MIT, but its
descriptor/profile provenance, undocumented inbox bindings and global IPC ACL
choices remain separate review items; no donor import is justified.

PadForge reads the latest SDL state after SDL has drained available HID
packets. HIDMaestro then writes a single latest-state shared-memory section and
signals the separate HID-driver and XUSB-companion auto-reset events. Multiple
physical reports can collapse in the SDL snapshot, and multiple submissions
before either consumer reads can overwrite one another. This can favor
newest-state freshness during a stall but can lose a short edge. Our bounded
transition journal makes the opposite tradeoff and can replay stale edges after
a stall. Comparison must therefore measure both newest-state age and endpoint
edge delivery, not declare either architecture a categorical winner.

Neither competitor span is physical-controller-to-game latency, and none of
this proves a 50 ms USB/IP floor. HIDMaestro's Xbox 360 path reads its seqlock
state directly through a companion, whereas our Xbox 360 travels through an
emulated full-speed USB endpoint whose descriptor permits one-millisecond
service. That is a real architectural difference for backend-entry-to-consumer
latency, but not evidence of a 50 ms transport minimum.

The competitor's four-actuator evidence is unresolved. HIDMaestro current at
`9df50410230c11b410f43909ede0e5fc8b23d15b` includes
`docs/investigations/wgi-silent-sink-2026-04/finding.md` (SHA-256
`DABC00BA3D424D4758E7A5B3163EFF19BB884761370D23193BDC7A920F3E1D18`).
That document reports that, on Windows 11 `26200.8115`, direct XInput motor
bytes reached its experimental ROOT-enumerated UMDF2 XUSB virtual while WGI
and Chromium sent probes/idle clears but no motor-bearing bytes at three raw
driver instrumentation layers. Its physical positive controls were tactile,
not byte-captured. The document infers that WGI dispatch may require a
USB-bus/xusb22 parent; that is not an official or independently reproduced
fact. It also contradicts itself about GameInput: the post-fix evidence matrix
says `SetRumbleState` reached the virtual, while its later open question and
consumer-impact section say GameInput was blocked. GameInput therefore remains
unknown rather than either passing or failing in this audit. This evidence
supports a real USB/IP API-visibility/feedback vertical slice and prevents a
ROOT-enumerated HIDMaestro shape from being treated as a compatibility oracle;
it does not prove that VIIPER USB/IP will bind or receive four channels. No
redistributable golden capture establishes HIDMaestro's private four-channel
packet assumptions.

PadForge's feedback decoder also depends on transport/profile-specific
assumptions: an extended XInput body adds trigger values beyond the documented
public two-motor API, while permissive HID length branches interpret motor
bytes at different offsets. Its physical-Xbox writer pairs the Nth synthetic
XInput slot with the Nth filtered XUSB HID interface and writes synchronously.
Same-model duplicates can be mispaired, and the bundled OpenXInput binary has
no normal open-source grant. These paths are behavioral evidence, not donors
or acceptable production dependencies.

PadForge does contain useful feedback-lifecycle defenses: it tears down
physical-feedback dispatchers before the virtual controller, rebuilds them on
slot migration, suppresses an outgoing release during an explicit successor
handoff, and parks the prior feedback slot before late callbacks can republish.
Those are clean-room behavioral precedents for callback fencing and successor
handoff, not a transport-wide generation/lease contract; the local exact owner,
generation, lease and mandatory-neutral contract remains stronger.

### Inferences and unknowns

- It is reasonable, but unproven, that a genuine wired Xbox transported
  through the exact USB/IP stack will bind like direct hardware.
- Modern Windows USB authentication is no longer an evidence blocker when the
  specified Windows-PC opt-out metadata is used. Older Windows releases and
  non-PC/console security remain out of scope; no credentials, transcripts, or
  console compatibility are needed or permitted.
- It is unknown which documented Windows surface will deliver four distinct
  channels to the eventual persona across representative games.
- It is unknown whether an arbitrary, lawfully assigned VIIPER USB VID/PID can
  bind to inbox `xinputhid.sys` without a custom package on supported Windows
  releases. The official descriptor/report contract is usable; the published
  no-INF compatible-ID sentence is internally contradicted and must not be
  encoded by truncation or another guess.
- A 50--124 ms tail is not attributable to USB/IP without QPC/ETW stage data.

MS-GIPUSB makes the raw-GIP USB/IP vertical slice the strongest documented
no-new-driver candidate. Offline descriptor, framing, metadata, lifecycle,
input, ACK and motor work can proceed now. Registration and a disposable live
slice require a lawfully assigned project VID/PID; they must record the actual
driver stack, XInput/GameInput/WGI/SDL/Raw Input visibility, four independent
output basis vectors, and USB/IP completion spans. A real wired Xbox positive
control should then compare direct attachment with Linux export and exact
0.9.7.7 Windows import. The procedure is in
`xbox-one-usbip-feasibility.md`. XInputHID remains a useful four-actuator
cross-check and potential lower-interval alternative, but its arbitrary-VID
no-INF route is contradicted; its documented custom signed-INF fallback is a
separately authorized scope. Neither result authorizes imitating Microsoft's
VID/PID or starting native-driver work.

## Switch 2 findings

### Local hardware observation

One user-supplied Switch 2 Pro was observed without output, feature, WinUSB,
memory, serial, association, firmware, or rumble writes. Windows reported
`057E:2069`, device revision `0x0201`, HID interface zero through `HidUsb`,
vendor interface one through `WinUSB`, and audio interface two through
`usbaudio`. HID reports are 64-byte input/output values with gamepad usage
page `0x01`, usage `0x05`.

The standalone passive probe captured one 2,048-frame run and one 4,096-frame
run of exact report-`0x05` frames in order. The second run had a mean host
completion delta of 3.990 ms, p50 4.001 ms, and p99 4.112 ms; every raw report
counter delta was `+4`. This observes approximately 250 reports/second in the
passive state. It neither defines the counter unit nor disproves a different
documented initialization mode. The raw captures and their integrity metadata remain outside git in
operator-controlled evidence storage; this document intentionally contains no
stable path, digest, device-path derivative, or other value that can link the
sanitized fixture back to that raw session. The capture metadata does not query
a serial. The probe's QPC values establish replay order only; managed
serialization and queued HID reads make them unsuitable as latency evidence.

After the schema-4 verifier passed 59 offline tests, an independent adversarial
safety review, and a warnings-as-errors Release build, one user-authorized run
re-established the exact `057E:2069` / `bcdDevice 0x0201` target but failed the
required exclusive MI_00 writer lease with `HidReadOpenFailed` / Win32 error
32. It therefore stopped before the command channel, battery request, Player
LED mutation, input capture, haptic path, or cleanup mutation. No process was
stopped and the verifier did not attribute the incompatible handle to a
process. The result SHA-256 is
`0DFC1F5240FF3BBBFD9DD8CC0F8689039981EEEA3EF9856E9E626A099E7F8F44`;
the bound verifier DLL SHA-256 is
`87D2A053DD6FF7A447D71E27B13D2715B188547E8A33E3816EF95164184F4A4E`.
This establishes only that the sole-writer gate failed closed; it adds no
controller-mechanism or rate evidence.

### Facts

- Switch 2 Pro is `057E:2069`. Captures show five USB interfaces, a 64-byte
  HID interrupt interface, and a vendor bulk command interface. Official SDL
  claims the command interface while using the normal HID path for input.
  Its Bluetooth initializer explicitly declines the device. See
  [`SDL_hidapi_switch2.c`](https://github.com/libsdl-org/SDL/blob/c71abd08605b8bb7078372307a93274725c99fe0/src/joystick/hidapi/SDL_hidapi_switch2.c).
- Official SDL and Switch2Connect both send vendor commands on interface one's
  bulk OUT endpoint. SDL reads one bulk reply after each initialization write
  but does not validate its length, tuple, or status; Switch2Connect treats
  write completion as command success. Production adoption instead requires a
  serialized transaction, bounded timeout/retry, and an exact match on command,
  type, interface, subcommand, declared length, status, and transport
  generation. Switch2Connect's HID fallback prepends report ID `0x02` and emits
  only 43 bytes even though the HID descriptor declares a 64-byte report; no
  pinned capture corroborates that route, so it is unsupported rather than a
  fallback.
- BLE advertisements use company `0x0553`, embed Nintendo vendor `0x057E`,
  and distinguish right Joy-Con `0x2066`, left Joy-Con `0x2067`, and Pro
  `0x2069`. A remembered host address is sensitive identity data and must be
  redacted.
- Switch 2 controllers use custom GATT rather than HOGP in the audited
  captures. Roles must be selected by UUID and properties, never by handle or
  sorted ordinal; later Pro firmware adds characteristics.
- Microsoft's official
  [GATT client documentation](https://learn.microsoft.com/en-us/windows/apps/develop/devices-sensors/gatt-client)
  exposes services, characteristics, descriptors, reads/writes, and
  notify/indicate callbacks rather than raw Bluetooth transport. It explicitly
  requires prior knowledge of the device's standard or vendor profile. The
  documented `GattCharacteristicProperties` flags can validate
  read/write-without-response/notify/indicate roles, but these generic Windows
  APIs establish no Switch 2 UUID, association exchange, encryption rule,
  report layout, HD-rumble envelope, connection interval, or 500 Hz rate. Those
  remain pinned-source or project-capture facts and must be measured through a
  serialized, generation-bound physical adapter feeding the shared canonical
  state/feedback paths.
- Common input report `0x05` is a 63-byte body. USB prepends report ID `0x05`;
  BLE selects the report by characteristic UUID. Counter is at `0x00`, buttons
  `0x04`, sticks `0x0A`/`0x0D`, mouse `0x10`, magnetometer `0x19`, battery
  `0x1F`, and motion `0x2A`. Short packets must be rejected, not padded.
- Model reports `0x07`, `0x08`, and `0x09` have known basic controls but their
  packed motion block is not yet specified. A safe decoder retains that block
  as opaque.
- Nine calibration bytes contain center, positive-range, and negative-range
  packed 12-bit pairs in licensed SDL's ordering. Factory primary is
  `0x130A8`, secondary is `0x130E8`: Pro uses primary/left and
  secondary/right, while either single Joy-Con uses its primary slot and maps
  that record to its logical side. Switch2Connect's current range ordering
  conflicts with licensed SDL and has no asymmetric hardware golden. The
  user-right slot also remains disputed.
- BlueRetro independently agrees with SDL's center/positive/negative stick
  order. Switch2Connect reverses the positive and negative range slots; that
  is a concrete source disagreement which symmetric calibration can hide, not
  a naming difference. Factory Pro left/right records are `0x130A8` and
  `0x130E8`. Live user-calibration adoption remains fail-closed while the
  project sources still disagree about the right user slot/validation flow.
- A raw subframe is four contiguous 10-bit fields in five bytes. Licensed SDL
  calls them high-frequency/high-amplitude then low-frequency/low-amplitude;
  other audited sources split each control field as nine frequency bits plus a
  tone bit and reverse the band labels. The implemented codec therefore uses
  semantic-neutral `Control0/Amp0/Control1/Amp1` names and preserves all bits.
- The inner output envelope is a 16-byte group: `0x50 | counter` plus three
  five-byte subframes. USB is exactly 64 bytes. Pro report `0x02` carries
  independent groups at offsets 1 and 17 under the same counter and requires a
  zero tail from 33; the capture corpus contained 1,587 valid reports with no
  group-counter mismatch. Licensed SDL corroborates the one-frame
  compatibility writer and a nominal 12 ms active resend policy. It explicitly
  reports no native trigger-rumble actuator. BLE envelope lengths still
  conflict and remain disabled.

Primary fact sources are the pinned SDL file above and the capture-backed
research documents
[`bluetooth_interface.md`](https://github.com/ndeadly/switch2_controller_research/blob/d1c5a7f7ba298f83017fae84952a4e6d2ef8fc92/bluetooth_interface.md),
[`hid_reports.md`](https://github.com/ndeadly/switch2_controller_research/blob/d1c5a7f7ba298f83017fae84952a4e6d2ef8fc92/hid_reports.md), and
[`memory_layout.md`](https://github.com/ndeadly/switch2_controller_research/blob/d1c5a7f7ba298f83017fae84952a4e6d2ef8fc92/memory_layout.md).

The “up to 500 Hz” statement is a claim, not a measured fact. Its first exact
Switch2Connect occurrence is README-only commit
`92a0ec5b16932f307e732cf617eac01c85cf546c` (2026-07-14); the repository has no
committed packet capture, timestamp trace, cadence test, or benchmark proving
it. PadForge also commits no 500-Hz Switch 2 trace. Stable uses a nominal
one-millisecond application loop; current development additionally exposes
1/2/4/8/16 ms per-profile loop targets. Both are host scheduling controls,
not physical arrival cadence. An independently parsed official-console capture at pinned
research revision `d1c5a7f...` contained 50,899 report-`0x09` packets after
feature mask `0x27`: mean 3.999935 ms, p50 3.999966 ms, p99 4.000134 ms, with
no report-`0x05` packets. Together with this project's passive report-`0x05`
observation, the established modes are approximately 250 Hz. Whether a
separately verified initialization selects a genuine 500-Hz mode remains
unknown and is reserved for the later bounded rate experiment.

HIDMaestro does not close that evidence gap. Its production path is described
and implemented as event-driven with no fixed cap; `500` appears in the test
CLI as the point at which the harness requests one-millisecond Windows timer
resolution. Its published 32--38 microsecond medians are one-at-a-time,
same-process, busy-polled XInput transitions on one host and do not establish a
500 Hz physical controller, ordered presentation, or end-to-end cadence.

### PadForge and Switch2Connect transport behavior

PadForge stable and development ship the same SDL3.dll built from the pinned
`d98c580...` fork. Its USB driver drains all currently queued HID packets, then
PadForge snapshots the latest SDL state once per application iteration. Its
custom BLE callback writes one 64-byte latest-report slot and a pending flag;
another notification can overwrite that slot before the SDL update consumes
it. Neither layer journals packet counters or short transitions. The fork
requests `ThroughputOptimized` connection parameters but releases the request
object, and no committed trace records the resulting PHY/interval/cadence. See
PadForge's pinned
[`InputManager.cs`](https://github.com/hifihedgehog/PadForge/blob/0794fd01bd19f4c096b982ffc824b88bce5ed743/PadForge.App/Common/Input/InputManager.cs)
and the fork's
[`SDL_ble_switch2joystick.c`](https://github.com/hifihedgehog/SDL/blob/d98c5804a9d20b0d96e993741797878c86b8f1e1/src/joystick/windows/SDL_ble_switch2joystick.c).

Switch2Connect's wired reader translates each successful HID read immediately,
but virtual publication is still one newest pending frame. A blocking native
submit causes the previous pending frame to be replaced; a sticky OR retains
button presses but can synthesize/delay edges and collapse releases, analog,
and motion. Its Full power mode also suppresses an otherwise identical
buttons/sticks/triggers signature for up to 50 ms. BLE parsing, calibration,
fusion, mapping, mouse logic, and publication execute synchronously in one
large notification callback, with no counter loss/reorder contract. See the
pinned
[`usb_hid_controller.py`](https://github.com/TommyWabg/Switch2Connect/blob/4487322a306f04efa27682e3f3a508635a84fd98/src/usb_hid_controller.py),
[`virtual_controller.py`](https://github.com/TommyWabg/Switch2Connect/blob/4487322a306f04efa27682e3f3a508635a84fd98/src/virtual_controller.py),
and
[`controller.py`](https://github.com/TommyWabg/Switch2Connect/blob/4487322a306f04efa27682e3f3a508635a84fd98/src/controller.py).

Switch2Connect's active wired-Pro output worker is newest-only with a 15 ms
minimum interval and no replay. Ordinary BLE evaluates output at 7 ms with one
write in flight; paired Joy-Con scheduling alternates sides with at least 12 ms
per side and adaptive lower target rates. These are software targets, not
physical-write or actuator-onset evidence. The bounded newest-only/single-flight
shape is useful, but its exact cadences and packed-byte limiter are not.

Switch2Connect has a narrow Xbox-feedback generation fence for delayed
four-motor callbacks. It does not bind all feedback sources, command responses,
or transport lifetimes, but it is evidence of one callback-lifecycle defense
and must not be described as having no generation handling at all.

BlueRetro `e1a9831...` is an archived Apache-2.0 corroborating source, not a
complete implementation template. Its pinned
[`sw2.c`](https://github.com/darthcloud/BlueRetro/blob/e1a9831a875f5313a923160a1379a7ebbfaa2b11/main/bluetooth/hidp/sw2.c)
reads a controller association key, compares it with host state, conditionally
performs a multi-step application association, then reads/stores the successor
key. That is stronger lifecycle evidence than Switch2Connect's unconditional
fixed-blob `pair()`, but its parser does not length-check all casts and its ATT
handles are fixed. Pairing/key/address flows are persistent and security
sensitive: no code, constants, credentials, or commands are copied or run.
BlueRetro's Joy-Con branches remain mostly placeholders, its Pro output length
truncates a later LED field, and it commits no Switch 2 cadence benchmark.

JoyShockLibrary `023dfb6...` is a negative control. Its current identifiers and
parser cover classic Nintendo `0x2006`, `0x2007`, `0x2009`, and `0x200E`, not
Switch 2 `0x2066`, `0x2067`, or `0x2069`; its README explicitly says Nintendo
HD rumble is unsupported. It contributes no Switch 2 cadence, association,
calibration, or HD-rumble fact.

### Rejected shortcuts

- Switch2Connect chooses characteristics by sorted position and matches
  responses mainly by command ID. Its command path has one mutable response
  future and expected ID with no enforced transaction lock, so concurrent calls
  can replace each other and a delayed same-command response can satisfy the
  wrong transaction. Firmware changes or delayed responses can therefore
  misroute both characteristics and operations.
- Switch2Connect accepts truncated USB/BLE report families and sometimes pads
  them. Our exact transport/model/UUID/property/length gates are retained.
- Its `pair()` sequence does not implement the described contribution,
  challenge, verification, or Windows key installation flow and picks the
  first local Bluetooth address on multi-radio systems.
- The hifi SDL experiment reads magnetometer data from offsets that overlap
  other fields, uses a one-latest-report slot which can lose short edges, and
  releases the Windows preferred-connection request without proving the
  negotiated interval. Its callback-safety strategy also retains controller
  state for process lifetime, so reconnect soak needs explicit ownership tests.
- Switch2Connect's active wired-Pro rumble worker is a latest-only 15 ms writer,
  not its unbound experimental 40 Hz routine. Its amplitude limiter edits
  bytes inside an already packed 40-bit waveform as if they were independent
  amplitudes, which can corrupt adjacent frequency/shape fields. No such logic
  may be copied; decode/clamp/re-encode and golden basis tests are required.
- Neither PadForge's exact SDL fork nor Switch2Connect commits a timestamped
  trace tying command `0x27`, report selection `0x05`, firmware, USB topology,
  and a 500 Hz result. Source comments and README claims are hypotheses until
  the later active test runs.
- A successful Windows preferred-parameters request is not the negotiated
  interval. The request is Windows 11 build 22000+ and the actual result must
  be measured; throughput mode can reduce simultaneous-device capacity.
- No audited evidence justifies calling hard-coded key material safe pairing,
  forwarding arbitrary raw commands, padding short reports, or promising a
  5 ms Windows BLE interval.

### Unknowns and gates

Live association, Windows key provisioning, NVM eviction/recovery, exact
initialization effects and sustainable rate, sensor scale and timestamp units, user-right
calibration location, model-report motion, accepted rumble lengths, subframe
timing, band order, side routing, cadence, stop latency, packet-loss handling,
thermal limits, and input/output contention all require new project-owned
captures and hardware measurements.

## Latency conclusion and measurement contract

No current data establishes physical-to-consumer latency for the untouched
exact-0.9.7.7 baseline, so neither parity nor regression can be claimed.
Measurements must keep these spans separate:

1. backend API entry to consumer observation;
2. physical report completion to consumer observation;
3. VIIPER publication to USB/IP terminal completion;
4. game output to internal feedback publication;
5. feedback publication to physical transport completion;
6. transport completion to measured actuator onset and stop.

Record p50 through p99.9, maximum, raw samples, queue age/high-water,
coalescing, drops, duplicates, reorder, CPU, allocation/GC, and scheduler
lateness. Compare direct, physical-over-USB/IP, synthetic, and competitor arms
on the same hardware and observer with predeclared sample counts and margins.

## Selected safe implementation boundary

Proceed now with strict semantic values, allocation-free codecs—including the
official XInputHID reports—age-aware bounded ordered replay, capture
conversion/redaction, feedback ownership, and probes.

The Xbox 360 migration now carries an optional explicit
`maximumOrderedAgeMilliseconds` device-creation value from DS4Windows
environment setting `DS4W_VIIPER_X360_MAX_ORDERED_AGE_MS` into VIIPER. Each
process converts the duration into its own monotonic clock; no absolute
timestamp crosses the socket. Unset retains the bounded journal with
fail-closed overflow and no invented age deadline. A positive shipping value
remains a measurement decision, not a hidden code default.
Do not register a guessed Xbox persona, perform Nintendo association, write
controller memory, start live Switch 2 output, copy credentials, or port the
native driver.

The next external evidence required is:

1. a lawfully assigned project USB VID/PID for any registered GIP persona, plus
   a real wired Xbox controller and Linux exporter for the direct/imported
   positive control and sanitized descriptors/transcripts;
2. the already authorized, bounded Switch 2 Pro USB rate/LED/rumble basis plan,
   but only after the source audit, implementation, offline gates, independent
   command review, automatic neutral/restore path, and secret-redaction review;
3. separate Switch 2 Pro BLE and Joy-Con 2 L/R hardware/capture access; and
4. explicit authorization before any association/NVM mutation, system driver
   installation, or native-backend phase. The output authorization does not
   authorize those operations.

### Privacy and publication gate

Current HEAD fixtures are being made fully synthetic, and stable documentation
does not retain a device-path derivative or raw-session digest. That is not
sufficient for publication: unpushed DS4Windows commit `942b3e6` and unpushed
VIIPER commit `3af2ce8` still contain linkable raw-capture material in history.
No branch containing those commits may be pushed. Before any later push, make a
recoverable backup, rewrite only the unpushed ranges, and require
`git log --all -S` privacy searches plus an independent review to return clean
for the exact outbound commit set. Do not attempt that history operation while
the VIIPER worktree contains unresolved scheduler/lifecycle work.

## Live checklist

| Item | Implemented | Hardware verified | Replay/simulation verified | Blocked |
|---|---:|---:|---:|---:|
| baseline and artifact pins | yes | installed state observed | yes | no |
| bounded Xbox 360 scheduler | DS4Windows projection/journal/writer admission and VIIPER lease-bound producer/source/final USB admission are wired without changing the 20-byte contract; the same optional duration reaches both local clock domains; compat/raw handoff now retires the prior owner epoch | no new hardware run | focused and full C#/Go suites, configured-CGO race, vet, projection, serialization, overflow-neutral-resync, owner-handoff, allocation, API and USB-boundary tests | default remains bounded compatibility mode until a workload-derived positive ordered-age value is predeclared and measured; live USB/IP consumer/end-to-end gates remain |
| bounded virtual Switch 2 scheduler | DS4Windows semantic projection/journal/final writer admission and VIIPER lease-bound Source are wired; committed report-ID `0`/`0x05`/`0x09` snapshots, exact HID-interface ownership, unsupported-ID stalls, config/overflow fences and compat/raw epoch handoff are implemented | no new hardware run; this is not physical Switch 2 input | focused and full C#/Go suites, configured-CGO race, vet, wire-level RET_SUBMIT controls, committed-snapshot, transition, overflow-neutral-resync, owner-handoff and allocation tests | live USB/IP consumer observation, workload-derived age policy and latency remain; physical USB/BLE adapters are separate |
| bounded Switch 2 Pro USB mechanism verifier | exact target/topology, dual sole-writer leases, fixed battery/LED/rate procedure, conservative cleanup ownership, closed-schema result, and closed live-haptic gate are implemented | exact target rediscovered; run failed closed at exclusive MI_00 acquisition before all commands, input capture, LED, or haptics | 59 focused offline tests and warning-as-error Release build; independent adversarial approval | controller writer must be closed for a retry; haptics remain deliberately blocked pending a documented watchdog or independent physical stop evidence |
| canonical four-actuator feedback value | yes in both repositories, unwired; token-bound delivery claim/complete/stop foundation added | no | focused mirrored unit/concurrency/allocation tests | runtime publication/arbitration/writer wiring and physical capability translation |
| official XInputHID descriptor/codecs | isolated official-descriptor and report-codec implementation exists but is unregistered | no | source structure plus codec/descriptor tests | arbitrary-VID inbox binding contradiction and vertical-slice gate |
| Xbox GIP protocol foundation | strict headers, input/direct-motor bodies, explicit descriptors, profile-bound Hello, exact Protocol Control ACK and no-events Extended Status, opaque externally compiled metadata framing, fail-closed auth seam, transactional final metadata admission/retry/progress ACK semantics, action-level lifecycle execution leases, reset fences, transactional sequence allocation, and a package-local late-selection EP0/IN/OUT coordinator | no | official-byte golden/exhaustive/property/fuzz/lifecycle/transaction tests; coordinator stress and concurrent interleaving tests; full Go suite, configured-CGO race, and vet | semantic compiled metadata, event/auth/security policy, unified server transaction/job binding, runtime integration, and conformance; timeout/cadence policy is implemented offline, not transport-validated |
| Xbox USB persona/session | deliberately unregistered and unwired; no default identity exists; coordinator tickets are copied and dormant rather than server-owned URBs; generic retained jobs now compose offline with an exact Reserve/build/Bind/activate/close/quarantine authority and one dormant Xbox owner, still without a call site | no | descriptor/Hello/status/session construction plus final START-input, failed-OUT, reset, forged-lease, three-lane concurrency, real parked scheduler composition, abort/build and activation/close race simulations | lawful VID/PID and boot-unique identity, exact strings, Windows binding/API visibility, semantic metadata/auth/event status, an opt-in production import binding, suspend/reset wiring, and vertical-slice evidence |
| Switch 2 pure report/calibration codecs | strict input/replay, model-aware factory metadata, and strict raw USB HD-rumble envelope | two passive USB streams validate only Pro `0x05` input framing; calibration and HD output remain unverified | yes | live transport and semantic output tuning |
| Switch 2 canonical physical-input session | exact USB/BLE protocol identity, owned 63-byte raw/canonical frame, 12-bit preservation, device/transport generation and QPC fences, device-generation-bound calibration fallback/adoption, profile projections, and pure Joy-Con pair epoch/skew/loss/split reducer are implemented; Pro USB and BLE owners reuse the same core | no live adapter run | focused Switch 2 suites cover reset, calibration drift, raw ownership, replay, pair tombstones, per-container reservation, BLE single-use admission, exact completion retirement, asynchronous pump/cancel races, teardown ambiguity, and zero-allocation steady state; full DS4Windows suite | production runtime manager, atomic legacy HID handoff, selected-radio affinity, encrypted-link proof, association/reconnect ownership, memory calibration acquisition, UI/removal binding, and runtime pair coordination remain absent |
| Switch 2 live USB/BLE transport | dormant read-only Pro USB Windows adapter/pump and concrete read-only WinRT BLE watcher/GATT lease, notification owner, and completion-driven pump exist; neither transport is registered | no | injected Windows topology/handle/completion simulations plus WinRT watcher/GATT notification/lifecycle/privacy simulations | production runtime/legacy handoff, selected-radio affinity, encrypted-link proof, hardware/capture, reconnect/suspend, and latency gates |
| Switch 2 association | no | no | no | security/recovery/authority gate |
| HD-rumble raw codec | subframe, group and strict USB envelopes | no | golden/property/exhaustive/allocation tests | semantic mapping and BLE envelope |
| HD-rumble translation/tuning | allocation-free pure compatibility translator only; no transport routing | no | pinned SDL body-rumble arithmetic plus exhaustive project-authored impulse-policy tests | physical sidedness/energy/cadence/stop/thermal behavior, adaptive-trigger programs, audio haptics, and authorized measurement gate |
| latency parity | no | no | no | same-method baseline/competitor gate |

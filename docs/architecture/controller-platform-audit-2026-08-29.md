# Controller platform audit, 2026-08-29

Status: source audit complete enough to select the safe pre-hardware work. Xbox
One/Series presentation and physical Switch 2 support are not production-ready.
This document keeps direct observations, inferences, and unknowns separate.

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
| PadForge development | `474d890db12c71d3a825ffd16c8d9dc1223aaff2` | development behavior kept separate from stable |
| HIDMaestro stable payload | `46054b862830fcec7bc98d72ccb7c4f0c0179fb1` (`1.7.0`) | PadForge stable virtual backend |
| HIDMaestro current | `9df50410230c11b410f43909ede0e5fc8b23d15b` (`1.7.1`) | current competitor backend |
| SDL | `c71abd08605b8bb7078372307a93274725c99fe0`; stable `147a8ee32dbf9ac02f3794964490687b6bbda1bc` | permissive Switch 2 USB reference |
| hifihedgehog SDL BLE branch | `d98c5804a9d20b0d96e993741797878c86b8f1e1` | explicitly hardware-gated experiment |
| xone | `3484f603484782dd7551c64e5a33fc602b127051` | wired/dongle GIP protocol facts |
| xpadneo | `3acca9f5e211edb601000bb64767b78b2468f787` | Xbox Bluetooth protocol corroboration only |
| switch2 controller research | `d1c5a7f7ba298f83017fae84952a4e6d2ef8fc92` | capture-backed Switch 2 facts; no code reuse |
| switch2-controllers | `e79720b6a3042f710b3a1c7470dc9575c42e560a` | corroborating facts; no code reuse |
| joycon2py | `9d80b764682a171580fd134b96f00866df6ae398` | MIT prototype evidence |
| joycon2cpp | `1db6999a17a36e24e9d5d09f9fafb4fb0adce6f1` | MIT WinRT wiring evidence |

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

The input scheduler now retains ordered transitions and replaces only
continuous state. A successful claim commit means the backend accepted and
copied the complete report; it does not mean an application observed it.

The mirrored `CFBK` v1 feedback value contains body-low, body-high,
left-trigger, and right-trigger amplitudes plus capability, lifecycle,
ownership, timestamp, TTL, neutral, and stop semantics. It is not wired to a
runtime transport yet.

## Capability and gap matrix

| Capability | Direct observation | Status and gate |
|---|---|---|
| Existing Xbox 360 virtual input | VIIPER has a real USB persona and now uses the bounded Source scheduler | implemented; replay/unit verified |
| Existing Switch 2 Pro virtual output | VIIPER has a 64-byte USB persona and 24-byte feeder state | implemented; not evidence of physical Switch 2 input |
| Xbox One/Series semantic input | base GIP body is publicly corroborated | implemented in isolated codecs; replay/unit verified |
| Xbox One/Series USB enumeration | no descriptor/session has been registered | blocked by real-device capture and transport positive control |
| Xbox authentication | current wired reference performs live randomized cryptography | blocked pending lawful device-side feasibility; no replay or credential copying |
| Four independent virtual Xbox actuators | WGI and GameInput expose four fields, but persona support is not established | blocked by direct/imported basis-vector captures and capability query |
| Switch 2 Pro USB input | descriptor and SDL USB path are corroborated; one `0x0201` device was passively captured | hardware-observed framing; runtime adapter/calibration still gated |
| Switch 2 Pro BLE input | custom GATT facts are capture-backed; official SDL path is a stub | codec/replay safe; live transport blocked by Windows/hardware matrix |
| Joy-Con 2 L/R BLE | advertisements, UUIDs, and basic reports are partly known | codec/replay safe; pairing, motion, merge, output need hardware gates |
| Charging Grip/wired Joy-Con 2 | no complete audited transport | unsupported |
| Switch 2 application association | controller NVM mutation and an AES-based exchange are described | blocked; explicit consent, recovery, and Windows key-provisioning evidence required |
| Switch 2 factory stick calibration | packed format and factory addresses agree | safe parser work |
| Switch 2 user calibration | right-stick address conflicts (`0x1FC060` versus `0x1FC080`) | live use blocked pending fresh before/after dumps |
| Switch 2 HD-rumble envelope | five-byte encoding and 16/42-byte grouping are partly corroborated | codec-only; band meaning, cadence, stop, thermal and side routing need measurement |
| Xbox to DualSense feedback | four semantic lanes can be mapped to body rumble and trigger-capable effects | policy can be designed; physical transfer remains unverified |
| Xbox to ordinary rumble | deterministic saturating downmix is possible | approximate by definition; policy/tests still required |
| Xbox/DualSense to Switch 2 HD rumble | no native trigger actuator exists; synthesis is required | approximate and hardware-gated |
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
- The xone wired match is interface zero with class/subclass/protocol
  `FF/47/D0`; data uses 64-byte interrupt IN/OUT buffers. Endpoint addresses,
  intervals, the complete configuration, strings, BOS data, EP0 behavior, and
  audio topology still require a physical capture.
- GIP command identifiers include ACK `0x01`, announce `0x02`, authenticate
  `0x06`, virtual key `0x07`, rumble `0x09`, HID report `0x0B`, and input
  `0x20`. See the pinned
  [framing implementation](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/bus/protocol.c).
- The base input body is fourteen little-endian bytes: buttons, two 16-bit
  triggers, and four signed 16-bit sticks. Guide is a separate virtual-key
  command. Share is capability-dependent.
- The rumble body is nine bytes: an unknown/reserved byte, enable mask,
  left-trigger, right-trigger, left-body, right-body, duration, delay, and
  repeat. See the pinned
  [gamepad structures](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/driver/gamepad.c).
- The wired reference starts a randomized certificate/key-exchange and
  transcript verification during probe. See
  [`auth.c`](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/auth/auth.c).
- Public XInput accepts only two body-motor values. WGI `GamepadVibration` and
  GameInput `GameInputRumbleParams` expose four fields, but GameInput may fold
  unsupported trigger channels. The device capability result and raw output
  packet must both be captured.

### Competitor comparison

PadForge does not use HIDMaestro's USB/IP backend for Xbox. Its Xbox
One/Series path creates a UMDF HID surface and relies on inbox `xinputhid`; its
Xbox 360/non-xinputhid path also uses an XUSB-facing companion. Input is copied
through shared memory and signaled with an event. This is a different
presentation architecture, so its HID descriptor is not a captured physical
Xbox USB persona.

HIDMaestro's approximately tens-of-microseconds benchmark starts at its state
submission API and ends at a busy-polled consumer in the same high-priority
process. PadForge adds its own nominal one-millisecond loop containing SDL
polling, enumeration, mapping, macros, feedback, and submission. Neither span
is physical-controller-to-game latency, and neither proves a 50 ms USB/IP
floor.

The competitor's four-actuator evidence is unresolved: its own WGI notes show
motor-bearing values failing to reach one virtual path, and its GameInput
notes conflict. No redistributable golden capture establishes its private
four-channel packet assumptions.

### Inferences and unknowns

- It is reasonable, but unproven, that a genuine wired Xbox transported
  through the exact USB/IP stack will bind like direct hardware.
- It is unknown whether Windows will accept a lawful synthetic/self-auth
  device, requires non-redistributable credentials, or permits useful input
  before authentication.
- A static authentication transcript cannot establish feasibility because
  randoms and session keys change.
- It is unknown which documented Windows surface will deliver four distinct
  channels to the eventual persona across representative games.
- A 50--124 ms tail is not attributable to USB/IP without QPC/ETW stage data.

The decisive first test is therefore a real wired Xbox positive control:
compare direct attachment with Linux export and exact-0.9.7.7 Windows import,
including driver binding, all claimed APIs, lifecycle, and four independent
output basis vectors. The full procedure is in
`xbox-one-usbip-feasibility.md`.

## Switch 2 findings

### Local hardware observation

One user-supplied Switch 2 Pro was observed without output, feature, WinUSB,
memory, serial, association, firmware, or rumble writes. Windows reported
`057E:2069`, device revision `0x0201`, HID interface zero through `HidUsb`,
vendor interface one through `WinUSB`, and audio interface two through
`usbaudio`. HID reports are 64-byte input/output values with gamepad usage
page `0x01`, usage `0x05`.

The standalone passive probe captured 2,048 exact report-`0x05` frames in
order. The raw local capture is intentionally outside git at
`_results/switch2-pro-usb-0x0201-passive-20260829/interactive-input-1.jsonl`,
SHA-256 `FF00B4A066CE00F4D8BBC11B1E1F4979EAB794B372ADAFD63CC3CF0F0D5D7CBC`.
Its metadata redacts the device path and does not query a serial. The probe's
QPC values establish replay order only; managed serialization and queued HID
reads make them unsuitable as latency evidence.

### Facts

- Switch 2 Pro is `057E:2069`. Captures show five USB interfaces, a 64-byte
  HID interrupt interface, and a vendor bulk command interface. Official SDL
  claims the command interface while using the normal HID path for input.
  Its Bluetooth initializer explicitly declines the device. See
  [`SDL_hidapi_switch2.c`](https://github.com/libsdl-org/SDL/blob/c71abd08605b8bb7078372307a93274725c99fe0/src/joystick/hidapi/SDL_hidapi_switch2.c).
- BLE advertisements use company `0x0553`, embed Nintendo vendor `0x057E`,
  and distinguish right Joy-Con `0x2066`, left Joy-Con `0x2067`, and Pro
  `0x2069`. A remembered host address is sensitive identity data and must be
  redacted.
- Switch 2 controllers use custom GATT rather than HOGP in the audited
  captures. Roles must be selected by UUID and properties, never by handle or
  sorted ordinal; later Pro firmware adds characteristics.
- Common input report `0x05` is a 63-byte body. USB prepends report ID `0x05`;
  BLE selects the report by characteristic UUID. Counter is at `0x00`, buttons
  `0x04`, sticks `0x0A`/`0x0D`, mouse `0x10`, magnetometer `0x19`, battery
  `0x1F`, and motion `0x2A`. Short packets must be rejected, not padded.
- Model reports `0x07`, `0x08`, and `0x09` have known basic controls but their
  packed motion block is not yet specified. A safe decoder retains that block
  as opaque.
- Nine calibration bytes contain six packed 12-bit values. Factory stick
  addresses agree; the user-right slot does not.
- The output envelope is one 16-byte LRA group per Joy-Con side or two for
  Pro, within a 42-byte body. Licensed SDL corroborates the five-byte encoder,
  four-bit counter, amplitude clamp, USB wrapper, and a 12 ms resend policy.
  SDL explicitly reports no native trigger-rumble actuator.

Primary fact sources are the pinned SDL file above and the capture-backed
research documents
[`bluetooth_interface.md`](https://github.com/ndeadly/switch2_controller_research/blob/d1c5a7f7ba298f83017fae84952a4e6d2ef8fc92/bluetooth_interface.md),
[`hid_reports.md`](https://github.com/ndeadly/switch2_controller_research/blob/d1c5a7f7ba298f83017fae84952a4e6d2ef8fc92/hid_reports.md), and
[`memory_layout.md`](https://github.com/ndeadly/switch2_controller_research/blob/d1c5a7f7ba298f83017fae84952a4e6d2ef8fc92/memory_layout.md).

### Rejected shortcuts

- Switch2Connect chooses characteristics by sorted position and matches
  responses mainly by command ID. Firmware changes or delayed responses can
  misroute both operations.
- Its `pair()` sequence does not implement the described contribution,
  challenge, verification, or Windows key installation flow and picks the
  first local Bluetooth address on multi-radio systems.
- The hifi SDL experiment reads magnetometer data from offsets that overlap
  other fields, uses a one-latest-report slot which can lose short edges, and
  does not retain/verify Windows preferred-connection negotiation correctly.
- A successful Windows preferred-parameters request is not the negotiated
  interval. The request is Windows 11 build 22000+ and the actual result must
  be measured; throughput mode can reduce simultaneous-device capacity.
- No audited evidence justifies calling hard-coded key material safe pairing,
  forwarding arbitrary raw commands, padding short reports, or promising a
  5 ms Windows BLE interval.

### Unknowns and gates

Live association, Windows key provisioning, NVM eviction/recovery, exact
initialization profiles, sensor scale and timestamp units, user-right
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

Proceed now with strict semantic values, allocation-free codecs, bounded
ordered replay, capture conversion/redaction, feedback ownership, and probes.
Do not register a guessed Xbox persona, perform Nintendo association, write
controller memory, start live Switch 2 output, copy credentials, or port the
native driver.

The next external evidence required is:

1. a real wired Xbox controller plus a Linux exporter for the direct/imported
   positive control and sanitized descriptors/transcripts;
2. project-owned Switch 2 Pro USB/BLE and Joy-Con 2 L/R captures from recorded
   firmware revisions, including calibration and rumble basis tests;
3. explicit authorization before any association/NVM mutation, system driver
   installation, or native-backend phase.

## Live checklist

| Item | Implemented | Hardware verified | Replay/simulation verified | Blocked |
|---|---:|---:|---:|---:|
| baseline and artifact pins | yes | installed state observed | yes | no |
| bounded Xbox 360 scheduler | yes | no new hardware run | yes | no |
| canonical four-actuator feedback value | yes in both repositories, unwired | no | yes | no |
| Xbox GIP body codecs | yes; framing deliberately absent | no | yes | framing capture gate |
| Xbox USB persona/session | no | no | no | auth/capture gate |
| Switch 2 pure report/calibration codecs | in progress | one USB stream captured | pending | no |
| Switch 2 live USB/BLE transport | no | no | no | hardware/capture gate |
| Switch 2 association | no | no | no | security/recovery/authority gate |
| HD-rumble translation/tuning | no | no | no | physical measurement gate |
| latency parity | no | no | no | same-method baseline/competitor gate |

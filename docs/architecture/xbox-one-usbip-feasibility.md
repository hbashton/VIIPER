# Xbox One/Series presentation through USB/IP: feasibility decision

Status: evidence gate open; no production Xbox One persona is registered.

This decision covers Windows PC controller presentation only. It does not
target an Xbox console, extract credentials, bypass authentication, or
authorize a new native driver.

## Decision

Keep usbip-win2 0.9.7.7 as the first candidate backend, but do not construct a
synthetic Xbox One/Series USB identity from HIDMaestro's HID profile or from a
replayed initialization transcript.

The next transport proof is a positive control: export a real wired Xbox
One/Series controller from Linux and import it on the same Windows build using
the exact pinned usbip-win2 0.9.7.7 userspace and drivers. Compare direct and
imported enumeration, driver binding, API visibility, input, guide/share, and
raw output captures for four independent actuator basis vectors. This test
isolates whether USB/IP preserves a real supported device before synthetic
descriptor or protocol work can confound the result.

Until that hardware test and a lawful device-side authentication feasibility
probe pass, safe implementation is limited to the transport-neutral semantic
state, versioned DS4Windows/VIIPER contract, four-actuator feedback ownership,
bounds-checked codecs, sanitized capture conversion, and deterministic replay.

## Source pins and legal treatment

| Source | Pin | License | Treatment |
|---|---|---|---|
| usbip-win2 | tag `v.0.9.7.7`, `7c219953101cc5d0ec9a0bcb3eb87259cf72bedd` | BSD-2-Clause | architecture and source evidence; locally installed hashes separately verified |
| xone | `3484f603484782dd7551c64e5a33fc602b127051` | GPL-2.0-or-later files; GPLv2 license text at root | protocol/behavior evidence; new VIIPER code must be independently written and provenance-recorded |
| HIDMaestro stable checkout | `46054b862830fcec7bc98d72ccb7c4f0c0179fb1` | MIT for authored code | Windows architecture evidence only; profiles/captures/binaries require their own provenance |
| HIDMaestro current | `9df50410230c11b410f43909ede0e5fc8b23d15b` | MIT for authored code | development evidence, not attributed to stable |
| PadForge stable | `0794fd01bd19f4c096b982ffc824b88bce5ed743` | CC BY-NC-SA 4.0 | clean-room behavioral evidence only; no code or close paraphrase |
| PadForge `v4-dev` | `474d890db12c71d3a825ffd16c8d9dc1223aaff2` | CC BY-NC-SA 4.0 | separate development evidence; never attributed to v4.3.2 |

Primary source links are pinned so later upstream changes cannot silently alter
the evidence:

- [xone wired transport](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/transport/wired.c)
- [xone gamepad packet bodies](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/driver/gamepad.c)
- [xone GIP framing and dispatch](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/bus/protocol.c)
- [xone live authentication](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/auth/auth.c)
- [usbip-win2 0.9.7.7 UDE receive path](https://github.com/vadimgrn/usbip-win2/blob/7c219953101cc5d0ec9a0bcb3eb87259cf72bedd/drivers/ude/wsk_receive.cpp)
- [usbip-win2 0.9.7.7 request completion](https://github.com/vadimgrn/usbip-win2/blob/7c219953101cc5d0ec9a0bcb3eb87259cf72bedd/drivers/ude/device_ioctl.cpp)

## Direct observations

### USB/IP transport

The exact installed usbip-win2 build is 0.9.7.7. `usbip.exe`,
`usbip2_ude.sys`, and `usbip2_filter.sys` match the DS4Windows fail-closed
hashes. The UDE driver receives USB/IP messages on a kernel WSK receive thread,
maps `RET_SUBMIT` responses back to pending WDF requests, and completes URBs.
The tag-head change explicitly uses `UdecxUrbComplete` for successful URBs and
`UdecxUrbCompleteWithNtStatus` for errors.

The current live VIIPER DualSense enumerates under this root with composite
USB, HID, and audio children. This proves real USB topology and URB transport,
not Xbox compatibility or latency parity.

### Wired GIP shape

At the pinned xone revision, the wired data interface match is vendor-specific
class/subclass/protocol `FF/47/D0`, interface zero. The driver discovers
interrupt IN and OUT endpoints and services 64-byte data buffers. Endpoint
address, interval, complete configuration, strings, BOS data, audio interface,
and EP0 behavior remain capture inputs; they are not supplied by the class
match alone.

GIP frames contain command, options/client bits, a nonzero sequence, a
variable-length packet length, optional chunk offset, and a body. The simple
gamepad input body is 14 bytes: buttons, two 16-bit triggers, and four 16-bit
sticks. The rumble body is 9 bytes and carries an enable mask, independent
left-trigger, right-trigger, left-body, and right-body magnitudes plus timing
fields. Guide is delivered through a distinct virtual-key command; Share is a
capability-dependent extension, not one of the base 14 bytes.

These packet bodies are sufficient for isolated golden codecs. They are not a
complete Windows device session.

### Authentication blocker

The pinned wired implementation begins authentication during gamepad probe.
It is not a static list of packets: host random values contribute to the
transcript; one path requests a client X.509 certificate/public key, exchanges
an RSA-protected premaster secret, and verifies a PRF result; another performs
ECDH and transcript verification.

It is unknown whether current Windows accepts a lawful ephemeral/self-auth
device path, requires device credentials not available for redistribution, or
uses a different policy for particular firmware. Capturing and replaying one
session cannot answer this because randoms and session keys change. VIIPER
must not extract, redistribute, forge, or bypass controller credentials.

### HIDMaestro is not a raw wired-persona specification

HIDMaestro's Xbox One/Series profiles contain a 262-byte HID report descriptor
for its UMDF/xinputhid-facing architecture. The Series profile deliberately
extends a native ten-button artifact to twelve buttons for Share. HIDMaestro
also coordinates root-enumerated HID, inbox `xinputhid`, and, for other Xbox
paths, a separate XUSB companion and shared-memory/event handoff.

Therefore those profile bytes describe a compatibility surface in that driver
stack, not an independently captured physical Xbox USB device descriptor.
Putting the descriptor behind VIIPER USB/IP would test a different,
undocumented binding assumption and a failure would not disprove USB/IP.

Standard `XInputSetState` exposes two body motors. HIDMaestro/PadForge's own
four-actuator routes include undocumented extended or other output surfaces;
their current evidence does not establish a supported Windows API contract
which every game uses for four independent channels.

## Fact, inference, and unknown matrix

| Item | Status | Consequence |
|---|---|---|
| usbip-win2 transports descriptor/control/interrupt topology through UdeCx | fact from pinned source and live DualSense | retain it as a candidate |
| real wired Xbox data uses 64-byte interrupt IN/OUT on `FF/47/D0` | fact from pinned xone source | do not model it as bulk or copy HID endpoint timing |
| base GIP input and four-actuator rumble bodies are 14 and 9 bytes | fact from pinned source | safe for isolated bounds-checked codecs |
| current wired startup includes a live cryptographic authentication exchange | fact from pinned source | static replay is invalid; credentials must not be guessed |
| a real Xbox transported over usbip-win2 will bind identically on this Windows build | untested inference | requires the positive control |
| a lawful synthetic device can complete Windows authentication | unknown | blocks a production raw-GIP persona |
| HIDMaestro's HID descriptor would automatically bind documented `xinputhid` through USB/IP | unknown/undocumented | do not use as the production plan |
| four independent game actuator values are available through a stable documented Windows surface for this persona | unknown by API/game | capture each claimed source; never infer from packet length |
| removing TCP/USB-IP would by itself eliminate observed 50–124 ms tails | unsupported inference | stage timestamps/ETW must attribute each tail first |

## Positive-control procedure

Use a disposable test environment and preserve all raw artifacts.

1. Record Windows build, host hardware, controller model/firmware, Linux exporter
   kernel/tool versions, usbip-win2 executable/driver hashes, and direct USB
   descriptors.
2. Capture direct Windows enumeration, driver stack, container/interface IDs,
   XInput/GameInput/WGI/Raw Input/HID/SDL/Steam visibility, guide/share, input,
   suspend/resume, unplug, and output.
3. Export the unchanged physical controller from Linux and import it through
   the exact Windows 0.9.7.7 stack. Do not replace a driver or alter controller
   firmware/credentials.
4. Repeat the same observations. Send body-left, body-right, left-trigger, and
   right-trigger basis vectors through every claimed Windows API while
   capturing the physical USB OUT traffic.
5. Compare topology, binding, identities, API behavior, packet sequence,
   acknowledgements, authentication, and output byte-for-byte where exactness
   is expected. Measure direct versus USB/IP with the same stage boundaries.

A positive result permits a synthetic session feasibility probe using exact
sanitized captures and independently written codecs. A negative result is a
USB/IP compatibility result only after exporter/importer correctness is also
established. A synthetic device failing authentication proves only that the
synthetic implementation is incomplete.

## Integration seam and native boundary

If the evidence gates pass, the future `device/xboxone` engine will own:

- one versioned canonical input contract;
- persona/session-specific GIP framing, init, sequence, acknowledgement,
  control, power, and teardown;
- `inputpresentation.Source` for bounded ordered transitions plus replaceable
  continuous state and immutable retry;
- a typed four-actuator feedback callback with explicit neutral/stop and
  generation/ownership metadata.

USB/IP remains a presentation backend below that core. A separately authorized
future UdeCx backend would implement the same claim/resolve boundary and must
not fork mapping or feedback policy.

No native-driver implementation is authorized by this decision. If the
positive control or lawful authentication gate fails, complete the safe
semantic/codec/replay work, mark Xbox presentation blocked, and request a
separate architecture decision from the user. Do not silently substitute a
private GUID, reverse-engineered IOCTL, copied credential, or unrelated mapping
stack.

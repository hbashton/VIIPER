# DualSense Controller

VIIPER emulates USB-connected DualSense and DualSense Edge devices,
including controls, touch, motion, adaptive triggers, lightbar, speaker,
advanced haptics, and microphone endpoints.

## Supported device types

The registry exposes only production V5 contracts. Input size is negotiated by
the exact device type; there is no per-frame downgrade:

| Device type | Input | Output event `0x85` | USB functions |
| --- | --- | --- | --- |
| `dualsensecombinedaudioduplexv5` | 33 bytes | No | DualSense HID, speaker/haptics OUT, microphone IN |
| `dualsenseaudioonlyduplexv5` | 33 bytes | No | DualSense audio sidecar |
| `dualsensegamepadv5` | 33 bytes | No | DualSense HID only |
| `dualsensecombinedaudioduplexv5events` | 33 bytes | Yes | DualSense HID and audio |
| `dualsenseaudioonlyduplexv5events` | 33 bytes | Yes | DualSense audio sidecar |
| `dualsensecombinedaudioduplexv5rawinputevents` | 53 bytes | Yes | DualSense HID and audio |
| `dualsenseaudioonlyduplexv5rawinputevents` | 53 bytes | Yes | DualSense audio sidecar |
| `dualsensegamepadv5rawinput` | 53 bytes | No | DualSense HID only |
| `dualsenseedgecombinedaudioduplexv5` | 33 bytes | No | DualSense Edge HID and audio |
| `dualsenseedgegamepadv5` | 33 bytes | No | DualSense Edge HID only |
| `dualsenseedgecombinedaudioduplexv5events` | 33 bytes | Yes | DualSense Edge HID and audio |
| `dualsenseedgecombinedaudioduplexv5rawinputevents` | 53 bytes | Yes | DualSense Edge HID and audio |
| `dualsenseedgegamepadv5rawinput` | 53 bytes | No | DualSense Edge HID only |

Deprecated pre-V5 raw, extended, V1, V2, V3, and V4 names are intentionally
not registered. Clients must use an exact V5 alias; VIIPER does not silently
negotiate an older header or split audio/state transport.

## V5 stream contract

Every packet uses a 16-byte header followed by its payload:

| Offset | Size | Field |
| --- | --- | --- |
| 0 | 4 | ASCII `VPCM` |
| 4 | 1 | Version `0x05` |
| 5 | 1 | Frame type |
| 6 | 2 | Payload length, little endian |
| 8 | 4 | Monotonic sequence, little endian |
| 12 | 4 | IEEE CRC32, little endian |

The CRC covers header bytes 4 through 11 followed by the payload. Sequence
numbers are shared by every frame type in one direction. A version mismatch,
sequence gap, CRC mismatch, invalid payload length, or unknown frame type
closes the stream instead of changing protocols.

| Direction | Type | Payload |
| --- | --- | --- |
| Client to VIIPER | `0x01` | Exact 33- or 53-byte controller input state selected by device alias |
| Client to VIIPER | `0x02` | 1,920-byte microphone PCM block: stereo S16LE, 48 kHz, 10 ms |
| VIIPER to client | `0x81` | 474-byte current combined controller feedback |
| VIIPER to client | `0x83` | Atomic feedback plus the matching 1,920-byte speaker PCM generation |
| VIIPER to client | `0x85` | Microphone-interface active byte plus 64-bit stream generation, event aliases only |

An atomic `0x83` payload begins with a little-endian 16-bit feedback length,
then the 474-byte feedback object, then exactly 480 stereo S16LE speaker
frames. The four-channel virtual USB source is preserved at 48 kHz: front
left/right become the speaker generation, while rear left/right independently
complete the 512-frame advanced-haptics clock. At each 480-frame presentation
boundary VIIPER consumes one completed rear sample or emits silence for that
lane, matching the proven V5 cadence without replaying stale haptics.

Controller state, adaptive triggers, lightbar, rumble, haptics, and speaker
data are serialized by one V5 writer. Media backpressure is bounded and
newest-wins; interface resets form a hard generation boundary so stale audio
cannot cross a stop/reconnect.

## Input state

Every input payload begins with the same 33-byte little-endian mapped state:

- Sticks: LX, LY, RX, RY as signed 8-bit values.
- Buttons: 32-bit bitfield.
- D-pad: 8-bit bitfield.
- L2 and R2: unsigned 8-bit values.
- Two touch contacts: X/Y, active flag, and tracking ID.
- Gyroscope and accelerometer: three signed 16-bit axes each.

Motion values use DS4Windows' calibrated canonical scale: gyroscope 16 units
per degree/second and accelerometer 8192 units per g. Feature `0x05` describes
that same scale for both DualSense personas, so a calibration-aware game does
not apply an unintended second gyro gain. These are virtual calibration values,
not a copied physical controller's factory coefficients.

Button bits:

| Control | Value |
| --- | --- |
| Square | `0x00000010` |
| Cross | `0x00000020` |
| Circle | `0x00000040` |
| Triangle | `0x00000080` |
| L1 / R1 | `0x00000100` / `0x00000200` |
| L2 / R2 | `0x00000400` / `0x00000800` |
| Create / Options | `0x00001000` / `0x00002000` |
| L3 / R3 | `0x00004000` / `0x00008000` |
| PS | `0x00010000` |
| Touchpad click | `0x00020000` |
| Mic mute | `0x00040000` |
| Edge LFn / RFn | `0x00100000` / `0x00200000` |
| Edge L4 / R4 | `0x00400000` / `0x00800000` |

D-pad bits are Up `0x01`, Down `0x02`, Left `0x04`, and Right `0x08`.

### Raw-input extension

Only exact `...v5rawinput...` aliases require the 53-byte payload. Existing
legacy aliases and `...v5events` aliases remain exactly 33 bytes; the events
suffix negotiates output lifecycle events, not enhanced input.

| Payload bytes | Meaning |
| --- | --- |
| `0:33` | mapped state described above |
| `33` | flags: bit 0 physical metadata valid; bit 1 physical source uses Edge layout |
| `34:38` | physical input report bytes `28:32`, normalized from USB or Bluetooth |
| `38:53` | physical input report bytes `41:56`, normalized from USB or Bluetooth |

Bit 1 is valid only together with bit 0; unknown bits close the framed stream.
Legacy decode explicitly clears the extension fields to prevent stale metadata
after reconnect or alias changes.

Valid metadata supplies the physical sensor timestamp, trigger mechanism and
effect status, host timestamp echo, battery, and common headset/filter status.
VIIPER copies physical report
bytes 49 through 52 only when the physical source and virtual target both use
the same base or Edge layout. On a mismatch it synthesizes the target layout:
base uses its generated device clock, while Edge uses `80 00 00 00`. The
virtual connection byte `54` always sets USB-wired bit `0x08` even when the source
report arrived over Bluetooth, while retaining physical headset/microphone
presence and mute bits `0x07`. Without valid metadata, trigger status is the
confirmed physical off/no-load value `09 09`, effect status is `00`, and the
clock/profile and battery use VIIPER's generated state.

Physical report bytes 56 through 63 are an eight-byte AES-CMAC. They are
deliberately excluded: remapping controls and replacing virtual
counters/connect state invalidates the physical authentication tag, and
VIIPER cannot recompute it without the controller key. The corresponding
virtual report tail stays zero.

Physical metadata is continuous latest-state data and never creates an ordered
queue entry by itself. A real button, D-pad, touch-contact/ID, or trigger
zero-crossing transition carries its complete metadata snapshot. In a trigger
epoch, peak strengthening couples only that trigger's status byte and effect
nibble; equal-analog settling such as L2 `28` to `29` is preserved before
release without turning normal mechanical progression into a stale FIFO.

## Feedback state

The 474-byte V5 feedback object contains:

- Bytes 0 through 5: compatible rumble, lightbar RGB, and player LEDs.
- Bytes 6 through 27: native-spaced R2 and L2 adaptive-trigger blocks.
- Bytes 28 through 75: the native 48-byte USB output report `0x02`.
- Bytes 76 through 473: the current 398-byte combined Bluetooth carrier.

The combined carrier keeps state and media on one presentation clock. The
physical-controller bridge supplies the encoded speaker lane and forwards it
using its V5 transport.

### DualSense Edge configuration boundary

Edge USB output `0x02` has 64 bytes including its report ID. Ordinary effects
use the same 48-byte prefix as DualSense, but the remaining bytes are **not
always padding**. Edge configuration previews carry stick-curve and trigger
deadzone parameters there; Edge extension commands also control profile-related
behavior. The fixed V5 feedback object cannot transport those commands intact.

Until a separately negotiated complete configuration transport and virtual
profile model exist, Edge commands with USB byte 39 bit `0x80` or USB byte 41
bit `0x80` are rejected atomically. No bundled rumble, trigger effect, LED, or
partial configuration reaches the feedback queue or persistent audio state.
EP0 `SET_REPORT` receives a USB STALL; interrupt OUT receives the existing
failed-admission response with zero accepted bytes. Later ordinary game effects
remain usable without reconnecting. Standard DualSense behavior is unchanged.

Edge onboard-profile features `0x60..0x65`, `0x68`, and `0x70..0x7B` likewise
STALL rather than acknowledging an unimplemented read or write. Edge feature
`0x80` profile-snapshot command `0x70` also STALLs without overwriting the last
implemented command response. Ordinary serial/status/sensor queries remain
unchanged. VIIPER does not
edit profiles stored on a physical Edge. Feature `0x20` remains a synthetic
identity rather than a captured complete Edge firmware response.

Protocol references:

- [dualsense-tester stick preview](https://github.com/daidr/dualsense-tester/blob/f6e6247fd66ada9c8b63f3ba62c6d72945c0da53/src/router/DualSenseEdge/views/_Profile/pages/JoystickSensitivity.vue)
- [dualsense-tester trigger preview](https://github.com/daidr/dualsense-tester/blob/f6e6247fd66ada9c8b63f3ba62c6d72945c0da53/src/router/DualSenseEdge/views/_Profile/pages/TriggerDeadZone.vue)
- [Titania output structure](https://github.com/neptuwunium/titania/blob/develop/src/structures.h) and [Edge controls](https://github.com/neptuwunium/titania/blob/develop/src/hid.c)
- [DS5Dongle Edge profile-snapshot preparation](https://github.com/awalol/DS5Dongle/blob/c67c7f685fe8d8cc44f519d27710c5a639a1be7d/src/dse.cpp#L50-L75)

These are source-verified report contracts, not a claim of physical hardware
validation or complete Sony firmware-management compatibility.

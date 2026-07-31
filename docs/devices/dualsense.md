# DualSense Controller

VIIPER emulates complete USB-connected DualSense and DualSense Edge devices,
including controls, touch, motion, adaptive triggers, lightbar, speaker,
advanced haptics, and microphone endpoints.

## Supported device types

VIIPER 0.0.6 exposes only the production PadSense-derived V5 contracts:

| Device type | USB functions |
| --- | --- |
| `dualsensecombinedaudioduplexv5` | DualSense HID, speaker/haptics OUT, microphone IN |
| `dualsenseaudioonlyduplexv5` | DualSense speaker/haptics OUT and microphone IN sidecar |
| `dualsenseedgecombinedaudioduplexv5` | DualSense Edge HID, speaker/haptics OUT, microphone IN |

The old raw, extended, V1, V2, V3, and V4 names are intentionally not
registered. Clients must use V5; VIIPER does not silently negotiate an older
header or split audio/state transport.

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
| Client to VIIPER | `0x01` | 33-byte controller input state |
| Client to VIIPER | `0x02` | 1,920-byte microphone PCM block: stereo S16LE, 48 kHz, 10 ms |
| VIIPER to client | `0x81` | 474-byte current combined controller feedback |
| VIIPER to client | `0x83` | Atomic feedback plus the matching 1,920-byte speaker PCM generation |

An atomic `0x83` payload begins with a little-endian 16-bit feedback length,
then the 474-byte feedback object, then exactly 480 stereo S16LE speaker
frames. The four-channel virtual USB source is preserved at 48 kHz: front
left/right become the speaker generation, while rear left/right independently
complete the 512-frame advanced-haptics clock. At each 480-frame presentation
boundary VIIPER consumes one completed rear sample or emits silence for that
lane, matching the proven PadSense cadence without replaying stale haptics.

Controller state, adaptive triggers, lightbar, rumble, haptics, and speaker
data are serialized by one V5 writer. Media backpressure is bounded and
newest-wins; interface resets form a hard generation boundary so stale audio
cannot cross a stop/reconnect.

## Input state

The 33-byte input payload is little endian:

- Sticks: LX, LY, RX, RY as signed 8-bit values.
- Buttons: 32-bit bitfield.
- D-pad: 8-bit bitfield.
- L2 and R2: unsigned 8-bit values.
- Two touch contacts: X/Y, active flag, and tracking ID.
- Gyroscope and accelerometer: three signed 16-bit axes each.

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

## Feedback state

The 474-byte V5 feedback object contains:

- Bytes 0 through 5: compatible rumble, lightbar RGB, and player LEDs.
- Bytes 6 through 27: native-spaced R2 and L2 adaptive-trigger blocks.
- Bytes 28 through 75: the native 48-byte USB output report `0x02`.
- Bytes 76 through 473: the current 398-byte combined Bluetooth carrier.

The combined carrier keeps state and media on one presentation clock. The
physical-controller bridge supplies the encoded speaker lane and forwards it
using its PadSense-derived transport.

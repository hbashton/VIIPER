# DualSense Bluetooth Haptics Notes

These notes track the implementation path for forwarding a game's virtual
DualSense haptics to a physical Bluetooth DualSense.

## Ground truth

- SAxense (`egormanga/SAxense`) streams haptics to a Bluetooth DualSense by
  writing HID output report `0x32` to the controller's `hidraw` node.
- SAxense accepts signed 8-bit stereo PCM at 3000 Hz.
- Each Bluetooth report is 141 bytes:
  - byte 0: report ID `0x32`
  - byte 1: tag/sequence nibble
  - packet `0x11`: stream/control packet, length 7, body
    `FE 00 00 00 00 FF <interval-counter>`
  - packet `0x12`: haptics PCM packet, length 64, body is 64 bytes of stereo
    PCM
  - final 4 bytes: DualSense Bluetooth CRC32 with seed `0xEADA2D49`
- VIIPER now has `BuildBluetoothHapticsReport` mirroring that packet layout so
  the runtime can package haptics PCM without re-discovering the format.

## PadForge / HIDMaestro read

PadForge and HIDMaestro are useful references, but mostly for the HID side of
the problem.

What the public code proves:

- PadForge uses HIDMaestro to create virtual DualSense / DualSense Edge HID
  devices.
- HIDMaestro profiles declare USB DualSense output report `0x02` as 48 bytes.
  The important HID fields are:
  - byte 1: `validFlag0`
  - byte 2: `validFlag1`
  - bytes 3-4: compatible rumble motors
  - byte 9: mute LED
  - bytes 11-21: right trigger effect
  - bytes 22-32: left trigger effect
  - bytes 45-47: lightbar RGB
  - bytes 1-47: `effectPayload`
- HIDMaestro profiles declare Bluetooth DualSense output report `0x31` as 78
  bytes, with a rolling tag, BT flag byte, shifted HID field offsets, and a
  CRC32 footer using prefix `[0xA2, 0x31]`.
- PadForge listens for HIDMaestro `OutputDecoded`, takes the decoded
  `effectPayload`, and forwards it to an assigned physical DualSense via
  `SDL_SendGamepadEffect`.
- PadForge also has a raw HID writer that encodes profile fields through
  HIDMaestro and writes to the physical HID path directly, bypassing SDL's
  effect state machine when needed.

What the public code does not prove:

- It does not obviously expose a virtual DualSense USB Audio Class function to
  games.
- It does not obviously capture host-to-device isochronous audio endpoint
  transfers from a game.
- Its README "audio" features appear, from source review, to include
  audio-reactive user effects and HID effect passthrough, not necessarily the
  native DualSense advanced-haptics audio interface that Ghost of Tsushima uses
  on a wired controller.

Practical takeaway:

- Use HIDMaestro's profiles as a spec oracle for USB `0x02` and Bluetooth
  `0x31` HID report mapping.
- Keep PadForge's queueing model in mind: HID output callbacks should enqueue
  and return quickly, while a worker forwards effects to physical hardware.
- Do not assume PadForge solves advanced haptics audio for VIIPER. The missing
  piece is still a virtual USB audio/haptics function or another endpoint path
  that lets the game send PCM-like haptics data to the virtual controller.

## What Ghost of Tsushima is likely sending

For native DualSense behavior, Ghosts can send separate channels of data:

- HID output report `0x02`: adaptive trigger state, lightbar/player LEDs, mute
  LED, and ordinary compatible rumble flags.
- USB audio stream: advanced haptics, exposed by a real wired DualSense as an
  audio function. SAxense's input format strongly suggests that the useful
  Bluetooth haptics payload is 3000 Hz stereo 8-bit PCM.

VIIPER's current DualSense device is HID-only. It captures report `0x02`, but it
does not expose a virtual DualSense USB audio function yet. That means a game
that sends advanced haptics through the DualSense audio interface has nowhere to
send those frames in the current virtual device, and the HID traffic dump alone
cannot recover them.

## Required next implementation path

1. Capture a real wired DualSense USB descriptor, including all audio
   interfaces, alternate settings, isochronous endpoints, and class-specific
   audio descriptors.
2. Extend VIIPER's DualSense descriptor to expose the same USB audio/haptics
   interface in addition to the HID interface.
3. Route host-to-device audio endpoint transfers into a DualSense haptics
   diagnostics ring buffer.
4. Add an experimental bridge that converts captured haptics PCM into
   `BuildBluetoothHapticsReport` frames and writes them to the physical
   Bluetooth DualSense transport.
5. Keep HID report `0x02` handling separate for adaptive triggers and LEDs.

## Near-term VIIPER work from the references

1. Add a USB `0x02` to BT `0x31` mapping helper using HIDMaestro's declared
   offsets and CRC32 scope. This is for ordinary HID effects: rumble, lightbar,
   mute LED, player LEDs, and adaptive trigger command blobs.
2. Keep the raw USB `0x02` report in diagnostics, because Ghost's HID output
   tells us when it is selecting the haptics path versus ordinary rumble.
3. Add audio endpoint capture only after the descriptor exposes the real
   DualSense audio interfaces. Until then, captures will show HID output only.

## Debug workflow

1. Start DualSense traffic capture in DS4Windows' VIIPER debugger.
2. Keep capture running while the game has focus.
3. Exercise the feature in game.
4. Return to DS4Windows and use Export DS Traffic.
5. Inspect the exported JSON for HID reports. If no audio endpoint capture is
   present, that confirms the current virtual device is still HID-only and the
   audio descriptor work is the next blocker.

## Ghost of Tsushima bow capture, 2026-06-14

Capture file:

- `%APPDATA%\DS4Windows\Logs\dualsense_traffic_20260614_103717.json`

Summary:

- 2,497 total traffic events.
- 1,248 parsed DualSense output reports.
- Three clear bow-draw clusters were captured.
- Each bow draw lasts roughly 1.5-1.7 seconds.
- Each bow draw contains:
  - 47-48 adaptive trigger output reports.
  - 94-100 compatible rumble output reports.
  - large motor rumble ramping up to `0xFF`.

Representative trigger sequence:

```text
21 ff 03 49 92 24 09 00 00 00 00
26 ff 03 49 92 24 09 00 00 7f 00
26 ff 03 49 92 24 09 00 00 7d 00
26 ff 03 49 92 24 09 00 00 7b 00
26 ff 03 49 92 24 09 00 00 79 00
26 ff 03 49 92 24 09 00 00 77 00
26 ff 03 49 92 24 09 00 00 75 00
26 ff 03 92 24 49 12 00 00 74 00
26 ff 03 92 24 49 12 00 00 72 00
26 ff 03 92 24 49 12 00 00 70 00
26 ff 03 92 24 49 12 00 00 6e 00
26 ff 03 92 24 49 12 00 00 6c 00
```

Representative compatible rumble report:

```text
02 02 40 00 ff 00 00 00 ...
```

Interpretation:

- Ghost is definitely driving the adaptive trigger over normal DualSense HID
  output report `0x02`.
- The harsh vibration felt during bow draw is visible as a compatible rumble
  ramp, not just inferred haptics.
- This capture does not include report `0x32` haptics PCM. That is expected:
  report `0x32` is the Bluetooth HID report SAxense writes to the physical
  controller after it already has PCM.
- The next bridge should first forward HID report `0x02` as Bluetooth `0x31`
  for adaptive triggers, while keeping compatible rumble separate from future
  SAxense-style haptics PCM.

Credit: SAxense research by egormanga/Sdore should be credited anywhere this
Bluetooth haptics packet path is surfaced to users or shipped.

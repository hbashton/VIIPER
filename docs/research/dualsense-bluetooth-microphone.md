# DualSense Bluetooth Microphone Notes

This document records the evidence and integration constraints for presenting a
physical Bluetooth DualSense microphone through a virtual DualSense audio
device. It is deliberately separate from the haptics notes: microphone capture
is controller-to-host traffic, while advanced haptics are host-to-controller
audio traffic.

## Conclusion

Bluetooth microphone capture from a physical DualSense is possible. This is not
a speculative protocol: two independently maintained Pico W bridge projects
implement it and expose it as a standard USB Audio Class capture endpoint.

The missing piece for DS4Windows is Windows transport validation. The working
bridges own the Bluetooth HID interrupt L2CAP channel directly. DS4Windows uses
Windows HidBth through a HID device handle, so it must prove both of these paths
before a user-facing microphone feature is claimed:

1. A Bluetooth control/audio report `0x36` of 398 bytes reaches the controller
   through `WriteFile` on the HidBth HID handle.
2. HidBth delivers microphone-tagged `0x31` reports to the existing HID input
   handle instead of filtering them.

The current DS4Windows receive loop is unsafe for the second case: it CRC-checks
every Bluetooth packet as a normal 78-byte input report before classifying the
packet. A microphone frame must be recognized and queued before normal input
CRC and gamepad parsing. Otherwise it can be counted as corruption and lead to
a disconnect.

## Observed Bluetooth microphone protocol

Both `awalol/DS5Dongle` and `hurryman2212/DS5_Bridge` implement the same
wire behavior:

- The controller microphone is enabled by bit 0 in byte 4 of a Bluetooth output
  report `0x36`. `0xFE` means disabled and `0xFF` means enabled.
- The control report is 398 bytes, contains a `0x11` stream/control subreport,
  a `0x10` SetStateData subreport, and a silent `0x12` haptics subreport.
- It is sent over the HID interrupt channel with HIDP output prefix `0xA2` and
  a DualSense Bluetooth CRC32 footer (seed `0xEADA2D49`).
- The controller returns a Bluetooth input report `0x31`. Bit 1 of its flag
  byte marks a microphone packet.
- In raw L2CAP framing, the Opus payload begins at byte 4: `A1 31 flags ...`.
  It is a 71-byte Opus frame, mono, 48 kHz, 480 samples per frame (10 ms).
- The bridge projects decode it with `opus_decode(..., 48000, 1, 480)` and
  duplicate mono samples into a stereo UAC capture endpoint because the real
  DualSense audio device is commonly enumerated by Windows as stereo capture.
- The mic stream is sticky after enabling. The OLED fork sends a silent control
  `0x36` at roughly 4 Hz only until frames arrive, then stops. It resumes that
  keepalive when the stream stalls. This avoids making microphone capture depend
  on a speaker stream and limits controller battery impact.

## Sources checked

All source repositories below are present in this workspace. DS5Dongle and its
OLED fork are MIT licensed. DS5_Bridge is AGPL-3.0, so it is a behavioral/spec
reference only and must not be copied into a non-AGPL component.

| Source | Relevant evidence |
| --- | --- |
| `external/DS5Dongle/src/main.cpp` | Classifies interrupt `0x31` reports with flag bit 1, then queues `data + 4`. |
| `external/DS5Dongle/src/audio.cpp` | Defines `MIC_OPUS_SIZE = 71`, `MIC_FRAMES = 480`, creates a 48 kHz mono Opus decoder, and sends report `0x36` with the mic enable bit. |
| `external/DS5Dongle/src/bt.cpp` | Prefixes outgoing interrupt reports with `0xA2` and calculates the DualSense Bluetooth CRC. |
| `external/DS5Dongle-OLED-Edition/BLUETOOTH_AUDIO_NOTES.md` | Documents the mic-enable discovery, sticky stream behavior, four-Hz arming keepalive, Opus decoding, jitter buffer, and tests with Discord/OBS. |
| `external/DS5_Bridge/src/main.cpp` and `src/audio.cpp` | Independently classifies `0x31` flag bit 1, accepts 71-byte packets, decodes mono Opus at 48 kHz/480 frames, and has loss concealment. Behavioral reference only because of AGPL-3.0. |
| `external/vds/module/vds_hcd_core.c` | Implements the virtual DualSense audio-IN endpoint and real-time ISO-IN completion pacing, but currently fills every input packet with silence. Its README explicitly states microphone input is unsupported. This is a useful virtual-UAC reference, not a physical microphone implementation. |
| `DS4Windows/DS4Library/InputDevices/DualSenseDevice.cs` | Current physical-controller path. Its Bluetooth input loop assumes only ordinary 78-byte `0x31` reports; it already uses `WriteOutputReportViaInterrupt` for experimental `0x32`/`0x35` reports. |

## Exact DS4Windows experiment before implementation

Do not wire the audio capture endpoint or add UI first. Build a narrowly scoped
diagnostic on a physical Bluetooth DualSense with verbose logging:

1. Log HID capabilities: input, output, and feature report byte lengths.
2. Submit one CRC-correct 398-byte, silent `0x36` microphone-enable report via
   `WriteOutputReportViaInterrupt`; log the returned Win32 error and exact byte
   count. Do not retry rapidly.
3. Add a pre-CRC classifier for `0x31` input reports. It must log only header,
   length, and the mic flag, not raw voice data.
4. If flagged frames arrive, copy exactly 71 bytes into a bounded queue and
   bypass normal controller-state parsing.
5. Decode the frames in a worker with Opus PLC/jitter buffering, then expose
   them only after a real virtual UAC capture endpoint exists.
6. On controller disconnect, profile change, and DS4Windows shutdown, send one
   best-effort mic-disable `0x36` report and dispose decoder/queues.

The report offset is transport-dependent: direct L2CAP sources see `A1 31 ...`;
Windows HidBth may strip the HIDP transaction prefix. The diagnostic must derive
the offset from the actual received buffer and never assume that raw L2CAP byte
positions map one-for-one onto `HidDevice.ReadFile` buffers.

## User-facing constraints

- Enabling controller mic should be explicit and opt-in. The documented stream
  keeps the controller audio subsystem active and costs battery.
- It must not be tied to Bluetooth speaker mirroring or advanced haptics.
- It should be disabled automatically when no app has opened the virtual audio
  capture endpoint, and immediately on DS4Windows shutdown.
- Audio should not be logged. Diagnostics should contain packet lengths, timing,
  counters, decoder results, and errors only.

## Relationship to virtual DualSense audio

VIIPER still needs a full composite UAC capture interface and reliable USB/IP
isochronous IN transfer handling before Windows can enumerate the virtual
DualSense microphone. vDS demonstrates the required device-side shape: its
virtual controller has audio-IN endpoint `0x82`, 48 kHz stereo S16_LE timing,
and completion pacing at audio cadence. Its present implementation zero-fills
the ISO-IN buffers, so it cannot be lifted wholesale. The physical Bluetooth
capture protocol above is independent of that virtual-device work. It is
valuable to validate the physical capture path first, because it isolates
HidBth compatibility from UAC/USBIP descriptor work.

## Attribution

Any implementation that ports the MIT-licensed DS5Dongle/OLED ideas should
retain their copyright and license notice. Cite `awalol/DS5Dongle` and the
OLED fork's Bluetooth audio notes in source attribution and release notes.

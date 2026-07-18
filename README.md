<img src="docs/viiper.svg" align="right" width="128" alt="VIIPER logo" />

# VIIPER

[![Build](https://github.com/hbashton/VIIPER/actions/workflows/snapshots.yml/badge.svg)](https://github.com/hbashton/VIIPER/actions)
[![Release](https://img.shields.io/github/v/release/hbashton/VIIPER?include_prereleases&sort=semver)](https://github.com/hbashton/VIIPER/releases)
[![License](https://img.shields.io/github/license/hbashton/VIIPER)](LICENSE.txt)

**Virtual Input over IP EmulatoR**

VIIPER is a userspace virtual USB device framework built on USBIP. This
hbashton fork is the backend used by the hbashton DS4Windows project for native
virtual controller output, including the ongoing DualSense audio, haptics, and
microphone work.

This repository is forked from [Alia5/VIIPER](https://github.com/Alia5/VIIPER).
The hbashton release channel contains the protocol and USB-audio changes needed
by [hbashton/DS4Windows](https://github.com/hbashton/DS4Windows). Install links
in this README download hbashton builds.

> **Windows releases from this fork are x64 only.** x86 Windows and x86
> DS4Windows builds are not compatible with VIIPER. Use 64-bit Windows and the
> x64 DS4Windows package.

## Install for DS4Windows

The simplest and recommended path is through a VIIPER-capable DS4Windows build:

1. Download the newest VIIPER pre-release from
   [hbashton/DS4Windows Releases](https://github.com/hbashton/DS4Windows/releases).
2. Open **DS4Windows > Settings**.
3. Under **VIIPER Virtual Controller Support**, click **Install / Repair VIIPER**.
4. Accept the administrator prompt and restart Windows if `usbip-win2` was installed or updated.
5. In a profile, choose a VIIPER output such as **DualSense (VIIPER)**.

DS4Windows installs VIIPER to `%LOCALAPPDATA%\VIIPER\viiper.exe`, installs the
required Windows USBIP driver when necessary, registers startup, and checks that
the local VIIPER API responds.

## Install VIIPER directly on Windows

Windows x64 users can run the hbashton installer from PowerShell:

```powershell
irm https://raw.githubusercontent.com/hbashton/VIIPER/main/scripts/install.ps1 | iex
```

The script:

1. Downloads the latest release from `hbashton/VIIPER`.
2. Accepts either the packaged Windows ZIP or the `viiper.exe` asset used by current releases.
3. Installs VIIPER to `%LOCALAPPDATA%\VIIPER\viiper.exe`.
4. Installs or updates `usbip-win2` when required.
5. Registers VIIPER for startup and starts the local server.

You can also download `viiper.exe` manually from the
[latest hbashton release](https://github.com/hbashton/VIIPER/releases/latest).
VIIPER itself is portable, but virtual devices on Windows still require the
[`usbip-win2`](https://github.com/vadimgrn/usbip-win2) kernel driver.

## What the hbashton fork adds

### DS4Windows controller backends

VIIPER can expose the following virtual USB devices for DS4Windows:

- Xbox 360 controller
- DualShock 4
- DualSense
- DualSense Edge
- Nintendo Switch 2 Pro Controller

The generic VIIPER keyboard and mouse devices remain available to other feeder
applications.

### Native DualSense input

The virtual DualSense and DualSense Edge paths carry:

- Face, shoulder, system, and mute buttons
- Sticks and analog triggers
- Touchpad click and two-finger coordinates
- Gyroscope and accelerometer reports
- DualSense Edge Fn buttons and back paddles
- Battery and controller metadata used by the USB identity

### Output reports and adaptive triggers

VIIPER returns host output to DS4Windows instead of reducing every command to
generic rumble. Extended DualSense streams preserve:

- The native USB HID output report `0x02`
- Lightbar, player LED, mute LED, and rumble state
- Native-spaced left and right adaptive-trigger effect blocks
- Optional Bluetooth haptics report `0x32`
- Combined Bluetooth state, haptics, and speaker report `0x36`

This lets DS4Windows forward game-authored adaptive-trigger commands and other
DualSense output to a compatible physical controller.

### Advanced haptics and speaker audio

The virtual DualSense includes the USB Audio Class interfaces expected by games.
The hbashton fork implements USBIP isochronous packet descriptors, completion
pacing, and audio-interface state so those endpoints can carry real data.

With the matching DS4Windows bridge:

- A game can open the virtual DualSense playback endpoint.
- DualSense haptics samples are converted into the Bluetooth haptics lane.
- Speaker samples can be forwarded to the physical controller speaker over Bluetooth.
- Haptics and speaker data share the combined Bluetooth report without one path starving the other.

### Microphone input

The microphone-capable DualSense device types expose a virtual Windows recording
endpoint. The framed feeder protocol accepts PCM microphone frames separately
from controller input state, and the USBIP ISO-IN path supplies them to Windows.

In the DS4Windows integration, microphone audio follows this path:

1. The physical Bluetooth DualSense supplies its encoded microphone frames.
2. DS4Windows decodes and conditions the signal.
3. DS4Windows sends framed PCM to VIIPER.
4. VIIPER presents that PCM through the virtual DualSense recording endpoint.

Transport framing and microphone data are deliberately isolated from HID input
reports. This prevents audio bytes from being interpreted as controller buttons,
keyboard commands, or mouse movement.

## Architecture

```text
Physical controller
        |
        | HID input, audio, and feedback
        v
DS4Windows feeder
        |
        | local framed TCP API
        v
VIIPER userspace USB device
        |
        | USBIP
        v
usbip-win2 virtual host controller
        |
        v
Windows, games, and audio services
```

VIIPER does not emulate a Bluetooth radio and does not make the virtual device
appear wirelessly paired. The game sees a native-style USB controller. DS4Windows
is responsible for translating and forwarding supported feedback between that
virtual USB device and the physical USB or Bluetooth controller.

## Requirements

### Windows

- Windows 10 or Windows 11 x64
- [`usbip-win2`](https://github.com/vadimgrn/usbip-win2)
- The hbashton VIIPER executable for the protocol used by your DS4Windows build
- Administrator approval for driver installation and startup registration

The current hbashton release channel prioritizes Windows x64 and DS4Windows.
The underlying VIIPER project remains cross-platform, but binaries and features
available from this fork may differ from upstream.

### Linux development

Linux uses the kernel USBIP client and `vhci-hcd` module. Package names vary by
distribution; common starting points are `linux-tools-generic` on Ubuntu and
`usbip` on Arch Linux.

## Server and API

The standalone `viiper` executable exposes a lightweight TCP API for bus and
device management. Management requests are null-terminated path/payload messages,
while active devices use persistent binary streams for low-latency input and
feedback.

Localhost feeder applications can create a bus, add a device, open its stream,
send input state, and receive output feedback. VIIPER handles USB descriptors,
USBIP requests, and device attachment.

See:

- [API overview](docs/api/overview.md)
- [DualSense protocol](docs/devices/dualsense.md)
- [DualShock 4 protocol](docs/devices/dualshock4.md)
- [Xbox 360 protocol](docs/devices/xbox360.md)
- [Switch 2 Pro protocol](docs/devices/ns2pro.md)
- [libVIIPER overview](docs/libviiper/overview.md)

## Build from source

### Prerequisites

- [Go](https://go.dev/) 1.26 or newer
- [just](https://github.com/casey/just), recommended
- USBIP support for the target operating system
- A C compiler only when building `libVIIPER`

### Build

```powershell
git clone https://github.com/hbashton/VIIPER.git
cd VIIPER
just build Release
```

The Windows executable is written to `dist/viiper-windows-amd64.exe`. Useful
development commands include:

```powershell
just test
go test ./...
go run ./cmd/viiper codegen
```

Client bindings are generated for TypeScript, C#, C++, and Rust. Run code
generation whenever a public device-state or feedback contract changes, then
build the client examples before publishing the change.

## Troubleshooting

- **DS4Windows says VIIPER is unavailable:** run **Install / Repair VIIPER** and
  restart Windows if `usbip-win2` was just installed.
- **No virtual controller appears:** confirm `viiper.exe server` is running and
  that the USBIP driver is installed.
- **Several stale controllers appear:** stop DS4Windows and VIIPER, start VIIPER
  once, then start DS4Windows. Report repeatable lifecycle bugs with both logs.
- **DualSense audio or microphone endpoints are missing:** use matching
  hbashton DS4Windows and VIIPER releases; older upstream VIIPER builds do not
  contain the same extended device types.
- **Input becomes corrupted while the microphone is active:** stop the test and
  report the DS4Windows log plus the VIIPER log. Microphone-capable streams must
  use the framed protocol and should never pass audio transport bytes into HID state.

Report backend issues at
[hbashton/VIIPER Issues](https://github.com/hbashton/VIIPER/issues). Report
controller mapping or DS4Windows UI issues at
[hbashton/DS4Windows Issues](https://github.com/hbashton/DS4Windows/issues).

## License

The VIIPER server and core are licensed under GPL-3.0-or-later. Generated client
libraries retain their documented MIT licensing. See [`LICENSE.txt`](LICENSE.txt)
and the individual client packages for details.

## Credits

VIIPER was created by Peter Repukat and the Alia5/VIIPER contributors. This fork
builds on that architecture for DS4Windows. It also depends on the USBIP project,
`usbip-win2`, and controller/audio protocol research shared by SDL, SAxense,
DualSense reverse-engineering projects, and the wider open-source community.

<img src="docs/viiper.svg" align="right" width="128" alt="VIIPER logo" />

# VIIPER

[![Build](https://github.com/hbashton/VIIPER/actions/workflows/snapshots.yml/badge.svg)](https://github.com/hbashton/VIIPER/actions)

**Virtual Input over IP EmulatoR** — a userspace virtual USB device framework
built on USB/IP, used as the controller backend for
[hbashton/DS4Windows](https://github.com/hbashton/DS4Windows).

This project builds on [Alia5/VIIPER](https://github.com/Alia5/VIIPER).

## Use with DS4Windows

Download the matching x64 package from
[DS4Windows Releases](https://github.com/hbashton/DS4Windows/releases).
The Setup EXE includes VIIPER and the pinned USB/IP driver. Portable packages
include their matching backend; extract the complete archive. Use
**Settings > Install / Repair VIIPER** if the app requests repair, and restart
Windows when setup asks. Do not independently substitute another backend or driver.

Windows controller releases require Windows 10 or 11 x64. Consult DS4Windows release notes for signing status and package requirements.

## Features

- Virtual Xbox 360, Xbox One/Series, DualShock 4, DualSense/Edge and Switch 2 Pro controller paths.
- Native controller feedback, adaptive triggers, audio and microphone transport
  where supported by the device, matching feeder and physical connection.
- Generic keyboard and mouse devices for feeder applications.
- TCP management and binary device streams, plus generated client libraries.

VIIPER presents USB devices; DS4Windows maps and forwards physical-controller
input and feedback. Virtual emulation does not guarantee every game or hardware feature.

## Develop

See [Documentation](docs/index.md) for installation, API, device protocols,
build instructions and historical validation. [CHANGELOG.md](CHANGELOG.md)
contains release history. Standalone deployment is a developer path and must
not compete with DS4Windows-managed startup and ownership.

Report backend bugs in [backend issues](https://github.com/hbashton/VIIPER/issues).
Report app UI or mapping bugs in [DS4Windows issues](https://github.com/hbashton/DS4Windows/issues).

The server/core are GPL-3.0-or-later; generated client libraries retain their
documented MIT licensing. See [LICENSE.txt](LICENSE.txt) and component notices.
Credits include Peter Repukat, Alia5 contributors, hbashton, USB/IP, usbip-win2,
and the controller/audio research community.

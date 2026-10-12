# VIIPER documentation

VIIPER is the USB/IP backend used by
[hbashton/DS4Windows](https://github.com/hbashton/DS4Windows).
This project continues hbashton's integration and the original Alia5 framework.

## DS4Windows users

Use the [matching DS4Windows package](https://github.com/hbashton/DS4Windows/releases)
and its setup/repair controls. See [Installation](getting-started/installation.md)
and [Startup diagnostics](startup-diagnostics.md). The app owns backend launch,
credentials and compatibility checks. Portable packages keep the matching
backend beside the app; installed copies use the managed installation.

## Feeder and backend developers

| Reference | Contents |
| --- | --- |
| [Quick start](getting-started/quickstart.md) | Standalone server and client examples |
| [Development](development.md) | Source build, tests, code generation and documentation |
| [CLI](cli/overview.md) | Server, proxy, codegen and configuration |
| [API](api/overview.md) | Bus/device management and streams |
| [Clients](clients/generator.md) | Go, C++, C#, Rust and TypeScript bindings |
| [libVIIPER](libviiper/overview.md) | Embedding device emulation |
| [Devices](devices/overview.md) | Device protocols and limits |
| [USB/IP](getting-started/usbip.md) | Driver/client prerequisites |
| [Technical records](reference.md) | Scheduling, architecture and dated validation |
| [Support](misc/support.md) | Where to report bugs and request help |

The standalone TCP server provides management requests and per-device binary
streams. DS4Windows supplies physical input and forwards compatible feedback;
VIIPER presents virtual USB devices to the operating system. Remote connections
use the implemented authentication/encryption contract; consult the API and
configuration reference before deploying beyond localhost.

Release history is maintained in
[CHANGELOG.md](https://github.com/hbashton/VIIPER/blob/main/CHANGELOG.md).
Dated records preserve evidence for their recorded source and hardware. They
are not current compatibility promises or instructions to install an old build.

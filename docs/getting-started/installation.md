# Installation

[Documentation index](../index.md) · [USB/IP prerequisites](usbip.md)

## DS4Windows on Windows x64

Use [hbashton/DS4Windows Releases](https://github.com/hbashton/DS4Windows/releases)
and follow [the app setup guide](https://github.com/hbashton/DS4Windows/blob/main/docs/getting-started.md).
The Setup EXE includes the matching backend and driver; portable users extract
the whole package and retain its backend files. Use Settings > Install / Repair
VIIPER when requested. Restart Windows if setup asks.

Installed copies use `%ProgramFiles%\DS4Windows\VIIPER`; portable copies keep
their backend beside DS4Windows. The app verifies the expected executable,
USB/IP files, ABI and API readiness. Avoid a separate standalone service that
competes with this ownership.

The current upstream controller package pins usbip-win2 **0.9.7.7 x64**. Update the app, backend and driver together when a new compatible package is released. Do not replace it with another
userspace/driver version. See [USB/IP](usbip.md) for the prerequisite details
and [startup diagnostics](../startup-diagnostics.md) for failed checks.
Consult the app release notes for signing status; driver
signing and app signing are separate properties.

## Standalone development

The VIIPER server is a userspace executable, but Windows virtual attachment
still needs the matching kernel driver. Check
[this project's releases](https://github.com/hbashton/VIIPER/releases) for actual
available assets, or [build from source](#building-from-source).
Do not assume every operating system/architecture has a published binary.

The legacy Windows install script is gated for deliberate standalone developer
use. Inspect `scripts/install.ps1` in your checkout before using it; it requires
`VIIPER_DEVELOPER_STANDALONE=1` and `-DeveloperStandalone`. It is not a supported
DS4Windows installation path. Ordinary users should use the app's setup/repair
controls. This checkout has no maintained `scripts/install.sh`; the former
download-and-execute instructions for that file are obsolete.

For Linux development, use the distribution's USB/IP client and `vhci-hcd`.
See [USB/IP](usbip.md); package names, permissions and kernel availability depend
on the distribution. Run the server using [CLI configuration](../cli/configuration.md).

## Building from source

Use the Go version required by `go.mod` (currently **1.26.2** or a compatible
newer toolchain), and optionally `just`:

```powershell
git clone https://github.com/hbashton/VIIPER.git
cd VIIPER
just build Release
```

By default, the executable is `dist/viiper.exe` on Windows or `dist/viiper` on
Linux; cross-build recipes can choose other output names. See
[Development](../development.md) for testing and generated clients.

## libVIIPER

libVIIPER embeds emulation in a shared library rather than a separate server.
Build it with `just build-libVIIPER Release`; this requires CGO and a compatible
C toolchain. Output is under `dist/libVIIPER/`. Do not assume the fork release
contains prebuilt shared libraries; inspect its actual assets.

The core/library licensing differs from generated TCP clients. See
[libVIIPER](../libviiper/overview.md) and the repository licenses before embedding.

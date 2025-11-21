# Installation

## Requirements

VIIPER relies on USBIP. You must have USBIP installed on your system.

### Linux

#### Ubuntu/Debian

```bash
sudo apt install linux-tools-generic
```

[Ubuntu USBIP Manual](https://manpages.ubuntu.com/manpages/noble/man8/usbip.8.html)

#### Arch Linux

```bash
sudo pacman -S usbip
```

[Arch Wiki: USBIP](https://wiki.archlinux.org/title/USB/IP)

### Windows

[usbip-win2](https://github.com/vadimgrn/usbip-win2) is by far the most complete implementation of USBIP for Windows (comes with a **SIGNED** kernel mode driver).

### Linux Kernel Module Setup (for Auto-Attach)

!!! info "Auto-Attach Feature"
    VIIPER can automatically attach created devices to the local machine (localhost) when enabled. This requires the `vhci-hcd` kernel module on Linux.

The `vhci-hcd` (Virtual Host Controller Interface) module is required for the auto-attach feature. Most Linux distributions include this module but don't load it automatically.

#### One-Time Setup

To load the module automatically on boot:

```bash
echo "vhci-hcd" | sudo tee /etc/modules-load.d/vhci-hcd.conf
sudo modprobe vhci-hcd
```

#### Manual Loading

To load the module for the current session only:

```bash
sudo modprobe vhci-hcd
```

#### Verification

Check if the module is loaded:

```bash
lsmod | grep vhci_hcd
```

If you don't plan to use the auto-attach feature, you can skip this setup and disable it with `--api.auto-attach-local-client=false`.

## Installing VIIPER

### Pre-built Binaries (Recommended)

Download the latest release from the [GitHub Releases](https://github.com/Alia5/VIIPER/releases) page. Pre-built binaries are available for:

- Windows (x64, ARM64)
- Linux (x64, ARM64)

### Portable Deployment

VIIPER does not require system-wide installation.  
The `viiper` executable is completely self-contained (and statically linked) and can be:

- Placed in any directory
- Shipped alongside your application
- Run directly without installation
- Bundled with your application's distribution

This makes VIIPER ideal for embedding in applications or distributing as part of a software package.

!!! warning "Daemon/Service Conflicts"
    If VIIPER is already running as a system service or daemon on the target machine, be aware of potential port conflicts. Applications should either:
    
    - Connect to the existing VIIPER instance (if accessible)
    - Use a custom port via `--api.addr` flag to run a separate instance
    - Check if VIIPER is already running before starting their own instance

### Building from Source

Building from source is only necessary if you need to modify VIIPER or target an unsupported platform.

#### Prerequisites

- [Go](https://go.dev/) 1.25 or newer
- USBIP installed
- (Optional) [Make](https://www.gnu.org/software/make/)
    - Linux/macOS: Usually pre-installed
    - Windows: `winget install ezwinports.make`

#### Build Steps

```bash
git clone https://github.com/Alia5/VIIPER.git
cd VIIPER
make build
```

The compiled binary will be in `dist/viiper` (or `dist/viiper.exe` on Windows).

**Additional build targets:**

```bash
make help          # Show all available make targets
make test          # Run tests
```

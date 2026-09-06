# Server Command

Start the VIIPER daemon/server to expose virtual devices.  
This is the default command you should run when you want to create virtual USB devices using VIIPER.

## Usage

```bash
viiper server [OPTIONS]
```

## Description

The `server` command starts the VIIPER USBIP server, which allows you to create and manage virtual USB devices that appear as real hardware to USBIP clients.

The server exposes two interfaces:

1. **USBIP Server** - Standard USBIP protocol for device attachment
2. **VIIPER API Server** - Management API for device/bus control

!!! warning "Authentication Required for Remote Connections"
    VIIPER requires **authentication for all remote (non-localhost) connections** to prevent unauthorized device creation.  

    On first start, VIIPER generates a random password
    and saves it to `<USER_CONFIG_DIR>/viiper.key.txt`.  
    Windows: `%APPDATA%\VIIPER\viiper.key.txt`  
    Linux (user): `~/.config/github.com/Alia5/viiper/viiper.key.txt`  
    Linux (root/systemd): `/etc/viiper/viiper.key.txt`
    
    - **Localhost clients** (`127.0.0.1`, `::1`): Authentication is optional by default
    - **Remote clients**: Authentication is required and enforced
    - All authenticated connections use **ChaCha20-Poly1305 encryption**
    
    See the `--api.require-local-host-auth` option below to require authentication for localhost connections.

!!! info "Automatic Local Attachment"
    By default, VIIPER automatically attaches newly created devices to the local USBIP client (localhost only).  
    This means when you create a device via the API, it will be immediately available on the same machine without manual `usbip attach` commands.  
    This behavior can be disabled with `--api.auto-attach-local-client=false` if you prefer manual control or are running on a remote server.

## Options

### `--key-file`

Explicit absolute path to the API password file. Omitting this option retains
the platform-specific default listed above. An explicit empty or relative path,
nonregular file, symbolic link/reparse point, unreadable file, or empty password
fails startup without falling back to the default location. Missing parent
directories are created with mode `0700`; a missing key is generated with the
existing cryptographic generator and exclusively created with mode `0600`.
Windows retains its normal inherited ACL behavior. Existing key bytes and
permissions are not changed, and a generated explicit key is not printed to logs.

Clients must read the same file. This option changes neither authentication nor
encryption, and it does not make the rest of the server portable or isolated.
There is deliberately no environment-variable alias for this explicit override.

Example for a separately managed local lab (PowerShell):

```powershell
.\viiper.exe --config-only --config 'C:\Lab\server.json' --update-notify=none --log.file='' --log.raw-file='' server --key-file 'C:\Lab\lab-data\viiper.key.txt' --usb.addr=127.0.0.1:3241 --api.addr=127.0.0.1:3242 --api.require-local-host-auth=true
```

`--update-notify=none` disables update checks and their shared dismissal-state
file. `--config-only` requires exactly one explicit absolute `--config` file and
never loads working-directory, user, or system fallback files. Missing,
unreadable, or malformed files fail before logger/server startup. Without
`--config-only`, the existing multi-location discovery behavior is unchanged.
Environment variables still apply; explicitly override settings needed by the
lab. Empty log-file flags prevent inherited file-log destinations.
This command does not install drivers or change startup registration. Do not use
the tray's startup-registration action for an isolated run.

### `--usb.addr`

USBIP server listen address.

**Default:** `127.0.0.1:3241`
**Environment Variable:** `VIIPER_USB_ADDR`

USB/IP has no authentication in this server. Setting a non-loopback address is
an explicit remote-exposure choice and must be protected by a trusted tunnel,
host firewall, or an equivalent authenticated network boundary. API
authentication does not authenticate the separate USB/IP port.

### `--api.addr`

API server listen address.

**Default:** `:3242`  
**Environment Variable:** `VIIPER_API_ADDR`

### `--api.device-handler-timeout`

Time before auto-cleanup occurs when a device handler has no active connection.

**Default:** `5s`  
**Environment Variable:** `VIIPER_API_DEVICE_HANDLER_TIMEOUT`

### `--api.auto-attach-local-client`

Automatically attach newly added devices to a local USBIP client on the same host (localhost only). This is a convenience feature; attachment failures (tool not found, error exit) are logged but do not abort device creation.

VIIPER expects the USBIP command-line tool to be in the PATH (should be by default) (`usbip` on Linux, `usbip.exe` on Windows). If it is missing, auto-attach will simply log an error.

**Default:** `true`  
**Environment Variable:** `VIIPER_API_AUTO_ATTACH_LOCAL_CLIENT`

Disable example:

```bash
viiper server --api.auto-attach-local-client=false
```

### `--api.require-local-host-auth`

Require authentication even for clients connecting from localhost (`127.0.0.1`, `::1`, `localhost`).

By default, localhost clients are exempt from authentication for convenience during local development.  
Enable this option if you want to enforce authentication for all connections regardless of origin.

**Default:** `false`  
**Environment Variable:** `VIIPER_API_REQUIRE_LOCALHOST_AUTH`

Enable example:

```bash
viiper server --api.require-local-host-auth=true
```

### `--connection-timeout`

Connection operation timeout for both USBIP and API servers.

**Default:** `30s`  
**Environment Variable:** `VIIPER_CONNECTION_TIMEOUT`

## Examples

### Basic Server

Start server with default settings (USBIP on `127.0.0.1:3241`, API on
`:3242`):

```bash
viiper server
```

### Custom Addresses

Start server on custom ports:

```bash
viiper server --usb.addr=127.0.0.1:9000 --api.addr=:9001
```

### With Logging

Start server with debug logging to file:

```bash
viiper server --log.level=debug --log.file=/var/log/viiper.log
```

### With Raw Packet Logging

Start server with raw USB packet logging (useful for reverse engineering):

```bash
viiper server --log.raw-file=/var/log/viiper-raw.log
```

## Connect from a client (USBIP)

After the server is running and a virtual device has been added to a bus (via the API), attach it from a client using USBIP.

Notes:

- VIIPER's USBIP server listens on `127.0.0.1:3241` by default (configurable via `--usb.addr`).
- A remote client requires an explicitly configured non-loopback address and
  a trusted tunnel/firewall boundary; the USB/IP protocol is not authenticated.
- The BUSID-DEVICEID you need (e.g. `1-1`) is returned by the API on device add and also visible via `usbip list`.

=== "Windows"

    On Windows, use [usbip-win2](https://github.com/vadimgrn/usbip-win2):

    - GUI: use the client to add a remote host and attach by busid.
    - CLI (similar flags):

    ```powershell
    usbip.exe list --remote VIIPER_HOST --tcp-port 3241
    usbip.exe attach --remote VIIPER_HOST --tcp-port 3241 --busid BUSID-DEVICEID
    ```

=== "Linux"

    ```bash
    # Load the virtual host controller (only needed once per boot)
    sudo modprobe vhci-hcd

    # List exportable devices on the VIIPER host
    usbip list --remote=VIIPER_HOST --tcp-port=3241

    # Attach a device by busid (long flags)
    sudo usbip attach --remote=VIIPER_HOST --tcp-port=3241 --busid=BUSID-DEVICEID

    # Equivalent short-form flags
    sudo usbip --tcp-port 3241 -r VIIPER_HOST -b BUSID-DEVICEID
    ```

    Replace `VIIPER_HOST` with the server's hostname/IP. If you changed the USBIP port, use that port instead of `3241`.

Once attached, the device will appear to the OS/applications as a local USB device.

## See Also

- [Configuration](configuration.md) - Environment variables and configuration files
- [API Reference](../api/overview.md) - API server documentation

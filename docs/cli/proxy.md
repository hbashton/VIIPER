# Proxy Command

Start the VIIPER USBIP proxy for traffic inspection and logging.

## Usage

```bash
viiper proxy --upstream=<address> [OPTIONS]
```

## Description

The `proxy` command starts VIIPER in proxy mode, sitting between a USBIP client and a USBIP server.  
VIIPER intercepts and logs all USB traffic passing through, without handling the devices directly.

This is useful for reverse engineering USB protocols and understanding how devices communicate.

## Options

### `--listen-addr`

Proxy listen address (where clients connect).

**Default:** `:3241`  
**Environment Variable:** `VIIPER_PROXY_ADDR`

**Example:**

```bash
viiper proxy --listen-addr=:9000 --upstream=192.168.1.100:3240
```

### `--upstream`

**Required.** Upstream USBIP server address (where real devices are).

**Environment Variable:** `VIIPER_PROXY_UPSTREAM`

**Example:**

```bash
viiper proxy --upstream=192.168.1.100:3240
```

### `--connection-timeout`

Connection timeout for proxy operations.

**Default:** `30s`  
**Environment Variable:** `VIIPER_PROXY_TIMEOUT`

**Example:**

```bash
viiper proxy --upstream=192.168.1.100:3240 --connection-timeout=60s
```

## Examples

### Basic Proxy

Start proxy between local clients and remote USBIP server:

```bash
viiper proxy --upstream=192.168.1.100:3240
```

Clients connect to `localhost:3241`, traffic is proxied to `192.168.1.100:3240`.

### Custom Listen Address

Start proxy on a different port:

```bash
viiper proxy --listen-addr=:9000 --upstream=192.168.1.100:3240
```

Clients connect to `localhost:9000`, traffic is proxied to `192.168.1.100:3240`.

### With Raw Packet Logging

Capture all USB traffic for reverse engineering:

```bash
viiper proxy --upstream=192.168.1.100:3240 --log.raw-file=usb-capture.log
```

All USB packets will be logged to `usb-capture.log`.

### With Debug Logging

Enable debug logging to see proxy operations:

```bash
viiper proxy --upstream=192.168.1.100:3240 --log.level=debug
```

## Use Cases

### Reverse Engineering

Intercept USB traffic between a client and server to understand device protocols:

```bash
viiper proxy --upstream=real-server:3240 --log.raw-file=device-capture.log
```

### Traffic Analysis

Monitor USB communication for debugging:

```bash
viiper proxy --upstream=real-server:3240 --log.level=trace
```

### Network Inspection

Route USB traffic through VIIPER to inspect and log all operations:

```bash
viiper proxy --upstream=real-server:3240 --log.level=debug --log.raw-file=traffic.log
```

## Proxy Architecture

```
USBIP Client  →  VIIPER Proxy  →  USBIP Server (real devices)
                      ↓
              Logs/Captures Traffic
```

VIIPER sits in the middle, forwarding all traffic while logging packets for inspection.

## See Also

- [Server Command](server.md) - Run VIIPER as a USBIP server
- [Configuration](configuration.md) - Environment variables and configuration files

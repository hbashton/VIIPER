# DualShock 4 Controller

The DualShock 4 virtual gamepad emulates a complete PlayStation 4 Controller (V1) connected via USB.  
It supports sticks, triggers, D-pad, face/shoulder buttons, PS button, touchpad click, IMU (gyro + accelerometer), and touchpad finger coordinates.

- USB IDs: VID `0x054C` (Sony), PID `0x05C4` (DualShock 4, v1)
- Device type id (for API add): `dualshock4`

## Client Library Support

The wire protocol is abstracted by client libraries. The **Go client** includes built-in types (`/device/dualshock4`), and **generated client libraries** provide equivalent structures with proper packing.  
You don't need to manually construct packets, just use the provided types and send them via the device stream.

See: [Go Client](../clients/go.md), [Generated Client Libraries](../clients/generator.md)

## Adding the device

Using the raw API (see [API Reference](../api/overview.md) for details):

```bash
# Create a bus
printf "bus/create\0" | nc localhost 3242

# Add DualShock 4 device with JSON payload
printf 'bus/1/add {"type":"dualshock4"}\0' | nc localhost 3242
```

Or use one of the [client libraries](../clients/generator.md) which handle the protocol automatically.

## Device stream protocol

The DS4 device stream is a bidirectional TCP stream with **fixed-size** packets.

### Device Input

- 31-byte packets (little-endian):
  - `stickLX: i8, stickLY: i8, stickRX: i8, stickRY: i8`
  - `buttons: u16`
  - `dpad: u8`
  - `triggerL2: u8, triggerR2: u8`
  - `touch1X: u16, touch1Y: u16, touch1Active: bool`
  - `touch2X: u16, touch2Y: u16, touch2Active: bool`
  - `gyroX: i16, gyroY: i16, gyroZ: i16`
  - `accelX: i16, accelY: i16, accelZ: i16`

See `/device/dualshock4/inputstate.go`

#### Touchpad coordinates

Touch coordinates are sent as `touch{1,2}X: u16` and `touch{1,2}Y: u16` plus an explicit boolean `touch{1,2}Active`.

VIIPER clamps touch coordinates to the DS4 range:

- X: **0..1920**
- Y: **0..942**

These are the bounds used by VIIPER’s DS4 implementation; see `/device/dualshock4/const.go`.

#### Gyro + accelerometer fixed-point units

VIIPER uses **fixed-point physical units** for IMU values on the wire (still stored as `int16`), to avoid float serialization differences across client languages.

Constants (see `/device/dualshock4/const.go`):

- `GyroCountsPerDps = 16`
- `AccelCountsPerMS2 = 512`

##### Formulas

Gyro (degrees/second):

```text
raw_gyro = round(gyro_dps * GyroCountsPerDps)
gyro_dps = raw_gyro / GyroCountsPerDps
```

Accelerometer (m/s²):

```text
raw_accel = round(accel_ms2 * AccelCountsPerMS2)
accel_ms2 = raw_accel / AccelCountsPerMS2
```

#### Resolution and range

With the default scales:

- Gyro (`GyroCountsPerDps = 16`):
  - Resolution: `1/16 = 0.0625 °/s`
  - Approx max magnitude: `32767/16 ≈ 2048 °/s`
- Accelerometer (`AccelCountsPerMS2 = 512`):
  - Resolution: `1/512 ≈ 0.001953125 m/s²`
  - Approx max magnitude: `32767/512 ≈ 64 m/s²` (≈ 6.5 g)

Conversions saturate to the `int16` range if inputs exceed representable values.

#### Default (neutral) report gravity

On device creation, VIIPER initializes the accelerometer to represent a controller lying flat on a table, with gravity "downwards":

- `g = 9.81 m/s²`
- Default accel is: `(0, 0, -g)`

In raw fixed-point units, this means:

- `AccelX = 0`
- `AccelY = 0`
- `AccelZ = round(-9.81 * 512) = -5023`

Helpers for converting between physical units and raw values are provided in `/device/dualshock4/helpers.go`.

## Device Output

The host sends rumble/LED updates which VIIPER parses and forwards to your client as a compact feedback packet.

Direction: server → client (feedback)

- 7-byte packets:
  - `RumbleSmall: u8`
  - `RumbleLarge: u8`
  - `LedRed: u8`
  - `LedGreen: u8`
  - `LedBlue: u8`
  - `FlashOn: u8` (units of 2.5ms)
  - `FlashOff: u8` (units of 2.5ms)

See `/device/dualshock4/inputstate.go` for the `OutputState` wire definition.

## Examples

A minimal example that drives a DS4 device is provided in `examples/go/virtual_ds4/`.

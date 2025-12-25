# Steam Deck Controller (Jupiter)

Steam Deck (Jupiter/LCD) virtual controller.

- Device type id (for API add): `steamdeck`

## Client library support

All supported client libraries generate strongly-typed structs/classes for the Steam Deck wire protocol from the `viiper:wire` annotations in `/device/steamdeck/deviceState.go`.

## Device stream protocol (client-facing)

The device stream is a bidirectional, raw TCP connection.

### Client → server (input)

- Fixed **52-byte** packet, little-endian.
- Fields (in order):
  - `buttons` (u64)
  - `leftPadX`, `leftPadY`, `rightPadX`, `rightPadY` (i16)
  - `accelX`, `accelY`, `accelZ` (i16)
  - `gyroX`, `gyroY`, `gyroZ` (i16)
  - `gyroQuatW`, `gyroQuatX`, `gyroQuatY`, `gyroQuatZ` (i16)
  - `triggerRawL`, `triggerRawR` (u16)
  - `leftStickX`, `leftStickY`, `rightStickX`, `rightStickY` (i16)
  - `pressurePadLeft`, `pressurePadRight` (u16)

### Server → client (haptics)

- Fixed **4-byte** packet, little-endian.
- Fields:
  - `leftMotor` (u16)
  - `rightMotor` (u16)

See: `/device/steamdeck/deviceState.go` and `/device/steamdeck/handler.go`.

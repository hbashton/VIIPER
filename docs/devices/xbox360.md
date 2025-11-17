# Xbox 360 Controller

The Xbox 360 virtual gamepad emulates an XInput-compatible controller that most operating systems and games understand out of the box.

- USB IDs: VID 0x045E (Microsoft), PID 0x028E (Xbox 360 Controller)
- Interfaces/Endpoints: single HID interface with one IN interrupt endpoint and one OUT interrupt endpoint for rumble
- Device type id (for API add): `xbox360`

## Client SDK Support

The wire protocol is abstracted by client SDKs. The **Go client** includes built-in types (`pkg/device/xbox360`), and **generated SDKs** provide equivalent structures with proper packing.  
You don't need to manually construct packets, just use the provided types and send them via the device stream.

See: [Go Client](../clients/go.md), [Generated SDKs](../clients/generator.md)

## Adding the device

Use the API to create a bus and add an Xbox 360 controller:

```text
bus/create
bus/1/add xbox360
```

The API returns a `busid` like `1-1`. Attach it from a USB/IP client, then open a stream to drive input and receive rumble.

## Streaming protocol

The device stream is a bidirectional, raw TCP connection with fixed-size packets.

Direction: client → server (input state)

- 14-byte packets, little-endian layout:
  - Buttons: uint32 (4 bytes)
  - LT, RT: uint8, uint8 (2 bytes)
  - LX, LY, RX, RY: int16 each (8 bytes)

Direction: server → client (rumble feedback)

- 2-byte packets:
  - LeftMotor: uint8, RightMotor: uint8

See `pkg/device/xbox360/inputstate.go` for details.

## Example

A minimal example program that sends input and reads rumble is provided in `examples/`.

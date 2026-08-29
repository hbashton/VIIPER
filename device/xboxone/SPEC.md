# Xbox One pre-capture semantic and body-codec contract

Status: pre-capture, backend-independent, no device registration.

This package is deliberately smaller than an Xbox One virtual device. It
defines the pieces that can be implemented without choosing USB descriptors,
claiming Windows compatibility, or inventing authentication and GIP lifecycle
messages.

## InputStateV1

`InputStateV1` is versioned at the Go type boundary. Its zero value is neutral.
It contains named base controls, 16-bit triggers, signed 16-bit sticks, and
explicit `Guide` and `Share` values.

Opposite directions on one D-pad axis are invalid. `Guide` and `Share` are
valid semantic inputs, but neither can be encoded into the 14-byte base body:

- Guide must be emitted separately as a virtual-key body.
- Share requires a device-capability-dependent input extension that is not
  implemented before an owned capture.

The base input body is exactly 14 bytes, little-endian:

| Offset | Width | Meaning |
| ---: | ---: | --- |
| 0 | 2 | buttons; mapped bits 2 through 15 |
| 2 | 2 | left trigger |
| 4 | 2 | right trigger |
| 6 | 2 | left stick X, signed bit pattern |
| 8 | 2 | left stick Y, signed bit pattern |
| 10 | 2 | right stick X, signed bit pattern |
| 12 | 2 | right stick Y, signed bit pattern |

Button bits 0 and 1 are rejected as reserved. All encoders and decoders require
the exact body size; a larger slice is not silently truncated.

Guide uses a separate exact two-byte virtual-key body: byte 0 is strict Boolean
down state and byte 1 is `0x5b`. The enclosing command value is `0x07`, but this
package does not create that GIP frame.

## RumbleBodyV1

The rumble body is exactly nine bytes:

| Offset | Width | Meaning |
| ---: | ---: | --- |
| 0 | 1 | unknown/reserved; must be zero |
| 1 | 1 | actuator enable mask |
| 2 | 1 | left-trigger magnitude |
| 3 | 1 | right-trigger magnitude |
| 4 | 1 | low-frequency/left-body magnitude |
| 5 | 1 | high-frequency/right-body magnitude |
| 6 | 1 | duration |
| 7 | 1 | delay |
| 8 | 1 | repeat |

Enable bits are high-frequency/right body `0x01`, low-frequency/left body
`0x02`, right trigger `0x04`, and left trigger `0x08`. Unknown mask bits are
rejected. VIIPER also rejects a non-zero magnitude on a disabled channel. That
no-stale-magnitude rule is a local safety invariant, not an assertion that the
wire protocol forbids ignored bytes.

`NewPinnedStopRumbleBody` selects all four actuators with zero magnitudes and
uses the pinned startup-stop timing bytes (`duration=0xff`, `repeat=0xeb`). A
zero mask alone is intentionally not described as a stop command.

No magnitude normalization or clamping is performed. The captured host/device
range remains unknown, so all magnitude and timing fields retain their exact
byte values.

## Session boundary

`SequenceCounter` emits 1 through 255 and wraps to 1. It is an egress allocator,
not a replay detector. No receive replay window is implemented.

`AuthProvider` accepts and produces opaque authentication bodies. The default
provider returns `ErrAuthenticationUnavailable`; a zero-value `Session` does
the same. The package does not define keys, certificates, handshake messages,
or success policy. It only bounds-checks the provider's reported output.

## Deliberately absent

- USB descriptors, identity, endpoints, configuration, or registration
- GIP header/varint/chunk framing or reassembly
- announce, identify, power, acknowledgement, or lifecycle payloads
- authentication implementation or borrowed device credentials
- Share extension encoding
- a receive sequence/replay policy
- USB/IP, UDE, UMDF, XInputHID, or XUSB backend coupling

See [PROVENANCE.md](PROVENANCE.md) for the pinned evidence and the reasons these
boundaries remain closed.

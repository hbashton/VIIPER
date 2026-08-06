# Xbox Series X|S Controller

The `xboxseries` virtual gamepad presents the native wired Xbox Series USB GIP
identity (`VID 045E`, `PID 0B12`, compatible ID `XGIP10`). Windows therefore
binds its Xbox controller stack rather than treating the device as a generic
HID gamepad.

## Input stream

The client-to-server packet is 20 bytes and retains the Xbox 360 packet layout
used by VIIPER clients:

- buttons: 32-bit little-endian mask;
- left and right triggers: one byte each;
- four signed 16-bit stick axes; and
- six reserved bytes.

Bit `0x00010000` is the Series Share button. VIIPER publishes Share through
GIP's 18-byte Console Function Map extension. Guide is emitted through the
dedicated GIP Guide command. Input packets are change-driven and Windows polls
the native full-speed interrupt endpoint at its documented 4 ms interval.

## Output stream

The server-to-client feedback packet is seven bytes:

| Offset | Meaning |
|---:|---|
| 0 | Left body vibration, normalized 0–255 |
| 1 | Right body vibration, normalized 0–255 |
| 2 | Left impulse trigger, normalized 0–255 |
| 3 | Right impulse trigger, normalized 0–255 |
| 4 | Duration in native 10 ms units |
| 5 | Delay in native 10 ms units |
| 6 | Repeat count |

VIIPER preserves the four independent motors and implements the GIP timing
contract. A newer motor command atomically supersedes an older command, and a
zero-duration command cancels all motors. Delay-free repeats are coalesced into
one continuous interval so the virtual controller does not invent vibration
gaps.

DS4Windows maps the body motors to ordinary physical-controller vibration. On
a physical DualSense or DualSense Edge, accurate-rumble mode drives the haptic
actuators and the two impulse channels independently drive L2 and R2 adaptive
trigger vibration. Controllers without adaptive triggers fold each impulse
channel into its corresponding ordinary rumble channel.

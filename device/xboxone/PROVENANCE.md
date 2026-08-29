# Xbox One body-codec provenance and uncertainty ledger

Audit date: 2026-08-29.

## Clean-room treatment

The implementation in this directory was written as an original Go API from
documented byte-level observations. No xone, PadForge, HIDMaestro, or other
project source text, control flow, data structure, or API was copied. The xone
files below were used only as pinned evidence of protocol facts. Tests use
independently written, minimal byte vectors.

The pinned xone files carry `SPDX-License-Identifier: GPL-2.0-or-later`, and the
[pinned repository license](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/LICENSE)
is GPLv2. VIIPER is distributed under GPL-3.0-or-later in its root
`LICENSE.txt`. This provenance record does not relicense upstream expression or
claim that protocol facts are upstream code.

PadForge and HIDMaestro were not inputs to these codecs. In particular, no
bundled binary was reverse engineered or treated as a wire oracle.

## Pinned sources

- xone commit
  [`3484f603484782dd7551c64e5a33fc602b127051`](https://github.com/medusalix/xone/commit/3484f603484782dd7551c64e5a33fc602b127051)
- xone
  [`driver/gamepad.c` lines 30-79](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/driver/gamepad.c#L30-L79)
  for button masks, the 14-byte base input body, motor mask bits, and the
  nine-byte rumble body
- xone
  [`driver/gamepad.c` lines 145-158](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/driver/gamepad.c#L145-L158)
  for the observed all-motor startup stop and its `0xff` duration / `0xeb`
  repeat bytes
- xone
  [`driver/gamepad.c` lines 254-300](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/driver/gamepad.c#L254-L300)
  for base input parsing and the capability-dependent Share offset
- xone
  [`bus/protocol.c` lines 24-46](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/bus/protocol.c#L24-L46)
  for virtual-key `0x5b`, virtual-key command `0x07`, and option bits
- xone
  [`bus/protocol.c` lines 1187-1207](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/bus/protocol.c#L1187-L1207)
  for the exact two-byte virtual-key body and separate Guide dispatch
- xone
  [`bus/protocol.c` lines 187-291](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/bus/protocol.c#L187-L291)
  for the observed varint/header encoder and decoder
- xone
  [`bus/protocol.c` lines 293-346](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/bus/protocol.c#L293-L346)
  for non-zero transmit sequences
- xone
  [`auth/auth.c`](https://github.com/medusalix/xone/blob/3484f603484782dd7551c64e5a33fc602b127051/auth/auth.c)
  as evidence that authentication is a stateful protocol boundary, not a
  constant announce or descriptor payload
- Microsoft
  [`GameInputRumbleParams`](https://learn.microsoft.com/en-us/gaming/gdk/docs/reference/input/gameinput/structs/gameinputrumbleparams)
  for the public Windows semantic names low-frequency, high-frequency,
  left-trigger, and right-trigger

## Facts implemented

| Fact | Implementation |
| --- | --- |
| Base input body has seven little-endian 16-bit fields | `input_body.go` exact 14-byte codec |
| Base buttons occupy bits 2 through 15 in the pinned map | named Boolean mapping; bits 0-1 rejected |
| Guide is a separate two-byte virtual-key body with key `0x5b` | strict Guide body codec; base encoder rejects Guide |
| Share is not in the base body and depends on reported interfaces/extensions | explicit semantic field; base encoder rejects Share |
| Rumble body has one unknown byte, mask, four magnitudes, and three timing bytes | `rumble_body.go` exact nine-byte codec |
| Motor mask maps right/high, left/low, right trigger, left trigger to bits 0-3 | `MotorMask` constants and channel-basis tests |
| xone emits an all-enabled zero-magnitude startup stop with `ff/00/eb` timing | `NewPinnedStopRumbleBody` and exact-vector test |
| Transmit sequence zero is skipped | `SequenceCounter` emits 1-255 and wraps to 1 |

## Local safety inferences

These are explicitly VIIPER policy, not protocol claims:

- Opposite directions on one D-pad axis are rejected.
- Reserved input bits, the unknown rumble byte, and unknown motor-mask bits
  fail closed.
- A non-zero magnitude on a disabled motor is rejected to prevent stale channel
  data from leaking through later translation.
- Guide down state is accepted only as `0` or `1`.
- An explicit stop selects every known motor and sets every magnitude to zero;
  a zero enable mask is not assumed to cancel a prior effect.
- Authentication is unavailable unless a provider is explicitly installed.

## Unknowns intentionally not implemented

1. Windows-visible USB identity, descriptors, endpoints, interface association,
   authentication gating, and XInput/GameInput enumeration behavior need an
   owned Windows capture and an API-visibility oracle.
2. GIP header parsing is not yet safe to call strict. In the pinned reference,
   even-length padding is encoded as an additional varint continuation byte,
   while the header-length calculation counts a zero chunk offset differently
   from the varint encoder. A capture is required to decide the canonical
   zero-offset and padding grammar before accepting hostile input.
3. Receive sequence ordering, retry, duplicate, reset, and replay-window rules
   are not established. Only the observed non-zero transmit allocator exists.
4. Share placement varies with advertised interfaces and dynamic-latency input;
   this package refuses to guess an extension body.
5. The valid magnitude range, duration/delay/repeat units, rollover behavior,
   and host stop variants are not established. Values remain raw bytes.
6. Authentication certificate provenance, credential ownership, key handling,
   and complete handshake behavior are outside this package. The default is
   intentionally unavailable.

## Capture gate for the next layer

Before adding a USB persona or GIP frame parser, collect one owned Windows trace
containing enumeration, successful authentication with legitimately controlled
credentials/hardware, neutral input, one basis event for every button/axis,
Guide and Share, four independent rumble basis vectors, explicit stop, timing
variation, disconnect, and reconnect. Correlate that trace with simultaneous
XInput and GameInput observations. Only captured byte sequences may become
golden vectors; malformed and boundary variants must then be tested before any
USB/IP registration is enabled.

# Xbox One dormant receive-framing gate

Audit and implementation date: 2026-08-31.

## Outcome

VIIPER does **not** yet have a production-safe general GIP fragment
reassembler or downstream replay window. The available sources establish the
fragment header grammar and strict contiguous receive behavior for reliable
large messages, but they do not establish a general rule which distinguishes a
delayed downstream duplicate from a later lawful message after an eight-bit
sequence wraps. None of the downstream message families currently accepted by
`DecodeControllerDownstreamPacket` lawfully requires fragmentation.

The implemented prerequisite is therefore deliberately smaller:
`DormantControllerDownstreamReceiveFramingGate` accepts exactly one complete,
non-fragmented, 1-through-64-byte packet already supported by the canonical
decoder. It reduces accepted transport scratch to the existing fixed-capacity
typed packet and hands that value off once. Reliable-fragment, extended-header,
unsupported-family, malformed, and replay-unprovable outcomes have distinct
typed blockers. Rejected bytes and partially decoded prefixes are never
retained.

The gate is dormant. No registry, USB device, descriptor, server, public
library, interrupt-OUT adapter, CLI, or production persona constructs it.

## Three boundaries which must not be conflated

1. **USB/IP stream framing.** `internal/server/usb/urb_stream.go` reads the
   declared OUT payload with `usbip.ReadExactly` before it calls a device. The
   `usb.InterruptOutTransactionRequest` therefore contains one complete
   non-isochronous interrupt submission, not an arbitrary TCP receive chunk.
2. **One USB data packet.** The controller data endpoint is a 64-byte interrupt
   endpoint. A packet may coalesce several complete GIP messages, so
   `DecodeControllerDownstreamPacket` walks each validated declared length and
   rejects the whole packet on any later error.
3. **GIP reliable-message fragmentation.** A GIP message larger than its data
   class MTU uses Fragment/InitFrag, a variable payload length, a total-length
   or offset field, one sequence for all fragments, acknowledgements, and a
   protocol-specific terminal handshake. This is protocol reassembly across
   complete USB packets; it is not USB/IP stream reassembly.

The gate composes only boundaries 1 and 2. Treating a USB/IP reader chunk as a
GIP fragment, or treating one USB submission sequence as a GIP replay token,
would invent a protocol relationship.

## Source evidence and license treatment

Normative protocol facts come from Microsoft Gaming Input Protocol USB
Extension revision 1.0, published 2024-09-16:

- [GIP data interface](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-gipusb/9c679e2d-9af0-433b-a43a-527b8976f166): interface 0 has 64-byte interrupt OUT endpoint `01` and interrupt IN endpoint `81`.
- [Payload MTUs](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-gipusb/738f56b9-1ee3-4ddc-a223-8de0bedc9310): Command, Low Latency, and Standard Latency use a 64-byte MTU; payload length excludes the header and is a bounded base-128 variable field.
- [Reliable large-message transmission](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-gipusb/b7af910e-a999-40ad-a645-df110961477b): all fragments use the same non-zero sequence; the first fragment carries total payload length, later fragments carry offsets, and downstream receivers must support the defined six-byte formats.
- [Reliable-message acknowledgement](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-gipusb/5a4be035-9e20-4350-b81a-a08627a985ca): a receiver which sees a noncontiguous offset reports its sequential contiguous byte count, discards fragments beyond the expected offset, and requires sequential resend.
- [Direct Motor](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-gipusb/ee8c5b28-e8da-4cc4-bb48-17781b8371af) and [Guide LED](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-gipusb/ec312389-2e05-4915-85ed-0e8fe9c3d33b): the supported output commands are fixed 13-byte and 7-byte single-packet messages with wrapping non-zero Command sequence IDs.
- [Messages metadata array](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-gipusb/c80f7fe9-623c-4b8c-a95f-6f39e35a9345): USB messages should not request strict downstream `+1` sequencing; without that declaration downstream sequence changes are not guaranteed to be monotonic. This prevents a general replay/window rule from being inferred from sequence distance alone.

The independently pinned xone tree was audited at
`3484f603484782dd7551c64e5a33fc602b127051`. It is GPL-2.0-or-later. Only
behavioral and protocol facts were used; no xone source expression was copied.
The relevant local files were `_references/xone/bus/protocol.c` and
`_references/xone/transport/wired.c`. They corroborate these observations:

- wired receive buffers are bounded to one 64-byte packet;
- payload length and total/offset fields are decoded separately;
- a first fragment starts a bounded allocation and later fragments target an
  offset; and
- command equality and total-length bounds are checked.

That implementation does not supply the stronger production authority VIIPER
would need here: it does not establish a general downstream replay window, and
its fragment copy path is not an exact proof for overlap, filled-range, stale
sequence, or post-terminal duplicate policy. Those omissions are evidence to
stay fail-closed, not behavior to reproduce.

## Gate contract

Construction requires a non-zero external generation and receive epoch. The
repository intentionally has no issuer for those values. One gate has a
monotonic state:

`Fresh -> Claiming -> Accepted -> Taken`

or

`Fresh -> Claiming -> Blocked`

An invalid credential before handoff changes `Accepted` to `Quarantined`.
There is no reset. Any concurrent or later Claim on the same lifetime returns
`ControllerDownstreamReceiveReplayUnprovable` and the preconstructed
`ErrControllerDownstreamReceiveReplayUnprovable`. Constructing a second gate is
not authenticated successor evidence and must not be presented as replay
support.

The first Claim delegates directly to `DecodeControllerDownstreamPacket`:

| Canonical result | Typed gate result |
| --- | --- |
| complete supported packet | private exact claim |
| `ErrFragmentedHeader` | `ReliableFragment` blocker |
| `ErrExtendedPayloadLength` | `ExtendedFraming` blocker |
| unsupported complete family | `UnsupportedMessage` blocker |
| every other decode failure | `Malformed` blocker |
| second/concurrent Claim | `ReplayUnprovable` blocker |

No prefix result escapes if a later coalesced message fails. No raw slice is
stored. `TakeExactPacket` authenticates owner address, generation, epoch,
private token, and message count, then returns the fixed-capacity typed packet
by value once. A foreign or forged credential quarantines that exact gate and
cannot clean or affect another owner.

Claim and Take perform no I/O and allocate zero on the warm accepted path.
The fragment and extended-framing blocker paths also allocate zero. The
constructor is cold and creates the one owner object. Diagnostics are
lock-free and never expose retained packet contents.

## Why reassembly is not implemented in this tranche

The current canonical downstream families are Metadata Request, Set Device
State, Protocol Control ACK, Direct Motor, and Guide LED. Their accepted wire
forms fit one 64-byte packet. The large-message families named by the source
are metadata responses, security, and extended commands; there is no current
downstream decoder, lifecycle owner, or executor for their reassembled payload.
Adding a generic byte assembler would therefore create raw payload authority
with no legal canonical consumer.

Even a byte-perfect assembler would not close terminal replay. GIP sequence is
eight-bit, non-zero, and wrapping. The reliable-fragment rules authenticate the
fragments of one active transfer, but the reviewed material does not define a
general receive window after that transfer retires. VIIPER also does not expose
an authenticated connection/lifecycle credential which proves when a reused
sequence starts a successor transfer. Accepting matching bytes, accepting a
USB/IP submission number as a GIP token, or choosing an arbitrary sequence
distance would all be guesses.

## Validation and remaining blockers

Offline tests cover complete/coalesced handoff, source-scratch mutation,
fragment after a valid prefix, extended framing, malformed and unsupported
packets, zero identities, foreign/stale/copied credentials, exact at-most-once
concurrent Claim/Take, concurrent snapshots, warm-path allocations, repeated
stress, race detection, and fuzz invariants.

Production remains blocked on all of the following:

- an exact production issuer for connection generation and receive-transfer
  epoch;
- a source-defined downstream replay/window and sequence-wrap retirement rule;
- an exact bounded reassembler with filled-range/overlap/gap/timeout and
  terminal-handshake ownership;
- canonical decoders and lifecycle owners for any large downstream family;
- composition with the existing interrupt-OUT response/worker transaction
  without executing effects under response-writer ownership; and
- production identity, descriptor, Windows binding, hardware feedback, and
  end-to-end conformance evidence.

This gate closes only the failure-atomic framing handoff for already-supported
complete packets. It does not clear any general reassembly, replay, USB/IP,
Xbox binding, output, latency, or hardware blocker.

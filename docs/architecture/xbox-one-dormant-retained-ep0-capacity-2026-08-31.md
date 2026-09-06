# Dormant Xbox retained EP0 254-byte response capacity (2026-08-31)

Status update (2026-09-01): still unregistered, but now transport-routed behind
the explicit retained-device/authority opt-in. Product creation, Windows
binding, and hardware validation remain absent.

## Bounded change

`DormantRetainedUSBAdapter.Limits` now reports
`MaximumControlResponse: 254`. This is the largest even descriptor length
representable by the one-byte USB string-descriptor `bLength` and matches the
fixed maximum used by the exact external-identity gate. No identity, string,
VID/PID, or authorization value is supplied by the adapter.

The remaining geometry is unchanged:

- control, interrupt IN, and interrupt OUT queue depths remain one;
- control OUT remains zero bytes;
- interrupt IN and interrupt OUT remain 64 bytes;
- aggregate buffered request storage remains 64 bytes; and
- endpoint routes remain interface zero, alternate zero, `81` IN and `01` OUT.

## Ownership and storage audit

`internal/retainedusb.Limits.Valid` accounts only bytes copied from host
requests: queue depth times control-OUT maximum plus queue depth times
interrupt-OUT maximum. A device-to-host EP0 response is not a request slab.
The dormant server scheduler therefore retains the existing zero-byte control
slab and one 64-byte interrupt-OUT slab.

At construction, the scheduler separately allocates one shared response
scratch of `RET_SUBMIT` header plus
`max(MaximumControlResponse, MaximumInterruptIn)`. Increasing the control
response maximum consequently changes that private scratch from header+64 to
header+254, without changing a queue, slot, or request slab. The response
serializer clears and capacity-clamps this fixed scratch before preparation.

For EP0 IN, the scheduler's response window is the minimum of the owner
maximum, USB/IP transfer length, and setup `wLength`. The Xbox coordinator then
claims through the canonical engine and exposes only the exact claimed prefix
to `AdmitAndCopy`. A response cannot exceed any host or owner bound. An
inconsistent setup/transfer length is rejected before response admission. The
host's `wLength` is a requested maximum rather than the descriptor's actual
size: a self-consistent request above 254 is admitted and returns the complete
254-byte descriptor. Ordinary smaller `wLength` is accepted as USB host
truncation.

No new owner, mapper, queue, response writer, replay rule, or transport route
was added.

## Evidence

The dormant server integration tests construct an exact one-shot authorized
persona with a product string containing 126 three-byte BMP scalars. That
produces an exact 254-byte UTF-16LE descriptor and exercises the real chain:

```text
retained scheduler -> DormantRetainedUSBAdapter -> existing coordinator
-> authorized ControllerPersonaEngine -> late RET_SUBMIT serializer
```

They verify:

- byte-exact 254-byte delivery and actual length;
- odd host `wLength` prefix truncation;
- rejection of an undersized transfer/setup mismatch;
- exact 254-byte delivery for lawful self-consistent host requests of 255 and
  65,535 bytes;
- the same 255/65,535 semantics directly at the retained owner boundary with
  only a 254-byte destination, proving that no host-sized response allocation
  is required;
- exact header+254 response scratch;
- unchanged depth-one lane storage, zero EP0/IN request slabs, and one 64-byte
  OUT request slab; and
- zero allocations over the warmed complete adapter/scheduler/serializer path.

## Deliberate non-activation

This removes only the former 64-versus-254 dormant serializer mismatch. The
adapter and authorized identity gate still have no device-registry,
descriptor-callback, CLI, USB/IP command-reader, Windows, or hardware call
site. The remaining persona capability blockers, identity provenance limits,
receive/replay gaps, power/reset integration, binding evidence, and conformance
work remain authoritative. This capacity result must not be described as Xbox
device registration or production compatibility.

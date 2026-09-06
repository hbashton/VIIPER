# Dormant Xbox external-identity gate

Date: 2026-08-31
Status: offline prerequisite only; no registry, CLI, USB/IP, device, Windows,
or hardware activation

## Result

VIIPER now has a narrow optional gate which can bind exact caller-supplied USB
Manufacturer, Product, and Serial strings and one explicit caller
authorization decision to one exact `ControllerPersonaEngine` construction.
It reuses the canonical `USBControlPlane`; it does not add a descriptor
dispatcher, mapper, transport owner, or second persona.

The existing unregistered constructors are unchanged:

- `NewUnregisteredControllerProfile` still requires every numeric identity
  field and supplies no defaults.
- `NewUSBControlPlane` still stalls indexes 1/2/3 with
  `ErrUSBStringDescriptorUnavailable`.
- `NewControllerPersonaEngine` still reports every persona capability blocker.

The new path is deliberately dormant. No production package calls
`NewAuthorizedControllerPersonaConfig` or
`NewAuthorizedControllerPersonaEngine`.

## Source boundary

Normative facts come from Microsoft MS-GIPUSB revision 1.0, published
2024-09-16:

- section 2.2.4, USB String Descriptors: strings use UTF-16LE; GIP USB uses
  LANGID `0x0409`; Manufacturer, Product, and Serial use indexes 1, 2, and 3;
  the Serial descriptor is unique and contains Device ID;
- table 7, USB Device Descriptor: index 3 is 32 hexadecimal digits containing
  the 64-bit Device ID; and
- USB 2.0 standard control transfer behavior: a descriptor response is bounded
  by host `wLength`.

The pinned official DOCX is `MS-GIPUSB-240916.docx`, SHA-256
`810F2841AF3FD832E5EBBDFB581D67413DDA5914364E713AEB35D63483D6D9B0`.
The package provenance ledger records the complete pin and Open
Specifications notice.

The gate's use of `%016x` as the fixed-width textual form of the profile's
64-bit numeric Device ID is a local, fail-closed normalization inference. It
does not synthesize bytes: the caller must supply all 32 hexadecimal digits.
It can reject a different valid serial convention and never broadens
acceptance to a serial which omits that numeric value. It is not serial
generation or uniqueness evidence.

No Microsoft VID/PID, manufacturer string, product string, serial, device
credential, or source expression from xone, PadForge, HIDMaestro, or another
implementation is embedded here.

## External decision is not legal proof

`ControllerIdentityAuthorizationGranted` records only an explicit decision by
the caller. It does not mean that VIIPER checked USB-IF allocation ownership,
trademark rights, manufacturer identity, serial uniqueness, patent rights, or
Windows compatibility. `Denied` returns before profile/string validation,
allocates no credential, and does not consume or revoke the caller's reusable
input values. Undefined decisions fail closed.

Accordingly the authorized engine clears exactly these local capability bits:

- `ControllerPersonaBlockerExternalUSBStrings`; and
- `ControllerPersonaBlockerIdentityAuthorization`.

Every other blocker remains set, including metadata semantic validation,
authentication, reassembly/replay, lifecycle, backend integration, Windows
binding, and hardware conformance.

## Exact lifetime and copy/ABA model

Profile construction and metadata binding each allocate a private,
non-zero-sized issuance object. Value copies from one issuance intentionally
share that issuance, while an independently reconstructed equal numeric
profile or equal metadata blob receives a different issuance. Both the legacy
persona constructor and `NewMetadataTransfer` now require the metadata's exact
profile issuance in addition to numeric identity equality.

Authorization records, under one private owner:

- the exact profile and metadata issuance pointers;
- the numeric identity and USB configuration;
- metadata identity, byte length, and SHA-256 digest;
- a private copy of the compiled metadata;
- three immutable, pre-encoded fixed-capacity UTF-16LE descriptors; and
- one non-zero-sized private credential pointer.

The opaque authorization value is copyable only as a reference to the same
one-shot owner. Its state transition is:

```text
Issued -> Consuming -> Consumed
                  \-> Quarantined
```

The mutex-protected `Issued -> Consuming` transition linearizes before engine
construction. A successfully constructed pointer is bound to both its exact
address and the address of its embedded canonical `USBControlPlane` before
the state becomes `Consumed` and before the pointer escapes. Any construction
failure after consumption begins leaves the owner quarantined. Concurrent or
sequential copied authorizations therefore produce at most one engine.

A copied engine reports the full blocker mask. A copied engine or copied
control plane cannot serve identity strings because its address does not match
the private binding. Forged, cross-owner, same-value ABA, malformed private
descriptor, stale, and quarantined credentials fail without publishing a
claim. A forged copy does not poison the canonical unconsumed credential.

## String and EP0 rules

Construction rejects:

- a raw string longer than 378 UTF-8 bytes before Unicode validation scans it;
  378 is the maximum source for 126 three-byte BMP code points, the most UTF-8
  bytes that can still fit the descriptor's 126 UTF-16 code units;
- empty or malformed UTF-8 Manufacturer/Product/Serial values;
- embedded NUL and invalid Unicode scalar values;
- UTF-16LE descriptors larger than 254 bytes or with malformed private length
  and surrogate structure;
- a Serial value other than exactly 32 ASCII hexadecimal digits; and
- a Serial value which does not contain the fixed-width numeric Device ID.

Encoding happens only during authorization construction into private
`[254]byte` records. The authorized canonical control plane serves only
indexes 1, 2, and 3 with LANGID `0x0409`. Wrong language, wrong index,
unavailable legacy strings, malformed private state, and mismatched authority
stall without a claim. Accepted responses use the existing
claim/exact-size-admit/terminal-resolve transaction and normal `wLength`
prefix truncation. No rejected request retains or partially decodes bytes.

The successful warm string path performs no allocation, I/O, sleep, registry
lookup, or transport call. Its larger fixed EP0 plan capacity is 254 bytes,
the largest even length representable by a USB string descriptor's one-byte
`bLength`.

## Evidence and remaining blockers

Deterministic tests cover exact descriptor bytes, supplementary Unicode,
length/language/index rejection, serial grammar, denied/unknown decisions,
foreign same-value metadata, copied/forged/cross-owner/stale authorization,
same-value ABA, post-consumption construction failure, exact engine/control
plane copying, concurrent one-winner consumption, and zero warm allocations.
Property/fuzz tests cover arbitrary UTF-8, serial values, descriptor indexes,
languages, and `wLength`; race tests cover concurrent authorization copies.

This evidence does not establish a lawful production identity, metadata
semantics, authentication policy, generic GIP receive behavior, USB power and
reset coordination, backend integration, Windows binding, or hardware
conformance. Production activation remains forbidden until those independent
blockers are closed and measured.

The separately reviewed dormant retained-control extension now advertises
`MaximumControlResponse: 254`, matching the authorized UTF-16LE descriptor
maximum. Its scheduler allocates one fixed response scratch at construction;
EP0 OUT remains zero bytes, interrupt request/response maxima remain 64 bytes,
and every lane remains depth one. Offline end-to-end tests prove full-size
delivery, smaller host `wLength` truncation, inconsistent setup/transfer
rejection, full 254-byte replies for lawful host maxima of 255 and 65,535,
unchanged request slabs, and zero warmed allocations. A host maximum above the
descriptor size is not itself malformed. This closes only the former
serializer-capacity mismatch. It does not attach the identity gate to a
registry, device, USB/IP command reader, Windows backend, or hardware.

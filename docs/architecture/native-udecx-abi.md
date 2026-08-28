# Native UdeCx ABI landing zone

This directory contains the transport contract only. It intentionally does not
contain a driver runtime, device installation code, controller scheduling, or a
Windows client. Keeping the contract independently testable lets the native
transport land underneath the current input scheduler instead of importing an
older scheduler with it.

## Provenance

ABI 1.14 was extracted from
`origin/feature/native-udecx-bus` at
`cece30774e9df8183dc74fe6cd25950f1a74d021`. That revision introduced the
kernel-authored device-correlation receipt and package version `0.1.0.38`.
The field-offset guards originate in
`c6ce82b01bcbb3e91543cac986061e26f1955158`, and the fail-closed negotiation
contract originates in
`ed5b23b31392694c21793de942c4d5d3cdec23a1`.

The C header is kept byte-for-byte compatible with the ABI 1.14 source. The Go
package is platform-neutral and carries the corresponding constants,
encoders/decoders, identity rules, and negotiation validator.

## Compatibility rules

- The magic, ABI major, ABI minor, fixed sizes, and reserved flags must match
  exactly. Older and newer minor versions are rejected.
- Negotiation requires the exact advertised capability set. Streams are
  defined by the ABI but are not advertised by this package.
- The loaded driver must return the exact client nonce, a nonzero driver nonce,
  bounded nonzero limits, and the exact source-bound SHA-256 build identity.
- Device identity is the pair `DeviceId` and `Generation`.
- Endpoint identity additionally includes `EndpointAddress` and the immutable
  `EndpointGeneration`. Interrupt-input submissions also require a strictly
  positive signed-64-bit sequence.
- A successful create receipt contains exactly one kernel-authored port:
  USB 2.0 for low/full/high-speed devices, or the globally numbered USB 3.0
  port for SuperSpeed devices.

## Verification

`go test ./internal/transport/udecx` requires no WDK and mechanically compares
the Go contract with `native/udecx/include/ViiperUdeProtocol.h`:

- every packed structure size and every asserted field offset;
- capability and limit masks;
- descriptor and operation enums;
- interface GUID components;
- CTL_CODE function, transfer method, access mask, and final numeric value;
- golden negotiation, build-identity, and interrupt-input wire images;
- malformed messages, unsupported versions/capabilities, invalid identities,
  oversized limits, and mismatched build identities.

The header also retains C/C++ compile-time size and offset guards for the later
WDK build.

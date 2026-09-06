# Authenticated transport direction review, 2026-09-02

Status: **original v1 defect confirmed; coordinated v2 source repair implemented
on September 3, with deployment/SDK acceptance still open**. This is not an exploitation claim, a hardware result, or
an explanation for the isolated b48 probe failure. No live credentials or raw
process traces were inspected for this review.

## AUTH-V1-DIRECTION-REUSE

Independent review of the DS4Windows handshake-key lifetime repair found an
inherited problem in the original `eVI1\0` encrypted protocol. The source details
in this finding describe the pre-repair implementation. Parent review
confirmed the same behavior in the Go server:

- DS4Windows `DS4Control/Viiper/ViiperAuthenticatedStream.cs` constructs one
  ChaCha20-Poly1305 object from `sessionKey`. Send and receive counters start
  at zero; both record directions require a zero four-byte nonce prefix.
- VIIPER `internal/server/api/auth/conn.go`, `WrapConn`, `Write`, and `Read`
  have the same single-key, zero-prefix, zero-based-counter convention.
- `internal/server/api/server.go` derives one session key from the deployment
  key and both handshake nonces, then passes it to `auth.WrapConn`.

Consequently client record 0 and server record 0 use the same key/nonce pair;
the collision repeats for corresponding counters. Per-direction monotonic
checks do not prevent cross-direction nonce reuse or reflection. This violates
the nonce-uniqueness requirement of
[RFC 8439, sections 2.3 and 2.6](https://www.rfc-editor.org/rfc/rfc8439.html#section-2.3).
The RFC explicitly describes distinct sender prefixes for multiple senders
sharing a key. Current encrypted-loopback operation and passing payload
round-trip tests do not establish cryptographic protection.

This issue is distinct from controller authentication: it concerns the local
DS4Windows-to-VIIPER API connection, not Xbox console security or Nintendo host
association. No console behavior, keys, security settings, driver or installed
component was changed.

## Separately repaired C# ownership defects

`CopyDerivedDeploymentKey` now returns a connection-owned copy under the cache
lock. A concurrent cache refresh may clear the old cache allocation without
changing an in-flight handshake's HMAC/session derivation key. A deterministic
interleaving failed the original code with `AuthenticationTagMismatchException`
and passes the repair. Failed handshakes clear only their private scratch;
returned encrypted streams retain their own session-key allocation.

Encrypted-stream disposal now clears its key and buffers in `finally`, even
when the underlying transport throws on close. The dedicated throwing-close
test failed before this repair. These changes preserve v1 wire behavior; they
**do not repair AUTH-V1-DIRECTION-REUSE**.

## Coordinated repair scope

Do not silently reinterpret `eVI1`, change only one endpoint's nonce layout,
disable authentication, downgrade to plaintext, or claim the old protocol is
secure. A versioned change must cover:

1. Go handshake detection, transcript contexts, server wrapping, and Go clients
   (`internal/server/api/auth`, `internal/server/api/server.go`, and all
   `HandleAuthHandshake` / `WrapConn` call sites).
2. DS4Windows authenticated probe, controller streams and protocol tests.
3. Generated C#, C++, Rust and TypeScript authentication implementations under
   `internal/codegen/generator`, checked-in generated clients if present, and
   their regeneration/golden-vector tests.
4. Compatibility/migration documentation and explicit failure on an unsupported
   authenticated version. A client must not silently retry v1 or plaintext.

Minimal v2 contract accepted in independent review and implemented in source:

- A distinct `eVI2\0` handshake plus version-separated authentication/session
  contexts; retain the existing bounded packet envelope and established
  cryptographic primitives.
- Explicit client/server roles. Partition the 96-bit nonce into a fixed
  direction-specific 32-bit prefix and a monotonically increasing 64-bit
  counter. The receiver accepts only the opposite role's prefix and its exact
  next counter; counter exhaustion closes the connection. Role is not inferred
  from the first received packet.
- Default authenticated operation rejects v1 with an actionable migration
  error. Preserve separate explicit plaintext policy rather than treating a
  failed encrypted handshake as permission to use it.
- No extra per-report allocation, timer, lock, retry queue, or worker in the
  production Go/DS4Windows stream. Prefix encoding/validation fits its existing
  preallocated record path. Generated-client concurrency/partial-I/O repairs
  use the ownership needed by each language's stream API.

Required acceptance evidence before declaring the repair complete:

- fixed independent cross-language handshake/session/record vectors;
- identical plaintext sent simultaneously in both directions produces distinct
  key/nonce inputs, and reflected/opposite-direction records are rejected;
- replay, reorder, tamper, truncation, partial I/O, counter exhaustion, bad key,
  unsupported version and no-downgrade tests;
- Go/C# full-duplex interoperability using only synthetic keys on isolated
  endpoints, plus generated-client checks;
- warmed input and feedback record paths retain their allocation guarantees;
- independent adversarial review and portable end-to-end retest of the matched
  app/broker pair. Existing b48/b49 hashes remain historical v1 candidates.

The physical USB unplug and Bluetooth/game feature gates remain separate and
unpassed; none is replaced by this security repair scope.

## September 3 source repair and validation

The wire contract is now `eVI2\0`, `VIIPER-Auth-v2`, and `VIIPER-Session-v2`.
The existing `VIIPER-Key-v1` PBKDF2 salt/iterations remain unchanged: deployment
password files need no migration. Client send/server receive uses BE32 domain
0; server send/client receive uses domain 1. Both append the BE64 sequence,
beginning at zero. Sequence advancement requires authenticated plaintext on
read, and wrap is refused. Unsupported encrypted versions cannot enter the
separate plaintext-policy path. **Update client and broker together.** No
automatic v1/plaintext retry was added, and no running binary was replaced.

The two direction/reflection tests first failed against v1. Independent
Node/OpenSSL record vectors now agree with Go, DS4Windows, generated C#, and
generated Rust/TypeScript. Fixed session-derivation vectors cover nonce order
and v2 context; a fixed HMAC request is accepted by the real Go handshake.
Malformed records fault the production streams without reporting ciphertext
byte counts as delivered plaintext. Empty operations do no I/O; empty valid
records do not masquerade as EOF.

Independent follow-up review found and prompted fixes for Rust's buffered
final-record liveness, TypeScript EOF/backpressure, and Node's short final
`read(n)` result. Generated Rust request/send boundaries now flush the bounded
pending record, consistent with the
[Tokio AsyncWrite flush contract](https://docs.rs/tokio/latest/tokio/io/trait.AsyncWrite.html#tymethod.poll_flush).
Its public writer retains an exact ciphertext suffix across partial writes or
Pending; a deterministic three-byte writer tests an abandoned flush followed
by a successor write. TypeScript tests cover coalesced handshake/first record,
truncated nonce responses, record tails, full readable buffers followed by EOF,
and counters beyond JavaScript Number precision. Generated C# serializes
counter selection through the complete async frame write with independent
read/write gates. Generated C++ uses one checked receive path for byte and
line readers and OpenSSL randomness for handshake nonces.

Full Go tests and the auth/client/API race suites passed. Go's new read
allocation test failed at one allocation per record, then passed at zero after
moving the four-byte header into the connection's read-locked storage. The
existing warmed write allocation test remains zero. See the
[September 3 validation ledger](controller-platform-validation-2026-09-03.md)
for exact final test counts and the remaining acceptance gates. In particular,
full generated SDK builds are **not green**: Xbox DTO/type generation and the
C# SDK target/toolchain mismatch remain separate problems, and C++ auth has
not yet been compiled in this environment. Passing isolated transport tests
must not be presented as full SDK, installer, or hardware readiness.

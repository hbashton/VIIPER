# Exact production Xbox One removal

The authenticated `bus/{id}/add-authorized-xboxone` response includes a
top-level `removalToken`. It is a fresh 256-bit `crypto/rand` nonce encoded as
exactly 64 lowercase hexadecimal characters. This is a creation-only secret:
it is not in ordinary device listings, descriptors, device-specific arguments,
input, feedback, or API diagnostics.

The same creation receipt includes `usbipBusId`, an independent public
per-registration export alias, and `removalTimeoutMilliseconds`, the actual
normal server close budget rounded upward. The timeout is a positive integer
at most 300000; unsupported configurations are rejected before production
creation rather than clamped. The normal CLI default is 90000 milliseconds
(three times its 30-second connection timeout).

`usbipBusId` is `x1-` followed by 26 canonical lowercase unpadded base32
characters from 16 independent cryptographically random bytes (29 ASCII
bytes total). Its final character is one of `a/e/i/m/q/u/y/4`. It is not
derived from the secret removal token. The immutable alias is installed before
registration publication, including descriptor admission; aliases already
reserved by unpublished registrations also fail the collision check.

USB/IP DEVLIST, IMPORT reply, native Windows attach, and Linux/Windows CLI
attach all preserve this exact export identity. Protected production imports
accept no numeric bus-device fallback. Stale aliases therefore cannot select a
different controller after numeric address reuse, bus reuse, or server restart.
Numeric USB/IP bus/device fields and the resulting URB transfer device ID are
unchanged. Generic and explicitly dormant/test registrations remain numeric.

The production registry binds the nonce once to the unique lifetime channel
of the successful `VirtualBus.Add`. Neither numeric address reuse, bus-number
reuse, server restart, nor re-adding the same device pointer transfers that
authority. No predictable local counter is used as the secret.

Send the following over an authenticated API session:

```text
bus/{busId}/{devId}/remove-authorized-xboxone {"version":1,"removalToken":"<captured 64-character token>"}
```

The same closed capability payload is required for
`bus/{busId}/{devId}/stream-authorized-xboxone` and
`bus/{busId}/{devId}/activate-authorized-xboxone`. The former is a NUL-terminated
stream command followed by unchanged X1BR v1 framing; the latter returns
`version:1`, the exact public `usbipBusId`, a positive `usbipPort`, and
`usbipOwnerSerial` (empty for pinned native 0.9.7.7). Legacy address-only broker
opening is rejected for Xbox. Duplicate authorized streams cannot displace the
consumer; activation revalidates both registration and consumer after native
I/O. Registration leases cover only synchronous lifecycle transitions, never
network/native I/O or acquisition of the stream-coordinator mutex.

The request schema is closed and bounded to 256 bytes. Both fields are required;
unknown fields, duplicate fields, case-folded names, wrong types, unsupported
versions, noncanonical tokens, and trailing JSON are rejected. Authentication
is checked before parsing or lookup. The route never echoes token material in
an error.

Responses are `{"version":1,"removed":true}` or
`{"version":1,"removed":false}`. A missing/already-gone target or a validly
formed token which does not authorize the captured incarnation returns false
without touching any successor. False is not a new proof of neutralization.

For a matching live incarnation, the server fences further retained stream
registration for that exact Add, captures and cancels only its existing USB
streams, and joins their existing retained lifecycle. The feedback consumer
must remain alive during this request so `DisconnectNeutral` can receive its
exact canonical Stop acknowledgement. Only after successful joining does
the operation complete exact registration removal. A concurrent safe one-shot
retirement may already have removed that same registration; this still counts
as true for the closer which captured it live. Concurrent callers may join
the result or observe that the registration is already gone.

Rejected/ambiguous feedback, drain or transport failures, and incomplete joins
return a conflict instead of claiming success. Quarantined owner/admission
proofs stay with their existing lifecycle owners. The exact close fence cannot
be reopened by a late stream completion. Normal registration cancellation
cleans up idle close bookkeeping, including when this API was never called.

The join budget is the server's existing retained close deadline: normally
three times `ConnectionTimeout`, using a five-second lifecycle fallback when
unset (15 seconds total), or an already-established server shutdown deadline.
Clients need a compatible bounded response timeout; a client timeout is not
permission to attempt numeric device, bus, or port cleanup.

This API does **not** prove that a Windows USB/IP port has been permanently
detached or that saved-location auto-reattach has been canceled. In pinned
usbip-win2 0.9.7.7, receiver socket loss can schedule another attach by the
saved export location. The independent export alias makes such an old retry
fail lookup instead of selecting a numeric-address successor. No client-side
numeric port detach is authorized by this behavior. Token-bound broker stream
and activation admission remain necessary alongside alias-safe USB selection;
an export alias is public routing identity, not API authentication.

### Native activation retry policy (September 5 source follow-up)

Windows native production Xbox activation now uses pinned 0.9.7.7's
`PLUGIN_HARDWARE_ONCE`. The request/response layout, exact export alias and
positive-port validation are unchanged. If the initial native call fails,
it must not also schedule a background import after the creation transaction
has reported failure and rolled back. Legacy numeric exports keep ordinary
`PLUGIN_HARDWARE`; no global driver retry setting is changed.

This operation does **not** cancel reattachment after a successful import later
loses its broker connection. Source review confirms that receiver teardown
schedules those requests separately. Exact stale-location retry cancellation
remains an open cleanup gate; neither a fixed delay nor a saved numeric port
proves the driver has finished creating its retry work. In particular, calling
the location-based `STOP_ATTACH_ATTEMPTS` before that work exists can return
zero and still be followed by a retry. Do not report such a zero as permanent
retirement, or use global Stop/Detach All to conceal this gap.

The five new fake-native call-boundary tests and `go test ./...` pass. No
hardware operation is performed by these tests, and the running portable b56
broker has not been replaced with this source follow-up.

### Native activation cancellation (September 5 source follow-up)

Native attach now opens an overlapped driver handle and owns one manual-reset
completion event, pinned payload and `OVERLAPPED` structure. Cancellation uses
`CancelIoEx` with that exact non-null structure; neither all-handle cancellation
nor port-number detach is used. The helper observes the operation's actual
completion and joins any cancellation callback before releasing its event,
buffer or outer driver handle. Pending/incomplete observations cannot retire
that storage. A success which wins cancellation retains its returned port as
evidence; cancellation is not falsely reported as native removal.

This follows Microsoft's [CancelIoEx contract](https://learn.microsoft.com/en-us/windows/win32/api/ioapiset/nf-ioapiset-cancelioex)
and [GetOverlappedResult completion rules](https://learn.microsoft.com/en-us/windows/win32/api/ioapiset/nf-ioapiset-getoverlappedresult).
The configured API `ConnectionTimeout` (30-second fallback when unset/nonpositive)
requests activation cancellation; it is **not a hard native completion bound**.
A driver which delays or cannot complete cancellation can still retain the
in-flight operation. Request/registration cancellation and the deadline prevent
that operation from subsequently committing a successful activation response.
The existing API request context does not independently observe a peer closing
its management socket while the handler is executing; that event alone is not
claimed as an immediate cancellation source.

Before native submission, activation rejects an already-canceled request without
consuming the ready owner. Once reserved, the operation retains its reservation
through actual native completion and cancellation cleanup. Cancellation removes
only the captured registration through the existing retained stream drain/Stop
ACK lifecycle. A reused numeric address cannot select a successor. Incomplete
cleanup returns conflict and remains fenced, not successful removal. Commit
revalidates context, exact registration and feedback-consumer readiness. Normal
non-cancellation attach errors retain the prior same-owner retry policy.

Six new native-operation tests and five new handler tests cover pending abort,
late success/ERROR_NOT_FOUND, callback/storage lifetime, malformed responses,
pre-cancellation, deadline propagation, duplicate activation while cancellation
is pending, and exact-registration replacement. The prior ABI tests now exercise
the same production helper; the obsolete synchronous helper was removed.
`go test ./... -count=1`, `go vet ./...`, and ten race-enabled repetitions of
native/activation regressions pass. These tests use fake native calls and real
loopback authenticated broker streams, not the installed driver.

Successful-retired-import retry cleanup is still a distinct open gate. Pinned
0.9.7.7 handles imported-device queries and retry cancellation on a parallel
queue, separately from detach/retry creation. Cancellation matches a 32-bit
location hash (zero selects all), not an opaque native attachment identity.
Neither observing absence nor canceling zero current retries establishes that
future retry work cannot appear. No speculative native cleanup operation was
added. This source is not in the running portable b56 payload.

When a positive `BusCleanupTimeout` is configured, creating an initially empty
bus schedules the same exact bus/empty-incarnation cleanup used after device
removal. A rejected factory therefore does not require numeric bus deletion by
the client. Initial zero-timeout embedding behavior remains unchanged.

Source-backed in-process coverage includes real production broker/retained USB
Stop ACK success, rejection, wrong correlation, socket loss, late import
registration, close timeout, duplicate-import Busy isolation, registration
cleanup, stale address/bus reuse, token isolation, schema validation, and
concurrent idempotent removal. These tests do not access physical hardware.

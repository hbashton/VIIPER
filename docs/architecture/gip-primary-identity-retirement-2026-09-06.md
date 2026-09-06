# Primary GIP identity must survive asynchronous native retirement

The primary GIP Hello DeviceID is an OS lookup key, not a model identifier.
Changing only the USB serial or retained import lease ID does not make two
primary GIP devices distinct. Local Windows dump evidence from September 6
showed two different USB instances with one configured Hello identity, the
second device's collision flag set, and the first device stuck waiting for
rundown in dc1-controller release-hardware while holding the global PnP lock.
The resulting shutdown bugcheck was 0x9F/4. Private device identifiers and
Windows implementation/disassembly are deliberately not reproduced here.

## Broker contract

The authorized Xbox wrapper exposes its immutable, validated profile DeviceID
through `retainedusb.PrimaryGIPIdentityDevice`. This is separate from
`RetainedUSBImportDeviceID` and grants no authority. Production Xbox registration
requires the identity capability; common retained registration also checks it
when present, so choosing the numeric retained route does not bypass the guard.
Identity callbacks remain outside server/global locks and panic fails closed.

Before descriptor publication, the server atomically reserves the primary ID
under its existing registration locks. A duplicate fails before USB discovery,
even while the first descriptor is still being admitted. The reservation is
retained after failed provisional admission, device removal and bus removal:
USB/IP drain is not proof of native Windows PDO removal. The set is capped at
65,536 identities per server lifetime; exhaustion rejects without evicting old
entries. No input, feedback, endpoint scheduling or authentication behavior is
changed. The runtime hot path does not consult this set.

Clients must allocate a fresh primary DeviceID and matching USB serial for
every new registration. DS4Windows b79 uses an explicitly authorized
`derivePerRegistrationIdentity` deployment option. Its earlier serial-only
option is insufficient and is not silently widened. Existing fixed-identity
clients now receive a clear registration error instead of exposing a duplicate
to Windows. The wire request schema is unchanged.

This server-lifetime guard is not a machine-global registry and does not prove
the absence of orphaned IDs from earlier broker processes or other exporters.
Clients' random process seeds lower that risk but do not eliminate it. Do not
retry the old duplicate-identity reproduction on a live Windows host.

## Tests

`go test ./... -count=1` passes, as do race tests for `internal/server/usb`,
`internal/registry`, `internal/server/api/handler`, and `device/xboxone`.
Coverage includes distinct serial/import ID duplicate rejection, reservations
through removal, an unpublished blocking descriptor, 32 concurrent competing
registrations, missing/invalid/panicking callbacks, and bounded exhaustion.
The existing real-persona two-pad USB/IP test now verifies each decoded Hello
DeviceID against its distinct requested ID while retaining independent input,
four-motor feedback and exact-removal isolation checks. Lifecycle fixtures were
updated to distinct identities without weakening stale capability assertions.

These are synthetic transport tests. They do not alone certify Windows PnP
teardown, physical Switch 2 reconnection, game compatibility or latency.

## Personal Windows acceptance after the synthetic tests

A portable build from `f39b732`, with the DS4Windows b79 factory/client, passed
five neutral-only native create/remove cycles, including two simultaneous
Windows.Gaming.Input gamepads. Removing the second preserved the first pad's
visibility and semantic-input acknowledgements. All five exact USB instances
were observed started/problem-zero and then absent through Windows Config
Manager, not just gone from USB/IP. Removal plus 100 ms polling observation
took 120.8–124.9 ms in these samples. Activation ACKs took 2,090–2,144 ms;
neither number is input latency. This does not certify physical controller
reconnection or the complete multi-controller game matrix.

The post-removal broker log also exposed delayed retries of retired aliases.
usbip-win2 0.9.7.7's PLUGIN_HARDWARE_ONCE suppresses initial failure retry only;
its device-disconnect path can separately enqueue delayed reattachment. The
five test-owned retry locations were stopped through the supported per-location
CLI, without numeric detach or stop-all. Automatic production retry cleanup
remains open and needs an exact retirement-bound lifecycle integration; a
single early cancellation before native teardown finishes is not sufficient.

## b80: production retry cleanup

The above open item is implemented at the production attach boundary. An
authenticated, exact registration arms a cleanup record before native attach;
the record cannot run until registration retirement AND native operation
completion. Request cancellation or a returned numeric port does not grant
cleanup authority. Failed/cancelled activation and stream-triggered retirement
use the same registration cancellation signal.

The native operation is the documented usbip-win2 0.9.7.7
`STOP_ATTACH_ATTEMPTS` (0x0022e014): a 1,104-byte request/response with the exact
`localhost`, server TCP port and never-reused production export alias. It is
not a port detach, stop-all, persistent-settings change or driver patch. The
driver internally hashes this location; this API is not an atomic owner-token
detach guarantee. Command-mode initial attach also requests `--once`.

An off-input-path sweep handles delayed enqueue. A zero count is not proof
that native retirement has finished: a compact server-lifetime tombstone
also recognizes later failed imports of that same retired alias and schedules
another coalesced cleanup. Unknown aliases and live registrations cannot arm
or trigger native cleanup. Tombstones are capped at 65,536 and retain export
metadata and a cancellation channel, not whole retired devices/buffers.
Errors remain warnings; native cancellation always joins actual operation
completion before releasing pinned memory or its handle.

Tests cover exact-location ABI, forbidden empty/numeric targets, native
completion/cancellation ordering, active/stale registration rejection and a
retry arriving after an earlier zero-count sweep. Full Go and race suites pass.
This service-lifetime cleanup cannot survive force-killing the broker itself;
it does not claim orphan cleanup across process crashes or Windows reboot.

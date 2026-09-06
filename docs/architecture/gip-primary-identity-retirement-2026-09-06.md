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

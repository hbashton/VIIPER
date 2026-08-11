# Native UDE package installation transaction

The native UDE release is installed through one fail-closed composition
transaction. `viiper native-package-install` is a hidden bootstrapper boundary;
users enter it only through a signed DS4Windows/VIIPER installer that embeds the
reviewed SHA-256 values. It cannot turn a CI test-signed package into production
media. The driver must first satisfy the production HLK/WHCP contract in
[`native-udecx-signing.md`](native-udecx-signing.md).

## Trust inputs

The signed bootstrapper supplies all of the following as immutable build data:

- the exact VIIPER broker, `ViiperUdeCtl.exe`, and reviewed production-manifest
  SHA-256 values;
- the reviewed 40-64 hexadecimal source revision;
- the runtime driver directory containing only the Microsoft-returned INF,
  SYS, and CAT, plus the source-bound HLK/WHCP manifest; and
- the target interactive-user SID whose legacy startup ownership may be
  migrated.

Before its first mutation, the command holds non-write-shared and
non-delete-shared handles to the broker, helper, manifest, INF, SYS, and CAT,
plus every local directory ancestor used to reopen those paths. It rejects
reparse points, hard links, ancestor replacement, extra package files, hash changes,
noncanonical INF contracts, non-production manifests, and packages that do not
pass the helper's read-only signature/catalog verification. The helper proves
that the exact adjacent `ViiperUde.inf` and `ViiperUde.sys` are both members of
the exact adjacent Microsoft catalog under Windows driver policy before any
SetupAPI mutation.
Production checks
the actual catalog signer certificate for Windows Hardware Driver Verification
EKU and explicitly rejects the attestation EKU; the publisher display name is
not sufficient. The helper repeats
the installer-embedded manifest SHA-256 check before both verification and
installation. Paths are passed as arguments, never through a command shell.

The certification/intake artifact still contains the PDB, and the intake gate
binds that PDB to the same manifest. The public runtime bundle omits it: the
installer pins the validated manifest hash and rechecks the unchanged INF,
while Windows consumes only INF/SYS/CAT. Debug symbols therefore remain part
of the source-provenance evidence without becoming a user-machine dependency.

## Commit order

1. Acquire the administrator-only machine package mutex and validate every
   immutable input.
2. Inspect `VIIPERNativeBroker` without changing it. A protected canonical
   service and executable become an exact rollback source. An exact service
   name whose executable is restricted to one of VIIPER's managed Program
   Files layouts, but whose service/image ACL is weak or stale, is stopped and
   deleted; its unsafe ACL is never repaired in place or restored.
3. Create `%ProgramFiles%\VIIPER` with the canonical protected ACL, or require
   an existing directory to already have that exact ACL. Write a random sibling
   staging file with the exact executable ACL, flush it, verify its SHA-256 and
   single-link identity, retain the previous broker under a random protected
   rollback name, and publish with `MoveFileExW(..., WRITE_THROUGH)`. The outer
   transaction also creates and holds a random one-time token with an
   administrator/SYSTEM-only DACL and passes its installer-bound SHA-256 to the
   helper.
4. `ViiperUdeCtl verify` repeats source/package verification without mutation.
   `ViiperUdeCtl install` then retains its in-memory pre-install DriverStore and
   devnode snapshot while launching the staged broker's hidden
   `native-package-broker-commit` command. That command reopens the immutable
   token, requires its exact DACL/hash/path, and proves the package mutex is
   still owned by the separate outer process before it may enter the normal
   broker service transaction.
5. The broker transaction creates or updates the LocalSystem service, rotates
   its protected credential, starts it, and requires authenticated `ping`
   identity, `Ready=true`, ABI 1.8, and the exact negotiated capability mask.
   Only then does it disable legacy Run/task/process ownership, and it
   authenticates again before returning success.
6. The helper commits the driver only after that broker proof. A broker failure
   first rolls back SCM, credential, and legacy state inside the broker, then
   restores the prior driver packages/devnode inside the still-running helper.
   The outer command finally restores the old broker image and prior service
   run-state. It never removes USB/IP directly.

The mutating broker process is never hard-terminated. The outer absolute
four-minute deadline is passed through the helper into the nested broker, so it
does not receive a fresh budget after driver mutation. The broker owns a
separately bounded rollback and unwinds cooperatively; the helper retains the
driver snapshot and polls only through that explicit rollback ceiling. If the
child violates both bounds, the helper reports an indeterminate rollback and
does not race the still-owning child with a second driver rollback. The outer rollback
uses its own non-canceled two-minute context. Synchronous SetupAPI work is
checked immediately before and after each mutating boundary; no new phase may
start after expiry, and no process is killed mid-rollback.

## Reference-backed Windows invariants

- The machine transaction lock uses a private namespace bounded to the local
  Administrators SID and a protected SYSTEM/Administrators DACL. It then waits
  on the mutex and owns it until `ReleaseMutex`; object existence is not lock
  ownership, and `WAIT_ABANDONED` triggers a fresh inventory before mutation.
  This follows Microsoft's [private namespace](https://learn.microsoft.com/windows/win32/sync/object-namespaces)
  and [mutex wait](https://learn.microsoft.com/windows/win32/sync/using-mutex-objects)
  contracts and prevents an unelevated process from pre-creating the machine
  lock name.
- Native ABI health opens the UDE control interface for overlapped I/O. A
  pending `DeviceIoControl` is waited only until the absolute transaction
  deadline; timeout calls `CancelIoEx` and drains the exact `OVERLAPPED` before
  rollback continues. This is the documented [overlapped DeviceIoControl](https://learn.microsoft.com/windows/win32/api/ioapiset/nf-ioapiset-deviceiocontrol)
  and [CancelIoEx](https://learn.microsoft.com/windows/win32/fileio/cancelioex-func)
  lifetime rule.
- SetupAPI rollback preserves the captured root device instance ID. Per
  [`SetupDiCreateDeviceInfoW`](https://learn.microsoft.com/windows/win32/api/setupapi/nf-setupapi-setupdicreatedeviceinfow),
  omitting `DICD_GENERATE_ID` makes `DeviceName` the complete instance ID;
  generated IDs are used only for a first-time forward install. Rollback
  reconciles and verifies the captured identity, topology, and signed package
  hash rather than deleting every matching devnode and manufacturing a
  replacement.
- ViGEmBus's root-enumerated bus architecture is used only as the lifecycle
  reference: the bus owns its exact child identities and separates user-mode
  submission from PnP mutation. usbip-win2 remains an untouched legacy
  fallback until authenticated native health succeeds; package rollback never
  treats its service or driver store as installer-owned.

Every public native install, repair, or uninstall takes locks in the same
machine-wide order: package mutex, then broker-service mutex. The nested helper
callback does not reacquire the package mutex (which would deadlock); the
protected one-time token and zero-time ownership check authorize that one
service transaction. The token is removed on commit or rollback and is inert
without a live outer mutex owner. Because Win32 mutexes are thread-owned, each
Go acquisition pins its goroutine to that OS thread until the matching release;
scheduler migration cannot strand either global lock.

## Restart boundary

If Windows reports that driver activation requires a restart, the helper does
not start the broker or remove legacy ownership. It rolls the attempted driver
transaction back, the outer transaction restores the prior executable/service,
and preserves Windows `ERROR_SUCCESS_REBOOT_REQUIRED` (3010) through the Go
bootstrapper for the signed installer. After restart, the installer
retries the complete preflight and transaction from the beginning. No
cross-reboot journal is trusted as executable authority.

## Deterministic gates

The normal Go suite runs a failpoint matrix for every transaction phase,
including partial preparation, authenticated-health failure, commit failure,
caller cancellation, rollback failure, and close failure. A source-contract
test requires the immutable-input locks, read-only helper verification,
protected ACLs, weak-service delete/recreate path, atomic publication, inner
driver/broker rollback, protected nested-commit token, global lock ordering,
and authenticated proof. It also rejects hard process
termination, context-killed helper processes, recursive deletion, or direct
legacy/USB-IP removal in the outer layer.

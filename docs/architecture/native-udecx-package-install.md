# Native UDE package installation and removal transactions

The native UDE release is installed through one fail-closed composition
transaction. `viiper native-package-install` is a hidden bootstrapper boundary;
users enter it only through a signed DS4Windows/VIIPER installer that embeds the
reviewed SHA-256 values. It cannot turn a CI test-signed package into production
media. The driver must first satisfy the production HLK/WHCP contract in
[`native-udecx-signing.md`](native-udecx-signing.md).

Production removal enters through `viiper uninstall`. The signed installer must
pass the packaged `ViiperUdeCtl.exe` path, its installer-bound SHA-256, and the
interactive-user SID. A direct Windows uninstall without those immutable helper
inputs fails before mutation; the command does not fall back to deleting only
the broker and leaving the devnode or Driver Store package behind.

## Trust inputs

The signed bootstrapper supplies all of the following as immutable build data:

- the exact VIIPER broker, `ViiperUdeCtl.exe`, production manifest, INF, SYS,
  and CAT SHA-256 values;
- the reviewed exact 40- or 64-hexadecimal source revision;
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
2. Create and hold a random one-time token below `%ProgramFiles%\VIIPER` with
   an administrator/SYSTEM-only DACL, and pass its installer-bound SHA-256 to
   `ViiperUdeCtl install`. The helper independently reopens and verifies the
   source manifest and all three runtime driver hashes, acquires its private
   driver mutex, and snapshots the exact Driver Store/devnode topology.
3. Classify the driver under that mutex. Exact package bytes plus an exact
   started binding cause no SetupAPI mutation. Exact bytes with missing,
   stopped, or stale topology select the already-published driver for the exact
   devnode and call `DiInstallDevice`; they never replace same-version Driver
   Store content. An absent or newer candidate uses `DiInstallDriverW` under the
   monotonic version policy. Same-version INF/SYS/CAT conflicts and implicit
   downgrades fail before mutation.
4. After the exact binding is verified, the helper launches the immutable
   package broker's hidden `native-package-broker-commit` command while still
   holding the driver mutex and snapshot. That command reopens the token,
   requires its exact DACL/hash/path, proves the separate outer process still
   owns the package mutex, then acquires the broker-service mutex.
5. The nested command first checks for a true no-op: canonical protected
   service/image/credential state, no live legacy owner, stable service PID, and
   authenticated `ping` with `Ready=true`, ABI 1.10, the exact capability mask,
   package version, and loaded-kernel build identity. If any part is unhealthy,
   it transactionally publishes the exact broker through a flushed protected
   sibling, creates or repairs the LocalSystem service, rotates its credential,
   and repeats authenticated health before and after removing legacy
   Run/task/process ownership. A weak pre-existing service is deleted and
   recreated; its unsafe ACL is never repaired in place or restored.
6. The child emits one newline-terminated canonical result. A broker failure
   first rolls back SCM, credential, and legacy state, then restores the prior
   broker image and run-state inside the child. Only a pre-mutation proof or a
   fully settled child rollback authorizes the still-running helper to restore
   its captured driver packages/devnode. Crash, malformed/missing proof, exit 3,
   pipe/wait ambiguity, or an over-budget child leaves driver rollback
   unauthorized and reports external reconciliation. USB/IP itself is never
   directly removed by this transaction.

The mutating broker process is never hard-terminated. The outer absolute
four-minute deadline is passed through the helper into the nested broker, so it
does not receive a fresh budget after driver mutation. The broker owns a
separately bounded rollback and unwinds cooperatively; the helper retains the
driver snapshot through a three-minute post-deadline ceiling that covers the
45-second inner service rollback plus the outer non-canceled two-minute image
rollback. Even after that ceiling it retains the driver mutex until the child
actually exits, then reports an indeterminate result rather than racing the
child with a second driver rollback. Synchronous SetupAPI work is
checked immediately before and after each mutating boundary; no new phase may
start after expiry, and no process is killed mid-rollback.

Before calling Go's `Cmd.Wait`, the outer transaction duplicates the exact
helper process handle with `SYNCHRONIZE`. It independently waits for that
process object to become signaled before releasing the package mutex or any
immutable input handle. A non-exit `Cmd.Wait` error is therefore still
indeterminate, but it can no longer let a live mutating helper escape the
transaction scope.

## Exact package removal

Removal is a separate fail-closed composition transaction; it does not reuse
the historical broker-only uninstall routine.

1. Acquire the package mutex and then the broker-service mutex. Lock every local
   ancestor of the packaged helper, hold its leaf handle without write/delete
   sharing, and require the installer-bound SHA-256, one-link identity,
   non-reparse identity, and PE header.
2. Inventory only the exact `VIIPERNativeBroker` name. A service is eligible to
   stop only when its LocalSystem configuration, arguments, service DACL,
   recovery policy, credential path, and managed Program Files executable are
   canonical. The executable, credential, and protected directory chains remain
   locked and hash-snapshotted. A running broker's optional log is first held by
   file identity with sharing compatible with its trusted writer. A same-named
   weak, non-LocalSystem, or non-managed service fails preflight and is never
   adopted or deleted.
3. Stop the exact trusted service but keep its SCM registration, credential, and
   managed files intact. Before launching the helper, upgrade any live-log probe
   to a non-write-shared delete handle and require the same volume/file identity,
   one-link state, non-reparse identity, and a stable hash. Launch
   `ViiperUdeCtl remove` with the outer absolute
   deadline. The Go parent never uses a context-killed process or hard
   termination after launch, and it applies the same retained-process join as
   install before releasing its service/package scope. The helper checks the
   cooperative deadline before
   and after each SetupAPI boundary and owns a separate two-minute cooperative
   rollback ceiling. If the live-log identity cannot be upgraded exactly, the
   helper is not launched and the broker stays stopped rather than reopening an
   ambiguous LocalSystem-managed path.
4. Accept only one structured helper outcome whose process and reported exit
   codes agree. Exit 0 is verified final success. Exit 3010 is verified Windows
   reboot-success. A preflight rejection proves no driver mutation; exit 1 with
   `rollback=succeeded` proves that the exact captured package/devnode topology
   was restored. A no-reboot result in those two failure classes permits the
   exact prior broker run-state to be restored after its locked service/files
   are revalidated. Rollback that still requires a reboot preserves the files
   but leaves the service stopped until Windows can settle the binding.
   Exit 3, a crash, a missing/malformed proof, or any ambiguous wait cannot prove
   a safe binding, so the broker remains stopped and the command reports that
   external reconciliation is required.
   Before mutation, an exact three-file INF/SYS/CAT rollback tree is placed
   below the non-reparse Windows temporary directory in a cryptographically
   unpredictable location. Every directory and file is created with, and then
   verified against, an explicit protected Administrators/LocalSystem-only ACL;
   payload writes are write-through, explicitly flushed, signature/hash
   revalidated, and locked against write/delete sharing. A canonical journal
   binds every captured devnode to one package index and every package to its
   relative backup paths and exact hashes. It is written to a private temporary
   name, flushed, atomically published with a write-through rename, reopened,
   ACL/byte verified, and flushed again before the first SetupAPI mutation.
   Journal presence means manual reconciliation may be required; it never
   authorizes automatic restoration. Preservation is armed before mutation and
   therefore survives C++ exceptions or process failure. It is disarmed only
   after explicit, verified deletion following either committed removal or a
   verified rollback; cleanup failure is surfaced and retains the journal and
   backup tree. A backup-preparation failure whose tree cannot be deleted emits
   the retained root and planned journal path with `recoveryRecordWritten=0`.
   The allocation-free exception outcome separately tracks whether transaction
   mutation actually started: pre-mutation exceptions remain exit 4 with
   `changed=0`, while post-mutation exceptions require exit 3 reconciliation.
5. Only after exit 0 or 3010 does cleanup revalidate and delete the exact service,
   credential, broker log, and installer-owned broker images. Deletion uses the
   retained file identities rather than a second untrusted path lookup. It does
   not recursively delete either managed directory, and it never enumerates or
   changes unrelated devnodes, Driver Store packages, files, scheduled tasks,
   Run registrations, processes, or USB/IP state. A repeat after partial cleanup
   safely reconciles exact protected leftovers; complete service/driver absence
   is idempotent. If the uninstalling process is itself the exact locked broker
   image and Windows will not mark the mapped image for immediate deletion, the
   transaction proves that identity by volume/file ID, schedules only that
   protected path with `MoveFileEx(..., MOVEFILE_DELAY_UNTIL_REBOOT)`, and folds
   the result into exit 3010.
   A retry that finds the exact service already marked for deletion waits under
   the same transaction deadline, then continues from the proven-absent service
   state and reconciles only retained exact files.

This ordering follows Microsoft's separation between
[`DiUninstallDevice`](https://learn.microsoft.com/windows/win32/api/newdev/nf-newdev-diuninstalldevice),
which removes a selected devnode and its child topology, and
[`DiUninstallDriverW`](https://learn.microsoft.com/windows/win32/api/newdev/nf-newdev-diuninstalldriverw),
which removes a specified package from devices and then the Driver Store. Both
APIs return a `NeedReboot` result; the caller must aggregate that result while it
finishes its other required uninstall operations. VIIPER therefore preserves
3010 only after exact owned cleanup has reconciled. The
[usbip-win2 uninstall sequence](https://github.com/vadimgrn/usbip-win2#uninstallation-of-usbip)
is used only as the devnode-before-package lifecycle reference, while
[ViGEmBus releases](https://github.com/ViGEm/ViGEmBus/releases) are used only as
the root-bus installer lifecycle reference. Neither product's broad package or
registration cleanup is treated as VIIPER ownership authority.

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
  forward creation passes the VIIPER-owned `VIIPERUDE` device name with
  `DICD_GENERATE_ID` and verifies the returned `ROOT\VIIPERUDE\####` identity.
  Rollback omits `DICD_GENERATE_ID`, making `DeviceName` the complete captured
  instance ID. It accepts only that namespace or the exact legacy
  `ROOT\USB\####` form produced when older builds incorrectly passed the USB
  class name, after the existing service/package ownership proof. It then
  verifies the restored identity, topology, and signed package hashes rather
  than deleting every matching devnode and manufacturing a replacement.
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

For removal, 3010 means the helper accepted the exact devnode/package removal
but Windows needs a restart to finish it. The service and exact managed
ownership are cleaned first, then 3010 is returned. A retry before or after the
restart performs a fresh exact inventory and is idempotent; no pending-removal
journal is trusted as authority. If owned cleanup itself fails, the command
reports failure (including that Windows still requires restart) rather than
misrepresenting a partial uninstall as 3010 success.

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

The removal matrix independently covers both mutex acquisitions, immutable
preflight, service inventory, partial stop, helper launch/outcome, exact cleanup,
restore failure, close failure, 3010, structured preflight, verified rollback,
unverified rollback, malformed proof, idempotent absence, and exact ownership.
The targeted matrix is also run repeatedly to catch state leakage and ordering
regressions.

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
   immutable input. Then acquire the broker-service mutex, inspect the exact
   service/configuration/DACL/recovery/image/run-state, and retain a protected
   hash snapshot of any trusted prior broker. This is the global
   package-then-service lock order.
2. Create and hold a random one-time token below `%ProgramFiles%\VIIPER` with
   an administrator/SYSTEM-only DACL, and pass its installer-bound SHA-256 to
   `ViiperUdeCtl install`. The helper independently reopens and verifies the
   source manifest and all three runtime driver hashes, acquires its private
   driver mutex, and snapshots the exact Driver Store/devnode topology. Before
   its first SetupAPI or broker mutation, it creates the protected fixed
   `%ProgramData%\VIIPER\UdeCx\Transactions\active-v2` journal, copies and
   revalidates immutable prior/candidate recovery material, and publishes the
   first write-through record in a bounded canonical SHA-256 chain. Every later
   staging, quiescence, binding, rollback, reboot, and broker handoff cut point
   is durably appended before the next mutation. Startup admission reconciles
   this journal from exact current state; unknown transitions, identities,
   package inventories, or partial topology fail closed and retain evidence.
3. Classify the driver under that mutex. Exact package bytes plus an exact
   started binding cause no SetupAPI mutation. Same-version INF/SYS/CAT
   conflicts and implicit downgrades fail before mutation. A missing candidate
   is add-only staged with `SetupCopyOEMInfW`; the helper validates its returned
   published name, bytes, catalog, signer, and complete package inventory, then
   proves that staging did not alter the captured root. Only after that proof
   does it signal the inherited quiescence request. The outer transaction stops
   only a trusted broker while retaining the broker-service mutex. Weak service
   ownership aborts because it is not a safe rollback source. After quiescence,
   the helper re-enumerates global topology, repeats exact package/root-byte and
   pristine-runtime proofs, prepares the compatible-driver list, and switches
   only the captured devnode in place with `DiInstallDevice`. There is no
   forward remove/recreate gap. If the captured topology had no root, the helper
   obtains the generated instance ID, durably records that exact receipt before
   setting its hardware ID or registering it, and can therefore reconcile a
   crash-partial root without adopting a lookalike. The captured snapshot and
   exact staging receipt remain authoritative until broker commit; rollback
   restores the prior binding before removing only a package proved staged by
   this transaction.
4. After the exact binding is verified, the helper signals its inherited broker
   handoff event. The outer transaction releases its protected prior-image and
   SCM handles, then releases the broker-service mutex on the same pinned OS
   thread. Only then does the helper launch the immutable package broker's
   hidden `native-package-broker-commit` command while still holding the driver
   mutex and snapshot. That command reopens the token, requires its exact
   DACL/hash/path, proves that the separate outer process still owns the package
   mutex, then acquires the broker-service mutex. An exact driver no-op skips
   service quiescence but uses the same handoff before broker health or repair.
5. Before its first broker mutation, the nested command builds all rollback
   material below a protected
   `%ProgramData%\VIIPER\BrokerTransactions\preparing-<transaction>` directory.
   Its bounded canonical snapshot binds the outer token, candidate and prior
   image, exact service state, target SID, and encrypted prior credential and
   legacy-registration artifacts. Only after every file is flushed, reopened,
   hash-verified, and protected does an atomic no-replace rename publish the
   fixed `active-v1` journal. A write-through SHA-256 chain then records intent
   and return phases around service stop, atomic image replacement, legacy-owner
   stop, credential rotation, SCM configuration, start, authentication, legacy
   removal, and reauthentication.
6. The nested command accepts a true no-op only when the protected
   service/image/credential state is canonical, no legacy owner is live, the
   service PID is stable, and authenticated `ping` proves `Ready=true`, ABI
   1.14, the exact capability mask, package version, controller session and
   instance identities, and loaded-kernel build
   identity. Otherwise it performs the journaled repair. Exact forward health
   ends at durable `nested-ready`; it does not delete rollback material or claim
   outer success. A broker failure restores SCM, credential, image, legacy
   state, and prior run-state in dependency order before recording an exact
   rollback result. Missing, malformed, ambiguous, or corrupt evidence latches
   manual reconciliation and never authorizes an independent driver rollback.
7. Driver and broker success settle through a two-phase receipt. The helper
   durably records `BrokerOuterSettlementPending` and emits one canonical
   binding containing both transaction IDs, both pending journal digests, the
   candidate and outer-token identity, a fresh settlement nonce, and the
   protected request hash. The Go parent revalidates live forward state, records
   `outer-settlement-pending`, atomically publishes the protected request, and
   calls the hash-pinned helper's `broker-settlement-ack` command while it still
   owns the package mutex. The helper authenticates both journals and the
   request, records `BrokerOuterSettled`, atomically retires its active journal
   to an exact settled tombstone, and returns the final driver digest. Go binds
   that receipt into `outer-settled`, publishes the protected final receipt,
   atomically retires its journal, and only then asks the helper to atomically
   rename the driver tombstone to an inert discarding name. Recursive deletion
   is best-effort after those authoritative renames.
8. Every ordinary process or power cut re-enters the same transaction. A
   protected `nested-ready`, pending settlement, active final state, or settled
   tombstone is replayed idempotently using the original token and journal
   identities; a new broker child is not started while old settlement exists.
   The caller retries its requested new transaction only after the old one is
   exactly settled. A pre-mutation proof or fully settled child rollback is the
   only authority for restoring the captured driver packages/devnode. If the
   outer transaction stopped a trusted prior broker and failure occurred before
   handoff, it restores the exact snapshot only after settled driver proof.
   After handoff, indeterminate proof leaves the service stopped and preserves
   both journals for reconciliation. The legacy transport itself is never
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
helper process handle with `SYNCHRONIZE`. It waits on that retained process
object together with the two child-to-parent coordination events, and passes
only four unnamed, explicitly inherited events to the exact helper process.
The process signal has priority over stale event observations. The retained
process must become signaled before the package mutex or any immutable input
handle can unwind. A non-exit `Cmd.Wait` or event-wait error is therefore still
indeterminate, but it can no longer let a live mutating helper escape the
transaction scope or authorize an unsafe prior-service restart.

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
   Before mutation, the helper creates the fixed protected
   `%ProgramData%\VIIPER-UdeCx-RemoveTransactions\active-v2` recovery root and
   stores the exact prior devnode plus every INF/SYS/CAT package in immutable
   write-through backups. A canonical, bounded, append-only SHA-256 chain binds
   those backups, boot/reboot epochs, and device/package entered, returned, and
   committed cut points. Every directory and file is non-reparse, single-link,
   Administrators/LocalSystem-only, explicitly flushed, reopened, byte-compared,
   and held against write/delete sharing. Startup recovery uses exact raw-root,
   package-inventory, and cut-point authority to finish removal or restore the
   prior state; unknown, mixed, or concurrent topology latches manual
   reconciliation without broad mutation. Terminal validation releases evidence
   handles, atomically renames `active-v2` to a transaction-bound settled
   tombstone, proves active admission absent, and only then makes deletion
   best-effort. A retained tombstone is a successful but explicitly surfaced
   cleanup warning, never hidden evidence loss. Preservation is armed before
   mutation and survives exceptions, process failure, power loss, and
   reboot-required SetupAPI returns.
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
  3010 only after exact owned cleanup has reconciled. The transaction follows
  the generic Windows devnode-before-package lifecycle and root-bus ownership
  model; no third-party package, registration, service, or cleanup convention is
  treated as VIIPER ownership authority.

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
- SetupAPI upgrade and rollback preserve the captured root device instance ID.
  Upgrade add-only stages the exact candidate before broker quiescence, then
  performs an exact selected-driver switch on the captured devnode. Root
  creation is needed only when no prior root exists. Per
  [`SetupDiCreateDeviceInfoW`](https://learn.microsoft.com/windows/win32/api/setupapi/nf-setupapi-setupdicreatedeviceinfow),
  forward creation passes the VIIPER-owned `VIIPERUDE` device name with
  `DICD_GENERATE_ID` and verifies the returned `ROOT\VIIPERUDE\####` identity.
  Rollback recreation omits `DICD_GENERATE_ID`, making `DeviceName` the complete
  captured instance ID. It accepts only that namespace or the exact legacy
  `ROOT\USB\####` form produced when older builds incorrectly passed the USB
  class name, after the existing service/package ownership proof. The helper
  then verifies the restored identity, topology, and signed package hashes rather
  than deleting every matching devnode and manufacturing a replacement.
- The root-enumerated bus owns its exact child identities and separates
  user-mode submission from PnP mutation. The configured legacy transport
  remains untouched until authenticated native health succeeds; package
  rollback never treats its service or Driver Store packages as installer-owned.

Every public native install, repair, or uninstall takes locks in the same
machine-wide order: package mutex, then broker-service mutex. During install,
the outer transaction retains both across any required broker stop and driver
replacement, then performs an explicit service-lock handoff to the nested
broker commit while continuing to own the package mutex. The nested callback
does not reacquire the package mutex (which would deadlock); the protected
one-time token and zero-time ownership check authorize that one service
transaction. Once handoff occurs, an unsettled helper exit closes but preserves
the exact token so journal replay can prove the original outer identity. It is
deleted only after exact rollback or completed two-phase forward settlement and
is inert without the matching package-mutex owner. A later outer package run
may reconcile that old transaction under the same package-to-service lock order
but must return retry instead of beginning a new child in the same admission.
Because Win32 mutexes are thread-owned, each Go acquisition pins its goroutine
to that OS thread until the matching release; scheduler migration cannot strand
either global lock.

## Restart boundary

The normal newer-package path add-only stages and verifies the candidate before
quiescence, then switches only the captured root in place. Every SetupAPI return
that requests a restart is durably recorded with the boot identifier that
produced it before control can leave the mutation boundary. On that same boot,
reconciliation returns the pending restart without repeating the mutation,
starting the broker, removing legacy ownership, or retiring recovery evidence.
The production composition attempts exact driver rollback when a forward
activation requests a restart; if restoring the prior binding also needs a
restart, the journal enters `RestoreRebootPending` and the prior broker remains
stopped. A changed forward state that has not completed broker settlement is an
unsettled reconciliation result, never a success-shaped 3010.

After Windows crosses the recorded boot boundary, the helper treats current
root/package/service state as authority and the protected journal as the narrow
ownership receipt. It revalidates exact bytes, topology, phase history, and
reboot epoch before finishing forward, finishing rollback, or latching manual
reconciliation. A new install cannot start while `active-v2` or a pending
cross-journal broker settlement exists. Only an exact terminal state is
atomically retired; rerunning the signed installer then starts a fresh
transaction if the requested update still remains.

Removal uses the same rule. Device and package API returns record their fresh
restart bit and generating boot before any subsequent step. Same-boot recovery
does not repeat a pending device removal or mutate packages. After a later boot,
the raw root namespace and exact package inventory must prove either the
expected removed prefix or the exact rollback state before work continues.
Forward 3010 authorizes only the cleanup associated with that exact committed
removal; rollback 3010 preserves the prior managed files and leaves the service
stopped until restoration settles. A crossed restart that still exposes an
indeterminate pending root, any extra package/root, or any mismatched epoch
latches manual reconciliation instead of requesting restart forever. Cleanup
failure is reported separately and never turns a partial uninstall into
terminal success.

## Deterministic gates

The normal Go suite and compiled helper self-test run a failpoint matrix for
every driver, broker, cross-journal settlement, rollback, reboot, and retirement
phase. Coverage includes partial protected preparation, every atomic record
publication cut, authenticated-health failure, child exit before parent proof,
both sides of the settlement acknowledgement, final-receipt publication,
active-to-settled and settled-to-discarding renames, caller cancellation,
rollback failure, and retained cleanup. Source contracts require immutable
input locks, read-only helper verification, protected ACLs, exact service
ownership, atomic image publication, exact package/root mutation, nested token
binding, global lock ordering, and both-journal authenticated proof. They reject
hard process termination, context-killed helper processes, in-place recursive
deletion of authoritative evidence, or direct legacy-transport removal in the
outer layer.

The removal matrix independently covers both mutex acquisitions, immutable
preflight, service inventory, partial stop, helper launch/outcome, exact cleanup,
restore failure, close failure, every 3010 cut and boot epoch, structured
preflight, verified and unverified rollback, malformed proof, concurrent root or
package appearance, idempotent absence, evidence-lock release, tombstone cleanup
warnings, and exact ownership. The targeted matrices are also run repeatedly to
catch state leakage and ordering regressions.

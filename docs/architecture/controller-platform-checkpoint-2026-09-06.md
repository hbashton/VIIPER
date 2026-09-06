# Controller platform source checkpoint, 2026-09-06

This is a source-preservation checkpoint on `feature/native-udecx-landing-zone`,
not a release tag or a claim that the remaining hardware/game acceptance matrix
is complete. It collects the accumulated Xbox One retained-USB, authentication,
feedback, lifecycle and Switch 2 runtime-status work with its tests and dated
provenance/validation ledgers. The matching DS4Windows checkpoint contains the
physical controller integration and canonical mapper/output composition.

## Verification at checkpoint

- A fresh `go test -count=1 ./...` using Go 1.27.0, Windows amd64 and
  `CGO_ENABLED=0` exited successfully: 33 packages passed; 23 had no test files.
  Local evidence: `viiper-checkpoint-full-20260906.log` in the portable lab.
  This does not run opt-in `_testing` hardware harnesses or replace race tests.
- All 603 entries in the preview.70 broker's source manifest were compared with
  the working tree. Only `.gitignore` and the September 4 validation ledger
  differed. The additional multi-instance Xbox USB regression is test-only;
  `_testing` harnesses and editor files were outside that packaging manifest.
- Generated authentication harness `bin`, `obj` and Rust `target` directories
  are excluded from Git. The checked-in `synthetic.key.txt` is the documented
  public test literal, verified against the harness's required value; no real
  deployment key or generated peer executable is part of this checkpoint.
- Staged whitespace checks and common private-key/token signature scans passed.
  These are repository hygiene checks, not a comprehensive security audit.

The existing DS4Windows b74 canonical and exact-published suites each passed
3,738 tests with zero failures and three explicitly opt-in live-audio skips.
Before checkpoint-only whitespace/documentation edits, all 1,054 files in its
pre-packaging source manifest matched byte for byte. See the
[September 4/5 ledger](controller-platform-validation-2026-09-04.md) for the
original failing allocation evidence, measured runtime-accounting diagnosis,
strict test-only allocation scopes, hardware boundaries and installer hashes.
No new .NET full-suite run is claimed by this checkpoint.

## Open boundaries

The user's multi-controller **displayed report-interval** increase remains an
unattributed performance issue. Shared keyboard/mouse synchronization and the
shared Bluetooth adapter are candidates, not established causes. No new
hot-path optimization or end-to-end latency result is claimed here.

The current delivery remains an unsigned personal preview; the documented
public-binary dependency-licensing and fresh combined-build hardware acceptance
gates remain. Checkpointing source does not publish a new installer, certify
Joy-Con/Pro feature parity, complete the native backend, or alter installed apps.

## User-authorized artifact cleanup

The second cleanup removed 18,022 obsolete binary/dependency files and three
duplicate Desktop installers included in that count: 17,689,747,472 logical bytes
(16.475 GiB). A verified 496,143,854-byte deduplicated archive preserves 79 unique
historical core application binaries; net logical recovery is 16.013 GiB before
small audit manifests. C: free space measured 46.297 -> 62.784 GiB during deletion
(the archive already existed at the first measurement). Filesystem free-space
observations may include unrelated activity.

All 5,150 protected source/profile/evidence/current-build hashes matched after
deletion. Program Files, Codex history/databases, current b74 and complete b67/b68
portable fallbacks were untouched. Older b1-b66 directories retain profiles and
evidence but are no longer complete runnable builds. Historical dependency
payloads require rebuild/re-extraction; installer duplicates have hash-identical
retained copies. There is no Recycle Bin backup. The local audit is portable lab
`evidence/cleanup-second-20260906`, including exact file and recovery manifests,
the core archive, deletion journal and `result.json`.

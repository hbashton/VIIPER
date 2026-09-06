# Controller platform validation status, 2026-09-03

Continuation: [September 4 evidence](controller-platform-validation-2026-09-04.md)
establishes and repairs the recurring USB counter disconnect, records actual
Go↔DS4Windows auth interoperability, and stages the matched portable b50 pair.
The session details below describe their historical observation time.

This is a dated evidence snapshot, **not a completion/release certificate**.
The full source-prompt feature, physical-source/virtual-target, wireless,
feedback-fidelity, game, latency and installer gates remain unchanged.
See the September 2 ledger for b43 tactile results and b45–b49 lifecycle work.

## Portable session preserved

- DS4Windows PID 27892 and VIIPER PID 27880 remain the b48 typed-removal pair
  under the Desktop lab. No replacement/restart, controller output pulse,
  Bluetooth association, driver/installer action, or system setting change was
  performed in this continuation. The current source is **not** the running
  b48 binary. b49 is still an unlaunched historical candidate.
- A read-only PnP query found five present VID_057E/PID_2069 USB nodes. The app
  log last recorded an attached Switch 2 Pro with the Xbox One lab profile and
  90% battery. This does not independently prove current virtual-pad readiness.
- Log lifecycle events occurred at 23:55:32 → 23:55:35 on September 2 and
  00:19:24 → 00:19:26 on September 3, following an earlier 23:31 event. There is
  no user-confirmed unplug corresponding to these events; do not label them
  successful surprise-unplug tests or attribute them to the user. Their cause
  remains unestablished. The repeated roughly 24-minute spacing merits follow-up.
- The user was told to leave the controller as it is and not enter pairing
  mode while source validation proceeds. A controlled unplug/UI-cleanup test
  remains pending. No repeated input request was sent during offline work.

## AUTH-V1-DIRECTION-REUSE: coordinated source repair

The independent review and parent reproduction established an inherited
bidirectional ChaCha20-Poly1305 nonce collision in auth v1. This concerns the
DS4Windows/VIIPER API, not Xbox console security or Nintendo pairing, and is
**not an established explanation for b48's isolated readiness-probe failure**.

Implemented v2 together in Go server/clients, DS4Windows, and the four generated
auth templates: explicit roles, directional nonce domains 0/1, exact counters,
version-separated magic/HMAC/session contexts, no fallback, no deployment-key
format change. Go and C# reject record faults without resuming the stream;
partial encrypted reads no longer report ciphertext bytes as plaintext.
Production buffer ownership is still independent per direction. No USB/GIP
descriptor, controller rate, mapper queue, or driver pacing changed here.

Generated-client fixes required by the migration:

- C#: read/write gates, complete-frame serialization, checked lengths/counters,
  proper partial-header EOF handling, bounded rejected-handshake diagnostics.
- TypeScript: bigint counters, opposite-domain validation, fail-closed frames,
  exact handshake reads retaining surplus bytes, backpressure-aware EOF,
  bounded record lengths. A short non-null Node read at EOF is now rejected.
- Rust: strict sync/async receiving, short-record safety, overflow checks,
  pending ciphertext ownership across partial writes; every high-level async
  request/send boundary flushes. The writer's default transport is unchanged;
  a generic inner writer permits deterministic partial/Pending tests.
- C++: v2 contexts and nonce domains, checked shared receive path, retained
  record tails, serialized frames, counter fences, OpenSSL-generated nonces.
  **Compilation and runtime tests still pending.**

Independent adversarial review found three concrete generated-client defects
during development (Rust final-record stall; TypeScript EOF/backpressure;
short handshake read). All three are repaired in source and targeted tests
now cover the cases. This is narrow review evidence, not a security audit
certificate or proof of all possible schedules.

## Verification receipts

- Go `go test ./...` passed after coordinated v2 and again after the final
  header-buffer allocation repair. Focused auth/client/API tests and `-race`
  passed before and after the final header-storage change. The final auth,
  Go-client and API race runs passed in 3.667 s, 2.856 s and 6.970 s respectively;
  focused `go vet` for these packages and all generators also passed.
- Go v2 tests cover independent record/HMAC/session vectors, both directions,
  reflection, replay, gaps, tampering, truncation, invalid size, counter
  exhaustion, explicit roles, zero-byte operations, empty valid frames,
  unsupported encrypted versions and no direct-helper bypass.
- New Go 72-byte read allocation test **failed before the header repair at
  one allocation per record**, then read/write allocation tests passed ten
  repetitions at zero after warm-up. This is a synthetic steady-state
  allocation measurement, not physical input latency.
- DS4Windows full suite before the final read-allocation case: **2,996 passed,
  3 skipped, 0 failed** (`auth-v2-full-final-20260903.trx`). Earlier v2 full runs
  passed 2,995 tests. The three skips are existing opt-in live-audio cases.
  New cases include independent direction/session vectors, invalid-frame
  non-delivery, stream fault persistence, truncation, wrap refusal, zero-byte
  operations and simultaneous loopback read/write traffic in both directions.
- Final DS4Windows suite after the warmed feedback-read allocation test:
  **2,997 passed, 3 skipped, 0 failed (3,000 total; 31 seconds)**,
  `auth-v2-allocation-full-20260903.trx`. The 1,000 warmed 24-byte feedback
  reads allocate zero bytes, as do the existing input-write checks.
- Generated TypeScript auth compiled in isolation and **18 tests passed**,
  including full-buffer EOF, a coalesced first encrypted record, four truncated
  handshake sizes, exact counters above 2^53, replay/reflection/tamper and the
  independent record/session vectors.
- Generated C# auth compiled against .NET 8 in an isolated harness and passed
  vectors, one-byte reads, invalid records, overflow, and 128 overlapping async
  writes. The harness initially caught the wrong exception family; its checks
  now distinguish InvalidDataException and IOException explicitly.
- Generated Rust auth compiled with async support in isolation and **5 tests
  passed**, including deterministic three-byte partial writes alternating with
  Pending, an abandoned flush, a successor send, and exact resulting records.
  Sync cases cover vectors, record tails, invalid records and exhaustion.

Reproducible harness sources: `_testing/authv2/`. Generated output and all new
compiler/dependency caches are under
`Desktop/Controller-Platform-Portable-Lab-2026-08-31/`. Rust 1.98.0 components
were downloaded from the official dated Rust manifest (2026-08-20) and each
archive was SHA256-verified before extraction/execution. No Rust installer,
PATH persistence, system package installation, or security-policy change was
used. The existing workspace w64devkit supplies the assembler/dlltool; Rust
uses its own verified self-contained link libraries. Two initial compiler
setup attempts lacked the matching link libraries/dlltool configuration; those
were toolchain setup failures, not passing or failing application tests.

## Unresolved generated SDK / deployment gates

Whole-SDK checks exposed code-generation gaps outside auth:

- TypeScript: missing `XboxoneInput` export target and `Unknown` nested types in
  `ManagementDtos.ts`. Auth-only compilation/testing must not hide this failure.
- Rust: `Unknown` nested DTO fields and many Xbox internal constant types not
  defined in the generated public module. Full `cargo check --features async`
  is not green; the isolated auth module is green.
- C#: generated project targets net10.0, while the available SDK is 9.0.316 and
  existing client CI declares SDK 8. The isolated auth harness targets net8.0;
  no production SDK was installed or target silently downgraded.
- C++: OpenSSL headers/library needed for compile/runtime validation are not
  yet set up; source review alone is insufficient.

Still required: repair and test complete SDK generation; actual Go↔DS4Windows
matched-process v2 interoperability; compatibility/no-downgrade integration
matrix; independent final review of subsequent changes; matched portable
app/broker staging and hardware retest. Never deploy one v2 endpoint against
b48/b49's v1 counterpart, and never work around a mismatch by disabling auth.

The physical unplug test, unexplained recurring lifecycle events, Bluetooth
association/input/reconnect, Joy-Con2 hardware matrix, feature parity, feedback
fidelity, live game validation, comparative latency and test installer remain
open. No commit, push, release, deletion, or installed-component replacement
was performed. The active goal remains **in progress**.

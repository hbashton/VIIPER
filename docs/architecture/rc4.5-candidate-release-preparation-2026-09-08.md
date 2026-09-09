# RC4.5 candidate release preparation, 2026-09-08

This source checkpoint prepares VIIPER `v0.1.3-rc4.5` for the DS4Windows
`5.0.5.0-VIIPERRC4.5` tester package. It is not a GitHub release, signed binary,
live hardware acceptance, driver replacement, or permission to change the
running controller session. Exact artifact hashes and build metadata belong in
the generated candidate provenance file, after the checkpoint is compiled.

## Explicitly licensed YAML dependency

The formerly pinned `github.com/alecthomas/kong-yaml v0.2.0` module did not
contain a license. Its explicit omission from the dependency notice generator
was not clearance to distribute it. The candidate instead pins the later
upstream commit `a56dc1466c79ffc467aac6e158b878b2d7903bd2`, resolved by Go as
`v0.2.1-0.20240409173824-a56dc1466c79`.

- [Upstream license-addition PR #22](https://github.com/alecthomas/kong-yaml/pull/22)
- [Exact pinned MIT COPYING](https://github.com/alecthomas/kong-yaml/blob/a56dc1466c79ffc467aac6e158b878b2d7903bd2/COPYING)

The package API retains `Loader` and unchanged resolution behavior. The source
delta also adds an optional `Validate` hook: it rejects unused keys and treats
valid nested command mappings as unmatched top-level keys. VIIPER historically
accepted both nested and flattened mappings and ignored unused compatibility
keys. A small `kong.ResolverFunc(resolver.Resolve)` adapter deliberately retains
that contract for both normal configuration discovery and explicit exclusive
YAML configuration. It neither copies the old unlicensed implementation nor
weakens the existing explicit-file path, single-document, malformed-input,
JSON, TOML, or command-line parsing checks.

Before the adapter, all four new normal/exclusive flat/nested compatibility
cases failed with `extra configuration keys`. After it, those cases, false
boolean values, command-line precedence, unused-key compatibility, malformed
YAML rejection, and the existing exclusive-file tests pass. Tests only parse
temporary files and CLI models; no server, tray window, controller, or driver
is started.

## Reproducible release configuration and notices

Use the checked-in release recipe's equivalent flags: Windows/amd64,
`CGO_ENABLED=0`, `-tags release`, `-trimpath`, and stripped `-s -w` linking.
Set main.Version to `v0.1.3-rc4.5`, main.Commit to the checkpoint's short hash,
main.Date to the recorded UTC build time, and the code-generator Version to
the same candidate version. Regenerate the PE resource with the existing
version injector and `goversioninfo v1.7.0`: file version `0.1.3.0`, product
version `0.1.3-rc4.5`. Go `1.27.0 windows/amd64` is the available verified
toolchain. The production executable already includes the authorized Xbox One
stream/creation routes and registered Switch 2 Pro, DS4, DualSense and Xbox 360
device paths; no lab or experimental tag is needed.

Remove kong-yaml from the license ignore list. Fresh dependency notices must
contain its full MIT notice. The copied Windows-only systray implementation is
inside the VIIPER module, which the dependency report excludes; therefore the
notice template now explicitly includes its complete Apache-2.0 license and
exact upstream revision. Package `LICENSE.txt`, the fresh generated
`licenses.txt`, and `internal/tray/systray/README.viiper.md` with the binary.
The latter documents the local modifications. Do not substitute stale notices
from the previous 0.1.2 artifact.

USB/IP remains the separately validated `0.9.7.7` prerequisite. The broker's
file name, exact SHA-256, PE version, package manifest and DS4Windows bootstrap
probe must all be updated together; the old 0.1.2 artifact is not this build.

## Source validation before candidate build

- Go Release-tag full suite: PASS before and after the dependency update.
- `go test -tags release ./cmd/viiper -count=1`: PASS after the adapter.
- `go test -race -tags release ./cmd/viiper ./internal/tray/... -count=1`: PASS
  using the existing w64devkit GCC toolchain; no installation was needed.
- `golangci-lint run --new-from-rev HEAD ./cmd/viiper/... ./internal/tray/...`:
  zero new issues before this checkpoint. Unfiltered lint reports three
  pre-existing gofmt findings in `meta.go`, `meta_test.go` and
  `startup_windows.go`; no correctness checks or file exclusions were added.
- Fresh `go-licenses v2.0.1` report: PASS with MIT kong-yaml and explicit local
  systray Apache notice; only the existing Go assembly inspection warnings in
  x/crypto and x/sys remain. This is not a claim to a general legal audit.

The candidate should be inspected through PE metadata, Go build information,
plain help and content hashes. No server startup is required for those checks.

## Public RC pipeline preparation, 2026-09-09

The previously compiled `6205074` tester binary was local evidence, not an
already published release. At this audit, public `v0.1.2` pointed to
`f5d097b7e4a11d72df9e1627046923b17ef8ff5e`; the RC source was 21 commits
ahead with no divergent main commits. The intended new tag is
`v0.1.3-rc4.5`, not stable `v0.1.3`.

The release workflow now validates the complete semantic tag before builds.
A prerelease identifier, including `-rc4.5`, sets both `prerelease: true` and
`draft: true` and disables client registry publication. Maintainers inspect the
draft assets before explicitly publishing it. Stable tags retain normal
non-draft releases and optional registry publication. Hyphens inside build
metadata alone do not turn a stable semantic version into a prerelease.
The pure release-policy helper includes stable, RC, metadata and malformed-tag
regressions and does not contact GitHub.

The shared build/test jobs pin Go `1.27.0`, matching the locally validated
toolchain. Its availability is confirmed by the
[official Go release history](https://go.dev/doc/devel/release#go1.27.0).
This is a reproducibility pin, not a claim that it is the latest Go patch.
Release-tag tests now run alongside the existing coverage tests. Formatting
the three previously reported cmd/tray-scope files changed Windows CRLF to LF
only; there was no tracked functional delta. Broader CI lint remains a
separate release gate; the earlier scoped lint result is not a whole-repository
lint pass.

Every executable and library archive now includes the full VIIPER GPL text,
fresh dependency notices, `VIIPER-SYSTRAY-NOTICE.md`, and the copied complete
`VIIPER-SYSTRAY-LICENSE.txt`. The executable build checks that generated notices
contain the pinned kong-yaml MIT grant and the full copied systray license.
Windows executable archives additionally contain `BUILD-INFO.json`, recording
the actual executable SHA-256 and length, PE versions, UTC build timestamp,
GitHub run/attempt, all four notice hashes, and embedded Go module/build
information. Admission requires the current source commit, an unmodified tree,
Go 1.27.0, the release tag, trimpath, CGO disabled, and the intended target.
Windows/amd64 also records a help-only smoke result; ARM64 metadata inspection
does not attempt to run an ARM64 binary on an x64 runner. No signing claim is
made by the release or snapshot instructions.

Publication order must prevent two different same-version binaries from
being presented as the tested DS4Windows dependency:

1. Commit reviewed source, fast-forward main without rewriting its history,
   and create the annotated RC tag. A main push also triggers the existing
   development-snapshot workflow; that does not substitute for the tagged RC.
2. Let the tag workflow finish its draft, then download its actual Windows
   amd64 archive. Validate its executable, `BUILD-INFO.json`, notices and tag
   commit. Do not overwrite that executable with the older local tester build.
3. Attach a matching exact-commit `git archive --format=zip --prefix=VIIPER/`
   source archive and SHA-256 manifest to the draft, then publish the verified
   prerelease. Do not rerun the release creation job after manual publication
   without reviewing its draft/asset-update behavior.
4. Repin DS4Windows to this one authoritative CI executable and all new
   provenance/notice/source hashes before building and publishing DS4Windows.
   A previous tester SHA does not prove the new CI bytes were tested.

These workflow changes alone do not publish a release, change an installed
broker, or establish controller/driver acceptance for newly compiled bytes.

### First actual main CI feedback

Main commit `f8aa588` reached GitHub Actions run `34336834061`. C#, Rust and
TypeScript client builds passed. Two gates failed before any binary/release
jobs ran: the C++ generator assigned required nested DTOs directly to JSON
instead of using their member serializer, and Linux CGO lint identified a
redundant `uint32` conversion in the Switch 2 C-library test. The latter scope
was not included in the earlier CGO-disabled cross-target lint result.

The focused CGO Release tests passed after removing that redundant conversion.
The C++ correction preserves the existing optional/pointer/array paths while
using the existing member serializer for required named children. Deserialization
value-initializes the result so absent nested children cannot expose uninitialized
primitive values. These are client-generator/test corrections, not a new USB
protocol or controller mapping. The failed run is retained as diagnostic
evidence; a fresh successful main/tag run remains required for publication.

### Local pre-publication acceptance of the pipeline changes

- Release policy: four test methods covering 21 tag cases pass; malformed
  command-line input exits nonzero, while stable metadata containing a hyphen
  remains stable. The actual RC emits draft/prerelease true and registry false.
- `actionlint v1.7.12` passes the three edited release/build/snapshot workflows.
  The old unreachable Windows-tool step in the Linux-only test job was removed
  because it referenced a matrix that did not exist. Shellcheck/pyflakes were
  not invoked by that local actionlint command.
- A fresh license report and the exact workflow notice-validation block pass.
  The actual provenance block, using the prior known candidate as an offline
  fixture, accepts its source/hash/versions and rejects a different expected
  commit. That fixture is not a claim that the new source has been compiled.
- Full `go test -tags release -count=1 ./...` passes on Windows/amd64 with
  Go 1.27.0 and CGO disabled after lint cleanup. Evidence log:
  `rc45-full-release-accepted.log` in the local tester kit's build directory.
- Full Linux-target `golangci-lint v2.13.1 run ./...` passes with zero issues
  using `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`, without changed exclusions or
  disabled checks. Evidence: `rc45-full-linux-target-lint-accepted.log`.
  This is cross-target static analysis, not execution of Linux tests or proof
  of the Linux CGO shared-library build; the real CI jobs remain required.

The broader lint pass initially found 51 non-format findings plus 125
Windows-CRLF-only formatter findings. Cleanup retains production callbacks,
error propagation and protocol values; it removes only globally unreferenced
private helpers, ignored private return values and redundant assignments or
constant parameters. Test-only fixed defaults replace parameters that every
existing caller supplied identically; assertions and scenarios remain intact.
Additional tests exercise foreign IN/OUT session authorities, non-next reset
generations, a nonzero persona clock with backward-time rejection, and unknown
endpoint/direction cases. Diagnosed formatter normalization introduces no
tracked structural changes outside deliberately edited files.

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

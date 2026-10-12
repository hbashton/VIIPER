# VIIPER development

[Documentation index](index.md) · [Installation](getting-started/installation.md)

Use the Go toolchain required by `go.mod` (currently 1.26.2 or a compatible newer
version). `just` provides the repository's build recipes. From the repository root:

```powershell
go test ./...
just build Release
go run ./cmd/viiper codegen
```

Build the generated client examples after changing public state or feedback
contracts. The module path remains `github.com/Alia5/VIIPER`; fork repository
ownership does not change Go imports or on-disk configuration namespaces.

The default server output is `dist/viiper.exe` on Windows or `dist/viiper` on
Linux. libVIIPER requires CGO and a compatible C toolchain. See `justfile` and
[the shared-library guide](libviiper/overview.md).

## Tests and hardware

Unit tests and client interoperability checks do not prove game or hardware
compatibility. Record the source commit, device model, connection, feeder,
driver and procedure for live tests. Use the documented opt-in test tools;
keep authentication and backend ownership intact during experiments.

- [Latency testing](testing/e2e_latency.md)
- [DualSense end-to-end testing](https://github.com/hbashton/VIIPER/blob/main/_testing/e2e/DUALSENSE_V5.md)
- [Startup diagnostics](startup-diagnostics.md)
- [Technical records](reference.md)

## Documentation

Use [the MkDocs guide](mkdocs-readme.md) to preview and build the site. The docs
deployment workflow publishes main/tag versions automatically; do not manually
publish unrelated development content to GitHub Pages.

Keep README concise, release notes in CHANGELOG.md, and dated investigations
clearly identified as evidence. Retain licensing notices and attribution.

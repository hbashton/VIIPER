# Build and preview the documentation

[Documentation index](index.md)

Run commands from the **repository root**, where `mkdocs.yml` is located.
Use a Python virtual environment with `mkdocs-material` and `mike` installed:

```powershell
python -m venv .venv
.\.venv\Scripts\python -m pip install mkdocs-material mike
.\.venv\Scripts\python -m mkdocs serve
```

Open the local address printed by MkDocs. On Linux, use `.venv/bin/python`.

```powershell
.\.venv\Scripts\python -m mkdocs build
```

The generated site is written to `site/`; do not commit it as source. Review
build warnings and check new navigation targets. `docs/` holds the maintained
pages; `mkdocs.yml` controls navigation. Repository-level licenses, source tools
and CHANGELOG.md are linked on GitHub where appropriate.

The Deploy Documentation workflow publishes versioned main/tag pages using
`mike`. It also generates commit changelogs for those versions. Those generated
commit lists are distinct from the curated release history in CHANGELOG.md.
Do not run a manual deployment as part of an ordinary documentation edit.

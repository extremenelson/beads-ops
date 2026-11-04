# bops — reference implementation

Go module for the intent-level command layer specified in [`../SPEC.md`](../SPEC.md). Contract 1.

```bash
cd bops
go build -o bops ./cmd/bops
./bops version            # key=value on a TTY, JSON when piped or with --json
./bops manifest           # the embedded commands.json
./bops gen docs           # render docs/COMMANDS.md from the manifest
go test ./...             # unit + golden tests; no Dolt required
go test ./internal/gen -update   # regenerate goldens — review the diff as a contract change
```

Layout:

| Path | Role |
|---|---|
| `commands.json` | Normative manifest (SPEC §4), embedded into the binary |
| `cmd/bops` | CLI entry; dispatch, exit codes (0 verdict / 1 could-not-run / 2 usage) |
| `internal/manifest` | Parse + validate the manifest |
| `internal/bdx` | The only package that talks to `bd`; `Runner` interface with `Exec` and `Fake` |
| `internal/gen` | Binding generators (`docs` today; `cc`, `codex`, `hermes`, `clj`, `mcp` to come) |
| `testdata/golden` | Golden outputs, one per generator target |

Commands named in the manifest but not yet implemented exit 1 with `did NOT run`, so a binding never mistakes "unimplemented" for "nothing to do".

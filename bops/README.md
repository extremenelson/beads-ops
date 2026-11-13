# bops — reference implementation

Go module for the intent-level command layer specified in [`../SPEC.md`](../SPEC.md). Contract 1, released as `bops/v0.1.0` on 2026-11-12.

## Install

```bash
go install github.com/extremenelson/beads-ops/bops/cmd/bops@v0.1.0   # the tagged release
# or, from a checkout, the latest main:
cd bops && go install ./cmd/bops
bops version
```

Needs `bd` ≥ 1.3 on `PATH` at run time (CI tests 1.3.4) and `git`. `sweep` and `land --verify` also use `gh` or `glab` when the forge is GitHub or GitLab.

New to `bops`? Start with the [tutorial](../docs/tutorial-first-loop.md). Every command, flag and output field is in the generated [command reference](../docs/COMMANDS.md); config keys are in the [configuration reference](../docs/reference/configuration.md).

## Commands

| Command | Intent | Kind |
|---|---|---|
| `next` | What should I work on? | read |
| `claim` | Take this bead for this run | write |
| `note` | Record where I am, keep the claim | write |
| `release` | Give the claim back | write |
| `land` | Record that work reached its destination | write |
| `sweep` | Close landed beads whose refs are merged | write |
| `stale` | Claims nobody is working | read |
| `lint` | Is this bead workable? | read |
| `sync` | Pull, heartbeat, push, with the one retry | write |
| `session-start` | Orient a fresh agent session | read+sync |
| `doctor` | Can the claim lock work against its remote? | read |
| `version` | Report bops, contract, and detected bd | read |
| `manifest` | Print the embedded `commands.json` | read |
| `gen <target>` | Render a binding from the manifest (`docs` today) | read |

`gates` and `deps` are in the manifest but not implemented. They exit 1 with `did NOT run`, so a binding never mistakes "unimplemented" for "nothing to do".

Output is JSON when stdout is not a terminal or with `--json`, and `key=value` lines on a terminal. Exit codes:

| Exit | Meaning |
|---|---|
| 0 | done, or a read's verdict |
| 1 | could not run: remote or ledger unreachable, precondition unverifiable, `bd` error, bad config |
| 2 | usage error |
| 3 | a write's negative verdict (`lost`, `refused`), so `bops claim <id> && work` can't proceed |

## Develop

```bash
cd bops
go build -o bops ./cmd/bops
go test ./...                            # unit + golden tests with a fake bd; no Dolt needed
gofmt -l .                               # must print nothing (CI checks it)
go vet ./...
go run ./cmd/bops gen docs > ../docs/COMMANDS.md   # after any change to commands.json
go test ./internal/gen -update           # regenerate goldens; review the diff as a contract change
BOPS_INTEGRATION=1 go test -count=1 -timeout 40m ./cmd/bops ./internal/intent   # real bd: live ledger and two-replica race suites
```

CI ([`.github/workflows/bops.yml`](../.github/workflows/bops.yml)) runs `gofmt`, `go vet`, `go test`, checks that `docs/COMMANDS.md` matches the manifest, and runs the integration suites against a checksum-verified `bd` 1.3.4. `BOPS_RACE_TRIALS` sets the claim-race trial count.

## Layout

| Path | Role |
|---|---|
| `commands.json` | Normative manifest (SPEC §4), embedded into the binary by `manifest.go` |
| `cmd/bops` | CLI entry: dispatch, flags, config loading, exit codes, per-command time budgets |
| `internal/intent` | The commands: `next`, `claim`, `note`, `land`, `release`, `sweep`, `stale`, `sync`, `lint`, `session-start`, `doctor` |
| `internal/lock` | The claim lock: `refs/bops/claims/<id>`, the push-and-classify protocol, `doctor`'s probes |
| `internal/bdx` | The only package that talks to `bd`; `Runner` interface with `Exec` and `Fake` |
| `internal/ledger` | bd JSON types and decoding (the only place field names live) |
| `internal/forge` | The only package that knows what a pull request is (`gh`, `glab`, or none) |
| `internal/runstate` | Per-machine run identity, hold journal and event log (`$XDG_STATE_HOME/bops/`) |
| `internal/config` | `.bops.json`, per-user config and `BOPS_*` env, merged (SPEC §10) |
| `internal/manifest` | Parse + validate the manifest |
| `internal/gen` | Binding generators: `docs` today; `cc`, `codex`, `hermes`, `clj` and `mcp` are planned (SPEC §8) and not registered yet |
| `testdata/golden` | Golden outputs, one per generator target |
| `testdata/README.md` | Measurements the lock design rests on (GitHub ref races, `bd` behaviors) |

[`docs/ARCHITECTURE.md`](../docs/ARCHITECTURE.md) explains how these fit together and why ownership is a git ref.

# bops — reference implementation

Go module for the intent-level command layer specified in [`../SPEC.md`](../SPEC.md). Contract 1. Latest release `bops/v0.4.0` (2026-11-19); contract 1 has been frozen since `bops/v0.1.0`.

## Install

```bash
go install github.com/extremenelson/beads-ops/bops/cmd/bops@v0.4.0   # the tagged release
# or, from a checkout, the latest main:
cd bops && go install ./cmd/bops
bops version
```

Without Go, download a binary from the [releases page](https://github.com/extremenelson/beads-ops/releases). Each `bops/vX.Y.Z` release has `bops_vX.Y.Z_<os>_<arch>.tar.gz` (`.zip` on Windows) for linux, darwin and windows on amd64 and arm64, and android/arm64 for Termux, plus `checksums.txt`:

```bash
v=v0.4.0 t=linux_amd64      # or linux_arm64, darwin_arm64, android_arm64, ...
base=https://github.com/extremenelson/beads-ops/releases/download/bops/$v
curl -fsSLO "$base/bops_${v}_$t.tar.gz" && curl -fsSLO "$base/checksums.txt"
grep " bops_${v}_$t.tar.gz\$" checksums.txt | sha256sum -c -    # macOS: shasum -a 256 -c -
tar xzf "bops_${v}_$t.tar.gz" && install "bops_${v}_$t/bops" ~/.local/bin/   # Termux: $PREFIX/bin/
```

Release binaries start at v0.2.0; v0.1.0 is `go install` only (`@v0.1.0`).

**Termux:** the `android_arm64` build installs and runs natively, and `bops doctor` passes, fetching from and pushing to the remote. Commands that open the ledger (`next`, `claim`, `note`, `sync` and the rest) don't work natively yet. That's a `bd` limit, not a `bops` one. bd 1.3.4's `android_arm64` build has no cgo, so it can't open an embedded ledger (`bd bootstrap` fails with `embeddeddolt: requires CGO`), and its `linux_arm64` build needs glibc, which Termux doesn't have. Until bd ships an Android build with cgo, run both inside `proot-distro` (Debian, say) with their `linux_arm64` builds. That route is untested (bops-ced).

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
| `gen <target>` | Render a binding from the manifest: `docs`, `cc`, `codex`, `clj`, `mcp` (`mcp` takes `--enable-writes`) | read |
| `serve-mcp` | Serve the MCP tools on stdio, read-only unless `--enable-writes` ([`bindings/mcp`](../bindings/mcp/)) | read |

`bops deps --engine graph` is in the manifest but not implemented. It exits 1 with `did NOT run`, so a binding never mistakes "unimplemented" for "nothing to do". `--engine cte` needs bd in server mode (`bd init --server`); on an embedded ledger it exits 1 saying so.

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
go run ./cmd/bops gen cc --out ../bindings/claude-code && cp ../bindings/claude-code/skills/bops/SKILL.md ../.claude/skills/bops/SKILL.md   # same, and after editing internal/gen/templates
go run ./cmd/bops gen codex --out ../bindings/codex && cp ../bindings/codex/skills/bops/SKILL.md ../.agents/skills/bops/SKILL.md
go run ./cmd/bops gen clj --out ../bindings/clojure
go test ./internal/gen -update           # regenerate goldens; review the diff as a contract change
BOPS_INTEGRATION=1 go test -count=1 -timeout 40m ./cmd/bops ./internal/intent   # real bd: live ledger and two-replica race suites
```

CI ([`.github/workflows/bops.yml`](../.github/workflows/bops.yml)) runs `gofmt`, `go vet`, `go test`, checks that `docs/COMMANDS.md` and the Claude Code and Codex bindings match the manifest, and runs the integration suites against a checksum-verified `bd` 1.3.4. `BOPS_RACE_TRIALS` sets the claim-race trial count.

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

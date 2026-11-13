# Contributing

Thanks for looking. Three kinds of contribution are especially welcome, in this order. Changes to the `bops` Go code follow the extra rules in §4.

## 1. Harness bindings

A binding is how one coding agent / orchestrator meets `docs/PROCESS.md`. The contract is four points, documented in [`bindings/README.md`](bindings/README.md): session start, claim, handoff note, stop. A new binding is a directory under `bindings/<harness>/` with a `README.md` that says which of the four it covers and how, plus whatever config files the harness needs. Mark anything you haven't run.

## 2. Lint rules

`scripts/bd-lint-claim.sh` is numbered against the workability table in `PROCESS.md` §3.6. A new rule should cite its row (or add one, with the "why it matters" column filled in). Rules that need judgment are `WARN`; rules bd or a regex can decide are `FAIL`. Add a case to the stub test in `scripts/test/` so the row is exercised without a live `bd`.

## 3. Process changes

Open a PR that names the failure the change prevents. "We lost a day because X" is the bar; "it would be cleaner if" usually isn't. Changes that make the process *stricter* need a worked example of the bead that would now be rejected.

## 4. `bops` code

[`SPEC.md`](SPEC.md) is the source of truth; the Go code in [`bops/`](bops/) conforms to it. A change in behavior, output shape or exit semantics changes the SPEC in the same PR. Contract 1 is frozen from the `bops/v0.1.0` tag: removing or renaming a command, argument, output field or outcome needs a new contract number (SPEC §1.9, §11). Adding is fine.

Before you open a PR, from `bops/`:

```bash
gofmt -l .        # must print nothing
go vet ./...
go test ./...     # unit + golden tests, fake bd, no Dolt needed
go run ./cmd/bops gen docs | diff - ../docs/COMMANDS.md   # CI fails on any diff
```

- **Changing a command's arguments or outputs** means editing `bops/commands.json`, then regenerating `docs/COMMANDS.md` with `go run ./cmd/bops gen docs > ../docs/COMMANDS.md`. A changed golden file (`go test ./internal/gen -update`) is a contract change and gets reviewed as one.
- **Touching the claim lock, `sync` or `claim`** means running the real-`bd` suites too: `BOPS_INTEGRATION=1 go test -count=1 -timeout 40m ./cmd/bops ./internal/intent` with `bd` 1.3.x on `PATH`. CI runs them on every PR that touches `bops/`.
- **Every shipped bug gets a test that fails without the fix** (SPEC §9).
- **Standard library only.** A new module dependency must be justified here, in this section, in the same PR (SPEC §11a). There are none today.
- Package boundaries are deliberate: only `internal/bdx` runs `bd`, only `internal/lock` knows refnames, only `internal/forge` knows what a PR is, only `internal/ledger` knows `bd`'s JSON field names. [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) explains why.

## Things that won't be merged

- Copies of `bd` CLI documentation. `bd prime` and `bd --help` are canonical; this repo points at them.
- Edits inside `vendor/`. That tree is upstream's; refresh it by copy and update `SOURCE.md`.
- Harness-specific policy that contradicts `PROCESS.md` without a change to `PROCESS.md`.

## Conventions

- Shell: `bash`, `set -u`, POSIX-friendly where cheap. `shellcheck` clean.
- Docs: Markdown, one sentence per claim, cite the upstream doc or `bd` command a mechanical claim rests on.
- Commits: work tracked by a bead starts the subject with its id (`bops-0g6: refuse release --reason done with no land record`). Other commits use a short prefix: `docs:`, `lint:`, `binding(kestrel):`, `process:`, `chore:`, and `beads:` for a ledger export.
- Branches: `work/<bead-id>` for bead work, `docs/…` or `chore/…` otherwise. Changes land through a PR to `main`.
- Track your own work on this repo in beads, naturally. `bd init` is set up; `bops next` (or `bd ready`) shows what's open.

## License

By contributing you agree your contribution is licensed under the repo's MIT license.

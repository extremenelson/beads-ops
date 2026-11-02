# Contributing

Thanks for looking. Three kinds of contribution are especially welcome, in this order.

## 1. Harness bindings

A binding is how one coding agent / orchestrator meets `docs/PROCESS.md`. The contract is four points, documented in [`bindings/README.md`](bindings/README.md): session start, claim, handoff note, stop. A new binding is a directory under `bindings/<harness>/` with a `README.md` that says which of the four it covers and how, plus whatever config files the harness needs. Mark anything you haven't run.

## 2. Lint rules

`scripts/bd-lint-claim.sh` is numbered against the workability table in `PROCESS.md` §3.6. A new rule should cite its row (or add one, with the "why it matters" column filled in). Rules that need judgment are `WARN`; rules bd or a regex can decide are `FAIL`. Add a case to the stub test in `scripts/test/` so the row is exercised without a live `bd`.

## 3. Process changes

Open a PR that names the failure the change prevents. "We lost a day because X" is the bar; "it would be cleaner if" usually isn't. Changes that make the process *stricter* need a worked example of the bead that would now be rejected.

## Things that won't be merged

- Copies of `bd` CLI documentation. `bd prime` and `bd --help` are canonical; this repo points at them.
- Edits inside `vendor/`. That tree is upstream's; refresh it by copy and update `SOURCE.md`.
- Harness-specific policy that contradicts `PROCESS.md` without a change to `PROCESS.md`.

## Conventions

- Shell: `bash`, `set -u`, POSIX-friendly where cheap. `shellcheck` clean.
- Docs: Markdown, one sentence per claim, cite the upstream doc or `bd` command a mechanical claim rests on.
- Commits: conventional-commit prefixes (`docs:`, `lint:`, `binding(kestrel):`, `process:`).
- Track your own work on this repo in beads, naturally. `bd init` is set up; `bd ready`.

## License

By contributing you agree your contribution is licensed under the repo's MIT license.

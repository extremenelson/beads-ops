# Changelog

## Unreleased

- `bops release --reason done` is refused (exit 3, `outcome: refused`) when the bead has no land record. Run `bops land` first. The check runs before the claim ref or the ledger changes, in both lock modes, and the hold is kept (bops-0g6).
- Docs: a [tutorial](docs/tutorial-first-loop.md), [how to set up the claim lock](docs/howto-claim-lock.md), a [configuration reference](docs/reference/configuration.md), an [architecture overview](docs/ARCHITECTURE.md) and a [docs index](docs/README.md). README, `bops/README.md`, CONTRIBUTING and the Claude Code binding brought up to date (bops-ilg).

## bops 0.1.0 (contract 1) — 2026-11-12

First release of `bops`, the intent-level command layer over `bd` specified in [`SPEC.md`](SPEC.md). Tag `bops/v0.1.0`. Contract 1 is frozen from this tag.

- Commands: `next`, `claim`, `note`, `release`, `land`, `sweep`, `stale`, `lint`, `sync`, `session-start`, `doctor`, `version`, `manifest`, `gen docs`. `gates` and `deps` are specified and exit 1 as not implemented.
- **Claim lock.** Ownership is a git ref per bead, `refs/bops/claims/<id>`, written with a create-only, leased push, so two machines can never both hold a bead. The ledger's assignee and `run:` label display it and are repaired by `sync`. Takeover of a stale claim (`claim --force`), PR rework (`claim --resume`), and landing refs that `sweep` deletes once the PR merges. Recovery runbook in [PROCESS.md §12](docs/PROCESS.md#12-recovery-runbook-bops-ledgers).
- `bops doctor` checks the lock remote, fetch and push access, create-only push support, and that the Dolt remote is on the same repository.
- Configuration: `.bops.json`, a per-user file and `BOPS_*` variables, validated strictly. `lock.remote` picks the lock's git remote; `lock.mode: none` turns the lock off for a single-machine ledger (repo file only). `stale_minutes`, `hold_minutes`, `pool`, `caps` and `forge` are applied.
- `claim`, `sync`, `sweep` and `doctor` refuse with `remote_mismatch` when the lock remote and the Dolt remote differ or there is no Dolt remote.
- Forge adapter for GitHub (`gh`) and GitLab (`glab`), inferred from `lock.remote`.
- `release` takes optional text with `--file <path>` or `--file -`, recorded in its `bops:released` comment. `release --reason stale` is renamed `abandoned`.
- `session-start --hook-json` emits Claude Code's SessionStart hook envelope; this repo runs it as its own SessionStart hook.
- The human-review label is spelled `needs-human` everywhere; the legacy `needs:human` is still read until v0.2.0.
- Exit 3 for a write's negative verdict (`lost`, `refused`), separate from exit 1 (could not run).
- Tests: unit and golden suites with a fake `bd`; live-ledger and two-replica race suites against real `bd` 1.3.4 in CI.
- Bindings: a hand-written Claude Code skill and hooks for the `bops` loop; the Kestrel binding claims, lands, releases and syncs through `bops`.
- ADR 0001 (derived read model, amended for the claim lock), ADR 0002 (manifest-driven Go CLI), the claim-lock design, and research on the beads pre-commit export skew.

## 0.2.0 — 2026-11-02

- `docs/PROCESS.md` v0.2: §3.6 workability table (21 rows), §3.7 pre-claim lint.
- `docs/MULTI-DEV-PLAN.md`: hub-and-spoke topology, five-phase rollout, conflict/upgrade runbooks.
- `scripts/bd-lint-claim.sh` with stub test; `scripts/systemd/` sync timer.
- Bindings: Claude Code (reference), Hermes (sketch), Kestrel (draft Babashka skill).
- Vendored upstream `plugins/beads` @ cbfdafc.

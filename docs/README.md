# beads-ops documentation

Start with the tutorial if you're new. Use the reference when you know what you want and need the exact flag, key or field.

## Learn

- [Tutorial: your first bops loop](tutorial-first-loop.md). One bead through `next`, `claim`, `note`, `land` and `release` on a scratch ledger, in about ten minutes.

## Do

- [How to set up the claim lock](howto-claim-lock.md). Choose `ref` or `none`, wire the remote, check it with `bops doctor`, prove it with two clones.
- [Recovery runbook](PROCESS.md#12-recovery-runbook-bops-ledgers) (PROCESS.md §12). Stuck, lost, stale and orphaned claims.
- [Multi-developer rollout](MULTI-DEV-PLAN.md). Topology, five-phase rollout, sync conflicts, `bd` upgrades.
- [Harness bindings](../bindings/README.md). Wire the process into Claude Code, Hermes, Kestrel, or your own harness.

## Look up

- [Command reference](COMMANDS.md). Every `bops` command, flag, outcome and output field. Generated from `bops/commands.json`; don't edit it by hand.
- [Configuration reference](reference/configuration.md). `.bops.json`, the user config file, `BOPS_*` variables, validation errors.
- [SPEC.md](../SPEC.md). The normative contract: glossary, ledger conventions, the claim lock, lint rules, ordering, versioning.
- [PROCESS.md](PROCESS.md). The operating process: roles, session protocol, handoff notes, the 21-row workability checklist.

## Understand

- [Architecture](ARCHITECTURE.md). How `bops` is put together, where each kind of state lives, and why the claim lock is a git ref.
- [ADR 0001](adr/0001-derived-read-model-not-write-path.md). A graph layer is a derived read model, never a write path (amended for the claim lock).
- [ADR 0002](adr/0002-go-reference-implementation.md). One manifest-driven CLI, Go reference implementation.
- [Claim-lock design](designs/bops-claim-lock-v0.1.0.md). The v0.1.0 design, review rounds and measurements.
- [Research snapshots](research/). Dated investigations behind the ADRs, each with a "status as of" header.

## Plans

- [v0.1.0 delivery plan](plan/v0.1.0.md). The epic and tasks that built `bops` v0.1.0 (shipped 2026-11-12).

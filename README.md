# beads-ops

**The operating layer for [beads](https://github.com/gastownhall/beads): process, lint, sync automation, and agent-harness bindings for teams running multiple developers, multiple machines, and multiple coding agents on one issue graph.**

Beads gives agents a dependency-aware, git-synced task memory. It deliberately stops at the mechanics (`bd ready`, `bd prime`, Dolt sync) and leaves the *policy* open: who closes epics, what a handoff note must contain, when a bead is actually workable, how sync runs on a laptop that's sometimes offline. This repo fills that gap with opinionated, tested defaults you can adopt wholesale or cherry-pick.

> Status: **v0.2, early.** Used daily by the maintainer across Claude Code, Hermes, and a Clojure orchestrator. Field names and `bd` behaviors are verified against upstream at the commit pinned in [`vendor/beads-skill/SOURCE.md`](vendor/beads-skill/SOURCE.md). Requires `bd` ≥ 1.3 (CI tests 1.3.4); pin one `bd` version per shared ledger.

## What's here

| Path | What it is |
|---|---|
| [`SPEC.md`](SPEC.md) | **`bops` contract 1** — the intent-level command layer over `bd`: glossary, ledger state conventions, the claim lock, command manifest, outcomes and exit codes, `next` ordering, lint rules, forge adapter, generated bindings |
| [`bops/`](bops/) | Go reference implementation of `SPEC.md`: `next claim note release land sweep sync stale lint version` (`session-start gates deps` are specified, not built); `bops/commands.json` is the manifest every binding is generated from; [`docs/COMMANDS.md`](docs/COMMANDS.md) is its generated reference |
| [`docs/adr/`](docs/adr/) | Architecture decisions with ranked options and switching conditions: [0001](docs/adr/0001-derived-read-model-not-write-path.md) derived read model, never a write path; [0002](docs/adr/0002-go-reference-implementation.md) manifest-driven CLI, Go reference implementation |
| [`docs/plan/`](docs/plan/) | One file per milestone; [`v0.1.0.md`](docs/plan/v0.1.0.md) is the epic + 14 tasks that [`scripts/plan/v0.1.0-beads.sh`](scripts/plan/v0.1.0-beads.sh) creates in the ledger |
| [`docs/research/`](docs/research/) | Dated research snapshots behind the ADRs; each carries a "status as of" header |
| [`docs/PROCESS.md`](docs/PROCESS.md) | The operating process: invariants, roles, session protocol, handoff-note contract, decomposition rules, gates, formulas, offline/multi-machine rules, a 21-row **workability checklist** for beads, and a **recovery runbook** for stuck claims |
| [`docs/MULTI-DEV-PLAN.md`](docs/MULTI-DEV-PLAN.md) | Topology decision (hub-and-spoke on your git remote, embedded Dolt, `bd sync`) with ranked alternatives and exact switching conditions; five-phase rollout; conflict and recovery runbooks; upgrade procedure |
| [`scripts/bd-lint-claim.sh`](scripts/bd-lint-claim.sh) | Pre-claim lint. Exit 0 = workable, 1 = fix the bead, 2 = tool error. `--json` for agents |
| [`scripts/systemd/`](scripts/systemd/) | `beads-sync@.timer` for always-on machines, keyed on `bd sync` exit codes. No `bd reclaim` on a `bops` ledger |
| [`bindings/`](bindings/) | How each agent harness meets the process. [`claude-code/`](bindings/claude-code/) is the reference; [`hermes/`](bindings/hermes/) and [`kestrel/`](bindings/kestrel/) are the maintainer's stack. The [binding contract](bindings/README.md) is small — add yours |
| [`formulas/`](formulas/) | Distilled `bd` formulas (`*.formula.toml`) for repeatable workflows. Empty until a workflow has run twice |
| [`vendor/beads-skill/`](vendor/beads-skill/) | Upstream's `plugins/beads` tree, pinned. Theirs, not ours; refreshed by copy |

## Quickstart (one developer, one repo)

```bash
# 0. bd ≥ 1.3 and Go installed; project has a git origin, and bd's Dolt remote is that same origin
cd your-project
bd init                                   # or: bd dolt remote list  (verify origin is wired)
bd setup claude                           # upstream hooks + CLAUDE.md section

# 1. install bops
go install github.com/extremenelson/beads-ops/bops/cmd/bops@v0.1.0   # once the bops/v0.1.0 tag exists
#    until then, from a clone of this repo:  (cd bops && go install ./cmd/bops)
bops version                              # prints contract 1 and the bd it found

# 2. adopt the process
git clone https://github.com/extremenelson/beads-ops ../beads-ops
cp ../beads-ops/docs/PROCESS.md docs/
cp ../beads-ops/scripts/bd-lint-claim.sh scripts/
cat ../beads-ops/bindings/claude-code/CLAUDE.md.snippet >> CLAUDE.md
#    merge ../beads-ops/bindings/claude-code/settings.bops.json into .claude/settings.json

# 3. the loop
bops next                                 # ranked, workable candidates
bops claim bd-a1b2 && echo mine           # exit 0 claimed or held; exit 3 lost or refused: pick another
echo "NEXT: write the parser test" | bops note bd-a1b2
bops release bd-a1b2 --reason done        # or blocked|scope|stale|human
```

`bops claim` takes a bead through a git ref on your remote (`refs/bops/claims/<id>`), so two machines can never both hold it. `bd update --claim` cannot promise that across machines. Claims made with raw `bd`, including `/bead-loop` and the Kestrel binding today, bypass the lock. When something gets stuck, [`PROCESS.md` §12](docs/PROCESS.md#12-recovery-runbook-bops-ledgers) is the runbook.

For a team on many machines, follow [`docs/MULTI-DEV-PLAN.md`](docs/MULTI-DEV-PLAN.md) phases 0–5.

## The three invariants

Everything in `PROCESS.md` derives from these:

1. **The graph is the plan.** No markdown TODO lists. Work is a bead; durable facts are `bd remember`; everything else may die with the conversation.
2. **Notes are written for a reader with zero context.** Every `in_progress` bead carries `COMPLETED / IN PROGRESS / NEXT / BLOCKER` so a fresh agent — on any machine, after compaction — can resume.
3. **`blocks` means *cannot proceed*.** Only `blocks` affects `bd ready`. Preferences, context, and provenance use the other three edge types, or parallelism dies.

## Relationship to upstream

- Mechanics come from `bd prime` and upstream's skill; this repo never duplicates the CLI reference. Where upstream and `PROCESS.md` disagree on *how a command works*, upstream wins. Where they disagree on *policy*, this repo is making a deliberate choice and says so.
- Upstream's skill tree is vendored under `vendor/` with its commit pinned so the line between theirs and ours stays visible. Refresh: re-clone upstream, copy `plugins/beads/`, update `SOURCE.md`.
- Pieces that prove general (the lint, the sync timer) are candidates to contribute upstream.

## Contributing

Bindings for other harnesses (Aider, Cursor, Codex, OpenCode, Gemini CLI…) are the most useful contribution — see [`bindings/README.md`](bindings/README.md) for the four-point contract. Process changes should come with the failure they prevent. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

## License and attribution

MIT © Alex Nelson. Developed and maintained with support from [ArchWise](https://archwise.cloud) — solution architecture for mid-market teams adopting agentic development. Upstream beads is MIT © Steve Yegge; see `vendor/beads-skill/SOURCE.md`.

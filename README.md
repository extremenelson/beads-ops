# beads-ops

**The operating layer for [beads](https://github.com/gastownhall/beads): process, lint, sync automation, and agent-harness bindings for teams running multiple developers, multiple machines, and multiple coding agents on one issue graph.**

Beads gives agents a dependency-aware, git-synced task memory. It deliberately stops at the mechanics (`bd ready`, `bd prime`, Dolt sync) and leaves the *policy* open: who closes epics, what a handoff note must contain, when a bead is actually workable, how sync runs on a laptop that's sometimes offline. This repo fills that gap with opinionated, tested defaults you can adopt wholesale or cherry-pick.

> Status: **v0.2, early.** Used daily by the maintainer across Claude Code, Hermes, and a Clojure orchestrator. Field names and `bd` behaviors are verified against upstream at the commit pinned in [`vendor/beads-skill/SOURCE.md`](vendor/beads-skill/SOURCE.md). Expect churn while `bd` is pre-1.0.

## What's here

| Path | What it is |
|---|---|
| [`docs/PROCESS.md`](docs/PROCESS.md) | The operating process: invariants, roles, session protocol, handoff-note contract, decomposition rules, gates, formulas, offline/multi-machine rules, and a 21-row **workability checklist** for beads |
| [`docs/MULTI-DEV-PLAN.md`](docs/MULTI-DEV-PLAN.md) | Topology decision (hub-and-spoke on your git remote, embedded Dolt, `bd sync`) with ranked alternatives and exact switching conditions; five-phase rollout; conflict and recovery runbooks; upgrade procedure |
| [`scripts/bd-lint-claim.sh`](scripts/bd-lint-claim.sh) | Pre-claim lint. Exit 0 = workable, 1 = fix the bead, 2 = tool error. `--json` for agents |
| [`scripts/systemd/`](scripts/systemd/) | `beads-sync@.timer` for always-on machines, keyed on `bd sync` exit codes |
| [`bindings/`](bindings/) | How each agent harness meets the process. [`claude-code/`](bindings/claude-code/) is the reference; [`hermes/`](bindings/hermes/) and [`kestrel/`](bindings/kestrel/) are the maintainer's stack. The [binding contract](bindings/README.md) is small — add yours |
| [`formulas/`](formulas/) | Distilled `bd` formulas (`*.formula.toml`) for repeatable workflows. Empty until a workflow has run twice |
| [`vendor/beads-skill/`](vendor/beads-skill/) | Upstream's `plugins/beads` tree, pinned. Theirs, not ours; refreshed by copy |

## Quickstart (one developer, one repo)

```bash
# 0. bd ≥ 0.60 installed; project has a git origin
cd your-project
bd init                                   # or: bd dolt remote list  (verify origin is wired)
bd setup claude                           # upstream hooks + CLAUDE.md section

# 1. adopt the process
git clone https://github.com/extremenelson/beads-ops ../beads-ops
cp ../beads-ops/docs/PROCESS.md docs/
cp ../beads-ops/scripts/bd-lint-claim.sh scripts/
cat ../beads-ops/bindings/claude-code/CLAUDE.md.snippet >> CLAUDE.md
#    merge ../beads-ops/bindings/claude-code/settings.json into .claude/settings.json

# 2. claim through the lint from now on
alias bdclaim='f(){ scripts/bd-lint-claim.sh "$1" && bd update "$1" --claim; }; f'
bdclaim bd-a1b2
```

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

MIT © Alex Nelson. Developed and maintained with support from [ArchWise](https://archwise.io) — solution architecture for mid-market teams adopting agentic development. Upstream beads is MIT © Steve Yegge; see `vendor/beads-skill/SOURCE.md`.

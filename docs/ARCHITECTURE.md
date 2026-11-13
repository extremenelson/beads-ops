# How bops is built, and why

`bops` sits between coding agents and the beads issue tracker. Agents think in intents ("what next", "take it", "record where I am", "give it back", "prove it landed"); `bd` speaks in low-level writes. `bops` turns each intent into one command with a fixed JSON answer and exit code, and it adds the one thing `bd` can't promise yet: a claim that holds across machines.

This page explains the design. For exact behavior, read [SPEC.md](../SPEC.md) (normative) and the [command reference](COMMANDS.md). For the decisions behind it, read [ADR 0001](adr/0001-derived-read-model-not-write-path.md), [ADR 0002](adr/0002-go-reference-implementation.md) and the [claim-lock design](designs/bops-claim-lock-v0.1.0.md).

## The problem

Two failures drove the design.

**Every team rewrites the same wrappers, and the copies drift.** Before `bops`, the readiness filter, the handoff-note format and the "is this bead really workable" rules lived in shell aliases, jq one-liners, a Clojure orchestrator and prompt text. Each copy encoded the rules slightly differently, so an agent in one harness would claim a bead an agent in another harness considered off limits.

**`bd update --claim` holds on one replica only.** In a two-replica test on `bd` 1.3.4, both replicas claimed the same bead. A's push succeeded, B's was rejected; B pulled, Dolt settled the assignee last-write-wins in B's favor, and B pushed. The agent that had been told "claimed" on A had silently lost the bead. Two agents on two machines then worked the same task.

## The approach

### One executable, one manifest

```
  harness bindings              Claude Code skill + hooks, Kestrel (Clojure), Codex, MCP …
  (generated or hand-written)   call the CLI and read its JSON; never re-implement a rule
            │
            ▼
  bops CLI  ── commands.json (embedded) ──► bops gen docs ──► docs/COMMANDS.md
  cmd/bops     the contract: commands, args, outputs, exit codes
            │
            ▼
  internal/intent      next, claim, note, land, release, sweep, stale, sync, lint, session-start, doctor
     │         │              │                 │
     ▼         ▼              ▼                 ▼
  internal/  internal/     internal/          internal/
  bdx        lock          forge              runstate
  bd --json  git refs on   gh / glab:         ~/.local/state/bops/<hash>/
  (all       the remote    "has this PR       run ids, hold journal,
  ledger     (ownership)   merged?"           events.jsonl
  reads and
  writes)
```

[`bops/commands.json`](../bops/commands.json) is the contract: every command, argument, wrapped call and output field. It is compiled into the binary (`go:embed`), printed by `bops manifest`, and rendered to [docs/COMMANDS.md](COMMANDS.md) by `bops gen docs`. CI fails if the rendered file and the manifest disagree, and a golden test pins the generator's output. Bindings are meant to be generated from the same bytes, so a binding and the binary it calls can't disagree about the contract. Today only the `docs` target exists; the Claude Code, Codex, Hermes, Clojure and MCP generators are planned ([SPEC §8](../SPEC.md#8-harness-bindings-generated)), and the current bindings are hand-written.

Rules live in exactly one place. A binding that needs a fact calls a command that returns it. `next` is the only place ordering lives; `lint` and `next` share the one implementation of the workability rules.

### Packages

Each package owns one outside system, so no other code has to know about it:

| Package | Owns | Depends on |
|---|---|---|
| `cmd/bops` | argument parsing, config loading, exit codes, time budgets per command | everything below |
| `internal/intent` | the commands themselves; deterministic given the `bd` runner's replies | `bdx`, `lock`, `forge`, `ledger`, `runstate` |
| `internal/bdx` | every call to `bd`; a `Runner` interface with a real `Exec` and a test `Fake` | none |
| `internal/ledger` | `bd`'s JSON field names and decoding | none |
| `internal/lock` | the claim ref: refnames, message format, push classification, `doctor` | `runstate` |
| `internal/runstate` | per-machine state that must never reach the ledger | none |
| `internal/forge` | what a pull request is (`github`, `gitlab`, `none`) | none |
| `internal/config` | `.bops.json`, user config, `BOPS_*` env | none |
| `internal/manifest` | parsing and validating `commands.json` | none |
| `internal/gen` | binding generators, pure functions of the manifest | `manifest` |

`bops` uses the Go standard library only, and it shells out to `bd --json` rather than linking the beads library, so its contract can be checked against any `bd` build ([ADR 0002](adr/0002-go-reference-implementation.md)). `bdx.Runner` is the seam for opening the ledger in-process later.

### Where each kind of state lives

| State | Lives in | Written by | Merges across machines? |
|---|---|---|---|
| Issue data (title, status, deps, acceptance) | the Dolt ledger | `bd`, always | yes, through Dolt sync |
| Events (`bops:claimed`, `bops:note`, `bops:landed`, `bops:released`) | bead comments | `bops` via `bd comments add` | yes; two replicas' comments both survive |
| Landed evidence (PR URL, sha) | `metadata.bops` on the bead | `bops land` via `bd update --metadata` | yes |
| Who holds a bead | `refs/bops/claims/<id>` on the git remote | `internal/lock` | n/a: one ref, compare-and-set by git |
| Display of the holder (assignee, `run:`, `bops:landing`) | the ledger | `bops`, best effort after the ref | yes, and `sync` repairs it |
| Run identity, hold journal, event log | `$XDG_STATE_HOME/bops/<repo-hash>/` | `internal/runstate` | never leaves the machine |

`bops` never writes issue data outside `bd`, and never adds tables or files to the ledger ([ADR 0001](adr/0001-derived-read-model-not-write-path.md)). Events go in comments rather than the `notes` field because comments merge across Dolt sync and `notes` conflicts.

### The claim lock

Ownership is one git ref per claimed bead, on the same remote the ledger already syncs through. No server, no new service. Its value is a commit on the empty tree whose message records `run`, `actor`, `state` (`held` or `landing`), a random `nonce` and `at`.

A claim is a **create-only push**: `git push --force-with-lease=<ref>: <sha>:<ref>`. Git applies a lease atomically on the server, so when two machines push at once, exactly one wins. Measured on GitHub: one winner in 40 of 40 create races and 20 of 20 takeover races.

Every ref write follows one protocol, inside `internal/lock`:

```
  1. journal "about to push sha X for bead B"   (local state file, under a file lock)
  2. git push --force-with-lease …               (network; no file lock held)
  3. classify: success, or re-read the ref with ls-remote
        holds X            → we won (the acknowledgement was just lost)
        holds another sha  → lost
        unchanged          → unreachable, nothing written
  4. record the result in the journal
  5. then, best effort: bd assignee, run: label, bops:claimed comment, bd dolt push
        failure here → ledger_pending: true; the next `bops sync` repairs it
```

The journal entry is written before the push, so a crash or a lost reply is settled at the next `sync` by reading the ref, rather than guessed. **The ref decides; the ledger displays.** No ownership decision reads the assignee or labels, so a stale display can't cause a double claim.

`next` doesn't hit the network: it reads the copies of claim refs the last `sync` fetched into `refs/bops/seen/*` (lint rule W14). It can be out of date; `claim` is what decides.

Landing and closing are separate on purpose. `release --reason done` rewrites the ref to `state=landing` instead of deleting it, so nobody else can claim a bead whose PR is still open. Only `sweep` (once the forge says the PR merged) or a human closes it.

### Single-machine mode

`lock.mode: none` in the committed `.bops.json` turns the ref off. Claims fall back to `bd update --claim`, which keeps worktrees of one clone apart and nothing more. It exists so a solo repo with no remote can still use the loop. Only the repo file can choose it, so one machine on a shared ledger can't quietly opt out of the lock ([configuration](reference/configuration.md#lockmode)).

## Trade-offs

- **Claims need the network.** `claim`, `release` and `land` refuse with `unreachable` when the remote can't be reached. `note` and the work itself keep going offline. That's the price of a lock that holds across machines.
- **The lock only covers `bops`.** A raw `bd update --claim`, `bd reclaim`, or an older `bops` that predates the lock bypasses it. Every machine on a ledger has to run a lock-aware `bops`. `/bead-loop` still claims through raw `bd` until bead `bops-t8j` moves it.
- **The actor isn't authenticated.** Anyone with push access can write a claim ref under any name. The actor check on `claim --resume` is a safety rail, not access control. Access control is whoever can push to the remote.
- **The forge must accept custom refs.** A host that rejects pushes under `refs/bops/` gives `lock_unsupported`, and there's no fallback in v0.1.0.
- **Two writes per claim.** A ref push, then the ledger display. The display can lag (`ledger_pending`), so raw `bd show` may briefly name the wrong assignee.
- **Shelling out to `bd` costs startup time on every call.** Accepted for v0 so the contract is verifiable against any `bd`; `bdx.Runner` leaves room for an in-process ledger later.
- **The lock is temporary by design.** It's deleted the day `bd` offers a cross-replica compare-and-set.

## Alternatives considered

From the [claim-lock design](designs/bops-claim-lock-v0.1.0.md#approaches-considered):

- **Partition machines by `pool:` label.** Rejected: two machines sharing a pool still race.
- **Reconcile inside `bd`: pull, claim, push, and the loser undoes.** Rejected: the undo is itself a write that can lose to last-write-wins.
- **Wait for a cross-replica compare-and-set in `bd`.** Rejected for v0.1.0 because the loop couldn't wait on upstream's schedule. The lock goes away when that ships.
- **Publish the ledger before `claim` returns.** The first version of the design. Rejected because it needed a five-state recovery table that three review rounds kept finding defects in. Letting the ledger lag behind the ref removed most of those states.

From [ADR 0002](adr/0002-go-reference-implementation.md#options-considered), on where the commands live:

- **A Claude Code plugin only.** Rejected as the home because it locks the semantics into one harness. It survives as a binding generated from the manifest.
- **An MCP server first.** Rejected as the core: 10-50k tokens of tool definitions against about 1-2k for a skill, a 25k-token result cap, and MCP can't serve hooks. It's planned as the last binding.
- **Kestrel-native only, or a fork of `bd` with custom tables.** Rejected: hooks need a zero-daemon binary, and a second write path breaks [ADR 0001](adr/0001-derived-read-model-not-write-path.md).

On language, Go beat TypeScript/Bun (the runner-up), Babashka and POSIX sh: one static binary on every target including Termux, and the same ecosystem as `bd`.

## Related

- [Tutorial: your first bops loop](tutorial-first-loop.md)
- [How to set up the claim lock](howto-claim-lock.md)
- [PROCESS.md §12 recovery runbook](PROCESS.md#12-recovery-runbook-bops-ledgers): what to do when a claim is stuck.
- [`bops/testdata/README.md`](../bops/testdata/README.md): the GitHub race measurements.

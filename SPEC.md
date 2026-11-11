# bops — intent-level command layer for beads

**Spec v0.1 (draft) — 2026-11-04, amended 2026-11-10 for the claim lock — contract=1**
License: MIT. Status: implemented by `bops/` for v0.1.0; contract 1 freezes at the `bops/v0.1.0` tag (§11). This document is the source of truth; implementations conform to it, not the reverse.

## 0. Why this exists

`bd` is a complete, low-level vocabulary: create, update, dep, ready, claim, close, sync. Agents don't think in that vocabulary; they think in intents: *what should I do next, take it, record where I got to, give it back, prove it landed.* Every team that runs agents on beads ends up writing the same dozen wrappers, each one baking the same readiness filters and state conventions into shell, jq, Python, or prose — and each copy drifts.

`bops` is one executable that implements those intents once, with machine-readable output and a published contract, and a manifest from which every harness binding (Claude Code, Codex, Hermes, a Clojure orchestrator, an MCP server) is *generated* rather than hand-maintained.

Non-goals: `bops` is not a tracker, not a sync protocol, not a merge tool, and never a second write path for issue data. All issue state lives in beads; all issue writes go through `bd`. The one exception is claim ownership: `bd`'s compare-and-set holds on one replica only, so two machines can both claim a bead and last-write-wins settles it later. Ownership is therefore a git ref per bead (§3.0), which holds no issue data and is retired when `bd` offers a cross-replica compare-and-set ([ADR 0001 amendment](docs/adr/0001-derived-read-model-not-write-path.md#amendment-2026-11-10-a-git-ref-claim-lock)).

## 1. Principles

1. **The graph is the plan.** No sidecar plan files. If it matters across sessions or machines, it is a bead, a label, a comment, or a `metadata` key. Who holds a bead is the one fact kept beside the graph, in its claim ref (§3.0).
2. **Mechanics in the executable, judgment in prose.** `bops` returns facts and deterministic verdicts. "Is this bead worth doing" stays in the harness prompt; "is this bead *workable*" is a `bops` verdict.
3. **One implementation of every rule.** Bindings never re-implement a filter, regex, or ordering. If a binding needs a fact, there is a command that returns it.
4. **Writes only via `bd`.** Including the Go library or `bd serve` HTTP as sanctioned surfaces. Never direct SQL writes, never custom tables.
5. **Comments, not notes, for events.** Comments merge across Dolt sync; the `notes` field conflicts. Notes are reserved for human-authored content. Comments added to one bead on two replicas both survive the merge (measured on bd 1.3.4, 2026-11-10), but a comment exists elsewhere only once its replica pushes, so a comment is a record, never the source of a decision.
6. **Per-machine state stays off the ledger.** Lease caches, watermarks, journal cursors live in `$XDG_STATE_HOME/bops/<repo-hash>/`.
7. **Text never on the command line.** Bodies, comments, and reasons arrive via stdin or `--file`; arguments are IDs, flags, enums. The one exception is the generated, fixed-vocabulary event line (`bops:claimed run=… actor=… machine=…`), which contains no user- or bead-authored text.
8. **Fail closed on writes, fail open on reads.** A write that cannot verify its precondition does nothing and exits 1. A read that cannot reach the ledger says so and exits 1; it never fabricates.
9. **Contract-versioned.** `bops version` prints `contract=<n>`; any change to output shape, exit semantics, or state conventions bumps `n`. Bindings pin a contract. Waiver: contract 1 changed before release (the claim lock, exit 3, outcomes and codes, `--idempotency-key` removed) without a bump, because no tag or external binding had pinned it. From the `bops/v0.1.0` tag on, the rule applies without exception.

## 2. Glossary (normative)

| Term | Meaning | Not to be confused with |
|---|---|---|
| **ready** | `bd ready` says all blocking deps are satisfied and status permits work | workable |
| **workable** | ready **and** passes the lint rules in §6 (has acceptance, not human-gated, machine-eligible, no stale claim, …) | ready |
| **claim** | taking a bead for a run by creating its claim ref with a create-only push; the ref *is* the lock, and the assignee and `run:` label are a display of it | lease |
| **claim ref** | `refs/bops/claims/<id>` on the lock remote (`lock.remote`, default `origin`), naming the holder's run, actor, state (`held` or `landing`) and time (§3.0) | assignee |
| **holder** | the run the claim ref names; only the holder may note, land or release | assignee |
| **lease** | beads' node-local TTL on a claim (`bd heartbeat` / `bd reclaim`); not committed to Dolt, and not used by `bops` to decide ownership | claim |
| **run** | one worktree's execution of an agent loop on one machine; identified by a ULID kept in per-clone state and shown as a `run:` label | session |
| **note** | milestone state written while *keeping* the claim; also refreshes the claim ref's time | release |
| **release** | returning a claim with a classified reason; the claim ref is deleted and the bead goes back to the pool, except `done`, which keeps the ref as `landing` | close |
| **land** | recording evidence that work reached its destination (PR/MR URL + sha); **never** performs the merge | close |
| **landing** | a bead released `done` whose PR is not yet merged; its ref has `state=landing` and only `sweep` or `claim --resume` acts on it | held |
| **close** | terminal status; only `bops sweep` or a human closes, never a worker mid-run | land |
| **gate** | a beads gate (`bd gate`: human, timer, CI, PR) that blocks progress | dep |
| **pool** | an opt-in partition of ready work via `pool:<name>` label; a run serves one pool | machine label |
| **machine label** | a capability constraint (`needs:<cap>`) matched against the run's declared capabilities | pool |

## 3. State conventions on the ledger

All keys are namespaced so that `bops` can coexist with any other tooling on the same ledger.

### 3.0 The claim lock (ownership)

One git ref per claimed bead, `refs/bops/claims/<id>`, on the lock remote: the git remote named by `lock.remote` (§10, default `origin`), which must be the same repository as the Dolt ledger. `claim` refuses with `no_origin` when no git remote has that name, and with `remote_mismatch` when its URL is not the Dolt remote. `doctor` checks the same remote. The ref's value is a commit on the empty tree, authored `bops <bops@invalid>`, whose message is one `key=value` per line:

```
run=<ulid>
actor=<actor>
state=held|landing
nonce=<random>
at=<iso8601>
```

- Every write is `git push --no-verify --force-with-lease=<ref>:<expected>`: create-only for a claim, leased on the sha last read for every other write. `-f` is never used. Of two writers, exactly one wins; the other gets `lost`.
- **The ref decides; the ledger displays.** The assignee, `run:` label and `bops:landing` label are written best effort after the ref, can lag it (`ledger_pending`), and are repaired by `sync`. No ownership decision reads them.
- `actor` is unauthenticated free text (`BEADS_ACTOR`, else git `user.name`, else the OS user), and anyone with push access can write these refs. The actor check on `claim --resume` is a safety check, not authentication.
- A ref message is untrusted input: unknown keys are ignored, size is bounded, and an unreadable or future `at` never counts as stale.
- Staleness is `stale_minutes` (§10, default 120) from the ref's `at`. `note` refreshes `at`. Set it in the committed `.bops.json` so every machine on a ledger measures the same window: a machine with a shorter one takes over claims sooner.
- **The guarantee covers claims made through `bops`.** A raw `bd update --claim` writes no ref and bypasses the lock. `/bead-loop` and the Kestrel binding still claim through raw `bd` until bead `bops-ecs` moves them, so they are outside the guarantee. Every machine on a ledger must run a `bops` that knows the lock before the first claim on it.

**`lock.mode: none` (one machine only).** A ledger used on a single machine, with or without a git remote, can turn the lock off in the committed `.bops.json` (`{"lock": {"mode": "none"}}`, §10). No command then reads or writes a claim ref, contacts a remote for locking, or refuses with `no_origin` or `remote_mismatch`; ownership is the ledger alone, as for a claim with no ref:

- `claim` pulls best effort, checks the same preconditions, and claims with `bd update --claim`, whose compare-and-set holds on one replica, so it separates the worktrees of one clone and nothing more. If that write fails and the bead now has an assignee, the result is `lost` with that holder. The hold is journaled with no sha. The `run:` label, `bops:claimed` comment and Dolt push follow best effort, and their failures are warnings; nothing sets `ledger_pending`.
- `claim --force` measures staleness from the ledger claim's `updated_at` (as with no ref) and takes over with `bd update --assignee --status in_progress --force`. Two concurrent takeovers are not separated. `claim --resume` is refused: its rule is the ref's state and actor, which do not exist here. To rework a landing bead, remove `bops:landing`, set it `open`, and claim it.
- `release` and `land` check the ledger instead of the ref: a `run:` label of another run (or, with no `run:` label, another assignee) is `lost` with no write. `release` never deletes a ref.
- `sweep` takes the no-ref path for every bead: `bops:landing` and a merged PR close it. `stale` ages every claim from the ledger and reports no orphans. `next` skips W14's ref check. `sync` and `session-start` report no `lost_claims`; a stranded hold is still reported.
- Every one of these results carries the warning `lock.mode is none: …`, so an unprotected claim is not mistaken for a protected one. `doctor` reports all four checks `skip`, naming `lock.mode`, and exits 0.
- Only `.bops.json` can set `none`: a per-user file or `BOPS_LOCK_MODE` that sets it is an error unless the repo file already does. One machine of a shared ledger turning the lock off would claim beads others hold without anyone being told. Either layer may set `ref`.
- Switch modes with nothing claimed. Holds journaled under `ref` are released through the ledger under `none` and their refs stay on the remote; holds taken under `none` have no ref, and under `ref` are treated like claims made before the lock.

### 3.1 Labels (machine-readable state)

| Label | Set by | Meaning |
|---|---|---|
| `run:<ulid>` | `claim` | display of the run holding the bead; removed by `release`/`sweep`. The claim ref is authoritative |
| `bops:landing` | `release --reason done` | display of a landing ref; removed by `claim --resume` and `sweep` |
| `pool:<name>` | humans / `bd` | partition; absence = default pool |
| `needs:<cap>` | humans | required machine capability (`gpu`, `macos`, `net:internal`, …) |
| `needs:human` | humans / `release --reason human` | excluded from `next` until removed |
| `bops:hold` | `release --reason abandoned|scope` | cooling-off; excluded from `next` for `hold_minutes` (default 120). A label has no timestamp, so the window runs from the bead's `updated_at`: a later write extends it, and a bead with no `updated_at` stays held |

### 3.2 Comments (append-only events)

Each `bops` write adds exactly one comment whose first line is a typed event, so any consumer can reconstruct history with `bd comments <id> --json` (there is no `bd comments list`):

```
bops:claimed run=<ulid> actor=<actor> machine=<name> [takeover=<old-run> | resumed=<old-run>]
bops:note run=<ulid>
<free text, ≥1 line; last paragraph SHOULD start with NEXT: / BLOCKER: / KEY DECISION: / REF:>
bops:released run=<ulid> reason=<done|blocked|scope|abandoned|human> 
bops:landed run=<ulid> ref=<url> sha=<sha>
bops:heartbeat run=<ulid>            (only if lease heartbeats are configured to be recorded)
```

### 3.3 `metadata` keys (structured, merge-safe)

```json
{ "bops": {
    "contract": 1,
    "landed":  [{ "ref": "<url>", "sha": "<sha>", "run": "<ulid>", "at": "<iso8601>" }],
    "acceptance_checked_at": "<iso8601>"
} }
```

`metadata.bops` is the only key `bops` owns. It never writes elsewhere in `metadata`.

### 3.4 Per-machine state (`$XDG_STATE_HOME/bops/<repo-hash>/`)

`<repo-hash>` is the first 8 bytes, in hex, of the SHA-256 of the clone's git common directory, so every worktree of one clone shares one directory and each worktree is its own run.

- `state.json` (version 2): one run per worktree (id, actor, machine, pool, capabilities, started_at), each with a hold journal keyed by bead: `{sha, intended, phase: pushing|held, ledger_pending, delete_pending}`. Every claim-ref write is journalled before it is pushed and settled after, so a crash or a lost acknowledgement is resolved by `sync`. A file with a newer version is refused. A legacy `held` list still decodes; its holds have no sha and are never reported lost.
- `lock`: the write lock, held only around file writes and never across a network call. It carries an owner token, and a lock older than its stale limit is taken over.
- `events.jsonl`: one line per step and outcome of every ownership command, rotated at 1 MB to one old copy. It is local and is the first thing to read when an outcome needs explaining.
- `refs/bops/seen/*` (in the clone, not this directory): the last fetched copy of every claim ref, written by `sync`, so `next` can apply W14 without the network.

Reserved for later, not written by v0.1.0: `watermark`, `journal.cursor`, `leases.json`.

## 4. Command manifest

The manifest is [`bops/commands.json`](bops/commands.json). It is embedded in the binary, printed by `bops manifest`, consumed by the generator in §8, and rendered to [`docs/COMMANDS.md`](docs/COMMANDS.md), which is golden-tested. It is the normative list of commands, arguments, wrapped calls and output fields; this section states only what is common to every command. Common to every command:

- `--json` (default when stdout is not a TTY) → one JSON document on stdout. Human mode prints the same facts as `key=value` lines.
- Exit codes: **0** done, or a read's verdict; **1** could not run (remote or ledger unreachable, precondition unverifiable, `bd` error); **2** usage; **3** a write's negative verdict (`lost`, `refused`), with `ok:false`, so `bops claim x && work` cannot proceed. Diagnostics on stderr only.
- Every JSON result carries `{"contract":1,"command":"<name>","ok":<bool>, ...}`. A failure that could not run also carries a stable `code` (below) and an `error` with credentials scrubbed.
- Budgets: `claim`, `release`, `note` and `session-start` have 60 s, `sync` 2 min, `sweep` 5 min, every other command 30 s. Each git call has its own 20 s limit and runs without a terminal prompt.
- `gates` and `deps` are specified but not yet implemented; they print so on stderr and exit 1 without running.

**Ownership outcomes** (`claim`, `release`, `note`, `land`):

| `outcome` | Exit | `ok` | Meaning for the agent |
|---|---|---|---|
| `claimed` | 0 | true | You own it. `ledger_pending: true` means the display catches up at `sync` |
| `held` | 0 | true | This run already owns it; carry on |
| `released` | 0 | true | Given back. `ref_pending: true` means `sync` finishes deleting the ref |
| `noted`, `landed` | 0 | true | Written |
| `lost` | 3 | false | Someone else holds it; pick another bead. `holder` names their actor, run and age |
| `refused` | 3 | false | A precondition failed; pick another bead. `reason` says which (`detail` on `release`, whose `reason` is the release reason) |
| `unreachable` | 1 | false | Could not verify; stop or retry. `retry` bounds it and `side_effects` says what may have been written |

**Stable codes** (`code` on a result that could not run, and in `events.jsonl`): `unreachable` (remote not reached), `git_auth` (credentials refused), `lock_unsupported` (the forge rejects custom refs), `ref_contention` (a local ref update collided), `state_write_failed` (the state file could not be written), `hold_not_recorded` (the ref was won but the hold could not be saved; the result names the sha and how to recover), `no_origin` (lock remote missing: no git remote named `lock.remote`, default `origin`) and `remote_mismatch` (the lock remote is not the Dolt remote), `bad_id` (not a bead id), `bad_message` (an unreadable claim ref), `git_failed` (any other git error).

## 5. Ordering rules for `next`

Deterministic, documented, and the only place ordering lives:

1. Exclude: not ready; epics; any failing lint rule in §6 (`needs:human`, `bops:hold` within window, `pool:` ≠ run's pool, any `needs:<cap>` not in run's caps, open gates, a live claim); and W14, a claim ref seen at the last `sync` that names another run. W14 reads `refs/bops/seen/*`, so `next` needs no network and is only as fresh as the last `sync`; `claim` is what decides.
2. Order: priority ascending (0 first) → beads whose parent epic already has in-progress siblings (finish what's started) → oldest `created_at` → ID. If the in-progress listing fails, `next` still returns candidates without that step and says so in `warnings`.
3. `why` lists which rules passed; `why_not` lists which rules excluded it (only with `--include-unworkable`).

The harness decides *among* candidates; `bops` never picks silently. `claim` is a separate call so the agent's choice is visible in the transcript.

## 6. Lint rules (workability)

Each rule has an id, a severity (`fail`|`warn`), and a one-line rationale; `--strict` promotes warns. Initial set (ids stable; add, don't renumber):

| id | severity | rule |
|---|---|---|
| W01 | fail | has non-empty acceptance criteria |
| W02 | fail | status permits work (open/ready; not blocked/closed) |
| W03 | fail | not `needs:human` |
| W04 | fail | machine capability constraints satisfiable by this run |
| W05 | fail | no open gates on the bead |
| W06 | fail | not claimed by a live run (claimed by stale run → warn W06s) |
| W07 | warn | has a parent epic (orphans are allowed but flagged) |
| W08 | warn | body under configured size (default 8 kB) — oversized beads overflow agent context |
| W09 | warn | last `bops:note` ends with a `NEXT:` paragraph (resume readability) |
| W10 | warn | no sibling bead with the same title (duplicate smell) |
| W11 | fail | no dependency cycle reachable (delegates to `bd dep cycles`) |
| W12 | fail | not labelled `bops:hold` (cooling off after a scope/abandoned release) |
| W13 | fail | in this run's pool: a run serves exactly one pool; unlabelled beads are the default pool, and a pooled run never takes default-pool beads |
| W14 | fail | no claim ref held by another run, as seen at the last `sync` (evaluated by `next`, which has the refs) |

`W06s` is the stale variant of W06: the bead carries a claim, but its last heartbeat/update is older than `stale_minutes` (default 120). It is a *warn*, so a stale claim is a candidate again; `claim` on it still requires `--force`, which records the takeover in the event comment. `claim --force` measures staleness from the claim ref's `at` (§3.0), and from `updated_at` only for a ledger claim with no ref.

W12's window runs from the bead's `updated_at`, because a label carries no timestamp. A bead with no `updated_at` stays held.

Rules the bead alone cannot answer (W09 needs comments, W10 needs the whole list, W11 needs bd) are evaluated by the commands that have that data; `bops lint <id>` reports them as not evaluated rather than passed.

## 7. Forge adapter

Only `sweep` (and optionally `land --verify`) touch a forge. Adapter interface: `is_merged(ref) → true|false|unknown`, `ref_sha(ref)`. Implementations: `github` (`gh api`), `gitlab` (`glab api`), `none`. Selection: `--forge` when given, else `forge` in config (§10), else (`auto`) inferred from the host of `git remote get-url <lock.remote>` (default `origin`), github or gitlab by name and `none` otherwise or when that remote is missing. Inference reads `lock.remote` even under `lock.mode: none`: it is the remote the repo is configured to share through. Nothing else in `bops` knows what a PR is.

## 8. Harness bindings (generated)

The generator reads `commands.json` and emits:

| Target | Emits | Notes |
|---|---|---|
| Claude Code | `skills/bops/SKILL.md` (hand-written preamble + generated reference), `hooks.json` fragment (`SessionStart → bops session-start`, `PreCompact → bops note` prompt, `Stop → bops sync`), optional `/bops-next` command | ≤2k tokens; the model runs CLI commands |
| Codex | `.codex/hooks.json` fragment + `.agents/skills/bops/SKILL.md` | same prose |
| Hermes | `tools/bops.json` tool descriptors mapping to CLI invocations | format TBD after Hermes research |
| Clojure (Kestrel or any) | `bops.clj`: one fn per command shelling out, Malli schemas generated from `output` | in-process option later |
| MCP | `mcp/tools.json`: one tool per command; `kind: write` tools omitted unless `--enable-writes`; results paginated at 20 kB | respects CC's 25k-token result cap |
| Docs | `docs/COMMANDS.md` | |

Bindings carry `contract=<n>` and refuse to run against a mismatched `bops version`.

## 9. Testing contract

- Every command has a test suite that stubs `bd` (and `gh`/`glab`) with a fake returning fixture JSON; no suite requires Dolt.
- One integration suite runs against a throwaway `bd init` ledger in CI, gated behind `BOPS_INTEGRATION=1`.
- A two-replica suite runs in the same CI job against real `bd` (pinned, checksum-verified) and a bare git remote: two clones race to claim one bead at least 40 times with distinct actors and 40 with one shared actor, and every trial must end with exactly one `claimed` and one `lost`. It also covers takeover, resume, worktrees, a slow remote, a failing pre-push hook, offline `note`, ledger repair by `sync`, and `sweep`. `BOPS_RACE_TRIALS` sets the trial count.
- Golden-file tests for every generated binding; a change to a golden file is a contract review.
- A suite exists for each bug that shipped — pin the bug, then fix it.

## 10. Configuration

`.bops.json` in repo root (committed) merged with `$XDG_CONFIG_HOME/bops/config.json` (per user) and `BOPS_*` env. Keys: `pool`, `caps[]`, `hold_minutes`, `stale_minutes`, `forge`, `lint.strict`, `lint.disable[]`, `note.require_next` (bool), `lock.mode`, `lock.remote`.

Precedence, lowest to highest: built-in defaults, `.bops.json` in the worktree root, the per-user file (`~/.config/bops/config.json` when `XDG_CONFIG_HOME` is unset), `BOPS_*` env, then command flags. Each layer sets only the keys it names; a missing file is skipped. Nested keys are JSON objects (`{"lint": {"strict": true}}`), and each key's variable is `BOPS_` plus its path in upper case with `.` as `_` (`BOPS_STALE_MINUTES`, `BOPS_LINT_STRICT`); a list in the environment is comma-separated and an empty variable is unset. An unknown key, a wrong type, a non-integer or non-positive minute count, a `forge` outside `auto|github|gitlab|none`, a `lock.mode` outside `ref|none`, `lock.mode: none` from a layer above `.bops.json` when the repo file does not set it, or a `lock.remote` that is not a plausible git remote name (a letter or digit, then up to 99 letters, digits, `.`, `_` or `-`; no `..`, not ending in `.` or `.lock`) fails the command (exit 1) naming the file or variable. The loader is `bops/internal/config`.

| Key | Default | Applied by |
|---|---|---|
| `stale_minutes` | 120 | `next` (W06s), `claim --force`, `stale` when `--older-than` is not given, `session-start` |
| `hold_minutes` | 120 | `next` and `lint` (W12) |
| `pool`, `caps[]` | none | every command's run; `next --pool/--caps` override |
| `forge` | `auto` | `land --verify` and `sweep` when `--forge` is not given (§7) |
| `lint.strict`, `lint.disable[]` | `false`, none | loaded and validated; not applied yet (`lint --strict` is the flag) |
| `note.require_next` | `false` | loaded and validated; not applied yet |
| `lock.mode` (`BOPS_LOCK_MODE`) | `ref` | every command and `doctor`: `none` turns the claim lock off for a one-machine ledger (§3.0). Only `.bops.json` may set `none` |
| `lock.remote` | `origin` | every command that reads or writes claim refs, the ledger check (§3.0) and `doctor` (`BOPS_LOCK_REMOTE`), and forge inference (§7); the lock itself is unused under `lock.mode: none` |

Other environment `bops` reads: `BEADS_ACTOR` (actor), `BOPS_MACHINE` (machine name, else the hostname), `XDG_STATE_HOME` (§3.4), and `BOPS_INTEGRATION` / `BOPS_RACE_TRIALS` in tests.

## 11. Versioning and compatibility

- `bops` semver; `contract` integer. Minor releases may add commands/fields; removing or renaming anything bumps `contract`. Contract 1 freezes at the `bops/v0.1.0` tag (§1.9 waiver).
- Supported `bd`: ≥ 1.3 (`bd update --claim`, `--assignee --status --force` for display repair, `bd heartbeat`, `bd comments <id> --json`); CI pins and tests 1.3.4. `bd reclaim` ignores the claim lock, so it is not run on a ledger `bops` claims on. `bops version` reports the detected `bd` and backend and warns outside the tested range.
- Supported git remotes: any that accept custom refs under `refs/bops/` with `--force-with-lease`. GitHub is measured. A forge that rejects them gives `code: lock_unsupported`.
- Upgrade every machine on a ledger before the first claim with a new `bops`: an older binary ignores the lock.
- JSONL-only (`no-db`) beads: read commands work; write commands refuse (no CAS) unless `--unsafe-no-cas`.

## 11a. Reference implementation

- **Language: Go.** Single static binary per platform (Linux, macOS, Windows, android/arm64 for Termux), no runtime, same ecosystem as `bd` itself. Module lives at `bops/` in this repo (`github.com/extremenelson/beads-ops/bops`).
- **Ledger access.** v0 shells out to `bd --json` so the contract is verifiable against any `bd` build. The `internal/bdx` package is the only place that knows how; it is an interface so a later implementation can open the Dolt ledger once per process via the beads Go library (`OpenBestAvailable`) and avoid embedded-engine startup on every call. Writes stay on `bd`/the library — never raw SQL.
- **Manifest is embedded** (`go:embed commands.json`); `bops manifest` prints it; `bops gen <target>` runs the binding generators from the same bytes the binary was built with, so a binary and its bindings can never disagree about the contract.
- **Dependencies.** Standard library only for v0 (`flag`, `encoding/json`, `os/exec`). Each added module must justify itself in `CONTRIBUTING.md`.
- **Tests.** `go test ./...` runs unit + golden tests with a fake `bd` (`bdx.Fake`); `BOPS_INTEGRATION=1 go test ./...` adds the throwaway-ledger suite.

## 12. Roadmap after contract 1

- `deps --engine graph`: Datalevin/SQLite read model built from `bd export --all` + events journal; cross-machine `stale` with lease reconstruction from comments.
- Formula-aware `next`: prefer the next step of a molecule the run is already inside.
- `bops explain <id>`: one screen of why a bead is where it is (events + gates + deps), for humans.

## 13. Provenance note

This specification was written from the behaviour of prior wrappers the author built and from the public beads documentation; it contains no code from, and the reference implementation will be written without reference to, any prior employer-owned implementation. The spec and its commit history are public to make that provenance auditable.

# bops — intent-level command layer for beads

**Spec v0.1 (draft) — 2026-11-04 — contract=1**
License: MIT. Status: pre-implementation. This document is the source of truth; implementations conform to it, not the reverse.

## 0. Why this exists

`bd` is a complete, low-level vocabulary: create, update, dep, ready, claim, close, sync. Agents don't think in that vocabulary; they think in intents: *what should I do next, take it, record where I got to, give it back, prove it landed.* Every team that runs agents on beads ends up writing the same dozen wrappers, each one baking the same readiness filters and state conventions into shell, jq, Python, or prose — and each copy drifts.

`bops` is one executable that implements those intents once, with machine-readable output and a published contract, and a manifest from which every harness binding (Claude Code, Codex, Hermes, a Clojure orchestrator, an MCP server) is *generated* rather than hand-maintained.

Non-goals: `bops` is not a tracker, not a sync protocol, not a merge tool, and never a second write path. All durable state lives in beads; all writes go through `bd`.

## 1. Principles

1. **The graph is the plan.** No sidecar plan files. If it matters across sessions or machines, it is a bead, a label, a comment, or a `metadata` key.
2. **Mechanics in the executable, judgment in prose.** `bops` returns facts and deterministic verdicts. "Is this bead worth doing" stays in the harness prompt; "is this bead *workable*" is a `bops` verdict.
3. **One implementation of every rule.** Bindings never re-implement a filter, regex, or ordering. If a binding needs a fact, there is a command that returns it.
4. **Writes only via `bd`.** Including the Go library or `bd serve` HTTP as sanctioned surfaces. Never direct SQL writes, never custom tables.
5. **Comments, not notes, for events.** Comments merge across Dolt sync; the `notes` field conflicts. Notes are reserved for human-authored content.
6. **Per-machine state stays off the ledger.** Lease caches, watermarks, journal cursors live in `$XDG_STATE_HOME/bops/<repo-hash>/`.
7. **Text never on the command line.** Bodies, comments, and reasons arrive via stdin or `--file`; arguments are IDs, flags, enums. The one exception is the generated, fixed-vocabulary event line (`bops:claimed run=… actor=… machine=…`), which contains no user- or bead-authored text.
8. **Fail closed on writes, fail open on reads.** A write that cannot verify its precondition does nothing and exits 1. A read that cannot reach the ledger says so and exits 1; it never fabricates.
9. **Contract-versioned.** `bops version` prints `contract=<n>`; any change to output shape, exit semantics, or state conventions bumps `n`. Bindings pin a contract.

## 2. Glossary (normative)

| Term | Meaning | Not to be confused with |
|---|---|---|
| **ready** | `bd ready` says all blocking deps are satisfied and status permits work | workable |
| **workable** | ready **and** passes the lint rules in §6 (has acceptance, not human-gated, machine-eligible, no stale claim, …) | ready |
| **claim** | atomic assignment of a bead to an actor+run via compare-and-swap; the claim *is* the lock | lease |
| **lease** | beads' node-local TTL on a claim (`bd heartbeat` / `bd reclaim`); not committed to Dolt | claim |
| **run** | one execution of an agent loop on one machine; identified by a `run:` label value | session |
| **note** | milestone state written while *keeping* the claim | release |
| **release** | returning a claim with a classified reason; the bead goes back to the pool | close |
| **land** | recording evidence that work reached its destination (PR/MR URL + sha); **never** performs the merge | close |
| **close** | terminal status; only `bops sweep` or a human closes, never a worker mid-run | land |
| **gate** | a beads gate (`bd gate`: human, timer, CI, PR) that blocks progress | dep |
| **pool** | an opt-in partition of ready work via `pool:<name>` label; a run serves one pool | machine label |
| **machine label** | a capability constraint (`needs:<cap>`) matched against the run's declared capabilities | pool |

## 3. State conventions on the ledger

All keys are namespaced so that `bops` can coexist with any other tooling on the same ledger.

### 3.1 Labels (machine-readable state)

| Label | Set by | Meaning |
|---|---|---|
| `run:<ulid>` | `claim` | the run currently holding the bead; removed by `release`/`sweep` |
| `pool:<name>` | humans / `bd` | partition; absence = default pool |
| `needs:<cap>` | humans | required machine capability (`gpu`, `macos`, `net:internal`, …) |
| `needs:human` | humans / `release --reason human` | excluded from `next` until removed |
| `bops:hold` | `release --reason stale|scope` | cooling-off; excluded from `next` for `hold_minutes` (config, default 120) |

### 3.2 Comments (append-only events)

Each `bops` write adds exactly one comment whose first line is a typed event, so any consumer can reconstruct history with `bd comments list --json`:

```
bops:claimed run=<ulid> actor=<actor> machine=<name>
bops:note run=<ulid>
<free text, ≥1 line; last paragraph SHOULD start with NEXT: / BLOCKER: / KEY DECISION: / REF:>
bops:released run=<ulid> reason=<done|blocked|scope|stale|human> 
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

`run.json` (current run id, pool, capabilities, started_at), `watermark` (last-synced ledger revision), `journal.cursor` (beads events-journal position, for a future read model), `leases.json` (local mirror of heartbeat times).

## 4. Command manifest

Normative shape of `commands.json`; the generator in §8 consumes it. Common to every command:

- `--json` (default when stdout is not a TTY) → one JSON document on stdout. Human mode prints the same facts as `key=value` lines.
- Exit codes: **0** verdict produced (even a negative one), **1** could not run (ledger unreachable, precondition unverifiable, `bd` error), **2** usage. Diagnostics on stderr only.
- Every JSON result carries `{"contract":1,"command":"<name>","ok":<bool>, ...}`.
- Idempotency: writes accept `--idempotency-key <k>`; a repeat with the same key is a no-op returning the original result.

```json
{
  "contract": 1,
  "commands": [
    {
      "name": "next",
      "intent": "What should I work on?",
      "kind": "read",
      "args": { "pool": "string?", "caps": "string[]?", "n": "int=3", "include-unworkable": "bool=false" },
      "wraps": ["bd ready --json", "bd show --json", "bd gate list --json"],
      "output": {
        "candidates": [{ "id": "string", "title": "string", "priority": "int", "type": "string",
                         "workable": "bool", "why": "string[]", "why_not": "string[]",
                         "blocked_by_gates": "string[]", "parent": "string?" }],
        "excluded_count": "int", "ledger_revision": "string"
      },
      "surfaces": { "cc": ["skill", "hook:SessionStart"], "kestrel": "fn", "hermes": "tool", "mcp": { "readOnly": true } }
    },
    { "name": "claim",  "intent": "Take this bead for this run", "kind": "write",
      "args": { "id": "string", "force": "bool=false" },
      "preconditions": ["assignee is empty OR --force", "status in (open, ready)", "no run: label"],
      "wraps": ["bd update <id> --claim --if-assignee ''", "bd label add run:<ulid>", "bd comments add", "bd dolt push"],
      "output": { "id": "string", "run": "string", "claimed_at": "string", "readback_ok": "bool" },
      "surfaces": { "cc": ["skill"], "kestrel": "fn", "hermes": "tool", "mcp": { "readOnly": false } } },
    { "name": "note",   "intent": "Record where I am, keep the claim", "kind": "write",
      "args": { "id": "string", "text": "stdin|--file" },
      "preconditions": ["run: label matches current run"],
      "lint": ["last paragraph starts with NEXT:|BLOCKER:|KEY DECISION:|REF: (warn, not fail)"],
      "wraps": ["bd comments add", "bd heartbeat"],
      "output": { "id": "string", "comment_id": "string", "warnings": "string[]" } },
    { "name": "release", "intent": "Give the claim back", "kind": "write",
      "args": { "id": "string", "reason": "enum[done,blocked,scope,stale,human]", "text": "stdin|--file?" },
      "effects": { "blocked": "status→blocked", "human": "+needs:human", "stale|scope": "+bops:hold", "done": "none (await land/sweep)" },
      "wraps": ["bd update --assignee ''", "bd label remove run:*", "bd comments add", "bd dolt push"],
      "output": { "id": "string", "reason": "string" } },
    { "name": "land",   "intent": "Record that work reached its destination", "kind": "write",
      "args": { "id": "string", "ref": "url", "sha": "string" },
      "wraps": ["bd comments add", "bd update --metadata (merge bops.landed)", "bd dolt push"],
      "never": ["merge", "push a branch", "close"],
      "output": { "id": "string", "landed": "object" } },
    { "name": "sweep",  "intent": "Close landed beads whose refs are merged", "kind": "write",
      "args": { "dry-run": "bool=true", "forge": "enum[auto,github,gitlab,none]=auto" },
      "wraps": ["forge adapter: is-merged(ref)", "bd close --reason 'merged via <ref> (<sha>)'"],
      "output": { "closed": "string[]", "pending": "string[]", "unknown": "string[]" } },
    { "name": "stale",  "intent": "Claims nobody is working", "kind": "read",
      "args": { "older-than": "duration=2h", "all-machines": "bool=true" },
      "wraps": ["bd list --status in_progress --json", "bd comments list --json"],
      "output": { "stale": [{ "id": "string", "run": "string", "last_event_at": "string", "age_minutes": "int", "same_machine": "bool" }] } },
    { "name": "lint",   "intent": "Is this bead workable?", "kind": "read",
      "args": { "id": "string", "strict": "bool=false" },
      "output": { "id": "string", "workable": "bool", "fails": "rule[]", "warns": "rule[]", "passes": "string[]" } },
    { "name": "sync",   "intent": "Pull, heartbeat, push — with the one retry", "kind": "write",
      "args": { "no-push": "bool=false" },
      "wraps": ["bd dolt pull", "bd heartbeat (for held claims)", "bd dolt push"],
      "retry": { "on": "dolt lock", "times": 1, "after_ms": 5000 },
      "output": { "pulled": "bool", "pushed": "bool", "revision": "string", "unpublished": "int" } },
    { "name": "session-start", "intent": "Orient a fresh agent session", "kind": "read+sync",
      "wraps": ["sync", "bd prime", "stale --older-than 2h", "my in_progress"],
      "output": { "mine": "string[]", "stale": "int", "ready_count": "int", "prime": "string" } },
    { "name": "gates",  "intent": "What is blocking this bead's path?", "kind": "read",
      "args": { "id": "string" },
      "wraps": ["bd gate list --json", "bd dep list --json"],
      "output": { "gates": [{ "id": "string", "type": "string", "on": "string", "satisfied": "bool" }], "blocking_deps": "string[]" } },
    { "name": "deps",   "intent": "Blockers, dependents, longest path", "kind": "read",
      "args": { "id": "string", "transitive": "bool=false", "path": "bool=false", "engine": "enum[bd,cte,graph]=bd" },
      "output": { "blocks": "string[]", "blocked_by": "string[]", "critical_path": "string[]?", "depth": "int" } },
    { "name": "version", "kind": "read", "output": { "bops": "semver", "contract": "int", "bd": "string", "backend": "string" } }
  ]
}
```

## 5. Ordering rules for `next`

Deterministic, documented, and the only place ordering lives:

1. Exclude: not ready; `needs:human`; `bops:hold` within window; `pool:` ≠ run's pool; any `needs:<cap>` not in run's caps; open gates; `run:` label present (unless stale and `--include-stale`).
2. Order: priority ascending (0 first) → beads whose parent epic already has in-progress siblings (finish what's started) → oldest `created_at` → ID.
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
| W12 | fail | not labelled `bops:hold` (cooling off after a scope/stale release) |
| W13 | fail | in this run's pool: a run serves exactly one pool; unlabelled beads are the default pool, and a pooled run never takes default-pool beads |

`W06s` is the stale variant of W06: the bead carries a claim, but its last heartbeat/update is older than `stale_minutes` (default 120). It is a *warn*, so a stale claim is a candidate again; `claim` on it still requires `--force`, which records the takeover in the event comment.

Rules the bead alone cannot answer (W09 needs comments, W10 needs the whole list, W11 needs bd) are evaluated by the commands that have that data; `bops lint <id>` reports them as not evaluated rather than passed.

## 7. Forge adapter

Only `sweep` (and optionally `land --verify`) touch a forge. Adapter interface: `is_merged(ref) → true|false|unknown`, `ref_sha(ref)`. Implementations: `github` (`gh api`), `gitlab` (`glab api`), `none`. Selection: `--forge`, else `bops.forge` in config, else inferred from `git remote get-url origin`. Nothing else in `bops` knows what a PR is.

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
- Golden-file tests for every generated binding; a change to a golden file is a contract review.
- A suite exists for each bug that shipped — pin the bug, then fix it.

## 10. Configuration

`.bops.json` in repo root (committed) merged with `$XDG_CONFIG_HOME/bops/config.json` (per user) and `BOPS_*` env. Keys: `pool`, `caps[]`, `hold_minutes`, `stale_minutes`, `forge`, `lint.strict`, `lint.disable[]`, `note.require_next` (bool).

## 11. Versioning and compatibility

- `bops` semver; `contract` integer. Minor releases may add commands/fields; removing or renaming anything bumps `contract`.
- Supported `bd`: ≥ 1.3 (claims via `--if-assignee`, `bd heartbeat`/`bd reclaim`, events journal). `bops version` reports the detected `bd` and backend and warns outside the tested range.
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

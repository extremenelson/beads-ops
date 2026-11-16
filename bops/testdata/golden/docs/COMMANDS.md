# bops commands (contract 1)

Generated from `bops/commands.json` by `bops gen docs`. Do not edit by hand.

Exit codes: **0** = done, or a read's verdict; **1** = could not run: remote or ledger unreachable, precondition unverifiable, bd error; **2** = usage; **3** = negative verdict on a write: lost or refused, ok:false.

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
| `sync` | Pull, heartbeat, push — with the one retry | write |
| `session-start` | Orient a fresh agent session | read+sync |
| `doctor` | Can the claim lock work against its remote (lock.remote, default origin)? | read |
| `gates` | What is blocking this bead's path? | read |
| `deps` | Blockers, dependents, longest path | read |
| `version` | Report bops, contract, and detected bd | read |

## `bops next`

What should I work on?

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--caps` | string[]? |  | Capabilities this run offers; beads with unmet needs:<cap> are excluded. |
| `--include-unworkable` | bool | `false` | Also return excluded candidates with why_not, and the beads open gates alone keep out of bd ready (W05, blocked_by_gates; not in excluded_count). |
| `--n` | int | `3` |  |
| `--pool` | string? |  | Serve only beads labelled pool:<pool>. |

Wraps: `bd ready --json`, `bd list --status in_progress --json`, `with --include-unworkable: bd gate list --limit 0 --json, bd blocked --json, bd show <id> --json (each bead open gates alone hold back)`.

Output:

```json
{
  "candidates": [
    {
      "id": "string",
      "title": "string",
      "priority": "int",
      "type": "string",
      "workable": "bool",
      "why": "string[]",
      "why_not": "string[]",
      "blocked_by_gates": "string[]",
      "parent": "string?"
    }
  ],
  "excluded_count": "int",
  "ledger_revision": "string",
  "warnings": "string[]?"
}
```

## `bops claim`

Take this bead for this run

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--force` | bool | `false` | Take over a claim idle for stale_minutes (default 120), or an orphan. Refused on a landing bead. |
| `<id>` (positional) | string |  |  |
| `--resume` | bool | `false` | Take back a landing bead for PR rework. Only the ref's actor can. |

Preconditions: assignee is empty OR --force; status in (open, ready); no run: label; the lock remote (lock.remote, default origin) matches the Dolt remote (lock.mode ref).

Wraps: `git push refs/bops/claims/<id> (create-only)`, `bd dolt pull`, `bd update <id> --claim`, `bd label add <id> run:<ulid>`, `bd comments add <id>`, `bd dolt push`.

Output:

```json
{
  "id": "string",
  "run": "string",
  "outcome": "string",
  "claimed_at": "string?",
  "readback_ok": "bool?",
  "ledger_pending": "bool?",
  "ref_pending": "bool?",
  "holder": "object?",
  "reason": "string?",
  "age_minutes": "int?",
  "stale_minutes": "int?",
  "retry": "string?",
  "side_effects": "string?",
  "warnings": "string[]?"
}
```

## `bops note`

Record where I am, keep the claim

| Arg | Type | Default | Doc |
|---|---|---|---|
| `<id>` (positional) | string |  |  |
| `--file <path>` | stdin\|file |  | Never on the command line. |

Preconditions: this run holds the bead and its ref still matches.

Wraps: `git push refs/bops/claims/<id> (lease; a network failure is a warning)`, `bd comments add <id> -f <file>`, `bd heartbeat <id>`.

Output:

```json
{
  "id": "string",
  "outcome": "string",
  "comment_id": "string?",
  "warnings": "string[]",
  "holder": "object?",
  "reason": "string?",
  "saved_to": "string?"
}
```

## `bops release`

Give the claim back

| Arg | Type | Default | Doc |
|---|---|---|---|
| `<id>` (positional) | string |  |  |
| `--reason` | enum: done \| blocked \| scope \| abandoned \| human |  |  |
| `--file <path>` \| `--file -` | stdin\|file? |  | Optional. Read only when asked: --file <path>, or --file - for stdin. Never on the command line. Recorded below the event line of the release's bops:released comment; a lost or refused release writes none of it. |

Preconditions: this run holds the bead and its ref still matches; done: the bead has a landed record (bops land first); else refused, before the ref or the ledger changes.

Effects by reason: `abandoned` → +bops:hold; `blocked` → status→blocked; `done` → none; await land/sweep; `human` → +needs-human; `scope` → +bops:hold.

Wraps: `git fetch refs/bops/claims/<id>`, `bd show <id> --json (done: the landed record)`, `git push (lease): state=landing on done, delete otherwise`, `bd update <id> --assignee ''`, `bd label remove <id> run:*`, `bd comments add <id> <event> (no text), or bd comments add <id> -f <file> (event line, then the --file text)`, `bd dolt push`.

Output:

```json
{
  "id": "string",
  "reason": "string",
  "outcome": "string",
  "ledger_pending": "bool?",
  "ref_pending": "bool?",
  "holder": "object?",
  "detail": "string?",
  "warnings": "string[]?"
}
```

## `bops land`

Record that work reached its destination

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--forge` | enum: auto \| github \| gitlab \| none | `auto` |  |
| `<id>` (positional) | string |  |  |
| `--ref` | url |  |  |
| `--sha` | string |  |  |
| `--verify` | bool | `false` |  |

Preconditions: this run holds the bead and its ref still matches.

Never: merge, push a branch, close.

Wraps: `bd update <id> --metadata @<file> (merge bops.landed)`, `bd comments add <id>`, `bd dolt push`.

Output:

```json
{
  "id": "string",
  "outcome": "string",
  "landed": {
    "ref": "string",
    "sha": "string",
    "run": "string",
    "at": "string"
  },
  "holder": "object?",
  "reason": "string?",
  "warnings": "string[]?"
}
```

## `bops sweep`

Close landed beads whose refs are merged

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--dry-run` | bool | `true` |  |
| `--forge` | enum: auto \| github \| gitlab \| none | `auto` |  |

Never: skip a bops:landing bead with no landed record (it is listed under unknown).

Wraps: `bd dolt pull`, `bd list --status in_progress --limit 0 --json`, `forge.is_merged(ref)`, `git fetch --prune refs/bops/claims/*`, `git push (lease): delete the ref`, `bd close <id> --reason 'merged via <ref> (<sha>)'`, `bd label remove <id> bops:landing (after the close; a failure is a warning)`, `bd dolt push`.

Output:

```json
{
  "dry_run": "bool",
  "closed": "string[]",
  "would_close": "string[]",
  "pending": "string[]",
  "unknown": "string[]",
  "warnings": "string[]?"
}
```

## `bops stale`

Claims nobody is working

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--all-machines` | bool | `true` | false hides claims and orphans from other machines. |
| `--older-than` | duration | `stale_minutes` | Idle longer than this is stale; defaults to the stale_minutes config (120). Age is from the claim ref's at, which note refreshes; a claim with no ref uses updated_at, heartbeat_at and its newest bops: comment. |

Wraps: `git fetch --prune refs/bops/claims/* (the last sync's copy if offline, with a warning)`, `bd list --status in_progress --limit 0 --json`, `bd comments <id> --json`.

Output:

```json
{
  "stale": [
    {
      "id": "string",
      "run": "string",
      "actor": "string",
      "last_event_at": "string",
      "age_minutes": "int",
      "same_machine": "bool",
      "source": "ref|ledger"
    }
  ],
  "orphans": [
    {
      "id": "string",
      "run": "string",
      "actor": "string",
      "at": "string",
      "age_minutes": "int",
      "same_machine": "bool"
    }
  ],
  "warnings": "string[]"
}
```

## `bops lint`

Is this bead workable?

| Arg | Type | Default | Doc |
|---|---|---|---|
| `<id>` (positional) | string |  |  |
| `--strict` | bool | `false` |  |

Wraps: `bd show <id> --json`, `bd gate list <id> --json`, `bd dep cycles --json`.

Output:

```json
{
  "id": "string",
  "workable": "bool",
  "fails": [
    {
      "id": "string",
      "rule": "string"
    }
  ],
  "warns": [
    {
      "id": "string",
      "rule": "string"
    }
  ],
  "passes": "string[]"
}
```

## `bops sync`

Pull, heartbeat, push — with the one retry

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--no-push` | bool | `false` |  |

Retry: on dolt lock, 1 time(s), after 5000 ms.

Wraps: `git fetch --prune refs/bops/claims/*`, `bd dolt pull`, `bd heartbeat <id> (held claims)`, `bd update <id> --assignee '' --status open; bd label remove <id> run:<ulid> (this run's claim whose ref is gone and that nobody took)`, `bd dolt push`.

Output:

```json
{
  "pulled": "bool",
  "pushed": "bool",
  "revision": "string",
  "unpublished": "int",
  "stranded_holds": "string[]",
  "lost_claims": "string[]",
  "warnings": "string[]?"
}
```

## `bops session-start`

Orient a fresh agent session

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--hook-json` | bool | `false` | Print the document as Claude Code SessionStart hook output ({hookSpecificOutput:{hookEventName,additionalContext}}) and always exit 0, so the harness injects it. |

Wraps: `sync --no-push`, `stale (stale_minutes; count, all machines)`, `next (count of workable)`, `bd prime --mcp --max-memory-chars 600 (capped at 3000 bytes)`.

Output:

```json
{
  "mine": "string[]",
  "lost_claims": "string[]",
  "stranded_holds": "string[]",
  "stale": "int",
  "ready_count": "int",
  "synced": "bool",
  "prime": "string",
  "warnings": "string[]"
}
```

## `bops doctor`

Can the claim lock work against its remote (lock.remote, default origin)?

Never: write a claim ref or the ledger, leave its probe ref behind silently (a failed delete is a warning naming the command).

Wraps: `origin: git remote get-url <lock.remote>`, `fetch: git ls-remote <lock.remote> refs/bops/claims/*`, `push: git push --dry-run (create-only) refs/bops/doctor/<nonce>`, `lock: git push (create-only) refs/bops/doctor/<nonce>, then a second create-only push that must be refused`, `git push (lease): delete the probe ref`, `dolt_remote: bd dolt remote list --json (a Dolt remote on the lock remote's repository, else remote_mismatch)`, `git log -1 -- bops/ and git merge-base --is-ancestor, only in bops's own source checkout: a binary built before the last bops/ commit is a warning, not a failed check`.

Output:

```json
{
  "remote": "string",
  "probe_ref": "string",
  "checks": [
    {
      "id": "origin|fetch|push|lock|dolt_remote",
      "status": "ok|fail|skip",
      "detail": "string",
      "code": "string?",
      "next": "string?"
    }
  ],
  "failed": "string[] (any entry: exit 1, code from the first failure)",
  "warnings": "string[]"
}
```

## `bops gates`

What is blocking this bead's path?

| Arg | Type | Default | Doc |
|---|---|---|---|
| `<id>` (positional) | string |  |  |

Wraps: `bd gate list <id> --all --json (satisfied: the gate is closed)`, `bd dep list <id> --json (blocking_deps: open blocks-type deps that are not gates; parent-child is not one)`.

Output:

```json
{
  "gates": [
    {
      "id": "string",
      "type": "string",
      "on": "string",
      "satisfied": "bool"
    }
  ],
  "blocking_deps": "string[]"
}
```

## `bops deps`

Blockers, dependents, longest path

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--engine` | enum: bd \| cte \| graph | `bd` |  |
| `<id>` (positional) | string |  |  |
| `--path` | bool | `false` |  |
| `--transitive` | bool | `false` |  |

Wraps: `bd list --all --include-gates --limit 0 --json (engine=bd: one call; the graph is the beads not closed, joined by blocks, conditional-blocks and waits-for edges; parent-child is not one; a blocker the list hides counts as open)`, `depth and critical_path: the longest chain of open beads ending at <id>, by Kahn's algorithm in Go; a cycle among the beads <id> waits on exits 1 naming it`, `bd sql --json <recursive CTE> (engine=cte: needs bd in server mode and exits 1 on an embedded ledger; one query for the closure and the longest chain to each bead, and a second only to name a cycle; Go walks the chain with engine=bd's tie-break, so the answer is engine=bd's)`, `engine=graph: reserved, exits 1 as not implemented`.

Output:

```json
{
  "blocks": "string[]",
  "blocked_by": "string[]",
  "critical_path": "string[]?",
  "depth": "int"
}
```

## `bops version`

Report bops, contract, and detected bd

Wraps: `bd version --json`.

Output:

```json
{
  "bops": "string",
  "contract": "int",
  "bd": "string",
  "backend": "string",
  "revision": "string (the commit this binary was built from; empty when the go command recorded none)",
  "modified": "bool (built from a tree with uncommitted changes)",
  "revision_time": "string (that commit's time, RFC 3339; empty with no revision)"
}
```


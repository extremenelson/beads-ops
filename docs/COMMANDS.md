# bops commands (contract 1)

Generated from `bops/commands.json` by `bops gen docs`. Do not edit by hand.

Exit codes: **0** = verdict produced (may be negative); **1** = could not run: ledger unreachable, precondition unverifiable, bd error; **2** = usage.

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
| `gates` | What is blocking this bead's path? | read |
| `deps` | Blockers, dependents, longest path | read |
| `version` | Report bops, contract, and detected bd | read |

## `bops next`

What should I work on?

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--caps` | string[]? |  | Capabilities this run offers; beads with unmet needs:<cap> are excluded. |
| `--include-unworkable` | bool | `false` | Also return excluded candidates with why_not. |
| `--n` | int | `3` |  |
| `--pool` | string? |  | Serve only beads labelled pool:<pool>. |

Wraps: `bd ready --json`, `bd show --json`, `bd gate list --json`.

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
  "ledger_revision": "string"
}
```

## `bops claim`

Take this bead for this run

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--force` | bool | `false` |  |
| `--id` | string |  |  |

Preconditions: assignee is empty OR --force; status in (open, ready); no run: label.

Wraps: `bd update <id> --claim --if-assignee ''`, `bd label add <id> run:<ulid>`, `bd comments add <id>`, `bd dolt push`.

Output:

```json
{
  "id": "string",
  "run": "string",
  "claimed_at": "string",
  "readback_ok": "bool"
}
```

## `bops note`

Record where I am, keep the claim

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--id` | string |  |  |
| `--text` | stdin|file |  | Never on the command line. |

Preconditions: run: label matches current run.

Wraps: `bd comments add <id>`, `bd heartbeat <id>`.

Output:

```json
{
  "id": "string",
  "comment_id": "string",
  "warnings": "string[]"
}
```

## `bops release`

Give the claim back

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--id` | string |  |  |
| `--reason` | enum: done \| blocked \| scope \| stale \| human |  |  |
| `--text` | stdin|file? |  |  |

Effects by reason: `blocked` → status→blocked; `done` → none; await land/sweep; `human` → +needs:human; `scope` → +bops:hold; `stale` → +bops:hold.

Wraps: `bd update <id> --assignee ''`, `bd label remove <id> run:*`, `bd comments add <id>`, `bd dolt push`.

Output:

```json
{
  "id": "string",
  "reason": "string"
}
```

## `bops land`

Record that work reached its destination

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--id` | string |  |  |
| `--ref` | url |  |  |
| `--sha` | string |  |  |

Never: merge, push a branch, close.

Wraps: `bd comments add <id>`, `bd update <id> --metadata (merge bops.landed)`, `bd dolt push`.

Output:

```json
{
  "id": "string",
  "landed": {
    "ref": "string",
    "sha": "string",
    "run": "string",
    "at": "string"
  }
}
```

## `bops sweep`

Close landed beads whose refs are merged

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--dry-run` | bool | `true` |  |
| `--forge` | enum: auto \| github \| gitlab \| none | `auto` |  |

Wraps: `forge.is_merged(ref)`, `bd close <id> --reason 'merged via <ref> (<sha>)'`.

Output:

```json
{
  "closed": "string[]",
  "pending": "string[]",
  "unknown": "string[]"
}
```

## `bops stale`

Claims nobody is working

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--all-machines` | bool | `true` |  |
| `--older-than` | duration | `2h` |  |

Wraps: `bd list --status in_progress --json`, `bd comments list <id> --json`.

Output:

```json
{
  "stale": [
    {
      "id": "string",
      "run": "string",
      "last_event_at": "string",
      "age_minutes": "int",
      "same_machine": "bool"
    }
  ]
}
```

## `bops lint`

Is this bead workable?

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--id` | string |  |  |
| `--strict` | bool | `false` |  |

Wraps: `bd show <id> --json`, `bd gate list --json`, `bd dep cycles --json`.

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

Wraps: `bd dolt pull`, `bd heartbeat <id> (held claims)`, `bd dolt push`.

Output:

```json
{
  "pulled": "bool",
  "pushed": "bool",
  "revision": "string",
  "unpublished": "int"
}
```

## `bops session-start`

Orient a fresh agent session

Wraps: `sync`, `bd prime`, `stale --older-than 2h`, `bd list --assignee <me> --status in_progress --json`.

Output:

```json
{
  "mine": "string[]",
  "stale": "int",
  "ready_count": "int",
  "prime": "string"
}
```

## `bops gates`

What is blocking this bead's path?

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--id` | string |  |  |

Wraps: `bd gate list --json`, `bd dep list <id> --json`.

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
| `--id` | string |  |  |
| `--path` | bool | `false` |  |
| `--transitive` | bool | `false` |  |

Wraps: `bd dep list <id> --json`, `bd sql (engine=cte)`.

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
  "backend": "string"
}
```


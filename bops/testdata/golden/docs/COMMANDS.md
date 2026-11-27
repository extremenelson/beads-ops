# bops commands (contract 1)

Generated from `bops/commands.json` by `bops gen docs`. Do not edit by hand.

Exit codes: **0** = done, or a read's verdict; **1** = could not run: remote or ledger unreachable, precondition unverifiable, bd error; **2** = usage; **3** = negative verdict on a write: lost or refused, ok:false.

| Command | Intent | Kind |
|---|---|---|
| `next` | What should I work on? | read |
| `claim` | Take this bead for this run | write |
| `note` | Record where I am, keep the claim | write |
| `renew` | Heartbeat my claims, no comment | write |
| `release` | Give the claim back | write |
| `land` | Record that work reached its destination | write |
| `sweep` | Close landed beads whose refs are merged | write |
| `stale` | Claims nobody is working | read |
| `status` | Every claim on every machine | read |
| `reap` | Release stale claims that hold no work | write |
| `gc` | Delete refs of closed beads and orphans | write |
| `lint` | Is this bead workable? | read |
| `sync` | Pull, heartbeat, push — with the one retry | write |
| `session-start` | Orient a fresh agent session | read+sync |
| `guard` | Refuse a raw bd claim before it runs | read |
| `doctor` | Can the claim lock work against its remote (lock.remote, default origin)? | read |
| `gates` | What is blocking this bead's path? | read |
| `deps` | Blockers, dependents, longest path | read |
| `version` | Report bops, contract, and detected bd | read |

## `bops next`

What should I work on?

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--avoid-conflicts` | bool | `false` | Also exclude candidates whose touches:<path> labels overlap a live claim's (advisory; a bead with none never conflicts). |
| `--caps` | string[]? |  | Capabilities this run offers; beads with unmet needs:<cap> are excluded. |
| `--include-unworkable` | bool | `false` | Also return excluded candidates with why_not, and the beads open gates alone keep out of bd ready (W05, blocked_by_gates; not in excluded_count). |
| `--n` | int | `3` |  |
| `--pool` | string? |  | Serve only beads labelled pool:<pool>. |

Wraps: `bd ready --json`, `bd list --status in_progress --json`, `git fetch refs/bops/claims/* (one round trip, so W14 sees claims the ledger has not caught up with; on failure a warning and the last sync's copy)`, `with --include-unworkable: bd gate list --limit 0 --json, bd blocked --json, bd show <id> --json (each bead open gates alone hold back)`.

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
      "parent": "string?",
      "touches": "string[]?"
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
| `--force` | bool | `false` | Take over a claim idle for stale_minutes (default 120), or an orphan. Refused on a landing bead. On success, previous names the last holder, its last note's NEXT/BLOCKER paragraph (note_run), branch and MR, all inferred ledger text. |
| `<id>` (positional) | string? |  | The bead to claim. Required unless --next. |
| `--next` | bool | `false` | No id: claim the first workable bead in next's order, trying the next on lost, refused, ref_contention or a bead bd cannot show (at most 5 tries, while 30s of budget is left; skipped lists them). Refused, naming them, when the run already holds a bead. |
| `--resume` | bool | `false` | Take back a landing bead for PR rework, or your own held bead from another machine. Only the ref's actor can. |

Preconditions: an id, or --next and no id (--next takes neither --force nor --resume, and is refused while the run holds any bead); assignee is empty OR --force; status in (open, ready); no run: label; no label listed in blocking_labels (config; --force skips it, and a bead this run already holds answers held); the lock remote (lock.remote, default origin) matches the Dolt remote (lock.mode ref); no other bops process on this machine is working on the bead (waits up to 10 seconds, then ref_contention); --resume: the claim ref's actor is this run's, and its state is landing, or held with a machine= that is not this machine (a held ref with no machine= is refused).

Wraps: `with --next: next's reads, then the steps below per candidate (bead wait 2 seconds)`, `git push refs/bops/claims/<id> (create-only)`, `bd dolt pull`, `bd update <id> --claim`, `bd label add <id> run:<ulid>`, `bd comments add <id>`, `bd dolt push`.

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
  "next_candidate": "string?",
  "skipped": "object[]?",
  "previous": "object?",
  "warnings": "string[]?"
}
```

## `bops note`

Record where I am, keep the claim

| Arg | Type | Default | Doc |
|---|---|---|---|
| `<id>` (positional) | string |  |  |
| `--op-id` | string? |  | Names this write (op=<id> on its event comment). Running the same op again (same run) writes nothing and answers with replayed:true, so a write that hit a busy bead, a rejected push, a timeout or a lost answer is simply re-run. Default: derived from command, bead, run and content. |
| `--file <path>` | stdin\|file |  | Never on the command line. |

Preconditions: this run holds the bead and its ref still matches; no other bops process on this machine is working on the bead (waits up to 10 seconds, then ref_contention).

Wraps: `bd comments <id> --json (op id check)`, `git push refs/bops/claims/<id> (lease; a network failure is a warning)`, `bd comments add <id> -f <file>`, `bd heartbeat <id>`.

Output:

```json
{
  "id": "string",
  "outcome": "string",
  "comment_id": "string?",
  "warnings": "string[]",
  "holder": "object?",
  "reason": "string?",
  "saved_to": "string?",
  "op_id": "string?",
  "replayed": "bool?"
}
```

## `bops renew`

Heartbeat my claims, no comment

| Arg | Type | Default | Doc |
|---|---|---|---|
| `<id>` (positional) | string |  | The bead to renew; the command line takes several, space-separated, each with its own outcome. Run as the run that claimed (the same checkout and BOPS_RUN). On any non-zero exit, read beads[].outcome: a run with a bead not refreshed exits 1 even when another is lost. |

Preconditions: the claim lock is on (lock.mode is not none); this run holds the bead and its ref still matches (an invalid id is refused, code bad_id); the hold has a claim ref: one taken before the claim lock is refused, code legacy_hold; no other bops process on this machine is working on the bead (waits up to 10 seconds, then ref_contention).

Never: write the ledger (no comment, no bd heartbeat).

Wraps: `git push refs/bops/claims/<id> (lease), per bead; after an unreachable or git_auth failure the rest are not tried`, `git push (lease): delete the ref of a release whose delete never finished, as note does`.

Output:

```json
{
  "beads": [
    {
      "id": "string",
      "outcome": "renewed|lost|refused|unreachable|error",
      "reason": "string?",
      "code": "string?",
      "holder": "object?"
    }
  ],
  "warnings": "string[]"
}
```

## `bops release`

Give the claim back

| Arg | Type | Default | Doc |
|---|---|---|---|
| `<id>` (positional) | string |  |  |
| `--op-id` | string? |  | As for note. Replays only once this run no longer holds the bead, with the same reason, and pushes the ledger again; a release still held runs in full. |
| `--reason` | enum: done \| blocked \| scope \| abandoned \| human |  |  |
| `--file <path>` \| `--file -` | stdin\|file? |  | Optional. Read only when asked: --file <path>, or --file - for stdin. Never on the command line. Recorded below the event line of the release's bops:released comment; a lost or refused release writes none of it. |

Preconditions: this run holds the bead and its ref still matches; done: the bead has a landed record (bops land first); else refused, before the ref or the ledger changes; no other bops process on this machine is working on the bead (waits up to 10 seconds, then ref_contention).

Effects by reason: `abandoned` → +bops:hold; `blocked` → status→blocked; `done` → none; await land/sweep; `human` → +needs-human; `scope` → +bops:hold.

Wraps: `bd comments <id> --json (op id check, when not held)`, `git fetch refs/bops/claims/<id>`, `bd show <id> --json (done: the landed record)`, `git push (lease): state=landing on done, delete otherwise`, `bd update <id> --assignee ''`, `bd label remove <id> run:*`, `bd comments add <id> <event> (no text), or bd comments add <id> -f <file> (event line, then the --file text)`, `bd dolt push`.

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
  "saved_to": "string?",
  "warnings": "string[]?",
  "op_id": "string?",
  "replayed": "bool?"
}
```

## `bops land`

Record that work reached its destination

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--forge` | enum: auto \| github \| gitlab \| none | `auto` |  |
| `<id>` (positional) | string |  |  |
| `--op-id` | string? |  | As for note. A repeat of the same ref and sha adds no record; the op id also answers one retried after the release. A replay pushes the ledger again. |
| `--ref` | url |  |  |
| `--sha` | string |  |  |
| `--verify` | bool | `false` |  |

Preconditions: this run holds the bead and its ref still matches; no other bops process on this machine is working on the bead (waits up to 10 seconds, then ref_contention).

Never: merge, push a branch, close.

Wraps: `bd comments <id> --json (op id check, when not held or a repeat)`, `bd update <id> --metadata @<file> (merge bops.landed)`, `bd comments add <id>`, `bd dolt push`.

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
  "warnings": "string[]?",
  "op_id": "string?",
  "replayed": "bool?"
}
```

## `bops sweep`

Close landed beads whose refs are merged

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--dry-run` | bool | `true` |  |
| `--forge` | enum: auto \| github \| gitlab \| none | `auto` |  |

Never: skip a bops:landing bead with no landed record (it is listed under unknown), wait for a bead another bops process on this machine is working on (it is listed under unknown), close a bead that changed since sweep read it: re-held, resumed or given back (it is listed under pending).

Wraps: `bd dolt pull`, `bd list --status in_progress --limit 0 --json`, `forge.is_merged(ref)`, `git fetch --prune refs/bops/claims/*`, `bd show <id> --json (under the bead lock)`, `git push (lease): delete the ref`, `bd close <id> --reason 'merged via <ref> (<sha>)'`, `bd label remove <id> bops:landing (after the close; a failure is a warning)`, `bd dolt push`.

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

## `bops status`

Every claim on every machine

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--mine` | bool | `false` | Only claims made on this machine (same_machine: a run of this clone, or a ref or bops:claimed comment naming this machine). |
| `--older-than` | duration | `stale_minutes` | Idle longer than this is stale, as for stale. |
| `--stale` | bool | `false` | Only stale, orphan and unknown claims (unknown: staleness could not be judged, so it may be stale). A held ref that is stale and does not match its ledger claim is one orphan row, where stale lists it under both. |

Never: write the ledger or a ref, call a claim stale when stale would not: a ref whose state or at is unreadable or whose at is in the future, or a no-ref claim whose comments could not be read, is unknown, ask the forge: branch and mr are inferred from branch names and the ledger, as claim --force's previous infers them.

Wraps: `git fetch --prune refs/bops/claims/* (the last sync's copy if offline, with a warning)`, `bd list --status in_progress --limit 0 --json`, `bd comments <id> --json`, `git ls-remote --heads <lock remote> (and origin)`, `git for-each-ref refs/heads refs/remotes`.

Output:

```json
{
  "claims": [
    {
      "id": "string",
      "title": "string?",
      "state": "live|stale|landing|orphan|unknown",
      "actor": "string",
      "machine": "string?",
      "run": "string",
      "age_minutes": "int?",
      "idle_minutes": "int?",
      "last_event_at": "string?",
      "last_note_minutes": "int?",
      "branch": "string?",
      "mr": "string?",
      "same_machine": "bool",
      "source": "ref|ledger|ref+ledger",
      "touches": "string[]?"
    }
  ],
  "warnings": "string[]"
}
```

## `bops reap`

Release stale claims that hold no work

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--dry-run` | bool | `true` |  |
| `--older-than` | duration | `stale_minutes` | Idle longer than this is stale, as for stale; shorter than stale_minutes is refused. |
| `--only` | string? |  | Comma-separated bead ids, normally the ones a preview listed under would_release: act on these only. One that is no longer a stale claim is listed under skipped; an empty list is a usage error. A claim made outside bops (no run label, no ref) is released only when named here. |

Preconditions: the claim lock is on (lock.mode is not none); a Dolt remote beside the lock remote (not with --dry-run); --older-than, when given, is at least stale_minutes.

Never: release a claim labelled needs-land, with a merge request URL on the bead, held by a live worktree of this clone, or whose bead id is carried by a branch name (lock remote, origin, local, or a worktree's branch or directory); it is listed under kept with what was found, release anything when the ledger pull or the first branch listing fails; a later listing failure stops the run, listing the rest under skipped, release a claim made outside bops (no run label, no ref) unless --only names it, release a claim whose evidence appears after its takeover (this run keeps it, listed under failed with outcome held), start a takeover with less than 180 s of the command's time left (90 s for it, 30 s for the evidence re-read, 60 s for its release, each on its own clock; the rest are listed under skipped), release a claim refreshed since stale read it (claim --force re-checks under a lease), touch an orphan ref (a ref with no claim on the ledger).

Wraps: `bd dolt pull (not with --dry-run)`, `stale --all-machines`, `git remote (when the lock remote is not origin)`, `git ls-remote --heads <lock remote> (and origin, when it exists)`, `git for-each-ref refs/heads refs/remotes (local branches, and other remotes as of their last fetch)`, `git worktree list --porcelain`, `bd show <id> --json`, `bd comments <id> --json`, `claim <id> --force`, `release <id> --reason abandoned (with a note)`.

Output:

```json
{
  "dry_run": "bool",
  "released": [
    {
      "id": "string",
      "run": "string",
      "actor": "string",
      "age_minutes": "int",
      "same_machine": "bool",
      "outcome": "string?",
      "detail": "string?",
      "saved_to": "string?"
    }
  ],
  "would_release": [
    {
      "id": "string",
      "run": "string",
      "actor": "string",
      "age_minutes": "int",
      "same_machine": "bool",
      "outcome": "string?",
      "detail": "string?",
      "saved_to": "string?"
    }
  ],
  "failed": [
    {
      "id": "string",
      "run": "string",
      "actor": "string",
      "age_minutes": "int",
      "same_machine": "bool",
      "outcome": "string?",
      "detail": "string?",
      "saved_to": "string?"
    }
  ],
  "skipped": [
    {
      "id": "string",
      "run": "string",
      "actor": "string",
      "age_minutes": "int",
      "same_machine": "bool",
      "outcome": "string?",
      "detail": "string?",
      "saved_to": "string?"
    }
  ],
  "kept": [
    {
      "id": "string",
      "run": "string",
      "actor": "string",
      "age_minutes": "int",
      "same_machine": "bool",
      "evidence": "string[]"
    }
  ],
  "warnings": "string[]"
}
```

## `bops gc`

Delete refs of closed beads and orphans

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--dry-run` | bool | `true` |  |
| `--only` | string? |  | Comma-separated bead ids, normally the ones a preview listed under would_delete: act on these only. One whose ref is no longer one gc deletes is listed under skipped; one with no ref at all is a warning; an empty list is a usage error. |

Preconditions: the claim lock is on (lock.mode is not none); a Dolt remote on the lock remote's repository (not with --dry-run).

Never: delete a ref whose bead is not closed and carries a claim by the ref's run or actor (in progress, or parked blocked or deferred; a parked one is listed under kept), or a landing ref of a bead in progress (sweep's), delete an orphan younger than stale_minutes, or a closed bead's ref younger than stale_minutes written later than 15 minutes before the bead's last ledger change (a claim whose ledger write may be pending, allowing for clock skew); the latter is listed under kept, delete anything when the ledger pull or the ref fetch fails, delete a ref a hold of this clone names, or whose bead changed since gc read it (re-read from the ledger as pulled, under the bead lock; listed under kept), delete a ref re-taken since gc read it (the delete leases the sha it read; listed under failed), wait for a bead another bops process on this machine is working on (listed under failed), delete or keep a ref whose bead could not be read, or that the command's time ran out before judging (listed under failed), write the ledger.

Wraps: `bd dolt pull (not with --dry-run)`, `git fetch --prune refs/bops/claims/* (not with --dry-run; stale's copy otherwise)`, `stale --all-machines`, `bd show <id> --json (each ref's bead; again under the bead lock)`, `git push (lease): delete the ref`.

Output:

```json
{
  "dry_run": "bool",
  "deleted": [
    {
      "id": "string",
      "run": "string",
      "actor": "string",
      "state": "held|landing",
      "why": "closed|orphan?",
      "age_minutes": "int",
      "same_machine": "bool",
      "detail": "string?"
    }
  ],
  "would_delete": [
    {
      "id": "string",
      "run": "string",
      "actor": "string",
      "state": "held|landing",
      "why": "closed|orphan?",
      "age_minutes": "int",
      "same_machine": "bool",
      "detail": "string?"
    }
  ],
  "kept": [
    {
      "id": "string",
      "run": "string",
      "actor": "string",
      "state": "held|landing",
      "why": "closed|orphan?",
      "age_minutes": "int",
      "same_machine": "bool",
      "detail": "string?"
    }
  ],
  "failed": [
    {
      "id": "string",
      "run": "string",
      "actor": "string",
      "state": "held|landing",
      "why": "closed|orphan?",
      "age_minutes": "int",
      "same_machine": "bool",
      "detail": "string?"
    }
  ],
  "skipped": [
    {
      "id": "string",
      "run": "string",
      "actor": "string",
      "state": "held|landing",
      "why": "closed|orphan?",
      "age_minutes": "int",
      "same_machine": "bool",
      "detail": "string?"
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

Wraps: `git fetch --prune refs/bops/claims/*`, `bd dolt pull`, `bd update <id> --assignee <actor> --status in_progress --force, or --assignee alone on a blocked or deferred bead (this run's live claims whose display differs)`, `bd heartbeat <id> (this run's live claims only; another run's are settled against their refs but not re-displayed or heartbeat)`, `bd update <id> --assignee '' --status open; bd label remove <id> run:<ulid> (this run's claim whose ref is gone and that nobody took)`, `bd dolt push`.

Output:

```json
{
  "pulled": "bool",
  "pushed": "bool",
  "revision": "string",
  "unpublished": "int",
  "stranded_holds": "string[]",
  "lost_claims": "string[]",
  "deferred": "string[]",
  "warnings": "string[]?"
}
```

## `bops session-start`

Orient a fresh agent session

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--hook-json` | bool | `false` | Print the document as Claude Code SessionStart hook output ({hookSpecificOutput:{hookEventName,additionalContext}}) and always exit 0, so the harness injects it. |

Wraps: `sync --no-push, for this run's holds only (another run's stranded hold is listed, nothing else of another run's is touched); its bd dolt pull ends 25 s before the 60 s budget, so the rest still reports`, `stale (stale_minutes; count, all machines)`, `next (count of workable)`, `bd prime --mcp --max-memory-chars 600 (capped at 3000 bytes)`.

Output:

```json
{
  "mine": "string[]",
  "lost_claims": "string[]",
  "stranded_holds": "string[]",
  "deferred": "string[]",
  "stale": "int",
  "ready_count": "int",
  "synced": "bool",
  "prime": "string",
  "warnings": "string[]"
}
```

## `bops guard`

Refuse a raw bd claim before it runs

| Arg | Type | Default | Doc |
|---|---|---|---|
| `--file <path>` | stdin\|file |  | The shell command to check. Without --hook-json only. |
| `--hook-json` | bool | `false` | Read Claude Code's PreToolUse payload ({tool_name, tool_input:{command}, cwd}) on stdin instead, and always exit 0: a refusal prints {hookSpecificOutput:{hookEventName:PreToolUse, permissionDecision:deny, permissionDecisionReason}}, anything else prints nothing, so the harness's own permission rules still apply. |

Preconditions: refuses only in a worktree with a .beads directory, or whose main checkout has one (the worktree holding the payload cwd, else the current directory), and whose claim lock is on (lock.mode is not none); every other case allows.

Never: run bd or touch the network: its one subprocess is git rev-parse --show-toplevel --git-common-dir, to find the worktree and its main checkout, cut off after 2 s (then the directory itself is used), refuse when it cannot decide (unreadable input or config allows the command, with a warning: it fails open), print an allow decision, which would skip the harness's permission prompt.

Wraps: `finds bd update ... --claim, bd ready ... --claim and bd reclaim in each simple command (after ;, &&, ||, |, subshells, command substitution, also inside double quotes, leading assignments and wrappers such as env and timeout, sh -c and eval), skipping quoted text, comments, heredoc bodies, shifts inside (( )) and --help calls; variables, aliases, sourced scripts and a script on a shell's stdin are not read`.

Output:

```json
{
  "refused": "bool",
  "matched": "string (the raw claim found: bd update --claim, bd ready --claim or bd reclaim; empty when none)",
  "use": "string (the bops command to run instead; empty unless refused)",
  "reason": "string (the refusal the agent sees; empty unless refused)",
  "warnings": "string[]"
}
```

## `bops doctor`

Can the claim lock work against its remote (lock.remote, default origin)?

Never: write a claim ref or the ledger, leave its probe ref behind silently (a failed delete is a warning naming the command).

Wraps: `origin: git remote get-url <lock.remote>`, `fetch: git ls-remote <lock.remote> refs/bops/claims/*`, `push: git push --dry-run (create-only) refs/bops/doctor/<nonce>`, `lock: git push (create-only) refs/bops/doctor/<nonce>, then a second create-only push that must be refused`, `git push (lease): delete the probe ref`, `dolt_remote: bd dolt remote list --json (a Dolt remote on the lock remote's repository, else remote_mismatch)`, `blocking_labels: bd list --all --include-gates --limit 0 --json, once, when blocking_labels are set: a label no bead carries, matched exactly, is a warning, and a list bd cannot give is a not-checked warning, neither a failed check`, `git log -1 -- bops/ and git merge-base --is-ancestor, only in bops's own source checkout: a binary built before the last bops/ commit is a warning, not a failed check`, `bops_version: only when .bops.json sets min_bops: this bops older than it fails (bops_too_old), and a build with no version is a warning`.

Output:

```json
{
  "remote": "string",
  "probe_ref": "string",
  "checks": [
    {
      "id": "origin|fetch|push|lock|dolt_remote|bops_version",
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


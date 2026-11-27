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
| **renew** | refreshes the claim ref's time and nothing else: no ledger write; an orchestrator's heartbeat | note |
| **release** | returning a claim with a classified reason; the claim ref is deleted and the bead goes back to the pool, except `done`, which keeps the ref as `landing`. `done` needs a landed record (`land` first): without one it is refused (exit 3) before the ref or the ledger changes, in both lock modes, and the hold is kept | close |
| **land** | recording evidence that work reached its destination (PR/MR URL + sha); **never** performs the merge | close |
| **landing** | a bead released `done` whose PR is not yet merged; its ref has `state=landing` and only `sweep` or `claim --resume` acts on it | held |
| **close** | terminal status; only `bops sweep` or a human closes, never a worker mid-run | land |
| **gate** | a beads gate (`bd gate`: human, timer, CI, PR) that blocks progress | dep |
| **pool** | an opt-in partition of ready work via `pool:<name>` label; a run serves one pool | machine label |
| **machine label** | a capability constraint (`needs:<cap>`) matched against the run's declared capabilities | pool |

## 3. State conventions on the ledger

All keys are namespaced so that `bops` can coexist with any other tooling on the same ledger.

### 3.0 The claim lock (ownership)

One git ref per claimed bead, `refs/bops/claims/<id>`, on the lock remote: the git remote named by `lock.remote` (§10, default `origin`), which must be the same repository as the Dolt ledger. `claim` refuses with `no_origin` when no git remote has that name, and with `remote_mismatch` when its URL is not the Dolt remote, or when there is no Dolt remote at all (`bd dolt remote list --json` lists none), before it pushes a ref or writes the ledger; the fix is a Dolt remote on the lock remote's repository, or `lock.mode: none` for a single-machine ledger. `claim --force` and `claim --resume` make the same check. A remote list that cannot be read or parsed is not compared, and the claim goes on. `doctor` checks the same remote: its `dolt_remote` check, after the lock checks, fails with `remote_mismatch` and the same fix in `next` when there is no Dolt remote or none is on the lock remote's repository, and is `skip` when the list cannot be read or the lock remote is missing. A hold that already exists when the Dolt remote is missing (claimed before this check, or the remote removed later) is reported by its cause rather than by bd's pull error: `sync` (and so `session-start`) and `sweep` fail with `remote_mismatch` and the fix before they pull; `land` and `release` finish (release still deletes the ref), skip the Dolt push, and warn `no Dolt remote: ledger not published: …` with the fix. A list that cannot be read keeps the old behaviour. The ref's value is a commit on the empty tree, authored `bops <bops@invalid>`, whose message is one `key=value` per line:

```
run=<ulid>
actor=<actor>
machine=<name>
state=held|landing
nonce=<random>
at=<iso8601>
```

- Every write is `git push --no-verify --force-with-lease=<ref>:<expected>`: create-only for a claim, leased on the sha last read for every other write. `-f` is never used. Of two writers, exactly one wins; the other gets `lost`.
- **The ref decides; the ledger displays.** The assignee, `run:` label and `bops:landing` label are written best effort after the ref, can lag it (`ledger_pending`), and are repaired by `sync`. No ownership decision reads them.
- `actor` is unauthenticated free text (`BEADS_ACTOR`, else git `user.name`, else the OS user), and anyone with push access can write these refs. The actor check on `claim --resume` is a safety check, not authentication.
- `machine` is the holder run's machine (`BOPS_MACHINE`, else the hostname, fixed when the run is created). A bops before bops-xwr writes no `machine` line; an older bops reading a ref that has one ignores it as an unknown key.
- **`claim --resume`** replaces a ref, under a lease on the sha it read, with this run and `state=held`, and only for the ref's own actor. It takes a `landing` bead from any machine, for PR rework. It takes a `held` bead only when the ref names a machine and it is not this one: that is how a person moves their own work between machines (bops-xwr). It refuses a `held` bead held on this machine (worktrees of one machine share an actor and must not take beads from each other; a removed worktree's hold is a stranded hold), a `held` ref with no `machine` line, and a ref whose state is unreadable. The displaced run finds out as from a takeover: its next `sync` lists the bead in `lost_claims`, and its next `note`, `land` or `release` is `lost`. Two machines resuming at once each win at most one lease round; one that reads the ref after the other won takes it in turn, so both may answer `claimed`, the ref names one run, and the other's next `sync` reports it lost. A person decides a resume; an autonomous loop never resumes a held bead.
- A ref message is untrusted input: unknown keys are ignored, size is bounded, and an unreadable or future `at` never counts as stale.
- Staleness is `stale_minutes` (§10, default 120) from the ref's `at`. `note` and `renew` refresh `at` (`renew` without writing the ledger: the heartbeat for an orchestrator). Set it in the committed `.bops.json` so every machine on a ledger measures the same window: a machine with a shorter one takes over claims sooner. So only `.bops.json` may shorten it: a user file or `BOPS_STALE_MINUTES` below the repo file's value (or the default, when it sets none) fails with `bad_config`; either may lengthen it (bops-i08).
- **The guarantee covers claims made through `bops`.** A raw `bd update --claim` writes no ref and bypasses the lock. The Kestrel binding claims through `bops claim`. `/bead-loop` claims through `bops claim` when the repo's protocol file sets `**Claim tool:** bops`; with no such line it claims through raw `bd` and is outside the guarantee, so set that line on every `bops` ledger (bops-t8j). Every machine on a ledger must run a `bops` that knows the lock before the first claim on it.

**`lock.mode: none` (one machine only).** A ledger used on a single machine, with or without a git remote, can turn the lock off in the committed `.bops.json` (`{"lock": {"mode": "none"}}`, §10). No command then reads or writes a claim ref, contacts a remote for locking, or refuses with `no_origin` or `remote_mismatch`; ownership is the ledger alone, as for a claim with no ref:

- `claim` pulls best effort, checks the same preconditions, and claims with `bd update --claim`, whose compare-and-set holds on one replica, so it separates the worktrees of one clone and nothing more. If that write fails and the bead now has an assignee, the result is `lost` with that holder. The hold is journaled with no sha. The `run:` label, `bops:claimed` comment and Dolt push follow best effort, and their failures are warnings; nothing sets `ledger_pending`.
- `claim --force` measures staleness from the ledger claim's `updated_at` (as with no ref) and takes over with `bd update --assignee --status in_progress --force`. Two concurrent takeovers are not separated. `claim --resume` is refused: its rule is the ref's state and actor, which do not exist here. To rework a landing bead, remove `bops:landing`, set it `open`, and claim it.
- `release` and `land` check the ledger instead of the ref: a `run:` label of another run (or, with no `run:` label, another assignee) is `lost` with no write. `release` never deletes a ref.
- `sweep` takes the no-ref path for every bead: `bops:landing` and a merged PR close it. `stale` ages every claim from the ledger and reports no orphans. `next` skips W14's ref check. `sync` and `session-start` report no `lost_claims`; a stranded hold is still reported.
- With no Dolt remote (`bd dolt remote list --json` lists none), the ledger has nowhere to publish, so `claim` (including `claim --force`), `release`, `land`, `sweep`, `sync` and `session-start` skip their Dolt pull and push and add the warning `no Dolt remote: ledger not published` instead of failing; `sync` then reports `pulled` and `pushed` false. A listing that fails counts as a remote. With a remote configured, a failed pull or push fails as under `ref`. Under `ref` a missing Dolt remote is reported as `remote_mismatch` instead (above), because the ledger must be shared with the lock.
- Every one of these results carries the warning `lock.mode is none: …`, so an unprotected claim is not mistaken for a protected one. `doctor` reports all five checks (`dolt_remote` included) `skip`, naming `lock.mode`, and exits 0.
- Only `.bops.json` can set `none`: a per-user file or `BOPS_LOCK_MODE` that sets it is an error unless the repo file already does. One machine of a shared ledger turning the lock off would claim beads others hold without anyone being told. Either layer may set `ref`.
- Switch modes with nothing claimed. Holds journaled under `ref` are released through the ledger under `none` and their refs stay on the remote; holds taken under `none` have no ref, and under `ref` are treated like claims made before the lock.

### 3.1 Labels (machine-readable state)

| Label | Set by | Meaning |
|---|---|---|
| `run:<ulid>` | `claim` | display of the run holding the bead; removed by `release`/`sweep`. The claim ref is authoritative |
| `bops:landing` | `release --reason done` | display of a landing ref; removed by `claim --resume` and `sweep`. `sweep` lists a `bops:landing` bead with no landed record under `unknown` (no PR to check) rather than skipping it |
| `pool:<name>` | humans / `bd` | partition; absence = default pool |
| `needs:<cap>` | humans | required machine capability (`gpu`, `macos`, `net:internal`, …) |
| `needs-human` | humans / `release --reason human` | excluded from `next` until removed. Hyphen, not colon: it is a policy label shared with PROCESS.md and `scripts/bd-lint-claim.sh`, not a `needs:<cap>` capability. bops writes only `needs-human`. **Transition:** bops before bops-cgw wrote `needs:human`; every read (W03, so `next`, `claim` and `lint`) treats `needs:human` as `needs-human` until it is dropped in v0.2.0, and `needs:<cap>` never matches `human`. Relabel old beads with the command in PROCESS.md §3.5 |
| `bops:hold` | `release --reason abandoned|scope` | cooling-off; excluded from `next` for `hold_minutes` (default 120). A label has no timestamp, so the window runs from the bead's `updated_at`: a later write extends it, and a bead with no `updated_at` stays held |

### 3.2 Comments (append-only events)

Each `bops` write adds exactly one comment whose first line is a typed event, so any consumer can reconstruct history with `bd comments <id> --json` (there is no `bd comments list`):

```
bops:claimed run=<ulid> actor=<actor> machine=<name> [takeover=<old-run> | resumed=<old-run>]
bops:note run=<ulid>
<free text, ≥1 line; last paragraph SHOULD start with NEXT: / BLOCKER: / KEY DECISION: / REF:>
bops:released run=<ulid> reason=<done|blocked|scope|abandoned|human>
[<optional free text from --file <path> or --file - (stdin); written only with a successful release>]
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

- `state.json` (version 5; version 3 added no field and marked a clone whose bops processes take the bead lock below, and version 4 adds none either and marks one whose bead lock files may be pruned, which is safe only for a bops that re-checks its lock file after locking (bops-exb), and version 5 adds named runs, below (bops-4ez); an older bops refuses the file and asks to be upgraded, so bops 0.4.0 to 0.4.3 refuses a version 4 clone and a bops before named runs refuses a version 5 one, which it would otherwise read as holding a removed worktree's stranded holds. A newer bops stamps its version the first time it opens an older file, read-only commands and the MCP server's read tools included, so the refusal starts with its first command, not its first write (bops-j9k); a read-only command that cannot take the state lock at once (it does not wait for it) or write the file still reads it and leaves the stamp to a later command. An older bops left on a machine, such as an earlier one on `PATH`, then refuses every command in that clone, `release` included, until it is upgraded; the file needs no repair. One already partway through a command when the stamp lands, between journalling a ref write and settling it, fails at the settle with its push done and its entry left `pushing`; the next `sync` by the newer bops settles it against the ref, so after upgrading, run `bops sync` (bops-mx0)): one run per worktree (id, actor, machine, pool, capabilities, started_at), keyed by the worktree root, plus a named run for each `BOPS_RUN` a command in it has set, keyed by the root and `#run=<name>` and carrying `name`; each run has a hold journal keyed by bead: `{sha, intended, phase: pushing|held, ledger_pending, delete_pending}`. Every claim-ref write is journalled before it is pushed and settled after, so a crash or a lost acknowledgement is resolved by `sync`. A file with a newer version is refused. A legacy `held` list still decodes; its holds have no sha and are never reported lost.
- `lock`: the write lock, held only around file writes and never across a network call. It carries an owner token, and a lock older than its stale limit is taken over.
- `beads/<digest>.lock`: one per bead, held for a whole command on that bead, network calls included (bops-yph). Every worktree of a clone shares this directory, and a session opened in a checkout shares that checkout's run (one in the main checkout shares the `/bead-loop` orchestrator's), so two bops processes could otherwise write one bead's claim at once. `claim` (and `--force`, `--resume`), `note`, `renew`, `land`, `release` and `sweep` take it and then re-read `state.json`, so what they read of the bead is current until they let go; a command waits up to 10 seconds for a bead another process holds, then answers `ref_contention` (`note` and `release` keep their text at `saved_to`). `sweep` never waits: it lists the bead under `unknown`. `sweep` read the refs and the ledger before taking the lock, so under it a bead any run of the clone now holds at another ref, or is writing, or that is no longer in progress with the same last landing, is `pending`. A hold whose release never finished deleting its ref is a release in progress, not a claim: `claim`, `note`, `renew`, `land` and `release` finish that delete first, as `sync` would, so `claim` then claims afresh and the others find the bead no longer held. `sync` heartbeats a hold only while it holds the bead's lock. `sync` and `session-start` take it per hold and skip a held bead, and also skip a hold whose entry changed since they read the holds, which they do before fetching the refs, so a ref is never older than the entry it is checked against; both list such beads under `deferred`. A hold with a write in flight is settled against the ref read under the lock, since its writer may have pushed and died after the fetch; if that ref is a landing (a `release --reason done` that died before dropping its hold), the hold is dropped and `sweep` closes the landing. It is an operating-system lock (`flock`, or `LockFileEx` on Windows), tried without blocking and polled every 200 ms while a command waits, and released when its process exits however it exits; each file is named by the first 16 bytes of the id's SHA-256 in hex. `sync` and `session-start` remove the files of beads no run in the clone holds, each only while holding its lock, taken without waiting (a busy file is left for the next), and `LockBead` re-checks after locking that the path still names the file it locked, starting again if a prune replaced it (bops-exb). That keeps two processes from holding one bead's lock on two files only if every bops in the clone re-checks, so a prune runs only once `state.json` on disk is at this bops's version (4 or later), which bops 0.4.0 to 0.4.3 refuses; a read-only command's stamp can be skipped, so the prune reads the file itself. A command that meets a bead's file while a prune holds it, for the moment between its lock and its unlink, sees the bead busy: `sync`, `session-start` and `sweep`, which never wait, defer it to their next run, and the others wait as for any holder. On Windows the files are left in place: a file deleted while another handle has it open lingers as delete-pending, and on older Windows its name cannot be reopened until every handle closes; they are empty. Where the state directory's file system has no file locks, commands run unserialized, as before, with a warning. `sync` takes the lock of every hold in the clone, sibling worktrees' included, and settles each against its ref: it finishes an interrupted write or delete, reports a lost claim or a stranded hold, whichever run's it is. It repairs the ledger display and runs `bd heartbeat` only for the calling run's live holds, since a heartbeat from another worktree would hide that run dying; a run repairs its own display with its own `sync` (bops-yr2). `session-start`, which runs on every session start in any checkout, settles only the calling run's holds and writes nothing for any other run's, only listing a stranded one in `stranded_holds`. Every session in one checkout shares that checkout's run unless it sets `BOPS_RUN`, which names a run of its own in that checkout (up to 64 letters, digits, `.`, `_` or `-`; anything else is refused). The `/bead-loop` orchestrator, which claims from the main checkout, sets it, so a session a person starts there is not the orchestrator's run: its `session-start` neither re-displays nor heartbeats the orchestrator's holds, nor lists them, and its `release` of one is refused as another run's, while its own hand claims work as before (bops-4ez). A named run whose worktree is removed is stranded like any other. An orchestrator that does not set it still shares the checkout's run, as before. Sync's display repair never changes a held bead's `blocked` or `deferred` status, which a person set after the claim; it repairs only the assignee and the `run:` label (bops-yr2). Their work under each bead's lock is local `bd` calls (a show, a display repair that is usually a no-op, a heartbeat), expected to take well under the 10-second wait; this is not yet measured, and `bd` calls have no limit of their own, only the command's budget, so a slow Dolt can stretch it. The exception is a hold whose write was interrupted (in flight, or a release that never finished deleting its ref): settling it takes a network call or two, 20 s each at most, and a command that meets it then gets `ref_contention` for its caller to retry, losing nothing (a note's or release's text is kept at `saved_to`). A claim holds its bead longest (13 s median, 25 s at most, measured from `events.jsonl` before bops-7uu), but a bead being claimed is rarely touched by anything else: another claim of it gets `ref_contention`, which is the right answer, and `sync`, `session-start` and `sweep` defer it. The `bead_lock` lines in `events.jsonl` are what to re-size the wait from (bops-7uu); a command killed while it held a lock logs no `released` line, so the longest holds are undercounted, and a line that cannot be written (the log ignores write errors) or that rotation has dropped is gone too. An older bops, which takes no bead lock or does not re-check it, refuses a version 4 state file.
- `events.jsonl`: one line per step and outcome of every ownership command, rotated at 1 MB to one old copy (`events.jsonl.1`). Rotation takes its own operating-system lock (`events.lock`) without waiting and re-checks the size under it, so two processes that both find the file full never both rotate it, the second moving the first's fresh file over the old copy (bops-qef); one that finds the lock held leaves the rotation to its holder and appends, and one that cannot take it at all rotates unserialized, as before. bops 0.4.0 to 0.4.3 rotates without the lock, so the guarantee holds once the clone's `state.json` is version 4, which that bops refuses. Each command's bead lock adds a `bead_lock` line, its `command` naming `claim --force`, `claim --resume` and `session-start` apart from `claim` and `sync`: `released` with `waited_ms` and `held_ms` (0 included), logged just before the lock is let go; `refresh_failed` with both when the state could not be re-read under the lock; `busy` with `waited_ms` when a command gave up waiting, or `timeout` when its own time ran out first (both answer `ref_contention`); or `deferred` when `sync`, `session-start` or `sweep`, which never wait, skipped a busy bead. A `bead_locks_pruned` line counts the lock files a `sync` or `session-start` removed, logged only when it removed some or failed. Where file locks are missing there is no lock and no line. It is local and is the first thing to read when an outcome needs explaining.
- `refs/bops/seen/*` (in the clone, not this directory): the last fetched copy of every claim ref, written by `sync`, so `next` can apply W14 without the network.

Reserved for later, not written by v0.1.0: `watermark`, `journal.cursor`, `leases.json`.

## 4. Command manifest

The manifest is [`bops/commands.json`](bops/commands.json). It is embedded in the binary, printed by `bops manifest`, consumed by the generator in §8, and rendered to [`docs/COMMANDS.md`](docs/COMMANDS.md), which is golden-tested. It is the normative list of commands, arguments, wrapped calls and output fields; this section states only what is common to every command. Common to every command:

- `--json` (default when stdout is not a TTY) → one JSON document on stdout. Human mode prints the same facts as `key=value` lines.
- Exit codes: **0** done, or a read's verdict; **1** could not run (remote or ledger unreachable, precondition unverifiable, `bd` error); **2** usage; **3** a write's negative verdict (`lost`, `refused`), with `ok:false`, so `bops claim x && work` cannot proceed. Diagnostics on stderr only.
- Every JSON result carries `{"contract":1,"command":"<name>","ok":<bool>, ...}`. A failure that could not run also carries a stable `code` (below) and an `error` with credentials scrubbed.
- Budgets: `claim`, `release`, `note`, `land`, `session-start` and `doctor` have 60 s, `sync` 2 min, `sweep` 5 min, every other command 30 s; `serve-mcp` runs each tool call as its own bops command with the same budget, and kills one still running after 6 min. Each git call has its own 20 s limit and runs without a terminal prompt; `guard`, a hook on every Bash call, makes one local git call cut off at 2 s and allows the command past it. `session-start`'s budget matches Claude Code's default hook timeout, so its `bd dolt pull` gets only what is left after 25 s are kept for the stale count, the workable count and `bd prime`; a pull cut off then fails the sync with a message naming `bops sync`, whose 2 minutes may finish it, and the rest of the document still comes (bops-9rb). When a command's time runs out, a running `bd` gets SIGTERM and 5 s to exit before it is killed (killed at once on Windows), so a pull stopped inside a Dolt merge or commit has the chance to stop cleanly; whether `bd` uses that chance is not measured.

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

**Stable codes** (`code` on a result that could not run, and in `events.jsonl`): `unreachable` (remote not reached), `git_auth` (credentials refused), `lock_unsupported` (the forge rejects custom refs), `ref_contention` (a local ref update collided, or another bops process on this machine is working on the same bead; retry when it finishes), `state_write_failed` (the state file could not be written), `hold_not_recorded` (the ref was won but the hold could not be saved; the result names the sha and how to recover), `no_origin` (lock remote missing: no git remote named `lock.remote`, default `origin`) and `remote_mismatch` (the lock remote is not the Dolt remote, including when there is no Dolt remote; `lock.mode: ref` only), `bad_id` (not a bead id), `bad_message` (an unreadable claim ref), `git_failed` (any other git error), `bad_config` (`.bops.json`, the per-user file or a `BOPS_*` variable is invalid, §10; the command did nothing, and it is reported in the result only, since no ownership step ran to log), `bops_too_old` (this `bops` is older than `min_bops` in `.bops.json`, §10; a write did nothing, and the error names the upgrade command; reported in the result only).

## 5. Ordering rules for `next`

Deterministic, documented, and the only place ordering lives:

1. Exclude: not ready; epics; any failing lint rule in §6 (`needs-human`, `bops:hold` within window, `pool:` ≠ run's pool, any `needs:<cap>` not in run's caps, open gates, a live claim, a label in `blocking_labels`); and W14, a claim ref seen at the last `sync` that names another run. W14 reads `refs/bops/seen/*`, so `next` needs no network and is only as fresh as the last `sync`; `claim` is what decides.
2. Order: priority ascending (0 first) → beads whose parent epic already has in-progress siblings (finish what's started) → oldest `created_at` → ID. If the in-progress listing fails, `next` still returns candidates without that step and says so in `warnings`.
3. `why` lists which rules passed; `why_not` lists which rules excluded it (only with `--include-unworkable`).

The harness decides *among* candidates; `bops` never picks silently. `claim` is a separate call so the agent's choice is visible in the transcript.

## 6. Lint rules (workability)

Each rule has an id, a severity (`fail`|`warn`), and a one-line rationale; `--strict` promotes warns. Initial set (ids stable; add, don't renumber):

| id | severity | rule |
|---|---|---|
| W01 | fail | has non-empty acceptance criteria |
| W02 | fail | status permits work (open/ready; not blocked/closed) |
| W03 | fail | not `needs-human` (nor the legacy `needs:human`, §3.1) |
| W04 | fail | machine capability constraints satisfiable by this run |
| W05 | fail | no open gates on the bead. `bd ready` already leaves gated beads out, so `next` reports W05 only for the beads it looks up with `--include-unworkable`; `lint <id>` checks the bead's gates itself |
| W06 | fail | not claimed by a live run (claimed by stale run → warn W06s) |
| W07 | warn | has a parent epic (orphans are allowed but flagged) |
| W08 | warn | body under configured size (default 8 kB) — oversized beads overflow agent context |
| W09 | warn | last `bops:note` ends with a `NEXT:` paragraph (resume readability) |
| W10 | warn | no sibling bead with the same title (duplicate smell) |
| W11 | fail | no dependency cycle reachable (delegates to `bd dep cycles`) |
| W12 | fail | not labelled `bops:hold` (cooling off after a scope/abandoned release) |
| W13 | fail | in this run's pool: a run serves exactly one pool; unlabelled beads are the default pool, and a pooled run never takes default-pool beads |
| W14 | fail | no claim ref held by another run, as seen at the last `sync` (evaluated by `next`, which has the refs) |
| W15 | fail | no label the repo lists in `blocking_labels` (§10), such as a `frozen` its loop excludes. A plain `claim` refuses such a bead too, unless this run already holds it (`held`); `claim --force` and `claim --resume` do not read it |

`W06s` is the stale variant of W06: the bead carries a claim, but its last heartbeat/update is older than `stale_minutes` (default 120). It is a *warn*, so a stale claim is a candidate again; `claim` on it still requires `--force`, which records the takeover in the event comment. `claim --force` measures staleness from the claim ref's `at` (§3.0), and from `updated_at` only for a ledger claim with no ref.

W12's window runs from the bead's `updated_at`, because a label carries no timestamp. A bead with no `updated_at` stays held.

Rules the bead alone cannot answer (W09 needs comments, W10 needs the whole list, W11 needs bd) are evaluated by the commands that have that data; `bops lint <id>` reports them as not evaluated rather than passed.

## 7. Forge adapter

Only `sweep` (and optionally `land --verify`) touch a forge. Adapter interface: `is_merged(ref) → true|false|unknown`, `ref_sha(ref)`. Implementations: `github` (`gh api`), `gitlab` (`glab api`), `none`. Selection: `--forge` when given, else `forge` in config (§10), else (`auto`) inferred from the host of `git remote get-url <lock.remote>` (default `origin`), github or gitlab by name and `none` otherwise or when that remote is missing. Inference reads `lock.remote` even under `lock.mode: none`: it is the remote the repo is configured to share through. Nothing else in `bops` knows what a PR is.

## 8. Harness bindings (generated)

The generator reads `commands.json` and emits the targets below. `cc` and `codex` also read `branch_pattern` from the `.bops.json` of the repo they run in (§10); every other target is a pure function of the manifest.

| Target | Emits | Notes |
|---|---|---|
| Claude Code | `bops gen cc --out <dir>`: `skills/bops/SKILL.md` (hand-written preamble + generated reference), `hooks.json` fragment (`SessionStart → bops session-start --hook-json`), `guard-hooks.json` opt-in fragment (`PreToolUse` on `Bash` → `bops guard --hook-json`), `commands/bops-next.md` | ≤2k tokens; the model runs CLI commands. No `Stop` hook: it fires after every reply, and `bops sync` pushes and takes about 3 s, so the skill runs it before stopping. No `PreCompact` hook: its output never reaches the model, so it cannot prompt a `bops note`; the skill asks for notes at milestones, and `SessionStart` fires again after compaction. The guard is a separate file because it is opt-in: it denies `bd update --claim`, `bd ready --claim` and `bd reclaim` while the claim lock is on, prints nothing otherwise (an allow decision would skip the permission prompt), and fails open, `\|\| true` covering a missing `bops` |
| Codex | `bops gen codex --out <dir>`: `skills/bops/SKILL.md` (for `.agents/skills/bops/`) + `hooks.json` fragment to merge into `.codex/hooks.json` (`SessionStart → bops session-start --hook-json`, matcher `startup\|resume\|clear`) | same prose and hook policy as Claude Code; the hook shape follows upstream bd's Codex hooks, unverified against a live Codex run |
| Hermes | `tools/bops.json` tool descriptors mapping to CLI invocations | format TBD after Hermes research |
| Clojure (Kestrel or any) | `bops gen clj --out <dir>`: `bops.clj` (namespace `bops`: one fn per command whose `surfaces.clj` is `fn`, shelling out to `bops <command> --json` and returning `{:exit :doc :err}`; a Malli schema per output, the envelope merged with the command's `output`) + `bb.edn` | exit 3 is returned, not thrown; a bad call (missing required option, unknown option) throws before `bops` runs; in-process option later |
| MCP | `bops serve-mcp` on stdio serving `bindings/mcp/tools.json` (`bops gen mcp`): one tool per command with an `mcp` surface; `kind: write` tools omitted unless `--enable-writes`; results paginated at 20 kB with a cursor | respects CC's 25k-token result cap |
| Docs | `docs/COMMANDS.md` | |

Bindings carry `contract=<n>` and refuse to run against a mismatched `bops version`.

## 9. Testing contract

- Every command has a test suite that stubs `bd` (and `gh`/`glab`) with a fake returning fixture JSON; no suite requires Dolt.
- One integration suite runs against a throwaway `bd init` ledger in CI, gated behind `BOPS_INTEGRATION=1`.
- A two-replica suite runs in the same CI job against real `bd` (pinned, checksum-verified) and a bare git remote: two clones race to claim one bead at least 40 times with distinct actors and 40 with one shared actor, and every trial must end with exactly one `claimed` and one `lost`. It also covers takeover, resume, worktrees, a slow remote, a failing pre-push hook, offline `note`, ledger repair by `sync`, and `sweep`. `BOPS_RACE_TRIALS` sets the trial count.
- Golden-file tests for every generated binding; a change to a golden file is a contract review.
- A suite exists for each bug that shipped — pin the bug, then fix it.

## 10. Configuration

`.bops.json` in repo root (committed) merged with `$XDG_CONFIG_HOME/bops/config.json` (per user) and `BOPS_*` env. Keys: `pool`, `caps[]`, `hold_minutes`, `stale_minutes`, `forge`, `lint.strict`, `lint.disable[]`, `note.require_next` (bool), `lock.mode`, `lock.remote`, `blocking_labels[]`, `min_bops`, `branch_pattern`.

Precedence, lowest to highest: built-in defaults, `.bops.json` in the worktree root, the per-user file (`~/.config/bops/config.json` when `XDG_CONFIG_HOME` is unset), `BOPS_*` env, then command flags. Each layer sets only the keys it names, except `blocking_labels`, to which each layer adds, so no layer can drop a label a lower one names; a missing file is skipped. Nested keys are JSON objects (`{"lint": {"strict": true}}`), and each key's variable is `BOPS_` plus its path in upper case with `.` as `_` (`BOPS_STALE_MINUTES`, `BOPS_LINT_STRICT`); a list in the environment is comma-separated and an empty variable is unset. An unknown key, a wrong type, a non-integer or non-positive minute count, a `forge` outside `auto|github|gitlab|none`, a `lock.mode` outside `ref|none`, `lock.mode: none` from a layer above `.bops.json` when the repo file does not set it, a `blocking_labels` entry that is empty or holds whitespace, a comma or a non-printable character, or a `lock.remote` that is not a plausible git remote name (a letter or digit, then up to 99 letters, digits, `.`, `_` or `-`; no `..`, not ending in `.` or `.lock`) fails the command (exit 1, `code: bad_config`) naming the file or variable. The loader is `bops/internal/config`.

| Key | Default | Applied by |
|---|---|---|
| `stale_minutes` | 120 | `next` (W06s), `claim --force`, `stale` when `--older-than` is not given, `session-start` |
| `hold_minutes` | 120 | `next` and `lint` (W12) |
| `pool`, `caps[]` | none | every command's run; `next --pool/--caps` override |
| `forge` | `auto` | `land --verify` and `sweep` when `--forge` is not given (§7) |
| `lint.strict`, `lint.disable[]` | `false`, none | loaded and validated; not applied yet (`lint --strict` is the flag). Only `.bops.json` may disable `W15`, so no layer undoes `blocking_labels` (bops-jmp) |
| `note.require_next` | `false` | loaded and validated; not applied yet |
| `lock.mode` (`BOPS_LOCK_MODE`) | `ref` | every command and `doctor`: `none` turns the claim lock off for a one-machine ledger (§3.0). Only `.bops.json` may set `none` |
| `blocking_labels[]` (`BOPS_BLOCKING_LABELS`) | none | `next` and `lint` (W15), and a plain `claim`, which refuses a bead carrying one. Additive across layers. bops's own labels (`bops:`, `run:`, `pool:` prefixes) are refused, and `doctor` warns of a label no bead carries (bops-jmp) |
| `min_bops` | none | every command and `doctor`. Only `.bops.json` may set it, as `MAJOR.MINOR.PATCH` with an optional `v`. A write or read+sync command run by an older `bops` exits 1 with `code: bops_too_old` before any step; a read answers with a warning; `doctor` fails a `bops_version` check. A prerelease or pseudo-version counts as older than its release, and a build with no version (`dev`) is not checked: `doctor` warns (bops-n4r) |
| `branch_pattern` | `work/<id>` | `gen cc` and `gen codex`: the branch the skill names for a bead's worktree. Only `.bops.json` may set it, at most 40 letters, digits, `.`, `_`, `/`, `-` and `<placeholder>` words that git accepts as a branch name (bops-onn). |
| `lock.remote` | `origin` | every command that reads or writes claim refs, the ledger check (§3.0) and `doctor` (`BOPS_LOCK_REMOTE`), and forge inference (§7); the lock itself is unused under `lock.mode: none` |

Other environment `bops` reads: `BEADS_ACTOR` (actor), `BOPS_MACHINE` (machine name, else the hostname), `BOPS_RUN` (a named run within the worktree, §3.4), `XDG_STATE_HOME` (§3.4), and `BOPS_INTEGRATION` / `BOPS_RACE_TRIALS` in tests.

## 11. Versioning and compatibility

- `bops` semver; `contract` integer. Minor releases may add commands/fields; removing or renaming anything bumps `contract`. Contract 1 freezes at the `bops/v0.1.0` tag (§1.9 waiver).
- Supported `bd`: ≥ 1.3 (`bd update --claim`, `--assignee --status --force` for display repair, `bd heartbeat`, `bd comments <id> --json`); CI pins and tests 1.3.4. `bd reclaim` ignores the claim lock, so it is not run on a ledger `bops` claims on. `bops version` reports the detected `bd` and backend and warns outside the tested range. It also reports the commit the binary was built from (`revision`, `modified`, `revision_time`, from the go command's build info; empty for a build outside git). In bops's own source checkout, `doctor` warns, without failing, when that commit predates the last commit to change `bops/`, naming the rebuild command; elsewhere it makes no such check.
- Supported git remotes: any that accept custom refs under `refs/bops/` with `--force-with-lease`. GitHub is measured. A forge that rejects them gives `code: lock_unsupported`.
- Upgrade every machine on a ledger before the first claim with a new `bops`: an older binary ignores the lock. `min_bops` (§10) makes that a check.
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

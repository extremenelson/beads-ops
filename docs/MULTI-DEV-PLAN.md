# Beads for Multiple Developers × Multiple Machines — Rollout Plan

**Status:** v0.1 — grounded in upstream docs at gastownhall/beads commit `cbfdafc` (2026-11-01): `sync-concepts.md`, `sync-setup.md`, `architecture/dolt.md`, `protected-branches.md`, `git-integration.md`, `configuration.md`, CLI reference for `bd sync` / `bd reclaim` / `bd heartbeat`.
**Companion:** `PROCESS.md` (operating process). This document covers topology, rollout, and operations; `PROCESS.md` covers how people and agents use the graph day to day.
**Scope:** N developers (humans + their agents), each on 2–4 machines, one project, one shared issue graph. Machines are sometimes offline. Everyone already shares a git remote.

---

## 1. The decision

**Recommendation: hub-and-spoke on the existing git remote. Embedded Dolt on every clone. `bd sync` at session boundaries. No new infrastructure.**

Why this beats the alternatives:

- Dolt stores issue history under `refs/dolt/data` on the same git remote the code uses. There is nothing new to host, secure, back up, or keep awake. Offline work is native: every clone is a full replica.
- Hash IDs (`bd-a1b2`) mean concurrent creates on different machines never collide. Dolt merges at the cell level, so two people only conflict when they edit the *same field of the same issue* — which the claim discipline in `PROCESS.md` makes rare.
- `bd sync` (new in the 0.60 line) is the whole multi-machine loop in one command with machine-readable exit codes: pull → positive conflict detection → recompute `is_blocked` → push with bounded retries. It never auto-resolves anything it can't settle convergently; it exits 2 and waits for a human. That is exactly the safety property you want when agents are the ones running it.
- Embedded mode needs no `dolt` binary on the machine and has zero ops surface. Server mode is only required for *concurrent writers on the same machine*, which this topology doesn't create.

### Ranked alternatives and when to switch

| # | Option | Pick it instead when |
|---|---|---|
| 2 | **Shared Dolt server per developer machine** (`bd init --server` or `dolt.shared-server: true`), still syncing to git origin | A developer runs Hermes + Kestrel + Claude Code *simultaneously* on one box and hits `database is locked`. Switch that machine only; the remote model is unchanged. |
| 3 | **Central team Dolt server** (`dolt sql-server` on a VPS/LAN host, every clone in server mode pointing at it; or the experimental `beads-team-server`) | You need sub-second visibility of each other's claims (true real-time dispatch across people), or you have >~8 active writers and push races start exhausting `bd sync` retries (exit 3) routinely. Cost: a service to run, TLS, credentials file, no offline writes. |
| 4 | **DoltHub / S3 remote** instead of git origin | The git host blocks custom refs or you want issue history separate from code access control (e.g., contractors see issues but not source). |
| 5 | **Federation** (`bd federation add-peer`, peer-to-peer towns) | Two *independent* projects with their own graphs need to exchange issues. Not for one project. Criss-cross merges can conflict under Dolt 2.6; avoid unless the organizational boundary is real. |

Don't pick 3 pre-emptively "for scale." Option 1 degrades gracefully into exit-3 retries long before it breaks, and that is the signal to move.

---

## 2. Architecture

```
                       git remote (GitHub/GitLab)
                 ┌──────────────────────────────────────┐
                 │  refs/heads/*      ← code, PRs        │
                 │  refs/dolt/data    ← issue graph      │
                 └───────▲──────────────────▲────────────┘
                         │ bd sync          │ bd sync
          ┌──────────────┴──────┐    ┌──────┴───────────────┐
          │ Dev A                │    │ Dev B                │
          │  laptop   .beads/    │    │  desktop  .beads/    │
          │  desktop  .beads/    │    │  server   .beads/    │
          │  (each: embedded     │    │  (each: embedded     │
          │   Dolt, full replica)│    │   Dolt, full replica)│
          └──────────────────────┘    └──────────────────────┘
```

Properties:

- **Source of truth** is the Dolt history at `refs/dolt/data`. `.beads/issues.jsonl` is an export for viewers only; never `bd import` it as sync.
- **Committed to git:** `.beads/.gitignore`, `.beads/metadata.json`, `.beads/config.yaml` (carries `sync.remote`), `.beads/formulas/*.toml`, `AGENTS.md`, `.claude/settings.json`, `docs/PROCESS.md`.
- **Never committed:** `.beads/embeddeddolt/` (or `.beads/dolt/`), `.beads/push-state.json` (per-machine), `.beads-wisp/`.
- **Locks:** on a `bops` ledger the cross-developer lock is `bops claim`: one git ref per bead, `refs/bops/claims/<id>`, beside `refs/dolt/data` on the same remote, written with a create-only or leased push so exactly one machine wins ([SPEC §3.0](../SPEC.md#30-the-claim-lock-ownership)). `bd update <id> --claim` compares and sets on one replica only: two machines can both claim, and last-write-wins settles it at the next sync with nobody told (measured on bd 1.3.4). Without `bops`, that claim and its lease (`bd heartbeat`, `bd reclaim`) are the lock, with that limit.
- **Identity:** `BEADS_ACTOR` resolves per machine from `git config user.name` by default. Pin it explicitly (§4.2) so the audit trail reads the same from every machine a person owns.

---

## 3. Prerequisites and pins

| Item | Requirement | Why |
|---|---|---|
| `bd` | ≥ 0.60.0, same minor across the team | `bd sync` exists; schema compatibility |
| `dolt` CLI | **Not required** for embedded mode. If anyone goes to option 2/3: pin **2.6.0** exactly, never `releases/latest` | 2.3.x has a `DOLT_RESET('--hard')` regression that breaks `bd dolt pull`'s rollback; `latest` can resolve backwards |
| git remote | SSH preferred: `git+ssh://git@host/org/repo.git` | HTTPS works but credential handling via `DOLT_REMOTE_*` is clumsier |
| Cloud-synced filesystems | `.beads/` must **not** live in Dropbox/OneDrive/iCloud-synced paths | Dolt file locks + sync clients = `disk I/O error (522)` |
| Version policy | One person owns the `bd` version bump; everyone upgrades within a week | Schema-ahead databases refuse older binaries (§7) |

---

## 4. Rollout phases

### Phase 0 — Decide and document (1 hour, one person)

1. Confirm the git host preserves custom refs: `git ls-remote origin | grep -c dolt` after the first push should return 1. GitHub and GitLab do; some mirrors and read-only proxies don't.
2. Agree the team `bd` version. Record it in `AGENTS.md`.
3. Agree actor handles (`BEADS_ACTOR` = GitHub handle is the obvious choice).
4. Agree on the `.gitignore`/commit policy in §2.

Exit: this document and `PROCESS.md` committed under `docs/`.

### Phase 1 — Seed the hub (30 minutes, repo owner, from the machine with the authoritative graph)

```bash
cd repo
bd version                                  # ≥ 0.60.0
bd init                                     # or verify existing: bd dolt remote list
bd dolt remote list                         # expect: origin <git url>
# if missing (project initialized by an older bd):
bd export -o .beads/issues.pre-remote.jsonl # audit snapshot
bd dolt remote add origin git+ssh://git@github.com/org/repo.git
bd dolt push
git ls-remote origin | grep dolt            # <hash> refs/dolt/data

bd hooks install --beads                    # thin shims; safe with lefthook/husky via --chain
bd setup claude                             # project .claude/settings.json + CLAUDE.md section
git add .beads/.gitignore .beads/metadata.json .beads/config.yaml .gitignore AGENTS.md .claude/settings.json
git commit -m "beads: enable Dolt sync via refs/dolt/data"
git push
```

If the project previously used the removed `sync.branch` / `beads-sync` workflow: `bd config set sync.branch ""`, remove `.git/beads-worktrees`, `git worktree prune`, archive the old branch.

Exit: `refs/dolt/data` exists on origin; `config.yaml` with `sync.remote` is committed.

### Phase 2 — Onboard every developer and every machine (15 minutes per machine)

Per machine runbook (make this a formula later — `mol-onboard-machine`):

```bash
# 1. identity (shell profile)
export BEADS_ACTOR="<github-handle>"

# 2. clone and bootstrap
git clone git@github.com:org/repo.git && cd repo
bd version                                  # matches team pin
bd bootstrap                                # clones refs/dolt/data, wires origin as Dolt remote
bd list | head                              # issues visible
bd vc status                                # clean working set
bd doctor                                   # green

# 3. agent harness
bd setup claude                             # idempotent; adds hooks for this clone
#    (Hermes / Kestrel: AGENTS.md already carries the section; see PROCESS.md §9)

# 4. prove the loop
bd sync; echo "exit=$?"                     # expect 0
```

If `bd bootstrap` fails, the manual path is in upstream `sync-setup.md` (init → `bd dolt stop` → remove empty db → `dolt clone` → `bd dolt start` → `bd migrate --yes`). It requires the `dolt` CLI; prefer fixing bootstrap.

Exit per machine: `bd sync` returns 0 and `bd list` matches another machine.

### Phase 3 — Make sync automatic (1 hour, one person; then it's config)

`bd sync` is idempotent and safe to run often. Three triggers:

**(a) Agent session boundaries** — in `.claude/settings.json` (and the equivalent for Hermes/Kestrel per `PROCESS.md` §9):

```json
{
  "hooks": {
    "SessionStart": [{ "matcher": "", "hooks": [
      { "type": "command", "command": "bd sync --json >/dev/null 2>&1; bd prime" },
      { "type": "command", "command": "bd gate check 2>/dev/null || true" },
      { "type": "command", "command": "bd list --status in_progress --json 2>/dev/null || true" }
    ]}],
    "Stop": [{ "matcher": "", "hooks": [
      { "type": "command", "command": "bd sync --json >/dev/null 2>&1 || true" }
    ]}],
    "PreCompact": [{ "matcher": "", "hooks": [
      { "type": "command", "command": "bd prime" }
    ]}]
  }
}
```

Verify the `Stop` hook name against your installed Claude Code version before relying on it; `SessionStart` and `PreCompact` are what upstream ships.

**(b) Git edges** — `bd hooks install --beads` already wires `pre-commit` (JSONL export when `export.auto`), `post-merge`, `post-checkout`, `pre-push`. Add a chained `pre-push` that runs `bd sync` so a code push never leaves issue state behind:

```bash
# .beads/hooks/pre-push (bd hooks run pre-push is already there; chain this)
bd sync --json >/dev/null || { echo "bd sync failed (exit $?); push anyway? issue state not published"; }
```

**(c) Timer on always-on machines** (Alex's home server, any dev box that stays up) — a user-level systemd timer every 15 minutes:

```ini
# ~/.config/systemd/user/beads-sync@.service
[Unit]
Description=bd sync for %I
[Service]
Type=oneshot
WorkingDirectory=%h/src/%i
ExecStart=/usr/local/bin/bd sync --json
SuccessExitStatus=0 3
# exit 2 (conflict) and 4 (stuck working set) stay failures → visible in `systemctl --user --failed`

# ~/.config/systemd/user/beads-sync@.timer
[Unit]
Description=bd sync timer for %I
[Timer]
OnBootSec=2m
OnUnitActiveSec=15m
RandomizedDelaySec=2m
[Install]
WantedBy=timers.target
```

```bash
systemctl --user enable --now beads-sync@repo.timer
```

Exit 3 (push race / another writer busy) is treated as success here because the next tick retries. Exit 2 and 4 need a human (§6).

Exit criterion: a change made on machine A appears on machine B within one timer tick without anyone typing `bd`.

### Phase 4 — Team conventions (committed to `AGENTS.md`, enforced by habit)

- **Claim before edit, one claim per actor per session.** The claim is the lock: `bops claim <id>`, which needs the network. A claim exits 3 when someone else has it; pick another bead. `bops claim --force` takes over a claim idle for 120 minutes, and the rest of recovery is [PROCESS §12](PROCESS.md#12-recovery-runbook-bops-ledgers). Without `bops`: `bd ready --claim` only takes unassigned issues, `--claim` on someone else's in_progress refuses (anti-steal), and `--force` is for abandoned claims only.
- **Claim pools for dispatch.** A lead pre-assigns issues to a pool alias and lists it: `bd config set claim.pools "core-crew,night-crew"`. Anyone can `bd update <id> --claim` a pool-assigned issue; nobody can steal a person's. This is the mechanism for "here's this week's work, take what you want."
- **Keep long work visibly alive.** On a `bops` ledger, `bops note` at each milestone refreshes the claim ref, which is what `--force` measures; do not run `bd reclaim` at all, because it ignores the lock and would hand out beads someone still holds. Without `bops`, loops call `bd heartbeat <id>` faster than the lease TTL, and one always-on machine runs `bd reclaim --older-than 2h` on the sync timer.
- **Upgrade together.** Every machine runs a `bops` that knows the lock before anyone claims with it; an older binary ignores the lock.
- **Per-machine work gets a label** (`gpu-3090`, `mac-arm`, `debian-box`). Machine-bound agents filter `bd ready` by their label plus unlabeled.
- **Priority and epic ownership** per `PROCESS.md` §1: only designated humans set P0/P1 and close epics.
- **Notes are the handoff**, including handoff to *another person's* machine. The `COMPLETED / IN PROGRESS / NEXT / BLOCKER` format is the contract.

### Phase 5 — Pilot, then expand (2 weeks)

Week 1: two developers, all their machines, one epic. Measure: `bd sync` exit-code distribution (from the timer journal), number of manual conflict resolutions, time-to-visibility A→B.
Week 2: whole team.

Go/no-go to expand: zero exit-4s, ≤1 exit-2 per developer-week, no one has typed `bd import`.

---

## 5. Day-to-day protocol (the human version of PROCESS.md §2)

```
start of session   bd sync → bops next → bops claim <id>        (no bops: bd ready → bd update <id> --claim)
during             bd update <id> --append-notes "...", bd create ... --deps discovered-from:<id>
switching machine  bd update <id> --append-notes "<handoff>"; bd sync     ← non-negotiable
end of session     bd close <id> --reason "..."; bd sync
offline            work normally; bd sync when back (note "REF: offline, unsynced" in the handoff)
```

The single rule that prevents 90% of multi-machine pain: **sync before you stand up from a machine.** Unpushed Dolt commits exist only on that disk.

---

## 6. Conflicts and recovery

### What conflicts, what doesn't

| Situation | Result |
|---|---|
| Two people create issues offline | Never conflicts (hash IDs) |
| Two people edit different fields of one issue | Cell-level merge, no conflict |
| Two people claim the same issue with `bd`, on two replicas | No conflict: last-write-wins settles `assignee` at the next sync, and the losing agent is not told (measured on bd 1.3.4). This is why `bops claim` exists; with it, one claim wins and the other exits 3 |
| Two people append notes to one issue offline | Same cell → **exit 2** (last-write-wins is applied only where the pull can settle convergently; notes on the same issue generally can't) |
| Dependency edges added on two replicas | Merged; `bd sync` recomputes `is_blocked` so `bd ready` isn't stale |
| Machine-local metadata (`push-state.json`) | Never in the database; never conflicts |

The claim discipline exists to make the exit-2 row above rare and the silent claim row impossible: if you hold the claim, nobody else should be writing to that bead.

### Exit-2 runbook (merge conflict)

```bash
bd sync --json                     # confirms exit 2, says whether merge is aborted or live
bd doctor                          # shows conflict tables
bd doctor --fix                    # settles what it safely can
bd vc merge <branch> --strategy ours|theirs   # when doctor can't; pick deliberately, then
bd sync                            # re-run; expect 0
```

Policy: whoever owns the *claim* on the conflicting bead resolves it; their copy of notes wins (`ours` from their machine). Record the resolution in the bead's notes.

### Exit-4 runbook (stuck dirty working set)

Something left uncommitted edits in the local Dolt working set and they aren't clearing. `bd vc status` shows them. `bd dolt commit` if they're legitimate; `bd doctor --fix` if they're junk. Then `bd sync`.

### Machine lost / database corrupt

```bash
bd doctor --deep
# if unrecoverable:
rm -rf .beads/embeddeddolt      # local replica only — the hub is intact
bd bootstrap                    # re-clone from refs/dolt/data
```

Anything unsynced on that machine is gone. This is why §5's rule exists.

### Hub lost (git remote rewritten, `refs/dolt/data` deleted)

Any clone that synced recently is a full replica: `git update-ref -d refs/dolt/data` on the remote side is irrelevant to it. From the most recent clone: `bd dolt push`. If the push says "no common ancestor", clear the stale ref first (`git update-ref -d refs/dolt/data` locally) and push again.

### Hard rules

- Never run raw `dolt` commands while `bd` has the database open.
- Never delete files inside `.dolt/`/`embeddeddolt/` (including `noms/LOCK`) by hand — `bd doctor --fix --yes` handles stale locks.
- Never `bd import issues.jsonl` as a sync shortcut — upsert-only, resurrects deleted/pruned beads.
- Never `export BEADS_DIR` globally.

---

## 7. Upgrades and schema migrations

Upgrading `bd` is the one operation that is genuinely multi-machine-sensitive. A database migrated by a newer binary refuses older binaries (`schema version mismatch`). Procedure, from upstream's upgrade guide:

1. **Everyone syncs** (`bd sync` → 0 on every machine). Announce a freeze window.
2. **One designated clone** (the upgrade owner): `bd export --all` (backup), install new `bd`, `bd info --whats-new`, `bd migrate schema`, `bd hooks install`, `bd dolt push`.
3. **Everyone else:** install the new binary, then `bd bootstrap` (not `bd dolt pull`) if the release notes say the migration crossed a schema version; otherwise `bd sync`.
4. Anyone who can't upgrade yet can read with `BD_IGNORE_SCHEMA_SKEW=1` but must not write.

Put the team pin in `AGENTS.md` and have the SessionStart hook compare `bd version` against it — a one-line shell check that prints a warning is enough.

---

## 8. Operations

| Cadence | Who | What |
|---|---|---|
| Every 15 min (timer, always-on boxes) | automatic | `bd sync`; `bd reclaim --older-than 2h` only on a ledger without `bops` |
| Daily | automatic | `bd backup sync` to a NAS/S3 path registered with `bd backup init` (full Dolt backup, unlike JSONL) |
| Weekly | lead | `bd stats`; review exit-2/4 journal; `bd mol wisp gc --closed --force` |
| Monthly | lead | `bd prune --older-than 90d --dry-run` → `--force`; `bd flatten` if the store has grown; `bd doctor --deep` |
| Per release | upgrade owner | §7 |

Observability: the systemd journal for the timer is the metric source. `journalctl --user -u 'beads-sync@*' | grep -c 'exit=2'` is your conflict rate.

---

## 9. Failure modes this plan is designed around

| Failure | Consequence | Mitigation |
|---|---|---|
| Developer forgets to sync before switching machines | Stale `bd ready`, duplicate claims | Stop hook + pre-push chain + timer; §5 rule |
| Two agents on one box write concurrently | `database is locked` | Switch that box to shared-server mode (option 2); nothing else changes |
| Schema upgraded on one machine, others lag | Older binaries refuse the db | §7 freeze window; version pin check in SessionStart |
| Someone commits `.beads/embeddeddolt/` | Repo bloat, merge chaos | `bd doctor --fix` repairs `.gitignore`; `git rm --cached -r` |
| Git host drops `refs/dolt/data` | Hub gone | Any recent clone is a full replica; re-push |
| `.beads/` on a cloud-synced folder | I/O 522 errors, corruption | Prerequisite check; `bd doctor` warns |
| Crashed agent holds a claim forever | Work blocked for the team | `bops` ledger: `bops claim --force` after 120 idle minutes (PROCESS §12). Without `bops`: lease + `bd heartbeat` + timer-driven `bd reclaim` |
| Two machines claim one bead | Both agents work it; one loses silently at sync | `bops claim`; raw `bd` claims and `/bead-loop` still bypass it (bead `bops-t8j`) |
| Exit-3 storms (push races) | Nothing lost, but sync stalls | Raise `--attempts`; if chronic with many writers, move to option 3 |

---

## 10. Open items

- **Lease TTL default and `bd heartbeat` cadence** — not pinned down in the docs I read; confirm with `bd heartbeat --help` and `bd reclaim --help` on the installed version before setting the timer's `--older-than`. Applies only to ledgers without `bops`; `bops` ledgers do not run `bd reclaim`.
- **Claude Code `Stop` hook name** — verify against the installed version; upstream only ships `SessionStart` and `PreCompact`.
- **`bd init --team` wizard** — exists; I haven't run it and don't know what it configures beyond the defaults. Try it on a scratch repo before recommending it over the explicit Phase 1 steps.
- **Hermes/Kestrel hook surfaces** — same caveat as `PROCESS.md` §11.
- **Whether to turn on `versioned-history.enabled`** — gives `bd versions <id>` snapshots per change; useful for audit with multiple people, costs storage. Decide after the pilot.

---

*Sources: upstream docs/core-concepts/sync-concepts.md, docs/getting-started/sync-setup.md, docs/architecture/dolt.md, docs/reference/{protected-branches,git-integration,configuration,troubleshooting}.md, docs/CLI_REFERENCE.md (`bd sync`, `bd reclaim`, `bd heartbeat`, `bd init`), README upgrade section — gastownhall/beads @ cbfdafc.*

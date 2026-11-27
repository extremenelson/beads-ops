---
name: bops
description: Work a beads issue through the bops loop (next, claim, note, land, release). Use when picking up, resuming, finishing or giving back a bead yourself in a repo that uses bops; not in an agent /bead-loop dispatched.
---

# bops: the work loop for a bead

`bops` wraps `bd` in intent-level commands. Every command prints JSON when piped: read `ok`, `outcome` and `code`; exit codes are at the end.

An agent whose orchestrator claimed for it (`/bead-loop`) follows the orchestrator, not this skill. A repo protocol file (`docs/agent-protocol.md`) wins on branch, worktree, landing and sync.

A claim is held by a git ref on `origin`, which makes it safe across machines and which raw `bd update --claim` bypasses. `claim`, `release` and `land` need the network.

## The loop

1. **Orient.** Run `bd list --status in_progress --json`. Work the bead the person named; otherwise unfinished work comes first. A bead assigned to your name may be another session's (agents share names): read `bd comments <id>`, and ask if unsure. `bops:landing` means done and waiting on its PR.
2. **Choose.** Run `bops next`. Take the first unless you have a reason not to; `next` never claims. `bd show <id>` gives its acceptance criteria.
3. **Make a worktree, then claim inside it.** Your run identity belongs to the worktree:
   ```bash
   bd worktree create .worktrees/<id> --branch work/<id>
   cd .worktrees/<id>
   bops claim <id>
   ```
   If `bd worktree create` refuses, use `git worktree add .worktrees/<id> -b work/<id>` instead. If the outcome is `lost` or `refused`, run `git worktree remove .worktrees/<id>` and choose again: `next_candidate` names one. `held` means this run already has it, so carry on. `ledger_pending: true` means you own the bead and `bops sync` will finish the bookkeeping. Never `--force` a claim that may be live: it works only after `stale_minutes` idle (`.bops.json`, default 120), and only a person decides that.
4. **Work.** Run `bops` commands from inside the worktree until you remove it in step 7. Put the bead id in commit subjects.
5. **Note your progress.** Run `bops note` at each milestone, **at least once an hour during long turns**, and before you stop. After `stale_minutes` with no note, a claim counts as abandoned on other machines. `note` refreshes the claim directly, and works offline with a warning:
   ```bash
   bops note <id> <<'EOF'
   Finished the parser. Tests pass.

   NEXT: wire it into cmd/bops and add the failing-input case.
   EOF
   ```
   Write for a reader with no context. End with a paragraph starting `NEXT:`, `BLOCKER:`, `KEY DECISION:` or `REF:`. Text goes through stdin or `--file`, never as an argument.
6. **Open the PR, then record it.** Push your branch to `origin`, open the PR against the base branch the person named or else the default branch, then:
   ```bash
   bops land <id> --ref <pr-url> --sha "$(git rev-parse HEAD)" --verify
   ```
   `land` records evidence; it never merges or closes. `--verify` checks the PR is at that commit. Add `--forge github|gitlab` only if `--verify` says no forge is configured.
7. **Let go.** Run `bops release <id> --reason done`, then remove the worktree with `git worktree remove .worktrees/<id>` from the main checkout.
8. **Before you stop,** run `bops sync`, from the main checkout if you have removed the worktree.

## Giving a bead back

Use `bops release <id> --reason <reason>`, with your explanation in a `bops note` first.

| Reason | When | What happens |
|---|---|---|
| `done` | Work is landed | Stays in progress until its PR merges |
| `blocked` | A dependency stops you | Status becomes blocked |
| `scope` | The bead is wrong or too big | Back to the pool after a cooling-off period |
| `abandoned` | You are abandoning it | Same as `scope` |
| `human` | A person must decide | Out of the pool until a person removes `needs-human` |

## Closing

Do not close beads yourself. `bops sweep` previews which landed beads have merged PRs, and `bops sweep --dry-run=false` closes them; a person or a scheduled job runs that. In beads-ops itself, rebuild first after a `bops/` change merges: `(cd bops && go install ./cmd/bops)`; `bops doctor` warns when it is older.

## When something goes wrong

- **`refused`, `not held by this run`:** you are in the wrong directory, or another run owns the bead. Check `pwd` first.
- **`release --reason done` is `refused` with `no land record`:** `bops land` has not succeeded for this bead. Fix and rerun `land`, then release again; you still hold the bead. If there is no PR to land, use `--reason scope` instead.
- **`lost`:** someone else holds the bead now, and `holder` says who. Stop work on it and tell the person. A refused note's text is at `saved_to`.
- **`sync` lists `lost_claims`:** those beads were taken over. Stop work on them.
- **`unreachable` or `git_auth`:** `bops` could not reach `origin`. Follow `retry`, or run the credential command in `error`.
- **`could not save run state` or `state lock`:** another `bops` process is busy. Wait and retry once.
- **`ref_contention`:** another `bops` process here has the bead for a moment. Rerun it after a few seconds, at most 3 times. A refused note's text is at `saved_to`.
- **`ledger_pending: true`:** you still own the bead. Run `bops sync`, and report it if that does not clear it.
- **Moving machines:** after `bops note` and a branch push, the person runs `bops claim --resume <id>` from the new machine's worktree. Never in an autonomous loop.
- **A reviewer asks for changes after `release --reason done`:** run `bops claim --resume <id>` from a worktree, then continue from step 4. Only the actor who landed it can.
- **`bops sync` lists `stranded_holds`:** those beads were held by a removed worktree. If the work is abandoned, run `bops release <id> --reason abandoned` from any worktree of this clone.
- **A bead looks abandoned:** `bops stale` lists idle claims and orphan refs; `bops reap` previews those with no visible work, `bops gc` the refs to delete. Acting on either, or `claim --force`, is a person's call: report the list. Runbook: PROCESS §12.

## Command reference (contract 1)

Generated by `bops gen cc`. If `bops version` reports a contract other than 1, stop and tell the person: this skill does not match the binary.

Exit codes: **0** done, or a read's verdict; **1** could not run: remote or ledger unreachable, precondition unverifiable, bd error; **2** usage; **3** negative verdict on a write: lost or refused, ok:false.

| Command | Kind | Intent |
|---|---|---|
| `bops next [--caps] [--include-unworkable] [--n] [--pool]` | read | What should I work on? |
| `bops claim [<id>] [--force] [--next] [--resume]` | write | Take this bead for this run |
| `bops note <id> [--file <path>]` | write | Record where I am, keep the claim |
| `bops renew <id>` | write | Heartbeat my claims, no comment |
| `bops release <id> --reason <reason> [--file <path>\|-]` | write | Give the claim back |
| `bops land <id> [--forge] --ref <ref> --sha <sha> [--verify]` | write | Record that work reached its destination |
| `bops sweep [--dry-run] [--forge]` | write | Close landed beads whose refs are merged |
| `bops stale [--all-machines] [--older-than]` | read | Claims nobody is working |
| `bops reap [--dry-run] [--older-than] [--only]` | write | Release stale claims that hold no work |
| `bops gc [--dry-run] [--only]` | write | Delete refs of closed beads and orphans |
| `bops lint <id> [--strict]` | read | Is this bead workable? |
| `bops sync [--no-push]` | write | Pull, heartbeat, push — with the one retry |
| `bops doctor` | read | Can the claim lock work against its remote (lock.remote, default origin)? |
| `bops gates <id>` | read | What is blocking this bead's path? |
| `bops deps <id> [--engine] [--path] [--transitive]` | read | Blockers, dependents, longest path |
| `bops version` | read | Report bops, contract, and detected bd |

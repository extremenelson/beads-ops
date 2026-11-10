---
name: bops
description: Work a beads issue through the bops loop (next, claim, note, land, release). Use when picking up, resuming, finishing or giving back a bead in a repo that uses bops.
---

# bops: the work loop for a bead

`bops` wraps `bd` in intent-level commands. Use it instead of raw `bd update --claim`, which skips this loop's bookkeeping. Every command prints JSON when piped: read `ok` and `error`. Exit 0 means it ran, 1 means it could not, 2 means bad usage.

**Hand-written for v0.1.0. Until the claim lock ships, do not work the same bead from two machines at once:** `bd`'s claim is atomic on one machine only.

## The loop

1. **Orient.** Run `bd list --status in_progress --json`. If the person named a bead, work on that one. Otherwise, unfinished work comes before anything new. Agents can share one assignee name, so a bead assigned to your name may belong to another session: read `bd comments <id>`, and ask if you can't tell whose it is. `bops:landing` means the work is done and waiting on its PR.
2. **Choose.** Run `bops next`. Candidates are already in priority order, so take the first unless you have a reason not to. `bops next` never claims for you. Read the bead with `bd show <id>` for its description and acceptance criteria.
3. **Make a worktree, then claim inside it.** Your run identity belongs to the worktree, so the order matters:
   ```bash
   bd worktree create .worktrees/<id> --branch work/<id>
   cd .worktrees/<id>
   bops claim <id>
   ```
   If `bd worktree create` refuses, use `git worktree add .worktrees/<id> -b work/<id>` instead. If the claim fails because the bead is taken, run `git worktree remove .worktrees/<id>` and choose again. Never use `--force` on a claim that may be live.
4. **Work.** Run `bops` commands from inside the worktree until you remove it in step 7. Put the bead id in commit subjects.
5. **Note your progress.** Run `bops note` at each milestone, **at least once an hour during long turns**, and before you stop. After two hours with no visible activity, a claim counts as abandoned on other machines. A note only reaches them after `bops sync`, so during a long turn run that too:
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
   `land` records evidence. It never merges and never closes. `--verify` confirms the PR is at that commit. The forge is detected from `origin`, so add `--forge github` or `--forge gitlab` only if `--verify` reports that no forge is configured.
7. **Let go.** Run `bops release <id> --reason done`, then remove the worktree with `git worktree remove .worktrees/<id>` from the main checkout.
8. **Before you stop,** run `bops sync`, from the main checkout if you have removed the worktree.

## Giving a bead back

Use `bops release <id> --reason <reason>`, with your explanation in a `bops note` first.

| Reason | When | What happens |
|---|---|---|
| `done` | Work is landed | Stays in progress until its PR merges |
| `blocked` | A dependency stops you | Status becomes blocked |
| `scope` | The bead is wrong or too big | Back to the pool after a cooling-off period |
| `stale` | You are abandoning it | Same as `scope` |
| `human` | A person must decide | Out of the pool until a person removes `needs:human` |

## Closing

Do not close beads yourself. `bops sweep` previews which landed beads have merged PRs, and `bops sweep --dry-run=false` closes them. A person or a scheduled job runs that.

## When something goes wrong

- **`not held by this run`:** you are in the wrong directory, or another run owns the bead. Check `pwd` first.
- **`could not save run state` or `state lock`:** another `bops` process is busy. Wait and retry once.
- **`push failed`:** your work is recorded locally. Run `bops sync`, and report it if that also fails.
- **A reviewer asks for changes after `release --reason done`:** there is no re-entry command yet. Stop and tell the person.
- **`bops sync` lists `stranded_holds`:** those beads were held by a removed worktree. If the work is abandoned, run `bops release <id> --reason stale`.

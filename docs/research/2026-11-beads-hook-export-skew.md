# Beads pre-commit hook commits a stale `issues.jsonl` (bops-7fw)

> **Status as of 2026-11-11, bd 1.3.4 (Homebrew).** Source links are pinned to
> the `v1.3.4` tag ([`7ef2fd0`](https://github.com/gastownhall/beads/tree/7ef2fd05eb0cd3bdbde051f93cfc14ccf270cdde)).
> Upstream `main` has already changed part of this (see "Upstream state");
> re-check before acting on a newer bd.

## Summary

The stale commits are not caused by a stale read inside the hook. Three bd
behaviours combine to produce them:

1. **`bd export` with no `-o` writes to stdout, not the file.**
   ([export.go#L67](https://github.com/gastownhall/beads/blob/7ef2fd05eb0cd3bdbde051f93cfc14ccf270cdde/cmd/bd/export.go#L67))
   In the reported sequence (`bd export`, then `git add .beads/issues.jsonl`),
   the file on disk is not touched. What gets staged is the last
   **auto-export**.
2. **Auto-export after a bd write is throttled to one per `export.interval`
   (default 60 s) and nothing writes the skipped change later.**
   `maybeAutoExport` returns early when the last export is under 60 s old
   ([export_auto.go#L126-L149](https://github.com/gastownhall/beads/blob/7ef2fd05eb0cd3bdbde051f93cfc14ccf270cdde/cmd/bd/export_auto.go#L126-L149)).
   A claim made a second after another write is in Dolt but not in
   `.beads/issues.jsonl` until the next unthrottled write. This is hypothesis (a)
   in the bead, but the cause is the throttle, not a stale Dolt read:
   `bd export -o` itself is always a full, current export.
3. **The pre-commit hook re-exports to the working tree but does not stage the
   result unless `export.git-add` is true (the default is false).**
   `exportJSONLForCommit` runs `bd export -o <beadsDir>/issues.jsonl`
   ([hooks.go#L1566](https://github.com/gastownhall/beads/blob/7ef2fd05eb0cd3bdbde051f93cfc14ccf270cdde/cmd/bd/hooks.go#L1566))
   and calls `git add` only under `export.git-add`
   ([hooks.go#L1580-L1584](https://github.com/gastownhall/beads/blob/7ef2fd05eb0cd3bdbde051f93cfc14ccf270cdde/cmd/bd/hooks.go#L1580-L1584);
   default false at [config.go#L333](https://github.com/gastownhall/beads/blob/7ef2fd05eb0cd3bdbde051f93cfc14ccf270cdde/internal/config/config.go#L333)).
   Git commits the index it already had. The hook's "Exported 43 issues"
   line refers to the working-tree copy, which is why the file shows as
   modified by that one line right after the commit. This is hypothesis (b).

The hook only exports when something under `.beads/` is staged
([hooks.go#L1546](https://github.com/gastownhall/beads/blob/7ef2fd05eb0cd3bdbde051f93cfc14ccf270cdde/cmd/bd/hooks.go#L1546),
[#L1607-L1621](https://github.com/gastownhall/beads/blob/7ef2fd05eb0cd3bdbde051f93cfc14ccf270cdde/cmd/bd/hooks.go#L1607-L1621)).
That check runs git with the hook's `GIT_INDEX_FILE` removed, so it reads
`.git/index`. Under `git commit -a` or `git commit -- <paths>` it can miss what
is being committed, and then no export runs at all.

**Why a hook in a linked worktree writes the main checkout's file:** the
destination is `beads.FindBeadsDir()`. A linked worktree has the tracked
`.beads/` files but no database, so step 3c falls back to the shared
`.beads/` of the main checkout through `git-common-dir`
([beads.go#L914-L922](https://github.com/gastownhall/beads/blob/7ef2fd05eb0cd3bdbde051f93cfc14ccf270cdde/internal/beads/beads.go#L914-L922)).
That is the right place to find the database, but the hook also uses it as
the place to write the export. `gitAddFile` then declines to stage a path
outside the hook's worktree
([export_auto.go#L1002-L1007](https://github.com/gastownhall/beads/blob/7ef2fd05eb0cd3bdbde051f93cfc14ccf270cdde/cmd/bd/export_auto.go#L1002-L1007)),
so the main checkout is left with an unexplained `M .beads/issues.jsonl` even
when `export.git-add` is true. Upstream already has this as
[#6680](https://github.com/gastownhall/beads/issues/6680), fixed on `main` by
[#6717](https://github.com/gastownhall/beads/pull/6717) (`hookJSONLDir`), but
the fix is **not in v1.3.4**: the tag and the fix commit have diverged, and the
tagged `hooks.go` has no `hookJSONLDir`.

## Evidence from this repo

Commit `0cb63b3` ("beads: export ledger after splitting bops-ecs") records
`bops-ecs` as `"status":"open"`, `updated_at 2026-11-11T18:13:09Z`. The next
export commit `2d91670` has it `in_progress` at `18:13:10Z`. The claim came one
second after the previous write, inside the 60 s throttle. `bd export` without
`-o` did not rewrite the file. The hook's export went to the working tree
unstaged. Repo config: `export.auto: true`, `export.git-add` unset (false),
`core.hooksPath=.beads/hooks`, hook marker `BEADS INTEGRATION v1.3.4`.

## Reproduction (scratch repo, bd 1.3.4)

Setup: `git init` in `/tmp/bops-7fw-repro`,
`bd init --non-interactive -p rp --skip-agents` (installs `.beads/hooks` and
sets `core.hooksPath`), `bd config set export.auto true`, `bd create`, commit.
The comparison after each variant is `git show HEAD:.beads/issues.jsonl` vs a
fresh `bd export -o /tmp/fresh.jsonl`.

| Variant | Commands | Result |
|---|---|---|
| A | `bd update <id> --claim`; `bd export` (no `-o`); `git add .beads/issues.jsonl`; `git commit` | **Stale.** The claim was throttled (26 s after the last auto-export), the file did not change, and there was nothing to commit. |
| B (the reported case) | wait 62 s; `bd create` (auto-export fires); `bd update <id> --claim` (throttled); `bd export`; `git add .beads/issues.jsonl`; `git commit` | **Stale.** The hook printed `Exported 2 issues to …/.beads/issues.jsonl`. The commit has the new bead `open` while the ledger has `in_progress`, and afterwards `git diff` shows exactly that one line changed. |
| C | `bd create`; `bd update --claim` (both throttled); `git add .beads/issues.jsonl` plus another file; `git commit` | **Stale.** `.beads/` had nothing staged, so the hook skipped its export and the commit carried no ledger change. |
| D | claim; `bd export -o .beads/issues.jsonl`; `git add`; `git commit` | **Current.** |
| E | claim; `bd export -o .beads/issues.jsonl`; `git add`; `git commit --no-verify` | **Current.** The hook is not needed. |
| E2 | claim; `git add` (stale file); `git commit --no-verify` | **Stale**, as expected. `--no-verify` changes nothing by itself. |
| F | `export.git-add: true`; wait 62 s; create; claim; `git add .beads/issues.jsonl`; `git commit` | **Current.** The hook's `git add` reaches the index that a plain `git commit` uses. |
| G | linked worktree (`git worktree add`); claim from the worktree; `bd export -o .beads/issues.jsonl`; `git add`; `git commit` there | **Current** in the worktree commit, but the hook wrote `Exported 8 issues to /private/tmp/bops-7fw-repro/.beads/issues.jsonl` and the **main checkout** shows ` M .beads/issues.jsonl`. |
| H | `export.git-add: true`; create; claim; `git commit -a` | **Stale.** `BD_DEBUG=1` showed `pre-commit: skipping JSONL export — no staged .beads paths`. |
| I | under the default config: wait 62 s; create; claim; `bd export -o .beads/issues.jsonl && git add .beads/issues.jsonl && git commit`; `git status --short -- .beads/issues.jsonl` | **Current**, and the status check prints nothing. |
| J | B's sequence, then `git status` shows ` M`; `bd export -o .beads/issues.jsonl; git add .beads/issues.jsonl; git commit --amend --no-edit` | Stale, then **current after the amend**. |

Reproduced: **yes**. The scripts are in `/tmp/bops-7fw-scripts/` on the machine
that ran them; they are not kept.

## Safe procedure (verified, variants D, E, I, J)

1. Make the last ledger write you want recorded, such as a claim.
2. `bd export -o .beads/issues.jsonl`. **`-o` is required**: plain
   `bd export` prints to stdout.
3. `git add .beads/issues.jsonl && git commit …`, with no bd writes between
   step 2 and the commit.
4. `git status --short -- .beads/issues.jsonl` must print nothing. If it shows
   ` M`, a write landed after step 2. Repeat step 2, then
   `git add .beads/issues.jsonl && git commit --amend --no-edit`.

In a linked worktree, run step 2 from the worktree root so that `-o` writes
the worktree's copy. On bd 1.3.4 the hook will still rewrite the main
checkout's `.beads/issues.jsonl`. That file is only an export: leave it for
the next export commit there, or discard it. Use a plain `git commit`, not
`-a` or `-- <paths>`, for export commits.

## Local config options (proposed, not applied)

- **`export.git-add: true`** in `.beads/config.yaml` makes the hook stage its
  fresh export, which fixes plain `git commit` in the main checkout (variant F).
  It is **not applied** for three reasons. It does nothing in linked worktrees
  on 1.3.4, where agents commit, because the export goes to the main checkout
  and staging is refused. It does nothing for `git commit -a` or pathspec
  commits (variant H). And it adds `git add .beads/issues.jsonl` to the main
  checkout's index after every unthrottled bd write, so the export can slip
  into unrelated commits (compare upstream
  [#3962](https://github.com/gastownhall/beads/issues/3962)). The owner can
  turn it on if main-checkout export commits are the common case. Revert by
  deleting the key.
- **`export.interval`** set lower, such as `1s`, narrows the throttle window
  but does not close it, and every write pays for a full export. Not proposed.
- **`export.auto: false`** stops the hook exporting at all, so only an explicit
  `bd export -o` changes the file. That makes the procedure above the only
  path, which removes the confusing working-tree rewrite. It is a reasonable
  alternative, but it changes behaviour for anything that reads the file
  between commits, so it is the owner's call.

## Upstream state

- [#6680](https://github.com/gastownhall/beads/issues/6680): worktree hook
  writes the primary checkout's file. Closed; fixed on `main` by #6717 after
  1.3.4.
- [#4080](https://github.com/gastownhall/beads/issues/4080): keep
  `GIT_INDEX_FILE` for pathspec-commit staging. Closed; `main` keeps a
  repo-local `GIT_INDEX_FILE`.
- [#3970](https://github.com/gastownhall/beads/issues/3970): a failed
  auto-export `git add` leaves stale staged JSONL. Closed.
- No existing report covers the **default `export.git-add=false` case**. In
  that case the hook rewrites a staged export and leaves the rewrite unstaged,
  so the commit is stale and the tree dirty, and the throttled auto-export has
  no trailing flush. `main` (`3b82808`, 2026-11-11) still stages only under
  `export.git-add`. Draft below; **not posted**.

### Draft upstream issue (for owner review; do not post without approval)

> **Title:** pre-commit export rewrites a staged `issues.jsonl` without staging it (export.git-add=false), so the commit records a stale export
>
> **Version:** bd 1.3.4 (Homebrew, macOS). The same logic is on `main` at 3b82808.
>
> **Summary.** With `export.auto: true` and the default `export.git-add: false`,
> the pre-commit hook runs `bd export -o .beads/issues.jsonl` whenever a
> `.beads/` path is staged, prints `Exported N issues to …`, and leaves the
> new file unstaged. Git commits the copy already in the index. If a bd write
> happened after that copy was produced (most often a write inside the 60 s
> auto-export throttle, which never gets written later), the commit records a
> stale ledger and the working tree is dirty immediately afterwards. The
> "Exported" message suggests the commit is current.
>
> **Repro.**
> ```sh
> git init -q r && cd r && git config user.email r@example.com && git config user.name r
> bd init --non-interactive -p rp --skip-agents >/dev/null
> bd config set export.auto true
> bd create first -t task >/dev/null; git add .beads; git commit -qm baseline
> sleep 61
> ID=$(bd create second -t task --json | jq -r .id)   # auto-export fires
> bd update "$ID" --claim                               # inside 60 s: throttled
> git add .beads/issues.jsonl && git commit -m x        # hook: "Exported 2 issues"
> git show HEAD:.beads/issues.jsonl | grep "$ID"        # "status":"open"
> bd show "$ID" --json | jq -r '.[0].status'            # in_progress
> git status --short                                    # " M .beads/issues.jsonl"
> ```
>
> **Cause.** `exportJSONLForCommit` (cmd/bd/hooks.go) writes the export
> unconditionally once `preCommitHasStagedBeadsFiles` is true, but stages it
> only under `export.git-add`. Separately, `maybeAutoExport` throttles to
> `export.interval` with no trailing flush, so the staged copy is often older
> than the ledger.
>
> **Suggested fix (any one).**
> 1. When the export path is itself staged (the user has said they are
>    committing it), re-stage the fresh export whatever `export.git-add` is
>    set to, into the commit's `GIT_INDEX_FILE`.
> 2. Or, when not staging, do not rewrite the working-tree file during
>    pre-commit; warn if it differs from the staged blob ("staged
>    issues.jsonl is behind the ledger; run bd export -o … && git add").
> 3. Separately, have a throttled auto-export record a pending flag so that
>    the next bd command or the hook writes it.
>
> Related: #6680 (worktree destination, fixed after 1.3.4), #3970, #4080, #3962.

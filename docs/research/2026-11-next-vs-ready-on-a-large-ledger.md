# bops next against bd ready on a large ledger

bops-r94, 2026-11-23. On 2026-11-18, `bops session-start` in development-collaboration reported a ready_count of 19 while `bd ready` listed 339 beads. This note re-takes both counts on the current build (after bops-yja's `blocking_labels` change), sorts the excluded beads by reason, and judges each reason.

development-collaboration is a private work repository, so this note gives counts only. Bead ids and titles from it went to the owner directly.

## Method

From a development-collaboration checkout on `master`, with `bops` built from beads-ops main at 07f34f6 (bops-onn merged):

```bash
bd ready --json --limit 0                          # the full ready set
bops next --include-unworkable --n 400 --json      # what bops sees today
bops stale --json                                  # claims nobody is working
```

That repository's `.bops.json` (committed 2026-11-19) sets `stale_minutes` to 240 and `blocking_labels` to `["frozen"]`. An earlier version of this note said it had none; that was wrong (bops-lvp).

## Finding 1: bops reads only the first 100 ready beads

`bd ready --limit 0` returned **406** beads. `bops next --include-unworkable --n 400` returned **100** candidates, 18 of them workable.

`intent.Next` runs `bd ready --json` with no `--limit`, and bd caps that output at 100 rows by default. `next`'s ordering step had the same gap: its `bd list --status in_progress` read capped at bd's default of 50 rows (also fixed in bops-na4). Every other bd list call in bops already passes `--limit 0`. So `next`, and the `ready_count` that `session-start` builds on it, silently cover only the first 100 rows of the ready list.

A scratch build that passes `--limit 0` (nothing committed) saw all 406:

| | Current build | Reading the whole list |
|---|---:|---:|
| Candidates | 100 | 406 |
| Workable | 18 | 99 |
| Excluded | 82 | 307 |

Filed as **bops-na4** (bug, discovered-from bops-r94).

## Finding 2: the excluded beads by reason

Reading the whole list, the 307 excluded beads fail on these reasons:

| Reason | Beads | Verdict |
|---|---:|---|
| W01: no acceptance criteria | 291 | Right |
| W01 and epic | 10 | Right |
| epic: agents work leaves, not epics | 5 | Right |
| W15: labelled `frozen` | 1 | Right |

**W01 is right.** The 301 W01 beads have an empty `acceptance_criteria` field. Of those:

- **203** carry a label the repository's own dispatch rules already skip (`spec-blocked` 152, `not-slingable` 30, and others), or are an epic or a decision. W01 adds no loss there.
- **16** state their criteria in the description under an "Acceptance" or "Done when" heading. Their criteria belong in the field, which `/bead-prep` drafts. Teaching W01 to parse descriptions would make lint depend on prose conventions.
- The rest have no stated criteria at all, which is what W01 is for.

**epic and W15 are right.** Agents work leaves, and `frozen` is the label the repository's `.bops.json` lists in `blocking_labels`.

No reason was judged too strict, so no filter-fix bead was filed.

## Finding 3: the filters are too lenient for this repository

Of the 99 workable beads, **30** carry a label that development-collaboration's AGENTS.md tells agents never to take (`spec-blocked` 20, `not-slingable` 7, `needs-spec` 3). One more is of type `decision`, which that file also excludes. That leaves **69** beads an agent there may take.

bops does not know these labels, because the repository's `.bops.json` lists only `frozen`, its protocol's one extra Blocking label, in `blocking_labels`. It leaves out the five labels every dispatch there excludes. The fix is configuration in that repository, not code in bops: `blocking_labels` should hold the five as well as `frozen`. development-collaboration-7g9t makes that change, and has the repository's suite check `blocking_labels` against both lists.

bops has no setting for excluding the `decision` type. That one bead does not justify a new key yet.

## Finding 4: stale claims

`bops stale` listed **7** stale claims, all held by the owner's actor and all still `in_progress`. Four are from this machine and idle since 2026-11-23 01:29–02:35Z. Three are from another machine and idle since 2026-11-20.

It also listed **2 orphans**, claim refs from another machine whose beads are already closed. They were left on 2026-11-19 and 2026-11-20.

None was forced or released. The list went to the owner to decide.

## Summary

| | Count |
|---|---:|
| `bd ready` | 406 |
| `bops next` today | 18 workable of 100 seen |
| `bops next`, whole list | 99 workable of 406 |
| …and also passing the repository's own label rules | 69 |

The 19-versus-339 gap of 2026-11-18 is mostly the 100-row cap (bops-na4), plus missing acceptance criteria on most of the backlog. W01 is right to exclude those beads.

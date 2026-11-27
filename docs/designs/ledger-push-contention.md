# Design: take the ledger push off the claim path

Written 2026-11-27 from the [ledger contention measurements](../research/2026-11-ledger-contention.md) and one extra measurement below.
Status: PROPOSED. Implementation is filed as separate work; nothing here is built yet.
Measured on: bd 1.3.4 (embedded Dolt), bops `648d9a2`, and the Dolt CLI 2.7.7 for the branch measurement.

## Problem

Every `bops claim`, `land` and `release` writes the shared ledger and pushes it, so every one of them pushes the single Dolt branch `main` that `refs/dolt/data` carries on the git remote. With writers on more than one machine those pushes reject each other non-fast-forward, and each retry pulls first. The measurements say what that costs:

1. **The ref race grows with machines, not agents.** One machine of 4 agents had no rejected push; 2 machines x 4 agents had 44% of pushes rejected, 3 x 4 had 60%, 4 x 4 had 68%.
2. **The 60 s command budget fails before the five tries do.** At 8 writers 4 of 189 commands used all five tries, while 7 claims, lands and releases hit the budget. Each retry pulls, and a pull waits for the store.
3. **One Dolt store serializes its agents.** About three quarters of a claim's time at 1 x 4 (15 s, against 3.8 s alone) is waiting for the other local agents. Slow calls turn into `database is locked` errors.
4. **About ten serial git round trips per command.** An uncontended claim makes 10 bd calls and 11 git network round trips; latency multiplies all of the above.
5. **The claim ref was always right.** It kept every claim correct at 16 writers. What degraded was the ledger: 6% of claims at 8 writers, 19% at 12 and 34% at 16 came back `ledger_pending`, and a few `release --reason done` writes were never repaired.

## Constraints

- All ledger writes go through `bd` ([SPEC](../../SPEC.md) non-goals, [ADR 0001](../adr/0001-derived-read-model-not-write-path.md)). bops does not run `dolt` itself.
- The ref decides who holds a bead (SPEC §3.0). But the display is not ignored everywhere: some decisions read it as evidence that a claim **exists**, and they assume a claim's display reaches the remote within seconds:
  - `stale` builds its list of stale claims from the ledger (`bd list --status in_progress`; the ref only supplies the age), so `reap` sees only claims the ledger shows. A held ref older than `stale_minutes` with no claim on the ledger is an **orphan** instead (`intent/stale.go`), which `reap` leaves alone and `gc` deletes with no check for a branch or merge request (`intent/gc.go`).
  - `claim`'s `notClaimable` refuses a bead whose ledger shows an assignee or `run:` label, before and after taking the ref; `claim --force` on a bead with no ref judges staleness from the ledger's `updated_at`.
  - `status` lists ledger claims beside ref claims, and `lint`, `gc` and `sweep` read the `run:` label, assignee, `in_progress` status and `bops:landing` label.
  - Under `lock.mode none` the ledger is the only ownership record.
- People and tools that read raw `bd` (`bd ready`, `bd list`, Beads Viewer) on a bops ledger still need to see, eventually, who holds what.
- `note` already writes the ledger locally and never pushes it (it pushes only the claim ref, to refresh it); its comment rides on the next push. That is the precedent the chosen option extends.

## Options

### Baseline: server mode (MULTI-DEV-PLAN §1, options 2 and 3)

A **central Dolt server** (option 3) removes the shared ref altogether: every clone writes SQL transactions to one server, so there is no push to reject and claims are visible in under a second. It costs a service to run, TLS and credentials, and offline writes. Every bd call becomes a round trip to that server, so finding 4 moves rather than disappears: ten bd calls over a 100 ms link are a second of claim. A **per-machine server** (option 2) may cut the local queueing of finding 3, but leaves the cross-machine race of finding 1 as it is. Neither was measured.

Server mode stays what the [switch condition](../MULTI-DEV-PLAN.md#measured-limits) says it is: the move for more writers than the embedded topology supports. The options below try to raise that ceiling without a service.

### (a) Batch ledger writes behind the lock ref

Split "write the ledger" from "publish the ledger" for the claim. The claim ref is pushed synchronously as now; the claim's display goes to the local store in as few bd calls as possible and is published by the next push from that machine. One Dolt push carries every local commit of every agent on the machine, so the store itself does the batching.

Two independent parts:

- **Fewer bd calls per command.** bd 1.3.4 takes `bd update <id> --assignee <a> --status in_progress --force --add-label run:<r> --remove-label <each stale run: label, bops:hold, bops:landing>` as one call (`--force` as today, so a repair can overwrite another actor's assignee) and one Dolt commit (checked on a scratch ledger), and `bd update <id> --claim --add-label run:<r>` with the same `--remove-label` set likewise. Today's claim display is an update, a label add and a label remove for each stale label, then a comments read (the check that keeps a retry from adding a second `bops:claimed`) and a comment add, then a readback `show`. It becomes one update, the comments read and add, and the readback. Each call removed is one less wait for the store (finding 3).
- **No push from the claim.** Under `lock.mode ref`, `claim` stops pushing the ledger. `land` and `release` (every reason) keep their pushes: the landed record and `bops:landing` are what `sweep` reads on other machines, and a hand-back must show the bead free before other machines claim it. Per bead that is two pushes instead of three, and each of them also carries the claim's display. The claim, the most frequent and most budget-bound command, makes none (findings 1, 2 and 4). It still makes its one pull after taking the ref, so its queueing on the store shrinks but does not go away (finding 3).

What it costs: the remote ledger shows a claim at that machine's next ledger push (its next land, release, reap, sweep, or `bops sync` without `--no-push`; `session-start` never pushes) instead of seconds later. Only `next` and the claim commands read the refs alone; `stale`, `reap`, `status`, raw `bd ready` and the other readers listed under Constraints see the claim late or not at all. Four things follow, and each needs handling before the claim stops pushing:

- **Orphan deletion.** On another machine a claim with an unpublished display is not a stale claim but, once its ref is older than `stale_minutes` (no `note` or `renew` refreshed its `at`), an orphan, and `gc` deletes orphan refs with no evidence check. `gc`'s closed-bead rule has the same hole: it deletes a held ref older than `stale_minutes` whose bead reads closed, and its keep-window for "a claim whose ledger write may be pending" assumes that write is seconds old, so a bead reopened and claimed on one machine and not yet pushed reads closed elsewhere. `gc` must apply `reap`'s work-evidence checks before both deletions, with `reap`'s rules: a forge search that fails or does not answer counts as evidence, the refs must have been fetched, and the check is repeated under the bead lock. From another machine only the remote branch and the forge are visible (not the claiming machine's worktrees or unpublished comments), so this gives parity with what `reap` does to an idle claim today, not more: an agent that pushed no branch and neither noted nor renewed within `stale_minutes` still loses its claim, as it would to `reap` now.
- **Ghost displays.** A claim whose holder dies before its machine pushes, and which `claim --force` takes over elsewhere, arrives on the remote at that machine's next push as an `in_progress` bead showing a dead run; the other machine never saw that `run:` label, so its writes did not remove it. Existing code already covers most of this: with no ref for the bead, `stale` lists the display as a claim with `source: ledger`, aged from the ledger's activity (expected to be the original claim's `updated_at`, though the takeover's release also wrote that row, so step 1 confirms which value and which newest `bops:` comment survive the merge), and `reap` releases it on its evidence rules; with a live ref for another run, that run's next display write removes every other `run:` label. What is needed is a test that both hold for a display that arrives late, not a new writer.
- **Pull conflicts.** The unpushed window grows from seconds to a whole claim-to-land session, so any edit to the same bead row from another machine in that window meets the local display at the next pull. MULTI-DEV-PLAN §6 records last-write-wins for `assignee` and a stop for a person on some same-cell edits (notes). What `bd dolt pull` does for each case that can occur (a takeover's assignee, status and labels; a person's edit of the same bead; a `sweep` close) must be measured before building, and any stop must be resolved from the ref or reported. Today a failed pull on the claim path is only a warning, and the `notClaimable` re-check after it is skipped; it must become an event that `status` and `doctor` report.
- **Unpublished claims.** A loop cancelled after claiming, or a machine taken offline, leaves displays no other machine sees until it pushes again. `status` and `doctor` must report when the local ledger has commits the remote lacks, or has diverged from it.

A cost that stays: raw `bd ready` on another machine shows a held bead as ready until the holder's machine pushes, now for the length of the work rather than seconds, and a raw `bd update --claim` there is settled last-write-wins with nobody told (MULTI-DEV-PLAN §2). On a bops ledger claims already go through `bops`, and `bops next` reads the refs, so this harms only people claiming with raw `bd` on another machine. That is the same population (c) is rejected for below, for a shorter window and only on other machines; whether it is a supported population is a lead's call.

### (b) Per-machine ledger branches merged by sync

Each machine writes its own Dolt branch and pushes only that; `bops sync` merges the branches into `main`. This is the Dolt form of grite's per-writer event log merged on read ([grite](https://github.com/neul-labs/grite), [paper](https://arxiv.org/html/2606.19616)).

The race does go away at the push. Measured with the Dolt CLI 2.7.7 against a local `git+file` remote, 4 clones pushing at once, 10 commits each:

| Pushes to | Push attempts | Rejected non-fast-forward | Commits that gave up after 8 tries | Wall time |
|---|---|---|---|---|
| one branch, `main` (two runs) | 101 / 96 | 68 (67%) / 62 (65%) | 7 / 6 | 61 s / 64 s |
| one branch per clone | 40 | 0 | 0 | 34 s |

All branches live under the one git ref `refs/dolt/data`, and Dolt settled its updates without any rejection reaching the client. One valid per-branch run; a clone per "machine" on one Mac.

It does not fit bops today:

- bd 1.3.4 cannot work on a branch other than `main`: `bd branch` creates one, but nothing checks one out, and `bd dolt push` pushes the current branch. bops would have to run `dolt` itself, which ADR 0001 rules out, or wait for bd.
- The merge into `main` is a push to the shared branch again, at sync frequency. Until it runs, `main` (what every other machine and raw `bd` reads) shows none of a machine's writes: landed records and releases included, not only claims. Every lag problem of (a) applies to every command, not just the claim.
- Two machines' branches that edit the same cell conflict at the merge, and `bd sync` stops for a person on those (exit 2). Today that window is one push; here it is a whole session.
- It does nothing for finding 3: the agents of one machine still share one store.

Worth asking bd for if (a) is not enough, not worth building around bd.

### (c) Keep only claim state in refs

Stop writing claim state to the ledger: no assignee, no `run:` label, no `bops:claimed` comment, no `bops:landing` label. Claim is a `bd show` and a ref push; release and land write only content (notes, the landed record, a blocked status, `bops:needs-human`).

It is the fastest claim possible. But every reader listed under Constraints would have to move to the refs: `sweep` would enumerate landing refs instead of `bd list --status in_progress`, `stale` and `gc` would need a new definition of an orphan, and `lint`, `status` and `claim`'s pre-checks would change. Raw `bd ready` and Beads Viewer would show held beads as open and unassigned for good, on every machine including the claiming one, so people on raw `bd` pick up beads agents hold, and the `bops:claimed` history that hand-offs read disappears.

## Decision

**Option (a): the claim stops pushing; its display rides on the machine's next push.** The claim, the hottest command and the one that runs into the budget, loses its push and its retries; per bead two pushes are left instead of three; and bops needs nothing from bd 1.3.4 that it lacks. (b) is blocked on bd and lags every command, not just the claim; (c) rewrites every reader to get a claim path (a) mostly gets anyway. `land` and `release` keep pushing, which keeps `sweep`, hand-backs and the done path working as today. Server mode stays the switch for writer counts past what the re-measured limits support.

## How it relates to `ledger_pending` and `release_pending`

Today (`intent/ownership.go`, `internal/runstate`, state.json currently version 6):

- `ledger_pending` on a hold means the claim's display write or its push failed; `bops sync` from the same run re-runs the display from the ref and pushes.
- `release_pending` is used only by `reap`'s releases: the release is written to the local ledger, and the claim ref stays until a push carries it, so no machine sees the ref gone while the remote still shows the claim. A plain `bops release` that hands a bead back pushes and deletes the ref either way: a failed push alone is carried by the next push, but if its local write fails, nothing journals the owed display.
- `release --reason done` moves the ref to `landing` and then drops the hold, so if its display write fails nothing repairs it (filed as a bug from the contention measurement).

Under (a):

- A claim whose local display write completes is logged `written` and answers as today; an unpublished claim is the normal case and gets no per-claim flag. `ledger_pending` narrows to "the local write did not complete". The switch-condition count of `pending` against `written` in MULTI-DEV-PLAN §1 then measures only local failures, not push contention, which moves to `land` and `release`; the condition is restated over their push events (step 4).
- Whether the ledger is published needs no new bops state: any push carries everything, and Dolt knows whether local `main` is ahead of the remote-tracking branch. bd 1.3.4 has no ahead/behind command (`bd vc status` gives branch and commit only), so the report goes through `bd sql` over Dolt's system tables, and it is only as fresh as the last pull or fetch; step 4 decides whether `doctor` spends a fetch on it.
- `release_pending` and every release path are unchanged. The done-display bug and the plain hand-back gap above are independent of this design; their fix (likely a given hold entry with `ledger_pending` that `sync` finishes, as `runstate.Entry.Given()` already supports) does not wait on it, nor it on them.
- `lock.mode none` keeps its push: there the ledger is the only record of a claim.

## Risks

- **A machine that never pushes again** keeps its claims' display local. Its refs still decide; other machines recover the beads with `claim --force` once the refs are stale, or with `gc` once they are orphans and show no work evidence.
- **Pull conflicts** may stop a machine's publishing (above). Measured first; if a likely case stops for a person and cannot be resolved from the ref, (a) is not built and server mode is the next step.
- **The estimate is not a measurement.** Removing one of three pushes per bead, the claim's, should cut rejections and budget failures, but by how much is for the re-measurement to say. If 2 x 4 on a 400 ms link still leaves claims slow, the next step is server mode, not (b).

## Implementation order

Filed as separate work:

1. Measure `bd dolt pull` on each same-row edit an unpushed claim display can meet (go/no-go for step 4).
2. Write each command's display in one `bd update` (independent).
3. `gc` applies `reap`'s work-evidence checks before deleting an orphan ref or a closed bead's ref (independent; it also protects today's `ledger_pending` claims).
4. Stop pushing from `claim`: a failed claim-path pull becomes a reported event; `land`, `release` and `sweep` log each ledger push's outcome and tries to `events.jsonl`, since the claim's `pending` count no longer sees push contention and the switch condition in MULTI-DEV-PLAN §1 must be restated over these; `status` and `doctor` report an unpublished or diverged local ledger, and tests show a late-arriving display is reaped or cleaned as described above (needs 1 and 3).
5. Re-measure with the contention harness and revise the limits in MULTI-DEV-PLAN §1 and PROCESS §8 (needs 2 and 4).

## Method for the branch measurement

A bare git repository with one commit on `main`, a Dolt database pushed to it as `git+file://<path>`, four `dolt clone`s with a local `user.name`. Each clone runs this at the same time, once on `main` and once on its own branch `m<N>` (made with `dolt checkout -b` after a pull):

```sh
# usage: run.sh <clone> <branch> <commits> <log>
cd "$1"; b=$2; n=$3; log=$4
for i in $(seq 1 $n); do
  dolt sql -q "insert into t values ('$b-$(basename $1)-$i', $i)" >/dev/null 2>&1
  dolt commit -am "w $i" >/dev/null 2>&1
  for try in 1 2 3 4 5 6 7 8; do
    out=$(dolt push origin $b 2>&1) && { echo "ok $try" >>$log; break; }
    echo "rej $(echo "$out" | tr '\r' '\n' | grep -v Uploading | tr '\n' ' ')" >>$log
    [ "$b" = main ] && dolt pull origin main >/dev/null 2>&1
  done
done
```

Every `rej` line on `main` was `! [rejected] main -> main (non-fast-forward)`. Afterward a fresh clone read 10 rows of each clone's own on its branch. Limits: the Dolt CLI, not bd's embedded Dolt; a local remote; one host; one valid per-branch run.

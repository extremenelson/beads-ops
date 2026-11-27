# Ledger contention: several agents per machine, several machines, one remote

> **Measured 2026-11-27** on one Mac (Darwin 27.2, Apple silicon, 18 cores),
> bd 1.3.4 (Homebrew), bops built from `648d9a2` (main), embedded Dolt, a
> local bare git repository as the remote. Every "machine" below is a separate
> clone with its own Dolt store and bops state **on that one Mac**. No real
> second machine and no real network were in the loop. Re-measure on a newer
> bd or after a change to how bops pushes the ledger.

## Question

[MULTI-DEV-PLAN §1](../MULTI-DEV-PLAN.md#1-the-decision) assumed no
concurrent writers on one machine and put the move to a central server at
"more than ~8 active writers". `/bead-loop` runs up to 4 agents on one
machine against one embedded Dolt store, and every claim, land and release
pulls and pushes the single `refs/dolt/data` ref. How do claim latency, ledger
push retries and failures, and database-lock errors behave at 1, 2, 3 and 4
machines of 4 agents each? What limits do the numbers support?

## Answer

- **One machine, 4 agents: works, but every bd call queues.** On a local
  remote, in 32 loops no command failed, no ledger push was rejected and bd
  returned no lock error. One store runs one bd call at a time, so claim p50
  went from 3.8 s (one agent) to 12 s and p95 to 45 s. With 400 ms added to
  every git network call, the same 4 agents saw 4 `database is locked by
  another dolt process` errors in 503 bd calls, and 1 claim in 20 failed on
  one (cleanly: before taking the claim ref, so the bead stayed free).
- **Across machines the shared Dolt ref is the bottleneck.** At 2 machines x
  4 agents (8 writers) 44% of `bd dolt push` attempts were rejected
  non-fast-forward; at 3 x 4, 60%; at 4 x 4, 68%. bops's five tries absorb
  most of it at 8 writers, but commands run into bops's 60 s command budget:
  claim p95 is the budget from 8 writers up.
- **No claim was lost or taken twice**, at any size: the claim ref decides
  ownership, and a claim whose ledger push did not finish answers `claimed`
  with `ledger_pending`. What degrades is the ledger: 6% of claims at 8
  writers, 19% at 12 and 34% at 16 came back `ledger_pending`.
- **Almost every pending write reached the remote later**, carried by the
  next push or `bops sync`, except one case: a `release --reason done` whose
  bd writes the budget stopped leaves the ledger showing the bead held, and
  nothing repairs it (1 of 64 releases at 2 x 4, 2 of 80 at 4 x 4, 1 of
  19 at 1 x 4 with latency and 5 of 39 at 2 x 4 with latency). Filed as a bug.
- **Latency multiplies all of it.** With 400 ms per git network call, 2 x 4
  nearly doubled claim p50 (18 s to 35 s), left 25% of claims
  `ledger_pending`, and raised lock errors from 6 to 22.

**Supported limits (from these numbers):** up to **4 agents per machine**
(more were not measured), and up to **8 concurrent writers in total on a
fast remote**, expecting claims of tens of seconds and some `ledger_pending`
that a later push or `bops sync` finishes. On a hosted remote with real
latency the numbers support **one machine of 4 agents**, plus occasional
single sessions elsewhere: at 8 writers with 400 ms of latency a quarter of
claims came back `ledger_pending`, past the switch condition in
MULTI-DEV-PLAN §1. Above 8
writers embedded Dolt on a git remote is past its limit on any remote. The
36 writers of 3 developers x 3 machines x 4 agents is far beyond what was
measured to work. What a real network does to two real machines is **not
measured**; the script below runs it.

## Results

Each agent loops over beads of its own: `bops claim`, `bops note`,
`bops land --forge none` (no `--verify`) and `bops release --reason done`
(or `abandoned` when the land failed), then `bops sync` when its loop ends.
"Writers" is machines x agents.

| Run (machines x agents x loops, delay) | Writers | Claim p50 | p95 | max | Claims failed | Claims `ledger_pending` | Lands not published at once | Releases `ledger_pending` |
|---|---|---|---|---|---|---|---|---|
| 1 x 1 x 8, 0 ms (baseline) | 1 | 3.8 s | 3.9 s | 3.9 s | 0/8 | 0/8 | 0/8 | 0/8 |
| 1 x 4 x 8, 0 ms | 4 | 12.0 s | 44.9 s | 45.4 s | 0/32 | 0/32 | 0/32 | 0/32 |
| 1 x 4 x 5, 400 ms | 4 | 24.0 s | 61.4 s | 65.0 s | 1/20 | 2/20 | 1/19 | 1/19 |
| **2 x 4 x 8, 0 ms** | **8** | **18.2 s** | **60.0 s** | **67.0 s** | **0/64** | **4/64** | **3/64** | **1/64** |
| 2 x 4 x 5, 400 ms | 8 | 35.1 s | 65.0 s | 68.5 s | 1/40 | 10/40 | 7/39 | 11/39 |
| 3 x 4 x 6, 0 ms | 12 | 27.3 s | 65.0 s | 69.6 s | 0/72 | 14/72 | 5/72 | 9/72 |
| 4 x 4 x 5, 0 ms | 16 | 35.2 s | 65.1 s | 69.6 s | 0/80 | 27/80 | 12/80 | 20/80 |

A claim past 60 s is the budget plus bops's own cleanup. Both failed claims
were a `bd show` that hit `database is locked by another dolt process`, was
retried once by bops and timed out at the budget, before the claim ref was
written: the bead stayed free, and an agent would claim it again. "Lands not
published at once" are `bops land` exits of 1: all but two "landed locally;
push failed" after five rejected pushes or the budget (a later push carried
the record), and two (one at 4 x 4, one at 1 x 4 with latency) stopped by
the budget before the record was written, after which the harness released
the bead `abandoned`. Notes hit the budget twice: 1 of 80 at 4 x 4 and 1 of
39 at 2 x 4 with latency. `ref_pending` came up once (a release at
4 x 4).

### Ledger pushes

`bd dolt push` calls by claim, land and release together. A command's
retries are its pushes minus one. "Five tries" is a command that used all of
bops's tries; "gave up" is one whose last push failed, either five
rejections or the budget stopping it.

| Run | Pushes | Rejected non-fast-forward | Commands retried | Five tries | Gave up |
|---|---|---|---|---|---|
| 1 x 4 x 8, 0 ms | 96 | 0 (0%) | 0/96 | 0 | 0 |
| 1 x 4 x 5, 400 ms | 55 | 0 (0%) | 0/55 | 0 | 1 (a lock error) |
| 2 x 4 x 8, 0 ms | 335 | 148 (44%) | 103/189 (54%) | 4 | 4 (2%) |
| 2 x 4 x 5, 400 ms | 179 | 85 (47%) | 46/105 (44%) | 3 | 14 (13%) |
| 3 x 4 x 6, 0 ms | 496 | 300 (60%) | 152/213 (71%) | 20 | 24 (11%) |
| 4 x 4 x 5, 0 ms | 600 | 410 (68%) | 182/230 (79%) | 33 | 45 (20%) |

Every rejected push was `! [rejected] main -> main (non-fast-forward)`: the
remote had moved. On one machine no push was ever rejected, because the
store serializes a pull and its push against the other local agents.

### Database-lock errors on one store

bd 1.3.4 mostly waits for its store rather than failing, so most contention
shows as wait: at 1 x 4 single `bd show` and `bd label add` calls took up to
tens of seconds, against well under a second alone. But it does fail
sometimes, with `failed to open database: ... the database is locked by
another dolt process`, exit 1:

| Run | bd calls | Lock errors | Took |
|---|---|---|---|
| 1 x 1 and 1 x 4, 0 ms | 1,055 | 0 | |
| 1 x 4 x 5, 400 ms | 503 | 4 | 2 to 59 s |
| 2 x 4 x 8, 0 ms | 1,978 | 6 | 0.3 to 35 s |
| 3 x 4 x 6, 0 ms | 2,482 | 13 | 0.3 to 42 s |
| 2 x 4 x 5, 400 ms | 1,154 | 22 | 0.2 to 57 s |
| 4 x 4 x 5, 0 ms | 2,834 | 22 | 0.2 to 59 s |

They hit every kind of call (pull, push, show, update, label, comments,
heartbeat), some at once and some after waiting most of the budget. bops
retries a lock error once after 5 s; two claims still failed at the budget
(above). Lock errors grow with how long
bd calls hold the store, so with machines and with latency, not only with
agents on the machine.

### Where a claim's time goes

Mean seconds per claim; "other bd" is every bd call except pull and push
(update, label, comments, show, remote list), most of it waiting for the
store. The rest is bops itself and its claim-ref git calls.

| Run | Claim | in `bd dolt pull` | in `bd dolt push` | other bd |
|---|---|---|---|---|
| 1 x 1, 0 ms | 3.8 | 0.5 | 1.1 | 2.0 |
| 1 x 4, 0 ms | 14.8 | 1.4 | 3.6 | 9.5 |
| 1 x 4, 400 ms | 28.4 | 3.5 | 7.6 | 16.4 |
| 2 x 4, 0 ms | 22.1 | 3.8 | 3.8 | 13.5 |
| 3 x 4, 0 ms | 30.9 | 7.8 | 5.4 | 15.4 |
| 4 x 4, 0 ms | 39.5 | 10.6 | 8.0 | 17.7 |
| 2 x 4, 400 ms | 38.2 | 7.4 | 6.7 | 21.4 |

One uncontended claim makes 10 bd calls and 11 git network round trips
(the create-only ref push, plus Dolt's own fetches, `ls-remote` and pushes
inside `bd dolt pull` and `bd dolt push`); land makes 9 and release 11, all
in sequence. On a remote where each round trip costs a second (an SSH
handshake to a hosted forge), an uncontended claim alone would take 10 s or
more. That was not measured.

## For the push-contention design

The design that answers this section: [ledger push contention](../designs/ledger-push-contention.md).
Its go/no-go measurement is [Same-row pulls on an unpushed claim display](#same-row-pulls-on-an-unpushed-claim-display-2026-11-27) below.

What a structural fix has to beat, from the numbers above:

1. **The ref push race scales with machines, not agents.** One machine of 4
   agents had no rejected push; a second machine took the rejection rate to
   44%, and each further one added 8 to 16 points. A design that leaves one
   shared ref written by every command keeps this curve.
2. **The 60 s budget, not the five tries, is what fails first.** At 8
   writers 4 of 189 commands used all five tries, while 7 claims, lands and
   releases hit the budget. Retries inside a contended store are slow
   because each one pulls, and a pull waits for the store like any other
   call.
3. **One machine's store serializes its agents, and sometimes fails them.**
   About three quarters of a claim's time at 1 x 4 (15 s, against 3.8 s
   alone) is waiting for the other local agents, independent of the network, and slow calls turn into `database is locked`
   errors that cost a retry or a claim. Fewer bd calls per command (batching
   the label, comment and update into one commit) cuts this directly; a
   per-machine Dolt server (option 2 of the plan) was not measured.
4. **Ownership never broke.** The claim ref kept every claim correct at 16
   writers. Leaning on it further (the ledger as a lagging display) is
   sound so long as every pending display write is retried: today a
   `release --reason done` one is not.
5. **Latency is the multiplier.** Each command makes ~10 serial git round
   trips; anything that removes round trips from the claim path helps at
   every size.

## Method

[`2026-11-ledger-contention.sh`](2026-11-ledger-contention.sh) reproduces it.
On one host:

```sh
bash docs/research/2026-11-ledger-contention.sh   # usage
bash docs/research/2026-11-ledger-contention.sh local /tmp/lc-2x4 2 4 8
MEASURE_DELAY_MS=400 bash docs/research/2026-11-ledger-contention.sh local /tmp/lc-2x4-d400 2 4 5
```

`local <workdir> <machines> <agents> <loops>` builds everything inside a new
work directory (it refuses one inside a git work tree or holding files it did
not make): a bare git repository as the remote, a seed clone that runs
`bd init` and creates machines x agents x loops beads titled
`contention NNNN` and pushes them, then one clone per machine (`git clone`
and `bd bootstrap`, so its own Dolt store). `XDG_STATE_HOME`,
`XDG_CONFIG_HOME` and `TMPDIR` point inside the work directory (one state
directory per machine) and every `BEADS_*`, `BD_*` and `BOPS_*` variable is
unset, so the run never touches the real `~/.local/state/bops` or a
configured bd server. (bd and Dolt still queue their usage events under
`~/.beads` and `~/.dolt`.) Interrupting the script stops its agents and their bops
commands; a bd call already started may still finish. Every agent of a machine runs in that machine's
checkout with its own `BOPS_RUN`, as `/bead-loop` claims from the main
checkout; each machine has its own `BOPS_MACHINE` and `BEADS_ACTOR`. All
agents start at once; each works its own beads in turn, then runs
`bops sync`, as a session end would.

Wrappers named `bd` and `git` sit first on `PATH`. The `bd` wrapper runs bd
as a child, passes on the SIGTERM bops sends when a command's budget runs
out, and logs every call with the bops command that made it, its time, exit,
and whether its output carried a database-lock error or a rejected push. The
`git` wrapper counts fetch, push and `ls-remote` (bops's claim-ref traffic
and the git calls Dolt makes inside `bd dolt pull` and `bd dolt push`) and,
with `MEASURE_DELAY_MS`, sleeps that long before each one, then execs git.
Because Dolt runs git while bd holds the store, the delay lands inside the
store lock as a slow link would. It is a fixed delay per call, not bandwidth
or jitter.

After the run, a fresh clone checks every claimed bead for its note comment
(when `note` reported `noted`), its landed record (when `land` reported
`landed`) and its `bops:released` comment (when `release` reported
`released`). Results: `<workdir>/m<N>/results.tsv` (one row per bops
command), `<workdir>/calls.tsv` (one row per bd call and git network call),
`<workdir>/verify.tsv`, each command's JSON under `<workdir>/m<N>/runs/`, and
setup output in `<workdir>/setup.log`.
`report <workdir>...` prints the tables.

A first pass with an earlier version of the wrapper gave the same latencies
and rejection rates within a few points, but its wrapper did not pass on
bops's SIGTERM: a bd call the budget stopped kept running unlogged, so that
pass reported no lock errors. Only the second pass's numbers are above.

### Two real machines

The run that matters most here, two real machines over a real network, is
the one this measurement could not make. To make it:

1. Create an **empty scratch repository** both machines can push to (a
   private throwaway repository on the forge, or a bare repository on one
   machine reached over SSH from the other). Never a real project: the run
   writes `refs/dolt/data` and `refs/bops/claims/*`. `seed` refuses a
   repository with any ref, and `run` refuses a ledger `seed` did not make.
2. On one machine: `bash 2026-11-ledger-contention.sh seed /tmp/lc <url> 40`
   (machines x agents x loops beads; 2 x 4 x 5 = 40).
3. Pick a start a minute or two ahead, `start=$(( $(date +%s) + 120 ))`, and
   on each machine, with the same agents, loops and start:
   - machine A: `bash 2026-11-ledger-contention.sh run /tmp/lc <url> 0 1 4 5 $start`
   - machine B: `bash 2026-11-ledger-contention.sh run /tmp/lc <url> 1 1 4 5 $start`

   The third argument numbers the machine, so each works its own beads.
   Clocks must agree to a second or so (NTP).
4. When both have finished, copy machine B's work directory to machine A
   (say to `/tmp/lc-b`), then on A run
   `bash 2026-11-ledger-contention.sh verify /tmp/lc <url> /tmp/lc-b` and
   `bash 2026-11-ledger-contention.sh report /tmp/lc /tmp/lc-b`.

A machine can also stand in for several: `run ... 0 2 4 5 $start` runs
machines 0 and 1 on one host.

## Limits

- **No real network, no real second machine.** The remote was a local file
  path; every "machine" shared one Mac's CPU and disk. Real remotes are
  slower; the 400 ms runs show the direction, not the size, of the effect. A
  real two-machine run needs the procedure above.
- **One host for up to 16 agents.** At 4 x 4, 16 agents and their bd and git
  processes ran on 18 cores; some of the latency there is CPU, not ledger.
  The wrappers add a shell and two `perl` starts per bd call, about 0.3 to 0.5 s of a 3.8 s uncontended claim.
- **Small samples:** 20 to 80 loops per run, one run per shape. Rates near
  5% are uncertain by several points; the 0 lock errors at 1 x 4 without
  delay do not mean they cannot happen there.
- **A stopped bd may outlive its budget.** bops gives bd 5 s after SIGTERM
  and then kills; the wrapper passes on the SIGTERM, but the kill lands on
  the wrapper, not bd. A bd that ignores SIGTERM for 5 s then keeps running
  unlogged, holding the store (its lock errors count against other calls)
  and possibly finishing a push bops already reported pending. How often
  that happened is not known; bd mostly exited on SIGTERM in these runs.
- **Your git config applies.** The harness does not isolate global git
  config (credentials, hooks, `url.*.insteadOf`), so a real-remote run uses
  your credentials and your hooks.
- **Embedded mode only.** Server mode (a per-machine `dolt sql-server` or a
  central one) was not measured; that it removes the ref race is an
  expectation from its design, not a measurement.
- **Each agent works its own beads.** Two agents racing for one bead
  (`claim --next` on a shared queue) were not measured; the claim ref decides
  those, and earlier tests cover it.
- **Not a full agent.** Real agents spend minutes between claim and land,
  so their commands overlap less than this loop's back-to-back ones; the
  ledger work per bead is the same, but the rate is far higher here. The
  numbers are a stress bound per writer, not a forecast of a working day.
- bd 1.3.4 and bops `648d9a2` only.

## Same-row pulls on an unpushed claim display (2026-11-27)

> **Measured 2026-11-27** on the same Mac, bd 1.3.4 (Homebrew, embedded
> Dolt), a local bare git repository as the remote, two clones of it. The
> Dolt CLI 2.7.7 only read copies of a store. The first eight cases ran four
> times with the same outcome in every row; `takeover-multi`, `park`,
> `delete` and `delete-today` were added after review and ran once or twice.

### Question

The [push-contention design](../designs/ledger-push-contention.md) stops
`claim` from pushing its ledger display, so the display stays unpushed for a
whole claim-to-land session. Another machine that edits the same bead row in
that window meets it at the claiming machine's next `bd dolt pull`. What does
that pull do for each edit that can happen: which cell values survive, which
side wins, does it stop for a person, and does the push after it succeed?

### Answer

- **No edit of a live bead stopped.** A takeover, a person's title,
  priority or notes edit, parking it `blocked`, label changes, a `sweep`
  close, a comment and an abandoned takeover: in each A's `bd dolt pull`
  exited 0 and the `bd dolt push` after it exited 0. A claim display writes
  `assignee`, `status`, `started_at`, `updated_at`, labels and a comment,
  never `notes`, so the same-cell `notes` stop in
  [MULTI-DEV-PLAN §6](../MULTI-DEV-PLAN.md#6-conflicts-and-recovery)
  cannot come from it. (`note`'s `bd heartbeat` writes `heartbeat_at` to the
  `leases` table, which Dolt ignores, so it is never pushed or merged.)
- **Deleting the held bead stops the pull, and the design makes that more
  likely.** A person's `bd delete` of the bead meets the claim display's row
  change as a modify/delete conflict: `bd dolt pull` exits 1 ("merge
  conflicts in issues require operator resolution; merge aborted"), and
  every later pull and push from that clone fails the same way, so the
  machine publishes nothing, for any bead, until a person resolves it.
  Today's flow can stop the same way (`delete-today`: the display pushed at
  once, then the holder writes the row again before any pull reaches its
  clone), but today any pull in between (every `claim`, `sweep`, `reap`,
  `sync` and `session-start` pulls) fast-forwards the delete into the
  holder's clone instead, and only that bead's `land` or `release` fails
  (not measured). Under the design the display stays unpushed, so the first
  such pull stops. `--strategy theirs` kept the delete in `delete-today` but
  failed with an error in `delete`; `--strategy ours` brought the bead back,
  without its labels and comments in `delete-today` and without the seeded
  label in `delete`; both left tables uncommitted, so a person resolving
  with a strategy meets the commit gap below too. No resolution can be read
  from the ref: the ref says the bead is held, the person meant to delete
  it.
- **bd settles the issue row field by field.** A field only one side changed
  takes that side's value. A field both sides changed is settled
  last-write-wins by time: the side with the newer `updated_at` wins, either
  way round (in `takeover-first` the claim display was newer and won). bd
  says which fields it settled in a `Notice: auto-merged issue <id>; ...`
  line on the pull. Labels and comments are unioned, and a label removed on
  one side stays removed.
- **But bd 1.3.4 leaves the union uncommitted.** When the issue row needed
  settling **and** the other side had added labels or comments (the
  takeover cases; the person and close cases settled row fields too, but B
  added no labels or comments there, and nothing was left), the other side's new labels and
  comments were in A's merged view but only in its **working set**: the
  merge commit, the one A's push publishes, does not have them. So after A's
  push the remote shows B's takeover labels and comments as gone, and B's own
  next pull removes them from B's ledger too. They come back only when A
  next commits (a pull that merges something new did it in every run, and
  in one hand check so did a bd write to another bead) and pushes that. A store lost before then loses them for good. `bd vc status`
  does not show the uncommitted tables (the Dolt CLI's `dolt status` on a
  copy of the store does), and `bd sql` does not run in embedded mode.
- **`bd dolt commit` straight after the pull closes the gap.** With it, the
  remote after A's push showed exactly A's merged view in every case that did not stop. When
  the pull left nothing uncommitted it printed `Nothing to commit.` and
  exited 0, so it is safe to run before every push. Before every push, not
  only after bops's own pulls: on a machine of several agents, another
  agent's first push try (which does not pull) can land between one agent's
  pull and its commit, and a budget SIGTERM can stop a command between the
  two.
- **The abandoned takeover leaves a mixed row.** After A's pull the bead
  shows `in_progress` (only A changed `status`: B's release put it back to
  the base value `open`), no assignee (B's release was newer), labels
  `run:<A's run>` and `bops:hold`, `updated_at` from **B's release**, and
  B's `bops:released` as the newest `bops:` comment. So `stale` lists it
  (it is `in_progress`) and ages it from the takeover's release, not from the
  original claim, and `notClaimable` refuses it until `reap` releases it.

### Results

Clone A wrote a claim display (one `bd update --assignee devA --status
in_progress --force --add-label run:runA`, then a `bops:claimed` comment) and
did not push; 1.2 s later clone B made the edit and pushed it. "A's merged
view" is what A's ledger shows after its pull; "remote" is what a pull on B
shows after A's push. Every pull and push below exited 0 in every run, except in the two delete
cases.

| Case: B's edit | bd settled last-write-wins | A's merged view | Remote after A's push, no commit | Remote after A's push, with `bd dolt commit` |
|---|---|---|---|---|
| control: B edits another bead | nothing | A's display | as A's view | as A's view |
| takeover: `--assignee devB --status in_progress --force --add-label run:runB`, `bops:claimed` comment | assignee, updated_at, started_at (B newer) | assignee devB, `in_progress`, `run:runA` and `run:runB`, both comments | assignee devB, **`run:runA` only, A's comment only** | as A's view |
| person: `--title`, `--priority 1`, `--notes` | updated_at | A's display plus B's title, priority and notes | as A's view | as A's view |
| labels: add `area:new`, remove `area:seed` | nothing | `area:new`, `run:runA`; `area:seed` gone | as A's view | as A's view |
| close: `bd close --reason "merged via ..."`, as sweep does | status, updated_at, closed_at, close_reason (B newer) | `closed`, but assignee devA and `run:runA` stay | as A's view | as A's view |
| comment: a person's comment | nothing | both comments | as A's view | as A's view |
| takeover, then release `--reason abandoned` (assignee cleared, status open, `run:runB` removed, `bops:hold` added, `bops:released` comment) | assignee, updated_at, started_at (B newer) | `in_progress`, no assignee, `run:runA` and `bops:hold`, 3 comments, newest `bops:released`, updated_at of B's release | `in_progress`, no assignee, **`run:runA` only, A's comment only**, updated_at of B's release | as A's view |
| takeover-first: B's takeover pushed before A writes its display | assignee, updated_at, started_at (**A newer**) | assignee devA, `run:runA` and `run:runB`, both comments | assignee devA, **`run:runA` only, A's comment only** | as A's view |
| takeover-multi: as takeover, but both sides write today's display (`bd update --claim`, then `bd label add` and the comment; B sees the bead open, as a real `claim --force` there would), and A a `bd heartbeat` as `note` does | assignee, updated_at, started_at (B newer) | as takeover | as takeover | as A's view |
| park: `bd update --status blocked` | status, updated_at (B newer) | assignee devA, `blocked`, `run:runA` | as A's view | as A's view |
| delete: `bd delete --force` | **stops: exit 1, merge aborted; A's push then fails, and so does every later pull and push** | A's display, unmerged | the bead stays deleted | stops the same way |
| delete-today: A's display pushed at once and B has it; B deletes; A writes the row again (`--notes`; land's record is a `--metadata` update of the same row) | **stops the same way** | as delete | as delete | not run |

In the bold cells the missing rows came back once A wrote again and pushed;
in the "A pull again" step a merging pull committed them too. The
uncommitted tables after A's plain pull (no `--strategy`) were `comments`
and `labels`, both `modified` (never `conflict`), in exactly the four
takeover cases and none elsewhere. `takeover-multi` shows the outcome does not depend on the claim
display being one `bd update`: today's multi-commit display behaves the
same.

What each row means for an unpushed claim display:

- **Takeover.** The right holder wins the assignee, because a takeover comes
  after the claim it takes over (the claim ref must be stale first), so long
  as the machines' clocks agree. `takeover-first` shows the rule is by time,
  not by side: a claiming machine whose clock runs ahead by more than the
  gap would win the assignee back for a holder the ref has replaced. The ref
  still decides ownership; the display would name the wrong holder until the
  new holder's next display write or release. Ties within `updated_at`'s
  one-second resolution were not measured. The old run's `run:` label stays
  beside the new one until the new holder's `release` (which removes every
  `run:` label) or a display write of its run.
- **Person's edit and labels.** Merged cleanly: the person's fields and the
  claim display both stand.
- **Close.** The close wins, and the claim's assignee and `run:` label stay
  on the closed bead. Only a bead reopened, claimed on one machine and
  closed on another inside the window gets here. `display` skips closed
  beads, but the holder's own `release` clears the assignee and `run:`
  label (its display does not skip closed beads; `--reason done` also adds
  `bops:landing` to the closed bead).
- **Park.** The person's `blocked` wins and the claim's assignee and `run:`
  label stay, which is what a person parking a held bead gets today; `sync`
  keeps a parked status when it repairs a display.
- **Delete.** See the Answer: a stop today's flow can also hit, which the
  design makes more likely.
- **Comment.** Both stand, committed.
- **Abandoned takeover (the ghost display).** `reap` (which looks at every
  machine's claims) lists it, aged from the takeover's release, and releases
  it unless its evidence rules find work by the dead run (a pushed branch or
  an open merge request); then it stays `in_progress` for a person, as an
  idle claim does today. Plain `stale` shows it only on the dead holder's
  machine unless asked for all machines.

### Verdict for the design

**Go, with one named resolution:** bops runs `bd dolt commit` before every
`bd dolt push` it makes (so after every pull, too), so an auto-merged pull
publishes what it merged. That is a bops fix, not a resolution from the
ref. The one stop measured, a person deleting a bead an agent holds,
cannot be resolved from the ref, and the design makes it more likely (any
pull on the holder's machine during the session meets it, not only the
holder's next row write). It is not counted as a likely case: on a bops
ledger a person deletes a held bead only by mistake, and it can stop
today's flow too. Because it stops every push from that machine, it must be
reported with its runbook (step 4 makes a failed pull an event `status` and
`doctor` report). A team that deletes held beads routinely should take
that as the no-go and move to server mode. The commit gap
exists today too, whenever a push retry's pull auto-merges a same-row edit,
and a person's raw `bd dolt push` after such a pull still publishes the
incomplete merge; the design only makes the window longer. bd should commit its union itself (an upstream
report is worth filing); while bops supports bd 1.3.4, the commit stays in
bops.

### Method

[`2026-11-ledger-contention.sh`](2026-11-ledger-contention.sh) `pulls`:

```sh
bash docs/research/2026-11-ledger-contention.sh pulls /tmp/pulls              # every case
MEASURE_COMMIT=1 bash docs/research/2026-11-ledger-contention.sh pulls /tmp/pulls-c   # and each with bd dolt commit
bash docs/research/2026-11-ledger-contention.sh pulls /tmp/pulls-t takeover   # one case
```

It isolates bd and bops state as `local` does. It seeds one bare remote with
two beads, each labelled `area:seed`, then gives each case its own copy of
that remote and two new clones (`git clone`, `bd bootstrap`) with their own
`BEADS_ACTOR`. No bops runs: A and B make the bd calls bops makes, with the
claim display in the single `bd update` the design proposes (in
`takeover-multi`, the separate calls bops makes today). After A's pull
and push it reads the remote from B, then B writes another bead and pushes, A
pulls again, writes another bead and pushes, and B reads the remote once
more. A case whose pull exits non-zero would run again from the start with
`--strategy ours` and with `--strategy theirs`; only the delete cases did. Each step's row
(exit, the tables a copy of the store shows uncommitted, the bead's
assignee, status, labels, title, priority, notes, updated_at, closed_at,
comment count and newest `bops:` comment, and bd's output) goes to
`<workdir>/pulls.tsv`.

### Limits

- bd 1.3.4 embedded only; a newer bd may commit its union itself or settle
  differently. Re-run `pulls` before relying on this on another bd.
- One host, a local remote, B's edit 1.2 s after A's write. Clock skew
  between real machines was not measured; settling by time assumes clocks
  agree.
- One edit per case, on one bead. Two edits of the same bead by B in one
  window, a three-way meeting, dependency or parent edits and a `deferred`
  park were not run.
- bops was not in the loop: what `claim`, `land`, `release` and `sweep` do
  with a merged row is read from their code, not measured.
- The per-run `pulls.tsv` files are not committed; `pulls` reproduces them
  in about five minutes with `MEASURE_COMMIT=1`.

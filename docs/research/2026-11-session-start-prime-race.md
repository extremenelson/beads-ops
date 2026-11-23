# bops session-start and bd prime started together on one embedded Dolt store (bops-5qn)

> **Measured 2026-11-23** on macOS (Darwin 27.2, Apple silicon), bd 1.3.4
> (Homebrew), bops built from `ac803af` (main after bops-4ez), embedded Dolt
> (`dolt_mode: embedded`), 20 trials per case. Re-measure on a newer bd.

## Question

Claude Code starts every matching SessionStart hook at once. A repo that keeps
bd's own `bd prime --hook-json` hook (`bd setup` re-adds it) beside
`bops session-start --hook-json` runs bops's `bd dolt pull`, display repair,
heartbeat and its own `bd prime` alongside bd's `bd prime` on one embedded
store. Does either hook fail on the store's lock and leave the session without
its document? And, from the bead's comment, does a running `/bead-loop`'s
ordinary bd writes (labels, comments, `bd dolt push`) fail against
session-start's pull?

## Answer

**No failure from running them together.** In 60 trials each, `bd prime
--hook-json` always returned its document and `bops session-start --hook-json`
always synced with no warning, with the store idle, during a `bd dolt pull`,
and during `/bead-loop`-style writes. Every label and comment the loop wrote
was kept and reached the other clone.

The only failures were the loop's own `bd dolt push`, rejected as
non-fast-forward in 5 of 20 trials. That is the remote having moved, not
contention: run alone with no hook (the control), the same push is rejected in
20 of 20, and with session-start beside it only 5, because session-start's pull
usually merged the remote change first. `/bead-loop` already has to pull and
retry a rejected push whatever else runs.

So no ordering needs documenting, and session-start does not need to skip its
own prime when bd's hook is present. That bd's prime then appears twice in the
session's context is a size question, not a failure, and is out of scope here.

## Results

| Case | Command | Failed | Mean | Max |
|---|---|---|---|---|
| idle | `bd prime --hook-json` | 0/20 | 0.11 s | 0.11 s |
| idle | `bops session-start --hook-json` | 0/20 | 2.16 s | 2.26 s |
| during a pull | `bd prime --hook-json` | 0/20 | 0.36 s | 3.32 s |
| during a pull | `bops session-start --hook-json` | 0/20 | 2.80 s | 3.10 s |
| during a pull | `bd dolt pull` | 0/20 | 1.35 s | 3.79 s |
| during loop writes | `bd prime --hook-json` | 0/20 | 0.28 s | 2.99 s |
| during loop writes | `bops session-start --hook-json` | 0/20 | 3.47 s | 5.10 s |
| during loop writes | label, comment, `bd dolt push` | 5/20 | 3.13 s | 5.03 s |
| control: loop writes alone | label, comment, `bd dolt push` | 20/20 | | |

Every failure is the push's `Error 1105: ... ! [rejected] main -> main
(non-fast-forward)`; the label and comment before it succeeded each time.

Under contention `bd prime` sometimes waits: 4 of its 40 contended runs took
0.6 to 3.3 s instead of 0.1 s, consistent with waiting on bd's store lock
(`.beads/embeddeddolt.gate.lock`) rather than failing on it. A session-start
counts as passing only when its document says `"synced": true` with
`"warnings": []`, since a failed pull or bd call inside it is a warning, not an
exit code.

## Method

[`2026-11-session-start-prime-race.sh`](2026-11-session-start-prime-race.sh)
reproduces it: `sh 2026-11-session-start-prime-race.sh 20 <workdir>` for the
three cases, and `sh 2026-11-session-start-prime-race.sh 20 <workdir> control`
for the control. It builds everything inside the work directory: a bare git
repository as the remote, clone `a` (the session's, with `bd init`, ten beads
and one bead claimed through `bops claim`, so session-start's display repair
and heartbeat have a hold to work on) and clone `b` (`bd bootstrap`).
`XDG_STATE_HOME` points inside it, and `BOPS_RUN` is unset.

Before every trial `b` creates a bead and pushes, so `a`'s pull has a change to
merge, as on a ledger other machines write. Each trial then starts, at the
same moment in clone `a`:

- **idle:** `bops session-start --hook-json` and `bd prime --hook-json`.
- **pull:** those two and `bd dolt pull`.
- **writes:** those two and `bd label add`, `bd comments add` and
  `bd dolt push` in sequence on another bead, as `/bead-loop` does between
  claims.
- **control:** only the writes.

Results go to `<workdir>/results.tsv`, one row per command per trial, and
each command's output to `<workdir>/runs/<case>-<trial>/`.

## Limits

- One machine, a local file remote. On a slow link a pull runs far longer
  (bops-9rb saw pulls past 35 s). If `bd prime` waits for a whole pull, as the
  timings above suggest it can, a slow pull another process runs could push
  bd's own prime hook toward Claude Code's 60 s hook timeout. That was not
  measured. bops-9rb caps only session-start's own pull, at 35 s.
- 20 trials a case: a failure rarer than about 1 in 20 can still exist.
- The hooks were started together by a shell script, not by Claude Code, so
  any stagger Claude Code adds is not reproduced; starting them at exactly
  the same moment is the harder case.
- bd 1.3.4 only. A change to bd's store locking makes this stale.

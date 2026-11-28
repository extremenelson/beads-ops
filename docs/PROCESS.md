# Beads Operating Process

**Status:** v0.4 draft (v0.4: roles for more than one human in §1, takeover policy in §12; v0.2: §3.6 workability table, §3.7 pre-claim lint; v0.3: the `bops` claim lock in §1, §2.2, §3.7, §8, §9.3 and the recovery runbook in §12) — opinionated process layer over the upstream beads skill (gastownhall/beads, plugin v1.3.2 / skill v0.60.0, 2026-11-01).
**Scope:** one or more humans (developers, at least one of them a lead, §1) + their agents (Claude Code, Hermes, Kestrel) sharing one issue graph per repo, each human on several machines that are sometimes offline. A solo repo is the case of one developer who is also the lead.
**Relationship to upstream:** this document does not replace `bd prime` or the skill's `resources/`. It fixes the decisions upstream leaves open. Where upstream and this file disagree on *mechanics*, upstream wins (it tracks the binary). Where they disagree on *policy*, this file wins.

---

## 0. The three invariants

Everything else in this document is derived from these.

1. **The graph is the plan.** No markdown TODO lists, no `PLAN.md`, no `MEMORY.md`. If it is work, it is a bead. If it is a durable project fact, it is `bd remember`. If it is neither, it belongs in the conversation and is allowed to die with it.
2. **Notes are written for a reader with zero context.** Every `in_progress` bead carries a note that a fresh agent — or a person after two weeks — can resume from without the conversation. This is the only thing that makes compaction, crashes, and machine-hopping survivable.
3. **`blocks` means *cannot proceed*, nothing softer.** Only `blocks` affects `bd ready`. Everything that is sequencing preference, context, or provenance uses `related`, `parent-child`, or `discovered-from`. An over-constrained graph collapses `bd ready` to one item and kills parallelism — which is the whole point of having more than one agent.

---

## 1. Roles

| Actor | Owns | Never does |
|---|---|---|
| **Lead (human)** | `bd init`, epic-level goals, priority (P0–P1), resolving `human` gates, deciding what gets distilled into a formula, `bd dolt push` policy, the team's `bd` and `bops` versions, taking over another person's landing or deleting an orphan claim ref (§12) | Hand-edit `.beads/`; create P2–P4 tasks that agents could file themselves |
| **Developer (human)** | Their own claims and their agents' work; reviewing and merging PRs; `bops claim --resume` of their own beads; `bops claim --force` of a stale claim after trying to reach its holder (§12) | Hand-edit `.beads/`; set P0/P1 or close an epic unless also a lead; act as another person's actor |
| **Agent (any harness)** | Decomposition below the epic, claiming, notes, discovered-from filing, closing with reasons, `bd gate check` at session start | `bd init`; change P0/P1 priority; close an epic; resolve a `human` gate; delete beads; run `bd dolt push --force` |
| **Autonomous loop** (task-agent, Kestrel worker) | `bops next` → `bops claim` → execute → close → repeat (`bd ready --claim` on a ledger without `bops`) | Pick up anything with label `needs-human`; create epics; work when `bd blocked` shows a `human` gate on the current epic |

`bd ready --claim` and `bd update --claim` compare and set on one replica only: two machines can both claim one bead, and Dolt's last-write-wins settles it later without telling the loser. `bops claim` closes that gap with a git ref per bead (§2.2). The guarantee covers only claims made through `bops`. The Kestrel binding claims through `bops claim`. `/bead-loop` claims through `bops claim` when the repo's protocol file sets `**Claim tool:** bops`; with no such line it claims through raw `bd` and is outside the guarantee, so set that line on every `bops` ledger.

**Who the leads are** is written in the repo's `AGENTS.md` (a `Leads:` line naming their actor handles), so agents and people read it from the same place. A team has at least one lead; in a solo repo the one developer is the lead.

**Actor handles.** Each person has one `BEADS_ACTOR` handle, the same on every machine they use (the forge handle is the obvious choice), agreed when the ledger is set up ([MULTI-DEV-PLAN Phase 0](MULTI-DEV-PLAN.md#phase-0--decide-and-document-1-hour-one-person)). Their agents claim under that handle; the claim ref's `run` and `machine` tell their sessions apart. The handle is what `claim --resume` checks, so a person who uses two spellings on two machines cannot move a bead between them. A harness that runs under its own actor (Kestrel's `kestrel@<host>`, §9.3) is its own owner for these checks.

The split is deliberate: humans set direction and approve, agents do decomposition and bookkeeping. Agents filing their own follow-ups is what makes the system cheaper than a human-maintained tracker; agents re-prioritizing is what makes it drift.

---

## 2. Session protocol (every harness, every session)

### 2.1 Start

```bash
bd prime                              # workflow rules + bd remember facts (hooks do this in Claude Code / Codex)
bd gate check                         # resolve elapsed timers, merged PRs, green CI
bd list --status in_progress --json   # what was I (or another agent) in the middle of?
bd ready --json                       # current ready front
```

On a `bops` ledger, `bops session-start` does the pull and the reports in one call, and is what the Claude Code hook runs as `bops session-start --hook-json`, which wraps the document in the hook envelope Claude Code injects ([`hooks.json`](../bindings/claude-code/hooks.json)): `mine` (what this worktree holds), `lost_claims`, `stranded_holds`, the `stale` count, `ready_count` and bd's minimal prime. It does not run `bd gate check`. A failed sync still prints the document, with `ok:false`.

Then **report before acting**: "N in progress (ids), M ready. Last note on `<id>`: NEXT = …. Continue with that?" In an unattended run, skip the question, pick the highest-priority `in_progress` item assigned to this actor, else the highest-priority `ready` item, and say which you chose in your first line of output.

Rule: **in_progress beats ready.** Finish or hand off before claiming new work. Two half-done beads are worth less than one closed one.

### 2.2 Claim

```bash
bd show <id> --long                   # read description, design, acceptance, notes, deps
bops claim <id>                       # the lock: one git ref per bead, then assignee + in_progress
bops claim --next                     # or: claim next's first free bead, moving on if one is lost
```

Never start editing files before the claim lands. In a multi-agent setup the claim *is* the lock, and on a `bops` ledger the lock is the claim ref, not the assignee. Act on the exit code:

| Exit | `outcome` | Do |
|---|---|---|
| 0 | `claimed`, `held` | Work. The claim's display is on this machine's ledger and reaches the remote with its next push (land, release, sync); `ledger_pending: true` means even the local write did not complete, and `bops sync` finishes it |
| 3 | `lost`, `refused` | Do not work. Pick another bead; `holder` or `reason` says why, and `next_candidate` names one to try |
| 1 | `unreachable` and the rest | Stop. Rerun when the remote is reachable; the result's `side_effects` says what may have been written |

Claiming needs the network: the lock lives on the remote. Notes work offline (§8). The exception is a ledger used on one machine with `lock.mode: none` in `.bops.json` (§8): claims then go through `bd update --claim` with no ref and no network, and every result warns `lock.mode is none`. A ledger without `bops` keeps `bd update <id> --claim`, with the one-replica limit above.

### 2.3 Work

- Decompose the acceptance criteria into the harness's ephemeral list (TodoWrite in Claude Code, Hermes's task list, Kestrel's step vector). That list is disposable.
- At each **milestone** (not each keystroke) write a handoff note (§4). Milestone = a test passes, a decision is made, a blocker appears, you are about to stop.
- Discovered work → file it immediately (§5), link `discovered-from`, decide blocker-or-defer, move on. Do not carry discoveries in your head.

### 2.4 Stop (planned or not)

Before the session ends, or any time the context window is getting heavy:

```bash
bd update <id> --append-notes "<handoff note, §4 format>"
bd dolt push                          # if a remote is configured and you are online
```

Planned stop with work complete → §2.5. Planned stop with work incomplete → leave `in_progress`, note written. Blocked → `bd update <id> --status blocked --append-notes "BLOCKER: …"`.

### 2.5 Close

Closing is a claim that acceptance criteria are met. Before `bd close`:

- [ ] every acceptance criterion is verifiably true (tests, output, link)
- [ ] follow-ups are filed and linked (`discovered-from`)
- [ ] RESOLUTION note written if the outcome differed from the design
- [ ] `bd close <id> --reason "<what was done, how verified, PR/commit ref>"`
- [ ] `bd ready --json` — report what just became unblocked

Agents close tasks and features. **Only a lead closes epics**, after confirming the parent outcome — not merely that all children are closed.

---

## 3. Issue quality

### 3.1 Fields and what goes in them

| Field | Content | Stability |
|---|---|---|
| **Title** | Specific, verb-first, searchable. `Fix: refresh token expires before rotation` not `Auth bug` | Fixed |
| **Description** | Problem, context, *why it matters*. Written for someone who doesn't know the codebase | Fixed |
| `--design` | HOW: approach, architecture, trade-offs, rejected alternatives | May change during work |
| `--acceptance` | WHAT must be true when done. Outcome-focused, each item a yes/no check | Stable across sessions |
| `--notes` / `--append-notes` | Handoff state (§4), implementation guide, RESOLUTION | Evolves |

**The acceptance test:** if the solution were rewritten with a completely different approach, would every acceptance criterion still apply? If not, it's design, not acceptance.

### 3.2 Size

A task should be closeable in **one focused agent session (≤ ~2 hours of agent work, ≤ one context window)**. If you can't see the end from the start, it's an epic or needs a spike first.

- 1 unit of work → `task` / `bug` / `chore`
- 2–4 related units with a shared outcome → `feature` with `parent-child` tasks
- 5+ units or spans sessions by design → `epic`

### 3.3 When an agent asks before filing

Create directly: clear bug, obvious follow-up, scoped tech debt, a dependency you just hit.
Ask first: fuzzy knowledge work, unclear intent, multiple valid approaches, anything that would change priority of existing P0/P1 work.

### 3.4 Priority (fixed meaning)

| P | Meaning | Who sets |
|---|---|---|
| 0 | Production down / data at risk / blocks everything | Lead |
| 1 | Blocks the current epic's ready front | Lead (agent may propose in notes) |
| 2 | Default for planned work | Agent |
| 3 | Nice to have, no deadline | Agent |
| 4 | Backlog / someday | Agent |

Discovered bugs default to P2 unless they block the current work, in which case the agent sets P1 **and says so in the note**.

### 3.5 Labels (small, fixed vocabulary)

`needs-human` · `spike` · `template` (reserved by bd for protos) · `security` · `cost` · `infra` · `<machine-name>` (for work that must run on a specific box, e.g. `gpu-3090`)

Anything else needs a one-line justification in `bd remember`.

`needs-human` is spelt with a hyphen everywhere. Early `bops` builds wrote `needs:human` on `release --reason human`; `bops` and `scripts/bd-lint-claim.sh` still read that spelling as `needs-human` until v0.2.0 (SPEC §3.1). To relabel a ledger's old beads, once per ledger:

```bash
bd list --all --label needs:human -n 0 --json | jq -r '.[].id' | while read -r id; do
  bd label add "$id" needs-human && bd label remove "$id" needs:human
done
```

**Optional: `touches:<path>`.** A bead may declare the repo-relative paths it expects to change, one label each (`bd label add <id> touches:bops/internal/intent/`). Paths are case-sensitive. A directory covers everything under it; a glob covers the directory above its first wildcard (`bops/*.go` covers `bops/`, `*` the whole repo). `bops next --avoid-conflicts` then leaves out a candidate whose paths overlap those of a claimed bead, yours included, naming the overlap in `why_not`; `next` and `bops status` show each bead's `touches`. It is advisory only: nothing stops a claim or an edit, `claim --next` does not screen, two candidates in one answer may overlap each other, and a bead that declares nothing never conflicts. Use it when parallel agents keep colliding on the same files; skip it otherwise.

### 3.6 Workability — what must be true before a bead is claimed

"Workable" has two layers. **Mechanical** is what `bd ready` computes; bd enforces it. **Executable** is whether a cold agent can finish the bead without rediscovering context; nothing enforces it except this section and `scripts/bd-lint-claim.sh` (§3.7).

**A. Mechanical — enforced by `bd ready`**

| # | Requirement | Why it matters |
|---|---|---|
| 1 | Status is `open` | `ready` means claimable now. `in_progress` under a dead agent is invisible until it is taken over: on a `bops` ledger with `bops claim --force` after 120 minutes (§12), elsewhere with `bd reclaim`. |
| 2 | No open `blocks` dependency | Only `blocks` affects readiness; this is what makes the graph the plan. |
| 3 | No unsatisfied `external:` dependency | Hidden until the other project `bd ship`s the capability. |
| 4 | No open gate on it | Irreversible steps wait for approval/CI. |
| 5 | Unassigned, or assigned to a listed claim pool | `--claim` refuses another real actor's bead (anti-steal). |
| 6 | Not deferred past now | `--defer-until` parks without lying about status. |
| 7 | `is_blocked` flag is current | Denormalized; stale on an unsynced replica. `bd sync` recomputes it. |
| 8 | Not labeled `needs-human` (or the legacy `needs:human`, §3.5) *(policy)* | Autonomous loops must skip these. |

**B. Executable — what a cold agent needs**

| # | Requirement | Why it matters |
|---|---|---|
| 9 | Specific, verb-first title | Title is all that shows in `bd ready`; vague titles force a `bd show` per candidate. |
| 10 | Description states problem, context, *why* | Zero-context reader must know what's wrong and what it affects. |
| 11 | Acceptance criteria: outcome-focused, each verifiable yes/no | Definition of done that survives a change of approach. Test: rewrite the solution differently — do the criteria still apply? |
| 12 | Design kept out of acceptance | Design may change mid-work; acceptance must not. |
| 13 | Scope ≤ one agent session | Otherwise half-finished at compaction; split into epic or spike first. |
| 14 | Correct type and priority | `ready` sorts by priority; loops pick top-first. |
| 15 | If resumed: handoff note with a one-step `NEXT` | Only memory across compaction and machines. Resume refuses on missing `NEXT`. |
| 16 | If non-obvious technical work: implementation guide | Unfamiliar APIs/formats cost a session to rediscover. Skip for obvious work. |
| 17 | Provenance linked (`discovered-from` / `parent-child`) | Says why this exists and whether it still matters. |
| 18 | Dependencies point dependent → prerequisite | Validate with `bd blocked`; phase-named beads are the usual cause of inversion. |
| 19 | Labels carry execution constraints (machine, `security`, `spike`) | A 3090-bound bead is not workable on the laptop. |
| 20 | Parent epic still wanted | Children stay mechanically ready after an epic is abandoned; weekly review closes dead epics. |
| 21 | Replica synced (`bd sync` → 0 recently) | Workable is a property of the hub, not your disk. |

### 3.7 Pre-claim lint

`scripts/bd-lint-claim.sh <id>` (ships beside this doc) checks rows 1–8 (via `bd ready` membership and `bd show`), 9–12, 14–15, 19, 21 mechanically, and prints warnings for the judgment rows (13, 16, 17, 20). Exit 0 = claim; 1 = fix the bead first; 2 = tool/shape error. Wire it in front of every claim:

```bash
alias bdclaim='f(){ scripts/bd-lint-claim.sh "$1" && bops claim "$1"; }; f'
# a ledger without bops: ... && bd update "$1" --claim
```

Kestrel's `claim!` does the same: it runs the lint, then `bops claim <id> --json`, and returns `lost` or `refused` as data. Any Hermes claim step should shell out the same way.

---

## 4. Handoff notes

Notes are the memory. Format is non-negotiable so that any harness can parse it.

```
COMPLETED: <specific accomplishments — what works now, not "made progress">
IN PROGRESS: <exact current state; which part is half-done>
NEXT: <one concrete step a stranger could execute>
BLOCKER: <none | what, who, since when>
KEY DECISION: <choice + rationale + what was rejected>   (optional)
REF: <paths, PR, URL, bead ids>                            (optional)
```

Rules:

- **Current state, not a diary.** Each handoff note describes the state *now*. Append is fine (`--append-notes`) but the latest block must stand alone.
- **NEXT is one step.** "Continue implementation" is a failed note. "Add rate limit (5/15min) to `POST /refresh`, then run `make test-auth`" is a passing one.
- **Decisions get rationale.** The rejected alternative is more valuable to a future reader than the chosen one.
- For complex technical work (unfamiliar API, specific output format, "occult" behavior) add an **IMPLEMENTATION GUIDE** block: working code that was actually run, a sample of the real response shape, an example of the desired output. Skip this for anything a competent engineer would figure out in five minutes.

**Parse contract for Kestrel/Hermes:** `^(COMPLETED|IN PROGRESS|NEXT|BLOCKER|KEY DECISION|REF):\s*(.+)` on the last block of the notes field. Treat a missing `NEXT` as a hard error at resume time.

---

## 5. Decomposition and dependencies

### 5.1 Walk backward, name by WHAT

Plan epics as **ready fronts**, not phases. Start at the deliverable and ask "what does this need?" until you hit things that need nothing.

```
"Integration tests green"      needs  "Streaming", "Header"
"Streaming", "Header"          need   "Message rendering"
"Message rendering"            needs  "Buffer layout"   ← ready front 1
```

Encode each "X needs Y" literally: `bd dep add X Y`. Direction mnemonic: **dependent first, prerequisite second**. Never name beads "Phase 1 / Phase 2" — temporal names invert dependencies in people's heads and in agents' outputs.

Validate with `bd blocked`: every item must be blocked *by its prerequisite*, never by its dependent. If `setup` is blocked by `integration`, the edge is backwards.

### 5.2 Dependency type decision

```
Does A prevent B from starting?            → blocks        (affects bd ready)
Is B a planned sub-part of A?              → parent-child  (structure only)
Was B found while working on A?            → discovered-from (provenance only)
Otherwise connected?                       → related
```

`blocks` + `parent-child` together is the normal shape of an ordered epic: parent-child for structure, blocks between children for order. Use `blocks` only for real technical prerequisites. If two children *could* proceed in either order, do not link them — let `bd ready` offer both.

### 5.3 Standard shapes

- **Foundation fan-out:** one bead blocks N — maximizes the next ready front. Prefer this over chains.
- **Diamond:** setup → {A, B} → verify. The canonical "parallel then merge".
- **Parallel research → decision:** N spikes block one `decision` bead; decision bead's note records the choice and rejected options; implementation beads are `discovered-from` the decision.
- **Spike:** time-boxed, `spike` label, findings in `--design`, follow-ups filed `discovered-from`, closed with a recommendation. A spike never ships code.

### 5.4 Hierarchy depth

Max three levels: epic → feature → task. Beads supports deeper hierarchical ids (`bd-a3f8.1.1`), but a fourth level is a smell that the epic is actually a program and should be two epics.

---

## 6. Gates

Gates are ephemeral wisps that hold a bead blocked until a condition is met. Use them instead of an agent polling or inventing a "waiting" bead.

| Situation | Gate |
|---|---|
| Needs a lead's sign-off (deploy, spend, external comms, anything irreversible) | `bd gate create --type human --blocks <id> --reason "…"` |
| Waiting on GitHub Actions | `--type gh:run --await-id <run-id>` |
| Waiting on PR merge | `--type gh:pr --await-id <pr#>` |
| Propagation / cool-down | `--type timer --timeout 15m` |
| Waiting on another local bead that isn't a `blocks` relationship | `--type bead --await-id <id>` |

Policy:

- **Any irreversible or money-spending step gets a `human` gate** and the `needs-human` label on the gated bead. Autonomous loops skip `needs-human` work entirely.
- `bd gate check` runs at every session start (§2.1). Only `timer`, `gh:*`, and `bead` gates auto-resolve; `human` gates resolve only via `bd gate resolve`.
- `--timeout` is only enforced on `timer` gates. Do not rely on it to expire a human gate — if one sits for more than a week, the next agent that sees it reports it rather than working around it.
- When a `gh:*` gate escalates (failed CI, PR closed unmerged) the agent does **not** resolve it; it files a bug `discovered-from` the gated bead, writes a BLOCKER note, and stops.

---

## 7. Molecules, wisps, and when to template

| Work | Form | End state |
|---|---|---|
| Feature, bug, spec — anything someone will look at later | **mol** (`bd mol pour` / regular beads) | `bd close` |
| Grooming, review pass, health check, agent coordination scratch | **wisp** (`bd mol wisp <proto>`) | `squash` if there were findings, `burn` if not |
| Repeatable workflow (release, onboarding, FinOps quick-scan, Debian migration of one host) | **proto / formula** | spawn per run |

Decision test for wisp vs mol: *will anyone reference this in two weeks?* No → wisp.

**Distill, don't pre-design.** Run a workflow as an ad-hoc epic twice. If the second run has the same shape as the first, `bd mol distill <epic> --as "<Name>" --var <k>=<v>` and commit the formula. Writing a formula before the second run is speculative and usually wrong.

Formulas live in `.beads/formulas/*.formula.toml`, with `[[steps]]` carrying `id`, `title`, `needs = [...]`, and a `description` that is itself the agent prompt for that step. Variables are `{{var}}`. The upstream release formula (`vendor/beads-skill/_upstream-extras/beads-release.formula.toml`) is the best worked example: gate-aware, phase split, prompt-quality step descriptions.

Candidate first formulas for this setup: `mol-debian-host-migration` (per-host Arch→Trixie runbook), `mol-quick-scan` (FinOps assessment), `mol-weekly-review` (triage, priorities, stale close).

---

## 8. Multi-machine and offline

Beads is Dolt-backed; `.beads/issues.jsonl` is an export for viewers, not the source of truth.

- **Embedded mode (default)** on every machine: `.beads/embeddeddolt/`. Right choice for one human + sequential agents per box, and for up to 4 concurrent agents (`/bead-loop`) on one machine: bd mostly queues them on the store rather than failing, at the cost of latency (claim p50 ~5 s instead of ~2 s).
- **Supported concurrency** (bops ledgers, [measured](research/2026-11-ledger-contention.md#re-measured-with-claims-that-do-not-push-2026-11-27) with claims that do not push): **at most 4 agents per machine, and at most 8 concurrent writers in total on a fast remote** (2 machines x 4 agents). **On a hosted remote, run one `/bead-loop` machine** plus occasional single sessions elsewhere: with 400 ms per git call, 8 writers kept claims fast (p50 9 s) but 13% of land and release pushes failed after their tries (15% counting releases that failed before pushing), at or over the switch condition on a small sample; a two-machine run on a real network has not been made. Within these limits expect claims of a few seconds (p95 under 30 s on a fast remote, about 40 s with 400 ms of latency), lands and releases of 5 to 30 s at the median with a few reaching the 60 s budget, the odd land or release whose push fails after its tries (`landed locally; push failed`, or `ledger_pending` on a release: the next push or `bops sync` publishes it), and the rare `database is locked` failure. A claim that fails on one leaves the bead free to claim again; a `release` that answers `error` leaves the bead held, so run it again. One known bug remains: a `release --reason done` whose display write fails is not repaired, and the ledger can go on showing the bead held. At 12 writers more than 1 land or release push in 10 failed. Above these limits, or when more than 1 ledger push in 10 that carries a write fails after its tries (every push logs one `ledger_push` line with its command, outcome and tries; `cat events.jsonl.1 events.jsonl 2>/dev/null | jq -r 'select(.step=="ledger_push" and .command!="sync" and .tries>0) | .outcome' | sort | uniq -c` counts `failed` against `pushed` for `land`, `release`, `sweep` and `reap`, leaving out `sync`, whose pushes often carry nothing, and pushes a stuck ledger stopped before any try; a failed line's `detail` tells a rejected push or the command budget from an unreachable remote; a land or release that failed on the local store before pushing logs no `ledger_push` line, so count those exits too), move to a central server ([MULTI-DEV-PLAN §1](MULTI-DEV-PLAN.md#measured-limits)).
- **Claims are published late.** Under `lock.mode ref` a claim writes its display to this machine's ledger and does not push it; the next `land`, `release`, `bops sync`, or `reap` or `sweep` that releases or closes something, from this machine publishes it. Until then raw `bd ready` and `bd list` on another machine can show a held bead as ready, and that machine's `bops stale`, `reap` and `status` do not see the claim. Raw-`bd` claiming on another machine is not supported on a `bops` ledger: claim through `bops` (`bops next` reads the claim refs), and install `bops guard`, which blocks a raw `bd update --claim`. `bops status` and `bops doctor` say when this machine's ledger has commits the remote lacks (`ledger unpublished`), has diverged, or a claim's pull failed (`ledger not pulled`); `bops sync` clears the first two when the remote answers, and the third once a pull succeeds and the bead shows no other run's claim and is not closed (or the claim is released). Before taking a machine offline or ending a loop, run `bops sync`.
- **Sync** is `bd dolt pull` at session start (when online) and `bd dolt push` at session end. The remote is `refs/dolt/data` on the git remote — no extra service. Dolt does cell-level merge; hash ids (`bd-a1b2`) make concurrent creates collision-free.
- **Offline** is fully supported: everything in §2 works without network. The only thing deferred is sync. Note in the handoff when you were offline (`REF: offline, not pushed`) so the next session pulls before claiming.
- **Server mode** (`bd init --server`, external `dolt sql-server`): a per-machine server if a machine's agents routinely fail on `database is locked` (rare with 4 agents on bd 1.3.4); a central server when the supported concurrency above is exceeded. Hermes + Kestrel on one box, each in its own session, is still sequential enough for embedded.
- **Per-machine work:** label it (`gpu-3090`, `debian-box`). Autonomous loops filter `bd ready --json` by their own machine label plus unlabeled work.
- **Worktrees:** `bd worktree create .worktrees/<name> --branch feature/<name>` shares the repo's `.beads/` automatically; one worktree per concurrently active agent. `bd where` when anything looks wrong. Each worktree is its own `bops` run, so two agents on one clone cannot both hold a bead. Sessions in one worktree share its run unless they set `BOPS_RUN=<name>`, which gives them a named run of their own there: an orchestrator that claims from the main checkout, such as `/bead-loop`, sets it on every `bops` call, so a session a person starts in that checkout neither heartbeats nor repairs the orchestrator's claims, and cannot release them (bops-4ez).
- **The claim lock** (`bops` ledgers) is `refs/bops/claims/<id>` on the lock remote (`lock.remote`, default `origin`), which must be the same repository as `refs/dolt/data`; `bops claim` refuses if that remote is missing or is not the Dolt remote, and `bops doctor` checks it. Claim, release, land and sweep need the network. `note` works offline and refreshes the lock when it can. **Orchestrator heartbeat:** `bops renew <id> [<id>...]` refreshes the claim ref (its `at`, the age `stale`, `reap` and `claim --force` read) of each bead this run holds, and writes nothing to the ledger: no comment, no `bd heartbeat`. Run it as the run that claimed: from the checkout that claimed, with the same `BOPS_RUN` (an orchestrator that claims for its agents, such as `/bead-loop`, renews from the main checkout; an agent that claimed in its worktree renews there). Call it on a fixed cadence well inside `stale_minutes` (every few minutes, not every second: each renew pushes a new commit), and only while the agent working the bead is alive, since a renewed claim never goes stale for `reap` to find. Each bead gets its own outcome: `renewed`; `refused` (not held by this run, an invalid id, or `legacy_hold`, a hold from before the lock with no ref) or `lost` (another run's ref now; the hold is dropped), which exit 3; `unreachable` or `error` (the ref was not refreshed, the hold is kept), which exit 1 for a retry; once the remote is unreachable, the beads after it are not tried. A run with both exits 1, so on any non-zero exit read `beads[].outcome`. It needs the claim lock, so it refuses under `lock.mode none`, where a claim ages from the ledger and `note` refreshes it. `bops sync` fetches every claim ref, finishes the deletes and ref writes that failed in any worktree of the clone, repairs the assignee display of this worktree's claims from them (never changing a `blocked` or `deferred` status a person set), and reports `lost_claims` (someone took a bead from you) and `stranded_holds` (a worktree that no longer exists). In the beads-ops checkout, rebuild the installed `bops` after a change to `bops/` merges (`cd bops && go install ./cmd/bops`), before the sweep; `bops doctor` warns when it predates the last `bops/` commit.
- **One machine, no lock:** a ledger only ever used on one machine (with or without a remote) may set `{"lock": {"mode": "none"}}` in the committed `.bops.json`. Ownership is then the ledger alone, which separates the worktrees of that machine and nothing more; `claim --resume` is refused and `bops doctor` skips its checks (SPEC §3.0). Never set it anywhere else: a per-user file or `BOPS_LOCK_MODE` that turns the lock off is refused, because one machine of a shared ledger without the lock silently takes beads others hold. Release everything before switching modes. The lock remote is not used under `none`.
- **Upgrades:** every machine on a ledger runs a `bops` that knows the lock before anyone claims with it; an older binary ignores the lock. Once every machine has upgraded, raise `min_bops` in `.bops.json` to that release: an older `bops` then refuses every write with `bops_too_old` instead of relying on everyone remembering ([configuration](reference/configuration.md#min_bops)). Do not run `bd reclaim` on a `bops` ledger, by hand or on a timer: it ignores the lock.
- **Local record:** each clone keeps `$XDG_STATE_HOME/bops/<hash>/events.jsonl` (default `~/.local/state/bops/…`), one line per step and outcome. Read it first when an outcome needs explaining.

**Committing the export.** On bd 1.3.4, `.beads/issues.jsonl` on disk can be behind the ledger, and the pre-commit hook does not fix the commit. Auto-export runs at most once per 60 s, so a claim made right after another write is not in the file. Plain `bd export` prints to stdout. The hook rewrites the file after `git add` without staging it. To commit a current export:

1. Run `bd export -o .beads/issues.jsonl`. `-o` is required.
2. Run `git add .beads/issues.jsonl && git commit`, with no bd write between the two steps. Use a plain commit, not `-a` or `-- <paths>`.
3. Run `git status --short -- .beads/issues.jsonl`. It must print nothing. If it shows ` M`, export again and run `git add .beads/issues.jsonl && git commit --amend --no-edit`.

From a linked worktree, run step 1 from the worktree root. The hook there also rewrites the main checkout's `.beads/issues.jsonl`. That file is only an export, so ignore the change or discard it. Root cause and reproduction: [research/2026-11-beads-hook-export-skew.md](research/2026-11-beads-hook-export-skew.md) (bops-7fw).

Never `export BEADS_DIR` globally — it overrides every project's own `.beads/`. Use it per-command for the user-level `~/.beads` only.

---

## 9. Harness bindings

The process above is harness-neutral. This section is what each one needs so the agent actually follows it.

### 9.1 Claude Code

Install upstream, then layer policy on top.

```bash
bd init                      # per repo; writes AGENTS.md section + .claude/settings.json hooks
# or, in an existing project:
bd setup claude              # idempotent; --check to verify, --global for ~/.claude
```

What the plugin gives you (`plugins/beads/.claude-plugin/plugin.json`): `SessionStart` and `PreCompact` hooks that run `bd prime`, the `beads` skill, 29 `/bd-*` slash commands, and `task-agent`. Upstream deliberately does **not** run `bd gate check` or `bd list --status in_progress` in hooks. Add them.

**`.claude/settings.json` (project):** merge [`bindings/claude-code/settings.json`](../bindings/claude-code/settings.json), which adds sync, `bd gate check` and the `in_progress` report to the upstream hooks. If you use `bops`, merge [`hooks.json`](../bindings/claude-code/hooks.json), which `bops gen cc` generates, in its place. To have Claude Code refuse raw claims, also merge [`guard-hooks.json`](../bindings/claude-code/guard-hooks.json): a `PreToolUse` hook on Bash that runs `bops guard --hook-json` and denies `bd update --claim`, `bd ready --claim` and `bd reclaim` while the claim lock is on, naming the `bops` command to run instead. It is opt-in, and it fails open: when `bops` is missing or cannot decide, the command runs.

**`CLAUDE.md` addition:** append [`bindings/claude-code/CLAUDE.md.snippet`](../bindings/claude-code/CLAUDE.md.snippet). It carries the process rules, the `bd-lint-claim.sh` claim rule and the `bops` session protocol.

Install steps are in [`bindings/claude-code/README.md`](../bindings/claude-code/README.md).

**Compaction:** the upstream `PreCompact` hook re-injects `bd prime` but cannot save your notes for you. Add a habit (or a `UserPromptSubmit` hook that checks context size) to write a handoff note when the conversation gets long. The Codex plugin's `PostCompact` + `UserPromptSubmit` refresh marker is the pattern to copy if you want it automated in Claude Code — not provided upstream for Claude.

**Autonomous:** `/bd-ready` → pick → `claude -p "$(bd show <id> --json | jq …)"` in a worktree, or use the plugin's `task-agent`. Filter out `needs-human` before dispatch.

### 9.2 Hermes

Hermes consumes `AGENTS.md`, so the baseline is the upstream `bd init` section plus the same policy block as §9.1's `CLAUDE.md` (Hermes should read `AGENTS.md` — don't duplicate into a Hermes-specific file).

Hooks: wire whatever Hermes's session-start mechanism is to run `bd prime && bd gate check && bd list --status in_progress --json`. If Hermes has no hook surface, make it the first line of every Hermes system prompt that touches a repo with `.beads/`. **Verify this against your Hermes config** — I have not checked Hermes's hook model and am not asserting it has one.

Skill: drop `skills/beads/` from the zip into Hermes's skill directory as-is. It reads `SKILL.md` → `resources/*.md` on demand, which is exactly the progressive-disclosure shape Hermes wants.

### 9.3 Kestrel (Clojure)

Kestrel should treat `bd --json` as its persistence API and never touch `.beads/` directly. A Babashka skill wrapping the CLI is ~100 lines; the value is in the schemas and the resume contract, not the plumbing.

The skill lives at `bindings/kestrel/beads.clj` (draft; see `bindings/kestrel/README.md` for what still needs verifying against a live `bd`). Summary of its surface:

| Function | Contract point |
|---|---|
| `session-start` | §2.1 — sync, gate check, in_progress beats ready, needs-human/machine filter, throws on missing `NEXT` |
| `claim!` | §2.2 — runs `bd-lint-claim.sh --json` first; refuses on fail. Claims with `bops claim`, so it holds the `bops` lock; returns `:claimed`, `:held`, `:lost` (with `:holder`) or `:refused` (with `:reason`), and throws with the stable `code` when `bops` could not run |
| `handoff!` | §4 — Malli-validated note |
| `discovered!`, `block!`, `gate-human!` | §5, §6 — `block!` and `gate-human!` give the claim back with `bops release` (reason `blocked`, `human`), then write the BLOCKER note or the human gate with `bd` |
| `close!` | §2.5 — refuses epics; `bops land` then `bops release --reason done`; does not close: `bops sweep` closes after the merge |
| `sync!` | §8 — `bops sync`; offline returns a map |

Integration points in Kestrel:

- An Integrant component `:kestrel/beads` holding `{:actor "kestrel@<host>" :machine-label "<host>"}`; `session-start` runs in the component's init and its result seeds the first agent prompt.
- The core.async work loop is `ready → claim! → run steps → handoff! at each step boundary → close!`. A step boundary that would do something irreversible calls `gate-human!` and parks the channel.
- `handoff!` on every `:step/complete` event is cheap and is what makes a Kestrel crash recoverable: the next `session-start` finds the bead `in_progress` with a valid `NEXT`.
- Kestrel's SQLite stays for *its own* run-time state (channel offsets, tool-call logs). Anything a human or another agent needs to see goes to beads. Don't mirror.

---

## 10. Anti-patterns this process exists to prevent

| Smell | What it means | Fix |
|---|---|---|
| `bd ready` shows one item in a 20-bead epic | `blocks` used for preferences | Audit with `bd blocked`; downgrade to `related` |
| Notes say "made progress, more to do" | Note written for self-with-context | Rewrite in §4 format; fail resume if no NEXT |
| Agent closes an epic | Agent conflated "children closed" with "outcome achieved" | Only a lead closes epics |
| `PLAN.md` or `TODO.md` appears in the repo | Someone bypassed the graph | Convert to beads, delete file, `bd remember "no markdown plans"` |
| Formula written before the workflow has run twice | Speculative templating | Run ad-hoc, then `bd mol distill` |
| Three beads `in_progress` for one actor | Context-hopping | Finish or hand off; in_progress beats ready |
| `export BEADS_DIR` in a shell profile | Every project now shares one DB | Per-command only |
| Agent resolves a `human` gate | Policy breach | Agents never resolve human gates; label `needs-human` |
| Phase-1/Phase-2 bead names | Inverted deps incoming | Rename by WHAT, re-walk backward |

---

## 11. What's deliberately not decided yet

- **Server mode trigger.** Stay embedded until a real claim conflict happens between Hermes and Kestrel on one host. Revisit then.
- **Hermes hook mechanism.** §9.2 is conditional on how Hermes actually loads context. Confirm before relying on it.
- **Kestrel skill** is a draft. It needs to run against the installed `bd` once, with the `--json` shapes checked for `ready`, `list`, `show`, and `create`, and its `claim!` run once through a live `bops claim`.
- **Cross-repo capabilities** (`bd ship`, `external:` deps). Not needed until the venture work and the home-lab work live in separate repos and depend on each other. They may.
- **Compaction automation in Claude Code.** Upstream re-primes on `PreCompact` but does not write notes. Whether to automate the handoff note or keep it a discipline is open; try the discipline first.

---

## 12. Recovery runbook (`bops` ledgers)

The commands below say `origin`; if `lock.remote` (SPEC §10) names another remote, use that name instead. Every procedure here was run once against two clones of a scratch ledger and real `bd` 1.3.4 on 2026-11-10. The two-machine run for `bops/v0.1.0` repeats them across machines. The claim ref is ownership and the ledger is its display (§2.2), so most stuck states are fixed by reading the ref and then running one `bops` command.

**Read a claim.** Who holds a bead, in what state, since when:

```bash
git fetch origin "+refs/bops/claims/<id>:refs/bops/seen/<id>"
git log -1 --format=%B refs/bops/seen/<id>     # run=, actor=, state=held|landing, at=
```

`bops sync` fetches every claim ref the same way. The clone's own record of what it did is `~/.local/state/bops/*/events.jsonl`.

**List what is stuck.** `bops stale` lists claims idle longer than `--older-than` (default 2h) with their actor, age and `same_machine`, and `orphans`: held claim refs, over the same age, whose bead shows no claim on the ledger (`bops gc` deletes those whose bead shows no work). Age comes from the claim ref, which `note` and `renew` refresh; a claim with no ref is aged from the ledger. `--all-machines=false` keeps to this machine. Offline, it uses the refs from the last `sync` and says so in `warnings`.

**See who holds what.** `bops status` is the board: every claim on every machine, joined from the claim refs and the ledger, one row per bead with its `state` (`live`, `stale`, `landing` or `orphan` as `stale` judges them, the ref deciding over the `bops:landing` label; `unknown` when it cannot be judged: a ref whose state or `at` is unreadable or whose `at` is in the future, or a claim with no ref whose comments could not be read), holder, machine, run, `age_minutes` (since the run's `bops:claimed` comment), `idle_minutes` (the age `stale` measures), `last_note_minutes`, and the `branch` and `mr` when they can be inferred (as `claim --force`'s `previous` infers them; the forge is not asked). At a terminal it prints a table, `*` marking this machine; piped, JSON. `--mine` keeps to claims made on this machine, `--stale` to stale, orphan and `unknown` claims, and `--older-than` sets the window as for `stale`. It writes nothing; a comment or branch read that fails leaves its fields empty with a warning, and offline it lists no branches. A ref whose bead is closed, blocked or deferred is judged from the ref alone.

| Symptom | Cause | Do |
|---|---|---|
| `claim` exits 3 with `lost` | Another run holds the bead | Nothing to fix. Pick another bead; `holder` says who |
| `claim` exits 3 with `refused` | A precondition failed: assigned, held, landing, or not open | Pick another bead. If the reason says the holder is gone, see the stale rows below |
| exit 1, `outcome: unreachable` | The remote could not be reached | Follow `retry` (30 s, at most 3 times), then rerun the same command. A rerun settles anything half done; the claim comes back `claimed` or `held` |
| `note`, `land` or `release` failed (busy bead, rejected push, timeout) or its answer was lost | The write may or may not have landed | Rerun the same command with the same text; do not read the bead first. Each carries an operation id (`--op-id`, else derived from the command, bead, run and content) on its event comment, and a rerun that finds it writes nothing and answers the earlier outcome with `replayed: true`. Only the same run's comment counts. A replayed `note` still refreshes the claim, and answers `refused` or `lost` once the bead is no longer held; a `release` replays only once the hold is gone and with the same reason, and one still held runs in full; a replayed `release` or `land` pushes the ledger again. To write the same note twice on purpose, pass distinct `--op-id`s |
| `ledger_pending: true` or `ref_pending: true` | The local ledger write (a claim) or the ledger push or ref delete (a release) failed after the ref decided | `bops sync` until both are gone. The claim is valid meanwhile, unless the claim answered `refused` or failed with `code: ledger_conflict`: then it gave the bead back and only the ref's delete is pending (on a stuck ledger `bops sync` cannot finish it until the conflict is resolved; the next claim or release of the bead from this checkout does). A `release --reason done` keeps its hold, release pending, until `bops sync` (or the next command on the bead from this clone) has written its display and pushed it; a display written then carries no release text, which went to `saved_to` |
| `bops sync` keeps failing to push the ledger | Dolt push rejected or locked | `bd dolt commit`, `bd dolt pull`, `bd dolt commit` again, then `bd dolt push`, and read the error. A pull that merged an edit to the same bead can leave rows uncommitted that `bd vc status` does not show, and an interrupted command can leave them behind; "Nothing to commit." is fine. An exit 2 merge conflict is [MULTI-DEV-PLAN §6](MULTI-DEV-PLAN.md) |
| `code: no_origin` | No git remote has the lock remote's name (`lock.remote`, default `origin`) | `git remote -v`; add the remote, or set `lock.remote` in `.bops.json` (or `BOPS_LOCK_REMOTE`) to the remote beside the ledger |
| `code: remote_mismatch` | The lock remote is not the Dolt remote, or there is no Dolt remote (from `claim`, `sync`, `session-start`, `sweep`, or `bops doctor`'s `dolt_remote` check; `land` and `release` warn `no Dolt remote: ledger not published` instead) | Compare `git remote get-url <lock.remote>` with `bd dolt remote list`, and point that remote (or `lock.remote`) at the ledger's repository; with no Dolt remote, add one on that repository (`bd dolt remote add origin git+<url>`), or set `lock.mode: none` in `.bops.json` for a single-machine ledger |
| `code: ledger_conflict` | A ledger pull stopped on a merge conflict bd leaves for a person: on bd 1.3.4, a bead deleted on another machine while this clone changed its row. Every pull and push from this clone fails until it is resolved, so the command stops retrying at once; under `lock.mode ref` a `bops claim` (plain, `--next` or `--force`) that meets it gives back any claim ref it just created and writes nothing to the ledger; `bops status` (`ledger_conflict`, and a `ledger stuck:` warning) and `bops doctor` (a failed `ledger_conflict` check) report it, with the beads only this clone or only the remote has, until a pull succeeds | A person resolves it on that clone with the exit-2 runbook ([MULTI-DEV-PLAN §6](MULTI-DEV-PLAN.md#exit-2-runbook-merge-conflict)): `bd dolt pull --strategy theirs` keeps the delete (it can fail with a foreign key error), `--strategy ours` brings the bead back without its labels and comments (then `bd delete <id> --force` if the delete stands); then `bd dolt commit` and `bops sync` |
| `code: lock_unsupported` | The forge rejects pushes to `refs/bops/*` | Check the forge's ref rules or protections; there is no fallback in v0.1.0 |
| `code: state_write_failed` | The state directory is not writable | Fix permissions on `~/.local/state/bops/`. Nothing was pushed: the journal is written before the ref |
| `code: hold_not_recorded`, error names a sha | The ref was won but the state file was not written | Fix the state directory, then remove the ref as an orphan (below), using that sha |

**Who may take over what.** Every takeover is a person's decision; an autonomous loop never forces, resumes or deletes a claim. So agents only preview `reap` and `gc`: the generated skills list them as `preview` and tell the agent to leave the real run to a person, and the MCP tools refuse `dry-run: false` even under `--enable-writes`. The skills are instructions, not enforcement: an agent with a shell can still type the command, as it can `claim --force`. An agent that finds claims to release or refs to delete hands a person the `bops reap --dry-run=false --only <only>,<only>` command, built from each `would_release` entry's `only` (or `bops gc --dry-run=false --only <id>`), to run at the CLI. It does not build `<id>@<run>` from the `run` field: a run label is ledger text anyone with write access can set, so `only` pins a run only when it is shell-safe and is the bare id otherwise.

| Takeover | Who may | First |
|---|---|---|
| Your own bead, from another of your machines (`claim --resume`) | You | Push the branch and `bops note` on the old machine if you can |
| A stale claim of yours (`claim --force`) | You | Nothing: the old run is yours |
| Another person's stale claim (`claim --force`) | Any developer | Try to reach the holder (the actor in the ref), and wait if they answer |
| Stale claims that hold no visible work (`reap --dry-run=false --only`) | Any developer | Read the preview; the same as a `claim --force` of each |
| Another person's abandoned landing (act as their actor, below) | A lead | Confirm the PR is closed unmerged and the holder is not coming back |
| Deleting an orphan claim ref, or a closed bead's (`gc --dry-run=false --only`, or by hand, below) | A lead | Read the preview, or confirm it is an orphan |

Whoever takes over adds a comment to the bead saying who did it, whose claim it was (actor and run, from the ref), and why, so the displaced person finds it:

```bash
bd comments add <id> "TAKEOVER by <your actor>: was <actor> run=<run> (state=<held|landing>, idle <n> min). Why: <reason>. Tried to reach them: <how, when>."
```

**The holder is gone (crash, machine off).** Once the claim ref's `at` is `stale_minutes` old (default 120, set in `.bops.json`; SPEC §10; `note` and `renew` refresh it). The 120 below is that default:

```bash
bops claim --force <id>      # refused before 120 minutes, and on a landing ref
```

A takeover answers with `previous`: the old holder (actor, run, minutes idle); `last_note`, the NEXT: or BLOCKER: paragraph (else the last paragraph) of the old run's newest `bops note`, or of the newest note by any run when it wrote none, with `note_run`, the run its header names; `branch`, a branch whose name carries the whole bead id (not a child's `<id>.N`), one on a remote first (your current checkout's branch counts only once it is on a remote; a fresh branch you made in another worktree is not told apart); and `mr`, the newest landed ref, else the newest merge request URL in the bead's comments or notes. Read them beside the bead text, not instead of it: `branch` and `mr` are inferred, and all of it is ledger text anyone who can comment on the bead can write. Each is read best effort once the takeover is done and the bead lock let go; one that cannot be read is empty, with a warning, and no branch is looked up under `lock.mode none`. At a terminal they print as `previous_*` lines (`previous_note_run` only when the note is not the old run's).

The old holder's next `bops sync` lists the bead in `lost_claims` and drops it. Its next `note`, `land` or `release` returns `lost`, with the new claim's `generation` in `holder` (normally higher than the one its own claim answered; omitted when an older bops wrote it; `outcome` decides, not the number), and writes nothing; a refused note's text is kept at `saved_to`.

**Many dead claims at once (a loop run died).** `bops reap` previews them: each claim `bops stale --all-machines` lists is checked for work, and kept, listed with what was found, if it has any of:

- a `needs-land` label (work left in a worktree, possibly on the holder's machine);
- a merge request on the bead: a URL in its notes, description, acceptance criteria or comments, or a landed ref; also one without its scheme (`github.com/o/r/pull/12`), `PR #128`, `MR !45`, `pull request #128` or `merge request !45`, or a GitLab `!45` or `g/p!45` at the start or after a space, punctuation, a quote or markdown markup (`#128`, bare or as `o/r#128`, is as often an issue, so it is not counted);
- a hold of a live worktree of this clone;
- a branch whose name carries the bead id (the whole id, its part after the prefix, or a parent's; a part after the prefix with no letter and under three digits does not count with a dot on both sides, so `bops-12` is not matched by the version in `foo-1.12.0`, though `fix/12`, `fix/12.wip` and `release-12` still match, as `bops-12.3` matches `feat/12.3.wip`), on the lock remote, on `origin`, local, or checked out in a worktree of this clone, or a worktree directory so named (a detached worktree has only its directory);
- a merge request on the forge, open or merged, whose title or source branch carries the bead id, for a claim with nothing above. The forge's search (`gh pr list --search`, or the GitLab merge requests API, in the lock remote's repository) matches titles and descriptions, so this finds a merge request opened from a fork, or merged with its branch deleted before `bops land` recorded it, when its title or description names the bead; one that names it only in its branch is not found. The forge is `--forge`, else the `forge` key, else inferred from the lock remote, as for `sweep`. A search that fails, takes over 20 s or returns a full page of 100 without a match keeps the claim (`forge unanswered`); after one that fails or times out the run searches no more, keeps every later claim with no other evidence, and warns: without a working `gh` or `glab`, pass `--forge none` to release on the other evidence alone. With no forge nothing is searched and a warning says so. A merged merge request keeps the claim on every run, so a bead reopened after one merged needs a person's `claim --force`;
- no bops run at all: a claim made with plain `bd` (no `run:` label, no claim ref) is aged from the ledger only, so it is kept unless `--only` names it.

Claims with none are listed under `would_release`, each with `same_machine`. Releasing them is the same takeover as above and needs the same decision: pass the previewed claims with `--only`, so a claim that went stale after the preview is not released unseen. At a terminal the preview prints each as `<id>@<run>` (the JSON gives it as each entry's `only`); pass that form to pin the holder too, so a claim someone else took over since the preview (and left idle) is listed under `skipped` rather than released. A bare id releases whoever holds the claim then. Only a shell-safe run is pinned (A-Z, a-z, 0-9 and `.` `_` `:` `-`; bops runs are ULIDs): a claim whose run is anything else, or has no run (empty or `-`, as a claim made with plain `bd`), prints as the bare id, so a run label someone wrote with `;`, `$` or backticks is never pasted into your shell. The terminal line then ends `run not pinnable, check its holder first`: do that before passing its bare id. A bead id that is not a valid one is offered as nothing to paste: `only` is absent (an agent skips that entry) and the terminal prints the id single-quoted with a note. The real run pulls the ledger, lists the branches and reads the evidence again before each `claim --force`, and once more after it (searching the forge again); work that appears then keeps the claim with your run, listed under `failed` with what to run once you decide. A claim refreshed, released or taken over by another run (another `reap` racing this one, say) since the real run read it is refused and listed under `skipped` with why; nothing was left undone, so it makes the run exit 3 only when `--only` named it. One no longer stale, or not reached before the command's time ran out, is listed under `skipped` too; so is one whose evidence read or forge search the command's deadline cut short (and the rest after it), rather than kept as unread: run reap again. If the pull or the first branch listing fails, nothing is released; a later listing failure stops the run and lists the rest under `skipped`. An empty `--only` is refused rather than read as every claim. Each released bead carries `bops:hold` for its cooling-off and a note naming the old holder. The takeover and release of each claim run under one bead lock, so a `bops sync` in this clone cannot act between them, and the claim ref is deleted only once the release's ledger push has landed: if it fails, the release is reported with `(pending: run bops sync from this checkout)`, the run exits 3 and the ref stays, so no machine sees the ref gone while the remote ledger still shows the old holder; the next `bops sync` (not `--no-push`), or any command on the bead from this clone, pushes and then deletes it. A real run refuses a Dolt remote on another repository than the lock remote's, as `claim` does. Branches on other remotes, such as a fork, count as of their last fetch. It cannot see worktrees on other machines or in other clones on this one, so work left uncommitted there is not evidence; check the holder's machine first if you can. At a terminal it prints why each claim was kept and what to do about each one it could not finish.

```bash
bops reap                                     # preview: would_release and kept
bops reap --dry-run=false --only <id>@<run>,<id>    # release those previewed claims, as the preview printed them
```

**Moving your own bead to another machine.** The claim belongs to the run that took it, on its machine, so a plain claim from your other machine is refused. On the old machine, if you can: push the branch and `bops note <id>` with a `NEXT:` paragraph. On the new machine, from a worktree on that branch:

```bash
bops claim --resume <id>     # only the ref's actor, only from a different machine
```

The old run is displaced as by a takeover: its next `bops sync` lists the bead in `lost_claims`, and its next `note`, `land` or `release` returns `lost`. Work it never pushed stays on that disk. On the same machine `--resume` refuses a held bead: work in the worktree that holds it, or release it there.

**You were displaced** (`lost` from `note`, `land` or `release`, or the bead in `lost_claims`). Stop working on that bead. Push your branch, and put what you learned where the new holder will see it: a comment on the bead, or the text from `saved_to`.

**A ledger claim with no ref.** The assignee and `run:` label are set but there is no claim ref: a raw `bd update --claim`, a claim made before the lock, or a ref deleted with no new holder. A plain claim refuses it and `--force` waits 120 minutes from `updated_at`. When the ref of a `bops` claim is deleted and nobody takes the bead, the holder's next `bops sync` returns it to the pool by itself and lists it in `lost_claims`. For a raw `bd` claim, or a holder whose clone is gone, if the assignee is you, clear the display yourself instead of waiting:

```bash
bd update <id> --assignee '' --status open --remove-label run:<ulid>
bops sync                                   # publish it
```

**A stranded hold** (`stranded_holds` from `sync`). A worktree was removed while it held a bead. Release it from any worktree of the same clone:

```bash
bops release <id> --reason scope
```

**An abandoned landing.** The bead was released `done`, its PR was closed unmerged, and its holder is not coming back. `claim --force` refuses a landing ref and `sweep` leaves it pending. A lead reads the actor from the ref (above), then acts as that actor, from any machine:

```bash
BEADS_ACTOR='<actor from the ref>' bops claim --resume <id>
BEADS_ACTOR='<actor from the ref>' bops release <id> --reason scope
```

The ref is deleted and the bead goes back to the pool behind the usual `bops:hold`. The actor is not authenticated ([SPEC §3.0](../SPEC.md#30-the-claim-lock-ownership)), so this is a deliberate act: add the takeover comment above.

**An orphan ref** (`state=held` naming a run with no claim on the ledger, for example after `hold_not_recorded`). `bops stale` lists it under `orphans` once it is older than `--older-than`. A plain claim reports `lost` to that run. An orphan need not be dead: a claim whose ledger write has not reached this ledger (`ledger_pending`) looks the same from another machine, which is why `bops gc` keeps one whose bead shows work. First confirm it is an orphan, because deleting a live ref takes the bead from its holder:

```bash
git fetch origin "+refs/bops/claims/<id>:refs/bops/seen/<id>"
git log -1 --format=%B refs/bops/seen/<id>   # state=held, and the run is not one you can reach
bd show <id> --json                           # no assignee, no run: label for that run
```

**Many leftover refs at once (beads closed with plain `bd close`).** A bead closed with `bd close` instead of `bops sweep` keeps its claim ref, held or landing, on the remote. `bops gc` previews every ref it would delete: the ref of each closed bead, and each orphan `bops stale --all-machines` lists (a bead missing from the ledger counts). It never deletes a ref whose bead is not closed and carries a claim by that run or actor, in progress or parked `blocked` or `deferred` (a parked one is listed under `kept`: `stale` lists its ref as an orphan, but it is held), nor a landing ref of a bead in progress, which is `sweep`'s. A closed bead's ref younger than `stale_minutes` and written later than 15 minutes before the bead's last ledger change is kept, since it may be a claim of a reopened bead whose ledger write is pending, on a machine whose clock differs. Before deleting an orphan's ref, or a closed bead's ref written after the bead's last ledger change (however old: a reopened bead's claim whose ledger write has not arrived), it reads the bead's work evidence as `bops reap` does: a `needs-land` label, a merge request on the bead, a live hold of this clone, a branch naming the bead, and, when none of those shows, an open or merged merge request the forge finds (`--forge`, default `auto`). A ref whose bead shows work is listed under `kept` with what was found. A forge search that fails or does not answer keeps the ref too, with a warning naming `--forge none`, as does evidence that cannot be read; a branch listing that fails lists the ref under `failed`. A closed bead's landing leaves evidence for good (its merge request, its branch), so such a closed bead's ref, once kept, stays until a lead deletes it by hand (below). From another machine only remote branches and the forge are visible, so a claim that pushed no branch and neither noted nor renewed within `stale_minutes` can still be deleted, as `reap` would release it. A ref whose bead cannot be read, for any reason but bd's `no issues found`, is left and listed under `failed` as `unread`. The real run refuses a Dolt remote on another repository than the lock remote's. The real run pulls the ledger and fetches the refs first, deleting nothing if either fails; then, under each bead's lock, it re-reads the bead from the ledger it pulled, this clone's holds and, where it applies, the work evidence on a fresh branch listing (the real run asks the forge only here, once per ref), keeps a ref that is no longer garbage, whose bead shows work, or that a hold here names (listed under `kept`), and deletes with a lease on the sha it read, so a ref re-taken meanwhile is left alone (listed under `failed`). It writes nothing to the ledger. Afterwards `bops stale` lists no orphans.

```bash
bops gc                                     # preview: would_delete and kept
bops gc --dry-run=false --only <id>,<id>    # delete those previewed refs
```

For a single orphan, either wait 120 minutes and `bops claim --force <id>`, or delete it now with a lease on the sha you read, so a ref that changed in the meantime is left alone:

```bash
git push --no-verify --force-with-lease=refs/bops/claims/<id>:$(git rev-parse refs/bops/seen/<id>) origin :refs/bops/claims/<id>
```

Never delete or rewrite a claim ref with `-f`: it cancels the lease.

**Never on a `bops` ledger:** `bd reclaim` (by hand or on a timer) and `bd update --claim`, which both ignore the lock; and an older `bops` on any machine. In Claude Code, the opt-in [`guard-hooks.json`](../bindings/claude-code/guard-hooks.json) hook refuses the first two (and `bd ready --claim`) before they run, so the rule does not rest on the agent remembering it (§9.1).

---

*Sources: upstream `plugins/beads/skills/beads/SKILL.md` and `resources/{WORKFLOWS,RESUMABILITY,ISSUE_CREATION,DEPENDENCIES,MOLECULES,CHEMISTRY_PATTERNS,ASYNC_GATES,AGENTS,WORKTREES,PATTERNS,INTEGRATION_PATTERNS}.md`, `.claude-plugin/plugin.json`, `.codex-plugin/hooks/hooks.json`, `.beads/formulas/beads-release.formula.toml`, `docs/getting-started/ide-setup.md`, README at commit cbfdafc (2026-11-01). Policy decisions are mine and marked as such by being here rather than there.*

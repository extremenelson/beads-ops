# Beads Operating Process

**Status:** v0.3 draft (v0.2: §3.6 workability table, §3.7 pre-claim lint; v0.3: the `bops` claim lock in §1, §2.2, §3.7, §8, §9.3 and the recovery runbook in §12) — opinionated process layer over the upstream beads skill (gastownhall/beads, plugin v1.3.2 / skill v0.60.0, 2026-11-01).
**Scope:** one human (Alex) + multiple agents (Claude Code, Hermes, Kestrel) sharing one issue graph per repo, across several machines that are sometimes offline.
**Relationship to upstream:** this document does not replace `bd prime` or the skill's `resources/`. It fixes the decisions upstream leaves open. Where upstream and this file disagree on *mechanics*, upstream wins (it tracks the binary). Where they disagree on *policy*, this file wins.

---

## 0. The three invariants

Everything else in this document is derived from these.

1. **The graph is the plan.** No markdown TODO lists, no `PLAN.md`, no `MEMORY.md`. If it is work, it is a bead. If it is a durable project fact, it is `bd remember`. If it is neither, it belongs in the conversation and is allowed to die with it.
2. **Notes are written for a reader with zero context.** Every `in_progress` bead carries a note that a fresh agent — or Alex after two weeks — can resume from without the conversation. This is the only thing that makes compaction, crashes, and machine-hopping survivable.
3. **`blocks` means *cannot proceed*, nothing softer.** Only `blocks` affects `bd ready`. Everything that is sequencing preference, context, or provenance uses `related`, `parent-child`, or `discovered-from`. An over-constrained graph collapses `bd ready` to one item and kills parallelism — which is the whole point of having more than one agent.

---

## 1. Roles

| Actor | Owns | Never does |
|---|---|---|
| **Alex (human)** | `bd init`, epic-level goals, priority (P0–P1), resolving `human` gates, deciding what gets distilled into a formula, `bd dolt push` policy | Hand-edit `.beads/`; create P2–P4 tasks that agents could file themselves |
| **Agent (any harness)** | Decomposition below the epic, claiming, notes, discovered-from filing, closing with reasons, `bd gate check` at session start | `bd init`; change P0/P1 priority; close an epic; resolve a `human` gate; delete beads; run `bd dolt push --force` |
| **Autonomous loop** (task-agent, Kestrel worker) | `bops next` → `bops claim` → execute → close → repeat (`bd ready --claim` on a ledger without `bops`) | Pick up anything with label `needs-human`; create epics; work when `bd blocked` shows a `human` gate on the current epic |

`bd ready --claim` and `bd update --claim` compare and set on one replica only: two machines can both claim one bead, and Dolt's last-write-wins settles it later without telling the loser. `bops claim` closes that gap with a git ref per bead (§2.2). The guarantee covers only claims made through `bops`. `/bead-loop` and the Kestrel binding still claim through raw `bd`, so they are outside it until bead `bops-ecs` moves them.

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

Then **report before acting**: "N in progress (ids), M ready. Last note on `<id>`: NEXT = …. Continue with that?" In an unattended run, skip the question, pick the highest-priority `in_progress` item assigned to this actor, else the highest-priority `ready` item, and say which you chose in your first line of output.

Rule: **in_progress beats ready.** Finish or hand off before claiming new work. Two half-done beads are worth less than one closed one.

### 2.2 Claim

```bash
bd show <id> --long                   # read description, design, acceptance, notes, deps
bops claim <id>                       # the lock: one git ref per bead, then assignee + in_progress
```

Never start editing files before the claim lands. In a multi-agent setup the claim *is* the lock, and on a `bops` ledger the lock is the claim ref, not the assignee. Act on the exit code:

| Exit | `outcome` | Do |
|---|---|---|
| 0 | `claimed`, `held` | Work. `ledger_pending: true` only means the assignee shows up after `bops sync` |
| 3 | `lost`, `refused` | Do not work. Pick another bead; `holder` or `reason` says why |
| 1 | `unreachable` and the rest | Stop. Rerun when the remote is reachable; the result's `side_effects` says what may have been written |

Claiming needs the network: the lock lives on the remote. Notes work offline (§8). A ledger without `bops` keeps `bd update <id> --claim`, with the one-replica limit above.

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

Agents close tasks and features. **Only Alex closes epics**, after confirming the parent outcome — not merely that all children are closed.

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
| 0 | Production down / data at risk / blocks everything | Alex |
| 1 | Blocks the current epic's ready front | Alex (agent may propose in notes) |
| 2 | Default for planned work | Agent |
| 3 | Nice to have, no deadline | Agent |
| 4 | Backlog / someday | Agent |

Discovered bugs default to P2 unless they block the current work, in which case the agent sets P1 **and says so in the note**.

### 3.5 Labels (small, fixed vocabulary)

`needs-human` · `spike` · `template` (reserved by bd for protos) · `security` · `cost` · `infra` · `<machine-name>` (for work that must run on a specific box, e.g. `gpu-3090`)

Anything else needs a one-line justification in `bd remember`.

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
| 8 | Not labeled `needs-human` *(policy)* | Autonomous loops must skip these. |

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

Kestrel's `claim!` and any Hermes claim step should shell out to it the same way. Today Kestrel's `claim!` ends in `bd update --claim`, which bypasses the lock (§1).

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
| Needs Alex's sign-off (deploy, spend, external comms, anything irreversible) | `bd gate create --type human --blocks <id> --reason "…"` |
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

- **Embedded mode (default)** on every machine: `.beads/embeddeddolt/`, single writer per machine. Right choice for one human + sequential agents per box.
- **Sync** is `bd dolt pull` at session start (when online) and `bd dolt push` at session end. The remote is `refs/dolt/data` on the git remote — no extra service. Dolt does cell-level merge; hash ids (`bd-a1b2`) make concurrent creates collision-free.
- **Offline** is fully supported: everything in §2 works without network. The only thing deferred is sync. Note in the handoff when you were offline (`REF: offline, not pushed`) so the next session pulls before claiming.
- **Server mode** (`bd init --server`, external `dolt sql-server`) only if two agents need to *write concurrently on the same machine*. Hermes + Kestrel on one box, each in its own session, is still sequential enough for embedded. Revisit if claims start failing.
- **Per-machine work:** label it (`gpu-3090`, `debian-box`). Autonomous loops filter `bd ready --json` by their own machine label plus unlabeled work.
- **Worktrees:** `bd worktree create .worktrees/<name> --branch feature/<name>` shares the repo's `.beads/` automatically; one worktree per concurrently active agent. `bd where` when anything looks wrong. Each worktree is its own `bops` run, so two agents on one clone cannot both hold a bead.
- **The claim lock** (`bops` ledgers) is `refs/bops/claims/<id>` on the same remote as `refs/dolt/data`; `bops claim` refuses if git `origin` and the Dolt remote differ. Claim, release, land and sweep need the network. `note` works offline and refreshes the lock when it can. `bops sync` fetches every claim ref, repairs the assignee display from them, finishes deletes and ledger writes that failed, and reports `lost_claims` (someone took a bead from you) and `stranded_holds` (a worktree that no longer exists).
- **Upgrades:** every machine on a ledger runs a `bops` that knows the lock before anyone claims with it; an older binary ignores the lock. Do not run `bd reclaim` on a `bops` ledger, by hand or on a timer: it ignores the lock.
- **Local record:** each clone keeps `$XDG_STATE_HOME/bops/<hash>/events.jsonl` (default `~/.local/state/bops/…`), one line per step and outcome. Read it first when an outcome needs explaining.

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

**`.claude/settings.json` (project):** merge [`bindings/claude-code/settings.json`](../bindings/claude-code/settings.json), which adds sync, `bd gate check` and the `in_progress` report to the upstream hooks. If you use `bops`, merge [`settings.bops.json`](../bindings/claude-code/settings.bops.json) in its place.

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
| `claim!` | §2.2 — runs `bd-lint-claim.sh --json` first; refuses on fail. Claims with `bd update --claim`, so it is outside the `bops` lock until bead `bops-ecs` switches it to `bops claim` |
| `handoff!` | §4 — Malli-validated note |
| `discovered!`, `block!`, `gate-human!` | §5, §6 |
| `close!` | §2.5 — refuses epics |
| `sync!` | §8 — exit 3 tolerated, offline returns a map |

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
| Agent closes an epic | Agent conflated "children closed" with "outcome achieved" | Only Alex closes epics |
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
- **Kestrel skill** is a draft. It needs to run against the installed `bd` once, with the `--json` shapes checked for `ready`, `list`, `show`, and `create`.
- **Cross-repo capabilities** (`bd ship`, `external:` deps). Not needed until the venture work and the home-lab work live in separate repos and depend on each other. They may.
- **Compaction automation in Claude Code.** Upstream re-primes on `PreCompact` but does not write notes. Whether to automate the handoff note or keep it a discipline is open; try the discipline first.

---

## 12. Recovery runbook (`bops` ledgers)

Every procedure here was run once against two clones of a scratch ledger and real `bd` 1.3.4 on 2026-11-10. The two-machine run for `bops/v0.1.0` repeats them across machines. The claim ref is ownership and the ledger is its display (§2.2), so most stuck states are fixed by reading the ref and then running one `bops` command.

**Read a claim.** Who holds a bead, in what state, since when:

```bash
git fetch origin "+refs/bops/claims/<id>:refs/bops/seen/<id>"
git log -1 --format=%B refs/bops/seen/<id>     # run=, actor=, state=held|landing, at=
```

`bops sync` fetches every claim ref the same way. The clone's own record of what it did is `~/.local/state/bops/*/events.jsonl`. `bops stale`, which will list stale claims and orphans in one place, is not built yet (bead `bops-ss6.4`).

| Symptom | Cause | Do |
|---|---|---|
| `claim` exits 3 with `lost` | Another run holds the bead | Nothing to fix. Pick another bead; `holder` says who |
| `claim` exits 3 with `refused` | A precondition failed: assigned, held, landing, or not open | Pick another bead. If the reason says the holder is gone, see the stale rows below |
| exit 1, `outcome: unreachable` | The remote could not be reached | Follow `retry` (30 s, at most 3 times), then rerun the same command. A rerun settles anything half done; the claim comes back `claimed` or `held` |
| `ledger_pending: true` or `ref_pending: true` | The ledger push or ref delete failed after the claim decided | `bops sync` until both are gone. The claim is valid meanwhile |
| `bops sync` keeps failing to push the ledger | Dolt push rejected or locked | `bd dolt pull`, then `bd dolt push`, and read the error. An exit 2 merge conflict is [MULTI-DEV-PLAN §6](MULTI-DEV-PLAN.md) |
| `code: remote_mismatch` or `no_origin` | git `origin` is not the Dolt remote | Compare `git remote get-url origin` with `bd dolt remote list` and point `origin` at the same repository |
| `code: lock_unsupported` | The forge rejects pushes to `refs/bops/*` | Check the forge's ref rules or protections; there is no fallback in v0.1.0 |
| `code: state_write_failed` | The state directory is not writable | Fix permissions on `~/.local/state/bops/`. Nothing was pushed: the journal is written before the ref |
| `code: hold_not_recorded`, error names a sha | The ref was won but the state file was not written | Fix the state directory, then remove the ref as an orphan (below), using that sha |

**The holder is gone (crash, machine off).** Once the claim ref's `at` is 120 minutes old (`note` refreshes it):

```bash
bops claim --force <id>      # refused before 120 minutes, and on a landing ref
```

The old holder's next `bops sync` lists the bead in `lost_claims` and drops it. Its next `note`, `land` or `release` returns `lost`; a refused note's text is kept at `saved_to`.

**You were displaced** (`lost` from `note`, `land` or `release`, or the bead in `lost_claims`). Stop working on that bead. Push your branch, and put what you learned where the new holder will see it: a comment on the bead, or the text from `saved_to`.

**A ledger claim with no ref.** The assignee and `run:` label are set but there is no claim ref: a raw `bd update --claim`, a claim made before the lock, or a ref deleted with no new holder. A plain claim refuses it and `--force` waits 120 minutes from `updated_at`. If the assignee is you, clear the display yourself instead of waiting:

```bash
bd update <id> --assignee '' --status open
bd label remove <id> run:<ulid>
bops sync                                   # publish it
```

**A stranded hold** (`stranded_holds` from `sync`). A worktree was removed while it held a bead. Release it from any worktree of the same clone:

```bash
bops release <id> --reason scope
```

**An abandoned landing.** The bead was released `done`, its PR was closed unmerged, and its holder is not coming back. `claim --force` refuses a landing ref and `sweep` leaves it pending. Read the actor from the ref (above), then act as that actor, from any machine:

```bash
BEADS_ACTOR='<actor from the ref>' bops claim --resume <id>
BEADS_ACTOR='<actor from the ref>' bops release <id> --reason scope
```

The ref is deleted and the bead goes back to the pool behind the usual `bops:hold`. The actor is not authenticated ([SPEC §3.0](../SPEC.md#30-the-claim-lock-ownership)), so this is a deliberate act: say who did it and why in a comment on the bead.

**An orphan ref** (`state=held` naming a run with no claim on the ledger, for example after `hold_not_recorded`). A plain claim reports `lost` to that run. First confirm it is an orphan, because deleting a live ref takes the bead from its holder:

```bash
git fetch origin "+refs/bops/claims/<id>:refs/bops/seen/<id>"
git log -1 --format=%B refs/bops/seen/<id>   # state=held, and the run is not one you can reach
bd show <id> --json                           # no assignee, no run: label for that run
```

Then either wait 120 minutes and `bops claim --force <id>`, or delete it now with a lease on the sha you read, so a ref that changed in the meantime is left alone:

```bash
git push --no-verify --force-with-lease=refs/bops/claims/<id>:$(git rev-parse refs/bops/seen/<id>) origin :refs/bops/claims/<id>
```

Never delete or rewrite a claim ref with `-f`: it cancels the lease.

**Never on a `bops` ledger:** `bd reclaim` (by hand or on a timer) and `bd update --claim`, which both ignore the lock; and an older `bops` on any machine.

---

*Sources: upstream `plugins/beads/skills/beads/SKILL.md` and `resources/{WORKFLOWS,RESUMABILITY,ISSUE_CREATION,DEPENDENCIES,MOLECULES,CHEMISTRY_PATTERNS,ASYNC_GATES,AGENTS,WORKTREES,PATTERNS,INTEGRATION_PATTERNS}.md`, `.claude-plugin/plugin.json`, `.codex-plugin/hooks/hooks.json`, `.beads/formulas/beads-release.formula.toml`, `docs/getting-started/ide-setup.md`, README at commit cbfdafc (2026-11-01). Policy decisions are mine and marked as such by being here rather than there.*

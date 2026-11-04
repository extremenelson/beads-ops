# ADR 0002 — `bops` is a single manifest-driven CLI with a Go reference implementation

- **Status:** Accepted, 2026-11-04 (language decision supersedes the Babashka/TypeScript lean in the options evaluation)
- **Scope:** Where the intent-level commands over beads live, how harness bindings are produced, and what language the reference implementation uses
- **Source:** "Intent command layer options" evaluation, 2026-11-04 (inventory of three overlapping projects); [`SPEC.md`](../../SPEC.md) §1, §8, §11a

## Context

Three projects had entered the same space: a Compassion-owned workflow repo
with the only *working* intent commands (shell, GitLab-shaped, work-for-hire),
beads-ops (a policy layer with one lint script and two unverified bindings),
and a prose-only skill library. The same readiness filter existed four ways
across bash, jq, Clojure and shell, and two vocabularies collided (*handoff* =
keep the claim and write state vs *handback* = return the claim).

Stated consumers: Claude Code (primary; hooks need a sub-100 ms, zero-daemon
executable), Hermes, Kestrel (Clojure), Codex, and later a read-only MCP
server. The result must be open source for a broad audience, and the author
decided on a **spec-level re-implementation** rather than reuse of employer-owned
code: spec first with dated public commits, a different language, no
employer-specific labels, forge verbs or repo names.

## Decision

1. **One harness-agnostic CLI, `bops`, lives in beads-ops.** ~13 intent-level
   commands (`next claim note release land sweep stale lint sync session-start
   gates deps version`), `--json` everywhere, exit 0 verdict / 1 could-not-run /
   2 usage, contract-versioned (`bops version` prints `contract=<n>`).
2. **A JSON manifest (`bops/commands.json`) is the single source of truth** for
   names, args, output schemas, side effects and surfaces. Every harness
   binding — Claude Code skill + hooks, Kestrel Clojure fns with Malli schemas,
   Hermes tool spec, MCP tool list, `docs/COMMANDS.md` — is **generated** from
   it. Adding an intent = one manifest entry + one implementation + one test.
3. **Go is the reference implementation**: a single static binary (incl.
   `android/arm64` for Termux), stdlib only unless justified in CONTRIBUTING.md,
   `bd --json` shell-out behind one seam (`internal/bdx`) so the beads Go library
   can replace it later without touching callers, manifest embedded so a binary
   and its bindings can never disagree.
4. Vocabulary is fixed in SPEC §2: `note` (keep the claim), `release` (give it
   back with a classified reason), `land` (record evidence, never merge); only
   `sweep` or a human closes.
5. Forge operations sit behind a two-function adapter (`is_merged`, `ref_sha`;
   SPEC §7). Ledger-side commands never call a forge.

## Options considered

### Where the commands live

| # | Option | Verdict | Why |
|---|---|---|---|
| A | **CLI + manifest-generated bindings** (chosen) | **Accepted** | Only option serving all consumers; follows beads' own guidance (CLI + hooks over MCP for token cost, state outside the DB, writes via `bd`); gives the ADR 0001 read model a place to land without consumer changes |
| B | Claude Code plugin only | Rejected as home, kept as a *binding* | Locks semantics into one harness; the plugin is a generator output of A, so nothing is lost by starting with A |
| C | MCP server first | Rejected as core, kept as last binding | 10–50k tokens of tool definitions vs ~1–2k for a skill; 25k-token result cap; hooks cannot be served by MCP |
| D | Extend the employer-owned shell scripts in place | Rejected | Cannot become the open-source asset; GitLab-shaped; 2.8k–9.3k-line POSIX sh is past the point of adding graph queries |
| E | Kestrel-native only | Rejected for now | Hooks need a zero-daemon binary; Kestrel should *import* bops, not *be* it |
| F | Fork `bd` / custom tables | Rejected | See ADR 0001 |

### Reference-implementation language

| # | Option | Verdict | Why |
|---|---|---|---|
| 1 | **Go** (chosen) | **Accepted** | Matches the beads ecosystem and its contributors; can open the ledger once via the beads Go library instead of paying embedded-Dolt startup per `bd` call; single static binary on every target including Termux; different language from the employer-owned code makes clean-room provenance obvious |
| 2 | TypeScript / Bun | Runner-up | Fastest for the author, npm distribution; loses on per-command latency across 3–4 chained `bd` calls and on `bun build --compile` targets |
| 3 | Babashka | Rejected | Native to Kestrel, but bb on Termux/Android is not first-class and a broad audience will not read EDN; Kestrel calls the binary instead |
| 4 | POSIX sh | Rejected (hook shims only) | Already proven unmaintainable at scale in option D |

## Switching conditions

- **B (plugin only):** if, 90 days in, Claude Code is still the only harness
  intents are run from. The `cc` generator output already *is* the plugin.
- **C (MCP first):** if the dominant consumer becomes shell-less (Claude
  desktop / Cowork, a Hermes build without exec). Still generated from the
  manifest.
- **E (Kestrel-native):** once Kestrel is the stable hub and the ADR 0001 read
  model is in — by then `bops` is a thin shell over Kestrel's namespaces, the
  intended end state.
- **TypeScript instead of Go:** if contributors who show up are TS people and
  Go becomes a barrier, or if the beads Go library proves unusable and the
  latency argument evaporates. The spec is language-neutral by design.

## Consequences

- SPEC.md §11a records the Go-specific constraints; CI fails if
  `docs/COMMANDS.md` drifts from the manifest.
- `bindings/kestrel/beads.clj` (never run) and `scripts/bd-lint-claim.sh` are
  superseded by generated code and `bops lint` respectively; retirement is
  tracked in the v0.1.0 plan.
- The IP posture is procedural: dated spec commits, no copied code, different
  language, and the provenance note in SPEC §13. The author is not a lawyer;
  the employment agreement controls.

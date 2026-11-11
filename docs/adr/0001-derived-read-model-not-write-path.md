# ADR 0001 — A graph layer over beads is a derived read model, never a write path

- **Status:** Accepted, 2026-11-04. Amended 2026-11-10 for the claim lock (see [Amendment](#amendment-2026-11-10-a-git-ref-claim-lock))
- **Scope:** Any index, cache, or graph store that beads-ops (or a consumer such as Kestrel) keeps alongside the beads ledger
- **Source:** [`docs/research/2026-11-graph-read-model.md`](../research/2026-11-graph-read-model.md) (four research tracks, synthesized 2026-11-04)

## Context

beads-ops wants queries that `bd` does not offer: transitive blockers across the
four blocking edge types, critical path, parallel tracks (connected components of
the open blocking subgraph), formula soundness, and — most important for a
multi-machine team — stale-claim detection, because bd ≥ 1.3 leases and
heartbeats are deliberately node-local and never reach Dolt.

Beads itself is no longer JSONL + SQLite. Dolt became the default in v0.50.0
(2026-02) and the SQLite backend was removed in v0.58.0 (2026-03); as of v1.3.4
`.beads/issues.jsonl` is a passive export. Dolt already provides history,
cell-level merge, and cross-machine sync over `refs/dolt/data`. Upstream tried
and abandoned the "canonical file + local cache" two-store design; the only
lineage that still runs it (the `br` Rust port) needs three-way merge,
tombstones and explicit reconciliation to keep the stores aligned. Upstream
guidance is "keep extension state outside the beads database and connect it
to beads through stable CLI surfaces", and the roadmap's "Beads Graph Preview 2"
(BDP) means upstream is building its own graph store.

## Decision

1. Any graph or index beads-ops maintains is a **rebuildable projection**: seeded
   from `bd export --all`, kept current by tailing the beads events journal, and
   fully re-projected after every `bd dolt pull`, `bd bootstrap`, or journal gap.
   `rebuild = truncate + replay`; drift check = rebuild into a temp store and diff.
2. **All writes go through `bd`** (CLI, `bd serve` HTTP, or the Go library) with
   `--if-revision` / `--if-assignee` compare-and-set where a write is conditional.
   No direct SQL writes, no custom tables, no second sync fabric. (Amended: the
   claim lock is one narrow exception. See below.)
3. Durable shared state lives in the ledger as labels, comments, and
   `metadata` keys (SPEC §3), except claim ownership (amended, see below). Per-machine state (watermarks, journal cursors,
   lease shadows) lives in `$XDG_STATE_HOME/bops/<repo-hash>/`, never in the
   projection and never in beads.
4. `bd ready` remains the arbiter of what is claimable. The projection answers
   analytic questions; it does not re-implement upstream readiness policy.
5. The projection is owned by a **single process** (the orchestrator); every
   other consumer reaches it through `bops --json` or that process's API.

In `bops` this surfaces as the `--engine` switch on `deps` and `stale`
(`bd` today, `cte` next, `graph` reserved) — consumers never know which engine
answered.

## Options considered

| # | Option | Verdict | Why |
|---|---|---|---|
| 1 | **Derived read model** (chosen): projection from export + events journal, writes only via `bd` | **Accepted** | Only design that does not re-solve merge, hash-ID assignment, cycle rejection, close policy and lease arbitration that `bd` already performs on write; retirable the day BDP serves issues |
| 2 | Graph DB as source of truth, `bd` as a view | Rejected | No embeddable graph engine is mergeable (LMDB, columnar, Arrow files are opaque binaries); would reinvent Dolt sync and collide with BDP |
| 3 | Write-through cache (graph and Dolt both written by bops) | Rejected | Two write paths racing; exactly the failure mode upstream hit with JSONL + SQLite and designed away |
| 4 | Custom tables inside the beads Dolt database | Rejected | Upstream calls direct-storage custom tables a legacy SQLite-only pattern; version-pinning pain across a fleet |
| 5 | GUN / CRDT sync layer (asked 2026-11-04) | Rejected | Solves replication, which Dolt owns, with weaker semantics (per-property LWW, no CAS) and no query engine; JS-only |
| 6 | No projection at all: `bd sql` + recursive CTEs + Kahn's algorithm in code | Viable fallback | Zero new dependencies, adequate to tens of thousands of nodes; this is `--engine cte` |

### Engine choice within option 1 (informational, not binding)

Ranked for a Clojure-hub stack: Datalevin (EPL-2.0, Datalog rules, LMDB,
read-only MCP) → no engine (CTEs + code) → Datascript (in-memory first
iteration) → graphqlite (Cypher without a JVM, TypeScript side) → LadybugDB
(the live Kùzu continuation; only past ~100k edges). Excluded: Kùzu
(archived 2025-10), CozoDB (dormant since 2023), Memgraph / FalkorDB /
SurrealDB (BSL/SSPL), Neo4j embedded (GPL, single-instance).

## Switching conditions

- **Choose option 6 outright** (no engine) if the Clojure orchestrator is not
  the hub of the tooling within ~90 days, or if queries stay fixed-depth and
  single-edge-type. Nothing in `bops`' contract changes.
- **Revisit this ADR** if upstream BDP starts serving issues and dependencies
  (currently "unchanged, not served over BDP in v0") — the projection then
  becomes a BDP client or is deleted.
- **Never** promote the projection to a write path, whatever the engine. If a
  write-side need appears, the answer is a `bd` feature request or a hook. The
  claim lock is the one exception, and it is not a projection. See below.

## Consequences

- `bops` carries no graph dependency in contract 1; `deps` and `stale` run on
  `bd` output, with a `cte` engine planned (plan task 9).
- Cross-machine stale-claim detection is implemented by bops over the synced
  fields (`status`, `assignee`, `updated_at`, `bops:*` comments) — this is the
  highest-leverage gap upstream leaves open and does not need a graph engine.
- Two assumptions remain to be tested empirically and are tracked as beads, not
  settled here: whether events-journal sequence numbers survive `bd dolt pull`
  across machines (design assumes not), and the exact `bd ready` effect of
  `parent-child` on the pinned bd version.

## Amendment 2026-11-10: a git ref claim lock

**Why.** `--if-assignee` and `bd update --claim` compare and set on one replica
only. In a two-replica test on bd 1.3.4 both replicas claimed one bead, and
Dolt's last-write-wins then settled the assignee, so an agent told it had
succeeded lost the bead and nothing told it so. Decision 2's compare-and-set
cannot prevent that across machines, and `bd` offers no cross-replica form.

**Decision.** Claim ownership moves to one git ref per bead,
`refs/bops/claims/<id>`, on the remote the project already uses, written with a
create-only or leased push. The design is
[`docs/designs/bops-claim-lock-v0.1.0.md`](../designs/bops-claim-lock-v0.1.0.md).

This changes three statements above:

- **Decision 2.** The lock is a second write path, for ownership only. It adds no
  server or new service. It carries no issue data: a ref holds only the run,
  actor, state, a nonce and a timestamp. Every issue write still goes through `bd`.
- **Decision 3.** Ownership is no longer ledger state. The ref decides who holds a
  bead. The assignee and the `run:` and `bops:landing` labels become a best-effort
  display for people and for tools reading raw `bd`.
- **The "never" switching condition.** The rule against promoting the projection
  stands. A `bd` feature request remains the preferred answer to a write-side
  need; the lock exists because the loop cannot wait for one.

**Retirement.** Delete the lock the day `bd` offers cross-replica compare-and-set.
Ownership then returns to the ledger and this amendment is withdrawn.

**Limits.** The guarantee covers claims made through `bops`. A raw `bd update
--claim` bypasses the lock, and until they are moved so do `/bead-loop` and
Kestrel. How often claims really collide is unmeasured, and the two-machine run
for v0.1.0 records a count. The race itself is reproduced, which is why the lock
is built before that number exists.

**Verified.** The two-replica suite (SPEC §9) runs in CI against real `bd`
1.3.4: in 40 races with distinct actors and 40 with one shared actor, every
trial ended with exactly one `claimed` and one `lost` (2026-11-10). Forty clean
trials bound the failure rate to about 7.5%; the guarantee itself rests on the
remote's leased push, measured separately on GitHub.

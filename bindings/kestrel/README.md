# Kestrel binding (Clojure / Babashka)

**Status: draft. Not yet run against a live `bd`.** Field names match upstream `internal/types/types.go` and `docs/reference/json-schema.md` at the pinned commit; `--json` shapes for `ready`, `list`, `show`, `create` still need one real run to confirm.

Kestrel treats `bd --json` as its durable task memory and never touches `.beads/` directly. `beads.clj` wraps the CLI (~120 lines); the value is in the schemas and the resume contract, not the plumbing.

| Contract point | Function | Notes |
|---|---|---|
| 1 Session start | `session-start` | runs `bd sync` (tolerates 3), `bd gate check`, lists in_progress + ready, filters `needs-human` and machine labels, picks resume target; **throws if an in_progress bead has no `NEXT`** |
| 2 Claim | `claim!` | shells to `scripts/bd-lint-claim.sh --json` first; refuses on exit 1. Claims with `bd update --claim`, which bypasses the `bops` claim lock: two machines can both claim a bead. Outside the lock's guarantee until bead `bops-ecs` switches it to `bops claim` (exit 3 = pick another bead) |
| 3 Handoff note | `handoff!` | Malli-validated `Handoff` map → `--append-notes` in the §4 format |
| 4 Stop | `sync!`, `close!` | `close!` refuses epics; `sync!` returns `{:offline true}` instead of throwing |

## Integration

- Integrant component `:kestrel/beads` holding `{:actor "kestrel@<host>" :machine-label "<host>" :lint-path "scripts/bd-lint-claim.sh"}`; `session-start` runs in init and seeds the first prompt.
- core.async loop: `ready → claim! → run steps → handoff! on every :step/complete → close!`. A step that would do something irreversible calls `gate-human!` and parks the channel.
- Kestrel's SQLite stays for its *own* runtime state (channel offsets, tool-call logs). Anything a human or another agent needs to see goes to beads. Don't mirror.

## To verify

- [ ] `bd ready --json` / `bd list --json` item shape (expect `id title status priority issue_type labels assignee parent`)
- [ ] `bd show --json` returns array; `dependencies[].{issue_id,depends_on_id,type}`; `gated_by`
- [ ] `bd create --json` returns object with `id`
- [ ] `bd config get claim.pools` output format
- [ ] Non-zero exit → stderr JSON `{error, code}`; adjust `bd` fn to surface `code`

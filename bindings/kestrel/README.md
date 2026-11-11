# Kestrel binding (Clojure / Babashka)

**Status: draft. Not yet run against a live `bd` or `bops`.** Field names match upstream `internal/types/types.go` and `docs/reference/json-schema.md` at the pinned commit; `--json` shapes for `ready`, `list`, `show`, `create` still need one real run to confirm.

Kestrel treats `bd --json` as its durable task memory and never touches `.beads/` directly. `beads.clj` wraps the CLI (~220 lines), and claims through `bops claim` so the claim lock covers it; the value is in the schemas and the resume contract, not the plumbing.

| Contract point | Function | Notes |
|---|---|---|
| 1 Session start | `session-start` | runs `bd sync` (tolerates 3), `bd gate check`, lists in_progress + ready, filters `needs-human` and machine labels, picks resume target; **throws if an in_progress bead has no `NEXT`** |
| 2 Claim | `claim!` | shells to `scripts/bd-lint-claim.sh --json` first; throws `bead not workable` on fail. Then claims with `bops claim <id> --json`, inside the claim lock. Returns `{:outcome :claimed}` or `:held` (you own it), `:lost` with `:holder {:actor :run :age_minutes}`, or `:refused` with `:reason` (exit 3: pick another bead). Throws `bops claim could not run` with `:code` (`unreachable`, `git_auth`, `lock_unsupported`, `no_origin`, `remote_mismatch`, ...) and `:error` when `bops` exits 1 or 2. `parse-claim` is the pure mapping |
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
- [ ] `bops claim --json` against a live ledger: `claimed`, then `lost` from a second clone, then a refusal

## Test

`parse-claim` and `claim!`'s branching are tested without `bd` or `bops` (needs Babashka; Malli comes from Maven):

```bash
bb -Sdeps '{:deps {metosin/malli {:mvn/version "0.16.1"}}}' bindings/kestrel/beads_test.clj
```

## Known gap: the rest of the loop is still raw `bd`

Only the claim goes through `bops`. `close!` (`bd close`) and `block!` (`bd update --status blocked`) do not release the claim, so after them the bead's claim ref (`refs/bops/claims/<id>`) and this run's hold stay in place until `bops release` is run. The `bops` equivalents are `bops land` and `bops release --reason done` then `bops sweep` (instead of `close!`), and `bops release --reason blocked|human` (instead of `block!` and `gate-human!`); `sync!` has `bops sync` and `session-start` has `bops session-start`.

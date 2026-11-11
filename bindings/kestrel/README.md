# Kestrel binding (Clojure / Babashka)

**Status: draft. Not yet run against a live `bd` or `bops`.** Field names match upstream `internal/types/types.go` and `docs/reference/json-schema.md` at the pinned commit; `--json` shapes for `ready`, `list`, `show`, `create` still need one real run to confirm. The `bops` result shapes come from `bops/commands.json` and the Go output structs, not from a live run.

Kestrel treats `bd --json` as its durable task memory and never touches `.beads/` directly. `beads.clj` wraps the CLI (~310 lines). Everything that touches ownership goes through `bops`, so the claim lock covers it: `claim!`, `close!`, `block!`, `gate-human!` and `sync!`. The value is in the schemas and the resume contract, not the plumbing.

| Contract point | Function | Notes |
|---|---|---|
| 1 Session start | `session-start` | runs `sync!` (`bops sync`), `bd gate check`, lists in_progress + ready, filters `needs-human` (and the legacy `needs:human` until bops v0.2.0, SPEC §3.1) and machine labels, picks resume target; **throws if an in_progress bead has no `NEXT`** |
| 2 Claim | `claim!` | shells to `scripts/bd-lint-claim.sh --json` first; throws `bead not workable` on fail. Then claims with `bops claim <id> --json`, inside the claim lock. Returns `{:outcome :claimed}` or `:held` (you own it), `:lost` with `:holder {:actor :run :age_minutes}`, or `:refused` with `:reason` (exit 3: pick another bead) |
| 3 Handoff note | `handoff!` | Malli-validated `Handoff` map → `--append-notes` in the §4 format |
| 4 Stop | `close!`, `block!`, `gate-human!`, `sync!` | below |

### Stop: give the claim back through `bops`

| Function | `bops` calls | Returns | Then, with raw `bd` |
|---|---|---|---|
| `(close! cfg bead {:ref url :sha sha :verify? bool})` | `bops land <id> --ref --sha [--verify]`, then `bops release <id> --reason done` | the release map with `:land` added; or the land map when land is `:lost`/`:refused` (no release then) | nothing |
| `(block! cfg id reason)` | `bops release <id> --reason blocked` (status → blocked) | the release map | on `:released` only: `bd update --append-notes "BLOCKER: <reason>"` |
| `(gate-human! cfg id reason)` | `bops release <id> --reason human` (reopens, `+needs-human`) | the release map | on `:released` only: `bd gate create --type human --blocks <id> --reason <reason>` |
| `(sync! cfg)` | `bops sync` | `{:synced true :pulled :pushed :lost-claims :stranded-holds ...}`, or `{:synced false :offline true :code ..}`; never throws | nothing |

Common to `claim!`, `close!`, `block!` and `gate-human!`:

- `:lost` (someone else holds the bead, `:holder` names them) and `:refused` (a precondition failed, such as this run not holding the bead; `:reason` says which) are returned, not thrown, and write nothing to the ledger. Stop working that bead.
- When `bops` exits 1 or 2 they throw `bops <command> could not run`, with the stable `:code` (`unreachable`, `git_auth`, `no_origin`, `remote_mismatch`, ...), `:error`, `:exit`, `:retry` and `:side-effects` in `ex-data`.
- `parse-outcome` is the pure mapping from exit code and `--json` document to that map, shared by `claim`, `release` and `land` (`parse-claim`, `parse-release`, `parse-land` are its three uses); `parse-sync` is `sync!`'s. A released map carries `:ref-pending` and `:ledger-pending` (`bops sync` finishes either); `:release-reason` is the reason it was released with.

**`close!` no longer closes the bead.** SPEC §2 lets only `bops sweep` or a human close; a worker lands and releases `done`. So `close!` now needs the PR/MR URL and commit, not a close reason, and returns the release map instead of the new ready front. After it returns `:released`, this run holds nothing: the claim ref stays on the remote as `state=landing` (that is what `release --reason done` does, SPEC §3.0), the bead stays `in_progress` with label `bops:landing`, and `bops sweep` deletes the ref and closes the bead once the PR is merged. Epics are still refused before any write.

`block!` and `gate-human!` release first and write their `bd` note or gate only after `:released`, so a displaced holder writes nothing. `bops release` in contract 1 takes no text, so the `BLOCKER:` note stays a `bd` write; `bops` has no gate command, so the human gate does too. The `needs-human` label is `bops`'s own write, the spelling PROCESS.md and `scripts/bd-lint-claim.sh` row 8 use. If one of those `bd` writes fails after the release, it throws and the bead is released without it.

`session-start` stays on `bd`: `bops session-start` returns ids and a ready count, while this binding returns the in_progress and ready beads themselves, filters machine labels and checks the resume target's `NEXT`.

## Integration

- Integrant component `:kestrel/beads` holding `{:actor "kestrel@<host>" :machine-label "<host>" :lint-path "scripts/bd-lint-claim.sh"}` (`:bops-path` optional); `session-start` runs in init and seeds the first prompt.
- core.async loop: `ready → claim! → run steps → handoff! on every :step/complete → open the PR → close!`. A step that would do something irreversible calls `gate-human!` and parks the channel. Run `bops sweep --dry-run=false` (or let a human) to close landed beads after their PRs merge.
- Kestrel's SQLite stays for its *own* runtime state (channel offsets, tool-call logs). Anything a human or another agent needs to see goes to beads. Don't mirror.

## To verify

None of the `bops` paths has been run against a live `bd` and `bops`; the tests below stub `bops` and `bd`.

- [ ] `bd ready --json` / `bd list --json` item shape (expect `id title status priority issue_type labels assignee parent`)
- [ ] `bd show --json` returns array; `dependencies[].{issue_id,depends_on_id,type}`; `gated_by`
- [ ] `bd create --json` returns object with `id`
- [ ] `bd config get claim.pools` output format
- [ ] Non-zero exit → stderr JSON `{error, code}`; adjust `bd` fn to surface `code`
- [ ] `bops claim --json` against a live ledger: `claimed`, then `lost` from a second clone, then a refusal
- [ ] `close!` against a live ledger: `landed` then `released`, the ref left as `state=landing`, no hold in `bops sync`'s state; `bops sweep` closes it after the merge
- [ ] `block!` and `gate-human!` against a live ledger: ref deleted, status and labels as above, note / gate written; `lost` from a second clone writes nothing
- [ ] `sync!` output from a live `bops sync`, online and offline

## Test

The parsers and the branching of `claim!`, `close!`, `block!`, `gate-human!` and `sync!` are tested without `bd` or `bops` (needs Babashka; Malli comes from Maven):

```bash
bb -Sdeps '{:deps {metosin/malli {:mvn/version "0.16.1"}}}' bindings/kestrel/beads_test.clj
```

# testdata

- `golden/` — one file per `bops gen` target. Regenerate with
  `go test ./internal/gen -update` and review the diff as a contract change.
- Fixtures used by `internal/intent` tests are inline and **synthetic**: they
  copy the field names of `bd 1.3.x --json` output (`ready`, `list`, `show`)
  with invented content. Never commit a real ledger export here.

## bd JSON shapes we rely on (bd 1.3.2, raw mode)

- `bd ready --json`, `bd list … --json`: a JSON array of issue objects.
- `bd show <id> --json`: a one-element array.
- Failures: `{"error": "...", "hint": "...", "schema_version": 1}` on stdout, non-zero exit.
- Issue fields read: `id title description acceptance_criteria status priority
  issue_type assignee owner labels notes parent dependencies[] {issue_id
  depends_on_id type} created_at updated_at started_at lease_expires_at heartbeat_at`.

## Verified bd syntaxes (bd 1.3.4)

Run against a throwaway `bd init` ledger by `BOPS_INTEGRATION=1 go test ./internal/intent`.

- `bd update <id> --claim` claims atomically on this replica. It refuses
  `--if-assignee` and `--if-status` (`cannot combine … with --claim`).
- `bd update <id> --assignee '' --status open|blocked` clears the assignee and
  sets status in one call.
- `bd label add|remove <id> <label>`.
- `bd comments add <id> <text>`, or `-f <file>` for text from a file.
- `bd comments <id> --json`: array of `{id, issue_id, author, text, created_at}`.
  There is no `bd comments list`.

## Git ref lock on GitHub (git 2.56.0, 2026-11-09)

Two clones pushing at once to a private GitHub repository, with
`git push --porcelain --no-verify origin --force-with-lease=<ref>:<expected> <sha>:<ref>`.

| Case | Trials | Exactly one winner | Loser's porcelain summary |
|---|---|---|---|
| Create the same ref | 40 | 40 | `[remote rejected] (cannot lock ref '<ref>': reference already exists)` |
| Replace with the same lease (takeover) | 20 | 20 | `[remote rejected] (cannot lock ref '<ref>': is at <sha> but expected <sha>)` |

One-at-a-time pushes:

| Case | Result |
|---|---|
| Create when the ref exists | `[rejected] (stale info)`, exit 1 |
| Replace, correct lease | `(forced update)`, exit 0 |
| Replace, wrong lease | `[rejected] (stale info)`, exit 1, ref unchanged |
| Delete, correct lease | `[deleted]`, exit 0 |
| Delete, wrong lease | `[rejected] (stale info)`, exit 1, ref unchanged |
| Delete, ref already gone | `[rejected] (stale info)`, exit 1 |
| Create with an identical object | `[up to date]`, exit 0, so both sides "win" |

Rules for the lock code:

- Never pass `-f` with `--force-with-lease`. `-f` cancels the lease, and a wrong lease then succeeds (reproduced).
- Make every lock commit unique, because of the identical-object row.
- A race loser's reason differs from a local bare repo, which says `(reference already exists)`, so classify by re-reading the ref.
- A failed delete can mean the ref is already gone. Re-read it before treating that as a failure.

## Open: bd 2.0 JSON envelope

bd warns that `--json` output changes in 2.0 (`BD_JSON_ENVELOPE=1` opts in
early; see upstream `docs/reference/json-schema.md`). `internal/ledger`
decodes raw arrays and error objects only. Envelope support lands with a
pinned fixture captured from `BD_JSON_ENVELOPE=1 bd ready --json`; until
then `bops` must run with the envelope off.

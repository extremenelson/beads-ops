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

## Open: bd 2.0 JSON envelope

bd warns that `--json` output changes in 2.0 (`BD_JSON_ENVELOPE=1` opts in
early; see upstream `docs/reference/json-schema.md`). `internal/ledger`
decodes raw arrays and error objects only. Envelope support lands with a
pinned fixture captured from `BD_JSON_ENVELOPE=1 bd ready --json`; until
then `bops` must run with the envelope off.

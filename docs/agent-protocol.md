# Agent protocol: beads-ops

Shared conventions: `docs/team-workflow/reference-agent-dispatch.md` in the
development-collaboration repo, format `v:1` of its `reference-protocol-file.md`. Follow
them; this file adds only what is ours. The day-to-day loop for this repo is the `bops`
skill (`.claude/skills/bops/SKILL.md`) and [PROCESS.md](PROCESS.md).

- **Gate:** from `bops/`: `test -z "$(gofmt -l .)"`, `go vet ./...`, `go test ./...`,
  `go run ./cmd/bops gen docs | diff - ../docs/COMMANDS.md` and
  `sh ../scripts/check-bindings.sh`. The two-replica suite (`BOPS_INTEGRATION=1`, a pinned
  `bd`) runs in CI. A docs-only change needs only the last two.
- **Isolation:** `bd worktree create .worktrees/<id> --branch work/<id>`, then `bops claim`
  from inside it, because a bops claim belongs to the worktree that made it. No
  per-worktree setup. Stale-check match: `^refs/heads/work/<id>$`.
- **Landing path:** a branch and a GitHub pull request that a human merges; no auto-merge.
  Agents cannot land here: `/land-mr` opens, finds and comments on merge requests with
  `glab` only, and this repo is on GitHub, so `/bead-loop` stops before claiming. Land by
  hand with the `bops` skill: push, `gh pr create --base main`, then
  `bops land <id> --ref <pr-url> --sha <sha> --verify` and `bops release <id> --reason done`.
  Commit subjects end with the bead ID in parentheses; a ledger export commit names the
  beads it follows instead.
- **Carve-outs:** changes to the contract (`SPEC.md`, `bops/commands.json`), CI
  (`.github/`), agent instructions (`AGENTS.md`, `CLAUDE.md`, `.claude/`, `.agents/`) and
  this file. Every pull request here waits for a human merge anyway.
- **Carve-outs block dispatch:** yes.
- **Blocking labels:** none.
- **Max agents:** none.
- **Loop auto-merge:** none.
- **Claim tool:** bops. This ledger's claims go through `bops claim`; raw
  `bd update --claim` bypasses the claim lock and is never used here (PROCESS §12).
- **GitLab issue sync:** not configured.
- **Close contract:** do not close the bead at hand-off. `bops land --verify` records the
  pull request on the bead and `bops release --reason done` leaves it landing. After a human
  merges, a post-merge session runs `bops sweep --dry-run=false`, which closes it once the
  forge reports the pull request merged, and then commits the ledger export (PROCESS §8).
  Epics and beads with no pull request are closed by a lead with
  `bd close <id> --reason "…"`.

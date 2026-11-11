# Claude Code binding (reference)

| Contract point | Mechanism | Verified |
|---|---|---|
| 1 Session start | `settings.json` `SessionStart` hooks: `bd sync` → `bd prime` → `bd gate check` → `bd list --status in_progress --json` | SessionStart/PreCompact shapes match upstream `plugins/beads/.claude-plugin/plugin.json`. The added commands are plain shell. |
| 2 Claim | `CLAUDE.md.snippet` policy + `bdclaim` alias through `bd-lint-claim.sh` | Policy text only; relies on the model following it. The lint itself is tested against a stub `bd`. |
| 3 Handoff note | `CLAUDE.md.snippet` policy; `PreCompact` re-primes but cannot write notes for you | Policy text only. |
| 4 Stop | `settings.json` `Stop` hook runs `bd sync` | **Hook name unverified** against current Claude Code — check `claude --help` / hooks docs; `SessionEnd` may be the right event on your version. |

## Install

```bash
bd setup claude                                      # upstream: hooks + CLAUDE.md section
cat bindings/claude-code/CLAUDE.md.snippet >> CLAUDE.md
# merge bindings/claude-code/settings.json into .claude/settings.json (jq -s '.[0] * .[1]' works if you have no other hooks)
cp scripts/bd-lint-claim.sh ./scripts/
alias bdclaim='f(){ scripts/bd-lint-claim.sh "$1" && bops claim "$1"; }; f'
# a ledger without bops: ... && bd update "$1" --claim  (holds on one replica only)
```

## Using bops

`skills/bops/SKILL.md` and `settings.bops.json` drive the same contract through `bops` instead of raw `bd`. Both are hand-written until the generator ships (`bops gen cc`).

```bash
go install github.com/extremenelson/beads-ops/bops/cmd/bops@v0.1.0   # or: go install ./cmd/bops from bops/
mkdir -p .claude/skills && cp -R bindings/claude-code/skills/bops .claude/skills/
# use settings.bops.json in place of settings.json when merging into .claude/settings.json
```

There is no Stop hook yet, because `bops sync` runs a fetch, pull and push and its cost on every turn has not been measured. The skill tells the agent to run it before stopping.

## Autonomous dispatch

Upstream ships `agents/task-agent.md` (ready → claim → execute → close loop). To run it under this process, filter `needs-human` (and the legacy `needs:human` until bops v0.2.0, SPEC §3.1) and machine labels before dispatch:

```bash
bd ready --json \
 | jq -r --arg m "$(hostname -s)" '.[] | select((.labels // []) | (index("needs-human") or index("needs:human")) | not)
     | select(((.labels // []) | map(select(startswith("gpu-") or startswith("host-"))) | length == 0)
              or ((.labels // []) | index($m)))
     | .id' \
 | head -1 \
 | xargs -I{} sh -c 'scripts/bd-lint-claim.sh {} --json >/dev/null && claude -p "Work bead {}: $(bd show {} --json)"'
```

Run each dispatched agent in its own worktree (`bd worktree create .worktrees/{} --branch work/{}`) if more than one runs at once.

## Compaction

`PreCompact` re-injects `bd prime` but does not save your notes. The discipline is: when the conversation feels heavy, write the handoff note *before* compaction fires. If you want it automated, the Codex plugin's `PostCompact` + `UserPromptSubmit` refresh-marker pattern (in `vendor/beads-skill/.codex-plugin/hooks/hooks.json`) is the shape to copy; Claude Code doesn't ship it upstream.

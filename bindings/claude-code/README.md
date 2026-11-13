# Claude Code binding (reference)

| Contract point | Mechanism | Verified |
|---|---|---|
| 1 Session start | `settings.json` `SessionStart` hooks: `bd sync` → `bd prime` → `bd gate check` → `bd list --status in_progress --json` | SessionStart/PreCompact shapes match upstream `plugins/beads/.claude-plugin/plugin.json`. The added commands are plain shell. |
| 2 Claim | `CLAUDE.md.snippet` policy + `bdclaim` alias through `bd-lint-claim.sh` | Policy text only; relies on the model following it. The lint itself is tested against a stub `bd`. |
| 3 Handoff note | `CLAUDE.md.snippet` policy; `PreCompact` re-primes but cannot write notes for you | Policy text only. |
| 4 Stop | `settings.json` `Stop` hook runs `bd sync` | `Stop` is a Claude Code hook event, seen firing on this maintainer's sessions (2026-11). It runs after **every** reply, not once per session, so this costs one `bd sync` per turn; use `SessionEnd` instead if you only want it at exit. |

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

`bops session-start --hook-json` replaces upstream's SessionStart `bd prime` hook (remove that one if `bd setup claude` installed it; keep the PreCompact one). It prints what this worktree holds, lost and stranded claims, the stale and workable counts and a trimmed `bd prime`, wrapped in Claude Code's hook envelope, and always exits 0. This repo uses the same hook in its own [`.claude/settings.json`](../../.claude/settings.json).

There is deliberately no Stop hook: a full `bops sync` fetches, pulls and pushes, measured at about 3 s (2026-11-10), and Stop fires after every reply. The skill tells the agent to run `bops sync` before stopping instead.

Before the first claim, run `bops doctor` on each machine ([how-to](../../docs/howto-claim-lock.md)). New to `bops`? The [tutorial](../../docs/tutorial-first-loop.md) runs the loop by hand first.

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

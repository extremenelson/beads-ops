# Harness bindings

`docs/PROCESS.md` is harness-neutral. A *binding* is the small amount of config or code that makes one coding agent or orchestrator actually follow it. The contract is four points; a binding documents which it covers and how.

## The contract

| # | Point | What the harness must do | Where it's specified |
|---|---|---|---|
| 1 | **Session start** | Run `bd sync` (tolerating exit 3), `bd prime`, `bd gate check`, `bd list --status in_progress --json`; report before acting; *in_progress beats ready* | PROCESS §2.1 |
| 2 | **Claim** | Never edit files before the claim lands: `bops claim <id>` (exit 0 → work; 3 → pick another; 1 → stop), or `bd update <id> --claim` on a ledger without `bops`, which holds on one replica only; route the claim through `scripts/bd-lint-claim.sh` (exit 0 → claim; 1 → fix or pick another) | PROCESS §2.2, §3.7 |
| 3 | **Handoff note** | At each milestone and before any stop, `bd update <id> --append-notes` in the `COMPLETED / IN PROGRESS / NEXT / BLOCKER` format; `NEXT` is one concrete step | PROCESS §4 |
| 4 | **Stop** | Write the handoff note, then `bd sync` if online; never close an epic; never resolve a `human` gate | PROCESS §2.4, §1 |

Anything beyond these four (slash commands, autonomous loops, worktree dispatch) is nice-to-have and binding-specific.

## Bindings

| Harness | Status | Covers |
|---|---|---|
| [`claude-code/`](claude-code/) | **Reference.** Maintainer uses daily | 1 (hooks), 2–4 (CLAUDE.md policy + lint alias) |
| [`hermes/`](hermes/) | Maintainer's stack; hook surface unverified | 1 via AGENTS.md, 2–4 via policy block |
| [`kestrel/`](kestrel/) | Maintainer's Clojure orchestrator; draft, not yet run against live `bd` | 1–4 programmatically |
| Aider, Cursor, Codex, OpenCode, Gemini CLI, Copilot CLI, Kiro… | **Wanted** | `bd setup <harness>` gives you point 1; the rest is a policy block in the file that harness reads |

## Writing a new binding

1. Run `bd setup --list` and `bd setup <harness>` — upstream already writes the instruction file and, where the harness supports it, the session-start hook. Don't redo that.
2. Append the policy block from `claude-code/CLAUDE.md.snippet` to whatever file your harness reads (`AGENTS.md`, `.cursor/rules/*.mdc`, `GEMINI.md`, …). It's harness-neutral text.
3. If the harness has a hook surface, add `bd sync` before `bd prime` at session start and a stop hook that runs `bd sync`. If it doesn't, say so in your README and make the policy block carry it ("before you stop, run…").
4. If the harness can shell out before a claim, wire `bd-lint-claim.sh`. If not, say so.
5. `README.md` in your directory: which of the four points are covered, by what mechanism, and what you have actually run.

Open a PR. Unverified is fine as long as it's labeled.

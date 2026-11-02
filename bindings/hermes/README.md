# Hermes binding

**Status: maintainer's stack. Hook surface not yet verified — treat as a sketch.**

| Contract point | Mechanism | Verified |
|---|---|---|
| 1 Session start | Hermes reads `AGENTS.md`; upstream `bd init` writes the beads section there. If Hermes exposes a session-start hook, run `bd sync --json; bd prime; bd gate check; bd list --status in_progress --json` from it. If not, the policy block must carry the instruction as text. | **No** |
| 2 Claim | Policy block (same text as `../claude-code/CLAUDE.md.snippet`) appended to `AGENTS.md`; if Hermes's tool layer can shell out before a claim, call `scripts/bd-lint-claim.sh` | **No** |
| 3 Handoff note | Policy block | Text only |
| 4 Stop | Policy block; stop hook if one exists | **No** |

## Install (as currently understood)

```bash
bd init                                   # writes AGENTS.md beads section
cat ../claude-code/CLAUDE.md.snippet >> AGENTS.md   # harness-neutral policy text
cp -r ../../vendor/beads-skill/skills/beads <hermes-skills-dir>/beads   # progressive-disclosure skill tree
```

Don't duplicate the policy into a Hermes-specific file; `AGENTS.md` is the one Hermes reads and the one every other AGENTS-first harness reads too.

## To verify

- [ ] Does Hermes have a session-start / pre-turn hook? Where is it configured?
- [ ] Can a Hermes skill run a shell command conditionally before another tool call (for the lint-before-claim gate)?
- [ ] Does Hermes compact context, and if so is there a pre-compact event?

Answers go here and in the table above.

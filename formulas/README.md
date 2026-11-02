# Formulas

Distilled `bd` formulas (`*.formula.toml`) for workflows that have run **at least twice** as ad-hoc epics and came out the same shape both times. Writing a formula before the second run is speculative; `PROCESS.md` §7.

Copy into a project's `.beads/formulas/` and spawn:

```bash
bd mol pour <name> --var key=value      # persistent
bd mol wisp <name> --var key=value      # ephemeral (patrols, checks)
```

Format reference: `vendor/beads-skill/_upstream-extras/beads-release.formula.toml` — gate-aware, phase-split, with step descriptions written as agent prompts. That's the bar.

Planned (not yet distilled):

- `mol-onboard-machine` — `MULTI-DEV-PLAN.md` Phase 2 per-machine runbook
- `mol-weekly-review` — triage, priority check, stale-epic close, wisp gc
- `mol-bd-upgrade` — `MULTI-DEV-PLAN.md` §7 freeze → migrate → bootstrap

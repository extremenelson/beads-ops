---
description: Show the beads bops would serve next, without claiming one
---

Run `bops next --json` and list each candidate's `id`, `priority` and `title` in the order given, with its `why` if it has one. If `ok` is false or there are no candidates, say so and stop.

Do not claim a bead. Ask which one to start; starting it follows the bops skill (worktree first, then `bops claim` inside it).

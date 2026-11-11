# beads-sync systemd timer

User-level timer that runs `bd sync` every 15 minutes for one repo. For always-on machines (home servers, desktops that stay up). Laptops rely on session hooks instead.

```bash
mkdir -p ~/.config/systemd/user
cp beads-sync@.service beads-sync@.timer ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now beads-sync@myrepo.timer     # repo lives at ~/src/myrepo
systemctl --user list-timers 'beads-sync@*'
```

If the repo isn't under `~/src/`, override the working directory:

```bash
systemctl --user edit beads-sync@myrepo
# [Service]
# WorkingDirectory=%h/code/myrepo
```

Exit-code policy (see `bd sync --help`):

| exit | meaning | unit state |
|---|---|---|
| 0 | synced / nothing to do | success |
| 3 | push race or busy writer — transient | success (retried next tick) |
| 2 | merge conflict halted | **failed** — resolve by hand (`MULTI-DEV-PLAN.md` §6) |
| 4 | stuck dirty working set | **failed** — `bd vc status`, `bd doctor --fix` |
| 1 | transport/auth | failed |

Conflict rate for the team: `journalctl --user -u 'beads-sync@*' --since -7d | grep -c 'exit.*2'`.

On a ledger without `bops`, add a `bd reclaim --older-than 2h` to the same unit (second `ExecStart=` line) on one always-on machine per team so crashed agents' claims return to the pool. **Not on a `bops` ledger:** `bd reclaim` ignores the claim lock and would return beads someone still holds. There, a claim idle for 120 minutes is taken over with `bops claim --force` ([PROCESS §12](../../docs/PROCESS.md#12-recovery-runbook-bops-ledgers)).

Linger must be enabled for the timer to run when you're not logged in: `loginctl enable-linger $USER`.

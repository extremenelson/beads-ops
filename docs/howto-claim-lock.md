# How to set up the claim lock

Get `bops claim` to guarantee that only one agent, on any machine, holds a bead at a time, and confirm it with `bops doctor` before the first claim.

`bops` keeps ownership in one git ref per claimed bead, `refs/bops/claims/<id>`, on your git remote. A claim is a create-only push, so of two machines racing for one bead exactly one push succeeds. The ledger's assignee and `run:` label only display what the ref decided. [ARCHITECTURE.md](ARCHITECTURE.md#the-claim-lock) explains why; [SPEC §3.0](../SPEC.md#30-the-claim-lock-ownership) is the definition.

## Pick a mode first

| Your setup | Mode | Do this |
|---|---|---|
| More than one machine (or developer) shares the ledger | `ref` (the default) | Steps 1-4 below |
| One machine only, with or without a remote | `none` | [Single-machine ledger](#single-machine-ledger-lockmode-none) |

`lock.mode: none` only keeps worktrees of one clone apart. If a second machine might ever claim beads on this ledger, use `ref`.

## Prerequisites

- `bops` installed on **every** machine that will claim, from the same release. An older `bops`, or a raw `bd update --claim`, doesn't see the lock and can take a bead someone holds.
- `bd` 1.3 or later, the same version on every machine.
- A git remote that accepts pushes to custom refs under `refs/bops/` with `--force-with-lease`. GitHub has been measured. A forge that rejects them shows up as `lock_unsupported` in step 3.
- Push access to that remote from every machine.

## 1. Point the ledger at the same repository as the lock

The lock remote (`origin` unless you set [`lock.remote`](reference/configuration.md#lockremote)) must be the repository the Dolt ledger syncs with. Check both:

```bash
git remote get-url origin
bd dolt remote list
```

If `bd dolt remote list` shows nothing, add the Dolt remote on the same repository:

```bash
bd dolt remote add origin <the URL git printed>
```

`bd` stores it with a `git+` prefix (`git+https://…`, `git+ssh://…`, `git+file://…`). That's expected.

If your shared remote isn't called `origin`, tell `bops` in the committed `.bops.json`:

```json
{ "lock": { "remote": "upstream" } }
```

## 2. Publish the ledger

The other machines need the ledger before they can claim from it:

```bash
bd dolt push
```

On each other machine, clone the repo and run `bd bootstrap` ([MULTI-DEV-PLAN Phase 2](MULTI-DEV-PLAN.md#phase-2--onboard-every-developer-and-every-machine-15-minutes-per-machine)).

## 3. Run `bops doctor` on every machine

```bash
bops doctor
```

`doctor` runs five checks in order. It writes nothing to the ledger, and its one write to the remote is a probe ref that it deletes again.

| Check | What it proves |
|---|---|
| `origin` | the lock remote exists in this clone |
| `fetch` | this machine can read `refs/bops/claims/*` from it |
| `push` | the remote accepts a push from this account (dry run) |
| `lock` | a create-only push to `refs/bops/doctor/<nonce>` succeeds and a second one is refused, which is the property the lock depends on |
| `dolt_remote` | a Dolt remote points at the same repository |

A healthy run (`--json`, trimmed):

```json
{
  "ok": true,
  "failed": [],
  "checks": [
    { "id": "origin", "status": "ok", "detail": "origin is git@github.com:you/repo.git" },
    { "id": "fetch", "status": "ok", "detail": "read refs under refs/bops/claims/ from origin" },
    { "id": "push", "status": "ok", "detail": "origin accepts a push from this account (dry run, nothing written)" },
    { "id": "lock", "status": "ok", "detail": "wrote and deleted the probe ref refs/bops/doctor/1c4a781864c6 on origin: a create-only push succeeded and a second one was refused" },
    { "id": "dolt_remote", "status": "ok", "detail": "a Dolt remote is on the same repository as origin" }
  ]
}
```

Exit 0 means the lock will work from this machine. Any failure exits 1, and the failing check carries a `code` and a `next` line with the fix. Later checks are `skip` when an earlier one fails.

## 4. Prove the lock with two clones

Claim one bead from two machines (or two separate clones) at once. One wins:

```bash
# machine A
bops claim shr-d10 --json | jq '{outcome, ok}'
```

```json
{ "outcome": "claimed", "ok": true }
```

```bash
# machine B
bops claim shr-d10 --json; echo "exit=$?"
```

```json
{
  "command": "claim",
  "contract": 1,
  "id": "shr-d10",
  "ok": false,
  "outcome": "lost",
  "holder": { "actor": "alice", "age_minutes": 0, "run": "01M7AWP55YNR97RWZJ0TB1XQDK" },
  "run": "01M7AWP88BXMHA61GAF5EH0V83"
}
exit=3
```

Read the ref itself to see who holds the bead:

```bash
git fetch origin "+refs/bops/claims/shr-d10:refs/bops/seen/shr-d10"
git log -1 --format=%B refs/bops/seen/shr-d10
```

```
run=01M7AWP55YNR97RWZJ0TB1XQDK
actor=alice
state=held
nonce=28d281cb2d746f70b5c63f8c7cc0134a
at=2026-11-13T16:43:38Z
```

In zsh, write `${ID}` rather than `$ID` before a `:` in that refspec: zsh reads `$ID:r` as a filename modifier.

Release the test bead from machine A with `bops release shr-d10 --reason scope`.

## Single-machine ledger (`lock.mode: none`)

Add this to the committed `.bops.json`:

```json
{ "lock": { "mode": "none" } }
```

Then:

```bash
bops doctor --json | jq '[.checks[] | {id, status}]'
```

```json
[
  { "id": "origin", "status": "skip" },
  { "id": "fetch", "status": "skip" },
  { "id": "push", "status": "skip" },
  { "id": "lock", "status": "skip" },
  { "id": "dolt_remote", "status": "skip" }
]
```

Every check is skipped, naming `lock.mode`, and `doctor` exits 0. Every other command adds the warning `lock.mode is none: claims are decided by this machine's ledger alone, …`. With no Dolt remote, `release`, `land`, `sweep`, `sync` and `session-start` skip publishing and warn `no Dolt remote: ledger not published` instead of failing.

Only `.bops.json` can set `none`. A user config file or `BOPS_LOCK_MODE=none` is rejected unless the repo file already says `none` ([why](reference/configuration.md#lockmode)).

## Verification

- `bops doctor` exits 0 on every machine that will claim.
- A claim from a second clone of a bead that's already held returns `outcome: lost` and exits 3.
- `git ls-remote origin 'refs/bops/claims/*'` lists one ref per bead currently held or landing.

## Troubleshooting

These are the failures `doctor` reports, with the `next` text it prints. Stuck claims after setup (a crashed holder, an orphan ref, a stranded hold) are in the [PROCESS.md §12 recovery runbook](PROCESS.md#12-recovery-runbook-bops-ledgers).

| Failing check, `code` | Cause | Fix |
|---|---|---|
| `origin`, `no_origin` | No git remote has the lock remote's name | `git remote add origin <url>`, or set `lock.remote` (or `BOPS_LOCK_REMOTE`) to the remote beside the Dolt ledger |
| `fetch` or `push`, `git_auth` | The remote refused this machine's credentials | Fix SSH keys or the credential helper; `git ls-remote origin` should work without a prompt |
| `fetch` or `push`, `unreachable` | Network or host down | Retry once you can reach the remote |
| `lock`, `lock_unsupported` | The forge rejects pushes to `refs/bops/*` | Check branch/ref protection rules on the forge. There's no fallback in v0.1.0 |
| `dolt_remote`, `remote_mismatch` | No Dolt remote, or it points at a different repository | `bd dolt remote add origin git+<the lock remote's URL>`, or set `lock.mode: none` in `.bops.json` for a one-machine repo |
| Exit 1 before any check runs, `code: bad_config` | A bad `.bops.json`, user config or `BOPS_*` value | Fix the named file or variable ([validation](reference/configuration.md#validation)) |

Never run `bd reclaim` (by hand or on a timer) on a ledger that uses the lock. It ignores claim refs and would hand out beads someone still holds.

## Related

- [Configuration reference](reference/configuration.md): `lock.mode`, `lock.remote`, `stale_minutes`.
- [Command reference: `bops doctor`](COMMANDS.md#bops-doctor).
- [MULTI-DEV-PLAN.md](MULTI-DEV-PLAN.md): the full multi-machine rollout this step belongs to.

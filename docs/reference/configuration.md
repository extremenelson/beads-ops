# bops configuration reference

`bops` reads its settings from a committed `.bops.json`, an optional per-user file, and `BOPS_*` environment variables. Most repos need no config at all. The usual reasons to add one are a single-machine ledger (`lock.mode: none`), a claim lock on a remote not named `origin` (`lock.remote`), or a team-wide staleness window (`stale_minutes`).

The normative definition is [SPEC.md §10](../../SPEC.md#10-configuration). The loader is [`bops/internal/config/config.go`](../../bops/internal/config/config.go). This page describes the same rules with examples.

## Where settings come from

Lowest to highest precedence. A higher layer overrides only the keys it names.

| # | Layer | Location | Notes |
|---|---|---|---|
| 1 | Built-in defaults | in the binary | see the key table below |
| 2 | Repo file | `.bops.json` in the worktree root | committed; shared by every machine on the ledger |
| 3 | User file | `$XDG_CONFIG_HOME/bops/config.json`, else `~/.config/bops/config.json` | per user, every repo |
| 4 | Environment | `BOPS_<KEY>` | per shell or per process |
| 5 | Flags | e.g. `bops next --pool`, `bops stale --older-than` | per invocation |

A missing file is skipped. A file that exists must be one JSON object.

## Keys

| Key | Type | Default | Env var | Used by |
|---|---|---|---|---|
| [`stale_minutes`](#stale_minutes) | integer ≥ 1 | `120` | `BOPS_STALE_MINUTES` | `next` (W06s), `claim --force`, `stale`, `session-start` |
| [`hold_minutes`](#hold_minutes) | integer ≥ 1 | `120` | `BOPS_HOLD_MINUTES` | `next`, `lint` (W12) |
| [`pool`](#pool-and-caps) | string | none (default pool) | `BOPS_POOL` | every command's run |
| [`caps`](#pool-and-caps) | list of strings | none | `BOPS_CAPS` (comma-separated) | every command's run |
| [`forge`](#forge) | `auto` \| `github` \| `gitlab` \| `none` | `auto` | `BOPS_FORGE` | `land --verify`, `sweep` |
| [`lock.mode`](#lockmode) | `ref` \| `none` | `ref` | `BOPS_LOCK_MODE` | every command, `doctor` |
| [`lock.remote`](#lockremote) | git remote name | `origin` | `BOPS_LOCK_REMOTE` | claim-ref commands, `doctor`, forge inference |
| `lint.strict` | boolean | `false` | `BOPS_LINT_STRICT` | loaded and validated, **not applied yet**; use `bops lint --strict` |
| `lint.disable` | list of rule ids | none | `BOPS_LINT_DISABLE` | loaded and validated, **not applied yet** |
| `note.require_next` | boolean | `false` | `BOPS_NOTE_REQUIRE_NEXT` | loaded and validated, **not applied yet** |

Nested keys are nested JSON objects in a file (`{"lock": {"mode": "none"}}`). The environment name is `BOPS_` plus the key path in upper case, with `.` replaced by `_`.

### `stale_minutes`

How long a claim can sit idle before another run may take it over with `bops claim --force`. Age is measured from the claim ref's `at` time, which `bops note` refreshes. A claim with no ref is aged from the ledger's `updated_at`.

Set it in the committed `.bops.json` so every machine on a ledger agrees. A machine with a shorter window takes over claims sooner than its teammates expect.

### `hold_minutes`

How long a bead stays out of `bops next` after `bops release --reason abandoned` or `--reason scope` labels it `bops:hold`. A label has no timestamp, so the window runs from the bead's `updated_at`: any later write to the bead restarts it.

### `pool` and `caps`

`pool` makes a run serve only beads labelled `pool:<name>`. A run with no pool serves only unlabelled beads; a pooled run never takes default-pool beads (lint rule W13).

`caps` lists what this machine can do. A bead labelled `needs:<cap>` is workable only on a run whose `caps` include `<cap>` (W04). These are per-machine facts, so they usually belong in the user file or the environment, not in `.bops.json`. `bops next --pool` and `--caps` override both for one call.

### `forge`

Which forge `sweep` and `land --verify` ask whether a pull request merged. `auto` infers it from the host of `git remote get-url <lock.remote>`: `github` or `gitlab` by name, otherwise `none`. With `none`, `sweep` can't tell whether a PR merged and lists landing beads under `unknown`. `--forge` overrides it for one call.

### `lock.mode`

- `ref` (default): ownership is a git ref per bead, `refs/bops/claims/<id>`, on the lock remote. Two machines can never both hold a bead. Requires a git remote named by `lock.remote` and a Dolt remote on the same repository.
- `none`: ownership is the local ledger alone. For a ledger used on **one machine** only. Two worktrees of one clone are still kept apart; two machines are not. Every result carries the warning `lock.mode is none: …`, and `bops doctor` reports all its checks as `skip`.

**Only `.bops.json` can turn the lock off.** A user file or `BOPS_LOCK_MODE` that sets `none` is an error unless the repo file already sets it. One machine turning the lock off on a shared ledger would claim beads others hold without anyone being told. Any layer may set `ref`.

Change modes only when nothing is claimed. See [How to set up the claim lock](../howto-claim-lock.md).

### `lock.remote`

The git remote that holds the claim refs. It must be the same repository as the Dolt ledger's remote (`bd dolt remote list`); `claim` and `doctor` refuse with `remote_mismatch` otherwise. The value must look like a git remote name: a letter or digit, then up to 99 letters, digits, `.`, `_` or `-`, with no `..` and not ending in `.` or `.lock`.

## Other environment variables

| Variable | Effect | Default |
|---|---|---|
| `BEADS_ACTOR` | the actor written into claim refs and event comments | git `user.name`, then the OS user name |
| `BOPS_MACHINE` | the machine name in `bops:claimed` comments and `stale` output | the hostname |
| `XDG_STATE_HOME` | base of the per-machine state directory, `<base>/bops/<repo-hash>/` | `~/.local/state` |
| `XDG_CONFIG_HOME` | base of the user config file | `~/.config` |
| `BOPS_INTEGRATION=1` | tests only: run the live-ledger and two-replica suites | unset |
| `BOPS_RACE_TRIALS` | tests only: claim-race trial count | see `bops/cmd/bops/replica_test.go` |

`BEADS_ACTOR` is not authenticated. Anyone with push access to the remote can write a claim ref under any actor name. See [SPEC §3.0](../../SPEC.md#30-the-claim-lock-ownership).

## Validation

`bops` never ignores a bad setting. An unknown key, a wrong type, or an out-of-range value makes every command exit 1 before it does anything, naming the file or variable at fault. The JSON result carries `"code": "bad_config"`, so a binding can tell a config mistake from an unreachable remote without parsing the message. The messages below are real output:

```console
$ cat .bops.json
{"stale_minute": 60}
$ bops next
bops next: bad_config: config /path/to/repo/.bops.json: unknown key "stale_minute" (have: pool, caps, hold_minutes, stale_minutes, forge, lint.strict, lint.disable, note.require_next, lock.mode, lock.remote) (did NOT run to completion)

$ BOPS_STALE_MINUTES=0 bops next
bops next: bad_config: config BOPS_STALE_MINUTES: stale_minutes: want at least 1, got 0 (did NOT run to completion)

$ cat ~/.config/bops/config.json
{"lock":{"mode":"none"}}
$ bops next
bops next: bad_config: config /home/you/.config/bops/config.json: lock.mode none is accepted only from .bops.json, the committed file every machine reads; set it there if this ledger is used on one machine (did NOT run to completion)
```

With `--json` (or when piped), the same failure is one document on stdout:

```json
{
  "code": "bad_config",
  "command": "next",
  "contract": 1,
  "error": "bad_config: config /path/to/repo/.bops.json: unknown key \"stale_minute\" (have: …)",
  "ok": false
}
```

`bad_config` is reported in the result only. It isn't written to `events.jsonl`, which records the steps of ownership commands, and a config error stops a command before its first step.

Other rejected values: minute counts that aren't whole numbers, a `forge` outside `auto|github|gitlab|none`, a `lock.mode` outside `ref|none`, a list key given a non-list in JSON, and a non-object where a section (`lint`, `note`, `lock`) is expected. In the environment, a list is comma-separated and an empty variable counts as unset.

## Examples

A single-developer repo with no remote:

```json
{ "lock": { "mode": "none" } }
```

A team ledger whose shared remote is called `upstream`, with a four-hour staleness window everyone agrees on:

```json
{
  "stale_minutes": 240,
  "lock": { "remote": "upstream" }
}
```

A GPU box that serves the `ml` pool (user file, `~/.config/bops/config.json`):

```json
{ "pool": "ml", "caps": ["gpu", "linux"] }
```

The same thing for one shell session:

```bash
export BOPS_POOL=ml BOPS_CAPS=gpu,linux
```

## Related

- [How to set up the claim lock](../howto-claim-lock.md): choose a lock mode and check it with `bops doctor`.
- [Command reference](../COMMANDS.md): every command, flag and output field.
- [Tutorial: your first bops loop](../tutorial-first-loop.md): `lock.mode: none` in use.

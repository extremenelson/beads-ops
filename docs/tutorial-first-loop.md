# Tutorial: your first bops loop

In about ten minutes you will take one bead through the whole `bops` loop on a scratch ledger: find it, claim it, record progress, record where the work landed, and give it back. You will also see a second agent get turned away from a bead that is already taken. By the end you will know what each command is for and how to read its answer, which is all an agent binding does.

Everything here runs on one machine with no git remote, so nothing leaves your laptop. The [claim-lock how-to](howto-claim-lock.md) covers the shared, multi-machine setup.

## What you'll need

- `bd` 1.3 or later on your `PATH` (`bd version`). The output below is from `bd` 1.3.4.
- Go 1.24 or later (`go version`).
- `git` and `jq`.
- A clone of this repo. The tutorial builds `bops` from `main`, because one behavior it shows (step 6) landed after the `bops/v0.1.0` tag.

`bops` prints JSON when its output goes to a pipe or when you pass `--json`, and `key=value` lines on a terminal. This tutorial passes `--json` so what you see matches what an agent sees. Some outputs are trimmed with `…`.

## Step 1: Build bops and make a scratch ledger

From your clone of beads-ops:

```bash
(cd bops && go install ./cmd/bops)
bops version --json
```

```json
{
  "contract": 1,
  "command": "version",
  "ok": true,
  "bops": "dev",
  "bd": "1.3.4",
  "backend": ""
}
```

`contract: 1` is the version of the output format that bindings pin to. `bd` is the beads CLI that `bops` found on your `PATH`.

Now make a throwaway repo with a beads ledger in it:

```bash
mkdir /tmp/bops-demo && cd /tmp/bops-demo
git init -q -b main
echo "# demo" > README.md && git add README.md && git commit -qm init
bd init --prefix demo
printf '{"lock": {"mode": "none"}}\n' > .bops.json
```

`.bops.json` is the committed `bops` config for a repo. `lock.mode: none` tells `bops` this ledger lives on one machine, so it decides claims from the local ledger and needs no git remote. On a shared ledger you would leave it out and get the claim lock instead ([configuration reference](reference/configuration.md#lockmode)).

## Step 2: File a bead and ask what to work on

```bash
bd create "Add a --verbose flag" -t task -p 1 \
  --acceptance "bops --verbose prints each bd call" --json | jq -r .id
```

```
demo-iq1
```

Your id will differ. Use yours wherever this page says `demo-iq1`.

Ask `bops` what is workable:

```bash
bops next --json
```

```json
{
  "candidates": [
    {
      "id": "demo-iq1",
      "title": "Add a --verbose flag",
      "priority": 1,
      "type": "task",
      "workable": true,
      "why": ["W01", "W02", "W03", "W04", "W05", "W06", "W08", "W12", "W13", "W07 (warn): no parent epic"],
      "why_not": [],
      "blocked_by_gates": []
    }
  ],
  "command": "next",
  "contract": 1,
  "excluded_count": 0,
  "ok": true,
  "warnings": [
    "lock.mode is none: claims are decided by this machine's ledger alone, with no claim ref; two machines can both claim one bead"
  ]
}
```

"Ready" in beads means nothing blocks the bead. "Workable" means ready **and** it passes the lint rules: it has acceptance criteria, nobody holds it, it isn't waiting on a human, and so on. `why` lists the rules it passed. The W07 warning is only a flag that the bead has no parent epic. The full rule table is [SPEC §6](../SPEC.md#6-lint-rules-workability).

Every result in this mode carries the `lock.mode is none` warning, so an unprotected claim is never mistaken for a protected one.

To see a failing rule, create a second bead without `--acceptance` and run `bops lint <id> --json` on it: W01 shows up under `fails` and `workable` is `false`.

## Step 3: Claim it

```bash
bops claim demo-iq1 --json
```

```json
{
  "claimed_at": "2026-11-13T16:42:46.424802Z",
  "command": "claim",
  "contract": 1,
  "id": "demo-iq1",
  "ok": true,
  "outcome": "claimed",
  "readback_ok": true,
  "run": "01M7AWMGT9QKRY2YVF4YWA0VKZ",
  "warnings": [
    "lock.mode is none: …",
    "ledger not pulled: bd dolt pull: exit status 1: Error: fetch from origin/main: Error 1105: no remote"
  ]
}
```

The bead is yours. The actor recorded with the claim is `BEADS_ACTOR` if set, else your git `user.name`; this page's runs used `alice`. `run` is this worktree's identity, a ULID that `bops` keeps in `~/.local/state/bops/`. The "ledger not pulled" warning is expected here: `claim` tries to pull first, and this ledger has no remote to pull from.

Claiming again from the same worktree is harmless:

```bash
bops claim demo-iq1 --json | jq '{outcome, ok}'
```

```json
{ "outcome": "held", "ok": true }
```

## Step 4: Watch a second agent get turned away

Each git worktree is its own run, which is how two agents on one machine stay separate. Make a second worktree and try to take the same bead from it:

```bash
git add -A && git commit -qm "beads"
git worktree add -q ../bops-demo-wt2
(cd ../bops-demo-wt2 && bops claim demo-iq1 --json); echo "exit=$?"
```

```json
{
  "command": "claim",
  "contract": 1,
  "id": "demo-iq1",
  "ok": false,
  "outcome": "refused",
  "reason": "assigned to \"alice\"; if that claim is abandoned, use claim --force after it goes stale",
  "run": "01M7AWN7KFQ2VGPH01RF0HBSNK",
  …
}
exit=3
```

Exit code 3 means `bops` ran and the answer was no. That is why bindings write `bops claim <id> && work`: an agent that lost the claim can't fall through into editing files. Exit 1 means `bops` couldn't run at all (for example, the remote was unreachable), and the agent should stop rather than pick another bead. The [command reference](COMMANDS.md) lists every outcome.

On a shared ledger, a second **machine** gets `outcome: lost` with a `holder` naming who has the bead. The [how-to](howto-claim-lock.md#4-prove-the-lock-with-two-clones) shows that.

## Step 5: Record where you got to

A note is a handoff for whoever reads the bead next, maybe you after a context reset. Its text arrives on stdin, never as a command-line argument:

```bash
printf 'COMPLETED: flag parsed\n\nNEXT: print each bd call before running it\n' \
  | bops note demo-iq1 --json | jq '{outcome, comment_id}'
```

```json
{ "outcome": "noted", "comment_id": "7d890338-b04d-5fa3-bf75-85e61ea8d2ef" }
```

The note is stored as a comment on the bead, because comments merge cleanly across machines and the `notes` field does not. End it with a `NEXT:` paragraph: lint rule W09 checks for one, and it's the first thing a resuming agent reads.

## Step 6: Land the work, then give the bead back

Say the work is now in a pull request. Try releasing it as done:

```bash
bops release demo-iq1 --reason done --json | jq '{outcome, detail}'
```

```json
{
  "outcome": "refused",
  "detail": "demo-iq1 has no land record: run `bops land demo-iq1 --ref <pr-url> --sha <sha>` first, or give it back with --reason scope"
}
```

`bops` won't let you call work done without saying where it went. (This refusal landed after `bops/v0.1.0`; a tagged v0.1.0 binary accepts the release.) Record the PR and the commit:

```bash
bops land demo-iq1 --ref https://github.com/example/demo/pull/1 --sha 0123abc --json | jq '{outcome, landed}'
```

```json
{
  "outcome": "landed",
  "landed": {
    "at": "2026-11-13T16:42:49Z",
    "ref": "https://github.com/example/demo/pull/1",
    "run": "01M7AWMGT9QKRY2YVF4YWA0VKZ",
    "sha": "0123abc"
  }
}
```

`land` only records evidence. It never merges, pushes or closes anything. Now the release goes through:

```bash
bops release demo-iq1 --reason done --json | jq '{outcome, reason}'
```

```json
{ "outcome": "released", "reason": "done" }
```

## Step 7: See the history, then close the bead

Every `bops` write left one typed event comment on the bead:

```bash
bd comments demo-iq1 --json | jq -r '.[].text'
```

```
bops:claimed run=01M7AWMGT9QKRY2YVF4YWA0VKZ actor=alice machine=laptop
bops:note run=01M7AWMGT9QKRY2YVF4YWA0VKZ
COMPLETED: flag parsed

NEXT: print each bd call before running it

bops:landed run=01M7AWMGT9QKRY2YVF4YWA0VKZ ref=https://github.com/example/demo/pull/1 sha=0123abc
bops:released run=01M7AWMGT9QKRY2YVF4YWA0VKZ reason=done
```

The bead is still `in_progress` and labelled `bops:landing`: it waits for its PR to merge. Workers never close beads. On a real repo, `bops sweep --dry-run=false` asks GitHub or GitLab whether the PR merged and closes the bead. This scratch repo has no forge, so `sweep` can't check and lists the bead under `unknown`:

```bash
bops sweep --json | jq '{dry_run, would_close, unknown}'
```

```json
{ "dry_run": true, "would_close": [], "unknown": ["demo-iq1"] }
```

So you play the human and close it:

```bash
bd close demo-iq1 --reason "merged via https://github.com/example/demo/pull/1"
```

Clean up when you're done: `git worktree remove ../bops-demo-wt2 && rm -rf /tmp/bops-demo`.

## What you built

You ran the whole intent loop an agent binding runs:

| Intent | Command | What you saw |
|---|---|---|
| What should I do? | `bops next` | ranked, workable candidates and why |
| Take it | `bops claim` | `claimed`, then `held`; a second worktree `refused` with exit 3 |
| Record where I am | `bops note` | a handoff comment ending in `NEXT:` |
| Prove it landed | `bops land` | the PR and sha stored on the bead |
| Give it back | `bops release --reason done` | refused until landed, then `released` |
| Close it | `bops sweep` or a human | workers never close |

Next:

- Put it on a real, shared repo: [How to set up the claim lock](howto-claim-lock.md).
- Wire it into an agent: [Claude Code binding](../bindings/claude-code/README.md).
- Every flag and output field: [command reference](COMMANDS.md). Every config key: [configuration reference](reference/configuration.md).
- When something gets stuck: [PROCESS.md §12 recovery runbook](PROCESS.md#12-recovery-runbook-bops-ledgers).
